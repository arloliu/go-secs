package hsms

// socket_observer_test.go — white-box tests for the core half of the socket record:
// the adoption capability that stores a socket identity on the epoch,
// the epoch's socket close routed through the transport's optional CloseSocket capability,
// and the close reason the epoch records for that close, an error and a cause from one initiator
// (the write error that initiated the teardown, ErrT7Timeout only when the T7 transition won,
// the transition's own cause, a local Close, a Start that failed after the TCP-up),
// the first of which wins.
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
	conn  net.Conn
	err   error
	cause TransitionCause
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

func (m *socketClosingMock) CloseSocket(conn net.Conn, err error, cause TransitionCause) {
	m.calls <- socketCloseCall{conn: conn, err: err, cause: cause}
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

// A local Close hands the epoch's socket to the transport's CloseSocket with no failure and CauseLocalClose,
// and the core does not close the conn itself.
func TestSocketObserver_LocalCloseRoutesThroughCapabilityWithNilError(t *testing.T) {
	tr := newSocketClosingMock()
	sock := &closeCountConn{}
	c, _ := openAdopted(t, tr, sock, 3, nil)

	require.NoError(t, c.Close())

	call := tr.next(t)
	require.Same(t, sock, call.conn)
	require.NoError(t, call.err, "a local close carries no failure")
	require.Equal(t, CauseLocalClose, call.cause)
	require.Zero(t, sock.closes.Load(), "with the capability present the transport closes the socket, not the core")
}

// A Close that tears an epoch down without a transition still closes its socket with CauseLocalClose.
// The epoch holds a socket while the state is still NotConnected:
// the adoption published it and the TCP-up commit has not landed,
// so the Close moves the state nowhere and fires no transition, and only the Close's own teardown records a reason.
func TestSocketObserver_CloseWithoutTransitionReportsLocalClose(t *testing.T) {
	tr := newSocketClosingMock()
	sock := &closeCountConn{}
	published := make(chan struct{})
	tr.startFn = func(_ context.Context, rt TransportRuntime) error {
		c := runtimeConnection(rt)
		require.True(t, c.publishSocket(c.CurrentGeneration(), sock, 7), "the live generation must take the socket")
		close(published)

		return nil
	}

	conn, err := NewConnection(DefaultConnectionConfig(), tr)
	require.NoError(t, err)
	c, ok := conn.(*connection)
	require.True(t, ok)
	t.Cleanup(func() { _ = c.Close() })

	var events atomic.Int32
	t.Cleanup(c.SubscribeLifecycle(func(LifecycleEvent) { events.Add(1) }))

	require.NoError(t, c.Open(t.Context(), OpenBackground))
	<-published
	require.Equal(t, NotConnectedState, c.State())

	require.NoError(t, c.Close())

	call := tr.next(t)
	require.Same(t, sock, call.conn)
	require.NoError(t, call.err, "a local close carries no failure")
	require.Equal(t, CauseLocalClose, call.cause)
	require.Zero(t, events.Load(), "the Close fired no transition")
}

// Without the capability the epoch closes its socket directly, exactly once.
func TestSocketObserver_EpochClosesDirectlyWithoutCapability(t *testing.T) {
	e := newEpoch(t.Context(), DefaultConnectionConfig().logger, 1)
	sock := &closeCountConn{}
	e.adoptConn(sock, 9)
	e.recordCloseReason(errors.New("ignored without the capability"), CauseIOError)

	e.closeSocket()
	e.closeSocket()

	require.Equal(t, int32(1), sock.closes.Load(), "the direct close runs once")
	require.Nil(t, e.liveConn())
	require.Equal(t, uint64(9), e.socketID())
}

// The first recorded close reason wins whole, a reason without an error included,
// and the epoch hands it to the capability once;
// a later record changes neither its error nor its cause.
func TestSocketObserver_EpochPassesFirstRecordedReason(t *testing.T) {
	first := errors.New("first")
	later := errors.New("later")

	tests := []struct {
		name      string
		record    func(e *epoch)
		wantErr   error
		wantCause TransitionCause
	}{
		{
			name: "a failure first",
			record: func(e *epoch) {
				e.recordCloseReason(first, CauseIOError)
				e.recordCloseReason(later, CauseT7Timeout)
				e.recordCloseReason(nil, CauseLocalClose)
			},
			wantErr:   first,
			wantCause: CauseIOError,
		},
		{
			name: "a reason without a failure first",
			record: func(e *epoch) {
				e.recordCloseReason(nil, CauseLocalClose)
				e.recordCloseReason(later, CauseIOError)
			},
			wantErr:   nil,
			wantCause: CauseLocalClose,
		},
		{
			name:      "nothing recorded",
			record:    func(*epoch) {},
			wantErr:   nil,
			wantCause: CauseUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := newSocketClosingMock()
			e := newEpoch(t.Context(), DefaultConnectionConfig().logger, 1)
			e.closeConn = tr.CloseSocket

			sock := &closeCountConn{}
			e.adoptConn(sock, 2)

			tt.record(e)

			e.closeSocket()
			e.closeSocket()

			call := tr.next(t)
			require.Same(t, sock, call.conn)
			if tt.wantErr == nil {
				require.NoError(t, call.err)
			} else {
				require.ErrorIs(t, call.err, tt.wantErr)
			}
			require.Equal(t, tt.wantCause, call.cause)
			require.Empty(t, tr.calls, "the capability is called once per socket")
		})
	}
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
	require.Equal(t, CauseIOError, call.cause)
}

