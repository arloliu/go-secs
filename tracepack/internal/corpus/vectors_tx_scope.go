package corpus

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// metaPeriodStartTag is the pack metadata tag of period_start, which the scope-breach surgery moves with period_end
// (the tracepack format specification §5).
const metaPeriodStartTag uint16 = 0x0010

// halfHourMs is thirty minutes in milliseconds.
const halfHourMs = 30 * 60 * 1000

// txScopeVectors returns the recipes of the tx group that check the scopes a lookup reads and what they leave unsearched:
// conflicted scopes, records outside their scope's hour, a window seq held only by an hour not read,
// and coverage entries and barriers against the window and the hours scheduled
// (the tracepack semantics specification §7.2 and §9, the tracepack storage specification §4 and §5).
func txScopeVectors() []Recipe {
	return []Recipe{
		{
			ID:    "tx-conflicted-scope",
			Title: "two archives of equal rank in the primary's hour (no key) and in a later hour (a match beside it)",
			Cites: []string{"SEM §7.2", "STO §4", "SEM §9", "CORPUS §5.11", "CORPUS §8"}, Class: ClassMultiPack,
			Build: conflictedScopeBuild,
			// Two complete replacement sets of the highest rank make a scope conflicted, never a silent choice
			// (the tracepack storage specification §4), and a lookup reads none of its packs.
			// In the primary's hour it explains the missing primary: the lookup ends there, without a key.
			// In a later hour, the lookup goes on with the next one; the hour stays scheduled, and seq 2,
			// held only by the unread archives, is a seq of the window no scope read yielded
			// (the tracepack semantics specification §7.2, Scopes read, Identity across the scopes read).
			Expect: &Expectation{
				Packs: []*Expectation{
					finalizedPack(1, 2), finalizedPack(1, 2), finalizedPack(1, 1), packOf(1, 2), packOf(1, 2), packOf(1, 3, 4),
				},
				Lookups: []LookupWant{
					{
						ID: "later-hour", Outcome: "matched", Key: txPrimaryKey(1, conflictedSystemBytes), WindowEnd: new(uint64(4)),
						Records: []LookupRecord{
							txAt(txKept(1, false, false, RolePrimary), 0, 2), txAt(txCandidate(3, true), 2, 5),
							txAt(txKept(4, false, true, RoleClosing), 2, 5),
						},
						Gaps: []FactWant{
							{Fact: Fact{Reason: FactConflicted, Hours: []I64{I64(txHour + 1)}}}, {Fact: Fact{Reason: FactSeqGap}},
						},
						Searched: []SearchedScope{{Hour: I64(txHour), Indexed: true, Packs: []int{2}}, {Hour: I64(txHour + 2), Indexed: true, Packs: []int{5}}},
					},
					{
						ID: "primary-hour", Outcome: "incomplete",
						Gaps: []FactWant{{Fact: Fact{Reason: FactNoKey}}, {Fact: Fact{Reason: FactConflicted, Hours: []I64{I64(txHour)}}}},
					},
				},
			},
		},
		{
			ID:    "tx-scope-breach",
			Title: "a pack of hour H whose blocks lie in another hour, a same-hour clock step, and a breach that explains a missing primary",
			Cites: []string{"SEM §7.2", "FMT I-13", "STO §4", "SEM §9", "CORPUS §5.11"}, Class: ClassMultiPack, Labels: []string{LabelNonconformingWriter},
			Build: scopeBreachBuild,
			// A version whose ts_utc_ns lies outside the hour of the scope it was read from breaches that scope:
			// each is a fact, and the result is incomplete even with a match found;
			// in the primary's scope a breach also explains a missing primary, which ends the lookup there
			// (the tracepack semantics specification §7.2, Identity across the scopes read).
			// A record outside its segment's period but inside its hour breaches nothing
			// (the tracepack storage specification §4, Flush; §5, Completeness).
			// Packs 0 and 2 keep to no scope, which no conforming writer does (the tracepack format specification I-13).
			Expect: &Expectation{
				Packs: []*Expectation{finalizedPack(1, 3), finalizedPack(1, 3), packOf(2, 11, 13)},
				Lookups: []LookupWant{
					{
						ID: "breach", Outcome: "incomplete", Key: txPrimaryKey(1, breachSystemBytes), WindowEnd: new(uint64(3)),
						Records: []LookupRecord{
							txKept(1, false, false, RolePrimary), txCandidate(2, true), txKept(3, false, true, RoleClosing),
						},
						Gaps:     []FactWant{txBreach(0, 0, 1), txBreach(0, 0, 2), txBreach(0, 0, 3)},
						Searched: txSearched(),
					},
					{
						ID: "breach-explains-missing-primary", Outcome: "incomplete",
						Gaps:     []FactWant{{Fact: Fact{Reason: FactNoKey}}, txBreach(2, 1, 13)},
						Searched: []SearchedScope{{Hour: I64(txHour), Indexed: true, Packs: []int{2}}},
					},
					{
						ID: "same-hour-step", Outcome: "matched", Key: txPrimaryKey(1, breachSystemBytes), WindowEnd: new(uint64(3)),
						Records: []LookupRecord{
							txAt(txKept(1, false, false, RolePrimary), 0, 1), txAt(txCandidate(2, true), 0, 1), txAt(txKept(3, false, true, RoleClosing), 0, 1),
						},
						Searched: []SearchedScope{{Hour: I64(txHour), Indexed: true, Packs: []int{1}}},
					},
				},
			},
		},
		{
			ID: "tx-clock-step",
			Title: "versions of window seqs in the hour before the primary's, after a backward clock step: " +
				"outside the comparison, and a seq gap",
			Cites: []string{"SEM §7.2", "SEM §4", "SEM §9", "CORPUS §5.11"}, Class: ClassMultiPack,
			Build: clockStepBuild,
			// The comparison covers the scopes the lookup read: pack 1's version of seq 3, in the hour before the primary's,
			// is not compared with pack 0's, and seq 4, held only by pack 1, is a seq of the window no scope read yielded
			// (the tracepack semantics specification §7.2, Identity across the scopes read).
			Expect: &Expectation{
				Packs: []*Expectation{packOf(1, 1, 2, 3, 5), packOf(1, 3, 4)},
				Lookups: []LookupWant{{
					ID: "outside-comparison-and-seq-gap", Outcome: "matched", Key: txPrimaryKey(1, clockStepSystemBytes), WindowEnd: new(uint64(5)),
					Records: []LookupRecord{
						txKept(1, false, false, RolePrimary), txCandidate(2, true), txKept(5, false, true, RoleClosing),
					},
					Gaps:     []FactWant{{Fact: Fact{Reason: FactSeqGap}}},
					Searched: txSearched(),
				}},
			},
		},
		{
			ID: "tx-coverage-barrier",
			Title: "coverage entries meeting the window or the hours alone, and both; " +
				"a barrier meeting only a conflicted hour; a barrier on both bases",
			Cites: []string{"SEM §7.2", "STO §5", "FMT §5", "SEM §9", "CORPUS §5.11"}, Class: ClassMultiPack,
			Build: coverageBarrierBuild,
			// A coverage entry of a pack read keeps the lookup from unmatched when it meets both the window's seqs
			// and the time range of the hours scheduled, a conflicted hour included (the tracepack semantics specification §7.2).
			// A stop-unclean boundary is a barrier on the epoch basis when it is in the primary's capture's evidence
			// and the lookup read no closing record of the primary's epoch,
			// and on the time basis when its gap meets the hours scheduled, of any capture
			// (the tracepack storage specification §5, Completeness; the tracepack corpus specification §5.11, Barrier).
			// D's stop-unclean is of the primary's epoch, so it is a capture-boundary of that epoch besides.
			Expect: &Expectation{
				Packs: []*Expectation{
					finalizedPack(1, 3), finalizedPack(1, 3), finalizedPack(1, 3), packOf(1, 4), packOf(1, 4), packOf(1, 1),
					finalizedPack(1, 2), packOf(1, 5),
				},
				Lookups: []LookupWant{
					{
						ID: "conflicted-hour-only", Outcome: "incomplete", Key: txPrimaryKey(1, coverageSystemBytes), WindowEnd: new(uint64(3)),
						Records: []LookupRecord{txAt(txKept(1, false, false, RolePrimary), 0, 2), txAt(txKept(3, false, true, RoleClosing), 0, 2)},
						Gaps: []FactWant{
							{Fact: Fact{Reason: FactConflicted, Hours: []I64{I64(txHour + 1)}}},
							{Fact: Fact{Reason: FactCoverage, Hours: []I64{I64(txHour)}, Pack: new(2), Coverage: coverageEntry(2, 2, 1)}, Capture: new(2)},
							{Fact: Fact{Reason: FactBarrier, Boundary: coverageBarrierBoundary(), Basis: []string{BasisTime}}, Capture: new(3)},
						},
						Searched: []SearchedScope{{Hour: I64(txHour), Indexed: true, Packs: []int{2}}},
					},
					{
						ID: "coverage-meets", Outcome: "incomplete", Key: txPrimaryKey(1, coverageSystemBytes), WindowEnd: new(uint64(3)),
						Records: []LookupRecord{txAt(txKept(1, false, false, RolePrimary), 0, 1), txAt(txKept(3, false, true, RoleClosing), 0, 1)},
						Gaps: []FactWant{
							{Fact: Fact{Reason: FactCoverage, Hours: []I64{I64(txHour)}, Pack: new(1), Coverage: coverageEntry(2, 2, 0)}, Capture: new(1)},
						},
						Searched: []SearchedScope{{Hour: I64(txHour), Indexed: true, Packs: []int{1}}},
					},
					{
						ID: "coverage-misses", Outcome: "unmatched", Key: txPrimaryKey(1, coverageSystemBytes), WindowEnd: new(uint64(3)),
						Records:  []LookupRecord{txKept(1, false, false, RolePrimary), txKept(3, false, true, RoleClosing)},
						Searched: txSearched(),
					},
					{
						ID: "epoch-and-time-barrier", Outcome: "incomplete", Key: txPrimaryKey(1, coverageSystemBytes),
						Records: []LookupRecord{txAt(txKept(1, false, false, RolePrimary), 0, 6)},
						Gaps: []FactWant{
							{Fact: Fact{Reason: FactBarrier, Boundary: twoBasesBoundary(), Basis: []string{BasisEpoch, BasisTime}}, Capture: new(4)},
							{
								Fact:    Fact{Reason: FactCaptureBoundary, Boundary: &FactBoundary{Seq: U64(twoBasesSeq), BoundaryKind: "stop-unclean", Epoch: 1}},
								Capture: new(4),
							},
							{Fact: Fact{Reason: FactOpenWindow}},
						},
						Searched: []SearchedScope{{Hour: I64(txHour), Indexed: true, Packs: []int{6}}},
					},
				},
			},
		},
	}
}

