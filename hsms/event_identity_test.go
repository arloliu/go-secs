package hsms

// event_identity_test.go pins which socket and generation a LifecycleEvent and a TxEvent name:
// the ones the transition or the send belonged to,
// never whichever generation happens to be current when the event is built or delivered.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A synchronous send pinned to one generation reports THAT generation and socket,
// even when a reconnect has published a successor by the time the send returns.
// The seam publishes the successor after the send captured its socket and before it writes.
//
// The second case also tears the pinned generation down in that window,
// so the send returns ErrConnClosed after teardown cleared the generation's connection:
// the event still names the socket that generation used.
//
// Teeth: resolving the identity from c.cur when the event is built names the successor (generation 6, socket 12).
func TestSocketRace_TxEventKeepsPinnedGenerationAcrossReconnect(t *testing.T) {
	cases := []struct {
		name        string
		teardown    bool
		wantOutcome TxOutcome
	}{
		{name: "sent after the successor published", wantOutcome: TxSent},
		{name: "canceled by the pinned generation's teardown", teardown: true, wantOutcome: TxCanceled},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spy := &txEventSpy{}
			c, _ := newTestSendConn(t, SelectedState, WithTransactionObserver(spy.observe))

			pinned := c.cur.Load()
			pinned.id = 5
			pinned.adoptConn(&idConn{id: 1}, 11)

			successor := identityEpoch(t, 6, 12)

			c.testHookAfterWriteLock = func() {
				if tc.teardown {
					pinned.teardown(time.Second)
				}
				c.cur.Store(successor)
			}

			_, err := c.WriteMessage(t.Context(), mustSendData(t, [4]byte{0, 0, 0, 9}, false))
			if tc.teardown {
				require.ErrorIs(t, err, ErrConnClosed)
				require.Nil(t, pinned.liveConn(), "teardown must have cleared the pinned generation's connection")
			} else {
				require.NoError(t, err)
			}

			require.Len(t, spy.events, 1)
			ev := spy.events[0]
			require.Equal(t, tc.wantOutcome, ev.Outcome)
			require.Equal(t, uint64(5), ev.Generation, "the event must name the generation the send was pinned to")
			require.Equal(t, uint64(11), ev.Socket, "the event must name the socket the pinned generation used")
			require.Equal(t, uint16(0xFFFF), ev.SessionID, "the session id comes from the primary")
		})
	}
}

// ForwardDataMessage's path pins its generation the same way.
func TestSocketRace_TxEventNoReplyKeepsPinnedGeneration(t *testing.T) {
	spy := &txEventSpy{}
	c, _ := newTestSendConn(t, SelectedState, WithTransactionObserver(spy.observe))

	pinned := c.cur.Load()
	pinned.id = 5
	pinned.adoptConn(&idConn{id: 1}, 11)

	successor := identityEpoch(t, 6, 12)

	c.testHookAfterWriteLock = func() { c.cur.Store(successor) }

	require.NoError(t, c.WriteMessageNoReply(t.Context(), mustSendData(t, [4]byte{0, 0, 0, 9}, true)))

	require.Len(t, spy.events, 1)
	require.Equal(t, TxSent, spy.events[0].Outcome)
	require.Equal(t, uint64(5), spy.events[0].Generation)
	require.Equal(t, uint64(11), spy.events[0].Socket)
}

// A send that finds no generation at all reports ErrNotOpen with generation 0 and socket 0,
// on both synchronous chokepoints.
func TestTxEvent_NotOpenReportsNoIdentity(t *testing.T) {
	spy := &txEventSpy{}
	cfg := DefaultConnectionConfig()
	require.NoError(t, cfg.apply(WithTransactionObserver(spy.observe)))

	conn, err := NewConnection(cfg, &mockTransport{})
	require.NoError(t, err)

	c, ok := conn.(*connection)
	require.True(t, ok)

	msg, err := NewDataMessage(1, 1, true, 0x0102, [4]byte{0, 0, 0, 1}, nil)
	require.NoError(t, err)

	_, err = c.WriteMessage(t.Context(), msg)
	require.ErrorIs(t, err, ErrNotOpen)
	require.ErrorIs(t, c.WriteMessageNoReply(t.Context(), msg), ErrNotOpen)

	require.Len(t, spy.events, 2)
	for _, ev := range spy.events {
		require.Equal(t, TxSendError, ev.Outcome)
		require.Zero(t, ev.Generation)
		require.Zero(t, ev.Socket)
		require.Equal(t, uint16(0x0102), ev.SessionID)
	}
}

