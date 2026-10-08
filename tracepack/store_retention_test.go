package tracepack

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// observeRetention observes captureLow over the hours [memTestHour, memTestHour+hours) through a store source over store and cat
// whose Retention is p, its options changed further by edit when set.
// On error it requires no observation and every object opened closed once;
// on success it requires the charges recounted, and closes the observation when the test ends,
// requiring then every object opened closed once.
func observeRetention(t testing.TB, store *memStore, cat *fakeCatalog, p Retention, hours int64, edit func(o *StoreSourceOptions)) (*storeObservation, error) {
	t.Helper()

	src := newTestStoreSource(t, store, cat, func(o *StoreSourceOptions) {
		o.Retention = p
		if edit != nil {
			edit(o)
		}
	})
	o, err := src.Observe(t.Context(), captureLow, memTestHour, memTestHour+hours)
	if err != nil {
		require.Nil(t, o)
		requireClosedOnce(t, store)

		return nil, err
	}
	so := asStoreObservation(t, o)
	requireStoreCharges(t, so)
	t.Cleanup(func() {
		assert.NoError(t, o.Close())
		requireClosedOnce(t, store)
	})

	return so, nil
}

// requireRemovedScope requires that o reports the scope of memTestHour + hours removed, with no readers.
func requireRemovedScope(t testing.TB, o *storeObservation, hours int64) {
	t.Helper()

	sc, err := o.Scope(t.Context(), memTestHour+hours)
	require.NoError(t, err)
	require.Equal(t, SourceScope{Removed: true}, sc, "hour %d", hours)
}

// requireScopeIDs requires that o reports the scope of memTestHour + hours retained, with the readers of ids in order.
func requireScopeIDs(t testing.TB, o *storeObservation, hours int64, ids ...UUID) {
	t.Helper()

	sc, err := o.Scope(t.Context(), memTestHour+hours)
	require.NoError(t, err)
	require.False(t, sc.Removed, "hour %d", hours)
	if len(ids) == 0 {
		require.Empty(t, sc.Readers, "hour %d", hours)

		return
	}
	require.Equal(t, ids, packIDs(sc.Readers), "hour %d", hours)
}

// requireClosedNow requires that every object opened under keys is closed already, as many times as it was opened.
func requireClosedNow(t testing.TB, store *memStore, keys ...string) {
	t.Helper()

	opens, closes := store.openCounts()
	for _, k := range keys {
		require.Positive(t, opens[k], "%q was opened", k)
		require.Equal(t, opens[k], closes[k], "%q is closed", k)
	}
}

// pageCount returns how many pages of prefix store listed.
func (s *memStore) pageCount(prefix string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range s.events {
		if e.op == "list" && e.key == prefix {
			n++
		}
	}

	return n
}

// openEvents returns how many opens store was asked for, of objects present or not.
func (s *memStore) openEvents() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range s.events {
		if e.op == "open" {
			n++
		}
	}

	return n
}

// confirmed reports whether c was asked to confirm captureLow's scope of memTestHour + hours.
func (c *fakeCatalog) confirmed(hours int64) bool {
	for _, e := range c.catalogLog() {
		if e.op == "confirm" && e.hour == memTestHour+hours {
			return true
		}
	}

	return false
}

// retentionArchive returns archive id of captureLow in memTestHour + hours, the member of generation set ranked gen, holding seqs.
func retentionArchive(t testing.TB, id, set UUID, hours int64, gen uint64, seqs ...uint64) []byte {
	t.Helper()

	return txPackIn(t, id, hours, func(m *PackMeta) { generationMeta(m, set, 1, gen, nil) }, txBlock(txSeqs(seqs...)...))
}

// retentionSegment returns segment id of captureLow in memTestHour + hours holding seqs.
func retentionSegment(t testing.TB, id UUID, hours int64, seqs ...uint64) []byte {
	t.Helper()

	return txPackIn(t, id, hours, nil, txBlock(txSeqs(seqs...)...))
}

