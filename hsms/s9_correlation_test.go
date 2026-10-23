package hsms

// s9_correlation_test.go covers WithS9Correlation:
// an S9F1, S9F3, S9F5, S9F7, or S9F11 ends a send still waiting for its reply with an *S9Error
// when its MHEAD body identifies that send,
// and the S9 still reaches the data handlers exactly as it would without the option.
// Every send-level case also checks what the send leaves behind:
// no reply registration, no inflight count, and no data-message error.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// s9Functions are the S9 functions whose body is the MHEAD of a message we sent.
var s9Functions = []byte{1, 3, 5, 7, 11}

// mustS9 builds an S9 message of function f with the envelope System Bytes sb and the given body.
func mustS9(t *testing.T, f byte, sb [4]byte, body secs2.Item) *DataMessage {
	t.Helper()

	msg, err := NewDataMessage(9, f, false, 0xFFFF, sb, body)
	require.NoError(t, err)

	return msg
}

// s9Envelope is the System Bytes an S9 notification carries in these tests.
// The peer draws them fresh for the notification, so they never equal the faulted message's.
var s9Envelope = [4]byte{0, 0, 0x99, 0x99}

// mustS9For builds an S9 of function f whose body is the 10-byte MHEAD mhead.
func mustS9For(t *testing.T, f byte, mhead [10]byte) *DataMessage {
	t.Helper()

	return mustS9(t, f, s9Envelope, secs2.NewBinaryItem(mhead[:]))
}

// lastSentHeader returns the 10-byte header of the last frame mt captured.
// The captured frame is the full ToBytes output, so the header starts after the 4-byte length prefix.
func lastSentHeader(t *testing.T, mt *mockTransport) [10]byte {
	t.Helper()

	frames := mt.writtenFrames()
	require.NotEmpty(t, frames, "the primary must be on the wire before the S9 is delivered")

	return [10]byte(frames[len(frames)-1][4:14])
}

// deliverBeforeWait makes the next synchronous send receive, through the inbound path,
// every frame frames builds from the header the primary was sent with,
// once the primary is on the wire and before the reply-wait select runs.
func deliverBeforeWait(t *testing.T, c *connection, mt *mockTransport, frames func(sent [10]byte) [][]byte) {
	t.Helper()

	c.testHookBeforeReplyWait = func() {
		for _, f := range frames(lastSentHeader(t, mt)) {
			require.NoError(t, c.DeliverOwnedFrame(f))
		}
	}
}

// handlerSpy collects every message the data handlers receive.
type handlerSpy struct {
	ch chan *DataMessage
}

func newHandlerSpy(c *connection) *handlerSpy {
	s := &handlerSpy{ch: make(chan *DataMessage, 8)}
	c.AddDataMessageHandler(func(msg *DataMessage, _ SECS2Endpoint) { s.ch <- msg })

	return s
}

// next returns the next message the data handlers received.
func (s *handlerSpy) next(t *testing.T) *DataMessage {
	t.Helper()

	select {
	case msg := <-s.ch:
		return msg
	case <-time.After(2 * time.Second):
		t.Fatal("no message reached the data handlers")

		return nil
	}
}

// requireS9 checks that msg is the S9 of function f these tests send.
func (s *handlerSpy) requireS9(t *testing.T, f byte) {
	t.Helper()

	msg := s.next(t)
	require.Equal(t, uint8(9), msg.Stream(), "the S9 must reach the data handlers")
	require.Equal(t, f, msg.Function())
	require.Equal(t, s9Envelope, msg.SystemBytes())
}

// requireEmpty checks that the data handlers received nothing else.
func (s *handlerSpy) requireEmpty(t *testing.T) {
	t.Helper()

	select {
	case msg := <-s.ch:
		t.Fatalf("unexpected message at the data handlers: S%dF%d", msg.Stream(), msg.Function())
	default:
	}
}

// requireS9Error checks that err is the *S9Error of function f for the message sent with header sent,
// carrying the S9 message itself.
func requireS9Error(t *testing.T, err error, f byte, sent [10]byte) {
	t.Helper()

	var s9Err *S9Error
	require.ErrorAs(t, err, &s9Err)
	require.Equal(t, f, s9Err.Function)
	require.Equal(t, sent, s9Err.MHEAD)
	require.NotNil(t, s9Err.Msg, "the S9 message must be carried")
	require.Equal(t, uint8(9), s9Err.Msg.Stream())
	require.Equal(t, f, s9Err.Msg.Function())
	require.Equal(t, s9Envelope, s9Err.Msg.SystemBytes())
}

