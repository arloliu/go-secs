package corpus

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
)

// hoursOf returns hs as fact hours.
func hoursOf(hs ...int64) []I64 {
	out := make([]I64, 0, len(hs))
	for _, h := range hs {
		out = append(out, I64(h))
	}

	return out
}

// capBoundary returns the boundary of a capture-boundary fact.
func capBoundary(capture tracepack.UUID, seq U64, kind string, epoch uint32) *FactBoundary {
	return &FactBoundary{CaptureID: capture.String(), Seq: seq, BoundaryKind: kind, Epoch: epoch}
}

// barrierFact returns a barrier fact of a stop-unclean boundary at seq 3 of capture, edited by edit, with basis.
func barrierFact(capture tracepack.UUID, edit func(*FactBoundary), basis ...string) Fact {
	b := &FactBoundary{CaptureID: capture.String(), Seq: 3, BoundaryKind: "stop-unclean", TS: new(I64(-9)), Epoch: 1}
	if edit != nil {
		edit(b)
	}

	return Fact{Reason: FactBarrier, Boundary: b, Basis: basis}
}

// coverageFact returns a coverage fact of pack 1 in hour 10 whose coverage object edit sets.
func coverageFact(edit func(*Coverage)) Fact {
	var c Coverage
	edit(&c)

	return Fact{Reason: FactCoverage, Hours: hoursOf(10), Pack: new(1), Coverage: &c}
}

