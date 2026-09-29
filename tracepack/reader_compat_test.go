package tracepack

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/format"
	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// compatUnknown is an enum value outside every registry of the tracepack format specification §9,
// as a newer minor version may define one.
const compatUnknown = 200

// compatConfig is one Writer configuration the forward-compatibility vectors are built from.
type compatConfig struct {
	name     string
	codec    Codec
	validate bool
}

// compatRead is what reading one pack with compatQueries gives.
type compatRead struct {
	r    *Reader
	runs []iterRun
	// pruned holds, per query, the blocks the footer excludes from it.
	pruned [][]int
}

// compatConfigs returns the configurations of the vectors: codec none, and codec zstd validated by the Writer.
func compatConfigs() []compatConfig {
	return []compatConfig{
		{name: "none", codec: CodecNone},
		{name: "zstd validated", codec: CodecZstd, validate: true},
	}
}

// compatMetaEntries returns pack metadata entries that no registry of this package knows,
// which a reader skips and preserves (the tracepack format specification §5):
// an unregistered public tag, a private tag, and a private tag of a value type no registry defines.
func compatMetaEntries() []RawEntry {
	return []RawEntry{
		{Tag: 0x0100, Type: uint8(tlv.TypeU64), Value: []byte{1, 2, 3, 4, 5, 6, 7, 0}},
		{Tag: 0x8001, Type: uint8(tlv.TypeBytes), Value: []byte("private")},
		{Tag: 0x8002, Type: 0x2A, Value: []byte{0xA1, 0xA2, 0xA3}},
	}
}

// compatRecords returns the records of the vector packs, which compatPack writes as three blocks,
// with a value beyond the registry in each of kind, dir, fidelity and decode_status:
//
//	block 0, hour 0: 0 data, 1 kind 200, 2 control, then Flush
//	block 1, hour 0: 3 socket-close transport event, 4 data of dir 200, 5 short capture of 6 bytes
//	block 2, hour 1: 6 data of fidelity 200, 7 data of decode_status 200, 8 data
func compatRecords(t testing.TB) []Record {
	t.Helper()

	h1 := blockTestHour + hourNs
	unknownKind := testDataRecord(1, blockTestHour+20, 1)
	unknownKind.Kind = Kind(compatUnknown)
	unknownDir := testDataRecord(4, blockTestHour+50, 1)
	unknownDir.Dir = Dir(compatUnknown)
	unknownFidelity := testDataRecord(6, h1+1, 2)
	unknownFidelity.Fidelity = Fidelity(compatUnknown)
	unknownStatus := testDataRecord(7, h1+2, 2)
	unknownStatus.DecodeStatus = DecodeStatus(compatUnknown)

	return []Record{
		testDataRecord(0, blockTestHour+10, 1),
		unknownKind,
		controlRecord(2, blockTestHour+30, 1, DirHostToEquipment),
		testEventRecord(t, 3, blockTestHour+40, 1, &TransportEvent{Event: EventSocketClose}),
		unknownDir,
		shortCapture(5, blockTestHour+60, 6),
		unknownFidelity,
		unknownStatus,
		testDataRecord(8, h1+3, 2),
	}
}

// compatPack writes compatRecords with cfg; meta, when set, adjusts the pack metadata before the Writer is created.
func compatPack(t testing.TB, cfg compatConfig, meta func(*PackMeta)) []byte {
	t.Helper()

	return writeFlushedPack(t, cfg.codec, cfg.validate, meta, compatRecords(t), 2)
}

// Indexes into compatQueries of the queries whose results the vectors check beyond completeness.
const (
	compatQuerySF          = 2
	compatQueryUnknownKind = 4
	compatQueryUnknownDir  = 5
)

// compatQueries returns the queries every vector is read with:
// every record with and without payloads, an HSMS header query,
// a time query and queries on the unknown kind and dir, which the footer answers by pruning,
// and a kind query with payloads.
func compatQueries() []Query {
	h1 := blockTestHour + hourNs
	s1f3 := Filter{SF: []SF{{Stream: 1, Function: 3}}}

	return []Query{
		{Payloads: true},
		{},
		{Filter: s1f3},
		{Filter: Filter{TimeFrom: new(h1), TimeTo: new(h1 + hourNs)}},
		{Filter: Filter{Kinds: []Kind{Kind(compatUnknown)}}},
		{Filter: Filter{Dirs: []Dir{Dir(compatUnknown)}}},
		{Payloads: true, Filter: Filter{Kinds: []Kind{KindControl, KindTransportEvent}}},
	}
}

