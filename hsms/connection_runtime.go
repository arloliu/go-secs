package hsms

import "errors"

// RouteData delivers an inbound data message to the session fan-out (TransportRuntime).
// The session synchronously fans out to registered handlers (§5.4); it never errors here,
// so RouteData always returns nil.
func (c *connection) RouteData(msg *DataMessage) error {
	c.recvDataMsg(msg) // promoted from the embedded session

	return nil
}

// CommitSelected performs the H2 §7.D synchronous responder commit via the supervisor
// (TransportRuntime). It nil-guards the supervisor: with no live supervisor it reports
// false (no commit happened).
func (c *connection) CommitSelected() bool {
	if s := c.sup.Load(); s != nil {
		return s.CommitSelected()
	}

	return false
}

// SelectLost injects evSelectLost into the supervisor (Selected -> NotSelected,
// TransportRuntime). It nil-guards the supervisor: with no live supervisor it is a no-op.
func (c *connection) SelectLost() {
	if s := c.sup.Load(); s != nil {
		s.CommitSelectLost() // synchronous guarded CAS Selected->NotSelected, then enqueue evSelectLost (I3)
	}
}

// T7Expired injects evT7Timeout (NOT-SELECTED dwell expiry) — TransportRuntime. See the interface doc.
func (c *connection) T7Expired() {
	if s := c.sup.Load(); s != nil {
		s.inject(evT7Timeout)
	}
}

// DeliverOwnedFrame decodes an owned (zero-copy) DATA frame and routes it (TransportRuntime, spec §6.1).
// The recv loop calls this ONLY for a DataMsgType frame received while Selected. Reply correlation is
// offered ONLY for a SECONDARY (a reply: even non-zero function, W-bit clear), which reuses the primary's
// System Bytes (E37 §8.2.6.9); on a registry hit it satisfies the waiting W-bit sender. A PRIMARY (odd
// function, or W-bit set) is NEVER offered to the registry — it always goes to the session handlers via
// RouteData — so a peer's primary is never mis-consumed as a local sender's reply when their System Bytes
// collide (both peers' generators start at 1). An orphan secondary that misses the registry also falls
// through to RouteData (delivered as unsolicited). A decode error is returned (the recv loop treats it as
// a protocol error and keeps reading); it is effectively unreachable because readFrame + dispatchFrame
// already validated length/PType/SType.
func (c *connection) DeliverOwnedFrame(frame []byte) error {
	msg, err := decodeOwnedFrame(frame)
	if err != nil {
		return err
	}

	dm, ok := msg.(*DataMessage)
	if !ok {
		// Unreachable: dispatchFrame routes only DataMsgType frames here. Defensive.
		return errors.New("hsms: DeliverOwnedFrame received a non-data frame")
	}

	c.metrics.incDataMsgRecv() // the single data-receive chokepoint (DataMsgRecvCount)

	// Reply correlation applies ONLY to a SECONDARY (reply). A reply reuses the primary's System
	// Bytes (E37 §8.2.6.9) and, per SECS-II, carries an even function with the W-bit clear (including
	// SxF0, the transaction-abort secondary — E5 §7.2/§10.4.1).
	// Routing a PRIMARY (odd function, or W-bit set) through the reply registry would mis-deliver a
	// peer's primary as a local sender's reply whenever their System Bytes collide — and both peers'
	// generators start at 1, so concurrent first-W-bit transactions DO collide (symmetric HSMS-SS).
	// So only a secondary is offered to the registry; a primary (and an orphan secondary that misses)
	// is delivered to the session's data handlers.
	if isSecondaryReply(dm) {
		if c.RouteReply(dm) {
			return nil
		}
	}

	return c.RouteData(dm)
}

// isSecondaryReply reports whether dm is a SECS-II secondary (reply): an even function code with
// the W-bit clear. This includes function 0 — SEMI E5 §7.2/§10.4.1 reserve SxF0 as the
// transaction-ABORT secondary, sent in lieu of the expected reply (reusing the primary's System
// Bytes) so the originator terminates promptly instead of waiting out T3. Primaries (odd function,
// per §7.2) and any W-bit message are never replies. This is the discriminator that keeps a peer's
// primary out of the local sender's reply registry (see DeliverOwnedFrame) even when System Bytes
// collide across the two symmetric peers; F0 is never a primary (it only ever aborts an existing
// transaction), so admitting it as a secondary is safe for that guard.
func isSecondaryReply(dm *DataMessage) bool {
	return !dm.WaitBit() && dm.Function()%2 == 0
}

// RouteReply looks up the System Bytes of msg in the per-generation sender-owned reply
// registry and non-blocking-delivers the reply to the waiting sender (TransportRuntime, spec
// §5.5). It returns true on a registry hit (a sender received the reply), false on a miss (the
// reply is unsolicited — the caller routes it as a secondary or drops it). It never touches the
// inflight gauge. With no live epoch it reports a miss.
//
// A peer Reject.req (SType 7, E37 §7.9) correlates to our in-flight transaction by System Bytes
// but is a REJECTION, not a reply: it is delivered to the waiting sender as a *RejectError
// (carrying the E37 reason code, header byte 3), so SendDataMessage/SendSECS2Message return
// (nil, *RejectError) rather than silently swallowing the reject as an un-assertable
// *ControlMessage. Legitimate control responses (Select.rsp / Deselect.rsp / Linktest.rsp) are
// still delivered as the routed message, because their responder procedures read the routed rsp.
//
// A miss means the reply is unsolicited (no open transaction). Per E37 §8.3.20 the caller must
// answer an orphan control RESPONSE (Select/Deselect/Linktest.rsp — even SType) with
// Reject(TransactionNotOpen, reason 3), keeping the link; the HSMS-SS recv loop does so on a miss
// (see transport.dispatchFrame → sendRejectTransactionNotOpen). An orphan inbound Reject.req
// (SType 7, not a response) is dropped rather than re-rejected; an orphan data secondary that
// misses falls through to the session as unsolicited.
func (c *connection) RouteReply(msg Message) bool {
	e := c.cur.Load()
	if e == nil {
		return false
	}

	if msg.Type() == RejectReqType {
		// Read header byte 3 (the E37 reject reason) DIRECTLY rather than via GetRejectReasonCode:
		// we surface whatever reason the peer actually sent, including reserved codes (5..255) that
		// GetRejectReasonCode would reject — faithful reporting beats validation on this inbound path.
		header := msg.HeaderBytes()
		return e.replies.route(msg.SystemBytes(), replyResult{err: &RejectError{Reason: header[3]}})
	}

	return e.replies.route(msg.SystemBytes(), replyResult{msg: msg})
}
