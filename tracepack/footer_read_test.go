package tracepack

import (
	"bytes"
	"math"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/codec"
	"github.com/arloliu/go-secs/tracepack/internal/format"
	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// footerTestPack is a finalized pack written by NewWriter, with the parts parseFooter takes.
type footerTestPack struct {
	file []byte
	// blocks are the Writer's own block summaries, in file order.
	blocks      []blockSummary
	tr          format.Trailer
	decoded     []byte
	blocksStart uint64
}

// footerTestStep is one step of writing a footer test pack: append rec, then flush when flush is set.
type footerTestStep struct {
	rec   Record
	flush bool
}

// richFooterSteps returns the records of the rich footer test pack, with caller seqs from 10.
// With the 250-byte block threshold and one Flush they form five blocks over two UTC hours:
//
//	block 0, hour 0: 10 start boundary e0, 11 data e1, 13 data e1 (seq gap), then Flush
//	block 1, hour 0: 14 data e1, 15 socket-close e1, 20 gap boundary e0 with gap_start and gap_end (seq gap)
//	block 2, hour 0: 21 data e2
//	block 3, hour 1: 22 data e2, 23 data e2, 30 data e3 (seq gap)
//	block 4, hour 1: 31 stop boundary e3
func richFooterSteps(t testing.TB) []footerTestStep {
	t.Helper()

	gapStart, gapEnd := blockTestHour+100, blockTestHour+200
	h1 := blockTestHour + hourNs

	return []footerTestStep{
		{rec: testEventRecord(t, 10, blockTestHour+50, 0, &TransportEvent{
			Event: EventCaptureBoundary, BoundaryKind: new(BoundaryKindStart),
		})},
		{rec: testDataRecord(11, blockTestHour+10, 1)},
		{rec: testDataRecord(13, blockTestHour+20, 1), flush: true},
		{rec: testDataRecord(14, blockTestHour+30, 1)},
		{rec: testEventRecord(t, 15, blockTestHour+40, 1, &TransportEvent{Event: EventSocketClose})},
		{rec: testEventRecord(t, 20, blockTestHour+300, 0, &TransportEvent{
			Event: EventCaptureBoundary, BoundaryKind: new(BoundaryKindGap), GapStart: &gapStart, GapEnd: &gapEnd,
		})},
		{rec: testDataRecord(21, blockTestHour+400, 2)},
		{rec: testDataRecord(22, h1+1, 2)},
		{rec: testDataRecord(23, h1+2, 2)},
		{rec: testDataRecord(30, h1+3, 3)},
		{rec: testEventRecord(t, 31, h1+4, 3, &TransportEvent{
			Event: EventCaptureBoundary, BoundaryKind: new(BoundaryKindStop),
		})},
	}
}

// writeFooterTestPack writes steps with codec c into a new pack, closes it,
// and returns the pack with its trailer, decoded footer and blocks start.
func writeFooterTestPack(t testing.TB, c Codec, steps []footerTestStep) *footerTestPack {
	t.Helper()

	meta := &PackMeta{
		ToolID: "tool", Recorder: "rec", Writer: "wr", Classifier: new("c"),
		PackRole: PackRoleSegment, ScopeGeneration: new(uint64(0)),
	}
	if len(steps) > 0 {
		meta.SeqStart = steps[0].rec.Seq
	}

	var buf bytes.Buffer
	w, err := NewWriter(&buf, WriterOptions{Meta: meta, Codec: c, BlockThreshold: 250})
	require.NoError(t, err)
	for i := range steps {
		require.NoError(t, w.Append(&steps[i].rec), "step %d", i)
		if steps[i].flush {
			require.NoError(t, w.Flush())
		}
	}
	_, err = w.Close()
	require.NoError(t, err)

	p := &footerTestPack{file: buf.Bytes(), blocks: w.blocks}
	p.tr, p.decoded, p.blocksStart = splitPack(t, p.file)

	return p
}

// splitPack returns the trailer, the decoded footer and the blocks start of a finalized pack.
func splitPack(t testing.TB, file []byte) (format.Trailer, []byte, uint64) {
	t.Helper()

	hdr, err := format.UnmarshalFileHeader(file)
	require.NoError(t, err)
	tr, err := format.UnmarshalTrailer(file[len(file)-format.TrailerLen:])
	require.NoError(t, err)

	onDisk := file[tr.FooterOffset : len(file)-format.TrailerLen]
	decoded, err := codec.Decode(tr.FooterCodec, nil, onDisk, int(tr.FooterUncompressedLen))
	require.NoError(t, err)

	return tr, decoded, uint64(format.FileHeaderLen) + uint64(hdr.PackMetadataLen)
}

// richFooterPack writes the rich footer test pack with codec c.
func richFooterPack(t testing.TB, c Codec) *footerTestPack {
	t.Helper()

	return writeFooterTestPack(t, c, richFooterSteps(t))
}

// emptyFooterPack writes a pack without records.
func emptyFooterPack(t testing.TB) *footerTestPack {
	t.Helper()

	return writeFooterTestPack(t, CodecZstd, nil)
}

// sortedEpochBlocks returns a copy of blocks with every block's epochs in ascending epoch order,
// the order a block summary lists them in.
func sortedEpochBlocks(blocks []blockSummary) []blockSummary {
	out := slices.Clone(blocks)
	for i := range out {
		out[i].epochs = slices.SortedFunc(slices.Values(out[i].epochs), compareEpochs)
	}

	return out
}

// footerParts is a decoded footer split into the structures a test mutates one at a time.
// encode rebuilds the footer with every section offset and summary location recomputed,
// so changing one value never moves another section out of place.
type footerParts struct {
	pro     format.FooterPrologue
	entries []format.F2Entry
	// f3 holds each block's F-3 entry list, parallel to entries.
	f3 [][]tlv.Entry
	f5 []tlv.Entry
	// entryPad is appended to every F-2 entry, so f2_entry_len is 80 + len(entryPad).
	entryPad []byte
}

// splitFooter splits a decoded footer written by NewWriter into its parts.
// The parts copy decoded, so a test may mutate them freely.
func splitFooter(t testing.TB, decoded []byte) *footerParts {
	t.Helper()

	b := bytes.Clone(decoded)
	pro, err := format.UnmarshalFooterPrologue(b)
	require.NoError(t, err)

	p := &footerParts{pro: pro}
	for i := range int(pro.BlockCount) {
		e, err := format.UnmarshalF2Entry(b[int(pro.F2Offset)+i*int(pro.F2EntryLen):], int(pro.F2EntryLen))
		require.NoError(t, err)
		p.entries = append(p.entries, e)

		start := pro.F3Offset + e.SummaryOffset
		list, err := tlv.Decode(b[start : start+uint64(e.SummaryLen)])
		require.NoError(t, err)
		p.f3 = append(p.f3, list)
	}

	p.f5, err = tlv.Decode(b[pro.F5Offset : pro.F5Offset+pro.F5Len])
	require.NoError(t, err)

	return p
}

// encode returns the decoded footer of p:
// F-1, F-2 at offset 72, F-3 right after F-2, an empty F-4 at the end of F-3, and F-5 right after.
// Every prologue field other than the section offsets and lengths and f2_entry_len is written as p holds it,
// and so is every F-2 field other than summary_offset and summary_len.
func (p *footerParts) encode() []byte {
	entryLen := format.F2EntryLen + len(p.entryPad)
	entries := slices.Clone(p.entries)

	var f3 []byte
	for i := range entries {
		start := len(f3)
		for _, e := range p.f3[i] {
			f3 = tlv.AppendEntry(f3, e)
		}
		entries[i].SummaryOffset = uint64(start)
		entries[i].SummaryLen = uint32(len(f3) - start)
	}

	var f5 []byte
	for _, e := range p.f5 {
		f5 = tlv.AppendEntry(f5, e)
	}

	pro := p.pro
	pro.F2EntryLen = uint32(entryLen)
	pro.F2Offset = format.FooterPrologueLen
	pro.F3Offset = uint64(format.FooterPrologueLen + len(entries)*entryLen)
	pro.F3Len = uint64(len(f3))
	pro.F4Offset = pro.F3Offset + pro.F3Len
	pro.F4Len = 0
	pro.F5Offset = pro.F4Offset
	pro.F5Len = uint64(len(f5))

	out := format.AppendFooterPrologue(nil, &pro)
	for i := range entries {
		out = format.AppendF2Entry(out, &entries[i])
		out = append(out, p.entryPad...)
	}
	out = append(out, f3...)

	return append(out, f5...)
}

// patchPrologue returns a copy of the decoded footer b whose F-1 prologue is re-encoded after fn changes it.
func patchPrologue(t testing.TB, b []byte, fn func(*format.FooterPrologue)) []byte {
	t.Helper()

	pro, err := format.UnmarshalFooterPrologue(b)
	require.NoError(t, err)
	fn(&pro)

	out := bytes.Clone(b)
	copy(out, format.AppendFooterPrologue(nil, &pro))

	return out
}

// patchEntry returns a copy of the decoded footer b whose F-2 entry i is re-encoded after fn changes it;
// b's entries must be 80 bytes long.
func patchEntry(t testing.TB, b []byte, i int, fn func(*format.F2Entry)) []byte {
	t.Helper()

	off := format.FooterPrologueLen + i*format.F2EntryLen
	e, err := format.UnmarshalF2Entry(b[off:], format.F2EntryLen)
	require.NoError(t, err)
	fn(&e)

	out := bytes.Clone(b)
	copy(out[off:], format.AppendF2Entry(nil, &e))

	return out
}

// entryIndex returns the index of the n-th entry of list with tag, or -1.
func entryIndex(list []tlv.Entry, tag uint16, n int) int {
	for i, e := range list {
		if e.Tag != tag {
			continue
		}
		if n == 0 {
			return i
		}
		n--
	}

	return -1
}

// replaceEntry returns a copy of list with its n-th entry of tag replaced by e.
func replaceEntry(t testing.TB, list []tlv.Entry, tag uint16, n int, e tlv.Entry) []tlv.Entry {
	t.Helper()

	i := entryIndex(list, tag, n)
	require.GreaterOrEqual(t, i, 0, "entry %d of tag 0x%04X", n, tag)
	out := slices.Clone(list)
	out[i] = e

	return out
}

// removeEntries returns a copy of list without its entries of tag.
func removeEntries(list []tlv.Entry, tag uint16) []tlv.Entry {
	return slices.DeleteFunc(slices.Clone(list), func(e tlv.Entry) bool { return e.Tag == tag })
}

// withNested returns the nested entry e with its nested entry of tag replaced by v, or added when absent;
// a zero-tagged v removes the nested entry instead.
func withNested(t testing.TB, e tlv.Entry, tag uint16, v tlv.Entry) tlv.Entry {
	t.Helper()

	nested, err := e.Nested()
	require.NoError(t, err)
	nested = slices.DeleteFunc(slices.Clone(nested), func(n tlv.Entry) bool { return n.Tag == tag })
	if v.Tag != 0 {
		nested = append(nested, v)
	}

	return tlv.NestedEntry(e.Tag, nested)
}

// nthEntry returns the n-th entry of list with tag.
func nthEntry(t testing.TB, list []tlv.Entry, tag uint16, n int) tlv.Entry {
	t.Helper()

	i := entryIndex(list, tag, n)
	require.GreaterOrEqual(t, i, 0, "entry %d of tag 0x%04X", n, tag)

	return list[i]
}

// footerStats returns the F-5 statistics of the decoded footer as read,
// for a footer parseFooter accepted.
func footerStats(t testing.TB, decoded []byte) *packStats {
	t.Helper()

	pro, err := format.UnmarshalFooterPrologue(decoded)
	require.NoError(t, err)
	sec, err := locateSections(decoded, &pro)
	require.NoError(t, err)
	st, err := parsePackStats(sec.f5, pro.BlockCount > 0)
	require.NoError(t, err)

	return st
}

// normalizeStats returns a copy of st in a canonical form for comparison:
// count arrays without trailing zeros, epochs in ascending epoch order, boundaries sorted by every field,
// and every empty slice as nil.
func normalizeStats(st *packStats) packStats {
	out := *st
	out.kindCounts = trimZeros(st.kindCounts)
	out.dirCounts = trimZeros(st.dirCounts)
	out.decodeStatusCounts = trimZeros(st.decodeStatusCounts)
	out.seqRanges = slices.Clone(st.seqRanges)
	out.epochs = slices.SortedFunc(slices.Values(st.epochs), compareEpochs)
	out.boundaries = slices.SortedFunc(slices.Values(st.boundaries), compareBoundaries)

	if len(out.kindCounts) == 0 {
		out.kindCounts = nil
	}
	if len(out.dirCounts) == 0 {
		out.dirCounts = nil
	}
	if len(out.decodeStatusCounts) == 0 {
		out.decodeStatusCounts = nil
	}
	if len(out.seqRanges) == 0 {
		out.seqRanges = nil
	}
	if len(out.epochs) == 0 {
		out.epochs = nil
	}
	if len(out.boundaries) == 0 {
		out.boundaries = nil
	}

	return out
}

func TestParseFooterAcceptsWriterPacks(t *testing.T) {
	t.Parallel()

	for _, c := range []Codec{CodecNone, CodecZstd} {
		t.Run(c.String(), func(t *testing.T) {
			t.Parallel()

			p := richFooterPack(t, c)
			require.Len(t, p.blocks, 5)

			idx, err := parseFooter(p.decoded, &p.tr, p.blocksStart)
			require.NoError(t, err)

			pro, err := format.UnmarshalFooterPrologue(p.decoded)
			require.NoError(t, err)
			assert.Equal(t, format.FooterLayoutVersion, pro.FooterLayoutVersion)
			assert.Equal(t, uint32(len(p.blocks)), pro.BlockCount)
			require.Len(t, idx.blocks, len(p.blocks))
			for i := range p.blocks {
				assert.Equal(t, f2EntryOf(&p.blocks[i], 0, 0), f2EntryOf(&idx.blocks[i], 0, 0), "F-2 entry %d", i)
			}
			assert.Equal(t, sortedEpochBlocks(p.blocks), idx.blocks)
			assert.Equal(t, aggregate(p.blocks), footerStats(t, p.decoded))
		})
	}
}

func TestParseFooterRichPackCoversEveryStructure(t *testing.T) {
	t.Parallel()

	p := richFooterPack(t, CodecZstd)
	idx, err := parseFooter(p.decoded, &p.tr, p.blocksStart)
	require.NoError(t, err)

	// Block 0 holds seqs 10, 11 and 13, so its summary carries seq_range entries.
	b0 := idx.blocks[0]
	assert.Equal(t, []seqRange{{10, 11}, {13, 13}}, b0.seqRanges)
	assert.Equal(t, uint32(3), b0.recordCount)

	// Block 1 lists epoch 1 before epoch 0 by first appearance; F-3 lists them in ascending order.
	b1 := idx.blocks[1]
	require.Len(t, b1.epochs, 2)
	assert.Equal(t, []uint32{0, 1}, []uint32{b1.epochs[0].epoch, b1.epochs[1].epoch})
	assert.True(t, b1.epochs[1].hasCloseSeq, "the socket close ends epoch 1")
	assert.Equal(t, uint64(15), b1.epochs[1].closeSeq)
	require.Len(t, b1.boundaries, 1)
	require.NotNil(t, b1.boundaries[0].gapStart)
	require.NotNil(t, b1.boundaries[0].gapEnd)
	assert.Equal(t, BoundaryKindGap, b1.boundaries[0].kind)

	assert.NotEqual(t, hourOf(idx.blocks[0].tsMin), hourOf(idx.blocks[4].tsMin), "the pack spans two hours")
	stats := footerStats(t, p.decoded)
	assert.Equal(t, []seqRange{{10, 11}, {13, 15}, {20, 23}, {30, 31}}, stats.seqRanges)
	assert.Len(t, stats.boundaries, 3)
}

func TestParseFooterAcceptsEmptyPack(t *testing.T) {
	t.Parallel()

	p := emptyFooterPack(t)
	require.Equal(t, p.blocksStart, p.tr.FooterOffset)

	idx, err := parseFooter(p.decoded, &p.tr, p.blocksStart)
	require.NoError(t, err)
	assert.Empty(t, idx.blocks)
	assert.Equal(t, packStats{}, normalizeStats(footerStats(t, p.decoded)))
}

func TestParseFooterAcceptsVariants(t *testing.T) {
	t.Parallel()

	base := richFooterPack(t, CodecNone)
	unknown := tlv.BytesEntry(0x0100, []byte{1, 2, 3})
	reserved := tlv.U64Entry(0x0010, 7)

	tests := []struct {
		name   string
		mutate func(t *testing.T, p *footerParts)
	}{
		{
			name: "f2_entry_len 88 with 8 extra bytes per entry",
			mutate: func(_ *testing.T, p *footerParts) {
				p.entryPad = []byte{0xA1, 0xA2, 0xA3, 0xA4, 0xA5, 0xA6, 0xA7, 0xA8}
			},
		},
		{
			name: "unknown and reserved F-3 tags",
			mutate: func(_ *testing.T, p *footerParts) {
				for i := range p.f3 {
					p.f3[i] = append(slices.Clone(p.f3[i]), unknown, reserved, unknown)
				}
			},
		},
		{
			name: "unknown F-5 tags",
			mutate: func(_ *testing.T, p *footerParts) {
				p.f5 = append([]tlv.Entry{unknown, tlv.I64Entry(0x0042, -1)}, p.f5...)
			},
		},
		{
			name: "F-5 count arrays with extra trailing zeros",
			mutate: func(t *testing.T, p *footerParts) {
				for _, tag := range []uint16{f5TagKindCounts, f5TagDirCounts, f5TagDecodeStatusCounts} {
					e := nthEntry(t, p.f5, tag, 0)
					padded := append(slices.Clone(e.Value), make([]byte, 3*8)...)
					p.f5 = replaceEntry(t, p.f5, tag, 0, tlv.BytesEntry(tag, padded))
				}
			},
		},
		{
			name: "F-3 count arrays with extra trailing zeros",
			mutate: func(t *testing.T, p *footerParts) {
				e := nthEntry(t, p.f3[0], f3TagKindCounts, 0)
				padded := append(slices.Clone(e.Value), make([]byte, 5*4)...)
				p.f3[0] = replaceEntry(t, p.f3[0], f3TagKindCounts, 0, tlv.BytesEntry(f3TagKindCounts, padded))
			},
		},
		{
			name: "F-5 epochs and boundaries in reverse order",
			mutate: func(_ *testing.T, p *footerParts) {
				var epochs, boundaries, rest []tlv.Entry
				for _, e := range p.f5 {
					switch e.Tag {
					case f5TagEpoch:
						epochs = append(epochs, e)
					case f5TagBoundary:
						boundaries = append(boundaries, e)
					default:
						rest = append(rest, e)
					}
				}
				slices.Reverse(epochs)
				slices.Reverse(boundaries)
				p.f5 = slices.Concat(rest, boundaries, epochs)
			},
		},
		{
			name: "unknown nested epoch and boundary tags",
			mutate: func(t *testing.T, p *footerParts) {
				p.f3[0] = replaceEntry(t, p.f3[0], f3TagEpoch, 0,
					withNested(t, nthEntry(t, p.f3[0], f3TagEpoch, 0), 0x0040, unknown))
				p.f5 = replaceEntry(t, p.f5, f5TagBoundary, 0,
					withNested(t, nthEntry(t, p.f5, f5TagBoundary, 0), 0x0040, unknown))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p := splitFooter(t, base.decoded)
			tt.mutate(t, p)
			decoded := p.encode()
			_, err := parseFooter(decoded, &base.tr, base.blocksStart)
			require.NoError(t, err)
			assert.Equal(t, normalizeStats(aggregate(base.blocks)), normalizeStats(footerStats(t, decoded)))
		})
	}
}

func TestFooterPartsRoundTrip(t *testing.T) {
	t.Parallel()

	p := richFooterPack(t, CodecZstd)
	assert.Equal(t, p.decoded, splitFooter(t, p.decoded).encode(), "the test helper rebuilds the writer's footer byte for byte")
}

// footerRejectCase is one footer mutation parseFooter must reject.
type footerRejectCase struct {
	name string
	// parts, raw and trailer mutate, in that order, the split footer, the re-encoded footer and the trailer;
	// each may be nil.
	parts   func(t *testing.T, p *footerParts)
	raw     func(t *testing.T, b []byte) []byte
	trailer func(tr *format.Trailer)
	// startShift is added to the blocks start passed to parseFooter.
	startShift uint64
	// want is a substring of the error that names the failed check.
	want string
}

// runFooterRejectCases checks that parseFooter accepts the unmutated footer of base
// and rejects every case with an error wrapping ErrInvalidFooter that contains the case's want.
func runFooterRejectCases(t *testing.T, base *footerTestPack, tests []footerRejectCase) {
	t.Helper()

	_, err := parseFooter(splitFooter(t, base.decoded).encode(), &base.tr, base.blocksStart)
	require.NoError(t, err, "the unmutated footer is valid")

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p := splitFooter(t, base.decoded)
			if tt.parts != nil {
				tt.parts(t, p)
			}
			b := p.encode()
			if tt.raw != nil {
				b = tt.raw(t, b)
			}
			tr := base.tr
			if tt.trailer != nil {
				tt.trailer(&tr)
			}

			idx, err := parseFooter(b, &tr, base.blocksStart+tt.startShift)
			require.ErrorIs(t, err, ErrInvalidFooter)
			assert.ErrorContains(t, err, tt.want)
			assert.Nil(t, idx)
		})
	}
}

