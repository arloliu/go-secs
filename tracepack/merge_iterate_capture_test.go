package tracepack

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// yieldView is what an Item yielded names: its representative's pack and block, its record's seq and its Conflict flag.
type yieldView struct {
	pack, block int
	seq         uint64
	conflict    bool
}

// captureRead validates and plans the read of q over readers in capture order with opts, its Order set,
// then reads it, passing each Item to fn when fn is set.
// It returns a copy of every Item passed to fn, the Result and the error,
// requiring nothing to stay held, whatever the outcome.
func captureRead(t testing.TB, ctx context.Context, readers []*Reader, q Query, opts MergeIterateOptions, fn func(*Item) error,
) ([]iterItem, Result, error) {
	t.Helper()

	var items []iterItem
	yield := func(it *Item) error {
		items = append(items, copyItem(it))
		if fn != nil {
			return fn(it)
		}

		return nil
	}
	opts.Order = OrderCapture
	o, err := checkMergeIterate(ctx, readers, &q, opts, yield)
	if err != nil {
		return nil, Result{}, err
	}
	p := newMergeIteratePlan(o)
	if err = p.build(ctx, readers, &q); err == nil {
		err = p.runCapture(ctx, &q, yield)
	}
	assert.Zero(t, p.loader.budget.held, "nothing stays held")
	res, err := p.rs.end(p.res, err)

	return items, res, err
}

// mustCaptureRead reads q over files in capture order, requiring no error.
func mustCaptureRead(t testing.TB, q Query, files ...[]byte) ([]iterItem, Result) {
	t.Helper()

	items, res, err := captureRead(t, t.Context(), openAll(t, files...), q, MergeIterateOptions{}, nil)
	require.NoError(t, err)

	return items, res
}

// yieldViews returns what each item names, in order.
func yieldViews(items []iterItem) []yieldView {
	out := make([]yieldView, len(items))
	for i := range items {
		out[i] = yieldView{pack: items[i].pack, block: items[i].block, seq: items[i].rec.Seq, conflict: items[i].conflict}
	}

	return out
}

// blockAt returns data records of seqs in one block, the k-th at blockTestHour + ts + k,
// so the block's ts_min is blockTestHour + ts.
func blockAt(ts int64, seqs ...uint64) []footerTestStep {
	steps := make([]footerTestStep, len(seqs))
	for k, seq := range seqs {
		steps[k] = footerTestStep{rec: testDataRecord(seq, blockTestHour+ts+int64(k), 1), flush: k == len(seqs)-1}
	}

	return steps
}

// TestRunCaptureInterleaves reads two captures whose blocks interleave in ts_min:
// each capture's records come in ascending seq,
// the next from the capture whose next record lies in the block with the smaller ts_min,
// an equal ts_min going to the smaller capture_id, whatever the readers' order.
func TestRunCaptureInterleaves(t *testing.T) {
	t.Parallel()

	high := planTestPack(t, seg0, captureHigh, false, slices.Concat(blockAt(10, 1), blockAt(20, 2, 3), blockAt(60, 4)))
	low := planTestPack(t, seg1, captureLow, false, slices.Concat(blockAt(10, 1, 2), blockAt(30, 3, 4), blockAt(50, 5)))
	items, res := mustCaptureRead(t, Query{Payloads: true}, high, low)
	want := []yieldView{
		{pack: 1, block: 0, seq: 1}, {pack: 1, block: 0, seq: 2},
		{pack: 0, block: 0, seq: 1}, {pack: 0, block: 1, seq: 2}, {pack: 0, block: 1, seq: 3},
		{pack: 1, block: 1, seq: 3}, {pack: 1, block: 1, seq: 4}, {pack: 1, block: 2, seq: 5},
		{pack: 0, block: 2, seq: 4},
	}
	assert.Equal(t, want, yieldViews(items))
	for _, it := range items {
		assert.Equal(t, blockTestFrame, it.rec.Payload, "seq %d", it.rec.Seq)
	}
	assert.Empty(t, res.Conflicts)
	assert.Empty(t, res.Incomplete)
}

