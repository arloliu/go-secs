package corpus

import (
	"errors"
	"fmt"

	"github.com/arloliu/go-secs/tracepack"
)

// txMatchVectors returns the recipes of the tx group that match replies to primaries:
// across packs and hours, across epochs, and beside records whose fields are unavailable
// (the tracepack semantics specification §7.2 and §9).
func txMatchVectors() []Recipe {
	return []Recipe{
		{
			ID:    "tx-cross-pack",
			Title: "a reply in another segment of the primary's hour, and a reply in the next hour's scope",
			Cites: []string{"SEM §7.2", "SEM §9", "CORPUS §5.11", "CORPUS §8"}, Class: ClassMultiPack,
			Build: crossPackBuild,
			// The scope of hour H holds packs 0 and 1, in the order view lists them, and the scope of H + 1 pack 2;
			// two scopes are scheduled, and the lookup reads both, also after the window is closed
			// (the tracepack semantics specification §7.2, Scopes read).
			// Each primary's reply carries its System Bytes and no other record does, and the socket-close at 5 ends both windows.
			Expect: &Expectation{
				Packs: []*Expectation{packOf(1, 1), packOf(1, 2, 3), packOf(1, 4, 5)},
				Lookups: []LookupWant{
					{
						ID: "next-hour", Outcome: "matched", Key: txPrimaryKey(3, crossPackSystemBytes['y']), WindowEnd: new(uint64(5)),
						Records: []LookupRecord{
							txAt(txKept(3, false, false, RolePrimary), 0, 1),
							txAt(txCandidate(4, true), 1, 2),
							txAt(txKept(5, false, true, RoleClosing), 1, 2),
						},
						Searched: crossPackSearched(),
					},
					{
						ID: "same-hour-other-segment", Outcome: "matched", Key: txPrimaryKey(1, crossPackSystemBytes['x']), WindowEnd: new(uint64(5)),
						Records: []LookupRecord{
							txKept(1, false, false, RolePrimary),
							txAt(txCandidate(2, true), 0, 1),
							txAt(txKept(5, false, true, RoleClosing), 1, 2),
						},
						Searched: crossPackSearched(),
					},
				},
			},
		},
		{
			ID:    "tx-refused-socket",
			Title: "an outstanding primary of epoch 1, the socket-close of a refused socket of epoch 2, then the reply of epoch 1",
			Cites: []string{"FMT I-7", "SEM §7.2", "SEM §9", "CORPUS §5.11"}, Class: ClassMultiPack,
			Build: refusedSocketBuild,
			// The socket-close at 2 ends epoch 2, not the primary's: it is no closing record and plays no role, so it is not kept,
			// and the reply at 3 still matches; the socket-close of epoch 1 at 4 ends the window
			// (the tracepack semantics specification §7.2: a later epoch is not closure evidence).
			Expect: &Expectation{
				Packs: []*Expectation{finalizedPack(1, 4)},
				Lookups: []LookupWant{{
					ID: "e1-reply-after-e2", Outcome: "matched", Key: txPrimaryKey(1, refusedSystemBytes), WindowEnd: new(uint64(4)),
					Records: []LookupRecord{
						txKept(1, false, false, RolePrimary), txCandidate(3, true), txKept(4, false, true, RoleClosing),
					},
					Searched: txSearched(),
				}},
			},
		},
		{
			ID:    "tx-unavailable-fields",
			Title: "a primary without System Bytes, and an unanswered primary followed in its window by a reply-direction record without System Bytes",
			Cites: []string{"FMT §7.2", "SEM §7.2", "SEM §9", "CORPUS §5.11"}, Class: ClassMultiPack,
			Build: unavailableFieldsBuild,
			// The primary at 1 has no association key, so its lookup ends after its scope, incomplete, with its version alone.
			// The record at 4 is no candidate of the primary at 3, its System Bytes unavailable, but it could be the reply:
			// a possible reply in the window keeps the result from unmatched (the tracepack semantics specification §7.2).
			Expect: &Expectation{
				Packs: []*Expectation{finalizedPack(1, 5)},
				Lookups: []LookupWant{
					{
						ID: "no-system-bytes", Outcome: "incomplete", Key: txKeyWithout(1, 1, unavailableSystemBytes['x']),
						Records:  []LookupRecord{txKept(1, false, false, RolePrimary)},
						Gaps:     []FactWant{{Fact: Fact{Reason: FactNoKey}}},
						Searched: txSearched(),
					},
					{
						ID: "unavailable-reply", Outcome: "incomplete", Key: txEpochKey(2, 1, unavailableSystemBytes['y']), WindowEnd: new(uint64(5)),
						Records: []LookupRecord{
							txKept(3, false, false, RolePrimary), txKept(4, true, false, RolePossibleReply), txKept(5, false, true, RoleClosing),
						},
						Gaps:     []FactWant{txUnavailable(4)},
						Searched: txSearched(),
					},
				},
			},
		},
		{
			ID: "tx-possible-primary",
			Title: "a possible same-key primary without System Bytes between two replies, " +
				"and a record in direction unknown that is both a possible reply and a possible same-key primary",
			Cites: []string{"SEM §7.2", "SEM §9", "CORPUS §5.11"}, Class: ClassMultiPack,
			Build: possiblePrimaryBuild,
			// The record at 3 could repeat the primary's key, so only the candidate before it is eligible:
			// 2 matches, and 4, a candidate after it, could be the reply, a fact beside the match.
			// The record at 7, its direction unknown, could be the reply of 6 or repeat its key; it bounds no window,
			// and the socket-close of epoch 2 at 8 does (the tracepack semantics specification §7.2, Definitions).
			Expect: &Expectation{
				Packs: []*Expectation{finalizedPack(1, 8)},
				Lookups: []LookupWant{
					{
						ID: "reply-before-and-after", Outcome: "matched", Key: txPrimaryKey(1, possibleSystemBytes['a']), WindowEnd: new(uint64(5)),
						Records: []LookupRecord{
							txKept(1, false, false, RolePrimary), txCandidate(2, true), txKept(3, true, false, RolePossiblePrimary),
							txFlagged(4, true, false, false), txKept(5, false, true, RoleClosing),
						},
						Gaps:     []FactWant{txUnavailable(4)},
						Searched: txSearched(),
					},
					{
						ID: "unknown-direction", Outcome: "incomplete", Key: txEpochKey(2, 1, possibleSystemBytes['b']), WindowEnd: new(uint64(8)),
						Records: []LookupRecord{
							txKept(6, false, false, RolePrimary), txKept(7, true, false, RolePossibleReply, RolePossiblePrimary),
							txKept(8, false, true, RoleClosing),
						},
						Gaps:     []FactWant{txUnavailable(7)},
						Searched: txSearched(),
					},
				},
			},
		},
	}
}

