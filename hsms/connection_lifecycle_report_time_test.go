package hsms

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// newUnstartedReportTimeConn builds a connection wired with a real supervisor — the same generation-match logic production uses —
// but never runs it (no go s.run()/notifier()).
// The caller drives every transition explicitly via s.step,
// so a captured event can be replayed at a schedule point the test itself chooses, not one the goroutine scheduler happens to pick.
// react counts every deduped transition the supervisor fires,
// so "no reaction" is asserted directly instead of inferred from timing.
func newUnstartedReportTimeConn(t *testing.T) (c *connection, reactions *int) {
	t.Helper()

	conn, err := NewConnection(DefaultConnectionConfig(), &mockTransport{})
	require.NoError(t, err)

	c, ok := conn.(*connection)
	require.True(t, ok, "NewConnection must return the concrete *connection")

	reactions = new(int)
	s := newSupervisorWithEventsCap(func(ConnState, ConnState, TransitionCause) { *reactions++ }, &c.handlers, &c.lifecycleSubs, 8)
	s.curGen = c.CurrentGeneration
	s.curEpoch = c.cur.Load
	c.sup.Store(s)

	return c, reactions
}

// TestReconnect_UnnamedDisconnectStaysBoundToItsReportingGeneration is the deterministic counterpart to TestReconnect_StaleRecvLoopTCPDownDoesNotDisconnectNewGen:
// it freezes the schedule instead of racing it.
//
// TCPDown carries no generation identity, so its queued event must be matched against the generation
// that was current when the report was MADE, not whichever one is current when the FSM gets around to processing it.
// This test captures the queued command while the reporting generation is still the one waiting for its successor,
// then publishes that successor and lands its own TCP-up commit BEFORE the captured command is ever stepped —
// reproducing the exact window a stale recv-loop report can land in without depending on which goroutine the scheduler happens to run first.
func TestReconnect_UnnamedDisconnectStaysBoundToItsReportingGeneration(t *testing.T) {
	c, reactions := newUnstartedReportTimeConn(t)
	s := c.sup.Load()

	genN := &epoch{id: 1}
	genN.markEnded() // already torn down, waiting for its successor to publish
	c.cur.Store(genN)

	c.TCPDown(errors.New("hsms_test: stale drop reported by a generation that already ended"))

	require.True(t, genN.commsFailure.Load(), "the reporting generation's own comms-failure flag must be set at report time")

	var cmd fsmCommand
	select {
	case cmd = <-s.events:
	default:
		t.Fatal("TCPDown must enqueue evDisconnect")
	}

	genN1 := &epoch{id: genN.id + 1}
	c.cur.Store(genN1) // the reconnect loop publishes the successor
	require.True(t, s.commitFrom(genN1.id, NotConnectedState, NotSelectedState, evTCPUp, CauseLocalOpen),
		"the successor's own TCP-up commit must land before the captured report is stepped")

	staleBefore := s.staleGen.Load()
	s.step(cmd) // step the report captured back when it was still bound to genN

	require.Equal(t, NotSelectedState, s.State(),
		"a report bound to the generation that already ended must not disconnect its successor")
	require.Equal(t, staleBefore+1, s.staleGen.Load())
	require.Equal(t, 0, *reactions, "a report bound to an ended generation must fire no reaction")
	require.False(t, genN1.commsFailure.Load(), "the successor's own comms-failure flag must stay untouched")
}

