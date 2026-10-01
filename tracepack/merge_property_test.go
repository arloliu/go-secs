package tracepack

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/codec"
	"github.com/arloliu/go-secs/tracepack/internal/format"
	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// walkedBlock is one block of a finalized pack,
// located by walking the blocks forward from the end of the pack metadata (I-1)
// and decoded from its bytes as the tracepack format specification §6 lays them out, without the Reader or the footer.
type walkedBlock struct {
	offset uint64
	// raw holds the block's envelope and on-disk body.
	raw []byte
	env format.BlockEnvelope
	// records holds the block's records in file order.
	records []recordFacts
}

// recordFacts is one record as its block stores it,
// with what it contributes to a footer summary (the tracepack format specification §10).
type recordFacts struct {
	// h is the record header, its 44 bytes known in format 1.0 decoded;
	// row is its header row as stored, record_header_len bytes, and payload its payload.
	h       format.RecordHeader
	row     []byte
	payload []byte
	// boundary is set for a capture-boundary record, a transport event whose payload is a capture-boundary event.
	boundary *boundarySummary
	// closes reports a record that ends its epoch: a socket-close event, or a capture-boundary event of kind stop.
	closes bool
}

// factsOf returns the record of header h, header row row and payload payload,
// with what it contributes to a footer summary.
// A transport event's payload is decoded with UnmarshalTransportEvent;
// one that does not decode contributes no boundary and closes no epoch.
// A capture-boundary event without boundary_kind is of kind unknown, 0.
func factsOf(h format.RecordHeader, row, payload []byte) recordFacts {
	f := recordFacts{h: h, row: row, payload: payload}
	if Kind(h.Kind) != KindTransportEvent {
		return f
	}
	ev, err := UnmarshalTransportEvent(payload)
	if err != nil {
		return f
	}

	// A socket-close event closes its epoch, a capture-boundary event is a boundary,
	// and any other event contributes nothing to a summary.
	f.closes = ev.Event == EventSocketClose
	if ev.Event == EventCaptureBoundary {
		kind := BoundaryKindUnknown
		if ev.BoundaryKind != nil {
			kind = *ev.BoundaryKind
		}
		f.boundary = &boundarySummary{seq: h.Seq, kind: kind, ts: h.TSUTCNs, epoch: h.Epoch, gapStart: ev.GapStart, gapEnd: ev.GapEnd}
		f.closes = kind == BoundaryKindStop
	}

	return f
}

// walkBlocks returns every block of the finalized pack file in file order, and the pack's trailer.
// It walks the envelopes from the end of the pack metadata to the trailer's footer_offset,
// requiring each block's body CRC and I-2 to hold and the last block to end at footer_offset.
func walkBlocks(t testing.TB, file []byte) ([]walkedBlock, format.Trailer) {
	t.Helper()

	l := layoutOf(t, file)
	var out []walkedBlock
	off := l.blocksStart
	for off < l.tr.FooterOffset {
		env, err := format.UnmarshalBlockEnvelope(file[off:])
		require.NoError(t, err, "the envelope at offset %d", off)
		end := off + format.EnvelopeLen + uint64(env.BodyLen)
		require.LessOrEqual(t, end, l.tr.FooterOffset, "the block at offset %d ends past footer_offset", off)
		body := file[off+format.EnvelopeLen : end]
		require.Equal(t, env.BodyCRC, format.CRC(body), "body_crc of the block at offset %d", off)
		decoded, err := codec.Decode(env.Codec, nil, body, int(env.UncompressedLen))
		require.NoError(t, err, "the body of the block at offset %d", off)

		b := walkedBlock{offset: off, raw: file[off:end], env: env}
		n, rhl := int(env.RecordCount), int(env.RecordHeaderLen)
		require.LessOrEqual(t, n*rhl, len(decoded), "the header section of the block at offset %d", off)
		rows := format.UntransposeHeaders(nil, decoded[:n*rhl], n, rhl)
		p := n * rhl
		for j := range n {
			row := rows[j*rhl : (j+1)*rhl : (j+1)*rhl]
			h, err := format.UnmarshalRecordHeader(row, format.RecordHeaderLen)
			require.NoError(t, err)
			require.LessOrEqual(t, p+int(h.PayloadLen), len(decoded), "record %d of the block at offset %d", j, off)
			b.records = append(b.records, factsOf(h, row, decoded[p:p+int(h.PayloadLen)]))
			p += int(h.PayloadLen)
		}
		require.Equal(t, len(decoded), p, "I-2: the records of the block at offset %d fill its body", off)
		require.NotEmpty(t, b.records)
		require.Equal(t, env.FirstSeq, b.records[0].h.Seq, "I-2: first_seq of the block at offset %d", off)

		out = append(out, b)
		off = end
	}
	require.Equal(t, l.tr.FooterOffset, off, "the blocks end at footer_offset")

	return out, l.tr
}

// derivedStats returns the statistics the tracepack format specification §10 defines over records, in ascending seq,
// whose decoded size, Σ record_header_len + payload_len, is content:
// the F-5 statistics of a pack's records, and the F-3 summary of a block's.
// Counts are per raw enum value, values outside the registry included;
// the time bounds are the true minimum and maximum;
// an epoch entry per epoch, whose close_seq is the first record that closes the epoch;
// a boundary entry per capture-boundary record; and the seqs as sorted maximal ranges.
func derivedStats(records []recordFacts, content uint64) *packStats {
	st := &packStats{recordCount: uint64(len(records)), contentBytes: content}
	index := make(map[uint32]int)
	for i := range records {
		f := &records[i]
		h := &f.h
		if i == 0 {
			st.tsMin, st.tsMax = h.TSUTCNs, h.TSUTCNs
		}
		st.tsMin, st.tsMax = min(st.tsMin, h.TSUTCNs), max(st.tsMax, h.TSUTCNs)
		st.kindCounts = countValue(st.kindCounts, h.Kind)
		st.dirCounts = countValue(st.dirCounts, h.Dir)
		st.decodeStatusCounts = countValue(st.decodeStatusCounts, h.DecodeStatus)
		st.qualityUnion |= Quality(h.Quality)

		if n := len(st.seqRanges); n > 0 && st.seqRanges[n-1].last+1 == h.Seq {
			st.seqRanges[n-1].last = h.Seq
		} else {
			st.seqRanges = append(st.seqRanges, seqRange{first: h.Seq, last: h.Seq})
		}

		k, ok := index[h.Epoch]
		if !ok {
			k = len(st.epochs)
			index[h.Epoch] = k
			st.epochs = append(st.epochs, epochSummary{epoch: h.Epoch, seqFirst: h.Seq, tsMin: h.TSUTCNs, tsMax: h.TSUTCNs})
		}
		e := &st.epochs[k]
		e.recordCount++
		e.seqLast = h.Seq
		e.tsMin, e.tsMax = min(e.tsMin, h.TSUTCNs), max(e.tsMax, h.TSUTCNs)
		if f.closes && !e.hasCloseSeq {
			e.closeSeq, e.hasCloseSeq = h.Seq, true
		}

		if f.boundary != nil {
			st.boundaries = append(st.boundaries, *f.boundary)
		}
	}

	return st
}