// TestRunCaptureSeqOrder reads one capture over two packs whose blocks form three clusters:
// its records come in ascending seq across the clusters and the packs,
// and a record both packs hold is yielded once, from its representative, the first holding block in cluster order.
func TestRunCaptureSeqOrder(t *testing.T) {
	t.Parallel()

	// Clusters: [1, 2] of pack 1; [3, 5] of pack 0's first block and pack 1's second, which hold 3 and 4 alike;
	// [8, 9] of pack 0.
	a := planTestPack(t, seg0, captureLow, false, timedBlocks(blockTestHour, []uint64{3, 4, 5}, []uint64{8, 9}))
	b := planTestPack(t, seg1, captureLow, false, timedBlocks(blockTestHour, []uint64{1, 2}, []uint64{3, 4}))
	items, res := mustCaptureRead(t, Query{}, a, b)
	want := []yieldView{
		{pack: 1, block: 0, seq: 1}, {pack: 1, block: 0, seq: 2},
		{pack: 0, block: 0, seq: 3}, {pack: 0, block: 0, seq: 4}, {pack: 0, block: 0, seq: 5},
		{pack: 0, block: 1, seq: 8}, {pack: 0, block: 1, seq: 9},
	}
	assert.Equal(t, want, yieldViews(items))
	assert.Empty(t, res.Conflicts)
}

// TestRunCaptureFilteredHead reads a capture whose first record the filter rejects,
// in a block of a later ts_min than the block of its next record, a backward step of time:
// the capture is scheduled by its next selected record, so its rejected record does not delay it.
func TestRunCaptureFilteredHead(t *testing.T) {
	t.Parallel()

	// The filter tests the system bytes, which no footer states, so no cluster is excluded.
	low := planTestPack(t, seg0, captureLow, false, slices.Concat(changed(blockAt(60, 1), 1, 1), blockAt(10, 2)))
	high := planTestPack(t, seg1, captureHigh, false, blockAt(30, 1))
	q := Query{Filter: Filter{SystemBytes: &[4]byte{0xDE, 0xAD, 0xBE, 0xEF}}}
	items, _ := mustCaptureRead(t, q, low, high)
	assert.Equal(t, []yieldView{{pack: 0, block: 1, seq: 2}, {pack: 1, block: 0, seq: 1}}, yieldViews(items))
}

// TestRunCaptureVersions reads a capture holding several versions of one seq, each the last record of its block,
// beside another capture:
// the selected versions are yielded in version order, each scheduled by the ts_min of its own representative's block,
// before the capture's next seq, with the other capture's records between them.
func TestRunCaptureVersions(t *testing.T) {
	t.Parallel()

	t.Run("two versions", func(t *testing.T) {
		t.Parallel()

		// Version 0's block has ts_min 100, version 1's 0; the other capture's block 50.
		files := [][]byte{
			planTestPack(t, seg0, captureLow, false, blockAt(100, 7)),
			planTestPack(t, seg1, captureLow, false, blockAt(0, 7)),
			planTestPack(t, seg2, captureHigh, false, blockAt(50, 7)),
		}
		items, res := mustCaptureRead(t, Query{}, files...)
		want := []yieldView{{pack: 2, seq: 7}, {pack: 0, seq: 7, conflict: true}, {pack: 1, seq: 7, conflict: true}}
		assert.Equal(t, want, yieldViews(items))
		assert.Equal(t, []Conflict{{CaptureID: captureLow, Seq: 7, Versions: [][]UUID{{seg0}, {seg1}}}}, res.Conflicts)
	})

	t.Run("the other capture between them", func(t *testing.T) {
		t.Parallel()

		// Version 0's block has ts_min 0, version 1's 100; the other capture's block 50.
		files := [][]byte{
			planTestPack(t, seg0, captureLow, false, blockAt(0, 7)),
			planTestPack(t, seg1, captureLow, false, blockAt(100, 7)),
			planTestPack(t, seg2, captureHigh, false, blockAt(50, 7)),
		}
		items, res := mustCaptureRead(t, Query{}, files...)
		want := []yieldView{{pack: 0, seq: 7, conflict: true}, {pack: 2, seq: 7}, {pack: 1, seq: 7, conflict: true}}
		assert.Equal(t, want, yieldViews(items))
		assert.Equal(t, []Conflict{{CaptureID: captureLow, Seq: 7, Versions: [][]UUID{{seg0}, {seg1}}}}, res.Conflicts)
	})

	t.Run("three versions, the second filtered out", func(t *testing.T) {
		t.Parallel()

		// Versions 0 and 1 differ in dir, which the filter rejects for version 1; version 2's block has ts_min 0.
		files := [][]byte{
			planTestPack(t, seg0, captureLow, false, blockAt(100, 7)),
			planTestPack(t, seg1, captureLow, false, toEquipment(blockAt(100, 7))),
			planTestPack(t, seg2, captureLow, false, blockAt(0, 7)),
			planTestPack(t, seg3, captureHigh, false, slices.Concat(blockAt(50, 1), blockAt(200, 2))),
		}
		q := Query{Filter: Filter{Dirs: []Dir{DirHostToEquipment}}}
		items, res := mustCaptureRead(t, q, files...)
		want := []yieldView{
			{pack: 3, block: 0, seq: 1},
			{pack: 0, seq: 7, conflict: true}, {pack: 2, seq: 7, conflict: true},
			{pack: 3, block: 1, seq: 2},
		}
		assert.Equal(t, want, yieldViews(items))
		assert.Equal(t, []Conflict{{CaptureID: captureLow, Seq: 7, Versions: [][]UUID{{seg0}, {seg1}, {seg2}}}}, res.Conflicts)
	})
}

