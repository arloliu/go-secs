package hsmsss

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// assertCleanShutdown is the primary leak gate for hsmsss tests.
//
// It checks the package's authoritative internal counters after a Connection
// has been closed: every reply-channel map entry, every taskMgr-tracked
// goroutine, every queued sender request, and the in-flight data-message
// gauge must return to zero. The checks are polled briefly to absorb the
// short window where the close path is still running its post-Wait drain.
//
// Per chaos plan §7 #7, internal counters are the primary (must-pass)
// invariant layer. Coarse goroutine / fd counts live at the integration
// layer in tests/hsmsss_integration/.
//
// Verifiable-success criterion (chaos plan §3 P0.4): reverting any of the
// 1.16.1 leak-fix commits — 0eec4b2 (replyMsgChans leak on Reject),
// 3d2e3ad (stranded replyErrs), 075d6d3 (DataMsgInflightCount underflow),
// b1d9cb4 (selectSession-via-taskMgr join), f604180 (post-Wait
// senderMsgChan drain) — must trip ≥ 1 of these assertions.
func assertCleanShutdown(t testing.TB, c *Connection) {
	t.Helper()

	require := require.New(t)

	// Poll briefly. The post-close drain inside closeConn returns before
	// asyncStateChangeTask / asyncHandlerDispatcher have fully unblocked
	// their consumers, so observers can race past the last decrement.
	const (
		pollEvery = 5 * time.Millisecond
		pollFor   = 500 * time.Millisecond
	)

	require.Eventually(func() bool {
		return c.replyMsgChans.Size() == 0 &&
			c.replyErrs.Size() == 0 &&
			c.GetMetrics().DataMsgInflightCount.Load() == 0 &&
			c.taskMgr.TaskCount() == 0 &&
			len(c.senderMsgChan) == 0
	}, pollFor, pollEvery,
		"clean-shutdown invariants not met after close: "+
			"replyMsgChans=%d replyErrs=%d DataMsgInflight=%d tasks=%d senderQueue=%d",
		c.replyMsgChans.Size(),
		c.replyErrs.Size(),
		c.GetMetrics().DataMsgInflightCount.Load(),
		c.taskMgr.TaskCount(),
		len(c.senderMsgChan),
	)

	// Sharp per-counter assertions for crisp failure messages once we know
	// the aggregate Eventually passed (or, if it didn't, fail-fast on the
	// final observed value). These are redundant under success but turn
	// failure mode into "X is wrong" instead of "everything is wrong".
	require.Equal(0, c.replyMsgChans.Size(),
		"replyMsgChans leak — pending reply channels remain after close")
	require.Equal(0, c.replyErrs.Size(),
		"replyErrs leak — stranded reply errors remain after close")
	require.Equal(int64(0), c.GetMetrics().DataMsgInflightCount.Load(),
		"DataMsgInflightCount imbalance — non-zero in-flight gauge after close")
	require.Equal(0, c.taskMgr.TaskCount(),
		"taskMgr leak — managed goroutines still running after close")
	require.Equal(0, len(c.senderMsgChan),
		"senderMsgChan not drained — frames stranded after close")
}
