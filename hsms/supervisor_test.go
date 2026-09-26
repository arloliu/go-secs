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

// newTestSupervisor builds a supervisor with a no-op react, an 8-slot events queue,
// and a local handlers pointer wired with hs (as the Connection would own it).
func newTestSupervisor(t *testing.T, hs ...StateChangeHandler) *supervisor {
	t.Helper()

	return newSupervisorWithEventsCap(func(_, _ ConnState) {}, newHandlerPtr(hs...), nil, 8)
}

func TestTransition_E37Table(t *testing.T) {
	cases := []struct {
		cur  ConnState
		ev   fsmEvent
		next ConnState
		ok   bool
	}{
		{NotSelectedState, evTCPUp, NotSelectedState, true}, // tolerate CommitConnected's pre-commit
		{SelectedState, evTCPUp, SelectedState, true},       // tolerate a pre-committed Select racing ahead of its own evTCPUp (coalesced report — see step)
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
		// NotConnected + evTCPUp is a safe no-op: production only enqueues evTCPUp after its own
		// NotConnected->NotSelected CAS succeeds, so finding NotConnected at processing time means
		// a disconnect overtook it — treating it as legal would resurrect a dead generation.
		{NotConnectedState, evTCPUp, NotConnectedState, false},
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
	s := newSupervisorWithEventsCap(func(_, _ ConnState) {}, newHandlerPtr(), nil, 8)
	for range cap(s.notify) {
		s.notify <- stateChange{prev: NotConnectedState, next: NotSelectedState} // fill with stale advisory
	}
	go s.run()
	defer s.stop()

	s.CommitConnected(CauseUnknown) // NotConnected -> NotSelected; enqueues evTCPUp
	s.inject(evSelectAccepted, CauseUnknown)
	s.inject(evDisconnect, CauseUnknown) // terminal NotConnected — drop-oldest must keep THIS (the latest)

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

// The supervisor must NEVER block on notify,
// so a concurrent Close's inject(evClose) always makes progress —
// even when notify is full AND the notifier is parked in a stalled user handler, AND across reconnect generations that emit further terminals.
// The teeth come from the per-generation terminal acknowledgment (the terminals channel) timing out,
// not from inject blocking on a full events queue:
// a terminal's own notify send runs BEFORE its react call (fireTransition's terminal ordering),
// so a supervisor that blocked on a full notify would never reach react's terminals<- send at all,
// and the wait below times out.
//
// Each generation's bring-up goes through CommitConnected (its CAS requires state==NotConnected),
// so the injecting goroutine acknowledges the previous generation's entering-NotConnected reaction
// — via a dedicated react callback, delivered on the run goroutine and so never blocked by the
// stuck StateChangeHandler below —
// before starting the next one;
// otherwise a disconnect still in flight could make the next CommitConnected's CAS a silent no-op.
func TestSupervisor_NeverBlocksEventsDrainEvenAcrossSecondTerminal(t *testing.T) {
	stuck := make(chan struct{})
	terminals := make(chan struct{}, 8)
	react := func(_, next ConnState) {
		if next == NotConnectedState {
			terminals <- struct{}{}
		}
	}
	s := newSupervisorWithEventsCap(react, newHandlerPtr(func(_, _ ConnState) { <-stuck }), nil, 4)
	t.Cleanup(func() { close(stuck) })

	for range cap(s.notify) {
		s.notify <- stateChange{prev: NotConnectedState, next: NotSelectedState}
	}
	go s.run()
	defer s.stop()
	go s.notifier() // parks in the stuck handler after freeing one notify slot

	done := make(chan struct{})
	go func() {
		for range 4 { // four reconnect generations, each emitting its own terminal
			s.CommitConnected(CauseUnknown)
			s.inject(evSelectAccepted, CauseUnknown)
			s.inject(evDisconnect, CauseUnknown)

			select {
			case <-terminals:
			case <-time.After(2 * time.Second):
				return // the outer select below reports the hang; do not close done on a stall
			}
		}
		s.inject(evClose, CauseUnknown) // Close-like: must still make progress
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor parked on a blocking notify send — inject deadlocked across terminals with a stalled notifier")
	}
}

// events is a GUARANTEED command queue — inject must BLOCK on a full buffer, never drop. This
// is the mechanism that protects evSelectAccepted (and evClose). DETERMINISTIC: do not start
// run() (nothing drains), fill events to capacity, then prove the next inject blocks; then
// drain via run() and prove the blocked inject completes (nothing lost).
func TestSupervisor_InjectIsGuaranteedNotDropping(t *testing.T) {
	s := newSupervisorWithEventsCap(func(_, _ ConnState) {}, newHandlerPtr(), nil, 2)
	// run() NOT started yet -> no drain.
	s.inject(evTCPUp, CauseUnknown)
	s.inject(evTCPUp, CauseUnknown) // events buffer now full (cap 2)

	blocked := make(chan struct{})
	go func() {
		s.inject(evTCPUp, CauseUnknown) // 3rd inject MUST block (guaranteed), not drop+return
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
	s := newSupervisor(func(_, _ ConnState) {}, newHandlerPtr(), nil)
	go s.run()
	defer s.stop()

	// A fresh supervisor starts at NotConnected.
	require.Equal(t, NotConnectedState, s.State())

	require.True(t, s.CommitConnected(CauseUnknown), "first commit performs the CAS")
	require.Equal(t, NotSelectedState, s.State(), "state is NotSelected SYNCHRONOUSLY, before the run loop drains")
	require.False(t, s.CommitConnected(CauseUnknown), "second commit is a no-op (already NotSelected)")
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
	}, newHandlerPtr(), nil)
	go s.run()
	defer s.stop()

	notifs := make(chan stateChange, 8)
	go func() {
		for sc := range s.notify {
			notifs <- sc
		}
	}()

	require.True(t, s.CommitConnected(CauseUnknown)) // pre-commits state=NotSelected AND enqueues evTCPUp
	require.Equal(t, NotSelectedState, s.State())

	// The entering-NotSelected reaction must arrive exactly once, as NotConnected -> NotSelected.
	require.Eventually(t, func() bool { return countEnter() == 1 },
		time.Second, time.Millisecond,
		"entering-NotSelected reaction must fire EXACTLY once as NotConnected->NotSelected (not dropped, not duplicated)")

	// A second CommitConnected (already NotSelected) is a no-op and produces no additional reaction.
	require.False(t, s.CommitConnected(CauseUnknown), "second commit is a no-op (already NotSelected)")

	// evTCPUp is legal from NotSelected (tolerates the pre-commit): injecting/processing another one
	// is a benign no-op — no duplicate reaction. Assert the count stays 1 over a bounded window.
	s.inject(evTCPUp, CauseUnknown)
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
	s := newSupervisor(func(_, _ ConnState) {}, newHandlerPtr(), nil)
	go s.run()
	defer s.stop()

	// Reach NotSelected first (a synchronous CAS — no wait needed).
	s.CommitConnected(CauseUnknown)
	require.Equal(t, NotSelectedState, s.State())

	require.True(t, s.CommitSelected(CauseUnknown), "first commit performs the CAS")
	require.Equal(t, SelectedState, s.State(), "state is Selected SYNCHRONOUSLY, before any rsp write")
	require.False(t, s.CommitSelected(CauseUnknown), "second commit is a no-op (already Selected)")
}

// TestSupervisor_CommitSelectLostIsSynchronous proves I3: SelectLost commits Selected->NotSelected
// SYNCHRONOUSLY (a guarded CAS), so a re-Select pipelined right after a Deselect finds NotSelected
// and its CommitSelected CAS succeeds — instead of the old async inject that left state Selected and
// let a re-Select be answered "success" without a real commit. run() is not started; every commit is
// a synchronous CAS, asserted immediately.
func TestSupervisor_CommitSelectLostIsSynchronous(t *testing.T) {
	s := newTestSupervisor(t)

	require.True(t, s.CommitConnected(CauseUnknown)) // NotConnected -> NotSelected (sync CAS)
	require.True(t, s.CommitSelected(CauseUnknown))  // NotSelected -> Selected (sync CAS)
	require.Equal(t, SelectedState, s.State())

	require.True(t, s.CommitSelectLost(CauseUnknown), "SelectLost performs the CAS Selected->NotSelected")
	require.Equal(t, NotSelectedState, s.State(), "state is NotSelected SYNCHRONOUSLY, before run() drains")
	require.False(t, s.CommitSelectLost(CauseUnknown), "second is a no-op (not Selected)")

	// The crux: a pipelined re-Select now commits, because state is already NotSelected.
	require.True(t, s.CommitSelected(CauseUnknown), "re-Select after synchronous SelectLost commits")
	require.Equal(t, SelectedState, s.State())
}

// TestSupervisor_ClosedLatchIgnoresLateEvents proves a late evTCPUp
// (queued behind evClose in the Close-vs-reconnect-Start race)
// cannot resurrect NotSelected after Close.
// NotConnected + evTCPUp is illegal at the table level on its own (see transition),
// so this specific resurrection does not isolate the closed latch by itself —
// see TestSupervisor_ClosedLatchBlocksALegalEventFromANonTerminalState for that isolation,
// which drives a still-legal queued event against a non-terminal stored state instead.
// run() is deliberately NOT started so step() is driven in the exact order the race produces.
func TestSupervisor_ClosedLatchIgnoresLateEvents(t *testing.T) {
	s := newTestSupervisor(t)

	require.True(t, s.CommitConnected(CauseUnknown)) // a generation came up: TCP-up pre-committed NotSelected
	require.Equal(t, NotSelectedState, s.State())

	s.step(fsmCommand{ev: evClose}) // Close: NotSelected -> NotConnected, and LATCH closed
	require.Equal(t, NotConnectedState, s.State())

	s.step(fsmCommand{ev: evTCPUp}) // the evTCPUp queued behind evClose MUST be ignored now
	require.Equal(t, NotConnectedState, s.State(),
		"latched: a late evTCPUp must not resurrect NotSelected after Close")
}

// TestSupervisor_StaleSelectLostAbandonedAfterReCommit proves NEW-2: an evSelectLost that a pipelined
// re-Select superseded (a CommitSelected re-committed after CommitSelectLost's CAS) is ABANDONED by
// step, leaving the re-committed Selected intact rather than flapping it to NotSelected (which would
// spuriously Reject the peer's next frame — the efb220b class). run() is not started; step is driven
// directly in the exact order the pipeline produces.
func TestSupervisor_StaleSelectLostAbandonedAfterReCommit(t *testing.T) {
	s := newTestSupervisor(t)

	require.True(t, s.CommitConnected(CauseUnknown))  // -> NotSelected
	require.True(t, s.CommitSelected(CauseUnknown))   // -> Selected
	require.True(t, s.CommitSelectLost(CauseUnknown)) // -> NotSelected (Deselect); enqueues evSelectLost
	require.Equal(t, NotSelectedState, s.State())

	// A pipelined re-Select commits BEFORE the supervisor processes the queued evSelectLost.
	require.True(t, s.CommitSelected(CauseUnknown)) // -> Selected again
	require.Equal(t, SelectedState, s.State())

	// The stale evSelectLost is now processed: it must be abandoned (state stays Selected).
	s.step(fsmCommand{ev: evSelectLost})
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
	}, newHandlerPtr(), nil)
	go s.run()
	defer s.stop()

	notifs := make(chan stateChange, 8)
	go func() {
		for sc := range s.notify {
			notifs <- sc
		}
	}()

	s.CommitConnected(CauseUnknown) // pre-commits state=NotSelected AND enqueues evTCPUp

	// Acknowledge the entering-NotSelected reaction before the Select commit below: otherwise the
	// Select CAS could land before run() dequeues evTCPUp, coalescing the bring-up into a single
	// NotConnected->Selected report instead of the plain NotSelected->Selected this test targets.
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.Contains(reactions, [2]ConnState{NotConnectedState, NotSelectedState})
	}, time.Second, time.Millisecond, "the entering-NotSelected reaction must be processed before the Select commit")

	require.True(t, s.CommitSelected(CauseUnknown)) // pre-commits state=Selected AND enqueues evSelectAccepted

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
	s := newTestSupervisor(t,
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
	s := newTestSupervisor(t, func(prev, next ConnState) { seen <- stateChange{prev: prev, next: next} })
	go s.run()
	defer s.stop()
	go s.notifier()

	s.CommitConnected(CauseUnknown) // NotConnected -> NotSelected; enqueues evTCPUp
	s.inject(evSelectAccepted, CauseUnknown)
	s.inject(evDisconnect, CauseUnknown) // -> terminal NotConnected

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
	s := newTestSupervisor(t)
	go s.run()
	defer s.stop()

	s.requestClose(e, CauseUnknown) // pins e, injects evClose -> supervisor initiates teardown of the pinned epoch
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
	}, newHandlerPtr(), nil)
	go s.run()
	defer s.stop()

	// Reach NotSelected first (TCP up, not yet selected — the T7 dwell window; a synchronous CAS).
	s.CommitConnected(CauseUnknown)
	require.Equal(t, NotSelectedState, s.State())

	// T7 expires while still NotSelected: disconnect + reconnect (here just the reaction fires).
	s.inject(evT7Timeout, CauseUnknown)

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
	}, newHandlerPtr(), nil)
	go s.run()
	defer s.stop()

	// Drive all the way to Selected.
	s.CommitConnected(CauseUnknown) // NotConnected -> NotSelected; enqueues evTCPUp
	s.inject(evSelectAccepted, CauseUnknown)
	require.Eventually(t, func() bool { return s.State() == SelectedState }, time.Second, time.Millisecond)

	// A stale T7 fires: it MUST be a no-op from Selected — no state change, no NotConnected reaction.
	s.inject(evT7Timeout, CauseUnknown)

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
	s := newSupervisor(func(_, _ ConnState) {}, newHandlerPtr(), nil)
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

	s.step(fsmCommand{ev: evT7Timeout})

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
	s := newSupervisorWithEventsCap(func(_, _ ConnState) {}, newHandlerPtr(), nil, 8)

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
	s := newSupervisor(func(_, _ ConnState) {}, newHandlerPtr(), nil)

	require.Equal(t, supervisorFallbackCloseTimeout, s.resolveCloseTimeout(), "nil provider → fallback")

	var live atomic.Int64
	live.Store(int64(3 * time.Second))
	s.closeTimeout = func() time.Duration { return time.Duration(live.Load()) }
	require.Equal(t, 3*time.Second, s.resolveCloseTimeout())

	live.Store(int64(7 * time.Second)) // a mid-session config change
	require.Equal(t, 7*time.Second, s.resolveCloseTimeout(), "provider must be read live, not cached")
}

