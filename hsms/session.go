package hsms

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/arloliu/go-secs/v2/secs2"
)

// Compile-time assertion that *session implements SECS2Endpoint.
var _ SECS2Endpoint = (*session)(nil)

// session is the unexported concrete implementation of [SECS2Endpoint].
// One session exists per HSMS-SS connection (single-session per E37.1).
//
// # Fan-out approach
//
// recvDataMsgOn delivers messages synchronously on the calling goroutine (the epoch-joined recv loop, G3),
// fenced to the specific generation's cancellation channel that admitted the message,
// to both func handlers and channel handlers:
//
//   - Func handlers ([DataMessageHandler]) are called directly; no goroutine is spawned.
//   - Channel handlers (chan *[DataMessage]) are delivered via
//     select { case ch <- msg: case <-done: return }
//     so a stalled receiver can never block the fan-out past that generation's teardown (J5).
//
// Because both paths are synchronous, no goroutines are spawned and no separate join
// is needed (G3 is satisfied by the recv-loop epoch join, built in Task 19).
//
// # AddConnStateChangeHandler
//
// Spec §5.1 places state-change handlers on the Connection (atomic.Pointer[[]StateChangeHandler])
// so they persist across Open/Close cycles while the supervisor is recreated per Open.
// The session holds connHandlers, a pointer to the Connection's field. If connHandlers
// is nil (pre-Task-13 wiring), the call is a documented no-op; Task 13 sets the real
// pointer after constructing the session.
type session struct {
	id     uint16
	rt     TransportRuntime
	sysGen *sysBytesGen

	mu                sync.RWMutex
	handlers          []DataMessageHandler
	chans             []chan *DataMessage
	decodeErrHandlers []DecodeErrorHandler

	// connHandlers points to the Connection's persistent state-change handler slice.
	// nil until Task 13 wires it; AddConnStateChangeHandler is a no-op until then.
	connHandlers *atomic.Pointer[[]StateChangeHandler]
}

// newSession creates a session for the given id, backed by rt and sysGen.
// connHandlers is deliberately not a constructor parameter; Task 13 sets
// s.connHandlers = &connection.handlers after calling newSession.
func newSession(id uint16, rt TransportRuntime, sysGen *sysBytesGen) *session {
	return &session{
		id:     id,
		rt:     rt,
		sysGen: sysGen,
	}
}

// SessionID returns the HSMS session ID.
//
// Never blocks.
func (s *session) SessionID() uint16 { return s.id }

// SendDataMessage builds a primary data message and delegates to rt.WriteMessage.
//
// SendDataMessage always mints fresh System Bytes, so it always opens a new transaction and is always a primary;
// function must be odd (SEMI E5 §7.2), or this returns ErrEvenFunctionPrimary without sending anything.
//
// When replyExpected is true, WriteMessage waits for the T3-bounded reply;
// the B1 IsSelected gate, I1 inflight accounting, and T3 timer enforcement are all owned by the engine (Task 12) inside WriteMessage — not here.
func (s *session) SendDataMessage(ctx context.Context, stream, function byte, replyExpected bool, item secs2.Item) (*DataMessage, error) {
	if function%2 == 0 {
		return nil, ErrEvenFunctionPrimary
	}

	msg, err := NewDataMessage(stream, function, replyExpected, s.rt.SessionID(), s.sysGen.next(), item)
	if err != nil {
		return nil, err
	}

	reply, err := s.rt.WriteMessage(ctx, msg)
	if err != nil {
		return nil, err
	}

	// nil interface → (nil, false); typed-nil is returned as nil.
	dm, _ := reply.(*DataMessage)

	// A reply whose body fails to decode must not unblock the caller as a clean success.
	// Surface the decode error, but return the message alongside it (non-destructive: the
	// header stays available to the caller). A nil reply (timeout, or a control-message
	// reply) skips the check.
	if dm != nil {
		if derr := dm.DecodeErr(); derr != nil {
			return dm, derr
		}
	}

	return dm, nil
}

