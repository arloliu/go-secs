package hsms

// connection_lifecycle_start_after_tcpup_test.go —
// a custom transport's Start reporting TCP-up and then failing must hand its retry off to the drop reaction's own loop, never run a second reconnect loop alongside it, and never leave a generation orphaned or the FSM stuck.
//
// Every test here drives a real *connection against the in-package mockTransport (never real TCP),
// synchronizing on the seams connectLoop, react, and the TCP-up gate now expose —
// testHookConnectLoopBarrier, testHookLoopSpawned, testHookBeforeHandoffInject, testHookLoopExit,
// testHookConnectLoopBeforeFailureTeardown, testHookReactBeforeCurLoad, testHookTCPUpCommitBeforeMarker, testHookBackoff —
// and on the existing supervisor seams (testHookAfterStateLoad, testHookBeforeEnqueue) rather than on any measured delay.

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// errStartAfterTCPUpDrop is the involuntary-drop error every TCP-up-then-drop script in this file reports through TCPDown.
var errStartAfterTCPUpDrop = errors.New("hsms_test: start-after-tcpup simulated drop")

// errStartAfterTCPUpFail is the Start failure every script in this file returns
// once it has driven TCP-up (and, where noted, a drop) for the generation under test.
var errStartAfterTCPUpFail = errors.New("hsms_test: start-after-tcpup simulated start failure")

// startWaitTimeout bounds every bounded wait in this file.
const startWaitTimeout = 5 * time.Second

// startCallRecorder records the generation identity current at the moment each Start call begins (currentGenerationOf(rt)), in call order,
// so a test can tell which connectLoop invocation actually dialed which generation,
// and detect an orphaned or doubly-dialed generation from the sequence alone.
type startCallRecorder struct {
	mu   sync.Mutex
	gens []uint64
}

func (r *startCallRecorder) record(gen uint64) {
	r.mu.Lock()
	r.gens = append(r.gens, gen)
	r.mu.Unlock()
}

func (r *startCallRecorder) snapshot() []uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]uint64(nil), r.gens...)
}

// waitThreeGens blocks (bounded by startWaitTimeout, via require.Eventually) until at least three generations have been recorded —
// every test in this file that reaches this call expects exactly gen 1, 2, and 3.
func (r *startCallRecorder) waitThreeGens(t *testing.T) []uint64 {
	t.Helper()

	const n = 3

	require.Eventually(t, func() bool { return len(r.snapshot()) >= n }, startWaitTimeout, time.Millisecond,
		"start-call recorder: timed out waiting for %d calls, have %v", n, r.snapshot())

	return r.snapshot()
}

// requireOneChain asserts the standard one-successful-chain outcome most tests in this file drive toward:
// gen 1, 2, and 3 each dialed exactly once, in order, and never more than one Start running at a time.
// Tests whose failure message needs scenario-specific wording keep their own inline assertions instead.
func requireOneChain(t *testing.T, rec *startCallRecorder, conc *concurrentStartTracker) {
	t.Helper()

	gens := rec.waitThreeGens(t)
	require.Equal(t, []uint64{1, 2, 3}, gens, "each Start call must dial its own generation exactly once, in order")
	require.Equal(t, int32(1), conc.max.Load(), "at most one Start may run at a time")
}

// startStep is one Start call's scripted behavior, given the ctx Start received and the TransportRuntime back-channel.
type startStep func(ctx context.Context, rt TransportRuntime) error

// concurrentStartTracker records how many Start calls are in flight at once,
// and the maximum ever observed — the "max concurrent Start == 1" assertion the two-loop bug would violate.
type concurrentStartTracker struct {
	active atomic.Int32
	max    atomic.Int32
}

func (c *concurrentStartTracker) enter() {
	n := c.active.Add(1)
	for {
		prevMax := c.max.Load()
		if n <= prevMax || c.max.CompareAndSwap(prevMax, n) {
			return
		}
	}
}

func (c *concurrentStartTracker) exit() { c.active.Add(-1) }

// withMockTransportSequence builds a mockScript that runs steps[i] for the (i+1)th Start call (1-indexed),
// recording the generation identity live at that call into rec first and the concurrent-Start high-water mark into conc
// (both optional — either may be nil), and repeats the LAST step for any further call beyond len(steps).
func withMockTransportSequence(rec *startCallRecorder, conc *concurrentStartTracker, steps ...startStep) mockScript {
	var calls atomic.Int32

	return func(ctx context.Context, rt TransportRuntime) error {
		n := int(calls.Add(1))
		if rec != nil {
			rec.record(currentGenerationOf(rt))
		}

		if conc != nil {
			conc.enter()
			defer conc.exit()
		}

		idx := n - 1
		if idx >= len(steps) {
			idx = len(steps) - 1
		}

		return steps[idx](ctx, rt)
	}
}

// connCloseTracker counts Close calls across every trackedConn built from it —
// used to verify every generation's socket is actually closed, not leaked by an orphaned generation.
type connCloseTracker struct{ closed atomic.Int32 }

// trackedConn is a fakeConn whose Close reports into a shared connCloseTracker.
type trackedConn struct {
	fakeConn
	tracker *connCloseTracker
}

func (c trackedConn) Close() error {
	c.tracker.closed.Add(1)

	return nil
}

// mkConn returns a fakeConn, or a trackedConn reporting into tracker when tracker is non-nil.
func mkConn(tracker *connCloseTracker) net.Conn {
	if tracker == nil {
		return fakeConn{}
	}

	return trackedConn{tracker: tracker}
}

// stepConnectAndSelect drives a full TCP-up + Select bring-up, exactly like withMockTransport, and returns nil.
func stepConnectAndSelect(tracker *connCloseTracker) startStep {
	return func(ctx context.Context, rt TransportRuntime) error {
		conn := mkConn(tracker)

		go func() {
			rt.TCPUp(conn)
			driveSelect(ctx, rt)
		}()

		return nil
	}
}

// stepTCPUpDropThenFail drives TCPUp then an immediate TCPDown (an involuntary drop before Select),
// waits for release to be closed (if non-nil), and returns errStartAfterTCPUpFail —
// modeling a custom transport whose Start fails after already reporting TCP-up and a subsequent drop.
func stepTCPUpDropThenFail(tracker *connCloseTracker, release <-chan struct{}) startStep {
	return func(_ context.Context, rt TransportRuntime) error {
		rt.TCPUp(mkConn(tracker))
		rt.TCPDown(errStartAfterTCPUpDrop)

		if release != nil {
			<-release
		}

		return errStartAfterTCPUpFail
	}
}

// stepTCPUpThenFail drives TCPUp only (no TCPDown — the FSM never leaves NotSelected on its own),
// waits for release (if non-nil), then returns errStartAfterTCPUpFail.
func stepTCPUpThenFail(tracker *connCloseTracker, release <-chan struct{}) startStep {
	return func(_ context.Context, rt TransportRuntime) error {
		rt.TCPUp(mkConn(tracker))

		if release != nil {
			<-release
		}

		return errStartAfterTCPUpFail
	}
}

// waitStartCalls is a bounded poll (never a bare time.Sleep) on the mock's cumulative Start count — a metric counter,
// the one case 300-testing.md allows require.Eventually-style polling for.
func waitStartCalls(t *testing.T, mt *mockTransport, n int) {
	t.Helper()
	require.Eventually(t, func() bool { return mt.startCalls() >= n }, startWaitTimeout, time.Millisecond,
		"expected at least %d Start calls, have %d", n, mt.startCalls())
}

// onceSignal is a channel closed exactly once via a sync.Once, safe to close from concurrent hooks.
type onceSignal struct {
	ch   chan struct{}
	once sync.Once
}

func newOnceSignal() *onceSignal { return &onceSignal{ch: make(chan struct{})} }
func (o *onceSignal) fire()      { o.once.Do(func() { close(o.ch) }) }

// guardedClose wraps ch's close in a sync.OnceFunc and registers a t.Cleanup for it,
// so a test failure or panic partway through (before the test's own normal release point)
// still releases whatever mock callback or hook is blocked on ch, instead of leaving it — and so Close's own join — blocked forever.
// t.Cleanup runs LIFO, so this must be called AFTER newLifeConn (whose own cleanup calls Close),
// so this release runs FIRST at cleanup time.
// The returned func is idempotent and safe to call again at the test's own normal release point.
func guardedClose(t *testing.T, ch chan struct{}) func() {
	t.Helper()

	release := sync.OnceFunc(func() { close(ch) })
	t.Cleanup(release)

	return release
}

func (o *onceSignal) wait(t *testing.T, msg string) {
	t.Helper()
	select {
	case <-o.ch:
	case <-time.After(startWaitTimeout):
		t.Fatal(msg)
	}
}

// waitBounded blocks (bounded by startWaitTimeout) until ch delivers a value, and returns it.
// It fails the test with msg on timeout instead of letting a raw, unbounded channel receive hang forever.
func waitBounded[T any](t *testing.T, ch <-chan T, msg string) T {
	t.Helper()

	select {
	case v := <-ch:
		return v
	case <-time.After(startWaitTimeout):
		t.Fatal(msg)

		var zero T

		return zero
	}
}

// installNthLoopSpawnSignal installs c.testHookLoopSpawned
// so the returned signal fires the instant the nth reconnect loop is spawned (1-indexed), counting every startConnectLoop call from installation onward.
// It must be installed before the spawn it means to count can possibly happen.
func installNthLoopSpawnSignal(c *connection, n int32) *onceSignal {
	sig := newOnceSignal()

	var count atomic.Int32

	c.testHookLoopSpawned = func() {
		if count.Add(1) == n {
			sig.fire()
		}
	}

	return sig
}

// installNthConnectLoopBarrierSignal installs c.testHookConnectLoopBarrier
// so the returned signal fires the instant the nth connectLoop invocation reaches the barrier check (1-indexed), counting every call from installation onward.
// A predecessor's OWN barrier check reaches this hook too —
// and resolves instantly whenever ITS predecessor already released its own startReturned —
// so a test after a single reconnect hop wants n=2
// (the FIRST call is that predecessor's own, uninteresting, already-resolved check; the SECOND is its successor's REAL one).
func installNthConnectLoopBarrierSignal(c *connection, n int32) *onceSignal {
	sig := newOnceSignal()

	var count atomic.Int32

	c.testHookConnectLoopBarrier = func() {
		if count.Add(1) == n {
			sig.fire()
		}
	}

	return sig
}

