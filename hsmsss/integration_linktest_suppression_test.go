package hsmsss

// integration_linktest_suppression_test.go — behavior suite for activity-based linktest
// suppression (hsms.WithLinktestSuppression, default ON). Rules under test:
//   1. activity reset  — no Linktest.req while the line saw a frame within the last interval
//   2. inflight skip   — no Linktest.req while a sent data message awaits its reply
//   3. liveness credit — a T6-failed probe is not counted toward the disconnect threshold
//                        while the link shows other signs of life
// All negative ("no probe") assertions are baseline-delta: a legal idle probe may fire
// between Selected-entry and the test's first controlled action, so each test establishes
// its precondition, snapshots LinktestSendCount, and asserts no growth afterwards.
// LinktestSendCount is an attempt counter incremented before the frame is written
// (transport_procedures.go), so causal on-wire ordering is asserted via the ChaosProxy
// filter where it matters (rule 3). Uses the shared harness (newEndpoint / echoHandler /
// waitSelected / controlMetrics / assertStaysSelected / ChaosProxy / waitNotConnectedEvent).
// Not t.Parallel: interval/T6 timing runs cleaner sequentially.

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

const (
	// suppInterval is deliberately generous relative to the traffic cadence used below
	// (interval/5) so scheduler jitter under -race cannot open a full-interval silence
	// gap between two pings and let a legitimate probe through mid-test.
	suppInterval = 300 * time.Millisecond
	suppT6       = 500 * time.Millisecond
)

// TestHSMS_LinktestSuppression_BusyLinkSendsNoLinktest: rule 1. After one controlled
// round trip establishes the activity baseline, continuous request/reply traffic at
// interval/5 keeps the line busy: not a single NEW Linktest.req attempt may be made.
func TestHSMS_LinktestSuppression_BusyLinkSendsNoLinktest(t *testing.T) {
	ctx := t.Context()
	port := freeLoopbackPort(t)

	passive := newEndpoint(t, port, false, nil, echoHandler)
	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, passive)

	active := newEndpoint(t, port, true, []Option{
		WithConnectionOption(hsms.WithLinktestInterval(suppInterval)),
		WithConnectionOption(hsms.WithT6(suppT6)),
	})
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, active)

	waitSelected(t, active)
	waitSelected(t, passive)

	m := controlMetrics(t, active)

	// Prime the activity window, then baseline the attempt counter.
	_, err := active.conn.SendDataMessage(ctx, 99, 1, true, secs2.NewASCIIItem("prime"))
	require.NoError(t, err)
	baseSend := m.LinktestSendCount()
	baseSupp := m.LinktestSuppressedCount()

	// Drive request/reply pings every interval/5 for 10 intervals: the line is never idle
	// for a full interval, so rule 1 must suppress every probe opportunity.
	deadline := time.Now().Add(10 * suppInterval)
	for time.Now().Before(deadline) {
		_, err := active.conn.SendDataMessage(ctx, 99, 1, true, secs2.NewASCIIItem("ping"))
		require.NoError(t, err)
		time.Sleep(suppInterval / 5) // scenario pacing, not state-waiting
	}

	require.Equal(t, baseSend, m.LinktestSendCount(),
		"no new Linktest.req attempt may be made once traffic keeps the line busy")
	require.Greater(t, m.LinktestSuppressedCount(), baseSupp,
		"the suppressed-probe counter must record the skipped opportunities")
	require.Equal(t, hsms.SelectedState, active.conn.State())
}

// TestHSMS_LinktestSuppression_IdleLinkStillProbes: the over-suppression guard. A fully
// idle link must be probed at the configured cadence exactly as without suppression —
// this preserves dead-link detection on quiet links.
func TestHSMS_LinktestSuppression_IdleLinkStillProbes(t *testing.T) {
	ctx := t.Context()
	port := freeLoopbackPort(t)

	passive := newEndpoint(t, port, false, nil)
	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, passive)

	active := newEndpoint(t, port, true, []Option{
		WithConnectionOption(hsms.WithLinktestInterval(suppInterval)),
		WithConnectionOption(hsms.WithT6(suppT6)),
	})
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, active)

	waitSelected(t, active)
	waitSelected(t, passive)

	m := controlMetrics(t, active)

	// No traffic at all after Select: at least 3 successful probes within a generous window.
	require.Eventually(t, func() bool {
		return m.LinktestRecvCount() >= 3
	}, 20*suppInterval, 10*time.Millisecond,
		"an idle link must still be probed every interval (no over-suppression)")
	require.Equal(t, hsms.SelectedState, active.conn.State())
}

