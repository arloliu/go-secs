package tracepack

import (
	"bytes"
	"errors"
	"io"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// listScratch is a scratchPool that lends last in, first out, deterministically, also under -race,
// where a sync.Pool drops some of what it is given.
type listScratch[T any] struct {
	free []*T
}

func (l *listScratch[T]) get() *T {
	n := len(l.free)
	if n == 0 {
		return new(T)
	}
	v := l.free[n-1]
	l.free[n-1] = nil
	l.free = l.free[:n-1]

	return v
}

func (l *listScratch[T]) put(v *T) {
	l.free = append(l.free, v)
}

// requireMergeScratchClean checks that sc, a scratch a merge run gave back, keeps no view, alias, hook or summary,
// no pointer past its free list, and only empty buffers.
func requireMergeScratchClean(t *testing.T, sc *mergeScratch) {
	t.Helper()

	for _, buf := range sc.free[len(sc.free):cap(sc.free)] {
		require.Nil(t, buf, "a pointer past the free list")
	}
	for _, buf := range sc.free {
		require.Empty(t, buf.raw)
		require.Empty(t, buf.decoded)
		require.Empty(t, buf.rows)
		require.Empty(t, buf.offs)
		require.Equal(t, decodedBlock{}, buf.blk)
	}
	for _, u := range []*unitBuilder{&sc.unit, &sc.group.records} {
		require.Zero(t, u.headerLen)
		require.Zero(t, u.size)
		require.Empty(t, u.rows)
		require.Empty(t, u.payloads)
		require.Empty(t, u.ends)
		for _, v := range u.rowViews[:cap(u.rowViews)] {
			require.Nil(t, v, "a row view")
		}
		for _, v := range u.payloadViews[:cap(u.payloadViews)] {
			require.Nil(t, v, "a payload view")
		}
	}
	require.Empty(t, sc.group.raw)
	for _, gu := range sc.group.units[:cap(sc.group.units)] {
		require.Equal(t, groupUnit{}, gu)
	}
	for _, e := range []*encodeBuf{&sc.enc, &sc.group.enc} {
		require.Empty(t, e.raw)
		require.Empty(t, e.body)
		require.Empty(t, e.enc)
		require.Nil(t, e.check.raw, "the check's alias of the last block built")
		require.Equal(t, decodedBlock{}, e.check.blk)
		require.Nil(t, e.hook)
	}
}

// mergeThrough merges inputs through pool, with the plan changed by hooks when set.
func mergeThrough(t *testing.T, inputs []MergeInput, opts MergeOptions, pool scratchPool[mergeScratch], hooks func(p *mergePlan)) ([]byte, error) {
	t.Helper()

	if opts.Now == nil {
		now := time.Unix(0, blockTestHour)
		opts.Now = func() time.Time { return now }
	}
	out, _, err := writeWithHooks(t, inputs, opts, func(p *mergePlan) {
		p.scratch = pool
		if hooks != nil {
			hooks(p)
		}
	})

	return out, err
}

// TestMergeScratchReuse runs merges through one pool, each after a merge that succeeded or failed with buffers in use:
// the pool gets back the same scratch, clean, and the next merge writes the archive a merge through an empty pool writes.
// A buffer a block still held when the run failed is not pooled.
func TestMergeScratchReuse(t *testing.T) {
	t.Parallel()

	early := mergePack(t, seg4, nil, hourSteps(1, 4, 4))
	a := mergePackCodec(t, CodecZstd, seg0, hourSteps(10, 13, 4))
	b := mergePackCodec(t, CodecNone, seg1, hourSteps(10, 13, 2))
	c := mergePackCodec(t, CodecZstd, seg2, hourSteps(20, 29, 3))
	resolved, other := inputsOf(early, a, b), inputsOf(c, early)

	corrupt := func(p *mergePlan) {
		p.encodedHook = func(enc []byte) []byte {
			enc[len(enc)-1] ^= 1

			return enc
		}
	}
	tests := []struct {
		name   string
		inputs []MergeInput
		edit   func(opts *MergeOptions)
		hooks  func(p *mergePlan)
		err    string
	}{
		{name: "after a merge that resolves and coalesces", inputs: resolved},
		{name: "after a merge without coalescing", inputs: resolved, edit: func(o *MergeOptions) { o.NoCoalesce = true }},
		{name: "after a block that fails its check", inputs: resolved, hooks: corrupt, err: "fails its check"},
		{name: "after a MaxOpenBlocks failure", inputs: resolved, edit: func(o *MergeOptions) { o.MaxOpenBlocks = 1 }, err: "MaxOpenBlocks"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			pool := &listScratch[mergeScratch]{}
			sc := pool.get()
			pool.put(sc)
			opts := mergeOpts()
			if tt.edit != nil {
				tt.edit(&opts)
			}
			released := map[*blockBuf]bool{}
			_, err := mergeThrough(t, tt.inputs, opts, pool, func(p *mergePlan) {
				p.releaseHook = func(buf *blockBuf) { released[buf] = true }
				if tt.hooks != nil {
					tt.hooks(p)
				}
			})
			if tt.err != "" {
				require.ErrorContains(t, err, tt.err)
			} else {
				require.NoError(t, err)
			}
			require.Len(t, pool.free, 1)
			require.Same(t, sc, pool.free[0], "the run gave back the scratch it took")
			requireMergeScratchClean(t, sc)
			for _, buf := range sc.free {
				require.True(t, released[buf], "a pooled buffer that no block released")
			}
			require.Positive(t, cap(sc.unit.rows)+cap(sc.group.records.rows)+len(sc.free), "the run used the scratch")

			for _, next := range [][]MergeInput{other, resolved} {
				want, err := mergeThrough(t, next, mergeOpts(), &listScratch[mergeScratch]{}, nil)
				require.NoError(t, err)
				got, err := mergeThrough(t, next, mergeOpts(), pool, nil)
				require.NoError(t, err)
				require.Equal(t, want, got, "the archive through the reused scratch")
				require.Same(t, sc, pool.free[0])
				requireMergeScratchClean(t, sc)
			}
		})
	}
}