// TestReconnect_StartAfterTCPUpHandsOffWhenDropProcessedFirst drives a reconnect generation whose Start reports TCP-up, then a drop, then blocks —
// so the drop's own reaction has already spawned a successor loop by the time Start finally returns the failure.
// The failing Start must hand its retry off to that already-spawned loop rather than retry itself:
// exactly one successor chain, the failing loop never dials again or publishes a generation,
// at most one Start runs at a time, the reconnect gauge never reads 2, every generation's socket is closed,
// and the eventual recovery is counted exactly once.
func TestReconnect_StartAfterTCPUpHandsOffWhenDropProcessedFirst(t *testing.T) {
	rec := &startCallRecorder{}
	conc := &concurrentStartTracker{}
	conns := &connCloseTracker{}
	release := make(chan struct{})

	c, mt := newLifeConn(t, withMockTransportSequence(rec, conc,
		stepConnectAndSelect(conns),           // gen 1: normal bring-up
		stepTCPUpDropThenFail(conns, release), // gen 2: TCPUp -> TCPDown -> held -> error
		stepConnectAndSelect(conns),           // gen 3: the drop reaction's own successor
	))
	releaseFn := guardedClose(t, release) // safety net: never leave the mock's Start blocked past this test
	require.NoError(t, c.UpdateConfigOptions(WithT5(10*time.Millisecond)))

	require.NoError(t, c.Open(t.Context(), OpenBackground))
	requireSelected(t, c)

	// Spawn #1 is gen 1's own drop reaction (produces gen 2);
	// spawn #2 is gen 2's OWN TCPDown reaction (produces gen 3) — installed before EITHER can happen.
	secondSpawn := installNthLoopSpawnSignal(c, 2)

	mt.simulateReadError(errStartAfterTCPUpDrop) // gen 1 drops -> react spawns gen 2's connectLoop
	waitStartCalls(t, mt, 2)                     // gen 2's Start has begun and is now held

	secondSpawn.wait(t, "gen 2's own TCPDown must produce a second reaction spawn before its Start returns")

	require.Equal(t, int32(1), conc.max.Load(), "at most one Start may run at a time")
	require.Equal(t, int64(1), c.Metrics().Reconnecting(), "the gauge must read exactly 1 while gen 2's Start is held")

	releaseFn() // gen 2's Start now returns the failure; hand off to the already-spawned loop

	waitStartCalls(t, mt, 3)
	requireSelected(t, c)

	gens := rec.waitThreeGens(t)
	require.Equal(t, []uint64{1, 2, 3}, gens, "each Start call must dial its own generation exactly once, in order")
	require.Equal(t, int32(1), conc.max.Load(), "at most one Start may run at a time, across the whole schedule")

	// Both settle asynchronously, on gen 3's own connectLoop goroutine, strictly AFTER State() already reads Selected —
	// bounded, not immediate.
	require.Eventually(t, func() bool { return c.Metrics().Reconnecting() == 0 }, startWaitTimeout, time.Millisecond,
		"the gauge must settle back to 0 once gen 3 selects")
	require.Eventually(t, func() bool { return c.Metrics().Reconnects() == 1 }, startWaitTimeout, time.Millisecond,
		"the recovery is counted once, not twice")

	require.NoError(t, c.Close())
	require.Equal(t, int32(3), conns.closed.Load(), "every TCPUp'd socket must be closed after Close")
}

// TestReconnect_DelayedReactionStillTargetsTheDroppedGeneration holds the drop reaction between the FSM's own state store and its generation resolution (testHookReactBeforeCurLoad) —
// the window the ownership argument depends on being empty of any other publisher —
// for the reaction driven by gen 2's own hand-off report.
// It must still resolve gen 2 itself, never a successor, and still produce exactly one reconnect chain.
func TestReconnect_DelayedReactionStillTargetsTheDroppedGeneration(t *testing.T) {
	rec := &startCallRecorder{}
	conc := &concurrentStartTracker{}
	conns := &connCloseTracker{}

	c, mt := newLifeConn(t, withMockTransportSequence(rec, conc,
		stepConnectAndSelect(conns),       // gen 1: normal bring-up
		stepTCPUpDropThenFail(conns, nil), // gen 2: TCPUp -> TCPDown -> error immediately
		stepConnectAndSelect(conns),       // gen 3: the drop reaction's own successor
	))
	require.NoError(t, c.UpdateConfigOptions(WithT5(10*time.Millisecond)))

	require.NoError(t, c.Open(t.Context(), OpenBackground))
	requireSelected(t, c)

	// react fires once for gen 1's own drop (spawning gen 2's loop — must run normally)
	// and once for gen 2's drop (spawning gen 3's loop — the one this test holds).
	var reactCount atomic.Int32

	held := newOnceSignal()
	resume := make(chan struct{})
	resumeFn := guardedClose(t, resume) // safety net: never leave react blocked past this test
	c.testHookReactBeforeCurLoad = func() {
		if reactCount.Add(1) != 2 {
			return
		}

		held.fire()
		<-resume
	}

	mt.simulateReadError(errStartAfterTCPUpDrop) // gen 1 drops -> react (uncounted hold) spawns gen 2's loop
	waitStartCalls(t, mt, 2)                     // gen 2's Start runs, fails, and hands off

	held.wait(t, "gen 2's drop reaction must be held before it resolves its generation")

	// While react is held here, gen 2 must still be the live generation —
	// nothing else could have published a successor in this window (see the hook's own doc).
	pinned := c.cur.Load()
	require.NotNil(t, pinned)
	require.Equal(t, uint64(2), pinned.id, "gen 2 must still be the live generation while its own reaction is held")

	resumeFn() // let react resolve — it must resolve to the SAME gen 2, and spawn gen 3's loop

	waitStartCalls(t, mt, 3)
	requireSelected(t, c)

	gens := rec.waitThreeGens(t)
	require.Equal(t, []uint64{1, 2, 3}, gens, "exactly one chain: gen 3 is dialed once, for gen 2's own drop")
	require.Equal(t, int32(1), conc.max.Load(), "at most one Start may run at a time")

	require.NoError(t, c.Close())
}

// TestReconnect_StartAfterTCPUpNoTCPDownStillDropsAndRetries covers the side gap the ownership argument closes for a Start that never reports TCPDown at all:
// gen 2's Start drives TCP-up, then fails, with no TCPDown ever called.
// With no drop already in flight, connectLoop's own injectDisconnect is the ONLY possible drop producer —
// the FSM must still leave NotSelected for NotConnected (reported with CauseUnknown),
// and exactly one successor Start must follow.
func TestReconnect_StartAfterTCPUpNoTCPDownStillDropsAndRetries(t *testing.T) {
	rec := &startCallRecorder{}
	conc := &concurrentStartTracker{}
	conns := &connCloseTracker{}

	var mu sync.Mutex

	var drops []LifecycleEvent

	c, mt := newLifeConn(t, withMockTransportSequence(rec, conc,
		stepConnectAndSelect(conns),   // gen 1: normal bring-up
		stepTCPUpThenFail(conns, nil), // gen 2: TCPUp only -> error, no TCPDown
		stepConnectAndSelect(conns),   // gen 3: the drop reaction's own successor
	))
	require.NoError(t, c.UpdateConfigOptions(WithT5(10*time.Millisecond)))

	cancel := c.SubscribeLifecycle(func(ev LifecycleEvent) {
		if ev.Current == NotConnectedState {
			mu.Lock()
			drops = append(drops, ev)
			mu.Unlock()
		}
	})
	defer cancel()

	require.NoError(t, c.Open(t.Context(), OpenBackground))
	requireSelected(t, c)

	mt.simulateReadError(errStartAfterTCPUpDrop) // gen 1 drops -> react spawns gen 2's connectLoop

	waitStartCalls(t, mt, 3)
	requireSelected(t, c)

	requireOneChain(t, rec, conc)

	// The notifier delivers lifecycle events on its own goroutine, asynchronously from the FSM's own state store —
	// observing Selected does not by itself prove every prior drop has already been delivered to this subscription,
	// so wait for the delivery count as its own acknowledgement before taking the authoritative snapshot.
	snapshotDrops := func() []LifecycleEvent {
		mu.Lock()
		defer mu.Unlock()

		return append([]LifecycleEvent(nil), drops...)
	}
	require.Eventually(t, func() bool { return len(snapshotDrops()) >= 2 }, startWaitTimeout, time.Millisecond,
		"both drops must be delivered to the subscription")

	gotDrops := snapshotDrops()
	require.Len(t, gotDrops, 2, "gen 1's own drop and gen 2's own (TCPDown-less) drop, one each")
	last := gotDrops[len(gotDrops)-1]
	require.Equal(t, NotSelectedState, last.Previous, "gen 2 must be reported dropped from NotSelected — it never reached Selected")
	require.Equal(t, CauseUnknown, last.Cause, "gen 2's own report carries no classification")

	require.NoError(t, c.Close())
}