// A T7 expiry racing a Select commit:
// the epoch records ErrT7Timeout with CauseT7Timeout only when the T7 transition wins and drives the teardown.
// When the Select commit lands inside step's load-to-CAS window, the T7 report is discarded,
// and the later local close carries no failure and CauseLocalClose — the report itself recorded nothing.
func TestSocketRace_T7RacingSelectRecordsOnlyWinner(t *testing.T) {
	tests := []struct {
		name       string
		selectWins bool
		wantErr    error
		wantCause  TransitionCause
	}{
		{name: "T7 wins", selectWins: false, wantErr: ErrT7Timeout, wantCause: CauseT7Timeout},
		{name: "Select wins", selectWins: true, wantErr: nil, wantCause: CauseLocalClose},
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
			require.Equal(t, tt.wantCause, call.cause)
		})
	}
}

// errStartAfterAdoption is the Start failure the scripts below return once their Start has adopted a socket.
var errStartAfterAdoption = errors.New("hsms_test: start failed after its socket was adopted")

// A Start that fails after its TCP-up committed closes the socket it adopted with the reason the connection reports for that failure:
// on a reconnect, the Start error and CauseUnknown, as the reconnect loop reports it;
// on the initial Open, which rolls the connection back through a local close, no failure and CauseLocalClose.
func TestSocketObserver_StartFailureAfterTCPUp(t *testing.T) {
	t.Run("reconnect", func(t *testing.T) {
		tr := newSocketClosingMock()
		first, second := &closeCountConn{}, &closeCountConn{}
		var starts atomic.Int32
		tr.startFn = func(_ context.Context, rt TransportRuntime) error {
			c := runtimeConnection(rt)
			switch starts.Add(1) {
			case 1:
				c.AdoptSocketFromGeneration(c.CurrentGeneration(), first, 1)

				return nil
			case 2:
				c.AdoptSocketFromGeneration(c.CurrentGeneration(), second, 2)

				return errStartAfterAdoption
			default:
				return nil // the reconnect after the failure: stays NotConnected until Close
			}
		}

		conn, err := NewConnection(DefaultConnectionConfig(), tr)
		require.NoError(t, err)
		c, ok := conn.(*connection)
		require.True(t, ok)
		require.NoError(t, c.UpdateConfigOptions(WithT5(time.Nanosecond))) // near-immediate redial
		t.Cleanup(func() { _ = c.Close() })

		require.NoError(t, c.Open(t.Context(), OpenBackground))
		require.Eventually(t, func() bool { return c.State() == NotSelectedState }, 5*time.Second, time.Millisecond)
		c.TCPDownFromGeneration(c.CurrentGeneration(), errors.New("hsms_test: drop"), CauseIOError)

		require.Same(t, first, tr.next(t).conn, "the dropped generation closes its own socket first")

		call := tr.next(t)
		require.Same(t, second, call.conn)
		require.ErrorIs(t, call.err, errStartAfterAdoption, "the close carries the Start error")
		require.Equal(t, CauseUnknown, call.cause)
	})

	t.Run("initial Open rollback", func(t *testing.T) {
		tr := newSocketClosingMock()
		sock := &closeCountConn{}
		tr.startFn = func(_ context.Context, rt TransportRuntime) error {
			c := runtimeConnection(rt)
			require.True(t, c.AdoptSocketFromGeneration(c.CurrentGeneration(), sock, 1), "the live generation must adopt its socket")

			return errStartAfterAdoption
		}

		conn, err := NewConnection(DefaultConnectionConfig(), tr)
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })

		require.ErrorIs(t, conn.Open(t.Context(), OpenBackground), errStartAfterAdoption)

		call := tr.next(t)
		require.Same(t, sock, call.conn)
		require.NoError(t, call.err, "the rollback is a local close")
		require.Equal(t, CauseLocalClose, call.cause)
	})
}