// withNestedAt returns a copy of list whose n-th entry of listTag has its nested entry of tag set to v, as withNested does.
func withNestedAt(t testing.TB, list []tlv.Entry, listTag uint16, n int, tag uint16, v tlv.Entry) []tlv.Entry {
	t.Helper()

	return replaceEntry(t, list, listTag, n, withNested(t, nthEntry(t, list, listTag, n), tag, v))
}

// withU32Counts returns a copy of list whose u32 count array of tag is replaced by counts.
func withU32Counts(t testing.TB, list []tlv.Entry, tag uint16, counts []uint32) []tlv.Entry {
	t.Helper()

	return replaceEntry(t, list, tag, 0, tlv.BytesEntry(tag, tlv.AppendU32Array(nil, counts)))
}

// u32Counts decodes the u32 count array of the entry of tag in list.
func u32Counts(t testing.TB, list []tlv.Entry, tag uint16) []uint32 {
	t.Helper()

	v, err := tlv.DecodeU32Array(nthEntry(t, list, tag, 0).Value)
	require.NoError(t, err)

	return v
}

// withU64CountAt returns a copy of list whose u64 count array of tag has element i increased by one.
func withU64CountAt(t testing.TB, list []tlv.Entry, tag uint16, i int) []tlv.Entry {
	t.Helper()

	v, err := tlv.DecodeU64Array(nthEntry(t, list, tag, 0).Value)
	require.NoError(t, err)
	require.Less(t, i, len(v))
	v[i]++

	return replaceEntry(t, list, tag, 0, tlv.BytesEntry(tag, tlv.AppendU64Array(nil, v)))
}

