package tracepack

import (
	"context"
	"fmt"
	"io"
	"slices"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gateKey names one ReadAt call of a prefetch test: the index of its reader and its offset.
type gateKey struct {
	reader int
	off    int64
}

// readGate counts the ReadAt calls of the readers of one read and, while armed, holds the calls it selects
// until the test releases them, so a test under testing/synctest can count the calls in flight at a quiescent point.
type readGate struct {
	mu sync.Mutex
	// armed is set once the readers are open: Open's own calls pass uncounted.
	armed bool
	// hold selects the calls the gate holds; nil holds every call.
	hold func(k gateKey) bool
	// fail selects the calls that fail with errInjected, after the gate releases them.
	fail func(k gateKey) bool
	// release is closed to release the calls held.
	release chan struct{}
	// inflight is the calls started and not returned, held or not; peak is its high-water mark.
	inflight, peak int
	// calls counts the calls of each reader and offset.
	calls map[gateKey]int
	// returned is set once the read returned; late counts the calls started after it.
	returned bool
	late     int
}

// gatedAt is the io.ReaderAt of one reader of a prefetch test, over data, through a shared readGate.
type gatedAt struct {
	data   []byte
	reader int
	g      *readGate
}

var _ io.ReaderAt = (*gatedAt)(nil)

// newReadGate returns an unarmed readGate.
func newReadGate() *readGate {
	return &readGate{release: make(chan struct{}), calls: map[gateKey]int{}}
}

// ReadAt serves data from off, counting the call and holding it at the gate when the gate selects it.
func (a *gatedAt) ReadAt(p []byte, off int64) (int, error) {
	g, k := a.g, gateKey{reader: a.reader, off: off}
	g.mu.Lock()
	armed := g.armed
	var release chan struct{}
	if armed {
		g.calls[k]++
		g.inflight++
		g.peak = max(g.peak, g.inflight)
		if g.returned {
			g.late++
		}
		if g.hold == nil || g.hold(k) {
			release = g.release
		}
	}
	g.mu.Unlock()
	if !armed {
		return copyAt(a.data, p, off)
	}

	if release != nil {
		<-release
	}
	g.mu.Lock()
	g.inflight--
	failed := g.fail != nil && g.fail(k)
	g.mu.Unlock()
	if failed {
		return 0, errInjected
	}

	return copyAt(a.data, p, off)
}

// copyAt copies data from off into p as io.ReaderAt does.
func copyAt(data, p []byte, off int64) (int, error) {
	if off >= int64(len(data)) {
		return 0, io.EOF
	}
	n := copy(p, data[off:])
	if n < len(p) {
		return n, io.EOF
	}

	return n, nil
}

// arm starts counting and holding calls.
func (g *readGate) arm() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.armed = true
}

// open releases the calls held.
func (g *readGate) open() {
	g.mu.Lock()
	defer g.mu.Unlock()
	close(g.release)
	g.release = make(chan struct{})
}

// snapshot returns the calls in flight and their high-water mark.
func (g *readGate) snapshot() (int, int) {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.inflight, g.peak
}

// openBehindGate opens each file through a gatedAt over g with opts, the gate unarmed.
func openBehindGate(t testing.TB, g *readGate, opts ReaderOptions, files ...[]byte) []*Reader {
	t.Helper()

	readers := make([]*Reader, len(files))
	for i, f := range files {
		r, err := Open(t.Context(), &gatedAt{data: f, reader: i, g: g}, int64(len(f)), opts)
		require.NoError(t, err)
		readers[i] = r
	}

	return readers
}

// gatedOutcome is what a read run by runGated returned.
type gatedOutcome struct {
	items []iterItem
	res   Result
	err   error
	// held is the loader's held bytes once the run returned.
	held int64
	// peak is the most ReadAt calls in flight at once.
	peak int
}

