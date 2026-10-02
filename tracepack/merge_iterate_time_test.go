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

// orderedRead is what orderRead read: a copy of every Item passed to fn, the Result,
// and the number of full-block reads of each block by (reader, block), planning and resolution together.
type orderedRead struct {
	items []iterItem
	res   Result
	reads map[[2]int]int
}

// orderRead validates and plans the read of q over readers in opts.Order, then reads it,
// passing each Item to fn when fn is set.
// It returns what it read and the error, requiring nothing to stay held, whatever the outcome.
func orderRead(t testing.TB, ctx context.Context, readers []*Reader, q Query, opts MergeIterateOptions, fn func(*Item) error,
) (orderedRead, error) {
	t.Helper()

	var out orderedRead
	yield := func(it *Item) error {
		out.items = append(out.items, copyItem(it))
		if fn != nil {
			return fn(it)
		}

		return nil
	}
	p, reads, err := planReads(t, ctx, readers, q, opts, nil)
	if err == nil {
		switch opts.Order {
		case OrderCapture:
			err = p.runCapture(ctx, &q, yield)
		case OrderTime:
			err = p.runTime(ctx, &q, yield)
		default:
			require.Failf(t, "no order", "order %v", opts.Order)
		}
	}
	assert.Zero(t, p.loader.budget.held, "nothing stays held")
	out.res, out.reads = p.res, reads

	return out, err
}

// timeRead validates and plans the read of q over readers in time order with opts, its Order set,
// then reads it, passing each Item to fn when fn is set.
// It returns a copy of every Item passed to fn, the Result and the error,
// requiring nothing to stay held, whatever the outcome.
func timeRead(t testing.TB, ctx context.Context, readers []*Reader, q Query, opts MergeIterateOptions, fn func(*Item) error,
) ([]iterItem, Result, error) {
	t.Helper()

	opts.Order = OrderTime
	out, err := orderRead(t, ctx, readers, q, opts, fn)

	return out.items, out.res, err
}

// mustTimeRead reads every record of files in time order, requiring no error.
func mustTimeRead(t testing.TB, files ...[]byte) ([]iterItem, Result) {
	t.Helper()

	items, res, err := timeRead(t, t.Context(), openAll(t, files...), Query{}, MergeIterateOptions{}, nil)
	require.NoError(t, err)

	return items, res
}

// recordsAt returns data records in one block, of seqs from first, the k-th at ts[k].
func recordsAt(first uint64, ts ...int64) []footerTestStep {
	steps := make([]footerTestStep, len(ts))
	for k := range ts {
		steps[k] = footerTestStep{rec: testDataRecord(first+uint64(k), ts[k], 1), flush: k == len(ts)-1}
	}

	return steps
}

// TestRunTimeBackwardTimestamps reads records whose timestamps step backward in write order:
// within a block, across the blocks of a pack, and across packs, in one cluster and in several;
// each read yields them in ascending ts_utc_ns.
func TestRunTimeBackwardTimestamps(t *testing.T) {
	t.Parallel()

	h := blockTestHour
	tests := []struct {
		name  string
		files func(t testing.TB) [][]byte
		want  []yieldView
	}{
		{name: "within a block", files: func(t testing.TB) [][]byte {
			return [][]byte{planTestPack(t, seg0, captureLow, false, recordsAt(1, h+30, h+10, h+20))}
		}, want: []yieldView{{seq: 2}, {seq: 3}, {seq: 1}}},
		{name: "across the blocks of a pack", files: func(t testing.TB) [][]byte {
			return [][]byte{planTestPack(t, seg0, captureLow, false, slices.Concat(recordsAt(1, h+50, h+60), recordsAt(3, h+10, h+70)))}
		}, want: []yieldView{{block: 1, seq: 3}, {seq: 1}, {seq: 2}, {block: 1, seq: 4}}},
		{name: "across packs", files: func(t testing.TB) [][]byte {
			return [][]byte{
				planTestPack(t, seg0, captureLow, false, recordsAt(1, h+100, h+110)),
				planTestPack(t, seg1, captureLow, false, recordsAt(3, h+0, h+5)),
			}
		}, want: []yieldView{{pack: 1, seq: 3}, {pack: 1, seq: 4}, {seq: 1}, {seq: 2}}},
		{name: "across packs, one cluster", files: func(t testing.TB) [][]byte {
			return [][]byte{
				planTestPack(t, seg0, captureLow, false, slices.Concat(recordsAt(1, h+100), recordsAt(3, h+0))),
				planTestPack(t, seg1, captureLow, false, recordsAt(2, h+50)),
			}
		}, want: []yieldView{{block: 1, seq: 3}, {pack: 1, seq: 2}, {seq: 1}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			items, res := mustTimeRead(t, tt.files(t)...)
			assert.Equal(t, tt.want, yieldViews(items))
			assert.Empty(t, res.Conflicts)
			assert.Empty(t, res.Incomplete)
		})
	}
}

