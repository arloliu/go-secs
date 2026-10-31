package tracepack

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gapHours describes each gap as "<reason>@<hours - memTestHour>", followed by "/<seq>" when it has a seq, in order.
func gapHours(gaps []TxGap) []string {
	out := make([]string, len(gaps))
	for i := range gaps {
		g := &gaps[i]
		hours := make([]string, len(g.Hours))
		for j, h := range g.Hours {
			hours[j] = fmt.Sprint(h - memTestHour)
		}
		out[i] = fmt.Sprintf("%v@%s", g.Reason, strings.Join(hours, ","))
		if g.Seq != nil {
			out[i] += fmt.Sprintf("/%d", *g.Seq)
		}
	}

	return out
}

// txBreachPack writes steps, whose records keep their times, into a pack id whose period is the hour memTestHour + hours,
// as txPack does, unfinalized when walked is set.
func txBreachPack(t testing.TB, id UUID, hours int64, walked bool, steps []footerTestStep) []byte {
	t.Helper()

	return withIDs(t, writeRepairPack(t, CodecZstd, func(m *PackMeta) {
		m.QualityEvaluated = true
		m.PeriodStart += hours * hourNs
		m.PeriodEnd += hours * hourNs
	}, walked, steps).file, id, captureLow)
}

// TestFindTransactionScopeBreach looks up txRecord's primary at 12 in memTestHour
// beside versions whose ts_utc_ns lies outside the hour of the read that yields them
// (the tracepack semantics specification §7.2, the tracepack format specification I-13):
// each is a TxGapScopeBreach gap naming the hour and the version's seq, which makes the lookup incomplete,
// also beside a match or a closed window, whatever its capture;
// a version of the primary's capture is still classified, and kept as any other version is.
// The packs' footers are truthful, used or not (walked).
// A segment whose period is narrower than its hour, its records in the hour but outside the period, is no breach,
// and the same-hour clock step it stands for is none either.
// Each lookup returns what the snapshot-bounded check computes from the packs given.
func TestFindTransactionScopeBreach(t *testing.T) {
	t.Parallel()

	socketClose := func(seq uint64) Record {
		return testEventRecord(t, seq, blockTestHour+int64(seq), txTestEpoch, &TransportEvent{Event: EventSocketClose})
	}
	tests := []struct {
		name string
		// steps are the records of a pack in memTestHour, and later those of a pack in the next hour.
		steps, later []footerTestStep
		walked       bool
		// other adds a pack of another capture to memTestHour holding a record of the next hour.
		other    bool
		meta     func(m *PackMeta)
		outcome  TxOutcome
		breaches []string
		records  []string
	}{
		{
			name:    "the primary and its reply in the next hour",
			steps:   inHours(1, txBlock(txRecord(12, nil), txReply(13, nil))),
			outcome: TxMatched, breaches: []string{"scope-breach@0/12", "scope-breach@0/13"},
			records: []string{"12@0 primary", "13@0 candidate in eligible decidable valid"},
		},
		{
			name:  "the primary and its reply in the next hour, walked",
			steps: inHours(1, txBlock(txRecord(12, nil), txReply(13, nil))), walked: true,
			outcome: TxMatched, breaches: []string{"scope-breach@0/12", "scope-breach@0/13"},
			records: []string{"12@0 primary", "13@0 candidate in eligible decidable valid"},
		},
		{
			name:  "a record of another capture in the next hour",
			steps: txBlock(txRecord(12, nil), txReply(13, nil)), other: true,
			outcome: TxMatched, breaches: []string{"scope-breach@0/40"},
			records: []string{"12@0 primary", "13@0 candidate in eligible decidable valid"},
		},
		{
			name:    "a reply in the next hour",
			steps:   append(txBlock(txRecord(12, nil)), inHours(1, txBlock(txReply(13, nil)))...),
			outcome: TxMatched, breaches: []string{"scope-breach@0/13"},
			records: []string{"12@0 primary", "13@0 candidate in eligible decidable valid"},
		},
		{
			name:  "a reply in the next hour, walked",
			steps: append(txBlock(txRecord(12, nil)), inHours(1, txBlock(txReply(13, nil)))...), walked: true,
			outcome: TxMatched, breaches: []string{"scope-breach@0/13"},
			records: []string{"12@0 primary", "13@0 candidate in eligible decidable valid"},
		},
		{
			name:  "a closing record in the next hour, walked",
			steps: append(txBlock(txRecord(12, nil)), inHours(1, txBlock(socketClose(13)))...), walked: true,
			outcome: TxNotFound, breaches: []string{"scope-breach@0/13"},
			records: []string{"12@0 primary", "13@0 closing bound"},
		},
		{
			name:    "a closing record in the next hour",
			steps:   append(txBlock(txRecord(12, nil)), inHours(1, txBlock(socketClose(13)))...),
			outcome: TxNotFound, breaches: []string{"scope-breach@0/13"},
			records: []string{"12@0 primary", "13@0 closing bound"},
		},
		{
			name:    "a record below the primary in the hour before",
			steps:   append(inHours(-1, txBlock(txNote(10, nil))), txBlock(txRecord(12, nil), txRecord(13, nil))...),
			outcome: TxNotFound, breaches: []string{"scope-breach@0/10"},
			records: []string{"12@0 primary", "13@0 same-key-primary bound"},
		},
		{
			name:  "a record of a later scope in the hour after it",
			steps: txBlock(txRecord(12, nil), txReply(13, nil)), later: inHours(2, txBlock(txNote(20, nil))),
			outcome: TxMatched, breaches: []string{"scope-breach@1/20"},
			records: []string{"12@0 primary", "13@0 candidate in eligible decidable valid"},
		},
		{
			name:    "a reply at the first nanosecond of the next hour",
			steps:   append(txBlock(txRecord(12, nil)), atTS((memTestHour+1)*hourNs, txBlock(txReply(13, nil)))...),
			outcome: TxMatched, breaches: []string{"scope-breach@0/13"},
			records: []string{"12@0 primary", "13@0 candidate in eligible decidable valid"},
		},
		{
			name:    "a reply at the last nanosecond of the hour",
			steps:   append(txBlock(txRecord(12, nil)), atTS((memTestHour+1)*hourNs-1, txBlock(txReply(13, nil)))...),
			outcome: TxMatched, breaches: []string{},
			records: []string{"12@0 primary", "13@0 candidate in eligible decidable valid"},
		},
		{
			name:    "a record below the primary at the last nanosecond of the hour before",
			steps:   append(atTS(memTestHour*hourNs-1, txBlock(txNote(10, nil))), txBlock(txRecord(12, nil), txRecord(13, nil))...),
			outcome: TxNotFound, breaches: []string{"scope-breach@0/10"},
			records: []string{"12@0 primary", "13@0 same-key-primary bound"},
		},
		{
			name:    "a segment whose period starts in the middle of the hour",
			steps:   txBlock(txRecord(12, nil), txRecord(13, nil)),
			meta:    func(m *PackMeta) { m.PeriodStart += hourNs / 2 },
			outcome: TxNotFound, breaches: []string{},
			records: []string{"12@0 primary", "13@0 same-key-primary bound"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := newMemSource()
			file := txBreachPack(t, seg0, 0, tt.walked, tt.steps)
			if tt.meta != nil {
				file = txPack(t, seg0, tt.meta, tt.steps)
			}
			s.addPack(t, memTestHour, file)
			if tt.later != nil {
				s.addPack(t, memTestHour+1, txBreachPack(t, seg1, 1, false, tt.later))
			}
			if tt.other {
				s.addPack(t, memTestHour, withIDs(t, txBreachPack(t, seg2, 0, false, inHours(1, txBlock(txNote(40, nil)))), seg2, captureHigh))
			}
			key, opts := txKeyAt(12), TxOptions{MaxHours: 2}
			res, err := findTx(t, t.Context(), s, key, opts)
			require.NoError(t, err)
			assert.Equal(t, tt.outcome, res.Outcome)
			breaches := slices.DeleteFunc(gapHours(res.Incomplete), func(g string) bool { return !strings.HasPrefix(g, "scope-breach") })
			assert.Equal(t, tt.breaches, breaches)
			assert.Equal(t, tt.records, recordFlags(res.Records))
			for _, g := range res.Incomplete {
				if g.Reason == TxGapScopeBreach {
					// The snapshot-bounded check compares the pack and block.
					assert.True(t, g.Pack != nil && g.Block >= 0 && g.Offset == -1, "the gap names the pack and block")
				}
			}

			snap := newTxSnapshot(t, s, captureLow, key.Hour, key.Hour+int64(opts.MaxHours))
			want, notPrimary := snap.expect(key, opts.MaxHours)
			requireTxExpected(t, want, notPrimary, res, err)
		})
	}
}

