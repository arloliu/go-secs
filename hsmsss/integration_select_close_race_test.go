package hsmsss

// select_close_race_test.go — v2 port of the v1
// tests/hsmsss_integration/select_close_race_test.go regression.
//
// TestHSMS_SelectCloseRace_NoPanicNoZombie exercises the race between Close() and an in-flight
// select handshake. The active endpoint dials a raw TCP listener that accepts the connection but
// intentionally never sends a Select.rsp (mode=0): the FSM reaches NotSelected and the select task
// blocks waiting for the reply. Calling Close() at that moment must join the in-flight select task
// (bounded, no "send on closed channel" panic, no zombie surviving into the next Open).
//
// Iteration shape (×N):
//  1. mode=0 (stall)   → Open → wait NotSelected → Close (races the in-flight select task)
//  2. mode=1 (respond) → Open → wait Selected    → Close (clean path; proves no zombie leaked)
//
// T6 is 30 s and T7 60 s so the stall path is exercised via Close-driven cancellation, not a timer.
// Re-pointed to v2: New/NewConfig, Open(ctx, OpenBackground), State()-polled waits, WithCloseTimeout,
// and assertCleanShutdown after the final Close. A panic anywhere crashes the process → test FAIL.

import (
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/stretchr/testify/require"
)

func TestHSMS_SelectCloseRace_NoPanicNoZombie(t *testing.T) {
	const iterations = 100

	// mode=0: accept TCP but never reply to Select.req (stall path)
	// mode=1: accept TCP and reply Select.rsp(success)    (respond path)
	var mode atomic.Int32

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	addr, ok := ln.Addr().(*net.TCPAddr)
	require.True(t, ok, "expected *net.TCPAddr")
	port := addr.Port

	listenerDone := make(chan struct{})
	go func() {
		defer close(listenerDone)
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return // ln closed — normal shutdown
			}
			go rawSelectPeer(c, &mode)
		}
	}()

	ctx := t.Context()

	// Long T6/T7 so we never hit a timer path — we rely purely on Close-driven context
	// cancellation interrupting the pending Select.rsp wait. WithCloseTimeout bounds teardown.
	cfg, err := NewConfig("127.0.0.1", port,
		WithActive(),
		WithConnectionOption(hsms.WithT3(5*time.Second)),
		WithConnectionOption(hsms.WithT5(100*time.Millisecond)),
		WithConnectionOption(hsms.WithT6(30*time.Second)), // long: Close cancels, not T6
		WithConnectionOption(hsms.WithT7(60*time.Second)), // long: prevent T7 firing mid-test
		WithConnectionOption(hsms.WithT8(2*time.Second)),
		WithConnectionOption(hsms.WithCloseTimeout(5*time.Second)),
		WithConnectionOption(hsms.WithLinktestInterval(0)),
	)
	require.NoError(t, err)

	conn, err := New(cfg)
	require.NoError(t, err)

	// waitFor polls conn.State() (require.Eventually, never time.Sleep) until the target is reached.
	waitFor := func(t *testing.T, target hsms.ConnState, label string) {
		t.Helper()
		require.Eventuallyf(t, func() bool {
			return conn.State() == target
		}, 10*time.Second, 5*time.Millisecond, "%s: timed out waiting for state %s", label, target)
	}

	for i := range iterations {
		// --- Phase A: stall path (select task in flight during Close) ---
		mode.Store(0)

		require.NoErrorf(t, conn.Open(ctx, hsms.OpenBackground), "iter %d: Open (stall phase)", i)

		// At NotSelected the select task is registered and blocked waiting for Select.rsp.
		waitFor(t, hsms.NotSelectedState, "phase-A NotSelected")

		// Race Close() against the blocked select task. A panic here crashes the process.
		// Close must return bounded (teardown is internally bounded by WithCloseTimeout); we assert
		// it does not hang. The error itself may be a bounded ErrCloseTimeout under load — what we
		// require is no panic and a prompt return.
		start := time.Now()
		_ = conn.Close()
		require.Lessf(t, time.Since(start), 15*time.Second, "iter %d: phase-A Close did not return bounded", i)

		waitFor(t, hsms.NotConnectedState, "phase-A NotConnected")

		// --- Phase B: respond path (proves no zombie select task leaked) ---
		mode.Store(1)

		require.NoErrorf(t, conn.Open(ctx, hsms.OpenBackground), "iter %d: Open (respond phase)", i)

		// If a zombie select task from phase A wrote into stale state, the FSM may never reach
		// Selected here.
		waitFor(t, hsms.SelectedState, "phase-B Selected")

		require.NoErrorf(t, conn.Close(), "iter %d: Close (respond phase)", i)
	}

	// After the final (clean) Close the connection must be quiesced.
	assertCleanShutdown(t, conn)

	// Shut down the raw listener so its accept goroutine exits cleanly.
	ln.Close()
	<-listenerDone
}

// rawSelectPeer serves one raw TCP connection.
// mode=0: read the Select.req, then stall (never reply) until the peer closes the connection.
// mode=1: read the Select.req, then reply Select.rsp(success).
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
		// Stall: hold the connection open but never reply; block until the peer (Close) closes it.
		io.ReadFull(c, make([]byte, 1)) //nolint:errcheck
		return
	}

	// mode==1: send Select.rsp(success). System bytes echo the request (payload[6:10]).
	rsp := make([]byte, 14)
	rsp[0], rsp[1], rsp[2], rsp[3] = 0x00, 0x00, 0x00, 0x0A // length = 10
	rsp[4], rsp[5] = 0xFF, 0xFF                             // header[0:2] = session ID wildcard
	rsp[6] = 0x00                                           // header[2]
	rsp[7] = byte(hsms.SelectStatusSuccess)                 // header[3] = select status
	rsp[8] = 0x00                                           // header[4] = PType
	rsp[9] = byte(hsms.SelectRspType)                       // header[5] = SType (Select.rsp)
	copy(rsp[10:14], payload[6:10])                         // header[6:10] = echoed system bytes

	c.Write(rsp) //nolint:errcheck

	// Keep the connection alive until the peer closes it.
	io.ReadFull(c, make([]byte, 1)) //nolint:errcheck
}
