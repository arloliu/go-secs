package tracepack

import (
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unsafe"

	"github.com/arloliu/go-secs/tracepack/internal/tlv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// storeRecount returns the charge of the state the observation o keeps, recounted from that state alone:
// the observation and its hours, each retained Reader, and the copied evidence and barriers.
// After every Observe it equals what Observe charged, since every charge of a descriptor is released once the observation is built.
func storeRecount(o *storeObservation) int64 {
	n := storeObservationCharge + int64(len(o.scopes))*storeScopeCharge
	for _, sc := range o.scopes {
		for _, r := range sc.readers {
			n += readerCost(r)
		}
	}
	for i := range o.evidence.Boundaries {
		n += boundaryCost(&o.evidence.Boundaries[i])
	}
	n += int64(len(o.evidence.Closures)) * storeClosureCharge
	for i := range o.barriers {
		n += boundaryCost(&o.barriers[i])
	}

	return n
}

// deepSizer measures the bytes a value holds, each pointed-to value once.
type deepSizer struct {
	seen map[uintptr]bool
}

// deepSize returns the bytes the value p points to holds: the value itself and everything it reaches,
// slices at their lengths, strings at their lengths, map entries at their key and value sizes.
// It counts nothing behind an interface:
// a Reader's object is the store's, and an error's text is not charged.
func deepSize(p any) int64 {
	v := reflect.ValueOf(p)
	s := &deepSizer{seen: map[uintptr]bool{}}

	return s.of(v)
}

// of returns the bytes v reaches beyond its own storage, which its holder counts; a pointer counts what it points to.
func (s *deepSizer) of(v reflect.Value) int64 {
	var n int64
	switch v.Kind() { //nolint:exhaustive // every other kind holds nothing beyond its holder, or is not charged
	case reflect.Pointer:
		if v.IsNil() || s.seen[v.Pointer()] {
			return 0
		}
		s.seen[v.Pointer()] = true
		n = int64(v.Type().Elem().Size()) + s.of(v.Elem())
	case reflect.Struct:
		for _, field := range v.Fields() {
			n += s.of(field)
		}
	case reflect.Array:
		for i := range v.Len() {
			n += s.of(v.Index(i))
		}
	case reflect.Slice:
		n = int64(v.Len()) * int64(v.Type().Elem().Size())
		for i := range v.Len() {
			n += s.of(v.Index(i))
		}
	case reflect.String:
		n = int64(v.Len())
	case reflect.Map:
		entry := int64(v.Type().Key().Size() + v.Type().Elem().Size())
		for it := v.MapRange(); it.Next(); {
			n += entry + s.of(it.Key()) + s.of(it.Value())
		}
	default:
		// Scalars live in their holder; interfaces are not charged.
	}

	return n
}

// fillValue sets every field v reaches to a value that is not zero:
// strings of eight bytes, slices of two elements, every pointer to a filled value, every number 7,
// so a field added to a type is filled too.
func fillValue(v reflect.Value) {
	switch v.Kind() { //nolint:exhaustive // a kind PackMeta does not hold panics
	case reflect.String:
		v.SetString("abcdefgh")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(7)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(7)
	case reflect.Array:
		for i := range v.Len() {
			fillValue(v.Index(i))
		}
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 2, 2))
		for i := range v.Len() {
			fillValue(v.Index(i))
		}
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fillValue(v.Elem())
	case reflect.Struct:
		for _, field := range v.Fields() {
			fillValue(field)
		}
	default:
		panic(fmt.Sprintf("fillValue: kind %v", v.Kind()))
	}
}

// filledPackMeta returns a PackMeta whose every field, nested ones included, is set.
func filledPackMeta(t testing.TB) *PackMeta {
	t.Helper()

	m := &PackMeta{}
	fillValue(reflect.ValueOf(m).Elem())
	v := reflect.ValueOf(*m)
	for i := range v.NumField() {
		require.False(t, v.Field(i).IsZero(), "PackMeta.%s is set", v.Type().Field(i).Name)
	}

	return m
}