// countValue counts one record of enum value v in counts, indexed by value.
func countValue(counts []uint64, v uint8) []uint64 {
	if int(v) >= len(counts) {
		counts = append(counts, make([]uint64, int(v)+1-len(counts))...)
	}
	counts[v]++

	return counts
}

// narrowCounts returns the counts of a block, each below 2^32, as the u32 elements of its F-3 count arrays.
func narrowCounts(counts []uint64) []uint32 {
	out := make([]uint32, len(counts))
	for i, c := range counts {
		out[i] = uint32(c)
	}

	return out
}

// derivedSummary returns the F-2 entry, but for the location of its F-3 list, and the F-3 summary of block b,
// as the tracepack format specification §10 defines them from b's bytes and records:
// its offset, its on-disk length and body CRC, its record_header_len from its envelope,
// its decoded length, record count, first and last seq, true time and epoch extremes from its records,
// and the statistics derivedStats gives its records.
func derivedSummary(b *walkedBlock) blockSummary {
	recs := b.records
	var content uint64
	epochMin, epochMax := recs[0].h.Epoch, recs[0].h.Epoch
	for i := range recs {
		content += uint64(len(recs[i].row)) + uint64(len(recs[i].payload))
		epochMin, epochMax = min(epochMin, recs[i].h.Epoch), max(epochMax, recs[i].h.Epoch)
	}
	st := derivedStats(recs, content)

	return blockSummary{
		offset:             b.offset,
		onDiskLen:          uint32(len(b.raw)),
		uncompressedLen:    uint32(content),
		recordCount:        uint32(len(recs)),
		bodyCRC:            format.CRC(b.raw[format.EnvelopeLen:]),
		recordHeaderLen:    b.env.RecordHeaderLen,
		firstSeq:           recs[0].h.Seq,
		lastSeq:            recs[len(recs)-1].h.Seq,
		tsMin:              st.tsMin,
		tsMax:              st.tsMax,
		epochMin:           epochMin,
		epochMax:           epochMax,
		kindCounts:         narrowCounts(st.kindCounts),
		dirCounts:          narrowCounts(st.dirCounts),
		decodeStatusCounts: narrowCounts(st.decodeStatusCounts),
		qualityUnion:       st.qualityUnion,
		epochs:             st.epochs,
		boundaries:         st.boundaries,
		seqRanges:          st.seqRanges,
	}
}

// normalizeSummary returns a copy of s in a canonical form for comparison, as normalizeStats does for F-5:
// count arrays without trailing zeros, epochs in ascending epoch order, boundaries sorted by every field,
// every empty slice nil, and neither an epoch index nor a verbatim F-3 list.
func normalizeSummary(s *blockSummary) blockSummary {
	out := *s
	out.kindCounts = nilIfEmpty(trimZeros(s.kindCounts))
	out.dirCounts = nilIfEmpty(trimZeros(s.dirCounts))
	out.decodeStatusCounts = nilIfEmpty(trimZeros(s.decodeStatusCounts))
	out.epochs = nilIfEmpty(slices.SortedFunc(slices.Values(s.epochs), compareEpochs))
	out.boundaries = nilIfEmpty(slices.SortedFunc(slices.Values(s.boundaries), compareBoundaries))
	out.seqRanges = nilIfEmpty(slices.Clone(s.seqRanges))
	out.epochIndex, out.verbatimF3 = nil, nil

	return out
}

// requireNestedTags requires every nested entry of e, an F-3 entry of block i, to have a tag from first to last.
func requireNestedTags(t *testing.T, i int, e tlv.Entry, first, last uint16) {
	t.Helper()

	nested, err := e.Nested()
	require.NoError(t, err)
	for _, n := range nested {
		assert.True(t, n.Tag >= first && n.Tag <= last, "block %d: F-3 tag 0x%04X holds nested tag 0x%04X", i, e.Tag, n.Tag)
	}
}

// requireBuiltF3 requires list, the F-3 list a merge built for block i, which it encoded,
// to hold only the tags the tracepack format specification §10 defines, so no retired tag,
// in its entries and in their nested epoch and boundary entries;
// each count array and quality_union once, at least one epoch entry,
// and seq_range entries exactly when the block's seqs are not contiguous, which sparse reports.
func requireBuiltF3(t *testing.T, i int, list []byte, sparse bool) {
	t.Helper()

	entries, err := tlv.Decode(list)
	require.NoError(t, err)
	seen := make(map[uint16]int)
	for _, e := range entries {
		seen[e.Tag]++
		switch e.Tag {
		case f3TagKindCounts, f3TagDirCounts, f3TagDecodeStatusCounts, f3TagQualityUnion, f3TagSeqRange:
		case f3TagEpoch:
			requireNestedTags(t, i, e, epochTagEpoch, epochTagCloseSeq)
		case f3TagBoundary:
			requireNestedTags(t, i, e, boundaryTagSeq, boundaryTagGapEnd)
		default:
			assert.Fail(t, "a tag a merge does not build", "block %d: F-3 tag 0x%04X", i, e.Tag)
		}
	}
	for _, tag := range []uint16{f3TagKindCounts, f3TagDirCounts, f3TagDecodeStatusCounts, f3TagQualityUnion} {
		assert.Equal(t, 1, seen[tag], "block %d: F-3 tag 0x%04X once", i, tag)
	}
	assert.Positive(t, seen[f3TagEpoch], "block %d: an epoch entry", i)
	assert.Equal(t, sparse, seen[f3TagSeqRange] > 0, "block %d: seq_range entries iff its seqs are not contiguous", i)
}

