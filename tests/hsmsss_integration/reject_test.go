package hsmsssintegration

// Integration tests for HSMS-SS Reject.req round-trips.
//
// All four SEMI E37 §8.3.20 reason codes are covered end-to-end:
//
//   - Code 1 (STypeNotSupported): message_reader.go detects pType==0 with undefined SType,
//     returns a headerRejectError; receiverTask sends Reject.req and keeps connection alive.
//     Tested by TestReject_STypeNotSupported.
//
//   - Code 2 (PTypeNotSupported): same path but pType != 0.
//     Tested by TestReject_PTypeNotSupported.
//
//   - Code 3 (TransactionNotOpen): passive side, unsolicited Select.rsp — conn_passive.go
//     Tested by TestReject_TransactionNotOpen.
//
//   - Code 4 (NotSelected): both sides, data message in non-Selected state — conn_passive.go / conn_active.go
//     Tested by TestReject_DataMessageNotSelected and TestReject_InboundReject_PoisonsInFlightSend.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/arloliu/go-secs/hsms"
	"github.com/arloliu/go-secs/hsmsss"
	"github.com/arloliu/go-secs/secs2"
	"github.com/stretchr/testify/require"
)

// --- helpers local to reject tests ---

// buildRejectFrame builds a 10-byte Reject.req HSMS payload.
// reasonCode is placed in header[3]; header[2] holds sType (per NewRejectReq logic for control msgs).
// systemBytes must be 4 bytes and echo the system bytes of the message being rejected.
func buildRejectFrame(reasonCode byte, sType byte, systemBytes [4]byte) []byte {
	h := make([]byte, 10)
	binary.BigEndian.PutUint16(h[0:2], 0xFFFF) // session ID for control messages
	h[2] = sType                               // echo sType of the rejected message
	h[3] = reasonCode
	h[4] = 0
	h[5] = byte(hsms.RejectReqType) // 7
	copy(h[6:10], systemBytes[:])

	return h
}

// buildDataFrame builds a 10-byte-header + optional SECS-II body HSMS data frame.
// sessionID, stream, function, wBit (W-bit as 0x80 or 0x00), and systemBytes are set accordingly.
//
//nolint:unparam // sessionID is always testSessionID in current tests; kept for readability.
func buildDataFrame(sessionID uint16, stream, function byte, wBit bool, systemBytes [4]byte, body []byte) []byte {
	h := make([]byte, 10)
	binary.BigEndian.PutUint16(h[0:2], sessionID)
	h[2] = stream
	if wBit {
		h[2] |= 0x80
	}
	h[3] = function
	h[4] = 0 // pType = 0
	h[5] = 0 // sType = 0 (data message)
	copy(h[6:10], systemBytes[:])

	frame := make([]byte, 0, len(h)+len(body))
	frame = append(frame, h...)
	frame = append(frame, body...)

	return frame
}

// assertRejectFrame reads one HSMS frame from conn and asserts it is a Reject.req
// with the expected reason code and system bytes.
func assertRejectFrame(t *testing.T, conn net.Conn, wantReason byte, wantSysBytes [4]byte) {
	t.Helper()

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))

	payload, err := readFrame(conn)
	require.NoError(t, err, "reading Reject.req frame")
	require.GreaterOrEqual(t, len(payload), 10, "Reject.req frame too short")

	require.Equal(t, byte(hsms.RejectReqType), payload[5], "expected SType=7 (Reject.req)")
	require.Equal(t, wantReason, payload[3], "Reject.req reason code mismatch")
	require.Equal(t, wantSysBytes, [4]byte(payload[6:10]), "Reject.req system bytes mismatch")
}

