package secs1integration

// TestSECS1_UserHandlerReceivesLifecycleTransitionsOnce is a parity regression
// test for SECS-I that mirrors TestLifecycle_UserHandlerReceivesAllTransitions
// in tests/hsmsss_integration/lifecycle_test.go.
//
// Both transports share hsms.ConnStateMgr, so commit 7bb93eb's
// idempotent-async invariant (no prev==next event) and the handler-delivery
// guarantee introduced by the ToNotConnected-before-stateMgr.Stop ordering
// must hold for SECS-I too.
//
// Regression guarded: secs1/conn.go Close() was previously calling
// stateMgr.Stop() before stateMgr.ToNotConnected(), which caused the
// async-handler dispatcher to exit before it could deliver the terminal
// Selected→NotConnected event to user-registered ConnStateChangeHandlers.
// Commit 069b92f fixed this by mirroring the ordering from hsmsss/conn.go.
//
// What this test asserts per cycle (active side):
//
//	*→NotSelected, NotSelected→Selected, Selected→NotConnected (in order)
//
// Additional invariants:
//   - No (prev == next) tuple anywhere across all events.
//   - Total observed cycles == 5.
import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/go-secs/hsms"
	"github.com/arloliu/go-secs/secs2"
	"github.com/stretchr/testify/require"
)

func TestSECS1_UserHandlerReceivesLifecycleTransitionsOnce(t *testing.T) {
	const cycles = 5

	require := require.New(t)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	port := getFreePort(t)

	type transition struct{ prev, next hsms.ConnState }
	type observer struct {
		mu     sync.Mutex
		events []transition
	}

	newObs := func() *observer {
		return &observer{events: make([]transition, 0, cycles*4)}
	}
	activeObs := newObs()

	record := func(o *observer) hsms.ConnStateChangeHandler {
		return func(_ hsms.Connection, prev, next hsms.ConnState) {
			o.mu.Lock()
			o.events = append(o.events, transition{prev, next})
			o.mu.Unlock()
		}
	}

	// Build fresh endpoints once; reuse across cycles to verify that
	// Start/Stop cycles preserve handler registration and do not leak state.
	eqp := newEndpoint(t, ctx, port, true, false, runEchoReply)
	host := newEndpoint(t, ctx, port, false, true)

	// Register async state-change handler on the active (host) side only.
	// The passive side's state machine is not the primary regression target here.
	host.session.AddConnStateChangeHandler(record(activeObs))

	t.Cleanup(func() {
		closeEndpoint(t, host)
		closeEndpoint(t, eqp)
	})

	for i := range cycles {
		// Open passive first so active can connect immediately.
		require.NoError(eqp.conn.Open(false), "cycle %d: eqp open", i)
		require.NoError(host.conn.Open(true), "cycle %d: host open", i)

		waitSelected(t, host, 5*time.Second)
		waitSelected(t, eqp, 5*time.Second)

		// Send one S1F1 to confirm the connection is functional this cycle.
		reply, err := host.session.SendDataMessage(1, 1, true, secs2.NewASCIIItem(fmt.Sprintf("ping-%d", i)))
		require.NoError(err, "cycle %d: SendDataMessage", i)
		require.NotNil(reply, "cycle %d: reply is nil", i)
		reply.Free()

		// Close active (host) first — this is what exercises the
		// Selected→NotConnected event delivery guarantee.
		require.NoError(host.conn.Close(), "cycle %d: host close", i)
		require.NoError(eqp.conn.Close(), "cycle %d: eqp close", i)

		// Drain the internal selectedCh so waitSelected in the next cycle
		// starts from an empty queue.
		drainSecs1StateCh(host.selectedCh)
		drainSecs1StateCh(eqp.selectedCh)
	}

	// --- Assertions ---

	activeObs.mu.Lock()
	defer activeObs.mu.Unlock()

	t.Logf("active side recorded %d events total", len(activeObs.events))
	for idx, ev := range activeObs.events {
		t.Logf("  [%02d] %s → %s", idx, ev.prev, ev.next)
	}

	// Invariant 1: no idempotent (prev == next) tuple.
	for idx, ev := range activeObs.events {
		if ev.prev == ev.next {
			t.Fatalf("active: event[%d] has prev==next==%s; idempotent-async invariant broken",
				idx, ev.prev)
		}
	}

	// Invariant 2: each cycle must contain the three anchor transitions in order:
	//   *→NotSelected, NotSelected→Selected, Selected→NotConnected
	//
	// We allow incidental intermediate events (e.g. Connecting on transient
	// retries), but require the three anchors to appear in this order within
	// each lifecycle scan.
	anchors := []hsms.ConnState{
		hsms.NotSelectedState,
		hsms.SelectedState,
		hsms.NotConnectedState,
	}

	var seenCycles int
	i := 0
	for i < len(activeObs.events) {
		// Scan forward for the rising edge (event whose .next == NotSelected).
		for i < len(activeObs.events) && activeObs.events[i].next != hsms.NotSelectedState {
			i++
		}
		if i >= len(activeObs.events) {
			break
		}

		// Verify the subsequent anchor transitions appear in order.
		for ai, expected := range anchors {
			if i >= len(activeObs.events) {
				t.Fatalf("active: cycle %d truncated — missing anchor %s at anchor index %d (full sequence: %+v)",
					seenCycles, expected, ai, activeObs.events)
			}
			if activeObs.events[i].next != expected {
				t.Fatalf("active: cycle %d anchor %d: want next=%s, got next=%s (full sequence: %+v)",
					seenCycles, ai, expected, activeObs.events[i].next, activeObs.events)
			}
			i++
		}
		seenCycles++
	}

	require.Equalf(cycles, seenCycles,
		"active: expected %d observed cycles, got %d (full sequence: %+v)",
		cycles, seenCycles, activeObs.events)

	// Invariant 3: total event count must equal cycles × anchors.
	// The SECS-I active side does not emit a Connecting event during normal
	// open/close cycles (SECS-I has no select handshake; tryConnect goes
	// directly to NotSelected). Any extra event here signals a regression.
	require.Equalf(cycles*len(anchors), len(activeObs.events),
		"active: expected exactly %d events (%d cycles × %d anchors), got %d (full sequence: %+v)",
		cycles*len(anchors), cycles, len(anchors), len(activeObs.events), activeObs.events)
}

// drainSecs1StateCh empties the endpoint's selected-signal channel so the
// next waitSelected call starts from a clean state.
func drainSecs1StateCh(ch chan struct{}) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}
