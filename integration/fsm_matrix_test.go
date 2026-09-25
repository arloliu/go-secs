package integration

import (
	"context"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/hsmsss"
	"github.com/stretchr/testify/require"
)

// hasStateEdge reports whether the recorded transition sequence contains the (prev,next) edge. The
// connection-state matrix asserts a driven edge is PRESENT (rather than pinning the full sequence)
// because a scenario that keeps the lifecycle running — a reconnect churn, say — accumulates further
// edges after the one under test.
func hasStateEdge(edges []stateEdge, prev, next hsms.ConnState) bool {
	for _, e := range edges {
		if e.prev == prev && e.next == next {
			return true
		}
	}

	return false
}

// bringUpSelectedEdges enumerates the two edges the supervisor can legally report for a successful open reaching Selected.
// Entering Selected is never a "drop" (step's dropped exception applies only to next == NotConnectedState),
// so the fire here is always the ordinary dedup, next != lastReacted,
// and prev is always whatever lastReacted last held:
// NotSelectedState (the ordinary two-step climb) or NotConnectedState (the collapsed shape below).
// Two events can drive that fire: evSelectAccepted,
// whose transition() entry returns SelectedState from either NotSelectedState or,
// tolerating CommitSelected's own H2 pre-commit,
// SelectedState itself;
// and evTCPUp,
// whose own entry now also tolerates a Select commit's CAS
// that raced ahead of it (transition(SelectedState, evTCPUp) is a same-state no-op).
// In the ordinary two-step climb evSelectAccepted is what fires the entering-Selected edge.
// In the race below it is evTCPUp that fires it,
// and the evSelectAccepted that follows then finds next already equal to lastReacted and fires nothing.
//
// A same-generation scheduler race can legally collapse the two-step climb into a single
// NotConnected -> Selected edge, with the NotSelected -> Selected edge never firing at all
// (not merely dropped from notify's delivery buffer — lastReacted itself never passes through
// NotSelectedState):
// CommitConnected's CAS+injectFrom (evTCPUp) and CommitSelected's CAS+injectFrom (evSelectAccepted)
// run synchronously on the CALLERS' own goroutines
// (the connect-procedure goroutine and the Select-procedure/recv-loop goroutine respectively),
// fully decoupled from when run() actually DEQUEUES and processes either event.
// evTCPUp being enqueued first only guarantees it is DEQUEUED first —
// it says nothing about what cur := s.state.Load() reads at that moment.
// If run() is scheduler-starved long enough for BOTH synchronous CASes to complete
// (state already Selected) before run() drains ANY of the backlog,
// step() reads cur == SelectedState for the queued evTCPUp:
// its table entry tolerates that, so it fires the coalesced bring-up directly —
// (NotConnectedState, SelectedState), with the reported cause substituted to CauseSelectAccepted
// (step's coalesced-cause rule) — and sets lastReacted to SelectedState.
// TestSupervisor_PreCommittedSelectFiresReactionExactlyOnce (hsms/supervisor_test.go) explicitly WAITS for evTCPUp to finish processing before calling CommitSelected,
// which is why that test does not exercise this window;
// TestSupervisor_TCPUpCoalescesPrecommittedSelectThenDisconnectFiresOnce (same file) is the dedicated unit-level test for it.
//
// A cross-generation variant of this race is not reachable:
// any real drop into NotConnectedState always fires its own reaction and notification (step's dropped exception),
// so lastReacted can never be stranded at NotConnectedState behind an unreported intermediate drop the way a generation-mismatch discard of some OTHER generation's stale evTCPUp would require.
var bringUpSelectedEdges = []stateEdge{
	{prev: hsms.NotSelectedState, next: hsms.SelectedState},
	{prev: hsms.NotConnectedState, next: hsms.SelectedState},
}

