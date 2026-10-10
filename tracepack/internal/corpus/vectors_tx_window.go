package corpus

import (
	"errors"
	"fmt"
	"slices"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// Nested tags of a boundary entry that a planted claim writes beside its seq and kind (the tracepack format specification §10).
const (
	boundaryTSTag    uint16 = 0x0003
	boundaryEpochTag uint16 = 0x0004
)

// txWindowVectors returns the recipes of the tx group that bound the window and check the evidence's closure claims:
// the records that bound a window, the epoch barrier a closing record lifts,
// and footer claims that the records contradict or that name a seq in conflict
// (the tracepack semantics specification §7.2 and §9).
func txWindowVectors() []Recipe {
	return []Recipe{
		{
			ID:    "tx-window-bounds",
			Title: "windows bounded by a same-key primary, a socket-close, a clean stop of another epoch, and by no record read",
			Cites: []string{"SEM §7.2", "SEM §9", "CORPUS §5.11"}, Class: ClassMultiPack,
			Build: windowBoundsBuild,
			// The window ends at the smallest seq above the primary of a same-key primary or of a closing record:
			// the primary at 2 repeats 1's key, the socket-close at 3 ends epoch 1, and the stop at 5 ends the capture, of any epoch
			// (the tracepack semantics specification §7.2, Definitions).
			// Records of another epoch play no role, so the primary at 4 is not kept by the lookups of epoch 1,
			// and the stop at 5, a closing record of any epoch, is kept by each.
			// The stop is of epoch 3, so it is no capture-boundary of epoch 2's; the evidence's close_seqs and stop are each read and true.
			// Capture B's scope holds no record that bounds the window of its primary: the window stays open.
			Expect: &Expectation{
				Packs: []*Expectation{
					withStats(finalizedPack(1, 5), []EpochWant{closedAt(1, 3), {Epoch: 2}, closedAt(3, 5)}, BoundaryWant{Seq: 5, Kind: tracepack.BoundaryKindStop}),
					withStats(finalizedPack(1, 2), []EpochWant{{Epoch: 1}}),
				},
				Lookups: []LookupWant{
					{
						ID: "none-in-scopes", Outcome: "incomplete", Key: txPrimaryKey(1, windowSystemBytes['x']),
						Records:  []LookupRecord{txAt(txKept(1, false, false, RolePrimary), 0, 1)},
						Gaps:     []FactWant{{Fact: Fact{Reason: FactOpenWindow}}},
						Searched: []SearchedScope{{Hour: I64(txHour), Indexed: true, Packs: []int{1}}},
					},
					{
						ID: "same-key-primary", Outcome: "unmatched", Key: txPrimaryKey(1, windowSystemBytes['x']), WindowEnd: new(uint64(2)),
						Records: []LookupRecord{
							txKept(1, false, false, RolePrimary), txKept(2, false, true, RoleSameKeyPrimary),
							txKept(3, false, false, RoleClosing), txKept(5, false, false, RoleClosing),
						},
						Searched: txSearched(),
					},
					{
						ID: "socket-close", Outcome: "unmatched", Key: txPrimaryKey(1, windowSystemBytes['x']), WindowEnd: new(uint64(3)),
						Records: []LookupRecord{
							txKept(2, false, false, RolePrimary), txKept(3, false, true, RoleClosing), txKept(5, false, false, RoleClosing),
						},
						Searched: txSearched(),
					},
					{
						ID: "stop-other-epoch", Outcome: "unmatched", Key: txEpochKey(2, 1, windowSystemBytes['y']), WindowEnd: new(uint64(5)),
						Records:  []LookupRecord{txKept(4, false, false, RolePrimary), txKept(5, false, true, RoleClosing)},
						Searched: txSearched(),
					},
				},
			},
		},
		{
			ID: "tx-epoch-barrier",
			Title: "a stop-unclean barrier beside a stop of another epoch, which bounds the window but keeps the barrier, " +
				"and beside a socket-close of the primary's epoch, which lifts it",
			Cites: []string{"SEM §7.2", "STO §5", "CORPUS §5.11"}, Class: ClassMultiPack,
			Build: epochBarrierBuild,
			// Each capture ends with a stop-unclean of epoch 2 in hour H + 3, its gap inside that hour, so it meets no hour scheduled.
			// It is a barrier on the epoch basis while the lookup read no closing record of the primary's epoch:
			// M's stop at 3 is of epoch 2, so it bounds the window but leaves epoch 1 open;
			// N's socket-close at 3 is of epoch 1, the primary's, and lifts the barrier
			// (the tracepack storage specification §5, Completeness; the tracepack corpus specification §5.11, Barrier).
			// Neither boundary is of epoch 1, so neither is a capture-boundary of the primary's epoch.
			// M's clean stop followed by a stop-unclean is deliberately incoherent, a capture that ends twice:
			// it is the only way to observe that a stop of another epoch bounds the window but does not lift the epoch basis.
			Expect: &Expectation{
				Packs: []*Expectation{
					withStats(finalizedPack(1, 3), []EpochWant{{Epoch: 1}, closedAt(2, 3)}, BoundaryWant{Seq: 3, Kind: tracepack.BoundaryKindStop}),
					withStats(packOf(1, 9), []EpochWant{{Epoch: 2}}, BoundaryWant{Seq: 9, Kind: tracepack.BoundaryKindStopUnclean}),
					withStats(finalizedPack(1, 3), []EpochWant{closedAt(1, 3)}),
					withStats(packOf(1, 9), []EpochWant{{Epoch: 2}}, BoundaryWant{Seq: 9, Kind: tracepack.BoundaryKindStopUnclean}),
				},
				Lookups: []LookupWant{
					{
						ID: "other-epoch-stop", Outcome: "incomplete", Key: txPrimaryKey(1, barrierSystemBytes), WindowEnd: new(uint64(3)),
						Records:  []LookupRecord{txKept(1, false, false, RolePrimary), txKept(3, false, true, RoleClosing)},
						Gaps:     []FactWant{{Fact: Fact{Reason: FactBarrier, Boundary: epochBarrierBoundary(), Basis: []string{BasisEpoch}}, Capture: new(0)}},
						Searched: txSearched(),
					},
					{
						ID: "own-epoch-socket-close", Outcome: "unmatched", Key: txPrimaryKey(1, barrierSystemBytes), WindowEnd: new(uint64(3)),
						Records: []LookupRecord{
							txAt(txKept(1, false, false, RolePrimary), 0, 2), txAt(txKept(3, false, true, RoleClosing), 0, 2),
						},
						Searched: []SearchedScope{{Hour: I64(txHour), Indexed: true, Packs: []int{2}}},
					},
				},
			},
		},
		{
			ID: "tx-closure-contradiction",
			Title: "footer claims naming an annotation, the primary's seq, a stop of another epoch, a socket-close as a stop, " +
				"and a seq below the primary: contradictions",
			Cites: []string{"SEM §7.2", "SEM §9", "FMT §10", "CORPUS §5.11"}, Class: ClassMultiPack, Labels: []string{LabelNonconformingWriter},
			Build: closureContradictionBuild,
			// Each pack's footer claims, consistent with itself and so accepted, what its records do not show;
			// verify finds its block disagreeing with its F-3 summary (the tracepack format specification §10, §13).
			// A claim at or below the primary, or at a seq the lookup read whose versions are not what it names, is a contradiction,
			// never a bound; a contradiction only keeps the result from unmatched
			// (the tracepack semantics specification §7.2, Definitions; the tracepack corpus specification §5.11, Contradiction).
			// The stop claimed at C's 3 is not a record read, so it is a capture-boundary of epoch 1 besides; the window ends at the socket-close.
			// D's claim at 1 is of epoch 2, no capture-boundary of the primary's epoch.
			Expect: &Expectation{
				Packs: []*Expectation{
					claimedPack(7, []EpochWant{closedAt(1, 2), closedAt(2, 5)}),
					claimedPack(3, []EpochWant{closedAt(1, 3), closedAt(2, 3)}, BoundaryWant{Seq: 3, Kind: tracepack.BoundaryKindStop}),
					claimedPack(4, []EpochWant{closedAt(1, 3)}, BoundaryWant{Seq: 3, Kind: tracepack.BoundaryKindStop}),
					claimedPack(4, []EpochWant{closedAt(1, 4)}, BoundaryWant{Seq: 1, Kind: tracepack.BoundaryKindStop}),
				},
				Lookups: []LookupWant{
					{
						ID: "close-seq-names-annotation", Outcome: "matched", Key: txPrimaryKey(1, contradictionSystemBytes['x']), WindowEnd: new(uint64(4)),
						Records: []LookupRecord{
							txKept(1, false, false, RolePrimary), txCandidate(3, true), txKept(4, false, true, RoleClosing),
						},
						Gaps:     []FactWant{txContradiction(2)},
						Searched: txSearched(),
					},
					{
						ID: "closure-at-primary", Outcome: "matched", Key: txEpochKey(2, 1, contradictionSystemBytes['y']), WindowEnd: new(uint64(7)),
						Records: []LookupRecord{
							txKept(5, false, false, RolePrimary), txCandidate(6, true), txKept(7, false, true, RoleClosing),
						},
						Gaps:     []FactWant{txContradiction(5)},
						Searched: txSearched(),
					},
					{
						ID: "e1-closure-names-e2-stop", Outcome: "incomplete", Key: txPrimaryKey(1, contradictionSystemBytes['x']), WindowEnd: new(uint64(3)),
						Records: []LookupRecord{
							txAt(txKept(1, false, false, RolePrimary), 0, 1), txAt(txKept(3, false, true, RoleClosing), 0, 1),
						},
						Gaps:     []FactWant{txContradiction(3)},
						Searched: []SearchedScope{{Hour: I64(txHour), Indexed: true, Packs: []int{1}}},
					},
					{
						ID: "other-epoch-stop-below-primary", Outcome: "matched", Key: txPrimaryKey(1, contradictionSystemBytes['x']), WindowEnd: new(uint64(4)),
						Records: []LookupRecord{
							txAt(txKept(2, false, false, RolePrimary), 0, 3), txAt(txCandidate(3, true), 0, 3), txAt(txKept(4, false, true, RoleClosing), 0, 3),
						},
						Gaps:     []FactWant{txContradiction(1)},
						Searched: []SearchedScope{{Hour: I64(txHour), Indexed: true, Packs: []int{3}}},
					},
					{
						ID: "stop-claim-names-socket-close", Outcome: "incomplete", Key: txPrimaryKey(1, contradictionSystemBytes['x']), WindowEnd: new(uint64(3)),
						Records: []LookupRecord{
							txAt(txKept(1, false, false, RolePrimary), 0, 2), txAt(txKept(3, false, true, RoleClosing), 0, 2),
						},
						Gaps: []FactWant{
							{Fact: Fact{Reason: FactCaptureBoundary, Boundary: &FactBoundary{Seq: 3, BoundaryKind: "stop", Epoch: 1}}, Capture: new(2)},
							txContradiction(3),
						},
						Searched: []SearchedScope{{Hour: I64(txHour), Indexed: true, Packs: []int{2}}},
					},
				},
			},
		},
		{
			ID:    "tx-closure-conflict",
			Title: "a close_seq naming a seq with two versions, one of them the socket-close: the conflict, no contradiction",
			Cites: []string{"SEM §7.2", "SEM §9", "CORPUS §5.11"}, Class: ClassMultiPack,
			Build: closureConflictBuild,
			// Pack 0's footer claims epoch 1 closed at 3, where pack 1 holds an annotation: the scope read yields two versions of 3.
			// The socket-close version is labelled closing by its bytes, but in conflict it bounds nothing, so the window stays open;
			// the claim names a seq whose versions conflict, one of them the event it names: the conflict only, no contradiction
			// (the tracepack semantics specification §7.2, Definitions, Identity across the scopes read).
			// The annotation is kept as the other version of a seq that qualified, with no role.
			Expect: &Expectation{
				Packs: []*Expectation{
					withStats(finalizedPack(1, 3), []EpochWant{closedAt(1, 3)}),
					withStats(packOf(1, 3), []EpochWant{{Epoch: 1}}),
				},
				Lookups: []LookupWant{{
					ID: "conflicting-socket-close", Outcome: "incomplete", Key: txPrimaryKey(1, closureConflictSystemBytes),
					Records: []LookupRecord{
						txKept(1, false, false, RolePrimary), txCandidate(2, true),
						{Seq: 3, Hour: I64(txHour), Conflict: true, Roles: []string{RoleClosing}, InWindow: true},
						{Seq: 3, Hour: I64(txHour), Pack: 1, Conflict: true, Roles: []string{}, InWindow: true},
					},
					Gaps: []FactWant{
						{Fact: Fact{Reason: FactConflict, Hours: []I64{I64(txHour)}, Seq: new(U64(3))}},
						{Fact: Fact{Reason: FactOpenWindow}},
					},
					Searched:  []SearchedScope{{Hour: I64(txHour), Indexed: true, Packs: []int{0, 1}}},
					Conflicts: []ConflictWant{{Capture: 0, Seq: 3, Versions: [][]int{{0}, {1}}}},
				}},
			},
		},
	}
}

// windowSystemBytes are the System Bytes of the transactions of tx-window-bounds, by letter.
var windowSystemBytes = map[byte]uint32{'x': 0x61, 'y': 0x62}

// windowBoundsBuild builds tx-window-bounds, two segments in hour H, their quality evaluated, one block each:
// pack 0 of capture A: 1 S1F1 x and 2 S1F1 x, 3 a socket-close, of epoch 1; 4 S1F1 y of epoch 2; 5 a stop of epoch 3;
// pack 1 of capture B: 1 S1F1 x, 2 an annotation.
// It checks that 2 repeats 1's key, the epochs, that 3 is a socket-close and 5 a stop, and that B's primary carries A's x.
func windowBoundsBuild(seed string) (*Built, error) {
	sb := windowSystemBytes
	closed, err := newSocketEvent(3, 1, tracepack.EventSocketClose)
	if err != nil {
		return nil, err
	}
	stop, err := txBoundaryRecord(5, 3, msAt(5), tracepack.BoundaryKindStop, nil)
	if err != nil {
		return nil, err
	}
	note, err := newNote(2, "a record of B's scope that bounds no window")
	if err != nil {
		return nil, err
	}
	a := []tracepack.Record{
		txPrimary(1, 1, sb['x']), txPrimary(2, 1, sb['x']), closed, txData(4, 2, tracepack.DirHostToEquipment, 1, true, sb['y']), stop,
	}
	b := []tracepack.Record{txPrimary(1, 1, sb['x']), note}
	switch {
	case !sameKey(&a[0], &a[1]) || a[0].Dir != a[1].Dir:
		return nil, errors.New("corpus: the primary at 2 does not repeat the key of 1")
	case a[3].Epoch != 2 || !isSocketClose(&a[2], 1) || !isBoundary(&a[4], tracepack.BoundaryKindStop, 3):
		return nil, errors.New("corpus: the records at 3 to 5 are not a socket-close of epoch 1, data of epoch 2 and a stop of epoch 3")
	case !sameKey(&a[0], &b[0]):
		return nil, errors.New("corpus: B's primary does not carry the fields of A's")
	}
	packs, err := writeMultiPacks(seed, []multiPack{
		{capture: 0, blocks: [][]tracepack.Record{a}, edit: qualityEvaluated},
		{capture: 1, blocks: [][]tracepack.Record{b}, edit: qualityEvaluated},
	})
	if err != nil {
		return nil, err
	}

	return &Built{Packs: packs, Lookups: []LookupSpec{
		txLookupIn(seed, "none-in-scopes", 1, 1, 0, 1),
		txLookup(seed, "same-key-primary", 1, 0, 1),
		txLookup(seed, "socket-close", 2, 0, 1),
		txLookup(seed, "stop-other-epoch", 4, 0, 1),
	}}, nil
}

// barrierSystemBytes are the System Bytes of the primaries of tx-epoch-barrier.
const barrierSystemBytes uint32 = 0x71

// Where the stop-unclean boundaries of tx-epoch-barrier lie: hour H + 3, the boundary record at 9 ms,
// its gap from 1 ms to 8 ms, inside that hour.
const (
	barrierHour              = 3
	barrierGapStartMs        = 1
	barrierGapEndMs          = 8
	barrierSeq        uint64 = 9
)

// epochBarrierBuild builds tx-epoch-barrier, four segments, their quality evaluated, one block each:
// pack 0 of capture M in hour H: 1 S1F1 x and 2 an annotation, of epoch 1, 3 a stop of epoch 2;
// pack 1 of M in hour H + 3: 9 a stop-unclean of epoch 2 (epochBarrierBoundary);
// pack 2 of capture N in hour H: 1 S1F1 x, 2 an annotation, 3 a socket-close, of epoch 1;
// pack 3 of N in hour H + 3: pack 1's stop-unclean.
// It checks the kinds and epochs of the boundaries and of the socket-close, and that the stop-unclean's gap lies in H + 3.
func epochBarrierBuild(seed string) (*Built, error) {
	x := barrierSystemBytes
	stop, err := txBoundaryRecord(3, 2, msAt(3), tracepack.BoundaryKindStop, nil)
	if err != nil {
		return nil, err
	}
	closed, err := newSocketEvent(3, 1, tracepack.EventSocketClose)
	if err != nil {
		return nil, err
	}
	gap := [2]int64{hourAt(barrierHour, barrierGapStartMs), hourAt(barrierHour, barrierGapEndMs)}
	unclean, err := txBoundaryRecord(barrierSeq, 2, hourAt(barrierHour, int64(barrierSeq)), tracepack.BoundaryKindStopUnclean, &gap)
	if err != nil {
		return nil, err
	}
	notes := make([]tracepack.Record, 2)
	for i := range notes {
		if notes[i], err = newNote(2, "a record of the window"); err != nil {
			return nil, err
		}
	}
	m := []tracepack.Record{txPrimary(1, 1, x), notes[0], stop}
	n := []tracepack.Record{txPrimary(1, 1, x), notes[1], closed}
	switch {
	case !isBoundary(&stop, tracepack.BoundaryKindStop, 2) || !isSocketClose(&closed, 1):
		return nil, errors.New("corpus: M's 3 is not a stop of epoch 2, or N's 3 not a socket-close of epoch 1")
	case !isBoundary(&unclean, tracepack.BoundaryKindStopUnclean, 2) || hourOf(gap[0]) != hourOf(gap[1]) || hourOf(gap[0]) != txHour+barrierHour:
		return nil, errors.New("corpus: the stop-unclean is not of epoch 2 with its gap in hour H + 3")
	}
	packs, err := writeMultiPacks(seed, []multiPack{
		{capture: 0, blocks: [][]tracepack.Record{m}, edit: qualityEvaluated},
		{capture: 0, hour: barrierHour, blocks: [][]tracepack.Record{{unclean}}, edit: qualityEvaluated},
		{capture: 1, blocks: [][]tracepack.Record{n}, edit: qualityEvaluated},
		{capture: 1, hour: barrierHour, blocks: [][]tracepack.Record{{unclean}}, edit: qualityEvaluated},
	})
	if err != nil {
		return nil, err
	}

	return &Built{Packs: packs, Lookups: []LookupSpec{
		txLookup(seed, "other-epoch-stop", 1, 0, 1),
		txLookupIn(seed, "own-epoch-socket-close", 1, 1, 2, 3),
	}}, nil
}

// epochBarrierBoundary returns the boundary of the barrier fact of tx-epoch-barrier: the stop-unclean of epoch 2 at seq 9 in hour H + 3,
// its gap from 1 ms to 8 ms into that hour; its capture_id is left to the fact's Capture.
func epochBarrierBoundary() *FactBoundary {
	return &FactBoundary{
		Seq: U64(barrierSeq), BoundaryKind: "stop-unclean", TS: new(I64(hourAt(barrierHour, int64(barrierSeq)))), Epoch: 2,
		GapStart: new(I64(hourAt(barrierHour, barrierGapStartMs))), GapEnd: new(I64(hourAt(barrierHour, barrierGapEndMs))),
	}
}

// contradictionSystemBytes are the System Bytes of the transactions of tx-closure-contradiction, by letter.
var contradictionSystemBytes = map[byte]uint32{'x': 0x81, 'y': 0x82}

// closureContradictionBuild builds tx-closure-contradiction, four segments in hour H, their quality evaluated, one block each,
// each footer then given claims the records do not support, in block 0's F-3 summary and in F-5 together (plantClaims):
// pack 0 of capture A: 1 S1F1 x, 2 an annotation, 3 S1F2 x, 4 a socket-close, of epoch 1;
// 5 S1F1 y, 6 S1F2 y, 7 a socket-close, of epoch 2; its claims epoch 1 closed at 2 and epoch 2 at 5, instead of 4 and 7;
// pack 1 of capture B: 1 S1F1 x and 2 an annotation, of epoch 1, 3 a stop of epoch 2; its claim epoch 1 closed at 3, added;
// pack 2 of capture C: 1 S1F1 x, 2 an annotation, 3 a socket-close, 4 an annotation, of epoch 1; its claim a stop of epoch 1 at 3, added;
// pack 3 of capture D: 1 an annotation, 2 S1F1 x, 3 S1F2 x, 4 a socket-close, of epoch 1; its claim a stop of epoch 2 at 1, added.
// It checks the replies' keys and the closing records' kinds and epochs; the expectation checks each footer's claims as statistics.
func closureContradictionBuild(seed string) (*Built, error) {
	sb := contradictionSystemBytes
	var err error
	ev := func(seq uint64, epoch uint32) tracepack.Record {
		var r tracepack.Record
		if err == nil {
			r, err = newSocketEvent(seq, epoch, tracepack.EventSocketClose)
		}

		return r
	}
	note := func(seq uint64) tracepack.Record {
		var r tracepack.Record
		if err == nil {
			r, err = newNote(seq, "a record that ends nothing")
		}

		return r
	}
	a := []tracepack.Record{
		txPrimary(1, 1, sb['x']), note(2), txReply(3, 1, 2, sb['x']), ev(4, 1),
		txData(5, 2, tracepack.DirHostToEquipment, 1, true, sb['y']), txData(6, 2, tracepack.DirEquipmentToHost, 2, false, sb['y']), ev(7, 2),
	}
	bNote := note(2)
	c := []tracepack.Record{txPrimary(1, 1, sb['x']), note(2), ev(3, 1), note(4)}
	d := []tracepack.Record{note(1), txPrimary(2, 1, sb['x']), txReply(3, 1, 2, sb['x']), ev(4, 1)}
	if err != nil {
		return nil, err
	}
	stop, err := txBoundaryRecord(3, 2, msAt(3), tracepack.BoundaryKindStop, nil)
	if err != nil {
		return nil, err
	}
	b := []tracepack.Record{txPrimary(1, 1, sb['x']), bNote, stop}
	for _, pair := range [][2]*tracepack.Record{{&a[0], &a[2]}, {&a[4], &a[5]}, {&d[1], &d[2]}} {
		if !sameKey(pair[0], pair[1]) || pair[0].Dir == pair[1].Dir {
			return nil, fmt.Errorf("corpus: the record at %d is not a candidate of %d", pair[1].Seq, pair[0].Seq)
		}
	}
	if !isSocketClose(&a[3], 1) || !isSocketClose(&a[6], 2) || !isSocketClose(&c[2], 1) || !isSocketClose(&d[3], 1) ||
		!isBoundary(&b[2], tracepack.BoundaryKindStop, 2) {
		return nil, errors.New("corpus: the closing records are not of the kinds and epochs planned")
	}
	packs, err := writeMultiPacks(seed, []multiPack{
		{capture: 0, blocks: [][]tracepack.Record{a}, edit: qualityEvaluated},
		{capture: 1, blocks: [][]tracepack.Record{b}, edit: qualityEvaluated},
		{capture: 2, blocks: [][]tracepack.Record{c}, edit: qualityEvaluated},
		{capture: 3, blocks: [][]tracepack.Record{d}, edit: qualityEvaluated},
	})
	if err != nil {
		return nil, err
	}
	claims := []struct {
		closes map[uint32]uint64
		bound  *plantedBoundary
	}{
		{closes: map[uint32]uint64{1: 2, 2: 5}},
		{closes: map[uint32]uint64{1: 3}},
		{bound: &plantedBoundary{seq: 3, epoch: 1, ts: c[2].TSUTCNs}},
		{bound: &plantedBoundary{seq: 1, epoch: 2, ts: d[0].TSUTCNs}},
	}
	for n, cl := range claims {
		if packs[n].Bytes, err = plantClaims(packs[n].Bytes, cl.closes, cl.bound); err != nil {
			return nil, fmt.Errorf("corpus: pack %d: %w", n, err)
		}
	}

	return &Built{Packs: packs, Lookups: []LookupSpec{
		txLookup(seed, "close-seq-names-annotation", 1, 0),
		txLookup(seed, "closure-at-primary", 5, 0),
		txLookupIn(seed, "e1-closure-names-e2-stop", 1, 1, 1),
		txLookupIn(seed, "other-epoch-stop-below-primary", 3, 2, 3),
		txLookupIn(seed, "stop-claim-names-socket-close", 2, 1, 2),
	}}, nil
}

// closureConflictSystemBytes are the System Bytes of the transaction of tx-closure-conflict.
const closureConflictSystemBytes uint32 = 0x91

// closureConflictBuild builds tx-closure-conflict, two segments of capture A in hour H, their quality evaluated, one block each:
// pack 0 in the first minute, 1 S1F1 x, 2 S1F2 x, 3 a socket-close, of epoch 1, its footer claiming epoch 1 closed at 3 as written;
// pack 1 in the second minute, 3 an annotation of epoch 1, another version of seq 3.
// It checks that 2 is a candidate of 1, that pack 0's 3 is the socket-close of epoch 1, and that pack 1's 3 is not.
func closureConflictBuild(seed string) (*Built, error) {
	x := closureConflictSystemBytes
	closed, err := newSocketEvent(3, 1, tracepack.EventSocketClose)
	if err != nil {
		return nil, err
	}
	note, err := newNote(3, "another version of the socket-close's seq")
	if err != nil {
		return nil, err
	}
	records := []tracepack.Record{txPrimary(1, 1, x), txReply(2, 1, 2, x), closed}
	switch {
	case !sameKey(&records[0], &records[1]) || records[1].Dir == records[0].Dir:
		return nil, errors.New("corpus: the record at 2 is not a candidate of 1")
	case !isSocketClose(&closed, 1) || isSocketClose(&note, 1) || note.Seq != closed.Seq:
		return nil, errors.New("corpus: the versions of seq 3 are not a socket-close of epoch 1 and an annotation")
	}
	packs, err := writeMultiPacks(seed, []multiPack{
		{capture: 0, blocks: [][]tracepack.Record{records}, edit: qualityEvaluated},
		{capture: 0, minute: 1, blocks: [][]tracepack.Record{{note}}, edit: qualityEvaluated},
	})
	if err != nil {
		return nil, err
	}

	return &Built{Packs: packs, Lookups: []LookupSpec{txLookup(seed, "conflicting-socket-close", 1, 0, 1)}}, nil
}

// txLookupIn returns txLookup's lookup from the primary at seq of capture, the vector's capture number capture.
func txLookupIn(seed, id string, capture int, seq uint64, view ...int) LookupSpec {
	l := txLookup(seed, id, seq, view...)
	l.Key.CaptureID = captureOf(seed, capture).String()

	return l
}

// txBoundaryRecord returns a capture-boundary record of seq and epoch at ts, of kind kind, with the gap bounds gap when given.
func txBoundaryRecord(seq uint64, epoch uint32, ts int64, kind tracepack.BoundaryKind, gap *[2]int64) (tracepack.Record, error) {
	ev := &tracepack.TransportEvent{Event: tracepack.EventCaptureBoundary, BoundaryKind: new(kind)}
	if gap != nil {
		ev.GapStart, ev.GapEnd = new(gap[0]), new(gap[1])
	}
	r, err := newEvent(seq, epoch, ev)
	r.TSUTCNs = ts

	return r, err
}

// isSocketClose reports whether r is a socket-close event of epoch.
func isSocketClose(r *tracepack.Record, epoch uint32) bool {
	if r.Kind != tracepack.KindTransportEvent || r.Epoch != epoch {
		return false
	}
	ev, err := tracepack.UnmarshalTransportEvent(r.Payload)

	return err == nil && ev.Event == tracepack.EventSocketClose
}

// isBoundary reports whether r is a capture-boundary record of kind and epoch.
func isBoundary(r *tracepack.Record, kind tracepack.BoundaryKind, epoch uint32) bool {
	if r.Kind != tracepack.KindTransportEvent || r.Epoch != epoch {
		return false
	}
	ev, err := tracepack.UnmarshalTransportEvent(r.Payload)

	return err == nil && ev.Event == tracepack.EventCaptureBoundary && ev.BoundaryKind != nil && *ev.BoundaryKind == kind
}

// hourOf returns the UTC hour of ts, in hours since the Unix epoch.
func hourOf(ts int64) int64 {
	h := ts / hourNs
	if ts%hourNs < 0 {
		h--
	}

	return h
}

// plantedBoundary is a boundary entry a footer claims: a stop of epoch at seq, its ts that of the record at seq.
type plantedBoundary struct {
	seq   uint64
	epoch uint32
	ts    int64
}

// plantClaims returns pack, a finalized pack of one block, with footer claims its records do not support,
// in block 0's F-3 summary and in F-5 together, so that the footer stays consistent with itself and is used:
// each epoch of closes gets that close_seq, replacing the one it has or added to its entry, and bound, when set, a stop boundary entry.
// Every other entry is kept as stored.
func plantClaims(pack []byte, closes map[uint32]uint64, bound *plantedBoundary) ([]byte, error) {
	return PatchFooter(pack, func(d []byte) ([]byte, error) {
		parts, err := SplitFooter(d)
		if err != nil {
			return nil, err
		}
		f3, err := F3List(d, 0)
		if err != nil {
			return nil, err
		}
		list, err := claimList(d[f3.Off:f3.Off+f3.Len], f3EpochTag, f3BoundaryTag, closes, bound)
		if err != nil {
			return nil, err
		}
		if err := parts.SetF3List(0, list); err != nil {
			return nil, err
		}
		if parts.F5, err = claimList(parts.F5, f5EpochTag, f5BoundaryTag, closes, bound); err != nil {
			return nil, err
		}

		return parts.Join()
	})
}

// claimList returns the entry list list, an F-3 summary or F-5, with closes and bound planted (plantClaims),
// its epoch and boundary entries of tags epochTag and boundaryTag; every epoch of closes must have an entry.
func claimList(list []byte, epochTag, boundaryTag uint16, closes map[uint32]uint64, bound *plantedBoundary) ([]byte, error) {
	entries, err := tlv.Decode(list)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrPackEdit, err)
	}
	out := make([]byte, 0, len(list))
	planted := 0
	for _, e := range entries {
		stored := list[e.Offset : e.Offset+tlv.HeaderLen+len(e.Value)]
		if e.Tag != epochTag {
			out = append(out, stored...)
			continue
		}
		nested, err := e.Nested()
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrPackEdit, err)
		}
		k := slices.IndexFunc(nested, func(n tlv.Entry) bool { return n.Tag == epochEpochTag })
		if k < 0 {
			return nil, fmt.Errorf("%w: an epoch entry without its epoch", ErrPackEdit)
		}
		epoch, err := nested[k].U64()
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrPackEdit, err)
		}
		v, ok := closes[uint32(epoch)]
		if !ok {
			out = append(out, stored...)
			continue
		}
		nested = slices.DeleteFunc(nested, func(n tlv.Entry) bool { return n.Tag == epochCloseSeqTag })
		out = tlv.AppendEntry(out, tlv.NestedEntry(epochTag, append(nested, tlv.U64Entry(epochCloseSeqTag, v))))
		planted++
	}
	if planted != len(closes) {
		return nil, fmt.Errorf("%w: %d of %d epochs to close have an entry", ErrPackEdit, planted, len(closes))
	}
	if bound != nil {
		out = tlv.AppendEntry(out, tlv.NestedEntry(boundaryTag, []tlv.Entry{
			tlv.U64Entry(boundarySeqTag, bound.seq), tlv.U8Entry(boundaryKindTag, uint8(tracepack.BoundaryKindStop)),
			tlv.I64Entry(boundaryTSTag, bound.ts), tlv.U64Entry(boundaryEpochTag, uint64(bound.epoch)),
		}))
	}

	return out, nil
}

