package tracepack

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"slices"

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

// maxLinearEpochs is the number of epoch entries a block summary searches linearly;
// a summary with more keeps an epoch index, so a block of n records in n epochs is summarized in O(n).
const maxLinearEpochs = 8

// epochProbe, when a test sets it, receives the number of epoch entries each epoch lookup examined:
// those it compared, and those it indexed when it built the epoch index.
var epochProbe func(examined int)

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
	// closeSeq is the lowest seq among the socket-close events and clean stop boundaries of this epoch in the block;
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
	// epochIndex maps an epoch to its entry in epochs once a lookup has found more than maxLinearEpochs entries.
	// It is building state only: a clone and a finished summary hold none,
	// and the next lookup that needs it builds it again.
	epochIndex map[uint32]int
	// boundaries holds one entry per capture-boundary record, in seq order.
	boundaries []boundarySummary
	// seqRanges holds the block's seqs as sorted maximal runs; one run means the seqs are contiguous.
	seqRanges []seqRange
	// verbatimF3, when not empty, is the block's F-3 entry list as another pack's footer stores it,
	// which the footer writes byte for byte in place of the list appendF3 builds (the tracepack format specification §10);
	// only a summary the Writer keeps for a block appended with a verbatim list holds one, and a clone holds none.
	verbatimF3 []byte
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
	hsLen, err := headerSectionLen(env.RecordCount, env.RecordHeaderLen, env.UncompressedLen)
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

// encodeRawBlock encodes one block from records of another pack, given as stored, with codec c,
// and checks the encoding in memory before it returns it
// (the tracepack storage specification §4: a merger validates every new encoding before it writes it).
//
// The decoded body is the header section, the rows' bytes transposed column by column (the tracepack format specification §6),
// followed by the payloads; encodeBlock may store it with the none codec instead of c.
// No record header is decoded and encoded again,
// so reserved and retired bits and the extension area behind offset 44 stay as they are (§7.1).
// Before it allocates anything, and before any length is narrowed into the envelope, it checks with overflow checks:
// headerLen within 44 to 65535; at least one record, and one payload per row; each row exactly headerLen bytes;
// seqs strictly increasing (I-2); the decoded body, from the rows' payload_len, within 2^31-1 bytes (§2);
// and each payload as long as its row's payload_len.
// Then it decodes the block it built as a reader does (§6), checks I-2 on the gathered headers,
// and compares every record's header row and payload byte for byte with its source.
// It leaves to the Writer's appendBlock the checks appendBlock runs on every block:
// the block's seqs against the pack's (I-12, seq_start included), its records' UTC hour (I-13) and the pack's commitments.
//
// Parameters:
//   - e: the buffers to build and check the block in, reused from one call to the next.
//   - c: CodecNone or CodecZstd.
//   - headerLen: the record_header_len of the rows.
//   - rows: the records' header rows as stored, in seq order.
//   - payloads: the records' payloads, parallel to rows.
//
// Returns:
//   - []byte: the block's envelope and on-disk body, for appendBlock, aliasing e until its next use.
//   - blockSummary: the summary of its records, as summaryOf gives it, sharing no memory with e.
//   - error: the failed check; no block is returned.
func encodeRawBlock(e *encodeBuf, c Codec, headerLen int, rows, payloads [][]byte) ([]byte, blockSummary, error) {
	uncompressedLen, firstSeq, err := checkRawRecords(headerLen, rows, payloads)
	if err != nil {
		return nil, blockSummary{}, fmt.Errorf("tracepack: encode a block: %w", err)
	}

	raw, used, err := e.build(c, headerLen, uncompressedLen, rows, payloads)
	if err != nil {
		return nil, blockSummary{}, fmt.Errorf("tracepack: encode a block: %w", err)
	}
	if e.hook != nil {
		raw = append(raw[:format.EnvelopeLen], e.hook(raw[format.EnvelopeLen:])...)
		e.raw = raw
	}
	enc := raw[format.EnvelopeLen:]
	if len(enc) > int(format.MaxLen32) {
		return nil, blockSummary{}, fmt.Errorf("tracepack: encode a block: body_len %d above %d", len(enc), format.MaxLen32)
	}

	// checkRawRecords checked every value narrowed here: headerLen, the record count, which is below 2^31, and uncompressedLen.
	env := format.BlockEnvelope{
		Codec:           uint8(used),
		RecordHeaderLen: uint16(headerLen),
		BodyLen:         uint32(len(enc)),
		UncompressedLen: uint32(uncompressedLen),
		RecordCount:     uint32(len(rows)),
		BodyCRC:         format.CRC(enc),
		FirstSeq:        firstSeq,
	}
	// The envelope fills the 40 bytes build left in front of the body.
	format.AppendBlockEnvelope(raw[:0], &env)

	s, err := checkRawBlock(&e.check, raw, rows, payloads)
	if err != nil {
		return nil, blockSummary{}, fmt.Errorf("tracepack: the new block of seq %d fails its check: %w", firstSeq, err)
	}

	return raw, s, nil
}

