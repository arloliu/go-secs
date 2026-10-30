package tracepack

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/format"
	"github.com/arloliu/go-secs/tracepack/internal/marking"
	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// redactionPadTag is the unknown top-level tag whose bytes value pads the metadata of the redaction test packs,
// so a surgery can rewrite the metadata in place, at its stored length.
const redactionPadTag uint16 = 0x7F00

// maskedNoteTextOffset is the offset of the text value in maskedNote's payload:
// the annotation_kind entry, then the text entry's header.
const maskedNoteTextOffset = 2*tlv.HeaderLen + 1

// redactionCase is one case of a redaction test table:
// the defects expected in the pack header and on each wholly masked record.
type redactionCase struct {
	name string
	// file is the pack read.
	file []byte
	// header lists the defects of PackHeader.RedactionDefects, in order, by the check that found each.
	header []error
	// masked maps the seq of every record expected wholly masked to the checks of its defects, in order;
	// every other record that carries quality.redacted or is named by an entry is expected not wholly masked.
	masked map[uint64][]error
}

// u64Array returns vs as a u64 array, little-endian, with no check of the elements.
func u64Array(vs ...uint64) []byte {
	var b []byte
	for _, v := range vs {
		b = binary.LittleEndian.AppendUint64(b, v)
	}

	return b
}

// padMeta adds to m the unknown entry rewriteMeta resizes.
func padMeta(m *PackMeta) *PackMeta {
	m.Unknown = append(m.Unknown, RawEntry{Tag: redactionPadTag, Type: uint8(tlv.TypeBytes), Value: make([]byte, 256)})

	return m
}

// rewriteMeta returns a copy of file, written over metadata padMeta padded,
// whose pack metadata is the stored one decoded under the file's minor and changed by fn,
// re-encoded at the stored length by resizing the pad entry.
// It lets a test store metadata the Writer refuses, with every offset of the pack unchanged.
func rewriteMeta(t *testing.T, file []byte, fn func(m *PackMeta)) []byte {
	t.Helper()

	hdr, err := format.UnmarshalFileHeader(file)
	require.NoError(t, err)
	raw := file[format.FileHeaderLen : format.FileHeaderLen+int(hdr.PackMetadataLen)]
	m, err := UnmarshalPackMetaMinor(raw, hdr.FormatMinor)
	require.NoError(t, err)
	fn(m)

	pad := slices.IndexFunc(m.Unknown, func(e RawEntry) bool { return e.Tag == redactionPadTag })
	require.GreaterOrEqual(t, pad, 0, "the pack was written over padded metadata")
	enc, err := m.MarshalBinary()
	require.NoError(t, err)
	grown := len(m.Unknown[pad].Value) + len(raw) - len(enc)
	require.GreaterOrEqual(t, grown, 0, "the pad entry is long enough")
	m.Unknown[pad].Value = make([]byte, grown)
	enc, err = m.MarshalBinary()
	require.NoError(t, err)
	require.Len(t, enc, len(raw))

	return patchMeta(t, file, func(meta []byte) { copy(meta, enc) })
}

// maskedNote returns an annotation record of seq carrying quality.redacted, whose text value "secret" is masked.
func maskedNote(seq uint64) Record {
	return Record{
		Seq: seq, TSUTCNs: blockTestHour + int64(seq), Epoch: 1, Kind: KindAnnotation, Dir: DirLocal,
		Fidelity: FidelityNotApplicable, DecodeStatus: DecodeStatusNotApplicable, Quality: QualityRedacted,
		Payload: jsonlTLV(tlv.U8Entry(0x0001, uint8(AnnotationKindNote)), tlv.UTF8Entry(0x0005, "secret")),
	}
}

// maskedNoteEntry returns the entry masking maskedNote's text value, with a 16-byte siv when siv is set.
func maskedNoteEntry(seq uint64, siv bool) RedactionEntry {
	e := RedactionEntry{Seq: seq, MaskedRanges: []MaskedRange{{Offset: maskedNoteTextOffset, Length: 6}}, Domain: "note", Digest: make([]byte, 32)}
	if siv {
		e.SIV, e.HasSIV = bytes.Repeat([]byte{0x5A}, 16), true
	}

	return e
}

