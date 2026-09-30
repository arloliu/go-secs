package tracepack

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// sourceBlock is one block of a pack as a full read gives it: its on-disk bytes and the summary of its records.
type sourceBlock struct {
	raw []byte
	sum blockSummary
}

// readSourceBlocks opens file and reads every located block in full,
// requiring each to be read (a block whose records disagree with its F-2 entry is read too).
func readSourceBlocks(t testing.TB, file []byte) (*Reader, []sourceBlock) {
	t.Helper()

	r := mustOpen(t, file, ReaderOptions{})
	var buf blockBuf
	out := make([]sourceBlock, len(r.blocks))
	for i := range r.blocks {
		d, def, err := r.readBlock(i, &buf)
		require.NoError(t, err)
		require.NotNil(t, d, "block %d fails: %v", i, def)
		out[i] = sourceBlock{raw: bytes.Clone(buf.raw), sum: summaryOf(d)}
	}

	return r, out
}

// copyOptions returns WriterOptions for a pack that copies blocks from the pack r opened:
// its metadata with seq_start at the first block's first seq, its pack and capture ids, and footer codec c.
func copyOptions(r *Reader, blocks []sourceBlock, c Codec) WriterOptions {
	h := r.Header()
	if len(blocks) > 0 {
		h.Meta.SeqStart = blocks[0].sum.firstSeq
	}

	return WriterOptions{Meta: h.Meta, Codec: c, PackID: h.PackID, CaptureID: h.CaptureID}
}

// copyBlocks writes blocks into a new pack with opts and returns it.
func copyBlocks(t testing.TB, opts WriterOptions, blocks []sourceBlock) []byte {
	t.Helper()

	var out bytes.Buffer
	w, err := startWriter(&out, opts)
	require.NoError(t, err)
	for i := range blocks {
		require.NoError(t, w.appendBlock(blocks[i].raw, &blocks[i].sum), "block %d", i)
	}
	_, err = w.Close()
	require.NoError(t, err)

	return out.Bytes()
}

// footerCodecOf returns the footer codec of a finalized pack.
func footerCodecOf(t testing.TB, file []byte) Codec {
	t.Helper()

	tr, err := format.UnmarshalTrailer(file[len(file)-format.TrailerLen:])
	require.NoError(t, err)

	return Codec(tr.FooterCodec)
}

// blocksStartOf returns the offset of the first block of file.
func blocksStartOf(t testing.TB, file []byte) int {
	t.Helper()

	hdr, err := format.UnmarshalFileHeader(file)
	require.NoError(t, err)

	return format.FileHeaderLen + int(hdr.PackMetadataLen)
}

func TestAppendBlockCopiesPacks(t *testing.T) {
	t.Parallel()

	for _, c := range []Codec{CodecNone, CodecZstd} {
		rich := richFooterPack(t, c).file
		sources := map[string][]byte{
			"rich":           rich,
			"widened blocks": widenFile(t, rich, func(i int) bool { return i%2 == 0 }),
			"unknown values": compatPack(t, compatConfig{codec: c}, nil),
		}
		for name, src := range sources {
			r, blocks := readSourceBlocks(t, src)
			cp := copyBlocks(t, copyOptions(r, blocks, footerCodecOf(t, src)), blocks)

			rep := mustVerify(t, cp)
			assert.Equal(t, OutcomeFinalizedConsistent, rep.Outcome, "%s %s", c, name)
			assert.Equal(t, len(blocks), rep.Validated, "%s %s", c, name)
			// Same metadata, so the same offsets: blocks, footer and trailer are the source's byte for byte.
			start := blocksStartOf(t, src)
			assert.Equal(t, src[start:], cp[start:], "%s %s", c, name)
		}
	}
}

func TestAppendBlockCopiesASubset(t *testing.T) {
	t.Parallel()

	src := richFooterPack(t, CodecZstd).file
	r, blocks := readSourceBlocks(t, src)
	require.Len(t, blocks, 5)
	// Drop block 1, which holds epoch 1's socket-close, and block 4, which holds the stop boundary.
	kept := []sourceBlock{blocks[0], blocks[2], blocks[3]}
	cp := copyBlocks(t, copyOptions(r, kept, CodecZstd), kept)

	rep := mustVerify(t, cp)
	require.Equal(t, OutcomeFinalizedConsistent, rep.Outcome)
	var want uint64
	for _, b := range kept {
		want += uint64(b.sum.recordCount)
	}
	assert.Equal(t, want, rep.Records)

	cr := mustOpen(t, cp, ReaderOptions{})
	require.NoError(t, cr.Header().FooterErr)
	assert.Equal(t, kept[2].sum.lastSeq, cr.Header().Trailer.LastSeq)
	for i, b := range cr.footer.blocks {
		for _, bd := range b.boundaries {
			assert.NotEqual(t, BoundaryKindStop, bd.kind, "block %d: the stop boundary was not copied", i)
		}
		for _, e := range b.epochs {
			assert.False(t, e.hasCloseSeq, "block %d epoch %d: the closing block was not copied", i, e.epoch)
		}
	}
}

