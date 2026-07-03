package hsms

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/logger"
	"github.com/stretchr/testify/require"
)

func TestEpoch_SpawnRunsWithCtx(t *testing.T) {
	e := newEpoch(t.Context(), logger.Default(), 8)
	var wg sync.WaitGroup
	wg.Add(1)
	// Capture facts ABOUT the ctx rather than the ctx value itself (avoids fatcontext).
	var gotNonNil bool
	var gotErr error
	ok := e.spawn(logger.Default(), "probe", func(ctx context.Context) {
		defer wg.Done()
		if ctx != nil {
			gotNonNil = true
			gotErr = ctx.Err()
		}
	})
	require.True(t, ok)
	wg.Wait()
	require.True(t, gotNonNil, "spawn must pass a non-nil ctx to fn")
	require.NoError(t, gotErr, "ctx live while epoch live")
}

func TestEpoch_SetConnLiveConn(t *testing.T) {
	e := newEpoch(t.Context(), logger.Default(), 8)
	require.Nil(t, e.liveConn(), "no socket before setConn")

	c1, c2 := net.Pipe()
	t.Cleanup(func() { _ = c1.Close(); _ = c2.Close() })

	e.setConn(c1)
	require.Same(t, c1, e.liveConn(), "liveConn returns the published socket")
}

func TestEpoch_SpawnAfterClosingIsNoOp(t *testing.T) {
	e := newEpoch(t.Context(), logger.Default(), 8)
	e.spawnMu.Lock()
	e.closing = true
	e.spawnMu.Unlock()
	ran := atomic.Bool{}
	ok := e.spawn(logger.Default(), "late", func(ctx context.Context) { ran.Store(true) })
	require.False(t, ok, "spawn after closing must be a no-op")
	require.False(t, ran.Load(), "no-op spawn must not run fn")
}

func TestEpoch_SpawnPanicIsContained(t *testing.T) {
	e := newEpoch(t.Context(), logger.Default(), 8)
	done := make(chan struct{})
	ok := e.spawn(logger.Default(), "boom", func(ctx context.Context) {
		defer close(done)
		panic("should be recovered, not crash the process")
	})
	require.True(t, ok)
	<-done
	e.wg.Wait() // recover + wg.Done must let Wait return
}

func TestEpoch_SpawnRacesWaitNoMisuse(t *testing.T) {
	// -race -count=200: many goroutines hammer spawn() while another seals closing then Waits.
	// A correct guard => every spawn after closing is a no-op, so Wait never sees a 0->1 Add.
	// Reintroducing the bug (Add before the closing-check) trips the race detector or panics
	// "WaitGroup is reused before previous Wait has returned".
	for range 50 {
		e := newEpoch(t.Context(), logger.Default(), 8)
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() { _ = e.spawn(logger.Default(), "racer", func(ctx context.Context) {}) })
		}
		// Concurrently seal + join.
		e.spawnMu.Lock()
		e.closing = true
		e.spawnMu.Unlock()
		e.wg.Wait() // must not race a 0->1 Add
		wg.Wait()
	}
}

func TestEpoch_TeardownJoinsSpawnedTasks(t *testing.T) {
	e := newEpoch(t.Context(), logger.Default(), 8)
	started := make(chan struct{})
	e.spawn(logger.Default(), "worker", func(ctx context.Context) {
		close(started)
		<-ctx.Done() // exits when teardown cancels
	})
	<-started
	e.teardown(2 * time.Second)  // non-blocking init
	require.NoError(t, e.wait()) // wait observes the result
	e.teardown(2 * time.Second)  // idempotent — closeOnce no-op
	require.NoError(t, e.wait()) // same result
}

func TestEpoch_TeardownBoundedOnStuckTask(t *testing.T) {
	e := newEpoch(t.Context(), logger.Default(), 8)
	block := make(chan struct{})
	e.spawn(logger.Default(), "stuck", func(ctx context.Context) { <-block }) // ignores ctx → never exits
	e.teardown(100 * time.Millisecond)
	require.ErrorIs(t, e.wait(), ErrCloseTimeout, "a stuck task must yield a bounded close-timeout, not a hang")
	close(block)
}

func TestEpoch_TeardownInitReturnsBeforeJoinCompletes(t *testing.T) {
	// teardown() must return PROMPTLY (it only initiates); the join runs on a separate
	// goroutine. A stuck task must NOT make teardown() itself block (only wait() blocks).
	e := newEpoch(t.Context(), logger.Default(), 8)
	block := make(chan struct{})
	e.spawn(logger.Default(), "stuck", func(ctx context.Context) { <-block })
	start := time.Now()
	e.teardown(2 * time.Second)
	require.Less(t, time.Since(start), 200*time.Millisecond, "teardown() is the non-blocking initiator (§5.3)")
	close(block)
	require.NoError(t, e.wait())
}

func TestEpoch_TeardownClosesSocketBeforeJoin(t *testing.T) {
	// A goroutine parked in conn.Read must be unblocked by teardown's closeSocket
	// (J5) so the join completes. Use a net.Pipe conn.
	srv, cli := net.Pipe()
	e := newEpoch(t.Context(), logger.Default(), 8)
	e.setConn(cli)
	reading := make(chan struct{})
	e.spawn(logger.Default(), "reader", func(ctx context.Context) {
		close(reading)
		buf := make([]byte, 1)
		_, _ = cli.Read(buf) // parks until closeSocket() closes cli
	})
	<-reading
	e.teardown(2 * time.Second)
	require.NoError(t, e.wait()) // must NOT time out — closeSocket unblocked the reader
	_ = srv.Close()
}
