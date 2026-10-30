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
			{CaptureID: capture, Seq: 10, Kind: BoundaryKindStart, TS: h + 50, Epoch: 0},
			{CaptureID: capture, Seq: 20, Kind: BoundaryKindGap, TS: h + 300, Epoch: 0, GapStart: &gapStart, GapEnd: &gapEnd},
			{CaptureID: capture, Seq: 31, Kind: BoundaryKindStop, TS: h1 + 4, Epoch: 3},
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

// TestReaderStatsOwnership mutates every slice and pointer of a PackStats:
// Stats called again returns the original values in fresh storage.
func TestReaderStatsOwnership(t *testing.T) {
	t.Parallel()

	file := richStatsPack(t, captureLow)
	_, decoded, _ := splitPack(t, file)
	want := richStats(captureLow, footerStats(t, decoded))
	r := mustOpen(t, file, ReaderOptions{})

	first, ok := r.Stats()
	require.True(t, ok)

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

	*second.Boundaries[1].GapStart, *second.Boundaries[1].GapEnd = 98, 98
	second.Boundaries[1].Seq = 98
	third, _ := r.Stats()
	assert.Equal(t, want, third)
}