// TestStoreSourceRetentionSamples observes, with a boundary that never removes an hour,
// an indexed hour of two packs and a listed hour of an archive and a segment, listed in pages of one key:
// the observation is the one taken without a boundary,
// and the provider is asked once before every page and every open, once before the listed hour's view, and once at the end.
func TestStoreSourceRetentionSamples(t *testing.T) {
	t.Parallel()

	build := func(t testing.TB) (*memStore, *fakeCatalog) {
		t.Helper()

		store, cat := newMemStore(), newFakeCatalog()
		store.pageSize = 1
		cat.setView(captureLow, memTestHour,
			store.putPack(t, memTestHour, retentionSegment(t, seg0, 0, 1)), store.putPack(t, memTestHour, retentionSegment(t, seg1, 0, 2)))
		cat.setIndexed(captureLow, memTestHour+1, false)
		store.putPack(t, memTestHour+1, retentionArchive(t, memA, setA, 1, 1, 10))
		store.putCommit(t, captureLow, memTestHour+1, setA)
		store.putPack(t, memTestHour+1, retentionSegment(t, seg2, 1, 11))

		return store, cat
	}

	plainStore, plainCat := build(t)
	plain, err := observeRetention(t, plainStore, plainCat, nil, 2, nil)
	require.NoError(t, err)

	store, cat := build(t)
	p := newTestRetention(memTestHour)
	o, err := observeRetention(t, store, cat, p, 2, nil)
	require.NoError(t, err)
	for h := range int64(2) {
		want, err := plain.Scope(t.Context(), memTestHour+h)
		require.NoError(t, err)
		requireScopeIDs(t, o, h, packIDs(want.Readers)...)
	}
	assert.Equal(t, plain.charged, o.charged)

	pages := store.pageCount(commitDir(t, memTestHour+1)) + store.pageCount(archiveDir(t, memTestHour+1)) + store.pageCount(stagingDir(t))
	assert.Equal(t, pages+store.openEvents()+1+1, p.callCount(), "a sample per page and open, before the listed view, and at the end")
}

