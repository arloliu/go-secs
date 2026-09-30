package tracepack

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/arloliu/go-secs/tracepack/internal/codec"
	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// DefaultBlockThreshold is the uncompressed block size at which a Writer closes a block
// when WriterOptions.BlockThreshold is zero: 4 MiB (the tracepack format specification §6).
const DefaultBlockThreshold = 4 << 20

// fileHeaderRedactionPresent is file header flag bit 0, redaction-present (the tracepack format specification §4).
const fileHeaderRedactionPresent uint32 = 1

// Sentinel errors returned by Writer.
var (
	// ErrSeqOrder reports a record seq that is not above the previous record's,
	// a first record whose seq is not the pack's seq_start,
	// or a seq above the 2^63-1 limit of the tracepack format specification §2.
	// A writer never renumbers a seq it received (the tracepack format specification I-12),
	// so a caller holding an earlier record opens another Writer for it.
	ErrSeqOrder = errors.New("tracepack: record seq out of order")
	// ErrPayloadTooLarge reports a record payload too large for any block:
	// a block's uncompressed_len, 44 header bytes plus the payload for a record alone,
	// must not exceed 2^31-1 (the tracepack format specification §2).
	ErrPayloadTooLarge = errors.New("tracepack: record payload too large")
	// ErrFieldValidity reports a data or control record whose field_validity marks an HSMS header field
	// whose bytes its payload lacks, a writer defect every writer rejects when the record is appended
	// (the tracepack format specification §7.2).
	// The record was not added and the Writer is still usable.
	ErrFieldValidity = errors.New("tracepack: field_validity marks a field the payload lacks")
	// ErrMetadataCommitment reports a contradiction with what the pack metadata or the file header commits to,
	// wrapped in a *FieldError naming the commitment.
	//
	// Append returns it for a record the committed metadata cannot describe:
	// a classified decode_status in a pack without classifier ("classifier"),
	// an oversized record in a pack without max_frame_len ("max_frame_len"),
	// or a quality.redacted record in a pack that is not an extract ("pack_role")
	// or whose file header does not set redaction-present ("redaction-present").
	// The record was not added and the Writer is still usable.
	//
	// NewWriter returns it when WriterOptions.Facts.AnyRedacted would set redaction-present in a pack that is not an extract ("redaction-present");
	// it writes nothing and returns no Writer.
	//
	// Close returns it when the file header set redaction-present but no written record carries quality.redacted ("redaction-present");
	// it writes neither footer nor trailer, and the Writer has failed.
	ErrMetadataCommitment = errors.New("tracepack: record contradicts the pack metadata")
	// ErrValidation reports that an encoded block failed the checks a validating Writer runs before writing it
	// (the tracepack format specification §12: the decoded body against I-2);
	// the block was not written and the Writer has failed.
	ErrValidation = errors.New("tracepack: block failed validation")
	// ErrWriterFailed reports a call on a Writer that an earlier failure left unusable:
	// a failed validation, a failed or short write, or a failed sync.
	// A failure before Close writes the footer leaves the pack unfinalized;
	// a failure of the footer-and-trailer write or of the sync after it may leave a finalized pack,
	// which the Writer still reports as failed, since it cannot vouch for it.
	// Either way the caller discards the output and starts a new pack.
	ErrWriterFailed = errors.New("tracepack: writer failed")
	// ErrClosed reports a call on a Writer after Close.
	ErrClosed = errors.New("tracepack: writer closed")
)

// Syncer flushes written bytes to stable storage, such as *os.File.
//
// A Writer's output and its durability operation are separate capabilities (the tracepack format specification §12):
// a Writer without a Syncer makes no durability promise.
type Syncer interface {
	Sync() error
}

