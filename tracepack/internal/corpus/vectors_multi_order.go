package corpus

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// Offsets of the record header fields the order and identity vectors compare (the tracepack format specification §7.1).
const (
	recordSeqOff        = 0
	recordTSOff         = 8
	recordPayloadLenOff = 28
	recordDirOff        = 39
)

// multiOrderVectors returns the recipes of the multi group that pin the order of a read over several packs
// and the identity of the records it compares (the tracepack corpus specification §9.4).
func multiOrderVectors() []Recipe {
	return []Recipe{
		{
			ID: "multi-time-backward", Title: "timestamps running backward within a block, across blocks and across packs, read in time order",
			Cites: []string{"SEM §7.4", "SEM §9", "CORPUS §5.10"}, Class: ClassMultiPack,
			Build: timeBackwardBuild,
			// Time order takes the clusters by ts_min: B's block (1), A's block 1 (2), A's block 0 (3).
			// Before it reads each, it yields the records already read whose ts is below that cluster's ts_min:
			// B's seq 1 (1) before A's block 1, A's seq 4 (2) before A's block 0; then every remaining record by ts.
			Expect: &Expectation{
				Packs: []*Expectation{finalizedPack(2, 5), finalizedPack(1, 2)},
				Reads: []ReadWant{{ID: "time-order", Items: []ItemWant{
					{Seq: 1, Pack: 1, Block: 0}, {Seq: 4, Pack: 0, Block: 1}, {Seq: 2, Pack: 0, Block: 0}, {Seq: 3, Pack: 0, Block: 0},
					{Seq: 1, Pack: 0, Block: 0}, {Seq: 5, Pack: 0, Block: 1}, {Seq: 2, Pack: 1, Block: 0},
				}}},
			},
		},
		{
			ID: "multi-equal-timestamps", Title: "equal timestamps across captures, equal ts_min across clusters, and two and three versions of one record at one timestamp",
			Cites: []string{"SEM §7.4", "SEM §9", "CORPUS §5.10"}, Class: ClassMultiPack,
			Build: equalTimestampsBuild,
			// Time order is by (ts_utc_ns, capture_id, seq), then version.
			// Equal timestamps across captures: capture A's two records before B's, A's capture_id being the lower.
			// Equal ts_min across clusters: the records of both clusters by ts, then seq, not cluster by cluster.
			// Equal timestamps across versions: the versions of seq 7 in version order, the order of the first block holding each
			// in cluster order: pack 4's block first, its first seq 6 below the others' 7, then the packs in the order given.
			// Equal ts_min across clusters of one capture, each holding a conflict: the clusters are read by first seq,
			// so the conflict on seq 5 is listed before the one on seq 7, though pack 6, given first, holds only seqs 7 and 8.
			Expect: &Expectation{
				Packs: []*Expectation{
					finalizedPack(1, 2), finalizedPack(1, 2), packOf(2, 5, 6, 7, 8),
					packOf(1, 7), packOf(1, 6, 7), packOf(1, 7),
					packOf(1, 7, 8), packOf(2, 5, 6, 7, 8), packOf(1, 5, 6),
				},
				Reads: []ReadWant{
					{ID: "captures-tie", Items: slices.Concat(blockItems(0, 0, 1, 2), blockItems(1, 0, 1, 2))},
					{ID: "clusters-tie", Items: []ItemWant{
						{Seq: 5, Pack: 2, Block: 0}, {Seq: 7, Pack: 2, Block: 1}, {Seq: 6, Pack: 2, Block: 0}, {Seq: 8, Pack: 2, Block: 1},
					}},
					{
						ID: "clusters-tie-conflicts",
						Items: []ItemWant{
							{Seq: 5, Pack: 7, Block: 0, Conflict: true}, {Seq: 5, Pack: 8, Block: 0, Conflict: true},
							{Seq: 7, Pack: 6, Block: 0, Conflict: true}, {Seq: 7, Pack: 7, Block: 1, Conflict: true},
							{Seq: 6, Pack: 7, Block: 0}, {Seq: 8, Pack: 6, Block: 0},
						},
						Conflicts: []ConflictWant{{Capture: 3, Seq: 5, Versions: [][]int{{7}, {8}}}, {Capture: 3, Seq: 7, Versions: [][]int{{6}, {7}}}},
					},
					{
						ID: "versions-three",
						Items: []ItemWant{
							{Seq: 6, Pack: 4, Block: 0}, {Seq: 7, Pack: 4, Block: 0, Conflict: true},
							{Seq: 7, Pack: 3, Block: 0, Conflict: true}, {Seq: 7, Pack: 5, Block: 0, Conflict: true},
						},
						Conflicts: []ConflictWant{{Capture: 2, Seq: 7, Versions: [][]int{{4}, {3}, {5}}}},
					},
					{
						ID:        "versions-two",
						Items:     []ItemWant{{Seq: 6, Pack: 4, Block: 0}, {Seq: 7, Pack: 4, Block: 0, Conflict: true}, {Seq: 7, Pack: 3, Block: 0, Conflict: true}},
						Conflicts: []ConflictWant{{Capture: 2, Seq: 7, Versions: [][]int{{4}, {3}}}},
					},
				},
			},
		},
		{
			ID: "multi-conflict-selection", Title: "a conflict of two versions differing in dir, with both, one and no version selected, listed each time",
			Cites: []string{"SEM §7.4", "SEM §9", "CORPUS §5.10"}, Class: ClassMultiPack,
			Build: conflictSelectionBuild,
			// The copies of seq 2 differ in dir, so the record is a conflict of two versions, pack 0's equipment-to-host and pack 1's host-to-equipment,
			// listed whether or not the filter selects a version; each version the filter selects is yielded once, marked as a conflict.
			// With annotations only, seq 1 selected keeps the cluster from being excluded, so seq 2 is compared.
			Expect: &Expectation{
				Packs: []*Expectation{finalizedPack(1, 3), finalizedPack(1, 3)},
				Reads: []ReadWant{
					{
						ID: "both-selected",
						Items: []ItemWant{
							{Seq: 1, Pack: 0, Block: 0}, {Seq: 2, Pack: 0, Block: 0, Conflict: true}, {Seq: 2, Pack: 1, Block: 0, Conflict: true},
							{Seq: 3, Pack: 0, Block: 0},
						},
						Conflicts: selectionConflict,
					},
					{ID: "none-selected", Items: blockItems(0, 0, 1), Conflicts: selectionConflict},
					{
						ID:        "one-selected",
						Items:     []ItemWant{{Seq: 2, Pack: 1, Block: 0, Conflict: true}, {Seq: 3, Pack: 0, Block: 0}},
						Conflicts: selectionConflict,
					},
				},
			},
		},
		{
			ID: "multi-conflict-time-range", Title: "two versions of one seq with different timestamps and a time range selecting one: both read, the conflict listed",
			Cites: []string{"SEM §7.4", "SEM §9", "CORPUS §5.10"}, Class: ClassMultiPack,
			Build: conflictTimeRangeBuild,
			// The two blocks overlap in seq 3, so they form one cluster, which pack 0's block keeps from being excluded:
			// both are read and compared, and pack 0's version of seq 3, the one inside the range, is yielded as a conflict.
			Expect: &Expectation{
				Packs: []*Expectation{finalizedPack(1, 3), packOf(1, 3)},
				Reads: []ReadWant{{
					ID:        "time-selects-one",
					Items:     []ItemWant{{Seq: 1, Pack: 0, Block: 0}, {Seq: 2, Pack: 0, Block: 0}, {Seq: 3, Pack: 0, Block: 0, Conflict: true}},
					Conflicts: []ConflictWant{{Capture: 0, Seq: 3, Versions: [][]int{{0}, {1}}}},
				}},
			},
		},
		{
			ID: "multi-cluster-two-hours", Title: "a cluster spanning two hours, in capture and time order",
			Cites: []string{"SEM §7.4", "SEM §9", "CORPUS §5.10"}, Class: ClassMultiPack,
			Build: clusterTwoHoursBuild,
			// The blocks overlap in seq range across the two hours' packs, so they form one cluster:
			// capture order yields its records by seq, time order by timestamp, seq 4 of the first hour before seqs 2 and 3 of the second.
			Expect: &Expectation{
				Packs: []*Expectation{packOf(1, 1, 4), packOf(1, 2, 3)},
				Reads: []ReadWant{
					{ID: "capture-order", Items: []ItemWant{
						{Seq: 1, Pack: 0, Block: 0}, {Seq: 2, Pack: 1, Block: 0}, {Seq: 3, Pack: 1, Block: 0}, {Seq: 4, Pack: 0, Block: 0},
					}},
					{ID: "time-order", Items: []ItemWant{
						{Seq: 1, Pack: 0, Block: 0}, {Seq: 4, Pack: 0, Block: 0}, {Seq: 2, Pack: 1, Block: 0}, {Seq: 3, Pack: 1, Block: 0},
					}},
				},
			},
		},
		{
			ID: "multi-unfinalized-beside-archive", Title: "an unfinalized segment beside its archive, read in both orders",
			Cites: []string{"SEM §7.4", "SEM §9", "CORPUS §5.10"}, Class: ClassMultiPack,
			Build: unfinalizedBesideArchiveBuild,
			// The records are identical, so no conflict; each is yielded once from its representative,
			// the first block holding it in cluster order (ascending first seq, then the order the packs are given):
			// the archive's block, first seq 1, comes before the segment's block 1, first seq 3, in both orders.
			// The segment is not finalized: truncated at its end, and its footer, which it does not have, is not used.
			Expect: &Expectation{
				Packs: []*Expectation{
					{Outcome: tracepack.OutcomeUnfinalized, Unfinalized: true, Blocks: 2, Seqs: seqRange(1, 4), PrefixEnd: AtEnd()},
					finalizedPack(1, 4),
				},
				Reads: []ReadWant{
					{
						ID: "archive-first", Items: blockItems(1, 0, 1, 2, 3, 4),
						Incomplete: unfinalizedSegment, FooterErrors: []int{0},
					},
					{
						ID: "segment-first", Items: slices.Concat(blockItems(0, 0, 1, 2), blockItems(1, 0, 3, 4)),
						Incomplete: unfinalizedSegment, FooterErrors: []int{0},
					},
				},
			},
		},
	}
}