// failingOut is an io.Writer that fails every write after its first ok writes.
type failingOut struct {
	buf bytes.Buffer
	ok  int
}

var errFailingOut = errors.New("failing output")

func (f *failingOut) Write(p []byte) (int, error) {
	if f.ok == 0 {
		return 0, errFailingOut
	}
	f.ok--

	return f.buf.Write(p)
}

// scratchWriterOpts returns the options of the Writer tests of the scratch pool: fixed IDs and start time, pool as the scratch pool.
func scratchWriterOpts(pool scratchPool[writerScratch]) WriterOptions {
	m := readerTestMeta()
	m.PeriodStart, m.PeriodEnd = blockTestHour-blockTestHour%hourNs, blockTestHour-blockTestHour%hourNs+hourNs
	now := time.Unix(0, blockTestHour)

	return WriterOptions{
		Meta: m, Facts: PackFacts{AnyClassified: true}, AssignSeq: true, BlockThreshold: 600,
		PackID: seg0, CaptureID: captureLow, Now: func() time.Time { return now }, scratch: pool,
	}
}

// writeScratchPack writes n data records through a Writer of opts into out and closes it.
func writeScratchPack(t *testing.T, opts WriterOptions, out *bytes.Buffer, n int) *Writer {
	t.Helper()

	w, err := NewWriter(out, opts)
	require.NoError(t, err)
	for i := range n {
		rec := testDataRecord(0, blockTestHour+int64(i), 1)
		require.NoError(t, w.Append(&rec))
	}
	_, err = w.Close()
	require.NoError(t, err)

	return w
}

// TestWriterScratchReuse writes packs through one pool:
// a closed Writer gives its buffers back and keeps none, the next Writer takes them and writes the pack a Writer through an empty pool writes,
// and the closed Writer still answers as a closed Writer.
// A Writer whose Close fails gives nothing back.
func TestWriterScratchReuse(t *testing.T) {
	t.Parallel()

	pool := &listScratch[writerScratch]{}
	var first bytes.Buffer
	w1 := writeScratchPack(t, scratchWriterOpts(pool), &first, 40)
	require.Len(t, pool.free, 1)
	sc := pool.free[0]
	require.Positive(t, cap(sc.headers))
	require.Positive(t, cap(sc.body))
	require.Positive(t, cap(sc.val.rows))
	require.Nil(t, w1.block.headers)
	require.Nil(t, w1.block.payloads)
	require.Nil(t, w1.bodyBuf)
	require.Nil(t, w1.encBuf)
	require.Equal(t, validateBuf{}, w1.valBuf)
	require.Nil(t, w1.sc)
	id, capture, anchor := w1.PackID(), w1.CaptureID(), w1.anchor()

	for _, n := range []int{7, 90} {
		var want, got bytes.Buffer
		writeScratchPack(t, scratchWriterOpts(&listScratch[writerScratch]{}), &want, n)
		writeScratchPack(t, scratchWriterOpts(pool), &got, n)
		require.Equal(t, want.Bytes(), got.Bytes(), "the pack through the reused buffers")
		require.Len(t, pool.free, 1)
		require.Same(t, sc, pool.free[0])
	}

	rec := testDataRecord(0, blockTestHour, 1)
	require.ErrorIs(t, w1.Append(&rec), ErrClosed)
	require.ErrorIs(t, w1.Flush(), ErrClosed)
	_, err := w1.Close()
	require.ErrorIs(t, err, ErrClosed)
	require.Equal(t, id, w1.PackID())
	require.Equal(t, capture, w1.CaptureID())
	require.Equal(t, anchor, w1.anchor())

	// The last write of a Close writes the footer, after every block was written: a Close that fails there fails in finalize.
	counted := &failingOut{ok: math.MaxInt}
	writeScratchPackTo(t, scratchWriterOpts(&listScratch[writerScratch]{}), counted, 40)
	writes := math.MaxInt - counted.ok
	for _, ok := range []int{1, writes - 1} {
		failPool := &listScratch[writerScratch]{}
		w, err := NewWriter(&failingOut{ok: ok}, scratchWriterOpts(failPool))
		require.NoError(t, err)
		for i := range 40 {
			r := testDataRecord(0, blockTestHour+int64(i), 1)
			if err := w.Append(&r); err != nil {
				require.ErrorIs(t, err, errFailingOut)
			}
		}
		_, err = w.Close()
		require.ErrorIs(t, err, errFailingOut, "%d writes succeed", ok)
		require.Empty(t, failPool.free, "a Writer whose Close failed gives nothing back, %d writes succeed", ok)
		require.NotNil(t, w.sc)
	}
}

// writeScratchPackTo writes n data records through a Writer of opts into out and closes it.
func writeScratchPackTo(t *testing.T, opts WriterOptions, out io.Writer, n int) {
	t.Helper()

	w, err := NewWriter(out, opts)
	require.NoError(t, err)
	for i := range n {
		rec := testDataRecord(0, blockTestHour+int64(i), 1)
		require.NoError(t, w.Append(&rec))
	}
	_, err = w.Close()
	require.NoError(t, err)
}
