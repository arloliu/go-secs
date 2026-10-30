package tracepack

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedKind names the capacities of the buffers a test pool starts with, against the sizes of a held block.
type seedKind int

const (
	seedEmpty  seedKind = iota // no buffer: every block's buffers allocated at exactly its sizes
	seedSmall                  // one element short of each size: never kept
	seedEqual                  // exactly each size: kept
	seedWithin                 // an eighth above each size: kept
	seedOver                   // one element more than an eighth above: never kept
)

func (k seedKind) String() string {
	return [...]string{"empty", "small", "equal", "within", "over"}[k]
}

// seedCap returns the capacity a slice of kind k has against n elements.
func seedCap(k seedKind, n int) int {
	switch k {
	case seedSmall:
		return max(n-1, 0)
	case seedWithin:
		return n + n/8
	case seedOver:
		return n + n/8 + 1
	case seedEmpty, seedEqual:
		return n
	}

	return n
}

// seededPool returns a pool that lends each held block a buffer of kind k against the block's own sizes.
// s and count are unused: every loan is shaped for the block that takes it.
func seededPool(k seedKind, _ blockSizes, _ int) *loanBufs {
	return &loanBufs{reuse: true, shape: k}
}

// lentBytes returns the bytes of the slice capacities of every buffer l has lent and not been given back (blockBufBytes).
func (l *loanBufs) lentBytes() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()

	var n int64
	for buf := range l.out {
		n += buf.bytes()
	}

	return n
}

// poison fills the whole capacity of every buffer l holds, so that a read still aliasing one sees the change.
func (l *loanBufs) poison() {
	l.mu.Lock()
	defer l.mu.Unlock()

	for _, buf := range l.free {
		for _, b := range [][]byte{buf.raw[:cap(buf.raw)], buf.decoded[:cap(buf.decoded)], buf.rows[:cap(buf.rows)]} {
			for k := range b {
				b[k] = 0xa5
			}
		}
		offs := buf.offs[:cap(buf.offs)]
		for k := range offs {
			offs[k] = 0xa5a5a5a5
		}
	}
}

// pooledRead is what one read over several packs yielded and returned, with the most its budget held at once:
// the error as its text and as the chain of its wrapped errors' types and texts, and whether it wraps ErrReadLimit.
type pooledRead struct {
	items   []iterItem
	res     Result
	err     string
	chain   []string
	isLimit bool
	peak    int64
}

// errChain returns the type and text of err and of every error it wraps, depth first.
func errChain(err error) []string {
	var out []string
	var walk func(err error, depth int)
	walk = func(err error, depth int) {
		if err == nil {
			return
		}
		out = append(out, fmt.Sprintf("%d %T: %v", depth, err, err))
		if u, ok := any(err).(interface{ Unwrap() error }); ok {
			walk(u.Unwrap(), depth+1)
		}
		if u, ok := any(err).(interface{ Unwrap() []error }); ok {
			for _, e := range u.Unwrap() {
				walk(e, depth+1)
			}
		}
	}
	walk(err, 0)

	return out
}

// pooledReadConfig configures readPooled.
type pooledReadConfig struct {
	opts       MergeIterateOptions
	noPrefetch bool
	// stopAfter, when positive, ends the read with errStopRead after that many items.
	stopAfter int
}