// TestReconnect_UnnamedT7ExpiryStaysBoundToItsReportingGeneration is the T7-dwell-expiry counterpart of TestReconnect_UnnamedDisconnectStaysBoundToItsReportingGeneration:
// the same unnamed-report hazard, for the other back-channel event that names no generation of its own.
func TestReconnect_UnnamedT7ExpiryStaysBoundToItsReportingGeneration(t *testing.T) {
	c, reactions := newUnstartedReportTimeConn(t)
	s := c.sup.Load()

	genN := &epoch{id: 1}
	genN.markEnded()
	c.cur.Store(genN)

	c.T7Expired()

	var cmd fsmCommand
	select {
	case cmd = <-s.events:
	default:
		t.Fatal("T7Expired must enqueue evT7Timeout")
	}

	genN1 := &epoch{id: genN.id + 1}
	c.cur.Store(genN1)
	require.True(t, s.commitFrom(genN1.id, NotConnectedState, NotSelectedState, evTCPUp, CauseLocalOpen),
		"the successor's own TCP-up commit must land before the captured report is stepped")

	staleBefore := s.staleGen.Load()
	s.step(cmd)

	require.Equal(t, NotSelectedState, s.State(),
		"a T7 report bound to the generation that already ended must not disconnect its successor")
	require.Equal(t, staleBefore+1, s.staleGen.Load())
	require.Equal(t, 0, *reactions, "a report bound to an ended generation must fire no reaction")
	require.False(t, genN1.commsFailure.Load(), "the successor's own comms-failure flag must stay untouched")
}

// wireRealGates installs the connection's real gates on s, in place of the nil gates newUnstartedReportTimeConn leaves behind.
// A test that wants to exercise a synchronous commit's generation fence —
// rather than the nil-gate bypass every commit falls back to when no connection backs it —
// must call this before making that commit,
// or it exercises the bypass and proves nothing about the fence.
func wireRealGates(c *connection, s *supervisor) {
	s.commitGate = c.commitGate
	s.selectGate = c.selectCommitGate
	s.tcpUpCommitGate = c.tcpUpCommitGate
}

// TestReconnect_DelayedUnnamedSelectAcceptedRejectedAfterSuccessorPublishes proves the queued-follow-up fence:
// a Select-accepted commit's own report is captured while it is still bound to the generation that validated it,
// delayed past that generation's end and its successor's own TCP-up, and only then stepped.
//
// The command's gen is asserted to equal N's id, proving the report was queued bound to N.
//
// It then steps the successor's own reports afterward and asserts their reaction, notification, and marker effects,
// proving the rejection above cost the successor nothing.
func TestReconnect_DelayedUnnamedSelectAcceptedRejectedAfterSuccessorPublishes(t *testing.T) {
	c, reactions := newUnstartedReportTimeConn(t)
	s := c.sup.Load()
	wireRealGates(c, s)

	genN := &epoch{id: 1, genGate: &c.genGate}
	c.cur.Store(genN)
	s.state.Store(uint32(NotSelectedState))
	s.lastReacted = NotSelectedState

	require.True(t, c.CommitSelected(), "N's own Select CAS must land synchronously")
	require.Equal(t, SelectedState, s.State())

	var cmd fsmCommand
	select {
	case cmd = <-s.events:
	default:
		t.Fatal("CommitSelected must enqueue evSelectAccepted")
	}

	genN.markEnded()                         // teardown
	s.state.Store(uint32(NotConnectedState)) // the drop
	s.lastReacted = NotConnectedState

	genN1 := &epoch{id: genN.id + 1, genGate: &c.genGate}
	c.cur.Store(genN1)
	c.TCPUp(fakeConn{})
	require.Equal(t, NotSelectedState, s.State(), "the successor's own TCP-up CAS must land")
	require.True(t, genN1.tcpUpCommitted.Load(), "the successor's own TCP-up marker must be set")

	staleBefore := s.staleGen.Load()
	reactionsBefore := *reactions
	s.step(cmd) // step the report captured back when it was still bound to genN

	require.Equal(t, NotSelectedState, s.State(),
		"a report bound to the generation that already ended must not promote its successor without its own handshake")
	require.Equal(t, staleBefore+1, s.staleGen.Load())
	require.Equal(t, reactionsBefore, *reactions, "a report bound to an ended generation must fire no reaction")
	require.Equal(t, genN.id, cmd.gen, "the queued follow-up must carry the generation the gate validated, not 0")

	select {
	case sc := <-s.notify:
		t.Fatalf("a report bound to an ended generation must fire no notification, got %+v", sc)
	default:
	}

	// The successor's own reports must still land normally, proving the rejection above retained its useful work.
	var tcpUpCmd fsmCommand
	select {
	case tcpUpCmd = <-s.events:
	default:
		t.Fatal("the successor's own TCP-up must have enqueued its follow-up")
	}
	require.Equal(t, genN1.id, tcpUpCmd.gen)
	s.step(tcpUpCmd)
	require.Equal(t, NotSelectedState, s.State())
	require.Equal(t, reactionsBefore+1, *reactions, "the successor's own bring-up must still fire its reaction")

	select {
	case sc := <-s.notify:
		require.Equal(t, stateChange{prev: NotConnectedState, next: NotSelectedState, cause: CauseLocalOpen, gen: genN1.id}, sc,
			"the successor's own bring-up must notify with its own cause")
	default:
		t.Fatal("the successor's own bring-up must notify")
	}

	require.True(t, c.CommitSelected(), "the successor's own Select CAS must still land")
	require.True(t, genN1.reachedSelected.Load(), "the successor's own commit must latch its reconnect-backoff marker")

	var selCmd fsmCommand
	select {
	case selCmd = <-s.events:
	default:
		t.Fatal("the successor's own Select-accepted must have enqueued its follow-up")
	}
	require.Equal(t, genN1.id, selCmd.gen)
	s.step(selCmd)
	require.Equal(t, SelectedState, s.State())
	require.Equal(t, reactionsBefore+2, *reactions, "the successor's own entering-Selected reaction must fire")

	select {
	case sc := <-s.notify:
		require.Equal(t, stateChange{prev: NotSelectedState, next: SelectedState, cause: CauseSelectAccepted, gen: genN1.id}, sc,
			"the successor's own entering-Selected transition must notify with CauseSelectAccepted")
	default:
		t.Fatal("the successor's own entering-Selected transition must notify")
	}
}

