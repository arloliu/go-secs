package tracepack

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// txBoundaryAt returns a capture-boundary record of kind at seq in epoch, at blockTestHour + seq,
// its gap bounds from and to.
func txBoundaryAt(t testing.TB, seq uint64, epoch uint32, kind BoundaryKind, from, to *int64) Record {
	t.Helper()

	return testEventRecord(t, seq, blockTestHour+int64(seq), epoch,
		&TransportEvent{Event: EventCaptureBoundary, BoundaryKind: new(kind), GapStart: from, GapEnd: to})
}

// edited returns r changed by edit.
func edited(r Record, edit func(r *Record)) Record {
	edit(&r)

	return r
}

// TestFindTransactionCoverageGaps looks up the primary at 12 of a window (12, 16) whose seqs one hour's read yields,
// beside one coverage entry of a pack read:
// the entry makes a TxGapCoverage gap only when it meets both the window's seqs and the time range of the hours scheduled
// (the tracepack format specification §5), an absent bound unbounded, an entry inverted on either side meeting every query:
// an entry that meets the window's seqs but not the hours is none, and neither is the reverse.
// An entry of another capture meets none, one naming the primary's capture as its own does,
// and so does none in a pack of another capture, whose entries without capture_id belong to that capture;
// the time side spans every hour scheduled.
// An empty window, (12, 13), is met only by an inverted entry.
func TestFindTransactionCoverageGaps(t *testing.T) {
	t.Parallel()

	from, to := blockTestHour, blockTestHour+hourNs
	seqs := func(first, last uint64) Coverage { return Coverage{SeqFirst: new(first), SeqLast: new(last)} }
	timed := func(c Coverage, start, end *int64) Coverage {
		c.TimeStart, c.TimeEnd = start, end
		return c
	}
	other := seqs(14, 14)
	other.CaptureID = new(captureHigh)
	own := seqs(14, 14)
	own.CaptureID = new(captureLow)
	tests := []struct {
		name string
		cov  Coverage
		// empty bounds the window at 13, so that it holds no seq.
		empty bool
		// alone puts the entry in a pack of its own, without records.
		alone bool
		// foreign puts the entry in a pack of another capture, without records.
		foreign bool
		// scopes is the number of hours read, the next one empty; zero means 1.
		scopes int
		gap    bool
	}{
		{name: "inside the window", cov: seqs(14, 14), gap: true},
		{name: "at the window's end", cov: seqs(16, 20)},
		{name: "at the primary", cov: seqs(5, 12)},
		{name: "spanning the window", cov: seqs(5, 20), gap: true},
		{name: "without bounds", cov: Coverage{}, gap: true},
		{name: "one-sided", cov: Coverage{SeqLast: new(uint64(13))}, gap: true},
		{name: "inverted seqs outside the window", cov: seqs(30, 20), gap: true},
		{name: "inverted times outside the hour", cov: timed(seqs(20, 30), new(to+5), new(from-5)), gap: true},
		{name: "another capture", cov: other},
		{name: "the primary's capture", cov: own, gap: true},
		{name: "seqs inside, times outside", cov: timed(seqs(14, 14), new(to), new(to+10))},
		{name: "seqs inside, times in the next hour read", cov: timed(seqs(14, 14), new(to), new(to+10)), scopes: 2, gap: true},
		{name: "seqs outside, times inside", cov: timed(seqs(20, 30), new(from), new(to-1))},
		{name: "both inside", cov: timed(seqs(14, 14), new(from-10), new(from)), gap: true},
		{name: "in a pack without records", cov: seqs(13, 15), alone: true, gap: true},
		{name: "in a pack of another capture", cov: seqs(13, 15), foreign: true},
		{name: "an empty window", cov: seqs(5, 20), empty: true},
		{name: "an empty window, inverted seqs", cov: seqs(30, 20), empty: true, gap: true},
		{name: "an empty window, inverted times", cov: timed(seqs(20, 30), new(to), new(from)), empty: true, gap: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			recs := []Record{txRecord(12, nil), txNote(13, nil), txNote(14, nil), txNote(15, nil), txRecord(16, nil)}
			if tt.empty {
				recs = []Record{txRecord(12, nil), txRecord(13, nil)}
			}
			withCov := func(m *PackMeta) { m.Coverage = []Coverage{tt.cov} }
			files := [][]byte{txPack(t, seg0, withCov, txBlock(recs...))}
			holder := seg0
			if tt.alone || tt.foreign {
				files = [][]byte{txPack(t, seg0, nil, txBlock(recs...)), txPack(t, seg1, withCov, nil)}
				holder = seg1
			}
			if tt.foreign {
				files[1] = withIDs(t, files[1], seg1, captureHigh)
			}
			s := txSource(t, files...)
			res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: max(tt.scopes, 1)})
			require.NoError(t, err)
			if !tt.gap {
				assert.Equal(t, TxNotFound, res.Outcome)
				assert.Empty(t, res.Incomplete)

				return
			}
			assert.Equal(t, TxNotFound, res.Outcome)
			require.Len(t, res.Incomplete, 1)
			g := res.Incomplete[0]
			assert.Equal(t, TxGap{
				Reason: TxGapCoverage, Hours: []int64{memTestHour}, Pack: new(holder), Block: -1, Offset: -1, Coverage: &tt.cov, Err: g.Err,
			}, g)
			require.Error(t, g.Err)
		})
	}
}