// runGated runs the planned read of q over readers in opts.Order under testing/synctest, on a goroutine of its own,
// releasing the calls g holds each time every goroutine of the bubble is blocked, until the read returns.
// The gate is armed after the plan is built, so only the run's calls are counted.
// It must be called inside a synctest bubble.
func runGated(t *testing.T, ctx context.Context, g *readGate, readers []*Reader, q Query, opts MergeIterateOptions,
	noPrefetch bool, fn func(*Item) error,
) gatedOutcome {
	t.Helper()

	var out gatedOutcome
	yield := func(it *Item) error {
		out.items = append(out.items, copyItem(it))
		if fn != nil {
			return fn(it)
		}

		return nil
	}
	o, err := checkMergeIterate(ctx, readers, &q, opts, yield)
	require.NoError(t, err)
	p := newMergeIteratePlan(o)
	p.noPrefetch = noPrefetch
	require.NoError(t, p.build(ctx, readers, &q))
	g.arm()

	done := make(chan struct{})
	go func() {
		defer close(done)
		if o.Order == OrderCapture {
			out.err = p.runCapture(ctx, &q, yield)
		} else {
			out.err = p.runTime(ctx, &q, yield)
		}
		g.mu.Lock()
		g.returned = true
		g.mu.Unlock()
	}()
	for {
		synctest.Wait()
		select {
		case <-done:
			_, out.peak = g.snapshot()
			out.res, out.held = p.res, p.loader.budget.held

			return out
		default:
			g.open()
		}
	}
}

// nonePack writes the records of steps, in blocks closed where steps say, into a finalized pack id of capture, codec None.
// The prefetch tests that run under testing/synctest read None packs:
// a pooled zstd decoder made outside the bubble cannot be used inside it.
func nonePack(t testing.TB, id, capture UUID, steps []footerTestStep) []byte {
	t.Helper()

	return withIDs(t, writeRepairPack(t, CodecNone, nil, false, steps).file, id, capture)
}

// prefetchSteps returns n one-block clusters of one capture, seqs 2k+1 and 2k+2 at ts_min 10k, so ascending in time.
func prefetchSteps(n int) []footerTestStep {
	steps := make([]footerTestStep, 0, 2*n)
	for k := range n {
		steps = append(steps, blockAt(int64(10*k), uint64(2*k+1), uint64(2*k+2))...)
	}

	return steps
}

// TestMergeIteratePrefetchOverlaps reads packs whose blocks lie outside every byte Open retains,
// behind a gate that holds every ReadAt until all goroutines are blocked:
// in time order, and in capture order over one capture, several block reads are in flight at once,
// while a capture-order read of two captures reads one block at a time, as the read without prefetch does.
func TestMergeIteratePrefetchOverlaps(t *testing.T) {
	t.Parallel()

	one := nonePack(t, seg0, captureLow, prefetchSteps(6))
	other := nonePack(t, seg1, captureHigh, prefetchSteps(6))
	tests := []struct {
		name  string
		order Order
		files [][]byte
		peak  int
	}{
		{name: "time order", order: OrderTime, files: [][]byte{one, other}, peak: 8},
		{name: "capture order over one capture", order: OrderCapture, files: [][]byte{one}, peak: 6},
		{name: "capture order over two captures", order: OrderCapture, files: [][]byte{one, other}, peak: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
				g := newReadGate()
				readers := openBehindGate(t, g, uncachedWindows(), tt.files...)
				out := runGated(t, t.Context(), g, readers, Query{}, MergeIterateOptions{Order: tt.order}, false, nil)
				require.NoError(t, out.err)
				assert.Len(t, out.items, 12*len(tt.files))
				assert.Equal(t, tt.peak, out.peak, "the most block reads in flight at once")
				assert.Zero(t, out.held)
			})
		})
	}
}

// prefetchOutcome is what prefetchRead read.
type prefetchOutcome struct {
	items []iterItem
	res   Result
	err   error
	// peak is the most bytes the loader's budget held at once.
	peak int64
	// queued and inflight are the prefetch queue's length and its speculative reads in flight once the run returned;
	// both 0 without prefetch.
	queued   int
	inflight int32
}

// prefetchRun is a read of a prefetch test, planned and not yet run.
type prefetchRun struct {
	p     *mergeIteratePlan
	q     Query
	yield func(*Item) error
	out   prefetchOutcome
}

// prefetchRead validates, plans and runs the read of q over readers in opts.Order (planPrefetch), then finishes it.
func prefetchRead(t testing.TB, ctx context.Context, g *readGate, readers []*Reader, q Query, opts MergeIterateOptions,
	noPrefetch bool, fn func(p *mergeIteratePlan, it *Item) error,
) prefetchOutcome {
	t.Helper()

	r := planPrefetch(t, ctx, g, readers, q, opts, noPrefetch, fn)
	r.run(ctx)

	return r.finish(t)
}

