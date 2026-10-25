package tracepack

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// txParWatchdog bounds every wait of the parallel protocol tests, and txParWorkers is the workers they run on.
const (
	txParWatchdog = 10 * time.Second
	txParWorkers  = 4
)

// txParHour returns a source of one hour holding the n primaries of seqs 10 to 10+n-1, and their range.
func txParHour(t *testing.T, n int) (PackSource, TxRange) {
	t.Helper()

	recs := make([]Record, n)
	for i := range recs {
		recs[i] = txRecord(10+uint64(i), nil)
	}

	return txSource(t, txPack(t, seg0, nil, txBlock(recs...))).view(), txBatchHour(0)
}

// txParMark returns the result the protocol tests' lookups give for the i-th key: one searched scope, of hour i.
func txParMark(i int) TxResult {
	return TxResult{Searched: []TxScope{{Hour: int64(i)}}}
}

// txParOut is what a FindTransactions call of the parallel protocol tests did:
// its result and error, the indices of the pairings passed, in order, and its batch, set before anything is read.
type txParOut struct {
	res    TxRangeResult
	passed []int
	batch  *txBatch
	err    error
}

// txParRun runs FindTransactions with ctx over an hour of n primaries on txParWorkers workers, each lookup replaced by lookup,
// into out, which it returns; fn, when set, is called with the index of each pairing passed.
// Each pairing passed must be the k-th key with lookup's result for it; one that is not ends the call with an error.
// A call that does not return within txParWatchdog ends the test binary.
// out.batch is set before anything is read, so that a test whose call panics still has it.
func txParRun(t *testing.T, ctx context.Context, out *txParOut, n int,
	lookup func(ctx context.Context, i int, key TxKey) (TxResult, error), fn func(k int) error,
) *txParOut {
	t.Helper()

	// A protocol that blocks would hang the test until the binary's own timeout: the watchdog ends the binary sooner, saying why.
	watchdog := time.AfterFunc(txParWatchdog, func() {
		panic(fmt.Sprintf("%s: FindTransactions did not return within %v", t.Name(), txParWatchdog))
	})
	defer watchdog.Stop()
	src, r := txParHour(t, n)
	out.res, out.err = findTransactionsWith(ctx, src, r, TxRangeOptions{Lookup: TxOptions{MaxScopes: 1}}, func(p *TxPairing) error {
		k := len(out.passed)
		want := TxKey{Capture: captureLow, Seq: 10 + uint64(k), Hour: memTestHour}
		if !assert.Equal(t, want, p.Key, "pairing %d", k) || !assert.Equal(t, txParMark(k), p.Result, "pairing %d", k) {
			return fmt.Errorf("pairing %d is not key %v with its result", k, want)
		}
		out.passed = append(out.passed, k)
		if fn != nil {
			return fn(k)
		}

		return nil
	}, func(b *txBatch) {
		out.batch = b
		b.workers = txParWorkers
		b.parallelLookup = lookup
	})

	return out
}

// requireParEnd requires out to have passed the pairings of indices 0 to passed-1 and counted them,
// over one hour read, and every worker to have ended.
func requireParEnd(t *testing.T, out *txParOut, passed int) {
	t.Helper()

	want := make([]int, passed)
	for i := range want {
		want[i] = i
	}
	if passed == 0 {
		assert.Empty(t, out.passed, "no pairing passed")
	} else {
		assert.Equal(t, want, out.passed, "the pairings passed")
	}
	assert.Equal(t, TxRangeResult{HoursRead: 1, Primaries: passed}, out.res)
	require.NotNil(t, out.batch)
	assert.Zero(t, out.batch.liveWorkers.Load(), "no worker left running")
}