// TestStoreSourceRetentionListPages advances the boundary past a listed hour after a page of its listings, in pages of one key:
// after the first page of its first commit traversal, after its first traversal, after its archive listing's first page,
// and after the staging listing's first page.
// The sample before the next page finds the hour removed:
// no further page of its listings is taken and none of its packs is opened,
// and the scope is reported removed; the indexed hour beside it keeps its view.
// The staging listing goes on while another listed hour is retained, and stops once none is.
func TestStoreSourceRetentionListPages(t *testing.T) {
	t.Parallel()

	type fixture struct {
		store *memStore
		cat   *fakeCatalog
		p     *testRetention
		// indexed is the indexed hour's pack; archives and segments the listed hour's packs.
		indexed            CatalogPack
		archives, segments []string
	}
	build := func(t testing.TB, listed int64) *fixture {
		t.Helper()

		f := &fixture{p: newTestRetention(memTestHour)}
		f.store, f.cat = newMemStore(), newFakeCatalog()
		f.store.pageSize = 1
		f.indexed = f.store.putPack(t, memTestHour+2, retentionSegment(t, seg4, 2, 40))
		f.cat.setView(captureLow, memTestHour+2, f.indexed)
		for h := range listed {
			f.cat.setIndexed(captureLow, memTestHour+h, false)
		}
		f.archives = []string{
			f.store.putPack(t, memTestHour, retentionArchive(t, memA, setA, 0, 1, 1)).Key,
			f.store.putPack(t, memTestHour, retentionArchive(t, memB, setB, 0, 2, 1)).Key,
		}
		f.store.putCommit(t, captureLow, memTestHour, setA)
		f.store.putCommit(t, captureLow, memTestHour, setB)
		f.segments = []string{
			f.store.putPack(t, memTestHour, retentionSegment(t, seg0, 0, 2)).Key,
			f.store.putPack(t, memTestHour, retentionSegment(t, seg1, 0, 3)).Key,
		}

		return f
	}
	// advanceAfter returns an afterList hook advancing f's boundary to memTestHour + to after the first page of prefix.
	advanceAfter := func(f *fixture, prefix string, to int64) func(string, string, *ObjectPage) error {
		return func(pr, token string, _ *ObjectPage) error {
			if pr == prefix && token == "" {
				f.p.set(memTestHour + to)
			}

			return nil
		}
	}
	requireNoneOpened := func(t testing.TB, store *memStore, keys []string) {
		t.Helper()

		opens, _ := store.openCounts()
		for _, k := range keys {
			assert.Zero(t, opens[k], "%q is not opened", k)
		}
	}

	t.Run("commit traversal", func(t *testing.T) {
		t.Parallel()

		f := build(t, 1)
		f.store.afterList = advanceAfter(f, commitDir(t, memTestHour), 1)
		o, err := observeRetention(t, f.store, f.cat, f.p, 3, nil)
		require.NoError(t, err)
		requireRemovedScope(t, o, 0)
		requireScopeIDs(t, o, 2, seg4)
		assert.Equal(t, 1, f.store.pageCount(commitDir(t, memTestHour)), "the first page only")
		assert.Zero(t, f.store.pageCount(archiveDir(t, memTestHour)))
		assert.Zero(t, f.store.pageCount(stagingDir(t)), "no listed hour is retained")
		requireNoneOpened(t, f.store, append(f.archives, f.segments...))
		assert.False(t, f.cat.confirmed(0))
	})
	t.Run("second commit traversal", func(t *testing.T) {
		t.Parallel()

		f := build(t, 1)
		dir := commitDir(t, memTestHour)
		f.store.afterList = func(prefix, token string, page *ObjectPage) error {
			// The last page of the first traversal: its hour is removed, its commit objects then deleted.
			if prefix == dir && page.Next == "" && f.store.listCount(dir) == 1 {
				f.p.set(memTestHour + 1)
				for _, set := range []UUID{setA, setB} {
					key, err := CommitKey(storeTestPrefix, memStoreTool, captureLow, memTestHour, set)
					require.NoError(t, err)
					f.store.remove(key)
				}
			}

			return nil
		}
		o, err := observeRetention(t, f.store, f.cat, f.p, 3, nil)
		require.NoError(t, err)
		requireRemovedScope(t, o, 0)
		assert.Equal(t, 1, f.store.listCount(dir), "the second traversal is not started")
	})
	t.Run("archive listing", func(t *testing.T) {
		t.Parallel()

		f := build(t, 1)
		f.store.afterList = advanceAfter(f, archiveDir(t, memTestHour), 1)
		o, err := observeRetention(t, f.store, f.cat, f.p, 3, nil)
		require.NoError(t, err)
		requireRemovedScope(t, o, 0)
		requireScopeIDs(t, o, 2, seg4)
		assert.Equal(t, 1, f.store.pageCount(archiveDir(t, memTestHour)), "the first page only")
		assert.Zero(t, f.store.pageCount(stagingDir(t)))
		requireNoneOpened(t, f.store, append(f.archives, f.segments...))
	})
	t.Run("staging listing, another listed hour retained", func(t *testing.T) {
		t.Parallel()

		f := build(t, 2)
		f.store.afterList = advanceAfter(f, stagingDir(t), 1)
		o, err := observeRetention(t, f.store, f.cat, f.p, 3, nil)
		require.NoError(t, err)
		requireRemovedScope(t, o, 0)
		requireScopeIDs(t, o, 1)
		requireScopeIDs(t, o, 2, seg4)
		assert.Equal(t, 3, f.store.pageCount(stagingDir(t)), "two keys and the last, empty, page")
		// Each segment's head is read to learn its hour, which is removed: none is opened in full.
		requireNoneOpened(t, f.store, f.archives)
		assert.True(t, f.cat.confirmed(1))
		assert.False(t, f.cat.confirmed(0))
	})
	t.Run("staging listing, every listed hour removed", func(t *testing.T) {
		t.Parallel()

		f := build(t, 2)
		f.store.afterList = advanceAfter(f, stagingDir(t), 2)
		o, err := observeRetention(t, f.store, f.cat, f.p, 3, nil)
		require.NoError(t, err)
		requireRemovedScope(t, o, 0)
		requireRemovedScope(t, o, 1)
		requireScopeIDs(t, o, 2, seg4)
		assert.Equal(t, 1, f.store.pageCount(stagingDir(t)), "the first page only")
		requireNoneOpened(t, f.store, append(f.archives, f.segments...))
		assert.False(t, f.cat.confirmed(0))
		assert.False(t, f.cat.confirmed(1))
	})
}

