package hsmsss

// procedures_test.go — control-procedure tests for the hsmsss transport (spec §6.3):
// the auto-linktest initiator (D5a-5), the Linktest responder, the responder-only Deselect
// (D5a-4), and the E37.1 §7.6 Separate tear-down-in-any-connected-substate guard.
//
// The file lives in package hsmsss (not hsmsss_test) because transport is unexported;
// only in-package tests can construct *transport values and drive the responder helpers
// directly. The recording runtime (recRT, reader_test.go) captures WriteMessage /
// SendAsync / SelectLost / TCPDown and exposes a settable State + linktest config.
//
// All tests run with -race and use require.Eventually / channel waits (never time.Sleep) to
// synchronise with the auto-linktest goroutine.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/stretchr/testify/require"
)

// newLinktestTransport builds a *transport wired to rt with tr.wg.ctx set, WITHOUT
// dialing a socket: the control-procedure helpers under test (startLinktest / stopLinktest /
// handleLinktestReq / handleDeselectReq / handleSeparateReq) only touch rt and the current
// generation bundle's own linktest/T7 fields, so no real TCP connection is needed.
// rt takes the hsms.TransportRuntime interface (not the concrete *recRT) so a caller can supply a
// thin wrapper around recRT that overrides a single method (e.g. commitSuccessRT below).
func newLinktestTransport(t *testing.T, rt hsms.TransportRuntime, ctx context.Context) *transport {
	t.Helper()

	cfg, err := NewConfig("127.0.0.1", 5000)
	require.NoError(t, err)

	tr := newTransport(cfg)
	tr.rt = rt
	tr.wg.ctx = ctx

	return tr
}

// waitLinktestExit asserts the auto-linktest goroutine has fully exited (the current generation's
// linktest WaitGroup drained) within a bounded time — proving stopLinktest / a threshold disconnect
// actually stopped it. These single-generation tests never swap tr.wg, so tr.wg is the bundle the
// goroutine registered on.
func waitLinktestExit(t *testing.T, tr *transport) {
	t.Helper()

	done := make(chan struct{})
	go func() {
		tr.wg.linktest.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("auto-linktest goroutine did not exit within timeout")
	}
}

// TestLinktest_AutoFiresWhileSelected — with a short interval and State()==Selected, the
// auto-linktest goroutine issues repeated Linktest.req; cancelling it (stopLinktest) halts it.
func TestLinktest_AutoFiresWhileSelected(t *testing.T) {
	t.Parallel()

	rt := newRecRT()
	rt.setState(hsms.SelectedState)
	rt.setLinktest(10*time.Millisecond, 3)
	rt.setTimers(hsms.TimerConfig{T6: time.Second})
	rt.setWriteMsgFn(func(_ context.Context, msg hsms.Message) (hsms.Message, error) {
		return msg, nil // linktest transaction succeeds
	})

	ctx := t.Context()

	tr := newLinktestTransport(t, rt, ctx)
	tr.startLinktest(tr.wg)

	require.Eventually(t, func() bool { return rt.writtenCount() >= 2 },
		15*time.Second, 5*time.Millisecond, "auto-linktest must issue Linktest.req while Selected")
	require.Equal(t, hsms.LinktestReqType, rt.lastWritten().Type(), "auto send must be a Linktest.req")

	// Stopping the linktest (ctx cancel) halts the goroutine: it exits and issues no more sends.
	tr.stopLinktest(tr.wg)
	waitLinktestExit(t, tr)

	stopped := rt.writtenCount()
	require.Never(t, func() bool { return rt.writtenCount() > stopped },
		100*time.Millisecond, 20*time.Millisecond, "no Linktest.req after the goroutine stopped")
}

