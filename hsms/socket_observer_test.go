package hsms

// socket_observer_test.go — white-box tests for the core half of the socket record:
// the adoption capability that stores a socket identity on the epoch,
// the epoch's socket close routed through the transport's optional CloseSocket capability,
// and the failure the epoch records for that close
// (the write error that initiated the teardown, and ErrT7Timeout only when the T7 transition won).
// The transport half — minting, the close gate and the observer events — lives in hsmsss/socket_observer_test.go.

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// socketCloseCall is one CloseSocket call a socketClosingMock received.
type socketCloseCall struct {
	conn net.Conn
	err  error
}

// socketClosingMock is the lifecycle mock transport plus the CloseSocket capability,
// standing in for the HSMS-SS transport whose close gate records the failure it is handed.
// It never closes the conn itself; it only records the call.
type socketClosingMock struct {
	*mockTransport

	calls chan socketCloseCall
}

func newSocketClosingMock() *socketClosingMock {
	return &socketClosingMock{mockTransport: &mockTransport{}, calls: make(chan socketCloseCall, 8)}
}

func (m *socketClosingMock) CloseSocket(conn net.Conn, err error) {
	m.calls <- socketCloseCall{conn: conn, err: err}
}

// next waits for the next CloseSocket call.
func (m *socketClosingMock) next(t *testing.T) socketCloseCall {
	t.Helper()

	return waitBounded(t, m.calls, "the epoch never closed its socket through the transport's CloseSocket")
}

// closeCountConn is a distinct-identity net.Conn that counts its Close calls.
type closeCountConn struct {
	fakeConn

	closes atomic.Int32
}

func (c *closeCountConn) Close() error {
	c.closes.Add(1)

	return nil
}

// openAdopted opens a connection over tr whose first Start adopts sock under socket id socketID and never selects.
// arm, when non-nil, runs inside that first Start before the adoption,
// the point at which a supervisor test seam can be installed without racing the FSM goroutine.
// It returns the concrete connection and the adopting generation.
func openAdopted(t *testing.T, tr *socketClosingMock, sock net.Conn, socketID uint64, arm func(c *connection, gen uint64)) (*connection, uint64) {
	t.Helper()

	var genSeen atomic.Uint64
	adopted := make(chan struct{})
	var first atomic.Bool
	tr.startFn = func(_ context.Context, rt TransportRuntime) error {
		if !first.CompareAndSwap(false, true) {
			return nil // a reconnect after the scenario's own drop: stays NotConnected until Close
		}

		c := runtimeConnection(rt)
		gen := c.CurrentGeneration()
		genSeen.Store(gen)

		if arm != nil {
			arm(c, gen)
		}

		require.True(t, c.AdoptSocketFromGeneration(gen, sock, socketID), "the live generation must adopt its socket")
		close(adopted)

		return nil
	}

	cfg := DefaultConnectionConfig()
	conn, err := NewConnection(cfg, tr)
	require.NoError(t, err)

	c, ok := conn.(*connection)
	require.True(t, ok)

	require.NoError(t, conn.Open(t.Context(), OpenBackground))
	t.Cleanup(func() { _ = conn.Close() })

	<-adopted
	require.Eventually(t, func() bool { return conn.State() == NotSelectedState }, 5*time.Second, time.Millisecond)

	return c, genSeen.Load()
}

// Adoption stores the socket identity on the epoch alongside its conn,
// and the identity survives the teardown that clears the conn, so an event built late can still name the socket.
func TestSocketObserver_AdoptionStoresSocketIdentity(t *testing.T) {
	tr := newSocketClosingMock()
	sock := &closeCountConn{}
	c, _ := openAdopted(t, tr, sock, 41, nil)

	e := c.cur.Load()
	require.Same(t, sock, e.liveConn())
	require.Equal(t, uint64(41), e.socketID())

	require.NoError(t, c.Close())
	require.Nil(t, e.liveConn(), "teardown clears the conn")
	require.Equal(t, uint64(41), e.socketID(), "the socket identity outlives the conn")
}

// A generation that has already ended refuses the adoption, exactly as TCPUpFromGeneration does,
// and stores nothing on any epoch.
func TestSocketObserver_AdoptionRefusedForEndedGeneration(t *testing.T) {
	tr := newSocketClosingMock()
	c, gen := openAdopted(t, tr, &closeCountConn{}, 5, nil)
	e := c.cur.Load()

	require.NoError(t, c.Close())

	late := &closeCountConn{}
	require.False(t, c.AdoptSocketFromGeneration(gen, late, 6), "an ended generation must refuse the adoption")
	require.Nil(t, e.liveConn())
	require.Equal(t, uint64(5), e.socketID(), "a refused adoption must not overwrite the recorded identity")
}

// A local Close hands the epoch's socket to the transport's CloseSocket with no failure,
// and the core does not close the conn itself.
func TestSocketObserver_LocalCloseRoutesThroughCapabilityWithNilError(t *testing.T) {
	tr := newSocketClosingMock()
	sock := &closeCountConn{}
	c, _ := openAdopted(t, tr, sock, 3, nil)

	require.NoError(t, c.Close())

	call := tr.next(t)
	require.Same(t, sock, call.conn)
	require.NoError(t, call.err, "a local close carries no failure")
	require.Zero(t, sock.closes.Load(), "with the capability present the transport closes the socket, not the core")
}

