package hsms

// abort_reply_error_test.go covers WithAbortReplyError:
// a function 0 reply (SxF0) to a reply-expected data send is returned with ErrTransactionAborted when the option is on,
// and as a plain reply when it is off.
// Every case also checks that the send leaves no reply registration, no inflight count, and no data-message error behind.

import (
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// mustAbortReply builds a header-only SxF0 of the given stream answering the primary with System Bytes sb.
func mustAbortReply(t *testing.T, stream uint8, sb [4]byte) *DataMessage {
	t.Helper()

	msg, err := NewDataMessage(stream, 0, false, 0xFFFF, sb, nil)
	require.NoError(t, err)

	return msg
}

// malformedAbortFrame returns an owned S1F0 frame (header + body, no length prefix) answering System Bytes sb,
// whose body claims a 255-byte binary item where only 1 byte follows, so its lazy decode fails.
func malformedAbortFrame(t *testing.T, sb [4]byte) []byte {
	t.Helper()

	good, err := NewDataMessage(1, 0, false, 0xFFFF, sb, secs2.NewBinaryItem([]byte{0x00}))
	require.NoError(t, err)

	raw := good.ToBytes() // length-prefixed HSMS frame: [4]len | [10]header | body

	const bodyLenByteOffset = 4 + 10 + 1 // len prefix + header + format byte
	raw[bodyLenByteOffset] = 0xFF

	msg, err := DecodeHSMSMessage(raw)
	require.NoError(t, err, "HSMS framing must decode; only the body is malformed")

	dm, ok := msg.ToDataMessage()
	require.True(t, ok)
	require.Error(t, dm.DecodeErr(), "body must fail to decode")

	return raw[4:]
}

// lastSentSystemBytes returns the System Bytes of the last frame mt captured.
// The captured frame is the full ToBytes output, so the header starts after the 4-byte length prefix.
func lastSentSystemBytes(t *testing.T, mt *mockTransport) [4]byte {
	t.Helper()

	frames := mt.writtenFrames()
	require.NotEmpty(t, frames, "the primary must be on the wire before its reply is routed")

	var sb [4]byte
	copy(sb[:], frames[len(frames)-1][10:14])

	return sb
}

// answerBeforeWait makes the next synchronous send receive the frame answer builds,
// delivered through the inbound path once the primary is on the wire and before the reply-wait select runs.
func answerBeforeWait(t *testing.T, c *connection, mt *mockTransport, answer func(sb [4]byte) []byte) {
	t.Helper()

	c.testHookBeforeReplyWait = func() {
		require.NoError(t, c.DeliverOwnedFrame(answer(lastSentSystemBytes(t, mt))))
	}
}

// requireTransactionClean checks that a finished send left no registration, no inflight count, and no counted error.
func requireTransactionClean(t *testing.T, c *connection) {
	t.Helper()

	require.Zero(t, c.cur.Load().replies.len(), "the reply registration must be removed")
	require.Equal(t, int64(0), c.metrics.DataMsgInflightCount(), "the inflight gauge must return to 0")
	require.Equal(t, uint64(0), c.metrics.DataMsgErrCount(), "a peer-answered send is never a data-message error")
}

// abortSendFuncs are the two session methods that wait for a reply through this package.
var abortSendFuncs = []struct {
	name string
	send func(t *testing.T, c *connection) (*DataMessage, error)
}{
	{
		name: "SendDataMessage",
		send: func(t *testing.T, c *connection) (*DataMessage, error) {
			return c.SendDataMessage(t.Context(), 1, 1, true, secs2.NewASCIIItem("PING"))
		},
	},
	{
		name: "SendSECS2Message",
		send: func(t *testing.T, c *connection) (*DataMessage, error) {
			return c.SendSECS2Message(t.Context(), secs2.NewMessage(1, 1, true, secs2.NewASCIIItem("PING")))
		},
	},
}

// TestAbortReplyError_SessionSends answers each reply-expected session send with an S1F0, with the option on and off.
// On, the send returns the S1F0 with ErrTransactionAborted and reports TxAborted;
// off, it returns the S1F0 as the reply and reports TxReplied.
// The metrics are the same either way.
//
// Teeth: dropping the reply arm's replyOutcome call, or session's ErrTransactionAborted branch, fails the "on" cases.
func TestAbortReplyError_SessionSends(t *testing.T) {
	for _, sf := range abortSendFuncs {
		for _, enabled := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/enabled=%v", sf.name, enabled), func(t *testing.T) {
				spy := &txEventSpy{}
				c, mt := newTestSendConn(t, SelectedState, WithAbortReplyError(enabled), WithTransactionObserver(spy.observe))

				var sb [4]byte
				answerBeforeWait(t, c, mt, func(got [4]byte) []byte {
					sb = got
					return ownedFrame(t, mustAbortReply(t, 1, got))
				})

				reply, err := sf.send(t, c)

				require.NotNil(t, reply, "the SxF0 must be returned either way")
				require.Equal(t, uint8(1), reply.Stream())
				require.Zero(t, reply.Function())
				require.Equal(t, sb, reply.SystemBytes())

				ev, ok := spy.last()
				require.True(t, ok, "the send must report a TxEvent")

				if enabled {
					require.Equal(t, ErrTransactionAborted, err)
					require.Equal(t, TxAborted, ev.Outcome)
					require.Equal(t, ErrTransactionAborted, ev.Err)
				} else {
					require.NoError(t, err)
					require.Equal(t, TxReplied, ev.Outcome)
					require.NoError(t, ev.Err)
				}

				requireTransactionClean(t, c)
				require.Equal(t, uint64(1), c.metrics.DataMsgSendCount())
				require.Equal(t, uint64(1), c.metrics.DataMsgRecvCount())
			})
		}
	}
}