// WriterOptions configures NewWriter.
type WriterOptions struct {
	// Meta is the pack metadata written at the start of the pack; required.
	// NewWriter copies it and does not modify it; the Writer sets seq_start from NextSeq when AssignSeq is set.
	Meta *PackMeta
	// Facts are the facts about the pack's records and scope that Meta is validated against.
	// Facts.AnyRedacted also sets the file header's redaction-present flag,
	// without which Append rejects a record carrying quality.redacted.
	Facts PackFacts
	// Codec compresses block bodies.
	// The zero value is CodecNone;
	// the tracepack format specification §2 recommends CodecZstd.
	Codec Codec
	// BlockThreshold is the uncompressed block size, in bytes, at which a block is closed;
	// zero means DefaultBlockThreshold.
	// A block never exceeds it, except a block holding a single record larger than it.
	BlockThreshold int
	// Validate makes the Writer decode every encoded block and check it against I-2 before writing it
	// (the tracepack format specification §12, which recommends it);
	// it commits nothing to the pack metadata, since a reader checks every block it reads.
	Validate bool
	// Sync, when set, is called after every block is written and after the trailer.
	Sync Syncer
	// AssignSeq makes the Writer assign each record's seq, starting from NextSeq;
	// otherwise the caller sets Record.Seq, starting at Meta.SeqStart and strictly increasing, with gaps allowed.
	AssignSeq bool
	// NextSeq is the first seq the Writer assigns when AssignSeq is set.
	NextSeq uint64
	// PackID identifies the pack file; the zero UUID makes NewWriter generate a UUIDv7.
	PackID UUID
	// CaptureID identifies the capture; the zero UUID makes NewWriter generate a UUIDv7.
	// A capture that rolls into several packs passes the same CaptureID to each Writer.
	CaptureID UUID
	// DetectClockSteps makes the Writer apply the clock anchor rule of the tracepack semantics specification §4 to every record with MonoPresent set.
	// The anchor starts at (Meta.CaptureOriginUTCNs, 0);
	// when a record's drift (TSUTCNs − anchor wall) − (MonoNs − anchor mono) exceeds Meta.ClockStepToleranceNs in magnitude,
	// the Writer closes and writes the open block, writes a clock-step transport event whose clock_step_ns is the drift,
	// with the record's timestamps and epoch, alone in a block of its own,
	// and takes it as the new anchor before it appends the record.
	// A clock-step event with mono that the caller appends is not checked:
	// the Writer closes the block holding it, so it is written and synced, and then takes it as the anchor.
	// It requires a capture-clock pack and AssignSeq, since the event takes the seq before the record's.
	// Without it the Writer stores what it receives, including a producer's clock-step events.
	DetectClockSteps bool
}

// Writer writes one tracepack file: the file header and pack metadata when it is created,
// then its records in blocks, and on Close the footer and the trailer (the tracepack format specification §12).
//
// A Writer is not safe for concurrent use.
// Any failed write, sync or validation leaves it failed:
// every later call returns an error wrapping ErrWriterFailed, and the caller discards the output (see ErrWriterFailed).
type Writer struct {
	out       io.Writer
	packID    UUID
	captureID UUID
	syncer    Syncer
	codec     Codec
	threshold int
	validate  bool
	assignSeq bool
	// detectSteps enables the clock anchor rule, with tolerance and the anchor (anchorWall, anchorMono).
	detectSteps bool
	tolerance   uint64
	anchorWall  int64
	anchorMono  int64
	// The written pack metadata and file header commit to hasClassifier, hasMaxFrameLen, packRole and redactionPresent;
	// every record appended must agree with them.
	hasClassifier    bool
	hasMaxFrameLen   bool
	packRole         PackRole
	redactionPresent bool
	// seqStart is the pack's seq_start, which the first record's seq must equal.
	seqStart uint64
	// nextSeq is the smallest seq the next record may carry, and the seq AssignSeq gives it.
	nextSeq uint64
	// offset is the file offset of the next byte written.
	offset uint64
	block  blockBuilder
	// blocks summarizes every block written, in file order, for the footer.
	blocks []blockSummary
	// bodyBuf, encBuf and valBuf are reused across blocks.
	bodyBuf []byte
	encBuf  []byte
	valBuf  validateBuf
	// encodedHook, set only by tests, changes an encoded block body before it is validated and written.
	encodedHook func(enc []byte) []byte
	// maxF3ListLen is the longest F-3 entry list the footer's u32 summary_len can state;
	// tests lower it to reach the check.
	maxF3ListLen uint64
	closed       bool
	// failure is the error that left the Writer failed; nil while it is usable.
	failure error
}

// NewWriter validates the options, then writes the file header and the pack metadata to w
// (the tracepack format specification §4, §5 and §12 step 1).
//
// With opts.AssignSeq the pack metadata's seq_start is opts.NextSeq.
//
// Parameters:
//   - w: the output; bytes are written in file order and never rewritten.
//   - opts: see WriterOptions.
//
// Returns:
//   - *Writer: the Writer, positioned after the pack metadata; nil on error.
//   - error: a *FieldError from PackMeta.Validate, an invalid option,
//     a *FieldError wrapping ErrMetadataCommitment when opts.Facts.AnyRedacted is set in a pack that is not an extract,
//     or the error of writing the header,
//     which is io.ErrShortWrite when w takes fewer bytes than it was given without reporting an error.
func NewWriter(w io.Writer, opts WriterOptions) (*Writer, error) {
	var err error
	if opts.CaptureID, err = idOrNew(opts.CaptureID); err != nil {
		return nil, err
	}

	return startWriter(w, opts)
}

