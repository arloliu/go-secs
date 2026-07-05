package hsms

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/logger/loggertest"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// newTestSendConn builds a connection engine wired to a fresh mockTransport, with a supervisor
// pinned to state and a live epoch published on c.cur — the minimum to exercise the Task-12
// send/reply engine in isolation (Open/recv loop are later tasks). State is set directly on the
// supervisor's lock-free atomic (white-box), which is what the send path reads via IsSelected().
func newTestSendConn(t *testing.T, state ConnState) (*connection, *mockTransport) {
	t.Helper()

	cfg := DefaultConnectionConfig()
	tr := &mockTransport{}

	conn, err := NewConnection(cfg, tr)
	require.NoError(t, err)

	c, ok := conn.(*connection)
	require.True(t, ok, "NewConnection must return the concrete *connection")

	sup := newSupervisor(func(_, _ ConnState) {}, &c.handlers)
	sup.state.Store(uint32(state))
	c.sup.Store(sup)

	e := newEpoch(t.Context(), cfg.logger, cfg.senderQueueSize)
	// A Selected connection always has a live epoch socket (TCPUp -> setConn); writeFrame binds the
	// write to it (I1 full-fix), so the harness must publish one. The mock records writes by frame
	// and by conn, so any non-nil conn suffices here.
	e.setConn(fakeConn{})
	c.cur.Store(e)

	return c, tr
}

// mustSendData builds a primary S1F1 data message with a non-empty ASCII body. waitBit selects
// the W-bit (reply-expected) flag; function 1 is odd, so a W-bit primary is valid.
func mustSendData(t *testing.T, sysBytes [4]byte, waitBit bool) *DataMessage {
	t.Helper()

	msg, err := NewDataMessage(1, 1, waitBit, 0xFFFF, sysBytes, secs2.NewASCIIItem("PING"))
	require.NoError(t, err)

	return msg
}

// mustSendReply builds an S1F2 secondary carrying the SAME system bytes as the primary — the key
// RouteReply correlates on (E37 §8.2.6.9). Function 2 is even, W-bit false.
func mustSendReply(t *testing.T, sysBytes [4]byte) *DataMessage {
	t.Helper()

	msg, err := NewDataMessage(1, 2, false, 0xFFFF, sysBytes, secs2.NewASCIIItem("PONG"))
	require.NoError(t, err)

	return msg
}

// TestSend_FrameBytesEqualToBytes proves writeFrame's writev output is byte-for-byte equal to
// Message.ToBytes() for both a data message (multi-slice prefix+body path) and a control message
// (single-slice header-only path) — §6.2.
func TestSend_FrameBytesEqualToBytes(t *testing.T) {
	c, tr := newTestSendConn(t, SelectedState)
	e := c.cur.Load()

	data := mustSendData(t, [4]byte{0x00, 0x01, 0x02, 0x03}, true)
	require.Positive(t, data.BodyLen(), "framing test needs a non-empty body to exercise the multi-slice path")
	require.NoError(t, c.writeFrame(t.Context(), e, data))

	ctrl := NewSelectReq(0xFFFF, [4]byte{0x0A, 0x0B, 0x0C, 0x0D})
	require.NoError(t, c.writeFrame(t.Context(), e, ctrl))

	frames := tr.writtenFrames()
	require.Len(t, frames, 2)
	require.Equal(t, data.ToBytes(), frames[0], "data frame must equal ToBytes (prefix ++ body.Buffers)")
	require.Equal(t, ctrl.ToBytes(), frames[1], "control frame must equal ToBytes (single-slice header-only)")
}

