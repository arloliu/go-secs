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

// IsConnecting returns if the current state is connecting.
func (cs ConnState) IsConnecting() bool { return cs == ConnectingState }

// IsNotSelected returns if the current state is not selected.
func (cs ConnState) IsNotSelected() bool { return cs == NotSelectedState }

// IsSelected returns if the current state is selected.
func (cs ConnState) IsSelected() bool { return cs == SelectedState }

// String returns string representation of the current state.
func (cs ConnState) String() string {
	switch cs {
	case NotConnectedState:
		return "not-connected"
	case ConnectingState:
		return "connecting"
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
	// This state is used when the connection is not yet established or has been closed.
	NotConnectedState ConnState = iota

	// ConnectingState indicates that the TCP/IP connection is in the process of being established.
	// This state is not a final state, but rather a transitional state.
	//   - For the active connection, this state is used when trying to connect to the remote server.
	//   - For the passive connection, this state is used when listening for incoming connections and waiting for a client to connect.
	ConnectingState

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
	mu              sync.Mutex
	stateMu         sync.Mutex
	pctx            context.Context
	ctx             context.Context
	cancel          context.CancelFunc
	wg              sync.WaitGroup
	shutdowned      atomic.Bool
	cond            *sync.Cond
	state           atomic.Uint32
	desiredState    atomic.Uint32
	conn            Connection
	logger          logger.Logger
	stateChangeChan chan ConnState
	handlers        []ConnStateChangeHandler
}

// NewConnStateMgr creates a new ConnStateMgr instance, initializing it to the NotConnectedState.
//
// It accepts optional ConnStateChangeHandler functions that will be invoked when the connection state changes.
func NewConnStateMgr(ctx context.Context, conn Connection, handlers ...ConnStateChangeHandler) *ConnStateMgr {
	cs := &ConnStateMgr{
		pctx:     ctx,
		conn:     conn,
		handlers: make([]ConnStateChangeHandler, 0, len(handlers)),
	}

	for _, handler := range handlers {
		cs.AddHandler(handler)
	}

	if conn != nil {
		cs.logger = conn.GetLogger()
	} else {
		cs.logger = logger.GetLogger()
	}

	cs.state.Store(uint32(NotConnectedState))
	cs.cond = sync.NewCond(&cs.mu)

	return cs
}

func (cs *ConnStateMgr) Start() {
	cs.stateMu.Lock()
	defer cs.stateMu.Unlock()

	cs.logger.Debug("start connection state manager")

	cctx, cancel := context.WithCancel(cs.pctx)
	cs.ctx = cctx
	cs.cancel = cancel
	cs.stateChangeChan = make(chan ConnState, 10)

	cs.logger.Debug("set shutdowned flag to false")
	cs.shutdowned.Store(false)

	cs.wg.Add(1)
	go cs.asyncStateChangeTask()
}

func (cs *ConnStateMgr) Stop() {
	if cs.shutdowned.Load() {
		cs.logger.Debug("conn state manager already shutdowned, ignore stop",
			"method", "Stop",
			"curState", cs.State(),
			"desiredState", cs.DesiredState(),
			"shutdowned", cs.shutdowned.Load(),
		)

		return
	}

	cs.logger.Debug("stop connection state manager")
	cs.cancel()

	cs.wg.Wait()

	cs.stateMu.Lock()
	// set shutdowned flag to true to prevent any further state changes
	// it shoule be set before closing the stateChangeChan to prevent any further channel writes
	cs.shutdowned.Store(true)

	cs.logger.Debug("close stateChangeChan", "shutdowned", cs.shutdowned.Load())
	close(cs.stateChangeChan)

	cs.setState(NotConnectedState)
	cs.logger.Debug("stop connection state manager finished", "state", cs.State().String())
	cs.stateMu.Unlock()
}

// State returns the current connection state.
func (cs *ConnStateMgr) State() ConnState {
	return ConnState(cs.state.Load())
}

func (cs *ConnStateMgr) DesiredState() ConnState {
	return ConnState(cs.desiredState.Load())
}

// AddHandler adds one or more ConnStateChangeHandler functions to be invoked on state changes.
//
// The handler is responsible for processing the state change and taking appropriate action.
//   - Ths handler should keep as light as possible to avoid blocking the channel.
//   - The handler should only call async state change methods to avoid deadlock.
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

	// change state to not connected BEFORE all handlers finished
	cs.setState(NotConnectedState)
	cs.setDesiredState(NotConnectedState)

	cs.invokeHandlers(curState, NotConnectedState)
}

// ToConnecting transitions the connection state to ConnectingState.
// This transition is allowed from NotConnectedState and indicates that the connection is being established.
func (cs *ConnStateMgr) ToConnecting() error {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	curState := cs.State()
	if curState.IsConnecting() {
		return nil // Already in ConnectingState, No-op
	}

	if !curState.IsNotConnected() {
		return ErrInvalidTransition
	}
	// change state to connecting BEFORE all handlers finished
	cs.setState(ConnectingState)
	cs.setDesiredState(ConnectingState)

	cs.invokeHandlers(curState, ConnectingState)

	return nil
}

