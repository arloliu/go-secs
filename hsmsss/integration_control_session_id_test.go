package hsmsss

// integration_control_session_id_test.go — the E37.1 control-frame session-ID regression guard.
//
// SEMI E37.1 §8.1:
// "In HSMS-SS Control Messages, Session ID will always assume the special value 0xFFFF (all one bits)."
// §7.1.1 states it for Select.req and §7.6 for Separate.req.
// The configured session ID (hsms.WithSessionID) is the DEVICE ID,
// and belongs in DATA messages only (§7.2, §8.1).
//
// v2.0.1 regression: the active Select.req went out with the CONFIGURED session ID.
// A host configured with a device ID other than 0xFFFF (e.g. 0 or 1000) had its Select.req rejected by compliant equipment,
// which then closed the socket — an endless NotSelected -> NotConnected reconnect loop.
// v1.17.1 hard-coded 0xFFFF; this file pins that back.

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// testDeviceID is a configured session ID deliberately DIFFERENT from 0xFFFF,
// so a control frame that wrongly stamps the configured value is distinguishable from a correct 0xFFFF frame.
const testDeviceID uint16 = 1000

// frameSessionID returns the session ID (header bytes 0–1, big-endian) of a payload returned by peerReadFrame
// (a 10-byte header + optional body).
func frameSessionID(t *testing.T, payload []byte) uint16 {
	t.Helper()
	require.GreaterOrEqual(t, len(payload), 10, "frame must carry a 10-byte header")

	return binary.BigEndian.Uint16(payload[0:2])
}

// TestActive_SelectAndSeparateUseControlSessionID — with a configured device ID of 1000,
// the active side's Select.req and its farewell Separate.req must both carry 0xFFFF (E37.1 §7.1.1, §7.6),
// while a DATA message on the same connection must carry 1000 (§7.2, §8.1).
//
// The data-message assertion is the counter-assertion that keeps the fix honest:
// without it the test would still pass if the configured session ID were forced to 0xFFFF everywhere.
func TestActive_SelectAndSeparateUseControlSessionID(t *testing.T) {
	t.Parallel()

	ln, port := listenLoopback(t)
	t.Cleanup(func() { _ = ln.Close() })
	peerCh := acceptOneAsync(ln)

	conn := newActiveConn(t, port,
		hsms.WithT6(2*time.Second),
		hsms.WithSessionID(testDeviceID),
		hsms.WithLinktestInterval(0), // no auto-linktest frames interleaving with the reads below
	)
	require.NoError(t, conn.Open(context.Background(), hsms.OpenBackground))
	t.Cleanup(func() { _ = conn.Close() })

	peer := waitPeer(t, peerCh)
	t.Cleanup(func() { _ = peer.Close() })

	// (1) Select.req — E37.1 §7.1.1: SessionID 0xFFFF, never the configured device ID.
	reqPayload, err := peerReadFrame(peer, 5*time.Second)
	require.NoError(t, err, "peer must read our Select.req")
	require.Equal(t, byte(hsms.SelectReqType), reqPayload[5], "active side sends Select.req first")
	require.Equal(t, hsms.ControlSessionID, frameSessionID(t, reqPayload),
		"Select.req must carry SessionID 0xFFFF (E37.1 §7.1.1), not the configured device ID")

	_, err = peer.Write(selectRspFrame(reqPayload[:10], hsms.SelectStatusSuccess))
	require.NoError(t, err)

	require.Eventually(t, func() bool { return conn.State() == hsms.SelectedState },
		5*time.Second, 5*time.Millisecond, "active side must reach Selected")

	// (2) DATA message — E37.1 §7.2 / §8.1: the CONFIGURED device ID, not 0xFFFF.
	//
	// SendDataMessage (not the Async form) is deliberate, and it is a correctness barrier rather than a style preference.
	// SendDataMessageAsync only ENQUEUES,
	// so it establishes no happens-before with the async sender's release of epoch.writeMu.
	// Close's farewell Separate takes that same writeMu with TryLock and SKIPS on contention,
	// so an async send racing Close could legitimately produce no Separate at all and flake step (3).
	// A W-bit-less SendDataMessage returns only once the frame has been written,
	// so by the time Close runs the writer is provably done.
	_, err = conn.SendDataMessage(context.Background(), 1, 1, false, secs2.NewEmptyItem())
	require.NoError(t, err)

	dataPayload, err := peerReadFrame(peer, 5*time.Second)
	require.NoError(t, err, "peer must read our data message")
	require.Equal(t, byte(0), dataPayload[5], "expected an SType 0 data frame")
	require.Equal(t, testDeviceID, frameSessionID(t, dataPayload),
		"a data message must carry the CONFIGURED device ID (E37.1 §7.2)")

	// (3) Farewell Separate.req on a graceful Close from Selected — E37.1 §7.6: always 0xFFFF.
	require.NoError(t, conn.Close())

	sepPayload, err := peerReadFrame(peer, 5*time.Second)
	require.NoError(t, err, "peer must read our farewell Separate.req")
	require.Equal(t, byte(hsms.SeparateReqType), sepPayload[5], "expected a Separate.req on graceful close")
	require.Equal(t, hsms.ControlSessionID, frameSessionID(t, sepPayload),
		"Separate.req must always use SessionID 0xFFFF (E37.1 §7.6)")
}

