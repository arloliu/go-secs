package tracepack

import (
	"context"
	"errors"
	"fmt"
	"math"
	"runtime/debug"
	"time"

	"github.com/arloliu/go-secs/tracepack/internal/format"
)

const (
	// DefaultFlushInterval is the period of a segment when SegmentWriterOptions.FlushInterval is zero: 5 minutes.
	DefaultFlushInterval = 5 * time.Minute
	// DefaultMaxSegmentBytes is the size at which a SegmentWriter rolls a segment
	// when SegmentWriterOptions.MaxSegmentBytes is zero: 64 MiB.
	DefaultMaxSegmentBytes = 64 << 20
)

// tracepackModulePath is this module's path, which the writer tag reports the version of.
const tracepackModulePath = "github.com/arloliu/go-secs/tracepack"

// ErrInvalidRecord reports a record a SegmentWriter refuses before it touches a segment:
// an AppendFrame for a capture method whose frames it cannot describe,
// a time that is not representable,
// a capture-boundary event of kind start, stop or stop-unclean, which the SegmentWriter writes itself,
// a transport-event or annotation record whose field_validity is not 0 or whose payload does not decode,
// so Verify finds no writer defect in a record the SegmentWriter appended,
// a classifier's trailing_bytes outside 0 to 2^32-1,
// or a nil record or event.
// The record was not added and the SegmentWriter is still usable.
var ErrInvalidRecord = errors.New("tracepack: record refused by the segment writer")

// writerTag is the writer tag of every segment a SegmentWriter writes.
var writerTag = writerTagOf(debug.ReadBuildInfo())

// SegmentWriterOptions configures NewSegmentWriter.
type SegmentWriterOptions struct {
	// Capture describes the capture; required, and checked with CaptureDescriptor.Validate.
	// NewSegmentWriter copies it.
	Capture CaptureDescriptor
	// Sink stores the segments; required.
	Sink SegmentSink
	// FlushInterval is the length of a segment's period;
	// it must divide one hour, so every period lies inside one UTC hour.
	// Zero means DefaultFlushInterval.
	// It bounds how long a record stays in an open segment only while Tick is driven;
	// it is no durability guarantee.
	FlushInterval time.Duration
	// MaxSegmentBytes is the size, in bytes handed to the sink, at which a segment rolls before the next record;
	// zero means DefaultMaxSegmentBytes, and a negative value is refused.
	// The limit is soft: see SegmentWriter.
	MaxSegmentBytes int64
	// Classifier, when set, classifies every frame AppendFrame takes,
	// and its Name and MaxFrameLen are written as the classifier and max_frame_len tags of every segment.
	Classifier Classifier
	// Uncompressed writes blocks with CodecNone; otherwise they are written with CodecZstd.
	Uncompressed bool
	// BlockThreshold is WriterOptions.BlockThreshold for every segment.
	BlockThreshold int
	// SkipValidation is WriterOptions.SkipValidation for every segment.
	SkipValidation bool
	// Now returns the current time, which a zero CaptureOrigin takes and Close stamps the stop boundary with;
	// nil means time.Now.
	// It is also every segment's WriterOptions.Now, which stamps the segment's writer_start_utc_ns,
	// and it gives the time part of the capture_id and of every segment pack_id the SegmentWriter generates.
	Now func() time.Time
}