// readPooled reads every record of readers, whose held blocks borrow from pool, as MergeIterate does with cfg,
// and checks, whenever a block is read, an item yielded or a block dropped,
// that every slice of a held block fits its block's sizes (fits)
// and that the slice capacities of the buffers lent are at most 9/8 of what the budget holds.
func readPooled(t *testing.T, readers []*Reader, pool *loanBufs, cfg pooledReadConfig) pooledRead {
	t.Helper()

	for _, r := range readers {
		r.bufs = pool
	}
	ctx := t.Context()
	q := Query{Payloads: true}
	o, err := checkMergeIterate(ctx, readers, &q, cfg.opts, acceptItems)
	require.NoError(t, err)
	p := newMergeIteratePlan(o)
	p.noPrefetch = cfg.noPrefetch
	bound := func(where string) {
		held := p.loader.budget.held
		assert.LessOrEqual(t, pool.lentBytes(), held+held/8, "%s: lent capacities within 9/8 of the reservations", where)
	}
	sizes := func(pack, block int) blockSizes {
		s, def := readers[pack].preflightBlock(block)
		require.Nil(t, def)

		return s
	}
	p.loader.readHook = func(pack, block int, buf *blockBuf) {
		s := sizes(pack, block)
		info := &readers[pack].blocks[block]
		if readers[pack].heldRange(int64(info.Offset), int64(info.OnDiskLen)) != nil {
			assert.Zero(t, cap(buf.raw), "pack %d, block %d: a retained block has no raw before its read", pack, block)
		} else {
			assert.True(t, fits(cap(buf.raw), s.raw), "pack %d, block %d: raw %d for %d", pack, block, cap(buf.raw), s.raw)
		}
		assert.True(t, fits(cap(buf.rows), s.rows), "pack %d, block %d: rows %d for %d", pack, block, cap(buf.rows), s.rows)
		assert.True(t, fits(cap(buf.offs), s.offs), "pack %d, block %d: offs %d for %d", pack, block, cap(buf.offs), s.offs)
		assert.True(t, cap(buf.decoded) == 0 || fits(cap(buf.decoded), s.decoded), "pack %d, block %d: decoded %d for %d",
			pack, block, cap(buf.decoded), s.decoded)
		bound("read")
	}
	p.loader.dropHook = func(h *heldBlock) {
		s := sizes(h.pack, h.block)
		buf := h.buf
		assert.True(t, cap(buf.raw) == 0 || fits(cap(buf.raw), s.raw), "drop: raw %d for %d", cap(buf.raw), s.raw)
		assert.True(t, fits(cap(buf.rows), s.rows), "drop: rows %d for %d", cap(buf.rows), s.rows)
		assert.True(t, fits(cap(buf.offs), s.offs), "drop: offs %d for %d", cap(buf.offs), s.offs)
		assert.True(t, cap(buf.decoded) == 0 || fits(cap(buf.decoded), s.decoded), "drop: decoded %d for %d", cap(buf.decoded), s.decoded)
		bound("drop")
	}
	var out pooledRead
	fn := func(it *Item) error {
		out.items = append(out.items, copyItem(it))
		bound("yield")
		if cfg.stopAfter > 0 && len(out.items) == cfg.stopAfter {
			return errStopRead
		}

		return nil
	}
	err = p.build(ctx, readers, &q)
	if err == nil {
		if cfg.opts.Order == OrderCapture {
			err = p.runCapture(ctx, &q, fn)
		} else {
			err = p.runTime(ctx, &q, fn)
		}
	}
	assert.Zero(t, p.loader.budget.held, "nothing stays held")
	pool.requireSettled(t, -1)
	out.res, out.peak = p.res, p.loader.budget.peak
	if err != nil {
		out.err, out.chain, out.isLimit = err.Error(), errChain(err), errors.Is(err, ErrReadLimit)
	}

	return out
}

// poolFixture is a set of packs whose blocks overlap, all of about the same sizes;
// retained, when set, gives for each pack whether its Reader retains it (overriding the case's own choice),
// so a read mixes blocks the prefetch reads ahead with blocks a Reader serves from its retained bytes.
type poolFixture struct {
	name     string
	files    [][]byte
	retained []bool
}