// readCompat opens file and requires a finalized pack whose footer the Reader uses;
// then it runs every compat query and requires each result complete.
func readCompat(t *testing.T, file []byte) *compatRead {
	t.Helper()

	r := mustOpen(t, file, ReaderOptions{})
	h := r.Header()
	require.True(t, h.Finalized)
	require.NoError(t, h.FooterErr)
	for i, b := range r.Blocks() {
		require.True(t, b.Indexed, "block %d", i)
	}

	cr := &compatRead{r: r}
	for qi, q := range compatQueries() {
		run := iterate(t, r, q)
		require.True(t, run.res.Complete(), "query %d: %v", qi, run.res.Incomplete)
		require.NoError(t, run.res.FooterErr, "query %d", qi)

		var pruned []int
		for i := range r.blocks {
			if r.prunes(i, &q.Filter) {
				pruned = append(pruned, i)
			}
		}
		cr.runs, cr.pruned = append(cr.runs, run), append(cr.pruned, pruned)
	}

	return cr
}

// requireSameRead requires got to yield what want yields for every query,
// with the same Result and the same blocks pruned;
// extra, when set, gives the HeaderExtra each item of got must carry in place of want's.
func requireSameRead(t *testing.T, want, got *compatRead, extra func(it *iterItem) []byte) {
	t.Helper()

	for qi := range want.runs {
		items := slices.Clone(want.runs[qi].items)
		if extra != nil {
			for k := range items {
				items[k].extra = extra(&items[k])
			}
		}
		assert.Equal(t, items, got.runs[qi].items, "query %d", qi)
		assert.Equal(t, want.runs[qi].res, got.runs[qi].res, "query %d", qi)
		assert.Equal(t, want.pruned[qi], got.pruned[qi], "query %d", qi)
	}
}

// storedOf returns r as the Writer stores it and a full read yields it: its record header made canonical.
func storedOf(r *Record) Record {
	h := canonicalHeader(r, r.Seq, transportEventOf(r))

	return storedRecord(&h, r.Payload)
}

// editFooter returns file with its footer split into parts, changed by each of edits in turn, and re-encoded.
func editFooter(t testing.TB, file []byte, edits ...func(t testing.TB, p *footerParts)) []byte {
	t.Helper()

	_, decoded, _ := splitPack(t, file)
	parts := splitFooter(t, decoded)
	for _, edit := range edits {
		edit(t, parts)
	}

	return refooter(t, file, parts.encode())
}

// addFooterTags adds unknown tags to the footer of p:
// in every F-3 entry list an unregistered public tag, a tag reserved for the index structures, and a private tag,
// with an unknown nested tag in the block's first epoch entry;
// and in F-5 an unregistered public tag and a private tag (the tracepack format specification §10).
func addFooterTags(t testing.TB, p *footerParts) {
	t.Helper()

	public := tlv.BytesEntry(0x0100, []byte{1, 2, 3})
	reserved := tlv.U64Entry(0x0010, 7)
	private := tlv.BytesEntry(0x8001, []byte("private"))

	for i := range p.f3 {
		p.f3[i] = replaceEntry(t, p.f3[i], f3TagEpoch, 0, withNested(t, nthEntry(t, p.f3[i], f3TagEpoch, 0), 0x0040, public))
		p.f3[i] = append(p.f3[i], public, reserved, private)
	}
	p.f5 = append([]tlv.Entry{public, private}, p.f5...)
}

// lengthenEntries makes every F-2 entry of the footer of p 88 bytes long, the 8 bytes behind offset 80 non-zero
// (the tracepack format specification §10).
func lengthenEntries(_ testing.TB, p *footerParts) {
	p.entryPad = []byte{0xA1, 0xA2, 0xA3, 0xA4, 0xA5, 0xA6, 0xA7, 0xA8}
}

// widenFile returns file with every record header of the blocks wide selects widened to 64 bytes by wideExtra.
// The blocks keep their records, so they still satisfy I-2.
func widenFile(t testing.TB, file []byte, wide func(i int) bool) []byte {
	t.Helper()

	return rebuildPack(t, file, func(i int, b *testBlock) {
		if wide(i) {
			widen(b)
		}
	})
}

