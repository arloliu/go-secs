package hsmsssintegration

// TestHSMS_IdempotentAsync_NoDuplicateEvents is a property test asserting the
// idempotent-async invariant from commit 7bb93eb:
//
//   Async handlers MUST NEVER observe a (prev, next) tuple where prev == next.
//
// The invariant is enforced in hsms/conn_state.go publishAsyncEvent, which
// gates on prevState == newState and drops the event before it reaches any
// user-registered async handler. This test verifies the gate holds across 50
// rapid open/select/close cycles with randomized timing, and across a second
// stress pass with no sleeps (Gosched only).
//
// Idempotent-transition provocation: every 4th cycle calls Close() twice in
// rapid succession. The second Close() will hit stateMgr.ToNotConnected() when
// the state is already NotConnectedState (prev==next). publishAsyncEvent must
// suppress the resulting event. If it ever reaches an async handler, the test
// fails immediately.
//
// Related: hsms/conn_state.go:publishAsyncEvent (lines 494-514).
// Related: tests/hsmsss_integration/lifecycle_test.go:TestLifecycle_UserHandlerReceivesAllTransitions.

import (
	"context"
	"fmt"
	"math/rand"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/go-secs/hsms"
	"github.com/stretchr/testify/require"
)

// stateTuple records a single async transition event observed by a handler.
type stateTuple struct {
	prev  hsms.ConnState
	next  hsms.ConnState
	cycle int
	phase string
}

// asyncObserver collects (prev, next) tuples from async handlers under a mutex.
// Handler closures must only call mu.Lock/append/mu.Unlock — no t.Fatal, no I/O.
type asyncObserver struct {
	mu           sync.Mutex
	events       []stateTuple
	selectedSeen int // incremented each time next == SelectedState
}

// assertNoDuplicateEvents checks that no (prev==next) tuple was delivered to
// an async handler. It must be called post-test on the main goroutine.
func assertNoDuplicateEvents(t *testing.T, name string, obs *asyncObserver) {
	t.Helper()
	obs.mu.Lock()
	defer obs.mu.Unlock()

	for _, ev := range obs.events {
		if ev.prev == ev.next {
			t.Errorf(
				"%s: invariant violated — async handler received idempotent transition "+
					"(prev=%s == next=%s) at cycle=%d phase=%q",
				name, ev.prev, ev.next, ev.cycle, ev.phase,
			)
		}
	}
}

func TestHSMS_IdempotentAsync_NoDuplicateEvents(t *testing.T) {
	const cycles = 50
	const minSelectedPerPhase = 40

	// --- Phase A: random-sleep stress ---
	t.Run("random_sleep", func(t *testing.T) {
		runIdempotentAsyncPhase(t, "random_sleep", cycles, minSelectedPerPhase,
			func(rng *rand.Rand) { time.Sleep(time.Duration(rng.Intn(11)) * time.Millisecond) },
		)
	})

	// --- Phase B: back-to-back (Gosched only) ---
	t.Run("gosched_only", func(t *testing.T) {
		runIdempotentAsyncPhase(t, "gosched_only", cycles, minSelectedPerPhase,
			func(_ *rand.Rand) { runtime.Gosched() },
		)
	})
}

