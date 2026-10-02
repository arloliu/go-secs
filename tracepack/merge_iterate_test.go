package tracepack

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// collectMergeIterate runs MergeIterate and returns a copy of every Item it passed to fn, the Result and the error.
func collectMergeIterate(ctx context.Context, readers []*Reader, q Query, opts MergeIterateOptions) ([]iterItem, Result, error) {
	var items []iterItem
	res, err := MergeIterate(ctx, readers, q, opts, func(it *Item) error {
		items = append(items, copyItem(it))
		return nil
	})

	return items, res, err
}

// sortedViews returns what each item names, sorted by pack, block and seq.
func sortedViews(items []iterItem) []yieldView {
	out := yieldViews(items)
	slices.SortFunc(out, func(a, b yieldView) int {
		return cmp.Or(cmp.Compare(a.pack, b.pack), cmp.Compare(a.block, b.block), cmp.Compare(a.seq, b.seq))
	})

	return out
}

func TestMergeIterateInvalid(t *testing.T) {
	t.Parallel()

	file := planTestPack(t, seg0, captureLow, false, blockAt(0, 1))
	r := mustOpen(t, file, ReaderOptions{})
	same := mustOpen(t, file, ReaderOptions{})
	inTime := MergeIterateOptions{Order: OrderTime}
	called := func(*Item) error {
		t.Error("fn called")

		return errStopIterate
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()

	tests := []struct {
		name    string
		ctx     context.Context
		readers []*Reader
		q       Query
		opts    MergeIterateOptions
		nilFn   bool
		want    error
	}{
		{name: "nil fn", readers: []*Reader{r}, opts: inTime, nilFn: true, want: ErrInvalidQuery},
		{name: "no order", readers: []*Reader{r}, want: ErrInvalidQuery},
		{name: "unknown order", readers: []*Reader{r}, opts: MergeIterateOptions{Order: OrderTime + 1}, want: ErrInvalidQuery},
		{name: "inverted time range", readers: []*Reader{r}, opts: inTime,
			q: Query{Filter: Filter{TimeFrom: new(int64(2)), TimeTo: new(int64(1))}}, want: ErrInvalidQuery},
		{name: "nil reader", readers: []*Reader{r, nil}, opts: inTime, want: ErrInvalidQuery},
		{name: "a Reader twice", readers: []*Reader{r, r}, opts: inTime, want: ErrInvalidQuery},
		{name: "a pack_id twice", readers: []*Reader{r, same}, opts: inTime, want: ErrInvalidQuery},
		{name: "ctx done", ctx: cancelled, readers: []*Reader{r}, opts: inTime, want: context.Canceled},
		{name: "ctx done, no readers", ctx: cancelled, opts: inTime, want: context.Canceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := tt.ctx
			if ctx == nil {
				ctx = t.Context()
			}
			fn := called
			if tt.nilFn {
				fn = nil
			}
			res, err := MergeIterate(ctx, tt.readers, tt.q, tt.opts, fn)
			require.ErrorIs(t, err, tt.want)
			assert.Equal(t, Result{}, res)
		})
	}
}

func TestMergeIterateNoReaders(t *testing.T) {
	t.Parallel()

	for _, order := range []Order{OrderCapture, OrderTime} {
		for _, readers := range [][]*Reader{nil, {}} {
			res, err := MergeIterate(t.Context(), readers, Query{}, MergeIterateOptions{Order: order}, func(*Item) error {
				t.Error("fn called")

				return nil
			})
			require.NoError(t, err)
			assert.Equal(t, Result{}, res, "%v", order)
		}
	}
}