// TestS9Correlation_EachFunctionEndsSend answers each reply-expected session send with each S9 function whose body is its MHEAD.
// The send returns (nil, *S9Error) and reports TxPeerError,
// the data handlers receive the S9 as well,
// and no metric counts it as an error.
// No auto S9F9 is attempted: the send did not time out.
//
// TEETH: drop correlateS9On's call and every row waits out T3;
// key failS9's lookup on the S9 envelope's System Bytes instead of the MHEAD's and every row waits out T3,
// since s9Envelope differs from the sent System Bytes;
// return before routeDataOn on a match and the handler assertion fails.
func TestS9Correlation_EachFunctionEndsSend(t *testing.T) {
	for _, sf := range abortSendFuncs {
		for _, f := range s9Functions {
			t.Run(fmt.Sprintf("%s/S9F%d", sf.name, f), func(t *testing.T) {
				spy := &txEventSpy{}
				c, mt := newTestSendConn(t, SelectedState,
					WithS9Correlation(true), WithAutoS9F9(true), WithTransactionObserver(spy.observe))
				handlers := newHandlerSpy(c)

				var s9f9Attempted atomic.Bool
				c.testHookAutoS9F9 = func() { s9f9Attempted.Store(true) }

				var sent [10]byte
				deliverBeforeWait(t, c, mt, func(h [10]byte) [][]byte {
					sent = h
					return [][]byte{ownedFrame(t, mustS9For(t, f, h))}
				})

				reply, err := sf.send(t, c)

				require.Nil(t, reply)
				requireS9Error(t, err, f, sent)
				require.False(t, IsTransient(err))
				require.False(t, IsTimeout(err))

				handlers.requireS9(t, f)
				handlers.requireEmpty(t)

				ev, ok := spy.last()
				require.True(t, ok, "the send must report a TxEvent")
				require.Equal(t, TxPeerError, ev.Outcome)
				requireS9Error(t, ev.Err, f, sent)

				require.False(t, s9f9Attempted.Load(), "a send the peer faulted did not time out")
				requireTransactionClean(t, c)
				require.Equal(t, uint64(1), c.metrics.DataMsgSendCount())
				require.Equal(t, uint64(1), c.metrics.DataMsgRecvCount())
				require.Zero(t, c.metrics.ReplyMismatchCount())
			})
		}
	}
}

// TestS9Error_Error checks the message names the faulted stream and function from MHEAD and the S9 function's meaning.
func TestS9Error_Error(t *testing.T) {
	mhead := [10]byte{0, 1, 0x81, 3, 0, 0, 0, 0, 0, 1}

	tests := []struct {
		f    byte
		want string
	}{
		{1, "hsms: peer answered S1F3 with S9F1 (unrecognized device ID)"},
		{3, "hsms: peer answered S1F3 with S9F3 (unrecognized stream type)"},
		{5, "hsms: peer answered S1F3 with S9F5 (unrecognized function type)"},
		{7, "hsms: peer answered S1F3 with S9F7 (illegal data)"},
		{11, "hsms: peer answered S1F3 with S9F11 (data too long)"},
		{9, "hsms: peer answered S1F3 with S9F9 (error)"},
	}

	for _, tt := range tests {
		require.Equal(t, tt.want, (&S9Error{Function: tt.f, MHEAD: mhead}).Error())
	}
}