// TestRunTimeEqualTimestamps reads records of equal ts_utc_ns from clusters resolved in the other order:
// across captures they come in ascending capture_id, across the clusters of a capture in ascending seq,
// and the versions of one record in version order, whatever order the packs holding them were given in.
func TestRunTimeEqualTimestamps(t *testing.T) {
	t.Parallel()

	h := blockTestHour

	t.Run("across captures", func(t *testing.T) {
		t.Parallel()

		// captureHigh's cluster is resolved first, its ts_min 0; its seq 2 shares ts 50 with captureLow's.
		items, _ := mustTimeRead(t,
			planTestPack(t, seg0, captureHigh, false, recordsAt(1, h+0, h+50)),
			planTestPack(t, seg1, captureLow, false, recordsAt(2, h+50)),
		)
		want := []yieldView{{pack: 0, seq: 1}, {pack: 1, seq: 2}, {pack: 0, seq: 2}}
		assert.Equal(t, want, yieldViews(items))
	})

	t.Run("across clusters", func(t *testing.T) {
		t.Parallel()

		// The cluster of seqs 10 and 11 is resolved first, its ts_min 0; its seq 11 shares ts 50 with seq 1.
		items, _ := mustTimeRead(t,
			planTestPack(t, seg0, captureLow, false, slices.Concat(recordsAt(1, h+50, h+60), recordsAt(10, h+0, h+50))),
		)
		want := []yieldView{{block: 1, seq: 10}, {seq: 1}, {block: 1, seq: 11}, {seq: 2}}
		assert.Equal(t, want, yieldViews(items))
	})

	t.Run("two versions", func(t *testing.T) {
		t.Parallel()

		// Pack 1's block, of first seq 5, comes first in cluster order, so it holds version 0 of seq 7.
		items, res := mustTimeRead(t,
			planTestPack(t, seg0, captureLow, false, changed(recordsAt(7, h+100), 1, 7)),
			planTestPack(t, seg1, captureLow, false, recordsAt(5, h+0, h+50, h+100)),
		)
		want := []yieldView{
			{pack: 1, seq: 5}, {pack: 1, seq: 6},
			{pack: 1, seq: 7, conflict: true}, {pack: 0, seq: 7, conflict: true},
		}
		assert.Equal(t, want, yieldViews(items))
		assert.Equal(t, []Conflict{{CaptureID: captureLow, Seq: 7, Versions: [][]UUID{{seg1}, {seg0}}}}, res.Conflicts)
	})

	t.Run("three versions", func(t *testing.T) {
		t.Parallel()

		// Cluster order: pack 1's block (first seq 5), pack 2's (6), pack 0's (7).
		items, res := mustTimeRead(t,
			planTestPack(t, seg0, captureLow, false, changed(recordsAt(7, h+100), 1, 7)),
			planTestPack(t, seg1, captureLow, false, recordsAt(5, h+0, h+50, h+100)),
			planTestPack(t, seg2, captureLow, false, changed(recordsAt(6, h+50, h+100), 2, 7)),
		)
		want := []yieldView{
			{pack: 1, seq: 5}, {pack: 1, seq: 6},
			{pack: 1, seq: 7, conflict: true}, {pack: 2, seq: 7, conflict: true}, {pack: 0, seq: 7, conflict: true},
		}
		assert.Equal(t, want, yieldViews(items))
		assert.Equal(t, []Conflict{{CaptureID: captureLow, Seq: 7, Versions: [][]UUID{{seg1}, {seg2}, {seg0}}}}, res.Conflicts)
	})
}