// claimedPack is the expectation of a pack of plantClaims: finalized, one block holding the records of seqs 1 to last,
// its footer valid and so used, its statistics the claims epochs and boundaries,
// and its block disagreeing with its F-3 summary, as verify finds by reading the records.
func claimedPack(last uint64, epochs []EpochWant, boundaries ...BoundaryWant) *Expectation {
	e := withStats(finalizedPack(1, last), epochs, boundaries...)
	e.Outcome, e.Disagreeing = tracepack.OutcomeFinalizedInconsistent, []int{0}

	return e
}

// withStats returns e with the statistics a valid footer gives: epochs, ascending by epoch, and boundaries, ascending by seq.
func withStats(e *Expectation, epochs []EpochWant, boundaries ...BoundaryWant) *Expectation {
	e.Stats = &StatsWant{Available: true, Epochs: epochs, Boundaries: boundaries}

	return e
}

// closedAt returns an epoch of the statistics whose close_seq is seq.
func closedAt(epoch uint32, seq uint64) EpochWant {
	return EpochWant{Epoch: epoch, CloseSeq: new(seq)}
}

// txContradiction returns the contradiction fact at seq.
func txContradiction(seq uint64) FactWant {
	return FactWant{Fact: Fact{Reason: FactContradiction, Seq: new(U64(seq))}}
}
