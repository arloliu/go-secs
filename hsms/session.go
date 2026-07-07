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
// [session.recvDataMsg] delivers messages synchronously on the calling goroutine
// (the epoch-joined recv loop, G3) to both func handlers and channel handlers:
//
//   - Func handlers ([DataMessageHandler]) are called directly; no goroutine is spawned.
//   - Channel handlers (chan *[DataMessage]) are delivered via
//     select { case ch <- msg: case <-rt.Done(): return }
//     so a stalled receiver can never block the fan-out past connection teardown (J5).
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

// SessionID returns the HSMS session ID. Never blocks.
func (s *session) SessionID() uint16 { return s.id }

// SendDataMessage builds a primary data message and delegates to rt.WriteMessage.
// When replyExpected is true, WriteMessage waits for the T3-bounded reply; the B1
// IsSelected gate, I1 inflight accounting, and T3 timer enforcement are all owned
// by the engine (Task 12) inside WriteMessage — not here.
func (s *session) SendDataMessage(ctx context.Context, stream, function byte, replyExpected bool, item secs2.Item) (*DataMessage, error) {
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

// SendDataMessageAsync builds a data message and enqueues it on the per-generation
// async send channel via rt.SendAsync. No reply is awaited.
func (s *session) SendDataMessageAsync(ctx context.Context, stream, function byte, replyExpected bool, item secs2.Item) error {
	msg, err := NewDataMessage(stream, function, replyExpected, s.rt.SessionID(), s.sysGen.next(), item)
	if err != nil {
		return err
	}

	return s.rt.SendAsync(ctx, msg)
}

// SendSECS2Message builds an HSMS DataMessage from a [secs2.SECS2Message] (stream,
// function, W-bit, item) and delegates to rt.WriteMessage. Returns the reply DataMessage
// when the W-bit is set.
func (s *session) SendSECS2Message(ctx context.Context, msg secs2.SECS2Message) (*DataMessage, error) {
	dm, err := NewDataMessage(
		msg.StreamCode(), msg.FunctionCode(), msg.WaitBit(),
		s.rt.SessionID(), s.sysGen.next(), msg.Item(),
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

// ForwardDataMessage writes a pre-built data message verbatim (preserving its System Bytes, W-bit,
// session ID, and stream/function) via rt.WriteMessageNoReply and returns once the frame is on the
// wire. No reply is registered or consumed here — a secondary the peer sends is delivered to the
// registered DataMessageHandlers, so the caller owns reply correlation. See the
// SECS2Endpoint.ForwardDataMessage contract. Returns ErrNilMessage if msg is nil.
func (s *session) ForwardDataMessage(ctx context.Context, msg *DataMessage) error {
	if msg == nil {
		return ErrNilMessage
	}

	return s.rt.WriteMessageNoReply(ctx, msg)
}

// ForwardDataMessageAsync enqueues a pre-built data message verbatim on the per-generation async
// send channel via rt.SendAsync, preserving its full envelope. No reply is registered or consumed
// here (see ForwardDataMessage). Returns ErrNilMessage if msg is nil.
func (s *session) ForwardDataMessageAsync(ctx context.Context, msg *DataMessage) error {
	if msg == nil {
		return ErrNilMessage
	}

	return s.rt.SendAsync(ctx, msg)
}

// ReplyDataMessage sends a secondary data message in reply to primary. The reply function
// is primary.Function()+1 (SECS-II secondary-function convention: primary is odd, reply is
// even), replyExpected is false, and system bytes are taken verbatim from primary
// (E37 §8.2.6.9 — system bytes must match). The message is enqueued via rt.SendAsync
// (no W-bit, no reply correlation needed).
func (s *session) ReplyDataMessage(ctx context.Context, primary *DataMessage, item secs2.Item) error {
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

	return s.rt.SendAsync(ctx, dm)
}

// AddDataMessageHandler appends one or more inbound data-message handlers under mu.Lock.
// recvDataMsg snapshots the slice header under RLock, so concurrent registration and
// delivery are race-free.
func (s *session) AddDataMessageHandler(handlers ...DataMessageHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers = append(s.handlers, handlers...)
}

// AddDecodeErrorHandler appends one or more inbound decode-error handlers under mu.Lock.
// dispatchDecodeError snapshots the slice header under RLock, so concurrent registration
// and delivery are race-free.
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

// dispatchDecodeError delivers (msg, err) to every registered decode-error handler.
// Handlers are snapshotted under RLock and invoked with the lock released, matching
// recvDataMsg's fan-out discipline. The snapshot is safe because AddDecodeErrorHandler
// only ever appends and never mutates existing elements (a growing append writes only at
// indices >= the snapshot's length, into spare capacity or a fresh backing array), and
// the header read/write is mutex-synchronized — so ranging the aliased header after
// RUnlock cannot race a concurrent registration.
func (s *session) dispatchDecodeError(msg *DataMessage, err error) {
	s.mu.RLock()
	handlers := s.decodeErrHandlers
	s.mu.RUnlock()

	for _, h := range handlers {
		h(msg, err, s)
	}
}

// addChanHandler adds a channel-based data-message handler (unexported). The session
// delivers each inbound message to ch via a select that includes rt.Done() (J5),
// so a full channel can never block the fan-out past connection teardown. Called by
// the connection engine and in tests.
func (s *session) addChanHandler(ch chan *DataMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chans = append(s.chans, ch)
}

// AddConnStateChangeHandler registers state-change handlers on the Connection's
// persistent handler slice (spec §5.1). Uses a lock-free CAS loop to append atomically
// to the atomic.Pointer[[]StateChangeHandler] held by connHandlers.
//
// If connHandlers is nil (pre-Task-13 wiring), this call is a no-op; Task 13 wires the
// real pointer after session construction so subsequent calls reach the Connection's
// persistent storage.
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

// recvDataMsg delivers msg to every registered handler synchronously (§5.4, J5/G3).
// The same immutable *DataMessage pointer is passed to every handler — no Clone (D7).
//
// Fan-out order:
//  1. Func handlers ([DataMessageHandler]) are called directly (no goroutine spawned).
//  2. Channel handlers are delivered via
//     select { case ch <- msg: case <-s.rt.Done(): return }
//     so a full/stalled channel never blocks the fan-out past connection teardown (J5).
//
// Returns immediately if there are no handlers or if the generation is already torn down.
func (s *session) recvDataMsg(msg *DataMessage) {
	// Fast-path exit if the generation is already torn down.
	select {
	case <-s.rt.Done():
		return
	default:
	}

	s.mu.RLock()
	handlers := s.handlers
	chans := s.chans
	s.mu.RUnlock()

	for _, h := range handlers {
		h(msg, s)
	}

	for _, ch := range chans {
		select {
		case ch <- msg:
		case <-s.rt.Done():
			return
		}
	}
}