// Without the capability the epoch closes its socket directly, exactly once.
func TestSocketObserver_EpochClosesDirectlyWithoutCapability(t *testing.T) {
	e := newEpoch(t.Context(), DefaultConnectionConfig().logger, 1)
	sock := &closeCountConn{}
	e.adoptConn(sock, 9)
	e.recordCloseFailure(errors.New("ignored without the capability"))

	e.closeSocket()
	e.closeSocket()

	require.Equal(t, int32(1), sock.closes.Load(), "the direct close runs once")
	require.Nil(t, e.liveConn())
	require.Equal(t, uint64(9), e.socketID())
}

// The first recorded failure wins, and the epoch hands it to the capability once.
func TestSocketObserver_EpochPassesFirstRecordedFailure(t *testing.T) {
	tr := newSocketClosingMock()
	e := newEpoch(t.Context(), DefaultConnectionConfig().logger, 1)
	e.closeConn = tr.CloseSocket

	sock := &closeCountConn{}
	e.adoptConn(sock, 2)

	first := errors.New("first")
	e.recordCloseFailure(first)
	e.recordCloseFailure(errors.New("second"))

	e.closeSocket()
	e.closeSocket()

	call := tr.next(t)
	require.Same(t, sock, call.conn)
	require.ErrorIs(t, call.err, first)
	require.Empty(t, tr.calls, "the capability is called once per socket")
}

// A write that fails on a live generation records its error on the epoch before it initiates the teardown,
// so the socket close that teardown performs carries that error.
func TestSocketObserver_WriteErrorReachesCloseSocket(t *testing.T) {
	tr := newSocketClosingMock()
	sock := &closeCountConn{}
	c, gen := openAdopted(t, tr, sock, 4, nil)

	require.True(t, c.CommitSelectedFromGeneration(gen))
	require.Eventually(t, func() bool { return c.State() == SelectedState }, 5*time.Second, time.Millisecond)

	injected := errors.New("injected write failure")
	tr.setWriteErr(injected)

	_, err := c.SendDataMessage(t.Context(), 1, 1, false, nil)
	require.ErrorIs(t, err, injected)

	call := tr.next(t)
	require.Same(t, sock, call.conn)
	require.ErrorIs(t, call.err, injected, "the close carries the write error that initiated it")
}

// A T7 expiry racing a Select commit:
// the epoch records ErrT7Timeout only when the T7 transition wins and drives the teardown.
// When the Select commit lands inside step's load-to-CAS window, the T7 report is discarded,
// and the later local close carries no failure — the report itself recorded nothing.
func TestSocketRace_T7RacingSelectRecordsOnlyWinner(t *testing.T) {
	tests := []struct {
		name       string
		selectWins bool
		wantErr    error
	}{
		{name: "T7 wins", selectWins: false, wantErr: ErrT7Timeout},
		{name: "Select wins", selectWins: true, wantErr: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := newSocketClosingMock()
			sock := &closeCountConn{}
			// Carries whether the interposed Select commit succeeded; the hook runs on the FSM goroutine,
			// where a require failure could not stop the test, so the outcome is asserted on the test goroutine.
			interposed := make(chan bool, 1)

			arm := func(c *connection, gen uint64) {
				if !tt.selectWins {
					return
				}

				var once sync.Once
				c.sup.Load().testHookAfterStateLoad = func(ev fsmEvent) {
					if ev != evT7Timeout {
						return
					}

					once.Do(func() {
						// The Select commit lands after step read NotSelected and before its T7 CAS.
						interposed <- c.CommitSelectedFromGeneration(gen)
					})
				}
			}

			c, gen := openAdopted(t, tr, sock, 8, arm)
			c.T7ExpiredFromGeneration(gen)

			if tt.selectWins {
				require.True(t, <-interposed, "the interposed Select commit must succeed")
				require.Eventually(t, func() bool { return c.State() == SelectedState }, 5*time.Second, time.Millisecond)
				require.Empty(t, tr.calls, "a discarded T7 closes nothing")
				require.NoError(t, c.Close())
			}

			call := tr.next(t)
			require.Same(t, sock, call.conn)
			if tt.wantErr == nil {
				require.NoError(t, call.err)
			} else {
				require.ErrorIs(t, call.err, tt.wantErr)
			}
		})
	}
}

// ObserveSocket hands the event it is given to the installed observer unchanged,
// and returns at once when none is installed.
func TestSocketObserver_ObserveSocketDelivers(t *testing.T) {
	var got []SocketEvent
	c, _ := newWireSendConn(t, WithSocketObserver(func(ev SocketEvent) { got = append(got, ev) }))

	want := SocketEvent{Kind: SocketClosed, Socket: 3, Generation: 2, At: time.Now(), Err: errors.New("x")}
	c.ObserveSocket(want)
	require.Equal(t, []SocketEvent{want}, got)

	bare, _ := newWireSendConn(t)
	require.NotPanics(t, func() { bare.ObserveSocket(want) })
}

// SocketEventKind names its values, and an unknown value prints its number.
func TestSocketEventKind_String(t *testing.T) {
	tests := []struct {
		kind SocketEventKind
		want string
	}{
		{SocketConnected, "Connected"},
		{SocketAccepted, "Accepted"},
		{SocketRefused, "Refused"},
		{SocketClosed, "Closed"},
		{SocketEventKind(0), "SocketEventKind(0)"},
		{SocketEventKind(9), "SocketEventKind(9)"},
	}

	for _, tt := range tests {
		require.Equal(t, tt.want, tt.kind.String())
	}
}
