package hsmsss

// connect_reconnect_test.go — Part A of the T29b-5 reconnect cluster (v2 port of the v1
// tests/hsmsss_integration/connect_reconnect_test.go), re-pointed onto the v2 public surface
// (New / Open(ctx, mode) / hsms.Connection / SendDataMessage(ctx, ...)). It exercises the
// I1-fixed reconnect/Close lifecycle on a race-free core: recover-from-abrupt-drop (active &
// passive), the T5-floored reconnect cadence, and the T6/T7 control-timeout teardowns.
//
// v2 ADAPTATIONS (documented per the shared prime directive):
//   - v1 host/equip roles are DROPPED (v2 has only the transport role); active/passive is kept.
//   - v2 auto-reconnect is ALWAYS ON. Every "→ NotConnected" is asserted via the TRANSITION event
//     (waitNotConnectedEvent) rather than an instantaneous conn.State() poll, because the reconnect
//     loop can recover a transient NotConnected before a poll observes it.
//   - v2's reconnect separation is a FLAT T5 floor (reconnectSleep waits exactly T5 each attempt),
//     NOT the v1 "exponential" growth; TestActiveReconnectCadence_FlatT5 asserts that flat T5 cadence
//     (the v1 test NAME is retained per the port map).
//   - In v2 an ACTIVE dial that fails on the FIRST attempt makes Open return the error immediately
//     (no reconnect loop spins on a never-available port — startActive dials synchronously). So the
//     backoff test establishes a live TCP generation first, then drops it, so the reconnect loop
//     re-dials a now-closed port on the T5 cadence. The dial-attempt cadence is observed white-box
//     via the "reconnect dial failed" debug log (v2's ConnRetryCount is a GAUGE held at 1 for the
//     lifetime of a single reconnect loop, so it does not climb per dial attempt).
//
// The file is package hsmsss (white-box) so it reuses the shared harness (newEndpoint /
// newEndpointPair / waitState / waitSelected / closeEndpoint / drainStateCh / echoHandler from
// integration_helpers_test.go, freeLoopbackPort / dialPassive / expectSelectRsp from passive_test.go,
// listenLoopback / acceptOneAsync / waitPeer / peerReadSelectReqHeader / selectReqFrame /
// selectRspFrame from active_test.go / transport_test.go, waitNotConnectedEvent from
// chaos_scenarios_test.go, and newChaosProxy from chaos_proxy_test.go). All readiness waits are
// event- or State()-driven (never time.Sleep-to-sync) and run under -race.

import (
	"sync"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/logger"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// dialFailLogger is a white-box logger wrapper that timestamps every "reconnect dial failed" debug
// log the engine emits, so a test can observe the reconnect dial cadence without a fixed Sleep. It
// embeds the default logger (which suppresses Debug at its Info sink, so there is no test spam) and
// overrides only Debug. With returns the same instance so a child logger still records (the engine
// logs this message on cfg.logger directly, so this is belt-and-suspenders).
type dialFailLogger struct {
	logger.Logger
	mu    *sync.Mutex
	times *[]time.Time
}

func (l *dialFailLogger) Debug(msg string, keysAndValues ...any) {
	if msg == "hsms: reconnect dial failed, retrying" {
		l.mu.Lock()
		*l.times = append(*l.times, time.Now())
		l.mu.Unlock()
	}

	l.Logger.Debug(msg, keysAndValues...)
}

func (l *dialFailLogger) With(_ ...any) logger.Logger { return l }

// ---------------------------------------------------------------------------
// 1. TestActiveRecoverFromAbruptDrop
//
// An active endpoint reaches Selected through a ChaosProxy against a real passive endpoint; the proxy
// then drops the live TCP connection (CloseConnections). v2 auto-reconnect re-dials through the proxy
// (which forwards again) and re-Selects, and a data round-trip works post-reconnect. TEETH: the
// NotConnected event only fires because the drop tears the link down; the subsequent Selected proves
// the reconnect loop rebuilt the generation, and the echoed round-trip proves the new generation is
// usable end-to-end.
// ---------------------------------------------------------------------------
func TestActiveRecoverFromAbruptDrop(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	portP := freeLoopbackPort(t)

	passive := newEndpoint(t, portP, false, nil, echoHandler)
	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, passive)

	proxy := newChaosProxy(t, portP)
	proxy.Start(t) // default filter forwards everything
	t.Cleanup(proxy.Stop)

	active := newEndpoint(t, proxy.Port(), true, nil)
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, active)

	waitSelected(t, active)
	waitSelected(t, passive)

	// Drain the observer channel so the NotConnected we match is the induced drop, not a stale event.
	drainStateCh(active.states)

	// Abruptly drop the live TCP connection.
	proxy.CloseConnections()

	// The active detects the drop → NotConnected → auto-reconnect through the proxy → re-Select.
	waitNotConnectedEvent(t, active)
	waitSelected(t, active)
	waitSelected(t, passive)

	// Post-reconnect the new generation must be usable end-to-end.
	reply, err := active.conn.SendDataMessage(ctx, 1, 1, true, secs2.A("after-drop"))
	require.NoError(t, err)
	require.NotNil(t, reply)

	item, err := reply.Item()
	require.NoError(t, err)
	got, err := item.ToASCII()
	require.NoError(t, err)
	require.Equal(t, "after-drop", got, "the echoed reply must survive the reconnect")
}