// TestReconnect_BlockedStartKeepsGaugeAtOneUntilBarrierReleases holds gen 2's Start blocked AFTER TCP-up and a drop,
// and confirms — while held — that gen 3's already-spawned successor loop has reached its barrier
// (acknowledged, not inferred from timing), that no successor Start has run,
// and that the reconnect gauge reads exactly 1, never 2.
// This is the increment-at-entry mutant's teeth: claiming the gauge before the barrier would let it read 2 here.
func TestReconnect_BlockedStartKeepsGaugeAtOneUntilBarrierReleases(t *testing.T) {
	rec := &startCallRecorder{}
	conc := &concurrentStartTracker{}
	conns := &connCloseTracker{}
	release := make(chan struct{})

	c, mt := newLifeConn(t, withMockTransportSequence(rec, conc,
		stepConnectAndSelect(conns),           // gen 1: normal bring-up
		stepTCPUpDropThenFail(conns, release), // gen 2: TCPUp -> TCPDown -> held -> error
		stepConnectAndSelect(conns),           // gen 3: the drop reaction's own successor
	))
	releaseFn := guardedClose(t, release) // safety net: never leave the mock's Start blocked past this test
	require.NoError(t, c.UpdateConfigOptions(WithT5(10*time.Millisecond)))

	require.NoError(t, c.Open(t.Context(), OpenBackground))
	requireSelected(t, c)

	// n=2: the FIRST call is gen 2's own barrier check (resolves instantly against gen 1's already-closed startReturned);
	// the SECOND is gen 3's REAL block.
	barrierEntered := installNthConnectLoopBarrierSignal(c, 2)

	mt.simulateReadError(errStartAfterTCPUpDrop) // gen 1 drops -> react spawns gen 2's connectLoop
	waitStartCalls(t, mt, 2)                     // gen 2's Start has begun and is now held

	// gen 2's own TCPDown already fired; react (running independently of the held Start) tears gen 2 down
	// and spawns gen 3's connectLoop, which reaches — and blocks at — gen 2's own barrier,
	// since gen 2's Start has not returned and so has not released it yet.
	barrierEntered.wait(t, "gen 3's connectLoop must reach the barrier before gen 2's Start is released")

	require.Equal(t, 2, mt.startCalls(), "no successor Start may run while the barrier holds")
	require.Equal(t, int64(1), c.Metrics().Reconnecting(), "the gauge must read exactly 1 while the barrier holds")

	releaseFn() // release gen 2's Start; its failure hand-off releases the barrier

	waitStartCalls(t, mt, 3)
	requireSelected(t, c)

	gens := rec.waitThreeGens(t)
	require.Equal(t, []uint64{1, 2, 3}, gens)
	require.Equal(t, int32(1), conc.max.Load(), "at most one Start may run at a time")
	// Settles asynchronously, on gen 3's own connectLoop goroutine, strictly AFTER State() already reads Selected.
	require.Eventually(t, func() bool { return c.Metrics().Reconnecting() == 0 }, startWaitTimeout, time.Millisecond,
		"the gauge must settle back to 0 once gen 3 selects")

	require.NoError(t, c.Close())
}

// TestOpen_ColdBranchLoopPostTCPUpFailureStillCountsRecovery drives Open's cold-active background retry
// (a first dial that fails cold, never driving TCP-up) into its OWN retry attempt reporting TCP-up, then a drop, then failing —
// the same hand-off shape as the post-Selected reconnect tests above, but originating from a countReconnect=false lineage.
// It covers all four combinations of the two axes that shape:
// whether the cold retry's own drop is processed by the FSM before or after its Start call returns (matching the
// TestReconnect_StartAfterTCPUpHandsOff... schedules above), and whether that generation ever reached Selected before dropping.
// In every case the eventual recovery is counted exactly once — the drop reaction's own loop always counts,
// regardless of the failed attempt's own lineage — and the exact backoff delay sequence is pinned:
// the cold retry's own attempt sleeps the configured initial delay,
// and the hand-off's own successor sleeps initial*multiplier when the failing generation never reached Selected,
// or resets to initial when it did.
func TestOpen_ColdBranchLoopPostTCPUpFailureStillCountsRecovery(t *testing.T) {
	cases := []struct {
		name              string
		startReturnsFirst bool
		reachSelected     bool
	}{
		{"drop processed first, never selected", false, false},
		{"drop processed first, reached Selected", false, true},
		{"Start returns first, never selected", true, false},
		{"Start returns first, reached Selected", true, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &startCallRecorder{}
			conc := &concurrentStartTracker{}
			conns := &connCloseTracker{}

			release := make(chan struct{})
			var releaseFn func()
			if !tc.startReturnsFirst {
				releaseFn = guardedClose(t, release) // safety net: never leave the mock's Start blocked past this test
			}

			reachedPause := newOnceSignal()
			resumePause := make(chan struct{})
			var resumePauseFn func()
			if tc.startReturnsFirst {
				resumePauseFn = guardedClose(t, resumePause) // safety net: never leave the supervisor blocked past this test
			}

			coldRetryStep := func(ctx context.Context, rt TransportRuntime) error {
				if tc.startReturnsFirst {
					runtimeConnection(rt).sup.Load().testHookAfterStateLoad = func(ev fsmEvent) {
						if ev != evDisconnect {
							return
						}

						reachedPause.fire()
						<-resumePause
					}
				}

				rt.TCPUp(mkConn(conns))
				if tc.reachSelected {
					driveSelect(ctx, rt)
				}

				rt.TCPDown(errStartAfterTCPUpDrop)

				if !tc.startReturnsFirst {
					<-release
				}

				return errStartAfterTCPUpFail
			}

			c, mt := newLifeConn(t, withMockTransportSequence(rec, conc,
				func(_ context.Context, _ TransportRuntime) error { return errStartAfterTCPUpFail }, // call 1: cold dial failure, no TCP-up
				coldRetryStep,               // call 2: the cold loop's own retry
				stepConnectAndSelect(conns), // call 3: the drop reaction's own successor
			))
			smallBackoffConfig(t, c) // (10ms, x2, T5=1s): small enough to run fast, large enough that T5 never clamps the distinguishing multiplier away

			br := newBackoffRecorder()
			c.testHookBackoff = br.record

			// n=2: spawn #1 is Open's own cold-branch spawn of call 2's loop;
			// spawn #2 is that generation's own drop reaction, which spawns call 3's loop —
			// installed before Open ever runs, so it counts both.
			spawned := installNthLoopSpawnSignal(c, 2)

			require.NoError(t, c.Open(t.Context(), OpenBackground))

			if tc.startReturnsFirst {
				reachedPause.wait(t, "the cold retry's own drop report must reach the FSM before it is allowed to proceed")

				require.Eventually(t, func() bool {
					e := c.cur.Load()

					return e != nil && e.id == 2 && e.ended.Load()
				}, startWaitTimeout, time.Millisecond, "the cold retry's own generation must be latched ended by its own teardown before the FSM ever sees its drop")

				resumePauseFn()
			} else {
				spawned.wait(t, "the cold retry's own drop must spawn the reaction's own loop before its Start is released")
				releaseFn()
			}

			waitStartCalls(t, mt, 3)
			requireSelected(t, c)

			requireOneChain(t, rec, conc)

			// Both settle asynchronously, on the successor's own connectLoop goroutine, strictly AFTER State() already reads Selected.
			require.Eventually(t, func() bool { return c.Metrics().Reconnects() == 1 }, startWaitTimeout, time.Millisecond,
				"the drop reaction's own loop always counts the recovery, regardless of the failed attempt's own lineage")
			require.Eventually(t, func() bool { return c.Metrics().Reconnecting() == 0 }, startWaitTimeout, time.Millisecond,
				"the gauge must settle back to 0")

			// Both hooked delays are guaranteed recorded by now:
			// each one fires strictly before the Start call it precedes,
			// and both those Start calls (2 and 3) have already happened.
			got := br.snapshot()
			second := 20 * time.Millisecond // initial(10ms) * multiplier(2): the ramp continues
			if tc.reachSelected {
				second = 10 * time.Millisecond // reset to initial: the failing generation reached Selected
			}
			require.Equal(t, []time.Duration{10 * time.Millisecond, second}, got,
				"the cold retry's own first sleep, then the hand-off's own successor's sleep")

			require.NoError(t, c.Close())
		})
	}
}

// TestOpen_InitialStartAfterTCPUpFailsDropProcessedFirst drives Open's OWN first Start reporting TCP-up, then a drop, then blocking —
// so the drop's own reaction may spawn a stray reconnect loop (acknowledged via the startConnectLoop hook)
// BEFORE Open's own rollback ever sets shutdown — before Start finally returns the failure.
// Open must still return the Start error, no loop may remain once it does,
// and a leaked loop parked in its configured backoff must not delay the next Open.
//
// The configured backoff is deliberately SMALL (2s), not the extreme value an earlier version of this test used,
// and BOTH Open calls below are bounded tightly (500ms), far under that backoff:
// a correct rollback interrupts the stray loop's backoff via reconnectCancel almost instantly,
// so Open itself — not just a LATER Open — returns well inside 500ms in the correct implementation.
// The mutant this guards against (rollback that forgets to close reconnectCancel)
// leaves the stray loop sleeping out the FULL backoff before its own shutdown fence lets it exit —
// that delay actually surfaces INSIDE rollbackFailedOpen's own connectLoopWg.Wait(),
// so it is THIS Open call, not the next one, that is slow under the mutant.
// A loose bound here (the package's own default test timeout, or even this file's usual multi-second startWaitTimeout)
// would let a 2s stray sleep pass unnoticed, and an hour-long one used to be caught only by the whole package's test-run timeout —
// a slow, indirect signal whose failure message names no cause,
// and whose cleanup (Close(), needing the same lifeMu this leaked goroutine holds) hangs for just as long.
// The tight bounds here fail fast, with a clear message,
// and keep cleanup itself bounded to the small configured backoff even when the mutation is present.
func TestOpen_InitialStartAfterTCPUpFailsDropProcessedFirst(t *testing.T) {
	rec := &startCallRecorder{}
	conc := &concurrentStartTracker{}
	conns := &connCloseTracker{}
	release := make(chan struct{})

	c, _ := newLifeConn(t, withMockTransportSequence(rec, conc,
		stepTCPUpDropThenFail(conns, release), // gen 1 (Open's own): TCPUp -> TCPDown -> held -> error
	))
	releaseFn := guardedClose(t, release) // safety net: never leave the mock's Start blocked past this test
	require.NoError(t, c.UpdateConfigOptions(WithReconnectBackoff(2*time.Second, 1), WithT5(2*time.Second)))

	spawned := installNthLoopSpawnSignal(c, 1)

	openErr := make(chan error, 1)
	go func() { openErr <- c.Open(t.Context(), OpenBackground) }()

	spawned.wait(t, "the drop reaction must spawn a stray loop for gen 1's drop before Start is released")

	releaseFn() // Open's own Start now returns the failure

	select {
	case err := <-openErr:
		require.ErrorIs(t, err, errStartAfterTCPUpFail, "Open must return the tr.Start failure")
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Open did not return within 500ms: its own rollback left a stray loop parked in its own backoff")
	}

	require.Equal(t, NotConnectedState, c.State())
	require.Equal(t, int64(0), c.Metrics().Reconnecting(), "no loop may remain once Open has returned")

	// A leaked loop parked in its configured backoff must not delay the next Open either.
	nextOpenDone := make(chan error, 1)
	go func() { nextOpenDone <- c.Open(t.Context(), OpenBackground) }()

	select {
	case err := <-nextOpenDone:
		_ = err // whichever way the second Open resolves is not this test's concern
	case <-time.After(500 * time.Millisecond):
		t.Fatal("the next Open did not return within 500ms: a stray loop from the first Open is still parked in its own backoff")
	}

	require.NoError(t, c.Close())
}

