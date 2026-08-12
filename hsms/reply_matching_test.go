package hsms

// reply_matching_test.go exercises Task 6's E37 §9.4.1 reply-field matching at the connection
// level: RouteReply/DeliverOwnedFrame wired to a real sendWaitReply transaction, default (observe)
// vs WithStrictReplyMatching (enforce), the T3-stall/no-stall consequence, the SessionID and
// Reject.req exemption legs, and the audit's D1 forwarding-collision scenario.
// The pure
// field-comparison matrix lives in reply_registry_test.go; this file proves the wiring around it.

import (
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// mustReplyDM builds a well-formed *DataMessage secondary (W-bit clear) with the given
// stream/function and System Bytes, for feeding to DeliverOwnedFrame (unlike reply_registry_test.go's
// white-box replyDM, this one must decode cleanly).
func mustReplyDM(t *testing.T, stream, function uint8, sysBytes [4]byte) *DataMessage {
	t.Helper()

	msg, err := NewDataMessage(stream, function, false, 0xFFFF, sysBytes, secs2.NewASCIIItem("REPLY"))
	require.NoError(t, err)

	return msg
}

// TestReplyMatching_Default_MismatchStillDelivered proves the default (observe) posture: a
// wrong-stream reply still completes the sender's transaction (no T3 stall, no fall-through to
// the data handlers) while ReplyMismatchCount observes the sloppy peer.
func TestReplyMatching_Default_MismatchStillDelivered(t *testing.T) {
	c, _ := newTestSendConn(t, SelectedState)
	sb := [4]byte{0, 0, 0, 1}

	handlerCh := make(chan *DataMessage, 1)
	c.AddDataMessageHandler(func(msg *DataMessage, _ SECS2Endpoint) { handlerCh <- msg })

	type result struct {
		msg Message
		err error
	}
	resCh := make(chan result, 1)
	go func() {
		m, err := c.sendWaitReply(t.Context(), mustSendData(t, sb, true)) // S1F1 primary
		resCh <- result{m, err}
	}()

	require.Eventually(t, func() bool {
		return c.metrics.DataMsgInflightCount() == 1
	}, 2*time.Second, time.Millisecond)

	// Wrong stream (2, not 1), correct function (2 == primary+1).
	require.NoError(t, c.DeliverOwnedFrame(ownedFrame(t, mustReplyDM(t, 2, 2, sb))))

	got := <-resCh
	require.NoError(t, got.err)
	require.NotNil(t, got.msg, "default mode delivers despite the stream mismatch")
	gotDM, ok := got.msg.(*DataMessage)
	require.True(t, ok)
	require.Equal(t, uint8(2), gotDM.Stream())
	require.Equal(t, uint64(1), c.metrics.ReplyMismatchCount())

	select {
	case <-handlerCh:
		t.Fatal("a delivered reply must not also reach the data handler")
	default:
	}
}

// TestReplyMatching_Default_FunctionMismatchStillDelivered mirrors the stream case for function:
// a function that is neither primary+1 nor 0 still delivers by default, counted once.
func TestReplyMatching_Default_FunctionMismatchStillDelivered(t *testing.T) {
	c, _ := newTestSendConn(t, SelectedState)
	sb := [4]byte{0, 0, 0, 2}

	type result struct {
		msg Message
		err error
	}
	resCh := make(chan result, 1)
	go func() {
		m, err := c.sendWaitReply(t.Context(), mustSendData(t, sb, true))
		resCh <- result{m, err}
	}()

	require.Eventually(t, func() bool {
		return c.metrics.DataMsgInflightCount() == 1
	}, 2*time.Second, time.Millisecond)

	// Correct stream (1), wrong function (4: neither primary+1=2 nor 0).
	require.NoError(t, c.DeliverOwnedFrame(ownedFrame(t, mustReplyDM(t, 1, 4, sb))))

	got := <-resCh
	require.NoError(t, got.err)
	require.NotNil(t, got.msg)
	require.Equal(t, uint64(1), c.metrics.ReplyMismatchCount())
}

// TestReplyMatching_Default_CombinedMismatchCountsOnce proves a candidate wrong on BOTH stream and
// function increments ReplyMismatchCount exactly once, not twice.
func TestReplyMatching_Default_CombinedMismatchCountsOnce(t *testing.T) {
	c, _ := newTestSendConn(t, SelectedState)
	sb := [4]byte{0, 0, 0, 3}

	go func() { _, _ = c.sendWaitReply(t.Context(), mustSendData(t, sb, true)) }()

	require.Eventually(t, func() bool {
		return c.metrics.DataMsgInflightCount() == 1
	}, 2*time.Second, time.Millisecond)

	require.NoError(t, c.DeliverOwnedFrame(ownedFrame(t, mustReplyDM(t, 5, 6, sb))))

	require.Eventually(t, func() bool {
		return c.metrics.DataMsgInflightCount() == 0
	}, 2*time.Second, time.Millisecond, "the delivered reply must end the transaction")
	require.Equal(t, uint64(1), c.metrics.ReplyMismatchCount(), "a combined mismatch counts once, not per field")
}

// TestReplyMatching_CorrectReply_NoMismatchInEitherMode proves a correct reply is delivered in
// both modes and ReplyMismatchCount stays 0 — the counter-assertion that would catch a check
// flagging everything.
func TestReplyMatching_CorrectReply_NoMismatchInEitherMode(t *testing.T) {
	for _, strict := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "strict"}[strict], func(t *testing.T) {
			var opts []ConnOption
			if strict {
				opts = append(opts, WithStrictReplyMatching(true))
			}
			c, _ := newTestSendConn(t, SelectedState, opts...)
			sb := [4]byte{0, 0, 0, 4}

			type result struct {
				msg Message
				err error
			}
			resCh := make(chan result, 1)
			go func() {
				m, err := c.sendWaitReply(t.Context(), mustSendData(t, sb, true))
				resCh <- result{m, err}
			}()

			require.Eventually(t, func() bool {
				return c.metrics.DataMsgInflightCount() == 1
			}, 2*time.Second, time.Millisecond)

			require.NoError(t, c.DeliverOwnedFrame(ownedFrame(t, mustReplyDM(t, 1, 2, sb))))

			got := <-resCh
			require.NoError(t, got.err)
			require.NotNil(t, got.msg)
			require.Equal(t, uint64(0), c.metrics.ReplyMismatchCount())
		})
	}
}

