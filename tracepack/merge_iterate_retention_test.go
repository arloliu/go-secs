package tracepack

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// retentionMergePack writes the records of steps, in blocks closed where steps say, into a pack id of capture
// whose period starts at hour, finalized, or with open left unfinalized so the forward walk finds its blocks.
func retentionMergePack(t testing.TB, id, capture UUID, hour int64, open bool, steps []footerTestStep) []byte {
	t.Helper()

	meta := func(m *PackMeta) { m.PeriodStart, m.PeriodEnd = hour*hourNs, (hour+1)*hourNs }

	return withIDs(t, writeRepairPack(t, CodecZstd, meta, open, steps).file, id, capture)
}

// advanceAt returns an fn for orderRead that calls advance during its n-th call, counting from 1.
func advanceAt(n int, advance func()) func(*Item) error {
	calls := 0

	return func(*Item) error {
		calls++
		if calls == n {
			advance()
		}

		return nil
	}
}

// blockTransitions returns the number of items whose representative's block, by pack and block, differs from the item before,
// the first item included.
func blockTransitions(items []iterItem) int {
	n := 0
	for i := range items {
		if i == 0 || items[i].pack != items[i-1].pack || items[i].block != items[i-1].block {
			n++
		}
	}

	return n
}

func TestMergeIterateRetentionRemovedBeforeRead(t *testing.T) {
	t.Parallel()

	h := blockTestHour
	files := [][]byte{
		retentionMergePack(t, seg0, captureLow, retentionHour, false, recordsAt(1, h, h+1)),
		retentionMergePack(t, seg1, captureHigh, retentionHour+1, true, recordsAt(1, h+hourNs, h+hourNs+1)),
	}
	for _, order := range []Order{OrderCapture, OrderTime} {
		p := newTestRetention(retentionHour + 2)
		out, err := orderRead(t, t.Context(), openAll(t, files...), Query{Retention: p}, MergeIterateOptions{Order: order}, nil)
		require.ErrorIs(t, err, ErrRemoved, "%v", order)
		assert.Empty(t, out.items, "%v: no record is passed to fn", order)
		assert.Empty(t, out.reads, "%v: no block is read, the walked block's planning read included", order)
		assert.Equal(t, 1, p.callCount(), "%v", order)
		assert.Equal(t, []int64{retentionHour, retentionHour + 1}, out.res.Removed, "%v", order)
		assert.False(t, out.res.Complete(), "%v", order)

		res, err := MergeIterate(t.Context(), openAll(t, files...), Query{Retention: p}, MergeIterateOptions{Order: order}, acceptItems)
		require.ErrorIs(t, err, ErrRemoved, "%v", order)
		assert.Equal(t, []int64{retentionHour, retentionHour + 1}, res.Removed, "%v: MergeIterate returns the removed hours", order)
	}
}

// TestMergeIterateRetentionStreamsFilled advances the boundary in the first call of fn,
// after capture order has filled both captures' streams, both blocks read:
// the rest of the first block is yielded, the other pack's block 0, also block 0, is not.
func TestMergeIterateRetentionStreamsFilled(t *testing.T) {
	t.Parallel()

	h := blockTestHour
	files := [][]byte{
		retentionMergePack(t, seg0, captureLow, retentionHour, false, recordsAt(1, h, h+1)),
		retentionMergePack(t, seg1, captureHigh, retentionHour, false, recordsAt(1, h+10, h+11)),
	}
	for _, order := range []Order{OrderCapture, OrderTime} {
		p := newTestRetention(retentionHour)
		fn := advanceAt(1, func() { p.set(retentionHour + 1) })
		out, err := orderRead(t, t.Context(), openAll(t, files...), Query{Retention: p}, MergeIterateOptions{Order: order}, fn)
		require.ErrorIs(t, err, ErrRemoved, "%v", order)
		assert.Equal(t, []yieldView{{seq: 1}, {seq: 2}}, yieldViews(out.items), "%v", order)
		assert.Equal(t, map[[2]int]int{{0, 0}: 1, {1, 0}: 1}, out.reads, "%v: both blocks were read before the check", order)
		assert.Equal(t, 3, p.callCount(), "%v: before the read, before block 0 of each pack", order)
		assert.Equal(t, []int64{retentionHour}, out.res.Removed, "%v", order)
	}
}