// TestRunCaptureConflictOrder reads two captures whose first seq conflicts and is filtered out whole:
// the captures are started in ascending capture_id, whatever the readers' order,
// so their first conflicts are listed in that order;
// later conflicts follow the order the read advances the captures in.
// A second read of the same readers yields the same items and lists the same conflicts.
func TestRunCaptureConflictOrder(t *testing.T) {
	t.Parallel()

	// Each capture's two packs hold seqs 1 to 3 in one block, 1 and 3 in two versions; the filter rejects 1.
	steps := func(ts int64) []footerTestStep {
		s := blockAt(ts, 1, 2, 3)
		s[0].rec.Dir = DirEquipmentToHost

		return s
	}
	readers := openAll(t,
		planTestPack(t, seg0, captureHigh, false, steps(10)),
		planTestPack(t, seg1, captureLow, false, steps(50)),
		planTestPack(t, seg2, captureHigh, false, changed(steps(10), 1, 1, 3)),
		planTestPack(t, seg3, captureLow, false, changed(steps(50), 1, 1, 3)),
	)
	q := Query{Filter: Filter{Dirs: []Dir{DirHostToEquipment}}}
	conflictAt := func(capture UUID, seq uint64, a, b UUID) Conflict {
		return Conflict{CaptureID: capture, Seq: seq, Versions: [][]UUID{{a}, {b}}}
	}
	wantConflicts := []Conflict{
		conflictAt(captureLow, 1, seg1, seg3), conflictAt(captureHigh, 1, seg0, seg2),
		conflictAt(captureHigh, 3, seg0, seg2), conflictAt(captureLow, 3, seg1, seg3),
	}
	want := []yieldView{
		{pack: 0, seq: 2}, {pack: 0, seq: 3, conflict: true}, {pack: 2, seq: 3, conflict: true},
		{pack: 1, seq: 2}, {pack: 1, seq: 3, conflict: true}, {pack: 3, seq: 3, conflict: true},
	}

	items, res, err := captureRead(t, t.Context(), readers, q, MergeIterateOptions{}, nil)
	require.NoError(t, err)
	assert.Equal(t, want, yieldViews(items))
	assert.Equal(t, wantConflicts, res.Conflicts)

	again, res2, err := captureRead(t, t.Context(), readers, q, MergeIterateOptions{}, nil)
	require.NoError(t, err)
	assert.Equal(t, items, again)
	assert.Equal(t, res, res2)
}

