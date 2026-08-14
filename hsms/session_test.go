package hsms

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// mustDataMsg creates a minimal DataMessage for use in tests.
func mustDataMsg(t *testing.T) *DataMessage {
	t.Helper()

	msg, err := NewDataMessage(1, 1, true, 0xFFFF, [4]byte{0, 0, 0, 1}, secs2.NewEmptyItem())
	require.NoError(t, err)

	return msg
}

// TestSession_FanOutDeliversSamePointer registers two DataMessageHandlers, delivers a
// message via recvDataMsg, and asserts that both handlers observe the exact same
// *DataMessage pointer (no Clone — D7).
func TestSession_FanOutDeliversSamePointer(t *testing.T) {
	rt := newMockRuntime(t)
	s := newSession(0xFFFF, rt, &sysBytesGen{})

	var got1, got2 *DataMessage
	var wg sync.WaitGroup
	wg.Add(2)
	s.AddDataMessageHandler(
		func(m *DataMessage, ep SECS2Endpoint) { got1 = m; wg.Done() },
		func(m *DataMessage, ep SECS2Endpoint) { got2 = m; wg.Done() },
	)

	msg := mustDataMsg(t)
	s.recvDataMsg(msg)
	wg.Wait()

	require.Same(t, msg, got1)
	require.Same(t, got1, got2, "every handler gets the SAME immutable pointer (no Clone, D7)")
}

// TestSession_SessionID verifies the SECS2Endpoint identity accessor (SessionID, the sole
// SessionID method on the endpoint surface) reports the session's ID through the interface.
func TestSession_SessionID(t *testing.T) {
	rt := newMockRuntime(t)
	var ep SECS2Endpoint = newSession(0x1234, rt, &sysBytesGen{})
	require.Equal(t, uint16(0x1234), ep.SessionID())
}

// TestSession_J5_BlockedChannelDoesNotWedgeFanOut verifies that a full/stalled channel
// handler cannot block recvDataMsg past connection teardown (J5).
//
// Design: an unbuffered channel with no reader is registered. The goroutine running
// recvDataMsg blocks on the channel-delivery select. Closing rt.Done() must unblock it.
//
// Teeth-check: temporarily omit `case <-s.rt.Done()` from the channel-delivery select
// in session.go and run this test — it times out at 2 s, confirming J5 is real.
func TestSession_J5_BlockedChannelDoesNotWedgeFanOut(t *testing.T) {
	rt := newMockRuntime(t)
	s := newSession(0xFFFF, rt, &sysBytesGen{})

	// Unbuffered channel with no reader: ch <- msg would block indefinitely without
	// the <-rt.Done() guard in the select.
	blockedCh := make(chan *DataMessage)
	s.AddDataMessageChan(blockedCh)

	msg := mustDataMsg(t)

	recvDone := make(chan struct{})
	go func() {
		defer close(recvDone)
		s.recvDataMsg(msg)
	}()

	// Give the goroutine time to enter the channel-delivery select before closing Done.
	// Even if closeDone fires first, the early-exit in recvDataMsg returns immediately —
	// the test still passes because the fan-out did not block permanently.
	time.Sleep(20 * time.Millisecond)
	rt.closeDone()

	select {
	case <-recvDone:
		// J5 satisfied: fan-out unblocked when rt.Done() closed.
	case <-time.After(2 * time.Second):
		t.Fatal("J5 violated: recvDataMsg did not return after rt.Done() was closed")
	}
}

// TestSession_AddDataMessageChan_NilPanics verifies that registering a nil channel panics at
// registration time (edge case 1): a nil send case would make every delivery select wait only
// on rt.Done(), wedging the connection from the first inbound message.
func TestSession_AddDataMessageChan_NilPanics(t *testing.T) {
	rt := newMockRuntime(t)
	s := newSession(0xFFFF, rt, &sysBytesGen{})

	require.Panics(t, func() { s.AddDataMessageChan(nil) })
}

