package secs2

import (
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"unicode"
	"unsafe"
)

var asciiQuote byte = '"'

// UseASCIISingleQuote sets the quoting character for ASCII items in SML to a single quote (').
func UseASCIISingleQuote() {
	asciiQuote = '\''
}

// UseASCIIDoubleQuote sets the quoting character for ASCII items in SML to a double quote (").
func UseASCIIDoubleQuote() {
	asciiQuote = '"'
}

// ASCIIQuote returns the quote of ASCII items
func ASCIIQuote() rune {
	return rune(asciiQuote)
}

var asciiStrictMode atomic.Bool

// WithASCIIStrictMode enables or disables strict mode for generating SML representations of ASCII items.
//
// In strict mode, non-printable ASCII characters and escape characters are represented literally in the SML output.
// This is useful for generating SML that adheres to the ASCII standard (character codes 32 to 126).
//
// By default, strict mode is disabled.
func WithASCIIStrictMode(enable bool) {
	asciiStrictMode.Store(enable)
}

// ASCIIItem represents an ASCII string in a SECS-II message.
//
// It implements the Item interface, providing methods to interact with and manipulate the ASCII data.
//
// Immutability:
// For operations that should not modify the original item, use the `Clone()` method to create a new,
// independent copy of the item.
//
// Size and Value:
// The size of an ASCIIItem is determined by the length of the string itself. Therefore, an ASCIIItem
// can only store a single string value.
type ASCIIItem struct {
	baseItem
	value    []byte // The ASCII byte literal
	rawBytes []byte // Raw bytes for the item, if needed
}

// NewASCIIItem creates a new ASCIIItem containing the given ASCII string.
//
// The input `value` must consist solely of ASCII characters (code points 0-127).
// If the string length exceeds the maximum allowed size, an error is set on the item.
// Similarly, if the string contains any non-ASCII characters, an error is set.
//
// The newly created ASCIIItem is returned, potentially with an error attached.
func NewASCIIItem(value string) Item {
	item := getASCIIItem()
	_ = item.SetValues(value)
	return item
}

// NewASCIIItemWithBytes creates a new ASCIIItem with the given raw bytes
//
// This function is useful when you have the raw byte representation of the ASCII data
// and want to create an ASCIIItem without needing to convert the string to bytes again.
//
// Note: This function does not validate the raw bytes, so it should only be used when you are sure that the
// raw bytes conform to the SECS-II data format for a ASCIIItem.
//
// Added in v1.10.0
func NewASCIIItemWithBytes(rawBytes []byte, value string) Item {
	item := getASCIIItem()
	_ = item.SetValues(value)
	item.rawBytes = rawBytes

	return item
}

// Free releases the ASCIIItem back to the pool for reuse.
//
// After calling Free, the ASCIIItem should not be accessed or used again, as its underlying memory
// might be reused for other ASCIIItem objects.
//
// This method is essential for efficient memory management when working with a large number of ASCIIItem objects.
func (item *ASCIIItem) Free() {
	putASCIIItem(item)
}

// Get implements Item.Get().
//
// It does not accept any index arguments as ASCIIItem represents a single item, not a list.
//
// If any indices are provided, an error is returned indicating the item is not a list.
func (item *ASCIIItem) Get(indices ...int) (Item, error) {
	if len(indices) != 0 {
		err := fmt.Errorf("item is not a list, item is %s, indices is %v", item.ToSML(), indices)
		item.setError(err)
		return nil, err
	}

	return item, nil
}

// ToASCII retrieves the ASCII data stored within the item.
func (item *ASCIIItem) ToASCII() (string, error) {
	return BytesToString(item.value), nil
}

// Values retrieves the ASCII string value as the any data format stored in the item.
//
// This method implements the Item.Values() interface. It returns the
// underlying ASCII string value associated with the item.
//
// The returned value can be type-asserted to a `string`.
func (item *ASCIIItem) Values() any {
	return BytesToString(item.value)
}

// SetValues sets the ASCII string for the item.
//
// This method implements the Item.SetValues() interface.
// It accepts one or more values, which must be of type `string` or `[]byte`.
// All provided string values are concatenated and stored within the item.
//
// If any of the provided values are not of type `string`, an error is returned
// and also stored within the item for later retrieval.
func (item *ASCIIItem) SetValues(values ...any) error {
	item.resetError()

	var itemValue []byte
	if len(values) == 1 { // avoid memory allocation if there is only one value
		switch val := values[0].(type) {
		case string:
			itemValue = StringToBytes(val)
		case []byte:
			itemValue = val
		default:
			err := NewItemErrorWithMsg("the value is not a string or []byte")
			item.setError(err)
			return err
		}
	} else {
		for _, value := range values {
			switch val := value.(type) {
			case string:
				itemValue = append(itemValue, StringToBytes(val)...)
			case []byte:
				itemValue = append(itemValue, val...)
			default:
				err := NewItemErrorWithMsg("the value is not a string")
				item.setError(err)
				return err
			}
		}
	}

	dataBytes, _ := getDataByteLength(ASCIIType, len(itemValue))
	if dataBytes > MaxByteSize {
		item.setErrorMsg("string length limit exceeded")
		return item.Error()
	}

	if asciiStrictMode.Load() {
		for _, ch := range itemValue {
			if ch > unicode.MaxASCII {
				item.setErrorMsg("encountered non-ASCII character")
				return item.itemErr
			}
		}
	}

	item.value = itemValue

	return nil
}