// ---------------------------------------------------------------------------
// 2. TestPassiveRecoverFromAbruptDrop
//
// A passive endpoint reaches Selected with a first raw client; the client abruptly drops; the passive
// returns to NotConnected and re-listens; a SECOND raw client dials the re-listened port and completes
// Select, and the passive returns to Selected. Proves passive-side reconnect (re-listen + re-select).
// ---------------------------------------------------------------------------
func TestPassiveRecoverFromAbruptDrop(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	port := freeLoopbackPort(t)

	passive := newEndpoint(t, port, false, nil)
	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, passive)

	// First generation: a raw client connects and selects.
	client1 := dialPassive(t, port)
	_, err := client1.Write(selectReqFrame([4]byte{0x11, 0x11, 0x11, 0x11}))
	require.NoError(t, err)
	expectSelectRsp(t, client1)
	waitSelected(t, passive)

	drainStateCh(passive.states)

	// Abrupt drop of the first client → the passive recv loop sees EOF → NotConnected → re-listen.
	require.NoError(t, client1.Close())
	waitNotConnectedEvent(t, passive)

	// Second generation: a fresh raw client dials the re-listened port and selects again. dialPassive
	// retries, so it tolerates the brief re-listen window.
	client2 := dialPassive(t, port)
	t.Cleanup(func() { _ = client2.Close() })
	_, err = client2.Write(selectReqFrame([4]byte{0x22, 0x22, 0x22, 0x22}))
	require.NoError(t, err)
	expectSelectRsp(t, client2)
	waitSelected(t, passive)
}

// ---------------------------------------------------------------------------
// 3. TestActiveReconnectCadence_FlatT5
//
// The reconnect dial cadence respects the T5 floor. v2 uses a FLAT T5 separation between reconnect
// dial attempts (not exponential growth — see the file header). Because an active dial that fails on
// the FIRST attempt makes Open return immediately, we first establish a live TCP generation (accept
// the initial dial), then drop it so the reconnect loop re-dials a now-closed port and its dial
// failures cadence at ~T5. The cadence is measured from the engine's "reconnect dial failed" debug
// logs; assertions are ordering + loose bounds only (tolerant of CI scheduling jitter).
// ---------------------------------------------------------------------------
func TestActiveReconnectCadence_FlatT5(t *testing.T) {
	t.Parallel()

	const t5 = 200 * time.Millisecond
	ctx := t.Context()

	ln, port := listenLoopback(t)

	var (
		mu    sync.Mutex
		fails []time.Time
	)
	capLog := &dialFailLogger{Logger: logger.Default(), mu: &mu, times: &fails}

	active := newEndpoint(t, port, true, []Option{
		WithConnectionOption(hsms.WithT5(t5)),
		WithConnectionOption(hsms.WithT6(500 * time.Millisecond)),
		WithConnectionOption(hsms.WithLogger(capLog)),
	})
	t.Cleanup(func() { closeEndpoint(t, active) })

	// Accept the FIRST dial so Open succeeds and a TCP generation goes live (v2 startActive dials
	// synchronously — an initial dial failure would abort Open instead of starting the loop).
	peerCh := acceptOneAsync(ln)
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	peer := waitPeer(t, peerCh)

	// Drop the link and remove the listener so every reconnect dial is refused → the loop re-dials on
	// the T5 cadence, logging a dial failure each attempt.
	require.NoError(t, ln.Close())
	require.NoError(t, peer.Close())

	// Collect enough dial-failure timestamps to measure the cadence.
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()

		return len(fails) >= 5
	}, 15*time.Second, 10*time.Millisecond, "expected repeated reconnect dial failures on the T5 cadence")

	// The reconnect loop must be live while it retries (v2 ConnRetryCount is a gauge held at 1 for the
	// loop's lifetime — it does not climb per attempt).
	require.Positive(t, active.conn.Metrics().ConnRetryCount(), "reconnect loop must be live while retrying")

	mu.Lock()
	times := append([]time.Time(nil), fails...)
	mu.Unlock()

	// Cadence: no runaway growth (each gap far below a generous multiple of T5) and at least one gap
	// reflects the T5 floor (proves it is not a hot-spin). Loose bounds only.
	sawFloor := false
	for i := 1; i < len(times); i++ {
		delta := times[i].Sub(times[i-1])
		t.Logf("reconnect dial-fail gap %d→%d: %v", i-1, i, delta)
		require.Less(t, delta, 10*t5, "reconnect cadence must stay near the T5 floor (no runaway backoff)")

		if delta >= t5/2 {
			sawFloor = true
		}
	}
	require.True(t, sawFloor, "at least one reconnect gap must reflect the T5 floor (no busy-spin)")
}

