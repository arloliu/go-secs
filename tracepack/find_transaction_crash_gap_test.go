package tracepack

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// txCrashPack returns a pack id of captureHigh, its period the hour memTestHour + hours,
// holding a stop-unclean boundary at seq 5 of epoch 1, at blockTestHour + 5, with the gap bounds from and to;
// its footer is not valid when walked is set, so that a read takes the boundary from the record it decodes.
func txCrashPack(t testing.TB, id UUID, hours int64, from, to *int64, walked bool) []byte {
	t.Helper()

	file := txPack(t, id, func(m *PackMeta) {
		m.PeriodStart += hours * hourNs
		m.PeriodEnd += hours * hourNs
	}, txBlock(txBoundaryAt(t, 5, 1, BoundaryKindStopUnclean, from, to)))
	if walked {
		file = invalidFooterFile(t, file)
	}

	return withIDs(t, file, id, captureHigh)
}

// TestFindTransactionCrashGaps looks up txRecord's primary at 12, answered at 13, its window closed at 14,
// over the hours memTestHour and the next,
// beside a pack of another capture in memTestHour holding a stop-unclean boundary
// (the tracepack semantics specification §7.2, `incomplete` reasons, and §7.4, Crash gaps):
// a boundary whose gap meets the time range of the hours scheduled is a TxGapCrashGap gap beside the match,
// naming the hour of the read, the pack and the boundary, whether its pack's footer holds it or its record is decoded;
// a gap inverted or without bounds meets every range;
// one that misses the hours scheduled, or meets only the hour before them, is none.
// A keyless primary, which ends the lookup after its hour, reports a crash gap that meets its hour,
// and none that meets only the next hour scheduled.
func TestFindTransactionCrashGaps(t *testing.T) {
	t.Parallel()

	h := blockTestHour
	tests := []struct {
		name     string
		from, to *int64
		walked   bool
		// keyless takes the primary's SessionID away, so that the lookup returns early.
		keyless bool
		gap     bool
	}{
		{name: "meets the primary's hour", from: new(h + 100), to: new(h + 200), gap: true},
		{name: "meets the primary's hour, walked", from: new(h + 100), to: new(h + 200), walked: true, gap: true},
		{name: "meets the next hour scheduled", from: new(h + hourNs + 5), to: new(h + hourNs + 10), gap: true},
		{name: "inverted", from: new(h + 5*hourNs), to: new(h - 5*hourNs), gap: true},
		{name: "without bounds", gap: true},
		{name: "after the hours scheduled", from: new(h + 2*hourNs), to: new(h + 3*hourNs)},
		{name: "after the hours scheduled, walked", from: new(h + 2*hourNs), to: new(h + 3*hourNs), walked: true},
		{name: "in the hour before", from: new(h - hourNs), to: new(h - 1)},
		{name: "a keyless primary, meeting its hour", from: new(h + 100), to: new(h + 200), keyless: true, gap: true},
		{name: "a keyless primary, meeting only the next hour", from: new(h + hourNs + 5), to: new(h + hourNs + 10), keyless: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var edit func(r *Record)
			if tt.keyless {
				edit = func(r *Record) { r.FieldValidity &^= FieldValiditySessionID }
			}
			s := txSource(t,
				txPack(t, seg0, nil, txBlock(txRecord(12, edit), txReply(13, nil), txSocketClose(t, 14))),
				txCrashPack(t, seg2, 0, tt.from, tt.to, tt.walked))
			res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 2})
			require.NoError(t, err)
			var want []string
			switch {
			case tt.keyless:
				assert.Equal(t, TxNotFound, res.Outcome)
				want = append(want, "no-key@12")
			default:
				assert.Equal(t, TxMatched, res.Outcome)
			}
			if tt.gap {
				want = append(want, "crash-gap")
			}
			assert.Equal(t, want, nilIfEmpty(gapSeqs(res.Incomplete)))
			if !tt.gap {
				return
			}
			g := res.Incomplete[len(res.Incomplete)-1]
			assert.Equal(t, TxGap{
				Reason: TxGapCrashGap, Hours: []int64{memTestHour}, Pack: new(seg2), Block: -1, Offset: -1,
				Boundary: &Boundary{
					Capture: captureHigh, Seq: 5, Kind: BoundaryKindStopUnclean, TS: blockTestHour + 5, Epoch: 1, GapStart: tt.from, GapEnd: tt.to,
				},
				Err: g.Err,
			}, g)
			require.Error(t, g.Err)
		})
	}
}

