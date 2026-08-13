package hsms

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/logger"
	"github.com/stretchr/testify/require"
)

// TestReconnect_AfterDropReselects is the happy-path reconnect (spec §5.2): an active pair opens,
// selects, suffers an involuntary drop, and the reconnect loop dials a fresh generation that
// re-selects — then Close tears it down cleanly. This is ALSO the round-6 supervisor-lifetime
// teeth: the supervisor spans generations (it is connection-owned, NOT epoch-spawned), so it is
// still alive to drive evTCPUp/evSelectAccepted for generation N+1. An epoch-spawned supervisor
// would have been killed by the drop's epoch-ctx cancel and gen N+1 would never reselect.
func TestReconnect_AfterDropReselects(t *testing.T) {
	c, mt := newLifeConn(t, withMockTransport())
	require.NoError(t, c.UpdateConfigOptions(WithT5(10*time.Millisecond)))

	require.NoError(t, c.Open(t.Context(), OpenBackground))
	requireSelected(t, c)

	mt.simulateReadError(io.EOF) // involuntary drop

	// A second Start (dial) proves the reconnect loop ran; Selected proves gen N+1 reselected.
	require.Eventually(t, func() bool { return mt.startCalls() >= 2 }, 3*time.Second, time.Millisecond,
		"reconnect must dial a second generation")
	require.Eventually(t, func() bool { return c.State() == SelectedState }, 3*time.Second, time.Millisecond,
		"gen N+1 must reselect")

	require.NoError(t, c.Close())
	require.Equal(t, NotConnectedState, c.State())
}

// TestReconnect_WriteFailureTearsDownAndReconnects strengthens the I5/CRIT-1 recovery proof (the
// gap Codex flagged): TestSend_WriteFailureTearsDownGeneration only proves the write-failure path
// sets commsFailure with a supervisor that is never run(). Here the supervisor is RUNNING (Open
// wired it), so a transport write failure — what a write-deadline timeout on a wedged peer produces
// — must drive TCPDown -> evDisconnect -> react -> teardown -> NotConnected (an ACTUAL FSM teardown,
// not just a flag), after which the reconnect loop dials a fresh generation that re-selects.
//
// Teeth: drop the c.TCPDown(err) from writeFrame's write-error path — the send still returns the
// error, but the generation stays Selected, no second Start is dialed, and startCalls>=2 bites.
func TestReconnect_WriteFailureTearsDownAndReconnects(t *testing.T) {
	c, mt := newLifeConn(t, withMockTransport())
	require.NoError(t, c.UpdateConfigOptions(WithT5(10*time.Millisecond)))

	require.NoError(t, c.Open(t.Context(), OpenBackground))
	requireSelected(t, c)

	// Arm a transport write failure (a write-deadline timeout on a wedged peer produces exactly this).
	mt.setWriteErr(errors.New("simulated wedged-peer write failure"))

	// A synchronous W-bit send now fails at the transport writev.
	_, err := c.sendWaitReply(t.Context(), mustSendData(t, [4]byte{0, 0, 0, 7}, true))
	require.Error(t, err, "the transport write failure must propagate to the caller")

	// The RUNNING supervisor must act on the resulting evDisconnect: react tears the generation down
	// and the reconnect loop dials a fresh one — a second Start proves the write failure drove an
	// actual FSM teardown, not merely a commsFailure flag.
	require.Eventually(t, func() bool { return mt.startCalls() >= 2 }, 3*time.Second, time.Millisecond,
		"a write failure under a running supervisor must tear down the generation and reconnect (a fresh Start)")

	// Clear the fault so the reconnected generation is usable, and confirm it re-selects.
	mt.setWriteErr(nil)
	require.Eventually(t, func() bool { return c.State() == SelectedState }, 3*time.Second, time.Millisecond,
		"the reconnected generation must reselect")

	require.NoError(t, c.Close())
	require.Equal(t, NotConnectedState, c.State())
}

// TestReconnect_GenFenceAfterClose: a reconnect scheduled just before Close must NOT publish a new
// generation after Close (G2). It also exercises the round-8 no-deadlock property: Close holds
// lifeMu while it connectLoopWg.Wait()s the paused loop, and the loop's fence is ATOMICS ONLY, so
// it can abandon and return without ever contending lifeMu — no deadlock.
// Teeth: drop the shutdown/reconnectGen re-check (F3 + the G2 fence) → the loop publishes a fresh
// epoch after Close (require.Same on cur then fails).
func TestReconnect_GenFenceAfterClose(t *testing.T) {
	c, mt := newLifeConn(t, withMockTransport())
	require.NoError(t, c.UpdateConfigOptions(WithT5(time.Millisecond)))

	reached := make(chan struct{})
	proceed := make(chan struct{})
	var hookOnce sync.Once
	c.testHookConnectLoop = func() {
		hookOnce.Do(func() {
			close(reached)
			<-proceed
		})
	}

	require.NoError(t, c.Open(t.Context(), OpenBackground))
	requireSelected(t, c)

	prevEpoch := c.cur.Load()
	mt.simulateReadError(io.EOF) // drop → reconnect loop starts

	<-reached // the loop is past its backoff, paused at the fence, BEFORE publishing

	// Close while the loop is paused: it sets shutdown + bumps reconnectGen, then (holding lifeMu)
	// blocks in connectLoopWg.Wait() until the loop abandons at its atomics-only fence.
	closeErr := make(chan error, 1)
	go func() { closeErr <- c.Close() }()

	require.Eventually(t, func() bool { return c.shutdown.Load() }, time.Second, time.Millisecond,
		"Close must set shutdown before the loop resumes")

	close(proceed) // resume the loop → its fence sees shutdown → abandons the fresh epoch

	select {
	case err := <-closeErr:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Close hung — the atomics-only G2 fence must not deadlock against lifeMu-held connectLoopWg.Wait (round-8)")
	}

	require.Same(t, prevEpoch, c.cur.Load(),
		"reconnect must NOT publish a new generation after Close (G2 fence)")
	require.Equal(t, NotConnectedState, c.State())
}

