package tracepack

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"

	"github.com/arloliu/go-secs/tracepack/internal/codec"
	"github.com/arloliu/go-secs/tracepack/internal/format"
	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// defaultMaxOpenBlocks is the MergeOptions.MaxOpenBlocks that zero selects.
const defaultMaxOpenBlocks = 64

// Sentinel errors returned by Merge.
var (
	// ErrMergeInput reports a merge input that Merge refuses (the tracepack storage specification §4 Merge),
	// wrapped with the cause and, for one input, its position and the pack_id view.Packs names there.
	// Merge returns it, before it touches dst, for:
	// a number of inputs other than the number of view.Packs;
	// an input that is not the pack view.Packs names at its position,
	// or that is neither a generation-0 pack listed in view.CompactedFrom nor the member of view.Generation;
	// an input Open fails on, for its size, file header, format version or pack metadata, with Open's error wrapped,
	// an object shorter than its MergeInput.Size included when its ReaderAt reports the end with io.EOF
	// (another error for it, io.ErrUnexpectedEOF included, is a failed read);
	// a pack that is not finalized, or whose footer is not valid;
	// a pack_role that takes no part in a scope's view, a capture other than view.Capture,
	// a period that is empty or not inside view.Hour, a block whose records lie outside that hour,
	// a seq_start other than the first record's seq,
	// records that the input's own pack metadata cannot describe (a classified record without classifier,
	// an oversized record without max_frame_len, or a redacted record),
	// a compaction_level of 255, which leaves the archive no level;
	// and a capture-level tag whose value differs from the first input's.
	// After it wrote the archive's header, as it reads each input block, Merge returns it, naming the pack and the block, for:
	// a block that fails its full read (the tracepack format specification §6: its envelope, body CRC, codec or I-2);
	// a block whose records disagree with its F-2 entry, the ReasonIndexMismatch a read reports beside the block;
	// and a block whose records disagree with its F-3 summary.
	// A failed read of an input, and an input over a reader budget of MergeOptions.Reader, are not refused this way:
	// the error wraps the ReaderAt's error, or ErrReadLimit, naming the input,
	// so a caller retries the read or raises the budget instead of repairing a sound pack.
	// A block read cut short after the input was opened, its ReaderAt reporting io.EOF before the block ends,
	// is such a read error too, wrapping io.ErrUnexpectedEOF:
	// the object changed after Merge opened it, or its ReaderAt is inconsistent, as verification also reports it.
	ErrMergeInput = errors.New("tracepack: merge input rejected")
	// ErrMergeConflict reports that two records of the view have the same capture_id and seq and different bytes
	// (the tracepack format specification I-12), which fails the merge (the tracepack storage specification §4 Merge).
	// Merge returns it only after it compared every block, with MergeReport.ConflictsComplete set
	// and MergeReport.Conflicts the number of conflicts,
	// once MergeOptions.OnConflict has received every conflict.
	ErrMergeConflict = errors.New("tracepack: merge found conflicting records")
	// ErrMergeLimit reports a merge over its budget:
	// the archive's pack metadata over MergeOptions.Reader.MaxPackMetadataLen, which Merge checks before it touches dst;
	// over MergeOptions.MaxFooterLen, the archive's footer, decoded or on disk,
	// or the block summaries a merge keeps for it while it writes;
	// or, over MergeOptions.MaxOpenBlocks, the blocks that the record-by-record resolution of an overlap cluster holds at once,
	// which the error names by its seqs.
	// Merge checks MaxOpenBlocks once the block that would exceed it was read and checked,
	// so a defective block is ErrMergeInput first.
	// It wraps no other error.
	ErrMergeLimit = errors.New("tracepack: merge over its budget")
)

// captureTags lists the capture-level tags of the tracepack storage specification §4 Merge,
// which every input carries with the same value, each with the comparison of two inputs' pack metadata:
// the tag absent in both, or equal decoded values.
// hsms_timers compares timers T1 to T8,
// since a nested timer entry a reader does not know is neither compared nor carried;
// source_ref compares the ordered list, duplicates included,
// because an annotation's source_index is a position in it (the tracepack format specification §8).
var captureTags = [...]struct {
	name  string
	equal func(a, b *PackMeta) bool
}{
	{"tool_id", func(a, b *PackMeta) bool { return a.ToolID == b.ToolID }},
	{"transport", func(a, b *PackMeta) bool { return a.Transport == b.Transport }},
	{"capture_method", func(a, b *PackMeta) bool { return a.CaptureMethod == b.CaptureMethod }},
	{"vantage", func(a, b *PackMeta) bool { return a.Vantage == b.Vantage }},
	{"recorder", func(a, b *PackMeta) bool { return a.Recorder == b.Recorder }},
	{"time_source", func(a, b *PackMeta) bool { return a.TimeSource == b.TimeSource }},
	{"lifecycle_coverage", func(a, b *PackMeta) bool { return a.LifecycleCoverage == b.LifecycleCoverage }},
	{"quality_evaluated", func(a, b *PackMeta) bool { return a.QualityEvaluated == b.QualityEvaluated }},
	{"recorder_instance_id", func(a, b *PackMeta) bool { return a.RecorderInstanceID == b.RecorderInstanceID }},
	{"capture_origin_utc_ns", func(a, b *PackMeta) bool { return equalPtr(a.CaptureOriginUTCNs, b.CaptureOriginUTCNs) }},
	{"capture_origin_mono_ns", func(a, b *PackMeta) bool { return equalPtr(a.CaptureOriginMonoNs, b.CaptureOriginMonoNs) }},
	{"clock_step_tolerance_ns", func(a, b *PackMeta) bool { return equalPtr(a.ClockStepToleranceNs, b.ClockStepToleranceNs) }},
	{"previous_capture_id", func(a, b *PackMeta) bool { return equalPtr(a.PreviousCaptureID, b.PreviousCaptureID) }},
	{"site_id", func(a, b *PackMeta) bool { return equalPtr(a.SiteID, b.SiteID) }},
	{"equipment_model", func(a, b *PackMeta) bool { return equalPtr(a.EquipmentModel, b.EquipmentModel) }},
	{"equipment_sw_rev", func(a, b *PackMeta) bool { return equalPtr(a.EquipmentSWRev, b.EquipmentSWRev) }},
	{"host_software", func(a, b *PackMeta) bool { return equalPtr(a.HostSoftware, b.HostSoftware) }},
	{"host_endpoint", func(a, b *PackMeta) bool { return equalPtr(a.HostEndpoint, b.HostEndpoint) }},
	{"equipment_endpoint", func(a, b *PackMeta) bool { return equalPtr(a.EquipmentEndpoint, b.EquipmentEndpoint) }},
	{"equipment_connect_mode", func(a, b *PackMeta) bool { return equalPtr(a.EquipmentConnectMode, b.EquipmentConnectMode) }},
	{"device_id", func(a, b *PackMeta) bool { return equalPtr(a.DeviceID, b.DeviceID) }},
	{"hsms_timers", func(a, b *PackMeta) bool { return equalTimers(a.HSMSTimers, b.HSMSTimers) }},
	{"source_tz", func(a, b *PackMeta) bool { return equalPtr(a.SourceTZ, b.SourceTZ) }},
	{"source_dialect", func(a, b *PackMeta) bool { return equalPtr(a.SourceDialect, b.SourceDialect) }},
	{"source_ref", func(a, b *PackMeta) bool { return slices.Equal(a.SourceRefs, b.SourceRefs) }},
}

