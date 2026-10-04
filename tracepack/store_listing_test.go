package tracepack

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// excludedLog records the packs a store source reports through OnExcluded; it is safe for concurrent use.
type excludedLog struct {
	mu   sync.Mutex
	got  []ExcludedPack
	hook func(p ExcludedPack)
}

// add records p, then runs hook when set.
func (l *excludedLog) add(p ExcludedPack) {
	l.mu.Lock()
	l.got = append(l.got, p)
	hook := l.hook
	l.mu.Unlock()
	if hook != nil {
		hook(p)
	}
}

// list returns the packs reported so far, in order.
func (l *excludedLog) list() []ExcludedPack {
	l.mu.Lock()
	defer l.mu.Unlock()

	return append([]ExcludedPack(nil), l.got...)
}

// reporting returns an option edit that reports the excluded packs to l.
func (l *excludedLog) reporting(o *StoreSourceOptions) {
	o.OnExcluded = l.add
}

// listCount returns how many traversals of prefix store started: its listings with no token.
func (s *memStore) listCount(prefix string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range s.events {
		if e.op == "list" && e.key == prefix && e.token == "" {
			n++
		}
	}

	return n
}

// commitDir returns the listing prefix of captureLow's commit objects of hour under storeTestPrefix.
func commitDir(t testing.TB, hour int64) string {
	t.Helper()

	dir, err := commitKeyDir(storeTestPrefix, memStoreTool, captureLow, hour)
	require.NoError(t, err)

	return dir
}

// archiveDir returns the listing prefix of the archive packs of hour under storeTestPrefix.
func archiveDir(t testing.TB, hour int64) string {
	t.Helper()

	dir, err := archiveKeyDir(storeTestPrefix, memStoreTool, hour)
	require.NoError(t, err)

	return dir
}

// stagingDir returns the listing prefix of captureLow's segments under storeTestPrefix.
func stagingDir(t testing.TB) string {
	t.Helper()

	dir, err := stagingKeyDir(storeTestPrefix, memStoreTool, captureLow)
	require.NoError(t, err)

	return dir
}

// listedArchive returns archive id of captureLow in memTestHour + hours, the member of generation set ranked (epoch, gen),
// holding steps moved by hours.
func listedArchive(t testing.TB, id, set UUID, hours int64, epoch, gen uint64, steps []footerTestStep) []byte {
	t.Helper()

	return txPackIn(t, id, hours, func(m *PackMeta) { generationMeta(m, set, epoch, gen, nil) }, steps)
}

// listedPatch returns patch id of captureLow in memTestHour + hours, based on base, replacing supersedes, holding steps moved by hours.
func listedPatch(t testing.TB, id UUID, hours int64, base *UUID, supersedes []UUID, steps []footerTestStep) []byte {
	t.Helper()

	return txPackIn(t, id, hours, func(m *PackMeta) { m.PackRole, m.PatchBase, m.Supersedes = PackRoleRepair, base, supersedes }, steps)
}

