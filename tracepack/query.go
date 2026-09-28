package tracepack

import (
	"cmp"
	"errors"
	"fmt"
	"slices"

	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// Query modes, the modes of the tracepack semantics specification §7.4.
const (
	// QueryProvisional evaluates the copy-field predicates of a Filter on the stored record-header copies,
	// which are unvalidated in a block that is not attested (the tracepack format specification §6).
	// A result that relied on the copies of such a block lists the block in Result.HeaderValidated,
	// also when the block yields no record.
	QueryProvisional QueryMode = iota
	// QueryAuthoritative evaluates the copy-field predicates of a Filter on the values in the payload (I-10),
	// or on the stored copies of an attested block, which carry the guarantees of a full read.
	// A block that is not attested is read in full whenever the Filter has a copy-field predicate,
	// so it is never excluded by its stored copies.
	QueryAuthoritative
)

// Read levels of a block, the levels of the tracepack format specification §6.
const (
	// ReadHeaderOnly is a header-only read of a block that is not attested:
	// the record headers passed the checks of §6, but their copy fields are not validated against the payloads (I-10),
	// and no payload was decoded.
	ReadHeaderOnly ReadLevel = iota + 1
	// ReadAttested is a header-only read of an attested block,
	// which carries the guarantees of a full read because its bytes are CRC-identical to bytes that passed full validation.
	ReadAttested
	// ReadFull is a full read: the whole body was decoded, I-2 holds, and every record carries its payload.
	ReadFull
)

// ErrInvalidQuery reports a Query that Reader.Iterate cannot run: an unknown Mode, or a TimeFrom after TimeTo.
var ErrInvalidQuery = errors.New("tracepack: invalid query")

// QueryMode selects how a query evaluates the copy-field predicates of its Filter
// (the tracepack semantics specification §7.4).
//
// String prints an undefined value as "unknown(<n>)".
type QueryMode uint8

var _ fmt.Stringer = QueryMode(0)

// Query selects records of a pack for Reader.Iterate.
type Query struct {
	// Filter selects the records; its zero value selects every record.
	Filter Filter
	// Mode selects how the copy-field predicates of Filter are evaluated.
	Mode QueryMode
	// Payloads makes Iterate read in full every block it reads, so every Item carries its payload.
	Payloads bool
}

// SF is the stream and function of an HSMS message header.
type SF struct {
	Stream   uint8
	Function uint8
}

// Filter holds the predicates a record must satisfy to be selected: every predicate that is set.
//
// A nil or empty slice places no constraint.
// The time range, Kinds, Dirs and Epochs test fields that are not copies,
// so every mode evaluates them on the stored record header.
// SF, SessionIDs and SystemBytes test copy fields (the tracepack format specification §7.2),
// which the Query's mode decides how to evaluate.
// A copy field is available when its field_validity bit is set:
// the bit stored in the record header, or, when the mode evaluates the payload's values, the bit the payload gives,
// which is clear for every copy field of a record that is neither data nor control.
// A predicate whose field is unavailable cannot match, unless IncludeUnavailable is set.
type Filter struct {
	// TimeFrom and TimeTo bound ts_utc_ns to the half-open range [TimeFrom, TimeTo); a nil bound is unbounded.
	// TimeFrom equal to TimeTo is an empty range, which selects no record.
	TimeFrom, TimeTo *int64
	// Kinds selects records of one of the kinds.
	Kinds []Kind
	// Dirs selects records of one of the directions.
	Dirs []Dir
	// Epochs selects records of one of the epochs.
	Epochs []uint32
	// SF selects records whose stream and function equal one of the pairs; it needs both fields available.
	SF []SF
	// SessionIDs selects records whose session_id is one of the values.
	SessionIDs []uint16
	// SystemBytes selects records whose system_bytes equal it.
	SystemBytes *[4]byte
	// IncludeUnavailable makes a record whose field needed by a copy-field predicate is unavailable satisfy that predicate,
	// the explicit request for records with unavailable fields of the tracepack format specification §7.2.
	IncludeUnavailable bool
}

// ReadLevel is the level at which the block of a yielded record was read (the tracepack format specification §6).
//
// The zero value is not a level; String prints it and any undefined value as "unknown(<n>)".
type ReadLevel uint8

var _ fmt.Stringer = ReadLevel(0)

// Item is one record yielded by Reader.Iterate.
//
// An Item is valid only during the callback that receives it:
// Iterate reuses the Item, and Record.Payload and HeaderExtra alias Iterate's buffers.
// A caller that keeps a record copies it, cloning Payload and HeaderExtra.
type Item struct {
	// Record is the record as stored, every copy field as its record header holds it, whatever the mode;
	// SetHeaderCopies on a copy of a full read gives the payload's values, which an authoritative query selects on.
	// Payload is set iff Level is ReadFull.
	Record Record
	// HeaderExtra holds the record-header bytes behind offset 56, which a newer minor version may define
	// (the tracepack format specification §7.1); nil for a 56-byte record header.
	HeaderExtra []byte
	// Block is the index into Reader.Blocks of the record's block.
	Block int
	// Level is the level the record's block was read at.
	Level ReadLevel
	// CopyMismatch reports, at ReadFull, that the stored copy fields, W or field_validity of a data or control record
	// differ from the values Record.SetHeaderCopies computes from its payload (I-10);
	// it is always false at the other levels, which do not decode the payload.
	CopyMismatch bool
}

// overlaps reports whether the inclusive interval [lo, hi] meets the half-open range [from, to),
// a nil bound of either being unbounded.
// An empty range, from equal to to, meets nothing.
func overlaps(lo, hi, from, to *int64) bool {
	if from != nil && to != nil && *from >= *to {
		return false
	}

	return (hi == nil || from == nil || *hi >= *from) && (lo == nil || to == nil || *lo < *to)
}

// inverted reports whether both bounds are present and the first exceeds the last.
func inverted[T cmp.Ordered](first, last *T) bool {
	return first != nil && last != nil && *first > *last
}

// oneOf reports whether values is empty or holds v.
func oneOf[T comparable](values []T, v T) bool {
	return len(values) == 0 || slices.Contains(values, v)
}

// String returns the mode's name, or "unknown(<n>)" for a value this package does not define.
func (m QueryMode) String() string {
	switch m {
	case QueryProvisional:
		return "provisional"
	case QueryAuthoritative:
		return "authoritative"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(m))
	}
}

