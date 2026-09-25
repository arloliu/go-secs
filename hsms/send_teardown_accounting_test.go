package hsms

// send_teardown_accounting_test.go proves a synchronous data send interrupted by teardown is a lifecycle event, not a send error.
// The caller sees ErrConnClosed, still matching the raw transport error via errors.Is.
// DataMsgErrCount does not count it, WithTransactionObserver reports TxCanceled, and no TCPDown is reported for the interrupted write.
// It also proves the reply-wait select's priority when several outcomes are ready at once:
// a reply beats a concurrent teardown, and a teardown beats a concurrent protocol-timer (T3/T6) timeout.
//
// Two harnesses are used, matching the two teardown entry points a write can race:
//   - the voluntary Close case, through the full lifecycle harness (real Open, supervisor, and Close — mirrors newLifeConn)
//     and a mockTransport carrying a blockingWriteFn whose write blocks until the socket closes.
//   - the shared involuntary-drop / Open-failure path, which never goes through Close: the lightweight send-path harness
//     (newTestSendConn) with the same blockingWriteFn installed, calling epoch.teardown directly.

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// closableBlockingConn is a net.Conn stand-in whose Close unblocks a write parked on it.
// It lets a test reproduce a synchronous send already inside c.tr.Write when epoch.teardown closes the socket underneath it.
type closableBlockingConn struct {
	fakeConn
	closed    chan struct{}
	closeOnce sync.Once
}

func newClosableBlockingConn() *closableBlockingConn {
	return &closableBlockingConn{closed: make(chan struct{})}
}

// Close closes the conn exactly once, releasing any write parked on it.
func (c *closableBlockingConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })

	return nil
}

// blockingWriteFn is a mockTransport writeFn (see harness_mock_transport_test.go)
// that blocks a write targeting a *closableBlockingConn until that conn closes, then returns closedErr
// (net.ErrClosed unless a test overrides it before the write begins).
// Any other conn — a control frame racing the block, or a harness that never installed one —
// delegates to mt's own default frame-capture Write: the choice is by conn identity, not by frame type.
// writeEntered closes exactly once, the instant a blocking write begins,
// guarded by entered against a second write on the same blocking conn (e.g. a retry) closing an already-closed channel;
// it lets a test wait for the exact race window instead of sleeping.
type blockingWriteFn struct {
	mt           *mockTransport
	writeEntered chan struct{}
	entered      sync.Once
	closedErr    error
}

func newBlockingWriteFn(mt *mockTransport) *blockingWriteFn {
	return &blockingWriteFn{mt: mt, writeEntered: make(chan struct{}), closedErr: net.ErrClosed}
}

func (b *blockingWriteFn) write(ctx context.Context, conn net.Conn, bufs net.Buffers) error {
	bc, ok := conn.(*closableBlockingConn)
	if !ok {
		return b.mt.captureWrite(conn, bufs)
	}

	b.entered.Do(func() { close(b.writeEntered) })

	select {
	case <-bc.closed:
		return b.closedErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// requireWriteEntered waits (bounded, never a sleep-poll) for a blockingWriteFn's write to actually be in flight
// before the test drives teardown.
// Without it, teardown could win the race against writeFrame's own early e.ctx.Done() check,
// and the test would exercise a different return path than the one it means to.
func requireWriteEntered(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("write never reached the transport")
	}
}

// requireSendDone waits (bounded) for a send goroutine's result and returns it.
func requireSendDone(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("send never returned")

		return nil
	}
}

// newBlockingWriteConn builds a connection wired to a mockTransport carrying a blockingWriteFn,
// through the real Open/supervisor/Close lifecycle (mirrors newLifeConn).
// TCP comes up on a *closableBlockingConn,
// so a synchronous send that reaches the wire blocks until Close's teardown closes the socket.
func newBlockingWriteConn(t *testing.T, opts ...ConnOption) (*connection, *blockingWriteFn) {
	t.Helper()

	bc := newClosableBlockingConn()

	mt := &mockTransport{startFn: func(ctx context.Context, rt TransportRuntime) error {
		go func() {
			rt.TCPUp(bc)
			driveSelect(ctx, rt)
		}()

		return nil
	}}
	bw := newBlockingWriteFn(mt)
	mt.writeFn = bw.write

	cfg := DefaultConnectionConfig()
	require.NoError(t, cfg.apply(opts...))

	conn, err := NewConnection(cfg, mt)
	require.NoError(t, err)

	c, ok := conn.(*connection)
	require.True(t, ok, "NewConnection must return the concrete *connection")

	t.Cleanup(func() { _ = c.Close() })

	return c, bw
}