// appendRawBody appends to dst the decoded body of the records rows and payloads, each row headerLen bytes long:
// the header section, the rows' bytes column by column (the tracepack format specification §6), then the payloads.
// The caller checked the header section's length, len(rows) × headerLen.
func appendRawBody(dst []byte, headerLen int, rows, payloads [][]byte) []byte {
	n, start := len(rows), len(dst)
	dst = slices.Grow(dst, n*headerLen)[:start+n*headerLen]
	section := dst[start:]
	for i, row := range rows {
		for j, b := range row {
			section[j*n+i] = b
		}
	}
	for _, p := range payloads {
		dst = append(dst, p...)
	}

	return dst
}

// checkRawRecords checks the records encodeRawBlock takes as rows and payloads, before anything is allocated,
// and returns the length of their decoded body and their first seq.
// The decoded body is record_count × headerLen + Σ payload_len,
// each product and sum checked against the 2^31-1 limit before it is taken.
// Each record's payload_len is added to that sum before its payload's length is compared with it.
func checkRawRecords(headerLen int, rows, payloads [][]byte) (int, uint64, error) {
	switch {
	case headerLen < format.RecordHeaderLen || headerLen > math.MaxUint16:
		return 0, 0, fmt.Errorf("record_header_len %d outside %d to %d", headerLen, format.RecordHeaderLen, math.MaxUint16)
	case len(rows) == 0:
		return 0, 0, errors.New("no records")
	case len(payloads) != len(rows):
		return 0, 0, fmt.Errorf("%d record headers and %d payloads", len(rows), len(payloads))
	case uint64(len(rows)) > uint64(format.MaxLen32)/uint64(headerLen):
		return 0, 0, fmt.Errorf("a header section of %d records of %d bytes exceeds %d bytes", len(rows), headerLen, format.MaxLen32)
	}

	n := uint64(len(rows)) * uint64(headerLen)
	var first, prev uint64
	for i, row := range rows {
		h, err := rawRecordHeader(i, row, headerLen)
		if err != nil {
			return 0, 0, err
		}
		if i == 0 {
			first = h.Seq
		}
		if err := checkBlockSeq(i, h.Seq, prev, first); err != nil {
			return 0, 0, err
		}
		prev = h.Seq

		n += uint64(h.PayloadLen)
		if n > uint64(format.MaxLen32) {
			return 0, 0, fmt.Errorf("record %d: the decoded body exceeds %d bytes", i, format.MaxLen32)
		}
		if p := payloads[i]; uint64(len(p)) != uint64(h.PayloadLen) {
			return 0, 0, fmt.Errorf("record %d: payload of %d bytes, payload_len %d", i, len(p), h.PayloadLen)
		}
	}

	return int(n), first, nil
}

// rawRecordHeader decodes the 44 bytes format 1.0 defines of row, record i's header as stored,
// after checking that row is headerLen bytes long.
// The extension area is not copied.
func rawRecordHeader(i int, row []byte, headerLen int) (format.RecordHeader, error) {
	if len(row) != headerLen {
		return format.RecordHeader{}, fmt.Errorf("record %d: header of %d bytes, record_header_len %d", i, len(row), headerLen)
	}
	h, err := format.UnmarshalRecordHeader(row, format.RecordHeaderLen)
	if err != nil {
		return format.RecordHeader{}, fmt.Errorf("record %d: %w", i, err)
	}

	return h, nil
}

