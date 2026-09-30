package tracepack

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/format"
	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// mustAnalyze opens file and analyzes it, keeping every block's analysis.
func mustAnalyze(t testing.TB, file []byte) (*Reader, *analysis) {
	t.Helper()

	r := mustOpen(t, file, ReaderOptions{})
	a, err := r.analyze(t.Context(), true)
	require.NoError(t, err)
	require.Len(t, a.blocks, len(r.blocks))

	return r, a
}

// envelopeCRCAt returns the envelope CRC of the block envelope at off in file.
func envelopeCRCAt(t testing.TB, file []byte, off uint64) uint32 {
	t.Helper()

	env, err := format.UnmarshalBlockEnvelope(file[off : off+format.EnvelopeLen])
	require.NoError(t, err)

	return env.EnvelopeCRC
}

func TestAnalyzeKeepsTheRecordsBounds(t *testing.T) {
	t.Parallel()

	rich := richFooterPack(t, CodecZstd)
	for name, file := range map[string][]byte{
		"indexed": rich.file,
		"walked":  invalidFooterFile(t, rich.file),
	} {
		r, a := mustAnalyze(t, file)
		assert.Equal(t, mustVerify(t, file), a.report, "%s: the report is Verify's", name)
		require.Len(t, a.blocks, len(rich.blocks), name)
		for i, b := range a.blocks {
			s := &rich.blocks[i]
			assert.False(t, b.failed, "%s block %d", name, i)
			assert.Equal(t, [4]int64{int64(s.firstSeq), int64(s.lastSeq), s.tsMin, s.tsMax},
				[4]int64{int64(b.firstSeq), int64(b.lastSeq), b.tsMin, b.tsMax}, "%s block %d", name, i)
			assert.Equal(t, envelopeCRCAt(t, file, r.blocks[i].Offset), b.envelopeCRC, "%s block %d", name, i)
		}
		assert.True(t, a.blocks[0].classified, "%s: block 0 holds data records", name)
		assert.False(t, a.blocks[4].classified, "%s: block 4 holds only the stop boundary", name)
	}
}

func TestAnalyzeTakesBoundsFromRecordsNotTheIndex(t *testing.T) {
	t.Parallel()

	const last = 39
	file, edited := indexMismatchPack(t, func(b *testBlock) { b.headers[last].Seq++ })
	r, a := mustAnalyze(t, file)
	require.NoError(t, r.Header().FooterErr)
	require.NotEqual(t, edited.headers[last].Seq, r.blocks[1].LastSeq, "the F-2 entry understates the block")
	assert.Equal(t, edited.headers[last].Seq, a.blocks[1].lastSeq)
	assert.False(t, a.blocks[1].failed, "a block that disagrees with its index is validated")
	require.NotEmpty(t, a.report.Disagreements, "the block disagrees with its F-2 entry and F-3 summary")
}

func TestAnalyzeMarksFailedBlocks(t *testing.T) {
	t.Parallel()

	rich := richFooterPack(t, CodecZstd)
	file := flipByte(rich.file, rich.blocks[2].offset+format.EnvelopeLen+1)
	_, a := mustAnalyze(t, file)
	for i, b := range a.blocks {
		assert.Equal(t, i == 2, b.failed, "block %d", i)
	}
	assert.Equal(t, OutcomeCorruptMiddle, a.report.Outcome)

	r := mustOpen(t, file, ReaderOptions{})
	v, err := r.analyze(t.Context(), false)
	require.NoError(t, err)
	assert.Nil(t, v.blocks, "Verify keeps no block analysis")
}

func TestBlockAnalysisOfFacts(t *testing.T) {
	t.Parallel()

	counts := func(pairs ...int) []uint32 {
		var c []uint32
		for k := 0; k < len(pairs); k += 2 {
			for range pairs[k+1] {
				c = countEnum(c, uint8(pairs[k]))
			}
		}

		return c
	}
	tests := []struct {
		name                            string
		s                               blockSummary
		classified, oversized, redacted bool
	}{
		{name: "not attempted and not applicable", s: blockSummary{
			decodeStatusCounts: counts(int(DecodeStatusNotAttempted), 2, int(DecodeStatusNotApplicable), 1),
		}},
		{name: "ok", s: blockSummary{decodeStatusCounts: counts(int(DecodeStatusOK), 1)}, classified: true},
		{name: "oversized", s: blockSummary{decodeStatusCounts: counts(int(DecodeStatusOversized), 1)}, classified: true, oversized: true},
		{name: "a value outside the registry", s: blockSummary{decodeStatusCounts: counts(200, 1)}, classified: true},
		{name: "redacted", s: blockSummary{qualityUnion: QualityRedacted}, redacted: true},
	}
	for _, tt := range tests {
		b := blockAnalysisOf(&format.BlockEnvelope{EnvelopeCRC: 7}, &tt.s)
		assert.Equal(t, [3]bool{tt.classified, tt.oversized, tt.redacted}, [3]bool{b.classified, b.oversized, b.redacted}, tt.name)
		assert.Equal(t, uint32(7), b.envelopeCRC, tt.name)
	}
}

// repairSteps returns the records of the repair test pack, with caller seqs from 10, all in blockTestHour,
// written by writeRepairPack as five blocks:
//
//	block 0: 10 start boundary e0, 11 data e1, 13 data e1 (seq gap)
//	block 1: 14 data e1, 15 socket-close e1, 20 gap boundary e0 with gap_start and gap_end (seq gap)
//	block 2: 21 data e2, 22 data e2
//	block 3: 23 data e2, 30 data e3 (seq gap)
//	block 4: 31 stop boundary e3
func repairSteps(t testing.TB) []footerTestStep {
	t.Helper()

	h := blockTestHour
	boundary := func(seq uint64, ts int64, epoch uint32, ev *TransportEvent) Record {
		return testEventRecord(t, seq, ts, epoch, ev)
	}

	return []footerTestStep{
		{rec: boundary(10, h+50, 0, &TransportEvent{Event: EventCaptureBoundary, BoundaryKind: new(BoundaryKindStart)})},
		{rec: testDataRecord(11, h+10, 1)},
		{rec: testDataRecord(13, h+20, 1), flush: true},
		{rec: testDataRecord(14, h+30, 1)},
		{rec: boundary(15, h+35, 1, &TransportEvent{Event: EventSocketClose})},
		{rec: boundary(20, h+300, 0, &TransportEvent{
			Event: EventCaptureBoundary, BoundaryKind: new(BoundaryKindGap), GapStart: new(h + 100), GapEnd: new(h + 200),
		}), flush: true},
		{rec: testDataRecord(21, h+400, 2)},
		{rec: testDataRecord(22, h+410, 2), flush: true},
		{rec: testDataRecord(23, h+420, 2)},
		{rec: testDataRecord(30, h+500, 3), flush: true},
		{rec: boundary(31, h+600, 3, &TransportEvent{Event: EventCaptureBoundary, BoundaryKind: new(BoundaryKindStop)})},
	}
}

