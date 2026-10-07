package tracepack_test

import (
	"bytes"
	"errors"
	"io"
	"math"
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

func TestRecordSetCapturedFieldValidity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		kind tracepack.Kind
		n    int
		want tracepack.FieldValidity
	}{
		{name: "whole data frame", kind: tracepack.KindData, n: len(s1f3Frame), want: allValidity},
		{name: "whole control frame", kind: tracepack.KindControl, n: len(linktestReqFrame), want: allValidity},
		{name: "4 bytes, the length prefix only", kind: tracepack.KindData, n: 4},
		{name: "5 bytes, half of SessionID", kind: tracepack.KindData, n: 5},
		{name: "6 bytes", kind: tracepack.KindData, n: 6, want: tracepack.FieldValiditySessionID},
		{name: "7 bytes", kind: tracepack.KindData, n: 7, want: tracepack.FieldValiditySessionID | tracepack.FieldValidityStreamAndW},
		{name: "8 bytes", kind: tracepack.KindData, n: 8,
			want: tracepack.FieldValiditySessionID | tracepack.FieldValidityStreamAndW | tracepack.FieldValidityFunction},
		{name: "9 bytes", kind: tracepack.KindData, n: 9,
			want: allValidity &^ (tracepack.FieldValiditySType | tracepack.FieldValiditySystemBytes)},
		{name: "10 bytes", kind: tracepack.KindData, n: 10, want: allValidity &^ tracepack.FieldValiditySystemBytes},
		{name: "13 bytes lack a System Bytes byte", kind: tracepack.KindData, n: 13, want: allValidity &^ tracepack.FieldValiditySystemBytes},
		{name: "transport event", kind: tracepack.KindTransportEvent, n: len(s1f3Frame)},
		{name: "annotation", kind: tracepack.KindAnnotation, n: len(s1f3Frame)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			frame := s1f3Frame
			if tt.kind == tracepack.KindControl {
				frame = linktestReqFrame
			}
			r := tracepack.Record{Kind: tt.kind, Payload: frame[:tt.n], FieldValidity: 0xFF}
			r.SetCapturedFieldValidity()
			assert.Equal(t, tt.want, r.FieldValidity)
		})
	}
}

func TestRecordHSMSHeader(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		r    tracepack.Record
		want tracepack.HSMSHeader
	}{
		{
			name: "data frame",
			r:    tracepack.Record{Kind: tracepack.KindData, Payload: s1f3Frame, FieldValidity: allValidity},
			want: tracepack.HSMSHeader{
				SessionID: 0x1234, Stream: 1, W: true, Function: 3,
				SystemBytes: [4]byte{0xDE, 0xAD, 0xBE, 0xEF}, Available: allValidity,
			},
		},
		{
			// A control frame's bytes 6 and 7 are status or reason codes, read like any other.
			name: "control frame",
			r:    tracepack.Record{Kind: tracepack.KindControl, Payload: linktestReqFrame, FieldValidity: allValidity},
			want: tracepack.HSMSHeader{SessionID: 0xFFFF, SType: 5, SystemBytes: [4]byte{0, 0, 0, 7}, Available: allValidity},
		},
		{
			// A log conversion without source System Bytes: the placeholder bytes are visible, never available.
			name: "clear bit over present bytes",
			r:    tracepack.Record{Kind: tracepack.KindData, Payload: s1f3Frame, FieldValidity: allValidity &^ tracepack.FieldValiditySystemBytes},
			want: tracepack.HSMSHeader{
				SessionID: 0x1234, Stream: 1, W: true, Function: 3,
				SystemBytes: [4]byte{0xDE, 0xAD, 0xBE, 0xEF}, Available: allValidity &^ tracepack.FieldValiditySystemBytes,
			},
		},
		{
			// A writer defect: the bits claim fields the payload lacks, which stay unavailable.
			name: "set bits beyond the payload",
			r:    tracepack.Record{Kind: tracepack.KindData, Payload: s1f3Frame[:8], FieldValidity: allValidity},
			want: tracepack.HSMSHeader{
				SessionID: 0x1234, Stream: 1, W: true, Function: 3,
				Available: tracepack.FieldValiditySessionID | tracepack.FieldValidityStreamAndW | tracepack.FieldValidityFunction,
			},
		},
		{
			// Control status and reason codes in bytes 6 and 7 read as stream, W and function (SEMI E37 §8.3).
			name: "control frame with status bytes",
			r: tracepack.Record{
				Kind: tracepack.KindControl, FieldValidity: allValidity,
				Payload: []byte{0, 0, 0, 0x0A, 0xFF, 0xFF, 0x85, 0x03, 0x00, 0x02, 0, 0, 0, 9},
			},
			want: tracepack.HSMSHeader{
				SessionID: 0xFFFF, Stream: 5, W: true, Function: 3, SType: 2,
				SystemBytes: [4]byte{0, 0, 0, 9}, Available: allValidity,
			},
		},
		{
			name: "reserved bits are never available",
			r:    tracepack.Record{Kind: tracepack.KindData, Payload: s1f3Frame, FieldValidity: 0xC0},
			want: tracepack.HSMSHeader{SessionID: 0x1234, Stream: 1, W: true, Function: 3, SystemBytes: [4]byte{0xDE, 0xAD, 0xBE, 0xEF}},
		},
		{
			name: "transport event",
			r:    tracepack.Record{Kind: tracepack.KindTransportEvent, Payload: s1f3Frame, FieldValidity: allValidity},
		},
		{
			name: "no payload",
			r:    tracepack.Record{Kind: tracepack.KindData, FieldValidity: allValidity},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, tt.r.HSMSHeader())
		})
	}
}

// hourMeta returns basePackMeta for a segment whose period is the hour starting at hourStart.
func hourMeta() *tracepack.PackMeta {
	m := basePackMeta()
	m.PeriodStart, m.PeriodEnd = hourStart, hourStart+hourNs

	return m
}