// TestFindTransactionCoverageWindowShrinks reads, in the primary's hour,
// a window bounded at 20 with a coverage entry over 17 and 18;
// the next hour holds a same-key primary at 15, after 16 to 20 in time by a backward clock step.
// Reading one hour, the entry meets the window; reading both, the window shrinks to (12, 15) and the entry meets nothing.
func TestFindTransactionCoverageWindowShrinks(t *testing.T) {
	t.Parallel()

	cov := Coverage{SeqFirst: new(uint64(17)), SeqLast: new(uint64(18))}
	first := txPack(t, seg0, func(m *PackMeta) { m.Coverage = []Coverage{cov} },
		txBlock(txRecord(12, nil), txNote(13, nil), txNote(14, nil), txNote(16, nil), txNote(17, nil), txNote(18, nil), txNote(19, nil), txRecord(20, nil)))
	later := txPackIn(t, seg1, 1, nil, txBlock(txRecord(15, nil)))
	for _, tt := range []struct {
		scopes int
		gaps   []string
	}{{scopes: 1, gaps: []string{"seq-gap@15", "coverage"}}, {scopes: 2}} {
		res, err := findTx(t, t.Context(), txHours(t, [][]byte{first}, [][]byte{later}), txKeyAt(12), TxOptions{MaxScopes: tt.scopes})
		require.NoError(t, err)
		assert.Equal(t, tt.gaps, nilIfEmpty(gapSeqs(res.Incomplete)), "%d scopes", tt.scopes)
	}
}

// TestFindTransactionCaptureBoundaries looks up the primary at 12 in epoch 1 beside capture-boundary records read:
// every capture-boundary record of the primary's epoch read above the primary (the epoch-boundary condition)
// is a TxGapCaptureBoundary gap that names its hour, pack and block, a stop-unclean included,
// except the clean stop of the epoch that bounds the window;
// a stop of the epoch after the window's end is one;
// a boundary of another epoch, one below the primary, and one in an hour the lookup does not read are none,
// and a footer's boundary entry is never one.
func TestFindTransactionCaptureBoundaries(t *testing.T) {
	t.Parallel()

	stop := func(seq uint64) Record { return txBoundaryAt(t, seq, txTestEpoch, BoundaryKindStop, nil, nil) }
	gap := func(seq uint64) Record { return txBoundaryAt(t, seq, txTestEpoch, BoundaryKindGap, nil, nil) }
	past := new(blockTestHour - 2*hourNs)
	tests := []struct {
		name string
		recs []Record
		// later is held by a pack of the next hour, which the lookup reads only when hours is 2.
		later []Record
		hours int
		end   uint64
		gaps  []string
	}{
		{name: "a clean stop that bounds the window", recs: []Record{txNote(13, nil), stop(14)}, end: 14},
		{name: "beside a gap boundary", recs: []Record{gap(13), stop(14)}, end: 14, gaps: []string{"capture-boundary@13"}},
		{name: "beside a stop-unclean of the epoch", recs: []Record{txBoundaryAt(t, 13, txTestEpoch, BoundaryKindStopUnclean, past, past), stop(14)},
			end: 14, gaps: []string{"capture-boundary@13"}},
		{name: "a gap boundary of another epoch", recs: []Record{txBoundaryAt(t, 13, 2, BoundaryKindGap, nil, nil), stop(14)}, end: 14},
		{name: "a stop after the window's end", recs: []Record{txNote(13, nil), txRecord(14, nil), stop(16)}, end: 14,
			gaps: []string{"capture-boundary@16"}},
		{name: "a gap boundary in the next hour, read", recs: []Record{txNote(13, nil), txRecord(14, nil)},
			later: []Record{gap(20)}, hours: 2, end: 14, gaps: []string{"capture-boundary@20"}},
		{name: "a gap boundary in an hour not read", recs: []Record{txNote(13, nil), txRecord(14, nil)},
			later: []Record{gap(20)}, hours: 1, end: 14},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := txHours(t, [][]byte{txPack(t, seg0, nil, txBlock(append([]Record{txRecord(12, nil)}, tt.recs...)...))})
			if tt.later != nil {
				s.addPack(t, memTestHour+1, txPackIn(t, seg1, 1, nil, txBlock(tt.later...)))
			}
			res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: max(tt.hours, 1)})
			require.NoError(t, err)
			assert.Equal(t, TxNotFound, res.Outcome)
			assert.Equal(t, new(tt.end), res.WindowEnd)
			assert.Equal(t, tt.gaps, nilIfEmpty(gapSeqs(res.Incomplete)))
			for _, g := range res.Incomplete {
				require.Equal(t, TxGapCaptureBoundary, g.Reason)
				require.NotNil(t, g.Boundary)
				assert.Equal(t, *g.Seq, g.Boundary.Seq)
				assert.Equal(t, uint32(txTestEpoch), g.Boundary.Epoch)
				assert.Equal(t, 0, g.Block)
				require.NotNil(t, g.Pack)
				if *g.Seq == 20 {
					assert.Equal(t, []int64{memTestHour + 1}, g.Hours)
					assert.Equal(t, seg1, *g.Pack)
				} else {
					assert.Equal(t, []int64{memTestHour}, g.Hours)
					assert.Equal(t, seg0, *g.Pack)
				}
			}
		})
	}
}

