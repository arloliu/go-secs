package tracepack

import (
	"bytes"
	"fmt"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// bigDataRecord returns a data record over an HSMS S1F3 frame with a message text of n bytes.
func bigDataRecord(seq uint64, ts int64, n int) Record {
	frame := make([]byte, len(blockTestFrame)+n)
	copy(frame, blockTestFrame)
	frame[0], frame[1], frame[2], frame[3] = byte((10+n)>>24), byte((10+n)>>16), byte((10+n)>>8), byte(10+n)

	r := Record{
		Seq: seq, TSUTCNs: ts, MonoPresent: true, Epoch: 1,
		Kind: KindData, Dir: DirHostToEquipment, DecodeStatus: DecodeStatusOK, Payload: frame,
	}
	r.SetCapturedFieldValidity()

	return r
}

// patchEnvelope returns a copy of file whose block envelope at off is re-encoded, with a fresh envelope_crc, after fn changes it.
func patchEnvelope(t testing.TB, file []byte, off uint64, fn func(*format.BlockEnvelope)) []byte {
	t.Helper()

	e, err := format.UnmarshalBlockEnvelope(file[off:])
	require.NoError(t, err)
	fn(&e)

	out := bytes.Clone(file)
	copy(out[off:], format.AppendBlockEnvelope(nil, &e))

	return out
}

func TestOpenWalksUnfinalizedPack(t *testing.T) {
	t.Parallel()

	for _, c := range []Codec{CodecNone, CodecZstd} {
		t.Run(c.String(), func(t *testing.T) {
			t.Parallel()

			p := writeReaderPack(t, readerPackConfig{codec: c, validate: true, open: true}, hourRecords(3, 40))
			require.Len(t, p.blocks, 3)

			r := mustOpen(t, p.file, ReaderOptions{})
			h := r.Header()
			assert.False(t, h.Finalized)
			assert.Nil(t, h.Trailer)
			require.Error(t, h.FooterErr)
			assert.Equal(t, walkedInfos(p.blocks), r.Blocks())
			assert.Equal(t, []int64{int64(len(p.file))}, defectOffsets(t, r.openDefects),
				"a clean walk of a pack that is not finalized ends where more records may follow")
		})
	}
}

func TestOpenWalkTruncation(t *testing.T) {
	t.Parallel()

	p := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, hourRecords(4, 5))
	require.Len(t, p.blocks, 4)
	l := layoutOf(t, p.file)
	fo := l.tr.FooterOffset

	cuts := make([]uint64, 0, 1+6*len(p.blocks)+3)
	cuts = append(cuts, l.blocksStart)
	for _, b := range p.blocks {
		end := b.offset + uint64(b.onDiskLen)
		cuts = append(cuts, b.offset+1, b.offset+format.EnvelopeLen-1, b.offset+format.EnvelopeLen,
			b.offset+format.EnvelopeLen+uint64(b.onDiskLen-format.EnvelopeLen)/2, end-1, end)
	}
	cuts = append(cuts, fo+1, fo+format.EnvelopeLen, uint64(l.size)-1)

	for _, cut := range cuts {
		t.Run(fmt.Sprint(cut), func(t *testing.T) {
			t.Parallel()

			var whole []blockSummary
			stop := cut
			for _, b := range p.blocks {
				if b.offset+uint64(b.onDiskLen) > cut {
					stop = min(stop, b.offset)
					break
				}
				whole = append(whole, b)
			}

			r := mustOpen(t, p.file[:cut], ReaderOptions{})
			h := r.Header()
			assert.False(t, h.Finalized)
			assert.Nil(t, h.Trailer)
			assert.Equal(t, walkedInfos(whole), r.Blocks(), "the walk keeps every whole block before the cut")
			assert.Equal(t, []int64{int64(min(stop, fo))}, defectOffsets(t, r.openDefects))
		})
	}

	t.Run("inside the pack metadata", func(t *testing.T) {
		t.Parallel()

		_, err := openBytes(t, p.file[:l.blocksStart-1], ReaderOptions{})
		require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	})
}

func TestOpenWalkStopsAtBadEnvelope(t *testing.T) {
	t.Parallel()

	open := writeReaderPack(t, readerPackConfig{open: true}, hourRecords(3, 40))
	closed := writeReaderPack(t, readerPackConfig{}, hourRecords(3, 40))
	require.Len(t, open.blocks, 3)
	at := open.blocks[1].offset

	faults := []struct {
		name  string
		patch func(t testing.TB, file []byte) []byte
	}{
		{name: "bad magic", patch: func(_ testing.TB, file []byte) []byte { return flipByte(file, at) }},
		{name: "bad envelope_crc", patch: func(_ testing.TB, file []byte) []byte { return flipByte(file, at+8) }},
		{name: "record_count 0", patch: func(t testing.TB, file []byte) []byte {
			return patchEnvelope(t, file, at, func(e *format.BlockEnvelope) { e.RecordCount = 0 })
		}},
		{name: "record_header_len below 44", patch: func(t testing.TB, file []byte) []byte {
			return patchEnvelope(t, file, at, func(e *format.BlockEnvelope) { e.RecordHeaderLen = 43 })
		}},
		{name: "body_len over the format limit", patch: func(t testing.TB, file []byte) []byte {
			return patchEnvelope(t, file, at, func(e *format.BlockEnvelope) { e.BodyLen = format.MaxLen32 + 1 })
		}},
		{name: "body past the end of the block region", patch: func(t testing.TB, file []byte) []byte {
			return patchEnvelope(t, file, at, func(e *format.BlockEnvelope) { e.BodyLen = uint32(len(file)) })
		}},
	}
	for _, f := range faults {
		t.Run(f.name+" unfinalized", func(t *testing.T) {
			t.Parallel()

			r := mustOpen(t, f.patch(t, open.file), ReaderOptions{})
			assert.False(t, r.Header().Finalized)
			assert.Equal(t, walkedInfos(open.blocks[:1]), r.Blocks())
			assert.Equal(t, []int64{int64(at)}, defectOffsets(t, r.openDefects), "one defect, at the block the walk stops at")
		})
		t.Run(f.name+" finalized", func(t *testing.T) {
			t.Parallel()

			r := mustOpen(t, f.patch(t, invalidFooterFile(t, closed.file)), ReaderOptions{})
			h := r.Header()
			assert.True(t, h.Finalized)
			require.ErrorIs(t, h.FooterErr, ErrInvalidFooter)
			assert.Equal(t, walkedInfos(closed.blocks[:1]), r.Blocks())
			assert.Equal(t, []int64{int64(at)}, defectOffsets(t, r.openDefects))
		})
	}
}