// SegmentWriter writes one capture, in capture order, as segments over a SegmentSink
// (the tracepack storage specification §4, Flush).
//
// It owns the pack metadata the CaptureDescriptor leaves out:
// the writer tag, classifier, max_frame_len, period_start, period_end, seq_start,
// pack_role segment, compaction_level 0 and scope_generation 0,
// a capture_id it generates, and the capture's seq counter, which starts at 0.
// Its segments carry no flush_interval_ns, since the sink is not a durable spool.
//
// Boundaries: NewSegmentWriter appends the capture's start boundary at CaptureOrigin, which opens the first segment,
// and Close appends the stop boundary at Now() before it finalizes the last segment;
// both are transport-event records of epoch 0, dir local, fidelity and decode_status not-applicable, without a seq range.
// For a capture-clock capture, start carries mono_ns 0 when CaptureOrigin has a monotonic reading,
// and stop carries mono_ns as AppendFrame sets it.
//
// Opening: a segment is opened by the first record that needs one.
// Its period is the FlushInterval holding the record's ts_utc_ns, inside the record's UTC hour,
// its seq_start the capture's next seq and its pack_id a fresh UUIDv7.
//
// Rolls: the open segment is finalized and committed
// before a record of another UTC hour than its period, in either direction;
// before a record of the same hour at or after the period's end;
// before a record the caller appends once the segment has handed MaxSegmentBytes or more to the sink;
// on Tick with a time at or after the period's end; and on Rotate.
// The size counts the file header, the pack metadata and the closed blocks, not the open block,
// so a segment can pass the limit by its open block, the record that closes it and its footer,
// and a record larger than the limit shares its segment with the records before it.
// A record of the period's hour before the period's start, after a backward clock step, stays in the open segment.
//
// Clock steps: for a capture-clock capture
// the SegmentWriter applies the clock anchor rule of the tracepack semantics specification §4 to every record with mono_ns,
// as WriterOptions.DetectClockSteps describes,
// and carries the anchor in force from one segment to the next,
// so a new segment never detects again a step an earlier one recorded.
// An inserted clock-step record goes into the segment of the record it precedes.
//
// Times: a time is representable when time.Unix(0, t.UnixNano()) equals it
// and its whole FlushInterval, start and exclusive end, fits in int64 nanoseconds.
// Nothing is clamped.
//
// Refusal: each append first checks the record without touching a segment:
// its time (ErrInvalidRecord), its payload size (ErrPayloadTooLarge), its field_validity (ErrFieldValidity),
// the commitments of the segments' metadata (ErrMetadataCommitment),
// the boundary kinds the SegmentWriter owns (ErrInvalidRecord), the event's encoding,
// a transport-event or annotation record's field_validity, which must be 0, and payload, which must decode (ErrInvalidRecord),
// so Verify finds no writer defect in a record the SegmentWriter appended,
// and the seqs left (ErrSeqOrder): one, or two when a clock-step record would precede it, up to 2^63-1.
// A refused record is not added, no segment is rolled or opened, and the SegmentWriter is still usable.
// So is a call whose ctx is done before its first sink or Writer call.
//
// Failure: after those checks any error, of a segment's Writer, of the sink or of ctx, fails the SegmentWriter.
// ctx is checked between sink calls, and once more before an append reports success,
// so a ctx done during an append's sink calls fails it even when every sink call succeeded.
// It aborts the open segment once, and returns the cause joined with any Abort error after it;
// a segment whose Commit returned nil is no longer open, so a later failure leaves it published.
// The error of a Commit that failed after the segment became visible wraps ErrPublishUncertain.
// The seq counter is never rewound, so lost records leave a seq gap, never a reused seq.
// Every later call returns an error wrapping ErrWriterFailed, and Close returns the failure without a sink call.
//
// A SegmentWriter is not safe for concurrent use.
type SegmentWriter struct {
	// desc is the descriptor, resolved, which the SegmentWriter owns.
	desc  CaptureDescriptor
	sink  SegmentSink
	now   func() time.Time
	flush int64
	// maxBytes is MaxSegmentBytes, the size at which a segment rolls before a caller's record.
	maxBytes   uint64
	classifier Classifier
	// params holds the writer, classifier and max_frame_len tags; each segment adds its period and seq_start.
	params      segmentMetaParams
	commitments packCommitments
	codec       Codec
	threshold   int
	skipValid   bool
	captureID   UUID
	// originMono reports that CaptureOrigin carries a monotonic reading.
	originMono bool
	// detect applies the clock anchor rule, with tolerance, to a capture-clock capture.
	detect    bool
	tolerance uint64
	// anchor is the clock anchor in force while no segment is open; an open segment's Writer holds the one in force.
	anchor clockAnchor
	// nextSeq is the capture's next seq.
	nextSeq uint64
	// seg is the open segment; nil when none is open.
	seg *openSegment
	// failure is the error that failed the SegmentWriter; nil while it is usable.
	failure error
	// closed is set once Close has run, with closeErr its result.
	closed   bool
	closeErr error
}

// openSegment is the segment a SegmentWriter is writing.
type openSegment struct {
	file SegmentFile
	w    *Writer
	out  *segmentOutput
	// hour is the UTC hour of the period [start, end).
	hour       int64
	start, end int64
}

// segmentOutput is the output of a segment's Writer.
// Until the sink has created the segment it holds what the Writer writes, the file header and the pack metadata,
// so every check NewWriter runs comes before the sink's Create;
// once the file is attached it passes every write and sync to the file,
// after checking ctx, the context of the SegmentWriter call writing through it,
// so a call whose ctx is done makes no further sink call.
type segmentOutput struct {
	file SegmentFile
	head []byte
	// ctx is set only during a SegmentWriter call, by use; no context outlives the call.
	ctx context.Context
}