// runIdempotentAsyncPhase executes one phase of the property test.
//
//   - phase:    human-readable label for error messages
//   - cycles:   number of open/close cycles to run
//   - minSel:   minimum number of cycles that must reach SelectedState
//   - inject:   timing-variation function called at randomized points per cycle
func runIdempotentAsyncPhase(
	t *testing.T,
	phase string,
	cycles int,
	minSel int,
	inject func(rng *rand.Rand),
) {
	t.Helper()

	require := require.New(t)
	port := getFreePort(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// Build observers — one per side, shared across all cycles in this phase.
	activeObs := &asyncObserver{events: make([]stateTuple, 0, cycles*4)}
	passiveObs := &asyncObserver{events: make([]stateTuple, 0, cycles*4)}

	// activeCycleIdx / passiveCycleIdx are written atomically by the main loop
	// and read atomically by the async handler goroutine. The cycle label is
	// diagnostic only (±1 race is harmless for log readability, but we keep it
	// race-detector clean).
	var activeCycleIdx, passiveCycleIdx atomic.Int64

	// Build endpoints once; reuse across cycles (like lifecycle_test.go).
	passive := newEndpoint(t, ctx, port, true, false, nil, echoHandler)
	active := newEndpoint(t, ctx, port, false, true, nil)

	// Register the property-testing async handlers. The helper-registered
	// handler in newEndpoint() already feeds selectedCh; we register a second
	// independent handler here.
	passive.session.AddConnStateChangeHandler(func(_ hsms.Connection, prev, next hsms.ConnState) {
		passiveObs.mu.Lock()
		passiveObs.events = append(passiveObs.events, stateTuple{
			prev:  prev,
			next:  next,
			cycle: int(passiveCycleIdx.Load()),
			phase: phase,
		})
		if next == hsms.SelectedState {
			passiveObs.selectedSeen++
		}
		passiveObs.mu.Unlock()
	})

	active.session.AddConnStateChangeHandler(func(_ hsms.Connection, prev, next hsms.ConnState) {
		activeObs.mu.Lock()
		activeObs.events = append(activeObs.events, stateTuple{
			prev:  prev,
			next:  next,
			cycle: int(activeCycleIdx.Load()),
			phase: phase,
		})
		if next == hsms.SelectedState {
			activeObs.selectedSeen++
		}
		activeObs.mu.Unlock()
	})

	t.Cleanup(func() {
		closeEndpoint(t, active)
		closeEndpoint(t, passive)
	})

	rng := rand.New(rand.NewSource(time.Now().UnixNano())) //nolint:gosec // test PRNG; security not required

	for i := range cycles {
		// Update cycle labels atomically so the async handler goroutine sees
		// a consistent value without triggering the race detector.
		activeCycleIdx.Store(int64(i))
		passiveCycleIdx.Store(int64(i))

		inject(rng)

		require.NoError(passive.conn.Open(false), "phase=%s cycle=%d: passive open", phase, i)
		require.NoError(active.conn.Open(false), "phase=%s cycle=%d: active open", phase, i)

		inject(rng)

		waitState(t, active, hsms.SelectedState)
		waitState(t, passive, hsms.SelectedState)

		inject(rng)

		require.NoError(active.conn.Close(), "phase=%s cycle=%d: active close", phase, i)
		require.NoError(passive.conn.Close(), "phase=%s cycle=%d: passive close", phase, i)

		// In 25% of cycles (every 4th), call Close() again immediately to
		// attempt provoking an idempotent ToNotConnected (prev==next==NotConnected).
		// publishAsyncEvent must suppress this at the gate. If it ever reaches an
		// async handler, the invariant check below will catch it.
		if i%4 == 3 {
			// Errors from double-Close are expected (already closed); ignore them.
			_ = active.conn.Close()
			_ = passive.conn.Close()
		}

		inject(rng)

		// Drain state channels so waitState in the next cycle starts from an
		// empty queue (mirrors lifecycle_test.go pattern).
		drainStateCh(active.selectedCh)
		drainStateCh(passive.selectedCh)
	}

	// Allow the async dispatcher to drain any in-flight events before we inspect.
	// A small fixed sleep is acceptable here — it does not inflate timing; it
	// just avoids a benign race between the last Close() and the assertion.
	time.Sleep(200 * time.Millisecond)

	// --- Property assertion: no (prev==next) tuples ---
	assertNoDuplicateEvents(t, fmt.Sprintf("active/%s", phase), activeObs)
	assertNoDuplicateEvents(t, fmt.Sprintf("passive/%s", phase), passiveObs)

	// --- Multiset sanity: event count in plausible range ---
	// Each cycle produces at least 2 distinct transitions (NotConnected→NotSelected,
	// NotSelected→Selected or NotSelected→NotConnected on fast close) and at most
	// ~6 (adding Connecting states, idempotent no-ops suppressed = not counted).
	// We allow a loose bound: ≥ cycles (at minimum 1 per cycle) and ≤ 6*cycles.
	activeObs.mu.Lock()
	activeCount := len(activeObs.events)
	activeSelected := activeObs.selectedSeen
	activeObs.mu.Unlock()

	passiveObs.mu.Lock()
	passiveCount := len(passiveObs.events)
	passiveSelected := passiveObs.selectedSeen
	passiveObs.mu.Unlock()

	t.Logf("phase=%s active: total_events=%d selected_seen=%d", phase, activeCount, activeSelected)
	t.Logf("phase=%s passive: total_events=%d selected_seen=%d", phase, passiveCount, passiveSelected)

	require.GreaterOrEqualf(activeCount, cycles,
		"phase=%s active: expected >= %d events, got %d", phase, cycles, activeCount)
	require.LessOrEqualf(activeCount, 6*cycles,
		"phase=%s active: suspiciously many events (>6×cycles), got %d", phase, activeCount)

	require.GreaterOrEqualf(passiveCount, cycles,
		"phase=%s passive: expected >= %d events, got %d", phase, cycles, passiveCount)
	require.LessOrEqualf(passiveCount, 6*cycles,
		"phase=%s passive: suspiciously many events (>6×cycles), got %d", phase, passiveCount)

	// --- Selected-reached assertion ---
	require.GreaterOrEqualf(activeSelected, minSel,
		"phase=%s active: expected Selected reached >= %d times out of %d, got %d",
		phase, minSel, cycles, activeSelected)
	require.GreaterOrEqualf(passiveSelected, minSel,
		"phase=%s passive: expected Selected reached >= %d times out of %d, got %d",
		phase, minSel, cycles, passiveSelected)

	// --- Per-cycle event distribution log ---
	logPerCycleDistribution(t, phase, "active", activeObs)
	logPerCycleDistribution(t, phase, "passive", passiveObs)
}

// logPerCycleDistribution prints the (prev→next) transition shape per cycle as
// a diagnostic log, allowing reviewers to verify the invariant was actually
// exercised rather than passing vacuously.
func logPerCycleDistribution(t *testing.T, phase, side string, obs *asyncObserver) {
	t.Helper()
	obs.mu.Lock()
	defer obs.mu.Unlock()

	// Group events by cycle.
	byCycle := make(map[int][]stateTuple)
	for _, ev := range obs.events {
		byCycle[ev.cycle] = append(byCycle[ev.cycle], ev)
	}

	for c := range 50 {
		evts, ok := byCycle[c]
		if !ok {
			continue
		}
		transitions := make([]string, 0, len(evts))
		for _, ev := range evts {
			transitions = append(transitions, fmt.Sprintf("%s→%s", ev.prev, ev.next))
		}
		t.Logf("phase=%s side=%s cycle=%d: %v", phase, side, c, transitions)
	}
}
