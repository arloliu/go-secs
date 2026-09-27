package tracepack_test

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"hash/crc32"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/internal/format"
	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// Times of the footer test pack: three consecutive UTC hours.
const (
	hour0 = hourStart
	hour1 = hourStart + hourNs
	hour2 = hourStart + 2*hourNs
)

// footerPackStep is one step of building the footer test pack: append Record, then Flush when Flush is set.
type footerPackStep struct {
	Record tracepack.Record
	Flush  bool
}

// eventRecord returns a transport-event record carrying e.
func eventRecord(t *testing.T, seq uint64, ts int64, epoch uint32, e *tracepack.TransportEvent) tracepack.Record {
	t.Helper()

	return tracepack.Record{
		Seq: seq, TSUTCNs: ts, MonoNs: ts - hourStart, MonoPresent: true, Epoch: epoch,
		Kind: tracepack.KindTransportEvent, Dir: tracepack.DirLocal, Fidelity: tracepack.FidelityNotApplicable,
		DecodeStatus: tracepack.DecodeStatusNotApplicable, Payload: mustEventPayload(t, e),
	}
}

// dataRecordIn returns dataRecord(seq, ts) in epoch.
func dataRecordIn(seq uint64, ts int64, epoch uint32) tracepack.Record {
	r := dataRecord(seq, ts)
	r.Epoch = epoch

	return r
}

// footerPackSteps builds a pack of four blocks with caller seqs from 10, gaps, four epochs,
// three capture boundaries (start, gap with bounds, stop) and a socket close:
//
//	block A, hour 0: 10 start e0, 11 data e1, 12 control e1, 13 short frame without mono e1 (ts not ascending)
//	block B, hour 1: 15 data e1, 16 socket-close e1, 20 gap e0, 21 data e2, 22 annotation e2
//	block C, hour 2: 23 data e2, then Flush
//	block D, hour 2: 24 stop e2, 25 data e3
func footerPackSteps(t *testing.T) []footerPackStep {
	t.Helper()

	gapStart, gapEnd := hour0+100, hour1+1
	control := dataRecordIn(12, hour0+20, 1)
	control.Kind, control.Dir, control.Payload = tracepack.KindControl, tracepack.DirEquipmentToHost, linktestReqFrame
	control.SetHeaderCopies()

	short := dataRecordIn(13, hour0+30, 1)
	short.Payload, short.DecodeStatus, short.MonoPresent, short.MonoNs = s1f3Frame[:9], tracepack.DecodeStatusShortFrame, false, 0
	short.SetHeaderCopies()
	short.Quality = tracepack.QualityDecodeFailed | tracepack.QualityNoMono

	note, err := (&tracepack.Annotation{AnnotationKind: tracepack.AnnotationKindNote, Text: new("note")}).MarshalBinary()
	require.NoError(t, err)
	annotation := tracepack.Record{
		Seq: 22, TSUTCNs: hour1 + 9, Epoch: 2, Kind: tracepack.KindAnnotation, Dir: tracepack.DirLocal,
		Fidelity: tracepack.FidelityNotApplicable, DecodeStatus: tracepack.DecodeStatusNotApplicable,
		Payload: note, Quality: tracepack.QualityNoMono,
	}

	return []footerPackStep{
		{Record: eventRecord(t, 10, hour0+50, 0, &tracepack.TransportEvent{
			Event: tracepack.EventCaptureBoundary, BoundaryKind: new(tracepack.BoundaryKindStart),
		})},
		{Record: dataRecordIn(11, hour0+10, 1)},
		{Record: control},
		{Record: short},
		{Record: dataRecordIn(15, hour1+5, 1)},
		{Record: eventRecord(t, 16, hour1+6, 1, &tracepack.TransportEvent{Event: tracepack.EventSocketClose})},
		{Record: eventRecord(t, 20, hour1+7, 0, &tracepack.TransportEvent{
			Event: tracepack.EventCaptureBoundary, BoundaryKind: new(tracepack.BoundaryKindGap), GapStart: &gapStart, GapEnd: &gapEnd,
		})},
		{Record: dataRecordIn(21, hour1+8, 2)},
		{Record: annotation},
		{Record: dataRecordIn(23, hour2+1, 2), Flush: true},
		{Record: eventRecord(t, 24, hour2+2, 2, &tracepack.TransportEvent{
			Event: tracepack.EventCaptureBoundary, BoundaryKind: new(tracepack.BoundaryKindStop),
		})},
		{Record: dataRecordIn(25, hour2+3, 3)},
	}
}