// newBlockingWriteLightConn builds the lightweight send-path harness (newTestSendConn).
// It installs a blockingWriteFn on its mockTransport, then swaps the live epoch's socket for a *closableBlockingConn.
// A synchronous send reaching the wire then blocks until the test calls epoch.teardown directly —
// the shared involuntary-drop / Open-failure path, with no supervisor reaction involved.
func newBlockingWriteLightConn(t *testing.T, opts ...ConnOption) (*connection, *epoch, *blockingWriteFn) {
	t.Helper()

	c, mt := newTestSendConn(t, SelectedState, opts...)

	bw := newBlockingWriteFn(mt)
	mt.writeFn = bw.write

	e := c.cur.Load()
	e.setConn(newClosableBlockingConn())

	return c, e, bw
}

// txEventSpy records every TxEvent reported via WithTransactionObserver.
type txEventSpy struct {
	mu     sync.Mutex
	events []TxEvent
}

func (s *txEventSpy) observe(ev TxEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
}

// last returns the most recently observed TxEvent.
func (s *txEventSpy) last() (TxEvent, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.events) == 0 {
		return TxEvent{}, false
	}

	return s.events[len(s.events)-1], true
}

// ── Teardown-interrupted writes are not a send error ────────────────────────────────────────

// TestSendTeardown_CloseInterruptsBlockedWrite proves that a synchronous data send already inside c.tr.Write,
// when a voluntary Close tears the generation down, reports ErrConnClosed (still matching the raw transport error via errors.Is),
// is not counted in DataMsgErrCount, reports TxCanceled to a WithTransactionObserver, and never triggers a TCPDown.
//
// Teeth: removing the e.ctx.Err() != nil check in writeFrame makes every case count the error and report TxSendError instead.
func TestSendTeardown_CloseInterruptsBlockedWrite(t *testing.T) {
	tests := []struct {
		name string
		send func(*connection) error
	}{
		{
			name: "W-bit SendDataMessage",
			send: func(c *connection) error {
				_, err := c.SendDataMessage(t.Context(), 1, 1, true, secs2.NewASCIIItem("PING"))

				return err
			},
		},
		{
			name: "fire-and-forget SendDataMessage",
			send: func(c *connection) error {
				_, err := c.SendDataMessage(t.Context(), 1, 1, false, secs2.NewASCIIItem("PING"))

				return err
			},
		},
		{
			name: "ForwardDataMessage",
			send: func(c *connection) error {
				msg, err := NewDataMessage(1, 1, true, c.SessionID(), c.NextSystemBytes(), secs2.NewASCIIItem("PING"))
				require.NoError(t, err)

				return c.ForwardDataMessage(t.Context(), msg)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spy := &txEventSpy{}
			c, bw := newBlockingWriteConn(t, WithTransactionObserver(spy.observe))
			require.NoError(t, c.Open(t.Context(), OpenWaitSelected))

			done := make(chan error, 1)
			go func() { done <- tt.send(c) }()
			requireWriteEntered(t, bw.writeEntered)

			require.NoError(t, c.Close())
			err := requireSendDone(t, done)

			require.ErrorIs(t, err, ErrConnClosed)
			require.ErrorIs(t, err, net.ErrClosed)
			require.Equal(t, uint64(0), c.metrics.DataMsgErrCount())
			// Guard only: this proves no TCPDown was ALSO reported.
			// It has no teeth against writeFrame dropping the ErrConnClosed wrap — the ErrorIs assertions above cover that.
			require.False(t, c.cur.Load().commsFailure.Load(), "a Close-interrupted write must not report TCPDown")

			ev, ok := spy.last()
			require.True(t, ok, "the send must report exactly one TxEvent")
			require.Equal(t, TxCanceled, ev.Outcome)
		})
	}
}

