package corpus

import (
	"bytes"
	"errors"
	"fmt"
	"slices"

	"github.com/arloliu/go-secs/tracepack"
)

// Cites of every read over several packs.
var readCites = []string{"SEM §7.4", "CORPUS §5.10"}

// multiVectors returns the recipes of the multi group, reads over several packs (the tracepack corpus specification §9.4).
func multiVectors() []Recipe {
	return append([]Recipe{
		{
			ID:    "multi-capture-interleave",
			Title: "two captures whose blocks interleave by ts_min in capture order, and a filter that rejects one capture's next record",
			Cites: []string{"SEM §7.4", "SEM §9", "CORPUS §5.10"}, Class: ClassMultiPack,
			Build: captureInterleaveBuild,
			// Capture order takes the capture whose next selected record has the representative block with the smaller ts_min.
			// Every record yielded: A's block at 0, B's at 10 and 30, then A's at 40 and 20, in seq order.
			// Data only: A's block at 40 holds annotations alone, so A's next selected record lies in its block at 20,
			// which comes before B's block at 30 (the tracepack semantics specification §7.4, Order of a read over several packs).
			Expect: &Expectation{
				Packs: []*Expectation{finalizedPack(3, 6), finalizedPack(2, 4)},
				Reads: []ReadWant{
					{ID: "capture-order", Items: slices.Concat(
						blockItems(0, 0, 1, 2), blockItems(1, 0, 1, 2), blockItems(1, 1, 3, 4), blockItems(0, 1, 3, 4), blockItems(0, 2, 5, 6))},
					{ID: "filter-rejects-next", Items: slices.Concat(
						blockItems(0, 0, 1, 2), blockItems(1, 0, 1, 2), blockItems(0, 2, 5, 6), blockItems(1, 1, 3, 4))},
				},
			},
		},
		{
			ID: "multi-identical-copy", Title: "one block held identically by two packs, read in both orders: yielded once, no conflict",
			Cites: []string{"SEM §7.4", "SEM §9", "CORPUS §5.10"}, Class: ClassMultiPack,
			Build: identicalCopyBuild,
			// Identical copies are one record, yielded once from its representative:
			// the block of the pack given first, both blocks starting at seq 1.
			Expect: &Expectation{
				Packs: []*Expectation{finalizedPack(1, 3), finalizedPack(1, 3)},
				Reads: []ReadWant{
					{ID: "given-order", Items: blockItems(0, 0, 1, 2, 3)},
					{ID: "reversed", Items: blockItems(1, 0, 1, 2, 3)},
				},
			},
		},
		{
			ID: "multi-payload-only-conflict", Title: "copies differing in one message-text byte, read with and without payloads: a conflict",
			Cites: []string{"FMT §16", "SEM §7.4", "SEM §9", "CORPUS §5.10"}, Class: ClassMultiPack,
			Build: payloadOnlyConflictBuild,
			// The copies of seq 2 differ in their payloads alone, which the read compares whether or not it returns them:
			// two versions, each yielded from its representative, the block of the pack holding it, in the order the packs are given.
			Expect: &Expectation{
				Packs: []*Expectation{finalizedPack(1, 3), finalizedPack(1, 3)},
				Reads: []ReadWant{
					payloadConflictRead("with-payloads"),
					payloadConflictRead("without-payloads"),
				},
			},
		},
	}, slices.Concat(multiOrderVectors(), multiSurgeryVectors())...)
}

// captureInterleaveBuild builds multi-capture-interleave:
// pack 0 of capture A, its blocks in seq order at 0 ms (data), 40 ms (annotations) and 20 ms (data),
// and pack 1 of capture B, its blocks at 10 ms and 30 ms (data), every record 1 ms after the one before it in its block.
func captureInterleaveBuild(seed string) (*Built, error) {
	n3, err := multiNote(3, msAt(40), "a")
	if err != nil {
		return nil, err
	}
	n4, err := multiNote(4, msAt(41), "b")
	if err != nil {
		return nil, err
	}
	packs, err := writeMultiPacks(seed, []multiPack{
		{capture: 0, blocks: [][]tracepack.Record{
			{multiData(1, msAt(0), nil), multiData(2, msAt(1), nil)},
			{n3, n4},
			{multiData(5, msAt(20), nil), multiData(6, msAt(21), nil)},
		}},
		{capture: 1, blocks: [][]tracepack.Record{
			{multiData(1, msAt(10), nil), multiData(2, msAt(11), nil)},
			{multiData(3, msAt(30), nil), multiData(4, msAt(31), nil)},
		}},
	})
	if err != nil {
		return nil, err
	}

	return &Built{Packs: packs, Reads: []ReadSpec{
		{ID: "capture-order", Cites: readCites, Packs: []int{0, 1}, Order: ReadOrderCapture},
		{
			ID: "filter-rejects-next", Cites: readCites, Packs: []int{0, 1}, Order: ReadOrderCapture,
			Filter: Filter{Kinds: []string{tracepack.KindData.String()}},
		},
	}}, nil
}

// identicalCopyBuild builds multi-identical-copy:
// two segments of capture A, in the first and second minutes of the hour,
// each holding one block of the data records of seqs 1 to 3, byte for byte the same block.
func identicalCopyBuild(seed string) (*Built, error) {
	block := []tracepack.Record{multiData(1, msAt(1), nil), multiData(2, msAt(2), nil), multiData(3, msAt(3), nil)}
	packs, err := writeMultiPacks(seed, []multiPack{
		{capture: 0, blocks: [][]tracepack.Record{block}},
		{capture: 0, minute: 1, blocks: [][]tracepack.Record{block}},
	})
	if err != nil {
		return nil, err
	}
	a, b, err := firstBlocks(packs)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(a, b) {
		return nil, errors.New("corpus: the two packs' blocks are not the same bytes")
	}

	return &Built{Packs: packs, Reads: []ReadSpec{
		{ID: "given-order", Cites: readCites, Packs: []int{0, 1}, Order: ReadOrderCapture},
		{ID: "reversed", Cites: readCites, Packs: []int{1, 0}, Order: ReadOrderCapture},
	}}, nil
}

