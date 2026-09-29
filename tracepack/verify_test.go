package tracepack

import (
	"bytes"
	"context"
	"slices"
	"testing"

	"github.com/arloliu/go-secs/tracepack/internal/format"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// verifyBytes runs Verify over file.
func verifyBytes(t testing.TB, file []byte, opts VerifyOptions) (Report, error) {
	t.Helper()

	return Verify(t.Context(), bytes.NewReader(file), int64(len(file)), opts)
}

// mustVerify runs Verify over file and fails the test on an error.
func mustVerify(t testing.TB, file []byte) Report {
	t.Helper()

	rep, err := verifyBytes(t, file, VerifyOptions{})
	require.NoError(t, err)

	return rep
}

// blockEnd returns the file offset just past block s.
func blockEnd(s *blockSummary) uint64 {
	return s.offset + uint64(s.onDiskLen)
}

// requireFailedBlock requires that failed is the failure of block i of blocks, of reason.
func requireFailedBlock(t *testing.T, failed Defect, blocks []blockSummary, i int, reason IncompleteReason) {
	t.Helper()

	assert.Equal(t, reason, failed.Reason, "%v", failed.Err)
	assert.Equal(t, i, failed.Block)
	assert.Equal(t, int64(blocks[i].offset), failed.Offset)
}

func TestVerifyConsistent(t *testing.T) {
	t.Parallel()

	for _, c := range []Codec{CodecNone, CodecZstd} {
		t.Run(c.String(), func(t *testing.T) {
			t.Parallel()

			p := writeReaderPack(t, readerPackConfig{codec: c}, hourRecords(3, 40))
			rep := mustVerify(t, p.file)

			assert.Equal(t, OutcomeFinalizedConsistent, rep.Outcome)
			assert.True(t, rep.Finalized)
			require.NoError(t, rep.FooterErr)
			assert.Equal(t, 3, rep.Blocks)
			assert.Equal(t, 3, rep.Validated)
			assert.Equal(t, uint64(120), rep.Records)
			assert.Equal(t, layoutOf(t, p.file).tr.FooterOffset, rep.PrefixEnd)
			assert.Empty(t, rep.Failed)
			assert.Empty(t, rep.Disagreements)
			assert.Empty(t, rep.WriterDefects)
			assert.Empty(t, rep.Lost)
		})
	}
}

func TestVerifyEmptyPack(t *testing.T) {
	t.Parallel()

	p := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, nil)
	rep := mustVerify(t, p.file)

	assert.Equal(t, OutcomeFinalizedConsistent, rep.Outcome)
	assert.Zero(t, rep.Blocks)
	assert.Equal(t, layoutOf(t, p.file).blocksStart, rep.PrefixEnd)
}

