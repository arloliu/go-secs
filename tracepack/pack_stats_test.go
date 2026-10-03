package tracepack

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// richStatsPack writes the records of richFooterSteps into pack seg0 of capture, finalized.
func richStatsPack(t testing.TB, capture UUID) []byte {
	t.Helper()

	return planTestPack(t, seg0, capture, false, richFooterSteps(t))
}

// richStats returns the F-5 statistics of the records of richFooterSteps in a pack of capture,
// the count arrays, content bytes and quality union taken from ref, the pack's F-5 as parsePackStats types it.
func richStats(capture UUID, ref *packStats) PackStats {
	h, h1 := blockTestHour, blockTestHour+hourNs
	gapStart, gapEnd := h+100, h+200

	return PackStats{
		RecordCount:        11,
		KindCounts:         ref.kindCounts,
		DirCounts:          ref.dirCounts,
		DecodeStatusCounts: ref.decodeStatusCounts,
		TSMin:              h + 10,
		TSMax:              h1 + 4,
		ContentBytes:       ref.contentBytes,
		QualityUnion:       ref.qualityUnion,
		SeqRanges:          []SeqRange{{First: 10, Last: 11}, {First: 13, Last: 15}, {First: 20, Last: 23}, {First: 30, Last: 31}},
		Epochs: []EpochStats{
			{Epoch: 0, RecordCount: 2, SeqFirst: 10, SeqLast: 20, TSMin: h + 50, TSMax: h + 300},
			{Epoch: 1, RecordCount: 4, SeqFirst: 11, SeqLast: 15, TSMin: h + 10, TSMax: h + 40, CloseSeq: new(uint64(15))},
			{Epoch: 2, RecordCount: 3, SeqFirst: 21, SeqLast: 23, TSMin: h + 400, TSMax: h1 + 2},
			{Epoch: 3, RecordCount: 2, SeqFirst: 30, SeqLast: 31, TSMin: h1 + 3, TSMax: h1 + 4, CloseSeq: new(uint64(31))},
		},
		Boundaries: []Boundary{
			{Capture: capture, Seq: 10, Kind: BoundaryKindStart, TS: h + 50, Epoch: 0},
			{Capture: capture, Seq: 20, Kind: BoundaryKindGap, TS: h + 300, Epoch: 0, GapStart: &gapStart, GapEnd: &gapEnd},
			{Capture: capture, Seq: 31, Kind: BoundaryKindStop, TS: h1 + 4, Epoch: 3},
		},
	}
}

// TestReaderStats reads the statistics of a pack with every F-5 structure:
// every value as the records give it, each boundary with the file header's capture_id.
func TestReaderStats(t *testing.T) {
	t.Parallel()

	file := richStatsPack(t, captureHigh)
	_, decoded, _ := splitPack(t, file)
	st, ok := mustOpen(t, file, ReaderOptions{}).Stats()
	require.True(t, ok)
	assert.Equal(t, richStats(captureHigh, footerStats(t, decoded)), st)
}

// TestReaderStatsEmptyPack reads the statistics of a valid pack without records:
// present, with no time, seq, epoch or boundary entries.
func TestReaderStatsEmptyPack(t *testing.T) {
	t.Parallel()

	st, ok := mustOpen(t, planTestPack(t, seg0, captureLow, false, nil), ReaderOptions{}).Stats()
	require.True(t, ok)
	assert.Zero(t, st.RecordCount)
	assert.Zero(t, st.ContentBytes)
	assert.Nil(t, st.SeqRanges)
	assert.Nil(t, st.Epochs)
	assert.Nil(t, st.Boundaries)

	var e CaptureEvidence
	AddPackEvidence(&e, &st)
	assert.Equal(t, CaptureEvidence{}, e)
}

