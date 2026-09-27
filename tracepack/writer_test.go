package tracepack_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/internal/codec"
	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// hourStart is 2026-09-21T14:00:00Z as nanoseconds since the Unix epoch: 1789999200 s, a whole UTC hour
// (1789999200 = 497222 × 3600).
const hourStart int64 = 1_789_999_200_000_000_000

// hourNs is one hour in nanoseconds.
const hourNs int64 = 3_600_000_000_000

// s1f3Frame is an HSMS S1F3 W data message with an empty message text, byte by byte per SEMI E37 §8.2:
// length 10 (00 00 00 0A), session_id 0x1234, byte 2 = W | stream 1 (0x81), function 3,
// ptype 0, stype 0, system bytes DE AD BE EF.
var s1f3Frame = []byte{
	0x00, 0x00, 0x00, 0x0A,
	0x12, 0x34, 0x81, 0x03, 0x00, 0x00, 0xDE, 0xAD, 0xBE, 0xEF,
}

// linktestReqFrame is an HSMS Linktest.req control message (stype 5) with session_id 0xFFFF,
// stream and function bytes 0, and system bytes 00 00 00 07.
var linktestReqFrame = []byte{
	0x00, 0x00, 0x00, 0x0A,
	0xFF, 0xFF, 0x00, 0x00, 0x00, 0x05, 0x00, 0x00, 0x00, 0x07,
}

const allValidity = tracepack.FieldValiditySessionID | tracepack.FieldValidityStreamAndW |
	tracepack.FieldValidityFunction | tracepack.FieldValidityPType |
	tracepack.FieldValiditySType | tracepack.FieldValiditySystemBytes

func TestRecordSetHeaderCopies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   tracepack.Record
		want tracepack.Record
	}{
		{
			name: "data frame copies every field",
			in:   tracepack.Record{Kind: tracepack.KindData, Payload: s1f3Frame},
			want: tracepack.Record{
				Kind: tracepack.KindData, Payload: s1f3Frame,
				SessionID: 0x1234, Stream: 1, W: true, Function: 3, PType: 0, SType: 0,
				SystemBytes: [4]byte{0xDE, 0xAD, 0xBE, 0xEF}, FieldValidity: allValidity,
			},
		},
		{
			name: "control frame copies byte 2 and 3 as raw bits",
			in:   tracepack.Record{Kind: tracepack.KindControl, Payload: linktestReqFrame},
			want: tracepack.Record{
				Kind: tracepack.KindControl, Payload: linktestReqFrame,
				SessionID: 0xFFFF, SType: 5,
				SystemBytes: [4]byte{0, 0, 0, 7}, FieldValidity: allValidity,
			},
		},
		{
			name: "short capture of 8 bytes keeps session_id, byte 2 and function only",
			in: tracepack.Record{
				Kind: tracepack.KindData, Payload: s1f3Frame[:8],
				// Stale values the helper must clear.
				PType: 9, SType: 9, SystemBytes: [4]byte{1, 2, 3, 4},
			},
			want: tracepack.Record{
				Kind: tracepack.KindData, Payload: s1f3Frame[:8],
				SessionID: 0x1234, Stream: 1, W: true, Function: 3,
				FieldValidity: tracepack.FieldValiditySessionID | tracepack.FieldValidityStreamAndW | tracepack.FieldValidityFunction,
			},
		},
		{
			name: "short capture of 13 bytes lacks only the system bytes",
			in:   tracepack.Record{Kind: tracepack.KindData, Payload: s1f3Frame[:13]},
			want: tracepack.Record{
				Kind: tracepack.KindData, Payload: s1f3Frame[:13],
				SessionID: 0x1234, Stream: 1, W: true, Function: 3,
				FieldValidity: allValidity &^ tracepack.FieldValiditySystemBytes,
			},
		},
		{
			name: "short capture of 5 bytes has no copy field",
			in:   tracepack.Record{Kind: tracepack.KindData, Payload: s1f3Frame[:5], SessionID: 7},
			want: tracepack.Record{Kind: tracepack.KindData, Payload: s1f3Frame[:5]},
		},
		{
			name: "transport event zeroes every copy",
			in: tracepack.Record{
				Kind: tracepack.KindTransportEvent, Payload: s1f3Frame,
				SessionID: 1, Stream: 2, W: true, Function: 3, PType: 4, SType: 5,
				SystemBytes: [4]byte{6, 7, 8, 9}, FieldValidity: allValidity,
			},
			want: tracepack.Record{Kind: tracepack.KindTransportEvent, Payload: s1f3Frame},
		},
		{
			name: "annotation zeroes every copy",
			in:   tracepack.Record{Kind: tracepack.KindAnnotation, Payload: s1f3Frame, SessionID: 1, FieldValidity: 1},
			want: tracepack.Record{Kind: tracepack.KindAnnotation, Payload: s1f3Frame},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := tt.in
			got.SetHeaderCopies()
			assert.Equal(t, tt.want, got)
		})
	}
}

