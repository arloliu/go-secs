package tracepack

import (
	"errors"
	"fmt"
	"slices"

	"github.com/arloliu/go-secs/tracepack/internal/marking"
)

// ErrRedactionDefect is wrapped by the Err of every RedactionDefect:
// a recoverable defect of a pack's redaction entries or of a record they mask
// (the tracepack semantics specification §8).
var ErrRedactionDefect = errors.New("tracepack: redaction defect")

// RedactionDefect is one recoverable defect of the Marking of a pack (the tracepack semantics specification §8):
// a redaction entry that fails a check on its own, entries out of order,
// a redaction-present flag that disagrees with the entries, or a record that its entries do not fit.
// A defect never fails Open or a read, and never makes a read incomplete.
type RedactionDefect struct {
	// Seq is the seq of the record the defect concerns: the seq the entry names for a defect of an entry,
	// the record's seq for a defect found when the record was read; 0 for the redaction-present flag.
	Seq uint64
	// Entry is the index into PackMeta.Redaction of the entry concerned;
	// -1 for a defect of the record itself or of the redaction-present flag.
	Entry int
	// Err describes the defect; it wraps ErrRedactionDefect.
	Err error
}

// RecordRedaction is the Marking of one record that carries quality.redacted or that a redaction entry names
// (the tracepack semantics specification §8).
//
// A record is wholly masked when one of its checks fails, or a check of an entry naming it:
// for a data record its whole message text is unavailable, for an annotation the values of its text and raw entries,
// or its whole payload when its entry boundaries cannot be established;
// a defective entry discards every entry of the record,
// and its stored ranges are never proof that a byte is unmasked.
type RecordRedaction struct {
	// Entries are copies of the pack's redaction entries that name the record, in stored order;
	// empty for a record carrying quality.redacted that no entry names.
	Entries []RedactionEntry
	// WhollyMasked reports that Defects is not empty.
	WhollyMasked bool
	// Defects lists every defect of the record and of the entries naming it:
	// first those found at bootstrap, as PackHeader.RedactionDefects lists them, by entry,
	// then those found when the record was read.
	Defects []RedactionDefect
}

// redactionIndex holds what a Reader's reads need to check the Marking of each record they yield,
// built once from the pack metadata and the file header (the tracepack semantics specification §8).
type redactionIndex struct {
	// encrypted reports a policy carrying encryption_algorithm,
	// under which a data record's entry carries siv and an annotation's entry none.
	encrypted bool
	// meta is the decoded pack metadata the entries are copied from.
	meta *PackMeta
	// entries holds each redaction entry as the checks see it, by index into meta.Redaction.
	entries []marking.Entry
	// bySeq holds the indexes of the entries naming each seq, in stored order.
	bySeq map[uint64][]int
	// entryDefects holds the bootstrap defects of each entry, by index into meta.Redaction; nil when there are none.
	entryDefects [][]RedactionDefect
	// header holds every bootstrap defect, by entry, then the redaction-present flag's.
	header []RedactionDefect
}

// newRedactionIndex returns the index of the redaction entries of meta in a pack whose file header holds flags,
// with every bootstrap check run (the tracepack semantics specification §8):
// each entry on its own, the order of the entries, and the redaction-present flag.
// A defect of an entry is kept with the entry, and an order defect with both entries of the pair,
// so the records they name are wholly masked when read.
func newRedactionIndex(meta *PackMeta, flags uint32) *redactionIndex {
	x := &redactionIndex{
		encrypted: meta.RedactionPolicy != nil && meta.RedactionPolicy.EncryptionAlgorithm != nil,
		meta:      meta,
	}
	if n := len(meta.Redaction); n > 0 {
		x.entries = make([]marking.Entry, n)
		x.entryDefects = make([][]RedactionDefect, n)
		x.bySeq = make(map[uint64][]int)
	}
	for i := range meta.Redaction {
		e := &meta.Redaction[i]
		x.entries[i] = markingEntry(e)
		x.bySeq[e.Seq] = append(x.bySeq[e.Seq], i)
		for _, err := range marking.CheckEntry(&x.entries[i]) {
			x.addEntryDefect(i, err)
		}
		if i > 0 {
			if err := marking.CheckOrder(&x.entries[i-1], &x.entries[i]); err != nil {
				x.addEntryDefect(i-1, err)
				x.addEntryDefect(i, err)
			}
		}
	}
	for _, d := range x.entryDefects {
		x.header = append(x.header, d...)
	}
	if err := marking.CheckPresence(flags&fileHeaderRedactionPresent != 0, len(meta.Redaction)); err != nil {
		x.header = append(x.header, RedactionDefect{Entry: -1, Err: redactionDefectError(err)})
	}

	return x
}

