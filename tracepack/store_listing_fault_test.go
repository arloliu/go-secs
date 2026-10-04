package tracepack

import (
	"context"
	"errors"
	"math"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// listedFixture is a listed scope of captureLow in memTestHour with one pack of each kind the listing finds,
// beside a segment of an hour outside the observation:
// generation setA's member, committed, and a segment.
type listedFixture struct {
	store              *memStore
	cat                *fakeCatalog
	archive, segment   string
	otherHour, commits string
}

// newListedFixture returns a listedFixture.
func newListedFixture(t testing.TB) *listedFixture {
	t.Helper()

	store, cat := listedStore()
	f := &listedFixture{store: store, cat: cat, commits: commitDir(t, memTestHour)}
	f.archive = store.putPack(t, memTestHour, storeArchive(t, memA, setA, 1, 2)).Key
	store.putCommit(t, captureLow, memTestHour, setA)
	f.segment = store.putPack(t, memTestHour, storeSegment(t, seg1, nil, 3)).Key
	f.otherHour = store.putPack(t, memTestHour+3, txPackIn(t, seg2, 3, nil, txBlock(inHour(3, txRecord(30, nil))))).Key

	return f
}

// TestStoreSourceListedLimits observes a listed scope at its limits:
// commit objects that change during the first two traversals, with MaxCommitListings 3 and 2,
// and its keys and objects, with MaxObjects at their number and one below, each listed key counted, every commit traversal's again.
// At the limit Observe succeeds; one below, it fails with ErrReadLimit, every object opened closed once.
func TestStoreSourceListedLimits(t *testing.T) {
	t.Parallel()

	t.Run("MaxCommitListings", func(t *testing.T) {
		t.Parallel()

		for _, limit := range []int{3, 2} {
			f := newListedFixture(t)
			traversal := 0
			f.store.beforeList = func(prefix, token string) {
				if prefix != f.commits || token != "" {
					return
				}
				// The first two traversals each find a commit object written just before them.
				if traversal++; traversal <= 2 {
					f.store.putCommit(t, captureLow, memTestHour, UUID{0x5E, 0x20, byte(traversal)})
				}
			}
			o, err := observeOne(t, f.store, f.cat, func(o *StoreSourceOptions) { o.MaxCommitListings = limit })
			if limit == 3 {
				require.NoError(t, err)
				require.NoError(t, o.Close())

				continue
			}
			require.ErrorIs(t, err, ErrReadLimit)
			require.ErrorContains(t, err, "within MaxCommitListings 2")
			assert.Equal(t, 2, f.store.listCount(f.commits))
		}
	})

	t.Run("MaxObjects", func(t *testing.T) {
		t.Parallel()

		// Two commit traversals of one key, one archive key, two staging keys; three objects opened.
		const objects = 2 + 1 + 2 + 3
		f := newListedFixture(t)
		o, err := observeOne(t, f.store, f.cat, func(o *StoreSourceOptions) { o.MaxObjects = objects })
		require.NoError(t, err)
		require.NoError(t, o.Close())
		_, err = observeOne(t, f.store, f.cat, func(o *StoreSourceOptions) { o.MaxObjects = objects - 1 })
		require.ErrorIs(t, err, ErrReadLimit)
		require.ErrorContains(t, err, "more than MaxObjects 7 objects")
	})
}

// TestStoreSourceListedFailures fails a listed scope's acquisition at each step:
// each listing, each open and bootstrap, an object returned with an error, and a cancellation at each step,
// one during the first of two confirmations observed before the second.
// Observe returns no observation and the step's error, every object opened closed once,
// and a lookup over the source fails with a zero outcome.
func TestStoreSourceListedFailures(t *testing.T) {
	t.Parallel()

	failed := errors.New("adapter failed")
	tests := []struct {
		name string
		// fault breaks the fixture; cancel cancels Observe's context.
		fault func(t testing.TB, f *listedFixture, cancel context.CancelFunc)
		is    error
	}{
		{name: "the commit listing", fault: func(_ testing.TB, f *listedFixture, _ context.CancelFunc) {
			f.store.afterList = failPrefix(f.commits, failed)
		}, is: failed},
		{name: "the archive listing", fault: func(t testing.TB, f *listedFixture, _ context.CancelFunc) {
			f.store.afterList = failPrefix(archiveDir(t, memTestHour), failed)
		}, is: failed},
		{name: "the staging listing", fault: func(t testing.TB, f *listedFixture, _ context.CancelFunc) {
			f.store.afterList = failPrefix(stagingDir(t), failed)
		}, is: failed},
		{name: "the head read's open", fault: func(_ testing.TB, f *listedFixture, _ context.CancelFunc) {
			f.store.openHook = failOpen(f.otherHour, failed)
		}, is: failed},
		{name: "a segment returned with an error", fault: func(_ testing.TB, f *listedFixture, _ context.CancelFunc) {
			f.store.openHook = withObject(f.store, f.segment, ErrObjectNotFound)
		}, is: ErrObjectNotFound},
		{name: "an archive's open", fault: func(_ testing.TB, f *listedFixture, _ context.CancelFunc) {
			f.store.openHook = failOpen(f.archive, failed)
		}, is: failed},
		{name: "an archive returned with an error", fault: func(_ testing.TB, f *listedFixture, _ context.CancelFunc) {
			f.store.openHook = withObject(f.store, f.archive, failed)
		}, is: failed},
		{name: "an archive's bootstrap", fault: func(_ testing.TB, f *listedFixture, _ context.CancelFunc) {
			f.store.objects[f.archive] = make([]byte, len(f.store.objects[f.archive]))
		}, is: ErrNotTracepack},
		{name: "a segment's full open", fault: func(t testing.TB, f *listedFixture, _ context.CancelFunc) {
			end := blocksStartOf(t, f.store.objects[f.segment])
			f.store.readHook = func(key string, off int64, n int) error {
				if key == f.segment && off+int64(n) > end {
					return failed
				}

				return nil
			}
		}, is: failed},
		{name: "cancelled during the commit traversals", fault: func(_ testing.TB, f *listedFixture, cancel context.CancelFunc) {
			f.store.afterList = cancelOn(f.commits, cancel)
		}, is: context.Canceled},
		{name: "cancelled during the staging listing", fault: func(t testing.TB, f *listedFixture, cancel context.CancelFunc) {
			f.store.afterList = cancelOn(stagingDir(t), cancel)
		}, is: context.Canceled},
		{name: "cancelled during a head read", fault: func(_ testing.TB, f *listedFixture, cancel context.CancelFunc) {
			f.store.readHook = func(key string, _ int64, _ int) error {
				if key == f.otherHour {
					cancel()
				}

				return nil
			}
		}, is: context.Canceled},
		{name: "cancelled during an archive's open", fault: func(_ testing.TB, f *listedFixture, cancel context.CancelFunc) {
			f.store.readHook = func(key string, _ int64, _ int) error {
				if key == f.archive {
					cancel()
				}

				return nil
			}
		}, is: context.Canceled},
		{name: "cancelled during the first confirmation", fault: func(_ testing.TB, f *listedFixture, cancel context.CancelFunc) {
			f.cat.setIndexed(captureLow, memTestHour+1, false)
			f.cat.beforeConfirm = func(int64) error {
				cancel()

				return nil
			}
		}, is: context.Canceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := newListedFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			tt.fault(t, f, cancel)
			src := newTestStoreSource(t, f.store, f.cat, nil)
			o, err := src.Observe(ctx, captureLow, memTestHour, memTestHour+2)
			require.ErrorIs(t, err, tt.is)
			require.Nil(t, o)
			requireClosedOnce(t, f.store)

			res, err := FindTransaction(ctx, src, txKeyAt(1), TxOptions{MaxScopes: 1})
			require.Error(t, err)
			assert.Zero(t, res.Outcome)
			requireClosedOnce(t, f.store)
		})
	}
}

