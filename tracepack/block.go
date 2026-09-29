package tracepack

import (
	"fmt"

	"github.com/arloliu/go-secs/tracepack/internal/codec"
	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// recordHeaderLen is the record_header_len this writer gives every block: the 44 bytes format 1.0 defines.
const recordHeaderLen = format.RecordHeaderLen

// maxPayloadLen is the largest payload a record may carry.
// A record alone in its own block has uncompressed_len = 44 + payload_len,
// which the tracepack format specification §2 limits to 2^31-1.
const maxPayloadLen = int(format.MaxLen32) - recordHeaderLen

// hourNs is one hour in nanoseconds, the width of the UTC hour a block's records share (I-13).
const hourNs int64 = 3_600_000_000_000

// seqRange is one maximal run of consecutive seqs, first to last inclusive.
type seqRange struct {
	first uint64
	last  uint64
}

// epochSummary accumulates the records of one epoch within one block,
// for the nested epoch entry of the block's footer summary (the tracepack format specification §10),
// and within the whole pack for the pack statistics.
type epochSummary struct {
	epoch       uint32
	recordCount uint64
	seqFirst    uint64
	seqLast     uint64
	tsMin       int64
	tsMax       int64
	// closeSeq is the seq of the first socket-close event or clean stop boundary of this epoch in the block;
	// valid iff hasCloseSeq.
	closeSeq    uint64
	hasCloseSeq bool
}

// boundarySummary is one capture-boundary record of a block,
// for the nested boundary entry of the block's footer summary (the tracepack format specification §10).
type boundarySummary struct {
	seq      uint64
	kind     BoundaryKind
	ts       int64
	epoch    uint32
	gapStart *int64
	gapEnd   *int64
}

// blockSummary is what the footer needs to know about one written block:
// its block index entry and its block summary (the tracepack format specification §10),
// so the footer is built from these values without re-reading any block.
type blockSummary struct {
	// offset is the file offset of the block envelope.
	offset uint64
	// onDiskLen is the envelope plus body length.
	onDiskLen       uint32
	uncompressedLen uint32
	recordCount     uint32
	bodyCRC         uint32
	recordHeaderLen uint16
	firstSeq        uint64
	lastSeq         uint64
	tsMin           int64
	tsMax           int64
	epochMin        uint32
	epochMax        uint32
	// kindCounts, dirCounts and decodeStatusCounts count records per raw enum value,
	// each sized by the largest value seen plus one, since a record may carry a value outside the registry.
	kindCounts         []uint32
	dirCounts          []uint32
	decodeStatusCounts []uint32
	qualityUnion       Quality
	// epochs holds one entry per epoch present, in order of first appearance.
	epochs []epochSummary
	// boundaries holds one entry per capture-boundary record, in seq order.
	boundaries []boundarySummary
	// seqRanges holds the block's seqs as sorted maximal runs; one run means the seqs are contiguous.
	seqRanges []seqRange
}

// blockBuilder accumulates the open block: its record headers as consecutive rows, its payload section and its summary.
type blockBuilder struct {
	// headers holds the record headers one after another; body transposes them into the header section.
	headers  []byte
	payloads []byte
	hour     int64
	summary  blockSummary
}

// hourOf returns the UTC hour number of ts,
// rounding toward negative infinity so that times before 1970 stay in their own hour.
func hourOf(ts int64) int64 {
	h := ts / hourNs
	if ts%hourNs < 0 {
		h--
	}

	return h
}

// exceedsLimit reports whether size + add exceeds limit, for sizes that are never negative.
// The sum is taken in uint64, because in int it can overflow where int is 32 bits.
func exceedsLimit(size, add, limit int) bool {
	return uint64(size)+uint64(add) > uint64(limit)
}

// validateBody checks a decoded block body against I-2 of the tracepack format specification §3,
// with the record_header_len this writer uses:
// it gathers the record headers from the column-by-column header section into buf.rows (§6),
// then checks that record_count × record_header_len + Σ payload_len equals the decoded length,
// the first record's seq equals firstSeq, and seqs strictly increase.
// The checks are the reader's, checkRecords;
// every product and sum is checked before it is used to slice.
// buf's buffers are reused.
func validateBody(decoded []byte, count uint32, firstSeq uint64, buf *validateBuf) error {
	// The writer's decoded length is at most 2^31-1, so it fits uncompressed_len.
	env := format.BlockEnvelope{
		RecordHeaderLen: recordHeaderLen,
		UncompressedLen: uint32(len(decoded)),
		RecordCount:     count,
		FirstSeq:        firstSeq,
	}
	hsLen, err := headerSectionLen(&env)
	if err != nil {
		return err
	}
	buf.rows = format.UntransposeHeaders(buf.rows[:0], decoded[:hsLen], int(count), recordHeaderLen)
	buf.offs, _, err = checkRecords(buf.rows, &env, buf.offs)

	return err
}

// checkBlockSeq checks the seq rules of I-2 for record i of a block.
func checkBlockSeq(i int, seq, prevSeq, firstSeq uint64) error {
	if i == 0 && seq != firstSeq {
		return fmt.Errorf("first record seq %d, first_seq %d", seq, firstSeq)
	}
	if i > 0 && seq <= prevSeq {
		return fmt.Errorf("record %d seq %d does not follow %d", i, seq, prevSeq)
	}

	return nil
}

// countEnum increments counts[v], growing counts to hold index v.
func countEnum(counts []uint32, v uint8) []uint32 {
	if int(v) >= len(counts) {
		counts = append(counts, make([]uint32, int(v)+1-len(counts))...)
	}
	counts[v]++

	return counts
}

// encodeBlock encodes body with c and returns the on-disk body and the codec actually used.
//
// A zstd frame of incompressible input can be longer than the input;
// when it would exceed the 2^31-1 body_len limit of the tracepack format specification §2,
// the block is stored with the none codec instead, which the per-block codec field allows.
// For the none codec the on-disk body is body itself, not a copy.
func encodeBlock(c Codec, dst, body []byte) ([]byte, Codec, error) {
	if c == CodecNone {
		return body, CodecNone, nil
	}

	enc, err := codec.Encode(uint8(c), dst, body)
	if err != nil {
		return nil, c, err
	}
	if len(enc) > int(format.MaxLen32) {
		return body, CodecNone, nil
	}

	return enc, c, nil
}

// validateBuf holds the buffers the writer's block validation reuses across blocks.
type validateBuf struct {
	// decoded holds the decoded body.
	decoded []byte
	// rows holds the record headers gathered from the header section.
	rows []byte
	// offs holds the records' payload offsets.
	offs []uint32
}

// validateEncoded decodes an encoded block body into buf and checks it with validateBody.
func validateEncoded(c Codec, buf *validateBuf, enc []byte, s *blockSummary, uncompressedLen int) error {
	var err error
	buf.decoded, err = codec.Decode(uint8(c), buf.decoded, enc, uncompressedLen)
	if err != nil {
		return fmt.Errorf("decode the encoded body: %w", err)
	}

	return validateBody(buf.decoded, s.recordCount, s.firstSeq, buf)
}

// empty reports whether the open block holds no record.
func (b *blockBuilder) empty() bool {
	return b.summary.recordCount == 0
}

// size returns the open block's uncompressed length so far.
func (b *blockBuilder) size() int {
	return len(b.headers) + len(b.payloads)
}

// add appends one record, whose canonical header is h, to the open block and updates its summary.
// ev is the record's decoded transport-event payload, or nil.
func (b *blockBuilder) add(h *format.RecordHeader, payload []byte, ev *TransportEvent) {
	if b.empty() {
		b.hour = hourOf(h.TSUTCNs)
	}

	b.headers = format.AppendRecordHeader(b.headers, h)
	b.payloads = append(b.payloads, payload...)
	b.summary.addRecord(h, ev)
}

// body returns the decoded block body in buf: the header section, the record headers transposed column by column
// (the tracepack format specification §6), followed by the payload section.
func (b *blockBuilder) body(buf []byte) []byte {
	buf = format.TransposeHeaders(buf[:0], b.headers, int(b.summary.recordCount), recordHeaderLen)

	return append(buf, b.payloads...)
}

// reset empties the open block, keeping its buffers for reuse.
func (b *blockBuilder) reset() {
	b.headers = b.headers[:0]
	b.payloads = b.payloads[:0]
	b.summary = blockSummary{}
}

// addRecord folds one record into s.
func (s *blockSummary) addRecord(h *format.RecordHeader, ev *TransportEvent) {
	if s.recordCount == 0 {
		s.firstSeq = h.Seq
		s.tsMin, s.tsMax = h.TSUTCNs, h.TSUTCNs
		s.epochMin, s.epochMax = h.Epoch, h.Epoch
	}

	s.recordCount++
	s.lastSeq = h.Seq
	s.tsMin, s.tsMax = min(s.tsMin, h.TSUTCNs), max(s.tsMax, h.TSUTCNs)
	s.epochMin, s.epochMax = min(s.epochMin, h.Epoch), max(s.epochMax, h.Epoch)
	s.kindCounts = countEnum(s.kindCounts, h.Kind)
	s.dirCounts = countEnum(s.dirCounts, h.Dir)
	s.decodeStatusCounts = countEnum(s.decodeStatusCounts, h.DecodeStatus)
	s.qualityUnion |= Quality(h.Quality)
	s.addSeq(h.Seq)

	e := s.epochEntry(h)
	if ev != nil {
		s.addEvent(h, ev, e)
	}
}

// addSeq extends the last seq run with seq, or starts a new run after a gap.
// Seqs arrive strictly increasing.
func (s *blockSummary) addSeq(seq uint64) {
	if n := len(s.seqRanges); n > 0 && s.seqRanges[n-1].last+1 == seq {
		s.seqRanges[n-1].last = seq
		return
	}

	s.seqRanges = append(s.seqRanges, seqRange{first: seq, last: seq})
}

// epochEntry folds the record into the entry of its epoch, creating it on first appearance, and returns it.
func (s *blockSummary) epochEntry(h *format.RecordHeader) *epochSummary {
	for i := range s.epochs {
		e := &s.epochs[i]
		if e.epoch != h.Epoch {
			continue
		}

		e.recordCount++
		e.seqLast = h.Seq
		e.tsMin, e.tsMax = min(e.tsMin, h.TSUTCNs), max(e.tsMax, h.TSUTCNs)

		return e
	}

	s.epochs = append(s.epochs, epochSummary{
		epoch: h.Epoch, recordCount: 1,
		seqFirst: h.Seq, seqLast: h.Seq,
		tsMin: h.TSUTCNs, tsMax: h.TSUTCNs,
	})

	return &s.epochs[len(s.epochs)-1]
}

// addEvent records what a transport event contributes to the summary:
// a boundary entry for a capture-boundary event,
// and the close seq of the record's epoch for a socket-close event or a clean stop boundary.
func (s *blockSummary) addEvent(h *format.RecordHeader, ev *TransportEvent, e *epochSummary) {
	closes := ev.Event == EventSocketClose

	if ev.Event == EventCaptureBoundary {
		kind := BoundaryKindUnknown
		if ev.BoundaryKind != nil {
			kind = *ev.BoundaryKind
		}

		s.boundaries = append(s.boundaries, boundarySummary{
			seq: h.Seq, kind: kind, ts: h.TSUTCNs, epoch: h.Epoch,
			gapStart: ev.GapStart, gapEnd: ev.GapEnd,
		})
		closes = kind == BoundaryKindStop
	}

	if closes && !e.hasCloseSeq {
		e.closeSeq, e.hasCloseSeq = h.Seq, true
	}
}