// markingEntry returns e as the checks of internal/marking see it.
func markingEntry(e *RedactionEntry) marking.Entry {
	me := marking.Entry{
		Seq: e.Seq, Malformed: len(e.RawMaskedRanges) > 0,
		HasSIV: e.HasSIV || len(e.SIV) > 0, SIVLen: len(e.SIV),
	}
	if !me.Malformed {
		me.Ranges = make([]marking.Range, len(e.MaskedRanges))
		for k, r := range e.MaskedRanges {
			me.Ranges[k] = marking.Range{Offset: r.Offset, Length: r.Length}
		}
	}

	return me
}

// markingRecord returns rec as the checks of internal/marking see it; its payload must be the stored one.
func markingRecord(rec *Record) marking.Record {
	// Every kind but data and annotation is one the checks find never masked.
	kind := marking.KindOther
	if rec.Kind == KindData {
		kind = marking.KindData
	}
	if rec.Kind == KindAnnotation {
		kind = marking.KindAnnotation
	}

	return marking.Record{Seq: rec.Seq, Kind: kind, Redacted: rec.Quality.Has(QualityRedacted), Payload: rec.Payload}
}

// redactionDefectError returns err, a failed Marking check, as the Err of a RedactionDefect.
func redactionDefectError(err error) error {
	return fmt.Errorf("%w: %w", ErrRedactionDefect, err)
}

// cloneRedactionEntry returns a copy of e that shares no memory with it.
func cloneRedactionEntry(e *RedactionEntry) RedactionEntry {
	c := *e
	c.ItemPath = clonePtr(e.ItemPath)
	c.MaskedRanges = slices.Clone(e.MaskedRanges)
	c.RawMaskedRanges = slices.Clone(e.RawMaskedRanges)
	c.Digest = slices.Clone(e.Digest)
	c.SIV = slices.Clone(e.SIV)

	return c
}

// appendMarking is what a Writer of a pack masked at write checks the Marking of its records against
// (the tracepack semantics specification §8): the index of the entries it wrote,
// and which of the seqs they name an appended record carried.
// Its entries are copies, sharing no memory with the caller's pack metadata;
// the policy is read once, when it is built.
type appendMarking struct {
	x *redactionIndex
	// seqs holds the distinct seqs the entries name, ascending, and carried[i] whether an appended record carried seqs[i].
	seqs    []uint64
	carried []bool
	// scratch holds the entries naming the record check is checking, reused across records.
	scratch []marking.Entry
}

// newAppendMarking returns the Marking of a pack masked at write, under policy, holding entries,
// with the file header's redaction-present flag set when redactionPresent is.
//
// Returns:
//   - *appendMarking: the Marking, of copies of entries; nil on error.
//   - error: a *FieldError wrapping ErrMetadataCommitment and ErrRedactionDefect for the first bootstrap defect:
//     an entry failing a check on its own or out of order ("redaction"),
//     or a redaction-present flag set other than exactly when an entry exists ("redaction-present").
func newAppendMarking(policy *RedactionPolicy, entries []RedactionEntry, redactionPresent bool) (*appendMarking, error) {
	m := &PackMeta{RedactionPolicy: policy, Redaction: make([]RedactionEntry, len(entries))}
	for i := range entries {
		m.Redaction[i] = cloneRedactionEntry(&entries[i])
	}
	var flags uint32
	if redactionPresent {
		flags = fileHeaderRedactionPresent
	}

	x := newRedactionIndex(m, flags)
	if len(x.header) > 0 {
		d := x.header[0]
		field := "redaction"
		if d.Entry < 0 {
			field = "redaction-present"
		}

		return nil, &FieldError{Field: field, Err: fmt.Errorf("%w: %w", ErrMetadataCommitment, d.Err)}
	}

	am := &appendMarking{x: x}
	for _, e := range m.Redaction {
		// The entries passed the order check, so equal seqs are adjacent.
		if n := len(am.seqs); n == 0 || am.seqs[n-1] != e.Seq {
			am.seqs = append(am.seqs, e.Seq)
		}
	}
	am.carried = make([]bool, len(am.seqs))

	return am, nil
}

