// Package hsmstest provides an in-memory hsms.SECS2Endpoint fake and DataMessage equality
// assertions for testing code that depends on hsms.SECS2Endpoint / hsms.Connection.
// Import this package only in test code; it pulls in testify/require.
package hsmstest

import (
	"context"
	"errors"
	"reflect"
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
// The zero value FakeEndpoint{} is ready to use (embed it directly in a custom fake, no
// constructor call required).
// NewFakeEndpoint exists only to apply FakeOptions.
// FakeEndpoint is safe for concurrent use.
type FakeEndpoint struct {
	mu            sync.Mutex
	sessionID     uint16
	sysBytes      atomic.Uint32
	sent          []SentMessage
	replies       []reply
	dataHandlers  []hsms.DataMessageHandler
	dataChans     []chan *hsms.DataMessage
	stateHandlers []hsms.StateChangeHandler

	decodeErrHandlers []hsms.DecodeErrorHandler

	// done and closeOnce back Close/Deliver's channel-teardown signal.
	// done is lazily initialized (see doneChan) rather than set in NewFakeEndpoint.
	// That keeps a bare FakeEndpoint{} literal — e.g. from embedding FakeEndpoint in a
	// custom fake, the CHANGELOG's recommended migration for a hand-rolled SECS2Endpoint —
	// fully usable without requiring NewFakeEndpoint.
	closeOnce sync.Once
	done      chan struct{}
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

// doneChan returns f.done, initializing it on first use under f.mu.
// This lazy init (rather than setting done in NewFakeEndpoint) is what keeps a bare
// FakeEndpoint{} literal fully usable.
// Deliver and Close both go through this accessor instead of touching f.done directly, so
// neither one requires NewFakeEndpoint to have run.
func (f *FakeEndpoint) doneChan() chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.done == nil {
		f.done = make(chan struct{})
	}

	return f.done
}

// Deliver invokes every registered DataMessageHandler with (msg, f), and delivers msg to every
// channel registered via AddDataMessageChan, as if msg had arrived on the wire.
// Handlers and channels are snapshotted under the lock and invoked/sent with the lock released,
// so a handler may call back into f (e.g. f.ReplyDataMessage) without deadlocking.
//
// Channel delivery selects on a per-FakeEndpoint done signal alongside the channel send,
// mirroring the real hsms.SECS2Endpoint.AddDataMessageChan contract: a full channel blocks
// Deliver until either the consumer drains it or Close unblocks the send.
func (f *FakeEndpoint) Deliver(msg *hsms.DataMessage) {
	f.mu.Lock()
	handlers := slices.Clone(f.dataHandlers)
	chans := slices.Clone(f.dataChans)
	f.mu.Unlock()

	for _, h := range handlers {
		h(msg, f)
	}

	if len(chans) == 0 {
		return
	}

	done := f.doneChan()
	for _, ch := range chans {
		select {
		case ch <- msg:
		case <-done:
			return
		}
	}
}

// Close signals teardown to any goroutine blocked delivering to a channel registered via
// AddDataMessageChan, mirroring the real connection's teardown-unblocks-fan-out behavior.
// It is idempotent and safe for concurrent use.
// Call it from t.Cleanup in a test that registers channels.
//
// Close is a bare unblock signal only:
// unlike hsms.Connection.Close, it returns no error, has no timeout, and does not wait for
// anything — it just closes the done channel Deliver selects on.
func (f *FakeEndpoint) Close() {
	done := f.doneChan()
	f.closeOnce.Do(func() { close(done) })
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

// AddDataMessageChan registers ch to receive every inbound data message Deliver fans out,
// mirroring the real hsms.SECS2Endpoint.AddDataMessageChan contract.
// Panics if ch is nil.
func (f *FakeEndpoint) AddDataMessageChan(ch chan *hsms.DataMessage) {
	if ch == nil {
		panic("hsmstest: AddDataMessageChan: ch must not be nil")
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.dataChans = append(f.dataChans, ch)
}

// AddDecodeErrorHandler records decode-error handlers so FakeEndpoint satisfies the
// hsms.SECS2Endpoint interface. DeliverDecodeError invokes them.
func (f *FakeEndpoint) AddDecodeErrorHandler(handlers ...hsms.DecodeErrorHandler) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.decodeErrHandlers = append(f.decodeErrHandlers, handlers...)
}

// DeliverDecodeError invokes every registered DecodeErrorHandler with (msg, err, f),
// as if an undecodable message had arrived on the wire. Handlers are snapshotted under
// the lock and invoked with it released, matching Deliver's contract.
func (f *FakeEndpoint) DeliverDecodeError(msg *hsms.DataMessage, err error) {
	f.mu.Lock()
	handlers := slices.Clone(f.decodeErrHandlers)
	f.mu.Unlock()

	for _, h := range handlers {
		h(msg, err, f)
	}
}

// AddConnStateChangeHandler appends one or more connection state-change handlers.
func (f *FakeEndpoint) AddConnStateChangeHandler(handlers ...hsms.StateChangeHandler) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stateHandlers = append(f.stateHandlers, handlers...)
}

// isNilMessage reports whether msg is a nil interface value,
// or an interface holding a typed nil.
//
// It mirrors the guard hsms.session.SendSECS2Message applies,
// so the fake rejects the same inputs the real endpoint rejects.
func isNilMessage(msg secs2.SECS2Message) bool {
	if msg == nil {
		return true
	}

	rv := reflect.ValueOf(msg)
	switch rv.Kind() { //nolint:exhaustive // only nil-capable kinds may call Value.IsNil
	case reflect.Chan, reflect.Func, reflect.Map, reflect.Pointer, reflect.Slice:
		return rv.IsNil()
	default:
		return false
	}
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
	if primary == nil {
		return hsms.ErrNilMessage
	}

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

	// Mirror the real session's reply-path decode check (hsms.session.SendDataMessage /
	// SendSECS2Message): a scripted reply that framed but whose SECS-II body fails to decode
	// is surfaced as (msg, decodeErr) — not a clean success — so a test exercising a malformed
	// reply behaves identically against the fake and a live endpoint. Only applied on the
	// success path (r.err == nil), matching the real code, which checks DecodeErr only after
	// WriteMessage returns no error.
	if r.err == nil && r.msg != nil {
		if derr := r.msg.DecodeErr(); derr != nil {
			return r.msg, derr
		}
	}

	return r.msg, r.err
}

// SendDataMessage records the send. function must be odd (SEMI E5 §7.2), matching the real
// session's guard; an even function returns hsms.ErrEvenFunctionPrimary without recording
// anything.
// When replyExpected is false it returns (nil, nil) without consulting the reply
// script (fire-and-forget, matching the real engine's sendWaitReply short-circuit); when true
// it pops the next scripted reply.
func (f *FakeEndpoint) SendDataMessage(_ context.Context, stream, function byte, replyExpected bool, item secs2.Item) (*hsms.DataMessage, error) {
	if function%2 == 0 {
		return nil, hsms.ErrEvenFunctionPrimary
	}

	if err := f.record("SendDataMessage", stream, function, replyExpected, item); err != nil {
		return nil, err
	}
	if !replyExpected {
		return nil, nil //nolint:nilnil // fire-and-forget: matches the real engine's sendWaitReply contract
	}

	return f.popScriptedReply()
}

// SendDataMessageAsync records the send and always returns nil (no reply is ever awaited).
// function must be odd (SEMI E5 §7.2), matching the real session's guard; an even function
// returns hsms.ErrEvenFunctionPrimary without recording anything. replyExpected must be false:
// the real async path never begins a reply timer (SEMI E37 §9.4.1.2), so a reply-expecting send
// returns hsms.ErrAsyncReplyExpected without recording anything.
func (f *FakeEndpoint) SendDataMessageAsync(_ context.Context, stream, function byte, replyExpected bool, item secs2.Item) error {
	if function%2 == 0 {
		return hsms.ErrEvenFunctionPrimary
	}
	if replyExpected {
		return hsms.ErrAsyncReplyExpected
	}

	return f.record("SendDataMessageAsync", stream, function, replyExpected, item)
}

// SendSECS2Message records the send. msg's function must be odd (SEMI E5 §7.2), matching the
// real session's guard; an even function returns hsms.ErrEvenFunctionPrimary without recording
// anything.
// Every accessor is read once into a local before the guard runs and reused for
// recording, mirroring the real session's TOCTOU-safe snapshot discipline for the externally
// implementable secs2.SECS2Message interface.
// When the snapshotted WaitBit is false it returns
// (nil, nil) without consulting the reply script; when true it pops the next scripted reply.
// A nil or typed-nil msg returns hsms.ErrNilMessage without recording anything.
func (f *FakeEndpoint) SendSECS2Message(_ context.Context, msg secs2.SECS2Message) (*hsms.DataMessage, error) {
	if isNilMessage(msg) {
		return nil, hsms.ErrNilMessage
	}

	stream, function, waitBit, item := msg.StreamCode(), msg.FunctionCode(), msg.WaitBit(), msg.Item()
	if function%2 == 0 {
		return nil, hsms.ErrEvenFunctionPrimary
	}

	if err := f.record("SendSECS2Message", stream, function, waitBit, item); err != nil {
		return nil, err
	}
	if !waitBit {
		return nil, nil //nolint:nilnil // fire-and-forget: matches the real engine's sendWaitReply contract
	}

	return f.popScriptedReply()
}

// ReplyDataMessage records the reply.
// It never consults the reply script (a reply is not a synchronous send awaiting a response).
// Returns hsms.ErrNilMessage if primary is nil, recording nothing, matching the real session.
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