// MergeInput is one pack a merge reads.
type MergeInput struct {
	// ReaderAt reads the pack; Merge may issue several ReadAt calls in parallel.
	// An error it returns, other than io.EOF, is a failed read, which Merge returns naming the input, never as ErrMergeInput.
	// io.EOF before the end of a read is an object shorter than Size while Merge opens the input, which ErrMergeInput refuses,
	// and a failed read, wrapping io.ErrUnexpectedEOF, when it cuts a block read short after that.
	ReaderAt io.ReaderAt
	// Size is the pack's size in bytes.
	Size int64
}

// MergeOptions configures Merge.
type MergeOptions struct {
	// Reader configures the bootstrap and the reader budgets of every input, as for Open.
	// Its FooterOffset hint, which locates one object's footer, applies to every input, so a caller leaves it zero.
	// Its MaxPackMetadataLen, or DefaultMaxPackMetadataLen when it is zero or negative, also bounds the archive's pack metadata,
	// so a reader with the same budget, the next merge included, accepts the archive.
	Reader ReaderOptions
	// Writer is the archive's writer tag, the implementation and version that writes it; required.
	Writer string
	// ScopeGeneration is the generation number the merge claimed for the scope (the tracepack storage specification §2);
	// at least 1.
	ScopeGeneration uint64
	// PublisherEpoch is the fence epoch of the publisher (the tracepack storage specification §2).
	// With ScopeGeneration it is the archive's rank, which must be above the rank of the view's generation, when it has one.
	PublisherEpoch uint64
	// ReplacementSetID identifies the archive's replacement set, a new one for every claim (the tracepack storage specification §4);
	// the zero UUID makes Merge generate a UUIDv7.
	// It must differ from the replacement_set_id of the view's generation.
	ReplacementSetID UUID
	// PackID identifies the archive; the zero UUID makes Merge generate a UUIDv7.
	// It must differ from every pack of the view's Packs and CompactedFrom, and from the member of the view's generation.
	PackID UUID
	// Codec compresses the blocks Merge encodes and the footer; a block copied verbatim keeps its own codec.
	// The zero value is CodecNone.
	Codec Codec
	// BlockThreshold is the decoded block size, in bytes, up to which Merge coalesces blocks and fills the blocks it encodes
	// (the tracepack storage specification §4 Merge); zero means DefaultBlockThreshold.
	// A record whose decoded size alone reaches it is written whole, in a block of its own,
	// and a block whose decoded size alone reaches it is not coalesced.
	BlockThreshold int
	// NoCoalesce makes Merge write every block it would otherwise coalesce as it is, without coalescing small blocks:
	// a block that no other block overlaps copied verbatim, and a block the resolution encodes as it encoded it.
	NoCoalesce bool
	// MaxOpenBlocks is the largest number of blocks
	// that the record-by-record resolution of an overlap cluster holds decoded at once;
	// zero means 64.
	// A block counts from when it is read and kept until its last record is resolved;
	// a block read and dropped as a duplicate of a block held never counts,
	// so Merge holds at most MaxOpenBlocks blocks, and the one it is reading.
	// It bounds the overlap depth at one seq, not the number of blocks that overlap in a chain:
	// a block is read only when the resolution reaches its first seq, and released after its last record.
	// Over it the merge fails with ErrMergeLimit.
	MaxOpenBlocks int
	// MaxFooterLen bounds the archive's footer, decoded and on disk, and what Merge keeps to build it while it writes;
	// over it the merge fails with ErrMergeLimit.
	// Zero means DefaultMaxFooterLen, so a Reader with default options uses the archive's footer.
	// The budget counts, for each block written, its F-2 entry, its F-3 list and the block descriptor Merge keeps,
	// and a verbatim F-3 list copied from an input counts twice, as the footer's bytes and as Merge's copy of them,
	// so a budget admits an archive footer of up to about half its size.
	MaxFooterLen int64
	// OnConflict receives every record conflict the merge finds, in ascending seq, at most once per capture_id and seq;
	// required, since a merge that finds a conflict fails and its report does not hold the conflicts.
	// It is called synchronously, and it may keep each Conflict it receives, which Merge never reuses.
	OnConflict func(Conflict)
	// Sync, when set, is called after every block written and after the trailer, as WriterOptions.Sync is.
	Sync Syncer
}

