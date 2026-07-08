package hsmsss

// lifecycle_test.go — Part B (lifecycle) of the T29b-5 cluster (v2 port of the v1
// tests/hsmsss_integration/lifecycle_test.go), re-pointed onto the v2 public surface. It exercises
// the I1-fixed Open/Close/reopen lifecycle: clean reopen on the same connections, a graceful
// responder-only Deselect, abrupt peer-drop detection, active-initiated teardown propagating to the
// passive, and the full ordered state-transition sequence across repeated open/close cycles.
//
// v2 ADAPTATIONS (documented per the shared prime directive):
//   - v1 host/equip roles dropped; active/passive kept. State handler is func(prev, next ConnState).
//   - GRACEFUL DESELECT: v1 asserted "deselect ⇒ NotConnected". v2's responder-only Deselect
//     (handleDeselectReq / §7.7) replies Deselect.rsp(success) and transitions Selected → NotSelected
//     on the SAME TCP connection (re-arming T7), NOT straight to NotConnected. The brief allows
//     NotConnected/NotSelected; the load-bearing assertion is that the endpoint LEAVES Selected, which
//     here means it reaches NotSelected. Documented in TestLifecycle_GracefulDeselect.
//   - v2 auto-reconnect is ALWAYS ON, so a passive whose peer drops re-listens (staying NotConnected);
//     assertCleanShutdown observes State==NotConnected + zero in-flight, which holds while re-listening.
//
// The file is package hsmsss (white-box) so it reuses the shared harness (newEndpoint / waitState /
// waitSelected / closeEndpoint / drainStateCh / echoHandler from integration_helpers_test.go,
// freeLoopbackPort / dialPassive from passive_test.go, listenLoopback / acceptOneAsync / waitPeer /
// peerReadFrame / peerReadSelectReqHeader / selectRspFrame from active_test.go / transport_test.go,
// buildControlFrame from raw_control_frames_test.go, waitNotConnectedEvent from
// chaos_scenarios_test.go, assertCleanShutdown from assert_clean_shutdown_test.go). All readiness
// waits are event- or State()-driven (never time.Sleep-to-sync) and run under -race.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// 6. TestLifecycle_OpenCloseReopen
//
// A real active+passive pair: Open → S1F1/S1F2 → Close → re-Open → S1F1/S1F2 (a distinct second
// payload, echoed correctly). Proves a clean reopen on the SAME Connection objects (v2 Close fully
// tears the generation down and is bounded, so the reopen races nothing — no settle Sleep needed).
// ---------------------------------------------------------------------------
func TestLifecycle_OpenCloseReopen(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	port := freeLoopbackPort(t)

	passive := newEndpoint(t, port, false, nil, echoHandler)
	active := newEndpoint(t, port, true, nil)
	defer closeEndpoint(t, active)
	defer closeEndpoint(t, passive)

	// --- first open ---
	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	waitSelected(t, active)
	waitSelected(t, passive)

	reply, err := active.conn.SendDataMessage(ctx, 1, 1, true, secs2.A("first"))
	require.NoError(t, err)
	require.NotNil(t, reply)

	// --- close both ---
	require.NoError(t, active.conn.Close())
	require.NoError(t, passive.conn.Close())

	// --- reopen (passive must listen before the active dials) ---
	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	waitSelected(t, active)
	waitSelected(t, passive)

	reply2, err := active.conn.SendDataMessage(ctx, 1, 1, true, secs2.A("second"))
	require.NoError(t, err)
	require.NotNil(t, reply2)

	item, err := reply2.Item()
	require.NoError(t, err)
	got, err := item.ToASCII()
	require.NoError(t, err)
	require.Equal(t, "second", got, "the reopened generation must echo the second payload")
}