// selectionConflict is the conflict every read of multi-conflict-selection lists: seq 2 of capture A, pack 0's version, then pack 1's.
var selectionConflict = []ConflictWant{{Capture: 0, Seq: 2, Versions: [][]int{{0}, {1}}}}

// unfinalizedSegment is the incomplete reason of each read of multi-unfinalized-beside-archive:
// pack 0 is not finalized, truncated at the end of its last block, the end of the pack.
var unfinalizedSegment = []PackIncompleteWant{{Pack: 0, IncompleteWant: Truncated(AtEnd())}}

// timeBackwardBuild builds multi-time-backward:
// pack 0 of capture A, block 0 holding seqs 1 to 3 at 5, 3 and 4 ms and block 1 seqs 4 and 5 at 2 and 6 ms,
// and pack 1 of capture B, seqs 1 and 2 at 1 and 7 ms.
// It checks that the timestamps run backward within block 0, from block 0 to block 1, and from pack 0 to pack 1,
// and that no two records share one.
func timeBackwardBuild(seed string) (*Built, error) {
	packs, err := writeMultiPacks(seed, []multiPack{
		{capture: 0, blocks: [][]tracepack.Record{
			{multiData(1, msAt(5), nil), multiData(2, msAt(3), nil), multiData(3, msAt(4), nil)},
			{multiData(4, msAt(2), nil), multiData(5, msAt(6), nil)},
		}},
		{capture: 1, blocks: [][]tracepack.Record{{multiData(1, msAt(1), nil), multiData(2, msAt(7), nil)}}},
	})
	if err != nil {
		return nil, err
	}
	c, err := packCopies(packs)
	if err != nil {
		return nil, err
	}
	a, b := c[0], c[1]
	var ts []int64
	for _, p := range c {
		for _, r := range p {
			ts = append(ts, r.ts)
		}
	}
	slices.Sort(ts)
	switch {
	case a[2].ts >= a[1].ts:
		return nil, errors.New("corpus: the timestamps of block 0 do not run backward")
	case a[4].block != 1 || a[4].ts >= min(a[1].ts, a[2].ts, a[3].ts):
		return nil, errors.New("corpus: block 1 does not start below block 0's timestamps")
	case b[1].ts >= min(a[4].ts, a[2].ts):
		return nil, errors.New("corpus: pack 1 does not hold a timestamp below pack 0's")
	case len(slices.Compact(ts)) != len(ts):
		return nil, errors.New("corpus: two records share a timestamp")
	}

	return &Built{Packs: packs, Reads: []ReadSpec{
		{ID: "time-order", Cites: readCites, Packs: []int{0, 1}, Order: ReadOrderTime},
	}}, nil
}

