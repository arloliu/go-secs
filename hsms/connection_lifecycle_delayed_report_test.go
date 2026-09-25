package hsms

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// runtimeConnection resolves rt back to the *connection it wraps,
// the way an in-package test reaches implementation details a real (out-of-module) transport never touches.
// It panics rather than comma-ok'ing quietly:
// production always passes the connection itself as rt (Open calls c.tr.Start(e.ctx, c)),
// so a false comma-ok here means the test itself is wired wrong, not a legitimate runtime case.
func runtimeConnection(rt TransportRuntime) *connection {
	c, ok := rt.(*connection)
	if !ok {
		panic("hsms_test: the mock runtime must be the connection itself")
	}

	return c
}

// withMockTransportDisconnectDuringDelayedTCPUpReport arms the FIRST generation's TCP-up commit with a withheld-enqueue hook:
// the CAS lands (state reaches NotSelected)
// but, before its own evTCPUp report reaches the queue, a real disconnect is injected ahead of it —
// reproducing a disconnect that overtakes a TCP-up report which has not yet been reported.
// The hook then lets the delayed evTCPUp enqueue normally, right behind the disconnect, and clears itself
// so it never intercepts a later generation's own commit.
//
// It also installs testHookAfterStateLoad BEFORE this generation's very first commit —
// the only point that is safely synchronized against run(),
// which has nothing to read the hook FOR until this same call chain enqueues its own first event —
// so the sentinelSeen signal test-side code waits on later never races run()'s step() reading the same field.
// The closure only ever acts on evSelectLost, the test's sentinel,
// so leaving it installed for the rest of the supervisor's life (across every later generation) is inert.
//
// The SECOND call — the successor generation this scenario targets — blocks on release before dialing,
// so the test controls exactly when its own bring-up runs,
// after it has confirmed the delayed report was settled, never before.
// It installs its own one-shot testHookBeforeEnqueue right before its TCP-up call, on the SAME goroutine
// that then calls it
// (no cross-goroutine synchronization needed — commitFrom reads the hook on the caller's own goroutine, never run()'s),
// and closes successorCASSucceeded from inside it:
// that hook runs only once commitFrom's CAS has actually succeeded (see testHookBeforeEnqueue's field doc),
// so a resurrected NotSelected —
// which would make this generation's own NotConnected -> NotSelected CAS fail —
// starves the close instead of firing it.
//
// Any call past the second is outside this scenario's scope
// (this scenario's own reconnect ends at the second generation)
// and connects normally, touching neither the shared hooks nor successorCASSucceeded:
// only the exact second call ever closes that channel,
// so it can never be closed twice even under a stray extra reconnect.
func withMockTransportDisconnectDuringDelayedTCPUpReport(sentinelSeen chan<- struct{}, release <-chan struct{}, successorCASSucceeded chan<- struct{}) mockScript {
	var calls atomic.Int32

	return func(ctx context.Context, rt TransportRuntime) error {
		switch calls.Add(1) {
		case 1:
			sup := runtimeConnection(rt).sup.Load()
			sup.testHookAfterStateLoad = func(ev fsmEvent) {
				if ev == evSelectLost {
					close(sentinelSeen)
				}
			}
			sup.testHookBeforeEnqueue = func(_ uint64, ev fsmEvent, _ TransitionCause) (skipEnqueue bool) {
				if ev != evTCPUp {
					return false
				}
				// A single-shot commit, filtered to evTCPUp:
				// this branch cannot run a second time for this generation,
				// so nothing here needs its own sync.Once.
				sup.testHookBeforeEnqueue = nil // never intercepts a later generation's commit
				rt.TCPDown(io.EOF)              // overtakes this TCP-up's own (not yet enqueued) report

				return false // let the delayed evTCPUp enqueue normally, right behind the disconnect
			}
			rt.TCPUp(fakeConn{})

			return nil
		case 2:
			go func() {
				<-release // released only once the test has confirmed the delayed evTCPUp was stepped

				sup := runtimeConnection(rt).sup.Load()
				sup.testHookBeforeEnqueue = func(_ uint64, ev fsmEvent, _ TransitionCause) (skipEnqueue bool) {
					if ev == evTCPUp {
						sup.testHookBeforeEnqueue = nil
						close(successorCASSucceeded)
					}

					return false
				}
				rt.TCPUp(fakeConn{})
				driveSelect(ctx, rt)
			}()

			return nil
		default:
			go func() {
				rt.TCPUp(fakeConn{})
				driveSelect(ctx, rt)
			}()

			return nil
		}
	}
}