// await waits on a worker for c to be closed or to yield a value, or for ctx to end, whose error it returns then.
// A worker never fails its test itself: a lookup that cannot go on returns an error the test then sees.
func await[T any](ctx context.Context, c <-chan T) error {
	select {
	case <-c:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// recv waits on the test's goroutine for a value on c, failing the test past the watchdog.
func recv[T any](t *testing.T, c <-chan T, what string) T {
	t.Helper()

	select {
	case v := <-c:
		return v
	case <-time.After(txParWatchdog):
		t.Fatalf("timed out waiting for %s", what)
	}

	var zero T

	return zero
}

// With 4 workers, the lookup of key 4 does not start while key 0 is still running: each worker runs its next lookup
// only once the pairing before it in its slot is passed; once key 0 ends, every key is passed in order.
func TestFindTransactionsParallelWindow(t *testing.T) {
	t.Parallel()

	started := make(chan int, 12)
	release := make(chan struct{})
	done := make(chan struct{})
	var out txParOut
	go func() {
		defer close(done)
		txParRun(t, t.Context(), &out, 12, func(ctx context.Context, i int, _ TxKey) (TxResult, error) {
			started <- i
			if i == 0 {
				if err := await(ctx, release); err != nil {
					return TxResult{}, err
				}
			}

			return txParMark(i), nil
		}, nil)
	}()

	first := map[int]bool{}
	for range 4 {
		first[recv(t, started, "the first lookups")] = true
	}
	assert.Equal(t, map[int]bool{0: true, 1: true, 2: true, 3: true}, first, "the lookups started while key 0 runs")
	select {
	case i := <-started:
		t.Fatalf("the lookup of key %d started while key 0 runs", i)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	recv(t, done, "the batch")
	require.NoError(t, out.err)
	requireParEnd(t, &out, 12)
}

// Every slot is reused, a partial last round included: the keys are passed in order whatever the order the lookups end in.
func TestFindTransactionsParallelWraparound(t *testing.T) {
	t.Parallel()

	for _, n := range []int{2*txParWorkers + 1, 3*txParWorkers - 1, 2, txParWorkers} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			t.Parallel()

			out := txParRun(t, t.Context(), &txParOut{}, n, func(_ context.Context, i int, _ TxKey) (TxResult, error) {
				// Later keys of a round end first.
				time.Sleep(time.Duration((n-i)%txParWorkers) * time.Millisecond)

				return txParMark(i), nil
			}, nil)
			require.NoError(t, out.err)
			requireParEnd(t, out, n)
		})
	}
}

// A lookup failing after a later key's lookup already failed is the error the batch returns, after the keys before it:
// the results are taken in key order.
// A later failure alone ends the batch after every key before it.
func TestFindTransactionsParallelErrorOrder(t *testing.T) {
	t.Parallel()

	t.Run("an earlier failure ending after a later one", func(t *testing.T) {
		t.Parallel()

		release := make(chan struct{})
		failed3 := make(chan struct{})
		var once sync.Once
		out := txParRun(t, t.Context(), &txParOut{}, 8, func(ctx context.Context, i int, _ TxKey) (TxResult, error) {
			switch i {
			case 1:
				if err := await(ctx, failed3); err != nil {
					return TxResult{}, err
				}
				if err := await(ctx, release); err != nil {
					return TxResult{}, err
				}

				return TxResult{}, errors.New("key 1 fails")
			case 3:
				defer once.Do(func() {
					close(failed3)
					close(release)
				})
				return TxResult{}, errors.New("key 3 fails")
			default:
				return txParMark(i), nil
			}
		}, nil)
		require.EqualError(t, out.err, fmt.Sprintf("tracepack: find transactions: primary seq 11 of hour %d: key 1 fails", memTestHour))
		requireParEnd(t, out, 1)
	})

	t.Run("a later failure alone", func(t *testing.T) {
		t.Parallel()

		out := txParRun(t, t.Context(), &txParOut{}, 8, func(_ context.Context, i int, _ TxKey) (TxResult, error) {
			if i == 3 {
				return TxResult{}, ErrReadLimit
			}

			return txParMark(i), nil
		}, nil)
		require.ErrorIs(t, out.err, ErrReadLimit)
		assert.Equal(t, fmt.Sprintf("tracepack: find transactions: primary seq 13 of hour %d: %v", memTestHour, ErrReadLimit), out.err.Error())
		requireParEnd(t, out, 3)
	})
}