// TestSendTeardown_DirectTeardownInterruptsBlockedWrite covers the shared involuntary-drop / Open-failure teardown path
// — epoch.teardown called directly, never through Close — on the lightweight send-path harness.
func TestSendTeardown_DirectTeardownInterruptsBlockedWrite(t *testing.T) {
	spy := &txEventSpy{}
	c, e, bw := newBlockingWriteLightConn(t, WithTransactionObserver(spy.observe))

	done := make(chan error, 1)
	go func() {
		_, err := c.WriteMessage(t.Context(), mustSendData(t, [4]byte{0, 0, 0, 1}, true))
		done <- err
	}()
	requireWriteEntered(t, bw.writeEntered)

	e.teardown(2 * time.Second)
	err := requireSendDone(t, done)

	require.ErrorIs(t, err, ErrConnClosed)
	require.ErrorIs(t, err, net.ErrClosed)
	require.Equal(t, uint64(0), c.metrics.DataMsgErrCount())
	// Guard only: this proves no TCPDown was ALSO reported.
	// It has no teeth against writeFrame dropping the ErrConnClosed wrap — the ErrorIs assertions above cover that.
	require.False(t, e.commsFailure.Load(), "a teardown-interrupted write must not report TCPDown")

	ev, ok := spy.last()
	require.True(t, ok, "WriteMessage must report exactly one TxEvent")
	require.Equal(t, TxCanceled, ev.Outcome)
}

// TestSendTeardown_BareErrConnClosedDuringTeardown_NoDoubleWrap covers a transport (such as secs1)
// whose Write itself returns the bare ErrConnClosed sentinel while teardown is in progress:
// writeFrame must return that exact error unchanged, not wrap ErrConnClosed around itself.
func TestSendTeardown_BareErrConnClosedDuringTeardown_NoDoubleWrap(t *testing.T) {
	c, e, bw := newBlockingWriteLightConn(t)
	bw.closedErr = ErrConnClosed

	done := make(chan error, 1)
	go func() {
		_, err := c.sendWaitReply(t.Context(), mustSendData(t, [4]byte{0, 0, 0, 2}, true))
		done <- err
	}()
	requireWriteEntered(t, bw.writeEntered)

	e.teardown(2 * time.Second)
	err := requireSendDone(t, done)

	require.ErrorIs(t, err, ErrConnClosed)
	require.Equal(t, ErrConnClosed, err, "the sentinel must not be double-wrapped")
	require.Equal(t, uint64(0), c.metrics.DataMsgErrCount())
}

// ── Regression guard: a write that fails on a LIVE generation still counts ─────────────────

// TestSendTeardown_LiveGenerationWriteStillCounts proves the teardown exclusion is narrow.
// A write that fails while the generation is still live (no teardown in progress) counts, reports TCPDown with CauseIOError,
// and returns the raw transport error, never ErrConnClosed.
// No production change is expected to make this pass;
// it guards writeFrame's teardown check against widening into a blanket "Close is nearby" rule.
func TestSendTeardown_LiveGenerationWriteStillCounts(t *testing.T) {
	c, mt := newLifeConn(t, withMockTransportNeverSelects())
	log := &eventLog{}
	subscribeLog(t, c, log)
	openSelected(t, c, log)

	writeErr := errors.New("boom: write failed")
	mt.setWriteErr(writeErr)

	_, err := c.SendDataMessage(t.Context(), 1, 1, true, secs2.NewASCIIItem("PING"))
	require.Error(t, err)
	require.Equal(t, writeErr, err, "a live-generation write failure must be returned raw, unwrapped")
	require.False(t, errors.Is(err, ErrConnClosed))
	require.Equal(t, uint64(1), c.metrics.DataMsgErrCount())

	log.requireCount(t, "sub", 3) // NotConnected->NotSelected, NotSelected->Selected, Selected->NotConnected (TCPDown)
	got := log.filter("sub")
	require.Equal(t, SelectedState, got[2].prev)
	require.Equal(t, NotConnectedState, got[2].next)
	require.Equal(t, CauseIOError, got[2].cause)
}

// ── Reply-wait priority when several outcomes are ready at once ────────────────────────────
//
// Each test below drives the higher-priority outcome (a reply, or a teardown) from testHookWaitPicked,
// which fires AFTER Go's select has already committed to a lower-priority arm.
// This makes the arm Go picks deterministic —
// one iteration proves the arm's own re-check, rather than relying on many iterations to eventually land on it.