// TestReaderStatsFooterNotUsed opens packs whose footer the Reader does not use:
// over MaxFooterLen, invalid under the footer validation, and not finalized.
// Stats reports none, although the forward walk accounts for every record.
func TestReaderStatsFooterNotUsed(t *testing.T) {
	t.Parallel()

	file := richStatsPack(t, captureLow)
	tests := []struct {
		name string
		file []byte
		opts ReaderOptions
	}{
		{name: "footer over MaxFooterLen", file: file, opts: ReaderOptions{MaxFooterLen: 16}},
		{name: "invalid footer", file: invalidFooterFile(t, file)},
		{name: "not finalized", file: planTestPack(t, seg0, captureLow, true, richFooterSteps(t))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := mustOpen(t, tt.file, tt.opts)
			require.Error(t, r.Header().FooterErr)
			var records uint64
			for _, b := range r.Blocks() {
				assert.False(t, b.Indexed)
				records += uint64(b.RecordCount)
			}
			require.Equal(t, uint64(11), records, "the walk accounts for every record")

			st, ok := r.Stats()
			assert.False(t, ok)
			assert.Equal(t, PackStats{}, st)
		})
	}
}

// TestReaderStatsAsStored reads a valid footer whose F-5 count arrays carry extra trailing zeros,
// and whose epoch and boundary entries are stored in reverse order,
// with a second boundary entry at seq 20, of a lower kind, stored after the first:
// the count arrays come as stored, the epochs in ascending epoch,
// and the boundaries in ascending seq, the two of seq 20 in the footer's order although a sort by kind would swap them.
func TestReaderStatsAsStored(t *testing.T) {
	t.Parallel()

	file := richStatsPack(t, captureLow)
	_, decoded, _ := splitPack(t, file)
	ref := footerStats(t, decoded)
	p := splitFooter(t, decoded)

	for _, tag := range []uint16{f5TagKindCounts, f5TagDirCounts, f5TagDecodeStatusCounts} {
		e := nthEntry(t, p.f5, tag, 0)
		p.f5 = replaceEntry(t, p.f5, tag, 0, tlv.BytesEntry(tag, append(slices.Clone(e.Value), make([]byte, 3*8)...)))
	}

	gap := nthEntry(t, p.f5, f5TagBoundary, 1)
	start := withNested(t, gap, boundaryTagKind, tlv.U8Entry(boundaryTagKind, uint8(BoundaryKindStart)))
	start.Tag = f3TagBoundary
	for i := range p.entries {
		if p.entries[i].FirstSeq <= 20 && 20 <= p.entries[i].LastSeq {
			p.f3[i] = append(slices.Clone(p.f3[i]), start)
		}
	}
	start.Tag = f5TagBoundary

	var epochs, boundaries, rest []tlv.Entry
	for _, e := range p.f5 {
		switch e.Tag {
		case f5TagEpoch:
			epochs = append(epochs, e)
		case f5TagBoundary:
			if slices.Equal(e.Value, gap.Value) {
				boundaries = append(boundaries, start)
			}
			boundaries = append(boundaries, e)
		default:
			rest = append(rest, e)
		}
	}
	slices.Reverse(epochs)
	slices.Reverse(boundaries)
	p.f5 = slices.Concat(rest, boundaries, epochs)

	r := mustOpen(t, refooter(t, file, p.encode()), ReaderOptions{})
	require.NoError(t, r.Header().FooterErr)
	got, ok := r.Stats()
	require.True(t, ok)

	want := richStats(captureLow, ref)
	for _, c := range []*[]uint64{&want.KindCounts, &want.DirCounts, &want.DecodeStatusCounts} {
		*c = append(slices.Clone(*c), 0, 0, 0)
	}
	gapEntry := want.Boundaries[1]
	startEntry := gapEntry
	startEntry.Kind = BoundaryKindStart
	want.Boundaries = []Boundary{want.Boundaries[0], gapEntry, startEntry, want.Boundaries[2]}
	assert.Equal(t, want, got)
}