// Cancelled while the batch waits for the head key's lookup, the batch returns ctx's error, wrapped by the batch or by that key,
// passing nothing,
// and every worker, running or waiting for its turn, has ended.
func TestFindTransactionsParallelCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started := make(chan int, 8)
	go func() {
		for range 4 {
			<-started
		}
		cancel()
	}()
	out := txParRun(t, ctx, &txParOut{}, 8, func(ctx context.Context, i int, _ TxKey) (TxResult, error) {
		started <- i
		// Every lookup runs until ctx ends.
		<-ctx.Done()

		return TxResult{}, fmt.Errorf("tracepack: find transaction: %w", ctx.Err())
	}, nil)
	require.ErrorIs(t, out.err, context.Canceled)
	// The batch's own check sees the cancellation, or the head key's lookup, ready at the same time, reports it first.
	assert.Contains(t, []string{
		"tracepack: find transactions: context canceled",
		fmt.Sprintf("tracepack: find transactions: primary seq 10 of hour %d: tracepack: find transaction: context canceled", memTestHour),
	}, out.err.Error())
	requireParEnd(t, out, 0)
}

// fn's error and fn's panic end the batch while other lookups run and a worker waits for its turn:
// the error is returned as is, the panic raised again, and every worker has ended.
func TestFindTransactionsParallelFnEnds(t *testing.T) {
	t.Parallel()

	boom := errors.New("boom")
	// lookup ends keys 0 and 1 at once, signals each later key's start and runs it until ctx ends.
	lookup := func(running chan<- int) func(ctx context.Context, i int, _ TxKey) (TxResult, error) {
		return func(ctx context.Context, i int, _ TxKey) (TxResult, error) {
			if i < 2 {
				return txParMark(i), nil
			}
			running <- i
			<-ctx.Done()

			return TxResult{}, ctx.Err()
		}
	}

	t.Run("an error", func(t *testing.T) {
		t.Parallel()

		running := make(chan int, 12)
		out := txParRun(t, t.Context(), &txParOut{}, 12, lookup(running), func(k int) error {
			if k < 1 {
				return nil
			}
			// Keys 2 and 3 are running, and the worker of key 1 waits for its next turn.
			recv(t, running, "key 2 or 3")
			recv(t, running, "key 3 or 2")

			return boom
		})
		require.Equal(t, boom, out.err, "fn's error as is")
		requireParEnd(t, out, 2)
	})

	t.Run("a panic", func(t *testing.T) {
		t.Parallel()

		running := make(chan int, 12)
		var out txParOut
		assert.PanicsWithValue(t, "fn panics", func() {
			txParRun(t, t.Context(), &out, 12, lookup(running), func(k int) error {
				if k < 1 {
					return nil
				}
				recv(t, running, "key 2 or 3")
				recv(t, running, "key 3 or 2")
				panic("fn panics")
			})
		})
		assert.Equal(t, []int{0, 1}, out.passed)
		require.NotNil(t, out.batch)
		assert.Zero(t, out.batch.liveWorkers.Load())
	})
}

// A lookup that panics, once the keys before it are passed, panics on the calling goroutine with the same value,
// while a later lookup runs; every worker has ended.
func TestFindTransactionsParallelLookupPanic(t *testing.T) {
	t.Parallel()

	running := make(chan int, 8)
	var out txParOut
	assert.PanicsWithValue(t, "key 2 panics", func() {
		txParRun(t, t.Context(), &out, 8, func(ctx context.Context, i int, _ TxKey) (TxResult, error) {
			switch {
			case i == 2:
				// Key 3 is running when key 2 panics.
				if err := await(ctx, running); err != nil {
					return TxResult{}, err
				}
				panic("key 2 panics")
			case i >= 3:
				running <- i
				<-ctx.Done()

				return TxResult{}, ctx.Err()
			default:
				return txParMark(i), nil
			}
		}, nil)
	})
	assert.Equal(t, []int{0, 1}, out.passed)
	require.NotNil(t, out.batch)
	assert.Zero(t, out.batch.liveWorkers.Load())
}

