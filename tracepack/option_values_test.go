package tracepack

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// optionCase sets one budget field of an options struct to -1, named as the error names it.
type optionCase[T any] struct {
	field string
	set   func(o *T)
}

// sharedReadCounter is an io.ReaderAt over data that counts its calls, safe for the concurrent calls of a read round.
type sharedReadCounter struct {
	data  []byte
	calls atomic.Int64
}

var _ io.ReaderAt = (*sharedReadCounter)(nil)

func (r *sharedReadCounter) ReadAt(p []byte, off int64) (int, error) {
	r.calls.Add(1)

	return copyAt(r.data, p, off)
}

// readerBudgetCases lists every budget field of ReaderOptions.
var readerBudgetCases = []optionCase[ReaderOptions]{
	{"WholeReadThreshold", func(o *ReaderOptions) { o.WholeReadThreshold = -1 }},
	{"HeadWindow", func(o *ReaderOptions) { o.HeadWindow = -1 }},
	{"TailWindow", func(o *ReaderOptions) { o.TailWindow = -1 }},
	{"MaxPackMetadataLen", func(o *ReaderOptions) { o.MaxPackMetadataLen = -1 }},
	{"MaxFooterLen", func(o *ReaderOptions) { o.MaxFooterLen = -1 }},
	{"MaxBlockLen", func(o *ReaderOptions) { o.MaxBlockLen = -1 }},
	{"MaxWalkedBlocks", func(o *ReaderOptions) { o.MaxWalkedBlocks = -1 }},
}

// txBudgetCases lists every budget field of TxOptions.
var txBudgetCases = []optionCase[TxOptions]{
	{"MaxHours", func(o *TxOptions) { o.MaxHours = -1 }},
	{"MaxHeldBytes", func(o *TxOptions) { o.MaxHeldBytes = -1 }},
	{"MaxConflicts", func(o *TxOptions) { o.MaxConflicts = -1 }},
	{"MaxStateBytes", func(o *TxOptions) { o.MaxStateBytes = -1 }},
}

// optionTestPack returns a finalized pack of two one-block hours.
func optionTestPack(t *testing.T) []byte {
	t.Helper()

	return writeReaderPack(t, readerPackConfig{codec: CodecZstd}, hourRecords(2, 3)).file
}

// Open refuses a negative ReaderOptions field with a plain error naming it, before any ReadAt;
// a zero field takes its default, and a short object is refused first.
func TestOpenNegativeOption(t *testing.T) {
	t.Parallel()

	file := optionTestPack(t)
	for _, tt := range readerBudgetCases {
		t.Run(tt.field, func(t *testing.T) {
			t.Parallel()

			var opts ReaderOptions
			tt.set(&opts)
			ra := &sharedReadCounter{data: file}
			r, err := Open(t.Context(), ra, int64(len(file)), opts)
			require.EqualError(t, err, "tracepack: ReaderOptions."+tt.field+" -1 is negative")
			assert.NoError(t, errors.Unwrap(err), "a plain error")
			assert.Nil(t, r)
			assert.Zero(t, ra.calls.Load(), "no ReadAt")
		})
	}

	t.Run("zero takes the default", func(t *testing.T) {
		t.Parallel()

		ra := &sharedReadCounter{data: file}
		r, err := Open(t.Context(), ra, int64(len(file)), ReaderOptions{})
		require.NoError(t, err)
		assert.Equal(t, ReaderOptions{}.withDefaults(), r.opts)
		assert.Positive(t, ra.calls.Load())
	})

	t.Run("a short object wins", func(t *testing.T) {
		t.Parallel()

		ra := &sharedReadCounter{data: file}
		_, err := Open(t.Context(), ra, 10, ReaderOptions{MaxBlockLen: -1})
		require.ErrorIs(t, err, ErrNotTracepack)
		assert.Zero(t, ra.calls.Load(), "no ReadAt")
	})
}

// Verify refuses a negative field of VerifyOptions.Reader as Open does.
func TestVerifyNegativeOption(t *testing.T) {
	t.Parallel()

	file := optionTestPack(t)
	for _, tt := range readerBudgetCases {
		t.Run(tt.field, func(t *testing.T) {
			t.Parallel()

			var opts VerifyOptions
			tt.set(&opts.Reader)
			ra := &sharedReadCounter{data: file}
			rep, err := Verify(t.Context(), ra, int64(len(file)), opts)
			require.EqualError(t, err, "tracepack: ReaderOptions."+tt.field+" -1 is negative")
			assert.NoError(t, errors.Unwrap(err), "a plain error")
			assert.Zero(t, rep.Outcome)
			assert.Zero(t, ra.calls.Load(), "no ReadAt")
		})
	}

	t.Run("zero takes the default", func(t *testing.T) {
		t.Parallel()

		ra := &sharedReadCounter{data: file}
		rep, err := Verify(t.Context(), ra, int64(len(file)), VerifyOptions{})
		require.NoError(t, err)
		assert.Equal(t, VerifyOutcomeFinalizedConsistent, rep.Outcome)
		assert.Positive(t, ra.calls.Load())
	})

	t.Run("a short object wins", func(t *testing.T) {
		t.Parallel()

		ra := &sharedReadCounter{data: file}
		_, err := Verify(t.Context(), ra, 10, VerifyOptions{Reader: ReaderOptions{MaxBlockLen: -1}})
		require.ErrorIs(t, err, ErrNotTracepack)
		assert.Zero(t, ra.calls.Load(), "no ReadAt")
	})
}