// equalTimestampsBuild builds multi-equal-timestamps:
// pack 0 of capture A and pack 1 of capture B, each seqs 1 and 2 at 5 ms;
// pack 2 of capture A, in the next minute, block 0 holding seqs 5 and 6 and block 1 seqs 7 and 8, at 10 and 11 ms each;
// packs 3, 4 and 5 of capture C, in its first three minutes, each holding a version of seq 7 at 20 ms,
// the versions differing in their message text alone, and pack 4 also seq 6 at 20 ms, in the block of its seq 7;
// and packs 6, 7 and 8 of capture D, in its first three minutes, every block's records at 30 and 31 ms:
// pack 6 one block of seqs 7 and 8, pack 7 a block of seqs 5 and 6 and a block of seqs 7 and 8, pack 8 one block of seqs 5 and 6,
// the copies of seq 7 in packs 6 and 7 and of seq 5 in packs 7 and 8 differing in their message text alone.
// It checks that A's capture_id is below B's, the two clusters of pack 2 have one ts_min,
// the three copies of seq 7 have one timestamp and differ in their payloads alone, pack 4's seq 6 shares their timestamp and its seq 7's block,
// and that capture D's two clusters have one ts_min, their copies of seqs 5 and 7 differ in their payloads alone and the others are the same bytes.
func equalTimestampsBuild(seed string) (*Built, error) {
	if a, b := captureOf(seed, 0), captureOf(seed, 1); bytes.Compare(a[:], b[:]) >= 0 {
		return nil, errors.New("corpus: capture A's capture_id is not below capture B's")
	}
	at := msAt(5)
	seven := func(c byte) tracepack.Record { return multiData(7, msAt(20), asciiItem(c)) }
	packs, err := writeMultiPacks(seed, []multiPack{
		{capture: 0, blocks: [][]tracepack.Record{{multiData(1, at, nil), multiData(2, at, nil)}}},
		{capture: 1, blocks: [][]tracepack.Record{{multiData(1, at, nil), multiData(2, at, nil)}}},
		{capture: 0, minute: 1, blocks: [][]tracepack.Record{
			{multiData(5, msAt(10), nil), multiData(6, msAt(11), nil)},
			{multiData(7, msAt(10), nil), multiData(8, msAt(11), nil)},
		}},
		{capture: 2, blocks: [][]tracepack.Record{{seven('x')}}},
		{capture: 2, minute: 1, blocks: [][]tracepack.Record{{multiData(6, msAt(20), asciiItem('w')), seven('y')}}},
		{capture: 2, minute: 2, blocks: [][]tracepack.Record{{seven('z')}}},
		{capture: 3, blocks: [][]tracepack.Record{{multiData(7, msAt(30), asciiItem('a')), multiData(8, msAt(31), nil)}}},
		{capture: 3, minute: 1, blocks: [][]tracepack.Record{
			{multiData(5, msAt(30), asciiItem('c')), multiData(6, msAt(31), nil)},
			{multiData(7, msAt(30), asciiItem('b')), multiData(8, msAt(31), nil)},
		}},
		{capture: 3, minute: 2, blocks: [][]tracepack.Record{{multiData(5, msAt(30), asciiItem('d')), multiData(6, msAt(31), nil)}}},
	})
	if err != nil {
		return nil, err
	}
	c, err := packCopies(packs)
	if err != nil {
		return nil, err
	}
	if c[2][5].block == c[2][7].block || c[2][5].ts != c[2][7].ts {
		return nil, errors.New("corpus: pack 2's blocks do not have one ts_min")
	}
	versions := []storedCopy{c[3][7], c[4][7], c[5][7]}
	for i := range versions {
		for j := range i {
			if !onlyPayloadDiffers(versions[i], versions[j]) {
				return nil, fmt.Errorf("corpus: the copies of seq 7 in packs %d and %d do not differ in their payloads alone", 3+j, 3+i)
			}
		}
	}
	if c[4][6].block != c[4][7].block || c[4][6].ts != c[4][7].ts {
		return nil, errors.New("corpus: pack 4's seq 6 does not share its seq 7's block and timestamp")
	}
	x, y, z := c[6], c[7], c[8]
	switch {
	case y[5].block == y[7].block || y[5].ts != y[7].ts || x[7].ts != y[7].ts || z[5].ts != y[5].ts:
		return nil, errors.New("corpus: capture D's clusters do not have one ts_min")
	case !onlyPayloadDiffers(x[7], y[7]) || !onlyPayloadDiffers(y[5], z[5]):
		return nil, errors.New("corpus: capture D's copies of seqs 5 and 7 do not differ in their payloads alone")
	case !x[8].same(y[8]) || !y[6].same(z[6]):
		return nil, errors.New("corpus: capture D's copies of seqs 6 and 8 are not the same bytes")
	}

	return &Built{Packs: packs, Reads: []ReadSpec{
		{ID: "captures-tie", Cites: readCites, Packs: []int{0, 1}, Order: ReadOrderTime},
		{ID: "clusters-tie", Cites: readCites, Packs: []int{2}, Order: ReadOrderTime},
		{ID: "clusters-tie-conflicts", Cites: readCites, Packs: []int{6, 7, 8}, Order: ReadOrderTime},
		{ID: "versions-three", Cites: readCites, Packs: []int{3, 4, 5}, Order: ReadOrderTime},
		{ID: "versions-two", Cites: readCites, Packs: []int{3, 4}, Order: ReadOrderTime},
	}}, nil
}