// TestSession_AddDataMessageChan_DuplicateRegistrationDeliversTwice verifies that registering
// the same channel twice delivers each message once per registration (edge case 6), matching
// DataMessageHandler's un-deduplicated registration.
func TestSession_AddDataMessageChan_DuplicateRegistrationDeliversTwice(t *testing.T) {
	rt := newMockRuntime(t)
	s := newSession(0xFFFF, rt, &sysBytesGen{})

	ch := make(chan *DataMessage, 2)
	s.AddDataMessageChan(ch)
	s.AddDataMessageChan(ch) // same channel, registered a second time

	msg := mustDataMsg(t)
	s.recvDataMsg(msg)

	require.Same(t, msg, <-ch, "first registration must deliver")
	require.Same(t, msg, <-ch, "second registration must deliver independently")

	select {
	case <-ch:
		t.Fatal("channel must receive exactly twice, once per registration, not deduplicated")
	default:
	}
}

// TestSession_AddDataMessageChan_FIFOWithFuncHandlers verifies that a registered channel
// receives messages in arrival order (FIFO) and observes the same sequence a func handler does.
func TestSession_AddDataMessageChan_FIFOWithFuncHandlers(t *testing.T) {
	rt := newMockRuntime(t)
	s := newSession(0xFFFF, rt, &sysBytesGen{})

	var mu sync.Mutex
	var handlerOrder []uint8
	s.AddDataMessageHandler(func(m *DataMessage, _ SECS2Endpoint) {
		mu.Lock()
		handlerOrder = append(handlerOrder, m.Function())
		mu.Unlock()
	})

	ch := make(chan *DataMessage, 4)
	s.AddDataMessageChan(ch)

	msgs := make([]*DataMessage, 0, 3)
	for _, fn := range []uint8{1, 3, 5} {
		m, err := NewDataMessage(1, fn, false, 0xFFFF, [4]byte{0, 0, 0, fn}, secs2.NewEmptyItem())
		require.NoError(t, err)
		msgs = append(msgs, m)
	}

	for _, m := range msgs {
		s.recvDataMsg(m)
	}

	mu.Lock()
	require.Equal(t, []uint8{1, 3, 5}, handlerOrder, "func handler must observe arrival order")
	mu.Unlock()

	for _, want := range msgs {
		require.Same(t, want, <-ch, "channel must observe the same arrival order")
	}
}

// TestAddDecodeErrorHandler_registrationAndDispatch verifies that decode-error handlers
// register under the session lock and that dispatchDecodeError delivers the exact
// (msg, err) pair to every registered handler, passing the session as the endpoint.
func TestAddDecodeErrorHandler_registrationAndDispatch(t *testing.T) {
	s := newSession(1, nil, &sysBytesGen{}) // rt nil is fine; we only test registration+dispatch

	if s.hasDecodeErrorHandlers() {
		t.Fatal("hasDecodeErrorHandlers() = true before any registration")
	}

	var gotErr error
	var gotMsg *DataMessage
	s.AddDecodeErrorHandler(func(msg *DataMessage, err error, _ SECS2Endpoint) {
		gotMsg, gotErr = msg, err
	})

	if !s.hasDecodeErrorHandlers() {
		t.Fatal("hasDecodeErrorHandlers() = false after registration")
	}

	want := errors.New("boom")
	dm := &DataMessage{}
	s.dispatchDecodeError(dm, want)

	if !errors.Is(gotErr, want) {
		t.Errorf("handler err = %v, want %v", gotErr, want)
	}
	if gotMsg != dm {
		t.Error("handler received the wrong message pointer")
	}
}

