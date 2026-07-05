// Package hsmstest provides an in-memory hsms.SECS2Endpoint fake and DataMessage equality
// assertions for testing code that depends on hsms.SECS2Endpoint / hsms.Connection.
// Import this package only in test code; it pulls in testify/require.
package hsmstest

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
)

// ErrNoScriptedReply is returned by a synchronous send (SendDataMessage, SendSECS2Message) when
// the reply script is empty. An unstubbed synchronous send is a test-authoring bug: the fake
// fails fast with this sentinel rather than silently returning a zero-valued message.
var ErrNoScriptedReply = errors.New("hsmstest: no scripted reply queued")

// SentMessage records one outbound call made through FakeEndpoint.
type SentMessage struct {
	Method  string            // "SendDataMessage" | "SendDataMessageAsync" | "SendSECS2Message" | "ReplyDataMessage" | "ForwardDataMessage" | "ForwardDataMessageAsync"
	Message *hsms.DataMessage // the message as the fake reconstructed it (Forward* record it verbatim)
	Primary *hsms.DataMessage // ReplyDataMessage only: the primary being replied to
}

// reply is one scripted (reply, err) result for a synchronous send.
type reply struct {
	msg *hsms.DataMessage
	err error
}

// FakeEndpoint is an in-memory hsms.SECS2Endpoint for tests: it records every send/reply,
// returns scripted replies to synchronous sends, and can deliver an inbound message to the
// registered DataMessageHandlers as if it arrived on the wire — all with no TCP connection.
// FakeEndpoint is safe for concurrent use.
type FakeEndpoint struct {
	mu            sync.Mutex
	sessionID     uint16
	sysBytes      atomic.Uint32
	sent          []SentMessage
	replies       []reply
	dataHandlers  []hsms.DataMessageHandler
	stateHandlers []hsms.StateChangeHandler
}

var _ hsms.SECS2Endpoint = (*FakeEndpoint)(nil)

// FakeOption configures a FakeEndpoint at construction time.
type FakeOption func(*FakeEndpoint)

// WithSessionID sets the session ID FakeEndpoint reports and stamps onto recorded messages.
func WithSessionID(id uint16) FakeOption {
	return func(f *FakeEndpoint) { f.sessionID = id }
}

// NewFakeEndpoint returns a new FakeEndpoint with no recorded sends and an empty reply script.
func NewFakeEndpoint(opts ...FakeOption) *FakeEndpoint {
	f := &FakeEndpoint{}
	for _, opt := range opts {
		opt(f)
	}

	return f
}

// SessionID returns the configured session ID (WithSessionID; 0 by default).
func (f *FakeEndpoint) SessionID() uint16 { return f.sessionID }

// ScriptReply enqueues one (reply, err) result, consumed FIFO by the next synchronous send
// (SendDataMessage or SendSECS2Message with a reply expected).
func (f *FakeEndpoint) ScriptReply(msg *hsms.DataMessage, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies = append(f.replies, reply{msg: msg, err: err})
}

// Sent returns a snapshot copy of every call recorded so far.
func (f *FakeEndpoint) Sent() []SentMessage {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.sent)
}

// Deliver invokes every registered DataMessageHandler with (msg, f), as if msg had arrived on
// the wire. Handlers are snapshotted under the lock and invoked with the lock released, so a
// handler may call back into f (e.g. f.ReplyDataMessage) without deadlocking.
func (f *FakeEndpoint) Deliver(msg *hsms.DataMessage) {
	f.mu.Lock()
	handlers := slices.Clone(f.dataHandlers)
	f.mu.Unlock()

	for _, h := range handlers {
		h(msg, f)
	}
}

// DeliverState invokes every registered StateChangeHandler with (prev, next), as if the
// connection's FSM had transitioned. Handlers are snapshotted under the lock and invoked with
// the lock released, matching Deliver's contract.
func (f *FakeEndpoint) DeliverState(prev, next hsms.ConnState) {
	f.mu.Lock()
	handlers := slices.Clone(f.stateHandlers)
	f.mu.Unlock()

	for _, h := range handlers {
		h(prev, next)
	}
}

// AddDataMessageHandler appends one or more inbound data message handlers.
func (f *FakeEndpoint) AddDataMessageHandler(handlers ...hsms.DataMessageHandler) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dataHandlers = append(f.dataHandlers, handlers...)
}

// AddConnStateChangeHandler appends one or more connection state-change handlers.
func (f *FakeEndpoint) AddConnStateChangeHandler(handlers ...hsms.StateChangeHandler) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stateHandlers = append(f.stateHandlers, handlers...)
}

// record constructs the DataMessage via hsms.NewDataMessage and returns its error, if any,
// BEFORE any side effect — no Sent() entry, no scripted-reply pop, no handler invocation
// happens on error. This mirrors the real session methods, which all return a NewDataMessage
// construction error immediately without writing/enqueueing anything.
func (f *FakeEndpoint) record(method string, stream, function byte, replyExpected bool, item secs2.Item) error {
	msg, err := hsms.NewDataMessage(stream, function, replyExpected, f.sessionID, hsms.ToSystemBytes(f.sysBytes.Add(1)), item)
	if err != nil {
		return err
	}

	f.mu.Lock()
	f.sent = append(f.sent, SentMessage{Method: method, Message: msg})
	f.mu.Unlock()

	return nil
}

