package tracepack

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/codec"
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

// TestBoundaryBitMakesNoBoundaryRecord stores quality bit 0 (capture-boundary) on records that are not capture-boundary records,
// which the Writer never does, so the block is encoded from raw header rows:
// a data record and a socket-close event of epoch 1, and, in epoch 2, a transport-event record whose payload is not a valid TLV body.
// Only a capture-boundary event in the payload makes a boundary entry (the tracepack format specification §10),
// so F-3 and F-5 hold the bit in quality_union and no boundary entry;
// the socket-close event still ends epoch 1, and the undecodable payload ends no epoch and is a writer defect.
// The pack verifies finalized-consistent.
func TestBoundaryBitMakesNoBoundaryRecord(t *testing.T) {
	t.Parallel()

	undecodable := Record{
		Seq: 2, TSUTCNs: blockTestHour + 2, Epoch: 2, Kind: KindTransportEvent, Dir: DirLocal,
		DecodeStatus: DecodeStatusNotApplicable, Payload: []byte{0xFF},
	}
	recs := []Record{
		testDataRecord(0, blockTestHour, 1),
		testEventRecord(t, 1, blockTestHour+1, 1, &TransportEvent{Event: EventSocketClose}),
		undecodable,
	}
	rows, payloads := make([][]byte, 0, len(recs)), make([][]byte, 0, len(recs))
	for i := range recs {
		h := canonicalHeader(&recs[i], recs[i].Seq, transportEventOf(&recs[i]))
		require.Zero(t, h.Quality&uint16(QualityCaptureBoundary), "record %d: the Writer would not set the bit", i)
		h.Quality |= uint16(QualityCaptureBoundary)
		rows = append(rows, format.AppendRecordHeader(nil, &h))
		payloads = append(payloads, recs[i].Payload)
	}

	raw, sum, err := encodeRawBlock(&encodeBuf{}, CodecZstd, format.RecordHeaderLen, rows, payloads)
	require.NoError(t, err)
	assert.Empty(t, sum.boundaries)
	assert.True(t, sum.qualityUnion.Has(QualityCaptureBoundary))
	require.Len(t, sum.epochs, 2)
	assert.Equal(t, epochSummary{
		epoch: 1, recordCount: 2, seqFirst: 0, seqLast: 1, tsMin: blockTestHour, tsMax: blockTestHour + 1,
		closeSeq: 1, hasCloseSeq: true,
	}, sum.epochs[0])
	assert.False(t, sum.epochs[1].hasCloseSeq, "an undecodable payload ends no epoch")

	meta := &PackMeta{
		ToolID: "tool", Recorder: "rec", Writer: "wr", Classifiers: []string{"c"},
		PackRole: PackRoleSegment, ScopeGeneration: new(uint64(0)),
		PeriodStart: blockTestHour, PeriodEnd: blockTestHour + hourNs,
	}
	pack := copyBlocks(t, WriterOptions{Meta: meta, Codec: CodecZstd}, []sourceBlock{{raw: raw, sum: sum}})

	rep := mustVerify(t, pack)
	assert.Equal(t, OutcomeFinalizedConsistent, rep.Outcome, "failed %v, disagreements %v", rep.Failed, rep.Disagreements)
	require.Len(t, rep.WriterDefects, 1)
	assert.Equal(t, WriterDefectEventPayload, rep.WriterDefects[0].Kind)
	assert.Equal(t, uint64(2), rep.WriterDefects[0].Seq)

	tr, decoded, blocksStart := splitPack(t, pack)
	idx, err := parseFooter(decoded, &tr, blocksStart, DefaultMaxFooterLen, false)
	require.NoError(t, err)
	require.Len(t, idx.blocks, 1)
	assert.Empty(t, idx.blocks[0].boundaries, "F-3 boundary entries")
	assert.True(t, idx.blocks[0].qualityUnion.Has(QualityCaptureBoundary), "F-3 quality_union")
	st := footerStats(t, decoded)
	assert.Empty(t, st.boundaries, "F-5 boundary entries")
	assert.True(t, st.qualityUnion.Has(QualityCaptureBoundary), "F-5 quality_union")
}

