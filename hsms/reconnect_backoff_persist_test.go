package hsms

// reconnect_backoff_persist_test.go — reconnect-backoff persistence regression tests.
//
// The reconnect loop keeps its exponential backoff growing across separate connectLoop invocations
// until a generation actually reaches Selected,
// instead of reseeding to the initial delay on every involuntary drop.
//
// Every test drives a real *connection against the in-package mockTransport (never real TCP) and
// observes the exact delay VALUES the loop computes via connection.testHookBackoff, called with
// the sleepFor duration immediately before the loop actually sleeps.
// Because the assertions are on the computed value (not on measured wall-clock timing),
// the oracle is exact, not a tolerance band —
// small configured delays (single-digit-to-double-digit milliseconds) still keep the tests fast.

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// errBackoffTestDrop is the involuntary-drop error every TCP-up-then-drop script in this file
// reports through TCPDown.
var errBackoffTestDrop = errors.New("hsms: reconnect-backoff test simulated drop")

// errBackoffTestDialFail is the dial error every dial-failure script in this file returns from
// Start.
var errBackoffTestDialFail = errors.New("hsms: reconnect-backoff test simulated dial failure")

// backoffWaitTimeout bounds every backoffRecorder wait in this file: every scenario here runs on
// millisecond-scale configured delays, so this is generous headroom, not a tight budget.
const backoffWaitTimeout = 5 * time.Second

// backoffRecorder collects the sequence of delays connection.testHookBackoff is called with, in
// call order, safely across the reconnect loop's own goroutine and the test goroutine.
//
// record appends under mu and then does a non-blocking signal on sig (capacity 1) — it can never
// block the reconnect loop, even if the test is not currently waiting.
type backoffRecorder struct {
	mu     sync.Mutex
	delays []time.Duration
	sig    chan struct{}
}

func newBackoffRecorder() *backoffRecorder {
	return &backoffRecorder{sig: make(chan struct{}, 1)}
}

// record is installed as connection.testHookBackoff.
func (r *backoffRecorder) record(d time.Duration) {
	r.mu.Lock()
	r.delays = append(r.delays, d)
	r.mu.Unlock()

	select {
	case r.sig <- struct{}{}:
	default:
	}
}

func (r *backoffRecorder) snapshot() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]time.Duration(nil), r.delays...)
}

// waitCount blocks (bounded by backoffWaitTimeout) until at least n delays have been recorded, and
// returns the snapshot at that point.
// On timeout it fails the test with what was actually observed,
// so a baseline-red run reports a diagnosable message instead of hanging.
func (r *backoffRecorder) waitCount(t *testing.T, n int) []time.Duration {
	t.Helper()

	deadline := time.After(backoffWaitTimeout)

	for {
		if snap := r.snapshot(); len(snap) >= n {
			return snap
		}

		select {
		case <-r.sig:
		case <-deadline:
			t.Fatalf("timed out waiting for %d hooked backoff delays; saw %v", n, r.snapshot())

			return nil
		}
	}
}

// waitGrowth blocks (bounded by timeout) until at least n delays have been recorded AND the most
// recently recorded delay exceeds initial —
// the "grown precondition" the reset-behavior tests need before they can exercise their reset assertion.
// Before this fix, connectLoop reseeded every invocation, so this never became true and the wait timed out.
func (r *backoffRecorder) waitGrowth(t *testing.T, n int, initial time.Duration, timeout time.Duration) []time.Duration {
	t.Helper()

	deadline := time.After(timeout)

	for {
		snap := r.snapshot()
		if len(snap) >= n && snap[len(snap)-1] > initial {
			return snap
		}

		select {
		case <-r.sig:
		case <-deadline:
			t.Fatalf("timed out waiting for a hooked backoff delay > initial (%v) after %d attempts; saw %v (%d total)",
				initial, n, capDelaysForLog(r.snapshot()), len(r.snapshot()))

			return nil
		}
	}
}

// capDelaysForLog truncates a failure-message delay slice to a readable prefix — a baseline-red
// wait that never satisfies its precondition can otherwise accumulate hundreds of identical
// entries before its bounded timeout fires.
func capDelaysForLog(delays []time.Duration) []time.Duration {
	const maxLogged = 10
	if len(delays) <= maxLogged {
		return delays
	}

	return delays[:maxLogged]
}