// writerMeta returns a PackMeta valid for records classified by a classifier.
func writerMeta() *tracepack.PackMeta {
	m := basePackMeta()
	m.Classifier = new("go-secs/test")

	return m
}

// newTestWriter opens a Writer over a fresh buffer with writerMeta and the given options.
func newTestWriter(t *testing.T, opts tracepack.WriterOptions) (*tracepack.Writer, *bytes.Buffer) {
	t.Helper()

	if opts.Meta == nil {
		opts.Meta = writerMeta()
	}
	opts.Facts.AnyClassified = true

	var buf bytes.Buffer
	w, err := tracepack.NewWriter(&buf, opts)
	require.NoError(t, err)

	return w, &buf
}

// mustEventPayload marshals e and fails the test on error.
func mustEventPayload(t *testing.T, e *tracepack.TransportEvent) []byte {
	t.Helper()

	b, err := e.MarshalBinary()
	require.NoError(t, err)

	return b
}

// mixedRecords returns one record of each kind, in seq order from 0,
// with the copy fields set by SetHeaderCopies and the derived bits already correct,
// so a round trip must reproduce them exactly.
func mixedRecords(t *testing.T) []tracepack.Record {
	t.Helper()

	note, err := (&tracepack.Annotation{AnnotationKind: tracepack.AnnotationKindNote, Text: new("operator note")}).MarshalBinary()
	require.NoError(t, err)

	recs := []tracepack.Record{
		{
			Kind: tracepack.KindTransportEvent, Dir: tracepack.DirLocal, Fidelity: tracepack.FidelityNotApplicable,
			DecodeStatus: tracepack.DecodeStatusNotApplicable, Quality: tracepack.QualityCaptureBoundary,
			Payload: mustEventPayload(t, &tracepack.TransportEvent{
				Event: tracepack.EventCaptureBoundary, BoundaryKind: new(tracepack.BoundaryKindStart),
			}),
		},
		{
			Kind: tracepack.KindData, Dir: tracepack.DirHostToEquipment, Fidelity: tracepack.FidelityWireExact,
			DecodeStatus: tracepack.DecodeStatusOK, Epoch: 1, Payload: s1f3Frame,
		},
		{
			Kind: tracepack.KindControl, Dir: tracepack.DirEquipmentToHost, Fidelity: tracepack.FidelityWireExact,
			DecodeStatus: tracepack.DecodeStatusOK, Epoch: 1, Payload: linktestReqFrame,
		},
		{
			Kind: tracepack.KindData, Dir: tracepack.DirHostToEquipment, Fidelity: tracepack.FidelityWireExact,
			DecodeStatus: tracepack.DecodeStatusShortFrame, Epoch: 1, Payload: s1f3Frame[:9],
			Quality: tracepack.QualityDecodeFailed,
		},
		{
			Kind: tracepack.KindAnnotation, Dir: tracepack.DirLocal, Fidelity: tracepack.FidelityNotApplicable,
			DecodeStatus: tracepack.DecodeStatusNotApplicable, Payload: note,
			Quality: tracepack.QualityNoMono,
		},
	}

	for i := range recs {
		r := &recs[i]
		r.Seq = uint64(i)
		r.TSUTCNs = hourStart + int64(i)*1_000_000
		if !r.Quality.Has(tracepack.QualityNoMono) {
			r.MonoNs, r.MonoPresent = int64(i)*1_000_000+5, true
		}
		r.SetHeaderCopies()
	}

	return recs
}

// appendAll appends every record of recs to w, failing the test on error.
func appendAll(t *testing.T, w *tracepack.Writer, recs []tracepack.Record) {
	t.Helper()

	for i := range recs {
		r := recs[i]
		require.NoError(t, w.Append(&r), "record %d", i)
	}
}