// repairTestMeta returns the pack metadata of the repair test pack: a segment whose period is blockTestHour.
func repairTestMeta() *PackMeta {
	return &PackMeta{
		ToolID: "tool", Recorder: "rec", Writer: "wr", Classifiers: []string{"c"},
		PackRole: PackRoleSegment, ScopeGeneration: new(uint64(0)),
		PeriodStart: blockTestHour, PeriodEnd: blockTestHour + hourNs,
	}
}

// repairPack is a pack written for the repair tests, with the Writer's own summaries of its blocks.
type repairPack struct {
	file   []byte
	blocks []blockSummary
}

// writeRepairPack writes steps into a new pack whose metadata is repairTestMeta changed by meta, when set;
// with open it flushes the last block and never closes the Writer, so the pack is not finalized.
func writeRepairPack(t testing.TB, c Codec, meta func(*PackMeta), open bool, steps []footerTestStep) *repairPack {
	t.Helper()

	m := repairTestMeta()
	if meta != nil {
		meta(m)
	}
	if len(steps) > 0 {
		m.SeqStart = steps[0].rec.Seq
	}

	var buf bytes.Buffer
	w, err := NewWriter(&buf, WriterOptions{Meta: m, Codec: c, BlockThreshold: 1 << 16})
	require.NoError(t, err)
	for i := range steps {
		require.NoError(t, w.Append(&steps[i].rec), "step %d", i)
		if steps[i].flush {
			require.NoError(t, w.Flush())
		}
	}
	if open {
		require.NoError(t, w.Flush())
	} else {
		_, err = w.Close()
		require.NoError(t, err)
	}

	return &repairPack{file: buf.Bytes(), blocks: slices.Clone(w.blocks)}
}

// defaultRepairPack writes the repair test pack of repairSteps, finalized, with CodecZstd.
func defaultRepairPack(t testing.TB) *repairPack {
	t.Helper()

	p := writeRepairPack(t, CodecZstd, nil, false, repairSteps(t))
	require.Len(t, p.blocks, 5)

	return p
}

// bodyByteOf returns the offset of the second body byte of block i of p.
func (p *repairPack) bodyByteOf(i int) uint64 {
	return p.blocks[i].offset + format.EnvelopeLen + 1
}

// repairOpts returns the RepairOptions of the tests: a writer tag and nothing else.
func repairOpts() RepairOptions {
	return RepairOptions{Writer: "repair-test"}
}

// repairBytes repairs file with opts and returns what Repair wrote, its report and its error.
func repairBytes(t testing.TB, file []byte, opts RepairOptions) ([]byte, RepairReport, error) {
	t.Helper()

	var out bytes.Buffer
	rep, err := Repair(t.Context(), bytes.NewReader(file), int64(len(file)), &out, opts)

	return out.Bytes(), rep, err
}

// mustRepair repairs file with the default options and returns the patch and the report.
func mustRepair(t testing.TB, file []byte) ([]byte, RepairReport) {
	t.Helper()

	patch, rep, err := repairBytes(t, file, repairOpts())
	require.NoError(t, err)
	require.Equal(t, uint64(len(patch)), rep.Size)

	return patch, rep
}

// setMetaRole returns file with the pack metadata's pack_role value byte set to role, in place.
func setMetaRole(t testing.TB, file []byte, role uint8) []byte {
	t.Helper()

	return patchMeta(t, file, func(meta []byte) {
		entries, err := tlv.Decode(meta)
		require.NoError(t, err)
		for _, e := range entries {
			if e.Tag == tagPackRole {
				meta[e.Offset+tlv.HeaderLen] = role

				return
			}
		}
		t.Fatal("no pack_role entry")
	})
}

// renumber returns an edit that gives every record of a block consecutive seqs from first, its envelope included.
func renumber(first uint64) func(b *testBlock) {
	return func(b *testBlock) {
		b.env.FirstSeq = first
		for j := range b.headers {
			b.headers[j].Seq = first + uint64(j)
		}
	}
}