// TestHSMS_LinktestSuppression_DisabledRestoresUnconditionalProbing: the knob. With
// WithLinktestSuppression(false), probes fire despite continuous traffic (v2.0.x behavior).
func TestHSMS_LinktestSuppression_DisabledRestoresUnconditionalProbing(t *testing.T) {
	ctx := t.Context()
	port := freeLoopbackPort(t)

	passive := newEndpoint(t, port, false, nil, echoHandler)
	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, passive)

	active := newEndpoint(t, port, true, []Option{
		WithConnectionOption(hsms.WithLinktestInterval(suppInterval)),
		WithConnectionOption(hsms.WithT6(suppT6)),
		WithConnectionOption(hsms.WithLinktestSuppression(false)),
	})
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, active)

	waitSelected(t, active)
	waitSelected(t, passive)

	m := controlMetrics(t, active)

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = active.conn.SendDataMessage(ctx, 99, 1, true, secs2.NewASCIIItem("ping"))
				time.Sleep(suppInterval / 5) // scenario pacing
			}
		}
	}()
	t.Cleanup(func() { close(stop); <-done })

	require.Eventually(t, func() bool {
		return m.LinktestSendCount() >= 2
	}, 20*suppInterval, 10*time.Millisecond,
		"with suppression disabled, probes must fire despite continuous traffic")
	require.Zero(t, m.LinktestSuppressedCount(),
		"the suppression counter must stay zero when the feature is off")
}

// slowEchoHandler replies after a deliberate processing delay, simulating aged equipment
// grinding through a long command (e.g. a recipe read). The delay is scenario injection.
func slowEchoHandler(delay time.Duration) hsms.DataMessageHandler {
	return func(msg *hsms.DataMessage, ep hsms.SECS2Endpoint) {
		if !msg.WaitBit() {
			return
		}
		item, err := msg.Item()
		if err != nil {
			return
		}
		go func() { // off the recv goroutine: handlers must not block it
			time.Sleep(delay) // the simulated processing time itself
			_ = ep.ReplyDataMessage(context.Background(), msg, item)
		}()
	}
}

// TestHSMS_LinktestSuppression_NoProbeWhileAwaitingReply: rule 2. During a long silent
// wait for a data reply (equipment busy processing), no NEW probe attempt may be made
// even though the line is idle far beyond the linktest interval — T3 owns that window.
// The baseline is taken only after the transaction is confirmed inflight, so a legal
// idle probe from the pre-send window cannot fail the assertion.
func TestHSMS_LinktestSuppression_NoProbeWhileAwaitingReply(t *testing.T) {
	ctx := t.Context()
	port := freeLoopbackPort(t)

	const processing = 4 * suppInterval // 1.2s of line silence while "processing" (T3 base is 3s)

	passive := newEndpoint(t, port, false, nil, slowEchoHandler(processing))
	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, passive)

	active := newEndpoint(t, port, true, []Option{
		WithConnectionOption(hsms.WithLinktestInterval(suppInterval)),
		WithConnectionOption(hsms.WithT6(suppT6)),
	})
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, active)

	waitSelected(t, active)
	waitSelected(t, passive)

	m := controlMetrics(t, active)
	coreMetrics := active.conn.Metrics()

	done := make(chan error, 1)
	go func() {
		_, err := active.conn.SendDataMessage(ctx, 7, 5, true, secs2.NewASCIIItem("recipe?"))
		done <- err
	}()

	// Gate: the transaction is on the wire and awaiting its reply.
	require.Eventually(t, func() bool {
		return coreMetrics.DataMsgInflightCount() == 1
	}, 5*time.Second, 5*time.Millisecond, "the slow transaction must become inflight")

	baseSend := m.LinktestSendCount()

	select {
	case err := <-done:
		require.NoError(t, err, "the slow transaction must complete within T3")
	case <-time.After(10 * time.Second):
		t.Fatal("slow transaction did not complete")
	}

	require.Equal(t, baseSend, m.LinktestSendCount(),
		"no probe attempt may be made while the reply was outstanding, despite >interval of silence")
	require.Positive(t, m.LinktestSuppressedCount())
	require.Equal(t, hsms.SelectedState, active.conn.State())
}