// redactionExtract writes a format 1.0 extract under a policy without encryption, its metadata padded:
// the masked S7F3 records 0, 1 and 2, each with its entry, then the masked annotation 3 with its entry.
func redactionExtract(t *testing.T) []byte {
	t.Helper()

	m := padMeta(extractMinorMeta(0, 1, 2))
	m.Redaction = append(m.Redaction, maskedNoteEntry(3, false))

	return writeMinorPack(t, WriterOptions{Meta: m, Facts: PackFacts{AnyRedacted: true}},
		redactedRecord(0), redactedRecord(1), redactedRecord(2), maskedNote(3))
}

// redactionMasked writes a format 1.1 segment masked at write, its metadata padded:
// the masked S7F3 records 0, 1 and 2, each with its entry carrying a 16-byte siv,
// then the masked annotation 3 with its entry, without siv.
func redactionMasked(t *testing.T) []byte {
	t.Helper()

	m := padMeta(maskedMeta(true, 0, 1, 2))
	m.Redaction = append(m.Redaction, maskedNoteEntry(3, false))

	return writeMinorPack(t, WriterOptions{Meta: m, Facts: PackFacts{AnyRedacted: true}},
		redactedRecord(0), redactedRecord(1), redactedRecord(2), maskedNote(3))
}

// mergedRedaction is what MergeIterate yields for one record: the index of its representative's pack and its Marking.
type mergedRedaction struct {
	pack      int
	redaction *RecordRedaction
}

// readRedactions reads every record of r with Iterate and returns the Item.Redaction of each, by seq.
// Every redaction test pack is finalized and holds only recoverable redaction defects,
// which never make a read incomplete, so the read must be complete.
func readRedactions(t *testing.T, r *Reader, payloads bool) map[uint64]*RecordRedaction {
	t.Helper()

	got := map[uint64]*RecordRedaction{}
	res, err := r.Iterate(t.Context(), Query{Payloads: payloads}, func(it *Item) error {
		got[it.Record.Seq] = it.Redaction

		return nil
	})
	require.NoError(t, err)
	require.Empty(t, res.Incomplete, "a redaction defect never makes a read incomplete")
	require.True(t, res.Complete())

	return got
}

// mergeMarkings reads every record of readers with MergeIterate, in both orders,
// and returns the pack and Marking of each, by seq; both orders must agree.
// As for readRedactions, the read must be complete and, the copies of each record being equal, free of conflicts.
func mergeMarkings(t *testing.T, readers []*Reader, payloads bool) map[uint64]mergedRedaction {
	t.Helper()

	orders := []Order{OrderCapture, OrderTime}
	byOrder := make([]map[uint64]mergedRedaction, 0, len(orders))
	for _, order := range orders {
		got := map[uint64]mergedRedaction{}
		res, err := MergeIterate(t.Context(), readers, Query{Payloads: payloads}, MergeIterateOptions{Order: order}, func(it *Item) error {
			got[it.Record.Seq] = mergedRedaction{pack: it.Pack, redaction: it.Redaction}

			return nil
		})
		require.NoError(t, err)
		require.Empty(t, res.Incomplete, "%v: a redaction defect never makes a read incomplete", order)
		require.Empty(t, res.Conflicts, "%v", order)
		require.True(t, res.Complete())
		byOrder = append(byOrder, got)
	}
	require.Equal(t, byOrder[0], byOrder[1], "both orders give each record the same representative and Marking")

	return byOrder[0]
}

// mergeRedactions reads every record of readers as mergeMarkings does and returns the Item.Redaction of each, by seq.
func mergeRedactions(t *testing.T, readers []*Reader, payloads bool) map[uint64]*RecordRedaction {
	t.Helper()

	got := map[uint64]*RecordRedaction{}
	for seq, m := range mergeMarkings(t, readers, payloads) {
		got[seq] = m.redaction
	}

	return got
}

// requireDefects checks that got holds one defect per check of want, in order, each wrapping ErrRedactionDefect.
func requireDefects(t *testing.T, want []error, got []RedactionDefect) {
	t.Helper()

	require.Len(t, got, len(want), "%v", got)
	for i, err := range want {
		require.ErrorIs(t, got[i].Err, ErrRedactionDefect, "defect %d", i)
		require.ErrorIs(t, got[i].Err, err, "defect %d", i)
	}
}