// writerMeta returns hourMeta valid for records classified by a classifier.
func writerMeta() *tracepack.PackMeta {
	m := hourMeta()
	m.Classifiers = []string{"go-secs/test"}

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

// TestRecordHSMSHeaderThresholds reads a frame cut at every length from 4 to 14 bytes, every bit set:
// each field carries its value exactly when the payload holds all of its bytes, and is available exactly then.
func TestRecordHSMSHeaderThresholds(t *testing.T) {
	t.Parallel()

	whole := tracepack.Record{Kind: tracepack.KindData, Payload: s1f3Frame, FieldValidity: allValidity}
	full := whole.HSMSHeader()
	for n := 4; n <= len(s1f3Frame); n++ {
		r := tracepack.Record{Kind: tracepack.KindData, Payload: s1f3Frame[:n], FieldValidity: allValidity}
		h := r.HSMSHeader()

		var want tracepack.HSMSHeader
		if n >= 6 {
			want.SessionID = full.SessionID
			want.Available |= tracepack.FieldValiditySessionID
		}
		if n >= 7 {
			want.Stream, want.W = full.Stream, full.W
			want.Available |= tracepack.FieldValidityStreamAndW
		}
		if n >= 8 {
			want.Function = full.Function
			want.Available |= tracepack.FieldValidityFunction
		}
		if n >= 9 {
			want.PType = full.PType
			want.Available |= tracepack.FieldValidityPType
		}
		if n >= 10 {
			want.SType = full.SType
			want.Available |= tracepack.FieldValiditySType
		}
		if n >= 14 {
			want.SystemBytes = full.SystemBytes
			want.Available |= tracepack.FieldValiditySystemBytes
		}
		assert.Equal(t, want, h, "%d bytes", n)

		r.SetCapturedFieldValidity()
		assert.Equal(t, want.Available, r.FieldValidity, "%d bytes: SetCapturedFieldValidity agrees with the availability", n)
	}
}

// mustEventPayload marshals e and fails the test on error.
func mustEventPayload(t *testing.T, e *tracepack.TransportEvent) []byte {
	t.Helper()

	b, err := e.MarshalBinary()
	require.NoError(t, err)

	return b
}

// mixedRecords returns one record of each kind, in seq order from 0,
// with the field_validity of a raw capture and the derived bits already correct,
// so a round trip must reproduce them exactly.
func mixedRecords(t *testing.T) []tracepack.Record {
	t.Helper()

	note, err := (&tracepack.Annotation{AnnotationKind: tracepack.AnnotationKindNote, Text: new("operator note")}).MarshalBinary()
	require.NoError(t, err)

	recs := []tracepack.Record{
		{
			Kind: tracepack.KindTransportEvent, Dir: tracepack.DirLocal, Fidelity: tracepack.FidelityNotApplicable,
			DecodeStatus: tracepack.DecodeStatusNotApplicable,
			Quality:      tracepack.QualityCaptureBoundary | tracepack.QualityCorrelationIncomplete,
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
		},
		{
			Kind: tracepack.KindAnnotation, Dir: tracepack.DirLocal, Fidelity: tracepack.FidelityNotApplicable,
			DecodeStatus: tracepack.DecodeStatusNotApplicable, Payload: note,
			Quality: tracepack.QualityCorrelationIncomplete,
		},
	}

	// The annotation has no monotonic time.
	const noMono = 4
	for i := range recs {
		r := &recs[i]
		r.Seq = uint64(i)
		r.TSUTCNs = hourStart + int64(i)*1_000_000
		if i != noMono {
			r.MonoNs, r.MonoPresent = int64(i)*1_000_000+5, true
		}
		r.SetCapturedFieldValidity()
	}

	return recs
}

// closeErr closes w and returns only the error.
func closeErr(w *tracepack.Writer) error {
	_, err := w.Close()

	return err
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
	assert.Equal(t, meta, p.Meta, "metadata round trip; a validating Writer adds no blocks_validated")
	assert.Empty(t, p.Blocks)

	mustClose(t, w)
	p = mustWalkPack(t, buf.Bytes())
	require.NotNil(t, p.Footer, "Close finalizes the pack")
	assert.Equal(t, uint64(format.FileHeaderLen+len(p.MetaBytes)), p.Footer.Trailer.FooterOffset, "no block before the footer")
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
			mustClose(t, w)

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

// dataRecord returns an S1F3 data record at ts, with mono present and the field_validity of the whole frame.
func dataRecord(seq uint64, ts int64) tracepack.Record {
	r := tracepack.Record{
		Seq: seq, TSUTCNs: ts, MonoNs: ts - hourStart, MonoPresent: true, Epoch: 1,
		Kind: tracepack.KindData, Dir: tracepack.DirHostToEquipment, Fidelity: tracepack.FidelityWireExact,
		DecodeStatus: tracepack.DecodeStatusOK, Payload: s1f3Frame,
	}
	r.SetCapturedFieldValidity()

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

	// Only an extract holds records of several UTC hours.
	w, buf := newTestWriter(t, tracepack.WriterOptions{Meta: asExtract(writerMeta()), Codec: tracepack.CodecZstd})
	appendAll(t, w, recs)
	mustClose(t, w)

	p := mustWalkPack(t, buf.Bytes())
	assert.Equal(t, [][]uint64{{0, 1}, {2, 3}, {4}, {5}, {6}}, blockSeqs(p))
	assert.Equal(t, recs, p.records())
}

func TestWriterClosesBlockAtThreshold(t *testing.T) {
	t.Parallel()

	// Each record is 44 header bytes plus the 14-byte frame: 58 bytes uncompressed.
	tests := []struct {
		name      string
		threshold int
		want      [][]uint64
	}{
		{"closes before a record that would exceed it", 150, [][]uint64{{0, 1}, {2, 3}, {4, 5}, {6}}},
		{"closes when a record reaches it exactly", 174, [][]uint64{{0, 1, 2}, {3, 4, 5}, {6}}},
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
			mustClose(t, w)

			p := mustWalkPack(t, buf.Bytes())
			assert.Equal(t, tt.want, blockSeqs(p))
			assert.Equal(t, recs, p.records())
			for _, b := range p.Blocks {
				assert.Equal(t, uint32(58*len(b.Records)), b.Env.UncompressedLen)
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
	mustClose(t, w)

	p := mustWalkPack(t, buf.Bytes())
	assert.Equal(t, [][]uint64{{0, 1}, {2}, {3}}, blockSeqs(p))
	assert.Equal(t, uint32(44+14+300), p.Blocks[1].Env.UncompressedLen)
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
	mustClose(t, w)

	p := mustWalkPack(t, buf.Bytes())
	assert.Equal(t, [][]uint64{{0}, {1}}, blockSeqs(p))
}

func TestWriterDerivesOwnedBits(t *testing.T) {
	t.Parallel()

	// Quality bits 3 and 6 are retired (the tracepack format specification §9): a caller's bits are cleared,
	// and nothing derives them from decode_status or mono_present any longer.
	retiredBits := dataRecord(0, hourStart)
	retiredBits.Quality = 1<<3 | 1<<6 | tracepack.QualityDirectionInferred

	malformedWithoutMono := dataRecord(1, hourStart+1)
	malformedWithoutMono.DecodeStatus = tracepack.DecodeStatusItemDecodeError
	malformedWithoutMono.MonoPresent = false
	malformedWithoutMono.MonoNs = 12345 // stale: must not reach the file

	in := []tracepack.Record{retiredBits, malformedWithoutMono}

	want := []tracepack.Record{retiredBits, malformedWithoutMono}
	want[0].Quality = tracepack.QualityDirectionInferred
	want[1].MonoNs = 0

	w, buf := newTestWriter(t, tracepack.WriterOptions{})
	appendAll(t, w, in)
	mustClose(t, w)

	p := mustWalkPack(t, buf.Bytes())
	assert.Equal(t, want, p.records())

	flags := recordFlagsOf(t, p)
	assert.Equal(t, tracepack.RecordFlagsMonoPresent, flags[0])
	assert.Equal(t, tracepack.RecordFlags(0), flags[1], "mono_present clear without mono; the retired W bit is never written")
}

func TestWriterDerivesEpochAndBoundaryBits(t *testing.T) {
	t.Parallel()

	unknownEpoch := dataRecordIn(0, hourStart, 0)

	keptIncomplete := dataRecordIn(1, hourStart+1, 1)
	keptIncomplete.Quality = tracepack.QualityCorrelationIncomplete

	dataClaimingBoundary := dataRecordIn(2, hourStart+2, 1)
	dataClaimingBoundary.Quality = tracepack.QualityCaptureBoundary

	boundaryWithoutBit := eventRecord(t, 3, hourStart+3, 1, &tracepack.TransportEvent{
		Event: tracepack.EventCaptureBoundary, BoundaryKind: new(tracepack.BoundaryKindGap),
	})

	closeClaimingBoundary := eventRecord(t, 4, hourStart+4, 1, &tracepack.TransportEvent{Event: tracepack.EventSocketClose})
	closeClaimingBoundary.Quality = tracepack.QualityCaptureBoundary

	undecodableClaimingBoundary := eventRecord(t, 5, hourStart+5, 1, &tracepack.TransportEvent{Event: tracepack.EventCaptureBoundary})
	undecodableClaimingBoundary.Payload = []byte{0xFF}
	undecodableClaimingBoundary.Quality = tracepack.QualityCaptureBoundary

	in := []tracepack.Record{
		unknownEpoch, keptIncomplete, dataClaimingBoundary, boundaryWithoutBit, closeClaimingBoundary, undecodableClaimingBoundary,
	}
	want := make([]tracepack.Record, len(in))
	copy(want, in)
	want[0].Quality = tracepack.QualityCorrelationIncomplete
	want[2].Quality = 0
	want[3].Quality = tracepack.QualityCaptureBoundary
	want[4].Quality = 0
	want[5].Quality = 0

	w, buf := newTestWriter(t, tracepack.WriterOptions{})
	appendAll(t, w, in)
	mustClose(t, w)

	p := mustWalkPack(t, buf.Bytes())
	assert.Equal(t, want, p.records())
}

func TestWriterStoresFieldValidity(t *testing.T) {
	t.Parallel()

	// A 7-byte short capture holds SessionID and byte 6 only.
	short := tracepack.Record{
		Seq: 0, TSUTCNs: hourStart, Kind: tracepack.KindData, Dir: tracepack.DirHostToEquipment,
		DecodeStatus: tracepack.DecodeStatusShortFrame, Payload: s1f3Frame[:7],
	}
	short.SetCapturedFieldValidity()

	// A log conversion keeps a clear bit over placeholder bytes (the tracepack storage specification §7).
	logConverted := dataRecord(1, hourStart+1)
	logConverted.FieldValidity &^= tracepack.FieldValiditySystemBytes

	// Reserved bits are written as zero (I-9).
	reserved := dataRecord(2, hourStart+2)
	reserved.FieldValidity |= 0xC0

	// A record without an HSMS frame always has field_validity 0.
	event := tracepack.Record{
		Seq: 3, TSUTCNs: hourStart + 3, Kind: tracepack.KindTransportEvent,
		DecodeStatus:  tracepack.DecodeStatusNotApplicable,
		Payload:       mustEventPayload(t, &tracepack.TransportEvent{Event: tracepack.EventClockStep, ClockStepNs: new(int64(5))}),
		FieldValidity: allValidity,
	}

	w, buf := newTestWriter(t, tracepack.WriterOptions{})
	appendAll(t, w, []tracepack.Record{short, logConverted, reserved, event})
	mustClose(t, w)

	p := mustWalkPack(t, buf.Bytes())
	got := p.records()
	require.Len(t, got, 4)
	assert.Equal(t, tracepack.FieldValiditySessionID|tracepack.FieldValidityStreamAndW, got[0].FieldValidity)
	assert.Equal(t, allValidity&^tracepack.FieldValiditySystemBytes, got[1].FieldValidity)
	assert.Equal(t, allValidity, got[2].FieldValidity)
	assert.Equal(t, tracepack.FieldValidity(0), got[3].FieldValidity)
}

func TestWriterRejectsFieldValidityBeyondPayload(t *testing.T) {
	t.Parallel()

	for _, validate := range []bool{false, true} {
		w, buf := newTestWriter(t, tracepack.WriterOptions{SkipValidation: !validate})

		for _, n := range []int{5, 9, 13} {
			r := dataRecord(0, hourStart) // every bit set
			r.Payload = s1f3Frame[:n]
			r.DecodeStatus = tracepack.DecodeStatusShortFrame
			require.ErrorIs(t, w.Append(&r), tracepack.ErrFieldValidity, "validate %v, %d bytes", validate, n)
		}

		control := dataRecord(0, hourStart)
		control.Kind, control.Payload = tracepack.KindControl, linktestReqFrame[:9]
		require.ErrorIs(t, w.Append(&control), tracepack.ErrFieldValidity, "a control record is checked the same way")

		good := dataRecord(0, hourStart)
		require.NoError(t, w.Append(&good), "a rejected record leaves the Writer usable")
		mustClose(t, w)

		p := mustWalkPack(t, buf.Bytes())
		assert.Equal(t, []tracepack.Record{good}, p.records(), "no rejected record is written")
	}
}

func TestWriterCodecsDecodeToEqualBodies(t *testing.T) {
	t.Parallel()

	bodies := make(map[tracepack.Codec][][]byte)
	for _, c := range []tracepack.Codec{tracepack.CodecNone, tracepack.CodecZstd} {
		w, buf := newTestWriter(t, tracepack.WriterOptions{Codec: c, BlockThreshold: 250, PackID: uuidOfByte(1), CaptureID: uuidOfByte(2)})
		recs := mixedRecords(t)
		appendAll(t, w, recs)
		mustClose(t, w)

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

		count, hlen := int(b.Env.RecordCount), int(b.Env.RecordHeaderLen)
		for i := range count {
			h, err := format.UnmarshalRecordHeader(gatherHeader(decoded, count, hlen, i), hlen)
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
	mustClose(t, w)

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
	mustClose(t, w)

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
	mustClose(t, w)

	p := mustWalkPack(t, buf.Bytes())
	require.Len(t, p.Blocks, 3)
	assert.Equal(t, len(p.Blocks)+1, s.calls, "one sync per block and one after the trailer")
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
	require.ErrorIs(t, closeErr(w), tracepack.ErrWriterFailed)
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

// shortWriter accepts every byte until full bytes are written,
// then reports one byte fewer than each later write was given, with a nil error.
type shortWriter struct {
	buf  bytes.Buffer
	full int
}

func (s *shortWriter) Write(p []byte) (int, error) {
	if s.buf.Len()+len(p) <= s.full {
		return s.buf.Write(p)
	}
	if len(p) == 0 {
		return 0, nil
	}

	return s.buf.Write(p[:len(p)-1])
}

func TestNewWriterRejectsShortWrite(t *testing.T) {
	t.Parallel()

	sw := &shortWriter{}
	w, err := tracepack.NewWriter(sw, tracepack.WriterOptions{Meta: writerMeta()})
	require.ErrorIs(t, err, io.ErrShortWrite)
	assert.Nil(t, w)
}

func TestWriterShortWriteFailsWriter(t *testing.T) {
	t.Parallel()

	var probe bytes.Buffer
	_, err := tracepack.NewWriter(&probe, tracepack.WriterOptions{Meta: writerMeta(), Facts: tracepack.PackFacts{AnyClassified: true}})
	require.NoError(t, err)

	sw := &shortWriter{full: probe.Len()}
	w, err := tracepack.NewWriter(sw, tracepack.WriterOptions{Meta: writerMeta(), Facts: tracepack.PackFacts{AnyClassified: true}})
	require.NoError(t, err)

	r := dataRecord(0, hourStart)
	require.NoError(t, w.Append(&r))
	require.ErrorIs(t, w.Flush(), io.ErrShortWrite)
	written := sw.buf.Len()

	r = dataRecord(1, hourStart+1)
	require.ErrorIs(t, w.Append(&r), tracepack.ErrWriterFailed)
	err = closeErr(w)
	require.ErrorIs(t, err, tracepack.ErrWriterFailed)
	require.ErrorIs(t, err, io.ErrShortWrite, "the failure names its cause")
	assert.Equal(t, written, sw.buf.Len(), "no footer and no trailer follow a short write")
}

func TestWriterRejectsCallsAfterClose(t *testing.T) {
	t.Parallel()

	w, buf := newTestWriter(t, tracepack.WriterOptions{})
	r := dataRecord(0, hourStart)
	require.NoError(t, w.Append(&r))
	mustClose(t, w)
	n := buf.Len()

	r = dataRecord(1, hourStart+1)
	require.ErrorIs(t, w.Append(&r), tracepack.ErrClosed)
	require.ErrorIs(t, w.Flush(), tracepack.ErrClosed)
	require.ErrorIs(t, closeErr(w), tracepack.ErrClosed)
	assert.Equal(t, n, buf.Len())
}

// aboveBodyLimit is 2^31, one above the largest block body, as a variable so the tests compile where int is 32 bits.
var aboveBodyLimit int64 = 1 << 31

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
		// Where int is 32 bits the conversion wraps to a negative threshold, refused all the same.
		{"threshold above the body limit", tracepack.WriterOptions{Meta: writerMeta(), BlockThreshold: int(aboveBodyLimit)}},
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

	_, buf := newTestWriter(t, tracepack.WriterOptions{Meta: extractMeta(), Facts: tracepack.PackFacts{AnyRedacted: true}})

	p := mustWalkPack(t, buf.Bytes())
	assert.Equal(t, uint32(1), p.Header.Flags)
}

// asExtract makes m the pack metadata of an extract, the one pack role whose records may span UTC hours.
func asExtract(m *tracepack.PackMeta) *tracepack.PackMeta {
	m.PackRole = tracepack.PackRoleExtract
	m.ExtractFilter = new("stream 1")
	m.ScopeGeneration = nil

	return m
}

// extractMeta returns writerMeta for an extract pack, with one redaction entry for seq 0.
func extractMeta() *tracepack.PackMeta {
	m := asExtract(writerMeta())
	m.Redaction = []tracepack.RedactionEntry{{
		Seq: 0, Domain: "d", Digest: make([]byte, 32), MaskedRanges: []tracepack.MaskedRange{{Offset: 10, Length: 4}},
	}}

	return m
}

func TestWriterRejectsRecordMetadataCannotDescribe(t *testing.T) {
	t.Parallel()

	redactedMeta := writerMeta()
	redactedMeta.Redaction = extractMeta().Redaction

	tests := []struct {
		name  string
		meta  *tracepack.PackMeta
		facts tracepack.PackFacts
		edit  func(r *tracepack.Record)
		field string
	}{
		{
			"classified record without classifier", hourMeta(), tracepack.PackFacts{},
			func(r *tracepack.Record) { r.DecodeStatus = tracepack.DecodeStatusOK },
			"classifier",
		},
		{
			"unregistered decode_status without classifier", hourMeta(), tracepack.PackFacts{},
			func(r *tracepack.Record) { r.DecodeStatus = 200 },
			"classifier",
		},
		{
			"oversized record without max_frame_len", writerMeta(), tracepack.PackFacts{AnyClassified: true},
			func(r *tracepack.Record) { r.DecodeStatus = tracepack.DecodeStatusOversized },
			"max_frame_len",
		},
		{
			"redacted record without redaction-present", extractMeta(), tracepack.PackFacts{AnyClassified: true},
			func(r *tracepack.Record) { r.Quality = tracepack.QualityRedacted },
			"redaction-present",
		},
		{
			"redacted record outside an extract", redactedMeta, tracepack.PackFacts{AnyClassified: true},
			func(r *tracepack.Record) { r.Quality = tracepack.QualityRedacted },
			"pack_role",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			w, err := tracepack.NewWriter(&buf, tracepack.WriterOptions{Meta: tt.meta, Facts: tt.facts})
			require.NoError(t, err)

			bad := dataRecord(0, hourStart)
			bad.DecodeStatus = tracepack.DecodeStatusNotAttempted
			tt.edit(&bad)
			err = w.Append(&bad)
			require.ErrorIs(t, err, tracepack.ErrMetadataCommitment)
			var fe *tracepack.FieldError
			require.ErrorAs(t, err, &fe)
			assert.Equal(t, tt.field, fe.Field)

			good := dataRecord(0, hourStart)
			good.DecodeStatus = tracepack.DecodeStatusNotAttempted
			require.NoError(t, w.Append(&good), "the rejected record was not added and the Writer is still usable")
			mustClose(t, w)

			p := mustWalkPack(t, buf.Bytes())
			assert.Equal(t, []tracepack.Record{good}, p.records())
		})
	}
}

func TestWriterAcceptsRecordsMetadataDescribes(t *testing.T) {
	t.Parallel()

	unclassified := hourMeta()
	oversizedMeta := writerMeta()
	oversizedMeta.MaxFrameLens = []uint64{1 << 20}

	tests := []struct {
		name  string
		meta  *tracepack.PackMeta
		facts tracepack.PackFacts
		edit  func(r *tracepack.Record)
	}{
		{
			"not-attempted without classifier", unclassified, tracepack.PackFacts{},
			func(r *tracepack.Record) { r.DecodeStatus = tracepack.DecodeStatusNotAttempted },
		},
		{
			"not-applicable without classifier", unclassified, tracepack.PackFacts{},
			func(r *tracepack.Record) { r.DecodeStatus = tracepack.DecodeStatusNotApplicable },
		},
		{
			"oversized with max_frame_len", oversizedMeta, tracepack.PackFacts{AnyOversized: true},
			func(r *tracepack.Record) { r.DecodeStatus = tracepack.DecodeStatusOversized },
		},
		{
			"redacted in an extract with redaction-present", extractMeta(), tracepack.PackFacts{AnyClassified: true, AnyRedacted: true},
			func(r *tracepack.Record) { r.Quality = tracepack.QualityRedacted },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			w, err := tracepack.NewWriter(&buf, tracepack.WriterOptions{Meta: tt.meta, Facts: tt.facts})
			require.NoError(t, err)

			r := dataRecord(0, hourStart)
			tt.edit(&r)
			require.NoError(t, w.Append(&r))
			mustClose(t, w)

			p := mustWalkPack(t, buf.Bytes())
			assert.Equal(t, []tracepack.Record{r}, p.records())
		})
	}
}

func TestValidatingWriterWritesAgreeingPack(t *testing.T) {
	t.Parallel()

	meta := writerMeta()
	w, buf := newTestWriter(t, tracepack.WriterOptions{Meta: meta, Codec: tracepack.CodecZstd, BlockThreshold: 200})
	recs := mixedRecords(t)
	appendAll(t, w, recs)
	mustClose(t, w)

	p := mustWalkPack(t, buf.Bytes())
	assert.Equal(t, meta, p.Meta, "validation commits nothing to the pack metadata")
	assert.Greater(t, len(p.Blocks), 1)
	assert.Equal(t, recs, p.records())
}

func TestWriterDropsRetiredMetadataTags(t *testing.T) {
	t.Parallel()

	meta := writerMeta()
	meta.Unknown = []tracepack.RawEntry{
		{Tag: 0x0001, Type: 4, Value: make([]byte, 8)},
		{Tag: 0x0014, Type: 2, Value: []byte{1}},
		{Tag: 0x0030, Type: 2, Value: []byte{1}},
		{Tag: 0x0050, Type: 7, Value: []byte{0xAA}},
	}
	for _, validate := range []bool{false, true} {
		_, buf := newTestWriter(t, tracepack.WriterOptions{Meta: meta, SkipValidation: !validate})

		p := mustWalkPack(t, buf.Bytes())
		assert.Equal(t, []tracepack.RawEntry{{Tag: 0x0050, Type: 7, Value: []byte{0xAA}}}, p.Meta.Unknown,
			"validate %v: retired tags are never written, other unknown tags are kept", validate)
	}
}

// corruptFirstSeq returns a hook that inverts byte 0 of the encoded body of block n and later ones, counting from 0,
// which for codec none is byte 0 of the first record's seq, so the block's first seq no longer equals first_seq (I-2).
func corruptFirstSeq(n int) func(enc []byte) []byte {
	block := 0

	return func(enc []byte) []byte {
		if block >= n {
			enc[0] ^= 0xFF
		}
		block++

		return enc
	}
}

func TestValidatingWriterRejectsCorruptEncoding(t *testing.T) {
	t.Parallel()

	w, buf := newTestWriter(t, tracepack.WriterOptions{})
	tracepack.SetEncodedHook(w, corruptFirstSeq(1))

	good := dataRecord(0, hourStart)
	require.NoError(t, w.Append(&good))
	require.NoError(t, w.Flush())
	written := buf.Len()

	bad := dataRecord(1, hourStart+1)
	require.NoError(t, w.Append(&bad), "the defect is found when the block is checked, before it is written")
	require.ErrorIs(t, w.Flush(), tracepack.ErrValidation)
	assert.Equal(t, written, buf.Len(), "nothing of the failed block is written")

	next := dataRecord(2, hourStart+2)
	err := w.Append(&next)
	require.ErrorIs(t, err, tracepack.ErrWriterFailed)
	require.ErrorIs(t, err, tracepack.ErrValidation)
	require.ErrorIs(t, w.Flush(), tracepack.ErrWriterFailed)
	require.ErrorIs(t, closeErr(w), tracepack.ErrWriterFailed)
	assert.Equal(t, written, buf.Len())

	p := mustWalkPack(t, buf.Bytes())
	assert.Equal(t, [][]uint64{{0}}, blockSeqs(p))
}

// TestValidatingWriterChecksColumnarSection injects I-2 defects into the column-by-column header section
// of an encoded three-record block, after encoding, under both codecs:
// the validation decodes the body and gathers the headers before it checks them (the tracepack format specification §12).
func TestValidatingWriterChecksColumnarSection(t *testing.T) {
	t.Parallel()

	const (
		count      = 3
		bodyLen    = count * (format.RecordHeaderLen + 14)
		seqOff     = 0
		payloadOff = 28
	)
	defects := []struct {
		name string
		// at is the offset of the changed byte in the header section: byte j of record i at j × count + i.
		at int
		to byte
	}{
		{name: "second seq repeats the first", at: seqOff*count + 1, to: 0},
		{name: "last payload_len grows", at: payloadOff*count + 2, to: 15},
	}
	for _, c := range []tracepack.Codec{tracepack.CodecNone, tracepack.CodecZstd} {
		for _, d := range defects {
			t.Run(c.String()+" "+d.name, func(t *testing.T) {
				t.Parallel()

				w, buf := newTestWriter(t, tracepack.WriterOptions{Codec: c})
				tracepack.SetEncodedHook(w, func(enc []byte) []byte {
					body, err := codec.Decode(uint8(c), nil, enc, bodyLen)
					require.NoError(t, err)
					body[d.at] = d.to
					out, err := codec.Encode(uint8(c), nil, body)
					require.NoError(t, err)

					return out
				})
				head := buf.Len()
				for i := range count {
					r := dataRecord(uint64(i), hourStart+int64(i))
					require.NoError(t, w.Append(&r))
				}
				require.ErrorIs(t, w.Flush(), tracepack.ErrValidation)
				require.ErrorIs(t, closeErr(w), tracepack.ErrWriterFailed)
				assert.Equal(t, head, buf.Len(), "neither the block nor a trailer is written")
			})
		}
	}
}

func TestNonValidatingWriterWritesCorruptEncoding(t *testing.T) {
	t.Parallel()

	w, buf := newTestWriter(t, tracepack.WriterOptions{SkipValidation: true})
	tracepack.SetEncodedHook(w, corruptFirstSeq(0))
	r := dataRecord(0, hourStart)
	require.NoError(t, w.Append(&r))
	mustClose(t, w)

	_, err := walkPack(buf.Bytes())
	require.Error(t, err, "without validation the defect reaches the file, where a reader finds it")
}

// clockTolerance is the clock_step_tolerance_ns of clockMeta: 1 ms.
const clockTolerance int64 = 1_000_000

// clockMeta returns writerMeta for a capture-clock pack,
// whose clock anchor starts at (capture_origin_utc_ns = hourStart, mono 0) and tolerates clockTolerance of drift.
func clockMeta() *tracepack.PackMeta {
	m := writerMeta()
	m.TimeSource = tracepack.TimeSourceCaptureClock
	m.CaptureOriginUTCNs = new(hourStart)
	m.CaptureOriginMonoNs = new(int64(123_456)) // the process clock reading; record mono values are relative to it
	m.ClockStepToleranceNs = new(uint64(clockTolerance))

	return m
}

// monoRecord returns a data record observed at wall clock wall and capture-relative mono clock mono.
func monoRecord(wall, mono int64) tracepack.Record {
	r := dataRecord(0, wall)
	r.MonoNs = mono

	return r
}

// clockStepRecord returns the record a detecting Writer writes before r under seq for a step of drift.
func clockStepRecord(t *testing.T, r *tracepack.Record, seq uint64, drift int64) tracepack.Record {
	t.Helper()

	return tracepack.Record{
		Seq: seq, TSUTCNs: r.TSUTCNs, MonoNs: r.MonoNs, MonoPresent: true, Epoch: r.Epoch,
		Kind: tracepack.KindTransportEvent, Dir: tracepack.DirLocal,
		Fidelity: tracepack.FidelityNotApplicable, DecodeStatus: tracepack.DecodeStatusNotApplicable,
		Payload: mustEventPayload(t, &tracepack.TransportEvent{Event: tracepack.EventClockStep, ClockStepNs: &drift}),
	}
}

// newClockWriter opens a Writer over a fresh buffer for clockMeta with assigned seqs and the given options.
func newClockWriter(t *testing.T, opts tracepack.WriterOptions) (*tracepack.Writer, *bytes.Buffer) {
	t.Helper()

	opts.Meta = clockMeta()
	opts.AssignSeq = true

	return newTestWriter(t, opts)
}

func TestWriterDetectsClockStep(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	s := &sizeSyncer{buf: &buf}
	w, err := tracepack.NewWriter(&buf, tracepack.WriterOptions{
		Meta: clockMeta(), Facts: tracepack.PackFacts{AnyClassified: true}, AssignSeq: true, DetectClockSteps: true, Sync: s,
	})
	require.NoError(t, err)

	in := []tracepack.Record{
		monoRecord(hourStart+1_000, 1_000),     // drift 0 against (hourStart, 0)
		monoRecord(hourStart+2_000, 2_000),     // drift 0
		monoRecord(hourStart+5_003_000, 3_000), // drift +5 ms: a step forward
		monoRecord(hourStart+5_404_000, 4_000), // drift +0.4 ms against the new anchor (hourStart+5_003_000, 3_000)
		monoRecord(hourStart+2_005_000, 5_000), // drift -3 ms against it: a step back
	}
	in[2].Epoch = 4
	for i := range in {
		require.NoError(t, w.Append(&in[i]), "record %d", i)
	}
	assert.Equal(t, []uint64{0, 1, 3, 4, 6}, []uint64{in[0].Seq, in[1].Seq, in[2].Seq, in[3].Seq, in[4].Seq},
		"each step takes the seq before its record")
	mustClose(t, w)

	p := mustWalkPack(t, buf.Bytes())
	assert.Equal(t, [][]uint64{{0, 1}, {2}, {3, 4}, {5}, {6}}, blockSeqs(p),
		"the open block is closed before the step, and the step is written alone before its record")
	want := []tracepack.Record{
		in[0], in[1],
		clockStepRecord(t, &in[2], 2, 5_000_000), in[2], in[3],
		clockStepRecord(t, &in[4], 5, -3_000_000), in[4],
	}
	assert.Equal(t, want, p.records())

	require.Len(t, s.sizes, len(p.Blocks)+1, "every block, the step's included, is synced; then the trailer")
	for i, b := range p.Blocks {
		assert.Equal(t, b.Offset+format.EnvelopeLen+len(b.Body), s.sizes[i], "block %d is synced once written", i)
	}
}

func TestWriterClockStepBeforeFirstRecord(t *testing.T) {
	t.Parallel()

	w, buf := newClockWriter(t, tracepack.WriterOptions{DetectClockSteps: true})
	r := monoRecord(hourStart+2*clockTolerance, 0)
	require.NoError(t, w.Append(&r))
	mustClose(t, w)

	p := mustWalkPack(t, buf.Bytes())
	assert.Equal(t, [][]uint64{{0}, {1}}, blockSeqs(p), "no open block to close: the step is the first record")
	assert.Equal(t, []tracepack.Record{clockStepRecord(t, &r, 0, 2*clockTolerance), r}, p.records())
}

func TestWriterClockDriftWithinTolerance(t *testing.T) {
	t.Parallel()

	w, buf := newClockWriter(t, tracepack.WriterOptions{DetectClockSteps: true})
	in := []tracepack.Record{
		monoRecord(hourStart+1_000+clockTolerance, 1_000), // drift exactly +tolerance
		monoRecord(hourStart+2_000, clockTolerance+2_000), // drift exactly -tolerance
		monoRecord(hourStart+3_000, 3_000),
	}
	appendAssigned(t, w, in)
	mustClose(t, w)

	p := mustWalkPack(t, buf.Bytes())
	assert.Equal(t, in, p.records(), "a drift that does not exceed the tolerance is no step")
}

func TestWriterClockStepSkipsRecordsWithoutMono(t *testing.T) {
	t.Parallel()

	w, buf := newClockWriter(t, tracepack.WriterOptions{DetectClockSteps: true})
	noMono := monoRecord(hourStart+hourNs/2, 0)
	noMono.MonoPresent, noMono.MonoNs = false, 0
	in := []tracepack.Record{
		monoRecord(hourStart+1_000, 1_000),
		noMono, // 30 minutes off, but without mono the rule does not apply
		monoRecord(hourStart+hourNs/2+500, hourNs/2), // drift +500 ns against the unmoved anchor
	}
	appendAssigned(t, w, in)
	mustClose(t, w)

	p := mustWalkPack(t, buf.Bytes())
	assert.Equal(t, in, p.records())
}

func TestWriterAdoptsReceivedClockStepAsAnchor(t *testing.T) {
	t.Parallel()

	w, buf := newClockWriter(t, tracepack.WriterOptions{DetectClockSteps: true})
	received := monoRecord(hourStart+10*clockTolerance+1_000, 1_000)
	received = clockStepRecord(t, &received, 0, 10*clockTolerance)
	in := []tracepack.Record{
		monoRecord(hourStart, 0),
		received, // exempt from the rule, and the new anchor
		monoRecord(hourStart+10*clockTolerance+2_000, 2_000), // drift 0 against the received step
	}
	appendAssigned(t, w, in)
	mustClose(t, w)

	p := mustWalkPack(t, buf.Bytes())
	assert.Equal(t, in, p.records(), "the writer adds no step of its own")
}

func TestWriterMakesCallerClockStepDurableBeforeAnchoring(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	s := &sizeSyncer{buf: &buf}
	w, err := tracepack.NewWriter(&buf, tracepack.WriterOptions{
		Meta: clockMeta(), Facts: tracepack.PackFacts{AnyClassified: true}, AssignSeq: true, DetectClockSteps: true, Sync: s,
	})
	require.NoError(t, err)

	before := monoRecord(hourStart, 0)
	require.NoError(t, w.Append(&before))
	assert.Empty(t, s.sizes, "a record without a step stays in the open block")

	stepped := monoRecord(hourStart+10*clockTolerance+1_000, 1_000)
	step := clockStepRecord(t, &stepped, 0, 10*clockTolerance)
	require.NoError(t, w.Append(&step))
	require.Len(t, s.sizes, 1, "the block holding the caller's step is written and synced by its Append")
	assert.Equal(t, buf.Len(), s.sizes[0])

	// Drift 0 against the step, 10 tolerances against the capture origin:
	// without a new anchor the writer would insert a step of its own before it.
	after := monoRecord(hourStart+10*clockTolerance+2_000, 2_000)
	require.NoError(t, w.Append(&after))
	assert.Equal(t, step.Seq+1, after.Seq, "no step is inserted: the record is anchored on the caller's step")
	mustClose(t, w)

	p := mustWalkPack(t, buf.Bytes())
	assert.Equal(t, [][]uint64{{0, 1}, {2}}, blockSeqs(p), "the step's block precedes the record's")
	assert.Equal(t, p.Blocks[0].Offset+format.EnvelopeLen+len(p.Blocks[0].Body), s.sizes[0])
	assert.Equal(t, []tracepack.Record{before, step, after}, p.records())
}

func TestWriterWithoutDetectionPreservesClockSteps(t *testing.T) {
	t.Parallel()

	w, buf := newClockWriter(t, tracepack.WriterOptions{})
	drifted := monoRecord(hourStart+50*clockTolerance, 1_000)
	in := []tracepack.Record{
		monoRecord(hourStart, 0),
		clockStepRecord(t, &drifted, 0, 50*clockTolerance), // producer-detected
		drifted,
		monoRecord(hourStart+hourNs/4, 2_000), // a drift no producer marked: stored as received
	}
	appendAssigned(t, w, in)
	mustClose(t, w)

	p := mustWalkPack(t, buf.Bytes())
	assert.Equal(t, in, p.records())
	assert.Equal(t, [][]uint64{{0, 1, 2, 3}}, blockSeqs(p))
}

func TestWriterClockStepDriftSaturates(t *testing.T) {
	t.Parallel()

	// An extract, since the record lies in another UTC hour than the period.
	w, buf := newTestWriter(t, tracepack.WriterOptions{Meta: asExtract(clockMeta()), AssignSeq: true, DetectClockSteps: true})
	// (wall - anchor wall) - mono is far below -2^63: the recorded step saturates.
	r := monoRecord(math.MinInt64, math.MaxInt64)
	require.NoError(t, w.Append(&r))
	mustClose(t, w)

	p := mustWalkPack(t, buf.Bytes())
	require.Len(t, p.records(), 2)
	ev, err := tracepack.UnmarshalTransportEvent(p.records()[0].Payload)
	require.NoError(t, err)
	assert.Equal(t, tracepack.EventClockStep, ev.Event)
	assert.Equal(t, new(int64(math.MinInt64)), ev.ClockStepNs)
}

func TestNewWriterRejectsClockStepOptions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts tracepack.WriterOptions
	}{
		{"caller seqs leave no seq for a step", tracepack.WriterOptions{Meta: clockMeta(), DetectClockSteps: true}},
		{"not a capture-clock pack", tracepack.WriterOptions{Meta: writerMeta(), DetectClockSteps: true, AssignSeq: true}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tt.opts.Facts.AnyClassified = true
			var buf bytes.Buffer
			w, err := tracepack.NewWriter(&buf, tt.opts)
			require.Error(t, err)
			assert.Nil(t, w)
			assert.Zero(t, buf.Len())
		})
	}
}

// appendAssigned appends every record of recs to a Writer that assigns seqs,
// storing each assigned seq back in recs.
func appendAssigned(t *testing.T, w *tracepack.Writer, recs []tracepack.Record) {
	t.Helper()

	for i := range recs {
		require.NoError(t, w.Append(&recs[i]), "record %d", i)
	}
}

func TestNewWriterRejectsRedactionPresentOutsideExtract(t *testing.T) {
	t.Parallel()

	m := writerMeta()
	m.Redaction = []tracepack.RedactionEntry{{Seq: 0, Domain: "d", Digest: make([]byte, 32)}}

	var buf bytes.Buffer
	_, err := tracepack.NewWriter(&buf, tracepack.WriterOptions{
		Meta:  m,
		Facts: tracepack.PackFacts{AnyClassified: true, AnyRedacted: true},
	})
	require.ErrorIs(t, err, tracepack.ErrMetadataCommitment)

	var fe *tracepack.FieldError
	require.ErrorAs(t, err, &fe)
	assert.Equal(t, "redaction-present", fe.Field)
	assert.Zero(t, buf.Len())
}

// TestNewWriterRejectsRetiredPackRole checks that a Writer never writes the retired pack_role 5
// (the tracepack format specification §9).
func TestNewWriterRejectsRetiredPackRole(t *testing.T) {
	t.Parallel()

	m := writerMeta()
	m.PackRole = tracepack.PackRole(5)

	var buf bytes.Buffer
	_, err := tracepack.NewWriter(&buf, tracepack.WriterOptions{Meta: m, Facts: tracepack.PackFacts{AnyClassified: true}})
	require.ErrorIs(t, err, tracepack.ErrFieldValue)

	var fe *tracepack.FieldError
	require.ErrorAs(t, err, &fe)
	assert.Equal(t, "pack_role", fe.Field)
	assert.Zero(t, buf.Len())
}

func TestWriterCloseFailsWithoutARedactedRecordUnderTheFlag(t *testing.T) {
	t.Parallel()

	filter := "S1F3"
	m := writerMeta()
	m.PackRole = tracepack.PackRoleExtract
	m.ScopeGeneration = nil
	m.ExtractFilter = &filter
	m.Redaction = []tracepack.RedactionEntry{{Seq: 0, Domain: "d", Digest: make([]byte, 32)}}

	w, buf := newTestWriter(t, tracepack.WriterOptions{
		Meta:  m,
		Facts: tracepack.PackFacts{AnyRedacted: true},
	})
	r := dataRecord(0, hourStart)
	require.NoError(t, w.Append(&r))

	err := closeErr(w)
	require.ErrorIs(t, err, tracepack.ErrMetadataCommitment)
	require.ErrorIs(t, closeErr(w), tracepack.ErrWriterFailed)
	tail := buf.Bytes()[buf.Len()-len(format.TrailerMagic):]
	assert.NotEqual(t, format.TrailerMagic, string(tail), "no trailer after the failed close")
}
