package tracepack

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// storeSegment returns segment id of captureLow in memTestHour holding seqs, its pack metadata changed by meta when set.
func storeSegment(t testing.TB, id UUID, meta func(m *PackMeta), seqs ...uint64) []byte {
	t.Helper()

	return txPack(t, id, meta, txBlock(txSeqs(seqs...)...))
}

// storeArchive returns archive id of captureLow in memTestHour, the member of generation set, holding seqs.
func storeArchive(t testing.TB, id, set UUID, seqs ...uint64) []byte {
	t.Helper()

	return storeSegment(t, id, func(m *PackMeta) { generationMeta(m, set, 1, 1, nil) }, seqs...)
}

// putAt stores file under key and returns its descriptor, its pack_id the file header's.
func (s *memStore) putAt(t testing.TB, key string, file []byte) CatalogPack {
	t.Helper()

	s.put(key, file)

	return CatalogPack{Key: key, PackID: mustOpen(t, file, ReaderOptions{}).Header().PackID, Size: int64(len(file))}
}

// putCommit stores the commit object of id in capture's scope of hour under storeTestPrefix and returns its key.
func (s *memStore) putCommit(t testing.TB, capture UUID, hour int64, id UUID) string {
	t.Helper()

	key, err := CommitKey(storeTestPrefix, memStoreTool, capture, hour, id)
	require.NoError(t, err)
	s.put(key, nil)

	return key
}

// segmentKeyAt returns the key of a segment of captureLow under storeTestPrefix.
func segmentKeyAt(t testing.TB, seqFirst uint64, pack UUID) string {
	t.Helper()

	key, err := SegmentKey(storeTestPrefix, memStoreTool, captureLow, seqFirst, pack)
	require.NoError(t, err)

	return key
}

// archiveKeyAt returns the key of an archive of captureLow in hour under storeTestPrefix.
func archiveKeyAt(t testing.TB, hour int64, pack UUID) string {
	t.Helper()

	key, err := ArchiveKey(storeTestPrefix, memStoreTool, captureLow, hour, pack)
	require.NoError(t, err)

	return key
}

// observeOne observes captureLow over memTestHour and the next hour through a store source over store and cat,
// with its options changed by edit when set.
// On error it requires that no observation is returned and every object opened is closed once;
// on success, that the charges are as recounted and cover the observation.
func observeOne(t testing.TB, store *memStore, cat *fakeCatalog, edit func(o *StoreSourceOptions)) (*storeObservation, error) {
	t.Helper()

	o, err := newTestStoreSource(t, store, cat, edit).Observe(t.Context(), captureLow, memTestHour, memTestHour+2)
	if err != nil {
		require.Nil(t, o)
		requireClosedOnce(t, store)

		return nil, err
	}
	t.Cleanup(func() { assert.NoError(t, o.Close()) })
	so := asStoreObservation(t, o)
	requireStoreCharges(t, so)

	return so, nil
}