// TestMergeIterateRetentionBlocksOfOnePack reads one pack of two blocks, records of seqs 1 and 2, then 3 and 4:
// the read asks the boundary again at the change of block within the pack.
// Advanced in the first call of fn, it ends the read before block 1;
// never advanced, it is asked before the read, before block 0, before block 1 and before success.
func TestMergeIterateRetentionBlocksOfOnePack(t *testing.T) {
	t.Parallel()

	files := [][]byte{retentionMergePack(t, seg0, captureLow, retentionHour, false, seqBlocks([]uint64{1, 2}, []uint64{3, 4}))}
	require.Len(t, openAll(t, files...)[0].Blocks(), 2)
	for _, order := range []Order{OrderCapture, OrderTime} {
		p := newTestRetention(retentionHour)
		fn := advanceAt(1, func() { p.set(retentionHour + 1) })
		out, err := orderRead(t, t.Context(), openAll(t, files...), Query{Retention: p}, MergeIterateOptions{Order: order}, fn)
		require.ErrorIs(t, err, ErrRemoved, "%v", order)
		assert.Equal(t, []yieldView{{seq: 1}, {seq: 2}}, yieldViews(out.items), "%v: nothing of block 1", order)
		assert.Equal(t, []int64{retentionHour}, out.res.Removed, "%v", order)

		p = newTestRetention(retentionHour)
		out, err = orderRead(t, t.Context(), openAll(t, files...), Query{Retention: p}, MergeIterateOptions{Order: order}, nil)
		require.NoError(t, err, "%v", order)
		assert.Len(t, out.items, 4, "%v", order)
		assert.Equal(t, 4, p.callCount(), "%v: before the read, before block 0, before block 1 and before success", order)
	}
}

// TestMergeIterateRetentionConflictVersions advances the boundary while fn holds the first version of a conflicting record:
// the second version, queued from the other pack's block 0, is not yielded.
func TestMergeIterateRetentionConflictVersions(t *testing.T) {
	t.Parallel()

	h := blockTestHour
	files := [][]byte{
		retentionMergePack(t, seg0, captureLow, retentionHour, false, recordsAt(1, h+1, h+2, h+3)),
		retentionMergePack(t, seg1, captureLow, retentionHour, false, changed(recordsAt(1, h+1, h+2, h+3), 1, 2)),
	}
	for _, order := range []Order{OrderCapture, OrderTime} {
		p := newTestRetention(retentionHour)
		fn := advanceAt(2, func() { p.set(retentionHour + 1) })
		out, err := orderRead(t, t.Context(), openAll(t, files...), Query{Retention: p}, MergeIterateOptions{Order: order}, fn)
		require.ErrorIs(t, err, ErrRemoved, "%v", order)
		assert.Equal(t, []yieldView{{seq: 1}, {seq: 2, conflict: true}}, yieldViews(out.items), "%v", order)
		assert.Len(t, out.res.Conflicts, 1, "%v: the conflict found so far", order)
		assert.Equal(t, []int64{retentionHour}, out.res.Removed, "%v", order)
	}
}