// TestStoreSourceListed observes scopes the catalog does not index through a StoreSource and through the memSource it mirrors,
// beside indexed ones:
// a committed generation replaced by a committed patch, beside an uncommitted patch, a rejected segment,
// an archive of the retired role and an archive of another capture;
// a conflicted pair of generations; a walked segment holding a record of another hour; and an empty hour.
// Each scope holds the same packs in the same order, not indexed, the conflicted one with no readers;
// the retired archive is reported through OnExcluded once per Observe;
// the segment of the indexed hour is opened for its head only;
// and FindTransaction over both sources returns the same result, listing cold and the scope breach,
// the store's objects each closed once.
func TestStoreSourceListed(t *testing.T) {
	t.Parallel()

	build := func(t testing.TB) *memSource {
		t.Helper()

		s := newMemSource()
		s.addPack(t, memTestHour, txPack(t, seg0, nil, txBlock(txRecord(12, nil))))
		s.stagePack(t, memTestHour+1, listedArchive(t, memA, setA, 1, 1, 1, txBlock(txSeqs(14, 15)...)))
		s.stagePack(t, memTestHour+1, listedPatch(t, patP1, 1, new(setA), []UUID{memA}, txBlock(txSeqs(14, 15)...)))
		s.stagePack(t, memTestHour+1, listedPatch(t, patP2, 1, new(setA), []UUID{memA}, txBlock(txSeqs(14)...)))
		s.stagePack(t, memTestHour+1, txPackIn(t, seg1, 1, nil, txBlock(txReply(13, nil))))
		s.stagePack(t, memTestHour+1, setMetaRole(t, listedArchive(t, memC, setC, 1, 1, 1, txBlock(txSeqs(16)...)), packRoleRetired))
		s.commit(captureLow, memTestHour+1, setA)
		s.commit(captureLow, memTestHour+1, patP1)
		s.conflict(t, memTestHour+2, true, txBlock(inHour(2, txReply(30, nil))))
		s.stagePack(t, memTestHour+3, txBreachPack(t, seg3, 3, true, txBlock(txRecord(40, nil))))
		s.setIndexed(captureLow, memTestHour, true)
		s.setIndexed(captureLow, memTestHour+4, true)

		return s
	}
	mem := build(t)
	store, cat := mirrorMemSource(t, mem, captureLow, memTestHour, memTestHour+5)
	other := withIDs(t, listedArchive(t, memD, setD, 1, 1, 1, txBlock(txSeqs(1)...)), memD, captureHigh)
	otherKey, err := ArchiveKey(storeTestPrefix, memStoreTool, captureHigh, memTestHour+1, memD)
	require.NoError(t, err)
	store.put(otherKey, other)
	var excluded excludedLog
	src := newTestStoreSource(t, store, cat, excluded.reporting)

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
		assert.Equal(t, want.Indexed, got.Indexed, "hour %d", h-memTestHour)
		assert.Equal(t, want.Conflicted, got.Conflicted, "hour %d", h-memTestHour)
	}
	sc, err := so.Scope(t.Context(), memTestHour+1)
	require.NoError(t, err)
	assert.Equal(t, []UUID{seg1, patP1}, packIDs(sc.Readers), "the patch replaced the archive; the uncommitted patch is out")
	sc, err = so.Scope(t.Context(), memTestHour+2)
	require.NoError(t, err)
	assert.Equal(t, SourceScope{Conflicted: true}, sc)
	sc, err = so.Scope(t.Context(), memTestHour+3)
	require.NoError(t, err)
	assert.Equal(t, []UUID{seg3}, packIDs(sc.Readers), "a walked segment of hour 3 holding a record of hour 0")
	retiredKey := archiveKeyAt(t, memTestHour+1, memC)
	assert.Equal(t, []ExcludedPack{{Key: retiredKey, PackID: memC, Role: packRoleRetired, Hour: memTestHour + 1}}, excluded.list())
	opens, closes := store.openCounts()
	segKey := segmentKeyAt(t, 12, seg0)
	assert.Equal(t, 2, opens[segKey], "the indexed hour's segment: its view's open and its head read")
	assert.Equal(t, 1, closes[segKey], "the head read's object closed")
	assert.Zero(t, opens[otherKey], "another capture's archive is never opened")
	assert.Equal(t, 1, closes[retiredKey])

	for _, maxScopes := range []int{1, 2, 3, 5} {
		want, err := FindTransaction(t.Context(), build(t), txKeyAt(12), TxOptions{MaxScopes: maxScopes})
		require.NoError(t, err)
		got, err := FindTransaction(t.Context(), src, txKeyAt(12), TxOptions{MaxScopes: maxScopes})
		require.NoError(t, err)
		assert.Equal(t, want, got, "MaxScopes %d", maxScopes)
		if maxScopes > 1 {
			assert.Contains(t, gapHours(got.Gaps), "cold@1")
		}
		if maxScopes == 5 {
			assert.Contains(t, gapHours(got.Gaps), "scope-breach@3/40", "the walked segment of hour 3 holds a record of hour 0")
		}
	}
	assert.Len(t, excluded.list(), 4, "one report per Observe reaching hour 1")
	require.NoError(t, so.Close())
	requireClosedOnce(t, store)
}