// Merge refuses a negative field of MergeOptions.Reader with a plain error naming it, before any ReadAt or write;
// a zero field takes its default, and an input without a ReaderAt is refused first.
func TestMergeNegativeOption(t *testing.T) {
	t.Parallel()

	file := mergePack(t, seg0, nil, hourSteps(10, 13, 2))
	for _, tt := range readerBudgetCases {
		t.Run(tt.field, func(t *testing.T) {
			t.Parallel()

			opts := mergeOpts()
			tt.set(&opts.Reader)
			ra := &sharedReadCounter{data: file}
			var dst touchWriter
			rep, err := Merge(t.Context(), &dst, []MergeInput{{ReaderAt: ra, Size: int64(len(file))}}, opts)
			require.EqualError(t, err, "tracepack: MergeOptions.Reader."+tt.field+" -1 is negative")
			assert.NoError(t, errors.Unwrap(err), "a plain error")
			assert.Equal(t, MergeReport{}, rep)
			assert.Zero(t, ra.calls.Load(), "no ReadAt")
			assert.Zero(t, dst.calls, "dst untouched")
		})
	}

	t.Run("zero takes the default", func(t *testing.T) {
		t.Parallel()

		ra := &sharedReadCounter{data: file}
		var dst bytes.Buffer
		rep, err := Merge(t.Context(), &dst, []MergeInput{{ReaderAt: ra, Size: int64(len(file))}}, mergeOpts())
		require.NoError(t, err)
		assert.Equal(t, uint64(4), rep.Records)
		assert.Positive(t, ra.calls.Load())
	})

	t.Run("an input without a ReaderAt wins", func(t *testing.T) {
		t.Parallel()

		opts := mergeOpts()
		opts.Reader.MaxBlockLen = -1
		var dst touchWriter
		_, err := Merge(t.Context(), &dst, []MergeInput{{Size: int64(len(file))}}, opts)
		require.EqualError(t, err, "tracepack: merge: input 0 has no ReaderAt")
		assert.Zero(t, dst.calls, "dst untouched")
	})
}

