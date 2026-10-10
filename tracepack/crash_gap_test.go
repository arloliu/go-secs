package tracepack

import (
	"bytes"
	"fmt"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// crashGapStart and crashGapEnd bound the gap of the crash-gap test boundary.
var (
	crashGapStart = blockTestHour + 100
	crashGapEnd   = blockTestHour + 200
)

// crashGapSteps returns the records of the crash-gap test pack as three blocks,
// the boundary at seq 12 of kind with the gap bounds gapStart and gapEnd:
//
//	block 0: 10 data at +10, 11 data at +11
//	block 1: 12 capture-boundary at +50
//	block 2: 13 data at +300
func crashGapSteps(t testing.TB, kind BoundaryKind, gapStart, gapEnd *int64) []footerTestStep {
	t.Helper()

	h := blockTestHour

	return []footerTestStep{
		{rec: testDataRecord(10, h+10, 1)},
		{rec: testDataRecord(11, h+11, 1), flush: true},
		{rec: testEventRecord(t, 12, h+50, 1, &TransportEvent{
			Event: EventCaptureBoundary, BoundaryKind: new(kind), GapStart: gapStart, GapEnd: gapEnd,
		}), flush: true},
		{rec: testDataRecord(13, h+300, 1), flush: true},
	}
}

// crashGapPack writes steps into a finalized pack id of capture and returns it with the Writer's block summaries.
func crashGapPack(t testing.TB, id, capture UUID, meta func(*PackMeta), steps []footerTestStep) *repairPack {
	t.Helper()

	p := writeRepairPack(t, CodecZstd, meta, false, steps)
	p.file = withIDs(t, p.file, id, capture)

	return p
}

// uncleanPack returns the crash-gap test pack id of capture,
// its boundary a stop-unclean one over [crashGapStart, crashGapEnd].
func uncleanPack(t testing.TB, id, capture UUID) *repairPack {
	t.Helper()

	return crashGapPack(t, id, capture, nil, crashGapSteps(t, BoundaryKindStopUnclean, new(crashGapStart), new(crashGapEnd)))
}

// pastBoundaryBlock is a filter that excludes blocks 0 and 1 of the crash-gap test pack, keeps block 2,
// and whose time range meets the gap [crashGapStart, crashGapEnd].
func pastBoundaryBlock() Filter {
	return Filter{TimeFrom: new(blockTestHour + 150), TimeTo: new(blockTestHour + 400)}
}

// pastGap is a filter that keeps block 2 of the crash-gap test pack
// and whose time range misses the gap [crashGapStart, crashGapEnd].
func pastGap() Filter {
	return Filter{TimeFrom: new(blockTestHour + 250), TimeTo: new(blockTestHour + 400)}
}

// requireNoCrashGap runs q over r with Iterate and with MergeIterate in both orders,
// and requires that neither reports a crash gap.
func requireNoCrashGap(t *testing.T, r *Reader, q Query) {
	t.Helper()

	run := iterate(t, r, q)
	assert.Equal(t, []uint64{13}, run.seqs())
	assert.Empty(t, run.res.Incomplete, "Iterate")
	for _, order := range []Order{OrderCapture, OrderTime} {
		_, res, err := collectMergeIterate(t.Context(), []*Reader{r}, q, MergeIterateOptions{Order: order})
		require.NoError(t, err)
		assert.Empty(t, res.Incomplete, "MergeIterate, %v", order)
	}
}

// crashGapDefect returns the ReasonCrashGap defect of pack, without its Err,
// for the stop-unclean boundary at seq 12 of capture with the gap bounds gapStart and gapEnd.
func crashGapDefect(pack int, capture UUID, gapStart, gapEnd int64) Defect {
	return Defect{
		Reason: ReasonCrashGap, Pack: pack, Block: -1, Offset: -1,
		Boundary: &Boundary{
			Capture: capture, Seq: 12, Kind: BoundaryKindStopUnclean, TS: blockTestHour + 50, Epoch: 1,
			GapStart: new(gapStart), GapEnd: new(gapEnd),
		},
	}
}

// withoutErrs returns a copy of ds whose Err fields are nil, requiring each crash-gap defect to carry an Err.
func withoutErrs(t testing.TB, ds []Defect) []Defect {
	t.Helper()

	out := make([]Defect, len(ds))
	for i, d := range ds {
		if d.Reason == ReasonCrashGap {
			require.Error(t, d.Err, "defect %d", i)
		}
		d.Err = nil
		out[i] = d
	}

	return out
}

// readsAt counts the calls of g at off.
func readsAt(g *gatedReader, off uint64) int {
	n := 0
	for _, c := range g.allCalls() {
		if c.off == int64(off) {
			n++
		}
	}

	return n
}

func TestCrashGapReasonString(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "crash-gap", ReasonCrashGap.String())
}

