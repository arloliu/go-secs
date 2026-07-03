package hsms

import (
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/logger"
	"github.com/arloliu/go-secs/v2/logger/loggertest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// newHandlerPtr builds a fresh Connection-style handler pointer wired with hs. With no
// handlers it stores nothing, so notifier's nil-guard is exercised.
func newHandlerPtr(hs ...StateChangeHandler) *atomic.Pointer[[]StateChangeHandler] {
	p := &atomic.Pointer[[]StateChangeHandler]{}
	if len(hs) > 0 {
		slice := slices.Clone(hs)
		p.Store(&slice)
	}

	return p
}

// newTestSupervisor builds a supervisor with a no-op react, an explicit events-queue capacity,
// and a local handlers pointer wired with hs (as the Connection would own it).
func newTestSupervisor(t *testing.T, eventsCap int, hs ...StateChangeHandler) *supervisor {
	t.Helper()

	return newSupervisorWithEventsCap(func(_, _ ConnState) {}, newHandlerPtr(hs...), eventsCap)
}

func TestTransition_E37Table(t *testing.T) {
	cases := []struct {
		cur  ConnState
		ev   fsmEvent
		next ConnState
		ok   bool
	}{
		{NotConnectedState, evTCPUp, NotSelectedState, true},
		{NotSelectedState, evTCPUp, NotSelectedState, true}, // tolerate CommitConnected's pre-commit
		{NotSelectedState, evSelectAccepted, SelectedState, true},
		{SelectedState, evSelectAccepted, SelectedState, true}, // H2: tolerate CommitSelected's pre-commit
		{SelectedState, evSelectLost, NotSelectedState, true},
		{SelectedState, evDisconnect, NotConnectedState, true},
		{NotSelectedState, evDisconnect, NotConnectedState, true},
		// evClose legal from ALL THREE states (Close must not hang during dial / passive-wait):
		{SelectedState, evClose, NotConnectedState, true},
		{NotSelectedState, evClose, NotConnectedState, true},
		{NotConnectedState, evClose, NotConnectedState, true},
		// NotConnected + evDisconnect is a safe no-op:
		{NotConnectedState, evDisconnect, NotConnectedState, false},
		// T7 NOT-SELECTED dwell (§9.2.2): legal ONLY from NotSelected -> NotConnected; a NO-OP
		// from Selected (the load-bearing guard — a validly-Selected session is never torn down
		// by a stale T7) and from NotConnected.
		{NotSelectedState, evT7Timeout, NotConnectedState, true},
		{SelectedState, evT7Timeout, SelectedState, false},
		{NotConnectedState, evT7Timeout, NotConnectedState, false},
		// illegal:
		{NotConnectedState, evSelectAccepted, NotConnectedState, false},
		{SelectedState, evTCPUp, SelectedState, false},
	}
	for _, c := range cases {
		next, ok := transition(c.cur, c.ev)
		require.Equal(t, c.ok, ok, "%v+%d ok", c.cur, c.ev)
		require.Equal(t, c.next, next, "%v+%d next", c.cur, c.ev)
	}
}

// F1 drop-OLDEST: the LATEST state always survives. Fill notify, then drive a terminal
// transition; the terminal (newest) must NOT be the one dropped — a later drain reads
// NotConnected as the surviving latest.
func TestSupervisor_LatestStateSurvivesDropOldestWhenNotifyFull(t *testing.T) {
	s := newSupervisorWithEventsCap(func(_, _ ConnState) {}, newHandlerPtr(), 8)
	for range cap(s.notify) {
		s.notify <- stateChange{prev: NotConnectedState, next: NotSelectedState} // fill with stale advisory
	}
	go s.run()
	defer s.stop()

	s.inject(evTCPUp)
	s.inject(evSelectAccepted)
	s.inject(evDisconnect) // terminal NotConnected — drop-oldest must keep THIS (the latest)

	require.Eventually(t, func() bool {
		var last *stateChange
		for {
			select {
			case sc := <-s.notify:
				last = &sc
			default:
				return last != nil && last.next == NotConnectedState && last.prev != NotConnectedState
			}
		}
	}, 2*time.Second, 10*time.Millisecond, "drop-oldest must preserve the LATEST (terminal NotConnected) state (F1)")

	require.Positive(t, s.droppedNotify.Load(), "coalescing should have counted drops")
}

// The supervisor must NEVER block on notify, so a concurrent Close's inject(evClose) always
// makes progress — even when notify is full AND the notifier is parked in a stalled user
// handler, AND across reconnect generations that emit further terminals. A small events buffer
// gives the test teeth: a supervisor that parks on a blocking notify send cannot drain events,
// so the guaranteed inject then blocks; a correct (non-blocking drop-oldest) supervisor drains
// forever and every inject completes.
func TestSupervisor_NeverBlocksEventsDrainEvenAcrossSecondTerminal(t *testing.T) {
	stuck := make(chan struct{})
	s := newTestSupervisor(t, 4, func(_, _ ConnState) { <-stuck })
	t.Cleanup(func() { close(stuck) })

	for range cap(s.notify) {
		s.notify <- stateChange{prev: NotConnectedState, next: NotSelectedState}
	}
	go s.run()
	defer s.stop()
	go s.notifier() // parks in the stuck handler after freeing one notify slot

	evs := []fsmEvent{
		evTCPUp, evSelectAccepted, evDisconnect, // first terminal
		evTCPUp, evSelectAccepted, evDisconnect, // second terminal (reconnect generation)
		evTCPUp, evSelectAccepted, evDisconnect, // third terminal
		evTCPUp, evSelectAccepted, evDisconnect, // fourth terminal
		evClose, // Close-like: must still make progress
	}
	done := make(chan struct{})
	go func() {
		for _, ev := range evs {
			s.inject(ev)
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor parked on a blocking notify send — inject deadlocked across terminals with a stalled notifier (round-5)")
	}
}

// events is a GUARANTEED command queue — inject must BLOCK on a full buffer, never drop. This
// is the mechanism that protects evSelectAccepted (and evClose). DETERMINISTIC: do not start
// run() (nothing drains), fill events to capacity, then prove the next inject blocks; then
// drain via run() and prove the blocked inject completes (nothing lost).
func TestSupervisor_InjectIsGuaranteedNotDropping(t *testing.T) {
	s := newSupervisorWithEventsCap(func(_, _ ConnState) {}, newHandlerPtr(), 2)
	// run() NOT started yet -> no drain.
	s.inject(evTCPUp)
	s.inject(evTCPUp) // events buffer now full (cap 2)

	blocked := make(chan struct{})
	go func() {
		s.inject(evTCPUp) // 3rd inject MUST block (guaranteed), not drop+return
		close(blocked)
	}()

	select {
	case <-blocked:
		t.Fatal("inject returned on a FULL events buffer — it must block (guaranteed command queue), not drop")
	case <-time.After(200 * time.Millisecond):
		// good: still blocked
	}

	go s.run() // drains; the blocked inject now completes — its command was not lost
	defer s.stop()

	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("blocked inject never completed after drain")
	}
}

func TestSupervisor_CommitConnectedIsSynchronousAndIdempotent(t *testing.T) {
	s := newSupervisor(func(_, _ ConnState) {}, newHandlerPtr())
	go s.run()
	defer s.stop()

	// A fresh supervisor starts at NotConnected.
	require.Equal(t, NotConnectedState, s.State())

	require.True(t, s.CommitConnected(), "first commit performs the CAS")
	require.Equal(t, NotSelectedState, s.State(), "state is NotSelected SYNCHRONOUSLY, before the run loop drains")
	require.False(t, s.CommitConnected(), "second commit is a no-op (already NotSelected)")
}

// CommitConnected pre-stores NotSelected, THEN enqueues evTCPUp. The supervisor must STILL fire the
// entering-NotSelected reaction/notify EXACTLY ONCE as (NotConnected -> NotSelected) — never drop
// it, never duplicate it, and a benign extra evTCPUp (now legal from NotSelected) must add nothing.
func TestSupervisor_CommitConnectedFiresReactionExactlyOnce(t *testing.T) {
	var mu sync.Mutex
	var reactions [][2]ConnState
	countEnter := func() int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, r := range reactions {
			if r == [2]ConnState{NotConnectedState, NotSelectedState} {
				n++
			}
		}

		return n
	}

	s := newSupervisor(func(prev, next ConnState) {
		mu.Lock()
		reactions = append(reactions, [2]ConnState{prev, next})
		mu.Unlock()
	}, newHandlerPtr())
	go s.run()
	defer s.stop()

	notifs := make(chan stateChange, 8)
	go func() {
		for sc := range s.notify {
			notifs <- sc
		}
	}()

	require.True(t, s.CommitConnected()) // pre-commits state=NotSelected AND enqueues evTCPUp
	require.Equal(t, NotSelectedState, s.State())

	// The entering-NotSelected reaction must arrive exactly once, as NotConnected -> NotSelected.
	require.Eventually(t, func() bool { return countEnter() == 1 },
		time.Second, time.Millisecond,
		"entering-NotSelected reaction must fire EXACTLY once as NotConnected->NotSelected (not dropped, not duplicated)")

	// A second CommitConnected (already NotSelected) is a no-op and produces no additional reaction.
	require.False(t, s.CommitConnected(), "second commit is a no-op (already NotSelected)")

	// evTCPUp is legal from NotSelected (tolerates the pre-commit): injecting/processing another one
	// is a benign no-op — no duplicate reaction. Assert the count stays 1 over a bounded window.
	s.inject(evTCPUp)
	require.Never(t, func() bool { return countEnter() != 1 },
		200*time.Millisecond, 10*time.Millisecond,
		"a benign extra evTCPUp from NotSelected must not fire a duplicate reaction")

	// And the first notification is exactly NotConnected -> NotSelected (never prev==next).
	select {
	case sc := <-notifs:
		require.Equal(t, NotConnectedState, sc.prev)
		require.Equal(t, NotSelectedState, sc.next)
	case <-time.After(200 * time.Millisecond):
		t.Fatal("expected a NotConnected->NotSelected notification")
	}
}

