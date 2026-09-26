package secs1

// tcpup_gate_test.go — SECS-I compatibility coverage for the core's TCP-up commit gate.
// secs1 has no generation identity:
// every TCPUp report is unnamed (gen 0), so the core's tcpUpCommitGate now applies to it too.
// This file proves normal active/passive bring-up is unaffected, and that a late, unnamed TCPUp racing a core teardown is refused rather than resurrecting a torn-down link.

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/stretchr/testify/require"
)

// secs1StateEdge is a minimal local mirror of integration/fsm_matrix_test.go's stateEdge —
// this package cannot import an unexported type from a sibling test package,
// and does not need the rest of that harness.
type secs1StateEdge struct {
	prev, next hsms.ConnState
}

// secs1BringUpEdges enumerates the two edges secs1's own back-to-back TCPUp/CommitSelected commits can legally settle at Selected through —
// the ordinary two-step climb, or the collapsed single edge
// when the supervisor goroutine is scheduled late enough to observe both synchronous commits already applied
// (see integration/fsm_matrix_test.go's bringUpSelectedEdges for the full race this mirrors).
var secs1BringUpEdges = []secs1StateEdge{
	{prev: hsms.NotSelectedState, next: hsms.SelectedState},
	{prev: hsms.NotConnectedState, next: hsms.SelectedState},
}

// secs1SettledAtSelected reports whether edges records either legal bring-up shape.
func secs1SettledAtSelected(edges []secs1StateEdge) bool {
	for _, want := range secs1BringUpEdges {
		if slices.Contains(edges, want) {
			return true
		}
	}

	return false
}

// TestSECS1_BringUpReachesSelectedWithExpectedCause proves normal active and passive bring-up reach Selected via one of the two legal bring-up shapes,
// and that the transition into Selected always carries CauseSelectAccepted, whichever shape produced it —
// never CauseLocalOpen, the cause of the intermediate NotConnected -> NotSelected step alone —
// see step's coalesced-cause rule (hsms/supervisor.go),
// and integration/fsm_matrix_test.go's TestFSM_SECS1Matrix, which pins the same invariant end to end.
func TestSECS1_BringUpReachesSelectedWithExpectedCause(t *testing.T) {
	t.Parallel()

	port := freePort(t)
	ctx := t.Context()

	passive := newSECS1Passive(t, port)
	active := newSECS1Active(t, port)
	defer closeSECS1Endpoint(t, active)
	defer closeSECS1Endpoint(t, passive)

	var mu sync.Mutex

	var passiveEdges, activeEdges []secs1StateEdge
	var passiveSelectedCauses, activeSelectedCauses []hsms.TransitionCause

	record := func(edges *[]secs1StateEdge, causes *[]hsms.TransitionCause) func(hsms.LifecycleEvent) {
		return func(ev hsms.LifecycleEvent) {
			mu.Lock()
			defer mu.Unlock()

			*edges = append(*edges, secs1StateEdge{prev: ev.Previous, next: ev.Current})
			if ev.Current == hsms.SelectedState {
				*causes = append(*causes, ev.Cause)
			}
		}
	}

	cancelPassive := passive.conn.SubscribeLifecycle(record(&passiveEdges, &passiveSelectedCauses))
	defer cancelPassive()

	cancelActive := active.conn.SubscribeLifecycle(record(&activeEdges, &activeSelectedCauses))
	defer cancelActive()

	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	waitSECS1Selected(t, passive)
	waitSECS1Selected(t, active)

	// State() flips synchronously with the commit,
	// but the lifecycle notification is delivered asynchronously by the notifier goroutine —
	// wait for the settling edge to actually arrive (a bounded poll, the one case 300-testing.md allows it for)
	// before reading the recorded edges/causes.
	snapshotEdges := func(edges *[]secs1StateEdge) []secs1StateEdge {
		var snap []secs1StateEdge

		require.Eventually(t, func() bool {
			mu.Lock()
			defer mu.Unlock()

			if !secs1SettledAtSelected(*edges) {
				return false
			}

			snap = append([]secs1StateEdge(nil), (*edges)...)

			return true
		}, 5*time.Second, time.Millisecond, "the entering-Selected notification must be delivered")

		return snap
	}

	legalBringUp := func(t *testing.T, edges []secs1StateEdge, causes []hsms.TransitionCause, role string) {
		t.Helper()
		require.True(t, secs1SettledAtSelected(edges), "%s must reach Selected via a legal bring-up shape", role)
		require.Len(t, causes, 1, "%s must report exactly one entering-Selected transition", role)
		require.Equal(t, hsms.CauseSelectAccepted, causes[0],
			"%s's entering-Selected transition must always report CauseSelectAccepted, whichever bring-up shape produced it", role)
	}

	pEdges := snapshotEdges(&passiveEdges)
	aEdges := snapshotEdges(&activeEdges)

	mu.Lock()
	pCauses := append([]hsms.TransitionCause(nil), passiveSelectedCauses...)
	aCauses := append([]hsms.TransitionCause(nil), activeSelectedCauses...)
	mu.Unlock()

	legalBringUp(t, pEdges, pCauses, "passive")
	legalBringUp(t, aEdges, aCauses, "active")
}

// TestSECS1_LateTCPUpAfterCloseIsRefused proves the gen-0 TCP-up commit-refusal behavior:
// a plain TCPUp report reaching the core after a completed Close is refused rather than resurrecting a torn-down link.
// secs1 always reports TCP-up unnamed (gen 0),
// so this exercises exactly the call shape startActive/acceptLoop use —
// reached here through the public hsms.TransportRuntime interface every in-module and out-of-module transport shares.
//
// This drives the report after a FULLY completed Close (teardown joined AND this transport's own Stop already sealed),
// not the narrower window the same refusal also covers —
// a report landing after the core has latched the generation ended
// but before this transport's own Stop has run its local t.stopping check
// (see acceptLoop's own comment on that window in secs1/transport.go).
// secs1 has no test seam that pauses inside that window,
// and the window itself is far too brief — it closes on the teardown goroutine's own path to stopTransport,
// not on any wait a test can observe — to hit deterministically over this package's test surface,
// so this test proves the refusal any later, straggler report finds instead.
func TestSECS1_LateTCPUpAfterCloseIsRefused(t *testing.T) {
	t.Parallel()

	port := freePort(t)
	ctx := t.Context()

	passive := newSECS1Passive(t, port)
	active := newSECS1Active(t, port)
	defer closeSECS1Endpoint(t, passive)

	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	waitSECS1Selected(t, passive)
	waitSECS1Selected(t, active)

	require.NoError(t, active.conn.Close())
	require.Equal(t, hsms.NotConnectedState, active.conn.State())

	// active.conn is secs1's own *connection wrapper (package-local, so accessible here),
	// which embeds hsms.Connection BY INTERFACE FIELD —
	// its promoted method set is exactly that interface's,
	// so TransportRuntime is reached through the embedded field's own dynamic value, never through the wrapper itself.
	sc, ok := active.conn.(*connection)
	require.True(t, ok, "New must return secs1's own *connection wrapper")

	rt, ok := sc.Connection.(hsms.TransportRuntime)
	require.True(t, ok, "the embedded core engine must implement TransportRuntime")

	rt.TCPUp(benchAckConn{}) // a late, unnamed report reaching the core after a completed Close

	// The refusal is synchronous inside the gate — TCPUp has already returned by the time this runs,
	// so no bounded wait is needed to give it a chance to (incorrectly) resurrect the link.
	require.Equal(t, hsms.NotConnectedState, active.conn.State(),
		"a TCP-up report after Close must never move the FSM off NotConnected")
}