// TestSend_B1GatesDataWhenNotSelected proves the B1 gate refuses data on BOTH send entry points
// while !Selected — before any write (sync) and before any enqueue (async) — counting each drop
// exactly once at the B3 chokepoint.
func TestSend_B1GatesDataWhenNotSelected(t *testing.T) {
	c, tr := newTestSendConn(t, NotSelectedState)
	e := c.cur.Load()

	// Sync W-bit path: refused before any write.
	_, err := c.sendWaitReply(t.Context(), mustSendData(t, [4]byte{0, 0, 0, 1}, true))
	require.ErrorIs(t, err, ErrNotSelectedState)
	require.Equal(t, uint64(1), c.metrics.DataMsgDropNotSelectedCount(), "B3 chokepoint counts the sync drop")
	require.Equal(t, 0, tr.writeCount(), "gated sync data must NOT be written")

	// Async path: ALSO refused before enqueue (B1 binds BOTH entry points) — must ERROR.
	errAsync := c.SendAsync(t.Context(), mustSendData(t, [4]byte{0, 0, 0, 2}, false))
	require.ErrorIs(t, errAsync, ErrNotSelectedState, "async data must be refused before enqueue while NotSelected")
	require.Equal(t, uint64(2), c.metrics.DataMsgDropNotSelectedCount(), "B3 chokepoint counts the async drop too")
	require.Empty(t, e.sendCh, "refused async data must NOT be enqueued")
	require.Equal(t, 0, tr.writeCount(), "refused async data must NOT be written")
}

// TestSend_ControlNotGatedWhenNotSelected proves control messages (the Select handshake) flow
// while NotSelected — they never touch the data-drop chokepoint.
func TestSend_ControlNotGatedWhenNotSelected(t *testing.T) {
	c, _ := newTestSendConn(t, NotSelectedState)
	e := c.cur.Load()

	require.NoError(t, c.SendAsync(t.Context(), NewSelectReq(0xFFFF, [4]byte{0, 0, 0, 9})))
	require.Len(t, e.sendCh, 1, "control send must be enqueued while NotSelected")
	require.Equal(t, uint64(0), c.metrics.DataMsgDropNotSelectedCount(), "control sends never touch the data-drop chokepoint")
}

// TestSend_InflightBalancedAcrossAllTerminals proves the I1 gauge returns to 0 across ALL FOUR
// terminal outcomes of the W-bit wait: reply, T3 timeout, caller-ctx cancel, and connection drop.
func TestSend_InflightBalancedAcrossAllTerminals(t *testing.T) {
	t.Run("reply", func(t *testing.T) {
		c, _ := newTestSendConn(t, SelectedState)
		sb := [4]byte{0, 0, 0, 1}

		type result struct {
			msg Message
			err error
		}
		resCh := make(chan result, 1)
		go func() {
			m, err := c.sendWaitReply(t.Context(), mustSendData(t, sb, true))
			resCh <- result{m, err}
		}()

		// Inc happens exactly once AFTER the primary is on the wire.
		require.Eventually(t, func() bool {
			return c.metrics.DataMsgInflightCount() == 1
		}, 2*time.Second, time.Millisecond, "inflight must reach 1 after the W-bit primary is on the wire")

		require.True(t, c.RouteReply(mustSendReply(t, sb)), "reply must hit the sender-owned registry")

		got := <-resCh
		require.NoError(t, got.err)
		require.NotNil(t, got.msg)
		require.Equal(t, sb, got.msg.SystemBytes(), "the routed reply is returned to the sender")
		require.Equal(t, int64(0), c.metrics.DataMsgInflightCount(), "inflight back to 0 on reply")
	})

	t.Run("t3-timeout", func(t *testing.T) {
		c, _ := newTestSendConn(t, SelectedState)
		c.cfg.Load().timers.T3 = 30 * time.Millisecond

		_, err := c.sendWaitReply(t.Context(), mustSendData(t, [4]byte{0, 0, 0, 2}, true))
		require.ErrorIs(t, err, ErrT3Timeout)
		require.Equal(t, int64(0), c.metrics.DataMsgInflightCount(), "inflight back to 0 on T3 timeout")
	})

	t.Run("ctx-cancel", func(t *testing.T) {
		c, _ := newTestSendConn(t, SelectedState)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		errCh := make(chan error, 1)
		go func() {
			_, err := c.sendWaitReply(ctx, mustSendData(t, [4]byte{0, 0, 0, 3}, true))
			errCh <- err
		}()

		require.Eventually(t, func() bool {
			return c.metrics.DataMsgInflightCount() == 1
		}, 2*time.Second, time.Millisecond)

		cancel()
		require.ErrorIs(t, <-errCh, context.Canceled)
		require.Equal(t, int64(0), c.metrics.DataMsgInflightCount(), "inflight back to 0 on ctx cancel")
	})

	t.Run("conn-drop", func(t *testing.T) {
		c, _ := newTestSendConn(t, SelectedState)
		e := c.cur.Load()

		errCh := make(chan error, 1)
		go func() {
			_, err := c.sendWaitReply(t.Context(), mustSendData(t, [4]byte{0, 0, 0, 4}, true))
			errCh <- err
		}()

		require.Eventually(t, func() bool {
			return c.metrics.DataMsgInflightCount() == 1
		}, 2*time.Second, time.Millisecond)

		e.teardown(2 * time.Second) // cancels the generation ctx (the conn-drop terminal)
		require.ErrorIs(t, <-errCh, ErrConnClosed)
		require.Equal(t, int64(0), c.metrics.DataMsgInflightCount(), "inflight back to 0 on connection drop")
	})
}