func poolFixtures(t *testing.T) []poolFixture {
	t.Helper()

	return []poolFixture{
		{"zstd conflicts", [][]byte{
			planTestPack(t, seg0, captureLow, false, blockAt(100, 5, 6, 7)),
			planTestPack(t, seg1, captureLow, false, changed(blockAt(0, 6, 7, 8), 1, 6)),
			planTestPack(t, seg2, captureLow, false, changed(blockAt(50, 7, 8, 9), 2, 7)),
		}, nil},
		{"none overlap", [][]byte{
			mergePackCodec(t, CodecNone, seg0, hourSteps(10, 20, 4)),
			mergePackCodec(t, CodecNone, seg1, hourSteps(14, 24, 4)),
			mergePackCodec(t, CodecNone, seg2, changed(hourSteps(18, 28, 4), 3, 19)),
		}, nil},
		{"mixed retention", [][]byte{
			planTestPack(t, seg0, captureLow, false, blockAt(100, 5, 6, 7)),
			planTestPack(t, seg1, captureLow, false, changed(blockAt(0, 6, 7, 8), 1, 6)),
			planTestPack(t, seg2, captureLow, false, changed(blockAt(50, 7, 8, 9), 2, 7)),
			planTestPack(t, seg3, captureLow, false, blockAt(200, 8, 9, 10)),
		}, []bool{false, true, false, true}},
		{"two captures, walked", [][]byte{
			planTestPack(t, seg0, captureLow, false, blockAt(100, 5, 6, 7)),
			planTestPack(t, seg1, captureHigh, true, blockAt(0, 5, 6, 7)),
			planTestPack(t, seg2, captureLow, true, changed(blockAt(50, 6, 7, 8), 1, 7)),
			planTestPack(t, seg3, captureHigh, false, changed(blockAt(10, 6, 7), 2, 6)),
		}, nil},
	}
}

// distinctReservations returns every distinct exact reservation of the blocks of readers, sorted.
func distinctReservations(t *testing.T, readers []*Reader) []int64 {
	t.Helper()

	seen := map[int64]bool{}
	for _, r := range readers {
		for i := range r.blocks {
			s, def := r.preflightBlock(i)
			require.Nil(t, def)
			seen[s.total] = true
		}
	}

	return slices.Sorted(maps.Keys(seen))
}

// largestSizes returns, of every block of the readers, the largest of each size.
func largestSizes(t *testing.T, readers []*Reader) blockSizes {
	t.Helper()

	var m blockSizes
	for _, r := range readers {
		for i := range r.blocks {
			s, def := r.preflightBlock(i)
			require.Nil(t, def)
			m.raw, m.decoded, m.rows, m.offs = max(m.raw, s.raw), max(m.decoded, s.decoded), max(m.rows, s.rows), max(m.offs, s.offs)
		}
	}

	return m
}

// A read over several packs whose held blocks borrow buffers from a pool of any capacities
// yields, returns and fails exactly as a read borrowing nothing, at every budget around what it needs:
// the reservations stay each block's exact sizes, so the pool changes no admission,
// and a pooled slice serves a block only within an eighth above the size it needs,
// so what is lent stays within 9/8 of what is reserved.
// The budgets are each block's exact reservation and the read's peak, each less one, exactly, and plus one;
// the readers retain their packs, read every block with ReadAt, or mix both, and the packs hold two captures and walked ones.
func TestHeldBlocksReusePooledBuffers(t *testing.T) {
	t.Parallel()

	for _, fx := range poolFixtures(t) {
		for _, retained := range []bool{true, false} {
			if fx.retained != nil && !retained {
				continue // the fixture sets each Reader's retention itself
			}
			for _, order := range []Order{OrderCapture, OrderTime} {
				for _, noPrefetch := range []bool{false, true} {
					name := fmt.Sprintf("%s/retained=%v/%v/prefetch=%v", fx.name, retained, order, !noPrefetch)
					t.Run(name, func(t *testing.T) {
						t.Parallel()

						open := func() []*Reader {
							readers := make([]*Reader, len(fx.files))
							for i, f := range fx.files {
								ropts := uncachedWindows()
								if (fx.retained == nil && retained) || (fx.retained != nil && fx.retained[i]) {
									ropts = ReaderOptions{}
								}
								readers[i] = mustOpen(t, f, ropts)
							}

							return readers
						}
						base := readPooled(t, open(), seededPool(seedEmpty, blockSizes{}, 0),
							pooledReadConfig{opts: MergeIterateOptions{Order: order}, noPrefetch: noPrefetch})
						require.Empty(t, base.err)
						require.NotEmpty(t, base.items)
						s := largestSizes(t, open())
						var limits []int64
						for _, v := range append(distinctReservations(t, open()), base.peak) {
							limits = append(limits, v-1, v, v+1)
						}
						refused := 0
						for _, limit := range limits {
							cfg := pooledReadConfig{opts: MergeIterateOptions{Order: order, MaxHeldBytes: limit}, noPrefetch: noPrefetch}
							want := readPooled(t, open(), seededPool(seedEmpty, s, 0), cfg)
							if want.isLimit {
								refused++
							}
							for _, k := range []seedKind{seedSmall, seedEqual, seedWithin, seedOver} {
								got := readPooled(t, open(), seededPool(k, s, 8), cfg)
								assert.Equal(t, want, got, "limit %d (peak %d), pool %v", limit, base.peak, k)
							}
						}
						assert.Positive(t, refused, "some budget refuses a block")
					})
				}
			}
		}
	}
}

