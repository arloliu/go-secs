package tracepack

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// listedStore returns a memStore and a fakeCatalog that does not index captureLow's scope of memTestHour.
func listedStore() (*memStore, *fakeCatalog) {
	store, cat := newMemStore(), newFakeCatalog()
	cat.setIndexed(captureLow, memTestHour, false)

	return store, cat
}

// TestStoreSourceListedPackChecks observes a listed scope holding a pack that disagrees with its key or its scope:
// a wrong-hour archive alone and two consistent ones, another tool with matching ids, another pack_id, another size,
// a segment under archive/ and an archive under staging/, empty periods and a period one ns beyond the hour,
// a pack_id twice in the scope, a segment's seq_first, and an archive of generation 0.
// Each fails Observe, every object opened closed once; a period ending at the next hour is accepted.
func TestStoreSourceListedPackChecks(t *testing.T) {
	t.Parallel()

	nextHour := func(m *PackMeta) {
		m.PeriodStart += hourNs
		m.PeriodEnd += hourNs
	}
	tests := []struct {
		name string
		put  func(t testing.TB, s *memStore)
		want string
	}{
		{name: "a wrong-hour archive", put: func(t testing.TB, s *memStore) {
			s.putAt(t, archiveKeyAt(t, memTestHour, memA), listedArchive(t, memA, setA, 1, 1, 1, txBlock(txSeqs(1)...)))
		}, want: "period in hour"},
		{name: "consistent wrong-hour packs", put: func(t testing.TB, s *memStore) {
			s.putAt(t, archiveKeyAt(t, memTestHour, memA), listedArchive(t, memA, setA, 1, 1, 1, txBlock(txSeqs(1)...)))
			s.putAt(t, archiveKeyAt(t, memTestHour, patP1), listedPatch(t, patP1, 1, new(setA), []UUID{memA}, txBlock(txSeqs(1)...)))
		}, want: "period in hour"},
		{name: "another tool", put: func(t testing.TB, s *memStore) {
			s.putPack(t, memTestHour, storeArchive(t, memA, setA, 1))
			s.putAt(t, archiveKeyAt(t, memTestHour, memB), storeSegment(t, memB, func(m *PackMeta) {
				generationMeta(m, setB, 1, 1, nil)
				m.ToolID = "other"
			}, 1))
		}, want: `tool_id "other", not "tool"`},
		{name: "another pack_id", put: func(t testing.TB, s *memStore) {
			s.putAt(t, archiveKeyAt(t, memTestHour, memB), storeArchive(t, memA, setA, 1))
		}, want: "file header pack_id"},
		{name: "another size", put: func(t testing.TB, s *memStore) {
			p := s.putPack(t, memTestHour, storeArchive(t, memA, setA, 1))
			s.sizeDelta[p.Key] = 1
		}, want: "bytes, not the"},
		{name: "a segment under archive/", put: func(t testing.TB, s *memStore) {
			s.putAt(t, archiveKeyAt(t, memTestHour, seg1), storeSegment(t, seg1, nil, 1))
		}, want: "pack_role segment under archive/"},
		{name: "an archive under staging/", put: func(t testing.TB, s *memStore) {
			s.putAt(t, segmentKeyAt(t, 1, memA), storeArchive(t, memA, setA, 1))
		}, want: "pack_role archive under staging/"},
		{name: "an empty segment period", put: func(t testing.TB, s *memStore) {
			s.putPack(t, memTestHour, storeSegment(t, seg1, func(m *PackMeta) { m.PeriodEnd = m.PeriodStart }, 1))
		}, want: "is not inside one UTC hour"},
		{name: "an empty archive period", put: func(t testing.TB, s *memStore) {
			s.putPack(t, memTestHour, storeSegment(t, memA, func(m *PackMeta) {
				generationMeta(m, setA, 1, 1, nil)
				m.PeriodEnd = m.PeriodStart
			}, 1))
		}, want: "is not inside one UTC hour"},
		{name: "a period one ns beyond the hour", put: func(t testing.TB, s *memStore) {
			s.putPack(t, memTestHour, storeSegment(t, seg1, func(m *PackMeta) { m.PeriodEnd++ }, 1))
		}, want: "is not inside one UTC hour"},
		{name: "a pack_id in a segment and an archive", put: func(t testing.TB, s *memStore) {
			s.putPack(t, memTestHour, storeSegment(t, seg1, nil, 1))
			s.putAt(t, archiveKeyAt(t, memTestHour, seg1), storeSegment(t, seg1, func(m *PackMeta) { generationMeta(m, setA, 1, 1, nil) }, 1))
		}, want: "pack_id 51000000-0000-0000-0000-000000000000 is listed twice"},
		{name: "a pack_id in two segments", put: func(t testing.TB, s *memStore) {
			s.putPack(t, memTestHour, storeSegment(t, seg1, nil, 1))
			s.putPack(t, memTestHour, storeSegment(t, seg1, nil, 2))
		}, want: "is listed twice"},
		{name: "a segment's seq_first", put: func(t testing.TB, s *memStore) {
			s.putAt(t, segmentKeyAt(t, 3, seg1), storeSegment(t, seg1, nil, 2))
		}, want: "first seq 2, not the key's seq_first 3"},
		{name: "an archive of generation 0", put: func(t testing.TB, s *memStore) {
			s.putPack(t, memTestHour, storeSegment(t, memA, func(m *PackMeta) { generationMeta(m, setA, 1, 0, nil) }, 1))
		}, want: "pack_role archive at scope_generation 0"},
		{name: "a segment of the next hour keyed in this hour's archive", put: func(t testing.TB, s *memStore) {
			s.putAt(t, archiveKeyAt(t, memTestHour, memA), storeSegment(t, memA, func(m *PackMeta) {
				generationMeta(m, setA, 1, 1, nil)
				nextHour(m)
			}, 1))
		}, want: "period in hour"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store, cat := listedStore()
			tt.put(t, store)
			_, err := observeOne(t, store, cat, nil)
			require.ErrorContains(t, err, tt.want)
			assert.NotErrorIs(t, err, ErrInvalidKey)
		})
	}

	t.Run("accepted", func(t *testing.T) {
		t.Parallel()

		store, cat := listedStore()
		segment := storeSegment(t, seg0, nil, 1)
		require.Equal(t, blockTestHour+hourNs, mustOpen(t, segment, ReaderOptions{}).Header().Meta.PeriodEnd, "the period ends at the next hour")
		store.putPack(t, memTestHour, segment)
		o, err := observeOne(t, store, cat, nil)
		require.NoError(t, err)
		sc, err := o.Scope(t.Context(), memTestHour)
		require.NoError(t, err)
		assert.Equal(t, []UUID{seg0}, packIDs(sc.Readers))
	})
}