// TestMergeIterateRetentionTimeRelease advances the boundary in the middle of the release of pending records of several blocks,
// before a later cluster and in the final drain: the release stops at the next block.
func TestMergeIterateRetentionTimeRelease(t *testing.T) {
	t.Parallel()

	h := blockTestHour
	// Both captures' first blocks start at h+1, so their records are pending together and released in turn.
	low := retentionMergePack(t, seg0, captureLow, retentionHour, false, recordsAt(1, h+1, h+3))
	high := retentionMergePack(t, seg1, captureHigh, retentionHour, false, recordsAt(1, h+1, h+4))
	later := retentionMergePack(t, seg2, captureLow, retentionHour, false, recordsAt(10, h+100, h+101))

	tests := []struct {
		name  string
		files [][]byte
	}{
		{name: "before a later cluster", files: [][]byte{low, high, later}},
		{name: "final drain", files: [][]byte{low, high}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			readers := openAll(t, tt.files...)
			all, err := orderRead(t, t.Context(), readers, Query{}, MergeIterateOptions{Order: OrderTime}, nil)
			require.NoError(t, err)
			require.Equal(t, []yieldView{{seq: 1}, {pack: 1, seq: 1}, {seq: 2}, {pack: 1, seq: 2}}, yieldViews(all.items)[:4])

			p := newTestRetention(retentionHour)
			fn := advanceAt(2, func() { p.set(retentionHour + 1) })
			out, err := orderRead(t, t.Context(), readers, Query{Retention: p}, MergeIterateOptions{Order: OrderTime}, fn)
			require.ErrorIs(t, err, ErrRemoved)
			assert.Equal(t, []yieldView{{seq: 1}, {pack: 1, seq: 1}}, yieldViews(out.items))
			assert.NotContains(t, out.reads, [2]int{2, 0}, "the read ends before the later cluster")
			assert.Equal(t, []int64{retentionHour}, out.res.Removed)
		})
	}
}

// TestMergeIterateRetentionClusterSpansHours reads one cluster of two packs of consecutive hours, P and P+1,
// the boundary passing P in the first call of fn: P+1 stays retained.
func TestMergeIterateRetentionClusterSpansHours(t *testing.T) {
	t.Parallel()

	files := [][]byte{
		retentionMergePack(t, seg0, captureLow, retentionHour, false, seqBlocks([]uint64{1, 3})),
		retentionMergePack(t, seg1, captureLow, retentionHour+1, false, timedBlocks(blockTestHour+hourNs, []uint64{2, 4})),
	}
	want := map[Order][]yieldView{
		OrderCapture: {{seq: 1}},
		OrderTime:    {{seq: 1}, {seq: 3}},
	}
	for _, order := range []Order{OrderCapture, OrderTime} {
		p := newTestRetention(retentionHour)
		fn := advanceAt(1, func() { p.set(retentionHour + 1) })
		out, err := orderRead(t, t.Context(), openAll(t, files...), Query{Retention: p}, MergeIterateOptions{Order: order}, fn)
		require.ErrorIs(t, err, ErrRemoved, "%v", order)
		assert.Equal(t, want[order], yieldViews(out.items), "%v: no record of pack 1, of hour P+1", order)
		assert.Equal(t, []int64{retentionHour}, out.res.Removed, "%v", order)
	}
}

func TestMergeIterateRetentionRemovedAfterLastCall(t *testing.T) {
	t.Parallel()

	h := blockTestHour
	files := [][]byte{
		retentionMergePack(t, seg0, captureLow, retentionHour, false, recordsAt(1, h, h+1)),
		retentionMergePack(t, seg1, captureHigh, retentionHour, false, recordsAt(1, h+10, h+11)),
	}
	for _, order := range []Order{OrderCapture, OrderTime} {
		p := newTestRetention(retentionHour)
		fn := advanceAt(4, func() { p.set(retentionHour + 1) })
		out, err := orderRead(t, t.Context(), openAll(t, files...), Query{Retention: p}, MergeIterateOptions{Order: order}, fn)
		require.ErrorIs(t, err, ErrRemoved, "%v: never success after the hour was removed", order)
		assert.Len(t, out.items, 4, "%v", order)
		assert.Equal(t, 4, p.callCount(), "%v", order)
		assert.Equal(t, []int64{retentionHour}, out.res.Removed, "%v", order)
		assert.False(t, out.res.Complete(), "%v", order)
	}
}