// SendDataMessageAsync builds a data message and enqueues it on the per-generation async send channel via rt.SendAsync.
//
// SendDataMessageAsync always mints fresh System Bytes, so it always opens a new transaction and
// is always a primary; function must be odd (SEMI E5 §7.2), or this returns
// ErrEvenFunctionPrimary without sending anything.
//
// No reply is awaited: SendAsync enqueues the message without beginning a reply timer, but SEMI
// E37 §9.4.1.2 requires a primary that expects a reply to begin one.
// So replyExpected must be false, or this returns ErrAsyncReplyExpected without sending anything;
// use [SendDataMessage] for a reply-bearing transaction.
func (s *session) SendDataMessageAsync(ctx context.Context, stream, function byte, replyExpected bool, item secs2.Item) error {
	if function%2 == 0 {
		return ErrEvenFunctionPrimary
	}
	if replyExpected {
		return ErrAsyncReplyExpected
	}

	msg, err := NewDataMessage(stream, function, replyExpected, s.rt.SessionID(), s.sysGen.next(), item)
	if err != nil {
		return err
	}

	return s.rt.SendAsync(ctx, msg)
}

// SendSECS2Message builds an HSMS DataMessage from a [secs2.SECS2Message] (stream, function, W-bit, item)
// and delegates to rt.WriteMessage.
//
// SendSECS2Message always mints fresh System Bytes, so it always opens a new transaction and is
// always a primary; msg's function must be odd (SEMI E5 §7.2), or this returns
// ErrEvenFunctionPrimary without sending anything.
//
// Returns ErrNilMessage if msg is nil or typed-nil.
//
// Returns the reply DataMessage when the W-bit is set.
func (s *session) SendSECS2Message(ctx context.Context, msg secs2.SECS2Message) (*DataMessage, error) {
	if isNilValue(msg) {
		return nil, ErrNilMessage
	}

	// secs2.SECS2Message is externally implementable and carries no immutability
	// guarantee, so every field is snapshotted once into a local before the guard
	// runs; the guard and the construction below both read the same snapshot,
	// closing a TOCTOU window where a call-sensitive implementation could return
	// an odd function to the guard and an even one to the constructor.
	stream, function, waitBit, item := msg.StreamCode(), msg.FunctionCode(), msg.WaitBit(), msg.Item()
	if function%2 == 0 {
		return nil, ErrEvenFunctionPrimary
	}

	dm, err := NewDataMessage(
		stream, function, waitBit,
		s.rt.SessionID(), s.sysGen.next(), item,
	)
	if err != nil {
		return nil, err
	}

	reply, err := s.rt.WriteMessage(ctx, dm)
	if err != nil {
		return nil, err
	}

	dataReply, _ := reply.(*DataMessage)

	// See SendDataMessage: an undecodable reply surfaces its decode error, but the message
	// is still returned alongside the error so the caller keeps the header.
	if dataReply != nil {
		if derr := dataReply.DecodeErr(); derr != nil {
			return dataReply, derr
		}
	}

	return dataReply, nil
}

// ForwardDataMessage writes a pre-built data message verbatim (preserving its System Bytes, W-bit, session ID,
// and stream/function) via rt.WriteMessageNoReply and returns once the frame is on the wire.
//
// No reply is registered or consumed here — a secondary the peer sends is delivered to the registered DataMessageHandlers,
// so the caller owns reply correlation.
// See the SECS2Endpoint.ForwardDataMessage contract.
// Returns ErrNilMessage if msg is nil.
func (s *session) ForwardDataMessage(ctx context.Context, msg *DataMessage) error {
	if msg == nil {
		return ErrNilMessage
	}

	return s.rt.WriteMessageNoReply(ctx, msg)
}

// ForwardDataMessageAsync enqueues a pre-built data message verbatim on the per-generation async send channel via rt.SendAsync, preserving its full envelope.
//
// No reply is registered or consumed here (see ForwardDataMessage).
// Returns ErrNilMessage if msg is nil.
func (s *session) ForwardDataMessageAsync(ctx context.Context, msg *DataMessage) error {
	if msg == nil {
		return ErrNilMessage
	}

	return s.rt.SendAsync(ctx, msg)
}

// originSender is the narrow capability [session.ReplyDataMessage] needs from its own connection:
// its identity token, to recognize a primary it admitted itself, and a generation-bound send to
// reply through that same generation.
// *connection satisfies it (asserted in connection.go).
// A session built over a test double that does not — most in-package mock runtimes —
// safely falls through to the ordinary send path,
// since the type assertion in ReplyDataMessage simply reports false for it.
type originSender interface {
	originIdentity() *connIdentity
	SendAsyncFromGeneration(ctx context.Context, gen uint64, msg Message) error
}