// TestFindTransactionBoundaryRecordsRead reads a segment that holds a gap boundary of the primary's epoch,
// with gap bounds, and a clean stop that bounds the window:
// the record read is a TxGapCaptureBoundary gap that names its hour, pack and block, and carries the record as a Boundary;
// the stop is none.
func TestFindTransactionBoundaryRecordsRead(t *testing.T) {
	t.Parallel()

	s := txSource(t, txPack(t, seg0, nil, slices.Concat(
		txBlock(txRecord(12, nil), txNote(13, nil)),
		txBlock(txBoundaryAt(t, 14, txTestEpoch, BoundaryKindGap, new(int64(5)), new(int64(9))), txBoundaryAt(t, 15, txTestEpoch, BoundaryKindStop, nil, nil)),
	)))
	res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 1})
	require.NoError(t, err)
	assert.Equal(t, TxNotFound, res.Outcome)
	assert.Equal(t, new(uint64(15)), res.WindowEnd)
	require.Equal(t, []string{"capture-boundary@14"}, gapSeqs(res.Incomplete))
	g := res.Incomplete[0]
	assert.Equal(t, TxGap{
		Reason: TxGapCaptureBoundary, Hours: []int64{memTestHour}, Pack: new(seg0), Block: 1, Offset: -1, Seq: new(uint64(14)),
		Boundary: &Boundary{
			CaptureID: captureLow, Seq: 14, Kind: BoundaryKindGap, TS: blockTestHour + 14, Epoch: txTestEpoch,
			GapStart: new(int64(5)), GapEnd: new(int64(9)),
		},
		Err: g.Err,
	}, g)
	require.Error(t, g.Err)
}