func TestRepairRefusals(t *testing.T) {
	t.Parallel()

	p := defaultRepairPack(t)
	damaged := flipByte(p.file, p.bodyByteOf(2))
	// Three single-record blocks at seqs 100, 101 and 102, for the seq-bound cases.
	three := writeRepairPack(t, CodecNone, nil, false, []footerTestStep{
		{rec: testDataRecord(100, blockTestHour+1, 1), flush: true},
		{rec: testDataRecord(101, blockTestHour+2, 1), flush: true},
		{rec: testDataRecord(102, blockTestHour+3, 1), flush: true},
	})
	threeEdit := func(edits map[int]func(b *testBlock), flip int) []byte {
		file := rebuildPack(t, three.file, func(i int, b *testBlock) {
			if e := edits[i]; e != nil {
				e(b)
			}
		})
		r := mustOpen(t, file, ReaderOptions{})

		return flipByte(file, r.blocks[flip].Offset+format.EnvelopeLen+1)
	}
	setStatus := func(v DecodeStatus) func(i int, b *testBlock) {
		return func(_ int, b *testBlock) {
			for j := range b.headers {
				if Kind(b.headers[j].Kind) == KindData {
					b.headers[j].DecodeStatus = uint8(v)
				}
			}
		}
	}
	// damagedWith returns a pack of repairSteps whose metadata meta adjusts, with a failed middle block.
	damagedWith := func(meta func(m *PackMeta)) []byte {
		return flipByte(writeRepairPack(t, CodecZstd, meta, false, repairSteps(t)).file, p.bodyByteOf(2))
	}
	extract := writeRepairPack(t, CodecZstd, func(m *PackMeta) {
		m.PackRole, m.ScopeGeneration, m.ExtractFilter = PackRoleExtract, nil, new("all")
	}, false, repairSteps(t)).file
	// Block 0 holds a redacted record and block 3 lies in the next hour, outside the period's.
	redactedAndOutside := rebuildPack(t, p.file, func(i int, tb *testBlock) {
		switch i {
		case 0:
			tb.headers[0].Quality |= uint16(QualityRedacted)
		case 3:
			for j := range tb.headers {
				tb.headers[j].TSUTCNs += hourNs
			}
		default:
		}
	})
	id := UUID(layoutHeader(t, p.file).PackID)
	unclassified := func(m *PackMeta) { m.Classifiers = nil }
	notAttempted := func(steps []footerTestStep) []footerTestStep {
		for i := range steps {
			if steps[i].rec.Kind == KindData {
				steps[i].rec.DecodeStatus = DecodeStatusNotAttempted
			}
		}

		return steps
	}

	// A refusal of the pack comes before an error of the options: opts, when set, replaces repairOpts.
	tests := []struct {
		name string
		file []byte
		opts *RepairOptions
		want error
		msg  string
	}{
		{name: "finalized-consistent", file: p.file, want: ErrRepairNotNeeded},
		{name: "extract", want: ErrNotRepairable, msg: "extract", file: extract},
		{name: "unknown pack role", file: setMetaRole(t, damaged, 0), want: ErrNotRepairable, msg: "pack_role unknown is"},
		{name: "retired pack role", file: setMetaRole(t, damaged, packRoleRetired), want: ErrNotRepairable, msg: "unknown(5)"},
		{name: "empty period", want: ErrNotRepairable, msg: "period", file: damagedWith(func(m *PackMeta) {
			m.PeriodEnd = m.PeriodStart
		})},
		{name: "period over two hours", want: ErrNotRepairable, msg: "period", file: damagedWith(func(m *PackMeta) {
			m.PeriodStart, m.PeriodEnd = blockTestHour+hourNs/2, blockTestHour+3*hourNs/2
		})},
		{name: "block outside the period's hour", want: ErrNotRepairable, msg: "outside the UTC hour", file: damagedWith(func(m *PackMeta) {
			m.PeriodStart, m.PeriodEnd = blockTestHour+hourNs, blockTestHour+2*hourNs
		})},
		{name: "seq-order writer defect", want: ErrNotRepairable, msg: "seq-order", file: flipByte(editBlock(t, p.file, 3, func(b *testBlock) {
			b.env.FirstSeq, b.headers[0].Seq = 22, 22
		}), p.bodyByteOf(1))},
		{name: "hour-span writer defect", want: ErrNotRepairable, msg: "hour-span", file: flipByte(editBlock(t, p.file, 3, func(b *testBlock) {
			b.headers[1].TSUTCNs = blockTestHour + hourNs
		}), p.bodyByteOf(1))},
		{name: "no seq between the neighbours", want: ErrNotRepairable, msg: "no seq between",
			file: threeEdit(map[int]func(*testBlock){1: renumber(50), 2: renumber(101)}, 1)},
		{name: "next validated seq at seq_start after a failed first block", want: ErrNotRepairable, msg: "no seq between",
			file: threeEdit(map[int]func(*testBlock){1: renumber(100)}, 0)},
		{name: "next validated seq 0 after a failed block", want: ErrNotRepairable, msg: "no seq between",
			file: threeEdit(map[int]func(*testBlock){0: renumber(0), 1: renumber(0)}, 0)},
		{name: "classified record without classifier", want: ErrNotRepairable, msg: "classifier",
			file: rebuildPack(t, writeRepairPack(t, CodecZstd, unclassified, false, notAttempted(repairSteps(t))).file, setStatus(DecodeStatusOK))},
		{name: "oversized record without max_frame_len", want: ErrNotRepairable, msg: "max_frame_len",
			file: rebuildPack(t, p.file, setStatus(DecodeStatusOversized))},
		{name: "redacted record", want: ErrNotRepairable, msg: "redacted", file: rebuildPack(t, p.file, func(_ int, b *testBlock) {
			b.headers[0].Quality |= uint16(QualityRedacted)
		})},
		{name: "a consistent pack before the pack's own id", file: p.file, opts: &RepairOptions{Writer: "w", PackID: id}, want: ErrRepairNotNeeded},
		{name: "a consistent pack before an empty writer", file: p.file, opts: &RepairOptions{}, want: ErrRepairNotNeeded},
		{name: "an extract before an empty writer", file: extract, opts: &RepairOptions{}, want: ErrNotRepairable, msg: "extract"},
		{name: "a block outside the hour before a redacted record", file: redactedAndOutside, want: ErrNotRepairable, msg: "outside the UTC hour"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			opts := repairOpts()
			if tt.opts != nil {
				opts = *tt.opts
			}
			out, rep, err := repairBytes(t, tt.file, opts)
			require.ErrorIs(t, err, tt.want)
			if tt.msg != "" {
				require.ErrorContains(t, err, tt.msg)
			}
			assert.Empty(t, out, "a refusal writes nothing")
			assert.Equal(t, mustVerify(t, tt.file), rep.Verify, "the report holds the verification")
			assert.Zero(t, rep.Blocks)
			assert.Empty(t, rep.Coverage)
		})
	}
}

func TestRepairOptionErrors(t *testing.T) {
	t.Parallel()

	p := defaultRepairPack(t)
	damaged := flipByte(p.file, p.bodyByteOf(2))
	id := UUID(layoutHeader(t, damaged).PackID)

	tests := []struct {
		name string
		opts RepairOptions
		msg  string
	}{
		{name: "no writer", opts: RepairOptions{}, msg: "Writer is empty"},
		{name: "the damaged pack's own id", opts: RepairOptions{Writer: "w", PackID: id}, msg: "own pack_id"},
		{name: "a codec outside the registry", opts: RepairOptions{Writer: "w", Codec: Codec(9)}, msg: "Codec"},
	}
	for _, tt := range tests {
		out, rep, err := repairBytes(t, damaged, tt.opts)
		require.ErrorContains(t, err, tt.msg, tt.name)
		assert.Empty(t, out, tt.name)
		assert.Equal(t, RepairReport{}, rep, tt.name)
	}

	_, err := Repair(t.Context(), bytes.NewReader(damaged[:10]), 10, io.Discard, repairOpts())
	require.ErrorIs(t, err, ErrNotTracepack, "an Open error passes through")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var out bytes.Buffer
	_, err = Repair(ctx, bytes.NewReader(damaged), int64(len(damaged)), &out, repairOpts())
	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, out.Len())
}

func TestRepairNilPackIDsAreNotTheSame(t *testing.T) {
	t.Parallel()

	p := defaultRepairPack(t)
	file := patchHeader(t, flipByte(p.file, p.bodyByteOf(2)), func(h *format.FileHeader) { h.PackID = format.UUID{} })
	patch, rep := mustRepair(t, file)
	assert.False(t, rep.PackID.IsZero(), "a zero PackID option generates the patch's id")
	assert.Equal(t, rep.PackID, UUID(layoutHeader(t, patch).PackID))
	assert.Equal(t, []UUID{{}}, mustOpen(t, patch, ReaderOptions{}).Header().Meta.Supersedes)
}

// archiveMeta makes the repair test pack a generation-2 archive of a capture-clock recorder,
// with compaction lineage, a durability interval, inherited coverage holding an unknown nested tag,
// and three private unknown entries that retagMetaEntries can turn into retired tags.
func archiveMeta(m *PackMeta) {
	m.PackRole = PackRoleArchive
	m.ScopeGeneration, m.PublisherEpoch = new(uint64(2)), new(uint64(3))
	m.ReplacementSetID, m.ReplacementSetSize, m.ReplacementSetIndex = new(UUID{0xAA}), new(uint64(1)), new(uint64(0))
	m.Supersedes = []UUID{{0xAB}}
	m.CompactionLevel, m.CompactedFrom = 1, []UUID{{0xC1}, {0xC2}}
	m.TimeSource = TimeSourceCaptureClock
	m.CaptureOriginUTCNs, m.CaptureOriginMonoNs, m.ClockStepToleranceNs = new(blockTestHour), new(int64(5)), new(uint64(1000))
	m.FlushIntervalNs = new(uint64(60_000_000_000))
	m.Coverage = []Coverage{{SeqFirst: new(uint64(1)), SeqLast: new(uint64(2)), Unknown: []RawEntry{{Tag: 0x0050, Type: 7, Value: []byte{9}}}}}
	m.Unknown = []RawEntry{
		{Tag: 0x8001, Type: 7, Value: []byte{1}},
		{Tag: 0x8002, Type: 7, Value: []byte{2}},
		{Tag: 0x8003, Type: 7, Value: []byte{3}},
		{Tag: 0x8004, Type: 7, Value: []byte("kept")},
	}
}