// failPrefix returns an afterList hook that fails every page of the listing of prefix with err.
func failPrefix(prefix string, err error) func(string, string, *ObjectPage) error {
	return func(p, _ string, page *ObjectPage) error {
		if p != prefix {
			return nil
		}
		*page = ObjectPage{}

		return err
	}
}

// cancelOn returns an afterList hook that cancels once the first page of the listing of prefix is taken.
func cancelOn(prefix string, cancel context.CancelFunc) func(string, string, *ObjectPage) error {
	return func(p, _ string, _ *ObjectPage) error {
		if p == prefix {
			cancel()
		}

		return nil
	}
}

// failOpen returns an openHook that fails the open of key with err.
func failOpen(key string, err error) func(context.Context, string) (bool, Object, error) {
	return func(_ context.Context, k string) (bool, Object, error) {
		if k != key {
			return false, nil, nil
		}

		return true, nil, err
	}
}

// withObject returns an openHook that opens key, then returns its object with err.
func withObject(s *memStore, key string, err error) func(context.Context, string) (bool, Object, error) {
	return func(_ context.Context, k string) (bool, Object, error) {
		if k != key {
			return false, nil, nil
		}

		return true, s.openObject(k), err
	}
}

// TestStoreSourceOnExcluded reports listed packs of a role outside the tiers:
// the retired role and an unknown role beside valid packs, beside a conflicted pair of generations, alone in the scope,
// and before a confirmation that fails or a cancellation during the first of two; each is reported once, after its object is closed.
// A callback that blocks while the context is cancelled makes Observe return the cancellation once it returns,
// and a callback that panics propagates its panic, every object closed once.
func TestStoreSourceOnExcluded(t *testing.T) {
	t.Parallel()

	retired := func(t testing.TB) []byte { return setMetaRole(t, storeArchive(t, memC, setC, 7), packRoleRetired) }
	unknown := func(t testing.TB) []byte { return setMetaRole(t, storeArchive(t, memD, setD, 8), 9) }
	tests := []struct {
		name string
		// setup fills the store and the catalog; it returns the error Observe must fail with, if any.
		setup func(t testing.TB, store *memStore, cat *fakeCatalog, cancel context.CancelFunc) error
		want  SourceScope
		// readers are the scope's readers when Observe succeeds.
		readers []UUID
	}{
		{name: "beside valid packs", setup: func(t testing.TB, store *memStore, _ *fakeCatalog, _ context.CancelFunc) error {
			store.putPack(t, memTestHour, storeSegment(t, seg0, nil, 1))
			store.putPack(t, memTestHour, retired(t))
			store.putPack(t, memTestHour, unknown(t))

			return nil
		}, readers: []UUID{seg0}},
		{name: "beside a conflicted pair", setup: func(t testing.TB, store *memStore, _ *fakeCatalog, _ context.CancelFunc) error {
			store.putPack(t, memTestHour, storeArchive(t, memA, setA, 1))
			store.putPack(t, memTestHour, storeArchive(t, memB, setB, 1))
			store.putCommit(t, captureLow, memTestHour, setA)
			store.putCommit(t, captureLow, memTestHour, setB)
			store.putPack(t, memTestHour, retired(t))
			store.putPack(t, memTestHour, unknown(t))

			return nil
		}, want: SourceScope{Conflicted: true}},
		{name: "alone", setup: func(t testing.TB, store *memStore, _ *fakeCatalog, _ context.CancelFunc) error {
			store.putPack(t, memTestHour, retired(t))
			store.putPack(t, memTestHour, unknown(t))

			return nil
		}},
		{name: "before a confirmation that fails", setup: func(t testing.TB, store *memStore, cat *fakeCatalog, _ context.CancelFunc) error {
			store.putPack(t, memTestHour, retired(t))
			store.putPack(t, memTestHour, unknown(t))
			cat.historyLost = true

			return errors.New("does not confirm")
		}},
		{name: "before a cancellation", setup: func(t testing.TB, store *memStore, cat *fakeCatalog, cancel context.CancelFunc) error {
			store.putPack(t, memTestHour, retired(t))
			store.putPack(t, memTestHour, unknown(t))
			cat.setIndexed(captureLow, memTestHour+1, false)
			cat.beforeConfirm = func(int64) error {
				cancel()

				return nil
			}

			return context.Canceled
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store, cat := listedStore()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			wantErr := tt.setup(t, store, cat, cancel)
			retiredKey, unknownKey := archiveKeyAt(t, memTestHour, memC), archiveKeyAt(t, memTestHour, memD)
			var log excludedLog
			log.hook = func(p ExcludedPack) {
				_, closes := store.openCounts()
				assert.Equal(t, 1, closes[p.Key], "closed before it is reported")
			}
			src := newTestStoreSource(t, store, cat, log.reporting)
			o, err := src.Observe(ctx, captureLow, memTestHour, memTestHour+2)
			assert.Equal(t, []ExcludedPack{
				{Key: retiredKey, PackID: memC, Role: packRoleRetired, Hour: memTestHour},
				{Key: unknownKey, PackID: memD, Role: 9, Hour: memTestHour},
			}, log.list())
			if wantErr != nil {
				require.Error(t, err)
				if errors.Is(wantErr, context.Canceled) {
					require.ErrorIs(t, err, context.Canceled)
				} else {
					require.ErrorContains(t, err, wantErr.Error())
				}
				require.Nil(t, o)
				requireClosedOnce(t, store)

				return
			}
			require.NoError(t, err)
			so := asStoreObservation(t, o)
			requireStoreCharges(t, so)
			sc, err := so.Scope(t.Context(), memTestHour)
			require.NoError(t, err)
			assert.Equal(t, tt.want.Conflicted, sc.Conflicted)
			if tt.readers == nil {
				assert.Empty(t, sc.Readers)
			} else {
				assert.Equal(t, tt.readers, packIDs(sc.Readers))
			}
			require.NoError(t, o.Close())
			requireClosedOnce(t, store)
		})
	}

	t.Run("blocking while cancelled", func(t *testing.T) {
		t.Parallel()

		store, cat := listedStore()
		store.putPack(t, memTestHour, storeSegment(t, seg0, nil, 1))
		store.putPack(t, memTestHour, retired(t))
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		entered, release := make(chan struct{}), make(chan struct{})
		var calls atomic.Int32
		src := newTestStoreSource(t, store, cat, func(o *StoreSourceOptions) {
			o.OnExcluded = func(ExcludedPack) {
				calls.Add(1)
				close(entered)
				<-release
			}
		})
		done := make(chan error, 1)
		go func() {
			o, err := src.Observe(ctx, captureLow, memTestHour, memTestHour+1)
			assert.Nil(t, o)
			done <- err
		}()
		<-entered
		cancel()
		close(release)
		require.ErrorIs(t, <-done, context.Canceled)
		assert.Equal(t, int32(1), calls.Load())
		requireClosedOnce(t, store)
	})

	t.Run("panicking", func(t *testing.T) {
		t.Parallel()

		store, cat := listedStore()
		store.putPack(t, memTestHour, storeSegment(t, seg0, nil, 1))
		store.putPack(t, memTestHour, retired(t))
		src := newTestStoreSource(t, store, cat, func(o *StoreSourceOptions) {
			o.OnExcluded = func(ExcludedPack) { panic("callback panic") }
		})
		require.PanicsWithValue(t, "callback panic", func() { _, _ = src.Observe(t.Context(), captureLow, memTestHour, memTestHour+1) })
		requireClosedOnce(t, store)
	})
}

