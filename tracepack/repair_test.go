package tracepack

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/format"
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