func TestNewWriterWritesHeaderAndMetadata(t *testing.T) {
	t.Parallel()

	packID, captureID := uuidOfByte(0x11), uuidOfByte(0x22)
	meta := writerMeta()
	meta.SeqStart = 5
	w, buf := newTestWriter(t, tracepack.WriterOptions{Meta: meta, PackID: packID, CaptureID: captureID})

	p := mustWalkPack(t, buf.Bytes())
	assert.Equal(t, format.FormatMajor, p.Header.FormatMajor)
	assert.Equal(t, uint16(0), p.Header.FormatMinor)
	assert.Equal(t, uint32(0), p.Header.Flags)
	assert.Equal(t, format.UUID(packID), p.Header.PackID)
	assert.Equal(t, format.UUID(captureID), p.Header.CaptureID)
	assert.NotZero(t, p.Header.WriterStartUTCNs)
	assert.Equal(t, meta, p.Meta, "metadata round trip; blocks_validated absent without Validate")
	assert.Empty(t, p.Blocks)

	require.NoError(t, w.Close())
	assert.Equal(t, format.FileHeaderLen+len(p.MetaBytes), buf.Len(), "Close writes no footer or trailer yet")
}

func TestNewWriterGeneratesUUIDv7(t *testing.T) {
	t.Parallel()

	_, buf := newTestWriter(t, tracepack.WriterOptions{})
	p := mustWalkPack(t, buf.Bytes())

	for name, u := range map[string]format.UUID{"pack_id": p.Header.PackID, "capture_id": p.Header.CaptureID} {
		assert.False(t, u.IsZero(), name)
		assert.Equal(t, byte(0x70), u[6]&0xF0, "%s version nibble", name)
		assert.Equal(t, byte(0x80), u[8]&0xC0, "%s variant bits", name)
	}
	assert.NotEqual(t, p.Header.PackID, p.Header.CaptureID)
}

func TestWriterRoundTripMixedKinds(t *testing.T) {
	t.Parallel()

	for _, c := range []tracepack.Codec{tracepack.CodecNone, tracepack.CodecZstd} {
		t.Run(c.String(), func(t *testing.T) {
			t.Parallel()

			want := mixedRecords(t)
			w, buf := newTestWriter(t, tracepack.WriterOptions{Codec: c})
			appendAll(t, w, want)
			require.NoError(t, w.Close())

			p := mustWalkPack(t, buf.Bytes())
			require.Len(t, p.Blocks, 1)
			assert.Equal(t, uint8(c), p.Blocks[0].Env.Codec)
			assert.Equal(t, uint16(format.RecordHeaderLen), p.Blocks[0].Env.RecordHeaderLen)
			assert.Equal(t, uint32(len(want)), p.Blocks[0].Env.RecordCount)
			assert.Equal(t, uint64(0), p.Blocks[0].Env.FirstSeq)
			assert.Equal(t, want, p.records())
		})
	}
}

// dataRecord returns an S1F3 data record at ts, with mono present and its copy fields set.
func dataRecord(seq uint64, ts int64) tracepack.Record {
	r := tracepack.Record{
		Seq: seq, TSUTCNs: ts, MonoNs: ts - hourStart, MonoPresent: true, Epoch: 1,
		Kind: tracepack.KindData, Dir: tracepack.DirHostToEquipment, Fidelity: tracepack.FidelityWireExact,
		DecodeStatus: tracepack.DecodeStatusOK, Payload: s1f3Frame,
	}
	r.SetHeaderCopies()

	return r
}

// blockSeqs returns the seqs of each block of p.
func blockSeqs(p *walkedPack) [][]uint64 {
	out := make([][]uint64, 0, len(p.Blocks))
	for _, b := range p.Blocks {
		seqs := make([]uint64, 0, len(b.Records))
		for _, r := range b.Records {
			seqs = append(seqs, r.Seq)
		}
		out = append(out, seqs)
	}

	return out
}

func TestWriterClosesBlockAtUTCHourBoundary(t *testing.T) {
	t.Parallel()

	nextHour := hourStart + hourNs
	recs := []tracepack.Record{
		dataRecord(0, hourStart),
		dataRecord(1, nextHour-1),
		dataRecord(2, nextHour),    // first nanosecond of the next hour
		dataRecord(3, nextHour+5),  // same hour as seq 2
		dataRecord(4, nextHour-10), // a wall-clock step back into the earlier hour
		dataRecord(5, -1),          // 1969-12-31T23:59:59.999999999Z
		dataRecord(6, 0),           // 1970-01-01T00:00:00Z, the next hour
	}

	w, buf := newTestWriter(t, tracepack.WriterOptions{Codec: tracepack.CodecZstd})
	appendAll(t, w, recs)
	require.NoError(t, w.Close())

	p := mustWalkPack(t, buf.Bytes())
	assert.Equal(t, [][]uint64{{0, 1}, {2, 3}, {4}, {5}, {6}}, blockSeqs(p))
	assert.Equal(t, recs, p.records())
}