// TestStoreSourceCoherentPagination observes a listed scope whose commit objects change during the first traversal, in pages of one key:
// beside generation G1 committed, and the commit object of a set whose member is gone,
// a patch P based on G1 and a cancelling generation G2 of a higher rank were both validated,
// and both commit writes land late, after the first page: G2's first, behind the page cursor, then P's, ahead of it.
// The first traversal sees G1 and P without G2, a set of commit objects no instant had, whose view is P alone;
// the second differs, a third agrees with it, and the view is G2's member, never G1's lineage with P.
func TestStoreSourceCoherentPagination(t *testing.T) {
	t.Parallel()

	store, cat := newMemStore(), newFakeCatalog()
	store.pageSize = 1
	cat.setIndexed(captureLow, memTestHour, false)
	// G1 is setB and G2 setA, so G2's commit key sorts before G1's and lands behind the cursor.
	store.putPack(t, memTestHour, listedArchive(t, memB, setB, 0, 1, 1, txBlock(txSeqs(1, 2)...)))
	store.putPack(t, memTestHour, listedArchive(t, memA, setA, 0, 2, 2, txBlock(txSeqs(1, 2)...)))
	store.putPack(t, memTestHour, listedPatch(t, patP1, 0, new(setB), []UUID{memB}, txBlock(txSeqs(1, 2)...)))
	store.putCommit(t, captureLow, memTestHour, setB)
	// The incomplete set's commit key sorts last, so the first page has a token.
	store.putCommit(t, captureLow, memTestHour, UUID{0xF0})
	dir := commitDir(t, memTestHour)
	first := true
	store.afterList = func(prefix, token string, _ *ObjectPage) error {
		if prefix == dir && token == "" && first {
			first = false
			store.putCommit(t, captureLow, memTestHour, setA)
			store.putCommit(t, captureLow, memTestHour, patP1)
		}

		return nil
	}

	o, err := observeOne(t, store, cat, nil)
	require.NoError(t, err)
	sc, err := o.Scope(t.Context(), memTestHour)
	require.NoError(t, err)
	assert.Equal(t, []UUID{memA}, packIDs(sc.Readers))
	assert.False(t, sc.Indexed)
	assert.Equal(t, 3, store.listCount(dir), "the second traversal differs from the first, the third agrees")
	_, closes := store.openCounts()
	assert.Equal(t, 1, closes[archiveKeyAt(t, memTestHour, memB)], "G1's member closed")
	assert.Equal(t, 1, closes[archiveKeyAt(t, memTestHour, patP1)], "P closed")
}

// TestStoreSourceLateWrites observes a listed scope holding generation G and a patch P based on it, whose commit object lands
// before the commit traversals, between the first two, or after them, during the archive listing;
// and a segment uploaded after the commit traversals, ahead of the staging listing's cursor or behind it, in pages of one key.
// P is in the view when its commit object lands before the traversals agree, and not after;
// the segment joins the view only when the staging traversal returns it.
func TestStoreSourceLateWrites(t *testing.T) {
	t.Parallel()

	commits := []struct {
		name   string
		land   string // "before", "between" or else after
		inView bool
	}{
		{name: "before", land: "before", inView: true},
		{name: "between", land: "between", inView: true},
		{name: "after", land: "after"},
	}
	for _, tt := range commits {
		t.Run("commit "+tt.name, func(t *testing.T) {
			t.Parallel()

			store, cat := newMemStore(), newFakeCatalog()
			store.pageSize = 1
			cat.setIndexed(captureLow, memTestHour, false)
			store.putPack(t, memTestHour, listedArchive(t, memA, setA, 0, 1, 1, txBlock(txSeqs(1, 2)...)))
			store.putPack(t, memTestHour, listedPatch(t, patP1, 0, new(setA), []UUID{memA}, txBlock(txSeqs(1, 2)...)))
			store.putCommit(t, captureLow, memTestHour, setA)
			land := func() { store.putCommit(t, captureLow, memTestHour, patP1) }
			dir, archives := commitDir(t, memTestHour), archiveDir(t, memTestHour)
			traversals := 0
			switch tt.land {
			case "before":
				cat.afterSnapshot = land
			case "between":
				store.beforeList = func(prefix, token string) {
					if prefix == dir && token == "" {
						if traversals++; traversals == 2 {
							land()
						}
					}
				}
			default:
				store.beforeList = func(prefix, token string) {
					if prefix == archives && token == "" {
						land()
					}
				}
			}
			o, err := observeOne(t, store, cat, nil)
			require.NoError(t, err)
			sc, err := o.Scope(t.Context(), memTestHour)
			require.NoError(t, err)
			want := []UUID{memA}
			if tt.inView {
				want = []UUID{patP1}
			}
			assert.Equal(t, want, packIDs(sc.Readers))
			wantTraversals := 2
			if tt.land == "between" {
				wantTraversals = 3
			}
			assert.Equal(t, wantTraversals, store.listCount(dir))
		})
	}

	for _, ahead := range []bool{true, false} {
		name := "segment behind the cursor"
		if ahead {
			name = "segment ahead of the cursor"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			store, cat := newMemStore(), newFakeCatalog()
			store.pageSize = 1
			cat.setIndexed(captureLow, memTestHour, false)
			store.putPack(t, memTestHour, storeSegment(t, seg1, nil, 3))
			store.putPack(t, memTestHour, storeSegment(t, seg2, nil, 5))
			late := storeSegment(t, seg0, nil, 4)
			if !ahead {
				late = storeSegment(t, seg0, nil, 1)
			}
			staging := stagingDir(t)
			store.afterList = func(prefix, token string, _ *ObjectPage) error {
				if prefix == staging && token == "" {
					store.putPack(t, memTestHour, late)
				}

				return nil
			}
			o, err := observeOne(t, store, cat, nil)
			require.NoError(t, err)
			sc, err := o.Scope(t.Context(), memTestHour)
			require.NoError(t, err)
			want := []UUID{seg1, seg2}
			if ahead {
				want = []UUID{seg0, seg1, seg2}
			}
			assert.Equal(t, want, packIDs(sc.Readers), "ahead %v", ahead)
		})
	}
}