// recordReply is ReplyDataMessage's equivalent of record: it constructs the reply message with
// the exact fixed shape the real engine uses (stream/function/W-bit/system-bytes all derived
// from primary, never from caller input), and returns any construction error BEFORE recording a
// SentMessage entry. The only constructor error reachable on this path is item.Error(), since
// primary.Stream() is already masked to <=127 and replyExpected is hardcoded false (so the
// even-function W-bit check can never fire here).
func (f *FakeEndpoint) recordReply(primary *hsms.DataMessage, item secs2.Item) error {
	msg, err := hsms.NewDataMessage(
		primary.Stream(), primary.Function()+1, false,
		f.sessionID, primary.SystemBytes(), item,
	)
	if err != nil {
		return err
	}

	f.mu.Lock()
	f.sent = append(f.sent, SentMessage{Method: "ReplyDataMessage", Message: msg, Primary: primary})
	f.mu.Unlock()

	return nil
}

// popScriptedReply pops and returns the next scripted (reply, err) result, or ErrNoScriptedReply
// if the script is empty.
func (f *FakeEndpoint) popScriptedReply() (*hsms.DataMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if len(f.replies) == 0 {
		return nil, ErrNoScriptedReply
	}

	r := f.replies[0]
	f.replies = f.replies[1:]

	return r.msg, r.err
}

// SendDataMessage records the send. When replyExpected is false it returns (nil, nil) without
// consulting the reply script (fire-and-forget, matching the real engine's sendWaitReply
// short-circuit); when true it pops the next scripted reply.
func (f *FakeEndpoint) SendDataMessage(_ context.Context, stream, function byte, replyExpected bool, item secs2.Item) (*hsms.DataMessage, error) {
	if err := f.record("SendDataMessage", stream, function, replyExpected, item); err != nil {
		return nil, err
	}
	if !replyExpected {
		return nil, nil //nolint:nilnil // fire-and-forget: matches the real engine's sendWaitReply contract
	}

	return f.popScriptedReply()
}

// SendDataMessageAsync records the send and always returns nil (no reply is ever awaited).
func (f *FakeEndpoint) SendDataMessageAsync(_ context.Context, stream, function byte, replyExpected bool, item secs2.Item) error {
	return f.record("SendDataMessageAsync", stream, function, replyExpected, item)
}

// SendSECS2Message records the send. When msg.WaitBit() is false it returns (nil, nil) without
// consulting the reply script; when true it pops the next scripted reply.
func (f *FakeEndpoint) SendSECS2Message(_ context.Context, msg secs2.SECS2Message) (*hsms.DataMessage, error) {
	if err := f.record("SendSECS2Message", msg.StreamCode(), msg.FunctionCode(), msg.WaitBit(), msg.Item()); err != nil {
		return nil, err
	}
	if !msg.WaitBit() {
		return nil, nil //nolint:nilnil // fire-and-forget: matches the real engine's sendWaitReply contract
	}

	return f.popScriptedReply()
}

// ReplyDataMessage records the reply. It never consults the reply script (a reply is not a
// synchronous send awaiting a response).
func (f *FakeEndpoint) ReplyDataMessage(_ context.Context, primary *hsms.DataMessage, item secs2.Item) error {
	return f.recordReply(primary, item)
}

// ForwardDataMessage records a verbatim forward send, preserving msg exactly (System Bytes and
// all). It never consults the reply script: the real engine routes any reply to the registered
// handlers (use Deliver to simulate it), not back as a return value. Returns hsms.ErrNilMessage
// if msg is nil, matching the real session.
func (f *FakeEndpoint) ForwardDataMessage(_ context.Context, msg *hsms.DataMessage) error {
	return f.recordForward("ForwardDataMessage", msg)
}

// ForwardDataMessageAsync records a verbatim forward send (see ForwardDataMessage). Returns
// hsms.ErrNilMessage if msg is nil.
func (f *FakeEndpoint) ForwardDataMessageAsync(_ context.Context, msg *hsms.DataMessage) error {
	return f.recordForward("ForwardDataMessageAsync", msg)
}

// recordForward records a pre-built message verbatim (unlike record, which constructs a fresh
// message with generated System Bytes). It returns hsms.ErrNilMessage BEFORE any side effect when
// msg is nil, mirroring the real session's nil guard.
func (f *FakeEndpoint) recordForward(method string, msg *hsms.DataMessage) error {
	if msg == nil {
		return hsms.ErrNilMessage
	}

	f.mu.Lock()
	f.sent = append(f.sent, SentMessage{Method: method, Message: msg})
	f.mu.Unlock()

	return nil
}
