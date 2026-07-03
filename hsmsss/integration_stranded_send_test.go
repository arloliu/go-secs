package hsmsss

// stranded_send_test.go — Part B (stranded-send) of the T29b-5 cluster (v2 port of the v1
// tests/hsmsss_integration/stranded_send_test.go). It guards the per-generation frame-isolation
// invariant across Close→Open cycles: a data (or control) frame racing the Close path must NEVER
// survive into the next TCP generation's send path. If it did, the new generation's sender could put
// a stale frame on the wire BEFORE Select.req — a SEMI E37 §7.4 violation (the C1 stale-frame bug).
//
// Observable (regression contract): when the same active *Connection is reopened against a fresh raw
// listener accept, the FIRST HSMS frame that accept receives MUST be a Select.req (SType 1). Any other
// SType (a stale data frame SType 0, a stale Separate.req SType 9, …) means a frame crossed the
// generation boundary. Concurrent senders + a TCP drainer widen the race window (as v1 does); here the
// senders are kept running THROUGH the Close so frames actively race the generation teardown, then
// stopped before the reopen so the next generation starts sender-free and its first frame is purely the
// Select handshake.
//
// v2 ADAPTATIONS: v1 host/equip roles dropped; New / Open(ctx, mode) / hsms.Connection surface;
// SendDataMessageAsync(ctx, ...). The post-Close gate returns an error (data refused while not
// Selected — the B1 gate); after reopen a full S1F1/S1F2 round-trip proves the new generation is
// healthy. Backlog is gated on an attempt counter via require.Eventually (never a fixed Sleep).
//
// The file is package hsmsss (white-box) so it reuses newEndpoint / newEndpointPair / waitState /
// waitSelected / closeEndpoint / echoHandler (integration_helpers_test.go), listenLoopback
// (transport_test.go), peerReadFrame / selectRspFrame (active_test.go). Runs under -race.

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

const (
	// generations is the number of Close→Open cycles. ≥50 gives the race window enough repetitions
	// to catch a cross-generation frame leak reliably.
	strandedGenerations = 50
	// strandedWorkers is the number of goroutines hammering SendDataMessageAsync during each Selected
	// window. More workers widen the race window; 30 reliably triggers the pre-fix regression.
	strandedWorkers = 30
	// strandedBacklog is the number of send attempts to observe before Close, ensuring the workers are
	// actively enqueuing frames (condition-based backlog gate — never a fixed Sleep).
	strandedBacklog = 60
)