// TestStoreSourceListedKeys observes listed scopes, in pages of one key, whose listings return keys that do not parse:
// the scope's capture in uppercase on a later page of the archive listing, where its pack would change the view;
// an uppercase pack_id in an archive key and in a segment key, an uppercase commit id;
// an extra suffix, and an extra component under each listing.
// Each fails Observe with ErrInvalidKey, every object opened closed once.
// A canonical key of another capture is skipped without being opened.
func TestStoreSourceListedKeys(t *testing.T) {
	t.Parallel()

	upperFrom := func(key string, from int) string {
		return key[:from] + strings.ToUpper(key[from:len(key)-len(keyPackSuffix)]) + keyPackSuffix
	}
	tests := []struct {
		name string
		key  func(t testing.TB) string
		want string
	}{
		{name: "an uppercase capture on a later page", key: func(t testing.TB) string {
			key := archiveKeyAt(t, memTestHour, patP1)
			dir := archiveDir(t, memTestHour)

			return dir + strings.ToUpper(key[len(dir):len(dir)+uuidKeyLen]) + key[len(dir)+uuidKeyLen:]
		}, want: "not in lowercase"},
		{name: "an uppercase archive pack_id", key: func(t testing.TB) string {
			key := archiveKeyAt(t, memTestHour, patP1)

			return upperFrom(key, len(key)-len(keyPackSuffix)-uuidKeyLen)
		}, want: "not in lowercase"},
		{name: "an uppercase segment pack_id", key: func(t testing.TB) string {
			key := segmentKeyAt(t, 7, memA)

			return upperFrom(key, len(key)-len(keyPackSuffix)-uuidKeyLen)
		}, want: "not in lowercase"},
		{name: "an uppercase commit id", key: func(t testing.TB) string {
			key, err := CommitKey(storeTestPrefix, memStoreTool, captureLow, memTestHour, setB)
			require.NoError(t, err)

			return key[:len(key)-uuidKeyLen] + strings.ToUpper(key[len(key)-uuidKeyLen:])
		}, want: "not in lowercase"},
		{name: "an extra suffix", key: func(t testing.TB) string { return archiveKeyAt(t, memTestHour, patP1) + ".bak" }, want: "not <capture_id>-<pack_id>.tpk"},
		{name: "an extra archive component", key: func(t testing.TB) string { return archiveDir(t, memTestHour) + "x/y" }, want: "7 components"},
		{name: "an extra staging component", key: func(t testing.TB) string { return stagingDir(t) + "x/y" }, want: "4 components"},
		{name: "an extra commit component", key: func(t testing.TB) string { return commitDir(t, memTestHour) + "x/y" }, want: "5 components"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store, cat := listedStore()
			store.pageSize = 1
			store.putPack(t, memTestHour, storeArchive(t, memA, setA, 1))
			store.putCommit(t, captureLow, memTestHour, setA)
			store.putPack(t, memTestHour, storeSegment(t, seg0, nil, 3))
			store.put(tt.key(t), storeSegment(t, seg1, nil, 4))
			_, err := observeOne(t, store, cat, nil)
			require.ErrorIs(t, err, ErrInvalidKey)
			require.ErrorContains(t, err, tt.want)
		})
	}

	t.Run("another capture", func(t *testing.T) {
		t.Parallel()

		store, cat := listedStore()
		store.pageSize = 1
		key, err := ArchiveKey(storeTestPrefix, memStoreTool, UUID{0x0A}, memTestHour, memB)
		require.NoError(t, err)
		store.put(key, []byte("not a pack"))
		store.putPack(t, memTestHour, storeArchive(t, memA, setA, 1))
		store.putCommit(t, captureLow, memTestHour, setA)
		o, err := observeOne(t, store, cat, nil)
		require.NoError(t, err)
		sc, err := o.Scope(t.Context(), memTestHour)
		require.NoError(t, err)
		assert.Equal(t, []UUID{memA}, packIDs(sc.Readers))
		opens, _ := store.openCounts()
		assert.Zero(t, opens[key])
	})
}