func TestSupervisor_CommitSelectedIsSynchronousAndIdempotent(t *testing.T) {
	s := newSupervisor(func(_, _ ConnState) {}, newHandlerPtr())
	go s.run()
	defer s.stop()

	// Reach NotSelected first.
	s.inject(evTCPUp)
	require.Eventually(t, func() bool { return s.State() == NotSelectedState }, time.Second, time.Millisecond)

	require.True(t, s.CommitSelected(), "first commit performs the CAS")
	require.Equal(t, SelectedState, s.State(), "state is Selected SYNCHRONOUSLY, before any rsp write")
	require.False(t, s.CommitSelected(), "second commit is a no-op (already Selected)")
}

// TestSupervisor_CommitSelectLostIsSynchronous proves I3: SelectLost commits Selected->NotSelected
// SYNCHRONOUSLY (a guarded CAS), so a re-Select pipelined right after a Deselect finds NotSelected
// and its CommitSelected CAS succeeds — instead of the old async inject that left state Selected and
// let a re-Select be answered "success" without a real commit. run() is not started; every commit is
// a synchronous CAS, asserted immediately.
func TestSupervisor_CommitSelectLostIsSynchronous(t *testing.T) {
	s := newTestSupervisor(t, 8)

	require.True(t, s.CommitConnected()) // NotConnected -> NotSelected (sync CAS)
	require.True(t, s.CommitSelected())  // NotSelected -> Selected (sync CAS)
	require.Equal(t, SelectedState, s.State())

	require.True(t, s.CommitSelectLost(), "SelectLost performs the CAS Selected->NotSelected")
	require.Equal(t, NotSelectedState, s.State(), "state is NotSelected SYNCHRONOUSLY, before run() drains")
	require.False(t, s.CommitSelectLost(), "second is a no-op (not Selected)")

	// The crux: a pipelined re-Select now commits, because state is already NotSelected.
	require.True(t, s.CommitSelected(), "re-Select after synchronous SelectLost commits")
	require.Equal(t, SelectedState, s.State())
}