// TestReplyMatching_SameStreamF0Accepted_NoMismatchInEitherMode proves same-stream F0 (the SxF0
// abort secondary, E5 §7.2/§10.4.1) is accepted in both modes with no counter increment — the
// counter-assertion that would catch an over-strict function == primary+1 check.
func TestReplyMatching_SameStreamF0Accepted_NoMismatchInEitherMode(t *testing.T) {
	for _, strict := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "strict"}[strict], func(t *testing.T) {
			var opts []ConnOption
			if strict {
				opts = append(opts, WithStrictReplyMatching(true))
			}
			c, _ := newTestSendConn(t, SelectedState, opts...)
			sb := [4]byte{0, 0, 0, 5}

			type result struct {
				msg Message
				err error
			}
			resCh := make(chan result, 1)
			go func() {
				m, err := c.sendWaitReply(t.Context(), mustSendData(t, sb, true)) // S1F1
				resCh <- result{m, err}
			}()

			require.Eventually(t, func() bool {
				return c.metrics.DataMsgInflightCount() == 1
			}, 2*time.Second, time.Millisecond)

			require.NoError(t, c.DeliverOwnedFrame(ownedFrame(t, mustReplyDM(t, 1, 0, sb)))) // S1F0

			got := <-resCh
			require.NoError(t, got.err)
			require.NotNil(t, got.msg)
			require.Equal(t, uint64(0), c.metrics.ReplyMismatchCount())
		})
	}
}

