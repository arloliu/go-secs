package tracepack

import (
	"context"
	"fmt"
	"math"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// The captures of the plan tests; captureLow sorts before captureHigh.
var (
	captureLow  = UUID{0xC0}
	captureHigh = UUID{0xC1}
)

// acceptItems is an fn for MergeIterate that accepts every item.
func acceptItems(*Item) error { return nil }

// planTestPack writes the records of steps, in blocks closed where steps say, into a pack id of capture,
// finalized, or with open left unfinalized so the forward walk finds its blocks.
func planTestPack(t testing.TB, id, capture UUID, open bool, steps []footerTestStep) []byte {
	t.Helper()

	return withIDs(t, writeRepairPack(t, CodecZstd, nil, open, steps).file, id, capture)
}

// openAll opens every file through a bytes.Reader.
func openAll(t testing.TB, files ...[]byte) []*Reader {
	t.Helper()

	out := make([]*Reader, len(files))
	for i, f := range files {
		out[i] = mustOpen(t, f, ReaderOptions{})
	}

	return out
}

// planReads validates and plans the read of q over readers with opts.
// Each full-block read is counted, then passed to hook when hook is set.
// It returns the plan, the number of full-block reads of each block by (reader, block), and build's error,
// requiring nothing to stay reserved.
func planReads(t testing.TB, ctx context.Context, readers []*Reader, q Query, opts MergeIterateOptions,
	hook func(pack, block int),
) (*mergeIteratePlan, map[[2]int]int, error) {
	t.Helper()

	o, err := checkMergeIterate(ctx, readers, &q, opts, acceptItems)
	require.NoError(t, err)
	p := newMergeIteratePlan(o)
	reads := map[[2]int]int{}
	p.loader.readHook = func(pack, block int, _ *blockBuf) {
		reads[[2]int{pack, block}]++
		if hook != nil {
			hook(pack, block)
		}
	}
	err = p.build(ctx, readers, &q)
	assert.Zero(t, p.loader.budget.held, "nothing stays reserved after planning")

	return p, reads, err
}

// mustPlan plans the read of q over readers in capture order, requiring no error.
func mustPlan(t testing.TB, readers []*Reader, q Query) (*mergeIteratePlan, map[[2]int]int) {
	t.Helper()

	p, reads, err := planReads(t, t.Context(), readers, q, MergeIterateOptions{Order: OrderCapture}, nil)
	require.NoError(t, err)

	return p, reads
}

// clusterShape is a planCluster by the (reader, block) of its blocks.
type clusterShape struct {
	first, last uint64
	blocks      [][2]int
	tsMin       int64
	excluded    bool
}

// shapesOf returns the clusters of each capture of p, by capture_id.
func shapesOf(p *mergeIteratePlan) map[UUID][]clusterShape {
	out := make(map[UUID][]clusterShape, len(p.captures))
	for _, c := range p.captures {
		shapes := make([]clusterShape, len(c.clusters))
		for k := range c.clusters {
			cl := &c.clusters[k]
			s := clusterShape{first: cl.first, last: cl.last, tsMin: cl.tsMin, excluded: cl.excluded}
			for _, b := range cl.blocks {
				s.blocks = append(s.blocks, [2]int{b.pack, b.block})
			}
			shapes[k] = s
		}
		out[c.id] = shapes
	}

	return out
}

// captureIDs returns the capture_ids of p's captures, in order.
func captureIDs(p *mergeIteratePlan) []UUID {
	out := make([]UUID, len(p.captures))
	for k := range p.captures {
		out[k] = p.captures[k].id
	}

	return out
}

// timedBlocks returns data records, one block for each list of seqs, the record of seq s at ts0 + s.
func timedBlocks(ts0 int64, blocks ...[]uint64) []footerTestStep {
	var steps []footerTestStep
	for _, seqs := range blocks {
		for i, seq := range seqs {
			steps = append(steps, footerTestStep{rec: testDataRecord(seq, ts0+int64(seq), 1), flush: i == len(seqs)-1})
		}
	}

	return steps
}

// toEquipment returns steps with every record's dir set to equipment-to-host.
func toEquipment(steps []footerTestStep) []footerTestStep {
	for i := range steps {
		steps[i].rec.Dir = DirEquipmentToHost
	}

	return steps
}

func TestCheckMergeIterate(t *testing.T) {
	t.Parallel()

	file := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, hourRecords(1, 3)).file
	// same is a second Reader of a's bytes, so it holds a's pack_id.
	a, same := mustOpen(t, file, ReaderOptions{}), mustOpen(t, file, ReaderOptions{})
	b := mustOpen(t, writeReaderPack(t, readerPackConfig{codec: CodecZstd}, hourRecords(1, 3)).file, ReaderOptions{})
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()

	inverted := Query{Filter: Filter{TimeFrom: new(int64(2)), TimeTo: new(int64(1))}}
	capture := MergeIterateOptions{Order: OrderCapture}
	tests := []struct {
		name    string
		ctx     context.Context
		readers []*Reader
		q       Query
		opts    MergeIterateOptions
		fn      func(*Item) error
		msg     string
	}{
		{name: "nil fn first", ctx: cancelled, readers: []*Reader{nil, a, a}, q: inverted, msg: "fn is nil"},
		{name: "zero order", ctx: cancelled, readers: []*Reader{nil}, q: inverted, fn: acceptItems, msg: "order unknown(0)"},
		{name: "unknown order", readers: []*Reader{a}, opts: MergeIterateOptions{Order: 3}, fn: acceptItems, msg: "order unknown(3)"},
		{name: "invalid query before readers", ctx: cancelled, readers: []*Reader{nil}, q: inverted, opts: capture, fn: acceptItems,
			msg: "TimeFrom 2 is after TimeTo 1"},
		{name: "nil reader before a repeated one", ctx: cancelled, readers: []*Reader{a, a, b, nil}, opts: capture, fn: acceptItems,
			msg: "reader 3 is nil"},
		{name: "repeated Reader before a repeated pack_id", ctx: cancelled, readers: []*Reader{a, same, b, a}, opts: capture, fn: acceptItems,
			msg: "readers 0 and 3 are the same Reader"},
		{name: "repeated pack_id before ctx", ctx: cancelled, readers: []*Reader{b, a, same}, opts: capture, fn: acceptItems,
			msg: "readers 1 and 2 hold the same pack_id " + UUID(a.hdr.PackID).String()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := tt.ctx
			if ctx == nil {
				ctx = t.Context()
			}
			_, err := checkMergeIterate(ctx, tt.readers, &tt.q, tt.opts, tt.fn)
			require.ErrorIs(t, err, ErrInvalidQuery)
			require.ErrorContains(t, err, tt.msg)
			assert.NotErrorIs(t, err, context.Canceled)
		})
	}

	t.Run("ctx", func(t *testing.T) {
		t.Parallel()

		_, err := checkMergeIterate(cancelled, []*Reader{a, b}, &Query{}, capture, acceptItems)
		require.ErrorIs(t, err, context.Canceled)
		require.NotErrorIs(t, err, ErrInvalidQuery)
		_, err = checkMergeIterate(cancelled, nil, &Query{}, capture, acceptItems)
		require.ErrorIs(t, err, context.Canceled, "ctx is checked with no reader")
	})

	t.Run("empty readers", func(t *testing.T) {
		t.Parallel()

		for _, readers := range [][]*Reader{nil, {}} {
			p, reads, err := planReads(t, t.Context(), readers, Query{}, MergeIterateOptions{Order: OrderTime}, nil)
			require.NoError(t, err)
			assert.Empty(t, reads)
			assert.Empty(t, p.captures)
			assert.Equal(t, Result{}, p.res)
		}
	})

	t.Run("defaults", func(t *testing.T) {
		t.Parallel()

		for _, tt := range []struct {
			in, want MergeIterateOptions
		}{
			{in: MergeIterateOptions{Order: OrderTime},
				want: MergeIterateOptions{Order: OrderTime, MaxHeldBytes: DefaultMaxHeldBytes, MaxConflicts: DefaultMaxConflicts}},
			{in: MergeIterateOptions{Order: OrderCapture, MaxHeldBytes: -1, MaxConflicts: -5},
				want: MergeIterateOptions{Order: OrderCapture, MaxHeldBytes: DefaultMaxHeldBytes, MaxConflicts: DefaultMaxConflicts}},
			{in: MergeIterateOptions{Order: OrderCapture, MaxHeldBytes: 1, MaxConflicts: 2},
				want: MergeIterateOptions{Order: OrderCapture, MaxHeldBytes: 1, MaxConflicts: 2}},
		} {
			got, err := checkMergeIterate(t.Context(), []*Reader{a, b}, &Query{}, tt.in, acceptItems)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)

			p := newMergeIteratePlan(got)
			assert.Equal(t, tt.want, p.opts)
			assert.Equal(t, heldBudget{limit: tt.want.MaxHeldBytes}, p.loader.budget)
		}
	})
}

