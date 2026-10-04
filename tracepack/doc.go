// Package tracepack is the Go reference implementation of the tracepack container format (.tpk):
// an immutable, compressed, self-indexed archive of recorded SECS-II traffic between one tool and its host,
// including HSMS control messages and transport lifecycle events.
//
// The format is specified in docs/specs/tracepack/ of the go-secs repository.
// The specification, not this package, defines every field;
// the conformance corpus is the executable arbiter where the two disagree.
//
// This module is versioned independently of go-secs, with tags of the form tracepack/vX.Y.Z.
// The package holds the record and pack-metadata types, the typed event payloads, the Writer, the Reader, Verify, Repair, ActiveView, Merge, MergeIterate, FindTransaction,
// the store-backed PackSource of NewStoreSource and the object key builders;
// extraction follows.
//
// ActiveView computes the active view of one scope, one capture in one UTC hour (the tracepack storage specification §2 and §4),
// from a complete observation of the scope's packs and commit objects:
// its current generation, the packs whose records the view reads, the lineage the next merge writes, and the packs that may be deleted.
// Merge writes the scope's next generation from the packs of that view, one archive:
// blocks that no other block overlaps copied verbatim, exact duplicates dropped, every other overlap resolved record by record,
// and small blocks coalesced unless MergeOptions.NoCoalesce is set.
// A defective input fails the merge with ErrMergeInput, and a record conflict with ErrMergeConflict.
// After any error the caller discards what Merge wrote;
// it publishes the archive only after Merge returns nil, under the commit protocol of the tracepack storage specification §5.
//
// Reader.Iterate reads one pack.
// MergeIterate is one read over several packs, typically the packs of the active views of the scopes a query touches
// (the tracepack semantics specification §7.4):
// it compares the copies of each record across the packs, yields identical copies once,
// and yields the records in the order MergeIterateOptions.Order selects:
// capture order, each capture's records in ascending seq, or time order, by ts_utc_ns.
// Records of one seq with different bytes are reported, not hidden:
// every version the query selects is yielded, marked as a conflict,
// and the conflict is listed in Result.Conflicts beside the defects in Result.Incomplete.
//
// FindTransaction looks up the reply to a primary record (the tracepack semantics specification §7.2)
// in one observation that a PackSource fixes before any record is read (the tracepack storage specification §5):
// the primary's hour and the next TxOptions.MaxScopes - 1 hours, each read once by MergeIterate unless its view is conflicted.
// It reports TxMatched, TxAmbiguous, TxUnmatched or TxIncomplete,
// with the records it kept and every reason the lookup is not complete, each a TxGap;
// it returns TxUnmatched only when it read every seq of a window that a record it read bounds, and found no such reason.
// Its comparison of a record's copies covers the hours it reads, as MergeIterate's covers the packs it is given.
// The Godoc of FindTransaction, PackSource and Observation states the rules.
//
// NewStoreSource returns a PackSource over an ObjectStore, the bucket, and a Catalog, the caller's catalog of one tool.
// It fixes a scope the catalog indexes from one catalog snapshot,
// and a scope it does not index by the coherent observation of the tracepack storage specification §5,
// which lists the scope's commit objects, archive packs and the capture's staging segments, and computes the view with ActiveView;
// the catalog then confirms that the scope stayed not indexed.
// Every pack opened is checked against its key and its scope, and against the catalog's descriptor in an indexed scope.
// SegmentKey, ArchiveKey and CommitKey build the keys of the tracepack storage specification §3.
// The Godoc of NewStoreSource, ObjectStore and Catalog states the rules and the contracts the store and the catalog must keep.
package tracepack