// requireDerivedFooter requires the footer and trailer of out,
// a finalized pack whose blocks walkBlocks returned with its trailer tr,
// to be those the tracepack format specification §10 and §11 derive from the blocks,
// recomputed from their records without the Writer's summaries, its F-3 lists or its aggregation:
// a layout 1 prologue without F-4, as a merger writes it;
// each block's F-2 entry, compared with F-2 as stored, its reserved bytes zero;
// each block's F-3 summary, compared with what parsing the footer gives, its known tags only;
// F-5, compared with what parsing it gives;
// and the trailer's block count, record count and last seq.
// The F-3 and F-5 values are compared after the reader's F-3 and F-5 parser has typed them.
// It also requires the F-3 section to hold the blocks' lists end to end, in block order, and nothing else:
// that is this Writer's layout, not a rule of §10, which allows lists that overlap,
// but it is the one check of summary_offset and summary_len that does not trust them.
// A block whose bytes and F-3 list equal those of a block of inputs, the pack files merged into out, was copied verbatim,
// its list byte for byte, retired and unknown tags included, as the input stored it (packBlocks);
// every other block's list was built by the merge (requireBuiltF3).
// It returns, per block, whether the block was copied verbatim.
func requireDerivedFooter(t *testing.T, inputs [][]byte, out []byte, blocks []walkedBlock, tr format.Trailer) []bool {
	t.Helper()

	_, decoded, _ := splitPack(t, out)
	parts := splitFooter(t, decoded)
	pro := parts.pro
	assert.Equal(t, format.FooterLayoutVersion, pro.FooterLayoutVersion)
	assert.Equal(t, format.FooterFlagF3Present, pro.Flags, "F-3 present, F-4 absent")
	assert.Equal(t, uint32(format.F2EntryLen), pro.F2EntryLen)
	assert.Zero(t, pro.ExtractionVersion)
	assert.Zero(t, pro.F4Len)
	assert.Equal(t, pro.F5Offset, pro.F4Offset)
	require.Len(t, parts.entries, len(blocks), "an F-2 entry per block")

	copies, _ := packBlockSet(t, inputs)
	parsed := mustOpen(t, out, ReaderOptions{}).footer.blocks
	require.Len(t, parsed, len(blocks))
	verbatim := make([]bool, len(blocks))
	var records []recordFacts
	var content, f3End uint64
	for i := range blocks {
		b := &blocks[i]
		want := derivedSummary(b)
		e := parts.entries[i]
		assert.Equal(t, format.F2Entry{
			Offset: want.offset, OnDiskLen: want.onDiskLen, UncompressedLen: want.uncompressedLen, RecordCount: want.recordCount,
			BodyCRC: want.bodyCRC, FirstSeq: want.firstSeq, LastSeq: want.lastSeq, TSMin: want.tsMin, TSMax: want.tsMax,
			EpochMin: want.epochMin, EpochMax: want.epochMax, SummaryOffset: e.SummaryOffset, SummaryLen: e.SummaryLen,
			RecordHeaderLen: want.recordHeaderLen,
		}, e, "block %d: F-2", i)
		// The two bytes behind record_header_len, at offset 78 of the entry, are reserved (I-9).
		at := pro.F2Offset + uint64(i)*uint64(pro.F2EntryLen) + 78
		assert.Equal(t, []byte{0, 0}, decoded[at:at+2], "block %d: F-2 reserved bytes", i)
		assert.Equal(t, normalizeSummary(&want), normalizeSummary(&parsed[i]), "block %d: F-3", i)
		assert.Equal(t, f3End, e.SummaryOffset, "block %d: in this Writer's layout, its F-3 list follows the previous one", i)
		f3End += uint64(e.SummaryLen)

		list := parts.f3Raw[i]
		_, verbatim[i] = copies[packBlock{bytes: string(b.raw), f3: string(list)}]
		if !verbatim[i] {
			requireBuiltF3(t, i, list, len(want.seqRanges) > 1)
		}
		records = append(records, b.records...)
		content += uint64(want.uncompressedLen)
	}
	assert.Equal(t, pro.F3Len, f3End, "in this Writer's layout, F-3 holds the blocks' lists end to end and nothing else")

	want := derivedStats(records, content)
	assert.Equal(t, normalizeStats(want), normalizeStats(footerStats(t, decoded)), "F-5")
	assert.Equal(t, uint32(len(blocks)), tr.BlockCount)
	assert.Equal(t, want.recordCount, tr.RecordCount)
	if n := len(records); n > 0 {
		assert.Equal(t, records[n-1].h.Seq, tr.LastSeq)
	}

	return verbatim
}

// requireMergeProperties requires out, the archive a merge of the pack files inputs wrote with opts and reported as rep,
// to have the properties of every successful merge:
//   - it verifies finalized-consistent;
//   - its records, read by walking its blocks, are the inputs' records, each seq once, in ascending seq, byte for byte;
//   - its footer and trailer are those its records give (requireDerivedFooter);
//   - every block it holds that is not an input block copied verbatim holds one record or stays within the threshold;
//     with coalescing on and no coalescing fallback,
//     two neighbouring blocks of one record_header_len exceed the threshold together,
//     since the greedy grouping of the tracepack storage specification §4 Merge would have coalesced them otherwise;
//   - rep counts what out holds, no conflict, and counts that agree (requireCountsAgree);
//     without coalescing, the blocks written are the blocks copied and those the resolution encoded.
func requireMergeProperties(t *testing.T, inputs [][]byte, out []byte, rep MergeReport, opts MergeOptions) {
	t.Helper()

	requireConsistent(t, out)
	blocks, tr := walkBlocks(t, out)

	records := make([][]rawRecord, len(inputs))
	for i, f := range inputs {
		records[i] = rawRecordsOf(t, f)
	}
	got := recordsOfBlocks(blocks)
	require.Equal(t, mergedRecords(t, slices.Concat(records...)), got, "the inputs' records, each once, in ascending seq")

	verbatim := requireDerivedFooter(t, inputs, out, blocks, tr)

	threshold := uint64(cmp.Or(opts.BlockThreshold, DefaultBlockThreshold))
	greedy := !opts.NoCoalesce && rep.CoalesceFallbacks == 0
	for i := range blocks {
		env := &blocks[i].env
		size := uint64(env.UncompressedLen)
		if !verbatim[i] {
			assert.True(t, env.RecordCount == 1 || size <= threshold,
				"block %d, which the merge encoded, holds %d records of %d bytes, above the threshold %d", i, env.RecordCount, size, threshold)
		}
		if i == 0 || !greedy {
			continue
		}
		if prev := &blocks[i-1].env; prev.RecordHeaderLen == env.RecordHeaderLen {
			assert.Greater(t, uint64(prev.UncompressedLen)+size, threshold, "blocks %d and %d fit the threshold %d together", i-1, i, threshold)
		}
	}

	assert.Equal(t, uint64(len(out)), rep.Size)
	assert.Equal(t, len(inputs), rep.Inputs)
	assert.Equal(t, len(blocks), rep.Blocks)
	assert.Equal(t, uint64(len(got)), rep.Records)
	assert.Zero(t, rep.Conflicts)
	assert.True(t, rep.ConflictsComplete)
	requireCountsAgree(t, inputs, out, rep)
	if opts.NoCoalesce {
		assert.Zero(t, rep.Coalesced)
		assert.Zero(t, rep.CoalescedEncodings)
		assert.Zero(t, rep.CoalesceFallbacks)
		assert.Equal(t, rep.Copied+rep.ResolvedEncodings, rep.Blocks)
	}
}

// failingEveryOther returns a hook for the on-disk body of new blocks, as mergePlan.coalescedHook takes it,
// that counts its calls in calls and drops the last byte of the body at every odd call,
// so that encoding fails its check and its group is written as its blocks.
func failingEveryOther(calls *int) func(enc []byte) []byte {
	return func(enc []byte) []byte {
		*calls++
		if *calls%2 == 1 {
			return enc[:len(enc)-1]
		}

		return enc
	}
}

// mergeChecked merges files, the packs v.Packs names in that order, with fixedIDs(opts),
// every other coalesced encoding made to fail its check when fallbacks is set;
// it requires the merge to succeed, which writeWithHooks requires to have the merge properties,
// and returns the archive and the report.
func mergeChecked(t *testing.T, v View, files [][]byte, opts MergeOptions, fallbacks bool) ([]byte, MergeReport) {
	t.Helper()

	var calls int
	out, rep, err := writeWithHooks(t, v, inputsOfFiles(files), opts, func(p *mergePlan) {
		if fallbacks {
			p.coalescedHook = failingEveryOther(&calls)
		}
	})
	require.NoError(t, err)

	return out, rep
}

// inputsOfFiles returns files as merge inputs, in their order.
func inputsOfFiles(files [][]byte) []MergeInput {
	inputs := make([]MergeInput, len(files))
	for i, f := range files {
		inputs[i] = MergeInput{ReaderAt: bytes.NewReader(f), Size: int64(len(f))}
	}

	return inputs
}