// TestFindTransactionOrderingUncertain reads a record of the primary's epoch with ordering-uncertain
// below the primary, at it, after the window's end, and in a pack whose footer is not used, read by walking:
// each is a TxGapOrderingUncertain gap at the first such record read;
// one of another epoch is none, and so is one of another capture in the primary's epoch.
func TestFindTransactionOrderingUncertain(t *testing.T) {
	t.Parallel()

	uncertain := func(epoch uint32) func(r *Record) {
		return func(r *Record) {
			r.Epoch = epoch
			r.Quality |= QualityOrderingUncertain
		}
	}
	tests := []struct {
		name   string
		recs   []Record
		walked bool
		// other adds a pack of another capture holding an ordering-uncertain record of the primary's epoch.
		other   bool
		outcome TxOutcome
		gaps    []string
	}{
		{name: "below the primary", recs: []Record{txRecord(11, uncertain(txTestEpoch)), txRecord(12, nil), txNote(13, nil), txRecord(14, nil)},
			outcome: TxNotFound, gaps: []string{"ordering-uncertain@11"}},
		{name: "at the primary", recs: []Record{txRecord(12, uncertain(txTestEpoch)), txNote(13, nil), txRecord(14, nil)},
			outcome: TxNotFound, gaps: []string{"ordering-uncertain@12"}},
		{name: "after the window", recs: []Record{txRecord(12, nil), txNote(13, nil), txRecord(14, nil), txReply(20, uncertain(txTestEpoch))},
			outcome: TxNotFound, gaps: []string{"ordering-uncertain@20"}},
		{name: "in another epoch", recs: []Record{txRecord(11, uncertain(2)), txRecord(12, nil), txRecord(13, uncertain(2)), txRecord(14, nil)},
			outcome: TxNotFound},
		{name: "in a walked pack", recs: []Record{txRecord(12, nil), edited(txNote(13, nil), uncertain(txTestEpoch)), txRecord(14, nil)}, walked: true,
			outcome: TxNotFound, gaps: []string{"ordering-uncertain@13"}},
		{name: "in another capture", recs: []Record{txRecord(12, nil), txNote(13, nil), txRecord(14, nil)}, other: true,
			outcome: TxNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			file := txPack(t, seg0, nil, txBlock(tt.recs...))
			if tt.walked {
				file = invalidFooterFile(t, file)
			}
			files := [][]byte{file}
			if tt.other {
				files = append(files, withIDs(t, txPack(t, seg1, nil, txBlock(txRecord(11, uncertain(txTestEpoch)))), seg1, captureHigh))
			}
			res, err := findTx(t, t.Context(), txSource(t, files...), txKeyAt(12), TxOptions{MaxScopes: 1})
			require.NoError(t, err)
			assert.Equal(t, tt.outcome, res.Outcome)
			assert.Equal(t, new(uint64(14)), res.WindowEnd)
			assert.Equal(t, tt.gaps, nilIfEmpty(gapSeqs(res.Incomplete)))
			if len(res.Incomplete) > 0 {
				g := res.Incomplete[0]
				assert.Equal(t, TxGap{
					Reason: TxGapOrderingUncertain, Hours: []int64{memTestHour}, Pack: new(seg0), Block: 0, Offset: -1, Seq: g.Seq, Err: g.Err,
				}, g)
				require.Error(t, g.Err)
			}
		})
	}
}

// TestFindTransactionCompletenessGapOrder looks up a primary
// beside one reason of each kind that the records' flags do not decide:
// a coverage entry, a stop-unclean boundary of the capture in another epoch whose gap meets the hour read,
// a gap boundary of the epoch and an ordering-uncertain record.
// They follow the window's gaps in this order: coverage, crash gap, capture-boundary, ordering-uncertain.
func TestFindTransactionCompletenessGapOrder(t *testing.T) {
	t.Parallel()

	cov := Coverage{SeqFirst: new(uint64(13)), SeqLast: new(uint64(13))}
	s := txSource(t, txPack(t, seg0, func(m *PackMeta) { m.Coverage = []Coverage{cov} }, txBlock(
		txRecord(11, func(r *Record) { r.Quality |= QualityOrderingUncertain }), txRecord(12, nil), txNote(13, nil),
		txBoundaryAt(t, 14, txTestEpoch, BoundaryKindGap, nil, nil), txRecord(16, nil),
		txBoundaryAt(t, 100, 2, BoundaryKindStopUnclean, new(blockTestHour+5), new(blockTestHour+6)),
	)))
	res, l, err := findTxState(t, s, TxOptions{MaxScopes: 1})
	require.NoError(t, err)
	assert.NotSame(t, l.coverage[0].cov, res.Incomplete[1].Coverage, "the gap holds a copy")
	assert.Equal(t, TxNotFound, res.Outcome)
	assert.Equal(t, []string{
		"seq-gap@15", "coverage", "crash-gap", "capture-boundary@14", "ordering-uncertain@11",
	}, gapSeqs(res.Incomplete))
	assert.Equal(t, &cov, res.Incomplete[1].Coverage)
	assert.Equal(t, &Boundary{
		CaptureID: captureLow, Seq: 100, Kind: BoundaryKindStopUnclean, TS: blockTestHour + 100, Epoch: 2,
		GapStart: new(blockTestHour + 5), GapEnd: new(blockTestHour + 6),
	}, res.Incomplete[2].Boundary)
	assert.NotSame(t, l.crash[0].b, res.Incomplete[2].Boundary, "the gap holds a copy")
	assert.Equal(t, &Boundary{CaptureID: captureLow, Seq: 14, Kind: BoundaryKindGap, TS: blockTestHour + 14, Epoch: txTestEpoch}, res.Incomplete[3].Boundary)
}