// TestSupervisor_GenerationMatchAtProcessingTime pins the half of the generation barrier that no
// injection-site check can provide: the decision is taken when the FSM APPLIES the event, not when
// the event was queued.
//
// The dangerous interleaving is a disconnect that passes every check its injector could make — the
// generation really was live then — and is only overtaken by the successor's bring-up while it sits
// in the queue. Here that is modelled directly: the event is queued naming gen 1, the live
// generation advances to 2, and the successor's Selected state must survive.
//
// A gen of 0 (an injection site that named no generation) and a nil provider (a supervisor with no
// connection) must both still be processed — suppressing those would silently disable involuntary
// disconnects for out-of-module transports.
func TestSupervisor_GenerationMatchAtProcessingTime(t *testing.T) {
	cases := []struct {
		name    string
		curGen  func() uint64
		cmdGen  uint64
		wantOut ConnState
	}{
		{name: "stale generation is discarded", curGen: func() uint64 { return 2 }, cmdGen: 1, wantOut: SelectedState},
		{name: "live generation is honored", curGen: func() uint64 { return 2 }, cmdGen: 2, wantOut: NotConnectedState},
		{name: "unnamed generation is honored", curGen: func() uint64 { return 2 }, cmdGen: 0, wantOut: NotConnectedState},
		{name: "no provider is honored", curGen: nil, cmdGen: 1, wantOut: NotConnectedState},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var reacted atomic.Bool
			s := newSupervisorWithEventsCap(func(_, next ConnState) {
				if next == NotConnectedState {
					reacted.Store(true)
				}
			}, newHandlerPtr(), nil, supervisorEventsCap)
			s.curGen = tc.curGen
			s.state.Store(uint32(SelectedState))
			s.lastReacted = SelectedState

			s.step(fsmCommand{ev: evDisconnect, cause: CauseIOError, gen: tc.cmdGen})

			require.Equal(t, tc.wantOut, s.State())
			require.Equal(t, tc.wantOut == NotConnectedState, reacted.Load(),
				"the reaction must fire exactly when the event was honored")

			wantStale := uint64(0)
			if tc.wantOut == SelectedState {
				wantStale = 1
			}
			require.Equal(t, wantStale, s.staleGen.Load(), "a discarded event must be counted")
		})
	}
}