// MergeReport is the result of Merge.
//
// On error only Conflicts and ConflictsComplete are set, and every other field is zero:
// Conflicts counts the conflicts MergeOptions.OnConflict received before Merge returned,
// and ConflictsComplete is set with ErrMergeConflict alone,
// which Merge returns once it compared every block.
// Any other error found while Merge compares, an input's, a read's, a budget's or ctx's,
// is returned as itself with the conflicts counted so far and ConflictsComplete false.
type MergeReport struct {
	// PackID is the archive's pack_id, and ReplacementSetID its replacement_set_id.
	PackID, ReplacementSetID UUID
	// Inputs is the number of input packs.
	Inputs int
	// Blocks is the number of blocks written:
	// Copied input blocks, blocks of the resolution written as it encoded them, and CoalescedEncodings.
	Blocks int
	// Records is the number of records written.
	Records uint64
	// Copied is the number of input blocks copied verbatim:
	// written as they are, their envelope and on-disk body with their F-3 list as their input's footer stores it,
	// each alone in its coalescing group, under MergeOptions.NoCoalesce, or in a group whose new encoding failed.
	// An input block coalesced into a new block is not copied, and is counted in Coalesced.
	//
	// Copied, Duplicates, Resolved and Coalesced count every input block once,
	// so their sum is the number of blocks of the inputs.
	Copied int
	// Duplicates is the number of input blocks dropped as byte-identical duplicates of a block already kept.
	Duplicates int
	// Resolved is the number of input blocks resolved record by record, not counting those dropped as duplicates,
	// whether the blocks the resolution encoded from their records are written as it encoded them or coalesced.
	Resolved int
	// Coalesced is the number of input blocks coalesced into a new block:
	// input blocks taken as they were read into a group of more than one block whose new encoding was written.
	// A block the resolution encoded is not an input block:
	// a group that holds one counts only its other blocks here,
	// the input blocks of its records being counted in Resolved.
	Coalesced int
	// ResolvedEncodings is the number of new blocks the record-by-record resolution encoded,
	// those coalesced afterwards included, whose records are then encoded twice.
	ResolvedEncodings int
	// CoalescedEncodings is the number of new blocks written that coalescing encoded,
	// each from a group of more than one block;
	// a group whose encoding failed is not counted here, but in CoalesceFallbacks.
	CoalescedEncodings int
	// CoalesceFallbacks is the number of groups whose new encoding failed, its codec or its check in memory,
	// so their blocks were written as they were.
	CoalesceFallbacks int
	// Conflicts is the number of record conflicts found.
	Conflicts int
	// ConflictsComplete reports that every block was compared, so Conflicts and OnConflict saw every conflict.
	ConflictsComplete bool
	// Size is the number of bytes written to dst.
	Size uint64
}

// Conflict is one record conflict of a merge or of a read over several packs:
// records of the packs compared with the same capture_id and seq and different bytes
// (the tracepack format specification I-12).
type Conflict struct {
	// CaptureID and Seq identify the record.
	CaptureID UUID
	Seq       uint64
	// Versions holds, for each distinct version of the record, the pack_ids of the packs holding it, each once:
	// for a merge, the inputs, those whose blocks were dropped as duplicates included, in the order of the view's Packs;
	// for a read, in the order the readers were given.
	// The versions are in the order of the first block holding each, blocks taken in cluster order:
	// ascending first seq, then the order of the view's Packs or of the readers, then file order.
	Versions [][]UUID
}

// inputReaderAt reads a merge input through ra, and marks every error ra returns other than io.EOF as an inputReadError,
// so a failed read of an input stays apart from a pack that Merge refuses, whenever the Reader reads it.
type inputReaderAt struct {
	ra io.ReaderAt
}

var _ io.ReaderAt = inputReaderAt{}

// inputReadError is an error a merge input's ReaderAt returned: a failed read, not a defect of the pack.
type inputReadError struct {
	err error
}

var _ error = (*inputReadError)(nil)

// mergePlan is what the checks of Merge decided before it touches its output:
// the opened inputs, their blocks in overlap clusters, and the archive's identities and pack metadata.
type mergePlan struct {
	// inputs holds the Reader of each input, in the order of the view's Packs; each keeps its footer's F-3 lists.
	inputs []*Reader
	// clusters holds the overlap clusters of every input block, in ascending first seq.
	clusters []mergeCluster
	// meta is the archive's pack metadata, validated against facts.
	meta  *PackMeta
	facts PackFacts
	// packID and captureID are the ids of the archive's file header.
	packID    UUID
	captureID UUID
	// threshold, maxOpenBlocks and maxFooterLen are the options with their defaults applied.
	threshold     int
	maxOpenBlocks int
	maxFooterLen  int64
	// encodedHook, set only by tests, changes the on-disk body of every block the resolution encodes before it is checked,
	// and coalescedHook that of every block coalescing encodes.
	encodedHook   func(enc []byte) []byte
	coalescedHook func(enc []byte) []byte
	// loadHook, set only by tests, receives every block the resolution of a cluster loads, before the block is read,
	// with the number of blocks open and of records resolved by then.
	loadHook func(b *mergeBlock, open, resolved int)
}

// mergeBlock is one block of a merge input, as the input's validated footer indexes it.
type mergeBlock struct {
	// input is the index of the block's input in the view's Packs, and block its index in that input's footer.
	input int
	block int
	// first and last are the block's F-2 seq range.
	first uint64
	last  uint64
	// f3 is the block's F-3 entry list as the input's footer stores it;
	// it aliases the input's retained F-3 section, which is never modified.
	f3 []byte
}

// seqCluster is one overlap cluster of blocks, each described by a T that gives its seq range:
// a maximal set of blocks whose seq ranges chain into one interval,
// two ranges overlapping when they share a seq.
type seqCluster[T any] struct {
	// first and last bound the seqs of the cluster's blocks.
	first uint64
	last  uint64
	// blocks holds the cluster's blocks in ascending first seq, then in the order they were given in.
	blocks []T
}