func TestWriterClosesBlockAtThreshold(t *testing.T) {
	t.Parallel()

	// Each record is 56 header bytes plus the 14-byte frame: 70 bytes uncompressed.
	tests := []struct {
		name      string
		threshold int
		want      [][]uint64
	}{
		{"closes before a record that would exceed it", 200, [][]uint64{{0, 1}, {2, 3}, {4, 5}, {6}}},
		{"closes when a record reaches it exactly", 210, [][]uint64{{0, 1, 2}, {3, 4, 5}, {6}}},
		{"threshold below one record puts each alone", 1, [][]uint64{{0}, {1}, {2}, {3}, {4}, {5}, {6}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			recs := make([]tracepack.Record, 7)
			for i := range recs {
				recs[i] = dataRecord(uint64(i), hourStart+int64(i))
			}

			w, buf := newTestWriter(t, tracepack.WriterOptions{BlockThreshold: tt.threshold})
			appendAll(t, w, recs)
			require.NoError(t, w.Close())

			p := mustWalkPack(t, buf.Bytes())
			assert.Equal(t, tt.want, blockSeqs(p))
			assert.Equal(t, recs, p.records())
			for _, b := range p.Blocks {
				assert.Equal(t, uint32(70*len(b.Records)), b.Env.UncompressedLen)
			}
		})
	}
}

func TestWriterWritesOversizedRecordAlone(t *testing.T) {
	t.Parallel()

	big := dataRecord(2, hourStart+2)
	big.Payload = append(bytes.Clone(s1f3Frame), make([]byte, 300)...)

	recs := []tracepack.Record{
		dataRecord(0, hourStart), dataRecord(1, hourStart+1), big, dataRecord(3, hourStart+3),
	}

	w, buf := newTestWriter(t, tracepack.WriterOptions{BlockThreshold: 200, Codec: tracepack.CodecZstd})
	appendAll(t, w, recs)
	require.NoError(t, w.Close())

	p := mustWalkPack(t, buf.Bytes())
	assert.Equal(t, [][]uint64{{0, 1}, {2}, {3}}, blockSeqs(p))
	assert.Equal(t, uint32(56+14+300), p.Blocks[1].Env.UncompressedLen)
	assert.Equal(t, recs, p.records())
}

func TestWriterFlushClosesOpenBlock(t *testing.T) {
	t.Parallel()

	w, buf := newTestWriter(t, tracepack.WriterOptions{})
	require.NoError(t, w.Flush(), "flush without records writes nothing")
	headLen := buf.Len()

	r0, r1 := dataRecord(0, hourStart), dataRecord(1, hourStart+1)
	require.NoError(t, w.Append(&r0))
	assert.Equal(t, headLen, buf.Len(), "an open block is not written before it closes")
	require.NoError(t, w.Flush())
	require.NoError(t, w.Flush(), "a second flush has no block to close")
	require.NoError(t, w.Append(&r1))
	require.NoError(t, w.Close())

	p := mustWalkPack(t, buf.Bytes())
	assert.Equal(t, [][]uint64{{0}, {1}}, blockSeqs(p))
}

func TestWriterDerivesOwnedBits(t *testing.T) {
	t.Parallel()

	okWithWrongBits := dataRecord(0, hourStart)
	okWithWrongBits.Quality = tracepack.QualityNoMono | tracepack.QualityDecodeFailed | tracepack.QualityDirectionInferred

	malformedWithoutBits := dataRecord(1, hourStart+1)
	malformedWithoutBits.DecodeStatus = tracepack.DecodeStatusItemDecodeError
	malformedWithoutBits.MonoPresent = false
	malformedWithoutBits.MonoNs = 12345 // stale: must not reach the file

	notAttempted := dataRecord(2, hourStart+2)
	notAttempted.DecodeStatus = tracepack.DecodeStatusNotAttempted
	notAttempted.Quality = tracepack.QualityDecodeFailed

	in := []tracepack.Record{okWithWrongBits, malformedWithoutBits, notAttempted}

	want := []tracepack.Record{okWithWrongBits, malformedWithoutBits, notAttempted}
	want[0].Quality = tracepack.QualityDirectionInferred
	want[1].Quality = tracepack.QualityDecodeFailed | tracepack.QualityNoMono
	want[1].MonoNs = 0
	want[2].Quality = 0

	w, buf := newTestWriter(t, tracepack.WriterOptions{})
	appendAll(t, w, in)
	require.NoError(t, w.Close())

	p := mustWalkPack(t, buf.Bytes())
	assert.Equal(t, want, p.records())

	flags := recordFlagsOf(t, p)
	assert.Equal(t, tracepack.RecordFlagsW|tracepack.RecordFlagsMonoPresent, flags[0])
	assert.Equal(t, tracepack.RecordFlagsW, flags[1], "mono_present clear iff no-mono")
}

