package tracepack

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"math/rand/v2"
	"slices"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/format"
	"github.com/arloliu/go-secs/tracepack/internal/tlv"
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

	return r, readBlocksOf(t, r)
}

// readVerbatimSource opens file keeping a valid footer's F-3 section, as a merge does,
// and reads every block in full as readSourceBlocks does.
func readVerbatimSource(t testing.TB, file []byte) (*Reader, []sourceBlock) {
	t.Helper()

	r := mustOpenKeepingF3(t, file, ReaderOptions{})
	require.NoError(t, r.Header().FooterErr)

	return r, readBlocksOf(t, r)
}

// readBlocksOf reads every located block of the pack r opened in full, requiring each to be read.
func readBlocksOf(t testing.TB, r *Reader) []sourceBlock {
	t.Helper()

	var buf blockBuf
	out := make([]sourceBlock, len(r.blocks))
	for i := range r.blocks {
		d, def, err := r.readBlock(i, &buf)
		require.NoError(t, err)
		require.NotNil(t, d, "block %d fails: %v", i, def)
		out[i] = sourceBlock{raw: bytes.Clone(buf.raw), sum: summaryOf(d)}
	}

	return out
}

// copyOptions returns WriterOptions for a pack that copies blocks from the pack r opened:
// its metadata with seq_start at the first block's first seq, its pack and capture ids, and footer codec c.
func copyOptions(r *Reader, blocks []sourceBlock, c Codec) WriterOptions {
	h := r.Header()
	if len(blocks) > 0 {
		h.Meta.SeqStart = blocks[0].sum.firstSeq
	}

	// The source packs have no period and span UTC hours, so the Writer's scope checks are off.
	return WriterOptions{Meta: h.Meta, Codec: c, PackID: h.PackID, CaptureID: h.CaptureID, allowScopeBreach: true}
}

// copyBlocks writes blocks into a new pack with opts and returns it.
func copyBlocks(t testing.TB, opts WriterOptions, blocks []sourceBlock) []byte {
	t.Helper()

	return copyBlocksWith(t, opts, blocks, nil)
}

// copyBlocksWith is copyBlocks with lists[i] as block i's verbatim F-3 list, or none when lists is nil.
func copyBlocksWith(t testing.TB, opts WriterOptions, blocks []sourceBlock, lists [][]byte) []byte {
	t.Helper()

	var out bytes.Buffer
	w, err := startWriter(&out, opts)
	require.NoError(t, err)
	for i := range blocks {
		var list []byte
		if lists != nil {
			list = lists[i]
		}
		require.NoError(t, w.appendBlock(blocks[i].raw, &blocks[i].sum, list), "block %d", i)
	}
	_, err = w.Close()
	require.NoError(t, err)

	return out.Bytes()
}

// rawF3Lists returns the F-3 list of every block of the pack r opened, as rawF3 gives it.
func rawF3Lists(r *Reader) [][]byte {
	out := make([][]byte, len(r.Blocks()))
	for i := range out {
		out[i] = r.rawF3(i)
	}

	return out
}

// unusualF3Pack returns the rich footer test pack written with codec c,
// its footer rebuilt with every block's F-3 list changed by unusualF3List, and the rebuilt decoded footer.
func unusualF3Pack(t testing.TB, c Codec) ([]byte, []byte) {
	t.Helper()

	base := richFooterPack(t, c)
	parts := splitFooter(t, base.decoded)
	for i := range parts.f3 {
		parts.f3[i] = unusualF3List(t, parts.f3[i])
	}
	decoded := parts.encode()

	return refooter(t, base.file, decoded), decoded
}

// footerOf returns the decoded footer of the finalized pack file.
func footerOf(t testing.TB, file []byte) []byte {
	t.Helper()

	_, decoded, _ := splitPack(t, file)

	return decoded
}

// f5Of returns the F-5 section of the decoded footer.
func f5Of(t testing.TB, decoded []byte) []byte {
	t.Helper()

	pro, err := format.UnmarshalFooterPrologue(decoded)
	require.NoError(t, err)

	return decoded[pro.F5Offset : pro.F5Offset+pro.F5Len]
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
			cp := copyBlocks(t, copyOptions(r, blocks, Codec(layoutOf(t, src).tr.FooterCodec)), blocks)

			rep := mustVerify(t, cp)
			assert.Equal(t, OutcomeFinalizedConsistent, rep.Outcome, "%s %s", c, name)
			assert.Equal(t, len(blocks), rep.Validated, "%s %s", c, name)
			// Same metadata, so the same offsets: blocks, footer and trailer are the source's byte for byte.
			start := int(layoutOf(t, src).blocksStart)
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
	start := int(layoutOf(t, src).blocksStart)
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
		require.NoError(t, w.appendBlock(blocks[i].raw, &s, nil))
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

	start := int(layoutOf(t, src).blocksStart)
	assert.Equal(t, src[start:], out.Bytes()[start:])
}

