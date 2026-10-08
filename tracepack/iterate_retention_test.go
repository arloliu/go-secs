package tracepack

import (
	"context"
	"math"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// retentionHour is the scope hour of the retention test packs: the hour of blockTestHour.
const retentionHour = blockTestHour / hourNs

// recordsPerRetentionBlock is the number of records each block of a retention test pack holds.
const recordsPerRetentionBlock = 2

// retentionPackConfig returns the configuration of a retention test pack:
// blocks of recordsPerRetentionBlock testDataRecords, a segment whose period starts at periodStart,
// or an extract when extract is set.
func retentionPackConfig(periodStart int64, extract, open bool) readerPackConfig {
	return readerPackConfig{
		codec:     CodecZstd,
		threshold: recordsPerRetentionBlock * (recordHeaderLen + len(blockTestFrame)),
		open:      open,
		meta: func(m *PackMeta) {
			m.PeriodStart = periodStart
			m.PeriodEnd = periodStart + 1
			if extract {
				m.PackRole, m.ScopeGeneration, m.ExtractFilter = PackRoleExtract, nil, new("test")
			}
		},
	}
}

// hourBlockRecords returns recordsPerRetentionBlock data records for each hour of hours, in that order,
// with seqs from 0, so a retention test pack holds one block per hour.
func hourBlockRecords(hours ...int64) []Record {
	recs := make([]Record, 0, len(hours)*recordsPerRetentionBlock)
	for _, h := range hours {
		for i := range recordsPerRetentionBlock {
			recs = append(recs, testDataRecord(uint64(len(recs)), h*hourNs+int64(i), 1))
		}
	}

	return recs
}

// openRetentionPack writes recs into a retention test pack configured by c and opens it through a gatedReader,
// requiring one block per recordsPerRetentionBlock records.
func openRetentionPack(t *testing.T, c readerPackConfig, recs []Record) (*Reader, *gatedReader) {
	t.Helper()

	p := writeReaderPack(t, c, recs)
	r, g := openGated(t, p.file)
	require.Len(t, r.Blocks(), len(recs)/recordsPerRetentionBlock)

	return r, g
}

// onBlockRead makes every later read of block i of r, opened through g, call fn first,
// and fail with errInjected when fn returns true.
func onBlockRead(r *Reader, g *gatedReader, i int, fn func() bool) {
	at := int64(r.Blocks()[i].Offset)

	g.mu.Lock()
	defer g.mu.Unlock()
	g.fail = func(off int64, _ int) bool { return off == at && fn() }
}

// blockReadCount returns the number of reads through g since Open, every one a block read.
func blockReadCount(g *gatedReader, opened int) int {
	return len(g.allCalls()) - opened
}

// retentionRun runs q over r and returns the seqs passed to fn with the Result and the error;
// after fn has seen the seq at, it calls advance, which may return an error for fn to return.
func retentionRun(ctx context.Context, r *Reader, q Query, at uint64, advance func() error) ([]uint64, Result, error) {
	var seen []uint64
	res, err := r.Iterate(ctx, q, func(it *Item) error {
		seen = append(seen, it.Record.Seq)
		if it.Record.Seq == at && advance != nil {
			return advance()
		}

		return nil
	})

	return seen, res, err
}

func TestIterateRetentionRemovedBeforeRead(t *testing.T) {
	t.Parallel()

	r, g := openRetentionPack(t, retentionPackConfig(blockTestHour, false, false), hourBlockRecords(retentionHour, retentionHour))
	opened := len(g.allCalls())
	p := newTestRetention(retentionHour + 1)

	seen, res, err := retentionRun(t.Context(), r, Query{Retention: p}, 0, nil)
	require.ErrorIs(t, err, ErrRemoved)
	assert.Empty(t, seen, "no record is passed to fn")
	assert.Zero(t, blockReadCount(g, opened), "no block is read")
	assert.Equal(t, 1, p.callCount())
	assert.Equal(t, []int64{retentionHour}, res.Removed)
	assert.False(t, res.Complete())
}

func TestIterateRetentionRemovedBetweenBlocks(t *testing.T) {
	t.Parallel()

	r, g := openRetentionPack(t, retentionPackConfig(blockTestHour, false, false),
		hourBlockRecords(retentionHour, retentionHour, retentionHour))
	opened := len(g.allCalls())
	p := newTestRetention(retentionHour)

	// The boundary passes the scope hour while fn holds the first record of block 0.
	seen, res, err := retentionRun(t.Context(), r, Query{Retention: p}, 0, func() error {
		p.set(retentionHour + 1)
		return nil
	})
	require.ErrorIs(t, err, ErrRemoved)
	assert.Equal(t, []uint64{0, 1}, seen, "the rest of block 0, but no record of block 1")
	assert.Equal(t, 2, blockReadCount(g, opened), "block 1 was read, and checked before its first record")
	assert.Equal(t, []int64{retentionHour}, res.Removed)
	assert.False(t, res.Complete())
}

func TestIterateRetentionRemovedAfterLastBlock(t *testing.T) {
	t.Parallel()

	r, _ := openRetentionPack(t, retentionPackConfig(blockTestHour, false, false), hourBlockRecords(retentionHour, retentionHour))
	p := newTestRetention(retentionHour)

	seen, res, err := retentionRun(t.Context(), r, Query{Retention: p}, 3, func() error {
		p.set(retentionHour + 1)
		return nil
	})
	require.ErrorIs(t, err, ErrRemoved, "never success after the hour was removed")
	assert.Equal(t, []uint64{0, 1, 2, 3}, seen)
	assert.Equal(t, []int64{retentionHour}, res.Removed)
	assert.False(t, res.Complete())
}

func TestIterateRetentionNeverRemoved(t *testing.T) {
	t.Parallel()

	r, _ := openRetentionPack(t, retentionPackConfig(blockTestHour, false, false),
		hourBlockRecords(retentionHour, retentionHour, retentionHour))
	want := iterate(t, r, Query{Payloads: true})

	run := iterate(t, r, Query{Payloads: true, Retention: nil})
	assert.Equal(t, want, run, "no provider removes nothing")

	p := newTestRetention(retentionHour)
	run = iterate(t, r, Query{Payloads: true, Retention: p})
	assert.Equal(t, want, run, "a boundary that never passes the scope hour changes nothing")
	assert.Nil(t, run.res.Removed)
	assert.Equal(t, 5, p.callCount(), "before the read, before each of the three blocks' records, and before success")

	p = newTestRetention(retentionHour)
	run = iterate(t, r, Query{Filter: Filter{SessionIDs: []uint16{0x9999}}, Retention: p})
	assert.Empty(t, run.items)
	assert.Equal(t, 2, p.callCount(), "a block whose records are all filtered out is not checked before fn")
}

func TestIterateRetentionHourNumbering(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		periodStart int64
		from        int64
		removed     []int64
	}{
		{name: "hour 0 retained", periodStart: 0, from: 0},
		{name: "hour 0 removed", periodStart: hourNs - 1, from: 1, removed: []int64{0}},
		{name: "hour -1 removed from hour 0", periodStart: -1, from: 0, removed: []int64{-1}},
		{name: "a negative hour retained", periodStart: -3 * hourNs, from: -5},
		{name: "a negative hour removed", periodStart: -5 * hourNs, from: -4, removed: []int64{-5}},
		{name: "MinInt64 boundary", periodStart: math.MinInt64, from: math.MinInt64},
		{name: "MaxInt64 boundary", periodStart: math.MaxInt64 - hourNs, from: math.MaxInt64, removed: []int64{hourOf(math.MaxInt64 - hourNs)}},
		{name: "MaxInt64 boundary, scope hour of blockTestHour", periodStart: blockTestHour, from: math.MaxInt64, removed: []int64{retentionHour}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r, _ := openRetentionPack(t, retentionPackConfig(tt.periodStart, false, false), hourBlockRecords(retentionHour))
			seen, res, err := retentionRun(t.Context(), r, Query{Retention: newTestRetention(tt.from)}, 0, nil)
			if tt.removed == nil {
				require.NoError(t, err)
				assert.Equal(t, []uint64{0, 1}, seen)
				assert.True(t, res.Complete())
			} else {
				require.ErrorIs(t, err, ErrRemoved)
				assert.Empty(t, seen)
			}
			assert.Equal(t, tt.removed, res.Removed)
		})
	}
}