// 11. TestHSMS_StrandedSend_NoFrameAcrossGeneration
func TestHSMS_StrandedSend_NoFrameAcrossGeneration(t *testing.T) {
	// Not parallel: 50 generations × 30 concurrent senders is resource-heavy; keep it serial so it
	// does not starve the parallel suite under -race -count.
	ctx := t.Context()

	// One raw listener lives for the whole test; each generation re-Accepts the active's fresh dial.
	ln, port := listenLoopback(t)
	t.Cleanup(func() { _ = ln.Close() })

	const acceptDeadline = 10 * time.Second

	// One active Connection reused across every generation (the shared transport the invariant guards).
	active := newEndpoint(t, port, true, []Option{
		WithConnectionOption(hsms.WithT5(200 * time.Millisecond)), // fast retry after Close
	})

	// Generation 0: first Open.
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	t.Cleanup(func() { _ = active.conn.Close() })

	for gen := range strandedGenerations {
		t.Run(fmt.Sprintf("gen-%02d", gen), func(t *testing.T) {
			// Phase A: accept the active's dial and read the FIRST frame — it MUST be a Select.req.
			require.NoError(t, ln.SetDeadline(time.Now().Add(acceptDeadline)))
			rawConn, err := ln.Accept()
			require.NoErrorf(t, err, "generation %d: accept failed", gen)

			firstFrame, err := peerReadFrame(rawConn, acceptDeadline)
			require.NoErrorf(t, err, "generation %d: read first frame failed", gen)
			require.GreaterOrEqualf(t, len(firstFrame), 10, "generation %d: first frame too short", gen)

			require.Equalf(t, byte(hsms.SelectReqType), firstFrame[5],
				"generation %d: first frame must be Select.req (SType=1), got SType=%d — a stale frame "+
					"from the previous generation crossed the connection boundary", gen, firstFrame[5])

			// Complete the Select handshake so the active reaches Selected.
			_, err = rawConn.Write(selectRspFrame(firstFrame[:10], hsms.SelectStatusSuccess))
			require.NoErrorf(t, err, "generation %d: write Select.rsp failed", gen)
			waitSelected(t, active)

			// Phase B: TCP drainer so the active's sends don't stall on socket-write backpressure
			// (widening the window and keeping the active's send loop flowing).
			drainerDone := make(chan struct{})
			go func() {
				defer close(drainerDone)
				buf := make([]byte, 4096)
				for {
					_ = rawConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
					if _, rerr := rawConn.Read(buf); rerr != nil {
						return
					}
				}
			}()

			// Phase C: hammer SendDataMessageAsync concurrently; keep the senders running THROUGH the
			// Close so frames actively race the generation teardown.
			var (
				stop     atomic.Bool
				attempts atomic.Int64
				wg       sync.WaitGroup
			)
			for range strandedWorkers {
				wg.Go(func() {
					for !stop.Load() {
						_ = active.conn.SendDataMessageAsync(ctx, 1, 1, true, secs2.A("race"))
						attempts.Add(1)
					}
				})
			}

			// Condition-based backlog gate: wait until the workers are actively enqueuing frames.
			require.Eventuallyf(t, func() bool { return attempts.Load() >= strandedBacklog },
				5*time.Second, time.Millisecond, "generation %d: senders never built a backlog", gen)

			// Isolation contract: Close tears down the current generation and abandons its per-generation
			// send channel; the next Open starts a FRESH generation with a fresh channel, so no frame
			// enqueued here can survive into it. Senders are still running — the exact race that isolation
			// must win.
			require.NoErrorf(t, active.conn.Close(), "generation %d: Close returned an error", gen)

			// Stop senders and the drainer.
			stop.Store(true)
			_ = rawConn.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
			<-drainerDone
			_ = rawConn.Close()
			wg.Wait()

			// Phase D: the post-Close gate must refuse sends.
			require.Errorf(t, active.conn.SendDataMessageAsync(ctx, 1, 1, true, secs2.A("post-close")),
				"generation %d: SendDataMessageAsync after Close must return an error", gen)

			// Phase E: reopen for the next generation (the next Accept re-checks the invariant).
			if gen < strandedGenerations-1 {
				require.NoErrorf(t, active.conn.Open(ctx, hsms.OpenBackground), "generation %d: re-Open failed", gen)
			}
		})
	}

	// The connection was not reopened after the last generation; a final Close must be a clean no-op.
	require.NoError(t, active.conn.Close(), "final Close on an already-closed connection must not error")
}

// 12. TestHSMS_StrandedSend_PostCloseGateAndReopenHealthCheck
//
// Two supplementary assertions: (1) after Close, SendDataMessageAsync must refuse with an error (the
// send gate is closed); (2) after reopen, a full S1F1/S1F2 round-trip succeeds (the new generation is
// healthy). Uses a real active+passive pair so the round-trip is genuine.
func TestHSMS_StrandedSend_PostCloseGateAndReopenHealthCheck(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	port := freeLoopbackPort(t)

	passive := newEndpoint(t, port, false, nil, echoHandler)
	active := newEndpoint(t, port, true, nil)
	defer closeEndpoint(t, passive)
	defer closeEndpoint(t, active)

	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	waitSelected(t, active)
	waitSelected(t, passive)

	// --- Gate check: Close, then SendDataMessageAsync must return an error ---
	require.NoError(t, active.conn.Close())
	require.Eventually(t, func() bool {
		return active.conn.SendDataMessageAsync(ctx, 1, 1, true, secs2.A("post-close")) != nil
	}, 2*time.Second, 5*time.Millisecond, "SendDataMessageAsync must return an error after Close")

	// --- Reopen check: new generation reaches Selected and exchanges S1F1/S1F2 ---
	// Closing the active dropped the passive, which re-listens asynchronously. v2's active dial is
	// synchronous, so a reopen that races the passive re-listen returns "connection refused"; retry
	// Open until the re-listen catches up (a failed Open rolls back cleanly and is retryable).
	require.Eventually(t, func() bool {
		return active.conn.Open(ctx, hsms.OpenBackground) == nil
	}, 10*time.Second, 100*time.Millisecond, "active reopen must succeed once the passive re-listens")
	waitSelected(t, active)
	waitSelected(t, passive)

	reply, err := active.conn.SendDataMessage(ctx, 1, 1, true, secs2.A("hello"))
	require.NoError(t, err)
	require.NotNil(t, reply)

	item, err := reply.Item()
	require.NoError(t, err)
	got, err := item.ToASCII()
	require.NoError(t, err)
	require.Equal(t, "hello", got, "S1F2 must echo S1F1 on the fresh generation")
}
