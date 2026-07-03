package integration

import (
	"context"
	"net"
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

		// The state notifier records asynchronously, so wait (bounded) until both connect edges land.
		require.Eventually(t, func() bool {
			p := rec.pairs()
			return hasStateEdge(p, hsms.NotConnectedState, hsms.NotSelectedState) &&
				hasStateEdge(p, hsms.NotSelectedState, hsms.SelectedState)
		}, 2*time.Second, 5*time.Millisecond, "connect must step NotConnected -> NotSelected -> Selected")

		// The notifier delivers edges in order and the connection is quiescent at Selected, so the two
		// connect edges are exactly the recorded sequence.
		require.Equal(t, []stateEdge{
			{prev: hsms.NotConnectedState, next: hsms.NotSelectedState},
			{prev: hsms.NotSelectedState, next: hsms.SelectedState},
		}, rec.pairs())
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

		require.NoError(t, conn.Open(ctx, hsms.OpenBackground))

		// The line comes up to NotSelected ...
		require.True(t, rec.awaitState(hsms.NotSelectedState, 2*time.Second),
			"the line must come up to NotSelected with the Select.rsp withheld")
		// ... then the T7 dwell expires and disconnects it.
		require.True(t, rec.awaitState(hsms.NotConnectedState, 2*time.Second),
			"the T7 (NOT-SELECTED) dwell must expire and disconnect")

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

		// Wait until the notifier has recorded the connect edges through Selected, so the only forward
		// edge into NotSelected the wait below can observe is the Deselect-driven Select-lost — not a
		// still-in-flight connect NotConnected -> NotSelected.
		require.Eventually(t, func() bool {
			return hasStateEdge(rec.pairs(), hsms.NotSelectedState, hsms.SelectedState)
		}, 2*time.Second, 5*time.Millisecond, "connect must settle at Selected before the Deselect")

		require.NoError(t, latestHSMSPeer(t, df).sendDeselect(0xFFFF))

		// Poll the recorded history (not awaitState): the peer's Deselect.req is consumed synchronously
		// over the in-memory pipe, so the Selected -> NotSelected edge can land before a fresh-edge wait
		// snapshots its baseline. Scanning the immutable history for the specific edge has no such race.
		// A Selected -> NotSelected edge can ONLY come from the Select-lost transition (a teardown would
		// be Selected -> NotConnected), so this pins the Select-lost edge exactly.
		require.Eventually(t, func() bool {
			return hasStateEdge(rec.pairs(), hsms.SelectedState, hsms.NotSelectedState)
		}, 2*time.Second, 5*time.Millisecond,
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

		require.Eventually(t, func() bool {
			return hasStateEdge(rec.pairs(), hsms.NotSelectedState, hsms.SelectedState)
		}, 2*time.Second, 5*time.Millisecond, "connect must settle at Selected")

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

// TestFSM_SECS1Matrix asserts the SPARSE SECS-I connection-state matrix and the absence of any Select
// layer. SECS-I (SEMI E4) has no HSMS-style Select handshake — a live line IS the selected session — so
// the transport auto-commits NotConnected -> Selected and can only ever emit Selected -> NotConnected on
// teardown. The Select-accepted, Select-lost, and T7 rows of the HSMS matrix are absent by design, and
// no NotSelected state is ever entered. The invariant is asserted end to end across a full lifecycle:
// open, involuntary line drop, reconnect, and close. The Select-lost edge in isolation is exercised by
// the package-level supervisor unit tests, the authority for the exhaustive abstract table; here the
// end-to-end teeth are that SECS-I never produces a NotSelected edge at all.
func TestFSM_SECS1Matrix(t *testing.T) {
	f := secs1Factory()

	rec, handler := newStateRecorder()
	conn, df := f.openWith(t, func(c hsms.Connection) {
		c.AddConnStateChangeHandler(handler)
	})
	defer func() { _ = conn.Close() }() // idempotent safety close on an early-failure path

	require.Equal(t, hsms.SelectedState, conn.State())

	// A live line auto-commits straight to Selected — the auto-commit edge.
	require.Eventually(t, func() bool {
		return hasStateEdge(rec.pairs(), hsms.NotConnectedState, hsms.SelectedState)
	}, 2*time.Second, 5*time.Millisecond, "a live SECS-I line must auto-commit NotConnected -> Selected")

	// Drop the live line involuntarily: the connection tears down (Selected -> NotConnected) and the
	// reconnect loop redials a fresh generation that auto-commits back to Selected.
	dropped := latestScriptable(t, df)
	dropped.dropLine()

	require.True(t, rec.awaitState(hsms.NotConnectedState, 3*time.Second),
		"an involuntary line drop must tear the SECS-I line down to NotConnected")
	require.True(t, rec.awaitState(hsms.SelectedState, 3*time.Second),
		"the reconnect must auto-commit the fresh SECS-I line back to Selected")

	// Close settles the terminal Selected -> NotConnected edge and joins the notifier, so the whole
	// lifecycle sequence is final once Close returns.
	require.NoError(t, conn.Close())

	p := rec.pairs()

	// The sparse sequence: only auto-commit (NotConnected -> Selected) and teardown (Selected ->
	// NotConnected) edges ever appear.
	require.True(t, hasStateEdge(p, hsms.NotConnectedState, hsms.SelectedState),
		"SECS-I must auto-commit NotConnected -> Selected")
	require.True(t, hasStateEdge(p, hsms.SelectedState, hsms.NotConnectedState),
		"SECS-I must tear down Selected -> NotConnected")

	// The teeth: across the ENTIRE lifecycle — open, drop, reconnect, close — SECS-I never passes
	// through NotSelected, because it has no Select layer. No edge touches NotSelected on either side.
	for _, e := range p {
		require.NotEqual(t, hsms.NotSelectedState, e.prev, "SECS-I must never emit a NotSelected edge")
		require.NotEqual(t, hsms.NotSelectedState, e.next, "SECS-I must never emit a NotSelected edge")
	}
}
