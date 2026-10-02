package tracepack

import (
	"cmp"
	"errors"
	"fmt"
	"slices"

	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// ErrInvalidQuery reports a Query that Reader.Iterate cannot run: a TimeFrom after TimeTo.
var ErrInvalidQuery = errors.New("tracepack: invalid query")

// Query selects records of a pack for Reader.Iterate.
type Query struct {
	// Filter selects the records; its zero value selects every record.
	Filter Filter
	// Payloads makes every Item carry its record's payload; otherwise Item.Record.Payload is nil.
	// It decides only what an Item carries: every block is read in full either way (the tracepack format specification §6),
	// and the Filter is evaluated before the payload is dropped, so the records selected do not depend on it.
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
// The time range, Kinds, Dirs and Epochs test the record header.
// SF, SessionIDs and SystemBytes test HSMS header fields, whose values are read from the payload
// (the tracepack format specification §7.2).
// A field is available when the field_validity bit stored in its record header is set and the payload holds all of its bytes;
// a clear bit leaves it unavailable even where the payload holds the field's bytes,
// and a record that is neither data nor control has no available field.
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
	// IncludeUnavailable makes a record whose field needed by an HSMS header predicate is unavailable satisfy that predicate,
	// the explicit request for records with unavailable fields of the tracepack format specification §7.2.
	IncludeUnavailable bool
}

// Item is one record yielded by Reader.Iterate.
//
// An Item is valid only during the callback that receives it:
// Iterate reuses the Item, and Record.Payload and HeaderExtra alias Iterate's buffers.
// A caller that keeps a record copies it, cloning Payload and HeaderExtra.
type Item struct {
	// Record is the record as stored.
	// Payload is set iff Query.Payloads; without it Record.HSMSHeader reports no field available.
	Record Record
	// HeaderExtra holds the record header's extension area, the bytes behind offset 44, which a newer minor version may define
	// (the tracepack format specification §7.1); nil for a 44-byte record header.
	HeaderExtra []byte
	// Pack is the index into the readers given to MergeIterate of the record's pack; 0 for Reader.Iterate.
	Pack int
	// Block is the index into the pack's Reader.Blocks of the record's block.
	//
	// For a record, or a version of a conflicting record, held by several packs,
	// Pack and Block name the representative of that version:
	// the first block holding it in cluster order
	// (ascending first seq, then the order the readers were given, then file order),
	// as the tracepack semantics specification §7.4 defines it.
	Block int
	// Conflict reports that another version of the record exists among the copies the read compared;
	// Reader.Iterate never sets it.
	Conflict bool
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

// validate checks q before Iterate reads anything: a time range whose start is not after its end.
func (q *Query) validate() error {
	f := &q.Filter
	if inverted(f.TimeFrom, f.TimeTo) {
		return fmt.Errorf("%w: TimeFrom %d is after TimeTo %d", ErrInvalidQuery, *f.TimeFrom, *f.TimeTo)
	}

	return nil
}

// hasFieldPredicate reports whether f tests an HSMS header field.
func (f *Filter) hasFieldPredicate() bool {
	return len(f.SF) > 0 || len(f.SessionIDs) > 0 || f.SystemBytes != nil
}

// matchHeader reports whether the stored record header h satisfies the predicates of f on the record header:
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

// matchFields reports whether the HSMS header fields h of a record's payload satisfy the field predicates of f,
// each field available iff its bit is set in h.Available.
func (f *Filter) matchFields(h *HSMSHeader) bool {
	if len(f.SF) > 0 &&
		!f.satisfied(h.Available, FieldValidityStreamAndW|FieldValidityFunction, slices.Contains(f.SF, SF{Stream: h.Stream, Function: h.Function})) {
		return false
	}
	if len(f.SessionIDs) > 0 && !f.satisfied(h.Available, FieldValiditySessionID, slices.Contains(f.SessionIDs, h.SessionID)) {
		return false
	}
	if f.SystemBytes != nil && !f.satisfied(h.Available, FieldValiditySystemBytes, *f.SystemBytes == h.SystemBytes) {
		return false
	}

	return true
}

// satisfied returns the outcome of a field predicate whose fields need the bits need of validity:
// match when every one is set, else IncludeUnavailable, since an unavailable field cannot match (§7.2).
func (f *Filter) satisfied(validity, need FieldValidity, match bool) bool {
	if validity&need != need {
		return f.IncludeUnavailable
	}

	return match
}