// inBlocks returns the steps writing recs in blocks of n records, the last block holding the rest.
func inBlocks(recs []Record, n int) []footerTestStep {
	steps := make([]footerTestStep, len(recs))
	for i := range recs {
		steps[i] = footerTestStep{rec: recs[i], flush: (i+1)%n == 0}
	}

	return steps
}

// cutAt returns steps with a block closed before the first record of seq from or above,
// so that no block holds records on both sides of from.
func cutAt(steps []footerTestStep, from uint64) []footerTestStep {
	out := slices.Clone(steps)
	for i := 1; i < len(out); i++ {
		if out[i-1].rec.Seq < from && out[i].rec.Seq >= from {
			out[i-1].flush = true
		}
	}

	return out
}

// seqExtra returns the extension area widenedFrom gives the record header of seq:
// wideExtraLen bytes that depend on the seq alone, so the record is the same bytes in every pack holding it.
func seqExtra(seq uint64) []byte {
	return []byte{0xE1, 0xA5, 2, 3, byte(seq >> 24), byte(seq >> 16), byte(seq >> 8), byte(seq)}
}

// widenedFrom returns file, a finalized pack none of whose blocks holds records on both sides of from,
// with every record header of the blocks of seqs from and above given seqExtra as its extension area.
func widenedFrom(t testing.TB, file []byte, from uint64) []byte {
	t.Helper()

	return rebuildPack(t, file, func(_ int, b *testBlock) {
		if b.headers[0].Seq < from {
			return
		}
		for j := range b.headers {
			b.headers[j].Extra = seqExtra(b.headers[j].Seq)
		}
	})
}

// otherCodec returns file, a finalized pack, with every block encoded with the codec it does not use,
// so the pack holds the same records in other bytes.
func otherCodec(t testing.TB, file []byte) []byte {
	t.Helper()

	return rebuildPack(t, file, func(_ int, b *testBlock) {
		if b.codec == uint8(CodecNone) {
			b.codec = uint8(CodecZstd)
		} else {
			b.codec = uint8(CodecNone)
		}
	})
}

// noteRecord returns an annotation record of epoch carrying a note.
func noteRecord(t testing.TB, epoch uint32) Record {
	t.Helper()

	note, err := (&Annotation{AnnotationKind: AnnotationKindNote, Text: new("note")}).MarshalBinary()
	require.NoError(t, err)

	return Record{Epoch: epoch, Kind: KindAnnotation, Dir: DirLocal, DecodeStatus: DecodeStatusNotApplicable, Payload: note}
}

// boundaryRecord returns a capture-boundary record of epoch 0 of kind, absent when nil, with the gap bounds given.
func boundaryRecord(t testing.TB, kind *BoundaryKind, gapStart, gapEnd *int64) Record {
	t.Helper()

	return testEventRecord(t, 0, 0, 0, &TransportEvent{Event: EventCaptureBoundary, BoundaryKind: kind, GapStart: gapStart, GapEnd: gapEnd})
}

// propertyCapture returns the records of the property vectors,
// a capture in the merge test hour, in ascending seq from 100:
//
//	epoch 1: a start boundary of epoch 0, data and a control record, closed by a socket-close event
//	epoch 2: data, a record of kind 200, a gap boundary with gap_start and gap_end, closed by a socket-close event
//	epoch 3, three seqs later: data of dir 200, of fidelity 200 and of decode_status 200, an annotation,
//	  a gap boundary with gap_start only, data of epoch 0, boundaries of kind 9 and without a kind,
//	  closed by a socket-close event
//	epoch 4: data at every other seq, a stop-unclean boundary without bounds, closed by a socket-close event
//	then a clean stop of epoch 0, which closes epoch 0.
//
// Payloads range from the bare frame to a few hundred bytes,
// and times are spread over the hour out of seq order, so the first record of a block is seldom its earliest.
func propertyCapture(t testing.TB) []Record {
	t.Helper()

	var out []Record
	seq := uint64(100)
	add := func(r Record) {
		r.Seq, r.TSUTCNs = seq, blockTestHour+int64(seq*7919%3593)*1_000_000_000
		out = append(out, r)
		seq++
	}
	data := func(epoch uint32, n int, edit func(r *Record)) Record {
		r := bigDataRecord(0, 0, n)
		r.Epoch = epoch
		if edit != nil {
			edit(&r)
		}

		return r
	}
	closing := func(epoch uint32) Record {
		return testEventRecord(t, 0, 0, epoch, &TransportEvent{Event: EventSocketClose})
	}

	add(boundaryRecord(t, new(BoundaryKindStart), nil, nil))
	for i := range 4 {
		add(data(1, 40*i, nil))
	}
	add(controlRecord(0, 0, 1, DirEquipmentToHost))
	add(closing(1))

	add(data(2, 0, nil))
	add(data(2, 150, func(r *Record) { r.Quality = QualityOrderingUncertain }))
	add(data(2, 10, func(r *Record) { r.Kind = Kind(compatUnknown) }))
	add(boundaryRecord(t, new(BoundaryKindGap), new(blockTestHour+100), new(blockTestHour+200)))
	add(data(2, 250, nil))
	add(closing(2))

	seq += 3
	add(data(3, 20, func(r *Record) { r.Dir = Dir(compatUnknown) }))
	add(data(3, 20, func(r *Record) { r.Fidelity = Fidelity(compatUnknown) }))
	add(data(3, 20, func(r *Record) { r.DecodeStatus = DecodeStatus(compatUnknown) }))
	add(noteRecord(t, 3))
	add(boundaryRecord(t, new(BoundaryKindGap), new(blockTestHour+300), nil))
	add(data(0, 30, nil))
	add(boundaryRecord(t, new(BoundaryKind(9)), nil, nil))
	add(boundaryRecord(t, nil, nil, nil))
	add(data(3, 60, func(r *Record) { r.Quality = QualityDirectionInferred }))
	add(closing(3))

	for i := range 5 {
		add(data(4, 10*i, nil))
		seq++
	}
	add(boundaryRecord(t, new(BoundaryKindStopUnclean), nil, nil))
	add(data(4, 5, nil))
	add(closing(4))
	add(boundaryRecord(t, new(BoundaryKindStop), nil, nil))

	return out
}

// requireRichArchive requires out, an archive of every record of propertyCapture,
// to show in its F-5 statistics what the property vectors exercise:
// five epochs, each with its close_seq; seven boundaries; seq gaps; and one record of each enum value 200.
func requireRichArchive(t *testing.T, out []byte) {
	t.Helper()

	st := footerStats(t, footerOf(t, out))
	require.Len(t, st.epochs, 5)
	for _, e := range st.epochs {
		assert.True(t, e.hasCloseSeq, "epoch %d closed", e.epoch)
	}
	assert.Len(t, st.boundaries, 7)
	assert.Greater(t, len(st.seqRanges), 2)
	for _, counts := range [][]uint64{st.kindCounts, st.dirCounts, st.decodeStatusCounts} {
		require.Greater(t, len(counts), compatUnknown)
		assert.Equal(t, uint64(1), counts[compatUnknown])
	}
}