// TestFactComparator checks, for every key of the comparator of the tracepack corpus specification §5.11,
// pairs of facts tied on every other key: each pair sorts to one array from both input orders,
// and identical facts collapse.
func TestFactComparator(t *testing.T) {
	t.Parallel()

	read := func(block *int, offset *U64, defect string) Fact {
		return Fact{Reason: FactRead, Hours: hoursOf(10), Pack: new(0), Block: block, Offset: offset, Defect: defect}
	}
	pairs := []struct {
		name   string
		lo, hi Fact
	}{
		{"reason by the table, not by name", Fact{Reason: FactScopeBreach, Hours: hoursOf(1), Pack: new(0), Block: new(0), Seq: new(U64(1))},
			Fact{Reason: FactCold, Hours: hoursOf(1)}},
		{"reason unavailable before contradiction", Fact{Reason: FactUnavailable, Hours: hoursOf(1), Pack: new(0), Block: new(0), Seq: new(U64(1))},
			Fact{Reason: FactContradiction, Seq: new(U64(1))}},
		{"reason no-key first", Fact{Reason: FactNoKey}, Fact{Reason: FactConflict, Hours: hoursOf(1), Seq: new(U64(1))}},
		{"hours as numbers", Fact{Reason: FactCold, Hours: hoursOf(9)}, Fact{Reason: FactCold, Hours: hoursOf(10)}},
		{"hours signed", Fact{Reason: FactCold, Hours: hoursOf(-2)}, Fact{Reason: FactCold, Hours: hoursOf(-1)}},
		{"hours negative before zero", Fact{Reason: FactCold, Hours: hoursOf(-1)}, Fact{Reason: FactCold, Hours: hoursOf(0)}},
		{"hours a prefix first", Fact{Reason: FactConflict, Hours: hoursOf(10), Seq: new(U64(5))},
			Fact{Reason: FactConflict, Hours: hoursOf(10, 11), Seq: new(U64(5))}},
		{"hours element by element", Fact{Reason: FactConflict, Hours: hoursOf(9, 12), Seq: new(U64(5))},
			Fact{Reason: FactConflict, Hours: hoursOf(10, 11), Seq: new(U64(5))}},
		{"pack", Fact{Reason: FactUnevaluated, Hours: hoursOf(1), Pack: new(9)}, Fact{Reason: FactUnevaluated, Hours: hoursOf(1), Pack: new(10)}},
		{"block absent first", read(nil, new(U64(5)), "truncated"), read(new(0), new(U64(5)), "corrupt-block")},
		{"block", read(new(9), new(U64(5)), "corrupt-block"), read(new(10), new(U64(5)), "corrupt-block")},
		{"offset absent first", read(nil, nil, "truncated"), read(nil, new(U64(0)), "truncated")},
		{"offset as numbers", read(nil, new(U64(9)), "truncated"), read(nil, new(U64(10)), "truncated")},
		{"seq as numbers", Fact{Reason: FactContradiction, Seq: new(U64(9))}, Fact{Reason: FactContradiction, Seq: new(U64(10))}},
		{"seq at the top of u64", Fact{Reason: FactContradiction, Seq: new(U64(1 << 62))}, Fact{Reason: FactContradiction, Seq: new(U64(1<<64 - 1))}},
		{"defect by the table", read(new(1), new(U64(5)), "corrupt-block"), read(new(1), new(U64(5)), "unknown-codec")},
		{"coverage capture_id absent first", coverageFact(func(c *Coverage) {}),
			coverageFact(func(c *Coverage) { c.CaptureID = new(testCaptureID.String()) })},
		{"coverage capture_id by bytes", coverageFact(func(c *Coverage) { c.CaptureID = new(testCaptureID.String()) }),
			coverageFact(func(c *Coverage) { c.CaptureID = new(testOtherCapture.String()) })},
		{"coverage seq_first absent first", coverageFact(func(c *Coverage) { c.SeqLast = new(U64(1)) }),
			coverageFact(func(c *Coverage) { c.SeqFirst, c.SeqLast = new(U64(0)), new(U64(1)) })},
		{"coverage seq_first as numbers", coverageFact(func(c *Coverage) { c.SeqFirst = new(U64(9)) }),
			coverageFact(func(c *Coverage) { c.SeqFirst = new(U64(10)) })},
		{"coverage seq_last", coverageFact(func(c *Coverage) { c.SeqLast = new(U64(9)) }),
			coverageFact(func(c *Coverage) { c.SeqLast = new(U64(10)) })},
		{"coverage seq_last absent first", coverageFact(func(c *Coverage) { c.SeqFirst = new(U64(1)) }),
			coverageFact(func(c *Coverage) { c.SeqFirst, c.SeqLast = new(U64(1)), new(U64(0)) })},
		{"coverage time_start absent first", coverageFact(func(c *Coverage) { c.TimeEnd = new(I64(1)) }),
			coverageFact(func(c *Coverage) { c.TimeStart, c.TimeEnd = new(I64(0)), new(I64(1)) })},
		{"coverage time_start signed", coverageFact(func(c *Coverage) { c.TimeStart = new(I64(-1)) }),
			coverageFact(func(c *Coverage) { c.TimeStart = new(I64(0)) })},
		{"coverage time_end signed", coverageFact(func(c *Coverage) { c.TimeEnd = new(I64(-1)) }),
			coverageFact(func(c *Coverage) { c.TimeEnd = new(I64(0)) })},
		{"coverage time_end absent first", coverageFact(func(c *Coverage) {}),
			coverageFact(func(c *Coverage) { c.TimeEnd = new(I64(-1)) })},
		{"coverage unknown absent first", coverageFact(func(c *Coverage) {}),
			coverageFact(func(c *Coverage) { c.Unknown = []UnknownEntry{{Tag: 0, ValueType: 0, Value: []byte{}}} })},
		{"coverage unknown a prefix first", coverageFact(func(c *Coverage) { c.Unknown = []UnknownEntry{{Tag: 9, Value: []byte{1}}} }),
			coverageFact(func(c *Coverage) { c.Unknown = []UnknownEntry{{Tag: 9, Value: []byte{1}}, {Tag: 1, Value: []byte{}}} })},
		{"coverage unknown tag", coverageFact(func(c *Coverage) { c.Unknown = []UnknownEntry{{Tag: 9, ValueType: 5, Value: []byte{9}}} }),
			coverageFact(func(c *Coverage) { c.Unknown = []UnknownEntry{{Tag: 10, ValueType: 1, Value: []byte{1}}} })},
		{"coverage unknown value_type", coverageFact(func(c *Coverage) { c.Unknown = []UnknownEntry{{Tag: 9, ValueType: 1, Value: []byte{9}}} }),
			coverageFact(func(c *Coverage) { c.Unknown = []UnknownEntry{{Tag: 9, ValueType: 2, Value: []byte{1}}} })},
		{"coverage unknown bytes unsigned", coverageFact(func(c *Coverage) { c.Unknown = []UnknownEntry{{Tag: 9, Value: []byte{0x7F}}} }),
			coverageFact(func(c *Coverage) { c.Unknown = []UnknownEntry{{Tag: 9, Value: []byte{0x80}}} })},
		{"coverage unknown bytes a prefix first", coverageFact(func(c *Coverage) { c.Unknown = []UnknownEntry{{Tag: 9, Value: []byte{1}}} }),
			coverageFact(func(c *Coverage) { c.Unknown = []UnknownEntry{{Tag: 9, Value: []byte{1, 0}}} })},
		{"boundary capture_id", Fact{Reason: FactCaptureBoundary, Boundary: capBoundary(testCaptureID, 8, "start", 1)},
			Fact{Reason: FactCaptureBoundary, Boundary: capBoundary(testOtherCapture, 1, "start", 0)}},
		{"boundary seq as numbers", Fact{Reason: FactCaptureBoundary, Boundary: capBoundary(testCaptureID, 9, "start", 1)},
			Fact{Reason: FactCaptureBoundary, Boundary: capBoundary(testCaptureID, 10, "start", 1)}},
		{"boundary_kind by value, not by name", Fact{Reason: FactCaptureBoundary, Boundary: capBoundary(testCaptureID, 8, "stop", 1)},
			Fact{Reason: FactCaptureBoundary, Boundary: capBoundary(testCaptureID, 8, "gap", 1)}},
		{"boundary_kind unknown first", Fact{Reason: FactCaptureBoundary, Boundary: capBoundary(testCaptureID, 8, "unknown", 1)},
			Fact{Reason: FactCaptureBoundary, Boundary: capBoundary(testCaptureID, 8, "start", 1)}},
		{"boundary_kind an unnamed value last", Fact{Reason: FactCaptureBoundary, Boundary: capBoundary(testCaptureID, 8, "stop-unclean", 1)},
			Fact{Reason: FactCaptureBoundary, Boundary: capBoundary(testCaptureID, 8, "unknown(9)", 1)}},
		{"boundary epoch before ts", barrierFact(testCaptureID, func(b *FactBoundary) { b.Epoch, b.TS = 1, new(I64(5)) }, BasisTime),
			barrierFact(testCaptureID, func(b *FactBoundary) { b.Epoch, b.TS = 2, new(I64(-5)) }, BasisTime)},
		{"boundary ts signed", barrierFact(testCaptureID, func(b *FactBoundary) { b.TS = new(I64(-1)) }, BasisTime),
			barrierFact(testCaptureID, func(b *FactBoundary) { b.TS = new(I64(0)) }, BasisTime)},
		{"boundary gap_start absent first", barrierFact(testCaptureID, nil, BasisTime),
			barrierFact(testCaptureID, func(b *FactBoundary) { b.GapStart = new(I64(-100)) }, BasisTime)},
		{"boundary gap_start signed", barrierFact(testCaptureID, func(b *FactBoundary) { b.GapStart = new(I64(-1)) }, BasisTime),
			barrierFact(testCaptureID, func(b *FactBoundary) { b.GapStart = new(I64(0)) }, BasisTime)},
		{"boundary gap_end absent first", barrierFact(testCaptureID, func(b *FactBoundary) { b.GapStart = new(I64(1)) }, BasisTime),
			barrierFact(testCaptureID, func(b *FactBoundary) { b.GapStart, b.GapEnd = new(I64(1)), new(I64(-1)) }, BasisTime)},
		{"boundary gap_end as numbers", barrierFact(testCaptureID, func(b *FactBoundary) { b.GapEnd = new(I64(9)) }, BasisTime),
			barrierFact(testCaptureID, func(b *FactBoundary) { b.GapEnd = new(I64(10)) }, BasisTime)},
		{"boundary gap_end signed", barrierFact(testCaptureID, func(b *FactBoundary) { b.GapEnd = new(I64(-1)) }, BasisTime),
			barrierFact(testCaptureID, func(b *FactBoundary) { b.GapEnd = new(I64(0)) }, BasisTime)},
		{"basis a prefix first", barrierFact(testCaptureID, nil, BasisEpoch), barrierFact(testCaptureID, nil, BasisEpoch, BasisTime)},
		{"basis epoch before time", barrierFact(testCaptureID, nil, BasisEpoch, BasisTime), barrierFact(testCaptureID, nil, BasisTime)},
	}
	for _, p := range pairs {
		t.Run(p.name, func(t *testing.T) {
			t.Parallel()

			want := marshalJSON(t, []Fact{p.lo, p.hi})
			for _, in := range [][]Fact{{p.lo, p.hi}, {p.hi, p.lo}, {p.hi, p.lo, p.hi, p.lo}} {
				got, err := canonicalFacts(in)
				require.NoError(t, err)
				require.Equal(t, want, marshalJSON(t, got))
			}
			require.Negative(t, compareFacts(&p.lo, &p.hi))
			require.Positive(t, compareFacts(&p.hi, &p.lo))
			require.Zero(t, compareFacts(&p.lo, &p.lo))
		})
	}

	// Keys the fact table makes present or absent for every fact of a reason are still ordered absent first.
	absent := []struct {
		name   string
		lo, hi Fact
	}{
		{"hours", Fact{Reason: FactCold}, Fact{Reason: FactCold, Hours: hoursOf(-5)}},
		{"pack", Fact{Reason: FactRead}, Fact{Reason: FactRead, Pack: new(0)}},
		{"seq", Fact{Reason: FactConflict}, Fact{Reason: FactConflict, Seq: new(U64(0))}},
		{"defect", Fact{Reason: FactRead}, Fact{Reason: FactRead, Defect: "truncated"}},
		{"defect truncated first", Fact{Reason: FactRead, Defect: "truncated"}, Fact{Reason: FactRead, Defect: "corrupt-block"}},
		{"coverage", Fact{Reason: FactCoverage}, Fact{Reason: FactCoverage, Coverage: &Coverage{}}},
		{"boundary", Fact{Reason: FactBarrier}, Fact{Reason: FactBarrier, Boundary: capBoundary(testCaptureID, 0, "unknown", 0)}},
		{"boundary ts", Fact{Reason: FactBarrier, Boundary: capBoundary(testCaptureID, 3, "stop-unclean", 1)},
			barrierFact(testCaptureID, func(b *FactBoundary) { b.TS = new(I64(-1 << 63)) })},
		{"basis", Fact{Reason: FactBarrier}, Fact{Reason: FactBarrier, Basis: []string{BasisEpoch}}},
	}
	for _, p := range absent {
		require.Negative(t, compareFacts(&p.lo, &p.hi), p.name)
		require.Positive(t, compareFacts(&p.hi, &p.lo), p.name)
	}
}