// check checks rec, a record about to be appended under its seq, against the entries naming it
// (marking.CheckRecord): the checks a reader runs on it, both ways.
//
// Returns:
//   - error: a *FieldError naming "redaction", wrapping ErrMetadataCommitment and ErrRedactionDefect,
//     for the first failed check.
func (am *appendMarking) check(rec *Record) error {
	idx := am.x.bySeq[rec.Seq]
	if len(idx) == 0 && !rec.Quality.Has(QualityRedacted) {
		return nil
	}

	am.scratch = am.scratch[:0]
	for _, i := range idx {
		am.scratch = append(am.scratch, am.x.entries[i])
	}
	mrec := markingRecord(rec)
	defects := marking.CheckRecord(&mrec, am.scratch, am.x.encrypted)
	if len(defects) == 0 {
		return nil
	}

	return &FieldError{Field: "redaction", Err: fmt.Errorf("%w: seq %d: %w", ErrMetadataCommitment, rec.Seq, redactionDefectError(defects[0].Err))}
}

// carry records that an appended record carried seq.
func (am *appendMarking) carry(seq uint64) {
	if i, found := slices.BinarySearch(am.seqs, seq); found {
		am.carried[i] = true
	}
}

// carryRange records that the records of a block copied whole carried every seq from first to last that an entry names,
// first to last being one run of consecutive seqs the block holds:
// the Writer does not see the records of such a block, whose caller checks their Marking.
func (am *appendMarking) carryRange(first, last uint64) {
	i, _ := slices.BinarySearch(am.seqs, first)
	for ; i < len(am.seqs) && am.seqs[i] <= last; i++ {
		am.carried[i] = true
	}
}

// checkCarried checks that an appended record carried every seq an entry names.
//
// Returns:
//   - error: a *FieldError naming "redaction", wrapping ErrMetadataCommitment and ErrRedactionDefect,
//     for the lowest seq none carried.
func (am *appendMarking) checkCarried() error {
	i := slices.Index(am.carried, false)
	if i < 0 {
		return nil
	}

	err := fmt.Errorf("an entry names seq %d, which no appended record carries: %w", am.seqs[i], marking.ErrRedactedBit)

	return &FieldError{Field: "redaction", Err: fmt.Errorf("%w: %w", ErrMetadataCommitment, redactionDefectError(err))}
}

// addEntryDefect keeps err, a failed bootstrap check, with entry i.
func (x *redactionIndex) addEntryDefect(i int, err error) {
	x.entryDefects[i] = append(x.entryDefects[i], RedactionDefect{
		Seq: x.meta.Redaction[i].Seq, Entry: i,
		Err: redactionDefectError(fmt.Errorf("entry %d: %w", i, err)),
	})
}

// headerDefects returns a copy of the bootstrap defects; nil when there are none.
func (x *redactionIndex) headerDefects() []RedactionDefect {
	return slices.Clone(x.header)
}

// record returns the Marking of rec, a record of the pack as stored, payload included:
// nil when rec carries no quality.redacted and no entry names it,
// else the copies of its entries, its bootstrap and read defects, and whether it is wholly masked.
func (x *redactionIndex) record(rec *Record) *RecordRedaction {
	idx := x.bySeq[rec.Seq]
	if len(idx) == 0 && !rec.Quality.Has(QualityRedacted) {
		return nil
	}

	rr := &RecordRedaction{}
	entries := make([]marking.Entry, len(idx))
	if len(idx) > 0 {
		rr.Entries = make([]RedactionEntry, len(idx))
	}
	for k, i := range idx {
		rr.Entries[k] = cloneRedactionEntry(&x.meta.Redaction[i])
		entries[k] = x.entries[i]
		rr.Defects = append(rr.Defects, x.entryDefects[i]...)
	}
	mrec := markingRecord(rec)
	for _, d := range marking.CheckRecord(&mrec, entries, x.encrypted) {
		entry := -1
		if d.Entry >= 0 {
			entry = idx[d.Entry]
		}
		rr.Defects = append(rr.Defects, RedactionDefect{Seq: rec.Seq, Entry: entry, Err: redactionDefectError(d.Err)})
	}
	rr.WhollyMasked = len(rr.Defects) > 0

	return rr
}