// retagMetaEntries returns file with the pack metadata entries tagged 0x8001 to 0x8003 retagged in place
// as the three retired pack metadata tags.
func retagMetaEntries(t testing.TB, file []byte) []byte {
	t.Helper()

	return patchMeta(t, file, func(meta []byte) {
		entries, err := tlv.Decode(meta)
		require.NoError(t, err)
		for _, e := range entries {
			if e.Tag >= 0x8001 && e.Tag <= 0x8003 {
				binary.LittleEndian.PutUint16(meta[e.Offset:], retiredPackMetaTags[e.Tag-0x8001])
			}
		}
	})
}

func TestRepairPatchMetadata(t *testing.T) {
	t.Parallel()

	src := writeRepairPack(t, CodecZstd, archiveMeta, false, repairSteps(t))
	file := retagMetaEntries(t, flipByte(src.file, src.bodyByteOf(0)))
	srcHeader := mustOpen(t, file, ReaderOptions{}).Header()
	require.Len(t, srcHeader.Meta.Unknown, 4, "the retired tags are read as unknown entries")

	base := UUID{0xBA, 5}
	opts := repairOpts()
	opts.PatchBase = &base
	patch, rep, err := repairBytes(t, file, opts)
	require.NoError(t, err)

	h := mustOpen(t, patch, ReaderOptions{}).Header()
	m := h.Meta
	assert.Equal(t, PackRoleRepair, m.PackRole)
	assert.Equal(t, new(uint64(0)), m.ScopeGeneration)
	assert.Nil(t, m.PublisherEpoch)
	assert.Nil(t, m.ReplacementSetID)
	assert.Nil(t, m.ReplacementSetSize)
	assert.Nil(t, m.ReplacementSetIndex)
	assert.Equal(t, []UUID{srcHeader.PackID}, m.Supersedes, "supersedes names only the damaged pack")
	assert.Equal(t, &base, m.PatchBase)
	assert.Equal(t, "repair-test", m.Writer)
	assert.Equal(t, uint64(14), m.SeqStart, "block 0 failed, so the patch starts at block 1's first seq")

	// Carried unchanged.
	want := srcHeader.Meta
	assert.Equal(t, want.CompactionLevel, m.CompactionLevel)
	assert.Equal(t, want.CompactedFrom, m.CompactedFrom)
	assert.Equal(t, want.TimeSource, m.TimeSource)
	assert.Equal(t, want.CaptureOriginUTCNs, m.CaptureOriginUTCNs)
	assert.Equal(t, want.CaptureOriginMonoNs, m.CaptureOriginMonoNs)
	assert.Equal(t, want.ClockStepToleranceNs, m.ClockStepToleranceNs)
	assert.Equal(t, want.FlushIntervalNs, m.FlushIntervalNs)
	assert.Equal(t, [2]int64{want.PeriodStart, want.PeriodEnd}, [2]int64{m.PeriodStart, m.PeriodEnd})
	assert.Equal(t, want.Classifiers, m.Classifiers)
	assert.Equal(t, []RawEntry{{Tag: 0x8004, Type: 7, Value: []byte("kept")}}, m.Unknown, "the retired tags are dropped")

	require.NotEmpty(t, rep.Coverage)
	assert.Equal(t, append(slices.Clone(want.Coverage), rep.Coverage...), m.Coverage, "inherited coverage first, as data")

	assert.Equal(t, srcHeader.CaptureID, h.CaptureID)
	assert.Equal(t, rep.PackID, h.PackID)
	assert.Zero(t, h.Flags, "no redaction-present flag")

	// Without a patch_base.
	patch, _ = mustRepair(t, file)
	assert.Nil(t, mustOpen(t, patch, ReaderOptions{}).Header().Meta.PatchBase)
}

// TestRepairCarriesEveryClassifier checks a pack whose metadata names two classifiers and two maximum frame lengths
// (the tracepack format specification §5): its classified records verify finalized-consistent,
// and a patch of it carries both values of each, in order (the tracepack storage specification §6).
func TestRepairCarriesEveryClassifier(t *testing.T) {
	t.Parallel()

	classifiers, maxFrameLens := []string{"go-secs/v2", "go-secs/v1"}, []uint64{1 << 20, 4096}
	p := writeRepairPack(t, CodecZstd, func(m *PackMeta) {
		m.Classifiers, m.MaxFrameLens = slices.Clone(classifiers), slices.Clone(maxFrameLens)
	}, false, repairSteps(t))
	v := mustVerify(t, p.file)
	require.Equal(t, OutcomeFinalizedConsistent, v.Outcome, "failed %v, disagreements %v", v.Failed, v.Disagreements)
	_, a := mustAnalyze(t, p.file)
	require.True(t, slices.ContainsFunc(a.blocks, func(b blockAnalysis) bool { return b.classified }), "the pack holds classified records")

	damaged := flipByte(p.file, p.bodyByteOf(2))
	patch, rep := mustRepair(t, damaged)
	m := mustOpen(t, patch, ReaderOptions{}).Header().Meta
	assert.Equal(t, classifiers, m.Classifiers)
	assert.Equal(t, maxFrameLens, m.MaxFrameLens)
	checkPatch(t, p.file, damaged, patch, rep)
}

func TestRepairKeepsANilCaptureID(t *testing.T) {
	t.Parallel()

	p := defaultRepairPack(t)
	file := patchHeader(t, flipByte(p.file, p.bodyByteOf(2)), func(h *format.FileHeader) { h.CaptureID = format.UUID{} })
	patch, rep := mustRepair(t, file)
	assert.True(t, UUID(layoutHeader(t, patch).CaptureID).IsZero())
	require.NotEmpty(t, rep.Coverage)
	assert.Equal(t, UUID{}, *rep.Coverage[0].CaptureID)
}

// scopeHour returns the first and last nanosecond of the repair test pack's scope hour.
func scopeHour() (int64, int64) {
	return hourBounds(hourOf(blockTestHour))
}

// lostEntry returns a coverage entry of capture over seqs first to last (unbounded when last is nil) and times start to end.
func lostEntry(capture UUID, first uint64, last *uint64, start, end int64) Coverage {
	return Coverage{CaptureID: new(capture), SeqFirst: new(first), SeqLast: last, TimeStart: new(start), TimeEnd: new(end)}
}

// coverageMatches reports whether c, an entry of a pack of capture, intersects both a query at seq and a query at ts,
// by the matching rule of the tracepack format specification §5.
func coverageMatches(c *Coverage, capture UUID, seq uint64, ts int64) bool {
	if c.CaptureID != nil && *c.CaptureID != capture {
		return false
	}
	// An entry whose first bound exceeds its last, on either side, intersects every query of its capture.
	if inverted(c.SeqFirst, c.SeqLast) || inverted(c.TimeStart, c.TimeEnd) {
		return true
	}

	return (c.SeqFirst == nil || seq >= *c.SeqFirst) && (c.SeqLast == nil || seq <= *c.SeqLast) &&
		(c.TimeStart == nil || ts >= *c.TimeStart) && (c.TimeEnd == nil || ts <= *c.TimeEnd)
}