// ---------------------------------------------------------------------------
// TestReject_DataMessageNotSelected (code 4)
//
// Raw client connects to a passive endpoint, skips the Select handshake, and
// sends an S1F1 data frame. The endpoint must reply with Reject.req(4) per
// SEMI E37 §8.3.20. After that the raw client completes the Select handshake
// and sends a valid S1F1, proving the receiver loop is not poisoned.
// ---------------------------------------------------------------------------
func TestReject_DataMessageNotSelected(t *testing.T) {
	port := getFreePort(t)
	ctx := t.Context()

	receivedCh := make(chan *hsms.DataMessage, 1)
	handler := func(msg *hsms.DataMessage, s hsms.Session) {
		receivedCh <- msg
		if msg.FunctionCode()%2 == 1 {
			_ = s.ReplyDataMessage(msg, secs2.NewASCIIItem("pong"))
		}
	}

	passive := newEndpoint(t, ctx, port, true, false, nil, handler)
	defer closeEndpoint(t, passive)
	require.NoError(t, passive.conn.Open(false))
	time.Sleep(200 * time.Millisecond) // let listener start

	rawConn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
	require.NoError(t, err)
	defer rawConn.Close()

	sysBytes := [4]byte{0xAA, 0xBB, 0xCC, 0xDD}

	// Send S1F1 data frame WITHOUT doing Select.req first.
	s1f1 := buildDataFrame(testSessionID, 1, 1, true, sysBytes, nil)
	require.NoError(t, writeFrame(rawConn, s1f1))

	// Endpoint must reply Reject.req(4) — data message received in not-selected state.
	assertRejectFrame(t, rawConn, hsms.RejectNotSelected, sysBytes)

	// Now complete the Select handshake so we can send a valid data message.
	selectSysBytes := [4]byte{0x01, 0x02, 0x03, 0x04}
	selectReq := buildControlHeader(0x00, hsms.SelectReqType, selectSysBytes)
	require.NoError(t, writeFrame(rawConn, selectReq))

	require.NoError(t, rawConn.SetReadDeadline(time.Now().Add(5*time.Second)))
	rsp, err := readFrame(rawConn)
	require.NoError(t, err)
	require.Equal(t, byte(hsms.SelectRspType), rsp[5], "expected Select.rsp after Select.req")
	require.Equal(t, byte(hsms.SelectStatusSuccess), rsp[3], "expected Select.rsp status=success")

	// Send a valid S1F1 after successful select — endpoint must deliver it and reply S1F2.
	validSysBytes := [4]byte{0x00, 0x00, 0x00, 0x02}
	validS1F1 := buildDataFrame(testSessionID, 1, 1, true, validSysBytes, nil)
	require.NoError(t, writeFrame(rawConn, validS1F1))

	// Assert handler received the message.
	require.NoError(t, rawConn.SetReadDeadline(time.Now().Add(5*time.Second)))
	select {
	case got := <-receivedCh:
		require.Equal(t, byte(1), got.StreamCode(), "stream mismatch")
		require.Equal(t, byte(1), got.FunctionCode(), "function mismatch")
	case <-time.After(5 * time.Second):
		t.Fatal("passive endpoint did not deliver data message after Reject round-trip")
	}

	// Assert S1F2 reply arrived on the wire.
	replyFrame, err := readFrame(rawConn)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(replyFrame), 10, "S1F2 reply frame too short")
	require.Equal(t, byte(0), replyFrame[5], "expected SType=0 (data message)")
	require.Equal(t, byte(2), replyFrame[3], "expected function=2 (S1F2)")
}

