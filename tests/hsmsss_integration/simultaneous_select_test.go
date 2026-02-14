// Package hsmsssintegration contains integration tests for the hsmsss package
// that exercise full HSMS-SS connection lifecycles over real TCP.
//
// The simultaneous select tests verify SEMI E37 section 7.4.3 compliance by using
// a raw TCP peer to orchestrate precise message ordering that cannot be achieved
// with two hsmsss.Connection instances (since only the active side initiates select).
package hsmsssintegration

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/go-secs/hsms"
	"github.com/arloliu/go-secs/hsmsss"
	"github.com/arloliu/go-secs/logger"
	"github.com/stretchr/testify/require"
)

const testSessionID = 0x0001

// --- HSMS frame helpers ---

// readFrame reads one complete HSMS frame (4-byte length + payload) from conn.
func readFrame(conn net.Conn) ([]byte, error) {
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(conn, lenBuf); err != nil {
		return nil, fmt.Errorf("read frame length: %w", err)
	}

	msgLen := binary.BigEndian.Uint32(lenBuf)
	payload := make([]byte, msgLen)

	if _, err := io.ReadFull(conn, payload); err != nil {
		return nil, fmt.Errorf("read frame payload: %w", err)
	}

	return payload, nil
}

// writeFrame writes a complete HSMS frame (4-byte length prefix + payload).
func writeFrame(conn net.Conn, payload []byte) error {
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(payload)))

	if _, err := conn.Write(lenBuf); err != nil {
		return fmt.Errorf("write frame length: %w", err)
	}

	if _, err := conn.Write(payload); err != nil {
		return fmt.Errorf("write frame payload: %w", err)
	}

	return nil
}

// buildControlHeader builds a 10-byte HSMS control message header.
// In HSMS-SS, session ID is always 0xFFFF and header byte 2 is always 0 for control messages.
//
//nolint:unparam // hb3 is always 0 in current tests (SelectStatusSuccess=0), but kept for clarity.
func buildControlHeader(hb3, stype byte, systemBytes [4]byte) []byte {
	h := make([]byte, 10)
	binary.BigEndian.PutUint16(h[0:2], 0xFFFF)
	h[2] = 0 // HB2: always 0 for control messages
	h[3] = hb3
	h[4] = 0 // PType = SECS-II
	h[5] = stype
	copy(h[6:10], systemBytes[:])

	return h
}

// getSystemBytes extracts the 4-byte system bytes from an HSMS header.
func getSystemBytes(header []byte) [4]byte {
	var sb [4]byte
	copy(sb[:], header[6:10])

	return sb
}

func getFreePort(t *testing.T) int {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer l.Close()

	addr, ok := l.Addr().(*net.TCPAddr)
	require.True(t, ok)

	return addr.Port
}

// --- Integration tests ---