// TestRunTimeClusterOrder reads clusters whose ts_min order is not their seq order:
// each is resolved in ascending ts_min, then first seq, which decides both which records are yielded before it
// and the order its conflicts are listed in.
func TestRunTimeClusterOrder(t *testing.T) {
	t.Parallel()

	h := blockTestHour

	t.Run("records", func(t *testing.T) {
		t.Parallel()

		// Clusters of seqs 1 and 2 at ts_min 0, of seq 10 at 100, and of seq 20 at 50:
		// seq 20 is yielded before seq 2 only when its cluster is resolved before seq 10's.
		items, _ := mustTimeRead(t,
			planTestPack(t, seg0, captureLow, false, slices.Concat(recordsAt(1, h+0, h+60), recordsAt(10, h+100), recordsAt(20, h+50))),
		)
		assert.Equal(t, []yieldView{{seq: 1}, {block: 2, seq: 20}, {seq: 2}, {block: 1, seq: 10}}, yieldViews(items))
	})

	t.Run("conflicts", func(t *testing.T) {
		t.Parallel()

		// Each seq is a cluster of both packs, which hold it in two versions:
		// seq 1 at ts_min 100, seq 10 at 0, seqs 20 and 30 both at 200.
		steps := slices.Concat(recordsAt(1, h+100), recordsAt(10, h+0), recordsAt(20, h+200), recordsAt(30, h+200))
		_, res := mustTimeRead(t,
			planTestPack(t, seg0, captureLow, false, steps),
			planTestPack(t, seg1, captureLow, false, changed(steps, 1, 1, 10, 20, 30)),
		)
		seqs := make([]uint64, len(res.Conflicts))
		for i, c := range res.Conflicts {
			seqs[i] = c.Seq
		}
		assert.Equal(t, []uint64{10, 1, 20, 30}, seqs)
	})
}

// TestRunTimeFilteredVersion reads three versions of one record at one timestamp, the second of which the filter rejects:
// the other two are yielded in version order, and the conflict lists all three.
func TestRunTimeFilteredVersion(t *testing.T) {
	t.Parallel()

	// Versions 0 and 1 differ in dir, which the filter rejects for version 1; version 2 in its payload.
	readers := openAll(t,
		planTestPack(t, seg0, captureLow, false, blockAt(0, 7)),
		planTestPack(t, seg1, captureLow, false, toEquipment(blockAt(0, 7))),
		planTestPack(t, seg2, captureLow, false, changed(blockAt(0, 7), 1, 7)),
	)
	q := Query{Filter: Filter{Dirs: []Dir{DirHostToEquipment}}}
	items, res, err := timeRead(t, t.Context(), readers, q, MergeIterateOptions{}, nil)
	require.NoError(t, err)
	assert.Equal(t, []yieldView{{pack: 0, seq: 7, conflict: true}, {pack: 2, seq: 7, conflict: true}}, yieldViews(items))
	assert.Equal(t, []Conflict{{CaptureID: captureLow, Seq: 7, Versions: [][]UUID{{seg0}, {seg1}, {seg2}}}}, res.Conflicts)
}

// TestRunTimeExtremeTimestamps reads records at the smallest and the largest ts_utc_ns, beside records between them:
// no timestamp is taken for the end of the clusters, so records at math.MaxInt64 are yielded, in capture_id order.
func TestRunTimeExtremeTimestamps(t *testing.T) {
	t.Parallel()

	items, res := mustTimeRead(t,
		planTestPack(t, seg0, captureHigh, false, slices.Concat(recordsAt(1, blockTestHour), recordsAt(2, math.MaxInt64))),
		planTestPack(t, seg1, captureLow, false, slices.Concat(recordsAt(1, math.MinInt64), recordsAt(2, math.MaxInt64))),
	)
	want := []yieldView{{pack: 1, seq: 1}, {pack: 0, seq: 1}, {pack: 1, block: 1, seq: 2}, {pack: 0, block: 1, seq: 2}}
	assert.Equal(t, want, yieldViews(items))
	assert.Equal(t, int64(math.MinInt64), items[0].rec.TSUTCNs)
	assert.Equal(t, int64(math.MaxInt64), items[3].rec.TSUTCNs)
	assert.Empty(t, res.Incomplete)
}