// String returns the level's name, or "unknown(<n>)" for a value this package does not define.
func (l ReadLevel) String() string {
	switch l {
	case ReadHeaderOnly:
		return "header-only"
	case ReadAttested:
		return "attested"
	case ReadFull:
		return "full"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(l))
	}
}

// validate checks q before Iterate reads anything: a defined mode, and a time range whose start is not after its end.
func (q *Query) validate() error {
	if q.Mode != QueryProvisional && q.Mode != QueryAuthoritative {
		return fmt.Errorf("%w: mode %v", ErrInvalidQuery, q.Mode)
	}

	f := &q.Filter
	if inverted(f.TimeFrom, f.TimeTo) {
		return fmt.Errorf("%w: TimeFrom %d is after TimeTo %d", ErrInvalidQuery, *f.TimeFrom, *f.TimeTo)
	}

	return nil
}

// hasCopyPredicate reports whether f tests a copy field.
func (f *Filter) hasCopyPredicate() bool {
	return len(f.SF) > 0 || len(f.SessionIDs) > 0 || f.SystemBytes != nil
}

// matchHeader reports whether the stored record header h satisfies the predicates of f on fields that are not copies:
// the time range, Kinds, Dirs and Epochs.
func (f *Filter) matchHeader(h *format.RecordHeader) bool {
	if f.TimeFrom != nil && h.TSUTCNs < *f.TimeFrom {
		return false
	}
	if f.TimeTo != nil && h.TSUTCNs >= *f.TimeTo {
		return false
	}

	return oneOf(f.Kinds, Kind(h.Kind)) && oneOf(f.Dirs, Dir(h.Dir)) && oneOf(f.Epochs, h.Epoch)
}

// matchCopies reports whether the copy fields c satisfy the copy-field predicates of f,
// each field available iff its bit is set in c.validity.
func (f *Filter) matchCopies(c *recordCopies) bool {
	if len(f.SF) > 0 &&
		!f.satisfied(c.validity, FieldValidityStreamAndW|FieldValidityFunction, slices.Contains(f.SF, SF{Stream: c.stream, Function: c.function})) {
		return false
	}
	if len(f.SessionIDs) > 0 && !f.satisfied(c.validity, FieldValiditySessionID, slices.Contains(f.SessionIDs, c.sessionID)) {
		return false
	}
	if f.SystemBytes != nil && !f.satisfied(c.validity, FieldValiditySystemBytes, *f.SystemBytes == c.systemBytes) {
		return false
	}

	return true
}

// satisfied returns the outcome of a copy-field predicate whose fields need the bits need of validity:
// match when every one is set, else IncludeUnavailable, since an unavailable field cannot match (§7.2).
func (f *Filter) satisfied(validity, need FieldValidity, match bool) bool {
	if validity&need != need {
		return f.IncludeUnavailable
	}

	return match
}