// dropCauses records the cause of every lifecycle event that drops the link into NotConnected.
type dropCauses struct {
	mu     sync.Mutex
	causes []TransitionCause
}

func (d *dropCauses) observe(ev LifecycleEvent) {
	if ev.Current != NotConnectedState {
		return
	}

	d.mu.Lock()
	d.causes = append(d.causes, ev.Cause)
	d.mu.Unlock()
}

// wait waits (bounded poll) for the first drop and returns every drop recorded by then.
func (d *dropCauses) wait(t *testing.T) []TransitionCause {
	t.Helper()

	var out []TransitionCause
	require.Eventually(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		out = append([]TransitionCause(nil), d.causes...)

		return len(out) > 0
	}, 5*time.Second, time.Millisecond, "the drop must reach the subscriber")

	return out
}

// A write error recorded just before a concurrent Close wins the transition:
// the socket closes once, with the write error and CauseIOError,
// while the lifecycle event reports CauseLocalClose, because the Close drove the state machine.
// The FSM is held in the Close's step, before its transition fires, while the write fails and records its error;
// the write's own disconnect report queues behind the Close, which wins the transition.
func TestSocketRace_WriteErrorRacingCloseThatWinsTransition(t *testing.T) {
	tr := newSocketClosingMock()
	sock := &closeCountConn{}
	closeReached := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

	arm := func(c *connection, _ uint64) {
		var once sync.Once
		c.sup.Load().testHookAfterStateLoad = func(ev fsmEvent) {
			if ev != evClose {
				return
			}

			once.Do(func() {
				close(closeReached)
				<-release
			})
		}
	}

	c, gen := openAdopted(t, tr, sock, 6, arm)
	require.True(t, c.CommitSelectedFromGeneration(gen))
	require.Eventually(t, func() bool { return c.State() == SelectedState }, 5*time.Second, time.Millisecond)

	drops := &dropCauses{}
	t.Cleanup(c.SubscribeLifecycle(drops.observe))

	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()
	awaitSignal(t, closeReached, "the Close to reach step before its transition")

	injected := errors.New("injected write failure")
	tr.setWriteErr(injected)
	_, err := c.SendDataMessage(t.Context(), 1, 1, false, nil)
	require.ErrorIs(t, err, injected)

	releaseOnce.Do(func() { close(release) })
	require.NoError(t, waitBounded(t, closed, "Close must return"))

	call := tr.next(t)
	require.Same(t, sock, call.conn)
	require.ErrorIs(t, call.err, injected, "the write error was recorded first")
	require.Equal(t, CauseIOError, call.cause, "the cause comes from the same initiator as the error")
	require.Empty(t, tr.calls, "the socket is closed once")
	require.Equal(t, []TransitionCause{CauseLocalClose}, drops.wait(t), "the Close drove the state machine")
}

// A write error that lands after a T7 expiry has won its transition, and before the teardown that transition triggers:
// the socket closes once, with the write error and CauseIOError,
// while the lifecycle event reports CauseT7Timeout.
// The drop reaction is held after the T7 transition fired and before it records the T7 error,
// while a control write fails on the still-open socket.
func TestSocketRace_WriteErrorRacingWinningT7(t *testing.T) {
	tr := newSocketClosingMock()
	sock := &closeCountConn{}
	reactReached := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

	arm := func(c *connection, _ uint64) {
		var once sync.Once
		c.testHookReactBeforeCurLoad = func() {
			once.Do(func() {
				close(reactReached)
				<-release
			})
		}
	}

	c, gen := openAdopted(t, tr, sock, 7, arm)

	drops := &dropCauses{}
	t.Cleanup(c.SubscribeLifecycle(drops.observe))

	c.T7ExpiredFromGeneration(gen)
	awaitSignal(t, reactReached, "the T7 transition to fire and its reaction to start before its teardown")

	injected := errors.New("injected write failure")
	tr.setWriteErr(injected)
	_, err := c.WriteMessage(t.Context(), NewLinktestReq([4]byte{0, 0, 0, 1}))
	require.ErrorIs(t, err, injected)

	releaseOnce.Do(func() { close(release) })

	call := tr.next(t)
	require.Same(t, sock, call.conn)
	require.ErrorIs(t, call.err, injected, "the write error was recorded first")
	require.Equal(t, CauseIOError, call.cause, "the cause comes from the same initiator as the error")
	require.Equal(t, []TransitionCause{CauseT7Timeout}, drops.wait(t), "the T7 expiry drove the state machine")

	require.NoError(t, c.Close())
	require.Empty(t, tr.calls, "the socket is closed once")
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