// TestIterateRetentionScopeBreach checks that a record of a pack other than an extract belongs to the pack's scope hour,
// whatever its timestamp: records of hour P−1 in a pack of hour P are not removed by the boundary P.
func TestIterateRetentionScopeBreach(t *testing.T) {
	t.Parallel()

	recs := hourBlockRecords(retentionHour - 1)
	r, _ := openRetentionPack(t, retentionPackConfig(blockTestHour, false, false), recs)

	seen, res, err := retentionRun(t.Context(), r, Query{Retention: newTestRetention(retentionHour)}, 0, nil)
	require.NoError(t, err)
	assert.Equal(t, []uint64{0, 1}, seen)
	assert.True(t, res.Complete())

	_, res, err = retentionRun(t.Context(), r, Query{Retention: newTestRetention(retentionHour + 1)}, 0, nil)
	require.ErrorIs(t, err, ErrRemoved)
	assert.Equal(t, []int64{retentionHour}, res.Removed, "the scope hour, not the records' hour")
}

func TestIterateRetentionKeepsLargestBoundary(t *testing.T) {
	t.Parallel()

	// A walked extract: block 0 holds records of hour P+5, block 1 of hour P, neither known before its read.
	r, _ := openRetentionPack(t, retentionPackConfig(0, true, true), hourBlockRecords(retentionHour+5, retentionHour))
	require.False(t, r.Blocks()[0].Indexed)

	// The provider answers P+1 before the read, then steps back.
	p := &scriptedRetention{script: []int64{retentionHour + 1, retentionHour - 10}}
	seen, res, err := retentionRun(t.Context(), r, Query{Retention: p}, 0, nil)
	require.ErrorIs(t, err, ErrRemoved, "hour P is compared with P+1, the largest boundary seen")
	assert.Equal(t, []uint64{0, 1}, seen)
	assert.Equal(t, []int64{retentionHour}, res.Removed)
	assert.Equal(t, 2, p.calls, "hour P was found removed when its block was decoded, before any further call")
}

