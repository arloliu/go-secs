package hsms

// open_close_abort_test.go covers Close never blocking behind an Open that is itself blocked,
// either in OpenWaitSelected's wait for Selected or in an active transport's first dial, and the
// two sides' registration bookkeeping (abortMu, pendingCloses, openAbort) never double-closing an
// abort token no matter how a registering Close races a publishing Open.
//
// Every test drives Open/Close on a goroutine and awaits the result with a bounded timeout (never
// a bare blocking call on the test goroutine), and releases every seam/channel in t.Cleanup, so a
// red run here fails the assertion instead of hanging the package.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/internal/gencap"
	"github.com/stretchr/testify/require"
)

// lifecycleTimeout bounds every Open/Close call this file drives on a goroutine.
// It is generous enough for CI scheduling jitter but short enough that a genuine hang fails the test promptly instead of stalling the whole package.
const lifecycleTimeout = 5 * time.Second

// errFakeDialFailure is a plain sentinel used where a test needs a dial failure unrelated to ctx
// cancellation — never a stdlib context error, which the caller-ctx / abort mapping this file
// exercises treats specially.
var errFakeDialFailure = errors.New("hsms: fake dial failure (test)")

// runOpen starts c.Open(ctx, mode) on its own goroutine and returns a channel that receives exactly the one result.
// It never blocks the caller.
func runOpen(c *connection, ctx context.Context, mode OpenMode) <-chan error {
	done := make(chan error, 1)
	go func() { done <- c.Open(ctx, mode) }()

	return done
}

// runClose starts c.Close() on its own goroutine and returns a channel that receives exactly the one result.
// It never blocks the caller.
func runClose(c *connection) <-chan error {
	done := make(chan error, 1)
	go func() { done <- c.Close() }()

	return done
}

// awaitErr waits for done to deliver a result within lifecycleTimeout, failing the test if it does
// not — the bounded-lifecycle-call discipline this whole file relies on so a red assertion can
// never hang the package.
func awaitErr(t *testing.T, done <-chan error, what string) error {
	t.Helper()

	select {
	case err := <-done:
		return err
	case <-time.After(lifecycleTimeout):
		t.Fatalf("%s did not return within %s", what, lifecycleTimeout)

		return nil
	}
}

// mockScriptBoundDial models an active transport whose dial ctx is derived through
// gencap.DialBounder, so these tests can drive the caller-ctx / abort cases on the FIRST dial
// without a real network call.
//
// started, if non-nil, is closed once BoundDial has been called and the script is parked waiting —
// the seam a test uses to know the dial is in flight before it drives ctx cancellation or Close.
// release, if a value is sent on it, completes the dial successfully
// (as if a real dialer's connect() had already finished) regardless of the bound ctx's state —
// modelling the boundary where a dial that has already succeeded is not retroactively undone.
// Otherwise the script blocks until the bound ctx is done.
// hold, if non-nil, is read from AFTER the bound ctx is done and BEFORE the script returns its
// error — a seam a test uses to keep Start (and so Open) from returning while it inspects
// registration state that must still be live only while Open has not yet returned.
// A nil hold is skipped, so a caller that has no need for this extra pause is unaffected.
func mockScriptBoundDial(started chan<- struct{}, release <-chan net.Conn, hold <-chan struct{}) mockScript {
	return func(ctx context.Context, rt TransportRuntime) error {
		dialCtx := context.Background()
		cancel := func() {}

		if db, ok := rt.(gencap.DialBounder); ok {
			dialCtx, cancel = db.BoundDial(ctx, dialCtx)
		}
		defer cancel()

		if started != nil {
			close(started)
		}

		select {
		case conn := <-release:
			go func() {
				rt.TCPUp(conn)
				driveSelect(ctx, rt)
			}()

			return nil
		case <-dialCtx.Done():
			err := dialCtx.Err()

			if hold != nil {
				<-hold
			}

			return err
		}
	}
}

// mockScriptBoundDialHeldAfterConn is mockScriptBoundDial's release path with an added post-commit
// hold: once the dial has committed to succeeding (a conn was delivered on release), it closes
// committed and then blocks on resume before calling TCPUp/returning — so a test can cancel the
// caller ctx or start a Close during exactly the window between the dial's own commit and Start's
// return, without racing the script's own select between the delivered conn and the bound ctx being
// done (see the boundary tests below for why that race matters).
func mockScriptBoundDialHeldAfterConn(started, committed chan<- struct{}, release <-chan net.Conn, resume <-chan struct{}) mockScript {
	return func(ctx context.Context, rt TransportRuntime) error {
		dialCtx := context.Background()
		cancel := func() {}

		if db, ok := rt.(gencap.DialBounder); ok {
			dialCtx, cancel = db.BoundDial(ctx, dialCtx)
		}
		defer cancel()

		if started != nil {
			close(started)
		}

		select {
		case conn := <-release:
			if committed != nil {
				close(committed)
			}
			<-resume

			go func() {
				rt.TCPUp(conn)
				driveSelect(ctx, rt)
			}()

			return nil
		case <-dialCtx.Done():
			return dialCtx.Err()
		}
	}
}