// settledAtSelected reports whether edges records either legal bring-up shape settling at Selected (bringUpSelectedEdges):
// the ordinary two-step climb or the collapsed single edge (the scheduler race bringUpSelectedEdges documents).
// It is shape-agnostic ON PURPOSE — see bringUpSelectedEdges —
// while still admitting exactly the enumerated legal set, not "any state change":
// a sequence that never reaches Selected at all still reports false.
func settledAtSelected(edges []stateEdge) bool {
	for _, want := range bringUpSelectedEdges {
		if hasStateEdge(edges, want.prev, want.next) {
			return true
		}
	}

	return false
}

// bringUpEdges are the ONLY edges a clean connect (Select.rsp granted, no forced disconnect) may ever record:
// the intermediate NotConnected -> NotSelected step
// (recorded whenever evTCPUp is processed before the race window bringUpSelectedEdges documents closes)
// plus the two settling shapes bringUpSelectedEdges enumerates.
// Anything else recorded during a connect — most notably a Selected -> NotSelected or
// Selected -> NotConnected edge appearing mid bring-up — is a genuine defect, not a legal shape.
var bringUpEdges = []stateEdge{
	{prev: hsms.NotConnectedState, next: hsms.NotSelectedState},
	{prev: hsms.NotSelectedState, next: hsms.SelectedState},
	{prev: hsms.NotConnectedState, next: hsms.SelectedState},
}

// isLegalBringUpEdge reports whether e is a member of bringUpEdges.
func isLegalBringUpEdge(e stateEdge) bool {
	return slices.Contains(bringUpEdges, e)
}

// bringUpSettledCleanly reports whether edges is a valid, complete connect bring-up:
// it settles at Selected (settledAtSelected)
// AND every recorded edge is a member of the legal bring-up set (bringUpEdges) —
// the shape-agnostic replacement for an exact two-step require.Equal pin.
// It still fails on a genuinely wrong outcome:
// never reaching Selected, or any edge outside the legal set (for example a Selected -> NotSelected mid bring-up).
func bringUpSettledCleanly(edges []stateEdge) bool {
	if !settledAtSelected(edges) {
		return false
	}

	for _, e := range edges {
		if !isLegalBringUpEdge(e) {
			return false
		}
	}

	return true
}

// TestSettledAtSelected is the teeth check for settledAtSelected against hand-built edge sequences, standing in for the live cross-generation race (bringUpSelectedEdges)
// that is too rare to force deterministically over the net.Pipe harness.
// It proves three things the staleT7NoOpFromSelected fence change depends on:
// the OLD fence (a bare hasStateEdge(NotSelected, Selected)) fails on the legal collapsed shape,
// the NEW fence (settledAtSelected) passes on BOTH legal shapes,
// and the NEW fence still correctly fails when Selected is never reached at all.
func TestSettledAtSelected(t *testing.T) {
	twoStep := []stateEdge{
		{prev: hsms.NotConnectedState, next: hsms.NotSelectedState},
		{prev: hsms.NotSelectedState, next: hsms.SelectedState},
	}
	collapsed := []stateEdge{
		{prev: hsms.NotConnectedState, next: hsms.SelectedState},
	}
	neverSelected := []stateEdge{
		{prev: hsms.NotConnectedState, next: hsms.NotSelectedState},
		{prev: hsms.NotSelectedState, next: hsms.NotConnectedState},
	}

	// The OLD fence (hasStateEdge(NotSelected, Selected) alone) admits the ordinary two-step shape ...
	require.True(t, hasStateEdge(twoStep, hsms.NotSelectedState, hsms.SelectedState),
		"the old fence must accept the ordinary two-step bring-up")
	// ... but fails on the legal collapsed shape — this is the flake staleT7NoOpFromSelected hit.
	require.False(t, hasStateEdge(collapsed, hsms.NotSelectedState, hsms.SelectedState),
		"the old fence must (incorrectly) reject the legal collapsed bring-up")

	// The NEW fence accepts both legal shapes ...
	require.True(t, settledAtSelected(twoStep), "settledAtSelected must accept the ordinary two-step bring-up")
	require.True(t, settledAtSelected(collapsed), "settledAtSelected must accept the collapsed bring-up")
	// ... and still correctly rejects a sequence that never reaches Selected — the genuinely wrong outcome the fence must keep failing on.
	require.False(t, settledAtSelected(neverSelected), "settledAtSelected must reject a sequence that never reaches Selected")
}

