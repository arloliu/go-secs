package tracepack

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"slices"

	"github.com/arloliu/go-secs/tracepack/internal/format"
	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// maxQualityUnion is the largest quality_union a footer may carry:
// the union of the records' quality bits, which are the low 16 bits (the tracepack format specification §10).
const maxQualityUnion = math.MaxUint16

// ErrInvalidFooter reports a footer that fails the footer validation of the tracepack format specification §10.
// A reader that meets it falls back to the forward walk of I-1 and never trusts the footer's index.
var ErrInvalidFooter = errors.New("tracepack: invalid footer")

// footerIndex is a decoded footer that passed the footer validation of the tracepack format specification §10.
type footerIndex struct {
	// blocks holds each block's F-2 entry and F-3 summary typed into the writer's shape, in file order;
	// a block whose seqs are contiguous has one seq range, first_seq to last_seq.
	blocks []blockSummary
	// f3 is a copy of the F-3 section, kept only when parseFooter was asked to keep it, else nil;
	// being a copy, it retains no other part of the decoded footer.
	// It is never modified, and the lists f3List returns alias it.
	f3 []byte
	// f3Spans locates each block's F-3 entry list in f3, parallel to blocks; nil when f3 is not kept.
	// Two spans may overlap: the footer validation requires each to lie inside F-3, not that they be disjoint.
	f3Spans []f3Span
	// stats is the footer's F-5 statistics as stored, which the validation found equal to the blocks' aggregate,
	// normalized: epochs in ascending epoch, boundaries in ascending seq.
	// It is never modified, and Reader.Stats copies it.
	stats *packStats
}

// footerSections holds the F-2, F-3 and F-5 sections of a decoded footer.
type footerSections struct {
	f2 []byte
	f3 []byte
	f5 []byte
}

// f3Span is the location of one block's F-3 entry list in the F-3 section: its F-2 summary_offset and summary_len.
type f3Span struct {
	offset uint64
	n      uint32
}

// parseFooter decodes a decoded footer and applies the footer validation of the tracepack format specification §10.
//
// decoded is the footer after the trailer's footer_codec, whose footer CRC the caller has verified;
// tr is the verified trailer, and blocksStart is 80 + pack_metadata_len, where the first block starts.
// The F-2 and F-3 checks run before the blocks are aggregated,
// so aggregate sees blocks in ascending seq order whose count arrays and epoch record counts sum to their record_count.
// Every value aggregate adds per block is then below 2^32, so no sum over fewer than 2^32 blocks wraps 64 bits,
// and a sum above 2^63-1 cannot equal its F-5 value, which the TLV decoder keeps at or below 2^63-1.
// Every section and summary location is bounds-checked before it slices,
// and the F-2 section is found inside decoded before its entries are allocated.
// The blocks' F-3 lists, each counted once per block that locates it, may total at most maxF3 bytes,
// which is checked before each block's list is parsed:
// the footer validation lets lists overlap, and every block parses its own, count arrays included,
// so lists that share bytes would otherwise cost them once per block.
// No input thus makes parseFooter panic or allocate beyond what decoded's length and maxF3 bound.
// With keepF3 set, a valid footer's index also keeps a copy of F-3 and each block's span in it, for f3List;
// without it the index keeps neither.
// A valid footer's index always keeps its decoded F-5 statistics, for Reader.Stats.
//
// Returns:
//   - *footerIndex: the decoded footer; nil on error.
//   - error: nil, an error wrapping ErrInvalidFooter that names the failed check and, for a block, its index,
//     or an error wrapping ErrReadLimit, and not ErrInvalidFooter, for lists over maxF3.
func parseFooter(decoded []byte, tr *format.Trailer, blocksStart, maxF3 uint64, keepF3 bool) (*footerIndex, error) {
	pro, err := format.UnmarshalFooterPrologue(decoded)
	if err != nil {
		return nil, footerErrorf("F-1: %w", err)
	}
	if err := checkPrologue(&pro, tr); err != nil {
		return nil, err
	}

	sec, err := locateSections(decoded, &pro)
	if err != nil {
		return nil, err
	}

	entries, err := parseBlockIndex(sec.f2, &pro, blocksStart, tr.FooterOffset)
	if err != nil {
		return nil, err
	}

	blocks := make([]blockSummary, len(entries))
	// Fewer than 2^32 lists of fewer than 2^32 bytes each, so the total cannot overflow 64 bits.
	var listsLen uint64
	for i := range entries {
		e := &entries[i]
		if listsLen += uint64(e.SummaryLen); listsLen > maxF3 {
			return nil, fmt.Errorf("tracepack: footer at offset %d: the F-3 lists of blocks 0 to %d total %d bytes and exceed MaxFooterLen %d: %w",
				tr.FooterOffset, i, listsLen, maxF3, ErrReadLimit)
		}
		list := sec.f3[e.SummaryOffset : e.SummaryOffset+uint64(e.SummaryLen)]
		if blocks[i], err = parseBlockSummary(i, list, e); err != nil {
			return nil, err
		}
	}

	stats, err := parsePackStats(sec.f5, len(blocks) > 0)
	if err != nil {
		return nil, err
	}
	if err := checkPackStats(stats, aggregate(blocks)); err != nil {
		return nil, err
	}
	if err := checkTrailerTotals(stats, blocks, tr); err != nil {
		return nil, err
	}

	stats.normalize()
	idx := &footerIndex{blocks: blocks, stats: stats}
	if keepF3 {
		idx.retainF3(sec.f3, entries)
	}

	return idx, nil
}