// TestStoreSourceDiscardCloseErrors fails the Close of objects a listed scope's Observe discards:
// a segment of an hour not listed, a pack excluded for its role, a candidate outside the view, and the packs of a conflicted view.
// With no earlier error, Observe fails with the close error, the excluded pack still reported;
// after a primary failure, the close error of a candidate not yet discarded is joined after it.
// A retained listed pack's close error is the observation Close's.
func TestStoreSourceDiscardCloseErrors(t *testing.T) {
	t.Parallel()

	closeFailed := errors.New("close failed")
	tests := []struct {
		name string
		// put fills the listed scope and returns the key whose Close fails.
		put func(t testing.TB, s *memStore) string
	}{
		{name: "another hour's segment", put: func(t testing.TB, s *memStore) string {
			s.putPack(t, memTestHour, storeSegment(t, seg0, nil, 1))

			return s.putPack(t, memTestHour+3, txPackIn(t, seg2, 3, nil, txBlock(inHour(3, txRecord(30, nil))))).Key
		}},
		{name: "an excluded pack", put: func(t testing.TB, s *memStore) string {
			s.putPack(t, memTestHour, storeSegment(t, seg0, nil, 1))

			return s.putPack(t, memTestHour, setMetaRole(t, storeArchive(t, memC, setC, 7), packRoleRetired)).Key
		}},
		{name: "a candidate outside the view", put: func(t testing.TB, s *memStore) string {
			s.putPack(t, memTestHour, storeSegment(t, seg0, nil, 1))

			return s.putPack(t, memTestHour, storeArchive(t, memA, setA, 1)).Key
		}},
		{name: "a conflicted view's pack", put: func(t testing.TB, s *memStore) string {
			s.putPack(t, memTestHour, storeArchive(t, memA, setA, 1))
			s.putCommit(t, captureLow, memTestHour, setA)
			s.putCommit(t, captureLow, memTestHour, setB)

			return s.putPack(t, memTestHour, storeArchive(t, memB, setB, 1)).Key
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store, cat := listedStore()
			key := tt.put(t, store)
			store.closeErr[key] = closeFailed
			var log excludedLog
			_, err := observeOne(t, store, cat, log.reporting)
			require.ErrorIs(t, err, closeFailed)
			require.ErrorContains(t, err, key)
			if tt.name == "an excluded pack" {
				assert.Len(t, log.list(), 1, "reported all the same")
			}
		})
	}

	t.Run("after a primary failure", func(t *testing.T) {
		t.Parallel()

		store, cat := listedStore()
		candidate := store.putPack(t, memTestHour, storeSegment(t, seg0, nil, 1)).Key
		archive := store.putPack(t, memTestHour, storeArchive(t, memA, setA, 1)).Key
		store.closeErr[candidate] = closeFailed
		failed := errors.New("open failed")
		store.openHook = failOpen(archive, failed)
		_, err := observeOne(t, store, cat, nil)
		require.ErrorIs(t, err, failed)
		require.ErrorIs(t, err, closeFailed)
		assert.Less(t, strings.Index(err.Error(), failed.Error()), strings.Index(err.Error(), closeFailed.Error()), "the primary error first")
	})

	t.Run("retained", func(t *testing.T) {
		t.Parallel()

		store, cat := listedStore()
		key := store.putPack(t, memTestHour, storeSegment(t, seg0, nil, 1)).Key
		store.closeErr[key] = closeFailed
		o, err := newTestStoreSource(t, store, cat, nil).Observe(t.Context(), captureLow, memTestHour, memTestHour+1)
		require.NoError(t, err)
		require.ErrorIs(t, o.Close(), closeFailed)
		requireClosedOnce(t, store)
	})
}