// TestSend_ControlUsesT6 proves a control W-bit transaction times out on T6 (not T3) and never
// touches the data inflight gauge.
func TestSend_ControlUsesT6(t *testing.T) {
	c, _ := newTestSendConn(t, SelectedState)
	c.cfg.Load().timers.T6 = 30 * time.Millisecond

	_, err := c.sendWaitReply(t.Context(), NewSelectReq(0xFFFF, [4]byte{0, 0, 0, 5}))
	require.ErrorIs(t, err, ErrT6Timeout, "control transaction times out on T6")
	require.Equal(t, int64(0), c.metrics.DataMsgInflightCount(), "control transactions never touch the data gauge")
}

// TestSend_T6TimeoutPostOnWire is the Task-24 E3 T6 verification. A control (W-bit) transaction
// whose reply never arrives returns hsms.ErrT6Timeout (the control-class timer, NOT T3). The T6
// clock is measured from AFTER the primary is on the wire: sendWaitReply arms pool.GetTimer(T6)
// only AFTER writeFrame returns (§9.4.1.2), so the post-on-wire ordering is structurally guaranteed
// — the primary MUST have reached the transport before the timeout could fire.
func TestSend_T6TimeoutPostOnWire(t *testing.T) {
	c, tr := newTestSendConn(t, SelectedState)
	c.cfg.Load().timers.T6 = 40 * time.Millisecond

	_, err := c.sendWaitReply(t.Context(), NewSelectReq(0xFFFF, [4]byte{0, 0, 0, 1}))
	require.ErrorIs(t, err, ErrT6Timeout, "a control transaction must time out on T6, not T3")
	require.GreaterOrEqual(t, tr.writeCount(), 1,
		"the primary must be on the wire BEFORE the T6 clock is armed (post-on-wire, §9.4.1.2)")
	require.Equal(t, int64(0), c.metrics.DataMsgInflightCount(),
		"control transactions never touch the data inflight gauge")
}

// TestSend_T3TimeoutPostOnWire is the Task-24 E3 T3 verification. A W-bit DATA transaction whose
// reply never arrives returns hsms.ErrT3Timeout (the data-class timer, NOT T6), measured from
// AFTER the primary is on the wire (sendWaitReply arms pool.GetTimer(T3) only after writeFrame
// returns, §9.4.1.2 — the same structural post-on-wire guarantee as T6 above).
func TestSend_T3TimeoutPostOnWire(t *testing.T) {
	c, tr := newTestSendConn(t, SelectedState)
	c.cfg.Load().timers.T3 = 40 * time.Millisecond

	_, err := c.sendWaitReply(t.Context(), mustSendData(t, [4]byte{0, 0, 0, 1}, true))
	require.ErrorIs(t, err, ErrT3Timeout, "a W-bit data transaction must time out on T3, not T6")
	require.GreaterOrEqual(t, tr.writeCount(), 1,
		"the primary must be on the wire BEFORE the T3 clock is armed (post-on-wire, §9.4.1.2)")
	require.Equal(t, int64(0), c.metrics.DataMsgInflightCount(), "inflight back to 0 on T3 timeout")
}