// conflictedSystemBytes are the System Bytes of the transactions of tx-conflicted-scope.
const conflictedSystemBytes uint32 = 0xD1

// conflictedScopeBuild builds tx-conflicted-scope, their quality evaluated, one block each, every record of epoch 1:
// packs 0 and 1, two archives of capture A's hour H, each its own replacement set, each 1 S1F1 x and 2 S1F2 x;
// pack 2, a segment of capture C in hour H, 1 S1F1 x;
// packs 3 and 4, two archives of C's hour H + 1, each its own replacement set, each 2 an annotation in H + 1;
// pack 5, a segment of C in hour H + 2, 3 S1F2 x and 4 a socket-close in H + 2.
// It checks that each pair of archives has the same rank and different replacement sets, and that 3 is a candidate of C's 1.
func conflictedScopeBuild(seed string) (*Built, error) {
	x := conflictedSystemBytes
	note, err := newNote(2, "a record of the conflicted hour")
	if err != nil {
		return nil, err
	}
	note.TSUTCNs = hourAt(1, 2)
	closed, err := newSocketEvent(4, 1, tracepack.EventSocketClose)
	if err != nil {
		return nil, err
	}
	closed.TSUTCNs = hourAt(2, 4)
	reply := txReply(3, 1, 2, x)
	reply.TSUTCNs = hourAt(2, 3)
	primary := txPrimary(1, 1, x)
	if !sameKey(&primary, &reply) || reply.Dir == primary.Dir {
		return nil, errors.New("corpus: the record at 3 is not a candidate of 1")
	}
	a := []tracepack.Record{primary, txReply(2, 1, 2, x)}
	packs, err := writeMultiPacks(seed, []multiPack{
		{capture: 0, archive: true, blocks: [][]tracepack.Record{a}, edit: qualityEvaluated},
		{capture: 0, archive: true, blocks: [][]tracepack.Record{a}, edit: qualityEvaluated},
		{capture: 1, blocks: [][]tracepack.Record{{primary}}, edit: qualityEvaluated},
		{capture: 1, hour: 1, archive: true, blocks: [][]tracepack.Record{{note}}, edit: qualityEvaluated},
		{capture: 1, hour: 1, archive: true, blocks: [][]tracepack.Record{{note}}, edit: qualityEvaluated},
		{capture: 1, hour: 2, blocks: [][]tracepack.Record{{reply, closed}}, edit: qualityEvaluated},
	})
	if err != nil {
		return nil, err
	}
	for _, pair := range [][2]int{{0, 1}, {3, 4}} {
		if err := checkEqualRank(packs[pair[0]].Bytes, packs[pair[1]].Bytes); err != nil {
			return nil, fmt.Errorf("corpus: packs %d and %d: %w", pair[0], pair[1], err)
		}
	}
	later := txLookupIn(seed, "later-hour", 1, 1, 2, 3, 4, 5)
	later.MaxScopes = 3

	return &Built{Packs: packs, Lookups: []LookupSpec{later, txLookup(seed, "primary-hour", 1, 0, 1)}}, nil
}