// TestReplyMatching_Strict_MismatchMissesAndStallsToT3 proves a strict-mode mismatch is a MISS: the
// message reaches the data handlers as unsolicited, ReplyMismatchCount increments, and — with no
// conforming reply following — the sender times out on T3.
func TestReplyMatching_Strict_MismatchMissesAndStallsToT3(t *testing.T) {
	c, _ := newTestSendConn(t, SelectedState, WithStrictReplyMatching(true))
	c.cfg.Load().timers.T3 = 40 * time.Millisecond
	sb := [4]byte{0, 0, 0, 6}

	handlerCh := make(chan *DataMessage, 1)
	c.AddDataMessageHandler(func(msg *DataMessage, _ SECS2Endpoint) { handlerCh <- msg })

	errCh := make(chan error, 1)
	go func() {
		_, err := c.sendWaitReply(t.Context(), mustSendData(t, sb, true)) // S1F1
		errCh <- err
	}()

	require.Eventually(t, func() bool {
		return c.metrics.DataMsgInflightCount() == 1
	}, 2*time.Second, time.Millisecond)

	require.NoError(t, c.DeliverOwnedFrame(ownedFrame(t, mustReplyDM(t, 2, 2, sb)))) // wrong stream

	select {
	case got := <-handlerCh:
		require.Equal(t, sb, got.SystemBytes(), "a strict-mode miss falls through to the data handler")
	case <-time.After(2 * time.Second):
		t.Fatal("mismatched reply must reach the data handler under strict mode")
	}

	require.ErrorIs(t, <-errCh, ErrT3Timeout, "with no conforming reply, the sender must stall to T3")
	require.Equal(t, uint64(1), c.metrics.ReplyMismatchCount())
}

// TestReplyMatching_Strict_FunctionMismatchMisses pins the function leg of the strict-mode miss
// (an even, W-clear function that is neither primary+1 nor 0) — wrong-stream alone must not be
// the only strict case this package guards.
func TestReplyMatching_Strict_FunctionMismatchMisses(t *testing.T) {
	c, _ := newTestSendConn(t, SelectedState, WithStrictReplyMatching(true))
	c.cfg.Load().timers.T3 = 40 * time.Millisecond
	sb := [4]byte{0, 0, 0, 7}

	handlerCh := make(chan *DataMessage, 1)
	c.AddDataMessageHandler(func(msg *DataMessage, _ SECS2Endpoint) { handlerCh <- msg })

	errCh := make(chan error, 1)
	go func() {
		_, err := c.sendWaitReply(t.Context(), mustSendData(t, sb, true)) // S1F1
		errCh <- err
	}()

	require.Eventually(t, func() bool {
		return c.metrics.DataMsgInflightCount() == 1
	}, 2*time.Second, time.Millisecond)

	require.NoError(t, c.DeliverOwnedFrame(ownedFrame(t, mustReplyDM(t, 1, 6, sb)))) // wrong function

	select {
	case <-handlerCh:
	case <-time.After(2 * time.Second):
		t.Fatal("mismatched reply must reach the data handler under strict mode")
	}

	require.ErrorIs(t, <-errCh, ErrT3Timeout)
	require.Equal(t, uint64(1), c.metrics.ReplyMismatchCount())
}

// TestReplyMatching_Strict_WrongStreamF0StillMisses proves the F0 exception is function-only: an
// F0 secondary on the WRONG stream is still a mismatch (and a miss under strict), unlike the
// same-stream F0 case above.
func TestReplyMatching_Strict_WrongStreamF0StillMisses(t *testing.T) {
	c, _ := newTestSendConn(t, SelectedState, WithStrictReplyMatching(true))
	c.cfg.Load().timers.T3 = 40 * time.Millisecond
	sb := [4]byte{0, 0, 0, 8}

	handlerCh := make(chan *DataMessage, 1)
	c.AddDataMessageHandler(func(msg *DataMessage, _ SECS2Endpoint) { handlerCh <- msg })

	errCh := make(chan error, 1)
	go func() {
		_, err := c.sendWaitReply(t.Context(), mustSendData(t, sb, true)) // S1F1
		errCh <- err
	}()

	require.Eventually(t, func() bool {
		return c.metrics.DataMsgInflightCount() == 1
	}, 2*time.Second, time.Millisecond)

	require.NoError(t, c.DeliverOwnedFrame(ownedFrame(t, mustReplyDM(t, 2, 0, sb)))) // S2F0: wrong stream

	select {
	case <-handlerCh:
	case <-time.After(2 * time.Second):
		t.Fatal("a wrong-stream F0 must still miss and reach the data handler under strict mode")
	}

	require.ErrorIs(t, <-errCh, ErrT3Timeout)
	require.Equal(t, uint64(1), c.metrics.ReplyMismatchCount())
}

