package tracepack

import (
	"math"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// loadTestPack is the index the held-loader tests give their Reader among the readers of a read,
// nonzero so a defect or error that misses it shows.
const loadTestPack = 2

// readAttempts is a heldLoader whose readHook counts the full-block reads of each block
// and keeps the buffers of the last read of each.
type readAttempts struct {
	heldLoader
	reads map[[2]int]int
	bufs  map[[2]int]*blockBuf
}

// newReadAttempts returns a readAttempts whose budget is limit.
func newReadAttempts(limit int64) *readAttempts {
	a := &readAttempts{reads: map[[2]int]int{}, bufs: map[[2]int]*blockBuf{}}
	a.budget.limit = limit
	a.readHook = func(pack, block int, buf *blockBuf) {
		a.reads[[2]int{pack, block}]++
		a.bufs[[2]int{pack, block}] = buf
	}

	return a
}

// bufCaps are the capacities and backing arrays of the buffers of a blockBuf allocated before its read:
// raw, rows and offs.
type bufCaps struct {
	caps  [3]int
	bases [3]any
}

// capsOf returns the capacities and backing arrays of raw, rows and offs of buf, each of capacity at least 1.
func capsOf(buf *blockBuf) bufCaps {
	return bufCaps{
		caps:  [3]int{cap(buf.raw), cap(buf.rows), cap(buf.offs)},
		bases: [3]any{&buf.raw[:1][0], &buf.rows[:1][0], &buf.offs[:1][0]},
	}
}

// requireDropped requires the buffers of buf to have been dropped.
func requireDropped(t *testing.T, buf *blockBuf) {
	t.Helper()

	require.NotNil(t, buf)
	assert.Nil(t, buf.raw)
	assert.Nil(t, buf.decoded)
	assert.Nil(t, buf.rows)
	assert.Nil(t, buf.offs)
}

// hugeBlock is the block hugeSectionPack makes state 2^24 records, its last.
const hugeBlock = 2

// hugeSectionPack returns a pack of three hourly blocks
// whose last states 2^24 records of 44 bytes in an uncompressed_len of 44:
// in its F-2 entry and F-3 summary when indexed,
// kept consistent with the rest of the footer and the trailer so the footer is used;
// else in its envelope, with the pack left unfinalized so the forward walk finds it.
func hugeSectionPack(t testing.TB, indexed bool) []byte {
	t.Helper()

	const count = 1 << 24
	recs := hourRecords(3, 40)
	if !indexed {
		open := writeReaderPack(t, readerPackConfig{codec: CodecZstd, open: true}, recs)

		return patchEnvelope(t, open.file, open.blocks[hugeBlock].offset, func(e *format.BlockEnvelope) {
			e.RecordCount, e.UncompressedLen = count, format.RecordHeaderLen
		})
	}

	closed := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, recs)
	extra := count - closed.blocks[hugeBlock].recordCount
	// addTo adds extra to the first nonzero count, so the counts sum to record_count again.
	addTo := func(counts []uint32) {
		k := slices.IndexFunc(counts, func(n uint32) bool { return n > 0 })
		counts[k] += extra
	}
	file := reindexedWith(t, closed.file, func(i int, s *blockSummary) {
		if i != hugeBlock {
			return
		}
		s.recordCount, s.uncompressedLen = count, format.RecordHeaderLen
		s.lastSeq += uint64(extra)
		addTo(s.kindCounts)
		addTo(s.dirCounts)
		addTo(s.decodeStatusCounts)
		require.Len(t, s.epochs, 1)
		s.epochs[0].recordCount += uint64(extra)
		s.epochs[0].seqLast = s.lastSeq
		for k := range s.seqRanges {
			s.seqRanges[k].last = s.lastSeq
		}
	})
	tr := layoutOf(t, file).tr
	tr.RecordCount += uint64(extra)
	tr.LastSeq += uint64(extra)

	return format.AppendTrailer(file[:len(file)-format.TrailerLen], &tr)
}