// checkPrologue checks the F-1 fields UnmarshalFooterPrologue leaves to the caller:
// F-3 present, F-4 present exactly when f4_len is not 0, f2_offset = 72, block_count equal to the trailer's, and an empty F-3 in a pack without blocks.
func checkPrologue(pro *format.FooterPrologue, tr *format.Trailer) error {
	if pro.Flags&format.FooterFlagF3Present == 0 {
		return footerErrorf("F-1 flags 0x%04X lack F-3 present", pro.Flags)
	}
	f4Present := pro.Flags&format.FooterFlagF4Present != 0
	if f4Present != (pro.F4Len != 0) {
		return footerErrorf("F-1 flags 0x%04X disagree with f4_len %d on F-4 present", pro.Flags, pro.F4Len)
	}
	if pro.F2Offset != format.FooterPrologueLen {
		return footerErrorf("F-1 f2_offset %d, not %d", pro.F2Offset, format.FooterPrologueLen)
	}
	if pro.BlockCount != tr.BlockCount {
		return footerErrorf("F-1 block_count %d, trailer block_count %d", pro.BlockCount, tr.BlockCount)
	}
	if pro.BlockCount == 0 && pro.F3Len != 0 {
		return footerErrorf("F-1 f3_len %d in a pack without blocks", pro.F3Len)
	}

	return nil
}

// locateSections finds F-2, F-3, F-4 and F-5 inside decoded, in that order and without overlap,
// and returns F-2, F-3 and F-5; F-4 may be empty, and its content is not read.
func locateSections(decoded []byte, pro *format.FooterPrologue) (footerSections, error) {
	// Both factors are below 2^32, so the F-2 length cannot overflow 64 bits.
	f2Len := uint64(pro.BlockCount) * uint64(pro.F2EntryLen)
	spans := [...]struct {
		name string
		off  uint64
		n    uint64
	}{
		{"F-2", pro.F2Offset, f2Len},
		{"F-3", pro.F3Offset, pro.F3Len},
		{"F-4", pro.F4Offset, pro.F4Len},
		{"F-5", pro.F5Offset, pro.F5Len},
	}

	var sec [len(spans)][]byte
	prevEnd := uint64(format.FooterPrologueLen)
	for i, s := range spans {
		end, carry := bits.Add64(s.off, s.n, 0)
		if carry != 0 || s.off < prevEnd || end > uint64(len(decoded)) {
			return footerSections{}, footerErrorf(
				"%s at offset %d of %d bytes does not lie between the previous section's end %d and the footer's end %d",
				s.name, s.off, s.n, prevEnd, len(decoded))
		}
		sec[i] = decoded[s.off:end]
		prevEnd = end
	}

	return footerSections{f2: sec[0], f3: sec[1], f5: sec[3]}, nil
}