// writeFooterPack writes the footer test pack with opts, closes it, and returns the walked pack and Close's seq.
func writeFooterPack(t *testing.T, opts tracepack.WriterOptions) (*walkedPack, uint64) {
	t.Helper()

	b, next := writeFooterPackBytes(t, opts)

	return mustWalkPack(t, b), next
}

// writeFooterPackBytes writes the footer test pack with opts, closes it, and returns the file and Close's seq.
func writeFooterPackBytes(t *testing.T, opts tracepack.WriterOptions) ([]byte, uint64) {
	t.Helper()

	meta := writerMeta()
	meta.SeqStart = 10
	opts.Meta = meta
	w, buf := newTestWriter(t, opts)
	for i, s := range footerPackSteps(t) {
		r := s.Record
		require.NoError(t, w.Append(&r), "step %d", i)
		if s.Flush {
			require.NoError(t, w.Flush())
		}
	}

	next := mustClose(t, w)

	return buf.Bytes(), next
}

// mustClose closes w, failing the test on error, and returns the next seq.
func mustClose(t *testing.T, w *tracepack.Writer) uint64 {
	t.Helper()

	next, err := w.Close()
	require.NoError(t, err)

	return next
}

// statsFromRecords computes the F-5 pack statistics of recs by brute force over the records themselves,
// independent of any F-2 or F-3 value:
// the count arrays are indexed by raw enum value and sized by the largest value present plus one,
// content_bytes counts a 56-byte header per record, epoch entries are in ascending epoch order,
// and close_seq is the first socket-close event or stop boundary of each epoch.
func statsFromRecords(t *testing.T, recs []tracepack.Record) packStat {
	t.Helper()

	count := uint64(len(recs))
	s := packStat{RecordCount: &count, KindCounts: []uint64{}, DirCounts: []uint64{}, DecodeStatusCounts: []uint64{}}
	for i := range recs {
		r := &recs[i]
		s.KindCounts = countAt(s.KindCounts, uint8(r.Kind))
		s.DirCounts = countAt(s.DirCounts, uint8(r.Dir))
		s.DecodeStatusCounts = countAt(s.DecodeStatusCounts, uint8(r.DecodeStatus))
		s.ContentBytes += uint64(format.RecordHeaderLen + len(r.Payload))
		s.QualityUnion |= uint64(r.Quality)
		if i == 0 {
			s.TSMin, s.TSMax = new(r.TSUTCNs), new(r.TSUTCNs)
		}
		s.TSMin, s.TSMax = new(min(*s.TSMin, r.TSUTCNs)), new(max(*s.TSMax, r.TSUTCNs))
		s.SeqRanges = appendCoalesced(s.SeqRanges, tlv.SeqRange{First: r.Seq, Last: r.Seq})
		s.Epochs = mergeEpoch(t, s.Epochs, epochStat{
			Epoch: uint64(r.Epoch), RecordCount: 1, SeqFirst: r.Seq, SeqLast: r.Seq, TSMin: r.TSUTCNs, TSMax: r.TSUTCNs,
			CloseSeq: closeSeqOf(t, r),
		})
		if b, ok := boundaryOf(t, r); ok {
			s.Boundaries = append(s.Boundaries, b)
		}
	}
	slices.SortFunc(s.Epochs, func(a, b epochStat) int { return cmp.Compare(a.Epoch, b.Epoch) })

	return s
}

// blockStatFromRecords computes the F-3 summary of one block's records by brute force:
// the pack statistics less record_count and the ts extremes, plus max_payload_len,
// and seq_range entries only when the seqs are not contiguous.
func blockStatFromRecords(t *testing.T, recs []tracepack.Record) packStat {
	t.Helper()

	s := statsFromRecords(t, recs)
	s.RecordCount, s.TSMin, s.TSMax = nil, nil, nil

	var maxLen uint64
	for _, r := range recs {
		maxLen = max(maxLen, uint64(len(r.Payload)))
	}
	s.MaxPayloadLen = &maxLen
	if len(s.SeqRanges) == 1 {
		s.SeqRanges = nil
	}

	return s
}

// countAt increments counts[v], extending counts to hold index v.
func countAt(counts []uint64, v uint8) []uint64 {
	for len(counts) <= int(v) {
		counts = append(counts, 0)
	}
	counts[v]++

	return counts
}