// TestRunTimeVersionsPending reads two versions of one record whose timestamps differ, beside another capture:
// version 1, the earlier, is yielded first, then the other capture's record, then version 0,
// which only a read that holds both versions pending before yielding either can do.
func TestRunTimeVersionsPending(t *testing.T) {
	t.Parallel()

	// Version 0's block has ts_min 100, version 1's 0; the other capture's block 50.
	items, res := mustTimeRead(t,
		planTestPack(t, seg0, captureLow, false, blockAt(100, 7)),
		planTestPack(t, seg1, captureLow, false, blockAt(0, 7)),
		planTestPack(t, seg2, captureHigh, false, blockAt(50, 7)),
	)
	want := []yieldView{{pack: 1, seq: 7, conflict: true}, {pack: 2, seq: 7}, {pack: 0, seq: 7, conflict: true}}
	assert.Equal(t, want, yieldViews(items))
	assert.Equal(t, []Conflict{{CaptureID: captureLow, Seq: 7, Versions: [][]UUID{{seg0}, {seg1}}}}, res.Conflicts)
}

// TestRunTimeClusterSpansHours reads a cluster of two blocks in consecutive hours whose seqs interleave:
// its records come in time order; a query of the second hour keeps the cluster, reads both blocks,
// and yields the second hour's records.
func TestRunTimeClusterSpansHours(t *testing.T) {
	t.Parallel()

	h := blockTestHour
	readers := openAll(t,
		planTestPack(t, seg0, captureLow, false, seqBlocks([]uint64{1, 3})),
		planTestPack(t, seg1, captureLow, false, timedBlocks(h+hourNs, []uint64{2, 4})),
	)

	all, err := orderRead(t, t.Context(), readers, Query{}, MergeIterateOptions{Order: OrderTime}, nil)
	require.NoError(t, err)
	assert.Equal(t, []yieldView{{seq: 1}, {seq: 3}, {pack: 1, seq: 2}, {pack: 1, seq: 4}}, yieldViews(all.items))
	assert.Equal(t, map[[2]int]int{{0, 0}: 1, {1, 0}: 1}, all.reads)

	q := Query{Filter: Filter{TimeFrom: new(h + hourNs)}}
	late, err := orderRead(t, t.Context(), readers, q, MergeIterateOptions{Order: OrderTime}, nil)
	require.NoError(t, err)
	assert.Equal(t, []yieldView{{pack: 1, seq: 2}, {pack: 1, seq: 4}}, yieldViews(late.items))
	assert.Equal(t, map[[2]int]int{{0, 0}: 1, {1, 0}: 1}, late.reads, "both blocks of the kept cluster are read")
}

// TestRunTimeMisindexed reads a pack whose footer misstates its block beside another capture:
// the block's records are yielded in time order, with its ReasonIndexMismatch defect.
func TestRunTimeMisindexed(t *testing.T) {
	t.Parallel()

	items, res := mustTimeRead(t,
		misindexedPack(t, seg0, captureHigh),
		planTestPack(t, seg1, captureLow, false, recordsAt(1, blockTestHour+11, blockTestHour+15)),
	)
	want := []yieldView{{seq: 10}, {pack: 1, seq: 1}, {seq: 12}, {seq: 14}, {pack: 1, seq: 2}, {seq: 16}}
	assert.Equal(t, want, yieldViews(items))
	require.Len(t, res.Incomplete, 1)
	assert.Equal(t, ReasonIndexMismatch, res.Incomplete[0].Reason)
}