// TestStoreSourceSnapshotChecks observes snapshots that break the catalog's contract:
// a scope missing or out of order, a conflicted scope not indexed, packs where none may be, a scope not indexed without a token,
// and in a view a repeated pack_id or key, a key that does not parse, and a key of another tool, capture or hour.
// Each fails Observe before any object is opened; a key that does not parse wraps ErrInvalidKey.
func TestStoreSourceSnapshotChecks(t *testing.T) {
	t.Parallel()

	// upper spells the pack_id of key, an archive key whose pack_id holds hex letters, in uppercase.
	upper := func(key string) string {
		i := len(key) - len(keyPackSuffix) - uuidKeyLen

		return key[:i] + strings.ToUpper(key[i:len(key)-len(keyPackSuffix)]) + keyPackSuffix
	}
	tests := []struct {
		name    string
		edit    func(t testing.TB, snap *CatalogSnapshot)
		want    string
		invalid bool
	}{
		{name: "a scope missing", edit: func(_ testing.TB, s *CatalogSnapshot) { s.Scopes = s.Scopes[:1] }, want: "holds 1 scopes for the 2 hours"},
		{
			name: "scopes out of order",
			edit: func(_ testing.TB, s *CatalogSnapshot) { s.Scopes[0], s.Scopes[1] = s.Scopes[1], s.Scopes[0] },
			want: "comes where hour",
		},
		{
			name: "conflicted, not indexed",
			edit: func(_ testing.TB, s *CatalogSnapshot) {
				s.Scopes[1] = CatalogScope{Hour: s.Scopes[1].Hour, Conflicted: true, Token: "t"}
			},
			want: "does not index as conflicted",
		},
		{
			name: "packs of a scope not indexed",
			edit: func(_ testing.TB, s *CatalogSnapshot) {
				s.Scopes[0].Indexed, s.Scopes[0].Token = false, "t"
			},
			want: "lists packs of a scope it does not index",
		},
		{
			name: "packs of a conflicted scope",
			edit: func(_ testing.TB, s *CatalogSnapshot) { s.Scopes[0].Conflicted = true },
			want: "lists packs of a scope it does not index or reports conflicted",
		},
		{
			name: "no token",
			edit: func(_ testing.TB, s *CatalogSnapshot) { s.Scopes[1].Indexed = false },
			want: "gives no token",
		},
		{
			name: "a repeated pack_id",
			edit: func(_ testing.TB, s *CatalogSnapshot) { s.Scopes[0].Packs[1].PackID = s.Scopes[0].Packs[0].PackID },
			want: "lists pack_id 50000000-0000-0000-0000-000000000000 twice",
		},
		{
			name: "a repeated key",
			edit: func(_ testing.TB, s *CatalogSnapshot) { s.Scopes[0].Packs[1].Key = s.Scopes[0].Packs[0].Key },
			want: "twice",
		},
		{
			name:    "an uppercase pack_id",
			edit:    func(_ testing.TB, s *CatalogSnapshot) { s.Scopes[0].Packs[1].Key = upper(s.Scopes[0].Packs[1].Key) },
			want:    "not in lowercase",
			invalid: true,
		},
		{
			name: "an uppercase capture_id",
			edit: func(t testing.TB, s *CatalogSnapshot) {
				s.Scopes[0].Packs[0].Key = strings.Replace(s.Scopes[0].Packs[0].Key, captureLow.String(), strings.ToUpper(captureLow.String()), 1)
			},
			want: "not in lowercase", invalid: true,
		},
		{
			name:    "an extra suffix",
			edit:    func(_ testing.TB, s *CatalogSnapshot) { s.Scopes[0].Packs[0].Key += ".bak" },
			want:    "not <seq_first>-<pack_id>.tpk",
			invalid: true,
		},
		{
			name: "an extra component",
			edit: func(_ testing.TB, s *CatalogSnapshot) {
				k := s.Scopes[0].Packs[1].Key
				i := strings.LastIndex(k, "/")
				s.Scopes[0].Packs[1].Key = k[:i] + "/x" + k[i:]
			},
			want: "7 components", invalid: true,
		},
		{
			name: "a malformed escape",
			edit: func(_ testing.TB, s *CatalogSnapshot) {
				s.Scopes[0].Packs[0].Key = strings.Replace(s.Scopes[0].Packs[0].Key, "/"+memStoreTool+"/", "/to%2fol/", 1)
			},
			want: "is not two uppercase hex digits", invalid: true,
		},
		{
			name: "a commit key",
			edit: func(t testing.TB, s *CatalogSnapshot) {
				key, err := CommitKey(storeTestPrefix, memStoreTool, captureLow, memTestHour, seg0)
				require.NoError(t, err)
				s.Scopes[0].Packs[0].Key = key
			},
			want: "not under", invalid: true,
		},
		{
			name: "another prefix",
			edit: func(_ testing.TB, s *CatalogSnapshot) {
				s.Scopes[0].Packs[0].Key = strings.TrimPrefix(s.Scopes[0].Packs[0].Key, "bucket/")
			},
			want: "not under", invalid: true,
		},
		{
			name: "another tool",
			edit: func(t testing.TB, s *CatalogSnapshot) {
				key, err := SegmentKey(storeTestPrefix, "other", captureLow, 1, seg0)
				require.NoError(t, err)
				s.Scopes[0].Packs[0].Key = key
			},
			want: `names tool "other", not "tool"`,
		},
		{
			name: "another capture",
			edit: func(t testing.TB, s *CatalogSnapshot) {
				key, err := ArchiveKey(storeTestPrefix, memStoreTool, captureHigh, memTestHour, memA)
				require.NoError(t, err)
				s.Scopes[0].Packs[1].Key = key
			},
			want: "names capture " + captureHigh.String(),
		},
		{
			name: "another hour",
			edit: func(t testing.TB, s *CatalogSnapshot) {
				s.Scopes[0].Packs[1].Key = archiveKeyAt(t, memTestHour+1, memA)
			},
			want: "names hour",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store, cat := newMemStore(), newFakeCatalog()
			cat.setView(captureLow, memTestHour,
				store.putPack(t, memTestHour, storeSegment(t, seg0, nil, 1)),
				store.putPack(t, memTestHour, storeArchive(t, memA, setA, 2)))
			cat.editSnapshot = func(s *CatalogSnapshot) { tt.edit(t, s) }
			_, err := observeOne(t, store, cat, nil)
			require.ErrorContains(t, err, tt.want)
			assert.Equal(t, tt.invalid, errors.Is(err, ErrInvalidKey), "wraps ErrInvalidKey")
			opens, _ := store.openCounts()
			assert.Empty(t, opens, "nothing opened")
		})
	}
}

