package tracepack

import (
	"context"
	"fmt"
)

// End states of a capture, as its per-capture evidence records them (the tracepack storage specification §5).
const (
	// EndOpen means no stop or stop-unclean boundary is recorded for the capture.
	EndOpen EndState = iota
	// EndStopped means the capture ended with a clean stop boundary.
	EndStopped
	// EndStoppedUnclean means the capture ended with a stop-unclean boundary, written by recovery.
	EndStoppedUnclean
)

// EndState is the end state of a capture.
//
// Any stop-unclean boundary recorded for the capture makes it EndStoppedUnclean, also beside a clean stop;
// otherwise a clean stop makes it EndStopped.
// The zero value is EndOpen; String prints an undefined value as "unknown(<n>)".
type EndState uint8

var _ fmt.Stringer = EndState(0)

// PackSource opens observations of one tool's scopes and per-capture evidence for FindTransaction
// (the tracepack storage specification §5, Observation of a lookup).
// One source serves one tool.
type PackSource interface {
	// Observe opens one observation of capture over the hours [from, to), fixed before it returns.
	// It returns no Observation with an error.
	Observe(ctx context.Context, capture UUID, from, to int64) (Observation, error)
}

// Observation answers, for one lookup, from one observation of a capture's scopes, its per-capture evidence and the tool's barriers
// (the tracepack storage specification §5, Observation of a lookup).
//
// The observation is immutable.
// PackSource.Observe fixes, before it returns, the view of every scope of its hours, the per-capture evidence and the tool's barriers;
// every later Scope, Evidence and Barriers call answers from what it fixed, never from a newer state,
// and a scope never switches from one source of its view to another, also when it is evicted meanwhile.
// A scope the catalog indexes is fixed from one catalog snapshot, which the evidence and barriers come from too,
// reflecting every registration the catalog completed before it.
// A scope the catalog does not index is fixed inside Observe by the complete coherent observation of the tracepack storage specification §5
// and its listing view, reported with Indexed false;
// the source establishes that procedure's premises, or Observe returns an error.
// A pack of a fixed view stays readable only as far as the storage specification's Deletion rules allow;
// a pack deleted meanwhile fails its read, which ends the lookup with that error.
//
// The source does not retry inside an observation: a failed call returns an error, which ends the lookup.
// Every value a method returns is the caller's:
// SourceScope.Readers a fresh slice, CaptureEvidence and the Boundary slices deep copies, their GapStart and GapEnd pointers fresh.
// The source never changes them afterwards, and FindTransaction never changes them either.
// Readers that Scope returned stay valid until Close.
// FindTransaction uses an Observation from one goroutine,
// and calls Close exactly once, after its last use, whatever happened.
type Observation interface {
	// Scope returns the fixed view of the scope (capture, hour), hour in the observation's [from, to):
	// its packs and whether the catalog indexes it.
	Scope(ctx context.Context, hour int64) (SourceScope, error)
	// Evidence returns the capture's per-capture evidence.
	Evidence(ctx context.Context) (CaptureEvidence, error)
	// Barriers returns every stop-unclean boundary of a capture of the tool whose gap interval [GapStart, GapEnd] meets [from, to),
	// in nanoseconds, a nil bound being unbounded;
	// an interval whose GapStart exceeds its GapEnd meets every range.
	// The range must be non-empty and lie within the observation's hours,
	// [from·3600·10⁹, to·3600·10⁹) for the from and to of Observe;
	// any other range is an error.
	Barriers(ctx context.Context, from, to int64) ([]Boundary, error)
	// Close releases the observation; the Readers it returned are not used after it.
	// It is bounded: it never waits on I/O or on other callers,
	// and a source whose release needs I/O starts it in the background.
	// It is idempotent, and valid after any method failed.
	Close() error
}

// SourceScope is the fixed view of one scope of an Observation.
type SourceScope struct {
	// Readers holds a Reader for each pack of the scope's view, distinct pack_ids, in view order;
	// empty for an hour without packs.
	Readers []*Reader
	// Indexed reports whether the catalog indexes the scope;
	// a scope not indexed was fixed by a coherent observation and its listing view.
	Indexed bool
}

// EpochClosure is the closure of one epoch that the per-capture evidence records:
// the smallest close_seq of the epoch over the packs it was taken from (the tracepack format specification §10).
// It is a footer's claim, which a lookup checks against the record it names and never uses as a closure by itself.
type EpochClosure struct {
	Epoch    uint32
	CloseSeq uint64
}

// CaptureEvidence is the per-capture evidence of one capture (the tracepack storage specification §5):
// what the catalog recorded from the footers of every pack presented for registration, rejected ones included.
type CaptureEvidence struct {
	// End is the capture's end state.
	End EndState
	// Boundaries holds every capture-boundary entry recorded for the capture, in ascending seq.
	// AddPackEvidence keeps each distinct entry once, but entries of one seq may differ,
	// and a source that builds evidence otherwise may repeat an entry.
	Boundaries []Boundary
	// Closures holds one closure per epoch that has one, in ascending epoch.
	Closures []EpochClosure
	// Partial reports that the source could not establish all of the evidence,
	// for instance because a pack's footer is not valid.
	Partial bool
}

// Boundary is one capture-boundary entry of a footer (the tracepack format specification §10),
// as per-capture evidence or as a barrier records it.
type Boundary struct {
	// Capture is the capture_id of the pack the entry was taken from.
	Capture UUID
	// Seq, Kind, TS and Epoch are the boundary record's seq, boundary_kind, ts_utc_ns and epoch.
	Seq   uint64
	Kind  BoundaryKind
	TS    int64
	Epoch uint32
	// GapStart and GapEnd are the entry's gap_start and gap_end; nil when absent.
	GapStart, GapEnd *int64
}

// cloneBoundary returns a copy of b with fresh GapStart and GapEnd pointers.
func cloneBoundary(b Boundary) Boundary {
	if b.GapStart != nil {
		b.GapStart = new(*b.GapStart)
	}
	if b.GapEnd != nil {
		b.GapEnd = new(*b.GapEnd)
	}

	return b
}

// barrierMeets reports whether the gap interval [GapStart, GapEnd] of the stop-unclean boundary b meets the time range [from, to)
// (the tracepack storage specification §5, Completeness):
// a nil bound is unbounded,
// and an interval whose GapStart exceeds its GapEnd meets every range, as an inverted coverage entry does.
func barrierMeets(b *Boundary, from, to int64) bool {
	return inverted(b.GapStart, b.GapEnd) || overlaps(b.GapStart, b.GapEnd, &from, &to)
}

// String returns the end state's name in the tracepack storage specification, or "unknown(<n>)" for a value this package does not define.
func (s EndState) String() string {
	switch s {
	case EndOpen:
		return "open"
	case EndStopped:
		return "stopped"
	case EndStoppedUnclean:
		return "stopped-unclean"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(s))
	}
}
