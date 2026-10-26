package tracepack

import (
	"bytes"
	"errors"
	"io"
	"math"
	"testing"
	"time"
	"unsafe"

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
		{name: "after a conflict", inputs: inputsOf(early, a, mergePackCodec(t, CodecZstd, seg3, seqSteps(12, 13, 2, blockTestHour+1000))),
			err: "conflicts"},
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
	opts := scratchWriterOpts(pool)
	scratchClock(opts.Meta)
	opts.DetectClockSteps = true
	var first bytes.Buffer
	w1, err := NewWriter(&first, opts)
	require.NoError(t, err)
	for i := range 40 {
		rec := testDataRecord(0, blockTestHour+int64(i), 1)
		require.NoError(t, w1.Append(&rec))
	}
	id, capture, anchor := w1.PackID(), w1.CaptureID(), w1.anchor()
	require.NotZero(t, anchor, "an active clock anchor")
	headers, body := unsafe.SliceData(w1.block.headers), unsafe.SliceData(w1.bodyBuf)
	cont, err := w1.Close()
	require.NoError(t, err)

	// A Writer through an empty pool, of the same options and records, returns the same Continuation.
	fresh := scratchWriterOpts(&listScratch[writerScratch]{})
	scratchClock(fresh.Meta)
	fresh.DetectClockSteps = true
	wf, err := NewWriter(&bytes.Buffer{}, fresh)
	require.NoError(t, err)
	for i := range 40 {
		rec := testDataRecord(0, blockTestHour+int64(i), 1)
		require.NoError(t, wf.Append(&rec))
	}
	wantCont, err := wf.Close()
	require.NoError(t, err)
	require.Equal(t, wantCont, cont)
	require.True(t, cont.ok)

	require.Len(t, pool.free, 1)
	sc := pool.free[0]
	require.Same(t, headers, unsafe.SliceData(sc.headers), "the headers handed over")
	require.Same(t, body, unsafe.SliceData(sc.body), "the body buffer handed over")
	require.Positive(t, cap(sc.val.rows))
	require.Nil(t, w1.block.headers)
	require.Nil(t, w1.block.payloads)
	require.Nil(t, w1.bodyBuf)
	require.Nil(t, w1.encBuf)
	require.Equal(t, validateBuf{}, w1.valBuf)
	require.Nil(t, w1.sc)
	require.Equal(t, id, w1.PackID())
	require.Equal(t, capture, w1.CaptureID())
	require.Equal(t, anchor, w1.anchor())

	// The next Writer takes the same storage.
	w2, err := NewWriter(&bytes.Buffer{}, scratchWriterOpts(pool))
	require.NoError(t, err)
	require.Empty(t, pool.free)
	require.Same(t, body, unsafe.SliceData(w2.bodyBuf[:1]), "the next Writer writes its blocks in the same storage")
	require.Equal(t, anchor, w1.anchor(), "while another Writer owns the storage")
	_, err = w2.Close()
	require.NoError(t, err)

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
	_, err = w1.Close()
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

// TestMergeScratchAfterPendingFailure fails a merge while its resolution holds resolved records in its unit,
// its coalescing group holds the unit of an earlier cluster, and a block holds the buffer the pool lent, grown by the block's read:
// the run gives back its scratch clean, without that buffer anywhere in its free list's storage,
// and the next merge through the pool writes what a merge through an empty pool writes.
func TestMergeScratchAfterPendingFailure(t *testing.T) {
	t.Parallel()

	// early is a cluster of its own, written first and kept in the coalescing group;
	// a, b and c overlap three deep at seq 14, which MaxOpenBlocks 2 refuses after the resolution took seqs 10 to 13,
	// while a, holding the lent buffer since its load at seq 10, is still open.
	early := mergePack(t, seg4, nil, hourSteps(1, 4, 4))
	a := mergePackCodec(t, CodecZstd, seg0, hourSteps(10, 20, 11))
	b := mergePackCodec(t, CodecNone, seg1, hourSteps(12, 15, 4))
	c := mergePackCodec(t, CodecZstd, seg2, hourSteps(14, 16, 3))

	lent := &blockBuf{raw: make([]byte, 0, 1)}
	sc := &mergeScratch{free: []*blockBuf{lent}}
	pool := &listScratch[mergeScratch]{free: []*mergeScratch{sc}}
	opts := mergeOpts()
	opts.MaxOpenBlocks = 2
	var unitRecords, groupUnits int
	var lentGrew bool
	_, err := mergeThrough(t, inputsOf(early, a, b, c), opts, pool, func(p *mergePlan) {
		p.stashHook = func(m *mergeRun) {
			unitRecords, groupUnits = len(m.unit.ends), len(m.group.units)
			lentGrew = cap(lent.raw) > 1
		}
	})
	require.ErrorContains(t, err, "overlap 3 deep at seq 14")
	require.Equal(t, 4, unitRecords, "the resolution held seqs 10 to 13")
	require.Equal(t, 1, groupUnits, "the group held the earlier cluster's unit")
	require.True(t, lentGrew, "the lent buffer was read into")

	require.Len(t, pool.free, 1)
	require.Same(t, sc, pool.free[0])
	requireMergeScratchClean(t, sc)
	require.NotContains(t, sc.free[:cap(sc.free)], lent, "the buffer a block held when the run failed")

	for _, next := range [][]MergeInput{inputsOf(early, a, b, c), inputsOf(a, b)} {
		want, err := mergeThrough(t, next, mergeOpts(), &listScratch[mergeScratch]{}, nil)
		require.NoError(t, err)
		got, err := mergeThrough(t, next, mergeOpts(), pool, nil)
		require.NoError(t, err)
		require.Equal(t, want, got)
		requireMergeScratchClean(t, sc)
	}
}

// TestMergeRunBufferClearsItsSlot checks that the free list keeps no pointer to a buffer it lends.
func TestMergeRunBufferClearsItsSlot(t *testing.T) {
	t.Parallel()

	x, y := new(blockBuf), new(blockBuf)
	m := &mergeRun{free: []*blockBuf{x, y}}
	require.Same(t, y, m.buffer())
	require.Equal(t, []*blockBuf{x, nil}, m.free[:2])
	require.Same(t, x, m.buffer())
	require.Equal(t, []*blockBuf{nil, nil}, m.free[:2])
	require.NotNil(t, m.buffer(), "an empty list lends a new buffer")
}

// TestScratchBytesTotals checks every charge of a scratch's byte total against a total built field by field,
// each slice at its own capacity, so a charge left out or counted at the wrong element width shows.
func TestScratchBytesTotals(t *testing.T) {
	t.Parallel()

	ws := &writerScratch{
		headers: make([]byte, 0, 3), payloads: make([]byte, 0, 5), body: make([]byte, 0, 7), enc: make([]byte, 0, 11),
		val: validateBuf{decoded: make([]byte, 0, 13), rows: make([]byte, 0, 17), offs: make([]uint32, 0, 19)},
	}
	require.Equal(t, int64(3+5+7+11+13+17+19*4), ws.bytes())

	ptr, hdr, word := int64(unsafe.Sizeof((*blockBuf)(nil))), int64(unsafe.Sizeof([]byte(nil))), int64(unsafe.Sizeof(0))
	gu := int64(unsafe.Sizeof(groupUnit{}))
	buf := func(raw, decoded, rows, offs int) *blockBuf {
		return &blockBuf{raw: make([]byte, 0, raw), decoded: make([]byte, 0, decoded), rows: make([]byte, 0, rows), offs: make([]uint32, 0, offs)}
	}
	unit := func(rows, payloads, ends, rowViews, payloadViews int) unitBuilder {
		return unitBuilder{
			rows: make([]byte, 0, rows), payloads: make([]byte, 0, payloads), ends: make([]int, 0, ends),
			rowViews: make([][]byte, 0, rowViews), payloadViews: make([][]byte, 0, payloadViews),
		}
	}
	enc := func(raw, body, e int, check *blockBuf) encodeBuf {
		return encodeBuf{raw: make([]byte, 0, raw), body: make([]byte, 0, body), enc: make([]byte, 0, e), check: *check}
	}
	free := make([]*blockBuf, 2, 5)
	free[0], free[1] = buf(1, 2, 3, 4), buf(5, 6, 7, 8)
	ms := &mergeScratch{
		free: free,
		unit: unit(10, 20, 30, 40, 50),
		enc:  enc(60, 70, 80, buf(90, 100, 110, 120)),
		group: coalesceGroup{records: unit(130, 140, 150, 160, 170), raw: make([]byte, 0, 180), units: make([]groupUnit, 0, 3),
			enc: enc(190, 200, 210, buf(220, 230, 240, 250))},
	}
	want := 5*ptr + (1 + 2 + 3 + 4*4) + (5 + 6 + 7 + 8*4) +
		(10 + 20 + 30*word + (40+50)*hdr) +
		(60 + 70 + 80 + (90 + 100 + 110 + 120*4)) +
		(130 + 140 + 150*word + (160+170)*hdr) + 180 + 3*gu +
		(190 + 200 + 210 + (220 + 230 + 240 + 250*4))
	require.Equal(t, want, ms.bytes())
}

// TestScratchBytesWiden checks the capacity sums on values whose int sum, or product, overflows where int is 32 bits.
func TestScratchBytesWiden(t *testing.T) {
	t.Parallel()

	const m = math.MaxInt32
	word, hdr := int64(unsafe.Sizeof(0)), int64(unsafe.Sizeof([]byte(nil)))
	require.Equal(t, int64(2)*m+m*word+2*m*hdr, unitBuilderBytes(m, m, m, m, m))
	require.Equal(t, int64(3)*m+5, encodeBufBytes(m, m, m, 5))
	require.Equal(t, int64(4)*m+7, writerScratchBytes(m, m, m, m, 7))
}

// TestScratchKeptUpToTheCap gives back a Writer's and a merge run's scratch holding exactly the cap, then one byte more:
// the first is kept, the second dropped.
func TestScratchKeptUpToTheCap(t *testing.T) {
	for _, extra := range []int{0, 1} {
		wpool := &listScratch[writerScratch]{}
		w := &Writer{scratch: wpool, sc: new(writerScratch), bodyBuf: make([]byte, 0, maxPooledWriterScratch+extra)}
		w.giveScratch()
		require.Len(t, wpool.free, 1-extra, "a Writer scratch of %d bytes over the cap", extra)

		mpool := &listScratch[mergeScratch]{}
		m := &mergeRun{p: &mergePlan{}, pool: mpool, sc: new(mergeScratch), unit: unitBuilder{payloads: make([]byte, 0, maxPooledMergeScratch+extra)}}
		m.stash()
		require.Len(t, mpool.free, 1-extra, "a merge scratch of %d bytes over the cap", extra)
	}
}

// scratchClock gives m the capture clock a Writer that detects clock steps needs, its origin 5 ns before the first record.
func scratchClock(m *PackMeta) {
	m.TimeSource = TimeSourceCaptureClock
	m.CaptureOriginUTCNs, m.CaptureOriginMonoNs, m.ClockStepToleranceNs = new(blockTestHour-5), new(int64(0)), new(uint64(1000))
}