// TestStoreSourcePackChecks observes an indexed view whose second pack disagrees with its key, its descriptor or its scope:
// another pack_id or capture_id, another tool, a period outside the scope's hour or not inside one hour,
// a segment under archive/ and an archive under staging/, another size, another first seq,
// a generation its role does not allow, and a role that takes no part in a view.
// Each fails Observe, every object opened closed once;
// a pack whose period ends at the end of the scope's hour, a patch without its generation's member,
// an empty segment keyed by its seq_start and a segment keyed by its first seq beside another seq_start are accepted.
func TestStoreSourcePackChecks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// pack puts the second pack of the view in store and returns its descriptor.
		pack func(t testing.TB, s *memStore) CatalogPack
		want string
	}{
		{
			name: "the descriptor's pack_id",
			pack: func(t testing.TB, s *memStore) CatalogPack {
				p := s.putAt(t, segmentKeyAt(t, 2, seg2), storeSegment(t, seg1, nil, 2))
				p.PackID = seg2

				return p
			},
			want: "file header pack_id 51000000-0000-0000-0000-000000000000, the catalog's 52000000",
		},
		{
			name: "the key's pack_id",
			pack: func(t testing.TB, s *memStore) CatalogPack {
				return s.putAt(t, segmentKeyAt(t, 2, seg2), storeSegment(t, seg1, nil, 2))
			},
			want: "file header pack_id 51000000-0000-0000-0000-000000000000, not the key's 52000000",
		},
		{
			name: "the capture_id",
			pack: func(t testing.TB, s *memStore) CatalogPack {
				return s.putAt(t, segmentKeyAt(t, 2, seg1), withIDs(t, storeSegment(t, seg1, nil, 2), seg1, captureHigh))
			},
			want: "file header capture_id " + captureHigh.String(),
		},
		{
			name: "the tool",
			pack: func(t testing.TB, s *memStore) CatalogPack {
				return s.putAt(t, segmentKeyAt(t, 2, seg1), storeSegment(t, seg1, func(m *PackMeta) { m.ToolID = "other" }, 2))
			},
			want: `tool_id "other", not "tool"`,
		},
		{
			name: "an archive of the next hour",
			pack: func(t testing.TB, s *memStore) CatalogPack {
				return s.putAt(t, archiveKeyAt(t, memTestHour, memA), txPackIn(t, memA, 1, func(m *PackMeta) { generationMeta(m, setA, 1, 1, nil) },
					txBlock(txSeqs(2)...)))
			},
			want: "period in hour",
		},
		{
			name: "a period one ns beyond the hour",
			pack: func(t testing.TB, s *memStore) CatalogPack {
				return s.putAt(t, segmentKeyAt(t, 2, seg1), storeSegment(t, seg1, func(m *PackMeta) { m.PeriodEnd++ }, 2))
			},
			want: "is not inside one UTC hour",
		},
		{
			name: "an empty period",
			pack: func(t testing.TB, s *memStore) CatalogPack {
				return s.putAt(t, segmentKeyAt(t, 2, seg1), storeSegment(t, seg1, func(m *PackMeta) { m.PeriodEnd = m.PeriodStart }, 2))
			},
			want: "is not inside one UTC hour",
		},
		{
			name: "a segment under archive/",
			pack: func(t testing.TB, s *memStore) CatalogPack {
				return s.putAt(t, archiveKeyAt(t, memTestHour, seg1), storeSegment(t, seg1, nil, 2))
			},
			want: "pack_role segment under archive/",
		},
		{
			name: "an archive under staging/",
			pack: func(t testing.TB, s *memStore) CatalogPack {
				return s.putAt(t, segmentKeyAt(t, 2, memA), storeArchive(t, memA, setA, 2))
			},
			want: "pack_role archive under staging/",
		},
		{
			name: "the descriptor's size",
			pack: func(t testing.TB, s *memStore) CatalogPack {
				p := s.putAt(t, segmentKeyAt(t, 2, seg1), storeSegment(t, seg1, nil, 2))
				p.Size++

				return p
			},
			want: "bytes, not the",
		},
		{
			name: "the object's size",
			pack: func(t testing.TB, s *memStore) CatalogPack {
				p := s.putAt(t, segmentKeyAt(t, 2, seg1), storeSegment(t, seg1, nil, 2))
				s.sizeDelta[p.Key] = -1

				return p
			},
			want: "bytes, not the",
		},
		{
			name: "the seq_first",
			pack: func(t testing.TB, s *memStore) CatalogPack {
				return s.putAt(t, segmentKeyAt(t, 3, seg1), storeSegment(t, seg1, nil, 2))
			},
			want: "first seq 2, not the key's seq_first 3",
		},
		{
			name: "an archive of generation 0",
			pack: func(t testing.TB, s *memStore) CatalogPack {
				return s.putAt(t, archiveKeyAt(t, memTestHour, memA),
					storeSegment(t, memA, func(m *PackMeta) { generationMeta(m, setA, 1, 0, nil) }, 2))
			},
			want: "pack_role archive at scope_generation 0",
		},
		{
			name: "a segment of generation 1",
			pack: func(t testing.TB, s *memStore) CatalogPack {
				return s.putAt(t, segmentKeyAt(t, 2, seg1), storeSegment(t, seg1, func(m *PackMeta) {
					generationMeta(m, setB, 1, 1, nil)
					m.PackRole, m.CompactionLevel = PackRoleSegment, 0
				}, 2))
			},
			want: "pack_role segment at scope_generation 1, not 0",
		},
		{
			name: "the retired role",
			pack: func(t testing.TB, s *memStore) CatalogPack {
				return s.putAt(t, archiveKeyAt(t, memTestHour, memA), setMetaRole(t, storeArchive(t, memA, setA, 2), packRoleRetired))
			},
			want: "pack_role unknown(5) takes no part in a scope's view",
		},
		{
			name: "an unknown role",
			pack: func(t testing.TB, s *memStore) CatalogPack {
				return s.putAt(t, archiveKeyAt(t, memTestHour, memA), setMetaRole(t, storeArchive(t, memA, setA, 2), 9))
			},
			want: "pack_role unknown(9) takes no part in a scope's view",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store, cat := newMemStore(), newFakeCatalog()
			cat.setView(captureLow, memTestHour,
				store.putPack(t, memTestHour, storeSegment(t, seg0, nil, 1)), tt.pack(t, store))
			_, err := observeOne(t, store, cat, nil)
			require.ErrorContains(t, err, tt.want)
			opens, _ := store.openCounts()
			assert.Len(t, opens, 2, "both packs opened")
		})
	}

	t.Run("accepted", func(t *testing.T) {
		t.Parallel()

		store, cat := newMemStore(), newFakeCatalog()
		segment := storeSegment(t, seg0, nil, 1)
		require.Equal(t, blockTestHour+hourNs, mustOpen(t, segment, ReaderOptions{}).Header().Meta.PeriodEnd, "the period ends at the next hour")
		patch := storeSegment(t, patP1, func(m *PackMeta) { m.PackRole, m.PatchBase, m.Supersedes = PackRoleRepair, new(setA), []UUID{memA} }, 2)
		empty := writeRepairPack(t, CodecZstd, func(m *PackMeta) { m.SeqStart = 9 }, false, nil).file
		// A seq_start defect: the key's seq_first is the first record's seq, not the seq_start.
		defect := withSeqStart(t, storeSegment(t, seg2, nil, 5), 4)
		cat.setView(captureLow, memTestHour,
			store.putPack(t, memTestHour, patch),
			store.putPack(t, memTestHour, segment),
			store.putAt(t, segmentKeyAt(t, 9, seg1), withIDs(t, empty, seg1, captureLow)),
			store.putAt(t, segmentKeyAt(t, 5, seg2), defect))
		o, err := observeOne(t, store, cat, nil)
		require.NoError(t, err)
		sc, err := o.Scope(t.Context(), memTestHour)
		require.NoError(t, err)
		assert.Equal(t, []UUID{patP1, seg0, seg1, seg2}, packIDs(sc.Readers), "the catalog's order, its view taken as given")
	})
}

