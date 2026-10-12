package marking

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"slices"

	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

const (
	// FramePrefixLen is the byte length of a frame's prefix, the 4-byte length and the 10-byte HSMS message header,
	// which no item or whole-text mask covers.
	FramePrefixLen = 14
	// SIVLen is the byte length of a well-formed siv, the synthetic initialization vector of AES-SIV.
	SIVLen = 16
)

// Annotation payload tags whose values an annotation mask covers (the tracepack format specification §8).
const (
	tagText uint16 = 0x0005
	tagRaw  uint16 = 0x0006
)

// Defects of an entry on its own, found at bootstrap.
var (
	// ErrRangesMalformed reports a masked_ranges value that is not an array of whole (offset, length) pairs:
	// a length not a multiple of 8, an odd element count, or an element above 2^63 - 1.
	ErrRangesMalformed = errors.New("marking: masked_ranges is not an array of whole (offset, length) pairs")
	// ErrRangesEmpty reports a masked_ranges array without a pair.
	ErrRangesEmpty = errors.New("marking: masked_ranges holds no range")
	// ErrRangeLength reports a masked range of length 0.
	ErrRangeLength = errors.New("marking: a masked range has length 0")
	// ErrRangeOrder reports masked ranges that are not ascending, or that overlap or touch.
	ErrRangeOrder = errors.New("marking: masked ranges are not ascending, disjoint and non-adjacent")
	// ErrSIVLength reports a present siv whose length is not 16 bytes.
	ErrSIVLength = errors.New("marking: siv is not 16 bytes")
)

// Defects of the entry list and of the file header, found at bootstrap.
var (
	// ErrEntryOrder reports entries not ordered by seq, then by first masked offset.
	ErrEntryOrder = errors.New("marking: redaction entries are not ordered by seq, then by first masked offset")
	// ErrPresence reports a redaction-present flag that is not set exactly when the pack holds a redaction entry.
	ErrPresence = errors.New("marking: redaction-present is not set exactly when a redaction entry exists")
)

// Defects of the entries naming a record against that record, found when it is read.
var (
	// ErrSeq reports an entry that names another seq than the record checked against it.
	ErrSeq = errors.New("marking: the entry names another record")
	// ErrKindNotMaskable reports a record of a kind that is never masked
	// (a control record, a transport event, any kind but data and annotation)
	// that carries quality.redacted or that an entry names.
	ErrKindNotMaskable = errors.New("marking: a record of this kind is never masked")
	// ErrRedactedBit reports a record carrying quality.redacted that no entry names,
	// or a record that an entry names without carrying it.
	ErrRedactedBit = errors.New("marking: quality.redacted is not set exactly when an entry names the record")
	// ErrRangeOutside reports a masked range that does not lie inside the record payload.
	ErrRangeOutside = errors.New("marking: a masked range lies outside the record payload")
	// ErrRangePrefix reports a masked range that covers part of the 14-byte frame prefix.
	ErrRangePrefix = errors.New("marking: a masked range covers the 14-byte frame prefix")
	// ErrRangeValue reports an annotation's masked range outside the value of its text or raw entry,
	// or any range of an annotation whose entry boundaries cannot be established.
	ErrRangeValue = errors.New("marking: a masked range lies outside the annotation's text or raw value")
	// ErrSIVMissing reports a data record's entry without siv under a policy with encryption.
	ErrSIVMissing = errors.New("marking: a data record's entry carries no siv under a policy with encryption")
	// ErrSIVAnnotation reports an annotation's entry carrying siv under a policy with encryption.
	ErrSIVAnnotation = errors.New("marking: an annotation's entry carries siv under a policy with encryption")
	// ErrEncryptedOverlap reports entries of one record carrying siv whose ranges overlap.
	ErrEncryptedOverlap = errors.New("marking: the ranges of the record's encrypted entries overlap")
)

// Kind is the kind of a record, as far as the checks tell kinds apart.
type Kind uint8

const (
	// KindOther is any kind but data and annotation: a control record, a transport event, a record of unknown kind.
	// A record of such a kind is never masked, so its carrying quality.redacted or being named by an entry is a defect.
	KindOther Kind = iota
	// KindData is a data record: its masks are item or whole-text masks.
	KindData
	// KindAnnotation is an annotation record: its masks cover the value of its text or raw entry.
	KindAnnotation
)

// Range is one (offset, length) pair of an entry's masked ranges, counted from the start of the record payload.
type Range struct {
	Offset uint64
	Length uint64
}

// Entry is a redaction entry as the checks see it.
type Entry struct {
	// Seq is the seq the entry names.
	Seq uint64
	// Ranges are the entry's masked ranges, in stored order; empty when Malformed.
	Ranges []Range
	// Malformed reports a stored masked_ranges value that is not an array of whole pairs.
	Malformed bool
	// HasSIV reports that the entry carries siv, and SIVLen is that siv's length.
	HasSIV bool
	SIVLen int
}