// TestStoreSourceListedCtxPerEntry counts the checks of Observe's context over listings that grow by n keys of another capture,
// which the source skips, in one page: the source checks its context for every key a listing returns.
func TestStoreSourceListedCtxPerEntry(t *testing.T) {
	t.Parallel()

	const n = 200
	checks := func(count int) int64 {
		store, cat := listedStore()
		for i := range count {
			key, err := ArchiveKey(storeTestPrefix, memStoreTool, UUID{0x0B, byte(i >> 8), byte(i)}, memTestHour, memA)
			require.NoError(t, err)
			store.put(key, nil)
		}
		ctx := errCountingCtx{Context: t.Context(), n: &atomic.Int64{}}
		o, err := newTestStoreSource(t, store, cat, nil).Observe(ctx, captureLow, memTestHour, memTestHour+1)
		require.NoError(t, err)
		require.NoError(t, o.Close())

		return ctx.n.Load()
	}
	assert.GreaterOrEqual(t, checks(2*n)-checks(n), int64(n))
}

// callerCtx is a context that records the function that first checks its Err once armed is set.
type callerCtx struct {
	context.Context
	armed *atomic.Bool
	first *atomic.Value
}

// Err records its caller's name on the first call once armed, and returns the context's error.
func (c callerCtx) Err() error {
	if c.armed.Load() && c.first.Load() == nil {
		if pc, _, _, ok := runtime.Caller(1); ok {
			c.first.Store(runtime.FuncForPC(pc).Name())
		}
	}

	return c.Context.Err()
}

