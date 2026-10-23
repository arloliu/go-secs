package secs1

// abort_reply_error_test.go — hsms.WithAbortReplyError over a real secs1 pair built by New.
// The active host sends a W-bit primary with SendDataMessage or SendSECS2Message,
// and the passive equipment answers it with hsms.ReplyAbort.

import (
	"fmt"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// TestAbortReplyError_EquipmentAbortsSend aborts the host's S6F11 with an S6F0, with the host's option on and off,
// sent with SendDataMessage or SendSECS2Message.
// On, the host's send returns the S6F0 with ErrTransactionAborted; off, it returns the S6F0 with a nil error.
func TestAbortReplyError_EquipmentAbortsSend(t *testing.T) {
	t.Parallel()

	sends := []struct {
		name string
		send func(t *testing.T, conn hsms.Connection) (*hsms.DataMessage, error)
	}{
		{
			name: "SendDataMessage",
			send: func(t *testing.T, conn hsms.Connection) (*hsms.DataMessage, error) {
				return conn.SendDataMessage(t.Context(), 6, 11, true, secs2.A("EVENT"))
			},
		},
		{
			name: "SendSECS2Message",
			send: func(t *testing.T, conn hsms.Connection) (*hsms.DataMessage, error) {
				return conn.SendSECS2Message(t.Context(), secs2.NewMessage(6, 11, true, secs2.A("EVENT")))
			},
		},
	}

	for _, sf := range sends {
		for _, enabled := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/enabled=%v", sf.name, enabled), func(t *testing.T) {
				t.Parallel()

				primaries := make(chan *hsms.DataMessage, 1)
				abortErr := make(chan error, 1)
				_, active := openAbortPair(t,
					func(msg *hsms.DataMessage, ep hsms.SECS2Endpoint) {
						abortErr <- hsms.ReplyAbort(t.Context(), ep, msg)
						primaries <- msg
					},
					nil,
					WithConnectionOption(hsms.WithAbortReplyError(enabled)),
				)

				out := make(chan abortReply, 1)
				go func() {
					reply, err := sf.send(t, active.conn)
					out <- abortReply{msg: reply, err: err}
				}()

				primary := receiveAbortDeliveryMsg(t, primaries)
				require.NoError(t, <-abortErr)

				var r abortReply
				select {
				case r = <-out:
				case <-time.After(10 * time.Second):
					t.Fatal("the host's send never returned")
				}

				require.NotNil(t, r.msg, "the S6F0 must be returned either way")
				require.Equal(t, byte(6), r.msg.Stream())
				require.Zero(t, r.msg.Function())
				require.Equal(t, primary.SystemBytes(), r.msg.SystemBytes())

				if enabled {
					require.ErrorIs(t, r.err, hsms.ErrTransactionAborted)
				} else {
					require.NoError(t, r.err)
				}

				m := active.conn.Metrics()
				require.Equal(t, uint64(0), m.DataMsgErrCount(), "an SxF0 reply is never a data-message error")
				require.Equal(t, int64(0), m.DataMsgInflightCount())
			})
		}
	}
}

// receiveAbortDeliveryMsg waits for the passive's handler to hand back the primary it aborted.
func receiveAbortDeliveryMsg(t *testing.T, ch <-chan *hsms.DataMessage) *hsms.DataMessage {
	t.Helper()

	select {
	case msg := <-ch:
		return msg
	case <-time.After(10 * time.Second):
		t.Fatal("the primary never reached the passive's data handler")
		return nil
	}
}
