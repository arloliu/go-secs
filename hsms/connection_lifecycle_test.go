package hsms

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// newLifeConn builds a connection engine wired to a scripted mockTransport for the lifecycle
// tests, and registers a cleanup Close so no generation / supervisor goroutine leaks across a
// -count run. It returns the concrete engine and the mock so tests can drive and inspect it.
func newLifeConn(t *testing.T, script mockScript) (*connection, *mockTransport) {
	t.Helper()

	mt := &mockTransport{startFn: script}
	conn, err := NewConnection(DefaultConnectionConfig(), mt)
	require.NoError(t, err)

	c, ok := conn.(*connection)
	require.True(t, ok, "NewConnection must return the concrete *connection")

	t.Cleanup(func() { _ = c.Close() }) // idempotent; ErrNotOpen if never opened

	return c, mt
}

// currentEpochForTest is the in-package accessor the reaction test uses to lock the REAL
// epoch write lock (spec §7.E teeth-check).
func (c *connection) currentEpochForTest() *epoch { return c.cur.Load() }

// requireSelected waits (subscription-free bounded poll) for the FSM to reach Selected.
func requireSelected(t *testing.T, c *connection) {
	t.Helper()
	require.Eventually(t, func() bool { return c.State() == SelectedState }, time.Second, time.Millisecond)
}

// ── Open ──────────────────────────────────────────────────────────────────────

// TestOpen_DoubleOpenNoOpNoHang: a second Open on a LIVE generation returns ErrAlreadyOpen as a
// no-op — it must NEVER <-e.done on a live generation (H6). Teeth: an unconditional <-e.done on
// the live gen hangs and this times out.
func TestOpen_DoubleOpenNoOpNoHang(t *testing.T) {
	c, _ := newLifeConn(t, withMockTransport())
	require.NoError(t, c.Open(t.Context(), OpenBackground))

	done := make(chan error, 1)
	go func() { done <- c.Open(t.Context(), OpenBackground) }()

	select {
	case err := <-done:
		require.ErrorIs(t, err, ErrAlreadyOpen)
	case <-time.After(time.Second):
		t.Fatal("double Open hung — must no-op, never <-done on a live generation (H6)")
	}
}

// TestOpen_WaitSelectedHonorsCtx: OpenWaitSelected returns the caller ctx error when selection
// never completes within the deadline.
func TestOpen_WaitSelectedHonorsCtx(t *testing.T) {
	c, _ := newLifeConn(t, withMockTransportNeverSelects())
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	require.ErrorIs(t, c.Open(ctx, OpenWaitSelected), context.DeadlineExceeded)
}

// TestOpen_BackgroundReturns: OpenBackground returns promptly after kickoff (before selection).
func TestOpen_BackgroundReturns(t *testing.T) {
	c, _ := newLifeConn(t, withMockTransport())

	done := make(chan error, 1)
	go func() { done <- c.Open(t.Context(), OpenBackground) }()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("OpenBackground must return promptly after kickoff")
	}

	requireSelected(t, c) // and selection still completes in the background
}

// TestOpen_TrNilError: Open on a connection built without a transport returns an error.
func TestOpen_TrNilError(t *testing.T) {
	conn, err := NewConnection(DefaultConnectionConfig(), nil)
	require.NoError(t, err)

	c, ok := conn.(*connection)
	require.True(t, ok)
	require.Error(t, c.Open(context.Background(), OpenBackground))
}

// TestOpenCloseReopenRestartsSupervisorNoClosedChannelPanic: Open after Close must create a
// FRESH supervisor (no send-on-closed panic from reusing stopped channels), and handlers
// registered once persist across cycles. Runs under -race -count to expose the closed-channel
// race (the reopen race, round-5).
func TestOpenCloseReopenRestartsSupervisorNoClosedChannelPanic(t *testing.T) {
	c, _ := newLifeConn(t, withMockTransport())

	var count atomic.Int64
	c.AddConnStateChangeHandler(func(_, _ ConnState) { count.Add(1) }) // registered ONCE

	for range 3 {
		require.NoError(t, c.Open(t.Context(), OpenBackground))
		requireSelected(t, c)
		require.NoError(t, c.Close())
	}

	require.Positive(t, count.Load(), "handler persists across Open/Close cycles; no closed-channel panic on reopen")
}