// propertyVariant is one set of merge options the property vectors are merged with.
type propertyVariant struct {
	name       string
	codec      Codec
	noCoalesce bool
	threshold  int
	fallbacks  bool
	// counted marks the variant whose report a vector's own check reads.
	counted bool
}

// propertyVariants returns the options the property vectors are merged with:
// no coalescing; coalescing with a threshold small enough to make groups form and split;
// the default threshold, under which neighbouring blocks of one record_header_len coalesce into one;
// and the small threshold with every other coalesced encoding failing.
func propertyVariants() []propertyVariant {
	return []propertyVariant{
		{name: "no coalescing", codec: CodecNone, noCoalesce: true, threshold: 300},
		{name: "threshold 300", codec: CodecZstd, threshold: 300, counted: true},
		{name: "default threshold", codec: CodecNone},
		{name: "threshold 300 with fallbacks", codec: CodecZstd, threshold: 300, fallbacks: true},
	}
}

// TestMergeProperties merges the property vectors, each under every propertyVariants option set,
// and requires every archive to have the merge properties (requireMergeProperties):
// its records the inputs' records, and its F-2, F-3 and F-5 those an independent recomputation from its records gives.
// The vectors hold the records of propertyCapture,
// so the archive's footer carries epoch closures, every kind of boundary, seq gaps and enum values outside the registry,
// in blocks copied verbatim, resolved and coalesced, of two record_header_len;
// one vector holds no record, so the archive has no block.
func TestMergeProperties(t *testing.T) {
	t.Parallel()

	c := propertyCapture(t)
	n := len(c)
	third, mid := n/3, c[n/2].Seq
	// bus is the segment of consumer k of three consumers of a durable bus taking turns of two records.
	bus := func(k int) []byte {
		var recs []Record
		for i := range c {
			if i/2%3 == k {
				recs = append(recs, c[i])
			}
		}

		return mergePack(t, UUID{0x60, byte(k)}, nil, inBlocks(recs, 2))
	}
	whole := mergePack(t, seg0, nil, inBlocks(c, 4))
	empty := func(id UUID, seq uint64) []byte {
		return mergePack(t, id, func(m *PackMeta) { m.SeqStart = seq }, nil)
	}

	tests := []struct {
		name    string
		files   [][]byte
		commits CommitSet
		opts    MergeOptions
		// empty reports a vector without records, and want checks the report of the counted variant.
		empty bool
		want  func(t *testing.T, rep MergeReport)
	}{
		{name: "one segment", files: [][]byte{mergePack(t, seg0, nil, inBlocks(c, 3))}},
		{name: "overlapping segments", files: [][]byte{
			mergePack(t, seg0, nil, inBlocks(c[:2*n/3], 2)),
			mergePackCodec(t, CodecNone, seg1, inBlocks(c[third:], 3)),
		}},
		{
			name: "copied, resolved and coalesced blocks",
			files: [][]byte{
				// seg0's blocks of eight records reach the threshold, so each is copied verbatim with its unusual F-3 list.
				unusualF3File(t, mergePack(t, seg0, nil, inBlocks(c[:third], 8))),
				mergePack(t, seg1, nil, inBlocks(c[third:2*third], 2)),
				mergePackCodec(t, CodecNone, seg2, inBlocks(c[third+2:2*third+2], 3)),
				mergePack(t, seg3, nil, inBlocks(c[2*third+2:], 1)),
			},
			want: func(t *testing.T, rep MergeReport) {
				assert.Positive(t, rep.Copied)
				assert.Positive(t, rep.ResolvedEncodings)
				assert.Positive(t, rep.CoalescedEncodings)
			},
		},
		{name: "interleaved bus segments", files: [][]byte{bus(0), bus(1), bus(2)}},
		{name: "duplicates", files: [][]byte{
			whole,
			withIDs(t, whole, seg1, viewCapture),
			withIDs(t, otherCodec(t, whole), seg2, viewCapture),
		}},
		{name: "header extensions", files: [][]byte{
			widenedFrom(t, mergePack(t, seg0, nil, cutAt(inBlocks(c[:2*n/3], 3), mid)), mid),
			widenedFrom(t, mergePackCodec(t, CodecNone, seg1, cutAt(inBlocks(c[third:], 2), mid)), mid),
		}},
		{
			name: "a generation and late segments",
			files: [][]byte{
				mergePack(t, memA, func(m *PackMeta) {
					generationMeta(m, setA, 2, 3, []UUID{{0xC1}})
					m.CompactionLevel = 1
				}, inBlocks(c[:2*n/3], 5)),
				mergePack(t, seg0, nil, inBlocks(c[n/2:], 2)),
			},
			commits: commitSet(setA),
			opts:    generationOpts(),
		},
		{name: "no records", files: [][]byte{empty(seg0, 5), empty(seg1, 9)}, empty: true},
	}
	for _, tt := range tests {
		for _, o := range propertyVariants() {
			t.Run(tt.name+"/"+o.name, func(t *testing.T) {
				t.Parallel()

				v := mergeView(t, tt.commits, tt.files...)
				opts := tt.opts
				if opts.Writer == "" {
					opts = mergeOpts()
				}
				opts.Codec, opts.NoCoalesce, opts.BlockThreshold = o.codec, o.noCoalesce, o.threshold
				out, rep := mergeChecked(t, v, filesOf(t, v, tt.files...), opts, o.fallbacks)
				if tt.empty {
					assert.Zero(t, rep.Blocks)
					return
				}
				requireRichArchive(t, out)
				if o.counted && tt.want != nil {
					tt.want(t, rep)
				}
			})
		}
	}
}

// genCapture returns n records of a generated capture in the merge test hour, in ascending seq from a random start,
// drawn from rng:
// data records of random payload lengths, and now and then a control record, an annotation,
// a data record of epoch 0, with quality bits, or with the value 200 in kind, dir, fidelity or decode_status,
// a gap boundary, a stop-unclean boundary, or a socket-close event that ends its epoch;
// a start boundary first and a clean stop last, both of epoch 0; and seq gaps of one or two seqs here and there.
// Times are random within the hour.
// Each epoch has at most one record that closes it, which keeps the epoch's close_seq unambiguous.
func genCapture(t testing.TB, rng *rand.Rand, n int) []Record {
	t.Helper()

	seq, epoch := rng.Uint64N(1000), uint32(1)
	out := make([]Record, 0, n)
	add := func(r Record) {
		r.Seq, r.TSUTCNs = seq, blockTestHour+rng.Int64N(hourNs)
		out = append(out, r)
		seq += 1 + uint64(max(0, rng.IntN(12)-9))
	}
	data := func() Record {
		r := bigDataRecord(0, 0, rng.IntN(240))
		r.Epoch = epoch
		return r
	}

	add(boundaryRecord(t, new(BoundaryKindStart), nil, nil))
	for len(out) < n-1 {
		switch rng.IntN(24) {
		case 0:
			add(testEventRecord(t, 0, 0, epoch, &TransportEvent{Event: EventSocketClose}))
			epoch++
		case 1:
			gapStart := blockTestHour + rng.Int64N(hourNs-1)
			add(boundaryRecord(t, new(BoundaryKindGap), &gapStart, new(gapStart+1)))
		case 2:
			add(boundaryRecord(t, new(BoundaryKindStopUnclean), nil, nil))
		case 3:
			add(controlRecord(0, 0, epoch, DirEquipmentToHost))
		case 4:
			add(noteRecord(t, epoch))
		case 5:
			r := data()
			switch rng.IntN(4) {
			case 0:
				r.Kind = Kind(compatUnknown)
			case 1:
				r.Dir = Dir(compatUnknown)
			case 2:
				r.Fidelity = Fidelity(compatUnknown)
			default:
				r.DecodeStatus = DecodeStatus(compatUnknown)
			}
			add(r)
		case 6:
			r := data()
			r.Epoch = 0
			add(r)
		case 7:
			r := data()
			r.Quality = QualityOrderingUncertain | QualityDirectionInferred
			add(r)
		default:
			add(data())
		}
	}
	add(boundaryRecord(t, new(BoundaryKindStop), nil, nil))

	return out
}