// parseBlockIndex decodes the F-2 entries of section f2 and checks each one alone,
// then that the blocks they locate are contiguous from blocksStart to footerOffset
// and that their seqs strictly ascend across entries.
func parseBlockIndex(f2 []byte, pro *format.FooterPrologue, blocksStart, footerOffset uint64) ([]format.F2Entry, error) {
	entryLen := int(pro.F2EntryLen)
	// f2 lies inside the decoded footer, so block_count entries of f2_entry_len bytes are bounded by its length.
	entries := make([]format.F2Entry, pro.BlockCount)
	next := blocksStart

	for i := range entries {
		e, err := format.UnmarshalF2Entry(f2[i*entryLen:], entryLen)
		if err != nil {
			return nil, footerErrorf("block %d: F-2: %w", i, err)
		}
		if e.Offset != next {
			return nil, footerErrorf("block %d: F-2 offset %d, the previous block ends at %d", i, e.Offset, next)
		}
		if err := checkBlockEntry(&e, pro.F3Len); err != nil {
			return nil, footerErrorf("block %d: %w", i, err)
		}
		if i > 0 && e.FirstSeq <= entries[i-1].LastSeq {
			return nil, footerErrorf("block %d: F-2 first_seq %d does not follow the previous last_seq %d", i, e.FirstSeq, entries[i-1].LastSeq)
		}

		entries[i] = e
		// offset is at most 2^63-1 and on_disk_len below 2^32, so the end cannot overflow 64 bits.
		next = e.Offset + uint64(e.OnDiskLen)
	}

	if next != footerOffset {
		return nil, footerErrorf("blocks end at %d, footer_offset is %d", next, footerOffset)
	}

	return entries, nil
}

// checkBlockEntry checks the rules of one F-2 entry that need no other entry,
// with f3Len the length of F-3, which must hold the entry's summary.
func checkBlockEntry(e *format.F2Entry, f3Len uint64) error {
	switch {
	case e.OnDiskLen < format.EnvelopeLen:
		return fmt.Errorf("F-2 on_disk_len %d below the %d-byte envelope", e.OnDiskLen, format.EnvelopeLen)
	case e.RecordCount == 0:
		return errors.New("F-2 record_count 0")
	case e.FirstSeq > e.LastSeq:
		return fmt.Errorf("F-2 first_seq %d above last_seq %d", e.FirstSeq, e.LastSeq)
	case e.TSMin > e.TSMax:
		return fmt.Errorf("F-2 ts_min %d above ts_max %d", e.TSMin, e.TSMax)
	case hourOf(e.TSMin) != hourOf(e.TSMax):
		return fmt.Errorf("F-2 ts_min %d and ts_max %d lie in different UTC hours", e.TSMin, e.TSMax)
	case e.EpochMin > e.EpochMax:
		return fmt.Errorf("F-2 epoch_min %d above epoch_max %d", e.EpochMin, e.EpochMax)
	case e.SummaryLen == 0:
		return errors.New("F-2 summary_len 0")
	case e.SummaryOffset > f3Len || uint64(e.SummaryLen) > f3Len-e.SummaryOffset:
		return fmt.Errorf("F-2 summary at %d of %d bytes outside the %d-byte F-3", e.SummaryOffset, e.SummaryLen, f3Len)
	}

	return nil
}

// parseBlockSummary decodes the F-3 entry list of block i, whose F-2 entry is e,
// validates it against the F-3 registry, types it, and checks it against e.
func parseBlockSummary(i int, list []byte, e *format.F2Entry) (blockSummary, error) {
	entries, err := decodeValidated(list, tlv.F3)
	if err != nil {
		return blockSummary{}, footerErrorf("block %d: F-3: %w", i, err)
	}

	s := blockSummaryOf(e)
	for _, en := range entries {
		if err := s.setF3(en); err != nil {
			return blockSummary{}, footerErrorf("block %d: F-3 %s: %w", i, tlv.F3[en.Tag].Name, err)
		}
	}
	if err := s.checkSummary(); err != nil {
		return blockSummary{}, footerErrorf("block %d: F-3 %w", i, err)
	}

	return s, nil
}

