package hsms

import (
	"context"
	"io"
	"math/rand/v2"
	"sync"
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

	t.Run("ToNotSelected_ToNotConnectedInHandler", func(t *testing.T) {
		stateChangeCount := 0
		// create instance for mock HSMS-SS connection
		cs := NewConnStateMgr(ctx, &ssConn{})
		cs.AddHandler(func(_ Connection, _ ConnState, _ ConnState) { stateChangeCount++ })
		cs.AddHandler(func(_ Connection, _ ConnState, newState ConnState) {
			// simulate the target doesn't reply the select.req message,
			// so the connection state manager should transition to NotConnectedState
			// after the ToNotSelected transition is completed.
			if newState == NotSelectedState {
				cs.ToNotConnectedAsync()
			}
		})

		cs.Start()
		defer cs.Stop()

		require.NoError(cs.ToNotSelected())

		tctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		err := cs.WaitState(tctx, NotConnectedState)
		require.NoError(err, "failed to wait for NotConnectedState when ToNotSelected handler triggered ToNotConnectedAsync")

		require.Equal(NotConnectedState, cs.State())
		require.Equal(2, stateChangeCount)
		require.True(cs.IsNotConnected())
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

// TestConnStateMgr_DoubleStart verifies that calling Start() twice without
// an intervening Stop() is idempotent and does not leak goroutines or
// overwrite the channel/context from the first Start().
func TestConnStateMgr_DoubleStart(t *testing.T) {
	require := require.New(t)

	ctx := context.Background()
	cs := NewConnStateMgr(ctx, nil)

	cs.Start()

	// Transition to NotSelected so we can observe the state is preserved.
	require.NoError(cs.ToNotSelected())
	require.Equal(NotSelectedState, cs.State())

	// Second Start() should be a no-op.
	cs.Start()

	// State should still be NotSelected (not reset to NotConnected).
	require.Equal(NotSelectedState, cs.State())

	// Async transitions should still work (channel not replaced).
	cs.ToNotConnectedAsync()
	require.Eventually(func() bool {
		return cs.IsNotConnected()
	}, time.Second, time.Millisecond)

	cs.Stop()
}

// TestConnStateMgr_DoubleStartAfterStop verifies the Start→Stop→Start cycle
// works correctly, creating a fresh goroutine and channel.
func TestConnStateMgr_DoubleStartAfterStop(t *testing.T) {
	require := require.New(t)

	ctx := context.Background()
	cs := NewConnStateMgr(ctx, nil)

	// First lifecycle.
	cs.Start()
	require.NoError(cs.ToNotSelected())
	cs.Stop()
	require.Equal(NotConnectedState, cs.State())

	// Second lifecycle — must not panic or hang.
	cs.Start()
	require.NoError(cs.ToNotSelected())
	cs.ToSelectedAsync()
	require.Eventually(func() bool {
		return cs.IsSelected()
	}, time.Second, time.Millisecond)
	cs.Stop()
	require.Equal(NotConnectedState, cs.State())
}

// TestConnStateMgr_AsyncAfterStop verifies that calling an async state change
// after Stop() does not panic (send on closed channel).
func TestConnStateMgr_AsyncAfterStop(t *testing.T) {
	ctx := context.Background()
	cs := NewConnStateMgr(ctx, nil)

	cs.Start()
	require.NoError(t, cs.ToNotSelected())
	cs.Stop()

	// These must not panic.
	cs.ToNotConnectedAsync()
	cs.ToNotSelectedAsync()
	cs.ToSelectedAsync()
	cs.ToConnectingAsync()
}

// TestConnStateMgr_ConcurrentAsyncAndStop verifies that concurrent async
// state changes and Stop() do not cause panics or data races.
func TestConnStateMgr_ConcurrentAsyncAndStop(t *testing.T) {
	ctx := context.Background()

	for range 100 {
		cs := NewConnStateMgr(ctx, nil)
		cs.Start()
		_ = cs.ToNotSelected()

		// Fire many async transitions concurrently with Stop.
		done := make(chan struct{})
		go func() {
			defer close(done)
			for range 20 {
				cs.ToNotConnectedAsync()
				cs.ToNotSelectedAsync()
				cs.ToSelectedAsync()
			}
		}()

		// Stop while the goroutine is firing.
		cs.Stop()
		<-done
	}
}

// ---------------------------------------------------------------------------
// Chaos / deadlock-focused tests for ConnStateMgr lifecycle
//
// Each test targets a specific deadlock or race vector:
//   - Stop vs changeStateAsync holding stateMu (the original deadlock)
//   - Concurrent Open/Close (Start/Stop) from multiple goroutines
//   - Rapid Start→Stop→Start cycling
//   - Async transitions interleaved with handler-triggered async transitions
//   - Stop racing with a channel-full condition (backpressure)
// ---------------------------------------------------------------------------

// TestConnStateMgr_Chaos_StopVsAsyncFlood is the most direct regression test
// for the original deadlock: Stop() must never block indefinitely when
// changeStateAsync() goroutines are in-flight holding stateMu while waiting
// on a full channel or ctx.Done().
//
// The test enforces a hard 5-second deadline per iteration; any deadlock
// makes it fail immediately rather than hanging for the full test timeout.
func TestConnStateMgr_Chaos_StopVsAsyncFlood(t *testing.T) {
	const iterations = 200
	const asyncGoroutines = 8
	const opsPerGoroutine = 50

	ctx := context.Background()

	for i := range iterations {
		func() {
			deadline := time.AfterFunc(5*time.Second, func() {
				// If we get here, we are deadlocked.
				panic("TestConnStateMgr_Chaos_StopVsAsyncFlood: deadlock detected on iteration " + string(rune('0'+i%10)))
			})
			defer deadline.Stop()

			cs := NewConnStateMgr(ctx, nil)
			cs.Start()
			// Put into NotSelected so all 4 async transitions are reachable.
			_ = cs.ToNotSelected()

			var wg sync.WaitGroup

			// Flood with async state changes from multiple goroutines.
			for range asyncGoroutines {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for range opsPerGoroutine {
						switch rand.IntN(4) {
						case 0:
							cs.ToNotConnectedAsync()
						case 1:
							cs.ToConnectingAsync()
						case 2:
							cs.ToNotSelectedAsync()
						case 3:
							cs.ToSelectedAsync()
						default:
						}
					}
				}()
			}

			// Race Stop against the flood.
			cs.Stop()
			wg.Wait()
		}()
	}
}

// TestConnStateMgr_Chaos_ConcurrentStartStop tests concurrent Stop() calls
// after a single Start(). Multiple goroutines race to Stop the same manager
// while others fire async transitions — this is the pattern that occurs when
// multiple error paths trigger close simultaneously. Only ONE Stop must win
// the CAS; the rest must be no-ops.
func TestConnStateMgr_Chaos_ConcurrentStartStop(t *testing.T) {
	const iterations = 200
	const stoppers = 10

	ctx := context.Background()

	for range iterations {
		func() {
			deadline := time.AfterFunc(5*time.Second, func() {
				panic("TestConnStateMgr_Chaos_ConcurrentStartStop: deadlock detected")
			})
			defer deadline.Stop()

			cs := NewConnStateMgr(ctx, nil)
			cs.Start()
			_ = cs.ToNotSelected()

			var wg sync.WaitGroup
			// Half the goroutines try Stop, half fire async transitions.
			for range stoppers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					cs.ToNotConnectedAsync()
					cs.Stop()
				}()
			}
			wg.Wait()

			if !cs.IsNotConnected() {
				t.Fatalf("expected NotConnected after Stop, got %v", cs.State())
			}
		}()
	}
}