// tcpUpThenDropScript drives TCPUp then an immediate involuntary TCPDown on every Start call —
// no Select ever happens.
// It is called synchronously on whatever goroutine invokes Start
// (Open's own goroutine for the very first generation, the reconnect loop's goroutine for every later one),
// which is safe: TCPUp/TCPDown only CAS state and enqueue bounded, non-blocking events.
func tcpUpThenDropScript() mockScript {
	return func(_ context.Context, rt TransportRuntime) error {
		rt.TCPUp(fakeConn{})
		rt.TCPDown(errBackoffTestDrop)

		return nil
	}
}

// mixedFailureScript alternates a dial failure on every even-numbered Start call with a
// TCP-up-then-drop success on every odd-numbered one (Start 1 always succeeds, so the connection
// opens normally).
func mixedFailureScript() mockScript {
	var calls atomic.Int32

	return func(_ context.Context, rt TransportRuntime) error {
		n := calls.Add(1)
		if n%2 == 0 {
			return errBackoffTestDialFail
		}

		rt.TCPUp(fakeConn{})
		rt.TCPDown(errBackoffTestDrop)

		return nil
	}
}

// commitSelectAndSignal retries CommitSelected (bounded ticker, never a time.Sleep-poll) until it
// commits (or the generation ctx ends), then closes done — the "commit completed" signal the
// harness contract requires: a test never infers a completed commit from State() alone.
func commitSelectAndSignal(ctx context.Context, rt TransportRuntime, done chan<- struct{}) {
	ticker := time.NewTicker(500 * time.Microsecond)
	defer ticker.Stop()

	for {
		if rt.CommitSelected() {
			close(done)

			return
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// selectedPredecessorScript drives generations 1-3 as TCP-up-then-drop (never selecting) and
// generation 4 as TCP-up then a real CommitSelected, signalling selectedSignal once that commit has returned.
// Any later generation (5+) falls back to TCP-up-then-drop
// so the connection keeps producing hooked delays after the drop that follows selection.
func selectedPredecessorScript(selectedSignal chan struct{}) mockScript {
	var calls atomic.Int32

	return func(ctx context.Context, rt TransportRuntime) error {
		n := calls.Add(1)
		if n == 4 {
			go func() {
				rt.TCPUp(fakeConn{})
				commitSelectAndSignal(ctx, rt, selectedSignal)
			}()

			return nil
		}

		rt.TCPUp(fakeConn{})
		rt.TCPDown(errBackoffTestDrop)

		return nil
	}
}

// smallBackoffConfig applies the (10ms, x2, T5=1s) configuration every hsms test in this file
// (except the ceiling/flat cases, which need their own T5/multiplier) uses.
func smallBackoffConfig(t *testing.T, c *connection) {
	t.Helper()
	require.NoError(t, c.UpdateConfigOptions(WithReconnectBackoff(10*time.Millisecond, 2.0), WithT5(time.Second)))
}

// ── Grows across TCP-up-then-drop cycles ────────────────────────────────────────────────

// TestReconnectBackoff_GrowsAcrossTCPUpThenDropCycles: every Start drives TCPUp then
// an immediate transport disconnect (no Select),
// so react's NotConnected reaction launches a fresh connectLoop invocation after every generation.
// The first five hooked delays must grow 10, 20, 40, 80, 160ms.
//
// Before this fix, connectLoop reseeded delay to the configured initial at the top of every invocation,
// so every hooked delay read 10ms.
func TestReconnectBackoff_GrowsAcrossTCPUpThenDropCycles(t *testing.T) {
	rec := newBackoffRecorder()
	c, _ := newLifeConn(t, tcpUpThenDropScript())
	smallBackoffConfig(t, c)
	c.testHookBackoff = rec.record

	require.NoError(t, c.Open(t.Context(), OpenBackground))

	got := rec.waitCount(t, 5)

	want := []time.Duration{
		10 * time.Millisecond,
		20 * time.Millisecond,
		40 * time.Millisecond,
		80 * time.Millisecond,
		160 * time.Millisecond,
	}
	require.Equal(t, want, got[:5], "the reconnect backoff must keep growing across separate TCP-up-then-drop cycles")
}

// ── Mixed failure kinds share one sequence ──────────────────────────────────────────────

// TestReconnectBackoff_MixedFailureKindsShareOneSequence: Starts alternate a dial
// failure with a TCP-up-then-drop success.
// The growing backoff sequence must be shared across both failure kinds: 10, 20, 40, 80ms.
//
// Before this fix, the delay reset to initial at the start of every connectLoop invocation,
// losing the cross-loop continuation through the TCP-up-then-drop successes.
func TestReconnectBackoff_MixedFailureKindsShareOneSequence(t *testing.T) {
	rec := newBackoffRecorder()
	c, _ := newLifeConn(t, mixedFailureScript())
	smallBackoffConfig(t, c)
	c.testHookBackoff = rec.record

	require.NoError(t, c.Open(t.Context(), OpenBackground))

	got := rec.waitCount(t, 4)

	want := []time.Duration{
		10 * time.Millisecond,
		20 * time.Millisecond,
		40 * time.Millisecond,
		80 * time.Millisecond,
	}
	require.Equal(t, want, got[:4], "the backoff sequence must grow across BOTH dial failures and TCP-up-then-drop cycles")
}

// ── Selected predecessor resets ─────────────────────────────────────────────────────────

// TestReconnectBackoff_SelectedPredecessorResets: generations 1-3 TCP-up-then-drop
// without ever selecting, generation 4 reaches Selected (signalled after CommitSelected returns),
// and the test then drops that selected link.
// The delay for the generation that follows a SELECTED predecessor must reset to initial (10ms), not continue growing to 80ms.
//
// This assertion holds even without a per-epoch selected marker,
// because that reset is also what the old always-reseed code produced;
// its teeth are the mutation check: removing epoch.reachedSelected's reset makes this read 80ms instead of 10ms.
func TestReconnectBackoff_SelectedPredecessorResets(t *testing.T) {
	rec := newBackoffRecorder()
	selectedSignal := make(chan struct{})
	c, mt := newLifeConn(t, selectedPredecessorScript(selectedSignal))
	smallBackoffConfig(t, c)
	c.testHookBackoff = rec.record

	require.NoError(t, c.Open(t.Context(), OpenBackground))

	// Starts 1-3 TCP-up-then-drop: the hooked delays preceding Starts 2, 3, and 4.
	pre := rec.waitCount(t, 3)
	t.Logf("delays preceding the selected generation (Start 4): %v", pre)

	// Start 4 selects; wait for the signal sent AFTER CommitSelected returns (never inferred
	// from State() alone).
	select {
	case <-selectedSignal:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for generation 4's CommitSelected to complete")
	}

	// Drop the now-selected link.
	mt.simulateReadError(errBackoffTestDrop)

	got := rec.waitCount(t, 4)
	require.Equal(t, 10*time.Millisecond, got[3],
		"the delay following a SELECTED predecessor must reset to initial, not continue growing")
}

// ── Reset on a Select commit that never produced a Selected reaction ───────────────────

// stepParker is a one-shot, targeted park installed as supervisor.testHookAfterStateLoad. It stays
// inert until armed, and then parks step() exactly once, on the first evDisconnect it observes
// after arming — letting the test interpose a concurrent CommitSelectedFromGeneration between the
// disconnect's generation-match check and its state transition/store.
type stepParker struct {
	armed   atomic.Bool
	parked  chan struct{}
	release chan struct{}
}

func newStepParker() *stepParker {
	return &stepParker{parked: make(chan struct{}), release: make(chan struct{})}
}

func (p *stepParker) hook(ev fsmEvent) {
	if ev != evDisconnect || !p.armed.CompareAndSwap(true, false) {
		return
	}

	close(p.parked)
	<-p.release
}

// TestReconnectBackoff_ResetOnSelectCommitWithoutReaction: a Select commit can
// succeed on the recv goroutine for a generation whose disconnect is ALREADY being processed by
// the supervisor — so the commit never produces a Selected reaction or notification, yet it must
// still reset the backoff for whatever generation follows.
//
// Construction:
//  1. supervisor.testHookAfterStateLoad is installed from the FIRST mock Start, before it drives
//     any runtime event.
//  2. Earlier generations (1 and 2) TCP-up-then-drop until a hooked delay exceeds initial.
//  3. Generation 3 (the target) reaches NotSelected and that reaction is observed complete via a
//     lifecycle subscription registered before Open.
//  4. The test injects generation 3's disconnect and waits for the armed hook to park inside
//     step(), after the generation match but before the transition/store.
//  5. It calls the real CommitSelectedFromGeneration for generation 3 and requires it returns
//     true (the queue has room; nothing else produces events in this fixture).
//  6. It releases the hook.
//     The disconnect's own plain Store then overwrites the late commit,
//     so the FSM ends at NotConnected with no Selected reaction ever recorded,
//     and the delay for the generation that follows must reset to initial.
//
// Before this fix, the grown-delay precondition in step 2 below never became true,
// because connectLoop always reseeded (see backoffRecorder.waitGrowth).
func TestReconnectBackoff_ResetOnSelectCommitWithoutReaction(t *testing.T) {
	rec := newBackoffRecorder()
	parker := newStepParker()

	var calls atomic.Int32
	script := func(_ context.Context, rt TransportRuntime) error {
		n := calls.Add(1)
		if n == 1 {
			s := connRuntimeSupervisor(rt)
			require.NotNil(t, s, "the supervisor must already be installed before the first Start runs")
			s.testHookAfterStateLoad = parker.hook
		}

		switch n {
		case 3:
			// The target generation: reach NotSelected and stay there.
			// The test drives the Select commit directly.
			rt.TCPUp(fakeConn{})
		default:
			// Generations 1, 2, and any later one: TCP-up-then-drop.
			rt.TCPUp(fakeConn{})
			rt.TCPDown(errBackoffTestDrop)
		}

		return nil
	}

	c, mt := newLifeConn(t, script)
	smallBackoffConfig(t, c)
	c.testHookBackoff = rec.record

	// The release must run even if an assertion between arming and the explicit release below fails.
	// t.Cleanup runs LIFO, and this is registered AFTER newLifeConn's own Close cleanup,
	// so it fires first and frees a parked step() before Close tries to join it.
	// Idempotent via sync.Once, since the test's own explicit release (after a successful run) also calls it.
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(parker.release) }) }
	t.Cleanup(release)

	var mu sync.Mutex
	var transitions []ConnState
	cancel := c.SubscribeLifecycle(func(ev LifecycleEvent) {
		mu.Lock()
		transitions = append(transitions, ev.Current)
		mu.Unlock()
	})
	defer cancel()

	require.NoError(t, c.Open(t.Context(), OpenBackground))

	// Step 2: earlier generations (1, 2) TCP-up-then-drop until a hooked delay exceeds initial —
	// the grown precondition.
	// This is the baseline-red point: it never becomes true today.
	grown := rec.waitGrowth(t, 1, 10*time.Millisecond, 5*time.Second)
	base := len(grown) // index of the hooked delay this test's reset assertion targets

	// Step 3: generation 3 (the target) reaches NotSelected.
	// Counting NotSelected entries (rather than reading the latest transition) rules out a race
	// where generation 2's own NotConnected notification has not yet been delivered when
	// generation 3's NotSelected fires — the latest transition could otherwise still read
	// generation 2's NotSelected.
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()

		n := 0
		for _, s := range transitions {
			if s == NotSelectedState {
				n++
			}
		}

		return n >= 3
	}, 5*time.Second, time.Millisecond, "generation 3 must reach NotSelected")

	targetGen := c.CurrentGeneration()
	require.NotZero(t, targetGen, "the target generation must have a live identity")

	// Step 4: arm the parker, then inject generation 3's disconnect; wait for the hook to park.
	parker.armed.Store(true)
	mt.simulateReadError(errBackoffTestDrop)

	select {
	case <-parker.parked:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the disconnect step to park")
	}

	// Step 5: the real commit, on behalf of the still-live (not yet ended) target generation.
	require.True(t, c.CommitSelectedFromGeneration(targetGen),
		"a Select commit for a generation whose disconnect has not yet stored NotConnected must still commit")

	// Step 6: release the parked step — its own plain Store overwrites the late commit.
	release()

	require.Eventually(t, func() bool { return c.State() == NotConnectedState }, 5*time.Second, time.Millisecond,
		"the disconnect must still win: the FSM ends NotConnected")

	// Notifications are delivered in order from one sender,
	// so a Selected reaction to the late commit would be delivered before the target generation's NotConnected.
	// Wait for that NotConnected (the third: generations 1, 2, and the target) before asserting none was seen.
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()

		n := 0
		for _, s := range transitions {
			if s == NotConnectedState {
				n++
			}
		}

		return n >= 3
	}, 5*time.Second, time.Millisecond, "the target generation's NotConnected notification must be delivered")
	require.Zero(t, c.sup.Load().droppedNotify.Load(), "no notification may have been coalesced away")

	mu.Lock()
	sawSelected := slices.Contains(transitions, SelectedState)
	mu.Unlock()
	require.False(t, sawSelected, "the late commit must never produce a Selected reaction or notification")

	// The delay for the generation that follows this disconnect must reset to initial.
	got := rec.waitCount(t, base+1)
	require.Equal(t, 10*time.Millisecond, got[base],
		"the delay following a generation whose Select commit succeeded must reset to initial, "+
			"even though the commit never produced a Selected reaction")
}