// mergeCluster is one overlap cluster of input blocks (the tracepack storage specification §4 Merge),
// by their F-2 seq ranges;
// its blocks are in ascending first seq, then in input order, then in block order.
type mergeCluster = seqCluster[mergeBlock]

// Merge writes to dst the next generation of the scope of view, from inputs:
// an archive, the one member of a new replacement set, as the tracepack storage specification §4 Merge builds it.
//
// view must be ActiveView's unchanged result over the observation the merge was claimed on,
// and inputs[i] must be the pack view.Packs[i] names, in that order;
// the archive lists the inputs' coverage entries in that order.
// Merge re-checks what its inputs can show: each input's pack_id,
// and that each is a generation-0 pack listed in view.CompactedFrom, or the member of view.Generation,
// whose compacted_from view.CompactedFrom holds.
//
// Before it touches dst, Merge checks the options and the archive's identities;
// opens every input with opts.Reader and checks it from its pack metadata and its footer (the checks ErrMergeInput lists);
// checks that the inputs carry the same capture-level tags;
// and plans the archive from the footers: the input blocks in overlap clusters, and the archive's pack metadata.
// The archive's file header carries view.Capture exactly, the nil UUID included, and no redaction-present flag.
// Its pack metadata is that of the tracepack storage specification §4 Merge:
// pack_role archive; the claim's scope_generation, publisher_epoch and replacement_set_id, in a replacement set of one member;
// view.Hour as the period, a bound that does not fit in an int64 clamped to the int64 range;
// compaction_level one above the highest input's; compacted_from as view.CompactedFrom, absent when it is empty;
// supersedes naming the member of view.Generation, absent without one;
// the capture-level tags of the inputs;
// every coverage entry of the inputs, in input order, each carried once:
// an entry is not carried again when its encoding equals one already carried,
// with the known nested tags in tag order, the unknown nested entries in stored order and reserved bytes zero;
// every distinct classifier and max_frame_len value of the inputs, sorted;
// opts.Writer as writer; and seq_start at the archive's first record, or without records the largest input seq_start.
// No other tag of an input is carried.
//
// Merge then writes the archive to dst: its file header and pack metadata,
// the overlap clusters in ascending first seq, then its footer and trailer, synced through opts.Sync as a Writer syncs.
// Every block of a cluster is read in full, once, and checked before any of its bytes is written:
// as a full read checks it (the tracepack format specification §6), against its F-2 entry,
// and its records against its F-3 summary (the checks ErrMergeInput lists).
// A cluster of one block is copied verbatim, unless coalescing groups it with other blocks:
// its envelope and on-disk body, and its F-3 list as its input's footer stores it.
// A cluster whose blocks share one F-2 identity,
// the first and last seq, record count, lengths, record_header_len and body_crc, is a duplicate candidate:
// when every block's envelope and on-disk body equal the first block's,
// the first is copied verbatim, as a cluster of one block is, and the others are dropped as duplicates.
//
// Any other cluster, and a duplicate candidate whose blocks differ, is resolved record by record, in ascending seq.
// Its blocks are taken in ascending first seq, then in the order of view.Packs;
// a block is read when the resolution reaches its first seq, and released once its last record is resolved,
// so at most opts.MaxOpenBlocks blocks are held at once.
// A block whose envelope and on-disk body equal those of a block held with the same first seq is dropped as a duplicate.
// Records of one seq that are equal, header row and payload byte for byte, are written once;
// records of one seq that differ are a conflict, which opts.OnConflict receives.
// The resolved records are written in new blocks encoded with opts.Codec, each checked in memory before it is written:
// a new block holds records of one record_header_len
// while their decoded size, Σ record_header_len + payload_len, stays within opts.BlockThreshold,
// so a record whose decoded size alone reaches it is written in a block of its own.
// A new block of the resolution that fails its check fails the merge.
// Once a conflict is found nothing more is written, but every block is still read, checked and compared,
// so opts.OnConflict receives every conflict, before Merge returns ErrMergeConflict.
//
// Unless opts.NoCoalesce is set, Merge coalesces small blocks (the tracepack storage specification §4 Merge).
// It takes the blocks it writes in seq order, those it copies and those the resolution encodes,
// and groups them greedily:
// a group grows while the next block has the group's record_header_len
// and the group's decoded size, that block's included, stays within opts.BlockThreshold,
// so a block whose decoded size alone reaches it is a group of its own.
// A group of one block is written as it is:
// an input block copied verbatim, or a block of the resolution as it encoded it.
// A larger group is encoded with opts.Codec as one new block, its records byte for byte and in their order,
// and checked in memory before it is written.
// A new block whose encoding fails, its codec or its check, is discarded,
// and the group's blocks are written as they are, each input block with its F-3 list byte for byte.
// A block whose decoded size alone reaches opts.BlockThreshold is written at once.
// Any other group waits for the block after it, so its blocks are written only once that block is read or encoded,
// and the last group once every block was read;
// a group is not written at all when an error or a conflict comes first.
// The archive's footer, decoded and on disk, and what Merge keeps to build it, stay within opts.MaxFooterLen.
//
// After any error the caller discards what was written to dst and publishes nothing;
// a caller publishes the archive only after Merge returns nil.
// Every error before the archive's header leaves dst untouched:
// an option or identity error, ErrMergeInput, an input's failed read or ErrReadLimit,
// ErrMergeLimit for the archive's pack metadata, or ctx's error.
// An error after the header returns without writing the footer or the trailer, so dst holds no finalized pack:
// ErrMergeInput for a block, an input's failed read or ErrReadLimit, ErrMergeLimit, ErrMergeConflict,
// a new block of the resolution that fails its check, ctx's error, or an error writing or syncing dst.
// Only an error of the footer-and-trailer write, or of the sync after it, may leave bytes that form a finalized pack,
// which is still a failure.
//
// Parameters:
//   - ctx: cancels the merge while it opens the inputs, before it writes, before each block it reads,
//     after every 4096 records it resolves, and before it writes the footer.
//   - dst: the archive's output; bytes are written in file order and never rewritten.
//   - view: the scope's active view.
//   - inputs: the packs of view.Packs, in that order.
//   - opts: see MergeOptions.
//
// Returns:
//   - MergeReport: what the archive holds; on error, only the conflicts found (see MergeReport).
//   - error: an empty opts.Writer, a nil opts.OnConflict, an opts.ScopeGeneration of 0,
//     an opts.ScopeGeneration or opts.PublisherEpoch above 2^63-1, an invalid opts.Codec,
//     a negative or oversized opts.BlockThreshold, a negative opts.MaxOpenBlocks or opts.MaxFooterLen,
//     a view without packs, or an input without a ReaderAt;
//     a rank at or below view.Generation's, an opts.ReplacementSetID equal to view.Generation's,
//     or an opts.PackID that names a pack of view.Packs, view.CompactedFrom or view.Generation;
//     an error wrapping ErrMergeInput; an error wrapping an input's ReaderAt error, or ErrReadLimit, naming the input;
//     a *FieldError validating the archive's pack metadata;
//     an error wrapping ErrMergeLimit for archive pack metadata longer than opts.Reader.MaxPackMetadataLen,
//     for an archive footer, or the block summaries kept for it, over opts.MaxFooterLen,
//     or for a cluster whose resolution would hold more than opts.MaxOpenBlocks blocks;
//     an error wrapping ErrMergeConflict once every block was compared;
//     the error of a new block of the resolution that fails its check;
//     an error wrapping io.ErrUnexpectedEOF, naming the input and the block, for a block read cut short;
//     the error of writing to dst, io.ErrShortWrite for a write that takes fewer bytes without an error,
//     or of syncing it;
//     ctx's error, wrapped.
func Merge(ctx context.Context, dst io.Writer, view View, inputs []MergeInput, opts MergeOptions) (MergeReport, error) {
	p, err := planMerge(ctx, &view, inputs, &opts)
	if err != nil {
		return MergeReport{}, err
	}

	return p.write(ctx, dst, &opts)
}