// TestConnStateMgr_Chaos_RapidLifecycle rapidly cycles Start→Stop many times
// on a single goroutine, interleaving async transitions at each step. This
// catches use-after-close on the channel and ctx, and ensures wg.Wait()
// never hangs.
func TestConnStateMgr_Chaos_RapidLifecycle(t *testing.T) {
	ctx := context.Background()
	cs := NewConnStateMgr(ctx, nil)

	for range 500 {
		cs.Start()
		_ = cs.ToNotSelected()

		// Fire async transitions that the asyncStateChangeTask must
		// either process or drain before Stop returns.
		cs.ToSelectedAsync()
		cs.ToNotConnectedAsync()
		cs.ToNotSelectedAsync()

		cs.Stop()

		// Post-Stop invariants.
		if cs.State() != NotConnectedState {
			t.Fatalf("state after Stop = %v, want NotConnected", cs.State())
		}
	}
}

// TestConnStateMgr_Chaos_ChannelBackpressure fills the stateChangeChan to
// capacity and then calls Stop(). This is the exact scenario that caused the
// original deadlock: the async reader (processAsyncStateChange) enqueues a
// recovery NotConnectedState, the channel is full, and it blocks — while
// changeStateAsync on another goroutine also blocks holding stateMu, and
// Stop() needs stateMu to proceed.
func TestConnStateMgr_Chaos_ChannelBackpressure(t *testing.T) {
	const iterations = 300

	ctx := context.Background()

	for range iterations {
		func() {
			deadline := time.AfterFunc(5*time.Second, func() {
				panic("TestConnStateMgr_Chaos_ChannelBackpressure: deadlock detected")
			})
			defer deadline.Stop()

			cs := NewConnStateMgr(ctx, nil)
			cs.Start()
			_ = cs.ToNotSelected()

			// Saturate the channel (capacity 10) from the test goroutine
			// before any reader can drain it.
			for range 20 {
				cs.ToSelectedAsync()
				cs.ToNotConnectedAsync()
			}

			// Stop must return even though the channel may be full and
			// processAsyncStateChange may be blocked on a recovery enqueue.
			cs.Stop()
		}()
	}
}