// TestStoreSourceRetentionOpens advances the boundary past an hour while one of its packs is read:
// an indexed hour of two packs, and a listed hour of two archives.
// The sample before the next open finds the hour removed:
// its next pack is not opened, the one opened is closed at once,
// and the scope is reported removed.
// A boundary passing several hours at once removes them all, a conflicted hour and a listed one among them,
// and nothing more is opened or listed for them.
func TestStoreSourceRetentionOpens(t *testing.T) {
	t.Parallel()

	t.Run("indexed", func(t *testing.T) {
		t.Parallel()

		store, cat := newMemStore(), newFakeCatalog()
		first := store.putPack(t, memTestHour, retentionSegment(t, seg0, 0, 1))
		second := store.putPack(t, memTestHour, retentionSegment(t, seg1, 0, 2))
		cat.setView(captureLow, memTestHour, first, second)
		next := store.putPack(t, memTestHour+1, retentionSegment(t, seg2, 1, 10))
		cat.setView(captureLow, memTestHour+1, next)
		p := newTestRetention(memTestHour)
		store.readHook = func(key string, _ int64, _ int) error {
			if key == first.Key {
				p.set(memTestHour + 1)
			}

			return nil
		}
		store.closeHook = func(key string) {
			if key == first.Key {
				opens, _ := store.openCounts()
				assert.Zero(t, opens[next.Key], "closed before the next scope's pack is opened")
			}
		}
		o, err := observeRetention(t, store, cat, p, 2, nil)
		require.NoError(t, err)
		requireRemovedScope(t, o, 0)
		requireScopeIDs(t, o, 1, seg2)
		opens, _ := store.openCounts()
		assert.Zero(t, opens[second.Key], "the removed hour's next pack is not opened")
		requireClosedNow(t, store, first.Key)
	})
	t.Run("later hours removed at once", func(t *testing.T) {
		t.Parallel()

		store, cat := newMemStore(), newFakeCatalog()
		first := store.putPack(t, memTestHour, retentionSegment(t, seg0, 0, 1))
		cat.setView(captureLow, memTestHour, first)
		second := store.putPack(t, memTestHour+1, retentionSegment(t, seg1, 1, 10))
		cat.setView(captureLow, memTestHour+1, second)
		cat.setConflicted(captureLow, memTestHour+2, true)
		cat.setIndexed(captureLow, memTestHour+3, false)
		store.putPack(t, memTestHour+3, retentionSegment(t, seg2, 3, 30))
		kept := store.putPack(t, memTestHour+4, retentionSegment(t, seg3, 4, 40))
		cat.setView(captureLow, memTestHour+4, kept)
		p := newTestRetention(memTestHour)
		store.readHook = func(key string, _ int64, _ int) error {
			if key == first.Key {
				p.set(memTestHour + 4)
			}

			return nil
		}
		o, err := observeRetention(t, store, cat, p, 5, nil)
		require.NoError(t, err)
		for h := range int64(4) {
			requireRemovedScope(t, o, h)
		}
		requireScopeIDs(t, o, 4, seg3)
		opens, _ := store.openCounts()
		assert.Zero(t, opens[second.Key])
		assert.Zero(t, store.pageCount(commitDir(t, memTestHour+3)), "nothing is listed for a removed hour")
		assert.Zero(t, store.pageCount(stagingDir(t)))
	})
	t.Run("listed", func(t *testing.T) {
		t.Parallel()

		store, cat := listedStore()
		first := store.putPack(t, memTestHour, retentionArchive(t, memA, setA, 0, 1, 1)).Key
		second := store.putPack(t, memTestHour, retentionArchive(t, memB, setB, 0, 2, 1)).Key
		store.putCommit(t, captureLow, memTestHour, setA)
		store.putCommit(t, captureLow, memTestHour, setB)
		p := newTestRetention(memTestHour)
		store.readHook = func(key string, _ int64, _ int) error {
			if key == first {
				p.set(memTestHour + 1)
			}

			return nil
		}
		o, err := observeRetention(t, store, cat, p, 2, nil)
		require.NoError(t, err)
		requireRemovedScope(t, o, 0)
		opens, _ := store.openCounts()
		assert.Zero(t, opens[second], "the removed hour's next pack is not opened")
		requireClosedNow(t, store, first)
		assert.False(t, cat.confirmed(0))
	})
}

// TestStoreSourceRetentionBeforeView advances the boundary past a listed hour without packs
// after its last listing, the staging listing:
// the sample before its view finds it removed, and the catalog is not asked to confirm it.
func TestStoreSourceRetentionBeforeView(t *testing.T) {
	t.Parallel()

	store, cat := listedStore()
	p := newTestRetention(memTestHour)
	staging := stagingDir(t)
	store.afterList = func(prefix, _ string, _ *ObjectPage) error {
		if prefix == staging {
			p.set(memTestHour + 1)
		}

		return nil
	}
	o, err := observeRetention(t, store, cat, p, 2, nil)
	require.NoError(t, err)
	requireRemovedScope(t, o, 0)
	requireScopeIDs(t, o, 1)
	assert.False(t, cat.confirmed(0), "a removed hour is not confirmed")
	assert.Zero(t, store.openEvents())
}