// transportEvent decodes r's payload when r is a transport-event record.
func transportEvent(t *testing.T, r *tracepack.Record) (*tracepack.TransportEvent, bool) {
	t.Helper()

	if r.Kind != tracepack.KindTransportEvent {
		return nil, false
	}
	ev, err := tracepack.UnmarshalTransportEvent(r.Payload)
	require.NoError(t, err)

	return ev, true
}

// closeSeqOf returns r's seq when r ends its epoch: a socket-close event or a stop boundary.
func closeSeqOf(t *testing.T, r *tracepack.Record) *uint64 {
	t.Helper()

	ev, ok := transportEvent(t, r)
	if !ok {
		return nil
	}
	stop := ev.Event == tracepack.EventCaptureBoundary && ev.BoundaryKind != nil && *ev.BoundaryKind == tracepack.BoundaryKindStop
	if ev.Event != tracepack.EventSocketClose && !stop {
		return nil
	}

	return new(r.Seq)
}

// boundaryOf returns the boundary entry of r when r is a capture-boundary record.
func boundaryOf(t *testing.T, r *tracepack.Record) (boundaryStat, bool) {
	t.Helper()

	ev, ok := transportEvent(t, r)
	if !ok || ev.Event != tracepack.EventCaptureBoundary {
		return boundaryStat{}, false
	}

	b := boundaryStat{Seq: r.Seq, TS: r.TSUTCNs, Epoch: uint64(r.Epoch), GapStart: ev.GapStart, GapEnd: ev.GapEnd}
	if ev.BoundaryKind != nil {
		b.Kind = uint8(*ev.BoundaryKind)
	}

	return b, true
}

func TestWriterFooterPassesValidationList(t *testing.T) {
	t.Parallel()

	for _, c := range []tracepack.Codec{tracepack.CodecNone, tracepack.CodecZstd} {
		t.Run(c.String(), func(t *testing.T) {
			t.Parallel()

			p, _ := writeFooterPack(t, tracepack.WriterOptions{Codec: c, Validate: true})
			verifyFooter(t, p)
			assert.Equal(t, [][]uint64{{10, 11, 12, 13}, {15, 16, 20, 21, 22}, {23}, {24, 25}}, blockSeqs(p))
			assert.Equal(t, uint8(c), p.Footer.Trailer.FooterCodec, "the footer uses the block codec")
		})
	}
}

func TestFooterBlockIndexMatchesBlocks(t *testing.T) {
	t.Parallel()

	p, _ := writeFooterPack(t, tracepack.WriterOptions{Codec: tracepack.CodecZstd})
	verifyFooter(t, p)

	var summaryOff uint64
	for i, b := range p.Blocks {
		e := p.Footer.F2[i]
		recs := b.Records

		tsMin := slices.MinFunc(recs, func(a, b tracepack.Record) int { return cmp.Compare(a.TSUTCNs, b.TSUTCNs) }).TSUTCNs
		tsMax := slices.MaxFunc(recs, func(a, b tracepack.Record) int { return cmp.Compare(a.TSUTCNs, b.TSUTCNs) }).TSUTCNs
		epochMin := slices.MinFunc(recs, func(a, b tracepack.Record) int { return cmp.Compare(a.Epoch, b.Epoch) }).Epoch
		epochMax := slices.MaxFunc(recs, func(a, b tracepack.Record) int { return cmp.Compare(a.Epoch, b.Epoch) }).Epoch

		want := format.F2Entry{
			Offset:          uint64(b.Offset),
			OnDiskLen:       uint32(format.EnvelopeLen + len(b.Body)),
			UncompressedLen: b.Env.UncompressedLen,
			RecordCount:     uint32(len(recs)),
			BodyCRC:         format.CRC(b.Body),
			FirstSeq:        recs[0].Seq,
			LastSeq:         recs[len(recs)-1].Seq,
			TSMin:           tsMin,
			TSMax:           tsMax,
			EpochMin:        epochMin,
			EpochMax:        epochMax,
			SummaryOffset:   summaryOff,
			SummaryLen:      e.SummaryLen,
			RecordHeaderLen: format.RecordHeaderLen,
		}
		assert.Equal(t, want, e, "block %d", i)
		summaryOff += uint64(e.SummaryLen)
	}
	assert.Equal(t, p.Footer.Prologue.F3Len, summaryOff, "the F-3 lists fill F-3 in block order")

	// Block A's first record is not its earliest: ts_min is the true minimum.
	assert.Equal(t, hour0+10, p.Footer.F2[0].TSMin)
	assert.Equal(t, hour0+50, p.Footer.F2[0].TSMax)
}