// conflictSelectionBuild builds multi-conflict-selection:
// two segments of capture A, in the first and second minutes of the hour, each one block of seqs 1 to 3:
// seq 1 an annotation and seq 3 a host-to-equipment data record, the same in both,
// and seq 2 a data record equipment-to-host in pack 0 and host-to-equipment in pack 1.
// It checks that the copies of seqs 1 and 3 are the same bytes and those of seq 2 differ in dir alone.
func conflictSelectionBuild(seed string) (*Built, error) {
	note, err := multiNote(1, msAt(1), "a")
	if err != nil {
		return nil, err
	}
	block := func(dir tracepack.Dir) []tracepack.Record {
		two := multiData(2, msAt(2), nil)
		two.Dir = dir

		return []tracepack.Record{note, two, multiData(3, msAt(3), nil)}
	}
	packs, err := writeMultiPacks(seed, []multiPack{
		{capture: 0, blocks: [][]tracepack.Record{block(tracepack.DirEquipmentToHost)}},
		{capture: 0, minute: 1, blocks: [][]tracepack.Record{block(tracepack.DirHostToEquipment)}},
	})
	if err != nil {
		return nil, err
	}
	c, err := packCopies(packs)
	if err != nil {
		return nil, err
	}
	switch {
	case !c[0][1].same(c[1][1]) || !c[0][3].same(c[1][3]):
		return nil, errors.New("corpus: the copies of seqs 1 and 3 are not the same bytes")
	case !slices.Equal(c[0][2].headerDiff(c[1][2]), []int{recordDirOff}) || !bytes.Equal(c[0][2].payload, c[1][2].payload):
		return nil, errors.New("corpus: the copies of seq 2 do not differ in dir alone")
	}

	reads := []ReadSpec{
		{ID: "both-selected", Filter: Filter{}},
		{ID: "none-selected", Filter: Filter{Kinds: []string{tracepack.KindAnnotation.String()}}},
		{ID: "one-selected", Filter: Filter{Dirs: []string{tracepack.DirHostToEquipment.String()}}},
	}
	for i := range reads {
		reads[i].Cites, reads[i].Packs, reads[i].Order = readCites, []int{0, 1}, ReadOrderCapture
	}

	return &Built{Packs: packs, Reads: reads}, nil
}

