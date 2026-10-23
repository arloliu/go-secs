package hsmsss

// abort_reply_error_test.go — hsms.WithAbortReplyError over a real hsmsss connection built by New, against a scripted raw peer.
// The peer answers the connection's W-bit primary with an SxF0,
// and then writes the same SxF0 again after the send returned, which no transaction claims.

import (
	"fmt"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// TestAbortReplyError_PeerAbortsSend answers a SendDataMessage or SendSECS2Message with an S1F0,
// with the option on and off.
// On, the send returns the S1F0 with ErrTransactionAborted; off, it returns the S1F0 with a nil error.
// Either way, the same S1F0 written again after the send returned reaches the data handlers as a plain secondary.
func TestAbortReplyError_PeerAbortsSend(t *testing.T) {
	t.Parallel()

	sends := []struct {
		name string
		send func(t *testing.T, conn Connection) (*hsms.DataMessage, error)
	}{
		{
			name: "SendDataMessage",
			send: func(t *testing.T, conn Connection) (*hsms.DataMessage, error) {
				return conn.SendDataMessage(t.Context(), 1, 1, true, secs2.A("PING"))
			},
		},
		{
			name: "SendSECS2Message",
			send: func(t *testing.T, conn Connection) (*hsms.DataMessage, error) {
				return conn.SendSECS2Message(t.Context(), secs2.NewMessage(1, 1, true, secs2.A("PING")))
			},
		},
	}

	for _, sf := range sends {
		for _, enabled := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/enabled=%v", sf.name, enabled), func(t *testing.T) {
				t.Parallel()

				handled := make(chan *hsms.DataMessage, 1)
				conn, peer := newAbortPassive(t,
					func(msg *hsms.DataMessage, _ hsms.SECS2Endpoint) { handled <- msg },
					WithConnectionOption(hsms.WithAbortReplyError(enabled)),
				)

				type result struct {
					msg *hsms.DataMessage
					err error
				}
				out := make(chan result, 1)
				go func() {
					reply, err := sf.send(t, conn)
					out <- result{msg: reply, err: err}
				}()

				frame, err := peerReadFrame(peer, 5*time.Second)
				require.NoError(t, err, "the peer must receive the primary")
				require.GreaterOrEqual(t, len(frame), 10)

				var sb [4]byte
				copy(sb[:], frame[6:10])

				_, err = peer.Write(dataFrame(testDeviceID, 1, 0, sb, nil))
				require.NoError(t, err)

				var r result
				select {
				case r = <-out:
				case <-time.After(5 * time.Second):
					t.Fatal("the send never returned")
				}

				require.NotNil(t, r.msg, "the S1F0 must be returned either way")
				require.Zero(t, r.msg.Function())
				require.Equal(t, sb, r.msg.SystemBytes())

				if enabled {
					require.ErrorIs(t, r.err, hsms.ErrTransactionAborted)
				} else {
					require.NoError(t, r.err)
				}

				require.Empty(t, handled, "the claimed S1F0 must not also reach the data handlers")

				_, err = peer.Write(dataFrame(testDeviceID, 1, 0, sb, nil))
				require.NoError(t, err)

				select {
				case got := <-handled:
					require.Zero(t, got.Function())
					require.Equal(t, sb, got.SystemBytes())
				case <-time.After(5 * time.Second):
					t.Fatal("the orphan S1F0 never reached the data handlers")
				}

				m := conn.Metrics()
				require.Equal(t, uint64(0), m.DataMsgErrCount(), "an SxF0 reply is never a data-message error")
				require.Equal(t, int64(0), m.DataMsgInflightCount())
			})
		}
	}
}