// TestStoreSourceOnExcludedCtx cancels Observe's context from OnExcluded, reporting the last pack opened:
// the context is checked as soon as the callback returns, before the view is computed, and Observe fails with the cancellation.
func TestStoreSourceOnExcludedCtx(t *testing.T) {
	t.Parallel()

	store, cat := listedStore()
	store.putPack(t, memTestHour, storeSegment(t, seg0, nil, 1))
	store.putPack(t, memTestHour, setMetaRole(t, storeArchive(t, memC, setC, 7), packRoleRetired))
	base, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx := callerCtx{Context: base, armed: &atomic.Bool{}, first: &atomic.Value{}}
	src := newTestStoreSource(t, store, cat, func(o *StoreSourceOptions) {
		o.OnExcluded = func(ExcludedPack) {
			cancel()
			ctx.armed.Store(true)
		}
	})
	views := 0
	src.activeView = func(packs []PackInfo, c CommitSet) (View, error) {
		views++

		return ActiveView(packs, c)
	}
	o, err := src.Observe(ctx, captureLow, memTestHour, memTestHour+1)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, o)
	assert.Zero(t, views)
	first, ok := ctx.first.Load().(string)
	require.True(t, ok, "the context is checked after the callback")
	assert.True(t, strings.HasSuffix(first, ".openArchive"), "checked by %s", first)
	requireClosedOnce(t, store)
}