// TestSupervisor_CommitFromGenerationHonorsTheGate pins the contract commitFrom relies on for the
// Select-accepted commit, which no end-to-end test can state directly: it is a CAS operation applied
// by the caller's own goroutine, so it never reaches step and step's generation match cannot cover it.
// Its barrier is selectGate, and this is what the gate must do with each answer.
//
// The rows that must still COMMIT are as load-bearing as the ones that must not: a gen of 0 is
// every out-of-module transport and secs1, and a nil gate is a supervisor built without a
// connection — suppressing either would silently disable Select for them.
// A gen of 0 is admitted on liveness alone, mirroring connection.selectCommitGate's own identity skip;
// the fake gate below treats gen 0 as live only when the row's zeroLive says so.
// This is never bypassed outright the way it is for the other two synchronous commits under commitGate:
// the "gen 0 but not live" row below is what proves gen 0 still REACHES the gate rather than skipping it.
func TestSupervisor_CommitFromGenerationHonorsTheGate(t *testing.T) {
	const liveGen = uint64(7)

	cases := []struct {
		name        string
		installGate bool
		cmdGen      uint64
		zeroLive    bool // only consulted when cmdGen == 0: whether the fake gate reports it live
		wantCommit  bool
		wantState   ConnState
		wantStale   uint64
	}{
		{name: "live generation commits", installGate: true, cmdGen: liveGen, wantCommit: true, wantState: SelectedState},
		{name: "ended generation is refused", installGate: true, cmdGen: liveGen - 1, wantState: NotSelectedState, wantStale: 1},
		{name: "unnamed generation is admitted on liveness, not identity", installGate: true, cmdGen: 0, zeroLive: true, wantCommit: true, wantState: SelectedState},
		{name: "unnamed generation still reaches the gate and can be refused on liveness", installGate: true, cmdGen: 0, zeroLive: false, wantState: NotSelectedState, wantStale: 1},
		{name: "no gate installed commits", installGate: false, cmdGen: liveGen, wantCommit: true, wantState: SelectedState},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newSupervisorWithEventsCap(func(_, _ ConnState) {}, newHandlerPtr(), nil, supervisorEventsCap)
			if tc.installGate {
				// The production gate resolves the live generation under connection.genGate; here the
				// answer is fixed, which is all commitFrom's contract depends on.
				// gen == 0 skips only the identity check —
				// the same skip connection.selectCommitGate applies for an unnamed generation —
				// liveness (zeroLive) is still enforced.
				s.selectGate = func(gen uint64, cas func() bool) (committed, live bool) {
					if gen == 0 {
						if !tc.zeroLive {
							return false, false
						}

						return cas(), true
					}
					if gen != liveGen {
						return false, false
					}

					return cas(), true
				}
			}
			s.state.Store(uint32(NotSelectedState))

			require.Equal(t, tc.wantCommit, s.CommitSelectedFromGeneration(tc.cmdGen, CauseSelectAccepted))
			require.Equal(t, tc.wantState, s.State(), "the CAS must run exactly when the gate admitted it")
			require.Equal(t, tc.wantStale, s.staleGen.Load(), "a refused commit must be counted")

			if tc.wantCommit {
				// The follow-up event carries the generation onward, so step's own match applies to it too.
				require.Equal(t, fsmCommand{ev: evSelectAccepted, cause: CauseSelectAccepted, gen: tc.cmdGen}, <-s.events)
			} else {
				require.Empty(t, s.events, "a refused commit must enqueue nothing")
			}
		})
	}
}