// TestHSMS_LinktestSuppression_LivenessCreditForgivesFailures: rule 3 (phase A), then
// proves the disconnect threshold still has teeth once all signs of life stop (phase B).
func TestHSMS_LinktestSuppression_LivenessCreditForgivesFailures(t *testing.T) {
	ctx := t.Context()
	portP := freeLoopbackPort(t)

	passive := newEndpoint(t, portP, false, nil)
	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, passive)

	// probeSeen fires when the proxy observes an outbound Linktest.req ON THE WIRE —
	// the causal anchor for "this frame arrived after that probe".
	probeSeen := make(chan struct{}, 16)

	proxy := newChaosProxy(t, portP)
	proxy.SetFilter(func(isClientToTarget bool, header []byte, _ []byte) (ProxyAction, time.Duration) {
		if len(header) < 10 {
			return ProxyActionForward, 0
		}
		if isClientToTarget && header[5] == byte(hsms.LinktestReqType) {
			select {
			case probeSeen <- struct{}{}:
			default:
			}
			return ProxyActionForward, 0
		}
		if !isClientToTarget && header[5] == byte(hsms.LinktestRspType) {
			return ProxyActionDrop, 0 // every probe of this test times out on T6
		}

		return ProxyActionForward, 0
	})
	proxy.Start(t)
	t.Cleanup(proxy.Stop)

	active := newEndpoint(t, proxy.Port(), true, []Option{
		WithConnectionOption(hsms.WithLinktestInterval(suppInterval)),
		WithConnectionOption(hsms.WithT6(suppT6)),
		WithConnectionOption(hsms.WithLinktestFailThreshold(3)),
	})
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, active)

	waitSelected(t, active)
	waitSelected(t, passive)
	drainStateCh(active.states)

	m := controlMetrics(t, active)

	// Phase A: for each of 4 rounds (> threshold), wait until a probe is on the wire,
	// then deliver proof of life inside its T6 window. Every timeout must be credited.
	for round := 1; round <= 4; round++ {
		select {
		case <-probeSeen:
		case <-time.After(30 * suppInterval):
			t.Fatalf("round %d: no Linktest.req observed on the wire", round)
		}

		_, sendErr := passive.conn.SendDataMessage(ctx, 6, 11, false, secs2.NewASCIIItem("alive"))
		require.NoError(t, sendErr)

		errsWant := uint64(round)
		require.Eventually(t, func() bool {
			return m.LinktestErrCount() >= errsWant && m.LinktestCreditedCount() >= errsWant
		}, 30*suppInterval, 5*time.Millisecond, "round %d: the T6 failure must be credited", round)
	}
	require.GreaterOrEqual(t, m.LinktestCreditedCount(), uint64(3),
		"at least threshold-many failures were credited")
	assertStaysSelected(t, active, suppT6+2*suppInterval)

	// Phase B: stop injecting life. Consecutive uncredited failures must now reach the
	// threshold and drop the link — credit must never mask a link that stopped showing life.
	waitNotConnectedEvent(t, active)
}

