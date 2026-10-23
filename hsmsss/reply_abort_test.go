package hsmsss

// reply_abort_test.go — hsms.ReplyAbort over a real hsmsss connection built by New, against a scripted raw peer.
// The peer reads the abort off the wire, so the test checks the SxF0 frame itself,
// through both the constructor-returned connection and the endpoint a data handler receives.

import (
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// abortDelivery is what the test's data handler hands back: the primary and the endpoint it was delivered with.
type abortDelivery struct {
	msg *hsms.DataMessage
	ep  hsms.SECS2Endpoint
}

// newAbortPassive builds a passive connection through New, registers handler, opens it,
// and returns it together with a scripted peer that has already selected it.
func newAbortPassive(t *testing.T, handler hsms.DataMessageHandler) (Connection, net.Conn) {
	t.Helper()

	port := freeLoopbackPort(t)

	cfg, err := NewConfig("127.0.0.1", port,
		WithPassive(),
		WithConnectionOption(hsms.WithSessionID(testDeviceID)),
		WithConnectionOption(hsms.WithLinktestInterval(0)),
	)
	require.NoError(t, err)

	conn, err := New(cfg)
	require.NoError(t, err)
	conn.AddDataMessageHandler(handler)
	t.Cleanup(func() { _ = conn.Close() })

	states := watchStates(conn)
	require.NoError(t, conn.Open(t.Context(), hsms.OpenBackground))

	peer := dialPassive(t, port)
	t.Cleanup(func() { _ = peer.Close() })

	_, err = peer.Write(selectReqFrame([4]byte{0, 0, 0, 1}))
	require.NoError(t, err)
	expectSelectRsp(t, peer)

	states.await(t, hsms.SelectedState)

	return conn, peer
}

// writeS6F11 writes an S6F11 primary from the peer with the given System Bytes and W-bit.
func writeS6F11(t *testing.T, peer net.Conn, sb [4]byte, waitBit bool) {
	t.Helper()

	frame := dataFrame(testDeviceID, 6, 11, sb, secs2.A("EVENT").ToBytes())
	if waitBit {
		frame[4+2] |= 0x80 // header byte 2, after the 4-byte length prefix
	}

	_, err := peer.Write(frame)
	require.NoError(t, err)
}

// requireAbortFrame reads the next frame from peer and checks it is the S6F0 answering the primary with System Bytes sb.
func requireAbortFrame(t *testing.T, peer net.Conn, sb [4]byte) {
	t.Helper()

	frame, err := peerReadFrame(peer, 5*time.Second)
	require.NoError(t, err, "the peer must receive the abort")
	require.Len(t, frame, 10, "an abort is a header-only data message")

	require.Equal(t, testDeviceID, binary.BigEndian.Uint16(frame[0:2]), "session ID")
	require.Zero(t, frame[2]&0x80, "an abort carries no W-bit")
	require.Equal(t, byte(6), frame[2]&0x7F, "an abort keeps the primary's stream")
	require.Zero(t, frame[3], "an abort is function 0")
	require.Zero(t, frame[4], "PType 0")
	require.Zero(t, frame[5], "SType 0, a data message")
	require.Equal(t, sb[:], frame[6:10], "an abort reuses the primary's System Bytes")
}

// receiveDelivery waits for the handler to hand back a delivery.
func receiveDelivery(t *testing.T, ch <-chan abortDelivery) abortDelivery {
	t.Helper()

	select {
	case d := <-ch:
		return d
	case <-time.After(5 * time.Second):
		t.Fatal("the primary never reached the data handler")
		return abortDelivery{}
	}
}

// TestReplyAbort_PeerReceivesSxF0 aborts a W-bit primary from inside the handler through its endpoint,
// and from the test through the connection New returned.
func TestReplyAbort_PeerReceivesSxF0(t *testing.T) {
	t.Parallel()

	t.Run("handler endpoint", func(t *testing.T) {
		t.Parallel()

		errCh := make(chan error, 1)
		_, peer := newAbortPassive(t, func(msg *hsms.DataMessage, ep hsms.SECS2Endpoint) {
			errCh <- hsms.ReplyAbort(t.Context(), ep, msg)
		})

		sb := [4]byte{0, 0, 2, 1}
		writeS6F11(t, peer, sb, true)

		requireAbortFrame(t, peer, sb)
		require.NoError(t, <-errCh)
	})

	t.Run("connection", func(t *testing.T) {
		t.Parallel()

		ch := make(chan abortDelivery, 1)
		conn, peer := newAbortPassive(t, func(msg *hsms.DataMessage, ep hsms.SECS2Endpoint) {
			ch <- abortDelivery{msg: msg, ep: ep}
		})

		sb := [4]byte{0, 0, 2, 2}
		writeS6F11(t, peer, sb, true)
		d := receiveDelivery(t, ch)

		require.NoError(t, hsms.ReplyAbort(t.Context(), conn, d.msg))
		requireAbortFrame(t, peer, sb)
	})
}

// TestReplyAbort_NoReplyExpectedSendsNothing refuses a primary without the W-bit through both targets.
// The W-bit primary that follows is aborted, and its abort is the first frame the peer reads,
// so nothing went out for the first one.
func TestReplyAbort_NoReplyExpectedSendsNothing(t *testing.T) {
	t.Parallel()

	ch := make(chan abortDelivery, 1)
	conn, peer := newAbortPassive(t, func(msg *hsms.DataMessage, ep hsms.SECS2Endpoint) {
		ch <- abortDelivery{msg: msg, ep: ep}
	})

	writeS6F11(t, peer, [4]byte{0, 0, 2, 3}, false)
	d := receiveDelivery(t, ch)

	require.ErrorIs(t, hsms.ReplyAbort(t.Context(), conn, d.msg), hsms.ErrNoReplyExpected)
	require.ErrorIs(t, hsms.ReplyAbort(t.Context(), d.ep, d.msg), hsms.ErrNoReplyExpected)

	sb := [4]byte{0, 0, 2, 4}
	writeS6F11(t, peer, sb, true)
	d = receiveDelivery(t, ch)

	require.NoError(t, hsms.ReplyAbort(t.Context(), conn, d.msg))
	requireAbortFrame(t, peer, sb)
}

// TestReplyAbort_StaleGenerationRefused returns ErrConnClosed for a primary whose generation ended when the connection closed,
// through both targets.
func TestReplyAbort_StaleGenerationRefused(t *testing.T) {
	t.Parallel()

	for _, via := range []string{"connection", "handler endpoint"} {
		t.Run(via, func(t *testing.T) {
			t.Parallel()

			ch := make(chan abortDelivery, 1)
			conn, peer := newAbortPassive(t, func(msg *hsms.DataMessage, ep hsms.SECS2Endpoint) {
				ch <- abortDelivery{msg: msg, ep: ep}
			})

			writeS6F11(t, peer, [4]byte{0, 0, 2, 5}, true)
			d := receiveDelivery(t, ch)

			require.NoError(t, conn.Close())

			target := hsms.SECS2Endpoint(conn)
			if via == "handler endpoint" {
				target = d.ep
			}

			require.ErrorIs(t, hsms.ReplyAbort(t.Context(), target, d.msg), hsms.ErrConnClosed)
		})
	}
}
