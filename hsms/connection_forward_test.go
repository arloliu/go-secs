package hsms

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestForward_WritesVerbatimPreservingEnvelope proves ForwardDataMessage writes the pre-built
// message byte-for-byte (System Bytes and W-bit intact) on the caller's goroutine and counts it as
// one on-wire send. This is the transparent-proxy contract: the envelope the caller built is what
// goes on the wire, unlike SendDataMessage which stamps fresh System Bytes.
func TestForward_WritesVerbatimPreservingEnvelope(t *testing.T) {
	c, tr := newTestSendConn(t, SelectedState)

	sb := [4]byte{0xDE, 0xAD, 0xBE, 0xEF}
	msg := mustSendData(t, sb, true) // W-bit set: the peer is still asked to reply
	require.True(t, msg.WaitBit())

	require.NoError(t, c.ForwardDataMessage(t.Context(), msg))

	frames := tr.writtenFrames()
	require.Len(t, frames, 1)
	require.Equal(t, msg.ToBytes(), frames[0], "forwarded frame must equal the pre-built message verbatim (System Bytes + W-bit preserved)")
	require.Equal(t, uint64(1), c.metrics.DataMsgSendCount(), "a forwarded frame on the wire counts as one send")
}

// TestForward_DoesNotRegisterReply is the crux of the forward/relay semantic: ForwardDataMessage
// registers NO reply channel, so the correlated secondary MISSES the reply registry and would fall
// through to RouteData (the session handlers) rather than being consumed by a waiting sender. This
// is what lets a router own reply correlation via its own inbound handler.
func TestForward_DoesNotRegisterReply(t *testing.T) {
	c, _ := newTestSendConn(t, SelectedState)

	sb := [4]byte{0, 0, 0, 7}
	require.NoError(t, c.ForwardDataMessage(t.Context(), mustSendData(t, sb, true)))

	e := c.cur.Load()
	require.Zero(t, e.replies.len(), "ForwardDataMessage must not register a reply expectation")
	require.False(t, c.RouteReply(mustSendReply(t, sb)),
		"the correlated reply must MISS the registry (routes to handlers), not satisfy a waiting sender")
}

// TestForward_B1GatedWhenNotSelected proves the forward path honours the B1 data-send gate: a
// forward while !Selected is refused before any write and counted once at the B3 chokepoint.
func TestForward_B1GatedWhenNotSelected(t *testing.T) {
	c, tr := newTestSendConn(t, NotSelectedState)

	err := c.ForwardDataMessage(t.Context(), mustSendData(t, [4]byte{0, 0, 0, 1}, true))
	require.ErrorIs(t, err, ErrNotSelectedState)
	require.Equal(t, 0, tr.writeCount(), "a B1-gated forward must not reach the wire")
	require.Equal(t, uint64(1), c.metrics.DataMsgDropNotSelectedCount(), "the gated forward is counted at the B3 chokepoint")
}

// TestForward_ReturnsWriteErrorSynchronously proves ForwardDataMessage surfaces a transport write
// failure on the caller's goroutine (bumping DataMsgErrCount, not DataMsgSendCount) — the property
// a router relies on to clean up its own pending-request table immediately instead of waiting out
// a reply timeout.
func TestForward_ReturnsWriteErrorSynchronously(t *testing.T) {
	c, tr := newTestSendConn(t, SelectedState)
	tr.writeErr = errors.New("boom")

	err := c.ForwardDataMessage(t.Context(), mustSendData(t, [4]byte{0, 0, 0, 1}, true))
	require.Error(t, err)
	require.Equal(t, uint64(1), c.metrics.DataMsgErrCount(), "a failed-before-wire forward is a data-message error")
	require.Equal(t, uint64(0), c.metrics.DataMsgSendCount(), "a forward that never reached the wire must not count as a send")
}

// TestForwardAsync_EnqueuesVerbatim proves ForwardDataMessageAsync enqueues the pre-built message
// unchanged on the per-generation async channel (no fresh System Bytes, no reply registration).
func TestForwardAsync_EnqueuesVerbatim(t *testing.T) {
	c, _ := newTestSendConn(t, SelectedState)

	sb := [4]byte{0x11, 0x22, 0x33, 0x44}
	msg := mustSendData(t, sb, true)
	require.NoError(t, c.ForwardDataMessageAsync(t.Context(), msg))

	e := c.cur.Load()
	require.Len(t, e.sendCh, 1, "the forwarded message must be enqueued")
	req := <-e.sendCh
	dm, ok := req.msg.(*DataMessage)
	require.True(t, ok)
	require.Same(t, msg, dm, "the async forward must enqueue the caller's message verbatim, not a rebuilt copy")
	require.Zero(t, e.replies.len(), "the async forward must not register a reply expectation")
}

// TestForward_NilMessage proves both forward entry points reject a nil message with ErrNilMessage
// rather than nil-dereferencing.
func TestForward_NilMessage(t *testing.T) {
	c, _ := newTestSendConn(t, SelectedState)

	require.ErrorIs(t, c.ForwardDataMessage(t.Context(), nil), ErrNilMessage)
	require.ErrorIs(t, c.ForwardDataMessageAsync(t.Context(), nil), ErrNilMessage)
}

// TestForward_NotOpenBeforeEpoch proves both forward entry points return ErrNotOpen when no
// generation has been published (before the first Open), rather than nil-dereferencing the epoch.
func TestForward_NotOpenBeforeEpoch(t *testing.T) {
	conn, err := NewConnection(DefaultConnectionConfig(), &mockTransport{})
	require.NoError(t, err)
	c, ok := conn.(*connection)
	require.True(t, ok)
	require.Nil(t, c.cur.Load(), "no epoch is published before the first Open")

	msg := mustSendData(t, [4]byte{0, 0, 0, 1}, true)
	require.ErrorIs(t, c.ForwardDataMessage(t.Context(), msg), ErrNotOpen)
	require.ErrorIs(t, c.ForwardDataMessageAsync(t.Context(), msg), ErrNotOpen)
}