// headOnly returns options under which a pack's head read reads the 80-byte file header, then the pack metadata, and nothing more.
func headOnly(o *StoreSourceOptions) {
	o.Reader = ReaderOptions{WholeReadThreshold: 1, HeadWindow: 1, TailWindow: 1}
}

// blocksStartOf returns where the first block of file starts, the end of its head.
func blocksStartOf(t testing.TB, file []byte) int64 {
	t.Helper()

	return int64(mustOpen(t, file, ReaderOptions{}).blocksStart())
}

// TestStoreSourceListedStaging observes a listed scope beside staging segments of other hours:
// a segment of an hour the observation does not list is opened for its head only, never read past it, and closed,
// even when every read of its blocks or footer would fail, and under the default options when its footer is corrupt;
// a head that does not validate, names another capture, or holds a period outside one hour fails Observe,
// since the segment could belong to a listed hour.
func TestStoreSourceListedStaging(t *testing.T) {
	t.Parallel()

	other := txPackIn(t, seg2, 3, nil, txBlock(inHour(3, txRecord(30, nil))))
	t.Run("another hour's segment", func(t *testing.T) {
		t.Parallel()

		store, cat := listedStore()
		store.putPack(t, memTestHour, storeSegment(t, seg0, nil, 1))
		key := store.putPack(t, memTestHour+3, other).Key
		end := blocksStartOf(t, other)
		var past atomic.Int32
		store.readHook = func(k string, off int64, n int) error {
			if k == key && off+int64(n) > end {
				past.Add(1)

				return errors.New("the footer is unreadable")
			}

			return nil
		}
		o, err := observeOne(t, store, cat, headOnly)
		require.NoError(t, err)
		sc, err := o.Scope(t.Context(), memTestHour)
		require.NoError(t, err)
		assert.Equal(t, []UUID{seg0}, packIDs(sc.Readers))
		assert.Zero(t, past.Load(), "nothing past the head is read")
		opens, closes := store.openCounts()
		assert.Equal(t, 1, opens[key])
		assert.Equal(t, 1, closes[key])
	})

	t.Run("another hour's segment with a corrupt footer", func(t *testing.T) {
		t.Parallel()

		store, cat := listedStore()
		store.putPack(t, memTestHour, storeSegment(t, seg0, nil, 1))
		r := mustOpen(t, other, ReaderOptions{})
		require.NotNil(t, r.trailer)
		corrupt := bytes.Clone(other)
		corrupt[r.trailer.FooterOffset+1] ^= 0xFF
		require.Error(t, mustOpen(t, corrupt, ReaderOptions{}).Header().FooterErr, "the footer is corrupt")
		key := store.putPack(t, memTestHour+3, corrupt).Key
		o, err := observeOne(t, store, cat, nil)
		require.NoError(t, err)
		sc, err := o.Scope(t.Context(), memTestHour)
		require.NoError(t, err)
		assert.Equal(t, []UUID{seg0}, packIDs(sc.Readers))
		opens, closes := store.openCounts()
		assert.Equal(t, 1, opens[key])
		assert.Equal(t, 1, closes[key])
	})

	badCRC := bytes.Clone(other)
	badCRC[format.FileHeaderLen+2] ^= 0xFF
	tests := []struct {
		name string
		file []byte
		is   error
		want string
	}{
		{name: "a bad metadata CRC", file: badCRC, is: ErrChecksum},
		{name: "another capture", file: withIDs(t, other, seg2, captureHigh), want: "file header capture_id " + captureHigh.String()},
		{name: "a period outside one hour", file: txPackIn(t, seg2, 3, func(m *PackMeta) { m.PeriodEnd += hourNs }, txBlock(inHour(3, txRecord(30, nil)))),
			want: "is not inside one UTC hour"},
		{name: "not a tracepack file", file: make([]byte, 200), is: ErrNotTracepack},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store, cat := listedStore()
			store.putPack(t, memTestHour, storeSegment(t, seg0, nil, 1))
			store.put(segmentKeyAt(t, 30, seg2), tt.file)
			_, err := observeOne(t, store, cat, headOnly)
			require.Error(t, err)
			if tt.is != nil {
				require.ErrorIs(t, err, tt.is)
			}
			if tt.want != "" {
				require.ErrorContains(t, err, tt.want)
			}
			require.ErrorContains(t, err, segmentKeyAt(t, 30, seg2))
		})
	}
}