// startWriter does the work of NewWriter, but writes opts.CaptureID as given, the nil UUID included,
// so a pack written from another pack keeps that pack's capture exactly.
func startWriter(w io.Writer, opts WriterOptions) (*Writer, error) {
	if err := checkOptions(&opts); err != nil {
		return nil, err
	}

	meta := *opts.Meta
	if opts.AssignSeq {
		meta.SeqStart = opts.NextSeq
	}

	var err error
	if opts.PackID, err = idOrNew(opts.PackID); err != nil {
		return nil, err
	}

	head, err := encodeHead(&meta, &opts)
	if err != nil {
		return nil, err
	}

	if _, err := writeFull(w, head); err != nil {
		return nil, fmt.Errorf("tracepack: write file header and pack metadata: %w", err)
	}

	threshold := opts.BlockThreshold
	if threshold == 0 {
		threshold = DefaultBlockThreshold
	}

	wr := &Writer{
		out:         w,
		packID:      opts.PackID,
		captureID:   opts.CaptureID,
		syncer:      opts.Sync,
		codec:       opts.Codec,
		threshold:   threshold,
		validate:    opts.Validate,
		assignSeq:   opts.AssignSeq,
		detectSteps: opts.DetectClockSteps,
		seqStart:    meta.SeqStart,
		nextSeq:     meta.SeqStart,
		offset:      uint64(len(head)),

		maxF3ListLen: math.MaxUint32,

		hasClassifier:    len(meta.Classifiers) > 0,
		hasMaxFrameLen:   len(meta.MaxFrameLens) > 0,
		packRole:         meta.PackRole,
		redactionPresent: opts.Facts.AnyRedacted,
	}
	if opts.DetectClockSteps {
		// checkOptions made this a capture-clock pack, whose metadata carries both values.
		wr.tolerance = *meta.ClockStepToleranceNs
		wr.anchorWall = *meta.CaptureOriginUTCNs
	}

	return wr, nil
}

// checkOptions rejects options NewWriter cannot honor.
func checkOptions(opts *WriterOptions) error {
	if opts.Meta == nil {
		return errors.New("tracepack: WriterOptions.Meta is nil")
	}
	if !codec.Known(uint8(opts.Codec)) {
		return fmt.Errorf("tracepack: WriterOptions.Codec %s is not a codec a writer may use", opts.Codec)
	}
	if opts.BlockThreshold < 0 || int64(opts.BlockThreshold) > int64(format.MaxLen32) {
		return fmt.Errorf("tracepack: WriterOptions.BlockThreshold %d outside 0 to %d", opts.BlockThreshold, format.MaxLen32)
	}

	seqStart := opts.Meta.SeqStart
	if opts.AssignSeq {
		seqStart = opts.NextSeq
	}
	if seqStart > format.MaxU64 {
		return fmt.Errorf("tracepack: seq_start %d above %d: %w", seqStart, format.MaxU64, ErrSeqOrder)
	}

	if err := checkClockOptions(opts); err != nil {
		return err
	}

	return opts.Meta.Validate(opts.Facts)
}

// checkClockOptions rejects DetectClockSteps where the Writer cannot apply the clock anchor rule.
// Validating the metadata of a capture-clock pack then guarantees capture_origin_utc_ns and clock_step_tolerance_ns.
func checkClockOptions(opts *WriterOptions) error {
	if !opts.DetectClockSteps {
		return nil
	}
	if !opts.AssignSeq {
		return errors.New("tracepack: WriterOptions.DetectClockSteps requires AssignSeq, so a clock-step record can take the seq before its record")
	}
	if opts.Meta.TimeSource != TimeSourceCaptureClock {
		return fmt.Errorf("tracepack: WriterOptions.DetectClockSteps requires time_source capture-clock, not %s", opts.Meta.TimeSource)
	}

	return nil
}

// encodeHead encodes the file header followed by meta, with the pack and capture ids of opts.
func encodeHead(meta *PackMeta, opts *WriterOptions) ([]byte, error) {
	metaBytes, err := meta.MarshalBinary()
	if err != nil {
		return nil, err
	}
	if uint64(len(metaBytes)) > math.MaxUint32 {
		return nil, fmt.Errorf("tracepack: pack metadata of %d bytes exceeds its u32 length field", len(metaBytes))
	}

	h := format.FileHeader{
		FormatMajor:      format.FormatMajor,
		PackMetadataLen:  uint32(len(metaBytes)),
		PackMetadataCRC:  format.CRC(metaBytes),
		WriterStartUTCNs: time.Now().UnixNano(),
		PackID:           format.UUID(opts.PackID),
		CaptureID:        format.UUID(opts.CaptureID),
	}
	if opts.Facts.AnyRedacted {
		// The flag is set only in extracts (the tracepack format specification §4).
		if meta.PackRole != PackRoleExtract {
			return nil, &FieldError{Field: "redaction-present", Err: ErrMetadataCommitment}
		}
		h.Flags |= fileHeaderRedactionPresent
	}

	head := format.AppendFileHeader(make([]byte, 0, format.FileHeaderLen+len(metaBytes)), &h)

	return append(head, metaBytes...), nil
}