// TestReconnect_StaleRecvLoopTCPDownDoesNotDisconnectNewGen (round-7 Critical): a stale gen-N
// recv-loop TCPDown must not disconnect gen N+1. The reconnect loop e.wait()s the prior epoch —
// whose teardown joins the recv loop via tr.Stop — BEFORE dialing gen N+1, so the stale TCPDown
// provably fires (and drains as a no-op evDisconnect from terminal NotConnected) while cur is
// still gen N. Teeth: skip the e.wait()-before-dial serialization → gen N+1 is dialed before the
// gen-N recv loop is joined, and the stale TCPDown lands on gen N+1 and disconnects it.
func TestReconnect_StaleRecvLoopTCPDownDoesNotDisconnectNewGen(t *testing.T) {
	c, mt := newLifeConn(t, withMockTransport())
	mt.holdableRecv = true                                             // arm a held gen-N recv loop on the first Start
	require.NoError(t, c.UpdateConfigOptions(WithT5(time.Nanosecond))) // near-immediate redial

	require.NoError(t, c.Open(t.Context(), OpenBackground))
	requireSelected(t, c)

	mt.dropGenerationButHoldTCPDown() // gen N drops; its recv loop's stale TCPDown is parked

	require.Eventually(t, func() bool { return mt.startCalls() == 2 && c.State() == SelectedState }, 3*time.Second, time.Millisecond,
		"gen N+1 must reselect (exactly two generations dialed so far)")

	mt.releaseHeldTCPDown() // the stale gen-N TCPDown (already drained during teardown) — a no-op

	// gen N+1 must STAY Selected. If the stale gen-N TCPDown had disconnected it, gen N+1 would
	// tear down and a THIRD generation would be dialed (startCalls > 2) — a monotonic signal that
	// survives the fast re-reconnect that would otherwise hide the brief NotConnected window.
	require.Never(t, func() bool { return mt.startCalls() > 2 || c.State() != SelectedState }, 500*time.Millisecond, 5*time.Millisecond,
		"a stale gen-N TCPDown must NOT disconnect gen N+1 (round-7)")

	require.NoError(t, c.Close())
}

// TestReconnect_LoopJoinSeparateFromEpochWg (§7.C): the reconnect loop is joined via connectLoopWg,
// SEPARATE from epoch.wg, so the dropped generation's teardown completes cleanly and promptly even
// while the loop is running. Teeth: run the loop under epoch.wg (e.spawn) → the dropped epoch's
// bounded join waits on the loop, the loop waits on that epoch's done, and teardown only unblocks
// at the close-timeout with ErrCloseTimeout (this 2s wait then trips).
func TestReconnect_LoopJoinSeparateFromEpochWg(t *testing.T) {
	c, mt := newLifeConn(t, withMockTransport())
	require.NoError(t, c.UpdateConfigOptions(WithT5(10*time.Millisecond)))

	require.NoError(t, c.Open(t.Context(), OpenBackground))
	requireSelected(t, c)

	prevEpoch := c.cur.Load()
	mt.simulateReadError(io.EOF) // drop → reconnect loop runs concurrently with teardown

	joinDone := make(chan error, 1)
	go func() { joinDone <- prevEpoch.wait() }()
	select {
	case err := <-joinDone:
		require.NoError(t, err, "dropped-generation teardown must complete cleanly, not time out")
	case <-time.After(2 * time.Second):
		t.Fatal("dropped-generation teardown deadlocked — reconnect loop must NOT be an epoch.wg task (§7.C)")
	}

	require.Eventually(t, func() bool { return mt.startCalls() >= 2 && c.State() == SelectedState }, 3*time.Second, time.Millisecond,
		"reconnect still makes progress under the separate join")

	require.NoError(t, c.Close())
}