// TestBringUpSettledCleanly is the teeth check for bringUpSettledCleanly,
// the replacement for connect's OLD exact-two-step require.Equal pin.
// It proves the pin still does its job after being made shape-agnostic:
// the collapsed shape now PASSES (the old exact-sequence pin would have failed it),
// the ordinary two-step shape still passes,
// and a genuinely illegal sequence — a Selected -> NotSelected edge appearing MID bring-up,
// which no clean connect can ever produce — still FAILS.
func TestBringUpSettledCleanly(t *testing.T) {
	twoStep := []stateEdge{
		{prev: hsms.NotConnectedState, next: hsms.NotSelectedState},
		{prev: hsms.NotSelectedState, next: hsms.SelectedState},
	}
	collapsed := []stateEdge{
		{prev: hsms.NotConnectedState, next: hsms.SelectedState},
	}
	// A stray Selected -> NotSelected edge mid bring-up:
	// not producible by a clean connect (it is the Select-lost edge, the subject of the separate selectLost subtest),
	// so bringUpSettledCleanly must reject it even though the sequence does eventually settle at Selected.
	illegalMidBringUp := []stateEdge{
		{prev: hsms.NotConnectedState, next: hsms.NotSelectedState},
		{prev: hsms.NotSelectedState, next: hsms.SelectedState},
		{prev: hsms.SelectedState, next: hsms.NotSelectedState},
		{prev: hsms.NotSelectedState, next: hsms.SelectedState},
	}
	neverSelected := []stateEdge{
		{prev: hsms.NotConnectedState, next: hsms.NotSelectedState},
	}

	require.True(t, bringUpSettledCleanly(twoStep), "the ordinary two-step bring-up must pass")
	require.True(t, bringUpSettledCleanly(collapsed),
		"the collapsed bring-up must pass — this is what the OLD exact-two-step pin would have failed")
	require.False(t, bringUpSettledCleanly(illegalMidBringUp),
		"a Selected -> NotSelected edge mid bring-up must still fail, even though the sequence eventually settles at Selected")
	require.False(t, bringUpSettledCleanly(neverSelected), "a sequence that never reaches Selected must still fail")
}

// newHSMSSSMatrixConn constructs an HSMS-SS connection over the net.Pipe harness for the
// connection-state matrix. It always applies a short T5 (fast reconnect cadence) plus any extra
// connection options, spawns peers via spawn, and registers a fresh state recorder BEFORE Open so the
// connect transitions are captured. It does NOT Open — the caller chooses OpenWaitSelected or
// OpenBackground, so a scenario that must dwell in NotSelected (with the peer withholding Select.rsp)
// can start the lifecycle in the background instead of blocking on a Selected that never arrives.
func newHSMSSSMatrixConn(t *testing.T, spawn func(net.Conn) scriptedPeer, opts ...hsms.ConnOption) (hsms.Connection, *dialFactory, *stateRecorder) {
	t.Helper()

	df := &dialFactory{spawn: spawn}
	t.Cleanup(df.closeAll)

	cfgOpts := make([]hsmsss.Option, 0, 3+len(opts))
	cfgOpts = append(cfgOpts,
		hsmsss.WithActive(),
		hsmsss.WithDialer(df.dial),
		hsmsss.WithConnectionOption(hsms.WithT5(50*time.Millisecond)),
	)
	for _, opt := range opts {
		cfgOpts = append(cfgOpts, hsmsss.WithConnectionOption(opt))
	}

	cfg, err := hsmsss.NewConfig("127.0.0.1", 5000, cfgOpts...)
	require.NoError(t, err)

	conn, err := hsmsss.New(cfg)
	require.NoError(t, err)

	rec, handler := newStateRecorder()
	conn.AddConnStateChangeHandler(handler)

	return conn, df, rec
}

