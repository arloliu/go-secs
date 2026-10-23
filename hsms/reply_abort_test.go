package hsms

import (
	"errors"
	"testing"

	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// reply_abort_test.go covers ReplyAbort over the in-package two-generation harness:
// the SxF0 it enqueues, its generation binding, and the calls it refuses without sending.

// foreignEndpoint is a SECS2Endpoint that go-secs did not build, so it has no ReplyAbort.
type foreignEndpoint struct{ SECS2Endpoint }

// mustAbortPrimary builds an S6F11 primary with the given W-bit and System Bytes.
func mustAbortPrimary(t *testing.T, sysBytes [4]byte, waitBit bool) *DataMessage {
	t.Helper()

	msg, err := NewDataMessage(6, 11, waitBit, 0xFFFF, sysBytes, secs2.NewASCIIItem("EVENT"))
	require.NoError(t, err)

	return msg
}

// deliverPrimary delivers msg through gen's fan-out and returns the message and endpoint the handler received.
// A gen of 0 takes the plain DeliverOwnedFrame ingress SECS-I uses.
func deliverPrimary(t *testing.T, c *connection, gen uint64, msg *DataMessage) (*DataMessage, SECS2Endpoint) {
	t.Helper()

	type delivery struct {
		msg *DataMessage
		ep  SECS2Endpoint
	}

	ch := make(chan delivery, 1)
	c.AddDataMessageHandler(func(m *DataMessage, ep SECS2Endpoint) { ch <- delivery{msg: m, ep: ep} })

	if gen == 0 {
		require.NoError(t, c.DeliverOwnedFrame(ownedFrame(t, msg)))
	} else {
		require.NoError(t, c.DeliverOwnedFrameFromGeneration(gen, ownedFrame(t, msg)))
	}

	select {
	case d := <-ch:
		return d.msg, d.ep
	default:
		t.Fatal("the primary must have reached the data handler")
		return nil, nil
	}
}

// requireAbortQueued takes the one message queued on e and checks it is the SxF0 answering primary.
func requireAbortQueued(t *testing.T, e *epoch, primary *DataMessage) {
	t.Helper()

	require.Len(t, e.sendCh, 1, "exactly one message must be queued")
	req := <-e.sendCh
	dm, ok := req.msg.(*DataMessage)
	require.True(t, ok, "the queued message must be a data message")

	require.Equal(t, primary.Stream(), dm.Stream(), "the abort keeps the primary's stream")
	require.Zero(t, dm.Function(), "an abort is function 0")
	require.False(t, dm.WaitBit(), "an abort carries no W-bit")
	require.Equal(t, primary.SystemBytes(), dm.SystemBytes(), "an abort reuses the primary's System Bytes")
	require.Zero(t, dm.BodyLen(), "an abort is header only")
}

// TestReplyAbort_LiveGeneration sends the abort through the connection and through the handler's endpoint,
// on both ingress paths, while the admitting generation is still live.
func TestReplyAbort_LiveGeneration(t *testing.T) {
	t.Parallel()

	for _, ingress := range []string{"generation", "plain"} {
		for _, via := range []string{"connection", "endpoint"} {
			t.Run(ingress+"/"+via, func(t *testing.T) {
				t.Parallel()

				c, genN, _ := newTwoGenConn(t)

				gen := genN.id
				if ingress == "plain" {
					gen = 0
				}

				primary, ep := deliverPrimary(t, c, gen, mustAbortPrimary(t, [4]byte{0, 0, 1, 1}, true))

				target := SECS2Endpoint(c)
				if via == "endpoint" {
					target = ep
				}

				require.NoError(t, ReplyAbort(t.Context(), target, primary))
				requireAbortQueued(t, genN, primary)
				require.Zero(t, c.staleSend.Load())
			})
		}
	}
}

// TestReplyAbort_StaleGeneration refuses the abort with ErrConnClosed once the admitting generation has ended,
// so it never lands on the successor's queue.
func TestReplyAbort_StaleGeneration(t *testing.T) {
	t.Parallel()

	for _, ingress := range []string{"generation", "plain"} {
		for _, via := range []string{"connection", "endpoint"} {
			t.Run(ingress+"/"+via, func(t *testing.T) {
				t.Parallel()

				c, genN, genSucc := newTwoGenConn(t)

				gen := genN.id
				if ingress == "plain" {
					gen = 0
				}

				primary, ep := deliverPrimary(t, c, gen, mustAbortPrimary(t, [4]byte{0, 0, 1, 2}, true))
				endAndSucceed(c, genN, genSucc)

				target := SECS2Endpoint(c)
				if via == "endpoint" {
					target = ep
				}

				require.ErrorIs(t, ReplyAbort(t.Context(), target, primary), ErrConnClosed)
				require.Empty(t, genSucc.sendCh, "the stale abort must never land on the successor's queue")
				require.Equal(t, uint64(1), c.staleSend.Load(), "the refused abort must be counted")
			})
		}
	}
}

// TestReplyAbort_NoReplyExpected refuses a primary without the W-bit before anything else,
// even one whose generation has ended, and sends nothing.
func TestReplyAbort_NoReplyExpected(t *testing.T) {
	t.Parallel()

	for _, stale := range []bool{false, true} {
		name := "live"
		if stale {
			name = "stale"
		}

		t.Run(name, func(t *testing.T) {
			t.Parallel()

			c, genN, genSucc := newTwoGenConn(t)
			primary, ep := deliverPrimary(t, c, genN.id, mustAbortPrimary(t, [4]byte{0, 0, 1, 3}, false))

			if stale {
				endAndSucceed(c, genN, genSucc)
			}

			require.ErrorIs(t, ReplyAbort(t.Context(), c, primary), ErrNoReplyExpected)
			require.ErrorIs(t, ReplyAbort(t.Context(), ep, primary), ErrNoReplyExpected)
			require.Empty(t, genN.sendCh)
			require.Empty(t, genSucc.sendCh)
			require.Zero(t, c.staleSend.Load(), "a refused W=0 primary never reaches the generation check")
		})
	}
}

// TestReplyAbort_NilPrimary returns ErrNilMessage through the connection and the package function.
func TestReplyAbort_NilPrimary(t *testing.T) {
	t.Parallel()

	c, genN, _ := newTwoGenConn(t)

	require.ErrorIs(t, c.ReplyAbort(t.Context(), nil), ErrNilMessage)
	require.ErrorIs(t, ReplyAbort(t.Context(), c, nil), ErrNilMessage)
	require.Empty(t, genN.sendCh)
}

// TestReplyAbort_UnsupportedEndpoint returns ErrAbortUnsupported for an endpoint go-secs did not build, and for nil.
func TestReplyAbort_UnsupportedEndpoint(t *testing.T) {
	t.Parallel()

	primary := mustAbortPrimary(t, [4]byte{0, 0, 1, 4}, true)

	require.ErrorIs(t, ReplyAbort(t.Context(), foreignEndpoint{}, primary), ErrAbortUnsupported)
	require.ErrorIs(t, ReplyAbort(t.Context(), nil, primary), ErrAbortUnsupported)
}

// TestErrAbortUnsupported_IsErrUnsupported checks that ErrAbortUnsupported matches errors.ErrUnsupported,
// both as the sentinel and as ReplyAbort returns it.
func TestErrAbortUnsupported_IsErrUnsupported(t *testing.T) {
	t.Parallel()

	require.ErrorIs(t, ErrAbortUnsupported, errors.ErrUnsupported)

	primary := mustAbortPrimary(t, [4]byte{0, 0, 1, 5}, true)
	require.ErrorIs(t, ReplyAbort(t.Context(), foreignEndpoint{}, primary), errors.ErrUnsupported)
}