func TestMergeIteratePlanDiagnostics(t *testing.T) {
	t.Parallel()

	finalized := func() []byte { return writeReaderPack(t, readerPackConfig{codec: CodecZstd}, hourRecords(2, 3)).file }
	covFile, _ := coveragePack(t, false)
	files := [][]byte{
		finalized(),
		invalidFooterFile(t, finalized()), // 1: the footer is not used
		covFile,                           // 2: coverage entries
		writeReaderPack(t, readerPackConfig{codec: CodecZstd, open: true}, hourRecords(2, 3)).file, // 3: not finalized
		finalized(),
	}
	readers := openAll(t, files...)
	q := Query{Filter: Filter{TimeFrom: new(blockTestHour + 150), TimeTo: new(blockTestHour + 400)}}

	// Each reader's defects are those Iterate reports for the same query, in reader order,
	// since no block of these packs fails its read.
	var want []Defect
	for i, r := range readers {
		res, err := r.Iterate(t.Context(), q, acceptItems)
		require.NoError(t, err)
		for _, d := range res.Incomplete {
			d.Pack = i
			want = append(want, d)
		}
	}
	require.Len(t, want, 5, "pack 2's four coverage defects, then pack 3's truncation")

	p, _ := mustPlan(t, readers, q)
	assert.Equal(t, want, p.res.Incomplete)
	for k, pack := range []int{2, 2, 2, 2, 3} {
		assert.Equal(t, pack, p.res.Incomplete[k].Pack, "defect %d", k)
	}
	assert.Equal(t, ReasonTruncated, p.res.Incomplete[4].Reason)
	require.Error(t, readers[1].footerErr)
	require.Error(t, readers[3].footerErr)
	assert.Equal(t, []PackError{{Pack: 1, Err: readers[1].footerErr}, {Pack: 3, Err: readers[3].footerErr}}, p.res.FooterErrs)
	assert.Nil(t, p.res.FooterErr)
	assert.Empty(t, p.res.Conflicts)

	// The Readers are unchanged: a Reader is shared across reads.
	assert.Zero(t, readers[3].openDefects[0].Pack)
	res, err := readers[2].Iterate(t.Context(), q, acceptItems)
	require.NoError(t, err)
	for _, d := range res.Incomplete {
		assert.Zero(t, d.Pack)
	}

	p, _ = mustPlan(t, []*Reader{readers[0], readers[4]}, q)
	assert.Nil(t, p.res.FooterErrs, "nil when every footer was used")
	assert.Nil(t, p.res.Incomplete)
}