// planPrefetch validates and plans the read of q over readers in opts.Order, with prefetch off when noPrefetch is set,
// then arms g when set, so that only the run's calls are counted.
// The run passes each Item and the plan to fn when fn is set.
func planPrefetch(t testing.TB, ctx context.Context, g *readGate, readers []*Reader, q Query, opts MergeIterateOptions,
	noPrefetch bool, fn func(p *mergeIteratePlan, it *Item) error,
) *prefetchRun {
	t.Helper()

	r := &prefetchRun{q: q}
	r.yield = func(it *Item) error {
		r.out.items = append(r.out.items, copyItem(it))
		if fn != nil {
			return fn(r.p, it)
		}

		return nil
	}
	o, err := checkMergeIterate(ctx, readers, &r.q, opts, r.yield)
	require.NoError(t, err)
	r.p = newMergeIteratePlan(o)
	r.p.noPrefetch = noPrefetch
	r.out.err = r.p.build(ctx, readers, &r.q)
	r.p.res.Incomplete = r.p.gaps.flush(r.p.res.Incomplete)
	if g != nil {
		g.arm()
	}

	return r
}

// run runs the planned read, unless planning it failed; it can run on a goroutine of its own.
func (r *prefetchRun) run(ctx context.Context) {
	if r.out.err != nil {
		return
	}
	if r.p.opts.Order == OrderCapture {
		r.out.err = r.p.runCapture(ctx, &r.q, r.yield)
	} else {
		r.out.err = r.p.runTime(ctx, &r.q, r.yield)
	}
}

// finish returns what the read that ran yielded and returned, requiring nothing to stay held, whatever its outcome.
func (r *prefetchRun) finish(t testing.TB) prefetchOutcome {
	t.Helper()

	assert.Zero(t, r.p.loader.budget.held, "nothing stays held")
	out := r.out
	out.res, out.peak = r.p.res, r.p.loader.budget.peak
	if pf := r.p.loader.pf; pf != nil {
		out.queued, out.inflight = len(pf.queue), pf.inflight.Load()
	}

	return out
}

// requirePrefetchSame requires the read with prefetch, got,
// to have yielded and returned what the read without it, want, did.
func requirePrefetchSame(t testing.TB, want, got prefetchOutcome) {
	t.Helper()

	require.Equal(t, want.items, got.items, "items")
	require.Equal(t, want.res, got.res, "Result")
	if want.err == nil {
		require.NoError(t, got.err)
	} else {
		require.EqualError(t, got.err, want.err.Error())
	}
}

// passingGate returns an unarmed readGate that holds no call, so it only counts the calls once armed.
func passingGate() *readGate {
	g := newReadGate()
	g.hold = func(gateKey) bool { return false }

	return g
}

// sizedSteps returns one-block clusters of one capture, block k holding seqs 2k+1 and 2k+2 at ts_min 10k,
// with payloads of sizes[k] pseudo-random bytes.
func sizedSteps(sizes ...int) []footerTestStep {
	steps := make([]footerTestStep, 0, 2*len(sizes))
	for k, n := range sizes {
		for j := range 2 {
			seq := uint64(2*k + 1 + j)
			steps = append(steps, footerTestStep{rec: randomDataRecord(seq, blockTestHour+int64(10*k+j), n), flush: j == 1})
		}
	}

	return steps
}

// blockTotals returns the loader reservation of each block of r.
func blockTotals(t testing.TB, r *Reader) []int64 {
	t.Helper()

	out := make([]int64, len(r.blocks))
	for i := range r.blocks {
		s, def := r.preflightBlock(i)
		require.Nil(t, def)
		out[i] = s.total
	}

	return out
}