func TestAppendBlockKeepsReservedBits(t *testing.T) {
	t.Parallel()

	// Set a reserved quality bit, a retired record_flags bit and a reserved field_validity bit in every record header,
	// which a record header encoder would mask.
	src := rebuildPack(t, richFooterPack(t, CodecNone).file, func(_ int, b *testBlock) {
		b.patchRows = func(rows []byte, rhl int) {
			for off := 0; off < len(rows); off += rhl {
				binary.LittleEndian.PutUint16(rows[off+36:], binary.LittleEndian.Uint16(rows[off+36:])|1<<15)
				rows[off+42] |= 1 << 7
				rows[off+43] |= 1 << 0
			}
		}
	})
	r, blocks := readSourceBlocks(t, src)
	cp := copyBlocks(t, copyOptions(r, blocks, CodecNone), blocks)

	rep := mustVerify(t, cp)
	assert.Equal(t, OutcomeFinalizedConsistent, rep.Outcome)
	start := blocksStartOf(t, src)
	end := int(r.Header().Trailer.FooterOffset)
	assert.Equal(t, src[start:end], cp[start:end], "the reserved bits are copied with the blocks")
}

func TestAppendBlockOwnsItsSummary(t *testing.T) {
	t.Parallel()

	src := richFooterPack(t, CodecZstd).file
	r, blocks := readSourceBlocks(t, src)

	var out bytes.Buffer
	w, err := startWriter(&out, copyOptions(r, blocks, CodecZstd))
	require.NoError(t, err)
	for i := range blocks {
		s := blocks[i].sum.clone()
		require.NoError(t, w.appendBlock(blocks[i].raw, &s))
		// The caller reuses its summary afterwards.
		s.kindCounts[0] += 7
		s.seqRanges[0].last++
		for j := range s.epochs {
			s.epochs[j].recordCount++
		}
		for j := range s.boundaries {
			s.boundaries[j].seq++
			if s.boundaries[j].gapStart != nil {
				*s.boundaries[j].gapStart++
			}
			if s.boundaries[j].gapEnd != nil {
				*s.boundaries[j].gapEnd++
			}
		}
	}
	_, err = w.Close()
	require.NoError(t, err)

	start := blocksStartOf(t, src)
	assert.Equal(t, src[start:], out.Bytes()[start:])
}

// failAfterWriter writes into buf until limit bytes are written, then fails each write,
// taking every byte of the write that crosses the limit when takeAll is set.
type failAfterWriter struct {
	buf     bytes.Buffer
	limit   int
	takeAll bool
}

var errTestWrite = errors.New("test write failed")

func (f *failAfterWriter) Write(p []byte) (int, error) {
	room := f.limit - f.buf.Len()
	if len(p) <= room {
		return f.buf.Write(p)
	}
	if f.takeAll {
		n, _ := f.buf.Write(p)
		return n, errTestWrite
	}
	n, _ := f.buf.Write(p[:max(room, 0)])

	return n, errTestWrite
}

// syncFailer fails every Sync after ok successful calls.
type syncFailer struct {
	calls int
	ok    int
}

var errTestSync = errors.New("test sync failed")

func (s *syncFailer) Sync() error {
	s.calls++
	if s.calls > s.ok {
		return errTestSync
	}

	return nil
}