// TestSupervisor_ClosedLatchIgnoresLateEvents proves the I2 latch: once step() processes evClose the
// supervisor is latched closed, so a late evTCPUp (queued behind evClose in the Close-vs-reconnect-
// Start race, where NotConnected->NotSelected is a legal table entry) cannot resurrect NotSelected
// after Close. run() is deliberately NOT started so step() is driven in the exact order the race
// produces. Without the latch, the final evTCPUp re-stores NotSelected — the I2 defect.
func TestSupervisor_ClosedLatchIgnoresLateEvents(t *testing.T) {
	s := newTestSupervisor(t, 8)

	require.True(t, s.CommitConnected()) // a generation came up: TCP-up pre-committed NotSelected
	require.Equal(t, NotSelectedState, s.State())

	s.step(evClose) // Close: NotSelected -> NotConnected, and LATCH closed
	require.Equal(t, NotConnectedState, s.State())

	s.step(evTCPUp) // the evTCPUp queued behind evClose MUST be ignored now
	require.Equal(t, NotConnectedState, s.State(),
		"latched: a late evTCPUp must not resurrect NotSelected after Close")
}

// TestSupervisor_StaleSelectLostAbandonedAfterReCommit proves NEW-2: an evSelectLost that a pipelined
// re-Select superseded (a CommitSelected re-committed after CommitSelectLost's CAS) is ABANDONED by
// step, leaving the re-committed Selected intact rather than flapping it to NotSelected (which would
// spuriously Reject the peer's next frame — the efb220b class). run() is not started; step is driven
// directly in the exact order the pipeline produces.
func TestSupervisor_StaleSelectLostAbandonedAfterReCommit(t *testing.T) {
	s := newTestSupervisor(t, 8)

	require.True(t, s.CommitConnected())  // -> NotSelected
	require.True(t, s.CommitSelected())   // -> Selected
	require.True(t, s.CommitSelectLost()) // -> NotSelected (Deselect); enqueues evSelectLost
	require.Equal(t, NotSelectedState, s.State())

	// A pipelined re-Select commits BEFORE the supervisor processes the queued evSelectLost.
	require.True(t, s.CommitSelected()) // -> Selected again
	require.Equal(t, SelectedState, s.State())

	// The stale evSelectLost is now processed: it must be abandoned (state stays Selected).
	s.step(evSelectLost)
	require.Equal(t, SelectedState, s.State(),
		"a superseded evSelectLost must be abandoned, not flap the re-committed Selected")
}