// TestSimultaneousSelect_ActiveAcceptsRemoteSelect tests the SEMI E37 section 7.4.3
// simultaneous select scenario.
//
// Timeline:
//  1. Active Connection dials TCP, enters NOT_SELECTED, sends Select.req(A)
//  2. Raw peer reads Select.req(A) but does NOT reply yet
//  3. Raw peer sends its OWN Select.req(B) creating the simultaneous select race
//  4. Active receives Select.req(B) in NOT_SELECTED state, replies Select.rsp(B, success) + ToSelectedAsync
//  5. Raw peer reads Select.rsp(B) and verifies status=success
//  6. Raw peer finally replies Select.rsp(A, success) to active original request
//  7. Active selectSession() returns, confirming SELECTED state
//  8. Data message round-trip proves the connection is fully functional
func TestSimultaneousSelect_ActiveAcceptsRemoteSelect(t *testing.T) {
	require := require.New(t)
	port := getFreePort(t)

	// Channel for the raw peer to report errors.
	peerErrCh := make(chan error, 1)
	// Channel to signal the raw peer is ready to accept.
	listenerReady := make(chan net.Listener, 1)
	// Channel to notify the peer when the active side is fully SELECTED.
	// Closed (not sent-to) so both main goroutine and peer can receive the signal.
	selectedCh := make(chan struct{})
	var selectedOnce sync.Once

	// Start the raw TCP peer.
	go func() {
		peerErrCh <- runSimultaneousSelectPeer(t, port, listenerReady, selectedCh)
	}()

	// Wait for listener to be ready.
	var listener net.Listener
	select {
	case l := <-listenerReady:
		listener = l
		defer listener.Close()
	case err := <-peerErrCh:
		t.Fatalf("peer failed before listener ready: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for peer listener")
	}

	// Create the active hsmsss.Connection.
	cfg, err := hsmsss.NewConnectionConfig("127.0.0.1", port,
		hsmsss.WithActive(),
		hsmsss.WithHostRole(),
		hsmsss.WithT3Timeout(3*time.Second),
		hsmsss.WithT5Timeout(500*time.Millisecond),
		hsmsss.WithT6Timeout(3*time.Second),
		hsmsss.WithT7Timeout(10*time.Second),
		hsmsss.WithT8Timeout(1*time.Second),
		hsmsss.WithConnectRemoteTimeout(2*time.Second),
		hsmsss.WithAutoLinktest(false),
		hsmsss.WithLogger(logger.GetLogger().With("role", "ACTIVE")),
	)
	require.NoError(err)

	ctx := t.Context()

	conn, err := hsmsss.NewConnection(ctx, cfg)
	require.NoError(err)

	session := conn.AddSession(testSessionID)

	// Track SELECTED state via public API (same pattern as secs1_integration).
	session.AddConnStateChangeHandler(func(_ hsms.Connection, _ hsms.ConnState, cur hsms.ConnState) {
		if cur.IsSelected() {
			selectedOnce.Do(func() { close(selectedCh) })
		}
	})

	// Register an echo handler for data messages.
	session.AddDataMessageHandler(func(msg *hsms.DataMessage, s hsms.Session) {
		if msg.FunctionCode()%2 == 1 {
			_ = s.ReplyDataMessage(msg, msg.Item())
		}
	})

	require.NoError(conn.Open(false))
	defer conn.Close()

	// Wait for SELECTED state (the simultaneous select dance happens in the background).
	select {
	case <-selectedCh:
		// success
	case err := <-peerErrCh:
		t.Fatalf("peer error during select: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for SELECTED state")
	}

	// Wait for peer to complete successfully.
	select {
	case err := <-peerErrCh:
		require.NoError(err, "raw peer should complete without error")
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for peer to finish")
	}
}

