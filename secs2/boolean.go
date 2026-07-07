package secs2

import (
	"errors"
	"iter"
	"slices"
	"strconv"
	"strings"
)

// BooleanItem represents an immutable list of boolean values in a SECS-II message.
//
// It implements the Item interface. All methods are safe for concurrent use and no method
// exposes mutable internal storage.
//
// Construct via NewBooleanItem. Accepted value types: bool and []bool.
type BooleanItem struct {
	size   int32
	scalar bool
	baseItem
	values []bool
}

var _ Item = (*BooleanItem)(nil)

// NewBooleanItem creates a new BooleanItem from the given values.
//
// Each argument can be:
//   - A single bool value.
//   - A []bool slice (all values are appended).
//
// If any argument is of an unsupported type, a deferred error is stored on the returned item;
// call Error() to inspect it. The deferred-error model means this function never panics.
//
// Parameters:
//   - values: the input arguments representing boolean values.
//
// Returns:
//   - Item: the created BooleanItem.
func NewBooleanItem(values ...any) Item {
	item := &BooleanItem{}

	if err := item.combineBoolValues(values...); err != nil {
		item.setError(err)

		return item
	}

	item.size = int32(len(item.values))
	if item.size == 1 {
		item.scalar = item.values[0]
		item.values = nil
	}

	if n, _ := getDataByteLength(BooleanType, int(item.size)); n > MaxByteSize {
		item.setErrorMsg("item size limit exceeded")
	}

	return item
}

// ToBoolean returns a fresh copy of the boolean values.
//
// Returns an error if the item carries a deferred construction error.
//
// It returns the item's deferred error (see Error) when the item was constructed with
// one — a passing Is* predicate does NOT imply a nil error here. Always check the
// returned error; do not discard it via `v, _ := item.ToBoolean()`.
func (item *BooleanItem) ToBoolean() ([]bool, error) {
	if item.itemErr != nil {
		return nil, item.itemErr
	}

	if item.size == 0 {
		return []bool{}, nil
	}
	if item.size == 1 {
		return []bool{item.scalar}, nil
	}

	return slices.Clone(item.values), nil
}

// BoolAt returns the boolean value at index i.
//
// Returns an error if the item carries a deferred error or i is out of range.
func (item *BooleanItem) BoolAt(i int) (bool, error) {
	if item.itemErr != nil {
		return false, item.itemErr
	}

	if i < 0 || i >= int(item.size) {
		return false, NewItemErrorWithMsg("index out of range")
	}

	if item.size == 1 {
		return item.scalar, nil
	}

	return item.values[i], nil
}

// Bools returns an iterator over the boolean values.
//
// Yields nothing if the item carries a deferred error.
func (item *BooleanItem) Bools() iter.Seq[bool] {
	return func(yield func(bool) bool) {
		if item.itemErr != nil {
			return
		}

		if item.size == 1 {
			_ = yield(item.scalar)
			return
		}

		for _, v := range item.values {
			if !yield(v) {
				return
			}
		}
	}
}

// Size returns the number of boolean values in this item.
func (item *BooleanItem) Size() int { return int(item.size) }

// EncodedLen returns the total SECS-II wire byte length (header + payload).
// Returns 0 for items with deferred errors.
func (item *BooleanItem) EncodedLen() int {
	if item.itemErr != nil {
		return 0
	}

	if item.rawPtr != nil {
		return item.rawLen
	}

	return headerLen(int(item.size)) + int(item.size)
}

// AppendTo appends the SECS-II wire encoding of this item into dst and returns the result.
// Returns dst unchanged for items with deferred errors.
func (item *BooleanItem) AppendTo(dst []byte) []byte {
	if item.itemErr != nil {
		return dst
	}

	if item.rawPtr != nil {
		return append(dst, item.raw()...)
	}

	dst, _ = appendHeaderBytes(dst, BooleanType, int(item.size)) //nolint:errcheck

	if item.size == 0 {
		return dst
	}

	if item.size == 1 {
		if item.scalar {
			dst = append(dst, 1)
		} else {
			dst = append(dst, 0)
		}

		return dst
	}

	for _, v := range item.values {
		if v {
			dst = append(dst, 1)
		} else {
			dst = append(dst, 0)
		}
	}

	return dst
}

// ToBytes allocates a single buffer and returns the SECS-II wire encoding.
// Equivalent to AppendTo(make([]byte, 0, EncodedLen())).
func (item *BooleanItem) ToBytes() []byte {
	return item.AppendTo(make([]byte, 0, item.EncodedLen()))
}

// Type returns "boolean".
func (item *BooleanItem) Type() string { return BooleanType }

// IsBoolean returns true.
//
// It reflects the DECLARED type only and does not consult the item's deferred error:
// a true result does not imply the item is usable. Gate value extraction on Error()
// (or the To* accessor's returned error), not on Is* alone.
func (item *BooleanItem) IsBoolean() bool { return true }

// ToSML returns the SML (SECS Message Language) text representation of this item.
// Boolean values are rendered as "True" or "False". An empty item is rendered as "<BOOLEAN[0]>".
func (item *BooleanItem) ToSML() string {
	if item.size == 0 {
		return "<BOOLEAN[0]>"
	}

	var sb strings.Builder

	sb.Grow(int(item.size)*6 + 12) //nolint:mnd

	sb.WriteString("<BOOLEAN[")
	sb.WriteString(strconv.Itoa(int(item.size)))
	sb.WriteString("] ")

	if item.size == 1 {
		if item.scalar {
			sb.WriteString("True")
		} else {
			sb.WriteString("False")
		}
	} else {
		for i, v := range item.values {
			if i > 0 {
				sb.WriteByte(' ')
			}

			if v {
				sb.WriteString("True")
			} else {
				sb.WriteString("False")
			}
		}
	}

	sb.WriteByte('>')

	return sb.String()
}

// combineBoolValues parses the variadic inputs and appends their boolean values into item.values.
// Accepted types: bool and []bool.
func (item *BooleanItem) combineBoolValues(values ...any) error {
	capacity := len(values)
	if len(values) == 1 {
		if v, ok := values[0].([]bool); ok {
			capacity = len(v)
		}
	}

	item.values = make([]bool, 0, capacity)

	for _, value := range values {
		switch v := value.(type) {
		case bool:
			item.values = append(item.values, v)
		case []bool:
			item.values = append(item.values, v...)
		default:
			return errors.New("input argument contains invalid type for BooleanItem")
		}
	}

	return nil
}