// TestFactsJSON reads every fact reason, with its keys, from JSON written by hand,
// and refuses facts whose keys their row of the fact table does not give.
func TestFactsJSON(t *testing.T) {
	t.Parallel()

	var ls Lookups
	require.NoError(t, Unmarshal(canonicalText(t, testLookupsJSON), &ls))
	reasons := map[string]bool{}
	for _, f := range *ls[0].Expect.Gaps {
		require.NoError(t, f.check())
		reasons[f.Reason] = true
	}
	require.Len(t, reasons, len(factTable), "every reason of the fact table")

	refused := []Fact{
		{Reason: "stale"},
		{Reason: FactNoKey, Hours: hoursOf(1)},
		{Reason: FactConflict, Seq: new(U64(1))},
		{Reason: FactCold, Hours: hoursOf(2, 1)},
		{Reason: FactConflict, Hours: hoursOf(1, 1), Seq: new(U64(1))},
		{Reason: FactCold, Hours: hoursOf(1, 2)},
		{Reason: FactUnevaluated, Hours: hoursOf(1), Pack: new(-1)},
		{Reason: FactIndex, Hours: hoursOf(1), Pack: new(0), Block: new(0)},
		{Reason: FactRead, Hours: hoursOf(1), Pack: new(0), Defect: "index-mismatch"},
		{Reason: FactRead, Hours: hoursOf(1), Pack: new(0), Defect: "corrupt-block", Block: new(1)},
		{Reason: FactRead, Hours: hoursOf(1), Pack: new(0), Defect: "truncated", Block: new(1), Offset: new(U64(1))},
		{Reason: FactCoverage, Hours: hoursOf(1), Pack: new(0), Coverage: &Coverage{CaptureID: new("x")}},
		barrierFact(testCaptureID, nil),
		barrierFact(testCaptureID, nil, BasisTime, BasisEpoch),
		barrierFact(testCaptureID, nil, BasisTime, BasisTime),
		barrierFact(testCaptureID, func(b *FactBoundary) { b.TS = nil }, BasisTime),
		barrierFact(testCaptureID, func(b *FactBoundary) { b.BoundaryKind = "stop" }, BasisTime),
		barrierFact(testCaptureID, func(b *FactBoundary) { b.BoundaryKind = "halt" }, BasisTime),
		{Reason: FactCaptureBoundary, Boundary: &FactBoundary{CaptureID: testCaptureID.String(), BoundaryKind: "stop", TS: new(I64(1))}},
		{Reason: FactCaptureBoundary, Boundary: &FactBoundary{CaptureID: testCaptureID.String(), BoundaryKind: "stop", GapEnd: new(I64(1))}},
		{Reason: FactContradiction, Seq: new(U64(1)), Hours: hoursOf(1)},
	}
	for i := range refused {
		_, err := canonicalFacts([]Fact{refused[i]})
		require.Error(t, err, "%+v", refused[i])
	}
}