// TestStoreSourceOpenErrors fails the second pack's Open: a missing object, an error with no object,
// and an object returned with ErrObjectNotFound, a cancellation or another error, its Close succeeding or failing.
// Observe fails with the open error, joined with the close error, and closes every object once, the one returned with the error included.
// A store returning neither an object nor an error fails Observe too.
func TestStoreSourceOpenErrors(t *testing.T) {
	t.Parallel()

	refused := errors.New("open refused")
	closeFailed := errors.New("close failed")
	tests := []struct {
		name string
		// withObject returns the object beside err; closeErr fails its Close.
		err        error
		withObject bool
		closeErr   error
		missing    bool
		want       string
	}{
		{name: "missing", missing: true, err: ErrObjectNotFound},
		{name: "an error alone", err: refused},
		{name: "not found with an object", err: ErrObjectNotFound, withObject: true},
		{name: "not found with an object failing to close", err: ErrObjectNotFound, withObject: true, closeErr: closeFailed},
		{name: "cancelled with an object", err: context.Canceled, withObject: true},
		{name: "cancelled with an object failing to close", err: context.Canceled, withObject: true, closeErr: closeFailed},
		{name: "an error with an object", err: refused, withObject: true},
		{name: "an error with an object failing to close", err: refused, withObject: true, closeErr: closeFailed},
		{name: "neither object nor error", want: "returned no object"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store, cat := newMemStore(), newFakeCatalog()
			first := store.putPack(t, memTestHour, storeSegment(t, seg0, nil, 1))
			second := store.putPack(t, memTestHour, storeSegment(t, seg1, nil, 2))
			cat.setView(captureLow, memTestHour, first, second)
			store.closeErr[second.Key] = tt.closeErr
			if tt.missing {
				store.remove(second.Key)
			} else {
				store.openHook = func(_ context.Context, key string) (bool, Object, error) {
					if key != second.Key {
						return false, nil, nil
					}
					if tt.withObject {
						return true, store.openObject(key), tt.err
					}

					return true, nil, tt.err
				}
			}
			_, err := observeOne(t, store, cat, nil)
			require.Error(t, err)
			if tt.err != nil {
				require.ErrorIs(t, err, tt.err)
			}
			if tt.want != "" {
				require.ErrorContains(t, err, tt.want)
			}
			if tt.closeErr != nil {
				require.ErrorIs(t, err, tt.closeErr)
				assert.Less(t, strings.Index(err.Error(), tt.err.Error()), strings.Index(err.Error(), tt.closeErr.Error()), "open error first")
			}
			opens, closes := store.openCounts()
			assert.Equal(t, 1, closes[first.Key])
			if tt.withObject {
				assert.Equal(t, 1, opens[second.Key])
			}
		})
	}
}

