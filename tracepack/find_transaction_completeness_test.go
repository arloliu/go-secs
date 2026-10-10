package tracepack

import (
	"context"
	"errors"
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

// txUnclean returns a stop-unclean boundary of capture at seq in epoch with the gap bounds from and to,
// as per-capture evidence records it.
func txUnclean(capture UUID, seq uint64, epoch uint32, from, to *int64) Boundary {
	return Boundary{Capture: capture, Seq: seq, Kind: BoundaryKindStopUnclean, TS: blockTestHour + int64(seq), Epoch: epoch, GapStart: from, GapEnd: to}
}

// txSpySource is a memSource whose observations return extra boundaries from Barriers beside the source's,
// and keep the evidence and the barriers they returned, so that a test can change them once the lookup is done.
type txSpySource struct {
	*memSource
	extra []Boundary
	obs   *txSpyObservation
}

// txSpyObservation is an observation of a txSpySource.
type txSpyObservation struct {
	Observation
	extra    []Boundary
	evidence CaptureEvidence
	barriers []Boundary
}

// Observe observes the memSource, wrapping the observation.
func (s *txSpySource) Observe(ctx context.Context, capture UUID, from, to int64) (Observation, error) {
	o, err := s.memSource.Observe(ctx, capture, from, to)
	if err != nil {
		return nil, err
	}
	s.obs = &txSpyObservation{Observation: o, extra: s.extra}

	return s.obs, nil
}

// Evidence returns the memSource's evidence, keeping it.
func (o *txSpyObservation) Evidence(ctx context.Context) (CaptureEvidence, error) {
	e, err := o.Observation.Evidence(ctx)
	o.evidence = e

	return e, err
}

// Barriers returns the memSource's barriers and a copy of the extra boundaries, keeping them.
func (o *txSpyObservation) Barriers(ctx context.Context, from, to int64) ([]Boundary, error) {
	bs, err := o.Observation.Barriers(ctx, from, to)
	bs = append(bs, cloneBoundaries(o.extra)...)
	o.barriers = bs

	return bs, err
}

// TestFindTransactionCoverageGaps looks up the primary at 12 of a window (12, 16) whose seqs one scope read yields,
// beside one coverage entry of a pack read:
// the entry makes a TxGapCoverage gap only when it meets both the window's seqs and the time range of the hour read
// (the tracepack format specification §5), an absent bound unbounded, an entry inverted on either side meeting every query;
// an entry of another capture meets none, one naming the primary's capture as its own does,
// and the time side spans every hour read.
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
		// scopes is the number of hours read, the next one indexed and empty; zero means 1.
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
			if tt.alone {
				files = [][]byte{txPack(t, seg0, nil, txBlock(recs...)), txPack(t, seg1, withCov, nil)}
				holder = seg1
			}
			s := txSource(t, true, files...)
			s.setIndexed(captureLow, memTestHour+1, true)
			res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: max(tt.scopes, 1)})
			require.NoError(t, err)
			if !tt.gap {
				assert.Equal(t, TxUnmatched, res.Outcome)
				assert.Empty(t, res.Gaps)

				return
			}
			assert.Equal(t, TxIncomplete, res.Outcome)
			require.Len(t, res.Gaps, 1)
			g := res.Gaps[0]
			assert.Equal(t, TxGap{
				Reason: TxGapCoverage, Hours: []int64{memTestHour}, Pack: new(holder), Block: -1, Offset: -1, Coverage: &tt.cov, Err: g.Err,
			}, g)
			require.Error(t, g.Err)
		})
	}
}

// TestFindTransactionCoverageWindowShrinks reads, in the primary's scope,
// a window bounded at 20 with a coverage entry over 17 and 18;
// the next scope holds a same-key primary at 15, after 16 to 20 in time by a backward clock step.
// Reading one scope, the entry meets the window; reading both, the window shrinks to (12, 15) and the entry meets nothing.
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
		assert.Equal(t, tt.gaps, nilIfEmpty(gapSeqs(res.Gaps)), "%d scopes", tt.scopes)
	}
}