func TestAppendBlockRejections(t *testing.T) {
	t.Parallel()

	src := richFooterPack(t, CodecZstd).file
	r, blocks := readSourceBlocks(t, src)

	tests := []struct {
		name string
		// opts adjusts the options; run appends blocks and returns the error of the rejected call.
		opts func(o *WriterOptions)
		run  func(w *Writer) error
		want error
	}{
		{
			name: "first seq is not seq_start",
			opts: func(o *WriterOptions) { o.Meta.SeqStart++ },
			run:  func(w *Writer) error { return w.appendBlock(blocks[0].raw, &blocks[0].sum) },
			want: ErrSeqOrder,
		},
		{
			name: "first seq not above the last seq written",
			run: func(w *Writer) error {
				require.NoError(t, w.appendBlock(blocks[0].raw, &blocks[0].sum))
				return w.appendBlock(blocks[0].raw, &blocks[0].sum)
			},
			want: ErrSeqOrder,
		},
		{
			name: "records in two hours",
			run: func(w *Writer) error {
				s := blocks[0].sum.clone()
				s.tsMax += hourNs
				return w.appendBlock(blocks[0].raw, &s)
			},
		},
		{
			name: "classified record without classifier",
			opts: func(o *WriterOptions) { o.Meta.Classifier = nil },
			run:  func(w *Writer) error { return w.appendBlock(blocks[0].raw, &blocks[0].sum) },
			want: ErrMetadataCommitment,
		},
		{
			name: "oversized record without max_frame_len",
			run: func(w *Writer) error {
				s := blocks[0].sum.clone()
				s.decodeStatusCounts = countEnum(s.decodeStatusCounts, uint8(DecodeStatusOversized))
				return w.appendBlock(blocks[0].raw, &s)
			},
			want: ErrMetadataCommitment,
		},
		{
			name: "redacted record outside an extract",
			run: func(w *Writer) error {
				s := blocks[0].sum.clone()
				s.qualityUnion |= QualityRedacted
				return w.appendBlock(blocks[0].raw, &s)
			},
			want: ErrMetadataCommitment,
		},
		{
			name: "summary disagrees with the envelope",
			run: func(w *Writer) error {
				s := blocks[0].sum.clone()
				s.recordCount++
				return w.appendBlock(blocks[0].raw, &s)
			},
		},
		{
			name: "raw holds more than the envelope's body",
			run: func(w *Writer) error {
				return w.appendBlock(append(bytes.Clone(blocks[0].raw), 0), &blocks[0].sum)
			},
		},
		{
			name: "a block is open",
			run: func(w *Writer) error {
				rec := testDataRecord(blocks[0].sum.firstSeq, blockTestHour, 1)
				require.NoError(t, w.Append(&rec))
				return w.appendBlock(blocks[1].raw, &blocks[1].sum)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			opts := copyOptions(r, blocks, CodecZstd)
			if tt.opts != nil {
				tt.opts(&opts)
			}
			w, err := startWriter(io.Discard, opts)
			require.NoError(t, err)

			err = tt.run(w)
			require.Error(t, err)
			if tt.want != nil {
				require.ErrorIs(t, err, tt.want)
			}
			require.ErrorIs(t, w.appendBlock(blocks[1].raw, &blocks[1].sum), ErrWriterFailed, "the rejection failed the Writer")
			_, err = w.Close()
			require.ErrorIs(t, err, ErrWriterFailed)
		})
	}
}

func TestAppendBlockAfterClose(t *testing.T) {
	t.Parallel()

	r, blocks := readSourceBlocks(t, richFooterPack(t, CodecZstd).file)
	w, err := startWriter(io.Discard, copyOptions(r, blocks, CodecZstd))
	require.NoError(t, err)
	_, err = w.Close()
	require.NoError(t, err)
	require.ErrorIs(t, w.appendBlock(blocks[0].raw, &blocks[0].sum), ErrClosed)
}

func TestAppendBlockOutputFailures(t *testing.T) {
	t.Parallel()

	src := richFooterPack(t, CodecZstd).file
	r, blocks := readSourceBlocks(t, src)
	head := blocksStartOf(t, src)

	t.Run("write fails in a block", func(t *testing.T) {
		t.Parallel()

		fw := &failAfterWriter{limit: head + 10}
		w, err := startWriter(fw, copyOptions(r, blocks, CodecZstd))
		require.NoError(t, err)
		require.ErrorIs(t, w.appendBlock(blocks[0].raw, &blocks[0].sum), errTestWrite)
		require.ErrorIs(t, w.appendBlock(blocks[1].raw, &blocks[1].sum), ErrWriterFailed)
	})

	t.Run("sync fails after a block", func(t *testing.T) {
		t.Parallel()

		opts := copyOptions(r, blocks, CodecZstd)
		opts.Sync = &syncFailer{ok: 1}
		w, err := startWriter(io.Discard, opts)
		require.NoError(t, err)
		require.NoError(t, w.appendBlock(blocks[0].raw, &blocks[0].sum))
		require.ErrorIs(t, w.appendBlock(blocks[1].raw, &blocks[1].sum), errTestSync)
		_, err = w.Close()
		require.ErrorIs(t, err, ErrWriterFailed)
	})
}