// TestReplyMatching_Strict_MismatchThenConformingReplyCompletes proves a strict-mode miss does NOT
// consume the registration: a conforming reply that arrives after the mismatch but before T3
// still completes the sender's transaction successfully.
func TestReplyMatching_Strict_MismatchThenConformingReplyCompletes(t *testing.T) {
	c, _ := newTestSendConn(t, SelectedState, WithStrictReplyMatching(true))
	c.cfg.Load().timers.T3 = 3 * time.Second // generous — the conforming reply must beat this
	sb := [4]byte{0, 0, 0, 9}

	handlerCh := make(chan *DataMessage, 1)
	c.AddDataMessageHandler(func(msg *DataMessage, _ SECS2Endpoint) { handlerCh <- msg })

	type result struct {
		msg Message
		err error
	}
	resCh := make(chan result, 1)
	go func() {
		m, err := c.sendWaitReply(t.Context(), mustSendData(t, sb, true)) // S1F1
		resCh <- result{m, err}
	}()

	require.Eventually(t, func() bool {
		return c.metrics.DataMsgInflightCount() == 1
	}, 2*time.Second, time.Millisecond)

	// First: a mismatched candidate misses and falls through to the handler.
	require.NoError(t, c.DeliverOwnedFrame(ownedFrame(t, mustReplyDM(t, 2, 2, sb))))
	select {
	case <-handlerCh:
	case <-time.After(2 * time.Second):
		t.Fatal("the mismatched candidate must reach the data handler")
	}

	// Then: the conforming reply, same System Bytes, satisfies the still-open registration.
	require.NoError(t, c.DeliverOwnedFrame(ownedFrame(t, mustReplyDM(t, 1, 2, sb))))

	got := <-resCh
	require.NoError(t, got.err, "a later conforming reply must complete the transaction, not T3")
	require.NotNil(t, got.msg)
	require.Equal(t, uint64(1), c.metrics.ReplyMismatchCount(), "only the first, mismatched candidate is counted")
}

// TestReplyMatching_Strict_RejectReqDeliversImmediately proves that under strict mode, an inbound
// Reject.req answering a W-bit data primary is delivered to the sender as a *RejectError
// immediately — ReplyMismatchCount stays 0 and nothing waits for T3. The registry entry is
// data-registered but the routed result is field-less (RejectError), so a comparison keyed on the
// registration alone would misclassify a legitimate rejection.
func TestReplyMatching_Strict_RejectReqDeliversImmediately(t *testing.T) {
	c, _ := newTestSendConn(t, SelectedState, WithStrictReplyMatching(true))
	c.cfg.Load().timers.T3 = 3 * time.Second // must NOT be reached
	sb := [4]byte{0, 0, 0, 10}

	primary := mustSendData(t, sb, true) // S1F1

	type result struct {
		msg Message
		err error
	}
	resCh := make(chan result, 1)
	go func() {
		m, err := c.sendWaitReply(t.Context(), primary)
		resCh <- result{m, err}
	}()

	require.Eventually(t, func() bool {
		return c.metrics.DataMsgInflightCount() == 1
	}, 2*time.Second, time.Millisecond)

	reject, err := NewRejectReq(primary, RejectTransactionNotOpen)
	require.NoError(t, err)
	require.True(t, c.RouteReply(reject), "a Reject.req answering an open transaction must hit the registry")

	select {
	case got := <-resCh:
		var rejectErr *RejectError
		require.ErrorAs(t, got.err, &rejectErr, "the sender must receive *RejectError, not a T3 timeout")
		require.Nil(t, got.msg)
	case <-time.After(2 * time.Second):
		t.Fatal("Reject.req must deliver immediately, not stall for T3")
	}

	require.Equal(t, uint64(0), c.metrics.ReplyMismatchCount(), "a field-less RejectError result is never a mismatch")
}

// TestReplyMatching_SessionID_ValidationOff_NotComparedInRegistry proves SessionID is NEVER
// compared in the registry: with WithSessionIDValidation off (the default), a reply whose
// SessionID differs from the connection's own is still delivered by field (stream/function) match,
// with no ReplyMismatchCount increment.
func TestReplyMatching_SessionID_ValidationOff_NotComparedInRegistry(t *testing.T) {
	c, _ := newTestSendConn(t, SelectedState)
	sb := [4]byte{0, 0, 0, 11}

	type result struct {
		msg Message
		err error
	}
	resCh := make(chan result, 1)
	go func() {
		m, err := c.sendWaitReply(t.Context(), mustSendData(t, sb, true)) // S1F1, SessionID 0xFFFF
		resCh <- result{m, err}
	}()

	require.Eventually(t, func() bool {
		return c.metrics.DataMsgInflightCount() == 1
	}, 2*time.Second, time.Millisecond)

	// Correct stream/function, but a DIFFERENT SessionID than the primary carried.
	mismatchedSession, err := NewDataMessage(1, 2, false, 0x0001, sb, secs2.NewASCIIItem("REPLY"))
	require.NoError(t, err)
	require.NoError(t, c.DeliverOwnedFrame(ownedFrame(t, mismatchedSession)))

	got := <-resCh
	require.NoError(t, got.err, "SessionID is never compared in the registry — a field match still delivers")
	require.NotNil(t, got.msg)
	require.Equal(t, uint64(0), c.metrics.ReplyMismatchCount())
}