// Size implements Item.Size().
func (item *ASCIIItem) Size() int {
	return len(item.value)
}

// ToBytes serializes the ASCIIItem into a byte slice conforming to the SECS-II data format.
//
// This method implements the Item.ToBytes() interface.
//
// If an error occurs during header generation, an empty byte slice is returned,
// and the error is stored within the item for later retrieval.
func (item *ASCIIItem) ToBytes() []byte {
	if item.rawBytes != nil {
		return item.rawBytes
	}
	result, _ := getHeaderBytes(ASCIIType, item.Size(), len(item.value))
	result = append(result, item.value...)
	item.rawBytes = result

	return result
}

// ToSML converts the ASCIIItem into its SML representation.
//
// This method implements the Item.ToSML() interface. It generates an SML string that
// represents the ASCII data stored in the item.
//
// It has two modes: strict and non-strict.
//
// The format of strict mode is as follows:
//   - `<A ...>`: The overall tag indicating an ASCII item.
//   - Within the tag:
//   - Printable ASCII characters are enclosed in double quotes (e.g., "Hello").
//   - Non-printable control characters (code points < 32 or 127) are represented in hexadecimal format (e.g., 0x0A for newline).
//   - If the item's value is empty, it's represented as `<A[0] ”>` or <A[0] "">.
//
// The format of non-strict mode is as follows:
//   - `<A ...>`: The overall tag indicating an ASCII item.
//   - Within the tag: the original value without encoding to hexadecimal and escaped format.
//
// Note: the non-strict mode can't handle non-printable characters and quote escaping well.
func (item *ASCIIItem) ToSML() string {
	if asciiStrictMode.Load() {
		return item.toSMLStrict()
	}

	return item.toSMLFast()
}

func (item *ASCIIItem) toSMLStrict() string {
	if len(item.value) == 0 {
		if asciiQuote == '"' {
			return "<A[0] \"\">"
		}

		return "<A[0] ''>"
	}

	var sb strings.Builder

	sizeStr := strconv.FormatInt(int64(item.Size()), 10)
	sb.Grow(len(item.value) + len(sizeStr)) // Pre-allocate space, accounting for "<A", ">", and potential quotes

	_, _ = sb.WriteString("<A[")
	sb.WriteString(sizeStr)
	sb.WriteByte(']')

	inPrintableRun := false

	for _, ch := range item.value {
		// 0x20: space, which is the first printable character, 0x7f: del
		isPrintable := ch >= 0x20 && ch != 0x7f

		if isPrintable && !inPrintableRun {
			sb.WriteByte(' ')
			sb.WriteByte(asciiQuote) // Start a printable run
			inPrintableRun = true
		} else if !isPrintable && inPrintableRun {
			sb.WriteByte(asciiQuote) // End a printable run
			inPrintableRun = false
		}

		if isPrintable {
			if ch == asciiQuote { // write escape char for quote
				sb.WriteRune('\\')
			}
			sb.WriteByte(ch)
		} else {
			_, _ = fmt.Fprintf(&sb, " 0x%02X", ch) // 0xNN format
		}
	}

	if inPrintableRun {
		sb.WriteByte(asciiQuote) // Close the final printable run if needed
	}

	sb.WriteByte('>') // Add the closing tag

	return sb.String()
}

func (item *ASCIIItem) toSMLFast() string {
	if len(item.value) == 0 {
		if asciiQuote == '"' {
			return "<A[0] \"\">"
		}

		return "<A[0] ''>"
	}

	var sb strings.Builder

	sizeStr := strconv.FormatInt(int64(item.Size()), 10)
	sb.Grow(len(item.value) + len(sizeStr)) // Pre-allocate space, accounting for "<A", ">", and potential quotes

	sb.WriteString("<A[")
	sb.WriteString(sizeStr)
	sb.WriteString("] ")

	sb.WriteByte(asciiQuote)
	sb.Write(item.value)
	sb.WriteByte(asciiQuote)

	sb.WriteByte('>') // Add the closing tag

	return sb.String()
}

// Clone creates a deep copy of the ASCIIItem.
//
// This method implements the Item.Clone() interface. It returns a new ASCIIItem
// with the same ASCII string value as the original item. Since strings are immutable in Go,
// a simple copy of the `value` field is sufficient to create a deep copy.
//
// The `SetValues` method is assumed to create a new string when modifying the item's value,
// ensuring that the cloned item remains independent of any future changes to the original.
func (item *ASCIIItem) Clone() Item {
	return &ASCIIItem{value: item.value}
}

// Type returns "ascii" string.
func (item *ASCIIItem) Type() string { return ASCIIType }

// IsASCII returns true, indicating that ASCIIItem is a ASCII data item.
func (item *ASCIIItem) IsASCII() bool { return true }

// StringToBytes converts a string to a byte slice without memory allocation.
func StringToBytes(s string) []byte {
	return unsafe.Slice(unsafe.StringData(s), len(s))
}

// BytesToString converts a byte slice to a string without memory allocation.
func BytesToString(bs []byte) string {
	return unsafe.String(unsafe.SliceData(bs), len(bs))
}