// TestS9Correlation_NoMatch delivers an S9 that must not end the send, followed by the genuine reply.
// The send returns the reply, and the data handlers receive the S9.
//
// TEETH: key failS9's lookup on the S9 envelope's System Bytes and also skip its header comparison,
// and the "envelope" row ends the send (the lookup change alone is caught by TestS9Correlation_EachFunctionEndsSend);
// skip the header comparison alone and the stream, function, W-bit, and session-ID rows end it;
// drop the function filter and the S9F9/S9F13 rows end it.
func TestS9Correlation_NoMatch(t *testing.T) {
	withByte := func(i int, b byte) func([10]byte) [10]byte {
		return func(h [10]byte) [10]byte {
			h[i] = b
			return h
		}
	}

	tests := []struct {
		name    string
		enabled bool
		s9      func(t *testing.T, sent [10]byte) *DataMessage
	}{
		{"option off", false, func(t *testing.T, sent [10]byte) *DataMessage { return mustS9For(t, 5, sent) }},
		{"other System Bytes", true, func(t *testing.T, sent [10]byte) *DataMessage {
			return mustS9For(t, 5, withByte(9, sent[9]+1)(sent))
		}},
		{"envelope System Bytes are ours but the MHEAD's are not", true, func(t *testing.T, sent [10]byte) *DataMessage {
			mhead := withByte(9, sent[9]+1)(sent)
			return mustS9(t, 5, [4]byte(sent[6:10]), secs2.NewBinaryItem(mhead[:]))
		}},
		{"other stream", true, func(t *testing.T, sent [10]byte) *DataMessage {
			return mustS9For(t, 3, withByte(2, 0x80|2)(sent))
		}},
		{"other function", true, func(t *testing.T, sent [10]byte) *DataMessage {
			return mustS9For(t, 5, withByte(3, 3)(sent))
		}},
		{"W-bit clear", true, func(t *testing.T, sent [10]byte) *DataMessage {
			return mustS9For(t, 7, withByte(2, sent[2]&0x7F)(sent))
		}},
		{"other session ID", true, func(t *testing.T, sent [10]byte) *DataMessage {
			return mustS9For(t, 1, withByte(1, sent[1]^0x01)(sent))
		}},
		{"S9F9 with our header", true, func(t *testing.T, sent [10]byte) *DataMessage { return mustS9For(t, 9, sent) }},
		{"S9F13 with our header", true, func(t *testing.T, sent [10]byte) *DataMessage { return mustS9For(t, 13, sent) }},
		{"A[10] body", true, func(t *testing.T, sent [10]byte) *DataMessage {
			return mustS9(t, 5, s9Envelope, secs2.NewASCIIItem(string(sent[:])))
		}},
		{"B[9] body", true, func(t *testing.T, sent [10]byte) *DataMessage {
			return mustS9(t, 5, s9Envelope, secs2.NewBinaryItem(sent[:9]))
		}},
		{"B[11] body", true, func(t *testing.T, sent [10]byte) *DataMessage {
			return mustS9(t, 5, s9Envelope, secs2.NewBinaryItem(append(sent[:], 0)))
		}},
		{"empty body", true, func(t *testing.T, _ [10]byte) *DataMessage { return mustS9(t, 5, s9Envelope, nil) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, mt := newTestSendConn(t, SelectedState, WithS9Correlation(tt.enabled))
			handlers := newHandlerSpy(c)

			var s9 *DataMessage
			deliverBeforeWait(t, c, mt, func(sent [10]byte) [][]byte {
				s9 = tt.s9(t, sent)
				return [][]byte{
					ownedFrame(t, s9),
					ownedFrame(t, mustReplyDM(t, 1, 2, [4]byte(sent[6:10]))),
				}
			})

			reply, err := c.SendDataMessage(t.Context(), 1, 1, true, secs2.NewASCIIItem("PING"))

			require.NoError(t, err, "the S9 must not end the send")
			require.NotNil(t, reply)
			require.Equal(t, uint8(2), reply.Function(), "the genuine reply must end the send")

			got := handlers.next(t)
			require.Equal(t, s9.HeaderBytes(), got.HeaderBytes(), "the S9 must reach the data handlers")
			handlers.requireEmpty(t)
			requireTransactionClean(t, c)
		})
	}
}

// malformedS9Frame returns an owned S9F5 frame whose body claims a 255-byte binary item where only 10 bytes follow,
// so its lazy decode fails.
// The 10 bytes that do follow are mhead, so only the decode failure keeps it from matching.
func malformedS9Frame(t *testing.T, mhead [10]byte) []byte {
	t.Helper()

	raw := mustS9For(t, 5, mhead).ToBytes() // [4]len | [10]header | format byte | length byte | 10 bytes

	const bodyLenByteOffset = 4 + 10 + 1
	raw[bodyLenByteOffset] = 0xFF

	msg, err := DecodeHSMSMessage(raw)
	require.NoError(t, err, "HSMS framing must decode; only the body is malformed")

	dm, ok := msg.ToDataMessage()
	require.True(t, ok)
	require.Error(t, dm.DecodeErr(), "body must fail to decode")

	return raw[4:]
}

// TestS9Correlation_UndecodableBody delivers an S9F5 whose body fails to decode, then the genuine reply.
// It ends no send.
// With a decode-error handler registered it is diverted there and counted, as any undecodable message is;
// without one it reaches the data handlers.
func TestS9Correlation_UndecodableBody(t *testing.T) {
	for _, withDecodeHandler := range []bool{true, false} {
		t.Run(fmt.Sprintf("decodeErrorHandler=%v", withDecodeHandler), func(t *testing.T) {
			c, mt := newTestSendConn(t, SelectedState, WithS9Correlation(true))
			handlers := newHandlerSpy(c)

			diverted := make(chan *DataMessage, 1)
			if withDecodeHandler {
				c.AddDecodeErrorHandler(func(msg *DataMessage, _ error, _ SECS2Endpoint) { diverted <- msg })
			}

			deliverBeforeWait(t, c, mt, func(sent [10]byte) [][]byte {
				return [][]byte{
					malformedS9Frame(t, sent),
					ownedFrame(t, mustReplyDM(t, 1, 2, [4]byte(sent[6:10]))),
				}
			})

			reply, err := c.SendDataMessage(t.Context(), 1, 1, true, secs2.NewASCIIItem("PING"))

			require.NoError(t, err, "an undecodable S9 must not end the send")
			require.Equal(t, uint8(2), reply.Function())

			if withDecodeHandler {
				select {
				case got := <-diverted:
					require.Equal(t, uint8(9), got.Stream())
				case <-time.After(2 * time.Second):
					t.Fatal("the undecodable S9 never reached the decode-error handler")
				}
				require.Equal(t, uint64(1), c.metrics.BodyDecodeErrCount())
			} else {
				handlers.requireS9(t, 5)
				require.Zero(t, c.metrics.BodyDecodeErrCount())
			}

			handlers.requireEmpty(t)
			requireTransactionClean(t, c)
		})
	}
}