// TestAbortReplyError_MalformedBody answers each session send with an S1F0 whose body fails to decode.
// On, the abort wins:
// the send returns ErrTransactionAborted, not the decode error, and the S1F0 still carries its decode error.
// Off, the existing decode-error return is unchanged.
func TestAbortReplyError_MalformedBody(t *testing.T) {
	for _, sf := range abortSendFuncs {
		for _, enabled := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/enabled=%v", sf.name, enabled), func(t *testing.T) {
				c, mt := newTestSendConn(t, SelectedState, WithAbortReplyError(enabled))
				answerBeforeWait(t, c, mt, func(sb [4]byte) []byte { return malformedAbortFrame(t, sb) })

				reply, err := sf.send(t, c)

				require.NotNil(t, reply)
				require.Zero(t, reply.Function())
				derr := reply.DecodeErr()
				require.Error(t, derr, "the S1F0 body must fail to decode")

				if enabled {
					require.Equal(t, ErrTransactionAborted, err)
				} else {
					require.Equal(t, derr, err)
				}

				requireTransactionClean(t, c)
			})
		}
	}
}

// TestAbortReplyError_RejectStillWins answers each session send with a peer Reject.req, with the option on.
// The option reports only an SxF0 reply as an abort,
// so the send still returns a *RejectError with no reply and reports TxRejected.
//
// Teeth: an abort check that overrides a non-nil reply error turns the Reject into ErrTransactionAborted.
func TestAbortReplyError_RejectStillWins(t *testing.T) {
	for _, sf := range abortSendFuncs {
		t.Run(sf.name, func(t *testing.T) {
			spy := &txEventSpy{}
			c, mt := newTestSendConn(t, SelectedState, WithAbortReplyError(true), WithTransactionObserver(spy.observe))

			c.testHookBeforeReplyWait = func() {
				reject, err := NewRejectReqRaw(0xFFFF, 0, 0, lastSentSystemBytes(t, mt), RejectNotSelected)
				require.NoError(t, err)
				require.True(t, c.RouteReply(reject), "the Reject.req must correlate to the waiting send")
			}

			reply, err := sf.send(t, c)

			var re *RejectError
			require.ErrorAs(t, err, &re, "a Reject.req must surface as a *RejectError, not ErrTransactionAborted")
			require.Equal(t, byte(RejectNotSelected), re.Reason)
			require.Nil(t, reply, "a rejected send returns no reply")

			ev, ok := spy.last()
			require.True(t, ok, "the send must report a TxEvent")
			require.Equal(t, TxRejected, ev.Outcome)

			require.Zero(t, c.cur.Load().replies.len(), "the reply registration must be removed")
			require.Equal(t, int64(0), c.metrics.DataMsgInflightCount(), "the inflight gauge must return to 0")
			require.Equal(t, uint64(0), c.metrics.DataMsgErrCount(), "a peer Reject is never a data-message error")
		})
	}
}