// TestLinktest_ThresholdDisconnect — every linktest times out (T6); after exactly threshold
// consecutive failures TCPDown(errLinktestFailed) fires (and not before).
func TestLinktest_ThresholdDisconnect(t *testing.T) {
	t.Parallel()

	const threshold = 4

	rt := newRecRT()
	rt.setState(hsms.SelectedState)
	rt.setLinktest(5*time.Millisecond, threshold)
	rt.setTimers(hsms.TimerConfig{T6: time.Second})
	rt.setWriteMsgFn(func(_ context.Context, _ hsms.Message) (hsms.Message, error) {
		return nil, context.DeadlineExceeded // every linktest times out (T6)
	})

	ctx := t.Context()

	tr := newLinktestTransport(t, rt, ctx)
	tr.startLinktest(tr.wg)

	select {
	case <-rt.tcpDownCh:
	case <-time.After(15 * time.Second):
		t.Fatal("threshold consecutive linktest T6 timeouts must drive TCPDown")
	}
	require.ErrorIs(t, rt.tcpDownCause(), errLinktestFailed, "disconnect cause must be errLinktestFailed")

	// runLinktest returns immediately after TCPDown, so exactly `threshold` linktests were
	// attempted — proving the disconnect fires on the Nth failure, not before.
	waitLinktestExit(t, tr)
	require.Equal(t, threshold, rt.writtenCount(),
		"TCPDown must fire on exactly the Nth consecutive failure, not before")
	// Every T6-timeout round-trip ran with the parent ctx ALIVE (ctx.Err()==nil), so each counts as a
	// linktest error — the increment side of the runLinktest guard (see NotCountedOnParentCancel below).
	require.Equal(t, uint64(threshold), tr.metrics.LinktestErrCount(),
		"each T6-timeout linktest round-trip (parent ctx alive) must increment LinktestErrCount")
}

// TestLinktest_SuccessResetsFailCounter — a single successful linktest between failures resets
// the consecutive-failure counter, so the disconnect only fires after a fresh run of threshold
// failures.
func TestLinktest_SuccessResetsFailCounter(t *testing.T) {
	t.Parallel()

	const threshold = 3

	rt := newRecRT()
	rt.setState(hsms.SelectedState)
	rt.setLinktest(5*time.Millisecond, threshold)
	rt.setTimers(hsms.TimerConfig{T6: time.Second})

	var calls atomic.Int32
	rt.setWriteMsgFn(func(_ context.Context, msg hsms.Message) (hsms.Message, error) {
		// Sequence: fail, fail, SUCCESS (resets), fail, fail, fail -> disconnect on the 6th.
		if calls.Add(1) == 3 {
			return msg, nil
		}

		return nil, context.DeadlineExceeded
	})

	ctx := t.Context()

	tr := newLinktestTransport(t, rt, ctx)
	tr.startLinktest(tr.wg)

	select {
	case <-rt.tcpDownCh:
	case <-time.After(15 * time.Second):
		t.Fatal("linktest must eventually disconnect after a fresh run of threshold failures")
	}
	require.ErrorIs(t, rt.tcpDownCause(), errLinktestFailed)

	waitLinktestExit(t, tr)
	require.Equal(t, 6, rt.writtenCount(),
		"a single success must reset the fail counter (no disconnect at the 3rd attempt)")
	// The CUMULATIVE error metric counts all 5 failures and does NOT reset on the mid-sequence success,
	// which is instead recorded as the single successful round-trip (recv). This is the observational
	// counter being deliberately distinct from the internal consecutive-failure counter that drives the
	// disconnect (the load-bearing property integration_linktest_threshold_test.go proves end-to-end).
	require.Equal(t, uint64(5), tr.metrics.LinktestErrCount(),
		"the cumulative error metric counts all 5 failures and never resets on the mid-sequence success")
	require.Equal(t, uint64(1), tr.metrics.LinktestRecvCount(),
		"the one successful round-trip is counted as a linktest recv")
}