// TestOpen_StartFailureNoSpuriousReconnect (E2 reconciliation #2): a tr.Start that fails AFTER
// driving evTCPUp (FSM at NotSelected) must NOT trigger a reconnect — the Open rollback sets
// shutdown before requestClose, so the NotSelected→NotConnected reaction skips startConnectLoop.
// Teeth: remove the rollback shutdown fence → the reaction starts a reconnect loop and a second
// Start (dial) occurs.
func TestOpen_StartFailureNoSpuriousReconnect(t *testing.T) {
	startErr := errors.New("hsms_test: dial failed after TCPUp")
	c, mt := newLifeConn(t, withMockTransportTCPUpThenStartError(startErr))
	require.NoError(t, c.UpdateConfigOptions(WithT5(time.Millisecond)))

	err := c.Open(t.Context(), OpenBackground)
	require.ErrorIs(t, err, startErr, "Open must return the tr.Start failure")

	require.Never(t, func() bool { return mt.startCalls() > 1 }, 300*time.Millisecond, 10*time.Millisecond,
		"a tr.Start failure after evTCPUp must NOT trigger a spurious reconnect (reconciliation #2)")
	require.Equal(t, NotConnectedState, c.State())
}

// TestOpen_ActiveBackgroundColdPeerRetriesInBackground proves Gap 1: an ACTIVE connection's
// OpenBackground on a peer that is down at Open time (dial refused, TCPUp never fires) returns
// nil immediately and keeps retrying in the background until the peer comes up — v1 parity.
func TestOpen_ActiveBackgroundColdPeerRetriesInBackground(t *testing.T) {
	dialErr := errors.New("simulated connection refused")
	c, mt := newLifeConn(t, withMockTransportDialFailsThenConnects(2, dialErr))
	require.NoError(t, c.UpdateConfigOptions(WithT5(50*time.Millisecond)))

	openDone := make(chan error, 1)
	go func() { openDone <- c.Open(t.Context(), OpenBackground) }()

	select {
	case err := <-openDone:
		require.NoError(t, err, "Open on a cold active peer under OpenBackground must return nil, not the dial error")
	case <-time.After(2 * time.Second):
		t.Fatal("Open must return promptly even though the first dial failed")
	}

	require.Eventually(t, func() bool { return c.State() == SelectedState }, 5*time.Second, time.Millisecond,
		"the background retry loop must eventually connect and select once the peer 'comes up'")
	require.GreaterOrEqual(t, mt.startCalls(), 3, "expected 2 failed dials + 1 successful dial")

	require.Equal(t, uint64(0), c.Metrics().Reconnects(),
		"the initial cold-connect retry must NOT count as a reconnect (never for the very first Open)")

	require.NoError(t, c.Close())
}

// TestOpen_PassiveBackgroundFirstFailureStaysFatal proves Gap 1 is scoped to the active role: a
// passive tr.Start failure (a real listen/bind error) stays fatal under OpenBackground exactly
// as before — it must never be retried.
func TestOpen_PassiveBackgroundFirstFailureStaysFatal(t *testing.T) {
	dialErr := errors.New("simulated listen failure")
	c, mt := newLifeConn(t, withMockTransportDialFails(dialErr))
	mt.setPassiveRole()

	err := c.Open(t.Context(), OpenBackground)
	require.ErrorIs(t, err, dialErr, "a passive Start failure must be returned, never retried")

	require.Never(t, func() bool { return mt.startCalls() > 1 }, 300*time.Millisecond, 10*time.Millisecond,
		"a passive listen failure must not trigger a background retry")
	require.Equal(t, NotConnectedState, c.State())
}

// TestOpen_ActiveWaitSelectedColdPeerStillFails proves Gap 1 is scoped to OpenBackground:
// OpenWaitSelected on a cold active peer still fails fast (it never even reaches the new
// branch, since the mode check gates on OpenBackground).
func TestOpen_ActiveWaitSelectedColdPeerStillFails(t *testing.T) {
	dialErr := errors.New("simulated connection refused")
	c, mt := newLifeConn(t, withMockTransportDialFails(dialErr))

	err := c.Open(t.Context(), OpenWaitSelected)
	require.ErrorIs(t, err, dialErr, "OpenWaitSelected must still fail fast on a cold peer")

	require.Never(t, func() bool { return mt.startCalls() > 1 }, 300*time.Millisecond, 10*time.Millisecond,
		"OpenWaitSelected must not trigger a background retry")
}

// TestOpen_ActiveBackgroundColdPeerCloseDuringRetryIsBounded: a Close() while the Gap-1 cold-connect
// retry loop is paused mid-backoff must terminate promptly. The cold-connect path reuses the exact
// same connectLoop/reconnectCancel/reconnectGen machinery TestReconnect_GenFenceAfterClose already
// proves bounded for the post-drop case — this test proves it holds for the cold-connect path too,
// not just assumed via code-sharing.
func TestOpen_ActiveBackgroundColdPeerCloseDuringRetryIsBounded(t *testing.T) {
	dialErr := errors.New("simulated connection refused")
	// A large failCount: this test closes the connection before the loop would ever succeed.
	c, _ := newLifeConn(t, withMockTransportDialFailsThenConnects(1000, dialErr))
	require.NoError(t, c.UpdateConfigOptions(WithT5(time.Millisecond)))

	reached := make(chan struct{})
	proceed := make(chan struct{})
	var hookOnce sync.Once
	c.testHookConnectLoop = func() {
		hookOnce.Do(func() {
			close(reached)
			<-proceed
		})
	}

	err := c.Open(t.Context(), OpenBackground)
	require.NoError(t, err, "Open on a cold active peer under OpenBackground must return nil")

	<-reached // the cold-connect retry loop is past its first backoff, paused at the fence

	closeErr := make(chan error, 1)
	go func() { closeErr <- c.Close() }()

	require.Eventually(t, func() bool { return c.shutdown.Load() }, time.Second, time.Millisecond,
		"Close must set shutdown before the loop resumes")

	close(proceed) // resume the loop -> its fence sees shutdown -> abandons

	select {
	case err := <-closeErr:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Close hung during a cold-connect retry — the reconnectCancel/reconnectGen fencing must bound it, same as the post-drop case")
	}

	require.Equal(t, NotConnectedState, c.State())
}

