package secs2

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

var jis8Quote = '"'

// UseJIS8SingleQuote sets the quoting character for JIS-8 items in SML to a single quote (').
func UseJIS8SingleQuote() {
	jis8Quote = '\''
}

// UseJIS8DoubleQuote sets the quoting character for JIS-8 items in SML to a double quote (").
func UseJIS8DoubleQuote() {
	jis8Quote = '"'
}

// JS88Quote returns the quote of JIS-8 items
func JS88Quote() rune {
	return jis8Quote
}

// JIS8Item represents an JIS-8 string in a SECS-II message.
//
// It implements the Item interface, providing methods to interact with and manipulate the JIS-8 data.
//
// Immutability:
// For operations that should not modify the original item, use the `Clone()` method to create a new,
// independent copy of the item.
//
// Size and Value:
// The size of an JIS8Item is determined by the length of the string itself. Therefore, an JIS8Item
// can only store a single string value.
type JIS8Item struct {
	baseItem
	value string // The JIS-8 string literal
}

var _ Item = (*JIS8Item)(nil)

// NewJIS8Item creates a new JIS8Item containing the given JIS-8 string.
//
// The input `value` must consist solely of JIS-8 characters (UTF-8 range).
// If the string length exceeds the maximum allowed size, an error is set on the item.
//
// The newly created JIS8Item is returned, potentially with an error attached.
func NewJIS8Item(value string) Item {
	item := getJIS8Item()
	_ = item.SetValues(value)
	return item
}

// Free releases the JIS8Item back to the pool for reuse.
//
// After calling Free, the JIS8Item should not be accessed or used again, as its underlying memory
// might be reused for other JIS8Item objects.
//
// This method is essential for efficient memory management when working with a large number of JIS8Item objects.
func (item *JIS8Item) Free() {
	putJIS8Item(item)
}

// Get implements Item.Get().
//
// It does not accept any index arguments as JIS8Item represents a single item, not a list.
//
// If any indices are provided, an error is returned indicating the item is not a list.
func (item *JIS8Item) Get(indices ...int) (Item, error) {
	if len(indices) != 0 {
		err := fmt.Errorf("item is not a list, item is %s, indices is %v", item.ToSML(), indices)
		item.setError(err)
		return nil, err
	}

	return item, nil
}

// ToJIS8 retrieves the JIS-8 data stored within the item.
func (item *JIS8Item) ToJIS8() (string, error) {
	return item.value, nil
}

// Values retrieves the JIS-8 string value as the any data format stored in the item.
//
// This method implements the Item.Values() interface. It returns the
// underlying JIS-8 string value associated with the item.
//
// The returned value can be type-asserted to a `string`.
func (item *JIS8Item) Values() any {
	return item.value
}

// SetValues sets the JIS-8 string for the item.
//
// This method implements the Item.SetValues() interface.
// It accepts one or more values, which must all be of type `string`.
// All provided string values are concatenated and stored within the item.
//
// If any of the provided values are not of type `string`, an error is returned
// and also stored within the item for later retrieval.
func (item *JIS8Item) SetValues(values ...any) error {
	item.resetError()

	var itemValue string
	for _, value := range values {
		strVal, ok := value.(string)
		if !ok {
			err := NewItemErrorWithMsg("the value is not a string")
			item.setError(err)
			return err
		}

		itemValue += strVal
	}

	dataBytes, _ := getDataByteLength(JIS8Type, len(itemValue))
	if dataBytes > MaxByteSize {
		item.setErrorMsg("string length limit exceeded")
		return item.Error()
	}

	for _, ch := range itemValue {
		if !utf8.ValidRune(ch) {
			item.setErrorMsg("encountered invalid UTF-8 character")
			return item.Error()
		}
	}

	item.value = itemValue

	return nil
}

// Size implements Item.Size().
func (item *JIS8Item) Size() int {
	return len(item.value)
}

// ToBytes serializes the JIS8Item into a byte slice conforming to the SECS-II data format.
//
// This method implements the Item.ToBytes() interface.
//
// If an error occurs during header generation, an empty byte slice is returned,
// and the error is stored within the item for later retrieval.
func (item *JIS8Item) ToBytes() []byte {
	result, _ := getHeaderBytes(JIS8Type, item.Size(), len(item.value))
	return append(result, []byte(item.value)...)
}

// ToSML converts the JIS8Item into its SML representation.
//
// This method implements the Item.ToSML() interface. It generates an SML string that
// represents the JIS-8 data stored in the item.
func (item *JIS8Item) ToSML() string {
	if item.value == "" {
		if jis8Quote == '"' {
			return "<J[0] \"\">"
		}

		return "<J[0] ''>"
	}

	var sb strings.Builder
	// Estimate capacity based on average JIS-8 representation length (around 3 characters per byte)
	sizeStr := strconv.FormatInt(int64(item.Size()), 10)
	sb.Grow(len(item.value) + len(item.value)*3)

	sb.WriteString("<J[")
	sb.WriteString(sizeStr)
	sb.WriteString("] ")

	sb.WriteRune(jis8Quote)
	sb.WriteString(item.value)
	sb.WriteRune(jis8Quote)

	sb.WriteRune('>') // Add the closing tag

	return sb.String()
}

// Clone creates a deep copy of the JIS8Item.
//
// This method implements the Item.Clone() interface. It returns a new JIS8Item
// with the same JIS-8 string value as the original item. Since strings are immutable in Go,
// a simple copy of the `value` field is sufficient to create a deep copy.
//
// The `SetValues` method is assumed to create a new string when modifying the item's value,
// ensuring that the cloned item remains independent of any future changes to the original.
func (item *JIS8Item) Clone() Item {
	return &JIS8Item{value: item.value}
}

// Type returns "jis-8" type string.
func (item *JIS8Item) Type() string { return JIS8Type }

// IsJIS8 returns true, indicating that JIS8Item is a JIS-8 data item.
func (item *JIS8Item) IsJIS8() bool { return true }