// TestStoreSourceListedDeletion deletes listed packs between their listing and their read:
// a segment of an hour outside the observation, after the staging listing, which a retry no longer lists;
// a listed hour's segment, whose records would conflict with the hour's other segment;
// a listed hour's segment at its open, at its first read, and between its head read and its full open;
// and a listed hour's archive.
// Each fails Observe with ErrObjectNotFound, every object opened closed once, and the retry succeeds where it should.
func TestStoreSourceListedDeletion(t *testing.T) {
	t.Parallel()

	t.Run("an unlisted hour's segment, then a retry", func(t *testing.T) {
		t.Parallel()

		store, cat := listedStore()
		store.putPack(t, memTestHour, storeSegment(t, seg0, nil, 1))
		gone := store.putPack(t, memTestHour+3, txPackIn(t, seg2, 3, nil, txBlock(inHour(3, txRecord(30, nil))))).Key
		staging := stagingDir(t)
		once := true
		store.afterList = func(prefix, _ string, _ *ObjectPage) error {
			if prefix == staging && once {
				once = false
				store.remove(gone)
			}

			return nil
		}
		_, err := observeOne(t, store, cat, nil)
		require.ErrorIs(t, err, ErrObjectNotFound)
		require.ErrorContains(t, err, gone)
		o, err := observeOne(t, store, cat, nil)
		require.NoError(t, err, "the retry no longer lists the deleted segment")
		sc, err := o.Scope(t.Context(), memTestHour)
		require.NoError(t, err)
		assert.Equal(t, []UUID{seg0}, packIDs(sc.Readers))
	})

	faults := []struct {
		name string
		// fault deletes the pack of key at its stage.
		fault func(s *memStore, key string)
	}{
		{name: "after the listing", fault: func(s *memStore, key string) {
			s.afterList = func(prefix, _ string, _ *ObjectPage) error {
				if strings.HasPrefix(key, prefix) {
					s.remove(key)
				}

				return nil
			}
		}},
		{name: "at the open", fault: func(s *memStore, key string) {
			s.openHook = func(_ context.Context, k string) (bool, Object, error) {
				if k == key {
					s.remove(k)
				}

				return false, nil, nil
			}
		}},
		{name: "at the first read", fault: func(s *memStore, key string) {
			s.openHook = func(_ context.Context, k string) (bool, Object, error) {
				if k != key {
					return false, nil, nil
				}
				obj := s.openObject(k)
				s.remove(k)

				return true, obj, nil
			}
		}},
	}
	t.Run("between a segment's head read and its full open", func(t *testing.T) {
		t.Parallel()

		store, cat := listedStore()
		key := store.putPack(t, memTestHour, storeSegment(t, seg0, nil, 1)).Key
		heads := 0
		store.readHook = func(k string, off int64, _ int) error {
			// The head read and the full open each start at offset 0, over one object.
			if k == key && off == 0 {
				if heads++; heads == 2 {
					store.remove(k)
				}
			}

			return nil
		}
		_, err := observeOne(t, store, cat, nil)
		require.ErrorIs(t, err, ErrObjectNotFound)
		require.ErrorContains(t, err, key)
		opens, _ := store.openCounts()
		assert.Equal(t, 1, opens[key], "one object for the head read and the full open")
	})

	for _, f := range faults {
		for _, archive := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, archive %v", f.name, archive), func(t *testing.T) {
				t.Parallel()

				store, cat := listedStore()
				store.putPack(t, memTestHour, storeSegment(t, seg0, nil, 1, 2))
				conflicting := txPack(t, seg1, nil, txBlock(txRecord(2, func(r *Record) { r.TSUTCNs++ })))
				if archive {
					conflicting = storeArchive(t, memA, setA, 2)
					store.putCommit(t, captureLow, memTestHour, setA)
				}
				key := store.putPack(t, memTestHour, conflicting).Key
				f.fault(store, key)
				_, err := observeOne(t, store, cat, nil)
				require.ErrorIs(t, err, ErrObjectNotFound)
				require.ErrorContains(t, err, key)
			})
		}
	}
}

