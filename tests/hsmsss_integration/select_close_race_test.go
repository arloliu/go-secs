package hsmsssintegration

// TestHSMS_SelectCloseRace_NoPanicNoZombie exercises the race between
// Close() and an in-flight selectSessionTask.
//
// # Background
//
// Commit b1d9cb4 (fix(hsmsss): join selectSession via taskMgr) wired
// selectSession into taskMgr so that closeConn's taskMgr.Wait() joins it
// before the senderMsgChan drain runs.  Without that fix:
//   - Close racing a Select.req could panic on "send on closed channel".
//   - A zombie selectSessionTask could survive Close and write into stale
//     state on the next Open().
//
// # Race-window mechanism
//
// The test opens an active endpoint against a raw TCP listener that accepts
// the TCP connection but intentionally never sends a Select.rsp (mode=0).
// selectSession calls sendControlMsg → sendMsg which registers a reply channel
// and blocks on the reply-channel select.  taskMgr.Start("selectSessionTask",
// ...) returns ONLY after the goroutine has called wg.Add(1) and signalled its
// started channel (see task.go:427), so once the NotSelected state event
// arrives the goroutine is provably in flight and blocked waiting for the
// Select.rsp.  Calling Close() at that precise moment exercises the race
// window without any runtime.Gosched() or sleep-based synchronisation.
//
// T6 is set to 30 s so the test always exercises the context-cancellation
// path, not the T6-fired timeout path.
//
// # Iteration shape (×100)
//
//  1. mode=0 (stall) → Open → wait NotSelected → Close (races selectSessionTask)
//  2. mode=1 (respond) → Open → wait Selected → Close (clean path; proves no zombie)
//
// A panic anywhere terminates the process and go test reports FAIL.

import (
	"context"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/go-secs/hsms"
	"github.com/arloliu/go-secs/hsmsss"
	"github.com/stretchr/testify/require"
)

