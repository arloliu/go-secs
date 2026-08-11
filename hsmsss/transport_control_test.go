package hsmsss

// transport_control_test.go — pin tests for the two control-message deviations reclassified in
// docs/specs/e37-1-hsms-ss-conformance-audit.md's "Deviations reviewed and accepted" section:
// answering an inbound Deselect.req despite E37.1 §7.3's "Deselect shall not be used", and the
// Select responder accepting (and echoing) a non-conformant inbound control SessionID.
//
// The file lives in package hsmsss (not hsmsss_test) because the Deselect test drives the
// unexported *transport / recRT doubles shared with transport_procedures_test.go; the SessionID
// test reuses the loopback-TCP helpers from integration_control_session_id_test.go, which live in
// the same package for the identical reason.

import (
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/stretchr/testify/require"
)

// TestDeselect_AnsweredDespiteE371Prohibition pins the deviation named in
// docs/specs/e37-1-hsms-ss-conformance-audit.md, "Deviations reviewed and accepted", §7.3:
// E37.1 §7.3 states "Deselect shall not be used" for HSMS-SS, yet handleDeselectReq answers an
// inbound Deselect.req received while Selected with status 0 (success) and transitions
// Selected -> NotSelected, rather than refusing it.
//
// This is the deviation's named pin — see the rationale comment at handleDeselectReq for why it
// is kept, and TestDeselect_WhileSelectedRepliesSuccessAndTransitions /
// TestDeselect_WhileNotSelectedRepliesFailureNoTransition (transport_procedures_test.go) for the
// mechanical coverage of both status branches and the auto-linktest stop.
func TestDeselect_AnsweredDespiteE371Prohibition(t *testing.T) {
	t.Parallel()

	rt := newRecRT()
	rt.setState(hsms.SelectedState)

	tr := newLinktestTransport(t, rt, t.Context())

	req := hsms.NewDeselectReq(0xFFFF, rt.NextSystemBytes())
	tr.handleDeselectReq(tr.wg, req)

	got := rt.lastSent()
	require.NotNil(t, got, "an inbound Deselect.req while Selected must be answered, not refused")
	require.Equal(t, hsms.DeselectRspType, got.Type(), "the answer must be a Deselect.rsp")
	require.Equal(t, byte(hsms.DeselectStatusSuccess), got.HeaderBytes()[3],
		"status must be 0 (success) despite E37.1 §7.3's prohibition on Deselect")
	require.Equal(t, hsms.NotSelectedState, rt.State(),
		"answering must still transition Selected -> NotSelected (rt.SelectLost)")
}

// TestSelectRsp_EchoesNonConformantSessionID pins BOTH halves of the "§8.1 echo" deviation named
// in docs/specs/e37-1-hsms-ss-conformance-audit.md, "Deviations reviewed and accepted":
//
// E37 §8.3.7.1 requires a Select.rsp to echo the Select.req's SessionID.
// E37.1 §8.1 requires every HSMS-SS control message — Select.req included — to carry 0xFFFF.
// The two cannot both be satisfied once a peer sends a non-0xFFFF Select.req: E37.1 says the
// request itself should never have carried anything else, while E37 says the response must carry
// back whatever the request carried.
// We follow E37, which binds the response to the request, rather than second-guessing the
// request's own conformance.
//
// TestPassive_SelectRspMirrorsRequestSessionID (integration_control_session_id_test.go) already
// pins the echo.
// This test pins the SAME resolution from the other side, with the acceptance half
// asserted directly (status 0, not merely an eventual state) rather than incidentally:
// the Select responder does not refuse a non-conformant SessionID either — it commits
// NotSelected -> Selected exactly as it would for a conformant 0xFFFF request.
func TestSelectRsp_EchoesNonConformantSessionID(t *testing.T) {
	t.Parallel()

	const probeSessionID uint16 = 0x1234 // non-conformant: neither 0xFFFF nor testDeviceID

	port := freeLoopbackPort(t)
	conn := newPassiveConn(t, port, hsms.WithSessionID(testDeviceID), hsms.WithLinktestInterval(0))
	require.NoError(t, conn.Open(t.Context(), hsms.OpenBackground))
	t.Cleanup(func() { _ = conn.Close() })

	peer := dialPassive(t, port)
	t.Cleanup(func() { _ = peer.Close() })

	_, err := peer.Write(selectReqFrameWithSessionID(probeSessionID, [4]byte{0x11, 0x22, 0x33, 0x44}))
	require.NoError(t, err)

	rspPayload, err := peerReadFrame(peer, 5*time.Second)
	require.NoError(t, err, "peer must read our Select.rsp")
	require.Equal(t, byte(hsms.SelectRspType), rspPayload[5])

	// Half 1: ACCEPT — status 0 (Communication Established), not refused for the bad SessionID.
	require.Equal(t, byte(hsms.SelectStatusSuccess), rspPayload[3],
		"a non-conformant SessionID must not cause the Select to be refused")
	require.Eventually(t, func() bool { return conn.State() == hsms.SelectedState },
		5*time.Second, 5*time.Millisecond,
		"the responder must commit Selected despite the non-conformant SessionID")

	// Half 2: ECHO — the response mirrors the request's SessionID (E37 §8.3.7.1) instead of
	// substituting 0xFFFF (E37.1 §8.1) or the configured device ID.
	require.Equal(t, probeSessionID, frameSessionID(t, rspPayload),
		"Select.rsp must echo the Select.req SessionID even though the request itself violates E37.1 §8.1")
}