func TestMergeIteratePlanPreRead(t *testing.T) {
	t.Parallel()

	// The same blocks, of several kinds, dirs and epochs, unfinalized and finalized.
	recs := pruneTestRecords()
	open := writeReaderPack(t, readerPackConfig{codec: CodecZstd, open: true}, recs).file
	closed := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, recs).file
	readers := openAll(t, closed, open)
	require.Len(t, readers[1].blocks, 4)
	require.False(t, readers[1].blocks[0].Indexed)

	p, reads := mustPlan(t, readers, Query{})
	assert.Equal(t, map[[2]int]int{{1, 0}: 1, {1, 1}: 1, {1, 2}: 1, {1, 3}: 1}, reads,
		"each walked block read once, no indexed block read")

	blocksOf := func(capture UUID) []planBlock {
		k := slices.IndexFunc(p.captures, func(c planCapture) bool { return c.id == capture })
		require.GreaterOrEqual(t, k, 0)
		var out []planBlock
		for _, c := range p.captures[k].clusters {
			out = append(out, c.blocks...)
		}

		return out
	}
	indexed := blocksOf(UUID(readers[0].hdr.CaptureID))
	computed := blocksOf(UUID(readers[1].hdr.CaptureID))
	require.Len(t, computed, 4)
	require.Len(t, indexed, 4)
	for k := range computed {
		want := indexed[k]
		want.pack = 1
		assert.Equal(t, want, computed[k], "block %d: the computed summary is what the index states", k)
	}
}