// checkRedactionCase opens tt.file and checks its header defects and the wholly masked records,
// through Iterate and MergeIterate, with and without payloads, each read complete.
func checkRedactionCase(t *testing.T, tt *redactionCase) *Reader {
	t.Helper()

	r := mustOpen(t, tt.file, ReaderOptions{})
	require.True(t, r.Header().Finalized)
	requireDefects(t, tt.header, r.Header().RedactionDefects)
	for _, payloads := range []bool{true, false} {
		got := readRedactions(t, r, payloads)
		require.Equal(t, got, mergeRedactions(t, []*Reader{r}, payloads), "MergeIterate gives the Marking Iterate gives")
		for seq, rr := range got {
			want, masked := tt.masked[seq]
			if rr == nil {
				assert.False(t, masked, "seq %d is not wholly masked: it has no Marking", seq)

				continue
			}
			assert.Equal(t, masked, rr.WhollyMasked, "seq %d wholly masked", seq)
			requireDefects(t, want, rr.Defects)
		}
		for seq := range tt.masked {
			require.NotNil(t, got[seq], "seq %d has a Marking", seq)
		}
	}

	return r
}

// The bootstrap checks of the redaction entries report recoverable defects, never failing Open:
// each entry on its own, the order of the entries and the redaction-present flag;
// the stored values survive the read and the export,
// and the records an entry defect names are wholly masked, while a redaction-present mismatch masks nothing.
func TestRedactionBootstrapDefects(t *testing.T) {
	t.Parallel()

	extract, masked := redactionExtract(t), redactionMasked(t)
	setEntry := func(file []byte, i int, fn func(e *RedactionEntry)) []byte {
		return rewriteMeta(t, file, func(m *PackMeta) { fn(&m.Redaction[i]) })
	}
	setRaw := func(raw []byte) func(e *RedactionEntry) {
		return func(e *RedactionEntry) { e.MaskedRanges, e.RawMaskedRanges = nil, raw }
	}
	setRanges := func(rs ...MaskedRange) func(e *RedactionEntry) {
		return func(e *RedactionEntry) { e.MaskedRanges = rs }
	}
	setSIV := func(n int) func(e *RedactionEntry) {
		return func(e *RedactionEntry) { e.SIV, e.HasSIV = bytes.Repeat([]byte{0x5A}, n), true }
	}
	ranges := func(e error) map[uint64][]error { return map[uint64][]error{1: {e}} }

	tests := []redactionCase{
		{"empty array", setEntry(extract, 1, setRanges()), []error{marking.ErrRangesEmpty}, ranges(marking.ErrRangesEmpty)},
		{"odd array", setEntry(extract, 1, setRaw(u64Array(16))), []error{marking.ErrRangesMalformed}, ranges(marking.ErrRangesMalformed)},
		{"length not a multiple of 8", setEntry(extract, 1, setRaw(make([]byte, 7))),
			[]error{marking.ErrRangesMalformed}, ranges(marking.ErrRangesMalformed)},
		{"element above 2^63 - 1", setEntry(extract, 1, setRaw(u64Array(1<<63, 1))),
			[]error{marking.ErrRangesMalformed}, ranges(marking.ErrRangesMalformed)},
		{"zero length", setEntry(extract, 1, setRanges(MaskedRange{16, 0})), []error{marking.ErrRangeLength}, ranges(marking.ErrRangeLength)},
		{"unsorted", setEntry(extract, 1, setRanges(MaskedRange{18, 2}, MaskedRange{16, 1})),
			[]error{marking.ErrRangeOrder}, ranges(marking.ErrRangeOrder)},
		{"overlapping", setEntry(extract, 1, setRanges(MaskedRange{16, 3}, MaskedRange{17, 2})),
			[]error{marking.ErrRangeOrder}, ranges(marking.ErrRangeOrder)},
		{"adjacent", setEntry(extract, 1, setRanges(MaskedRange{16, 2}, MaskedRange{18, 2})),
			[]error{marking.ErrRangeOrder}, ranges(marking.ErrRangeOrder)},
		{
			"entries out of order",
			rewriteMeta(t, extract, func(m *PackMeta) { m.Redaction[0], m.Redaction[1] = m.Redaction[1], m.Redaction[0] }),
			[]error{marking.ErrEntryOrder, marking.ErrEntryOrder},
			map[uint64][]error{0: {marking.ErrEntryOrder}, 1: {marking.ErrEntryOrder}},
		},
		{
			"redaction-present clear with entries",
			patchHeader(t, extract, func(h *format.FileHeader) { h.Flags &^= fileHeaderRedactionPresent }),
			[]error{marking.ErrPresence}, nil,
		},
		{"siv of 0 bytes", setEntry(masked, 1, setSIV(0)), []error{marking.ErrSIVLength}, ranges(marking.ErrSIVLength)},
		{"siv of 15 bytes", setEntry(masked, 1, setSIV(15)), []error{marking.ErrSIVLength}, ranges(marking.ErrSIVLength)},
		{"siv of 17 bytes", setEntry(masked, 1, setSIV(17)), []error{marking.ErrSIVLength}, ranges(marking.ErrSIVLength)},
	}
	for i := range tests {
		tt := &tests[i]
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := checkRedactionCase(t, tt)

			hdr, err := format.UnmarshalFileHeader(tt.file)
			require.NoError(t, err)
			stored, err := UnmarshalPackMetaMinor(tt.file[format.FileHeaderLen:format.FileHeaderLen+int(hdr.PackMetadataLen)], hdr.FormatMinor)
			require.NoError(t, err)
			first, second := r.Header(), r.Header()
			require.Equal(t, stored.Redaction, first.Meta.Redaction, "the entries as stored")
			require.Equal(t, first, second)
			first.RedactionDefects[0] = RedactionDefect{}
			require.NotEqual(t, first.RedactionDefects, r.Header().RedactionDefects, "each Header holds its own defects")
			enc, err := r.Header().Meta.MarshalBinary()
			require.NoError(t, err)
			require.Equal(t, tt.file[format.FileHeaderLen:format.FileHeaderLen+int(hdr.PackMetadataLen)], enc, "the metadata re-encodes as stored")

			out, _, err := runExportJSONL(t, r)
			require.NoError(t, err)
			head := jsonlLines(t, out)[0]
			for _, e := range stored.Redaction {
				if len(e.RawMaskedRanges) > 0 {
					assert.Contains(t, head, base64.StdEncoding.EncodeToString(e.RawMaskedRanges), "the stored masked_ranges is exported")
				}
				if len(e.SIV) > 0 {
					assert.Contains(t, head, base64.StdEncoding.EncodeToString(e.SIV), "the stored siv is exported")
				}
			}
		})
	}

	t.Run("a present empty siv stays present", func(t *testing.T) {
		t.Parallel()

		r := mustOpen(t, setEntry(masked, 1, setSIV(0)), ReaderOptions{})
		for range 2 {
			e := r.Header().Meta.Redaction[1]
			assert.True(t, e.HasSIV)
			assert.Empty(t, e.SIV)
		}
	})
}