// seqRangeEntries returns one seq_range entry with tag per range.
func seqRangeEntries(tag uint16, ranges ...seqRange) []tlv.Entry {
	out := make([]tlv.Entry, 0, len(ranges))
	for _, r := range ranges {
		out = append(out, tlv.BytesEntry(tag, tlv.AppendSeqRange(nil, tlv.SeqRange{First: r.first, Last: r.last})))
	}

	return out
}

// withSeqRanges returns a copy of list whose seq_range entries of tag are replaced by ranges.
func withSeqRanges(list []tlv.Entry, tag uint16, ranges ...seqRange) []tlv.Entry {
	return append(removeEntries(list, tag), seqRangeEntries(tag, ranges...)...)
}

// The rich pack's layout, which the rejection cases mutate (see richFooterSteps):
// block 0 holds seqs 10, 11 and 13 with seq_range entries [10, 11] and [13, 13],
// epoch 0 (seq 10, ts +50) and epoch 1 (seqs 11 to 13, ts +10 to +20), and the start boundary at seq 10;
// F-5 lists epochs 0 to 3 in order, epoch 1 with close_seq 15,
// and the boundaries at seqs 10 (start), 20 (gap, with gap_start and gap_end) and 31 (stop).

func TestParseFooterRejectsLayout(t *testing.T) {
	t.Parallel()

	base := richFooterPack(t, CodecNone)
	prologue := func(fn func(*format.FooterPrologue)) func(t *testing.T, b []byte) []byte {
		return func(t *testing.T, b []byte) []byte { return patchPrologue(t, b, fn) }
	}

	runFooterRejectCases(t, base, []footerRejectCase{
		{name: "footer shorter than F-1", raw: func(_ *testing.T, b []byte) []byte { return b[:71] }, want: "F-1"},
		{name: "footer_layout_version 2", raw: prologue(func(p *format.FooterPrologue) { p.FooterLayoutVersion = 2 }), want: "footer_layout_version"},
		{name: "F-3 present flag clear", raw: prologue(func(p *format.FooterPrologue) { p.Flags = 0x0002 }), want: "lack F-3 present"},
		{name: "f2_entry_len 79", raw: prologue(func(p *format.FooterPrologue) { p.F2EntryLen = 79 }), want: "f2_entry_len"},
		{name: "f2_offset 80", raw: prologue(func(p *format.FooterPrologue) { p.F2Offset = 80 }), want: "f2_offset 80"},
		{
			name:    "block_count differs from the trailer",
			trailer: func(tr *format.Trailer) { tr.BlockCount++ },
			want:    "block_count 5, trailer block_count 6",
		},
		{name: "F-2 past the footer end", raw: func(_ *testing.T, b []byte) []byte { return b[:200] }, want: "F-2 at offset 72"},
		{name: "F-3 overlaps F-2", raw: prologue(func(p *format.FooterPrologue) { p.F3Offset-- }), want: "F-3 at offset"},
		{name: "F-3 length at the u64 limit", raw: prologue(func(p *format.FooterPrologue) { p.F3Len = format.MaxU64 }), want: "F-3 at offset"},
		{
			name: "F-4 inside F-3",
			raw: prologue(func(p *format.FooterPrologue) {
				p.Flags, p.F4Offset, p.F4Len = p.Flags|format.FooterFlagF4Present, p.F3Offset, 1
			}),
			want: "F-4 at offset",
		},
		{
			name: "F-5 overlaps a non-empty F-4",
			raw:  prologue(func(p *format.FooterPrologue) { p.Flags, p.F4Len = p.Flags|format.FooterFlagF4Present, 1 }),
			want: "F-5 at offset",
		},
		{name: "F-4 bytes without the F-4 present flag", raw: prologue(func(p *format.FooterPrologue) { p.F4Len = 1 }), want: "F-4 present"},
		{
			name: "F-4 present flag without F-4 bytes",
			raw:  prologue(func(p *format.FooterPrologue) { p.Flags |= format.FooterFlagF4Present }),
			want: "F-4 present",
		},
		{
			name: "extraction_version without F-4",
			raw:  prologue(func(p *format.FooterPrologue) { p.ExtractionVersion = 1 }),
			want: "extraction_version 1",
		},
		{name: "F-5 before F-3", raw: prologue(func(p *format.FooterPrologue) { p.F5Offset = p.F3Offset }), want: "F-5 at offset"},
		{name: "F-5 past the footer end", raw: prologue(func(p *format.FooterPrologue) { p.F5Len++ }), want: "F-5 at offset"},
	})
}