// TestMergeIteratePrefetchAdmission admits blocks against MaxHeldBytes:
// a block whose full reservation does not fit beside those held is not admitted, nor is any block after it;
// one that fits is admitted with its full reservation, and its demand takes the reservation over without a second charge.
// Over more than prefetchDepth consecutive clusters, some of two blocks, under limits near their sizes,
// each read succeeds or fails with ErrReadLimit exactly as without prefetch,
// the bytes held never exceed the limit, and no block is read twice.
func TestMergeIteratePrefetchAdmission(t *testing.T) {
	t.Parallel()

	t.Run("admission and promotion", func(t *testing.T) {
		t.Parallel()

		file := nonePack(t, seg0, captureLow, sizedSteps(1000, 3000, 1000))
		sizes := blockTotals(t, mustOpen(t, file, uncachedWindows()))
		frontier := []blockRef{{0, 0}, {0, 1}, {0, 2}}
		start := func(limit int64) (*mergeIteratePlan, []*Reader, *prefetcher) {
			readers := []*Reader{mustOpen(t, file, uncachedWindows())}
			p, _, err := planReads(t, t.Context(), readers, Query{}, MergeIterateOptions{Order: OrderTime, MaxHeldBytes: limit}, nil)
			require.NoError(t, err)

			pf := p.startPrefetch(t.Context(), frontier)
			require.NotNil(t, pf, "the prefetch starts")

			return p, readers, pf
		}

		p, _, pf := start(sizes[0] + sizes[1] - 1)
		assert.Len(t, pf.queue, 1, "the second block does not fit, and the third, which would, waits behind it")
		assert.Equal(t, sizes[0], p.loader.budget.held)
		pf.close()
		assert.Zero(t, p.loader.budget.held)

		p, readers, pf := start(sizes[0] + sizes[1])
		require.Len(t, pf.queue, 2)
		held := p.loader.budget.held
		assert.Equal(t, sizes[0]+sizes[1], held, "each block admitted holds its full reservation")
		h, def, err := p.loader.load(readers[0], 0, 0)
		require.NoError(t, err)
		require.Nil(t, def)
		assert.Equal(t, held, p.loader.budget.held, "the demand takes over the queued reservation")
		assert.Len(t, pf.queue, 1)
		p.loader.release(h)
		pf.close()
		assert.Zero(t, p.loader.budget.held)

		// A demand the frontier does not predict stops the prefetch, releasing the queue before it reserves.
		p, readers, pf = start(sizes[0] + sizes[1])
		require.Len(t, pf.queue, 2)
		h, _, err = p.loader.load(readers[0], 0, 2)
		require.NoError(t, err)
		assert.True(t, pf.stopped)
		assert.Empty(t, pf.queue)
		assert.Equal(t, sizes[2], p.loader.budget.held)
		p.loader.release(h)
		assert.Zero(t, p.loader.budget.held)
	})

	t.Run("limits near the sizes", func(t *testing.T) {
		t.Parallel()

		sizes := []int{900, 2500, 400, 1800, 3000, 700, 2200, 1200, 600, 2800, 1500, 1000}
		steps := sizedSteps(sizes...)
		// A second pack holds another version of a record of clusters 2, 5 and 9, so those clusters hold two blocks at once.
		other := make([]footerTestStep, 0, 3)
		for _, k := range []int{2, 5, 9} {
			rec := randomDataRecord(uint64(2*k+1), blockTestHour+int64(10*k), sizes[k]+100)
			other = append(other, footerTestStep{rec: rec, flush: true})
		}
		files := [][]byte{nonePack(t, seg0, captureLow, steps), nonePack(t, seg1, captureLow, other)}
		var limits []int64
		for _, f := range files {
			for _, n := range blockTotals(t, mustOpen(t, f, uncachedWindows())) {
				limits = append(limits, n-1, n, n+1)
			}
		}
		a, b := blockTotals(t, mustOpen(t, files[0], uncachedWindows())), blockTotals(t, mustOpen(t, files[1], uncachedWindows()))
		for k, j := range []int{2, 5, 9} {
			n := a[j] + b[k]
			limits = append(limits, n-1, n, n+1)
		}
		slices.Sort(limits)
		limits = slices.Compact(limits)

		for _, order := range []Order{OrderTime, OrderCapture} {
			var refused, succeeded int
			for _, limit := range limits {
				opts := MergeIterateOptions{Order: order, MaxHeldBytes: limit}
				read := func(noPrefetch bool) (prefetchOutcome, map[gateKey]int) {
					g := passingGate()
					readers := make([]*Reader, len(files))
					for i, f := range files {
						r, err := Open(t.Context(), &gatedAt{data: f, reader: i, g: g}, int64(len(f)), uncachedWindows())
						require.NoError(t, err)
						readers[i] = r
					}

					return prefetchRead(t, t.Context(), g, readers, Query{}, opts, noPrefetch, nil), g.calls
				}
				want, _ := read(true)
				got, calls := read(false)
				requirePrefetchSame(t, want, got)
				if got.err != nil {
					require.ErrorIs(t, got.err, ErrReadLimit)
					refused++
				} else {
					succeeded++
				}
				assert.LessOrEqual(t, got.peak, limit, "order %v, limit %d", order, limit)
				for k, n := range calls {
					assert.Equal(t, 1, n, "order %v, limit %d: the block of reader %d at %d read once", order, limit, k.reader, k.off)
				}
			}
			assert.Positive(t, refused, "order %v: reads refused", order)
			assert.Positive(t, succeeded, "order %v: reads that succeed", order)
		}
	})
}