// TestS9Correlation_BodyLeftUndecoded delivers messages the option must not inspect:
// an S9F5 with the option off,
// and with it on an S9F9, an S9F13, and a non-S9 primary, each carrying a 10-byte binary body.
// Each reaches the data handlers with its body still undecoded.
//
// TEETH: decode dm's body at the top of correlateS9On and every row fails.
func TestS9Correlation_BodyLeftUndecoded(t *testing.T) {
	mhead := [10]byte{0, 1, 0x81, 1, 0, 0, 0, 0, 0, 1}

	tests := []struct {
		name   string
		on     bool
		stream byte
		fn     byte
		wbit   bool
	}{
		{name: "off/S9F5", on: false, stream: 9, fn: 5},
		{name: "on/S9F9", on: true, stream: 9, fn: 9},
		{name: "on/S9F13", on: true, stream: 9, fn: 13},
		{name: "on/S1F1", on: true, stream: 1, fn: 1, wbit: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newTestSendConn(t, SelectedState, WithS9Correlation(tt.on))

			undecoded := make(chan bool, 1)
			c.AddDataMessageHandler(func(msg *DataMessage, _ SECS2Endpoint) {
				undecoded <- msg.dec.item == nil && msg.dec.err == nil
			})

			msg, err := NewDataMessage(tt.stream, tt.fn, tt.wbit, 0xFFFF, s9Envelope, secs2.NewBinaryItem(mhead[:]))
			require.NoError(t, err)
			require.NoError(t, c.DeliverOwnedFrame(ownedFrame(t, msg)))

			select {
			case ok := <-undecoded:
				require.True(t, ok, "the message body must stay undecoded")
			case <-time.After(2 * time.Second):
				t.Fatal("the message never reached the data handlers")
			}
		})
	}
}

// TestS9Correlation_OptionOffWaitsOutT3 delivers a matching S9 with the option off and nothing else.
// The send times out exactly as before: ErrT3Timeout, a counted data-message error, the auto S9F9 attempted,
// and the S9 delivered to the data handlers.
func TestS9Correlation_OptionOffWaitsOutT3(t *testing.T) {
	spy := &txEventSpy{}
	c, mt := newTestSendConn(t, SelectedState, WithAutoS9F9(true), WithTransactionObserver(spy.observe))
	c.cfg.Load().timers.T3 = time.Millisecond
	handlers := newHandlerSpy(c)

	var s9f9Attempted atomic.Bool
	c.testHookAutoS9F9 = func() { s9f9Attempted.Store(true) }

	deliverBeforeWait(t, c, mt, func(sent [10]byte) [][]byte {
		return [][]byte{ownedFrame(t, mustS9For(t, 5, sent))}
	})

	reply, err := c.SendDataMessage(t.Context(), 1, 1, true, secs2.NewASCIIItem("PING"))

	require.Nil(t, reply)
	require.ErrorIs(t, err, ErrT3Timeout)
	require.True(t, s9f9Attempted.Load())

	ev, ok := spy.last()
	require.True(t, ok)
	require.Equal(t, TxT3Timeout, ev.Outcome)

	handlers.requireS9(t, 5)
	handlers.requireEmpty(t)
	require.Equal(t, uint64(1), c.metrics.DataMsgErrCount())
	require.Zero(t, c.cur.Load().replies.len())
	require.Zero(t, c.metrics.DataMsgInflightCount())
}

// TestS9Correlation_ArmThenS9 delivers a matching S9 right after the reply-wait select picked the protocol-timer arm,
// and right after it picked the teardown arm.
// Each arm's re-check returns the *S9Error instead of ErrT3Timeout or ErrConnClosed,
// and the timer arm never attempts the auto S9F9.
//
// TEETH: drop either arm's tryReply re-check and its row returns its own outcome instead.
func TestS9Correlation_ArmThenS9(t *testing.T) {
	for _, arm := range []replyWaitArm{replyWaitArmTimer, replyWaitArmTeardown} {
		t.Run(fmt.Sprintf("arm=%d", arm), func(t *testing.T) {
			spy := &txEventSpy{}
			c, _ := newTestSendConn(t, SelectedState,
				WithS9Correlation(true), WithAutoS9F9(true), WithTransactionObserver(spy.observe))
			e := c.cur.Load()
			sb := [4]byte{0, 0, 0, 60}
			primary := mustSendData(t, sb, true)

			var s9f9Attempted atomic.Bool
			c.testHookAutoS9F9 = func() { s9f9Attempted.Store(true) }

			if arm == replyWaitArmTimer {
				c.cfg.Load().timers.T3 = time.Millisecond
			} else {
				c.testHookBeforeReplyWait = func() { e.teardown(2 * time.Second) }
			}

			var picked replyWaitArm
			c.testHookWaitPicked = func(a replyWaitArm) {
				picked = a
				require.NoError(t, c.DeliverOwnedFrame(ownedFrame(t, mustS9For(t, 7, primary.HeaderBytes()))))
			}

			reply, err := c.WriteMessage(t.Context(), primary)

			require.Equal(t, arm, picked)
			require.Nil(t, reply)
			requireS9Error(t, err, 7, primary.HeaderBytes())
			require.False(t, s9f9Attempted.Load())

			ev, ok := spy.last()
			require.True(t, ok)
			require.Equal(t, TxPeerError, ev.Outcome)

			requireTransactionClean(t, c)
		})
	}
}

