package hsmsss

import (
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// TestForwardDataMessage_ReplyRoutesToHandler is the end-to-end proof of the forward/relay semantic
// on a live active+passive pair. The active side forwards a W-bit primary carrying CALLER-CHOSEN
// System Bytes via ForwardDataMessage (which registers no reply expectation); the passive peer
// echoes it as a secondary reusing those System Bytes (E37 §8.2.6.9); and the reply is delivered to
// the active side's DataMessageHandler — NOT consumed by the send call. This proves both halves of
// the contract at once:
//
//  1. System Bytes are preserved verbatim on the wire — the echo carries exactly what we sent, so
//     the caller (a router/proxy) can correlate the reply to an upstream request by System Bytes.
//  2. Because the forward registered no reply expectation, the secondary misses the reply registry
//     (RouteReply) and falls through to RouteData → the session handlers.
//
// TEETH: routing the forward through the reply-registering WriteMessage path would let the secondary
// be consumed at the registry instead of reaching the handler, so the capture below would never fire
// and the require.Eventually would time out.
func TestForwardDataMessage_ReplyRoutesToHandler(t *testing.T) {
	t.Parallel()

	port := freeLoopbackPort(t)
	ctx := t.Context()

	// Caller-owned System Bytes: a large fixed value disjoint from the library's monotonic generator
	// (which starts at 1), standing in for an upstream-assigned correlation/ticket ID.
	sysBytes := [4]byte{0xDE, 0xAD, 0xBE, 0xEF}

	type captured struct {
		sysBytes [4]byte
		function byte
		body     string
	}
	replies := make(chan captured, 1)

	// Active side (local proxy) handler: capture inbound SECONDARIES (the forwarded message's reply).
	capture := func(msg *hsms.DataMessage, _ hsms.SECS2Endpoint) {
		if msg.Function()%2 != 0 {
			return // ignore primaries; we only assert on the reply
		}
		item, err := msg.Item()
		if err != nil {
			return
		}
		s, _ := item.ToASCII()
		select {
		case replies <- captured{sysBytes: msg.SystemBytes(), function: msg.Function(), body: s}:
		default:
		}
	}

	// Passive side (peer/equipment): echo W-bit primaries, reusing the primary's System Bytes.
	passive := newEndpoint(t, port, false, nil, echoHandler)
	active := newEndpoint(t, port, true, nil, capture)
	defer closeEndpoint(t, active)
	defer closeEndpoint(t, passive)

	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	waitSelected(t, active)
	waitSelected(t, passive)

	// Build the primary to FORWARD: W-bit set (the peer is still asked to reply), caller-chosen
	// System Bytes, and the connection's own session ID.
	primary, err := hsms.NewDataMessage(1, 1, true, active.conn.SessionID(), sysBytes, secs2.A("ping"))
	require.NoError(t, err)

	// ForwardDataMessage: verbatim synchronous write, NO local reply-wait.
	require.NoError(t, active.conn.ForwardDataMessage(ctx, primary))

	// The reply must arrive at the ACTIVE side's handler (not be consumed by the send), carrying the
	// exact System Bytes we chose — proving verbatim wire System Bytes AND handler routing.
	require.Eventually(t, func() bool {
		select {
		case got := <-replies:
			require.Equal(t, sysBytes, got.sysBytes, "the reply must echo the caller-chosen System Bytes verbatim")
			require.Equal(t, byte(2), got.function, "the peer echo is the S1F2 secondary")
			require.Equal(t, "ping", got.body, "the forwarded body must round-trip intact")
			return true
		default:
			return false
		}
	}, 3*time.Second, 10*time.Millisecond, "the forwarded message's reply must reach the active side's DataMessageHandler")

	// A forward is an ordinary data send: the link stays Selected.
	require.Equal(t, hsms.SelectedState, active.conn.State())
}
