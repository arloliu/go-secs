//go:build stress

package hsms

import (
	"context"
	"math/rand/v2"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestConnStateChaos_PropertyOpMix runs a randomized operation mix against a
// single ConnStateMgr to flush out unexpected lock-ordering or channel-state
// issues that scripted tests miss. The op pool is broader than FuzzOps in
// conn_state_test.go and includes async-handler registration during the run.
//
// Property: regardless of operation interleaving, after a final Stop() the
// manager must be in NotConnectedState, no panic must occur, and the
// goroutine count must settle to within a small tolerance of pre-test.
//
// Gated behind //go:build stress because the matrix is broad and the run is
// long under -race.
func TestConnStateChaos_PropertyOpMix(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping property-style chaos in short mode")
	}

	const iterations = 50
	const workers = 8
	const opsPerWorker = 300

	ctx := context.Background()

	for iter := range iterations {
		func() {
			before := runtime.NumGoroutine()

			cs := NewConnStateMgr(ctx, &ssConn{})

			// Pre-seed a stable async handler so the dispatcher always has work.
			cs.AddAsyncHandler(func(_ Connection, _, _ ConnState) {})

			cs.Start()
			_ = cs.ToNotSelected()

			var wg sync.WaitGroup
			for range workers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for range opsPerWorker {
						switch rand.IntN(12) {
						case 0:
							_ = cs.ToConnecting()
						case 1:
							_ = cs.ToNotSelected()
						case 2:
							_ = cs.ToSelected()
						case 3:
							cs.ToNotConnected()
						case 4:
							cs.ToNotConnectedAsync()
						case 5:
							cs.ToConnectingAsync()
						case 6:
							cs.ToNotSelectedAsync()
						case 7:
							cs.ToSelectedAsync()
						case 8:
							_ = cs.State()
							_ = cs.DesiredState()
						case 9:
							tctx, cancel := context.WithTimeout(ctx, time.Millisecond)
							_ = cs.WaitState(tctx, ConnState(rand.IntN(4)))
							cancel()
						case 10:
							cs.AddAsyncHandler(func(_ Connection, _, _ ConnState) {})
						case 11:
							cs.AddHandler(func(_ Connection, _, _ ConnState) {})
						}
					}
				}()
			}
			wg.Wait()

			cs.Stop()

			require.Equal(t, NotConnectedState, cs.State(),
				"iter %d: state after final Stop must be NotConnected", iter)

			// Coarse goroutine settle — allow scheduler to drain.
			require.Eventually(t, func() bool {
				return runtime.NumGoroutine() <= before+2
			}, 2*time.Second, 10*time.Millisecond,
				"iter %d: goroutines did not settle (before=%d, now=%d)",
				iter, before, runtime.NumGoroutine())
		}()
	}
}