// checkRawBlock decodes raw, a block encodeRawBlock built, as a reader decodes it (decodeBlock), with buf's buffers,
// which checks I-2 on its gathered record headers,
// then compares every record's header row and payload with rows and payloads, its source.
// It returns the summary of the decoded records.
func checkRawBlock(buf *blockBuf, raw []byte, rows, payloads [][]byte) (blockSummary, error) {
	env, err := format.UnmarshalBlockEnvelope(raw)
	if err != nil {
		return blockSummary{}, err
	}
	buf.raw = raw
	d, _, err := decodeBlock(&env, buf)
	if err != nil {
		return blockSummary{}, err
	}

	rhl := int(env.RecordHeaderLen)
	for i := range d.count() {
		switch {
		case !bytes.Equal(d.section[i*rhl:(i+1)*rhl], rows[i]):
			return blockSummary{}, fmt.Errorf("record %d: the decoded header differs from its source", i)
		case !bytes.Equal(d.payload(i), payloads[i]):
			return blockSummary{}, fmt.Errorf("record %d: the decoded payload differs from its source", i)
		}
	}

	return summaryOf(d), nil
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

// encodeBuf holds the buffers encodeRawBlock reuses from one block to the next,
// so an encoding allocates only for a block larger than every block encoded before it with the same encodeBuf,
// besides the summary it returns.
type encodeBuf struct {
	// raw holds the block built last: its envelope, then its on-disk body.
	// With the none codec the decoded body is built in place behind the envelope.
	raw []byte
	// body holds the decoded body of a block encoded with another codec, and enc its encoded body.
	body []byte
	enc  []byte
	// check holds the buffers of the in-memory check, which decodes the block as a reader does.
	check blockBuf
	// hook, set only by tests, changes the on-disk body of every block before the block is checked.
	hook func(enc []byte) []byte
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

// build builds in e's buffers the block of the records rows and payloads, whose decoded body is uncompressedLen bytes,
// with its envelope left to fill: 40 bytes, then the on-disk body, the decoded body encoded with c.
// It returns the block, which aliases e.raw, and the codec encodeBlock used.
func (e *encodeBuf) build(c Codec, headerLen, uncompressedLen int, rows, payloads [][]byte) ([]byte, Codec, error) {
	if c == CodecNone {
		raw := slices.Grow(e.raw[:0], format.EnvelopeLen+uncompressedLen)[:format.EnvelopeLen]
		e.raw = appendRawBody(raw, headerLen, rows, payloads)

		return e.raw, CodecNone, nil
	}

	e.body = appendRawBody(slices.Grow(e.body[:0], uncompressedLen), headerLen, rows, payloads)
	enc, used, err := encodeBlock(c, e.enc, e.body)
	if err != nil {
		return nil, c, err
	}
	if used == c {
		e.enc = enc
	}
	e.raw = slices.Grow(e.raw[:0], format.EnvelopeLen+len(enc))[:format.EnvelopeLen]
	e.raw = append(e.raw, enc...)

	return e.raw, used, nil
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

// clone returns a copy of s that shares no memory with it, without its epoch index or verbatim F-3 list.
func (s *blockSummary) clone() blockSummary {
	c := *s
	c.kindCounts = slices.Clone(s.kindCounts)
	c.dirCounts = slices.Clone(s.dirCounts)
	c.decodeStatusCounts = slices.Clone(s.decodeStatusCounts)
	c.epochs = slices.Clone(s.epochs)
	c.epochIndex = nil
	c.seqRanges = slices.Clone(s.seqRanges)
	c.boundaries = slices.Clone(s.boundaries)
	for i := range c.boundaries {
		c.boundaries[i].gapStart = clonePtr(c.boundaries[i].gapStart)
		c.boundaries[i].gapEnd = clonePtr(c.boundaries[i].gapEnd)
	}
	c.verbatimF3 = nil

	return c
}

// commitmentFacts reports, as Append's commitment check counts them, whether any record of s is classified,
// with a decode_status other than not-attempted and not-applicable, a value outside the registry included;
// whether any is oversized; and whether any carries quality.redacted.
func (s *blockSummary) commitmentFacts() (classified, oversized, redacted bool) {
	for v, n := range s.decodeStatusCounts {
		if n > 0 && DecodeStatus(v) != DecodeStatusNotAttempted && DecodeStatus(v) != DecodeStatusNotApplicable {
			classified = true

			break
		}
	}
	oversized = anyCounted(s.decodeStatusCounts, []DecodeStatus{DecodeStatusOversized})

	return classified, oversized, s.qualityUnion.Has(QualityRedacted)
}

// checkStoredCommitments rejects the records of a stored pack, as facts describe them,
// when its pack metadata meta cannot describe them:
// a classified record without classifier, an oversized record without max_frame_len,
// or a redacted record, which no stored pack holds.
// Only the AnyClassified, AnyOversized and AnyRedacted facts are read.
func checkStoredCommitments(meta *PackMeta, facts PackFacts) error {
	switch {
	case facts.AnyClassified && len(meta.Classifiers) == 0:
		return errors.New("a classified record without classifier in the pack metadata")
	case facts.AnyOversized && len(meta.MaxFrameLens) == 0:
		return errors.New("an oversized record without max_frame_len in the pack metadata")
	case facts.AnyRedacted:
		return errors.New("a redacted record, which no stored pack holds")
	default:
		return nil
	}
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
	i, found, examined := s.findEpoch(h.Epoch)
	if epochProbe != nil {
		epochProbe(examined)
	}

	if found {
		e := &s.epochs[i]
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
	i = len(s.epochs) - 1
	if s.epochIndex != nil {
		s.epochIndex[h.Epoch] = i
	}

	return &s.epochs[i]
}

// findEpoch returns the position of epoch's entry in s.epochs, whether there is one,
// and the number of entries it examined.
// It checks the last entry first, since consecutive records mostly share an epoch;
// then it compares the other entries when there are at most maxLinearEpochs,
// or else looks the epoch up in s.epochIndex, which it builds from s.epochs when there is none.
func (s *blockSummary) findEpoch(epoch uint32) (i int, found bool, examined int) {
	n := len(s.epochs)
	if n == 0 {
		return 0, false, 0
	}
	if s.epochs[n-1].epoch == epoch {
		return n - 1, true, 1
	}

	if n <= maxLinearEpochs {
		for j := range n - 1 {
			if s.epochs[j].epoch == epoch {
				return j, true, j + 2
			}
		}

		return 0, false, n
	}

	examined = 1
	if s.epochIndex == nil {
		s.epochIndex = make(map[uint32]int, n)
		for j := range s.epochs {
			s.epochIndex[s.epochs[j].epoch] = j
		}
		examined += n
	}
	i, found = s.epochIndex[epoch]

	return i, found, examined + 1
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