// TestReconnect_DelayedUnnamedReportsRejectedBeforeSuccessorsOwnBringUpIsStepped is the TCP-up and Select-lost counterpart of TestReconnect_DelayedUnnamedSelectAcceptedRejectedAfterSuccessorPublishes:
// the same capture-delay-replay shape, for the other two synchronous commits, stepped against a successor whose own TCP-up CAS has landed but whose own follow-up has not yet been stepped —
// the schedule under which an unstamped report could still coalesce into the successor's own bring-up (a TCP-up) or fire a wrong-cause notification (a Select-lost), rather than being cleanly discarded.
func TestReconnect_DelayedUnnamedReportsRejectedBeforeSuccessorsOwnBringUpIsStepped(t *testing.T) {
	cases := []struct {
		name      string
		fromState ConnState
		toState   ConnState
		commit    func(c *connection) bool
		wantEv    fsmEvent
	}{
		{
			name:      "TCP-up",
			fromState: NotConnectedState,
			toState:   NotSelectedState,
			commit: func(c *connection) bool {
				c.TCPUp(fakeConn{})

				return c.State() == NotSelectedState
			},
			wantEv: evTCPUp,
		},
		{
			name:      "Select-lost",
			fromState: SelectedState,
			toState:   NotSelectedState,
			commit: func(c *connection) bool {
				c.SelectLost()

				return c.State() == NotSelectedState
			},
			wantEv: evSelectLost,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, reactions := newUnstartedReportTimeConn(t)
			s := c.sup.Load()
			wireRealGates(c, s)

			genN := &epoch{id: 1, genGate: &c.genGate}
			c.cur.Store(genN)
			s.state.Store(uint32(tc.fromState))
			s.lastReacted = tc.fromState

			require.True(t, tc.commit(c), "N's own commit must land synchronously")
			require.Equal(t, tc.toState, s.State())

			var cmd fsmCommand
			select {
			case cmd = <-s.events:
			default:
				t.Fatal("the commit must enqueue its follow-up")
			}
			require.Equal(t, tc.wantEv, cmd.ev)

			genN.markEnded()
			s.state.Store(uint32(NotConnectedState))
			s.lastReacted = NotConnectedState

			genN1 := &epoch{id: genN.id + 1, genGate: &c.genGate}
			c.cur.Store(genN1)
			c.TCPUp(fakeConn{}) // the successor's own TCP-up CAS lands; its own follow-up is left queued, not stepped yet
			require.Equal(t, NotSelectedState, s.State())

			staleBefore := s.staleGen.Load()
			reactionsBefore := *reactions
			s.step(cmd) // step the report captured back when it was still bound to genN

			require.Equal(t, NotSelectedState, s.State(),
				"a report bound to the generation that already ended must not be applied to its successor")
			require.Equal(t, staleBefore+1, s.staleGen.Load())
			require.Equal(t, reactionsBefore, *reactions, "a report bound to an ended generation must fire no reaction")
			require.Equal(t, genN.id, cmd.gen, "the queued follow-up must carry the generation the gate validated, not 0")

			select {
			case sc := <-s.notify:
				t.Fatalf("a report bound to an ended generation must fire no notification, got %+v", sc)
			default:
			}

			// The successor's own TCP-up follow-up must still land normally, proving the rejection above retained its useful work.
			var succCmd fsmCommand
			select {
			case succCmd = <-s.events:
			default:
				t.Fatal("the successor's own TCP-up must have enqueued its follow-up")
			}
			require.Equal(t, genN1.id, succCmd.gen)
			s.step(succCmd)
			require.Equal(t, NotSelectedState, s.State())
			require.Equal(t, reactionsBefore+1, *reactions, "the successor's own bring-up must still fire its reaction")

			select {
			case sc := <-s.notify:
				require.Equal(t, stateChange{prev: NotConnectedState, next: NotSelectedState, cause: CauseLocalOpen, gen: genN1.id}, sc,
					"the successor's own bring-up must notify with its own cause")
			default:
				t.Fatal("the successor's own bring-up must notify")
			}
		})
	}
}