// TestConnStateMgr_Chaos_HandlerTriggersAsync verifies that a state change
// handler calling an async transition during Stop's drain phase does not
// deadlock. This tests the mu ↔ stateMu ordering when handlers invoke
// changeStateAsync indirectly.
func TestConnStateMgr_Chaos_HandlerTriggersAsync(t *testing.T) {
	const iterations = 200

	ctx := context.Background()

	for range iterations {
		func() {
			deadline := time.AfterFunc(5*time.Second, func() {
				panic("TestConnStateMgr_Chaos_HandlerTriggersAsync: deadlock detected")
			})
			defer deadline.Stop()

			cs := NewConnStateMgr(ctx, nil)
			cs.AddHandler(func(_ Connection, _ ConnState, newState ConnState) {
				// When we transition to NotSelected, immediately ask
				// to go to Selected (simulating a select.req response).
				if newState == NotSelectedState {
					cs.ToSelectedAsync()
				}
				// When we transition to Selected, bounce back.
				if newState == SelectedState {
					cs.ToNotConnectedAsync()
				}
			})

			cs.Start()
			_ = cs.ToNotSelected()
			// Let the handler chain bounce a few times.
			time.Sleep(time.Millisecond)
			cs.Stop()
		}()
	}
}

// TestConnStateMgr_Chaos_StopBeforeStart verifies that Stop() on a
// never-started manager is a safe no-op (shutdowned starts true).
func TestConnStateMgr_Chaos_StopBeforeStart(t *testing.T) {
	ctx := context.Background()

	for range 100 {
		cs := NewConnStateMgr(ctx, nil)
		// Must not panic or hang.
		cs.Stop()
		cs.Stop()
		// Async calls must not panic.
		cs.ToNotSelectedAsync()
		cs.ToSelectedAsync()
	}
}