// conflictTimeRangeBuild builds multi-conflict-time-range:
// pack 0 of capture A, seqs 1 to 3 at 1 ms, 2 ms and 60,000 ms,
// and pack 1 of capture A, in the next minute, seq 3 alone at 1,800,000 ms, otherwise the same record;
// the read selects [0, 600,000) ms.
// It checks that the copies of seq 3 differ in ts_utc_ns alone, pack 0's inside the range and pack 1's outside it.
func conflictTimeRangeBuild(seed string) (*Built, error) {
	const to = 600_000
	packs, err := writeMultiPacks(seed, []multiPack{
		{capture: 0, blocks: [][]tracepack.Record{{multiData(1, msAt(1), nil), multiData(2, msAt(2), nil), multiData(3, msAt(60_000), nil)}}},
		{capture: 0, minute: 1, blocks: [][]tracepack.Record{{multiData(3, msAt(1_800_000), nil)}}},
	})
	if err != nil {
		return nil, err
	}
	c, err := packCopies(packs)
	if err != nil {
		return nil, err
	}
	a, b := c[0][3], c[1][3]
	if d := a.headerDiff(b); len(d) == 0 || d[0] < recordTSOff || d[len(d)-1] >= recordTSOff+8 || !bytes.Equal(a.payload, b.payload) {
		return nil, errors.New("corpus: the copies of seq 3 do not differ in ts_utc_ns alone")
	}
	if a.ts >= msAt(to) || b.ts < msAt(to) {
		return nil, errors.New("corpus: the range does not select pack 0's copy of seq 3 alone")
	}

	return &Built{Packs: packs, Reads: []ReadSpec{{
		ID: "time-selects-one", Cites: readCites, Packs: []int{0, 1}, Order: ReadOrderCapture,
		Filter: Filter{TimeFrom: new(I64(msAt(0))), TimeTo: new(I64(msAt(to)))},
	}}}, nil
}

