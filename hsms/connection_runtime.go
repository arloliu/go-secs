package hsms

import (
	"context"
	"errors"

	"github.com/arloliu/go-secs/v2/gem"
	"github.com/arloliu/go-secs/v2/logger"
)

// RouteData delivers an inbound data message to the session fan-out (TransportRuntime).
//
// The session synchronously fans out to registered handlers (§5.4); it never errors here, so RouteData always returns nil.
//
// When at least one decode-error handler is registered, RouteData forces the lazy SECS-II body decode here (where the metrics are in scope).
// An undecodable primary is counted and diverted to the decode-error handlers instead of the normal data-message fan-out;
// a message whose body decodes cleanly, and every message when no decode-error handler is registered, routes normally with decoding left lazy.
//
// RouteData resolves the current epoch once and delegates to routeDataOn,
// fencing the fan-out's cancellation check to the epoch resolved HERE rather than whatever generation is current when a handler or channel delivery actually runs.
func (c *connection) RouteData(msg *DataMessage) error {
	return c.routeDataOn(c.cur.Load(), msg)
}

// routeDataOn is RouteData's body, performed on an ALREADY-RESOLVED epoch e.
//
// e may be nil (no live epoch — before the first Open, or after a full Close):
// the fan-out then runs against a pre-closed done signal (epochDone), exactly as Done() reports today,
// so a decode-error/data delivery attempted with no live connection observes an already-cancelled generation and delivers to nothing.
//
// The decode-error diversion uses dispatchDecodeErrorOn, bound to e's own cancellation signal.
func (c *connection) routeDataOn(e *epoch, msg *DataMessage) error {
	done := epochDone(e)

	if c.hasDecodeErrorHandlers() {
		if derr := msg.DecodeErr(); derr != nil { // forces the lazy body decode and caches the result
			c.metrics.incBodyDecodeErr()
			c.dispatchDecodeErrorOn(done, msg, derr)

			return nil // diverted — do NOT fan out to the normal data-message/channel handlers
		}
	}

	c.recvDataMsgOn(done, msg) // promoted from the embedded session

	return nil
}

// CommitSelected performs the H2 §7.D synchronous responder commit via the supervisor (TransportRuntime).
//
// It nil-guards the supervisor: with no live supervisor it reports false (no commit happened).
// With a live supervisor it is still admitted only while a live, un-torn-down generation exists
// (see commitSelectAccepted).
func (c *connection) CommitSelected() bool {
	return c.commitSelectAccepted(0)
}

// CommitSelectedFromGeneration is CommitSelected performed ON BEHALF OF gen,
// the generation whose recv goroutine completed the handshake.
//
// Both Select commit sites run on the recv goroutine, and a bounded teardown join can abandon that goroutine.
// A delayed correlated Select.rsp, or an inbound Select.req, processed after that generation ended
// would otherwise flip the SUCCESSOR's FSM to Selected on a link that never ran a handshake.
// A gen of 0 skips only identity; liveness still applies (see commitSelectAccepted).
func (c *connection) CommitSelectedFromGeneration(gen uint64) bool {
	return c.commitSelectAccepted(gen)
}

// commitSelectAccepted is the shared body of the Select-accepted commit.
//
// It is gated even at gen 0 (secs1, and any out-of-module transport):
// the gate admits it on liveness alone, skipping only identity (see connection.selectCommitGate).
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

