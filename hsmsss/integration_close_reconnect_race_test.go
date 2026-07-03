package hsmsss

// close_reconnect_race_test.go — the load-bearing I1 teeth test (see
// .superpowers/sdd/task-i1fix-brief.md). It drives the "concurrent Close-during-reconnect + reopen
// on the shared transport" scenario the brief permits, using the ChaosProxy to make the reconnect
// fire RELIABLY. (A symmetric concurrent close of two real peers reproduces the same race but only
// ~1/12000 — each peer usually sets its own shutdown before it detects the peer's drop, so the
// reconnect loop, the orphan source, seldom even starts. Forcing the drop with the proxy makes the
// reconnect deterministic, so the race reproduces on essentially every run pre-fix.)
//
// THE RACE (pre-fix): the proxy CUTS the active's Select handshake, so every connection drops while
// NotSelected and — with shutdown still false — the active's reconnect loop perpetually publishes +
// tr.Start's successor generations on the shared, REUSED *transport (ConnRetryCount() stays > 0). A
// voluntary Close fired while that reconnect is in flight is not atomic w.r.t. the loop's
// fence+publish, so it pins the OLD generation and leaves a successor ORPHANED. The reopen on the
// next iteration then reuses the shared *transport while the orphaned generation's recv loop /
// Select procedure are still live: Start's writes (t.rt / g.recv.Add / g.proc.Go) race the
// straggler's reads (readFrame / runSelectProcedure) and Stop's WaitGroup Waits — a data race on
// the shared, reused transport state. Observed pre-fix: a DATA RACE on essentially every run under
// -race (Start vs readFrame/runSelectProcedure/g.recv/g.proc).
//
// THE FIX (I1): connection.publishMu linearizes the loop's fence+publish against Close's
// fence+successor-re-pin (Close pins any just-published successor → no orphan → Close tears it down
// and connectLoopWg.Wait joins the loop before returning), and the transport's startGate/stopping
// Add-vs-Wait guard (cleared per generation by ArmStart, ordered before any Close could seal that
// generation) keeps tr.Start's Adds and tr.Stop's Waits from ever being concurrent on the shared
// WaitGroups. So after Close returns no straggler generation is live, and the reopen races nothing.
//
// TEETH: revert either half of the fix and this test reproduces the data race under -race within a
// handful of iterations; with the fix it is green at -race -count=2000.

import (
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/stretchr/testify/require"
)

func TestHSMS_CloseDuringReconnect_NoTransportRace(t *testing.T) {
	// Each iteration: Open → wait for the reconnect loop to be live → Close racing it → reopen
	// IMMEDIATELY on the same transport. 30 inner iterations keep the shared transport reused across
	// many generations (the reuse the race needs) while staying fast enough for -race -count.
	const iterations = 30
	const closeBound = 5 * time.Second

	ctx := t.Context()
	portP := freeLoopbackPort(t)

	// Short-but-safe timers: a 1 ms T5 makes the involuntary-drop reconnect loop blast past its
	// backoff and reach the publish fence while the voluntary Close is in flight — the I1 window.
	timerOpts := []Option{
		WithConnectionOption(hsms.WithT3(2 * time.Second)),
		WithConnectionOption(hsms.WithT5(1 * time.Millisecond)),
		WithConnectionOption(hsms.WithT6(2 * time.Second)),
		WithConnectionOption(hsms.WithT7(2 * time.Second)),
		WithConnectionOption(hsms.WithT8(1 * time.Second)),
		WithConnectionOption(hsms.WithLinktestInterval(0)),
	}

	// Passive listens on portP and stays open across all iterations. The active dials the PROXY,
	// which relays to the passive but CUTS every client→target frame (the Select.req): the TCP dial
	// always succeeds (so tr.Start registers this generation's goroutines on the shared WaitGroups —
	// the Add side of the race), then the link drops before Select completes, driving a perpetual
	// involuntary-drop reconnect storm on the active while shutdown is still false.
	passiveCfg, err := NewConfig("127.0.0.1", portP, append([]Option{WithPassive()}, timerOpts...)...)
	require.NoError(t, err)
	passive, err := New(passiveCfg)
	require.NoError(t, err)
	require.NoError(t, passive.Open(ctx, hsms.OpenBackground))
	t.Cleanup(func() { _ = passive.Close() })

	proxy := newChaosProxy(t, portP)
	proxy.SetFilter(func(isClientToTarget bool, _ []byte, _ []byte) (ProxyAction, time.Duration) {
		if isClientToTarget {
			return ProxyActionCloseTCP, 0
		}

		return ProxyActionForward, 0
	})
	proxy.Start(t)
	t.Cleanup(proxy.Stop)

	// One active connection (and thus ONE *transport) REUSED across every iteration — the shared,
	// reused transport state is exactly what the pre-fix race trips.
	activeCfg, err := NewConfig("127.0.0.1", proxy.Port(), append([]Option{WithActive()}, timerOpts...)...)
	require.NoError(t, err)
	active, err := New(activeCfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = active.Close() })

	// waitReconnecting blocks until the active's reconnect loop is live (ConnRetryCount gauge > 0),
	// so the subsequent Close is guaranteed to race an in-flight reconnect (publish + tr.Start) —
	// the I1 window — rather than beating the involuntary-drop reaction to the shutdown fence.
	waitReconnecting := func(iter int) {
		require.Eventuallyf(t, func() bool {
			return active.Metrics().ConnRetryCount() > 0
		}, 10*time.Second, 200*time.Microsecond, "iter %d: active reconnect loop never became live", iter)
	}

	for i := range iterations {
		require.NoErrorf(t, active.Open(ctx, hsms.OpenBackground), "iter %d: active open", i)

		// The proxy cuts every Select attempt, so the active never selects and instead storms
		// through involuntary-drop reconnect generations on the shared transport. Wait until a
		// reconnect loop is actually in flight before closing.
		waitReconnecting(i)

		// Race the in-flight reconnect with a voluntary Close, then reopen IMMEDIATELY (no clean
		// gate): pre-fix, an orphaned successor generation's goroutines are still live on the shared
		// transport when the next iteration's Open calls tr.Start → a data race under -race. The
		// Close must stay clean (nil) and bounded regardless.
		start := time.Now()
		err := active.Close()
		elapsed := time.Since(start)
		require.NoErrorf(t, err, "iter %d: active Close returned error", i)
		require.Lessf(t, elapsed, closeBound, "iter %d: active Close not bounded (%s)", i, elapsed)
	}

	// After the final Close (post-fix: every generation was torn down and the reconnect loop joined),
	// the active must be quiesced — State NotConnected, no in-flight messages, all transport
	// goroutines gone.
	assertCleanShutdown(t, active)
}