// ReplyDataMessage sends a secondary data message in reply to primary.
//
// The reply function is primary.Function()+1 (SECS-II secondary-function convention: primary is odd, reply is even), replyExpected is false,
// and system bytes are taken verbatim from primary (E37 §8.2.6.9 — system bytes must match).
//
// Returns ErrNilMessage if primary is nil.
//
// When primary was admitted by THIS session's own connection —
// i.e. primary, or the copy it derives from via [DataMessage.WithSessionID], [DataMessage.WithSystemBytes], or [DataMessage.WithID], was delivered through this connection's [DataMessageHandler], [DecodeErrorHandler], or [SECS2Endpoint.AddDataMessageChan] fan-out —
// the reply is sent through that SAME generation and refused with [ErrConnClosed] once that generation has ended,
// rather than going out on a successor generation with the wrong System Bytes context.
// A primary that arrived on a different connection,
// that was never delivered through the fan-out,
// or that reached this call as a hand-built message via [DataMessage.Derive] and [DataMessageBuilder.Build] (which does not carry the origin forward)
// uses the ordinary send path instead —
// the same not-Selected, context, and validation errors as any other send apply —
// and the caller owns correlation for it.
//
// The message is enqueued via SendAsync/SendAsyncFromGeneration (no W-bit, no reply correlation needed).
func (s *session) ReplyDataMessage(ctx context.Context, primary *DataMessage, item secs2.Item) error {
	if primary == nil {
		return ErrNilMessage
	}

	dm, err := NewDataMessage(
		primary.Stream(),
		primary.Function()+1, // SECS-II secondary function = primary function + 1
		false,                // no W-bit on reply messages
		s.rt.SessionID(),
		primary.SystemBytes(), // verbatim primary system bytes (E37 §8.2.6.9)
		item,
	)
	if err != nil {
		return err
	}

	// primary.originIdent is an opaque token, not a pointer back to the connection that admitted primary (see DataMessage's origin fields),
	// so identifying "this session's own connection" goes through a type assertion on the runtime rather than a direct field read.
	if os, ok := s.rt.(originSender); ok && primary.originIdent != nil && os.originIdentity() == primary.originIdent {
		return os.SendAsyncFromGeneration(ctx, primary.originGen, dm)
	}

	return s.rt.SendAsync(ctx, dm)
}

// AddDataMessageHandler appends one or more inbound data-message handlers under mu.Lock. recvDataMsgOn snapshots the slice header under RLock,
// so concurrent registration and delivery are race-free.
func (s *session) AddDataMessageHandler(handlers ...DataMessageHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers = append(s.handlers, handlers...)
}

// AddDecodeErrorHandler appends one or more inbound decode-error handlers under mu.Lock. dispatchDecodeErrorOn snapshots the slice header under RLock,
// so concurrent registration and delivery are race-free.
func (s *session) AddDecodeErrorHandler(handlers ...DecodeErrorHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.decodeErrHandlers = append(s.decodeErrHandlers, handlers...)
}

// hasDecodeErrorHandlers reports whether at least one decode-error handler is
// registered. Read under RLock.
func (s *session) hasDecodeErrorHandlers() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.decodeErrHandlers) > 0
}

// dispatchDecodeErrorOn delivers (msg, err) to every registered decode-error handler, fenced to a
// specific generation's cancellation channel (done): done must be e.ctx.Done() for the generation
// that admitted msg, never e.done.
//
// Handlers are snapshotted under RLock and invoked with the lock released.
// The snapshot is safe because AddDecodeErrorHandler only ever appends and never mutates existing elements —
// a growing append writes only at indices >= the snapshot's length, into spare capacity or a fresh backing array —
// and the header read/write is mutex-synchronized,
// so ranging the aliased header after RUnlock cannot race a concurrent registration.
//
// Observed-cancellation contract: done is rechecked before EACH handler call, not just once at
// entry, so a cancellation observed mid-fan-out (the generation that admitted msg ends while an
// earlier handler runs) stops delivery to every handler still to come.
// A handler already running when done is observed is not interrupted — see [DecodeErrorHandler].
func (s *session) dispatchDecodeErrorOn(done <-chan struct{}, msg *DataMessage, err error) {
	s.mu.RLock()
	handlers := s.decodeErrHandlers
	s.mu.RUnlock()

	for _, h := range handlers {
		select {
		case <-done:
			return
		default:
		}

		s.callDecodeErrorHandler(h, msg, err)
	}
}

