package tracepack

import (
	"bytes"
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// retainedBytes returns the number of bootstrap bytes r keeps for its lifetime.
func retainedBytes(r *Reader) int {
	n := 0
	for _, s := range r.held {
		n += len(s.data)
	}

	return n
}

// retainBound returns the most bootstrap bytes a Reader opened with opts may keep:
// max(WholeReadThreshold, HeadWindow + TailWindow), after defaults.
func retainBound(opts ReaderOptions) int {
	o := opts.withDefaults()

	return int(max(o.WholeReadThreshold, o.HeadWindow+o.TailWindow))
}

// openLogged opens file through a gatedReader without gated rounds, which logs every ReadAt call.
func openLogged(t testing.TB, file []byte, opts ReaderOptions) (*Reader, *gatedReader) {
	t.Helper()

	g := newGatedReader(file)
	r, err := Open(t.Context(), g, int64(len(file)), opts)
	require.NoError(t, err)

	return r, g
}

// mergeItems runs a time-order MergeIterate of q over readers and returns copies of what it yielded,
// passing each Item to touch after copying it when touch is set.
func mergeItems(ctx context.Context, readers []*Reader, q Query, touch func(*Item)) ([]iterItem, error) {
	var items []iterItem
	_, err := MergeIterate(ctx, readers, q, MergeIterateOptions{Order: OrderTime}, func(it *Item) error {
		items = append(items, copyItem(it))
		if touch != nil {
			touch(it)
		}

		return nil
	})

	return items, err
}

// scribble overwrites the payload and the header extension bytes of it.
func scribble(it *Item) {
	for i := range it.Record.Payload {
		it.Record.Payload[i] = 0xEE
	}
	for i := range it.HeaderExtra {
		it.HeaderExtra[i] = 0xEE
	}
}

// windowsOver returns options that read the pack of layout l in the head and tail path,
// the head window ending at head and the tail window starting at tail.
func windowsOver(t testing.TB, l packLayout, head, tail uint64) ReaderOptions {
	t.Helper()

	opts := ReaderOptions{WholeReadThreshold: 1, HeadWindow: int64(head), TailWindow: l.size - int64(tail)}
	require.Less(t, opts.HeadWindow, l.size-opts.TailWindow, "the windows must not cover the pack")

	return opts
}

func TestOpenIterateSmallPackOneRead(t *testing.T) {
	t.Parallel()

	for _, open := range []bool{false, true} {
		p := writeReaderPack(t, readerPackConfig{codec: CodecZstd, open: open}, hourRecords(3, 40))
		require.LessOrEqual(t, int64(len(p.file)), int64(DefaultWholeReadThreshold))

		var stats perfStats
		ra := &perfReaderAt{data: p.file, stats: &stats}
		r, err := Open(t.Context(), ra, int64(len(p.file)), ReaderOptions{})
		require.NoError(t, err)
		require.Equal(t, !open, r.Header().Finalized)

		run := iterate(t, r, Query{Payloads: true})
		require.Len(t, run.items, 3*40)
		_, _, err = runExportJSONL(t, r)
		require.NoError(t, err)
		items, err := mergeItems(t.Context(), []*Reader{r}, Query{Payloads: true}, nil)
		require.NoError(t, err)
		require.Len(t, items, 3*40)

		assert.Equal(t, int64(1), stats.calls.Load(), "open %v: one ReadAt for Open and every read after it", open)
		assert.Equal(t, int64(len(p.file)), stats.bytes.Load())
	}
}

func TestReaderServesWindows(t *testing.T) {
	t.Parallel()

	p := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, hourRecords(4, 40))
	require.Len(t, p.blocks, 4)
	l := layoutOf(t, p.file)
	b := p.blocks

	tests := []struct {
		name string
		opts ReaderOptions
		read []int
	}{
		{name: "blocks inside the windows", opts: windowsOver(t, l, b[1].offset, b[3].offset), read: []int{1, 2}},
		{name: "windows ending inside blocks", opts: windowsOver(t, l, b[1].offset+10, b[3].offset-10), read: []int{1, 2}},
		{name: "head window over two blocks", opts: windowsOver(t, l, b[2].offset, l.tr.FooterOffset), read: []int{2, 3}},
		{name: "tail window over two blocks", opts: windowsOver(t, l, l.blocksStart, b[2].offset), read: []int{0, 1}},
		{name: "a block straddling each edge", opts: windowsOver(t, l, b[0].offset+10, b[3].offset+10), read: []int{0, 1, 2, 3}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r, g := openLogged(t, p.file, tt.opts)
			assert.LessOrEqual(t, retainedBytes(r), retainBound(tt.opts))
			opened := len(g.allCalls())

			run := iterate(t, r, Query{Payloads: true})
			require.Len(t, run.items, 4*40)
			assert.True(t, run.res.Complete())

			calls := g.allCalls()[opened:]
			read := make([]int, 0, len(calls))
			for _, c := range calls {
				for i := range b {
					if c.off == int64(b[i].offset) && c.n == int(b[i].onDiskLen) {
						read = append(read, i)
					}
				}
			}
			assert.Len(t, calls, len(read), "every read after Open is a block read")
			assert.Equal(t, tt.read, read)
		})
	}
}