// planMerge runs the checks of Merge that come before its output is touched, in order:
// the options and identities, every input's own checks, the capture-level tags, then the plan and ctx.
//
// Returns:
//   - *mergePlan: the plan; nil on error.
//   - error: the errors of Merge before the archive's header.
func planMerge(ctx context.Context, view *View, inputs []MergeInput, opts *MergeOptions) (*mergePlan, error) {
	if err := checkMergeOptions(view, inputs, opts); err != nil {
		return nil, err
	}
	p, setID, err := newMergePlan(view, opts)
	if err != nil {
		return nil, err
	}

	if err := p.openInputs(ctx, view, inputs, opts.Reader); err != nil {
		return nil, err
	}
	if err := p.checkCaptureTags(view); err != nil {
		return nil, err
	}
	p.clusters = clustersOf(p.inputs)
	if err := p.planMeta(view, opts, setID); err != nil {
		return nil, err
	}

	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("tracepack: merge: %w", err)
	}

	return p, nil
}

// checkMergeOptions rejects options Merge cannot honor, a view without packs, and an input without a ReaderAt.
func checkMergeOptions(view *View, inputs []MergeInput, opts *MergeOptions) error {
	switch {
	case opts.Writer == "":
		return errors.New("tracepack: MergeOptions.Writer is empty")
	case opts.OnConflict == nil:
		return errors.New("tracepack: MergeOptions.OnConflict is nil, so a conflict could not be reported")
	case opts.ScopeGeneration == 0:
		return errors.New("tracepack: MergeOptions.ScopeGeneration is 0; generations are numbered from 1")
	case opts.ScopeGeneration > format.MaxU64 || opts.PublisherEpoch > format.MaxU64:
		return fmt.Errorf("tracepack: MergeOptions rank (%d, %d) above %d", opts.PublisherEpoch, opts.ScopeGeneration, format.MaxU64)
	case !codec.Known(uint8(opts.Codec)):
		return fmt.Errorf("tracepack: MergeOptions.Codec %s is not a codec a writer may use", opts.Codec)
	case opts.BlockThreshold < 0 || int64(opts.BlockThreshold) > int64(format.MaxLen32):
		return fmt.Errorf("tracepack: MergeOptions.BlockThreshold %d outside 0 to %d", opts.BlockThreshold, format.MaxLen32)
	case opts.MaxOpenBlocks < 0:
		return fmt.Errorf("tracepack: MergeOptions.MaxOpenBlocks %d is negative", opts.MaxOpenBlocks)
	case opts.MaxFooterLen < 0:
		return fmt.Errorf("tracepack: MergeOptions.MaxFooterLen %d is negative", opts.MaxFooterLen)
	case len(view.Packs) == 0:
		return errors.New("tracepack: merge: the view holds no packs")
	}
	for i := range inputs {
		if inputs[i].ReaderAt == nil {
			return fmt.Errorf("tracepack: merge: input %d has no ReaderAt", i)
		}
	}

	return nil
}

// newMergePlan returns the plan's identities and options, with the defaults applied,
// and the archive's replacement_set_id, after checking them against view.
func newMergePlan(view *View, opts *MergeOptions) (*mergePlan, UUID, error) {
	g := view.Generation
	if g != nil {
		claim := Generation{PublisherEpoch: opts.PublisherEpoch, ScopeGeneration: opts.ScopeGeneration}
		if compareRank(claim, *g) <= 0 {
			return nil, UUID{}, fmt.Errorf("tracepack: MergeOptions rank (%d, %d) is not above the rank (%d, %d) of the view's generation",
				claim.PublisherEpoch, claim.ScopeGeneration, g.PublisherEpoch, g.ScopeGeneration)
		}
	}

	setID, err := idOrNew(opts.ReplacementSetID)
	if err != nil {
		return nil, UUID{}, err
	}
	if g != nil && setID == g.ReplacementSetID {
		return nil, UUID{}, fmt.Errorf("tracepack: MergeOptions.ReplacementSetID %s is the view's generation's", setID)
	}

	packID, err := idOrNew(opts.PackID)
	if err != nil {
		return nil, UUID{}, err
	}
	if slices.Contains(view.Packs, packID) || slices.Contains(view.CompactedFrom, packID) || (g != nil && packID == g.Member) {
		return nil, UUID{}, fmt.Errorf("tracepack: MergeOptions.PackID %s is the id of a pack of the view or its lineage", packID)
	}

	p := &mergePlan{
		packID:        packID,
		captureID:     view.Capture,
		threshold:     cmp.Or(opts.BlockThreshold, DefaultBlockThreshold),
		maxOpenBlocks: cmp.Or(opts.MaxOpenBlocks, defaultMaxOpenBlocks),
		maxFooterLen:  cmp.Or(opts.MaxFooterLen, DefaultMaxFooterLen),
	}

	return p, setID, nil
}

