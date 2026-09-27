package tlv

import (
	"maps"
	"slices"
)

// Validate checks a decoded entry list against the registry of its structure,
// applying the rules of the tracepack format specification §5.
//
// A known tag whose value_type differs from the registry, unknown value types included, is an error;
// so is a second occurrence of a known tag that is not repeatable,
// and the absence of a tag the registry marks Required.
// Unknown tags, private ones included, always pass and may repeat.
// Value lengths of fixed-length types are checked by Decode, and value contents by the typed accessors;
// Validate checks neither, nor does it descend into nested tlv values.
//
// Returns:
//   - error: nil, or an *EntryError for the first offending entry in list order;
//     if every entry passes, one for the lowest-numbered missing required tag, with Offset -1
func Validate(entries []Entry, reg Registry) error {
	seen := make(map[uint16]struct{}, len(reg))
	for _, e := range entries {
		f, known := reg[e.Tag]
		if !known {
			continue
		}
		if e.Type != f.Type {
			return e.errorf(ErrType)
		}
		if _, dup := seen[e.Tag]; dup && !f.Repeatable {
			return e.errorf(ErrDuplicate)
		}
		seen[e.Tag] = struct{}{}
	}

	for _, tag := range slices.Sorted(maps.Keys(reg)) {
		if _, ok := seen[tag]; !ok && reg[tag].Required {
			return &EntryError{Tag: tag, Offset: -1, Err: ErrMissing}
		}
	}

	return nil
}