// TestMergeIterateRetentionReadFailure fails the read of a block of the second pack,
// in the planning read of a walked block or in the read of an indexed one,
// the boundary passing the pack's hour, staying, or its provider failing, as the read fails.
// The ReadAt error is an I/O error or one wrapping ErrReadLimit, which is no refused reservation:
// the read checks the boundary once after either.
func TestMergeIterateRetentionReadFailure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// deleted runs as block 0 of pack 1 is read, before the read fails.
		deleted func(p *testRetention)
		removed []int64
		// errs and notErrs are the errors the read's error wraps, and does not, beside the read failure's.
		errs    []error
		notErrs []error
	}{
		{
			name: "the hour was removed", deleted: func(p *testRetention) { p.set(retentionHour + 1) },
			removed: []int64{retentionHour}, errs: []error{ErrRemoved},
		},
		{name: "the hour is retained", deleted: func(*testRetention) {}, notErrs: []error{ErrRemoved}},
		{
			name: "the provider fails", deleted: func(p *testRetention) { p.fail(errRetention) },
			errs: []error{errRetention}, notErrs: []error{ErrRemoved},
		},
	}
	causes := []struct {
		name string
		err  error
	}{
		{name: "an I/O error", err: errInjected},
		{name: "an error wrapping ErrReadLimit", err: fmt.Errorf("the store's own limit: %w", ErrReadLimit)},
	}
	h := blockTestHour
	for _, walked := range []bool{false, true} {
		for _, order := range []Order{OrderCapture, OrderTime} {
			for _, cause := range causes {
				for _, tt := range tests {
					t.Run(fmt.Sprintf("%s, %s, walked %v, %v", tt.name, cause.name, walked, order), func(t *testing.T) {
						t.Parallel()

						r0 := mustOpen(t, retentionMergePack(t, seg0, captureLow, retentionHour, false, recordsAt(1, h, h+1)), ReaderOptions{})
						r1, g := openGated(t, retentionMergePack(t, seg1, captureHigh, retentionHour, walked, recordsAt(1, h+10, h+11)))
						g.mu.Lock()
						g.failErr = cause.err
						g.mu.Unlock()
						p := newTestRetention(retentionHour)
						before := -1
						onBlockRead(r1, g, 0, func() bool {
							before = p.callCount()
							tt.deleted(p)

							return true
						})

						out, err := orderRead(t, t.Context(), []*Reader{r0, r1}, Query{Retention: p}, MergeIterateOptions{Order: order}, nil)
						require.ErrorIs(t, err, cause.err)
						for _, want := range tt.errs {
							require.ErrorIs(t, err, want)
						}
						for _, want := range tt.notErrs {
							require.NotErrorIs(t, err, want)
						}
						if len(tt.errs) > 0 {
							assert.ErrorIs(t, unwrapJoined(t, err)[0], cause.err, "the read error comes first")
						}
						assert.Equal(t, before+1, p.callCount(), "one check after the failure")
						assert.Equal(t, tt.removed, out.res.Removed)
						for _, it := range out.items {
							assert.Equal(t, 0, it.pack, "no record of the failed pack")
						}
					})
				}
			}
		}
	}
}

// TestMergeIterateRetentionReadLimit refuses a block's reservation after the boundary passed the read's hour,
// unasked: the refusal ends the read as it is, with no check after it.
func TestMergeIterateRetentionReadLimit(t *testing.T) {
	t.Parallel()

	// One cluster: pack 1's block is reserved while pack 0's is held, and refused.
	first := retentionMergePack(t, seg0, captureLow, retentionHour, false, seqBlocks([]uint64{1, 3}))
	files := [][]byte{first, retentionMergePack(t, seg1, captureLow, retentionHour, false, seqBlocks([]uint64{2, 4}))}
	// Capture order passes fn seq 1, checking the boundary, before it reserves pack 1's block; time order does not.
	scripts := map[Order][]int64{
		OrderCapture: {retentionHour, retentionHour, retentionHour + 1},
		OrderTime:    {retentionHour, retentionHour + 1},
	}
	for _, order := range []Order{OrderCapture, OrderTime} {
		p := &scriptedRetention{script: scripts[order]}
		opts := MergeIterateOptions{Order: order, MaxHeldBytes: heldSize(t, first)}
		out, err := orderRead(t, t.Context(), openAll(t, files...), Query{Retention: p}, opts, nil)
		require.ErrorIs(t, err, ErrReadLimit, "%v", order)
		require.NotErrorIs(t, err, ErrRemoved, "%v", order)
		assert.Equal(t, len(scripts[order])-1, p.calls, "%v: no check after the refusal", order)
		assert.Nil(t, out.res.Removed, "%v", order)
	}
}

