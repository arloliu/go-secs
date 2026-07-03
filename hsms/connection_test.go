package hsms

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// newTestConn builds a connection engine backed by a fresh mockTransport for the skeleton
// unit tests. It returns both the Connection interface and the concrete *connection so
// tests can inspect unexported state (white-box package hsms tests).
func newTestConn(t *testing.T) (Connection, *connection) {
	t.Helper()

	conn, err := NewConnection(DefaultConnectionConfig(), &mockTransport{})
	require.NoError(t, err)
	require.NotNil(t, conn)

	c, ok := conn.(*connection)
	require.True(t, ok, "NewConnection must return the concrete *connection")

	return conn, c
}

// TestNewConnection_ReturnsConnection verifies NewConnection returns a non-nil value that
// satisfies the Connection interface (and the concrete engine also satisfies
// TransportRuntime, wired via the session back-channel).
func TestNewConnection_ReturnsConnection(t *testing.T) {
	conn, c := newTestConn(t)

	require.NotNil(t, conn)

	// The concrete engine plays both roles (compile-time asserted in connection.go; this
	// re-checks at runtime for documentation value).
	var _ Connection = c
	var _ TransportRuntime = c

	// The session back-channel is the connection itself, and its persistent handler slice
	// is wired to the connection's field (spec §5.1 / §5.4).
	require.NotNil(t, c.session)
	require.Same(t, &c.handlers, c.connHandlers)
}

// TestNewConnection_NilConfigError verifies the cfg-non-nil validation.
func TestNewConnection_NilConfigError(t *testing.T) {
	conn, err := NewConnection(nil, &mockTransport{})
	require.Error(t, err)
	require.Nil(t, conn)
}

// TestConnection_StateBeforeOpenIsNotConnected verifies the round-7 nil-supervisor guard:
// State() reports NotConnectedState before the first Open (sup is nil) and never nil-derefs.
func TestConnection_StateBeforeOpenIsNotConnected(t *testing.T) {
	conn, c := newTestConn(t)

	require.Nil(t, c.sup.Load(), "no supervisor before Open")
	require.Equal(t, NotConnectedState, conn.State())
}

// TestConnection_Metrics_NonNil verifies Metrics() returns the live (non-nil) metrics.
func TestConnection_Metrics_NonNil(t *testing.T) {
	conn, c := newTestConn(t)

	m := conn.Metrics()
	require.NotNil(t, m)
	require.Same(t, &c.metrics, m, "Metrics must return the live counters, not a copy")
}

// TestConnection_UpdateConfigOptions_Transactional verifies a valid+invalid option pair
// does not half-mutate the live config (T4 apply is transactional — validate-all,
// commit-atomically).
func TestConnection_UpdateConfigOptions_Transactional(t *testing.T) {
	conn, c := newTestConn(t)

	origT3 := c.cfg.Load().timers.T3

	// One valid (WithT3) + one invalid (WithT6(-1)) option: NOTHING must commit.
	err := conn.UpdateConfigOptions(WithT3(2*time.Second), WithT6(-1))
	require.Error(t, err)
	require.Equal(t, origT3, c.cfg.Load().timers.T3, "valid option must NOT commit when a sibling fails")

	// A fully valid batch commits atomically.
	require.NoError(t, conn.UpdateConfigOptions(WithT3(3*time.Second), WithSessionID(7)))
	require.Equal(t, 3*time.Second, c.cfg.Load().timers.T3)
	require.Equal(t, uint16(7), c.cfg.Load().sessionID)
}

// TestConnection_DoneBeforeOpenIsClosed verifies Done() returns an already-closed channel
// when there is no live epoch (SELECT-ONLY, never nil-blocks — J5).
func TestConnection_DoneBeforeOpenIsClosed(t *testing.T) {
	conn, c := newTestConn(t)

	require.Nil(t, c.cur.Load(), "no epoch before Open")

	// Down-cast to reach the TransportRuntime method (Done is not on Connection).
	rt, ok := conn.(TransportRuntime)
	require.True(t, ok)

	select {
	case <-rt.Done():
	default:
		t.Fatal("Done() must be a pre-closed channel before Open")
	}
}

// TestConnection_TrivialRuntimeReads verifies the trivial TransportRuntime reads
// (Timers/SessionID) return the configured values.
func TestConnection_TrivialRuntimeReads(t *testing.T) {
	conn, _ := newTestConn(t)

	rt, ok := conn.(TransportRuntime)
	require.True(t, ok)

	require.Equal(t, DefaultConnectionConfig().timers, rt.Timers())
	require.Equal(t, uint16(0xFFFF), rt.SessionID())
}

// TestConnection_BeforeOpenReturnsHonestErrors verifies the lifecycle/send methods return
// honest errors (never panic) BEFORE the first Open, and that the still-stubbed recv path
// (DeliverOwnedFrame, T19) returns an honest error.
func TestConnection_BeforeOpenReturnsHonestErrors(t *testing.T) {
	conn, c := newTestConn(t)

	// Before Open: Close is ErrNotOpen (never-opened); sends are ErrNotOpen (no live epoch).
	require.ErrorIs(t, conn.Close(), ErrNotOpen)

	_, err := c.WriteMessage(context.Background(), nil)
	require.ErrorIs(t, err, ErrNotOpen)
	require.ErrorIs(t, c.SendAsync(context.Background(), nil), ErrNotOpen)

	// DeliverOwnedFrame is still stubbed (recv/decode is a later task) — honest error.
	require.Error(t, c.DeliverOwnedFrame(nil))
	require.False(t, c.RouteReply(nil))

	// The transport-driven no-ops must not panic with no live supervisor/epoch.
	require.NotPanics(t, func() {
		c.TCPUp(nil)
		c.TCPDown(nil)
		c.SelectLost()
		c.T7Expired()
		require.False(t, c.CommitSelected())
	})
}

// TestConnection_MockTransportSeam exercises the transport seam mock (the Task-26 seed):
// Start records the runtime and runs the scripted hook, Stop marks stopped, and the inert
// byte-sink / deadline stubs do not error.
func TestConnection_MockTransportSeam(t *testing.T) {
	conn, c := newTestConn(t)

	var hookRan bool
	mt := &mockTransport{
		startFn: func(_ context.Context, _ TransportRuntime) error {
			hookRan = true

			return nil
		},
	}

	// Open is stubbed, so the connection does not call Start yet; drive the seam directly.
	require.NoError(t, mt.Start(context.Background(), c))
	require.True(t, hookRan, "startFn must run")
	require.True(t, mt.wasStarted())
	require.Same(t, c, mt.runtime())

	require.NoError(t, mt.Stop(context.Background()))
	require.True(t, mt.wasStopped())

	require.NoError(t, mt.Write(context.Background(), fakeConn{}, nil))
	require.NoError(t, mt.SetReadDeadline(fakeConn{}, time.Time{}))
	require.NoError(t, mt.SetWriteDeadline(fakeConn{}, time.Time{}))

	// conn is a valid TransportRuntime that the mock can be handed.
	require.NotNil(t, conn)
}
