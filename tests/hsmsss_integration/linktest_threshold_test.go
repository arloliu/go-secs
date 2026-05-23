package hsmsssintegration

// TestHSMS_LinktestFailThreshold_ResetsOnSuccess exercises the auto-linktest
// failure-threshold mechanism with threshold=3 and a mid-sequence successful
// reply to prove the consecutive-failure counter resets.
//
// Drop pattern (counting Linktest.rsp frames #1, #2, ...):
//   #1 – forward (sanity: connection stays up)
//   #2 – drop    → failCount reaches 1; NotConnected must NOT fire
//   #3 – forward → failCount resets to 0 (counter reset on success)
//   #4 – drop    → failCount reaches 1 again; NotConnected must NOT fire
//   #5 – forward → failCount resets to 0 again
//   #6 – drop    → failCount = 1
//   #7 – drop    → failCount = 2
//   #8 – drop    → failCount = 3 ≥ threshold → NotConnected fires
//
// The test asserts:
//   (a) After drop #2 the connection is still Selected and errCount == 1.
//   (b) After forward #3 the connection is still Selected.
//   (c) After drop #4 the connection is still Selected and errCount == 2.
//   (d) After forward #5 the connection is still Selected.
//   (e) After drops #6 and #7 the connection is still Selected and errCount == 4.
//   (f) After drop #8 the connection transitions to NotConnected.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/go-secs/hsms"
	"github.com/arloliu/go-secs/hsmsss"
	"github.com/stretchr/testify/require"
)

// linktestInterval / T6 chosen so the full sequence completes in <30 s per
// iteration and still leaves enough headroom for race-detector overhead.
// T6 minimum is 1 s (API constraint); interval is 150 ms so each cycle
// completes quickly (T6 dominates for dropped frames).
const (
	ltThresholdInterval = 150 * time.Millisecond
	ltThresholdT6       = 1 * time.Second
	// slack added on top of T6 when waiting for a negative assertion
	ltThresholdSlack = 100 * time.Millisecond
)