// NewSegmentWriter checks opts, then writes the capture's start boundary, which opens the first segment.
//
// Everything is checked before the sink is called:
// the descriptor, the options, the classifier's MaxFrameLen, CaptureOrigin's representability and every pack metadata value.
//
// Parameters:
//   - ctx: checked before the first sink call, passed to the sink's Create,
//     then checked between sink calls and before NewSegmentWriter returns;
//     done once the first segment was created, it aborts that segment.
//   - opts: see SegmentWriterOptions.
//
// Returns:
//   - *SegmentWriter: the writer, its first segment open; nil on error.
//   - error: a *FieldError from CaptureDescriptor.Validate;
//     a *FieldError on max_frame_len wrapping ErrFieldValue for a classifier MaxFrameLen above 2^63-1,
//     or on classifier for an empty classifier Name;
//     an invalid option, a CaptureOrigin that is not representable or a pack metadata value the encoding refuses;
//     ctx's error; or an error of the sink or of writing the start boundary, after its segment was aborted.
func NewSegmentWriter(ctx context.Context, opts SegmentWriterOptions) (*SegmentWriter, error) {
	if err := opts.Capture.Validate(); err != nil {
		return nil, err
	}

	w, err := prepareSegmentWriter(&opts)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	origin := w.desc.CaptureOrigin
	start, err := w.boundaryRecord(BoundaryKindStart, origin, origin.UnixNano())
	if err != nil {
		return nil, err
	}
	if err := w.appendRecord(ctx, &start, false); err != nil {
		return nil, err
	}

	return w, nil
}

// prepareSegmentWriter checks the options, resolves the descriptor and returns the SegmentWriter, without a segment.
func prepareSegmentWriter(opts *SegmentWriterOptions) (*SegmentWriter, error) {
	if opts.Sink == nil {
		return nil, errors.New("tracepack: SegmentWriterOptions.Sink is nil")
	}

	flush := opts.FlushInterval
	if flush == 0 {
		flush = DefaultFlushInterval
	}
	// An interval that divides the hour is at most an hour long.
	if flush < 0 || time.Hour%flush != 0 {
		return nil, fmt.Errorf("tracepack: SegmentWriterOptions.FlushInterval %v does not divide one hour", flush)
	}

	maxBytes := opts.MaxSegmentBytes
	if maxBytes == 0 {
		maxBytes = DefaultMaxSegmentBytes
	}
	if maxBytes < 0 {
		return nil, fmt.Errorf("tracepack: SegmentWriterOptions.MaxSegmentBytes %d is negative", maxBytes)
	}

	params := segmentMetaParams{writer: writerTag}
	if c := opts.Classifier; c != nil {
		params.classifier, params.maxFrameLen = c.Name(), c.MaxFrameLen()
		if params.classifier == "" {
			return nil, &FieldError{Field: "classifier", Err: ErrFieldValue}
		}
		if params.maxFrameLen > format.MaxU64 {
			return nil, &FieldError{Field: "max_frame_len", Err: ErrFieldValue}
		}
	}

	now := clockOr(opts.Now)

	w := &SegmentWriter{
		desc:       opts.Capture.resolve(now()),
		sink:       opts.Sink,
		now:        now,
		flush:      int64(flush),
		maxBytes:   uint64(maxBytes),
		classifier: opts.Classifier,
		params:     params,
		commitments: packCommitments{
			hasClassifier:  params.classifier != "",
			hasMaxFrameLen: params.maxFrameLen > 0,
			packRole:       PackRoleSegment,
		},
		codec:     CodecZstd,
		threshold: opts.BlockThreshold,
		skipValid: opts.SkipValidation,
	}
	if opts.Uncompressed {
		w.codec = CodecNone
	}

	origin := w.desc.CaptureOrigin
	if _, ok := w.representable(origin); !ok {
		return nil, fmt.Errorf("tracepack: CaptureOrigin %v is not representable with a %v flush interval", origin, flush)
	}
	w.originMono = hasMonotonic(origin)
	if w.desc.TimeSource == TimeSourceCaptureClock {
		w.detect, w.tolerance = true, uint64(w.desc.ClockStepTolerance)
		w.anchor = clockAnchor{wall: origin.UnixNano()}
	}

	var err error
	if w.captureID, err = idOrNew(UUID{}, now); err != nil {
		return nil, err
	}

	return w, nil
}