// testRun returns a lookup run of the primary at seq 5 of hour 10, two hours scheduled, epoch 1, with gaps.
func testRun(gaps ...tracepack.TxGap) *LookupRun {
	return &LookupRun{
		Key: tracepack.TxKey{Capture: testCaptureID, Seq: 5, Hour: 10}, MaxScopes: 2, Numbers: testNumbers,
		Result: tracepack.TxResult{Outcome: tracepack.TxIncomplete, Epoch: 1, Gaps: gaps},
	}
}

// barrierGap returns a barrier gap of b.
func barrierGap(b *tracepack.Boundary) tracepack.TxGap {
	return tracepack.TxGap{Reason: tracepack.TxGapBarrier, Block: -1, Offset: -1, Barrier: b}
}

// normalized returns the canonical JSON of the facts of run's gaps.
func normalized(t *testing.T, run *LookupRun) string {
	t.Helper()

	fs, err := NormalizeGaps(run)
	require.NoError(t, err)

	return marshalJSON(t, fs)
}

func TestNormalizeBarriers(t *testing.T) {
	t.Parallel()

	inHours := new(10 * hourNs)
	before := new(int64(-100))
	tests := []struct {
		name  string
		b     *tracepack.Boundary
		gaps  int
		basis []string
	}{
		{"both bases in two gaps", testBoundary(testCaptureID, inHours, nil), 2, []string{BasisEpoch, BasisTime}},
		{"epoch alone", testBoundary(testCaptureID, before, before), 1, []string{BasisEpoch}},
		{"time alone, another capture", testBoundary(testOtherCapture, nil, inHours), 1, []string{BasisTime}},
		{"an inverted gap meets every range", testBoundary(testOtherCapture, new(int64(5)), new(int64(4))), 1, []string{BasisTime}},
		{"a gap ending at the first hour's start", testBoundary(testOtherCapture, before, inHours), 1, []string{BasisTime}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var gaps []tracepack.TxGap
			for range tt.gaps {
				c := *tt.b
				gaps = append(gaps, barrierGap(&c))
			}
			want := marshalJSON(t, []Fact{{Reason: FactBarrier, Boundary: barrierBoundary(tt.b), Basis: tt.basis}})
			require.Equal(t, want, normalized(t, testRun(gaps...)))

			// One gap more or fewer than the bases that hold fails.
			_, err := NormalizeGaps(testRun(append(gaps, barrierGap(tt.b))...))
			require.Error(t, err)
			_, err = NormalizeGaps(testRun(gaps[1:]...))
			if len(gaps) > 1 {
				require.Error(t, err)
			}
		})
	}

	// A gap past the hours scheduled, [10 h, 12 h), and of another capture: no basis holds.
	_, err := NormalizeGaps(testRun(barrierGap(testBoundary(testOtherCapture, new(12*hourNs), nil))))
	require.Error(t, err)

	// A closing record of the primary's epoch above it, without a conflict, lifts the epoch basis; one in conflict does not.
	closing := tracepack.TxRecord{Record: tracepack.Record{Seq: 7, Epoch: 1}, Class: tracepack.TxClosing}
	run := testRun(barrierGap(testBoundary(testCaptureID, inHours, nil)))
	run.Result.Records = []tracepack.TxRecord{closing}
	require.Equal(t, marshalJSON(t, []Fact{barrierFact(testCaptureID, func(b *FactBoundary) { b.GapStart = new(I64(10 * hourNs)) }, BasisTime)}),
		normalized(t, run))
	for _, edit := range []func(*tracepack.TxRecord){
		func(r *tracepack.TxRecord) { r.Conflict = true },
		func(r *tracepack.TxRecord) { r.Record.Epoch = 2 },
		func(r *tracepack.TxRecord) { r.Record.Seq = 5 },
	} {
		c := closing
		edit(&c)
		run.Result.Records = []tracepack.TxRecord{c}
		_, err := NormalizeGaps(run)
		require.Error(t, err, "the epoch basis holds, so one gap is short of two bases")
	}

	// Two evidence boundaries equal in capture, seq, kind and epoch, but not in ts and gap bounds, are two barriers.
	b1 := testBoundary(testCaptureID, before, before)
	b2 := testBoundary(testCaptureID, new(int64(-200)), before)
	b2.TS = 1
	fs, err := NormalizeGaps(testRun(barrierGap(b1), barrierGap(b2)))
	require.NoError(t, err)
	require.Len(t, fs, 2)
	require.Equal(t, []string{BasisEpoch}, fs[0].Basis)
	require.Equal(t, []string{BasisEpoch}, fs[1].Basis)
}

