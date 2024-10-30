package secs2

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/arloliu/go-secs/internal/util"
)

// BinaryItem represents a list of boolean items in a SECS-II message.
//
// It implements the Item interface, providing methods to interact with and manipulate the boolean data.
//
// For immutable operations (i.e., those that should not modify the original item), use the `Clone()` method to create a new, independent copy of the item.
type BinaryItem struct {
	baseItem
	values []byte // Array of binary values
}

// NewBinaryItem creates a new BinaryItem representing binary data.
//
// This function implements the Item interface.
//
// It accepts one or more arguments, each of which can be:
//   - A single byte (`byte`)
//   - A byte slice (`[]byte`)
//   - A string representing a binary value between 0 and 255
//     (e.g., "0b1101", "0x2F", "0o0073").
//   - A integer representing a binary value between 0 and 255
//     (e.g., 1, 13, 255).
//
// All provided values are combined into a single byte slice stored within the new item.
//
// If the combined data exceeds the maximum allowed byte size, an error is set on the item.
// Similarly, if there's an error combining the input values, an error is set on the item.
//
// The newly created BinaryItem is returned, potentially with an error attached.
func NewBinaryItem(values ...any) Item {
	item := getBinaryItem()
	_ = item.SetValues(values...)
	return item
}

// Free releases the BinaryItem back to the pool for reuse.
//
// After calling Free, the BinaryItem should not be accessed or used again, as its underlying memory
// might be reused for other BinaryItem objects.
//
// This method is essential for efficient memory management when working with a large number of BinaryItem objects.
func (item *BinaryItem) Free() {
	putBinaryItem(item)
}

// Get retrieves the current BinaryItem.
//
// This method implements the Item.Get() interface.
// It does not accept any index arguments as BinaryItem represents a single item, not a list.
//
// If any indices are provided, an error is returned indicating that the item is not a list.
func (item *BinaryItem) Get(indices ...int) (Item, error) {
	if len(indices) != 0 {
		err := fmt.Errorf("item is not a list, item is %s, indices is %v", item.ToSML(), indices)
		item.setError(err)
		return nil, err
	}

	return item, nil
}

// ToBinary retrieves the binary data stored within the item.
//
// This method implements a specialized version of Item.Get() for binary data retrieval.
// It returns the byte slice containing the binary data.
func (item *BinaryItem) ToBinary() ([]byte, error) {
	return item.values, nil
}

// Values retrieves the binary value stored in the item as a byte slice.
//
// This method implements the Item.Values() interface. It returns a direct
// reference to the underlying byte slice, allowing for potential modification.
//
// Caution: Modifying the returned slice will directly affect the data within the item.
// For immutable access, consider using `Clone()` to obtain a deep copy of the item first.
//
// The returned value can be type-asserted to a `[]byte`.
func (item *BinaryItem) Values() any {
	return item.values
}

// SetValues implements Item.SetValues().
//
// It accepts one or more arguments, each of which can be:
//   - A single byte (`byte`)
//   - A byte slice (`[]byte`)
//   - A string representing a binary value between 0 and 255
//     (e.g., "0b1101", "0x2F", "0o0073").
//   - A integer representing a binary value between 0 and 255
//     (e.g., 1, 13, 255).
//
// All provided values are combined into a single byte slice and stored within the item.
//
// If an error occurs during the combination process (e.g., due to incompatible types),
// the error is returned and also stored within the item for later retrieval.
func (item *BinaryItem) SetValues(values ...any) error {
	var err error

	item.resetError()
	item.values, err = combineByteValues(values)
	if err != nil {
		item.setError(err)
		return item.Error()
	}
	dataBytes, _ := getDataByteLength(BinaryType, len(item.values))
	if dataBytes > MaxByteSize {
		item.setErrorMsg("item size limit exceeded")
		return item.Error()
	}

	return nil
}

// Size implements Item.Size().
func (item *BinaryItem) Size() int {
	return len(item.values)
}

// ToBytes serializes the BinaryItem into a byte slice conforming to the SECS-II data format.
//
// This method implements the Item.ToBytes() interface.
//
// If an error occurs during header generation, an empty byte slice is returned,
// and the error is stored within the item for later retrieval.
func (item *BinaryItem) ToBytes() []byte {
	result, _ := getHeaderBytes(BinaryType, item.Size(), len(item.values))
	return append(result, item.values...)
}

// ToSML converts the BinaryItem into its SML representation.
//
// This method implements the Item.ToSML() interface. It generates an SML string
// that represents the binary data stored in the item.
//
// The format is as follows:
//   - If the item's data is empty, it's represented as `<B[0]>`.
//   - Otherwise:
//   - `<B[size] value1 value2 ...>`
//   - `size`: The number of bytes in the binary data.
//   - `value1`, `value2`, ...: Each byte is represented in binary format (e.g., 0b10101010).
func (item *BinaryItem) ToSML() string {
	if item.Size() == 0 {
		return "<B[0]>"
	}

	var sb strings.Builder
	// Estimate capacity based on average binary representation length (around 11 characters per byte)
	sb.Grow(len(item.values)*11 + 6)

	sb.WriteString(fmt.Sprintf("<B[%d] ", item.Size()))

	// Reuse a buffer for strconv.AppendInt to avoid allocations
	var binBuf [8]byte
	for i, v := range item.values {
		if i > 0 {
			sb.WriteByte(' ') // Add space separator between values
		}
		sb.WriteString("0b")
		sb.Write(strconv.AppendInt(binBuf[:0], int64(v), 2))
	}

	sb.WriteByte('>') // Close the SML tag

	return sb.String()
}

// Clone creates a deep copy of the BinaryItem.
//
// This method implements the Item.Clone() interface. It returns a new
// BinaryItem with a completely independent copy of the binary data.
func (item *BinaryItem) Clone() Item {
	return &BinaryItem{values: util.CloneSlice(item.values, 0)}
}

// Type returns "binary" string.
func (item *BinaryItem) Type() string { return BinaryType }

// IsBinary returns true, indicating that BinaryItem is a binary data item.
func (item *BinaryItem) IsBinary() bool { return true }

func combineByteValues(values []any) ([]byte, error) {
	itemValues := make([]byte, 0, len(values))
	for _, value := range values {
		switch v := value.(type) {
		case int:
			if v < 0 || v > 255 {
				return nil, fmt.Errorf("the value %d out of range, must between [0, 255]", v)
			}
			itemValues = append(itemValues, byte(v))
		case byte:
			itemValues = append(itemValues, v)
		case []byte:
			itemValues = append(itemValues, v...)
		case string:
			intVal, err := strconv.ParseInt(v, 0, 0)
			if err != nil {
				return nil, err
			}
			if intVal < 0 || intVal > 255 {
				return nil, fmt.Errorf("the value %d out of range, must between [0, 255]", intVal)
			}
			itemValues = append(itemValues, byte(intVal))
		default:
			return []byte{}, errors.New("the type of value needs to be byte, []byte, or string")
		}
	}

	return itemValues, nil
}