// runSimultaneousSelectPeer implements the raw TCP side of the simultaneous select test.
func runSimultaneousSelectPeer(t *testing.T, port int, listenerReady chan<- net.Listener, selectedCh <-chan struct{}) error {
	t.Helper()

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	// Signal that we are ready.
	listenerReady <- ln

	conn, err := ln.Accept()
	if err != nil {
		_ = ln.Close()
		return fmt.Errorf("accept: %w", err)
	}
	defer conn.Close()
	defer ln.Close()

	// Set read deadline for all operations.
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return fmt.Errorf("set deadline: %w", err)
	}

	// Step 1: Read the active side Select.req
	payload, err := readFrame(conn)
	if err != nil {
		return fmt.Errorf("read select.req: %w", err)
	}

	if len(payload) < 10 {
		return fmt.Errorf("select.req too short: %d bytes", len(payload))
	}

	if payload[5] != hsms.SelectReqType {
		return fmt.Errorf("expected select.req (SType=%d), got SType=%d", hsms.SelectReqType, payload[5])
	}

	activeSystemBytes := getSystemBytes(payload)

	// Step 2: Send our OWN Select.req BEFORE replying (creates simultaneous select).
	peerSystemBytes := [4]byte{0xAA, 0xBB, 0xCC, 0xDD}
	peerSelectReq := buildControlHeader(0x00, hsms.SelectReqType, peerSystemBytes)

	if err := writeFrame(conn, peerSelectReq); err != nil {
		return fmt.Errorf("write peer select.req: %w", err)
	}

	// Step 3: Read the active side Select.rsp for our Select.req.
	// The active side should accept it (status=0) since it is in NOT_SELECTED state.
	rspPayload, err := readFrame(conn)
	if err != nil {
		return fmt.Errorf("read select.rsp for peer: %w", err)
	}

	if len(rspPayload) < 10 {
		return fmt.Errorf("select.rsp too short: %d bytes", len(rspPayload))
	}

	if rspPayload[5] != hsms.SelectRspType {
		return fmt.Errorf("expected select.rsp (SType=%d), got SType=%d", hsms.SelectRspType, rspPayload[5])
	}

	rspSystemBytes := getSystemBytes(rspPayload)
	if rspSystemBytes != peerSystemBytes {
		return fmt.Errorf("select.rsp system bytes mismatch: got %v, want %v", rspSystemBytes, peerSystemBytes)
	}

	selectStatus := rspPayload[3]
	if selectStatus != hsms.SelectStatusSuccess {
		return fmt.Errorf("expected select status success (0), got %d", selectStatus)
	}

	// Step 4: Reply Select.rsp(success) for the active side original Select.req.
	activeSelectRsp := buildControlHeader(hsms.SelectStatusSuccess, hsms.SelectRspType, activeSystemBytes)

	if err := writeFrame(conn, activeSelectRsp); err != nil {
		return fmt.Errorf("write select.rsp for active: %w", err)
	}

	// Wait for the active side to fully transition to SELECTED before sending data.
	// The state transition is async, so without this synchronization the data message
	// may arrive while the active side is still in NOT_SELECTED.
	select {
	case <-selectedCh:
	case <-time.After(10 * time.Second):
		return fmt.Errorf("timeout waiting for active side SELECTED state")
	}

	// Step 5: Send a data message (S1F1) to verify the connection is functional.
	dataSysBytes := [4]byte{0x00, 0x00, 0x00, 0x10}
	dataHeader := make([]byte, 10)
	binary.BigEndian.PutUint16(dataHeader[0:2], testSessionID)
	dataHeader[2] = 0x81 // stream=1, W-bit=1
	dataHeader[3] = 0x01 // function=1
	dataHeader[4] = 0x00 // PType=0
	dataHeader[5] = hsms.DataMsgType
	copy(dataHeader[6:10], dataSysBytes[:])

	if err := writeFrame(conn, dataHeader); err != nil {
		return fmt.Errorf("write S1F1: %w", err)
	}

	// Step 6: Read the reply (S1F2) from the active side.
	replyPayload, err := readFrame(conn)
	if err != nil {
		return fmt.Errorf("read S1F2 reply: %w", err)
	}

	if len(replyPayload) < 10 {
		return fmt.Errorf("S1F2 too short: %d bytes", len(replyPayload))
	}

	if replyPayload[5] != hsms.DataMsgType {
		return fmt.Errorf("expected data msg (SType=0), got SType=%d", replyPayload[5])
	}

	replyFunction := replyPayload[3]
	if replyFunction != 2 {
		return fmt.Errorf("expected S1F2 (function=2), got function=%d", replyFunction)
	}

	replySysBytes := getSystemBytes(replyPayload)
	if replySysBytes != dataSysBytes {
		return fmt.Errorf("S1F2 system bytes mismatch: got %v, want %v", replySysBytes, dataSysBytes)
	}

	return nil
}