// genPack writes recs, in ascending seq, as pack id of viewCapture with codec c and the pack metadata meta adjusts,
// in blocks closed after each record with a probability drawn from rng,
// and before the first record of seq wideFrom or above, whose blocks are then widened (widenedFrom).
func genPack(t testing.TB, rng *rand.Rand, id UUID, c Codec, meta func(m *PackMeta), recs []Record, wideFrom uint64) []byte {
	t.Helper()

	p := 0.1 + 0.5*rng.Float64()
	steps := make([]footerTestStep, len(recs))
	for i := range recs {
		steps[i] = footerTestStep{rec: recs[i], flush: rng.Float64() < p}
	}
	file := writeRepairPack(t, c, meta, false, cutAt(steps, wideFrom)).file
	if len(recs) > 0 && recs[len(recs)-1].Seq >= wideFrom {
		file = widenedFrom(t, file, wideFrom)
	}

	return withIDs(t, file, id, viewCapture)
}

// genSparse returns recs, or, half the time, recs with each record dropped with probability 1/5, keeping at least one.
func genSparse(rng *rand.Rand, recs []Record) []Record {
	if rng.IntN(2) == 0 {
		return recs
	}
	var out []Record
	for _, r := range recs {
		if rng.IntN(5) != 0 {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		out = recs[:1]
	}

	return out
}

// genScope is a generated merge scope: its packs, the commit objects of its view, the merge options,
// and whether every other coalesced encoding is made to fail its check.
type genScope struct {
	files     [][]byte
	commits   CommitSet
	opts      MergeOptions
	fallbacks bool
}

// genScopeOf returns the scope generated from seed.
// Its records are those of a capture of genCapture, every record from a random seq on widened in a third of the scopes.
// Its packs hold them as one to four random windows of the capture,
// each possibly sparse, possibly beside the whole capture,
// or as the segments of two to four consumers of a durable bus taking turns of one to three records,
// a consumer now and then also taking a record of another's turn;
// each pack is written with a random codec in blocks closed at random,
// the first one in a fifth of the scopes as the member of a generation.
// Now and then a segment is also present as an exact duplicate, or with its records in blocks of the other codec,
// and a segment without records joins the scope.
// The merge takes a random codec, coalescing on or off, and a threshold from below one record to the default;
// with coalescing, a third of the scopes have every other coalesced encoding fail.
func genScopeOf(t testing.TB, seed uint64) genScope {
	t.Helper()

	rng := rand.New(rand.NewPCG(seed, 0x6d65726765))
	codecs := [...]Codec{CodecNone, CodecZstd}
	capture := genCapture(t, rng, 20+rng.IntN(100))
	n := len(capture)
	wideFrom := uint64(math.MaxUint64)
	if rng.IntN(3) == 0 {
		wideFrom = capture[rng.IntN(n)].Seq
	}

	var parts [][]Record
	switch rng.IntN(3) {
	case 0:
		consumers, turn := 2+rng.IntN(3), 1+rng.IntN(3)
		bus := make([][]Record, consumers)
		for i, r := range capture {
			k := i / turn % consumers
			bus[k] = append(bus[k], r)
			if o := rng.IntN(consumers); rng.IntN(10) == 0 && o != k {
				bus[o] = append(bus[o], r)
			}
		}
		parts = bus
	default:
		if rng.IntN(2) == 0 {
			parts = append(parts, capture)
		}
		for range 1 + rng.IntN(4) {
			start := rng.IntN(n)
			end := min(n, start+1+rng.IntN(n/2+1))
			parts = append(parts, genSparse(rng, capture[start:end]))
		}
	}

	s := genScope{opts: mergeOpts()}
	member := rng.IntN(5) == 0
	for k, recs := range parts {
		id, meta := UUID{0x60, byte(k)}, func(*PackMeta) {}
		if k == 0 && member {
			id, meta = memA, func(m *PackMeta) {
				generationMeta(m, setA, 2, 3, []UUID{{0xC1}})
				m.CompactionLevel = 1
			}
			s.commits, s.opts = commitSet(setA), generationOpts()
		}
		s.files = append(s.files, genPack(t, rng, id, codecs[rng.IntN(2)], meta, recs, wideFrom))
	}
	segments := s.files
	if member {
		segments = segments[1:]
	}
	if len(segments) > 0 && rng.IntN(3) == 0 {
		s.files = append(s.files, withIDs(t, segments[rng.IntN(len(segments))], UUID{0x61}, viewCapture))
	}
	if len(segments) > 0 && rng.IntN(4) == 0 {
		s.files = append(s.files, withIDs(t, otherCodec(t, segments[rng.IntN(len(segments))]), UUID{0x62}, viewCapture))
	}
	if rng.IntN(8) == 0 {
		seqStart := capture[rng.IntN(n)].Seq
		s.files = append(s.files, mergePack(t, UUID{0x63}, func(m *PackMeta) { m.SeqStart = seqStart }, nil))
	}

	s.opts.Codec = codecs[rng.IntN(2)]
	s.opts.NoCoalesce = rng.IntN(3) == 0
	s.opts.BlockThreshold = [...]int{0, 50, 150, 400, 1000, 3000}[rng.IntN(6)]
	s.fallbacks = !s.opts.NoCoalesce && rng.IntN(3) == 0

	return s
}

// genScopes is the number of scopes TestMergeGeneratedScopes merges.
const genScopes = 64

// TestMergeGeneratedScopes merges genScopes scopes of genScopeOf and requires every archive to have the merge properties
// (requireMergeProperties).
// Together the scopes write blocks in every way a merge writes them,
// copied, resolved, coalesced and written after a failed coalescing, and drop duplicates,
// which it checks when every scope was merged, not when a -run pattern selects some of them.
func TestMergeGeneratedScopes(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var sum MergeReport
	merged := 0
	// A cleanup runs once every scope's subtest has finished.
	t.Cleanup(func() {
		if merged != genScopes {
			return
		}
		t.Logf("over %d scopes: %+v", genScopes, sum)
		assert.Positive(t, sum.Copied)
		assert.Positive(t, sum.Duplicates)
		assert.Positive(t, sum.Resolved)
		assert.Positive(t, sum.Coalesced)
		assert.Positive(t, sum.ResolvedEncodings)
		assert.Positive(t, sum.CoalescedEncodings)
		assert.Positive(t, sum.CoalesceFallbacks)
	})
	for seed := range uint64(genScopes) {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			t.Parallel()

			s := genScopeOf(t, seed)
			v := mergeView(t, s.commits, s.files...)
			_, rep := mergeChecked(t, v, filesOf(t, v, s.files...), s.opts, s.fallbacks)

			mu.Lock()
			defer mu.Unlock()
			merged++
			sum.Copied += rep.Copied
			sum.Duplicates += rep.Duplicates
			sum.Resolved += rep.Resolved
			sum.Coalesced += rep.Coalesced
			sum.ResolvedEncodings += rep.ResolvedEncodings
			sum.CoalescedEncodings += rep.CoalescedEncodings
			sum.CoalesceFallbacks += rep.CoalesceFallbacks
		})
	}
}