// TestRunCaptureExcluded reads captures whose clusters the query excludes:
// an excluded capture yields nothing and is not read,
// a cluster excluded whole is not read and its conflict is not listed,
// and a read with nothing to yield calls fn never and returns only the planning's diagnostics.
func TestRunCaptureExcluded(t *testing.T) {
	t.Parallel()

	low := planTestPack(t, seg0, captureLow, false, timedBlocks(blockTestHour, []uint64{1, 2}, []uint64{3}))
	high := planTestPack(t, seg1, captureHigh, false, timedBlocks(blockTestHour+1000, []uint64{1, 2}, []uint64{3}))
	open := planTestPack(t, seg2, captureHigh, true, timedBlocks(blockTestHour+1000, []uint64{4, 5}))
	early := Query{Filter: Filter{TimeTo: new(blockTestHour + 500)}}
	called := func(*Item) error {
		t.Error("fn called")

		return errStopIterate
	}

	t.Run("one capture excluded", func(t *testing.T) {
		t.Parallel()

		readers := openAll(t, high, low)
		var read []int
		o, err := checkMergeIterate(t.Context(), readers, &early, MergeIterateOptions{Order: OrderCapture}, acceptItems)
		require.NoError(t, err)
		p := newMergeIteratePlan(o)
		p.loader.readHook = func(pack, _ int, _ *blockBuf) { read = append(read, pack) }
		require.NoError(t, p.build(t.Context(), readers, &early))
		var items []yieldView
		require.NoError(t, p.runCapture(t.Context(), &early, func(it *Item) error {
			items = append(items, yieldView{pack: it.Pack, block: it.Block, seq: it.Record.Seq})
			return nil
		}))
		assert.Equal(t, []yieldView{{pack: 1, seq: 1}, {pack: 1, seq: 2}, {pack: 1, block: 1, seq: 3}}, items)
		assert.Equal(t, []int{1, 1}, read, "only the kept capture's blocks are read")
		assert.Zero(t, p.loader.budget.held)
	})

	t.Run("a conflicting cluster excluded whole", func(t *testing.T) {
		t.Parallel()

		// The cluster of seqs 3 and 4 lies wholly after the range, and its two copies of seq 3 differ:
		// it is not read, and its conflict is not listed (the tracepack semantics specification §7.4, Exclusion).
		late := timedBlocks(blockTestHour+1000, []uint64{3, 4})
		a := planTestPack(t, seg0, captureLow, false, slices.Concat(timedBlocks(blockTestHour, []uint64{1, 2}), late))
		b := planTestPack(t, seg1, captureLow, false, changed(late, 1, 3))
		read := func(q Query) ([]yieldView, []Conflict, [][2]int) {
			readers := openAll(t, a, b)
			var items []yieldView
			var blocks [][2]int
			yield := func(it *Item) error {
				items = append(items, yieldView{pack: it.Pack, block: it.Block, seq: it.Record.Seq, conflict: it.Conflict})
				return nil
			}
			o, err := checkMergeIterate(t.Context(), readers, &q, MergeIterateOptions{Order: OrderCapture}, yield)
			require.NoError(t, err)
			p := newMergeIteratePlan(o)
			p.loader.readHook = func(pack, block int, _ *blockBuf) { blocks = append(blocks, [2]int{pack, block}) }
			require.NoError(t, p.build(t.Context(), readers, &q))
			err = p.runCapture(t.Context(), &q, yield)
			res, err := p.rs.end(p.res, err)
			require.NoError(t, err)
			assert.Zero(t, p.loader.budget.held)

			return items, res.Conflicts, blocks
		}

		items, conflicts, blocks := read(early)
		assert.Equal(t, []yieldView{{pack: 0, seq: 1}, {pack: 0, seq: 2}}, items)
		assert.Empty(t, conflicts)
		assert.Equal(t, [][2]int{{0, 0}}, blocks, "only the kept cluster's block is read")

		// Read whole, the same cluster is compared and its conflict listed.
		items, conflicts, blocks = read(Query{})
		assert.Equal(t, []yieldView{
			{pack: 0, seq: 1}, {pack: 0, seq: 2},
			{pack: 0, block: 1, seq: 3, conflict: true}, {pack: 1, seq: 3, conflict: true}, {pack: 0, block: 1, seq: 4},
		}, items)
		assert.Equal(t, []Conflict{{CaptureID: captureLow, Seq: 3, Versions: [][]UUID{{seg0}, {seg1}}}}, conflicts)
		assert.ElementsMatch(t, [][2]int{{0, 0}, {0, 1}, {1, 0}}, blocks)
	})

	t.Run("no readers", func(t *testing.T) {
		t.Parallel()

		_, res, err := captureRead(t, t.Context(), nil, Query{}, MergeIterateOptions{}, called)
		require.NoError(t, err)
		assert.Equal(t, Result{}, res)
	})

	t.Run("every cluster excluded", func(t *testing.T) {
		t.Parallel()

		late := Query{Filter: Filter{TimeFrom: new(blockTestHour + 2000)}}
		readers := openAll(t, low, high, open)
		_, res, err := captureRead(t, t.Context(), readers, late, MergeIterateOptions{}, called)
		require.NoError(t, err)
		require.Len(t, res.Incomplete, 1)
		assert.Equal(t, ReasonTruncated, res.Incomplete[0].Reason)
		assert.Equal(t, 2, res.Incomplete[0].Pack)
		assert.Equal(t, []PackError{{Pack: 2, Err: readers[2].footerErr}}, res.FooterErrs)
		assert.Empty(t, res.Conflicts)
	})
}

