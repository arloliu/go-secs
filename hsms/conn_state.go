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
// Two invocation models exist:
//
//   - Synchronous handlers, registered via [ConnStateMgr.AddHandler], run inline
//     under the state manager's mutex as part of the transition. They are used
//     by the library internally for invariant-preserving bookkeeping (e.g.
//     starting the sender/receiver tasks on NotSelected, closing the connection
//     on NotConnected) and must be short, non-blocking, and must not call any
//     synchronous state-change method (doing so re-enters the mutex and
//     deadlocks).
//
//   - Asynchronous handlers, registered via [ConnStateMgr.AddAsyncHandler] and
//     exposed to library users through Session.AddConnStateChangeHandler, are
//     dispatched on a dedicated goroutine after the transition is complete.
//     They may perform blocking work — including SendDataMessage with W-bit
//     set — without deadlocking, and may call any state-change method. The
//     trade-off is that the connection state observed via cs.State() may have
//     moved past the (prevState, newState) the handler was notified of by the
//     time it runs; handlers should act on the arguments they receive rather
//     than on the current live state.
//
// The handler function receives the connection and the previous/new states.
type ConnStateChangeHandler func(conn Connection, prevState ConnState, newState ConnState)

// asyncStateChangeEvent is a (prev, new) pair published to asyncHandlerChan and
// consumed by the asyncHandlerDispatcher goroutine.
type asyncStateChangeEvent struct {
	prev ConnState
	next ConnState
}

// asyncHandlerChanBuffer is the buffer size of the channel feeding the
// asynchronous user-handler dispatcher. State changes are infrequent in
// practice; this buffer absorbs any realistic burst.
const asyncHandlerChanBuffer = 64

// ConnStateMgr manages the connection state of an HSMS connection.
//
// It provides methods for managing state transitions and notifying listeners of state changes.
// The state transitions are thread safety in concurrent environments.
type ConnStateMgr struct {
	mu               sync.Mutex
	stateMu          sync.Mutex
	pctx             context.Context
	ctx              context.Context
	cancel           context.CancelFunc
	wg               sync.WaitGroup
	shutdowned       atomic.Bool
	cond             *sync.Cond
	state            atomic.Uint32
	desiredState     atomic.Uint32
	conn             Connection
	logger           logger.Logger
	stateChangeChan  chan ConnState
	handlers         []ConnStateChangeHandler // synchronous: invoked inline under mu
	asyncHandlers    []ConnStateChangeHandler // asynchronous: dispatched on asyncHandlerDispatcher
	asyncHandlerChan chan asyncStateChangeEvent
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
	cs.shutdowned.Store(true)
	cs.cond = sync.NewCond(&cs.mu)

	return cs
}

func (cs *ConnStateMgr) Start() {
	cs.stateMu.Lock()
	defer cs.stateMu.Unlock()

	// Already running — nothing to do. This prevents a data race when
	// Open() is called twice without an intervening Close(): the second
	// call would otherwise overwrite ctx/cancel/stateChangeChan while the
	// first asyncStateChangeTask goroutine is still running.
	if cs.ctx != nil && !cs.shutdowned.Load() {
		cs.logger.Debug("connection state manager already started, skipping")
		return
	}

	cs.logger.Debug("start connection state manager")

	cctx, cancel := context.WithCancel(cs.pctx)
	cs.ctx = cctx
	cs.cancel = cancel
	cs.stateChangeChan = make(chan ConnState, 10)
	cs.asyncHandlerChan = make(chan asyncStateChangeEvent, asyncHandlerChanBuffer)

	cs.logger.Debug("set shutdowned flag to false")
	cs.shutdowned.Store(false)

	cs.wg.Add(2)
	go cs.asyncStateChangeTask()
	go cs.asyncHandlerDispatcher()
}