// TestStoreSourceBootstrapErrors fails the second pack's bootstrap: an object that is no tracepack file,
// an object deleted between the snapshot and its open, and one deleted before its first read:
// Observe fails, a deletion with ErrObjectNotFound, every object closed once.
func TestStoreSourceBootstrapErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// fault breaks the second pack, of key.
		fault func(s *memStore, cat *fakeCatalog, key string)
		is    error
	}{
		{name: "not a tracepack file", fault: func(s *memStore, _ *fakeCatalog, key string) { s.objects[key] = make([]byte, len(s.objects[key])) },
			is: ErrNotTracepack},
		{name: "deleted before its open", fault: func(s *memStore, cat *fakeCatalog, key string) {
			cat.afterSnapshot = func() { s.remove(key) }
		}, is: ErrObjectNotFound},
		{name: "deleted before its first read", fault: func(s *memStore, _ *fakeCatalog, key string) {
			s.openHook = func(_ context.Context, k string) (bool, Object, error) {
				if k != key {
					return false, nil, nil
				}
				obj := s.openObject(k)
				s.remove(k)

				return true, obj, nil
			}
		}, is: ErrObjectNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store, cat := newMemStore(), newFakeCatalog()
			first := store.putPack(t, memTestHour, storeSegment(t, seg0, nil, 1))
			second := store.putPack(t, memTestHour, storeSegment(t, seg1, nil, 2))
			cat.setView(captureLow, memTestHour, first, second)
			tt.fault(store, cat, second.Key)
			_, err := observeOne(t, store, cat, nil)
			require.ErrorIs(t, err, tt.is)
			require.ErrorContains(t, err, second.Key)
		})
	}
}