func TestFooterBlockSummariesMatchRecords(t *testing.T) {
	t.Parallel()

	p, _ := writeFooterPack(t, tracepack.WriterOptions{})
	verifyFooter(t, p)

	for i, b := range p.Blocks {
		assert.Equal(t, blockStatFromRecords(t, b.Records), p.Footer.F3[i], "block %d", i)
	}
}

func TestFooterSeqRangeOnlyForNonContiguousBlocks(t *testing.T) {
	t.Parallel()

	p, _ := writeFooterPack(t, tracepack.WriterOptions{})
	verifyFooter(t, p)

	want := [][]tlv.SeqRange{
		nil, // 10-13
		{{First: 15, Last: 16}, {First: 20, Last: 22}},
		nil, // 23
		nil, // 24-25
	}
	got := make([][]tlv.SeqRange, 0, len(p.Footer.F3))
	for _, s := range p.Footer.F3 {
		got = append(got, s.SeqRanges)
	}
	assert.Equal(t, want, got)
	assert.Equal(t, []tlv.SeqRange{{First: 10, Last: 13}, {First: 15, Last: 16}, {First: 20, Last: 25}}, p.Footer.F5.SeqRanges)
}

func TestFooterEpochAndBoundaryEntries(t *testing.T) {
	t.Parallel()

	p, _ := writeFooterPack(t, tracepack.WriterOptions{})
	verifyFooter(t, p)

	// Block B holds epochs 0, 1 and 2, the socket close of epoch 1 and the gap boundary with its bounds.
	assert.Equal(t, []epochStat{
		{Epoch: 0, RecordCount: 1, SeqFirst: 20, SeqLast: 20, TSMin: hour1 + 7, TSMax: hour1 + 7},
		{Epoch: 1, RecordCount: 2, SeqFirst: 15, SeqLast: 16, TSMin: hour1 + 5, TSMax: hour1 + 6, CloseSeq: new(uint64(16))},
		{Epoch: 2, RecordCount: 2, SeqFirst: 21, SeqLast: 22, TSMin: hour1 + 8, TSMax: hour1 + 9},
	}, p.Footer.F3[1].Epochs)
	assert.Equal(t, []boundaryStat{{
		Seq: 20, Kind: uint8(tracepack.BoundaryKindGap), TS: hour1 + 7, Epoch: 0,
		GapStart: new(hour0 + 100), GapEnd: new(hour1 + 1),
	}}, p.Footer.F3[1].Boundaries)

	// Block D holds the stop boundary that closes epoch 2.
	assert.Equal(t, []epochStat{
		{Epoch: 2, RecordCount: 1, SeqFirst: 24, SeqLast: 24, TSMin: hour2 + 2, TSMax: hour2 + 2, CloseSeq: new(uint64(24))},
		{Epoch: 3, RecordCount: 1, SeqFirst: 25, SeqLast: 25, TSMin: hour2 + 3, TSMax: hour2 + 3},
	}, p.Footer.F3[3].Epochs)

	// F-5 combines each epoch across blocks and keeps the close_seq one block carries.
	assert.Equal(t, []epochStat{
		{Epoch: 0, RecordCount: 2, SeqFirst: 10, SeqLast: 20, TSMin: hour0 + 50, TSMax: hour1 + 7},
		{Epoch: 1, RecordCount: 5, SeqFirst: 11, SeqLast: 16, TSMin: hour0 + 10, TSMax: hour1 + 6, CloseSeq: new(uint64(16))},
		{Epoch: 2, RecordCount: 4, SeqFirst: 21, SeqLast: 24, TSMin: hour1 + 8, TSMax: hour2 + 2, CloseSeq: new(uint64(24))},
		{Epoch: 3, RecordCount: 1, SeqFirst: 25, SeqLast: 25, TSMin: hour2 + 3, TSMax: hour2 + 3},
	}, p.Footer.F5.Epochs)
	assert.Equal(t, []boundaryStat{
		{Seq: 10, Kind: uint8(tracepack.BoundaryKindStart), TS: hour0 + 50, Epoch: 0},
		{Seq: 20, Kind: uint8(tracepack.BoundaryKindGap), TS: hour1 + 7, Epoch: 0, GapStart: new(hour0 + 100), GapEnd: new(hour1 + 1)},
		{Seq: 24, Kind: uint8(tracepack.BoundaryKindStop), TS: hour2 + 2, Epoch: 2},
	}, p.Footer.F5.Boundaries)
}