// TestAbortReplyError_TimerArmThenAbort routes an S1F0 right after the reply-wait select picked the protocol-timer arm.
// The arm's reply re-check returns the abort, not ErrT3Timeout, and never attempts the auto-S9F9 notification.
//
// Teeth: dropping the timer arm's replyOutcome call makes this return (S1F0, nil) and report TxReplied.
func TestAbortReplyError_TimerArmThenAbort(t *testing.T) {
	spy := &txEventSpy{}
	c, _ := newTestSendConn(t, SelectedState, WithAbortReplyError(true), WithAutoS9F9(true), WithTransactionObserver(spy.observe))
	c.cfg.Load().timers.T3 = time.Millisecond
	sb := [4]byte{0, 0, 0, 1}

	var s9f9Attempted atomic.Bool
	c.testHookAutoS9F9 = func() { s9f9Attempted.Store(true) }

	var picked replyWaitArm
	c.testHookWaitPicked = func(arm replyWaitArm) {
		picked = arm
		require.True(t, c.RouteReply(mustAbortReply(t, 1, sb)))
	}

	reply, err := c.WriteMessage(t.Context(), mustSendData(t, sb, true))

	require.Equal(t, replyWaitArmTimer, picked, "T3=1ms with nothing else ready must pick the timer arm")
	require.Equal(t, ErrTransactionAborted, err)
	require.NotNil(t, reply)
	require.False(t, s9f9Attempted.Load(), "an aborted transaction did not time out")

	ev, ok := spy.last()
	require.True(t, ok)
	require.Equal(t, TxAborted, ev.Outcome)

	requireTransactionClean(t, c)
}

// TestAbortReplyError_TeardownArmThenAbort routes an S1F0 right after the reply-wait select picked the teardown arm.
// The arm's reply re-check returns the abort, not ErrConnClosed.
//
// Teeth: dropping the teardown arm's replyOutcome call makes this return (S1F0, nil) and report TxReplied.
func TestAbortReplyError_TeardownArmThenAbort(t *testing.T) {
	spy := &txEventSpy{}
	c, _ := newTestSendConn(t, SelectedState, WithAbortReplyError(true), WithTransactionObserver(spy.observe))
	sb := [4]byte{0, 0, 0, 2}
	e := c.cur.Load()

	c.testHookBeforeReplyWait = func() { e.teardown(2 * time.Second) }

	var picked replyWaitArm
	c.testHookWaitPicked = func(arm replyWaitArm) {
		picked = arm
		require.True(t, c.RouteReply(mustAbortReply(t, 1, sb)))
	}

	reply, err := c.WriteMessage(t.Context(), mustSendData(t, sb, true))

	require.Equal(t, replyWaitArmTeardown, picked, "a teardown started before the select, with default T3, must pick the teardown arm")
	require.Equal(t, ErrTransactionAborted, err)
	require.NotNil(t, reply)

	ev, ok := spy.last()
	require.True(t, ok)
	require.Equal(t, TxAborted, ev.Outcome)

	requireTransactionClean(t, c)
}

// TestAbortReplyError_WrongStream answers an S1F1 with an S2F0, with strict reply matching off and on.
// Off, the S2F0 is accepted, counted as a mismatch, and returned with ErrTransactionAborted.
// On, the S2F0 is a mismatch that falls through to the data handlers,
// and the conforming S1F0 that follows ends the transaction with ErrTransactionAborted.
func TestAbortReplyError_WrongStream(t *testing.T) {
	tests := []struct {
		name        string
		strict      bool
		wantStream  uint8
		wantHandled bool
	}{
		{name: "strict off", strict: false, wantStream: 2, wantHandled: false},
		{name: "strict on", strict: true, wantStream: 1, wantHandled: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, mt := newTestSendConn(t, SelectedState, WithAbortReplyError(true), WithStrictReplyMatching(tt.strict))

			handlerCh := make(chan *DataMessage, 2)
			c.AddDataMessageHandler(func(msg *DataMessage, _ SECS2Endpoint) { handlerCh <- msg })

			c.testHookBeforeReplyWait = func() {
				sb := lastSentSystemBytes(t, mt)
				require.NoError(t, c.DeliverOwnedFrame(ownedFrame(t, mustAbortReply(t, 2, sb))))

				if tt.strict {
					require.NoError(t, c.DeliverOwnedFrame(ownedFrame(t, mustAbortReply(t, 1, sb))))
				}
			}

			reply, err := c.SendDataMessage(t.Context(), 1, 1, true, secs2.NewASCIIItem("PING"))

			require.Equal(t, ErrTransactionAborted, err)
			require.NotNil(t, reply)
			require.Equal(t, tt.wantStream, reply.Stream())
			require.Zero(t, reply.Function())
			require.Equal(t, uint64(1), c.metrics.ReplyMismatchCount())

			if tt.wantHandled {
				select {
				case got := <-handlerCh:
					require.Equal(t, uint8(2), got.Stream(), "the mismatched S2F0 must reach the data handlers")
					require.Zero(t, got.Function())
				case <-time.After(2 * time.Second):
					t.Fatal("the mismatched S2F0 never reached the data handlers")
				}
			}

			require.Empty(t, handlerCh, "no other message may reach the data handlers")
			requireTransactionClean(t, c)
		})
	}
}

