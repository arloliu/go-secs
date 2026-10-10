package corpus

import (
	"errors"
	"fmt"

	"github.com/arloliu/go-secs/tracepack"
)

// rejectReasonNotSelected is the reason code of a Reject.req for a message of an entity not selected, header byte 3 (SEMI E37 §8.3.20).
const rejectReasonNotSelected byte = 4

// txEvidenceVectors returns the recipes of the tx group that check what keeps a lookup from establishing absence
// beside a window it closes: evidence of the primary's epoch, outcome records and a source that is not complete
// (the tracepack semantics specification §7.2 and §9, the tracepack storage specification §5).
func txEvidenceVectors() []Recipe {
	return []Recipe{
		{
			ID:    "tx-epoch-evidence",
			Title: "an ordering-uncertain record of the epoch below the primary, and a capture-boundary of the epoch in an hour not read",
			Cites: []string{"SEM §7.2", "SEM §9", "CORPUS §5.11", "CORPUS §8"}, Class: ClassMultiPack,
			Build: epochEvidenceBuild,
			// An ordering-uncertain record of the primary's epoch in a scope read counts at any seq, below the primary too;
			// a capture-boundary of the primary's epoch counts wherever the per-capture evidence has it, in an hour not scheduled too,
			// since the evidence is folded from every source pack of the capture
			// (the tracepack semantics specification §7.2, What prevents unmatched; the tracepack corpus specification §8).
			// Each is a fact beside the match, which stays matched.
			Expect: &Expectation{
				Packs: []*Expectation{
					withStats(finalizedPack(1, 4), []EpochWant{closedAt(1, 4)}),
					withStats(finalizedPack(1, 3), []EpochWant{closedAt(1, 3)}),
					withStats(packOf(1, 0), []EpochWant{{Epoch: 1}}, BoundaryWant{Seq: 0, Kind: tracepack.BoundaryKindStart}),
				},
				Lookups: []LookupWant{
					{
						ID: "capture-boundary-outside", Outcome: "matched", Key: txPrimaryKey(1, evidenceSystemBytes['x']), WindowEnd: new(uint64(3)),
						Records: []LookupRecord{
							txAt(txKept(1, false, false, RolePrimary), 0, 1), txAt(txCandidate(2, true), 0, 1),
							txAt(txKept(3, false, true, RoleClosing), 0, 1),
						},
						Gaps: []FactWant{{
							Fact: Fact{Reason: FactCaptureBoundary, Boundary: &FactBoundary{Seq: 0, BoundaryKind: "start", Epoch: 1}}, Capture: new(1),
						}},
						Searched: []SearchedScope{{Hour: I64(txHour), Indexed: true, Packs: []int{1}}},
					},
					{
						ID: "ordering-uncertain-below", Outcome: "matched", Key: txPrimaryKey(1, evidenceSystemBytes['x']), WindowEnd: new(uint64(4)),
						Records: []LookupRecord{
							txKept(2, false, false, RolePrimary), txCandidate(3, true), txKept(4, false, true, RoleClosing),
						},
						Gaps:     []FactWant{{Fact: Fact{Reason: FactOrderingUncertain}}},
						Searched: txSearched(),
					},
				},
			},
		},
		{
			ID: "tx-outcome-records",
			Title: "a pack with quality_evaluated false, Reject.req records with SType unavailable and available, " +
				"and a T3 timer-expiry without identifiers",
			Cites: []string{"SEM §7.2", "SEM §9", "SEM §6", "FMT §7.2", "CORPUS §5.11"}, Class: ClassMultiPack, Labels: []string{LabelNonconformingWriter},
			Build: outcomeRecordsBuild,
			// A pack whose quality was not evaluated keeps the lookup from unmatched (the tracepack semantics specification §6, §7.2).
			// An outcome record needs SType available and 7, and a T3 timer-expiry needs the primary's SessionID and System Bytes:
			// the Reject.req at 2, its SType unavailable, and the timer-expiry at 6, without them, play no role and are not kept;
			// the Reject.req at 4 is b's outcome record, reported in the window, never a match
			// (the tracepack semantics specification §7.2, Definitions).
			// A conforming writer of a capture sets a field_validity bit iff the payload holds the field's bytes
			// (the tracepack format specification §7.2): the Reject.req at 2 holds its SType byte with its bit clear.
			Expect: &Expectation{
				Packs: []*Expectation{finalizedPack(1, 2), finalizedPack(1, 7)},
				Lookups: []LookupWant{
					{
						ID: "reject-outcome", Outcome: "unmatched", Key: txPrimaryKey(1, outcomeSystemBytes['b']), WindowEnd: new(uint64(7)),
						Records: []LookupRecord{
							txAt(txKept(3, false, false, RolePrimary), 0, 1), txAt(txKept(4, true, false, RoleOutcome), 0, 1),
							txAt(txKept(7, false, true, RoleClosing), 0, 1),
						},
						Searched: outcomeSearched(),
					},
					outcomeUnmatched("reject-stype-unavailable", 1, 'a'),
					outcomeUnmatched("t3-without-identifiers", 5, 'c'),
					{
						ID: "unevaluated", Outcome: "incomplete", Key: txPrimaryKey(1, outcomeSystemBytes['x']), WindowEnd: new(uint64(2)),
						Records:  []LookupRecord{txKept(1, false, false, RolePrimary), txKept(2, false, true, RoleClosing)},
						Gaps:     []FactWant{{Fact: Fact{Reason: FactUnevaluated, Hours: []I64{I64(txHour)}, Pack: new(0)}}},
						Searched: txSearched(),
					},
				},
			},
		},
		{
			ID:    "tx-unindexed-source",
			Title: "a source that is not complete, and a pack whose evidence alone counts, its scope not indexed",
			Cites: []string{"STO §5", "SEM §7.2", "CORPUS §5.11", "CORPUS §8"}, Class: ClassMultiPack,
			Build: unindexedSourceBuild,
			// Without complete, no scope is indexed and the per-capture evidence is partial: the scope read is cold,
			// and both keep the lookup from unmatched, beside a match (the tracepack storage specification §5).
			// With complete, the scope of an evidence pack is not indexed: hour H + 1, scheduled and without a view pack, is searched cold,
			// its scope without packs (the tracepack corpus specification §8).
			Expect: &Expectation{
				Packs: []*Expectation{withStats(finalizedPack(1, 4), []EpochWant{closedAt(1, 4)}), packOf(1, 5)},
				Lookups: []LookupWant{
					{
						ID: "evidence-pack", Outcome: "matched", Key: txPrimaryKey(1, unindexedSystemBytes['x']), WindowEnd: new(uint64(4)),
						Records:  unindexedRecords(),
						Gaps:     []FactWant{{Fact: Fact{Reason: FactCold, Hours: []I64{I64(txHour + 1)}}}},
						Searched: []SearchedScope{{Hour: I64(txHour), Indexed: true, Packs: []int{0}}, {Hour: I64(txHour + 1), Packs: []int{}}},
					},
					{
						ID: "incomplete-source", Outcome: "matched", Key: txPrimaryKey(1, unindexedSystemBytes['x']), WindowEnd: new(uint64(4)),
						Records:  unindexedRecords(),
						Gaps:     unindexedFacts(),
						Searched: []SearchedScope{{Hour: I64(txHour), Packs: []int{0}}},
					},
					{
						ID: "unanswered-incomplete-source", Outcome: "incomplete", Key: txPrimaryKey(1, unindexedSystemBytes['y']), WindowEnd: new(uint64(4)),
						Records:  []LookupRecord{txKept(3, false, false, RolePrimary), txKept(4, false, true, RoleClosing)},
						Gaps:     unindexedFacts(),
						Searched: []SearchedScope{{Hour: I64(txHour), Packs: []int{0}}},
					},
				},
			},
		},
	}
}