// TestHSMS_LinktestSuppression_LifeBetweenFailuresRestartsRun: SMOKE for the failure-memory
// wiring (the deterministic proof is TestLinktestFailureStep_Sequence). With threshold 2
// and every Linktest.rsp dropped, one failure is recorded, a data frame is injected, and
// the disconnect must not arrive before cumulative LinktestErrCount >= 3 under any of the
// admitted interleavings (between-probes restart, or direct credit on a pending probe).
func TestHSMS_LinktestSuppression_LifeBetweenFailuresRestartsRun(t *testing.T) {
	ctx := t.Context()
	portP := freeLoopbackPort(t)

	passive := newEndpoint(t, portP, false, nil)
	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, passive)

	proxy := newChaosProxy(t, portP)
	proxy.SetFilter(func(isClientToTarget bool, header []byte, _ []byte) (ProxyAction, time.Duration) {
		if !isClientToTarget && len(header) >= 10 && header[5] == byte(hsms.LinktestRspType) {
			return ProxyActionDrop, 0
		}
		return ProxyActionForward, 0
	})
	proxy.Start(t)
	t.Cleanup(proxy.Stop)

	active := newEndpoint(t, proxy.Port(), true, []Option{
		WithConnectionOption(hsms.WithLinktestInterval(suppInterval)),
		WithConnectionOption(hsms.WithT6(suppT6)),
		WithConnectionOption(hsms.WithLinktestFailThreshold(2)),
	})
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, active)

	waitSelected(t, active)
	waitSelected(t, passive)
	drainStateCh(active.states)

	m := controlMetrics(t, active)

	// Failure #1 is counted (no life anywhere near it).
	require.Eventually(t, func() bool {
		return m.LinktestErrCount() >= 1
	}, 30*suppInterval, 5*time.Millisecond, "first probe failure must be recorded")

	// Life BETWEEN probes: the failed probe has resolved (err counted); the next probe
	// needs a fresh interval of silence after this frame, so it provably lands between.
	_, sendErr := passive.conn.SendDataMessage(ctx, 6, 11, false, secs2.NewASCIIItem("alive"))
	require.NoError(t, sendErr)

	// No further life: the run must restart at 1 (errs=2), then reach 2 (errs=3) -> drop.
	waitNotConnectedEvent(t, active)
	require.GreaterOrEqual(t, m.LinktestErrCount(), uint64(3),
		"the run must restart after mid-run life: disconnect at >= 3 cumulative errors, not 2")
}

// TestHSMS_LinktestSuppression_WedgedPeerStepsInAfterT3: the end-to-end guarantee. A peer
// that accepts a command and then goes fully silent (no reply, no linktest answers) is
// (1) left unprobed while the reply is outstanding, (2) surfaced to the caller as a T3
// reply timeout at the caller's configured deadline, and (3) disconnected after by the
// resumed auto-linktest. Total detection ~= T3 + threshold*(interval + T6).
func TestHSMS_LinktestSuppression_WedgedPeerStepsInAfterT3(t *testing.T) {
	ctx := t.Context()
	portP := freeLoopbackPort(t)

	// The passive never replies to data (no handler) — a wedged command processor.
	passive := newEndpoint(t, portP, false, nil)
	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, passive)

	proxy := newChaosProxy(t, portP)
	proxy.SetFilter(func(isClientToTarget bool, header []byte, _ []byte) (ProxyAction, time.Duration) {
		if !isClientToTarget && len(header) >= 10 && header[5] == byte(hsms.LinktestRspType) {
			return ProxyActionDrop, 0 // linktests are never answered either
		}
		return ProxyActionForward, 0
	})
	proxy.Start(t)
	t.Cleanup(proxy.Stop)

	const shortT3 = 1 * time.Second

	active := newEndpoint(t, proxy.Port(), true, []Option{
		WithConnectionOption(hsms.WithT3(shortT3)),
		WithConnectionOption(hsms.WithLinktestInterval(suppInterval)),
		WithConnectionOption(hsms.WithT6(suppT6)),
		WithConnectionOption(hsms.WithLinktestFailThreshold(2)),
	})
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, active)

	waitSelected(t, active)
	waitSelected(t, passive)
	drainStateCh(active.states)

	m := controlMetrics(t, active)
	coreMetrics := active.conn.Metrics()

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := active.conn.SendDataMessage(ctx, 7, 5, true, secs2.NewASCIIItem("recipe?"))
		done <- err
	}()

	require.Eventually(t, func() bool {
		return coreMetrics.DataMsgInflightCount() == 1
	}, 5*time.Second, 5*time.Millisecond, "the doomed transaction must become inflight")

	baseSend := m.LinktestSendCount()

	// (1)+(2): the transaction fails at the T3 deadline, and no probe attempt was made
	// while it was outstanding.
	var sendErr error
	select {
	case sendErr = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("wedged transaction never resolved")
	}
	elapsed := time.Since(start)
	require.ErrorIs(t, sendErr, hsms.ErrT3Timeout, "the wedged transaction must fail with a T3 reply timeout")
	require.GreaterOrEqual(t, elapsed, shortT3-50*time.Millisecond,
		"the caller must get the full configured T3 window")
	require.Equal(t, baseSend, m.LinktestSendCount(),
		"no probe attempt may be made while the reply was outstanding")

	// (3): with nothing inflight and the line silent, probing resumes; threshold
	// unanswered probes later the dead link is dropped. waitNotConnectedEvent observes
	// the state EVENT, so the auto-reconnect that follows cannot hide the disconnect.
	waitNotConnectedEvent(t, active)
	require.GreaterOrEqual(t, m.LinktestSendCount(), baseSend+2,
		"the disconnect must have been driven by resumed, unanswered probes")
}