// Record is a record as the checks see it.
type Record struct {
	// Seq is the record's seq.
	Seq uint64
	// Kind is the record's kind.
	Kind Kind
	// Redacted reports that the record carries quality.redacted.
	Redacted bool
	// Payload is the record payload; its length is the record's payload_len.
	Payload []byte
}

// Defect is one failed check of a record or of an entry naming it.
type Defect struct {
	// Entry is the index, into the entries given to CheckRecord, of the entry concerned; -1 for the record itself.
	Entry int
	// Err wraps the sentinel error of the check.
	Err error
}

// span is a masked range of an encrypted entry, with the index of its entry.
type span struct {
	off, end uint64
	entry    int
}

// CheckEntry checks e on its own (the tracepack semantics specification §8):
// a non-empty masked_ranges array of whole pairs, every element at or below 2^63 - 1, every length positive,
// the pairs ascending, none overlapping or touching another, and a present siv of 16 bytes.
//
// Returns:
//   - []error: one error per failed check, the ranges' first, then the siv's; nil when e passes.
func CheckEntry(e *Entry) []error {
	var errs []error
	if err := checkRanges(e); err != nil {
		errs = append(errs, err)
	}
	if e.HasSIV && e.SIVLen != SIVLen {
		errs = append(errs, fmt.Errorf("siv of %d bytes: %w", e.SIVLen, ErrSIVLength))
	}

	return errs
}

// CheckOrder checks that cur, the entry after prev, keeps the order of the entries:
// by seq, then by first masked offset, equal values keeping the order.
// The offsets are compared only when both entries have a range, so a defective ranges array adds no order defect.
//
// Returns:
//   - error: an error wrapping ErrEntryOrder when cur sorts before prev.
func CheckOrder(prev, cur *Entry) error {
	c := cmp.Compare(prev.Seq, cur.Seq)
	if c == 0 && len(prev.Ranges) > 0 && len(cur.Ranges) > 0 {
		c = cmp.Compare(prev.Ranges[0].Offset, cur.Ranges[0].Offset)
	}
	if c > 0 {
		return fmt.Errorf("an entry for seq %d follows one for seq %d: %w", cur.Seq, prev.Seq, ErrEntryOrder)
	}

	return nil
}

// CheckPresence checks that the file header's redaction-present flag, set when flag is true,
// is set exactly when the pack holds at least one of its entries entries.
//
// Returns:
//   - error: an error wrapping ErrPresence when it is not.
func CheckPresence(flag bool, entries int) error {
	if flag != (entries > 0) {
		return fmt.Errorf("redaction-present %t with %d entries: %w", flag, entries, ErrPresence)
	}

	return nil
}

// CheckRecord checks entries, the entries naming rec in stored order, against rec
// (the tracepack semantics specification §8):
// each entry names rec's seq; rec carries quality.redacted exactly when some entry names it;
// rec is a data record or an annotation when it carries quality.redacted or an entry names it,
// since a record of any other kind is never masked;
// each range of a data record lies inside the payload and past the 14-byte frame prefix,
// and each range of an annotation inside the value of a text or raw entry;
// the ranges of the entries carrying siv are pairwise disjoint, adjacent ranges allowed;
// and, when encrypted reports a policy carrying encryption_algorithm,
// every entry of a data record carries siv and no entry of an annotation does.
// The checks of each entry on its own are CheckEntry's.
//
// Returns:
//   - []Defect: one defect per failed check, the record's first, then each entry's in order, then the overlap's;
//     nil when every check passes.
func CheckRecord(rec *Record, entries []Entry, encrypted bool) []Defect {
	var defects []Defect
	if rec.Redacted != (len(entries) > 0) {
		defects = append(defects, Defect{Entry: -1, Err: fmt.Errorf("seq %d, redacted %t, %d entries: %w",
			rec.Seq, rec.Redacted, len(entries), ErrRedactedBit)})
	}
	// A record of a kind that is never masked has no maskable bytes, so its ranges and siv have no rule to meet.
	maskable := rec.Kind == KindData || rec.Kind == KindAnnotation
	if !maskable && (rec.Redacted || len(entries) > 0) {
		defects = append(defects, Defect{Entry: -1, Err: fmt.Errorf("seq %d: %w", rec.Seq, ErrKindNotMaskable)})
	}

	var values [][2]uint64
	valuesErr := false
	if rec.Kind == KindAnnotation && len(entries) > 0 {
		values, valuesErr = annotationValues(rec.Payload)
	}
	for i := range entries {
		e := &entries[i]
		if e.Seq != rec.Seq {
			defects = append(defects, Defect{Entry: i, Err: fmt.Errorf("entry for seq %d, record %d: %w", e.Seq, rec.Seq, ErrSeq)})
		}
		if !maskable {
			continue
		}
		if err := checkBounds(rec, e, values, valuesErr); err != nil {
			defects = append(defects, Defect{Entry: i, Err: err})
		}
		if !encrypted {
			continue
		}
		switch {
		case rec.Kind == KindData && !e.HasSIV:
			defects = append(defects, Defect{Entry: i, Err: fmt.Errorf("seq %d: %w", rec.Seq, ErrSIVMissing)})
		case rec.Kind == KindAnnotation && e.HasSIV:
			defects = append(defects, Defect{Entry: i, Err: fmt.Errorf("seq %d: %w", rec.Seq, ErrSIVAnnotation)})
		default:
			// A data entry with siv and an annotation entry without one are as the policy requires.
		}
	}
	if i, ok := encryptedOverlap(entries); ok {
		defects = append(defects, Defect{Entry: i, Err: fmt.Errorf("seq %d: %w", rec.Seq, ErrEncryptedOverlap)})
	}

	return defects
}