// TestMergeIterateRetentionWalkedExtractHours reads a walked extract block of hour P−3 that the planning read summarizes
// and the query then excludes: its records still cover their hour, removed.
func TestMergeIterateRetentionWalkedExtractHours(t *testing.T) {
	t.Parallel()

	r, _ := openRetentionPack(t, retentionPackConfig(0, true, true), hourBlockRecords(retentionHour-3))
	require.False(t, r.Blocks()[0].Indexed)

	for _, order := range []Order{OrderCapture, OrderTime} {
		p := newTestRetention(retentionHour - 2)
		q := Query{Filter: Filter{TimeFrom: new(blockTestHour)}, Retention: p}
		out, err := orderRead(t, t.Context(), []*Reader{r}, q, MergeIterateOptions{Order: order}, nil)
		require.ErrorIs(t, err, ErrRemoved, "%v", order)
		assert.Empty(t, out.items, "%v", order)
		assert.Equal(t, map[[2]int]int{{0, 0}: 1}, out.reads, "%v: the planning read only", order)
		assert.Equal(t, 1, p.callCount(), "%v: found as the block was summarized", order)
		assert.Equal(t, []int64{retentionHour - 3}, out.res.Removed, "%v", order)
	}
}

// TestMergeIterateRetentionWrongIndexHour reads an indexed extract whose block 0 holds records of hour P−5,
// its F-2 entry saying hour P, and the filter dropping them:
// the hours of the decoded records end the read, though no record of the block reaches fn.
func TestMergeIterateRetentionWrongIndexHour(t *testing.T) {
	t.Parallel()

	w := writeReaderPack(t, retentionPackConfig(0, true, false), hourBlockRecords(retentionHour-5, retentionHour))
	file := reindexedWith(t, w.file, func(i int, s *blockSummary) {
		if i == 0 {
			s.tsMin, s.tsMax = blockTestHour, blockTestHour+1
			s.epochs[0].tsMin, s.epochs[0].tsMax = blockTestHour, blockTestHour+1
		}
	})
	r := mustOpen(t, file, ReaderOptions{})
	require.True(t, r.Blocks()[0].Indexed)

	for _, order := range []Order{OrderCapture, OrderTime} {
		p := newTestRetention(retentionHour - 2)
		q := Query{Filter: Filter{TimeFrom: new(blockTestHour)}, Retention: p}
		out, err := orderRead(t, t.Context(), []*Reader{r}, q, MergeIterateOptions{Order: order}, nil)
		require.ErrorIs(t, err, ErrRemoved, "%v", order)
		assert.Empty(t, out.items, "%v", order)
		assert.Equal(t, []int64{retentionHour - 5}, out.res.Removed, "%v", order)
		require.Len(t, out.res.Incomplete, 1, "%v: the defect found so far", order)
		assert.Equal(t, ReasonIndexMismatch, out.res.Incomplete[0].Reason, "%v", order)
	}
}

func TestMergeIterateRetentionCancelledBeforeRead(t *testing.T) {
	t.Parallel()

	readers := openAll(t, retentionMergePack(t, seg0, captureLow, retentionHour, false, recordsAt(1, blockTestHour)))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	for _, order := range []Order{OrderCapture, OrderTime} {
		p := newTestRetention(retentionHour + 1)
		res, err := MergeIterate(ctx, readers, Query{Retention: p}, MergeIterateOptions{Order: order}, acceptItems)
		require.ErrorIs(t, err, context.Canceled, "%v", order)
		require.NotErrorIs(t, err, ErrRemoved, "%v", order)
		assert.Zero(t, p.callCount(), "%v: the boundary is not asked", order)
		assert.Equal(t, Result{}, res, "%v", order)
	}
}

