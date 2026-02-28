// The simultaneous select tests verify SEMI E37 section 7.4.3 compliance by using
// a raw TCP peer to orchestrate precise message ordering that cannot be achieved
// with two hsmsss.Connection instances (since only the active side initiates select).
package hsmsssintegration

import (
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/go-secs/hsms"
	"github.com/arloliu/go-secs/hsmsss"
	"github.com/stretchr/testify/require"
)

// TestSimultaneousSelect_ActiveAcceptsRemoteSelect tests the SEMI E37 section 7.4.3
// simultaneous select scenario.
func TestSimultaneousSelect_ActiveAcceptsRemoteSelect(t *testing.T) {
	require := require.New(t)
	port := getFreePort(t)

	peerErrCh := make(chan error, 1)
	listenerReady := make(chan net.Listener, 1)
	selectedCh := make(chan struct{})
	var selectedOnce sync.Once

	go func() {
		peerErrCh <- runSimultaneousSelectPeer(t, port, listenerReady, selectedCh)
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
		hsmsss.WithLogger(newLoggerWith("role", "ACTIVE")),
	)
	require.NoError(err)

	ctx := t.Context()

	conn, err := hsmsss.NewConnection(ctx, cfg)
	require.NoError(err)

	session := conn.AddSession(testSessionID)

	session.AddConnStateChangeHandler(func(_ hsms.Connection, _ hsms.ConnState, cur hsms.ConnState) {
		if cur.IsSelected() {
			selectedOnce.Do(func() { close(selectedCh) })
		}
	})

	session.AddDataMessageHandler(echoHandler)

	require.NoError(conn.Open(false))
	defer conn.Close()

	select {
	case <-selectedCh:
	case err := <-peerErrCh:
		t.Fatalf("peer error during select: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for SELECTED state")
	}

	select {
	case err := <-peerErrCh:
		require.NoError(err, "raw peer should complete without error")
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for peer to finish")
	}
}

func runSimultaneousSelectPeer(t *testing.T, port int, listenerReady chan<- net.Listener, selectedCh <-chan struct{}) error {
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

	payload, err := readFrame(conn)
	if err != nil {
		return fmt.Errorf("read select.req: %w", err)
	}

	if len(payload) < 10 || payload[5] != hsms.SelectReqType {
		return fmt.Errorf("expected select.req, got SType=%d", payload[5])
	}

	activeSystemBytes := getSystemBytes(payload)

	// Send our OWN Select.req BEFORE replying (creates simultaneous select).
	peerSystemBytes := [4]byte{0xAA, 0xBB, 0xCC, 0xDD}
	peerSelectReq := buildControlHeader(0x00, hsms.SelectReqType, peerSystemBytes)

	if err := writeFrame(conn, peerSelectReq); err != nil {
		return fmt.Errorf("write peer select.req: %w", err)
	}

	rspPayload, err := readFrame(conn)
	if err != nil {
		return fmt.Errorf("read select.rsp for peer: %w", err)
	}

	if len(rspPayload) < 10 || rspPayload[5] != hsms.SelectRspType {
		return fmt.Errorf("expected select.rsp, got SType=%d", rspPayload[5])
	}

	if getSystemBytes(rspPayload) != peerSystemBytes {
		return fmt.Errorf("select.rsp system bytes mismatch")
	}

	if rspPayload[3] != hsms.SelectStatusSuccess {
		return fmt.Errorf("expected select status success, got %d", rspPayload[3])
	}

	activeSelectRsp := buildControlHeader(hsms.SelectStatusSuccess, hsms.SelectRspType, activeSystemBytes)

	if err := writeFrame(conn, activeSelectRsp); err != nil {
		return fmt.Errorf("write select.rsp for active: %w", err)
	}

	select {
	case <-selectedCh:
	case <-time.After(10 * time.Second):
		return fmt.Errorf("timeout waiting for active side SELECTED state")
	}

	// Send S1F1 to verify connection is functional.
	dataSysBytes := [4]byte{0x00, 0x00, 0x00, 0x10}
	dataHeader := make([]byte, 10)
	binary.BigEndian.PutUint16(dataHeader[0:2], testSessionID)
	dataHeader[2] = 0x81 // stream=1, W-bit=1
	dataHeader[3] = 0x01 // function=1
	dataHeader[4] = 0x00
	dataHeader[5] = hsms.DataMsgType
	copy(dataHeader[6:10], dataSysBytes[:])

	if err := writeFrame(conn, dataHeader); err != nil {
		return fmt.Errorf("write S1F1: %w", err)
	}

	replyPayload, err := readFrame(conn)
	if err != nil {
		return fmt.Errorf("read S1F2 reply: %w", err)
	}

	if len(replyPayload) < 10 || replyPayload[5] != hsms.DataMsgType || replyPayload[3] != 2 {
		return fmt.Errorf("expected S1F2, got SType=%d func=%d", replyPayload[5], replyPayload[3])
	}

	return nil
}

// TestSimultaneousSelect_AlreadySelectedRejectsSecondSelect verifies §7.4.2.
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
		hsmsss.WithLogger(newLoggerWith("role", "ACTIVE")),
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

	select {
	case <-selectedCh:
	case err := <-peerErrCh:
		t.Fatalf("peer error during select: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for SELECTED state")
	}

	select {
	case err := <-peerErrCh:
		require.NoError(err, "raw peer should complete without error")
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for peer to finish")
	}
}

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

	// Complete first select.
	payload, err := readFrame(conn)
	if err != nil {
		return fmt.Errorf("read select.req: %w", err)
	}

	if len(payload) < 10 || payload[5] != hsms.SelectReqType {
		return fmt.Errorf("expected select.req, got SType=%d", payload[5])
	}

	activeSystemBytes := getSystemBytes(payload)
	selectRsp := buildControlHeader(hsms.SelectStatusSuccess, hsms.SelectRspType, activeSystemBytes)

	if err := writeFrame(conn, selectRsp); err != nil {
		return fmt.Errorf("write select.rsp: %w", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Send a second Select.req which should be rejected.
	secondSysBytes := [4]byte{0xDD, 0xEE, 0xFF, 0x00}
	secondSelectReq := buildControlHeader(0x00, hsms.SelectReqType, secondSysBytes)

	if err := writeFrame(conn, secondSelectReq); err != nil {
		return fmt.Errorf("write second select.req: %w", err)
	}

	rspPayload, err := readFrame(conn)
	if err != nil {
		return fmt.Errorf("read second select.rsp: %w", err)
	}

	if len(rspPayload) < 10 || rspPayload[5] != hsms.SelectRspType {
		return fmt.Errorf("expected select.rsp, got SType=%d", rspPayload[5])
	}

	if getSystemBytes(rspPayload) != secondSysBytes {
		return fmt.Errorf("system bytes mismatch")
	}

	if rspPayload[3] != hsms.SelectStatusActived {
		return fmt.Errorf("expected already-active status (%d), got %d", hsms.SelectStatusActived, rspPayload[3])
	}

	return nil
}
