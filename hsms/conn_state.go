package hsms

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/arloliu/go-secs/logger"
)

// ConnState represents the various stages of an HSMS connection.
type ConnState uint32

// IsNotConnected returns if the current state is not connected.
func (cs ConnState) IsNotConnected() bool { return cs == NotConnectedState }

// IsNotSelected returns if the current state is not selected.
func (cs ConnState) IsNotSelected() bool { return cs == NotSelectedState }

// IsSelected returns if the current state is selected.
func (cs ConnState) IsSelected() bool { return cs == SelectedState }

// String returns string representation of the current state.
func (cs ConnState) String() string {
	switch cs {
	case NotConnectedState:
		return "not-connected"
	case NotSelectedState:
		return "not-selected"
	case SelectedState:
		return "selected"
	default:
		return "unknown"
	}
}

// HSMS connection states representing the various stages of an HSMS connection.
const (
	// NotConnectedState indicates that the TCP/IP connection is not established.
	NotConnectedState ConnState = iota
	// NotSelectedState indicates that the HSMS connection is established, but not yet ready for data exchange.
	NotSelectedState
	// SelectedState indicates that the HSMS connection is established and ready for data exchange.
	SelectedState
)

// ConnStateChangeHandler is a function type that represents a handler for connection state changes.
// It is invoked when the state of an HSMS connection changes.
//
// Note: the handler will be invoked in a blocking mode. Take care with long-running implementations.
//
// The handler function receives two arguments:
//   - prevState: The previous connection state.
//   - newState: The current connection state.
type ConnStateChangeHandler func(conn Connection, prevState ConnState, newState ConnState)

// ConnStateMgr manages the connection state of an HSMS connection.
//
// It provides methods for managing state transitions and notifying listeners of state changes.
// The state transitions are thread safety in concurrent environments.
type ConnStateMgr struct {
	mu               sync.Mutex
	ctx              context.Context
	cond             *sync.Cond
	state            atomic.Uint32
	conn             Connection
	logger           logger.Logger
	asyncStateChange chan ConnState
	handlers         []ConnStateChangeHandler
}

// NewConnStateMgr creates a new ConnStateMgr instance, initializing it to the NotConnectedState.
//
// It accepts optional ConnStateChangeHandler functions that will be invoked when the connection state changes.
func NewConnStateMgr(ctx context.Context, conn Connection, handlers ...ConnStateChangeHandler) *ConnStateMgr {
	connState := &ConnStateMgr{
		ctx:              ctx,
		conn:             conn,
		asyncStateChange: make(chan ConnState, 10),
		handlers:         make([]ConnStateChangeHandler, 0, len(handlers)),
	}

	for _, handler := range handlers {
		connState.AddHandler(handler)
	}

	if conn != nil {
		connState.logger = conn.GetLogger()
	} else {
		connState.logger = logger.GetLogger()
	}

	connState.state.Store(uint32(NotConnectedState))
	connState.cond = sync.NewCond(&connState.mu)

	go connState.asyncStateChangeTask()

	return connState
}

// State returns the current connection state.
func (cs *ConnStateMgr) State() ConnState {
	return ConnState(cs.state.Load())
}

// AddHandler adds one or more ConnStateChangeHandler functions to be invoked on state changes.
func (cs *ConnStateMgr) AddHandler(handlers ...ConnStateChangeHandler) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.handlers = append(cs.handlers, handlers...)
}

// WaitState waits for the connection state to reach the specified state or until the context is done.
// It returns nil if the desired state is reached, or an error if the context is canceled or times out.
func (cs *ConnStateMgr) WaitState(ctx context.Context, state ConnState) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	cs.logger.Debug("wait connection state", "cur_state", cs.State(), "desired_state", state)
	if cs.State() == state {
		return nil
	}

	stopFunc := context.AfterFunc(ctx, func() {
		cs.cond.Broadcast()
	})
	defer stopFunc()

	for cs.State() != state {
		select {
		case <-ctx.Done():
			cs.logger.Debug("wait connection state receive ctx done", "cur_state", cs.State(), "desired_state", state)
			return ctx.Err()
		default:
			cs.logger.Debug("wait connection state CALL WAIT", "cur_state", cs.State(), "desired_state", state)
			cs.cond.Wait()
		}
	}
	cs.logger.Debug("wait connection state finished", "cur_state", cs.State(), "desired_state", state)

	return nil
}

// ToNotConnected transitions the connection state to NotConnectedState.
// This transition is allowed from any state and represents a disconnection or a reset of the connection.
func (cs *ConnStateMgr) ToNotConnected() {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	curState := cs.State()

	if curState == NotConnectedState {
		cs.logger.Debug("Already in NotConnectedState, no need to transition")
		return // Already in NotConnectedState, no need to transition
	}

	// change state to not connected BEFORE all handlers finished
	cs.setState(NotConnectedState)

	cs.invokeHandlers(curState, NotConnectedState)
}

