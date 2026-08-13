package hsms

import (
	"context"
	"errors"

	"github.com/arloliu/go-secs/v2/gem"
)

// RouteData delivers an inbound data message to the session fan-out (TransportRuntime).
//
// The session synchronously fans out to registered handlers (§5.4); it never errors here, so RouteData always returns nil.
//
// When at least one decode-error handler is registered, RouteData forces the lazy SECS-II body decode here (where the metrics are in scope).
// An undecodable primary is counted and diverted to the decode-error handlers instead of the normal data-message fan-out;
// a message whose body decodes cleanly, and every message when no decode-error handler is registered, routes normally with decoding left lazy.
func (c *connection) RouteData(msg *DataMessage) error {
	if c.hasDecodeErrorHandlers() {
		if derr := msg.DecodeErr(); derr != nil { // forces the lazy body decode and caches the result
			c.metrics.incBodyDecodeErr()
			c.dispatchDecodeError(msg, derr)

			return nil // diverted — do NOT fan out to the normal data-message/channel handlers
		}
	}

	c.recvDataMsg(msg) // promoted from the embedded session

	return nil
}

// CommitSelected performs the H2 §7.D synchronous responder commit via the supervisor (TransportRuntime).
//
// It nil-guards the supervisor: with no live supervisor it reports false (no commit happened).
func (c *connection) CommitSelected() bool {
	return c.commitSelectAccepted(0)
}

// CommitSelectedFromGeneration is CommitSelected performed ON BEHALF OF gen,
// the generation whose recv goroutine completed the handshake.
//
// Both Select commit sites run on the recv goroutine, and a bounded teardown join can abandon that goroutine.
// A delayed correlated Select.rsp, or an inbound Select.req, processed after that generation ended
// would otherwise flip the SUCCESSOR's FSM to Selected on a link that never ran a handshake.
// A gen of 0 skips the match and behaves exactly like CommitSelected.
func (c *connection) CommitSelectedFromGeneration(gen uint64) bool {
	return c.commitSelectAccepted(gen)
}

// commitSelectAccepted is the shared body of the Select-accepted commit.
func (c *connection) commitSelectAccepted(gen uint64) bool {
	if s := c.sup.Load(); s != nil {
		// CauseSelectAccepted: every caller of this commit is a completed select handshake.
		// That covers the HSMS-SS responder and initiator paths,
		// plus secs1's auto-commit, which runs no handshake but reaches Selected for the same reason: the session is now usable.
		return s.CommitSelectedFromGeneration(gen, CauseSelectAccepted)
	}

	return false
}

// SelectLost injects evSelectLost into the supervisor (Selected -> NotSelected, TransportRuntime).
//
// It nil-guards the supervisor: with no live supervisor it is a no-op.
//
// It carries no generation identity;
// an in-module transport reports through [connection.SelectLostFromGeneration] instead.
func (c *connection) SelectLost() {
	c.commitSelectLost(0)
}

// SelectLostFromGeneration is SelectLost reported ON BEHALF OF gen,
// the generation whose recv goroutine answered the Deselect.req.
//
// It closes the same hazard as [connection.CommitSelectedFromGeneration], from the same goroutine:
// a Deselect answered after the generation ended must not deselect the successor's live link.
// A gen of 0 skips the match.
//
// It reports whether the commit was applied, for the same reason [connection.TCPUpFromGeneration] reports its refusal:
// the caller has work of its own that belongs to the deselected link
// (stopping that link's auto-linktest, arming its T7 dwell)
// and must skip all of it when the commit was refused,
// or a generation that has ended silently disarms its successor.
// A refusal here means either that gen is over or that the link was not Selected to begin with;
// neither is a state the caller may act on.
func (c *connection) SelectLostFromGeneration(gen uint64) bool {
	return c.commitSelectLost(gen)
}

// commitSelectLost is the shared body of the Select-lost commit, reporting whether the CAS committed.
func (c *connection) commitSelectLost(gen uint64) bool {
	if s := c.sup.Load(); s != nil {
		// CausePeerDeselect: leaving Selected while the transport link stays up has exactly one producer,
		// the responder answering an inbound Deselect.req (E37 §7.7).
		// A peer Separate drops the link instead and arrives through TCPDownWithCause, not here.
		// Synchronous guarded CAS Selected->NotSelected, then enqueue evSelectLost (I3).
		return s.CommitSelectLostFromGeneration(gen, CausePeerDeselect)
	}

	return false
}