// TestMergeIteratePrefetchUnusedFailure fails the ReadAt of the fourth of ten one-block clusters,
// which the prefetch reads ahead.
// When fn stops the read at its first record, the failure is never consumed and not returned,
// although the prefetch issued the call;
// otherwise the read returns it, wrapped as without prefetch, once it has yielded the records of the first three clusters.
func TestMergeIteratePrefetchUnusedFailure(t *testing.T) {
	t.Parallel()

	file := nonePack(t, seg0, captureLow, prefetchSteps(10))
	failed := gateKey{reader: 0, off: int64(mustOpen(t, file, uncachedWindows()).blocks[3].Offset)}
	read := func(order Order, noPrefetch bool, fn func(*mergeIteratePlan, *Item) error) (prefetchOutcome, map[gateKey]int) {
		g := passingGate()
		g.fail = func(k gateKey) bool { return k == failed }
		readers := openBehindGate(t, g, uncachedWindows(), file)
		out := prefetchRead(t, t.Context(), g, readers, Query{}, MergeIterateOptions{Order: order}, noPrefetch, fn)

		return out, g.calls
	}
	stop := func(*mergeIteratePlan, *Item) error { return errStopIterate }

	for _, order := range []Order{OrderTime, OrderCapture} {
		t.Run(order.String(), func(t *testing.T) {
			t.Parallel()

			want, _ := read(order, true, stop)
			got, calls := read(order, false, stop)
			require.ErrorIs(t, got.err, errStopIterate)
			require.NotErrorIs(t, got.err, errInjected)
			assert.Equal(t, 1, calls[failed], "the failing block was read ahead")
			requirePrefetchSame(t, want, got)

			want, _ = read(order, true, nil)
			got, _ = read(order, false, nil)
			require.ErrorIs(t, got.err, errInjected)
			require.ErrorContains(t, got.err, "tracepack: pack 0, block 3:")
			assert.Len(t, got.items, 6, "the records of the first three clusters")
			requirePrefetchSame(t, want, got)
		})
	}
}

// TestMergeIteratePrefetchCleanup ends reads while a speculative ReadAt, of the third cluster's block, is held at a gate:
// fn's error, ctx cancelled, one conflict over MaxConflicts, and a ReadAt error of the second cluster's block, consumed.
// In each, the read returns only once the held call returned,
// with the error the read without prefetch returns, nothing held, nothing queued, no goroutine left,
// and no ReadAt call after it returned.
func TestMergeIteratePrefetchCleanup(t *testing.T) {
	t.Parallel()

	steps := prefetchSteps(6)
	low := nonePack(t, seg0, captureLow, steps)
	// other holds other versions of seqs 1 and 2: two conflicts in the first cluster.
	other := nonePack(t, seg1, captureLow, changed(steps[:2], 1, 1, 2))
	r := mustOpen(t, low, uncachedWindows())
	held := gateKey{reader: 0, off: int64(r.blocks[2].Offset)}
	second := gateKey{reader: 0, off: int64(r.blocks[1].Offset)}

	tests := []struct {
		name  string
		files [][]byte
		opts  MergeIterateOptions
		fail  *gateKey
		// fn returns the fn of a read whose ctx cancel cancels.
		fn   func(cancel context.CancelFunc) func(*mergeIteratePlan, *Item) error
		want error
	}{
		{
			name: "fn error", files: [][]byte{low},
			fn: func(context.CancelFunc) func(*mergeIteratePlan, *Item) error {
				return func(*mergeIteratePlan, *Item) error { return errStopIterate }
			},
			want: errStopIterate,
		},
		{
			name: "ctx cancelled", files: [][]byte{low},
			fn: func(cancel context.CancelFunc) func(*mergeIteratePlan, *Item) error {
				return func(*mergeIteratePlan, *Item) error {
					cancel()
					return nil
				}
			},
			want: context.Canceled,
		},
		{name: "conflict limit", files: [][]byte{low, other}, opts: MergeIterateOptions{MaxConflicts: 1}, want: ErrReadLimit},
		{name: "consumed ReadAt error", files: [][]byte{low}, fail: &second, want: errInjected},
	}
	for _, tt := range tests {
		for _, order := range []Order{OrderTime, OrderCapture} {
			t.Run(tt.name+"/"+order.String(), func(t *testing.T) {
				t.Parallel()

				opts := tt.opts
				opts.Order = order
				fnOf := func(cancel context.CancelFunc) func(*mergeIteratePlan, *Item) error {
					if tt.fn == nil {
						return nil
					}

					return tt.fn(cancel)
				}
				// The read without prefetch, through readers that fail the same call.
				g := passingGate()
				if tt.fail != nil {
					g.fail = func(k gateKey) bool { return k == *tt.fail }
				}
				ctx, cancel := context.WithCancel(t.Context())
				want := prefetchRead(t, ctx, g, openBehindGate(t, g, uncachedWindows(), tt.files...), Query{}, opts, true, fnOf(cancel))
				cancel()
				require.ErrorIs(t, want.err, tt.want)

				synctest.Test(t, func(t *testing.T) {
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					g := newReadGate()
					g.hold = func(k gateKey) bool { return k == held }
					if tt.fail != nil {
						g.fail = func(k gateKey) bool { return k == *tt.fail }
					}
					readers := openBehindGate(t, g, uncachedWindows(), tt.files...)

					read := planPrefetch(t, ctx, g, readers, Query{}, opts, false, fnOf(cancel))
					done := make(chan struct{})
					go func() {
						defer close(done)
						read.run(ctx)
						g.mu.Lock()
						g.returned = true
						g.mu.Unlock()
					}()
					synctest.Wait()
					early := false
					select {
					case <-done:
						early = true
					default:
					}
					inflight, _ := g.snapshot()
					g.open()
					<-done
					synctest.Wait()
					require.False(t, early, "the read returned while its speculative read was in flight")
					require.Equal(t, 1, inflight, "the held speculative read")
					got := read.finish(t)

					requirePrefetchSame(t, want, got)
					assert.Zero(t, got.queued, "nothing queued")
					assert.Zero(t, got.inflight, "no speculative read in flight")
					assert.Zero(t, g.late, "no ReadAt call after the read returned")
					assert.Equal(t, 1, g.calls[held], "the held block was read once")
				})
			})
		}
	}
}