// decodeValidated decodes the TLV entry list b and validates it against reg.
func decodeValidated(b []byte, reg tlv.Registry) ([]tlv.Entry, error) {
	entries, err := tlv.Decode(b)
	if err != nil {
		return nil, err
	}

	return entries, tlv.Validate(entries, reg)
}

// parsePackStats decodes the F-5 entry list f5, validates it against the F-5 registry, and types it.
// Every statistic that is not conditional must be present,
// since F-5 must equal the aggregate of the blocks;
// ts_min and ts_max are present iff the pack has blocks.
func parsePackStats(f5 []byte, hasBlocks bool) (*packStats, error) {
	entries, err := decodeValidated(f5, tlv.F5)
	if err != nil {
		return nil, footerErrorf("F-5: %w", err)
	}

	st := &packStats{}
	var seen [f5TagBoundary + 1]bool
	for _, en := range entries {
		if err := st.setF5(en); err != nil {
			return nil, footerErrorf("F-5 %s: %w", tlv.F5[en.Tag].Name, err)
		}
		if int(en.Tag) < len(seen) {
			seen[en.Tag] = true
		}
	}

	required := [...]uint16{f5TagRecordCount, f5TagKindCounts, f5TagDirCounts, f5TagDecodeStatusCounts, f5TagContentBytes, f5TagQualityUnion}
	for _, tag := range required {
		if !seen[tag] {
			return nil, footerErrorf("F-5 lacks %s", tlv.F5[tag].Name)
		}
	}
	for _, tag := range [...]uint16{f5TagTSMin, f5TagTSMax} {
		if seen[tag] != hasBlocks {
			return nil, footerErrorf("F-5 %s present is %t in a pack whose block presence is %t", tlv.F5[tag].Name, seen[tag], hasBlocks)
		}
	}

	if err := checkUniqueEpochs(st.epochs); err != nil {
		return nil, footerErrorf("F-5 %w", err)
	}

	return st, nil
}

// checkPackStats checks that the F-5 statistics got equal want, the aggregate of the blocks,
// compared by meaning (the tracepack format specification §10 aggregation rule):
// count arrays equal once trailing zeros are dropped, seq ranges exactly equal,
// epoch entries equal as a set keyed by epoch, and boundary entries equal as a multiset.
func checkPackStats(got, want *packStats) error {
	counts := [...]struct {
		name      string
		got, want []uint64
	}{
		{"kind_counts", got.kindCounts, want.kindCounts},
		{"dir_counts", got.dirCounts, want.dirCounts},
		{"decode_status_counts", got.decodeStatusCounts, want.decodeStatusCounts},
	}

	switch {
	case got.recordCount != want.recordCount:
		return footerErrorf("F-5 record_count %d, the blocks hold %d", got.recordCount, want.recordCount)
	case got.tsMin != want.tsMin || got.tsMax != want.tsMax:
		return footerErrorf("F-5 ts_min %d and ts_max %d, the blocks span %d to %d", got.tsMin, got.tsMax, want.tsMin, want.tsMax)
	case got.contentBytes != want.contentBytes:
		return footerErrorf("F-5 content_bytes %d, the blocks hold %d", got.contentBytes, want.contentBytes)
	case got.qualityUnion != want.qualityUnion:
		return footerErrorf("F-5 quality_union 0x%04X, the blocks' union is 0x%04X", uint16(got.qualityUnion), uint16(want.qualityUnion))
	case !slices.Equal(got.seqRanges, want.seqRanges):
		return footerErrorf("F-5 seq_range entries %v, the blocks hold %v", got.seqRanges, want.seqRanges)
	case !equalEpochSets(got.epochs, want.epochs):
		return footerErrorf("F-5 epoch entries differ from the blocks' epochs")
	case !equalBoundarySets(got.boundaries, want.boundaries):
		return footerErrorf("F-5 boundary entries differ from the blocks' boundaries")
	}

	for _, c := range counts {
		if !slices.Equal(trimZeros(c.got), trimZeros(c.want)) {
			return footerErrorf("F-5 %s %v, the blocks' sum is %v", c.name, c.got, c.want)
		}
	}

	return nil
}

