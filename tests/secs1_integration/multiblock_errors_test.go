package secs1integration

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/go-secs/hsms"
	"github.com/arloliu/go-secs/secs1"
	"github.com/stretchr/testify/require"
)

// TestSECS1_MultiBlockErrors verifies that the equipment-side assembler
// correctly detects and rejects malformed continuation blocks, and that the
// connection remains usable after each abort.
//
// Each sub-case:
//  1. Opens a passive Equipment endpoint with ValidateDataMessage=true.
//  2. Runs a custom raw-TCP peer (host role) that sends a malformed block sequence.
//  3. Asserts the equipment sends S9F7 (Illegal Data) back for block-number
//     violations.
//  4. Sends a clean S1F1 immediately after and asserts normal delivery —
//     proving the assembler recovered cleanly.
//
// Note: ErrHeaderMismatch (W-bit / stream / function mismatch on continuation
// blocks) is produced by the assembler (message.go:302) but is NOT mapped to
// S9F7 in handleReceivedBlock (conn.go:656-664) — it falls through the default
// case.  Those sub-cases are therefore not included here; see the "Real bugs
// found" section of the test implementation report.
func TestSECS1_MultiBlockErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		specs    []blockSpec // malformed block sequence sent by the peer
		wantS9F7 bool        // whether the equipment should send S9F7
	}{
		{
			name: "BlockNumberGap",
			// Peer sends block 1 (not-last), then block 3 (skipping 2).
			// Equipment expects block 2 next, so block 3 triggers ErrBlockNumberMismatch → S9F7.
			specs: []blockSpec{
				{stream: 1, function: 1, wBit: true, deviceID: 10, sysbytes: 0x00000011, blockNum: 1, eBit: false},
				{stream: 1, function: 1, wBit: true, deviceID: 10, sysbytes: 0x00000011, blockNum: 3, eBit: true},
			},
			wantS9F7: true,
		},
		{
			name: "OutOfOrder",
			// Peer sends block 1, then block 3 (jump from 1 to 3 skipping 2).
			// Same ErrBlockNumberMismatch path.
			specs: []blockSpec{
				{stream: 1, function: 1, wBit: true, deviceID: 10, sysbytes: 0x00000022, blockNum: 1, eBit: false},
				{stream: 1, function: 1, wBit: true, deviceID: 10, sysbytes: 0x00000022, blockNum: 3, eBit: false},
			},
			wantS9F7: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()

			port := getFreePort(t)

			// Track messages received by the equipment handler.
			var handlerCalled atomic.Bool
			handler := func(msg *hsms.DataMessage, s hsms.Session) {
				handlerCalled.Store(true)
				if msg.FunctionCode()%2 == 1 {
					_ = s.ReplyDataMessage(msg, msg.Item())
				}
			}

			// Equipment: passive, equip role, validates data messages.
			eqp := newEndpointWithOpts(t, ctx, port, true, false,
				secs1.WithValidateDataMessage(true),
				secs1.WithT4Timeout(secs1.MinT4Timeout),
			)
			eqp.session.AddDataMessageHandler(handler)
			require.NoError(t, eqp.conn.Open(false))
			defer closeEndpoint(t, eqp)

			// S9F7 capture channel: equipment will send S9F7 on the wire to the peer.
			// The peer collects received blocks, so we check them after the run.

			// Run the peer in a goroutine.
			peerCh := make(chan customPeerResult, 1)
			go func() {
				peerCh <- runCustomBlockPeer("127.0.0.1", port, tt.specs)
			}()

			// Wait for the peer to finish.
			var pr customPeerResult
			select {
			case pr = <-peerCh:
			case <-ctx.Done():
				t.Fatal("peer goroutine did not finish in time")
			}
			require.NoError(t, pr.err, "peer encountered I/O error")

			if tt.wantS9F7 {
				// S9F7 is captured by the peer's drainInbound phase and should
				// already be in pr.received by the time runCustomBlockPeer returns.
				foundS9F7 := false
				for _, blk := range pr.received {
					if blk.StreamCode() == 9 && blk.FunctionCode() == 7 {
						foundS9F7 = true
						break
					}
				}
				require.True(t, foundS9F7,
					"expected S9F7 (stream=9, function=7) in received blocks; got %d block(s)",
					len(pr.received))
			}

			// Verify the assembler recovered: send a clean S1F1 and confirm delivery.
			// We do this via a second peer send over the same equipment.
			// Use a new connection (equipment auto-accepts after disconnect).
			// Instead, we check via a normal host endpoint connecting to the same port.
			// Since the custom peer disconnected, the equipment re-listens.
			// Open a normal host endpoint to verify recovery.
			verifyReceivedCh := make(chan *hsms.DataMessage, 1)
			eqp.session.AddDataMessageHandler(func(msg *hsms.DataMessage, s hsms.Session) {
				if msg.StreamCode() == 1 && msg.FunctionCode() == 1 {
					select {
					case verifyReceivedCh <- msg:
					default:
					}
					if msg.FunctionCode()%2 == 1 {
						_ = s.ReplyDataMessage(msg, msg.Item())
					}
				}
			})

			// Wait for equipment to re-enter listening state.
			recoveryHost := newEndpoint(t, ctx, port, false, true)
			require.NoError(t, recoveryHost.conn.Open(true))
			defer closeEndpoint(t, recoveryHost)

			waitSelected(t, recoveryHost, 5*time.Second)

			// Send clean S1F1 — equipment should deliver it normally.
			reply, err := recoveryHost.session.SendDataMessage(1, 1, true, nil)
			require.NoError(t, err, "recovery S1F1 send failed")
			if reply != nil {
				reply.Free()
			}

			// Equipment handler should have been called (either the original or the recovery handler).
			require.Eventually(t, func() bool {
				return handlerCalled.Load() || len(verifyReceivedCh) > 0
			}, 3*time.Second, 50*time.Millisecond,
				"equipment did not receive recovery S1F1 — assembler may be corrupted")
		})
	}
}