func (cs *ConnStateMgr) Stop() {
	if !cs.shutdowned.CompareAndSwap(false, true) {
		cs.logger.Debug("conn state manager already shutdowned or not started, ignore stop",
			"method", "Stop",
			"curState", cs.State(),
			"desiredState", cs.DesiredState(),
			"shutdowned", cs.shutdowned.Load(),
		)

		return
	}

	cs.logger.Debug("stop connection state manager")

	// Snapshot cancel under stateMu so we don't race with Start() writing it.
	// Release stateMu BEFORE calling cancel — holding it across cancel()+wg.Wait()
	// would deadlock with changeStateAsync, which locks stateMu to send on the channel.
	cs.stateMu.Lock()
	cancel := cs.cancel
	cs.stateMu.Unlock()

	cancel()

	cs.wg.Wait()

	cs.stateMu.Lock()
	cs.logger.Debug("close stateChangeChan", "shutdowned", true)
	close(cs.stateChangeChan)
	// asyncHandlerChan is NOT closed here: late invokeHandlers calls (e.g. the
	// fallback ToNotConnected() in Close()) still use publishAsyncEvent, which
	// checks shutdowned and becomes a no-op. Closing the channel would force a
	// send-on-closed panic on those paths.

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

// AddHandler adds one or more synchronous ConnStateChangeHandler functions to
// be invoked on state changes.
//
// Handlers registered here run inline under the state manager's mutex as part
// of the transition, before the transition method returns. This invocation
// model is intended for library-internal, invariant-preserving bookkeeping
// (starting goroutines on NotSelected, closing the connection on
// NotConnected, etc.). Library users should prefer [ConnStateMgr.AddAsyncHandler]
// or the session-level AddConnStateChangeHandler, which are safer.
//
// Guidelines:
//   - Keep handlers short and non-blocking.
//   - Never call the synchronous state-change methods (ToX); they re-enter the
//     same mutex and deadlock. Use the *Async variants.
func (cs *ConnStateMgr) AddHandler(handlers ...ConnStateChangeHandler) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	cs.handlers = append(cs.handlers, handlers...)
}

// AddAsyncHandler adds one or more asynchronous ConnStateChangeHandler
// functions. Handlers registered here are dispatched on a dedicated goroutine
// after the transition is complete, so they may perform blocking work (I/O,
// SendDataMessage with the W-bit set, synchronous state-change calls) without
// deadlocking the state machine or the receive path.
//
// Ordering: handlers are invoked in FIFO order relative to transitions, and in
// registration order relative to each other.
//
// Trade-off: the value returned by cs.State() may have moved past the
// (prev, new) pair the handler was notified of by the time the handler runs.
// Handlers should act on the arguments they receive rather than inspect the
// live state.
//
// Delivery: the dispatcher uses a buffered channel. In the extremely unlikely
// case of overflow (a slow handler and a burst of transitions), the newest
// event is dropped with a warning log.
func (cs *ConnStateMgr) AddAsyncHandler(handlers ...ConnStateChangeHandler) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	cs.asyncHandlers = append(cs.asyncHandlers, handlers...)
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

	cs.setDesiredState(ConnectingState)

	curState := cs.State()
	if curState.IsConnecting() {
		return nil // Already in ConnectingState, No-op
	}

	if !curState.IsNotConnected() {
		return ErrInvalidTransition
	}
	// change state to connecting before all handlers finished
	cs.setState(ConnectingState)
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

	// change state before all handlers finished
	cs.setState(NotSelectedState)
	cs.invokeHandlers(curState, NotSelectedState)

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

	cs.setDesiredState(SelectedState)

	curState := cs.State()

	if curState.IsSelected() {
		return nil // Already in SelectedState, No-op
	}

	// Only allow transition from NotSelectedState
	if !curState.IsNotSelected() {
		return ErrInvalidTransition
	}

	// change state before all handlers finished
	cs.setState(SelectedState)

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