// callDecodeErrorHandler is the single call site that invokes a [DecodeErrorHandler] with (msg, err, s).
func (s *session) callDecodeErrorHandler(h DecodeErrorHandler, msg *DataMessage, err error) {
	h(msg, err, s)
}

// AddDataMessageChan implements the SECS2Endpoint method of the same name.
// See that interface method's godoc for the full consumer contract.
//
// The session delivers each inbound message to ch via a select that includes rt.Done() (J5),
// so a full channel can never block the fan-out past connection teardown.
//
// Panics if ch is nil: a nil channel would make every delivery select wait only on
// rt.Done(), wedging the connection from the first inbound message.
func (s *session) AddDataMessageChan(ch chan *DataMessage) {
	if ch == nil {
		panic("hsms: AddDataMessageChan: ch must not be nil")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.chans = append(s.chans, ch)
}

// AddConnStateChangeHandler registers state-change handlers on the Connection's persistent handler slice (spec §5.1).
//
// Uses a lock-free CAS loop to append atomically to the atomic.Pointer[[]StateChangeHandler] held by connHandlers.
//
// If connHandlers is nil (pre-Task-13 wiring), this call is a no-op; Task 13 wires the real pointer after session construction
// so subsequent calls reach the Connection's persistent storage.
func (s *session) AddConnStateChangeHandler(handlers ...StateChangeHandler) {
	if s.connHandlers == nil || len(handlers) == 0 {
		return
	}

	for {
		old := s.connHandlers.Load()
		var newSlice []StateChangeHandler

		if old != nil {
			newSlice = make([]StateChangeHandler, len(*old)+len(handlers))
			copy(newSlice, *old)
			copy(newSlice[len(*old):], handlers)
		} else {
			newSlice = make([]StateChangeHandler, len(handlers))
			copy(newSlice, handlers)
		}

		if s.connHandlers.CompareAndSwap(old, &newSlice) {
			return
		}
	}
}

// recvDataMsgOn delivers msg to every registered handler and channel synchronously (§5.4, J5/G3),
// fenced to a specific generation's cancellation channel (done):
// done must be e.ctx.Done() for the generation that admitted msg, never e.done.
// The same immutable *DataMessage pointer is passed to every handler — no Clone (D7).
//
// Fan-out order:
//  1. Func handlers ([DataMessageHandler]) are called directly (no goroutine spawned).
//  2. Channel handlers are delivered via
//     select { case ch <- msg: case <-done: return }
//     so a full/stalled channel never blocks the fan-out past that generation's teardown (J5).
//
// Returns immediately if there are no handlers or if the generation is already torn down.
//
// Observed-cancellation contract:
// done is rechecked before EACH handler call and before EACH channel delivery, not just once at entry,
// so a cancellation observed mid-fan-out (the generation that admitted msg ends while an earlier handler or channel send runs) stops delivery to every handler and channel still to come.
// A handler already running, or a channel send already unblocked by room becoming available,
// when done is observed is not interrupted —
// that race is the documented contract, not a bug;
// see [DataMessageHandler] and [SECS2Endpoint.AddDataMessageChan].
func (s *session) recvDataMsgOn(done <-chan struct{}, msg *DataMessage) {
	s.mu.RLock()
	handlers := s.handlers
	chans := s.chans
	s.mu.RUnlock()

	for _, h := range handlers {
		select {
		case <-done:
			return
		default:
		}

		s.callDataHandler(h, msg)
	}

	for _, ch := range chans {
		select {
		case <-done:
			return
		default:
		}

		select {
		case ch <- msg:
		case <-done:
			return
		}
	}
}

// callDataHandler is the single call site that invokes a [DataMessageHandler] with (msg, s).
func (s *session) callDataHandler(h DataMessageHandler, msg *DataMessage) {
	h(msg, s)
}
