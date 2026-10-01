package tracepack

import (
	"cmp"
	"fmt"
	"math"
	"slices"

	"github.com/arloliu/go-secs/tracepack/internal/format"
	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// Tags of an F-3 block summary (the tracepack format specification §10).
const (
	f3TagKindCounts         uint16 = 0x0001
	f3TagDirCounts          uint16 = 0x0002
	f3TagDecodeStatusCounts uint16 = 0x0003
	f3TagQualityUnion       uint16 = 0x0005
	f3TagEpoch              uint16 = 0x0007
	f3TagBoundary           uint16 = 0x0008
	f3TagSeqRange           uint16 = 0x0009
)

// Tags of the F-5 pack statistics (the tracepack format specification §10).
const (
	f5TagRecordCount        uint16 = 0x0001
	f5TagKindCounts         uint16 = 0x0002
	f5TagDirCounts          uint16 = 0x0003
	f5TagDecodeStatusCounts uint16 = 0x0004
	f5TagTSMin              uint16 = 0x0005
	f5TagTSMax              uint16 = 0x0006
	f5TagContentBytes       uint16 = 0x0007
	f5TagQualityUnion       uint16 = 0x0008
	f5TagSeqRange           uint16 = 0x0009
	f5TagEpoch              uint16 = 0x000A
	f5TagBoundary           uint16 = 0x000B
)

// Tags nested in an epoch entry of F-3 or F-5 (the tracepack format specification §10).
const (
	epochTagEpoch       uint16 = 0x0001
	epochTagRecordCount uint16 = 0x0002
	epochTagSeqFirst    uint16 = 0x0003
	epochTagSeqLast     uint16 = 0x0004
	epochTagTSMin       uint16 = 0x0005
	epochTagTSMax       uint16 = 0x0006
	epochTagCloseSeq    uint16 = 0x0007
)

// Tags nested in a boundary entry of F-3 or F-5 (the tracepack format specification §10).
const (
	boundaryTagSeq      uint16 = 0x0001
	boundaryTagKind     uint16 = 0x0002
	boundaryTagTS       uint16 = 0x0003
	boundaryTagEpoch    uint16 = 0x0004
	boundaryTagGapStart uint16 = 0x0005
	boundaryTagGapEnd   uint16 = 0x0006
)

// Estimates, in bytes on a 64-bit platform, of the memory a Writer holds for each block until Close,
// counted against its footer budget (footerCost):
// the blockSummary it keeps, the format.F2Entry buildFooter builds for the block at Close,
// and per entry of the summary's variable contents an epoch entry, a boundary entry with its two gap values,
// a seq range, and one element of a count array.
// TestFooterCostEstimates checks each against the size of its type.
const (
	keptSummaryLen  = 256
	keptF2EntryLen  = 80
	keptEpochLen    = 64
	keptBoundaryLen = 64
	keptSeqRangeLen = 16
	keptCountLen    = 4
)

// packStats is the F-5 pack statistics:
// the aggregate of the blocks' F-2 entries and F-3 summaries by the aggregation rule of the tracepack format specification §10.
//
// A pack has fewer than 2^32 blocks, and each block contributes fewer than 2^31 records and content bytes,
// so every sum stays below (2^32-1)×(2^31-1), under the 2^63-1 limit of a u64 value, without overflow checks.
type packStats struct {
	recordCount        uint64
	kindCounts         []uint64
	dirCounts          []uint64
	decodeStatusCounts []uint64
	// tsMin and tsMax are valid iff the pack has a block.
	tsMin        int64
	tsMax        int64
	contentBytes uint64
	qualityUnion Quality
	seqRanges    []seqRange
	// epochs holds one entry per epoch, in ascending epoch order.
	epochs []epochSummary
	// boundaries holds every block's boundary entries, in block order.
	boundaries []boundarySummary
}

// buildFooter returns the decoded footer of a pack whose written blocks are blocks, in file order,
// with the pack statistics it aggregated (the tracepack format specification §10).
//
// The decoded footer is the F-1 prologue, F-2 at offset 72, F-3 right after F-2, and F-5 right after F-3;
// F-4 is absent, recorded as zero bytes at the end of F-3, so the sections stay in order without overlap,
// and the F-1 flags set only F-3 present, since this writer builds no secondary index.
// A block whose summary holds a verbatim F-3 list gets that list byte for byte;
// every other block gets the list appendF3 builds.
// F-5 is computed from the F-2 and F-3 values alone, those of a block with a verbatim list taken from its summary.
// A block's F-3 entry list longer than maxF3ListLen, at most 2^32-1 so that F-2 summary_len can state it, is an error.
// When maxLen is not 0, a footer longer than maxLen is an error wrapping ErrMergeLimit,
// returned once F-5 is built, before the footer is assembled.
func buildFooter(blocks []blockSummary, maxF3ListLen, maxLen uint64) ([]byte, *packStats, error) {
	var f3 []byte
	index := make([]format.F2Entry, len(blocks))
	for i := range blocks {
		start := len(f3)
		if l := blocks[i].verbatimF3; len(l) > 0 {
			f3 = append(f3, l...)
		} else {
			f3 = appendF3(f3, &blocks[i])
		}
		n := uint64(len(f3) - start)
		summaryLen, ok := summaryLen32(n, maxF3ListLen)
		if !ok {
			return nil, nil, fmt.Errorf("tracepack: block %d: F-3 entry list of %d bytes exceeds the u32 summary_len", i, n)
		}
		index[i] = f2EntryOf(&blocks[i], uint64(start), summaryLen)
	}

	stats := aggregate(blocks)
	f5 := appendF5(nil, stats, len(blocks) > 0)

	f3Offset := uint64(format.FooterPrologueLen) + uint64(len(blocks))*format.F2EntryLen
	f5Offset := f3Offset + uint64(len(f3))
	if n := f5Offset + uint64(len(f5)); maxLen != 0 && n > maxLen {
		return nil, nil, fmt.Errorf("tracepack: footer of %d bytes exceeds the budget of %d: %w", n, maxLen, ErrMergeLimit)
	}

	pro := format.FooterPrologue{
		FooterLayoutVersion: format.FooterLayoutVersion,
		Flags:               format.FooterFlagF3Present,
		BlockCount:          uint32(len(blocks)),
		F2EntryLen:          format.F2EntryLen,
		F2Offset:            format.FooterPrologueLen,
		F3Offset:            f3Offset,
		F3Len:               uint64(len(f3)),
		F4Offset:            f5Offset,
		F5Offset:            f5Offset,
		F5Len:               uint64(len(f5)),
	}

	out := make([]byte, 0, int(f5Offset)+len(f5))
	out = format.AppendFooterPrologue(out, &pro)
	for i := range index {
		out = format.AppendF2Entry(out, &index[i])
	}
	out = append(out, f3...)

	return append(out, f5...), stats, nil
}

// summaryLen32 returns n, the length of a block's F-3 entry list, as the F-2 summary_len,
// or false when n exceeds limit or 2^32-1, the most summary_len can state.
func summaryLen32(n, limit uint64) (uint32, bool) {
	if n > min(limit, math.MaxUint32) {
		return 0, false
	}

	return uint32(n), true
}

// footerCost returns what a block adds to a Writer's footer budget,
// for the block of s with the verbatim F-3 list f3, or none when f3 is empty.
// It is the sum of the bytes the block adds to the decoded footer:
// its 80-byte F-2 entry and its F-3 list, f3 or the list appendF3 builds from s;
// and of the estimated memory the Writer holds for the block until Close, at the kept*Len estimates:
// the blockSummary it keeps, the summary's epoch, boundary, seq-range and count entries,
// its copy of f3, and the format.F2Entry buildFooter builds for it.
// A summary of fewer than 2^32 records holds fewer than 2^32 of each entry and at most 3 × 256 count elements,
// so the cost stays far below 2^64.
func footerCost(s *blockSummary, f3 []byte) uint64 {
	listLen := uint64(len(f3))
	if listLen == 0 {
		listLen = uint64(len(appendF3(nil, s)))
	}
	kept := keptSummaryLen + keptF2EntryLen + uint64(len(f3)) +
		uint64(len(s.epochs))*keptEpochLen + uint64(len(s.boundaries))*keptBoundaryLen +
		uint64(len(s.seqRanges))*keptSeqRangeLen +
		uint64(len(s.kindCounts)+len(s.dirCounts)+len(s.decodeStatusCounts))*keptCountLen

	return format.F2EntryLen + listLen + kept
}

// f2EntryOf returns the F-2 block index entry of s, whose F-3 list lies at summaryOffset in F-3 and is summaryLen bytes long.
func f2EntryOf(s *blockSummary, summaryOffset uint64, summaryLen uint32) format.F2Entry {
	return format.F2Entry{
		Offset:          s.offset,
		OnDiskLen:       s.onDiskLen,
		UncompressedLen: s.uncompressedLen,
		RecordCount:     s.recordCount,
		BodyCRC:         s.bodyCRC,
		FirstSeq:        s.firstSeq,
		LastSeq:         s.lastSeq,
		TSMin:           s.tsMin,
		TSMax:           s.tsMax,
		EpochMin:        s.epochMin,
		EpochMax:        s.epochMax,
		SummaryOffset:   summaryOffset,
		SummaryLen:      summaryLen,
		RecordHeaderLen: s.recordHeaderLen,
	}
}

// blockSummaryOf returns the blockSummary holding the block index fields of the F-2 entry e,
// the inverse of f2EntryOf; its F-3 fields are left empty.
func blockSummaryOf(e *format.F2Entry) blockSummary {
	return blockSummary{
		offset:          e.Offset,
		onDiskLen:       e.OnDiskLen,
		uncompressedLen: e.UncompressedLen,
		recordCount:     e.RecordCount,
		bodyCRC:         e.BodyCRC,
		recordHeaderLen: e.RecordHeaderLen,
		firstSeq:        e.FirstSeq,
		lastSeq:         e.LastSeq,
		tsMin:           e.TSMin,
		tsMax:           e.TSMax,
		epochMin:        e.EpochMin,
		epochMax:        e.EpochMax,
	}
}

// appendF3 appends the F-3 entry list of s to dst, in tag order:
// the count arrays, quality_union,
// one epoch entry per epoch in ascending epoch order, one boundary entry per capture-boundary record,
// and seq_range entries only when the block's seqs are not contiguous.
func appendF3(dst []byte, s *blockSummary) []byte {
	dst = tlv.AppendEntry(dst, tlv.BytesEntry(f3TagKindCounts, tlv.AppendU32Array(nil, s.kindCounts)))
	dst = tlv.AppendEntry(dst, tlv.BytesEntry(f3TagDirCounts, tlv.AppendU32Array(nil, s.dirCounts)))
	dst = tlv.AppendEntry(dst, tlv.BytesEntry(f3TagDecodeStatusCounts, tlv.AppendU32Array(nil, s.decodeStatusCounts)))
	dst = tlv.AppendEntry(dst, tlv.U64Entry(f3TagQualityUnion, uint64(s.qualityUnion)))

	epochs := slices.SortedFunc(slices.Values(s.epochs), compareEpochs)
	for i := range epochs {
		dst = tlv.AppendEntry(dst, epochEntry(f3TagEpoch, &epochs[i]))
	}
	for i := range s.boundaries {
		dst = tlv.AppendEntry(dst, boundaryEntry(f3TagBoundary, &s.boundaries[i]))
	}
	if len(s.seqRanges) > 1 {
		dst = appendSeqRanges(dst, f3TagSeqRange, s.seqRanges)
	}

	return dst
}

// appendF5 appends the F-5 entry list of st to dst, in tag order.
// A pack without blocks has no ts_min, ts_max, seq_range, epoch or boundary entry.
func appendF5(dst []byte, st *packStats, hasBlocks bool) []byte {
	dst = tlv.AppendEntry(dst, tlv.U64Entry(f5TagRecordCount, st.recordCount))
	dst = tlv.AppendEntry(dst, tlv.BytesEntry(f5TagKindCounts, tlv.AppendU64Array(nil, st.kindCounts)))
	dst = tlv.AppendEntry(dst, tlv.BytesEntry(f5TagDirCounts, tlv.AppendU64Array(nil, st.dirCounts)))
	dst = tlv.AppendEntry(dst, tlv.BytesEntry(f5TagDecodeStatusCounts, tlv.AppendU64Array(nil, st.decodeStatusCounts)))
	if hasBlocks {
		dst = tlv.AppendEntry(dst, tlv.I64Entry(f5TagTSMin, st.tsMin))
		dst = tlv.AppendEntry(dst, tlv.I64Entry(f5TagTSMax, st.tsMax))
	}
	dst = tlv.AppendEntry(dst, tlv.U64Entry(f5TagContentBytes, st.contentBytes))
	dst = tlv.AppendEntry(dst, tlv.U64Entry(f5TagQualityUnion, uint64(st.qualityUnion)))
	dst = appendSeqRanges(dst, f5TagSeqRange, st.seqRanges)
	for i := range st.epochs {
		dst = tlv.AppendEntry(dst, epochEntry(f5TagEpoch, &st.epochs[i]))
	}
	for i := range st.boundaries {
		dst = tlv.AppendEntry(dst, boundaryEntry(f5TagBoundary, &st.boundaries[i]))
	}

	return dst
}

// appendSeqRanges appends one seq_range entry with tag per range.
func appendSeqRanges(dst []byte, tag uint16, ranges []seqRange) []byte {
	for _, r := range ranges {
		dst = tlv.AppendEntry(dst, tlv.BytesEntry(tag, tlv.AppendSeqRange(nil, tlv.SeqRange{First: r.first, Last: r.last})))
	}

	return dst
}

// epochEntry returns the nested epoch entry of e with tag; close_seq is present iff e.hasCloseSeq.
func epochEntry(tag uint16, e *epochSummary) tlv.Entry {
	entries := []tlv.Entry{
		tlv.U64Entry(epochTagEpoch, uint64(e.epoch)),
		tlv.U64Entry(epochTagRecordCount, e.recordCount),
		tlv.U64Entry(epochTagSeqFirst, e.seqFirst),
		tlv.U64Entry(epochTagSeqLast, e.seqLast),
		tlv.I64Entry(epochTagTSMin, e.tsMin),
		tlv.I64Entry(epochTagTSMax, e.tsMax),
	}
	if e.hasCloseSeq {
		entries = append(entries, tlv.U64Entry(epochTagCloseSeq, e.closeSeq))
	}

	return tlv.NestedEntry(tag, entries)
}

// boundaryEntry returns the nested boundary entry of b with tag;
// gap_start and gap_end are present as in the record's payload.
func boundaryEntry(tag uint16, b *boundarySummary) tlv.Entry {
	entries := []tlv.Entry{
		tlv.U64Entry(boundaryTagSeq, b.seq),
		tlv.U8Entry(boundaryTagKind, uint8(b.kind)),
		tlv.I64Entry(boundaryTagTS, b.ts),
		tlv.U64Entry(boundaryTagEpoch, uint64(b.epoch)),
	}
	if b.gapStart != nil {
		entries = append(entries, tlv.I64Entry(boundaryTagGapStart, *b.gapStart))
	}
	if b.gapEnd != nil {
		entries = append(entries, tlv.I64Entry(boundaryTagGapEnd, *b.gapEnd))
	}

	return tlv.NestedEntry(tag, entries)
}

// aggregate computes the F-5 pack statistics from the blocks' F-2 and F-3 values by the aggregation rule of the tracepack format specification §10:
// counts and content_bytes summed, ts extremes taken, seq ranges unioned and coalesced,
// epoch entries combined per epoch keeping the minimum close_seq, and boundary entries unioned in block order.
func aggregate(blocks []blockSummary) *packStats {
	st := &packStats{}
	epochIndex := make(map[uint32]int)

	for i := range blocks {
		b := &blocks[i]
		if i == 0 {
			st.tsMin, st.tsMax = b.tsMin, b.tsMax
		}

		st.recordCount += uint64(b.recordCount)
		st.kindCounts = addCounts(st.kindCounts, b.kindCounts)
		st.dirCounts = addCounts(st.dirCounts, b.dirCounts)
		st.decodeStatusCounts = addCounts(st.decodeStatusCounts, b.decodeStatusCounts)
		st.tsMin, st.tsMax = min(st.tsMin, b.tsMin), max(st.tsMax, b.tsMax)
		st.contentBytes += uint64(b.uncompressedLen)
		st.qualityUnion |= b.qualityUnion
		st.seqRanges = unionSeqRanges(st.seqRanges, b.seqRanges)
		for j := range b.epochs {
			st.epochs = mergeEpoch(st.epochs, epochIndex, &b.epochs[j])
		}
		st.boundaries = append(st.boundaries, b.boundaries...)
	}
	slices.SortFunc(st.epochs, compareEpochs)

	return st
}

// addCounts adds the per-block counts add to the pack counts sum, element by element, extending sum as needed.
func addCounts(sum []uint64, add []uint32) []uint64 {
	if len(add) > len(sum) {
		sum = append(sum, make([]uint64, len(add)-len(sum))...)
	}
	for i, v := range add {
		sum[i] += uint64(v)
	}

	return sum
}

// unionSeqRanges appends a block's seq ranges to the pack's, coalescing ranges that touch.
// Blocks are in ascending seq order (I-12), so every range of add lies above every range of ranges.
func unionSeqRanges(ranges, add []seqRange) []seqRange {
	for _, r := range add {
		if n := len(ranges); n > 0 && ranges[n-1].last+1 == r.first {
			ranges[n-1].last = r.last
			continue
		}
		ranges = append(ranges, r)
	}

	return ranges
}

// mergeEpoch combines e into the pack entry of its epoch, whose index in epochs is index[e.epoch],
// or appends it as a new entry.
// Blocks arrive in ascending seq order, so the first close_seq present is the minimum, and it is kept.
func mergeEpoch(epochs []epochSummary, index map[uint32]int, e *epochSummary) []epochSummary {
	i, ok := index[e.epoch]
	if !ok {
		index[e.epoch] = len(epochs)
		return append(epochs, *e)
	}

	m := &epochs[i]
	m.recordCount += e.recordCount
	m.seqFirst, m.seqLast = min(m.seqFirst, e.seqFirst), max(m.seqLast, e.seqLast)
	m.tsMin, m.tsMax = min(m.tsMin, e.tsMin), max(m.tsMax, e.tsMax)
	if e.hasCloseSeq && !m.hasCloseSeq {
		m.closeSeq, m.hasCloseSeq = e.closeSeq, true
	}

	return epochs
}

// compareEpochs orders epoch entries by epoch number.
func compareEpochs(a, b epochSummary) int {
	return cmp.Compare(a.epoch, b.epoch)
}