// writerTagOf returns the writer tag for the build info of the running binary:
// tracepack/<version> of this module, as the main module or a dependency,
// tracepack/devel for a build without a module version, and tracepack/unknown without build info.
func writerTagOf(info *debug.BuildInfo, ok bool) string {
	const prefix = "tracepack/"
	if !ok || info == nil {
		return prefix + "unknown"
	}

	version := ""
	if info.Main.Path == tracepackModulePath {
		version = info.Main.Version
	} else {
		for _, dep := range info.Deps {
			if dep.Path == tracepackModulePath {
				version = dep.Version
				break
			}
		}
	}

	switch version {
	case "":
		return prefix + "unknown"
	case "(devel)":
		return prefix + "devel"
	}

	return prefix + version
}

// periodOf returns the period of length f, which divides one hour, that holds ts: [start, end), start a multiple of f,
// and false when the period does not fit in int64 nanoseconds.
func periodOf(ts, f int64) (int64, int64, bool) {
	q := ts / f
	if ts%f < 0 {
		q--
	}
	// math.MinInt64/f rounds toward zero, so it is the smallest q whose q*f is not below math.MinInt64.
	if q < math.MinInt64/f {
		return 0, 0, false
	}
	start := q * f
	if start > math.MaxInt64-f {
		return 0, 0, false
	}

	return start, start + f, true
}

// frameFidelity returns the fidelity of a frame captured by method m (the tracepack semantics specification §2),
// and false for a method whose frames AppendFrame cannot describe:
// a log's reconstructed frame holds placeholder bytes for identities its source lacks (the tracepack format specification §7.2),
// and an unknown method has no fidelity.
func frameFidelity(m CaptureMethod) (Fidelity, bool) {
	switch m {
	case CaptureMethodRawStream:
		return FidelityWireExact, true
	case CaptureMethodDecodedMessage:
		return FidelityReEncoded, true
	case CaptureMethodGenerator:
		return FidelitySynthesized, true
	case CaptureMethodUnknown, CaptureMethodLog:
		return FidelityUnknown, false
	}

	return FidelityUnknown, false
}

// hasMonotonic reports whether t carries a monotonic clock reading, which Round(0) strips.
func hasMonotonic(t time.Time) bool {
	return t != t.Round(0) //nolint:revive // Equal ignores the monotonic reading, which this comparison sees
}

// ownedBoundary reports whether ev is a capture boundary of a kind the SegmentWriter writes itself.
func ownedBoundary(ev *TransportEvent) bool {
	if ev == nil || ev.Event != EventCaptureBoundary || ev.BoundaryKind == nil {
		return false
	}
	switch *ev.BoundaryKind {
	case BoundaryKindStart, BoundaryKindStop, BoundaryKindStopUnclean:
		return true
	case BoundaryKindUnknown, BoundaryKindGap:
		return false
	}

	return false
}

// CaptureID returns the capture_id of every segment of the capture,
// which a recorder persists to give as the next capture's PreviousCaptureID.
func (w *SegmentWriter) CaptureID() UUID {
	return w.captureID
}

// AppendFrame appends a data or control record of a captured HSMS frame, length prefix included.
//
// The kind is control when the frame holds its SType byte, payload byte 9, and that byte is not 0, and data otherwise;
// field_validity marks the fields the frame's bytes hold;
// fidelity follows the capture method (wire-exact for raw-stream, re-encoded for decoded-message, synthesized for generator);
// ts_utc_ns is at's wall clock, and for a capture-clock capture mono_ns is at − CaptureOrigin when both carry a monotonic reading;
// decode_status and trailing_bytes come from the classifier, or are not-attempted and 0 without one.
// No quality bit is set but the derived ones (see CaptureDescriptor.QualityEvaluated).
// The classifier reads frame before AppendFrame copies it; the caller may reuse frame once AppendFrame returns.
//
// Returns:
//   - error: ErrInvalidRecord for a log capture method, whose reconstructed frames hold placeholder bytes for identities its source lacks,
//     for an unknown capture method, which has no fidelity rule,
//     for an at that is not representable, or for a classifier's trailing_bytes outside 0 to 2^32-1;
//     or a refusal or failure as SegmentWriter describes.
func (w *SegmentWriter) AppendFrame(ctx context.Context, at time.Time, dir Dir, epoch uint32, frame []byte) error {
	if err := w.usable(); err != nil {
		return err
	}

	fidelity, ok := frameFidelity(w.desc.CaptureMethod)
	if !ok {
		return fmt.Errorf("tracepack: AppendFrame for capture method %s: %w", w.desc.CaptureMethod, ErrInvalidRecord)
	}

	ts, err := w.recordTime(at)
	if err != nil {
		return err
	}

	r := Record{
		TSUTCNs: ts, Epoch: epoch, Kind: KindData, Dir: dir,
		Fidelity: fidelity, DecodeStatus: DecodeStatusNotAttempted, Payload: frame,
	}
	if len(frame) >= fieldSTypeEnd && frame[fieldSTypeOff] != 0 {
		r.Kind = KindControl
	}
	r.SetCapturedFieldValidity()
	r.MonoNs, r.MonoPresent = w.mono(at)
	if w.classifier != nil {
		status, trailing := w.classifier.Frame(frame)
		r.DecodeStatus = status
		if status == DecodeStatusOK || status == DecodeStatusOKWithTrailing {
			if trailing < 0 || uint64(trailing) > math.MaxUint32 {
				return fmt.Errorf("tracepack: classifier trailing_bytes %d outside 0 to %d: %w", trailing, uint32(math.MaxUint32), ErrInvalidRecord)
			}
			r.TrailingBytes = uint32(trailing)
		}
	}

	return w.appendRecord(ctx, &r, true)
}