// TestFindTransactionEpochBarrier looks up the primary at 12 in epoch 1
// of a capture whose evidence holds a stop-unclean boundary
// whose gap lies outside the hour read, so that only the epoch barrier can apply (the tracepack storage specification §5):
// it applies while the lookup read no closing record of the primary's epoch above it, without a conflict;
// a socket-close of the epoch read exempts the epoch, also after the window's end;
// a recorded close_seq not read, a clean stop of another epoch and a same-key primary do not,
// although the last two bound the window;
// a conflicting socket-close does not, while its seq has another version that is not one (a conflict, not a contradiction);
// and a primary in epoch 0 is barred, or exempt, the same way.
// A boundary whose gap meets the hour read is both an epoch barrier and a time barrier.
func TestFindTransactionEpochBarrier(t *testing.T) {
	t.Parallel()

	past := new(blockTestHour - 2*hourNs)
	tests := []struct {
		name  string
		recs  []Record
		other []Record
		// zero puts the primary and every record of its epoch in epoch 0.
		zero bool
		// meets gives the boundary a gap that meets the hour read.
		meets   bool
		closure uint64
		end     uint64
		outcome TxOutcome
		gaps    []string
	}{
		{name: "the epoch open", recs: []Record{txNote(13, nil), txRecord(14, nil)}, end: 14, outcome: TxIncomplete, gaps: []string{"barrier"}},
		{name: "a socket-close of the epoch read", recs: []Record{txNote(13, nil), txSocketClose(t, 14)}, end: 14, outcome: TxUnmatched},
		{name: "a socket-close of the epoch after the window", recs: []Record{txNote(13, nil), txRecord(14, nil), txSocketClose(t, 16)},
			end: 14, outcome: TxUnmatched},
		{name: "a close_seq not read", recs: []Record{txNote(13, nil), txRecord(14, nil)}, closure: 50, end: 14, outcome: TxIncomplete,
			gaps: []string{"barrier"}},
		{name: "a clean stop of another epoch", recs: []Record{txNote(13, nil), txBoundaryAt(t, 14, 2, BoundaryKindStop, nil, nil)},
			end: 14, outcome: TxIncomplete, gaps: []string{"barrier"}},
		{name: "a conflicting socket-close", recs: []Record{txNote(13, nil), txSocketClose(t, 14), txNote(15, nil), txRecord(16, nil)},
			other: []Record{txNote(14, nil)}, end: 16, outcome: TxIncomplete, gaps: []string{"conflict@14", "barrier"}},
		{name: "a primary in epoch 0", zero: true, recs: []Record{txNote(13, nil), txRecord(14, nil)}, end: 14, outcome: TxIncomplete,
			gaps: []string{"correlation@12", "barrier"}},
		{name: "a socket-close of epoch 0 read", zero: true, recs: []Record{txNote(13, nil), txSocketClose(t, 14)}, end: 14,
			outcome: TxIncomplete, gaps: []string{"correlation@12"}},
		{name: "a gap that meets the hour read", meets: true, recs: []Record{txNote(13, nil), txRecord(14, nil)}, end: 14,
			outcome: TxIncomplete, gaps: []string{"barrier", "barrier"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			epoch := uint32(txTestEpoch)
			if tt.zero {
				epoch = 0
			}
			recs := append([]Record{txRecord(12, nil)}, tt.recs...)
			for i := range recs {
				if recs[i].Epoch == txTestEpoch {
					recs[i].Epoch = epoch
				}
			}
			files := [][]byte{txPack(t, seg0, nil, txBlock(recs...))}
			if tt.other != nil {
				files = append(files, txPack(t, seg1, nil, txBlock(tt.other...)))
			}
			s := txSource(t, true, files...)
			b := txUnclean(captureLow, 100, 2, past, past)
			if tt.meets {
				b.GapEnd = new(blockTestHour)
			}
			s.addBoundary(b)
			if tt.closure != 0 {
				s.addClosure(captureLow, epoch, tt.closure)
			}
			res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 1})
			require.NoError(t, err)
			assert.Equal(t, tt.outcome, res.Outcome)
			assert.Equal(t, new(tt.end), res.WindowEnd)
			assert.Equal(t, tt.gaps, nilIfEmpty(gapSeqs(res.Gaps)))
			for _, g := range res.Gaps {
				if g.Reason == TxGapBarrier {
					assert.Equal(t, TxGap{Reason: TxGapBarrier, Block: -1, Offset: -1, Barrier: &b, Err: g.Err}, g)
					require.Error(t, g.Err)
				}
			}
		})
	}
}

// TestFindTransactionTimeBarriers looks up the primary at 12 beside a stop-unclean boundary of another capture of the tool:
// it is a time barrier when its gap interval meets the time range of the hours read, [from, to),
// gap_end inclusive, an absent bound unbounded, an inverted interval meeting every range;
// a gap in the next hour meets the range only when that hour is read.
func TestFindTransactionTimeBarriers(t *testing.T) {
	t.Parallel()

	from, to := blockTestHour, blockTestHour+hourNs
	tests := []struct {
		name       string
		start, end *int64
		scopes     int
		barrier    bool
	}{
		{name: "inside the range", start: new(from + 10), end: new(from + 20), barrier: true},
		{name: "ending at the range's start", start: new(from - 100), end: new(from), barrier: true},
		{name: "ending before the range", start: new(from - 100), end: new(from - 1)},
		{name: "starting at the range's end", start: new(to), end: new(to + 100)},
		{name: "starting before the range's end", start: new(to - 1), end: new(to + 100), barrier: true},
		{name: "without an end", start: new(from - 1), barrier: true},
		{name: "without a start, ending before", end: new(from - 1)},
		{name: "without a start, ending at the range's start", end: new(from), barrier: true},
		{name: "without bounds", barrier: true},
		{name: "inverted, outside the range", start: new(to + 100), end: new(from - 100), barrier: true},
		{name: "in the next hour, one hour read", start: new(to + 10), end: new(to + 20)},
		{name: "in the next hour, two hours read", start: new(to + 10), end: new(to + 20), scopes: 2, barrier: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := txHours(t, [][]byte{txPack(t, seg0, nil, txBlock(txRecord(12, nil), txNote(13, nil), txRecord(14, nil)))}, nil)
			b := txUnclean(captureHigh, 7, 1, tt.start, tt.end)
			s.addBoundary(b)
			res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: max(tt.scopes, 1)})
			require.NoError(t, err)
			if !tt.barrier {
				assert.Equal(t, TxUnmatched, res.Outcome)
				assert.Empty(t, res.Gaps)

				return
			}
			assert.Equal(t, TxIncomplete, res.Outcome)
			require.Len(t, res.Gaps, 1)
			assert.Equal(t, TxGap{Reason: TxGapBarrier, Block: -1, Offset: -1, Barrier: &b, Err: res.Gaps[0].Err}, res.Gaps[0])
			require.ErrorContains(t, res.Gaps[0].Err, "capture "+captureHigh.String())
		})
	}
}