// dialHandles bundles the two dial-control channels newAbortConn returns, keeping that
// constructor's own return count within revive's function-result-limit.
type dialHandles struct {
	release chan<- net.Conn
	started <-chan struct{}
}

// newAbortConn builds a connection wired to mockScriptBoundDial, mirroring newLifeConn's cleanup
// discipline: Close is registered in t.Cleanup, and cleanup additionally releases the dial (so a
// test that never itself unblocks the dial cannot hang its own cleanup Close).
func newAbortConn(t *testing.T) (c *connection, mt *mockTransport, h dialHandles) {
	t.Helper()

	startedCh := make(chan struct{})
	releaseCh := make(chan net.Conn, 1)

	c, mt = newLifeConn(t, mockScriptBoundDial(startedCh, releaseCh, nil))
	t.Cleanup(func() {
		select {
		case releaseCh <- fakeConn{}:
		default:
		}
	})

	return c, mt, dialHandles{release: releaseCh, started: startedCh}
}

// onceRelease returns a function that closes ch exactly once; further calls are safe no-ops.
//
// Every test below that parks a hook on a channel registers this release in t.Cleanup IMMEDIATELY —
// before doing anything that could fail an assertion — so a failed require call (which unwinds the
// test via runtime.Goexit, skipping the rest of the test body) still releases the parked hook
// instead of leaving it blocked forever and hanging the package's cleanup Close.
func onceRelease(ch chan struct{}) func() {
	var once sync.Once

	return func() { once.Do(func() { close(ch) }) }
}

// ── Close registration ordering never double-closes the abort token ──

// TestClose_RegistrationOrderingNeverDoubleClosesAbortToken exercises BOTH fire() call sites on the
// SAME abort token in a single run: Close A registers (bumps pendingCloses) BEFORE a fresh Open
// publishes its token, so Open's own publish-time check finds pendingCloses > 0 and pre-fires the
// token itself; Close B then registers WHILE Open still holds lifeMu, parked past its own aborted
// dial — Close B's registration finds openAbort already published and fires it again, on the SAME token.
// Both routes share ONE sync.Once, so this must never double-close the token's channel:
// a double-close panics the goroutine that hits it and crashes the whole test binary, which IS the
// failure mode this test exists to catch under either the Open-side or the Close-side Once bypass.
func TestClose_RegistrationOrderingNeverDoubleClosesAbortToken(t *testing.T) {
	startedCh := make(chan struct{})
	releaseCh := make(chan net.Conn, 1)
	holdCh := make(chan struct{})
	releaseHold := onceRelease(holdCh)
	t.Cleanup(releaseHold) // never leave the mock parked past its own abort if the test fails early

	c, _ := newLifeConn(t, mockScriptBoundDial(startedCh, releaseCh, holdCh))
	t.Cleanup(func() {
		select {
		case releaseCh <- fakeConn{}:
		default:
		}
	})

	reached := make(chan struct{})
	proceed := make(chan struct{})
	releaseHook := onceRelease(proceed)
	t.Cleanup(releaseHook)

	var once sync.Once
	c.testHookOpenBeforePublish = func() {
		once.Do(func() { close(reached) })
		<-proceed
	}
	t.Cleanup(func() { c.testHookOpenBeforePublish = nil })

	openDone := runOpen(c, t.Context(), OpenWaitSelected)
	awaitSignal(t, reached, "Open reaching the pre-publish hook")

	// Close A registers now, WHILE Open still holds lifeMu and has not yet published openAbort.
	// Its registration only takes abortMu, never lifeMu, so this returns promptly even though Open
	// is parked.
	closeADone := runClose(c)
	require.Eventually(t, func() bool {
		c.abortMu.Lock()
		defer c.abortMu.Unlock()

		return c.pendingCloses == 1
	}, lifecycleTimeout, time.Millisecond, "Close A must register before Open publishes")

	releaseHook() // Open publishes now, sees pendingCloses > 0, and pre-fires the token itself
	awaitSignal(t, startedCh, "the dial reaching BoundDial")

	// Close B registers now, after Open has already published AND pre-fired the token, but while
	// Open still holds lifeMu (the mock is parked past its own aborted dial, waiting on holdCh) —
	// its registration finds openAbort non-nil and calls fire() on the SAME token Open already fired.
	closeBDone := runClose(c)
	require.Eventually(t, func() bool {
		c.abortMu.Lock()
		defer c.abortMu.Unlock()

		return c.pendingCloses == 2
	}, lifecycleTimeout, time.Millisecond, "Close B must register while Open still holds lifeMu")

	c.abortMu.Lock()
	tok := c.openAbort
	c.abortMu.Unlock()
	require.NotNil(t, tok, "Open must still hold its published token while parked past the aborted dial")
	require.True(t, tok.fired(), "the token must already be fired by the time Close B registers")

	releaseHold() // let the mock return the abort error to Start, and Open roll back

	err := awaitErr(t, openDone, "Open")
	require.ErrorIs(t, err, ErrConnClosed, "an Open aborted by a pending Close must return ErrConnClosed")

	closeAErr := awaitErr(t, closeADone, "Close A")
	closeBErr := awaitErr(t, closeBDone, "Close B")
	require.NoError(t, closeAErr, "a Close pending behind an aborted first dial must return the rollback's recorded result")
	require.NoError(t, closeBErr, "a Close pending behind an aborted first dial must return the rollback's recorded result")
}

