// Package tracepack is the Go reference implementation of the tracepack container format (.tpk):
// an immutable, compressed, self-indexed archive of recorded SECS-II traffic between one tool and its host,
// including HSMS control messages and transport lifecycle events.
//
// The format is specified in docs/specs/tracepack/ of the go-secs repository.
// The specification, not this package, defines every field;
// the conformance corpus is the executable arbiter where the two disagree.
// This module is versioned independently of go-secs, with tags of the form tracepack/vX.Y.Z.
//
// # Concepts
//
// A capture is one run of one recorder over one tool, named by its capture_id; its records are numbered by seq.
// A pack is one immutable file of records with its pack metadata and a footer that indexes them.
// A scope is one capture in one UTC hour, and every pack but an extract holds the records of one scope.
// A recorder stages segments, the packs of one flush interval;
// a merge folds the packs of a scope its caller gives into one archive (the tracepack storage specification §2 and §4).
//
// # Recording
//
// A CaptureDescriptor holds the pack metadata the producer of a capture owns.
// SegmentWriter writes one capture as segments through a SegmentSink,
// owning the capture's start and stop boundaries, its seq counter and the segments' rolls;
// NewDirSink stores the segments as files laid out as a bucket's keys.
// A Classifier, such as the one tracepack/classify returns, sets the decode_status of each frame AppendFrame takes.
// The SegmentWriter Godoc states which errors refuse a record and leave the writer usable, and which fail it;
// an error wrapping ErrPublishUncertain reports a segment that is published but may not survive a system crash.
// NewWriter writes one pack from pack metadata the caller assembles, the layer under SegmentWriter and Merge;
// it and Merge write block bodies and the footer with zstd unless their options set Uncompressed.
//
// # Reading
//
// Open bootstraps a Reader over one pack, and Reader.Iterate reads its records.
// A Reader keeps the bytes of Open's first round that hold blocks and serves block reads from them,
// so a pack up to ReaderOptions.WholeReadThreshold, 4 MiB by default, costs one ReadAt for Open and none afterwards;
// it keeps at most max(WholeReadThreshold, HeadWindow + TailWindow) bytes,
// outside the budgets of MergeIterateOptions.MaxHeldBytes, TxOptions.MaxHeldBytes and TxOptions.MaxStateBytes.
// MergeIterate is one read over several packs, typically the packs of the scopes a query touches
// (the tracepack semantics specification §7.4):
// it compares the copies of each record across the packs, yields identical copies once,
// and yields the records in capture order or time order, as MergeIterateOptions.Order selects.
// Records of one seq with different bytes are reported, not hidden:
// every version the query selects is yielded, marked as a conflict,
// and the conflict is listed in Result.Conflicts beside the defects in Result.Incomplete.
// MergeIterate reads ahead of the block it resolves, within MaxHeldBytes,
// in time order, and in capture order when at most one capture has a cluster to read,
// so it may issue several ReadAt calls at once on a Reader's io.ReaderAt, as Open does,
// and it waits for all of them before it returns.
// Every read reports, as a ReasonCrashGap defect, each stop-unclean capture boundary whose gap meets the query's time range
// (the tracepack semantics specification §7.4, Crash gaps).
//
// FindTransaction looks up the reply to a primary record (the tracepack semantics specification §7.2)
// over the packs a PackSource gives for each hour it reads, of any capture.
// It reports TxMatched, TxAmbiguous or TxNotFound,
// with the records it kept and every reason the lookup is incomplete, each a TxGap in TxResult.Incomplete, beside any outcome;
// TxNotFound states nothing about records outside the packs given.
// NewReaderSource returns a PackSource over packs the caller opened, all of one tool, such as the files of a NewDirSink,
// grouped by the UTC hour of their period;
// the caller owns the readers and keeps them open while the source is used.
//
// ExportJSONL writes the canonical JSONL export of one pack, schema tracepack-jsonl/1
// (the tracepack JSONL specification):
// a header line, then one line per record, the same bytes any conforming exporter writes for the pack.
// A damaged pack exports the records of its validated blocks, with the Result Iterate returns for it:
// a failed block or a crash gap makes it incomplete, while a footer that is not used only sets FooterErr.
// A reader budget that keeps part of the pack out fails the export with an error wrapping ErrReadLimit instead.
// Each line is written in pieces of at most 64 KiB, so one line may take several Write calls.
// Verify reads every block of a pack and reports its outcome, from finalized-consistent to corrupt.
//
// # Merging and storage
//
// Merge writes one archive of a scope from the segments and archives its caller gives,
// with the scope_generation and publisher_epoch the caller supplies;
// it writes the archive's lineage, compacted_from and supersedes, from the inputs.
// The caller decides which packs a merge takes and when a pack may be deleted,
// and publishes the archive only after Merge returns nil (the tracepack storage specification §4).
// SegmentKey and ArchiveKey build the keys of the tracepack storage specification §3, and EscapeToolID spells a tool as a key does.
package tracepack