// TestSend_CtxCancelDoesNotRejectPeer proves the ctx-cancel contract (D5a-7): cancelling the
// wait returns ctx.Err() but NEVER emits a Reject/Separate to the peer — a local give-up must
// not be misrepresented as a peer protocol error.
func TestSend_CtxCancelDoesNotRejectPeer(t *testing.T) {
	c, tr := newTestSendConn(t, SelectedState)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		_, err := c.sendWaitReply(ctx, mustSendData(t, [4]byte{0, 0, 0, 7}, true))
		errCh <- err
	}()

	require.Eventually(t, func() bool {
		return c.metrics.DataMsgInflightCount() == 1
	}, 2*time.Second, time.Millisecond)

	cancel()
	require.ErrorIs(t, <-errCh, context.Canceled)

	require.GreaterOrEqual(t, tr.writeCount(), 1, "the primary must have been written before the wait")
	require.Equal(t, 0, tr.controlWrites(RejectReqType), "must NOT Reject the peer on local ctx cancel")
	require.Equal(t, 0, tr.controlWrites(SeparateReqType), "must NOT Separate the peer on local ctx cancel")
}

// TestSend_B2WriteBoundaryNotSelected proves the B2 write-boundary re-check: a data message
// that passes the B1 pre-register gate but finds the link NotSelected at the write boundary
// (under writeMu) yields a counted, non-fatal ErrNotSelectedState — not a raw transport error
// and not a teardown (spec §5.5).
//
// Teeth-check rationale: without the B2 re-check, writeFrame calls c.tr.Write after the hook
// flips state to NotSelected, the mock write succeeds, sendWaitReply enters the reply-wait
// select, and the test returns ErrT3Timeout — not ErrNotSelectedState. The assertion bites.
func TestSend_B2WriteBoundaryNotSelected(t *testing.T) {
	c, tr := newTestSendConn(t, SelectedState)

	// Install the write-boundary seam: after writeMu is acquired (but before the B2
	// re-check fires) flip the supervisor to NotSelected, simulating the race that B2 must
	// catch. This is the only window that cannot be exercised without a hook, because B1
	// and the write-lock acquisition are not observable from outside the goroutine.
	c.testHookAfterWriteLock = func() {
		if s := c.sup.Load(); s != nil {
			s.state.Store(uint32(NotSelectedState))
		}
	}

	_, err := c.sendWaitReply(t.Context(), mustSendData(t, [4]byte{0, 0, 0, 1}, true))
	require.ErrorIs(t, err, ErrNotSelectedState,
		"B2 write-boundary re-check must return ErrNotSelectedState, not a raw transport error or T3 timeout")
	require.Equal(t, uint64(1), c.metrics.DataMsgDropNotSelectedCount(),
		"B2 drop must be counted exactly once at the B3 chokepoint")
	require.Equal(t, 0, tr.writeCount(),
		"B2-gated frame must NOT reach the transport — write was aborted before c.tr.Write")
	// Non-fatal: the connection epoch is intact, no teardown occurred.
	require.NotNil(t, c.cur.Load(),
		"B2 drop is non-fatal — epoch must remain intact, no teardown")
}