func TestReaderHintSuffixNotRetained(t *testing.T) {
	t.Parallel()

	p := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, hourRecords(3, 40))
	l := layoutOf(t, p.file)
	// A stale hint at block 1: the suffix it reads holds blocks 1 and 2 and the footer.
	opts := windowsOver(t, l, p.blocks[1].offset, l.tr.FooterOffset)
	opts.FooterOffset = p.blocks[1].offset

	r, g := openLogged(t, p.file, opts)
	require.ElementsMatch(t, []readCall{{0, int(opts.HeadWindow)}, {int64(p.blocks[1].offset), int(l.size) - int(p.blocks[1].offset)}},
		g.allCalls(), "one round: the head window and the hinted suffix")
	h := r.Header()
	assert.True(t, h.Finalized, "the footer CRC comes from the hinted suffix")
	require.NoError(t, h.FooterErr)
	assert.Equal(t, int(opts.HeadWindow), retainedBytes(r), "only the head window is kept")
	assert.LessOrEqual(t, retainedBytes(r), retainBound(opts))

	run := iterate(t, r, Query{Payloads: true})
	require.Len(t, run.items, 3*40)
	assert.Zero(t, readsAt(g, p.blocks[0].offset), "block 0 lies in the head window")
	assert.Equal(t, 2, readsAt(g, p.blocks[1].offset), "the suffix read, then block 1 read again")
	assert.Equal(t, 1, readsAt(g, p.blocks[2].offset), "block 2 is read")
}

func TestReaderFooterCRCFromRetainedBytes(t *testing.T) {
	t.Parallel()

	t.Run("whole read", func(t *testing.T) {
		t.Parallel()

		p := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, hourRecords(3, 40))
		l := layoutOf(t, p.file)
		opts := ReaderOptions{MaxFooterLen: int64(l.tr.FooterLen) - 1}

		r, g := openLogged(t, p.file, opts)
		h := r.Header()
		assert.True(t, h.Finalized, "the footer CRC comes from the whole read")
		require.ErrorIs(t, h.FooterErr, ErrReadLimit, "the footer is not used")
		assert.Equal(t, walkedInfos(p.blocks), r.Blocks())
		assert.Empty(t, r.openDefects)

		run := iterate(t, r, Query{Payloads: true})
		require.Len(t, run.items, 3*40)
		assert.Len(t, g.allCalls(), 1, "one ReadAt in total")
	})

	t.Run("footer outside the windows", func(t *testing.T) {
		t.Parallel()

		p, err := bigFooterPack()
		require.NoError(t, err)
		l := layoutOf(t, p.file)
		opts := ReaderOptions{MaxFooterLen: 64 << 10, WholeReadThreshold: 1, HeadWindow: int64(p.blocks[3].offset)}
		require.Less(t, int64(DefaultTailWindow), int64(l.tr.FooterLen), "the tail window must lie inside the footer")

		const chunk = 1 << 20
		fo := int64(l.tr.FooterOffset)
		require.Greater(t, l.tr.FooterLen, uint64(chunk))
		require.LessOrEqual(t, l.tr.FooterLen, uint64(2*chunk))

		r, g := openLogged(t, p.file, opts)
		assert.True(t, r.Header().Finalized)
		calls := g.allCalls()
		assert.Contains(t, calls, readCall{fo, chunk}, "the footer CRC is streamed")
		assert.Contains(t, calls, readCall{fo + chunk, int(l.tr.FooterLen) - chunk})
		assert.Equal(t, int(opts.HeadWindow), retainedBytes(r), "only the head window is kept, no CRC chunk")
	})
}