// evidenceSystemBytes are the System Bytes of the data records of tx-epoch-evidence, by letter.
var evidenceSystemBytes = map[byte]uint32{'x': 0xA1, 'z': 0xA2}

// epochEvidenceBuild builds tx-epoch-evidence, three segments, their quality evaluated, one block each:
// pack 0 of capture A in hour H: 1 S1F2 z equipment-to-host, ordering-uncertain, 2 S1F1 x, 3 S1F2 x, 4 a socket-close;
// pack 1 of capture B in hour H: 1 S1F1 x, 2 S1F2 x, 3 a socket-close;
// pack 2 of capture B in hour H − 1: 0 a capture-boundary start; every record of epoch 1.
// It checks the replies' keys, that only A's 1 carries ordering-uncertain, and that the start lies in hour H − 1.
func epochEvidenceBuild(seed string) (*Built, error) {
	sb := evidenceSystemBytes
	closedA, err := newSocketEvent(4, 1, tracepack.EventSocketClose)
	if err != nil {
		return nil, err
	}
	closedB, err := newSocketEvent(3, 1, tracepack.EventSocketClose)
	if err != nil {
		return nil, err
	}
	start, err := txBoundaryRecord(0, 1, hourAt(-1, 0), tracepack.BoundaryKindStart, nil)
	if err != nil {
		return nil, err
	}
	uncertain := txReply(1, 1, 2, sb['z'])
	uncertain.Quality |= tracepack.QualityOrderingUncertain
	a := []tracepack.Record{uncertain, txPrimary(2, 1, sb['x']), txReply(3, 1, 2, sb['x']), closedA}
	b := []tracepack.Record{txPrimary(1, 1, sb['x']), txReply(2, 1, 2, sb['x']), closedB}
	switch {
	case !sameKey(&a[1], &a[2]) || a[1].Dir == a[2].Dir || !sameKey(&b[0], &b[1]) || b[0].Dir == b[1].Dir:
		return nil, errors.New("corpus: a reply does not carry its primary's key")
	case a[1].Quality|a[2].Quality|b[0].Quality|b[1].Quality != 0 || uncertain.Epoch != a[1].Epoch:
		return nil, errors.New("corpus: the ordering-uncertain record is not the only one, or not of the primary's epoch")
	case !isBoundary(&start, tracepack.BoundaryKindStart, 1) || hourOf(start.TSUTCNs) != txHour-1:
		return nil, errors.New("corpus: the start is not of epoch 1 in hour H − 1")
	}
	packs, err := writeMultiPacks(seed, []multiPack{
		{capture: 0, blocks: [][]tracepack.Record{a}, edit: qualityEvaluated},
		{capture: 1, blocks: [][]tracepack.Record{b}, edit: qualityEvaluated},
		{capture: 1, hour: -1, blocks: [][]tracepack.Record{{start}}, edit: qualityEvaluated},
	})
	if err != nil {
		return nil, err
	}

	return &Built{Packs: packs, Lookups: []LookupSpec{
		txLookupIn(seed, "capture-boundary-outside", 1, 1, 1, 2),
		txLookup(seed, "ordering-uncertain-below", 2, 0),
	}}, nil
}