// TestS9Correlation_LateS9ReachesHandlers delivers a matching S9 after the send already returned,
// for each way a send can end without one.
// No send claims it, so it only reaches the data handlers.
func TestS9Correlation_LateS9ReachesHandlers(t *testing.T) {
	tests := []struct {
		name    string
		arrange func(t *testing.T, c *connection, mt *mockTransport, primary *DataMessage)
		wantErr error
	}{
		{
			name: "replied",
			arrange: func(t *testing.T, c *connection, _ *mockTransport, primary *DataMessage) {
				c.testHookBeforeReplyWait = func() {
					require.NoError(t, c.DeliverOwnedFrame(ownedFrame(t, mustReplyDM(t, 1, 2, primary.SystemBytes()))))
				}
			},
		},
		{
			name: "T3 timeout",
			arrange: func(_ *testing.T, c *connection, _ *mockTransport, _ *DataMessage) {
				c.cfg.Load().timers.T3 = time.Millisecond
			},
			wantErr: ErrT3Timeout,
		},
		{
			name:    "caller canceled",
			arrange: func(_ *testing.T, _ *connection, _ *mockTransport, _ *DataMessage) {},
			wantErr: context.Canceled,
		},
		{
			name: "write failed",
			arrange: func(_ *testing.T, _ *connection, mt *mockTransport, _ *DataMessage) {
				mt.setWriteErr(errors.New("write failed"))
			},
			wantErr: errors.New("write failed"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, mt := newTestSendConn(t, SelectedState, WithS9Correlation(true))
			handlers := newHandlerSpy(c)
			primary := mustSendData(t, [4]byte{0, 0, 0, 61}, true)
			tt.arrange(t, c, mt, primary)

			ctx := t.Context()
			if errors.Is(tt.wantErr, context.Canceled) {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				c.testHookBeforeReplyWait = cancel
			}

			_, err := c.WriteMessage(ctx, primary)
			if tt.wantErr == nil {
				require.NoError(t, err)
			} else {
				require.EqualError(t, err, tt.wantErr.Error())
			}
			require.Zero(t, c.cur.Load().replies.len(), "the send must have deregistered")

			require.NoError(t, c.DeliverOwnedFrame(ownedFrame(t, mustS9For(t, 5, primary.HeaderBytes()))))

			handlers.requireS9(t, 5)
			handlers.requireEmpty(t)
			require.Zero(t, c.metrics.DataMsgInflightCount())
		})
	}
}

// TestS9Correlation_S9AndReplyOrders delivers the S9 and the genuine reply for one send, in both orders,
// while the first one's result is still buffered on the send's channel.
// The first one ends the send, and the other reaches the data handlers:
// a reply that loses to the S9 arrives there as an orphan secondary,
// and the S9 reaches them whichever order it came in.
func TestS9Correlation_S9AndReplyOrders(t *testing.T) {
	for _, s9First := range []bool{true, false} {
		t.Run(fmt.Sprintf("s9First=%v", s9First), func(t *testing.T) {
			c, mt := newTestSendConn(t, SelectedState, WithS9Correlation(true))
			handlers := newHandlerSpy(c)

			var sent [10]byte
			deliverBeforeWait(t, c, mt, func(h [10]byte) [][]byte {
				sent = h
				s9 := ownedFrame(t, mustS9For(t, 5, h))
				reply := ownedFrame(t, mustReplyDM(t, 1, 2, [4]byte(h[6:10])))
				if s9First {
					return [][]byte{s9, reply}
				}

				return [][]byte{reply, s9}
			})

			reply, err := c.SendDataMessage(t.Context(), 1, 1, true, secs2.NewASCIIItem("PING"))

			if s9First {
				require.Nil(t, reply)
				requireS9Error(t, err, 5, sent)
				handlers.requireS9(t, 5)
				orphan := handlers.next(t)
				require.Equal(t, uint8(2), orphan.Function(), "the reply that lost to the S9 must reach the data handlers")
				require.Equal(t, [4]byte(sent[6:10]), orphan.SystemBytes())
			} else {
				require.NoError(t, err)
				require.Equal(t, uint8(2), reply.Function())
				handlers.requireS9(t, 5)
			}

			handlers.requireEmpty(t)
			requireTransactionClean(t, c)
		})
	}
}