// TestMergeIteratePrefetchPhysicalReads counts the ReadAt calls of a read of twenty one-block clusters, in each order:
// a complete read issues exactly the calls of the read without prefetch;
// a read fn stops at its first record issues at most prefetchDepth more, none of them for a block read before.
func TestMergeIteratePrefetchPhysicalReads(t *testing.T) {
	t.Parallel()

	file := nonePack(t, seg0, captureLow, prefetchSteps(20))
	read := func(order Order, noPrefetch bool, fn func(*mergeIteratePlan, *Item) error) (prefetchOutcome, map[gateKey]int) {
		g := passingGate()
		readers := openBehindGate(t, g, uncachedWindows(), file)
		out := prefetchRead(t, t.Context(), g, readers, Query{}, MergeIterateOptions{Order: order}, noPrefetch, fn)

		return out, g.calls
	}
	stop := func(*mergeIteratePlan, *Item) error { return errStopIterate }
	for _, order := range []Order{OrderTime, OrderCapture} {
		t.Run(order.String(), func(t *testing.T) {
			t.Parallel()

			want, wantCalls := read(order, true, nil)
			got, gotCalls := read(order, false, nil)
			requirePrefetchSame(t, want, got)
			assert.Equal(t, wantCalls, gotCalls, "a complete read issues the calls of the read without prefetch")

			want, wantCalls = read(order, true, stop)
			got, gotCalls = read(order, false, stop)
			requirePrefetchSame(t, want, got)
			extra := 0
			for k, n := range gotCalls {
				assert.Equal(t, 1, n, "the block at %d read once", k.off)
				extra += n - wantCalls[k]
			}
			assert.Positive(t, extra, "the prefetch read ahead")
			assert.LessOrEqual(t, extra, prefetchDepth)
		})
	}
}