func TestCoverageMatches(t *testing.T) {
	t.Parallel()

	own, other := UUID{1}, UUID{2}
	u := func(v uint64) *uint64 { return new(v) }
	i := func(v int64) *int64 { return new(v) }
	tests := []struct {
		name string
		c    Coverage
		want bool
	}{
		{name: "inside both", c: Coverage{SeqFirst: u(5), SeqLast: u(15), TimeStart: i(50), TimeEnd: i(150)}, want: true},
		{name: "seq outside", c: Coverage{SeqFirst: u(11), SeqLast: u(15), TimeStart: i(50), TimeEnd: i(150)}},
		{name: "time outside", c: Coverage{SeqFirst: u(5), SeqLast: u(15), TimeStart: i(101), TimeEnd: i(150)}},
		{name: "inverted seqs, time outside", c: Coverage{SeqFirst: u(15), SeqLast: u(5), TimeStart: i(101), TimeEnd: i(150)}, want: true},
		{name: "inverted times, seq outside", c: Coverage{SeqFirst: u(11), SeqLast: u(15), TimeStart: i(150), TimeEnd: i(50)}, want: true},
		{name: "unbounded", c: Coverage{}, want: true},
		{name: "own capture named", c: Coverage{CaptureID: &own}, want: true},
		{name: "another capture", c: Coverage{CaptureID: &other}},
		{name: "another capture, inverted", c: Coverage{CaptureID: &other, SeqFirst: u(15), SeqLast: u(5)}},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, coverageMatches(&tt.c, own, 10, 100), tt.name)
	}
}

// blockBytes returns the on-disk bytes of every located block of file that keep selects, in file order.
func blockBytes(t testing.TB, file []byte, keep func(i int) bool) [][]byte {
	t.Helper()

	r := mustOpen(t, file, ReaderOptions{})
	var out [][]byte
	for i, b := range r.Blocks() {
		if keep(i) {
			out = append(out, file[b.Offset:b.Offset+uint64(b.OnDiskLen)])
		}
	}

	return out
}

// checkPatch checks a patch that repaired damaged, whose records before the damage were baseline's:
// the patch verifies finalized-consistent; its blocks are damaged's validated blocks byte for byte, in order;
// and every record of baseline the patch lacks is matched, by its seq and by its time, by one coverage entry of the patch.
func checkPatch(t *testing.T, baseline, damaged, patch []byte, rep RepairReport) {
	t.Helper()

	v := mustVerify(t, patch)
	require.Equal(t, OutcomeFinalizedConsistent, v.Outcome, "failed %v, disagreements %v", v.Failed, v.Disagreements)
	assert.Equal(t, rep.Verify.Records, v.Records, "every validated record is copied")
	assert.Equal(t, rep.Records, v.Records)

	_, a := mustAnalyze(t, damaged)
	want := blockBytes(t, damaged, func(i int) bool { return !a.blocks[i].failed })
	assert.Equal(t, want, blockBytes(t, patch, func(int) bool { return true }), "the validated blocks, byte for byte")
	assert.Len(t, want, rep.Blocks)

	pr := mustOpen(t, patch, ReaderOptions{})
	capture := pr.Header().CaptureID
	copied := make(map[uint64]bool)
	run := iterate(t, pr, Query{})
	for _, s := range run.seqs() {
		copied[s] = true
	}
	coverage := pr.Header().Meta.Coverage
	for _, it := range iterate(t, mustOpen(t, baseline, ReaderOptions{}), Query{}).items {
		if copied[it.rec.Seq] {
			continue
		}
		assert.True(t, slices.ContainsFunc(coverage, func(c Coverage) bool { return coverageMatches(&c, capture, it.rec.Seq, it.rec.TSUTCNs) }),
			"lost record seq %d at %d has no coverage entry", it.rec.Seq, it.rec.TSUTCNs)
	}
}

// flipAll returns file with the second body byte of every block of p that flip selects inverted.
func flipAll(file []byte, p *repairPack, flip ...int) []byte {
	for _, i := range flip {
		file = flipByte(file, p.bodyByteOf(i))
	}

	return file
}

func TestRepairPatches(t *testing.T) {
	t.Parallel()

	p := defaultRepairPack(t)
	open := writeRepairPack(t, CodecZstd, nil, true, repairSteps(t))
	h0, h1 := scopeHour()
	b := p.blocks
	footerLost := func(i int, first, last uint64) Coverage {
		return lostEntry(UUID{}, first, new(last), b[i].tsMin, b[i].tsMax)
	}
	hourLost := func(first uint64, last *uint64) Coverage { return lostEntry(UUID{}, first, last, h0, h1) }
	footerAt := layoutOf(t, p.file).tr.FooterOffset
	// understated makes block 1's last record one nanosecond later than its F-2 ts_max says.
	understated := editBlock(t, p.file, 1, func(tb *testBlock) { tb.headers[len(tb.headers)-1].TSUTCNs++ })
	// stopped corrupts block 3's envelope in a pack with an invalid footer, so the forward walk stops there.
	stopped := flipByte(invalidFooterFile(t, p.file), b[3].offset+4)

	tests := []struct {
		name     string
		baseline []byte
		damaged  []byte
		want     []Coverage
	}{
		{name: "valid footer, failed middle block", baseline: p.file, damaged: flipAll(p.file, p, 2),
			want: []Coverage{footerLost(2, 21, 22)}},
		{name: "valid footer, failed block of several seq ranges", baseline: p.file, damaged: flipAll(p.file, p, 0),
			want: []Coverage{footerLost(0, 10, 11), footerLost(0, 13, 13)}},
		{name: "valid footer, failed envelope", baseline: p.file, damaged: flipByte(p.file, b[2].offset+4),
			want: []Coverage{footerLost(2, 21, 22)}},
		{name: "every block failed", baseline: p.file, damaged: flipAll(p.file, p, 0, 1, 2, 3, 4),
			want: []Coverage{
				footerLost(0, 10, 11), footerLost(0, 13, 13), footerLost(1, 14, 15), footerLost(1, 20, 20),
				footerLost(2, 21, 22), footerLost(3, 23, 23), footerLost(3, 30, 30), footerLost(4, 31, 31),
			}},
		{name: "valid footer a validated block disagrees with", baseline: understated, damaged: flipAll(understated, p, 3),
			want: []Coverage{hourLost(23, new(uint64(30)))}},
		{name: "valid footer whose F-3 a validated block disagrees with, failed block", baseline: p.file,
			damaged: flipAll(resummarized(t, p.file, p.blocks, 0, func(s *blockSummary) { s.qualityUnion |= QualityRedacted }), p, 2),
			want:    []Coverage{hourLost(21, new(uint64(22)))}},
		{name: "invalid footer, failed middle block", baseline: p.file, damaged: flipAll(invalidFooterFile(t, p.file), p, 2),
			want: []Coverage{hourLost(21, new(uint64(22)))}},
		{name: "invalid footer, failed first block", baseline: p.file, damaged: flipAll(invalidFooterFile(t, p.file), p, 0),
			want: []Coverage{hourLost(10, new(uint64(13)))}},
		{name: "invalid footer, two lost runs", baseline: p.file, damaged: flipAll(invalidFooterFile(t, p.file), p, 1, 3),
			want: []Coverage{hourLost(14, new(uint64(20))), hourLost(23, new(uint64(30)))}},
		{name: "invalid footer, the walk stops", baseline: p.file, damaged: stopped,
			want: []Coverage{hourLost(23, nil)}},
		{name: "footer CRC flipped, failed middle block", baseline: p.file, damaged: flipByte(flipAll(p.file, p, 2), footerAt+1),
			want: []Coverage{hourLost(21, new(uint64(22))), hourLost(32, nil)}},
		{name: "unfinalized, cut on a block boundary", baseline: open.file, damaged: open.file[:b[3].offset],
			want: []Coverage{hourLost(23, nil)}},
		{name: "unfinalized, cut inside a block", baseline: open.file, damaged: open.file[:b[3].offset+50],
			want: []Coverage{hourLost(23, nil)}},
		{name: "unfinalized, last block failed", baseline: open.file, damaged: flipAll(open.file, p, 4),
			want: []Coverage{hourLost(31, nil)}},
		{name: "finalized-inconsistent, F-3 disagrees", baseline: p.file,
			damaged: resummarized(t, p.file, p.blocks, 2, func(s *blockSummary) { s.qualityUnion |= QualityRedacted })},
		{name: "finalized-inconsistent, invalid footer", baseline: p.file, damaged: invalidFooterFile(t, p.file)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			require.NotEqual(t, OutcomeFinalizedConsistent, mustVerify(t, tt.damaged).Outcome)
			patch, rep := mustRepair(t, tt.damaged)
			checkPatch(t, tt.baseline, tt.damaged, patch, rep)
			// Each pack has its own capture, so the cases leave it to this loop.
			want := slices.Clone(tt.want)
			for i := range want {
				want[i].CaptureID = new(UUID(layoutHeader(t, tt.damaged).CaptureID))
			}
			assert.Equal(t, want, rep.Coverage)
			assert.Equal(t, want, mustOpen(t, patch, ReaderOptions{}).Header().Meta.Coverage, "the source had no coverage")
		})
	}
}