// ---------------------------------------------------------------------------
// 7. TestLifecycle_GracefulDeselect
//
// A raw client Selects a passive endpoint, then sends a Deselect.req. The v2 responder replies
// Deselect.rsp(success) — SType 4, status 0 — with matching System Bytes, and transitions
// Selected → NotSelected on the same TCP connection (the responder-only Deselect path, §7.7; it does
// NOT go straight to NotConnected). Assert the raw client reads the Deselect.rsp and the endpoint
// leaves Selected. T7 is set long so the post-deselect NotSelected dwell is comfortably observable.
// ---------------------------------------------------------------------------
func TestLifecycle_GracefulDeselect(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	port := freeLoopbackPort(t)

	passive := newEndpoint(t, port, false, []Option{
		WithConnectionOption(hsms.WithT7(30 * time.Second)),
	})
	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, passive)

	// Raw client connects and selects.
	rawConn := dialPassive(t, port)
	t.Cleanup(func() { _ = rawConn.Close() })

	_, err := rawConn.Write(selectReqFrame([4]byte{0x01, 0x02, 0x03, 0x04}))
	require.NoError(t, err)
	expectSelectRsp(t, rawConn)
	waitSelected(t, passive)

	// Send Deselect.req (SType 3) with distinctive System Bytes.
	sb := [4]byte{0x10, 0x20, 0x30, 0x40}
	_, err = rawConn.Write(buildControlFrame(byte(hsms.DeselectReqType), 0, sb))
	require.NoError(t, err)

	// The responder must answer Deselect.rsp (SType 4), success status (0), echoing the System Bytes.
	rsp, err := peerReadFrame(rawConn, 5*time.Second)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(rsp), 10, "Deselect.rsp must carry a 10-byte header")
	require.Equal(t, byte(hsms.DeselectRspType), rsp[5], "expected a Deselect.rsp")
	require.Equal(t, byte(hsms.DeselectStatusSuccess), rsp[3], "expected Deselect.rsp success status")
	require.Equal(t, sb[:], rsp[6:10], "Deselect.rsp must echo the request System Bytes")

	// The endpoint must leave Selected. In v2 that is a transition to NotSelected (same TCP
	// connection; T7 re-arms) rather than v1's straight-to-NotConnected.
	waitState(t, passive, hsms.NotSelectedState)
}

// ---------------------------------------------------------------------------
// 8. TestLifecycle_AbruptPeerDropDetection
//
// An active endpoint is Selected against a raw peer; the raw peer abruptly closes its TCP socket. The
// active's recv loop sees EOF → TCPDown → NotConnected. Assert via the transition event. (The listener
// is closed first so post-drop reconnect dials are refused, keeping churn minimal.)
// ---------------------------------------------------------------------------
func TestLifecycle_AbruptPeerDropDetection(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	ln, port := listenLoopback(t)
	t.Cleanup(func() { _ = ln.Close() })

	active := newEndpoint(t, port, true, nil)
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, active)

	peerCh := acceptOneAsync(ln)
	peer := waitPeer(t, peerCh)

	reqHdr := peerReadSelectReqHeader(t, peer)
	_, err := peer.Write(selectRspFrame(reqHdr, hsms.SelectStatusSuccess))
	require.NoError(t, err)
	waitSelected(t, active)

	// Remove the listener (post-drop reconnect dials are then refused), drain, then drop the peer.
	require.NoError(t, ln.Close())
	drainStateCh(active.states)
	require.NoError(t, peer.Close())

	// The active must detect the abrupt drop and transition to NotConnected.
	waitNotConnectedEvent(t, active)
}

// ---------------------------------------------------------------------------
// 9. TestLifecycle_DeselectFromPassiveSide
//
// A real active+passive pair, both Selected. active.Close() tears the active down and drops the
// passive; BOTH must reach NotConnected and pass the clean-shutdown gate. (The passive re-listens
// after the drop — still NotConnected — so assertCleanShutdown, which requires State==NotConnected +
// zero in-flight, holds.)
// ---------------------------------------------------------------------------
func TestLifecycle_DeselectFromPassiveSide(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	port := freeLoopbackPort(t)

	passive := newEndpoint(t, port, false, nil, echoHandler)
	active := newEndpoint(t, port, true, nil)
	defer closeEndpoint(t, passive) // the active is closed explicitly below
	defer closeEndpoint(t, active)

	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	waitSelected(t, active)
	waitSelected(t, passive)

	// Confirm the link works before tearing it down.
	reply, err := active.conn.SendDataMessage(ctx, 1, 1, true, secs2.A("check"))
	require.NoError(t, err)
	require.NotNil(t, reply)

	// Close the active side; both endpoints must transition to NotConnected.
	require.NoError(t, active.conn.Close())

	waitState(t, active, hsms.NotConnectedState)
	waitState(t, passive, hsms.NotConnectedState)

	assertCleanShutdown(t, active.conn)
	assertCleanShutdown(t, passive.conn)
}