// CommitSelected pre-stores Selected, THEN enqueues evSelectAccepted. The supervisor must
// STILL fire the entering-Selected reaction/notify EXACTLY ONCE as (NotSelected -> Selected) —
// never drop it (the bug), never report prev==next.
func TestSupervisor_PreCommittedSelectFiresReactionExactlyOnce(t *testing.T) {
	var mu sync.Mutex
	var reactions [][2]ConnState
	s := newSupervisor(func(prev, next ConnState) {
		mu.Lock()
		reactions = append(reactions, [2]ConnState{prev, next})
		mu.Unlock()
	}, newHandlerPtr())
	go s.run()
	defer s.stop()

	notifs := make(chan stateChange, 8)
	go func() {
		for sc := range s.notify {
			notifs <- sc
		}
	}()

	s.inject(evTCPUp)
	require.Eventually(t, func() bool { return s.State() == NotSelectedState }, time.Second, time.Millisecond)

	require.True(t, s.CommitSelected()) // pre-commits state=Selected AND enqueues evSelectAccepted

	// The reaction for entering Selected must arrive exactly once, as NotSelected -> Selected.
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, r := range reactions {
			if r == [2]ConnState{NotSelectedState, SelectedState} {
				n++
			}
		}

		return n == 1
	}, time.Second, time.Millisecond, "entering-Selected reaction must fire EXACTLY once as NotSelected->Selected (not dropped, not duplicated)")

	// And the first notification never has prev==next.
	select {
	case sc := <-notifs:
		require.NotEqual(t, sc.prev, sc.next, "no prev==next notification (idempotent-async property)")
	case <-time.After(200 * time.Millisecond):
		t.Fatal("expected a NotSelected->Selected notification")
	}
}

func TestSupervisor_NotifierIsPanicIsolated(t *testing.T) {
	var ran2 atomic.Bool
	s := newTestSupervisor(t, 8,
		func(_, _ ConnState) { panic("handler 1 panics") },
		func(_, _ ConnState) { ran2.Store(true) }, // handler 2 must still run
	)
	go s.notifier()
	defer close(s.notify) // end the notifier range (run() is not started in this test)

	s.notify <- stateChange{prev: NotConnectedState, next: NotSelectedState} // both attempted, no crash
	require.Eventually(t, ran2.Load, time.Second, time.Millisecond, "panic in handler 1 must not stop handler 2 (H4)")
}

// terminal NotConnected must reach the user's StateChangeHandler through the notifier (not
// merely be readable on the channel). Drive a full lifecycle to NotConnected and assert a
// registered handler observes it.
func TestSupervisor_TerminalReachesUserHandler(t *testing.T) {
	seen := make(chan stateChange, 8)
	s := newTestSupervisor(t, 8, func(prev, next ConnState) { seen <- stateChange{prev: prev, next: next} })
	go s.run()
	defer s.stop()
	go s.notifier()

	s.inject(evTCPUp)
	s.inject(evSelectAccepted)
	s.inject(evDisconnect) // -> terminal NotConnected

	require.Eventually(t, func() bool {
		for {
			select {
			case sc := <-seen:
				if sc.next == NotConnectedState && sc.prev != NotConnectedState {
					return true
				}
			default:
				return false
			}
		}
	}, 2*time.Second, 10*time.Millisecond, "terminal NotConnected must be delivered to the user handler")
}