// TestStoreSourceConfirm observes listed scopes whose confirmation fails:
// the scope indexed before its confirmation, also when evicted again; a token the catalog no longer knows, as after a rebuild;
// lost history; another scope's token; an error.
// Each fails Observe, every object closed once.
// A scope indexed after its own confirmation, while a later hour's is pending, keeps its fixed listing view.
func TestStoreSourceConfirm(t *testing.T) {
	t.Parallel()

	refused := errors.New("confirm refused")
	tests := []struct {
		name string
		// fault breaks the confirmation of hour memTestHour+1, or of both hours when every is set.
		fault func(cat *fakeCatalog)
		every bool
		want  string
		is    error
	}{
		{name: "indexed", fault: func(cat *fakeCatalog) {
			cat.afterSnapshot = func() { cat.setIndexed(captureLow, memTestHour+1, true) }
		}, want: "does not confirm"},
		{name: "indexed and evicted", fault: func(cat *fakeCatalog) {
			cat.afterSnapshot = func() {
				cat.setIndexed(captureLow, memTestHour+1, true)
				cat.setIndexed(captureLow, memTestHour+1, false)
			}
		}, want: "does not confirm"},
		{name: "rebuilt", fault: func(cat *fakeCatalog) {
			cat.afterSnapshot = func() {
				cat.mu.Lock()
				defer cat.mu.Unlock()
				clear(cat.tokens)
			}
		}, every: true, want: "does not confirm"},
		{name: "history lost", fault: func(cat *fakeCatalog) { cat.historyLost = true }, every: true, want: "does not confirm"},
		{name: "another scope's token", fault: func(cat *fakeCatalog) {
			cat.editSnapshot = func(s *CatalogSnapshot) { s.Scopes[1].Token = s.Scopes[0].Token }
		}, want: "does not confirm"},
		{name: "an error", fault: func(cat *fakeCatalog) {
			cat.beforeConfirm = func(hour int64) error {
				if hour == memTestHour+1 {
					return refused
				}

				return nil
			}
		}, is: refused},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store, cat := newMemStore(), newFakeCatalog()
			cat.setIndexed(captureLow, memTestHour, false)
			cat.setIndexed(captureLow, memTestHour+1, false)
			store.putPack(t, memTestHour, storeSegment(t, seg0, nil, 1))
			store.putPack(t, memTestHour+1, txPackIn(t, seg1, 1, nil, txBlock(txSeqs(2)...)))
			tt.fault(cat)
			_, err := observeOne(t, store, cat, nil)
			require.Error(t, err)
			if tt.want != "" {
				require.ErrorContains(t, err, tt.want)
				hour := memTestHour + 1
				if tt.every {
					hour = memTestHour
				}
				require.ErrorContains(t, err, fmt.Sprintf("hour %d:", hour), "the first hour not confirmed")
			}
			if tt.is != nil {
				require.ErrorIs(t, err, tt.is)
			}
		})
	}

	t.Run("indexed after its confirmation", func(t *testing.T) {
		t.Parallel()

		store, cat := newMemStore(), newFakeCatalog()
		cat.setIndexed(captureLow, memTestHour, false)
		cat.setIndexed(captureLow, memTestHour+1, false)
		store.putPack(t, memTestHour, storeSegment(t, seg0, nil, 1))
		store.putPack(t, memTestHour+1, txPackIn(t, seg1, 1, nil, txBlock(txSeqs(2)...)))
		cat.beforeConfirm = func(hour int64) error {
			if hour == memTestHour+1 {
				cat.setView(captureLow, memTestHour)
			}

			return nil
		}
		o, err := observeOne(t, store, cat, nil)
		require.NoError(t, err)
		sc, err := o.Scope(t.Context(), memTestHour)
		require.NoError(t, err)
		assert.Equal(t, SourceScope{Readers: sc.Readers}, sc, "still not indexed")
		assert.Equal(t, []UUID{seg0}, packIDs(sc.Readers), "the fixed listing view, not the catalog's empty one")
		assert.Equal(t, 2, cat.confirms)
	})
}

