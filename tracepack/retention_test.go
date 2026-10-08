package tracepack

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// errRetention is the failure a test Retention returns.
var errRetention = errors.New("retention boundary unavailable")

// testRetention is a Retention whose boundary, or failure, a test sets from a callback or a read hook,
// and which counts the calls it answers.
type testRetention struct {
	mu    sync.Mutex
	from  int64
	err   error
	calls int
}

var _ Retention = (*testRetention)(nil)

// newTestRetention returns a testRetention whose boundary is from.
func newTestRetention(from int64) *testRetention {
	return &testRetention{from: from}
}

// RetainedFrom implements Retention.
func (p *testRetention) RetainedFrom(context.Context) (int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.calls++
	if p.err != nil {
		return 0, p.err
	}

	return p.from, nil
}

// set makes from the boundary of every later call.
func (p *testRetention) set(from int64) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.from = from
}

// fail makes every later call fail with err.
func (p *testRetention) fail(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.err = err
}

// callCount returns the number of calls answered so far.
func (p *testRetention) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.calls
}

// scriptedRetention is a Retention that returns the boundaries of its script in turn, the last one repeating.
type scriptedRetention struct {
	mu     sync.Mutex
	script []int64
	calls  int
}

var _ Retention = (*scriptedRetention)(nil)

// RetainedFrom implements Retention.
func (p *scriptedRetention) RetainedFrom(context.Context) (int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	from := p.script[min(p.calls, len(p.script)-1)]
	p.calls++

	return from, nil
}

func TestResultCompleteWithRemoved(t *testing.T) {
	t.Parallel()

	assert.True(t, Result{}.Complete())
	assert.False(t, Result{Removed: []int64{0}}.Complete(), "a read that ended on a removed hour is not complete")
	assert.False(t, Result{Incomplete: []Defect{{Reason: ReasonTruncated}}}.Complete())
}

func TestRetentionStateNilProvider(t *testing.T) {
	t.Parallel()

	rs := newRetentionState(nil)
	require.NoError(t, rs.add(math.MinInt64))
	require.NoError(t, rs.check(t.Context()))
	assert.Nil(t, rs.covered, "no hour is kept without a provider")
	assert.Nil(t, rs.removed())

	cause := errors.New("read failed")
	assert.Equal(t, cause, rs.recheckAfter(t.Context(), cause), "the failure stands as is") //nolint:testifylint // identity, not errors.Is
}

func TestRetentionStateBoundary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		covered []int64
		from    int64
		removed []int64
	}{
		{name: "hour 0 retained from 0", covered: []int64{0}, from: 0},
		{name: "hour 0 removed from 1", covered: []int64{0}, from: 1, removed: []int64{0}},
		{name: "hour -1 removed from 0", covered: []int64{-1}, from: 0, removed: []int64{-1}},
		{name: "negative hours retained", covered: []int64{-3, -2}, from: -5},
		{name: "negative hours partly removed", covered: []int64{-2, -5, -4}, from: -3, removed: []int64{-5, -4}},
		{name: "MinInt64 removes nothing", covered: []int64{math.MinInt64, 0, math.MaxInt64}, from: math.MinInt64},
		{
			name: "MaxInt64 removes every other hour", covered: []int64{math.MaxInt64, 7, math.MinInt64}, from: math.MaxInt64,
			removed: []int64{math.MinInt64, 7},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rs := newRetentionState(newTestRetention(tt.from))
			for _, h := range tt.covered {
				require.NoError(t, rs.add(h), "nothing is removed before the provider is asked")
			}

			err := rs.check(t.Context())
			if tt.removed == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, ErrRemoved)
			}
			assert.Equal(t, tt.removed, rs.removed())
		})
	}
}

func TestRetentionStateAddComparesAtOnce(t *testing.T) {
	t.Parallel()

	p := newTestRetention(10)
	rs := newRetentionState(p)
	require.NoError(t, rs.check(t.Context()))
	require.NoError(t, rs.add(10))
	require.NoError(t, rs.add(10), "an hour already covered is not added again")

	err := rs.add(9)
	require.ErrorIs(t, err, ErrRemoved, "a newly covered hour is compared with the boundary seen")
	assert.Equal(t, 1, p.callCount(), "without asking the provider")
	assert.Equal(t, []int64{9}, rs.removed())
}

func TestRetentionStateKeepsLargestBoundary(t *testing.T) {
	t.Parallel()

	p := &scriptedRetention{script: []int64{5, -100}}
	rs := newRetentionState(p)
	require.NoError(t, rs.check(t.Context()))
	require.NoError(t, rs.check(t.Context()), "a boundary that steps back")
	require.ErrorIs(t, rs.add(4), ErrRemoved, "the largest boundary seen is kept")
	assert.Equal(t, []int64{4}, rs.removed())
}

func TestRetentionStateProviderError(t *testing.T) {
	t.Parallel()

	p := newTestRetention(0)
	p.fail(errRetention)
	rs := newRetentionState(p)
	require.NoError(t, rs.add(-1))

	err := rs.check(t.Context())
	require.ErrorIs(t, err, errRetention)
	require.NotErrorIs(t, err, ErrRemoved)
	assert.Nil(t, rs.removed(), "a failed call sets no boundary")
}

func TestRetentionStateRecheckAfter(t *testing.T) {
	t.Parallel()

	cause := errors.New("read failed")

	p := newTestRetention(0)
	rs := newRetentionState(p)
	require.NoError(t, rs.add(0))
	assert.Equal(t, cause, rs.recheckAfter(t.Context(), cause), "nothing removed: the failure as is") //nolint:testifylint // identity, not errors.Is

	p.set(1)
	err := rs.recheckAfter(t.Context(), cause)
	require.ErrorIs(t, err, cause)
	require.ErrorIs(t, err, ErrRemoved)
	assert.Equal(t, []error{cause}, unwrapJoined(t, err)[:1], "the cause comes first")

	p = newTestRetention(0)
	p.fail(errRetention)
	rs = newRetentionState(p)
	require.NoError(t, rs.add(0))
	err = rs.recheckAfter(t.Context(), cause)
	require.ErrorIs(t, err, cause)
	require.ErrorIs(t, err, errRetention)
	require.NotErrorIs(t, err, ErrRemoved)
	assert.Equal(t, []error{cause}, unwrapJoined(t, err)[:1], "the cause comes first")
}

// unwrapJoined returns the errors joined in err, which must be a joined error.
func unwrapJoined(t *testing.T, err error) []error {
	t.Helper()

	j, ok := err.(interface{ Unwrap() []error }) //nolint:errorlint // the joined error itself
	require.True(t, ok, "a joined error: %v", err)

	return j.Unwrap()
}