// Every lookup FindTransaction makes over generated captures, whose packs lend their held blocks' buffers from pools
// seeded with buffers of every kind, gives the result and the error of the same lookup borrowing nothing,
// at the least budget each lookup needs, one less, one more, and twice it:
// the whole result in its strict form, and every error's chain and sentinels.
func TestFindTransactionReusesPooledBuffers(t *testing.T) {
	t.Parallel()

	for _, seed := range []uint64{1, 2, 3, 5} {
		for _, retained := range []bool{true, false} {
			t.Run(fmt.Sprintf("%d/retained=%v", seed, retained), func(t *testing.T) {
				t.Parallel()

				g := genTxCapture(t, seed)
				ropts := uncachedWindows()
				if retained {
					ropts = ReaderOptions{}
				}
				keys := g.keys
				if len(keys) > 6 {
					keys = keys[:6]
				}
				lookup := func(pool *loanBufs, key TxKey, limit int64) (txStrictForm, []string) {
					res, err := FindTransaction(t.Context(), optsSource{src: g.src, opts: ropts, pool: pool}, key,
						TxOptions{MaxScopes: g.maxScopes, MaxHeldBytes: limit})
					pool.requireSettled(t, -1)

					return strictForm(&res), txErrSnapshot(&res, err)
				}
				// least returns the smallest budget at which the lookup of key, borrowing nothing, passes every reservation.
				least := func(key TxKey) int64 {
					refused := func(limit int64) bool {
						_, err := FindTransaction(t.Context(), optsSource{src: g.src, opts: ropts, pool: &loanBufs{}}, key,
							TxOptions{MaxScopes: g.maxScopes, MaxHeldBytes: limit})

						return errors.Is(err, ErrReadLimit)
					}
					lo, hi := int64(1), int64(1)<<24
					require.False(t, refused(hi))
					for lo < hi {
						if mid := lo + (hi-lo)/2; refused(mid) {
							lo = mid + 1
						} else {
							hi = mid
						}
					}

					return lo
				}
				for _, key := range keys {
					l := least(key)
					for _, limit := range []int64{l - 1, l, l + 1, 2 * l} {
						want, wantErr := lookup(&loanBufs{reuse: true}, key, limit)
						for _, k := range []seedKind{seedSmall, seedEqual, seedWithin, seedOver} {
							got, gotErr := lookup(seededPool(k, blockSizes{}, 0), key, limit)
							requireStrictEqual(t, want, got, "key %v, limit %d, pool %v", key, limit, k)
							assert.Equal(t, wantErr, gotErr, "key %v, limit %d, pool %v", key, limit, k)
						}
					}
				}
			})
		}
	}
}

// holdAt is an io.ReaderAt over data that, once armed, holds every call until released, signalling the first.
type holdAt struct {
	data    []byte
	armed   atomic.Bool
	entered chan struct{}
	once    sync.Once
	release chan struct{}
	// fail, when set, fails a held call at that offset with errInjected once released.
	fail int64
}

func newHoldAt(data []byte) *holdAt {
	return &holdAt{data: data, entered: make(chan struct{}), release: make(chan struct{}), fail: -1}
}

func (h *holdAt) ReadAt(p []byte, off int64) (int, error) {
	if h.armed.Load() {
		h.once.Do(func() { close(h.entered) })
		<-h.release
		if off == h.fail {
			return 0, errInjected
		}
	}

	return copyAt(h.data, p, off)
}

var _ io.ReaderAt = (*holdAt)(nil)