func TestVerifyFailedBlocks(t *testing.T) {
	t.Parallel()

	recs := hourRecords(3, 40)
	p := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, recs)
	require.Len(t, p.blocks, 3)
	bodyByte := func(i int) uint64 { return p.blocks[i].offset + format.EnvelopeLen + 1 }

	t.Run("middle body with a valid footer", func(t *testing.T) {
		t.Parallel()

		rep := mustVerify(t, flipByte(p.file, bodyByte(1)))
		assert.Equal(t, OutcomeCorruptMiddle, rep.Outcome)
		assert.True(t, rep.Finalized)
		require.NoError(t, rep.FooterErr)
		assert.Equal(t, 2, rep.Validated)
		assert.Equal(t, uint64(80), rep.Records)
		assert.Equal(t, p.blocks[1].offset, rep.PrefixEnd)
		require.Len(t, rep.Failed, 1)
		requireFailedBlock(t, rep.Failed[0], p.blocks, 1, ReasonCorruptBlock)

		capture := UUID(layoutHeader(t, p.file).CaptureID)
		b := &p.blocks[1]
		assert.Equal(t, []Coverage{{
			CaptureID: &capture, SeqFirst: new(b.firstSeq), SeqLast: new(b.lastSeq), TimeStart: new(b.tsMin), TimeEnd: new(b.tsMax),
		}}, rep.Lost)
	})

	t.Run("middle body without a valid footer", func(t *testing.T) {
		t.Parallel()

		rep := mustVerify(t, flipByte(invalidFooterFile(t, p.file), bodyByte(1)))
		assert.Equal(t, OutcomeCorruptMiddle, rep.Outcome)
		assert.True(t, rep.Finalized)
		require.ErrorIs(t, rep.FooterErr, ErrInvalidFooter)
		require.Len(t, rep.Failed, 1)
		requireFailedBlock(t, rep.Failed[0], p.blocks, 1, ReasonCorruptBlock)
		assert.Empty(t, rep.Lost, "without a valid footer the failed block's seqs are unknown")
	})

	t.Run("last envelope with a valid footer", func(t *testing.T) {
		t.Parallel()

		rep := mustVerify(t, flipByte(p.file, p.blocks[2].offset+1))
		assert.Equal(t, OutcomeFinalizedTruncated, rep.Outcome)
		assert.Equal(t, 2, rep.Validated)
		assert.Equal(t, p.blocks[2].offset, rep.PrefixEnd)
		require.Len(t, rep.Failed, 1)
		requireFailedBlock(t, rep.Failed[0], p.blocks, 2, ReasonCorruptBlock)
		assert.Len(t, rep.Lost, 1)
	})

	t.Run("middle envelope with a valid footer", func(t *testing.T) {
		t.Parallel()

		rep := mustVerify(t, flipByte(p.file, p.blocks[1].offset+1))
		assert.Equal(t, OutcomeCorruptMiddle, rep.Outcome, "the F-2 entry passes the failed envelope")
		assert.Equal(t, 2, rep.Validated)
	})

	t.Run("middle envelope without a valid footer", func(t *testing.T) {
		t.Parallel()

		rep := mustVerify(t, flipByte(invalidFooterFile(t, p.file), p.blocks[1].offset+1))
		assert.Equal(t, OutcomeFinalizedTruncated, rep.Outcome, "no block after the stop is located")
		assert.Equal(t, 1, rep.Blocks)
		assert.Equal(t, 1, rep.Validated)
		assert.Equal(t, p.blocks[1].offset, rep.PrefixEnd)
		require.Len(t, rep.Failed, 1)
		assert.Equal(t, ReasonTruncated, rep.Failed[0].Reason)
		assert.Equal(t, -1, rep.Failed[0].Block)
		assert.Equal(t, int64(p.blocks[1].offset), rep.Failed[0].Offset)
	})
}

// layoutHeader returns the file header of file.
func layoutHeader(t testing.TB, file []byte) format.FileHeader {
	t.Helper()

	hdr, err := format.UnmarshalFileHeader(file)
	require.NoError(t, err)

	return hdr
}

func TestVerifyUnfinalized(t *testing.T) {
	t.Parallel()

	recs := hourRecords(3, 40)
	open := writeReaderPack(t, readerPackConfig{codec: CodecZstd, open: true}, recs)
	closed := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, recs)
	require.Len(t, open.blocks, 3)

	t.Run("ends on a block boundary", func(t *testing.T) {
		t.Parallel()

		rep := mustVerify(t, open.file)
		assert.Equal(t, OutcomeUnfinalized, rep.Outcome)
		assert.False(t, rep.Finalized)
		assert.Equal(t, 3, rep.Validated)
		assert.Equal(t, uint64(len(open.file)), rep.PrefixEnd)
		assert.Empty(t, rep.Failed)
	})

	t.Run("ends inside a block", func(t *testing.T) {
		t.Parallel()

		cut := open.file[:blockEnd(&open.blocks[2])-1]
		rep := mustVerify(t, cut)
		assert.Equal(t, OutcomeUnfinalized, rep.Outcome)
		assert.Equal(t, 2, rep.Validated)
		assert.Equal(t, open.blocks[2].offset, rep.PrefixEnd)
		require.Len(t, rep.Failed, 1)
		assert.Equal(t, ReasonTruncated, rep.Failed[0].Reason)
		assert.Equal(t, int64(open.blocks[2].offset), rep.Failed[0].Offset)
	})

	t.Run("footer CRC mismatch", func(t *testing.T) {
		t.Parallel()

		l := layoutOf(t, closed.file)
		rep := mustVerify(t, flipByte(closed.file, l.tr.FooterOffset+1))
		assert.Equal(t, OutcomeUnfinalized, rep.Outcome)
		assert.False(t, rep.Finalized)
		require.ErrorIs(t, rep.FooterErr, ErrChecksum)
		assert.Equal(t, 3, rep.Validated)
		assert.Equal(t, l.tr.FooterOffset, rep.PrefixEnd)
	})

	t.Run("trailer places the footer inside the pack metadata", func(t *testing.T) {
		t.Parallel()

		l := layoutOf(t, closed.file)
		trailerAt := uint64(l.size) - format.TrailerLen
		file := patchTrailer(t, closed.file, func(tr *format.Trailer) {
			tr.FooterOffset = l.blocksStart - 1
			tr.FooterLen = trailerAt - tr.FooterOffset
			tr.FooterCRC = format.CRC(closed.file[tr.FooterOffset:trailerAt])
		})
		rep := mustVerify(t, file)
		assert.Equal(t, OutcomeUnfinalized, rep.Outcome)
		assert.False(t, rep.Finalized)
		assert.Equal(t, 3, rep.Validated)
	})
}