// connRuntimeSupervisor resolves the live supervisor from a TransportRuntime handed to a mock
// script, exploiting that every TransportRuntime in this package IS the *connection.
func connRuntimeSupervisor(rt TransportRuntime) *supervisor {
	c, ok := rt.(*connection)
	if !ok {
		return nil
	}

	return c.sup.Load()
}

// ── Gen-0 Select commit on an ended generation ─────────────────────────────────────────

// TestReconnectBackoff_GenZeroCommitOnEndedGenerationIsRefused: a gen-0 CommitSelected (the shape
// secs1 and any out-of-module transport use) issued while the reconnect loop is paused right
// after joining the fully-torn-down predecessor and before publishing a successor — cur still
// points at the ended generation, no successor exists yet — must count in staleGen.
//
// The pause point reuses the same one-shot testHookConnectLoop park the existing
// TestReconnect_GenFenceAfterClose test uses: it fires between the backoff sleep and the fresh
// epoch's publish, after the prior epoch's join has completed.
//
// The commit's own CAS (NotSelected -> Selected) cannot match at this point regardless of any
// generation gate, because the disconnect that started this reconnect cycle already drove
// State() to NotConnected before ending the generation — so committed==false and State() is
// left unchanged on BOTH today's and the fixed code.
// staleGen is the actual discriminator.
//
// Before this fix, commitFrom skipped the connection's generation gate entirely for gen 0
// (the CommitSelected path had no selectGate distinct from commitGate yet), so staleGen was never touched.
//
// Trailing assertion: a sanity check that the grown delay survives,
// not proof that the gen-0 commit above was refused —
// an admitted commit would have failed its own CAS on NotConnected anyway
// (staleGen above is the actual discriminator for the refusal).
// The parked loop has already advanced and persisted the delay for the generation it is about to publish
// (10ms -> 20ms) before this refused commit runs,
// and releasing it lets that generation come up and drop again without selecting.
// The successor loop — launched fresh by that drop's own reaction — must read the persisted 20ms,
// not fall back to initial:
// the refused gen-0 commit above must have no effect on the ongoing backoff sequence.
func TestReconnectBackoff_GenZeroCommitOnEndedGenerationIsRefused(t *testing.T) {
	rec := newBackoffRecorder()
	c, _ := newLifeConn(t, tcpUpThenDropScript())
	smallBackoffConfig(t, c)
	c.testHookBackoff = rec.record

	reached := make(chan struct{})
	proceed := make(chan struct{})
	var hookOnce sync.Once
	c.testHookConnectLoop = func() {
		hookOnce.Do(func() {
			close(reached)
			<-proceed
		})
	}

	// Registered AFTER newLifeConn's own Close cleanup, so it runs FIRST (t.Cleanup is LIFO) and
	// frees a parked loop before Close tries to join it — even if an assertion below fails.
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(proceed) }) }
	t.Cleanup(release)

	require.NoError(t, c.Open(t.Context(), OpenBackground))

	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the reconnect loop to reach its post-backoff fence")
	}

	// At the fence: the prior generation is fully torn down and joined, and no successor has
	// been published yet — cur still points at the ended generation.
	before := c.State()
	staleBefore := c.sup.Load().staleGen.Load()

	committed := c.CommitSelected()

	require.False(t, committed, "a gen-0 Select commit issued with no live generation must not commit")
	require.Equal(t, before, c.State(), "an unsuccessful commit must leave State() unchanged")
	require.Greater(t, c.sup.Load().staleGen.Load(), staleBefore,
		"a gen-0 Select commit against an already-torn-down generation, with no successor yet published, must count in staleGen")

	// Let the loop continue: the successor dials, then (per the script) drops again without
	// selecting — the refused commit above must have no side effect on that ongoing cycle.
	release()

	// The parked loop's own hooked delay (10ms) was already recorded before it reached the fence;
	// the successor it now publishes drops without selecting, and the loop THAT drop's reaction
	// launches must continue the persisted 20ms rather than reseed to initial.
	got := rec.waitCount(t, 2)
	require.Equal(t, 20*time.Millisecond, got[1],
		"a successor that drops without selecting must keep the grown delay, unaffected by the refused gen-0 commit")

	require.NoError(t, c.Close())
}