// TestStoreSourceListingContract observes a listed scope whose staging listing, in pages of one key, breaks the listing contract:
// an error on the first, middle or last page, alone or with keys; a page returned twice; a token that comes again;
// a key outside the prefix; and more pages than MaxListPages.
// Each fails Observe, every object opened closed once.
// Empty pages with a token, and exactly MaxListPages pages, are accepted.
func TestStoreSourceListingContract(t *testing.T) {
	t.Parallel()

	listErr := errors.New("list failed")
	setup := func(t testing.TB) (*memStore, *fakeCatalog, []string) {
		t.Helper()

		store, cat := listedStore()
		store.pageSize = 1
		keys := make([]string, 0, 3)
		for i, id := range []UUID{seg0, seg1, seg2} {
			keys = append(keys, store.putPack(t, memTestHour, storeSegment(t, id, nil, uint64(i+1))).Key)
		}

		return store, cat, keys
	}
	// onStaging returns an afterList hook that runs edit on the n-th page of the staging listing, counted from 0.
	onStaging := func(t testing.TB, n int, edit func(page *ObjectPage, keys []string) error, keys []string) func(string, string, *ObjectPage) error {
		staging := stagingDir(t)
		page := 0

		return func(prefix, _ string, p *ObjectPage) error {
			if prefix != staging {
				return nil
			}
			page++
			if page-1 != n {
				return nil
			}

			return edit(p, keys)
		}
	}
	fails := []struct {
		name string
		page int
		edit func(page *ObjectPage, keys []string) error
		is   error
		want string
	}{
		{name: "an error on the first page", page: 0, edit: func(p *ObjectPage, _ []string) error { *p = ObjectPage{}; return listErr }, is: listErr},
		{name: "an error on the middle page", page: 1, edit: func(p *ObjectPage, _ []string) error { *p = ObjectPage{}; return listErr }, is: listErr},
		{name: "an error on the last page", page: 2, edit: func(p *ObjectPage, _ []string) error { *p = ObjectPage{}; return listErr }, is: listErr},
		{name: "an error with keys", page: 1, edit: func(*ObjectPage, []string) error { return listErr }, is: listErr},
		{name: "a page twice", page: 1, edit: func(p *ObjectPage, keys []string) error {
			*p = ObjectPage{Objects: []ObjectInfo{{Key: keys[0]}}, Next: keys[0]}

			return nil
		}, want: "is not above"},
		{name: "a token again", page: 1, edit: func(p *ObjectPage, keys []string) error {
			*p = ObjectPage{Next: keys[0]}

			return nil
		}, want: "comes again"},
		{name: "a key outside the prefix", page: 1, edit: func(p *ObjectPage, _ []string) error {
			p.Objects[0].Key = strings.Replace(p.Objects[0].Key, "/staging/", "/staging2/", 1)

			return nil
		}, want: "is outside the prefix"},
	}
	for _, tt := range fails {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store, cat, keys := setup(t)
			store.afterList = onStaging(t, tt.page, tt.edit, keys)
			_, err := observeOne(t, store, cat, nil)
			require.Error(t, err)
			if tt.is != nil {
				require.ErrorIs(t, err, tt.is)
			}
			if tt.want != "" {
				require.ErrorContains(t, err, tt.want)
			}
		})
	}

	t.Run("empty pages with a token", func(t *testing.T) {
		t.Parallel()

		store, cat, keys := setup(t)
		staging := stagingDir(t)
		store.afterList = func(prefix, token string, p *ObjectPage) error {
			if prefix == staging && token == "" {
				// An empty first page whose token, the prefix itself, sorts before every key, then an empty page after each key.
				*p = ObjectPage{Next: staging}
			} else if prefix == staging && token == staging {
				*p = ObjectPage{Objects: p.Objects, Next: keys[0] + "\x00"}
			}

			return nil
		}
		o, err := observeOne(t, store, cat, nil)
		require.NoError(t, err)
		sc, err := o.Scope(t.Context(), memTestHour)
		require.NoError(t, err)
		assert.Equal(t, []UUID{seg0, seg1, seg2}, packIDs(sc.Readers))
	})

	t.Run("MaxListPages", func(t *testing.T) {
		t.Parallel()

		store, cat, _ := setup(t)
		o, err := observeOne(t, store, cat, func(o *StoreSourceOptions) { o.MaxListPages = 3 })
		require.NoError(t, err)
		require.NoError(t, o.Close())
		_, err = observeOne(t, store, cat, func(o *StoreSourceOptions) { o.MaxListPages = 2 })
		require.ErrorIs(t, err, ErrReadLimit)
		require.ErrorContains(t, err, "more than MaxListPages 2 pages")
	})
}