func TestVerifyErrors(t *testing.T) {
	t.Parallel()

	p := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, hourRecords(3, 40))
	l := layoutOf(t, p.file)
	require.Greater(t, l.tr.FooterUncompressedLen, l.tr.FooterLen, "the footer must compress")

	tests := []struct {
		name string
		file []byte
		opts ReaderOptions
		want error
	}{
		{name: "bad magic", file: flipByte(p.file, 0), want: ErrNotTracepack},
		{name: "file header CRC", file: flipByte(p.file, 20), want: ErrChecksum},
		{name: "pack metadata CRC", file: flipByte(p.file, format.FileHeaderLen+1), want: ErrChecksum},
		{name: "block over MaxBlockLen", file: p.file, opts: ReaderOptions{MaxBlockLen: 16}, want: ErrReadLimit},
		{name: "footer over MaxFooterLen", file: p.file, opts: ReaderOptions{MaxFooterLen: int64(l.tr.FooterLen) - 1}, want: ErrReadLimit},
		{name: "decoded footer over MaxFooterLen", file: p.file, opts: ReaderOptions{MaxFooterLen: int64(l.tr.FooterLen)}, want: ErrReadLimit},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := verifyBytes(t, tt.file, VerifyOptions{Reader: tt.opts})
			require.ErrorIs(t, err, tt.want)
		})
	}

	t.Run("oversized footer with a CRC mismatch", func(t *testing.T) {
		t.Parallel()

		file := flipByte(p.file, l.tr.FooterOffset+1)
		rep, err := verifyBytes(t, file, VerifyOptions{Reader: ReaderOptions{MaxFooterLen: 1}})
		require.NoError(t, err)
		assert.Equal(t, OutcomeUnfinalized, rep.Outcome)
		require.ErrorIs(t, rep.FooterErr, ErrChecksum)
	})

	t.Run("canceled context", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := Verify(ctx, bytes.NewReader(p.file), int64(len(p.file)), VerifyOptions{})
		require.ErrorIs(t, err, context.Canceled)
	})
}

// cloneSummaries returns a deep copy of blocks, so a test may change any of its slices.
func cloneSummaries(blocks []blockSummary) []blockSummary {
	out := slices.Clone(blocks)
	for i := range out {
		s := &out[i]
		s.kindCounts = slices.Clone(s.kindCounts)
		s.dirCounts = slices.Clone(s.dirCounts)
		s.decodeStatusCounts = slices.Clone(s.decodeStatusCounts)
		s.epochs = slices.Clone(s.epochs)
		s.boundaries = slices.Clone(s.boundaries)
		s.seqRanges = slices.Clone(s.seqRanges)
	}

	return out
}

// resummarized returns file with its footer rebuilt by the Writer's footer builder from blocks after edit changes block i,
// so F-5 is the aggregate of the changed F-3 summary and the footer validates while disagreeing with the block's records.
func resummarized(t testing.TB, file []byte, blocks []blockSummary, i int, edit func(s *blockSummary)) []byte {
	t.Helper()

	changed := cloneSummaries(blocks)
	edit(&changed[i])
	footer, _ := buildFooter(changed)

	return refooter(t, file, footer)
}

// requireInconsistent requires that rep reports a finalized pack whose footer disagrees with block i, and nothing else.
func requireInconsistent(t *testing.T, rep Report, i int, msg string) {
	t.Helper()

	assert.Equal(t, OutcomeFinalizedInconsistent, rep.Outcome)
	require.NoError(t, rep.FooterErr, "the footer must stay valid")
	assert.Empty(t, rep.Failed)
	require.Len(t, rep.Disagreements, 1)
	assert.Equal(t, ReasonIndexMismatch, rep.Disagreements[0].Reason)
	assert.Equal(t, i, rep.Disagreements[0].Block)
	require.ErrorContains(t, rep.Disagreements[0].Err, msg)
}