// TestSession_SendDataMessage_DelegatesToWriteMessage is a smoke test confirming that
// SendDataMessage builds a DataMessage and calls rt.WriteMessage with it.
func TestSession_SendDataMessage_DelegatesToWriteMessage(t *testing.T) {
	rt := newMockRuntime(t)
	s := newSession(0xFFFF, rt, &sysBytesGen{})

	// Canned reply the mock will return.
	replyMsg, err := NewDataMessage(1, 2, false, 0xFFFF, [4]byte{0, 0, 0, 1}, secs2.NewEmptyItem())
	require.NoError(t, err)
	rt.writeReply = replyMsg

	reply, err := s.SendDataMessage(context.Background(), 1, 1, true, secs2.NewEmptyItem())
	require.NoError(t, err)

	rt.mu.Lock()
	writeCalled := rt.writeCalled
	writeMsg := rt.writeMsg
	rt.mu.Unlock()

	require.True(t, writeCalled, "WriteMessage must be called")
	require.NotNil(t, writeMsg, "WriteMessage must receive a message")

	dm, ok := writeMsg.(*DataMessage)
	require.True(t, ok, "message passed to WriteMessage must be a *DataMessage")
	require.Equal(t, uint8(1), dm.Stream())
	require.Equal(t, uint8(1), dm.Function())
	require.True(t, dm.WaitBit())
	require.Equal(t, replyMsg, reply, "SendDataMessage must return the reply from WriteMessage")
}

// malformedDataMsg builds a *DataMessage whose HSMS framing is valid (header accessors
// succeed) but whose SECS-II body fails to decode lazily: DecodeErr() != nil. It replicates
// the hsmstest.MalformedDataMessage trick here because a white-box package hsms test cannot
// import hsmstest (which imports hsms) without an import cycle.
func malformedDataMsg(t *testing.T, stream, function uint8, waitBit bool) *DataMessage {
	t.Helper()

	good, err := NewDataMessage(stream, function, waitBit, 0, [4]byte{0, 0, 0, 1}, secs2.NewBinaryItem([]byte{0x00}))
	require.NoError(t, err)

	raw := good.ToBytes() // length-prefixed HSMS frame: [4]len | [10]header | body

	const bodyLenByteOffset = 4 + 10 + 1 // len prefix + header + format byte
	require.Greater(t, len(raw), bodyLenByteOffset, "base frame shorter than expected")
	raw[bodyLenByteOffset] = 0xFF // claim a 255-byte item where only 1 byte follows

	msg, decErr := DecodeHSMSMessage(raw)
	require.NoError(t, decErr, "HSMS framing must decode; only the body is malformed")

	dm, ok := msg.ToDataMessage()
	require.True(t, ok, "decoded message must be a data message")
	require.Error(t, dm.DecodeErr(), "body must fail to decode")

	return dm
}

// paddedDataMsg builds a *DataMessage whose body is a single well-formed SECS-II item followed by
// padding bytes: the outer HSMS length prefix counts the padding as part of the body,
// but the padding is not part of the item's own encoding.
// DecodeErr() stays nil (the first item decodes cleanly) and TrailingBytes() reports len(padding) —
// the field-realistic shape this task's TrailingBytes diagnostic targets (SEMI E5 §10.3.1.3(2)).
func paddedDataMsg(t *testing.T, stream, function uint8, waitBit bool, padding []byte) *DataMessage {
	t.Helper()

	good, err := NewDataMessage(stream, function, waitBit, 0, [4]byte{0, 0, 0, 1}, secs2.NewASCIIItem("padded"))
	require.NoError(t, err)

	header := good.HeaderBytes()
	body := append(good.AppendBodyTo(nil), padding...)

	length := uint32(len(header) + len(body)) //nolint:gosec // test-controlled body length, never near uint32 overflow
	frame := make([]byte, 0, 4+len(header)+len(body))
	frame = append(frame, byte(length>>24), byte(length>>16), byte(length>>8), byte(length))
	frame = append(frame, header[:]...)
	frame = append(frame, body...)

	msg, decErr := DecodeHSMSMessage(frame)
	require.NoError(t, decErr, "padded frame must decode; only the body carries extra bytes")

	dm, ok := msg.ToDataMessage()
	require.True(t, ok)

	return dm
}

