package hsmsss

// timers_test.go — T7 NOT-SELECTED dwell lifecycle tests for the hsmsss transport (§6.3 / §9.2.2).
//
// The file lives in package hsmsss (not hsmsss_test) because transport is unexported; only
// in-package tests can construct *transport values and drive armT7 / cancelT7 / runT7 and the
// recvLoop-entry wiring directly. The recording runtime (recRT, reader_test.go) captures
// T7Expired via t7ExpiredCalls()/t7ExpiredCh and exposes a settable State + Timers.
//
// All tests run with -race and use require.Eventually / require.Never / channel waits (never
// time.Sleep) to synchronise with the one-shot T7 dwell goroutine. The lifecycle tests are
// intended to be exercised additionally at -count=100.
//
// T7 design (the DECISION): the T7 goroutine merely injects evT7Timeout via rt.T7Expired() on
// expiry; the CORE supervisor evaluates it SERIALLY — NotSelected -> NotConnected, but a NO-OP
// from Selected/NotConnected — so a validly-Selected session is NEVER torn down by a stale T7
// (see hsms/supervisor_test.go TestSupervisor_T7TimeoutFromSelectedIsNoOp). At this transport
// layer the recRT double simply records the T7Expired call, so cancellation is directly observable.

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/stretchr/testify/require"
)

// waitT7Exit asserts the T7 dwell goroutine has fully exited (the current generation's T7
// WaitGroup drained) within a bounded time — proving cancelT7 / Stop / expiry actually stopped it
// (no goroutine outlives Stop). These single-generation tests never swap tr.wg, so tr.wg is the
// bundle the goroutine registered on and Stop joined.
func waitT7Exit(t *testing.T, tr *transport) {
	t.Helper()

	done := make(chan struct{})
	go func() {
		tr.wg.t7.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("T7 dwell goroutine did not exit within timeout")
	}
}

// TestT7_FiresWhenNotSelectedPersists — with a short T7 and the FSM parked at NotSelected, the
// recvLoop-entry arm starts the dwell and, because Select never completes, T7Expired() fires.
// This exercises the real recvLoop-entry wiring over a loopback socket (not just armT7 in
// isolation): the peer never sends a Select, so the session stays NotSelected until T7 expires.
func TestT7_FiresWhenNotSelectedPersists(t *testing.T) {
	t.Parallel()

	rt := newRecRT()
	rt.setState(hsms.NotSelectedState) // TCP up, never selected — the T7 dwell window
	rt.setTimers(hsms.TimerConfig{T7: 30 * time.Millisecond})

	// startReader spawns the real recv loop, whose entry arms T7 for the live generation.
	_ = startReader(t, rt, nil)

	select {
	case <-rt.t7ExpiredCh:
	case <-time.After(2 * time.Second):
		t.Fatal("T7 must fire (rt.T7Expired) when the session stays NotSelected past T7 (§9.2.2)")
	}
	require.GreaterOrEqual(t, rt.t7ExpiredCalls(), 1, "T7Expired must be invoked on dwell expiry")
}

// TestT7_CancelledWhenSelectCompletes — arming T7 then cancelling it before it elapses (the
// CommitSelected==true path calls cancelT7) means T7Expired() is NEVER called. This is the
// "validly-Selected session is never torn down" property at the transport layer.
//
// Teeth: without cancelT7 actually cancelling the goroutine, the 80ms T7 would fire within the
// 250ms require.Never window and the assertion would bite.
func TestT7_CancelledWhenSelectCompletes(t *testing.T) {
	t.Parallel()

	rt := newRecRT()
	rt.setState(hsms.NotSelectedState)
	rt.setTimers(hsms.TimerConfig{T7: 80 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tr := newLinktestTransport(t, rt, ctx)

	tr.armT7(tr.wg) // enter NotSelected — dwell armed
	tr.cancelT7()   // Select completes before expiry — the CommitSelected==true path cancels T7

	// The goroutine exits promptly on cancellation, and no T7Expired ever fires.
	waitT7Exit(t, tr)
	require.Never(t, func() bool { return rt.t7ExpiredCalls() > 0 },
		250*time.Millisecond, 20*time.Millisecond,
		"a T7 cancelled on reaching Selected must NEVER fire T7Expired (§9.2.2)")
}

// TestT7_DeselectReArms — a Deselect.req while Selected transitions Selected->NotSelected
// (SelectLost) on the SAME TCP connection, so the T7 dwell re-applies: handleDeselectReq re-arms
// T7 and, if no re-Select follows, T7Expired() fires.
func TestT7_DeselectReArms(t *testing.T) {
	t.Parallel()

	rt := newRecRT()
	rt.setState(hsms.SelectedState)
	rt.setTimers(hsms.TimerConfig{T6: time.Second, T7: 30 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tr := newLinktestTransport(t, rt, ctx)

	// Deselect responder: replies success, SelectLost (-> NotSelected), stops linktest, re-arms T7.
	req := hsms.NewDeselectReq(0xFFFF, rt.NextSystemBytes())
	tr.handleDeselectReq(tr.wg, req)
	require.Equal(t, 1, rt.selectLostCalls(), "Deselect while Selected must transition via SelectLost")

	select {
	case <-rt.t7ExpiredCh:
	case <-time.After(2 * time.Second):
		t.Fatal("Deselect must re-arm T7 so a persisting NotSelected fires T7Expired (§9.2.2)")
	}
}

// TestT7_NoGoroutineOutlivesStop — after arming a LONG T7, Stop cancels the dwell (t7Cancel) and
// joins it (the generation's T7 WaitGroup), so Stop returns promptly and no T7 goroutine leaks;
// T7Expired never fires.
//
// Teeth: a long T7 (10s) means that without Stop cancelling the goroutine, waitT7Exit would time
// out (the goroutine would still be parked on the 10s timer) — proving Stop actually reaps it.
func TestT7_NoGoroutineOutlivesStop(t *testing.T) {
	t.Parallel()

	rt := newRecRT()
	rt.setState(hsms.NotSelectedState)
	rt.setTimers(hsms.TimerConfig{T7: 10 * time.Second}) // long: only Stop's cancel can end it in time

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tr := newLinktestTransport(t, rt, ctx)
	tr.armT7(tr.wg)

	require.NoError(t, tr.Stop(context.Background()), "Stop must return")
	waitT7Exit(t, tr) // generation T7 WaitGroup drained — no goroutine outlives Stop
	require.Zero(t, rt.t7ExpiredCalls(), "a T7 cancelled by Stop must not fire T7Expired")
}

// TestT7_DisabledWhenZero — a zero/negative T7 disables the dwell entirely: armT7 spawns no
// goroutine, so the generation's T7 WaitGroup is already drained and T7Expired never fires.
func TestT7_DisabledWhenZero(t *testing.T) {
	t.Parallel()

	rt := newRecRT()
	rt.setState(hsms.NotSelectedState)
	rt.setTimers(hsms.TimerConfig{T7: 0}) // disabled

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tr := newLinktestTransport(t, rt, ctx)
	tr.armT7(tr.wg)

	waitT7Exit(t, tr) // nothing was spawned
	require.Never(t, func() bool { return rt.t7ExpiredCalls() > 0 },
		100*time.Millisecond, 20*time.Millisecond, "a disabled (T7<=0) dwell must never fire")
}
