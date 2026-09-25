package hsms

import (
	"errors"
	"sync/atomic"
	"testing"

	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// ── inbound generation-fence test harness ───────────────────────────────────

// newTwoGenConn builds a connection engine with a Selected supervisor and ONE live, published
// generation (genN) plus a second, unpublished generation (genSucc) ready to become the
// successor.
// This mirrors the "reconnect" shape the inbound-fence tests below need — a stale generation whose
// ended latch can be set independently of whether/when genSucc is published on c.cur — WITHOUT
// driving a full mock-transport dial/select cycle.
// It follows the same white-box construction newTestSendConn uses for the send-path tests
// (connection_send_test.go).
//
// Neither epoch's socket/goroutines are ever spawned; every test below calls the inbound entry
// points directly on the calling goroutine (or, for the seam tests, one explicitly-synchronized
// goroutine), so there is nothing to join.
func newTwoGenConn(t *testing.T) (c *connection, genN, genSucc *epoch) {
	t.Helper()

	cfg := DefaultConnectionConfig()
	tr := &mockTransport{}

	conn, err := NewConnection(cfg, tr)
	require.NoError(t, err)

	cc, ok := conn.(*connection)
	require.True(t, ok, "NewConnection must return the concrete *connection")

	sup := newSupervisor(func(_, _ ConnState) {}, &cc.handlers, &cc.lifecycleSubs)
	sup.state.Store(uint32(SelectedState))
	cc.sup.Store(sup)

	genN = newEpoch(t.Context(), cfg.logger, cfg.senderQueueSize)
	genN.genGate = &cc.genGate
	genN.id = cc.genSeq.Add(1)
	genN.setConn(fakeConn{})
	cc.cur.Store(genN)

	genSucc = newEpoch(t.Context(), cfg.logger, cfg.senderQueueSize)
	genSucc.genGate = &cc.genGate
	genSucc.id = cc.genSeq.Add(1)
	genSucc.setConn(fakeConn{})

	t.Cleanup(func() {
		genN.cancel()
		genSucc.cancel()
	})

	return cc, genN, genSucc
}

// endAndSucceed ends genN (latches ended, cancels its ctx) and publishes genSucc as the new
// current generation — the two effects a real teardown+reconnect produces, in the order production
// requires: ended latches before any successor becomes observable.
func endAndSucceed(c *connection, genN, genSucc *epoch) {
	genN.markEnded()
	genN.cancel()
	c.cur.Store(genSucc)
}

// ── Test 1: DeliverOwnedFrameFromGeneration / RouteReplyFromGeneration identity ──────────────

// TestDeliverOwnedFrameFromGeneration_StaleGenDropsNoHandlerNoChannel covers a delivery reported
// against a generation whose teardown has already latched ended: liveEpoch(gen) must refuse
// admission, so the frame never reaches a handler or a registered channel.
func TestDeliverOwnedFrameFromGeneration_StaleGenDropsNoHandlerNoChannel(t *testing.T) {
	t.Parallel()

	c, genN, genSucc := newTwoGenConn(t)
	endAndSucceed(c, genN, genSucc) // genN is now over; genSucc is the live generation

	var handlerCalled atomic.Bool
	c.AddDataMessageHandler(func(_ *DataMessage, _ SECS2Endpoint) { handlerCalled.Store(true) })
	ch := make(chan *DataMessage, 1)
	c.AddDataMessageChan(ch)

	primary := mustSendData(t, [4]byte{0, 0, 0, 41}, false)
	err := c.DeliverOwnedFrameFromGeneration(genN.id, ownedFrame(t, primary))
	require.NoError(t, err, "a stale-generation delivery is dropped, not an error")

	require.False(t, handlerCalled.Load(), "a frame admitted for an ended generation must not reach a data handler")
	select {
	case <-ch:
		t.Fatal("a frame admitted for an ended generation must not reach a registered channel")
	default:
	}
	require.Equal(t, uint64(1), c.staleRecv.Load(), "the dropped delivery must be counted")
}

// TestDeliverOwnedFrameFromGeneration_LiveGenDelivers covers the live-generation case: admission
// and fan-out both complete before any teardown starts, so the frame reaches the data handler.
func TestDeliverOwnedFrameFromGeneration_LiveGenDelivers(t *testing.T) {
	t.Parallel()

	c, genN, _ := newTwoGenConn(t)

	handlerCh := make(chan *DataMessage, 1)
	c.AddDataMessageHandler(func(msg *DataMessage, _ SECS2Endpoint) { handlerCh <- msg })

	primary := mustSendData(t, [4]byte{0, 0, 0, 42}, false)
	require.NoError(t, c.DeliverOwnedFrameFromGeneration(genN.id, ownedFrame(t, primary)))

	select {
	case got := <-handlerCh:
		require.Equal(t, primary.SystemBytes(), got.SystemBytes())
	default:
		t.Fatal("a live generation's own delivery must reach the data handler")
	}
	require.Zero(t, c.staleRecv.Load())
}

// TestDeliverOwnedFrameFromGeneration_Gen0TakesPlainPath proves gen 0 (secs1, or any
// out-of-module transport) is unaffected: it takes the plain DeliverOwnedFrame path unchanged.
func TestDeliverOwnedFrameFromGeneration_Gen0TakesPlainPath(t *testing.T) {
	t.Parallel()

	c, _, _ := newTwoGenConn(t)

	handlerCh := make(chan *DataMessage, 1)
	c.AddDataMessageHandler(func(msg *DataMessage, _ SECS2Endpoint) { handlerCh <- msg })

	primary := mustSendData(t, [4]byte{0, 0, 0, 43}, false)
	require.NoError(t, c.DeliverOwnedFrameFromGeneration(0, ownedFrame(t, primary)))

	select {
	case <-handlerCh:
	default:
		t.Fatal("gen 0 must take the plain DeliverOwnedFrame path")
	}
}

// ── Test 2: admission cutoff ──────────────────────────────────────────────────────────────────

// TestDeliverOwnedFrame_AdmissionCutoff_CancelledBetweenAdmissionAndFanout covers a frame admitted
// while genN was still live, then paused (via testHookBeforeFanout) between admission and fan-out;
// genN ends and genSucc is published while it is paused.
//
// Contract: the fan-out must stay bound to genN (never deliver on genSucc's live link) once genN's
// own end is observed.
func TestDeliverOwnedFrame_AdmissionCutoff_CancelledBetweenAdmissionAndFanout(t *testing.T) {
	t.Parallel()

	c, genN, genSucc := newTwoGenConn(t)

	b := newBlocker(t)
	c.testHookBeforeFanout = b.block

	var handlerCalled atomic.Bool
	c.AddDataMessageHandler(func(_ *DataMessage, _ SECS2Endpoint) { handlerCalled.Store(true) })

	primary := mustSendData(t, [4]byte{0, 0, 0, 44}, false)

	done := make(chan error, 1)
	go func() { done <- c.DeliverOwnedFrameFromGeneration(genN.id, ownedFrame(t, primary)) }()

	awaitSignal(t, b.entered, "testHookBeforeFanout was never entered")

	// Teardown starts and the successor is published WHILE the frame sits paused between
	// admission and fan-out — the frame stays bound to genN and must not reach genSucc.
	endAndSucceed(c, genN, genSucc)
	b.unblock()

	err := recvWithin(t, done, "DeliverOwnedFrameFromGeneration")
	require.NoError(t, err)

	require.False(t, handlerCalled.Load(),
		"a frame paused between admission and fan-out must be skipped once its OWN generation ends, never delivered on the successor")
}

// TestDeliverOwnedFrame_AdmissionCutoff_EndedWithNoSuccessor covers admission attempted for a
// generation whose ended latch is ALREADY set, with no successor published yet (c.cur still
// points at genN itself — the window between a drop and reconnect).
//
// With genN merely marked ended (its ctx not cancelled), c.Done() still reads genN's own open ctx,
// so a fan-out fast-exit on Done() would not by itself catch this case:
// staleRecv is the assertion that only liveEpoch(gen)'s {id, not ended} check can satisfy.
func TestDeliverOwnedFrame_AdmissionCutoff_EndedWithNoSuccessor(t *testing.T) {
	t.Parallel()

	c, genN, _ := newTwoGenConn(t)
	genN.markEnded() // teardown started; no successor published yet

	primary := mustSendData(t, [4]byte{0, 0, 0, 45}, false)
	err := c.DeliverOwnedFrameFromGeneration(genN.id, ownedFrame(t, primary))
	require.NoError(t, err)

	require.Equal(t, uint64(1), c.staleRecv.Load(),
		"an admission naming an already-ended generation must be rejected and counted, even with no successor yet published")
}

// ── Test 3: RouteReplyFromGeneration must not steal a successor's transaction ────────────────

// TestRouteReplyFromGeneration_StaleGenDoesNotStealSuccessorTransaction_DataSecondary covers a
// stale-generation data-secondary reply that collides, by System Bytes, with the successor's own
// open transaction: RouteReplyFromGeneration must bind to genN via liveEpoch and never deliver
// into genSucc's registration.
func TestRouteReplyFromGeneration_StaleGenDoesNotStealSuccessorTransaction_DataSecondary(t *testing.T) {
	t.Parallel()

	c, genN, genSucc := newTwoGenConn(t)
	endAndSucceed(c, genN, genSucc)

	sb := [4]byte{0, 0, 0, 46}
	replyCh := genSucc.replies.register(sb, 1, 1, true) // successor's own open S1F1 transaction
	defer genSucc.replies.deregister(sb)

	stale := mustSendReply(t, sb) // S1F2 secondary carrying the SAME System Bytes, read on genN
	delivered := c.RouteReplyFromGeneration(genN.id, stale)
	require.True(t, delivered, "a stale-generation reply is reported delivered/dropped, not a miss")

	select {
	case <-replyCh:
		t.Fatal("a reply read by an ended generation must not complete the successor's open transaction")
	default:
	}
	require.Equal(t, uint64(1), c.staleRecv.Load(), "the dropped reply must be counted")
}

// TestRouteReplyFromGeneration_StaleGenDoesNotStealSuccessorTransaction_ControlResponse is the
// same collision as the data-secondary row above, for a control response (Linktest.rsp).
func TestRouteReplyFromGeneration_StaleGenDoesNotStealSuccessorTransaction_ControlResponse(t *testing.T) {
	t.Parallel()

	c, genN, genSucc := newTwoGenConn(t)
	endAndSucceed(c, genN, genSucc)

	req := NewLinktestReq([4]byte{0, 0, 0, 47})
	replyCh := genSucc.replies.register(req.SystemBytes(), 0, 0, false) // control registration

	rsp, err := NewLinktestRsp(req)
	require.NoError(t, err)

	delivered := c.RouteReplyFromGeneration(genN.id, rsp)
	require.True(t, delivered)

	select {
	case <-replyCh:
		t.Fatal("a control response read by an ended generation must not complete the successor's open Linktest transaction")
	default:
	}
	require.Equal(t, uint64(1), c.staleRecv.Load())
}

// TestRouteReplyFromGeneration_StaleGenDoesNotStealSuccessorTransaction_RejectReq is the same
// collision as the data-secondary row above, for a peer Reject.req.
func TestRouteReplyFromGeneration_StaleGenDoesNotStealSuccessorTransaction_RejectReq(t *testing.T) {
	t.Parallel()

	c, genN, genSucc := newTwoGenConn(t)
	endAndSucceed(c, genN, genSucc)

	sb := [4]byte{0, 0, 0, 48}
	replyCh := genSucc.replies.register(sb, 1, 1, true)
	defer genSucc.replies.deregister(sb)

	primary := mustSendData(t, sb, true)
	reject, err := NewRejectReq(primary, 3)
	require.NoError(t, err)

	delivered := c.RouteReplyFromGeneration(genN.id, reject)
	require.True(t, delivered)

	select {
	case res := <-replyCh:
		t.Fatalf("a Reject.req read by an ended generation must not reach the successor's waiter, got %+v", res)
	default:
	}
	require.Equal(t, uint64(1), c.staleRecv.Load())
}

// TestRouteReplyFromGeneration_LiveGenerationCompletesTransaction covers the positive case: a
// reply read by its OWN still-live generation must complete that generation's open transaction.
func TestRouteReplyFromGeneration_LiveGenerationCompletesTransaction(t *testing.T) {
	t.Parallel()

	c, genN, _ := newTwoGenConn(t)

	sb := [4]byte{0, 0, 0, 52}
	replyCh := genN.replies.register(sb, 1, 1, true)
	defer genN.replies.deregister(sb)

	reply := mustSendReply(t, sb)
	delivered := c.RouteReplyFromGeneration(genN.id, reply)
	require.True(t, delivered, "a reply read by its own live generation must be reported delivered")

	select {
	case res := <-replyCh:
		require.NoError(t, res.err)
		require.Equal(t, reply.SystemBytes(), res.msg.SystemBytes())
	default:
		t.Fatal("a live generation's own reply must complete its own open transaction")
	}
	require.Zero(t, c.staleRecv.Load())
}

// TestRouteReplyFromGeneration_Gen0TakesPlainPath proves gen 0 (secs1, or any out-of-module
// transport) is unaffected: it takes the plain RouteReply path unchanged, resolving whatever
// generation is current.
func TestRouteReplyFromGeneration_Gen0TakesPlainPath(t *testing.T) {
	t.Parallel()

	c, genN, _ := newTwoGenConn(t)

	sb := [4]byte{0, 0, 0, 53}
	replyCh := genN.replies.register(sb, 1, 1, true)
	defer genN.replies.deregister(sb)

	reply := mustSendReply(t, sb)
	delivered := c.RouteReplyFromGeneration(0, reply)
	require.True(t, delivered, "gen 0 must take the plain RouteReply path")

	select {
	case <-replyCh:
	default:
		t.Fatal("gen 0 must still complete the current live generation's transaction via the plain path")
	}
	require.Zero(t, c.staleRecv.Load())
}

// ── Test 4: per-handler / per-channel fan-out cancellation (session-level) ──────────────────

// TestSessionRecvDataMsgOn_CancellationInLastHandlerSkipsChannelSend covers the last/only-handler
// case: a cancellation observed while the (only) handler runs must stop the following channel
// send, even though the channel has room.
//
// Repeated trials, not a single call: with the channel writable and done already closed by the
// time the channel select runs, that select's two cases (ch <- msg and <-done) are both ready, and
// Go's select picks a ready case pseudo-randomly.
// The per-handler/per-channel precheck this test guards makes every trial deterministically skip
// the send; without it a single trial could still pass by scheduler luck.
// Repeating drives that false-negative probability to effectively zero while the test itself stays
// deterministic to run (a fixed iteration count, no sleep, no polling for state).
func TestSessionRecvDataMsgOn_CancellationInLastHandlerSkipsChannelSend(t *testing.T) {
	t.Parallel()

	const trials = 200

	for i := range trials {
		rt := newMockRuntime(t)
		s := newSession(1, rt, &sysBytesGen{})

		done := make(chan struct{})
		s.AddDataMessageHandler(func(_ *DataMessage, _ SECS2Endpoint) {
			close(done) // the generation ends WHILE this, the only/last handler, is running
		})
		ch := make(chan *DataMessage, 1) // buffered/writable — a blocked channel would mask the bug
		s.AddDataMessageChan(ch)

		s.recvDataMsgOn(0, done, NewEmptyDataMessage())

		select {
		case <-ch:
			t.Fatalf("trial %d: a channel delivery must be skipped once done is observed closed, even though the channel had room", i)
		default:
		}
	}
}

// TestSessionRecvDataMsgOn_CancellationInFirstHandlerSkipsSecondHandler covers the handler-1-of-2
// case: a cancellation observed after handler 1 must stop handler 2 from running.
func TestSessionRecvDataMsgOn_CancellationInFirstHandlerSkipsSecondHandler(t *testing.T) {
	t.Parallel()

	rt := newMockRuntime(t)
	s := newSession(1, rt, &sysBytesGen{})

	done := make(chan struct{})
	var secondCalled atomic.Bool
	s.AddDataMessageHandler(
		func(_ *DataMessage, _ SECS2Endpoint) { close(done) },
		func(_ *DataMessage, _ SECS2Endpoint) { secondCalled.Store(true) },
	)

	s.recvDataMsgOn(0, done, NewEmptyDataMessage())

	require.False(t, secondCalled.Load(), "a cancellation observed after handler 1 must stop handler 2 from running")
}

// ── Test 5: decode-error fan-out ─────────────────────────────────────────────────────────────

// TestDeliverOwnedFrameFromGeneration_StaleGenSkipsDecodeErrorHandlers covers a malformed body
// admitted for an ended generation: it must not reach the decode-error handlers.
func TestDeliverOwnedFrameFromGeneration_StaleGenSkipsDecodeErrorHandlers(t *testing.T) {
	t.Parallel()

	c, genN, genSucc := newTwoGenConn(t)
	endAndSucceed(c, genN, genSucc)

	var decodeErrCalled atomic.Bool
	c.AddDecodeErrorHandler(func(_ *DataMessage, _ error, _ SECS2Endpoint) { decodeErrCalled.Store(true) })

	bad := malformedDataMsg(t, 6, 11)
	err := c.DeliverOwnedFrameFromGeneration(genN.id, ownedFrame(t, bad))
	require.NoError(t, err)

	require.False(t, decodeErrCalled.Load(),
		"a malformed body admitted for an ended generation must not reach the decode-error handler")
	require.Equal(t, uint64(1), c.staleRecv.Load())
}

// TestSessionDispatchDecodeErrorOn_CancellationSkipsLaterHandlers covers the mid-fan-out
// cancellation case for the decode-error fan-out (session-level, mirroring Test 4's shape).
func TestSessionDispatchDecodeErrorOn_CancellationSkipsLaterHandlers(t *testing.T) {
	t.Parallel()

	rt := newMockRuntime(t)
	s := newSession(1, rt, &sysBytesGen{})

	done := make(chan struct{})
	var secondCalled atomic.Bool
	s.AddDecodeErrorHandler(
		func(_ *DataMessage, _ error, _ SECS2Endpoint) { close(done) },
		func(_ *DataMessage, _ error, _ SECS2Endpoint) { secondCalled.Store(true) },
	)

	s.dispatchDecodeErrorOn(0, done, NewEmptyDataMessage(), errors.New("boom"))

	require.False(t, secondCalled.Load(), "a cancellation observed after handler 1 must stop handler 2 from running")
}

// ── Test 6: S9F1 must not land on a successor ────────────────────────────────────────────────

// TestCheckSessionID_S9F1NotEnqueuedOnSuccessorAfterGenerationEnds covers a session-ID-mismatched
// primary admitted on genN: checkSessionID pauses (via testHookBeforeS9F1Enqueue) right before it
// enqueues the S9F1 notification, genN ends and genSucc is published while paused, then the
// enqueue is released.
// The S9F1 must land on genN's own send queue, never genSucc's.
func TestCheckSessionID_S9F1NotEnqueuedOnSuccessorAfterGenerationEnds(t *testing.T) {
	t.Parallel()

	c, genN, genSucc := newTwoGenConn(t)
	require.NoError(t, c.UpdateConfigOptions(WithSessionIDValidation(true)))

	b := newBlocker(t)
	c.testHookBeforeS9F1Enqueue = b.block

	// A primary whose SessionID does not match c.SessionID() (0xFFFF default) and is not S9F1
	// itself, so checkSessionID takes the mismatch branch and builds a reject.
	mismatched, err := NewDataMessage(1, 1, false, 0x1234, [4]byte{0, 0, 0, 49}, secs2.NewASCIIItem("X"))
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { done <- c.DeliverOwnedFrameFromGeneration(genN.id, ownedFrame(t, mismatched)) }()

	awaitSignal(t, b.entered, "testHookBeforeS9F1Enqueue was never entered")

	endAndSucceed(c, genN, genSucc)
	b.unblock()

	err = recvWithin(t, done, "DeliverOwnedFrameFromGeneration")
	require.NoError(t, err)

	require.Empty(t, genSucc.sendCh,
		"the S9F1 raised by a mismatched SessionID on genN must never land on genSucc's send queue")
}