// TestLinktest_ErrCount_NotCountedOnParentCancel mirrors the deleted hsms-level white-box
// "teardown-not-counted" / "caller-context-deadline-exceeded" tests (relocated here with the linktest
// counters): a linktest write that fails because the PARENT ctx was cancelled — teardown / Deselect /
// drop, or a caller deadline — must NOT be counted as a linktest error. runLinktest returns via
// `if ctx.Err() != nil { return }` BEFORE reaching incLinktestErr.
//
// It is deterministic, not timing-raced: the write fn cancels the generation ctx (from which
// startLinktest derives the linktest ctx) and THEN returns an error, so by the time the post-write
// guard runs, ctx.Err() is already non-nil — no race with any protocol timer. TEETH: delete the
// `if ctx.Err() != nil { return }` guard and LinktestErrCount becomes 1.
func TestLinktest_ErrCount_NotCountedOnParentCancel(t *testing.T) {
	t.Parallel()

	rt := newRecRT()
	rt.setState(hsms.SelectedState)
	rt.setLinktest(5*time.Millisecond, 3)
	rt.setTimers(hsms.TimerConfig{T6: time.Second})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tr := newLinktestTransport(t, rt, ctx)

	rt.setWriteMsgFn(func(_ context.Context, _ hsms.Message) (hsms.Message, error) {
		// Simulate a teardown / caller-cancel racing the in-flight linktest write: cancel the
		// generation ctx (the linktest ctx is derived from it) BEFORE the write returns, so the
		// post-write `if ctx.Err() != nil` guard fires and the failure is excluded.
		cancel()

		return nil, context.Canceled
	})

	tr.startLinktest(tr.wg)
	waitLinktestExit(t, tr)

	require.Equal(t, uint64(1), tr.metrics.LinktestSendCount(),
		"the send was attempted (incLinktestSend precedes the write)")
	require.Equal(t, uint64(0), tr.metrics.LinktestErrCount(),
		"a parent-ctx cancel (teardown/Deselect/drop/caller-deadline) must NOT count as a linktest error")
	require.Equal(t, uint64(0), tr.metrics.LinktestRecvCount(),
		"a cancelled write is neither a success nor a counted error")
}

// TestLinktest_ErrCount_NotCountedOnConnClosedRace covers the OTHER teardown race documented in
// runLinktest: the hsms epoch's own ctx can be cancelled by Close() an instant before the PARENT
// (generation) ctx runLinktest itself observes becomes cancelled — two separate cancellation
// cascades with no ordering guarantee between them — so a teardown-aborted reply wait can surface
// as hsms.ErrConnClosed while ctx.Err() still reads nil.
// Unlike TestLinktest_ErrCount_NotCountedOnParentCancel above, ctx here is NEVER cancelled: the write fn
// returns hsms.ErrConnClosed directly with the parent ctx alive throughout, so the fix's
// errors.Is(err, hsms.ErrConnClosed) disjunct — not the ctx.Err() check — is what has to catch it.
// The fail threshold is 1 so a miscount is both immediate and terminal (TCPDown fires on the very
// first counted failure), giving a fast, direct assertion failure rather than a goroutine hang.
// TEETH: delete the `|| errors.Is(err, hsms.ErrConnClosed)` disjunct in runLinktest and this fails
// -> LinktestErrCount becomes 1 (and TCPDown fires with cause errLinktestFailed).
func TestLinktest_ErrCount_NotCountedOnConnClosedRace(t *testing.T) {
	t.Parallel()

	rt := newRecRT()
	rt.setState(hsms.SelectedState)
	rt.setLinktest(5*time.Millisecond, 1)
	rt.setTimers(hsms.TimerConfig{T6: time.Second})
	rt.setWriteMsgFn(func(_ context.Context, _ hsms.Message) (hsms.Message, error) {
		// Simulate the epoch-ctx/genCtx ordering race: the reply wait aborts with ErrConnClosed
		// while the parent (generation) ctx is still alive — ctx.Err() reads nil at this instant.
		return nil, hsms.ErrConnClosed
	})

	ctx := t.Context()
	tr := newLinktestTransport(t, rt, ctx)

	tr.startLinktest(tr.wg)
	waitLinktestExit(t, tr)

	require.Equal(t, uint64(1), tr.metrics.LinktestSendCount(),
		"the send was attempted (incLinktestSend precedes the write)")
	require.Equal(t, uint64(0), tr.metrics.LinktestErrCount(),
		"a teardown-aborted reply wait (hsms.ErrConnClosed, parent ctx alive) must NOT count as a linktest error")
	require.Equal(t, uint64(0), tr.metrics.LinktestRecvCount(),
		"a teardown-aborted write is neither a success nor a counted error")
	require.False(t, rt.tcpDownDidFire(),
		"a teardown-aborted linktest must not itself trigger a second TCPDown")
}

