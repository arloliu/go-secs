package tracepack

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// storeTestPrefix is the bucket prefix of the store source tests, with a repeated slash that keys keep byte for byte.
const storeTestPrefix = "bucket//traces"

// newTestStoreSource returns a StoreSource of memStoreTool over store and cat, under storeTestPrefix,
// with its options changed by edit when set;
// its OnExcluded fails the test, since no indexed pack is ever reported.
func newTestStoreSource(t testing.TB, store ObjectStore, cat Catalog, edit func(o *StoreSourceOptions)) *storeSource {
	t.Helper()

	opts := StoreSourceOptions{
		Prefix: storeTestPrefix, Tool: memStoreTool,
		OnExcluded: func(p ExcludedPack) { t.Errorf("OnExcluded(%+v) for an indexed scope", p) },
	}
	if edit != nil {
		edit(&opts)
	}
	src, err := NewStoreSource(store, cat, opts)
	require.NoError(t, err)

	return asStoreSource(t, src)
}

// asStoreSource returns src as the *storeSource NewStoreSource built.
func asStoreSource(t testing.TB, src PackSource) *storeSource {
	t.Helper()

	s, ok := src.(*storeSource)
	require.True(t, ok)

	return s
}

// asStoreObservation returns o as the *storeObservation a storeSource's Observe returned.
func asStoreObservation(t testing.TB, o Observation) *storeObservation {
	t.Helper()

	so, ok := o.(*storeObservation)
	require.True(t, ok)

	return so
}

// observeStore observes captureLow over [from, to) through src, which must succeed, its charges as recounted;
// the observation is closed when the test ends.
func observeStore(t testing.TB, src PackSource, from, to int64) *storeObservation {
	t.Helper()

	o, err := src.Observe(t.Context(), captureLow, from, to)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, o.Close()) })
	so := asStoreObservation(t, o)
	requireStoreCharges(t, so)

	return so
}

// requireStoreCharges requires that the charges of o equal their recount and cover the bytes o holds.
func requireStoreCharges(t testing.TB, o *storeObservation) {
	t.Helper()

	require.Equal(t, storeRecount(o), o.charged, "the charges recounted")
	require.GreaterOrEqual(t, o.charged, deepSize(o), "the charges cover the observation")
}

// mirrorMemSource returns a memStore and a fakeCatalog holding what the memSource m holds of capture over [from, to),
// every scope of which m indexes:
// each scope's view, in view order, conflicted when m reports or computes it so, the capture's folded evidence,
// and every stop-unclean boundary m recorded as a barrier.
func mirrorMemSource(t testing.TB, m *memSource, capture UUID, from, to int64) (*memStore, *fakeCatalog) {
	t.Helper()

	store, cat := newMemStore(), newFakeCatalog()
	m.mu.Lock()
	defer m.mu.Unlock()
	for h := from; h < to; h++ {
		k := memScopeKey{capture: capture, hour: h}
		require.True(t, m.indexed[k], "hour %d is not indexed", h)
		var files [][]byte
		var err error
		if sc := m.scopes[k]; sc != nil {
			files, err = viewFiles(t.Context(), sc.registered, sc.commits)
		}
		if m.conflicted[k] || errors.Is(err, ErrViewConflicted) {
			cat.setConflicted(capture, h, true)

			continue
		}
		require.NoError(t, err)
		packs := make([]CatalogPack, len(files))
		for i, f := range files {
			packs[i] = store.putPack(t, h, f)
		}
		cat.setView(capture, h, packs...)
	}
	cat.setEvidence(capture, m.foldEvidence(capture))
	for _, id := range slices.SortedFunc(maps.Keys(m.evidence), compareUUIDs) {
		for _, b := range m.foldEvidence(id).Boundaries {
			if b.Kind == BoundaryKindStopUnclean {
				cat.addBarrier(b)
			}
		}
	}

	return store, cat
}

// compareUUIDs orders UUIDs by their bytes.
func compareUUIDs(a, b UUID) int {
	return slices.Compare(a[:], b[:])
}

// requireClosedOnce requires that every object store opened was closed exactly once, and none read after Close.
func requireClosedOnce(t testing.TB, store *memStore) {
	t.Helper()

	opens, closes := store.openCounts()
	assert.Equal(t, opens, closes, "every object opened is closed")
	assert.Zero(t, store.misuseCount(), "no object closed twice or read after Close")
}