// checkRanges checks e's masked ranges on their own.
func checkRanges(e *Entry) error {
	switch {
	case e.Malformed:
		return ErrRangesMalformed
	case len(e.Ranges) == 0:
		return ErrRangesEmpty
	}
	for i, r := range e.Ranges {
		switch {
		case r.Offset > math.MaxInt64 || r.Length > math.MaxInt64:
			return fmt.Errorf("range %d: %w", i, ErrRangesMalformed)
		case r.Length == 0:
			return fmt.Errorf("range %d: %w", i, ErrRangeLength)
		}
		// Both elements are at or below 2^63 - 1, so the end cannot overflow.
		if i > 0 {
			if prev := e.Ranges[i-1]; r.Offset <= prev.Offset+prev.Length {
				return fmt.Errorf("range %d at %d after one ending at %d: %w", i, r.Offset, prev.Offset+prev.Length, ErrRangeOrder)
			}
		}
	}

	return nil
}

// checkBounds checks that every range of e lies inside rec's payload, a data record or an annotation,
// past the frame prefix for a data record,
// and inside one of values, the [start, end) bounds of the text and raw values, for an annotation;
// valuesErr reports an annotation whose entry boundaries cannot be established, so no range of it lies inside a value.
//
// Returns:
//   - error: an error for the first range that fails.
func checkBounds(rec *Record, e *Entry, values [][2]uint64, valuesErr bool) error {
	n := uint64(len(rec.Payload))
	for i, r := range e.Ranges {
		if r.Offset > n || r.Length > n-r.Offset {
			return fmt.Errorf("range %d (%d, %d) of a %d-byte payload: %w", i, r.Offset, r.Length, n, ErrRangeOutside)
		}
		if rec.Kind != KindAnnotation {
			if r.Offset < FramePrefixLen {
				return fmt.Errorf("range %d at offset %d: %w", i, r.Offset, ErrRangePrefix)
			}

			continue
		}
		inside := !valuesErr && slices.ContainsFunc(values, func(v [2]uint64) bool {
			return r.Offset >= v[0] && r.Offset+r.Length <= v[1]
		})
		if !inside {
			return fmt.Errorf("range %d (%d, %d): %w", i, r.Offset, r.Length, ErrRangeValue)
		}
	}

	return nil
}

// annotationValues returns the [start, end) bounds, in payload, of the value of every top-level text and raw entry.
//
// Returns:
//   - [][2]uint64: the value bounds, in entry order.
//   - bool: true when payload is not a well-formed entry list, so no bound can be established.
func annotationValues(payload []byte) ([][2]uint64, bool) {
	var values [][2]uint64
	for e, err := range tlv.Entries(payload) {
		if err != nil {
			return nil, true
		}
		if e.Tag == tagText || e.Tag == tagRaw {
			start := uint64(e.Offset + tlv.HeaderLen)
			values = append(values, [2]uint64{start, start + uint64(len(e.Value))})
		}
	}

	return values, false
}

// encryptedOverlap reports whether a range of an entry carrying siv overlaps a range of another such entry,
// adjacent ranges allowed, and the index of the later entry of the first overlap found in offset order.
func encryptedOverlap(entries []Entry) (int, bool) {
	var spans []span
	for i := range entries {
		if !entries[i].HasSIV {
			continue
		}
		for _, r := range entries[i].Ranges {
			// A range whose end overflows is an entry defect; it is clamped here so the sweep stays ordered.
			end := r.Offset + r.Length
			if end < r.Offset {
				end = math.MaxUint64
			}
			spans = append(spans, span{off: r.Offset, end: end, entry: i})
		}
	}
	if len(spans) < 2 {
		return 0, false
	}
	slices.SortFunc(spans, func(a, b span) int {
		return cmp.Or(cmp.Compare(a.off, b.off), cmp.Compare(a.entry, b.entry))
	})

	// The sweep keeps the two largest ends seen, each of a different entry,
	// so the largest end of an entry other than the current span's is always one of them.
	var first, second span
	for k, s := range spans {
		other := first
		if first.entry == s.entry {
			other = second
		}
		if k > 0 && s.off < other.end {
			return max(s.entry, other.entry), true
		}
		switch {
		case k == 0 || s.end > first.end && s.entry == first.entry:
			first = s
		case s.end > first.end:
			first, second = s, first
		case s.entry != first.entry && s.end > second.end:
			second = s
		default:
			// s ends within the two largest ends of their entries, which stay the largest.
		}
	}

	return 0, false
}