// TestParseFooterAcceptsF4 checks that a footer with a non-empty F-4 and its present flag is accepted,
// its content unread, since the tracepack format specification §10 defers the secondary index.
func TestParseFooterAcceptsF4(t *testing.T) {
	t.Parallel()

	base := richFooterPack(t, CodecNone)
	b := splitFooter(t, base.decoded).encode()
	pro, err := format.UnmarshalFooterPrologue(b)
	require.NoError(t, err)

	const f4Len = 8
	withF4 := slices.Concat(b[:pro.F5Offset], []byte{1, 2, 3, 4, 5, 6, 7, 8}, b[pro.F5Offset:])
	withF4 = patchPrologue(t, withF4, func(p *format.FooterPrologue) {
		p.Flags |= format.FooterFlagF4Present
		p.ExtractionVersion = 1
		p.F4Offset, p.F4Len = p.F5Offset, f4Len
		p.F5Offset += f4Len
	})

	idx, err := parseFooter(withF4, &base.tr, base.blocksStart)
	require.NoError(t, err)
	assert.Len(t, idx.blocks, len(base.blocks))
}

// TestParseFooterAcceptsLargestBlock checks that a block whose body_len is at the 2^31-1 limit of
// the tracepack format specification §2 keeps its index entry:
// its on_disk_len is the 40-byte envelope plus that body, above 2^31-1.
func TestParseFooterAcceptsLargestBlock(t *testing.T) {
	t.Parallel()

	base := richFooterPack(t, CodecNone)
	last := len(base.blocks) - 1
	p := splitFooter(t, base.decoded)
	p.entries[last].OnDiskLen = format.EnvelopeLen + format.MaxLen32
	tr := base.tr
	tr.FooterOffset = base.blocks[last].offset + uint64(p.entries[last].OnDiskLen)

	_, err := parseFooter(p.encode(), &tr, base.blocksStart)
	require.NoError(t, err)

	p.entries[last].OnDiskLen++
	tr.FooterOffset++
	_, err = parseFooter(p.encode(), &tr, base.blocksStart)
	require.ErrorIs(t, err, ErrInvalidFooter)
	require.ErrorIs(t, err, format.ErrLimit)
}

