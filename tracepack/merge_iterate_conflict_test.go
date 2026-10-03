package tracepack

import (
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// TestMergeIterateConsecutiveVersions reads one capture in capture order with a zero filter,
// as a transaction lookup reads each scope:
// every version of a conflicting seq is yielded consecutively, each with Conflict set,
// no other seq between them, whatever the ts_min of the blocks holding them,
// also for a conflicting seq inside a multi-record block.
func TestMergeIterateConsecutiveVersions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		files [][]byte
		want  []yieldView
	}{
		{
			// Seq 6 is the middle record of pack 0's block (ts_min 100), and alone in packs 1 (ts_min 0) and 2 (ts_min 50).
			name: "three versions in three packs",
			files: [][]byte{
				planTestPack(t, seg0, captureLow, false, blockAt(100, 5, 6, 7)),
				planTestPack(t, seg1, captureLow, false, changed(blockAt(0, 6), 1, 6)),
				planTestPack(t, seg2, captureLow, false, changed(blockAt(50, 6), 2, 6)),
			},
			want: []yieldView{
				{pack: 0, seq: 5},
				{pack: 0, seq: 6, conflict: true}, {pack: 1, seq: 6, conflict: true}, {pack: 2, seq: 6, conflict: true},
				{pack: 0, seq: 7},
			},
		},
		{
			// Every seq of both multi-record blocks differs in ts_utc_ns, so each of 5 to 7 has two versions;
			// the block of the later version has the smaller ts_min.
			name: "multi-record blocks",
			files: [][]byte{
				planTestPack(t, seg0, captureLow, false, blockAt(100, 4, 5, 6, 7)),
				planTestPack(t, seg1, captureLow, false, slices.Concat(blockAt(0, 5, 6, 7), blockAt(200, 8))),
			},
			want: []yieldView{
				{pack: 0, seq: 4},
				{pack: 0, seq: 5, conflict: true}, {pack: 1, seq: 5, conflict: true},
				{pack: 0, seq: 6, conflict: true}, {pack: 1, seq: 6, conflict: true},
				{pack: 0, seq: 7, conflict: true}, {pack: 1, seq: 7, conflict: true},
				{pack: 1, block: 1, seq: 8},
			},
		},
		{
			// Two clusters, the second holding seq 9 in three versions, each in a multi-record block.
			name: "three versions in multi-record blocks of a later cluster",
			files: [][]byte{
				planTestPack(t, seg0, captureLow, false, slices.Concat(blockAt(0, 1, 2), blockAt(300, 8, 9, 10))),
				planTestPack(t, seg1, captureLow, false, changed(blockAt(300, 8, 9, 10), 1, 9)),
				planTestPack(t, seg2, captureLow, false, changed(blockAt(300, 8, 9, 10), 2, 9)),
			},
			want: []yieldView{
				{pack: 0, seq: 1}, {pack: 0, seq: 2},
				{pack: 0, block: 1, seq: 8},
				{pack: 0, block: 1, seq: 9, conflict: true}, {pack: 1, seq: 9, conflict: true}, {pack: 2, seq: 9, conflict: true},
				{pack: 0, block: 1, seq: 10},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			items, res, err := collectMergeIterate(t.Context(), openAll(t, tt.files...), Query{}, MergeIterateOptions{Order: OrderCapture})
			require.NoError(t, err)
			assert.Empty(t, res.Incomplete)
			assert.Equal(t, tt.want, yieldViews(items))

			// Each seq's items are one run, all with Conflict set exactly when the seq has several versions.
			conflicted := map[uint64]bool{}
			for _, c := range res.Conflicts {
				conflicted[c.Seq] = true
			}
			done := map[uint64]bool{}
			for i, it := range items {
				seq := it.rec.Seq
				assert.Equal(t, conflicted[seq], it.conflict, "item %d", i)
				if i > 0 && items[i-1].rec.Seq == seq {
					continue
				}
				assert.False(t, done[seq], "seq %d yielded again after another seq", seq)
				done[seq] = true
			}
		})
	}
}