// TestS9Correlation_ReplyThenS9Drained delivers the genuine reply, drains it as the sender would, then the S9,
// all before the sender deregisters.
// The drained buffer has room again, but the reply's claim still ends the wait:
// the S9 sends nothing and only reaches the data handlers.
func TestS9Correlation_ReplyThenS9Drained(t *testing.T) {
	c, _ := newTestSendConn(t, SelectedState, WithS9Correlation(true))
	e := c.cur.Load()
	handlers := newHandlerSpy(c)
	sb := [4]byte{0, 0, 0, 62}
	header := sentHeader(sb, 1, 1)
	ch := e.replies.registerData(header)
	defer e.replies.deregister(sb)

	require.NoError(t, c.DeliverOwnedFrame(ownedFrame(t, mustReplyDM(t, 1, 2, sb))))
	require.NoError(t, requireOneResult(t, ch).err)

	require.NoError(t, c.DeliverOwnedFrame(ownedFrame(t, mustS9For(t, 5, header))))

	requireNoResult(t, ch)
	handlers.requireS9(t, 5)
	handlers.requireEmpty(t)
}

// TestS9Correlation_ConcurrentS9AndReply races the S9 and the genuine reply for one open wait through the inbound path.
// Exactly one of them reaches the sender channel.
// The S9 always reaches the data handlers, and the reply reaches them only when the S9 won.
//
// TEETH: drop failS9's claim and some runs send both.
func TestS9Correlation_ConcurrentS9AndReply(t *testing.T) {
	c, _ := newTestSendConn(t, SelectedState, WithS9Correlation(true))
	e := c.cur.Load()
	handlers := newHandlerSpy(c)
	sb := [4]byte{0, 0, 0, 63}
	header := sentHeader(sb, 1, 1)
	ch := e.replies.registerData(header)
	defer e.replies.deregister(sb)

	s9Frame := ownedFrame(t, mustS9For(t, 5, header))
	replyFrame := ownedFrame(t, mustReplyDM(t, 1, 2, sb))

	start := make(chan struct{})
	frames := [][]byte{s9Frame, replyFrame}
	errs := make([]error, len(frames))
	var wg sync.WaitGroup
	for i, f := range frames {
		wg.Go(func() {
			<-start
			errs[i] = c.DeliverOwnedFrame(f)
		})
	}
	close(start)
	wg.Wait()

	for _, err := range errs {
		require.NoError(t, err)
	}

	res := requireOneResult(t, ch)
	requireNoResult(t, ch)

	var gotS9, gotReply int
	for range len(handlers.ch) {
		switch handlers.next(t).Stream() {
		case 9:
			gotS9++
		default:
			gotReply++
		}
	}
	require.Equal(t, 1, gotS9, "the S9 always reaches the data handlers")

	var s9Err *S9Error
	if errors.As(res.err, &s9Err) {
		require.Equal(t, 1, gotReply, "the reply lost, so it reaches the data handlers")
	} else {
		require.NoError(t, res.err)
		require.NotNil(t, res.msg, "the reply won, so it is the one sent")
		require.Zero(t, gotReply, "the reply won, so it does not reach the data handlers")
	}
}

// TestS9Correlation_OldGenerationPausedAcrossReconnect admits an S9 on generation N,
// pauses it before correlation, ends N and publishes its successor, and then releases it.
// Both generations have an open wait the S9's MHEAD identifies.
// The S9 stays bound to the generation that admitted it:
// it ends N's wait, never the successor's.
//
// TEETH: correlate against c.cur.Load() instead of the admitting epoch and the successor's wait is ended.
func TestS9Correlation_OldGenerationPausedAcrossReconnect(t *testing.T) {
	t.Parallel()

	c, genN, genSucc := newTwoGenConn(t)
	require.NoError(t, c.UpdateConfigOptions(WithS9Correlation(true)))

	sb := [4]byte{0, 0, 0, 64}
	header := sentHeader(sb, 1, 1)
	chN := genN.replies.registerData(header)
	chSucc := genSucc.replies.registerData(header)

	b := newBlocker(t)
	c.testHookBeforeFanout = b.block

	done := make(chan error, 1)
	go func() { done <- c.DeliverOwnedFrameFromGeneration(genN.id, ownedFrame(t, mustS9For(t, 5, header))) }()

	awaitSignal(t, b.entered, "testHookBeforeFanout was never entered")
	endAndSucceed(c, genN, genSucc)
	b.unblock()
	require.NoError(t, recvWithin(t, done, "DeliverOwnedFrameFromGeneration"))

	requireNoResult(t, chSucc)
	var s9Err *S9Error
	require.ErrorAs(t, requireOneResult(t, chN).err, &s9Err, "the S9 ends the wait of the generation that admitted it")

	delivered, _ := genSucc.replies.route(sb, replyResult{msg: replyDM(1, 2)}, false)
	require.True(t, delivered, "the successor's wait must still be open")
}