func TestMergeIteratePlanPreReadFailedBlock(t *testing.T) {
	t.Parallel()

	p := writeReaderPack(t, readerPackConfig{codec: CodecZstd, open: true}, hourRecords(3, 4))
	b := p.blocks[1]
	file := flipByte(p.file, b.offset+format.EnvelopeLen+uint64(b.onDiskLen-format.EnvelopeLen)/2)
	finalized := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, hourRecords(1, 3)).file
	readers := openAll(t, finalized, file)
	require.Len(t, readers[1].blocks, 3)

	plan, reads := mustPlan(t, readers, Query{})
	assert.Equal(t, map[[2]int]int{{1, 0}: 1, {1, 1}: 1, {1, 2}: 1}, reads, "the failed block is read once")
	require.Len(t, plan.res.Incomplete, 2)
	assert.Equal(t, ReasonTruncated, plan.res.Incomplete[0].Reason)
	def := plan.res.Incomplete[1]
	assert.Equal(t, ReasonCorruptBlock, def.Reason)
	assert.Equal(t, 1, def.Pack)
	assert.Equal(t, 1, def.Block)
	require.ErrorContains(t, def.Err, "body_crc")

	shapes := shapesOf(plan)[UUID(readers[1].hdr.CaptureID)]
	require.Len(t, shapes, 2, "the failed block takes no part")
	assert.Equal(t, [][2]int{{1, 0}}, shapes[0].blocks)
	assert.Equal(t, [][2]int{{1, 2}}, shapes[1].blocks)
}

func TestMergeIteratePlanPreReadPreflightDefect(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		file   func(t testing.TB) []byte
		opts   ReaderOptions
		block  int
		reason IncompleteReason
	}{
		{name: "over MaxBlockLen", file: func(t testing.TB) []byte {
			// Block 1 holds records of 4 KiB payloads, blocks 0 and 2 of 1 byte.
			recs := hourRecords(3, 40)
			for i := 40; i < 80; i++ {
				recs[i] = randomDataRecord(recs[i].Seq, recs[i].TSUTCNs, 4<<10)
			}

			return writeReaderPack(t, readerPackConfig{codec: CodecZstd, open: true}, recs).file
		}, opts: ReaderOptions{MaxBlockLen: 64 << 10}, block: 1, reason: ReasonLimit},
		{name: "inconsistent dimensions", file: func(t testing.TB) []byte { return hugeSectionPack(t, false) },
			block: hugeBlock, reason: ReasonCorruptBlock},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			finalized := mustOpen(t, writeReaderPack(t, readerPackConfig{codec: CodecZstd}, hourRecords(1, 3)).file, ReaderOptions{})
			walked := mustOpen(t, tt.file(t), tt.opts)
			require.Len(t, walked.blocks, 3)
			require.False(t, walked.blocks[tt.block].Indexed)

			p, reads := mustPlan(t, []*Reader{finalized, walked}, Query{})
			want := map[[2]int]int{}
			for k := range 3 {
				if k != tt.block {
					want[[2]int{1, k}] = 1
				}
			}
			assert.Equal(t, want, reads, "the block failing its preflight is not read")

			require.Len(t, p.res.Incomplete, 2)
			assert.Equal(t, ReasonTruncated, p.res.Incomplete[0].Reason)
			def := p.res.Incomplete[1]
			assert.Equal(t, tt.reason, def.Reason, "%v", def.Err)
			assert.Equal(t, 1, def.Pack)
			assert.Equal(t, tt.block, def.Block)

			var blocks [][2]int
			for _, c := range shapesOf(p)[UUID(walked.hdr.CaptureID)] {
				blocks = append(blocks, c.blocks...)
			}
			assert.NotContains(t, blocks, [2]int{1, tt.block}, "the block takes no part")
			assert.Len(t, blocks, 2)
		})
	}
}