// selectReqFrameWithSessionID builds a peer-originated Select.req carrying an ARBITRARY session ID.
// The shared selectReqFrame helper hard-codes 0xFFFF, the conformant HSMS-SS value.
// That makes it useless for proving Select.rsp MIRRORS the request,
// rather than merely emitting the same constant we would emit anyway.
func selectReqFrameWithSessionID(sessionID uint16, sb [4]byte) []byte {
	h := make([]byte, 10)
	binary.BigEndian.PutUint16(h[0:2], sessionID)
	h[5] = byte(hsms.SelectReqType)
	copy(h[6:10], sb[:])

	return frameBytes(10, h, nil)
}

// TestPassive_SelectRspMirrorsRequestSessionID — the RESPONDER side is unchanged by the fix and stays spec-correct.
// E37 §8.3.7.1 requires Select.rsp to echo the SessionID of the Select.req.
//
// The probe deliberately uses a NON-conformant request session ID — 0x0042, neither 0xFFFF nor our configured device ID.
// That is the only way the assertion can distinguish mirroring from a hard-coded constant:
// against a conformant 0xFFFF request, a responder that ignored the request entirely would pass too.
// A compliant HSMS-SS peer always sends 0xFFFF (E37.1 §8.1), so 0x0042 is a test instrument, not an endorsement.
func TestPassive_SelectRspMirrorsRequestSessionID(t *testing.T) {
	t.Parallel()

	const probeSessionID uint16 = 0x0042

	port := freeLoopbackPort(t)
	conn := newPassiveConn(t, port, hsms.WithSessionID(testDeviceID), hsms.WithLinktestInterval(0))
	require.NoError(t, conn.Open(context.Background(), hsms.OpenBackground))
	t.Cleanup(func() { _ = conn.Close() })

	peer := dialPassive(t, port)
	t.Cleanup(func() { _ = peer.Close() })

	_, err := peer.Write(selectReqFrameWithSessionID(probeSessionID, [4]byte{0x01, 0x02, 0x03, 0x04}))
	require.NoError(t, err)

	rspPayload, err := peerReadFrame(peer, 5*time.Second)
	require.NoError(t, err, "peer must read our Select.rsp")
	require.Equal(t, byte(hsms.SelectRspType), rspPayload[5])
	require.Equal(t, probeSessionID, frameSessionID(t, rspPayload),
		"Select.rsp must ECHO the Select.req SessionID (E37 §8.3.7.1), not substitute a constant")
	require.NotEqual(t, testDeviceID, frameSessionID(t, rspPayload),
		"Select.rsp must never carry our configured device ID")

	require.Eventually(t, func() bool { return conn.State() == hsms.SelectedState },
		5*time.Second, 5*time.Millisecond, "passive side must reach Selected")
}

