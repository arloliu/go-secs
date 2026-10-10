package corpus

import (
	"errors"
	"fmt"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// Tags of F-5 and of an epoch entry that the index surgery moves (the tracepack format specification §10).
const (
	f5TSMaxTag    uint16 = 0x0006
	epochTSMinTag uint16 = 0x0005
	epochTSMaxTag uint16 = 0x0006
)

// txConflictVectors returns the recipes of the tx group that check the identity of the records a lookup reads:
// conflicts within a scope read and across scope reads, and a block whose records disagree with its F-2 entry
// (the tracepack semantics specification §7.2, Identity across the scopes read, and §9).
func txConflictVectors() []Recipe {
	return []Recipe{
		{
			ID: "tx-conflicts",
			Title: "conflicts within a scope and across scopes, on the primary, a candidate and a record after the window, " +
				"and a conflicting primary whose first version has no function",
			Cites: []string{"SEM §7.2", "SEM §7.4", "SEM §9", "CORPUS §5.11"}, Class: ClassMultiPack, Labels: []string{LabelNonconformingWriter},
			Build: conflictsBuild,
			// Any conflict on a seq at or above the primary's makes the result incomplete, wherever it lies:
			// within one scope read, the read lists it; across scope reads, its hours are every hour whose read yielded the seq
			// (the tracepack semantics specification §7.2, Identity across the scopes read).
			// A version of a seq that qualified is kept with every other version the same read yielded, each by its own bytes;
			// a seq that plays no role is not kept, its conflict still a fact.
			// A conflict at the primary within its scope's read ends the lookup there, its key from the first version;
			// a conflict at the primary across scope reads does not: the lookup reads every hour scheduled
			// (the tracepack corpus specification §5.11, Early-return results).
			// J's first version of the primary holds its function byte with the function's field_validity bit clear:
			// for a raw capture, which pack 8 is, no conforming writer stores that (the tracepack format specification §7.2).
			Expect: &Expectation{
				Packs: []*Expectation{
					finalizedPack(1, 3), packOf(1, 2), finalizedPack(1, 4), packOf(1, 4), finalizedPack(1, 2),
					packOf(1, 1, 3), finalizedPack(1, 2), packOf(1, 2, 3), finalizedPack(1, 1), finalizedPack(1, 1),
				},
				Lookups: []LookupWant{
					{
						ID: "across-candidate", Outcome: "incomplete", Key: txPrimaryKey(1, conflictsSystemBytes), WindowEnd: new(uint64(3)),
						Records: []LookupRecord{
							txAt(txKept(1, false, false, RolePrimary), 0, 6),
							txInConflict(txAt(txCandidate(2, true), 0, 6)), txInConflict(txAt(txCandidate(2, true), 1, 7)),
							txAt(txKept(3, false, true, RoleClosing), 1, 7),
						},
						Gaps:     []FactWant{txConflict(2, 0, 1)},
						Searched: []SearchedScope{{Hour: I64(txHour), Indexed: true, Packs: []int{6}}, {Hour: I64(txHour + 1), Indexed: true, Packs: []int{7}}},
					},
					{
						ID: "across-primary", Outcome: "incomplete", Key: txPrimaryKey(1, conflictsSystemBytes), WindowEnd: new(uint64(3)),
						Records: []LookupRecord{
							txInConflict(txAt(txKept(1, false, false, RolePrimary), 0, 4)), txInConflict(txAt(txKept(1, false, false, RolePrimary), 1, 5)),
							txAt(txKept(3, false, true, RoleClosing), 1, 5),
						},
						Gaps:     []FactWant{{Fact: Fact{Reason: FactNoKey}}, txConflict(1, 0, 1)},
						Searched: []SearchedScope{{Hour: I64(txHour), Indexed: true, Packs: []int{4}}, {Hour: I64(txHour + 1), Indexed: true, Packs: []int{5}}},
					},
					{
						ID: "primary-first-version-no-function", Outcome: "incomplete", Key: txKeyWithoutFunction(),
						Records: []LookupRecord{
							txInConflict(txAt(txKept(1, false, false, RolePrimary), 0, 8)), txInConflict(txAt(txKept(1, false, false, RolePrimary), 0, 9)),
						},
						Gaps:      []FactWant{{Fact: Fact{Reason: FactNoKey}}, txConflict(1, 0)},
						Searched:  []SearchedScope{{Hour: I64(txHour), Indexed: true, Packs: []int{8, 9}}},
						Conflicts: []ConflictWant{{Capture: 4, Seq: 1, Versions: [][]int{{8}, {9}}}},
					},
					{
						ID: "within-candidate", Outcome: "incomplete", Key: txPrimaryKey(1, conflictsSystemBytes), WindowEnd: new(uint64(3)),
						Records: []LookupRecord{
							txKept(1, false, false, RolePrimary), txInConflict(txCandidate(2, true)), txInConflict(txAt(txCandidate(2, true), 0, 1)),
							txKept(3, false, true, RoleClosing),
						},
						Gaps:      []FactWant{txConflict(2, 0)},
						Searched:  []SearchedScope{{Hour: I64(txHour), Indexed: true, Packs: []int{0, 1}}},
						Conflicts: []ConflictWant{{Capture: 0, Seq: 2, Versions: [][]int{{0}, {1}}}},
					},
					{
						ID: "within-unrelated-after-window", Outcome: "incomplete", Key: txPrimaryKey(1, conflictsSystemBytes), WindowEnd: new(uint64(3)),
						Records: []LookupRecord{
							txAt(txKept(1, false, false, RolePrimary), 0, 2), txAt(txCandidate(2, true), 0, 2), txAt(txKept(3, false, true, RoleClosing), 0, 2),
						},
						Gaps:      []FactWant{txConflict(4, 0)},
						Searched:  []SearchedScope{{Hour: I64(txHour), Indexed: true, Packs: []int{2, 3}}},
						Conflicts: []ConflictWant{{Capture: 1, Seq: 4, Versions: [][]int{{2}, {3}}}},
					},
				},
			},
		},
		{
			ID:    "tx-index-mismatch",
			Title: "a block whose records disagree with its F-2 entry beside a match",
			Cites: []string{"SEM §7.2", "FMT §10", "SEM §9", "CORPUS §5.11"}, Class: ClassMultiPack, Labels: []string{LabelNonconformingWriter},
			Build: indexMismatchBuild,
			// The block's F-2 ts_max lies after its last record's time, and F-5's ts_max with it, so the footer stays consistent and is used;
			// the scope read finds the block disagreeing with its entry, which makes the result incomplete even with a match found
			// (the tracepack semantics specification §7.2, Identity across the scopes read).
			// The block is the pack's only one, so every record arrives in seq order, none above the primary before it.
			Expect: &Expectation{
				Packs: []*Expectation{disagreeingPack(finalizedPack(1, 3), 0)},
				Lookups: []LookupWant{{
					ID: "match-found", Outcome: "incomplete", Key: txPrimaryKey(1, indexSystemBytes), WindowEnd: new(uint64(3)),
					Records: []LookupRecord{
						txKept(1, false, false, RolePrimary), txCandidate(2, true), txKept(3, false, true, RoleClosing),
					},
					Gaps: []FactWant{{
						Fact: Fact{Reason: FactIndex, Hours: []I64{I64(txHour)}, Pack: new(0), Block: new(0)}, At: AtBlock(0),
					}},
					Searched: txSearched(),
				}},
			},
		},
	}
}

// conflictsSystemBytes are the System Bytes of every data record of tx-conflicts.
const conflictsSystemBytes uint32 = 0xE1

// conflictsBuild builds tx-conflicts, ten segments, their quality evaluated, one block each, every record of epoch 1,
// the captures A, E, F, G and J numbered 0 to 4; a second segment of a scope in its second minute:
// pack 0 of A in hour H: 1 S1F1 x, 2 S1F2 x, 3 a socket-close; pack 1 of A in H: 2 S1F2 x at another time;
// pack 2 of E in H: 1 S1F1 x, 2 S1F2 x, 3 a socket-close, 4 an annotation; pack 3 of E in H: 4 an annotation of other text;
// pack 4 of F in H: 1 S1F1 x, 2 an annotation; pack 5 of F in H + 1: 1 S1F1 x and 3 a socket-close, in H + 1;
// pack 6 of G in H: 1 S1F1 x, 2 S1F2 x; pack 7 of G in H + 1: 2 S1F2 x and 3 a socket-close, in H + 1;
// pack 8 of J in H: 1 S1F1 x, its function's field_validity bit clear; pack 9 of J in H: 1 S1F1 x.
// It checks that each reply carries its primary's key, that the copies of each seq held twice differ,
// that the records of hour H + 1 lie in it, and that J's two versions differ only in the function's availability.
func conflictsBuild(seed string) (*Built, error) {
	x := conflictsSystemBytes
	var err error
	// closeIn returns the socket-close at 3 of the scope of hour hour after H, every window's end.
	closeIn := func(hour int64) tracepack.Record {
		var r tracepack.Record
		if err == nil {
			r, err = newSocketEvent(3, 1, tracepack.EventSocketClose)
		}
		r.TSUTCNs = hourAt(hour, 3)

		return r
	}
	note := func(seq uint64, text string) tracepack.Record {
		var r tracepack.Record
		if err == nil {
			r, err = newNote(seq, text)
		}

		return r
	}
	inHour := func(r tracepack.Record, hour, ms int64) tracepack.Record {
		r.TSUTCNs = hourAt(hour, ms)

		return r
	}
	a0 := []tracepack.Record{txPrimary(1, 1, x), txReply(2, 1, 2, x), closeIn(0)}
	a1 := inHour(txReply(2, 1, 2, x), 0, minuteMs+2)
	e0 := []tracepack.Record{txPrimary(1, 1, x), txReply(2, 1, 2, x), closeIn(0), note(4, "a record after the window")}
	e1 := note(4, "another version of the record after the window")
	f0 := []tracepack.Record{txPrimary(1, 1, x), note(2, "a record of the window")}
	f1 := []tracepack.Record{inHour(txPrimary(1, 1, x), 1, 1), closeIn(1)}
	g0 := []tracepack.Record{txPrimary(1, 1, x), txReply(2, 1, 2, x)}
	g1 := []tracepack.Record{inHour(txReply(2, 1, 2, x), 1, 2), closeIn(1)}
	j0 := txPrimary(1, 1, x)
	j0.FieldValidity &^= tracepack.FieldValidityFunction
	j1 := txPrimary(1, 1, x)
	if err != nil {
		return nil, err
	}
	if err := checkConflictsRecords(a0, e0, g0, g1, [2]tracepack.Record{j0, j1}); err != nil {
		return nil, err
	}
	packs, err := writeMultiPacks(seed, []multiPack{
		{capture: 0, blocks: [][]tracepack.Record{a0}, edit: qualityEvaluated},
		{capture: 0, minute: 1, blocks: [][]tracepack.Record{{a1}}, edit: qualityEvaluated},
		{capture: 1, blocks: [][]tracepack.Record{e0}, edit: qualityEvaluated},
		{capture: 1, minute: 1, blocks: [][]tracepack.Record{{e1}}, edit: qualityEvaluated},
		{capture: 2, blocks: [][]tracepack.Record{f0}, edit: qualityEvaluated},
		{capture: 2, hour: 1, blocks: [][]tracepack.Record{f1}, edit: qualityEvaluated},
		{capture: 3, blocks: [][]tracepack.Record{g0}, edit: qualityEvaluated},
		{capture: 3, hour: 1, blocks: [][]tracepack.Record{g1}, edit: qualityEvaluated},
		{capture: 4, blocks: [][]tracepack.Record{{j0}}, edit: qualityEvaluated},
		{capture: 4, minute: 1, blocks: [][]tracepack.Record{{j1}}, edit: qualityEvaluated},
	})
	if err != nil {
		return nil, err
	}
	copies, err := packCopies(packs)
	if err != nil {
		return nil, err
	}
	for _, c := range []struct{ a, b, seq int }{{0, 1, 2}, {2, 3, 4}, {4, 5, 1}, {6, 7, 2}, {8, 9, 1}} {
		if copies[c.a][uint64(c.seq)].same(copies[c.b][uint64(c.seq)]) {
			return nil, fmt.Errorf("corpus: packs %d and %d hold the same copy of seq %d", c.a, c.b, c.seq)
		}
	}

	twoHours := func(id string, capture int, view ...int) LookupSpec {
		l := txLookupIn(seed, id, capture, 1, view...)
		l.MaxScopes = 2

		return l
	}

	return &Built{Packs: packs, Lookups: []LookupSpec{
		twoHours("across-candidate", 3, 6, 7),
		twoHours("across-primary", 2, 4, 5),
		txLookupIn(seed, "primary-first-version-no-function", 4, 1, 8, 9),
		txLookup(seed, "within-candidate", 1, 0, 1),
		txLookupIn(seed, "within-unrelated-after-window", 1, 1, 2, 3),
	}}, nil
}

// minuteMs is one minute in milliseconds: an offset into a scope's second minute, where its second segment's period lies.
const minuteMs = segmentPeriod / 1_000_000

// checkConflictsRecords checks the records of tx-conflicts: each reply of a0, e0, g0 and g1 a candidate of its capture's primary,
// g1's records in hour H + 1, and the versions j of J's primary alike but for the availability of the first one's function.
func checkConflictsRecords(a0, e0, g0, g1 []tracepack.Record, j [2]tracepack.Record) error {
	for _, pair := range [][2]*tracepack.Record{{&a0[0], &a0[1]}, {&e0[0], &e0[1]}, {&g0[0], &g0[1]}, {&g0[0], &g1[0]}} {
		if !sameKey(pair[0], pair[1]) || pair[0].Dir == pair[1].Dir {
			return fmt.Errorf("corpus: a record at %d is not a candidate of the primary at %d", pair[1].Seq, pair[0].Seq)
		}
	}
	if hourOf(g1[0].TSUTCNs) != txHour+1 || hourOf(g1[1].TSUTCNs) != txHour+1 {
		return errors.New("corpus: G's records of hour H + 1 do not lie in it")
	}
	h0, h1 := j[0].HSMSHeader(), j[1].HSMSHeader()
	switch {
	case h0.Available.Has(tracepack.FieldValidityFunction) || !h1.Available.Has(tracepack.FieldValidityFunction):
		return errors.New("corpus: J's first version of the primary has its function available, or its second not")
	case !sameKey(&j[0], &j[1]) || h0.Available|tracepack.FieldValidityFunction != h1.Available || j[1].HSMSHeader().Function%2 != 1:
		return errors.New("corpus: J's versions of the primary differ in more than the function's availability")
	}

	return nil
}

// txKeyWithoutFunction returns the fields of J's primary in tx-conflicts, from its first version: txPrimaryKey's without the function.
func txKeyWithoutFunction() *PrimaryKey {
	k := txPrimaryKey(1, conflictsSystemBytes)
	k.Function = nil

	return k
}

// indexSystemBytes are the System Bytes of the transaction of tx-index-mismatch.
const indexSystemBytes uint32 = 0xE2

// indexTSMaxMs is where tx-index-mismatch's F-2 entry, and F-5, put the block's ts_max: 30 ms into hour H, after its last record.
const indexTSMaxMs = 30

// indexMismatchBuild builds tx-index-mismatch: pack 0 of capture A in hour H, its quality evaluated, one block, epoch 1:
// 1 S1F1 x, 2 S1F2 x, 3 a socket-close; then its footer edited (movedTSMax): the block's F-2 ts_max, and F-5's ts_max with it,
// moved from the socket-close's time to indexTSMaxMs, later in hour H, its seq range unchanged.
// It checks that 2 is a candidate of 1 and that the new ts_max lies after every record, in hour H.
func indexMismatchBuild(seed string) (*Built, error) {
	x := indexSystemBytes
	closed, err := newSocketEvent(3, 1, tracepack.EventSocketClose)
	if err != nil {
		return nil, err
	}
	records := []tracepack.Record{txPrimary(1, 1, x), txReply(2, 1, 2, x), closed}
	moved := msAt(indexTSMaxMs)
	switch {
	case !sameKey(&records[0], &records[1]) || records[1].Dir == records[0].Dir:
		return nil, errors.New("corpus: the record at 2 is not a candidate of 1")
	case moved <= closed.TSUTCNs || hourOf(moved) != txHour:
		return nil, errors.New("corpus: the moved ts_max is not after the block's last record in hour H")
	}
	packs, err := writeMultiPacks(seed, []multiPack{{capture: 0, blocks: [][]tracepack.Record{records}, edit: qualityEvaluated}})
	if err != nil {
		return nil, err
	}
	if packs[0].Bytes, err = movedTSMax(packs[0].Bytes, closed.TSUTCNs, moved); err != nil {
		return nil, err
	}

	return &Built{Packs: packs, Lookups: []LookupSpec{txLookup(seed, "match-found", 1, 0)}}, nil
}

// movedTSMax returns pack, a finalized pack of one block, with the block's F-2 ts_max and F-5's ts_max moved from from to to,
// so that the footer stays consistent with itself while the block's records disagree with its F-2 entry.
func movedTSMax(pack []byte, from, to int64) ([]byte, error) {
	return PatchFooter(pack, func(d []byte) ([]byte, error) {
		e, err := F2Entry(d, 0)
		if err != nil {
			return nil, err
		}
		if err := setLE64(d, e.Off+f2TSMaxOff, uint64(from), uint64(to)); err != nil {
			return nil, err
		}
		f5, err := F5List(d)
		if err != nil {
			return nil, err
		}
		at, err := EntryOffset(d, f5, f5TSMaxTag, 0)
		if err != nil {
			return nil, err
		}

		return d, setLE64(d, at+tlv.HeaderLen, uint64(from), uint64(to))
	})
}

// disagreeingPack returns e for a pack whose footer is valid, and so used, while its blocks blocks disagree with their F-2 entries.
func disagreeingPack(e *Expectation, blocks ...int) *Expectation {
	e.Outcome, e.Disagreeing = tracepack.OutcomeFinalizedInconsistent, blocks

	return e
}

// txInConflict returns the kept version r, its seq found in conflict.
func txInConflict(r LookupRecord) LookupRecord {
	r.Conflict = true

	return r
}

// txConflict returns the conflict fact at seq of the scope reads of the hours hours after H.
func txConflict(seq uint64, hours ...int64) FactWant {
	hs := make([]I64, 0, len(hours))
	for _, h := range hours {
		hs = append(hs, I64(txHour+h))
	}

	return FactWant{Fact: Fact{Reason: FactConflict, Hours: hs, Seq: new(U64(seq))}}
}