// captureDialFailLogger records the timestamp of every "reconnect dial failed" Debug log so a
// test can measure the reconnect backoff cadence without a fixed Sleep (mirrors
// hsmsss/integration_connect_reconnect_test.go's dialFailLogger, which measures the same signal
// end-to-end over real TCP).
type captureDialFailLogger struct {
	logger.Logger
	mu    sync.Mutex
	times []time.Time
}

func (l *captureDialFailLogger) Debug(msg string, keysAndValues ...any) {
	if msg == "hsms: reconnect dial failed, retrying" {
		l.mu.Lock()
		l.times = append(l.times, time.Now())
		l.mu.Unlock()
	}

	l.Logger.Debug(msg, keysAndValues...)
}

func (l *captureDialFailLogger) With(_ ...any) logger.Logger { return l }

func (l *captureDialFailLogger) snapshot() []time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()

	return append([]time.Time(nil), l.times...)
}

// withMockTransportFailsAfterFirstConnect connects+selects on the FIRST Start call (so the
// initial Open/Select succeeds), fails the next failCount Start calls (simulating a peer that
// stays down across several reconnect attempts) without ever calling TCPUp, then connects+
// selects again on every later call.
func withMockTransportFailsAfterFirstConnect(failCount int, dialErr error) mockScript {
	calls := 0

	return func(ctx context.Context, rt TransportRuntime) error {
		calls++
		if calls == 1 || calls > failCount+1 {
			go func() {
				rt.TCPUp(fakeConn{})
				driveSelect(ctx, rt)
			}()

			return nil
		}

		return dialErr
	}
}

// TestReconnect_ExponentialBackoffGrowsToT5Ceiling proves Gap 2: the reconnect loop's dial-retry
// gaps grow (not flat) and never exceed the configured T5 ceiling. Loose bounds throughout —
// tolerant of CI scheduling jitter, mirroring the style of the hsmsss integration cadence test.
func TestReconnect_ExponentialBackoffGrowsToT5Ceiling(t *testing.T) {
	const t5 = 400 * time.Millisecond
	dialErr := errors.New("simulated refused reconnect dial")
	capLog := &captureDialFailLogger{Logger: DefaultConnectionConfig().Logger()}

	c, mt := newLifeConn(t, withMockTransportFailsAfterFirstConnect(4, dialErr))
	require.NoError(t, c.UpdateConfigOptions(WithT5(t5), WithLogger(capLog)))

	require.NoError(t, c.Open(t.Context(), OpenBackground))
	requireSelected(t, c)

	mt.simulateReadError(io.EOF) // involuntary drop → 4 failed re-dials, then success

	require.Eventually(t, func() bool { return c.State() == SelectedState }, 10*time.Second, time.Millisecond,
		"the reconnect loop must eventually recover once dial failures stop")

	times := capLog.snapshot()
	require.GreaterOrEqual(t, len(times), 4, "expected at least 4 recorded dial-failure retries")

	var gaps []time.Duration
	for i := 1; i < len(times); i++ {
		gaps = append(gaps, times[i].Sub(times[i-1]))
	}
	for i, g := range gaps {
		t.Logf("reconnect gap %d: %v", i, g)
	}

	require.Less(t, gaps[0], t5, "the first reconnect retry gap must be well under the T5 ceiling (fast first retry)")
	require.Less(t, gaps[len(gaps)-1], t5+100*time.Millisecond, "no gap may exceed the T5 ceiling (with jitter slack)")

	require.NoError(t, c.Close())
}