// wideExtraOf returns a function giving the HeaderExtra an item carries after widenFile widens the blocks wide selects:
// wideExtra of the item's position in its block, or nil in a block that is not widened.
func wideExtraOf(blocks []BlockInfo, wide func(i int) bool) func(it *iterItem) []byte {
	return func(it *iterItem) []byte {
		if !wide(it.block) {
			return nil
		}
		// The seqs of the compat packs are contiguous, so a record's position is its seq's distance from first_seq.
		return wideExtra(int(it.rec.Seq-blocks[it.block].FirstSeq), it.rec.Seq)
	}
}

func TestReaderCompatUnknownEnumValues(t *testing.T) {
	t.Parallel()

	for _, cfg := range compatConfigs() {
		t.Run(cfg.name, func(t *testing.T) {
			t.Parallel()

			recs := compatRecords(t)
			file := compatPack(t, cfg, nil)
			cr := readCompat(t, file)

			all := cr.runs[0]
			require.Len(t, all.items, len(recs))
			for k := range all.items {
				assert.Equal(t, storedOf(&recs[k]), all.items[k].rec, "record %d is yielded as written", k)
			}

			// Each unknown value is preserved and printed as unknown(n) (the tracepack format specification §1).
			assert.Equal(t, "unknown(200)", all.items[1].rec.Kind.String())
			assert.Equal(t, "unknown(200)", all.items[4].rec.Dir.String())
			assert.Equal(t, "unknown(200)", all.items[6].rec.Fidelity.String())
			assert.Equal(t, "unknown(200)", all.items[7].rec.DecodeStatus.String())

			// The footer counts the unknown values in F-3 and F-5, and still validates.
			f := cr.r.footer
			require.Greater(t, len(f.blocks[0].kindCounts), compatUnknown)
			assert.Equal(t, uint32(1), f.blocks[0].kindCounts[compatUnknown])
			require.Greater(t, len(f.blocks[1].dirCounts), compatUnknown)
			assert.Equal(t, uint32(1), f.blocks[1].dirCounts[compatUnknown])
			require.Greater(t, len(f.blocks[2].decodeStatusCounts), compatUnknown)
			assert.Equal(t, uint32(1), f.blocks[2].decodeStatusCounts[compatUnknown])
			_, decoded, _ := splitPack(t, file)
			stats := footerStats(t, decoded)
			require.Greater(t, len(stats.kindCounts), compatUnknown)
			assert.Equal(t, uint64(1), stats.kindCounts[compatUnknown])

			// A query on an unknown value is answered like any other, the F-3 counts pruning the blocks without it.
			assert.Equal(t, []uint64{1}, cr.runs[compatQueryUnknownKind].seqs())
			assert.Equal(t, []int{1, 2}, cr.pruned[compatQueryUnknownKind])
			assert.Equal(t, []uint64{4}, cr.runs[compatQueryUnknownDir].seqs())
			assert.Equal(t, []int{0, 2}, cr.pruned[compatQueryUnknownDir])

			// A record of an unknown kind has no HSMS header fields, so an S/F predicate never selects it.
			assert.Equal(t, FieldValidity(0), all.items[1].rec.FieldValidity)
			assert.NotContains(t, cr.runs[compatQuerySF].seqs(), uint64(1))
		})
	}
}

func TestReaderCompatUnknownPackMetaTags(t *testing.T) {
	t.Parallel()

	for _, cfg := range compatConfigs() {
		t.Run(cfg.name, func(t *testing.T) {
			t.Parallel()

			base := readCompat(t, compatPack(t, cfg, nil))
			got := readCompat(t, compatPack(t, cfg, func(m *PackMeta) { m.Unknown = compatMetaEntries() }))

			assert.Equal(t, compatMetaEntries(), got.r.Header().Meta.Unknown, "the unknown entries are skipped and preserved")
			requireSameRead(t, base, got, nil)
		})
	}
}

func TestReaderCompatFormatMinor(t *testing.T) {
	t.Parallel()

	for _, cfg := range compatConfigs() {
		t.Run(cfg.name, func(t *testing.T) {
			t.Parallel()

			file := compatPack(t, cfg, nil)
			base := readCompat(t, file)
			for _, minor := range []uint16{1, 0xFFFF} {
				got := readCompat(t, patchHeader(t, file, func(h *format.FileHeader) { h.FormatMinor = minor }))

				h := got.r.Header()
				assert.Equal(t, uint16(1), h.FormatMajor)
				assert.Equal(t, minor, h.FormatMinor, "a reader accepts any minor within its major (the tracepack format specification §14)")
				assert.Equal(t, base.r.Blocks(), got.r.Blocks())
				requireSameRead(t, base, got, nil)
			}
		})
	}
}