// TestReaderFooterCRCFromFirstRoundSuffix opens a pack whose footer exceeds MaxFooterLen and lies in the tail window,
// with every block envelope in the head window:
// the footer CRC comes from the tail window and the walk from the head window, so Open issues its first round only.
func TestReaderFooterCRCFromFirstRoundSuffix(t *testing.T) {
	t.Parallel()

	p := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, hourRecords(3, 40))
	l := layoutOf(t, p.file)
	last := p.blocks[len(p.blocks)-1].offset
	// The head window ends just after the last envelope, and the tail window starts one byte later, inside its body.
	opts := windowsOver(t, l, last+format.EnvelopeLen, last+format.EnvelopeLen+1)
	opts.MaxFooterLen = int64(l.tr.FooterLen) - 1

	r, g := openLogged(t, p.file, opts)
	assert.ElementsMatch(t, []readCall{{0, int(opts.HeadWindow)}, {l.size - opts.TailWindow, int(opts.TailWindow)}}, g.allCalls(),
		"one round: no CRC chunk, no envelope read")
	h := r.Header()
	assert.True(t, h.Finalized, "the footer CRC comes from the tail window")
	require.ErrorIs(t, h.FooterErr, ErrReadLimit)
	assert.Equal(t, walkedInfos(p.blocks), r.Blocks())
	assert.Empty(t, r.openDefects)
}

func TestReaderWalkFromBootstrap(t *testing.T) {
	t.Parallel()

	p := writeReaderPack(t, readerPackConfig{codec: CodecZstd, open: true}, hourRecords(3, 40))
	r, g := openLogged(t, p.file, ReaderOptions{})
	require.NotNil(t, r.Header().FooterErr)
	assert.Equal(t, walkedInfos(p.blocks), r.Blocks())

	run := iterate(t, r, Query{Payloads: true})
	require.Len(t, run.items, 3*40)
	assert.Equal(t, []readCall{{0, len(p.file)}}, g.allCalls(), "the walk and the reads use the whole read")
}

// requireUntouchedReads checks that repeated and concurrent reads of r, with callbacks that overwrite every payload
// and header extension they get, yield want, the records as stored.
func requireUntouchedReads(t *testing.T, r *Reader, want []iterItem) {
	t.Helper()

	q := Query{Payloads: true}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			got, err := runScribbled(t.Context(), r, q)
			assert.NoError(t, err)
			assert.Equal(t, want, got, "Iterate")

			items, err := mergeItems(t.Context(), []*Reader{r}, q, scribble)
			assert.NoError(t, err)
			assert.Equal(t, want, items, "MergeIterate")
		})
	}
	wg.Wait()

	got, err := runScribbled(t.Context(), r, q)
	require.NoError(t, err)
	assert.Equal(t, want, got, "a later Iterate")
}

// runScribbled runs q over r, copying each Item, then overwriting its payload and header extension bytes.
func runScribbled(ctx context.Context, r *Reader, q Query) ([]iterItem, error) {
	var items []iterItem
	_, err := r.Iterate(ctx, q, func(it *Item) error {
		items = append(items, copyItem(it))
		scribble(it)

		return nil
	})

	return items, err
}

func TestReaderBootstrapNoAliasing(t *testing.T) {
	t.Parallel()

	p := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, hourRecords(3, 40))
	l := layoutOf(t, p.file)
	b0, b1 := p.blocks[0], p.blocks[1]
	want := iterate(t, mustOpen(t, p.file, uncachedWindows()), Query{Payloads: true}).items

	// The head window holds block 0 and enough of block 1 that a block buffer taken from it could hold block 1,
	// whose read then goes to ReadAt.
	grow := uint64(max(int(b1.onDiskLen)-int(b0.onDiskLen), 0)) + 10
	require.Less(t, grow, uint64(b1.onDiskLen))

	for _, tt := range []struct {
		name  string
		opts  ReaderOptions
		reads int
	}{
		{name: "whole object", opts: ReaderOptions{}, reads: 1},
		{name: "windows", opts: windowsOver(t, l, b1.offset+grow, l.tr.FooterOffset)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r, g := openLogged(t, p.file, tt.opts)
			requireUntouchedReads(t, r, want)
			if tt.reads > 0 {
				assert.Len(t, g.allCalls(), tt.reads, "every read is served from the whole read")
			}
		})
	}
}