func TestNormalizeGaps(t *testing.T) {
	t.Parallel()

	at := func(seq uint64) *uint64 { return &seq }
	start := func(ts int64, gapStart *int64) *tracepack.Boundary {
		return &tracepack.Boundary{Capture: testCaptureID, Seq: 8, Kind: tracepack.BoundaryKindStart, TS: ts, Epoch: 1, GapStart: gapStart}
	}
	evidence := tracepack.TxGap{Reason: tracepack.TxGapCaptureBoundary, Block: -1, Offset: -1, Seq: at(8), Barrier: start(4, nil)}
	read := tracepack.TxGap{
		Reason: tracepack.TxGapCaptureBoundary, Hours: []int64{10}, Pack: new(testPack0), Block: 0, Offset: -1, Seq: at(8),
		Barrier: start(5, new(int64(1))),
	}
	oneBoundary := marshalJSON(t, []Fact{{Reason: FactCaptureBoundary, Boundary: capBoundary(testCaptureID, 8, "start", 1)}})

	// An evidence boundary and a read boundary record equal in capture, seq, kind and epoch,
	// but not in ts and gap bounds, are one capture-boundary fact; duplicates collapse.
	require.Equal(t, oneBoundary, normalized(t, testRun(evidence, read)))
	require.Equal(t, oneBoundary, normalized(t, testRun(read, evidence, read)))

	// The witness of ordering-uncertain is not projected:
	// a higher seq met first and the smallest seq give the same fact.
	uncertain := func(seq uint64) tracepack.TxGap {
		return tracepack.TxGap{
			Reason: tracepack.TxGapOrderingUncertain, Hours: []int64{10}, Pack: new(testPack0), Block: 0, Offset: -1, Seq: at(seq),
		}
	}
	higherFirst := normalized(t, testRun(uncertain(9)))
	require.Equal(t, higherFirst, normalized(t, testRun(uncertain(6))))
	require.Equal(t, marshalJSON(t, []Fact{{Reason: FactOrderingUncertain}}), higherFirst)

	// An index gap without a block is dropped beside an index gap of a block, and fails alone.
	blockless := tracepack.TxGap{Reason: tracepack.TxGapIndex, Hours: []int64{10}, Block: -1, Offset: -1, Seq: at(5)}
	block := tracepack.TxGap{
		Reason: tracepack.TxGapIndex, Hours: []int64{10}, Pack: new(testPack1), Block: 2, Offset: 77, Defect: tracepack.ReasonIndexMismatch,
	}
	require.Equal(t, marshalJSON(t, []Fact{{Reason: FactIndex, Hours: hoursOf(10), Pack: new(1), Block: new(2), Offset: new(U64(77))}}),
		normalized(t, testRun(blockless, block)))
	_, err := NormalizeGaps(testRun(blockless))
	require.Error(t, err)
	otherHour := block
	otherHour.Hours = []int64{11}
	_, err = NormalizeGaps(testRun(blockless, otherHour))
	require.Error(t, err, "an index gap of a block of another scope than the primary's does not account for it")

	// A no-key gap keeps only its reason, its coverage entry included.
	cov := testCoverage()
	noKey := tracepack.TxGap{Reason: tracepack.TxGapNoKey, Hours: []int64{10}, Block: -1, Offset: -1, Seq: at(5), Coverage: &cov}
	require.Equal(t, marshalJSON(t, []Fact{{Reason: FactNoKey}}), normalized(t, testRun(noKey, noKey)))

	refused := []struct {
		name string
		gap  tracepack.TxGap
	}{
		{"an undefined reason", tracepack.TxGap{Reason: 99, Block: -1, Offset: -1}},
		{"a conflict naming a pack", tracepack.TxGap{Reason: tracepack.TxGapConflict, Hours: []int64{10}, Pack: new(testPack0), Block: -1, Offset: -1, Seq: at(5)}},
		{"a conflict without a seq", tracepack.TxGap{Reason: tracepack.TxGapConflict, Hours: []int64{10}, Block: -1, Offset: -1}},
		{"a cold scope with a seq", tracepack.TxGap{Reason: tracepack.TxGapCold, Hours: []int64{10}, Block: -1, Offset: -1, Seq: at(5)}},
		{"a scope breach without a block", tracepack.TxGap{
			Reason: tracepack.TxGapScopeBreach, Hours: []int64{10}, Pack: new(testPack0), Block: -1, Offset: -1, Seq: at(5),
		}},
		{"a pack no source pack is", tracepack.TxGap{Reason: tracepack.TxGapUnevaluated, Hours: []int64{10}, Pack: new(testPackID), Block: -1, Offset: -1}},
		{"a read gap of a coverage defect", tracepack.TxGap{
			Reason: tracepack.TxGapRead, Hours: []int64{10}, Pack: new(testPack0), Block: -1, Offset: -1, Defect: tracepack.ReasonCoverage,
		}},
		{"an index gap of a read defect", tracepack.TxGap{
			Reason: tracepack.TxGapIndex, Hours: []int64{10}, Pack: new(testPack0), Block: 1, Offset: 1, Defect: tracepack.ReasonCorruptBlock,
		}},
		{"an index gap without its offset", tracepack.TxGap{
			Reason: tracepack.TxGapIndex, Hours: []int64{10}, Pack: new(testPack0), Block: 1, Offset: -1, Defect: tracepack.ReasonIndexMismatch,
		}},
		{"a capture-boundary of another epoch", tracepack.TxGap{
			Reason: tracepack.TxGapCaptureBoundary, Block: -1, Offset: -1, Seq: at(8),
			Barrier: &tracepack.Boundary{Capture: testCaptureID, Seq: 8, Kind: tracepack.BoundaryKindStart, Epoch: 2},
		}},
		{"a capture-boundary of another capture", tracepack.TxGap{
			Reason: tracepack.TxGapCaptureBoundary, Block: -1, Offset: -1, Seq: at(8),
			Barrier: &tracepack.Boundary{Capture: testOtherCapture, Seq: 8, Kind: tracepack.BoundaryKindStart, Epoch: 1},
		}},
		{"a capture-boundary at another seq", tracepack.TxGap{
			Reason: tracepack.TxGapCaptureBoundary, Block: -1, Offset: -1, Seq: at(9), Barrier: start(4, nil),
		}},
		{"a barrier of another kind", barrierGap(&tracepack.Boundary{Capture: testOtherCapture, Seq: 3, Kind: tracepack.BoundaryKindStop, Epoch: 1})},
		{"a contradiction with hours", tracepack.TxGap{Reason: tracepack.TxGapContradiction, Hours: []int64{10}, Block: -1, Offset: -1, Seq: at(5)}},
		{"a conflict whose hours repeat", tracepack.TxGap{Reason: tracepack.TxGapConflict, Hours: []int64{10, 10}, Block: -1, Offset: -1, Seq: at(5)}},
	}
	for _, tt := range refused {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := NormalizeGaps(testRun(tt.gap))
			require.Error(t, err)
		})
	}

	for _, run := range []*LookupRun{
		{Key: tracepack.TxKey{Hour: 10}, MaxScopes: 0},
		{Key: tracepack.TxKey{Hour: tracepack.MaxTxHour}, MaxScopes: 2},
		{Key: tracepack.TxKey{Hour: tracepack.MinTxHour - 1}, MaxScopes: 1},
	} {
		_, err := NormalizeGaps(run)
		require.Error(t, err, fmt.Sprintf("hour %d, max_scopes %d", run.Key.Hour, run.MaxScopes))
	}
}