// The checks of the entries naming a record against it, run when the record is read,
// wholly mask exactly the record concerned, with its defect on Item.Redaction.
func TestRedactionRecordDefects(t *testing.T) {
	t.Parallel()

	extract, masked := redactionExtract(t), redactionMasked(t)
	setEntries := func(file []byte, fn func(m *PackMeta)) []byte { return rewriteMeta(t, file, fn) }
	one := func(seq uint64, err error) map[uint64][]error { return map[uint64][]error{seq: {err}} }
	secondEntry := func(e RedactionEntry) func(m *PackMeta) {
		return func(m *PackMeta) { m.Redaction = slices.Insert(m.Redaction, 2, e) }
	}
	sivEntry := func(off, n uint64) RedactionEntry {
		e := maskEntries(true, 1)[0]
		e.MaskedRanges = []MaskedRange{{off, n}}

		return e
	}

	tests := []redactionCase{
		{"range past payload_len", setEntries(extract, func(m *PackMeta) { m.Redaction[1].MaskedRanges = []MaskedRange{{16, 5}} }),
			nil, one(1, marking.ErrRangeOutside)},
		{"range inside the frame prefix", setEntries(extract, func(m *PackMeta) { m.Redaction[1].MaskedRanges = []MaskedRange{{10, 4}} }),
			nil, one(1, marking.ErrRangePrefix)},
		{"annotation range outside its text value",
			setEntries(extract, func(m *PackMeta) { m.Redaction[3].MaskedRanges = []MaskedRange{{maskedNoteTextOffset - 1, 6}} }),
			nil, one(3, marking.ErrRangeValue)},
		{"redacted record without entry", setEntries(extract, func(m *PackMeta) { m.Redaction = slices.Delete(m.Redaction, 1, 2) }),
			nil, one(1, marking.ErrRedactedBit)},
		{"data entry without siv under encryption", setEntries(masked, func(m *PackMeta) {
			m.Redaction[1].SIV, m.Redaction[1].HasSIV = nil, false
		}), nil, one(1, marking.ErrSIVMissing)},
		{"annotation entry with siv under encryption", setEntries(masked, func(m *PackMeta) { m.Redaction[3] = maskedNoteEntry(3, true) }),
			nil, one(3, marking.ErrSIVAnnotation)},
		{"two encrypted entries overlapping", setEntries(masked, secondEntry(sivEntry(17, 2))), nil, one(1, marking.ErrEncryptedOverlap)},
		{"an exact duplicate", setEntries(masked, secondEntry(maskEntries(true, 1)[0])), nil, one(1, marking.ErrEncryptedOverlap)},
	}
	for i := range tests {
		tt := &tests[i]
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			checkRedactionCase(t, tt)
		})
	}

	t.Run("seq of a record without redacted", func(t *testing.T) {
		t.Parallel()

		plain := redactedRecord(1)
		plain.Quality = 0
		file := writeMinorPack(t, WriterOptions{Meta: padMeta(extractMinorMeta(0)), Facts: PackFacts{AnyRedacted: true}},
			redactedRecord(0), plain)
		file = rewriteMeta(t, file, func(m *PackMeta) { m.Redaction = append(m.Redaction, maskEntries(false, 1)...) })
		checkRedactionCase(t, &redactionCase{file: file, masked: one(1, marking.ErrRedactedBit)})
	})
}