// TestOpenCloseReopen_DataMessageChanPersists verifies that a channel registered via
// AddDataMessageChan before the first Open (edge case 8: registration is permanent for the
// connection's lifetime) still receives inbound messages after a Close/Open cycle, exactly like
// a registered DataMessageHandler.
func TestOpenCloseReopen_DataMessageChanPersists(t *testing.T) {
	c, _ := newLifeConn(t, withMockTransport())

	ch := make(chan *DataMessage, 1)
	c.AddDataMessageChan(ch) // registered ONCE, before any Open

	for range 3 {
		require.NoError(t, c.Open(t.Context(), OpenBackground))
		requireSelected(t, c)

		msg := mustDataMsg(t)
		c.recvDataMsg(msg) // promoted from the embedded session; exercises the live epoch's Done()

		select {
		case got := <-ch:
			require.Same(t, msg, got, "the channel registered before the first Open must keep receiving after reopen")
		case <-time.After(time.Second):
			t.Fatal("channel registered before Open did not receive after reopen")
		}

		require.NoError(t, c.Close())
	}
}

// TestOpen_WhileReconnectingReturnsErrAlreadyOpen (H6 guard fix): a second Open in the
// reconnect inter-generation window — supervisor alive (shutdown==false) but cur points at
// a done-closed epoch — must return ErrAlreadyOpen, NOT build a second supervisor on supWg.
//
// Teeth: with the old cur-done-based H6 guard, this Open would pass the guard (done closed),
// add two more goroutines to supWg, and store a second supervisor — Close's supWg.Wait()
// would then wait forever for the first supervisor's goroutines (which nobody stopped):
// a deterministic deadlock. Verify by temporarily reverting Open's guard to the cur.done
// select and running with -timeout=10s: Close will hang and the test times out.
func TestOpen_WhileReconnectingReturnsErrAlreadyOpen(t *testing.T) {
	// A long T5 keeps the reconnect loop parked in its backoff throughout the test window.
	// Close interrupts it via reconnectCancel so the test still finishes promptly.
	c, mt := newLifeConn(t, withMockTransport())
	require.NoError(t, c.UpdateConfigOptions(WithT5(500*time.Millisecond)))

	require.NoError(t, c.Open(t.Context(), OpenBackground))
	requireSelected(t, c)

	prevEpoch := c.cur.Load()
	mt.simulateReadError(io.EOF) // involuntary drop → reconnect loop starts, T5 backoff begins

	// Wait for the prior epoch to be fully torn down (prevEpoch.done closes). At this point:
	//   cur  → done-closed epoch (just torn down)
	//   supervisor → alive, shutdown==false
	// This is the exact window where the old cur-done H6 guard incorrectly allowed a reopen.
	select {
	case <-prevEpoch.done:
	case <-time.After(3 * time.Second):
		t.Fatal("prior epoch must tear down (done must close) within 3s")
	}

	// Second Open must be rejected as ErrAlreadyOpen (supervisor alive, shutdown==false).
	// It must NOT build a second supervisor on supWg.
	openDone := make(chan error, 1)
	go func() { openDone <- c.Open(t.Context(), OpenBackground) }()
	select {
	case err := <-openDone:
		require.ErrorIs(t, err, ErrAlreadyOpen,
			"Open in reconnect window (cur done-closed, supervisor alive) must return ErrAlreadyOpen (H6)")
	case <-time.After(time.Second):
		t.Fatal("second Open in reconnect window must return promptly")
	}

	// Close must complete promptly. With the old guard a second supervisor would have been
	// built; Close's supWg.Wait() would then block forever (first supervisor never stopped).
	closeDone := make(chan error, 1)
	go func() { closeDone <- c.Close() }()
	select {
	case err := <-closeDone:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("Close hung — supWg.Wait must not deadlock after a no-op second Open (H6 fix)")
	}
}

// ── Close ─────────────────────────────────────────────────────────────────────

// TestClose_ReturnsBoundedAndClean: Close after a Selected open returns promptly (bounded) and
// cleanly, leaves the FSM NotConnected, and is idempotent.
func TestClose_ReturnsBoundedAndClean(t *testing.T) {
	c, _ := newLifeConn(t, withMockTransport())
	require.NoError(t, c.Open(t.Context(), OpenBackground))
	requireSelected(t, c)

	start := time.Now()
	require.NoError(t, c.Close())
	require.Less(t, time.Since(start), 2*time.Second, "Close must be bounded")
	require.Equal(t, NotConnectedState, c.State())
	require.NoError(t, c.Close()) // idempotent
}