// TestFindTransactionBarriersChecked looks up the primary at 12 over an observation whose Barriers also returns
// a stop-unclean boundary whose gap does not meet the hour read and a gap boundary whose interval does:
// neither is a barrier.
// It then changes the barriers and the evidence the observation returned:
// the boundaries the result holds are copies.
func TestFindTransactionBarriersChecked(t *testing.T) {
	t.Parallel()

	s := &txSpySource{memSource: txSource(t, true, txPack(t, seg0, nil, txBlock(txRecord(12, nil), txNote(13, nil), txRecord(14, nil))))}
	s.extra = []Boundary{
		txUnclean(captureHigh, 7, 1, new(blockTestHour+hourNs), nil),
		{Capture: captureHigh, Seq: 8, Kind: BoundaryKindGap, GapStart: new(blockTestHour), GapEnd: new(blockTestHour + 1)},
	}
	res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 1})
	require.NoError(t, err)
	assert.Equal(t, TxUnmatched, res.Outcome)
	assert.Empty(t, res.Gaps)

	// A barrier of another capture and a stop-unclean of the primary's:
	// a time barrier, an epoch barrier and a capture-boundary.
	s = &txSpySource{memSource: txSource(t, true, txPack(t, seg0, nil, txBlock(txRecord(12, nil), txNote(13, nil), txRecord(14, nil))))}
	time := txUnclean(captureHigh, 7, 1, new(blockTestHour), nil)
	own := txUnclean(captureLow, 20, txTestEpoch, new(blockTestHour-hourNs), new(blockTestHour-hourNs))
	s.addBoundary(time)
	s.addBoundary(own)
	res, err = findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 1})
	require.NoError(t, err)
	require.Equal(t, []string{"barrier", "barrier", "capture-boundary@20"}, gapSeqs(res.Gaps))
	*s.obs.evidence.Boundaries[0].GapStart = 1
	s.obs.evidence.Boundaries[0].Seq = 1
	*s.obs.barriers[0].GapStart = 1
	assert.Equal(t, &own, res.Gaps[0].Barrier)
	assert.Equal(t, &time, res.Gaps[1].Barrier)
	assert.Equal(t, &own, res.Gaps[2].Barrier)
}

// TestFindTransactionBarriersError fails the observation's Barriers:
// the lookup fails with its error, wrapped, holding the result so far without an outcome, the observation closed once.
func TestFindTransactionBarriersError(t *testing.T) {
	t.Parallel()

	s := txSource(t, true, txPack(t, seg0, nil, txBlock(txRecord(12, nil), txReply(13, nil), txRecord(14, nil))))
	boom := errors.New("boom")
	s.beforeBarriers = func(context.Context, int64, int64) error { return boom }
	res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 1})
	require.ErrorIs(t, err, boom)
	require.ErrorContains(t, err, "tracepack: find transaction: barriers: boom")
	assert.Equal(t, TxOutcome(0), res.Outcome)
	assert.Equal(t, []string{"12@0 primary", "13@0 candidate in eligible decidable valid", "14@0 same-key-primary bound"}, recordFlags(res.Records))
	assert.Len(t, res.Searched, 1)
	_, _, closes := s.counts()
	assert.Equal(t, 1, closes)
}

