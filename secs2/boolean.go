package secs2

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/arloliu/go-secs/internal/util"
)

// BooleanItem represents a list of boolean values in a SECS-II message.
//
// It implements the Item interface, providing methods to interact with and manipulate the boolean data.
//
// For immutable operations (i.e., those that should not modify the original item), use the `Clone()` method to create a new, independent copy of the item.
type BooleanItem struct {
	baseItem
	values []bool // List of boolean values
}

// NewBooleanItem creates a new BooleanItem containing the provided boolean values.
//
// It accepts one or more arguments, each of which can be:
//   - A single boolean value (`bool`).
//   - A slice of boolean values (`[]bool`).
//
// All provided values are combined into a single slice stored within the new item.
//
// If the combined data exceeds the maximum allowed byte size, an error is set on the item.
// Similarly, if there's an error combining the input values (e.g., due to an unsupported type),
// an error is set on the item.
//
// The newly created BooleanItem is returned, potentially with an error attached.
func NewBooleanItem(values ...any) Item {
	item := getBooleanItem()
	_ = item.SetValues(values...)
	return item
}

// Free releases the BooleanItem back to the pool for reuse.
//
// After calling Free, the BooleanItem should not be accessed or used again, as its underlying memory
// might be reused for other BooleanItem objects.
//
// This method is essential for efficient memory management when working with a large number of BooleanItem objects.
func (item *BooleanItem) Free() {
	putBooleanItem(item)
}

// Get retrieves the current BooleanItem.
//
// This method implements the Item.Get() interface.
// It does not accept any index arguments as BooleanItem represents a single item, not a list.
//
// If any indices are provided, an error is returned indicating that the item is not a list.
func (item *BooleanItem) Get(indices ...int) (Item, error) {
	if len(indices) != 0 {
		err := fmt.Errorf("item is not a list, item is %s, indices is %v", item.ToSML(), indices)
		item.setError(err)
		return nil, err
	}

	return item, nil
}

// GetBoolean retrieves the boolean data stored within the item.
//
// This method implements a specialized version of Item.Get() for boolean data retrieval.
// It returns the bool slice containing the boolean data.
func (item *BooleanItem) ToBoolean() ([]bool, error) {
	return item.values, nil
}

// Values retrieves the binary value stored in the item as a boolean slice.
//
// This method implements the Item.Values() interface. It returns a direct
// reference to the underlying byte slice, allowing for potential modification.
//
// Caution: Modifying the returned slice will directly affect the data within the item.
// For immutable access, consider using `Clone()` to obtain a deep copy of the item first.
//
// The returned value can be type-asserted to a `[]bool`.
func (item *BooleanItem) Values() any {
	return item.values
}

// SetValues sets the boolean values within the BooleanItem.
//
// This method implements the Item.SetValues() interface. It accepts one or more values, each of which can be:
//   - A single boolean value (`bool`).
//   - A slice of boolean values (`[]bool`).
//
// All provided values are combined into a single slice and stored within the item.
//
// If an error occurs during the combination process (e.g., due to incompatible types),
// the error is returned and also stored within the item for later retrieval.
func (item *BooleanItem) SetValues(values ...any) error {
	var err error

	item.resetError()
	item.values, err = combineBoolValues(values)
	if err != nil {
		item.setError(err)
		return item.Error()
	}

	dataBytes, _ := getDataByteLength(BooleanType, len(item.values))
	if dataBytes > MaxByteSize {
		item.setErrorMsg("item size limit exceeded")
		return item.Error()
	}

	return nil
}

// Size implements Item.Size().
func (item *BooleanItem) Size() int {
	return len(item.values)
}

// ToBytes serializes the BooleanItem into a byte slice conforming to the SECS-II data format.
//
// This method implements the Item.ToBytes() interface.
//
// If an error occurs during header generation, an empty byte slice is returned,
// and the error is stored within the item for later retrieval.
func (item *BooleanItem) ToBytes() []byte {
	result, _ := getHeaderBytes(BooleanType, item.Size(), len(item.values))

	for _, value := range item.values {
		if value {
			result = append(result, 1)
		} else {
			result = append(result, 0)
		}
	}

	return result
}

// ToSML converts the BooleanItem into its SML representation.
//
// This method implements the Item.ToSML() interface.
// It generates an SML string that represents the boolean values stored in the item.
//
// The SML format is:
// - `<BOOLEAN[0]>` if the item has no values.
// - `<BOOLEAN[size] value1 value2 ...>` otherwise, where:
//   - `size`: The number of boolean values in the item.
//   - `value1`, `value2`, ...: Each value is represented as "T" (true) or "F" (false).
func (item *BooleanItem) ToSML() string {
	if item.Size() == 0 {
		return "<BOOLEAN[0]>"
	}

	itemSize := strconv.FormatInt(int64(item.Size()), 10)

	var sb strings.Builder
	sb.Grow(item.Size()*2 + 12 + len(itemSize))

	sb.WriteString("<BOOLEAN[")
	sb.WriteString(itemSize)
	sb.WriteString("] ")
	// sb.WriteString(fmt.Sprintf("<BOOLEAN[%d] ", item.Size()))

	for i, v := range item.values {
		if i > 0 {
			sb.WriteByte(' ') // Add space separator between values
		}
		if v {
			sb.WriteByte('T')
		} else {
			sb.WriteByte('F')
		}
	}

	sb.WriteByte('>') // Close the SML tag
	return sb.String()
}

// Clone creates a deep copy of the BooleanItem.
//
// This method implements the Item.Clone() interface. It returns a new
// BooleanItem with a completely independent copy of the boolean data.
func (item *BooleanItem) Clone() Item {
	return &BooleanItem{values: util.CloneSlice(item.values, 0)}
}

func (item *BooleanItem) Type() string { return BooleanType }

func (item *BooleanItem) IsBoolean() bool { return true }

func combineBoolValues(values []any) ([]bool, error) {
	itemValues := make([]bool, 0, len(values))
	for _, value := range values {
		switch v := value.(type) {
		case bool:
			itemValues = append(itemValues, v)
		case []bool:
			itemValues = append(itemValues, v...)
		default:
			return []bool{}, errors.New("the type of value needs to be bool or []bool")
		}
	}

	return itemValues, nil
}