// payloadOnlyConflictBuild builds multi-payload-only-conflict:
// two segments of capture A, in the first and second minutes of the hour,
// each holding one block of the data records of seqs 1 to 3 whose message text is an ASCII item of one character,
// seq 2's character a in pack 0 and b in pack 1.
// It checks that the blocks' record headers are the same bytes and their payloads differ in one byte.
func payloadOnlyConflictBuild(seed string) (*Built, error) {
	block := func(c byte) []tracepack.Record {
		return []tracepack.Record{
			multiData(1, msAt(1), asciiItem('x')), multiData(2, msAt(2), asciiItem(c)), multiData(3, msAt(3), asciiItem('y')),
		}
	}
	packs, err := writeMultiPacks(seed, []multiPack{
		{capture: 0, blocks: [][]tracepack.Record{block('a')}},
		{capture: 0, minute: 1, blocks: [][]tracepack.Record{block('b')}},
	})
	if err != nil {
		return nil, err
	}
	a, b, err := firstBlocks(packs)
	if err != nil {
		return nil, err
	}
	da, hs, err := decodeBlock(a)
	if err != nil {
		return nil, err
	}
	db, _, err := decodeBlock(b)
	if err != nil {
		return nil, err
	}
	if len(da) != len(db) || !bytes.Equal(da[:hs], db[:hs]) || differingBytes(da[hs:], db[hs:]) != 1 {
		return nil, errors.New("corpus: the copies do not differ in one payload byte alone")
	}

	return &Built{Packs: packs, Reads: []ReadSpec{
		{ID: "with-payloads", Cites: readCites, Packs: []int{0, 1}, Order: ReadOrderCapture, Payloads: true},
		{ID: "without-payloads", Cites: readCites, Packs: []int{0, 1}, Order: ReadOrderCapture},
	}}, nil
}

// payloadConflictRead is the result of a read of multi-payload-only-conflict, with or without payloads:
// seq 2 twice, once from each pack, each marked as a conflict, and the conflict of its two versions.
func payloadConflictRead(id string) ReadWant {
	return ReadWant{
		ID: id,
		Items: []ItemWant{
			{Seq: 1, Pack: 0, Block: 0}, {Seq: 2, Pack: 0, Block: 0, Conflict: true}, {Seq: 2, Pack: 1, Block: 0, Conflict: true},
			{Seq: 3, Pack: 0, Block: 0},
		},
		Conflicts: []ConflictWant{{Capture: 0, Seq: 2, Versions: [][]int{{0}, {1}}}},
	}
}

// multiData returns a data record of seq, epoch 1, at ts:
// host-to-equipment S1F1 with W, SessionID 1, its System Bytes seq, holding text, every field available.
func multiData(seq uint64, ts int64, text []byte) tracepack.Record {
	return frameRecord(tracepack.KindData, seq, ts, 1, tracepack.DirHostToEquipment, dataFrame(1, 1, true, uint32(seq), text))
}

// multiNote returns an annotation record of seq, epoch 1, at ts holding the note text.
func multiNote(seq uint64, ts int64, text string) (tracepack.Record, error) {
	r, err := newAnnotation(seq, 1, &tracepack.Annotation{AnnotationKind: tracepack.AnnotationKindNote, Text: &text})
	r.TSUTCNs = ts

	return r, err
}

// asciiItem returns the SECS-II ASCII item of the one character c.
func asciiItem(c byte) []byte {
	return []byte{0x41, 0x01, c}
}

// finalizedPack is the expectation of a finalized-consistent pack of blocks blocks holding the records of seqs 1 to last.
func finalizedPack(blocks int, last uint64) *Expectation {
	return &Expectation{
		Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true, Blocks: blocks, Seqs: seqRange(1, last), PrefixEnd: AtEnd(),
	}
}

// blockItems returns the items of seqs, yielded from block block of pack pack, none a conflict.
func blockItems(pack, block int, seqs ...uint64) []ItemWant {
	out := make([]ItemWant, 0, len(seqs))
	for _, s := range seqs {
		out = append(out, ItemWant{Seq: s, Pack: pack, Block: block})
	}

	return out
}

// firstBlocks returns the first block of each of the two packs.
func firstBlocks(packs []PackBuilt) ([]byte, []byte, error) {
	if len(packs) != 2 {
		return nil, nil, fmt.Errorf("corpus: %d packs, not two", len(packs))
	}
	var blocks [2][]byte
	for i := range 2 {
		l, err := Locate(packs[i].Bytes)
		if err != nil {
			return nil, nil, err
		}
		if len(l.Blocks) == 0 {
			return nil, nil, fmt.Errorf("corpus: pack %d has no block", i)
		}
		s := l.Blocks[0]
		blocks[i] = packs[i].Bytes[s.Offset : s.Offset+s.Len]
	}

	return blocks[0], blocks[1], nil
}

// differingBytes returns the number of positions at which a and b, of one length, differ.
func differingBytes(a, b []byte) int {
	n := 0
	for i := range a {
		if a[i] != b[i] {
			n++
		}
	}

	return n
}