// TestSend_I1StaleEpochWriteGuard proves the I1 fast-fail early-out: a synchronous sender that stalls
// across its generation's teardown — e.ctx cancelled AFTER it acquired writeMu — fails closed without
// reaching the wire. This is the belt-and-suspenders guard; TestSend_I1WriteBindsToPinnedEpochConn
// proves the PRIMARY fix (the write is bound to the pinned epoch's conn, so it can never hit a
// successor's socket even if this ctx guard is passed). The seam cancels the pinned epoch's ctx while
// writeMu is held, exactly that window.
//
// Teeth-check rationale: without the fast-fail guard, writeFrame reaches c.tr.Write (writeCount==1) and
// only then does sendWaitReply's select observe e.ctx.Done — so the RETURNED error is ErrConnClosed
// either way; the discriminating assertion is writeCount==0, which holds only when the guard aborts first.
func TestSend_I1StaleEpochWriteGuard(t *testing.T) {
	c, tr := newTestSendConn(t, SelectedState)
	e := c.cur.Load()

	c.testHookAfterWriteLock = func() {
		e.cancel() // simulate THIS generation being torn down after writeMu was acquired
	}

	_, err := c.sendWaitReply(t.Context(), mustSendData(t, [4]byte{0, 0, 0, 1}, true))
	require.ErrorIs(t, err, ErrConnClosed,
		"a send pinned to a torn-down epoch must fail closed")
	require.Equal(t, 0, tr.writeCount(),
		"the stale-epoch frame must NOT reach the transport — write aborted before c.tr.Write")
}

// TestSend_I1WriteBindsToPinnedEpochConn proves the I1 full-fix (the airtight cancel-after-guard case
// Codex flagged): writeFrame binds the write to the conn it captured from the PINNED epoch up front,
// so even if a full teardown+reconnect completes while the stale sender holds writeMu — publishing a
// SUCCESSOR generation with a DIFFERENT socket — the write still targets the pinned epoch's own conn,
// NEVER the successor's. This is the property that closes the residual TOCTOU: the old dynamic
// transport t.conn would have resolved the successor's live socket and interleaved a frame outside its
// writeMu.
//
// The seam advances c.cur to a fresh successor epoch e2 (socket c2) AFTER writeFrame captured e1's
// conn (c1) but before the write. e1.ctx is left ALIVE so the write proceeds (the fast-fail guard is
// deliberately not taken here — this exercises the guard-passed window). The mock records the conn of
// every Write, so the discriminating assertion is that the single write targeted c1 and never c2.
//
// Teeth-check rationale: revert writeFrame to write c.cur.Load()'s (or the transport's current) conn
// instead of the captured epoch's conn and the write would land on c2 → require.Same(c1) bites.
func TestSend_I1WriteBindsToPinnedEpochConn(t *testing.T) {
	c, tr := newTestSendConn(t, SelectedState)
	e1 := c.cur.Load()

	// Distinct socket pointers so the mock can tell which generation a write hit. fakeConn is a
	// zero-size struct, so &fakeConn{} pointers may share the runtime.zerobase address — idConn's
	// non-empty field guarantees each generation's conn has its own identity.
	c1 := &idConn{id: 1}
	c2 := &idConn{id: 2}
	e1.setConn(c1)

	cfg := DefaultConnectionConfig()
	e2 := newEpoch(t.Context(), cfg.logger, cfg.senderQueueSize)
	e2.setConn(c2)

	// Under writeMu, after writeFrame captured e1's conn, simulate the teardown+reconnect completing:
	// the successor generation e2 (socket c2) becomes current. e1.ctx stays alive so the write runs.
	c.testHookAfterWriteLock = func() {
		c.cur.Store(e2)
	}

	// Fire-and-forget (!W) so the send returns as soon as the frame is on the wire (no reply wait).
	_, err := c.sendWaitReply(t.Context(), mustSendData(t, [4]byte{0, 0, 0, 9}, false))
	require.NoError(t, err)

	conns := tr.writeConnsUsed()
	require.Len(t, conns, 1, "exactly one frame written")
	require.Same(t, c1, conns[0],
		"the stale sender must write the PINNED epoch's captured conn (c1)")
	require.NotSame(t, c2, conns[0],
		"it must NEVER write the successor generation's socket (c2) — the TOCTOU is closed")

	// The bounded-write deadline (I5/CRIT-1) must be armed on the SAME pinned epoch conn the write
	// targets — never the successor's — so a wedged-peer deadline can't be armed on the wrong socket.
	require.Same(t, c1, tr.deadlineConnUsed(),
		"the write deadline must be armed on the pinned epoch's conn (c1)")
	require.NotSame(t, c2, tr.deadlineConnUsed(),
		"the write deadline must NEVER be armed on the successor generation's socket (c2)")
}