// TestWriterFinalFailureMayLeaveAFinalizedPack shows that the error of the footer-and-trailer write,
// or of the sync after it, fails the Writer although the bytes that reached the output can already form a finalized pack.
func TestWriterFinalFailureMayLeaveAFinalizedPack(t *testing.T) {
	t.Parallel()

	src := richFooterPack(t, CodecZstd).file
	r, blocks := readSourceBlocks(t, src)

	t.Run("sync after the trailer fails", func(t *testing.T) {
		t.Parallel()

		var out bytes.Buffer
		opts := copyOptions(r, blocks, CodecZstd)
		opts.Sync = &syncFailer{ok: len(blocks)}
		w, err := startWriter(&out, opts)
		require.NoError(t, err)
		for i := range blocks {
			require.NoError(t, w.appendBlock(blocks[i].raw, &blocks[i].sum))
		}
		_, err = w.Close()
		require.ErrorIs(t, err, errTestSync)
		require.ErrorIs(t, w.appendBlock(blocks[0].raw, &blocks[0].sum), ErrWriterFailed)
		assert.Equal(t, OutcomeFinalizedConsistent, mustVerify(t, out.Bytes()).Outcome, "the trailer reached the output")
	})

	t.Run("the output takes the footer and trailer but reports an error", func(t *testing.T) {
		t.Parallel()

		fw := &failAfterWriter{limit: int(r.Header().Trailer.FooterOffset), takeAll: true}
		w, err := startWriter(fw, copyOptions(r, blocks, CodecZstd))
		require.NoError(t, err)
		for i := range blocks {
			require.NoError(t, w.appendBlock(blocks[i].raw, &blocks[i].sum))
		}
		_, err = w.Close()
		require.ErrorIs(t, err, errTestWrite)
		assert.Equal(t, OutcomeFinalizedConsistent, mustVerify(t, fw.buf.Bytes()).Outcome, "the trailer reached the output")
	})
}

func TestWriterRejectsAnOversizedBlockSummary(t *testing.T) {
	t.Parallel()

	src := richFooterPack(t, CodecZstd).file
	r, blocks := readSourceBlocks(t, src)

	var out bytes.Buffer
	w, err := startWriter(&out, copyOptions(r, blocks, CodecZstd))
	require.NoError(t, err)
	for i := range blocks {
		require.NoError(t, w.appendBlock(blocks[i].raw, &blocks[i].sum))
	}
	written := out.Len()
	w.maxF3ListLen = 10

	_, err = w.Close()
	require.ErrorContains(t, err, "u32 summary_len")
	assert.Equal(t, written, out.Len(), "neither footer nor trailer is written")
	_, err = w.Close()
	require.ErrorIs(t, err, ErrWriterFailed)
}

func TestSummaryLen32(t *testing.T) {
	t.Parallel()

	n, ok := summaryLen32(math.MaxUint32, math.MaxUint32)
	assert.True(t, ok)
	assert.Equal(t, uint32(math.MaxUint32), n)

	_, ok = summaryLen32(1<<32, math.MaxUint32)
	assert.False(t, ok, "2^32 does not fit summary_len")
	_, ok = summaryLen32(1<<32, 1<<40)
	assert.False(t, ok, "a limit above the u32 range is capped")
	_, ok = summaryLen32(11, 10)
	assert.False(t, ok, "a lower limit applies")
}

func TestNewWriterKeepsANilCaptureID(t *testing.T) {
	t.Parallel()

	meta := &PackMeta{
		ToolID: "tool", Recorder: "rec", Writer: "wr",
		PackRole: PackRoleSegment, ScopeGeneration: new(uint64(0)),
	}

	var kept bytes.Buffer
	w, err := startWriter(&kept, WriterOptions{Meta: meta})
	require.NoError(t, err)
	assert.True(t, w.CaptureID().IsZero())
	hdr, err := format.UnmarshalFileHeader(kept.Bytes())
	require.NoError(t, err)
	assert.Equal(t, format.UUID{}, hdr.CaptureID)

	w, err = NewWriter(io.Discard, WriterOptions{Meta: meta})
	require.NoError(t, err)
	assert.False(t, w.CaptureID().IsZero(), "NewWriter generates a capture id for the nil UUID")
}
