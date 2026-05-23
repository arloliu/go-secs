package secs1integration

import (
	"context"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/go-secs/hsms"
	"github.com/arloliu/go-secs/secs1"
	"github.com/stretchr/testify/require"
)

// TestSECS1_T4_Expires_AbortsMultiBlock verifies that when the T4 inter-block
// timeout fires between block 1 and block 2 of a multi-block message:
//
//   - The equipment handler is never invoked (message is aborted).
//   - Goroutines spawned by startT4 settle (no leak).
//   - The assembler is not corrupted: a subsequent clean S1F1 is delivered
//     normally.
//
// Note: MinT4Timeout is 1 second (SEMI E4 Table 4 lower bound). The peer
// sleeps T4 + 200ms to ensure expiry.
func TestSECS1_T4_Expires_AbortsMultiBlock(t *testing.T) {
	t.Parallel()

	const t4 = secs1.MinT4Timeout // 1s — minimum allowed by the config validator

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	port := getFreePort(t)

	var handlerCalled atomic.Bool
	handler := func(msg *hsms.DataMessage, s hsms.Session) {
		handlerCalled.Store(true)
		if msg.FunctionCode()%2 == 1 {
			_ = s.ReplyDataMessage(msg, msg.Item())
		}
	}

	eqp := newEndpointWithOpts(t, ctx, port, true, false,
		secs1.WithT4Timeout(t4),
		secs1.WithValidateDataMessage(true),
	)
	eqp.session.AddDataMessageHandler(handler)
	require.NoError(t, eqp.conn.Open(false))
	defer closeEndpoint(t, eqp)

	// Capture goroutine count before the scenario.
	goroutinesBefore := runtime.NumGoroutine()

	// Peer: send block 1 (not-last), wait > T4, then send block 2 (last).
	// Equipment should abort after T4 expires; block 2 arrives too late and
	// starts a new (orphaned) open-message that will itself time out.
	specs := []blockSpec{
		// Block 1: start of multi-block (eBit=false).
		{stream: 6, function: 1, wBit: true, deviceID: 10, sysbytes: 0x00000099, blockNum: 1, eBit: false},
		// Block 2: sent after T4 has already expired — equipment has already
		// aborted the open message.  This block arrives as if it were an
		// unexpected continuation and will be rejected (wrong block number for
		// a new message, since the open message no longer exists).
		{stream: 6, function: 1, wBit: true, deviceID: 10, sysbytes: 0x00000099, blockNum: 2, eBit: true,
			sendDelay: t4 + 200*time.Millisecond},
	}

	peerCh := make(chan customPeerResult, 1)
	go func() {
		peerCh <- runCustomBlockPeer("127.0.0.1", port, specs)
	}()

	// Wait for the peer to finish (the delay inside is ~T4+200ms = ~1.2s).
	var pr customPeerResult
	select {
	case pr = <-peerCh:
	case <-ctx.Done():
		t.Fatal("peer goroutine did not finish in time")
	}
	require.NoError(t, pr.err, "peer encountered I/O error")

	// The equipment must NOT have delivered the (now-aborted) multi-block message.
	require.False(t, handlerCalled.Load(),
		"handler was called, but the message should have been aborted by T4 expiry")

	// Goroutine count should settle back to near the baseline.
	// The T4 goroutine exits when the cancel channel is closed or the timer fires.
	// Allow a short settle window.
	require.Eventually(t, func() bool {
		return runtime.NumGoroutine() <= goroutinesBefore+2
	}, 3*time.Second, 50*time.Millisecond,
		"goroutine count did not settle after T4 expiry (possible leak): before=%d, now=%d",
		goroutinesBefore, runtime.NumGoroutine())

	// Recovery: send a clean S1F1 via a fresh host endpoint; equipment must accept it.
	// The peer disconnected, so equipment re-enters accept loop.
	var recoveryReceived atomic.Bool
	eqp.session.AddDataMessageHandler(func(msg *hsms.DataMessage, s hsms.Session) {
		if msg.StreamCode() == 1 && msg.FunctionCode() == 1 {
			recoveryReceived.Store(true)
			if msg.FunctionCode()%2 == 1 {
				_ = s.ReplyDataMessage(msg, msg.Item())
			}
		}
	})

	recoveryHost := newEndpoint(t, ctx, port, false, true)
	require.NoError(t, recoveryHost.conn.Open(true))
	defer closeEndpoint(t, recoveryHost)

	waitSelected(t, recoveryHost, 5*time.Second)

	reply, err := recoveryHost.session.SendDataMessage(1, 1, true, nil)
	require.NoError(t, err, "recovery S1F1 send failed after T4 expiry")
	if reply != nil {
		reply.Free()
	}

	require.Eventually(t, recoveryReceived.Load, 3*time.Second, 50*time.Millisecond,
		"equipment did not receive recovery message — assembler corrupted after T4 expiry")
}

// TestSECS1_T4_DoesNotExpire_DeliversMultiBlock verifies that when blocks
// arrive within the T4 window, the multi-block message is assembled and
// delivered normally, and no goroutine leak occurs from the cancelled timer.
func TestSECS1_T4_DoesNotExpire_DeliversMultiBlock(t *testing.T) {
	t.Parallel()

	const t4 = 2 * secs1.MinT4Timeout // 2s — well above the 300ms inter-block gap used below

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	port := getFreePort(t)

	var handlerCalled atomic.Bool
	handler := func(msg *hsms.DataMessage, s hsms.Session) {
		handlerCalled.Store(true)
		if msg.FunctionCode()%2 == 1 {
			_ = s.ReplyDataMessage(msg, msg.Item())
		}
	}

	eqp := newEndpointWithOpts(t, ctx, port, true, false,
		secs1.WithT4Timeout(t4),
		secs1.WithValidateDataMessage(true),
	)
	eqp.session.AddDataMessageHandler(handler)
	require.NoError(t, eqp.conn.Open(false))
	defer closeEndpoint(t, eqp)

	goroutinesBefore := runtime.NumGoroutine()

	// Peer: send block 1, wait 300ms (< T4), send block 2 (last).
	// Equipment assembles the 2-block message and delivers it.
	specs := []blockSpec{
		// Block 1: start of multi-block.
		{stream: 6, function: 1, wBit: false, deviceID: 10, sysbytes: 0x000000AA, blockNum: 1, eBit: false},
		// Block 2: arrives well within T4.
		{stream: 6, function: 1, wBit: false, deviceID: 10, sysbytes: 0x000000AA, blockNum: 2, eBit: true,
			sendDelay: 300 * time.Millisecond},
	}

	peerCh := make(chan customPeerResult, 1)
	go func() {
		peerCh <- runCustomBlockPeer("127.0.0.1", port, specs)
	}()

	var pr customPeerResult
	select {
	case pr = <-peerCh:
	case <-ctx.Done():
		t.Fatal("peer goroutine did not finish in time")
	}
	require.NoError(t, pr.err, "peer encountered I/O error")

	// Equipment must have delivered the assembled 2-block message.
	require.Eventually(t, handlerCalled.Load, 3*time.Second, 50*time.Millisecond,
		"equipment handler was not called — 2-block message was not delivered within T4 window")

	// T4 timer was cancelled (block arrived in time); goroutine count should settle.
	require.Eventually(t, func() bool {
		return runtime.NumGoroutine() <= goroutinesBefore+2
	}, 3*time.Second, 50*time.Millisecond,
		"goroutine count did not settle after successful multi-block delivery (possible T4 goroutine leak): before=%d, now=%d",
		goroutinesBefore, runtime.NumGoroutine())
}