// ---------------------------------------------------------------------------
// TestReject_TransactionNotOpen (code 3)
//
// Raw client completes a normal Select handshake with a passive endpoint, then
// sends an unsolicited Select.rsp (no open transaction). Per SEMI E37 §8.3.20,
// the endpoint must reply with Reject.req(3). After that a valid S1F1 is sent
// to prove the receiver loop survives the reject.
// ---------------------------------------------------------------------------
func TestReject_TransactionNotOpen(t *testing.T) {
	port := getFreePort(t)
	ctx := t.Context()

	receivedCh := make(chan *hsms.DataMessage, 1)
	handler := func(msg *hsms.DataMessage, s hsms.Session) {
		receivedCh <- msg
		if msg.FunctionCode()%2 == 1 {
			_ = s.ReplyDataMessage(msg, secs2.NewASCIIItem("pong"))
		}
	}

	passive := newEndpoint(t, ctx, port, true, false, nil, handler)
	defer closeEndpoint(t, passive)
	require.NoError(t, passive.conn.Open(false))
	time.Sleep(200 * time.Millisecond) // let listener start

	// Complete normal Select handshake.
	rawConn := rawClientSelectHandshake(t, port)
	defer rawConn.Close()

	// Wait for passive side to be in SelectedState.
	requireStateEvent(t, passive.selectedCh, hsms.SelectedState)

	// Send unsolicited Select.rsp (no open Select.req transaction).
	unsolicitedSysBytes := [4]byte{0xDE, 0xAD, 0xBE, 0xEF}
	unsolicitedRsp := buildControlHeader(hsms.SelectStatusSuccess, hsms.SelectRspType, unsolicitedSysBytes)
	require.NoError(t, writeFrame(rawConn, unsolicitedRsp))

	// Endpoint must reply Reject.req(3) — transaction not open.
	assertRejectFrame(t, rawConn, hsms.RejectTransactionNotOpen, unsolicitedSysBytes)

	// Send a valid S1F1 to prove the receiver loop was not poisoned by the reject.
	validSysBytes := [4]byte{0x00, 0x00, 0x00, 0x03}
	validS1F1 := buildDataFrame(testSessionID, 1, 1, true, validSysBytes, nil)
	require.NoError(t, writeFrame(rawConn, validS1F1))

	// Assert handler received the message.
	select {
	case got := <-receivedCh:
		require.Equal(t, byte(1), got.StreamCode(), "stream mismatch")
		require.Equal(t, byte(1), got.FunctionCode(), "function mismatch")
	case <-time.After(5 * time.Second):
		t.Fatal("passive endpoint did not deliver data message after Reject(3) round-trip")
	}

	// Assert S1F2 reply arrived on the wire.
	require.NoError(t, rawConn.SetReadDeadline(time.Now().Add(5*time.Second)))
	replyFrame, err := readFrame(rawConn)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(replyFrame), 10, "S1F2 reply frame too short")
	require.Equal(t, byte(0), replyFrame[5], "expected SType=0 (data message)")
	require.Equal(t, byte(2), replyFrame[3], "expected function=2 (S1F2)")
}

// ---------------------------------------------------------------------------
// TestReject_InboundReject_PoisonsInFlightSend (bonus)
//
// Active SUT dials a raw peer listener. After Select completes, SUT calls
// SendMessage(S1F1) — the raw peer responds with Reject.req(4) instead of
// S1F2. The SendMessage call must return hsms.ErrRejectNotSelected, and the
// connection must remain in SelectedState (not drop).
// ---------------------------------------------------------------------------
func TestReject_InboundReject_PoisonsInFlightSend(t *testing.T) {
	// Set up a raw listener that acts as the passive side.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	require.True(t, ok, "listener address is not a TCPAddr")
	port := tcpAddr.Port

	peerErrCh := make(chan error, 1)
	// peerReadyCh is closed when the peer has finished the select handshake and is
	// ready to receive the data message from the SUT.
	peerReadyCh := make(chan struct{})
	// peerDoneCh is closed by the test when assertions are complete, allowing the
	// peer to exit cleanly without dropping the TCP connection prematurely.
	peerDoneCh := make(chan struct{})

	go func() {
		peerErrCh <- runRejectInboundPeer(t, ln, peerReadyCh, peerDoneCh)
	}()

	ctx := t.Context()

	// SUT is active (host) side.
	opts := []hsmsss.ConnOption{
		hsmsss.WithT3Timeout(3 * time.Second),
		hsmsss.WithLogger(newLoggerWith("role", "ACTIVE-SUT")),
	}
	sut := newEndpoint(t, ctx, port, false, true, opts)
	defer closeEndpoint(t, sut)
	require.NoError(t, sut.conn.Open(false))

	// Wait for SUT to be in SelectedState.
	requireStateEvent(t, sut.selectedCh, hsms.SelectedState)

	// Wait for peer to signal readiness.
	select {
	case <-peerReadyCh:
	case err := <-peerErrCh:
		t.Fatalf("peer failed during select: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for raw peer to become ready")
	}

	// SUT sends S1F1 with W-bit=true; peer will reply with Reject.req(4).
	msg, err := hsms.NewDataMessage(1, 1, true, testSessionID, []byte{0x00, 0x00, 0x00, 0x10}, secs2.NewASCIIItem("hello"))
	require.NoError(t, err)

	_, sendErr := sut.session.SendMessage(msg)

	// SendMessage must return an error that wraps/is ErrRejectNotSelected.
	require.Error(t, sendErr, "expected error from SendMessage when peer sends Reject(4)")
	require.True(t, errors.Is(sendErr, hsms.ErrRejectNotSelected),
		"expected ErrRejectNotSelected, got: %v", sendErr)

	// Connection must still be in SelectedState — Reject.req must not drop the link.
	// Give the receiver loop a moment to process and assert no NotConnected event arrives.
	// We use a brief window: if a state change event arrives, it must NOT be NotConnected.
	timeout := time.NewTimer(500 * time.Millisecond)
	defer timeout.Stop()

	for {
		select {
		case s := <-sut.selectedCh:
			require.NotEqual(t, hsms.NotConnectedState, s,
				"SUT connection dropped after receiving Reject.req — receiver loop poisoned")
		case <-timeout.C:
			// No NotConnected event arrived — connection is healthy.
			goto connectionHealthy
		}
	}
connectionHealthy:

	// Signal the peer that assertions are done so it can exit cleanly
	// without dropping the TCP connection before we finish checking.
	close(peerDoneCh)

	// Peer goroutine must complete without error.
	select {
	case peerErr := <-peerErrCh:
		require.NoError(t, peerErr, "raw peer completed with error")
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for raw peer to finish")
	}
}