// Encrypted entries of one record whose ranges only touch are not a defect.
func TestRedactionAdjacentEncrypted(t *testing.T) {
	t.Parallel()

	file := rewriteMeta(t, redactionMasked(t), func(m *PackMeta) {
		m.Redaction[1].MaskedRanges = []MaskedRange{{16, 2}}
		e := maskEntries(true, 1)[0]
		e.MaskedRanges = []MaskedRange{{18, 2}}
		m.Redaction = slices.Insert(m.Redaction, 2, e)
	})
	checkRedactionCase(t, &redactionCase{file: file})

	rr := readRedactions(t, mustOpen(t, file, ReaderOptions{}), true)[1]
	require.NotNil(t, rr)
	assert.Len(t, rr.Entries, 2)
}

// A format 1.0 extract whose zero-mask entries of one record overlap reads as before: no defect.
func TestRedactionLegacyZeroOverlap(t *testing.T) {
	t.Parallel()

	file := rewriteMeta(t, redactionExtract(t), func(m *PackMeta) {
		e := maskEntries(false, 1)[0]
		e.MaskedRanges = []MaskedRange{{17, 2}}
		m.Redaction = slices.Insert(m.Redaction, 2, e)
	})
	r := checkRedactionCase(t, &redactionCase{file: file})
	assert.Zero(t, r.Header().FormatMinor)

	rr := readRedactions(t, r, true)[1]
	require.NotNil(t, rr)
	assert.Equal(t, r.Header().Meta.Redaction[1:3], rr.Entries)
}