func TestOpenWalkPassesOverBudgetBlock(t *testing.T) {
	t.Parallel()

	recs := []Record{
		testDataRecord(0, blockTestHour, 1),
		bigDataRecord(1, blockTestHour+hourNs, 20_000),
		testDataRecord(2, blockTestHour+2*hourNs, 1),
	}
	const maxBlock = 4096

	closed := writeReaderPack(t, readerPackConfig{}, recs)
	open := writeReaderPack(t, readerPackConfig{open: true}, recs)
	require.Len(t, closed.blocks, 3)
	require.Greater(t, closed.blocks[1].uncompressedLen, uint32(maxBlock))

	t.Run("walked", func(t *testing.T) {
		t.Parallel()

		r := mustOpen(t, closed.file, ReaderOptions{MaxBlockLen: maxBlock, MaxFooterLen: 16})
		h := r.Header()
		assert.True(t, h.Finalized)
		require.ErrorIs(t, h.FooterErr, ErrReadLimit)
		assert.Equal(t, walkedInfos(closed.blocks), r.Blocks(), "the walk passes over a block larger than MaxBlockLen")
		assert.Empty(t, r.openDefects)
	})

	t.Run("walked unfinalized", func(t *testing.T) {
		t.Parallel()

		r := mustOpen(t, open.file, ReaderOptions{MaxBlockLen: maxBlock})
		assert.Equal(t, walkedInfos(open.blocks), r.Blocks())
		assert.Equal(t, []int64{int64(len(open.file))}, defectOffsets(t, r.openDefects))
	})

	t.Run("indexed", func(t *testing.T) {
		t.Parallel()

		r := mustOpen(t, closed.file, ReaderOptions{MaxBlockLen: maxBlock})
		require.NoError(t, r.Header().FooterErr)
		assert.Equal(t, indexedInfos(closed.blocks), r.Blocks())
		assert.Empty(t, r.openDefects)
	})
}

func TestOpenWalkStopsAtMaxWalkedBlocks(t *testing.T) {
	t.Parallel()

	recs := hourRecords(3, 4)
	open := writeReaderPack(t, readerPackConfig{codec: CodecZstd, open: true}, recs)
	require.Len(t, open.blocks, 3)

	r := mustOpen(t, open.file, ReaderOptions{MaxWalkedBlocks: 2})
	assert.Equal(t, walkedInfos(open.blocks[:2]), r.Blocks(), "the walk indexes at most MaxWalkedBlocks blocks")

	run := iterate(t, r, Query{Payloads: true})
	assert.Equal(t, []uint64{0, 1, 2, 3, 4, 5, 6, 7}, run.seqs(), "the indexed blocks are read")
	require.Len(t, run.res.Incomplete, 1)
	d := run.res.Incomplete[0]
	assert.Equal(t, ReasonLimit, d.Reason)
	assert.Equal(t, -1, d.Block)
	assert.Equal(t, int64(open.blocks[2].offset), d.Offset, "the walk stops at the first block it does not index")
	require.ErrorIs(t, d.Err, ErrReadLimit)

	_, err := verifyBytes(t, open.file, VerifyOptions{Reader: ReaderOptions{MaxWalkedBlocks: 2}})
	require.ErrorIs(t, err, ErrReadLimit, "Verify guesses no outcome past the budget")

	rep, err := verifyBytes(t, open.file, VerifyOptions{Reader: ReaderOptions{MaxWalkedBlocks: 3}})
	require.NoError(t, err)
	assert.Equal(t, OutcomeUnfinalized, rep.Outcome, "a walk of exactly MaxWalkedBlocks blocks completes")

	closed := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, recs)
	r = mustOpen(t, closed.file, ReaderOptions{MaxWalkedBlocks: 1})
	assert.Len(t, r.Blocks(), 3, "a valid footer's index is not walked")
}

func TestReaderOptionsDefaultMaxWalkedBlocks(t *testing.T) {
	t.Parallel()

	assert.Equal(t, DefaultMaxWalkedBlocks, ReaderOptions{}.withDefaults().MaxWalkedBlocks)
	assert.Equal(t, DefaultMaxWalkedBlocks, ReaderOptions{MaxWalkedBlocks: -1}.withDefaults().MaxWalkedBlocks)
}
