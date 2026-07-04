package hsms

// send_metrics_test.go — white-box wiring tests for the send-path ConnectionMetrics counters
// (DataMsgSendCount / DataMsgErrCount). The pure atomic getters/inc helpers are covered in
// connection_metrics_test.go; these tests prove the increments fire at the intended call sites
// (writeFrame for on-wire sends, sendWaitReply for transaction outcomes) with the correct
// semantics, reusing the send-path harness (newTestSendConn / mustSendData / mustSendReply /
// mockTransport) from send_test.go.
//
// Linktest-specific coverage (LinktestSendCount/LinktestRecvCount/LinktestErrCount) moved to
// hsmsss.ConnectionMetrics — see hsmsss/integration_metrics_test.go for the equivalent end-to-end
// linktest send/recv/err coverage.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestSendMetrics_DataMsgSend_OnWireOnlyForData proves DataMsgSendCount is bumped once per DATA
// frame committed to the wire (including a secondary/reply send) and NOT for control frames.
func TestSendMetrics_DataMsgSend_OnWireOnlyForData(t *testing.T) {
	c, _ := newTestSendConn(t, SelectedState)
	e := c.cur.Load()

	require.Equal(t, uint64(0), c.metrics.DataMsgSendCount())

	// Primary data frame → counted once.
	require.NoError(t, c.writeFrame(t.Context(), e, mustSendData(t, [4]byte{0, 0, 0, 1}, true)))
	require.Equal(t, uint64(1), c.metrics.DataMsgSendCount())

	// Control frame (Select.req) → NOT counted.
	require.NoError(t, c.writeFrame(t.Context(), e, NewSelectReq(0xFFFF, [4]byte{0, 0, 0, 2})))
	require.Equal(t, uint64(1), c.metrics.DataMsgSendCount(), "control frames must not bump DataMsgSendCount")

	// Secondary (reply) data frame → counted too.
	require.NoError(t, c.writeFrame(t.Context(), e, mustSendReply(t, [4]byte{0, 0, 0, 3})))
	require.Equal(t, uint64(2), c.metrics.DataMsgSendCount(), "secondary/reply data sends count too")
}

// TestSendMetrics_DataMsgSend_NotCountedWhenWriteFails proves a data send that never reaches the
// wire (transport write error) does NOT bump DataMsgSendCount.
func TestSendMetrics_DataMsgSend_NotCountedWhenWriteFails(t *testing.T) {
	c, tr := newTestSendConn(t, SelectedState)
	tr.writeErr = errors.New("boom")
	e := c.cur.Load()

	require.Error(t, c.writeFrame(t.Context(), e, mustSendData(t, [4]byte{0, 0, 0, 1}, true)))
	require.Equal(t, uint64(0), c.metrics.DataMsgSendCount(), "a send that never reached the wire must not count")
}

// TestSendMetrics_DataMsgSend_NotCountedOnB2Drop proves a data frame dropped at the B2 write
// boundary (NotSelected under writeMu) does NOT bump DataMsgSendCount (it is a drop, not a send).
func TestSendMetrics_DataMsgSend_NotCountedOnB2Drop(t *testing.T) {
	c, _ := newTestSendConn(t, SelectedState)
	c.testHookAfterWriteLock = func() {
		if s := c.sup.Load(); s != nil {
			s.state.Store(uint32(NotSelectedState))
		}
	}
	e := c.cur.Load()

	require.ErrorIs(t, c.writeFrame(t.Context(), e, mustSendData(t, [4]byte{0, 0, 0, 1}, true)), ErrNotSelectedState)
	require.Equal(t, uint64(0), c.metrics.DataMsgSendCount(), "a B2-dropped frame must not count as a send")
	require.Equal(t, uint64(1), c.metrics.DataMsgDropNotSelectedCount(), "the B2 drop is counted at its own chokepoint")
}

// TestSendMetrics_DataMsgErr_OnT3Timeout proves a W-bit data transaction whose reply never arrives
// bumps DataMsgErrCount exactly once — while DataMsgSendCount still records the on-wire primary.
func TestSendMetrics_DataMsgErr_OnT3Timeout(t *testing.T) {
	c, _ := newTestSendConn(t, SelectedState)
	c.cfg.Load().timers.T3 = 30 * time.Millisecond

	_, err := c.sendWaitReply(t.Context(), mustSendData(t, [4]byte{0, 0, 0, 1}, true))
	require.ErrorIs(t, err, ErrT3Timeout)
	require.Equal(t, uint64(1), c.metrics.DataMsgErrCount(), "a T3 reply timeout is a data-message error")
	require.Equal(t, uint64(1), c.metrics.DataMsgSendCount(), "the primary reached the wire before T3 fired")
}