// TestFindTransactionCaptureBoundaries looks up the primary at 12 in epoch 1 beside capture-boundaries
// (the tracepack semantics specification §7.2):
// each one of the primary's epoch, in the evidence anywhere in the epoch or read above the primary,
// is a TxGapCaptureBoundary gap,
// a boundary both recorded and read listed once,
// except the clean stop of the epoch that bounds the window;
// a stop of the epoch after the window's end is one,
// and so is a recorded stop at the window's end that a same-key primary or a clean stop of another epoch bounds,
// whose claim the record contradicts;
// a recorded stop at the window's end that the clean stop of the epoch bounds is none;
// a boundary of another epoch is none.
func TestFindTransactionCaptureBoundaries(t *testing.T) {
	t.Parallel()

	stop := func(seq uint64) Record { return txBoundaryAt(t, seq, txTestEpoch, BoundaryKindStop, nil, nil) }
	gap := func(seq uint64) Record { return txBoundaryAt(t, seq, txTestEpoch, BoundaryKindGap, nil, nil) }
	tests := []struct {
		name string
		recs []Record
		// recorded is added to the evidence by hand.
		recorded *Boundary
		end      uint64
		outcome  TxOutcome
		gaps     []string
	}{
		{name: "a clean stop that bounds the window", recs: []Record{txNote(13, nil), stop(14)}, end: 14, outcome: TxUnmatched},
		{name: "beside a gap boundary", recs: []Record{gap(13), stop(14)}, end: 14, outcome: TxIncomplete,
			gaps: []string{"capture-boundary@13"}},
		{name: "a start below the primary", recs: []Record{txNote(13, nil), stop(14)},
			recorded: &Boundary{Capture: captureLow, Seq: 3, Kind: BoundaryKindStart, Epoch: txTestEpoch},
			end:      14, outcome: TxIncomplete, gaps: []string{"capture-boundary@3"}},
		{name: "a gap outside the hours read", recs: []Record{txNote(13, nil), stop(14)},
			recorded: &Boundary{Capture: captureLow, Seq: 500, Kind: BoundaryKindGap, Epoch: txTestEpoch},
			end:      14, outcome: TxIncomplete, gaps: []string{"capture-boundary@500"}},
		{name: "a gap of another epoch", recs: []Record{txNote(13, nil), stop(14)},
			recorded: &Boundary{Capture: captureLow, Seq: 500, Kind: BoundaryKindGap, Epoch: 2}, end: 14, outcome: TxUnmatched},
		{name: "a stop after the window's end", recs: []Record{txNote(13, nil), txRecord(14, nil), stop(16)}, end: 14,
			outcome: TxIncomplete, gaps: []string{"capture-boundary@16"}},
		{name: "a recorded stop where a same-key primary bounds", recs: []Record{txNote(13, nil), txRecord(14, nil)},
			recorded: &Boundary{Capture: captureLow, Seq: 14, Kind: BoundaryKindStop, Epoch: txTestEpoch},
			end:      14, outcome: TxIncomplete, gaps: []string{"capture-boundary@14", "contradiction@14"}},
		{name: "a recorded stop where a clean stop of another epoch bounds",
			recs:     []Record{txNote(13, nil), txBoundaryAt(t, 14, 2, BoundaryKindStop, nil, nil)},
			recorded: &Boundary{Capture: captureLow, Seq: 14, Kind: BoundaryKindStop, Epoch: txTestEpoch},
			end:      14, outcome: TxIncomplete, gaps: []string{"capture-boundary@14", "contradiction@14"}},
		{name: "a recorded stop where the clean stop of the epoch bounds", recs: []Record{txNote(13, nil), stop(14)},
			recorded: &Boundary{Capture: captureLow, Seq: 14, Kind: BoundaryKindStop, Epoch: txTestEpoch},
			end:      14, outcome: TxUnmatched},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := txSource(t, true, txPack(t, seg0, nil, txBlock(append([]Record{txRecord(12, nil)}, tt.recs...)...)))
			if tt.recorded != nil {
				s.addBoundary(*tt.recorded)
			}
			res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 1})
			require.NoError(t, err)
			assert.Equal(t, tt.outcome, res.Outcome)
			assert.Equal(t, new(tt.end), res.WindowEnd)
			assert.Equal(t, tt.gaps, nilIfEmpty(gapSeqs(res.Gaps)))
			for _, g := range res.Gaps {
				if g.Reason == TxGapCaptureBoundary {
					// Recorded by the evidence: no hour, pack or block.
					assert.Equal(t, TxGap{Reason: TxGapCaptureBoundary, Block: -1, Offset: -1, Seq: g.Seq, Barrier: g.Barrier, Err: g.Err}, g)
					require.NotNil(t, g.Barrier)
					assert.Equal(t, *g.Seq, g.Barrier.Seq)
					assert.Equal(t, uint32(txTestEpoch), g.Barrier.Epoch)
				}
			}
		})
	}
}

// TestFindTransactionBoundaryRecordsRead reads, in a scope the catalog does not index,
// a staged segment that holds a gap boundary
// of the primary's epoch that the evidence does not record, and a clean stop that bounds the window:
// the record read is a TxGapCaptureBoundary gap that names its hour, pack and block, beside the scope's TxGapCold gap;
// the stop is none.
func TestFindTransactionBoundaryRecordsRead(t *testing.T) {
	t.Parallel()

	s := newMemSource()
	s.stagePack(t, memTestHour, txPack(t, seg0, nil, slices.Concat(
		txBlock(txRecord(12, nil), txNote(13, nil)),
		txBlock(txBoundaryAt(t, 14, txTestEpoch, BoundaryKindGap, new(int64(5)), new(int64(9))), txBoundaryAt(t, 15, txTestEpoch, BoundaryKindStop, nil, nil)),
	)))
	res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 1})
	require.NoError(t, err)
	assert.Equal(t, TxIncomplete, res.Outcome)
	assert.Equal(t, new(uint64(15)), res.WindowEnd)
	require.Equal(t, []string{"cold", "capture-boundary@14"}, gapSeqs(res.Gaps))
	g := res.Gaps[1]
	assert.Equal(t, TxGap{
		Reason: TxGapCaptureBoundary, Hours: []int64{memTestHour}, Pack: new(seg0), Block: 1, Offset: -1, Seq: new(uint64(14)),
		Barrier: &Boundary{
			Capture: captureLow, Seq: 14, Kind: BoundaryKindGap, TS: blockTestHour + 14, Epoch: txTestEpoch,
			GapStart: new(int64(5)), GapEnd: new(int64(9)),
		},
		Err: g.Err,
	}, g)
	require.Error(t, g.Err)
}

