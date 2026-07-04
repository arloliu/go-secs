package hsms

import "strconv"

// ConnState is the HSMS connection state (SEMI E37 §5.4–§5.6): the only three
// logical states the FSM exposes.
//
// The three-state FSM carries HSMS-SS semantics (the Select/Deselect handshake).
// A SECS-I connection has no HSMS select handshake, so its lifecycle does not move
// through NotSelectedState as an HSMS-SS link does; treat these transitions as
// HSMS-SS-specific when consuming state for a SECS-I connection.
type ConnState uint32

const (
	// NotConnectedState is the state while TCP is down (active dialing or passive
	// listening happen here).
	NotConnectedState ConnState = iota
	// NotSelectedState is the state after TCP is up but before the session is
	// selected (the HSMS-SS T7 dwell applies here).
	NotSelectedState
	// SelectedState is the state in which the link is selected and data flows.
	SelectedState
)

// String returns the string representation of the ConnState.
func (s ConnState) String() string {
	switch s {
	case NotConnectedState:
		return "NotConnected"
	case NotSelectedState:
		return "NotSelected"
	case SelectedState:
		return "Selected"
	default:
		return "ConnState(" + strconv.FormatUint(uint64(s), 10) + ")"
	}
}

// StateChangeHandler observes a logical state transition. Handlers run only on
// the notifier goroutine, never inline on a protocol goroutine.
type StateChangeHandler func(prev, next ConnState)

// OpenMode selects Open's blocking behavior.
type OpenMode uint8

const (
	// OpenWaitSelected makes Open block until the link is Selected or a fatal
	// connect error occurs.
	OpenWaitSelected OpenMode = iota
	// OpenBackground makes Open start the lifecycle and return immediately;
	// connect, select, and reconnect run in the background.
	OpenBackground
)

// NOTE: the internal fsmEvent type + its constants (evTCPUp/evSelectAccepted/…) are defined in
// supervisor.go (Task 8), where they are first used — declaring them here would leave them
// `unused`-lint-red until the supervisor lands.