// runRejectInboundPeer accepts one connection from the active SUT, completes
// the Select handshake, reads one data message, then replies with Reject.req(4).
// doneCh is closed by the test when it has finished asserting so the peer can exit.
func runRejectInboundPeer(t *testing.T, ln net.Listener, ready chan<- struct{}, done <-chan struct{}) error {
	t.Helper()

	conn, err := ln.Accept()
	if err != nil {
		return fmt.Errorf("accept: %w", err)
	}
	defer conn.Close()

	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return fmt.Errorf("set deadline: %w", err)
	}

	// Read Select.req from active SUT.
	payload, err := readFrame(conn)
	if err != nil {
		return fmt.Errorf("read select.req: %w", err)
	}

	if len(payload) < 10 || payload[5] != byte(hsms.SelectReqType) {
		return fmt.Errorf("expected select.req, got SType=%d", payload[5])
	}

	activeSysBytes := getSystemBytes(payload)
	selectRsp := buildControlHeader(hsms.SelectStatusSuccess, hsms.SelectRspType, activeSysBytes)

	if err := writeFrame(conn, selectRsp); err != nil {
		return fmt.Errorf("write select.rsp: %w", err)
	}

	// Signal the test that the handshake is complete and peer is ready.
	close(ready)

	// Read the S1F1 data message from SUT.
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return fmt.Errorf("set deadline for data msg: %w", err)
	}

	dataPayload, err := readFrame(conn)
	if err != nil {
		return fmt.Errorf("read S1F1: %w", err)
	}

	if len(dataPayload) < 10 {
		return fmt.Errorf("S1F1 frame too short: %d bytes", len(dataPayload))
	}

	if dataPayload[5] != byte(hsms.DataMsgType) {
		return fmt.Errorf("expected data message (SType=0), got SType=%d", dataPayload[5])
	}

	// Reply with Reject.req(4) — echoing the system bytes of the received data message.
	dataSysBytes := getSystemBytes(dataPayload)
	rejectFrame := buildRejectFrame(hsms.RejectNotSelected, dataPayload[5], dataSysBytes)

	if err := writeFrame(conn, rejectFrame); err != nil {
		return fmt.Errorf("write Reject.req(4): %w", err)
	}

	// Keep the TCP connection open until the test has finished asserting,
	// so the SUT's receiver loop stays alive (no EOF). The peer drains any
	// incoming linktest or similar control messages until done is closed.
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		return fmt.Errorf("set post-reject deadline: %w", err)
	}

	select {
	case <-done:
		// Test done; allow deferred conn.Close() to run.
	case <-time.After(3 * time.Second):
		// Safety valve so the goroutine doesn't leak on test failure.
	}

	return nil
}

// buildBadHeaderFrame builds a 10-byte HSMS frame with the given pType and sType.
// sessionID is fixed to testSessionID and systemBytes to a recognisable sentinel.
// The length prefix is prepended so the frame can be written directly via writeFrame.
func buildBadHeaderFrame(sessionID uint16, pType, sType byte, systemBytes [4]byte) []byte {
	h := make([]byte, 10)
	binary.BigEndian.PutUint16(h[0:2], sessionID)
	h[2] = 0
	h[3] = 0
	h[4] = pType
	h[5] = sType
	copy(h[6:10], systemBytes[:])

	return h
}