// TestSimultaneousSelect_AlreadySelectedRejectsSecondSelect verifies that once
// the active side is in SELECTED state, a second Select.req from the remote
// receives a Select.rsp with status "communication already active" (section 7.4.2).
func TestSimultaneousSelect_AlreadySelectedRejectsSecondSelect(t *testing.T) {
	require := require.New(t)
	port := getFreePort(t)

	peerErrCh := make(chan error, 1)
	listenerReady := make(chan net.Listener, 1)

	go func() {
		peerErrCh <- runAlreadySelectedPeer(t, port, listenerReady)
	}()

	var listener net.Listener
	select {
	case l := <-listenerReady:
		listener = l
		defer listener.Close()
	case err := <-peerErrCh:
		t.Fatalf("peer failed before listener ready: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for peer listener")
	}

	cfg, err := hsmsss.NewConnectionConfig("127.0.0.1", port,
		hsmsss.WithActive(),
		hsmsss.WithHostRole(),
		hsmsss.WithT3Timeout(3*time.Second),
		hsmsss.WithT5Timeout(500*time.Millisecond),
		hsmsss.WithT6Timeout(3*time.Second),
		hsmsss.WithT7Timeout(10*time.Second),
		hsmsss.WithT8Timeout(1*time.Second),
		hsmsss.WithConnectRemoteTimeout(2*time.Second),
		hsmsss.WithAutoLinktest(false),
		hsmsss.WithLogger(logger.GetLogger().With("role", "ACTIVE")),
	)
	require.NoError(err)

	ctx := t.Context()

	conn, err := hsmsss.NewConnection(ctx, cfg)
	require.NoError(err)

	session := conn.AddSession(testSessionID)

	selectedCh := make(chan struct{}, 1)
	session.AddConnStateChangeHandler(func(_ hsms.Connection, _ hsms.ConnState, cur hsms.ConnState) {
		if cur.IsSelected() {
			select {
			case selectedCh <- struct{}{}:
			default:
			}
		}
	})

	require.NoError(conn.Open(false))
	defer conn.Close()

	// Wait for SELECTED state (normal select).
	select {
	case <-selectedCh:
	case err := <-peerErrCh:
		t.Fatalf("peer error during select: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for SELECTED state")
	}

	// Wait for peer to complete (it sends a second Select.req after first succeeds).
	select {
	case err := <-peerErrCh:
		require.NoError(err, "raw peer should complete without error")
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for peer to finish")
	}
}

// runAlreadySelectedPeer completes a normal select, then sends a second Select.req
// and verifies it gets rejected with "already active" status.
func runAlreadySelectedPeer(t *testing.T, port int, listenerReady chan<- net.Listener) error {
	t.Helper()

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	listenerReady <- ln

	conn, err := ln.Accept()
	if err != nil {
		_ = ln.Close()
		return fmt.Errorf("accept: %w", err)
	}
	defer conn.Close()
	defer ln.Close()

	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return fmt.Errorf("set deadline: %w", err)
	}

	// Step 1: Read active Select.req and reply with success.
	payload, err := readFrame(conn)
	if err != nil {
		return fmt.Errorf("read select.req: %w", err)
	}

	if len(payload) < 10 || payload[5] != hsms.SelectReqType {
		return fmt.Errorf("expected select.req, got SType=%d (len=%d)", payload[5], len(payload))
	}

	activeSystemBytes := getSystemBytes(payload)
	selectRsp := buildControlHeader(hsms.SelectStatusSuccess, hsms.SelectRspType, activeSystemBytes)

	if err := writeFrame(conn, selectRsp); err != nil {
		return fmt.Errorf("write select.rsp: %w", err)
	}

	// Give the active side time to transition to SELECTED.
	time.Sleep(100 * time.Millisecond)

	// Step 2: Send a second Select.req which should be rejected.
	secondSysBytes := [4]byte{0xDD, 0xEE, 0xFF, 0x00}
	secondSelectReq := buildControlHeader(0x00, hsms.SelectReqType, secondSysBytes)

	if err := writeFrame(conn, secondSelectReq); err != nil {
		return fmt.Errorf("write second select.req: %w", err)
	}

	// Step 3: Read the response, expect "already active" (status=1).
	rspPayload, err := readFrame(conn)
	if err != nil {
		return fmt.Errorf("read second select.rsp: %w", err)
	}

	if len(rspPayload) < 10 || rspPayload[5] != hsms.SelectRspType {
		return fmt.Errorf("expected select.rsp, got SType=%d", rspPayload[5])
	}

	rspSysBytes := getSystemBytes(rspPayload)
	if rspSysBytes != secondSysBytes {
		return fmt.Errorf("system bytes mismatch: got %v, want %v", rspSysBytes, secondSysBytes)
	}

	status := rspPayload[3]
	if status != hsms.SelectStatusActived {
		return fmt.Errorf("expected already-active status (%d), got %d", hsms.SelectStatusActived, status)
	}

	return nil
}