// TestSend_WriteIsDeadlineBounded proves the I5/CRIT-1 arming: writeFrame arms a write deadline
// (~now+writeTimeout, default 30s) BEFORE the writev, so a wedged/zero-window peer cannot stall the
// writev indefinitely. The mock captures the deadline live at the instant of Write (writeFrame's
// defer clears it afterward, so it must be observed at write time).
func TestSend_WriteIsDeadlineBounded(t *testing.T) {
	c, tr := newTestSendConn(t, SelectedState)

	before := time.Now()
	// Fire-and-forget (!W) so the send returns right after the on-wire write (no reply wait).
	_, err := c.sendWaitReply(t.Context(), mustSendData(t, [4]byte{0, 0, 0, 1}, false))
	require.NoError(t, err)

	d := tr.writeDeadlineAtLastWrite()
	require.False(t, d.IsZero(), "writeFrame must arm a write deadline before the writev")
	require.WithinRange(t, d, before.Add(25*time.Second), before.Add(35*time.Second),
		"the write deadline must be ~now+writeTimeout (default 30s)")
}

// TestSend_WriteFailureTearsDownGeneration proves the I5/CRIT-1 recovery: a transport write failure —
// which a write-deadline timeout on a wedged peer produces — is an involuntary drop that must tear
// the generation down (so the reconnect loop re-establishes it), not merely return the error over a
// possibly-partial frame. TCPDown sets commsFailure synchronously, asserted directly here.
func TestSend_WriteFailureTearsDownGeneration(t *testing.T) {
	c, tr := newTestSendConn(t, SelectedState)
	tr.writeErr = errors.New("simulated wedged-peer write failure")

	e := c.cur.Load()
	_, err := c.sendWaitReply(t.Context(), mustSendData(t, [4]byte{0, 0, 0, 1}, true))
	require.Error(t, err, "a transport write failure must propagate to the caller")
	require.True(t, e.commsFailure.Load(),
		"a write failure must drive TCPDown (involuntary drop) so the wedged generation is torn down")
}

// TestSend_DrainSendChWritesQueuedFrames proves the async sender goroutine drains epoch.sendCh
// and writes each queued frame (the fire-and-forget path), and exits on generation-ctx cancel.
func TestSend_DrainSendChWritesQueuedFrames(t *testing.T) {
	c, tr := newTestSendConn(t, SelectedState)
	e := c.cur.Load()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	msgs := []*DataMessage{
		mustSendData(t, [4]byte{0, 0, 0, 1}, false),
		mustSendData(t, [4]byte{0, 0, 0, 2}, false),
		mustSendData(t, [4]byte{0, 0, 0, 3}, false),
	}
	for _, m := range msgs {
		require.NoError(t, c.SendAsync(ctx, m))
	}

	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		c.drainSendCh(ctx, e)
	}()

	require.Eventually(t, func() bool {
		return tr.writeCount() == len(msgs)
	}, 2*time.Second, time.Millisecond, "drainSendCh must write every queued frame")

	frames := tr.writtenFrames()
	for i, m := range msgs {
		require.Equal(t, m.ToBytes(), frames[i], "queued async frame must be written byte-for-byte")
	}

	cancel()
	select {
	case <-drainDone:
	case <-time.After(2 * time.Second):
		t.Fatal("drainSendCh must return when the generation ctx is cancelled")
	}
}