// MergeIterate refuses a negative MergeIterateOptions field with an error wrapping ErrInvalidQuery that names it,
// before any ReadAt and any call of fn, also with ctx done;
// a zero field takes its default, and a nil fn is refused first.
func TestMergeIterateNegativeOption(t *testing.T) {
	t.Parallel()

	file := optionTestPack(t)
	// Windows that hold no block, so every block read is a ReadAt the counter sees.
	open := func(t *testing.T) (*Reader, *sharedReadCounter) {
		t.Helper()

		ra := &sharedReadCounter{data: file}
		r, err := Open(t.Context(), ra, int64(len(file)), uncachedWindows())
		require.NoError(t, err)

		return r, ra
	}
	cases := []optionCase[MergeIterateOptions]{
		{"MaxHeldBytes", func(o *MergeIterateOptions) { o.MaxHeldBytes = -1 }},
		{"MaxConflicts", func(o *MergeIterateOptions) { o.MaxConflicts = -1 }},
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	for _, tt := range cases {
		for _, c := range []struct {
			name string
			ctx  context.Context
		}{{"ctx live", t.Context()}, {"ctx done", cancelled}} {
			t.Run(tt.field+"/"+c.name, func(t *testing.T) {
				t.Parallel()

				r, ra := open(t)
				before := ra.calls.Load()
				opts := MergeIterateOptions{Order: OrderCapture}
				tt.set(&opts)
				calls := 0
				res, err := MergeIterate(c.ctx, []*Reader{r}, Query{}, opts, func(*Item) error { calls++; return nil })
				require.ErrorIs(t, err, ErrInvalidQuery)
				require.ErrorContains(t, err, "MergeIterateOptions."+tt.field+" -1 is negative")
				assert.Equal(t, Result{}, res)
				assert.Zero(t, calls, "fn not called")
				assert.Equal(t, before, ra.calls.Load(), "no ReadAt")
			})
		}
	}

	t.Run("zero takes the default", func(t *testing.T) {
		t.Parallel()

		r, ra := open(t)
		before := ra.calls.Load()
		calls := 0
		_, err := MergeIterate(t.Context(), []*Reader{r}, Query{}, MergeIterateOptions{Order: OrderCapture},
			func(*Item) error { calls++; return nil })
		require.NoError(t, err)
		assert.Equal(t, 6, calls)
		assert.Greater(t, ra.calls.Load(), before)
	})

	t.Run("a nil fn wins", func(t *testing.T) {
		t.Parallel()

		r, ra := open(t)
		before := ra.calls.Load()
		_, err := MergeIterate(t.Context(), []*Reader{r}, Query{}, MergeIterateOptions{Order: OrderCapture, MaxHeldBytes: -1}, nil)
		require.ErrorIs(t, err, ErrInvalidQuery)
		require.ErrorContains(t, err, "fn is nil")
		assert.Equal(t, before, ra.calls.Load(), "no ReadAt")
	})
}

// FindTransaction refuses a negative TxOptions field with an error wrapping ErrInvalidQuery that names it,
// before it calls the source; a zero field takes its default, and a zero capture_id is refused first.
func TestFindTransactionNegativeOption(t *testing.T) {
	t.Parallel()

	key := TxKey{CaptureID: captureLow, Seq: 1, Hour: memTestHour}
	for _, tt := range txBudgetCases {
		t.Run(tt.field, func(t *testing.T) {
			t.Parallel()

			s := newMemSource()
			var opts TxOptions
			tt.set(&opts)
			res, err := FindTransaction(t.Context(), s, key, opts)
			require.ErrorIs(t, err, ErrInvalidQuery)
			require.ErrorContains(t, err, "TxOptions."+tt.field+" -1 is negative")
			assert.Equal(t, TxResult{}, res)
			assert.Empty(t, s.counts(), "no source call")
		})
	}

	t.Run("zero takes the default", func(t *testing.T) {
		t.Parallel()

		s := newMemSource()
		res, err := FindTransaction(t.Context(), s, key, TxOptions{})
		require.ErrorIs(t, err, ErrNotPrimary)
		assert.Equal(t, []TxHour{{Hour: key.Hour}}, res.Searched)
		assert.Equal(t, map[int64]int{key.Hour: 1}, s.counts())
	})

	t.Run("a zero capture_id wins", func(t *testing.T) {
		t.Parallel()

		s := newMemSource()
		_, err := FindTransaction(t.Context(), s, TxKey{Seq: 1, Hour: memTestHour}, TxOptions{MaxHours: -1})
		require.ErrorIs(t, err, ErrInvalidQuery)
		require.ErrorContains(t, err, "capture_id is zero")
		assert.Empty(t, s.counts(), "no source call")
	})
}

// FindTransactions refuses a negative field of TxRangeOptions or of its Lookup,
// with an error wrapping ErrInvalidQuery that names it, before it calls the source; a zero field takes its default, and a nil fn is refused first.
func TestFindTransactionsNegativeOption(t *testing.T) {
	t.Parallel()

	at := time.Unix(0, memTestHour*hourNs)
	r := TxRange{CaptureID: captureLow, From: at, To: at.Add(time.Hour)}
	fn := func(*TxPairing) error { return nil }
	cases := make([]optionCase[TxRangeOptions], 0, 1+len(txBudgetCases))
	cases = append(cases, optionCase[TxRangeOptions]{"MaxCachedBytes", func(o *TxRangeOptions) { o.MaxCachedBytes = -1 }})
	for _, c := range txBudgetCases {
		cases = append(cases, optionCase[TxRangeOptions]{"Lookup." + c.field, func(o *TxRangeOptions) { c.set(&o.Lookup) }})
	}
	for _, tt := range cases {
		t.Run(tt.field, func(t *testing.T) {
			t.Parallel()

			s := newMemSource()
			var opts TxRangeOptions
			tt.set(&opts)
			res, err := FindTransactions(t.Context(), s, r, opts, fn)
			require.ErrorIs(t, err, ErrInvalidQuery)
			require.ErrorContains(t, err, "TxRangeOptions."+tt.field+" -1 is negative")
			assert.Equal(t, TxRangeResult{}, res)
			assert.Empty(t, s.counts(), "no source call")
		})
	}

	t.Run("zero takes the default", func(t *testing.T) {
		t.Parallel()

		s := newMemSource()
		_, err := FindTransactions(t.Context(), s, r, TxRangeOptions{}, fn)
		require.NoError(t, err)
		assert.NotEmpty(t, s.counts())
	})

	t.Run("a nil fn wins", func(t *testing.T) {
		t.Parallel()

		s := newMemSource()
		_, err := FindTransactions(t.Context(), s, r, TxRangeOptions{MaxCachedBytes: -1}, nil)
		require.ErrorIs(t, err, ErrInvalidQuery)
		require.ErrorContains(t, err, "fn is nil")
		assert.Empty(t, s.counts(), "no source call")
	})
}