// overlappingF3Segment returns segment seg0 of captureLow in memTestHour, three blocks of two records,
// with its F-3 section rebuilt so that block 1's list lies inside block 0's, as the value of an unknown entry,
// which the footer validation of the tracepack format specification §10 accepts;
// each block parses its own list.
func overlappingF3Segment(t testing.TB) []byte {
	t.Helper()

	file := txPack(t, seg0, nil, slices.Concat(txBlock(txSeqs(1, 2)...), txBlock(txSeqs(3, 4)...), txBlock(txSeqs(5, 6)...)))
	_, decoded, _ := splitPack(t, file)
	parts := splitFooter(t, decoded)
	lists := parts.f3Raw
	require.Len(t, lists, 3)
	list0 := tlv.AppendEntry(slices.Clone(lists[0]), tlv.BytesEntry(0x0101, lists[1]))
	spans := []f3Span{
		{offset: 0, n: uint32(len(list0))},
		{offset: uint64(len(list0) - len(lists[1])), n: uint32(len(lists[1]))},
		{offset: uint64(len(list0)), n: uint32(len(lists[2]))},
	}
	out := refooter(t, file, parts.encodeF3(slices.Concat(list0, lists[2]), spans))
	r := mustOpen(t, out, ReaderOptions{})
	require.NoError(t, r.Header().FooterErr)
	require.Len(t, r.footer.blocks, 3)

	return out
}

// TestStoreCostSufficient compares each cost function of a store source's charges with the bytes its structure holds:
// Readers with a valid footer, its F-5 count arrays extended with zeros, its F-3 lists overlapping, or kept,
// and Readers that walked their blocks, one of them with a filled PackMeta;
// a PackMeta with every field set; a block summary with an epoch index; a defect with a coverage entry; a boundary;
// a listed key and a PackInfo.
// No charge is below what its structure holds.
func TestStoreCostSufficient(t *testing.T) {
	t.Parallel()

	rich := richStatsPack(t, captureLow)
	_, decoded, _ := splitPack(t, rich)
	p := splitFooter(t, decoded)
	for _, tag := range []uint16{f5TagKindCounts, f5TagDirCounts, f5TagDecodeStatusCounts} {
		e := nthEntry(t, p.f5, tag, 0)
		p.f5 = replaceEntry(t, p.f5, tag, 0, tlv.BytesEntry(tag, append(slices.Clone(e.Value), make([]byte, 64*8)...)))
	}
	extended := refooter(t, rich, p.encode())
	overlapping, _ := overlappingF3Pack(t)
	walked := writeRepairPack(t, CodecZstd, nil, true, seqSteps(1, 200, 1, blockTestHour)).file

	readers := map[string]*Reader{
		"footer":      mustOpen(t, rich, ReaderOptions{}),
		"extended":    mustOpen(t, extended, ReaderOptions{}),
		"overlapping": mustOpen(t, overlapping, ReaderOptions{}),
		"kept F-3":    mustOpenKeepingF3(t, overlapping, ReaderOptions{}),
		"walked":      mustOpen(t, walked, ReaderOptions{}),
		"walk limit":  mustOpen(t, walked, ReaderOptions{MaxWalkedBlocks: 3}),
	}
	filled := mustOpen(t, walked, ReaderOptions{})
	filled.meta = filledPackMeta(t)
	readers["filled meta"] = filled
	require.NotNil(t, readers["extended"].footer)
	require.Greater(t, len(readers["extended"].footer.stats.kindCounts), 64)
	require.Nil(t, readers["walked"].footer)
	require.Len(t, readers["walked"].blocks, 200)
	require.NotEmpty(t, readers["walked"].openDefects)
	require.NotNil(t, readers["walk limit"].walkStop)
	// A retained Reader also takes a slot among its scope's readers and one among the objects to close.
	slots := int64(unsafe.Sizeof((*Reader)(nil)) + unsafe.Sizeof(Object(nil)))
	for name, r := range readers {
		assert.GreaterOrEqual(t, readerCost(r), deepSize(r)+slots, name)
		if r.footer != nil {
			assert.GreaterOrEqual(t, footerIndexCost(r.footer), deepSize(r.footer), name)
			assert.GreaterOrEqual(t, packStatsCost(r.footer.stats), deepSize(r.footer.stats), name)
			for i := range r.footer.blocks {
				assert.GreaterOrEqual(t, blockSummaryCost(&r.footer.blocks[i]), deepSize(&r.footer.blocks[i]), "%s block %d", name, i)
			}
		}
	}
	assert.Equal(t, readerCost(readers["footer"])+3*64*storeUint64Charge, readerCost(readers["extended"]),
		"the F-5 count arrays are charged as stored")

	m := filledPackMeta(t)
	assert.GreaterOrEqual(t, packMetaCost(m), deepSize(m))

	s := readers["footer"].footer.blocks[0]
	s.epochIndex = map[uint32]int{1: 0, 2: 1, 3: 2}
	assert.GreaterOrEqual(t, blockSummaryCost(&s), deepSize(&s))

	d := &Defect{}
	fillValue(reflect.ValueOf(d).Elem().FieldByName("Coverage"))
	assert.GreaterOrEqual(t, defectCost(d), deepSize(d))

	b := &Boundary{GapStart: new(int64(1)), GapEnd: new(int64(2))}
	assert.GreaterOrEqual(t, boundaryCost(b), deepSize(b))
	sc := &CatalogScope{Token: strings.Repeat("t", 100)}
	assert.GreaterOrEqual(t, scopeDescriptorCost(sc), deepSize(sc))
	cp := &CatalogPack{Key: strings.Repeat("k", 100)}
	assert.GreaterOrEqual(t, packDescriptorCost(cp), deepSize(cp))
	key := strings.Repeat("k", 100)
	l := &storeListed{key: key, size: 1, k: storePackKey{tool: memStoreTool}}
	assert.GreaterOrEqual(t, listedKeyCost(key), deepSize(l)-int64(len(l.k.tool)), "the tool is the source's own")
	assert.GreaterOrEqual(t, storeOpenedCharge, int64(unsafe.Sizeof(storeOpened{})), "its Reader and its key are charged apart")
	info := filled.Info()
	info.meta = filledPackMeta(t)
	assert.GreaterOrEqual(t, packInfoCost(&info), deepSize(&info)-int64(len(info.raw)), "its bytes are the Reader's")
}