func TestMergeIteratePlanPreReadErrors(t *testing.T) {
	t.Parallel()

	file := writeReaderPack(t, readerPackConfig{codec: CodecZstd, open: true}, hourRecords(3, 4)).file
	finalized := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, hourRecords(1, 3)).file

	for _, at := range []int{0, 1, 2} {
		t.Run(fmt.Sprintf("ReadAt fails at block %d", at), func(t *testing.T) {
			t.Parallel()

			g := newGatedReader(file)
			walked, err := openReader(t.Context(), g, int64(len(file)), ReaderOptions{}, false)
			require.NoError(t, err)
			require.Len(t, walked.blocks, 3)
			off := int64(walked.blocks[at].Offset)
			g.fail = func(o int64, _ int) bool { return o == off }

			p, reads, err := planReads(t, t.Context(), []*Reader{mustOpen(t, finalized, ReaderOptions{}), walked}, Query{},
				MergeIterateOptions{Order: OrderTime}, nil)
			require.ErrorIs(t, err, errInjected)
			require.ErrorContains(t, err, fmt.Sprintf("tracepack: pack 1, block %d:", at))
			for k := range 3 {
				want := 1
				if k > at {
					want = 0
				}
				assert.Equal(t, want, reads[[2]int{1, k}], "block %d: no read after the failure", k)
			}
			require.Len(t, p.res.Incomplete, 1, "the result so far: the truncation")
			assert.Equal(t, 1, p.res.Incomplete[0].Pack)
		})
	}

	t.Run("budget", func(t *testing.T) {
		t.Parallel()

		r := mustOpen(t, file, ReaderOptions{})
		s, def := r.preflightBlock(0)
		require.Nil(t, def)
		_, reads, err := planReads(t, t.Context(), []*Reader{r}, Query{}, MergeIterateOptions{Order: OrderCapture, MaxHeldBytes: s.total - 1}, nil)
		require.ErrorIs(t, err, ErrReadLimit)
		require.ErrorContains(t, err, "pack 0, block 0")
		assert.Empty(t, reads)

		// A budget of the largest walked block admits the pre-read, which holds one block at a time.
		largest := int64(0)
		for i := range r.blocks {
			s, def := r.preflightBlock(i)
			require.Nil(t, def)
			largest = max(largest, s.total)
		}
		_, reads, err = planReads(t, t.Context(), []*Reader{r}, Query{}, MergeIterateOptions{Order: OrderCapture, MaxHeldBytes: largest}, nil)
		require.NoError(t, err)
		assert.Len(t, reads, 3)
	})

	t.Run("ctx cancelled", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		r := mustOpen(t, file, ReaderOptions{})
		_, reads, err := planReads(t, ctx, []*Reader{r}, Query{}, MergeIterateOptions{Order: OrderCapture},
			func(_, block int) {
				if block == 1 {
					cancel()
				}
			})
		require.ErrorIs(t, err, context.Canceled)
		require.ErrorContains(t, err, "tracepack: merge iterate:")
		assert.Equal(t, map[[2]int]int{{0, 0}: 1, {0, 1}: 1}, reads, "no read after the cancellation")
	})
}

// mixedWalkedPack returns an unfinalized pack of four hourly blocks,
// each needing the most of one of the four buffers of a block read:
// block 0 of raw, its payloads incompressible;
// block 1 of decoded, its payloads compressible;
// block 2 of rows, its few record headers widened by 200 bytes;
// block 3 of offsets, its many records of plain headers.
func mixedWalkedPack(t testing.TB) []byte {
	t.Helper()

	var recs []Record
	add := func(hour int, n int, rec func(seq uint64, ts int64) Record) {
		for i := range n {
			recs = append(recs, rec(uint64(len(recs)), blockTestHour+int64(hour)*hourNs+int64(i)))
		}
	}
	add(0, 2, func(seq uint64, ts int64) Record { return randomDataRecord(seq, ts, 64<<10) })
	add(1, 4, func(seq uint64, ts int64) Record { return textDataRecord(seq, ts, 64<<10) })
	add(2, 300, func(seq uint64, ts int64) Record { return testDataRecord(seq, ts, 1) })
	add(3, 1000, func(seq uint64, ts int64) Record { return testDataRecord(seq, ts, 1) })
	file := writeReaderPack(t, readerPackConfig{codec: CodecZstd, open: true}, recs).file

	return rebuildPack(t, file, func(i int, b *testBlock) {
		if i == 2 {
			for j := range b.headers {
				b.headers[j].Extra = make([]byte, 200)
			}
		}
	})
}