func TestFooterPackStatsMatchRecords(t *testing.T) {
	t.Parallel()

	for _, c := range []tracepack.Codec{tracepack.CodecNone, tracepack.CodecZstd} {
		t.Run(c.String(), func(t *testing.T) {
			t.Parallel()

			p, _ := writeFooterPack(t, tracepack.WriterOptions{Codec: c})
			verifyFooter(t, p)
			assert.Equal(t, statsFromRecords(t, p.records()), p.Footer.F5)
		})
	}
}

func TestFooterPackStatsMatchRecordsAcrossManyBlocks(t *testing.T) {
	t.Parallel()

	// A threshold of one record per block and records spread over two hours with gaps.
	w, buf := newTestWriter(t, tracepack.WriterOptions{BlockThreshold: 150, Codec: tracepack.CodecZstd})
	recs := mixedRecords(t)
	for i := range 40 {
		r := dataRecordIn(uint64(len(recs)+2*i), hourStart+int64(i)*(hourNs/30), uint32(i%3))
		if i%7 == 0 {
			r.Payload = append(bytes.Clone(s1f3Frame), make([]byte, i)...)
		}
		recs = append(recs, r)
	}
	appendAll(t, w, recs)
	mustClose(t, w)

	p := mustWalkPack(t, buf.Bytes())
	verifyFooter(t, p)
	assert.Greater(t, len(p.Blocks), 10)
	assert.Equal(t, statsFromRecords(t, p.records()), p.Footer.F5)
}

func TestWriterEmptyPackFooter(t *testing.T) {
	t.Parallel()

	meta := writerMeta()
	meta.SeqStart = 7
	w, buf := newTestWriter(t, tracepack.WriterOptions{Meta: meta, Codec: tracepack.CodecZstd})
	assert.Equal(t, uint64(7), mustClose(t, w), "an empty pack's next seq is its seq_start")

	p := mustWalkPack(t, buf.Bytes())
	verifyFooter(t, p)
	f := p.Footer
	assert.Empty(t, p.Blocks)
	assert.Equal(t, uint64(format.FileHeaderLen+len(p.MetaBytes)), f.Trailer.FooterOffset)
	assert.Equal(t, uint32(0), f.Prologue.BlockCount)
	assert.Equal(t, uint32(0), f.Trailer.BlockCount)
	assert.Equal(t, uint64(0), f.Trailer.RecordCount)
	assert.Equal(t, uint64(0), f.Trailer.LastSeq, "ignored without records; written as 0")
	assert.Zero(t, f.Prologue.F3Len, "empty F-3")
	assert.Equal(t, packStat{
		RecordCount: new(uint64(0)), KindCounts: []uint64{}, DirCounts: []uint64{}, DecodeStatusCounts: []uint64{},
	}, f.F5)
}

func TestFooterCodecsDecodeEqual(t *testing.T) {
	t.Parallel()

	footers := make(map[tracepack.Codec]*walkedFooter)
	for _, c := range []tracepack.Codec{tracepack.CodecNone, tracepack.CodecZstd} {
		p, _ := writeFooterPack(t, tracepack.WriterOptions{Codec: c})
		verifyFooter(t, p)
		assert.Equal(t, uint8(c), p.Footer.Trailer.FooterCodec)
		footers[c] = p.Footer
	}

	none, zstd := footers[tracepack.CodecNone], footers[tracepack.CodecZstd]
	assert.Equal(t, none.Trailer.FooterUncompressedLen, none.Trailer.FooterLen, "the none codec stores the footer as is")
	assert.Equal(t, none.Prologue, zstd.Prologue)
	assert.Equal(t, none.F3, zstd.F3)
	assert.Equal(t, none.F5, zstd.F5)

	// F-2 differs only in what the block codec changes: offsets, on-disk lengths and body CRCs.
	require.Len(t, zstd.F2, len(none.F2))
	for i := range none.F2 {
		a, b := none.F2[i], zstd.F2[i]
		a.Offset, a.OnDiskLen, a.BodyCRC = 0, 0, 0
		b.Offset, b.OnDiskLen, b.BodyCRC = 0, 0, 0
		assert.Equal(t, a, b, "block %d", i)
	}
}

