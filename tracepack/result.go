package tracepack

import "fmt"

// Reasons a read is incomplete, the "incomplete" status of the tracepack format specification §13.
const (
	// ReasonCorruptBlock reports a block that failed an integrity check:
	// a CRC, its envelope against its F-2 entry, or I-2.
	ReasonCorruptBlock IncompleteReason = iota + 1
	// ReasonUnknownCodec reports a block whose codec is outside the registry of §2.
	ReasonUnknownCodec
	// ReasonTruncated reports a pack that is not finalized, so more records may follow,
	// or a forward walk that could not account for the pack's block region.
	ReasonTruncated
	// ReasonCoverage reports a coverage entry of the pack metadata that intersects the query (§5).
	ReasonCoverage
	// ReasonLimit reports a block larger than ReaderOptions.MaxBlockLen, which was not read,
	// or a forward walk that stopped at ReaderOptions.MaxWalkedBlocks, past which no block was read.
	ReasonLimit
	// ReasonIndexMismatch reports a block whose records disagree with its F-2 entry
	// (the tracepack format specification §10): its last seq, time range or epoch range.
	// The block's records are still read, because its envelope, body CRC and I-2 checks passed;
	// the read is incomplete, because the same index pruned the blocks it did not read.
	ReasonIndexMismatch
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
	// Coverage is the coverage entry that intersects the query; set for ReasonCoverage only.
	Coverage *Coverage
	// Err describes the defect.
	Err error
}

// Result is the status of a read, reported beside the records it yielded.
type Result struct {
	// Incomplete lists every cause that made the read incomplete, in the order they were found;
	// empty when the read is complete.
	Incomplete []Defect
	// Conflicts lists every conflict of every cluster a read over several packs compared, in discovery order
	// (the tracepack semantics specification §7.4), whether or not the filter selected a version of its record.
	// It is always empty for Reader.Iterate, since a pack's seqs strictly increase.
	Conflicts []Conflict
	// FooterErr is Reader.Iterate's PackHeader.FooterErr:
	// non-nil when the footer was not used and the blocks came from the forward walk;
	// nil for MergeIterate, which reports footer errors in FooterErrs.
	// It is independent of Incomplete:
	// a finalized pack whose walk accounts for every block and record is complete without its footer.
	FooterErr error
	// FooterErrs holds one entry for each pack whose footer was not used, its PackHeader.FooterErr,
	// in the order the readers were given;
	// nil when there is none.
	// For Reader.Iterate it holds FooterErr as the entry of pack 0 when FooterErr is non-nil.
	// Like FooterErr, it is independent of Incomplete.
	FooterErrs []PackError
}

// String returns the reason's name, or "unknown(<n>)" for a value this package does not define.
func (r IncompleteReason) String() string {
	switch r {
	case ReasonCorruptBlock:
		return "corrupt-block"
	case ReasonUnknownCodec:
		return "unknown-codec"
	case ReasonTruncated:
		return "truncated"
	case ReasonCoverage:
		return "coverage"
	case ReasonLimit:
		return "limit"
	case ReasonIndexMismatch:
		return "index-mismatch"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(r))
	}
}

// Complete reports whether the read is complete: whether Incomplete is empty.
// Neither FooterErr nor FooterErrs makes a read incomplete, and Conflicts are reported beside Incomplete, not in it.
func (r Result) Complete() bool {
	return len(r.Incomplete) == 0
}
