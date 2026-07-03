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
func NewBooleanItem(values ...any) Item {
	item := &BooleanItem{}

	if err := item.combineBoolValues(values...); err != nil {
		item.setError(err)

		return item
	}

	if n, _ := getDataByteLength(BooleanType, len(item.values)); n > MaxByteSize {
		item.setErrorMsg("item size limit exceeded")
	}

	return item
}

// ToBoolean returns a fresh copy of the boolean values. Returns an error if the item carries a
// deferred construction error.
func (item *BooleanItem) ToBoolean() ([]bool, error) {
	if item.itemErr != nil {
		return nil, item.itemErr
	}

	return slices.Clone(item.values), nil
}

// BoolAt returns the boolean value at index i. Returns an error if the item carries a deferred
// error or i is out of range.
func (item *BooleanItem) BoolAt(i int) (bool, error) {
	if item.itemErr != nil {
		return false, item.itemErr
	}

	if i < 0 || i >= len(item.values) {
		return false, NewItemErrorWithMsg("index out of range")
	}

	return item.values[i], nil
}

// Bools returns an iterator over the boolean values. Yields nothing if the item carries a
// deferred error.
func (item *BooleanItem) Bools() iter.Seq[bool] {
	return func(yield func(bool) bool) {
		if item.itemErr != nil {
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
func (item *BooleanItem) Size() int { return len(item.values) }

// EncodedLen returns the total SECS-II wire byte length (header + payload).
// Returns 0 for items with deferred errors.
func (item *BooleanItem) EncodedLen() int {
	if item.itemErr != nil {
		return 0
	}

	if item.raw != nil {
		return len(item.raw)
	}

	return headerLen(len(item.values)) + len(item.values)
}

// AppendTo appends the SECS-II wire encoding of this item into dst and returns the result.
// Returns dst unchanged for items with deferred errors.
func (item *BooleanItem) AppendTo(dst []byte) []byte {
	if item.itemErr != nil {
		return dst
	}

	if item.raw != nil {
		return append(dst, item.raw...)
	}

	dst, _ = appendHeaderBytes(dst, BooleanType, len(item.values)) //nolint:errcheck

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
func (item *BooleanItem) IsBoolean() bool { return true }

// ToSML returns the SML (SECS Message Language) text representation of this item.
// Boolean values are rendered as "True" or "False". An empty item is rendered as "<BOOLEAN[0]>".
func (item *BooleanItem) ToSML() string {
	if item.Size() == 0 {
		return "<BOOLEAN[0]>"
	}

	var sb strings.Builder

	sb.Grow(len(item.values)*6 + 12) //nolint:mnd

	sb.WriteString("<BOOLEAN[")
	sb.WriteString(strconv.Itoa(item.Size()))
	sb.WriteString("] ")

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