// TraceConfig returns trace enablement and its logger from one live configuration snapshot.
func (c *connection) TraceConfig() (bool, logger.Logger) {
	cfg := c.cfg.Load()

	return cfg.traceTraffic, cfg.logger
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
//
// DeliverOwnedFrame resolves the current epoch once and delegates to deliverOwnedFrameOn;
// see that function for the generation-fenced admission, S9F1, and fan-out behavior it drives.
// This is also the entry point SECS-I uses (secs1's assembler calls it directly, with no generation identity of its own),
// so every inbound primary — HSMS-SS or SECS-I — is stamped by the same code path.
func (c *connection) DeliverOwnedFrame(frame []byte) error {
	return c.deliverOwnedFrameOn(c.cur.Load(), frame)
}

// DeliverOwnedFrameFromGeneration is DeliverOwnedFrame performed ON BEHALF OF gen, the generation
// whose recv goroutine read frame.
//
// gen is resolved to its epoch under the generation gate (liveEpoch) BEFORE anything else runs —
// this is the admission cutoff: a generation whose teardown has already latched ended is refused here, before the frame is even decoded,
// so a straggler naming a dead generation can never reach the reply registry or the session fan-out, regardless of what publishes a successor in the meantime.
// A refusal is not a decode error (the frame may be perfectly well-formed) —
// it is dropped, counted at staleRecv, and logged at Debug, mirroring SendAsyncFromGeneration's stale accounting on the outbound side.
//
// Once gen is admitted, everything downstream (decode, S9F1, reply correlation, session fan-out)
// runs against the RESOLVED epoch, never a re-read of c.cur — see deliverOwnedFrameOn.
//
// A gen of 0 (secs1, or any out-of-module transport) takes the plain DeliverOwnedFrame path unchanged.
func (c *connection) DeliverOwnedFrameFromGeneration(gen uint64, frame []byte) error {
	if gen == 0 {
		return c.DeliverOwnedFrame(frame)
	}

	e := c.liveEpoch(gen)
	if e == nil {
		c.staleRecv.Add(1)
		c.cfg.Load().logger.Debug("hsms: dropped an inbound frame admitted by a generation that is no longer live",
			"reported_generation", gen, "current_generation", c.CurrentGeneration())

		return nil
	}

	return c.deliverOwnedFrameOn(e, frame)
}

// deliverOwnedFrameOn is DeliverOwnedFrame's body, performed on an ALREADY-RESOLVED epoch e.
//
// e may be nil (no live epoch — before the first Open, or after a full Close):
// decode still runs (so a caller with no live connection still gets an honest decode error for a malformed frame),
// but the message is not stamped with an origin at all,
// and every downstream fan-out check observes an already-cancelled done signal (epochDone), so nothing is ever delivered.
//
// The origin stamp:
// an inbound *DataMessage admitted while a live generation exists (e != nil) is stamped with the connection's identity token and that generation's id,
// so [SECS2Endpoint.ReplyDataMessage] can bind a reply to that same generation and refuse it once the generation has ended.
// This covers the plain (gen-less) DeliverOwnedFrame path SECS-I uses just the same, since e is still resolved from c.cur there.
func (c *connection) deliverOwnedFrameOn(e *epoch, frame []byte) error {
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
		// Unreachable: dispatchFrame routes only DataMsgType frames here.
		// Defensive.
		return errors.New("hsms: DeliverOwnedFrame received a non-data frame")
	}

	// Origin stamp: both fields are set only when a live generation admitted dm; see the doc above.
	if e != nil {
		dm.originIdent = c.ident
		dm.originGen = e.id
	}

	c.metrics.incDataMsgRecv() // the single data-receive chokepoint (DataMsgRecvCount)

	if cfg := c.cfg.Load(); cfg.traceTraffic {
		cfg.logger.Debug("hsms: trace: received data message",
			"session_id", dm.SessionID(), "system_bytes", dm.SystemBytes(), "raw", hexDump(dm.ToBytes()))
	}

	if c.cfg.Load().validateSessionID {
		if err := c.checkSessionID(e, dm); err != nil {
			return nil // dropped — not a decode error, so the recv loop keeps reading
		}
	}

	if hook := c.testHookBeforeFanout; hook != nil {
		hook()
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
		if c.routeReplyOn(e, dm) {
			return nil
		}
	}

	return c.routeDataOn(e, dm)
}