func TestIterateRetentionProviderError(t *testing.T) {
	t.Parallel()

	r, _ := openRetentionPack(t, retentionPackConfig(blockTestHour, false, false), hourBlockRecords(retentionHour, retentionHour))

	p := newTestRetention(retentionHour)
	p.fail(errRetention)
	seen, res, err := retentionRun(t.Context(), r, Query{Retention: p}, 0, nil)
	require.ErrorIs(t, err, errRetention, "before the read")
	require.NotErrorIs(t, err, ErrRemoved)
	assert.Empty(t, seen)
	assert.Nil(t, res.Removed)

	p = newTestRetention(retentionHour)
	seen, res, err = retentionRun(t.Context(), r, Query{Retention: p}, 1, func() error {
		p.fail(errRetention)
		return nil
	})
	require.ErrorIs(t, err, errRetention, "before the next block's records")
	require.NotErrorIs(t, err, ErrRemoved)
	assert.Equal(t, []uint64{0, 1}, seen)
	assert.Nil(t, res.Removed)
}

func TestIterateRetentionReadFailure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// deleted runs as block 1 is read, before the read fails.
		deleted func(p *testRetention)
		removed []int64
		errs    []error
		notErrs []error
	}{
		{
			name: "the hour was removed", deleted: func(p *testRetention) { p.set(retentionHour + 1) },
			removed: []int64{retentionHour}, errs: []error{errInjected, ErrRemoved},
		},
		{name: "the hour is retained", deleted: func(*testRetention) {}, errs: []error{errInjected}, notErrs: []error{ErrRemoved}},
		{
			name: "the provider fails", deleted: func(p *testRetention) { p.fail(errRetention) },
			errs: []error{errInjected, errRetention}, notErrs: []error{ErrRemoved},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r, g := openRetentionPack(t, retentionPackConfig(blockTestHour, false, false), hourBlockRecords(retentionHour, retentionHour))
			p := newTestRetention(retentionHour)
			onBlockRead(r, g, 1, func() bool {
				tt.deleted(p)
				return true
			})

			seen, res, err := retentionRun(t.Context(), r, Query{Retention: p}, 0, nil)
			for _, want := range tt.errs {
				require.ErrorIs(t, err, want)
			}
			for _, want := range tt.notErrs {
				require.NotErrorIs(t, err, want)
			}
			assert.Equal(t, []uint64{0, 1}, seen)
			assert.Equal(t, tt.removed, res.Removed)
			assert.Equal(t, 3, p.callCount(), "before the read, before block 0's records, after the failure")
		})
	}
}