// TestOpen_InitialStartAfterTCPUpFailsStartReturnsFirst forces the OPPOSITE schedule from the test above:
// gen 1's own TCPDown report is paused at the FSM
// (testHookAfterStateLoad, installed from inside the Start callback itself, race-free via the TCPDown report's own happens-before edge)
// until AFTER Open's rollback has already set shutdown — so no stray loop can ever be spawned for this drop.
// Open must still return the Start error, and no loop may remain once it does.
func TestOpen_InitialStartAfterTCPUpFailsStartReturnsFirst(t *testing.T) {
	rec := &startCallRecorder{}
	conc := &concurrentStartTracker{}
	conns := &connCloseTracker{}

	reachedPause := newOnceSignal()
	resumePause := make(chan struct{})

	gen1 := func(_ context.Context, rt TransportRuntime) error {
		runtimeConnection(rt).sup.Load().testHookAfterStateLoad = func(ev fsmEvent) {
			if ev != evDisconnect {
				return
			}

			reachedPause.fire()
			<-resumePause
		}

		rt.TCPUp(mkConn(conns))
		rt.TCPDown(errStartAfterTCPUpDrop)

		return errStartAfterTCPUpFail
	}

	c, _ := newLifeConn(t, withMockTransportSequence(rec, conc, gen1))
	resumeFn := guardedClose(t, resumePause) // safety net: never leave the supervisor blocked past this test
	require.NoError(t, c.UpdateConfigOptions(WithReconnectBackoff(time.Hour, 1), WithT5(time.Hour)))

	openErr := make(chan error, 1)
	go func() { openErr <- c.Open(t.Context(), OpenBackground) }()

	reachedPause.wait(t, "gen 1's own TCPDown report must reach the FSM before it is allowed to proceed")

	require.Eventually(t, func() bool { return c.shutdown.Load() }, startWaitTimeout, time.Millisecond,
		"Open's rollback must set shutdown before the paused drop is ever applied")

	require.Never(t, func() bool {
		select {
		case <-openErr:
			return true
		default:
			return false
		}
	}, 200*time.Millisecond, 5*time.Millisecond, "Open must not return before its own rollback join completes")

	resumeFn() // let the FSM finally process gen 1's drop — shutdown is already set, so react spawns nothing

	select {
	case err := <-openErr:
		require.ErrorIs(t, err, errStartAfterTCPUpFail, "Open must return the tr.Start failure")
	case <-time.After(startWaitTimeout):
		t.Fatal("Open did not return")
	}

	require.Equal(t, NotConnectedState, c.State())
	require.Equal(t, int64(0), c.Metrics().Reconnecting(), "no loop may remain once Open has returned")

	require.NoError(t, c.Close())
}

// TestOpen_RollbackWaitsForStrayLoopToExit reuses TestOpen_InitialStartAfterTCPUpFailsDropProcessedFirst's scenario
// (a stray reconnect loop the race spawns before Open's own rollback ever set shutdown) and holds that loop in its own exit hook, asserting Open has NOT returned while it is held —
// the rollback's own connectLoopWg.Wait() must block on it —
// then releases it and confirms Open returns.
//
// A single immediate, nonblocking read of openErr right after the stray loop reaches its exit hook cannot tell
// "the rollback is correctly blocked in connectLoopWg.Wait()"
// apart from "the rollback simply has not been scheduled that far yet" —
// a mutant that drops the Wait() call entirely could still pass a check that never gives it any time to actually run.
// This test closes that gap:
// it first confirms the supervisor THIS Open call built has already stopped (runDone)
// and its notifier has already joined (notifierDone) —
// the two steps rollbackFailedOpen performs BEFORE its own connectLoopWg.Wait() —
// so the only thing left that could still be keeping Open from returning is that Wait() call itself,
// and only then asserts Open's non-return over a real, repeatedly sampled window (require.Never),
// not a single snapshot.
func TestOpen_RollbackWaitsForStrayLoopToExit(t *testing.T) {
	rec := &startCallRecorder{}
	conc := &concurrentStartTracker{}
	conns := &connCloseTracker{}
	release := make(chan struct{})

	c, _ := newLifeConn(t, withMockTransportSequence(rec, conc,
		stepTCPUpDropThenFail(conns, release), // gen 1 (Open's own): TCPUp -> TCPDown -> held -> error
	))
	releaseFn := guardedClose(t, release) // safety net: never leave the mock's Start blocked past this test
	// A small, not extreme, configured backoff (see TestOpen_InitialStartAfterTCPUpFailsDropProcessedFirst's own doc):
	// this test holds the stray loop at its exit hook regardless of the backoff's length,
	// but keeping it small means a mutation that fails to interrupt the loop's sleep still resolves —
	// and this test's own cleanup still completes — in bounded time, not up to an hour.
	require.NoError(t, c.UpdateConfigOptions(WithReconnectBackoff(2*time.Second, 1), WithT5(2*time.Second)))

	spawned := installNthLoopSpawnSignal(c, 1)

	loopExitReached := newOnceSignal()
	resumeLoopExit := make(chan struct{})
	resumeLoopExitFn := guardedClose(t, resumeLoopExit) // safety net: never leave the stray loop blocked past this test
	c.testHookLoopExit = func() {
		loopExitReached.fire()
		<-resumeLoopExit
	}

	openErr := make(chan error, 1)
	go func() { openErr <- c.Open(t.Context(), OpenBackground) }()

	spawned.wait(t, "the drop reaction must spawn a stray loop for gen 1's drop before Start is released")

	// Captured BEFORE Start is released: the supervisor THIS Open call built,
	// so its runDone/notifierDone below can only be the ones rollbackFailedOpen itself joins.
	s := c.sup.Load()
	require.NotNil(t, s)

	releaseFn() // Open's own Start now returns the failure; its rollback fences the stray loop

	loopExitReached.wait(t, "the stray loop must reach its exit hook before this test proceeds")

	select {
	case <-s.runDone:
	case <-time.After(startWaitTimeout):
		t.Fatal("the rollback's own supervisor must stop before this test proceeds")
	}

	select {
	case <-s.notifierDone:
	case <-time.After(startWaitTimeout):
		t.Fatal("the rollback's own notifier must join before this test proceeds")
	}

	// Both joins rollbackFailedOpen performs before connectLoopWg.Wait() have already completed,
	// so the ONLY thing that can still be keeping Open from returning is that Wait() call itself,
	// blocked on the stray loop this test is holding at its exit hook.
	require.Never(t, func() bool {
		select {
		case <-openErr:
			return true
		default:
			return false
		}
	}, 200*time.Millisecond, 5*time.Millisecond, "Open must not return while the stray loop it must join is still held")

	resumeLoopExitFn() // let the stray loop actually exit

	select {
	case err := <-openErr:
		require.ErrorIs(t, err, errStartAfterTCPUpFail, "Open must return the tr.Start failure")
	case <-time.After(startWaitTimeout):
		t.Fatal("Open did not return")
	}

	require.Equal(t, NotConnectedState, c.State())
	require.Equal(t, int64(0), c.Metrics().Reconnecting(), "no loop may remain once Open has returned")

	require.NoError(t, c.Close())
}

// TestReconnect_HandoffInjectionAfterSuccessorIsCurrentIsStale exercises testHookBeforeHandoffInject —
// the hand-off's own late report seam, installed by no other test in this file —
// by holding it until gen 3 (the reaction's own successor,
// spawned by gen 2's own TCPDown well before gen 2's Start ever returns) has already run all the way to Selected:
// gen 2's own barrier has already released by the time this seam fires (see the field doc),
// so an already-unblocked gen 3 is free to run to completion while the injection is held.
// Forcing gen 3 past NotConnected before releasing matters:
// a report for gen 2 arriving while gen 3 sat AT NotConnected would be silently absorbed by the transition table alone
// (evDisconnect is illegal from NotConnected),
// which would pass this test even if the generation checks the report actually depends on were missing.
// With gen 3 already past NotConnected, the late report can only be discarded because it names a generation that is no longer current:
// injectDisconnect's own pre-check catches it first, with supervisor.step's own generation match as a second, redundant layer —
// either one alone is enough to keep this scenario's outcome the same,
// so staleGen increments by exactly one and the whole scenario reports exactly two NotConnected lifecycle events —
// gen 1's own drop and gen 2's own drop — never a third one the late report would otherwise manufacture.
// The seam's OWN placement is what this test actually discriminates, not which generation check catches the stale report:
// holding the hook one statement earlier, before close(e.startReturned) releases gen 3's barrier,
// leaves gen 3 permanently unable to reach Selected — this test's own waitStartCalls(t, mt, 3) then times out —
// which is how a wrongly-placed hook (or a hook that fired too early to prove anything) would be caught instead.
func TestReconnect_HandoffInjectionAfterSuccessorIsCurrentIsStale(t *testing.T) {
	rec := &startCallRecorder{}
	conc := &concurrentStartTracker{}
	conns := &connCloseTracker{}

	c, mt := newLifeConn(t, withMockTransportSequence(rec, conc,
		stepConnectAndSelect(conns),       // gen 1: normal bring-up
		stepTCPUpDropThenFail(conns, nil), // gen 2: TCPUp -> TCPDown -> error immediately
		stepConnectAndSelect(conns),       // gen 3: the drop reaction's own successor
	))
	require.NoError(t, c.UpdateConfigOptions(WithT5(10*time.Millisecond)))

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

	held := newOnceSignal()
	resume := make(chan struct{})
	resumeFn := guardedClose(t, resume) // safety net: never leave the hand-off blocked past this test
	c.testHookBeforeHandoffInject = func() {
		held.fire()
		<-resume
	}

	require.NoError(t, c.Open(t.Context(), OpenBackground))
	requireSelected(t, c)

	staleBefore := c.sup.Load().staleGen.Load()

	mt.simulateReadError(errStartAfterTCPUpDrop) // gen 1 drops -> react spawns gen 2's connectLoop

	held.wait(t, "gen 2's hand-off must reach the injection seam before this test proceeds")

	waitStartCalls(t, mt, 3) // gen 3, already unblocked, has been dialed
	requireSelected(t, c)    // and has run all the way past NotConnected to Selected

	resumeFn() // release the late, now-stale injectDisconnect(gen 2, ...) call

	// Bounded: the release above lets connectLoopStartFailure's own goroutine actually call injectDisconnect and return,
	// which is the only thing left for this wait to observe.
	require.Eventually(t, func() bool {
		return c.sup.Load().staleGen.Load() > staleBefore
	}, startWaitTimeout, time.Millisecond, "the late hand-off report must be counted as stale")

	require.Equal(t, staleBefore+1, c.sup.Load().staleGen.Load(), "exactly one stale report, never more")

	// The notifier delivers lifecycle events on its own goroutine, asynchronously from the FSM's own state store,
	// so wait for the delivery count as its own acknowledgement before taking the authoritative snapshot —
	// the late report being stale (just proven above)
	// is what makes a bounded wait for >= 2, followed by an exact-length assertion, safe:
	// no third can ever land.
	snapshotDrops := func() []LifecycleEvent {
		mu.Lock()
		defer mu.Unlock()

		return append([]LifecycleEvent(nil), drops...)
	}
	require.Eventually(t, func() bool { return len(snapshotDrops()) >= 2 }, startWaitTimeout, time.Millisecond,
		"both drops must be delivered to the subscription")

	gotDrops := snapshotDrops()
	require.Len(t, gotDrops, 2, "gen 1's own drop and gen 2's own drop, and nothing the late report manufactures")

	require.NoError(t, c.Close())
}

