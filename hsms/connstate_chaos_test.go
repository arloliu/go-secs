package hsms

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// This file covers the P0.1 chaos scenarios from tmp/chaos-test-plan.md that
// were not yet present in conn_state_test.go:
//
//   - TestConnStateChaos_AsyncHandlerPanics      — dispatcher must survive a
//     panicking user handler; sibling handlers must keep receiving events.
//   - TestConnStateChaos_StopWhileAsyncInFlight  — Stop() must wait for an
//     in-flight async handler to complete and return within bounded time.
//   - TestConnStateChaos_RegisterHandlerDuringDispatch — AddAsyncHandler
//     racing with active dispatch must not race, double-deliver, or skip.
//
// Each scenario already covered by an existing _test.go test is intentionally
// not duplicated here (StopVsAsyncFlood, OverflowDoesNotBlock, CanCallSyncToX,
// FuzzOps already exist in conn_state_test.go).

// TestConnStateChaos_AsyncHandlerPanics asserts that a panicking async handler
// does not take down the dispatcher goroutine. Without a recover() in
// dispatchAsyncEvent the panic would propagate up through asyncHandlerDispatcher
// and crash the test binary; a sibling handler registered after the panicker
// must still receive every subsequent transition.
func TestConnStateChaos_AsyncHandlerPanics(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()

	cs := NewConnStateMgr(ctx, &ssConn{})

	cs.AddAsyncHandler(func(_ Connection, _, _ ConnState) {
		panic("intentional panic from chaos test handler")
	})

	var survivorCount atomic.Int64
	cs.AddAsyncHandler(func(_ Connection, _, _ ConnState) {
		survivorCount.Add(1)
	})

	cs.Start()

	// Fire four real transitions through the full lifecycle.
	require.NoError(cs.ToConnecting())
	require.NoError(cs.ToNotSelected())
	require.NoError(cs.ToSelected())
	cs.ToNotConnected()

	// The survivor must observe all four events even though the sibling
	// handler panics on each invocation.
	require.Eventually(func() bool {
		return survivorCount.Load() >= 4
	}, 2*time.Second, 5*time.Millisecond,
		"survivor handler must receive events when sibling panics; got %d", survivorCount.Load())

	// Stop must still terminate cleanly.
	stopDone := make(chan struct{})
	go func() {
		cs.Stop()
		close(stopDone)
	}()
	select {
	case <-stopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() did not return after panicking handler")
	}

	require.Equal(NotConnectedState, cs.State())
}

// TestConnStateChaos_StopWhileAsyncInFlight verifies that Stop() returns
// within a bounded time when an async handler is mid-execution. The handler
// is released shortly after Stop() is invoked; Stop() must join the
// dispatcher cleanly (wg.Wait) without abandoning the in-flight handler.
func TestConnStateChaos_StopWhileAsyncInFlight(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()

	cs := NewConnStateMgr(ctx, &ssConn{})

	release := make(chan struct{})
	started := make(chan struct{})
	var startOnce sync.Once
	var handlerExited atomic.Bool

	cs.AddAsyncHandler(func(_ Connection, _, _ ConnState) {
		startOnce.Do(func() { close(started) })
		<-release
		handlerExited.Store(true)
	})

	cs.Start()
	require.NoError(cs.ToConnecting())

	// Wait for the handler to actually be in-flight on the dispatcher.
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never entered")
	}

	// Invoke Stop concurrently; release the handler shortly after so Stop's
	// wg.Wait must observe the dispatcher draining and finishing.
	stopDone := make(chan struct{})
	go func() {
		cs.Stop()
		close(stopDone)
	}()

	time.Sleep(20 * time.Millisecond)
	close(release)

	select {
	case <-stopDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop() did not return after handler released")
	}

	require.True(handlerExited.Load(), "handler must have completed before Stop returned")
	require.Equal(NotConnectedState, cs.State())
}

// TestConnStateChaos_RegisterHandlerDuringDispatch races AddAsyncHandler /
// AddHandler calls against an active transition flood. The invariants:
//
//   - no panic / data race (run under -race),
//   - a long-lived stable handler observes events throughout,
//   - the dispatcher continues delivering events to the stable handler after
//     the registration storm subsides.
//
// We do NOT assert anything about whether newly registered handlers receive
// any specific event — registration timing relative to publish/dispatch is
// inherently racy and the contract only guarantees no-replay, not delivery
// for transitions that may already be in the dispatcher buffer.
func TestConnStateChaos_RegisterHandlerDuringDispatch(t *testing.T) {
	require := require.New(t)
	const iterations = 30
	const concurrentRegistrants = 6
	const handlersPerRegistrant = 4

	ctx := context.Background()

	for iter := range iterations {
		func() {
			cs := NewConnStateMgr(ctx, &ssConn{})

			var stableCount atomic.Int64
			cs.AddAsyncHandler(func(_ Connection, _, _ ConnState) {
				stableCount.Add(1)
			})

			cs.Start()

			done := make(chan struct{})
			var wg sync.WaitGroup

			// Transition driver — keep firing real transitions to publish events.
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-done:
						return
					default:
						_ = cs.ToConnecting()
						_ = cs.ToNotSelected()
						_ = cs.ToSelected()
						cs.ToNotConnected()
					}
				}
			}()

			// Registration storm — race AddAsyncHandler / AddHandler against dispatch.
			for range concurrentRegistrants {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for range handlersPerRegistrant {
						cs.AddAsyncHandler(func(_ Connection, _, _ ConnState) {})
						cs.AddHandler(func(_ Connection, _, _ ConnState) {})
					}
				}()
			}

			time.Sleep(15 * time.Millisecond)
			close(done)
			wg.Wait()

			require.Eventually(func() bool {
				return stableCount.Load() > 0
			}, 2*time.Second, 1*time.Millisecond,
				"stable handler must observe events on iter %d", iter)

			cs.Stop()
		}()
	}
}