func TestParseFooterRejectsBlockIndex(t *testing.T) {
	t.Parallel()

	base := richFooterPack(t, CodecNone)
	entry := func(i int, fn func(*format.F2Entry)) func(t *testing.T, b []byte) []byte {
		return func(t *testing.T, b []byte) []byte { return patchEntry(t, b, i, fn) }
	}
	lastOffset := base.blocks[4].offset

	runFooterRejectCases(t, base, []footerRejectCase{
		{name: "first block after the blocks start", startShift: 1, want: "block 0: F-2 offset"},
		{name: "gap between blocks", parts: func(_ *testing.T, p *footerParts) { p.entries[2].Offset++ }, want: "block 2: F-2 offset"},
		{name: "blocks end before footer_offset", trailer: func(tr *format.Trailer) { tr.FooterOffset++ }, want: "blocks end at"},
		{name: "last block runs past footer_offset", parts: func(_ *testing.T, p *footerParts) { p.entries[4].OnDiskLen++ }, want: "blocks end at"},
		{
			name:    "on_disk_len below the envelope",
			parts:   func(_ *testing.T, p *footerParts) { p.entries[4].OnDiskLen = format.EnvelopeLen - 1 },
			trailer: func(tr *format.Trailer) { tr.FooterOffset = lastOffset + format.EnvelopeLen - 1 },
			want:    "block 4: F-2 on_disk_len 39",
		},
		{name: "record_count 0", parts: func(_ *testing.T, p *footerParts) { p.entries[2].RecordCount = 0 }, want: "block 2: F-2 record_count 0"},
		{
			name:  "first_seq above last_seq",
			parts: func(_ *testing.T, p *footerParts) { p.entries[2].FirstSeq = 22 },
			want:  "block 2: F-2 first_seq 22 above last_seq 21",
		},
		{
			name:  "seqs do not ascend across blocks",
			parts: func(_ *testing.T, p *footerParts) { p.entries[2].FirstSeq = 20 },
			want:  "block 2: F-2 first_seq 20 does not follow the previous last_seq 20",
		},
		{
			name:  "record_header_len 55",
			parts: func(_ *testing.T, p *footerParts) { p.entries[1].RecordHeaderLen = 55 },
			want:  "block 1: F-2: footer block index entry: record_header_len",
		},
		{
			name:  "first_seq above the u64 limit",
			parts: func(_ *testing.T, p *footerParts) { p.entries[0].FirstSeq = format.MaxU64 + 1 },
			want:  "block 0: F-2: footer block index entry: first_seq",
		},
		{
			name: "ts_min above ts_max",
			parts: func(_ *testing.T, p *footerParts) {
				p.entries[0].TSMin, p.entries[0].TSMax = p.entries[0].TSMax, p.entries[0].TSMin
			},
			want: "block 0: F-2 ts_min",
		},
		{
			name:  "ts_min and ts_max in different hours",
			parts: func(_ *testing.T, p *footerParts) { p.entries[1].TSMax += hourNs },
			want:  "block 1: F-2 ts_min",
		},
		{
			name:  "epoch_min above epoch_max",
			parts: func(_ *testing.T, p *footerParts) { p.entries[0].EpochMin, p.entries[0].EpochMax = 1, 0 },
			want:  "block 0: F-2 epoch_min 1 above epoch_max 0",
		},
		{name: "summary_len 0", raw: entry(1, func(e *format.F2Entry) { e.SummaryLen = 0 }), want: "block 1: F-2 summary_len 0"},
		{name: "summary runs past F-3", raw: entry(4, func(e *format.F2Entry) { e.SummaryLen++ }), want: "block 4: F-2 summary at"},
		{
			name: "summary_offset past F-3",
			raw:  entry(0, func(e *format.F2Entry) { e.SummaryOffset = 1 << 40 }),
			want: "block 0: F-2 summary at",
		},
	})
}