// ── Open reseeds ─────────────────────────────────────────────────────────────────────

// TestReconnectBackoff_OpenReseeds: after growing the delay across TCP-up-then-drop
// cycles, Close and re-Open with a script whose first dial fails.
// The first hooked delay of the NEW Open cycle must be initial, regardless of how far the prior cycle had grown.
//
// Before this fix, the grown-delay warm-up precondition never became true, because connectLoop always reseeded.
func TestReconnectBackoff_OpenReseeds(t *testing.T) {
	rec1 := newBackoffRecorder()
	c, mt := newLifeConn(t, tcpUpThenDropScript())
	smallBackoffConfig(t, c)
	c.testHookBackoff = rec1.record

	require.NoError(t, c.Open(t.Context(), OpenBackground))

	rec1.waitGrowth(t, 1, 10*time.Millisecond, 5*time.Second)

	require.NoError(t, c.Close())

	// A fresh recorder, installed only after Close has joined every loop from the prior cycle —
	// no observation from the dying cycle can land on it.
	rec2 := newBackoffRecorder()
	c.testHookBackoff = rec2.record
	mt.resetStart(withMockTransportDialFailsThenConnects(1, errBackoffTestDialFail))

	require.NoError(t, c.Open(t.Context(), OpenBackground))

	got := rec2.waitCount(t, 1)
	require.Equal(t, 10*time.Millisecond, got[0], "Open must reseed the backoff to initial regardless of the prior cycle's growth")
}

