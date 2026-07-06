package secs2

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// BinaryItem represents an immutable list of binary (byte) values in a SECS-II message.
//
// It implements the Item interface. All methods are safe for concurrent use and no method
// exposes mutable internal storage.
//
// Construct via NewBinaryItem. Accepted value types: byte, []byte, int (0–255), and string
// numeric literals (decimal, hex, octal, binary) in the range [0, 255].
type BinaryItem struct {
	baseItem
	values []byte
}

var _ Item = (*BinaryItem)(nil)

// NewBinaryItem creates a new BinaryItem from the given values.
//
// Each argument can be:
//   - A byte (uint8).
//   - A []byte slice (all bytes are appended).
//   - An int in the range [0, 255].
//   - A string containing a numeric literal (decimal, hex, octal, binary) in the range [0, 255].
//
// If any argument is out of range or of an unsupported type, a deferred error is stored on the
// returned item; call Error() to inspect it. The deferred-error model means this function never
// panics.
//
// Parameters:
//   - values: the input arguments representing binary bytes.
//
// Returns:
//   - Item: the created BinaryItem.
func NewBinaryItem(values ...any) Item {
	item := &BinaryItem{}

	if err := item.combineBinaryValues(values...); err != nil {
		item.setError(err)

		return item
	}

	if n, _ := getDataByteLength(BinaryType, len(item.values)); n > MaxByteSize {
		item.setErrorMsg("item size limit exceeded")
	}

	return item
}

// ToBinary returns a fresh copy of the binary data.
//
// Returns an error if the item carries a deferred construction error.
func (item *BinaryItem) ToBinary() ([]byte, error) {
	if item.itemErr != nil {
		return nil, item.itemErr
	}

	return slices.Clone(item.values), nil
}

// ByteAt returns the byte at index i.
//
// Returns an error if the item carries a deferred error or i is out of range.
func (item *BinaryItem) ByteAt(i int) (byte, error) {
	if item.itemErr != nil {
		return 0, item.itemErr
	}

	if i < 0 || i >= len(item.values) {
		return 0, NewItemErrorWithMsg("index out of range")
	}

	return item.values[i], nil
}

// AppendBinaryTo appends the item's byte values into dst and returns the result.
//
// Returns dst unchanged if the item carries a deferred error.
func (item *BinaryItem) AppendBinaryTo(dst []byte) []byte {
	if item.itemErr != nil {
		return dst
	}

	return append(dst, item.values...)
}

// Size returns the number of bytes stored in this item.
func (item *BinaryItem) Size() int { return len(item.values) }

// EncodedLen returns the total SECS-II wire byte length (header + payload).
// Returns 0 for items with deferred errors.
func (item *BinaryItem) EncodedLen() int {
	if item.itemErr != nil {
		return 0
	}

	if item.rawPtr != nil {
		return item.rawLen
	}

	return headerLen(len(item.values)) + len(item.values)
}

// AppendTo appends the SECS-II wire encoding of this item into dst and returns the result.
// Returns dst unchanged for items with deferred errors.
func (item *BinaryItem) AppendTo(dst []byte) []byte {
	if item.itemErr != nil {
		return dst
	}

	if item.rawPtr != nil {
		return append(dst, item.raw()...)
	}

	dst, _ = appendHeaderBytes(dst, BinaryType, len(item.values)) //nolint:errcheck

	return append(dst, item.values...)
}

// ToBytes allocates a single buffer and returns the SECS-II wire encoding.
// Equivalent to AppendTo(make([]byte, 0, EncodedLen())).
func (item *BinaryItem) ToBytes() []byte {
	return item.AppendTo(make([]byte, 0, item.EncodedLen()))
}

// ToSML returns the SML (SECS Message Language) text representation of this item.
// Binary values are rendered as upper-case, zero-padded hex literals (e.g. 0x2F).
func (item *BinaryItem) ToSML() string {
	if item.Size() == 0 {
		return "<B[0]>"
	}

	var sb strings.Builder

	sb.Grow(len(item.values)*5 + 6) //nolint:mnd

	fmt.Fprintf(&sb, "<B[%d] ", item.Size())

	for i, v := range item.values {
		if i > 0 {
			sb.WriteByte(' ')
		}

		fmt.Fprintf(&sb, "0x%02X", v)
	}

	sb.WriteByte('>')

	return sb.String()
}

// Type returns "binary".
func (item *BinaryItem) Type() string { return BinaryType }

// IsBinary returns true.
func (item *BinaryItem) IsBinary() bool { return true }

// combineBinaryValues parses the variadic inputs and appends their byte values into item.values.
// Accepted types: byte (uint8), []byte, int (0–255), and string numeric literals (0–255).
func (item *BinaryItem) combineBinaryValues(values ...any) error {
	capacity := len(values)
	if len(values) == 1 {
		if v, ok := values[0].([]byte); ok {
			capacity = len(v)
		}
	}

	item.values = make([]byte, 0, capacity)

	for _, value := range values {
		switch v := value.(type) {
		case int:
			if v < 0 || v > 255 {
				return fmt.Errorf("the value %d out of range, must between [0, 255]", v)
			}

			item.values = append(item.values, byte(v)) //nolint:gosec
		case byte: // uint8 alias
			item.values = append(item.values, v)
		case []byte:
			item.values = append(item.values, v...)
		case string:
			intVal, err := strconv.ParseInt(v, 0, 0)
			if err != nil {
				return err
			}

			if intVal < 0 || intVal > 255 {
				return fmt.Errorf("the value %d out of range, must between [0, 255]", intVal)
			}

			item.values = append(item.values, byte(intVal)) //nolint:gosec
		default:
			return errors.New("input argument contains invalid type for BinaryItem")
		}
	}

	return nil
}
