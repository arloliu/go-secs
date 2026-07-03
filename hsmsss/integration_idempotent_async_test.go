package hsmsss

// idempotent_async_property_test.go — v2 port of the v1
// tests/hsmsss_integration/idempotent_async_property_test.go property regression.
//
// Property (invariant): an async state-change handler MUST NEVER observe a (prev, next) tuple where
// prev == next. In v2 the supervisor enforces this via its lastReacted dedup: fireTransition is
// called only when next != lastReacted (see hsms/supervisor.go — the guarded block that reads
// "if next != s.lastReacted { s.fireTransition(s.lastReacted, next); s.lastReacted = next }"). The
// reported prev is always lastReacted, so a fired transition can never carry prev == next.
//
// The test drives a real active+passive New pair through >=50 open/select/close cycles under two
// timing regimes (random sleep and Gosched-only) and, every 4th cycle, calls Close() twice in rapid
// succession to provoke an idempotent ToNotConnected (state already NotConnected). If the dedup ever
// let such an event through, assertNoDuplicateEvents fails.
//
// Re-pointed to v2: hsms.Connection.AddConnStateChangeHandler(func(prev, next)); Open(ctx,
// OpenBackground); Close(); State()-polled waits (waitState). The final drain is a
// require.Eventually on the observers reaching the terminal NotConnected event (no time.Sleep).

import (
	"fmt"
	"math/rand"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/stretchr/testify/require"
)

// stateTuple records a single async transition event observed by a handler.
type stateTuple struct {
	prev  hsms.ConnState
	next  hsms.ConnState
	cycle int
	phase string
}

// asyncObserver collects (prev, next) tuples from async handlers under a mutex. Handler closures
// must only call mu.Lock/append/mu.Unlock — no t.Fatal, no I/O.
type asyncObserver struct {
	mu           sync.Mutex
	events       []stateTuple
	selectedSeen int // incremented each time next == SelectedState
}

// assertNoDuplicateEvents checks that no (prev==next) tuple was delivered to an async handler. It
// must be called post-test on the main goroutine.
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