// TestReplyMatching_SessionID_ValidationOn_DroppedBeforeRegistry proves the existing pinned
// behavior: with WithSessionIDValidation on, a SessionID-mismatched reply is dropped and answered
// with S9F1 BEFORE reply correlation — the registry never sees it, so ReplyMismatchCount stays 0.
func TestReplyMatching_SessionID_ValidationOn_DroppedBeforeRegistry(t *testing.T) {
	c, _ := newTestSendConn(t, SelectedState)
	require.NoError(t, c.UpdateConfigOptions(WithSessionIDValidation(true)))
	c.cfg.Load().timers.T3 = 60 * time.Millisecond
	sb := [4]byte{0, 0, 0, 12}

	errCh := make(chan error, 1)
	go func() {
		_, err := c.sendWaitReply(t.Context(), mustSendData(t, sb, true)) // S1F1, SessionID == c.SessionID()
		errCh <- err
	}()

	require.Eventually(t, func() bool {
		return c.metrics.DataMsgInflightCount() == 1
	}, 2*time.Second, time.Millisecond)

	// Correct stream/function but a mismatched SessionID: checkSessionID drops it before the
	// registry is ever consulted.
	mismatchedSession, err := NewDataMessage(1, 2, false, 0x0001, sb, secs2.NewASCIIItem("REPLY"))
	require.NoError(t, err)
	require.NoError(t, c.DeliverOwnedFrame(ownedFrame(t, mismatchedSession)))

	// The registry never saw it, so the sender is still waiting and must run to T3.
	require.ErrorIs(t, <-errCh, ErrT3Timeout, "a SessionID-dropped reply never reaches the registry")
	require.Equal(t, uint64(0), c.metrics.ReplyMismatchCount(), "dropped before the registry — never counted")
}

// TestReplyMatching_ForwardingCollision_Default reproduces the audit's D1 reply-theft case: a
// forwarded primary's caller-supplied System Bytes collide with a local in-flight transaction
// (ForwardDataMessage registers no waiter of its own — sendNoReply — so nothing but the local
// waiter's entry exists for this key). The peer answers with the forwarded message's own
// stream/function.
// Under the default, the stolen delivery still happens (current behavior) and
// the mismatch is counted.
func TestReplyMatching_ForwardingCollision_Default(t *testing.T) {
	c, _ := newTestSendConn(t, SelectedState)
	sb := [4]byte{0, 0, 0, 13}

	type result struct {
		msg Message
		err error
	}
	resCh := make(chan result, 1)
	go func() {
		// The local sender's own primary: S1F1.
		m, err := c.sendWaitReply(t.Context(), mustSendData(t, sb, true))
		resCh <- result{m, err}
	}()

	require.Eventually(t, func() bool {
		return c.metrics.DataMsgInflightCount() == 1
	}, 2*time.Second, time.Millisecond)

	// The peer's reply to a DIFFERENT, forwarded primary (S5F1) that happened to reuse the same
	// System Bytes (the collision) — echoing S5F2, not the local sender's S1F2.
	require.NoError(t, c.DeliverOwnedFrame(ownedFrame(t, mustReplyDM(t, 5, 2, sb))))

	got := <-resCh
	require.NoError(t, got.err)
	require.NotNil(t, got.msg, "default mode: the stolen delivery still happens")
	gotDM, ok := got.msg.(*DataMessage)
	require.True(t, ok)
	require.Equal(t, uint8(5), gotDM.Stream(),
		"the local sender received the forwarded transaction's reply, not its own")
	require.Equal(t, uint64(1), c.metrics.ReplyMismatchCount())
}