// TestReaderStatsOwnership mutates every slice and pointer of a PackStats, and of evidence folded from another:
// Stats called again returns the original values in fresh storage, and evidence folded earlier is unchanged.
func TestReaderStatsOwnership(t *testing.T) {
	t.Parallel()

	file := richStatsPack(t, captureLow)
	_, decoded, _ := splitPack(t, file)
	want := richStats(captureLow, footerStats(t, decoded))
	r := mustOpen(t, file, ReaderOptions{})

	first, ok := r.Stats()
	require.True(t, ok)
	var e CaptureEvidence
	AddPackEvidence(&e, &first)
	folded := CaptureEvidence{End: EndStopped, Boundaries: slices.Clone(want.Boundaries), Closures: []EpochClosure{{1, 15}, {3, 31}}}
	require.Equal(t, folded, e)

	first.KindCounts[0], first.DirCounts[0], first.DecodeStatusCounts[0] = 99, 99, 99
	first.SeqRanges[0].First = 99
	first.Epochs[0].RecordCount = 99
	*first.Epochs[1].CloseSeq = 99
	first.Boundaries[0].Seq = 99
	*first.Boundaries[1].GapStart, *first.Boundaries[1].GapEnd = 99, 99

	second, ok := r.Stats()
	require.True(t, ok)
	assert.Equal(t, want, second)
	assert.NotSame(t, first.Epochs[1].CloseSeq, second.Epochs[1].CloseSeq)
	assert.NotSame(t, first.Boundaries[1].GapStart, second.Boundaries[1].GapStart)
	assert.Equal(t, folded, e, "the evidence keeps its own pointers")

	var fresh CaptureEvidence
	AddPackEvidence(&fresh, &second)
	*second.Boundaries[1].GapStart, *second.Boundaries[1].GapEnd = 98, 98
	second.Boundaries[1].Seq = 98
	assert.Equal(t, folded, fresh, "evidence folded from a PackStats keeps its own pointers")
	third, _ := r.Stats()
	assert.Equal(t, want, third)
}

// TestAddPackEvidenceFolds folds packs repeated and in every order, holding boundaries that differ at one seq,
// pairs equal but for their epoch, their gap_end or their capture_id, and close_seqs of one epoch:
// the evidence is the same for every order, its boundaries sorted by every field and each distinct one kept once,
// the smallest close_seq per epoch in ascending epoch, and the end stopped-unclean,
// although the clean stop has the larger seq.
func TestAddPackEvidenceFolds(t *testing.T) {
	t.Parallel()

	at := func(v int64) *int64 { return &v }
	seq := func(v uint64) *uint64 { return &v }
	gapNoStart := Boundary{Capture: captureLow, Seq: 5, Kind: BoundaryKindGap, TS: 7, GapEnd: at(3)}
	gapStart1 := Boundary{Capture: captureLow, Seq: 5, Kind: BoundaryKindGap, TS: 7, GapStart: at(1), GapEnd: at(3)}
	gapStart2 := Boundary{Capture: captureLow, Seq: 5, Kind: BoundaryKindGap, TS: 7, GapStart: at(2)}
	startLate := Boundary{Capture: captureLow, Seq: 5, Kind: BoundaryKindStart, TS: 8}
	startEarly := Boundary{Capture: captureLow, Seq: 5, Kind: BoundaryKindStart, TS: 6, Epoch: 1}
	startEpoch0 := Boundary{Capture: captureLow, Seq: 5, Kind: BoundaryKindStart, TS: 6, Epoch: 0}
	gapNoEnd := Boundary{Capture: captureLow, Seq: 5, Kind: BoundaryKindGap, TS: 7, GapStart: at(1)}
	otherCapture := Boundary{Capture: captureHigh, Seq: 5, Kind: BoundaryKindStart, TS: 6, Epoch: 1}
	unclean := Boundary{Capture: captureLow, Seq: 30, Kind: BoundaryKindStopUnclean, TS: 30, Epoch: 3, GapStart: at(4)}
	stop := Boundary{Capture: captureLow, Seq: 50, Kind: BoundaryKindStop, TS: 50, Epoch: 3}

	packs := []PackStats{
		{
			Boundaries: []Boundary{stop, gapStart2, startEarly},
			Epochs:     []EpochStats{{Epoch: 3, CloseSeq: seq(50)}, {Epoch: 2, CloseSeq: seq(45)}, {Epoch: 1}},
		},
		{
			Boundaries: []Boundary{unclean, gapNoStart, startLate, otherCapture, gapNoEnd},
			Epochs:     []EpochStats{{Epoch: 2, CloseSeq: seq(25)}},
		},
		{
			Boundaries: []Boundary{gapStart1, startEarly, startEpoch0, startEarly},
			Epochs:     []EpochStats{{Epoch: 0, CloseSeq: seq(2)}},
		},
	}
	want := CaptureEvidence{
		End: EndStoppedUnclean,
		Boundaries: []Boundary{
			startEpoch0, startEarly, otherCapture, startLate, gapNoStart, gapNoEnd, gapStart1, gapStart2, unclean, stop,
		},
		Closures: []EpochClosure{{Epoch: 0, CloseSeq: 2}, {Epoch: 2, CloseSeq: 25}, {Epoch: 3, CloseSeq: 50}},
	}

	orders := [][]int{{0, 1, 2}, {2, 1, 0}, {1, 0, 2}, {0, 0, 1, 2, 1}, {2, 2, 1, 0, 0}}
	for _, order := range orders {
		var e CaptureEvidence
		for _, i := range order {
			AddPackEvidence(&e, &packs[i])
		}
		assert.Equal(t, want, e, "order %v", order)
	}
}