// requestClose pins the exact epoch and initiates its teardown from the supervisor's evClose
// handler even when no transition fires (fresh supervisor is already NotConnected), so Close's
// e.wait() cannot hang.
func TestSupervisor_RequestClosePinsAndTearsDownEpoch(t *testing.T) {
	e := newEpoch(t.Context(), logger.Default(), 8)
	s := newTestSupervisor(t, 8)
	go s.run()
	defer s.stop()

	s.requestClose(e) // pins e, injects evClose -> supervisor initiates teardown of the pinned epoch
	require.NoError(t, e.wait(), "requestClose must initiate teardown of the pinned epoch (e.wait returns)")
	require.Equal(t, e, s.closeEpoch.Load(), "requestClose pins the exact epoch")
}

// TestSupervisor_T7TimeoutFromNotSelectedDisconnects drives the supervisor to NotSelected, then
// injects evT7Timeout (the T7 NOT-SELECTED dwell expiry, §9.2.2). The FSM must advance NotSelected
// -> NotConnected and fire the entering-NotConnected reaction exactly once as
// (NotSelected -> NotConnected).
func TestSupervisor_T7TimeoutFromNotSelectedDisconnects(t *testing.T) {
	var mu sync.Mutex
	var reactions [][2]ConnState
	s := newSupervisor(func(prev, next ConnState) {
		mu.Lock()
		reactions = append(reactions, [2]ConnState{prev, next})
		mu.Unlock()
	}, newHandlerPtr())
	go s.run()
	defer s.stop()

	// Reach NotSelected first (TCP up, not yet selected — the T7 dwell window).
	s.inject(evTCPUp)
	require.Eventually(t, func() bool { return s.State() == NotSelectedState }, time.Second, time.Millisecond)

	// T7 expires while still NotSelected: disconnect + reconnect (here just the reaction fires).
	s.inject(evT7Timeout)

	require.Eventually(t, func() bool { return s.State() == NotConnectedState }, time.Second, time.Millisecond,
		"evT7Timeout from NotSelected must drive NotConnected (§9.2.2)")

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, r := range reactions {
			if r == [2]ConnState{NotSelectedState, NotConnectedState} {
				n++
			}
		}

		return n == 1
	}, time.Second, time.Millisecond,
		"entering-NotConnected reaction must fire exactly once as NotSelected->NotConnected")
}

// TestSupervisor_T7TimeoutFromSelectedIsNoOp is the LOAD-BEARING guard test (§9.2.2 / PART E1
// teeth-check target): a session that reached Selected before T7 expiry must NEVER be torn down by
// a stale evT7Timeout. Driving to Selected and injecting evT7Timeout must leave the state Selected
// and fire NO reaction.
//
// Teeth-check (performed once during implementation, then reverted): widening the transition clause
// to also fire from Selected (`cur == NotSelectedState || cur == SelectedState`) makes this test
// fail — the state flips to NotConnected — confirming the no-op-from-Selected clause is what holds
// the invariant.
func TestSupervisor_T7TimeoutFromSelectedIsNoOp(t *testing.T) {
	var reactionCount atomic.Int64
	s := newSupervisor(func(_, next ConnState) {
		if next == NotConnectedState {
			reactionCount.Add(1)
		}
	}, newHandlerPtr())
	go s.run()
	defer s.stop()

	// Drive all the way to Selected.
	s.inject(evTCPUp)
	s.inject(evSelectAccepted)
	require.Eventually(t, func() bool { return s.State() == SelectedState }, time.Second, time.Millisecond)

	// A stale T7 fires: it MUST be a no-op from Selected — no state change, no NotConnected reaction.
	s.inject(evT7Timeout)

	require.Never(t, func() bool { return s.State() != SelectedState },
		200*time.Millisecond, 10*time.Millisecond,
		"a validly-Selected session must NEVER be torn down by a stale T7 (§9.2.2)")
	require.Zero(t, reactionCount.Load(), "evT7Timeout from Selected must fire no entering-NotConnected reaction")
}

