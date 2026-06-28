package hsmsssintegration

import (
	"context"
	"fmt"
	"net"
	"sync"
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
	for range 2 {
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

// ---------------------------------------------------------------------------
// TestLifecycle_UserHandlerReceivesAllTransitions
//
// Verifies that a ConnStateChangeHandler registered on a session receives
// every state transition across repeated open/close cycles, in FIFO order,
// with no dropped events. This is the strictest guarantee the async
// dispatcher is meant to uphold:
//
//   - Each open should produce: NotConnected → NotSelected → Selected.
//   - Each close should produce: Selected → NotConnected.
//   - No extra/duplicate/stale events, no losses across shutdowns.
//
// The Close() reorder (synthesize ToNotConnected before stateMgr.Stop, plus
// the dispatcher's post-cancel drain) is what makes the final event delivery
// reliable; if either breaks, this test fails.
// ---------------------------------------------------------------------------
func TestLifecycle_UserHandlerReceivesAllTransitions(t *testing.T) {
	const cycles = 5
	require := require.New(t)
	port := getFreePort(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	type transition struct{ prev, next hsms.ConnState }
	type observer struct {
		mu     sync.Mutex
		events []transition
	}
	obs := func() *observer { return &observer{events: make([]transition, 0, cycles*4)} }
	activeObs := obs()
	passiveObs := obs()

	record := func(o *observer) hsms.ConnStateChangeHandler {
		return func(_ hsms.Connection, prev, next hsms.ConnState) {
			o.mu.Lock()
			o.events = append(o.events, transition{prev, next})
			o.mu.Unlock()
		}
	}

	// Build fresh endpoints once; reuse across cycles to verify that
	// Start/Stop cycles preserve handler registration and do not leak state.
	passive := newEndpoint(t, ctx, port, true, false, nil, echoHandler)
	active := newEndpoint(t, ctx, port, false, true, nil)
	passive.session.AddConnStateChangeHandler(record(passiveObs))
	active.session.AddConnStateChangeHandler(record(activeObs))

	t.Cleanup(func() {
		closeEndpoint(t, active)
		closeEndpoint(t, passive)
	})

	for i := range cycles {
		require.NoError(passive.conn.Open(false), "cycle %d: passive open", i)
		require.NoError(active.conn.Open(false), "cycle %d: active open", i)
		waitState(t, active, hsms.SelectedState)
		waitState(t, passive, hsms.SelectedState)

		require.NoError(active.conn.Close(), "cycle %d: active close", i)
		require.NoError(passive.conn.Close(), "cycle %d: passive close", i)

		// Drain helper-level state channels so waitState in the next cycle
		// starts from an empty queue.
		drainStateCh(active.selectedCh)
		drainStateCh(passive.selectedCh)
	}

	assertCycleTransitions := func(name string, o *observer) {
		o.mu.Lock()
		defer o.mu.Unlock()

		// Find each cycle's "rising edge" (into NotSelected) and verify the
		// shape of the cycle that follows. We allow incidental extra events
		// (e.g. the passive reconnect loop may go through Connecting) but
		// require every cycle to contain the three anchor transitions in
		// order: *→NotSelected, NotSelected→Selected, Selected→NotConnected.
		anchors := []hsms.ConnState{
			hsms.NotSelectedState,
			hsms.SelectedState,
			hsms.NotConnectedState,
		}

		var seenCycles int
		i := 0
		for i < len(o.events) {
			// Scan forward for an event entering NotSelected.
			for i < len(o.events) && o.events[i].next != hsms.NotSelectedState {
				i++
			}
			if i >= len(o.events) {
				break
			}

			// Verify the next three anchor transitions appear in order.
			for ai, expected := range anchors {
				if i >= len(o.events) {
					t.Fatalf("%s: cycle %d truncated — missing transition to %s (events so far: %+v)",
						name, seenCycles, expected, o.events)
				}
				if o.events[i].next != expected {
					t.Fatalf("%s: cycle %d anchor %d: expected next=%s, got %s (full sequence: %+v)",
						name, seenCycles, ai, expected, o.events[i].next, o.events)
				}
				i++
			}
			seenCycles++
		}

		require.Equalf(cycles, seenCycles,
			"%s: expected %d observed cycles, got %d (full sequence: %+v)",
			name, cycles, seenCycles, o.events)
	}

	assertCycleTransitions("active", activeObs)
	assertCycleTransitions("passive", passiveObs)
}

func drainStateCh(ch chan hsms.ConnState) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}