// TestAddPackEvidenceEnd folds a clean stop and a stop-unclean in both orders, alone and together,
// the clean stop above and below the stop-unclean:
// any stop-unclean makes the end stopped-unclean, else a clean stop makes it stopped, else it is open.
func TestAddPackEvidenceEnd(t *testing.T) {
	t.Parallel()

	stop := PackStats{Boundaries: []Boundary{{Capture: captureLow, Seq: 50, Kind: BoundaryKindStop}}}
	stopBelow := PackStats{Boundaries: []Boundary{{Capture: captureLow, Seq: 10, Kind: BoundaryKindStop}}}
	unclean := PackStats{Boundaries: []Boundary{{Capture: captureLow, Seq: 30, Kind: BoundaryKindStopUnclean}}}
	start := PackStats{Boundaries: []Boundary{{Capture: captureLow, Seq: 1, Kind: BoundaryKindStart}}}

	tests := []struct {
		name  string
		packs []*PackStats
		want  EndState
	}{
		{name: "start only", packs: []*PackStats{&start}, want: EndOpen},
		{name: "clean stop", packs: []*PackStats{&start, &stop}, want: EndStopped},
		{name: "stop-unclean", packs: []*PackStats{&unclean}, want: EndStoppedUnclean},
		{name: "clean stop then stop-unclean", packs: []*PackStats{&stop, &unclean}, want: EndStoppedUnclean},
		{name: "stop-unclean then clean stop", packs: []*PackStats{&unclean, &stop}, want: EndStoppedUnclean},
		{name: "lower clean stop then stop-unclean", packs: []*PackStats{&stopBelow, &unclean}, want: EndStoppedUnclean},
		{name: "stop-unclean then lower clean stop", packs: []*PackStats{&unclean, &stopBelow}, want: EndStoppedUnclean},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var e CaptureEvidence
			for _, s := range tt.packs {
				AddPackEvidence(&e, s)
			}
			assert.Equal(t, tt.want, e.End)
		})
	}
}

// TestAddPackEvidenceNilStats folds a pack without statistics before and after one with them:
// it marks the evidence partial and changes nothing else, and no later fold clears it.
// A nil evidence panics.
func TestAddPackEvidenceNilStats(t *testing.T) {
	t.Parallel()

	stop := PackStats{
		Boundaries: []Boundary{{Capture: captureLow, Seq: 50, Kind: BoundaryKindStop}},
		Epochs:     []EpochStats{{Epoch: 3, CloseSeq: new(uint64(50))}},
	}

	var e CaptureEvidence
	AddPackEvidence(&e, nil)
	assert.Equal(t, CaptureEvidence{Partial: true}, e)
	AddPackEvidence(&e, &stop)
	want := CaptureEvidence{End: EndStopped, Boundaries: stop.Boundaries, Closures: []EpochClosure{{Epoch: 3, CloseSeq: 50}}, Partial: true}
	assert.Equal(t, want, e)

	// Evidence whose End disagrees with its boundaries shows that a nil fold recomputes nothing.
	e.End = EndOpen
	AddPackEvidence(&e, nil)
	want.End = EndOpen
	assert.Equal(t, want, e)

	assert.Panics(t, func() { AddPackEvidence(nil, nil) })
	assert.Panics(t, func() { AddPackEvidence(nil, &stop) })
}