// outcomeSystemBytes are the System Bytes of the transactions of tx-outcome-records, by letter.
var outcomeSystemBytes = map[byte]uint32{'x': 0xB1, 'a': 0xB2, 'b': 0xB3, 'c': 0xB4}

// outcomeRecordsBuild builds tx-outcome-records, two segments in hour H, one block each:
// pack 0 of capture A, its quality not evaluated: 1 S1F1 x, 2 a socket-close;
// pack 1 of capture B, its quality evaluated: 1 S1F1 a, 2 a Reject.req for a, its SType bit clear;
// 3 S1F1 b, 4 a Reject.req for b; 5 S1F1 c, 6 a T3 timer-expiry naming c's stream and function only; 7 a socket-close.
// Every record is of epoch 1; each Reject.req is equipment-to-host and carries the rejected message's SessionID and System Bytes
// (SEMI E37 §8.3.20).
// It checks the fields each Reject.req makes available and that the timer-expiry carries no primary identifier.
func outcomeRecordsBuild(seed string) (*Built, error) {
	sb := outcomeSystemBytes
	closedA, err := newSocketEvent(2, 1, tracepack.EventSocketClose)
	if err != nil {
		return nil, err
	}
	closedB, err := newSocketEvent(7, 1, tracepack.EventSocketClose)
	if err != nil {
		return nil, err
	}
	timer, err := newEvent(6, 1, &tracepack.TransportEvent{
		Event: tracepack.EventTimerExpiry, Timer: new(tracepack.TimerT3), PrimaryStream: new(uint64(1)), PrimaryFunction: new(uint64(1)),
	})
	if err != nil {
		return nil, err
	}
	rejected := func(seq uint64, letter byte) tracepack.Record {
		return newControl(seq, tracepack.DirEquipmentToHost, hsmsFrame(txSessionID, stypeData, rejectReasonNotSelected, stypeRejectReq, sb[letter], nil))
	}
	unavailable := rejected(2, 'a')
	unavailable.FieldValidity &^= tracepack.FieldValiditySType
	b := []tracepack.Record{
		txPrimary(1, 1, sb['a']), unavailable, txPrimary(3, 1, sb['b']), rejected(4, 'b'), txPrimary(5, 1, sb['c']), timer, closedB,
	}
	if err := checkRejects(b); err != nil {
		return nil, err
	}
	ev, err := tracepack.UnmarshalTransportEvent(timer.Payload)
	if err != nil || ev.Event != tracepack.EventTimerExpiry || ev.Timer == nil || *ev.Timer != tracepack.TimerT3 ||
		ev.PrimarySessionID != nil || ev.PrimarySystemBytesPresent {
		return nil, errors.New("corpus: the record at 6 is not a T3 timer-expiry without the primary's identifiers")
	}
	packs, err := writeMultiPacks(seed, []multiPack{
		{capture: 0, blocks: [][]tracepack.Record{{txPrimary(1, 1, sb['x']), closedA}}},
		{capture: 1, blocks: [][]tracepack.Record{b}, edit: qualityEvaluated},
	})
	if err != nil {
		return nil, err
	}

	return &Built{Packs: packs, Lookups: []LookupSpec{
		txLookupIn(seed, "reject-outcome", 1, 3, 1),
		txLookupIn(seed, "reject-stype-unavailable", 1, 1, 1),
		txLookupIn(seed, "t3-without-identifiers", 1, 5, 1),
		txLookup(seed, "unevaluated", 1, 0),
	}}, nil
}

