package secs1

import (
	"sync/atomic"
	"testing"

	"github.com/arloliu/go-secs/hsms"
	"github.com/arloliu/go-secs/logger"
	"github.com/arloliu/go-secs/secs2"
	"github.com/stretchr/testify/require"
)

// warnCountLogger counts Warn calls and suppresses all output (it embeds a
// Fatal-level logger and overrides Warn), so a test can assert exactly how many
// not-Selected drop warnings the throttle emitted.
type warnCountLogger struct {
	logger.Logger
	warns atomic.Int64
}

func (w *warnCountLogger) Warn(string, ...any) { w.warns.Add(1) }

func newWarnCountLogger() *warnCountLogger {
	return &warnCountLogger{Logger: logger.NewSlog(logger.FatalLevel, false)}
}

// TestDropLog_ThrottledUnderFlood verifies the not-Selected drop log is
// rate-limited by dropLogThrottle: a flood of drops within the window emits
// exactly one Warn, while DataMsgDropNotSelectedCount still counts every drop.
// A never-opened connection is deterministically not Selected, so each send hits
// the drop gate.
func TestDropLog_ThrottledUnderFlood(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()

	cl := newWarnCountLogger()
	cfg, err := NewConnectionConfig(testIP, 5000, WithHostRole(), WithLogger(cl))
	require.NoError(err)

	conn, err := NewConnection(ctx, cfg)
	require.NoError(err)

	session := conn.AddSession(testSessionID)
	require.False(conn.stateMgr.IsSelected(), "never-opened connection must not be Selected")

	warnsBefore := cl.warns.Load()

	const drops = 8
	for range drops {
		m, mErr := hsms.NewDataMessage(1, 1, false, testSessionID, hsms.GenerateMsgSystemBytes(), secs2.A("x"))
		require.NoError(mErr)
		require.ErrorIs(session.SendMessageAsync(m), ErrNotSelectedState)
	}

	require.Equal(uint64(drops), conn.metrics.DataMsgDropNotSelectedCount.Load(),
		"every drop must be counted regardless of log throttling")
	require.Equal(warnsBefore+1, cl.warns.Load(),
		"a flood of drops within the window must emit exactly one Warn")
}
