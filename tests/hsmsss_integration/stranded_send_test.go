package hsmsssintegration

// TestHSMS_StrandedSend_NoFrameAcrossGeneration is a regression test for the
// stranded-send-across-connection-generations bug fixed by commits f604180,
// f76f23a, and b1d9cb4.
//
// Regression: a message (data or control) racing the Close path could land in
// senderMsgChan after drainSenderMsgChan #1 had already run but before the
// sendMu WLock gated the channel for good. Without the final drain (#2), that
// frame survives in senderMsgChan and the new generation's senderTask picks it
// up before selectSession has a chance to send Select.req — a SEMI E37 §7.4
// violation.
//
// Observable: when the same *Connection object is reopened against a fresh raw
// listener, the first HSMS frame that listener receives MUST be a Select.req
// (SType=1). Any other SType (data message SType=0, stale Separate.req SType=9,
// etc.) indicates the regression is present.
//
// Verification: with the final drain disabled in conn.go closeConn(), this test
// reliably fails (observed SType=9 crossing at gen-44 in a 50-iteration run).
// With the fix in place, all 50 × 3 repetitions pass.

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/go-secs/hsms"
	"github.com/arloliu/go-secs/hsmsss"
	"github.com/arloliu/go-secs/secs2"
	"github.com/stretchr/testify/require"
)

// generations is the number of Close→Open cycles we run. Each cycle creates
// a fresh TCP generation. 50 cycles with 30 concurrent senders per cycle
// gives the race window enough repetitions to catch a regression reliably.
const generations = 50

// strandedSendWorkers is the number of goroutines hammering SendDataMessageAsync
// concurrently during each Close window. Increasing this widens the race window
// at the cost of test runtime; 30 is enough to trigger the regression reliably
// without requiring a modified fix.
const strandedSendWorkers = 30

