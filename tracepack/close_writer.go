package tracepack

import (
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// CloseOptions configures WriteClose.
type CloseOptions struct {
	// Meta is the pack metadata of the close segment; required.
	// It holds the capture's descriptor, as DecodeDescriptor returns it,
	// and the consumer-owned values every segment of the capture carries:
	// writer, scope_generation, flush_interval_ns, and a period inside the UTC hour of Trigger,
	// and for a capture masked at write its redaction_policy, classifier and max_frame_len
	// (the tracepack storage specification §4, Closing a capture left open).
	// The capture's time_source is capture-clock, so Meta carries clock_step_tolerance_ns.
	// WriteClose writes it with pack_role segment and seq_start 2^63-1, whatever Meta holds for them;
	// it does not modify Meta.
	Meta *PackMeta
	// CaptureID is the capture closed; required, not the nil UUID.
	CaptureID UUID
	// Trigger is the close's ts_utc_ns: the successor's start ts_utc_ns when a successor triggers the close,
	// else the log service's wall clock; it must be representable as int64 nanoseconds.
	Trigger time.Time
	// HighestSeqTS is the ts_utc_ns of the record with the highest seq the service staged for the capture,
	// which is not necessarily the latest timestamp it staged;
	// nil when no record of the capture is staged.
	HighestSeqTS *int64
	// PackID identifies the pack file; the zero UUID makes WriteClose generate a UUIDv7.
	PackID UUID
	// Sync, when set, is called after the block and after the trailer, as WriterOptions.Sync is.
	Sync Syncer
	// Now returns the time the file header's writer_start_utc_ns is stamped with,
	// and the time part of a pack_id WriteClose generates; nil means time.Now.
	Now func() time.Time
}

// WriteClose writes to w the close segment of a capture its producer left open
// (the tracepack storage specification §4, Closing a capture left open):
// one stop-unclean capture-boundary record with seq 2^63-1, the last seq of a capture, alone in a segment.
//
// The record is a transport event at Trigger, dir local, epoch 0, without mono,
// fidelity and decode_status not-applicable, quality capture-boundary and correlation-incomplete;
// its gap_start is HighestSeqTS minus clock_step_tolerance_ns, absent without HighestSeqTS,
// and its gap_end Trigger plus clock_step_tolerance_ns,
// each computed in checked int64 arithmetic and absent when the computation leaves the int64 range.
// The segment's format_minor follows the tracepack format specification §14:
// 2 for metadata naming its recorder instance (RecorderInstanceName), else 1 for a capture masked at write or thinned, else 0.
//
// The block and the footer are written with CodecNone, so the bytes depend on no compressor:
// equal options with a fixed PackID and Now write equal bytes, also when the retry runs a newer tracepack,
// so a retried close is byte-identical and two closes of one capture never conflict.
// tracepack v0.8.0 and earlier wrote a zstd close by default,
// so a close first written by one of them and retried by this version writes other bytes (see the module's CHANGELOG).
// As for a Writer, the trailer is the commit: an error before it leaves the output unfinalized,
// while an error of the sync after it may leave a finalized pack that is not durable.
//
// Parameters:
//   - w: the output; bytes are written in file order and never rewritten.
//   - opts: see CloseOptions.
//
// Returns:
//   - UUID: the pack_id of the pack, returned also with any error after the CloseOptions and Trigger checks,
//     a NewWriter refusal included, though nothing may have been written;
//     the nil UUID with an error of those checks or of generating the pack_id.
//   - error: an error for invalid options, writing nothing;
//     an error wrapping ErrScopeBreach when Trigger lies outside the UTC hour of the period, writing nothing;
//     an error of NewWriter for metadata it refuses, such as a *FieldError, writing nothing;
//     or the error of writing or syncing the pack, as Writer.Append and Writer.Close return it.
//     An error opts determines also wraps ErrRefused.
func WriteClose(w io.Writer, opts CloseOptions) (UUID, error) {
	rec, meta, err := closeRecord(&opts)
	if err != nil {
		return UUID{}, refuse(err)
	}
	packID, err := idOrNew(opts.PackID, opts.Now)
	if err != nil {
		return UUID{}, refuse(err)
	}

	wr, err := NewWriter(w, WriterOptions{
		Meta:         meta,
		Facts:        PackFacts{DurableRecorder: true, RedactionPolicyApplied: meta.RedactionPolicy != nil},
		Uncompressed: true,
		Sync:         opts.Sync,
		PackID:       packID,
		CaptureID:    opts.CaptureID,
		Now:          opts.Now,
	})
	if err != nil {
		return packID, refuse(err)
	}
	if err := wr.Append(rec); err != nil {
		return packID, refuse(err)
	}
	if _, err := wr.Close(); err != nil {
		return packID, refuse(err)
	}

	return packID, nil
}

// closeRecord checks opts and returns the close record and the pack metadata of its segment.
func closeRecord(opts *CloseOptions) (*Record, *PackMeta, error) {
	switch {
	case opts.Meta == nil:
		return nil, nil, argument(errors.New("tracepack: CloseOptions.Meta is nil"))
	case opts.CaptureID.IsZero():
		return nil, nil, argument(errors.New("tracepack: CloseOptions.CaptureID is the nil UUID"))
	case opts.Meta.TimeSource != TimeSourceCaptureClock:
		return nil, nil, argument(fmt.Errorf("tracepack: close of a capture of time_source %s, not capture-clock", opts.Meta.TimeSource))
	case opts.Meta.ClockStepToleranceNs == nil:
		return nil, nil, argument(errors.New("tracepack: close of a capture without clock_step_tolerance_ns"))
	case !time.Unix(0, opts.Trigger.UnixNano()).Equal(opts.Trigger):
		return nil, nil, argument(fmt.Errorf("tracepack: close trigger %v is not representable as int64 nanoseconds", opts.Trigger))
	}

	meta := *opts.Meta
	meta.PackRole, meta.SeqStart = PackRoleSegment, format.MaxU64

	triggerNs := opts.Trigger.UnixNano()
	// The Writer refuses a record of another hour than the period's only after it wrote the file header.
	if hourOf(triggerNs) != hourOf(meta.PeriodStart) {
		return nil, nil, fmt.Errorf("tracepack: close trigger at %d ns in UTC hour %d, the period in hour %d: %w",
			triggerNs, hourOf(triggerNs), hourOf(meta.PeriodStart), ErrScopeBreach)
	}

	ev := &TransportEvent{Event: EventCaptureBoundary, BoundaryKind: new(BoundaryKindStopUnclean)}
	// A u64 pack metadata value is at most 2^63-1, so the tolerance fits an int64 in any metadata the Writer accepts.
	if tol := *meta.ClockStepToleranceNs; tol <= math.MaxInt64 {
		if opts.HighestSeqTS != nil {
			ev.GapStart = subChecked(*opts.HighestSeqTS, int64(tol))
		}
		ev.GapEnd = addChecked(triggerNs, int64(tol))
	}
	payload, err := ev.MarshalBinary()
	if err != nil {
		return nil, nil, fmt.Errorf("tracepack: encode close event: %w", err)
	}

	rec := &Record{
		Seq: format.MaxU64, TSUTCNs: triggerNs, Kind: KindTransportEvent, Dir: DirLocal,
		Fidelity: FidelityNotApplicable, DecodeStatus: DecodeStatusNotApplicable, Payload: payload,
	}

	return rec, &meta, nil
}

// addChecked returns a + b, or nil when the sum leaves the int64 range; b is not negative.
func addChecked(a, b int64) *int64 {
	if a > math.MaxInt64-b {
		return nil
	}

	return new(a + b)
}

// subChecked returns a - b, or nil when the difference leaves the int64 range; b is not negative.
func subChecked(a, b int64) *int64 {
	if a < math.MinInt64+b {
		return nil
	}

	return new(a - b)
}