// ToNotSelected transitions the connection state to NotSelectedState.
//
// This transition is only allowed from the NotConnected (HSMS-SS and HSMS-GS) or SelectedState (HSMS-GS only).
// If the state is already NotSelectedState, the function is a no-op.
//
// Returns nil on success, or ErrInvalidTransition if the current state is not ConnectedState.
func (cs *ConnStateMgr) ToNotSelected() error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	curState := cs.State()

	if curState.IsNotSelected() {
		return nil // Already in NotSelectedState, No-op
	}

	if cs.conn != nil && cs.conn.IsSingleSession() && !curState.IsNotConnected() { // HSMS-SS
		return ErrInvalidTransition
	} else if !curState.IsNotConnected() && !curState.IsSelected() { // HSMS-GS
		return ErrInvalidTransition
	}

	cs.invokeHandlers(curState, NotSelectedState)
	// change state after all handlers finished
	cs.setState(NotSelectedState)

	return nil
}

// ToSelected transitions the connection state to SelectedState.
//
// This transition is only allowed from the NotSelectedState and indicates that the HSMS session is
// established and ready for data exchange.
// If the state is already SelectedState, the function is a no-op.
//
// Returns nil on success, or ErrInvalidTransition if the current state is not NotSelectedState.
func (cs *ConnStateMgr) ToSelected() error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	curState := cs.State()

	if curState.IsSelected() {
		return nil // Already in SelectedState, No-op
	}

	// Only allow transition from NotSelectedState
	if !curState.IsNotSelected() {
		return ErrInvalidTransition
	}

	cs.invokeHandlers(curState, SelectedState)
	// change state after all handlers finished
	cs.setState(SelectedState)

	return nil
}

// ToNotConnectedAsync transitions connection state to NotConnectedState asynchronously.
//
// It will notify a goroutine and transite state in the back asynchronously.
//
// If the state is the same as the current state, the function is a no-op.
func (cs *ConnStateMgr) ToNotConnectedAsync() {
	cs.changeStateAsync(NotConnectedState)
}

// ToNotSelectedAsync transitions connection state to NotSelectedState asynchronously.
//
// It will notify a goroutine and transite state in the back asynchronously.
//
// If the state is the same as the current state, the function is a no-op.
func (cs *ConnStateMgr) ToNotSelectedAsync() {
	cs.changeStateAsync(NotSelectedState)
}

// ToSelectedAsync transitions connection state to SelectedState asynchronously.
//
// It will notify a goroutine and transite state in the back asynchronously.
//
// If the state is the same as the current state, the function is a no-op.
func (cs *ConnStateMgr) ToSelectedAsync() {
	cs.changeStateAsync(SelectedState)
}

// IsNotConnected returns if the current state is not connected.
func (cs *ConnStateMgr) IsNotConnected() bool {
	return cs.State().IsNotConnected()
}

// IsNotSelected returns if the current state is not selected.
func (cs *ConnStateMgr) IsNotSelected() bool {
	return cs.State().IsNotSelected()
}

// IsSelected returns if the current state is selected.
func (cs *ConnStateMgr) IsSelected() bool {
	return cs.State().IsSelected()
}

// setState atomically set current state to the newState. It also broadcasts a signal to any waiting goroutines.
func (cs *ConnStateMgr) setState(newState ConnState) {
	cs.state.Store(uint32(newState))
	cs.cond.Broadcast()
}

// invokeHandlers invokes all registered ConnStateChangeHandler functions with the previous and new states.
func (cs *ConnStateMgr) invokeHandlers(prevState ConnState, newState ConnState) {
	for _, handler := range cs.handlers {
		if handler != nil {
			handler(cs.conn, prevState, newState)
		}
	}
}

// changeStateAsync transitions the desired connection state asynchronously.
//
// It will notify a goroutine and transite state in the back asynchronously.
//
// If the state is the same as the current state, the function is a no-op.
func (cs *ConnStateMgr) changeStateAsync(state ConnState) {
	if cs.State() == state {
		return
	}

	cs.asyncStateChange <- state
}

// asyncStateChangeTask handles state changing in the background.
func (cs *ConnStateMgr) asyncStateChangeTask() {
	defer cs.logger.Debug("asyncStateChangeTask terminated")

	for {
		select {
		case <-cs.ctx.Done():
			return

		case desiredState := <-cs.asyncStateChange:
			prevState := cs.State()

			cs.logger.Debug("[start] async connection state",
				"method", "asyncStateChangeTask",
				"prevState", prevState, "curState", cs.State(), "desiredState", desiredState,
			)
			if desiredState == prevState {
				cs.logger.Debug("same state, exit", "method", "asyncStateChangeTask", "state", desiredState)
				break
			}

			var err error
			switch desiredState {
			case NotConnectedState:
				cs.ToNotConnected()
			case NotSelectedState:
				err = cs.ToNotSelected()
			case SelectedState:
				err = cs.ToSelected()
			}

			if err != nil {
				cs.logger.Error("[failed] async connection state",
					"method", "asyncStateChangeTask",
					"prevState", prevState, "curState", cs.State(), "desiredState", desiredState,
					"error", err,
				)
				if errors.Is(err, ErrInvalidTransition) {
					cs.asyncStateChange <- NotConnectedState
				}
			}
			cs.logger.Debug("[end] async connection state",
				"method", "asyncStateChangeTask",
				"prevState", prevState, "curState", cs.State(), "desiredState", desiredState,
			)
		}
	}
}