func TestHeldBudget(t *testing.T) {
	t.Parallel()

	b := heldBudget{limit: 100}
	require.NoError(t, b.reserve(60))
	require.NoError(t, b.reserve(40), "reaching the limit exactly is admitted")
	assert.Equal(t, int64(100), b.held)

	err := b.reserve(1)
	require.ErrorIs(t, err, ErrReadLimit)
	require.ErrorContains(t, err, "holding 1 bytes more beside the 100 held exceeds MaxHeldBytes 100")
	assert.Equal(t, int64(100), b.held, "a failed reservation holds nothing more")

	b.release(40)
	require.ErrorIs(t, b.reserve(41), ErrReadLimit, "one byte over the limit")
	assert.Equal(t, int64(60), b.held)
	require.ErrorIs(t, b.reserve(math.MaxInt64), ErrReadLimit, "a reservation that would overflow")
	require.ErrorIs(t, b.reserve(-1), ErrReadLimit, "a negative reservation")
	assert.Equal(t, int64(60), b.held)

	b.release(60)
	assert.Zero(t, b.held)
}

func TestPreflightBlockSizes(t *testing.T) {
	t.Parallel()

	p := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, hourRecords(3, 40))
	r := mustOpen(t, p.file, ReaderOptions{})
	for i := range r.blocks {
		info := &r.blocks[i]
		s, def := r.preflightBlock(i)
		require.Nil(t, def)

		rows := int(info.RecordCount) * int(info.RecordHeaderLen)
		offs := int(info.RecordCount) + 1
		assert.Equal(t, blockSizes{
			raw:     int(info.OnDiskLen),
			decoded: int(info.UncompressedLen),
			rows:    rows,
			offs:    offs,
			total:   int64(info.OnDiskLen) + int64(info.UncompressedLen) + int64(rows) + 4*int64(offs),
		}, s, "block %d", i)
	}
}

func TestHeldLoaderPreflightDefect(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		file    func(t testing.TB) []byte
		opts    ReaderOptions
		block   int
		indexed bool
		reason  IncompleteReason
		msg     string
	}{
		{name: "huge header section, indexed", file: func(t testing.TB) []byte { return hugeSectionPack(t, true) },
			block: hugeBlock, indexed: true, reason: ReasonCorruptBlock, msg: "header section of 738197504 bytes exceeds uncompressed_len 44"},
		{name: "huge header section, walked", file: func(t testing.TB) []byte { return hugeSectionPack(t, false) },
			block: hugeBlock, reason: ReasonCorruptBlock, msg: "header section of 738197504 bytes exceeds uncompressed_len 44"},
		{name: "over MaxBlockLen", file: func(t testing.TB) []byte {
			// Block 1 holds records of 4 KiB payloads, blocks 0 and 2 of 1 byte.
			recs := hourRecords(3, 40)
			for i := 40; i < 80; i++ {
				recs[i] = randomDataRecord(recs[i].Seq, recs[i].TSUTCNs, 4<<10)
			}

			return writeReaderPack(t, readerPackConfig{codec: CodecZstd}, recs).file
		}, opts: ReaderOptions{MaxBlockLen: 64 << 10}, block: 1, indexed: true, reason: ReasonLimit, msg: "exceeds MaxBlockLen"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := mustOpen(t, tt.file(t), tt.opts)
			require.Len(t, r.blocks, 3)
			require.Equal(t, tt.indexed, r.blocks[tt.block].Indexed, "the footer must be used exactly when indexed: %v", r.Header().FooterErr)

			l := newReadAttempts(math.MaxInt64)
			h, def, err := l.load(r, loadTestPack, tt.block)
			require.NoError(t, err)
			require.Nil(t, h)
			require.NotNil(t, def)
			assert.Equal(t, tt.reason, def.Reason, "%v", def.Err)
			assert.Equal(t, loadTestPack, def.Pack)
			assert.Equal(t, tt.block, def.Block)
			assert.Equal(t, int64(r.blocks[tt.block].Offset), def.Offset)
			require.ErrorContains(t, def.Err, tt.msg)
			if tt.reason == ReasonLimit {
				require.ErrorIs(t, def.Err, ErrReadLimit)
			}
			assert.Zero(t, l.budget.held, "nothing reserved")
			assert.Zero(t, l.reads[[2]int{loadTestPack, tt.block}], "no read attempt")

			// The other blocks still load.
			for i := range r.blocks {
				if i == tt.block {
					continue
				}
				h, def, err := l.load(r, loadTestPack, i)
				require.NoError(t, err)
				require.Nil(t, def, "block %d", i)
				requireBlockConsistent(t, &r.blocks[i], h.d)
				l.drop(h)
				assert.Equal(t, 1, l.reads[[2]int{loadTestPack, i}], "block %d", i)
			}
			assert.Zero(t, l.budget.held)
		})
	}
}