// TestReconnect_ConcurrentCloseDuringHandoffOrphansNothing drives a Close concurrently with gen 2's held Start,
// right after gen 3's successor loop has already reached its barrier (spawned by gen 2's own drop reaction).
// connectLoopWg still counts gen 2's own connectLoop invocation,
// so Close is bounded BY that held Start returning — it must not complete before release, and must complete once it does.
// No reconnect may ever follow, and the connection must settle at NotConnected with no orphaned generation.
func TestReconnect_ConcurrentCloseDuringHandoffOrphansNothing(t *testing.T) {
	rec := &startCallRecorder{}
	conc := &concurrentStartTracker{}
	conns := &connCloseTracker{}
	release := make(chan struct{})

	c, mt := newLifeConn(t, withMockTransportSequence(rec, conc,
		stepConnectAndSelect(conns),           // gen 1: normal bring-up
		stepTCPUpDropThenFail(conns, release), // gen 2: TCPUp -> TCPDown -> held -> error
		stepConnectAndSelect(conns),           // gen 3: would-be successor (must never be dialed)
	))
	releaseFn := guardedClose(t, release) // safety net: never leave the mock's Start blocked past this test
	require.NoError(t, c.UpdateConfigOptions(WithT5(10*time.Millisecond)))

	require.NoError(t, c.Open(t.Context(), OpenBackground))
	requireSelected(t, c)

	// n=2: the FIRST call is gen 2's own barrier check (resolves instantly against gen 1's already-closed startReturned);
	// the SECOND is gen 3's REAL block.
	barrierEntered := installNthConnectLoopBarrierSignal(c, 2)

	mt.simulateReadError(errStartAfterTCPUpDrop) // gen 1 drops -> react spawns gen 2's connectLoop
	waitStartCalls(t, mt, 2)                     // gen 2's Start has begun and is now held

	barrierEntered.wait(t, "gen 3's connectLoop must reach the barrier before Close")

	closeErr := make(chan error, 1)
	go func() { closeErr <- c.Close() }()

	// Close's own connectLoopWg.Wait() still counts gen 2's own connectLoop invocation
	// (blocked inside its held Start call), so Close cannot complete until that Start returns;
	// give Close a moment to actually reach that join before asserting it has not raced ahead somehow.
	require.Eventually(t, func() bool { return c.shutdown.Load() }, startWaitTimeout, time.Millisecond,
		"Close must fence reconnect before it can be blocked on the held Start's own connectLoop invocation")

	require.Never(t, func() bool {
		select {
		case <-closeErr:
			return true
		default:
			return false
		}
	}, 200*time.Millisecond, 5*time.Millisecond,
		"Close must not complete before gen 2's held Start returns — it owns a connectLoopWg share")

	releaseFn() // release gen 2's Start; Close can now complete

	select {
	case err := <-closeErr:
		require.NoError(t, err, "Close must succeed once the held Start returns")
	case <-time.After(startWaitTimeout):
		t.Fatal("Close must not hang past the held Start actually returning")
	}

	require.Equal(t, 2, mt.startCalls(), "no reconnect may ever follow a Close that already fenced the hand-off")
	require.Equal(t, NotConnectedState, c.State())
	require.Equal(t, int64(0), c.Metrics().Reconnecting())
	require.Equal(t, int32(2), conns.closed.Load(), "every TCPUp'd socket (gen 1 and gen 2; gen 3 was never dialed) must be closed")

	require.NoError(t, c.Close()) // idempotent re-Close
}

// TestTCPUpCommitGate_MarkerPlacement drives the real connection.tcpUpCommitGate directly
// (the same technique TestCommitGate_FencesTheCASAgainstMarkEnded uses for commitGate),
// parking a supplied CAS closure INSIDE the gate's RLock section to prove two things a fixed-answer test double cannot:
// a concurrent teardown-and-republish cannot complete while a commit is mid-CAS,
// and the marker this gate sets lands on the EXACT generation the RLock section resolved —
// never a successor a re-read of c.cur after the unlock could otherwise redirect it onto.
func TestTCPUpCommitGate_MarkerPlacement(t *testing.T) {
	t.Run("a successful commit's marker lands on e, never a republished successor", func(t *testing.T) {
		c, _ := newLifeConn(t, mockScript(func(_ context.Context, _ TransportRuntime) error { return nil }))
		require.NoError(t, c.Open(t.Context(), OpenBackground))

		e := c.cur.Load()
		require.NotNil(t, e)
		require.NotZero(t, e.id)

		inCAS := make(chan struct{})
		release := make(chan struct{})
		releaseFn := guardedClose(t, release) // safety net: never leave the parked commit blocked past this test
		gateDone := make(chan struct{})

		var committed, live bool
		go func() {
			committed, live = c.tcpUpCommitGate(e.id, func() bool {
				close(inCAS)
				<-release

				return true
			})
			close(gateDone)
		}()

		waitBounded(t, inCAS, "the parked commit must reach its CAS closure")

		// A concurrent teardown-and-republish must not complete while this commit is mid-CAS —
		// the same RLock-vs-Lock exclusion TestCommitGate_FencesTheCASAgainstMarkEnded proves for commitGate.
		republished := make(chan *epoch, 1)
		go func() {
			e.teardown(time.Second)
			_ = e.wait()

			e2 := newEpoch(t.Context(), c.cfg.Load().logger, 1)
			e2.id = e.id + 1
			c.cur.Store(e2)
			republished <- e2
		}()

		require.Never(t, func() bool {
			select {
			case <-republished:
				return true
			default:
				return false
			}
		}, 200*time.Millisecond, 5*time.Millisecond,
			"a generation must not be able to end and be replaced while a commit it admitted is still inside its CAS")

		releaseFn()
		waitBounded(t, gateDone, "the parked commit's own gate call must return")

		e2 := waitBounded(t, republished, "the concurrent teardown-and-republish must complete")

		require.True(t, live)
		require.True(t, committed)
		require.True(t, e.tcpUpCommitted.Load(), "the marker must land on the generation the RLock section resolved")
		require.False(t, e2.tcpUpCommitted.Load(), "the marker must never land on the republished successor")

		require.NoError(t, c.Close())
	})

	t.Run("a successful gen-0 commit's marker also lands on e, never a republished successor", func(t *testing.T) {
		// The same technique and the same proof as the named-generation subtest above,
		// but for gen 0 (SECS-I's and an out-of-module transport's own call shape) —
		// gen 0 skips only the identity comparison,
		// so it must be admitted, resolved, and marked on the SAME live epoch the RLock section actually resolved,
		// exactly like a named report.
		c, _ := newLifeConn(t, mockScript(func(_ context.Context, _ TransportRuntime) error { return nil }))
		require.NoError(t, c.Open(t.Context(), OpenBackground))

		e := c.cur.Load()
		require.NotNil(t, e)
		require.NotZero(t, e.id)

		inCAS := make(chan struct{})
		release := make(chan struct{})
		releaseFn := guardedClose(t, release) // safety net: never leave the parked commit blocked past this test
		gateDone := make(chan struct{})

		var committed, live bool
		go func() {
			committed, live = c.tcpUpCommitGate(0, func() bool {
				close(inCAS)
				<-release

				return true
			})
			close(gateDone)
		}()

		waitBounded(t, inCAS, "the parked commit must reach its CAS closure")

		republished := make(chan *epoch, 1)
		go func() {
			e.teardown(time.Second)
			_ = e.wait()

			e2 := newEpoch(t.Context(), c.cfg.Load().logger, 1)
			e2.id = e.id + 1
			c.cur.Store(e2)
			republished <- e2
		}()

		require.Never(t, func() bool {
			select {
			case <-republished:
				return true
			default:
				return false
			}
		}, 200*time.Millisecond, 5*time.Millisecond,
			"a generation must not be able to end and be replaced while a gen-0 commit it admitted is still inside its CAS")

		releaseFn()
		waitBounded(t, gateDone, "the parked commit's own gate call must return")

		e2 := waitBounded(t, republished, "the concurrent teardown-and-republish must complete")

		require.True(t, live)
		require.True(t, committed)
		require.True(t, e.tcpUpCommitted.Load(), "the marker must land on the generation the RLock section resolved")
		require.False(t, e2.tcpUpCommitted.Load(), "the marker must never land on the republished successor")

		require.NoError(t, c.Close())
	})

	t.Run("gen 0 on an already-ended generation is refused, with no marker anywhere", func(t *testing.T) {
		c, _ := newLifeConn(t, mockScript(func(_ context.Context, _ TransportRuntime) error { return nil }))
		require.NoError(t, c.Open(t.Context(), OpenBackground))

		e := c.cur.Load()
		require.NotNil(t, e)

		e.markEnded() // teardown wins the gate first

		var casRan bool
		committed, live := c.tcpUpCommitGate(0, func() bool {
			casRan = true

			return true
		})

		require.False(t, live, "an ended generation is no longer live, even unnamed (gen 0)")
		require.False(t, committed)
		require.False(t, casRan, "the CAS must never run for a refused commit")
		require.False(t, e.tcpUpCommitted.Load())
		require.False(t, e.tcpUpAdmitted.Load(), "admission itself must never be taken for a refused commit")
		require.Equal(t, NotConnectedState, c.State(), "the FSM must stay untouched")

		require.NoError(t, c.Close())
	})

	t.Run("a republish immediately after the gate returns cannot steal the marker", func(t *testing.T) {
		c, _ := newLifeConn(t, mockScript(func(_ context.Context, _ TransportRuntime) error { return nil }))
		require.NoError(t, c.Open(t.Context(), OpenBackground))

		e := c.cur.Load()
		require.NotNil(t, e)
		require.NotZero(t, e.id)

		reachedEnqueue := make(chan struct{})
		resumeEnqueue := make(chan struct{})
		resumeFn := guardedClose(t, resumeEnqueue) // safety net: never leave the parked commit blocked past this test

		// commitFrom reads this hook on the reporting goroutine itself (the one this subtest spawns below),
		// never on run(), so installing it here — before that goroutine starts — is race-free by construction.
		sup := c.sup.Load()
		sup.testHookBeforeEnqueue = func(gen uint64, ev fsmEvent, _ TransitionCause) (skipEnqueue bool) {
			if ev != evTCPUp || gen != e.id {
				return false
			}

			sup.testHookBeforeEnqueue = nil
			close(reachedEnqueue)
			<-resumeEnqueue

			return false
		}

		commitDone := make(chan struct{})
		go func() {
			c.TCPUpFromGeneration(e.id, mkConn(nil))
			close(commitDone)
		}()

		waitBounded(t, reachedEnqueue, "the parked commit must reach the pre-enqueue seam")

		// The gate itself has already returned by this point — its RLock section is closed —
		// so a correct implementation has already settled the marker on e; only a marker deferred past the gate,
		// and re-resolved against whatever generation is current when it finally runs, could still be redirected by what follows.
		e.teardown(time.Second)
		_ = e.wait()

		e2 := newEpoch(t.Context(), c.cfg.Load().logger, 1)
		e2.id = e.id + 1
		c.cur.Store(e2)

		resumeFn()
		waitBounded(t, commitDone, "the parked commit's own TCPUpFromGeneration call must return")

		require.True(t, e.tcpUpCommitted.Load(), "the marker belongs to the generation the gate resolved, not whichever one is current once enqueue finally runs")
		require.False(t, e2.tcpUpCommitted.Load(), "a republish that lands after the gate returns must never inherit the marker")

		require.NoError(t, c.Close())
	})
}