// TestReconnect_AbandonedGenerationTCPDownCannotDropSuccessor is the adversarial interleaving the
// generation-serialization argument does NOT cover.
// The bounded teardown join gives up on a wedged transport goroutine, so that goroutine can outlive
// its own generation, the reconnect, and the successor's select — and only then report the
// disconnect it was holding.
// Because the plain TCPDown entry points resolve the current generation at call time, such a report
// used to disconnect the SUCCESSOR and hand its lifecycle subscribers a cause from a link they never
// had.
//
// Here gen N's goroutine is parked across the whole cycle (the mock's Stop deliberately neither
// releases nor joins it), released only once gen N+1 is Selected, and reports naming gen N.
// gen N+1 must stay Selected and its subscribers must see no second drop.
//
// Teeth: drop the generation match (in connection.injectDisconnect, or in supervisor.step) → the released
// report disconnects gen N+1, a third generation is dialed, and a second NotConnected event with
// CauseIOError reaches the subscriber.
func TestReconnect_AbandonedGenerationTCPDownCannotDropSuccessor(t *testing.T) {
	c, mt := newLifeConn(t, withMockTransport())
	mt.abandonedGenRecv = true                                         // arm the gen-N goroutine the bounded join will abandon
	require.NoError(t, c.UpdateConfigOptions(WithT5(time.Nanosecond))) // near-immediate redial

	var mu sync.Mutex
	var drops []TransitionCause
	cancel := c.SubscribeLifecycle(func(ev LifecycleEvent) {
		if ev.Current != NotConnectedState {
			return
		}
		mu.Lock()
		drops = append(drops, ev.Cause)
		mu.Unlock()
	})
	defer cancel()

	dropCount := func() int {
		mu.Lock()
		defer mu.Unlock()

		return len(drops)
	}

	require.NoError(t, c.Open(t.Context(), OpenBackground))
	requireSelected(t, c)

	genN := c.CurrentGeneration()
	require.NotZero(t, genN, "a live generation must have an identity")

	mt.simulateReadError(io.EOF) // gen N drops for real; its abandoned goroutine stays parked

	require.Eventually(t, func() bool { return mt.startCalls() == 2 && c.State() == SelectedState }, 3*time.Second, time.Millisecond,
		"gen N+1 must reselect (exactly two generations dialed so far)")
	require.Eventually(t, func() bool { return dropCount() == 1 }, time.Second, time.Millisecond,
		"the real gen-N drop must have been reported exactly once")

	require.NotEqual(t, genN, c.CurrentGeneration(), "gen N+1 must carry a distinct identity")

	mt.releaseAbandonedGenTCPDown() // gen N reports its disconnect — while gen N+1 owns the link

	// gen N+1 must STAY Selected, no third generation may be dialed, and no second drop may reach a
	// subscriber. startCalls is monotonic, so it catches a disconnect even if the fast redial would
	// otherwise hide the brief NotConnected window.
	require.Never(t, func() bool {
		return mt.startCalls() > 2 || c.State() != SelectedState || dropCount() > 1
	}, 500*time.Millisecond, 5*time.Millisecond,
		"a disconnect reported by a generation that has ended must not drop its successor")

	require.Positive(t, c.sup.Load().staleGen.Load(), "the discarded report must be counted")

	require.NoError(t, c.Close())
}

// mustGenCapability reaches the generation-named back-channel the way an in-module transport does:
// by type assertion on the runtime, never through TransportRuntime.
// It panics rather than comma-ok'ing quietly, because a parked goroutine that skipped its injection
// would leave every assertion in these tests passing while nothing was ever injected.
// The assertion cannot actually fail — connection_lifecycle.go asserts it at compile time.
func mustGenCapability(rt TransportRuntime) genCapability {
	gc, ok := rt.(genCapability)
	if !ok {
		panic("hsms: the test runtime must offer the generation-named back-channel")
	}

	return gc
}

// staleCommitConn opens a connection whose FIRST generation parks a goroutine the bounded teardown
// join will abandon, and whose per-generation bring-up is scripted by bringUp.
// bringUp receives the 1-based Start number so a test can give the successor a different bring-up
// than the generation that will be abandoned.
// The parked goroutine runs inject with the generation identity read on the Start that spawned it —
// the same value a real transport stamps on genWG.gen — and only when the test releases it.
func staleCommitConn(t *testing.T, bringUp func(n int64, ctx context.Context, rt TransportRuntime), inject func(rt TransportRuntime, gen uint64)) (*connection, *mockTransport) {
	t.Helper()

	var starts atomic.Int64

	script := mockScript(func(ctx context.Context, rt TransportRuntime) error {
		n := starts.Add(1)
		go bringUp(n, ctx, rt)

		return nil
	})

	c, mt := newLifeConn(t, script)
	mt.abandonedGenAction = inject
	require.NoError(t, c.UpdateConfigOptions(WithT5(time.Nanosecond))) // near-immediate redial

	return c, mt
}