// TestClose_FiresAlreadyPublishedAbortToken is Open publishing its token BEFORE any Close
// registers: a later Close must still interrupt it — it finds openAbort non-nil and fires it itself.
func TestClose_FiresAlreadyPublishedAbortToken(t *testing.T) {
	c, _, h := newAbortConn(t)
	started := h.started

	openDone := runOpen(c, t.Context(), OpenBackground)
	awaitSignal(t, started, "the dial reaching BoundDial")

	closeDone := runClose(c)

	err := awaitErr(t, openDone, "Open")
	require.ErrorIs(t, err, ErrConnClosed, "a Close firing the published token must abort the blocked dial")

	require.NoError(t, awaitErr(t, closeDone, "Close"))
}

// TestClose_NeverOpenedDoesNotLeakPendingCloses: Close's registration bookkeeping must not change
// ErrNotOpen's result, and must not leak pendingCloses.
func TestClose_NeverOpenedDoesNotLeakPendingCloses(t *testing.T) {
	c, _ := newLifeConn(t, withMockTransportNeverSelects())

	require.ErrorIs(t, awaitErr(t, runClose(c), "Close"), ErrNotOpen)

	c.abortMu.Lock()
	pending := c.pendingCloses
	c.abortMu.Unlock()
	require.Zero(t, pending, "a completed Close must not leak a pendingCloses registration")
}

// TestClose_PendingBehindAbortedDialReturnsRollbackResult: a REAL Close call that has registered
// (and is blocked acquiring lifeMu) while a fresh Open aborts on its first dial must take the
// idempotent re-close path once it finally acquires lifeMu, and return the rollback's recorded
// result — NOT ErrNotOpen, even though this Open cycle never reached a live generation.
func TestClose_PendingBehindAbortedDialReturnsRollbackResult(t *testing.T) {
	c, mt, h := newAbortConn(t)
	release := h.release

	release <- fakeConn{} // establish + close once, so the connection is in the "reopen" state
	require.NoError(t, awaitErr(t, runOpen(c, t.Context(), OpenBackground), "Open"))
	requireSelected(t, c)
	require.NoError(t, awaitErr(t, runClose(c), "Close"))

	// Fresh started/release/hold channels for the SECOND generation under test.
	started2 := make(chan struct{})
	release2 := make(chan net.Conn, 1)
	hold2 := make(chan struct{})
	releaseHold2 := onceRelease(hold2)
	t.Cleanup(releaseHold2)
	mt.resetStart(mockScriptBoundDial(started2, release2, hold2))
	t.Cleanup(func() {
		select {
		case release2 <- fakeConn{}:
		default:
		}
	})

	reached := make(chan struct{})
	proceed := make(chan struct{})
	releaseHook := onceRelease(proceed)
	t.Cleanup(releaseHook)

	var once sync.Once
	c.testHookOpenBeforePublish = func() {
		once.Do(func() { close(reached) })
		<-proceed
	}
	t.Cleanup(func() { c.testHookOpenBeforePublish = nil })

	openDone := runOpen(c, t.Context(), OpenBackground)
	awaitSignal(t, reached, "the second Open reaching the pre-publish hook")

	// A REAL Close call now registers as pending — it only touches abortMu, so this returns
	// promptly even with Open parked, then blocks trying to acquire lifeMu.
	closeDone := runClose(c)
	require.Eventually(t, func() bool {
		c.abortMu.Lock()
		defer c.abortMu.Unlock()

		return c.pendingCloses > 0
	}, lifecycleTimeout, time.Millisecond, "the pending Close must register before Open publishes")

	releaseHook() // Open publishes, sees pendingCloses > 0, and pre-fires its token
	awaitSignal(t, started2, "the second dial reaching BoundDial")
	releaseHold2() // let the mock return the abort error and Open roll back

	openErr := awaitErr(t, openDone, "Open")
	require.ErrorIs(t, openErr, ErrConnClosed, "an Open that starts while a Close is pending must be aborted")

	closeErr := awaitErr(t, closeDone, "the pending Close")
	require.NoError(t, closeErr, "a Close pending behind an aborted-first-dial Open must return the rollback's recorded result, not ErrNotOpen")
	require.NotErrorIs(t, closeErr, ErrNotOpen)
}