// TestSession_SendDataMessage_paddedReplySucceeds is the counter-assertion this task adds:
// a synchronous SendDataMessage whose reply body carries trailing padding must return SUCCESS
// (nil error), with the trailing count exposed on the returned message — padding never fails a
// transaction, only DecodeErr does.
func TestSession_SendDataMessage_paddedReplySucceeds(t *testing.T) {
	rt := newMockRuntime(t)
	s := newSession(0xFFFF, rt, &sysBytesGen{})

	padding := []byte{0xAA, 0xBB, 0xCC}
	rt.writeReply = paddedDataMsg(t, 1, 14, false, padding)

	dm, err := s.SendDataMessage(t.Context(), 1, 13, true, secs2.NewEmptyItem())
	require.NoError(t, err, "padding in the reply body must not fail the transaction")
	require.NotNil(t, dm)
	require.Nil(t, dm.DecodeErr())
	require.Equal(t, len(padding), dm.TrailingBytes())
}

// TestSession_SendDataMessage_malformedReplyReturnsErrAndMsg verifies that when the reply's
// SECS-II body fails to decode, SendDataMessage surfaces the decode error but still returns the
// reply message alongside it (non-destructive: the header stays available to the caller).
func TestSession_SendDataMessage_malformedReplyReturnsErrAndMsg(t *testing.T) {
	rt := newMockRuntime(t)
	s := newSession(0xFFFF, rt, &sysBytesGen{})

	rt.writeReply = malformedDataMsg(t, 1, 14, false)

	dm, err := s.SendDataMessage(context.Background(), 1, 13, true, secs2.NewEmptyItem())
	require.Error(t, err, "SendDataMessage must not return a nil error for an undecodable reply")
	require.NotNil(t, dm, "SendDataMessage must return the reply alongside the error")
	require.Equal(t, uint8(1), dm.Stream())
	require.Equal(t, uint8(14), dm.Function())
}

// TestSession_SendSECS2Message_malformedReplyReturnsErrAndMsg mirrors the reply-path decode
// check for the SendSECS2Message wrapper.
func TestSession_SendSECS2Message_malformedReplyReturnsErrAndMsg(t *testing.T) {
	rt := newMockRuntime(t)
	s := newSession(0xFFFF, rt, &sysBytesGen{})

	rt.writeReply = malformedDataMsg(t, 1, 14, false)

	primary := secs2.NewMessage(1, 13, true, secs2.NewEmptyItem())
	dm, err := s.SendSECS2Message(context.Background(), primary)
	require.Error(t, err, "SendSECS2Message must not return a nil error for an undecodable reply")
	require.NotNil(t, dm, "SendSECS2Message must return the reply alongside the error")
	require.Equal(t, uint8(1), dm.Stream())
	require.Equal(t, uint8(14), dm.Function())
}

// TestSession_SendDataMessage_wellFormedReplyReturnsNilErr proves the reply-path decode check
// does not disturb the happy path: a well-formed reply is returned as (dm, nil).
func TestSession_SendDataMessage_wellFormedReplyReturnsNilErr(t *testing.T) {
	rt := newMockRuntime(t)
	s := newSession(0xFFFF, rt, &sysBytesGen{})

	replyMsg, err := NewDataMessage(1, 14, false, 0xFFFF, [4]byte{0, 0, 0, 1}, secs2.NewEmptyItem())
	require.NoError(t, err)
	rt.writeReply = replyMsg

	dm, err := s.SendDataMessage(context.Background(), 1, 13, true, secs2.NewEmptyItem())
	require.NoError(t, err, "well-formed reply must return a nil error")
	require.Same(t, replyMsg, dm)
}