func TestRepairEdgePacks(t *testing.T) {
	t.Parallel()

	h0, h1 := scopeHour()
	p := defaultRepairPack(t)

	t.Run("tail after the largest seq", func(t *testing.T) {
		t.Parallel()

		top := writeRepairPack(t, CodecZstd, nil, true, []footerTestStep{
			{rec: testDataRecord(format.MaxU64-1, blockTestHour+1, 1), flush: true},
			{rec: testDataRecord(format.MaxU64, blockTestHour+2, 1), flush: true},
		})
		patch, rep := mustRepair(t, top.file)
		checkPatch(t, top.file, top.file, patch, rep)
		assert.Empty(t, rep.Coverage, "no seq can follow 2^63-1")
	})

	t.Run("repair of a repair", func(t *testing.T) {
		t.Parallel()

		first, _ := mustRepair(t, flipAll(p.file, p, 2))
		fr := mustOpen(t, first, ReaderOptions{})
		require.Equal(t, PackRoleRepair, fr.Header().Meta.PackRole)
		inherited := fr.Header().Meta.Coverage
		require.Len(t, inherited, 1)
		require.Len(t, fr.Blocks(), 4)

		damaged := flipByte(first, fr.Blocks()[2].Offset+format.EnvelopeLen+1)
		second, rep := mustRepair(t, damaged)
		checkPatch(t, first, damaged, second, rep)
		m := mustOpen(t, second, ReaderOptions{}).Header().Meta
		assert.Equal(t, []UUID{fr.Header().PackID}, m.Supersedes, "the patch names only the damaged patch")
		assert.Equal(t, append(slices.Clone(inherited), rep.Coverage...), m.Coverage)
		require.Len(t, rep.Coverage, 2, "the damaged block holds seqs 23 and 30")
		assert.Equal(t, uint64(23), *rep.Coverage[0].SeqFirst)
		assert.Equal(t, uint64(30), *rep.Coverage[1].SeqFirst)
	})

	t.Run("writer defects survive the copy", func(t *testing.T) {
		t.Parallel()

		// Block 0's record 13 claims header fields its 4-byte payload lacks;
		// block 1's socket-close record has a nonzero field_validity and a payload that is not a TLV body.
		defective := rebuildPack(t, p.file, func(i int, tb *testBlock) {
			switch i {
			case 0:
				tb.payloads[2] = blockTestFrame[:4]
				tb.headers[2].PayloadLen = 4
			case 1:
				tb.payloads[1] = []byte{0xFF}
				tb.headers[1].PayloadLen = 1
				tb.headers[1].FieldValidity = 1
			default:
			}
		})
		damaged := flipAll(defective, p, 3)
		before := mustVerify(t, damaged).WriterDefects
		require.Len(t, before, 3)

		patch, rep := mustRepair(t, damaged)
		checkPatch(t, defective, damaged, patch, rep)
		after := mustVerify(t, patch).WriterDefects
		require.Len(t, after, len(before))
		for i := range after {
			assert.Equal(t, [2]any{before[i].Kind, before[i].Seq}, [2]any{after[i].Kind, after[i].Seq})
		}
	})

	t.Run("unknown and retired F-3 tags are not carried", func(t *testing.T) {
		t.Parallel()

		_, decoded, _ := splitPack(t, p.file)
		parts := splitFooter(t, decoded)
		parts.f3[0] = append(parts.f3[0],
			tlv.Entry{Tag: 0x0004, Type: tlv.TypeU64, Value: make([]byte, 8)},
			tlv.Entry{Tag: 0x0040, Type: tlv.TypeBytes, Value: []byte{7}})
		tagged := refooter(t, p.file, parts.encode())
		require.NoError(t, mustOpen(t, tagged, ReaderOptions{}).Header().FooterErr)

		damaged := flipAll(tagged, p, 2)
		patch, rep := mustRepair(t, damaged)
		checkPatch(t, p.file, damaged, patch, rep)
		_, pd, _ := splitPack(t, patch)
		for i, list := range splitFooter(t, pd).f3 {
			for _, e := range list {
				_, known := tlv.F3[e.Tag]
				assert.True(t, known, "block %d: F-3 tag 0x%04X", i, e.Tag)
			}
		}
	})

	t.Run("records in the scope hour outside the period", func(t *testing.T) {
		t.Parallel()

		// A same-hour clock step can put records outside a segment's period; the scope is still the hour.
		late := writeRepairPack(t, CodecZstd, func(m *PackMeta) { m.PeriodStart = blockTestHour + hourNs/2 }, false, repairSteps(t))
		damaged := flipAll(late.file, late, 2)
		patch, rep := mustRepair(t, damaged)
		checkPatch(t, late.file, damaged, patch, rep)
		require.Len(t, rep.Coverage, 1)
	})

	t.Run("metadata only, unfinalized", func(t *testing.T) {
		t.Parallel()

		empty := writeRepairPack(t, CodecZstd, func(m *PackMeta) { m.SeqStart = 40 }, true, nil)
		patch, rep := mustRepair(t, empty.file)
		checkPatch(t, empty.file, empty.file, patch, rep)
		capture := UUID(layoutHeader(t, empty.file).CaptureID)
		assert.Equal(t, []Coverage{lostEntry(capture, 40, nil, h0, h1)}, rep.Coverage)
		assert.Zero(t, rep.Blocks)
		assert.Equal(t, uint64(40), mustOpen(t, patch, ReaderOptions{}).Header().Meta.SeqStart)
	})

	t.Run("empty and finalized-inconsistent, with inherited coverage", func(t *testing.T) {
		t.Parallel()

		inherited := []Coverage{{SeqFirst: new(uint64(3))}}
		empty := writeRepairPack(t, CodecZstd, func(m *PackMeta) { m.SeqStart, m.Coverage = 40, inherited }, false, nil)
		damaged := invalidFooterFile(t, empty.file)
		require.Equal(t, OutcomeFinalizedInconsistent, mustVerify(t, damaged).Outcome)

		patch, rep := mustRepair(t, damaged)
		checkPatch(t, empty.file, damaged, patch, rep)
		assert.Empty(t, rep.Coverage)
		pr := mustOpen(t, patch, ReaderOptions{})
		assert.Equal(t, inherited, pr.Header().Meta.Coverage)
		assert.Zero(t, pr.Header().Trailer.BlockCount)
	})
}