// ── OpenWaitSelected blocked forever, interrupted by Close ──

// TestOpenWaitSelected_BlockedForeverInterruptedByClose: an OpenWaitSelected call that never
// reaches Selected must return ErrConnClosed promptly once Close is called, and Close itself must
// complete normally — not block behind the parked wait.
func TestOpenWaitSelected_BlockedForeverInterruptedByClose(t *testing.T) {
	c, _ := newLifeConn(t, withMockTransportNeverSelects())

	openDone := runOpen(c, t.Context(), OpenWaitSelected)
	require.Eventually(t, func() bool { return c.State() == NotSelectedState }, lifecycleTimeout, time.Millisecond)

	closeDone := runClose(c)

	err := awaitErr(t, openDone, "OpenWaitSelected")
	require.ErrorIs(t, err, ErrConnClosed, "a blocked OpenWaitSelected must return ErrConnClosed once Close interrupts it")

	require.NoError(t, awaitErr(t, closeDone, "Close"))
}

// ── first dial blocked, interrupted by the caller's ctx ──

// TestOpen_FirstDialBlockedCtxDeadlineRollsBack: Open(ctx with a deadline) on a blocked first dial
// returns the ctx error and rolls back cleanly, in both OpenWaitSelected and OpenBackground modes.
func TestOpen_FirstDialBlockedCtxDeadlineRollsBack(t *testing.T) {
	modes := []struct {
		name string
		mode OpenMode
	}{
		{"OpenWaitSelected", OpenWaitSelected},
		{"OpenBackground", OpenBackground},
	}

	for _, m := range modes {
		t.Run(m.name, func(t *testing.T) {
			c, mt, h := newAbortConn(t)
			started := h.started

			ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			defer cancel()

			openDone := runOpen(c, ctx, m.mode)
			awaitSignal(t, started, "the dial reaching BoundDial")

			err := awaitErr(t, openDone, "Open")
			require.ErrorIs(t, err, context.DeadlineExceeded, "an expired caller ctx must roll back with ctx.Err()")

			require.Equal(t, NotConnectedState, c.State(), "the rolled-back generation must leave the FSM NotConnected")

			// The rollback's shutdown result must be recorded, so a following Close returns it
			// without hanging, and a later Open must work again (no half-open state left behind).
			require.NoError(t, awaitErr(t, runClose(c), "Close"))

			// A plain, non-blocking script for the "later Open must work" check below: the
			// bound-dial mock's started channel is single-use per Start call (already consumed by
			// the aborted attempt above), and this phase isn't testing the dial bound anyway.
			mt.resetStart(withMockTransport())

			require.NoError(t, awaitErr(t, runOpen(c, t.Context(), OpenBackground), "Open"), "a later Open must succeed after the rolled-back cycle")
			requireSelected(t, c)
			require.NoError(t, awaitErr(t, runClose(c), "Close"))
		})
	}
}

// TestOpen_BackgroundColdPeerRetrySkippedOnAbortedDial is OpenBackground's specific claim, isolated:
// an expired caller ctx during a blocked first dial must NOT fall into the cold-active background
// retry branch, even though the FSM never left NotConnectedState (the exact condition that branch
// otherwise checks for). Proven deterministically, without a wall-clock wait: the rollback path
// synchronously joins the supervisor before Open returns (so its runDone is already closed), while
// the cold-active retry path leaves the supervisor alive for the background loop — the two are
// mutually exclusive, so checking runDone right after Open returns is conclusive.
func TestOpen_BackgroundColdPeerRetrySkippedOnAbortedDial(t *testing.T) {
	c, mt, h := newAbortConn(t)
	started := h.started

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	openDone := runOpen(c, ctx, OpenBackground)
	awaitSignal(t, started, "the dial reaching BoundDial")

	err := awaitErr(t, openDone, "Open")
	require.ErrorIs(t, err, context.DeadlineExceeded)

	s := c.sup.Load()
	require.NotNil(t, s)
	select {
	case <-s.runDone:
	default:
		t.Fatal("an aborted first dial must take the fatal rollback path (supervisor stopped synchronously), " +
			"not the cold-active background retry, which would leave the supervisor alive")
	}

	require.Equal(t, 1, mt.startCalls(), "an aborted first dial must not fall into the cold-active background retry branch")
	require.Equal(t, NotConnectedState, c.State())
}