// checkTrailerTotals checks the trailer against the validated footer's F-5 statistics st and blocks:
// record_count equals the F-5 record_count, which checkPackStats found equal to the sum of the F-2 record counts,
// and when the pack has blocks, last_seq equals the last block's last_seq.
func checkTrailerTotals(st *packStats, blocks []blockSummary, tr *format.Trailer) error {
	if tr.RecordCount != st.recordCount {
		return footerErrorf("trailer record_count %d, F-5 record_count %d", tr.RecordCount, st.recordCount)
	}
	if n := len(blocks); n > 0 && tr.LastSeq != blocks[n-1].lastSeq {
		return footerErrorf("trailer last_seq %d, the last block's last_seq %d", tr.LastSeq, blocks[n-1].lastSeq)
	}

	return nil
}

// checkCountSum checks that the u32 array counts sums to want, the block's record_count.
// The sum stops as soon as it passes want, so it never overflows.
func checkCountSum(counts []uint32, want uint32) error {
	var sum uint64
	for _, c := range counts {
		sum += uint64(c)
		if sum > uint64(want) {
			return fmt.Errorf("sums past record_count %d", want)
		}
	}
	if sum != uint64(want) {
		return fmt.Errorf("sums to %d, record_count is %d", sum, want)
	}

	return nil
}

// checkUniqueEpochs rejects an epoch listed twice.
func checkUniqueEpochs(epochs []epochSummary) error {
	sorted := slices.SortedFunc(slices.Values(epochs), compareEpochs)
	for i := 1; i < len(sorted); i++ {
		if sorted[i].epoch == sorted[i-1].epoch {
			return fmt.Errorf("epoch %d listed twice", sorted[i].epoch)
		}
	}

	return nil
}

// equalEpochSets reports whether got and want hold the same epoch entries, in any order;
// got's epochs are unique, which parsePackStats checked,
// and aggregate makes want's unique and sorts them by epoch.
func equalEpochSets(got, want []epochSummary) bool {
	return slices.Equal(slices.SortedFunc(slices.Values(got), compareEpochs), want)
}

// equalBoundarySets reports whether got and want hold the same boundary entries, each as often, in any order.
func equalBoundarySets(got, want []boundarySummary) bool {
	if len(got) != len(want) {
		return false
	}

	a := slices.SortedFunc(slices.Values(got), compareBoundaries)
	b := slices.SortedFunc(slices.Values(want), compareBoundaries)

	return slices.EqualFunc(a, b, func(x, y boundarySummary) bool { return compareBoundaries(x, y) == 0 })
}

// compareBoundaries orders boundary entries by every field:
// seq, boundary_kind, ts, epoch, then gap_start and gap_end, an absent bound before any present one.
func compareBoundaries(a, b boundarySummary) int {
	return cmp.Or(
		cmp.Compare(a.seq, b.seq),
		cmp.Compare(a.kind, b.kind),
		cmp.Compare(a.ts, b.ts),
		cmp.Compare(a.epoch, b.epoch),
		compareOptional(a.gapStart, b.gapStart),
		compareOptional(a.gapEnd, b.gapEnd),
	)
}

// compareOptional orders two optional values, an absent value before any present one.
func compareOptional(a, b *int64) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return -1
	case b == nil:
		return 1
	default:
		return cmp.Compare(*a, *b)
	}
}

// trimZeros returns counts without its trailing zero elements, which a reader treats as absent.
func trimZeros[T uint32 | uint64](counts []T) []T {
	n := len(counts)
	for n > 0 && counts[n-1] == 0 {
		n--
	}

	return counts[:n]
}

// epochNumberOf returns the value of a u64 entry holding an epoch, which a record stores as a u32.
func epochNumberOf(e tlv.Entry) (uint32, error) {
	v, err := e.U64()
	if err != nil {
		return 0, err
	}
	if v > math.MaxUint32 {
		return 0, fmt.Errorf("epoch %d exceeds 2^32-1", v)
	}

	return uint32(v), nil
}

