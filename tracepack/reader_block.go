package tracepack

import (
	"fmt"
	"math"
	"slices"

	"github.com/arloliu/go-secs/tracepack/internal/codec"
	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// blockBuf holds the buffers one iteration reuses across the blocks it reads,
// so a read allocates only for a block larger than every block read before it with the same blockBuf.
// Each iteration owns its blockBuf; nothing mutable is shared on the Reader.
type blockBuf struct {
	// raw holds the block as read: its envelope, then its on-disk body.
	raw []byte
	// decoded holds the decoded body of a full read, or the header section of a header-only read,
	// of a block whose codec is not None; a None block's body is used in place in raw.
	decoded []byte
	// offs holds the payload offsets of the block's records.
	offs []uint32
	blk  decodedBlock
}

// decodedBlock is a block that passed every check of its read level (the tracepack format specification §6).
//
// Its slices alias the blockBuf it was read with,
// so it is valid only until the next read with that blockBuf.
type decodedBlock struct {
	// env is the block envelope, which agrees with the block's BlockInfo.
	env format.BlockEnvelope
	// full reports a full read: the whole body was decoded and I-2 holds.
	// Otherwise the read was header-only, and the block carries no payload.
	full bool
	// section is the header section: record_count record headers of record_header_len bytes each.
	section []byte
	// body is the decoded body of a full read; nil for a header-only read.
	body []byte
	// offs holds record_count + 1 offsets into the decoded body: record i's payload spans offs[i] to offs[i+1].
	offs []uint32
}

// recordCopies is the copy fields a Filter tests and the field_validity bits that make them available
// (the tracepack format specification §7.2).
type recordCopies struct {
	systemBytes [4]byte
	sessionID   uint16
	stream      uint8
	function    uint8
	validity    FieldValidity
}

// recordSpan is what the F-2 entry of a block states about its records (the tracepack format specification §10):
// the last seq and the ranges of ts_utc_ns and epoch.
type recordSpan struct {
	lastSeq  uint64
	tsMin    int64
	tsMax    int64
	epochMin uint32
	epochMax uint32
}

// storedRecord returns the record whose stored header is h and whose payload is payload:
// every field as stored, with MonoPresent and W taken from record_flags.
func storedRecord(h *format.RecordHeader, payload []byte) Record {
	flags := RecordFlags(h.RecordFlags)

	return Record{
		Seq:           h.Seq,
		TSUTCNs:       h.TSUTCNs,
		MonoNs:        h.MonoNs,
		MonoPresent:   flags.Has(RecordFlagsMonoPresent),
		Epoch:         h.Epoch,
		Kind:          Kind(h.Kind),
		Dir:           Dir(h.Dir),
		Fidelity:      Fidelity(h.Fidelity),
		DecodeStatus:  DecodeStatus(h.DecodeStatus),
		TrailingBytes: h.TrailingBytes,
		SystemBytes:   h.SystemBytes,
		SessionID:     h.SessionID,
		Stream:        h.Stream,
		W:             flags.Has(RecordFlagsW),
		Function:      h.Function,
		PType:         h.PType,
		SType:         h.SType,
		Quality:       Quality(h.Quality),
		FieldValidity: FieldValidity(h.FieldValidity),
		Payload:       payload,
	}
}

// copyMismatch reports whether the record header h of a data or control record disagrees with its payload p under I-10:
// a copy field whose field_validity bit is set differs from the payload bytes it copies, or p lacks them.
// A copy field whose bit is clear is not compared, even where p holds its bytes,
// and reserved field_validity bits are ignored (I-9);
// a record of any other kind has no copies, so it never mismatches (the tracepack format specification §7.2).
// It is the Writer's I-10 check: copyMismatch(h, p) == (checkCopies(h, p) != nil).
func copyMismatch(h *format.RecordHeader, p []byte) bool {
	return copyDisagreement(h, p) >= 0
}

// copiesOf returns the copy fields a Filter tests of r, available as r.FieldValidity marks them.
func copiesOf(r *Record) recordCopies {
	return recordCopies{
		systemBytes: r.SystemBytes,
		sessionID:   r.SessionID,
		stream:      r.Stream,
		function:    r.Function,
		validity:    r.FieldValidity,
	}
}

// blockDefect returns the defect of block i, whose BlockInfo is info, at the block's envelope offset.
// Every defect of a block carries that offset,
// because a failed check of the decoded body has no file offset of its own.
func blockDefect(i int, info *BlockInfo, reason IncompleteReason, err error) *Defect {
	return &Defect{
		Reason: reason,
		Block:  i,
		Offset: int64(info.Offset),
		Err:    fmt.Errorf("tracepack: block %d at offset %d: %w", i, info.Offset, err),
	}
}

// checkEnvelope decodes the envelope that starts raw, the block read at the location of info,
// checks that it agrees with info, then checks body_crc over the on-disk body behind it and the codec.
//
// Returns:
//   - format.BlockEnvelope: the decoded envelope.
//   - IncompleteReason: ReasonUnknownCodec for a codec outside the registry of §2, else ReasonCorruptBlock.
//   - error: nil, or the failed check.
func checkEnvelope(raw []byte, info *BlockInfo) (format.BlockEnvelope, IncompleteReason, error) {
	env, err := format.UnmarshalBlockEnvelope(raw)
	if err != nil {
		return env, ReasonCorruptBlock, err
	}
	if err := envelopeAgrees(&env, info); err != nil {
		return env, ReasonCorruptBlock, err
	}
	// The envelope agrees with info, so the body behind it is exactly body_len bytes.
	if crc := format.CRC(raw[format.EnvelopeLen:]); crc != env.BodyCRC {
		return env, ReasonCorruptBlock, fmt.Errorf("body CRC 0x%08X, body_crc 0x%08X", crc, env.BodyCRC)
	}
	if !codec.Known(env.Codec) {
		return env, ReasonUnknownCodec, fmt.Errorf("codec %d: %w", env.Codec, codec.ErrUnknownCodec)
	}

	return env, 0, nil
}

// envelopeAgrees checks that the envelope e agrees with info, the block's F-2 entry or what the forward walk found:
// on_disk_len = 40 + body_len, uncompressed_len, record_count, body_crc, first_seq and record_header_len
// (the tracepack format specification §10).
func envelopeAgrees(e *format.BlockEnvelope, info *BlockInfo) error {
	src := "forward walk"
	if info.Indexed {
		src = "F-2 entry"
	}

	fields := [...]struct {
		name     string
		env, idx uint64
	}{
		{name: "on_disk_len", env: format.EnvelopeLen + uint64(e.BodyLen), idx: uint64(info.OnDiskLen)},
		{name: "uncompressed_len", env: uint64(e.UncompressedLen), idx: uint64(info.UncompressedLen)},
		{name: "record_count", env: uint64(e.RecordCount), idx: uint64(info.RecordCount)},
		{name: "body_crc", env: uint64(e.BodyCRC), idx: uint64(info.BodyCRC)},
		{name: "first_seq", env: e.FirstSeq, idx: info.FirstSeq},
		{name: "record_header_len", env: uint64(e.RecordHeaderLen), idx: uint64(info.RecordHeaderLen)},
	}
	for _, f := range fields {
		if f.env != f.idx {
			return fmt.Errorf("envelope %s %d disagrees with the %s's %d", f.name, f.env, src, f.idx)
		}
	}

	return nil
}

// decodeBlock decodes the block of env, read into buf.raw, at the full or the header-only level,
// and checks its records with checkRecords.
// A full read decodes the whole body to exactly uncompressed_len bytes;
// a header-only read decodes only the header section, leaving the stream after it unchecked.
// The header section's length is checked against uncompressed_len before either decode.
//
// Returns:
//   - *decodedBlock: &buf.blk, filled; nil on error.
//   - recordSpan: the records' last seq and time and epoch ranges.
//   - error: nil, or the failed check.
func decodeBlock(env *format.BlockEnvelope, buf *blockBuf, full bool) (*decodedBlock, recordSpan, error) {
	hsLen, err := headerSectionLen(env)
	if err != nil {
		return nil, recordSpan{}, err
	}

	decoded, err := decodeBody(env, buf, hsLen, full)
	if err != nil {
		return nil, recordSpan{}, err
	}

	d := &buf.blk
	*d = decodedBlock{env: *env, full: full, section: decoded[:hsLen]}
	if full {
		d.body = decoded
	}

	var span recordSpan
	buf.offs, span, err = checkRecords(d.section, env, buf.offs)
	if err != nil {
		return nil, recordSpan{}, err
	}
	d.offs = buf.offs

	return d, span, nil
}

// headerSectionLen returns the length of the header section of the block of env,
// record_count × record_header_len, after checking that it fits uncompressed_len.
func headerSectionLen(env *format.BlockEnvelope) (int, error) {
	hsLen, err := format.HeaderSectionLen(env.RecordCount, env.RecordHeaderLen)
	if err != nil {
		return 0, err
	}
	if n := int(env.UncompressedLen); hsLen > n {
		return 0, fmt.Errorf("header section of %d bytes exceeds uncompressed_len %d", hsLen, n)
	}

	return hsLen, nil
}

// decodeBody decodes the on-disk body in buf.raw of the block of env:
// all of it for a full read, else at least its first hsLen decoded bytes, the header section.
// A None body is the decoded body itself, already covered by body_crc,
// so it is used in place, aliasing buf.raw, instead of copied into buf.decoded.
func decodeBody(env *format.BlockEnvelope, buf *blockBuf, hsLen int, full bool) ([]byte, error) {
	n := int(env.UncompressedLen)
	body := buf.raw[format.EnvelopeLen:]

	var err error
	switch {
	case env.Codec == codec.None:
		err = codec.CheckNone(body, n)
	case full:
		buf.decoded, err = codec.Decode(env.Codec, buf.decoded, body, n)
		body = buf.decoded
	default:
		buf.decoded, err = codec.DecodePrefix(env.Codec, buf.decoded, body, hsLen, n)
		body = buf.decoded
	}

	switch {
	case err == nil:
		return body, nil
	case full:
		return nil, fmt.Errorf("decode body: %w", err)
	default:
		return nil, fmt.Errorf("decode header section: %w", err)
	}
}

// checkRecords checks the record headers of the header section of the block of env against I-2:
// the first seq equals first_seq, seqs strictly increase,
// and record_count × record_header_len + Σ payload_len equals uncompressed_len,
// the sum checked as it grows, so it never passes uncompressed_len.
// It reuses offs for the payload offsets.
//
// Returns:
//   - []uint32: record_count + 1 offsets into the decoded body, each record's payload between two of them.
//   - recordSpan: the records' last seq and time and epoch ranges.
//   - error: nil, or the first violation.
func checkRecords(section []byte, env *format.BlockEnvelope, offs []uint32) ([]uint32, recordSpan, error) {
	count, rhl := int(env.RecordCount), int(env.RecordHeaderLen)
	limit := uint64(env.UncompressedLen)
	end := uint64(len(section))

	offs = append(slices.Grow(offs[:0], count+1), uint32(end))
	var span recordSpan
	for i := range count {
		h, err := format.UnmarshalRecordHeader(section[i*rhl:], format.RecordHeaderLen)
		if err != nil {
			return offs, span, fmt.Errorf("record %d: %w", i, err)
		}
		if err := checkBlockSeq(i, h.Seq, span.lastSeq, env.FirstSeq); err != nil {
			return offs, span, err
		}
		span.add(i, &h)

		end += uint64(h.PayloadLen)
		if end > limit {
			return offs, span, fmt.Errorf("record %d: payload ends at %d, past uncompressed_len %d", i, end, limit)
		}
		offs = append(offs, uint32(end))
	}
	if end != limit {
		return offs, span, fmt.Errorf("record headers and payloads cover %d bytes, uncompressed_len is %d", end, limit)
	}

	return offs, span, nil
}

// readBlock reads block i at the full or the header-only level and validates it before any of its records is used
// (the tracepack format specification §6).
//
// It checks the block's on-disk body length and uncompressed_len against MaxBlockLen before it allocates anything,
// then reads the whole block with one ReadAt, because body_crc covers the whole on-disk body at both levels.
// The envelope must decode and agree with the block's BlockInfo:
// with its F-2 entry for an indexed block (§10), with what the forward walk found for a walked one.
// Then body_crc must match and the codec must be known (§2).
// A full read decodes the whole body and checks I-2.
// A header-only read checks that record_count × record_header_len fits uncompressed_len, decodes only that prefix,
// and checks the seqs of I-2 and its length equation from the headers' payload_len.
// For an indexed block the records' last seq, ts_utc_ns range and epoch range must also equal its F-2 entry.
//
// Parameters:
//   - i: the block's index into the Reader's block index.
//   - full: whether to decode the whole body; else the read is header-only.
//   - buf: the buffers to read into; the block returned aliases them until buf's next use.
//
// Returns:
//   - *decodedBlock: the validated block; nil with a defect or an error.
//   - *Defect: the block's defect, at its envelope offset:
//     ReasonLimit, wrapping ErrReadLimit, for a block over MaxBlockLen;
//     ReasonUnknownCodec for a codec outside the registry;
//     ReasonCorruptBlock for any other failed check.
//   - error: the ReadAt error, wrapping io.ErrUnexpectedEOF for a short read.
func (r *Reader) readBlock(i int, full bool, buf *blockBuf) (*decodedBlock, *Defect, error) {
	info := &r.blocks[i]
	if err := r.checkBlockBudget(info); err != nil {
		return nil, blockDefect(i, info, ReasonLimit, err), nil
	}

	buf.raw = slices.Grow(buf.raw[:0], int(info.OnDiskLen))[:info.OnDiskLen]
	if err := readAt(r.ra, buf.raw, int64(info.Offset)); err != nil {
		return nil, nil, err
	}

	env, reason, err := checkEnvelope(buf.raw, info)
	if err != nil {
		return nil, blockDefect(i, info, reason, err), nil
	}

	d, span, err := decodeBlock(&env, buf, full)
	if err == nil && info.Indexed {
		err = span.agrees(info)
	}
	if err != nil {
		return nil, blockDefect(i, info, ReasonCorruptBlock, err), nil
	}

	return d, nil, nil
}

// checkBlockBudget checks the on-disk body length and uncompressed_len of the block of info against MaxBlockLen.
// The on-disk length is taken from info, so the check runs before the block is read.
func (r *Reader) checkBlockBudget(info *BlockInfo) error {
	// A limit within the int range keeps every length that passes addressable where int is 32 bits.
	limit := min(r.opts.MaxBlockLen, int64(math.MaxInt-format.EnvelopeLen))

	if bodyLen := int64(info.OnDiskLen) - format.EnvelopeLen; bodyLen > limit {
		return fmt.Errorf("body_len %d exceeds MaxBlockLen %d: %w", bodyLen, r.opts.MaxBlockLen, ErrReadLimit)
	}
	if n := int64(info.UncompressedLen); n > limit {
		return fmt.Errorf("uncompressed_len %d exceeds MaxBlockLen %d: %w", n, r.opts.MaxBlockLen, ErrReadLimit)
	}

	return nil
}

// count returns the number of records in the block.
func (d *decodedBlock) count() int {
	return int(d.env.RecordCount)
}

// header returns the record header of record i,
// whose Extra aliases the header section's bytes behind offset 56, or is nil for 56-byte headers.
func (d *decodedBlock) header(i int) format.RecordHeader {
	rhl := int(d.env.RecordHeaderLen)
	b := d.section[i*rhl : (i+1)*rhl : (i+1)*rhl]

	// readBlock decoded every header of the block without error.
	h, _ := format.UnmarshalRecordHeader(b, format.RecordHeaderLen)
	if rhl > format.RecordHeaderLen {
		h.Extra = b[format.RecordHeaderLen:]
	}

	return h
}

// payload returns the payload of record i, aliasing the decoded body; nil for a header-only read.
func (d *decodedBlock) payload(i int) []byte {
	if !d.full {
		return nil
	}

	return d.body[d.offs[i]:d.offs[i+1]:d.offs[i+1]]
}

// add folds the header h of record i of a block into s.
func (s *recordSpan) add(i int, h *format.RecordHeader) {
	if i == 0 {
		s.tsMin, s.tsMax = h.TSUTCNs, h.TSUTCNs
		s.epochMin, s.epochMax = h.Epoch, h.Epoch
	}

	s.lastSeq = h.Seq
	s.tsMin, s.tsMax = min(s.tsMin, h.TSUTCNs), max(s.tsMax, h.TSUTCNs)
	s.epochMin, s.epochMax = min(s.epochMin, h.Epoch), max(s.epochMax, h.Epoch)
}

// agrees checks s against the F-2 entry of the indexed block of info:
// last_seq, ts_min, ts_max, epoch_min and epoch_max (the tracepack format specification §10).
func (s *recordSpan) agrees(info *BlockInfo) error {
	// Seqs are at most 2^63-1 and epochs below 2^32, so every value fits an int64.
	fields := [...]struct {
		name      string
		got, want int64
	}{
		{name: "last_seq", got: int64(s.lastSeq), want: int64(info.LastSeq)},
		{name: "ts_min", got: s.tsMin, want: info.TSMin},
		{name: "ts_max", got: s.tsMax, want: info.TSMax},
		{name: "epoch_min", got: int64(s.epochMin), want: int64(info.EpochMin)},
		{name: "epoch_max", got: int64(s.epochMax), want: int64(info.EpochMax)},
	}
	for _, f := range fields {
		if f.got != f.want {
			return fmt.Errorf("records give %s %d, the F-2 entry %d", f.name, f.got, f.want)
		}
	}

	return nil
}