// TestMergeIteratePlanPreReadDropsEachBuffer pre-reads mixedWalkedPack to each outcome of a block's pre-read:
// its summary, its defect, a ReadAt error, ctx cancelled.
// When each pre-read buffer is allocated, every earlier one is dropped,
// and the bytes held are only the block's own reservation;
// after planning every buffer is dropped.
// A read over the pack then holds at most the largest block.
func TestMergeIteratePlanPreReadDropsEachBuffer(t *testing.T) {
	t.Parallel()

	file := mixedWalkedPack(t)
	r := mustOpen(t, file, ReaderOptions{})
	require.Len(t, r.blocks, 4)
	sizes := make([]blockSizes, len(r.blocks))
	for i := range r.blocks {
		require.False(t, r.blocks[i].Indexed)
		s, def := r.preflightBlock(i)
		require.Nil(t, def)
		sizes[i] = s
	}
	largestAt := func(of func(s *blockSizes) int) int {
		k := 0
		for i := range sizes {
			if of(&sizes[i]) > of(&sizes[k]) {
				k = i
			}
		}

		return k
	}
	require.Equal(t, []int{0, 1, 2, 3}, []int{
		largestAt(func(s *blockSizes) int { return s.raw }),
		largestAt(func(s *blockSizes) int { return s.decoded }),
		largestAt(func(s *blockSizes) int { return s.rows }),
		largestAt(func(s *blockSizes) int { return s.offs }),
	}, "raw, decoded, rows and offsets each largest in a block of its own: %+v", sizes)

	type preReadCase struct {
		name   string
		reader func(t *testing.T) *Reader
		// cancelAt is the block whose pre-read cancels ctx; -1 for none.
		cancelAt int
		// defectAt is the block whose pre-read is a defect; -1 for none.
		defectAt int
		// err is the error build returns; nil for none.
		err error
		// read is the number of blocks read.
		read int
	}
	tests := []preReadCase{
		{
			name:   "summaries",
			reader: func(t *testing.T) *Reader { return mustOpen(t, file, ReaderOptions{}) }, cancelAt: -1, defectAt: -1, read: 4,
		},
	}
	for k := range r.blocks {
		b := &r.blocks[k]
		corrupt := flipByte(file, b.Offset+format.EnvelopeLen+uint64(b.OnDiskLen-format.EnvelopeLen)/2)
		tests = append(tests, preReadCase{
			name:   fmt.Sprintf("defect at block %d", k),
			reader: func(t *testing.T) *Reader { return mustOpen(t, corrupt, ReaderOptions{}) }, cancelAt: -1, defectAt: k, read: 4,
		}, preReadCase{
			name: fmt.Sprintf("ReadAt fails at block %d", k),
			reader: func(t *testing.T) *Reader {
				g := newGatedReader(file)
				walked, err := openReader(t.Context(), g, int64(len(file)), ReaderOptions{}, false)
				require.NoError(t, err)
				off := int64(b.Offset)
				g.fail = func(o int64, _ int) bool { return o == off }

				return walked
			}, cancelAt: -1, defectAt: -1, err: errInjected, read: k + 1,
		})
		if k < len(r.blocks)-1 {
			tests = append(tests, preReadCase{
				name:   fmt.Sprintf("ctx cancelled at block %d", k),
				reader: func(t *testing.T) *Reader { return mustOpen(t, file, ReaderOptions{}) }, cancelAt: k, defectAt: -1, err: context.Canceled, read: k + 1,
			})
		}
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			q := Query{}
			readers := []*Reader{tt.reader(t)}
			o, err := checkMergeIterate(ctx, readers, &q, MergeIterateOptions{Order: OrderCapture}, acceptItems)
			require.NoError(t, err)
			p := newMergeIteratePlan(o)
			var bufs []*blockBuf
			p.loader.readHook = func(_, block int, buf *blockBuf) {
				for k, prev := range bufs {
					assert.True(t, prev.raw == nil && prev.decoded == nil && prev.rows == nil && prev.offs == nil,
						"block %d: the buffers of block %d dropped before it is allocated", block, k)
				}
				assert.Equal(t, sizes[block].total, p.loader.budget.held, "block %d: only its reservation held", block)
				bufs = append(bufs, buf)
				if block == tt.cancelAt {
					cancel()
				}
			}

			err = p.build(ctx, readers, &q)
			if tt.err != nil {
				require.ErrorIs(t, err, tt.err)
			} else {
				require.NoError(t, err)
			}
			require.Len(t, bufs, tt.read)
			if tt.defectAt >= 0 {
				require.NotEmpty(t, p.res.Incomplete)
				def := p.res.Incomplete[len(p.res.Incomplete)-1]
				assert.Equal(t, ReasonCorruptBlock, def.Reason)
				assert.Equal(t, tt.defectAt, def.Block)
			}
			for _, buf := range bufs {
				requireDropped(t, buf)
			}
			assert.Zero(t, p.loader.budget.held)
		})
	}

	t.Run("resolution", func(t *testing.T) {
		t.Parallel()

		largest := sizes[largestAt(func(s *blockSizes) int { return int(s.total) })].total
		p, reads, err := planReads(t, t.Context(), openAll(t, file), Query{}, MergeIterateOptions{Order: OrderCapture, MaxHeldBytes: largest}, nil)
		require.NoError(t, err)
		require.Len(t, reads, 4)

		maxHeld := int64(0)
		p.loader.readHook = func(int, int, *blockBuf) { maxHeld = max(maxHeld, p.loader.budget.held) }
		calls := 0
		require.NoError(t, p.runCapture(t.Context(), &Query{}, func(*Item) error {
			calls++
			maxHeld = max(maxHeld, p.loader.budget.held)

			return nil
		}))
		assert.Equal(t, 1306, calls)
		assert.LessOrEqual(t, maxHeld, largest)
		assert.Zero(t, p.loader.budget.held)
	})
}