// TestSession_SendDataMessage_RejectsEvenFunctionPrimary verifies that SendDataMessage refuses
// an even-function send with ErrEvenFunctionPrimary (E5 §7.2): SendDataMessage always mints
// fresh System Bytes, so it always opens a new transaction and is always a primary.
func TestSession_SendDataMessage_RejectsEvenFunctionPrimary(t *testing.T) {
	rt := newMockRuntime(t)
	s := newSession(0xFFFF, rt, &sysBytesGen{})

	dm, err := s.SendDataMessage(t.Context(), 1, 2, false, secs2.NewEmptyItem())
	require.ErrorIs(t, err, ErrEvenFunctionPrimary)
	require.Nil(t, dm)

	rt.mu.Lock()
	writeCalled := rt.writeCalled
	rt.mu.Unlock()
	require.False(t, writeCalled, "a refused send must never reach WriteMessage")
}

// TestSession_SendDataMessageAsync_RejectsEvenFunctionPrimary mirrors the odd-function guard for
// the async entry point.
func TestSession_SendDataMessageAsync_RejectsEvenFunctionPrimary(t *testing.T) {
	rt := newMockRuntime(t)
	s := newSession(0xFFFF, rt, &sysBytesGen{})

	err := s.SendDataMessageAsync(t.Context(), 1, 2, false, secs2.NewEmptyItem())
	require.ErrorIs(t, err, ErrEvenFunctionPrimary)

	rt.mu.Lock()
	asyncCalled := rt.asyncCalled
	rt.mu.Unlock()
	require.False(t, asyncCalled, "a refused send must never reach SendAsync")
}

// TestSession_SendSECS2Message_RejectsNilMessage proves SendSECS2Message guards a nil message:
// it rejects a nil interface value and a typed-nil *secs2.Message,
// returning ErrNilMessage without panicking and without reaching WriteMessage.
func TestSession_SendSECS2Message_RejectsNilMessage(t *testing.T) {
	rt := newMockRuntime(t)
	s := newSession(0xFFFF, rt, &sysBytesGen{})

	var nilTyped *secs2.Message

	tests := []struct {
		name string
		msg  secs2.SECS2Message
	}{
		{name: "interface nil", msg: nil},
		{name: "typed nil", msg: nilTyped},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.NotPanics(t, func() {
				dm, err := s.SendSECS2Message(t.Context(), tt.msg)
				require.ErrorIs(t, err, ErrNilMessage)
				require.Nil(t, dm)
			})

			rt.mu.Lock()
			writeCalled := rt.writeCalled
			rt.mu.Unlock()
			require.False(t, writeCalled, "a nil message must never reach WriteMessage")
		})
	}
}

// TestSession_SendSECS2Message_RejectsEvenFunctionPrimary mirrors the odd-function guard for the
// secs2.SECS2Message entry point.
func TestSession_SendSECS2Message_RejectsEvenFunctionPrimary(t *testing.T) {
	rt := newMockRuntime(t)
	s := newSession(0xFFFF, rt, &sysBytesGen{})

	msg := secs2.NewMessage(1, 2, false, secs2.NewEmptyItem())
	dm, err := s.SendSECS2Message(t.Context(), msg)
	require.ErrorIs(t, err, ErrEvenFunctionPrimary)
	require.Nil(t, dm)

	rt.mu.Lock()
	writeCalled := rt.writeCalled
	rt.mu.Unlock()
	require.False(t, writeCalled, "a refused send must never reach WriteMessage")
}

// flipFunctionMessage is a secs2.SECS2Message stub whose FunctionCode() is call-sensitive:
// it returns an odd value on the first call and an even value on every call after.
// It reproduces the TOCTOU attack shape: a hostile externally implementable message that hands
// an odd function to a guard's first read, then an even function to a second read used for
// frame construction — defeating the odd-function-primary guard on the wire.
type flipFunctionMessage struct {
	calls int
}

func (m *flipFunctionMessage) StreamCode() uint8 { return 1 }