// TestAppendBlockCopiesVerbatimF3Lists copies a pack block by block, each block with its F-3 list as stored,
// and the lists hold what no Writer builds: unknown, reserved, retired and nested unknown tags, reordered entries.
// The copy keeps every list byte for byte (the tracepack format specification §10: a copied F-3 list is copied verbatim).
// The lists alias the source Reader's F-3 section;
// reading the source again before Close leaves the copy's lists as they were.
func TestAppendBlockCopiesVerbatimF3Lists(t *testing.T) {
	t.Parallel()

	for _, c := range []Codec{CodecNone, CodecZstd} {
		t.Run(c.String(), func(t *testing.T) {
			t.Parallel()

			src, decoded := unusualF3Pack(t, c)
			r, blocks := readVerbatimSource(t, src)

			var out bytes.Buffer
			w, err := startWriter(&out, copyOptions(r, blocks, c))
			require.NoError(t, err)
			for i := range blocks {
				require.NoError(t, w.appendBlock(blocks[i].raw, &blocks[i].sum, r.rawF3(i)), "block %d", i)
			}
			readBlocksOf(t, r)
			iterate(t, r, Query{})
			_, err = w.Close()
			require.NoError(t, err)
			assert.Zero(t, w.footerCounted, "nothing is counted without a footer budget")

			cp := out.Bytes()
			assert.Equal(t, OutcomeFinalizedConsistent, mustVerify(t, cp).Outcome)
			got := footerOf(t, cp)
			assert.Equal(t, splitFooter(t, decoded).f3Raw, splitFooter(t, got).f3Raw, "every F-3 list as the source stores it")
			assert.Equal(t, f5Of(t, decoded), f5Of(t, got), "F-5 aggregates the blocks' summaries")
			// The same metadata, and the lists laid out in block order as in the source:
			// blocks, footer and trailer are the source's byte for byte.
			start := int(layoutOf(t, src).blocksStart)
			assert.Equal(t, src[start:], cp[start:])
		})
	}
}

// TestAppendBlockCopiesOverlappingF3Lists checks that each list is copied whole, one after another,
// when the source's F-2 entries locate overlapping lists in its F-3 section.
func TestAppendBlockCopiesOverlappingF3Lists(t *testing.T) {
	t.Parallel()

	src, want := overlappingF3Pack(t)
	r, blocks := readVerbatimSource(t, src)
	cp := copyBlocksWith(t, copyOptions(r, blocks, CodecZstd), blocks, rawF3Lists(r))

	assert.Equal(t, OutcomeFinalizedConsistent, mustVerify(t, cp).Outcome)
	got := footerOf(t, cp)
	assert.Equal(t, want, splitFooter(t, got).f3Raw)
	assert.Equal(t, f5Of(t, footerOf(t, src)), f5Of(t, got))
}

// TestAppendBlockOwnsItsF3List checks that the Writer keeps its own copy of a verbatim F-3 list:
// the caller's later changes to its slice do not reach the footer.
func TestAppendBlockOwnsItsF3List(t *testing.T) {
	t.Parallel()

	src, decoded := unusualF3Pack(t, CodecZstd)
	r, blocks := readVerbatimSource(t, src)

	var out bytes.Buffer
	w, err := startWriter(&out, copyOptions(r, blocks, CodecZstd))
	require.NoError(t, err)
	for i := range blocks {
		list := bytes.Clone(r.rawF3(i))
		require.NoError(t, w.appendBlock(blocks[i].raw, &blocks[i].sum, list))
		for j := range list {
			list[j] = 0xFF
		}
	}
	_, err = w.Close()
	require.NoError(t, err)

	assert.Equal(t, splitFooter(t, decoded).f3Raw, splitFooter(t, footerOf(t, out.Bytes())).f3Raw)
}