// TestStoreConfirmReleasesTokens confirms two listed scopes with long tokens:
// each confirmation releases its scope's descriptor and token, before the next one is asked.
func TestStoreConfirmReleasesTokens(t *testing.T) {
	t.Parallel()

	store, cat := newMemStore(), newFakeCatalog()
	cat.tokenLen = 4096
	cat.setIndexed(captureLow, memTestHour, false)
	cat.setIndexed(captureLow, memTestHour+1, false)
	src := newTestStoreSource(t, store, cat, nil)
	a := &storeAcquisition{
		src: src, capture: captureLow, from: memTestHour, to: memTestHour + 2,
		budget: storeBudget{maxBytes: math.MaxInt64, maxObjects: math.MaxInt},
	}
	snap, err := cat.Snapshot(t.Context(), captureLow, memTestHour, memTestHour+2)
	require.NoError(t, err)
	require.NoError(t, a.takeSnapshot(t.Context(), &snap))
	hours := []*storeListedHour{
		{scope: 0, hour: memTestHour, token: snap.Scopes[0].Token},
		{scope: 1, hour: memTestHour + 1, token: snap.Scopes[1].Token},
	}
	var used []int64
	cat.beforeConfirm = func(int64) error {
		used = append(used, a.budget.used)

		return nil
	}
	require.NoError(t, a.confirm(t.Context(), snap.Scopes, hours))
	used = append(used, a.budget.used)
	require.Len(t, used, 3)
	for i := range 2 {
		assert.Equal(t, scopeDescriptorCost(&snap.Scopes[i]), used[i]-used[i+1], "hour %d", i)
		assert.Greater(t, scopeDescriptorCost(&snap.Scopes[i]), int64(4096))
	}
	assert.Zero(t, a.descriptors)
}