// TestSeparate_TearsDownWhileNotSelected — the load-bearing teeth-test for E37.1 §7.6:
// a Separate.req received while NOT Selected still tears the link down.
// E37 generic §7.9.2.3 would ignore it; §7.6 narrows that away
// ("After either initiating or receiving a Separate.req message, the entity shall immediately close the TCP/IP connection"),
// and NOT SELECTED is a substate of TCP/IP CONNECTED.
// TEETH: restore the `if t.rt.State() == hsms.SelectedState` guard in handleSeparateReq and this test fails.
func TestSeparate_TearsDownWhileNotSelected(t *testing.T) {
	t.Parallel()

	rt := newRecRT()
	rt.setState(hsms.NotSelectedState)

	ctx := t.Context()
	tr := newLinktestTransport(t, rt, ctx)

	keepReading := tr.handleSeparateReq(ctx, &genWG{})

	require.False(t, keepReading, "Separate must end the recv loop — the caller already drove TCPDown")
	require.True(t, rt.tcpDownDidFire(), "Separate while NotSelected must tear the link down (E37.1 §7.6)")
	require.ErrorIs(t, rt.tcpDownCause(), errPeerSeparate)
	require.Equal(t, uint64(1), tr.metrics.SeparateRecvCount(),
		"a Separate received while NotSelected is still a received Separate")
}

// TestSeparate_CancelledGenerationSkipsTCPDown — the C1 straggler guard on the Separate path.
// TCPDown resolves the CURRENT epoch/supervisor at call time,
// so a Separate read by a generation whose teardown already began must NOT inject evDisconnect —
// it would land on the successor generation and knock it out of NotSelected.
// A cancelled generation ctx is the signal that teardown owns the disconnect.
//
// SCOPE — read this before trusting the name.
// This proves only that an ALREADY-cancelled generation skips the injection.
// It does NOT prove cross-generation safety, and the guard does not provide it:
// cancellation landing BETWEEN the check and the TCPDown call still reaches a successor.
// A deterministic test for that would need to interleave the check with a real teardown and
// successor publication, which the recording runtime here cannot model.
// See handleSeparateReq's comment and the audit's Gap 2 for why closing the hole is a core change.
//
// TEETH: drop the genCtx.Err() check in handleSeparateReq and this test fails.
func TestSeparate_CancelledGenerationSkipsTCPDown(t *testing.T) {
	t.Parallel()

	rt := newRecRT()
	rt.setState(hsms.SelectedState) // the state that DID tear down before the guard existed

	ctx := t.Context()
	tr := newLinktestTransport(t, rt, ctx)

	// Cancel this generation's ctx: teardown has begun and owns the disconnect.
	genCtx, cancel := context.WithCancel(ctx)
	cancel()

	keepReading := tr.handleSeparateReq(genCtx, &genWG{})

	require.False(t, keepReading, "the loop must still end — the generation is going away regardless")
	require.False(t, rt.tcpDownDidFire(),
		"a Separate on a torn-down generation must NOT inject TCPDown (it would hit the successor)")
	require.Equal(t, uint64(1), tr.metrics.SeparateRecvCount(),
		"the frame was still received, so it is still counted")
}

