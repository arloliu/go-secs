package hsmsss

import (
	"context"
	"errors"
	"net"

	"github.com/arloliu/go-secs/v2/hsms"
)

// errPeerSeparate is the TCPDown cause used when a peer Separate.req (SType 9) arrives on a live generation (E37.1 §7.6).
// Routing the teardown through TCPDown marks the current generation commsFailure=true, which suppresses OUR farewell Separate —
// the peer already signalled it is leaving, so answering with a Separate is neither required nor correct.
var errPeerSeparate = errors.New("hsmsss: peer sent Separate.req")

// causeRuntime is the optional capability a runtime offers to accept the TransitionCause of an involuntary disconnect alongside the error TCPDown already carries.
// It is reached by type assertion, deliberately NOT through hsms.TransportRuntime:
// widening that exported interface would break external implementers, the same reason suppressionRuntime exists.
// A runtime that lacks the capability still gets the plain TCPDown, and its subscribers see hsms.CauseUnknown.
type causeRuntime interface {
	TCPDownWithCause(cause error, transitionCause hsms.TransitionCause)
}

// genRuntime is the optional capability a runtime offers to accept the identity of the generation a report belongs to,
// so a report arriving out of a generation that has already ended is discarded rather than applied to its successor.
// It is reached by type assertion for the same reason causeRuntime is.
//
// A generation's goroutines can outlive the generation itself:
// the teardown join is bounded, so one wedged past the close timeout is abandoned and may resume long afterwards.
// Every ctx check in this package narrows that window but cannot close it —
// cancellation can always land between the check and the call.
// Naming the generation moves the decision to the point where the event is applied to the FSM,
// where it cannot go stale.
//
// A runtime without the capability keeps the previous behavior:
// reports resolve whatever generation is current when they land.
type genRuntime interface {
	CurrentGeneration() uint64
	TCPUpFromGeneration(gen uint64, conn net.Conn) bool
	TCPDownFromGeneration(gen uint64, cause error, transitionCause hsms.TransitionCause)
	CommitSelectedFromGeneration(gen uint64) bool
	SelectLostFromGeneration(gen uint64) bool
	T7ExpiredFromGeneration(gen uint64)
	SendAsyncFromGeneration(ctx context.Context, gen uint64, msg hsms.Message) error
	WriteMessageFromGeneration(ctx context.Context, gen uint64, msg hsms.Message) (hsms.Message, error)
}

// currentGeneration reads the runtime's live generation identity, or 0 when the runtime does not offer one.
// Start calls it once per generation and stamps the answer on that generation's WaitGroup bundle,
// which is how every goroutine this transport spawns knows which generation it speaks for.
func (t *transport) currentGeneration() uint64 {
	if gr, ok := t.rt.(genRuntime); ok {
		return gr.CurrentGeneration()
	}

	return 0
}

// tcpDown reports an involuntary disconnect, naming its TransitionCause when the runtime accepts one.
// Every TCPDown producer in this package goes through it,
// so the cause is chosen at the site that knows why the link is going down.
//
// gen is the reporting generation (genWG.gen), so a report from a generation that has already ended is discarded.
// Pass 0 only where no generation bundle is in scope.
func (t *transport) tcpDown(gen uint64, cause error, transitionCause hsms.TransitionCause) {
	if gr, ok := t.rt.(genRuntime); ok {
		gr.TCPDownFromGeneration(gen, cause, transitionCause)

		return
	}

	if cr, ok := t.rt.(causeRuntime); ok {
		cr.TCPDownWithCause(cause, transitionCause)

		return
	}

	t.rt.TCPDown(cause)
}

// tcpUp reports an established socket for gen, the generation that established it,
// and reports whether the core accepted it onto a live generation.
// The passive accept goroutine
// (and, more narrowly, an active dial racing a concurrent teardown to completion)
// can be abandoned by a bounded Stop between the accept/dial and this call,
// so the generation is named for the same reason tcpDown names it.
//
// On false the caller owns conn —
// no epoch will ever take ownership of a refused socket —
// and must close it itself,
// skipping everything else a live TCP-up would otherwise start
// (the recv loop, the active Select procedure, activity-stamp bookkeeping).
// A runtime without the genRuntime capability offers no way to report a refusal,
// so this always reports true for it, preserving the pre-generation behavior.
func (t *transport) tcpUp(gen uint64, conn net.Conn) bool {
	if gr, ok := t.rt.(genRuntime); ok {
		return gr.TCPUpFromGeneration(gen, conn)
	}

	t.rt.TCPUp(conn)

	return true
}

// commitSelected commits the FSM to Selected for gen, the generation whose recv goroutine completed the handshake.
// A recv goroutine abandoned by a bounded Stop can process a Select frame long after its own generation ended,
// so naming the generation is what keeps that commit off the successor's FSM.
func (t *transport) commitSelected(gen uint64) bool {
	if gr, ok := t.rt.(genRuntime); ok {
		return gr.CommitSelectedFromGeneration(gen)
	}

	return t.rt.CommitSelected()
}