// TestSupervisor_CommitGateBypassesUnnamedGeneration pins commitFrom's gen-0 story for the
// Select-lost commit: commitGate is skipped OUTRIGHT for a gen of 0, not merely admitted on liveness.
// A gen of 0 must reach neither the gate's identity check nor its liveness check,
// while a NAMED generation still goes through the same gate and can be refused.
//
// The TCP-up commit no longer shares this story — it is dispatched through tcpUpCommitGate instead,
// which is consulted for every generation including gen 0; see TestSupervisor_TCPUpCommitGateRouting.
func TestSupervisor_CommitGateBypassesUnnamedGeneration(t *testing.T) {
	const liveGen = uint64(7)

	var called atomic.Bool
	refuseAll := func(gen uint64, cas func() bool) (committed, live bool) {
		called.Store(true)

		return false, false
	}

	s := newSupervisorWithEventsCap(func(_, _ ConnState) {}, newHandlerPtr(), nil, supervisorEventsCap)
	s.commitGate = refuseAll

	// gen 0, Select-lost commit from Selected: the gate must be bypassed entirely.
	s.state.Store(uint32(SelectedState))
	s.lastReacted = SelectedState
	require.True(t, s.CommitSelectLostFromGeneration(0, CauseUnknown), "gen 0 must bypass commitGate outright")
	require.Equal(t, NotSelectedState, s.State())
	require.False(t, called.Load(), "commitGate must not be consulted for gen 0")
	require.Equal(t, fsmCommand{ev: evSelectLost, cause: CauseUnknown, gen: 0}, <-s.events)

	// A NAMED, ended generation's Select-lost commit must still be refused by the very same gate.
	s.state.Store(uint32(SelectedState))
	require.False(t, s.CommitSelectLostFromGeneration(liveGen, CauseUnknown), "a named ended generation must be refused")
	require.Equal(t, SelectedState, s.State())
	require.True(t, called.Load(), "a named generation must reach commitGate")
	require.Equal(t, uint64(1), s.staleGen.Load(), "a refused named commit must be counted")
	require.Empty(t, s.events, "a refused commit must enqueue nothing")
}