// checkSessionID validates dm's SessionID against this connection's own configured SessionID
// (see WithSessionIDValidation), performed on the ALREADY-RESOLVED epoch e that admitted dm —
// e is whichever epoch deliverOwnedFrameOn resolved, never a re-read of c.cur.
// On a mismatch it sends an S9F1 from this connection's own SessionID
// (fire-and-forget — a failure to send the S9F1 does not change the outcome),
// and returns ErrUnrecognizedSessionID so the caller drops dm without routing it.
// An inbound S9F1 is exempted from the check entirely, regardless of its own SessionID:
// checkSessionID returns nil immediately, so the caller proceeds to route dm normally
// (delivered to DataMessageHandlers, or reply-correlated if it matches a pending transaction) —
// it is NOT dropped.
// This both avoids an S9F1-answers-S9F1 notification loop
// and preserves visibility into a diagnostic message the peer sent us.
//
// Per SEMI E5 §10.13, S9F1's body is MHEAD — the 10-byte header of the OFFENDING message (dm), not
// an empty item. gem.S9F1(dm.HeaderBytes()) builds the correctly-shaped SECS2Message; this
// re-stamps it as a DataMessage carrying THIS connection's own SessionID and a fresh System Bytes
// (S9F1 is a notification, not a reply to dm, so it must not reuse dm's System Bytes).
//
// The S9F1 is enqueued via enqueueAsync bound to e,
// so it can never land on a successor generation's send queue:
// a mismatch admitted by a generation that ends before the enqueue runs drops the S9F1 along with the rest of that generation's stranded queue (see SendAsyncFromGeneration).
// A nil e (no live epoch) skips the enqueue entirely —
// there is nowhere to send it —
// matching the outcome the old unconditional SendAsync call already had (ErrNotOpen, ignored).
func (c *connection) checkSessionID(e *epoch, dm *DataMessage) error {
	isS9F1 := dm.Stream() == 9 && dm.Function() == 1
	if isS9F1 || dm.SessionID() == c.SessionID() {
		return nil
	}

	if e != nil {
		s9 := gem.S9F1(dm.HeaderBytes())
		reject, err := NewDataMessage(
			s9.StreamCode(), s9.FunctionCode(), s9.WaitBit(),
			c.SessionID(), c.NextSystemBytes(), s9.Item(),
		)
		if err == nil {
			if hook := c.testHookBeforeS9F1Enqueue; hook != nil {
				hook()
			}
			_ = c.enqueueAsync(context.Background(), e, reject)
		}
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
// It returns true on a registry hit, false on a miss.
// A hit does not prove the waiter received the value:
// replyRegistry.route still reports a hit, and discards the result,
// when a duplicate reply finds the sender's one-slot buffer already full.
// A miss has three causes: the registry has no open transaction for these System Bytes;
// (WithStrictReplyMatching only) the candidate's stream/function diverged from the registered primary (E37 §9.4.1);
// or a control response (Select.rsp/Deselect.rsp/Linktest.rsp) answering an open DATA transaction,
// which is always a miss regardless of strict —
// a control response has no stream/function to validate against a data primary in the first place.
// Each miss kind is handled differently by the caller — see the §8.3.20 paragraph below;
// a miss never consumes the registration,
// so a later conforming reply can still complete the transaction.
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
//
// RouteReply resolves the current epoch once and delegates to routeReplyOn.
func (c *connection) RouteReply(msg Message) bool {
	return c.routeReplyOn(c.cur.Load(), msg)
}

// routeReplyOn is RouteReply's body, performed on an ALREADY-RESOLVED epoch e.
//
// A nil e (no live epoch — no generation is registered) is a miss,
// matching what RouteReply always reported before generations existed.
// The nil check runs BEFORE touching msg at all,
// so a nil msg with a nil e (the pre-Open honest-error case) never dereferences it.
func (c *connection) routeReplyOn(e *epoch, msg Message) bool {
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

// RouteReplyFromGeneration is RouteReply performed ON BEHALF OF gen, the generation whose recv goroutine read msg.
//
// gen is resolved under the generation gate exactly like DeliverOwnedFrameFromGeneration.
// A stale gen is reported delivered (true) rather than a miss (false):
// true enters dispatchFrame's HIT branch, which for a status-0 Select.rsp attempts commitSelected(gen) —
// a commit that liveEpoch's own {id, not ended} test refuses for a dead generation, so no side effect follows —
// while false would enter the MISS branch and attempt sendRejectTransactionNotOpen,
// which SendAsyncFromGeneration also drops.
// Both are safe; true avoids a pointless Reject attempt on a link that is already gone.
// staleRecv is counted, and the drop logged at Debug, once here, in this function —
// before dispatchFrame decides which of those two branches the true/false result sends it to.
//
// A gen of 0 takes the plain RouteReply path unchanged.
func (c *connection) RouteReplyFromGeneration(gen uint64, msg Message) bool {
	if gen == 0 {
		return c.RouteReply(msg)
	}

	e := c.liveEpoch(gen)
	if e == nil {
		c.staleRecv.Add(1)
		c.cfg.Load().logger.Debug("hsms: dropped a reply routed by a generation that is no longer live",
			"reported_generation", gen, "current_generation", c.CurrentGeneration())

		return true
	}

	return c.routeReplyOn(e, msg)
}