// TestSupervisor_T7TimeoutLosesTieToCommitSelected is the T24b guard test: it deterministically
// forces the nanosecond tie the reviewer described — a CommitSelected CAS that commits AFTER step()
// reads NotSelected but BEFORE step() stores NotConnected — and asserts the guarded CAS abandons the
// stale T7 disconnect so the validly-committed Selected session survives (§9.2.2). Using the
// testHookAfterStateLoad seam makes the interleaving deterministic; step() is driven synchronously.
//
// Teeth-check (performed once during implementation, then reverted): reverting the evT7Timeout store
// in step() to a plain s.state.Store(uint32(next)) makes this test fail — the store clobbers the
// committed Selected and the state becomes NotConnected — confirming the CAS guard is what holds the
// "never torn down by a stale T7" invariant.
func TestSupervisor_T7TimeoutLosesTieToCommitSelected(t *testing.T) {
	s := newSupervisor(func(_, _ ConnState) {}, newHandlerPtr())
	s.state.Store(uint32(NotSelectedState))
	s.lastReacted = NotSelectedState

	// Interpose a CommitSelected between step()'s state.Load() and its evT7Timeout CAS: this is the
	// exact tie the reviewer described (Select commits after we read NotSelected, before we store).
	s.testHookAfterStateLoad = func(ev fsmEvent) {
		if ev == evT7Timeout {
			s.testHookAfterStateLoad = nil // one-shot
			// mimic CommitSelected's synchronous CAS on the recv goroutine
			s.state.CompareAndSwap(uint32(NotSelectedState), uint32(SelectedState))
		}
	}

	s.step(evT7Timeout)

	// The guarded CAS must have FAILED (state moved to Selected), so the stale T7 disconnect is
	// abandoned and the validly-committed Selected session survives.
	require.Equal(t, SelectedState, s.State())
}

// TestSupervisor_ReportDropsSurfacesWarnEdgeTriggered is the M4 teeth: a coalesced (drop-oldest)
// notification is surfaced via a Warn (from the notifier goroutine's reportDrops, NOT emit — P1-B:
// logging must never run on the FSM run() goroutine where a blocking logger could stall Close). It
// is edge-triggered on droppedNotify: a stall-burst logs once with the running total, and no
// further drops mean no further logs. Teeth: dropping the reportDrops call (or the log) fails the
// AssertNumberOfCalls; a level-triggered (re-log-every-call) impl fails the "no new drops" check.
func TestSupervisor_ReportDropsSurfacesWarnEdgeTriggered(t *testing.T) {
	s := newSupervisorWithEventsCap(func(_, _ ConnState) {}, newHandlerPtr(), 8)

	mockLog := loggertest.NewMockLogger()
	mockLog.On("Warn", mock.Anything, mock.Anything).Return()
	s.logger = mockLog

	// emit() must be PURE non-blocking: it counts drops but never touches the logger.
	for range cap(s.notify) {
		s.notify <- stateChange{prev: NotConnectedState, next: NotSelectedState} // fill so emit drops
	}
	s.emit(stateChange{prev: NotSelectedState, next: SelectedState})
	require.Equal(t, uint64(1), s.droppedNotify.Load())
	mockLog.AssertNumberOfCalls(t, "Warn", 0) // emit itself never logs (P1-B)

	// The notifier's reportDrops surfaces the coalesced drop(s).
	s.reportDrops()
	mockLog.AssertNumberOfCalls(t, "Warn", 1)

	// No new drops → edge-triggered → no additional log.
	s.reportDrops()
	mockLog.AssertNumberOfCalls(t, "Warn", 1)

	// A fresh stall-burst (more drops) → one more log with the new running total.
	s.droppedNotify.Store(9)
	s.reportDrops()
	mockLog.AssertNumberOfCalls(t, "Warn", 2)
}

// TestSupervisor_ResolveCloseTimeoutIsLive is the M7 teeth: the evClose teardown reads the
// closeTimeout provider LIVE (so a mid-session UpdateConfigOptions(WithCloseTimeout) is honored),
// and falls back to supervisorFallbackCloseTimeout when no provider is installed. Teeth: caching
// the value instead of calling the provider makes the "live update reflected" assertion fail.
func TestSupervisor_ResolveCloseTimeoutIsLive(t *testing.T) {
	s := newSupervisor(func(_, _ ConnState) {}, newHandlerPtr())

	require.Equal(t, supervisorFallbackCloseTimeout, s.resolveCloseTimeout(), "nil provider → fallback")

	var live atomic.Int64
	live.Store(int64(3 * time.Second))
	s.closeTimeout = func() time.Duration { return time.Duration(live.Load()) }
	require.Equal(t, 3*time.Second, s.resolveCloseTimeout())

	live.Store(int64(7 * time.Second)) // a mid-session config change
	require.Equal(t, 7*time.Second, s.resolveCloseTimeout(), "provider must be read live, not cached")
}