// TestSupervisor_TCPUpCommitGateRouting pins commitFrom's dispatch for the TCP-up commit:
// unlike commitGate, tcpUpCommitGate is consulted for EVERY generation including gen 0,
// and is skipped outright only when nil (a standalone supervisor with no connection, as other unit tests build).
func TestSupervisor_TCPUpCommitGateRouting(t *testing.T) {
	newRawSupervisor := func() *supervisor {
		return newSupervisorWithEventsCap(func(_, _ ConnState) {}, newHandlerPtr(), nil, supervisorEventsCap)
	}

	t.Run("nil gate bypasses to a bare CAS on a standalone supervisor", func(t *testing.T) {
		s := newRawSupervisor()

		require.True(t, s.CommitConnectedFromGeneration(0, CauseUnknown), "a nil tcpUpCommitGate must bypass")
		require.Equal(t, NotSelectedState, s.State())
		require.Equal(t, fsmCommand{ev: evTCPUp, cause: CauseUnknown, gen: 0}, <-s.events)
	})

	t.Run("gen 0 live is admitted", func(t *testing.T) {
		var seenGen uint64

		s := newRawSupervisor()
		s.tcpUpCommitGate = func(gen uint64, cas func() bool) (committed, live bool) {
			seenGen = gen

			return cas(), true
		}

		require.True(t, s.CommitConnectedFromGeneration(0, CauseUnknown), "a live gen 0 must be admitted")
		require.Equal(t, NotSelectedState, s.State())
		require.Equal(t, uint64(0), seenGen, "gen 0 must reach the gate, not bypass it")
		require.Equal(t, fsmCommand{ev: evTCPUp, cause: CauseUnknown, gen: 0}, <-s.events)
	})

	t.Run("gen 0 ended is refused and counted", func(t *testing.T) {
		s := newRawSupervisor()
		s.tcpUpCommitGate = func(_ uint64, _ func() bool) (committed, live bool) { return false, false }

		require.False(t, s.CommitConnectedFromGeneration(0, CauseUnknown), "an ended gen 0 must be refused")
		require.Equal(t, NotConnectedState, s.State())
		require.Equal(t, uint64(1), s.staleGen.Load())
		require.Empty(t, s.events, "a refused commit must enqueue nothing")
	})

	t.Run("named live is admitted", func(t *testing.T) {
		const liveGen = uint64(3)

		var seenGen uint64

		s := newRawSupervisor()
		s.tcpUpCommitGate = func(gen uint64, cas func() bool) (committed, live bool) {
			seenGen = gen

			return cas(), true
		}

		require.True(t, s.CommitConnectedFromGeneration(liveGen, CauseUnknown))
		require.Equal(t, NotSelectedState, s.State())
		require.Equal(t, liveGen, seenGen)
		require.Equal(t, fsmCommand{ev: evTCPUp, cause: CauseUnknown, gen: liveGen}, <-s.events)
	})

	t.Run("named stale is refused and counted", func(t *testing.T) {
		const staleGen = uint64(9)

		s := newRawSupervisor()
		s.tcpUpCommitGate = func(_ uint64, _ func() bool) (committed, live bool) { return false, false }

		require.False(t, s.CommitConnectedFromGeneration(staleGen, CauseUnknown))
		require.Equal(t, NotConnectedState, s.State())
		require.Equal(t, uint64(1), s.staleGen.Load())
		require.Empty(t, s.events, "a refused commit must enqueue nothing")
	})
}

// recordingReact returns a react func plus the slice it appends (prev, next) pairs into.
// It is meant for the synchronous, single-goroutine tests below: step is called directly (run is never started), so no locking is needed around the slice.
func recordingReact() (func(prev, next ConnState), *[][2]ConnState) {
	var reactions [][2]ConnState

	return func(prev, next ConnState) {
		reactions = append(reactions, [2]ConnState{prev, next})
	}, &reactions
}

// countEnteringNotConnected returns how many recorded reactions entered NotConnectedState —
// the "one reaction per actual drop" count the tests below assert on,
// as distinct from the total number of react calls.
func countEnteringNotConnected(reactions [][2]ConnState) int {
	n := 0
	for _, r := range reactions {
		if r[1] == NotConnectedState {
			n++
		}
	}

	return n
}