func TestWriterRecordsBlockSummaries(t *testing.T) {
	t.Parallel()

	meta := &PackMeta{
		ToolID: "tool", Recorder: "rec", Writer: "wr", Classifiers: []string{"c"},
		PackRole: PackRoleSegment, ScopeGeneration: new(uint64(0)),
		PeriodStart: blockTestHour, PeriodEnd: blockTestHour + hourNs,
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

// epochRecord is one record of the epoch summary tests: its header, and its transport event or nil.
type epochRecord struct {
	h  format.RecordHeader
	ev *TransportEvent
}

// epochRecords returns one record per element of epochs, in that epoch, with consecutive seqs from firstSeq
// and timestamps that do not follow the seqs, so every epoch's ts range moves both ways.
// The records at the positions closes lists are socket-close events.
func epochRecords(firstSeq uint64, epochs []uint32, closes ...int) []epochRecord {
	recs := make([]epochRecord, len(epochs))
	for i, e := range epochs {
		seq := firstSeq + uint64(i)
		recs[i].h = format.RecordHeader{Seq: seq, TSUTCNs: blockTestHour + int64(seq*7919%1000), Epoch: e}
		if slices.Contains(closes, i) {
			recs[i].ev = &TransportEvent{Event: EventSocketClose}
		}
	}

	return recs
}

// epochRange returns the epochs from first to last, ascending or descending.
func epochRange(first, last uint32) []uint32 {
	var out []uint32
	for e := first; ; {
		out = append(out, e)
		switch {
		case e == last:
			return out
		case e < last:
			e++
		default:
			e--
		}
	}
}

// summarize folds recs into s, in order.
func summarize(s *blockSummary, recs []epochRecord) {
	for i := range recs {
		s.addRecord(&recs[i].h, recs[i].ev)
	}
}

// foldEpochs returns the epoch entries of recs as a linear search over the entries finds them:
// one per epoch in order of first appearance, keeping each epoch's first socket-close seq.
func foldEpochs(recs []epochRecord) []epochSummary {
	var out []epochSummary
	for _, r := range recs {
		i := slices.IndexFunc(out, func(e epochSummary) bool { return e.epoch == r.h.Epoch })
		if i < 0 {
			out = append(out, epochSummary{epoch: r.h.Epoch, seqFirst: r.h.Seq, tsMin: r.h.TSUTCNs, tsMax: r.h.TSUTCNs})
			i = len(out) - 1
		}
		e := &out[i]
		e.recordCount++
		e.seqLast = r.h.Seq
		e.tsMin, e.tsMax = min(e.tsMin, r.h.TSUTCNs), max(e.tsMax, r.h.TSUTCNs)
		if r.ev != nil && !e.hasCloseSeq {
			e.closeSeq, e.hasCloseSeq = r.h.Seq, true
		}
	}

	return out
}

// requireEpochIndex checks that s's epoch index, when s holds one, names the entry of every epoch of s.
func requireEpochIndex(t *testing.T, s *blockSummary) {
	t.Helper()

	if s.epochIndex == nil {
		return
	}
	require.Len(t, s.epochIndex, len(s.epochs))
	for i := range s.epochs {
		require.Equal(t, i, s.epochIndex[s.epochs[i].epoch], "epoch %d", s.epochs[i].epoch)
	}
}

// TestBlockSummaryEpochLookupIsLinear checks that a block summary finds a record's epoch entry in bounded work:
// 100 000 records in 100 000 epochs, then 100 000 records revisiting them in reverse,
// examine at most three entries per record in total, where a linear search examines about n²/2 per pass.
// It is not parallel because it sets epochProbe, a package variable:
// Go starts the parallel tests only after every sequential top-level test has finished.
func TestBlockSummaryEpochLookupIsLinear(t *testing.T) {
	var examined int
	epochProbe = func(n int) { examined += n }
	t.Cleanup(func() { epochProbe = nil })

	const n = 100_000
	recs := slices.Concat(epochRecords(0, epochRange(0, n-1)), epochRecords(n, epochRange(n-1, 0)))
	var s blockSummary
	summarize(&s, recs)
	assert.LessOrEqual(t, examined, 3*len(recs), "entries examined for %d records", len(recs))

	want := make([]epochSummary, n)
	for i := range want {
		seq := uint64(i)
		want[i] = epochSummary{
			epoch: uint32(i), recordCount: 2, seqFirst: seq, seqLast: 2*n - 1 - seq,
			tsMin: min(recs[i].h.TSUTCNs, recs[2*n-1-i].h.TSUTCNs), tsMax: max(recs[i].h.TSUTCNs, recs[2*n-1-i].h.TSUTCNs),
		}
	}
	assert.Equal(t, want, s.epochs)
	requireEpochIndex(t, &s)
}

// TestBlockSummaryEpochsScaleLinearly summarizes blocks of 10 000 and 100 000 records, each record in an epoch of its own,
// as BenchmarkBlockSummaryEpochs does:
// each block allocates at most 1 KiB per record,
// and the larger one allocates at most 20 times what the smaller one does and takes at most 20 times as long,
// where a search over the epoch entries would take about 100 times as long.
// A block's time is the fastest of several runs, which keeps a stray pause out of the ratio;
// no absolute time is asserted, since it would depend on the machine and its load.
// It is not parallel: no other test of the package runs beside the timed runs,
// and it measures the process-wide TotalAlloc.
func TestBlockSummaryEpochsScaleLinearly(t *testing.T) {
	const runs = 5

	// measure returns the fastest of the runs that summarize n records in n epochs, and what one run allocates.
	measure := func(n uint32) (time.Duration, uint64) {
		recs := epochRecords(0, epochRange(0, n-1))
		best := time.Duration(math.MaxInt64)
		var alloc uint64
		for range runs {
			var s blockSummary
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			start := time.Now()
			summarize(&s, recs)
			d := time.Since(start)
			runtime.ReadMemStats(&after)

			require.Len(t, s.epochs, int(n))
			best, alloc = min(best, d), after.TotalAlloc-before.TotalAlloc
		}

		return best, alloc
	}

	smallTime, smallAlloc := measure(10_000)
	largeTime, largeAlloc := measure(100_000)
	t.Logf("10 000 records: %v, %d bytes; 100 000 records: %v, %d bytes", smallTime, smallAlloc, largeTime, largeAlloc)
	assert.LessOrEqual(t, smallAlloc, uint64(10_000<<10), "at most 1 KiB per record")
	assert.LessOrEqual(t, largeAlloc, uint64(100_000<<10), "at most 1 KiB per record")
	assert.LessOrEqual(t, largeAlloc, 20*smallAlloc, "allocation grows linearly")
	assert.LessOrEqual(t, largeTime, 20*smallTime, "time grows linearly")
}

// TestBlockSummaryEpochIndex checks that a block summary's epoch entries equal a linear search's,
// below the threshold where it keeps an epoch index, across it, and when records revisit earlier epochs,
// the first socket-close seq of an epoch included.
func TestBlockSummaryEpochIndex(t *testing.T) {
	t.Parallel()

	// indexed says whether a lookup found more than maxLinearEpochs entries, so the summary built its epoch index.
	tests := []struct {
		name    string
		epochs  []uint32
		closes  []int
		indexed bool
	}{
		{"one epoch", []uint32{7, 7, 7, 7}, []int{1, 3}, false},
		{"at most eight epochs, revisited", []uint32{1, 2, 1, 3, 4, 5, 6, 7, 8, 2, 8, 1}, []int{2, 9}, false},
		{"nine epochs, the last one new", epochRange(0, 8), nil, false},
		{"nine epochs, then the last one again", slices.Concat(epochRange(0, 8), []uint32{8}), nil, false},
		{"nine epochs, then an earlier one", slices.Concat(epochRange(0, 8), []uint32{4}), nil, true},
		{
			// Epoch 3 closes before the index exists and again after it; epoch 5 closes only after it.
			"crossing the threshold, revisited", slices.Concat(epochRange(0, 20), []uint32{0, 5, 20, 9, 9, 3, 5}),
			[]int{3, 22, 26}, true,
		},
		{"descending, then revisited", slices.Concat(epochRange(30, 0), []uint32{30, 0, 15}), []int{31}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			recs := epochRecords(10, tt.epochs, tt.closes...)
			var s blockSummary
			summarize(&s, recs)
			assert.Equal(t, foldEpochs(recs), s.epochs)
			assert.Equal(t, tt.indexed, s.epochIndex != nil)
			requireEpochIndex(t, &s)
		})
	}
}

// TestBlockSummaryCloneHoldsNoEpochIndex checks that a clone of a summary with an epoch index shares none with it:
// records added to each afterwards reach only that one, which builds its own index when it needs one.
func TestBlockSummaryCloneHoldsNoEpochIndex(t *testing.T) {
	t.Parallel()

	for _, n := range []uint32{4, 12} {
		recs := slices.Concat(epochRecords(0, epochRange(0, n-1)), epochRecords(uint64(n), []uint32{2}))
		var s blockSummary
		summarize(&s, recs)
		require.Equal(t, n > maxLinearEpochs, s.epochIndex != nil, "%d epochs", n)

		c := s.clone()
		assert.Nil(t, c.epochIndex, "%d epochs", n)

		// Each side gains epochs past the threshold and revisits one of the shared ones.
		sMore := epochRecords(100, slices.Concat(epochRange(100, 110), []uint32{1, 105}), 11)
		cMore := epochRecords(100, slices.Concat(epochRange(200, 210), []uint32{1, 205}), 12)
		summarize(&s, sMore)
		summarize(&c, cMore)

		assert.Equal(t, foldEpochs(slices.Concat(recs, sMore)), s.epochs, "%d epochs", n)
		assert.Equal(t, foldEpochs(slices.Concat(recs, cMore)), c.epochs, "%d epochs", n)
		requireEpochIndex(t, &s)
		requireEpochIndex(t, &c)
		assert.NotContains(t, s.epochIndex, uint32(200), "%d epochs", n)
		assert.NotContains(t, c.epochIndex, uint32(100), "%d epochs", n)
	}
}

// TestBlockBuilderResetDropsEpochIndex checks that resetting the open block drops its epoch index with its epochs,
// so the next block's summary holds only its own records.
func TestBlockBuilderResetDropsEpochIndex(t *testing.T) {
	t.Parallel()

	var b blockBuilder
	for _, r := range epochRecords(0, slices.Concat(epochRange(0, 11), []uint32{3})) {
		b.add(&r.h, nil, r.ev)
	}
	require.NotNil(t, b.summary.epochIndex)

	b.reset()
	assert.Nil(t, b.summary.epochIndex)

	next := epochRecords(20, slices.Concat(epochRange(20, 11), []uint32{3, 20}), 13)
	for _, r := range next {
		b.add(&r.h, nil, r.ev)
	}
	assert.Equal(t, foldEpochs(next), b.summary.epochs)
	requireEpochIndex(t, &b.summary)
}

// TestWriterKeepsNoEpochIndex checks a block whose records span more epochs than a summary searches linearly:
// the Writer keeps its summary without the epoch index, a full read summarizes it to the same epoch entries,
// and the pack verifies finalized-consistent.
func TestWriterKeepsNoEpochIndex(t *testing.T) {
	t.Parallel()

	meta := &PackMeta{
		ToolID: "tool", Recorder: "rec", Writer: "wr", Classifiers: []string{"c"},
		PackRole: PackRoleSegment, ScopeGeneration: new(uint64(0)),
		PeriodStart: blockTestHour, PeriodEnd: blockTestHour + hourNs,
	}
	var buf bytes.Buffer
	w, err := NewWriter(&buf, WriterOptions{Meta: meta, Codec: CodecZstd, BlockThreshold: 1 << 16})
	require.NoError(t, err)
	epochs := slices.Concat(epochRange(0, 11), []uint32{3, 0, 11, 3})
	for i, e := range epochs {
		r := testDataRecord(uint64(i), blockTestHour+int64(i), e)
		require.NoError(t, w.Append(&r))
	}
	_, err = w.Close()
	require.NoError(t, err)

	require.Len(t, w.blocks, 1)
	assert.Nil(t, w.blocks[0].epochIndex)
	require.Len(t, w.blocks[0].epochs, 12)

	_, blocks := readSourceBlocks(t, buf.Bytes())
	require.Len(t, blocks, 1)
	assert.Nil(t, blocks[0].sum.epochIndex)
	assert.Equal(t, w.blocks[0].epochs, blocks[0].sum.epochs)

	v := mustVerify(t, buf.Bytes())
	assert.Equal(t, OutcomeFinalizedConsistent, v.Outcome, "failed %v, disagreements %v", v.Failed, v.Disagreements)
}

// rawTestRecords returns the header rows, headerLen bytes each, and the payloads of the rich footer test pack's records in its first UTC hour
// (seqs 10, 11, 13, 14, 15, 20 and 21: data records, a start and a gap boundary, a socket-close),
// each header as the Writer stores it, then changed as no record header encoder writes it:
// the retired quality bits 3 and 6, the reserved quality bit 15, the reserved field_validity bit 7,
// the retired record_flags bit 0 and the reserved record_flags bit 5 set,
// and headerLen − 44 extension bytes that differ from record to record.
func rawTestRecords(t testing.TB, headerLen int) ([][]byte, [][]byte) {
	t.Helper()

	var rows, payloads [][]byte
	for _, st := range richFooterSteps(t) {
		r := st.rec
		if hourOf(r.TSUTCNs) != hourOf(blockTestHour) {
			continue
		}
		h := canonicalHeader(&r, r.Seq, transportEventOf(&r))
		for k := range headerLen - format.RecordHeaderLen {
			h.Extra = append(h.Extra, byte(int(r.Seq)*7+k))
		}
		row := format.AppendRecordHeader(nil, &h)
		binary.LittleEndian.PutUint16(row[36:], h.Quality|1<<3|1<<6|1<<15)
		row[42] |= 1 << 7
		row[43] |= 1<<0 | 1<<5
		rows = append(rows, row)
		payloads = append(payloads, r.Payload)
	}
	require.Len(t, rows, 7)

	return rows, payloads
}

// TestEncodeRawBlockKeepsRecordBytes checks that a block encodeRawBlock builds holds every record byte for byte:
// each header row, reserved and retired bits and extension bytes included, and each payload,
// for both codecs and record_header_len 44, 45 to 55 and 65535.
// The block decodes as a reader decodes it, and the summary returned is the one a full read of the block gives.
func TestEncodeRawBlockKeepsRecordBytes(t *testing.T) {
	t.Parallel()

	lens := []int{format.RecordHeaderLen, math.MaxUint16}
	for n := 45; n <= 55; n++ {
		lens = append(lens, n)
	}
	for _, c := range []Codec{CodecNone, CodecZstd} {
		for _, hl := range lens {
			t.Run(fmt.Sprintf("%s/%d", c, hl), func(t *testing.T) {
				t.Parallel()

				rows, payloads := rawTestRecords(t, hl)
				raw, sum, err := encodeRawBlock(&encodeBuf{}, c, hl, rows, payloads)
				require.NoError(t, err)

				env, err := format.UnmarshalBlockEnvelope(raw)
				require.NoError(t, err)
				assert.Equal(t, uint8(c), env.Codec)
				assert.Equal(t, uint16(hl), env.RecordHeaderLen)
				assert.Equal(t, uint32(len(rows)), env.RecordCount)
				assert.Equal(t, uint64(10), env.FirstSeq)
				assert.Len(t, raw, format.EnvelopeLen+int(env.BodyLen))
				assert.Equal(t, format.CRC(raw[format.EnvelopeLen:]), env.BodyCRC)

				body, err := codec.Decode(env.Codec, nil, raw[format.EnvelopeLen:], int(env.UncompressedLen))
				require.NoError(t, err)
				n := len(rows)
				gathered := format.UntransposeHeaders(nil, body[:n*hl], n, hl)
				off := n * hl
				for i := range rows {
					assert.Equal(t, rows[i], gathered[i*hl:(i+1)*hl], "record %d header", i)
					assert.Equal(t, payloads[i], body[off:off+len(payloads[i])], "record %d payload", i)
					off += len(payloads[i])
				}
				assert.Len(t, body, off)

				meta := &PackMeta{
					ToolID: "tool", Recorder: "rec", Writer: "wr", Classifiers: []string{"c"},
					PackRole: PackRoleSegment, ScopeGeneration: new(uint64(0)), SeqStart: sum.firstSeq,
					PeriodStart: blockTestHour, PeriodEnd: blockTestHour + hourNs,
				}
				pack := copyBlocks(t, WriterOptions{Meta: meta, Codec: CodecZstd}, []sourceBlock{{raw: raw, sum: sum}})
				rep := mustVerify(t, pack)
				assert.Equal(t, OutcomeFinalizedConsistent, rep.Outcome, "failed %v, disagreements %v", rep.Failed, rep.Disagreements)
				_, blocks := readSourceBlocks(t, pack)
				require.Len(t, blocks, 1)
				assert.Equal(t, raw, blocks[0].raw)
				assert.Equal(t, blocks[0].sum, sum)
			})
		}
	}
}

// TestEncodeRawBlockReusesItsBuffers encodes blocks of several sizes and header lengths with one encodeBuf, in turn,
// some of them changed by the hook so that they fail their check:
// each block that passes equals the block a fresh encodeBuf builds,
// whatever the blocks before it, failed ones included, left in the buffers,
// and aliases the encodeBuf until its next use.
// A none block is built in place behind its envelope, without the decoded-body buffer.
func TestEncodeRawBlockReusesItsBuffers(t *testing.T) {
	t.Parallel()

	corrupt := func(enc []byte) []byte {
		enc[len(enc)-1] ^= 1

		return enc
	}
	for _, c := range []Codec{CodecNone, CodecZstd} {
		t.Run(c.String(), func(t *testing.T) {
			t.Parallel()

			var e encodeBuf
			steps := []struct {
				hl, n int
				fail  bool
			}{{52, 7, false}, {300, 7, true}, {44, 2, false}, {300, 7, false}, {45, 3, true}, {44, 1, false}, {45, 5, false}}
			for _, st := range steps {
				rows, payloads := rawTestRecords(t, st.hl)
				rows, payloads = rows[:st.n], payloads[:st.n]
				if st.fail {
					e.hook = corrupt
					raw, _, err := encodeRawBlock(&e, c, st.hl, rows, payloads)
					e.hook = nil
					require.ErrorContains(t, err, "fails its check", "record_header_len %d, %d records", st.hl, st.n)
					assert.Nil(t, raw)

					continue
				}

				want, wantSum, err := encodeRawBlock(&encodeBuf{}, c, st.hl, rows, payloads)
				require.NoError(t, err)
				raw, sum, err := encodeRawBlock(&e, c, st.hl, rows, payloads)
				require.NoError(t, err)
				assert.Equal(t, want, raw, "record_header_len %d, %d records", st.hl, st.n)
				assert.Equal(t, wantSum, sum, "record_header_len %d, %d records", st.hl, st.n)
				assert.Same(t, &e.raw[0], &raw[0], "the block aliases the encodeBuf")
				if c == CodecNone {
					assert.Empty(t, e.body, "a none body is built behind its envelope")
				}
			}
		})
	}
}

// TestEncodeRawBlockRejectsRecords checks every check encodeRawBlock runs on its records before it allocates the block,
// the limits of the tracepack format specification §2 included, which it reaches without allocating 2 GiB:
// the header section of many rows sharing one backing array, and a decoded body from a row's payload_len alone.
func TestEncodeRawBlockRejectsRecords(t *testing.T) {
	t.Parallel()

	// bigRow is a 65535-byte header row whose payload_len is 0;
	// 32769 of them make a header section one row past the 2^31-1 limit.
	bigRow := make([]byte, math.MaxUint16)
	bigRows := slices.Repeat([][]byte{bigRow}, 32769)
	bigPayloads := make([][]byte, len(bigRows))

	tests := []struct {
		name string
		// hl is the record_header_len passed; edit changes the rows and payloads of rawTestRecords(44).
		hl   int
		edit func(rows, payloads [][]byte) ([][]byte, [][]byte)
		want string
	}{
		{name: "record_header_len below 44", hl: 43, want: "record_header_len 43 outside"},
		{name: "record_header_len above 65535", hl: 65536, want: "record_header_len 65536 outside"},
		{
			name: "no records",
			edit: func(_, _ [][]byte) ([][]byte, [][]byte) { return nil, nil },
			want: "no records",
		},
		{
			name: "a payload missing",
			edit: func(rows, payloads [][]byte) ([][]byte, [][]byte) { return rows, payloads[:len(payloads)-1] },
			want: "7 record headers and 6 payloads",
		},
		{
			name: "a row shorter than record_header_len",
			edit: func(rows, payloads [][]byte) ([][]byte, [][]byte) {
				rows[2] = rows[2][:43]
				return rows, payloads
			},
			want: "record 2: header of 43 bytes",
		},
		{
			name: "a row longer than record_header_len",
			edit: func(rows, payloads [][]byte) ([][]byte, [][]byte) {
				rows[2] = append(rows[2], 0)
				return rows, payloads
			},
			want: "record 2: header of 45 bytes",
		},
		{
			name: "a payload shorter than its payload_len",
			edit: func(rows, payloads [][]byte) ([][]byte, [][]byte) {
				payloads[1] = payloads[1][:len(payloads[1])-1]
				return rows, payloads
			},
			want: "record 1: payload of",
		},
		{
			name: "a payload longer than its payload_len",
			edit: func(rows, payloads [][]byte) ([][]byte, [][]byte) {
				payloads[1] = append(slices.Clone(payloads[1]), 0)
				return rows, payloads
			},
			want: "record 1: payload of",
		},
		{
			name: "a seq repeated",
			edit: func(rows, payloads [][]byte) ([][]byte, [][]byte) {
				copy(rows[3][:8], rows[2][:8])
				return rows, payloads
			},
			want: "record 3 seq 13 does not follow 13",
		},
		{
			name: "seqs out of order",
			edit: func(rows, payloads [][]byte) ([][]byte, [][]byte) {
				rows[2], rows[3] = rows[3], rows[2]
				payloads[2], payloads[3] = payloads[3], payloads[2]
				return rows, payloads
			},
			want: "record 3 seq 13 does not follow 14",
		},
		{
			name: "a header section over the limit",
			hl:   math.MaxUint16,
			edit: func(_, _ [][]byte) ([][]byte, [][]byte) { return bigRows, bigPayloads },
			want: "header section of 32769 records",
		},
		{
			name: "a decoded body over the limit",
			edit: func(rows, payloads [][]byte) ([][]byte, [][]byte) {
				binary.LittleEndian.PutUint32(rows[0][28:], format.MaxLen32-format.RecordHeaderLen+1)
				return rows[:1], [][]byte{nil}
			},
			want: "record 0: the decoded body exceeds",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rows, payloads := rawTestRecords(t, format.RecordHeaderLen)
			if tt.edit != nil {
				rows, payloads = tt.edit(rows, payloads)
			}
			hl := tt.hl
			if hl == 0 {
				hl = format.RecordHeaderLen
			}

			// The records are rejected before any block is encoded.
			encoded := func(enc []byte) []byte {
				t.Error("a block was encoded")
				return enc
			}
			raw, sum, err := encodeRawBlock(&encodeBuf{hook: encoded}, CodecZstd, hl, rows, payloads)
			require.ErrorContains(t, err, tt.want)
			assert.Nil(t, raw)
			assert.Zero(t, sum.recordCount)
		})
	}
}