// autoAnswerSpawn spawns the default full-duplex HSMS peer: it auto-answers Select.req / Linktest.req
// and auto-echoes W-bit data, so the connection reaches Selected normally.
func autoAnswerSpawn(c net.Conn) scriptedPeer { return newHSMSPeer(c) }

// withholdSelectSpawn spawns an HSMS peer with the Select.rsp withheld from birth. Because the reconnect
// loop dials a fresh peer per generation, withholding at spawn keeps EVERY generation dwelling in
// NotSelected until its T7 (NOT-SELECTED) timer expires.
func withholdSelectSpawn(c net.Conn) scriptedPeer {
	p := newHSMSPeer(c)
	p.setWithholdSelect(true)

	return p
}

// latestHSMSPeer returns the current-generation peer as a concrete *hsmsPeer so a scenario can reach the
// HSMS-only scripting (a peer-originated Deselect.req) that the shared scriptablePeer surface does not
// expose. SECS-I has no Select layer, so those controls are deliberately absent from the shared surface.
func latestHSMSPeer(t *testing.T, df *dialFactory) *hsmsPeer {
	t.Helper()

	p, ok := df.latest().(*hsmsPeer)
	require.True(t, ok, "expected the current-generation peer to be an *hsmsPeer")

	return p
}

// TestFSM_HSMSSSMatrix drives the HSMS-SS connection through the SEMI E37 connection-state transition
// matrix over the net.Pipe harness and asserts each observed (prev,next) edge end to end: the connect
// climb, the T7 (NOT-SELECTED) dwell expiry, the Select-lost transition, the stale-T7-from-Selected
// no-op guard, and the close teardown.
func TestFSM_HSMSSSMatrix(t *testing.T) {
	// connect: a clean Open steps NotConnected -> NotSelected -> Selected as the E37 Select handshake
	// completes.
	t.Run("connect", func(t *testing.T) {
		conn, _, rec := newHSMSSSMatrixConn(t, autoAnswerSpawn)
		defer func() { require.NoError(t, conn.Close()) }()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		require.NoError(t, conn.Open(ctx, hsms.OpenWaitSelected))
		require.Equal(t, hsms.SelectedState, conn.State())

		// The state notifier records asynchronously, so wait (bounded) until the bring-up settles at Selected.
		// settledAtSelected, not a hard requirement of both specific edges, because the scheduler race / cross-generation race bringUpSelectedEdges documents can legally collapse the two-step climb into a single NotConnected -> Selected edge.
		require.Eventually(t, func() bool {
			return settledAtSelected(rec.pairs())
		}, 15*time.Second, 5*time.Millisecond, "connect must settle at Selected")

		// Pin what the scenario actually tests —
		// a clean bring-up settles at Selected with no illegal edge ever recorded — rather than demanding the two-step sequence specifically:
		// bringUpEdges enumerates the full legal set (the intermediate step plus both settling shapes),
		// so this still fails on a genuinely wrong outcome (for example a Selected -> NotSelected edge mid bring-up).
		require.True(t, bringUpSettledCleanly(rec.pairs()),
			"connect must settle at Selected via a legal bring-up shape with no illegal edge recorded: %+v", rec.pairs())
	})

	// t7DwellDisconnect: with the Select.rsp withheld the connection reaches NotSelected but never
	// Selected; the short T7 (NOT-SELECTED) dwell then expires, tearing the line down NotSelected ->
	// NotConnected. OpenBackground is required here — OpenWaitSelected would block until ctx expiry
	// because Selected is never reached.
	t.Run("t7DwellDisconnect", func(t *testing.T) {
		conn, _, rec := newHSMSSSMatrixConn(t, withholdSelectSpawn, hsms.WithT7(50*time.Millisecond))
		defer func() { require.NoError(t, conn.Close()) }()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		// Mark BEFORE Open: the notifier records on its own goroutine, so both transitions can land
		// while this goroutine is still descheduled.
		marked := rec.mark()

		require.NoError(t, conn.Open(ctx, hsms.OpenBackground))

		// The line comes up to NotSelected ...
		up, ok := rec.awaitStateFrom(marked, hsms.NotSelectedState, 15*time.Second)
		require.True(t, ok, "the line must come up to NotSelected with the Select.rsp withheld")
		// ... then the T7 dwell expires and disconnects it.
		_, ok = rec.awaitStateFrom(up+1, hsms.NotConnectedState, 15*time.Second)
		require.True(t, ok, "the T7 (NOT-SELECTED) dwell must expire and disconnect")

		p := rec.pairs()
		require.True(t, hasStateEdge(p, hsms.NotConnectedState, hsms.NotSelectedState),
			"connect must reach NotSelected")
		require.True(t, hasStateEdge(p, hsms.NotSelectedState, hsms.NotConnectedState),
			"the T7 dwell expiry must drive NotSelected -> NotConnected")

		// The Select.rsp was withheld on every generation, so the session never reached Selected.
		for _, e := range p {
			require.NotEqual(t, hsms.SelectedState, e.prev, "a session with the Select.rsp withheld must never reach Selected")
			require.NotEqual(t, hsms.SelectedState, e.next, "a session with the Select.rsp withheld must never reach Selected")
		}
	})

	// selectLost: a peer-originated Deselect.req (SEMI E37 §7.7) received while Selected drives the
	// core's Deselect responder to reply Deselect.rsp(success) and transition Selected -> NotSelected —
	// the Select-lost edge, driven end to end over the harness. A peer Separate.req, by contrast, is a
	// full teardown to NotConnected, so the Deselect.req is what isolates this specific edge.
	t.Run("selectLost", func(t *testing.T) {
		conn, df, rec := newHSMSSSMatrixConn(t, autoAnswerSpawn)
		defer func() { require.NoError(t, conn.Close()) }()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		require.NoError(t, conn.Open(ctx, hsms.OpenWaitSelected))
		require.Equal(t, hsms.SelectedState, conn.State())

		// Wait until the bring-up has settled at Selected (settledAtSelected — see bringUpSelectedEdges;
		// the scheduler race it documents can legally collapse the two-step climb into a single NotConnected -> Selected edge).
		// Either shape leaves the supervisor's own lastReacted at SelectedState once this returns true
		// (fireTransition fires whenever next != lastReacted, or unconditionally for a real drop into
		// NotConnectedState — a settling edge's next is always SelectedState, so here it is the ordinary dedup alone),
		// so the only way a LATER edge with next == NotSelectedState can appear is evSelectLost's tolerant table entry (Selected -> NotSelected) —
		// never a still-in-flight connect NotConnected -> NotSelected, which cannot fire again once lastReacted has already passed it.
		require.Eventually(t, func() bool {
			return settledAtSelected(rec.pairs())
		}, 15*time.Second, 5*time.Millisecond, "connect must settle at Selected before the Deselect")

		require.NoError(t, latestHSMSPeer(t, df).sendDeselect(0xFFFF))

		// Poll the recorded history (not awaitState): the peer's Deselect.req is consumed synchronously
		// over the in-memory pipe, so the Selected -> NotSelected edge can land before a fresh-edge wait
		// snapshots its baseline. Scanning the immutable history for the specific edge has no such race.
		// A Selected -> NotSelected edge can ONLY come from the Select-lost transition (a teardown would
		// be Selected -> NotConnected), so this pins the Select-lost edge exactly.
		require.Eventually(t, func() bool {
			return hasStateEdge(rec.pairs(), hsms.SelectedState, hsms.NotSelectedState)
		}, 15*time.Second, 5*time.Millisecond,
			"a peer Deselect.req while Selected must drive Selected -> NotSelected, not a teardown to NotConnected")
	})

	// staleT7NoOpFromSelected: once Selected, a stale T7 (NOT-SELECTED) timer must never tear the
	// session down — T7 expiry is a no-op from Selected. With a short T7, reach Selected and dwell well
	// past T7; no further transition may be recorded and the session must stay Selected. This is the
	// load-bearing guard that a session which reached Selected is never torn down by a stale T7.
	t.Run("staleT7NoOpFromSelected", func(t *testing.T) {
		conn, _, rec := newHSMSSSMatrixConn(t, autoAnswerSpawn, hsms.WithT7(50*time.Millisecond))
		defer func() { require.NoError(t, conn.Close()) }()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		require.NoError(t, conn.Open(ctx, hsms.OpenWaitSelected))
		require.Equal(t, hsms.SelectedState, conn.State())

		// settledAtSelected (not a bare hasStateEdge(NotSelected, Selected))
		// because the scheduler race bringUpSelectedEdges documents can legally collapse the two-step climb into a single NotConnected -> Selected edge,
		// with the NotSelected -> Selected edge never firing at all —
		// independent of the short T7 this scenario configures.
		// A fence pinned to the two-step shape alone burns its full timeout and fails on that legal outcome —
		// the flake this fence replaces.
		require.Eventually(t, func() bool {
			return settledAtSelected(rec.pairs())
		}, 15*time.Second, 5*time.Millisecond, "connect must settle at Selected")

		settled := rec.pairs()

		// Inject a dwell far longer than T7 (50ms) into the scenario. A stale T7 firing from Selected
		// would tear the session down here; the no-op guard means nothing happens.
		time.Sleep(250 * time.Millisecond)

		require.Equal(t, settled, rec.pairs(), "a stale T7 must not add any transition once Selected")
		require.Equal(t, hsms.SelectedState, conn.State(), "the session must remain Selected after the T7 window")
	})

	// close: Close from Selected drives the terminal Selected -> NotConnected edge. Close joins the
	// state notifier before returning, so the terminal edge is settled once Close returns.
	t.Run("close", func(t *testing.T) {
		conn, _, rec := newHSMSSSMatrixConn(t, autoAnswerSpawn)
		defer func() { _ = conn.Close() }() // idempotent safety close on an early-failure path

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		require.NoError(t, conn.Open(ctx, hsms.OpenWaitSelected))
		require.Equal(t, hsms.SelectedState, conn.State())

		require.NoError(t, conn.Close())

		p := rec.pairs()
		require.True(t, hasStateEdge(p, hsms.SelectedState, hsms.NotConnectedState),
			"Close from Selected must drive Selected -> NotConnected")
		require.Equal(t, stateEdge{prev: hsms.SelectedState, next: hsms.NotConnectedState}, p[len(p)-1],
			"the terminal edge must be Selected -> NotConnected")
		require.Equal(t, hsms.NotConnectedState, conn.State())
	})
}

