package corpus

import (
	"encoding/binary"
	"errors"
	"fmt"
	"slices"

	"github.com/arloliu/go-secs/tracepack"
)

// Cites of every transaction lookup.
var lookupCites = []string{"SEM §7.2", "CORPUS §5.11"}

// txHour is the hour H of the lookups: TimeBase's UTC hour, the scope of the packs of hour offset 0.
const txHour = TimeBase / hourNs

// Fields of the data messages of the lookup vectors: SessionID 1, and the HSMS SType of a Linktest.rsp (SEMI E37 §8.3).
// The control record of tx-candidate-selection carries SessionID 1, not the 0xFFFF of an E37 Linktest.rsp, on purpose:
// it then shares the key of its primary, so only its kind keeps it from being a candidate;
// a SessionID of 0xFFFF would let a lookup tell it apart by the key alone, and the vector would no longer test the kind.
const (
	txSessionID       uint16 = 1
	stypeLinktestRsp  byte   = 6
	txLinktestRspFunc byte   = 10
)

// txVectors returns the recipes of the tx group, transaction lookups over the packs of a vector
// (the tracepack corpus specification §9.4).
func txVectors() []Recipe {
	return append([]Recipe{
		{
			ID:    "tx-repeated-key",
			Title: "a completed transaction, then an unanswered primary of the same key: the same-key primary bounds the first window",
			Cites: []string{"SEM §7.2", "SEM §9", "CORPUS §5.11"}, Class: ClassMultiPack,
			Build: repeatedKeyBuild,
			// The primary at 1 is answered at 2; the primary at 3 repeats its key, so it ends 1's window,
			// and the socket-close at 4 ends 3's (the tracepack semantics specification §7.2, Window).
			// The socket-close is kept as a closing record after 1's window, not bounding it.
			Expect: &Expectation{
				Packs: []*Expectation{finalizedPack(1, 4)},
				Lookups: []LookupWant{
					{
						ID: "completed", Outcome: "matched", Key: txPrimaryKey(1, repeatedSystemBytes), WindowEnd: new(uint64(3)),
						Records: []LookupRecord{
							txKept(1, false, false, RolePrimary), txCandidate(2, true),
							txKept(3, false, true, RoleSameKeyPrimary), txKept(4, false, false, RoleClosing),
						},
						Searched: txSearched(),
					},
					{
						ID: "unanswered", Outcome: "unmatched", Key: txPrimaryKey(1, repeatedSystemBytes), WindowEnd: new(uint64(4)),
						Records:  []LookupRecord{txKept(3, false, false, RolePrimary), txKept(4, false, true, RoleClosing)},
						Searched: txSearched(),
					},
				},
			},
		},
		{
			ID: "tx-candidate-selection",
			Title: "a valid match, a wrong stream, an F0 abort, a wrong-stream F0, a control record that looks like F + 1, two valid matches, " +
				"and a key naming a reply",
			Cites: []string{"FMT §16", "SEM §7.2", "SEM §9", "CORPUS §5.11"}, Class: ClassMultiPack,
			Build: candidateSelectionBuild,
			// Each primary's System Bytes are its own, so each window holds its own replies only,
			// and the socket-close at 14 ends every window.
			// A candidate is valid with the primary's stream and function + 1 or 0; the control record at 10 is no candidate,
			// since a candidate is a data record (the tracepack semantics specification §7.2, Definitions).
			Expect: &Expectation{
				Packs: []*Expectation{finalizedPack(1, 14)},
				Lookups: []LookupWant{
					{
						ID: "control-like-reply", Outcome: "unmatched", Key: txPrimaryKey(9, selectionSystemBytes['e']), WindowEnd: new(uint64(14)),
						Records:  []LookupRecord{txKept(9, false, false, RolePrimary), txKept(14, false, true, RoleClosing)},
						Searched: txSearched(),
					},
					txSelection("f0-abort", 5, 'c', txCandidate(6, true)),
					{ID: "not-primary", Error: ErrorNotPrimary},
					txSelection("two-valid", 11, 'f', txCandidate(12, true), txCandidate(13, true)),
					txSelection("valid", 1, 'a', txCandidate(2, true)),
					txSelection("wrong-stream", 3, 'b', txCandidate(4, false)),
					txSelection("wrong-stream-f0", 7, 'd', txCandidate(8, false)),
				},
			},
		},
	}, slices.Concat(txMatchVectors(), txWindowVectors())...)
}

// repeatedSystemBytes are the System Bytes of every data record of tx-repeated-key.
const repeatedSystemBytes uint32 = 0x78

