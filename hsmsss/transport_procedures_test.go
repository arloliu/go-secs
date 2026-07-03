package hsmsss

// procedures_test.go — control-procedure tests for the hsmsss transport (spec §6.3):
// the auto-linktest initiator (D5a-5), the Linktest responder, the responder-only Deselect
// (D5a-4), and the §7.9.2 Separate ignore-if-not-Selected guard.
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
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/stretchr/testify/require"
)

// newLinktestTransport builds a *transport wired to rt with genCtx set, WITHOUT dialing a
// socket: the control-procedure helpers under test (startLinktest / stopLinktest /
// handleLinktestReq / handleDeselectReq / handleSeparateReq) only touch rt and the transport's
// own linktest fields, so no real TCP connection is needed.
func newLinktestTransport(t *testing.T, rt *recRT, ctx context.Context) *transport {
	t.Helper()

	cfg, err := NewConfig("127.0.0.1", 5000)
	require.NoError(t, err)

	tr := newTransport(cfg)
	tr.rt = rt
	tr.genCtx = ctx

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
	case <-time.After(2 * time.Second):
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tr := newLinktestTransport(t, rt, ctx)
	tr.startLinktest(tr.wg)

	require.Eventually(t, func() bool { return rt.writtenCount() >= 2 },
		2*time.Second, 5*time.Millisecond, "auto-linktest must issue Linktest.req while Selected")
	require.Equal(t, hsms.LinktestReqType, rt.lastWritten().Type(), "auto send must be a Linktest.req")

	// Stopping the linktest (ctx cancel) halts the goroutine: it exits and issues no more sends.
	tr.stopLinktest()
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tr := newLinktestTransport(t, rt, ctx)
	tr.startLinktest(tr.wg)

	select {
	case <-rt.tcpDownCh:
	case <-time.After(2 * time.Second):
		t.Fatal("threshold consecutive linktest T6 timeouts must drive TCPDown")
	}
	require.ErrorIs(t, rt.tcpDownCause(), errLinktestFailed, "disconnect cause must be errLinktestFailed")

	// runLinktest returns immediately after TCPDown, so exactly `threshold` linktests were
	// attempted — proving the disconnect fires on the Nth failure, not before.
	waitLinktestExit(t, tr)
	require.Equal(t, threshold, rt.writtenCount(),
		"TCPDown must fire on exactly the Nth consecutive failure, not before")
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tr := newLinktestTransport(t, rt, ctx)
	tr.startLinktest(tr.wg)

	select {
	case <-rt.tcpDownCh:
	case <-time.After(2 * time.Second):
		t.Fatal("linktest must eventually disconnect after a fresh run of threshold failures")
	}
	require.ErrorIs(t, rt.tcpDownCause(), errLinktestFailed)

	waitLinktestExit(t, tr)
	require.Equal(t, 6, rt.writtenCount(),
		"a single success must reset the fail counter (no disconnect at the 3rd attempt)")
}

// TestSeparate_IgnoredWhileNotSelected — the load-bearing teeth-test (§7.9.2): a Separate.req
// received while NOT Selected is ignored (handleSeparateReq returns true, keep reading) and
// does NOT tear the link down. TEETH: temporarily make handleSeparateReq always TCPDown and
// this test fails.
func TestSeparate_IgnoredWhileNotSelected(t *testing.T) {
	t.Parallel()

	rt := newRecRT()
	rt.setState(hsms.NotSelectedState)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tr := newLinktestTransport(t, rt, ctx)

	keepReading := tr.handleSeparateReq()

	require.True(t, keepReading, "Separate while NotSelected must keep the recv loop reading")
	require.False(t, rt.tcpDownDidFire(), "Separate while NotSelected must NOT tear the link down (§7.9.2)")
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tr := newLinktestTransport(t, rt, ctx)

	// Start the auto-linktest so we can prove the Deselect responder stops it.
	tr.startLinktest(tr.wg)
	require.Eventually(t, func() bool { return rt.writtenCount() >= 1 },
		2*time.Second, 5*time.Millisecond, "auto-linktest should be running before Deselect")

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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
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

// TestActive_SelectRspStatusHandling is the P1-A teeth: the active Select procedure treats
// Select-status 0 (Success) AND status 1 (Communication Already Active, E37 Table 7 / M5) as
// success — NOT a rejection — while any other non-zero status is a genuine failure that tears the
// link down. Status 1 is reachable in a simultaneous-select race (the peer already Selected when it
// answers our Select.req); tearing down on it would drop a validly-Selected link. Teeth: reverting
// runSelectProcedure to `!= SelectStatusSuccess` makes the status-1 case fire TCPDown and fail.
func TestActive_SelectRspStatusHandling(t *testing.T) {
	t.Parallel()

	cases := []struct {
		status       byte
		wantTearDown bool
	}{
		{hsms.SelectStatusSuccess, false},       // 0: established
		{hsms.SelectStatusAlreadyActive, false}, // 1: already active — NOT a failure (P1-A)
		{hsms.SelectStatusNotReady, true},       // 2: genuine failure
		{hsms.SelectStatusAlreadyUsed, true},    // 3: genuine failure
	}

	for _, tc := range cases {
		t.Run(fmt.Sprintf("status_%d", tc.status), func(t *testing.T) {
			t.Parallel()

			rt := newRecRT()
			rt.setState(hsms.SelectedState)
			status := tc.status
			rt.setWriteMsgFn(func(_ context.Context, req hsms.Message) (hsms.Message, error) {
				cm, ok := req.(*hsms.ControlMessage)
				require.True(t, ok, "active procedure must send a *ControlMessage Select.req")
				rsp, err := hsms.NewSelectRsp(cm, status)
				require.NoError(t, err)

				return rsp, nil
			})

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			tr := newLinktestTransport(t, rt, ctx)

			tr.runSelectProcedure(ctx)

			require.Equal(t, tc.wantTearDown, rt.tcpDownDidFire(),
				"status %d: tear-down expectation", tc.status)
		})
	}
}

// TestLinktest_InboundReqAnswered — an inbound Linktest.req is answered with a Linktest.rsp.
func TestLinktest_InboundReqAnswered(t *testing.T) {
	t.Parallel()

	rt := newRecRT()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tr := newLinktestTransport(t, rt, ctx)

	req := hsms.NewLinktestReq(rt.NextSystemBytes())
	tr.handleLinktestReq(req)

	got := rt.lastSent()
	require.NotNil(t, got)
	require.Equal(t, hsms.LinktestRspType, got.Type(), "Linktest.req must be answered with a Linktest.rsp")
}