func TestParseFooterRejectsBlockSummary(t *testing.T) {
	t.Parallel()

	base := richFooterPack(t, CodecNone)
	// epoch0 edits block 0's n-th epoch entry.
	epoch0 := func(n int, tag uint16, v tlv.Entry) func(t *testing.T, p *footerParts) {
		return func(t *testing.T, p *footerParts) { p.f3[0] = withNestedAt(t, p.f3[0], f3TagEpoch, n, tag, v) }
	}
	// ranges0 replaces block 0's seq_range entries.
	ranges0 := func(ranges ...seqRange) func(t *testing.T, p *footerParts) {
		return func(_ *testing.T, p *footerParts) { p.f3[0] = withSeqRanges(p.f3[0], f3TagSeqRange, ranges...) }
	}
	// counts0 edits block 0's u32 count array of tag.
	counts0 := func(tag uint16, fn func([]uint32) []uint32) func(t *testing.T, p *footerParts) {
		return func(t *testing.T, p *footerParts) {
			p.f3[0] = withU32Counts(t, p.f3[0], tag, fn(u32Counts(t, p.f3[0], tag)))
		}
	}
	plusOne := func(c []uint32) []uint32 { c[1]++; return c }

	runFooterRejectCases(t, base, []footerRejectCase{
		{
			name:  "entry list does not decode",
			parts: func(_ *testing.T, p *footerParts) { p.f3[0] = append(p.f3[0], tlv.U8Entry(0, 1)) },
			want:  "block 0: F-3: tlv: tag 0x0000",
		},
		{
			name:  "required tag missing",
			parts: func(_ *testing.T, p *footerParts) { p.f3[3] = removeEntries(p.f3[3], f3TagMaxPayloadLen) },
			want:  "block 3: F-3: tlv: tag 0x0004",
		},
		{
			name: "known tag of the wrong type",
			parts: func(t *testing.T, p *footerParts) {
				p.f3[0] = replaceEntry(t, p.f3[0], f3TagContentBytes, 0, tlv.I64Entry(f3TagContentBytes, 1))
			},
			want: "block 0: F-3: tlv: tag 0x0006",
		},
		{name: "kind_counts sum above record_count", parts: counts0(f3TagKindCounts, plusOne), want: "block 0: F-3 kind_counts sums past record_count 3"},
		{name: "dir_counts sum above record_count", parts: counts0(f3TagDirCounts, plusOne), want: "block 0: F-3 dir_counts sums past"},
		{
			name:  "decode_status_counts sum below record_count",
			parts: counts0(f3TagDecodeStatusCounts, func(c []uint32) []uint32 { return c[:len(c)-1] }),
			want:  "block 0: F-3 decode_status_counts sums to",
		},
		{
			name:  "kind_counts sum overflowing 32 bits",
			parts: counts0(f3TagKindCounts, func([]uint32) []uint32 { return []uint32{math.MaxUint32, 4} }),
			want:  "block 0: F-3 kind_counts sums past record_count 3",
		},
		{
			name: "count array of a partial element",
			parts: func(t *testing.T, p *footerParts) {
				p.f3[0] = replaceEntry(t, p.f3[0], f3TagKindCounts, 0, tlv.BytesEntry(f3TagKindCounts, []byte{3, 0, 0, 0, 0}))
			},
			want: "block 0: F-3 kind_counts: u32 array",
		},
		{
			name: "max_payload_len above the payload_len limit",
			parts: func(t *testing.T, p *footerParts) {
				p.f3[0] = replaceEntry(t, p.f3[0], f3TagMaxPayloadLen, 0, tlv.U64Entry(f3TagMaxPayloadLen, uint64(format.MaxLen32)+1))
			},
			want: "block 0: F-3 max_payload_len",
		},
		{
			name: "quality_union above 16 bits",
			parts: func(t *testing.T, p *footerParts) {
				p.f3[0] = replaceEntry(t, p.f3[0], f3TagQualityUnion, 0, tlv.U64Entry(f3TagQualityUnion, 0x10000))
			},
			want: "block 0: F-3 quality_union",
		},
		{
			name: "content_bytes differs from uncompressed_len",
			parts: func(t *testing.T, p *footerParts) {
				v := uint64(p.entries[0].UncompressedLen) + 1
				p.f3[0] = replaceEntry(t, p.f3[0], f3TagContentBytes, 0, tlv.U64Entry(f3TagContentBytes, v))
			},
			want: "block 0: F-3 content_bytes",
		},
		{
			name:  "epoch listed twice",
			parts: func(t *testing.T, p *footerParts) { p.f3[0] = append(p.f3[0], nthEntry(t, p.f3[0], f3TagEpoch, 0)) },
			want:  "block 0: F-3 epoch 0 listed twice",
		},
		{name: "epoch above epoch_max", parts: epoch0(1, epochTagEpoch, tlv.U64Entry(epochTagEpoch, 2)), want: "block 0: F-3 epoch 2 outside"},
		{
			name:  "epoch above 2^32-1",
			parts: epoch0(1, epochTagEpoch, tlv.U64Entry(epochTagEpoch, math.MaxUint32+1)),
			want:  "block 0: F-3 epoch: epoch 4294967296 exceeds",
		},
		{name: "epoch record_count 0", parts: epoch0(1, epochTagRecordCount, tlv.U64Entry(epochTagRecordCount, 0)), want: "block 0: F-3 epoch 1 record_count 0"},
		{name: "epoch seq_first above seq_last", parts: epoch0(1, epochTagSeqFirst, tlv.U64Entry(epochTagSeqFirst, 13+1)), want: "block 0: F-3 epoch 1 seqs"},
		{name: "epoch seq_first below first_seq", parts: epoch0(0, epochTagSeqFirst, tlv.U64Entry(epochTagSeqFirst, 9)), want: "block 0: F-3 epoch 0 seqs"},
		{name: "epoch seq_last above last_seq", parts: epoch0(1, epochTagSeqLast, tlv.U64Entry(epochTagSeqLast, 14)), want: "block 0: F-3 epoch 1 seqs"},
		{
			name:  "epoch ts_min above ts_max",
			parts: epoch0(1, epochTagTSMin, tlv.I64Entry(epochTagTSMin, blockTestHour+21)),
			want:  "block 0: F-3 epoch 1 times",
		},
		{
			name:  "epoch ts_max above the block's",
			parts: epoch0(0, epochTagTSMax, tlv.I64Entry(epochTagTSMax, blockTestHour+51)),
			want:  "block 0: F-3 epoch 0 times",
		},
		{
			name:  "epoch ts_min below the block's",
			parts: epoch0(1, epochTagTSMin, tlv.I64Entry(epochTagTSMin, blockTestHour+9)),
			want:  "block 0: F-3 epoch 1 times",
		},
		{
			name:  "close_seq in a seq gap",
			parts: epoch0(1, epochTagCloseSeq, tlv.U64Entry(epochTagCloseSeq, 12)),
			want:  "block 0: F-3 epoch 1 close_seq 12 is not a seq of the block",
		},
		{
			name:  "epoch record counts above record_count",
			parts: epoch0(1, epochTagRecordCount, tlv.U64Entry(epochTagRecordCount, 3)),
			want:  "block 0: F-3 epoch record counts sum past record_count 3",
		},
		{
			name:  "epoch record counts below record_count",
			parts: epoch0(1, epochTagRecordCount, tlv.U64Entry(epochTagRecordCount, 1)),
			want:  "block 0: F-3 epoch record counts sum to 2, record_count is 3",
		},
		{
			name: "epoch record counts overflowing 64 bits",
			parts: func(t *testing.T, p *footerParts) {
				// (2^63-1) + (2^63-1) + 5 wraps to 3, block 0's record_count.
				p.entries[0].EpochMax = 2
				p.f3[0] = withNestedAt(t, p.f3[0], f3TagEpoch, 0, epochTagRecordCount, tlv.U64Entry(epochTagRecordCount, format.MaxU64))
				p.f3[0] = withNestedAt(t, p.f3[0], f3TagEpoch, 1, epochTagRecordCount, tlv.U64Entry(epochTagRecordCount, format.MaxU64))
				p.f3[0] = append(p.f3[0], epochEntry(f3TagEpoch, &epochSummary{
					epoch: 2, recordCount: 5, seqFirst: 13, seqLast: 13, tsMin: blockTestHour + 20, tsMax: blockTestHour + 20,
				}))
			},
			want: "block 0: F-3 epoch record counts sum past record_count 3",
		},
		{
			name: "no seq_range entries hide a missing seq",
			parts: func(_ *testing.T, p *footerParts) {
				// Block 0 claims record_count 3 for seqs 10 to 13; F-5 is made to agree, so only the block's seq set is wrong.
				p.f3[0] = removeEntries(p.f3[0], f3TagSeqRange)
				p.f5 = withSeqRanges(p.f5, f5TagSeqRange, seqRange{10, 15}, seqRange{20, 23}, seqRange{30, 31})
			},
			want: "block 0: F-3 without seq_range entries seqs 10 to 13 are 4 records, record_count is 3",
		},
		{name: "seq_range runs backwards", parts: ranges0(seqRange{10, 11}, seqRange{13, 12}), want: "block 0: F-3 seq_range 1 runs from 13 back to 12"},
		{name: "seq_range not starting at first_seq", parts: ranges0(seqRange{11, 11}, seqRange{13, 14}), want: "block 0: F-3 seq_range 0 starts at 11"},
		{
			name:  "seq_range entries out of order",
			parts: ranges0(seqRange{10, 10}, seqRange{13, 13}, seqRange{11, 11}),
			want:  "block 0: F-3 seq_range 2 starts at 11, overlapping or touching",
		},
		{
			name:  "seq_range entries overlapping",
			parts: ranges0(seqRange{10, 11}, seqRange{11, 13}),
			want:  "block 0: F-3 seq_range 1 starts at 11, overlapping or touching",
		},
		{
			name:  "seq_range entries not maximal",
			parts: ranges0(seqRange{10, 10}, seqRange{11, 11}, seqRange{13, 13}),
			want:  "block 0: F-3 seq_range 1 starts at 11, overlapping or touching",
		},
		{name: "seq_range entries not ending at last_seq", parts: ranges0(seqRange{10, 11}, seqRange{14, 14}), want: "block 0: F-3 seq_range entries end at 14"},
		{
			name:  "seq_range entries holding fewer than record_count seqs",
			parts: ranges0(seqRange{10, 10}, seqRange{13, 13}),
			want:  "block 0: F-3 seq_range entries hold 2 seqs, record_count is 3",
		},
		{
			name:  "seq_range entries holding more than record_count seqs",
			parts: ranges0(seqRange{10, 11}, seqRange{13, 13}, seqRange{15, 15}),
			want:  "block 0: F-3 seq_range entries hold more than record_count 3 seqs",
		},
		{
			name: "seq_range of 15 bytes",
			parts: func(_ *testing.T, p *footerParts) {
				p.f3[0] = append(removeEntries(p.f3[0], f3TagSeqRange), tlv.BytesEntry(f3TagSeqRange, make([]byte, 15)))
			},
			want: "block 0: F-3 seq_range: seq range of 15 bytes",
		},
		{
			name: "boundary seq in a seq gap",
			parts: func(t *testing.T, p *footerParts) {
				p.f3[0] = withNestedAt(t, p.f3[0], f3TagBoundary, 0, boundaryTagSeq, tlv.U64Entry(boundaryTagSeq, 12))
			},
			want: "block 0: F-3 boundary seq 12 is not a seq of the block",
		},
		{
			name: "boundary epoch above 2^32-1",
			parts: func(t *testing.T, p *footerParts) {
				p.f3[0] = withNestedAt(t, p.f3[0], f3TagBoundary, 0, boundaryTagEpoch, tlv.U64Entry(boundaryTagEpoch, math.MaxUint32+1))
			},
			want: "block 0: F-3 boundary: epoch 4294967296 exceeds",
		},
	})
}