// TestStoreSourceListedViewChecks observes a listed scope through views that ActiveView never returns:
// a view of another capture or hour, a view naming a pack not opened or twice, and an error;
// each fails Observe, every object opened closed once.
// ActiveView is given the hour's packs in ascending pack_id byte order, whatever their key order or the order they were opened,
// and its commit set.
func TestStoreSourceListedViewChecks(t *testing.T) {
	t.Parallel()

	viewErr := errors.New("view failed")
	tests := []struct {
		name string
		edit func(v View) (View, error)
		is   error
		want string
	}{
		{name: "another capture", edit: func(v View) (View, error) { v.Capture = captureHigh; return v, nil }, want: "the active view is of capture"},
		{name: "another hour", edit: func(v View) (View, error) { v.Hour++; return v, nil }, want: "the active view is of capture"},
		{name: "a pack not opened", edit: func(v View) (View, error) { v.Packs = append(v.Packs, memD); return v, nil }, want: "not one opened, or twice"},
		{name: "a pack twice", edit: func(v View) (View, error) { v.Packs = append(v.Packs, v.Packs[0]); return v, nil }, want: "not one opened, or twice"},
		{name: "an error", edit: func(View) (View, error) { return View{}, viewErr }, is: viewErr},
	}
	setup := func(t testing.TB) (*memStore, *fakeCatalog) {
		t.Helper()

		store, cat := listedStore()
		// The patch's key, under archive/, sorts before the segment's, under staging/; their pack_ids sort the other way.
		store.putPack(t, memTestHour, storeSegment(t, seg1, nil, 1))
		store.putPack(t, memTestHour, storeSegment(t, patP1, func(m *PackMeta) { m.PackRole, m.Supersedes = PackRoleRepair, []UUID{memD} }, 1))
		store.putCommit(t, captureLow, memTestHour, patP1)

		return store, cat
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store, cat := setup(t)
			src := newTestStoreSource(t, store, cat, nil)
			src.activeView = func(packs []PackInfo, commits CommitSet) (View, error) {
				v, err := ActiveView(packs, commits)
				require.NoError(t, err)

				return tt.edit(v)
			}
			o, err := src.Observe(t.Context(), captureLow, memTestHour, memTestHour+1)
			require.Error(t, err)
			require.Nil(t, o)
			if tt.is != nil {
				require.ErrorIs(t, err, tt.is)
			}
			if tt.want != "" {
				require.ErrorContains(t, err, tt.want)
			}
			requireClosedOnce(t, store)
		})
	}

	store, cat := setup(t)
	src := newTestStoreSource(t, store, cat, nil)
	var given []UUID
	var commits CommitSet
	src.activeView = func(packs []PackInfo, c CommitSet) (View, error) {
		for _, p := range packs {
			given = append(given, p.PackID())
		}
		commits = c

		return ActiveView(packs, c)
	}
	o := observeStore(t, src, memTestHour, memTestHour+1)
	assert.Equal(t, []UUID{seg1, patP1}, given, "ascending pack_id byte order")
	assert.Equal(t, CommitSet{patP1: {}}, commits)
	sc, err := o.Scope(t.Context(), memTestHour)
	require.NoError(t, err)
	assert.Equal(t, []UUID{seg1, patP1}, packIDs(sc.Readers))

	// A segment, opened before the archives, whose pack_id sorts after the patch's.
	late := UUID{0xE0}
	store, cat = listedStore()
	store.putPack(t, memTestHour, storeSegment(t, late, nil, 1))
	store.putPack(t, memTestHour, storeSegment(t, patP1, func(m *PackMeta) { m.PackRole, m.Supersedes = PackRoleRepair, []UUID{memD} }, 1))
	store.putCommit(t, captureLow, memTestHour, patP1)
	src = newTestStoreSource(t, store, cat, nil)
	given = nil
	src.activeView = func(packs []PackInfo, c CommitSet) (View, error) {
		for _, p := range packs {
			given = append(given, p.PackID())
		}

		return ActiveView(packs, c)
	}
	o = observeStore(t, src, memTestHour, memTestHour+1)
	assert.Equal(t, []UUID{patP1, late}, given, "ascending pack_id byte order, not the order opened")
	sc, err = o.Scope(t.Context(), memTestHour)
	require.NoError(t, err)
	assert.Equal(t, []UUID{patP1, late}, packIDs(sc.Readers))
}