// writeFull writes b to w and returns what w reports it wrote;
// a write that takes fewer bytes than b without an error is io.ErrShortWrite,
// since the rest of b would otherwise be silently missing from the file.
func writeFull(w io.Writer, b []byte) (int, error) {
	n, err := w.Write(b)
	if err == nil && n < len(b) {
		err = io.ErrShortWrite
	}

	return n, err
}

// idOrNew returns id, or a new UUIDv7 when id is the nil UUID
// (the tracepack format specification §2 recommends UUIDv7 for pack_id and capture_id).
func idOrNew(id UUID) (UUID, error) {
	if !id.IsZero() {
		return id, nil
	}

	u, err := format.GenerateUUIDv7(time.Now, rand.Reader)
	if err != nil {
		return UUID{}, fmt.Errorf("tracepack: generate UUIDv7: %w", err)
	}

	return UUID(u), nil
}

// canonicalHeader builds the record header the Writer stores for r under seq,
// where ev is r's decoded transport-event payload, or nil:
// mono_ns is 0 unless MonoPresent,
// field_validity is 0 for a record that is neither data nor control,
// record_flags is derived from MonoPresent,
// quality.capture-boundary is derived, overriding r.Quality,
// and quality.correlation-incomplete is added for epoch 0.
// Retired and reserved bits of quality and field_validity are cleared, so the block summary's quality_union covers only stored bits.
func canonicalHeader(r *Record, seq uint64, ev *TransportEvent) format.RecordHeader {
	h := format.RecordHeader{
		Seq:           seq,
		TSUTCNs:       r.TSUTCNs,
		Epoch:         r.Epoch,
		PayloadLen:    uint32(len(r.Payload)),
		TrailingBytes: r.TrailingBytes,
		Kind:          uint8(r.Kind),
		Dir:           uint8(r.Dir),
		Fidelity:      uint8(r.Fidelity),
		DecodeStatus:  uint8(r.DecodeStatus),
	}
	if r.hasHSMSFrame() {
		h.FieldValidity = uint8(r.FieldValidity & fieldValidityMask)
	}

	q := r.Quality & qualityMask &^ QualityCaptureBoundary
	if ev != nil && ev.Event == EventCaptureBoundary {
		q |= QualityCaptureBoundary
	}
	// Epoch 0 is unknown, which forces correlation-incomplete (the tracepack format specification I-7);
	// the bit has other causes too, so a caller's bit on another epoch is kept.
	if r.Epoch == 0 {
		q |= QualityCorrelationIncomplete
	}
	if r.MonoPresent {
		h.MonoNs = r.MonoNs
		h.RecordFlags = uint8(RecordFlagsMonoPresent)
	}
	h.Quality = uint16(q)

	return h
}

// subSat returns a − b, saturated to the int64 range.
func subSat(a, b int64) int64 {
	d := a - b
	// The difference overflows only when a and b differ in sign and d's sign differs from a's.
	if (a >= 0) != (b >= 0) && (d >= 0) != (a >= 0) {
		if a >= 0 {
			return math.MaxInt64
		}

		return math.MinInt64
	}

	return d
}

// magnitude returns |v|, which for math.MinInt64 only a uint64 holds.
func magnitude(v int64) uint64 {
	if v < 0 {
		return uint64(-(v + 1)) + 1
	}

	return uint64(v)
}

// isClockStep reports whether ev is a clock-step event.
func isClockStep(ev *TransportEvent) bool {
	return ev != nil && ev.Event == EventClockStep
}

// transportEventOf decodes r's payload when r is a transport-event record, for the block summary.
// A payload that does not decode contributes nothing to the summary; its bytes are still stored as received.
func transportEventOf(r *Record) *TransportEvent {
	if r.Kind != KindTransportEvent {
		return nil
	}

	ev, err := UnmarshalTransportEvent(r.Payload)
	if err != nil {
		return nil
	}

	return ev
}