// clustersOf returns the overlap clusters of the blocks of inputs, whose footers are valid, in ascending first seq,
// as chainClusters chains them by their F-2 seq ranges.
func clustersOf(inputs []*Reader) []mergeCluster {
	total := 0
	for _, r := range inputs {
		total += len(r.footer.blocks)
	}
	blocks := make([]mergeBlock, 0, total)
	for i, r := range inputs {
		for j := range r.footer.blocks {
			s := &r.footer.blocks[j]
			blocks = append(blocks, mergeBlock{input: i, block: j, first: s.firstSeq, last: s.lastSeq, f3: r.rawF3(j)})
		}
	}
	// The blocks were gathered in input order, then block order, which the sort keeps among equal first seqs.
	return chainClusters(blocks, func(b *mergeBlock) (uint64, uint64) { return b.first, b.last })
}

// chainClusters sorts blocks by first seq, keeping their order among equal first seqs,
// and returns their overlap clusters in ascending first seq.
// A block joins the cluster before it when its first seq is at or below the cluster's last seq:
// taken in ascending first seq, it then shares its first seq with a block of that cluster.
//
// Parameters:
//   - blocks: the blocks, sorted in place.
//   - seqs: returns the first and last seq of a block.
//
// Returns:
//   - []seqCluster[T]: the clusters, whose blocks are sub-slices of blocks, each capped at its end.
func chainClusters[T any](blocks []T, seqs func(*T) (first, last uint64)) []seqCluster[T] {
	slices.SortStableFunc(blocks, func(a, b T) int {
		af, _ := seqs(&a)
		bf, _ := seqs(&b)

		return cmp.Compare(af, bf)
	})

	var out []seqCluster[T]
	start := 0
	for k := range blocks {
		first, last := seqs(&blocks[k])
		if n := len(out); n > 0 && first <= out[n-1].last {
			c := &out[n-1]
			c.last = max(c.last, last)
			c.blocks = blocks[start : k+1 : k+1]

			continue
		}
		start = k
		out = append(out, seqCluster[T]{first: first, last: last, blocks: blocks[k : k+1 : k+1]})
	}

	return out
}

// inputError returns an error wrapping ErrMergeInput for input i, the pack id, and its cause.
func inputError(i int, id UUID, cause error) error {
	return fmt.Errorf("%w: input %d, pack %s: %w", ErrMergeInput, i, id, cause)
}

// openInput opens input i, which view names id, keeping its footer's F-3 lists.
// It reads the input through inputReaderAt, which the Reader keeps for every later read.
// ctx's error is returned as itself, wrapped;
// a failed read of the input's ReaderAt, and the ErrReadLimit of a reader budget, the pack metadata's or the footer's,
// are returned as themselves, wrapped with the input;
// every other failure to open it, a short read of an object shorter than in.Size included, is an input error.
func openInput(ctx context.Context, i int, id UUID, in MergeInput, opts ReaderOptions) (*Reader, error) {
	r, err := openReader(ctx, inputReaderAt{ra: in.ReaderAt}, in.Size, opts, true)
	var readErr *inputReadError
	switch {
	case err == nil:
	case ctx.Err() != nil && errors.Is(err, ctx.Err()):
		return nil, fmt.Errorf("tracepack: merge: %w", err)
	case errors.As(err, &readErr), errors.Is(err, ErrReadLimit):
		return nil, fmt.Errorf("tracepack: merge: input %d, pack %s: %w", i, id, err)
	default:
		return nil, inputError(i, id, err)
	}

	// A footer, or its blocks' F-3 lists, over MaxFooterLen leaves the pack finalized without a footer index;
	// the budget decides, not the pack.
	if r.finalized && r.footer == nil && errors.Is(r.footerErr, ErrReadLimit) {
		return nil, fmt.Errorf("tracepack: merge: input %d, pack %s: %w", i, id, r.footerErr)
	}

	return r, nil
}

// checkInput checks the input r opened, which view names id, as openInputs states,
// and returns the facts of its records.
func checkInput(view *View, lineage map[UUID]struct{}, id UUID, r *Reader) (PackFacts, error) {
	m := r.meta
	switch got := UUID(r.hdr.PackID); {
	case got != id:
		return PackFacts{}, fmt.Errorf("the input is pack %s", got)
	case !tierRole(m.PackRole):
		return PackFacts{}, fmt.Errorf("pack_role %s takes no part in a scope's view", m.PackRole)
	}
	if err := checkInputLineage(view, lineage, id, m); err != nil {
		return PackFacts{}, err
	}
	if capture := UUID(r.hdr.CaptureID); capture != view.Capture {
		return PackFacts{}, fmt.Errorf("capture_id %s, not the view's %s", capture, view.Capture)
	}
	if hour, ok := periodHour(m.PeriodStart, m.PeriodEnd); !ok || hour != view.Hour {
		return PackFacts{}, fmt.Errorf("period %d to %d is not inside the view's UTC hour %d", m.PeriodStart, m.PeriodEnd, view.Hour)
	}

	return checkInputFooter(r, view.Hour)
}

