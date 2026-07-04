package hsmsss

// linktest_threshold_test.go — v2 port of the v1 tests/hsmsss_integration/linktest_threshold_test.go,
// upgraded to assert on the now-LIVE linktest metrics (Metrics().Linktest{Send,Recv,Err}Count()).
//
// It exercises the auto-linktest failure-threshold mechanism (T23/T24, hsmsss/procedures.go runLinktest)
// with threshold=3 and a mid-sequence successful reply, proving that the internal CONSECUTIVE-failure
// counter (which drives the involuntary disconnect) resets on success — while the CUMULATIVE
// LinktestErrCount metric does NOT reset. The two counters being distinct is the load-bearing property:
// LinktestErrCount climbs to 4 (> threshold 3) with the connection STILL Selected, because no run of 3
// consecutive failures ever occurs until the final phase.
//
// A frame-level ChaosProxy sits between active and passive; a stateful filter counts passive->active
// Linktest.rsp ordinals and drops/forwards per a deterministic pattern (drop is by ORDINAL, not timing,
// so the consecutive-run structure is timing-independent — jitter only slows the test, never changes
// which run of drops trips the threshold):
//
//   rsp #1 drop  #2 drop   -> phase 1: internal fails->2, STILL Selected (threshold 3 not tripped)
//   rsp #3 forward         -> phase 2: internal fails RESET to 0 (LinktestRecvCount++)
//   rsp #4 drop  #5 drop   -> phase 3: internal fails->2 again, STILL Selected; cumulative errs now 4 (>3)
//   rsp #6 forward         -> phase 4: internal fails RESET to 0 again (LinktestRecvCount++)
//   rsp #7 #8 #9 drop      -> phase 4: 3 CONSECUTIVE fails -> threshold -> involuntary disconnect
//   rsp #10+ forward       -> post-reconnect health (auto-reconnect re-Selects; no further errors)
//
// The connection is package-white-box (package hsmsss) so it reuses the shared harness (newEndpoint /
// waitSelected / closeEndpoint / drainStateCh / freeLoopbackPort), the frame ChaosProxy
// (chaos_proxy_test.go), and waitNotConnectedEvent (chaos_scenarios_test.go). Auto-reconnect is ALWAYS
// ON in v2, so the final "-> NotConnected" is asserted via the state-change EVENT (waitNotConnectedEvent),
// not an instantaneous poll. Not t.Parallel: the multi-cycle T6 timing runs cleaner in the sequential phase.

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/stretchr/testify/require"
)

const (
	// interval and T6 are kept modest so the full 9-round sequence completes in a few seconds while
	// leaving each phase (dropped cycle ~= T6+interval) well separated from the next under -race jitter.
	ltThresholdInterval = 100 * time.Millisecond
	ltThresholdT6       = 250 * time.Millisecond
	// staysWindow is the bounded window over which the connection must remain Selected. It is a fixed
	// wall-clock duration (< one dropped-cycle beyond the phase gate) and can never overrun into the
	// phase-4 disconnect, which is at minimum 3*(T6+interval) after the phase-3 gate (timers never fire early).
	ltStaysWindow = ltThresholdT6 + 100*time.Millisecond
)

