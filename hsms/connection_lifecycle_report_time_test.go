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
	s := newSupervisorWithEventsCap(func(ConnState, ConnState) { *reactions++ }, &c.handlers, &c.lifecycleSubs, 8)
	s.curGen = c.CurrentGeneration
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