// TestMergeIterateResult reads in each order an unfinalized pack and a finalized one of a capture,
// one record of which they hold in two versions, another capture's unfinalized pack whose timestamps step backward, and a pack whose footer misstates its block:
// each order yields its exact sequence, both the same records,
// and the Result lists the defects in their order with their packs, the footer errors in reader order, and the conflict.
func TestMergeIterateResult(t *testing.T) {
	t.Parallel()

	h := blockTestHour
	readers := openAll(t,
		planTestPack(t, seg0, captureLow, true, recordsAt(1, h+1, h+2, h+3)),
		planTestPack(t, seg1, captureLow, false, changed(recordsAt(1, h+1, h+2, h+3), 1, 2)),
		planTestPack(t, seg2, captureHigh, true, recordsAt(1, h+60, h+0)),
		misindexedPack(t, seg3, UUID{0xC2}),
	)
	low := []yieldView{{pack: 0, seq: 1}, {pack: 0, seq: 2, conflict: true}, {pack: 1, seq: 2, conflict: true}, {pack: 0, seq: 3}}
	misindexed := []yieldView{{pack: 3, seq: 10}, {pack: 3, seq: 12}, {pack: 3, seq: 14}, {pack: 3, seq: 16}}
	want := map[Order][]yieldView{
		OrderCapture: slices.Concat([]yieldView{{pack: 2, seq: 1}, {pack: 2, seq: 2}}, low, misindexed),
		OrderTime:    slices.Concat([]yieldView{{pack: 2, seq: 2}}, low, misindexed, []yieldView{{pack: 2, seq: 1}}),
	}

	got := map[Order][]iterItem{}
	for _, order := range []Order{OrderCapture, OrderTime} {
		items, res, err := collectMergeIterate(t.Context(), readers, Query{}, MergeIterateOptions{Order: order})
		require.NoError(t, err)
		got[order] = items
		assert.Equal(t, want[order], yieldViews(items), "%v", order)

		defects := make([]string, len(res.Incomplete))
		for i, d := range res.Incomplete {
			defects[i] = fmt.Sprintf("%v pack %d block %d", d.Reason, d.Pack, d.Block)
		}
		assert.Equal(t, []string{
			fmt.Sprintf("%v pack 0 block %d", ReasonTruncated, readers[0].openDefects[0].Block),
			fmt.Sprintf("%v pack 2 block %d", ReasonTruncated, readers[2].openDefects[0].Block),
			fmt.Sprintf("%v pack 3 block 0", ReasonIndexMismatch),
		}, defects, "%v", order)
		assert.Equal(t, []PackError{{Pack: 0, Err: readers[0].footerErr}, {Pack: 2, Err: readers[2].footerErr}}, res.FooterErrs)
		require.Error(t, res.FooterErrs[0].Err)
		require.Error(t, res.FooterErrs[1].Err)
		assert.NoError(t, res.FooterErr)
		assert.Equal(t, []Conflict{{CaptureID: captureLow, Seq: 2, Versions: [][]UUID{{seg0}, {seg1}}}}, res.Conflicts)
		assert.False(t, res.Complete())
	}
	assert.Equal(t, sortedViews(got[OrderCapture]), sortedViews(got[OrderTime]), "both orders yield the same records")
}

// TestMergeIterateWalkedBesideArchive reads in each order an unfinalized pack beside the archive of its records:
// each record is yielded once, without conflict, from the first block holding it in cluster order,
// and the read is incomplete only for the unfinalized pack's truncation.
func TestMergeIterateWalkedBesideArchive(t *testing.T) {
	t.Parallel()

	h := blockTestHour
	archive := planTestPack(t, seg0, captureLow, false, timedBlocks(h, []uint64{1, 2, 3}, []uint64{4, 5, 6}))
	walked := planTestPack(t, seg1, captureLow, true, timedBlocks(h, []uint64{1, 2, 3}, []uint64{4, 5}))
	tests := []struct {
		name  string
		files [][]byte
		// archive is the archive's index among the readers.
		archive int
		want    []yieldView
	}{
		{name: "archive first", files: [][]byte{archive, walked}, archive: 0, want: []yieldView{
			{seq: 1}, {seq: 2}, {seq: 3}, {block: 1, seq: 4}, {block: 1, seq: 5}, {block: 1, seq: 6},
		}},
		{name: "walked pack first", files: [][]byte{walked, archive}, archive: 1, want: []yieldView{
			{seq: 1}, {seq: 2}, {seq: 3}, {block: 1, seq: 4}, {block: 1, seq: 5}, {pack: 1, block: 1, seq: 6},
		}},
	}
	for _, tt := range tests {
		for _, order := range []Order{OrderCapture, OrderTime} {
			t.Run(tt.name+", "+order.String(), func(t *testing.T) {
				t.Parallel()

				items, res, err := collectMergeIterate(t.Context(), openAll(t, tt.files...), Query{Payloads: true}, MergeIterateOptions{Order: order})
				require.NoError(t, err)
				assert.Equal(t, tt.want, yieldViews(items))
				for _, it := range items {
					assert.Equal(t, blockTestFrame, it.rec.Payload, "seq %d", it.rec.Seq)
				}
				assert.Empty(t, res.Conflicts)
				require.Len(t, res.Incomplete, 1)
				assert.Equal(t, ReasonTruncated, res.Incomplete[0].Reason)
				assert.Equal(t, 1-tt.archive, res.Incomplete[0].Pack)
				require.Len(t, res.FooterErrs, 1)
				assert.Equal(t, 1-tt.archive, res.FooterErrs[0].Pack)
			})
		}
	}
}

// TestMergeIterateYieldCheck cancels ctx in the 4095th call of fn of a read of 5000 records in five blocks, in each order:
// the 4096th call is not made, after the last block's load, and MergeIterate returns ctx's error.
func TestMergeIterateYieldCheck(t *testing.T) {
	t.Parallel()

	file := mergePack(t, seg0, nil, hourSteps(1, 5000, 1000))
	for _, order := range []Order{OrderCapture, OrderTime} {
		t.Run(order.String(), func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			res, err := MergeIterate(ctx, openAll(t, file), Query{}, MergeIterateOptions{Order: order}, func(*Item) error {
				calls++
				if calls == 4095 {
					cancel()
				}

				return nil
			})
			require.ErrorIs(t, err, context.Canceled)
			require.ErrorContains(t, err, "tracepack: merge iterate:")
			assert.Equal(t, 4095, calls)
			assert.True(t, res.Complete())
		})
	}
}