// requireLentBeforeCheck requires every buffer pool lends, while a read of a block waits for its bytes, before its codec is known,
// to have raw at the block's length and every other slice within an eighth of the largest sizes s.
func requireLentBeforeCheck(t *testing.T, pool *loanBufs, s blockSizes) {
	t.Helper()

	pool.mu.Lock()
	defer pool.mu.Unlock()
	require.NotEmpty(t, pool.out, "the waiting read's block has its buffer lent")
	for buf := range pool.out {
		if buf.view != nil {
			continue // a block already checked, held beside the one waiting
		}
		assert.NotZero(t, len(buf.raw), "the read fills raw at the block's length")
		assert.True(t, fits(cap(buf.raw), len(buf.raw)), "raw %d for %d", cap(buf.raw), len(buf.raw))
		assert.LessOrEqual(t, cap(buf.decoded), s.decoded+s.decoded/8, "decoded is kept only when it fits")
		assert.LessOrEqual(t, cap(buf.rows), s.rows+s.rows/8)
		assert.LessOrEqual(t, cap(buf.offs), s.offs+s.offs/8)
	}
}

// hookAt is an io.ReaderAt over data that, once armed, calls onRead at the start of every call, on the caller's goroutine.
type hookAt struct {
	data   []byte
	armed  bool
	onRead func()
}

func (h *hookAt) ReadAt(p []byte, off int64) (int, error) {
	if h.armed {
		h.onRead()
	}

	return copyAt(h.data, p, off)
}

// While a read of a block waits for its bytes, before its codec is known, the block's buffer is lent
// with raw at the block's length and every other slice already fitting its sizes;
// the read then yields what a read with an empty pool yields, and gives every buffer back.
// The demand read, without prefetch, is checked inside its ReadAt for a None and a zstd pack;
// the prefetch's speculative reads, whose buffers are lent at admission, under testing/synctest once every goroutine is blocked,
// for a None pack only, since a zstd decoder created inside the bubble would be pooled and used outside it.
func TestHeldBlockBufferWhileItsReadIsHeld(t *testing.T) {
	t.Parallel()

	for _, c := range []Codec{CodecNone, CodecZstd} {
		for _, k := range []seedKind{seedEmpty, seedWithin, seedOver} {
			t.Run(fmt.Sprintf("%v/%v", c, k), func(t *testing.T) {
				t.Parallel()

				files := [][]byte{
					mergePackCodec(t, c, seg0, hourSteps(10, 20, 4)),
					mergePackCodec(t, c, seg1, hourSteps(14, 24, 4)),
				}
				want := readPooled(t, openAll(t, files...), seededPool(seedEmpty, blockSizes{}, 0),
					pooledReadConfig{opts: MergeIterateOptions{Order: OrderCapture}, noPrefetch: true})
				require.Empty(t, want.err)

				// The demand read, without prefetch.
				readers := make([]*Reader, len(files))
				ats := make([]*hookAt, len(files))
				for i, f := range files {
					ats[i] = &hookAt{data: f}
					r, err := Open(t.Context(), ats[i], int64(len(f)), uncachedWindows())
					require.NoError(t, err)
					readers[i] = r
				}
				s := largestSizes(t, readers)
				pool := seededPool(k, s, 8)
				for _, a := range ats {
					a.onRead = func() { requireLentBeforeCheck(t, pool, s) }
					a.armed = true
				}
				got := readPooled(t, readers, pool, pooledReadConfig{opts: MergeIterateOptions{Order: OrderCapture}, noPrefetch: true})
				assert.Equal(t, want, got)

				if c != CodecNone {
					return
				}
				// The prefetch's speculative reads.
				synctest.Test(t, func(t *testing.T) {
					release := make(chan struct{})
					readers := make([]*Reader, len(files))
					ats := make([]*holdAt, len(files))
					for i, f := range files {
						ats[i] = newHoldAt(f)
						ats[i].release = release
						r, err := Open(t.Context(), ats[i], int64(len(f)), uncachedWindows())
						require.NoError(t, err)
						readers[i] = r
					}
					pool := seededPool(k, s, 8)
					for _, r := range readers {
						r.bufs = pool
					}
					for _, a := range ats {
						a.armed.Store(true)
					}
					var items []iterItem
					done := make(chan error, 1)
					go func() {
						var err error
						items, _, err = collectMergeIterate(t.Context(), readers, Query{Payloads: true}, MergeIterateOptions{Order: OrderCapture})
						done <- err
					}()
					synctest.Wait()
					requireLentBeforeCheck(t, pool, s)
					close(release)
					require.NoError(t, <-done)
					assert.Equal(t, want.items, items)
					pool.requireSettled(t, -1)
				})
			})
		}
	}
}