// checkEqualRank checks that the packs a and b are archives of one scope with the same rank, scope_generation and publisher_epoch,
// each a complete replacement set of its own (the tracepack storage specification §2, §4 and §6).
func checkEqualRank(a, b []byte) error {
	ha, err := packHeader(a)
	if err != nil {
		return err
	}
	hb, err := packHeader(b)
	if err != nil {
		return err
	}
	ma, mb := ha.Meta, hb.Meta
	switch {
	case ma.PackRole != tracepack.PackRoleArchive || mb.PackRole != tracepack.PackRoleArchive:
		return errors.New("not two archives")
	case ma.ScopeGeneration == nil || mb.ScopeGeneration == nil || *ma.ScopeGeneration != *mb.ScopeGeneration ||
		ma.PublisherEpoch == nil || mb.PublisherEpoch == nil || *ma.PublisherEpoch != *mb.PublisherEpoch:
		return errors.New("not of the same rank")
	case ma.ReplacementSetID == nil || mb.ReplacementSetID == nil || *ma.ReplacementSetID == *mb.ReplacementSetID:
		return errors.New("not of two replacement sets")
	case *ma.ReplacementSetSize != 1 || *mb.ReplacementSetSize != 1:
		return errors.New("not each a complete replacement set")
	case ha.CaptureID != hb.CaptureID || hourOf(ma.PeriodStart) != hourOf(mb.PeriodStart):
		return errors.New("not of one scope")
	default:
		return nil
	}
}