// checkRejects checks the Reject.req records at 2 and 4 of b, tx-outcome-records' pack 1,
// against the primaries at 1 and 3: control records of SType 7 in the opposite direction, of the primary's epoch,
// with the primary's SessionID and System Bytes available; SType unavailable at 2 and available at 4.
func checkRejects(b []tracepack.Record) error {
	key := tracepack.FieldValiditySessionID | tracepack.FieldValiditySystemBytes
	for _, pair := range [][2]int{{0, 1}, {2, 3}} {
		p, r := &b[pair[0]], &b[pair[1]]
		h := r.HSMSHeader()
		if r.Kind != tracepack.KindControl || h.SType != stypeRejectReq || h.Available&key != key || !sameKey(p, r) || r.Dir == p.Dir {
			return fmt.Errorf("corpus: the record at %d is not a Reject.req of %d's key in the opposite direction", r.Seq, p.Seq)
		}
		if h.Available.Has(tracepack.FieldValiditySType) != (r.Seq == 4) {
			return fmt.Errorf("corpus: the SType of the Reject.req at %d is not as planned", r.Seq)
		}
	}

	return nil
}

// outcomeUnmatched returns the result of a lookup of tx-outcome-records' pack 1 from the primary at seq of the letter's transaction,
// whose window holds no record that plays a role: unmatched, bounded by the socket-close at 7.
func outcomeUnmatched(id string, seq uint64, letter byte) LookupWant {
	return LookupWant{
		ID: id, Outcome: "unmatched", Key: txPrimaryKey(1, outcomeSystemBytes[letter]), WindowEnd: new(uint64(7)),
		Records:  []LookupRecord{txAt(txKept(seq, false, false, RolePrimary), 0, 1), txAt(txKept(7, false, true, RoleClosing), 0, 1)},
		Searched: outcomeSearched(),
	}
}