func TestMergeIterateRetentionCallbackError(t *testing.T) {
	t.Parallel()

	h := blockTestHour
	files := [][]byte{
		retentionMergePack(t, seg0, captureLow, retentionHour, false, recordsAt(1, h, h+1)),
		retentionMergePack(t, seg1, captureHigh, retentionHour, false, recordsAt(1, h+10, h+11)),
	}
	for _, order := range []Order{OrderCapture, OrderTime} {
		p := newTestRetention(retentionHour)
		fn := func(*Item) error {
			p.set(retentionHour + 1)
			return errStopIterate
		}
		out, err := orderRead(t, t.Context(), openAll(t, files...), Query{Retention: p}, MergeIterateOptions{Order: order}, fn)
		require.Equal(t, errStopIterate, err, "%v: the callback's error as is", order) //nolint:testifylint // identity, not errors.Is
		assert.Equal(t, 2, p.callCount(), "%v: no check after the callback's error", order)
		assert.Nil(t, out.res.Removed, "%v", order)
	}
}

// TestMergeIterateRetentionNeverRemoved checks that a read whose boundary never passes a covered hour
// yields what a read without a provider yields,
// asking the provider before the read, at each change of block and before success.
func TestMergeIterateRetentionNeverRemoved(t *testing.T) {
	t.Parallel()

	h := blockTestHour
	files := [][]byte{
		retentionMergePack(t, seg0, captureLow, retentionHour, false, recordsAt(1, h+1, h+2, h+3)),
		retentionMergePack(t, seg1, captureLow, retentionHour, true, changed(recordsAt(1, h+1, h+2, h+3), 1, 2)),
		retentionMergePack(t, seg2, captureHigh, retentionHour, false, recordsAt(1, h+2, h+60)),
	}
	readers := openAll(t, files...)
	for _, order := range []Order{OrderCapture, OrderTime} {
		opts := MergeIterateOptions{Order: order}
		want, wantRes, err := collectMergeIterate(t.Context(), readers, Query{Payloads: true}, opts)
		require.NoError(t, err)
		require.Len(t, wantRes.Conflicts, 1)

		p := newTestRetention(retentionHour)
		items, res, err := collectMergeIterate(t.Context(), readers, Query{Payloads: true, Retention: p}, opts)
		require.NoError(t, err)
		assert.Equal(t, want, items, "%v", order)
		assert.Equal(t, wantRes, res, "%v", order)
		assert.Equal(t, 2+blockTransitions(items), p.callCount(), "%v", order)
	}
}

func TestMergeIterateRetentionConcurrent(t *testing.T) {
	t.Parallel()

	h := blockTestHour
	readers := openAll(t,
		retentionMergePack(t, seg0, captureLow, retentionHour, false, recordsAt(1, h+1, h+2, h+3)),
		retentionMergePack(t, seg1, captureHigh, retentionHour, false, recordsAt(1, h+2, h+60)),
	)
	p := newTestRetention(retentionHour)
	opts := MergeIterateOptions{Order: OrderTime}
	want, _, err := collectMergeIterate(t.Context(), readers, Query{}, opts)
	require.NoError(t, err)

	var wg sync.WaitGroup
	runs := make([][]iterItem, 4)
	errs := make([]error, len(runs))
	for i := range runs {
		wg.Go(func() { runs[i], _, errs[i] = collectMergeIterate(t.Context(), readers, Query{Retention: p}, opts) })
	}
	wg.Wait()

	for i := range runs {
		require.NoError(t, errs[i])
		assert.Equal(t, want, runs[i], "read %d", i)
	}
}