// TestStoreSourceLimits observes, for each case, with MaxSourceBytes at the largest charge the first Observe held and one byte below it:
// footerless packs with large walked indexes and tiny pack metadata, a valid footer with overlapping F-3 lists,
// a large range of empty indexed scopes,
// a long prefix and a long token, and large evidence and barriers without packs;
// and in listed scopes, candidate packs of an empty view, long listed keys and tokens, and long confirmation tokens.
// At the limit Observe succeeds, its charges as the case states and as recounted;
// one byte below, it fails with ErrReadLimit, every object opened closed once.
func TestStoreSourceLimits(t *testing.T) {
	t.Parallel()

	longPrefix := strings.Repeat("p", 4096)
	tests := []struct {
		name   string
		prefix string
		hours  int64
		setup  func(t testing.TB, store *memStore, cat *fakeCatalog)
		// check checks the charges of o.
		check func(t testing.TB, o *storeObservation)
	}{
		{
			name: "walked packs", hours: 1,
			setup: func(t testing.TB, store *memStore, cat *fakeCatalog) {
				packs := make([]CatalogPack, 0, 2)
				for i, id := range []UUID{seg0, seg1} {
					file := writeRepairPack(t, CodecZstd, nil, true, seqSteps(uint64(1000*i+1), uint64(1000*i+300), 1, blockTestHour)).file
					packs = append(packs, store.putPack(t, memTestHour, withIDs(t, file, id, captureLow)))
				}
				cat.setView(captureLow, memTestHour, packs...)
			},
			check: func(t testing.TB, o *storeObservation) {
				readers := o.scopes[0].readers
				require.Len(t, readers, 2)
				for _, r := range readers {
					require.Nil(t, r.footer)
					require.Len(t, r.blocks, 300)
					assert.Greater(t, readerCost(r), 300*storeBlockInfoCharge)
				}
				assert.Equal(t, storeObservationCharge+storeScopeCharge+readerCost(readers[0])+readerCost(readers[1]), o.charged)
			},
		},
		{
			name: "overlapping F-3 lists", hours: 1,
			setup: func(t testing.TB, store *memStore, cat *fakeCatalog) {
				cat.setView(captureLow, memTestHour, store.putPack(t, memTestHour, overlappingF3Segment(t)))
			},
			check: func(t testing.TB, o *storeObservation) {
				readers := o.scopes[0].readers
				require.Len(t, readers, 1)
				r := readers[0]
				require.NotNil(t, r.footer)
				n := int64(0)
				for i := range r.footer.blocks {
					n += blockSummaryCost(&r.footer.blocks[i])
				}
				assert.Greater(t, readerCost(r), n, "each block's summary is charged, whatever its list shares")
				assert.Equal(t, storeObservationCharge+storeScopeCharge+readerCost(r), o.charged)
			},
		},
		{
			name: "empty scopes", hours: 3000,
			check: func(t testing.TB, o *storeObservation) {
				assert.Equal(t, storeObservationCharge+3000*storeScopeCharge, o.charged)
				assert.Equal(t, 3000*storeScopeDescriptorCharge, o.peak-o.charged, "the descriptors are released")
			},
		},
		{
			name: "a long prefix and a long token", prefix: longPrefix, hours: 2,
			setup: func(t testing.TB, store *memStore, cat *fakeCatalog) {
				key, err := SegmentKey(longPrefix, memStoreTool, captureLow, 1, seg0)
				require.NoError(t, err)
				cat.setView(captureLow, memTestHour, store.putAt(t, key, storeSegment(t, seg0, nil, 1)))
				cat.editSnapshot = func(s *CatalogSnapshot) { s.Scopes[1].Token = strings.Repeat("t", 8192) }
			},
			check: func(t testing.TB, o *storeObservation) {
				key := len(longPrefix) + len("/staging/tool/") + uuidKeyLen + 1 + segmentFileLen
				assert.Equal(t, 2*storeScopeDescriptorCharge+8192+storePackDescriptorCharge+int64(key), o.peak-o.charged)
			},
		},
		{
			name: "candidates of an empty listing view", hours: 2,
			setup: func(t testing.TB, store *memStore, cat *fakeCatalog) {
				cat.setIndexed(captureLow, memTestHour, false)
				// Generations without commit objects: the view holds none of them.
				for i, id := range []UUID{memA, memB, memC, memD} {
					set := UUID{0x5E, 0x10, byte(i)}
					store.putPack(t, memTestHour, storeArchive(t, id, set, 1, 2, 3))
				}
			},
			check: func(t testing.TB, o *storeObservation) {
				assert.Empty(t, o.scopes[0].readers)
				assert.Equal(t, storeObservationCharge+2*storeScopeCharge, o.charged, "every candidate released")
				assert.Greater(t, o.peak-o.charged, 4*storeReaderCharge, "the candidates were charged")
			},
		},
		{
			name: "long listed keys and tokens", prefix: longPrefix, hours: 2,
			setup: func(t testing.TB, store *memStore, cat *fakeCatalog) {
				store.pageSize = 1
				cat.setIndexed(captureLow, memTestHour, false)
				// Walked segments, whose Readers outweigh the listing's keys and tokens.
				for i, id := range []UUID{seg0, seg1, seg2} {
					first := uint64(1000*i + 1)
					key, err := SegmentKey(longPrefix, memStoreTool, captureLow, first, id)
					require.NoError(t, err)
					file := writeRepairPack(t, CodecZstd, nil, true, seqSteps(first, first+299, 1, blockTestHour)).file
					store.putAt(t, key, withIDs(t, file, id, captureLow))
				}
			},
			check: func(t testing.TB, o *storeObservation) {
				readers := o.scopes[0].readers
				require.Len(t, readers, 3)
				key := int64(len(longPrefix) + len("/staging/tool/") + uuidKeyLen + 1 + segmentFileLen)
				require.Greater(t, readerCost(readers[0]), 3*(storeListedCharge+key)+2*(storeStringCharge+key))
				assert.Equal(t, storeObservationCharge+2*storeScopeCharge+readerCost(readers[0])+readerCost(readers[1])+readerCost(readers[2]), o.charged)
				// Every key is held from the staging listing until its pack is opened, with the listing's two tokens, each a key.
				assert.GreaterOrEqual(t, o.peak-storeObservationCharge-2*storeScopeCharge, 3*(storeListedCharge+key)+2*(storeStringCharge+key))
				// Each key stays held with its Reader, its entry and its pack_id until the view is fixed.
				var held int64
				for _, r := range readers {
					held += readerCost(r) + storeListedCharge + key + storeOpenedCharge + storeUUIDCharge
				}
				assert.GreaterOrEqual(t, o.peak-storeObservationCharge-2*storeScopeCharge, held)
			},
		},
		{
			name: "long confirmation tokens", hours: 3,
			setup: func(_ testing.TB, _ *memStore, cat *fakeCatalog) {
				cat.tokenLen = 8192
				for h := range int64(3) {
					cat.setIndexed(captureLow, memTestHour+h, false)
				}
			},
			check: func(t testing.TB, o *storeObservation) {
				assert.Equal(t, storeObservationCharge+3*storeScopeCharge, o.charged, "each token released at its confirmation")
				assert.Equal(t, 3*(storeScopeDescriptorCharge+8192), o.peak-o.charged)
			},
		},
		{
			name: "large evidence and barriers", hours: 2,
			setup: func(_ testing.TB, _ *memStore, cat *fakeCatalog) {
				var e CaptureEvidence
				for i := range 500 {
					b := Boundary{Capture: captureLow, Seq: uint64(i), Kind: BoundaryKindGap}
					if i%2 == 0 {
						b.GapStart, b.GapEnd = new(int64(i)), new(int64(i+1))
					}
					e.Boundaries = append(e.Boundaries, b)
					cat.addBarrier(Boundary{Capture: captureHigh, Seq: uint64(i), Kind: BoundaryKindStopUnclean, GapEnd: new(blockTestHour)})
				}
				for i := range 300 {
					e.Closures = append(e.Closures, EpochClosure{Epoch: uint32(i), CloseSeq: uint64(i)})
				}
				cat.setEvidence(captureLow, e)
			},
			check: func(t testing.TB, o *storeObservation) {
				require.Len(t, o.barriers, 500)
				bound := pointerCost(new(int64(0)))
				want := storeObservationCharge + 2*storeScopeCharge +
					250*(storeBoundaryCharge+2*bound) + 250*storeBoundaryCharge + 300*storeClosureCharge +
					500*(storeBoundaryCharge+bound)
				assert.Equal(t, want, o.charged)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store, cat := newMemStore(), newFakeCatalog()
			if tt.setup != nil {
				tt.setup(t, store, cat)
			}
			observe := func(limit int64) (*storeObservation, error) {
				t.Helper()

				src := newTestStoreSource(t, store, cat, func(o *StoreSourceOptions) {
					o.MaxSourceBytes = limit
					if tt.prefix != "" {
						o.Prefix = tt.prefix
					}
				})
				obs, err := src.Observe(t.Context(), captureLow, memTestHour, memTestHour+tt.hours)
				if err != nil {
					return nil, err
				}
				o := asStoreObservation(t, obs)
				requireStoreCharges(t, o)

				return o, nil
			}

			o, err := observe(math.MaxInt64)
			require.NoError(t, err)
			tt.check(t, o)
			require.NoError(t, o.Close())
			peak := o.peak

			o, err = observe(peak)
			require.NoError(t, err)
			assert.Equal(t, peak, o.peak)
			tt.check(t, o)
			require.NoError(t, o.Close())

			_, err = observe(peak - 1)
			require.ErrorIs(t, err, ErrReadLimit)
			require.ErrorContains(t, err, "MaxSourceBytes")
			requireClosedOnce(t, store)
		})
	}
}

