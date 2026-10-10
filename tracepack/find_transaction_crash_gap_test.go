package tracepack

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFindTransactionReadCrashGapsFiltered looks up txRecord's primary at 12 in one scope
// whose pack holds a stop-unclean boundary at 20, of a later epoch than the primary's, which a socket-close at 14 closes;
// the reads of a lookup report the boundary as a crash gap, which the lookup currently skips,
// since it evaluates stop-unclean boundaries itself:
// a gap outside the scheduled hours adds nothing, and one inside them is reported once, as a barrier;
// a primary that a coverage entry explains ends the lookup early with the no-key gap alone, whatever the gap;
// and a missing primary in an hour whose only defect is the crash gap is not a primary.
func TestFindTransactionReadCrashGapsFiltered(t *testing.T) {
	t.Parallel()

	h := blockTestHour
	outside := func() (*int64, *int64) { return new(h + 5*hourNs), new(h + 5*hourNs + 10) }
	inside := func() (*int64, *int64) { return new(h + 100), new(h + 200) }
	unclean := func(gap func() (*int64, *int64)) Record {
		from, to := gap()

		return txBoundaryAt(t, 20, txTestEpoch+1, BoundaryKindStopUnclean, from, to)
	}
	coverPrimary := func(m *PackMeta) { m.Coverage = []Coverage{{SeqFirst: new(uint64(12)), SeqLast: new(uint64(12))}} }

	tests := []struct {
		name    string
		meta    func(m *PackMeta)
		first   Record
		gap     func() (*int64, *int64)
		outcome TxOutcome
		gaps    []string
	}{
		{name: "gap outside the hours", first: txRecord(12, nil), gap: outside, outcome: TxMatched, gaps: []string{}},
		{name: "gap inside the hours", first: txRecord(12, nil), gap: inside, outcome: TxMatched, gaps: []string{"barrier"}},
		{name: "early return, gap outside", meta: coverPrimary, first: txRecord(11, nil), gap: outside, outcome: TxIncomplete,
			gaps: []string{"no-key@12"}},
		{name: "early return, gap inside", meta: coverPrimary, first: txRecord(11, nil), gap: inside, outcome: TxIncomplete,
			gaps: []string{"no-key@12"}},
	}
	for _, tt := range tests {
		file := txPack(t, seg0, tt.meta, txBlock(tt.first, txReply(13, nil), txSocketClose(t, 14), unclean(tt.gap)))
		res, err := findTx(t, t.Context(), txSource(t, true, file), txKeyAt(12), TxOptions{MaxScopes: 1})
		require.NoError(t, err, tt.name)
		assert.Equal(t, tt.outcome, res.Outcome, tt.name)
		assert.Equal(t, tt.gaps, gapSeqs(res.Gaps), tt.name)
		assert.NotContains(t, gapReasons(res.Gaps), TxGapRead, tt.name)
	}

	file := txPack(t, seg0, nil, txBlock(txRecord(11, nil), txReply(13, nil), txSocketClose(t, 14), unclean(inside)))
	_, err := findTx(t, t.Context(), txSource(t, true, file), txKeyAt(12), TxOptions{MaxScopes: 1})
	require.ErrorIs(t, err, ErrNotPrimary, "the lookup does not take the crash gap as explaining the missing primary")
}