// clusterTwoHoursBuild builds multi-cluster-two-hours:
// pack 0 of capture A in the last minute of TimeBase's hour, one block of seqs 1 and 4 at 59 minutes and 59 minutes 1 ms,
// and pack 1 of capture A in the first minute of the next hour, one block of seqs 2 and 3 at its start and 1 ms after.
// It checks that the packs' periods lie in two hours and their blocks' seq ranges overlap.
func clusterTwoHoursBuild(seed string) (*Built, error) {
	const minute59 = 59 * 60_000
	packs, err := writeMultiPacks(seed, []multiPack{
		{capture: 0, minute: 59, blocks: [][]tracepack.Record{{multiData(1, msAt(minute59), nil), multiData(4, msAt(minute59+1), nil)}}},
		{capture: 0, hour: 1, blocks: [][]tracepack.Record{{multiData(2, hourAt(1, 0), nil), multiData(3, hourAt(1, 1), nil)}}},
	})
	if err != nil {
		return nil, err
	}
	var hours [2]int64
	for n, p := range packs {
		h, err := packHeader(p.Bytes)
		if err != nil {
			return nil, err
		}
		hours[n] = (h.Meta.PeriodStart - TimeBase) / hourNs
	}
	c, err := packCopies(packs)
	if err != nil {
		return nil, err
	}
	if hours != [2]int64{0, 1} || len(c[0]) != 2 || len(c[1]) != 2 || c[0][1].block != c[0][4].block {
		return nil, errors.New("corpus: the packs do not hold one block each in two hours, seqs 1 and 4 and seqs 2 and 3")
	}

	return &Built{Packs: packs, Reads: []ReadSpec{
		{ID: "capture-order", Cites: readCites, Packs: []int{0, 1}, Order: ReadOrderCapture},
		{ID: "time-order", Cites: readCites, Packs: []int{0, 1}, Order: ReadOrderTime},
	}}, nil
}

// unfinalizedBesideArchiveBuild builds multi-unfinalized-beside-archive:
// pack 0, a segment of capture A left unfinalized, its block 0 holding seqs 1 and 2 and block 1 seqs 3 and 4,
// and pack 1, the archive of capture A's hour, one block of the same four records.
// It checks that pack 0 is a segment and pack 1 an archive of one hour and that every record is the same bytes in both.
func unfinalizedBesideArchiveBuild(seed string) (*Built, error) {
	recs := []tracepack.Record{multiData(1, msAt(1), nil), multiData(2, msAt(2), nil), multiData(3, msAt(3), nil), multiData(4, msAt(4), nil)}
	packs, err := writeMultiPacks(seed, []multiPack{
		{capture: 0, open: true, blocks: [][]tracepack.Record{recs[:2], recs[2:]}},
		{capture: 0, archive: true, blocks: [][]tracepack.Record{recs}},
	})
	if err != nil {
		return nil, err
	}
	var roles [2]tracepack.PackRole
	for n, p := range packs {
		h, err := packHeader(p.Bytes)
		if err != nil {
			return nil, err
		}
		roles[n] = h.Meta.PackRole
	}
	if roles != [2]tracepack.PackRole{tracepack.PackRoleSegment, tracepack.PackRoleArchive} {
		return nil, fmt.Errorf("corpus: the packs are a %v and a %v, not a segment and an archive", roles[0], roles[1])
	}
	c, err := packCopies(packs)
	if err != nil {
		return nil, err
	}
	for s := uint64(1); s <= 4; s++ {
		if !c[0][s].same(c[1][s]) {
			return nil, fmt.Errorf("corpus: the copies of seq %d are not the same bytes", s)
		}
	}

	return &Built{Packs: packs, Reads: []ReadSpec{
		{ID: "archive-first", Cites: readCites, Packs: []int{1, 0}, Order: ReadOrderCapture},
		{ID: "segment-first", Cites: readCites, Packs: []int{0, 1}, Order: ReadOrderCapture},
	}}, nil
}