// A read ended early by fn, by a read error or by ctx
// gives back, exactly once, the buffer of every block it borrowed one for,
// those the prefetch admitted and the read never demanded included,
// and fails as a read borrowing nothing does.
func TestHeldBlockBuffersGivenBackOnEveryEnd(t *testing.T) {
	t.Parallel()

	for _, fx := range poolFixtures(t) {
		t.Run(fx.name, func(t *testing.T) {
			t.Parallel()

			open := func() []*Reader {
				readers := make([]*Reader, len(fx.files))
				for i, f := range fx.files {
					readers[i] = mustOpen(t, f, uncachedWindows())
				}

				return readers
			}
			s := largestSizes(t, open())
			for _, stop := range []int{1, 2, 5} {
				cfg := pooledReadConfig{opts: MergeIterateOptions{Order: OrderTime}, stopAfter: stop}
				want := readPooled(t, open(), seededPool(seedEmpty, s, 0), cfg)
				require.Contains(t, want.err, errStopRead.Error())
				for _, k := range []seedKind{seedEqual, seedOver} {
					got := readPooled(t, open(), seededPool(k, s, 8), cfg)
					assert.Equal(t, want, got, "stop after %d, pool %v", stop, k)
				}
			}

			// A ReadAt failing for the second block read.
			readFailing := func(pool *loanBufs) (string, Result) {
				readers := make([]*Reader, len(fx.files))
				for i, f := range fx.files {
					a := newHoldAt(f)
					r, err := Open(t.Context(), a, int64(len(f)), uncachedWindows())
					require.NoError(t, err)
					r.bufs = pool
					readers[i] = r
					if i == 1 {
						a.fail = int64(r.blocks[0].Offset)
					}
					close(a.release)
					a.armed.Store(true)
				}
				_, res, err := collectMergeIterate(t.Context(), readers, Query{Payloads: true}, MergeIterateOptions{Order: OrderCapture})
				require.ErrorIs(t, err, errInjected)
				pool.requireSettled(t, -1)

				return err.Error(), res
			}
			wantErr, wantRes := readFailing(seededPool(seedEmpty, s, 0))
			gotErr, gotRes := readFailing(seededPool(seedEqual, s, 8))
			assert.Equal(t, wantErr, gotErr)
			assert.Equal(t, wantRes, gotRes)

			// A canceled read.
			ctx, cancel := context.WithCancel(t.Context())
			pool := seededPool(seedEqual, s, 8)
			readers := open()
			for _, r := range readers {
				r.bufs = pool
			}
			_, err := MergeIterate(ctx, readers, Query{Payloads: true}, MergeIterateOptions{Order: OrderCapture}, func(*Item) error {
				cancel()
				return nil
			})
			require.ErrorIs(t, err, context.Canceled)
			pool.requireSettled(t, -1)
		})
	}
}

// poolSource is a PackSource whose readers borrow their held blocks' buffers from pool.
type poolSource struct {
	PackSource
	pool blockBufPool
}

func (s poolSource) Readers(ctx context.Context, hour int64) ([]*Reader, error) {
	readers, err := s.PackSource.Readers(ctx, hour)
	for _, r := range readers {
		r.bufs = s.pool
	}

	return readers, err
}

// txErrSnapshot returns the error chain of err and of every gap and pack error of res,
// each with the read sentinels, and ErrNotPrimary, it matches (errors.Is).
func txErrSnapshot(res *TxResult, err error) []string {
	snap := func(e error) []string {
		out := errChain(e)
		for i, sentinel := range append(slices.Clone(readSentinels), ErrNotPrimary) {
			if e != nil && errors.Is(e, sentinel) {
				out = append(out, fmt.Sprintf("is %d", i))
			}
		}

		return append(out, "--")
	}
	out := snap(err)
	for i := range res.Incomplete {
		out = append(out, snap(res.Incomplete[i].Err)...)
	}
	for i := range res.FooterErrs {
		out = append(out, snap(res.FooterErrs[i].Err)...)
	}

	return out
}

