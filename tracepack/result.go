package tracepack

import "fmt"

// Reasons a read is incomplete, the "incomplete" status of the tracepack format specification §13.
const (
	// IncompleteReasonCorruptBlock reports a block that failed an integrity check:
	// a CRC, its envelope against its F-2 entry or the forward walk, a body that does not decode, or I-2;
	// for MergeIterate also dimensions its BlockInfo states that cannot be consistent.
	IncompleteReasonCorruptBlock IncompleteReason = iota + 1
	// IncompleteReasonUnknownCodec reports a block whose codec is outside the registry of §2.
	IncompleteReasonUnknownCodec
	// IncompleteReasonTruncated reports a pack that is not finalized, so more records may follow,
	// or a forward walk that could not account for the pack's block region.
	IncompleteReasonTruncated
	// IncompleteReasonCoverage reports a coverage entry of the pack metadata that intersects the query (§5).
	IncompleteReasonCoverage
	// IncompleteReasonLimit reports a block larger than ReaderOptions.MaxBlockLen, which was not read,
	// or a forward walk that stopped at ReaderOptions.MaxWalkedBlocks, past which no block was read.
	IncompleteReasonLimit
	// IncompleteReasonIndexMismatch reports a block whose records disagree with its F-2 entry
	// (the tracepack format specification §10): its last seq, time range or epoch range.
	// The block's records are still read, because its envelope, body CRC and I-2 checks passed;
	// the read is incomplete, because the same index pruned the blocks it did not read.
	IncompleteReasonIndexMismatch
	// IncompleteReasonCrashGap reports a stop-unclean capture boundary whose gap meets the query's time range
	// (the tracepack semantics specification §7.4, Crash gaps):
	// records of the gap may never have been recorded.
	// Defect.Boundary carries the boundary; Block and Offset are -1.
	IncompleteReasonCrashGap
)

// IncompleteReason classifies why a read is incomplete.
//
// The zero value is not a reason; String prints it and any undefined value as "unknown(<n>)".
type IncompleteReason uint8

var _ fmt.Stringer = IncompleteReason(0)

// Defect is one cause of an incomplete read,
// reported beside the records that were read, never instead of them (the tracepack format specification §13).
type Defect struct {
	// Reason classifies the defect.
	Reason IncompleteReason
	// Pack is the index into the readers given to MergeIterate of the pack the defect concerns; 0 for Reader.Iterate.
	Pack int
	// Block is the index into the pack's Reader.Blocks of the block the defect concerns; -1 when it concerns no single block.
	Block int
	// Offset is the file offset where the defect was found:
	// the block envelope's offset for a defect of a block, whose checks mostly have no finer position;
	// -1 when there is none.
	Offset int64
	// Coverage is the coverage entry that intersects the query; set for IncompleteReasonCoverage only.
	Coverage *Coverage
	// Boundary is the stop-unclean boundary whose gap meets the query, the caller's own copy;
	// set for IncompleteReasonCrashGap only, whose Pack is the smallest index of a pack holding the boundary.
	Boundary *Boundary
	// Err describes the defect.
	Err error
}

// Result is the status of a read, reported beside the records it yielded.
type Result struct {
	// Incomplete lists every cause that made the read incomplete, in the order they were found,
	// except the crash gaps of MergeIterate, which it lists together, by Pack, then by seq
	// (MergeIterate states where they stand among the other defects);
	// empty when the read is complete.
	Incomplete []Defect
	// Conflicts lists every conflict of every cluster a read over several packs compared, in discovery order
	// (the tracepack semantics specification §7.4), whether or not the filter selected a version of its record.
	// It is always empty for Reader.Iterate, since a pack's seqs strictly increase.
	Conflicts []Conflict
	// FooterErrs holds one entry for each pack whose footer was not used, its PackHeader.FooterErr,
	// in the order the readers were given;
	// nil when there is none.
	// For Reader.Iterate it holds at most one entry, for pack 0.
	// It is independent of Incomplete:
	// a finalized pack whose walk accounts for every block and record is complete without its footer.
	FooterErrs []PackError
}

// String returns the reason's name, or "unknown(<n>)" for a value this package does not define.
func (r IncompleteReason) String() string {
	switch r {
	case IncompleteReasonCorruptBlock:
		return "corrupt-block"
	case IncompleteReasonUnknownCodec:
		return "unknown-codec"
	case IncompleteReasonTruncated:
		return "truncated"
	case IncompleteReasonCoverage:
		return "coverage"
	case IncompleteReasonLimit:
		return "limit"
	case IncompleteReasonIndexMismatch:
		return "index-mismatch"
	case IncompleteReasonCrashGap:
		return "crash-gap"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(r))
	}
}

// Complete reports whether the read is complete: whether Incomplete is empty.
// FooterErrs never makes a read incomplete, and Conflicts are reported beside Incomplete, not in it.
func (r Result) Complete() bool {
	return len(r.Incomplete) == 0
}
