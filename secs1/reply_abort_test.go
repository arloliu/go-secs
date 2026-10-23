package secs1

// reply_abort_test.go — hsms.ReplyAbort over a real secs1 pair built by New.
// The active host sends a W-bit primary, and the passive equipment aborts it,
// through the endpoint its data handler receives or through the connection New returned.
// The host's SendDataMessage correlates the abort by System Bytes and returns it as the reply.

import (
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// abortDelivery is what the passive's data handler hands back: the primary and the endpoint it was delivered with.
type abortDelivery struct {
	msg *hsms.DataMessage
	ep  hsms.SECS2Endpoint
}

// abortReply is the outcome of the host's synchronous send.
type abortReply struct {
	msg *hsms.DataMessage
	err error
}

// openAbortPair opens a passive equipment with handler and an active host, both Selected, and closes them on cleanup.
// hostHandler, when not nil, receives the host's uncorrelated data messages.
func openAbortPair(t *testing.T, handler, hostHandler hsms.DataMessageHandler) (passive, active *secs1E) {
	t.Helper()

	port := freePort(t)

	passive = newSECS1Passive(t, port, handler)
	active = newSECS1Active(t, port)

	if hostHandler != nil {
		active.conn.AddDataMessageHandler(hostHandler)
	}

	t.Cleanup(func() { _ = active.conn.Close() })
	t.Cleanup(func() { _ = passive.conn.Close() })

	require.NoError(t, passive.conn.Open(t.Context(), hsms.OpenBackground))
	require.NoError(t, active.conn.Open(t.Context(), hsms.OpenBackground))
	waitSECS1Selected(t, passive)
	waitSECS1Selected(t, active)

	return passive, active
}

// sendS6F11 sends a W-bit S6F11 from the host on its own goroutine and returns the channel its outcome arrives on.
func sendS6F11(t *testing.T, active *secs1E) <-chan abortReply {
	t.Helper()

	out := make(chan abortReply, 1)
	go func() {
		reply, err := active.conn.SendDataMessage(t.Context(), 6, 11, true, secs2.A("EVENT"))
		out <- abortReply{msg: reply, err: err}
	}()

	return out
}

// receiveAbortDelivery waits for the passive's handler to hand back a delivery.
func receiveAbortDelivery(t *testing.T, ch <-chan abortDelivery) abortDelivery {
	t.Helper()

	select {
	case d := <-ch:
		return d
	case <-time.After(10 * time.Second):
		t.Fatal("the primary never reached the passive's data handler")
		return abortDelivery{}
	}
}

// requireAbortReply waits for the host's send and checks its reply is the S6F0 answering primary.
func requireAbortReply(t *testing.T, out <-chan abortReply, primary *hsms.DataMessage) {
	t.Helper()

	var r abortReply
	select {
	case r = <-out:
	case <-time.After(10 * time.Second):
		t.Fatal("the host's send never returned")
	}

	require.NoError(t, r.err)
	require.NotNil(t, r.msg)
	require.Equal(t, byte(6), r.msg.Stream(), "an abort keeps the primary's stream")
	require.Zero(t, r.msg.Function(), "an abort is function 0")
	require.False(t, r.msg.WaitBit(), "an abort carries no W-bit")
	require.Zero(t, r.msg.BodyLen(), "an abort is header only")
	require.Equal(t, primary.SystemBytes(), r.msg.SystemBytes(), "an abort reuses the primary's System Bytes")
}

// TestReplyAbort_HostReceivesSxF0 aborts the host's W-bit primary through both targets.
func TestReplyAbort_HostReceivesSxF0(t *testing.T) {
	t.Parallel()

	t.Run("handler endpoint", func(t *testing.T) {
		t.Parallel()

		ch := make(chan abortDelivery, 1)
		errCh := make(chan error, 1)
		_, active := openAbortPair(t, func(msg *hsms.DataMessage, ep hsms.SECS2Endpoint) {
			errCh <- hsms.ReplyAbort(t.Context(), ep, msg)
			ch <- abortDelivery{msg: msg, ep: ep}
		}, nil)

		out := sendS6F11(t, active)
		d := receiveAbortDelivery(t, ch)

		require.NoError(t, <-errCh)
		requireAbortReply(t, out, d.msg)
	})

	t.Run("connection", func(t *testing.T) {
		t.Parallel()

		ch := make(chan abortDelivery, 1)
		passive, active := openAbortPair(t, func(msg *hsms.DataMessage, ep hsms.SECS2Endpoint) {
			ch <- abortDelivery{msg: msg, ep: ep}
		}, nil)

		out := sendS6F11(t, active)
		d := receiveAbortDelivery(t, ch)

		require.NoError(t, hsms.ReplyAbort(t.Context(), passive.conn, d.msg))
		requireAbortReply(t, out, d.msg)
	})
}

// TestReplyAbort_NoReplyExpectedSendsNothing refuses a primary without the W-bit through both targets.
// The W-bit primary that follows is aborted, and the host's handler, which an uncorrelated abort would reach first,
// receives nothing.
func TestReplyAbort_NoReplyExpectedSendsNothing(t *testing.T) {
	t.Parallel()

	ch := make(chan abortDelivery, 1)
	hostGot := make(chan *hsms.DataMessage, 4)
	passive, active := openAbortPair(t,
		func(msg *hsms.DataMessage, ep hsms.SECS2Endpoint) { ch <- abortDelivery{msg: msg, ep: ep} },
		func(msg *hsms.DataMessage, _ hsms.SECS2Endpoint) { hostGot <- msg },
	)

	require.NoError(t, active.conn.SendDataMessageAsync(t.Context(), 6, 11, false, secs2.A("EVENT")))
	d := receiveAbortDelivery(t, ch)

	require.ErrorIs(t, hsms.ReplyAbort(t.Context(), passive.conn, d.msg), hsms.ErrNoReplyExpected)
	require.ErrorIs(t, hsms.ReplyAbort(t.Context(), d.ep, d.msg), hsms.ErrNoReplyExpected)

	out := sendS6F11(t, active)
	d = receiveAbortDelivery(t, ch)

	require.NoError(t, hsms.ReplyAbort(t.Context(), passive.conn, d.msg))
	requireAbortReply(t, out, d.msg)
	require.Empty(t, hostGot, "nothing may reach the host for the primary without the W-bit")
}

// TestReplyAbort_StaleGenerationRefused returns ErrConnClosed for a primary whose generation ended when the connection closed,
// through both targets.
func TestReplyAbort_StaleGenerationRefused(t *testing.T) {
	t.Parallel()

	for _, via := range []string{"connection", "handler endpoint"} {
		t.Run(via, func(t *testing.T) {
			t.Parallel()

			ch := make(chan abortDelivery, 1)
			passive, active := openAbortPair(t, func(msg *hsms.DataMessage, ep hsms.SECS2Endpoint) {
				ch <- abortDelivery{msg: msg, ep: ep}
			}, nil)

			out := sendS6F11(t, active)
			d := receiveAbortDelivery(t, ch)

			require.NoError(t, passive.conn.Close())

			target := hsms.SECS2Endpoint(passive.conn)
			if via == "handler endpoint" {
				target = d.ep
			}

			require.ErrorIs(t, hsms.ReplyAbort(t.Context(), target, d.msg), hsms.ErrConnClosed)

			// The host's send fails once the link is gone; wait for it so its goroutine ends with the test.
			select {
			case r := <-out:
				require.Error(t, r.err)
			case <-time.After(10 * time.Second):
				t.Fatal("the host's send never returned after the link closed")
			}
		})
	}
}