// TestDeselect_WhileSelectedRepliesSuccessAndTransitions — a Deselect.req while Selected is
// answered Deselect.rsp status 0, transitions the FSM via SelectLost, and stops the auto-linktest.
func TestDeselect_WhileSelectedRepliesSuccessAndTransitions(t *testing.T) {
	t.Parallel()

	rt := newRecRT()
	rt.setState(hsms.SelectedState)
	rt.setLinktest(10*time.Millisecond, 3)
	rt.setTimers(hsms.TimerConfig{T6: time.Second})
	rt.setWriteMsgFn(func(_ context.Context, msg hsms.Message) (hsms.Message, error) {
		return msg, nil
	})

	ctx := t.Context()
	tr := newLinktestTransport(t, rt, ctx)

	// Start the auto-linktest so we can prove the Deselect responder stops it.
	tr.startLinktest(tr.wg)
	require.Eventually(t, func() bool { return rt.writtenCount() >= 1 },
		15*time.Second, 5*time.Millisecond, "auto-linktest should be running before Deselect")

	req := hsms.NewDeselectReq(0xFFFF, rt.NextSystemBytes())
	tr.handleDeselectReq(tr.wg, req)

	got := rt.lastSent()
	require.NotNil(t, got)
	require.Equal(t, hsms.DeselectRspType, got.Type(), "responder must answer Deselect.req with a Deselect.rsp")
	require.Equal(t, byte(hsms.DeselectStatusSuccess), got.HeaderBytes()[3], "status must be 0 (success)")
	require.Equal(t, 1, rt.selectLostCalls(), "Selected Deselect must transition via SelectLost")

	// The auto-linktest must be stopped by the responder (stopLinktest).
	waitLinktestExit(t, tr)
}

// TestDeselect_WhileNotSelectedRepliesFailureNoTransition — a Deselect.req while NOT Selected
// is answered with a non-zero (NotEstablished) status and does NOT transition.
func TestDeselect_WhileNotSelectedRepliesFailureNoTransition(t *testing.T) {
	t.Parallel()

	rt := newRecRT()
	rt.setState(hsms.NotSelectedState)

	ctx := t.Context()
	tr := newLinktestTransport(t, rt, ctx)

	req := hsms.NewDeselectReq(0xFFFF, rt.NextSystemBytes())
	tr.handleDeselectReq(tr.wg, req)

	got := rt.lastSent()
	require.NotNil(t, got)
	require.Equal(t, hsms.DeselectRspType, got.Type())
	require.NotEqual(t, byte(hsms.DeselectStatusSuccess), got.HeaderBytes()[3],
		"Deselect while NotSelected must reply a non-zero (failure) status")
	require.Equal(t, byte(hsms.DeselectStatusNotEstablished), got.HeaderBytes()[3],
		"status must be NotEstablished (1)")
	require.Zero(t, rt.selectLostCalls(), "Deselect while NotSelected must NOT transition")
}

// TestSelect_DuplicateWhileSelectedRepliesAlreadyActive — a Select.req received while the
// responder is ALREADY Selected (CommitSelected returns false) is answered Select.rsp status 1
// (SelectStatusAlreadyActive, "Communication Already Active", E37 §8.3.7.2 Table 7 / M5), NOT status 0.
// The commit does not re-fire, so T7 is not re-cancelled and no second auto-linktest is spawned.
func TestSelect_DuplicateWhileSelectedRepliesAlreadyActive(t *testing.T) {
	t.Parallel()

	rt := newRecRT() // CommitSelected() returns false → the already-Selected duplicate case
	rt.setState(hsms.SelectedState)

	ctx := t.Context()
	tr := newLinktestTransport(t, rt, ctx)

	req := hsms.NewSelectReq(0xFFFF, rt.NextSystemBytes())
	tr.handleSelectReq(tr.wg, req)

	got := rt.lastSent()
	require.NotNil(t, got)
	require.Equal(t, hsms.SelectRspType, got.Type(), "responder must answer a Select.req with a Select.rsp")
	require.Equal(t, byte(hsms.SelectStatusAlreadyActive), got.HeaderBytes()[3],
		"a duplicate Select while already Selected must reply status 1 (Communication Already Active)")

	// No genuine transition happened, so the responder must not have spawned an auto-linktest on
	// this generation's bundle (which would leak a goroutine past the test).
	require.Zero(t, rt.writtenCount(), "a duplicate Select must NOT start a second auto-linktest")
}