func TestHSMS_LinktestFailThreshold_ResetsOnSuccess(t *testing.T) {
	require := require.New(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// --- equipment endpoint (passive, auto-linktest off) ---
	equipPort := getFreePort(t)
	equip := newEndpoint(t, ctx, equipPort, true, false, nil)
	defer closeEndpoint(t, equip)
	require.NoError(equip.conn.Open(false))

	// --- chaos proxy sitting between host and equipment ---
	proxy := newChaosProxy(t, equipPort)

	// rspCount tracks how many Linktest.rsp frames have been *seen* by the
	// filter (regardless of whether they were forwarded or dropped).
	var rspCount atomic.Int32

	proxy.SetFilter(func(isClientToTarget bool, header []byte, _ []byte) (ProxyAction, time.Duration) {
		// We only care about frames flowing from equipment → host (target→client).
		if isClientToTarget {
			return ProxyActionForward, 0
		}
		if len(header) < 6 || header[5] != byte(hsms.LinkTestRspType) {
			return ProxyActionForward, 0
		}

		n := int(rspCount.Add(1))
		// Drop frames #2, #4, #6, #7, #8
		switch n {
		case 2, 4, 6, 7, 8:
			return ProxyActionDrop, 0
		default:
			return ProxyActionForward, 0
		}
	})

	proxy.Start(t)
	defer proxy.Stop()

	// --- host endpoint (active, threshold=3) ---
	hostOpts := []hsmsss.ConnOption{
		hsmsss.WithAutoLinktest(true),
		hsmsss.WithLinktestInterval(ltThresholdInterval),
		hsmsss.WithT6Timeout(ltThresholdT6),
		hsmsss.WithLinktestFailThreshold(3),
	}
	host := newEndpoint(t, ctx, proxy.Port(), false, true, hostOpts)
	defer closeEndpoint(t, host)
	require.NoError(host.conn.Open(false))

	// Wait until both sides are Selected.
	requireStateEvent(t, host.selectedCh, hsms.SelectedState)
	requireStateEvent(t, equip.selectedCh, hsms.SelectedState)

	// -------------------------------------------------------------------
	// Phase 0: Sanity – at least one successful Linktest.rsp (#1) has gone
	// through; errCount is still 0.
	// -------------------------------------------------------------------
	require.Eventually(func() bool {
		return host.conn.GetMetrics().LinktestSendCount.Load() >= 1
	}, 5*time.Second, 20*time.Millisecond, "expected at least 1 linktest sent")

	// -------------------------------------------------------------------
	// Phase 1: Drop #2 – errCount reaches 1, connection remains Selected.
	// -------------------------------------------------------------------
	require.Eventually(func() bool {
		return host.conn.GetMetrics().LinktestErrCount.Load() >= 1
	}, 5*time.Second, 20*time.Millisecond, "expected errCount >= 1 after drop #2")

	// Give a full T6 window of quiet time: if threshold=1 had fired the
	// connection would already be NotConnected.
	assertStaysSelected(t, host, ltThresholdT6+ltThresholdSlack,
		"connection must NOT disconnect after a single dropped Linktest.rsp (threshold=3)")

	// -------------------------------------------------------------------
	// Phase 2: Forward #3 – counter resets; errCount stays at 1.
	// Wait for frame #3 to have been processed (sendCount ≥ 3).
	// -------------------------------------------------------------------
	require.Eventually(func() bool {
		return host.conn.GetMetrics().LinktestSendCount.Load() >= 3
	}, 5*time.Second, 20*time.Millisecond, "expected at least 3 linktests sent (frame #3 processed)")

	assertStaysSelected(t, host, ltThresholdSlack,
		"connection must remain Selected after successful Linktest.rsp #3 (reset)")

	// -------------------------------------------------------------------
	// Phase 3: Drop #4 – errCount reaches 2, connection remains Selected.
	// -------------------------------------------------------------------
	require.Eventually(func() bool {
		return host.conn.GetMetrics().LinktestErrCount.Load() >= 2
	}, 5*time.Second, 20*time.Millisecond, "expected errCount >= 2 after drop #4")

	assertStaysSelected(t, host, ltThresholdT6+ltThresholdSlack,
		"connection must NOT disconnect after isolated drop #4 (threshold=3, counter reset before it)")

	// -------------------------------------------------------------------
	// Phase 4: Forward #5 – counter resets again.
	// -------------------------------------------------------------------
	require.Eventually(func() bool {
		return host.conn.GetMetrics().LinktestSendCount.Load() >= 5
	}, 5*time.Second, 20*time.Millisecond, "expected at least 5 linktests sent")

	assertStaysSelected(t, host, ltThresholdSlack,
		"connection must remain Selected after successful Linktest.rsp #5 (second reset)")

	// -------------------------------------------------------------------
	// Phase 5: Drops #6 and #7 – errCount reaches 4, still Selected.
	//
	// We cannot use assertStaysSelected here with a full T6 window because
	// the 3rd consecutive drop (#8) fires ~interval+T6 after drop #7.
	// Instead: as soon as errCount hits 4, assert the current state is
	// still Selected (a point-in-time check, not a timed window).
	// -------------------------------------------------------------------
	require.Eventually(func() bool {
		return host.conn.GetMetrics().LinktestErrCount.Load() >= 4
	}, 8*time.Second, 20*time.Millisecond, "expected errCount >= 4 after drops #6 and #7")

	// At the instant errCount hits 4 (after 2 consecutive failures), the
	// connection must still be in Selected state — not yet disconnected.
	require.True(host.conn.GetMetrics().LinktestErrCount.Load() >= 4,
		"sanity: errCount must be >= 4 at this point")
	// Drain any non-NotConnected state events that may have been queued
	// (e.g. SelectedState confirmations), then verify no disconnect arrived.
	assertNoDisconnectYet(t, host,
		"connection must NOT be NotConnected after only 2 consecutive drops (threshold=3)")

	// -------------------------------------------------------------------
	// Phase 6: Drop #8 – 3rd consecutive failure → NotConnected.
	// -------------------------------------------------------------------
	requireStateEventTimeout(t, host.selectedCh, hsms.NotConnectedState,
		10*time.Second,
		"connection must transition to NotConnected after 3rd consecutive dropped Linktest.rsp")

	// Sanity: at least 5 linktest errors total (drops 2,4,6,7,8).
	finalErrCount := host.conn.GetMetrics().LinktestErrCount.Load()
	require.GreaterOrEqual(finalErrCount, uint64(5),
		"expected at least 5 total linktest errors (drops 2, 4, 6, 7, 8)")
}

// assertStaysSelected reads from the host's state channel for up to d and
// fails if any NotConnectedState event arrives during that window.
func assertStaysSelected(t *testing.T, ep *endpoint, d time.Duration, msg string) {
	t.Helper()

	timer := time.NewTimer(d)
	defer timer.Stop()

	for {
		select {
		case s := <-ep.selectedCh:
			if s == hsms.NotConnectedState {
				t.Fatalf("%s: got unexpected NotConnected transition", msg)
			}
		case <-timer.C:
			return // no disconnect observed — good
		}
	}
}

// assertNoDisconnectYet drains any already-queued state events from ep's
// state channel and fails if NotConnectedState is among them.  It does not
// wait for future events — call requireStateEventTimeout afterward to assert
// an expected disconnect.
func assertNoDisconnectYet(t *testing.T, ep *endpoint, msg string) {
	t.Helper()

	for {
		select {
		case s := <-ep.selectedCh:
			if s == hsms.NotConnectedState {
				t.Fatalf("%s", msg)
			}
		default:
			return // channel drained, no disconnect seen
		}
	}
}

// requireStateEventTimeout is like requireStateEvent but with a caller-supplied
// timeout and a descriptive failure message.
func requireStateEventTimeout(t *testing.T, ch <-chan hsms.ConnState, expected hsms.ConnState, d time.Duration, msg string) {
	t.Helper()

	timer := time.NewTimer(d)
	defer timer.Stop()

	for {
		select {
		case s := <-ch:
			if s == expected {
				return
			}
		case <-timer.C:
			t.Fatalf("%s: timed out waiting for state %s", msg, expected.String())
		}
	}
}
