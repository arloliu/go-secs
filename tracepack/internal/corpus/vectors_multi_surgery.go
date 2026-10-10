package corpus

import (
	"bytes"
	"errors"
	"fmt"
	"slices"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// Values of the record header surgery of multi-header-conflicts (the tracepack format specification §7.1, §7.2).
const (
	// recordFlagsOff is the offset of record_flags in a record header.
	recordFlagsOff = 43
	// reservedRecordFlag is bit 7 of record_flags, reserved.
	reservedRecordFlag = 0x80
	// extendedHeaderLen is the record_header_len of the extended blocks: four extension bytes.
	extendedHeaderLen = format.RecordHeaderLen + 4
)

// multiSurgeryVectors returns the recipes of the multi group whose packs are changed by raw surgery
// or whose reads depend on a conflict bound or on blocks a read may exclude (the tracepack corpus specification §9.4).
func multiSurgeryVectors() []Recipe {
	return []Recipe{
		{
			ID: "multi-header-conflicts", Title: "copies differing only in a reserved record_flags bit, in an extension byte and in record_header_len: three conflicts",
			Cites: []string{"FMT §7.1", "SEM §7.4", "SEM §9", "CORPUS §5.10"}, Class: ClassMultiPack, Labels: []string{LabelNonconformingWriter},
			Build: headerConflictsBuild,
			// A read compares the record header as stored, all record_header_len bytes:
			// each of seqs 1 to 3 has two versions, pack 0's and pack 1's, each yielded from its own pack's block.
			Expect: &Expectation{
				Packs: []*Expectation{finalizedPack(2, 3), finalizedPack(2, 3)},
				Reads: []ReadWant{{
					ID: "all",
					Items: []ItemWant{
						{Seq: 1, Pack: 0, Block: 0, Conflict: true}, {Seq: 1, Pack: 1, Block: 0, Conflict: true},
						{Seq: 2, Pack: 0, Block: 0, Conflict: true}, {Seq: 2, Pack: 1, Block: 0, Conflict: true},
						{Seq: 3, Pack: 0, Block: 1, Conflict: true}, {Seq: 3, Pack: 1, Block: 1, Conflict: true},
					},
					Conflicts: []ConflictWant{
						{Capture: 0, Seq: 1, Versions: [][]int{{0}, {1}}},
						{Capture: 0, Seq: 2, Versions: [][]int{{0}, {1}}},
						{Capture: 0, Seq: 3, Versions: [][]int{{0}, {1}}},
					},
				}},
			},
		},
		{
			ID: "multi-cluster-mixed-exclusion", Title: "a cluster of a block the time range excludes and one it does not: both read, the conflict listed",
			Cites: []string{"SEM §7.4", "SEM §9", "CORPUS §5.10"}, Class: ClassMultiPack,
			Build: clusterMixedExclusionBuild,
			// Pack 1's block lies after the range, but it shares seq 2 with pack 0's block, which the range keeps:
			// the cluster is not excluded, both blocks are read, and the copies of seq 2 are compared.
			// Pack 0's version of seq 2 is yielded as a conflict; pack 1's records lie outside the range.
			Expect: &Expectation{
				Packs: []*Expectation{finalizedPack(1, 3), packOf(1, 2, 4)},
				Reads: []ReadWant{{
					ID:        "one-block-excluded",
					Items:     []ItemWant{{Seq: 1, Pack: 0, Block: 0}, {Seq: 2, Pack: 0, Block: 0, Conflict: true}, {Seq: 3, Pack: 0, Block: 0}},
					Conflicts: []ConflictWant{{Capture: 0, Seq: 2, Versions: [][]int{{0}, {1}}}},
				}},
			},
		},
		{
			ID: "multi-conflict-limit", Title: "three conflicts, read with a bound of three (success) and of two (conflict-limit)",
			Cites: []string{"SEM §7.4", "SEM §9", "CORPUS §1", "CORPUS §5.10"}, Class: ClassMultiPack,
			Build: conflictLimitBuild,
			// Capture order, data records only, capture B (packs 0 and 1) and capture A (packs 2 and 3), conflicts discovered per seq.
			// The captures start in ascending capture_id, B's the lower, each up to its first selected record:
			// B up to its seq 1, leaving its annotation 2 unresolved, then A up to its seq 1, a conflict (the first).
			// B's seq 1 is yielded (ts_min 0 against A's 10), then B advances past its annotation 2, a conflict (the second),
			// to its seq 3 in its next block, a conflict (the third) scheduled at ts_min 20;
			// A's two versions of seq 1 and its seq 2 come between, at 10.
			// With a bound of three, the read succeeds; with a bound of two, it fails on the third, listing the first two:
			// A's seq 1 before B's seq 2, which the filter rejects.
			Expect: &Expectation{
				Packs: []*Expectation{finalizedPack(2, 3), finalizedPack(2, 3), finalizedPack(1, 2), finalizedPack(1, 2)},
				Reads: []ReadWant{
					{ID: "limit-exact", Items: []ItemWant{
						{Seq: 1, Pack: 0, Block: 0},
						{Seq: 1, Pack: 2, Block: 0, Conflict: true}, {Seq: 1, Pack: 3, Block: 0, Conflict: true}, {Seq: 2, Pack: 2, Block: 0},
						{Seq: 3, Pack: 0, Block: 1, Conflict: true}, {Seq: 3, Pack: 1, Block: 1, Conflict: true},
					}, Conflicts: limitConflicts},
					{ID: "limit-exceeded", Error: ErrorConflictLimit, Conflicts: limitConflicts[:2]},
				},
			},
		},
		{
			ID: "multi-walked-excluded", Title: "an unfinalized pack whose walked block's computed summary excludes its cluster",
			Cites: []string{"SEM §7.4", "SEM §9", "CORPUS §5.4", "CORPUS §5.10"}, Class: ClassMultiPack,
			Build: walkedExcludedBuild,
			// Pack 0 is not finalized, so its blocks are walked and their computed summaries stand in for their F-2 entries:
			// block 1's lies after the range, and its cluster, block 1 alone, holds no conflict and no selected record,
			// so the result is the same whether or not a reader excludes it.
			// Pack 0 is truncated at its end, and its footer, which it does not have, is not used.
			Expect: &Expectation{
				Packs: []*Expectation{
					{Outcome: tracepack.OutcomeUnfinalized, Unfinalized: true, Blocks: 2, Seqs: seqRange(1, 4), PrefixEnd: AtEnd()},
					packOf(1, 5, 6),
				},
				Reads: []ReadWant{{
					ID:           "walked-block-excluded",
					Items:        slices.Concat(blockItems(0, 0, 1, 2), blockItems(1, 0, 5, 6)),
					Incomplete:   []PackIncompleteWant{{Pack: 0, IncompleteWant: Truncated(AtEnd())}},
					FooterErrors: []int{0},
				}},
			},
		},
	}
}

// limitConflicts are the conflicts of multi-conflict-limit in discovery order: A's seq 1, B's seq 2, B's seq 3.
var limitConflicts = []ConflictWant{
	{Capture: 0, Seq: 1, Versions: [][]int{{2}, {3}}},
	{Capture: 1, Seq: 2, Versions: [][]int{{0}, {1}}},
	{Capture: 1, Seq: 3, Versions: [][]int{{0}, {1}}},
}

// earlyRange is the time range of the reads that a block after 600,000 ms lies outside.
const earlyRange = 600_000

// headerConflictsBuild builds multi-header-conflicts:
// two segments of capture A, in the first and second minutes of the hour,
// each block 0 holding the data records of seqs 1 and 2 and block 1 that of seq 3, the same records.
// Raw surgery then gives both block 0s 48-byte record headers with the same extension bytes,
// and in pack 1 sets a reserved record_flags bit of seq 1, changes an extension byte of seq 2,
// and extends seq 3's header to 48 bytes with zero extension bytes, pack 0's staying 44.
// It checks that each pair of copies differs in that alone.
func headerConflictsBuild(seed string) (*Built, error) {
	blocks := [][]tracepack.Record{
		{multiData(1, msAt(1), nil), multiData(2, msAt(2), nil)},
		{multiData(3, msAt(3), nil)},
	}
	packs, err := writeMultiPacks(seed, []multiPack{
		{capture: 0, blocks: blocks},
		{capture: 0, minute: 1, blocks: blocks},
	})
	if err != nil {
		return nil, err
	}
	// extend gives each record of a block the extension bytes ext(k) for its k-th record.
	extend := func(b *Body, ext func(k int) []byte) {
		var rows []byte
		for k := range len(b.Rows) / b.RecordHeaderLen {
			rows = append(rows, b.Rows[k*b.RecordHeaderLen:(k+1)*b.RecordHeaderLen]...)
			rows = append(rows, ext(k)...)
		}
		b.Rows, b.RecordHeaderLen = rows, extendedHeaderLen
	}
	for n := range packs {
		p, err := PatchBody(packs[n].Bytes, 0, func(b *Body) error {
			extend(b, func(k int) []byte { return []byte{0xE0 | byte(k), 0xE4, 0xE8, 0xEC} })
			if n == 1 {
				b.Rows[recordFlagsOff] ^= reservedRecordFlag
				b.Rows[extendedHeaderLen+format.RecordHeaderLen+1] ^= 0xFF
			}

			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("corpus: pack %d: %w", n, err)
		}
		packs[n].Bytes = p
	}
	if packs[1].Bytes, err = PatchBody(packs[1].Bytes, 1, func(b *Body) error {
		extend(b, func(int) []byte { return make([]byte, extendedHeaderLen-format.RecordHeaderLen) })
		return nil
	}); err != nil {
		return nil, fmt.Errorf("corpus: pack 1: %w", err)
	}

	c, err := packCopies(packs)
	if err != nil {
		return nil, err
	}
	a, b := c[0], c[1]
	for s := uint64(1); s <= 3; s++ {
		if !bytes.Equal(a[s].payload, b[s].payload) {
			return nil, fmt.Errorf("corpus: the copies of seq %d differ in their payloads", s)
		}
	}
	switch {
	case !slices.Equal(a[1].headerDiff(b[1]), []int{recordFlagsOff}) || a[1].header[recordFlagsOff]^b[1].header[recordFlagsOff] != reservedRecordFlag:
		return nil, errors.New("corpus: the copies of seq 1 do not differ in a reserved record_flags bit alone")
	case len(a[2].header) != extendedHeaderLen || len(a[2].headerDiff(b[2])) != 1 || a[2].headerDiff(b[2])[0] < format.RecordHeaderLen:
		return nil, errors.New("corpus: the copies of seq 2 do not differ in one extension byte alone")
	case len(a[3].header) != format.RecordHeaderLen || len(b[3].header) != extendedHeaderLen ||
		!bytes.Equal(a[3].header, b[3].header[:format.RecordHeaderLen]) || slices.ContainsFunc(b[3].header[format.RecordHeaderLen:], func(x byte) bool { return x != 0 }):
		return nil, errors.New("corpus: the copies of seq 3 do not differ in record_header_len alone")
	}

	return &Built{Packs: packs, Reads: []ReadSpec{{ID: "all", Cites: readCites, Packs: []int{0, 1}, Order: ReadOrderCapture}}}, nil
}

// clusterMixedExclusionBuild builds multi-cluster-mixed-exclusion:
// pack 0 of capture A, one block of seqs 1 to 3 at 60,000 ms and the 2 ms after,
// and pack 1 of capture A, in the next minute, one block of seqs 2 and 4 at 2,400,000 ms and 1 ms after;
// the read selects [0, 600,000) ms.
// It checks that the blocks share seq 2 alone, whose copies differ in ts_utc_ns alone,
// and that pack 0's block lies inside the range and pack 1's after it.
func clusterMixedExclusionBuild(seed string) (*Built, error) {
	const late = 2_400_000
	packs, err := writeMultiPacks(seed, []multiPack{
		{capture: 0, blocks: [][]tracepack.Record{{multiData(1, msAt(60_000), nil), multiData(2, msAt(60_001), nil), multiData(3, msAt(60_002), nil)}}},
		{capture: 0, minute: 1, blocks: [][]tracepack.Record{{multiData(2, msAt(late), nil), multiData(4, msAt(late+1), nil)}}},
	})
	if err != nil {
		return nil, err
	}
	c, err := packCopies(packs)
	if err != nil {
		return nil, err
	}
	a, b := c[0], c[1]
	shared := 0
	for s := range b {
		if _, ok := a[s]; ok {
			shared++
		}
	}
	d := a[2].headerDiff(b[2])
	switch {
	case shared != 1 || len(b) != 2:
		return nil, errors.New("corpus: the blocks do not share seq 2 alone")
	case len(d) == 0 || d[0] < recordTSOff || d[len(d)-1] >= recordTSOff+8 || !bytes.Equal(a[2].payload, b[2].payload):
		return nil, errors.New("corpus: the copies of seq 2 do not differ in ts_utc_ns alone")
	case a[3].ts >= msAt(earlyRange) || b[2].ts < msAt(earlyRange) || b[4].ts < msAt(earlyRange):
		return nil, errors.New("corpus: the range does not keep pack 0's block and exclude pack 1's")
	}

	return &Built{Packs: packs, Reads: []ReadSpec{{
		ID: "one-block-excluded", Cites: readCites, Packs: []int{0, 1}, Order: ReadOrderCapture,
		Filter: Filter{TimeFrom: new(I64(msAt(0))), TimeTo: new(I64(msAt(earlyRange)))},
	}}}, nil
}

// conflictLimitBuild builds multi-conflict-limit:
// packs 0 and 1 of capture B, in the first and second minutes of the hour,
// each block 0 holding seq 1, a data record at 0 ms, and seq 2, an annotation at 1 ms, and block 1 seq 3, a data record at 20 ms;
// and packs 2 and 3 of capture A, in its first and second minutes,
// each one block of seqs 1 and 2, data records at 10 and 11 ms.
// The copies of B's seqs 2 and 3 and of A's seq 1 differ in their text alone; the others are the same bytes.
// The reads select data records.
// It checks that B's capture_id is below A's, so B starts first,
// that the three seqs are the ones whose copies differ, and that in each the copies differ in their payloads alone.
func conflictLimitBuild(seed string) (*Built, error) {
	if a, b := captureOf(seed, 0), captureOf(seed, 1); bytes.Compare(b[:], a[:]) >= 0 {
		return nil, errors.New("corpus: capture B's capture_id is not below capture A's")
	}
	capture := func(c byte) ([][]tracepack.Record, [][]tracepack.Record, error) {
		note, err := multiNote(2, msAt(1), string(c))
		if err != nil {
			return nil, nil, err
		}
		b := [][]tracepack.Record{{multiData(1, msAt(0), nil), note}, {multiData(3, msAt(20), asciiItem(c))}}
		a := [][]tracepack.Record{{multiData(1, msAt(10), asciiItem(c)), multiData(2, msAt(11), nil)}}

		return b, a, nil
	}
	b0, a0, err := capture('x')
	if err != nil {
		return nil, err
	}
	b1, a1, err := capture('y')
	if err != nil {
		return nil, err
	}
	packs, err := writeMultiPacks(seed, []multiPack{
		{capture: 1, blocks: b0}, {capture: 1, minute: 1, blocks: b1},
		{capture: 0, blocks: a0}, {capture: 0, minute: 1, blocks: a1},
	})
	if err != nil {
		return nil, err
	}
	c, err := packCopies(packs)
	if err != nil {
		return nil, err
	}
	for _, pair := range []struct {
		pack      int
		differing []uint64
	}{{0, []uint64{2, 3}}, {2, []uint64{1}}} {
		x, y := c[pair.pack], c[pair.pack+1]
		for s := range x {
			switch {
			case slices.Contains(pair.differing, s) && !onlyPayloadDiffers(x[s], y[s]):
				return nil, fmt.Errorf("corpus: the copies of seq %d in packs %d and %d do not differ in their payloads alone", s, pair.pack, pair.pack+1)
			case !slices.Contains(pair.differing, s) && !x[s].same(y[s]):
				return nil, fmt.Errorf("corpus: the copies of seq %d in packs %d and %d differ", s, pair.pack, pair.pack+1)
			}
		}
	}

	reads := []ReadSpec{{ID: "limit-exact", MaxConflicts: 3}, {ID: "limit-exceeded", MaxConflicts: 2}}
	for i := range reads {
		reads[i].Cites, reads[i].Packs, reads[i].Order = readCites, []int{0, 1, 2, 3}, ReadOrderCapture
		reads[i].Filter = Filter{Kinds: []string{tracepack.KindData.String()}}
	}

	return &Built{Packs: packs, Reads: reads}, nil
}

// walkedExcludedBuild builds multi-walked-excluded:
// pack 0, a segment of capture A left unfinalized, its block 0 holding seqs 1 and 2 at 60,000 ms and 1 ms after
// and its block 1 seqs 3 and 4 at 2,400,000 ms and 1 ms after,
// and pack 1, a segment of capture A in the third minute of the hour, one block of seqs 5 and 6 at 120,000 ms and 1 ms after;
// the read selects [0, 600,000) ms.
// It checks that block 1 of pack 0 lies after the range and every other block inside it, and that no two blocks share a seq.
func walkedExcludedBuild(seed string) (*Built, error) {
	const late = 2_400_000
	packs, err := writeMultiPacks(seed, []multiPack{
		{capture: 0, open: true, blocks: [][]tracepack.Record{
			{multiData(1, msAt(60_000), nil), multiData(2, msAt(60_001), nil)},
			{multiData(3, msAt(late), nil), multiData(4, msAt(late+1), nil)},
		}},
		{capture: 0, minute: 2, blocks: [][]tracepack.Record{{multiData(5, msAt(120_000), nil), multiData(6, msAt(120_001), nil)}}},
	})
	if err != nil {
		return nil, err
	}
	c, err := packCopies(packs)
	if err != nil {
		return nil, err
	}
	for n, p := range c {
		for s, r := range p {
			if _, ok := c[1-n][s]; ok {
				return nil, fmt.Errorf("corpus: both packs hold seq %d", s)
			}
			if after := r.ts >= msAt(earlyRange); after != (n == 0 && r.block == 1) {
				return nil, fmt.Errorf("corpus: seq %d of pack %d is on the wrong side of the range", s, n)
			}
		}
	}

	return &Built{Packs: packs, Reads: []ReadSpec{{
		ID: "walked-block-excluded", Cites: readCites, Packs: []int{0, 1}, Order: ReadOrderCapture,
		Filter: Filter{TimeFrom: new(I64(msAt(0))), TimeTo: new(I64(msAt(earlyRange)))},
	}}}, nil
}