// TestReject_UsesControlSessionID — E37.1 §8.1 applies to Reject.req like every other control message,
// which is where E37 generic §8.3.21.1 ("equal to the Session ID in the message being rejected") differs.
//
// All THREE reject senders are covered, each reached by its own trigger,
// because they build their frames independently and a mutation to one would otherwise escape:
// sendRejectNotSelected (data while NotSelected, reason 4),
// sendReject (undefined SType, reason 1),
// and sendRejectTransactionNotOpen (orphan control response, reason 3).
//
// Every case feeds a NON-0xFFFF session ID in the offending frame,
// so an implementation that echoed it would produce a visibly different value.
//
// TEETH: restore `binary.BigEndian.Uint16(frame[0:2])` as any sender's Reject session ID and that subtest fails.
func TestReject_UsesControlSessionID(t *testing.T) {
	t.Parallel()

	const offendingDeviceID uint16 = 1234

	// undefinedSType is outside the E37 §8.2.6.6 SType set, so it draws Reject(STypeNotSupported).
	const undefinedSType byte = 0x7F

	cases := []struct {
		name       string
		sender     string
		frame      func(sb [4]byte) []byte
		wantReason byte
	}{
		{
			name:       "data_while_not_selected",
			sender:     "sendRejectNotSelected",
			frame:      func(sb [4]byte) []byte { return dataFrame(offendingDeviceID, 1, 1, sb, nil) },
			wantReason: hsms.RejectNotSelected,
		},
		{
			name:   "undefined_stype",
			sender: "sendReject",
			frame: func(sb [4]byte) []byte {
				return controlFrameWithSessionID(offendingDeviceID, undefinedSType, sb)
			},
			wantReason: hsms.RejectSTypeNotSupported,
		},
		{
			name:   "orphan_control_response",
			sender: "sendRejectTransactionNotOpen",
			frame: func(sb [4]byte) []byte {
				// A Linktest.rsp (even SType) correlating to no open transaction (E37 §8.3.20).
				return controlFrameWithSessionID(offendingDeviceID, byte(hsms.LinktestRspType), sb)
			},
			wantReason: hsms.RejectTransactionNotOpen,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			port := freeLoopbackPort(t)
			conn := newPassiveConn(t, port, hsms.WithSessionID(testDeviceID), hsms.WithLinktestInterval(0))
			require.NoError(t, conn.Open(context.Background(), hsms.OpenBackground))
			t.Cleanup(func() { _ = conn.Close() })

			peer := dialPassive(t, port)
			t.Cleanup(func() { _ = peer.Close() })

			sb := [4]byte{0x0A, 0x0B, 0x0C, 0x0D}
			_, err := peer.Write(tc.frame(sb))
			require.NoError(t, err)

			rejPayload, err := peerReadFrame(peer, 5*time.Second)
			require.NoError(t, err, "peer must read our Reject.req from %s", tc.sender)
			require.Equal(t, byte(hsms.RejectReqType), rejPayload[5], "expected a Reject.req")
			require.Equal(t, tc.wantReason, rejPayload[3], "unexpected reject reason")

			require.Equal(t, hsms.ControlSessionID, frameSessionID(t, rejPayload),
				"%s must carry SessionID 0xFFFF (E37.1 §8.1), not the rejected frame's session ID", tc.sender)
			require.NotEqual(t, offendingDeviceID, frameSessionID(t, rejPayload),
				"echoing the rejected frame's session ID is the E37-generic rule that E37.1 §8.1 overrides")

			// Correlation is preserved by System Bytes (E37 §8.3.21.3).
			// That is what makes dropping the echoed session ID harmless.
			require.Equal(t, sb[:], rejPayload[6:10], "Reject.req must echo the rejected frame's System Bytes")
		})
	}
}

// controlFrameWithSessionID builds a header-only control frame with an arbitrary session ID and SType.
// The shared buildControlFrame helper fixes the session ID at 0xFFFF,
// which cannot distinguish "we emitted the profile constant" from "we echoed the offending frame".
func controlFrameWithSessionID(sessionID uint16, sType byte, sb [4]byte) []byte {
	h := make([]byte, 10)
	binary.BigEndian.PutUint16(h[0:2], sessionID)
	h[5] = sType
	copy(h[6:10], sb[:])

	return frameBytes(10, h, nil)
}