// TestReconnect_AbandonedGenerationCommitSelectedCannotSelectSuccessor is the Select half of the
// generation barrier, and the sharpest of the three synchronous commits.
//
// CommitSelected is NOT an event on the supervisor queue — it is a CAS applied directly to the FSM
// state by the caller's own goroutine — so step's generation match never sees it. A delayed
// correlated Select.rsp (or an inbound Select.req) processed by a recv goroutine the bounded
// teardown join abandoned would therefore flip the SUCCESSOR to Selected over a link that ran no
// handshake at all, and would leave the successor's own commit a no-op: no T7 cancel, no
// auto-linktest, and no entering-Selected event for its subscribers.
//
// Here gen N is parked across the whole cycle, gen N+1 is brought up to NotSelected only (its
// script deliberately does not select), and the parked goroutine then commits naming gen N.
// gen N+1 must stay NotSelected and no Selected event may reach a subscriber.
//
// Teeth: drop the generation check in connection.commitGate (or make commitFrom ignore it) → the
// released commit CASes NotSelected -> Selected and both assertions below fail.
func TestReconnect_AbandonedGenerationCommitSelectedCannotSelectSuccessor(t *testing.T) {
	c, mt := staleCommitConn(t,
		func(n int64, ctx context.Context, rt TransportRuntime) {
			rt.TCPUp(fakeConn{})
			if n == 1 {
				driveSelect(ctx, rt) // only gen N selects; gen N+1 parks at NotSelected
			}
		},
		func(rt TransportRuntime, gen uint64) {
			_ = mustGenCapability(rt).CommitSelectedFromGeneration(gen)
		},
	)

	var selects atomic.Int64
	cancel := c.SubscribeLifecycle(func(ev LifecycleEvent) {
		if ev.Current == SelectedState {
			selects.Add(1)
		}
	})
	defer cancel()

	require.NoError(t, c.Open(t.Context(), OpenBackground))
	requireSelected(t, c)

	genN := c.CurrentGeneration()
	require.NotZero(t, genN, "a live generation must have an identity")
	require.Equal(t, int64(1), selects.Load(), "gen N's own select must have been reported once")

	mt.simulateReadError(io.EOF) // gen N drops for real; its abandoned goroutine stays parked

	require.Eventually(t, func() bool { return mt.startCalls() == 2 && c.State() == NotSelectedState }, 3*time.Second, time.Millisecond,
		"gen N+1 must come up and park at NotSelected")
	require.NotEqual(t, genN, c.CurrentGeneration(), "gen N+1 must carry a distinct identity")

	mt.releaseAbandonedGenAction() // gen N commits its Select — while gen N+1 owns the link

	require.Never(t, func() bool {
		return c.State() != NotSelectedState || selects.Load() != 1
	}, 500*time.Millisecond, 5*time.Millisecond,
		"a Select committed by a generation that has ended must not select its successor")

	require.Positive(t, c.sup.Load().staleGen.Load(), "the refused commit must be counted")

	require.NoError(t, c.Close())
}

// TestReconnect_AbandonedGenerationSelectLostCannotDeselectSuccessor is the Deselect-responder half.
//
// CommitSelectLost is the same shape as CommitSelected — a direct CAS, not a queued event — so a
// Deselect.req answered by a recv goroutine that outlived its own generation would deselect the
// SUCCESSOR's legitimately selected link, and the successor would sit at NotSelected until its T7
// dwell dropped it.
//
// Teeth: drop the generation check → the released SelectLost CASes Selected -> NotSelected and the
// successor is deselected.
//
// It also pins SelectLostFromGeneration's own applied/refused report,
// which is the signal its hsmsss caller relies on to skip the successor-affecting work
// a real Selected -> NotSelected transition owns
// (stopping the auto-linktest, arming the T7 dwell) —
// see hsmsss's TestDeselect_StaleGenerationLeavesSuccessorLinktestArmed for the transport-level half.
// The refusal has to be visible to the caller as a plain false return,
// not merely inferable after the fact from State().
//
// Teeth (return value): make SelectLostFromGeneration always return true → deselected below reads true
// and this assertion alone fails, even though the FSM assertion still passes.
func TestReconnect_AbandonedGenerationSelectLostCannotDeselectSuccessor(t *testing.T) {
	var deselected bool
	c, mt := staleCommitConn(t,
		func(_ int64, ctx context.Context, rt TransportRuntime) {
			rt.TCPUp(fakeConn{})
			driveSelect(ctx, rt) // BOTH generations select
		},
		func(rt TransportRuntime, gen uint64) {
			deselected = mustGenCapability(rt).SelectLostFromGeneration(gen)
		},
	)

	require.NoError(t, c.Open(t.Context(), OpenBackground))
	requireSelected(t, c)

	genN := c.CurrentGeneration()
	require.NotZero(t, genN, "a live generation must have an identity")

	mt.simulateReadError(io.EOF)

	require.Eventually(t, func() bool { return mt.startCalls() == 2 && c.State() == SelectedState }, 3*time.Second, time.Millisecond,
		"gen N+1 must reselect")
	require.NotEqual(t, genN, c.CurrentGeneration(), "gen N+1 must carry a distinct identity")

	mt.releaseAbandonedGenAction() // gen N reports its Deselect — while gen N+1 owns the link

	require.False(t, deselected,
		"a Deselect answered by a generation that has ended must be refused, not just silently dropped")

	require.Never(t, func() bool { return c.State() != SelectedState }, 500*time.Millisecond, 5*time.Millisecond,
		"a Deselect answered by a generation that has ended must not deselect its successor")

	require.Positive(t, c.sup.Load().staleGen.Load(), "the refused commit must be counted")

	require.NoError(t, c.Close())
}