// TestReconnect_ConcurrentTCPUpDuringFailingStartWaitsForMarker parks a concurrent transport goroutine's TCP-up commit inside tcpUpCommitGate's RLock section —
// its state CAS has already succeeded, but its marker store has not —
// while gen 2's own Start call returns a failure on a SEPARATE goroutine.
// An acknowledgement fires the instant the failure branch is reached (before its own teardown),
// proving Start really has returned while the marker is still unset — an early-read mutant would already see false here.
// The teardown that follows must still block on the parked commit (it needs the same write lock),
// so by the time the failure branch reads e.tcpUpCommitted, the marker is settled:
// the retiring invocation publishes nothing, the reaction-owned invocation retries, and there is exactly one chain.
func TestReconnect_ConcurrentTCPUpDuringFailingStartWaitsForMarker(t *testing.T) {
	rec := &startCallRecorder{}
	conc := &concurrentStartTracker{}
	conns := &connCloseTracker{}

	inGate := newOnceSignal()
	releaseGate := make(chan struct{})
	ackBeforeTeardown := newOnceSignal()

	gen2 := func(_ context.Context, rt TransportRuntime) error {
		c := runtimeConnection(rt)
		c.testHookTCPUpCommitBeforeMarker = func() {
			inGate.fire()
			<-releaseGate
		}
		c.testHookConnectLoopBeforeFailureTeardown = func() {
			ackBeforeTeardown.fire()
		}

		go rt.TCPUp(mkConn(conns)) // a separate "transport" goroutine drives the commit

		<-inGate.ch // wait for the commit to actually reach the gate before returning the failure

		return errStartAfterTCPUpFail
	}

	c, mt := newLifeConn(t, withMockTransportSequence(rec, conc,
		stepConnectAndSelect(conns), // gen 1: normal bring-up
		gen2,                        // gen 2: a concurrent TCP-up commit parked mid-gate, then error
		stepConnectAndSelect(conns), // gen 3: the drop reaction's own successor
	))
	// Safety net: never leave the parked commit blocked (holding genGate.RLock) past this test —
	// every teardown, including Close's own, needs the write lock this would otherwise starve forever.
	releaseGateFn := guardedClose(t, releaseGate)
	require.NoError(t, c.UpdateConfigOptions(WithT5(10*time.Millisecond)))

	// Two spawns are the real proof of a hand-off: spawn #1 is gen 1's own drop reaction (produces gen 2);
	// spawn #2 is gen 2's OWN hand-off (produces gen 3).
	// An epoch id sequence alone cannot tell a genuine hand-off apart from gen 2 retrying itself —
	// a self-retry mints the very same next id off the same monotonic counter, with no second spawn at all.
	var spawnCount atomic.Int32
	c.testHookLoopSpawned = func() { spawnCount.Add(1) }

	require.NoError(t, c.Open(t.Context(), OpenBackground))
	requireSelected(t, c)

	mt.simulateReadError(errStartAfterTCPUpDrop) // gen 1 drops -> react spawns gen 2's connectLoop

	ackBeforeTeardown.wait(t, "the failure branch must be reached — Start returned — before the parked commit is released")

	releaseGateFn() // the parked commit's state CAS already landed; it now stores the marker

	waitStartCalls(t, mt, 3)
	requireSelected(t, c)

	requireOneChain(t, rec, conc)
	require.Equal(t, int32(2), spawnCount.Load(), "gen 2 must hand off to a SECOND spawned loop, never retry itself")

	require.NoError(t, c.Close())
}

// TestReconnect_RetiringInvocationPublishesNothing proves the retiring invocation's own half of the hand-off directly,
// rather than inferring it from the epoch id sequence alone —
// an id sequence cannot by itself tell "gen 2 handed off to a genuinely new invocation"
// apart from "gen 2 retried itself and happened to mint the same next id"
// (see the spawnCount cross-check in TestReconnect_ConcurrentTCPUpDuringFailingStartWaitsForMarker for the companion proof from the other direction).
// It holds gen 3's connectLoop invocation — already spawned by react for gen 2's own drop,
// since the drop is processed before gen 2's Start ever returns —
// BEFORE it can even check gen 2's own startReturned barrier, let alone claim the reconnect gauge or dial.
// With gen 3 provably unable to do anything, releasing gen 2's held Start and waiting for gen 2's own connectLoop invocation to reach its exit hook —
// proves directly that the retiring invocation — the only one that could possibly still be running — published nothing of its own:
// mt.startCalls() and cur's identity are both unchanged from what gen 2's own drop left behind.
func TestReconnect_RetiringInvocationPublishesNothing(t *testing.T) {
	rec := &startCallRecorder{}
	conc := &concurrentStartTracker{}
	conns := &connCloseTracker{}
	release := make(chan struct{})

	c, mt := newLifeConn(t, withMockTransportSequence(rec, conc,
		stepConnectAndSelect(conns),           // gen 1: normal bring-up
		stepTCPUpDropThenFail(conns, release), // gen 2: TCPUp -> TCPDown -> held -> error
		stepConnectAndSelect(conns),           // gen 3: the drop reaction's own successor
	))
	releaseFn := guardedClose(t, release) // safety net: never leave the mock's Start blocked past this test
	require.NoError(t, c.UpdateConfigOptions(WithT5(10*time.Millisecond)))

	require.NoError(t, c.Open(t.Context(), OpenBackground))
	requireSelected(t, c)

	// n=2: the FIRST call is gen 2's own barrier check (resolves instantly against gen 1's already-closed startReturned,
	// so it must not be held); the SECOND is gen 3's REAL check, held BEFORE it even looks at gen 2's own startReturned.
	var barrierCount atomic.Int32

	gen3Parked := newOnceSignal()
	resumeGen3Barrier := make(chan struct{})
	resumeGen3BarrierFn := guardedClose(t, resumeGen3Barrier) // safety net: never leave gen 3 blocked past this test
	c.testHookConnectLoopBarrier = func() {
		if barrierCount.Add(1) != 2 {
			return
		}

		gen3Parked.fire()
		<-resumeGen3Barrier
	}

	loopExited := newOnceSignal()
	c.testHookLoopExit = func() { loopExited.fire() } // fire-only: gen 2's own invocation needs no further holding

	mt.simulateReadError(errStartAfterTCPUpDrop) // gen 1 drops -> react spawns gen 2's connectLoop
	waitStartCalls(t, mt, 2)                     // gen 2's Start has begun and is now held

	gen3Parked.wait(t, "gen 3's connectLoop must be held before it can check gen 2's own barrier")

	releaseFn() // gen 2's own Start now returns the failure; its hand-off releases gen 2's own barrier

	loopExited.wait(t, "gen 2's own (retiring) connectLoop invocation must reach its exit hook")

	// gen 3 is STILL held, before it could ever check gen 2's barrier, claim the gauge, or dial —
	// so anything observable here belongs to gen 2's own, now-exiting invocation alone.
	require.Equal(t, 2, mt.startCalls(), "the retiring invocation must not have redialed itself")
	require.Equal(t, uint64(2), c.cur.Load().id, "the retiring invocation must not have republished a new epoch of its own")

	resumeGen3BarrierFn() // let gen 3 proceed: check the (already closed) barrier, claim the gauge, dial

	waitStartCalls(t, mt, 3)
	requireSelected(t, c)

	gens := rec.waitThreeGens(t)
	require.Equal(t, []uint64{1, 2, 3}, gens, "exactly one chain")
	require.Equal(t, int32(1), conc.max.Load(), "at most one Start may run at a time")

	require.NoError(t, c.Close())
}