// ---------------------------------------------------------------------------
// 10. TestLifecycle_UserHandlerReceivesAllTransitions
//
// Over 5 Open→use→Close cycles a state-change handler records every (prev,next) transition. Each cycle
// must contain the ordered anchor transitions — *→NotSelected, NotSelected→Selected, Selected→
// NotConnected — with no gaps, for both the active and the passive side. This complements T29a's
// per-transition idempotency property (no prev==next): here the FULL ordered sequence is asserted.
// Handlers persist across Open/Close cycles (interface contract), so one registration covers all cycles.
// ---------------------------------------------------------------------------
func TestLifecycle_UserHandlerReceivesAllTransitions(t *testing.T) {
	t.Parallel()

	const cycles = 5
	ctx := t.Context()
	port := freeLoopbackPort(t)

	type transition struct{ prev, next hsms.ConnState }

	type observer struct {
		mu     sync.Mutex
		events []transition
	}

	newObs := func() *observer { return &observer{events: make([]transition, 0, cycles*4)} }
	activeObs := newObs()
	passiveObs := newObs()

	record := func(o *observer) func(prev, next hsms.ConnState) {
		return func(prev, next hsms.ConnState) {
			o.mu.Lock()
			o.events = append(o.events, transition{prev, next})
			o.mu.Unlock()
		}
	}

	countEntering := func(o *observer, state hsms.ConnState) int {
		o.mu.Lock()
		defer o.mu.Unlock()
		n := 0
		for _, e := range o.events {
			if e.next == state {
				n++
			}
		}

		return n
	}

	passive := newEndpoint(t, port, false, nil, echoHandler)
	active := newEndpoint(t, port, true, nil)
	passive.conn.AddConnStateChangeHandler(record(passiveObs))
	active.conn.AddConnStateChangeHandler(record(activeObs))

	t.Cleanup(func() {
		closeEndpoint(t, active)
		closeEndpoint(t, passive)
	})

	for i := range cycles {
		require.NoErrorf(t, passive.conn.Open(ctx, hsms.OpenBackground), "cycle %d: passive open", i)
		require.NoErrorf(t, active.conn.Open(ctx, hsms.OpenBackground), "cycle %d: active open", i)
		waitSelected(t, active)
		waitSelected(t, passive)

		require.NoErrorf(t, active.conn.Close(), "cycle %d: active close", i)
		require.NoErrorf(t, passive.conn.Close(), "cycle %d: passive close", i)

		// Wait for THIS cycle's terminal transition (entering NotConnected) to reach BOTH observers
		// before proceeding. Bounding delivery per-cycle prevents notifier lag under heavy concurrent
		// load from accumulating into a flaky global check after the loop.
		//
		// We count entering-NotConnected terminals, NOT a fixed 3-per-cycle total, because the
		// intermediate NotSelected is a BEST-EFFORT notification the engine may coalesce: when a fast
		// Select pre-commits Selected (CommitSelected) before the supervisor reacts to TCP-up, the
		// entering-NotSelected reaction is skipped and the observer sees NotConnected->Selected directly
		// (the chain stays continuous; only the intermediate state is dropped — the documented
		// drop-OLDEST/coalescing contract). The terminal, by contrast, is reliably delivered (emitted
		// before teardown and drained when the notifier is joined on Close), so it is the sound anchor
		// to wait on.
		wantTerminals := i + 1
		require.Eventuallyf(t, func() bool {
			return countEntering(activeObs, hsms.NotConnectedState) >= wantTerminals &&
				countEntering(passiveObs, hsms.NotConnectedState) >= wantTerminals
		}, 5*time.Second, 2*time.Millisecond,
			"cycle %d: terminal NotConnected not observed on both sides (want >= %d each)", i, wantTerminals)

		// Drain the best-effort observer channels so the next cycle's waits start from empty.
		drainStateCh(active.states)
		drainStateCh(passive.states)
	}

	assertCycleTransitions := func(name string, o *observer) {
		// Each cycle reliably delivers at least entering-Selected and its terminal entering-NotConnected
		// (2 transitions); the intermediate entering-NotSelected is best-effort and may coalesce, so the
		// guaranteed floor is cycles*2, not cycles*3.
		require.Eventuallyf(t, func() bool {
			o.mu.Lock()
			defer o.mu.Unlock()

			return len(o.events) >= cycles*2
		}, 5*time.Second, 5*time.Millisecond, "%s: expected at least %d transitions", name, cycles*2)

		o.mu.Lock()
		defer o.mu.Unlock()
		ev := o.events

		// Continuity + idempotency: even when an intermediate state coalesces, the observer must see an
		// UNBROKEN, non-self-looping state chain (emit reports prev = the last state actually delivered,
		// so ev[k].prev must equal ev[k-1].next). This catches genuinely dropped/duplicated/reordered
		// notifications, which coalescing does not produce.
		for k := range ev {
			require.NotEqualf(t, ev[k].prev, ev[k].next,
				"%s: self-loop transition at index %d (events: %+v)", name, k, ev)
			if k > 0 {
				require.Equalf(t, ev[k-1].next, ev[k].prev,
					"%s: discontinuous chain at index %d — prior next=%s, this prev=%s (events: %+v)",
					name, k, ev[k-1].next, ev[k].prev, ev)
			}
		}

		// Segment into cycles. Per cycle, in order: an OPTIONAL entering-NotSelected (coalesced away when
		// a fast Select pre-commits Selected before the supervisor reacts to TCP-up), a required
		// entering-Selected, then a required terminal entering-NotConnected.
		seenCycles := 0
		i := 0
		for i < len(ev) {
			if ev[i].next == hsms.NotSelectedState {
				i++ // consume the optional intermediate NotSelected
			}

			require.Lessf(t, i, len(ev),
				"%s: cycle %d truncated before entering Selected (events: %+v)", name, seenCycles, ev)
			require.Equalf(t, hsms.SelectedState, ev[i].next,
				"%s: cycle %d: expected entering Selected, got %s (events: %+v)", name, seenCycles, ev[i].next, ev)
			i++

			require.Lessf(t, i, len(ev),
				"%s: cycle %d truncated before terminal NotConnected (events: %+v)", name, seenCycles, ev)
			require.Equalf(t, hsms.NotConnectedState, ev[i].next,
				"%s: cycle %d: expected terminal NotConnected, got %s (events: %+v)", name, seenCycles, ev[i].next, ev)
			i++

			seenCycles++
		}

		require.Equalf(t, cycles, seenCycles,
			"%s: expected %d observed cycles, got %d (events: %+v)", name, cycles, seenCycles, ev)
	}

	assertCycleTransitions("active", activeObs)
	assertCycleTransitions("passive", passiveObs)
}