// TestEncodeRawBlockCheckRejectsCorruption checks that the in-memory check of encodeRawBlock rejects an encoding
// that does not hold its records: a corruption I-2 catches,
// and corruptions that decode cleanly and pass I-2 but change a record's bytes, which only the byte comparison with the source catches.
func TestEncodeRawBlockCheckRejectsCorruption(t *testing.T) {
	t.Parallel()

	const hl = 52
	n := 7

	tests := []struct {
		name string
		// edit changes the decoded body, whose header section stores byte j of record i at j × n + i.
		edit func(body []byte) []byte
		want string
	}{
		{
			name: "two seqs swapped",
			edit: func(body []byte) []byte {
				for j := range 8 {
					body[j*n+1], body[j*n+2] = body[j*n+2], body[j*n+1]
				}
				return body
			},
			want: "record 2 seq 11 does not follow 13",
		},
		{
			name: "an extension byte",
			edit: func(body []byte) []byte {
				body[(format.RecordHeaderLen+3)*n+1] ^= 0x10
				return body
			},
			want: "record 1: the decoded header differs",
		},
		{
			name: "a reserved quality bit",
			edit: func(body []byte) []byte {
				body[37*n+4] ^= 1 << 6
				return body
			},
			want: "record 4: the decoded header differs",
		},
		{
			name: "a payload byte",
			edit: func(body []byte) []byte {
				body[len(body)-1] ^= 1
				return body
			},
			want: "record 6: the decoded payload differs",
		},
		{
			name: "a body one byte short",
			edit: func(body []byte) []byte { return body[:len(body)-1] },
			want: "decode body",
		},
	}

	for _, c := range []Codec{CodecNone, CodecZstd} {
		for _, tt := range tests {
			t.Run(c.String()+"/"+tt.name, func(t *testing.T) {
				t.Parallel()

				rows, payloads := rawTestRecords(t, hl)
				require.Len(t, rows, n)
				_, _, err := encodeRawBlock(&encodeBuf{}, c, hl, rows, payloads)
				require.NoError(t, err, "the records encode without the hook")

				uncompressedLen := n * hl
				for _, p := range payloads {
					uncompressedLen += len(p)
				}
				// The hook changes the decoded body and encodes it again, so a zstd body stays a valid frame.
				hook := func(enc []byte) []byte {
					body, err := codec.Decode(uint8(c), nil, enc, uncompressedLen)
					require.NoError(t, err)
					out, err := codec.Encode(uint8(c), nil, tt.edit(body))
					require.NoError(t, err)

					return out
				}
				raw, sum, err := encodeRawBlock(&encodeBuf{hook: hook}, c, hl, rows, payloads)
				require.ErrorContains(t, err, "fails its check")
				require.ErrorContains(t, err, tt.want)
				assert.Nil(t, raw)
				assert.Zero(t, sum.recordCount)
			})
		}
	}
}