// TestStoreSourceRetentionFinal advances the boundary while the catalog confirms the last listed hour,
// after every view is fixed:
// past an indexed hour holding a pack and a listed hour holding a committed archive, and short of the next hour.
// The final sample finds both removed:
// their views are discarded, their objects closed before Observe returns, their charges released;
// the next hour keeps its view.
func TestStoreSourceRetentionFinal(t *testing.T) {
	t.Parallel()

	store, cat := newMemStore(), newFakeCatalog()
	indexed := store.putPack(t, memTestHour, retentionSegment(t, seg0, 0, 1))
	cat.setView(captureLow, memTestHour, indexed)
	cat.setIndexed(captureLow, memTestHour+1, false)
	archive := store.putPack(t, memTestHour+1, retentionArchive(t, memA, setA, 1, 1, 10)).Key
	store.putCommit(t, captureLow, memTestHour+1, setA)
	kept := store.putPack(t, memTestHour+2, retentionSegment(t, seg2, 2, 20))
	cat.setView(captureLow, memTestHour+2, kept)
	p := newTestRetention(memTestHour)
	cat.beforeConfirm = func(hour int64) error {
		if hour == memTestHour+1 {
			p.set(memTestHour + 2)
		}

		return nil
	}
	o, err := observeRetention(t, store, cat, p, 3, nil)
	require.NoError(t, err)
	requireRemovedScope(t, o, 0)
	requireRemovedScope(t, o, 1)
	requireScopeIDs(t, o, 2, seg2)
	requireClosedNow(t, store, indexed.Key, archive)
	assert.True(t, cat.confirmed(1), "the hour was confirmed before the final sample")
	assert.Equal(t, storeRecount(o), o.charged, "the removed hours' readers are released")
}

// TestStoreSourceRetentionExactBudget holds the readers of indexed hours,
// then advances the boundary past them while the last is read, one hour, then two at once;
// the next hour's pack, of many blocks, costs more than the readers held.
// MaxSourceBytes is the peak of observing the next hour with the held hours removed from the start,
// plus the slot of each object they opened, which stays charged until the observation is built.
// The sample before the next hour's open releases the removed hours' readers before the open reserves its own,
// so Observe succeeds;
// one byte less fails it with ErrReadLimit.
func TestStoreSourceRetentionExactBudget(t *testing.T) {
	t.Parallel()

	for _, held := range []int64{1, 2} {
		// observe observes the held hours and the next one, the boundary at first memTestHour + from, with MaxSourceBytes limit.
		observe := func(t testing.TB, from, limit int64) (*storeObservation, error) {
			t.Helper()

			store, cat := newMemStore(), newFakeCatalog()
			p := newTestRetention(memTestHour + from)
			for h := range held {
				pack := store.putPack(t, memTestHour+h, retentionSegment(t, UUID{0x50, byte(h)}, h, uint64(10*h+1)))
				cat.setView(captureLow, memTestHour+h, pack)
				if h == held-1 {
					store.readHook = func(key string, _ int64, _ int) error {
						if key == pack.Key {
							p.set(memTestHour + held)
						}

						return nil
					}
				}
			}
			steps := make([]footerTestStep, 0, 32)
			for i := range uint64(32) {
				steps = append(steps, txBlock(txRecord(100+i, nil))...)
			}
			next := store.putPack(t, memTestHour+held, txPackIn(t, UUID{0x50, byte(held)}, held, nil, steps))
			cat.setView(captureLow, memTestHour+held, next)

			return observeRetention(t, store, cat, p, held+1, func(o *StoreSourceOptions) { o.MaxSourceBytes = limit })
		}

		alone, err := observe(t, held, 0)
		require.NoError(t, err)
		limit := alone.peak + held*storeObjectSlotCharge
		o, err := observe(t, 0, limit)
		require.NoError(t, err, "%d hours held, MaxSourceBytes %d", held, limit)
		for h := range held {
			requireRemovedScope(t, o, h)
		}
		requireScopeIDs(t, o, held, UUID{0x50, byte(held)})
		assert.Equal(t, alone.charged, o.charged)
		_, err = observe(t, 0, limit-1)
		require.ErrorIs(t, err, ErrReadLimit, "%d hours held, MaxSourceBytes %d", held, limit-1)
	}
}