// TestFindTransactionCrashGapsOnce reads stop-unclean boundaries of another capture whose gaps meet the hours scheduled:
// one boundary that packs of two hours hold is one TxGapCrashGap gap, named by the first hour that read it;
// two boundaries of one seq whose gap bounds differ are two.
func TestFindTransactionCrashGapsOnce(t *testing.T) {
	t.Parallel()

	h := blockTestHour
	answered := txPack(t, seg0, nil, txBlock(txRecord(12, nil), txReply(13, nil), txSocketClose(t, 14)))

	s := txHours(t,
		[][]byte{answered, txCrashPack(t, seg2, 0, new(h+100), new(h+200), false)},
		[][]byte{txCrashPack(t, seg3, 1, new(h+100), new(h+200), false)})
	res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 2})
	require.NoError(t, err)
	assert.Equal(t, TxMatched, res.Outcome)
	assert.Equal(t, []TxGapReason{TxGapScopeBreach, TxGapCrashGap}, gapReasons(res.Incomplete),
		"seg3 holds a record of the hour before its own, and the boundary once")
	assert.Equal(t, []int64{memTestHour}, res.Incomplete[1].Hours)
	assert.Equal(t, new(seg2), res.Incomplete[1].Pack)

	s = txSource(t, answered,
		txCrashPack(t, seg2, 0, new(h+100), new(h+200), false),
		txCrashPack(t, seg3, 0, new(h+100), new(h+300), false))
	res, err = findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 1})
	require.NoError(t, err)
	assert.Equal(t, []string{"crash-gap", "crash-gap"}, gapSeqs(res.Incomplete))
	assert.Equal(t, new(h+200), res.Incomplete[0].Boundary.GapEnd)
	assert.Equal(t, new(h+300), res.Incomplete[1].Boundary.GapEnd)
}

// TestFindTransactionCrashGapExplainsMissingPrimary looks up a primary that its hour's read does not yield,
// beside a crash gap: one of the primary's capture that meets the primary's hour explains the missing primary,
// which is TxNotFound with its no-key gap and the crash gap, and no later hour read;
// one of another capture, or one that meets only a later hour, explains nothing: ErrNotPrimary.
func TestFindTransactionCrashGapExplainsMissingPrimary(t *testing.T) {
	t.Parallel()

	h := blockTestHour
	own := func(from, to *int64) []byte {
		return txPack(t, seg2, nil, txBlock(txBoundaryAt(t, 5, txTestEpoch, BoundaryKindStopUnclean, from, to)))
	}
	tests := []struct {
		name      string
		crash     []byte
		explained bool
	}{
		{name: "of the primary's capture, meeting its hour", crash: own(new(h+100), new(h+200)), explained: true},
		{name: "of the primary's capture, meeting only the next hour", crash: own(new(h+hourNs+5), new(h+hourNs+10))},
		{name: "of another capture, meeting the primary's hour", crash: txCrashPack(t, seg2, 0, new(h+100), new(h+200), false)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := txSource(t, txPack(t, seg0, nil, txBlock(txSeqs(11, 13)...)), tt.crash)
			res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 2})
			requireOneScopeRead(t, s)
			if !tt.explained {
				require.ErrorIs(t, err, ErrNotPrimary)
				assert.Empty(t, res.Incomplete)

				return
			}
			require.NoError(t, err)
			assert.Equal(t, TxNotFound, res.Outcome)
			assert.Equal(t, []string{"no-key@12", "crash-gap"}, gapSeqs(res.Incomplete))
			assert.Empty(t, res.Records)
		})
	}
}