// A defective entry of a later record isolates that record:
// an earlier masked record keeps its ranges, and nothing a read returned before changes.
func TestRedactionIsolation(t *testing.T) {
	t.Parallel()

	m := padMeta(maskedMeta(true, 0))
	m.Redaction = append(m.Redaction, maskedNoteEntry(9, false))
	var buf bytes.Buffer
	w, err := NewWriter(&buf, WriterOptions{Meta: m, Facts: PackFacts{AnyRedacted: true}})
	require.NoError(t, err)
	rec := redactedRecord(0)
	require.NoError(t, w.Append(&rec))
	require.NoError(t, w.Flush())
	note := maskedNote(9)
	require.NoError(t, w.Append(&note))
	_, err = w.Close()
	require.NoError(t, err)
	file := rewriteMeta(t, buf.Bytes(), func(m *PackMeta) { m.Redaction[1].MaskedRanges = []MaskedRange{{maskedNoteTextOffset, 7}} })

	r := mustOpen(t, file, ReaderOptions{})
	require.Len(t, r.Blocks(), 2)
	var early *RecordRedaction
	var snapshot RecordRedaction
	res, err := r.Iterate(t.Context(), Query{Payloads: true}, func(it *Item) error {
		switch it.Record.Seq {
		case 0:
			early = it.Redaction
			snapshot = *early
			snapshot.Entries = []RedactionEntry{cloneRedactionEntry(&early.Entries[0])}
		case 9:
			require.NotNil(t, it.Redaction)
			assert.True(t, it.Redaction.WhollyMasked)
			requireDefects(t, []error{marking.ErrRangeOutside}, it.Redaction.Defects)
		default:
			t.Errorf("unexpected seq %d", it.Record.Seq)
		}

		return nil
	})
	require.NoError(t, err)
	require.Empty(t, res.Incomplete)
	require.NotNil(t, early)
	assert.False(t, early.WhollyMasked)
	assert.Empty(t, early.Defects)
	assert.Equal(t, []MaskedRange{{16, 4}}, early.Entries[0].MaskedRanges)
	assert.Equal(t, snapshot, *early, "the earlier record's Marking is unchanged")
}

// MergeIterate gives each record the Marking of its representative's entries, as Iterate does.
func TestMergeIterateRedaction(t *testing.T) {
	t.Parallel()

	file := rewriteMeta(t, redactionMasked(t), func(m *PackMeta) { m.Redaction[1].MaskedRanges = []MaskedRange{{16, 5}} })
	r := mustOpen(t, file, ReaderOptions{})
	want := readRedactions(t, r, true)
	require.NotNil(t, want[1])
	require.True(t, want[1].WhollyMasked)
	require.NotNil(t, want[0])
	require.False(t, want[0].WhollyMasked)

	got := mergeRedactions(t, []*Reader{r}, true)
	assert.Equal(t, want, got)
}

// MergeIterate gives each record the Marking of its representative's pack,
// the first block holding it in cluster order (the tracepack semantics specification §7.4),
// never another pack's, although every copy of the record is equal byte for byte.
// Two packs of one capture hold the same masked records under entries a read tells apart:
// a digest in one, a defective range in the other.
func TestMergeIterateRedactionRepresentative(t *testing.T) {
	t.Parallel()

	capture := UUID{0x0C, 0xA9}
	write := func(id byte, seqs []uint64, edit func(m *PackMeta)) *Reader {
		m := padMeta(maskedMeta(true, seqs...))
		m.SeqStart = seqs[0]
		recs := make([]Record, 0, len(seqs))
		for _, seq := range seqs {
			recs = append(recs, redactedRecord(seq))
		}
		file := writeMinorPack(t, WriterOptions{Meta: m, Facts: PackFacts{AnyRedacted: true}, PackID: UUID{id}, CaptureID: capture}, recs...)
		if edit != nil {
			file = rewriteMeta(t, file, edit)
		}

		return mustOpen(t, file, ReaderOptions{})
	}
	// other differs from plain in the digest of its entry for seq 1 and in the range of its entry for seq 2.
	other := func(m *PackMeta) {
		for i := range m.Redaction {
			switch e := &m.Redaction[i]; e.Seq {
			case 1:
				e.Digest = bytes.Repeat([]byte{0x11}, 32)
			case 2:
				e.MaskedRanges = []MaskedRange{{16, 5}}
			default:
			}
		}
	}
	plain := write(1, []uint64{0, 1, 2}, nil)
	edited := write(2, []uint64{0, 1, 2}, other)
	late := write(3, []uint64{1, 2}, other)

	plainWant, editedWant, lateWant := readRedactions(t, plain, true), readRedactions(t, edited, true), readRedactions(t, late, true)
	require.NotEqual(t, plainWant[1], editedWant[1], "the packs' Markings of seq 1 differ")
	require.False(t, plainWant[2].WhollyMasked)
	require.True(t, editedWant[2].WhollyMasked)

	tests := []struct {
		name    string
		readers []*Reader
		// pack is the index of the representative's pack for every seq, and want its Marking by seq.
		pack map[uint64]int
		want map[uint64]*RecordRedaction
	}{
		{"plain first", []*Reader{plain, edited}, map[uint64]int{0: 0, 1: 0, 2: 0}, plainWant},
		{"edited first", []*Reader{edited, plain}, map[uint64]int{0: 0, 1: 0, 2: 0}, editedWant},
		// The block of plain starts at seq 0, before the block of late, so it represents seqs 1 and 2 from either position.
		{"the earlier block given second", []*Reader{late, plain}, map[uint64]int{0: 1, 1: 1, 2: 1}, plainWant},
		{"the earlier block given first", []*Reader{plain, late}, map[uint64]int{0: 0, 1: 0, 2: 0}, plainWant},
		{"a later block alone", []*Reader{late}, map[uint64]int{1: 0, 2: 0}, lateWant},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			for _, payloads := range []bool{true, false} {
				got := mergeMarkings(t, tt.readers, payloads)
				require.Len(t, got, len(tt.pack), "payloads %t", payloads)
				for seq, pack := range tt.pack {
					assert.Equal(t, pack, got[seq].pack, "seq %d, payloads %t", seq, payloads)
					assert.Equal(t, tt.want[seq], got[seq].redaction, "seq %d, payloads %t", seq, payloads)
				}
			}
		})
	}
}

