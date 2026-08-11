package hsmsss

import (
	"context"
	"errors"

	"github.com/arloliu/go-secs/v2/hsms"
)

// errPeerSeparate is the TCPDown cause used when a peer Separate.req (SType 9) arrives on a live generation (E37.1 §7.6).
// Routing the teardown through TCPDown marks the current generation commsFailure=true, which suppresses OUR farewell Separate —
// the peer already signalled it is leaving, so answering with a Separate is neither required nor correct.
var errPeerSeparate = errors.New("hsmsss: peer sent Separate.req")

// handleControlReq dispatches an inbound control request (Select.req, Deselect.req,
// Linktest.req) to the responder procedures. It runs on the recv goroutine.
//
// The Select responder (H2 §7.D), the Linktest responder (§7.8), and the responder-only Deselect
// (D5a-4, §7.7) all land here. HSMS-SS never initiates a Deselect (Separate is used for teardown),
// so only the Deselect.req responder path exists.
func (t *transport) handleControlReq(g *genWG, msg hsms.Message) {
	switch msg.Type() { //nolint:exhaustive // only inbound control REQUESTs reach here (dispatchFrame); rsp/data are routed elsewhere.
	case hsms.SelectReqType:
		t.handleSelectReq(g, msg)
	case hsms.LinktestReqType:
		t.handleLinktestReq(msg)
	case hsms.DeselectReqType:
		t.handleDeselectReq(g, msg)
	default:
		// Unreachable: dispatchFrame routes only Select/Linktest/Deselect.req here.
	}
}

// handleSelectReq is the Select RESPONDER path (H2 §7.D / E37 §7.4.3). It is exercised by BOTH
// a passive side and an active side in simultaneous-select. It runs on the recv goroutine.
//
// H2 INVARIANT (the efb220b fix): commit to Selected SYNCHRONOUSLY via rt.CommitSelected()
// (guarded CAS NotSelected -> Selected) BEFORE writing Select.rsp. Because this runs on the
// single sequential recv goroutine, IsSelected() is guaranteed true before the recv loop
// dispatches any data frame the peer pipelines right after our Select.rsp — so that data is
// NOT spuriously Rejected(NotSelected). Committing AFTER (or asynchronously relative to) the
// rsp reintroduces the bug.
//
// SelectStatus (E37 §8.3.7.2, Table 7): a genuine NotSelected->Selected transition (CAS success)
// answers status 0 (Communication Established). A duplicate Select.req while ALREADY Selected —
// CommitSelected returns false — answers status 1 (SelectStatusAlreadyActive, "Communication Already
// Active"): a prior select already established communication, so this one establishes nothing new
// (M5). The link stays Selected either way; the responder never Rejects a duplicate Select.
func (t *transport) handleSelectReq(g *genWG, req hsms.Message) {
	// H2: synchronous commit BEFORE the rsp. A genuine NotSelected->Selected transition (CAS
	// success) cancels the T7 dwell (§9.2.2 — the NOT-SELECTED window ends) and starts the
	// auto-linktest (D5a-5, on this generation's bundle g — NEW-1); an already-Selected duplicate
	// returns false and must NOT cancel/spawn again, and answers status 1 below.
	status := byte(hsms.SelectStatusSuccess)
	if t.rt.CommitSelected() {
		t.metrics.incSelectEstablished()
		t.cancelT7()
		t.startLinktest(g)
	} else {
		status = hsms.SelectStatusAlreadyActive
	}

	cm, ok := req.(*hsms.ControlMessage)
	if !ok {
		return // unreachable: decodeControlFrame produces *ControlMessage for control STypes
	}

	rsp, err := hsms.NewSelectRsp(cm, status)
	if err != nil {
		return // unreachable: dispatchFrame guarantees req is a Select.req
	}

	// Serialize the rsp through the core's async send path (same rationale as sendReject): the
	// core's single-writer writeMu invariant must not be bypassed by a direct recv-path Write.
	_ = t.rt.SendAsync(context.Background(), rsp)
}