// AppendEvent appends a transport-event record carrying ev:
// dir local, fidelity and decode_status not-applicable, ts_utc_ns and mono_ns from at as AppendFrame sets them.
// A capture-boundary event of kind gap is accepted; start, stop and stop-unclean are the SegmentWriter's own.
//
// Returns:
//   - error: ErrInvalidRecord for a nil ev, an at that is not representable, a capture boundary of kind start, stop or stop-unclean,
//     or an ev whose encoding does not decode;
//     the error of encoding ev; or a refusal or failure as SegmentWriter describes.
func (w *SegmentWriter) AppendEvent(ctx context.Context, at time.Time, epoch uint32, ev *TransportEvent) error {
	if err := w.usable(); err != nil {
		return err
	}
	if ev == nil {
		return fmt.Errorf("tracepack: nil transport event: %w", ErrInvalidRecord)
	}

	ts, err := w.recordTime(at)
	if err != nil {
		return err
	}
	payload, err := ev.MarshalBinary()
	if err != nil {
		return fmt.Errorf("tracepack: encode transport event: %w", err)
	}

	r := Record{
		TSUTCNs: ts, Epoch: epoch, Kind: KindTransportEvent, Dir: DirLocal,
		Fidelity: FidelityNotApplicable, DecodeStatus: DecodeStatusNotApplicable, Payload: payload,
	}
	r.MonoNs, r.MonoPresent = w.mono(at)

	return w.appendRecord(ctx, &r, true)
}

// Append appends a record the producer built, such as a log converter's:
// the SegmentWriter assigns its seq and takes every other field as given, without applying the classifier.
// It does not modify r, and the caller may reuse r's payload once Append returns.
//
// Returns:
//   - error: ErrInvalidRecord for a nil r, a ts_utc_ns that is not representable,
//     a capture boundary of kind start, stop or stop-unclean,
//     or a transport-event or annotation record whose field_validity is not 0 or whose payload does not decode;
//     or a refusal or failure as SegmentWriter describes.
func (w *SegmentWriter) Append(ctx context.Context, r *Record) error {
	if err := w.usable(); err != nil {
		return err
	}
	if r == nil {
		return fmt.Errorf("tracepack: nil record: %w", ErrInvalidRecord)
	}

	c := *r
	c.Seq = 0

	return w.appendRecord(ctx, &c, true)
}

// Tick finalizes and commits the open segment when now is at or after its period's end,
// so an idle segment is not held open; it does nothing without an open segment.
//
// Returns:
//   - error: ctx's error before any sink call, the SegmentWriter still usable; or a failure as SegmentWriter describes.
func (w *SegmentWriter) Tick(ctx context.Context, now time.Time) error {
	if err := w.usable(); err != nil {
		return err
	}
	if w.seg == nil || now.Before(time.Unix(0, w.seg.end)) {
		return nil
	}

	return w.roll(ctx)
}

// Rotate finalizes and commits the open segment; it does nothing without an open segment.
// The next record opens a segment for its own period.
//
// Returns:
//   - error: ctx's error before any sink call, the SegmentWriter still usable; or a failure as SegmentWriter describes.
func (w *SegmentWriter) Rotate(ctx context.Context) error {
	if err := w.usable(); err != nil {
		return err
	}
	if w.seg == nil {
		return nil
	}

	return w.roll(ctx)
}