func TestHSMS_LinktestFailThreshold_ResetsOnSuccess(t *testing.T) {
	ctx := t.Context()
	portP := freeLoopbackPort(t)

	// Passive endpoint: auto-linktest off (newEndpoint base LinktestInterval 0); it only answers the
	// active's Linktest.req with a Linktest.rsp (handleLinktestReq).
	passive := newEndpoint(t, portP, false, nil)
	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, passive)

	// Chaos proxy: forward everything except passive->active Linktest.rsp, dropped per the ordinal pattern.
	proxy := newChaosProxy(t, portP)

	var rspOrdinal atomic.Int32
	proxy.SetFilter(func(isClientToTarget bool, header []byte, _ []byte) (ProxyAction, time.Duration) {
		// Only passive->active Linktest.rsp frames are subject to the drop pattern; everything else
		// (the Select handshake, the active->passive Linktest.req) is forwarded so the link establishes.
		if isClientToTarget {
			return ProxyActionForward, 0
		}
		if len(header) < 10 || header[5] != byte(hsms.LinktestRspType) {
			return ProxyActionForward, 0
		}

		switch rspOrdinal.Add(1) {
		case 1, 2, 4, 5, 7, 8, 9: // phase 1 {1,2}, phase 3 {4,5}, phase 4 {7,8,9}
			return ProxyActionDrop, 0
		default: // #3, #6 (reset-on-success) and #10+ (post-reconnect health)
			return ProxyActionForward, 0
		}
	})
	proxy.Start(t)
	t.Cleanup(proxy.Stop)

	// Active endpoint: auto-linktest on, threshold 3, short T6 so each dropped round resolves fast.
	active := newEndpoint(t, proxy.Port(), true, []Option{
		WithConnectionOption(hsms.WithLinktestInterval(ltThresholdInterval)),
		WithConnectionOption(hsms.WithT6(ltThresholdT6)),
		WithConnectionOption(hsms.WithLinktestFailThreshold(3)),
	})
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, active)

	waitSelected(t, active)
	waitSelected(t, passive)
	// Clear the Select-handshake transitions so a later NotConnected event is unambiguously a fault.
	drainStateCh(active.states)

	m := controlMetrics(t, active)

	// Phase 0 (sanity): the auto-linktest is running — at least one Linktest.req reached the wire.
	require.Eventually(t, func() bool {
		return m.LinktestSendCount() >= 1
	}, 5*time.Second, 20*time.Millisecond, "auto-linktest must send at least one Linktest.req")

	// Phase 1: drops #1,#2 -> internal fails->2, cumulative errs->2. threshold=3 must NOT trip.
	require.Eventually(t, func() bool {
		return m.LinktestErrCount() >= 2
	}, 8*time.Second, 20*time.Millisecond, "LinktestErrCount must reach 2 after drops #1,#2")
	require.GreaterOrEqual(t, m.LinktestSendCount(), uint64(2), "at least two Linktest.req attempted")
	assertStaysSelected(t, active, ltStaysWindow) // 2 consecutive fails must not disconnect (threshold=3)

	// Phase 2: forward #3 -> internal consecutive counter RESETS to 0; a correlated rsp is counted.
	require.Eventually(t, func() bool {
		return m.LinktestRecvCount() >= 1
	}, 8*time.Second, 20*time.Millisecond, "LinktestRecvCount must reach 1 after forward #3")

	// Phase 3 (MONEY): drops #4,#5 -> cumulative errs->4 while STILL Selected. Because #3 reset the
	// internal counter, {4,5} is only a 2-run, so no disconnect — yet the CUMULATIVE LinktestErrCount
	// (4) already exceeds the threshold (3). That the link is still Selected here proves the disconnect
	// is driven by the internal reset-on-success counter, NOT by the never-resetting cumulative metric.
	require.Eventually(t, func() bool {
		return m.LinktestErrCount() >= 4
	}, 10*time.Second, 20*time.Millisecond, "LinktestErrCount must reach 4 after drops #4,#5")
	require.GreaterOrEqual(t, m.LinktestSendCount(), uint64(5), "at least five Linktest.req attempted")
	assertStaysSelected(t, active, ltStaysWindow) // 4 cumulative errs but no 3-run -> must stay Selected

	// Phase 4: forward #6 (reset again), then drops #7,#8,#9 -> 3 CONSECUTIVE fails -> threshold -> disconnect.
	require.Eventually(t, func() bool {
		return m.LinktestRecvCount() >= 2
	}, 10*time.Second, 20*time.Millisecond, "LinktestRecvCount must reach 2 after forward #6")
	// recvCount>=2 (rsp #6) is guaranteed to precede the #9 disconnect (~3 cycles later), so draining
	// now cannot swallow the disconnect event — it just clears any stale transitions.
	drainStateCh(active.states)
	waitNotConnectedEvent(t, active)

	// The cumulative error counter never resets: all 7 dropped rounds (#1,2,4,5,7,8,9) are counted.
	require.GreaterOrEqual(t, m.LinktestErrCount(), uint64(7),
		"cumulative LinktestErrCount must count all 7 dropped Linktest.rsp rounds (never resets)")
	require.GreaterOrEqual(t, m.LinktestSendCount(), uint64(9),
		"nine Linktest.req must have been attempted before the disconnect")
}

// assertStaysSelected reads the endpoint's state-change observer channel for up to d and fails if a
// NotConnected transition arrives during that window. It observes the state-change EVENT (not an
// instantaneous conn.State() poll) so a transient disconnect that v2's always-on auto-reconnect
// immediately recovers from is still caught — giving the reset-on-success regression real teeth. It is
// local to this file (ported from the v1 helper of the same name).
func assertStaysSelected(t *testing.T, ep *endpoint, d time.Duration) {
	t.Helper()

	timer := time.NewTimer(d)
	defer timer.Stop()

	for {
		select {
		case s := <-ep.states:
			if s == hsms.NotConnectedState {
				t.Fatalf("connection unexpectedly transitioned to NotConnected while it must stay Selected")
			}
		case <-timer.C:
			return // window elapsed with no disconnect — good
		}
	}
}