func TestFooterCost(t *testing.T) {
	t.Parallel()

	// Block 0 of the rich pack holds two epochs, one boundary and two seq ranges.
	s := &richFooterPack(t, CodecZstd).blocks[0]
	require.Len(t, s.epochs, 2)
	require.Len(t, s.boundaries, 1)
	require.Len(t, s.seqRanges, 2)
	// The kept summary and its F-2 index entry, then the summary's contents.
	kept := keptSummaryLen + keptF2EntryLen +
		2*keptEpochLen + keptBoundaryLen + 2*keptSeqRangeLen +
		(len(s.kindCounts)+len(s.dirCounts)+len(s.decodeStatusCounts))*keptCountLen

	built := len(appendF3(nil, s))
	assert.Equal(t, uint64(format.F2EntryLen+built+kept), footerCost(s, nil), "the list appendF3 builds, which the Writer does not keep")
	assert.Equal(t, uint64(format.F2EntryLen+2*1000+kept), footerCost(s, make([]byte, 1000)),
		"a verbatim list, counted in the footer and as the Writer's copy")
}

// TestFooterCostEstimates checks that no estimate footerCost counts is below the size of the type it stands for,
// so an estimate cannot fall behind a type that grows.
// The estimates have no headroom on purpose: each equals its type's size on a 64-bit platform,
// so a field that grows a counted struct fails this test until its constant is updated.
func TestFooterCostEstimates(t *testing.T) {
	t.Parallel()

	var gap int64
	assert.LessOrEqual(t, unsafe.Sizeof(blockSummary{}), uintptr(keptSummaryLen), "a kept block summary")
	assert.LessOrEqual(t, unsafe.Sizeof(format.F2Entry{}), uintptr(keptF2EntryLen), "the F-2 entry buildFooter builds per block")
	assert.LessOrEqual(t, unsafe.Sizeof(epochSummary{}), uintptr(keptEpochLen), "an epoch entry")
	assert.LessOrEqual(t, unsafe.Sizeof(boundarySummary{})+2*unsafe.Sizeof(gap), uintptr(keptBoundaryLen), "a boundary entry and its gap values")
	assert.LessOrEqual(t, unsafe.Sizeof(seqRange{}), uintptr(keptSeqRangeLen), "a seq range")
	assert.LessOrEqual(t, unsafe.Sizeof(uint32(0)), uintptr(keptCountLen), "a count array element")
}

// bigF3Pack returns the rich footer test pack whose block 1 F-3 list carries an unknown entry of n bytes.
func bigF3Pack(t testing.TB, n int) []byte {
	t.Helper()

	base := richFooterPack(t, CodecZstd)
	parts := splitFooter(t, base.decoded)
	parts.f3[1] = append(slices.Clone(parts.f3[1]), tlv.BytesEntry(0x0100, make([]byte, n)))

	return refooter(t, base.file, parts.encode())
}

// TestAppendBlockFooterBudget checks that appendBlock counts every block against the Writer's footer budget,
// and rejects a block over it with ErrMergeLimit
// before it writes or keeps anything of it, a large verbatim F-3 list included.
func TestAppendBlockFooterBudget(t *testing.T) {
	t.Parallel()

	src := bigF3Pack(t, 1<<20)
	r, blocks := readVerbatimSource(t, src)
	lists := rawF3Lists(r)
	opts := copyOptions(r, blocks, CodecZstd)

	costs := make([]uint64, len(blocks))
	for i := range blocks {
		costs[i] = footerCost(&blocks[i].sum, lists[i])
	}
	require.Greater(t, costs[1], uint64(2<<20), "block 1's list counts in the footer and as the Writer's copy")

	t.Run("a block over the budget", func(t *testing.T) {
		t.Parallel()

		var out bytes.Buffer
		w, err := startWriter(&out, opts)
		require.NoError(t, err)
		w.footerBudget = costs[0] + costs[1] - 1
		require.NoError(t, w.appendBlock(blocks[0].raw, &blocks[0].sum, lists[0]))
		written, counted := out.Len(), w.footerCounted
		require.Equal(t, costs[0], counted)

		require.ErrorIs(t, w.appendBlock(blocks[1].raw, &blocks[1].sum, lists[1]), ErrMergeLimit)
		assert.Equal(t, written, out.Len(), "the block is not written")
		assert.Len(t, w.blocks, 1, "nothing of the block is kept")
		assert.Equal(t, counted, w.footerCounted, "the block is not counted")

		require.ErrorIs(t, w.appendBlock(blocks[1].raw, &blocks[1].sum, nil), ErrWriterFailed)
		_, err = w.Close()
		require.ErrorIs(t, err, ErrWriterFailed)
		assert.Equal(t, written, out.Len(), "neither footer nor trailer is written")
	})

	t.Run("blocks that fill the budget", func(t *testing.T) {
		t.Parallel()

		w, err := startWriter(io.Discard, opts)
		require.NoError(t, err)
		w.footerBudget = costs[0] + costs[1]
		require.NoError(t, w.appendBlock(blocks[0].raw, &blocks[0].sum, lists[0]))
		require.NoError(t, w.appendBlock(blocks[1].raw, &blocks[1].sum, lists[1]))
		assert.Equal(t, w.footerBudget, w.footerCounted)
		require.ErrorIs(t, w.appendBlock(blocks[2].raw, &blocks[2].sum, nil), ErrMergeLimit, "a block without a verbatim list counts too")
	})

	t.Run("a budget that holds the pack", func(t *testing.T) {
		t.Parallel()

		var total uint64
		for _, c := range costs {
			total += c
		}
		var out bytes.Buffer
		w, err := startWriter(&out, opts)
		require.NoError(t, err)
		w.footerBudget = max(total, layoutOf(t, src).tr.FooterUncompressedLen)
		for i := range blocks {
			require.NoError(t, w.appendBlock(blocks[i].raw, &blocks[i].sum, lists[i]))
		}
		_, err = w.Close()
		require.NoError(t, err)
		assert.Equal(t, total, w.footerCounted)

		start := int(layoutOf(t, src).blocksStart)
		assert.Equal(t, src[start:], out.Bytes()[start:], "a budget changes no byte of the pack")
	})

	t.Run("no budget", func(t *testing.T) {
		t.Parallel()

		cp := copyBlocksWith(t, opts, blocks, lists)
		start := int(layoutOf(t, src).blocksStart)
		assert.Equal(t, src[start:], cp[start:])
	})
}