// TestRunCaptureExits ends reads in capture order while a capture holds two queued versions of one seq:
// a reservation over MaxHeldBytes, an error of fn, and the cancellation of ctx.
// Each returns its error with nothing held and no call of fn after it.
func TestRunCaptureExits(t *testing.T) {
	t.Parallel()

	// captureLow holds two versions of seq 1 at ts_min 30, queued together;
	// captureHigh's first block, at 10, is read first, then its second, of four records.
	v0 := planTestPack(t, seg0, captureLow, false, blockAt(30, 1))
	v1 := planTestPack(t, seg1, captureLow, false, changed(blockAt(30, 1), 1, 1))
	other := planTestPack(t, seg2, captureHigh, false, slices.Concat(blockAt(10, 1), blockAt(20, 2, 3, 4, 5)))
	conflicts := []Conflict{{CaptureID: captureLow, Seq: 1, Versions: [][]UUID{{seg0}, {seg1}}}}

	t.Run("MaxHeldBytes", func(t *testing.T) {
		t.Parallel()

		// Every first block fits exactly; once the other capture's first record is yielded,
		// its second block, larger than its first, does not.
		limit := heldSize(t, v0) + heldSize(t, v1) + heldSize(t, other)
		items, res, err := captureRead(t, t.Context(), openAll(t, v0, v1, other), Query{},
			MergeIterateOptions{MaxHeldBytes: limit}, nil)
		require.ErrorIs(t, err, ErrReadLimit)
		require.ErrorContains(t, err, "tracepack: pack 2, block 1:")
		assert.Equal(t, []yieldView{{pack: 2, seq: 1}}, yieldViews(items))
		assert.Equal(t, conflicts, res.Conflicts)
	})

	t.Run("fn error", func(t *testing.T) {
		t.Parallel()

		items, res, err := captureRead(t, t.Context(), openAll(t, v0, v1), Query{}, MergeIterateOptions{},
			func(*Item) error { return errStopIterate })
		require.ErrorIs(t, err, errStopIterate)
		assert.Equal(t, []yieldView{{pack: 0, seq: 1, conflict: true}}, yieldViews(items), "no call after the error")
		assert.Equal(t, conflicts, res.Conflicts)
	})

	t.Run("ctx done in fn", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		items, _, err := captureRead(t, ctx, openAll(t, v0, v1, other), Query{}, MergeIterateOptions{},
			func(*Item) error { cancel(); return nil })
		require.ErrorIs(t, err, context.Canceled)
		assert.Equal(t, []yieldView{{pack: 2, seq: 1}}, yieldViews(items), "stopped at the load of the next block")
	})
}