// handleSeparateReq processes an inbound Separate.req (SType 9).
// The peer is leaving: drive an involuntary disconnect via rt.TCPDown,
// whose commsFailure marking suppresses OUR farewell Separate (§9.1.1)
// and whose evDisconnect injection funnels teardown through the single NotConnected reaction.
// It returns false so the recv loop ends without a redundant TCPDown.
//
// The teardown is UNCONDITIONAL — NotSelected included — because E37.1 §7.6 overrides E37 generic here.
// The generic standard (§7.9.2.3) says "If the responding entity is not in the SELECTED state, the Separate.req is ignored",
// but E37.1 §7.6 narrows it:
// "the Separate.req is valid only in the TCP/IP CONNECTED state and its substates.
// After either initiating or receiving a Separate.req message,
// the entity shall immediately close the TCP/IP connection and transit to the TCP/IP NOT CONNECTED state."
// NOT SELECTED is such a substate, and the clause attaches no state qualifier to "receiving".
// Ignoring it left a peer that announced its exit but held the socket open to be reaped by the T7 dwell instead of at once.
//
// §7.6's "TCP/IP CONNECTED state and its substates" precondition needs no FSM check:
// this runs on the recv loop, which only exists for a generation whose socket is up.
//
// The genCtx check is the C1 straggler guard, byte-for-byte the same one recvLoop applies to read errors.
// TCPDown resolves the CURRENT epoch and supervisor at call time (see connection.TCPDown),
// so a Separate read by a generation whose teardown already began — a voluntary Close,
// or a straggler that outlived a bounded Stop — would otherwise inject evDisconnect into a SUCCESSOR generation
// and knock it out of NotSelected.
// Honoring §7.6 in every substate is what made this reachable at all:
// before, only a Selected-state Separate reached TCPDown.
//
// Be precise about what this buys, because an earlier revision of this comment over-claimed it.
// The check NARROWS the window; it is not an epoch barrier.
// Cancellation can still land between the check and the TCPDown call,
// so a sufficiently descheduled straggler can still inject into a successor.
// Closing that hole needs the queued FSM event to carry epoch identity and be revalidated at PROCESSING time,
// the pattern supervisor.requestClose already uses via closeEpoch.
// That is a core change affecting all seven TCPDown/T7Expired producers, not just this one.
// It is recorded as pre-existing architecture debt in docs/specs/e37-1-hsms-ss-conformance-audit.md, Gap 2.
func (t *transport) handleSeparateReq(genCtx context.Context) bool {
	t.metrics.incSeparateRecv()

	if genCtx != nil && genCtx.Err() != nil {
		return false
	}

	t.rt.TCPDown(errPeerSeparate)

	return false
}

// sendReject builds a typed Reject.req for a frame with an unsupported PType/SType or a
// malformed control length, and routes it through the core's async send channel (J3, §7.10.3 —
// a Reject keeps the link; it never tears down).
//
// Why rt.SendAsync and not a direct t.Write call:
//
// Go's internal/poll holds the fd write-lock across the whole Writev syscall, so concurrent
// t.Write calls are byte-atomic and there is no wire-corruption risk. The reason to serialize
// via rt.SendAsync is threefold:
//
//  1. transport.Write's documented contract requires the core to serialize all sends under
//     epoch.writeMu (see Write doc). A direct t.Write from the recv goroutine breaks that
//     single-writer invariant and relies on an undocumented runtime detail.
//  2. writeFarewellSeparate performs SetWriteDeadline→Write→clear under writeMu; a recv-path
//     Reject that bypasses writeMu is not fenced out of that shared write-deadline window.
//  3. t.Write is synchronous and deadline-less, so on a wedged peer it stalls the recv loop
//     indefinitely; rt.SendAsync enqueues and returns immediately, bounded by sendCh/genctx.
//
// A non-zero PType is reported as PType-not-supported; otherwise the reason is SType-not-supported.
// The E37 reason set (§7.10, Table 9) has no dedicated "malformed control length" code,
// so a valid-SType-but-wrong-length frame maps here too.
//
// The returned error (ErrNotOpen / ErrConnClosed) is intentionally ignored:
// both mean the generation is already tearing down and the Reject is irrelevant.
// The recv loop must not block on it.
//
// SessionID is hsms.ControlSessionID, NOT the rejected frame's — see the Reject SessionID note below.
func (t *transport) sendReject(frame []byte, pType, sType byte) {
	var systemBytes [4]byte
	copy(systemBytes[:], frame[6:10])

	reason := byte(hsms.RejectSTypeNotSupported)
	if pType != 0 {
		reason = hsms.RejectPTypeNotSupported
	}

	reject, err := hsms.NewRejectReqRaw(hsms.ControlSessionID, pType, sType, systemBytes, reason)
	if err != nil {
		return // unreachable: reason is always RejectSTypeNotSupported or RejectPTypeNotSupported, both non-zero
	}

	// Fire-and-forget: enqueue on the core's sendCh → drainSendCh → writeFrame under writeMu.
	// Control messages are NOT B1-gated, so this always enqueues while the generation is live.
	t.metrics.incRejectSent()
	_ = t.rt.SendAsync(context.Background(), reject)
}

