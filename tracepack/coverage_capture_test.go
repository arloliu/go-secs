package tracepack

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// coverageThirdCapture is a capture beside captureLow and captureHigh that no coverage entry of these tests names.
var coverageThirdCapture = UUID{0xC2}

// coverageCapturePack writes the records of seqs 0 to 2, at blockTestHour + seq, into one finalized block
// of a pack id of capture whose pack metadata carries entries.
func coverageCapturePack(t testing.TB, id, capture UUID, entries []Coverage) []byte {
	t.Helper()

	steps := txBlock(testDataRecord(0, blockTestHour, 1), testDataRecord(1, blockTestHour+1, 1), testDataRecord(2, blockTestHour+2, 1))

	return withIDs(t, writeRepairPack(t, CodecZstd, func(m *PackMeta) { m.Coverage = entries }, false, steps).file, id, capture)
}

// TestCoverageOtherCapture reads a pack A of captureLow holding coverage entries,
// one of captureHigh (B) over a time range past every record, one without capture_id, one of B inverted,
// and one of a capture no pack holds,
// alone and beside other packs (the tracepack semantics specification §7.4):
// an entry counts when its capture, its capture_id or else the capture of the pack holding it,
// is the capture of a pack the read reads, and the entry meets the query;
// its defect locates the pack holding it, A, whatever the order of the readers.
// A read of A alone, or of A beside a pack of a third capture, reports only the entry without capture_id;
// B's entries count beside B's pack, also when the query excludes every block of B,
// and the inverted one counts whatever the time range.
func TestCoverageOtherCapture(t *testing.T) {
	t.Parallel()

	h := blockTestHour
	entries := []Coverage{
		{CaptureID: new(captureHigh), TimeStart: new(h + 1000), TimeEnd: new(h + 1500)},
		{},
		{CaptureID: new(captureHigh), TimeStart: new(h + 500), TimeEnd: new(h + 100)},
		{CaptureID: new(UUID{0xC9})},
	}
	a := coverageCapturePack(t, seg0, captureLow, entries)
	b := coverageCapturePack(t, seg1, captureHigh, nil)
	c := coverageCapturePack(t, seg2, coverageThirdCapture, nil)

	t.Run("Iterate of A", func(t *testing.T) {
		t.Parallel()

		run := iterate(t, mustOpen(t, a, ReaderOptions{}), Query{})
		assert.Equal(t, []int{1}, coverageHits(t, &run.res, entries))
	})

	tests := []struct {
		name     string
		files    [][]byte
		from, to *int64
		// holder is the index of A among the readers.
		holder int
		want   []int
		// pruned reports that the query excludes every block, so the read yields nothing.
		pruned bool
	}{
		{name: "A alone", files: [][]byte{a}, want: []int{1}},
		{name: "A and B", files: [][]byte{a, b}, want: []int{0, 1, 2}},
		{name: "B and A", files: [][]byte{b, a}, holder: 1, want: []int{0, 1, 2}},
		{name: "A and a third capture", files: [][]byte{a, c}, want: []int{1}},
		{name: "A, B and a third capture", files: [][]byte{c, b, a}, holder: 2, want: []int{0, 1, 2}},
		{name: "A and B, every block pruned", files: [][]byte{a, b}, from: new(h + 1200), to: new(h + 1300), want: []int{0, 1, 2}, pruned: true},
		{name: "A and B, past every entry's time range", files: [][]byte{a, b}, from: new(h + 3000), to: new(h + 4000), want: []int{1, 2}, pruned: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			readers := openAll(t, tt.files...)
			items, res, err := collectMergeIterate(t.Context(), readers, Query{Filter: Filter{TimeFrom: tt.from, TimeTo: tt.to}},
				MergeIterateOptions{Order: OrderCapture})
			require.NoError(t, err)
			assert.Equal(t, tt.want, coverageHits(t, &res, entries))
			for _, d := range res.Incomplete {
				assert.Equal(t, tt.holder, d.Pack, "the defect locates the pack holding the entry")
			}
			if tt.pruned {
				assert.Empty(t, items)
			} else {
				assert.Len(t, items, 3*len(tt.files))
			}
		})
	}
}