// TestHSMS_LinktestSuppression_AsyncTrafficSuppresses: continuous fire-and-forget sends
// (no W-bit, so nothing inflight) still count as line activity — probes stay suppressed.
// This is the documented trade-off for streaming/relay workloads; the knob-off test above
// proves the escape hatch.
func TestHSMS_LinktestSuppression_AsyncTrafficSuppresses(t *testing.T) {
	ctx := t.Context()
	port := freeLoopbackPort(t)

	passive := newEndpoint(t, port, false, nil)
	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, passive)

	active := newEndpoint(t, port, true, []Option{
		WithConnectionOption(hsms.WithLinktestInterval(suppInterval)),
		WithConnectionOption(hsms.WithT6(suppT6)),
	})
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, active)

	waitSelected(t, active)
	waitSelected(t, passive)

	m := controlMetrics(t, active)

	// Prime with one no-reply send, then baseline.
	_, err := active.conn.SendDataMessage(ctx, 6, 11, false, secs2.NewASCIIItem("prime"))
	require.NoError(t, err)
	baseSend := m.LinktestSendCount()

	deadline := time.Now().Add(8 * suppInterval)
	for time.Now().Before(deadline) {
		_, err := active.conn.SendDataMessage(ctx, 6, 11, false, secs2.NewASCIIItem("stream"))
		require.NoError(t, err)
		time.Sleep(suppInterval / 5) // scenario pacing
	}

	require.Equal(t, baseSend, m.LinktestSendCount(),
		"fire-and-forget traffic must suppress probes (documented starvation trade-off)")
	require.Equal(t, hsms.SelectedState, active.conn.State())
}

// TestHSMS_LinktestSuppression_ReconfigAppliesNextSelected: a mid-session
// UpdateConfigOptions(WithLinktestSuppression(false)) does NOT change the current
// Selected session (its goroutine captured the flag at entry), but DOES apply to the
// re-Selected successor after a forced reconnect.
func TestHSMS_LinktestSuppression_ReconfigAppliesNextSelected(t *testing.T) {
	ctx := t.Context()
	portP := freeLoopbackPort(t)

	passive := newEndpoint(t, portP, false, nil, echoHandler)
	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, passive)

	proxy := newChaosProxy(t, portP) // no filter: pure pass-through + kill switch
	proxy.Start(t)
	t.Cleanup(proxy.Stop)

	active := newEndpoint(t, proxy.Port(), true, []Option{
		WithConnectionOption(hsms.WithLinktestInterval(suppInterval)),
		WithConnectionOption(hsms.WithT6(suppT6)),
	})
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, active)

	waitSelected(t, active)
	waitSelected(t, passive)
	drainStateCh(active.states)

	m := controlMetrics(t, active)

	// Toggle mid-session: current session must keep suppressing.
	require.NoError(t, active.conn.UpdateConfigOptions(hsms.WithLinktestSuppression(false)))

	_, err := active.conn.SendDataMessage(ctx, 99, 1, true, secs2.NewASCIIItem("prime"))
	require.NoError(t, err)
	baseSend := m.LinktestSendCount()

	deadline := time.Now().Add(5 * suppInterval)
	for time.Now().Before(deadline) {
		_, err := active.conn.SendDataMessage(ctx, 99, 1, true, secs2.NewASCIIItem("ping"))
		require.NoError(t, err)
		time.Sleep(suppInterval / 5) // scenario pacing
	}
	require.Equal(t, baseSend, m.LinktestSendCount(),
		"the current session must retain its captured suppression=on")

	// Force a reconnect; the successor session captures suppression=off at entry.
	proxy.CloseConnections()
	waitNotConnectedEvent(t, active)
	waitSelected(t, active)
	drainStateCh(active.states)

	baseSend = m.LinktestSendCount()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = active.conn.SendDataMessage(ctx, 99, 1, true, secs2.NewASCIIItem("ping"))
				time.Sleep(suppInterval / 5)
			}
		}
	}()
	t.Cleanup(func() { close(stop); <-done })

	require.Eventually(t, func() bool {
		return m.LinktestSendCount() > baseSend
	}, 20*suppInterval, 10*time.Millisecond,
		"the re-Selected session must probe despite traffic: suppression=off applied at entry")
}