// Reject SessionID (E37.1 §8.1 vs E37 §8.3.21.1) — why all three senders pass hsms.ControlSessionID.
//
// E37 generic §8.3.21.1 says a Reject.req's SessionID is "Equal to the value of the Session ID in the message being rejected",
// and E37's §8.3 summary table does NOT mark that field with the "*" it uses elsewhere to flag values a subsidiary standard may further specify.
// (Deselect.req and Separate.req ARE so marked.)
// Read alone, that says echo the offending frame's ID.
//
// E37.1 §8.1 overrides it for HSMS-SS, without exception or carve-out:
// "In HSMS-SS Control Messages, Session ID will always assume the special value 0xFFFF (all one bits)."
// A Reject.req is a control message, so it takes 0xFFFF like every other one.
// The subsidiary standard governs where the two disagree.
// That is the entire lesson of this package's E37.1 audit (docs/specs/e37-1-hsms-ss-conformance-audit.md),
// and Reject was the last site still following the generic rule.
//
// The cost is that a Reject no longer carries the rejected message's device ID.
// Correlation is unaffected: System Bytes are echoed verbatim (§8.3.21.4) and are what RouteReply matches on,
// and header byte 2 still echoes the offending PType/SType (§8.3.21.2).
//
// The generic constructors hsms.NewRejectReq / NewRejectReqRaw deliberately keep taking an arbitrary
// SessionID — they implement E37, not the HSMS-SS profile, so the profile value is applied here at the
// HSMS-SS call sites rather than baked into the shared message layer.

// sendRejectNotSelected answers a DATA frame received while the link is not Selected with a Reject.req carrying reason 4,
// keeping the link UP (RejectNotSelected, E37 §7.10.3 / spec §6.3).
// Like sendReject it routes through rt.SendAsync (the serialized core send path) rather than a direct recv-path Write.
// The System Bytes are echoed from the offending frame; PType / SType are 0 for a data message.
// The returned error is intentionally ignored
// (a tearing-down generation makes the Reject irrelevant; the recv loop must not block on it).
func (t *transport) sendRejectNotSelected(frame []byte) {
	var systemBytes [4]byte
	copy(systemBytes[:], frame[6:10])

	reject, err := hsms.NewRejectReqRaw(hsms.ControlSessionID, 0, 0, systemBytes, hsms.RejectNotSelected)
	if err != nil {
		return // unreachable: RejectNotSelected is a fixed non-zero constant
	}

	t.metrics.incRejectSent()
	_ = t.rt.SendAsync(context.Background(), reject)
}

// sendRejectTransactionNotOpen answers an ORPHAN control RESPONSE — a Select.rsp / Deselect.rsp /
// Linktest.rsp (even SType) that correlates to no open transaction — with a Reject.req carrying
// reason 3 (RejectTransactionNotOpen). E37 §8.3.20 REQUIRES this: "[Reject.req] must be used when an
// entity receives a control message which is a response (even numbered SType) for which there was no
// corresponding open transaction." The offending response's SType is echoed into header byte 2
// (§8.3.21.2) and its System Bytes into bytes 6–9. Like the other Rejects it routes through the
// serialized core send path and keeps the link UP (a Reject never tears down). An inbound Reject.req
// (SType 7, odd — itself NOT a response) is never re-rejected; the recv loop drops an orphan Reject.
func (t *transport) sendRejectTransactionNotOpen(frame []byte) {
	sType := frame[5]

	var systemBytes [4]byte
	copy(systemBytes[:], frame[6:10])

	reject, err := hsms.NewRejectReqRaw(hsms.ControlSessionID, 0, sType, systemBytes, hsms.RejectTransactionNotOpen)
	if err != nil {
		return // unreachable: RejectTransactionNotOpen is a fixed non-zero constant
	}

	t.metrics.incRejectSent()
	_ = t.rt.SendAsync(context.Background(), reject)
}