// TestStoreSourceListedOrder reads a listed scope holding a segment and a committed patch that hold one identical record
// in blocks with equal first seq, the patch's key listed first and its pack_id after the segment's:
// the readers come in ascending pack_id order, and the record's representative is the segment, the first reader holding it.
func TestStoreSourceListedOrder(t *testing.T) {
	t.Parallel()

	store, cat := listedStore()
	store.putPack(t, memTestHour, txPack(t, seg1, nil, txBlock(txRecord(12, nil))))
	store.putPack(t, memTestHour, txPack(t, patP1, func(m *PackMeta) { m.PackRole, m.Supersedes = PackRoleRepair, []UUID{memD} }, txBlock(txRecord(12, nil))))
	store.putCommit(t, captureLow, memTestHour, patP1)
	require.Less(t, archiveKeyAt(t, memTestHour, patP1), segmentKeyAt(t, 12, seg1), "the patch's key sorts first")
	src := newTestStoreSource(t, store, cat, nil)
	o := observeStore(t, src, memTestHour, memTestHour+1)
	sc, err := o.Scope(t.Context(), memTestHour)
	require.NoError(t, err)
	assert.Equal(t, []UUID{seg1, patP1}, packIDs(sc.Readers))

	res, err := FindTransaction(t.Context(), src, txKeyAt(12), TxOptions{MaxScopes: 1})
	require.NoError(t, err)
	require.NotEmpty(t, res.Records)
	assert.Equal(t, seg1, res.Records[0].Pack, "the first reader holding the record represents it")
	assert.False(t, res.Records[0].Conflict)
}

// TestStoreSourceListedSupersession observes listed scopes through a StoreSource and through the memSource it mirrors:
// a patch of a patch; a repaired generation whose member is listed, and whose member is gone, which leaves the set incomplete;
// a committed set whose member is missing; a stale patch of a lower generation and an uncommitted patch;
// a coverage-only repair of a segment; and a generation's predecessors deleted in each order.
// Each listing view holds the packs the memSource's view does, and the ones stated.
func TestStoreSourceListedSupersession(t *testing.T) {
	t.Parallel()

	seqs := func(s ...uint64) []footerTestStep { return txBlock(txSeqs(s...)...) }
	gen := func(t testing.TB, id, set UUID, epoch, g uint64, from ...UUID) []byte {
		return txPack(t, id, func(m *PackMeta) { generationMeta(m, set, epoch, g, from) }, seqs(1, 2))
	}
	patch := func(t testing.TB, id UUID, base *UUID, supersedes ...UUID) []byte {
		return listedPatch(t, id, 0, base, supersedes, seqs(1, 2))
	}
	coverage := func(t testing.TB, id UUID, base *UUID, supersedes UUID) []byte {
		first, last, start, end := uint64(3), uint64(4), blockTestHour, blockTestHour+10
		return txPack(t, id, func(m *PackMeta) {
			m.PackRole, m.PatchBase, m.Supersedes = PackRoleRepair, base, []UUID{supersedes}
			m.Coverage = []Coverage{{CaptureID: new(captureLow), SeqFirst: &first, SeqLast: &last, TimeStart: &start, TimeEnd: &end}}
		}, nil)
	}
	type scenario struct {
		name    string
		packs   func(t testing.TB) [][]byte
		commits []UUID
		want    []UUID
	}
	scenarios := make([]scenario, 0, 10)
	scenarios = append(scenarios, []scenario{
		{name: "a patch of a patch", packs: func(t testing.TB) [][]byte {
			return [][]byte{gen(t, memA, setA, 1, 1), patch(t, patP1, new(setA), memA), patch(t, patP2, new(setA), patP1)}
		}, commits: []UUID{setA, patP1, patP2}, want: []UUID{patP2}},
		{name: "a repaired generation", packs: func(t testing.TB) [][]byte {
			return [][]byte{gen(t, memA, setA, 1, 1), patch(t, patP1, new(setA), memA), storeSegment(t, seg0, nil, 3)}
		}, commits: []UUID{setA, patP1}, want: []UUID{seg0, patP1}},
		{name: "a repaired generation whose member is gone", packs: func(t testing.TB) [][]byte {
			return [][]byte{patch(t, patP1, new(setA), memA), storeSegment(t, seg0, nil, 3)}
		}, commits: []UUID{setA, patP1}, want: []UUID{seg0}},
		{name: "an incomplete committed set", packs: func(t testing.TB) [][]byte {
			return [][]byte{gen(t, memA, setA, 1, 1)}
		}, commits: []UUID{setA, setB}, want: []UUID{memA}},
		{name: "stale and uncommitted patches", packs: func(t testing.TB) [][]byte {
			return [][]byte{gen(t, memA, setA, 1, 1), gen(t, memB, setB, 1, 2), patch(t, patP1, new(setA), memA), patch(t, patP2, new(setB), memB)}
		}, commits: []UUID{setA, setB, patP1}, want: []UUID{memB}},
		{name: "a coverage-only repair", packs: func(t testing.TB) [][]byte {
			return [][]byte{gen(t, memA, setA, 1, 1), storeSegment(t, seg0, nil, 3, 4), coverage(t, patP1, new(setA), seg0)}
		}, commits: []UUID{setA, patP1}, want: []UUID{memA, patP1}},
	}...)
	for _, deleted := range [][]UUID{nil, {memA}, {seg0}, {memA, seg0}} {
		scenarios = append(scenarios, scenario{name: fmt.Sprintf("predecessors %v deleted", len(deleted)), packs: func(t testing.TB) [][]byte {
			all := map[UUID][]byte{memA: gen(t, memA, setA, 1, 1), seg0: storeSegment(t, seg0, nil, 1, 2), memB: gen(t, memB, setB, 1, 2, memA, seg0)}
			for _, id := range deleted {
				delete(all, id)
			}
			out := make([][]byte, 0, len(all))
			for _, id := range []UUID{memA, seg0, memB} {
				if f, ok := all[id]; ok {
					out = append(out, f)
				}
			}

			return out
		}, commits: []UUID{setA, setB}, want: []UUID{memB}})
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()

			mem := newMemSource()
			for _, f := range sc.packs(t) {
				mem.stagePack(t, memTestHour, f)
			}
			for _, id := range sc.commits {
				mem.commit(captureLow, memTestHour, id)
			}
			store, cat := mirrorMemSource(t, mem, captureLow, memTestHour, memTestHour+1)
			o := observeStore(t, newTestStoreSource(t, store, cat, nil), memTestHour, memTestHour+1)
			got, err := o.Scope(t.Context(), memTestHour)
			require.NoError(t, err)
			mo, err := mem.Observe(t.Context(), captureLow, memTestHour, memTestHour+1)
			require.NoError(t, err)
			defer func() { require.NoError(t, mo.Close()) }()
			want, err := mo.Scope(t.Context(), memTestHour)
			require.NoError(t, err)
			assert.Equal(t, packIDs(want.Readers), packIDs(got.Readers))
			assert.Equal(t, sc.want, packIDs(got.Readers))
		})
	}
}