// TestStoreSourceCancel cancels Observe's context once the snapshot is taken, and during the first pack's bootstrap:
// Observe fails with the cancellation before the next step, the second pack never opened, every object opened closed once.
func TestStoreSourceCancel(t *testing.T) {
	t.Parallel()

	for _, during := range []string{"snapshot", "bootstrap"} {
		t.Run(during, func(t *testing.T) {
			t.Parallel()

			store, cat := newMemStore(), newFakeCatalog()
			first := store.putPack(t, memTestHour, storeSegment(t, seg0, nil, 1))
			second := store.putPack(t, memTestHour, storeSegment(t, seg1, nil, 2))
			cat.setView(captureLow, memTestHour, first, second)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var attempts sync.Map
			store.openHook = func(_ context.Context, key string) (bool, Object, error) {
				attempts.Store(key, true)

				return false, nil, nil
			}
			if during == "snapshot" {
				cat.afterSnapshot = cancel
			} else {
				store.readHook = func(string, int64, int) error {
					cancel()

					return nil
				}
			}
			o, err := newTestStoreSource(t, store, cat, nil).Observe(ctx, captureLow, memTestHour, memTestHour+1)
			require.ErrorIs(t, err, context.Canceled)
			require.Nil(t, o)
			opens, _ := store.openCounts()
			_, attempted := attempts.Load(second.Key)
			assert.False(t, attempted, "the second pack's open is never attempted")
			if during == "bootstrap" {
				assert.Equal(t, 1, opens[first.Key])
			}
			requireClosedOnce(t, store)
		})
	}
}

// TestStoreSourcePanic panics in the store's Open of the second pack:
// the panic propagates, and the first pack's object is closed once.
func TestStoreSourcePanic(t *testing.T) {
	t.Parallel()

	store, cat := newMemStore(), newFakeCatalog()
	first := store.putPack(t, memTestHour, storeSegment(t, seg0, nil, 1))
	second := store.putPack(t, memTestHour, storeSegment(t, seg1, nil, 2))
	cat.setView(captureLow, memTestHour, first, second)
	store.openHook = func(_ context.Context, key string) (bool, Object, error) {
		if key == second.Key {
			panic("adapter panic")
		}

		return false, nil, nil
	}
	src := newTestStoreSource(t, store, cat, nil)
	require.PanicsWithValue(t, "adapter panic", func() { _, _ = src.Observe(t.Context(), captureLow, memTestHour, memTestHour+1) })
	_, closes := store.openCounts()
	assert.Equal(t, map[string]int{first.Key: 1}, closes)
}

// TestStoreSourceCloseErrors fails the Close of retained objects:
// after a later pack's failure, Observe returns that failure first, the close errors joined after it;
// after a successful Observe, the observation's Close closes every object once and returns the first close error, then the same again.
func TestStoreSourceCloseErrors(t *testing.T) {
	t.Parallel()

	firstErr, secondErr := errors.New("first close"), errors.New("second close")
	setup := func(t testing.TB) (*memStore, *fakeCatalog, [3]CatalogPack) {
		t.Helper()

		store, cat := newMemStore(), newFakeCatalog()
		var packs [3]CatalogPack
		for i, id := range []UUID{seg0, seg1, seg2} {
			packs[i] = store.putPack(t, memTestHour, storeSegment(t, id, nil, uint64(i+1)))
		}
		store.closeErr[packs[0].Key], store.closeErr[packs[1].Key] = firstErr, secondErr
		cat.setView(captureLow, memTestHour, packs[:]...)

		return store, cat, packs
	}

	store, cat, packs := setup(t)
	store.remove(packs[2].Key)
	_, err := observeOne(t, store, cat, nil)
	require.ErrorIs(t, err, ErrObjectNotFound)
	require.ErrorIs(t, err, firstErr)
	require.ErrorIs(t, err, secondErr)
	msg := err.Error()
	assert.Less(t, strings.Index(msg, ErrObjectNotFound.Error()), strings.Index(msg, firstErr.Error()))
	assert.Less(t, strings.Index(msg, firstErr.Error()), strings.Index(msg, secondErr.Error()))

	store, cat, _ = setup(t)
	o, err := newTestStoreSource(t, store, cat, nil).Observe(t.Context(), captureLow, memTestHour, memTestHour+1)
	require.NoError(t, err)
	require.ErrorIs(t, o.Close(), firstErr)
	err = o.Close()
	require.ErrorIs(t, err, firstErr)
	assert.NotErrorIs(t, err, secondErr)
	requireClosedOnce(t, store)
}

