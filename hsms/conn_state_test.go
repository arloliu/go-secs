package hsms

import (
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/go-secs/logger"
	"github.com/stretchr/testify/require"
)

func TestConnStateTransitions(t *testing.T) {
	require := require.New(t)

	ctx := context.Background()

	t.Run("Initial State", func(t *testing.T) {
		cs := NewConnStateMgr(ctx, nil)
		require.Equal(NotConnectedState, cs.State())
	})

	t.Run("ToNotSelected", func(t *testing.T) {
		stateChangeCount := 0
		// create instance for mock HSMS-SS connection
		cs := NewConnStateMgr(ctx, &ssConn{})
		cs.AddHandler(func(conn Connection, prevState ConnState, newState ConnState) { stateChangeCount++ })
		cs.Start()
		defer cs.Stop()

		require.NoError(cs.ToConnecting())
		require.Equal(ConnectingState, cs.State())
		require.Equal(1, stateChangeCount)
		require.True(cs.IsConnecting())

		require.NoError(cs.ToNotSelected())
		require.Equal(NotSelectedState, cs.State())
		require.Equal(2, stateChangeCount)
		require.True(cs.IsNotSelected())

		// No-op transition when already in NotSelectedState
		require.NoError(cs.ToNotSelected())
		require.Equal(2, stateChangeCount)

		// Transition to SelectedState
		require.NoError(cs.ToSelected())
		require.Equal(3, stateChangeCount)
		// Invalid transition from SelectedState to NotSelectedState
		require.ErrorIs(cs.ToNotSelected(), ErrInvalidTransition)

		stateChangeCount = 0
		// create instance for mock HSMS-GS connection
		cs = NewConnStateMgr(ctx, &gsConn{})
		cs.AddHandler(func(conn Connection, prevState ConnState, newState ConnState) { stateChangeCount++ })

		// No-op transition when already in NotSelectedState
		require.NoError(cs.ToNotSelected())
		require.Equal(1, stateChangeCount)

		// Transition to SelectedState
		require.NoError(cs.ToSelected())
		require.Equal(2, stateChangeCount)

		// Accept from SelectedState to NotSelectedState
		require.NoError(cs.ToNotSelected())
		require.Equal(NotSelectedState, cs.State())
		require.Equal(3, stateChangeCount)
	})

	t.Run("ToSelected", func(t *testing.T) {
		stateChangeCount := 0
		cs := NewConnStateMgr(ctx, nil)
		cs.AddHandler(func(conn Connection, prevState ConnState, newState ConnState) { stateChangeCount++ })
		cs.Start()
		defer cs.Stop()

		// Invalid transition from NotConnectedState to SelectedState
		require.ErrorIs(cs.ToSelected(), ErrInvalidTransition)
		require.Equal(0, stateChangeCount)

		require.NoError(cs.ToNotSelected()) // Transition to NotSelectedState
		require.Equal(1, stateChangeCount)

		require.NoError(cs.ToSelected())
		require.Equal(SelectedState, cs.State())
		require.Equal(2, stateChangeCount)
		require.True(cs.IsSelected())

		// No-op transition when already in SelectedState
		require.NoError(cs.ToSelected())
		require.Equal(2, stateChangeCount)
	})

	t.Run("ToNotConnected", func(t *testing.T) {
		stateChangeCount := 0
		cs := NewConnStateMgr(ctx, nil)
		cs.AddHandler(func(conn Connection, prevState ConnState, newState ConnState) { stateChangeCount++ })
		cs.Start()
		defer cs.Stop()

		require.NoError(cs.ToNotSelected()) // Transition to NotSelectedState
		require.Equal(1, stateChangeCount)
		require.NoError(cs.ToSelected()) // Transition to SelectedState
		require.Equal(2, stateChangeCount)

		cs.ToNotConnected()
		require.Equal(NotConnectedState, cs.State())
		require.Equal(3, stateChangeCount)
		require.True(cs.IsNotConnected())

		// ToNotConnectedState allows multiple transitions
		cs.ToNotConnected()
		require.Equal(4, stateChangeCount)
	})

	t.Run("setState", func(t *testing.T) {
		cs := NewConnStateMgr(ctx, nil)
		cs.Start()
		defer cs.Stop()

		cs.setState(NotConnectedState)
		require.Equal(NotConnectedState, cs.State())
		cs.setState(NotSelectedState)
		require.Equal(NotSelectedState, cs.State())
		cs.setState(SelectedState)
		require.Equal(SelectedState, cs.State())
	})
}