// TestClose_BeforeOpenReturnsErrNotOpen: Close before any Open returns ErrNotOpen (no nil-deref).
func TestClose_BeforeOpenReturnsErrNotOpen(t *testing.T) {
	c, _ := newLifeConn(t, withMockTransport())
	require.ErrorIs(t, c.Close(), ErrNotOpen)
}

// TestClose_IdempotentReCloseDoesNotHang: a second Close (after the first ran sup.stop(), so
// run() exited and events has no reader) short-circuits to the prior result — it must NOT block
// on a blocking inject(evClose) onto the now-unread events channel (round-4).
func TestClose_IdempotentReCloseDoesNotHang(t *testing.T) {
	c, _ := newLifeConn(t, withMockTransport())
	require.NoError(t, c.Open(t.Context(), OpenBackground))
	requireSelected(t, c)
	require.NoError(t, c.Close()) // first close: drives teardown + sup.stop()

	// Fill the supervisor's now-unread events buffer so a second requestClose's inject would
	// block forever WITHOUT the already-stopped short-circuit AND the runDone backstop (round-4
	// teeth). run() has exited, so these direct sends fill the buffer to capacity.
	s := c.sup.Load()
	for range cap(s.events) {
		select {
		case s.events <- fsmCommand{ev: evTCPUp}:
		default:
		}
	}

	done := make(chan error, 1)
	go func() { done <- c.Close() }() // second close: must short-circuit
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("re-Close hung — Close must short-circuit when the supervisor already stopped (round-4)")
	}
}

// TestClose_WhileNotSelectedDoesNotHang: OpenBackground returns before selection; Close while
// the FSM is still NotSelected (TCP up, not selected) must tear down and return — NOT block on
// e.done because no transition-to-NotConnected fired (round-2 Critical).
func TestClose_WhileNotSelectedDoesNotHang(t *testing.T) {
	c, _ := newLifeConn(t, withMockTransportThatConnectsButNeverSelects())
	require.NoError(t, c.Open(t.Context(), OpenBackground))
	require.Eventually(t, func() bool { return c.State() == NotSelectedState }, time.Second, time.Millisecond)

	done := make(chan error, 1)
	go func() { done <- c.Close() }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Close while NotSelected hung (round-2 Critical)")
	}
}

// TestClose_WhileDialingNotConnectedDoesNotHang: Close while the FSM is still NotConnected
// (dial in progress, no TCPUp) must ensure-teardown of the pinned epoch from NotConnected —
// where no transition fires — and return (round-2/7 Critical).
func TestClose_WhileDialingNotConnectedDoesNotHang(t *testing.T) {
	c, _ := newLifeConn(t, withMockTransportThatNeverConnects())
	require.NoError(t, c.Open(t.Context(), OpenBackground))
	require.Equal(t, NotConnectedState, c.State())

	done := make(chan error, 1)
	go func() { done <- c.Close() }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Close while dialing (NotConnected) hung — evClose must ensure teardown from NotConnected (round-2 Critical)")
	}
}

// TestClose_UnblocksStalledDataMessageChanConsumer verifies AddDataMessageChan's Close
// interaction (Item 1 of the v2.4 final review): a consumer that never drains its channel does
// NOT make Close burn the full close timeout. teardown cancels the generation ctx (e.cancel, the
// first statement in epoch.teardown, run when the supervisor reacts to evClose) well before
// Close's e.wait() can return anything else, so the delivery select in recvDataMsg
// (case <-s.rt.Done()) unblocks immediately — Close returns nil well inside the close timeout,
// and the in-flight message delivery is simply dropped (at-most-once), UNLIKE a func handler
// that blocks forever, which nothing here can preempt.
//
// Teeth-check: remove `case <-s.rt.Done()` from the channel-delivery select in session.go
// (recvDataMsg) — this test then times out waiting for Close.
func TestClose_UnblocksStalledDataMessageChanConsumer(t *testing.T) {
	c, _ := newLifeConn(t, withMockTransport())
	require.NoError(t, c.Open(t.Context(), OpenBackground))
	requireSelected(t, c)

	// A close timeout generous enough that, if the documented "burns the full timeout" claim were true, Close would take the whole duration —
	// the elapsed-time assertion below then catches a regression instead of merely being consistent with either outcome.
	require.NoError(t, c.UpdateConfigOptions(WithCloseTimeout(3*time.Second)))

	ch := make(chan *DataMessage) // unbuffered, never drained: delivery blocks with no reader
	c.AddDataMessageChan(ch)

	// Signals that the delivery goroutine below has run past the func-handler stage and is
	// about to enter the channel-delivery select — the point at which it blocks on the full
	// channel. A channel-close signal, not a sleep (300-testing.md async rules).
	handlerRan := make(chan struct{})
	c.AddDataMessageHandler(func(*DataMessage, SECS2Endpoint) { close(handlerRan) })

	msg := mustDataMsg(t)
	recvDone := make(chan struct{})
	go func() {
		defer close(recvDone)
		c.recvDataMsg(msg) // promoted from the embedded session; models a recv-loop delivery
	}()
	<-handlerRan // the delivery goroutine is now parked in the channel-delivery select

	start := time.Now()
	closeErr := make(chan error, 1)
	go func() { closeErr <- c.Close() }()

	select {
	case err := <-closeErr:
		require.NoError(t, err, "Close must unblock the stalled delivery promptly, not report ErrCloseTimeout")
		require.Less(t, time.Since(start), 2*time.Second, "Close must return well inside the 3s close timeout, not burn it")
	case <-time.After(4 * time.Second):
		t.Fatal("Close did not return — a stalled channel consumer must not wedge Close")
	}

	select {
	case <-recvDone:
		// The stalled delivery unblocked in response to teardown's ctx cancellation (J5).
	case <-time.After(time.Second):
		t.Fatal("recvDataMsg did not unblock after Close returned")
	}
}