// TestRunCaptureCancel cancels ctx in the k-th call of fn of a read of 7000 records,
// pack 0 holding seqs 1 to 7000 in blocks of 1000 and pack 1 holding seqs 1 to 2048 alike in blocks of 1024,
// so the read gathers its 4096th copy at seq 2048 and its 8192nd at seq 6144:
// the read stops at the next check of ctx, which is the check before the 4096th call when no block load
// or resolution check comes first, and returns ctx's error;
// fn's own error wins over the cancellation.
func TestRunCaptureCancel(t *testing.T) {
	t.Parallel()

	a := mergePack(t, seg0, nil, hourSteps(1, 7000, 1000))
	b := mergePack(t, seg1, nil, hourSteps(1, 2048, 1024))
	require.Len(t, mustOpen(t, a, ReaderOptions{}).Blocks(), 7)
	tests := []struct {
		name   string
		k      int
		calls  int
		fnErr  error
		reason string
	}{
		{name: "the 4095th call", k: 4095, calls: 4095, reason: "the check before the 4096th call"},
		{name: "the 4001st call", k: 4001, calls: 4095, reason: "the check before the 4096th call"},
		{name: "the 4096th call", k: 4096, calls: 5000, reason: "the load of the block of seq 5001"},
		{name: "the 6100th call", k: 6100, calls: 6143, reason: "the resolution check at the 8192nd copy"},
		{name: "the last call", k: 7000, calls: 7000, reason: "the check before returning"},
		{name: "fn's error", k: 10, calls: 10, fnErr: errStopIterate},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			_, _, err := captureRead(t, ctx, openAll(t, a, b), Query{}, MergeIterateOptions{}, func(*Item) error {
				calls++
				if calls == tt.k {
					cancel()

					return tt.fnErr
				}

				return nil
			})
			assert.Equal(t, tt.calls, calls, tt.reason)
			if tt.fnErr != nil {
				require.ErrorIs(t, err, tt.fnErr)
				assert.NotErrorIs(t, err, context.Canceled)

				return
			}
			require.ErrorIs(t, err, context.Canceled)
			require.ErrorContains(t, err, "tracepack: merge iterate:")
		})
	}
}

// TestRunCaptureDefects reads, beside an unfinalized pack,
// a pack with a corrupt block and one whose footer misstates its block:
// the read's defects list the planning's diagnostics first, then each defect of the read in the order its block is loaded,
// with the pack and block of each.
func TestRunCaptureDefects(t *testing.T) {
	t.Parallel()

	open := planTestPack(t, seg0, captureHigh, true, timedBlocks(blockTestHour+1000, []uint64{1, 2}))
	corrupt := planTestPack(t, seg1, captureLow, false, timedBlocks(blockTestHour+100, []uint64{1, 2}, []uint64{3, 4}, []uint64{5, 6}))
	info := mustOpen(t, corrupt, ReaderOptions{}).Blocks()[1]
	corrupt = flipByte(corrupt, info.Offset+format.EnvelopeLen+3)
	misindexed := misindexedPack(t, seg2, UUID{0xC2})

	items, res := mustCaptureRead(t, Query{}, open, corrupt, misindexed)
	want := []yieldView{
		{pack: 2, seq: 10}, {pack: 2, seq: 12}, {pack: 2, seq: 14}, {pack: 2, seq: 16},
		{pack: 1, seq: 1}, {pack: 1, seq: 2}, {pack: 1, block: 2, seq: 5}, {pack: 1, block: 2, seq: 6},
		{pack: 0, seq: 1}, {pack: 0, seq: 2},
	}
	assert.Equal(t, want, yieldViews(items))
	got := make([]string, len(res.Incomplete))
	for i, d := range res.Incomplete {
		got[i] = fmt.Sprintf("%v pack %d block %d", d.Reason, d.Pack, d.Block)
	}
	assert.Equal(t, []string{
		fmt.Sprintf("%v pack 0 block %d", ReasonTruncated, res.Incomplete[0].Block),
		fmt.Sprintf("%v pack 2 block 0", ReasonIndexMismatch),
		fmt.Sprintf("%v pack 1 block 1", ReasonCorruptBlock),
	}, got)
}