// TestOpen_UnrelatedStartErrorNotMappedToCtxErr: a Start failure whose error chain carries no
// context cancellation sentinel must be returned unchanged even when the caller's ctx happens to be
// done at the same moment — proving the error mapping distinguishes a genuinely ctx-caused dial
// failure (a cancelled/expired dial ctx) from an unrelated one
// (a passive listen refused because the port is already in use, for instance),
// rather than mapping every non-nil ctx error onto the returned error regardless of cause.
func TestOpen_UnrelatedStartErrorNotMappedToCtxErr(t *testing.T) {
	started := make(chan struct{})
	proceed := make(chan struct{})
	releaseProceed := onceRelease(proceed)
	t.Cleanup(releaseProceed)

	c, _ := newLifeConn(t, func(ctx context.Context, rt TransportRuntime) error {
		close(started)
		<-proceed

		return errFakeDialFailure
	})

	ctx, cancel := context.WithCancel(t.Context())
	openDone := runOpen(c, ctx, OpenBackground)
	awaitSignal(t, started, "Start reaching its own block")

	cancel() // the caller's ctx is done, but the Start error below is unrelated to it
	releaseProceed()

	err := awaitErr(t, openDone, "Open")
	require.ErrorIs(t, err, errFakeDialFailure, "an unrelated Start error must be returned unchanged, not mapped to ctx.Err()")
	require.NotErrorIs(t, err, context.Canceled)
}

// TestOpen_DialErrorNotMappedToCallerCtxErr: a Start failure that never went through BoundDial's caller-ctx bridge must be returned unchanged, whatever context sentinel it happens to wrap, even though the caller's ctx also happens to be done.
// A wrapped DeadlineExceeded is the realistic case — a WithConnectTimeout-style dial deadline firing on its own.
// A wrapped Canceled is included too: it is the exact sentinel the bridge's own cancel would have produced had it actually fired,
// so it is the case a sentinel-only mapping most easily mistakes for caller cancellation.
// The Start function here never calls BoundDial at all, the deterministic stand-in for "the caller's ctx never reached the bridge":
// a transport that does not route the caller ctx into the dial bound sees exactly this shape,
// so mapAbortedStartErr has no provenance to credit and must leave the raw error alone rather than swap it for ctx.Err() on the coincidence that both are done.
func TestOpen_DialErrorNotMappedToCallerCtxErr(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"dial timeout", fmt.Errorf("hsms: dial timeout (test): %w", context.DeadlineExceeded)},
		{"unrelated cancel", fmt.Errorf("hsms: unrelated cancel (test): %w", context.Canceled)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			started := make(chan struct{})
			proceed := make(chan struct{})
			releaseProceed := onceRelease(proceed)
			t.Cleanup(releaseProceed)

			c, _ := newLifeConn(t, func(ctx context.Context, rt TransportRuntime) error {
				close(started)
				<-proceed

				return tt.err
			})

			ctx, cancel := context.WithCancel(t.Context())
			openDone := runOpen(c, ctx, OpenBackground)
			awaitSignal(t, started, "Start reaching its own block")

			cancel() // the caller's ctx is done, but it never reached BoundDial for this Start call
			releaseProceed()

			err := awaitErr(t, openDone, "Open")
			require.Equal(t, tt.err, err, "the raw Start error must be returned unchanged, not replaced by ctx.Err()")
		})
	}
}

// TestMapAbortedStartErr exercises mapAbortedStartErr directly: a white-box check of the caller-cancellation provenance rule.
// Most cases here are also covered by an Open-level test.
// The one no timing-controlled Open-level test can build deterministically is callerCut paired with a DeadlineExceeded Start error —
// BoundDial's caller bridge fired, but the dial's own timeout still won the race for the merged ctx's own error —
// so this table exercises it directly instead of racing for it.
func TestMapAbortedStartErr(t *testing.T) {
	ctxErr := context.DeadlineExceeded
	dialTimeoutErr := fmt.Errorf("hsms: dial timeout (test): %w", context.DeadlineExceeded)
	bridgeCanceledErr := fmt.Errorf("hsms: dial ctx (test): %w", context.Canceled)
	unrelatedCanceledErr := fmt.Errorf("hsms: unrelated cancel (test): %w", context.Canceled)

	tests := []struct {
		name      string
		err       error
		ctxErr    error
		aborted   bool
		callerCut bool
		want      error
	}{
		{"abort wins over everything", errFakeDialFailure, ctxErr, true, true, ErrConnClosed},
		{"caller-cut Canceled maps to ctxErr", bridgeCanceledErr, ctxErr, false, true, ctxErr},
		{"caller-cut but not Canceled returns raw error", dialTimeoutErr, ctxErr, false, true, dialTimeoutErr},
		{"Canceled without caller-cut returns raw error", unrelatedCanceledErr, ctxErr, false, false, unrelatedCanceledErr},
		{"unrelated error returns unchanged", errFakeDialFailure, ctxErr, false, false, errFakeDialFailure},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mapAbortedStartErr(tt.err, tt.ctxErr, tt.aborted, tt.callerCut)
			require.Equal(t, tt.want, got)
		})
	}
}