// drainNotify does a single non-blocking read of s.notify. step is synchronous in these tests
// (run is never started), so a notification fired by the step call just made is already sitting
// in the buffered channel — no wait is needed, and a second call proves nothing further arrived.
func drainNotify(t *testing.T, s *supervisor) (stateChange, bool) {
	t.Helper()

	select {
	case sc := <-s.notify:
		return sc, true
	default:
		return stateChange{}, false
	}
}

// stepQueued dequeues exactly one command from s.events and applies it — the bounded,
// deterministic stand-in run() provides in these synchronous tests (run is never started).
// It requires exactly one command to already be queued,
// so a regression that fails to enqueue fails this assertion immediately instead of hanging on a bare <-s.events.
func stepQueued(t *testing.T, s *supervisor) {
	t.Helper()

	require.Len(t, s.events, 1, "exactly one command must be queued before stepQueued")
	s.step(<-s.events)
}

// waitEpochBounded bounds e.wait(), the epoch's ONLY blocking teardown-result read: a regression
// that never closes e.done must fail this test instead of hanging it.
// It also proves teardown fully COMPLETED —
// the bounded join returned and closed e.done —
// not merely that e.teardown() was called:
// teardown() itself is documented to return immediately,
// before the join it kicks off actually finishes.
func waitEpochBounded(t *testing.T, e *epoch) error {
	t.Helper()

	select {
	case <-e.done:
	case <-time.After(2 * time.Second):
		t.Fatal("epoch teardown did not complete within the bound")
	}

	return e.wait()
}

// TestSupervisor_TCPUpCoalescesPrecommittedSelectThenDisconnectFiresOnce drives the first lost-reaction order directly:
// a Select commit's CAS lands before its own evSelectAccepted report is enqueued, so the supervisor observes Selected while processing the still-queued evTCPUp.
// The coalesced bring-up must be reported once, as NotConnected->Selected with the cause a genuine Select carries — not evTCPUp's own.
// A real disconnect that follows must still fire its own entering-NotConnected reaction, and the Select commit's own report, arriving later, must find nothing left to fire or store.
func TestSupervisor_TCPUpCoalescesPrecommittedSelectThenDisconnectFiresOnce(t *testing.T) {
	react, reactions := recordingReact()
	s := newSupervisorWithEventsCap(react, newHandlerPtr(), nil, 8)

	require.True(t, s.CommitConnected(CauseLocalOpen)) // NotConnected -> NotSelected; enqueues evTCPUp
	// The Select responder's CAS lands before its own evSelectAccepted is ever enqueued.
	require.True(t, s.state.CompareAndSwap(uint32(NotSelectedState), uint32(SelectedState)))

	stepQueued(t, s) // processes the queued evTCPUp while state already reads Selected

	require.Equal(t, SelectedState, s.State())
	require.Equal(t, SelectedState, s.lastReacted)
	require.Len(t, *reactions, 1, "the coalesced bring-up must fire exactly once")
	require.Equal(t, [2]ConnState{NotConnectedState, SelectedState}, (*reactions)[0])

	sc, ok := drainNotify(t, s)
	require.True(t, ok, "expected a notification for the coalesced NotConnected->Selected transition")
	require.Equal(t, stateChange{prev: NotConnectedState, next: SelectedState, cause: CauseSelectAccepted}, sc,
		"the coalesced bring-up must report CauseSelectAccepted, not evTCPUp's own cause")

	s.inject(evDisconnect, CauseIOError)
	stepQueued(t, s)

	require.Equal(t, NotConnectedState, s.State())
	require.Len(t, *reactions, 2, "the real disconnect must still fire its own reaction")
	require.Equal(t, [2]ConnState{SelectedState, NotConnectedState}, (*reactions)[1])

	sc, ok = drainNotify(t, s)
	require.True(t, ok, "expected a notification for the disconnect")
	require.Equal(t, stateChange{prev: SelectedState, next: NotConnectedState, cause: CauseIOError}, sc)

	// The late evSelectAccepted — the Select commit's own report, only now enqueued — finds the
	// link already dropped: it must fire nothing and store nothing.
	s.inject(evSelectAccepted, CauseSelectAccepted)
	stepQueued(t, s)

	require.Equal(t, NotConnectedState, s.State())
	require.Len(t, *reactions, 2, "the late evSelectAccepted must not fire an additional reaction")
	_, ok = drainNotify(t, s)
	require.False(t, ok, "the late evSelectAccepted must not emit a notification")

	require.Equal(t, 1, countEnteringNotConnected(*reactions),
		"exactly one entering-NotConnected reaction across the whole sequence")
}

