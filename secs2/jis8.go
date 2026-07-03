package secs2 //nolint:dupl // JIS8Item intentionally mirrors ASCIIItem; they are distinct SECS-II types.

import (
	"strconv"
	"strings"
)

// JIS8Item represents an immutable JIS-8 string in a SECS-II message.
//
// It implements the Item interface. All methods are safe for concurrent use. The backing
// field is an immutable Go string, so reads from ToJIS8 require no copy and never expose
// mutable internal state.
type JIS8Item struct {
	baseItem
	value string
}

var _ Item = (*JIS8Item)(nil)

// NewJIS8Item creates a new JIS8Item containing the given string.
//
// Any string is accepted; strict-mode JIS-8 validation is not currently enforced. If the
// string's byte length exceeds MaxByteSize, a deferred error is stored on the returned item;
// call Error() to inspect it.
func NewJIS8Item(value string) Item {
	item := &JIS8Item{}

	if len(value) > MaxByteSize {
		item.setErrorMsg("string length limit exceeded")

		return item
	}

	item.value = value

	return item
}

// ToJIS8 returns the string value stored in this item. Returns an error if the item carries
// a deferred construction error.
func (item *JIS8Item) ToJIS8() (string, error) {
	if item.itemErr != nil {
		return "", item.itemErr
	}

	return item.value, nil
}

// Size returns the byte length of the string value.
func (item *JIS8Item) Size() int { return len(item.value) }

// Type returns "jis8".
func (item *JIS8Item) Type() string { return JIS8Type }

// IsJIS8 returns true.
func (item *JIS8Item) IsJIS8() bool { return true }

// EncodedLen returns the total SECS-II wire byte length (header + payload).
// Returns 0 for items with deferred errors.
func (item *JIS8Item) EncodedLen() int {
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
func (item *JIS8Item) AppendTo(dst []byte) []byte {
	if item.itemErr != nil {
		return dst
	}

	if item.raw != nil {
		return append(dst, item.raw...)
	}

	dst, _ = appendHeaderBytes(dst, JIS8Type, len(item.value)) //nolint:errcheck

	return append(dst, item.value...)
}

// ToBytes allocates a single buffer and returns the SECS-II wire encoding.
// Equivalent to AppendTo(make([]byte, 0, EncodedLen())).
func (item *JIS8Item) ToBytes() []byte {
	return item.AppendTo(make([]byte, 0, item.EncodedLen()))
}

// ToSML returns the SML (SECS Message Language) text representation of this item.
//
// Uses the default non-strict format. Empty strings are rendered as <J[0] "">;
// non-empty strings as <J[N] "value">. The default quote character is a double quote.
func (item *JIS8Item) ToSML() string {
	if len(item.value) == 0 {
		return `<J[0] "">`
	}

	var sb strings.Builder

	sizeStr := strconv.FormatInt(int64(len(item.value)), 10)
	sb.Grow(len(item.value) + len(sizeStr) + 8) // "<J[" + N + `] "` + value + `">`

	sb.WriteString("<J[")
	sb.WriteString(sizeStr)
	sb.WriteString(`] "`)
	sb.WriteString(item.value)
	sb.WriteByte('"')
	sb.WriteByte('>')

	return sb.String()
}