// breachSystemBytes are the System Bytes of the transactions of tx-scope-breach.
const breachSystemBytes uint32 = 0xD2

// breachExplainsPrimary is the seq of the primary tx-scope-breach's last lookup names, which pack 2 does not hold.
const breachExplainsPrimary uint64 = 12

// scopeBreachBuild builds tx-scope-breach, three segments, their quality evaluated, every record of epoch 1:
// pack 0 of capture A, written in hour H + 1 with its records, 1 S1F1 x, 2 S1F2 x and 3 a socket-close, its period then moved
// into hour H's first minute by raw surgery (movedPeriod), its footer as written;
// pack 1 of capture D in hour H's first minute, the same records 30 minutes into H;
// pack 2 of capture E in hour H, block 0 holding 11 an annotation in H and block 1 13 an annotation,
// moved with its footer statistics into hour H + 1 (movedBlock).
// It checks that 2 is a candidate of 1, where each pack's period and records lie, and that pack 2 holds no seq 12.
func scopeBreachBuild(seed string) (*Built, error) {
	x := breachSystemBytes
	transaction := func(hour, ms int64) ([]tracepack.Record, error) {
		closed, err := newSocketEvent(3, 1, tracepack.EventSocketClose)
		if err != nil {
			return nil, err
		}
		rs := []tracepack.Record{txPrimary(1, 1, x), txReply(2, 1, 2, x), closed}
		for i := range rs {
			rs[i].TSUTCNs = hourAt(hour, ms+int64(rs[i].Seq))
		}

		return rs, nil
	}
	a, err := transaction(1, 0)
	if err != nil {
		return nil, err
	}
	d, err := transaction(0, halfHourMs)
	if err != nil {
		return nil, err
	}
	if !sameKey(&a[0], &a[1]) || a[1].Dir == a[0].Dir {
		return nil, errors.New("corpus: the record at 2 is not a candidate of 1")
	}
	below, err := newNote(breachExplainsPrimary-1, "a record of the primary's hour below it")
	if err != nil {
		return nil, err
	}
	above, err := newNote(breachExplainsPrimary+1, "a record above the primary, moved to the next hour")
	if err != nil {
		return nil, err
	}
	packs, err := writeMultiPacks(seed, []multiPack{
		{capture: 0, hour: 1, blocks: [][]tracepack.Record{a}, edit: qualityEvaluated},
		{capture: 1, blocks: [][]tracepack.Record{d}, edit: qualityEvaluated},
		{capture: 2, blocks: [][]tracepack.Record{{below}, {above}}, edit: qualityEvaluated},
	})
	if err != nil {
		return nil, err
	}
	if packs[0].Bytes, err = movedPeriod(packs[0].Bytes, -1); err != nil {
		return nil, fmt.Errorf("corpus: pack 0: %w", err)
	}
	if packs[2].Bytes, err = movedBlock(packs[2].Bytes, 1, above.TSUTCNs, hourAt(1, int64(above.Seq))); err != nil {
		return nil, fmt.Errorf("corpus: pack 2: %w", err)
	}
	if err := checkBreachPlaces(packs); err != nil {
		return nil, err
	}
	explains := txLookupIn(seed, "breach-explains-missing-primary", 2, breachExplainsPrimary, 2)
	explains.MaxScopes = 2

	return &Built{Packs: packs, Lookups: []LookupSpec{
		txLookup(seed, "breach", 1, 0),
		explains,
		txLookupIn(seed, "same-hour-step", 1, 1, 1),
	}}, nil
}