// epochOf types a nested epoch entry of F-3 or F-5 that tlv.Validate accepted, skipping unknown nested tags.
func epochOf(e tlv.Entry) (epochSummary, error) {
	nested, err := e.Nested()
	if err != nil {
		return epochSummary{}, err
	}

	var out epochSummary
	for _, n := range nested {
		switch n.Tag {
		case epochTagEpoch:
			out.epoch, err = epochNumberOf(n)
		case epochTagRecordCount:
			out.recordCount, err = n.U64()
		case epochTagSeqFirst:
			out.seqFirst, err = n.U64()
		case epochTagSeqLast:
			out.seqLast, err = n.U64()
		case epochTagTSMin:
			out.tsMin, err = n.I64()
		case epochTagTSMax:
			out.tsMax, err = n.I64()
		case epochTagCloseSeq:
			out.closeSeq, err = n.U64()
			out.hasCloseSeq = true
		default:
			// An unknown nested tag is skipped.
		}
		if err != nil {
			return epochSummary{}, err
		}
	}

	return out, nil
}

// boundaryOf types a nested boundary entry of F-3 or F-5 that tlv.Validate accepted, skipping unknown nested tags.
func boundaryOf(e tlv.Entry) (boundarySummary, error) {
	nested, err := e.Nested()
	if err != nil {
		return boundarySummary{}, err
	}

	var out boundarySummary
	for _, n := range nested {
		var kind uint8
		switch n.Tag {
		case boundaryTagSeq:
			out.seq, err = n.U64()
		case boundaryTagKind:
			kind, err = n.U8()
			out.kind = BoundaryKind(kind)
		case boundaryTagTS:
			out.ts, err = n.I64()
		case boundaryTagEpoch:
			out.epoch, err = epochNumberOf(n)
		case boundaryTagGapStart:
			out.gapStart, err = optionalI64(n)
		case boundaryTagGapEnd:
			out.gapEnd, err = optionalI64(n)
		default:
			// An unknown nested tag is skipped.
		}
		if err != nil {
			return boundarySummary{}, err
		}
	}

	return out, nil
}

// qualityUnionOf returns the value of a quality_union entry, which holds only the 16 quality bits.
func qualityUnionOf(e tlv.Entry) (Quality, error) {
	v, err := e.U64()
	if err != nil {
		return 0, err
	}
	if v > maxQualityUnion {
		return 0, fmt.Errorf("0x%X sets bits above the 16 quality bits", v)
	}

	return Quality(v), nil
}

// optionalI64 returns the value of an i64 entry as a present optional value.
func optionalI64(e tlv.Entry) (*int64, error) {
	v, err := e.I64()
	if err != nil {
		return nil, err
	}

	return &v, nil
}

// seqRangeOf returns the value of a seq_range entry.
func seqRangeOf(e tlv.Entry) (seqRange, error) {
	r, err := tlv.DecodeSeqRange(e.Value)
	if err != nil {
		return seqRange{}, err
	}

	return seqRange{first: r.First, last: r.Last}, nil
}

// footerErrorf returns an error wrapping ErrInvalidFooter whose message describes the failed check;
// msg may wrap a further error with %w.
func footerErrorf(msg string, args ...any) error {
	return fmt.Errorf("%w: %w", ErrInvalidFooter, fmt.Errorf(msg, args...))
}

// retainF3 makes the index keep a copy of f3, the validated F-3 section,
// and each block's span in it, taken from entries, the blocks' F-2 entries.
// F-3 is copied, not sliced, so the index neither keeps the decoded footer alive nor changes when that buffer does.
func (idx *footerIndex) retainF3(f3 []byte, entries []format.F2Entry) {
	idx.f3 = bytes.Clone(f3)
	idx.f3Spans = make([]f3Span, len(entries))
	for i := range entries {
		idx.f3Spans[i] = f3Span{offset: entries[i].SummaryOffset, n: entries[i].SummaryLen}
	}
}

// f3List returns block i's F-3 entry list exactly as the footer stores it,
// retired, reserved and unknown tags, entry order and trailing zero count elements included.
// The list aliases the index's F-3 section, which is never modified, and the caller must not modify it;
// its capacity ends where the list does, so an append copies it instead of writing into the section.
// The index must keep F-3: parseFooter's keepF3 was set.
func (idx *footerIndex) f3List(i int) []byte {
	sp := idx.f3Spans[i]
	// parseFooter checked every span against F-3, so end lies inside f3.
	end := sp.offset + uint64(sp.n)

	return idx.f3[sp.offset:end:end]
}