func TestMergeIteratePlanClusters(t *testing.T) {
	t.Parallel()

	h := blockTestHour
	files := [][]byte{
		// 0: blocks 10-13 and 20-23.
		planTestPack(t, UUID{1}, captureHigh, false, timedBlocks(h, seqRun(10, 13), seqRun(20, 23))),
		// 1: blocks 12-15, overlapping pack 0's first, five ns earlier, and 30-33, overlapping nothing.
		planTestPack(t, UUID{2}, captureHigh, false, timedBlocks(h-5, seqRun(12, 15), seqRun(30, 33))),
		// 2: another capture, which sorts first.
		planTestPack(t, UUID{3}, captureLow, false, timedBlocks(h, seqRun(1, 4))),
		// 3: walked, its first block starting at pack 0's second block's first seq, its second chaining nothing.
		planTestPack(t, UUID{4}, captureHigh, true, timedBlocks(h, seqRun(20, 21), seqRun(40, 41))),
	}
	readers := openAll(t, files...)
	require.False(t, readers[3].blocks[0].Indexed)

	p, reads := mustPlan(t, readers, Query{})
	assert.Equal(t, map[[2]int]int{{3, 0}: 1, {3, 1}: 1}, reads)
	assert.Equal(t, []UUID{captureLow, captureHigh}, captureIDs(p))
	assert.Equal(t, map[UUID][]clusterShape{
		captureLow: {{first: 1, last: 4, blocks: [][2]int{{2, 0}}, tsMin: h + 1}},
		captureHigh: {
			{first: 10, last: 15, blocks: [][2]int{{0, 0}, {1, 0}}, tsMin: h + 7},
			{first: 20, last: 23, blocks: [][2]int{{0, 1}, {3, 0}}, tsMin: h + 20},
			{first: 30, last: 33, blocks: [][2]int{{1, 1}}, tsMin: h + 25},
			{first: 40, last: 41, blocks: [][2]int{{3, 1}}, tsMin: h + 40},
		},
	}, shapesOf(p))

	// Equal first seqs keep the readers' order, whichever comes first.
	p, _ = mustPlan(t, []*Reader{readers[3], readers[0]}, Query{})
	assert.Equal(t, [][2]int{{0, 0}, {1, 1}}, shapesOf(p)[captureHigh][1].blocks)
}

