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
// a merge folds a scope's packs into an archive, the scope's next generation (the tracepack storage specification §2 and §4).
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
// NewWriter writes one pack from pack metadata the caller assembles, the layer under SegmentWriter, Merge and Repair.
//
// # Reading
//
// Open bootstraps a Reader over one pack, and Reader.Iterate reads its records.
// MergeIterate is one read over several packs, typically the packs of the active views of the scopes a query touches
// (the tracepack semantics specification §7.4):
// it compares the copies of each record across the packs, yields identical copies once,
// and yields the records in capture order or time order, as MergeIterateOptions.Order selects.
// Records of one seq with different bytes are reported, not hidden:
// every version the query selects is yielded, marked as a conflict,
// and the conflict is listed in Result.Conflicts beside the defects in Result.Incomplete.
// With Query.Retention set, a read applies the retention boundary (the tracepack storage specification §5, Retention):
// a covered hour found removed ends the whole read with an error wrapping ErrRemoved,
// and the caller discards everything the read reported of the hours Result.Removed lists.
//
// FindTransaction looks up the reply to a primary record (the tracepack semantics specification §7.2)
// in one observation that a PackSource fixes before any record is read (the tracepack storage specification §5).
// It reports TxMatched, TxAmbiguous, TxUnmatched or TxIncomplete,
// with the records it kept and every reason the lookup is not complete, each a TxGap;
// it returns TxUnmatched only when it read every seq of a window that a record it read bounds, and found no such reason.
// NewReaderSource returns a PackSource over packs the caller opened, such as the files of a NewDirSink;
// its scopes are not indexed unless ReaderSourceOptions.Complete asserts
// that the packs given, the rejected registrations among them as Evidence, are every pack a catalog of the tool would hold evidence of.
//
// # Operating a store
//
// ActiveView computes the active view of one scope from a complete observation of the scope's packs and commit objects:
// its current generation, the packs whose records the view reads, the lineage the next merge writes, and the packs that may be deleted.
// Merge writes the scope's next generation from the packs of that view, one archive;
// the caller publishes it only after Merge returns nil, under the commit protocol of the tracepack storage specification §5.
// Verify reports the outcome of a pack's verification, and Repair writes a repair patch of a damaged pack.
// NewStoreSource returns a PackSource over an ObjectStore, the bucket, and a Catalog, the caller's catalog of one tool;
// the Godoc of NewStoreSource, ObjectStore and Catalog states the contracts the store and the catalog must keep.
// Give StoreSourceOptions.Retention and TxOptions.Retention the same Retention provider;
// deleting the objects of a removed hour is the storage service's work.
// SegmentKey, ArchiveKey and CommitKey build the keys of the tracepack storage specification §3.
package tracepack