func TestCrashGapMeetsRange(t *testing.T) {
	t.Parallel()

	p := func(v int64) *int64 { return new(v) }
	tests := []struct {
		name       string
		start, end *int64
		from, to   *int64
		want       bool
	}{
		{name: "inside", start: p(10), end: p(20), from: p(0), to: p(100), want: true},
		{name: "before", start: p(10), end: p(20), from: p(30), to: p(100)},
		{name: "after", start: p(200), end: p(300), from: p(0), to: p(100)},
		{name: "end touches from", start: p(10), end: p(30), from: p(30), to: p(100), want: true},
		{name: "end one below from", start: p(10), end: p(29), from: p(30), to: p(100)},
		{name: "start touches to", start: p(100), end: p(200), from: p(0), to: p(100)},
		{name: "start one below to", start: p(99), end: p(200), from: p(0), to: p(100), want: true},
		{name: "start absent, end before", end: p(20), from: p(30), to: p(100)},
		{name: "start absent, end inside", end: p(30), from: p(30), to: p(100), want: true},
		{name: "end absent, start after", start: p(100), from: p(0), to: p(100)},
		{name: "end absent, start inside", start: p(50), from: p(0), to: p(100), want: true},
		{name: "both absent", from: p(0), to: p(100), want: true},
		{name: "inverted", start: p(20), end: p(10), from: p(100), to: p(200), want: true},
		{name: "inverted, empty range", start: p(20), end: p(10), from: p(5), to: p(5), want: true},
		{name: "both absent, empty range", from: p(5), to: p(5)},
		{name: "from only, gap before", start: p(10), end: p(20), from: p(30)},
		{name: "from only, gap touches", start: p(10), end: p(20), from: p(20), want: true},
		{name: "to only, gap after", start: p(10), end: p(20), to: p(10)},
		{name: "to only, gap meets", start: p(10), end: p(20), to: p(11), want: true},
		{name: "no time range", start: p(10), end: p(20), want: true},
		{name: "no upper bound, gap at MaxInt64", start: p(math.MaxInt64), from: p(0), want: true},
		{name: "no upper bound, gap of MaxInt64 only", start: p(math.MaxInt64), end: p(math.MaxInt64), from: p(0), want: true},
		{name: "no upper bound, gap before", start: p(math.MinInt64), end: p(-1), from: p(0)},
	}
	for _, tt := range tests {
		b := Boundary{Kind: BoundaryKindStopUnclean, GapStart: tt.start, GapEnd: tt.end}
		assert.Equal(t, tt.want, crashGapMeets(&b, tt.from, tt.to), tt.name)
	}
}

// TestIterateCrashGapFromFooter reads a pack whose footer holds a stop-unclean boundary,
// with a filter that excludes the block holding it, so that block is not read:
// the read reports the boundary from the footer's boundary entry.
// The same holds through MergeIterate, the pack beside another of another capture.
// A time range that misses the gap reports nothing.
func TestIterateCrashGapFromFooter(t *testing.T) {
	t.Parallel()

	p := uncleanPack(t, seg0, captureLow)
	q := Query{Filter: pastBoundaryBlock()}
	want := []Defect{crashGapDefect(0, captureLow, crashGapStart, crashGapEnd)}

	r, g := openGated(t, p.file)
	require.Len(t, r.Blocks(), 3)
	require.True(t, r.Blocks()[1].Indexed)
	read, run := blocksRead(t, r, g, q)
	assert.Equal(t, []int{2}, read, "the filter excludes the block holding the boundary")
	assert.Equal(t, []uint64{13}, run.seqs())
	assert.Equal(t, want, withoutErrs(t, run.res.Incomplete))
	*run.res.Incomplete[0].Boundary.GapStart = 0
	assert.Equal(t, want, withoutErrs(t, iterate(t, r, q).res.Incomplete), "each read's boundary is its own copy")

	other := crashGapPack(t, seg1, captureHigh, nil, []footerTestStep{
		{rec: testDataRecord(1, blockTestHour+300, 1)}, {rec: testDataRecord(2, blockTestHour+310, 1), flush: true},
	}).file
	for _, order := range []Order{OrderCapture, OrderTime} {
		r, g := openGated(t, p.file)
		before := readsAt(g, p.blocks[1].offset)
		items, res, err := collectMergeIterate(t.Context(), []*Reader{mustOpen(t, other, ReaderOptions{}), r}, q,
			MergeIterateOptions{Order: order})
		require.NoError(t, err, "%v", order)
		assert.Len(t, items, 3, "%v", order)
		assert.Equal(t, before, readsAt(g, p.blocks[1].offset), "%v: the block holding the boundary is not read", order)
		want := []Defect{crashGapDefect(1, captureLow, crashGapStart, crashGapEnd)}
		assert.Equal(t, want, withoutErrs(t, res.Incomplete), "%v", order)
	}

	requireNoCrashGap(t, mustOpen(t, p.file, ReaderOptions{}), Query{Filter: pastGap()})
}

