package hsmsss

// transport_accept_diagnostic_test.go covers acceptConn's accept-retry diagnostic running off the
// accept goroutine, on a goroutine nothing joins, so a blocking logger can never stall Stop's
// unbounded g.accept.Wait, and the in-flight gate resetting so a later retry can still warn.
//
// It reuses alwaysFailingListener from transport_accept_retry_test.go (same package): a listener
// that fails every Accept with a transient error until Close, driving acceptConn's retry loop
// indefinitely so this test can observe the diagnostic gating deterministically.

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/logger"
	"github.com/stretchr/testify/require"
)

// blockingWarnLogger blocks the FIRST call whose message matches watchMsg until release closes,
// and counts EVERY call whose message matches (not global goroutines — the diagnostic gates at
// most one goroutine in flight, so a correct implementation calls Warn again only once the prior
// call's goroutine has reset the gate). A closed channel never blocks a later receive, so every
// call after release has been closed once passes straight through.
type blockingWarnLogger struct {
	logger.Logger

	watchMsg string
	entered  chan struct{}
	release  <-chan struct{}

	entries     atomic.Int64
	enteredOnce sync.Once
}

func newBlockingWarnLogger(watchMsg string, release <-chan struct{}) (*blockingWarnLogger, <-chan struct{}) {
	entered := make(chan struct{})

	return &blockingWarnLogger{
		Logger:   logger.Default(),
		watchMsg: watchMsg,
		entered:  entered,
		release:  release,
	}, entered
}

func (l *blockingWarnLogger) Warn(msg string, keysAndValues ...any) {
	if msg != l.watchMsg {
		l.Logger.Warn(msg, keysAndValues...)

		return
	}

	l.entries.Add(1)
	l.enteredOnce.Do(func() { close(l.entered) })
	<-l.release
}

func (l *blockingWarnLogger) With(...any) logger.Logger { return l }

// TestPassive_AcceptRetryDiagnosticDoesNotStallStop covers three things in sequence: a blocking
// accept-retry logger must not stall the accept loop's own retry cadence (at most one diagnostic
// goroutine outstanding while further Accept failures keep happening); once that first blocked
// call is released, the in-flight gate resets so a SECOND real retry can warn again — proving the
// gate is not permanently latched; and Stop still completes within its ctx deadline afterward.
//
// Teeth: reverting to a direct, inline log.Warn call in acceptConn (transport_passive.go) wedges
// the accept goroutine on the FIRST blocked call, so the retry-progress assertion
// (ln.calls.Load() >= 5) times out — the accept loop can no longer retry at all, which is the
// assertion that actually catches this regression, not the later Stop-timing check.
// Teeth: deleting the `defer t.acceptWarnInFlight.Store(false)` in reportAcceptRetry leaves the
// CAS gate permanently held after the first diagnostic, so no second Warn entry ever arrives and
// the require.Eventually(entries >= 2) below times out.
func TestPassive_AcceptRetryDiagnosticDoesNotStallStop(t *testing.T) {
	ln := &alwaysFailingListener{}
	listen := func(context.Context, string, string) (net.Listener, error) { return ln, nil }

	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	blockingLogger, entered := newBlockingWarnLogger("hsmsss: accept failed, retrying", release)

	cfg, err := NewConfig("127.0.0.1", 0, WithPassive(), WithListener(listen),
		WithConnectionOption(hsms.WithLogger(blockingLogger)))
	require.NoError(t, err)

	tr := newTransport(cfg)

	// Register cleanup BEFORE starting the transport: release the blocked logger and bound the
	// Stop join, so a failure anywhere below still lets this test's own cleanup Close complete
	// instead of leaking the blocked accept goroutine into the rest of the package's -count run.
	t.Cleanup(func() {
		releaseOnce()

		stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = tr.Stop(stopCtx)
	})

	require.NoError(t, tr.startPassive(context.Background()))

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the accept-retry diagnostic never ran")
	}

	// While the one diagnostic goroutine is blocked, the accept loop must keep retrying — a
	// blocked logger must not stall acceptConn's own retry loop, only the ONE diagnostic call.
	require.Eventually(t, func() bool { return ln.calls.Load() >= 5 }, 3*time.Second, time.Millisecond,
		"accept retries must keep happening while the diagnostic goroutine is blocked")

	require.Equal(t, int64(1), blockingLogger.entries.Load(),
		"at most one accept-retry diagnostic goroutine may be in flight at a time")

	// Phase 2: release the blocked call — the accept loop is still live and Accept keeps failing,
	// so the gate resets and a genuine SECOND Warn must arrive through the ordinary retry path.
	releaseOnce()
	require.Eventually(t, func() bool { return blockingLogger.entries.Load() >= 2 }, 3*time.Second, time.Millisecond,
		"the in-flight gate must reset once the first diagnostic goroutine returns, letting a later retry warn again")

	stopCtx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- tr.Stop(stopCtx) }()

	select {
	case err := <-done:
		require.NoError(t, err, "Stop must complete within its deadline")
	case <-time.After(2500 * time.Millisecond):
		t.Fatal("Stop stalled")
	}
}