func TestReaderCachedNoneAliasing(t *testing.T) {
	t.Parallel()

	p := writeReaderPack(t, readerPackConfig{codec: CodecNone}, hourRecords(3, 40))
	file := rebuildPack(t, p.file, func(_ int, b *testBlock) { widen(b) })
	l := layoutOf(t, file)
	blocks := mustOpen(t, file, ReaderOptions{}).Blocks()
	want := iterate(t, mustOpen(t, file, uncachedWindows()), Query{Payloads: true}).items
	require.NotEmpty(t, want[0].extra, "the headers carry extension bytes")

	for _, tt := range []struct {
		name string
		opts ReaderOptions
	}{
		{name: "whole object", opts: ReaderOptions{}},
		{name: "windows", opts: windowsOver(t, l, blocks[1].Offset, blocks[2].Offset)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r, g := openLogged(t, file, tt.opts)
			opened := len(g.allCalls())
			requireUntouchedReads(t, r, want)
			for _, c := range g.allCalls()[opened:] {
				assert.Equal(t, int64(blocks[1].Offset), c.off, "only block 1, outside the windows, is read")
			}
		})
	}
}

func TestReaderCachedReadCancel(t *testing.T) {
	t.Parallel()

	p := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, hourRecords(3, 40))
	for _, tt := range []struct {
		name  string
		opts  ReaderOptions
		reads int
	}{
		{name: "served from the whole read", opts: ReaderOptions{}},
		{name: "read with ReadAt", opts: uncachedWindows(), reads: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r, g := openLogged(t, p.file, tt.opts)
			opened := len(g.allCalls())
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			var seen []uint64
			_, err := r.Iterate(ctx, Query{}, func(it *Item) error {
				seen = append(seen, it.Record.Seq)
				cancel()

				return nil
			})
			require.ErrorIs(t, err, context.Canceled)
			assert.Len(t, seen, 40, "block 0's records, then the cancellation is seen before block 1")
			assert.Len(t, g.allCalls()[opened:], tt.reads)
		})
	}
}

func TestDefaultWholeReadThreshold(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 4<<20, DefaultWholeReadThreshold)
	assert.Equal(t, int64(DefaultWholeReadThreshold), ReaderOptions{}.withDefaults().WholeReadThreshold)
	assert.Equal(t, 4<<20, retainBound(ReaderOptions{}), "the default bound of retained bootstrap bytes")
}

func TestReaderCachedDefects(t *testing.T) {
	t.Parallel()

	p := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, hourRecords(3, 40))
	at := p.blocks[1].offset
	mismatch, _ := indexMismatchPack(t, func(b *testBlock) { b.headers[0].TSUTCNs++ })
	files := []struct {
		name   string
		file   []byte
		reason IncompleteReason
	}{
		{name: "damaged envelope", file: flipByte(p.file, at+12), reason: IncompleteReasonCorruptBlock},
		{name: "damaged body", file: flipByte(p.file, at+format.EnvelopeLen+5), reason: IncompleteReasonCorruptBlock},
		{name: "unknown codec", file: patchEnvelope(t, p.file, at, func(e *format.BlockEnvelope) { e.Codec = 7 }), reason: IncompleteReasonUnknownCodec},
		{name: "F-2 disagreement", file: mismatch, reason: IncompleteReasonIndexMismatch},
	}
	for _, f := range files {
		t.Run(f.name, func(t *testing.T) {
			t.Parallel()

			l := layoutOf(t, f.file)
			blocks := mustOpen(t, f.file, ReaderOptions{}).Blocks()

			var want *iterRun
			for _, path := range []struct {
				name  string
				opts  ReaderOptions
				reads int
			}{
				{name: "uncached", opts: uncachedWindows(), reads: 1},
				{name: "whole object", opts: ReaderOptions{}},
				{name: "windows", opts: windowsOver(t, l, blocks[2].Offset, l.tr.FooterOffset)},
			} {
				r, g := openLogged(t, f.file, path.opts)
				opened := len(g.allCalls())
				got := iterate(t, r, Query{Payloads: true})
				reads := 0
				for _, c := range g.allCalls()[opened:] {
					if c.off == int64(blocks[1].Offset) {
						reads++
					}
				}
				assert.Equal(t, path.reads, reads, "%s: block 1's reads after Open", path.name)

				require.NotEmpty(t, got.res.Incomplete, path.name)
				assert.Equal(t, f.reason, got.res.Incomplete[0].Reason, path.name)
				assert.Equal(t, 1, got.res.Incomplete[0].Block, path.name)
				if want == nil {
					want = &got
					continue
				}
				assert.Equal(t, want.items, got.items, path.name)
				requireSameResult(t, want.res, got.res)
			}
		})
	}
}