// TestClose_DecrementsPendingBeforeUnlockReturns: pausing Close right after it releases lifeMu, but
// before it returns, a queued Open that acquires lifeMu in that window must find pendingCloses
// already decremented — its token must NOT be pre-fired for a Close that has, by this point,
// already finished its own work.
func TestClose_DecrementsPendingBeforeUnlockReturns(t *testing.T) {
	c, mt := newLifeConn(t, withMockTransport())
	require.NoError(t, awaitErr(t, runOpen(c, t.Context(), OpenBackground), "Open"))
	requireSelected(t, c)

	reachedAfterUnlock := make(chan struct{})
	proceedAfterUnlock := make(chan struct{})
	releaseAfterUnlock := onceRelease(proceedAfterUnlock)
	t.Cleanup(releaseAfterUnlock)

	c.testHookCloseAfterUnlock = func() {
		close(reachedAfterUnlock)
		<-proceedAfterUnlock
	}
	t.Cleanup(func() { c.testHookCloseAfterUnlock = nil })

	closeDone := runClose(c)
	awaitSignal(t, reachedAfterUnlock, "Close reaching the post-unlock hook")

	// Reset the mock so the queued Open's generation never reaches Selected: OpenWaitSelected then
	// PARKS this Open in waitSelected, giving a stable window to inspect the published token
	// before Open can possibly clear it — rather than racing a fast, non-blocking Open's own
	// clear-on-return defer, which a still-Close-blocked test would otherwise have to win.
	mt.resetStart(withMockTransportNeverSelects())

	openCtx, openCancel := context.WithCancel(t.Context())
	defer openCancel()
	openDone := runOpen(c, openCtx, OpenWaitSelected)

	require.Eventually(t, func() bool {
		c.abortMu.Lock()
		tok := c.openAbort
		c.abortMu.Unlock()

		return tok != nil
	}, lifecycleTimeout, time.Millisecond, "the queued Open must publish its token")

	c.abortMu.Lock()
	tok := c.openAbort
	c.abortMu.Unlock()
	require.NotNil(t, tok)
	require.False(t, tok.fired(),
		"pendingCloses must already be decremented once lifeMu releases — a Close that has finished must not still abort a fresh Open")

	releaseAfterUnlock()

	require.NoError(t, awaitErr(t, closeDone, "Close"))

	// Cancel the queued Open's own ctx explicitly instead of waiting out a wall-clock deadline —
	// deterministic and immediate, and the exact channel (ctx.Done(), not the abort) that carries
	// this Open to its return proves the already-finished Close's token was never fired for it.
	openCancel()
	err := awaitErr(t, openDone, "Open")
	require.ErrorIs(t, err, context.Canceled,
		"a Close that already finished before this Open published must not abort it")
}

// ── first dial blocked, interrupted by Close ──

// TestOpen_FirstDialBlockedCloseAborts covers Close interrupting a blocked first dial in both
// OpenMode values — they differ only by mode, so one table covers both: Close during a blocked
// first dial must abort Open with ErrConnClosed, and Close itself must complete.
func TestOpen_FirstDialBlockedCloseAborts(t *testing.T) {
	modes := []struct {
		name string
		mode OpenMode
	}{
		{"OpenWaitSelected", OpenWaitSelected},
		{"OpenBackground", OpenBackground},
	}

	for _, m := range modes {
		t.Run(m.name, func(t *testing.T) {
			c, _, h := newAbortConn(t)
			started := h.started

			openDone := runOpen(c, t.Context(), m.mode)
			awaitSignal(t, started, "the dial reaching BoundDial")

			closeDone := runClose(c)

			err := awaitErr(t, openDone, "Open")
			require.ErrorIs(t, err, ErrConnClosed, "a Close during a blocked first dial must abort Open with ErrConnClosed")

			require.NoError(t, awaitErr(t, closeDone, "Close"))
		})
	}
}

// ── BoundDial identity ──