// outcomeSearched returns the scope the lookups over tx-outcome-records' pack 1 search: hour H, indexed, its view pack 1.
func outcomeSearched() []SearchedScope {
	return []SearchedScope{{Hour: I64(txHour), Indexed: true, Packs: []int{1}}}
}

// unindexedSystemBytes are the System Bytes of the transactions of tx-unindexed-source, by letter.
var unindexedSystemBytes = map[byte]uint32{'x': 0xC1, 'y': 0xC2}

// unindexedSourceBuild builds tx-unindexed-source, two segments of capture A, their quality evaluated, one block each:
// pack 0 in hour H, 1 S1F1 x, 2 S1F2 x, 3 S1F1 y, 4 a socket-close; pack 1 in hour H + 1, 5 an annotation; every record of epoch 1.
// It checks that 2 is a candidate of 1 and that 3 is no same-key primary of 1.
func unindexedSourceBuild(seed string) (*Built, error) {
	sb := unindexedSystemBytes
	closed, err := newSocketEvent(4, 1, tracepack.EventSocketClose)
	if err != nil {
		return nil, err
	}
	note, err := newNote(5, "a record of the evidence pack")
	if err != nil {
		return nil, err
	}
	note.TSUTCNs = hourAt(1, 5)
	records := []tracepack.Record{txPrimary(1, 1, sb['x']), txReply(2, 1, 2, sb['x']), txPrimary(3, 1, sb['y']), closed}
	if !sameKey(&records[0], &records[1]) || records[1].Dir == records[0].Dir || sameKey(&records[0], &records[2]) {
		return nil, errors.New("corpus: the record at 2 is not a candidate of 1, or 3 repeats 1's key")
	}
	packs, err := writeMultiPacks(seed, []multiPack{
		{capture: 0, blocks: [][]tracepack.Record{records}, edit: qualityEvaluated},
		{capture: 0, hour: 1, blocks: [][]tracepack.Record{{note}}, edit: qualityEvaluated},
	})
	if err != nil {
		return nil, err
	}
	evidence := txLookup(seed, "evidence-pack", 1, 0)
	evidence.Source.Evidence, evidence.MaxScopes = []int{1}, 2
	incomplete := func(id string, seq uint64) LookupSpec {
		l := txLookup(seed, id, seq, 0)
		l.Source.Complete = false

		return l
	}

	return &Built{Packs: packs, Lookups: []LookupSpec{
		evidence, incomplete("incomplete-source", 1), incomplete("unanswered-incomplete-source", 3),
	}}, nil
}

// unindexedRecords returns the versions a lookup of tx-unindexed-source from the primary at 1 keeps:
// the primary, its valid reply at 2 and the socket-close at 4 bounding the window.
func unindexedRecords() []LookupRecord {
	return []LookupRecord{txKept(1, false, false, RolePrimary), txCandidate(2, true), txKept(4, false, true, RoleClosing)}
}

// unindexedFacts returns the facts of a lookup of tx-unindexed-source over a source that is not complete:
// the scope of H not indexed, and the per-capture evidence partial.
func unindexedFacts() []FactWant {
	return []FactWant{{Fact: Fact{Reason: FactCold, Hours: []I64{I64(txHour)}}}, {Fact: Fact{Reason: FactEvidence}}}
}