// lastNext returns the next state of the most recently recorded event (NotConnectedState-sentinel
// -1 mapped to false via ok). Used to detect that the terminal NotConnected transition has been
// delivered to the async handler, which — because notify is a single FIFO channel — means every
// earlier event was delivered too.
func lastNext(obs *asyncObserver) (hsms.ConnState, bool) {
	obs.mu.Lock()
	defer obs.mu.Unlock()

	if len(obs.events) == 0 {
		return 0, false
	}

	return obs.events[len(obs.events)-1].next, true
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

	req := require.New(t)
	port := freeLoopbackPort(t)
	ctx := t.Context()

	// Build observers — one per side, shared across all cycles in this phase.
	activeObs := &asyncObserver{events: make([]stateTuple, 0, cycles*4)}
	passiveObs := &asyncObserver{events: make([]stateTuple, 0, cycles*4)}

	// activeCycleIdx / passiveCycleIdx are written atomically by the main loop and read atomically
	// by the async handler goroutine. The cycle label is diagnostic only (±1 race is harmless for
	// log readability, but we keep it race-detector clean).
	var activeCycleIdx, passiveCycleIdx atomic.Int64

	// Build endpoints once; reuse across cycles (v2 re-open after Close is supported once the prior
	// Close has drained — see connection_lifecycle Open's shutdown-gated reopen path).
	passive := newEndpoint(t, port, false, nil, echoHandler)
	active := newEndpoint(t, port, true, nil)

	// Register the property-testing async handlers (a second, independent handler on top of the
	// best-effort observer registered inside newEndpoint).
	passive.conn.AddConnStateChangeHandler(func(prev, next hsms.ConnState) {
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

	active.conn.AddConnStateChangeHandler(func(prev, next hsms.ConnState) {
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
		// Update cycle labels atomically so the async handler goroutine sees a consistent value
		// without triggering the race detector.
		activeCycleIdx.Store(int64(i))
		passiveCycleIdx.Store(int64(i))

		inject(rng)

		// Passive must be listening before the active dials.
		req.NoError(passive.conn.Open(ctx, hsms.OpenBackground), "phase=%s cycle=%d: passive open", phase, i)
		req.NoError(active.conn.Open(ctx, hsms.OpenBackground), "phase=%s cycle=%d: active open", phase, i)

		inject(rng)

		waitState(t, active, hsms.SelectedState)
		waitState(t, passive, hsms.SelectedState)

		inject(rng)

		req.NoError(active.conn.Close(), "phase=%s cycle=%d: active close", phase, i)
		req.NoError(passive.conn.Close(), "phase=%s cycle=%d: passive close", phase, i)

		// Every 4th cycle, call Close() again immediately to attempt provoking an idempotent
		// ToNotConnected (prev==next==NotConnected). The supervisor's lastReacted dedup must
		// suppress the resulting event; if it ever reaches an async handler the invariant check
		// below catches it. A second Close is idempotent (returns the prior nil error), so no error
		// is expected — but we ignore it defensively.
		if i%4 == 3 {
			_ = active.conn.Close()
			_ = passive.conn.Close()
		}

		inject(rng)

		// Drain the best-effort observer channels so the next cycle starts clean.
		drainStateCh(active.states)
		drainStateCh(passive.states)
	}

	// Wait (no time.Sleep) until both async observers have recorded their terminal NotConnected
	// event. notify is a single FIFO channel, so once the last-emitted (terminal NotConnected)
	// event has been delivered to a handler, every earlier event has been delivered too — the
	// observers are fully drained and safe to inspect.
	require.Eventually(t, func() bool {
		an, aok := lastNext(activeObs)
		pn, pok := lastNext(passiveObs)
		return aok && pok && an == hsms.NotConnectedState && pn == hsms.NotConnectedState
	}, 5*time.Second, 5*time.Millisecond, "async handlers did not observe terminal NotConnected")

	// --- Property assertion: no (prev==next) tuples ---
	assertNoDuplicateEvents(t, fmt.Sprintf("active/%s", phase), activeObs)
	assertNoDuplicateEvents(t, fmt.Sprintf("passive/%s", phase), passiveObs)

	// --- Multiset sanity: event count in plausible range ---
	// Each cycle produces at least 1 delivered transition and, with idempotent no-ops suppressed,
	// no more than ~6. We allow a loose bound: >= cycles and <= 6*cycles.
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

	req.GreaterOrEqualf(activeCount, cycles,
		"phase=%s active: expected >= %d events, got %d", phase, cycles, activeCount)
	req.LessOrEqualf(activeCount, 6*cycles,
		"phase=%s active: suspiciously many events (>6×cycles), got %d", phase, activeCount)

	req.GreaterOrEqualf(passiveCount, cycles,
		"phase=%s passive: expected >= %d events, got %d", phase, cycles, passiveCount)
	req.LessOrEqualf(passiveCount, 6*cycles,
		"phase=%s passive: suspiciously many events (>6×cycles), got %d", phase, passiveCount)

	// --- Selected-reached assertion ---
	req.GreaterOrEqualf(activeSelected, minSel,
		"phase=%s active: expected Selected reached >= %d times out of %d, got %d",
		phase, minSel, cycles, activeSelected)
	req.GreaterOrEqualf(passiveSelected, minSel,
		"phase=%s passive: expected Selected reached >= %d times out of %d, got %d",
		phase, minSel, cycles, passiveSelected)

	// --- Per-cycle event distribution log ---
	logPerCycleDistribution(t, phase, "active", activeObs)
	logPerCycleDistribution(t, phase, "passive", passiveObs)
}

// logPerCycleDistribution prints the (prev→next) transition shape per cycle as a diagnostic log,
// allowing reviewers to verify the invariant was actually exercised rather than passing vacuously.
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