// TestConnStateMgr_Chaos_FuzzOps randomly interleaves all public operations
// from multiple goroutines after a single Start(). This is the broadest net
// — it catches any unforeseen lock ordering, nil pointer, or channel state
// issue. Start() is NOT included because concurrent Start() calls race on
// ctx/cancel/channel fields (Start/Stop form a lifecycle pair and are only
// called from a single connection goroutine in production).
func TestConnStateMgr_Chaos_FuzzOps(t *testing.T) {
	const goroutines = 6
	const opsPerGoroutine = 200

	ctx := context.Background()

	for range 50 {
		func() {
			deadline := time.AfterFunc(10*time.Second, func() {
				panic("TestConnStateMgr_Chaos_FuzzOps: deadlock detected")
			})
			defer deadline.Stop()

			cs := NewConnStateMgr(ctx, &ssConn{})
			cs.Start()
			_ = cs.ToNotSelected() // put it in a useful starting state

			var wg sync.WaitGroup
			for range goroutines {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for range opsPerGoroutine {
						switch rand.IntN(11) {
						case 0:
							cs.Stop()
						case 1:
							_ = cs.ToConnecting()
						case 2:
							_ = cs.ToNotSelected()
						case 3:
							_ = cs.ToSelected()
						case 4:
							cs.ToNotConnected()
						case 5:
							cs.ToNotConnectedAsync()
						case 6:
							cs.ToConnectingAsync()
						case 7:
							cs.ToNotSelectedAsync()
						case 8:
							cs.ToSelectedAsync()
						case 9:
							_ = cs.State()
							_ = cs.DesiredState()
						case 10:
							tctx, cancel := context.WithTimeout(ctx, time.Millisecond)
							_ = cs.WaitState(tctx, ConnState(rand.IntN(4)))
							cancel()
						default:
						}
					}
				}()
			}
			wg.Wait()

			// Ensure we can cleanly stop regardless of what state we're in.
			cs.Stop()
		}()
	}
}

type ssConn struct{}

var _ Connection = (*ssConn)(nil)

func (c *ssConn) Open(waitOpened bool) error          { return nil }
func (c *ssConn) Close() error                        { return nil }
func (c *ssConn) AddSession(sessionID uint16) Session { return nil }
func (c *ssConn) IsSingleSession() bool               { return true }
func (c *ssConn) IsGeneralSession() bool              { return false }
func (c *ssConn) IsSECS1() bool                       { return false }
func (c *ssConn) GetLogger() logger.Logger            { return &mockLogger{} }
func (c *ssConn) ConnState() ConnState                { return NotConnectedState }
func (c *ssConn) OpState() OpState                    { return ClosedState }

type gsConn struct{}

var _ Connection = (*gsConn)(nil)

func (c *gsConn) Open(waitOpened bool) error          { return nil }
func (c *gsConn) Close() error                        { return nil }
func (c *gsConn) AddSession(sessionID uint16) Session { return nil }
func (c *gsConn) IsSingleSession() bool               { return false }
func (c *gsConn) IsGeneralSession() bool              { return true }
func (c *gsConn) IsSECS1() bool                       { return false }
func (c *gsConn) GetLogger() logger.Logger            { return &mockLogger{} }
func (c *gsConn) ConnState() ConnState                { return NotConnectedState }
func (c *gsConn) OpState() OpState                    { return ClosedState }

type mockLogger struct{}

var _ logger.Logger = (*mockLogger)(nil)

func (l *mockLogger) Debug(msg string, keysAndValues ...any) {}
func (l *mockLogger) Info(msg string, keysAndValues ...any)  {}
func (l *mockLogger) Warn(msg string, keysAndValues ...any)  {}
func (l *mockLogger) Error(msg string, keysAndValues ...any) {}
func (l *mockLogger) Fatal(msg string, keysAndValues ...any) {}
func (l *mockLogger) With(keyValues ...any) logger.Logger    { return &mockLogger{} }
func (l *mockLogger) Level() logger.LogLevel                 { return logger.InfoLevel }
func (l *mockLogger) SetLevel(level logger.LogLevel)         {}
func (l *mockLogger) SetOutput(output io.Writer)             {}