// checkInputLineage checks that the pack id whose pack metadata is m, an input of view, is in the view's lineage:
// a generation-0 pack that view.CompactedFrom lists,
// or an archive that is the member of view.Generation, every pack of whose compacted_from view.CompactedFrom lists.
// lineage holds the packs of view.CompactedFrom.
func checkInputLineage(view *View, lineage map[UUID]struct{}, id UUID, m *PackMeta) error {
	if err := checkRoleGeneration(m); err != nil {
		return err
	}
	if m.PackRole != PackRoleArchive {
		if _, listed := lineage[id]; !listed {
			return errors.New("a generation-0 pack that the view's compacted_from does not list")
		}

		return nil
	}

	gen := generationOf(PackInfo{packID: id, meta: m})
	if view.Generation == nil || gen != *view.Generation {
		return fmt.Errorf("an archive of replacement set %s ranked (%d, %d) that is not the member of the view's generation",
			gen.ReplacementSetID, gen.PublisherEpoch, gen.ScopeGeneration)
	}
	for _, from := range m.CompactedFrom {
		if _, listed := lineage[from]; !listed {
			return fmt.Errorf("its compacted_from lists %s, which the view's compacted_from does not", from)
		}
	}

	return nil
}

// checkInputFooter checks the finalized footer of the input r opened against the UTC hour hour and r's own pack metadata:
// every block's records lie in hour, a pack with records has seq_start at its first record's seq,
// and its records breach none of its commitments.
// It returns the facts of its records that the archive's pack metadata is validated against.
func checkInputFooter(r *Reader, hour int64) (PackFacts, error) {
	if !r.finalized {
		return PackFacts{}, fmt.Errorf("the pack is not finalized: %w", r.footerErr)
	}
	if r.footer == nil {
		return PackFacts{}, fmt.Errorf("the pack has no valid footer: %w", r.footerErr)
	}

	blocks := r.footer.blocks
	var facts PackFacts
	for j := range blocks {
		b := &blocks[j]
		// The footer validation puts an F-2 entry's ts_min and ts_max in one UTC hour, so one bound places the block.
		if hourOf(b.tsMin) != hour {
			return PackFacts{}, fmt.Errorf("block %d at offset %d holds records outside the view's UTC hour %d", j, b.offset, hour)
		}
		classified, oversized, redacted := b.commitmentFacts()
		facts.AnyClassified = facts.AnyClassified || classified
		facts.AnyOversized = facts.AnyOversized || oversized
		facts.AnyRedacted = facts.AnyRedacted || redacted
	}
	// The block read ties the first F-2 first_seq to the first record, through the envelope and I-2.
	if len(blocks) > 0 && r.meta.SeqStart != blocks[0].firstSeq {
		return PackFacts{}, fmt.Errorf("seq_start %d, but its first record's seq is %d", r.meta.SeqStart, blocks[0].firstSeq)
	}
	// The footer validation checked F-5 against the F-2 and F-3 values these facts come from.
	if err := checkStoredCommitments(r.meta, facts); err != nil {
		return PackFacts{}, err
	}

	return facts, nil
}

// equalPtr reports whether a and b are both nil, or both point to equal values.
func equalPtr[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == b
	}

	return *a == *b
}

// equalTimers reports whether a and b are both absent, or both present with equal timers T1 to T8.
func equalTimers(a, b *HSMSTimers) bool {
	if a == nil || b == nil {
		return a == b
	}

	return equalPtr(a.T1, b.T1) && equalPtr(a.T2, b.T2) && equalPtr(a.T3, b.T3) && equalPtr(a.T4, b.T4) &&
		equalPtr(a.T5, b.T5) && equalPtr(a.T6, b.T6) && equalPtr(a.T7, b.T7) && equalPtr(a.T8, b.T8)
}

// ReadAt implements io.ReaderAt.
func (a inputReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := a.ra.ReadAt(p, off)
	if err != nil && !errors.Is(err, io.EOF) {
		err = &inputReadError{err: err}
	}

	return n, err
}

// Error returns the ReaderAt's error message.
func (e *inputReadError) Error() string {
	return e.err.Error()
}

// Unwrap returns the ReaderAt's error.
func (e *inputReadError) Unwrap() error {
	return e.err
}

// openInputs opens every input of view and checks it: each is the pack view.Packs names at its position,
// in the view's lineage, of a role that takes part in a scope's view, of view's capture,
// with a period inside view.Hour and a finalized footer that checkInputFooter accepts.
// It keeps the Readers, and the facts of every input's records, in p.
func (p *mergePlan) openInputs(ctx context.Context, view *View, inputs []MergeInput, opts ReaderOptions) error {
	if len(inputs) != len(view.Packs) {
		return fmt.Errorf("%w: %d inputs for the %d packs of the view", ErrMergeInput, len(inputs), len(view.Packs))
	}

	lineage := make(map[UUID]struct{}, len(view.CompactedFrom))
	for _, id := range view.CompactedFrom {
		lineage[id] = struct{}{}
	}

	p.inputs = make([]*Reader, 0, len(inputs))
	for i, in := range inputs {
		id := view.Packs[i]
		r, err := openInput(ctx, i, id, in, opts)
		if err != nil {
			return err
		}
		facts, err := checkInput(view, lineage, id, r)
		if err != nil {
			return inputError(i, id, err)
		}
		p.facts.AnyClassified = p.facts.AnyClassified || facts.AnyClassified
		p.facts.AnyOversized = p.facts.AnyOversized || facts.AnyOversized
		p.inputs = append(p.inputs, r)
	}

	return nil
}

// checkCaptureTags checks that every input carries the first input's value of each capture-level tag.
func (p *mergePlan) checkCaptureTags(view *View) error {
	first := p.inputs[0].meta
	for i := 1; i < len(p.inputs); i++ {
		m := p.inputs[i].meta
		for _, tag := range captureTags {
			if !tag.equal(first, m) {
				return inputError(i, view.Packs[i], fmt.Errorf("capture-level tag %s differs from pack %s's", tag.name, view.Packs[0]))
			}
		}
	}

	return nil
}