func TestVerifyFooterSummaryDisagrees(t *testing.T) {
	t.Parallel()

	rich := richFooterPack(t, CodecZstd)
	require.Len(t, rich.blocks, 5)
	require.Len(t, rich.blocks[0].seqRanges, 2, "block 0 holds seqs 10, 11 and 13")
	require.Len(t, rich.blocks[1].boundaries, 1, "block 1 holds a gap boundary")
	require.Len(t, rich.blocks[1].epochs, 2, "block 1 holds epochs 0 and 1")

	tests := []struct {
		name  string
		block int
		edit  func(s *blockSummary)
		msg   string
	}{
		{name: "kind_counts", block: 3, edit: func(s *blockSummary) {
			s.kindCounts[KindData]--
			s.kindCounts = append(s.kindCounts, 0)
			s.kindCounts[KindControl]++
		}, msg: "kind_counts"},
		{name: "dir_counts", block: 3, edit: func(s *blockSummary) {
			k := slices.IndexFunc(s.dirCounts, func(n uint32) bool { return n > 0 })
			s.dirCounts[k]--
			s.dirCounts[(k+1)%len(s.dirCounts)]++
		}, msg: "dir_counts"},
		{name: "quality_union", block: 2, edit: func(s *blockSummary) { s.qualityUnion |= QualityOrderingUncertain }, msg: "quality_union"},
		{name: "epoch entry", block: 1, edit: func(s *blockSummary) { s.epochs[0].tsMax-- }, msg: "epoch"},
		{name: "boundary entry", block: 1, edit: func(s *blockSummary) { s.boundaries[0].gapEnd = new(*s.boundaries[0].gapEnd + 1) }, msg: "boundary"},
		{name: "duplicated boundary entry", block: 1, edit: func(s *blockSummary) { s.boundaries = append(s.boundaries, s.boundaries[0]) }, msg: "boundary"},
		{name: "seq_range", block: 0, edit: func(s *blockSummary) { s.seqRanges = []seqRange{{10, 10}, {12, 13}} }, msg: "seq_range"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			file := resummarized(t, rich.file, rich.blocks, tt.block, tt.edit)
			requireInconsistent(t, mustVerify(t, file), tt.block, tt.msg)
		})
	}
}

func TestVerifyRichPackConsistent(t *testing.T) {
	t.Parallel()

	for _, c := range []Codec{CodecNone, CodecZstd} {
		t.Run(c.String(), func(t *testing.T) {
			t.Parallel()

			rich := richFooterPack(t, c)
			rep := mustVerify(t, rich.file)
			assert.Equal(t, OutcomeFinalizedConsistent, rep.Outcome, "%v", rep.Disagreements)

			rep = mustVerify(t, editFooter(t, rich.file, addFooterTags))
			assert.Equal(t, OutcomeFinalizedConsistent, rep.Outcome, "unknown F-3 tags are skipped: %v", rep.Disagreements)
		})
	}
}

func TestVerifyTrailerDisagrees(t *testing.T) {
	t.Parallel()

	p := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, hourRecords(3, 40))
	file := patchTrailer(t, invalidFooterFile(t, p.file), func(tr *format.Trailer) { tr.RecordCount++ })

	rep := mustVerify(t, file)
	assert.Equal(t, OutcomeFinalizedInconsistent, rep.Outcome)
	assert.True(t, rep.Finalized)
	require.ErrorIs(t, rep.FooterErr, ErrInvalidFooter)
	assert.Equal(t, 3, rep.Validated)
	require.Len(t, rep.Disagreements, 1)
	assert.Equal(t, -1, rep.Disagreements[0].Block)
	assert.Equal(t, ReasonIndexMismatch, rep.Disagreements[0].Reason)
	require.ErrorContains(t, rep.Disagreements[0].Err, "the trailer claims 3 blocks of 121 records")

	rep = mustVerify(t, invalidFooterFile(t, p.file))
	assert.Equal(t, OutcomeFinalizedInconsistent, rep.Outcome, "an invalid footer alone")
	assert.Empty(t, rep.Disagreements)
}