func TestHeldLoaderValidBlock(t *testing.T) {
	t.Parallel()

	for _, c := range []Codec{CodecNone, CodecZstd} {
		t.Run(c.String(), func(t *testing.T) {
			t.Parallel()

			p := writeReaderPack(t, readerPackConfig{codec: c}, hourRecords(3, 40))
			r := mustOpen(t, p.file, ReaderOptions{})
			l := newReadAttempts(math.MaxInt64)
			var before bufCaps
			hook := l.readHook
			l.readHook = func(pack, block int, buf *blockBuf) {
				hook(pack, block, buf)
				before = capsOf(buf)
				assert.Nil(t, buf.decoded, "decoded is allocated only by the decode")
			}

			h, def, err := l.load(r, loadTestPack, 1)
			require.NoError(t, err)
			require.Nil(t, def)
			require.NotNil(t, h)
			assert.Equal(t, loadTestPack, h.pack)
			assert.Equal(t, 1, h.block)
			assert.Equal(t, 1, l.reads[[2]int{loadTestPack, 1}])
			requireBlockConsistent(t, &r.blocks[1], h.d)

			s, _ := r.preflightBlock(1)
			assert.Equal(t, [3]int{s.raw, s.rows, s.offs}, before.caps, "allocated at exactly the reserved sizes")
			assert.Equal(t, before, capsOf(h.buf), "the read grew no buffer")
			if c == CodecNone {
				assert.Nil(t, h.buf.decoded, "a None block never allocates decoded")
			} else {
				assert.Equal(t, s.decoded, cap(h.buf.decoded), "decoded allocated at exactly uncompressed_len")
			}
			assert.Equal(t, s.total, h.size)
			assert.Equal(t, h.size, l.budget.held)

			// The block aliases its held buffers: a None body is used in place in raw, any other in decoded.
			assert.Same(t, &h.buf.rows[0], &h.d.section[0])
			assert.Same(t, &h.buf.offs[0], &h.d.offs[0])
			if c == CodecNone {
				assert.Same(t, &h.buf.raw[format.EnvelopeLen], &h.d.body[0])
			} else {
				assert.Same(t, &h.buf.decoded[0], &h.d.body[0])
			}

			buf := h.buf
			l.drop(h)
			requireDropped(t, buf)
			assert.Nil(t, h.d)
			assert.Zero(t, h.size)
			assert.Zero(t, l.budget.held)
			l.drop(h)
			assert.Zero(t, l.budget.held, "a second drop releases nothing")
		})
	}
}

func TestHeldLoaderBudget(t *testing.T) {
	t.Parallel()

	p := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, hourRecords(3, 40))
	r := mustOpen(t, p.file, ReaderOptions{})
	s, def := r.preflightBlock(1)
	require.Nil(t, def)

	l := newReadAttempts(s.total - 1)
	h, def, err := l.load(r, loadTestPack, 1)
	require.ErrorIs(t, err, ErrReadLimit)
	require.ErrorContains(t, err, "pack 2, block 1")
	assert.Nil(t, h)
	assert.Nil(t, def)
	assert.Zero(t, l.budget.held, "nothing reserved")
	assert.Zero(t, l.reads[[2]int{loadTestPack, 1}], "no read attempt")

	l = newReadAttempts(s.total)
	h, def, err = l.load(r, loadTestPack, 1)
	require.NoError(t, err, "a block exactly the size of the budget is admitted")
	require.Nil(t, def)
	assert.Equal(t, s.total, l.budget.held)

	_, _, err = l.load(r, 0, 0)
	require.ErrorIs(t, err, ErrReadLimit, "the budget is held by block 1 of the other pack")
	require.ErrorContains(t, err, "pack 0, block 0")
	l.drop(h)
	assert.Zero(t, l.budget.held)
}

