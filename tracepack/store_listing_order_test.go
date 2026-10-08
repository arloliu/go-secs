package tracepack

import (
	"context"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// eventLog returns a copy of the store's event log.
func (s *memStore) eventLog() []memStoreEvent {
	s.mu.Lock()
	defer s.mu.Unlock()

	return slices.Clone(s.events)
}

// TestStoreSourceListingOrder uploads a segment of a listed hour during the scope's second commit traversal:
// the staging listing, which follows every commit traversal, returns it, and it is in the view.
// In the store's log, the archive and staging listings start after the last commit listing.
func TestStoreSourceListingOrder(t *testing.T) {
	t.Parallel()

	store, cat := listedStore()
	store.putCommit(t, captureLow, memTestHour, setA)
	dir := commitDir(t, memTestHour)
	traversals := 0
	store.beforeList = func(prefix, token string) {
		if prefix == dir && token == "" {
			if traversals++; traversals == 2 {
				store.putPack(t, memTestHour, storeSegment(t, seg0, nil, 1))
			}
		}
	}
	o, err := observeOne(t, store, cat, nil)
	require.NoError(t, err)
	sc, err := o.Scope(t.Context(), memTestHour)
	require.NoError(t, err)
	assert.Equal(t, []UUID{seg0}, packIDs(sc.Readers))

	lastCommit, firstArchive, firstStaging := -1, -1, -1
	for i, e := range store.eventLog() {
		if e.op != "list" {
			continue
		}
		switch e.key {
		case dir:
			lastCommit = i
		case archiveDir(t, memTestHour):
			if firstArchive < 0 {
				firstArchive = i
			}
		case stagingDir(t):
			if firstStaging < 0 {
				firstStaging = i
			}
		default:
			// The listings of other prefixes take no part.
		}
	}
	require.GreaterOrEqual(t, lastCommit, 0)
	assert.Greater(t, firstArchive, lastCommit, "the archive listing follows the commit traversals")
	assert.Greater(t, firstStaging, lastCommit, "the staging listing follows the commit traversals")
}

// TestStoreSourceListedCtxPerLoop cancels Observe's context at a point of a listed scope's acquisition,
// the adapters ignoring their context, and names the function that checks the context first:
// the check of the next page, the next commit traversal, the next segment, the next archive, the next hour's view,
// the next pack discarded and the next confirmation.
// Each is the check of the loop that would do the next step, Observe fails with the cancellation,
// no listing, open or read follows the cancellation, and every object opened is closed once.
func TestStoreSourceListedCtxPerLoop(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// setup fills store and cat and calls arm where the context is to be cancelled.
		setup func(t testing.TB, store *memStore, cat *fakeCatalog, arm func())
		want  string
	}{
		{name: "a page", want: ".traverse", setup: func(t testing.TB, store *memStore, _ *fakeCatalog, arm func()) {
			store.putPack(t, memTestHour, storeSegment(t, seg0, nil, 1))
			staging := stagingDir(t)
			store.afterList = func(prefix, token string, p *ObjectPage) error {
				if prefix == staging && token == "" {
					// An empty page with a token: only the check of the next page comes before the next listing.
					*p = ObjectPage{Next: staging}
					arm()
				}

				return nil
			}
		}},
		{name: "a commit traversal", want: ".listCommits", setup: func(t testing.TB, store *memStore, _ *fakeCatalog, arm func()) {
			dir := commitDir(t, memTestHour)
			store.afterList = func(prefix, _ string, _ *ObjectPage) error {
				if prefix == dir {
					arm()
				}

				return nil
			}
		}},
		{name: "a segment", want: ".openSegment", setup: func(t testing.TB, store *memStore, _ *fakeCatalog, arm func()) {
			first := store.putPack(t, memTestHour+3, txPackIn(t, seg2, 3, nil, txBlock(inHour(3, txRecord(30, nil))))).Key
			store.putPack(t, memTestHour+4, txPackIn(t, seg3, 4, nil, txBlock(inHour(4, txRecord(40, nil)))))
			store.closeHook = func(key string) {
				if key == first {
					arm()
				}
			}
		}},
		{name: "an archive", want: ".openArchives", setup: func(t testing.TB, store *memStore, _ *fakeCatalog, arm func()) {
			first := store.putPack(t, memTestHour, storeArchive(t, memA, setA, 1)).Key
			store.putPack(t, memTestHour, storeArchive(t, memB, setB, 1))
			store.readHook = func(key string, _ int64, _ int) error {
				if key == first {
					arm()
				}

				return nil
			}
		}},
		{name: "an hour's view", want: ".viewListed", setup: func(t testing.TB, store *memStore, cat *fakeCatalog, arm func()) {
			cat.setIndexed(captureLow, memTestHour+1, false)
			// A generation without a commit object, closed when the first hour's view is fixed.
			first := store.putPack(t, memTestHour, storeArchive(t, memA, setA, 1)).Key
			store.closeHook = func(key string) {
				if key == first {
					arm()
				}
			}
		}},
		{name: "a conflicted view's pack", want: ".fixListed", setup: func(t testing.TB, store *memStore, _ *fakeCatalog, arm func()) {
			first := store.putPack(t, memTestHour, storeArchive(t, memA, setA, 1)).Key
			store.putPack(t, memTestHour, storeArchive(t, memB, setB, 1))
			store.putCommit(t, captureLow, memTestHour, setA)
			store.putCommit(t, captureLow, memTestHour, setB)
			store.closeHook = func(key string) {
				if key == first {
					arm()
				}
			}
		}},
		{name: "a pack outside the view", want: ".fixListed", setup: func(t testing.TB, store *memStore, _ *fakeCatalog, arm func()) {
			first := store.putPack(t, memTestHour, storeArchive(t, memA, setA, 1)).Key
			store.putPack(t, memTestHour, storeArchive(t, memB, setB, 1))
			store.closeHook = func(key string) {
				if key == first {
					arm()
				}
			}
		}},
		{name: "a confirmation", want: ".confirm", setup: func(_ testing.TB, _ *memStore, cat *fakeCatalog, arm func()) {
			cat.setIndexed(captureLow, memTestHour+1, false)
			cat.beforeConfirm = func(hour int64) error {
				if hour == memTestHour {
					arm()
				}

				return nil
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store, cat := listedStore()
			store.ignoreCtx, cat.ignoreCtx = true, true
			base, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx := callerCtx{Context: base, armed: &atomic.Bool{}, first: &atomic.Value{}}
			var armedAt atomic.Int64
			armedAt.Store(-1)
			arm := func() {
				if ctx.armed.Load() {
					return
				}
				armedAt.Store(int64(len(store.eventLog())))
				cancel()
				ctx.armed.Store(true)
			}
			tt.setup(t, store, cat, arm)
			o, err := newTestStoreSource(t, store, cat, nil).Observe(ctx, captureLow, memTestHour, memTestHour+2)
			require.ErrorIs(t, err, context.Canceled)
			require.Nil(t, o)
			first, ok := ctx.first.Load().(string)
			require.True(t, ok, "the context is checked after the cancellation")
			assert.True(t, strings.HasSuffix(first, tt.want), "checked first by %s", first)
			events := store.eventLog()
			require.GreaterOrEqual(t, armedAt.Load(), int64(0))
			for _, e := range events[armedAt.Load():] {
				assert.NotContains(t, []string{"list", "open", "read"}, e.op, "%s %q after the cancellation", e.op, e.key)
			}
			requireClosedOnce(t, store)
		})
	}
}

// TestStoreSourceConfirmCutoff observes a listed scope with segments, a committed generation, a patch and a candidate outside the view,
// beside an unlisted hour's segment:
// every listing, open, read and view computation of the acquisition comes before the first confirmation.
func TestStoreSourceConfirmCutoff(t *testing.T) {
	t.Parallel()

	store, cat := listedStore()
	cat.setIndexed(captureLow, memTestHour+1, false)
	store.putPack(t, memTestHour, storeSegment(t, seg0, nil, 1))
	store.putPack(t, memTestHour, storeArchive(t, memA, setA, 2))
	store.putPack(t, memTestHour, storeArchive(t, memB, setB, 2))
	store.putCommit(t, captureLow, memTestHour, setA)
	store.putPack(t, memTestHour+1, txPackIn(t, seg1, 1, nil, txBlock(txSeqs(5)...)))
	store.putPack(t, memTestHour+3, txPackIn(t, seg2, 3, nil, txBlock(inHour(3, txRecord(30, nil)))))
	var steps []string
	cutoff := -1
	cat.beforeConfirm = func(int64) error {
		if cutoff < 0 {
			cutoff = len(store.eventLog())
		}
		steps = append(steps, "confirm")

		return nil
	}
	src := newTestStoreSource(t, store, cat, nil)
	src.activeView = func(packs []PackInfo, c CommitSet) (View, error) {
		steps = append(steps, "view")

		return ActiveView(packs, c)
	}
	observeStore(t, src, memTestHour, memTestHour+2)
	assert.Equal(t, []string{"view", "view", "confirm", "confirm"}, steps)
	require.GreaterOrEqual(t, cutoff, 0)
	for _, e := range store.eventLog()[cutoff:] {
		assert.NotContains(t, []string{"list", "open", "read"}, e.op, "%s %q after the first confirmation", e.op, e.key)
	}
}