// TestLifecycle_CloseIsBoundedUnderBlockingHandler proves C1: a data handler that blocks runs INLINE
// on the recv goroutine, so it would wedge the recv loop and, in turn, Stop's g.recv join and Close
// itself — forever. The bounded teardown join (Stop honoring the close-timeout ctx) makes Close
// return ErrCloseTimeout instead, abandoning the wedged straggler (recvLoop's gen-ctx guard keeps
// that straggler from later injecting a stale TCPDown). Without the fix Close hangs and this test
// times out — the exact F2-hang class the v2 redesign set out to dissolve.
func TestLifecycle_CloseIsBoundedUnderBlockingHandler(t *testing.T) {
	t.Parallel()

	port := freeLoopbackPort(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) }) // runs last: let the wedged straggler handler exit at test end

	var once sync.Once
	blockingHandler := func(_ *hsms.DataMessage, _ hsms.SECS2Endpoint) {
		once.Do(func() { close(entered) })
		<-release
	}

	passive := newEndpoint(t, port, false, []Option{
		WithConnectionOption(hsms.WithCloseTimeout(500 * time.Millisecond)),
	}, blockingHandler)
	active := newEndpoint(t, port, true, nil)
	t.Cleanup(func() { closeEndpoint(t, active) })

	require.NoError(t, passive.conn.Open(context.Background(), hsms.OpenBackground))
	require.NoError(t, active.conn.Open(context.Background(), hsms.OpenBackground))
	waitSelected(t, active)
	waitSelected(t, passive)

	// Drive a data message so the passive recv goroutine enters — and blocks in — the handler.
	require.NoError(t, active.conn.SendDataMessageAsync(context.Background(), 1, 1, false, secs2.A("wedge")))

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("blocking data handler was never invoked")
	}

	// Close MUST return promptly (bounded by the 500ms close timeout) with ErrCloseTimeout — not hang.
	done := make(chan error, 1)
	go func() { done <- passive.conn.Close() }()

	select {
	case err := <-done:
		require.ErrorIs(t, err, hsms.ErrCloseTimeout,
			"Close with a wedged inline data handler must return ErrCloseTimeout, not nil")
	case <-time.After(5 * time.Second):
		t.Fatal("Close HUNG behind the blocking data handler — the bounded-Stop (C1) fix is not working")
	}
}
