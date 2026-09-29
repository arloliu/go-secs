package tracepack

import (
	"bytes"
	"encoding/binary"
	"math"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// blockTestHour is 2026-09-21T14:00:00Z in nanoseconds, a whole UTC hour.
const blockTestHour int64 = 1_789_999_200_000_000_000

// blockTestFrame is an HSMS S1F3 W frame with an empty message text:
// length 10, session_id 0x1234, byte 2 = 0x81, function 3, ptype 0, stype 0, system bytes DE AD BE EF.
var blockTestFrame = []byte{0, 0, 0, 0x0A, 0x12, 0x34, 0x81, 0x03, 0x00, 0x00, 0xDE, 0xAD, 0xBE, 0xEF}

// testDataRecord returns a data record over blockTestFrame with the field_validity of a raw capture.
func testDataRecord(seq uint64, ts int64, epoch uint32) Record {
	r := Record{
		Seq: seq, TSUTCNs: ts, MonoPresent: true, Epoch: epoch,
		Kind: KindData, Dir: DirHostToEquipment, DecodeStatus: DecodeStatusOK, Payload: blockTestFrame,
	}
	r.SetCapturedFieldValidity()

	return r
}

// testEventRecord returns a transport-event record carrying ev.
func testEventRecord(t testing.TB, seq uint64, ts int64, epoch uint32, ev *TransportEvent) Record {
	t.Helper()

	p, err := ev.MarshalBinary()
	require.NoError(t, err)

	return Record{
		Seq: seq, TSUTCNs: ts, Epoch: epoch, Kind: KindTransportEvent, Dir: DirLocal,
		DecodeStatus: DecodeStatusNotApplicable, Payload: p,
	}
}

// buildBlock adds recs to a fresh blockBuilder the way Writer.Append does.
func buildBlock(recs []Record) *blockBuilder {
	var b blockBuilder
	for i := range recs {
		ev := transportEventOf(&recs[i])
		h := canonicalHeader(&recs[i], recs[i].Seq, ev)
		b.add(&h, recs[i].Payload, ev)
	}

	return &b
}

func TestHourOf(t *testing.T) {
	t.Parallel()

	assert.Equal(t, int64(0), hourOf(0))
	assert.Equal(t, int64(0), hourOf(hourNs-1))
	assert.Equal(t, int64(1), hourOf(hourNs))
	assert.Equal(t, int64(-1), hourOf(-1))
	assert.Equal(t, int64(-1), hourOf(-hourNs))
	assert.Equal(t, int64(-2), hourOf(-hourNs-1))
	assert.Equal(t, int64(497222), hourOf(blockTestHour))
}

func TestExceedsLimitDoesNotOverflow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		size, add, limit int
		want             bool
	}{
		{"sum below the limit", 10, 20, 100, false},
		{"sum equal to the limit", 80, 20, 100, false},
		{"sum above the limit", 81, 20, 100, true},
		{"sum past MaxInt32", math.MaxInt32 - 10, 20, math.MaxInt32, true},
		{"largest record on a near-full block", math.MaxInt32 - 1, math.MaxInt32, math.MaxInt32, true},
		{"sum past MaxInt", math.MaxInt - 10, 20, math.MaxInt, true},
		{"sum at MaxInt32", math.MaxInt32 - 20, 20, math.MaxInt32, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, exceedsLimit(tt.size, tt.add, tt.limit))
		})
	}
}

func TestValidateBodyAcceptsBuiltBlock(t *testing.T) {
	t.Parallel()

	b := buildBlock([]Record{
		testDataRecord(4, blockTestHour, 1),
		testDataRecord(5, blockTestHour+1, 1),
		testDataRecord(9, blockTestHour+2, 2),
	})

	require.NoError(t, validateBody(b.body(nil), 3, 4, &validateBuf{}))
}

// TestBlockBodyIsColumnar checks the header section a built block writes (the tracepack format specification §6):
// byte j of record i's header at offset j × record_count + i, then the payloads in record order.
func TestBlockBodyIsColumnar(t *testing.T) {
	t.Parallel()

	b := buildBlock([]Record{
		testDataRecord(4, blockTestHour, 1),
		testDataRecord(5, blockTestHour+1, 2),
	})
	body := b.body(nil)
	require.Len(t, body, 2*recordHeaderLen+2*len(blockTestFrame))

	for i := range 2 {
		row := b.headers[i*recordHeaderLen : (i+1)*recordHeaderLen]
		for j := range recordHeaderLen {
			require.Equal(t, row[j], body[j*2+i], "record %d byte %d", i, j)
		}
	}
	// The seqs 4 and 5 interleave in the first two columns: byte 0 of each, then byte 1 of each.
	assert.Equal(t, []byte{4, 5, 0, 0}, body[:4])
	assert.Equal(t, append(slices.Clone(blockTestFrame), blockTestFrame...), body[2*recordHeaderLen:])
}