// IsConnecting returns if the current state is connecting.
func (cs *ConnStateMgr) IsConnecting() bool {
	return cs.State().IsConnecting()
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

// invokeHandlers runs the synchronous handler list inline and publishes the
// transition to the async dispatcher. Must be called with cs.mu held.
func (cs *ConnStateMgr) invokeHandlers(prevState ConnState, newState ConnState) {
	for _, handler := range cs.handlers {
		if handler != nil {
			handler(cs.conn, prevState, newState)
		}
	}

	cs.publishAsyncEvent(prevState, newState)
}

// publishAsyncEvent hands a (prev, new) transition to the async dispatcher via
// a buffered channel using a non-blocking send. Overflow drops the newest
// event with a warning log rather than back-pressuring the state machine.
// Becomes a no-op once the state manager has been stopped.
//
// Idempotent ToX calls (e.g. ToNotConnected invoked while already in
// NotConnectedState) are suppressed here so user-facing async handlers never
// observe a prev==next pair.
func (cs *ConnStateMgr) publishAsyncEvent(prevState ConnState, newState ConnState) {
	if prevState == newState {
		return
	}

	if cs.shutdowned.Load() {
		return
	}

	if cs.asyncHandlerChan == nil {
		return
	}

	select {
	case cs.asyncHandlerChan <- asyncStateChangeEvent{prev: prevState, next: newState}:
	default:
		cs.logger.Warn("async state-change event channel full, dropping event",
			"prevState", prevState, "newState", newState,
		)
	}
}

// asyncHandlerDispatcher consumes transitions published by publishAsyncEvent
// and invokes registered async handlers serially, preserving FIFO ordering
// across transitions and registration order across handlers.
//
// On context cancel the dispatcher drains any remaining buffered events so
// that transitions published just before Stop() — e.g. the final
// ToNotConnected() synthesized by Close() — are still delivered to user
// handlers before Stop() returns.
func (cs *ConnStateMgr) asyncHandlerDispatcher() {
	defer func() {
		cs.logger.Debug("asyncHandlerDispatcher finished")
		cs.wg.Done()
	}()

loop:
	for {
		select {
		case <-cs.ctx.Done():
			break loop
		case ev := <-cs.asyncHandlerChan:
			cs.dispatchAsyncEvent(ev)
		}
	}

	// Drain remaining events with a non-blocking read. No new events can be
	// published after shutdowned is set, so this loop terminates.
	for {
		select {
		case ev := <-cs.asyncHandlerChan:
			cs.dispatchAsyncEvent(ev)
		default:
			return
		}
	}
}

// dispatchAsyncEvent snapshots the async handler slice and invokes each one
// without holding cs.mu, so handlers are free to call back into the state
// manager (including synchronous ToX methods) without re-entering the mutex.
func (cs *ConnStateMgr) dispatchAsyncEvent(ev asyncStateChangeEvent) {
	cs.mu.Lock()
	handlers := make([]ConnStateChangeHandler, len(cs.asyncHandlers))
	copy(handlers, cs.asyncHandlers)
	cs.mu.Unlock()

	for _, handler := range handlers {
		if handler != nil {
			handler(cs.conn, ev.prev, ev.next)
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

	// After Stop() closes stateChangeChan, a concurrent goroutine (e.g.
	// tryConnect → ToNotSelectedAsync) could still reach this point.
	// The shutdowned check is race-free here because Stop() sets the flag
	// before closing the channel, and both Stop() and this method serialise
	// on stateMu for the channel close / channel send.
	if cs.shutdowned.Load() {
		cs.logger.Debug("state manager shutdowned, ignoring async state change",
			"method", "changeStateAsync",
			"curState", cs.State(),
			"desiredState", state,
		)

		return
	}

	if cs.State() == state {
		return
	}

	cs.logger.Debug("put state change to stateChangeChan channel",
		"method", "changeStateAsync",
		"curState", cs.State(),
		"desiredState", state,
		"shutdowned", cs.shutdowned.Load(),
	)

	// Use select with ctx.Done() to avoid blocking indefinitely on a full
	// channel when the asyncStateChangeTask goroutine has already exited
	// (between cancel() and the channel close in Stop()).
	select {
	case cs.stateChangeChan <- state:
	case <-cs.ctx.Done():
		cs.logger.Debug("context canceled, dropping async state change",
			"method", "changeStateAsync",
			"curState", cs.State(),
			"desiredState", state,
		)
	}
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
	default:
		// No-op for unexpected states
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
			// Use select with default to avoid blocking when the channel buffer
			// is full. This goroutine is the sole reader of the channel, so a
			// blocking send here would self-deadlock.
			select {
			case cs.stateChangeChan <- NotConnectedState:
			default:
			}
		}
	}

	cs.logger.Debug("[end] async connection state",
		"method", "asyncStateChangeTask",
		"prevState", prevState, "curState", cs.State(), "desiredState", desiredState,
	)
}