// planMeta builds the archive's pack metadata into p, as Merge states it,
// validates it against the facts of the inputs' records,
// and checks its encoded length against the reader budget the inputs were opened with.
// Every input's period lies in view.Hour, so it is the hour of an int64 instant and hourBounds cannot overflow.
func (p *mergePlan) planMeta(view *View, opts *MergeOptions, setID UUID) error {
	c := p.inputs[0].Header().Meta
	start, last := hourBounds(view.Hour)
	// hourBounds clamps last to the int64 maximum only when the hour's end does not fit:
	// 2^63 is not a whole number of hours, so no unclamped last is the int64 maximum.
	end := last
	if last < math.MaxInt64 {
		end = last + 1
	}

	m := &PackMeta{
		ToolID: c.ToolID, Transport: c.Transport, CaptureMethod: c.CaptureMethod, Vantage: c.Vantage, Recorder: c.Recorder,
		TimeSource: c.TimeSource, LifecycleCoverage: c.LifecycleCoverage, QualityEvaluated: c.QualityEvaluated,
		RecorderInstanceID: c.RecorderInstanceID, CaptureOriginUTCNs: c.CaptureOriginUTCNs, CaptureOriginMonoNs: c.CaptureOriginMonoNs,
		ClockStepToleranceNs: c.ClockStepToleranceNs, PreviousCaptureID: c.PreviousCaptureID,
		SiteID: c.SiteID, EquipmentModel: c.EquipmentModel, EquipmentSWRev: c.EquipmentSWRev, HostSoftware: c.HostSoftware,
		HostEndpoint: c.HostEndpoint, EquipmentEndpoint: c.EquipmentEndpoint, EquipmentConnectMode: c.EquipmentConnectMode,
		DeviceID: c.DeviceID, HSMSTimers: c.HSMSTimers, SourceTZ: c.SourceTZ, SourceDialect: c.SourceDialect, SourceRefs: c.SourceRefs,

		Writer:      opts.Writer,
		PeriodStart: start, PeriodEnd: end,
		PackRole:        PackRoleArchive,
		ScopeGeneration: new(opts.ScopeGeneration), PublisherEpoch: new(opts.PublisherEpoch),
		ReplacementSetID: new(setID), ReplacementSetSize: new(uint64(1)), ReplacementSetIndex: new(uint64(0)),
		CompactedFrom: slices.Clone(view.CompactedFrom),
		SeqStart:      p.seqStart(),
	}
	if g := view.Generation; g != nil {
		m.Supersedes = []UUID{g.Member}
	}
	m.Classifiers, m.MaxFrameLens = p.distinctCommitments()

	var err error
	if m.CompactionLevel, err = p.compactionLevel(view); err != nil {
		return err
	}
	if m.Coverage, err = p.coverage(view); err != nil {
		return err
	}

	p.facts.GenerationHasPredecessor = view.Generation != nil
	if err := m.Validate(p.facts); err != nil {
		return err
	}
	raw, err := m.MarshalBinary()
	if err != nil {
		return err
	}
	// A reader configured as the inputs' readers, the next merge's included, then opens the archive.
	if budget := opts.Reader.withDefaults().MaxPackMetadataLen; int64(len(raw)) > budget {
		return fmt.Errorf("tracepack: merge: the archive's pack metadata of %d bytes exceeds MaxPackMetadataLen %d: %w",
			len(raw), budget, ErrMergeLimit)
	}
	p.meta = m

	return nil
}

// seqStart returns the archive's seq_start: its first record's seq, the first seq of the first cluster,
// or without records the largest seq_start of the inputs, the latest position they knew of the capture's next seq.
func (p *mergePlan) seqStart() uint64 {
	if len(p.clusters) > 0 {
		return p.clusters[0].first
	}

	var seq uint64
	for _, r := range p.inputs {
		seq = max(seq, r.meta.SeqStart)
	}

	return seq
}

// distinctCommitments returns every distinct classifier and max_frame_len value of the inputs, sorted;
// nil when the inputs hold none.
func (p *mergePlan) distinctCommitments() ([]string, []uint64) {
	var classifiers []string
	var maxFrameLens []uint64
	for _, r := range p.inputs {
		classifiers = append(classifiers, r.meta.Classifiers...)
		maxFrameLens = append(maxFrameLens, r.meta.MaxFrameLens...)
	}
	slices.Sort(classifiers)
	slices.Sort(maxFrameLens)

	return slices.Compact(classifiers), slices.Compact(maxFrameLens)
}

// compactionLevel returns the archive's compaction_level, one above the highest input's.
// An input at 255 leaves no level for the archive, which is an input error.
func (p *mergePlan) compactionLevel(view *View) (uint8, error) {
	var level uint8
	for i, r := range p.inputs {
		l := r.meta.CompactionLevel
		if l == math.MaxUint8 {
			return 0, inputError(i, view.Packs[i], errors.New("compaction_level 255 leaves no level for the archive"))
		}
		level = max(level, l)
	}

	return level + 1, nil
}

// coverage returns every coverage entry of the inputs, in input order and then in stored order, each carried as data,
// except an entry whose encoding equals an entry already returned.
// The encoding is the one MarshalBinary writes (the tracepack format specification §5):
// the known nested tags in tag order, the unknown nested entries in stored order, and reserved bytes zero.
func (p *mergePlan) coverage(view *View) ([]Coverage, error) {
	var out []Coverage
	seen := make(map[string]struct{})
	for i, r := range p.inputs {
		for k := range r.meta.Coverage {
			c := &r.meta.Coverage[k]
			e, err := c.entry()
			if err != nil {
				return nil, inputError(i, view.Packs[i], fmt.Errorf("coverage entry %d: %w", k, err))
			}
			key := string(tlv.AppendEntry(nil, e))
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, *cloneCoverage(c))
		}
	}

	return out, nil
}