// TestSend_InboundReject_SurfacesRejectError proves Fix A: a peer Reject.req correlated by System
// Bytes to a pending W-bit send is delivered to the waiter as a *RejectError carrying the E37
// reason code — NOT as a message — while a legitimate control response (Linktest.rsp) still routes
// as the reply message (no regression).
func TestSend_InboundReject_SurfacesRejectError(t *testing.T) {
	t.Run("data-reject-becomes-error", func(t *testing.T) {
		c, _ := newTestSendConn(t, SelectedState)
		sb := [4]byte{0, 0, 0, 1}

		done := make(chan error, 1)
		go func() {
			_, err := c.sendWaitReply(t.Context(), mustSendData(t, sb, true))
			done <- err
		}()

		// Wait until the W-bit primary is on the wire and its reply channel is registered.
		require.Eventually(t, func() bool { return c.metrics.DataMsgInflightCount() == 1 }, 2*time.Second, time.Millisecond)

		// The peer answers with a Reject.req(reason 4) correlated by System Bytes to the send.
		require.True(t, c.RouteReply(NewRejectReqRaw(0xFFFF, 0, 0, sb, RejectNotSelected)),
			"the Reject.req must correlate to the waiting sender")

		var re *RejectError
		require.ErrorAs(t, <-done, &re, "an inbound Reject.req must surface as a *RejectError")
		require.Equal(t, byte(RejectNotSelected), re.Reason, "the RejectError must carry the E37 reject reason")
	})

	t.Run("linktest-rsp-still-delivers-as-message", func(t *testing.T) {
		c, tr := newTestSendConn(t, SelectedState)
		sb := [4]byte{0, 0, 0, 2}
		req := NewLinktestReq(sb)

		type outcome struct {
			msg Message
			err error
		}
		done := make(chan outcome, 1)
		go func() {
			m, err := c.sendWaitReply(t.Context(), req)
			done <- outcome{msg: m, err: err}
		}()

		require.Eventually(t, func() bool { return tr.writeCount() == 1 }, 2*time.Second, time.Millisecond,
			"the Linktest.req (a stand-in control primary) must reach the wire before the reply is routed")

		rsp, err := NewLinktestRsp(req)
		require.NoError(t, err)
		require.True(t, c.RouteReply(rsp), "the Linktest.rsp must correlate to the waiting initiator")

		got := <-done
		require.NoError(t, got.err, "a legitimate control rsp must deliver as the reply message, not an error")
		require.NotNil(t, got.msg)
		require.Equal(t, LinktestRspType, got.msg.Type(), "the routed control reply must be the Linktest.rsp")
	})
}

// TestSend_FireAndForget_ShortCircuits proves Fix B: a DATA send with the W-bit CLEAR returns
// (nil, nil) the instant the frame is on the wire — it registers NO reply waiter, arms no T3
// timer, and never touches the inflight gauge. A short T3 is pinned so a regression (registering
// and waiting) would surface as a fast ErrT3Timeout rather than a full-length hang.
func TestSend_FireAndForget_ShortCircuits(t *testing.T) {
	c, _ := newTestSendConn(t, SelectedState)
	c.cfg.Load().timers.T3 = 50 * time.Millisecond
	e := c.cur.Load()

	reply, err := c.sendWaitReply(t.Context(), mustSendData(t, [4]byte{0, 0, 0, 1}, false))
	require.NoError(t, err, "a !W data send short-circuits with (nil, nil), never ErrT3Timeout")
	require.Nil(t, reply, "a fire-and-forget send has no reply")
	require.Equal(t, 0, e.replies.len(), "a fire-and-forget send must not register a reply waiter")
	require.Equal(t, uint64(1), c.metrics.DataMsgSendCount(), "the !W frame reached the wire")
	require.Equal(t, int64(0), c.metrics.DataMsgInflightCount(), "a !W send never touches the inflight gauge")
}