// The export keeps a present empty siv present and leaves an absent siv absent
// (the tracepack JSONL specification §4): its entry under schema tracepack-jsonl/2 has the key "siv" with the value "",
// while an annotation's entry, without siv, has no such key.
func TestRedactionExportEmptySIV(t *testing.T) {
	t.Parallel()

	file := rewriteMeta(t, redactionMasked(t), func(m *PackMeta) { m.Redaction[1].SIV = []byte{} })
	r := mustOpen(t, file, ReaderOptions{})
	require.True(t, r.Header().Meta.Redaction[1].HasSIV)
	require.False(t, r.Header().Meta.Redaction[3].HasSIV)

	out, _, err := runExportJSONL(t, r)
	require.NoError(t, err)
	var head map[string]json.RawMessage
	line := jsonlLines(t, out)[0]
	require.NoError(t, json.Unmarshal([]byte(line), &head))
	assert.JSONEq(t, `"tracepack-jsonl/2"`, string(head["schema"]))
	var meta map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(head["metadata"], &meta))
	var entries []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(meta["redaction"], &entries))
	require.Len(t, entries, 4)

	siv, ok := entries[1]["siv"]
	require.True(t, ok, "a present empty siv is exported: %s", line)
	assert.JSONEq(t, `""`, string(siv))
	assert.JSONEq(t, `"`+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x5A}, 16))+`"`, string(entries[0]["siv"]))
	_, ok = entries[3]["siv"]
	assert.False(t, ok, "an absent siv is not exported: %s", line)
}

// A read without payloads gives every record the Marking a read with payloads gives it.
func TestRedactionWithoutPayloads(t *testing.T) {
	t.Parallel()

	file := rewriteMeta(t, redactionMasked(t), func(m *PackMeta) {
		m.Redaction[1].MaskedRanges = []MaskedRange{{16, 5}}
		m.Redaction[3].MaskedRanges = []MaskedRange{{maskedNoteTextOffset - 1, 6}}
	})
	r := mustOpen(t, file, ReaderOptions{})
	with := readRedactions(t, r, true)
	require.True(t, with[1].WhollyMasked)
	require.True(t, with[3].WhollyMasked)
	require.False(t, with[2].WhollyMasked)

	assert.Equal(t, with, readRedactions(t, r, false), "Iterate")
	assert.Equal(t, with, mergeRedactions(t, []*Reader{r}, false), "MergeIterate")
}