// TestFSM_SECS1Matrix asserts the SPARSE SECS-I connection-state matrix and the absence of any Select handshake.
// SECS-I (SEMI E4) has no HSMS-style Select handshake —
// a live line IS the selected session —
// so the transport commits straight through to Selected via the same two back-to-back synchronous commits
// (TCPUp then CommitSelected — secs1/transport.go) HSMS-SS uses for its own bring-up.
// That means the SAME scheduler race bringUpSelectedEdges documents applies here too:
// bring-up may settle at Selected via either legal shape,
// the collapsed NotConnected -> Selected edge or the ordinary NotConnected -> NotSelected -> Selected climb.
// The Select-accepted, Select-lost, and T7 rows of the HSMS matrix are otherwise absent by design.
// The invariant is asserted end to end across a full lifecycle: open, involuntary line drop, reconnect, and close.
// The Select-lost edge in isolation is exercised by the package-level supervisor unit tests,
// the authority for the exhaustive abstract table;
// here the end-to-end teeth are that SECS-I never emits a Selected -> NotSelected edge —
// the one shape only a real Select-lost handshake, which SECS-I has none of, can legitimately produce.
func TestFSM_SECS1Matrix(t *testing.T) {
	f := secs1Factory()

	rec, handler := newStateRecorder()

	// Every entry into Selected must carry CauseSelectAccepted, whichever bring-up shape produced it.
	var causeMu sync.Mutex
	var selectedCauses []hsms.TransitionCause
	conn, df := f.openWith(t, func(c hsms.Connection) {
		c.AddConnStateChangeHandler(handler)
		c.SubscribeLifecycle(func(ev hsms.LifecycleEvent) {
			if ev.Current != hsms.SelectedState {
				return
			}
			causeMu.Lock()
			selectedCauses = append(selectedCauses, ev.Cause)
			causeMu.Unlock()
		})
	})
	defer func() { _ = conn.Close() }() // idempotent safety close on an early-failure path

	require.Equal(t, hsms.SelectedState, conn.State())

	// A live line reaches Selected via either legal bring-up shape (settledAtSelected — see bringUpSelectedEdges);
	// the pre-existing flake this replaces was a hard-coded collapsed-only assertion racing the same scheduler window HSMS-SS's own bring-up does.
	require.Eventually(t, func() bool {
		return settledAtSelected(rec.pairs())
	}, 15*time.Second, 5*time.Millisecond, "a live SECS-I line must reach Selected via a legal bring-up shape")

	// Mark BEFORE the drop: a SECS-I teardown lands within one poll tick (10ms), so a mark taken
	// after dropLine can already sit past the teardown edge and the wait would then miss it.
	marked := rec.mark()

	// Drop the live line involuntarily: the connection tears down (Selected -> NotConnected) and the
	// reconnect loop redials a fresh generation that commits back to Selected.
	dropped := latestScriptable(t, df)
	dropped.dropLine()

	torn, ok := rec.awaitStateFrom(marked, hsms.NotConnectedState, 3*time.Second)
	require.True(t, ok, "an involuntary line drop must tear the SECS-I line down to NotConnected")
	// Resume past the teardown match so the pre-drop Selected cannot satisfy this wait.
	_, ok = rec.awaitStateFrom(torn+1, hsms.SelectedState, 3*time.Second)
	require.True(t, ok, "the reconnect must bring the fresh SECS-I line back to Selected")

	// A clean Close settles the terminal Selected -> NotConnected edge and joins the notifier,
	// so the whole lifecycle sequence is final once Close returns.
	require.NoError(t, conn.Close())

	p := rec.pairs()

	// The sparse sequence: bring-up (either legal shape) and teardown (Selected -> NotConnected)
	// edges are all that ever appear.
	require.True(t, settledAtSelected(p), "SECS-I must reach Selected via a legal bring-up shape")
	require.True(t, hasStateEdge(p, hsms.SelectedState, hsms.NotConnectedState),
		"SECS-I must tear down Selected -> NotConnected")

	// The teeth: across the ENTIRE lifecycle — open, drop, reconnect, close — SECS-I never emits a
	// Selected -> NotSelected edge, because it has no Select-lost handshake to produce one.
	// NotConnected -> NotSelected and NotSelected -> Selected MAY both appear (the uncollapsed bring-up shape above),
	// so this checks the one illegal DIRECTION, not NotSelected's mere presence.
	for _, e := range p {
		require.False(t, e.prev == hsms.SelectedState && e.next == hsms.NotSelectedState,
			"SECS-I must never emit a Selected -> NotSelected edge: it has no Select-lost handshake")
	}

	// Close joined the notifier, so every lifecycle event has been delivered by now.
	causeMu.Lock()
	defer causeMu.Unlock()
	require.NotEmpty(t, selectedCauses, "the bring-up and the reconnect must both report entering Selected")
	for _, c := range selectedCauses {
		require.Equal(t, hsms.CauseSelectAccepted, c, "every entry into Selected must report CauseSelectAccepted")
	}
}