// checkBreachPlaces checks the packs of tx-scope-breach: each pack's period in hour H,
// pack 0's records in H + 1, pack 1's in H outside its period, pack 2's seq 11 in H and 13 in H + 1, and no seq 12.
func checkBreachPlaces(packs []PackBuilt) error {
	copies, err := packCopies(packs)
	if err != nil {
		return err
	}
	for n, p := range packs {
		h, err := packHeader(p.Bytes)
		if err != nil {
			return err
		}
		if h.Meta.PeriodStart != hourAt(0, 0) || h.Meta.PeriodEnd != hourAt(0, minuteMs) {
			return fmt.Errorf("corpus: pack %d's period is not hour H's first minute", n)
		}
	}
	for seq, c := range copies[0] {
		if hourOf(c.ts) != txHour+1 {
			return fmt.Errorf("corpus: pack 0's record at %d does not lie in hour H + 1", seq)
		}
	}
	for seq, c := range copies[1] {
		if hourOf(c.ts) != txHour || c.ts < hourAt(0, minuteMs) {
			return fmt.Errorf("corpus: pack 1's record at %d does not lie in hour H outside the period", seq)
		}
	}
	below, above := copies[2][breachExplainsPrimary-1], copies[2][breachExplainsPrimary+1]
	_, twelve := copies[2][breachExplainsPrimary]
	if len(copies[2]) != 2 || twelve || below.block != 0 || hourOf(below.ts) != txHour || above.block != 1 || hourOf(above.ts) != txHour+1 {
		return errors.New("corpus: pack 2 does not hold 11 in block 0 in hour H and 13 in block 1 in hour H + 1 alone")
	}

	return nil
}

// movedPeriod returns pack with its pack metadata period moved by hours hours, every other byte as stored but the header's CRCs:
// its records and footer stay where the writer put them.
func movedPeriod(pack []byte, hours int64) ([]byte, error) {
	return editMeta(pack, func(meta []byte) ([]byte, error) {
		s := Section{Len: len(meta)}
		for _, tag := range []uint16{metaPeriodStartTag, metaPeriodEndTag} {
			at, err := EntryOffset(meta, s, tag, 0)
			if err != nil {
				return nil, err
			}
			old := binary.LittleEndian.Uint64(meta[at+tlv.HeaderLen:])
			if err := setLE64(meta, at+tlv.HeaderLen, old, uint64(int64(old)+hours*hourNs)); err != nil {
				return nil, err
			}
		}

		return meta, nil
	})
}