// TestIterateRetentionWalkedExtractReadFailure checks that a failing read of a walked extract block,
// whose hour is unknown, fails as without retention: the hour it may hold is never covered.
func TestIterateRetentionWalkedExtractReadFailure(t *testing.T) {
	t.Parallel()

	r, g := openRetentionPack(t, retentionPackConfig(0, true, true), hourBlockRecords(retentionHour, retentionHour-3))
	onBlockRead(r, g, 1, func() bool { return true })

	// Hour P−3 would be removed, were it known.
	seen, res, err := retentionRun(t.Context(), r, Query{Retention: newTestRetention(retentionHour - 1)}, 0, nil)
	require.ErrorIs(t, err, errInjected)
	require.NotErrorIs(t, err, ErrRemoved)
	assert.Equal(t, []uint64{0, 1}, seen)
	assert.Nil(t, res.Removed)
}

func TestIterateRetentionExtractRecordHours(t *testing.T) {
	t.Parallel()

	// One walked extract block holding records of hours H, H+2 and H+4.
	const h = retentionHour
	c := retentionPackConfig(0, true, true)
	c.threshold = 0
	p := writeReaderPack(t, c, hourBlockRecords(h, h)[:3])
	file := rebuildPack(t, p.file, func(_ int, b *testBlock) {
		for j := range b.headers {
			b.headers[j].TSUTCNs = (h+2*int64(j))*hourNs + int64(j)
		}
	})
	r := mustOpen(t, file, ReaderOptions{})
	require.Len(t, r.Blocks(), 1)

	tests := []struct {
		name    string
		script  []int64
		removed []int64
		calls   int
	}{
		{name: "found before the first record", script: []int64{h, h + 5}, removed: []int64{h, h + 2, h + 4}, calls: 2},
		{name: "found as the block is decoded", script: []int64{h + 5}, removed: []int64{h, h + 2, h + 4}, calls: 1},
		{name: "partly removed", script: []int64{h + 3}, removed: []int64{h, h + 2}, calls: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rp := &scriptedRetention{script: tt.script}
			seen, res, err := retentionRun(t.Context(), r, Query{Retention: rp}, 0, nil)
			require.ErrorIs(t, err, ErrRemoved)
			assert.Empty(t, seen)
			assert.Equal(t, tt.removed, res.Removed, "every removed record hour of the block, and no hour between them")
			assert.Equal(t, tt.calls, rp.calls)
			assert.False(t, res.Complete())
		})
	}
}

func TestIterateRetentionFilteredRecordHours(t *testing.T) {
	t.Parallel()

	// A walked extract: block 0 holds records of hour P−3, which the filter drops, block 1 records of hour P.
	r, _ := openRetentionPack(t, retentionPackConfig(0, true, true), hourBlockRecords(retentionHour-3, retentionHour))
	rp := &scriptedRetention{script: []int64{retentionHour - 5, retentionHour - 2}}

	q := Query{Filter: Filter{TimeFrom: new(blockTestHour)}, Retention: rp}
	seen, res, err := retentionRun(t.Context(), r, q, 0, nil)
	require.ErrorIs(t, err, ErrRemoved, "the filtered-out records still cover their hour")
	assert.Empty(t, seen)
	assert.Equal(t, []int64{retentionHour - 3}, res.Removed)
}

