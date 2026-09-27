package tracepack

import (
	"bytes"
	"encoding/binary"
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

// testDataRecord returns a data record over blockTestFrame with its copy fields set.
func testDataRecord(seq uint64, ts int64, epoch uint32) Record {
	r := Record{
		Seq: seq, TSUTCNs: ts, MonoPresent: true, Epoch: epoch,
		Kind: KindData, Dir: DirHostToEquipment, DecodeStatus: DecodeStatusOK, Payload: blockTestFrame,
	}
	r.SetHeaderCopies()

	return r
}

// testEventRecord returns a transport-event record carrying ev.
func testEventRecord(t *testing.T, seq uint64, ts int64, epoch uint32, ev *TransportEvent) Record {
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
		h := canonicalHeader(&recs[i], recs[i].Seq)
		b.add(&h, recs[i].Payload, transportEventOf(&recs[i]))
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

func TestValidateBodyAcceptsBuiltBlock(t *testing.T) {
	t.Parallel()

	b := buildBlock([]Record{
		testDataRecord(4, blockTestHour, 1),
		testDataRecord(5, blockTestHour+1, 1),
		testDataRecord(9, blockTestHour+2, 2),
	})

	require.NoError(t, validateBody(b.body(nil), 3, 4))
}

func TestValidateBodyRejectsTamperedBody(t *testing.T) {
	t.Parallel()

	// Record header offsets of the tracepack format specification §7.1.
	const (
		seqOff        = 0
		payloadLenOff = 28
		streamOff     = 44
		sysBytesOff   = 36
	)

	tests := []struct {
		name     string
		tamper   func(body []byte) []byte
		count    uint32
		firstSeq uint64
	}{
		{"record_count one too many", nil, 4, 4},
		{"record_count one too few", nil, 2, 4},
		{"first seq differs from first_seq", nil, 3, 3},
		{"seq repeats", func(b []byte) []byte {
			binary.LittleEndian.PutUint64(b[recordHeaderLen+seqOff:], 4)
			return b
		}, 3, 4},
		{"seq decreases", func(b []byte) []byte {
			binary.LittleEndian.PutUint64(b[2*recordHeaderLen+seqOff:], 3)
			return b
		}, 3, 4},
		{"payload_len overruns the body", func(b []byte) []byte {
			binary.LittleEndian.PutUint32(b[2*recordHeaderLen+payloadLenOff:], 15)
			return b
		}, 3, 4},
		{"payload_len leaves bytes uncovered", func(b []byte) []byte {
			binary.LittleEndian.PutUint32(b[2*recordHeaderLen+payloadLenOff:], 13)
			return b
		}, 3, 4},
		{"body truncated", func(b []byte) []byte { return b[:len(b)-1] }, 3, 4},
		{"header section longer than the body", func(b []byte) []byte { return b[:2*recordHeaderLen] }, 3, 4},
		{"stream copy disagrees", func(b []byte) []byte {
			b[recordHeaderLen+streamOff] = 6
			return b
		}, 3, 4},
		{"system bytes copy disagrees", func(b []byte) []byte {
			b[sysBytesOff] ^= 0xFF
			return b
		}, 3, 4},
		{"W copy disagrees", func(b []byte) []byte {
			b[recordHeaderLen+53] &^= byte(RecordFlagsW)
			return b
		}, 3, 4},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			b := buildBlock([]Record{
				testDataRecord(4, blockTestHour, 1),
				testDataRecord(5, blockTestHour+1, 1),
				testDataRecord(9, blockTestHour+2, 2),
			})
			body := b.body(nil)
			if tt.tamper != nil {
				body = tt.tamper(body)
			}

			require.Error(t, validateBody(body, tt.count, tt.firstSeq))
		})
	}
}

func TestValidateBodyIgnoresCopiesOfOtherKinds(t *testing.T) {
	t.Parallel()

	ev := testEventRecord(t, 0, blockTestHour, 0, &TransportEvent{Event: EventClockStep})
	b := buildBlock([]Record{ev})
	require.NoError(t, validateBody(b.body(nil), 1, 0))
}

func TestValidateEncodedRejectsBrokenStream(t *testing.T) {
	t.Parallel()

	b := buildBlock([]Record{testDataRecord(0, blockTestHour, 1)})
	body := b.body(nil)

	enc, c, err := encodeBlock(CodecZstd, nil, body)
	require.NoError(t, err)
	require.Equal(t, CodecZstd, c)

	_, err = validateEncoded(c, nil, enc, &b.summary, len(body))
	require.NoError(t, err)

	_, err = validateEncoded(c, nil, enc[:len(enc)-1], &b.summary, len(body))
	require.Error(t, err, "a truncated frame does not complete")

	_, err = validateEncoded(c, nil, enc, &b.summary, len(body)+1)
	require.Error(t, err, "the frame decodes to fewer bytes than uncompressed_len")
}

func TestBlockSummaryAccumulatesFooterFacts(t *testing.T) {
	t.Parallel()

	gapStart, gapEnd := int64(100), int64(200)
	unknownKind := testDataRecord(3, blockTestHour+30, 2)
	unknownKind.Kind = Kind(7) // outside the registry: counted at its own index
	unknownKind.SetHeaderCopies()
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
		maxPayloadLen:      uint32(len(recs[0].Payload)),
		qualityUnion:       QualityNoMono | QualityDecodeFailed | QualityDirectionInferred,
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
	require.Greater(t, want.maxPayloadLen, uint32(len(blockTestFrame)), "an event payload is the largest here")

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
	require.NoError(t, w.Close())

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
	assert.Equal(t, uint64(buf.Len()), next)
	assert.Equal(t, []uint64{0, 2, 4}, []uint64{w.blocks[0].firstSeq, w.blocks[1].firstSeq, w.blocks[2].firstSeq})
	assert.Equal(t, []uint64{1, 3, 4}, []uint64{w.blocks[0].lastSeq, w.blocks[1].lastSeq, w.blocks[2].lastSeq})
}