// Append adds r to the pack.
//
// It never rejects a record for its time.
// It closes the open block first when r's ts_utc_ns lies in another UTC hour than the block's records
// (the tracepack format specification I-13),
// or when r would push the block past the size threshold;
// a record larger than the threshold is written alone in its own block.
// With WriterOptions.AssignSeq, Append assigns the next seq and stores it in r.Seq;
// otherwise r.Seq must equal seq_start for the first record and exceed the previous seq after that.
// It rejects a data or control record whose field_validity marks a field its payload lacks;
// a clear bit over present bytes is kept, as a log conversion needs (the tracepack format specification §7.2).
// Append copies r.Payload and does not modify any other field of r:
// the header it stores is canonical as described on Record, whatever r holds.
// With WriterOptions.DetectClockSteps, a record whose drift exceeds the tolerance is preceded by a clock-step record,
// written with the seq before r's in a block of its own after the open block is closed;
// a clock-step event r with mono closes the block holding it and becomes the anchor once written.
// Closing a block writes it, and with WriterOptions.Validate checks it first,
// so Append may return the error of the block it closed.
//
// Returns:
//   - error: ErrPayloadTooLarge, ErrFieldValidity, ErrSeqOrder, or a *FieldError wrapping ErrMetadataCommitment,
//     with r not added and the Writer still usable;
//     an error wrapping ErrValidation or a write or sync error, after which the Writer has failed;
//     ErrWriterFailed or ErrClosed on a failed or closed Writer.
func (w *Writer) Append(r *Record) error {
	if err := w.usable(); err != nil {
		return err
	}

	if len(r.Payload) > maxPayloadLen {
		return fmt.Errorf("tracepack: payload of %d bytes above %d: %w", len(r.Payload), maxPayloadLen, ErrPayloadTooLarge)
	}
	if r.hasHSMSFrame() {
		if beyond := r.FieldValidity & fieldValidityMask &^ capturedFields(len(r.Payload)); beyond != 0 {
			return fmt.Errorf("tracepack: field_validity marks %s, but the payload is %d bytes: %w", beyond, len(r.Payload), ErrFieldValidity)
		}
	}

	if err := w.checkCommitments(r); err != nil {
		return err
	}

	seq, err := w.seqFor(r)
	if err != nil {
		return err
	}

	ev := transportEventOf(r)
	if drift, step := w.clockStep(r, ev); step {
		if seq >= format.MaxU64 {
			return fmt.Errorf("tracepack: seq %d leaves no seq for the clock-step record before it: %w", seq, ErrSeqOrder)
		}
		if err := w.writeClockStep(r, seq, drift); err != nil {
			return err
		}
		seq++
	}

	recordLen := recordHeaderLen + len(r.Payload)
	if !w.block.empty() && (hourOf(r.TSUTCNs) != w.block.hour || exceedsLimit(w.block.size(), recordLen, w.threshold)) {
		if err := w.closeBlock(); err != nil {
			return err
		}
	}

	h := canonicalHeader(r, seq, ev)
	w.block.add(&h, r.Payload, ev)
	w.nextSeq = seq + 1
	if w.assignSeq {
		r.Seq = seq
	}
	if w.detectSteps && r.MonoPresent && isClockStep(ev) {
		return w.adoptClockStep(r)
	}

	if w.block.size() >= w.threshold {
		return w.closeBlock()
	}

	return nil
}

// Flush closes the open block, writing it and, with a Syncer, syncing it; it does nothing when no record is pending.
//
// Returns:
//   - error: as for Append's block close; ErrWriterFailed or ErrClosed on a failed or closed Writer.
func (w *Writer) Flush() error {
	if err := w.usable(); err != nil {
		return err
	}
	if w.block.empty() {
		return nil
	}

	return w.closeBlock()
}

// Close flushes the open block, then writes the footer and the trailer that finalize the pack,
// and with a Syncer syncs after the trailer (the tracepack format specification §10 to §12).
//
// The trailer is the commit: a failed Writer writes neither footer nor trailer, so its pack stays unfinalized;
// but an error from the footer-and-trailer write itself, or from the sync after it, may come after the trailer reached the output.
// Close does not close the underlying output.
//
// Returns:
//   - uint64: the seq after the pack's last record, or its seq_start when it holds no record;
//     a capture that continues in another pack starts it there. 0 on error.
//   - error: as for Flush, or the error of writing or syncing the footer and trailer, after which the Writer has failed;
//     an error, before the footer is written, for a block summary too long for the footer's u32 summary_len;
//     a *FieldError wrapping ErrMetadataCommitment, also leaving the Writer failed,
//     when the file header set redaction-present but no written record carries quality.redacted;
//     ErrWriterFailed on a failed Writer, and ErrClosed when already closed.
func (w *Writer) Close() (uint64, error) {
	if err := w.Flush(); err != nil {
		return 0, err
	}

	if err := w.finalize(); err != nil {
		w.failure = err
		return 0, err
	}

	w.closed = true

	return w.nextSeq, nil
}

