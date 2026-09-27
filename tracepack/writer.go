package tracepack

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math"
	"time"

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
	// a block's uncompressed_len, 56 header bytes plus the payload for a record alone,
	// must not exceed 2^31-1 (the tracepack format specification §2).
	ErrPayloadTooLarge = errors.New("tracepack: record payload too large")
	// ErrValidation reports that an encoded block failed the checks a validating Writer runs before writing it
	// (the tracepack format specification §12);
	// the block was not written and the Writer has failed.
	ErrValidation = errors.New("tracepack: block failed validation")
	// ErrWriterFailed reports a call on a Writer that an earlier failure left unusable:
	// a failed validation, a failed write or a failed sync.
	// The pack never becomes finalized; a caller starts a new pack instead.
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
	// NewWriter copies it and does not modify it:
	// the Writer sets blocks_validated itself from Validate,
	// and seq_start from NextSeq when AssignSeq is set.
	Meta *PackMeta
	// Facts are the facts about the pack's records and scope that Meta is validated against.
	// Facts.AnyRedacted also sets the file header's redaction-present flag.
	Facts PackFacts
	// Codec compresses block bodies.
	// The zero value is CodecNone;
	// the tracepack format specification §2 recommends CodecZstd.
	Codec Codec
	// BlockThreshold is the uncompressed block size, in bytes, at which a block is closed;
	// zero means DefaultBlockThreshold.
	// A block never exceeds it, except a block holding a single record larger than it.
	BlockThreshold int
	// Validate makes the Writer attest its blocks:
	// it commits blocks_validated = true in the pack metadata,
	// and decodes and checks every encoded block before writing it (the tracepack format specification §12).
	Validate bool
	// Sync, when set, is called after every block is written.
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
}

// Writer writes one tracepack file: the file header and pack metadata when it is created,
// then its records in blocks (the tracepack format specification §12).
//
// A Writer is not safe for concurrent use.
// Any failed write, sync or validation leaves it failed:
// every later call returns an error wrapping ErrWriterFailed, and the pack is never finalized.
type Writer struct {
	out       io.Writer
	syncer    Syncer
	codec     Codec
	threshold int
	validate  bool
	assignSeq bool
	// seqStart is the pack's seq_start, which the first record's seq must equal.
	seqStart uint64
	// nextSeq is the smallest seq the next record may carry, and the seq AssignSeq gives it.
	nextSeq uint64
	// offset is the file offset of the next byte written.
	offset uint64
	block  blockBuilder
	// blocks summarizes every block written, in file order, for the footer.
	blocks []blockSummary
	// bodyBuf, encBuf and decBuf are reused across blocks.
	bodyBuf []byte
	encBuf  []byte
	decBuf  []byte
	closed  bool
	// failure is the error that left the Writer failed; nil while it is usable.
	failure error
}

// NewWriter validates the options, then writes the file header and the pack metadata to w
// (the tracepack format specification §4, §5 and §12 step 1).
//
// With opts.Validate the pack metadata commits blocks_validated = true before any block is written;
// without it blocks_validated is absent, whatever opts.Meta holds.
// With opts.AssignSeq the pack metadata's seq_start is opts.NextSeq.
//
// Parameters:
//   - w: the output; bytes are written in file order and never rewritten.
//   - opts: see WriterOptions.
//
// Returns:
//   - *Writer: the Writer, positioned after the pack metadata; nil on error.
//   - error: a *FieldError from PackMeta.Validate, an invalid option, or the error of writing the header.
func NewWriter(w io.Writer, opts WriterOptions) (*Writer, error) {
	if err := checkOptions(&opts); err != nil {
		return nil, err
	}

	meta := *opts.Meta
	meta.BlocksValidated = nil
	if opts.Validate {
		validated := true
		meta.BlocksValidated = &validated
	}
	if opts.AssignSeq {
		meta.SeqStart = opts.NextSeq
	}

	head, err := encodeHead(&meta, &opts)
	if err != nil {
		return nil, err
	}

	if _, err := w.Write(head); err != nil {
		return nil, fmt.Errorf("tracepack: write file header and pack metadata: %w", err)
	}

	threshold := opts.BlockThreshold
	if threshold == 0 {
		threshold = DefaultBlockThreshold
	}

	return &Writer{
		out:       w,
		syncer:    opts.Sync,
		codec:     opts.Codec,
		threshold: threshold,
		validate:  opts.Validate,
		assignSeq: opts.AssignSeq,
		seqStart:  meta.SeqStart,
		nextSeq:   meta.SeqStart,
		offset:    uint64(len(head)),
	}, nil
}