// TestFindTransactionContradictions checks the per-capture evidence's closures against the records the lookup read
// (the tracepack semantics specification §7.2):
// a close_seq of the primary's epoch that names an annotation, a transport event that closes nothing,
// a socket-close or a clean stop of another epoch,
// or a stop claimed where a socket-close or a stop of another epoch is,
// recorded by hand as a structurally valid footer would have it, is a contradiction and never bounds the window,
// also when the pack whose footer recorded it is gone from the view;
// a close_seq or a clean stop at or below the primary is one too, and a stop-unclean beside it still bars the epoch;
// a close_seq that names a seq with two versions, one of them a socket-close of the epoch, is the conflict only;
// a close_seq at a seq the lookup did not read is neither used nor checked, inside the window or past the hours read.
func TestFindTransactionContradictions(t *testing.T) {
	t.Parallel()

	connect := testEventRecord(t, 14, blockTestHour+14, txTestEpoch, &TransportEvent{Event: EventSocketConnect})
	otherClose := testEventRecord(t, 14, blockTestHour+14, 2, &TransportEvent{Event: EventSocketClose})
	tests := []struct {
		name string
		recs []Record
		// other is a second pack of the scope; removed is a pack whose footer's evidence is recorded, then removed.
		other, removed []Record
		closure        uint64
		recorded       *Boundary
		unclean        bool
		end            *uint64
		outcome        TxOutcome
		gaps           []string
	}{
		{name: "a close_seq at an annotation", recs: []Record{txNote(13, nil), txNote(14, nil), txRecord(15, nil)}, closure: 14,
			end: new(uint64(15)), outcome: TxIncomplete, gaps: []string{"contradiction@14"}},
		{name: "a close_seq at a transport event that closes nothing", recs: []Record{txNote(13, nil), connect, txRecord(15, nil)},
			closure: 14, end: new(uint64(15)), outcome: TxIncomplete, gaps: []string{"contradiction@14"}},
		{name: "a close_seq at a socket-close of another epoch", recs: []Record{txNote(13, nil), otherClose, txRecord(15, nil)},
			closure: 14, end: new(uint64(15)), outcome: TxIncomplete, gaps: []string{"contradiction@14"}},
		{name: "a close_seq at a clean stop of another epoch",
			recs:    []Record{txNote(13, nil), txBoundaryAt(t, 14, 2, BoundaryKindStop, nil, nil)},
			closure: 14, end: new(uint64(14)), outcome: TxIncomplete, gaps: []string{"contradiction@14"}},
		{name: "a stop of the epoch claimed at a clean stop of another epoch",
			recs:     []Record{txNote(13, nil), txBoundaryAt(t, 14, 2, BoundaryKindStop, nil, nil)},
			recorded: &Boundary{Capture: captureLow, Seq: 14, Kind: BoundaryKindStop, Epoch: txTestEpoch},
			end:      new(uint64(14)), outcome: TxIncomplete, gaps: []string{"capture-boundary@14", "contradiction@14"}},
		{name: "a stop claimed at a socket-close", recs: []Record{txNote(13, nil), txSocketClose(t, 14)},
			recorded: &Boundary{Capture: captureLow, Seq: 14, Kind: BoundaryKindStop, Epoch: txTestEpoch},
			end:      new(uint64(14)), outcome: TxIncomplete, gaps: []string{"capture-boundary@14", "contradiction@14"}},
		{name: "a close_seq whose pack is gone", recs: []Record{txNote(13, nil), txNote(14, nil), txRecord(15, nil)},
			removed: []Record{txSocketClose(t, 14)}, end: new(uint64(15)), outcome: TxIncomplete, gaps: []string{"contradiction@14"}},
		{name: "a close_seq at the primary", recs: []Record{txNote(13, nil), txRecord(14, nil)}, closure: 12,
			end: new(uint64(14)), outcome: TxIncomplete, gaps: []string{"contradiction@12"}},
		{name: "a socket-close below the primary", recs: []Record{txNote(13, nil), txRecord(14, nil)},
			removed: []Record{txSocketClose(t, 10)}, end: new(uint64(14)), outcome: TxIncomplete, gaps: []string{"contradiction@10"}},
		{name: "a socket-close below the primary, the capture unclean", recs: []Record{txNote(13, nil), txRecord(14, nil)},
			removed: []Record{txSocketClose(t, 10)}, unclean: true, end: new(uint64(14)), outcome: TxIncomplete,
			gaps: []string{"barrier", "contradiction@10"}},
		{name: "a clean stop of another epoch below the primary", recs: []Record{txNote(13, nil), txRecord(14, nil)},
			recorded: &Boundary{Capture: captureLow, Seq: 9, Kind: BoundaryKindStop, Epoch: 2},
			end:      new(uint64(14)), outcome: TxIncomplete, gaps: []string{"contradiction@9"}},
		{name: "a close_seq at two versions, one a socket-close",
			recs:  []Record{txNote(13, nil), txSocketClose(t, 14), txNote(15, nil), txRecord(16, nil)},
			other: []Record{txNote(14, nil)}, end: new(uint64(16)), outcome: TxIncomplete, gaps: []string{"conflict@14"}},
		{name: "a stop claimed at two versions, one a stop",
			recs:  []Record{txNote(13, nil), txBoundaryAt(t, 14, 2, BoundaryKindStop, nil, nil), txNote(15, nil), txRecord(16, nil)},
			other: []Record{txNote(14, nil)}, end: new(uint64(16)), outcome: TxIncomplete, gaps: []string{"conflict@14"}},
		{name: "a close_seq at a seq not read", recs: []Record{txNote(13, nil), txRecord(15, nil)}, closure: 14,
			end: new(uint64(15)), outcome: TxIncomplete, gaps: []string{"seq-gap@14"}},
		{name: "a close_seq past the hours read", recs: []Record{txNote(13, nil)}, closure: 500,
			outcome: TxIncomplete, gaps: []string{"open-window"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			files := [][]byte{txPack(t, seg0, nil, txBlock(append([]Record{txRecord(12, nil)}, tt.recs...)...))}
			if tt.other != nil {
				files = append(files, txPack(t, seg1, nil, txBlock(tt.other...)))
			}
			s := txSource(t, true, files...)
			if tt.removed != nil {
				s.addPack(t, memTestHour, txPack(t, seg2, nil, txBlock(tt.removed...)))
				s.removePack(captureLow, memTestHour, seg2)
			}
			if tt.closure != 0 {
				s.addClosure(captureLow, txTestEpoch, tt.closure)
			}
			if tt.recorded != nil {
				s.addBoundary(*tt.recorded)
			}
			if tt.unclean {
				s.addBoundary(txUnclean(captureLow, 100, 2, new(blockTestHour-hourNs), new(blockTestHour-hourNs)))
			}
			res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 1})
			require.NoError(t, err)
			assert.Equal(t, tt.outcome, res.Outcome)
			assert.Equal(t, tt.end, res.WindowEnd)
			assert.Equal(t, tt.gaps, gapSeqs(res.Gaps))
			for _, g := range res.Gaps {
				if g.Reason == TxGapContradiction {
					assert.Equal(t, TxGap{Reason: TxGapContradiction, Block: -1, Offset: -1, Seq: g.Seq, Err: g.Err}, g)
					require.Error(t, g.Err)
				}
			}
		})
	}
}