// PackID returns the pack_id written in the file header:
// WriterOptions.PackID, or the UUIDv7 NewWriter generated.
func (w *Writer) PackID() UUID {
	return w.packID
}

// CaptureID returns the capture_id written in the file header:
// WriterOptions.CaptureID, or the UUIDv7 NewWriter generated.
func (w *Writer) CaptureID() UUID {
	return w.captureID
}

// usable reports why w cannot take a call, if it cannot.
func (w *Writer) usable() error {
	if w.failure != nil {
		return fmt.Errorf("%w: %w", ErrWriterFailed, w.failure)
	}
	if w.closed {
		return ErrClosed
	}

	return nil
}

// checkCommitments rejects r when the pack metadata and file header already written cannot describe it
// (the tracepack format specification §4 and §5, the tracepack semantics specification §8).
func (w *Writer) checkCommitments(r *Record) error {
	classified := r.DecodeStatus != DecodeStatusNotAttempted && r.DecodeStatus != DecodeStatusNotApplicable

	return w.checkCommitmentFacts(classified, r.DecodeStatus == DecodeStatusOversized, r.Quality.Has(QualityRedacted))
}

// checkCommitmentFacts rejects records that are classified, oversized or redacted, as the flags say,
// when the pack metadata and file header already written cannot describe them.
func (w *Writer) checkCommitmentFacts(classified, oversized, redacted bool) error {
	var field string
	switch {
	case classified && !w.hasClassifier:
		field = "classifier"
	case oversized && !w.hasMaxFrameLen:
		field = "max_frame_len"
	case redacted && w.packRole != PackRoleExtract:
		field = "pack_role"
	case redacted && !w.redactionPresent:
		field = "redaction-present"
	default:
		return nil
	}

	return &FieldError{Field: field, Err: ErrMetadataCommitment}
}

// seqFor returns the seq r is written under: the next assigned seq, or r.Seq checked against the order rules.
func (w *Writer) seqFor(r *Record) (uint64, error) {
	seq := w.nextSeq
	if !w.assignSeq {
		seq = r.Seq
	}

	if err := w.checkSeq(seq); err != nil {
		return 0, err
	}

	return seq, nil
}

// checkSeq checks seq, the seq of the next record written, against I-12:
// the pack's first seq is seq_start, every seq is above the last seq written, and none is above 2^63-1.
func (w *Writer) checkSeq(seq uint64) error {
	switch {
	case seq > format.MaxU64:
		return fmt.Errorf("tracepack: seq %d above %d: %w", seq, format.MaxU64, ErrSeqOrder)
	case len(w.blocks) == 0 && w.block.empty() && seq != w.seqStart:
		return fmt.Errorf("tracepack: first seq %d, seq_start is %d: %w", seq, w.seqStart, ErrSeqOrder)
	case seq < w.nextSeq:
		return fmt.Errorf("tracepack: seq %d after seq %d: %w", seq, w.nextSeq-1, ErrSeqOrder)
	}

	return nil
}

// clockStep applies the clock anchor rule of the tracepack semantics specification §4 to r:
// it reports whether r's drift against the anchor exceeds the tolerance, and the drift.
// A record without mono, and a clock-step event, which becomes the anchor itself, are never a step.
func (w *Writer) clockStep(r *Record, ev *TransportEvent) (int64, bool) {
	if !w.detectSteps || !r.MonoPresent || isClockStep(ev) {
		return 0, false
	}

	drift := subSat(subSat(r.TSUTCNs, w.anchorWall), subSat(r.MonoNs, w.anchorMono))

	return drift, magnitude(drift) > w.tolerance
}

// writeClockStep closes and writes the open block,
// then writes a clock-step transport event for r under seq, alone in its own block,
// so it is durable before r is appended (the tracepack semantics specification §4),
// and takes it as the new clock anchor.
// The event carries r's timestamps and epoch, and clock_step_ns = drift.
func (w *Writer) writeClockStep(r *Record, seq uint64, drift int64) error {
	if !w.block.empty() {
		if err := w.closeBlock(); err != nil {
			return err
		}
	}

	ev := &TransportEvent{Event: EventClockStep, ClockStepNs: &drift}
	payload, err := ev.MarshalBinary()
	if err != nil {
		return fmt.Errorf("tracepack: encode clock-step event: %w", err)
	}

	step := Record{
		TSUTCNs: r.TSUTCNs, MonoNs: r.MonoNs, MonoPresent: true, Epoch: r.Epoch,
		Kind: KindTransportEvent, Dir: DirLocal, Fidelity: FidelityNotApplicable, DecodeStatus: DecodeStatusNotApplicable,
		Payload: payload,
	}
	h := canonicalHeader(&step, seq, ev)
	w.block.add(&h, step.Payload, ev)
	w.nextSeq = seq + 1

	return w.adoptClockStep(&step)
}