// TestNewStoreSource builds sources from good and bad arguments:
// a nil store, catalog or OnExcluded, a prefix ending in a slash, a tool no key can hold and a footer offset are rejected;
// other prefixes are kept byte for byte, and every limit not set takes its default.
func TestNewStoreSource(t *testing.T) {
	t.Parallel()

	good := StoreSourceOptions{Tool: memStoreTool, OnExcluded: func(ExcludedPack) {}}
	rejected := []struct {
		name  string
		store ObjectStore
		cat   Catalog
		edit  func(o *StoreSourceOptions)
		want  string
	}{
		{name: "nil store", cat: newFakeCatalog(), want: "the object store is nil"},
		{name: "nil catalog", store: newMemStore(), want: "the catalog is nil"},
		{name: "nil OnExcluded", edit: func(o *StoreSourceOptions) { o.OnExcluded = nil }, want: "OnExcluded is nil"},
		{name: "trailing slash", edit: func(o *StoreSourceOptions) { o.Prefix = "a/" }, want: `prefix "a/" ends in '/'`},
		{name: "slash only", edit: func(o *StoreSourceOptions) { o.Prefix = "/" }, want: `prefix "/" ends in '/'`},
		{name: "empty tool", edit: func(o *StoreSourceOptions) { o.Tool = "" }, want: `tool "" cannot be stored`},
		{name: "dot tool", edit: func(o *StoreSourceOptions) { o.Tool = "." }, want: `tool "." cannot be stored`},
		{name: "dot-dot tool", edit: func(o *StoreSourceOptions) { o.Tool = ".." }, want: `tool ".." cannot be stored`},
		{name: "footer offset", edit: func(o *StoreSourceOptions) { o.Reader.FooterOffset = 80 }, want: "Reader.FooterOffset 80 is a hint for one object"},
	}
	for _, tt := range rejected {
		store, cat := tt.store, tt.cat
		if store == nil && cat == nil {
			store, cat = newMemStore(), newFakeCatalog()
		}
		opts := good
		if tt.edit != nil {
			tt.edit(&opts)
		}
		src, err := NewStoreSource(store, cat, opts)
		require.ErrorContains(t, err, tt.want, tt.name)
		assert.Nil(t, src, tt.name)
	}

	for _, prefix := range []string{"", "a", "a/b", "/a", "a//b"} {
		opts := good
		opts.Prefix, opts.Tool = prefix, "a/b c"
		src, err := NewStoreSource(newMemStore(), newFakeCatalog(), opts)
		require.NoError(t, err, "prefix %q", prefix)
		got := asStoreSource(t, src).opts
		assert.Equal(t, prefix, got.Prefix)
		assert.Equal(t, "a/b c", got.Tool)
		assert.Equal(t, DefaultMaxCommitListings, got.MaxCommitListings)
		assert.Equal(t, DefaultMaxListPages, got.MaxListPages)
		assert.Equal(t, DefaultMaxObjects, got.MaxObjects)
		assert.Equal(t, int64(DefaultMaxSourceBytes), got.MaxSourceBytes)
	}

	opts := good
	opts.MaxCommitListings, opts.MaxListPages, opts.MaxObjects, opts.MaxSourceBytes = -1, 3, 4, 5
	src, err := NewStoreSource(newMemStore(), newFakeCatalog(), opts)
	require.NoError(t, err)
	got := asStoreSource(t, src).opts
	assert.Equal(t, DefaultMaxCommitListings, got.MaxCommitListings, "a negative limit takes its default")
	assert.Equal(t, 3, got.MaxListPages)
	assert.Equal(t, 4, got.MaxObjects)
	assert.Equal(t, int64(5), got.MaxSourceBytes)
}