// TestIterateRetentionWrongIndexHour checks that the hour of a decoded record ends the read at once
// when it is already before the boundary seen,
// though its block's F-2 entry gave another hour and the filter drops the record:
// the cancellation that follows is never reached.
func TestIterateRetentionWrongIndexHour(t *testing.T) {
	t.Parallel()

	// An indexed extract: block 0 holds records of hour P−5, its F-2 entry says hour P; block 1 holds records of hour P.
	w := writeReaderPack(t, retentionPackConfig(0, true, false), hourBlockRecords(retentionHour-5, retentionHour))
	file := reindexedWith(t, w.file, func(i int, s *blockSummary) {
		if i == 0 {
			s.tsMin, s.tsMax = blockTestHour, blockTestHour+1
			s.epochs[0].tsMin, s.epochs[0].tsMax = blockTestHour, blockTestHour+1
		}
	})
	r, g := openGated(t, file)
	require.NoError(t, r.Header().FooterErr, "the shifted footer validates")
	require.True(t, r.Blocks()[0].Indexed)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	onBlockRead(r, g, 0, func() bool {
		cancel()
		return false
	})

	// Hour P is retained from P−2; hour P−5, found when block 0 is decoded, is not.
	q := Query{Filter: Filter{TimeFrom: new(blockTestHour)}, Retention: newTestRetention(retentionHour - 2)}
	seen, res, err := retentionRun(ctx, r, q, 0, nil)
	require.ErrorIs(t, err, ErrRemoved)
	require.NotErrorIs(t, err, context.Canceled)
	assert.Empty(t, seen)
	assert.Equal(t, []int64{retentionHour - 5}, res.Removed)
	require.Len(t, res.Incomplete, 1, "the defect found so far")
	assert.Equal(t, ReasonIndexMismatch, res.Incomplete[0].Reason)
}

func TestIterateRetentionPrecedence(t *testing.T) {
	t.Parallel()

	t.Run("callback error", func(t *testing.T) {
		t.Parallel()

		r, _ := openRetentionPack(t, retentionPackConfig(blockTestHour, false, false), hourBlockRecords(retentionHour, retentionHour))
		p := newTestRetention(retentionHour)
		_, res, err := retentionRun(t.Context(), r, Query{Retention: p}, 1, func() error {
			p.set(retentionHour + 1)
			return errStopIterate
		})
		require.Equal(t, errStopIterate, err, "the callback's error as is") //nolint:testifylint // identity, not errors.Is
		assert.Nil(t, res.Removed)
		assert.Equal(t, 2, p.callCount(), "no check after the callback's error")
	})

	t.Run("cancellation", func(t *testing.T) {
		t.Parallel()

		r, _ := openRetentionPack(t, retentionPackConfig(blockTestHour, false, false), hourBlockRecords(retentionHour, retentionHour))
		p := newTestRetention(retentionHour)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		_, res, err := retentionRun(ctx, r, Query{Retention: p}, 1, func() error {
			p.set(retentionHour + 1)
			cancel()

			return nil
		})
		require.ErrorIs(t, err, context.Canceled)
		require.NotErrorIs(t, err, ErrRemoved)
		assert.Nil(t, res.Removed)
		assert.Equal(t, 2, p.callCount(), "no check after the cancellation")
	})
}

func TestIterateRetentionConcurrent(t *testing.T) {
	t.Parallel()

	r, _ := openRetentionPack(t, retentionPackConfig(blockTestHour, false, false),
		hourBlockRecords(retentionHour, retentionHour, retentionHour))
	p := newTestRetention(retentionHour)
	want := iterate(t, r, Query{})

	var wg sync.WaitGroup
	runs := make([]iterRun, 4)
	errs := make([]error, len(runs))
	for i := range runs {
		wg.Go(func() { runs[i], errs[i] = runQuery(t.Context(), r, Query{Retention: p}) })
	}
	wg.Wait()

	for i := range runs {
		require.NoError(t, errs[i])
		assert.Equal(t, want, runs[i], "read %d", i)
	}
	assert.Equal(t, 5*len(runs), p.callCount())
}

// TestIterateRetentionIndexedExtractHours checks that the F-2 hours of an indexed extract's blocks are covered before the read,
// those of a block the query prunes included.
func TestIterateRetentionIndexedExtractHours(t *testing.T) {
	t.Parallel()

	r, g := openRetentionPack(t, retentionPackConfig(0, true, false), hourBlockRecords(retentionHour, retentionHour+1))
	require.True(t, r.Blocks()[0].Indexed)
	opened := len(g.allCalls())
	p := newTestRetention(retentionHour + 1)

	q := Query{Filter: Filter{TimeFrom: new(blockTestHour + hourNs)}, Retention: p}
	seen, res, err := retentionRun(t.Context(), r, q, 0, nil)
	require.ErrorIs(t, err, ErrRemoved)
	assert.Empty(t, seen)
	assert.Zero(t, blockReadCount(g, opened), "no block is read")
	assert.Equal(t, 1, p.callCount())
	assert.Equal(t, []int64{retentionHour}, res.Removed)
	assert.False(t, res.Complete())
}