// TestStoreSourceFixedListing fixes an observation of an indexed hour and a listed one, then, before the lookup reads,
// evicts the indexed scope, uploads a late segment holding a valid reply to the listed hour, registers a stop and a closure,
// and adds a barrier:
// the lookup answers as it does without those changes, from what was fixed.
func TestStoreSourceFixedListing(t *testing.T) {
	t.Parallel()

	setup := func(t testing.TB) (*memStore, *fakeCatalog) {
		t.Helper()

		store, cat := newMemStore(), newFakeCatalog()
		cat.setView(captureLow, memTestHour, store.putPack(t, memTestHour, txPack(t, seg0, nil, txBlock(txRecord(12, nil)))))
		cat.setIndexed(captureLow, memTestHour+1, false)
		store.putPack(t, memTestHour+1, txPackIn(t, seg1, 1, nil, txBlock(txSeqs(20)...)))

		return store, cat
	}
	store, cat := setup(t)
	want, err := FindTransaction(t.Context(), newTestStoreSource(t, store, cat, nil), txKeyAt(12), TxOptions{})
	require.NoError(t, err)
	require.Equal(t, TxIncomplete, want.Outcome, "the cold hour")

	store, cat = setup(t)
	src := afterObserveSource{PackSource: newTestStoreSource(t, store, cat, nil), after: func() {
		cat.setIndexed(captureLow, memTestHour, false)
		store.putPack(t, memTestHour+1, txPackIn(t, seg2, 1, nil, txBlock(txReply(13, nil))))
		at := blockTestHour
		cat.setEvidence(captureLow, CaptureEvidence{End: EndStopped, Closures: []EpochClosure{{Epoch: txTestEpoch, CloseSeq: 13}}})
		cat.addBarrier(Boundary{Capture: captureHigh, Seq: 1, Kind: BoundaryKindStopUnclean, GapStart: &at})
	}}
	got, err := FindTransaction(t.Context(), src, txKeyAt(12), TxOptions{})
	require.NoError(t, err)
	assert.Equal(t, want, got)
	requireClosedOnce(t, store)
}

// TestStoreSourceColdLookup reads a listed scope holding a late segment, the only hour of a lookup and beside an indexed one:
// the lookup lists cold and is incomplete when the late segment holds no reply,
// and matched beside cold when it holds the reply.
func TestStoreSourceColdLookup(t *testing.T) {
	t.Parallel()

	for _, reply := range []bool{false, true} {
		store, cat := newMemStore(), newFakeCatalog()
		cat.setView(captureLow, memTestHour, store.putPack(t, memTestHour, txPack(t, seg0, nil, txBlock(txRecord(12, nil)))))
		cat.setIndexed(captureLow, memTestHour+1, false)
		rec := txRecord(20, nil)
		if reply {
			rec = txReply(13, nil)
		}
		store.putPack(t, memTestHour+1, txPackIn(t, seg1, 1, nil, txBlock(rec)))
		res, err := FindTransaction(t.Context(), newTestStoreSource(t, store, cat, nil), txKeyAt(12), TxOptions{})
		require.NoError(t, err)
		assert.Contains(t, gapHours(res.Gaps), "cold@1", "reply %v", reply)
		want := TxIncomplete
		if reply {
			want = TxMatched
		}
		assert.Equal(t, want, res.Outcome, "reply %v", reply)
		requireClosedOnce(t, store)
	}
}