// TestStoreSourceIndexed observes indexed scopes through a StoreSource and through the memSource it mirrors:
// a generation with a patch and segments, a walked pack holding a record of the hour before,
// a conflicted view and a scope the catalog reports conflicted, with evidence and barriers.
// Each scope holds the same packs in the same order, indexed, the conflicted ones with no readers;
// the evidence and the barriers are the same;
// and FindTransaction over both sources returns the same result, the store's objects each closed once.
func TestStoreSourceIndexed(t *testing.T) {
	t.Parallel()

	reply := func(seq uint64) Record { return txReply(seq, nil) }
	build := func(t testing.TB) *memSource {
		t.Helper()

		s := newMemSource()
		archive := txPack(t, memA, func(m *PackMeta) { generationMeta(m, setA, 1, 1, nil) }, txBlock(txSeqs(1, 2)...))
		patch := txPack(t, patP1, func(m *PackMeta) {
			m.PackRole, m.PatchBase, m.Supersedes = PackRoleRepair, new(setA), []UUID{memA}
		}, txBlock(txSeqs(1, 2)...))
		s.addPack(t, memTestHour, archive)
		s.addPack(t, memTestHour, patch)
		s.commit(captureLow, memTestHour, setA)
		s.commit(captureLow, memTestHour, patP1)
		s.addPack(t, memTestHour, txPack(t, seg0, nil, txBlock(txRecord(12, nil))))
		s.addPack(t, memTestHour, txPack(t, seg1, nil, txBlock(txSeqs(13)...)))
		s.addPack(t, memTestHour+1, txBreachPack(t, seg3, 1, true, txBlock(reply(14))))
		s.addPack(t, memTestHour+2, txPackIn(t, seg2, 2, nil, txBlock(reply(20))))
		s.conflict(t, memTestHour+3, false, txBlock(inHour(3, reply(30))))
		s.setConflicted(captureLow, memTestHour+4, true)
		for h := range int64(5) {
			s.setIndexed(captureLow, memTestHour+h, true)
		}
		at := blockTestHour + 3*hourNs
		s.addBoundary(Boundary{Capture: captureHigh, Seq: 9, Kind: BoundaryKindStopUnclean, GapStart: &at})
		s.addClosure(captureLow, txTestEpoch, 40)

		return s
	}
	mem := build(t)
	store, cat := mirrorMemSource(t, mem, captureLow, memTestHour, memTestHour+5)
	src := newTestStoreSource(t, store, cat, nil)

	mo, err := mem.Observe(t.Context(), captureLow, memTestHour, memTestHour+5)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, mo.Close()) })
	so := observeStore(t, src, memTestHour, memTestHour+5)
	for h := memTestHour; h < memTestHour+5; h++ {
		want, err := mo.Scope(t.Context(), h)
		require.NoError(t, err)
		got, err := so.Scope(t.Context(), h)
		require.NoError(t, err)
		assert.Equal(t, packIDs(want.Readers), packIDs(got.Readers), "hour %d", h-memTestHour)
		assert.True(t, got.Indexed, "hour %d", h-memTestHour)
		assert.Equal(t, want.Conflicted, got.Conflicted, "hour %d", h-memTestHour)
	}
	sc, err := so.Scope(t.Context(), memTestHour)
	require.NoError(t, err)
	assert.Equal(t, []UUID{seg0, seg1, patP1}, packIDs(sc.Readers), "the patch replaced the archive")
	for _, h := range []int64{3, 4} {
		sc, err := so.Scope(t.Context(), memTestHour+h)
		require.NoError(t, err)
		assert.Equal(t, SourceScope{Indexed: true, Conflicted: true}, sc, "hour %d", h)
	}
	wantEvidence, err := mo.Evidence(t.Context())
	require.NoError(t, err)
	gotEvidence, err := so.Evidence(t.Context())
	require.NoError(t, err)
	assert.Equal(t, wantEvidence, gotEvidence)
	wantBarriers, err := mo.Barriers(t.Context(), blockTestHour, blockTestHour+5*hourNs)
	require.NoError(t, err)
	gotBarriers, err := so.Barriers(t.Context(), blockTestHour, blockTestHour+5*hourNs)
	require.NoError(t, err)
	assert.Equal(t, wantBarriers, gotBarriers)
	require.Len(t, gotBarriers, 1)

	for _, maxScopes := range []int{1, 2, 3, 5} {
		want, err := FindTransaction(t.Context(), build(t), txKeyAt(12), TxOptions{MaxScopes: maxScopes})
		require.NoError(t, err)
		got, err := FindTransaction(t.Context(), src, txKeyAt(12), TxOptions{MaxScopes: maxScopes})
		require.NoError(t, err)
		assert.Equal(t, want, got, "MaxScopes %d", maxScopes)
		if maxScopes > 1 {
			assert.Contains(t, gapHours(got.Gaps), "scope-breach@1/14", "the walked pack of hour 1 holds a reply of hour 0")
		}
	}
	require.NoError(t, so.Close())
	requireClosedOnce(t, store)
}