// crossPackSystemBytes are the System Bytes of the two transactions of tx-cross-pack, by letter.
var crossPackSystemBytes = map[byte]uint32{'x': 0x21, 'y': 0x22}

// crossPackBuild builds tx-cross-pack, three segments of capture A, their quality evaluated, one block each, epoch 1:
// pack 0 in the first minute of hour H, 1 S1F1 x;
// pack 1 in the second minute of H, 2 S1F2 x and 3 S1F3 y;
// pack 2 in the first minute of H + 1, its records in H + 1, 4 S1F4 y and 5 a socket-close.
// It checks the packs' hours, that each reply carries its primary's key in the opposite direction,
// and that the two transactions' System Bytes differ.
func crossPackBuild(seed string) (*Built, error) {
	sb := crossPackSystemBytes
	closed, err := newSocketEvent(5, 1, tracepack.EventSocketClose)
	if err != nil {
		return nil, err
	}
	next := []tracepack.Record{txReply(4, 1, 4, sb['y']), closed}
	for i := range next {
		next[i].TSUTCNs = hourAt(1, int64(next[i].Seq))
	}
	p1, r1, p3 := txPrimary(1, 1, sb['x']), txReply(2, 1, 2, sb['x']), txPrimary(3, 3, sb['y'])
	switch {
	case !sameKey(&p1, &r1) || r1.Dir == p1.Dir:
		return nil, errors.New("corpus: the record at 2 is not a candidate of 1")
	case !sameKey(&p3, &next[0]) || next[0].Dir == p3.Dir:
		return nil, errors.New("corpus: the record at 4 is not a candidate of 3")
	case sb['x'] == sb['y']:
		return nil, errors.New("corpus: the transactions share their System Bytes")
	}
	packs, err := writeMultiPacks(seed, []multiPack{
		{capture: 0, blocks: [][]tracepack.Record{{p1}}, edit: qualityEvaluated},
		{capture: 0, minute: 1, blocks: [][]tracepack.Record{{r1, p3}}, edit: qualityEvaluated},
		{capture: 0, hour: 1, blocks: [][]tracepack.Record{next}, edit: qualityEvaluated},
	})
	if err != nil {
		return nil, err
	}
	for n, want := range []int64{0, 0, 1} {
		h, err := packHeader(packs[n].Bytes)
		if err != nil {
			return nil, err
		}
		if h.Meta.PeriodStart < hourAt(want, 0) || h.Meta.PeriodEnd > hourAt(want+1, 0) {
			return nil, fmt.Errorf("corpus: pack %d's period does not lie in hour H + %d", n, want)
		}
	}

	twoHours := func(id string, seq uint64) LookupSpec {
		l := txLookup(seed, id, seq, 0, 1, 2)
		l.MaxScopes = 2

		return l
	}

	return &Built{Packs: packs, Lookups: []LookupSpec{twoHours("next-hour", 3), twoHours("same-hour-other-segment", 1)}}, nil
}