// adoptClockStep closes the open block, which holds r, a clock-step event,
// and only once that block is written and synced takes r as the new clock anchor:
// the anchor moves only at a durable clock-step event (the tracepack semantics specification §4).
func (w *Writer) adoptClockStep(r *Record) error {
	if err := w.closeBlock(); err != nil {
		return err
	}

	w.anchorWall, w.anchorMono = r.TSUTCNs, r.MonoNs

	return nil
}

// closeBlock encodes the open block, validates it when WriterOptions.Validate is set,
// writes envelope and body, syncs, and records the block's summary.
// Any failure leaves the Writer failed, so no trailer can follow a block that was not written whole.
func (w *Writer) closeBlock() error {
	if err := w.writeBlock(); err != nil {
		w.failure = err
		return err
	}

	w.block.reset()

	return nil
}

// writeBlock does the work of closeBlock, returning its first error.
func (w *Writer) writeBlock() error {
	s := &w.block.summary
	w.bodyBuf = w.block.body(w.bodyBuf)
	uncompressedLen := len(w.bodyBuf)

	enc, c, err := encodeBlock(w.codec, w.encBuf, w.bodyBuf)
	if err != nil {
		return fmt.Errorf("tracepack: encode block: %w", err)
	}
	if c == CodecZstd {
		w.encBuf = enc
	}
	if w.encodedHook != nil {
		enc = w.encodedHook(enc)
	}

	if w.validate {
		if err := validateEncoded(c, &w.valBuf, enc, s, uncompressedLen); err != nil {
			return fmt.Errorf("%w: block of seq %d-%d: %w", ErrValidation, s.firstSeq, s.lastSeq, err)
		}
	}

	env := format.BlockEnvelope{
		Codec:           uint8(c),
		RecordHeaderLen: recordHeaderLen,
		BodyLen:         uint32(len(enc)),
		UncompressedLen: uint32(uncompressedLen),
		RecordCount:     s.recordCount,
		BodyCRC:         format.CRC(enc),
		FirstSeq:        s.firstSeq,
	}

	var envBuf [format.EnvelopeLen]byte
	if err := w.write(format.AppendBlockEnvelope(envBuf[:0], &env), "block"); err != nil {
		return err
	}
	if err := w.write(enc, "block"); err != nil {
		return err
	}

	return w.finishBlock(*s, &env)
}

// finishBlock syncs the block just written, whose envelope is env, and keeps s, the summary of its records,
// for the footer, placed as env and the block's position say.
func (w *Writer) finishBlock(s blockSummary, env *format.BlockEnvelope) error {
	if w.syncer != nil {
		if err := w.syncer.Sync(); err != nil {
			return fmt.Errorf("tracepack: sync block: %w", err)
		}
	}

	onDiskLen := format.EnvelopeLen + uint64(env.BodyLen)
	s.offset = w.offset - onDiskLen
	s.onDiskLen = uint32(onDiskLen)
	s.uncompressedLen = env.UncompressedLen
	s.bodyCRC = env.BodyCRC
	s.recordHeaderLen = env.RecordHeaderLen
	w.blocks = append(w.blocks, s)

	return nil
}

// appendBlock writes raw, the envelope and on-disk body of a block a full read validated in another pack, byte for byte,
// and keeps a copy of s, the summary its records give (summaryOf), for the footer (the tracepack format specification §10).
// The block keeps its codec and record header length whatever the Writer's options.
//
// It requires no open block, and checks from s what Append checks of each record:
// I-12 (the pack's first seq is seq_start, every block's first seq is above the last seq written, no seq above 2^63-1),
// I-13 (the block's records lie in one UTC hour), and the commitments of the written pack metadata and file header.
// raw's envelope must agree with s on record_count and first_seq, and hold exactly the body that follows it.
// Any error leaves the Writer failed.
func (w *Writer) appendBlock(raw []byte, s *blockSummary) error {
	if err := w.usable(); err != nil {
		return err
	}

	if err := w.writeRawBlock(raw, s); err != nil {
		w.failure = err
		return err
	}

	return nil
}