// commitSuccessRT wraps recRT so CommitSelected reports a genuine NotSelected->Selected commit
// (recRT's own CommitSelected is hard-coded false, modelling the already-Selected / gate-refused
// case only). It exists so the selectedOnce latch tests can drive both commit outcomes from the
// same harness.
type commitSuccessRT struct {
	*recRT
}

func (commitSuccessRT) CommitSelected() bool { return true }

// TestSelect_ResponderLatchesSelectedOnceOnCommit is the responder-side latch teeth: a commit
// that SUCCEEDS (a genuine NotSelected->Selected transition) sets genWG.selectedOnce, and a commit
// that returns false (already Selected, or refused by the core's generation gate) leaves it false.
// Teeth: removing the store on the commit-succeeds branch of handleSelectReq fails the
// "commit_succeeds" row.
func TestSelect_ResponderLatchesSelectedOnceOnCommit(t *testing.T) {
	t.Parallel()

	t.Run("commit_succeeds", func(t *testing.T) {
		t.Parallel()

		rt := commitSuccessRT{newRecRT()}
		rt.setState(hsms.NotSelectedState)

		ctx := t.Context()
		tr := newLinktestTransport(t, rt, ctx)

		req := hsms.NewSelectReq(0xFFFF, rt.NextSystemBytes())
		tr.handleSelectReq(tr.wg, req)

		require.True(t, tr.wg.selectedOnce.Load(),
			"a successful commit must latch genWG.selectedOnce")
	})

	t.Run("commit_refused", func(t *testing.T) {
		t.Parallel()

		rt := newRecRT() // CommitSelected() returns false: already Selected / gate-refused
		rt.setState(hsms.SelectedState)

		ctx := t.Context()
		tr := newLinktestTransport(t, rt, ctx)

		req := hsms.NewSelectReq(0xFFFF, rt.NextSystemBytes())
		tr.handleSelectReq(tr.wg, req)

		require.False(t, tr.wg.selectedOnce.Load(),
			"a refused commit must NOT latch genWG.selectedOnce")
	})
}

// latchRT wraps recRT so the real recv loop's dispatchFrame (transport_recv.go) treats an inbound
// Select.rsp as ROUTED (RouteReply hits) and CommitSelected reports success. recRT's own
// RouteReply/CommitSelected are hard-coded to a miss/refusal, which cannot exercise the initiator
// commit branch dispatchFrame takes after a routed Select.rsp.
type latchRT struct {
	*recRT
}

func (r latchRT) RouteReply(msg hsms.Message) bool {
	r.recRT.RouteReply(msg) // still record it, so routedCount()/lastRouted() stay meaningful

	return true
}

func (latchRT) CommitSelected() bool { return true }