func TestValidateBodyRejectsTamperedBody(t *testing.T) {
	t.Parallel()

	// Record header offsets of the tracepack format specification §7.1.
	const (
		seqOff        = 0
		payloadLenOff = 28
	)

	tests := []struct {
		name string
		// rows changes the record headers, one after another, before they are transposed.
		rows func(rows []byte)
		// body changes the built body.
		body     func(body []byte) []byte
		count    uint32
		firstSeq uint64
	}{
		{name: "record_count one too many", count: 4, firstSeq: 4},
		{name: "record_count one too few", count: 2, firstSeq: 4},
		{name: "first seq differs from first_seq", count: 3, firstSeq: 3},
		{name: "seq repeats", rows: func(b []byte) {
			binary.LittleEndian.PutUint64(b[recordHeaderLen+seqOff:], 4)
		}, count: 3, firstSeq: 4},
		{name: "seq decreases", rows: func(b []byte) {
			binary.LittleEndian.PutUint64(b[2*recordHeaderLen+seqOff:], 3)
		}, count: 3, firstSeq: 4},
		{name: "payload_len overruns the body", rows: func(b []byte) {
			binary.LittleEndian.PutUint32(b[2*recordHeaderLen+payloadLenOff:], 15)
		}, count: 3, firstSeq: 4},
		{name: "payload_len leaves bytes uncovered", rows: func(b []byte) {
			binary.LittleEndian.PutUint32(b[2*recordHeaderLen+payloadLenOff:], 13)
		}, count: 3, firstSeq: 4},
		{name: "headers left in rows", body: func(b []byte) []byte {
			// The rows as a writer of the earlier layout stored them: the first seq no longer gathers to 4.
			return append(format.UntransposeHeaders(nil, b[:3*recordHeaderLen], 3, recordHeaderLen), b[3*recordHeaderLen:]...)
		}, count: 3, firstSeq: 4},
		{name: "body truncated", body: func(b []byte) []byte { return b[:len(b)-1] }, count: 3, firstSeq: 4},
		{name: "header section longer than the body", body: func(b []byte) []byte { return b[:2*recordHeaderLen] }, count: 3, firstSeq: 4},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			b := buildBlock([]Record{
				testDataRecord(4, blockTestHour, 1),
				testDataRecord(5, blockTestHour+1, 1),
				testDataRecord(9, blockTestHour+2, 2),
			})
			if tt.rows != nil {
				tt.rows(b.headers)
			}
			body := b.body(nil)
			if tt.body != nil {
				body = tt.body(body)
			}

			require.Error(t, validateBody(body, tt.count, tt.firstSeq, &validateBuf{}))
		})
	}
}

func TestValidateBodyAcceptsEvent(t *testing.T) {
	t.Parallel()

	ev := testEventRecord(t, 0, blockTestHour, 0, &TransportEvent{Event: EventClockStep})
	b := buildBlock([]Record{ev})
	require.NoError(t, validateBody(b.body(nil), 1, 0, &validateBuf{}))
}

func TestValidateEncodedRejectsBrokenStream(t *testing.T) {
	t.Parallel()

	b := buildBlock([]Record{testDataRecord(0, blockTestHour, 1)})
	body := b.body(nil)

	enc, c, err := encodeBlock(CodecZstd, nil, body)
	require.NoError(t, err)
	require.Equal(t, CodecZstd, c)

	var buf validateBuf
	require.NoError(t, validateEncoded(c, &buf, enc, &b.summary, len(body)))
	require.Error(t, validateEncoded(c, &buf, enc[:len(enc)-1], &b.summary, len(body)), "a truncated frame does not complete")
	require.Error(t, validateEncoded(c, &buf, enc, &b.summary, len(body)+1), "the frame decodes to fewer bytes than uncompressed_len")
}