// TestS9Correlation_StaleGenerationAdmission delivers an S9 named for a generation that already ended.
// It is refused before correlation: neither generation's wait ends.
func TestS9Correlation_StaleGenerationAdmission(t *testing.T) {
	t.Parallel()

	c, genN, genSucc := newTwoGenConn(t)
	require.NoError(t, c.UpdateConfigOptions(WithS9Correlation(true)))

	sb := [4]byte{0, 0, 0, 65}
	header := sentHeader(sb, 1, 1)
	chN := genN.replies.registerData(header)
	chSucc := genSucc.replies.registerData(header)

	endAndSucceed(c, genN, genSucc)
	require.NoError(t, c.DeliverOwnedFrameFromGeneration(genN.id, ownedFrame(t, mustS9For(t, 5, header))))

	require.Equal(t, uint64(1), c.staleRecv.Load())
	requireNoResult(t, chN)
	requireNoResult(t, chSucc)
}

// TestS9Correlation_ControlWaiterNotMatched opens a Linktest transaction and delivers an S9
// whose MHEAD carries the Linktest's System Bytes, then the Linktest.rsp.
// An S9 never ends a control transaction, so the Linktest completes with its response.
//
// TEETH: drop failS9's isData condition and the Linktest returns the *S9Error.
func TestS9Correlation_ControlWaiterNotMatched(t *testing.T) {
	c, _ := newTestSendConn(t, SelectedState, WithS9Correlation(true))
	handlers := newHandlerSpy(c)
	// Zero System Bytes make the control waiter's key equal its all-zero header's,
	// so an all-zero MHEAD would match it under the exact comparison if the waiter's kind were not checked.
	req := NewLinktestReq([4]byte{})
	sb := req.SystemBytes()

	rsp, err := NewLinktestRsp(req)
	require.NoError(t, err)

	c.testHookBeforeReplyWait = func() {
		require.NoError(t, c.DeliverOwnedFrame(ownedFrame(t, mustS9For(t, 5, [10]byte{}))))
		require.NoError(t, c.DeliverOwnedFrame(ownedFrame(t, mustS9For(t, 5, sentHeader(sb, 1, 1)))))
		require.True(t, c.RouteReply(rsp))
	}

	got, err := c.sendWaitReply(t.Context(), req)

	require.NoError(t, err)
	require.Same(t, rsp, got)
	handlers.requireS9(t, 5)
	handlers.requireS9(t, 5)
	handlers.requireEmpty(t)
	require.Zero(t, c.cur.Load().replies.len())
}

// TestS9Correlation_WithAbortReplyError enables both options and delivers an S9 and then an SxF0 for one send.
// The S9 ends the send, and the SxF0 that follows reaches the data handlers as an orphan secondary.
func TestS9Correlation_WithAbortReplyError(t *testing.T) {
	c, mt := newTestSendConn(t, SelectedState, WithS9Correlation(true), WithAbortReplyError(true))
	handlers := newHandlerSpy(c)

	var sent [10]byte
	deliverBeforeWait(t, c, mt, func(h [10]byte) [][]byte {
		sent = h
		return [][]byte{
			ownedFrame(t, mustS9For(t, 7, h)),
			ownedFrame(t, mustAbortReply(t, 1, [4]byte(h[6:10]))),
		}
	})

	reply, err := c.SendDataMessage(t.Context(), 1, 1, true, secs2.NewASCIIItem("PING"))

	require.Nil(t, reply)
	requireS9Error(t, err, 7, sent)
	require.NotErrorIs(t, err, ErrTransactionAborted)

	handlers.requireS9(t, 7)
	f0 := handlers.next(t)
	require.Zero(t, f0.Function(), "the SxF0 that lost to the S9 must reach the data handlers")
	handlers.requireEmpty(t)
	require.Zero(t, c.metrics.ReplyMismatchCount())
	requireTransactionClean(t, c)
}