func TestWriterZeroesUnavailableCopies(t *testing.T) {
	t.Parallel()

	// A 7-byte short capture: the length prefix plus session_id and byte 2 of the header.
	short := tracepack.Record{
		Seq: 0, TSUTCNs: hourStart, Kind: tracepack.KindData, Dir: tracepack.DirHostToEquipment,
		DecodeStatus: tracepack.DecodeStatusShortFrame, Payload: s1f3Frame[:7],
	}
	short.SetHeaderCopies()
	// Stale values for the fields the capture lacks, which SetHeaderCopies would have cleared.
	stale := short
	stale.Function, stale.PType, stale.SType, stale.SystemBytes = 3, 1, 2, [4]byte{9, 9, 9, 9}

	event := tracepack.Record{
		Seq: 1, TSUTCNs: hourStart + 1, Kind: tracepack.KindTransportEvent,
		DecodeStatus: tracepack.DecodeStatusNotApplicable,
		Payload:      mustEventPayload(t, &tracepack.TransportEvent{Event: tracepack.EventClockStep, ClockStepNs: new(int64(5))}),
		SessionID:    1, Stream: 2, W: true, Function: 3, PType: 4, SType: 5, SystemBytes: [4]byte{6, 7, 8, 9},
		FieldValidity: allValidity,
	}

	w, buf := newTestWriter(t, tracepack.WriterOptions{Validate: true})
	appendAll(t, w, []tracepack.Record{stale, event})
	require.NoError(t, w.Close())

	p := mustWalkPack(t, buf.Bytes())
	got := p.records()
	require.Len(t, got, 2)

	wantShort := short
	wantShort.Quality = tracepack.QualityDecodeFailed | tracepack.QualityNoMono
	assert.Equal(t, wantShort, got[0])
	assert.Equal(t, tracepack.FieldValiditySessionID|tracepack.FieldValidityStreamAndW, got[0].FieldValidity)
	assert.Equal(t, uint16(0x1234), got[0].SessionID)

	wantEvent := tracepack.Record{
		Seq: 1, TSUTCNs: hourStart + 1, Kind: tracepack.KindTransportEvent,
		DecodeStatus: tracepack.DecodeStatusNotApplicable, Payload: event.Payload, Quality: tracepack.QualityNoMono,
	}
	assert.Equal(t, wantEvent, got[1])
}

func TestWriterCodecsDecodeToEqualBodies(t *testing.T) {
	t.Parallel()

	bodies := make(map[tracepack.Codec][][]byte)
	for _, c := range []tracepack.Codec{tracepack.CodecNone, tracepack.CodecZstd} {
		w, buf := newTestWriter(t, tracepack.WriterOptions{Codec: c, BlockThreshold: 250, PackID: uuidOfByte(1), CaptureID: uuidOfByte(2)})
		recs := mixedRecords(t)
		appendAll(t, w, recs)
		require.NoError(t, w.Close())

		p := mustWalkPack(t, buf.Bytes())
		for _, b := range p.Blocks {
			decoded, err := codec.Decode(b.Env.Codec, nil, b.Body, int(b.Env.UncompressedLen))
			require.NoError(t, err)
			bodies[c] = append(bodies[c], decoded)
		}
		assert.Equal(t, recs, p.records(), c.String())
	}

	require.NotEmpty(t, bodies[tracepack.CodecNone])
	assert.Equal(t, bodies[tracepack.CodecNone], bodies[tracepack.CodecZstd])
}

// recordFlagsOf returns the raw record_flags byte of every record of p, read from the decoded header sections.
func recordFlagsOf(t *testing.T, p *walkedPack) []tracepack.RecordFlags {
	t.Helper()

	var out []tracepack.RecordFlags
	for _, b := range p.Blocks {
		decoded, err := codec.Decode(b.Env.Codec, nil, b.Body, int(b.Env.UncompressedLen))
		require.NoError(t, err)

		for i := range int(b.Env.RecordCount) {
			h, err := format.UnmarshalRecordHeader(decoded[i*int(b.Env.RecordHeaderLen):], int(b.Env.RecordHeaderLen))
			require.NoError(t, err)
			out = append(out, tracepack.RecordFlags(h.RecordFlags))
		}
	}

	return out
}