// optsSource is a PackSource over the packs of a memSource whose readers it opens with opts,
// lending their held blocks' buffers from pool.
type optsSource struct {
	src  *memSource
	opts ReaderOptions
	pool blockBufPool
}

func (s optsSource) Readers(ctx context.Context, hour int64) ([]*Reader, error) {
	s.src.mu.Lock()
	files := s.src.hours[hour]
	s.src.mu.Unlock()
	out := make([]*Reader, len(files))
	for i, f := range files {
		r, err := Open(ctx, bytes.NewReader(f), int64(len(f)), s.opts)
		if err != nil {
			return nil, err
		}
		r.bufs = s.pool
		out[i] = r
	}

	return out, nil
}

// FindTransactions copies the records of each hour it reads into its cache,
// so poisoning every buffer given back to the pool, while it passes pairings, changes none of the pairings after.
func TestFindTransactionsCacheOwnsItsRecords(t *testing.T) {
	t.Parallel()

	for _, seed := range []uint64{1, 2, 3, 5, 8} {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			t.Parallel()

			g := genTxCapture(t, seed)
			rng := TxRange{CaptureID: captureLow, From: time.Unix(0, g.first*hourNs), To: time.Unix(0, (g.first+int64(g.hours))*hourNs)}
			opts := TxRangeOptions{Lookup: TxOptions{MaxScopes: g.maxScopes}}
			run := func(pool *loanBufs, poison bool) ([]TxPairing, TxRangeResult, string) {
				var got []TxPairing
				res, err := FindTransactions(t.Context(), poolSource{PackSource: g.src, pool: pool}, rng, opts, func(p *TxPairing) error {
					got = append(got, *p) // the caller's own copy
					if poison {
						pool.poison()
					}

					return nil
				})
				pool.requireSettled(t, -1)
				msg := ""
				if err != nil {
					msg = err.Error()
				}

				return got, res, msg
			}
			want, wantRes, wantErr := run(&loanBufs{reuse: true}, false)
			got, gotRes, gotErr := run(&loanBufs{reuse: true}, true)
			assert.Equal(t, want, got)
			assert.Equal(t, wantRes, gotRes)
			assert.Equal(t, wantErr, gotErr)
		})
	}
}

// A held block within its Reader's retained bytes, compressed, allocates no raw:
// it is read in place, so its load allocates about its on-disk length less than the same block's load through ReadAt.
// It does not run in parallel: it measures the bytes each load allocates, which a test running beside it would add to.
// It keeps the least of 16 loads: under the race detector a sync.Pool drops a quarter of what is put back,
// so a load may build a new zstd decoder, several MB, and a few loads may all do so.
func TestRetainedHeldBlockAllocatesNoRaw(t *testing.T) {
	file := perfPack(t, perfHour(true, 0), 0, 0, 0)
	allocs := func(opts ReaderOptions) (uint64, blockSizes) {
		r := mustOpen(t, file, opts)
		r.bufs = &loanBufs{}
		s, def := r.preflightBlock(0)
		require.Nil(t, def)
		l := newReadAttempts(math64Max)
		var least uint64
		for k := range 16 {
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			h, def, err := l.load(r, 0, 0)
			runtime.ReadMemStats(&after)
			require.NoError(t, err)
			require.Nil(t, def)
			l.drop(h)
			if n := after.TotalAlloc - before.TotalAlloc; k == 0 || n < least {
				least = n
			}
		}

		return least, s
	}
	retained, s := allocs(ReaderOptions{})
	read, _ := allocs(ReaderOptions{WholeReadThreshold: 1, HeadWindow: 1, TailWindow: 1})
	require.Greater(t, s.raw, 256<<10, "a block long enough that its raw shows")
	assert.Less(t, retained+uint64(s.raw)/2, read,
		"retained load %d bytes, ReadAt load %d bytes, raw %d", retained, read, s.raw)
}

// math64Max is a budget no test block reaches.
const math64Max = int64(^uint64(0) >> 1)