// repeatedKeyBuild builds tx-repeated-key: pack 0 of capture A in hour H, its quality evaluated, one block, epoch 1:
// 1 a primary S1F1, 2 its reply S1F2, 3 a primary S1F1 of the same key, 4 a socket-close.
// It checks that 3 repeats 1's key and that 2 answers it.
func repeatedKeyBuild(seed string) (*Built, error) {
	x := repeatedSystemBytes
	closed, err := newSocketEvent(4, 1, tracepack.EventSocketClose)
	if err != nil {
		return nil, err
	}
	records := []tracepack.Record{
		txPrimary(1, 1, x), txReply(2, 1, 2, x), txPrimary(3, 1, x), closed,
	}
	switch {
	case !sameKey(&records[0], &records[2]) || records[0].Dir != records[2].Dir:
		return nil, errors.New("corpus: the primary at 3 does not repeat the key of 1")
	case !sameKey(&records[0], &records[1]) || records[1].Dir == records[0].Dir:
		return nil, errors.New("corpus: the record at 2 is not a candidate of 1")
	}
	packs, err := writeMultiPacks(seed, []multiPack{{capture: 0, blocks: [][]tracepack.Record{records}, edit: qualityEvaluated}})
	if err != nil {
		return nil, err
	}

	return &Built{Packs: packs, Lookups: []LookupSpec{
		txLookup(seed, "completed", 1, 0),
		txLookup(seed, "unanswered", 3, 0),
	}}, nil
}

// selectionSystemBytes are the System Bytes of each transaction of tx-candidate-selection, by its letter.
var selectionSystemBytes = map[byte]uint32{'a': 0x0A, 'b': 0x0B, 'c': 0x0C, 'd': 0x0D, 'e': 0x0E, 'f': 0x0F}

// candidateSelectionBuild builds tx-candidate-selection: pack 0 of capture A in hour H, its quality evaluated, one block, epoch 1:
// 1 S1F1 a, 2 S1F2 a; 3 S1F3 b, 4 S2F4 b; 5 S1F5 c, 6 S1F0 c; 7 S1F7 d, 8 S2F0 d;
// 9 S1F9 e, 10 a Linktest.rsp equipment-to-host of SessionID 1 and System Bytes e, its bytes 6 and 7 1 and 10
// (deliberately not the SessionID 0xFFFF of SEMI E37, so that it carries e's key);
// 11 S1F11 f, 12 S1F12 f, 13 S1F12 f; 14 a socket-close.
// Each odd seq up to 11 is a primary host-to-equipment, each reply equipment-to-host.
// It checks that the replies carry their primary's key, that the System Bytes of the transactions differ,
// and that the control record's header fields read as those of an S1F10 of e's key.
func candidateSelectionBuild(seed string) (*Built, error) {
	sb := selectionSystemBytes
	linktest := frameRecord(tracepack.KindControl, 10, msAt(10), 1, tracepack.DirEquipmentToHost,
		hsmsFrame(txSessionID, 1, txLinktestRspFunc, stypeLinktestRsp, sb['e'], nil))
	closed, err := newSocketEvent(14, 1, tracepack.EventSocketClose)
	if err != nil {
		return nil, err
	}
	records := []tracepack.Record{
		txPrimary(1, 1, sb['a']), txReply(2, 1, 2, sb['a']),
		txPrimary(3, 3, sb['b']), txReply(4, 2, 4, sb['b']),
		txPrimary(5, 5, sb['c']), txReply(6, 1, 0, sb['c']),
		txPrimary(7, 7, sb['d']), txReply(8, 2, 0, sb['d']),
		txPrimary(9, 9, sb['e']), linktest,
		txPrimary(11, 11, sb['f']), txReply(12, 1, 12, sb['f']), txReply(13, 1, 12, sb['f']),
		closed,
	}
	for _, pair := range [][2]int{{1, 2}, {3, 4}, {5, 6}, {7, 8}, {9, 10}, {11, 12}, {11, 13}} {
		p, r := &records[pair[0]-1], &records[pair[1]-1]
		if !sameKey(p, r) || p.Dir != tracepack.DirHostToEquipment || r.Dir != tracepack.DirEquipmentToHost {
			return nil, fmt.Errorf("corpus: the record at %d does not carry the key of the primary at %d", pair[1], pair[0])
		}
	}
	if len(sb) != 6 || len(map[uint32]bool{sb['a']: true, sb['b']: true, sb['c']: true, sb['d']: true, sb['e']: true, sb['f']: true}) != 6 {
		return nil, errors.New("corpus: the transactions do not each have their own System Bytes")
	}
	h := linktest.HSMSHeader()
	every := tracepack.FieldValiditySessionID | tracepack.FieldValidityStreamAndW | tracepack.FieldValidityFunction | tracepack.FieldValiditySType
	if h.Available&every != every || h.Stream != 1 || h.Function != txLinktestRspFunc || h.SType != stypeLinktestRsp || h.W {
		return nil, errors.New("corpus: the control record's bytes 6 and 7 do not read as S1F10 of a Linktest.rsp")
	}
	packs, err := writeMultiPacks(seed, []multiPack{{capture: 0, blocks: [][]tracepack.Record{records}, edit: qualityEvaluated}})
	if err != nil {
		return nil, err
	}

	return &Built{Packs: packs, Lookups: []LookupSpec{
		txLookup(seed, "control-like-reply", 9, 0),
		txLookup(seed, "f0-abort", 5, 0),
		txLookup(seed, "not-primary", 2, 0),
		txLookup(seed, "two-valid", 11, 0),
		txLookup(seed, "valid", 1, 0),
		txLookup(seed, "wrong-stream", 3, 0),
		txLookup(seed, "wrong-stream-f0", 7, 0),
	}}, nil
}