// TestFindTransactionCoverageOtherCapture looks up the primary at 12 of captureLow
// beside a pack of captureHigh, without records, holding a coverage entry that names captureLow:
// the entry is the primary's capture's,
// so it counts in the hour's read (the tracepack semantics specification §7.2, §7.4).
// Over the window (12, 16) it is a TxGapCoverage gap that locates the pack holding it;
// over a missing primary it explains the miss, as an entry of the primary's own pack does,
// and the no-key gap of the early return carries it.
// An entry of captureHigh's own in that pack explains nothing.
func TestFindTransactionCoverageOtherCapture(t *testing.T) {
	t.Parallel()

	foreign := func(c Coverage) []byte {
		return withIDs(t, txPack(t, seg1, func(m *PackMeta) { m.Coverage = []Coverage{c} }, nil), seg1, captureHigh)
	}

	t.Run("window", func(t *testing.T) {
		t.Parallel()

		cov := Coverage{CaptureID: new(captureLow), SeqFirst: new(uint64(14)), SeqLast: new(uint64(14))}
		own := txPack(t, seg0, nil, txBlock(txRecord(12, nil), txNote(13, nil), txNote(14, nil), txNote(15, nil), txRecord(16, nil)))
		res, err := findTx(t, t.Context(), txSource(t, own, foreign(cov)), txKeyAt(12), TxOptions{MaxHours: 1})
		require.NoError(t, err)
		assert.Equal(t, TxNotFound, res.Outcome)
		require.Len(t, res.Incomplete, 1)
		g := res.Incomplete[0]
		assert.Equal(t, TxGap{
			Reason: TxGapCoverage, Hours: []int64{memTestHour}, Pack: new(seg1), Block: -1, Offset: -1, Coverage: &cov, Err: g.Err,
		}, g)
		require.Error(t, g.Err)
	})

	missing := txPack(t, seg0, nil, txBlock(txSeqs(10, 11, 13)...))
	t.Run("missing primary", func(t *testing.T) {
		t.Parallel()

		cov := Coverage{CaptureID: new(captureLow), SeqFirst: new(uint64(12)), SeqLast: new(uint64(12))}
		s := txSource(t, missing, foreign(cov))
		res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxHours: 3})
		requireOneScopeRead(t, s)
		require.NoError(t, err)
		assert.Equal(t, TxNotFound, res.Outcome)
		assert.Equal(t, []TxGapReason{TxGapNoKey}, gapReasons(res.Incomplete))
		noKey := res.Incomplete[0]
		assert.Equal(t, []int64{memTestHour}, noKey.Hours)
		assert.Equal(t, new(uint64(12)), noKey.Seq)
		assert.Equal(t, &cov, noKey.Coverage)
		assert.Empty(t, res.Records)
		assert.Equal(t, []TxHour{{Hour: memTestHour, Packs: []UUID{seg0, seg1}}}, res.Searched)
	})

	t.Run("missing primary, an entry of the other capture", func(t *testing.T) {
		t.Parallel()

		s := txSource(t, missing, foreign(Coverage{CaptureID: new(captureHigh)}))
		res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxHours: 3})
		requireOneScopeRead(t, s)
		require.ErrorIs(t, err, ErrNotPrimary)
		assert.Empty(t, res.Incomplete)
	})
}

// TestFindTransactionCoverageAcrossHours looks up the primary at 12 of captureLow
// where an hour's read holds a pack of captureHigh, without records, but no pack of captureLow,
// and that pack holds a coverage entry:
// the lookup evaluates every entry of the primary's capture in every pack it reads
// (the tracepack semantics specification §7.2),
// so an entry naming captureLow counts even though the read of its hour does not report it.
// In the hour after the window (12, 16), over seq 14, it is a TxGapCoverage gap of that hour that locates its pack;
// an entry of captureHigh, named or without capture_id, is none.
// In the primary's hour, which then holds no pack of captureLow, it explains the missing primary.
func TestFindTransactionCoverageAcrossHours(t *testing.T) {
	t.Parallel()

	foreignIn := func(hours int64, c Coverage) []byte {
		return withIDs(t, txPackIn(t, seg1, hours, func(m *PackMeta) { m.Coverage = []Coverage{c} }, nil), seg1, captureHigh)
	}
	own := txPack(t, seg0, nil, txBlock(txRecord(12, nil), txNote(13, nil), txNote(14, nil), txNote(15, nil), txRecord(16, nil)))
	seq14 := func(capture *UUID) Coverage {
		return Coverage{CaptureID: capture, SeqFirst: new(uint64(14)), SeqLast: new(uint64(14))}
	}

	tests := []struct {
		name string
		cov  Coverage
		gap  bool
	}{
		{name: "the primary's capture", cov: seq14(new(captureLow)), gap: true},
		{name: "the other capture, named", cov: seq14(new(captureHigh))},
		{name: "the other capture, without capture_id", cov: seq14(nil)},
	}
	for _, tt := range tests {
		t.Run("window, "+tt.name, func(t *testing.T) {
			t.Parallel()

			s := txHours(t, [][]byte{own}, [][]byte{foreignIn(1, tt.cov)})
			res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxHours: 2})
			require.NoError(t, err)
			assert.Equal(t, TxNotFound, res.Outcome)
			assert.Equal(t, []TxHour{{Hour: memTestHour, Packs: []UUID{seg0}}, {Hour: memTestHour + 1, Packs: []UUID{seg1}}}, res.Searched)
			if !tt.gap {
				assert.Empty(t, res.Incomplete)

				return
			}
			require.Len(t, res.Incomplete, 1)
			g := res.Incomplete[0]
			assert.Equal(t, TxGap{
				Reason: TxGapCoverage, Hours: []int64{memTestHour + 1}, Pack: new(seg1), Block: -1, Offset: -1, Coverage: &tt.cov, Err: g.Err,
			}, g)
			require.Error(t, g.Err)
		})
	}

	t.Run("missing primary, no pack of its capture", func(t *testing.T) {
		t.Parallel()

		cov := Coverage{CaptureID: new(captureLow), SeqFirst: new(uint64(12)), SeqLast: new(uint64(12))}
		s := txSource(t, foreignIn(0, cov))
		res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxHours: 3})
		requireOneScopeRead(t, s)
		require.NoError(t, err)
		assert.Equal(t, TxNotFound, res.Outcome)
		assert.Equal(t, []TxGapReason{TxGapNoKey}, gapReasons(res.Incomplete))
		assert.Equal(t, &cov, res.Incomplete[0].Coverage)
		assert.Equal(t, []TxHour{{Hour: memTestHour, Packs: []UUID{seg1}}}, res.Searched)
	})

	t.Run("missing primary, no pack of its capture, an entry of the other capture", func(t *testing.T) {
		t.Parallel()

		s := txSource(t, foreignIn(0, Coverage{CaptureID: new(captureHigh)}))
		_, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxHours: 3})
		requireOneScopeRead(t, s)
		require.ErrorIs(t, err, ErrNotPrimary)
	})
}