// manyEpochPack returns a finalized pack of one block of n data records, each in an epoch of its own.
func manyEpochPack(t testing.TB, n int) []byte {
	t.Helper()

	meta := &PackMeta{
		ToolID: "tool", Recorder: "rec", Writer: "wr", Classifiers: []string{"c"},
		PackRole: PackRoleSegment, ScopeGeneration: new(uint64(0)),
		PeriodStart: blockTestHour, PeriodEnd: blockTestHour + hourNs,
	}
	var buf bytes.Buffer
	w, err := NewWriter(&buf, WriterOptions{Meta: meta, Codec: CodecZstd})
	require.NoError(t, err)
	for i := range n {
		rec := testDataRecord(uint64(i), blockTestHour+int64(i), uint32(i))
		require.NoError(t, w.Append(&rec))
	}
	_, err = w.Close()
	require.NoError(t, err)
	require.Len(t, w.blocks, 1)

	return buf.Bytes()
}

// TestWriterCloseFooterBudget checks that Close computes the footer's length before it assembles the footer
// and rejects a footer longer than the Writer's footer budget with ErrMergeLimit, writing neither footer nor trailer:
// also when the block's count is within the budget,
// and only the F-1 prologue and F-5 take the footer over it.
// The block holds 16 epochs: each adds an entry to F-3 and one to F-5,
// but to the block's count only its F-3 entry and the smaller estimate of the summary's epoch entry,
// so the footer is well past the count.
func TestWriterCloseFooterBudget(t *testing.T) {
	t.Parallel()

	r, one := readSourceBlocks(t, manyEpochPack(t, 16))

	// run copies blocks under budget and returns the output, its length before Close, and Close's error.
	run := func(t *testing.T, blocks []sourceBlock, budget uint64) ([]byte, int, error) {
		t.Helper()

		var out bytes.Buffer
		w, err := startWriter(&out, copyOptions(r, blocks, CodecZstd))
		require.NoError(t, err)
		w.footerBudget = budget
		for i := range blocks {
			require.NoError(t, w.appendBlock(blocks[i].raw, &blocks[i].sum, nil))
		}
		written := out.Len()
		_, err = w.Close()
		if err != nil {
			_, again := w.Close()
			require.ErrorIs(t, again, ErrWriterFailed)
		}

		return out.Bytes(), written, err
	}

	counted := footerCost(&one[0].sum, nil)
	full := copyBlocks(t, copyOptions(r, one, CodecZstd), one)
	footerLen := layoutOf(t, full).tr.FooterUncompressedLen
	require.Greater(t, footerLen, counted, "the prologue and F-5 take the footer past what the block counted")

	empty := copyBlocks(t, copyOptions(r, nil, CodecZstd), nil)
	emptyLen := layoutOf(t, empty).tr.FooterUncompressedLen

	tests := []struct {
		name   string
		blocks []sourceBlock
		budget uint64
		want   []byte
	}{
		{name: "F-5 takes the footer over the budget", blocks: one, budget: counted},
		{name: "the footer one byte over the budget", blocks: one, budget: footerLen - 1},
		{name: "the footer fills the budget", blocks: one, budget: footerLen, want: full},
		{name: "a budget below an empty footer", budget: emptyLen - 1},
		{name: "an empty footer fills the budget", budget: emptyLen, want: empty},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			out, written, err := run(t, tt.blocks, tt.budget)
			if tt.want == nil {
				require.ErrorIs(t, err, ErrMergeLimit)
				assert.Len(t, out, written, "neither footer nor trailer is written")

				return
			}

			require.NoError(t, err)
			start := int(layoutOf(t, tt.want).blocksStart)
			assert.Equal(t, tt.want[start:], out[start:])
		})
	}
}