func TestReaderCompatWideRecordHeaders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		wide func(i int) bool
	}{
		{name: "every block 64", wide: func(int) bool { return true }},
		{name: "blocks of 56 and 64", wide: func(i int) bool { return i == 1 }},
	}
	for _, cfg := range compatConfigs() {
		for _, tt := range tests {
			t.Run(cfg.name+" "+tt.name, func(t *testing.T) {
				t.Parallel()

				file := compatPack(t, cfg, nil)
				base := readCompat(t, file)
				got := readCompat(t, widenFile(t, file, tt.wide))

				blocks := got.r.Blocks()
				for i, b := range blocks {
					want := uint16(format.RecordHeaderLen)
					if tt.wide(i) {
						want += wideExtraLen
					}
					assert.Equal(t, want, b.RecordHeaderLen, "block %d", i)
				}
				requireSameRead(t, base, got, wideExtraOf(blocks, tt.wide))
			})
		}
	}
}

func TestReaderCompatLongerF2Entries(t *testing.T) {
	t.Parallel()

	for _, cfg := range compatConfigs() {
		t.Run(cfg.name, func(t *testing.T) {
			t.Parallel()

			file := compatPack(t, cfg, nil)
			base := readCompat(t, file)
			edited := editFooter(t, file, lengthenEntries)
			got := readCompat(t, edited)

			require.NotNil(t, got.r.footer)
			require.Equal(t, uint32(format.F2EntryLen+8), footerPrologueOf(t, edited).F2EntryLen)
			assert.Equal(t, base.r.Blocks(), got.r.Blocks(), "the fields of each entry are read at their offsets, the rest skipped")
			requireSameRead(t, base, got, nil)
		})
	}
}

func TestReaderCompatUnknownFooterTags(t *testing.T) {
	t.Parallel()

	for _, cfg := range compatConfigs() {
		t.Run(cfg.name, func(t *testing.T) {
			t.Parallel()

			file := compatPack(t, cfg, nil)
			base := readCompat(t, file)
			edited := editFooter(t, file, addFooterTags)
			_, decoded, _ := splitPack(t, edited)
			parts := splitFooter(t, decoded)
			require.GreaterOrEqual(t, entryIndex(parts.f3[0], 0x8001, 0), 0, "the edited footer carries the private F-3 tag")
			require.GreaterOrEqual(t, entryIndex(parts.f5, 0x8001, 0), 0, "the edited footer carries the private F-5 tag")
			got := readCompat(t, edited)

			assert.Equal(t, base.r.Blocks(), got.r.Blocks())
			requireSameRead(t, base, got, nil)
		})
	}
}

// TestReaderCompatNewerMinorPack reads a pack carrying every change
// that the tracepack format specification §14 allows a minor version:
// format_minor 1, unknown pack metadata tags, unknown enum values, 64-byte record headers beside 56-byte ones,
// 88-byte F-2 entries and unknown F-3 and F-5 tags.
func TestReaderCompatNewerMinorPack(t *testing.T) {
	t.Parallel()

	wide := func(i int) bool { return i != 1 }
	for _, cfg := range compatConfigs() {
		t.Run(cfg.name, func(t *testing.T) {
			t.Parallel()

			base := readCompat(t, compatPack(t, cfg, nil))

			file := compatPack(t, cfg, func(m *PackMeta) { m.Unknown = compatMetaEntries() })
			file = widenFile(t, file, wide)
			file = editFooter(t, file, lengthenEntries, addFooterTags)
			file = patchHeader(t, file, func(h *format.FileHeader) { h.FormatMinor = 1 })
			got := readCompat(t, file)

			h := got.r.Header()
			assert.Equal(t, uint16(1), h.FormatMinor)
			assert.Equal(t, compatMetaEntries(), h.Meta.Unknown)
			require.NotNil(t, got.r.footer)
			require.Equal(t, uint32(format.F2EntryLen+8), footerPrologueOf(t, file).F2EntryLen)
			requireSameRead(t, base, got, wideExtraOf(got.r.Blocks(), wide))
		})
	}
}

// footerPrologueOf returns the F-1 prologue of the footer of the tracepack file.
func footerPrologueOf(t *testing.T, file []byte) format.FooterPrologue {
	t.Helper()

	_, decoded, _ := splitPack(t, file)
	pro, err := format.UnmarshalFooterPrologue(decoded)
	require.NoError(t, err)

	return pro
}