// TestMergeIteratePrefetchCachedDemand reads the demands A, B and C:
// A and B, whose first seqs are equal, are loaded together, and C starts a later cluster;
// A and C need a ReadAt, B is served from the bytes its Reader retains,
// and B holds twice A's records, C as many as A, so B's reservation is about twice A's and C's about A's.
// Every A and B timestamp lies below C's, so in time order A and B are released before C is loaded.
// Under a MaxHeldBytes of A's and B's reservations the read succeeds; one byte less, it fails with ErrReadLimit at B;
// both as without prefetch, the bytes held never over the limit, no block read twice and B never read with ReadAt,
// whatever the capacities of the buffers the held blocks borrow, every loan given back.
// The same holds with two blocks served from retained bytes, B and B2, between A and C,
// the limit then the sum of the three reservations and the refusal at B2.
func TestMergeIteratePrefetchCachedDemand(t *testing.T) {
	t.Parallel()

	ab := func(seqs ...uint64) []footerTestStep {
		steps := make([]footerTestStep, len(seqs))
		for k, seq := range seqs {
			steps[k] = footerTestStep{rec: randomDataRecord(seq, blockTestHour+int64(seq), 2000), flush: k == len(seqs)-1}
		}

		return steps
	}
	c := []footerTestStep{
		{rec: randomDataRecord(10, blockTestHour+100, 2000)},
		{rec: randomDataRecord(11, blockTestHour+101, 2000), flush: true},
	}
	large := nonePack(t, seg0, captureLow, slices.Concat(ab(1, 2), c))
	b := nonePack(t, seg1, captureLow, ab(1, 2, 3, 4))
	b2 := nonePack(t, seg2, captureLow, ab(1, 2, 3, 5))

	for _, cached := range [][][]byte{{b}, {b, b2}} {
		files := append([][]byte{large}, cached...)
		open := func(g *readGate) []*Reader {
			readers := make([]*Reader, len(files))
			for i, f := range files {
				opts := ReaderOptions{}
				if i == 0 {
					opts = uncachedWindows()
				}
				r, err := Open(t.Context(), &gatedAt{data: f, reader: i, g: g}, int64(len(f)), opts)
				require.NoError(t, err)
				readers[i] = r
			}

			return readers
		}
		readers := open(passingGate())
		sizes := blockTotals(t, readers[0])
		limit := sizes[0]
		for _, r := range readers[1:] {
			info := &r.blocks[0]
			require.NotNil(t, r.heldRange(int64(info.Offset), int64(info.OnDiskLen)), "B is served from retained bytes")
			limit += blockTotals(t, r)[0]
		}
		require.Less(t, sizes[1], limit-sizes[0], "C fits beside A")

		for _, order := range []Order{OrderTime, OrderCapture} {
			for _, held := range []int64{limit, limit - 1} {
				for _, k := range []seedKind{seedEmpty, seedSmall, seedEqual, seedWithin, seedOver} {
					opts := MergeIterateOptions{Order: order, MaxHeldBytes: held}
					want := prefetchRead(t, t.Context(), nil, open(passingGate()), Query{}, opts, true, nil)
					g := passingGate()
					pool := seededPool(k, blockSizes{}, 0)
					readers := open(g)
					for _, r := range readers {
						r.bufs = pool
					}
					got := prefetchRead(t, t.Context(), g, readers, Query{}, opts, false, nil)
					requirePrefetchSame(t, want, got)
					assert.Equal(t, errChain(want.err), errChain(got.err), "pool %v", k)
					pool.requireSettled(t, -1)
					if held == limit {
						require.NoError(t, got.err, "%d cached, order %v, pool %v", len(cached), order, k)
					} else {
						require.ErrorIs(t, got.err, ErrReadLimit, "%d cached, order %v, pool %v", len(cached), order, k)
						require.ErrorContains(t, got.err, fmt.Sprintf("tracepack: pack %d, block 0:", len(cached)), "the refusal at the last block served")
					}
					assert.LessOrEqual(t, got.peak, held)
					for key, n := range g.calls {
						assert.Equal(t, 0, key.reader, "only A and C need a ReadAt")
						assert.Equal(t, 1, n, "the block at %d read once", key.off)
					}
				}
			}
		}
	}
}

// TestMergeIteratePrefetchMandatoryRefusal reads a block whose reservation alone exceeds MaxHeldBytes after one that fits:
// the prefetch never admits it, so its demand is made with the queue empty and fails with ErrReadLimit
// as without prefetch, the Result equal, nothing queued or in flight, and nothing held once the read returned,
// whatever the capacities of the buffers the held blocks borrow, every loan given back.
func TestMergeIteratePrefetchMandatoryRefusal(t *testing.T) {
	t.Parallel()

	file := nonePack(t, seg0, captureLow, sizedSteps(1000, 8000))
	sizes := blockTotals(t, mustOpen(t, file, uncachedWindows()))
	limit := sizes[1] - 1
	require.Less(t, sizes[0], limit)

	for _, order := range []Order{OrderTime, OrderCapture} {
		for _, k := range []seedKind{seedEmpty, seedSmall, seedEqual, seedWithin, seedOver} {
			opts := MergeIterateOptions{Order: order, MaxHeldBytes: limit}
			want := prefetchRead(t, t.Context(), nil, openAll(t, file), Query{}, opts, true, nil)
			require.ErrorIs(t, want.err, ErrReadLimit)

			g := passingGate()
			pool := seededPool(k, blockSizes{}, 0)
			readers := openBehindGate(t, g, uncachedWindows(), file)
			for _, r := range readers {
				r.bufs = pool
			}
			got := prefetchRead(t, t.Context(), g, readers, Query{}, opts, false,
				func(p *mergeIteratePlan, _ *Item) error {
					assert.Empty(t, p.loader.pf.queue, "the second block is not admitted")
					assert.Zero(t, p.loader.pf.inflight.Load())

					return nil
				})
			requirePrefetchSame(t, want, got)
			assert.Len(t, got.items, 2)
			assert.Zero(t, got.queued)
			assert.Zero(t, got.inflight)
			assert.Equal(t, map[gateKey]int{{reader: 0, off: int64(mustOpen(t, file, ReaderOptions{}).blocks[0].Offset)}: 1}, g.calls,
				"the refused block is never read")
			assert.Equal(t, errChain(want.err), errChain(got.err), "pool %v", k)
			pool.requireSettled(t, -1)
		}
	}
}