// TestStoreSourceCloseNoWait closes an observation while another Close of it is still closing an object:
// the second Close returns at once, closing nothing, and every object is closed once when the first returns.
func TestStoreSourceCloseNoWait(t *testing.T) {
	t.Parallel()

	store, cat := newMemStore(), newFakeCatalog()
	cat.setView(captureLow, memTestHour, store.putPack(t, memTestHour, storeSegment(t, seg0, nil, 1)))
	o, err := newTestStoreSource(t, store, cat, nil).Observe(t.Context(), captureLow, memTestHour, memTestHour+1)
	require.NoError(t, err)

	closing, release := make(chan struct{}), make(chan struct{})
	store.closeHook = func(string) {
		close(closing)
		<-release
	}
	first := make(chan error, 1)
	go func() { first <- o.Close() }()
	<-closing
	second := make(chan error, 1)
	go func() { second <- o.Close() }()
	select {
	case err := <-second:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the second Close waited on the first")
	}
	close(release)
	require.NoError(t, <-first)
	requireClosedOnce(t, store)
}

// TestStoreSourceAdapterDeadline bootstraps a pack in two concurrent reads, a head read blocked until the store's own deadline
// while its partner fails at once: Observe fails with the head read's error, the first in request order,
// once both reads returned, and closes the object once.
// A cancellation during a read is observed before the next read: no read follows it.
func TestStoreSourceAdapterDeadline(t *testing.T) {
	t.Parallel()

	split := func(o *StoreSourceOptions) {
		o.Reader = ReaderOptions{WholeReadThreshold: 1, HeadWindow: 1, TailWindow: 1}
	}
	deadline := errors.New("store deadline exceeded")
	partner := errors.New("partner failed")

	store, cat := newMemStore(), newFakeCatalog()
	p := store.putPack(t, memTestHour, storeSegment(t, seg0, nil, 1, 2, 3))
	cat.setView(captureLow, memTestHour, p)
	var reads atomic.Int32
	store.readHook = func(_ string, off int64, _ int) error {
		reads.Add(1)
		if off == 0 {
			<-time.After(20 * time.Millisecond)

			return deadline
		}

		return partner
	}
	_, err := observeOne(t, store, cat, split)
	require.ErrorIs(t, err, deadline)
	assert.Equal(t, int32(2), reads.Load(), "both reads of the round ran")

	store, cat = newMemStore(), newFakeCatalog()
	cat.setView(captureLow, memTestHour, store.putPack(t, memTestHour, storeSegment(t, seg0, nil, 1, 2, 3)))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var after atomic.Int32
	store.readHook = func(string, int64, int) error {
		if ctx.Err() != nil {
			after.Add(1)
		}
		cancel()

		return nil
	}
	src := newTestStoreSource(t, store, cat, split)
	o, err := src.Observe(ctx, captureLow, memTestHour, memTestHour+1)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, o)
	assert.LessOrEqual(t, after.Load(), int32(1), "only the concurrent partner of the cancelling read may run after it")
	requireClosedOnce(t, store)
}

// damagedFirstBlock returns file, a pack that is not finalized, with its first block envelope's magic broken,
// so the forward walk finds no block.
func damagedFirstBlock(t testing.TB, file []byte) []byte {
	t.Helper()

	r := mustOpen(t, file, ReaderOptions{})
	require.Nil(t, r.footer)
	out := bytes.Clone(file)
	out[r.blocksStart()] ^= 0xFF
	require.Empty(t, mustOpen(t, out, ReaderOptions{}).Blocks())

	return out
}

