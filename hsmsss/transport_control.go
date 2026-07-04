package hsmsss

import (
	"context"
	"encoding/binary"
	"errors"

	"github.com/arloliu/go-secs/v2/hsms"
)

// errPeerSeparate is the TCPDown cause used when a peer Separate.req (SType 9) arrives while
// the link is Selected (E37 §7.9.2). Routing the teardown through TCPDown marks the current
// generation commsFailure=true, which suppresses OUR farewell Separate — the peer already
// signalled it is leaving, so answering with a Separate is neither required nor correct.
var errPeerSeparate = errors.New("hsmsss: peer sent Separate.req while selected")

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

// handleSeparateReq processes an inbound Separate.req (SType 9, E37 §7.9.2). While Selected
// the peer is closing the session: drive an involuntary disconnect via rt.TCPDown, whose
// commsFailure marking suppresses OUR farewell Separate (§9.1.1) and whose evDisconnect
// injection funnels teardown through the single NotConnected reaction. It returns false so
// the recv loop ends without a redundant TCPDown. While NOT Selected the Separate is ignored
// (no state to tear down; no Separate is sent back) and it returns true to keep reading.
func (t *transport) handleSeparateReq() bool {
	if t.rt.State() == hsms.SelectedState {
		t.metrics.incSeparateRecv()
		t.rt.TCPDown(errPeerSeparate)
		return false
	}

	return true
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
// A non-zero PType is reported as PType-not-supported; otherwise the reason is
// SType-not-supported (the E37 reason set, §7.9, has no dedicated "malformed control length"
// code, so a valid-SType-but-wrong-length frame maps here too).
//
// The returned error (ErrNotOpen / ErrConnClosed) is intentionally ignored: both mean the
// generation is already tearing down and the Reject is irrelevant. The recv loop must not
// block on it.
func (t *transport) sendReject(frame []byte, pType, sType byte) {
	sessionID := binary.BigEndian.Uint16(frame[0:2])

	var systemBytes [4]byte
	copy(systemBytes[:], frame[6:10])

	reason := byte(hsms.RejectSTypeNotSupported)
	if pType != 0 {
		reason = hsms.RejectPTypeNotSupported
	}

	reject := hsms.NewRejectReqRaw(sessionID, pType, sType, systemBytes, reason)

	// Fire-and-forget: enqueue on the core's sendCh → drainSendCh → writeFrame under writeMu.
	// Control messages are NOT B1-gated, so this always enqueues while the generation is live.
	t.metrics.incRejectSent()
	_ = t.rt.SendAsync(context.Background(), reject)
}

// sendRejectNotSelected answers a DATA frame received while the link is not Selected with a
// Reject.req carrying reason 4 (RejectNotSelected, E37 §7.10.3 / spec §6.3), keeping the link
// UP. Like sendReject it routes through rt.SendAsync (the serialized core send path) rather
// than a direct recv-path Write. The System Bytes are echoed from the offending frame; PType /
// SType are 0 for a data message. The returned error is intentionally ignored (a tearing-down
// generation makes the Reject irrelevant; the recv loop must not block on it).
func (t *transport) sendRejectNotSelected(frame []byte) {
	sessionID := binary.BigEndian.Uint16(frame[0:2])

	var systemBytes [4]byte
	copy(systemBytes[:], frame[6:10])

	reject := hsms.NewRejectReqRaw(sessionID, 0, 0, systemBytes, hsms.RejectNotSelected)

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
	sessionID := binary.BigEndian.Uint16(frame[0:2])
	sType := frame[5]

	var systemBytes [4]byte
	copy(systemBytes[:], frame[6:10])

	reject := hsms.NewRejectReqRaw(sessionID, 0, sType, systemBytes, hsms.RejectTransactionNotOpen)

	t.metrics.incRejectSent()
	_ = t.rt.SendAsync(context.Background(), reject)
}

// handleLinktestReq answers an inbound Linktest.req with a Linktest.rsp (E37 §7.8), keeping the link
// up. It runs on the recv goroutine and routes the rsp through the core's serialized async send path
// (same single-writer rationale as sendReject).
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

// handleDeselectReq is the responder-only Deselect path (D5a-4, §7.7). HSMS-SS uses Separate for
// teardown; we answer Deselect only far enough not to strand a peer on T6. If currently Selected:
// reply Deselect.rsp status 0 (success) and transition Selected->NotSelected (rt.SelectLost) + stop
// the auto-linktest. If NOT Selected: reply a non-zero (NotEstablished) status and do NOT transition.
// We NEVER initiate an outbound Deselect.req. Runs on the recv goroutine.
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