// TestIterateCrashGapWalkedPack reads the crash-gap test pack with its footer rejected:
// the boundary record the forward walk's block decodes is reported;
// with that block failing its read, only its own defect is.
// A time range that misses the gap reports nothing, through Iterate and MergeIterate.
func TestIterateCrashGapWalkedPack(t *testing.T) {
	t.Parallel()

	p := uncleanPack(t, seg0, captureLow)
	q := Query{Filter: pastBoundaryBlock()}

	r := mustOpen(t, invalidFooterFile(t, p.file), ReaderOptions{})
	require.ErrorIs(t, r.Header().FooterErr, ErrInvalidFooter)
	run := iterate(t, r, q)
	assert.Equal(t, []uint64{13}, run.seqs())
	assert.Equal(t, []Defect{crashGapDefect(0, captureLow, crashGapStart, crashGapEnd)}, withoutErrs(t, run.res.Incomplete))
	requireNoCrashGap(t, r, Query{Filter: pastGap()})

	r = mustOpen(t, invalidFooterFile(t, flipByte(p.file, p.bodyByteOf(1))), ReaderOptions{})
	require.ErrorIs(t, r.Header().FooterErr, ErrInvalidFooter)
	run = iterate(t, r, q)
	assert.Equal(t, []uint64{13}, run.seqs())
	require.Len(t, run.res.Incomplete, 1, "the failed block's defect alone")
	assert.Equal(t, ReasonCorruptBlock, run.res.Incomplete[0].Reason)
	assert.Equal(t, 1, run.res.Incomplete[0].Block)
}

// TestMergeIterateCrashGapDedup reads boundaries that several packs hold:
// one boundary in a segment and in its archive is one defect, its Pack the first given;
// two boundaries of one seq whose gap bounds differ are two defects;
// a boundary of another capture is reported too.
func TestMergeIterateCrashGapDedup(t *testing.T) {
	t.Parallel()

	archive := func(m *PackMeta) {
		m.PackRole = PackRoleArchive
		m.ScopeGeneration, m.PublisherEpoch = new(uint64(1)), new(uint64(1))
		m.ReplacementSetID, m.ReplacementSetSize, m.ReplacementSetIndex = new(setA), new(uint64(1)), new(uint64(0))
	}
	steps := crashGapSteps(t, BoundaryKindStopUnclean, new(crashGapStart), new(crashGapEnd))
	seg := mustOpen(t, uncleanPack(t, seg0, captureLow).file, ReaderOptions{})
	arch := mustOpen(t, crashGapPack(t, memA, captureLow, archive, steps).file, ReaderOptions{})
	for _, order := range []Order{OrderCapture, OrderTime} {
		for _, readers := range [][]*Reader{{seg, arch}, {arch, seg}} {
			_, res, err := collectMergeIterate(t.Context(), readers, Query{}, MergeIterateOptions{Order: order})
			require.NoError(t, err)
			assert.Empty(t, res.Conflicts)
			assert.Equal(t, []Defect{crashGapDefect(0, captureLow, crashGapStart, crashGapEnd)}, withoutErrs(t, res.Incomplete),
				"%v: one defect, its Pack the first given", order)
		}
	}

	later := crashGapEnd + 50
	differing := mustOpen(t, crashGapPack(t, seg1, captureLow, nil,
		crashGapSteps(t, BoundaryKindStopUnclean, new(crashGapStart), new(later))).file, ReaderOptions{})
	high := mustOpen(t, uncleanPack(t, seg2, captureHigh).file, ReaderOptions{})
	for _, order := range []Order{OrderCapture, OrderTime} {
		_, res, err := collectMergeIterate(t.Context(), []*Reader{seg, differing, high, arch}, Query{}, MergeIterateOptions{Order: order})
		require.NoError(t, err)
		assert.Len(t, res.Conflicts, 1, "%v: the boundary records differ", order)
		assert.Equal(t, []Defect{
			crashGapDefect(0, captureLow, crashGapStart, crashGapEnd),
			crashGapDefect(1, captureLow, crashGapStart, later),
			crashGapDefect(2, captureHigh, crashGapStart, crashGapEnd),
		}, withoutErrs(t, res.Incomplete), "%v", order)
	}
}

