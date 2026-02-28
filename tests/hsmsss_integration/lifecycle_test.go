package hsmsssintegration

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/arloliu/go-secs/hsms"
	"github.com/arloliu/go-secs/hsmsss"
	"github.com/arloliu/go-secs/secs2"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// TestLifecycle_OpenCloseReopen
//
// Verifies that a Connection can be opened, used, closed, then re-opened
// and used again without error.
// ---------------------------------------------------------------------------
func TestLifecycle_OpenCloseReopen(t *testing.T) {
	require := require.New(t)
	port := getFreePort(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	passive := newEndpoint(t, ctx, port, true, false, nil, echoHandler)
	active := newEndpoint(t, ctx, port, false, true, nil)
	defer closeEndpoint(t, passive)
	defer closeEndpoint(t, active)

	// --- first open ---
	require.NoError(passive.conn.Open(false))
	require.NoError(active.conn.Open(false))
	waitState(t, active, hsms.SelectedState)
	waitState(t, passive, hsms.SelectedState)

	msg, err := hsms.NewDataMessage(1, 1, true, testSessionID, []byte{0, 0, 0, 1}, secs2.NewASCIIItem("first"))
	require.NoError(err)
	reply, err := active.session.SendMessage(msg)
	require.NoError(err)
	require.NotNil(reply)

	// --- close both ---
	require.NoError(active.conn.Close())
	require.NoError(passive.conn.Close())
	time.Sleep(300 * time.Millisecond) // allow goroutines to settle

	// --- reopen ---
	require.NoError(passive.conn.Open(false))
	require.NoError(active.conn.Open(false))
	waitState(t, active, hsms.SelectedState)
	waitState(t, passive, hsms.SelectedState)

	msg2, err := hsms.NewDataMessage(1, 1, true, testSessionID, []byte{0, 0, 0, 2}, secs2.NewASCIIItem("second"))
	require.NoError(err)
	reply2, err := active.session.SendMessage(msg2)
	require.NoError(err)
	require.NotNil(reply2)

	dataReply, ok := reply2.(*hsms.DataMessage)
	require.True(ok)
	v, err := dataReply.Item().ToASCII()
	require.NoError(err)
	require.Equal("second", v)
}

// ---------------------------------------------------------------------------
// TestLifecycle_GracefulDeselect
//
// A raw TCP peer sends Deselect.req to the passive hsmsss connection.
// Verify the connection replies Deselect.rsp(success) and transitions
// through NotConnected (because HSMS-SS treats deselect as disconnect).
// ---------------------------------------------------------------------------
func TestLifecycle_GracefulDeselect(t *testing.T) {
	require := require.New(t)
	port := getFreePort(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	passive := newEndpoint(t, ctx, port, true, false, nil)
	defer closeEndpoint(t, passive)

	require.NoError(passive.conn.Open(false))
	time.Sleep(200 * time.Millisecond)

	// Raw client connects and selects
	rawConn := rawClientSelectHandshake(t, port)
	defer rawConn.Close()

	waitState(t, passive, hsms.SelectedState)

	// Send Deselect.req
	sysBytes := [4]byte{0x10, 0x20, 0x30, 0x40}
	deselectReq := buildControlHeader(0x00, hsms.DeselectReqType, sysBytes)
	require.NoError(writeFrame(rawConn, deselectReq))

	// Read Deselect.rsp
	require.NoError(rawConn.SetReadDeadline(time.Now().Add(5 * time.Second)))
	rsp, err := readFrame(rawConn)
	require.NoError(err)
	require.Equal(byte(hsms.DeselectRspType), rsp[5], "expected Deselect.rsp")
	require.Equal(byte(0), rsp[3], "expected success status")

	// Connection should transition to NotConnected
	waitState(t, passive, hsms.NotConnectedState)
}

// ---------------------------------------------------------------------------
// TestLifecycle_AbruptPeerDropDetection
//
// An active hsmsss connection is selected with a raw peer. The raw peer
// abruptly closes the TCP socket. The active side should detect the drop
// and transition to NotConnected.
// ---------------------------------------------------------------------------
func TestLifecycle_AbruptPeerDropDetection(t *testing.T) {
	require := require.New(t)
	port := getFreePort(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// Start raw listener BEFORE creating active endpoint
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	require.NoError(err)
	defer ln.Close()

	opts := []hsmsss.ConnOption{
		hsmsss.WithAutoLinktest(true),
		hsmsss.WithLinktestInterval(300 * time.Millisecond),
		hsmsss.WithT6Timeout(1 * time.Second),
	}
	active := newEndpoint(t, ctx, port, false, true, opts)
	defer closeEndpoint(t, active)

	require.NoError(active.conn.Open(false))

	rawConn := rawPeerSelectHandshake(t, ln)
	waitState(t, active, hsms.SelectedState)

	// Reply to the first linktest so the connection stabilises
	require.NoError(rawConn.SetReadDeadline(time.Now().Add(3 * time.Second)))
	frame, err := readFrame(rawConn)
	require.NoError(err)
	require.Equal(byte(hsms.LinkTestReqType), frame[5])

	sysBytes := getSystemBytes(frame)
	rsp := buildControlHeader(0x00, hsms.LinkTestRspType, sysBytes)
	require.NoError(writeFrame(rawConn, rsp))

	// Abruptly close the raw peer
	require.NoError(rawConn.Close())

	// Active side should detect the drop and transition to NotConnected
	waitState(t, active, hsms.NotConnectedState)
}

// ---------------------------------------------------------------------------
// TestLifecycle_LinktestAutoAndT6Failure
//
// Enables auto-linktest (500ms interval). A raw peer replies to the first
// 2 Linktest.req but then stops. The T6 timeout (1s) should fire and
// the connection transitions to NotConnected.
// ---------------------------------------------------------------------------
func TestLifecycle_LinktestAutoAndT6Failure(t *testing.T) {
	require := require.New(t)
	port := getFreePort(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// Start a raw listener BEFORE creating the active endpoint
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	require.NoError(err)
	defer ln.Close()

	opts := []hsmsss.ConnOption{
		hsmsss.WithAutoLinktest(true),
		hsmsss.WithLinktestInterval(500 * time.Millisecond),
		hsmsss.WithT6Timeout(1 * time.Second),
	}
	active := newEndpoint(t, ctx, port, false, true, opts)
	defer closeEndpoint(t, active)

	require.NoError(active.conn.Open(false))

	// Accept and complete select handshake
	rawConn := rawPeerSelectHandshake(t, ln)
	defer rawConn.Close()

	waitState(t, active, hsms.SelectedState)

	// Reply to the first 2 Linktest.req messages
	for i := 0; i < 2; i++ {
		require.NoError(rawConn.SetReadDeadline(time.Now().Add(3 * time.Second)))
		frame, rErr := readFrame(rawConn)
		require.NoError(rErr)
		require.Equal(byte(hsms.LinkTestReqType), frame[5], "expected Linktest.req")

		sysBytes := getSystemBytes(frame)
		rsp := buildControlHeader(0x00, hsms.LinkTestRspType, sysBytes)
		require.NoError(writeFrame(rawConn, rsp))
	}

	// Stop replying -> T6 should fire
	waitState(t, active, hsms.NotConnectedState)
}

// ---------------------------------------------------------------------------
// TestLifecycle_DeselectFromPassiveSide
//
// Two real connections (active + passive). The host side closes; both
// sides should transition to NotConnected.
// ---------------------------------------------------------------------------
func TestLifecycle_DeselectFromPassiveSide(t *testing.T) {
	require := require.New(t)
	port := getFreePort(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	passive := newEndpoint(t, ctx, port, true, false, nil, echoHandler)
	active := newEndpoint(t, ctx, port, false, true, nil, echoHandler)
	defer closeEndpoint(t, passive)
	defer closeEndpoint(t, active)

	require.NoError(passive.conn.Open(false))
	require.NoError(active.conn.Open(false))
	waitState(t, active, hsms.SelectedState)
	waitState(t, passive, hsms.SelectedState)

	// Verify connection works
	msg, err := hsms.NewDataMessage(1, 1, true, testSessionID, []byte{0, 0, 0, 1}, secs2.NewASCIIItem("check"))
	require.NoError(err)
	reply, err := active.session.SendMessage(msg)
	require.NoError(err)
	require.NotNil(reply)

	// Close active (host) side
	require.NoError(active.conn.Close())

	// Both should transition to NotConnected
	waitState(t, active, hsms.NotConnectedState)
	waitState(t, passive, hsms.NotConnectedState)
}