func TestHSMS_SelectCloseRace_NoPanicNoZombie(t *testing.T) {
	const iterations = 100

	// mode=0: accept TCP but never reply to Select.req (stall path)
	// mode=1: accept TCP and reply Select.rsp(success)    (respond path)
	var mode atomic.Int32

	// Start a raw TCP listener that either stalls or responds based on mode.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	addr, ok := ln.Addr().(*net.TCPAddr)
	require.True(t, ok, "expected *net.TCPAddr")

	port := addr.Port

	// goroutine pool done signal
	listenerDone := make(chan struct{})
	go func() {
		defer close(listenerDone)
		for {
			c, err := ln.Accept()
			if err != nil {
				// ln was closed — normal shutdown.
				return
			}
			go rawSelectPeer(c, &mode)
		}
	}()

	// Use a long T6 so we never hit the T6 timeout path — we rely purely on
	// context cancellation from Close() interrupting the pending Select.rsp wait.
	activeOpts := []hsmsss.ConnOption{
		hsmsss.WithT3Timeout(5 * time.Second),
		hsmsss.WithT5Timeout(100 * time.Millisecond),
		hsmsss.WithT6Timeout(30 * time.Second), // long: close cancels, not T6
		hsmsss.WithT7Timeout(60 * time.Second), // long: prevent T7 firing during test
		hsmsss.WithT8Timeout(2 * time.Second),
		hsmsss.WithConnectRemoteTimeout(2 * time.Second),
		hsmsss.WithCloseConnTimeout(5 * time.Second),
		hsmsss.WithAutoLinktest(false),
		hsmsss.WithHostRole(),
		hsmsss.WithActive(),
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	cfg, err := hsmsss.NewConnectionConfig("127.0.0.1", port, activeOpts...)
	require.NoError(t, err)

	conn, err := hsmsss.NewConnection(ctx, cfg)
	require.NoError(t, err)

	session := conn.AddSession(testSessionID)

	// stateCh receives every connection state transition.
	// Buffer is large enough to absorb events from all iterations without blocking.
	stateCh := make(chan hsms.ConnState, iterations*8)
	session.AddConnStateChangeHandler(func(_ hsms.Connection, _ hsms.ConnState, cur hsms.ConnState) {
		select {
		case stateCh <- cur:
		default:
		}
	})

	// drainStateCh discards all pending events so each iteration starts clean.
	drainStateCh := func() {
		for {
			select {
			case <-stateCh:
			default:
				return
			}
		}
	}

	// waitFor drains until the target state is seen, with a generous timeout.
	waitFor := func(t *testing.T, target hsms.ConnState, label string) {
		t.Helper()
		deadline := time.NewTimer(10 * time.Second)
		defer deadline.Stop()
		for {
			select {
			case s := <-stateCh:
				if s == target {
					return
				}
			case <-deadline.C:
				t.Fatalf("%s: timed out waiting for state %s", label, target)
			}
		}
	}

	for i := range iterations {
		// --- Phase A: stall path (selectSessionTask in flight during Close) ---
		mode.Store(0) // listener will not reply to Select.req
		drainStateCh()

		require.NoError(t, conn.Open(false), "iter %d: Open (stall phase) failed", i)

		// Wait for NotSelected: at this point selectSessionTask has been
		// registered with taskMgr.Start() and is blocked waiting for Select.rsp.
		waitFor(t, hsms.NotSelectedState, "phase-A NotSelected")

		// Race Close() against the blocked selectSessionTask.
		// A panic here causes the process to crash and the test to FAIL.
		// taskMgr.Wait() inside closeConn will join selectSessionTask before
		// returning, so no zombie can survive.
		closeErr := conn.Close()
		// Close may return a timeout error if the test is unusually slow;
		// what we care about is absence of panic and clean task join.
		_ = closeErr

		// --- Phase B: respond path (proves no zombie state leaked) ---
		mode.Store(1) // listener now replies with Select.rsp(success)
		drainStateCh()

		require.NoError(t, conn.Open(false), "iter %d: Open (respond phase) failed", i)

		// If a zombie selectSessionTask survived phase A and wrote into stale
		// state, the state machine may never reach Selected here.
		waitFor(t, hsms.SelectedState, "phase-B Selected")

		require.NoError(t, conn.Close(), "iter %d: Close (respond phase) failed", i)
	}

	// Shut down the raw listener so the goroutine exits cleanly.
	ln.Close()
	<-listenerDone
}

// rawSelectPeer serves one raw TCP connection.
// mode=0: accept the TCP conn, read the Select.req, and then stall (never reply).
// mode=1: accept the TCP conn, read the Select.req, and reply Select.rsp(success).
// The connection is closed on any I/O error.
func rawSelectPeer(c net.Conn, mode *atomic.Int32) {
	defer c.Close()

	// Read the Select.req frame (4-byte length prefix + 10-byte header).
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(c, lenBuf); err != nil {
		return
	}

	msgLen := int(lenBuf[0])<<24 | int(lenBuf[1])<<16 | int(lenBuf[2])<<8 | int(lenBuf[3])
	if msgLen <= 0 || msgLen > 64 {
		return
	}

	payload := make([]byte, msgLen)
	if _, err := io.ReadFull(c, payload); err != nil {
		return
	}

	// payload[5] is the SType byte; Select.req == 1.
	if len(payload) < 10 || payload[5] != byte(hsms.SelectReqType) {
		return
	}

	if mode.Load() == 0 {
		// Stall: hold the connection open but never reply.
		// Block until the connection is closed (i.e., until Close() cancels it).
		io.ReadFull(c, make([]byte, 1)) //nolint:errcheck
		return
	}

	// mode==1: send Select.rsp(success).
	// System bytes are the last 4 bytes of the 10-byte payload (bytes [6:10]).
	rsp := make([]byte, 14)
	// 4-byte length: 0x00 0x00 0x00 0x0A (10 bytes)
	rsp[0], rsp[1], rsp[2], rsp[3] = 0x00, 0x00, 0x00, 0x0A
	// Header[0:2] = 0xFFFF (session ID wildcard for Select.rsp)
	rsp[4], rsp[5] = 0xFF, 0xFF
	// Header[2] = 0x00
	rsp[6] = 0x00
	// Header[3] = select status 0 (success)
	rsp[7] = byte(hsms.SelectStatusSuccess)
	// Header[4] = PType 0x00
	rsp[8] = 0x00
	// Header[5] = SType 0x02 (Select.rsp)
	rsp[9] = byte(hsms.SelectRspType)
	// Header[6:10] = system bytes (echo from request)
	copy(rsp[10:14], payload[6:10])

	c.Write(rsp) //nolint:errcheck

	// Keep the connection alive until the peer closes it.
	io.ReadFull(c, make([]byte, 1)) //nolint:errcheck
}