// TestStoreSourceRetentionGone deletes objects of an hour just after the boundary passed it,
// at the open of one of them or after:
// an indexed segment at its open and at its first read, a listed archive at its open and at its first read,
// and a listed hour's segment after its head was read.
// The object's hour is known, and the sample after the failure finds it removed:
// the scope is reported removed, Observe succeeds.
// The boundary not passing the hour, or no boundary, the failure stands, wrapping ErrObjectNotFound.
func TestStoreSourceRetentionGone(t *testing.T) {
	t.Parallel()

	type deletion struct {
		name string
		// put puts the packs of memTestHour, the one deleted under the key it returns.
		put func(t testing.TB, store *memStore, cat *fakeCatalog) string
		// gone deletes the object of key, running advance first; the deletion hooks are set on store.
		gone func(store *memStore, key string, advance func())
	}
	putIndexed := func(t testing.TB, store *memStore, cat *fakeCatalog) string {
		t.Helper()

		pack := store.putPack(t, memTestHour, retentionSegment(t, seg0, 0, 1))
		cat.setView(captureLow, memTestHour, pack)

		return pack.Key
	}
	putArchive := func(t testing.TB, store *memStore, cat *fakeCatalog) string {
		t.Helper()

		cat.setIndexed(captureLow, memTestHour, false)
		store.putCommit(t, captureLow, memTestHour, setA)

		return store.putPack(t, memTestHour, retentionArchive(t, memA, setA, 0, 1, 1)).Key
	}
	putSegment := func(t testing.TB, store *memStore, cat *fakeCatalog) string {
		t.Helper()

		cat.setIndexed(captureLow, memTestHour, false)

		return store.putPack(t, memTestHour, retentionSegment(t, seg0, 0, 1)).Key
	}
	atOpen := func(store *memStore, key string, advance func()) {
		store.openHook = func(_ context.Context, k string) (bool, Object, error) {
			if k == key {
				advance()
				store.remove(k)
			}

			return false, nil, nil
		}
	}
	atFirstRead := func(store *memStore, key string, advance func()) {
		store.openHook = func(_ context.Context, k string) (bool, Object, error) {
			if k != key {
				return false, nil, nil
			}
			obj := store.openObject(k)
			advance()
			store.remove(k)

			return true, obj, nil
		}
	}
	afterHead := func(store *memStore, key string, advance func()) {
		heads := 0
		store.readHook = func(k string, off int64, _ int) error {
			// The head read and the full open each start at offset 0, over one object.
			if k == key && off == 0 {
				if heads++; heads == 2 {
					advance()
					store.remove(k)
				}
			}

			return nil
		}
	}
	deletions := []deletion{
		{name: "indexed segment at its open", put: putIndexed, gone: atOpen},
		{name: "indexed segment at its first read", put: putIndexed, gone: atFirstRead},
		{name: "listed archive at its open", put: putArchive, gone: atOpen},
		{name: "listed archive at its first read", put: putArchive, gone: atFirstRead},
		{name: "listed segment after its head", put: putSegment, gone: afterHead},
	}
	for _, d := range deletions {
		t.Run(d.name, func(t *testing.T) {
			t.Parallel()

			for _, c := range []struct {
				name     string
				p        *testRetention
				to       int64
				removed  bool
				provider bool
			}{
				{name: "removed", p: newTestRetention(memTestHour), to: memTestHour + 1, removed: true, provider: true},
				{name: "retained", p: newTestRetention(memTestHour), to: memTestHour, provider: true},
				{name: "no boundary"},
			} {
				store, cat := newMemStore(), newFakeCatalog()
				key := d.put(t, store, cat)
				var p Retention
				advance := func() {}
				if c.provider {
					p = c.p
					advance = func() { c.p.set(c.to) }
				}
				d.gone(store, key, advance)
				o, err := observeRetention(t, store, cat, p, 2, nil)
				if !c.removed {
					require.ErrorIs(t, err, ErrObjectNotFound, c.name)
					require.ErrorContains(t, err, key, c.name)

					continue
				}
				require.NoError(t, err, c.name)
				requireRemovedScope(t, o, 0)
				requireScopeIDs(t, o, 1)
				assert.False(t, cat.confirmed(0), c.name)
			}
		})
	}
}