// TestReconnect_AbandonedGenerationTCPUpCannotResurrectAfterClose is the TCP-up half, and it is the
// one case the generation IDENTITY alone cannot decide.
//
// CommitConnected CASes out of NotConnected — the very state a generation that has ended leaves
// behind — and connection.cur keeps pointing at that dead generation until a successor is
// published, which after a Close never happens. So a passive accept goroutine abandoned by the
// bounded teardown join, resuming after Close, still matches the live generation id, and its direct
// CAS would republish a dead socket on the closed epoch and resurrect NotSelected on a connection
// the user closed. Only epoch.ended — latched at the top of teardown, under the same gate the
// commit takes — rejects it.
//
// Teeth: drop the !e.ended.Load() term from connection.commitGate → State() reports NotSelected
// after Close and the socket assertion fails; keep it and both hold.
//
// It also pins TCPUpFromGeneration's own accept/refuse report
// (the signal [connection.TCPUpFromGeneration]'s hsmsss caller relies on
// to close the socket itself and skip spawning a recv loop —
// see hsmsss's TestActive_RefusedTCPUpClosesConnAndSkipsRecvLoop /
// TestPassive_RefusedTCPUpClosesConnAndSkipsRecvLoop
// for the transport-level half of that contract):
// the refusal this test proves at the FSM/socket level
// must also be visible to the caller as a plain false return,
// not just inferable after the fact from State()/liveConn().
//
// Teeth (return value): make TCPUpFromGeneration always return true → accepted below reads true
// and this assertion alone fails, even though the FSM/socket assertions still pass.
func TestReconnect_AbandonedGenerationTCPUpCannotResurrectAfterClose(t *testing.T) {
	conn := fakeConn{}

	var accepted bool
	c, mt := staleCommitConn(t,
		func(_ int64, _ context.Context, _ TransportRuntime) {}, // never connects: the FSM stays NotConnected
		func(rt TransportRuntime, gen uint64) {
			accepted = mustGenCapability(rt).TCPUpFromGeneration(gen, conn)
		},
	)

	require.NoError(t, c.Open(t.Context(), OpenBackground))
	require.Equal(t, NotConnectedState, c.State())

	genN := c.CurrentGeneration()
	require.NotZero(t, genN, "a live generation must have an identity")

	require.NoError(t, c.Close())

	e := c.cur.Load()
	require.Equal(t, genN, e.id, "Close does not publish a successor, so the closed generation is still the current one")

	mt.releaseAbandonedGenAction() // the abandoned accept goroutine reports its socket, after Close

	require.False(t, accepted, "a TCP-up reported by a generation that has ended must be refused, not just silently dropped")
	require.Equal(t, NotConnectedState, c.State(),
		"a TCP-up reported by a generation that has ended must not resurrect the FSM")
	require.Nil(t, e.liveConn(), "a TCP-up reported by a generation that has ended must not republish a socket on it")
	require.Positive(t, c.sup.Load().staleGen.Load(), "the refused commit must be counted")
}

// TestReconnect_AbandonedGenerationT7TimeoutCannotDropSuccessor is the T7-dwell half of the
// disconnect barrier, and the last of genCapability's five methods to get dedicated coverage.
//
// T7Expired is not a synchronous commit like CommitSelected/SelectLost/TCPUp.
// injectT7Expiry goes straight to s.injectFrom with no pre-check of its own,
// so it is QUEUED exactly like TCPDown,
// and the only thing standing between a stale dwell timer and a live successor is step's generation match (supervisor.go).
// That makes staleGen here a clean, specific proxy for that one check:
// no other guard on this path could have incremented it.
//
// evT7Timeout is legal ONLY from NotSelected (supervisor.go's transition table);
// from Selected it is a no-op regardless of generation.
// So proving anything requires gen N+1 to ALSO be parked at NotSelected.
// A Selected successor would pass even with the barrier deleted, because the transition itself would be illegal on cause, not generation.
// Both generations' bring-up therefore stops at TCPUp and never selects.
//
// Here gen N's dwell timer is parked across the whole reconnect cycle
// (the mock's action release stands in for a real T7 timer goroutine the bounded teardown join abandoned),
// gen N+1 comes up to NotSelected only, and the parked goroutine then fires T7Expired naming gen N.
// gen N+1 must stay NotSelected and report no NotConnected transition.
//
// Teeth: drop the generation match in supervisor.step (cmd.gen != 0 && s.curGen() != cmd.gen) —
// the released expiry legally transitions gen N+1's NotSelected -> NotConnected,
// a third generation is dialed,
// and a second NotConnected event with CauseT7Timeout reaches the subscriber.
func TestReconnect_AbandonedGenerationT7TimeoutCannotDropSuccessor(t *testing.T) {
	c, mt := staleCommitConn(t,
		func(_ int64, _ context.Context, rt TransportRuntime) {
			rt.TCPUp(fakeConn{}) // neither generation selects; both park at NotSelected
		},
		func(rt TransportRuntime, gen uint64) {
			mustGenCapability(rt).T7ExpiredFromGeneration(gen)
		},
	)

	var mu sync.Mutex
	var drops []TransitionCause
	cancel := c.SubscribeLifecycle(func(ev LifecycleEvent) {
		if ev.Current != NotConnectedState {
			return
		}
		mu.Lock()
		drops = append(drops, ev.Cause)
		mu.Unlock()
	})
	defer cancel()

	dropCount := func() int {
		mu.Lock()
		defer mu.Unlock()

		return len(drops)
	}

	require.NoError(t, c.Open(t.Context(), OpenBackground))
	require.Eventually(t, func() bool { return c.State() == NotSelectedState }, 3*time.Second, time.Millisecond,
		"gen N must come up and park at NotSelected")

	genN := c.CurrentGeneration()
	require.NotZero(t, genN, "a live generation must have an identity")

	mt.simulateReadError(io.EOF) // gen N drops for real; its abandoned goroutine stays parked

	require.Eventually(t, func() bool { return mt.startCalls() == 2 && c.State() == NotSelectedState }, 3*time.Second, time.Millisecond,
		"gen N+1 must come up and park at NotSelected")
	require.Eventually(t, func() bool { return dropCount() == 1 }, time.Second, time.Millisecond,
		"the real gen-N drop must have been reported exactly once")

	require.NotEqual(t, genN, c.CurrentGeneration(), "gen N+1 must carry a distinct identity")

	mt.releaseAbandonedGenAction() // gen N's T7 dwell fires -- while gen N+1 owns the link

	require.Never(t, func() bool {
		return mt.startCalls() > 2 || c.State() != NotSelectedState || dropCount() > 1
	}, 500*time.Millisecond, 5*time.Millisecond,
		"a T7 dwell expiry from a generation that has ended must not drop its successor")

	require.Positive(t, c.sup.Load().staleGen.Load(), "the stale T7 expiry must be counted")

	require.NoError(t, c.Close())
}