// crossPackSearched returns the scopes the lookups of tx-cross-pack search: hour H, indexed, its view packs 0 and 1,
// and hour H + 1, indexed, its view pack 2.
func crossPackSearched() []SearchedScope {
	return []SearchedScope{{Hour: I64(txHour), Indexed: true, Packs: []int{0, 1}}, {Hour: I64(txHour + 1), Indexed: true, Packs: []int{2}}}
}

// refusedSystemBytes are the System Bytes of the transaction of tx-refused-socket.
const refusedSystemBytes uint32 = 0x31

// refusedSocketBuild builds tx-refused-socket: pack 0 of capture A in hour H, its quality evaluated, one block:
// 1 S1F1 x of epoch 1, 2 a socket-close of epoch 2, 3 S1F2 x of epoch 1, 4 a socket-close of epoch 1.
// It checks that 3 is a candidate of 1 and that the socket-closes are of epochs 2 and 1.
func refusedSocketBuild(seed string) (*Built, error) {
	x := refusedSystemBytes
	refused, err := newSocketEvent(2, 2, tracepack.EventSocketClose)
	if err != nil {
		return nil, err
	}
	closed, err := newSocketEvent(4, 1, tracepack.EventSocketClose)
	if err != nil {
		return nil, err
	}
	records := []tracepack.Record{txPrimary(1, 1, x), refused, txReply(3, 1, 2, x), closed}
	switch {
	case !sameKey(&records[0], &records[2]) || records[2].Dir == records[0].Dir:
		return nil, errors.New("corpus: the record at 3 is not a candidate of 1")
	case records[1].Epoch != 2 || records[3].Epoch != 1:
		return nil, errors.New("corpus: the socket-closes are not of epochs 2 and 1")
	}
	packs, err := writeMultiPacks(seed, []multiPack{{capture: 0, blocks: [][]tracepack.Record{records}, edit: qualityEvaluated}})
	if err != nil {
		return nil, err
	}

	return &Built{Packs: packs, Lookups: []LookupSpec{txLookup(seed, "e1-reply-after-e2", 1, 0)}}, nil
}

// unavailableSystemBytes are the System Bytes of the transactions of tx-unavailable-fields, by letter;
// the frame of the primary at 1 carries x, but the record stores only its first ten bytes, without them.
var unavailableSystemBytes = map[byte]uint32{'x': 0x41, 'y': 0x42}