// TestStoreSourceRetentionGoneSegment deletes a staging segment before its head is read, its hour unknown,
// with the boundary advancing past the listed hour of the segment at its open:
// with another listed hour retained, Observe fails with ErrObjectNotFound;
// with every listed hour removed, it succeeds, and the next segment is not opened.
// A boundary that passes every listed hour while a segment is read stops the opens of the segments after it.
func TestStoreSourceRetentionGoneSegment(t *testing.T) {
	t.Parallel()

	build := func(t testing.TB, listed int64) (*memStore, *fakeCatalog, string, string) {
		t.Helper()

		store, cat := newMemStore(), newFakeCatalog()
		for h := range listed {
			cat.setIndexed(captureLow, memTestHour+h, false)
		}
		first := store.putPack(t, memTestHour, retentionSegment(t, seg0, 0, 1)).Key
		next := store.putPack(t, memTestHour, retentionSegment(t, seg1, 0, 2)).Key

		return store, cat, first, next
	}

	t.Run("another listed hour retained", func(t *testing.T) {
		t.Parallel()

		store, cat, first, _ := build(t, 2)
		p := newTestRetention(memTestHour)
		store.openHook = func(_ context.Context, k string) (bool, Object, error) {
			if k == first {
				p.set(memTestHour + 1)
				store.remove(k)
			}

			return false, nil, nil
		}
		_, err := observeRetention(t, store, cat, p, 2, nil)
		require.ErrorIs(t, err, ErrObjectNotFound)
		require.ErrorContains(t, err, first)
	})
	t.Run("every listed hour removed", func(t *testing.T) {
		t.Parallel()

		for _, read := range []bool{false, true} {
			store, cat, first, next := build(t, 1)
			p := newTestRetention(memTestHour)
			store.openHook = func(_ context.Context, k string) (bool, Object, error) {
				if k != first {
					return false, nil, nil
				}
				var obj Object
				if read {
					obj = store.openObject(k)
				}
				p.set(memTestHour + 1)
				store.remove(k)
				if obj != nil {
					return true, obj, nil
				}

				return false, nil, nil
			}
			o, err := observeRetention(t, store, cat, p, 2, nil)
			require.NoError(t, err, "gone at its first read: %v", read)
			requireRemovedScope(t, o, 0)
			opens, _ := store.openCounts()
			assert.Zero(t, opens[next], "the pending segment is not opened")
		}
	})
	t.Run("boundary passing while a segment is read", func(t *testing.T) {
		t.Parallel()

		store, cat, first, next := build(t, 1)
		p := newTestRetention(memTestHour)
		store.readHook = func(k string, _ int64, _ int) error {
			if k == first {
				p.set(memTestHour + 1)
			}

			return nil
		}
		o, err := observeRetention(t, store, cat, p, 2, nil)
		require.NoError(t, err)
		requireRemovedScope(t, o, 0)
		opens, _ := store.openCounts()
		assert.Zero(t, opens[next], "the pending segment is not opened")
		requireClosedNow(t, store, first)
	})
}

// TestStoreSourceRetentionNoLowerGeneration observes a listed hour holding two committed generations,
// whose newer one's archive is deleted, after the boundary passed the hour,
// between the commit listings and the archive listing:
// the archive listing returns the older generation alone,
// and the hour is reported removed, never as the older generation's view.
// Without a boundary the same deletion fixes the older generation's view, the outcome the boundary exists to prevent.
func TestStoreSourceRetentionNoLowerGeneration(t *testing.T) {
	t.Parallel()

	build := func(t testing.TB, p *testRetention) *memStore {
		t.Helper()

		store, _ := listedStore()
		store.putPack(t, memTestHour, retentionArchive(t, memA, setA, 0, 1, 1))
		newer := store.putPack(t, memTestHour, retentionArchive(t, memB, setB, 0, 2, 1)).Key
		store.putCommit(t, captureLow, memTestHour, setA)
		store.putCommit(t, captureLow, memTestHour, setB)
		archives := archiveDir(t, memTestHour)
		store.beforeList = func(prefix, token string) {
			if prefix == archives && token == "" {
				if p != nil {
					p.set(memTestHour + 1)
				}
				store.remove(newer)
			}
		}

		return store
	}

	p := newTestRetention(memTestHour)
	store := build(t, p)
	cat := newFakeCatalog()
	cat.setIndexed(captureLow, memTestHour, false)
	o, err := observeRetention(t, store, cat, p, 2, nil)
	require.NoError(t, err)
	requireRemovedScope(t, o, 0)

	store = build(t, nil)
	cat = newFakeCatalog()
	cat.setIndexed(captureLow, memTestHour, false)
	o, err = observeRetention(t, store, cat, nil, 2, nil)
	require.NoError(t, err)
	requireScopeIDs(t, o, 0, memA)
}