// edited returns r changed by edit.
func edited(r Record, edit func(r *Record)) Record {
	edit(&r)

	return r
}

// TestFindTransactionOrderingUncertain reads a record of the primary's epoch with ordering-uncertain
// below the primary, at it, after the window's end, and in a pack whose footer is not used, read by walking:
// each is a TxGapOrderingUncertain gap at the first such record read,
// the last beside the partial evidence its registration left;
// one of another epoch is none.
func TestFindTransactionOrderingUncertain(t *testing.T) {
	t.Parallel()

	uncertain := func(epoch uint32) func(r *Record) {
		return func(r *Record) {
			r.Epoch = epoch
			r.Quality |= QualityOrderingUncertain
		}
	}
	tests := []struct {
		name    string
		recs    []Record
		walked  bool
		outcome TxOutcome
		gaps    []string
	}{
		{name: "below the primary", recs: []Record{txRecord(11, uncertain(txTestEpoch)), txRecord(12, nil), txNote(13, nil), txRecord(14, nil)},
			outcome: TxIncomplete, gaps: []string{"ordering-uncertain@11"}},
		{name: "at the primary", recs: []Record{txRecord(12, uncertain(txTestEpoch)), txNote(13, nil), txRecord(14, nil)},
			outcome: TxIncomplete, gaps: []string{"ordering-uncertain@12"}},
		{name: "after the window", recs: []Record{txRecord(12, nil), txNote(13, nil), txRecord(14, nil), txReply(20, uncertain(txTestEpoch))},
			outcome: TxIncomplete, gaps: []string{"ordering-uncertain@20"}},
		{name: "in another epoch", recs: []Record{txRecord(11, uncertain(2)), txRecord(12, nil), txRecord(13, uncertain(2)), txRecord(14, nil)},
			outcome: TxUnmatched},
		{name: "in a walked pack", recs: []Record{txRecord(12, nil), edited(txNote(13, nil), uncertain(txTestEpoch)), txRecord(14, nil)}, walked: true,
			outcome: TxIncomplete, gaps: []string{"ordering-uncertain@13", "evidence"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			file := txPack(t, seg0, nil, txBlock(tt.recs...))
			if tt.walked {
				file = invalidFooterFile(t, file)
			}
			res, err := findTx(t, t.Context(), txSource(t, true, file), txKeyAt(12), TxOptions{MaxScopes: 1})
			require.NoError(t, err)
			assert.Equal(t, tt.outcome, res.Outcome)
			assert.Equal(t, new(uint64(14)), res.WindowEnd)
			assert.Equal(t, tt.gaps, nilIfEmpty(gapSeqs(res.Gaps)))
			if len(res.Gaps) > 0 {
				g := res.Gaps[0]
				assert.Equal(t, TxGap{
					Reason: TxGapOrderingUncertain, Hours: []int64{memTestHour}, Pack: new(seg0), Block: 0, Offset: -1, Seq: g.Seq, Err: g.Err,
				}, g)
				require.Error(t, g.Err)
			}
		})
	}
}

// TestFindTransactionPartialEvidence looks up a primary whose capture's per-capture evidence its source marks partial:
// a lookup that would be TxUnmatched is TxIncomplete, a match stays TxMatched, each with a TxGapEvidence gap beside it.
func TestFindTransactionPartialEvidence(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		reply   Record
		outcome TxOutcome
	}{{reply: txNote(13, nil), outcome: TxIncomplete}, {reply: txReply(13, nil), outcome: TxMatched}} {
		s := txSource(t, true, txPack(t, seg0, nil, txBlock(txRecord(12, nil), tt.reply, txRecord(14, nil))))
		s.setPartial(captureLow)
		res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 1})
		require.NoError(t, err)
		assert.Equal(t, tt.outcome, res.Outcome)
		require.Len(t, res.Gaps, 1)
		assert.Equal(t, TxGap{Reason: TxGapEvidence, Block: -1, Offset: -1, Err: res.Gaps[0].Err}, res.Gaps[0])
		require.Error(t, res.Gaps[0].Err)
	}
}

