package tracepack

import "context"

// PackSource gives FindTransaction the packs of each hour it reads (the tracepack semantics specification §7.2):
// the packs its caller gives for that hour, of any capture.
// NewReaderSource returns one over packs the caller opened.
type PackSource interface {
	// Readers returns a Reader for each pack given for the UTC hour hour, of any capture, distinct pack_ids,
	// in a fresh slice, which FindTransaction never changes; empty for an hour without packs.
	// The Readers stay valid while the lookup runs.
	Readers(ctx context.Context, hour int64) ([]*Reader, error)
}

// Boundary is one capture-boundary entry of a footer (the tracepack format specification §10),
// or the capture-boundary record it summarizes.
type Boundary struct {
	// Capture is the capture_id of the pack the entry or record was taken from.
	Capture UUID
	// Seq, Kind, TS and Epoch are the boundary record's seq, boundary_kind, ts_utc_ns and epoch.
	Seq   uint64
	Kind  BoundaryKind
	TS    int64
	Epoch uint32
	// GapStart and GapEnd are the entry's gap_start and gap_end; nil when absent.
	GapStart, GapEnd *int64
}