// TestCommitGate_FencesTheCASAgainstMarkEnded tests the RWMutex ordering itself, which is the whole
// safety property of the generation-guarded synchronous commits and the one thing every other test
// in this file takes on trust.
//
// The end-to-end tests above release their stale commit only after the successor is already
// published, so they exercise an already-stale IDENTITY check.
// TestSupervisor_CommitFromGenerationHonorsTheGate replaces the gate with a fixed-answer closure,
// so it cannot see the ordering either.
// Neither would notice a gate that checks liveness under the RLock, releases it, and then runs the
// CAS: every one of their assertions still holds, while the ABA window the gate exists to close
// reopens — teardown can latch ended, complete its join, and let a successor be published, all
// between the check and a CAS that then lands on that successor.
//
// So this drives the real connection.commitGate with a CAS that parks inside the gate, and proves
// the two halves of the fence directly:
// a teardown latching ended cannot complete while a commit is mid-CAS,
// and a commit arriving after that latch is refused without its CAS ever running.
//
// Teeth: move the CAS out of the RLock section in connection.commitGate (RUnlock right after the
// liveness check) → markEnded completes while the CAS is still parked and the first subtest fails.
func TestCommitGate_FencesTheCASAgainstMarkEnded(t *testing.T) {
	// A generation that comes up and parks: the gate cares only about identity and the ended latch,
	// so the CAS below is supplied by the test rather than driven through the FSM.
	openParkedGeneration := func(t *testing.T) (*connection, *epoch) {
		t.Helper()

		c, _ := newLifeConn(t, mockScript(func(_ context.Context, _ TransportRuntime) error { return nil }))
		require.NoError(t, c.Open(t.Context(), OpenBackground))

		e := c.cur.Load()
		require.NotNil(t, e, "Open must publish a generation")
		require.NotZero(t, e.id, "a published generation must carry an identity")

		return c, e
	}

	t.Run("markEnded waits for a CAS already inside the gate", func(t *testing.T) {
		c, e := openParkedGeneration(t)

		inCAS := make(chan struct{})   // closed once the CAS is running inside the gate
		release := make(chan struct{}) // the test releases the parked CAS
		gateDone := make(chan struct{})

		var committed, live bool
		go func() {
			committed, live = c.commitGate(e.id, func() bool {
				close(inCAS)
				<-release

				return true
			})
			close(gateDone)
		}()

		<-inCAS

		ended := make(chan struct{})
		go func() {
			e.markEnded()
			close(ended)
		}()

		endedReturned := func() bool {
			select {
			case <-ended:
				return true
			default:
				return false
			}
		}

		require.Never(t, endedReturned, 200*time.Millisecond, 5*time.Millisecond,
			"a generation must not be able to end while a commit it admitted is still inside its CAS")
		require.False(t, e.ended.Load(), "the latch itself must still be unset, not merely unobserved")

		close(release)
		<-gateDone
		<-ended

		require.True(t, live, "the generation was live when the gate admitted the commit")
		require.True(t, committed, "the CAS ran and reported success")
		require.True(t, e.ended.Load(), "markEnded completes once the gate is released")

		require.NoError(t, c.Close())
	})

	t.Run("a commit after the latch is refused without running its CAS", func(t *testing.T) {
		c, e := openParkedGeneration(t)

		e.markEnded() // teardown wins the gate first

		var casRan bool
		committed, live := c.commitGate(e.id, func() bool {
			casRan = true

			return true
		})

		require.False(t, live, "a generation whose teardown latched ended is no longer live")
		require.False(t, committed, "a refused commit reports no commit")
		require.False(t, casRan, "the CAS must never run for a refused commit")

		require.NoError(t, c.Close())
	})
}