func TestMergeIteratePlanExclusion(t *testing.T) {
	t.Parallel()

	h := blockTestHour
	files := [][]byte{
		// 0: blocks 10-13 in hour 0 and 20-23 in hour 1.
		planTestPack(t, UUID{1}, captureHigh, false, append(timedBlocks(h, seqRun(10, 13)), timedBlocks(h+hourNs, seqRun(20, 23))...)),
		// 1: blocks 12-15 and 20-21, both in hour 1.
		planTestPack(t, UUID{2}, captureHigh, false, timedBlocks(h+hourNs, seqRun(12, 15), seqRun(20, 21))),
		// 2: walked, blocks 1-2 in hour 0 and 40-43 in hour 1, the only block of equipment-to-host records.
		planTestPack(t, UUID{3}, captureHigh, true, append(timedBlocks(h, seqRun(1, 2)), toEquipment(timedBlocks(h+hourNs, seqRun(40, 43)))...)),
	}
	readers := openAll(t, files...)
	require.False(t, readers[2].blocks[0].Indexed)
	require.True(t, readers[1].blocks[0].Indexed)

	tests := []struct {
		name     string
		f        Filter
		excluded []bool
	}{
		{name: "no filter", excluded: []bool{false, false, false, false}},
		{
			// Cluster 10-15 holds pack 0's block in hour 0 and pack 1's in hour 1: kept, both blocks to be read.
			name: "hour 0", f: Filter{TimeFrom: new(h), TimeTo: new(h + hourNs)},
			excluded: []bool{false, false, true, true},
		},
		{name: "hour 1", f: Filter{TimeFrom: new(h + hourNs)}, excluded: []bool{true, false, false, false}},
		{name: "kinds", f: Filter{Kinds: []Kind{KindControl}}, excluded: []bool{true, true, true, true}},
		{name: "dirs", f: Filter{Dirs: []Dir{DirEquipmentToHost, DirHostToEquipment}}, excluded: []bool{false, false, false, false}},
		{name: "host to equipment", f: Filter{Dirs: []Dir{DirHostToEquipment}}, excluded: []bool{false, false, false, true}},
		{name: "equipment to host", f: Filter{Dirs: []Dir{DirEquipmentToHost}}, excluded: []bool{true, true, true, false}},
		{name: "epochs", f: Filter{Epochs: []uint32{2}}, excluded: []bool{true, true, true, true}},
		{name: "kind out of range", f: Filter{Kinds: []Kind{Kind(math.MaxUint8)}}, excluded: []bool{true, true, true, true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p, reads := mustPlan(t, readers, Query{Filter: tt.f})
			assert.Equal(t, map[[2]int]int{{2, 0}: 1, {2, 1}: 1}, reads, "only the walked blocks are read, once each")
			shapes := shapesOf(p)[captureHigh]
			require.Len(t, shapes, 4)
			assert.Equal(t, [][2]int{{2, 0}}, shapes[0].blocks)
			assert.Equal(t, [][2]int{{0, 0}, {1, 0}}, shapes[1].blocks)
			assert.Equal(t, [][2]int{{0, 1}, {1, 1}}, shapes[2].blocks)
			assert.Equal(t, [][2]int{{2, 1}}, shapes[3].blocks)
			assert.Equal(t, h+10, shapes[1].tsMin, "the smallest ts_min of the cluster's blocks")
			got := make([]bool, len(shapes))
			for k := range shapes {
				got[k] = shapes[k].excluded
			}
			assert.Equal(t, tt.excluded, got)
		})
	}
}