// setF3 folds one entry of the block's F-3 list into s, checking what a single entry can show;
// unknown tags, the retired and reserved tags included, are skipped.
func (s *blockSummary) setF3(e tlv.Entry) error {
	var err error

	switch e.Tag {
	case f3TagKindCounts:
		s.kindCounts, err = tlv.DecodeU32Array(e.Value)
	case f3TagDirCounts:
		s.dirCounts, err = tlv.DecodeU32Array(e.Value)
	case f3TagDecodeStatusCounts:
		s.decodeStatusCounts, err = tlv.DecodeU32Array(e.Value)
	case f3TagQualityUnion:
		s.qualityUnion, err = qualityUnionOf(e)
	case f3TagEpoch:
		var ep epochSummary
		ep, err = epochOf(e)
		s.epochs = append(s.epochs, ep)
	case f3TagBoundary:
		var b boundarySummary
		b, err = boundaryOf(e)
		s.boundaries = append(s.boundaries, b)
	case f3TagSeqRange:
		var r seqRange
		r, err = seqRangeOf(e)
		s.seqRanges = append(s.seqRanges, r)
	default:
		// An unknown tag, a reserved index tag included, is skipped.
	}

	return err
}

// checkSummary checks the typed F-3 summary s against its F-2 fields:
// the count arrays, the seq set, the epoch entries, and the boundary seqs.
// A block without seq_range entries gets its one contiguous range, first_seq to last_seq.
func (s *blockSummary) checkSummary() error {
	counts := [...]struct {
		name   string
		counts []uint32
	}{
		{"kind_counts", s.kindCounts},
		{"dir_counts", s.dirCounts},
		{"decode_status_counts", s.decodeStatusCounts},
	}
	for _, c := range counts {
		if err := checkCountSum(c.counts, s.recordCount); err != nil {
			return fmt.Errorf("%s %w", c.name, err)
		}
	}

	if err := s.checkSeqSet(); err != nil {
		return err
	}
	if err := s.checkEpochs(); err != nil {
		return err
	}
	for _, b := range s.boundaries {
		if !s.inSeqSet(b.seq) {
			return fmt.Errorf("boundary seq %d is not a seq of the block", b.seq)
		}
	}

	return nil
}

// checkSeqSet checks that the block's seq set holds exactly record_count seqs from first_seq to last_seq.
// seq_range entries must be sorted, non-overlapping and maximal, so no two touch;
// without them the seqs are first_seq to last_seq, which checkSeqSet records as the block's one range.
// Every range's member count is at most 2^63, and the running total stops as soon as it passes record_count,
// so the sum never overflows.
func (s *blockSummary) checkSeqSet() error {
	if len(s.seqRanges) == 0 {
		// first_seq ≤ last_seq ≤ 2^63-1, so the span cannot overflow.
		if span := s.lastSeq - s.firstSeq + 1; span != uint64(s.recordCount) {
			return fmt.Errorf("without seq_range entries seqs %d to %d are %d records, record_count is %d",
				s.firstSeq, s.lastSeq, span, s.recordCount)
		}
		s.seqRanges = []seqRange{{first: s.firstSeq, last: s.lastSeq}}

		return nil
	}

	var members uint64
	for j, r := range s.seqRanges {
		switch {
		case r.first > r.last:
			return fmt.Errorf("seq_range %d runs from %d back to %d", j, r.first, r.last)
		case j == 0 && r.first != s.firstSeq:
			return fmt.Errorf("seq_range 0 starts at %d, first_seq is %d", r.first, s.firstSeq)
		case j > 0 && r.first <= s.seqRanges[j-1].last+1:
			return fmt.Errorf("seq_range %d starts at %d, overlapping or touching the previous one ending at %d",
				j, r.first, s.seqRanges[j-1].last)
		}

		members += r.last - r.first + 1
		if members > uint64(s.recordCount) {
			return fmt.Errorf("seq_range entries hold more than record_count %d seqs", s.recordCount)
		}
	}

	if last := s.seqRanges[len(s.seqRanges)-1].last; last != s.lastSeq {
		return fmt.Errorf("seq_range entries end at %d, last_seq is %d", last, s.lastSeq)
	}
	if members != uint64(s.recordCount) {
		return fmt.Errorf("seq_range entries hold %d seqs, record_count is %d", members, s.recordCount)
	}

	return nil
}