// TestFindTransactionScopeBreachSettlement looks up a primary that its hour's read does not yield,
// beside a version of another hour below or above the primary's seq in the hour:
// the breach explains the missing primary, as a read defect does,
// so the lookup is TxNotFound with the scope-breach and no-key gaps and reads no later hour;
// without the breach, the missing primary is ErrNotPrimary.
// A record at the primary's seq that is not a primary is ErrNotPrimary beside a breach, as it is beside an index mismatch.
// The state's charge equals its recount on each path.
func TestFindTransactionScopeBreachSettlement(t *testing.T) {
	t.Parallel()

	later := [][]byte{txPackIn(t, seg1, 1, nil, txBlock(txReply(14, nil)))}
	tests := []struct {
		name   string
		files  [][]byte
		seq    uint64
		gaps   []string
		notPri bool
	}{
		{
			name:  "a breach below the primary",
			files: [][]byte{txBreachPack(t, seg0, 0, false, append(inHours(-1, txBlock(txNote(10, nil))), txBlock(txNote(11, nil), txNote(13, nil))...))},
			seq:   12, gaps: []string{"scope-breach@0/10", "no-key@0/12"},
		},
		{
			name:  "a breach above the primary",
			files: [][]byte{txBreachPack(t, seg0, 0, false, append(txBlock(txNote(11, nil)), inHours(1, txBlock(txNote(13, nil)))...))},
			seq:   12, gaps: []string{"scope-breach@0/13", "no-key@0/12"},
		},
		{
			name:  "no breach",
			files: [][]byte{txBreachPack(t, seg0, 0, false, txBlock(txNote(11, nil), txNote(13, nil)))},
			seq:   12, gaps: []string{}, notPri: true,
		},
		{
			name:  "a reply at the primary's seq beside a breach",
			files: [][]byte{txBreachPack(t, seg0, 0, false, append(inHours(-1, txBlock(txNote(10, nil))), txBlock(txReply(12, nil))...))},
			seq:   12, gaps: []string{"scope-breach@0/10"}, notPri: true,
		},
		{
			name:  "a reply at the primary's seq beside an index mismatch",
			files: [][]byte{txMisindexed(t, 20), txPack(t, seg1, nil, txBlock(txReply(15, nil)))},
			seq:   15, gaps: []string{"index@0", "index@0/15"}, notPri: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := txHours(t, tt.files, later)
			key := txKeyAt(tt.seq)
			res, err := findTx(t, t.Context(), s, key, TxOptions{MaxHours: 2})
			if tt.notPri {
				require.ErrorIs(t, err, ErrNotPrimary)
				assert.Zero(t, res.Outcome)
			} else {
				require.NoError(t, err)
				assert.Equal(t, TxNotFound, res.Outcome)
			}
			assert.Equal(t, tt.gaps, gapHours(res.Incomplete))
			assert.Empty(t, res.Records)
			scopes := s.counts()
			assert.Equal(t, map[int64]int{memTestHour: 1}, scopes, "no later hour read")

			if tt.name != "a reply at the primary's seq beside an index mismatch" {
				snap := newTxSnapshot(t, s, captureLow, key.Hour, key.Hour+2)
				want, notPrimary := snap.expect(key, 2)
				requireTxExpected(t, want, notPrimary, res, err)
			}
		})
	}
}