// TestSendWaitReply_AutoS9F9_EnabledOnTimeout proves that with WithAutoS9F9 enabled, a data
// message's T3 timeout enqueues an S9F9 notification (SEMI E5 §10.13) carrying the timed-out
// message's own header as SHEAD, sent from this connection's own SessionID.
func TestSendWaitReply_AutoS9F9_EnabledOnTimeout(t *testing.T) {
	c, _ := newTestSendConn(t, SelectedState)
	c.cfg.Load().timers.T3 = 30 * time.Millisecond
	c.cfg.Load().autoS9F9 = true

	primary := mustSendData(t, [4]byte{0, 0, 0, 9}, true)
	_, err := c.sendWaitReply(t.Context(), primary)
	require.ErrorIs(t, err, ErrT3Timeout)

	e := c.cur.Load()
	require.Len(t, e.sendCh, 1, "a T3 timeout with AutoS9F9 enabled must enqueue exactly one S9F9")
	req := <-e.sendCh
	reply, ok := req.msg.(*DataMessage)
	require.True(t, ok, "the auto-notification is a DataMessage")
	require.Equal(t, uint8(9), reply.Stream())
	require.Equal(t, uint8(9), reply.Function())
	require.Equal(t, c.SessionID(), reply.SessionID(), "S9F9 is sent from this connection's own SessionID")

	item, err := reply.Item()
	require.NoError(t, err)
	body, err := item.ToBinary()
	require.NoError(t, err)
	shead := primary.HeaderBytes()
	require.Equal(t, shead[:], body, "S9F9 body must equal the timed-out message's 10-byte SHEAD")
}

// TestSendWaitReply_AutoS9F9_DisabledByDefault proves the default (AutoS9F9 not enabled) sends
// nothing extra on a T3 timeout — v2's pre-existing shipped behavior is unchanged.
func TestSendWaitReply_AutoS9F9_DisabledByDefault(t *testing.T) {
	c, _ := newTestSendConn(t, SelectedState)
	c.cfg.Load().timers.T3 = 30 * time.Millisecond

	_, err := c.sendWaitReply(t.Context(), mustSendData(t, [4]byte{0, 0, 0, 10}, true))
	require.ErrorIs(t, err, ErrT3Timeout)
	require.Empty(t, c.cur.Load().sendCh, "AutoS9F9 disabled must never enqueue an S9F9")
}

// TestSendWaitReply_AutoS9F9_ControlTimeoutNeverSends proves a CONTROL (T6) timeout never sends
// S9F9 even with AutoS9F9 enabled — S9F9 is a SECS-II (data-message) notification only (matches
// v1's isDataMsg-gated behavior).
func TestSendWaitReply_AutoS9F9_ControlTimeoutNeverSends(t *testing.T) {
	c, _ := newTestSendConn(t, SelectedState)
	c.cfg.Load().timers.T6 = 30 * time.Millisecond
	c.cfg.Load().autoS9F9 = true

	_, err := c.sendWaitReply(t.Context(), NewSelectReq(0xFFFF, [4]byte{0, 0, 0, 11}))
	require.ErrorIs(t, err, ErrT6Timeout)
	require.Empty(t, c.cur.Load().sendCh, "a control T6 timeout must never enqueue an S9F9")
}

// TestWriteFrame_TraceTraffic_LogsSentFrame proves that with WithTraceTraffic enabled, a
// successful writeFrame logs a Debug line carrying the frame's hex dump. Uses loggertest.MockLogger
// (the package's existing testify-mock Logger double, already used by hsms/supervisor_test.go).
func TestWriteFrame_TraceTraffic_LogsSentFrame(t *testing.T) {
	c, _ := newTestSendConn(t, SelectedState)
	mockLog := loggertest.NewMockLogger()
	mockLog.On("Debug", mock.Anything, mock.Anything).Return()
	c.cfg.Load().logger = mockLog
	c.cfg.Load().traceTraffic = true

	msg := mustSendData(t, [4]byte{0, 0, 0, 20}, false)
	require.NoError(t, c.writeFrame(t.Context(), c.cur.Load(), msg))

	require.Len(t, mockLog.Calls, 1)
	call := mockLog.Calls[0]
	require.Equal(t, "Debug", call.Method)
	keyvals, ok := call.Arguments[1].([]any)
	require.True(t, ok, "Debug's variadic keysAndValues must be forwarded as a []any slice")
	require.Contains(t, keyvals, hexDump(msg.ToBytes()),
		"the logged payload must carry the exact frame hex dump")
}
