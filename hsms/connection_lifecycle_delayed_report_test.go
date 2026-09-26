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

	// The delayed TCP-up report is stamped with the generation its gate validated
	// (still N's, since N is still current when its CAS lands).
	// Holding the successor's PUBLICATION (not merely its own dial) until the same release this test already uses forces the schedule this guard is meant to exercise:
	// N still current when the stamped report is admitted by the generation match, dying on the illegal NotConnected+evTCPUp transition below —
	// rather than the generation match discarding it before it ever reaches the transition table,
	// which would leave that guard unexercised and make a passing run prove nothing about it.
	var holdPublishOnce sync.Once
	c.testHookConnectLoop = func() {
		holdPublishOnce.Do(func() { <-release })
	}

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
	require.Equal(t, uint64(0), sup.staleGen.Load(),
		"the delayed report must be admitted by the generation match (N still current) and die on the illegal transition below, not be discarded as stale")

	releaseOnce.Do(func() { close(release) }) // the delayed report is settled — the successor may now publish and dial

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

// TestReconnect_WithheldSelectAcceptedReportCannotPromoteSuccessor proves the pre-enqueue seam end to end.
// A Select-accepted commit's own report is withheld right at commitFrom's pre-enqueue hook,
// then replayed only once a successor generation has published and brought itself up to NotSelected on its own TCP-up.
// It must not promote that successor to Selected without its own handshake.
//
// The predecessor's own TCP-up follow-up is withheld and discarded too, never replayed.
// This scenario only cares about its state, which the CAS already settles synchronously.
// Discarding it keeps evTCPUp — reused below as a same-state, no-op sentinel from either NotSelected or Selected —
// from ever appearing on the queue for a reason other than the successor's own genuine report or the deliberate sentinel this test injects after the replay.
func TestReconnect_WithheldSelectAcceptedReportCannotPromoteSuccessor(t *testing.T) {
	var startCalls atomic.Int32
	gen2Started := make(chan struct{})
	script := mockScript(func(_ context.Context, _ TransportRuntime) error {
		if startCalls.Add(1) == 2 {
			close(gen2Started)
		}

		return nil
	})

	c, _ := newLifeConn(t, script)
	require.NoError(t, c.UpdateConfigOptions(WithReconnectBackoff(5*time.Millisecond, 1.0)))

	require.NoError(t, c.Open(t.Context(), OpenBackground))
	require.Equal(t, int32(1), startCalls.Load())

	// Installed right after Open returns and before any commit this test drives —
	// call 1's own script does nothing, so nothing can reach either hook before this point.
	sup := c.sup.Load()

	// Counts every evTCPUp step() begins processing:
	// the first is the successor's own genuine report, the second is this test's own sentinel injected after the replay below.
	// Neither changes state (evTCPUp is a same-state no-op from NotSelected or Selected alike),
	// so counting occurrences — rather than trying to tell them apart some other way —
	// is enough to recognize the second one without racing whichever goroutine the scheduler happens to run first.
	var evTCPUpCount atomic.Int32
	replaySeen := make(chan struct{})
	sup.testHookAfterStateLoad = func(ev fsmEvent) {
		if ev == evTCPUp && evTCPUpCount.Add(1) == 2 {
			close(replaySeen)
		}
	}

	// The hook below runs on this same test goroutine — commitFrom reads it from whichever goroutine
	// calls c.TCPUp/c.CommitSelected, both called directly below — so pending needs no synchronization of its own.
	var pending pendingReport
	sup.testHookBeforeEnqueue = func(gen uint64, ev fsmEvent, cause TransitionCause) (skipEnqueue bool) {
		if ev == evTCPUp { //nolint:staticcheck // if/else, not an exhaustive switch: only these two events are ever reachable here.
			return true // N's own TCP-up follow-up: its state is already settled by the CAS; discard it
		} else if ev == evSelectAccepted {
			pending = pendingReport{gen: gen, ev: ev, cause: cause}
			sup.testHookBeforeEnqueue = nil // one-shot: never intercepts the successor's own reports

			return true // withhold: the test replays it later, once the successor has published
		}

		return false
	}

	c.TCPUp(fakeConn{})
	require.True(t, c.CommitSelected(), "N's own Select CAS must land synchronously")
	require.Equal(t, SelectedState, c.State())

	captured := pending
	require.Equal(t, evSelectAccepted, captured.ev, "the withheld report must be captured before the drop")

	genN := c.cur.Load()
	require.Equal(t, genN.id, captured.gen, "the withheld report must carry the generation the gate validated, N's own")

	c.TCPDown(io.EOF) // drop N; the entering-NotConnected reaction starts the reconnect loop

	waitBounded(t, gen2Started, "the reconnect must publish and start the successor generation")

	genN1 := c.cur.Load()
	require.NotNil(t, genN1)
	require.NotEqual(t, captured.gen, genN1.id, "the successor must be a different generation from the one that captured the report")

	c.TCPUp(fakeConn{}) // the successor's own TCP-up; state -> NotSelected

	sup.injectFrom(captured.gen, captured.ev, captured.cause) // replay N's withheld report, with its captured generation
	sup.inject(evTCPUp, CauseUnknown)                         // FIFO sentinel: a same-state no-op from either NotSelected or Selected
	waitBounded(t, replaySeen, "the sentinel must be stepped, proving the replayed report was processed first")

	require.Equal(t, NotSelectedState, c.State(),
		"a Select-accepted report withheld from a predecessor generation must not promote its successor without its own handshake")
	require.Equal(t, uint64(1), sup.staleGen.Load(), "the replayed report must be discarded at the generation match, not merely absorbed")

	require.True(t, c.CommitSelected(), "the successor's own Select CAS must still land")
	require.True(t, genN1.reachedSelected.Load(), "the successor's own commit must latch its reconnect-backoff marker")

	require.NoError(t, c.Close())
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