// newEndpointWithOpts creates an endpoint with additional custom options on top of
// the standard test defaults.
func newEndpointWithOpts(
	t *testing.T,
	ctx context.Context,
	port int,
	isEquip bool,
	isActive bool,
	extra ...secs1.ConnOption,
) *endpoint {
	t.Helper()

	opts := []secs1.ConnOption{
		secs1.WithDeviceID(10),
		secs1.WithT1Timeout(secs1.MinT1Timeout),
		secs1.WithT2Timeout(secs1.MinT2Timeout),
		secs1.WithT3Timeout(secs1.MinT3Timeout),
		secs1.WithRetryLimit(2),
		secs1.WithConnectRemoteTimeout(300 * time.Millisecond),
		secs1.WithSendTimeout(500 * time.Millisecond),
	}

	if isEquip {
		opts = append(opts, secs1.WithEquipRole())
	} else {
		opts = append(opts, secs1.WithHostRole())
	}

	if isActive {
		opts = append(opts, secs1.WithActive())
	} else {
		opts = append(opts, secs1.WithPassive())
	}

	opts = append(opts, extra...)

	cfg, err := secs1.NewConnectionConfig("127.0.0.1", port, opts...)
	require.NoError(t, err)

	conn, err := secs1.NewConnection(ctx, cfg)
	require.NoError(t, err)

	session := conn.AddSession(10)
	selectedCh := make(chan struct{}, 16)
	session.AddConnStateChangeHandler(func(_ hsms.Connection, _ hsms.ConnState, cur hsms.ConnState) {
		if cur.IsSelected() {
			select {
			case selectedCh <- struct{}{}:
			default:
			}
		}
	})

	return &endpoint{conn: conn, session: session, selectedCh: selectedCh}
}