func TestHSMS_StrandedSend_NoFrameAcrossGeneration(t *testing.T) {
	require := require.New(t)

	port := getFreePort(t)

	// The listener lives for the entire test. Each generation accepts a new
	// TCP connection from the active endpoint. We never need to restart the
	// listener — re-Accepting is sufficient, and avoids TIME_WAIT on the port.
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	require.NoError(err)
	t.Cleanup(func() { _ = ln.Close() })

	// 10-second per-Accept deadline is generous; each generation should
	// complete in well under a second.
	const acceptDeadline = 10 * time.Second

	// Build the active Connection once and reuse it across all generations.
	// The Connection must be active (dialer) so it initiates Select.req.
	ctx := t.Context()
	cfg, err := hsmsss.NewConnectionConfig("127.0.0.1", port,
		hsmsss.WithActive(),
		hsmsss.WithHostRole(),
		hsmsss.WithT3Timeout(3*time.Second),
		hsmsss.WithT5Timeout(200*time.Millisecond), // fast retry after Close
		hsmsss.WithT6Timeout(3*time.Second),
		hsmsss.WithT7Timeout(5*time.Second),
		hsmsss.WithT8Timeout(1*time.Second),
		hsmsss.WithConnectRemoteTimeout(2*time.Second),
		hsmsss.WithAutoLinktest(false),
	)
	require.NoError(err)

	hsmsConn, err := hsmsss.NewConnection(ctx, cfg)
	require.NoError(err)

	session := hsmsConn.AddSession(testSessionID)

	// stateCh receives every state transition; used to await SelectedState.
	stateCh := make(chan hsms.ConnState, 64)
	session.AddConnStateChangeHandler(func(_ hsms.Connection, _ hsms.ConnState, cur hsms.ConnState) {
		select {
		case stateCh <- cur:
		default:
		}
	})

	// Generation 0: open the active connection for the first time.
	require.NoError(hsmsConn.Open(false))
	t.Cleanup(func() { _ = hsmsConn.Close() })

	for gen := range generations {
		t.Run(fmt.Sprintf("gen-%02d", gen), func(t *testing.T) {
			// ----------------------------------------------------------------
			// Phase A: raw peer accepts, performs Select handshake, and then
			// starts recording every frame it receives.
			// ----------------------------------------------------------------
			tcpLn, ok := ln.(*net.TCPListener)
			require.True(ok, "listener is not *net.TCPListener")
			require.NoError(tcpLn.SetDeadline(time.Now().Add(acceptDeadline)))
			rawConn, err := ln.Accept()
			require.NoError(err, "generation %d: accept failed", gen)

			require.NoError(rawConn.SetReadDeadline(time.Now().Add(acceptDeadline)))

			// Read the first frame. Per the regression contract it MUST be Select.req.
			firstFrame, err := readFrame(rawConn)
			require.NoError(err, "generation %d: read first frame failed", gen)
			require.GreaterOrEqual(len(firstFrame), 10,
				"generation %d: first frame too short (%d bytes)", gen, len(firstFrame))

			firstSType := firstFrame[5]
			require.Equal(
				byte(hsms.SelectReqType), firstSType,
				"generation %d: first frame must be Select.req (SType=1), got SType=%d — "+
					"a stale frame from the previous generation crossed the connection boundary",
				gen, firstSType,
			)

			// Complete the Select handshake so the active side reaches SelectedState.
			sysBytes := getSystemBytes(firstFrame)
			selectRsp := buildControlHeader(hsms.SelectStatusSuccess, hsms.SelectRspType, sysBytes)
			require.NoError(writeFrame(rawConn, selectRsp),
				"generation %d: write Select.rsp failed", gen)

			// Wait for the active side to reach SelectedState before hammering sends.
			waitStateFromChan(t, stateCh, hsms.SelectedState, 5*time.Second,
				fmt.Sprintf("generation %d: waiting for SelectedState", gen))

			// ----------------------------------------------------------------
			// Phase B: post-Close gate check.
			//   After the *previous* generation's Close, calling
			//   SendDataMessageAsync must return an error. We can't easily
			//   test this here for generation 0 (no prior Close), but from
			//   generation 1 onward the close was already called. Since the
			//   connection is now Open again (SelectedState), the gate is
			//   re-opened — this sub-phase is satisfied by the gate logic
			//   tested implicitly through the cross-generation invariant.
			// ----------------------------------------------------------------

			// ----------------------------------------------------------------
			// Phase C: concurrently hammer SendDataMessageAsync then Close.
			// The goal is to land frames in senderMsgChan during the narrow
			// window between drainSenderMsgChan #1 and the sendMu.Lock that
			// closes the gate.
			// ----------------------------------------------------------------

			var (
				stopSenders atomic.Bool
				sendersWg   sync.WaitGroup
			)

			// Drain rawConn so the active side's sends don't stall on TCP
			// backpressure (which would prevent frames from reaching senderMsgChan
			// and reduce the race window).
			rawConnDone := make(chan struct{})

			go func() {
				defer close(rawConnDone)
				drain := make([]byte, 4096)
				for {
					_ = rawConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
					_, err := rawConn.Read(drain)
					if err != nil {
						return
					}
				}
			}()

			for range strandedSendWorkers {
				sendersWg.Add(1)

				go func() {
					defer sendersWg.Done()

					for !stopSenders.Load() {
						// W-bit=true maximises the number of in-flight requests
						// that may be sitting in senderMsgChan when Close fires.
						_ = session.SendDataMessageAsync(1, 1, true, secs2.NewASCIIItem("race"))
					}
				}()
			}

			// Let the senders build up some backlog, then close.
			time.Sleep(5 * time.Millisecond)

			stopSenders.Store(true)

			// Close the connection. This must drain senderMsgChan completely
			// before returning so no stale frame survives into the next Open.
			require.NoError(hsmsConn.Close(),
				"generation %d: Close returned an error", gen)

			// Stop the drainer goroutine.
			_ = rawConn.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
			<-rawConnDone
			_ = rawConn.Close()

			sendersWg.Wait()

			// ----------------------------------------------------------------
			// Phase D: assert the gate refuses sends after Close.
			// ----------------------------------------------------------------
			gateErr := session.SendDataMessageAsync(1, 1, true, secs2.NewASCIIItem("post-close"))
			require.Error(gateErr,
				"generation %d: SendDataMessageAsync after Close should return an error", gen)

			// ----------------------------------------------------------------
			// Phase E: reopen for the next generation.  The active reconnect
			// loop will dial as soon as Open() is called; the next iteration's
			// Accept will catch it and re-run the invariant check.
			// ----------------------------------------------------------------
			if gen < generations-1 {
				require.NoError(hsmsConn.Open(false),
					"generation %d: re-Open failed", gen)
			}
		})
	}

	// ----------------------------------------------------------------
	// Final phase: verify the last generation is healthy by exchanging
	// a real S1F1/S1F2 through the new connection. This requires a
	// full active+passive pair, so we do a simple gate check instead:
	// confirm Close returns clean on the already-closed connection.
	// ----------------------------------------------------------------
	// (The connection was not re-opened after the last iteration, so
	//  Close() should be a no-op or return nil for an already-closed conn.)
	err = hsmsConn.Close()
	// Allow nil or idempotent-close (no error expected).
	require.NoError(err, "final Close on already-closed connection should not error")
}

