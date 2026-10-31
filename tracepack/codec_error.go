package tracepack

import (
	"errors"
	"fmt"
	"strings"

	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// fieldErrorOf returns err, an error of encoding or decoding a TLV body whose registry is reg,
// with the fault of its entry reported as a *FieldError:
// the field is the entry's registry name, dotted through the nested registries for a nested entry,
// or "0x%04X" for a tag its registry does not name;
// its Err wraps ErrRequiredTag for a missing tag, ErrFieldLength for a value of the wrong length,
// and ErrFieldValue for any other fault of a value, the outermost *tlv.EntryError beside it.
//
// It returns err unchanged when err already holds a *FieldError or holds no *tlv.EntryError,
// and when the entry list itself ends inside an entry (tlv.ErrTruncated at the top level),
// a fault of the bytes that no field owns.
// An entry list nested in a value that ends inside an entry is a fault of the value that holds it.
func fieldErrorOf(err error, reg tlv.Registry) error {
	var fe *FieldError
	if err == nil || errors.As(err, &fe) {
		return err
	}
	var outer *tlv.EntryError
	if !errors.As(err, &outer) {
		return err
	}

	var path []string
	ee := outer
	for {
		f, known := reg[ee.Tag]
		inner, nested := ee.Err.(*tlv.EntryError) //nolint:errorlint // walks the nested entry errors link by link
		if nested {
			path = append(path, tagName(f, known, ee.Tag))
			reg = nil
			if known && f.Nested != nil {
				reg = *f.Nested
			}
			ee = inner

			continue
		}

		sentinel := ErrFieldValue
		switch {
		case errors.Is(ee.Err, tlv.ErrTruncated):
			// The value holding the nested list is the field at fault; the top-level list has none.
			if len(path) == 0 {
				return err
			}
		case errors.Is(ee.Err, tlv.ErrMissing):
			sentinel = ErrRequiredTag
			path = append(path, tagName(f, known, ee.Tag))
		case errors.Is(ee.Err, tlv.ErrLength):
			sentinel = ErrFieldLength
			path = append(path, tagName(f, known, ee.Tag))
		default:
			path = append(path, tagName(f, known, ee.Tag))
		}

		return &FieldError{Field: strings.Join(path, "."), Err: fmt.Errorf("%w: %w", sentinel, outer)}
	}
}

// corruptError returns err, an error of decoding a TLV body of what, also wrapping ErrCorrupt,
// with its entry's fault reported by fieldErrorOf with reg.
func corruptError(what string, err error, reg tlv.Registry) error {
	return fmt.Errorf("tracepack: decode %s: %w: %w", what, ErrCorrupt, fieldErrorOf(err, reg))
}

// tagName returns the registry name of f, the field of tag, when known, or "0x%04X" for an unknown tag.
func tagName(f tlv.Field, known bool, tag uint16) string {
	if known {
		return f.Name
	}

	return fmt.Sprintf("0x%04X", tag)
}