// TestSupervisor_DropReportsActualPredecessorEvenWhenLastReactedLags is the teeth test for the
// unconditional drop-predecessor rule.
// It drives the one order the withheld-report tests above cannot reach: there the Select commit's
// CAS always lands BEFORE its own evTCPUp is even processed, so lastReacted jumps straight from
// NotConnectedState to SelectedState and never disagrees with cur at drop time.
// Here evTCPUp is processed FIRST,
// while state still reads NotSelected —
// an ordinary, correctly reported bring-up that advances lastReacted to NotSelectedState —
// and ONLY THEN does the Select commit's CAS land,
// with its own evSelectAccepted never reaching the queue.
// The disconnect that follows must report prev as SelectedState, the state the link actually left,
// not NotSelectedState, the stale value lastReacted was left holding:
// a predecessor rule that falls back to lastReacted
// whenever it happens to already read NotConnectedState —
// rather than always reporting cur for a real drop —
// reports the stale NotSelectedState here instead.
func TestSupervisor_DropReportsActualPredecessorEvenWhenLastReactedLags(t *testing.T) {
	react, reactions := recordingReact()
	s := newSupervisorWithEventsCap(react, newHandlerPtr(), nil, 8)

	require.True(t, s.CommitConnected(CauseLocalOpen)) // NotConnected -> NotSelected; enqueues evTCPUp
	stepQueued(t, s)                                   // ordinary bring-up: lastReacted advances to NotSelected

	require.Equal(t, NotSelectedState, s.State())
	require.Equal(t, NotSelectedState, s.lastReacted)
	require.Len(t, *reactions, 1)
	require.Equal(t, [2]ConnState{NotConnectedState, NotSelectedState}, (*reactions)[0])

	sc, ok := drainNotify(t, s)
	require.True(t, ok)
	require.Equal(t, stateChange{prev: NotConnectedState, next: NotSelectedState, cause: CauseLocalOpen}, sc)

	// The Select commit's CAS lands ONLY NOW, with its own evSelectAccepted never enqueued.
	require.True(t, s.state.CompareAndSwap(uint32(NotSelectedState), uint32(SelectedState)))

	s.inject(evDisconnect, CauseIOError)
	stepQueued(t, s)

	require.Equal(t, NotConnectedState, s.State())
	require.Len(t, *reactions, 2, "the disconnect must still fire its own reaction")
	require.Equal(t, [2]ConnState{SelectedState, NotConnectedState}, (*reactions)[1],
		"prev must be the state the link actually left (Selected), not the stale lastReacted (NotSelected)")

	sc, ok = drainNotify(t, s)
	require.True(t, ok, "expected a notification for the disconnect")
	require.Equal(t, stateChange{prev: SelectedState, next: NotConnectedState, cause: CauseIOError}, sc)

	require.Equal(t, 1, countEnteringNotConnected(*reactions))
}

// TestSupervisor_DropReportsStateReplacedWhenSelectCommitsDuringStep covers a Select commit
// whose CAS lands after step has loaded the state for a disconnect but before it stores NotConnected.
// The drop must report the state the store actually replaced (Selected),
// not the value step loaded (NotSelected),
// because react decides the courtesy Separate from that predecessor.
func TestSupervisor_DropReportsStateReplacedWhenSelectCommitsDuringStep(t *testing.T) {
	react, reactions := recordingReact()
	s := newSupervisorWithEventsCap(react, newHandlerPtr(), nil, 8)

	require.True(t, s.CommitConnected(CauseLocalOpen))
	stepQueued(t, s)
	_, ok := drainNotify(t, s)
	require.True(t, ok)

	s.testHookAfterStateLoad = func(ev fsmEvent) {
		if ev == evDisconnect {
			// The Select commit's CAS lands inside step's load-to-store window; its report is never enqueued.
			require.True(t, s.state.CompareAndSwap(uint32(NotSelectedState), uint32(SelectedState)))
		}
	}

	s.inject(evDisconnect, CauseIOError)
	stepQueued(t, s)

	require.Equal(t, NotConnectedState, s.State())
	require.Len(t, *reactions, 2)
	require.Equal(t, [2]ConnState{SelectedState, NotConnectedState}, (*reactions)[1],
		"prev must be the state the store replaced, not the state step loaded")

	sc, ok := drainNotify(t, s)
	require.True(t, ok)
	require.Equal(t, stateChange{prev: SelectedState, next: NotConnectedState, cause: CauseIOError}, sc)
}

// TestSupervisor_DisconnectFromUnreportedTCPUpFiresAndBlocksResurrection drives the second lost-reaction order directly:
// the TCP-up CAS lands but its own evTCPUp report is withheld from the queue, and a disconnect overtakes it.
// The disconnect must still fire its entering-NotConnected reaction — reporting the state the link actually left, since lastReacted has nothing useful of its own to report —
// and the delayed evTCPUp, arriving after, must not resurrect NotSelected on the generation that already dropped.
// A second disconnect must find nothing further to do, and a fresh CommitConnected for a successor generation must still succeed.
func TestSupervisor_DisconnectFromUnreportedTCPUpFiresAndBlocksResurrection(t *testing.T) {
	react, reactions := recordingReact()
	s := newSupervisorWithEventsCap(react, newHandlerPtr(), nil, 8)

	// The TCP-up CAS lands, but (unlike CommitConnected) its own evTCPUp report is withheld.
	require.True(t, s.state.CompareAndSwap(uint32(NotConnectedState), uint32(NotSelectedState)))

	s.inject(evDisconnect, CauseIOError)
	stepQueued(t, s)

	require.Equal(t, NotConnectedState, s.State())
	require.Len(t, *reactions, 1, "the disconnect overtaking the unreported TCP-up must still fire")
	require.Equal(t, [2]ConnState{NotSelectedState, NotConnectedState}, (*reactions)[0],
		"the reported prev must be the state the link actually dropped from")

	sc, ok := drainNotify(t, s)
	require.True(t, ok, "expected a notification for the disconnect that overtook the unreported TCP-up")
	require.Equal(t, stateChange{prev: NotSelectedState, next: NotConnectedState, cause: CauseIOError}, sc)

	// The delayed evTCPUp report finally arrives: it must not resurrect NotSelected.
	s.inject(evTCPUp, CauseLocalOpen)
	stepQueued(t, s)

	require.Equal(t, NotConnectedState, s.State(), "a delayed TCP-up report must not resurrect a dropped generation")
	require.Len(t, *reactions, 1, "the delayed TCP-up report must fire nothing")
	_, ok = drainNotify(t, s)
	require.False(t, ok, "the delayed TCP-up report must emit no notification")

	// A second disconnect finds nothing left to do.
	s.inject(evDisconnect, CauseIOError)
	stepQueued(t, s)

	require.Equal(t, NotConnectedState, s.State())
	require.Len(t, *reactions, 1, "a disconnect from an already-NotConnected state must fire nothing")
	_, ok = drainNotify(t, s)
	require.False(t, ok, "a disconnect from an already-NotConnected state must emit no notification")

	// A fresh CommitConnected for the successor generation still succeeds.
	require.True(t, s.CommitConnected(CauseLocalOpen))
	require.Equal(t, NotSelectedState, s.State())

	require.Equal(t, 1, countEnteringNotConnected(*reactions))
}