// TestAbortReplyError_OrphanReachesHandlers delivers the same S1F0 again after the aborted send returned.
// No transaction claims it, so it reaches the data handlers as a plain secondary, with the option on.
func TestAbortReplyError_OrphanReachesHandlers(t *testing.T) {
	c, mt := newTestSendConn(t, SelectedState, WithAbortReplyError(true))

	handlerCh := make(chan *DataMessage, 1)
	c.AddDataMessageHandler(func(msg *DataMessage, _ SECS2Endpoint) { handlerCh <- msg })

	var sb [4]byte
	answerBeforeWait(t, c, mt, func(got [4]byte) []byte {
		sb = got
		return ownedFrame(t, mustAbortReply(t, 1, got))
	})

	_, err := c.SendDataMessage(t.Context(), 1, 1, true, secs2.NewASCIIItem("PING"))
	require.Equal(t, ErrTransactionAborted, err)
	require.Empty(t, handlerCh, "the claimed S1F0 must not also reach the data handlers")

	require.NoError(t, c.DeliverOwnedFrame(ownedFrame(t, mustAbortReply(t, 1, sb))))

	select {
	case got := <-handlerCh:
		require.Zero(t, got.Function())
		require.Equal(t, sb, got.SystemBytes())
	case <-time.After(2 * time.Second):
		t.Fatal("the orphan S1F0 never reached the data handlers")
	}
}

// TestAbortReplyError_ControlTransactionUnaffected answers a Linktest.req with a data S1F0 carrying its System Bytes.
// A control registration accepts any result uncompared, and the option applies only to data transactions,
// so the S1F0 is returned with a nil error.
//
// Teeth: dropping the isData condition from the option check makes this return ErrTransactionAborted.
func TestAbortReplyError_ControlTransactionUnaffected(t *testing.T) {
	c, _ := newTestSendConn(t, SelectedState, WithAbortReplyError(true))
	sb := [4]byte{0, 0, 0, 3}

	c.testHookBeforeReplyWait = func() {
		require.True(t, c.RouteReply(mustAbortReply(t, 1, sb)))
	}

	reply, err := c.sendWaitReply(t.Context(), NewLinktestReq(sb))

	require.NoError(t, err)
	require.NotNil(t, reply)
	require.Zero(t, c.cur.Load().replies.len())
}

// TestAbortReplyError_SessionKeepsReplyOnlyForAbort drives the session methods against a canned runtime.
// An ErrTransactionAborted result keeps its message, even one whose body fails to decode;
// any other error still drops the message.
//
// Teeth: removing the ErrTransactionAborted branch in either session method returns a nil message for the abort.
func TestAbortReplyError_SessionKeepsReplyOnlyForAbort(t *testing.T) {
	otherErr := errors.New("some other failure")

	tests := []struct {
		name    string
		err     error
		wantMsg bool
	}{
		{name: "abort keeps the message", err: ErrTransactionAborted, wantMsg: true},
		{name: "wrapped abort keeps the message", err: fmt.Errorf("wrapped: %w", ErrTransactionAborted), wantMsg: true},
		{name: "other error drops the message", err: otherErr, wantMsg: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := newMockRuntime(t)
			s := newSession(0xFFFF, rt, &sysBytesGen{})
			rt.writeReply = malformedDataMsg(t, 1, 0)
			rt.writeErr = tt.err

			dm1, err1 := s.SendDataMessage(t.Context(), 1, 1, true, secs2.NewEmptyItem())
			dm2, err2 := s.SendSECS2Message(t.Context(), secs2.NewMessage(1, 1, true, secs2.NewEmptyItem()))

			require.Equal(t, tt.err, err1, "the runtime's error must be returned unchanged, never the decode error")
			require.Equal(t, tt.err, err2, "the runtime's error must be returned unchanged, never the decode error")

			if tt.wantMsg {
				require.Same(t, rt.writeReply, dm1)
				require.Same(t, rt.writeReply, dm2)
			} else {
				require.Nil(t, dm1)
				require.Nil(t, dm2)
			}
		})
	}
}

// TestTxAborted_OutcomeAndString checks TxAborted's position after the earlier outcomes, its String form,
// and that the classifier maps ErrTransactionAborted, plain or wrapped, to it.
func TestTxAborted_OutcomeAndString(t *testing.T) {
	require.Equal(t, TxSendError+1, TxAborted, "TxAborted is appended, so the earlier outcomes keep their values")
	require.Equal(t, "Aborted", TxAborted.String())
	require.Equal(t, TxAborted, classifyTxOutcome(true, ErrTransactionAborted))
	require.Equal(t, TxAborted, classifyTxOutcome(true, fmt.Errorf("wrapped: %w", ErrTransactionAborted)))
	require.Equal(t, TxReplied, classifyTxOutcome(true, nil))
}