// TestReconnect_UnnamedSelectLostRefusedOnEndedCurrentGeneration pins the Select-lost commit's own liveness fence for a gen of 0:
// an ended-but-still-current generation
// (the shape a Start failure that reports TCP-up and then errors can leave behind, before its own hand-off disconnect is stepped)
// refuses the commit exactly like a named one would,
// and a live generation still commits normally, with its follow-up carrying that generation's id.
func TestReconnect_UnnamedSelectLostRefusedOnEndedCurrentGeneration(t *testing.T) {
	cases := []struct {
		name       string
		ended      bool
		wantState  ConnState
		wantStale  uint64
		wantQueued bool
	}{
		{name: "ended current generation refuses the commit", ended: true, wantState: SelectedState, wantStale: 1, wantQueued: false},
		{name: "live current generation still commits", ended: false, wantState: NotSelectedState, wantStale: 0, wantQueued: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newUnstartedReportTimeConn(t)
			s := c.sup.Load()
			wireRealGates(c, s)

			genN := &epoch{id: 1, genGate: &c.genGate}
			c.cur.Store(genN)
			s.state.Store(uint32(SelectedState))
			s.lastReacted = SelectedState

			if tc.ended {
				genN.markEnded() // ended but still current: no successor has published yet
			}

			staleBefore := s.staleGen.Load()
			c.SelectLost()

			require.Equal(t, tc.wantState, s.State())
			require.Equal(t, staleBefore+tc.wantStale, s.staleGen.Load())

			select {
			case cmd := <-s.events:
				require.True(t, tc.wantQueued, "no follow-up must be enqueued for a refused commit")
				require.Equal(t, genN.id, cmd.gen)
			default:
				require.False(t, tc.wantQueued, "the commit must enqueue its follow-up")
			}
		})
	}
}