// blockReadAt serves file, and on each read of exactly the n bytes at off calls hook with the read's number, from 1,
// and the bytes served, which hook may change; an error of hook fails the read.
type blockReadAt struct {
	file  []byte
	off   int64
	n     int
	reads int
	hook  func(read int, p []byte) error
}

func (b *blockReadAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := bytes.NewReader(b.file).ReadAt(p, off)
	if err == nil && off == b.off && len(p) == b.n {
		b.reads++
		if err := b.hook(b.reads, p); err != nil {
			return 0, err
		}
	}

	return n, err
}

func TestRepairSecondReadDiffers(t *testing.T) {
	t.Parallel()

	p := defaultRepairPack(t)
	damaged := flipAll(p.file, p, 1)
	b := p.blocks[3]
	errRead := errors.New("second read failed")

	tests := []struct {
		name   string
		second func(p []byte) error
		want   error
		msg    string
	}{
		{name: "a resealed envelope", msg: "changed between", second: func(p []byte) error {
			p[35]++ // a reserved envelope byte
			binary.LittleEndian.PutUint32(p[36:], format.CRC(p[:36]))
			return nil
		}},
		{name: "a corrupt body", msg: "then failed on the copy", second: func(p []byte) error {
			p[format.EnvelopeLen+1]++
			return nil
		}},
		{name: "a failed read", want: errRead, second: func([]byte) error { return errRead }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ra := &blockReadAt{file: damaged, off: int64(b.offset), n: int(b.onDiskLen), hook: func(read int, p []byte) error {
				if read < 2 {
					return nil
				}
				return tt.second(p)
			}}
			var out bytes.Buffer
			_, err := Repair(t.Context(), ra, int64(len(damaged)), &out, repairOpts())
			require.Error(t, err)
			if tt.want != nil {
				require.ErrorIs(t, err, tt.want)
			}
			if tt.msg != "" {
				require.ErrorContains(t, err, tt.msg)
			}
			assert.Equal(t, 2, ra.reads, "the block is read by the verification and by the copy")
			assert.Equal(t, OutcomeUnfinalized, mustVerify(t, out.Bytes()).Outcome, "no trailer is written")
		})
	}
}

func TestRepairCancelledBeforeTheFooter(t *testing.T) {
	t.Parallel()

	p := defaultRepairPack(t)
	damaged := flipAll(p.file, p, 1)
	last := p.blocks[4]
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// The copy reads the last block second: cancel then, after the last block is read and before the footer.
	ra := &blockReadAt{file: damaged, off: int64(last.offset), n: int(last.onDiskLen), hook: func(read int, _ []byte) error {
		if read == 2 {
			cancel()
		}
		return nil
	}}

	var out bytes.Buffer
	_, err := Repair(ctx, ra, int64(len(damaged)), &out, repairOpts())
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, OutcomeUnfinalized, mustVerify(t, out.Bytes()).Outcome, "no trailer is written")
}

// shortAfterWriter writes into buf until limit bytes are written, then takes one byte fewer than each write without an error.
type shortAfterWriter struct {
	buf   bytes.Buffer
	limit int
}

func (s *shortAfterWriter) Write(p []byte) (int, error) {
	if s.buf.Len()+len(p) <= s.limit || len(p) == 0 {
		return s.buf.Write(p)
	}

	return s.buf.Write(p[:len(p)-1])
}

func TestRepairOutputFailures(t *testing.T) {
	t.Parallel()

	p := defaultRepairPack(t)
	damaged := flipAll(p.file, p, 1)
	good, _ := mustRepair(t, damaged)
	l := layoutOf(t, good)
	head, footerAt := int(l.blocksStart), int(l.tr.FooterOffset)

	run := func(t *testing.T, dst io.Writer, opts RepairOptions) (RepairReport, error) {
		t.Helper()

		return Repair(t.Context(), bytes.NewReader(damaged), int64(len(damaged)), dst, opts)
	}

	t.Run("header", func(t *testing.T) {
		t.Parallel()

		rep, err := run(t, &failAfterWriter{limit: 10}, repairOpts())
		require.ErrorIs(t, err, errTestWrite)
		assert.Equal(t, RepairReport{}, rep)
		rep, err = run(t, &shortAfterWriter{limit: 10}, repairOpts())
		require.ErrorIs(t, err, io.ErrShortWrite)
		assert.Equal(t, RepairReport{}, rep)
	})

	t.Run("block", func(t *testing.T) {
		t.Parallel()

		fw := &failAfterWriter{limit: head + 10}
		_, err := run(t, fw, repairOpts())
		require.ErrorIs(t, err, errTestWrite)
		assert.Equal(t, OutcomeUnfinalized, mustVerify(t, fw.buf.Bytes()).Outcome)

		sw := &shortAfterWriter{limit: head + 10}
		_, err = run(t, sw, repairOpts())
		require.ErrorIs(t, err, io.ErrShortWrite)
		assert.Equal(t, OutcomeUnfinalized, mustVerify(t, sw.buf.Bytes()).Outcome)

		var out bytes.Buffer
		opts := repairOpts()
		opts.Sync = &syncFailer{ok: 1}
		_, err = run(t, &out, opts)
		require.ErrorIs(t, err, errTestSync)
		assert.Equal(t, OutcomeUnfinalized, mustVerify(t, out.Bytes()).Outcome)
	})

	t.Run("after the trailer", func(t *testing.T) {
		t.Parallel()

		var out bytes.Buffer
		opts := repairOpts()
		opts.Sync = &syncFailer{ok: 4} // four blocks are copied; the sync after the trailer fails
		rep, err := run(t, &out, opts)
		require.ErrorIs(t, err, errTestSync)
		assert.Equal(t, RepairReport{}, rep)
		assert.Equal(t, OutcomeFinalizedConsistent, mustVerify(t, out.Bytes()).Outcome, "the bytes may form a finalized pack")

		fw := &failAfterWriter{limit: footerAt, takeAll: true}
		_, err = run(t, fw, repairOpts())
		require.ErrorIs(t, err, errTestWrite)
		assert.Equal(t, OutcomeFinalizedConsistent, mustVerify(t, fw.buf.Bytes()).Outcome, "the bytes may form a finalized pack")
	})
}

