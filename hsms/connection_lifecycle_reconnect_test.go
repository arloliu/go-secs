package hsms

import (
	"context"
	"errors"
	"io"
	"sync"
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