// ---------------------------------------------------------------------------
// TestReject_PTypeNotSupported (code 2)
//
// Raw client completes a normal Select handshake with a passive endpoint, then
// sends a frame with pType=1 (non-zero) and a valid sType. Per SEMI E37 §7.10.3
// the endpoint must reply Reject.req(2). The reason code, echoed session ID,
// echoed pType (in header[2]), and echoed system bytes are all asserted. A valid
// S1F1 is sent afterward to prove the receiver loop survived the Reject.
// ---------------------------------------------------------------------------
func TestReject_PTypeNotSupported(t *testing.T) {
	port := getFreePort(t)
	ctx := t.Context()

	receivedCh := make(chan *hsms.DataMessage, 1)
	handler := func(msg *hsms.DataMessage, s hsms.Session) {
		receivedCh <- msg
		if msg.FunctionCode()%2 == 1 {
			_ = s.ReplyDataMessage(msg, secs2.NewASCIIItem("pong"))
		}
	}

	passive := newEndpoint(t, ctx, port, true, false, nil, handler)
	defer closeEndpoint(t, passive)
	require.NoError(t, passive.conn.Open(false))
	time.Sleep(200 * time.Millisecond) // let listener start

	// Complete normal Select handshake.
	rawConn := rawClientSelectHandshake(t, port)
	defer rawConn.Close()

	// Wait for passive side to reach SelectedState.
	requireStateEvent(t, passive.selectedCh, hsms.SelectedState)

	// Send a frame with pType=1, sType=hsms.LinkTestReqType (valid sType, irrelevant here).
	badPType := byte(0x01)
	badSType := byte(hsms.LinkTestReqType)
	badSysBytes := [4]byte{0xDE, 0xAD, 0xBE, 0xEF}
	badFrame := buildBadHeaderFrame(testSessionID, badPType, badSType, badSysBytes)
	require.NoError(t, writeFrame(rawConn, badFrame))

	// Endpoint must reply Reject.req(2) — PType not supported.
	require.NoError(t, rawConn.SetReadDeadline(time.Now().Add(5*time.Second)))
	rejectPayload, err := readFrame(rawConn)
	require.NoError(t, err, "reading Reject.req frame")
	require.GreaterOrEqual(t, len(rejectPayload), 10, "Reject.req frame too short")

	require.Equal(t, byte(hsms.RejectReqType), rejectPayload[5], "expected SType=7 (Reject.req)")
	require.Equal(t, byte(hsms.RejectPTypeNotSupported), rejectPayload[3], "Reject.req reason code mismatch")
	// Per NewRejectReqRaw: for PTypeNotSupported, header[2] echoes the received pType.
	require.Equal(t, badPType, rejectPayload[2], "Reject.req must echo pType in header[2]")
	require.Equal(t, badSysBytes, [4]byte(rejectPayload[6:10]), "Reject.req system bytes mismatch")
	// Session ID must be echoed.
	gotSessionID := binary.BigEndian.Uint16(rejectPayload[0:2])
	require.Equal(t, uint16(testSessionID), gotSessionID, "Reject.req session ID mismatch")

	// Send a valid S1F1 to prove the receiver loop is still alive.
	validSysBytes := [4]byte{0x00, 0x00, 0x00, 0x05}
	validS1F1 := buildDataFrame(testSessionID, 1, 1, true, validSysBytes, nil)
	require.NoError(t, writeFrame(rawConn, validS1F1))

	// Assert handler received the data message.
	select {
	case got := <-receivedCh:
		require.Equal(t, byte(1), got.StreamCode(), "stream mismatch")
		require.Equal(t, byte(1), got.FunctionCode(), "function mismatch")
	case <-time.After(5 * time.Second):
		t.Fatal("passive endpoint did not deliver data message after Reject(2) round-trip")
	}

	// Assert S1F2 reply arrived on the wire.
	require.NoError(t, rawConn.SetReadDeadline(time.Now().Add(5*time.Second)))
	replyFrame, err := readFrame(rawConn)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(replyFrame), 10, "S1F2 reply frame too short")
	require.Equal(t, byte(0), replyFrame[5], "expected SType=0 (data message)")
	require.Equal(t, byte(2), replyFrame[3], "expected function=2 (S1F2)")
}