// movedBlock returns pack, a finalized pack whose last block i holds one record of epoch 1 at from, with that record at to,
// in the block and in the footer statistics it determines, so that the footer stays truthful:
// the block's F-2 ts_min and ts_max, the ts_min and ts_max of its F-3 summary's epoch entry,
// and F-5's ts_max and the ts_max of its epoch entry.
func movedBlock(pack []byte, i int, from, to int64) ([]byte, error) {
	out, err := ReencodeBlocks(pack, func(k int, b *Block) error {
		if k != i {
			return nil
		}
		if len(b.Headers) != 1 || b.Headers[0].TSUTCNs != from || b.Headers[0].Epoch != 1 {
			return fmt.Errorf("%w: block %d does not hold one record of epoch 1 at %d", ErrPackEdit, i, from)
		}
		b.Headers[0].TSUTCNs = to

		return nil
	})
	if err != nil {
		return nil, err
	}
	out, err = PatchFooter(out, func(d []byte) ([]byte, error) {
		e, err := F2Entry(d, i)
		if err != nil {
			return nil, err
		}
		for _, off := range []int{f2TSMinOff, f2TSMaxOff} {
			if err := setLE64(d, e.Off+off, uint64(from), uint64(to)); err != nil {
				return nil, err
			}
		}
		f3, err := F3List(d, i)
		if err != nil {
			return nil, err
		}
		for _, tag := range []uint16{epochTSMinTag, epochTSMaxTag} {
			if err := setNested(d, f3, f3EpochTag, 0, tag, uint64(from), uint64(to)); err != nil {
				return nil, err
			}
		}
		f5, err := F5List(d)
		if err != nil {
			return nil, err
		}
		at, err := EntryOffset(d, f5, f5TSMaxTag, 0)
		if err != nil {
			return nil, err
		}
		if err := setLE64(d, at+tlv.HeaderLen, uint64(from), uint64(to)); err != nil {
			return nil, err
		}

		return d, setNested(d, f5, f5EpochTag, 0, epochTSMaxTag, uint64(from), uint64(to))
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// txBreach returns the scope-breach fact of the version at seq that the read of hour H yielded from block block of pack pack.
func txBreach(pack, block int, seq uint64) FactWant {
	return FactWant{Fact: Fact{Reason: FactScopeBreach, Hours: []I64{I64(txHour)}, Pack: new(pack), Block: new(block), Seq: new(U64(seq))}}
}

// clockStepSystemBytes are the System Bytes of the transaction of tx-clock-step.
const clockStepSystemBytes uint32 = 0xD3

// clockStepBuild builds tx-clock-step, two segments of capture A, their quality evaluated, one block each, every record of epoch 1:
// pack 0 in hour H, 1 S1F1 x, 2 S1F2 x, 3 an annotation, 5 a socket-close;
// pack 1 in hour H − 1, as a recorder writes after a backward clock step, 3 an annotation of other text and 4 an annotation.
// It checks that 2 is a candidate of 1, that pack 1's records lie in hour H − 1, that its 3 differs from pack 0's,
// and that only pack 1 holds seq 4.
func clockStepBuild(seed string) (*Built, error) {
	x := clockStepSystemBytes
	closed, err := newSocketEvent(5, 1, tracepack.EventSocketClose)
	if err != nil {
		return nil, err
	}
	notes := make([]tracepack.Record, 3)
	for i, n := range []struct {
		seq  uint64
		text string
	}{{3, "a record of the window"}, {3, "another version of it, after a backward clock step"}, {4, "a record only the hour before holds"}} {
		if notes[i], err = newNote(n.seq, n.text); err != nil {
			return nil, err
		}
	}
	for i := 1; i < len(notes); i++ {
		notes[i].TSUTCNs = hourAt(-1, int64(notes[i].Seq))
	}
	a := []tracepack.Record{txPrimary(1, 1, x), txReply(2, 1, 2, x), notes[0], closed}
	if !sameKey(&a[0], &a[1]) || a[1].Dir == a[0].Dir {
		return nil, errors.New("corpus: the record at 2 is not a candidate of 1")
	}
	if bytes.Equal(notes[0].Payload, notes[1].Payload) {
		return nil, errors.New("corpus: the two versions of seq 3 hold the same bytes")
	}
	packs, err := writeMultiPacks(seed, []multiPack{
		{capture: 0, blocks: [][]tracepack.Record{a}, edit: qualityEvaluated},
		{capture: 0, hour: -1, blocks: [][]tracepack.Record{notes[1:]}, edit: qualityEvaluated},
	})
	if err != nil {
		return nil, err
	}
	copies, err := packCopies(packs)
	if err != nil {
		return nil, err
	}
	_, four := copies[0][4]
	for seq, c := range copies[1] {
		if hourOf(c.ts) != txHour-1 {
			return nil, fmt.Errorf("corpus: pack 1's record at %d does not lie in hour H − 1", seq)
		}
	}
	if four || copies[0][3].same(copies[1][3]) {
		return nil, errors.New("corpus: pack 0 holds seq 4, or the packs' copies of seq 3 are the same")
	}

	return &Built{Packs: packs, Lookups: []LookupSpec{txLookup(seed, "outside-comparison-and-seq-gap", 1, 0, 1)}}, nil
}

// coverageSystemBytes are the System Bytes of the transactions of tx-coverage-barrier.
const coverageSystemBytes uint32 = 0xD4

// Where the stop-unclean boundaries of tx-coverage-barrier lie:
// C's at seq 1, 1 ms into hour H + 3, its gap from 1 ms to 8 ms into hour H + 1;
// D's at seq 5, 5 ms into hour H + 2, its gap from 10 ms into hour H to 4 ms into hour H + 2.
const (
	coverageBarrierSeq  uint64 = 1
	coverageBarrierHour        = 3
	twoBasesSeq         uint64 = 5
	twoBasesHour               = 2
)

// coverageBarrierBuild builds tx-coverage-barrier, their quality evaluated, one block each, every record of epoch 1,
// the captures A, K, B, C and D numbered 0 to 4:
// pack 0 of A in hour H, 1 S1F1 x, 2 an annotation, 3 a socket-close, its coverage entries {A, seqs 2 to 2, a time in H + 5}
// and {A, seqs 10 to 12, a time in H};
// pack 1 of K in H, the same records, its coverage entry {K, seqs 2 to 2, a time in H};
// pack 2 of B in H, the same records, its coverage entry {B, seqs 2 to 2, a time in H + 1};
// packs 3 and 4, two archives of B's hour H + 1, each its own replacement set, each 4 an annotation in H + 1;
// pack 5 of C in hour H + 3, 1 a stop-unclean, its gap in H + 1 (coverageBarrierBoundary);
// pack 6 of D in H, 1 S1F1 x, 2 an annotation; pack 7 of D in hour H + 2, 5 a stop-unclean, its gap from H to H + 2 (twoBasesBoundary).
// Each coverage entry is a segment's: the format leaves it optional outside a repair.
// It checks the pair of archives, the boundaries and their gaps, and that each segment kept its coverage entries.
func coverageBarrierBuild(seed string) (*Built, error) {
	x := coverageSystemBytes
	window := func(capture int) ([]tracepack.Record, error) {
		closed, err := newSocketEvent(3, 1, tracepack.EventSocketClose)
		if err != nil {
			return nil, err
		}
		note, err := newNote(2, fmt.Sprintf("a record of capture %d's window", capture))
		if err != nil {
			return nil, err
		}

		return []tracepack.Record{txPrimary(1, 1, x), note, closed}, nil
	}
	var windows [3][]tracepack.Record
	for k := range windows {
		var err error
		if windows[k], err = window(k); err != nil {
			return nil, err
		}
	}
	archived, err := newNote(4, "a record of the conflicted hour")
	if err != nil {
		return nil, err
	}
	archived.TSUTCNs = hourAt(1, 4)
	b := coverageBarrierBoundary()
	c, err := txBoundaryRecord(coverageBarrierSeq, 1, int64(*b.TS), tracepack.BoundaryKindStopUnclean, &[2]int64{int64(*b.GapStart), int64(*b.GapEnd)})
	if err != nil {
		return nil, err
	}
	b = twoBasesBoundary()
	d, err := txBoundaryRecord(twoBasesSeq, 1, int64(*b.TS), tracepack.BoundaryKindStopUnclean, &[2]int64{int64(*b.GapStart), int64(*b.GapEnd)})
	if err != nil {
		return nil, err
	}
	dNote, err := newNote(2, "a record of D's scope that bounds no window")
	if err != nil {
		return nil, err
	}
	withCoverage := func(capture int, entries ...Coverage) func(m *tracepack.PackMeta) {
		return func(m *tracepack.PackMeta) {
			qualityEvaluated(m)
			id := captureOf(seed, capture)
			for _, e := range entries {
				m.Coverage = append(m.Coverage, tracepack.Coverage{
					CaptureID: new(id), SeqFirst: new(uint64(*e.SeqFirst)), SeqLast: new(uint64(*e.SeqLast)),
					TimeStart: new(int64(*e.TimeStart)), TimeEnd: new(int64(*e.TimeEnd)),
				})
			}
		}
	}
	packs, err := writeMultiPacks(seed, []multiPack{
		{capture: 0, blocks: [][]tracepack.Record{windows[0]}, edit: withCoverage(0, *coverageEntry(2, 2, 5), *coverageEntry(10, 12, 0))},
		{capture: 1, blocks: [][]tracepack.Record{windows[1]}, edit: withCoverage(1, *coverageEntry(2, 2, 0))},
		{capture: 2, blocks: [][]tracepack.Record{windows[2]}, edit: withCoverage(2, *coverageEntry(2, 2, 1))},
		{capture: 2, hour: 1, archive: true, blocks: [][]tracepack.Record{{archived}}, edit: qualityEvaluated},
		{capture: 2, hour: 1, archive: true, blocks: [][]tracepack.Record{{archived}}, edit: qualityEvaluated},
		{capture: 3, hour: coverageBarrierHour, blocks: [][]tracepack.Record{{c}}, edit: qualityEvaluated},
		{capture: 4, blocks: [][]tracepack.Record{{txPrimary(1, 1, x), dNote}}, edit: qualityEvaluated},
		{capture: 4, hour: twoBasesHour, blocks: [][]tracepack.Record{{d}}, edit: qualityEvaluated},
	})
	if err != nil {
		return nil, err
	}
	if err := checkEqualRank(packs[3].Bytes, packs[4].Bytes); err != nil {
		return nil, fmt.Errorf("corpus: packs 3 and 4: %w", err)
	}
	switch {
	case hourOf(int64(*coverageBarrierBoundary().GapStart)) != txHour+1 || hourOf(int64(*coverageBarrierBoundary().GapEnd)) != txHour+1:
		return nil, errors.New("corpus: C's gap does not lie in hour H + 1")
	case hourOf(int64(*twoBasesBoundary().GapStart)) != txHour:
		return nil, errors.New("corpus: D's gap does not meet hour H")
	case !isBoundary(&c, tracepack.BoundaryKindStopUnclean, 1) || !isBoundary(&d, tracepack.BoundaryKindStopUnclean, 1):
		return nil, errors.New("corpus: C's and D's boundaries are not stop-uncleans of epoch 1")
	}
	for n, want := range []int{2, 1, 1} {
		h, err := packHeader(packs[n].Bytes)
		if err != nil {
			return nil, err
		}
		if len(h.Meta.Coverage) != want {
			return nil, fmt.Errorf("corpus: pack %d holds %d coverage entries, not %d", n, len(h.Meta.Coverage), want)
		}
	}

	cov := func(id string, capture int, view ...int) LookupSpec {
		return txLookupIn(seed, id, capture, 1, view...)
	}
	conflicted := cov("conflicted-hour-only", 2, 2, 3, 4, 5)
	conflicted.MaxScopes = 2

	return &Built{Packs: packs, Lookups: []LookupSpec{
		conflicted, cov("coverage-meets", 1, 1), cov("coverage-misses", 0, 0), cov("epoch-and-time-barrier", 4, 6, 7),
	}}, nil
}

// coverageEntry returns a coverage entry of tx-coverage-barrier without its capture_id: seqs first to last,
// and the time from 1 ms to 999 ms into the hour hour hours after H.
func coverageEntry(first, last uint64, hour int64) *Coverage {
	return &Coverage{
		SeqFirst: new(U64(first)), SeqLast: new(U64(last)), TimeStart: new(I64(hourAt(hour, 1))), TimeEnd: new(I64(hourAt(hour, 999))),
	}
}

// coverageBarrierBoundary returns C's stop-unclean of tx-coverage-barrier, its capture_id left to the fact's Capture:
// seq 1 of epoch 1, 1 ms into hour H + 3, its gap from 1 ms to 8 ms into hour H + 1.
func coverageBarrierBoundary() *FactBoundary {
	return &FactBoundary{
		Seq: U64(coverageBarrierSeq), BoundaryKind: "stop-unclean", TS: new(I64(hourAt(coverageBarrierHour, 1))), Epoch: 1,
		GapStart: new(I64(hourAt(1, 1))), GapEnd: new(I64(hourAt(1, 8))),
	}
}

// twoBasesBoundary returns D's stop-unclean of tx-coverage-barrier, its capture_id left to the fact's Capture:
// seq 5 of epoch 1, 5 ms into hour H + 2, its gap from 10 ms into hour H to 4 ms into hour H + 2.
func twoBasesBoundary() *FactBoundary {
	return &FactBoundary{
		Seq: U64(twoBasesSeq), BoundaryKind: "stop-unclean", TS: new(I64(hourAt(twoBasesHour, int64(twoBasesSeq)))), Epoch: 1,
		GapStart: new(I64(hourAt(0, 10))), GapEnd: new(I64(hourAt(twoBasesHour, 4))),
	}
}
