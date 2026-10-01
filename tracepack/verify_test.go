package tracepack

import (
	"bytes"
	"context"
	"math"
	"slices"
	"testing"

	"github.com/arloliu/go-secs/tracepack/internal/format"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// verifyBytes runs Verify over file.
func verifyBytes(t testing.TB, file []byte, opts VerifyOptions) (VerifyReport, error) {
	t.Helper()

	return Verify(t.Context(), bytes.NewReader(file), int64(len(file)), opts)
}

// mustVerify runs Verify over file and fails the test on an error.
func mustVerify(t testing.TB, file []byte) VerifyReport {
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

	t.Run("block of several seq ranges with a valid footer", func(t *testing.T) {
		t.Parallel()

		rich := richFooterPack(t, CodecZstd)
		b := &rich.blocks[0]
		require.Len(t, b.seqRanges, 2, "block 0 holds seqs 10, 11 and 13")
		rep := mustVerify(t, flipByte(rich.file, b.offset+format.EnvelopeLen+1))
		assert.Equal(t, OutcomeCorruptMiddle, rep.Outcome)

		capture := UUID(layoutHeader(t, rich.file).CaptureID)
		lost := func(first, last uint64) Coverage {
			return Coverage{CaptureID: &capture, SeqFirst: new(first), SeqLast: new(last), TimeStart: new(b.tsMin), TimeEnd: new(b.tsMax)}
		}
		assert.Equal(t, []Coverage{lost(10, 11), lost(13, 13)}, rep.Lost, "one entry per seq range, each with the block's time range")
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
	out := make([]blockSummary, len(blocks))
	for i := range blocks {
		out[i] = blocks[i].clone()
	}

	return out
}

// resummarized returns file with its footer rebuilt by the Writer's footer builder from blocks after edit changes block i,
// so F-5 is the aggregate of the changed F-3 summary and the footer validates while disagreeing with the block's records.
func resummarized(t testing.TB, file []byte, blocks []blockSummary, i int, edit func(s *blockSummary)) []byte {
	t.Helper()

	changed := cloneSummaries(blocks)
	edit(&changed[i])
	footer, _, err := buildFooter(changed, math.MaxUint32, 0)
	require.NoError(t, err)

	return refooter(t, file, footer)
}

// requireInconsistent requires that rep reports a finalized pack whose footer disagrees with block i, and nothing else.
func requireInconsistent(t *testing.T, rep VerifyReport, i int, msg string) {
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

// defectTestSteps returns the records of the writer-defect test pack, three blocks in one UTC hour:
//
//	block 0: 0 data, 1 annotation, 2 state-transition event
//	block 1: 3 data, 4 data
//	block 2: 5 data
func defectTestSteps(t testing.TB) []footerTestStep {
	t.Helper()

	note, err := (&Annotation{AnnotationKind: AnnotationKindNote, Text: new("note")}).MarshalBinary()
	require.NoError(t, err)
	ts := func(i int64) int64 { return blockTestHour + 1000*i }

	return []footerTestStep{
		{rec: testDataRecord(0, ts(0), 1)},
		{rec: Record{Seq: 1, TSUTCNs: ts(1), Epoch: 1, Kind: KindAnnotation, Dir: DirLocal, DecodeStatus: DecodeStatusNotApplicable, Payload: note}},
		{rec: testEventRecord(t, 2, ts(2), 1, &TransportEvent{
			Event: EventStateTransition, PrevState: new(StateNotSelected), CurState: new(StateSelected),
		}), flush: true},
		{rec: testDataRecord(3, ts(3), 1)},
		{rec: testDataRecord(4, ts(4), 1), flush: true},
		{rec: testDataRecord(5, ts(5), 1)},
	}
}

// defectTestPack writes the writer-defect test pack, finalized, and checks its layout.
func defectTestPack(t testing.TB) *footerTestPack {
	t.Helper()

	p := writeFooterTestPack(t, CodecZstd, defectTestSteps(t))
	require.Len(t, p.blocks, 3)
	require.Equal(t, []uint32{3, 2, 1}, []uint32{p.blocks[0].recordCount, p.blocks[1].recordCount, p.blocks[2].recordCount})

	return p
}

// editBlock returns file with block i's records changed by edit and the block re-encoded (see rebuildPack).
func editBlock(t testing.TB, file []byte, i int, edit func(b *testBlock)) []byte {
	t.Helper()

	return rebuildPack(t, file, func(j int, b *testBlock) {
		if j == i {
			edit(b)
		}
	})
}

// requireWriterDefect requires that defects holds exactly one defect, of kind, in block i at off, for seq.
func requireWriterDefect(t *testing.T, defects []WriterDefect, kind WriterDefectKind, i int, off, seq uint64) {
	t.Helper()

	require.Len(t, defects, 1, "%v", defects)
	d := defects[0]
	assert.Equal(t, kind, d.Kind, "%v", d.Err)
	assert.Equal(t, i, d.Block)
	assert.Equal(t, int64(off), d.Offset)
	assert.Equal(t, seq, d.Seq)
	require.Error(t, d.Err)
}

func TestVerifyRecordWriterDefects(t *testing.T) {
	t.Parallel()

	p := defectTestPack(t)
	tests := []struct {
		name string
		edit func(b *testBlock)
		kind WriterDefectKind
		seq  uint64
	}{
		{name: "System Bytes marked beyond a 10-byte payload", edit: func(b *testBlock) {
			b.payloads[0] = b.payloads[0][:10]
			b.headers[0].PayloadLen = 10
		}, kind: WriterDefectFieldValidity, seq: 0},
		{name: "annotation with field_validity", edit: func(b *testBlock) {
			b.headers[1].FieldValidity = uint8(FieldValiditySessionID)
		}, kind: WriterDefectEventFieldValidity, seq: 1},
		{name: "transport event with field_validity", edit: func(b *testBlock) {
			b.headers[2].FieldValidity = uint8(FieldValiditySystemBytes)
		}, kind: WriterDefectEventFieldValidity, seq: 2},
		{name: "annotation payload not a TLV body", edit: func(b *testBlock) {
			b.payloads[1] = []byte{0xFF}
			b.headers[1].PayloadLen = 1
		}, kind: WriterDefectEventPayload, seq: 1},
		{name: "transport event payload not a TLV body", edit: func(b *testBlock) {
			b.payloads[2] = []byte{0xFF}
			b.headers[2].PayloadLen = 1
		}, kind: WriterDefectEventPayload, seq: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rep := mustVerify(t, editBlock(t, p.file, 0, tt.edit))
			assert.Equal(t, OutcomeFinalizedConsistent, rep.Outcome, "a writer defect never changes the outcome: %v", rep.Disagreements)
			requireWriterDefect(t, rep.WriterDefects, tt.kind, 0, p.blocks[0].offset, tt.seq)
		})
	}

	t.Run("reserved field_validity bits are ignored", func(t *testing.T) {
		t.Parallel()

		rep := mustVerify(t, editBlock(t, p.file, 0, func(b *testBlock) { b.headers[1].FieldValidity = 0xC0 }))
		assert.Equal(t, OutcomeFinalizedConsistent, rep.Outcome)
		assert.Empty(t, rep.WriterDefects)
	})
}

func TestVerifyBlockWriterDefects(t *testing.T) {
	t.Parallel()

	p := defectTestPack(t)
	open := writeFooterTestPack(t, CodecZstd, defectTestSteps(t))
	openFile := open.file[:open.tr.FooterOffset]
	nextHour := blockTestHour + hourNs

	// seqOverlap makes block 1 start at seq 2, the last seq of block 0, and states it in its F-2 entry, so the index is faithful.
	seqOverlap := func(b *testBlock) {
		b.env.FirstSeq = 2
		b.headers[0].Seq = 2
	}
	// hourSpan moves block 1's last record into the next hour.
	hourSpan := func(b *testBlock) { b.headers[1].TSUTCNs = nextHour }
	faithfulHourSpan := func(b *testBlock) {
		hourSpan(b)
		b.patchEntry = func(e *format.F2Entry) { e.TSMax = nextHour }
	}

	tests := []struct {
		name      string
		file      []byte
		want      Outcome
		footerErr bool
		disagrees bool
		kind      WriterDefectKind
		seq       uint64
	}{
		{name: "seq order, faithful index", file: editBlock(t, p.file, 1, seqOverlap),
			want: OutcomeFinalizedInconsistent, footerErr: true, kind: WriterDefectSeqOrder, seq: 2},
		{name: "seq order, unfinalized", file: editBlock(t, openFile, 1, seqOverlap),
			want: OutcomeUnfinalized, footerErr: true, kind: WriterDefectSeqOrder, seq: 2},
		{name: "hour span, faithful index", file: editBlock(t, p.file, 1, faithfulHourSpan),
			want: OutcomeFinalizedInconsistent, footerErr: true, kind: WriterDefectHourSpan, seq: 3},
		{name: "hour span, index understating it", file: editBlock(t, p.file, 1, hourSpan),
			want: OutcomeFinalizedInconsistent, disagrees: true, kind: WriterDefectHourSpan, seq: 3},
		{name: "hour span, unfinalized", file: editBlock(t, openFile, 1, hourSpan),
			want: OutcomeUnfinalized, footerErr: true, kind: WriterDefectHourSpan, seq: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rep := mustVerify(t, tt.file)
			assert.Equal(t, tt.want, rep.Outcome)
			assert.Equal(t, tt.footerErr, rep.FooterErr != nil, "%v", rep.FooterErr)
			assert.Equal(t, tt.disagrees, len(rep.Disagreements) > 0, "%v", rep.Disagreements)
			assert.Equal(t, 3, rep.Validated)
			requireWriterDefect(t, rep.WriterDefects, tt.kind, 1, p.blocks[1].offset, tt.seq)
		})
	}

	t.Run("precedence with a failed block", func(t *testing.T) {
		t.Parallel()

		defective := editBlock(t, p.file, 0, func(b *testBlock) {
			b.headers[2].TSUTCNs = nextHour
			b.patchEntry = func(e *format.F2Entry) { e.TSMax = nextHour }
		})
		// Re-encoding block 0 may change its length, so the later blocks are located in the rebuilt file.
		blocks := mustOpen(t, defective, ReaderOptions{}).Blocks()
		body := func(i int) uint64 { return blocks[i].Offset + format.EnvelopeLen + 1 }

		rep := mustVerify(t, flipByte(defective, body(2)))
		assert.Equal(t, OutcomeFinalizedTruncated, rep.Outcome)
		requireWriterDefect(t, rep.WriterDefects, WriterDefectHourSpan, 0, p.blocks[0].offset, 0)

		rep = mustVerify(t, flipByte(defective, body(1)))
		assert.Equal(t, OutcomeCorruptMiddle, rep.Outcome)
		requireWriterDefect(t, rep.WriterDefects, WriterDefectHourSpan, 0, p.blocks[0].offset, 0)
	})
}

func TestVerifySeqStartDefect(t *testing.T) {
	t.Parallel()

	// The repair test pack starts at seq 10; its blocks start at 10, 14, 21, 23 and 31.
	p := defaultRepairPack(t)
	failedFirst := flipByte(p.file, p.bodyByteOf(0))

	tests := []struct {
		name   string
		file   []byte
		want   Outcome
		defect bool
		block  int
		seq    uint64
	}{
		{name: "seq_start below the first record", file: withSeqStart(t, p.file, 9),
			want: OutcomeFinalizedConsistent, defect: true, block: 0, seq: 10},
		{name: "seq_start above the first record", file: withSeqStart(t, p.file, 11),
			want: OutcomeFinalizedConsistent, defect: true, block: 0, seq: 10},
		{name: "after a failed block, seq_start at the first validated record", file: withSeqStart(t, failedFirst, 14),
			want: OutcomeCorruptMiddle, defect: true, block: 1, seq: 14},
		{name: "after a failed block, seq_start below the first validated record", file: withSeqStart(t, failedFirst, 13),
			want: OutcomeCorruptMiddle},
		{name: "after a failed block, the pack's own seq_start", file: failedFirst, want: OutcomeCorruptMiddle},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rep := mustVerify(t, tt.file)
			assert.Equal(t, tt.want, rep.Outcome, "a writer defect never changes the outcome: %v", rep.Disagreements)
			if !tt.defect {
				assert.Empty(t, rep.WriterDefects)

				return
			}
			requireWriterDefect(t, rep.WriterDefects, WriterDefectSeqStart, tt.block, p.blocks[tt.block].offset, tt.seq)
		})
	}
}