func TestWriterCallerSeqsAllowGapsAndRejectDisorder(t *testing.T) {
	t.Parallel()

	meta := writerMeta()
	meta.SeqStart = 10
	w, buf := newTestWriter(t, tracepack.WriterOptions{Meta: meta})

	for _, seq := range []uint64{10, 12, 20} {
		r := dataRecord(seq, hourStart+int64(seq))
		require.NoError(t, w.Append(&r), "seq %d", seq)
	}

	for _, seq := range []uint64{20, 15, 0} {
		r := dataRecord(seq, hourStart+30)
		require.ErrorIs(t, w.Append(&r), tracepack.ErrSeqOrder, "seq %d", seq)
	}

	r := dataRecord(21, hourStart+21)
	require.NoError(t, w.Append(&r), "a rejected record leaves the writer usable")
	require.NoError(t, w.Close())

	p := mustWalkPack(t, buf.Bytes())
	assert.Equal(t, [][]uint64{{10, 12, 20, 21}}, blockSeqs(p))
	assert.Equal(t, uint64(10), p.Meta.SeqStart)
}

func TestWriterFirstSeqMustEqualSeqStart(t *testing.T) {
	t.Parallel()

	for _, seq := range []uint64{4, 6} {
		meta := writerMeta()
		meta.SeqStart = 5
		w, _ := newTestWriter(t, tracepack.WriterOptions{Meta: meta})

		r := dataRecord(seq, hourStart)
		require.ErrorIs(t, w.Append(&r), tracepack.ErrSeqOrder, "first seq %d with seq_start 5", seq)
	}
}

func TestWriterRejectsSeqAboveLimit(t *testing.T) {
	t.Parallel()

	const limit = uint64(1)<<63 - 1

	meta := writerMeta()
	meta.SeqStart = limit
	w, _ := newTestWriter(t, tracepack.WriterOptions{Meta: meta})
	r := dataRecord(limit, hourStart)
	require.NoError(t, w.Append(&r), "2^63-1 is the largest valid seq")
	r = dataRecord(limit+1, hourStart)
	require.ErrorIs(t, w.Append(&r), tracepack.ErrSeqOrder)

	meta = writerMeta()
	meta.SeqStart = limit + 1
	_, err := tracepack.NewWriter(&bytes.Buffer{}, tracepack.WriterOptions{Meta: meta})
	require.ErrorIs(t, err, tracepack.ErrSeqOrder)

	_, err = tracepack.NewWriter(&bytes.Buffer{}, tracepack.WriterOptions{Meta: writerMeta(), AssignSeq: true, NextSeq: limit + 1})
	require.ErrorIs(t, err, tracepack.ErrSeqOrder)
}

func TestWriterAssignsSeqFromNextSeq(t *testing.T) {
	t.Parallel()

	meta := writerMeta()
	meta.SeqStart = 3 // ignored: AssignSeq makes seq_start NextSeq
	w, buf := newTestWriter(t, tracepack.WriterOptions{Meta: meta, AssignSeq: true, NextSeq: 100})

	assigned := make([]uint64, 0, 3)
	for i := range 3 {
		r := dataRecord(7, hourStart+int64(i)) // the caller's Seq is ignored
		require.NoError(t, w.Append(&r))
		assigned = append(assigned, r.Seq)
	}
	require.NoError(t, w.Close())

	assert.Equal(t, []uint64{100, 101, 102}, assigned, "Append stores the assigned seq in the record")

	p := mustWalkPack(t, buf.Bytes())
	assert.Equal(t, [][]uint64{{100, 101, 102}}, blockSeqs(p))
	assert.Equal(t, uint64(100), p.Meta.SeqStart)
	assert.Equal(t, uint64(3), meta.SeqStart, "NewWriter does not modify the caller's PackMeta")
}

// countingSyncer counts Sync calls and fails once failAt calls have succeeded, when failAt is positive.
type countingSyncer struct {
	calls  int
	failAt int
}

var errSync = errors.New("sync failed")

func (s *countingSyncer) Sync() error {
	s.calls++
	if s.failAt > 0 && s.calls > s.failAt {
		return errSync
	}

	return nil
}