// TestMergeIterateReadCounts reads, in each order, packs whose blocks fall in every row of the read counts:
// an indexed block is read only when its cluster is kept, once;
// a walked block is read before the read, then once more only when its cluster is kept,
// and not again when that first read fails;
// a block failing its preflight is never read.
func TestMergeIterateReadCounts(t *testing.T) {
	t.Parallel()

	h := blockTestHour
	// 0: indexed, block 0 in hour 0, excluded, block 1 in hour 1, kept.
	indexed := planTestPack(t, seg0, captureLow, false, slices.Concat(timedBlocks(h, []uint64{1, 2}), timedBlocks(h+hourNs, []uint64{10, 11})))
	// 1: walked, block 0 in hour 0, excluded; block 1 in hour 1, kept; block 2 in hour 1, corrupt.
	walked := planTestPack(t, seg1, captureHigh, true,
		slices.Concat(timedBlocks(h, []uint64{1, 2}), timedBlocks(h+hourNs, []uint64{3, 4}, []uint64{5, 6})))
	info := mustOpen(t, walked, ReaderOptions{}).Blocks()[2]
	walked = flipByte(walked, info.Offset+format.EnvelopeLen+3)
	// 2 and 3: blocks 0, 1 and 2 in hours 0, 1 and 2, block 2 stating dimensions that cannot be consistent;
	// walked, then indexed.
	walkedHuge := withIDs(t, hugeSectionPack(t, false), seg2, UUID{0xC2})
	indexedHuge := withIDs(t, hugeSectionPack(t, true), seg3, UUID{0xC3})
	// 4: walked, blocks 0, 1 and 2 in hours 0, 1 and 2, block 1 over MaxBlockLen.
	recs := hourRecords(3, 40)
	for i := 40; i < 80; i++ {
		recs[i] = randomDataRecord(recs[i].Seq, recs[i].TSUTCNs, 4<<10)
	}
	limited := withIDs(t, writeReaderPack(t, readerPackConfig{codec: CodecZstd, open: true}, recs).file, UUID{0x54}, UUID{0xC4})

	readers := openAll(t, indexed, walked, walkedHuge, indexedHuge)
	readers = append(readers, mustOpen(t, limited, ReaderOptions{MaxBlockLen: 64 << 10}))
	require.Nil(t, readers[3].footerErr)
	q := Query{Filter: Filter{TimeFrom: new(h + hourNs)}}

	for _, order := range []Order{OrderCapture, OrderTime} {
		t.Run(order.String(), func(t *testing.T) {
			t.Parallel()

			out, err := orderRead(t, t.Context(), readers, q, MergeIterateOptions{Order: order}, nil)
			require.NoError(t, err)
			items, res, reads := out.items, out.res, out.reads
			assert.Equal(t, map[[2]int]int{
				{0, 1}: 1,
				{1, 0}: 1, {1, 1}: 2, {1, 2}: 1,
				{2, 0}: 1, {2, 1}: 2,
				{3, 1}: 1,
				{4, 0}: 1, {4, 2}: 2,
			}, reads)

			got := make([]string, len(res.Incomplete))
			for i, d := range res.Incomplete {
				got[i] = fmt.Sprintf("%v pack %d block %d", d.Reason, d.Pack, d.Block)
			}
			truncated := func(pack int) string {
				return fmt.Sprintf("%v pack %d block %d", ReasonTruncated, pack, readers[pack].openDefects[0].Block)
			}
			assert.Equal(t, []string{
				truncated(1), truncated(2), truncated(4),
				fmt.Sprintf("%v pack 1 block 2", ReasonCorruptBlock),
				fmt.Sprintf("%v pack 2 block %d", ReasonCorruptBlock, hugeBlock),
				fmt.Sprintf("%v pack 4 block 1", ReasonLimit),
				fmt.Sprintf("%v pack 3 block %d", ReasonCorruptBlock, hugeBlock),
			}, got)
			// 2 records of pack 0, 2 of pack 1, 40 of each of packs 2 and 3, 40 of pack 4.
			assert.Len(t, items, 2+2+40+40+40)
		})
	}
}

// TestRunTimeHeldBytes reads two one-record clusters of equal timestamps under a budget of one block:
// the first cluster's candidate is still pending when the second cluster is loaded, so the load fails with ErrReadLimit;
// a budget of both blocks yields both.
func TestRunTimeHeldBytes(t *testing.T) {
	t.Parallel()

	low := planTestPack(t, seg0, captureLow, false, blockAt(0, 1))
	high := planTestPack(t, seg1, captureHigh, false, blockAt(0, 1))
	one := max(heldSize(t, low), heldSize(t, high))

	items, res, err := timeRead(t, t.Context(), openAll(t, low, high), Query{}, MergeIterateOptions{MaxHeldBytes: one}, nil)
	require.ErrorIs(t, err, ErrReadLimit)
	require.ErrorContains(t, err, "tracepack: pack 1, block 0:")
	assert.Empty(t, items)
	assert.Empty(t, res.Incomplete)

	items, _, err = timeRead(t, t.Context(), openAll(t, low, high), Query{},
		MergeIterateOptions{MaxHeldBytes: heldSize(t, low) + heldSize(t, high)}, nil)
	require.NoError(t, err)
	assert.Equal(t, []yieldView{{seq: 1}, {pack: 1, seq: 1}}, yieldViews(items))
}