// ── Cold-active retry then Select failure ───────────────────────────────────────────────

// TestReconnectBackoff_ColdRetryThenReactLaunchedGrowth: an active connection's cold
// initial dial fails twice (the background retry loop, countReconnect=false),
// then succeeds but drops immediately without selecting (still within that SAME loop, still uncounted),
// and only THEN does a react-launched loop (countReconnect=true) take over.
// The react-launched loop's first delay must continue growing from the cold-retry loop's last delay (40ms),
// not reseed to initial — and only the react-launched success may increment Reconnects().
//
// Before this fix, the react-launched loop, being a NEW connectLoop invocation,
// reseeded to initial (10ms instead of 40ms).
func TestReconnectBackoff_ColdRetryThenReactLaunchedGrowth(t *testing.T) {
	rec := newBackoffRecorder()

	var calls atomic.Int32
	script := func(ctx context.Context, rt TransportRuntime) error {
		n := calls.Add(1)
		switch n {
		case 1, 2:
			return errBackoffTestDialFail
		case 3:
			// The cold-retry loop's uncounted success: connect then drop immediately.
			rt.TCPUp(fakeConn{})
			rt.TCPDown(errBackoffTestDrop)

			return nil
		default:
			// The react-launched loop's counted success: connect and STAY selected.
			go func() {
				rt.TCPUp(fakeConn{})
				driveSelect(ctx, rt)
			}()

			return nil
		}
	}

	c, _ := newLifeConn(t, script)
	require.NoError(t, c.UpdateConfigOptions(WithReconnectBackoff(10*time.Millisecond, 2.0), WithT5(time.Second)))
	c.testHookBackoff = rec.record

	require.NoError(t, c.Open(t.Context(), OpenBackground))

	// Hooked delays 1 and 2 precede Starts 2 and 3 (the cold-retry loop): 10ms, 20ms.
	// Compare only the first two:
	// a third delay (the react-launched loop's own) can already have landed by the time waitCount returns,
	// since it only waits for AT LEAST 2.
	got := rec.waitCount(t, 2)
	require.GreaterOrEqual(t, len(got), 2)
	require.Equal(t, []time.Duration{10 * time.Millisecond, 20 * time.Millisecond}, got[:2],
		"the cold-retry loop's own in-loop growth must be unaffected")

	// Hooked delay 3 precedes Start 4, on the REACT-launched loop.
	got = rec.waitCount(t, 3)
	require.Equal(t, 40*time.Millisecond, got[2],
		"the react-launched loop must continue growing from the cold-retry loop's last delay, not reseed")

	require.Eventually(t, func() bool { return c.State() == SelectedState }, 5*time.Second, time.Millisecond,
		"Start 4 must select and stay selected")

	// Reconnects() must read 1 only after the fourth successful Start,
	// with that generation kept stable (no further drop is scripted).
	require.Eventually(t, func() bool { return c.Metrics().Reconnects() == 1 }, 5*time.Second, time.Millisecond,
		"only the react-launched success may count as a reconnect")
}