// A lifecycle notification already fired keeps the identity it was fired with,
// even when the notifier delivers it only after its generation tore down and a successor was published.
//
// A blocking subscriber holds the notifier on the bring-up event while the drop fires and queues its own event,
// the drop's reaction tears the generation down, and the successor is published before the subscriber is released.
//
// Teeth: building the event from c.cur at delivery time names the successor (generation 2, socket 22).
func TestSocketRace_QueuedLifecycleEventKeepsGenerationAfterTeardown(t *testing.T) {
	_, c := newTestConn(t)
	c.shutdown.Store(true) // the drop's reaction tears down but starts no reconnect loop

	s := newSupervisor(c.react, &c.handlers, &c.lifecycleSubs)
	s.curGen = c.CurrentGeneration
	s.curEpoch = c.cur.Load
	c.sup.Store(s)

	genN := identityEpoch(t, 1, 21)
	c.cur.Store(genN)

	entered := make(chan struct{})
	release := make(chan struct{})
	got := make(chan LifecycleEvent, 4)
	first := true

	cancel := c.SubscribeLifecycle(func(ev LifecycleEvent) {
		if first {
			first = false
			close(entered)
			<-release
		}
		got <- ev
	})
	defer cancel()

	go s.notifier()
	defer func() {
		close(s.notify)
		<-s.notifierDone
	}()

	s.state.Store(uint32(NotSelectedState))
	s.step(fsmCommand{ev: evTCPUp, cause: CauseLocalOpen, gen: genN.id})
	<-entered // the notifier is parked inside the subscriber on the bring-up event

	s.step(fsmCommand{ev: evDisconnect, cause: CauseIOError, gen: genN.id})
	require.NoError(t, genN.wait(), "the drop's reaction must have torn the generation down")
	require.Nil(t, genN.liveConn())

	c.cur.Store(identityEpoch(t, 2, 22))

	close(release)

	up := <-got
	require.Equal(t, NotSelectedState, up.Current)
	require.Equal(t, uint64(1), up.Generation)
	require.Equal(t, uint64(21), up.Socket)

	drop := <-got
	require.Equal(t, NotConnectedState, drop.Current)
	require.Equal(t, CauseIOError, drop.Cause)
	require.Equal(t, uint64(1), drop.Generation, "a queued notification must keep the generation it fired for")
	require.Equal(t, uint64(21), drop.Socket, "a queued notification must keep the socket it fired for")
}

// The identity a fired transition carries comes from the epoch the transition belongs to:
// a Close names the epoch requestClose pinned, not the current one;
// a generation-named event names the current epoch it matched;
// an epoch that never acquired a socket reports socket 0;
// and a Close with no epoch at all reports 0/0.
func TestSupervisor_TransitionIdentitySnapshot(t *testing.T) {
	pinned := identityEpoch(t, 3, 7)
	current := identityEpoch(t, 4, 8)
	noSocket := identityEpoch(t, 9, 0)

	cases := []struct {
		name       string
		cur        *epoch
		from       ConnState
		fire       func(s *supervisor)
		wantGen    uint64
		wantSocket uint64
	}{
		{
			name: "close names the pinned epoch",
			cur:  current,
			from: SelectedState,
			fire: func(s *supervisor) {
				s.closeEpoch.Store(pinned)
				s.step(fsmCommand{ev: evClose, cause: CauseLocalClose})
			},
			wantGen:    3,
			wantSocket: 7,
		},
		{
			name:       "named event names the matched epoch",
			cur:        current,
			from:       SelectedState,
			fire:       func(s *supervisor) { s.step(fsmCommand{ev: evDisconnect, cause: CauseIOError, gen: 4}) },
			wantGen:    4,
			wantSocket: 8,
		},
		{
			name:       "epoch without a socket",
			cur:        noSocket,
			from:       NotSelectedState,
			fire:       func(s *supervisor) { s.step(fsmCommand{ev: evT7Timeout, cause: CauseT7Timeout, gen: 9}) },
			wantGen:    9,
			wantSocket: 0,
		},
		{
			name: "close with no epoch",
			cur:  nil,
			from: SelectedState,
			fire: func(s *supervisor) {
				s.requestClose(nil, CauseLocalClose)
				s.step(<-s.events)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestSupervisor(t)
			s.curGen = func() uint64 {
				if tc.cur == nil {
					return 0
				}

				return tc.cur.id
			}
			s.curEpoch = func() *epoch { return tc.cur }
			s.state.Store(uint32(tc.from))
			s.lastReacted = tc.from

			tc.fire(s)

			sc, ok := drainNotify(t, s)
			require.True(t, ok, "the transition must fire")
			require.Equal(t, NotConnectedState, sc.next)
			require.Equal(t, tc.wantGen, sc.gen)
			require.Equal(t, tc.wantSocket, sc.socket)
		})
	}
}

// identityEpoch builds a real epoch, so a Close can tear it down, with the given generation and socket.
// A socket of 0 leaves the epoch without one.
func identityEpoch(t *testing.T, id, socket uint64) *epoch {
	t.Helper()

	cfg := DefaultConnectionConfig()
	e := newEpoch(t.Context(), cfg.logger, cfg.senderQueueSize)
	e.id = id

	if socket != 0 {
		e.adoptConn(&idConn{}, socket)
	}

	return e
}

// Close before any Open fires no transition at all: there is no event, so nothing can carry an identity.
func TestLifecycle_CloseBeforeOpenReportsNothing(t *testing.T) {
	conn, _ := newTestConn(t)

	var events []LifecycleEvent
	cancel := conn.SubscribeLifecycle(func(ev LifecycleEvent) { events = append(events, ev) })
	defer cancel()

	require.ErrorIs(t, conn.Close(), ErrNotOpen)
	require.Empty(t, events)
}
