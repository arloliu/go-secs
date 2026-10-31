package tracepack

import "context"

// PackSource gives FindTransaction and FindTransactions the packs of each hour they read
// (the tracepack semantics specification §7.2):
// the packs its caller gives for that hour, of any capture.
// NewReaderSource returns one over packs the caller opened.
//
// A lookup reads the packs Readers returns for an hour only during that hour's read.
// Once it enters its next Readers call, or once the public call returns when there is no next call,
// it issues no read of them and has no read in flight on them,
// on success, error and cancellation alike.
// A cancelled ctx alone is not that point:
// a ReadAt in flight takes no context and may still run until the read joins it.
// After that point the lookup still uses each Reader's decoded header and metadata, which need no reads,
// so the Reader values stay valid until the call returns,
// while the caller may close the objects backing that hour's Readers, such as their files.
// The promise covers that hour's observation in that call only:
// an object the source also returns for another hour, or shares with another lookup,
// must not be closed while those may read it.
type PackSource interface {
	// Readers returns a Reader for each pack given for the UTC hour hour, of any capture, distinct pack_ids,
	// in a fresh slice, which FindTransaction never changes; empty for an hour without packs.
	// The Readers stay valid until the lookup returns;
	// the objects backing them may be closed sooner, as PackSource describes.
	Readers(ctx context.Context, hour int64) ([]*Reader, error)
}

// Boundary is one capture-boundary entry of a footer (the tracepack format specification §10),
// or the capture-boundary record it summarizes.
type Boundary struct {
	// CaptureID is the capture_id of the pack the entry or record was taken from.
	CaptureID UUID
	// Seq, Kind, TS and Epoch are the boundary record's seq, boundary_kind, ts_utc_ns and epoch.
	Seq   uint64
	Kind  BoundaryKind
	TS    int64
	Epoch uint32
	// GapStart and GapEnd are the entry's gap_start and gap_end; nil when absent.
	GapStart, GapEnd *int64
}