// TestFindTransactionCompletenessGapOrder looks up a primary
// beside one gap of each kind that the records' flags do not decide:
// a coverage entry, a stop-unclean boundary of the capture in another epoch whose gap meets the hour read,
// a gap boundary of the epoch, an ordering-uncertain record, partial evidence and a close_seq at an annotation.
// They follow the window's gaps in this order: coverage, the epoch barrier, the time barrier, the capture-boundary,
// ordering-uncertain, evidence, contradiction.
func TestFindTransactionCompletenessGapOrder(t *testing.T) {
	t.Parallel()

	cov := Coverage{SeqFirst: new(uint64(13)), SeqLast: new(uint64(13))}
	s := txSource(t, true, txPack(t, seg0, func(m *PackMeta) { m.Coverage = []Coverage{cov} }, txBlock(
		txRecord(11, func(r *Record) { r.Quality |= QualityOrderingUncertain }), txRecord(12, nil), txNote(13, nil),
		txBoundaryAt(t, 14, txTestEpoch, BoundaryKindGap, nil, nil), txRecord(16, nil),
	)))
	unclean := txUnclean(captureLow, 100, 2, new(blockTestHour+5), new(blockTestHour+6))
	s.addBoundary(unclean)
	s.addClosure(captureLow, txTestEpoch, 13)
	s.setPartial(captureLow)
	res, l, err := findTxState(t, s, TxOptions{MaxScopes: 1})
	require.NoError(t, err)
	assert.NotSame(t, l.coverage[0].cov, res.Gaps[1].Coverage, "the gap holds a copy")
	assert.Equal(t, TxIncomplete, res.Outcome)
	assert.Equal(t, []string{
		"seq-gap@15", "coverage", "barrier", "barrier", "capture-boundary@14", "ordering-uncertain@11", "evidence", "contradiction@13",
	}, gapSeqs(res.Gaps))
	assert.Equal(t, &cov, res.Gaps[1].Coverage)
	assert.Equal(t, &unclean, res.Gaps[2].Barrier)
	assert.Equal(t, &unclean, res.Gaps[3].Barrier)
	assert.Equal(t, &Boundary{Capture: captureLow, Seq: 14, Kind: BoundaryKindGap, TS: blockTestHour + 14, Epoch: txTestEpoch}, res.Gaps[4].Barrier)
}

// TestFindTransactionEndEvidence runs the end-evidence vectors of the tracepack storage specification §8 through a lookup
// of the primary at 12 in hour H.
//
// A cold recovered boundary: the capture's clock stepped back to an hour outside the hours read,
// where recovery's stop-unclean segment, whose gap covers H, was rejected and its evidence recorded:
// the epoch barrier, the time barrier and the capture-boundary keep the lookup TxIncomplete.
//
// A clean end evicted: the stop that ends the capture lies in hour H + 1, evicted after its registration;
// a lookup whose window reaches it reads it through the listing view and is TxIncomplete with a TxGapCold gap only,
// TxUnmatched while H + 1 is indexed.
//
// A bounded unclean end evicted: hour H + 1, evicted, holds the socket-close of the primary's epoch and the stop-unclean
// whose gap lies in hour H + 2.
// A lookup that reads H + 1 reads the closing record, so the epoch is exempt from the barrier,
// and is TxIncomplete with TxGapCold;
// one that reads H alone is barred by the epoch although the evidence records the closure;
// one that reads three hours also meets the gap, a time barrier.
//
// An entirely cold capture: another capture, none of whose scopes is admitted, recorded a stop-unclean whose gap meets H,
// which holds only the primary's capture: a time barrier.
func TestFindTransactionEndEvidence(t *testing.T) {
	t.Parallel()

	h := blockTestHour
	t.Run("a cold recovered boundary", func(t *testing.T) {
		t.Parallel()

		s := txHours(t, [][]byte{txPack(t, seg0, nil, txBlock(txRecord(12, nil), txNote(13, nil), txRecord(14, nil)))})
		recovered := txBoundaryAt(t, 20, txTestEpoch, BoundaryKindStopUnclean, new(h-10), new(h+hourNs))
		s.registerEvidence(t, txPackIn(t, seg1, -5, nil, txBlock(recovered)))
		res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 1})
		require.NoError(t, err)
		assert.Equal(t, TxIncomplete, res.Outcome)
		assert.Equal(t, []string{"barrier", "barrier", "capture-boundary@20"}, gapSeqs(res.Gaps))
	})

	t.Run("a clean end evicted", func(t *testing.T) {
		t.Parallel()

		for _, evicted := range []bool{false, true} {
			s := txHours(t,
				[][]byte{txPack(t, seg0, nil, txBlock(txRecord(12, nil), txNote(13, nil)))},
				[][]byte{txPackIn(t, seg1, 1, nil, txBlock(txBoundaryAt(t, 14, txTestEpoch, BoundaryKindStop, nil, nil)))})
			s.setIndexed(captureLow, memTestHour+1, !evicted)
			res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 2})
			require.NoError(t, err)
			assert.Equal(t, new(uint64(14)), res.WindowEnd)
			if !evicted {
				assert.Equal(t, TxUnmatched, res.Outcome)
				assert.Empty(t, res.Gaps)

				continue
			}
			assert.Equal(t, TxIncomplete, res.Outcome)
			assert.Equal(t, []string{"cold"}, gapSeqs(res.Gaps))
		}
	})

	t.Run("a bounded unclean end evicted", func(t *testing.T) {
		t.Parallel()

		gapStart, gapEnd := h+2*hourNs+10, h+2*hourNs+20
		for _, tt := range []struct {
			scopes int
			gaps   []string
		}{
			{scopes: 1, gaps: []string{"open-window", "barrier", "capture-boundary@15"}},
			{scopes: 2, gaps: []string{"cold", "capture-boundary@15"}},
			{scopes: 3, gaps: []string{"cold", "barrier", "capture-boundary@15"}},
		} {
			s := txHours(t,
				[][]byte{txPack(t, seg0, nil, txBlock(txRecord(12, nil), txNote(13, nil)))},
				[][]byte{txPackIn(t, seg1, 1, nil, txBlock(
					txSocketClose(t, 14), txBoundaryAt(t, 15, txTestEpoch, BoundaryKindStopUnclean, &gapStart, &gapEnd)))},
				nil)
			s.setIndexed(captureLow, memTestHour+1, false)
			res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: tt.scopes})
			require.NoError(t, err)
			assert.Equal(t, TxIncomplete, res.Outcome, "%d scopes", tt.scopes)
			assert.Equal(t, tt.gaps, gapSeqs(res.Gaps), "%d scopes", tt.scopes)
		}
	})

	t.Run("an entirely cold capture", func(t *testing.T) {
		t.Parallel()

		s := txHours(t, [][]byte{txPack(t, seg0, nil, txBlock(txRecord(12, nil), txNote(13, nil), txRecord(14, nil)))})
		other := txBoundaryAt(t, 30, 1, BoundaryKindStopUnclean, new(h+100), new(h+200))
		s.registerEvidence(t, withIDs(t, writeRepairPack(t, CodecZstd, nil, false, txBlock(other)).file, seg3, captureHigh))
		res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 1})
		require.NoError(t, err)
		assert.Equal(t, TxIncomplete, res.Outcome)
		require.Equal(t, []string{"barrier"}, gapSeqs(res.Gaps))
		assert.Equal(t, captureHigh, res.Gaps[0].Barrier.Capture)
	})
}