// unavailableFieldsBuild builds tx-unavailable-fields: pack 0 of capture A in hour H, its quality evaluated, one block:
// 1 S1F1 x of epoch 1, its System Bytes not stored, 2 a socket-close of epoch 1;
// 3 S1F1 y of epoch 2, 4 S1F2 equipment-to-host of epoch 2, its System Bytes not stored, 5 a socket-close of epoch 2.
// A record without System Bytes holds its frame's first ten bytes, as a raw capture stores a short frame:
// its field_validity bit for System Bytes is clear, the other header fields available,
// and it is not correlation-incomplete (the tracepack format specification §7.2).
// It checks those fields, that 4 carries 3's SessionID in the opposite direction, and the epochs.
func unavailableFieldsBuild(seed string) (*Built, error) {
	sb := unavailableSystemBytes
	closed1, err := newSocketEvent(2, 1, tracepack.EventSocketClose)
	if err != nil {
		return nil, err
	}
	closed2, err := newSocketEvent(5, 2, tracepack.EventSocketClose)
	if err != nil {
		return nil, err
	}
	primary3 := txData(3, 2, tracepack.DirHostToEquipment, 1, true, sb['y'])
	records := []tracepack.Record{
		txShort(txData(1, 1, tracepack.DirHostToEquipment, 1, true, sb['x'])), closed1,
		primary3, txShort(txData(4, 2, tracepack.DirEquipmentToHost, 2, false, sb['y'])), closed2,
	}
	if err := checkWithoutSystemBytes(&records[0], &records[3]); err != nil {
		return nil, err
	}
	h3, h4 := records[2].HSMSHeader(), records[3].HSMSHeader()
	switch {
	case h4.SessionID != h3.SessionID || records[3].Dir == records[2].Dir || records[3].Epoch != records[2].Epoch:
		return nil, errors.New("corpus: the record at 4 is not of 3's session and epoch in the opposite direction")
	case records[0].Epoch != 1 || records[1].Epoch != 1 || records[2].Epoch != 2 || records[4].Epoch != 2:
		return nil, errors.New("corpus: the records are not of epochs 1, 1, 2, 2, 2")
	}
	packs, err := writeMultiPacks(seed, []multiPack{{capture: 0, blocks: [][]tracepack.Record{records}, edit: qualityEvaluated}})
	if err != nil {
		return nil, err
	}

	return &Built{Packs: packs, Lookups: []LookupSpec{
		txLookup(seed, "no-system-bytes", 1, 0),
		txLookup(seed, "unavailable-reply", 3, 0),
	}}, nil
}

// possibleSystemBytes are the System Bytes of the transactions of tx-possible-primary, by letter.
var possibleSystemBytes = map[byte]uint32{'a': 0x51, 'b': 0x52}

// possiblePrimaryBuild builds tx-possible-primary: pack 0 of capture A in hour H, its quality evaluated, one block:
// 1 S1F1 a, 2 S1F2 a, 3 S1F3 host-to-equipment without System Bytes, 4 S1F2 a, 5 a socket-close, of epoch 1;
// 6 S1F1 b, 7 S1F7 b in direction unknown, 8 a socket-close, of epoch 2.
// The record at 3 holds its frame's first ten bytes, as unavailableFieldsBuild's records without System Bytes do.
// It checks that 2 and 4 carry 1's key in the opposite direction, that 3 carries 1's SessionID, direction and epoch,
// and that 7 carries 6's key in direction unknown.
func possiblePrimaryBuild(seed string) (*Built, error) {
	sb := possibleSystemBytes
	closed1, err := newSocketEvent(5, 1, tracepack.EventSocketClose)
	if err != nil {
		return nil, err
	}
	closed2, err := newSocketEvent(8, 2, tracepack.EventSocketClose)
	if err != nil {
		return nil, err
	}
	records := []tracepack.Record{
		txPrimary(1, 1, sb['a']), txReply(2, 1, 2, sb['a']),
		txShort(txData(3, 1, tracepack.DirHostToEquipment, 3, true, sb['a'])), txReply(4, 1, 2, sb['a']), closed1,
		txData(6, 2, tracepack.DirHostToEquipment, 1, true, sb['b']), txData(7, 2, tracepack.DirUnknown, 7, true, sb['b']), closed2,
	}
	p, unknown := &records[0], &records[6]
	for _, r := range []*tracepack.Record{&records[1], &records[3]} {
		if !sameKey(p, r) || r.Dir == p.Dir {
			return nil, fmt.Errorf("corpus: the record at %d is not a candidate of 1", r.Seq)
		}
	}
	if err := checkWithoutSystemBytes(&records[2]); err != nil {
		return nil, err
	}
	if records[2].HSMSHeader().SessionID != p.HSMSHeader().SessionID || records[2].Dir != p.Dir || records[2].Epoch != p.Epoch {
		return nil, errors.New("corpus: the record at 3 is not of 1's session, direction and epoch")
	}
	if !sameKey(&records[5], unknown) || unknown.Dir != tracepack.DirUnknown {
		return nil, errors.New("corpus: the record at 7 does not carry 6's key in direction unknown")
	}
	packs, err := writeMultiPacks(seed, []multiPack{{capture: 0, blocks: [][]tracepack.Record{records}, edit: qualityEvaluated}})
	if err != nil {
		return nil, err
	}

	return &Built{Packs: packs, Lookups: []LookupSpec{
		txLookup(seed, "reply-before-and-after", 1, 0),
		txLookup(seed, "unknown-direction", 6, 0),
	}}, nil
}