func TestTrailerBytes(t *testing.T) {
	t.Parallel()

	b, _ := writeFooterPackBytes(t, tracepack.WriterOptions{Codec: tracepack.CodecZstd})

	// Decode the trailer by the offsets of the tracepack format specification §11, without the format package.
	tr := b[len(b)-64:]
	le := binary.LittleEndian
	lastBlock := mustWalkPack(t, b).Blocks[3]
	footerOffset := uint64(lastBlock.Offset + format.EnvelopeLen + len(lastBlock.Body))

	assert.Equal(t, footerOffset, le.Uint64(tr[0:]), "footer_offset: the footer follows the last block")
	assert.Equal(t, uint64(len(b)-64)-footerOffset, le.Uint64(tr[8:]), "footer_len")
	assert.Equal(t, uint32(4), le.Uint32(tr[24:]), "block_count")
	assert.Equal(t, crc32.ChecksumIEEE(b[footerOffset:len(b)-64]), le.Uint32(tr[28:]), "footer_crc over the on-disk footer")
	assert.Equal(t, uint64(12), le.Uint64(tr[32:]), "record_count")
	assert.Equal(t, uint64(25), le.Uint64(tr[40:]), "last_seq")
	assert.Equal(t, uint16(1), le.Uint16(tr[48:]), "trailer_version")
	assert.Equal(t, uint8(tracepack.CodecZstd), tr[50], "footer_codec")
	assert.Equal(t, uint8(0), tr[51], "flags")
	assert.Equal(t, crc32.ChecksumIEEE(tr[:52]), le.Uint32(tr[52:]), "trailer_crc over bytes 0-51")
	assert.Equal(t, []byte{0x54, 0x50, 0x4B, 0x45, 0x4E, 0x44, 0x0D, 0x0A}, tr[56:], "magic TPKEND\\r\\n")
}

func TestWriterCloseReturnsNextSeq(t *testing.T) {
	t.Parallel()

	_, next := writeFooterPack(t, tracepack.WriterOptions{})
	assert.Equal(t, uint64(26), next, "caller seqs: the last seq plus one")

	w, _ := newTestWriter(t, tracepack.WriterOptions{AssignSeq: true, NextSeq: 100})
	for i := range 3 {
		r := dataRecord(0, hourStart+int64(i))
		require.NoError(t, w.Append(&r))
	}
	assert.Equal(t, uint64(103), mustClose(t, w), "assigned seqs: the last seq plus one")

	w, _ = newTestWriter(t, tracepack.WriterOptions{AssignSeq: true, NextSeq: 100})
	assert.Equal(t, uint64(100), mustClose(t, w), "no record: NextSeq, the pack's seq_start")
}

// sizeSyncer records the output length at every Sync call.
type sizeSyncer struct {
	buf   *bytes.Buffer
	sizes []int
}

func (s *sizeSyncer) Sync() error {
	s.sizes = append(s.sizes, s.buf.Len())
	return nil
}

func TestWriterCloseSyncsAfterTrailer(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	s := &sizeSyncer{buf: &buf}
	w, err := tracepack.NewWriter(&buf, tracepack.WriterOptions{
		Meta: writerMeta(), Facts: tracepack.PackFacts{AnyClassified: true}, Sync: s, BlockThreshold: 140,
	})
	require.NoError(t, err)

	for i := range 5 {
		r := dataRecord(uint64(i), hourStart+int64(i))
		require.NoError(t, w.Append(&r))
	}
	mustClose(t, w)

	p := mustWalkPack(t, buf.Bytes())
	verifyFooter(t, p)
	require.Len(t, p.Blocks, 3)
	require.Len(t, s.sizes, len(p.Blocks)+1, "one sync per block and one after the trailer")
	for i, b := range p.Blocks {
		assert.Equal(t, b.Offset+format.EnvelopeLen+len(b.Body), s.sizes[i], "block %d is synced once written", i)
	}
	assert.Equal(t, buf.Len(), s.sizes[len(s.sizes)-1], "the last sync follows the trailer")
}