// TestS9Correlation_StrictReplyMatching delivers three messages for one send:
// a reply with the wrong stream, then the S9, then the conforming reply,
// with strict reply matching off and on.
// Off, the wrong-stream reply is accepted and ends the send, so the S9 finds the wait over.
// On, the wrong-stream reply is a mismatch that leaves the wait open, so the S9 ends the send.
// Either way the mismatch is counted once, and every message that did not end the send reaches the data handlers.
func TestS9Correlation_StrictReplyMatching(t *testing.T) {
	for _, strict := range []bool{false, true} {
		t.Run(fmt.Sprintf("strict=%v", strict), func(t *testing.T) {
			c, mt := newTestSendConn(t, SelectedState, WithS9Correlation(true), WithStrictReplyMatching(strict))
			handlers := newHandlerSpy(c)

			var sent [10]byte
			deliverBeforeWait(t, c, mt, func(h [10]byte) [][]byte {
				sent = h
				sb := [4]byte(h[6:10])

				return [][]byte{
					ownedFrame(t, mustReplyDM(t, 2, 2, sb)),
					ownedFrame(t, mustS9For(t, 5, h)),
					ownedFrame(t, mustReplyDM(t, 1, 2, sb)),
				}
			})

			reply, err := c.SendDataMessage(t.Context(), 1, 1, true, secs2.NewASCIIItem("PING"))

			if strict {
				require.Nil(t, reply)
				requireS9Error(t, err, 5, sent)
				require.Equal(t, uint8(2), handlers.next(t).Stream(), "the mismatched reply falls through")
				handlers.requireS9(t, 5)
				require.Equal(t, uint8(1), handlers.next(t).Stream(), "the conforming reply lost to the S9")
			} else {
				require.NoError(t, err)
				require.Equal(t, uint8(2), reply.Stream(), "the default posture accepts the wrong-stream reply")
				handlers.requireS9(t, 5)
			}

			handlers.requireEmpty(t)
			require.Equal(t, uint64(1), c.metrics.ReplyMismatchCount())
			requireTransactionClean(t, c)
		})
	}
}

// TestTxPeerError_OutcomeAndString checks TxPeerError's position after the earlier outcomes, its String form,
// and that the classifier maps an *S9Error, plain or wrapped, to it.
func TestTxPeerError_OutcomeAndString(t *testing.T) {
	require.Equal(t, TxAborted+1, TxPeerError, "TxPeerError is appended, so the earlier outcomes keep their values")
	require.Equal(t, "PeerError", TxPeerError.String())
	require.Equal(t, TxPeerError, classifyTxOutcome(true, &S9Error{Function: 5}))
	require.Equal(t, TxPeerError, classifyTxOutcome(true, fmt.Errorf("wrapped: %w", &S9Error{Function: 5})))
}

// secs1MHEADMock is a mockTransport that reports, through UsesSECS1MHEAD, whether its peer sends SECS-I block headers.
type secs1MHEADMock struct {
	mockTransport

	secs1 bool
}

func (m *secs1MHEADMock) UsesSECS1MHEAD() bool { return m.secs1 }

// TestNewConnection_MHEADPolicy checks the comparison NewConnection chooses from its transport:
// block for a transport reporting SECS-I block headers, exact for one reporting false or not implementing the method.
func TestNewConnection_MHEADPolicy(t *testing.T) {
	tests := []struct {
		name string
		tr   transport
		want mheadPolicy
	}{
		{"no UsesSECS1MHEAD", &mockTransport{}, mheadExact},
		{"UsesSECS1MHEAD false", &secs1MHEADMock{secs1: false}, mheadExact},
		{"UsesSECS1MHEAD true", &secs1MHEADMock{secs1: true}, mheadBlock},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn, err := NewConnection(DefaultConnectionConfig(), tt.tr)
			require.NoError(t, err)

			c, ok := conn.(*connection)
			require.True(t, ok)
			require.Equal(t, tt.want, c.mhead)
		})
	}
}

// TestS9Correlation_BlockPolicyEndsSend runs a send on a connection using the SECS-I comparison
// and answers it with an S9 whose MHEAD is the block header the peer received: R-bit, E-bit, and block number set.
// It ends the send; the same MHEAD with another device ID does not.
func TestS9Correlation_BlockPolicyEndsSend(t *testing.T) {
	for _, otherDevice := range []bool{false, true} {
		t.Run(fmt.Sprintf("otherDevice=%v", otherDevice), func(t *testing.T) {
			c, mt := newTestSendConn(t, SelectedState, WithS9Correlation(true))
			c.mhead = mheadBlock
			handlers := newHandlerSpy(c)

			var sent [10]byte
			deliverBeforeWait(t, c, mt, func(h [10]byte) [][]byte {
				sent = h
				mhead := h
				mhead[0] |= 0x80
				mhead[4], mhead[5] = 0x80, 0x01
				if otherDevice {
					mhead[1] ^= 0x01
				}

				return [][]byte{
					ownedFrame(t, mustS9For(t, 1, mhead)),
					ownedFrame(t, mustReplyDM(t, 1, 2, [4]byte(h[6:10]))),
				}
			})

			reply, err := c.SendDataMessage(t.Context(), 1, 1, true, secs2.NewASCIIItem("PING"))

			handlers.requireS9(t, 1)
			if otherDevice {
				require.NoError(t, err)
				require.Equal(t, uint8(2), reply.Function())
			} else {
				require.Nil(t, reply)

				var s9Err *S9Error
				require.ErrorAs(t, err, &s9Err)
				require.NotEqual(t, sent, s9Err.MHEAD, "the MHEAD is reported as the peer sent it")
				require.Equal(t, uint8(2), handlers.next(t).Function(), "the reply lost to the S9")
			}

			handlers.requireEmpty(t)
			requireTransactionClean(t, c)
		})
	}
}