// Bits of the control byte of FuzzMerge and FuzzMergeGenerated, which chooses the merge options.
const (
	// fuzzNoCoalesce sets MergeOptions.NoCoalesce, and fuzzZstd sets MergeOptions.Codec to CodecZstd.
	fuzzNoCoalesce = 1 << 0
	fuzzZstd       = 1 << 1
	// fuzzThresholdShift places the two bits that choose MergeOptions.BlockThreshold among fuzzThresholds.
	fuzzThresholdShift = 2
	// fuzzOneOpenBlock sets MergeOptions.MaxOpenBlocks to 1, and fuzzSmallFooter MergeOptions.MaxFooterLen to 1 KiB.
	fuzzOneOpenBlock = 1 << 4
	fuzzSmallFooter  = 1 << 5
	// fuzzFallbacks makes every other coalesced encoding fail its check.
	fuzzFallbacks = 1 << 6
	// fuzzCommitted gives FuzzMerge's view a commit object for every archive and patch of its packs,
	// so an archive or a patch can be a merge input.
	fuzzCommitted = 1 << 7
)

// fuzzThresholds holds the MergeOptions.BlockThreshold values the control byte chooses from.
var fuzzThresholds = [...]int{0, 60, 250, 1000}

// fuzzMergeReader is the reader budget of FuzzMerge's packs.
var fuzzMergeReader = ReaderOptions{MaxPackMetadataLen: 1 << 20, MaxFooterLen: 1 << 20, MaxBlockLen: 1 << 16}

// fuzzOptions returns opts with the codec, coalescing, threshold, MaxOpenBlocks and MaxFooterLen
// that the control byte ctl chooses.
func fuzzOptions(opts MergeOptions, ctl uint8) MergeOptions {
	opts.Codec = CodecNone
	if ctl&fuzzZstd != 0 {
		opts.Codec = CodecZstd
	}
	opts.NoCoalesce = ctl&fuzzNoCoalesce != 0
	opts.BlockThreshold = fuzzThresholds[ctl>>fuzzThresholdShift&3]
	if ctl&fuzzOneOpenBlock != 0 {
		opts.MaxOpenBlocks = 1
	}
	if ctl&fuzzSmallFooter != 0 {
		opts.MaxFooterLen = 1 << 10
	}

	return opts
}

// fuzzMergeResult is what fuzzMergeRun gives of a merge:
// what it wrote, its report, the number of conflicts, and its error.
type fuzzMergeResult struct {
	out       []byte
	rep       MergeReport
	conflicts int
	err       error
}

// fuzzMergeRun merges files, the packs v.Packs names in that order, with opts,
// every other coalesced encoding made to fail its check when fallbacks is set.
// It requires every conflict OnConflict receives to be of v's capture, with two versions or more,
// and the conflicts to come in ascending seq, each seq once.
func fuzzMergeRun(t *testing.T, v View, files [][]byte, opts MergeOptions, fallbacks bool) fuzzMergeResult {
	t.Helper()

	var conflicts []Conflict
	opts.OnConflict = func(c Conflict) { conflicts = append(conflicts, c) }
	inputs := inputsOfFiles(files)
	var out bytes.Buffer
	var rep MergeReport
	p, err := planMerge(t.Context(), &v, inputs, &opts)
	if err == nil {
		var calls int
		if fallbacks {
			p.coalescedHook = failingEveryOther(&calls)
		}
		rep, err = p.write(t.Context(), &out, &opts)
	}

	for i, c := range conflicts {
		require.Equal(t, v.Capture, c.CaptureID)
		require.GreaterOrEqual(t, len(c.Versions), 2, "conflict of seq %d", c.Seq)
		if i > 0 {
			require.Greater(t, c.Seq, conflicts[i-1].Seq, "conflicts in ascending seq, each once")
		}
	}

	return fuzzMergeResult{out: out.Bytes(), rep: rep, conflicts: len(conflicts), err: err}
}

// requireNothingFinalized requires out, what a failed merge wrote, to hold no finalized pack.
func requireNothingFinalized(t *testing.T, out []byte) {
	t.Helper()

	if len(out) == 0 {
		return
	}
	rep, err := Verify(t.Context(), bytes.NewReader(out), int64(len(out)), VerifyOptions{})
	require.NoError(t, err)
	require.Equal(t, OutcomeUnfinalized, rep.Outcome, "a failed merge leaves no finalized pack")
}

// fuzzMergeInput is one input of FuzzMerge: two packs and the control byte choosing the merge options.
type fuzzMergeInput struct {
	a, b []byte
	ctl  uint8
}

// fuzzMergeSeeds returns the seed inputs of FuzzMerge, made of the records of propertyCapture:
// valid inputs that are copied and coalesced, resolved, conflicting, duplicates, interleaved,
// copied with unusual F-3 lists, or that exceed a budget, a block over MaxBlockLen included;
// a generation's member beside a late segment, and a patch that replaced a damaged segment, both committed;
// and inputs already damaged:
// cut short, with an invalid footer, a corrupt block, a false seq_start, or F-3 disagreeing with a block.
func fuzzMergeSeeds(t testing.TB) []fuzzMergeInput {
	t.Helper()

	c := propertyCapture(t)
	n := len(c)
	early := mergePack(t, seg0, nil, inBlocks(c[:n/3], 4))
	late := mergePack(t, seg1, nil, inBlocks(c[n/3:], 1))
	a := mergePack(t, seg0, nil, inBlocks(c[:2*n/3], 2))
	b := mergePackCodec(t, CodecNone, seg1, inBlocks(c[n/3:], 3))
	conflicting := mergePack(t, seg1, nil, changed(inBlocks(c[n/3:], 3), 1, c[n/2].Seq))
	var even, odd []Record
	for i := range c {
		if i/2%2 == 0 {
			even = append(even, c[i])
		} else {
			odd = append(odd, c[i])
		}
	}
	busA, busB := mergePack(t, seg0, nil, inBlocks(even, 2)), mergePack(t, seg1, nil, inBlocks(odd, 2))
	// big holds one record whose block decodes to more than fuzzMergeReader.MaxBlockLen.
	big := mergePack(t, seg1, nil, []footerTestStep{{rec: bigDataRecord(c[n-1].Seq+1, blockTestHour+1, 1<<16)}})
	member := mergePack(t, memA, func(m *PackMeta) {
		generationMeta(m, setA, 2, 3, []UUID{{0xC1}})
		m.CompactionLevel = 1
	}, inBlocks(c[:2*n/3], 3))
	second := mustOpen(t, a, ReaderOptions{}).Blocks()[1]
	damaged := flipByte(a, second.Offset+format.EnvelopeLen+1)
	patch, _ := mustRepair(t, damaged)
	threshold := func(i uint8) uint8 { return i << fuzzThresholdShift }

	return []fuzzMergeInput{
		{a: early, b: late},
		{a: early, b: late, ctl: fuzzNoCoalesce | fuzzZstd},
		{a: early, b: late, ctl: threshold(2) | fuzzFallbacks},
		{a: a, b: b, ctl: fuzzZstd | threshold(2)},
		{a: a, b: b, ctl: fuzzNoCoalesce | threshold(1)},
		{a: a, b: b, ctl: threshold(3) | fuzzFallbacks},
		{a: a, b: conflicting},
		{a: a, b: conflicting, ctl: fuzzNoCoalesce | threshold(1)},
		{a: a, b: withIDs(t, a, seg1, viewCapture)},
		{a: a, b: withIDs(t, otherCodec(t, a), seg1, viewCapture), ctl: threshold(2)},
		{a: busA, b: busB, ctl: threshold(1)},
		{a: unusualF3File(t, early), b: late, ctl: threshold(2)},
		{a: a, b: b, ctl: fuzzOneOpenBlock},
		{a: early, b: late, ctl: fuzzSmallFooter},
		{a: early, b: big},
		{a: member, b: late, ctl: fuzzCommitted | threshold(2)},
		{a: damaged, b: patch, ctl: fuzzCommitted},
		{a: a[:len(a)/2], b: b},
		{a: invalidFooterFile(t, a), b: b},
		{a: damaged, b: b},
		{a: withSeqStart(t, a, c[0].Seq+1), b: b},
		{a: editBlock(t, a, 1, func(tb *testBlock) { tb.headers[0].Kind = uint8(KindControl) }), b: b},
		{a: mergePack(t, seg0, func(m *PackMeta) { m.SeqStart = 7 }, nil), b: late},
		{a: a},
	}
}