// prefetchSeeds is the number of reads TestMergeIteratePrefetchEquivalent generates,
// and prefetchSeedsLong the number without -short.
const (
	prefetchSeeds     = 64
	prefetchSeedsLong = 256
)

// TestMergeIteratePrefetchEquivalent reads generated reads over several packs (genReadOf),
// one capture or several, walked blocks in clusters excluded and kept, filters on HSMS header fields among them,
// with half of the packs' blocks served from retained bytes and the others read with ReadAt,
// in each order, under the default MaxHeldBytes, a tight one, and the generated one when set,
// and, when the read lists conflicts, under a MaxConflicts of their number and of one less.
// Each read yields the same items, Result and error, its conflicts in the same order, with prefetch on and off;
// a complete read issues exactly the ReadAt calls of the read without prefetch,
// and any read at most prefetchDepth more, none for a block read before.
func TestMergeIteratePrefetchEquivalent(t *testing.T) {
	t.Parallel()

	n := prefetchSeedsLong
	if testing.Short() {
		n = prefetchSeeds
	}
	for seed := range uint64(n) {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			t.Parallel()

			g := genReadOf(t, seed)
			open := func(gate *readGate) []*Reader {
				readers := make([]*Reader, len(g.files))
				for i, f := range g.files {
					opts := ReaderOptions{}
					if i%2 == 0 {
						opts = uncachedWindows()
					}
					r, err := Open(t.Context(), &gatedAt{data: f, reader: i, g: gate}, int64(len(f)), opts)
					require.NoError(t, err)
					readers[i] = r
				}

				return readers
			}
			var largest int64
			for _, r := range open(passingGate()) {
				for i := range r.blocks {
					if s, def := r.preflightBlock(i); def == nil {
						largest = max(largest, s.total)
					}
				}
			}
			limits := []int64{0, largest + largest/2}
			if g.heldLimit != 0 {
				limits = append(limits, g.heldLimit)
			}

			for _, order := range []Order{OrderCapture, OrderTime} {
				for _, limit := range limits {
					opts := MergeIterateOptions{Order: order, MaxHeldBytes: limit}
					want := comparePrefetch(t, open, g.q, opts)
					if conflicts := len(want.res.Conflicts); want.err == nil && conflicts >= 2 {
						opts.MaxConflicts = conflicts
						comparePrefetch(t, open, g.q, opts)
						opts.MaxConflicts = conflicts - 1
						over := comparePrefetch(t, open, g.q, opts)
						require.ErrorIs(t, over.err, ErrReadLimit)
					}
				}
			}
		})
	}
}

// comparePrefetch reads q with opts over the readers open returns, with prefetch off, then on,
// and requires both to yield and return the same, the ReadAt calls of a complete read to be the same,
// and any read to issue at most prefetchDepth calls more, none for a block read before.
// It returns the read without prefetch.
func comparePrefetch(t *testing.T, open func(*readGate) []*Reader, q Query, opts MergeIterateOptions) prefetchOutcome {
	t.Helper()

	wantGate, gotGate := passingGate(), passingGate()
	want := prefetchRead(t, t.Context(), wantGate, open(wantGate), q, opts, true, nil)
	got := prefetchRead(t, t.Context(), gotGate, open(gotGate), q, opts, false, nil)
	requirePrefetchSame(t, want, got)
	if want.err == nil {
		require.Equal(t, wantGate.calls, gotGate.calls, "a complete read issues the calls of the read without prefetch")

		return want
	}
	extra := 0
	for k, n := range gotGate.calls {
		require.LessOrEqual(t, n, wantGate.calls[k]+1, "the block of reader %d at %d read ahead once at most", k.reader, k.off)
		extra += n - wantGate.calls[k]
	}
	require.LessOrEqual(t, extra, prefetchDepth)

	return want
}