// Under cancellation the calling goroutine's checks decide what is passed:
// a lookup's error ready together with the cancellation is one of the two errors, nothing passed;
// fn cancelling ctx and returning its own error has its error returned as is;
// a worker seeing the cancellation while fn runs for an earlier key changes nothing about that pairing,
// and the batch's own next check ends it with ctx's error.
func TestFindTransactionsParallelCancelPrecedence(t *testing.T) {
	t.Parallel()

	t.Run("a lookup's error and the cancellation both ready", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		out := txParRun(t, ctx, &txParOut{}, 8, func(_ context.Context, i int, _ TxKey) (TxResult, error) {
			if i == 0 {
				cancel()

				return TxResult{}, errors.New("key 0 fails")
			}

			return txParMark(i), nil
		}, nil)
		assert.Contains(t, []string{
			"tracepack: find transactions: context canceled",
			fmt.Sprintf("tracepack: find transactions: primary seq 10 of hour %d: key 0 fails", memTestHour),
		}, errText(out.err))
		requireParEnd(t, out, 0)
	})

	t.Run("fn cancels and returns its own error", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		boom := errors.New("boom")
		out := txParRun(t, ctx, &txParOut{}, 8, func(_ context.Context, i int, _ TxKey) (TxResult, error) {
			return txParMark(i), nil
		}, func(k int) error {
			if k == 2 {
				cancel()

				return boom
			}

			return nil
		})
		require.Equal(t, boom, out.err, "fn's error as is")
		requireParEnd(t, out, 3)
	})

	t.Run("a worker sees the cancellation while fn runs", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		started, seen := make(chan struct{}), make(chan struct{})
		out := txParRun(t, ctx, &txParOut{}, 8, func(ctx context.Context, i int, _ TxKey) (TxResult, error) {
			if i == 0 {
				return txParMark(i), nil
			}
			if i == 1 {
				close(started)
			}
			<-ctx.Done()
			if i == 1 {
				close(seen)
			}

			return TxResult{}, ctx.Err()
		}, func(int) error {
			// The pairing of key 0 passed the batch's check; key 1's worker, running, sees the cancellation before fn returns.
			recv(t, started, "key 1's lookup")
			cancel()
			recv(t, seen, "key 1's worker seeing the cancellation")

			return nil
		})
		require.ErrorIs(t, out.err, context.Canceled)
		assert.Equal(t, "tracepack: find transactions: context canceled", out.err.Error(), "the batch's own check")
		requireParEnd(t, out, 1)
	})
}

// txArmedCtx is a context whose Err reports cancellation once on is set,
// and which closes seen at the first call of Err after armed is set.
// Its Done never closes, so only Err tells of the cancellation; the calling goroutine of a batch is the only caller of Err.
type txArmedCtx struct {
	context.Context
	armed, on atomic.Bool
	seen      chan struct{}
	once      sync.Once
}

func (c *txArmedCtx) Err() error {
	if c.on.Load() {
		return context.Canceled
	}
	if c.armed.Load() {
		c.once.Do(func() { close(c.seen) })
	}

	return nil
}

// Cancelled while the calling goroutine waits for a lookup's result, after its check before that result,
// the batch does not pass the pairing: its check before each pairing sees the cancellation.
func TestFindTransactionsParallelCancelWhileWaiting(t *testing.T) {
	t.Parallel()

	ctx := &txArmedCtx{Context: t.Context(), seen: make(chan struct{})}
	out := txParRun(t, ctx, &txParOut{}, 8, func(lctx context.Context, i int, _ TxKey) (TxResult, error) {
		if i == 1 {
			// The calling goroutine has checked ctx before key 1's result: the cancellation comes while it waits.
			if err := await(lctx, ctx.seen); err != nil {
				return TxResult{}, err
			}
			ctx.on.Store(true)
		}

		return txParMark(i), nil
	}, func(int) error {
		ctx.armed.Store(true)

		return nil
	})
	require.ErrorIs(t, out.err, context.Canceled)
	assert.Equal(t, "tracepack: find transactions: context canceled", out.err.Error())
	requireParEnd(t, out, 1)
}