// TestMergeIterateCrashGapMixedSources reads one boundary from a pack without a used footer, given first,
// whose pre-read decodes it, and from the footer of a pack given second:
// one defect, Pack 0, at the start of the run, before the defect of a block the run reads.
// Distinct boundaries, the footer's found before the walked pack's, are listed by Pack all the same.
func TestMergeIterateCrashGapMixedSources(t *testing.T) {
	t.Parallel()

	walked := mustOpen(t, invalidFooterFile(t, uncleanPack(t, seg0, captureLow).file), ReaderOptions{})
	require.Error(t, walked.Header().FooterErr)
	p := uncleanPack(t, seg1, captureLow)
	indexed := mustOpen(t, flipByte(p.file, p.bodyByteOf(2)), ReaderOptions{})
	require.NoError(t, indexed.Header().FooterErr)

	for _, order := range []Order{OrderCapture, OrderTime} {
		_, res, err := collectMergeIterate(t.Context(), []*Reader{walked, indexed}, Query{}, MergeIterateOptions{Order: order})
		require.NoError(t, err)
		got := withoutErrs(t, res.Incomplete)
		require.Len(t, got, 2, "%v", order)
		assert.Equal(t, crashGapDefect(0, captureLow, crashGapStart, crashGapEnd), got[0], "%v", order)
		assert.Equal(t, ReasonCorruptBlock, got[1].Reason, "%v: the run's defect follows", order)
		assert.Equal(t, [2]int{1, 2}, [2]int{got[1].Pack, got[1].Block}, "%v", order)
	}

	walkedHigh := mustOpen(t, invalidFooterFile(t, uncleanPack(t, seg2, captureHigh).file), ReaderOptions{})
	for _, order := range []Order{OrderCapture, OrderTime} {
		_, res, err := collectMergeIterate(t.Context(), []*Reader{walkedHigh, indexed}, Query{}, MergeIterateOptions{Order: order})
		require.NoError(t, err)
		got := withoutErrs(t, res.Incomplete)
		require.Len(t, got, 3, "%v", order)
		assert.Equal(t, []Defect{
			crashGapDefect(0, captureHigh, crashGapStart, crashGapEnd),
			crashGapDefect(1, captureLow, crashGapStart, crashGapEnd),
		}, got[:2], "%v: by Pack, not in the order found", order)
		assert.Equal(t, ReasonCorruptBlock, got[2].Reason, "%v", order)
	}
}

// TestMergeIterateCrashGapExcludedCluster reads the crash-gap test pack with its footer rejected:
// a filter that excludes the cluster of the block holding the boundary, by the block's computed summary,
// still reports the boundary its pre-read decoded, the block read once;
// without a filter the block is read twice, and the boundary is reported once.
func TestMergeIterateCrashGapExcludedCluster(t *testing.T) {
	t.Parallel()

	p := uncleanPack(t, seg0, captureLow)
	file := invalidFooterFile(t, p.file)
	want := []Defect{crashGapDefect(0, captureLow, crashGapStart, crashGapEnd)}
	for _, tt := range []struct {
		name  string
		q     Query
		reads int
	}{
		{name: "cluster excluded", q: Query{Filter: pastBoundaryBlock()}, reads: 1},
		{name: "cluster read", q: Query{}, reads: 2},
	} {
		for _, order := range []Order{OrderCapture, OrderTime} {
			r, g := openGated(t, file)
			require.False(t, r.Blocks()[1].Indexed)
			before := readsAt(g, p.blocks[1].offset)
			_, res, err := collectMergeIterate(t.Context(), []*Reader{r}, tt.q, MergeIterateOptions{Order: order})
			require.NoError(t, err)
			assert.Equal(t, tt.reads, readsAt(g, p.blocks[1].offset)-before, "%s, %v: reads of the boundary's block", tt.name, order)
			assert.Equal(t, want, withoutErrs(t, res.Incomplete), "%s, %v", tt.name, order)
		}
	}
}