// TestFindTransactionScopeBreachGapOrder pins where a read lists its scope-breach gaps:
// as the read yields their versions, before the gaps its mapping adds, the index mismatch first among them,
// and before the settlement's gaps.
// The misindexed block's same-key primary at 20, yielded before the primary, is not classified, so the window stays open.
func TestFindTransactionScopeBreachGapOrder(t *testing.T) {
	t.Parallel()

	breach := txBreachPack(t, seg1, 0, false, append(txBlock(txRecord(12, nil)), inHours(1, txBlock(txNote(13, nil), txNote(14, nil)))...))
	s := txHours(t, [][]byte{txMisindexed(t, 20), breach})
	res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxHours: 1})
	require.NoError(t, err)
	assert.Equal(t, TxNotFound, res.Outcome)
	assert.Equal(t, []string{"scope-breach@0/13", "scope-breach@0/14", "index@0", "index@0/12", "open-window@"}, gapHours(res.Incomplete))
}

// TestTxTruthBreach requires the safety check to reject a complete TxNotFound or TxMatched
// whose comparison domain holds a version outside its pack's hour, and to pass an incomplete lookup.
func TestTxTruthBreach(t *testing.T) {
	t.Parallel()

	tr := newTxTruth(txSeqRange(11, 14))
	txTruthPack(t, tr, seg0, 0, nil, txNote(11, nil), txRecord(12, nil), txNote(13, nil), inHour(1, txRecord(14, nil)))
	key := txKeyAt(12)
	want := fmt.Sprintf("pack %v of hour %d holds seq 14 of hour %d", seg0, memTestHour, memTestHour+1)
	for _, outcome := range []TxOutcome{TxNotFound, TxMatched} {
		res := TxResult{Outcome: outcome}
		assert.Contains(t, tr.violations(key, 1, &res, nil), want, "%v", outcome)
	}
	res := TxResult{Outcome: TxNotFound, Incomplete: []TxGap{{Reason: TxGapScopeBreach}}}
	assert.Empty(t, tr.violations(key, 1, &res, nil), "an incomplete lookup")
}

// atTS returns steps moved in time so that the first record's ts_utc_ns is ts, the others keeping their distance to it,
// and the monotonic times moved with them.
func atTS(ts int64, steps []footerTestStep) []footerTestStep {
	out := slices.Clone(steps)
	d := ts - out[0].rec.TSUTCNs
	for i := range out {
		out[i].rec.TSUTCNs += d
		out[i].rec.MonoNs += d
	}

	return out
}