// TestStoreSourceMaxObjects observes a view of three packs with MaxObjects 3 and 2:
// at the limit Observe succeeds; one below, it fails with ErrReadLimit before opening the third pack,
// the others closed once.
func TestStoreSourceMaxObjects(t *testing.T) {
	t.Parallel()

	store, cat := newMemStore(), newFakeCatalog()
	packs := make([]CatalogPack, 0, 3)
	for i, id := range []UUID{seg0, seg1, seg2} {
		packs = append(packs, store.putPack(t, memTestHour, storeSegment(t, id, nil, uint64(i+1))))
	}
	cat.setView(captureLow, memTestHour, packs...)

	o, err := observeOne(t, store, cat, func(o *StoreSourceOptions) { o.MaxObjects = 3 })
	require.NoError(t, err)
	require.NoError(t, o.Close())
	_, err = observeOne(t, store, cat, func(o *StoreSourceOptions) { o.MaxObjects = 2 })
	require.ErrorIs(t, err, ErrReadLimit)
	require.ErrorContains(t, err, "more than MaxObjects 2 objects")
	opens, _ := store.openCounts()
	assert.Equal(t, map[string]int{packs[0].Key: 2, packs[1].Key: 2, packs[2].Key: 1}, opens)
}

// TestStoreBudget charges and counts at a budget's limits: a charge or a count that would pass its limit fails,
// charging or counting nothing, also for the largest charge, so neither counter can wrap; a release returns the charge.
func TestStoreBudget(t *testing.T) {
	t.Parallel()

	b := storeBudget{maxBytes: 10, maxObjects: 1}
	require.NoError(t, b.reserve(4))
	require.ErrorIs(t, b.reserve(7), ErrReadLimit)
	require.ErrorIs(t, b.reserve(math.MaxInt64), ErrReadLimit)
	assert.Equal(t, int64(4), b.used)
	require.NoError(t, b.reserve(6))
	assert.Equal(t, int64(10), b.peak)
	b.release(6)
	assert.Equal(t, int64(4), b.used)
	assert.Equal(t, int64(10), b.peak)

	require.NoError(t, b.count())
	require.ErrorIs(t, b.count(), ErrReadLimit)
	assert.Equal(t, 1, b.objects)

	full := storeBudget{maxBytes: math.MaxInt64, maxObjects: math.MaxInt}
	full.used, full.objects = math.MaxInt64-1, math.MaxInt-1
	require.NoError(t, full.reserve(1))
	require.ErrorIs(t, full.reserve(1), ErrReadLimit)
	require.NoError(t, full.count())
	require.ErrorIs(t, full.count(), ErrReadLimit)
	assert.Equal(t, int64(math.MaxInt64), full.used)
	assert.Equal(t, math.MaxInt, full.objects)
}