// TestStoreSourceObserveArguments observes a zero capture, empty and reversed hours, and hours beyond a lookup's:
// each fails before the catalog is asked.
// The widest range a lookup reads, [MinTxHour, MaxTxHour] in full, passes to the catalog.
func TestStoreSourceObserveArguments(t *testing.T) {
	t.Parallel()

	cat := newFakeCatalog()
	cat.snapshotErr = errors.New("snapshot refused")
	src := newTestStoreSource(t, newMemStore(), cat, nil)
	tests := []struct {
		name     string
		capture  UUID
		from, to int64
		want     string
	}{
		{name: "zero capture", from: 0, to: 1, want: "the capture_id is zero"},
		{name: "empty", capture: captureLow, from: 1, to: 1, want: "are empty"},
		{name: "reversed", capture: captureLow, from: 2, to: 1, want: "are empty"},
		{name: "before MinTxHour", capture: captureLow, from: MinTxHour - 1, to: 0, want: "not within"},
		{name: "after MaxTxHour", capture: captureLow, from: 0, to: MaxTxHour + 2, want: "not within"},
		{name: "the widest", capture: captureLow, from: MinTxHour, to: MaxTxHour + 1, want: "snapshot refused"},
	}
	for _, tt := range tests {
		o, err := src.Observe(t.Context(), tt.capture, tt.from, tt.to)
		require.ErrorContains(t, err, tt.want, tt.name)
		assert.Nil(t, o, tt.name)
	}
	assert.Equal(t, 1, cat.snapshots)
}

// TestStoreSourceObservationErrors asks an observation for an hour outside it and after Close:
// both fail, and Close stays idempotent, closing each object once.
func TestStoreSourceObservationErrors(t *testing.T) {
	t.Parallel()

	store, cat := newMemStore(), newFakeCatalog()
	cat.setView(captureLow, memTestHour, store.putPack(t, memTestHour, txPack(t, seg0, nil, txBlock(txSeqs(1)...))))
	o, err := newTestStoreSource(t, store, cat, nil).Observe(t.Context(), captureLow, memTestHour, memTestHour+1)
	require.NoError(t, err)

	_, err = o.Scope(t.Context(), memTestHour+1)
	require.ErrorContains(t, err, "outside the observation's hours")
	_, err = o.Scope(t.Context(), memTestHour-1)
	require.ErrorContains(t, err, "outside the observation's hours")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = o.Evidence(ctx)
	require.ErrorIs(t, err, context.Canceled)

	require.NoError(t, o.Close())
	require.NoError(t, o.Close())
	_, err = o.Scope(t.Context(), memTestHour)
	require.ErrorContains(t, err, "closed")
	_, err = o.Evidence(t.Context())
	require.ErrorContains(t, err, "closed")
	_, err = o.Barriers(t.Context(), blockTestHour, blockTestHour+1)
	require.ErrorContains(t, err, "closed")
	requireClosedOnce(t, store)
}