// selectLost reports the loss of Selected for gen, the generation whose recv goroutine answered the Deselect.req,
// and reports whether the core applied the transition.
// It names the generation for the same reason commitSelected does, and from the same goroutine.
//
// On false the deselect did NOT happen — gen has already ended, or the link was not Selected —
// and the caller must skip everything that belongs to a real Selected -> NotSelected transition
// (stopping the auto-linktest, arming the T7 dwell),
// because those act on whatever generation is CURRENT, not on gen.
// A runtime without the genRuntime capability offers no way to report a refusal,
// so this reports true for it, preserving the pre-generation behavior — the same fallback tcpUp makes.
func (t *transport) selectLost(gen uint64) bool {
	if gr, ok := t.rt.(genRuntime); ok {
		return gr.SelectLostFromGeneration(gen)
	}

	t.rt.SelectLost()

	return true
}

// t7Expired reports the NOT-SELECTED dwell expiry for gen, the generation whose dwell it was.
// A dwell belonging to a generation that has ended must not drop its successor,
// so it goes through the same generation-naming capability tcpDown uses.
func (t *transport) t7Expired(gen uint64) {
	if gr, ok := t.rt.(genRuntime); ok {
		gr.T7ExpiredFromGeneration(gen)

		return
	}

	t.rt.T7Expired()
}

// sendResponse enqueues an answer this transport owes the peer — a control response or a Reject.req —
// on the send queue of gen, the generation whose recv goroutine read the request being answered.
//
// Every one of these is written from the recv path, which a bounded Stop can abandon,
// so the plain SendAsync (which resolves the live generation at call time) would put a straggler's answer,
// carrying the ORIGINAL request's System Bytes, onto the successor's socket.
// The peer never opened that transaction on the successor's association,
// and no later refusal can retract a frame already queued.
// The core drops such a send instead; see hsms's SendAsyncFromGeneration.
//
// The response is deliberately not made conditional on anything ELSE the handler decides.
// Binding it to the generation is what makes it safe, and it keeps a live generation's answer unconditional,
// which is what the responder procedures require (§7.8.2, §8.3.20).
//
// The error is ignored at every call site, as it was before: ErrNotOpen / ErrConnClosed / a refused generation
// all mean the answer has nowhere to go, and the recv loop must not block on it.
// A runtime without the genRuntime capability keeps the unqualified send.
func (t *transport) sendResponse(gen uint64, msg hsms.Message) error {
	if gr, ok := t.rt.(genRuntime); ok {
		return gr.SendAsyncFromGeneration(context.Background(), gen, msg)
	}

	return t.rt.SendAsync(context.Background(), msg)
}