// writeRawBlock does the work of appendBlock, returning its first error.
func (w *Writer) writeRawBlock(raw []byte, s *blockSummary) error {
	env, err := w.checkAppendBlock(raw, s)
	if err != nil {
		return err
	}
	if err := w.write(raw, "block"); err != nil {
		return err
	}
	if err := w.finishBlock(s.clone(), &env); err != nil {
		return err
	}
	w.nextSeq = s.lastSeq + 1

	return nil
}

// checkAppendBlock returns raw's envelope once raw and s pass the checks of appendBlock.
func (w *Writer) checkAppendBlock(raw []byte, s *blockSummary) (format.BlockEnvelope, error) {
	if !w.block.empty() {
		return format.BlockEnvelope{}, errors.New("tracepack: append a block while a block is open")
	}
	env, err := format.UnmarshalBlockEnvelope(raw)
	if err != nil {
		return format.BlockEnvelope{}, fmt.Errorf("tracepack: append a block: %w", err)
	}
	switch {
	case uint64(len(raw)) != format.EnvelopeLen+uint64(env.BodyLen):
		return format.BlockEnvelope{}, fmt.Errorf("tracepack: block of %d bytes, its envelope states a %d-byte body", len(raw), env.BodyLen)
	case env.RecordCount != s.recordCount || env.FirstSeq != s.firstSeq:
		return format.BlockEnvelope{}, fmt.Errorf("tracepack: block envelope states %d records from seq %d, its summary %d from seq %d",
			env.RecordCount, env.FirstSeq, s.recordCount, s.firstSeq)
	}
	if err := w.checkSeq(s.firstSeq); err != nil {
		return format.BlockEnvelope{}, err
	}
	switch {
	case s.lastSeq > format.MaxU64:
		return format.BlockEnvelope{}, fmt.Errorf("tracepack: seq %d above %d: %w", s.lastSeq, format.MaxU64, ErrSeqOrder)
	case hourOf(s.tsMin) != hourOf(s.tsMax):
		return format.BlockEnvelope{}, fmt.Errorf("tracepack: block of seq %d-%d has records in more than one UTC hour", s.firstSeq, s.lastSeq)
	}

	if err := w.checkCommitmentFacts(s.commitmentFacts()); err != nil {
		return format.BlockEnvelope{}, err
	}

	return env, nil
}

// finalize writes the footer built from the written blocks, encoded with the Writer's codec,
// then the trailer, then syncs (the tracepack format specification §12 step 3).
// It writes nothing when the header set redaction-present but the pack's quality_union lacks quality.redacted:
// the flag promised a redacted record, so the pack cannot be finalized under it (the tracepack format specification §4).
func (w *Writer) finalize() error {
	if uint64(len(w.blocks)) > math.MaxUint32 {
		return fmt.Errorf("tracepack: %d blocks exceed the u32 block_count", len(w.blocks))
	}

	footer, stats, err := buildFooter(w.blocks, w.maxF3ListLen)
	if err != nil {
		return err
	}
	if w.redactionPresent && !stats.qualityUnion.Has(QualityRedacted) {
		return &FieldError{Field: "redaction-present", Err: ErrMetadataCommitment}
	}

	enc, err := codec.Encode(uint8(w.codec), nil, footer)
	if err != nil {
		return fmt.Errorf("tracepack: encode footer: %w", err)
	}

	tr := format.Trailer{
		FooterOffset:          w.offset,
		FooterLen:             uint64(len(enc)),
		FooterUncompressedLen: uint64(len(footer)),
		BlockCount:            uint32(len(w.blocks)),
		FooterCRC:             format.CRC(enc),
		RecordCount:           stats.recordCount,
		TrailerVersion:        format.TrailerVersion,
		FooterCodec:           uint8(w.codec),
	}
	if n := len(w.blocks); n > 0 {
		tr.LastSeq = w.blocks[n-1].lastSeq
	}

	if err := w.write(format.AppendTrailer(enc, &tr), "footer and trailer"); err != nil {
		return err
	}
	if w.syncer != nil {
		if err := w.syncer.Sync(); err != nil {
			return fmt.Errorf("tracepack: sync trailer: %w", err)
		}
	}

	return nil
}

// write writes b, the named part of the file, in full and advances the file offset by what was written.
// A short write is an error, so the caller fails the Writer rather than write past a gap.
func (w *Writer) write(b []byte, what string) error {
	n, err := writeFull(w.out, b)
	w.offset += uint64(n)
	if err != nil {
		return fmt.Errorf("tracepack: write %s: %w", what, err)
	}

	return nil
}