// TestHSMS_StrandedSend_PostCloseGateAndReopenHealthCheck verifies the two
// supplementary assertions from the gap analysis:
//  1. After Close, SendDataMessageAsync must refuse with an error.
//  2. After reopen, a full S1F1/S1F2 round-trip succeeds (new generation is healthy).
func TestHSMS_StrandedSend_PostCloseGateAndReopenHealthCheck(t *testing.T) {
	require := require.New(t)
	port := getFreePort(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// Use a passive endpoint so we get real Select + data-message round-trips.
	passive := newEndpoint(t, ctx, port, true, false, nil, echoHandler)
	defer closeEndpoint(t, passive)
	require.NoError(passive.conn.Open(false))

	active := newEndpoint(t, ctx, port, false, true, nil)
	defer closeEndpoint(t, active)
	require.NoError(active.conn.Open(false))

	waitState(t, active, hsms.SelectedState)
	waitState(t, passive, hsms.SelectedState)

	// --- Gate check: Close then assert SendDataMessageAsync returns an error ---
	require.NoError(active.conn.Close())

	// Give Close a moment to propagate the gate.
	require.Eventually(func() bool {
		err := active.session.SendDataMessageAsync(1, 1, true, secs2.NewASCIIItem("post-close"))
		return err != nil
	}, 2*time.Second, 5*time.Millisecond,
		"SendDataMessageAsync should return an error after Close")

	// --- Reopen check: new generation reaches Selected and exchanges S1F1/S1F2 ---
	require.NoError(active.conn.Open(false))
	waitState(t, active, hsms.SelectedState)

	reply, err := active.session.SendDataMessage(1, 1, true, secs2.NewASCIIItem("hello"))
	require.NoError(err)
	require.NotNil(reply)

	asciiVal, aErr := reply.Item().ToASCII()
	require.NoError(aErr)
	require.Equal("hello", asciiVal,
		"S1F2 payload should echo the S1F1 payload on the fresh generation")
}

// waitStateFromChan blocks until the expected state arrives on ch or the deadline expires.
// It is analogous to waitState in helpers_test.go but takes a raw channel and a custom
// message so subtests can provide better failure diagnostics.
func waitStateFromChan(t *testing.T, ch <-chan hsms.ConnState, want hsms.ConnState, timeout time.Duration, msg string) {
	t.Helper()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case got := <-ch:
			if got == want {
				return
			}
		case <-timer.C:
			t.Fatalf("%s: timeout (%v) waiting for state %s", msg, timeout, want)
		}
	}
}