// handleLinktestReq answers an inbound Linktest.req with a Linktest.rsp (E37 §7.8.2), keeping the link up.
// It runs on the recv goroutine and routes the rsp through the core's serialized async send path
// (same single-writer rationale as sendReject).
//
// The answer is UNCONDITIONAL — NotSelected included — and that is a deliberate interoperability policy, not an oversight.
// E37.1 §7.4 restricts the use of Linktest to the SELECTED state (Table 3 scopes the whole transaction, not just the request),
// and §7.7 says a violation of a §7 restriction is a communications failure.
// Our INITIATOR side honors that:
// startLinktest / stopLinktest bracket the Selected window, so we never probe outside it.
//
// What we decline to do is punish the PEER for it.
// Two things make the lenient response defensible.
// E37 §7.8.2's responder procedure is unconditional ("receives the Linktest.req … sends a Linktest.rsp").
// And E37 §9.1.1's remedy for a communications failure is that the entity "should terminate the TCP/IP connection" — should, not shall.
// So §7.7 governs how the request is CLASSIFIED, and leaves the consequence to the implementation.
//
// Answering costs a response and has no FSM effect:
// nothing here transitions state, and T7 keeps running,
// so a peer cannot hold an unselected session open by probing — it must still complete Select before the dwell expires.
// Refusing instead (the strict reading) would drop a live TCP connection whenever a peer probes during the NotSelected window,
// and a peer implemented against E37 generic §7.8 — which permits probing anytime in CONNECTED — legitimately does.
// That trade is what this comment records; do not "fix" it into a disconnect.
// See docs/specs/e37-1-hsms-ss-conformance-audit.md, Gap 3, and TestLinktest_InboundReqAnsweredWhileNotSelected.
func (t *transport) handleLinktestReq(msg hsms.Message) {
	cm, ok := msg.(*hsms.ControlMessage)
	if !ok {
		return // unreachable: decodeControlFrame yields *ControlMessage for control STypes
	}

	rsp, err := hsms.NewLinktestRsp(cm)
	if err != nil {
		return // unreachable: dispatchFrame guarantees a well-formed Linktest.req
	}

	t.metrics.incLinktestReqRecv()
	_ = t.rt.SendAsync(context.Background(), rsp)
}

// handleDeselectReq is the responder-only Deselect path (D5a-4, E37.1 §7.3, §7.7).
// HSMS-SS uses Separate for teardown; we NEVER initiate an outbound Deselect.req.
// Runs on the recv goroutine.
//
// Answering an inbound Deselect.req at all is a deliberate deviation, not an oversight.
// E37.1 §7.3 states "Deselect shall not be used" for HSMS-SS,
// and §7.7 makes any violation of a §7 restriction a communications failure —
// so a peer that sends Deselect is, strictly, in violation.
//
// The reason to answer rather than refuse is interoperability, not T6.
// A peer implemented against E37 generic legitimately sends Deselect —
// §7.3's prohibition is an HSMS-SS narrowing the peer may not implement —
// and answering keeps an otherwise-usable peer selected-capable
// without forcing a TCP teardown and reconnect cycle.
// This is not a T6-stranding trade: E37 §9.1.1's remedy for a communications failure
// is that the entity "should terminate the TCP/IP connection" — should, not shall —
// and that close would release the peer sooner than any dwell timer would.
// Refusing was never the only way to avoid stranding it; the choice here is purely about
// keeping a usable link up rather than dropping it over a procedure violation.
//
// If currently Selected: reply Deselect.rsp status 0 (success) and transition
// Selected->NotSelected (rt.SelectLost) + stop the auto-linktest.
// If NOT Selected: reply a non-zero (NotEstablished) status and do NOT transition.
// See docs/specs/e37-1-hsms-ss-conformance-audit.md, "Deviations reviewed and accepted", §7.3,
// and TestDeselect_AnsweredDespiteE371Prohibition.
func (t *transport) handleDeselectReq(g *genWG, msg hsms.Message) {
	cm, ok := msg.(*hsms.ControlMessage)
	if !ok {
		return // unreachable
	}

	status := byte(hsms.DeselectStatusSuccess)
	if t.rt.State() != hsms.SelectedState {
		status = hsms.DeselectStatusNotEstablished
	}

	rsp, err := hsms.NewDeselectRsp(cm, status)
	if err != nil {
		return // unreachable: dispatchFrame guarantees a well-formed Deselect.req
	}

	_ = t.rt.SendAsync(context.Background(), rsp)

	if status == hsms.DeselectStatusSuccess {
		t.rt.SelectLost() // Selected -> NotSelected (evSelectLost)
		t.stopLinktest()
		// Back in NotSelected on the SAME TCP connection: the T7 dwell re-applies (§9.2.2), so
		// re-arm it on this generation's bundle g (NEW-1) — if no re-Select follows, T7 expiry
		// drops + reconnects.
		t.armT7(g)
	}
}