// TestFindTransactionFixedObservation changes the source during Observe and before each Scope call:
// the scope evicted, a rejected segment holding a valid reply staged, a closure at an annotation,
// a capture-boundary of the epoch
// and a stop-unclean whose gap meets the hour recorded, and the evidence marked partial.
// The lookup answers from what Observe fixed: TxUnmatched, without a gap.
func TestFindTransactionFixedObservation(t *testing.T) {
	t.Parallel()

	s := txSource(t, true, txPack(t, seg0, nil, txBlock(txRecord(12, nil), txNote(13, nil), txRecord(14, nil))))
	late := txPack(t, seg1, nil, txBlock(txReply(13, nil)))
	change := func() {
		s.setIndexed(captureLow, memTestHour, false)
		s.stagePack(t, memTestHour, late)
		s.addClosure(captureLow, txTestEpoch, 13)
		s.addBoundary(Boundary{Capture: captureLow, Seq: 13, Kind: BoundaryKindGap, Epoch: txTestEpoch})
		s.addBoundary(txUnclean(captureLow, 100, txTestEpoch, new(blockTestHour), nil))
		s.setPartial(captureLow)
	}
	s.duringObserve = change
	s.beforeScope = func(context.Context, int64) error {
		change()
		return nil
	}
	res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 1})
	require.NoError(t, err)
	assert.Equal(t, TxUnmatched, res.Outcome)
	assert.Empty(t, res.Gaps)
	assert.Equal(t, []string{"12@0 primary", "14@0 same-key-primary"}, recordSeqs(res.Records))
}

// TestFindTransactionClaimBeforePrimary reads, in the primary's scope, a block whose F-2 entry under-reports its last seq,
// so that its record at 20 arrives before the primary at 15 and is neither classified nor kept,
// beside a close_seq of the primary's epoch at 20 and a stop of the epoch claimed at 17, an annotation read after the primary:
// the claim at 20 is not checked, the order of that read being broken, while the one at 17 is still a contradiction.
func TestFindTransactionClaimBeforePrimary(t *testing.T) {
	t.Parallel()

	s := txSource(t, true, txMisindexed(t, 20), txPack(t, seg1, nil, txBlock(txRecord(15, nil), txNote(17, nil))))
	s.addClosure(captureLow, txTestEpoch, 20)
	s.addBoundary(Boundary{Capture: captureLow, Seq: 17, Kind: BoundaryKindStop, Epoch: txTestEpoch})
	res, err := findTx(t, t.Context(), s, txKeyAt(15), TxOptions{MaxScopes: 1})
	require.NoError(t, err)
	assert.Equal(t, TxIncomplete, res.Outcome)
	assert.Equal(t, []string{"index", "index@15", "open-window", "capture-boundary@17", "contradiction@17"}, gapSeqs(res.Gaps))
	assert.Equal(t, []string{"15@0 primary"}, recordSeqs(res.Records))
}