// ---------------------------------------------------------------------------
// TestReject_STypeNotSupported (code 1)
//
// Raw client completes a normal Select handshake with a passive endpoint, then
// sends a frame with pType=0 and sType=8 (undefined per SEMI E37). The endpoint
// must reply Reject.req(1). The reason code, echoed session ID, echoed sType
// (in header[2]), and echoed system bytes are all asserted. A valid S1F1 is
// sent afterward to prove the receiver loop survived the Reject.
// ---------------------------------------------------------------------------
func TestReject_STypeNotSupported(t *testing.T) {
	port := getFreePort(t)
	ctx := t.Context()

	receivedCh := make(chan *hsms.DataMessage, 1)
	handler := func(msg *hsms.DataMessage, s hsms.Session) {
		receivedCh <- msg
		if msg.FunctionCode()%2 == 1 {
			_ = s.ReplyDataMessage(msg, secs2.NewASCIIItem("pong"))
		}
	}

	passive := newEndpoint(t, ctx, port, true, false, nil, handler)
	defer closeEndpoint(t, passive)
	require.NoError(t, passive.conn.Open(false))
	time.Sleep(200 * time.Millisecond) // let listener start

	// Complete normal Select handshake.
	rawConn := rawClientSelectHandshake(t, port)
	defer rawConn.Close()

	// Wait for passive side to reach SelectedState.
	requireStateEvent(t, passive.selectedCh, hsms.SelectedState)

	// Send a frame with pType=0, sType=8 (undefined — not in IsValidSType set).
	badPType := byte(0x00)
	badSType := byte(0x08) // SType 8 is not assigned by SEMI E37
	badSysBytes := [4]byte{0xCA, 0xFE, 0xBA, 0xBE}
	badFrame := buildBadHeaderFrame(testSessionID, badPType, badSType, badSysBytes)
	require.NoError(t, writeFrame(rawConn, badFrame))

	// Endpoint must reply Reject.req(1) — SType not supported.
	require.NoError(t, rawConn.SetReadDeadline(time.Now().Add(5*time.Second)))
	rejectPayload, err := readFrame(rawConn)
	require.NoError(t, err, "reading Reject.req frame")
	require.GreaterOrEqual(t, len(rejectPayload), 10, "Reject.req frame too short")

	require.Equal(t, byte(hsms.RejectReqType), rejectPayload[5], "expected SType=7 (Reject.req)")
	require.Equal(t, byte(hsms.RejectSTypeNotSupported), rejectPayload[3], "Reject.req reason code mismatch")
	// Per NewRejectReqRaw: for STypeNotSupported, header[2] echoes the received sType.
	require.Equal(t, badSType, rejectPayload[2], "Reject.req must echo sType in header[2]")
	require.Equal(t, badSysBytes, [4]byte(rejectPayload[6:10]), "Reject.req system bytes mismatch")
	// Session ID must be echoed.
	gotSessionID := binary.BigEndian.Uint16(rejectPayload[0:2])
	require.Equal(t, uint16(testSessionID), gotSessionID, "Reject.req session ID mismatch")

	// Send a valid S1F1 to prove the receiver loop is still alive.
	validSysBytes := [4]byte{0x00, 0x00, 0x00, 0x06}
	validS1F1 := buildDataFrame(testSessionID, 1, 1, true, validSysBytes, nil)
	require.NoError(t, writeFrame(rawConn, validS1F1))

	// Assert handler received the data message.
	select {
	case got := <-receivedCh:
		require.Equal(t, byte(1), got.StreamCode(), "stream mismatch")
		require.Equal(t, byte(1), got.FunctionCode(), "function mismatch")
	case <-time.After(5 * time.Second):
		t.Fatal("passive endpoint did not deliver data message after Reject(1) round-trip")
	}

	// Assert S1F2 reply arrived on the wire.
	require.NoError(t, rawConn.SetReadDeadline(time.Now().Add(5*time.Second)))
	replyFrame, err := readFrame(rawConn)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(replyFrame), 10, "S1F2 reply frame too short")
	require.Equal(t, byte(0), replyFrame[5], "expected SType=0 (data message)")
	require.Equal(t, byte(2), replyFrame[3], "expected function=2 (S1F2)")
}