func TestWriterSyncsEveryBlock(t *testing.T) {
	t.Parallel()

	s := &countingSyncer{}
	w, buf := newTestWriter(t, tracepack.WriterOptions{Sync: s, BlockThreshold: 140})
	assert.Equal(t, 0, s.calls, "the header and pack metadata are not synced on their own")

	for i := range 5 {
		r := dataRecord(uint64(i), hourStart+int64(i))
		require.NoError(t, w.Append(&r))
	}
	require.NoError(t, w.Close())

	p := mustWalkPack(t, buf.Bytes())
	require.Len(t, p.Blocks, 3)
	assert.Equal(t, len(p.Blocks), s.calls)
}

func TestWriterSyncFailureFailsWriter(t *testing.T) {
	t.Parallel()

	s := &countingSyncer{failAt: 1}
	w, _ := newTestWriter(t, tracepack.WriterOptions{Sync: s})

	r := dataRecord(0, hourStart)
	require.NoError(t, w.Append(&r))
	require.NoError(t, w.Flush())

	r = dataRecord(1, hourStart+1)
	require.NoError(t, w.Append(&r))
	require.ErrorIs(t, w.Flush(), errSync)

	r = dataRecord(2, hourStart+2)
	err := w.Append(&r)
	require.ErrorIs(t, err, tracepack.ErrWriterFailed)
	require.ErrorIs(t, err, errSync, "the failure names its cause")
	require.ErrorIs(t, w.Close(), tracepack.ErrWriterFailed)
}

// failingWriter accepts limit bytes, then fails every write.
type failingWriter struct {
	buf   bytes.Buffer
	limit int
}

var errWrite = errors.New("write failed")

func (f *failingWriter) Write(p []byte) (int, error) {
	room := f.limit - f.buf.Len()
	if len(p) <= room {
		return f.buf.Write(p)
	}

	n, _ := f.buf.Write(p[:max(room, 0)])

	return n, errWrite
}

func TestWriterWriteFailureFailsWriter(t *testing.T) {
	t.Parallel()

	var probe bytes.Buffer
	_, err := tracepack.NewWriter(&probe, tracepack.WriterOptions{Meta: writerMeta(), Facts: tracepack.PackFacts{AnyClassified: true}})
	require.NoError(t, err)

	_, err = tracepack.NewWriter(&failingWriter{limit: 10}, tracepack.WriterOptions{Meta: writerMeta()})
	require.ErrorIs(t, err, errWrite, "a failed header write fails NewWriter")

	fw := &failingWriter{limit: probe.Len() + 20} // the envelope write fails part way
	w, err := tracepack.NewWriter(fw, tracepack.WriterOptions{Meta: writerMeta()})
	require.NoError(t, err)

	r := dataRecord(0, hourStart)
	require.NoError(t, w.Append(&r))
	require.ErrorIs(t, w.Flush(), errWrite)
	require.ErrorIs(t, w.Flush(), tracepack.ErrWriterFailed)
}

func TestWriterRejectsCallsAfterClose(t *testing.T) {
	t.Parallel()

	w, buf := newTestWriter(t, tracepack.WriterOptions{})
	r := dataRecord(0, hourStart)
	require.NoError(t, w.Append(&r))
	require.NoError(t, w.Close())
	n := buf.Len()

	r = dataRecord(1, hourStart+1)
	require.ErrorIs(t, w.Append(&r), tracepack.ErrClosed)
	require.ErrorIs(t, w.Flush(), tracepack.ErrClosed)
	require.ErrorIs(t, w.Close(), tracepack.ErrClosed)
	assert.Equal(t, n, buf.Len())
}

func TestNewWriterRejectsInvalidOptions(t *testing.T) {
	t.Parallel()

	extract := writerMeta()
	extract.PackRole = tracepack.PackRoleExtract // requires extract_filter

	tests := []struct {
		name string
		opts tracepack.WriterOptions
	}{
		{"nil meta", tracepack.WriterOptions{}},
		{"unknown codec", tracepack.WriterOptions{Meta: writerMeta(), Codec: 2}},
		{"negative threshold", tracepack.WriterOptions{Meta: writerMeta(), BlockThreshold: -1}},
		{"threshold above the body limit", tracepack.WriterOptions{Meta: writerMeta(), BlockThreshold: 1 << 31}},
		{"metadata failing its rules", tracepack.WriterOptions{Meta: extract}},
		{"classified records without a classifier", tracepack.WriterOptions{Meta: basePackMeta(), Facts: tracepack.PackFacts{AnyClassified: true}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			w, err := tracepack.NewWriter(&buf, tt.opts)
			require.Error(t, err)
			assert.Nil(t, w)
			assert.Zero(t, buf.Len(), "nothing is written for invalid options")
		})
	}
}