// Close appends the stop boundary at Now(), then finalizes and commits the last segment.
//
// A Now() that is not representable fails Close, which aborts the open segment and writes no stop.
// When too few seqs are left for the stop boundary, Close finalizes and commits the open segment without it and fails.
// Once a Close has run to its end, a later Close returns its result without a sink call,
// as does a Close on a failed SegmentWriter, which returns the failure;
// a Close refused because ctx was done leaves the SegmentWriter usable, and can be retried.
//
// Returns:
//   - error: ctx's error before any sink call, the SegmentWriter still usable;
//     an error wrapping ErrSeqOrder when no seq is left for the stop boundary;
//     or a failure as SegmentWriter describes.
func (w *SegmentWriter) Close(ctx context.Context) error {
	if w.closed {
		return w.closeErr
	}
	if w.failure != nil {
		return w.failure
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	err := w.closeCapture(ctx)
	w.closed, w.closeErr = true, err

	return err
}

// closeCapture does the work of Close once ctx was checked.
func (w *SegmentWriter) closeCapture(ctx context.Context) error {
	now := w.now()
	ts, ok := w.representable(now)
	if !ok {
		return w.fail(fmt.Errorf("tracepack: Now() at Close, %v, is not representable", now))
	}

	stop, err := w.boundaryRecord(BoundaryKindStop, now, ts)
	if err != nil {
		return w.fail(err)
	}
	if err := w.preflight(&stop, false); err != nil {
		// The stop boundary's time is representable and it is the writer's own record, so only the seqs left refuse it.
		return w.closeWithout(ctx, err)
	}

	if err := w.place(ctx, ts, false); err != nil {
		return w.fail(err)
	}
	if err := w.appendToSegment(ctx, &stop); err != nil {
		return w.fail(err)
	}
	if err := w.finalize(ctx); err != nil {
		return w.fail(err)
	}

	return nil
}

// closeWithout finalizes and commits the open segment without the stop boundary that cause refused,
// and fails the SegmentWriter with cause.
func (w *SegmentWriter) closeWithout(ctx context.Context, cause error) error {
	if w.seg != nil {
		if err := w.finalize(ctx); err != nil {
			return w.fail(errors.Join(cause, err))
		}
	}
	w.failure = cause

	return cause
}

// usable reports why w cannot take a call, if it cannot.
func (w *SegmentWriter) usable() error {
	if w.failure != nil {
		return fmt.Errorf("%w: %w", ErrWriterFailed, w.failure)
	}
	if w.closed {
		return ErrClosed
	}

	return nil
}

// representable returns t's UnixNano when t is representable: when the nanoseconds hold t exactly
// and t's whole flush interval fits in int64 nanoseconds.
func (w *SegmentWriter) representable(t time.Time) (int64, bool) {
	utcNs := t.UnixNano()
	if !time.Unix(0, utcNs).Equal(t) {
		return 0, false
	}
	if _, _, ok := periodOf(utcNs, w.flush); !ok {
		return 0, false
	}

	return utcNs, true
}

// recordTime returns the ts_utc_ns of a record appended at at, or an error wrapping ErrInvalidRecord when at is not representable.
func (w *SegmentWriter) recordTime(at time.Time) (int64, error) {
	ts, ok := w.representable(at)
	if !ok {
		return 0, fmt.Errorf("tracepack: time %v is not representable with a %v flush interval: %w", at, time.Duration(w.flush), ErrInvalidRecord)
	}

	return ts, nil
}

// mono returns the mono_ns of a record at at, and whether it has one:
// at − CaptureOrigin for a capture-clock capture when both carry a monotonic reading (the tracepack semantics specification §4).
func (w *SegmentWriter) mono(at time.Time) (int64, bool) {
	if w.desc.TimeSource != TimeSourceCaptureClock || !w.originMono || !hasMonotonic(at) {
		return 0, false
	}

	return int64(at.Sub(w.desc.CaptureOrigin)), true
}

// boundaryRecord returns the capture-boundary record of kind the SegmentWriter writes at at, whose UnixNano is ts.
func (w *SegmentWriter) boundaryRecord(kind BoundaryKind, at time.Time, ts int64) (Record, error) {
	ev := TransportEvent{Event: EventCaptureBoundary, BoundaryKind: &kind}
	payload, err := ev.MarshalBinary()
	if err != nil {
		return Record{}, fmt.Errorf("tracepack: encode %s boundary: %w", kind, err)
	}

	r := Record{
		TSUTCNs: ts, Kind: KindTransportEvent, Dir: DirLocal,
		Fidelity: FidelityNotApplicable, DecodeStatus: DecodeStatusNotApplicable, Payload: payload,
	}
	r.MonoNs, r.MonoPresent = w.mono(at)

	return r, nil
}

// appendRecord appends r, a caller's record or one of the SegmentWriter's own boundaries:
// it checks r without touching a segment, then ctx, then rolls or opens the segment r needs and appends r to it.
// An error after the ctx check fails the SegmentWriter.
func (w *SegmentWriter) appendRecord(ctx context.Context, r *Record, caller bool) error {
	if err := w.usable(); err != nil {
		return err
	}
	if err := w.preflight(r, caller); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	if err := w.place(ctx, r.TSUTCNs, caller); err != nil {
		return w.fail(err)
	}
	if err := w.appendToSegment(ctx, r); err != nil {
		return w.fail(err)
	}
	w.nextSeq = w.seg.w.nextSeq

	return nil
}

// appendToSegment appends r to the open segment's Writer, whose writes and syncs check ctx before each sink call,
// then checks ctx again, so a call whose ctx was done during its sink calls does not report success.
func (w *SegmentWriter) appendToSegment(ctx context.Context, r *Record) error {
	done := w.seg.out.use(ctx)
	defer done()
	if err := w.seg.w.Append(r); err != nil {
		return err
	}

	return ctx.Err()
}

// preflight checks r as the segment's Writer will, and against what the SegmentWriter owns, without touching a segment:
// its period representable, its payload, the segments' commitments,
// for a caller's record an event or annotation's field_validity and payload and the boundary kinds the SegmentWriter owns,
// and the seqs left for it and for a clock-step record the Writer would insert before it.
func (w *SegmentWriter) preflight(r *Record, caller bool) error {
	if _, _, ok := periodOf(r.TSUTCNs, w.flush); !ok {
		return fmt.Errorf("tracepack: ts_utc_ns %d is not representable with a %v flush interval: %w", r.TSUTCNs, time.Duration(w.flush), ErrInvalidRecord)
	}
	if err := checkPayload(r); err != nil {
		return err
	}
	if err := w.commitments.checkCommitments(r); err != nil {
		return err
	}
	if caller {
		if err := checkEventRecord(r); err != nil {
			return err
		}
	}

	ev := transportEventOf(r)
	if caller && ownedBoundary(ev) {
		return fmt.Errorf("tracepack: capture boundary %s is written by the segment writer: %w", *ev.BoundaryKind, ErrInvalidRecord)
	}

	var slots uint64 = 1
	if w.detect {
		if _, step := clockDrift(r, ev, w.anchorInForce(), w.tolerance); step {
			slots = 2
		}
	}
	// nextSeq is at most 2^63, one past the last seq, so neither side wraps.
	if w.nextSeq > format.MaxU64+1-slots {
		return fmt.Errorf("tracepack: next seq %d leaves no %d seqs up to %d: %w", w.nextSeq, slots, format.MaxU64, ErrSeqOrder)
	}

	return nil
}

// checkEventRecord refuses, with ErrInvalidRecord, a transport-event or annotation record
// whose field_validity is not 0, which the tracepack format specification §7.2 requires of it and the Writer would drop,
// or whose payload does not decode (§8), which Verify would report as a writer defect.
// A reserved field_validity bit counts too: the Writer writes 0 for such a record, so it would drop the bit as well.
func checkEventRecord(r *Record) error {
	if r.Kind != KindTransportEvent && r.Kind != KindAnnotation {
		return nil
	}
	if r.FieldValidity != 0 {
		return fmt.Errorf("tracepack: %s record with field_validity %#02x, not 0: %w", r.Kind, uint8(r.FieldValidity), ErrInvalidRecord)
	}
	if err := checkEventPayload(r.Kind, r.Payload); err != nil {
		return fmt.Errorf("tracepack: %s record payload: %w: %w", r.Kind, ErrInvalidRecord, err)
	}

	return nil
}

// anchorInForce returns the clock anchor the next record is checked against.
func (w *SegmentWriter) anchorInForce() clockAnchor {
	if w.seg != nil {
		return w.seg.w.anchor()
	}

	return w.anchor
}

// place makes the segment a record at ts goes into the open one:
// it rolls the open segment when the record lies in another hour or at or after the period's end,
// or, for a caller's record, when the segment reached maxBytes; then it opens a segment when none is open.
func (w *SegmentWriter) place(ctx context.Context, ts int64, caller bool) error {
	if s := w.seg; s != nil {
		if hourOf(ts) != s.hour || ts >= s.end || caller && s.w.offset >= w.maxBytes {
			if err := w.finalize(ctx); err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
		}
	}
	if w.seg != nil {
		return nil
	}

	return w.open(ctx, ts)
}

// roll finalizes and commits the open segment for Tick and Rotate, after checking ctx.
func (w *SegmentWriter) roll(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := w.finalize(ctx); err != nil {
		return w.fail(err)
	}

	return nil
}

// open opens the segment of the record at ts: it builds its Writer, then has the sink create it and writes its head.
func (w *SegmentWriter) open(ctx context.Context, ts int64) error {
	start, end, _ := periodOf(ts, w.flush) // preflight checked the period
	packID, err := idOrNew(UUID{}, w.now)
	if err != nil {
		return err
	}

	p := w.params
	p.periodStart, p.periodEnd, p.seqStart = start, end, w.nextSeq
	out := &segmentOutput{}
	wr, err := startWriter(out, WriterOptions{
		Meta:             w.desc.packMeta(p),
		Facts:            p.facts(),
		Codec:            w.codec,
		BlockThreshold:   w.threshold,
		SkipValidation:   w.skipValid,
		Sync:             out,
		AssignSeq:        true,
		NextSeq:          w.nextSeq,
		PackID:           packID,
		CaptureID:        w.captureID,
		DetectClockSteps: w.detect,
		Now:              w.now,
		anchor:           &w.anchor,
	})
	if err != nil {
		return err
	}

	info := SegmentInfo{
		ToolID: w.desc.ToolID, CaptureID: w.captureID, PackID: packID, SeqFirst: w.nextSeq,
		Hour: hourOf(start), PeriodStart: time.Unix(0, start).UTC(), PeriodEnd: time.Unix(0, end).UTC(),
	}
	file, err := w.sink.Create(ctx, info)
	if err != nil {
		return fmt.Errorf("tracepack: create segment: %w", err)
	}
	if file == nil {
		return errors.New("tracepack: SegmentSink.Create returned no file")
	}
	w.seg = &openSegment{file: file, w: wr, out: out, hour: info.Hour, start: start, end: end}

	if err := ctx.Err(); err != nil {
		return err
	}

	return out.attach(file)
}

// finalize closes the open segment's Writer, then commits the segment and detaches it;
// on an error the segment stays open, for fail to abort.
func (w *SegmentWriter) finalize(ctx context.Context) error {
	s := w.seg
	done := s.out.use(ctx)
	defer done()
	next, err := s.w.Close()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.file.Commit(ctx); err != nil {
		return fmt.Errorf("tracepack: commit segment: %w", err)
	}

	w.seg = nil
	w.nextSeq = next
	if w.detect {
		w.anchor = s.w.anchor()
	}

	return nil
}

// fail fails the SegmentWriter with cause: it aborts the open segment, if any, joins any Abort error after cause,
// and keeps the result as the failure every later call reports.
func (w *SegmentWriter) fail(cause error) error {
	err := cause
	if s := w.seg; s != nil {
		w.seg = nil
		if aerr := s.file.Abort(); aerr != nil {
			err = errors.Join(cause, fmt.Errorf("tracepack: abort segment: %w", aerr))
		}
	}
	w.failure = err

	return err
}

// Write buffers p until the segment's file is attached, then writes it to the file unless ctx is done.
func (o *segmentOutput) Write(p []byte) (int, error) {
	if o.file == nil {
		o.head = append(o.head, p...)
		return len(p), nil
	}
	if err := o.ctx.Err(); err != nil {
		return 0, err
	}

	return o.file.Write(p)
}

// Sync syncs the segment's file unless ctx is done;
// the Writer syncs only after a block or the trailer, once the file is attached.
func (o *segmentOutput) Sync() error {
	if err := o.ctx.Err(); err != nil {
		return err
	}

	return o.file.Sync()
}

// use makes ctx the context o checks until the function it returns runs.
func (o *segmentOutput) use(ctx context.Context) (done func()) {
	o.ctx = ctx

	return func() { o.ctx = nil }
}

// attach makes file the output and writes to it the head buffered so far.
func (o *segmentOutput) attach(file SegmentFile) error {
	o.file = file
	head := o.head
	o.head = nil
	if _, err := writeFull(file, head); err != nil {
		return fmt.Errorf("tracepack: write file header and pack metadata: %w", err)
	}

	return nil
}