// TestReconnect_DelayedTCPUpReportOvertakenByDisconnectCannotResurrect proves the second lost-reaction order end to end:
// a generation's TCP-up commit lands (state moves to NotSelected) before its own report reaches the queue,
// a real disconnect overtakes it and drops the link,
// and the delayed TCP-up report finally arrives.
// It must not resurrect NotSelected on the generation that already dropped —
// the successor's own TCP-up, dialed by the reconnect loop, is what brings the connection back.
//
// Two deterministic markers replace polling c.State() for an absence:
// a sentinel event (testHookAfterStateLoad's signal, FIFO-ordered strictly behind the delayed evTCPUp)
// proves the delayed report has already been stepped before the successor is ever released to dial,
// and the successor's own testHookBeforeEnqueue signal proves its TCP-up CAS actually succeeded —
// which a resurrected NotSelected would have starved instead.
func TestReconnect_DelayedTCPUpReportOvertakenByDisconnectCannotResurrect(t *testing.T) {
	sentinelSeen := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	successorCASSucceeded := make(chan struct{})

	c, mt := newLifeConn(t, withMockTransportDisconnectDuringDelayedTCPUpReport(sentinelSeen, release, successorCASSucceeded))
	// LIFO: runs BEFORE newLifeConn's own Close cleanup,
	// so a test that fails before reaching the happy-path release below still frees the successor goroutine instead of hanging Close's join.
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	require.NoError(t, c.UpdateConfigOptions(WithReconnectBackoff(300*time.Millisecond, 1.0)))

	var mu sync.Mutex
	var drops []LifecycleEvent
	cancel := c.SubscribeLifecycle(func(ev LifecycleEvent) {
		if ev.Current == NotConnectedState {
			mu.Lock()
			drops = append(drops, ev)
			mu.Unlock()
		}
	})
	defer cancel()

	require.NoError(t, c.Open(t.Context(), OpenBackground))

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()

		return len(drops) == 1
	}, 3*time.Second, time.Millisecond, "the disconnect that overtook the delayed TCP-up report must still drop the link")
	mu.Lock()
	require.Equal(t, NotSelectedState, drops[0].Previous, "the reported drop must name the state the link actually reached")
	mu.Unlock()

	// Enqueue the sentinel event the mock script's testHookAfterStateLoad is already watching for:
	// illegal from NotConnectedState either way (a safe no-op),
	// but FIFO guarantees step() cannot begin processing it before the delayed evTCPUp —
	// already queued behind the disconnect — has itself been fully stepped.
	sup := c.sup.Load()
	sup.inject(evSelectLost, CauseUnknown)

	select {
	case <-sentinelSeen:
	case <-time.After(3 * time.Second):
		t.Fatal("the delayed TCP-up report was never stepped")
	}

	require.Equal(t, NotConnectedState, c.State(), "the delayed TCP-up report must not resurrect NotSelected on the generation that already dropped")

	releaseOnce.Do(func() { close(release) }) // the delayed report is settled — let the successor dial

	select {
	case <-successorCASSucceeded:
	case <-time.After(3 * time.Second):
		t.Fatal("the successor's own TCP-up CAS never succeeded — a resurrected NotSelected would starve it")
	}

	require.Eventually(t, func() bool { return mt.startCalls() == 2 && c.State() == SelectedState }, 3*time.Second, time.Millisecond,
		"the successor generation must reselect on its own TCP-up")

	require.NoError(t, c.Close())
}

// pendingReport is one synchronous commit's report
// that a test withheld via supervisor.testHookBeforeEnqueue,
// replayed later (via injectFrom) once the scenario under test is ready for it.
type pendingReport struct {
	gen   uint64
	ev    fsmEvent
	cause TransitionCause
}

// errMockSelectCASDidNotLand is returned by withMockTransportWithholdingReports's Start
// when rt.CommitSelected unexpectedly fails to land its CAS synchronously —
// a violation of this mock's own precondition (TCPUp always leaves state NotSelected for CommitSelected to CAS out of),
// not a scenario either test drives,
// so it is surfaced as an ordinary Start error instead of a panic:
// a panic on Open's calling goroutine would crash the test in a way that shadows the real failure.
var errMockSelectCASDidNotLand = errors.New("hsms_test: the Select CAS must land synchronously")

// withMockTransportWithholdingReports commits TCP-up then Select SYNCHRONOUSLY inside Start —
// state reaches Selected before Start returns —
// while withholding BOTH commits' own reports (via supervisor.testHookBeforeEnqueue) into *pending,
// so neither ever reaches the FSM queue until the test replays them.
// Start then returns startErr (nil for a plain successful Open, non-nil to drive Open's own rollback).
func withMockTransportWithholdingReports(pending *[]pendingReport, mu *sync.Mutex, startErr error) mockScript {
	return func(_ context.Context, rt TransportRuntime) error {
		sup := runtimeConnection(rt).sup.Load()
		sup.testHookBeforeEnqueue = func(gen uint64, ev fsmEvent, cause TransitionCause) (skipEnqueue bool) {
			mu.Lock()
			*pending = append(*pending, pendingReport{gen: gen, ev: ev, cause: cause})
			mu.Unlock()

			return true // withhold: the test replays these once the scenario is ready for them
		}

		rt.TCPUp(fakeConn{})
		if !rt.CommitSelected() {
			return errMockSelectCASDidNotLand
		}
		sup.testHookBeforeEnqueue = nil

		return startErr
	}
}