func (m *flipFunctionMessage) FunctionCode() uint8 {
	m.calls++
	if m.calls == 1 {
		return 1 // odd: passes the guard
	}

	return 2 // even: would poison construction if re-read
}

func (m *flipFunctionMessage) WaitBit() bool    { return false }
func (m *flipFunctionMessage) Item() secs2.Item { return secs2.NewEmptyItem() }

// TestSession_SendSECS2Message_SnapshotsFunctionCode proves the TOCTOU fix: SendSECS2Message
// reads FunctionCode() exactly once and both validates and constructs from that single snapshot.
// For a stub returning odd on the first call and even on every call after, the guard passes
// (the snapshot is odd) and the frame sent to WriteMessage must carry that same odd function —
// never the poisoned even value a second read would produce — and FunctionCode() must be
// called exactly once.
func TestSession_SendSECS2Message_SnapshotsFunctionCode(t *testing.T) {
	rt := newMockRuntime(t)
	s := newSession(0xFFFF, rt, &sysBytesGen{})

	replyMsg, err := NewDataMessage(1, 2, false, 0xFFFF, [4]byte{0, 0, 0, 1}, secs2.NewEmptyItem())
	require.NoError(t, err)
	rt.writeReply = replyMsg

	msg := &flipFunctionMessage{}
	dm, err := s.SendSECS2Message(t.Context(), msg)
	require.NoError(t, err, "the first-read snapshot is odd, so the guard must pass and the send must proceed")
	require.NotNil(t, dm)
	require.Equal(t, 1, msg.calls, "FunctionCode must be read exactly once, never re-read for construction")

	rt.mu.Lock()
	writeMsg := rt.writeMsg
	rt.mu.Unlock()

	sent, ok := writeMsg.(*DataMessage)
	require.True(t, ok, "message passed to WriteMessage must be a *DataMessage")
	require.Equal(t, uint8(1), sent.Function(), "the sent frame must use the first-read (odd) snapshot, never a re-read even value")
}

// TestSession_ReplyDataMessage_AllowsEvenFunction is the counter-assertion: ReplyDataMessage
// derives an even function (primary.Function()+1) and must keep succeeding.
// It catches an over-fix that places the odd-function guard in NewDataMessage instead of the
// three primary entry points.
func TestSession_ReplyDataMessage_AllowsEvenFunction(t *testing.T) {
	rt := newMockRuntime(t)
	s := newSession(0xFFFF, rt, &sysBytesGen{})

	primary, err := NewDataMessage(1, 1, true, 0xFFFF, [4]byte{0, 0, 0, 1}, secs2.NewEmptyItem())
	require.NoError(t, err)

	err = s.ReplyDataMessage(t.Context(), primary, secs2.NewEmptyItem())
	require.NoError(t, err)

	rt.mu.Lock()
	asyncMsg := rt.asyncMsg
	rt.mu.Unlock()

	dm, ok := asyncMsg.(*DataMessage)
	require.True(t, ok)
	require.Equal(t, uint8(2), dm.Function(), "reply function must be primary.Function()+1 (even)")
}

func TestSession_ReplyDataMessage_NilPrimary(t *testing.T) {
	rt := newMockRuntime(t)
	s := newSession(0xFFFF, rt, &sysBytesGen{})

	err := s.ReplyDataMessage(t.Context(), nil, secs2.NewEmptyItem())
	require.ErrorIs(t, err, ErrNilMessage)
}

func TestSession_ReplyDataMessage_Function255UsesFunction0(t *testing.T) {
	rt := newMockRuntime(t)
	s := newSession(0xFFFF, rt, &sysBytesGen{})
	primary, err := NewDataMessage(1, 255, true, 0xFFFF, [4]byte{0, 0, 0, 1}, secs2.NewEmptyItem())
	require.NoError(t, err)

	require.NoError(t, s.ReplyDataMessage(t.Context(), primary, secs2.NewEmptyItem()))
	rt.mu.Lock()
	sent := rt.asyncMsg
	rt.mu.Unlock()

	dm, ok := sent.(*DataMessage)
	require.True(t, ok)
	require.Equal(t, uint8(0), dm.Function())
}