// TestBoundDial_IdentityMismatchIsNoOp: a startCtx that does not match the published firstDial's
// epoch ctx by IDENTITY is never bound, even while that firstDial is still published — a reconnect
// generation's Start (a different ctx value) must never observe the unrelated Open's bound dial ctx
// or abort.
func TestBoundDial_IdentityMismatchIsNoOp(t *testing.T) {
	c, _ := newLifeConn(t, withMockTransportNeverSelects())

	callerCtx, callerCancel := context.WithCancel(context.Background())
	t.Cleanup(callerCancel)

	epochCtx, epochCancel := context.WithCancel(context.Background())
	t.Cleanup(epochCancel)

	abort := make(chan struct{})

	c.firstDial.Store(&firstDialState{epochCtx: epochCtx, callerCtx: callerCtx, abort: abort})
	t.Cleanup(func() { c.firstDial.Store(nil) })

	otherStartCtx := context.Background() // NOT epochCtx — a different identity
	dialCtx, dialCancel := context.WithCancel(context.Background())
	t.Cleanup(dialCancel)

	got, cancel := c.BoundDial(otherStartCtx, dialCtx)
	defer cancel()

	require.True(t, got == dialCtx, "a mismatched startCtx must return dialCtx unchanged")

	// Non-vacuous: fire BOTH signals that would cancel a correctly-matched dial, then check got's
	// OWN error — not merely the absence of a select firing, which a channel nobody closes would
	// pass just as trivially.
	close(abort)
	callerCancel()
	require.NoError(t, got.Err(), "an unrelated Open's abort/caller-cancel must never reach a mismatched dial ctx")
}

// boundDialFixture bundles a matching-identity BoundDial setup for the three cancellation-source
// subtests below, keeping each subtest's own body to the one signal it drives.
type boundDialFixture struct {
	epochCtx     context.Context
	callerCancel context.CancelFunc
	abort        chan struct{}
	dialCtx      context.Context
}

func newBoundDialFixture(t *testing.T) (c *connection, f boundDialFixture) {
	t.Helper()

	c, _ = newLifeConn(t, withMockTransportNeverSelects())

	callerCtx, callerCancel := context.WithCancel(context.Background())
	t.Cleanup(callerCancel)

	epochCtx, epochCancel := context.WithCancel(context.Background())
	t.Cleanup(epochCancel)

	abort := make(chan struct{})

	c.firstDial.Store(&firstDialState{epochCtx: epochCtx, callerCtx: callerCtx, abort: abort})
	t.Cleanup(func() { c.firstDial.Store(nil) })

	dialCtx, dialCancel := context.WithCancel(context.Background())
	t.Cleanup(dialCancel)

	return c, boundDialFixture{epochCtx: epochCtx, callerCancel: callerCancel, abort: abort, dialCtx: dialCtx}
}

// TestBoundDial_MatchingIdentityCancelsFromEverySource: a startCtx that DOES match the published
// firstDial's epoch ctx by identity is bound — the returned ctx is a distinct merged value, and it
// is cancelled by the caller's ctx, by the abort channel, and by the returned cancel func itself.
func TestBoundDial_MatchingIdentityCancelsFromEverySource(t *testing.T) {
	t.Run("callerCtxCancel", func(t *testing.T) {
		c, f := newBoundDialFixture(t)

		got, cancel := c.BoundDial(f.epochCtx, f.dialCtx)
		defer cancel()
		require.True(t, got != f.dialCtx, "a matching startCtx must return a distinct merged ctx")

		f.callerCancel()

		select {
		case <-got.Done():
		case <-time.After(lifecycleTimeout):
			t.Fatal("the caller's ctx cancellation must cancel the merged dial ctx")
		}
	})

	t.Run("abortFire", func(t *testing.T) {
		c, f := newBoundDialFixture(t)

		got, cancel := c.BoundDial(f.epochCtx, f.dialCtx)
		defer cancel()
		require.True(t, got != f.dialCtx)

		close(f.abort)

		select {
		case <-got.Done():
		case <-time.After(lifecycleTimeout):
			t.Fatal("a fired abort must cancel the merged dial ctx")
		}
	})

	t.Run("returnedCancel", func(t *testing.T) {
		c, f := newBoundDialFixture(t)

		got, cancel := c.BoundDial(f.epochCtx, f.dialCtx)
		cancel()

		select {
		case <-got.Done():
		case <-time.After(lifecycleTimeout):
			t.Fatal("the returned cancel func must cancel the merged dial ctx")
		}
		require.ErrorIs(t, got.Err(), context.Canceled)
	})
}

// ── successful dial, then the caller's ctx / a Close arrives ──