// TestRunTimeExits ends reads in time order while candidates are pending:
// an error of fn, a ReadAt error of a block resolved after the first record is yielded, and the cancellation of ctx.
// Each returns its error with nothing held and no call of fn after it.
func TestRunTimeExits(t *testing.T) {
	t.Parallel()

	// Two clusters: seqs 1 and 2 at ts_min 10, then seqs 3 to 5 at ts_min 20; captureHigh's seq 1 at 50.
	low := planTestPack(t, seg0, captureLow, false, slices.Concat(blockAt(10, 1, 2), blockAt(20, 3, 4, 5)))
	high := planTestPack(t, seg1, captureHigh, false, blockAt(50, 1))

	t.Run("fn error", func(t *testing.T) {
		t.Parallel()

		calls := 0
		items, _, err := timeRead(t, t.Context(), openAll(t, low, high), Query{}, MergeIterateOptions{}, func(*Item) error {
			calls++
			if calls == 3 {
				return errStopIterate
			}

			return nil
		})
		require.ErrorIs(t, err, errStopIterate)
		assert.Equal(t, []yieldView{{seq: 1}, {seq: 2}, {block: 1, seq: 3}}, yieldViews(items), "no call after the error")
	})

	t.Run("ReadAt error", func(t *testing.T) {
		t.Parallel()

		g := newGatedReader(low)
		r, err := openReader(t.Context(), g, int64(len(low)), ReaderOptions{}, false)
		require.NoError(t, err)
		off := int64(r.blocks[1].Offset)
		g.fail = func(o int64, _ int) bool { return o == off }
		items, _, err := timeRead(t, t.Context(), []*Reader{r, mustOpen(t, high, ReaderOptions{})}, Query{}, MergeIterateOptions{}, nil)
		require.ErrorIs(t, err, errInjected)
		require.ErrorContains(t, err, "tracepack: pack 0, block 1:")
		assert.Equal(t, []yieldView{{seq: 1}, {seq: 2}}, yieldViews(items), "the records below the failed cluster's ts_min")
	})

	t.Run("ctx done in fn", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		items, _, err := timeRead(t, ctx, openAll(t, low, high), Query{}, MergeIterateOptions{}, func(*Item) error {
			cancel()

			return nil
		})
		require.ErrorIs(t, err, context.Canceled)
		require.ErrorContains(t, err, "tracepack: merge iterate:")
		assert.Equal(t, []yieldView{{seq: 1}, {seq: 2}}, yieldViews(items), "stopped at the load of the next cluster")
	})
}

// TestRunTimeCancel cancels ctx in the k-th call of fn of a read in time order of 5000 records in five clusters of 1000,
// whose timestamps ascend with their seqs:
// the records of each cluster are yielded before the next cluster is resolved, and those of the last in the final drain,
// calls 4001 to 5000.
// The read stops at the next check of ctx and returns ctx's error;
// fn's own error wins over the cancellation.
func TestRunTimeCancel(t *testing.T) {
	t.Parallel()

	file := mergePack(t, seg0, nil, hourSteps(1, 5000, 1000))
	require.Len(t, mustOpen(t, file, ReaderOptions{}).Blocks(), 5)
	tests := []struct {
		name   string
		k      int
		calls  int
		fnErr  error
		reason string
	}{
		{name: "the 3999th call", k: 3999, calls: 4000, reason: "the load of the last cluster's block"},
		{name: "the 4095th call", k: 4095, calls: 4095, reason: "the check before the 4096th call, in the final drain"},
		{name: "the 4001st call", k: 4001, calls: 4095, reason: "the check before the 4096th call, in the final drain"},
		{name: "the 4096th call", k: 4096, calls: 5000, reason: "the check before returning"},
		{name: "the last call", k: 5000, calls: 5000, reason: "the check before returning"},
		{name: "fn's error", k: 4500, calls: 4500, fnErr: errStopIterate},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			_, _, err := timeRead(t, ctx, openAll(t, file), Query{}, MergeIterateOptions{}, func(*Item) error {
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
