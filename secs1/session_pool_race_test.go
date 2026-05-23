package secs1

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/arloliu/go-secs/hsms"
	"github.com/arloliu/go-secs/secs2"
	"github.com/stretchr/testify/require"
)

// TestSession_MultiHandlerDispatch_ConcurrentSendStressor exercises the
// multi-handler dispatch path under heavy concurrent W-bit send pressure.
//
// HISTORY: originally written as a focused regression stressor for the
// DataMessage pool double-Free race observed in the 2026-05-23 stress run
// (see tmp/secs1-race-investigation.md). It does NOT trigger that race —
// confirmed by passing at -race -count=200 -p $(nproc) in ~23 s. The race
// requires the broader pool-churn context of the full secs1 test binary
// running at -count=50; this stressor in isolation does not reproduce it.
//
// Why it's retained: the multi-handler-with-concurrent-sends pattern is
// itself a real coverage gap — no other secs1 test combines per-handler
// Clone() with this much concurrent send pressure on one connection pair.
// It guards against future regressions in that pattern even though it is
// not the regression guard for the original race.
//
// Why this should fire the race faster than the original:
//
//   - 32-way concurrent SendDataMessage from the host saturates
//     handleOutgoingMessage's defer-Free path across many in-flight
//     outgoing *DataMessage instances.
//   - The 2-handler dispatch on the equipment maximises Clone() activity
//     on the receive path, increasing the rate of pool Put/Get round
//     trips on the receive side.
//   - All traffic flows through the same *secs1.Connection pair, so
//     the active-side outgoing pool churn and the passive-side incoming
//     pool churn run on the same pool with no other tests draining or
//     filling it.
//
// Expected outcome: clean under -race at any count tried so far.
func TestSession_MultiHandlerDispatch_ConcurrentSendStressor(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping pool-race stressor in short mode")
	}

	req := require.New(t)
	ctx := t.Context()
	port := getPort()

	hostComm := newNoHandlerTestComm(ctx, t, port, true, true)
	eqpComm := newNoHandlerTestComm(ctx, t, port, false, false)

	defer func() {
		req.NoError(hostComm.close())
		req.NoError(eqpComm.close())
	}()

	const senders = 32
	const sendsPerWorker = 16

	var handlerCount atomic.Int64

	// Two handlers on the equipment — mirrors the failing test's pattern
	// and forces the per-handler Clone() path on every dispatch.
	eqpComm.session.AddDataMessageHandler(func(msg *hsms.DataMessage, session hsms.Session) {
		handlerCount.Add(1)
		// Reply with cloned payload (mirrors the original test).
		_ = session.ReplyDataMessage(msg, msg.Item().Clone())
		msg.Free()
	})
	eqpComm.session.AddDataMessageHandler(func(msg *hsms.DataMessage, _ hsms.Session) {
		handlerCount.Add(1)
		// Touch the item before freeing — surfaces use-after-Free corruption
		// when the bug is present (Item() on a stale pointer reads whatever
		// dataItem the recycled msg now holds, which the ASCIIItem.Free()
		// below will then mis-Free, snowballing into the secs2 pool race
		// observed in the stress report).
		_ = msg.Item()
		msg.Free()
	})

	req.NoError(eqpComm.open(false))
	req.NoError(hostComm.open(false))

	req.NoError(eqpComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))
	req.NoError(hostComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))

	var wg sync.WaitGroup
	var sendErrs atomic.Int64
	var nilReplies atomic.Int64

	for w := range senders {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := range sendsPerWorker {
				payload := fmt.Sprintf("w%02d-i%02d", workerID, i)
				reply, err := hostComm.session.SendDataMessage(1, 1, true, secs2.A(payload))
				if err != nil {
					sendErrs.Add(1)
					return
				}
				if reply == nil {
					nilReplies.Add(1)
					return
				}
				reply.Free()
			}
		}(w)
	}

	wg.Wait()

	req.Zero(sendErrs.Load(), "%d sends returned errors (a non-race symptom of the same lifecycle bug)", sendErrs.Load())
	req.Zero(nilReplies.Load(), "%d sends got nil reply", nilReplies.Load())

	// Each S1F1 dispatches to BOTH handlers → expected = 2 × senders × sendsPerWorker.
	// Allow a small slack for any messages that didn't complete handler delivery
	// before close, though with Wait() above they all should have.
	expected := int64(senders * sendsPerWorker * 2)
	got := handlerCount.Load()
	req.GreaterOrEqual(got, expected-2, "handlers under-fired: got %d, expected ≥ %d", got, expected-2)
}