// checkEpochs checks the block's epoch entries:
// unique epochs within epoch_min to epoch_max, each with at least one record,
// seq and time bounds inside the block's, a close_seq in the block's seq set,
// and record counts summing to record_count.
// The running total stops as soon as it passes record_count, so the sum never overflows.
func (s *blockSummary) checkEpochs() error {
	if err := checkUniqueEpochs(s.epochs); err != nil {
		return err
	}

	var total uint64
	for _, e := range s.epochs {
		switch {
		case e.epoch < s.epochMin || e.epoch > s.epochMax:
			return fmt.Errorf("epoch %d outside epoch_min %d to epoch_max %d", e.epoch, s.epochMin, s.epochMax)
		case e.recordCount == 0:
			return fmt.Errorf("epoch %d record_count 0", e.epoch)
		case e.seqFirst > e.seqLast || e.seqFirst < s.firstSeq || e.seqLast > s.lastSeq:
			return fmt.Errorf("epoch %d seqs %d to %d outside the block's %d to %d", e.epoch, e.seqFirst, e.seqLast, s.firstSeq, s.lastSeq)
		case e.tsMin > e.tsMax || e.tsMin < s.tsMin || e.tsMax > s.tsMax:
			return fmt.Errorf("epoch %d times %d to %d outside the block's %d to %d", e.epoch, e.tsMin, e.tsMax, s.tsMin, s.tsMax)
		case e.hasCloseSeq && !s.inSeqSet(e.closeSeq):
			return fmt.Errorf("epoch %d close_seq %d is not a seq of the block", e.epoch, e.closeSeq)
		}

		total += e.recordCount
		if total > uint64(s.recordCount) {
			return fmt.Errorf("epoch record counts sum past record_count %d", s.recordCount)
		}
	}

	if total != uint64(s.recordCount) {
		return fmt.Errorf("epoch record counts sum to %d, record_count is %d", total, s.recordCount)
	}

	return nil
}

// inSeqSet reports whether seq is one of the block's seqs; checkSeqSet has made seqRanges sorted and disjoint.
func (s *blockSummary) inSeqSet(seq uint64) bool {
	_, found := slices.BinarySearchFunc(s.seqRanges, seq, func(r seqRange, v uint64) int {
		switch {
		case r.last < v:
			return -1
		case r.first > v:
			return 1
		default:
			return 0
		}
	})

	return found
}

// setF5 folds one entry of the F-5 list into st, checking what a single entry can show;
// unknown tags are skipped.
func (st *packStats) setF5(e tlv.Entry) error {
	var err error

	switch e.Tag {
	case f5TagRecordCount:
		st.recordCount, err = e.U64()
	case f5TagKindCounts:
		st.kindCounts, err = tlv.DecodeU64Array(e.Value)
	case f5TagDirCounts:
		st.dirCounts, err = tlv.DecodeU64Array(e.Value)
	case f5TagDecodeStatusCounts:
		st.decodeStatusCounts, err = tlv.DecodeU64Array(e.Value)
	case f5TagTSMin:
		st.tsMin, err = e.I64()
	case f5TagTSMax:
		st.tsMax, err = e.I64()
	case f5TagContentBytes:
		st.contentBytes, err = e.U64()
	case f5TagQualityUnion:
		st.qualityUnion, err = qualityUnionOf(e)
	case f5TagSeqRange:
		var r seqRange
		r, err = seqRangeOf(e)
		st.seqRanges = append(st.seqRanges, r)
	case f5TagEpoch:
		var ep epochSummary
		ep, err = epochOf(e)
		st.epochs = append(st.epochs, ep)
	case f5TagBoundary:
		var b boundarySummary
		b, err = boundaryOf(e)
		st.boundaries = append(st.boundaries, b)
	default:
		// An unknown tag is skipped.
	}

	return err
}