// TestStoreSourceSeqFirst observes segments whose first seq damage hides or which hold no record
// (the tracepack storage specification §3):
// a segment whose first block is damaged, its seq_start not the key's seq_first, is accepted,
// and a lookup over it reports the damage and is incomplete;
// so is a segment that is not finalized and holds no block, since more records may follow;
// a finalized segment known to hold no record, by its footer or by a walk that agrees with its trailer,
// is checked against its seq_start and fails when it differs.
func TestStoreSourceSeqFirst(t *testing.T) {
	t.Parallel()

	quality := func(m *PackMeta) { m.QualityEvaluated = true }
	open := withSeqStart(t, withIDs(t, writeRepairPack(t, CodecZstd, quality, true,
		txBlock(txRecord(5, nil), txReply(6, nil))).file, seg1, captureLow), 4)
	damaged := damagedFirstBlock(t, open)

	store, cat := newMemStore(), newFakeCatalog()
	cat.setView(captureLow, memTestHour, store.putAt(t, segmentKeyAt(t, 5, seg1), damaged))
	src := newTestStoreSource(t, store, cat, nil)
	observeStore(t, src, memTestHour, memTestHour+2)
	res, err := FindTransaction(t.Context(), src, txKeyAt(5), TxOptions{})
	require.NoError(t, err)
	assert.Equal(t, TxIncomplete, res.Outcome)
	assert.Contains(t, gapHours(res.Gaps), "read@0", "the walk's stop is reported")

	empty := withIDs(t, writeRepairPack(t, CodecZstd, func(m *PackMeta) { m.SeqStart = 9 }, true, nil).file, seg2, captureLow)
	store, cat = newMemStore(), newFakeCatalog()
	cat.setView(captureLow, memTestHour, store.putAt(t, segmentKeyAt(t, 8, seg2), empty))
	_, err = observeOne(t, store, cat, nil)
	require.NoError(t, err, "an unfinalized segment without blocks may hold records")

	finalized := withIDs(t, writeRepairPack(t, CodecZstd, func(m *PackMeta) { m.SeqStart = 9 }, false, nil).file, seg2, captureLow)
	store, cat = newMemStore(), newFakeCatalog()
	cat.setView(captureLow, memTestHour, store.putAt(t, segmentKeyAt(t, 8, seg2), finalized))
	_, err = observeOne(t, store, cat, nil)
	require.ErrorContains(t, err, "first seq 9, not the key's seq_first 8")

	// A footer over MaxFooterLen leaves the finalized segment to the walk, which agrees with its trailer.
	tiny := ReaderOptions{MaxFooterLen: 1}
	h := mustOpen(t, finalized, tiny).Header()
	require.True(t, h.Finalized)
	require.ErrorIs(t, h.FooterErr, ErrReadLimit)
	store, cat = newMemStore(), newFakeCatalog()
	cat.setView(captureLow, memTestHour, store.putAt(t, segmentKeyAt(t, 8, seg2), finalized))
	_, err = observeOne(t, store, cat, func(o *StoreSourceOptions) { o.Reader = tiny })
	require.ErrorContains(t, err, "first seq 9, not the key's seq_first 8", "a walk that agrees with the trailer finds no record")
}

// errCountingCtx is a context that counts the calls of its Err.
type errCountingCtx struct {
	context.Context
	n *atomic.Int64
}

// Err counts the call and returns the context's error.
func (c errCountingCtx) Err() error {
	c.n.Add(1)

	return c.Context.Err()
}

// TestStoreSourceCtxPerEntry counts the checks of Observe's context over snapshots that grow by n entries of one kind —
// pack descriptors, evidence boundaries, evidence closures, barriers —
// and requires at least n more checks: the source checks its context for every entry it copies or checks.
func TestStoreSourceCtxPerEntry(t *testing.T) {
	t.Parallel()

	const n = 200
	kinds := map[string]func(t testing.TB, cat *fakeCatalog, count int){
		"pack descriptors": func(t testing.TB, cat *fakeCatalog, count int) {
			packs := make([]CatalogPack, count)
			for i := range packs {
				id := UUID{0x60, byte(i >> 8), byte(i)}
				packs[i] = CatalogPack{Key: segmentKeyAt(t, uint64(i+1), id), PackID: id, Size: 1000}
			}
			// The objects are missing, so Observe fails at the first open, after checking every descriptor.
			cat.setView(captureLow, memTestHour, packs...)
		},
		"evidence boundaries": func(_ testing.TB, cat *fakeCatalog, count int) {
			e := CaptureEvidence{Boundaries: make([]Boundary, count)}
			for i := range e.Boundaries {
				e.Boundaries[i] = Boundary{Capture: captureLow, Seq: uint64(i)}
			}
			cat.setEvidence(captureLow, e)
		},
		"evidence closures": func(_ testing.TB, cat *fakeCatalog, count int) {
			e := CaptureEvidence{Closures: make([]EpochClosure, count)}
			for i := range e.Closures {
				e.Closures[i] = EpochClosure{Epoch: uint32(i)}
			}
			cat.setEvidence(captureLow, e)
		},
		"barriers": func(_ testing.TB, cat *fakeCatalog, count int) {
			for i := range count {
				cat.addBarrier(Boundary{Capture: captureHigh, Seq: uint64(i), Kind: BoundaryKindStopUnclean})
			}
		},
	}
	for name, setup := range kinds {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			checks := func(count int) int64 {
				store, cat := newMemStore(), newFakeCatalog()
				setup(t, cat, count)
				ctx := errCountingCtx{Context: t.Context(), n: &atomic.Int64{}}
				if o, err := newTestStoreSource(t, store, cat, nil).Observe(ctx, captureLow, memTestHour, memTestHour+1); err == nil {
					require.NoError(t, o.Close())
				}

				return ctx.n.Load()
			}
			assert.GreaterOrEqual(t, checks(2*n)-checks(n), int64(n))
		})
	}
}