func TestHeldLoaderFailedBlock(t *testing.T) {
	t.Parallel()

	p := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, hourRecords(3, 40))
	b := p.blocks[1]
	file := flipByte(p.file, b.offset+format.EnvelopeLen+uint64(b.onDiskLen-format.EnvelopeLen)/2)
	r := mustOpen(t, file, ReaderOptions{})
	require.True(t, r.blocks[1].Indexed)

	l := newReadAttempts(math.MaxInt64)
	h, def, err := l.load(r, loadTestPack, 1)
	require.NoError(t, err)
	assert.Nil(t, h)
	require.NotNil(t, def)
	assert.Equal(t, ReasonCorruptBlock, def.Reason)
	assert.Equal(t, loadTestPack, def.Pack)
	assert.Equal(t, 1, def.Block)
	require.ErrorContains(t, def.Err, "body_crc")
	assert.Equal(t, 1, l.reads[[2]int{loadTestPack, 1}])
	requireDropped(t, l.bufs[[2]int{loadTestPack, 1}])
	assert.Zero(t, l.budget.held)
}

func TestHeldLoaderBodyDefect(t *testing.T) {
	t.Parallel()

	closed := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, hourRecords(3, 40))
	tests := []struct {
		name string
		edit func(b *testBlock)
		msg  string
	}{
		{name: "zstd frame cut short", edit: func(b *testBlock) {
			b.onDisk = func(enc []byte) []byte { return enc[:len(enc)-1] }
		}, msg: "decode body"},
		{name: "payload_len one too large", edit: func(b *testBlock) { b.headers[len(b.headers)-1].PayloadLen++ },
			msg: "past uncompressed_len"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			file := rebuildPack(t, closed.file, func(i int, b *testBlock) {
				if i == 1 {
					tt.edit(b)
				}
			})
			r := mustOpen(t, file, ReaderOptions{})
			require.NoError(t, r.Header().FooterErr, "the footer must stay valid")

			l := newReadAttempts(math.MaxInt64)
			h, def, err := l.load(r, loadTestPack, 1)
			require.NoError(t, err)
			assert.Nil(t, h)
			require.NotNil(t, def)
			assert.Equal(t, ReasonCorruptBlock, def.Reason)
			assert.Equal(t, loadTestPack, def.Pack)
			require.ErrorContains(t, def.Err, tt.msg)
			assert.Equal(t, 1, l.reads[[2]int{loadTestPack, 1}])
			requireDropped(t, l.bufs[[2]int{loadTestPack, 1}])
			assert.Zero(t, l.budget.held)
		})
	}
}

func TestHeldLoaderIndexMismatch(t *testing.T) {
	t.Parallel()

	file, _ := indexMismatchPack(t, func(b *testBlock) { b.headers[0].TSUTCNs++ })
	r := mustOpen(t, file, ReaderOptions{})
	require.NoError(t, r.Header().FooterErr, "the footer must stay valid")

	l := newReadAttempts(math.MaxInt64)
	h, def, err := l.load(r, loadTestPack, 1)
	require.NoError(t, err)
	require.NotNil(t, h, "a block disagreeing with its F-2 entry is still usable")
	require.NotNil(t, def)
	assert.Equal(t, ReasonIndexMismatch, def.Reason)
	assert.Equal(t, loadTestPack, def.Pack)
	assert.Equal(t, 1, def.Block)
	require.ErrorContains(t, def.Err, "ts_min")
	assert.Equal(t, h.size, l.budget.held)

	l.drop(h)
	assert.Zero(t, l.budget.held)
}

func TestHeldLoaderReadError(t *testing.T) {
	t.Parallel()

	p := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, hourRecords(3, 40))
	g := newGatedReader(p.file)
	r, err := openReader(t.Context(), g, int64(len(p.file)), ReaderOptions{}, false)
	require.NoError(t, err)
	at := int64(r.blocks[1].Offset)
	g.fail = func(off int64, _ int) bool { return off == at }

	l := newReadAttempts(math.MaxInt64)
	h, def, err := l.load(r, loadTestPack, 1)
	require.ErrorIs(t, err, errInjected)
	require.ErrorContains(t, err, "tracepack: pack 2, block 1:")
	assert.Nil(t, h)
	assert.Nil(t, def)
	assert.Equal(t, 1, l.reads[[2]int{loadTestPack, 1}])
	requireDropped(t, l.bufs[[2]int{loadTestPack, 1}])
	assert.Zero(t, l.budget.held)

	g.fail = nil
	h, def, err = l.load(r, loadTestPack, 1)
	require.NoError(t, err)
	require.Nil(t, def)
	assert.Equal(t, 2, l.reads[[2]int{loadTestPack, 1}], "every read attempt counts")
	l.drop(h)
	assert.Zero(t, l.budget.held)
}