// FuzzMerge merges two mutated packs with options a control byte chooses.
// Packs that do not open, or whose ActiveView fails, are left to FuzzOpen and the view tests.
// The view takes no commit object, so its packs are segments,
// unless the control byte gives one to every archive and patch, ranking the merge's claim above any generation.
// Every merge ends in one of the outcomes Merge has for such inputs:
// ErrMergeInput; ErrMergeConflict, once every conflict reached OnConflict;
// a budget error, ErrMergeLimit or ErrReadLimit;
// or an archive with the merge properties (requireMergeProperties).
// Any other error, a panic, or an archive without the properties is a failure.
// OnConflict receives each conflict once, in ascending seq;
// after an error the report holds only the conflicts, and dst holds no finalized pack.
// Every section of a pack is under a CRC, so most mutations end in ErrMergeInput;
// FuzzMergeGenerated mutates valid scopes instead.
// Its inputs are packs of several KiB:
// the fuzzing engine's default minimization of each new input can take a minute on them, stalling every worker,
// so run it with -fuzzminimizetime of a few seconds.
func FuzzMerge(f *testing.F) {
	for _, s := range fuzzMergeSeeds(f) {
		f.Add(s.a, s.b, s.ctl)
	}

	f.Fuzz(func(t *testing.T, a, b []byte, ctl uint8) {
		var files [][]byte
		var infos []PackInfo
		for _, data := range [][]byte{a, b} {
			if len(data) == 0 {
				continue
			}
			r, err := Open(t.Context(), bytes.NewReader(data), int64(len(data)), fuzzMergeReader)
			if err != nil {
				return
			}
			files, infos = append(files, data), append(infos, r.Info())
		}
		var commits CommitSet
		if ctl&fuzzCommitted != 0 {
			commits = make(CommitSet)
			for _, p := range infos {
				switch m := p.meta; {
				case m.PackRole == PackRoleArchive && m.ReplacementSetID != nil:
					commits[*m.ReplacementSetID] = struct{}{}
				case m.PackRole == PackRoleRepair:
					commits[p.packID] = struct{}{}
				default:
					// A segment, or a role outside the tiers, has no commit object.
				}
			}
		}
		v, err := ActiveView(infos, commits)
		if err != nil || len(v.Packs) == 0 {
			return
		}
		files = filesOf(t, v, files...)

		opts := fuzzOptions(MergeOptions{Reader: fuzzMergeReader, Writer: "fuzz", ScopeGeneration: 1, PublisherEpoch: 1}, ctl)
		if g := v.Generation; g != nil {
			// The claim ranks above the view's generation.
			if g.PublisherEpoch >= format.MaxU64 {
				return
			}
			opts.PublisherEpoch = g.PublisherEpoch + 1
		}
		r := fuzzMergeRun(t, v, files, opts, ctl&fuzzFallbacks != 0)
		switch err := r.err; {
		case err == nil:
			require.Zero(t, r.conflicts)
			requireMergeProperties(t, files, r.out, r.rep, opts)

			return
		case errors.Is(err, ErrMergeConflict):
			require.Positive(t, r.conflicts)
			require.Equal(t, MergeReport{Conflicts: r.conflicts, ConflictsComplete: true}, r.rep)
		case errors.Is(err, ErrMergeInput), errors.Is(err, ErrMergeLimit), errors.Is(err, ErrReadLimit):
			require.Equal(t, MergeReport{Conflicts: r.conflicts}, r.rep)
		default:
			t.Fatalf("a merge error outside the outcomes it may have: %v", err)
		}
		requireNothingFinalized(t, r.out)
	})
}

// FuzzMergeGenerated merges the scope genScopeOf generates from a seed, with options a control byte chooses,
// so that mutations explore valid scopes, which the CRCs of FuzzMerge's packs leave it few of.
// The scope's packs are valid and agree,
// so the merge writes an archive with the merge properties (requireMergeProperties),
// or, under a budget the control byte makes small, fails with ErrMergeLimit, leaving no finalized pack.
func FuzzMergeGenerated(f *testing.F) {
	threshold := func(i uint8) uint8 { return i << fuzzThresholdShift }
	for i, ctl := range []uint8{
		0, fuzzNoCoalesce | fuzzZstd, threshold(1), threshold(2) | fuzzFallbacks,
		threshold(3) | fuzzZstd | fuzzFallbacks, fuzzOneOpenBlock, fuzzSmallFooter | threshold(2), fuzzNoCoalesce | threshold(1),
	} {
		f.Add(uint64(i), ctl)
	}

	f.Fuzz(func(t *testing.T, seed uint64, ctl uint8) {
		s := genScopeOf(t, seed)
		v := mergeView(t, s.commits, s.files...)
		files := filesOf(t, v, s.files...)
		opts := fuzzOptions(s.opts, ctl)
		r := fuzzMergeRun(t, v, files, opts, ctl&fuzzFallbacks != 0)
		require.Zero(t, r.conflicts)
		if ctl&(fuzzOneOpenBlock|fuzzSmallFooter) != 0 && errors.Is(r.err, ErrMergeLimit) {
			require.Equal(t, MergeReport{}, r.rep)
			requireNothingFinalized(t, r.out)

			return
		}
		require.NoError(t, r.err)
		requireMergeProperties(t, files, r.out, r.rep, opts)
	})
}