// TestHSMS_LinktestSuppression_InflightGaugeTerminalOutcomes: reply, T3 timeout, caller
// cancellation, and connection drop each return DataMsgInflightCount to zero — the gauge
// the inflight-skip rule depends on. A leak would disable dead-link detection forever.
func TestHSMS_LinktestSuppression_InflightGaugeTerminalOutcomes(t *testing.T) {
	ctx := t.Context()
	portP := freeLoopbackPort(t)

	// Passive replies only to S99 (echoHandler ignores nothing, so use a selective handler):
	// S99Fx gets an echo; S7Fx gets silence (drives T3/cancel/drop cases).
	selective := func(msg *hsms.DataMessage, ep hsms.SECS2Endpoint) {
		if msg.Stream() != 99 || !msg.WaitBit() {
			return
		}
		item, err := msg.Item()
		if err != nil {
			return
		}
		_ = ep.ReplyDataMessage(context.Background(), msg, item)
	}

	passive := newEndpoint(t, portP, false, nil, selective)
	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, passive)

	proxy := newChaosProxy(t, portP)
	proxy.Start(t)
	t.Cleanup(proxy.Stop)

	active := newEndpoint(t, proxy.Port(), true, []Option{
		WithConnectionOption(hsms.WithT3(1 * time.Second)),
		WithConnectionOption(hsms.WithLinktestInterval(suppInterval)),
		WithConnectionOption(hsms.WithT6(suppT6)),
	})
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, active)

	waitSelected(t, active)
	waitSelected(t, passive)

	gauge := active.conn.Metrics()
	zero := func(label string) {
		require.Eventually(t, func() bool {
			return gauge.DataMsgInflightCount() == 0
		}, 5*time.Second, 5*time.Millisecond, "gauge must return to zero after %s", label)
	}

	// 1. Reply.
	_, err := active.conn.SendDataMessage(ctx, 99, 1, true, secs2.NewASCIIItem("ok"))
	require.NoError(t, err)
	zero("reply")

	// 2. T3 timeout (S7 gets no reply). After the terminal, prove probing RESUMES —
	// the gauge returning to zero is only half the promise; a wedged suppression
	// state would still never probe again.
	_, err = active.conn.SendDataMessage(ctx, 7, 5, true, secs2.NewASCIIItem("t3"))
	require.Error(t, err)
	zero("T3 timeout")

	probeBase := controlMetrics(t, active).LinktestSendCount()
	require.Eventually(t, func() bool {
		return controlMetrics(t, active).LinktestSendCount() > probeBase
	}, 20*suppInterval, 10*time.Millisecond,
		"idle probing must resume once the T3 terminal cleared the gauge")

	// 3. Caller cancellation.
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := active.conn.SendDataMessage(cctx, 7, 5, true, secs2.NewASCIIItem("cancel"))
		done <- err
	}()
	require.Eventually(t, func() bool { return gauge.DataMsgInflightCount() == 1 },
		5*time.Second, 5*time.Millisecond)
	cancel()
	require.Error(t, <-done)
	zero("caller cancellation")

	// 4. Connection drop mid-wait. After the drop, wait for the auto-reconnect to
	// re-Select, then prove idle probing resumes in the successor session too.
	drainStateCh(active.states)
	go func() {
		_, _ = active.conn.SendDataMessage(ctx, 7, 5, true, secs2.NewASCIIItem("drop"))
	}()
	require.Eventually(t, func() bool { return gauge.DataMsgInflightCount() == 1 },
		5*time.Second, 5*time.Millisecond)
	proxy.CloseConnections()
	waitNotConnectedEvent(t, active)
	zero("connection drop")

	waitSelected(t, active)
	probeBase = controlMetrics(t, active).LinktestSendCount()
	require.Eventually(t, func() bool {
		return controlMetrics(t, active).LinktestSendCount() > probeBase
	}, 20*suppInterval, 10*time.Millisecond,
		"the re-Selected session must resume idle probing after the drop terminal")
}