// writeMessage issues a synchronous REQUEST this transport initiates — the active Select.req, the auto-linktest probe —
// on behalf of gen, the generation whose procedure goroutine is sending.
//
// Unlike a response, a request opens a transaction: it registers a reply channel and then waits on it.
// Both halves belong to gen's own epoch, which is what the generation-named core entry point guarantees;
// the unqualified WriteMessage would open the transaction on whatever generation is current instead.
// On a refusal it reports hsms.ErrConnClosed, which both procedures already read as their own generation's death.
// A runtime without the genRuntime capability keeps the unqualified send.
func (t *transport) writeMessage(ctx context.Context, gen uint64, msg hsms.Message) (hsms.Message, error) {
	if gr, ok := t.rt.(genRuntime); ok {
		return gr.WriteMessageFromGeneration(ctx, gen, msg)
	}

	return t.rt.WriteMessage(ctx, msg)
}

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
		t.handleLinktestReq(g, msg)
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
	if t.commitSelected(g.gen) {
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
	// It is enqueued on THIS generation's queue, so a straggler's answer can never reach the successor's peer.
	_ = t.sendResponse(g.gen, rsp)
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
// Two guards keep this teardown inside the generation that read the Separate.
//
// The genCtx check is the straggler guard, byte-for-byte the same one recvLoop applies to read errors:
// a generation whose teardown already began owns no disconnect — that is a voluntary Close,
// or a goroutine abandoned by a bounded Stop — so it returns without reporting one.
// It is an early exit, not a barrier: cancellation can land between the check and the call below.
//
// The barrier is g.gen, the identity of the generation this recv goroutine belongs to.
// It travels with the disconnect all the way to the FSM and is matched against the live generation at the moment the event is APPLIED,
// so a Separate read by a generation that has since ended can no longer disconnect its successor
// or report CausePeerSeparate to that successor's lifecycle subscribers.
// See hsms.connection.TCPDownFromGeneration and hsms.supervisor.step.
//
// Honoring §7.6 in every substate is what made the hazard reachable from this site at all:
// before, only a Selected-state Separate reported a disconnect.
func (t *transport) handleSeparateReq(genCtx context.Context, g *genWG) bool {
	t.metrics.incSeparateRecv()

	if genCtx != nil && genCtx.Err() != nil {
		return false
	}

	t.tcpDown(g.gen, errPeerSeparate, hsms.CausePeerSeparate)

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
func (t *transport) sendReject(g *genWG, frame []byte, pType, sType byte) {
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
	// RejectSentCount is documented as Rejects EMITTED, so a response refused because this generation
	// has ended is not one: count only what the core accepted.
	if t.sendResponse(g.gen, reject) == nil {
		t.metrics.incRejectSent()
	}
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
func (t *transport) sendRejectNotSelected(g *genWG, frame []byte) {
	var systemBytes [4]byte
	copy(systemBytes[:], frame[6:10])

	reject, err := hsms.NewRejectReqRaw(hsms.ControlSessionID, 0, 0, systemBytes, hsms.RejectNotSelected)
	if err != nil {
		return // unreachable: RejectNotSelected is a fixed non-zero constant
	}

	if t.sendResponse(g.gen, reject) == nil {
		t.metrics.incRejectSent()
	}
}

// sendRejectTransactionNotOpen answers an ORPHAN control RESPONSE — a Select.rsp / Deselect.rsp /
// Linktest.rsp (even SType) that correlates to no open transaction — with a Reject.req carrying
// reason 3 (RejectTransactionNotOpen). E37 §8.3.20 REQUIRES this: "[Reject.req] must be used when an
// entity receives a control message which is a response (even numbered SType) for which there was no
// corresponding open transaction." The offending response's SType is echoed into header byte 2
// (§8.3.21.2) and its System Bytes into bytes 6–9. Like the other Rejects it routes through the
// serialized core send path and keeps the link UP (a Reject never tears down). An inbound Reject.req
// (SType 7, odd — itself NOT a response) is never re-rejected; the recv loop drops an orphan Reject.
func (t *transport) sendRejectTransactionNotOpen(g *genWG, frame []byte) {
	sType := frame[5]

	var systemBytes [4]byte
	copy(systemBytes[:], frame[6:10])

	reject, err := hsms.NewRejectReqRaw(hsms.ControlSessionID, 0, sType, systemBytes, hsms.RejectTransactionNotOpen)
	if err != nil {
		return // unreachable: RejectTransactionNotOpen is a fixed non-zero constant
	}

	if t.sendResponse(g.gen, reject) == nil {
		t.metrics.incRejectSent()
	}
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
func (t *transport) handleLinktestReq(g *genWG, msg hsms.Message) {
	cm, ok := msg.(*hsms.ControlMessage)
	if !ok {
		return // unreachable: decodeControlFrame yields *ControlMessage for control STypes
	}

	rsp, err := hsms.NewLinktestRsp(cm)
	if err != nil {
		return // unreachable: dispatchFrame guarantees a well-formed Linktest.req
	}

	// LinktestReqRecvCount is documented as probes ANSWERED, so a rsp the core refused
	// (this generation has ended) is not one.
	if t.sendResponse(g.gen, rsp) == nil {
		t.metrics.incLinktestReqRecv()
	}
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
//
// The transition's SIDE EFFECTS are gated on the commit actually being applied, not on the status alone.
// stopLinktest and armT7 both act on whatever generation is current —
// they cancel t.linktestCancel and derive a T7 ctx from t.genCtx —
// so a recv goroutine that outlived its own generation would cancel the SUCCESSOR's auto-linktest
// and hang a stale dwell on it, leaving a link that is still logically Selected with no liveness probing at all.
// selectLost reports the core's refusal precisely so that work can be skipped.
//
// The response is still enqueued BEFORE the commit, and that ordering is deliberate.
// The §7.D/I3 invariant is satisfied by the commit being SYNCHRONOUS on the recv goroutine,
// not by its position relative to the rsp:
// this loop reads frames sequentially, so the CAS lands before any re-Select.req the peer pipelines is ever dispatched,
// and SendAsync only enqueues the rsp anyway.
// Deriving the status from the commit instead would also break the runtimes that cannot report a refusal:
// on the plain SelectLost path selectLost always reports true,
// so a Deselect answered while NOT Selected would be told status 0 and handed a SelectLost it never asked for.
// The status therefore stays a statement about the link the peer sees, which is what State() answers.
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

	_ = t.sendResponse(g.gen, rsp)

	// Selected -> NotSelected (evSelectLost), isolated to THIS generation.
	// On a refusal the transition did not happen, so neither may anything downstream of it.
	if status == hsms.DeselectStatusSuccess && t.selectLost(g.gen) {
		t.stopLinktest()
		// Back in NotSelected on the SAME TCP connection: the T7 dwell re-applies (§9.2.2), so
		// re-arm it on this generation's bundle g (NEW-1) — if no re-Select follows, T7 expiry
		// drops + reconnects.
		t.armT7(g)
	}
}