func TestReaderZeroBlockOversizedFooter(t *testing.T) {
	t.Parallel()

	p := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, nil)
	require.Empty(t, p.blocks)
	l := layoutOf(t, p.file)
	require.Equal(t, l.blocksStart, l.tr.FooterOffset, "the pack has no block")
	opts := ReaderOptions{MaxFooterLen: int64(l.tr.FooterLen) - 1}

	t.Run("CRC valid", func(t *testing.T) {
		t.Parallel()

		r, g := openLogged(t, p.file, opts)
		assert.Len(t, g.allCalls(), 1, "the CRC comes from the whole read")
		h := r.Header()
		assert.True(t, h.Finalized)
		require.ErrorIs(t, h.FooterErr, ErrReadLimit)
		assert.Empty(t, r.Blocks())
		assert.Empty(t, r.openDefects)
		assert.Zero(t, retainedBytes(r), "a pack without blocks keeps nothing")
	})

	t.Run("CRC broken", func(t *testing.T) {
		t.Parallel()

		r, g := openLogged(t, flipByte(p.file, l.tr.FooterOffset+1), opts)
		assert.Len(t, g.allCalls(), 1)
		h := r.Header()
		assert.False(t, h.Finalized, "the CRC is checked before finalization")
		require.ErrorIs(t, h.FooterErr, ErrChecksum)
		assert.NotErrorIs(t, h.FooterErr, ErrReadLimit, "the CRC is checked before the footer budget")
		assert.Zero(t, retainedBytes(r))
	})
}

func TestReaderFooterHintPositions(t *testing.T) {
	t.Parallel()

	closed := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, hourRecords(3, 40))
	open := writeReaderPack(t, readerPackConfig{codec: CodecZstd, open: true}, hourRecords(3, 40))
	l := layoutOf(t, closed.file)
	b1 := closed.blocks[1].offset
	// Both windows hold block bytes: the head window block 0, the tail window the end of block 2 without a valid trailer.
	closedBase := windowsOver(t, l, b1, l.tr.FooterOffset)
	openBase := ReaderOptions{WholeReadThreshold: 1, HeadWindow: int64(b1), TailWindow: format.TrailerLen}

	tests := []struct {
		name string
		file []byte
		base ReaderOptions
		hint uint64
	}{
		{name: "below footer_offset", file: closed.file, base: closedBase, hint: b1 + 3},
		{name: "at footer_offset", file: closed.file, base: closedBase, hint: l.tr.FooterOffset},
		{name: "above footer_offset", file: closed.file, base: closedBase, hint: l.tr.FooterOffset + 10},
		{name: "invalid trailer", file: open.file, base: openBase, hint: b1 + 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			plain := mustOpen(t, tt.file, tt.base)
			hinted := tt.base
			hinted.FooterOffset = tt.hint
			r := mustOpen(t, tt.file, hinted)
			require.True(t, r.hintUsable(), "the hint must be usable")

			assert.Equal(t, plain.Header(), r.Header())
			assert.Equal(t, plain.Blocks(), r.Blocks())
			assert.Equal(t, plain.openDefects, r.openDefects)
			assert.Equal(t, iterate(t, plain, Query{Payloads: true}), iterate(t, r, Query{Payloads: true}))

			assert.LessOrEqual(t, retainedBytes(r), retainBound(hinted))
			for _, s := range r.held {
				assert.Zero(t, s.off, "only the head window is kept, never the hinted suffix")
			}
			require.Len(t, r.held, 1)
			assert.True(t, bytes.Equal(tt.file[:hinted.HeadWindow], r.held[0].data))
		})
	}
}