// TestSendTeardown_TimerArmThenTeardown_TeardownWins proves
// that a teardown beating the protocol-timer (T3/T6) arm to the select still wins.
// The wait returns ErrConnClosed, never ErrT3Timeout.
// It does not count a data-message error, and it never attempts the auto-S9F9 notification.
// T3=1ms and no sleep make the timer arm the only one ready when the select runs, so it is the arm Go's select picks.
//
// Teeth: removing the e.ctx.Err() re-check in the timer arm makes this fail (ErrT3Timeout instead of ErrConnClosed).
func TestSendTeardown_TimerArmThenTeardown_TeardownWins(t *testing.T) {
	c, _ := newTestSendConn(t, SelectedState, WithAutoS9F9(true))
	c.cfg.Load().timers.T3 = time.Millisecond

	var s9f9Attempted atomic.Bool
	c.testHookAutoS9F9 = func() { s9f9Attempted.Store(true) }

	e := c.cur.Load()

	var picked replyWaitArm
	c.testHookWaitPicked = func(arm replyWaitArm) {
		picked = arm
		e.teardown(2 * time.Second)
	}

	_, err := c.sendWaitReply(t.Context(), mustSendData(t, [4]byte{0, 0, 0, 1}, true))

	require.Equal(t, replyWaitArmTimer, picked, "T3=1ms with nothing else ready must pick the timer arm")
	require.ErrorIs(t, err, ErrConnClosed)
	require.Equal(t, uint64(0), c.metrics.DataMsgErrCount())
	require.False(t, s9f9Attempted.Load(), "teardown beating the protocol timer must never attempt auto-S9F9")
}

// TestSendTeardown_TimerArmThenReply_ReplyWins proves
// that a reply routed immediately after the select picks the timer arm still wins:
// that arm's tryReply re-check returns the reply (never ErrT3Timeout) and does not count a data-message error.
// T3=1ms with nothing else ready makes the timer arm the one Go's select picks.
//
// Teeth: removing the tryReply re-check in the timer arm makes this fail (ErrT3Timeout instead of the reply).
func TestSendTeardown_TimerArmThenReply_ReplyWins(t *testing.T) {
	c, _ := newTestSendConn(t, SelectedState)
	c.cfg.Load().timers.T3 = time.Millisecond
	sb := [4]byte{0, 0, 0, 2}

	var picked replyWaitArm
	c.testHookWaitPicked = func(arm replyWaitArm) {
		picked = arm
		require.True(t, c.RouteReply(mustSendReply(t, sb)))
	}

	reply, err := c.sendWaitReply(t.Context(), mustSendData(t, sb, true))

	require.Equal(t, replyWaitArmTimer, picked, "T3=1ms with nothing else ready must pick the timer arm")
	require.NoError(t, err)
	require.NotNil(t, reply)
	require.Equal(t, uint64(0), c.metrics.DataMsgErrCount())
}

// TestSendTeardown_TeardownArmThenReply_ReplyWins proves
// that a reply routed immediately after the select picks the teardown arm still wins:
// that arm's tryReply re-check returns the reply (never ErrConnClosed).
// testHookBeforeReplyWait tears the epoch down BEFORE the select runs, with T3 at its default
// (long) duration, so the teardown channel is the only one ready and is the arm Go's select picks.
//
// Teeth: removing the tryReply re-check in the teardown arm makes this fail (ErrConnClosed instead of the reply).
func TestSendTeardown_TeardownArmThenReply_ReplyWins(t *testing.T) {
	c, _ := newTestSendConn(t, SelectedState)
	sb := [4]byte{0, 0, 0, 3}
	e := c.cur.Load()

	c.testHookBeforeReplyWait = func() { e.teardown(2 * time.Second) }

	var picked replyWaitArm
	c.testHookWaitPicked = func(arm replyWaitArm) {
		picked = arm
		require.True(t, c.RouteReply(mustSendReply(t, sb)))
	}

	reply, err := c.sendWaitReply(t.Context(), mustSendData(t, sb, true))

	require.Equal(t, replyWaitArmTeardown, picked, "a teardown started before the select, with default T3, must pick the teardown arm")
	require.NoError(t, err)
	require.NotNil(t, reply)
}