// ---------------------------------------------------------------------------
// 4. TestT7Timeout_PassiveWaitSelect
//
// A raw client connects to a passive endpoint (TCP up → NotSelected) but never sends Select.req.
// After the T7 NOT-SELECTED dwell the passive tears the connection down → NotConnected. Assert the
// NotConnected event arrives ~T7 after entering NotSelected (short T7; loose timing bounds).
// ---------------------------------------------------------------------------
func TestT7Timeout_PassiveWaitSelect(t *testing.T) {
	t.Parallel()

	const t7 = 1 * time.Second
	ctx := t.Context()
	port := freeLoopbackPort(t)

	passive := newEndpoint(t, port, false, []Option{
		WithConnectionOption(hsms.WithT7(t7)),
	})
	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, passive)

	// Raw client connects (TCP up) but never sends Select.req.
	client := dialPassive(t, port)
	t.Cleanup(func() { _ = client.Close() })

	// Passive accepts → NotSelected (T7 dwell armed). Record the dwell start.
	waitState(t, passive, hsms.NotSelectedState)
	drainStateCh(passive.states)
	start := time.Now()

	// No Select.req → T7 fires → the passive drops the unselected peer → NotConnected.
	waitNotConnectedEvent(t, passive)
	elapsed := time.Since(start)

	require.GreaterOrEqual(t, elapsed, t7/2, "passive must wait ~T7 before dropping an unselected peer")
	require.Less(t, elapsed, 4*t7, "passive must drop shortly after T7 (loose upper bound for CI jitter)")
	t.Logf("passive enforced T7 in %v", elapsed)
}

// ---------------------------------------------------------------------------
// 5. TestT6Timeout_ActiveControlReply
//
// An active endpoint dials a raw peer that accepts TCP and reads the active's Select.req but NEVER
// replies. After T6 (the control-reply timeout) the active tears the link down → NotConnected, then
// auto-reconnects (re-dials the still-open listener and re-sends Select.req). Distinct from the
// dropped-Select-RSP-via-proxy scenario (T29b-2): here the raw peer simply never answers. Short T6;
// loose timing bounds.
// ---------------------------------------------------------------------------
func TestT6Timeout_ActiveControlReply(t *testing.T) {
	t.Parallel()

	const t6 = 1 * time.Second
	ctx := t.Context()

	ln, port := listenLoopback(t)
	t.Cleanup(func() { _ = ln.Close() })

	active := newEndpoint(t, port, true, []Option{
		WithConnectionOption(hsms.WithT6(t6)),
		WithConnectionOption(hsms.WithT5(200 * time.Millisecond)),
	})
	t.Cleanup(func() { closeEndpoint(t, active) })

	peerCh := acceptOneAsync(ln)
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	peer := waitPeer(t, peerCh)
	t.Cleanup(func() { _ = peer.Close() })

	// Read the active's Select.req but NEVER reply → T6 governs the control-reply timeout.
	_ = peerReadSelectReqHeader(t, peer)
	drainStateCh(active.states)
	start := time.Now()

	waitNotConnectedEvent(t, active)
	elapsed := time.Since(start)

	require.GreaterOrEqual(t, elapsed, t6/2, "active must wait ~T6 before tearing down an unanswered Select.req")
	require.Less(t, elapsed, 4*t6, "active must tear down shortly after T6 (loose upper bound for CI jitter)")
	t.Logf("active enforced T6 in %v", elapsed)

	// The active must then reconnect: it re-dials the still-open listener and re-sends Select.req.
	peerCh2 := acceptOneAsync(ln)
	peer2 := waitPeer(t, peerCh2)
	t.Cleanup(func() { _ = peer2.Close() })
	_ = peerReadSelectReqHeader(t, peer2) // proves the reconnect re-dialed and re-issued Select.req
}

// ---------------------------------------------------------------------------
// 6. TestConcurrentClose
//
// N goroutines Close the SAME established connection simultaneously. This is the idempotent-re-Close
// path (lifeMu serialization + the runDone short-circuit) under a concurrent burst — genuinely
// distinct from the I1 teeth-test (TestHSMS_CloseDuringReconnect_NoTransportRace), which races Close
// against an in-flight reconnect PUBLISH, and from TestHSMS_CloseRace_BoundedCleanShutdown, which
// loops sequential Open/Close. Every Close must return consistently (nil), none may panic, and all
// must return within a generous bound (no deadlock). Runs under -race.
// ---------------------------------------------------------------------------
func TestConcurrentClose(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	passive, active := newEndpointPair(t)
	t.Cleanup(func() {
		closeEndpoint(t, active)
		closeEndpoint(t, passive)
	})

	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	waitSelected(t, active)
	waitSelected(t, passive)

	const closers = 10

	var wg sync.WaitGroup
	errs := make([]error, closers)

	for i := range closers {
		wg.Go(func() {
			errs[i] = active.conn.Close()
		})
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent Close did not return within bound (possible deadlock)")
	}

	for i, err := range errs {
		require.NoErrorf(t, err, "concurrent Close %d must return nil (idempotent, consistent)", i)
	}

	assertCleanShutdown(t, active.conn)
}