// assertDropOvertookUnreportedSelect is the shared assertion body for the Close and failed-Open rollback scenarios below:
// both drive the exact same schedule (TCP-up and Select land, neither is reported, then a local teardown overtakes both),
// so both must produce the same observable outcome.
//
// It does NOT replay the withheld reports:
// by the time either scenario below calls this,
// Close (or Open's rollback) has already joined run() (joinSupervisor's <-s.runDone),
// so injectFrom would only hit that closed channel's runDone case and never reach step() at all —
// a replay here would be vacuous, proving nothing about the closed latch itself.
// That latch is exercised directly, with step() actually invoked, at the supervisor level:
// TestSupervisor_CloseOvertakingUnreportedCommitsFiresWithPrevSelected and
// TestSupervisor_ClosedLatchBlocksALegalEventFromANonTerminalState.
func assertDropOvertookUnreportedSelect(t *testing.T, c *connection, mt *mockTransport, drops *[]LifecycleEvent, pending *[]pendingReport, mu *sync.Mutex) {
	t.Helper()

	require.Equal(t, NotConnectedState, c.State())
	require.Equal(t, 1, mt.startCalls(), "overtaking the unreported commits must not trigger a reconnect")
	require.Len(t, *drops, 1, "exactly one NotConnected event must be reported")
	require.Equal(t, SelectedState, (*drops)[0].Previous,
		"the reported drop must name the state the link actually reached, not an unreported intermediate one")
	require.Equal(t, CauseLocalClose, (*drops)[0].Cause)
	require.Equal(t, 1, mt.separateWrites(), "a teardown from a link that reached Selected attempts the courtesy farewell")

	mu.Lock()
	toReplay := append([]pendingReport(nil), *pending...)
	mu.Unlock()
	require.Len(t, toReplay, 2, "both commits' reports must have been withheld")
}

// TestClose_OvertakesWithheldSelectAndTCPUpReports drives a voluntary Close
// that overtakes both the TCP-up and Select commits before either report ever reaches the FSM queue:
// Close's own evClose is genuinely the first command the queue ever sees.
// It must still report the drop exactly once,
// naming Selected as the state the link actually reached, attempt the courtesy farewell, and start no reconnect.
func TestClose_OvertakesWithheldSelectAndTCPUpReports(t *testing.T) {
	var pending []pendingReport
	var mu sync.Mutex
	c, mt := newLifeConn(t, withMockTransportWithholdingReports(&pending, &mu, nil))

	var drops []LifecycleEvent
	cancel := c.SubscribeLifecycle(func(ev LifecycleEvent) {
		if ev.Current == NotConnectedState {
			drops = append(drops, ev)
		}
	})
	defer cancel()

	require.NoError(t, c.Open(t.Context(), OpenBackground))
	require.Equal(t, SelectedState, c.State(), "both commits must have landed synchronously before either was reported")

	require.NoError(t, c.Close())

	assertDropOvertookUnreportedSelect(t, c, mt, &drops, &pending, &mu)
}

// TestOpenRollback_OvertakesWithheldSelectAndTCPUpReports is the failed-Open half of the same schedule:
// the mock's Start commits TCP-up and Select synchronously —
// exactly as the Close case above —
// but then returns an error, driving Open's own rollback instead of a separate Close call.
// The rollback must produce the identical outcome:
// one drop reported with Previous Selected, the courtesy farewell attempted, and no reconnect.
func TestOpenRollback_OvertakesWithheldSelectAndTCPUpReports(t *testing.T) {
	startErr := errors.New("hsms_test: dial failed after TCP-up and Select landed")

	var pending []pendingReport
	var mu sync.Mutex
	c, mt := newLifeConn(t, withMockTransportWithholdingReports(&pending, &mu, startErr))

	var drops []LifecycleEvent
	cancel := c.SubscribeLifecycle(func(ev LifecycleEvent) {
		if ev.Current == NotConnectedState {
			drops = append(drops, ev)
		}
	})
	defer cancel()

	err := c.Open(t.Context(), OpenBackground)
	require.ErrorIs(t, err, startErr, "Open must return the tr.Start failure")

	assertDropOvertookUnreportedSelect(t, c, mt, &drops, &pending, &mu)
}
