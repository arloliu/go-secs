package tlv

import (
	"maps"
	"math"
	"slices"
)

// MaxNestingDepth is the number of levels of nested tlv values Validate descends into.
// The package's own registries nest at most two levels deep;
// the limit stops a registry that names itself, directly or through a cycle, from recursing without end.
const MaxNestingDepth = 32

// Validate checks a decoded entry list against the registry of its structure,
// applying the rules of the tracepack format specification §5,
// with every tag of the registry known, those of the newest minor included:
// it is ValidateMinor for the newest minor.
//
// Each entry of a known tag is checked in turn:
// its value_type must equal the registry's, unknown value types included,
// and a caller-built value of a fixed-length type must have that length;
// its value must pass the content rule the typed accessor of its type applies
// (a bool is 0 or 1, a u64 is at most MaxU64, a utf8 value is valid UTF-8, a leading BOM included);
// if its field names a Nested registry, its value must decode and validate against that registry;
// and a second occurrence of a tag that is not repeatable is an error.
// Finally a tag the registry marks Required must be present.
// Unknown tags, private ones included, always pass, may repeat, and have their values left unexamined,
// as does a known tlv tag whose field names no nested registry.
//
// Validate descends at most MaxNestingDepth levels of nested values;
// a tlv value nested deeper is an error wrapping ErrDepth.
//
// Returns:
//   - error: nil, or an *EntryError for the first offending entry in list order;
//     if every entry passes, one for the lowest-numbered missing required tag, with Offset -1.
//     A failure inside a nested value is an *EntryError for the enclosing tlv entry
//     that wraps the nested entry's *EntryError, whose offset is relative to the start of the nested value.
func Validate(entries []Entry, reg Registry) error {
	return ValidateMinor(entries, reg, math.MaxUint16)
}

// ValidateMinor is Validate for an entry list of a pack whose file header states the format minor:
// a field whose Since is above minor is unknown, at every nesting level,
// so the list is checked as a reader of that minor checks it (the tracepack format specification §5 and §14).
// A minor above the newest one this package knows reads with every tag known.
//
// Returns:
//   - error: as for Validate.
func ValidateMinor(entries []Entry, reg Registry, minor uint16) error {
	return validateAt(entries, reg, minor, 0)
}

// validateAt implements ValidateMinor for an entry list nested depth levels deep.
func validateAt(entries []Entry, reg Registry, minor uint16, depth int) error {
	seen := make(map[uint16]struct{}, len(reg))
	for _, e := range entries {
		f, known := reg[e.Tag]
		if !known || f.Since > minor {
			continue
		}
		if err := e.CheckValue(f.Type); err != nil {
			return err
		}
		if f.Type == TypeTLV && f.Nested != nil {
			if err := e.validateNested(*f.Nested, minor, depth); err != nil {
				return err
			}
		}
		if _, dup := seen[e.Tag]; dup && !f.Repeatable {
			return e.errorf(ErrDuplicate)
		}
		seen[e.Tag] = struct{}{}
	}

	for _, tag := range slices.Sorted(maps.Keys(reg)) {
		f := reg[tag]
		if _, ok := seen[tag]; !ok && f.Required && f.Since <= minor {
			return &EntryError{Tag: tag, Offset: -1, Err: ErrMissing}
		}
	}

	return nil
}

// validateNested decodes the value of the tlv entry e, which lies in an entry list nested depth levels deep,
// and validates the result against reg as a pack of the minor reads it.
func (e Entry) validateNested(reg Registry, minor uint16, depth int) error {
	if depth >= MaxNestingDepth {
		return e.errorf(ErrDepth)
	}
	nested, err := Decode(e.Value)
	if err != nil {
		return e.errorf(err)
	}
	if err := validateAt(nested, reg, minor, depth+1); err != nil {
		return e.errorf(err)
	}

	return nil
}