// ── NotConnected reaction (farewell Separate) ───────────────────────────────────

// TestReaction_GracefulSendsFarewellSeparate: a graceful Close from Selected writes exactly one
// courtesy Separate.req (§7.E).
func TestReaction_GracefulSendsFarewellSeparate(t *testing.T) {
	c, mt := newLifeConn(t, withMockTransport())
	require.NoError(t, c.Open(t.Context(), OpenBackground))
	requireSelected(t, c)
	require.NoError(t, c.Close())

	require.Equal(t, 1, mt.separateWrites(), "graceful teardown from Selected writes one farewell Separate")
}

// TestReaction_CommsFailureSendsNoSeparate: a comms-failure teardown (read error → TCPDown)
// sends NO Separate — the link is already gone (§9.1.1).
func TestReaction_CommsFailureSendsNoSeparate(t *testing.T) {
	c, mt := newLifeConn(t, withMockTransport())
	require.NoError(t, c.Open(t.Context(), OpenBackground))
	requireSelected(t, c)

	mt.simulateReadError(io.EOF) // drives TCPDown(comms-failure)
	require.Eventually(t, func() bool { return c.State() == NotConnectedState }, time.Second, time.Millisecond)

	require.Equal(t, 0, mt.separateWrites(), "comms-failure teardown sends NO Separate (§9.1.1)")
}

// TestReaction_FarewellNeverBlocksTeardown: holding the REAL epoch write lock (as a wedged
// W-bit writer would) must not make the farewell block teardown — the farewell TryLock fails,
// it is skipped, and the unconditional closeSocket() still runs so Close is bounded (§7.E).
// Teeth: a blocking Lock() in the farewell hangs this test.
func TestReaction_FarewellNeverBlocksTeardown(t *testing.T) {
	c, _ := newLifeConn(t, withMockTransport())
	require.NoError(t, c.Open(t.Context(), OpenBackground))
	requireSelected(t, c)

	e := c.currentEpochForTest()
	e.writeMu.Lock()
	defer e.writeMu.Unlock()

	start := time.Now()
	require.NoError(t, c.Close())
	require.Less(t, time.Since(start), 2*time.Second, "farewell must not block teardown behind a held writeMu (§7.E)")
}

// ── cfg atomic-swap race ────────────────────────────────────────────────────────

// TestUpdateConfigOptions_ConcurrentWithReads: concurrent UpdateConfigOptions writers vs
// Timers/SessionID readers must be -race clean (the atomic.Pointer swap never mutates the live
// config in place). Teeth: reverting UpdateConfigOptions to an in-place *c=scratch makes -race
// fail here.
func TestUpdateConfigOptions_ConcurrentWithReads(t *testing.T) {
	_, c := newTestConn(t)

	var wg sync.WaitGroup
	for i := range 8 {
		d := time.Duration(i+1) * time.Second
		wg.Go(func() {
			for range 200 {
				_ = c.UpdateConfigOptions(WithT3(d), WithSessionID(uint16(i))) //nolint:gosec // small test index
			}
		})
	}
	for range 8 {
		wg.Go(func() {
			for range 200 {
				_ = c.Timers()
				_ = c.SessionID()
			}
		})
	}
	wg.Wait()
}
