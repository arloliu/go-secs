package hsmsss

import "sync/atomic"

type OpState uint32

const (
	ClosedState OpState = iota
	ClosingState
	OpeningState
	OpenedState
)

type AtomicOpState struct {
	state atomic.Uint32
}

func (st *AtomicOpState) String() string {
	switch st.Get() {
	case ClosedState:
		return "Closed"
	case ClosingState:
		return "Closing"
	case OpeningState:
		return "Opening"
	case OpenedState:
		return "Opened"
	default:
		return "Unknown"
	}
}

// Get returns the current state of the AtomicOpState.
func (st *AtomicOpState) Get() OpState {
	return OpState(st.state.Load())
}

// Set sets the state of the AtomicOpState to the given state.
func (st *AtomicOpState) Set(state OpState) {
	st.state.Store(uint32(state))
}

func (st *AtomicOpState) IsClosed() bool {
	return st.Get() == ClosedState
}

func (st *AtomicOpState) IsClosing() bool {
	return st.Get() == ClosingState
}

func (st *AtomicOpState) IsOpening() bool {
	return st.Get() == OpeningState
}

func (st *AtomicOpState) IsOpened() bool {
	return st.Get() == OpenedState
}

func (st *AtomicOpState) ToOpening() bool {
	return st.state.CompareAndSwap(uint32(ClosedState), uint32(OpeningState))
}

func (st *AtomicOpState) ToOpened() bool {
	if st.IsOpened() {
		return true
	}

	return st.state.CompareAndSwap(uint32(OpeningState), uint32(OpenedState))
}

func (st *AtomicOpState) ToClosing() bool {
	result := st.state.CompareAndSwap(uint32(OpenedState), uint32(ClosingState))
	if !result {
		return st.state.CompareAndSwap(uint32(OpeningState), uint32(ClosingState))
	}

	return result
}

func (st *AtomicOpState) ToClosed() bool {
	if st.IsClosed() {
		return true
	}

	return st.state.CompareAndSwap(uint32(ClosingState), uint32(ClosedState))
}
