package hsms

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAtomicOpState_String(t *testing.T) {
	tests := []struct {
		name          string
		initialState  OpState
		expectedState string
	}{
		{
			name:          "ClosedState",
			initialState:  ClosedState,
			expectedState: "Closed",
		},
		{
			name:          "ClosingState",
			initialState:  ClosingState,
			expectedState: "Closing",
		},
		{
			name:          "OpeningState",
			initialState:  OpeningState,
			expectedState: "Opening",
		},
		{
			name:          "OpenedState",
			initialState:  OpenedState,
			expectedState: "Opened",
		},
		{
			name:          "UnknownState",
			initialState:  OpState(99),
			expectedState: "Unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &AtomicOpState{}
			st.Set(tt.initialState)
			assert.Equal(t, tt.expectedState, st.String())
		})
	}
}

func TestAtomicOpState_Get(t *testing.T) {
	tests := []struct {
		name          string
		initialState  OpState
		expectedState OpState
	}{
		{
			name:          "ClosedState",
			initialState:  ClosedState,
			expectedState: ClosedState,
		},
		{
			name:          "ClosingState",
			initialState:  ClosingState,
			expectedState: ClosingState,
		},
		{
			name:          "OpeningState",
			initialState:  OpeningState,
			expectedState: OpeningState,
		},
		{
			name:          "OpenedState",
			initialState:  OpenedState,
			expectedState: OpenedState,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &AtomicOpState{}
			st.Set(tt.initialState)
			assert.Equal(t, tt.expectedState, st.Get())
		})
	}
}

func TestAtomicOpState_Set(t *testing.T) {
	tests := []struct {
		name          string
		initialState  OpState
		newState      OpState
		expectedState OpState
	}{
		{
			name:          "ClosedToOpening",
			initialState:  ClosedState,
			newState:      OpeningState,
			expectedState: OpeningState,
		},
		{
			name:          "OpenedToClosing",
			initialState:  OpenedState,
			newState:      ClosingState,
			expectedState: ClosingState,
		},
		{
			name:          "ClosingToClosed",
			initialState:  ClosingState,
			newState:      ClosedState,
			expectedState: ClosedState,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &AtomicOpState{}
			st.Set(tt.initialState)
			st.Set(tt.newState)
			assert.Equal(t, tt.expectedState, st.Get())
		})
	}
}

func TestAtomicOpState_IsStates(t *testing.T) {
	tests := []struct {
		name         string
		initialState OpState
		isClosed     bool
		isClosing    bool
		isOpening    bool
		isOpened     bool
	}{
		{
			name:         "ClosedState",
			initialState: ClosedState,
			isClosed:     true,
			isClosing:    false,
			isOpening:    false,
			isOpened:     false,
		},
		{
			name:         "ClosingState",
			initialState: ClosingState,
			isClosed:     false,
			isClosing:    true,
			isOpening:    false,
			isOpened:     false,
		},
		{
			name:         "OpeningState",
			initialState: OpeningState,
			isClosed:     false,
			isClosing:    false,
			isOpening:    true,
			isOpened:     false,
		},
		{
			name:         "OpenedState",
			initialState: OpenedState,
			isClosed:     false,
			isClosing:    false,
			isOpening:    false,
			isOpened:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &AtomicOpState{}
			st.Set(tt.initialState)
			assert.Equal(t, tt.isClosed, st.IsClosed())
			assert.Equal(t, tt.isClosing, st.IsClosing())
			assert.Equal(t, tt.isOpening, st.IsOpening())
			assert.Equal(t, tt.isOpened, st.IsOpened())
		})
	}
}

func TestAtomicOpState_ToOpening(t *testing.T) {
	t.Run("FromClosed", func(t *testing.T) {
		st := &AtomicOpState{}
		st.Set(ClosedState)
		assert.True(t, st.ToOpening())
		assert.Equal(t, OpeningState, st.Get())
	})

	t.Run("FromOpened", func(t *testing.T) {
		st := &AtomicOpState{}
		st.Set(OpenedState)
		assert.False(t, st.ToOpening())
		assert.Equal(t, OpenedState, st.Get())
	})
}

func TestAtomicOpState_ToOpened(t *testing.T) {
	t.Run("FromOpening", func(t *testing.T) {
		st := &AtomicOpState{}
		st.Set(OpeningState)
		assert.True(t, st.ToOpened())
		assert.Equal(t, OpenedState, st.Get())
	})

	t.Run("FromOpened", func(t *testing.T) {
		st := &AtomicOpState{}
		st.Set(OpenedState)
		assert.True(t, st.ToOpened())
		assert.Equal(t, OpenedState, st.Get())
	})

	t.Run("FromClosed", func(t *testing.T) {
		st := &AtomicOpState{}
		st.Set(ClosedState)
		assert.False(t, st.ToOpened())
		assert.Equal(t, ClosedState, st.Get())
	})
}

func TestAtomicOpState_ToClosing(t *testing.T) {
	t.Run("FromOpened", func(t *testing.T) {
		st := &AtomicOpState{}
		st.Set(OpenedState)
		assert.True(t, st.ToClosing())
		assert.Equal(t, ClosingState, st.Get())
	})

	t.Run("FromOpening", func(t *testing.T) {
		st := &AtomicOpState{}
		st.Set(OpeningState)
		assert.True(t, st.ToClosing())
		assert.Equal(t, ClosingState, st.Get())
	})

	t.Run("FromClosed", func(t *testing.T) {
		st := &AtomicOpState{}
		st.Set(ClosedState)
		assert.False(t, st.ToClosing())
		assert.Equal(t, ClosedState, st.Get())
	})
}

func TestAtomicOpState_ToClosed(t *testing.T) {
	t.Run("FromClosing", func(t *testing.T) {
		st := &AtomicOpState{}
		st.Set(ClosingState)
		assert.True(t, st.ToClosed())
		assert.Equal(t, ClosedState, st.Get())
	})

	t.Run("FromClosed", func(t *testing.T) {
		st := &AtomicOpState{}
		st.Set(ClosedState)
		assert.True(t, st.ToClosed())
		assert.Equal(t, ClosedState, st.Get())
	})

	t.Run("FromOpened", func(t *testing.T) {
		st := &AtomicOpState{}
		st.Set(OpenedState)
		assert.False(t, st.ToClosed())
		assert.Equal(t, OpenedState, st.Get())
	})
}