func TestParseFooterRejectsPackStats(t *testing.T) {
	t.Parallel()

	base := richFooterPack(t, CodecZstd)
	// f5 replaces the F-5 entry of tag with e.
	f5 := func(tag uint16, e tlv.Entry) func(t *testing.T, p *footerParts) {
		return func(t *testing.T, p *footerParts) { p.f5 = replaceEntry(t, p.f5, tag, 0, e) }
	}
	// epoch1 edits F-5's entry for epoch 1, the one with close_seq 15.
	epoch1 := func(tag uint16, v tlv.Entry) func(t *testing.T, p *footerParts) {
		return func(t *testing.T, p *footerParts) { p.f5 = withNestedAt(t, p.f5, f5TagEpoch, 1, tag, v) }
	}
	// gap edits F-5's gap boundary entry.
	gap := func(tag uint16, v tlv.Entry) func(t *testing.T, p *footerParts) {
		return func(t *testing.T, p *footerParts) { p.f5 = withNestedAt(t, p.f5, f5TagBoundary, 1, tag, v) }
	}
	drop := func(tag uint16) func(t *testing.T, p *footerParts) {
		return func(_ *testing.T, p *footerParts) { p.f5 = removeEntries(p.f5, tag) }
	}
	ranges := func(r ...seqRange) func(t *testing.T, p *footerParts) {
		return func(_ *testing.T, p *footerParts) { p.f5 = withSeqRanges(p.f5, f5TagSeqRange, r...) }
	}
	counts := func(tag uint16) func(t *testing.T, p *footerParts) {
		return func(t *testing.T, p *footerParts) { p.f5 = withU64CountAt(t, p.f5, tag, 1) }
	}
	stats := aggregate(base.blocks)

	runFooterRejectCases(t, base, []footerRejectCase{
		{name: "entry list does not decode", parts: func(_ *testing.T, p *footerParts) { p.f5 = append(p.f5, tlv.U8Entry(0, 1)) }, want: "F-5: tlv: tag 0x0000"},
		{name: "known tag of the wrong type", parts: f5(f5TagRecordCount, tlv.I64Entry(f5TagRecordCount, 11)), want: "F-5: tlv: tag 0x0001"},
		{name: "record_count missing", parts: drop(f5TagRecordCount), want: "F-5 lacks record_count"},
		{name: "kind_counts missing", parts: drop(f5TagKindCounts), want: "F-5 lacks kind_counts"},
		{name: "dir_counts missing", parts: drop(f5TagDirCounts), want: "F-5 lacks dir_counts"},
		{name: "decode_status_counts missing", parts: drop(f5TagDecodeStatusCounts), want: "F-5 lacks decode_status_counts"},
		{name: "content_bytes missing", parts: drop(f5TagContentBytes), want: "F-5 lacks content_bytes"},
		{name: "quality_union missing", parts: drop(f5TagQualityUnion), want: "F-5 lacks quality_union"},
		{name: "ts_min missing", parts: drop(f5TagTSMin), want: "F-5 ts_min present is false"},
		{name: "ts_max missing", parts: drop(f5TagTSMax), want: "F-5 ts_max present is false"},
		{name: "record_count differs", parts: f5(f5TagRecordCount, tlv.U64Entry(f5TagRecordCount, stats.recordCount+1)), want: "F-5 record_count 12, the blocks hold 11"},
		{name: "kind_counts differ", parts: counts(f5TagKindCounts), want: "F-5 kind_counts"},
		{name: "dir_counts differ", parts: counts(f5TagDirCounts), want: "F-5 dir_counts"},
		{name: "decode_status_counts differ", parts: counts(f5TagDecodeStatusCounts), want: "F-5 decode_status_counts"},
		{
			name:  "count array above the u64 limit",
			parts: f5(f5TagKindCounts, tlv.BytesEntry(f5TagKindCounts, tlv.AppendU64Array(nil, []uint64{format.MaxU64 + 1}))),
			want:  "F-5 kind_counts: u64 array element 0",
		},
		{name: "ts_min differs", parts: f5(f5TagTSMin, tlv.I64Entry(f5TagTSMin, stats.tsMin-1)), want: "F-5 ts_min"},
		{name: "ts_max differs", parts: f5(f5TagTSMax, tlv.I64Entry(f5TagTSMax, stats.tsMax+1)), want: "F-5 ts_min"},
		{name: "content_bytes differs", parts: f5(f5TagContentBytes, tlv.U64Entry(f5TagContentBytes, stats.contentBytes+1)), want: "F-5 content_bytes"},
		{
			name:  "quality_union differs",
			parts: f5(f5TagQualityUnion, tlv.U64Entry(f5TagQualityUnion, uint64(stats.qualityUnion^QualityRedacted))),
			want:  "F-5 quality_union 0x",
		},
		{
			name:  "quality_union above 16 bits",
			parts: f5(f5TagQualityUnion, tlv.U64Entry(f5TagQualityUnion, uint64(stats.qualityUnion)|0x10000)),
			want:  "F-5 quality_union: 0x",
		},
		{name: "seq range missing", parts: ranges(seqRange{10, 11}, seqRange{13, 15}, seqRange{20, 23}), want: "F-5 seq_range entries"},
		{name: "seq ranges not coalesced", parts: ranges(seqRange{10, 11}, seqRange{13, 14}, seqRange{15, 15}, seqRange{20, 23}, seqRange{30, 31}), want: "F-5 seq_range entries"},
		{name: "seq ranges out of order", parts: ranges(seqRange{13, 15}, seqRange{10, 11}, seqRange{20, 23}, seqRange{30, 31}), want: "F-5 seq_range entries"},
		{
			name: "seq_range of 17 bytes",
			parts: func(_ *testing.T, p *footerParts) {
				p.f5 = append(p.f5, tlv.BytesEntry(f5TagSeqRange, make([]byte, 17)))
			},
			want: "F-5 seq_range: seq range of 17 bytes",
		},
		{name: "epoch record_count differs", parts: epoch1(epochTagRecordCount, tlv.U64Entry(epochTagRecordCount, 5)), want: "F-5 epoch entries differ"},
		{name: "epoch seq_first differs", parts: epoch1(epochTagSeqFirst, tlv.U64Entry(epochTagSeqFirst, 12)), want: "F-5 epoch entries differ"},
		{name: "epoch seq_last differs", parts: epoch1(epochTagSeqLast, tlv.U64Entry(epochTagSeqLast, 16)), want: "F-5 epoch entries differ"},
		{name: "epoch ts_min differs", parts: epoch1(epochTagTSMin, tlv.I64Entry(epochTagTSMin, blockTestHour)), want: "F-5 epoch entries differ"},
		{name: "epoch ts_max differs", parts: epoch1(epochTagTSMax, tlv.I64Entry(epochTagTSMax, blockTestHour+41)), want: "F-5 epoch entries differ"},
		{name: "epoch close_seq differs", parts: epoch1(epochTagCloseSeq, tlv.U64Entry(epochTagCloseSeq, 14)), want: "F-5 epoch entries differ"},
		{name: "epoch close_seq missing", parts: epoch1(epochTagCloseSeq, tlv.Entry{}), want: "F-5 epoch entries differ"},
		{
			name: "epoch close_seq added",
			parts: func(t *testing.T, p *footerParts) {
				p.f5 = withNestedAt(t, p.f5, f5TagEpoch, 2, epochTagCloseSeq, tlv.U64Entry(epochTagCloseSeq, 21))
			},
			want: "F-5 epoch entries differ",
		},
		{
			name: "epoch missing",
			parts: func(t *testing.T, p *footerParts) {
				p.f5 = slices.Delete(slices.Clone(p.f5), entryIndex(p.f5, f5TagEpoch, 3), entryIndex(p.f5, f5TagEpoch, 3)+1)
			},
			want: "F-5 epoch entries differ",
		},
		{
			name: "epoch added",
			parts: func(_ *testing.T, p *footerParts) {
				p.f5 = append(p.f5, epochEntry(f5TagEpoch, &epochSummary{epoch: 9, recordCount: 1, seqFirst: 31, seqLast: 31}))
			},
			want: "F-5 epoch entries differ",
		},
		{
			name:  "epoch replaced by another",
			parts: epoch1(epochTagEpoch, tlv.U64Entry(epochTagEpoch, 7)),
			want:  "F-5 epoch entries differ",
		},
		{
			name: "epoch listed twice",
			parts: func(t *testing.T, p *footerParts) {
				p.f5 = replaceEntry(t, p.f5, f5TagEpoch, 3, nthEntry(t, p.f5, f5TagEpoch, 2))
			},
			want: "F-5 epoch 2 listed twice",
		},
		{name: "epoch above 2^32-1", parts: epoch1(epochTagEpoch, tlv.U64Entry(epochTagEpoch, math.MaxUint32+1)), want: "F-5 epoch: epoch 4294967296 exceeds"},
		{name: "boundary seq differs", parts: gap(boundaryTagSeq, tlv.U64Entry(boundaryTagSeq, 21)), want: "F-5 boundary entries differ"},
		{name: "boundary kind differs", parts: gap(boundaryTagKind, tlv.U8Entry(boundaryTagKind, uint8(BoundaryKindStart))), want: "F-5 boundary entries differ"},
		{name: "boundary ts differs", parts: gap(boundaryTagTS, tlv.I64Entry(boundaryTagTS, blockTestHour)), want: "F-5 boundary entries differ"},
		{name: "boundary epoch differs", parts: gap(boundaryTagEpoch, tlv.U64Entry(boundaryTagEpoch, 1)), want: "F-5 boundary entries differ"},
		{name: "boundary gap_start missing", parts: gap(boundaryTagGapStart, tlv.Entry{}), want: "F-5 boundary entries differ"},
		{name: "boundary gap_end differs", parts: gap(boundaryTagGapEnd, tlv.I64Entry(boundaryTagGapEnd, 0)), want: "F-5 boundary entries differ"},
		{
			name: "boundary missing",
			parts: func(t *testing.T, p *footerParts) {
				p.f5 = slices.Delete(slices.Clone(p.f5), entryIndex(p.f5, f5TagBoundary, 2), entryIndex(p.f5, f5TagBoundary, 2)+1)
			},
			want: "F-5 boundary entries differ",
		},
		{
			name: "one boundary listed twice in place of another",
			parts: func(t *testing.T, p *footerParts) {
				p.f5 = replaceEntry(t, p.f5, f5TagBoundary, 2, nthEntry(t, p.f5, f5TagBoundary, 0))
			},
			want: "F-5 boundary entries differ",
		},
		{
			name:  "boundary epoch above 2^32-1",
			parts: gap(boundaryTagEpoch, tlv.U64Entry(boundaryTagEpoch, math.MaxUint32+1)),
			want:  "F-5 boundary: epoch 4294967296 exceeds",
		},
	})
}

