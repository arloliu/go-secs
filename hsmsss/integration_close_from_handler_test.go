package hsmsss

import (
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/stretchr/testify/require"
)

// TestClose_FromDataHandlerWaitsOutCloseTimeout pins the documented DataMessageHandler contract:
// Close called from inside a handler waits for the receive goroutine that is running that handler,
// so it always waits out the close timeout and returns ErrCloseTimeout.
// The error carries the transport's own join-timeout text rather than a live-task count,
// because every task had already exited when the deadline ran out.
func TestClose_FromDataHandlerWaitsOutCloseTimeout(t *testing.T) {
	const closeTimeout = 500 * time.Millisecond

	passive, active := newEndpointPair(t, WithConnectionOption(hsms.WithCloseTimeout(closeTimeout)))

	type result struct {
		err     error
		elapsed time.Duration
	}
	res := make(chan result, 1)
	active.conn.AddDataMessageHandler(func(_ *hsms.DataMessage, _ hsms.SECS2Endpoint) {
		start := time.Now()
		err := active.conn.Close()
		select {
		case res <- result{err: err, elapsed: time.Since(start)}:
		default:
		}
	})

	require.NoError(t, passive.conn.Open(t.Context(), hsms.OpenBackground))
	require.NoError(t, active.conn.Open(t.Context(), hsms.OpenWaitSelected))
	waitSelected(t, passive)
	require.NoError(t, passive.conn.SendDataMessageAsync(t.Context(), 1, 1, false, nil))

	select {
	case r := <-res:
		require.ErrorIs(t, r.err, hsms.ErrCloseTimeout)
		require.ErrorContains(t, r.err, "transport teardown join timed out")
		require.GreaterOrEqual(t, r.elapsed, closeTimeout*9/10, "Close must wait out the close timeout")
	case <-time.After(10 * closeTimeout):
		t.Fatal("Close called from a data handler never returned")
	}
}