// TestOpen_DialAlreadySucceededLaterCtxCancelHasNoEffect: once the first dial has committed to
// succeeding, cancelling the caller's ctx during the window before Start returns — and again well
// after Open has returned — must have NO lifecycle effect on the resulting generation.
func TestOpen_DialAlreadySucceededLaterCtxCancelHasNoEffect(t *testing.T) {
	startedCh := make(chan struct{})
	committedCh := make(chan struct{})
	releaseCh := make(chan net.Conn, 1)
	resumeCh := make(chan struct{})
	resumeOnce := onceRelease(resumeCh)
	t.Cleanup(resumeOnce)

	c, _ := newLifeConn(t, mockScriptBoundDialHeldAfterConn(startedCh, committedCh, releaseCh, resumeCh))
	t.Cleanup(func() {
		select {
		case releaseCh <- fakeConn{}:
		default:
		}
	})

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	openDone := runOpen(c, ctx, OpenBackground)
	awaitSignal(t, startedCh, "the dial reaching BoundDial")
	releaseCh <- fakeConn{} // deliver the conn: the dial commits to succeeding
	awaitSignal(t, committedCh, "the dial committing to its conn")

	// Cancel the caller's ctx WHILE Start is still paused past the dial's own commit, before Start
	// has returned — the exact boundary a post-commit cancellation check must be immune to.
	// This is race-free: the script has already left its own select (proven by committedCh),
	// so this cancel can no longer influence which branch that select picked.
	cancel()
	resumeOnce()

	require.NoError(t, awaitErr(t, openDone, "Open"))
	requireSelected(t, c)

	require.Never(t, func() bool { return c.State() != SelectedState }, 200*time.Millisecond, 5*time.Millisecond,
		"a caller ctx cancelled during or after the post-dial commit window must not affect a live generation")

	require.NoError(t, awaitErr(t, runClose(c), "Close"))
}

// TestOpen_DialAlreadySucceededCloseOwnsTeardown: a Close started while the dial is held in its
// post-commit, pre-return window must not race the (already inert) abort path — Open still
// succeeds normally, and Close owns the resulting teardown.
func TestOpen_DialAlreadySucceededCloseOwnsTeardown(t *testing.T) {
	startedCh := make(chan struct{})
	committedCh := make(chan struct{})
	releaseCh := make(chan net.Conn, 1)
	resumeCh := make(chan struct{})
	resumeOnce := onceRelease(resumeCh)
	t.Cleanup(resumeOnce)

	c, _ := newLifeConn(t, mockScriptBoundDialHeldAfterConn(startedCh, committedCh, releaseCh, resumeCh))
	t.Cleanup(func() {
		select {
		case releaseCh <- fakeConn{}:
		default:
		}
	})

	openDone := runOpen(c, t.Context(), OpenBackground)
	awaitSignal(t, startedCh, "the dial reaching BoundDial")
	releaseCh <- fakeConn{}
	awaitSignal(t, committedCh, "the dial committing to its conn")

	// Start Close WHILE the dial is held past its own commit — Close's registration fires the
	// (already inert) abort token, which has no effect since the script no longer selects on it.
	closeDone := runClose(c)
	resumeOnce()

	require.NoError(t, awaitErr(t, openDone, "Open"), "Open's outcome must be the normal success")
	require.NoError(t, awaitErr(t, closeDone, "Close"), "Close must own the teardown of the just-opened generation")
	// No final-state assertion here: the mock reports TCP-up through the non-generation-aware
	// TCPUp entry point (the same one an out-of-module transport would use), which by contract
	// bypasses the generation gate — so a background TCPUp goroutine racing Close's own teardown
	// can legitimately still flip the FSM after Close returns.
	// That is an accepted property of TCPUp itself, unrelated to what this test exercises.
	// hsmsss's active transport uses the generation-aware entry point instead — see its own equivalent test.
	// secs1 has no such race to begin with:
	// it reports TCP-up synchronously, before its own Start call returns, so no delayed TCP-up can ever land after Close.
}

// ── regression: cold-active background retry still works when ctx is not cancelled ──

// TestOpen_BackgroundColdPeerRetryStillWorksWithLiveCtx: the pre-existing cold-active background
// retry must be unaffected when the caller's ctx is never cancelled and no Close aborts the first
// dial.
func TestOpen_BackgroundColdPeerRetryStillWorksWithLiveCtx(t *testing.T) {
	calls := 0
	c, _ := newLifeConn(t, func(ctx context.Context, rt TransportRuntime) error {
		calls++
		if calls <= 2 {
			return errFakeDialFailure
		}

		go func() {
			rt.TCPUp(fakeConn{})
			driveSelect(ctx, rt)
		}()

		return nil
	})
	require.NoError(t, c.UpdateConfigOptions(WithT5(50*time.Millisecond)))

	err := awaitErr(t, runOpen(c, t.Context(), OpenBackground), "Open")
	require.NoError(t, err, "a cold-active peer under OpenBackground with a live ctx must still return nil and retry in the background")

	requireSelected(t, c)
	require.GreaterOrEqual(t, calls, 3)

	require.NoError(t, awaitErr(t, runClose(c), "Close"))
}