// T7Expired injects evT7Timeout (NOT-SELECTED dwell expiry) — TransportRuntime.
//
// See the interface doc.
// It carries no generation identity;
// an in-module transport reports through [connection.T7ExpiredFromGeneration] instead.
func (c *connection) T7Expired() {
	c.injectT7Expiry(0)
}

// T7ExpiredFromGeneration is T7Expired reported ON BEHALF OF the generation whose NOT-SELECTED dwell expired.
//
// A dwell timer belonging to a generation that has already ended must not drop its successor,
// which is the same hazard [connection.TCPDownFromGeneration] exists to close and is closed the same way:
// the generation travels with the queued event and is re-checked when the FSM processes it.
// A gen of 0 skips the match.
func (c *connection) T7ExpiredFromGeneration(gen uint64) {
	c.injectT7Expiry(gen)
}

// injectT7Expiry is the shared body of the T7 dwell-expiry back-channel.
func (c *connection) injectT7Expiry(gen uint64) {
	if s := c.sup.Load(); s != nil {
		// The site IS the T7 dwell expiry; no other event reaches here.
		s.injectFrom(gen, evT7Timeout, CauseT7Timeout)
	}
}

// DeliverOwnedFrame decodes an owned (zero-copy) DATA frame and routes it (TransportRuntime, spec §6.1).
//
// The recv loop calls this ONLY for a DataMsgType frame received while Selected.
// Reply correlation is offered ONLY for a SECONDARY (a reply: even non-zero function, W-bit clear),
// which reuses the primary's System Bytes (E37 §8.2.6.9); on a registry hit it satisfies the waiting W-bit sender.
// A PRIMARY (odd function, or W-bit set) is NEVER offered to the registry — it always goes to the session handlers via RouteData —
// so a peer's primary is never mis-consumed as a local sender's reply when their System Bytes collide (both peers' generators start at 1).
// An orphan secondary that misses the registry also falls through to RouteData (delivered as unsolicited).
// A decode error is returned (the recv loop treats it as a protocol error and keeps reading);
// it is effectively unreachable because readFrame + dispatchFrame already validated length/PType/SType.
func (c *connection) DeliverOwnedFrame(frame []byte) error {
	msg, err := decodeOwnedFrame(frame)
	if err != nil {
		c.metrics.incDecodeErr()

		if cfg := c.cfg.Load(); cfg.traceTraffic {
			cfg.logger.Debug("hsms: trace: inbound frame decode failed",
				"error", err, "raw", hexDump(frame))
		}

		return err
	}

	dm, ok := msg.(*DataMessage)
	if !ok {
		// Unreachable: dispatchFrame routes only DataMsgType frames here. Defensive.
		return errors.New("hsms: DeliverOwnedFrame received a non-data frame")
	}

	c.metrics.incDataMsgRecv() // the single data-receive chokepoint (DataMsgRecvCount)

	if cfg := c.cfg.Load(); cfg.traceTraffic {
		cfg.logger.Debug("hsms: trace: received data message",
			"session_id", dm.SessionID(), "system_bytes", dm.SystemBytes(), "raw", hexDump(dm.ToBytes()))
	}

	if c.cfg.Load().validateSessionID {
		if err := c.checkSessionID(dm); err != nil {
			return nil // dropped — not a decode error, so the recv loop keeps reading
		}
	}

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

// checkSessionID validates dm's SessionID against this connection's own configured SessionID (see
// WithSessionIDValidation). On a mismatch it sends an S9F1 from this connection's own SessionID
// (fire-and-forget — a failure to send the S9F1 does not change the outcome) and returns
// ErrUnrecognizedSessionID so the caller drops dm without routing it. An inbound S9F1 is exempted
// from the check entirely (regardless of its own SessionID): checkSessionID returns nil immediately,
// so the caller proceeds to route dm normally (delivered to DataMessageHandlers, or reply-correlated
// if it matches a pending transaction) — it is NOT dropped. This both avoids an S9F1-answers-S9F1
// notification loop and preserves visibility into a diagnostic message the peer sent us.
//
// Per SEMI E5 §10.13, S9F1's body is MHEAD — the 10-byte header of the OFFENDING message (dm), not
// an empty item. gem.S9F1(dm.HeaderBytes()) builds the correctly-shaped SECS2Message; this
// re-stamps it as a DataMessage carrying THIS connection's own SessionID and a fresh System Bytes
// (S9F1 is a notification, not a reply to dm, so it must not reuse dm's System Bytes).
func (c *connection) checkSessionID(dm *DataMessage) error {
	isS9F1 := dm.Stream() == 9 && dm.Function() == 1
	if isS9F1 || dm.SessionID() == c.SessionID() {
		return nil
	}

	s9 := gem.S9F1(dm.HeaderBytes())
	reject, err := NewDataMessage(
		s9.StreamCode(), s9.FunctionCode(), s9.WaitBit(),
		c.SessionID(), c.NextSystemBytes(), s9.Item(),
	)
	if err == nil {
		_ = c.SendAsync(context.Background(), reject)
	}

	return ErrUnrecognizedSessionID
}

// isSecondaryReply reports whether dm is a SECS-II secondary (reply): the exact negation of
// [DataMessage.IsPrimary], kept as its own name at this call site for readability.
// This includes function 0 — SEMI E5 §7.2/§10.4.1 reserve SxF0 as the transaction-ABORT
// secondary, sent in lieu of the expected reply (reusing the primary's System Bytes) so the
// originator terminates promptly instead of waiting out T3.
// Primaries (odd function, per §7.2) and any W-bit message are never replies.
// This is the discriminator that keeps a peer's primary out of the local sender's reply registry
// (see DeliverOwnedFrame) even when System Bytes collide across the two symmetric peers; F0 is
// never a primary (it only ever aborts an existing transaction), so admitting it as a secondary
// is safe for that guard.
func isSecondaryReply(dm *DataMessage) bool {
	return !dm.IsPrimary()
}

// RouteReply looks up the System Bytes of msg in the per-generation sender-owned reply registry
// and non-blocking-delivers the reply to the waiting sender (TransportRuntime, spec §5.5).
//
// It returns true on delivery (a sender received the reply), false on a miss — either the registry
// has no open transaction for these System Bytes, or (WithStrictReplyMatching only) the candidate's
// stream/function diverged from the registered primary (E37 §9.4.1). Either way the caller routes
// the message as a secondary or drops it; a strict-mode field mismatch does NOT consume the
// registration, so a later conforming reply can still complete the transaction.
// It never touches the inflight gauge.
// With no live epoch it reports a miss.
//
// Every mismatched candidate increments ReplyMismatchCount, regardless of strict — see that
// counter's godoc for what each reading means.
// Control responses (Select.rsp/Deselect.rsp/
// Linktest.rsp) and a peer Reject.req are never compared (replyRegistry.route's control-transaction
// exemption) and so never move the counter.
//
// A peer Reject.req (SType 7, E37 §7.10) correlates to our in-flight transaction by System Bytes
// but is a REJECTION, not a reply:
// it is delivered to the waiting sender as a *RejectError (carrying the E37 reason code, header byte 3),
// so SendDataMessage/SendSECS2Message return (nil, *RejectError) rather than silently swallowing the reject as an un-assertable
// *ControlMessage. Legitimate control responses (Select.rsp / Deselect.rsp / Linktest.rsp) are
// still delivered as the routed message, because their responder procedures read the routed rsp.
//
// Per E37 §8.3.20 the caller must answer an orphan control RESPONSE (Select/Deselect/Linktest.rsp —
// even SType) with Reject(TransactionNotOpen, reason 3), keeping the link; the HSMS-SS recv loop does
// so on a miss (see transport.dispatchFrame → sendRejectTransactionNotOpen).
// An orphan inbound Reject.req (SType 7, not a response) is dropped rather than re-rejected; an orphan data secondary
// that misses falls through to the session as unsolicited.
func (c *connection) RouteReply(msg Message) bool {
	e := c.cur.Load()
	if e == nil {
		return false
	}

	strict := c.cfg.Load().strictReplyMatching

	var delivered, mismatched bool
	if msg.Type() == RejectReqType {
		// Read header byte 3 (the E37 reject reason) DIRECTLY rather than via GetRejectReasonCode:
		// we surface whatever reason the peer actually sent, including reason 0 — a value no SEMI
		// E37 entity ever assigns, but which GetRejectReasonCode would reject — faithful reporting
		// beats validation on this inbound path.
		header := msg.HeaderBytes()
		delivered, mismatched = e.replies.route(msg.SystemBytes(), replyResult{err: &RejectError{Reason: header[3]}}, strict)
	} else {
		delivered, mismatched = e.replies.route(msg.SystemBytes(), replyResult{msg: msg}, strict)
	}

	if mismatched {
		c.metrics.incReplyMismatch()
	}

	return delivered
}