// TestWriterCloseEncodedFooterBudget checks that Close also checks the footer's encoded length against the budget
// before it writes the footer, since a reader's MaxFooterLen bounds the footer on disk as well as decoded.
// The source's one block carries 64 KiB of random bytes as an unknown entry of its F-3 list,
// which zstd stores as they are, so the zstd footer is a few bytes longer than the decoded one.
// The budget is set only after the block is appended:
// what the block counts, its verbatim list twice among it, would otherwise reject the block before Close.
func TestWriterCloseEncodedFooterBudget(t *testing.T) {
	t.Parallel()

	random := make([]byte, 64<<10)
	rng := rand.New(rand.NewPCG(1, 2))
	for i := range random {
		random[i] = byte(rng.Uint32())
	}
	base := manyEpochPack(t, 1)
	parts := splitFooter(t, footerOf(t, base))
	parts.f3[0] = append(slices.Clone(parts.f3[0]), tlv.BytesEntry(0x0100, random))
	src := refooter(t, base, parts.encode())
	tr := layoutOf(t, src).tr
	require.Greater(t, tr.FooterLen, tr.FooterUncompressedLen, "the zstd footer is longer than the decoded one")

	r, blocks := readVerbatimSource(t, src)
	lists := rawF3Lists(r)

	tests := []struct {
		name   string
		budget uint64
		ok     bool
	}{
		{name: "the decoded footer fills the budget", budget: tr.FooterUncompressedLen},
		{name: "the encoded footer one byte over the budget", budget: tr.FooterLen - 1},
		{name: "the encoded footer fills the budget", budget: tr.FooterLen, ok: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var out bytes.Buffer
			w, err := startWriter(&out, copyOptions(r, blocks, CodecZstd))
			require.NoError(t, err)
			require.NoError(t, w.appendBlock(blocks[0].raw, &blocks[0].sum, lists[0]))
			written := out.Len()
			w.footerBudget = tt.budget

			_, err = w.Close()
			if !tt.ok {
				require.ErrorIs(t, err, ErrMergeLimit)
				require.ErrorContains(t, err, "on disk")
				assert.Equal(t, written, out.Len(), "neither footer nor trailer is written")

				return
			}

			require.NoError(t, err)
			start := int(layoutOf(t, src).blocksStart)
			assert.Equal(t, src[start:], out.Bytes()[start:])
		})
	}
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
			run:  func(w *Writer) error { return w.appendBlock(blocks[0].raw, &blocks[0].sum, nil) },
			want: ErrSeqOrder,
		},
		{
			name: "first seq not above the last seq written",
			run: func(w *Writer) error {
				require.NoError(t, w.appendBlock(blocks[0].raw, &blocks[0].sum, nil))
				return w.appendBlock(blocks[0].raw, &blocks[0].sum, nil)
			},
			want: ErrSeqOrder,
		},
		{
			name: "records in two hours",
			run: func(w *Writer) error {
				s := blocks[0].sum.clone()
				s.tsMax += hourNs
				return w.appendBlock(blocks[0].raw, &s, nil)
			},
		},
		{
			name: "classified record without classifier",
			opts: func(o *WriterOptions) { o.Meta.Classifiers = nil },
			run:  func(w *Writer) error { return w.appendBlock(blocks[0].raw, &blocks[0].sum, nil) },
			want: ErrMetadataCommitment,
		},
		{
			name: "oversized record without max_frame_len",
			run: func(w *Writer) error {
				s := blocks[0].sum.clone()
				s.decodeStatusCounts = countEnum(s.decodeStatusCounts, uint8(DecodeStatusOversized))
				return w.appendBlock(blocks[0].raw, &s, nil)
			},
			want: ErrMetadataCommitment,
		},
		{
			name: "redacted record outside an extract",
			run: func(w *Writer) error {
				s := blocks[0].sum.clone()
				s.qualityUnion |= QualityRedacted
				return w.appendBlock(blocks[0].raw, &s, nil)
			},
			want: ErrMetadataCommitment,
		},
		{
			name: "summary disagrees with the envelope",
			run: func(w *Writer) error {
				s := blocks[0].sum.clone()
				s.recordCount++
				return w.appendBlock(blocks[0].raw, &s, nil)
			},
		},
		{
			name: "raw holds more than the envelope's body",
			run: func(w *Writer) error {
				return w.appendBlock(append(bytes.Clone(blocks[0].raw), 0), &blocks[0].sum, nil)
			},
		},
		{
			name: "a block is open",
			run: func(w *Writer) error {
				rec := testDataRecord(blocks[0].sum.firstSeq, blockTestHour, 1)
				require.NoError(t, w.Append(&rec))
				return w.appendBlock(blocks[1].raw, &blocks[1].sum, nil)
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
			require.ErrorIs(t, w.appendBlock(blocks[1].raw, &blocks[1].sum, nil), ErrWriterFailed, "the rejection failed the Writer")
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
	require.ErrorIs(t, w.appendBlock(blocks[0].raw, &blocks[0].sum, nil), ErrClosed)
}

func TestAppendBlockOutputFailures(t *testing.T) {
	t.Parallel()

	src := richFooterPack(t, CodecZstd).file
	r, blocks := readSourceBlocks(t, src)
	head := int(layoutOf(t, src).blocksStart)

	t.Run("write fails in a block", func(t *testing.T) {
		t.Parallel()

		fw := &failAfterWriter{limit: head + 10}
		w, err := startWriter(fw, copyOptions(r, blocks, CodecZstd))
		require.NoError(t, err)
		require.ErrorIs(t, w.appendBlock(blocks[0].raw, &blocks[0].sum, nil), errTestWrite)
		require.ErrorIs(t, w.appendBlock(blocks[1].raw, &blocks[1].sum, nil), ErrWriterFailed)
	})

	t.Run("sync fails after a block", func(t *testing.T) {
		t.Parallel()

		opts := copyOptions(r, blocks, CodecZstd)
		opts.Sync = &syncFailer{ok: 1}
		w, err := startWriter(io.Discard, opts)
		require.NoError(t, err)
		require.NoError(t, w.appendBlock(blocks[0].raw, &blocks[0].sum, nil))
		require.ErrorIs(t, w.appendBlock(blocks[1].raw, &blocks[1].sum, nil), errTestSync)
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
			require.NoError(t, w.appendBlock(blocks[i].raw, &blocks[i].sum, nil))
		}
		_, err = w.Close()
		require.ErrorIs(t, err, errTestSync)
		require.ErrorIs(t, w.appendBlock(blocks[0].raw, &blocks[0].sum, nil), ErrWriterFailed)
		assert.Equal(t, OutcomeFinalizedConsistent, mustVerify(t, out.Bytes()).Outcome, "the trailer reached the output")
	})

	t.Run("the output takes the footer and trailer but reports an error", func(t *testing.T) {
		t.Parallel()

		fw := &failAfterWriter{limit: int(r.Header().Trailer.FooterOffset), takeAll: true}
		w, err := startWriter(fw, copyOptions(r, blocks, CodecZstd))
		require.NoError(t, err)
		for i := range blocks {
			require.NoError(t, w.appendBlock(blocks[i].raw, &blocks[i].sum, nil))
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
		require.NoError(t, w.appendBlock(blocks[i].raw, &blocks[i].sum, nil))
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
		PeriodStart: blockTestHour, PeriodEnd: blockTestHour + hourNs,
	}

	var kept bytes.Buffer
	w, err := startWriter(&kept, WriterOptions{Meta: meta})
	require.NoError(t, err)
	assert.True(t, w.CaptureID().IsZero())
	assert.Equal(t, format.UUID{}, layoutHeader(t, kept.Bytes()).CaptureID)

	w, err = NewWriter(io.Discard, WriterOptions{Meta: meta})
	require.NoError(t, err)
	assert.False(t, w.CaptureID().IsZero(), "NewWriter generates a capture id for the nil UUID")
}