// checkOptions rejects options NewWriter cannot honor.
func checkOptions(opts *WriterOptions) error {
	if opts.Meta == nil {
		return errors.New("tracepack: WriterOptions.Meta is nil")
	}
	if opts.Codec != CodecNone && opts.Codec != CodecZstd {
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

	return opts.Meta.Validate(opts.Facts)
}

// encodeHead encodes the file header followed by meta, generating any zero pack or capture id.
func encodeHead(meta *PackMeta, opts *WriterOptions) ([]byte, error) {
	metaBytes, err := meta.MarshalBinary()
	if err != nil {
		return nil, err
	}
	if len(metaBytes) > math.MaxUint32 {
		return nil, fmt.Errorf("tracepack: pack metadata of %d bytes exceeds its u32 length field", len(metaBytes))
	}

	packID, err := idOrNew(opts.PackID)
	if err != nil {
		return nil, err
	}
	captureID, err := idOrNew(opts.CaptureID)
	if err != nil {
		return nil, err
	}

	h := format.FileHeader{
		FormatMajor:      format.FormatMajor,
		PackMetadataLen:  uint32(len(metaBytes)),
		PackMetadataCRC:  format.CRC(metaBytes),
		WriterStartUTCNs: time.Now().UnixNano(),
		PackID:           format.UUID(packID),
		CaptureID:        format.UUID(captureID),
	}
	if opts.Facts.AnyRedacted {
		h.Flags |= fileHeaderRedactionPresent
	}

	head := format.AppendFileHeader(make([]byte, 0, format.FileHeaderLen+len(metaBytes)), &h)

	return append(head, metaBytes...), nil
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

// canonicalHeader builds the record header the Writer stores for r under seq:
// mono_ns is 0 unless MonoPresent,
// every copy field whose field_validity bit is clear is zero,
// as is every copy field of a record that is neither data nor control,
// record_flags is derived from W and MonoPresent,
// and quality.no-mono and quality.decode-failed are derived, overriding r.Quality.
func canonicalHeader(r *Record, seq uint64) format.RecordHeader {
	c := *r
	if !c.hasCopies() {
		c.FieldValidity = 0
	}
	c.zeroUnavailableCopies()

	h := format.RecordHeader{
		Seq:           seq,
		TSUTCNs:       c.TSUTCNs,
		Epoch:         c.Epoch,
		PayloadLen:    uint32(len(c.Payload)),
		TrailingBytes: c.TrailingBytes,
		SystemBytes:   c.SystemBytes,
		SessionID:     c.SessionID,
		Stream:        c.Stream,
		Function:      c.Function,
		PType:         c.PType,
		SType:         c.SType,
		Kind:          uint8(c.Kind),
		Dir:           uint8(c.Dir),
		Fidelity:      uint8(c.Fidelity),
		DecodeStatus:  uint8(c.DecodeStatus),
		FieldValidity: uint8(c.FieldValidity),
	}

	q := c.Quality &^ (QualityNoMono | QualityDecodeFailed)
	var flags RecordFlags
	if c.MonoPresent {
		h.MonoNs = c.MonoNs
		flags |= RecordFlagsMonoPresent
	} else {
		q |= QualityNoMono
	}
	if c.DecodeStatus.Malformed() {
		q |= QualityDecodeFailed
	}
	if c.W {
		flags |= RecordFlagsW
	}

	h.Quality = uint16(q)
	h.RecordFlags = uint8(flags)

	return h
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
// Append copies r.Payload and does not modify any other field of r:
// the header it stores is canonical as described on Record, whatever r holds.
// Closing a block writes it, and with WriterOptions.Validate checks it first,
// so Append may return the error of the block it closed.
//
// Returns:
//   - error: ErrPayloadTooLarge or ErrSeqOrder with r not added and the Writer still usable;
//     an error wrapping ErrValidation or a write or sync error, after which the Writer has failed;
//     ErrWriterFailed or ErrClosed on a failed or closed Writer.
func (w *Writer) Append(r *Record) error {
	if err := w.usable(); err != nil {
		return err
	}

	if len(r.Payload) > maxPayloadLen {
		return fmt.Errorf("tracepack: payload of %d bytes above %d: %w", len(r.Payload), maxPayloadLen, ErrPayloadTooLarge)
	}

	seq, err := w.seqFor(r)
	if err != nil {
		return err
	}

	recordLen := recordHeaderLen + len(r.Payload)
	if !w.block.empty() && (hourOf(r.TSUTCNs) != w.block.hour || w.block.size()+recordLen > w.threshold) {
		if err := w.closeBlock(); err != nil {
			return err
		}
	}

	h := canonicalHeader(r, seq)
	w.block.add(&h, r.Payload, transportEventOf(r))
	w.nextSeq = seq + 1
	if w.assignSeq {
		r.Seq = seq
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

// Close flushes the open block and closes the Writer.
//
// Close does not yet write the footer or the trailer (the tracepack format specification §10 and §11),
// so the file it leaves is unfinalized: a reader recovers its blocks by the forward walk of I-1.
// Close does not close the underlying output.
//
// Returns:
//   - error: as for Flush; ErrClosed when already closed.
func (w *Writer) Close() error {
	if err := w.Flush(); err != nil {
		return err
	}

	w.closed = true

	return nil
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

// seqFor returns the seq r is written under: the next assigned seq, or r.Seq checked against the order rules.
func (w *Writer) seqFor(r *Record) (uint64, error) {
	seq := w.nextSeq
	if !w.assignSeq {
		seq = r.Seq
	}

	switch {
	case seq > format.MaxU64:
		return 0, fmt.Errorf("tracepack: seq %d above %d: %w", seq, format.MaxU64, ErrSeqOrder)
	case len(w.blocks) == 0 && w.block.empty() && seq != w.seqStart:
		return 0, fmt.Errorf("tracepack: first seq %d, seq_start is %d: %w", seq, w.seqStart, ErrSeqOrder)
	case seq < w.nextSeq:
		return 0, fmt.Errorf("tracepack: seq %d after seq %d: %w", seq, w.nextSeq-1, ErrSeqOrder)
	}

	return seq, nil
}

// closeBlock encodes the open block, validates it when the Writer attests its blocks,
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

	if w.validate {
		w.decBuf, err = validateEncoded(c, w.decBuf, enc, s, uncompressedLen)
		if err != nil {
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
	if err := w.write(format.AppendBlockEnvelope(envBuf[:0], &env)); err != nil {
		return err
	}
	if err := w.write(enc); err != nil {
		return err
	}
	if w.syncer != nil {
		if err := w.syncer.Sync(); err != nil {
			return fmt.Errorf("tracepack: sync block: %w", err)
		}
	}

	s.offset = w.offset - uint64(format.EnvelopeLen+len(enc))
	s.onDiskLen = uint32(format.EnvelopeLen + len(enc))
	s.uncompressedLen = env.UncompressedLen
	s.bodyCRC = env.BodyCRC
	s.recordHeaderLen = recordHeaderLen
	w.blocks = append(w.blocks, *s)

	return nil
}

// write writes b in full and advances the file offset by what was written.
func (w *Writer) write(b []byte) error {
	n, err := w.out.Write(b)
	w.offset += uint64(n)
	if err != nil {
		return fmt.Errorf("tracepack: write block: %w", err)
	}

	return nil
}