func TestWriterCloseSyncsEmptyPack(t *testing.T) {
	t.Parallel()

	s := &countingSyncer{}
	w, _ := newTestWriter(t, tracepack.WriterOptions{Sync: s})
	mustClose(t, w)
	assert.Equal(t, 1, s.calls, "no block: only the sync after the trailer")
}

func TestWriterCloseAfterFailureWritesNothing(t *testing.T) {
	t.Parallel()

	w, buf := newTestWriter(t, tracepack.WriterOptions{Validate: true})
	good := dataRecord(0, hourStart)
	require.NoError(t, w.Append(&good))
	require.NoError(t, w.Flush())

	bad := s6f11CopiesOverS1F3(1)
	require.NoError(t, w.Append(&bad))
	require.ErrorIs(t, w.Flush(), tracepack.ErrValidation)
	written := buf.Len()

	next, err := w.Close()
	require.ErrorIs(t, err, tracepack.ErrWriterFailed)
	assert.Zero(t, next)
	assert.Equal(t, written, buf.Len(), "no footer and no trailer after a failure")

	p := mustWalkPack(t, buf.Bytes())
	assert.Nil(t, p.Footer, "the pack stays unfinalized")
	assert.Equal(t, [][]uint64{{0}}, blockSeqs(p))
}

func TestWriterCloseFailsOnFooterWriteError(t *testing.T) {
	t.Parallel()

	var probe bytes.Buffer
	pw, err := tracepack.NewWriter(&probe, tracepack.WriterOptions{Meta: writerMeta(), Facts: tracepack.PackFacts{AnyClassified: true}})
	require.NoError(t, err)
	r := dataRecord(0, hourStart)
	require.NoError(t, pw.Append(&r))
	require.NoError(t, pw.Flush())

	fw := &failingWriter{limit: probe.Len() + 10} // the footer write fails part way
	w, err := tracepack.NewWriter(fw, tracepack.WriterOptions{Meta: writerMeta(), Facts: tracepack.PackFacts{AnyClassified: true}})
	require.NoError(t, err)
	r = dataRecord(0, hourStart)
	require.NoError(t, w.Append(&r))

	_, err = w.Close()
	require.ErrorIs(t, err, errWrite)
	_, err = w.Close()
	require.ErrorIs(t, err, tracepack.ErrWriterFailed)
}

func TestWriterIDAccessors(t *testing.T) {
	t.Parallel()

	packID, captureID := uuidOfByte(0x31), uuidOfByte(0x32)
	w, _ := newTestWriter(t, tracepack.WriterOptions{PackID: packID, CaptureID: captureID})
	assert.Equal(t, packID, w.PackID())
	assert.Equal(t, captureID, w.CaptureID())

	w, buf := newTestWriter(t, tracepack.WriterOptions{})
	p := mustWalkPack(t, buf.Bytes())
	assert.Equal(t, tracepack.UUID(p.Header.PackID), w.PackID(), "a generated pack_id is the one written")
	assert.Equal(t, tracepack.UUID(p.Header.CaptureID), w.CaptureID(), "a generated capture_id is the one written")
	assert.False(t, w.PackID().IsZero())
}

func TestFooterPackStatsKeepFirstCloseSeq(t *testing.T) {
	t.Parallel()

	// Epoch 1 closes in the first block and again in the second: F-5 keeps the first close_seq.
	w, buf := newTestWriter(t, tracepack.WriterOptions{})
	closeEvent := &tracepack.TransportEvent{Event: tracepack.EventSocketClose}
	steps := []tracepack.Record{
		dataRecordIn(0, hourStart, 1),
		eventRecord(t, 1, hourStart+1, 1, closeEvent),
		eventRecord(t, 2, hourStart+2, 1, closeEvent),
	}
	for i := range steps {
		require.NoError(t, w.Append(&steps[i]))
		if i == 1 {
			require.NoError(t, w.Flush())
		}
	}
	mustClose(t, w)

	p := mustWalkPack(t, buf.Bytes())
	require.Len(t, p.Blocks, 2)
	assert.Equal(t, new(uint64(1)), p.Footer.F3[0].Epochs[0].CloseSeq)
	assert.Equal(t, new(uint64(2)), p.Footer.F3[1].Epochs[0].CloseSeq)
	require.Len(t, p.Footer.F5.Epochs, 1)
	assert.Equal(t, new(uint64(1)), p.Footer.F5.Epochs[0].CloseSeq)
	assert.Equal(t, statsFromRecords(t, p.records()), p.Footer.F5)
}