// TestSession_ForwardDataMessage_AllowsEvenFunction is the counter-assertion for the forward
// path: ForwardDataMessage bypasses construction entirely and must forward an even-function
// message verbatim.
// It catches an over-fix that places the odd-function guard in NewDataMessage instead of the
// three primary entry points.
func TestSession_ForwardDataMessage_AllowsEvenFunction(t *testing.T) {
	rt := newMockRuntime(t)
	s := newSession(0xFFFF, rt, &sysBytesGen{})

	evenMsg, err := NewDataMessage(1, 2, false, 0xFFFF, [4]byte{0, 0, 0, 1}, secs2.NewEmptyItem())
	require.NoError(t, err)

	err = s.ForwardDataMessage(t.Context(), evenMsg)
	require.NoError(t, err)

	rt.mu.Lock()
	writeMsg := rt.writeMsg
	rt.mu.Unlock()
	require.Same(t, evenMsg, writeMsg, "ForwardDataMessage must forward the message verbatim")
}

// TestSession_SendDataMessageAsync_RejectsReplyExpected verifies that SendDataMessageAsync
// refuses a send with replyExpected true: SendAsync never begins a reply timer (E37 §9.4.1.2),
// so a W-bit primary sent through it can never be conformantly answered.
// Reply-bearing transactions must go through SendDataMessage instead.
func TestSession_SendDataMessageAsync_RejectsReplyExpected(t *testing.T) {
	rt := newMockRuntime(t)
	s := newSession(0xFFFF, rt, &sysBytesGen{})

	err := s.SendDataMessageAsync(t.Context(), 1, 1, true, secs2.NewEmptyItem())
	require.ErrorIs(t, err, ErrAsyncReplyExpected)

	rt.mu.Lock()
	asyncCalled := rt.asyncCalled
	rt.mu.Unlock()
	require.False(t, asyncCalled, "a refused send must never reach SendAsync")
}

// TestSession_SendDataMessageAsync_AllowsReplyExpectedFalse proves the replyExpected guard does
// not disturb the ordinary fire-and-forget path.
func TestSession_SendDataMessageAsync_AllowsReplyExpectedFalse(t *testing.T) {
	rt := newMockRuntime(t)
	s := newSession(0xFFFF, rt, &sysBytesGen{})

	err := s.SendDataMessageAsync(t.Context(), 1, 1, false, secs2.NewEmptyItem())
	require.NoError(t, err)

	rt.mu.Lock()
	asyncCalled := rt.asyncCalled
	rt.mu.Unlock()
	require.True(t, asyncCalled, "SendDataMessageAsync must still reach SendAsync when replyExpected is false")
}

// TestSession_SendDataMessageAsync_RejectsEvenFunctionAndReplyExpected covers the guard-order
// case where both violations are present at once: an even function AND replyExpected true.
// The even-function check runs first, so ErrEvenFunctionPrimary (not ErrAsyncReplyExpected) is
// the error returned.
func TestSession_SendDataMessageAsync_RejectsEvenFunctionAndReplyExpected(t *testing.T) {
	rt := newMockRuntime(t)
	s := newSession(0xFFFF, rt, &sysBytesGen{})

	err := s.SendDataMessageAsync(t.Context(), 1, 2, true, secs2.NewEmptyItem())
	require.ErrorIs(t, err, ErrEvenFunctionPrimary)
	require.NotErrorIs(t, err, ErrAsyncReplyExpected)

	rt.mu.Lock()
	asyncCalled := rt.asyncCalled
	rt.mu.Unlock()
	require.False(t, asyncCalled, "a refused send must never reach SendAsync")
}