// TestSelect_InitiatorLatchesSelectedOnceOnRoutedSuccess is the initiator-side latch teeth
// (dispatchFrame's routed-reply branch): a routed status-0 Select.rsp whose commit succeeds sets
// genWG.selectedOnce, over the REAL recv loop (startReader).
// A routed status-1 Select.rsp never reaches the commit call at all — dispatchFrame gates the
// commit on select-status 0 — so it must never touch the latch, regardless of what CommitSelected
// would report.
// Teeth: removing the store on dispatchFrame's commit-succeeds branch fails the
// "status_0_routed_commit_succeeds" row.
func TestSelect_InitiatorLatchesSelectedOnceOnRoutedSuccess(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		status byte
		want   bool
	}{
		{"status_0_routed_commit_succeeds", hsms.SelectStatusSuccess, true},
		{"status_1_routed_never_reaches_commit", hsms.SelectStatusAlreadyActive, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rt := latchRT{newRecRT()}

			var tr *transport
			peer := startReader(t, rt, nil, func(installed *transport) { tr = installed })

			reqHdr := header10(0, byte(hsms.SelectReqType))
			_, err := peer.Write(selectRspFrame(reqHdr, tc.status))
			require.NoError(t, err)

			require.Eventually(t, func() bool { return rt.routedCount() >= 1 },
				5*time.Second, 5*time.Millisecond, "the recv loop must route the Select.rsp we sent")

			if tc.want {
				require.Eventually(t, func() bool { return tr.wg.selectedOnce.Load() },
					5*time.Second, 5*time.Millisecond,
					"a routed status-0 commit success must latch genWG.selectedOnce")

				return
			}

			require.Never(t, func() bool { return tr.wg.selectedOnce.Load() },
				100*time.Millisecond, 10*time.Millisecond,
				"a routed status-1 Select.rsp must never touch genWG.selectedOnce")
		})
	}
}

// TestActive_SelectRspStatusHandling is the teeth for the active Select procedure's status
// handling: status 0 (Success) is always a success, and any status >= 2 (Not Ready, Exhaust) is
// always a genuine failure that tears the link down.
// Status 1 (Communication Already Active, E37 Table 7 / M5) is neither unconditionally: it is a
// success ONLY when this generation's genWG.selectedOnce latch already reads true — proof that a
// prior commitSelected succeeded on this generation, which is what a legitimate simultaneous-select
// race leaves behind (the peer already Selected when it answers our Select.req).
// A status-1 answer
// on a generation whose latch is still false means the peer refused our Select outright (its own
// prior session, a stale half-open connection, or go-secs's own passive refuse path), and that MUST
// tear down and reconnect rather than sit NotSelected until T7 finally drops it.
// The latch is deliberately read instead of State(): a peer may answer status 1 and then send its
// own Deselect.req before we observe it, leaving State() at NotSelected on a generation that DID
// select — tearing down on State() alone would drop that link wrongly (see TestT7_DeselectReArms's
// latch-survives-Deselect row for the corresponding preservation guard).
// Teeth: reverting runSelectProcedure to treat status 1 as always-success (dropping the latch
// check) makes the "status 1, latch false" row fail to tear down; reverting it to
// `!= SelectStatusSuccess` (treating status 1 as always-failure) makes both status-1/latch-true rows
// fire TCPDown when they must not.
func TestActive_SelectRspStatusHandling(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		status       byte
		selectedOnce bool
		state        hsms.ConnState
		wantTearDown bool
	}{
		// 0: established.
		// The initiator commit happens on the recv loop AFTER routing (H2), so the latch may still
		// read false here — status 0 must not consult it at all.
		{"status_0_established", hsms.SelectStatusSuccess, false, hsms.SelectedState, false},
		// 0, State() also still NotSelected:
		// the procedure can wake and read a success status before the recv loop's initiator commit lands —
		// status 0 must not consult State() any more than it consults the latch.
		{"status_0_established_state_not_selected", hsms.SelectStatusSuccess, false, hsms.NotSelectedState, false},
		// 1, latch true: a legitimate simultaneous select — this generation already selected.
		{"status_1_already_active_latch_true", hsms.SelectStatusAlreadyActive, true, hsms.SelectedState, false},
		// 1, latch true, but State() has since moved to NotSelected (peer Deselected before/around
		// answering). The commit still happened on this generation, so this must still be treated
		// as success — a State()-based predicate would wrongly tear down here.
		{"status_1_already_active_latch_true_state_not_selected", hsms.SelectStatusAlreadyActive, true, hsms.NotSelectedState, false},
		// 1, latch false: nothing on this generation ever committed — a genuine refusal.
		// State() is deliberately set to Selected here (rather than NotSelected)
		// to prove the teardown decision reads the latch, never State().
		{"status_1_already_active_latch_false", hsms.SelectStatusAlreadyActive, false, hsms.SelectedState, true},
		{"status_2_not_ready", hsms.SelectStatusNotReady, false, hsms.SelectedState, true},
		{"status_3_already_used", hsms.SelectStatusAlreadyUsed, false, hsms.SelectedState, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rt := newRecRT()
			rt.setState(tc.state)
			status := tc.status
			rt.setWriteMsgFn(func(_ context.Context, req hsms.Message) (hsms.Message, error) {
				cm, ok := req.(*hsms.ControlMessage)
				require.True(t, ok, "active procedure must send a *ControlMessage Select.req")
				rsp, err := hsms.NewSelectRsp(cm, status)
				require.NoError(t, err)

				return rsp, nil
			})

			ctx := t.Context()
			tr := newLinktestTransport(t, rt, ctx)

			g := &genWG{}
			if tc.selectedOnce {
				g.selectedOnce.Store(true)
			}

			tr.runSelectProcedure(ctx, g)

			require.Equal(t, tc.wantTearDown, rt.tcpDownDidFire(),
				"%s: tear-down expectation", tc.name)
			if tc.wantTearDown {
				require.ErrorIs(t, rt.tcpDownCause(), errSelectRejected,
					"%s: a torn-down status must report errSelectRejected", tc.name)
			}
		})
	}
}