// TestStoreSourceBarriersRange asks an observation of two hours for its barriers over ranges within its hours and beyond them,
// as TestMemSourceBarriersRange asks a memSource's:
// the exact interval and strict subranges answer, with the unbounded and inverted barriers and those of another capture;
// a range with either end outside the hours, an empty range and a reversed one are errors.
func TestStoreSourceBarriersRange(t *testing.T) {
	t.Parallel()

	at := func(v int64) *int64 { return &v }
	from, to := blockTestHour, blockTestHour+2*hourNs
	unbounded := Boundary{Capture: captureLow, Seq: 1, Kind: BoundaryKindStopUnclean}
	inverted := Boundary{Capture: captureHigh, Seq: 2, Kind: BoundaryKindStopUnclean, GapStart: at(to + 5), GapEnd: at(from - 5)}
	second := Boundary{Capture: captureHigh, Seq: 3, Kind: BoundaryKindStopUnclean, GapStart: at(from + hourNs), GapEnd: at(from + hourNs)}
	cat := newFakeCatalog()
	for _, b := range []Boundary{unbounded, inverted, second} {
		cat.addBarrier(b)
	}
	o := observeStore(t, newTestStoreSource(t, newMemStore(), cat, nil), memTestHour, memTestHour+2)

	tests := []struct {
		name     string
		from, to int64
		want     []Boundary
		err      bool
	}{
		{name: "the observation's hours", from: from, to: to, want: []Boundary{unbounded, inverted, second}},
		{name: "the first hour", from: from, to: from + hourNs, want: []Boundary{unbounded, inverted}},
		{name: "the second hour", from: from + hourNs, to: to, want: []Boundary{unbounded, inverted, second}},
		{name: "one ns inside", from: from + 1, to: from + 2, want: []Boundary{unbounded, inverted}},
		{name: "the last ns", from: to - 1, to: to, want: []Boundary{unbounded, inverted}},
		{name: "starting before", from: from - 1, to: to, err: true},
		{name: "ending after", from: from, to: to + 1, err: true},
		{name: "wholly before", from: from - hourNs, to: from, err: true},
		{name: "wholly after", from: to, to: to + hourNs, err: true},
		{name: "empty", from: from + 5, to: from + 5, err: true},
		{name: "reversed", from: from + 6, to: from + 5, err: true},
	}
	for _, tt := range tests {
		got, err := o.Barriers(t.Context(), tt.from, tt.to)
		if tt.err {
			require.ErrorContains(t, err, "is empty or not within", tt.name)
			assert.Nil(t, got, tt.name)

			continue
		}
		require.NoError(t, err, tt.name)
		assert.Equal(t, tt.want, got, tt.name)
	}

	// The widest observation's range reaches both ends of the lookup's hours without overflow.
	wide := observeStore(t, newTestStoreSource(t, newMemStore(), cat, nil), MinTxHour, MinTxHour+1)
	got, err := wide.Barriers(t.Context(), MinTxHour*hourNs, (MinTxHour+1)*hourNs)
	require.NoError(t, err)
	assert.Equal(t, []Boundary{unbounded, inverted}, got)
	wide = observeStore(t, newTestStoreSource(t, newMemStore(), cat, nil), MaxTxHour, MaxTxHour+1)
	got, err = wide.Barriers(t.Context(), MaxTxHour*hourNs, (MaxTxHour+1)*hourNs)
	require.NoError(t, err)
	assert.Equal(t, []Boundary{unbounded, inverted}, got)
}