// TestReconnect_SuccessPathReleasesGaugeBeforeBarrier drives a successful Start whose generation drops immediately afterward
// (TCPUp, TCPDown, then a nil return — the reconnect_backoff_persist shape),
// and parks the predecessor invocation at its exit hook — reached only AFTER it has already released its gauge share and its own barrier.
// While parked there, the drop's own successor must already have claimed the gauge: it must read exactly 1, never 2.
// This is the teeth for a success path that forgets to release its share before releasing the barrier.
func TestReconnect_SuccessPathReleasesGaugeBeforeBarrier(t *testing.T) {
	rec := &startCallRecorder{}
	conc := &concurrentStartTracker{}
	conns := &connCloseTracker{}

	// gen 3's OWN Start blocks here until this test has already confirmed gen 2 parked at its exit hook —
	// otherwise gen 3 (whose own Start returns nil almost immediately, driveSelect running on its own goroutine)
	// could race gen 2 to testHookLoopExit, a single connection-wide seam neither invocation can tell the other's exit apart from.
	gen3Proceed := make(chan struct{})

	c, mt := newLifeConn(t, withMockTransportSequence(rec, conc,
		stepConnectAndSelect(conns), // gen 1: normal bring-up
		func(_ context.Context, rt TransportRuntime) error { // gen 2: TCPUp -> TCPDown -> success
			rt.TCPUp(mkConn(conns))
			rt.TCPDown(errStartAfterTCPUpDrop)

			return nil
		},
		func(ctx context.Context, rt TransportRuntime) error { // gen 3: the drop reaction's own successor
			<-gen3Proceed

			go func() {
				rt.TCPUp(mkConn(conns))
				driveSelect(ctx, rt)
			}()

			return nil
		},
	))
	gen3ProceedFn := guardedClose(t, gen3Proceed) // safety net: never leave gen 3's Start blocked past this test
	require.NoError(t, c.UpdateConfigOptions(WithT5(10*time.Millisecond)))

	require.NoError(t, c.Open(t.Context(), OpenBackground))
	requireSelected(t, c)

	parked := newOnceSignal()
	resume := make(chan struct{})
	resumeFn := guardedClose(t, resume) // safety net: never leave gen 2's connectLoop blocked past this test
	c.testHookLoopExit = func() {
		parked.fire()
		<-resume
	}

	mt.simulateReadError(errStartAfterTCPUpDrop) // gen 1 drops -> react spawns gen 2's connectLoop

	parked.wait(t, "gen 2's connectLoop must park at its exit hook after releasing the barrier")
	waitStartCalls(t, mt, 3) // gen 3 has claimed the gauge and called Start (now blocked on gen3Proceed)

	require.Equal(t, int64(1), c.Metrics().Reconnecting(),
		"gen 3 must already own the gauge share while gen 2 is parked here, never both at once")

	resumeFn()      // release gen 2 (it has nothing left to do but return)
	gen3ProceedFn() // let gen 3 actually dial and select

	waitStartCalls(t, mt, 3)
	requireSelected(t, c)

	gens := rec.waitThreeGens(t)
	require.Equal(t, []uint64{1, 2, 3}, gens)
	// Settles asynchronously, on gen 3's own connectLoop goroutine, strictly AFTER State() already reads Selected.
	require.Eventually(t, func() bool { return c.Metrics().Reconnecting() == 0 }, startWaitTimeout, time.Millisecond,
		"the gauge must settle back to 0")

	require.NoError(t, c.Close())
}

// TestReconnect_GenZeroSecondAdmissionRefused drives gen 2's own TCPUp then TCPDown (both unnamed, gen 0 — the shape a plain SECS-I or out-of-module report takes),
// then, once the reaction for that drop is confirmed blocked (testHookReactBeforeCurLoad holds it before it can ever tear gen 2 down),
// a delayed SECOND gen-0 TCP-up report arrives for the still-live generation —
// through the REAL TransportRuntime call, not a synthetic CAS, the same call shape a plain SECS-I or out-of-module transport actually uses.
// react's hook fires only after step() has already stored NotConnectedState for gen 2's own drop,
// so by the time the delayed report lands, the state CAS a successful admission would run is one that WOULD succeed —
// at-most-one admission is the only thing left that can still refuse it,
// which is what State() staying at NotConnectedState proves.
// Once the reaction is released, gen 2's Start errors and hands off: exactly one drop, one chain.
func TestReconnect_GenZeroSecondAdmissionRefused(t *testing.T) {
	rec := &startCallRecorder{}
	conc := &concurrentStartTracker{}
	conns := &connCloseTracker{}

	reactHeld := newOnceSignal()
	resumeReact := make(chan struct{})

	gen2 := func(_ context.Context, rt TransportRuntime) error {
		c := runtimeConnection(rt)

		// Installed before TCPDown enqueues gen 2's own drop report,
		// so react — whenever it eventually runs — is guaranteed to reach and block on this hook before it can ever tear gen 2 down;
		// gen 1's own reaction (which spawned this very invocation) already ran before this callback started,
		// so the first call this hook ever sees is gen 2's own.
		var reactCount atomic.Int32
		c.testHookReactBeforeCurLoad = func() {
			if reactCount.Add(1) != 1 {
				return
			}

			reactHeld.fire()
			<-resumeReact
		}

		rt.TCPUp(mkConn(conns))
		rt.TCPDown(errStartAfterTCPUpDrop)

		// Wait for gen 2's own drop reaction to actually reach (and block at) the hook above —
		// not merely for it to have been enqueued.
		// step() stores NotConnectedState for that drop strictly BEFORE it ever calls react,
		// so this wait is what pins down that the FSM has already left NotSelected before the delayed report below runs;
		// react cannot have torn gen 2 down yet either, since it is guaranteed to block at the hook first.
		select {
		case <-reactHeld.ch:
		case <-time.After(startWaitTimeout):
		}

		// A delayed SECOND gen-0 TCP-up report on this still-live generation, driven through the REAL
		// TransportRuntime call.
		rt.TCPUp(mkConn(conns))

		return errStartAfterTCPUpFail
	}

	c, mt := newLifeConn(t, withMockTransportSequence(rec, conc,
		stepConnectAndSelect(conns), // gen 1: normal bring-up
		gen2,                        // gen 2: TCPUp -> TCPDown -> delayed 2nd TCP-up -> error
		stepConnectAndSelect(conns), // gen 3: the drop reaction's own successor
	))
	resumeReactFn := guardedClose(t, resumeReact) // safety net: never leave react blocked past this test
	require.NoError(t, c.UpdateConfigOptions(WithT5(10*time.Millisecond)))

	require.NoError(t, c.Open(t.Context(), OpenBackground))
	requireSelected(t, c)

	mt.simulateReadError(errStartAfterTCPUpDrop) // gen 1 drops -> react spawns gen 2's connectLoop

	reactHeld.wait(t, "gen 2's own drop reaction must be held before this test proceeds")

	// gen 3 cannot have been published yet:
	// react's own step that would spawn its loop is what this test is holding,
	// so cur is still gen 2's own epoch.
	e := c.cur.Load()
	require.NotNil(t, e)
	require.Equal(t, uint64(2), e.id, "gen 2 must still be the live generation while its own reaction is held")

	// e.ended is NOT asserted here:
	// connectLoopStartFailure tears gen 2 down on its OWN, independent of react's hold,
	// the instant gen 2's Start callback returns —
	// so by the time this goroutine gets to look, gen 2 may already be marked ended —
	// regardless of how this scenario actually played out.
	// That race is harmless to the property under test:
	// the delayed report's own TCPUp call runs strictly BEFORE gen 2's Start returns
	// (so strictly before connectLoopStartFailure ever runs), and it must have been refused:
	// the FSM stays at NotConnectedState,
	// since a successful admission would have flipped it straight to NotSelectedState —
	// via the same synchronous CAS the first report used.
	require.Equal(t, NotConnectedState, c.State(), "a second admission on the same generation must be refused")
	require.Equal(t, 2, mt.startCalls(), "no successor Start yet — the reaction is still held")

	resumeReactFn()

	waitStartCalls(t, mt, 3)
	requireSelected(t, c)

	gens := rec.waitThreeGens(t)
	require.Equal(t, []uint64{1, 2, 3}, gens, "exactly one drop, one chain")
	require.Equal(t, int32(1), conc.max.Load())

	require.NoError(t, c.Close())
}