// TestSupervisor_CloseOvertakingUnreportedCommitsFiresWithPrevSelected drives a voluntary Close that overtakes both the TCP-up and Select commits before either report reaches the queue —
// exactly reproducing the FIFO order a real Close can observe: both CASes land directly on state (no inject), so evClose is genuinely the first command the queue ever sees.
// The resulting entering-NotConnected reaction must still fire exactly once, reporting the state the link actually reached (Selected),
// and it must tear down the pinned epoch.
// The delayed TCP-up and Select reports, released only afterward, must find the supervisor latched closed:
// no store, no reaction, no notification.
func TestSupervisor_CloseOvertakingUnreportedCommitsFiresWithPrevSelected(t *testing.T) {
	react, reactions := recordingReact()
	s := newSupervisorWithEventsCap(react, newHandlerPtr(), nil, 8)

	// Both synchronous commits land directly on state; neither report is ever enqueued.
	require.True(t, s.state.CompareAndSwap(uint32(NotConnectedState), uint32(NotSelectedState)))
	require.True(t, s.state.CompareAndSwap(uint32(NotSelectedState), uint32(SelectedState)))

	e := newEpoch(t.Context(), logger.Default(), 8)
	s.requestClose(e, CauseLocalClose) // pins e and injects evClose — the FIRST command ever queued

	stepQueued(t, s)

	require.Equal(t, NotConnectedState, s.State())
	require.Equal(t, NotConnectedState, s.lastReacted)
	require.Len(t, *reactions, 1, "Close overtaking the unreported commits must still fire once")
	require.Equal(t, [2]ConnState{SelectedState, NotConnectedState}, (*reactions)[0],
		"the reported prev must be the state the link actually reached, not an unreported intermediate one")

	sc, ok := drainNotify(t, s)
	require.True(t, ok)
	require.Equal(t, stateChange{prev: SelectedState, next: NotConnectedState, cause: CauseLocalClose}, sc)

	// closeOnce admits exactly one teardown owner (epoch.teardown),
	// and step's own closed latch
	// (checked before the evClose handling below ever runs again)
	// keeps this the only evClose this supervisor will ever process —
	// so "torn down once" here turns on completion, not repetition:
	// the bounded wait below proves the join actually finished,
	// not merely that teardown() (which is documented to return immediately) was called.
	require.NoError(t, waitEpochBounded(t, e), "requestClose must have torn down the pinned epoch")

	// The delayed reports, released only now, must be rejected by the closed latch: no store, no
	// reaction, no notification.
	s.inject(evTCPUp, CauseLocalOpen)
	stepQueued(t, s)
	require.Equal(t, NotConnectedState, s.State())
	require.Equal(t, NotConnectedState, s.lastReacted)
	require.Len(t, *reactions, 1)

	s.inject(evSelectAccepted, CauseSelectAccepted)
	stepQueued(t, s)
	require.Equal(t, NotConnectedState, s.State())
	require.Equal(t, NotConnectedState, s.lastReacted)
	require.Len(t, *reactions, 1)

	_, ok = drainNotify(t, s)
	require.False(t, ok, "a report delayed past Close must emit no notification")
}

// TestSupervisor_ClosedLatchBlocksALegalEventFromANonTerminalState isolates the closed latch
// that step() checks first and unconditionally once evClose has been processed,
// from the separate illegal-from-NotConnected evTCPUp table entry:
// it puts the supervisor in a NON-terminal stored state with an otherwise perfectly legal queued event,
// so only the closed latch itself —
// not an illegal transition —
// can be what blocks it.
func TestSupervisor_ClosedLatchBlocksALegalEventFromANonTerminalState(t *testing.T) {
	react, reactions := recordingReact()
	s := newSupervisorWithEventsCap(react, newHandlerPtr(), nil, 8)

	s.step(fsmCommand{ev: evClose}) // a fresh, already-NotConnected supervisor: latches closed, fires nothing
	require.True(t, s.closed)
	require.Empty(t, *reactions)

	// A non-terminal stored state with an otherwise legal queued event (NotSelected + evSelectAccepted).
	s.state.Store(uint32(NotSelectedState))

	s.step(fsmCommand{ev: evSelectAccepted})

	require.Equal(t, NotSelectedState, s.State(), "the closed latch must block even a legal transition")
	require.Empty(t, *reactions, "the closed latch must block the reaction")
	_, ok := drainNotify(t, s)
	require.False(t, ok, "the closed latch must block the notification")
}