// txData returns a data record of seq and epoch at msAt(seq) in direction dir: S1F<function>, W set when wbit,
// SessionID 1 and System Bytes systemBytes, every field available.
func txData(seq uint64, epoch uint32, dir tracepack.Dir, function byte, wbit bool, systemBytes uint32) tracepack.Record {
	return newData(seq, epoch, dir, dataFrame(1, function, wbit, systemBytes, nil))
}

// txShort returns r with only the first ten bytes of its frame, as a raw capture stores a short frame:
// the length field and the header up to SType, without System Bytes;
// its field_validity is that of the bytes held, System Bytes unavailable (the tracepack format specification §7.2).
func txShort(r tracepack.Record) tracepack.Record {
	r.Payload = r.Payload[:10]
	r.SetCapturedFieldValidity()

	return r
}

// checkWithoutSystemBytes checks that each record of rs is a data record whose System Bytes are unavailable
// while its SessionID, stream and function are available, and that it is not correlation-incomplete.
func checkWithoutSystemBytes(rs ...*tracepack.Record) error {
	rest := tracepack.FieldValiditySessionID | tracepack.FieldValidityStreamAndW | tracepack.FieldValidityFunction
	for _, r := range rs {
		h := r.HSMSHeader()
		if r.Kind != tracepack.KindData || h.Available.Has(tracepack.FieldValiditySystemBytes) || h.Available&rest != rest ||
			r.Quality&tracepack.QualityCorrelationIncomplete != 0 {
			return fmt.Errorf("corpus: the record at %d is not a data record without System Bytes, its other fields available", r.Seq)
		}
	}

	return nil
}

// txEpochKey returns txPrimaryKey's fields of a primary of epoch epoch.
func txEpochKey(epoch uint32, function uint8, systemBytes uint32) *PrimaryKey {
	k := txPrimaryKey(function, systemBytes)
	k.Epoch = epoch

	return k
}

// txKeyWithout returns txEpochKey's fields without System Bytes: those of a primary whose System Bytes are unavailable.
func txKeyWithout(epoch uint32, function uint8, systemBytes uint32) *PrimaryKey {
	k := txEpochKey(epoch, function, systemBytes)
	k.SystemBytes = nil

	return k
}

// txAt returns the kept version r read in the scope hour hours after H, from pack pack.
func txAt(r LookupRecord, hour int64, pack int) LookupRecord {
	r.Hour, r.Pack = I64(txHour+hour), pack

	return r
}

// txFlagged returns txCandidate's candidate at seq with the flags given.
func txFlagged(seq uint64, decidable, eligible, valid bool) LookupRecord {
	r := txCandidate(seq, valid)
	r.Decidable, r.Eligible = new(decidable), new(eligible)

	return r
}

// txUnavailable returns the unavailable fact of the version at seq
// kept from block 0 of pack 0 in the scope read of hour H.
func txUnavailable(seq uint64) FactWant {
	return FactWant{Fact: Fact{Reason: FactUnavailable, Hours: []I64{I64(txHour)}, Pack: new(0), Block: new(0), Seq: new(U64(seq))}}
}