// TestReconnect_FailedCommitAndTCPDownWithoutTCPUpKeepLoopOwnership covers two ways the failure hand-off must NOT trigger:
// an admitted TCP-up commit whose state CAS itself still returns false leaves no marker,
// and a Start that reports TCPDown without ever reporting TCPUp first leaves no marker and fires no reaction.
// In both, the SAME connectLoop invocation must keep ownership of the retry — never a second, reaction-spawned loop.
func TestReconnect_FailedCommitAndTCPDownWithoutTCPUpKeepLoopOwnership(t *testing.T) {
	t.Run("a unit-level false CAS on an admitted commit still leaves no marker", func(t *testing.T) {
		// No real single-generation schedule can reach a false CAS here:
		// a live generation's FIRST admitted TCP-up is always the one CAS-ing state out of NotConnected,
		// and nothing else can have moved it away first (see connection.tcpUpCommitGate's own doc).
		// This drives the gate directly with a false CAS instead —
		// the same technique TestTCPUpCommitGate_MarkerPlacement uses —
		// to pin the gate's own defensive "admitted but not committed" branch: the marker it guards must still not be set.
		c, _ := newLifeConn(t, mockScript(func(_ context.Context, _ TransportRuntime) error { return nil }))
		require.NoError(t, c.Open(t.Context(), OpenBackground))

		e := c.cur.Load()
		require.NotNil(t, e)

		committed, live := c.tcpUpCommitGate(0, func() bool { return false })

		require.True(t, live)
		require.False(t, committed)
		require.True(t, e.tcpUpAdmitted.Load(), "admission is still taken even when the CAS itself fails")
		require.False(t, e.tcpUpCommitted.Load(), "no marker when the CAS fails")

		require.NoError(t, c.Close())
	})

	t.Run("TCPDown without any TCPUp keeps the SAME invocation's own ownership", func(t *testing.T) {
		rec := &startCallRecorder{}
		conc := &concurrentStartTracker{}
		conns := &connCloseTracker{}

		var spawnCount atomic.Int32

		c, mt := newLifeConn(t, withMockTransportSequence(rec, conc,
			stepConnectAndSelect(conns), // gen 1: normal bring-up
			func(_ context.Context, rt TransportRuntime) error { // gen 2: TCPDown only, no TCPUp -> error
				rt.TCPDown(errStartAfterTCPUpDrop)

				return errStartAfterTCPUpFail
			},
			stepConnectAndSelect(conns), // gen 3: the SAME invocation's own next attempt (not a hand-off)
		))
		require.NoError(t, c.UpdateConfigOptions(WithT5(time.Millisecond)))

		c.testHookLoopSpawned = func() { spawnCount.Add(1) }

		require.NoError(t, c.Open(t.Context(), OpenBackground))
		requireSelected(t, c)

		mt.simulateReadError(errStartAfterTCPUpDrop) // gen 1 drops -> react spawns gen 2's connectLoop (spawn #1)

		waitStartCalls(t, mt, 3)
		requireSelected(t, c)

		gens := rec.waitThreeGens(t)
		require.Equal(t, []uint64{1, 2, 3}, gens)
		require.Equal(t, int32(1), spawnCount.Load(),
			"the same invocation must retry itself — no second, reaction-spawned loop for a TCPDown-less, TCPUp-less failure")

		require.NoError(t, c.Close())
	})
}

// TestReconnect_CancelledAtBarrierGaugeNeverNegative releases a successor parked at the barrier through Close's reconnectCancel (stop) instead of the predecessor's own startReturned,
// before it ever increments the reconnect gauge.
// gen 2's own held Start already owns the gauge's one share throughout (Reconnecting() reports 1 while a reconnect loop is actively retrying —
// that share is legitimately gen 2's, not gen 3's),
// so the property under test is that it never reads 2 (gen 3 cancelled at the barrier must never ALSO claim a share) and settles at 0 —
// never -1 from an unconditional decrement —
// once gen 2 itself releases its own share on the very same hand-off path the earlier tests in this file exercise.
func TestReconnect_CancelledAtBarrierGaugeNeverNegative(t *testing.T) {
	rec := &startCallRecorder{}
	conc := &concurrentStartTracker{}
	conns := &connCloseTracker{}
	release := make(chan struct{})

	c, mt := newLifeConn(t, withMockTransportSequence(rec, conc,
		stepConnectAndSelect(conns),           // gen 1: normal bring-up
		stepTCPUpDropThenFail(conns, release), // gen 2: TCPUp -> TCPDown -> held -> error
		stepConnectAndSelect(conns),           // gen 3: would-be successor (must never be dialed)
	))
	releaseFn := guardedClose(t, release) // safety net: never leave the mock's Start blocked past this test
	require.NoError(t, c.UpdateConfigOptions(WithT5(10*time.Millisecond)))

	require.NoError(t, c.Open(t.Context(), OpenBackground))
	requireSelected(t, c)

	// n=2:
	// the FIRST call is gen 2's own barrier check (resolves instantly against gen 1's already-closed startReturned, and claims the gauge's one share for the whole time gen 2's own Start remains in flight);
	// the SECOND is gen 3's REAL block, cancelled below before it can add a second share.
	barrierEntered := installNthConnectLoopBarrierSignal(c, 2)

	mt.simulateReadError(errStartAfterTCPUpDrop) // gen 1 drops -> react spawns gen 2's connectLoop
	waitStartCalls(t, mt, 2)                     // gen 2's Start has begun (and owns the gauge) and is now held

	barrierEntered.wait(t, "gen 3's connectLoop must reach the barrier before Close")

	require.Equal(t, int64(1), c.Metrics().Reconnecting(), "gen 2's own held Start still owns the gauge's one share")

	closeErr := make(chan error, 1)
	go func() { closeErr <- c.Close() }()

	// Close closes reconnectCancel promptly, even while gen 2's Start is still held, releasing gen
	// 3's barrier wait through stop — before it ever increments.
	require.Eventually(t, func() bool { return c.shutdown.Load() }, startWaitTimeout, time.Millisecond,
		"Close must fence reconnect promptly")

	require.Never(t, func() bool { return c.Metrics().Reconnecting() != 1 }, 200*time.Millisecond, 5*time.Millisecond,
		"gen 3 cancelled at the barrier must never touch the gauge — it must stay at gen 2's own 1, never 2 nor 0 early")

	releaseFn() // release gen 2's held Start so Close can complete

	select {
	case err := <-closeErr:
		require.NoError(t, err)
	case <-time.After(startWaitTimeout):
		t.Fatal("Close did not complete")
	}

	require.Equal(t, int64(0), c.Metrics().Reconnecting(), "gen 2's own release must settle the gauge at 0, never -1")
	require.Equal(t, 2, mt.startCalls(), "gen 3 must never actually be dialed")

	require.NoError(t, c.Close())
}

// TestReconnect_StartAfterTCPUpHandsOffWhenStartReturnsFirst drives the same reconnect-generation scenario as the drop-processed-first test above, but forces the OPPOSITE schedule:
// the failing Start returns (and the hand-off's own injectDisconnect report is queued) BEFORE the FSM has processed the drop TCPDown produced, by pausing the supervisor on that drop's own step() —
// installed and confirmed reached before Start is released —
// until after Start has returned.
// The outcome must be identical:
// exactly one successor chain, no orphan, the gauge never reads 2, and the recovery is counted once.
func TestReconnect_StartAfterTCPUpHandsOffWhenStartReturnsFirst(t *testing.T) {
	rec := &startCallRecorder{}
	conc := &concurrentStartTracker{}
	conns := &connCloseTracker{}

	reachedPause := newOnceSignal()
	resumePause := make(chan struct{})

	// gen 2's own Start callback installs the pausing hook itself, immediately before the TCPDown call whose resulting evDisconnect it means to pause:
	// TCPDown's own injectFrom is a channel send,
	// and the corresponding receive on the supervisor's run() goroutine happens-after it,
	// so this plain field write is ordered before that goroutine's read —
	// race-free by construction, with no separate synchronization needed.
	// Gen 1's own drop (which must run normally, to spawn gen 2's loop in the first place) is already fully processed by the time gen 2's Start callback runs at all,
	// since react — which spawns that loop — runs synchronously inside gen 1's own step() call.
	gen2 := func(_ context.Context, rt TransportRuntime) error {
		runtimeConnection(rt).sup.Load().testHookAfterStateLoad = func(ev fsmEvent) {
			if ev != evDisconnect {
				return
			}

			reachedPause.fire()
			<-resumePause
		}

		rt.TCPUp(mkConn(conns))
		rt.TCPDown(errStartAfterTCPUpDrop)

		return errStartAfterTCPUpFail
	}

	c, mt := newLifeConn(t, withMockTransportSequence(rec, conc,
		stepConnectAndSelect(conns), // gen 1: normal bring-up
		gen2,                        // gen 2: TCPUp -> TCPDown -> returns the failure immediately
		stepConnectAndSelect(conns), // gen 3: the drop reaction's own successor
	))
	resumePauseFn := guardedClose(t, resumePause) // safety net: never leave the supervisor blocked past this test
	require.NoError(t, c.UpdateConfigOptions(WithT5(10*time.Millisecond)))

	loopExited := newOnceSignal()
	c.testHookLoopExit = func() { loopExited.fire() } // fire-only: gen 2's own invocation needs no further holding

	require.NoError(t, c.Open(t.Context(), OpenBackground))
	requireSelected(t, c)

	mt.simulateReadError(errStartAfterTCPUpDrop) // gen 1 drops -> react spawns gen 2's connectLoop

	reachedPause.wait(t, "gen 2's own TCPDown report must reach the FSM before Start is released")

	// gen 2's Start has already returned the failure by now
	// (the pause fires only once its own TCPDown report has been sent, which happens before Start's return).
	// e.ended.Load() alone is NOT enough to prove its failure handling has fully run:
	// markEnded latches ended at the very START of teardown,
	// well before connectLoopStartFailure goes on to join that teardown, read the tcpUpCommitted marker, and decide the hand-off.
	// testHookLoopExit, in contrast, fires as the VERY LAST step of gen 2's own connectLoop invocation —
	// after that whole decision (and injectDisconnect's own enqueue, which never waits on the paused supervisor to drain it)
	// has already completed.
	// Waiting for it is what proves the schedule really did land Start's return,
	// and everything that follows it, before the FSM ever applies gen 2's drop.
	loopExited.wait(t, "gen 2's own connectLoop invocation must reach its exit hook before the FSM ever sees its drop")

	resumePauseFn() // let the FSM finally process gen 2's drop; react spawns gen 3's connectLoop

	waitStartCalls(t, mt, 3)
	requireSelected(t, c)

	gens := rec.waitThreeGens(t)
	require.Equal(t, []uint64{1, 2, 3}, gens, "each Start call must dial its own generation exactly once, in order")
	require.Equal(t, int32(1), conc.max.Load(), "at most one Start may run at a time, across the whole schedule")

	// Both settle asynchronously, on gen 3's own connectLoop goroutine, strictly AFTER State() already reads Selected —
	// bounded, not immediate.
	require.Eventually(t, func() bool { return c.Metrics().Reconnecting() == 0 }, startWaitTimeout, time.Millisecond,
		"the gauge must settle back to 0 once gen 3 selects")
	require.Eventually(t, func() bool { return c.Metrics().Reconnects() == 1 }, startWaitTimeout, time.Millisecond,
		"the recovery is counted once, not twice")

	require.NoError(t, c.Close())
	require.Equal(t, int32(3), conns.closed.Load(), "every TCPUp'd socket must be closed after Close")
}