// TestSendMetrics_DataMsgErr_OnWriteError proves a data send that fails before the wire (transport
// write error) bumps DataMsgErrCount and not DataMsgSendCount.
func TestSendMetrics_DataMsgErr_OnWriteError(t *testing.T) {
	c, tr := newTestSendConn(t, SelectedState)
	tr.writeErr = errors.New("boom")

	_, err := c.sendWaitReply(t.Context(), mustSendData(t, [4]byte{0, 0, 0, 1}, true))
	require.Error(t, err)
	require.Equal(t, uint64(1), c.metrics.DataMsgErrCount(), "a failed-before-wire send is a data-message error")
	require.Equal(t, uint64(0), c.metrics.DataMsgSendCount())
}

// TestSendMetrics_FireAndForget_SendPlusOne_ErrZero proves Fix B: a !W DATA send via the
// synchronous path records DataMsgSend+1 (the frame reached the wire) and DataMsgErr+0 — it
// short-circuits after the write, so it never reaches the T3-timeout error path. This resolves the
// metrics-review concern that a !W send previously recorded send+1 AND err+1. A short T3 is pinned
// so a regression (register-and-wait) would surface as a fast ErrT3Timeout.
func TestSendMetrics_FireAndForget_SendPlusOne_ErrZero(t *testing.T) {
	c, _ := newTestSendConn(t, SelectedState)
	c.cfg.Load().timers.T3 = 50 * time.Millisecond

	reply, err := c.sendWaitReply(t.Context(), mustSendData(t, [4]byte{0, 0, 0, 1}, false))
	require.NoError(t, err, "a !W send short-circuits (Fix B), it does not time out on T3")
	require.Nil(t, reply)
	require.Equal(t, uint64(1), c.metrics.DataMsgSendCount(), "the !W frame reached the wire")
	require.Equal(t, uint64(0), c.metrics.DataMsgErrCount(), "a !W send is send+1, err+0 (metrics concern resolved)")
}

// TestSendMetrics_DataMsgErr_NotOnSuccessOrCancelOrDrop proves DataMsgErrCount is NOT bumped by a
// successful reply, a caller cancellation, a connection drop, or a B1 NotSelected drop.
func TestSendMetrics_DataMsgErr_NotOnSuccessOrCancelOrDrop(t *testing.T) {
	t.Run("reply-success", func(t *testing.T) {
		c, _ := newTestSendConn(t, SelectedState)
		sb := [4]byte{0, 0, 0, 1}

		done := make(chan error, 1)
		go func() {
			_, err := c.sendWaitReply(t.Context(), mustSendData(t, sb, true))
			done <- err
		}()
		require.Eventually(t, func() bool { return c.metrics.DataMsgSendCount() == 1 }, 2*time.Second, time.Millisecond)
		require.True(t, c.RouteReply(mustSendReply(t, sb)))
		require.NoError(t, <-done)
		require.Equal(t, uint64(0), c.metrics.DataMsgErrCount(), "a correlated reply is a success, not an error")
	})

	t.Run("caller-cancel", func(t *testing.T) {
		c, _ := newTestSendConn(t, SelectedState)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		done := make(chan error, 1)
		go func() {
			_, err := c.sendWaitReply(ctx, mustSendData(t, [4]byte{0, 0, 0, 2}, true))
			done <- err
		}()
		require.Eventually(t, func() bool { return c.metrics.DataMsgInflightCount() == 1 }, 2*time.Second, time.Millisecond)
		cancel()
		require.ErrorIs(t, <-done, context.Canceled)
		require.Equal(t, uint64(0), c.metrics.DataMsgErrCount(), "caller cancellation is not a data-message error")
	})

	t.Run("conn-drop", func(t *testing.T) {
		c, _ := newTestSendConn(t, SelectedState)
		e := c.cur.Load()

		done := make(chan error, 1)
		go func() {
			_, err := c.sendWaitReply(t.Context(), mustSendData(t, [4]byte{0, 0, 0, 3}, true))
			done <- err
		}()
		require.Eventually(t, func() bool { return c.metrics.DataMsgInflightCount() == 1 }, 2*time.Second, time.Millisecond)
		e.teardown(2 * time.Second)
		require.ErrorIs(t, <-done, ErrConnClosed)
		require.Equal(t, uint64(0), c.metrics.DataMsgErrCount(), "a connection drop is not a data-message error")
	})

	t.Run("b1-drop", func(t *testing.T) {
		c, _ := newTestSendConn(t, NotSelectedState)
		_, err := c.sendWaitReply(t.Context(), mustSendData(t, [4]byte{0, 0, 0, 4}, true))
		require.ErrorIs(t, err, ErrNotSelectedState)
		require.Equal(t, uint64(0), c.metrics.DataMsgErrCount(), "a B1 NotSelected drop is a drop, not an error")
	})
}

// Linktest-specific tests (success round-trip, T6 timeout, caller-side deadline, and
// teardown/cancel non-counting) moved to hsmsss.ConnectionMetrics coverage — see
// hsmsss/integration_metrics_test.go.