func TestBlockSummaryAccumulatesFooterFacts(t *testing.T) {
	t.Parallel()

	gapStart, gapEnd := int64(100), int64(200)
	unknownKind := testDataRecord(3, blockTestHour+30, 2)
	unknownKind.Kind = Kind(7) // outside the registry: counted at its own index
	unknownKind.SetCapturedFieldValidity()
	unknownKind.Quality = QualityDirectionInferred
	malformed := testDataRecord(5, blockTestHour+10, 2)
	malformed.DecodeStatus = DecodeStatusShortFrame

	recs := []Record{
		testEventRecord(t, 1, blockTestHour+20, 0, &TransportEvent{
			Event: EventCaptureBoundary, BoundaryKind: new(BoundaryKindGap), GapStart: &gapStart, GapEnd: &gapEnd,
		}),
		testDataRecord(2, blockTestHour+5, 2),
		unknownKind,
		malformed,
		testEventRecord(t, 6, blockTestHour+40, 2, &TransportEvent{Event: EventSocketClose}),
		testEventRecord(t, 7, blockTestHour+41, 2, &TransportEvent{Event: EventSocketClose}),
		testDataRecord(8, blockTestHour+50, 3),
		testEventRecord(t, 9, blockTestHour+60, 3, &TransportEvent{
			Event: EventCaptureBoundary, BoundaryKind: new(BoundaryKindStop),
		}),
	}

	s := buildBlock(recs).summary

	want := blockSummary{
		recordCount: 8,
		firstSeq:    1, lastSeq: 9,
		tsMin: blockTestHour + 5, tsMax: blockTestHour + 60,
		epochMin: 0, epochMax: 3,
		// Kinds in order: event, data, kind 7, data, event, event, data, event.
		kindCounts:         []uint32{0, 3, 0, 4, 0, 0, 0, 1},
		dirCounts:          []uint32{0, 4, 0, 4},
		decodeStatusCounts: []uint32{0, 3, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 4},
		// The boundary events derive capture-boundary, and the epoch-0 one correlation-incomplete.
		qualityUnion: QualityDirectionInferred | QualityCaptureBoundary | QualityCorrelationIncomplete,
		epochs: []epochSummary{
			{epoch: 0, recordCount: 1, seqFirst: 1, seqLast: 1, tsMin: blockTestHour + 20, tsMax: blockTestHour + 20},
			{
				epoch: 2, recordCount: 5, seqFirst: 2, seqLast: 7, tsMin: blockTestHour + 5, tsMax: blockTestHour + 41,
				closeSeq: 6, hasCloseSeq: true,
			},
			{
				epoch: 3, recordCount: 2, seqFirst: 8, seqLast: 9, tsMin: blockTestHour + 50, tsMax: blockTestHour + 60,
				closeSeq: 9, hasCloseSeq: true,
			},
		},
		boundaries: []boundarySummary{
			{seq: 1, kind: BoundaryKindGap, ts: blockTestHour + 20, epoch: 0, gapStart: &gapStart, gapEnd: &gapEnd},
			{seq: 9, kind: BoundaryKindStop, ts: blockTestHour + 60, epoch: 3},
		},
		seqRanges: []seqRange{{1, 3}, {5, 9}},
	}
	assert.Equal(t, want, s)
}

func TestBlockSummarySkipsUndecodableEvent(t *testing.T) {
	t.Parallel()

	bad := Record{
		Seq: 0, TSUTCNs: blockTestHour, Kind: KindTransportEvent,
		DecodeStatus: DecodeStatusNotApplicable, Payload: []byte{0xFF},
	}
	s := buildBlock([]Record{bad}).summary

	assert.Equal(t, uint32(1), s.recordCount)
	assert.Empty(t, s.boundaries)
	assert.False(t, s.epochs[0].hasCloseSeq)
}

func TestWriterRecordsBlockSummaries(t *testing.T) {
	t.Parallel()

	meta := &PackMeta{
		ToolID: "tool", Recorder: "rec", Writer: "wr", Classifier: new("c"),
		PackRole: PackRoleSegment, ScopeGeneration: new(uint64(0)),
	}

	var buf bytes.Buffer
	w, err := NewWriter(&buf, WriterOptions{Meta: meta, Codec: CodecZstd, BlockThreshold: 140})
	require.NoError(t, err)
	headLen := buf.Len()

	for i := range 5 {
		r := testDataRecord(uint64(i), blockTestHour+int64(i), 1)
		require.NoError(t, w.Append(&r))
	}
	_, err = w.Close()
	require.NoError(t, err)

	require.Len(t, w.blocks, 3)
	next := uint64(headLen)
	for i, s := range w.blocks {
		assert.Equal(t, next, s.offset, "block %d starts where the previous one ends", i)

		env, err := format.UnmarshalBlockEnvelope(buf.Bytes()[s.offset:])
		require.NoError(t, err)
		assert.Equal(t, uint32(format.EnvelopeLen)+env.BodyLen, s.onDiskLen)
		assert.Equal(t, env.UncompressedLen, s.uncompressedLen)
		assert.Equal(t, env.RecordCount, s.recordCount)
		assert.Equal(t, env.BodyCRC, s.bodyCRC)
		assert.Equal(t, env.FirstSeq, s.firstSeq)
		assert.Equal(t, env.RecordHeaderLen, s.recordHeaderLen)
		assert.Equal(t, uint8(CodecZstd), env.Codec)

		next += uint64(s.onDiskLen)
	}
	tr, err := format.UnmarshalTrailer(buf.Bytes()[buf.Len()-format.TrailerLen:])
	require.NoError(t, err)
	assert.Equal(t, tr.FooterOffset, next, "the footer follows the last block")
	assert.Equal(t, []uint64{0, 2, 4}, []uint64{w.blocks[0].firstSeq, w.blocks[1].firstSeq, w.blocks[2].firstSeq})
	assert.Equal(t, []uint64{1, 3, 4}, []uint64{w.blocks[0].lastSeq, w.blocks[1].lastSeq, w.blocks[2].lastSeq})
}