func TestParseFooterRejectsTrailerTotals(t *testing.T) {
	t.Parallel()

	base := richFooterPack(t, CodecZstd)

	runFooterRejectCases(t, base, []footerRejectCase{
		{name: "record_count differs", trailer: func(tr *format.Trailer) { tr.RecordCount++ }, want: "trailer record_count 12, F-5 record_count 11"},
		{name: "last_seq differs", trailer: func(tr *format.Trailer) { tr.LastSeq-- }, want: "trailer last_seq 30, the last block's last_seq 31"},
		{name: "block_count differs", trailer: func(tr *format.Trailer) { tr.BlockCount-- }, want: "block_count 5, trailer block_count 4"},
		{name: "footer_offset differs", trailer: func(tr *format.Trailer) { tr.FooterOffset-- }, want: "blocks end at"},
	})
}

func TestParseFooterRejectsEmptyPack(t *testing.T) {
	t.Parallel()

	base := emptyFooterPack(t)
	add := func(e tlv.Entry) func(t *testing.T, p *footerParts) {
		return func(_ *testing.T, p *footerParts) { p.f5 = append(p.f5, e) }
	}

	runFooterRejectCases(t, base, []footerRejectCase{
		{name: "stray ts_min", parts: add(tlv.I64Entry(f5TagTSMin, blockTestHour)), want: "F-5 ts_min present is true"},
		{name: "stray ts_max", parts: add(tlv.I64Entry(f5TagTSMax, blockTestHour)), want: "F-5 ts_max present is true"},
		{name: "stray seq_range", parts: add(seqRangeEntries(f5TagSeqRange, seqRange{0, 0})[0]), want: "F-5 seq_range entries"},
		{
			name:  "stray epoch",
			parts: add(epochEntry(f5TagEpoch, &epochSummary{epoch: 1, recordCount: 1})),
			want:  "F-5 epoch entries differ",
		},
		{
			name:  "stray boundary",
			parts: add(boundaryEntry(f5TagBoundary, &boundarySummary{kind: BoundaryKindStart})),
			want:  "F-5 boundary entries differ",
		},
		{
			name: "record_count 1",
			parts: func(t *testing.T, p *footerParts) {
				p.f5 = replaceEntry(t, p.f5, f5TagRecordCount, 0, tlv.U64Entry(f5TagRecordCount, 1))
			},
			trailer: func(tr *format.Trailer) { tr.RecordCount = 1 },
			want:    "F-5 record_count 1, the blocks hold 0",
		},
		{
			name: "non-zero kind count",
			parts: func(t *testing.T, p *footerParts) {
				p.f5 = replaceEntry(t, p.f5, f5TagKindCounts, 0, tlv.BytesEntry(f5TagKindCounts, tlv.AppendU64Array(nil, []uint64{0, 1})))
			},
			want: "F-5 kind_counts",
		},
		{name: "footer_offset after the blocks start", trailer: func(tr *format.Trailer) { tr.FooterOffset++ }, want: "blocks end at"},
		{
			name: "non-empty F-3",
			raw: func(t *testing.T, b []byte) []byte {
				// F-3 takes the first byte of F-5's former place; F-5 moves one byte on.
				b = append(slices.Clone(b[:format.FooterPrologueLen]), append([]byte{0}, b[format.FooterPrologueLen:]...)...)
				return patchPrologue(t, b, func(p *format.FooterPrologue) {
					p.F3Len, p.F4Offset, p.F5Offset = 1, p.F4Offset+1, p.F5Offset+1
				})
			},
			want: "F-1 f3_len 1 in a pack without blocks",
		},
	})
}

func TestParseFooterIgnoresEmptyPackLastSeq(t *testing.T) {
	t.Parallel()

	p := emptyFooterPack(t)
	tr := p.tr
	tr.LastSeq = 12345

	_, err := parseFooter(p.decoded, &tr, p.blocksStart)
	require.NoError(t, err)
}

func FuzzParseFooter(f *testing.F) {
	rich := richFooterPack(f, CodecZstd)
	plain := richFooterPack(f, CodecNone)
	empty := emptyFooterPack(f)
	padded := splitFooter(f, rich.decoded)
	padded.entryPad = make([]byte, 8)

	seeds := []struct {
		decoded []byte
		tr      format.Trailer
		start   uint64
	}{
		{rich.decoded, rich.tr, rich.blocksStart},
		{plain.decoded, plain.tr, plain.blocksStart},
		{padded.encode(), rich.tr, rich.blocksStart},
		{empty.decoded, empty.tr, empty.blocksStart},
	}
	for _, s := range seeds {
		f.Add(s.decoded, s.tr.FooterOffset, s.tr.BlockCount, s.tr.RecordCount, s.tr.LastSeq, s.start)
	}

	f.Fuzz(func(t *testing.T, decoded []byte, footerOffset uint64, blockCount uint32, recordCount, lastSeq, blocksStart uint64) {
		tr := format.Trailer{FooterOffset: footerOffset, BlockCount: blockCount, RecordCount: recordCount, LastSeq: lastSeq}

		idx, err := parseFooter(decoded, &tr, blocksStart)
		if err != nil {
			require.ErrorIs(t, err, ErrInvalidFooter)
			require.Nil(t, idx)

			return
		}

		require.Len(t, idx.blocks, int(blockCount))
		stats := footerStats(t, decoded)
		assert.Equal(t, recordCount, stats.recordCount)
		assert.Equal(t, normalizeStats(aggregate(idx.blocks)), normalizeStats(stats),
			"the statistics of an accepted footer are the aggregate of its blocks")
	})
}