// TestLinktest_InboundReqAnswered — an inbound Linktest.req is answered with a Linktest.rsp.
func TestLinktest_InboundReqAnswered(t *testing.T) {
	t.Parallel()

	rt := newRecRT()

	ctx := t.Context()
	tr := newLinktestTransport(t, rt, ctx)

	req := hsms.NewLinktestReq(rt.NextSystemBytes())
	tr.handleLinktestReq(tr.wg, req)

	got := rt.lastSent()
	require.NotNil(t, got)
	require.Equal(t, hsms.LinktestRspType, got.Type(), "Linktest.req must be answered with a Linktest.rsp")
	require.Equal(t, uint64(1), tr.metrics.LinktestReqRecvCount(), "inbound Linktest.req must be counted")
}

// TestLinktest_InboundReqAnsweredWhileNotSelected — the deliberate E37.1 §7.4 deviation (audit Gap 3).
// §7.4 limits the use of Linktest to SELECTED and §7.7 classes a violation as a communications failure,
// but E37 §7.8.2's responder procedure is unconditional
// and E37 §9.1.1's remedy is that the entity "should" — not "shall" — terminate.
// We therefore answer the probe and keep the connection, rather than punishing a peer written against E37 generic §7.8.
//
// This pins all three properties the deviation depends on:
// the rsp goes out, the link is NOT torn down, and the probe is still counted.
// It is the guard the plain-Selected test above does not provide,
// since that one exercises recRT's default state.
func TestLinktest_InboundReqAnsweredWhileNotSelected(t *testing.T) {
	t.Parallel()

	rt := newRecRT()
	rt.setState(hsms.NotSelectedState)

	ctx := t.Context()
	tr := newLinktestTransport(t, rt, ctx)

	req := hsms.NewLinktestReq(rt.NextSystemBytes())
	tr.handleLinktestReq(tr.wg, req)

	got := rt.lastSent()
	require.NotNil(t, got, "a Linktest.req while NotSelected must still be answered")
	require.Equal(t, hsms.LinktestRspType, got.Type(), "the answer must be a Linktest.rsp")
	require.False(t, rt.tcpDownDidFire(),
		"answering is deliberate: an out-of-state probe must NOT be treated as a link-dropping failure")
	require.Equal(t, uint64(1), tr.metrics.LinktestReqRecvCount(),
		"an out-of-state Linktest.req is still a received Linktest.req")
}
