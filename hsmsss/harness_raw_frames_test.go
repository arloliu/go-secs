package hsmsss

// raw_control_frames_test.go — raw HSMS control-frame builders shared by the protocol-edge
// regression suite (simultaneous_select_test.go, reject_test.go). These author the on-wire
// control frames that the existing helper vocabulary (selectReqFrame / selectRspFrame /
// dataFrame in active_test.go) does not cover: a Reject.req (SType 7) and an arbitrary
// header-only control frame (used here for an UNSOLICITED / orphan control response).
//
// Every builder returns a FULLY FRAMED byte slice (4-byte big-endian length prefix + 10-byte
// header) via frameBytes, so a raw peer can conn.Write(...) it directly, symmetric with
// selectReqFrame / dataFrame. Control frames are header-only (E37 §9.3.3.1): message length is
// always exactly 10 and there is no body. The session ID is fixed to 0xFFFF, the HSMS-SS
// single-session control-frame convention (E37.1 §3).

import (
	"encoding/binary"

	"github.com/arloliu/go-secs/v2/hsms"
)

// buildRejectFrame builds a fully-framed HSMS Reject.req (SType 7, E37 §7.9 / §8.3.20).
//
// Header layout matches hsms.NewRejectReqRaw / hsms.NewRejectReq so a v2 endpoint correlates it:
//   - header[2] = rejectedSTypeOrPType — the SType of the rejected message (or its PType when
//     reason == RejectPTypeNotSupported=2). For a rejected DATA message this is 0.
//   - header[3] = reason — one of hsms.RejectSTypeNotSupported(1) / RejectPTypeNotSupported(2) /
//     RejectTransactionNotOpen(3) / RejectNotSelected(4).
//   - header[6:10] = sb — the System Bytes of the rejected message, VERBATIM, so an inbound
//     Reject.req routes to the blocked sender's pending transaction by System Bytes (T19 /
//     RouteReply).
func buildRejectFrame(rejectedSTypeOrPType byte, reason byte, sb [4]byte) []byte {
	h := make([]byte, 10)
	binary.BigEndian.PutUint16(h[0:2], 0xFFFF) // control-frame session ID (E37.1 §3)
	h[2] = rejectedSTypeOrPType                // echoes rejected SType (or PType for reason 2)
	h[3] = reason
	h[4] = 0 // PType — always 0 for a control frame
	h[5] = byte(hsms.RejectReqType)
	copy(h[6:10], sb[:])

	return frameBytes(10, h, nil)
}

// buildControlFrame builds a fully-framed header-only HSMS control frame with the given SType and
// header[3] (the status/reason byte — e.g. a Select.rsp select-status). It is used to synthesize
// an UNSOLICITED control response (a control frame with no matching open transaction), which the
// existing selectRspFrame helper cannot express because that helper derives its System Bytes from
// a real Select.req header. header[4] (PType) is 0 and the session ID is 0xFFFF (E37.1 §3).
// header3 is kept explicit for caller documentation (a Select.rsp select-status / Deselect.rsp status)
// even though every current call site passes 0 (SelectStatusSuccess / an unused status byte).
//
//nolint:unparam // header3 kept explicit for caller documentation; all current call sites pass 0.
func buildControlFrame(sType byte, header3 byte, sb [4]byte) []byte {
	h := make([]byte, 10)
	binary.BigEndian.PutUint16(h[0:2], 0xFFFF)
	h[3] = header3
	h[5] = sType
	copy(h[6:10], sb[:])

	return frameBytes(10, h, nil)
}