// packOf is the expectation of a finalized-consistent pack of blocks blocks holding the records of seqs, in order.
func packOf(blocks int, seqs ...uint64) *Expectation {
	return &Expectation{Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true, Blocks: blocks, Seqs: seqs, PrefixEnd: AtEnd()}
}

// storedCopy is a pack's copy of a record as its block stores it: the block, the timestamp,
// and the bytes a read over several packs compares, the record header as stored and the payload.
type storedCopy struct {
	block   int
	ts      int64
	header  []byte
	payload []byte
}

// same reports whether c and o are the same bytes.
func (c storedCopy) same(o storedCopy) bool {
	return bytes.Equal(c.header, o.header) && bytes.Equal(c.payload, o.payload)
}

// headerDiff returns the offsets at which the record headers of c and o differ, ascending; every offset past the shorter one.
func (c storedCopy) headerDiff(o storedCopy) []int {
	var out []int
	for i := range max(len(c.header), len(o.header)) {
		if i >= len(c.header) || i >= len(o.header) || c.header[i] != o.header[i] {
			out = append(out, i)
		}
	}

	return out
}

// onlyPayloadDiffers reports whether a and b have one record header and different payloads.
func onlyPayloadDiffers(a, b storedCopy) bool {
	return bytes.Equal(a.header, b.header) && !bytes.Equal(a.payload, b.payload)
}

// packCopies returns the stored copies of the records of each pack of packs, by seq (storedCopies).
func packCopies(packs []PackBuilt) ([]map[uint64]storedCopy, error) {
	out := make([]map[uint64]storedCopy, 0, len(packs))
	for n, p := range packs {
		c, err := storedCopies(p.Bytes)
		if err != nil {
			return nil, fmt.Errorf("corpus: pack %d: %w", n, err)
		}
		out = append(out, c)
	}

	return out, nil
}

// storedCopies returns the records every located block of pack stores, by seq, each as its block stores it.
// The blocks must decode and hold each seq once.
func storedCopies(pack []byte) (map[uint64]storedCopy, error) {
	l, err := Locate(pack)
	if err != nil {
		return nil, err
	}
	out := make(map[uint64]storedCopy)
	for i, s := range l.Blocks {
		stored := pack[s.Offset : s.Offset+s.Len]
		decoded, hs, err := decodeBlock(stored)
		if err != nil {
			return nil, fmt.Errorf("block %d: %w", i, err)
		}
		count := int(binary.LittleEndian.Uint32(stored[envelopeRecordCountOff:]))
		rhl := int(binary.LittleEndian.Uint16(stored[envelopeRecordHeaderLenOff:]))
		rows := format.UntransposeHeaders(nil, decoded[:hs], count, rhl)
		payloads := decoded[hs:]
		for k := range count {
			h := rows[k*rhl : (k+1)*rhl]
			n := int(binary.LittleEndian.Uint32(h[recordPayloadLenOff:]))
			if n > len(payloads) {
				return nil, fmt.Errorf("block %d, record %d: payload_len %d past the block", i, k, n)
			}
			seq := binary.LittleEndian.Uint64(h[recordSeqOff:])
			if _, ok := out[seq]; ok {
				return nil, fmt.Errorf("block %d: seq %d twice", i, seq)
			}
			out[seq] = storedCopy{
				block: i, ts: int64(binary.LittleEndian.Uint64(h[recordTSOff:])),
				header: bytes.Clone(h), payload: bytes.Clone(payloads[:n]),
			}
			payloads = payloads[n:]
		}
	}

	return out, nil
}

// packHeader returns the file header and pack metadata of pack.
func packHeader(pack []byte) (*tracepack.PackHeader, error) {
	r, err := openBytes(context.Background(), pack)
	if err != nil {
		return nil, err
	}

	return r.Header(), nil
}