// TestStoreSourceRetentionError fails the provider: at the first sample, after an indexed pack was opened,
// and at the sample after a pack is gone.
// Observe fails with the provider's error, joined after ErrObjectNotFound in the last case, every object opened closed once.
func TestStoreSourceRetentionError(t *testing.T) {
	t.Parallel()

	build := func(t testing.TB) (*memStore, *fakeCatalog, CatalogPack, CatalogPack) {
		t.Helper()

		store, cat := newMemStore(), newFakeCatalog()
		first := store.putPack(t, memTestHour, retentionSegment(t, seg0, 0, 1))
		second := store.putPack(t, memTestHour, retentionSegment(t, seg1, 0, 2))
		cat.setView(captureLow, memTestHour, first, second)

		return store, cat, first, second
	}

	t.Run("first sample", func(t *testing.T) {
		t.Parallel()

		store, cat, _, _ := build(t)
		p := newTestRetention(memTestHour)
		p.fail(errRetention)
		_, err := observeRetention(t, store, cat, p, 2, nil)
		require.ErrorIs(t, err, errRetention)
		assert.Zero(t, store.openEvents())
	})
	t.Run("after an open", func(t *testing.T) {
		t.Parallel()

		store, cat, first, second := build(t)
		p := newTestRetention(memTestHour)
		store.readHook = func(key string, _ int64, _ int) error {
			if key == first.Key {
				p.fail(errRetention)
			}

			return nil
		}
		_, err := observeRetention(t, store, cat, p, 2, nil)
		require.ErrorIs(t, err, errRetention)
		opens, _ := store.openCounts()
		assert.Equal(t, map[string]int{first.Key: 1}, opens, "%q is not opened", second.Key)
	})
	t.Run("after a pack is gone", func(t *testing.T) {
		t.Parallel()

		store, cat, first, _ := build(t)
		p := newTestRetention(memTestHour)
		store.openHook = func(_ context.Context, k string) (bool, Object, error) {
			if k == first.Key {
				p.fail(errRetention)
				store.remove(k)
			}

			return false, nil, nil
		}
		_, err := observeRetention(t, store, cat, p, 2, nil)
		require.ErrorIs(t, err, errRetention)
		require.ErrorIs(t, err, ErrObjectNotFound)
		assert.False(t, errors.Is(err, ErrRemoved), "the source never reports ErrRemoved")
	})
}

// TestStoreSourceRetentionLastCommitListing deletes a listed hour's commit object just after the boundary passed the hour,
// before the last traversal MaxCommitListings allows, which then disagrees with the one before it:
// the sample at the limit finds the hour removed, so the scope is reported removed instead of failing with ErrReadLimit.
// Without the boundary moving, the disagreement fails Observe with ErrReadLimit.
func TestStoreSourceRetentionLastCommitListing(t *testing.T) {
	t.Parallel()

	for _, advance := range []bool{true, false} {
		store, cat := newMemStore(), newFakeCatalog()
		cat.setIndexed(captureLow, memTestHour, false)
		cat.setIndexed(captureLow, memTestHour+1, false)
		store.putPack(t, memTestHour, retentionArchive(t, memA, setA, 0, 1, 1))
		store.putCommit(t, captureLow, memTestHour, setA)
		deleted := store.putCommit(t, captureLow, memTestHour, setB)
		p := newTestRetention(memTestHour)
		dir := commitDir(t, memTestHour)
		store.beforeList = func(prefix, token string) {
			// The second traversal starts after its first page's sample: only the sample at the limit sees the boundary.
			if prefix == dir && token == "" && store.listCount(dir) == 1 {
				if advance {
					p.set(memTestHour + 1)
				}
				store.remove(deleted)
			}
		}
		o, err := observeRetention(t, store, cat, p, 2, func(o *StoreSourceOptions) { o.MaxCommitListings = 2 })
		if !advance {
			require.ErrorIs(t, err, ErrReadLimit)

			continue
		}
		require.NoError(t, err)
		requireRemovedScope(t, o, 0)
		requireScopeIDs(t, o, 1)
		assert.Equal(t, 2, store.listCount(dir))
		assert.Zero(t, store.pageCount(archiveDir(t, memTestHour)), "nothing more is listed for the removed hour")
	}
}

// TestStoreSourceRetentionGoneCloseError opens an indexed pack whose Open returns an object with ErrObjectNotFound,
// the boundary passing the pack's hour, and the object's Close failing:
// the scope is removed, but the close error still fails Observe.
func TestStoreSourceRetentionGoneCloseError(t *testing.T) {
	t.Parallel()

	closeFailed := errors.New("close failed")
	store, cat := newMemStore(), newFakeCatalog()
	pack := store.putPack(t, memTestHour, retentionSegment(t, seg0, 0, 1))
	cat.setView(captureLow, memTestHour, pack)
	store.closeErr[pack.Key] = closeFailed
	p := newTestRetention(memTestHour)
	store.openHook = func(_ context.Context, k string) (bool, Object, error) {
		if k != pack.Key {
			return false, nil, nil
		}
		obj := store.openObject(k)
		p.set(memTestHour + 1)

		return true, obj, ErrObjectNotFound
	}
	_, err := observeRetention(t, store, cat, p, 2, nil)
	require.ErrorIs(t, err, closeFailed)
}