// ToNotSelected transitions the connection state to NotSelectedState.
//
// This transition is only allowed from the NotConnected/Connecting (HSMS-SS and HSMS-GS) or SelectedState (HSMS-GS only).
// If the state is already NotSelectedState, the function is a no-op.
//
// Returns nil on success, or ErrInvalidTransition if fails.
func (cs *ConnStateMgr) ToNotSelected() error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	cs.setDesiredState(NotSelectedState)

	curState := cs.State()

	if curState.IsNotSelected() {
		return nil // Already in NotSelectedState, No-op
	}

	if cs.conn != nil && cs.conn.IsSingleSession() {
		if !curState.IsNotConnected() && !curState.IsConnecting() {
			return ErrInvalidTransition
		}
	} else { // HSMS-GS
		if !curState.IsNotConnected() && !curState.IsConnecting() && !curState.IsSelected() {
			return ErrInvalidTransition
		}
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

	// change state BEFORE all handlers finished
	cs.setState(SelectedState)
	cs.setDesiredState(SelectedState)

	cs.invokeHandlers(curState, SelectedState)

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

// ToConnectingAsync transitions connection state to ConnectingState asynchronously.
//
// It will notify a goroutine and transite state in the back asynchronously.
//
// If the state is the same as the current state, the function is a no-op.
func (cs *ConnStateMgr) ToConnectingAsync() {
	cs.changeStateAsync(ConnectingState)
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

// setDesiredState atomically set desired state to the newState. It also broadcasts a signal to any waiting goroutines.
func (cs *ConnStateMgr) setDesiredState(newState ConnState) {
	cs.desiredState.Store(uint32(newState))
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
	cs.stateMu.Lock()
	defer cs.stateMu.Unlock()

	if cs.State() == state {
		return
	}

	cs.logger.Debug("put state change to stateChangeChan channel",
		"method", "changeStateAsync",
		"curState", cs.State(),
		"desiredState", state,
		"shutdowned", cs.shutdowned.Load(),
	)

	cs.stateChangeChan <- state
}

// asyncStateChangeTask handles state changing in the background.
func (cs *ConnStateMgr) asyncStateChangeTask() {
	defer func() {
		cs.logger.Debug("asyncStateChangeTask finished")
		cs.wg.Done()
	}()

loop:
	for {
		select {
		case <-cs.ctx.Done():
			break loop

		case desiredState := <-cs.stateChangeChan:
			cs.processAsyncStateChange(desiredState, false)
		}
	}

	cs.logger.Debug("asyncStateChangeTask receive ctx done, start drain asyncStateChange channel")
	// drain the asyncStateChange channel
	for {
		select {
		case desiredState, ok := <-cs.stateChangeChan:
			if !ok {
				cs.logger.Debug("asyncStateChangeTask receive channel closed, exit")
				return
			}
			cs.logger.Debug("drain asyncStateChange channel",
				"method", "asyncStateChangeTask",
				"curState", cs.State(),
				"desiredState", desiredState.String(),
			)
			cs.processAsyncStateChange(desiredState, true)
		default:
			cs.logger.Debug("no data, drain asyncStateChange channel finished")
			return
		}
	}
}

func (cs *ConnStateMgr) processAsyncStateChange(desiredState ConnState, shutdown bool) {
	cs.setDesiredState(desiredState)

	prevState := cs.State()

	cs.logger.Debug("[start] async connection state",
		"method", "asyncStateChangeTask",
		"prevState", prevState, "curState", cs.State(), "desiredState", desiredState,
	)
	if desiredState == prevState {
		cs.logger.Debug("same state, exit", "method", "asyncStateChangeTask", "state", desiredState)
		return
	}

	var err error
	switch desiredState {
	case NotConnectedState:
		cs.ToNotConnected()
	case ConnectingState:
		err = cs.ToConnecting()
	case NotSelectedState:
		err = cs.ToNotSelected()
	case SelectedState:
		err = cs.ToSelected()
	}

	if err != nil {
		cs.logger.Error("[failed] async connection state",
			"method", "asyncStateChangeTask",
			"prevState", prevState, "curState", cs.State(), "desiredState", desiredState,
			"shutdown", shutdown,
			"error", err,
		)
		if !shutdown && errors.Is(err, ErrInvalidTransition) {
			cs.logger.Debug("invalid transition, set state to not connected",
				"method", "asyncStateChangeTask",
				"prevState", prevState, "curState", cs.State(), "desiredState", desiredState,
			)
			cs.stateChangeChan <- NotConnectedState
		}
	}

	cs.logger.Debug("[end] async connection state",
		"method", "asyncStateChangeTask",
		"prevState", prevState, "curState", cs.State(), "desiredState", desiredState,
	)
}