func TestIterateRetentionCancelledBeforeRead(t *testing.T) {
	t.Parallel()

	r, _ := openRetentionPack(t, retentionPackConfig(blockTestHour, false, false), hourBlockRecords(retentionHour))
	p := newTestRetention(retentionHour + 1)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, res, err := retentionRun(ctx, r, Query{Retention: p}, 0, nil)
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, ErrRemoved)
	assert.Zero(t, p.callCount(), "the boundary is not asked")
	assert.Nil(t, res.Removed)
}

// TestIterateRetentionCancelledEmptyPack reads a pack without blocks under a cancelled ctx:
// without a provider the read ends as before retention, with its Result and no error, since ctx is checked only before a block;
// with one, ctx ends the read before the boundary is asked.
func TestIterateRetentionCancelledEmptyPack(t *testing.T) {
	t.Parallel()

	r, _ := openRetentionPack(t, retentionPackConfig(blockTestHour, false, false), nil)
	require.Empty(t, r.Blocks())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	t.Run("no provider", func(t *testing.T) {
		t.Parallel()

		_, want, err := retentionRun(t.Context(), r, Query{}, 0, nil)
		require.NoError(t, err)
		_, res, err := retentionRun(ctx, r, Query{}, 0, nil)
		require.NoError(t, err)
		assert.Equal(t, want, res)
	})

	t.Run("a provider", func(t *testing.T) {
		t.Parallel()

		p := newTestRetention(retentionHour)
		_, res, err := retentionRun(ctx, r, Query{Retention: p}, 0, nil)
		require.ErrorIs(t, err, context.Canceled)
		assert.Zero(t, p.callCount(), "the boundary is not asked")
		assert.Nil(t, res.Removed)
	})
}

// TestIterateRetentionPacksWithoutRecords checks that a read covers its pack's scope hour
// though it passes fn no record: an empty pack, a pack whose records are all filtered out beside a defect,
// and a pack with a coverage defect.
func TestIterateRetentionPacksWithoutRecords(t *testing.T) {
	t.Parallel()

	t.Run("empty", func(t *testing.T) {
		t.Parallel()

		r, _ := openRetentionPack(t, retentionPackConfig(blockTestHour, false, false), nil)
		seen, res, err := retentionRun(t.Context(), r, Query{Retention: newTestRetention(retentionHour + 1)}, 0, nil)
		require.ErrorIs(t, err, ErrRemoved)
		assert.Empty(t, seen)
		assert.Equal(t, []int64{retentionHour}, res.Removed)
		assert.False(t, res.Complete())
	})

	t.Run("records filtered out, a truncation defect", func(t *testing.T) {
		t.Parallel()

		r, g := openRetentionPack(t, retentionPackConfig(blockTestHour, false, true), hourBlockRecords(retentionHour))
		p := newTestRetention(retentionHour)
		onBlockRead(r, g, 0, func() bool {
			p.set(retentionHour + 1)
			return false
		})

		q := Query{Filter: Filter{SessionIDs: []uint16{0x9999}}, Retention: p}
		seen, res, err := retentionRun(t.Context(), r, q, 0, nil)
		require.ErrorIs(t, err, ErrRemoved, "found before success")
		assert.Empty(t, seen)
		assert.Equal(t, []int64{retentionHour}, res.Removed)
		require.Len(t, res.Incomplete, 1, "the defect found so far")
		assert.Equal(t, ReasonTruncated, res.Incomplete[0].Reason)
		assert.False(t, res.Complete())
	})

	t.Run("a coverage defect", func(t *testing.T) {
		t.Parallel()

		file, _ := coveragePack(t, false)
		r := mustOpen(t, file, ReaderOptions{})
		hour := hourOf(r.Header().Meta.PeriodStart)

		seen, res, err := retentionRun(t.Context(), r, Query{Retention: newTestRetention(hour + 1)}, 0, nil)
		require.ErrorIs(t, err, ErrRemoved)
		assert.Empty(t, seen)
		assert.Equal(t, []int64{hour}, res.Removed)
		require.NotEmpty(t, res.Incomplete)
		assert.Equal(t, -1, res.Incomplete[0].Block, "the coverage defect is kept")
		assert.False(t, res.Complete())
	})
}