// A pack masked at write whose policy holds one encryption tag, or neither, opens with no defect for the policy,
// and its metadata reads back as stored (the tracepack format specification §5);
// the siv presence checks apply exactly when the policy carries encryption_algorithm,
// while an ill-sized siv is a defect whatever the policy holds.
func TestRedactionEncryptionTags(t *testing.T) {
	t.Parallel()

	masked := redactionMasked(t)
	tests := []struct {
		name   string
		policy func(p *RedactionPolicy)
		// missing lists the defects of a data entry without siv.
		missing []error
	}{
		{"neither tag", func(p *RedactionPolicy) { p.EncryptionAlgorithm, p.EncryptionKeyID = nil, nil }, nil},
		{"algorithm only", func(p *RedactionPolicy) { p.EncryptionKeyID = nil }, []error{marking.ErrSIVMissing}},
		{"key id only", func(p *RedactionPolicy) { p.EncryptionAlgorithm = nil }, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			file := rewriteMeta(t, masked, func(m *PackMeta) { tt.policy(m.RedactionPolicy) })
			r := checkRedactionCase(t, &redactionCase{file: file})
			want := encryptedPolicy()
			tt.policy(want)
			assert.Equal(t, want, r.Header().Meta.RedactionPolicy)
			enc, err := r.Header().Meta.MarshalBinary()
			require.NoError(t, err)
			hdr, err := format.UnmarshalFileHeader(file)
			require.NoError(t, err)
			assert.Equal(t, file[format.FileHeaderLen:format.FileHeaderLen+int(hdr.PackMetadataLen)], enc)

			var masks map[uint64][]error
			if tt.missing != nil {
				masks = map[uint64][]error{1: tt.missing}
			}
			checkRedactionCase(t, &redactionCase{
				file:   rewriteMeta(t, file, func(m *PackMeta) { m.Redaction[1].SIV, m.Redaction[1].HasSIV = nil, false }),
				masked: masks,
			})
			checkRedactionCase(t, &redactionCase{
				file:   rewriteMeta(t, file, func(m *PackMeta) { m.Redaction[1].SIV = m.Redaction[1].SIV[:15] }),
				header: []error{marking.ErrSIVLength},
				masked: map[uint64][]error{1: {marking.ErrSIVLength}},
			})
		})
	}

	t.Run("an extract whose policy carries the encryption tags", func(t *testing.T) {
		t.Parallel()

		file := patchHeader(t, redactionExtract(t), func(h *format.FileHeader) { h.FormatMinor = 1 })
		file = rewriteMeta(t, file, func(m *PackMeta) {
			m.RedactionPolicy = encryptedPolicy()
			for i := range m.Redaction[:3] {
				m.Redaction[i].SIV, m.Redaction[i].HasSIV = bytes.Repeat([]byte{0x5A}, 16), true
			}
		})
		r := checkRedactionCase(t, &redactionCase{file: file})
		assert.Equal(t, encryptedPolicy(), r.Header().Meta.RedactionPolicy)
	})
}

// A record of a kind the tracepack semantics specification §8 never masks
// (a control record, a transport event, a record of unknown kind)
// that carries quality.redacted or that an entry names is a Marking defect:
// it is wholly masked and reported, even when its ranges would fit a data record's.
func TestRedactionUnmaskableKinds(t *testing.T) {
	t.Parallel()

	control := controlRecord(1, blockTestHour+1, 1, DirHostToEquipment)
	control.Quality = QualityRedacted
	event := Record{
		Seq: 2, TSUTCNs: blockTestHour + 2, Epoch: 1, Kind: KindTransportEvent, Dir: DirLocal,
		Fidelity: FidelityNotApplicable, DecodeStatus: DecodeStatusNotApplicable, Quality: QualityRedacted,
		Payload: mustMarshalEvent(t, &TransportEvent{Event: EventClockStep, ClockStepNs: new(int64(5)), Detail: new("a detail to mask")}),
	}
	require.Greater(t, len(event.Payload), 20)
	unknown := Record{Seq: 3, TSUTCNs: blockTestHour + 3, Epoch: 1, Kind: KindUnknown, Dir: DirLocal, Payload: make([]byte, 30)}
	entry := func(seq uint64) RedactionEntry {
		return RedactionEntry{Seq: seq, MaskedRanges: []MaskedRange{{16, 4}}, Domain: "d", Digest: make([]byte, 32)}
	}

	m := padMeta(extractMinorMeta(0))
	m.Redaction = append(m.Redaction, entry(2))
	file := writeMinorPack(t, WriterOptions{Meta: m, Facts: PackFacts{AnyRedacted: true}},
		redactedRecord(0), control, event, unknown)
	// The writer is not asked to name the record of unknown kind: the entry is added by surgery.
	file = rewriteMeta(t, file, func(m *PackMeta) { m.Redaction = append(m.Redaction, entry(3)) })

	checkRedactionCase(t, &redactionCase{file: file, masked: map[uint64][]error{
		1: {marking.ErrRedactedBit, marking.ErrKindNotMaskable},
		2: {marking.ErrKindNotMaskable},
		3: {marking.ErrRedactedBit, marking.ErrKindNotMaskable},
	}})
}
