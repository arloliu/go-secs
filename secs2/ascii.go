package secs2 //nolint:dupl // ASCIIItem intentionally mirrors JIS8Item; they are distinct SECS-II types.

import (
	"strconv"
	"strings"
)

// ASCIIItem represents an immutable ASCII string in a SECS-II message.
//
// It implements the Item interface. All methods are safe for concurrent use. The backing
// field is an immutable Go string, so reads from ToASCII require no copy and never expose
// mutable internal state.
type ASCIIItem struct {
	baseItem
	value string
}

var _ Item = (*ASCIIItem)(nil)

// NewASCIIItem creates a new ASCIIItem containing the given string.
//
// Any string is accepted, including strings that contain non-ASCII bytes; strict-mode
// ASCII validation is not currently enforced. If the string's byte length exceeds
// MaxByteSize, a deferred error is stored on the returned item; call Error() to inspect it.
//
// Parameters:
//   - value: the input Go string.
//
// Returns:
//   - Item: the created ASCIIItem.
func NewASCIIItem(value string) Item {
	item := &ASCIIItem{}

	if len(value) > MaxByteSize {
		item.setErrorMsg("string length limit exceeded")

		return item
	}

	item.value = value

	return item
}

// ToASCII returns the string value stored in this item. Returns an error if the item carries
// a deferred construction error.
func (item *ASCIIItem) ToASCII() (string, error) {
	if item.itemErr != nil {
		return "", item.itemErr
	}

	return item.value, nil
}

// Size returns the byte length of the string value.
func (item *ASCIIItem) Size() int { return len(item.value) }

// Type returns "ascii".
func (item *ASCIIItem) Type() string { return ASCIIType }

// IsASCII returns true.
func (item *ASCIIItem) IsASCII() bool { return true }

// EncodedLen returns the total SECS-II wire byte length (header + payload).
// Returns 0 for items with deferred errors.
func (item *ASCIIItem) EncodedLen() int {
	if item.itemErr != nil {
		return 0
	}

	if item.raw != nil {
		return len(item.raw)
	}

	n := len(item.value)

	return headerLen(n) + n
}

// AppendTo appends the SECS-II wire encoding of this item into dst and returns the result.
// Returns dst unchanged for items with deferred errors.
func (item *ASCIIItem) AppendTo(dst []byte) []byte {
	if item.itemErr != nil {
		return dst
	}

	if item.raw != nil {
		return append(dst, item.raw...)
	}

	dst, _ = appendHeaderBytes(dst, ASCIIType, len(item.value)) //nolint:errcheck

	return append(dst, item.value...)
}

// ToBytes allocates a single buffer and returns the SECS-II wire encoding.
// Equivalent to AppendTo(make([]byte, 0, EncodedLen())).
func (item *ASCIIItem) ToBytes() []byte {
	return item.AppendTo(make([]byte, 0, item.EncodedLen()))
}

// ToSML returns the SML (SECS Message Language) text representation of this item.
//
// Uses the default non-strict format. Empty strings are rendered as <A[0] "">;
// non-empty strings as <A[N] "value">. The default quote character is a double quote.
func (item *ASCIIItem) ToSML() string {
	if len(item.value) == 0 {
		return `<A[0] "">`
	}

	var sb strings.Builder

	sizeStr := strconv.FormatInt(int64(len(item.value)), 10)
	sb.Grow(len(item.value) + len(sizeStr) + 7) // "<A[" + N + `] "` + value + `"` + ">"

	sb.WriteString("<A[")
	sb.WriteString(sizeStr)
	sb.WriteString(`] "`)
	sb.WriteString(item.value)
	sb.WriteByte('"')
	sb.WriteByte('>')

	return sb.String()
}