// TestMergeIteratePackIndexesReaders reads packs given out of pack_id order, one with a corrupt block:
// Item.Pack and Defect.Pack index the readers given,
// so readers[i].Header().PackID is the pack_id of the pack holding a record or a defect.
func TestMergeIteratePackIndexesReaders(t *testing.T) {
	t.Parallel()

	corrupt := planTestPack(t, seg0, captureLow, false, timedBlocks(blockTestHour, []uint64{1, 2}, []uint64{3, 4}))
	corrupt = flipByte(corrupt, mustOpen(t, corrupt, ReaderOptions{}).Blocks()[1].Offset+format.EnvelopeLen+3)
	readers := openAll(t,
		planTestPack(t, seg2, captureLow, false, changed(timedBlocks(blockTestHour, []uint64{3, 4}), 1, 4)),
		corrupt,
		planTestPack(t, seg1, captureLow, false, timedBlocks(blockTestHour, []uint64{5, 6})),
	)
	items, res, err := collectMergeIterate(t.Context(), readers, Query{}, MergeIterateOptions{Order: OrderCapture})
	require.NoError(t, err)

	holder := map[uint64]UUID{1: seg0, 2: seg0, 3: seg2, 4: seg2, 5: seg1, 6: seg1}
	got := map[uint64]UUID{}
	for _, it := range items {
		got[it.rec.Seq] = readers[it.pack].Header().PackID
	}
	assert.Equal(t, holder, got)
	require.Len(t, res.Incomplete, 1)
	assert.Equal(t, ReasonCorruptBlock, res.Incomplete[0].Reason)
	assert.Equal(t, seg0, readers[res.Incomplete[0].Pack].Header().PackID)
	assert.Equal(t, 1, res.Incomplete[0].Block)
}

// TestMergeIterateReserveConflict reads, in each order, three conflicts, one of them in three versions,
// with a reserveConflict that admits a given number of conflicts:
// it is called once per conflict, also when the filter selects no version, in place of the check against MaxConflicts,
// and its error ends the read, wrapped, with the conflicts reserved before it listed.
func TestMergeIterateReserveConflict(t *testing.T) {
	t.Parallel()

	steps := hourSteps(10, 14, 5)
	readers := openAll(t,
		mergePack(t, seg0, nil, steps),
		mergePack(t, seg1, nil, changed(steps, 1, 11, 12, 13)),
		mergePack(t, seg2, nil, changed(steps, 2, 12)),
	)
	conflicts := []Conflict{
		{CaptureID: viewCapture, Seq: 11, Versions: [][]UUID{{seg0, seg2}, {seg1}}},
		{CaptureID: viewCapture, Seq: 12, Versions: [][]UUID{{seg0}, {seg1}, {seg2}}},
		{CaptureID: viewCapture, Seq: 13, Versions: [][]UUID{{seg0, seg2}, {seg1}}},
	}
	errAllowance := fmt.Errorf("allowance spent: %w", ErrReadLimit)
	tests := []struct {
		name string
		// allow is the number of conflicts reserveConflict admits.
		allow        int
		maxConflicts int
		q            Query
		want         []Conflict
		wantItems    int
		wantErr      bool
	}{
		{name: "within the allowance, over MaxConflicts", allow: 3, maxConflicts: 1, want: conflicts, wantItems: 9},
		{name: "one conflict over the allowance", allow: 2, maxConflicts: 10, want: conflicts[:2], wantErr: true},
		{
			name: "no version selected", allow: 3, maxConflicts: 1,
			// No footer states system bytes, so the filter excludes no cluster and rejects every record.
			q: Query{Filter: Filter{SystemBytes: &[4]byte{1, 2, 3, 4}}}, want: conflicts,
		},
	}
	for _, order := range []Order{OrderCapture, OrderTime} {
		for _, tt := range tests {
			t.Run(fmt.Sprintf("%v/%s", order, tt.name), func(t *testing.T) {
				t.Parallel()

				calls := 0
				reserve := func() error {
					calls++
					if calls > tt.allow {
						return errAllowance
					}

					return nil
				}
				opts := MergeIterateOptions{Order: order, MaxConflicts: tt.maxConflicts}
				items := 0
				res, err := mergeIterateWith(t.Context(), readers, tt.q, opts, mergeIterateOptions{reserveConflict: reserve}, func(*Item) error {
					items++

					return nil
				})
				assert.Equal(t, tt.want, res.Conflicts)
				if tt.wantErr {
					require.ErrorIs(t, err, errAllowance)
					require.ErrorIs(t, err, ErrReadLimit)
					require.ErrorContains(t, err, "the conflict at seq 13 of capture "+viewCapture.String()+": allowance spent")
					assert.Equal(t, tt.allow+1, calls)

					return
				}
				require.NoError(t, err)
				assert.Equal(t, len(tt.want), calls, "one call per conflict")
				if tt.wantItems > 0 {
					assert.Equal(t, tt.wantItems, items, "seqs 10 and 14 once, 11 and 13 in two versions, 12 in three")
				} else {
					assert.Zero(t, items)
				}
			})
		}
	}
}