// ── Ceiling and flat ─────────────────────────────────────────────────────────────────

// TestReconnectBackoff_CeilingAndFlat: growth caps at the configured T5 ceiling,
// and a multiplier of 1.0 preserves a flat delay.
// Both are in-loop dial-failure sequences (no generation ever connects until the final attempt),
// which today's code already computes correctly —
// this test is a preservation check, expected to pass on both the current and the fixed code.
func TestReconnectBackoff_CeilingAndFlat(t *testing.T) {
	t.Run("ceiling", func(t *testing.T) {
		rec := newBackoffRecorder()

		var calls atomic.Int32
		script := func(_ context.Context, rt TransportRuntime) error {
			if calls.Add(1) <= 4 {
				return errBackoffTestDialFail
			}

			rt.TCPUp(fakeConn{})

			return nil
		}

		c, _ := newLifeConn(t, script)
		require.NoError(t, c.UpdateConfigOptions(WithReconnectBackoff(10*time.Millisecond, 2.0), WithT5(40*time.Millisecond)))
		c.testHookBackoff = rec.record

		require.NoError(t, c.Open(t.Context(), OpenBackground))

		got := rec.waitCount(t, 4)
		want := []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond, 40 * time.Millisecond}
		require.Equal(t, want, got[:4], "growth must cap at the T5 ceiling and stay flat after")
	})

	t.Run("flat", func(t *testing.T) {
		rec := newBackoffRecorder()

		var calls atomic.Int32
		script := func(_ context.Context, rt TransportRuntime) error {
			if calls.Add(1) <= 3 {
				return errBackoffTestDialFail
			}

			rt.TCPUp(fakeConn{})

			return nil
		}

		c, _ := newLifeConn(t, script)
		require.NoError(t, c.UpdateConfigOptions(WithReconnectBackoff(10*time.Millisecond, 1.0), WithT5(time.Second)))
		c.testHookBackoff = rec.record

		require.NoError(t, c.Open(t.Context(), OpenBackground))

		got := rec.waitCount(t, 3)
		want := []time.Duration{10 * time.Millisecond, 10 * time.Millisecond, 10 * time.Millisecond}
		require.Equal(t, want, got[:3], "a multiplier of 1.0 must keep the delay flat at initial")
	})
}