func TestHourBounds(t *testing.T) {
	t.Parallel()

	first, last := hourBounds(hourOf(blockTestHour))
	assert.Equal(t, [2]int64{blockTestHour, blockTestHour + hourNs - 1}, [2]int64{first, last})

	first, last = hourBounds(hourOf(math.MinInt64))
	assert.Equal(t, int64(math.MinInt64), first, "clamped")
	assert.Equal(t, hourOf(math.MinInt64), hourOf(last))
	assert.NotEqual(t, hourOf(math.MinInt64), hourOf(last+1))

	first, last = hourBounds(hourOf(math.MaxInt64))
	assert.Equal(t, int64(math.MaxInt64), last, "clamped")
	assert.Equal(t, hourOf(math.MaxInt64), hourOf(first))
	assert.NotEqual(t, hourOf(math.MaxInt64), hourOf(first-1))
}

func TestRunCoverage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		prev    uint64
		hasPrev bool
		next    uint64
		hasNext bool
		first   uint64
		last    *uint64
		ok      bool
	}{
		{name: "between neighbours", prev: 9, hasPrev: true, next: 20, hasNext: true, first: 10, last: new(uint64(19)), ok: true},
		{name: "from seq_start", next: 20, hasNext: true, first: 5, last: new(uint64(19)), ok: true},
		{name: "unbounded above", prev: 9, hasPrev: true, first: 10, ok: true},
		{name: "adjacent neighbours", prev: 9, hasPrev: true, next: 10, hasNext: true},
		{name: "after the largest seq", prev: format.MaxU64, hasPrev: true},
		{name: "before seq 0", next: 0, hasNext: true},
		{name: "next below seq_start", next: 5, hasNext: true},
	}
	for _, tt := range tests {
		c, ok := runCoverage(tt.prev, tt.hasPrev, tt.next, tt.hasNext, 5)
		require.Equal(t, tt.ok, ok, tt.name)
		if ok {
			assert.Equal(t, Coverage{SeqFirst: new(tt.first), SeqLast: tt.last}, c, tt.name)
		}
	}
}

// budgetSteps returns n data records of one epoch at seqs 0 to n−1 in blockTestHour, flushed every 10 records.
func budgetSteps(n int) []footerTestStep {
	steps := make([]footerTestStep, n)
	for i := range steps {
		steps[i] = footerTestStep{rec: testDataRecord(uint64(i), blockTestHour+int64(i), 1), flush: i%10 == 9}
	}

	return steps
}

func TestRepairBudgets(t *testing.T) {
	t.Parallel()

	closed := writeRepairPack(t, CodecZstd, nil, false, budgetSteps(40))
	open := writeRepairPack(t, CodecZstd, nil, true, budgetSteps(40))
	require.Len(t, closed.blocks, 4)
	l := layoutOf(t, closed.file)
	require.Greater(t, l.tr.FooterUncompressedLen, l.tr.FooterLen, "the footer must compress")
	var maxBody int64
	var decodedLarger bool
	for _, b := range closed.blocks {
		maxBody = max(maxBody, int64(b.onDiskLen)-format.EnvelopeLen)
	}
	for _, b := range closed.blocks {
		decodedLarger = decodedLarger || int64(b.uncompressedLen) > maxBody
	}
	require.True(t, decodedLarger, "a block must decode past every block's on-disk length")

	for _, tt := range []struct {
		name string
		file []byte
		opts ReaderOptions
	}{
		{name: "pack metadata over MaxPackMetadataLen", file: closed.file, opts: ReaderOptions{MaxPackMetadataLen: 8}},
		{name: "block over MaxBlockLen on disk", file: closed.file, opts: ReaderOptions{MaxBlockLen: 16}},
		{name: "block over MaxBlockLen decoded only", file: closed.file, opts: ReaderOptions{MaxBlockLen: maxBody}},
		{name: "footer over MaxFooterLen on disk", file: closed.file, opts: ReaderOptions{MaxFooterLen: int64(l.tr.FooterLen) - 1}},
		{name: "footer over MaxFooterLen decoded only", file: closed.file, opts: ReaderOptions{MaxFooterLen: int64(l.tr.FooterLen)}},
		{name: "walk past MaxWalkedBlocks", file: open.file, opts: ReaderOptions{MaxWalkedBlocks: 3}},
	} {
		opts := repairOpts()
		opts.Reader = tt.opts
		out, rep, err := repairBytes(t, tt.file, opts)
		require.ErrorIs(t, err, ErrReadLimit, tt.name)
		assert.Equal(t, RepairReport{}, rep, "%s: a budget is an error, never lost records", tt.name)
		assert.Empty(t, out, tt.name)
	}

	capture := UUID(layoutHeader(t, open.file).CaptureID)
	closedCapture := UUID(layoutHeader(t, closed.file).CaptureID)
	h0, h1 := scopeHour()
	for _, tt := range []struct {
		name     string
		baseline []byte
		file     []byte
		opts     ReaderOptions
		want     []Coverage
	}{
		{name: "a walk of exactly MaxWalkedBlocks blocks", baseline: open.file, file: open.file, opts: ReaderOptions{MaxWalkedBlocks: 4},
			want: []Coverage{lostEntry(capture, 40, nil, h0, h1)}},
		{name: "a valid footer is not walked", baseline: closed.file, file: flipByte(closed.file, closed.blocks[1].offset+format.EnvelopeLen+1),
			opts: ReaderOptions{MaxWalkedBlocks: 1},
			want: []Coverage{lostEntry(closedCapture, 10, new(uint64(19)), closed.blocks[1].tsMin, closed.blocks[1].tsMax)}},
		{name: "a footer over MaxFooterLen whose CRC fails", baseline: closed.file, file: flipByte(closed.file, l.tr.FooterOffset+1),
			opts: ReaderOptions{MaxFooterLen: 1}, want: []Coverage{lostEntry(closedCapture, 40, nil, h0, h1)}},
	} {
		opts := repairOpts()
		opts.Reader = tt.opts
		patch, rep, err := repairBytes(t, tt.file, opts)
		require.NoError(t, err, tt.name)
		checkPatch(t, tt.baseline, tt.file, patch, rep)
		assert.Equal(t, tt.want, rep.Coverage, tt.name)
	}
}

func TestRepairCancelledAfterVerification(t *testing.T) {
	t.Parallel()

	p := defaultRepairPack(t)
	damaged := flipAll(p.file, p, 1)
	last := p.blocks[4]
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// The verification reads the last block first: cancel then, so the verification completes and nothing is written.
	ra := &blockReadAt{file: damaged, off: int64(last.offset), n: int(last.onDiskLen), hook: func(read int, _ []byte) error {
		if read == 1 {
			cancel()
		}
		return nil
	}}

	var out bytes.Buffer
	rep, err := Repair(ctx, ra, int64(len(damaged)), &out, repairOpts())
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, RepairReport{}, rep)
	assert.Zero(t, out.Len(), "cancellation before the patch's header writes nothing")
}