// TestStoreSourceSettled observes one set of packs and commit objects as an indexed scope, its view the catalog's, and as a listed one:
// a settled scope, a generation alone; and a scope not settled, with a committed patch not yet folded and a superseded generation awaiting deletion.
// Both listing views equal the catalog's views.
func TestStoreSourceSettled(t *testing.T) {
	t.Parallel()

	for _, settled := range []bool{true, false} {
		mem := newMemSource()
		mem.addPack(t, memTestHour, txPack(t, memB, func(m *PackMeta) { generationMeta(m, setB, 1, 2, nil) }, txBlock(txSeqs(1, 2)...)))
		mem.commit(captureLow, memTestHour, setB)
		if !settled {
			mem.addPack(t, memTestHour, txPack(t, memA, func(m *PackMeta) { generationMeta(m, setA, 1, 1, nil) }, txBlock(txSeqs(1)...)))
			mem.commit(captureLow, memTestHour, setA)
			mem.addPack(t, memTestHour, listedPatch(t, patP1, 0, new(setB), []UUID{memB}, txBlock(txSeqs(1, 2)...)))
			mem.commit(captureLow, memTestHour, patP1)
		}
		readers := map[bool][]UUID{}
		for _, indexed := range []bool{true, false} {
			mem.setIndexed(captureLow, memTestHour, indexed)
			store, cat := mirrorMemSource(t, mem, captureLow, memTestHour, memTestHour+1)
			o := observeStore(t, newTestStoreSource(t, store, cat, nil), memTestHour, memTestHour+1)
			sc, err := o.Scope(t.Context(), memTestHour)
			require.NoError(t, err)
			assert.Equal(t, indexed, sc.Indexed)
			readers[indexed] = packIDs(sc.Readers)
		}
		assert.Equal(t, readers[true], readers[false], "settled %v", settled)
		want := []UUID{memB}
		if !settled {
			want = []UUID{patP1}
		}
		assert.Equal(t, want, readers[false], "settled %v", settled)
	}
}

// TestStoreSourceListedConcurrent runs observations of one source over listed scopes concurrently, sharing its adapters,
// each reporting the retired archive of its scope once to a callback the race detector watches.
func TestStoreSourceListedConcurrent(t *testing.T) {
	t.Parallel()

	store, cat := listedStore()
	store.putPack(t, memTestHour, storeSegment(t, seg0, nil, 1))
	store.putPack(t, memTestHour, setMetaRole(t, storeArchive(t, memA, setA, 1), packRoleRetired))
	var mu sync.Mutex
	reports := 0
	src := newTestStoreSource(t, store, cat, func(o *StoreSourceOptions) {
		o.OnExcluded = func(ExcludedPack) {
			mu.Lock()
			reports++
			mu.Unlock()
		}
	})
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Go(func() {
			o, err := src.Observe(t.Context(), captureLow, memTestHour, memTestHour+1)
			if err == nil {
				err = o.Close()
			}
			errs[i] = err
		})
	}
	wg.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}
	assert.Equal(t, len(errs), reports)
	requireClosedOnce(t, store)
}