// TestStoreSourceOwnership changes the adapters' state after Observe returned —
// the catalog's views, evidence and barriers, and the snapshot it returned, which is the source's own —
// then mutates every value the observation returned and asks again:
// the answers are unchanged, the source's copies its own, each Readers slice and boundary pointer fresh.
func TestStoreSourceOwnership(t *testing.T) {
	t.Parallel()

	store, cat := newMemStore(), newFakeCatalog()
	cat.setView(captureLow, memTestHour, store.putPack(t, memTestHour, txPack(t, seg0, nil, txBlock(txSeqs(1)...))))
	gapStart := blockTestHour + 7
	cat.setEvidence(captureLow, CaptureEvidence{
		End: EndStoppedUnclean, Closures: []EpochClosure{{Epoch: 1, CloseSeq: 1}},
		Boundaries: []Boundary{{Capture: captureLow, Seq: 1, Kind: BoundaryKindStopUnclean, GapStart: &gapStart}},
	})
	cat.addBarrier(Boundary{Capture: captureLow, Seq: 1, Kind: BoundaryKindStopUnclean, GapStart: &gapStart})
	var returned CatalogSnapshot
	cat.editSnapshot = func(snap *CatalogSnapshot) { returned = *snap }
	o := observeStore(t, newTestStoreSource(t, store, cat, nil), memTestHour, memTestHour+1)

	cat.setView(captureLow, memTestHour)
	cat.setEvidence(captureLow, CaptureEvidence{})
	cat.addBarrier(Boundary{Capture: captureHigh, Seq: 5, Kind: BoundaryKindStopUnclean})
	returned.Scopes[0].Packs[0].PackID = seg1
	*returned.Evidence.Boundaries[0].GapStart = 99
	returned.Evidence.Closures[0].CloseSeq = 99
	*returned.Barriers[0].GapStart = 99

	check := func() {
		t.Helper()

		sc, err := o.Scope(t.Context(), memTestHour)
		require.NoError(t, err)
		assert.Equal(t, []UUID{seg0}, packIDs(sc.Readers))
		e, err := o.Evidence(t.Context())
		require.NoError(t, err)
		assert.Equal(t, EndStoppedUnclean, e.End)
		assert.Equal(t, uint64(1), e.Boundaries[0].Seq)
		assert.Equal(t, blockTestHour+7, *e.Boundaries[0].GapStart)
		assert.Equal(t, uint64(1), e.Closures[0].CloseSeq)
		b, err := o.Barriers(t.Context(), blockTestHour, blockTestHour+hourNs)
		require.NoError(t, err)
		require.Len(t, b, 1)
		assert.Equal(t, blockTestHour+7, *b[0].GapStart)
	}
	check()

	sc, err := o.Scope(t.Context(), memTestHour)
	require.NoError(t, err)
	sc.Readers[0] = nil
	e, err := o.Evidence(t.Context())
	require.NoError(t, err)
	*e.Boundaries[0].GapStart = 98
	e.Boundaries[0].Seq = 98
	e.Closures[0].CloseSeq = 98
	b, err := o.Barriers(t.Context(), blockTestHour, blockTestHour+hourNs)
	require.NoError(t, err)
	*b[0].GapStart = 98
	check()
}

// TestStoreSourceConcurrent runs observations and lookups of one source concurrently, sharing its adapters:
// each gets its own observation, and the race detector finds no shared state.
func TestStoreSourceConcurrent(t *testing.T) {
	t.Parallel()

	mem := txHours(t, [][]byte{txPack(t, seg0, nil, txBlock(txRecord(12, nil), txReply(13, nil)))}, nil)
	store, cat := mirrorMemSource(t, mem, captureLow, memTestHour, memTestHour+2)
	src := newTestStoreSource(t, store, cat, nil)
	want, err := FindTransaction(t.Context(), mem, txKeyAt(12), TxOptions{})
	require.NoError(t, err)

	var wg sync.WaitGroup
	results := make([]TxResult, 8)
	errs := make([]error, len(results))
	for i := range results {
		wg.Go(func() { results[i], errs[i] = FindTransaction(t.Context(), src, txKeyAt(12), TxOptions{}) })
	}
	wg.Wait()
	for i := range results {
		require.NoError(t, errs[i])
		assert.Equal(t, want, results[i])
	}
	requireClosedOnce(t, store)
}

// afterObserveSource is a PackSource that runs after once each Observe of its source succeeded.
type afterObserveSource struct {
	PackSource
	after func()
}

// Observe observes through the source, then runs after on success.
func (s afterObserveSource) Observe(ctx context.Context, capture UUID, from, to int64) (Observation, error) {
	o, err := s.PackSource.Observe(ctx, capture, from, to)
	if err == nil {
		s.after()
	}

	return o, err
}

// TestStoreSourceAfterAcquisition deletes a retained pack after Observe returned, before the lookup reads it:
// the lookup's block read fails with ErrObjectNotFound, the outcome zero, and the observation's objects are closed once.
func TestStoreSourceAfterAcquisition(t *testing.T) {
	t.Parallel()

	store, cat := newMemStore(), newFakeCatalog()
	p := store.putPack(t, memTestHour, txPack(t, seg0, nil, txBlock(txRecord(12, nil), txReply(13, nil))))
	cat.setView(captureLow, memTestHour, p)
	src := afterObserveSource{PackSource: newTestStoreSource(t, store, cat, nil), after: func() { store.remove(p.Key) }}

	res, err := FindTransaction(t.Context(), src, txKeyAt(12), TxOptions{})
	require.ErrorIs(t, err, ErrObjectNotFound)
	assert.Zero(t, res.Outcome)
	opens, _ := store.openCounts()
	assert.Equal(t, map[string]int{p.Key: 1}, opens)
	requireClosedOnce(t, store)
}