// TestReplyMatching_ForwardingCollision_Strict proves strict mode closes the D1 theft: the
// colliding reply misses (routes to the data handlers instead of the local waiter), and a later
// conforming reply still satisfies the local waiter.
func TestReplyMatching_ForwardingCollision_Strict(t *testing.T) {
	c, _ := newTestSendConn(t, SelectedState, WithStrictReplyMatching(true))
	c.cfg.Load().timers.T3 = 3 * time.Second // generous — the conforming reply must beat this
	sb := [4]byte{0, 0, 0, 14}

	handlerCh := make(chan *DataMessage, 1)
	c.AddDataMessageHandler(func(msg *DataMessage, _ SECS2Endpoint) { handlerCh <- msg })

	type result struct {
		msg Message
		err error
	}
	resCh := make(chan result, 1)
	go func() {
		m, err := c.sendWaitReply(t.Context(), mustSendData(t, sb, true)) // local S1F1
		resCh <- result{m, err}
	}()

	require.Eventually(t, func() bool {
		return c.metrics.DataMsgInflightCount() == 1
	}, 2*time.Second, time.Millisecond)

	// The colliding forwarded-transaction reply (S5F2) misses under strict mode.
	require.NoError(t, c.DeliverOwnedFrame(ownedFrame(t, mustReplyDM(t, 5, 2, sb))))
	select {
	case got := <-handlerCh:
		require.Equal(t, uint8(5), got.Stream(), "the stolen reply must reach the data handler, not the local waiter")
	case <-time.After(2 * time.Second):
		t.Fatal("the colliding reply must fall through to the data handler under strict mode")
	}

	// The local sender's OWN conforming reply (S1F2) still arrives and completes the transaction.
	require.NoError(t, c.DeliverOwnedFrame(ownedFrame(t, mustReplyDM(t, 1, 2, sb))))

	got := <-resCh
	require.NoError(t, got.err, "the local waiter must still be satisfied by its own conforming reply")
	require.NotNil(t, got.msg)
	require.Equal(t, uint64(1), c.metrics.ReplyMismatchCount(), "only the stolen candidate is counted")
}

// TestReplyMatching_ControlRegistration_DataSecondaryDeliveredUncompared exercises the
// registration-side exemption leg (isData) through the REAL DeliverOwnedFrame path, not the
// synthetic registry-level construction in reply_registry_test.go.
// A control primary
// (Select.req) registers with isData=false, stream=0, function=0 (see sendWaitReply /
// connection_send.go). DeliverOwnedFrame offers every secondary — control OR data — to
// RouteReply as a *DataMessage when it is one (isSecondaryReply narrows on the wire's W-bit/
// function parity, not on what kind of primary is open), so a data secondary whose System Bytes
// happen to collide with an open Select transaction reaches route() with want.isData=false and a
// genuine *DataMessage result: leg 2 (the type assertion) passes, so ONLY leg 1 (isData) keeps
// this uncompared.
// Without leg 1, dm.Stream() != 0 is a mismatch for any real stream, which would
// count under default and miss (stalling the Select initiator to T6) under strict — the same
// field-bug shape the audit found, reached through a data secondary rather than a genuine
// control response.
func TestReplyMatching_ControlRegistration_DataSecondaryDeliveredUncompared(t *testing.T) {
	c, _ := newTestSendConn(t, SelectedState)
	e := c.cur.Load()
	c.cfg.Load().timers.T6 = 3 * time.Second // must NOT be reached
	sb := [4]byte{0, 0, 0, 15}

	type result struct {
		msg Message
		err error
	}
	resCh := make(chan result, 1)
	go func() {
		m, err := c.sendWaitReply(t.Context(), NewSelectReq(0xFFFF, sb))
		resCh <- result{m, err}
	}()

	// Control sends never touch the data inflight gauge (TestSend_ControlUsesT6), so the sync
	// point is the registry entry itself.
	require.Eventually(t, func() bool {
		return e.replies.len() == 1
	}, 2*time.Second, time.Millisecond)

	// A genuine DATA secondary (S1F2, W-clear) colliding with the open Select's System Bytes.
	require.NoError(t, c.DeliverOwnedFrame(ownedFrame(t, mustReplyDM(t, 1, 2, sb))))

	got := <-resCh
	require.NoError(t, got.err, "a control-registered entry must deliver uncompared, never stall")
	require.NotNil(t, got.msg)
	require.Equal(t, uint64(0), c.metrics.ReplyMismatchCount(),
		"a control registration (isData=false) is never a compared candidate")
}