func TestNewWriterSetsRedactionPresentFromFacts(t *testing.T) {
	t.Parallel()

	meta := writerMeta()
	meta.Redaction = []tracepack.RedactionEntry{{
		Seq: 0, Domain: "d", Digest: make([]byte, 32), MaskedRanges: []tracepack.MaskedRange{{Offset: 14, Length: 2}},
	}}
	_, buf := newTestWriter(t, tracepack.WriterOptions{Meta: meta, Facts: tracepack.PackFacts{AnyRedacted: true}})

	p := mustWalkPack(t, buf.Bytes())
	assert.Equal(t, uint32(1), p.Header.Flags)
}

func TestValidatingWriterAttestsAgreeingPack(t *testing.T) {
	t.Parallel()

	meta := writerMeta()
	w, buf := newTestWriter(t, tracepack.WriterOptions{Meta: meta, Validate: true, Codec: tracepack.CodecZstd, BlockThreshold: 200})
	recs := mixedRecords(t)
	appendAll(t, w, recs)
	require.NoError(t, w.Close())

	p := mustWalkPack(t, buf.Bytes())
	require.NotNil(t, p.Meta.BlocksValidated)
	assert.True(t, *p.Meta.BlocksValidated)
	assert.Greater(t, len(p.Blocks), 1)
	assert.Equal(t, recs, p.records())
	assert.Nil(t, meta.BlocksValidated, "NewWriter does not modify the caller's PackMeta")
}

func TestNonValidatingWriterNeverAttests(t *testing.T) {
	t.Parallel()

	meta := writerMeta()
	meta.BlocksValidated = new(true)
	_, buf := newTestWriter(t, tracepack.WriterOptions{Meta: meta})

	p := mustWalkPack(t, buf.Bytes())
	assert.Nil(t, p.Meta.BlocksValidated, "blocks_validated is absent unless the writer validates")
}

// s6f11CopiesOverS1F3 returns a record whose header copies claim S6F11 while its payload is the S1F3 frame.
func s6f11CopiesOverS1F3(seq uint64) tracepack.Record {
	r := dataRecord(seq, hourStart+int64(seq))
	r.Stream, r.Function = 6, 11

	return r
}

func TestValidatingWriterRejectsCopyDisagreement(t *testing.T) {
	t.Parallel()

	w, buf := newTestWriter(t, tracepack.WriterOptions{Validate: true, Codec: tracepack.CodecZstd})

	good := dataRecord(0, hourStart)
	require.NoError(t, w.Append(&good))
	require.NoError(t, w.Flush())
	written := buf.Len()

	bad := s6f11CopiesOverS1F3(1)
	require.NoError(t, w.Append(&bad), "the disagreement is found when the block is checked, before it is written")
	require.ErrorIs(t, w.Flush(), tracepack.ErrValidation)
	assert.Equal(t, written, buf.Len(), "nothing of the failed block is written")

	next := dataRecord(2, hourStart+2)
	err := w.Append(&next)
	require.ErrorIs(t, err, tracepack.ErrWriterFailed)
	require.ErrorIs(t, err, tracepack.ErrValidation)
	require.ErrorIs(t, w.Flush(), tracepack.ErrWriterFailed)
	require.ErrorIs(t, w.Close(), tracepack.ErrWriterFailed)
	assert.Equal(t, written, buf.Len())

	p := mustWalkPack(t, buf.Bytes())
	assert.Equal(t, [][]uint64{{0}}, blockSeqs(p))
}

func TestValidatingWriterRejectsValidityBeyondPayload(t *testing.T) {
	t.Parallel()

	w, _ := newTestWriter(t, tracepack.WriterOptions{Validate: true})

	r := dataRecord(0, hourStart)
	r.Payload = s1f3Frame[:9] // ptype is the last captured byte
	r.DecodeStatus = tracepack.DecodeStatusShortFrame
	require.NoError(t, w.Append(&r))
	require.ErrorIs(t, w.Close(), tracepack.ErrValidation, "field_validity claims stype and system_bytes the payload lacks")
}

func TestNonValidatingWriterStoresCopyDisagreement(t *testing.T) {
	t.Parallel()

	w, buf := newTestWriter(t, tracepack.WriterOptions{})
	bad := s6f11CopiesOverS1F3(0)
	require.NoError(t, w.Append(&bad))
	require.NoError(t, w.Close())

	p := mustWalkPack(t, buf.Bytes())
	assert.Equal(t, []tracepack.Record{bad}, p.records(), "the writer stores what it receives")
}