// txSelection is the result of a lookup of tx-candidate-selection from the primary at seq of the transaction letter:
// the primary, its candidates candidates, and the socket-close at 14 bounding the window, without a fact;
// matched with one valid candidate, ambiguous with several, unmatched with none.
func txSelection(id string, seq uint64, letter byte, candidates ...LookupRecord) LookupWant {
	valid := 0
	for _, c := range candidates {
		if *c.Valid {
			valid++
		}
	}
	outcome := "unmatched"
	switch {
	case valid == 1:
		outcome = "matched"
	case valid > 1:
		outcome = "ambiguous"
	default:
		// No valid match, and no fact: unmatched.
	}
	records := append([]LookupRecord{txKept(seq, false, false, RolePrimary)}, candidates...)

	return LookupWant{
		ID: id, Outcome: outcome, Key: txPrimaryKey(uint8(seq), selectionSystemBytes[letter]), WindowEnd: new(uint64(14)),
		Records: append(records, txKept(14, false, true, RoleClosing)), Searched: txSearched(),
	}
}

// txPrimary returns a data record of seq, epoch 1, at msAt(seq): host-to-equipment S1F<function> with W,
// SessionID 1 and System Bytes systemBytes, every field available.
func txPrimary(seq uint64, function byte, systemBytes uint32) tracepack.Record {
	return newData(seq, 1, tracepack.DirHostToEquipment, dataFrame(1, function, true, systemBytes, nil))
}

// txReply returns a data record of seq, epoch 1, at msAt(seq): equipment-to-host S<stream>F<function> without W,
// SessionID 1 and System Bytes systemBytes, every field available.
func txReply(seq uint64, stream, function byte, systemBytes uint32) tracepack.Record {
	return newData(seq, 1, tracepack.DirEquipmentToHost, dataFrame(stream, function, false, systemBytes, nil))
}

// qualityEvaluated declares a pack's quality evaluated:
// a clear stored quality bit of its records means the condition was checked absent (the tracepack semantics specification §6).
func qualityEvaluated(m *tracepack.PackMeta) {
	m.QualityEvaluated = true
}

// sameKey reports whether a and b carry the same SessionID and System Bytes, each available on both, in the same epoch.
func sameKey(a, b *tracepack.Record) bool {
	ha, hb := a.HSMSHeader(), b.HSMSHeader()
	both := tracepack.FieldValiditySessionID | tracepack.FieldValiditySystemBytes

	return a.Epoch == b.Epoch && ha.Available&both == both && hb.Available&both == both &&
		ha.SessionID == hb.SessionID && ha.SystemBytes == hb.SystemBytes
}

// txLookup returns the lookup id of the vector of seed from the primary at seq of capture A, the vector's capture 0, in hour H,
// over the view packs view, without evidence, complete, one scope scheduled.
func txLookup(seed, id string, seq uint64, view ...int) LookupSpec {
	return LookupSpec{
		ID: id, Cites: lookupCites, Source: LookupSource{View: view, Evidence: []int{}, Complete: true},
		Key: LookupKey{CaptureID: captureOf(seed, 0).String(), Seq: U64(seq), Hour: I64(txHour)}, MaxScopes: 1,
	}
}

// txPrimaryKey returns the fields of a primary of txPrimary: epoch 1, host-to-equipment, SessionID 1, stream 1 with W,
// its function and System Bytes as given.
func txPrimaryKey(function uint8, systemBytes uint32) *PrimaryKey {
	return &PrimaryKey{
		Epoch: 1, Dir: tracepack.DirHostToEquipment.String(), SessionID: new(txSessionID),
		SystemBytes: binary.BigEndian.AppendUint32(nil, systemBytes), Stream: new(uint8(1)), Function: new(function), W: new(true),
	}
}

// txKept returns a version kept from block 0 of pack 0 in the scope read of hour H, without a conflict,
// in the window or not, bounding it or not, its roles roles.
func txKept(seq uint64, inWindow, bound bool, roles ...string) LookupRecord {
	return LookupRecord{Seq: U64(seq), Hour: I64(txHour), Roles: append([]string{}, roles...), InWindow: inWindow, Bound: bound}
}

// txCandidate returns txKept's candidate at seq, in the window, decidable and eligible, a valid match when valid.
func txCandidate(seq uint64, valid bool) LookupRecord {
	r := txKept(seq, true, false, RoleCandidate)
	r.Decidable, r.Eligible, r.Valid = new(true), new(true), new(valid)

	return r
}

// txSearched returns the scopes a lookup of one scope over pack 0 searches: hour H, indexed, its view pack 0.
func txSearched() []SearchedScope {
	return []SearchedScope{{Hour: I64(txHour), Indexed: true, Packs: []int{0}}}
}