// TestExportJSONLCrashGap exports the crash-gap test pack, with its footer used and rejected:
// its Result holds the crash gap, as Iterate's does, and the export is the pack's records.
func TestExportJSONLCrashGap(t *testing.T) {
	t.Parallel()

	file := uncleanPack(t, seg0, captureLow).file
	for _, walked := range []bool{false, true} {
		f := file
		if walked {
			f = invalidFooterFile(t, file)
		}
		r := mustOpen(t, f, ReaderOptions{})
		var buf bytes.Buffer
		res, err := ExportJSONL(t.Context(), r, &buf)
		require.NoError(t, err)
		assert.Equal(t, []Defect{crashGapDefect(0, captureLow, crashGapStart, crashGapEnd)}, withoutErrs(t, res.Incomplete),
			"walked %v", walked)

		run := iterate(t, r, Query{Payloads: true})
		requireSameResult(t, run.res, res)
		assert.Equal(t, 1+len(run.items), bytes.Count(buf.Bytes(), []byte("\n")), "walked %v: a header line, then a line per record", walked)
	}
}

// TestCrashGapNotFromCleanStop reads packs whose boundary at seq 12 is a clean stop or a gap boundary,
// its gap bounds meeting the query: no read reports anything.
func TestCrashGapNotFromCleanStop(t *testing.T) {
	t.Parallel()

	for _, kind := range []BoundaryKind{BoundaryKindStop, BoundaryKindGap} {
		file := crashGapPack(t, seg0, captureLow, nil, crashGapSteps(t, kind, new(crashGapStart), new(crashGapEnd))).file
		for _, walked := range []bool{false, true} {
			f := file
			if walked {
				f = invalidFooterFile(t, file)
			}
			r := mustOpen(t, f, ReaderOptions{})
			assert.Empty(t, iterate(t, r, Query{}).res.Incomplete, "%v, walked %v", kind, walked)
			_, res, err := collectMergeIterate(t.Context(), []*Reader{r}, Query{}, MergeIterateOptions{Order: OrderTime})
			require.NoError(t, err)
			assert.Empty(t, res.Incomplete, "%v, walked %v", kind, walked)
		}
	}
}

// TestMergeIterateCrashGapPrereadFailure reads a pack whose footer holds a stop-unclean boundary,
// a pack without a used footer whose pre-read decodes another,
// and a third whose pre-read fails on ReadAt:
// the read returns the error, its Result holding both crash gaps, by pack.
func TestMergeIterateCrashGapPrereadFailure(t *testing.T) {
	t.Parallel()

	indexed := mustOpen(t, uncleanPack(t, seg0, captureLow).file, ReaderOptions{})
	walked := mustOpen(t, invalidFooterFile(t, uncleanPack(t, seg1, captureHigh).file), ReaderOptions{})
	failing := invalidFooterFile(t, crashGapPack(t, seg2, captureLow, nil, crashGapSteps(t, BoundaryKindStop, nil, nil)).file)
	for _, order := range []Order{OrderCapture, OrderTime} {
		t.Run(fmt.Sprint(order), func(t *testing.T) {
			t.Parallel()

			g := newGatedReader(failing)
			r, err := Open(t.Context(), g, int64(len(failing)), uncachedWindows())
			require.NoError(t, err)
			off := int64(r.Blocks()[1].Offset)
			g.fail = func(o int64, _ int) bool { return o == off }

			_, res, err := collectMergeIterate(t.Context(), []*Reader{indexed, walked, r}, Query{}, MergeIterateOptions{Order: order})
			require.ErrorIs(t, err, errInjected)
			assert.Equal(t, []Defect{
				crashGapDefect(0, captureLow, crashGapStart, crashGapEnd),
				crashGapDefect(1, captureHigh, crashGapStart, crashGapEnd),
			}, withoutErrs(t, res.Incomplete))
		})
	}
}
