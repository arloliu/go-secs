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
// A held block of a read over several packs has a blockBuf of its own instead,
// read into once and kept until the block is dropped.
type blockBuf struct {
	// raw holds the block as read with a ReadAt: its envelope, then its on-disk body.
	// It is always storage of the read's own, never the Reader's retained bytes.
	raw []byte
	// view is the block's on-disk bytes, envelope then body, that the checks and the decode read:
	// raw, or, for a compressed block within the Reader's retained bytes, those bytes themselves, read in place.
	// A None block's body is used in place as its decoded body, so a None block is always copied into raw first:
	// nothing a callback receives ever aliases the retained bytes.
	view []byte
	// retained reports that view is the Reader's retained bytes, not raw.
	retained bool
	// decoded holds the decoded body of a block whose codec is not None; a None block's body is used in place in raw.
	decoded []byte
	// rows holds the record headers gathered from the header section, one after another.
	rows []byte
	// offs holds the payload offsets of the block's records.
	offs []uint32
	blk  decodedBlock
}

// decodedBlock is a block that passed every check of a full read (the tracepack format specification §6).
//
// Its slices alias the blockBuf it was read with,
// so it is valid only until the next read with that blockBuf,
// or, for a held block, until the block is dropped.
type decodedBlock struct {
	// env is the block envelope, which agrees with the block's BlockInfo.
	env format.BlockEnvelope
	// section holds the record headers gathered from the header section, one after another:
	// record_count headers of record_header_len bytes each.
	section []byte
	// body is the decoded body.
	body []byte
	// offs holds record_count + 1 offsets into the decoded body: record i's payload spans offs[i] to offs[i+1].
	offs []uint32
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
// every field as stored, with MonoPresent taken from record_flags.
func storedRecord(h *format.RecordHeader, payload []byte) Record {
	flags := recordFlagSet(h.RecordFlags)

	return Record{
		Seq:           h.Seq,
		TSUTCNs:       h.TSUTCNs,
		MonoNs:        h.MonoNs,
		MonoPresent:   flags.Has(recordFlagMonoPresent),
		Epoch:         h.Epoch,
		Kind:          Kind(h.Kind),
		Dir:           Dir(h.Dir),
		Fidelity:      Fidelity(h.Fidelity),
		DecodeStatus:  DecodeStatus(h.DecodeStatus),
		TrailingBytes: h.TrailingBytes,
		Quality:       Quality(h.Quality),
		FieldValidity: FieldValidity(h.FieldValidity),
		Payload:       payload,
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
//   - IncompleteReason: IncompleteReasonUnknownCodec for a codec outside the registry of §2, else IncompleteReasonCorruptBlock.
//   - error: nil, or the failed check.
func checkEnvelope(raw []byte, info *BlockInfo) (format.BlockEnvelope, IncompleteReason, error) {
	env, err := format.UnmarshalBlockEnvelope(raw)
	if err != nil {
		return env, IncompleteReasonCorruptBlock, err
	}
	if err := envelopeAgrees(&env, info); err != nil {
		return env, IncompleteReasonCorruptBlock, err
	}
	// The envelope agrees with info, so the body behind it is exactly body_len bytes.
	if crc := format.CRC(raw[format.EnvelopeLen:]); crc != env.BodyCRC {
		return env, IncompleteReasonCorruptBlock, fmt.Errorf("body CRC 0x%08X, body_crc 0x%08X", crc, env.BodyCRC)
	}
	if !codec.Known(env.Codec) {
		return env, IncompleteReasonUnknownCodec, fmt.Errorf("codec %d: %w", env.Codec, codec.ErrUnknownCodec)
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

// decodeBlock decodes the whole body of the block of env, whose on-disk bytes buf.view holds, to exactly uncompressed_len bytes,
// and checks its records with checkRecords.
// The header section's length is checked against uncompressed_len before the decode.
//
// Returns:
//   - *decodedBlock: &buf.blk, filled; nil on error.
//   - recordSpan: the records' last seq and time and epoch ranges.
//   - error: nil, or the failed check.
func decodeBlock(env *format.BlockEnvelope, buf *blockBuf) (*decodedBlock, recordSpan, error) {
	hsLen, err := headerSectionLen(env.RecordCount, env.RecordHeaderLen, env.UncompressedLen)
	if err != nil {
		return nil, recordSpan{}, err
	}

	decoded, err := decodeBody(env, buf)
	if err != nil {
		return nil, recordSpan{}, err
	}

	// The header section stores the headers column by column (the tracepack format specification §6);
	// every record-level path reads them gathered into rows, copied so a None body used in place is never changed.
	buf.rows = format.UntransposeHeaders(buf.rows[:0], decoded[:hsLen], int(env.RecordCount), int(env.RecordHeaderLen))
	d := &buf.blk
	*d = decodedBlock{env: *env, section: buf.rows, body: decoded}

	var span recordSpan
	buf.offs, span, err = checkRecords(d.section, env, buf.offs)
	if err != nil {
		return nil, recordSpan{}, err
	}
	d.offs = buf.offs

	return d, span, nil
}

// headerSectionLen returns the length of the header section of a block of count records of rhl bytes each,
// count × rhl, after checking that it fits the block's uncompressed_len ulen.
func headerSectionLen(count uint32, rhl uint16, ulen uint32) (int, error) {
	hsLen, err := format.HeaderSectionLen(count, rhl)
	if err != nil {
		return 0, err
	}
	if n := int(ulen); hsLen > n {
		return 0, fmt.Errorf("header section of %d bytes exceeds uncompressed_len %d", hsLen, n)
	}

	return hsLen, nil
}

// decodeBody decodes the whole on-disk body in buf.view of the block of env.
// A None body is the decoded body itself, already covered by body_crc,
// so it is used in place, aliasing buf.view, which is then buf.raw, instead of copied into buf.decoded.
// Any other body is decoded into buf.decoded,
// first allocated at exactly uncompressed_len when buf.decoded has no capacity.
func decodeBody(env *format.BlockEnvelope, buf *blockBuf) ([]byte, error) {
	n := int(env.UncompressedLen)
	body := buf.view[format.EnvelopeLen:]

	var err error
	if env.Codec == codec.None {
		err = codec.CheckNone(body, n)
	} else {
		if cap(buf.decoded) == 0 {
			// A fresh buffer gets the exact length, which growing it by appending would round up.
			buf.decoded = make([]byte, 0, n)
		}
		buf.decoded, err = codec.Decode(env.Codec, buf.decoded, body, n)
		body = buf.decoded
	}
	if err != nil {
		return nil, fmt.Errorf("decode body: %w", err)
	}

	return body, nil
}

// checkRecords checks the record headers of the block of env, gathered from its header section into section, against I-2:
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
		// The section holds count × rhl bytes, rhl at least 44, so only the two limits of UnmarshalRecordHeader can fail;
		// it builds the error of a header outside them.
		h := format.DecodeRecordHeader(section[i*rhl:])
		if h.Seq > format.MaxU64 || h.PayloadLen > format.MaxLen32 {
			_, err := format.UnmarshalRecordHeader(section[i*rhl:], format.RecordHeaderLen)

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

// readBlock reads block i in full and validates it before any of its records is used
// (the tracepack format specification §6: every block read is a full read).
//
// It checks the block's on-disk body length and uncompressed_len against MaxBlockLen before it allocates anything,
// then reads the whole block into buf:
// copied from the bootstrap bytes the Reader keeps when they hold the whole block, else with one ReadAt.
// Either way the block passes every check below; a block from kept bytes skips none.
// The envelope must decode and agree with the block's BlockInfo:
// with its F-2 entry for an indexed block (§10), with what the forward walk found for a walked one.
// Then body_crc must match and the codec must be known (§2).
// It checks that record_count × record_header_len fits uncompressed_len, decodes the whole body and checks I-2.
// For an indexed block the records' last seq, ts_utc_ns range and epoch range are then compared with its F-2 entry;
// a disagreement is reported beside the block, not instead of it (§10).
//
// Parameters:
//   - i: the block's index into the Reader's block index.
//   - buf: the buffers to read into; the block returned aliases them until buf's next use.
//
// Returns:
//   - *decodedBlock: the validated block; nil with a failure or an error.
//   - *Defect: the block's defect, at its envelope offset:
//     IncompleteReasonLimit, wrapping ErrReadLimit, for a block over MaxBlockLen;
//     IncompleteReasonUnknownCodec for a codec outside the registry;
//     IncompleteReasonCorruptBlock for any other failed check, all three with a nil block;
//     IncompleteReasonIndexMismatch, with the block, for an indexed block whose records disagree with its F-2 entry.
//   - error: the ReadAt error, wrapping io.ErrUnexpectedEOF for a short read; never one for a block from kept bytes.
func (r *Reader) readBlock(i int, buf *blockBuf) (*decodedBlock, *Defect, error) {
	info := &r.blocks[i]
	if err := r.checkBlockBudget(info); err != nil {
		return nil, blockDefect(i, info, IncompleteReasonLimit, err), nil
	}

	if held := r.heldRange(int64(info.Offset), int64(info.OnDiskLen)); held != nil {
		buf.view, buf.retained = held, true
	} else {
		buf.raw = slices.Grow(buf.raw[:0], int(info.OnDiskLen))[:info.OnDiskLen]
		if err := readAt(r.ra, buf.raw, int64(info.Offset)); err != nil {
			return nil, nil, err
		}
		buf.view, buf.retained = buf.raw, false
	}
	d, def := r.checkAndDecode(i, buf)

	return d, def, nil
}

// checkAndDecode checks and decodes block i, whose on-disk bytes buf.view holds, as readBlock does once it has read them:
// the envelope's checks, the decode of the body and its records,
// and for an indexed block the comparison of its records with its F-2 entry.
// It does not check the block against MaxBlockLen, which its caller did before reading it.
//
// Returns:
//   - *decodedBlock: the validated block, aliasing buf; nil with a failure.
//   - *Defect: the block's defect, at its envelope offset, as readBlock reports it:
//     IncompleteReasonUnknownCodec or IncompleteReasonCorruptBlock with a nil block,
//     or IncompleteReasonIndexMismatch beside the block.
func (r *Reader) checkAndDecode(i int, buf *blockBuf) (*decodedBlock, *Defect) {
	info := &r.blocks[i]
	env, reason, err := checkEnvelope(buf.view, info)
	if err != nil {
		return nil, blockDefect(i, info, reason, err)
	}
	if env.Codec == codec.None && buf.retained {
		buf.ownView()
	}

	d, span, err := decodeBlock(&env, buf)
	if err != nil {
		return nil, blockDefect(i, info, IncompleteReasonCorruptBlock, err)
	}
	if info.Indexed {
		if err := span.agrees(info); err != nil {
			return d, blockDefect(i, info, IncompleteReasonIndexMismatch, err)
		}
	}

	return d, nil
}

// ownView copies the block's bytes in view into raw, exactly as long as they are when raw is too short, and points view at raw.
func (buf *blockBuf) ownView() {
	if cap(buf.raw) < len(buf.view) {
		buf.raw = make([]byte, len(buf.view))
	} else {
		buf.raw = buf.raw[:len(buf.view)]
	}
	copy(buf.raw, buf.view)
	buf.view, buf.retained = buf.raw, false
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
// whose Extra aliases the header's extension area behind offset 44, or is nil for 44-byte headers.
func (d *decodedBlock) header(i int) format.RecordHeader {
	rhl := int(d.env.RecordHeaderLen)
	b := d.section[i*rhl : (i+1)*rhl : (i+1)*rhl]

	// checkRecords accepted every header of the block before the block was made.
	h := format.DecodeRecordHeader(b)
	if rhl > format.RecordHeaderLen {
		h.Extra = b[format.RecordHeaderLen:]
	}

	return h
}

// payload returns the payload of record i, aliasing the decoded body.
func (d *decodedBlock) payload(i int) []byte {
	return d.body[d.offs[i]:d.offs[i+1]:d.offs[i+1]]
}

// summaryOf returns the summary the Writer builds for the records of d (the tracepack format specification §10);
// the fields the block's envelope and position give are left to the caller.
// The summary shares no memory with d, and holds no epoch index.
func summaryOf(d *decodedBlock) blockSummary {
	var s blockSummary
	for j := range d.count() {
		h := d.header(j)
		var ev *TransportEvent
		if Kind(h.Kind) == KindTransportEvent {
			// A payload that does not decode contributes nothing, as in the Writer's summary.
			ev, _ = UnmarshalTransportEvent(d.payload(j))
		}
		s.addRecord(&h, ev)
	}
	s.epochIndex = nil

	return s
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