func TestConnStateAsyncTransitions(t *testing.T) {
	require := require.New(t)

	ctx := context.Background()

	t.Run("ToNotConnectedAsync", func(t *testing.T) {
		stateChangeCount := 0
		cs := NewConnStateMgr(ctx, nil)
		cs.AddHandler(func(conn Connection, prevState ConnState, newState ConnState) { stateChangeCount++ })
		cs.Start()
		defer cs.Stop()

		cs.ToNotConnectedAsync()
		time.Sleep(10 * time.Millisecond) // allow async transition to complete
		require.Equal(NotConnectedState, cs.State())
		require.Equal(0, stateChangeCount) // no state change event for NotConnectedState
		require.True(cs.IsNotConnected())

		// No-op transition when already in NotConnectedState
		cs.ToNotConnectedAsync()
		time.Sleep(10 * time.Millisecond)
		require.Equal(0, stateChangeCount)
	})

	t.Run("ToNotSelectedAsync", func(t *testing.T) {
		var stateChangeCount atomic.Int32
		cs := NewConnStateMgr(ctx, nil)
		cs.AddHandler(func(conn Connection, prevState ConnState, newState ConnState) { stateChangeCount.Add(1) })
		cs.Start()
		defer cs.Stop()

		cs.ToNotSelectedAsync()
		require.Eventually(func() bool {
			return cs.IsNotSelected() && stateChangeCount.Load() == int32(1)
		}, time.Second, 1*time.Millisecond)

		// No-op transition when already in NotSelectedState
		cs.ToNotSelectedAsync()
		time.Sleep(10 * time.Millisecond)
		require.Equal(int32(1), stateChangeCount.Load())
	})

	t.Run("ToSelectedAsync", func(t *testing.T) {
		var stateChangeCount atomic.Int32

		cs := NewConnStateMgr(ctx, nil)
		cs.AddHandler(func(conn Connection, prevState ConnState, newState ConnState) { stateChangeCount.Add(1) })
		cs.Start()
		defer cs.Stop()

		err := cs.ToNotSelected()
		require.NoError(err)
		require.Eventually(func() bool {
			return cs.IsNotSelected() && stateChangeCount.Load() == int32(1)
		}, time.Second, 1*time.Millisecond)

		cs.ToSelectedAsync()
		time.Sleep(10 * time.Millisecond) // allow async transition to complete

		require.Eventually(func() bool {
			return cs.IsSelected() && stateChangeCount.Load() == int32(2)
		}, time.Second, 1*time.Millisecond)

		// No-op transition when already in SelectedState
		cs.ToSelectedAsync()
		time.Sleep(10 * time.Millisecond)
		require.Equal(int32(2), stateChangeCount.Load())
	})
}

func TestWaitConnState(t *testing.T) {
	require := require.New(t)

	cs := NewConnStateMgr(context.Background(), nil)

	go func() {
		time.Sleep(10 * time.Millisecond)
		err := cs.ToNotSelected()
		require.NoError(err)
	}()

	begin := time.Now()
	ctx, cancel := context.WithTimeout(context.TODO(), 100*time.Millisecond)
	defer cancel()

	err := cs.WaitState(ctx, NotSelectedState)
	require.NoError(err)

	// wait ConnectedState again
	err = cs.WaitState(ctx, NotSelectedState)
	require.NoError(err)

	err = cs.WaitState(ctx, SelectedState)
	require.ErrorIs(err, context.DeadlineExceeded)
	require.WithinDuration(begin.Add(100*time.Millisecond), time.Now(), 20*time.Millisecond)
}

type ssConn struct{}

var _ Connection = (*ssConn)(nil)

func (_ *ssConn) Open(waitOpened bool) error          { return nil }
func (_ *ssConn) Close() error                        { return nil }
func (_ *ssConn) AddSession(sessionID uint16) Session { return nil }
func (_ *ssConn) IsSingleSession() bool               { return true }
func (_ *ssConn) IsGeneralSession() bool              { return false }
func (_ *ssConn) GetLogger() logger.Logger            { return &mockLogger{} }
func (_ *ssConn) ConnState() ConnState                { return NotConnectedState }
func (_ *ssConn) OpState() OpState                    { return ClosedState }

type gsConn struct{}

var _ Connection = (*gsConn)(nil)

func (_ *gsConn) Open(waitOpened bool) error          { return nil }
func (_ *gsConn) Close() error                        { return nil }
func (_ *gsConn) AddSession(sessionID uint16) Session { return nil }
func (_ *gsConn) IsSingleSession() bool               { return false }
func (_ *gsConn) IsGeneralSession() bool              { return true }
func (_ *gsConn) GetLogger() logger.Logger            { return &mockLogger{} }
func (_ *gsConn) ConnState() ConnState                { return NotConnectedState }
func (_ *gsConn) OpState() OpState                    { return ClosedState }

type mockLogger struct{}

var _ logger.Logger = (*mockLogger)(nil)

func (_ *mockLogger) Debug(msg string, keysAndValues ...any) {}
func (_ *mockLogger) Info(msg string, keysAndValues ...any)  {}
func (_ *mockLogger) Warn(msg string, keysAndValues ...any)  {}
func (_ *mockLogger) Error(msg string, keysAndValues ...any) {}
func (_ *mockLogger) Fatal(msg string, keysAndValues ...any) {}
func (_ *mockLogger) With(keyValues ...any) logger.Logger    { return &mockLogger{} }
func (_ *mockLogger) Level() logger.LogLevel                 { return logger.InfoLevel }
func (_ *mockLogger) SetLevel(level logger.LogLevel)         {}
func (_ *mockLogger) SetOutput(output io.Writer)             {}
