package secs2

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/arloliu/go-secs/internal/util"
)

// UintItem represents a list of unsigned integer items in a SECS-II message.
//
// It implements the Item interface, providing methods to interact with and manipulate the unsigned integer data.
//
// For immutable operations (i.e., those that should not modify the original item), use the `Clone()` method to create a new, independent copy of the item.
type UintItem struct {
	baseItem
	byteSize int      // Byte size of the integers; should be either 1, 2, 4, or 8
	values   []uint64 // Array of unsigned integers
}

// NewUintItem creates a new UintItem representing unsigned integer data in a SECS-II message.
//
// This function implements the Item interface.
//
// It accepts two types of arguments:
//
// byteSize (int): The size of each integer value in bytes (1, 2, 4, or 8).
//
// values (...any): One or more values to be stored in the UintItem. Each value can be:
//   - An unsigned integer (`uint`, `uint8`, `uint16`, `uint32`, `uint64`).
//   - A slice of unsigned integers (`[]uint`, `[]uint8`, `[]uint16`, `[]uint32`, `[]uint64`).
//   - A signed integer (`int`, `int8`, `int16`, `int32`, `int64`) that is non-negative.
//   - A slice of signed integers (`[]int`, `[]int8`, `[]int16`, `[]int32`, `[]int64`) where all values are non-negative.
//   - A string representing a non-negative integer value.
//
// All provided values are combined into a single slice stored within the new item.
//
// If the `byteSize` is invalid, any of the input values cannot be converted to a valid unsigned int64,
// or any signed integer value is negative, an error is set on the item.
//
// The newly created UintItem is returned, potentially with an error attached.
func NewUintItem(byteSize int, values ...any) Item {
	item := getUintItem()

	if byteSize != 1 && byteSize != 2 && byteSize != 4 && byteSize != 8 {
		item.setErrorMsg("invalid byte size")
		return item
	}

	item.byteSize = byteSize

	_ = item.SetValues(values...)

	return item
}

// Free releases the UintItem back to the pool for reuse.
//
// After calling Free, the UintItem should not be accessed or used again, as its underlying memory
// might be reused for other UintItem objects.
//
// This method is essential for efficient memory management when working with a large number of UintItem objects.
func (item *UintItem) Free() {
	putUintItem(item)
}

// Get retrieves the current UintItem.
//
// This method implements the Item.Get() interface.
// It does not accept any index arguments as UintItem represents a single item, not a list.
//
// If any indices are provided, an error is returned indicating that the item is not a list.
func (item *UintItem) Get(indices ...int) (Item, error) {
	if len(indices) != 0 {
		err := newItemError(fmt.Errorf("item is not a list, item is %s, indices is %v", item.ToSML(), indices))
		item.setError(err)
		return nil, err
	}

	return item, nil
}

// GetUint retrieves the unsigned integer data stored within the item.
//
// This method implements a specialized version of Item.Get() for unsigned integer data retrieval.
// It returns the uint64 slice containing the unsigned integer data.
func (item *UintItem) ToUint() ([]uint64, error) {
	return item.values, nil
}

// Size implements Item.Size().
func (item *UintItem) Size() int {
	return len(item.values)
}

// Values retrieves the unsigned integer value stored in the item as a uint64 slice.
//
// This method implements the Item.Values() interface. It returns a direct
// reference to the underlying byte slice, allowing for potential modification.
//
// Caution: Modifying the returned slice will directly affect the data within the item.
// For immutable access, consider using `Clone()` to obtain a deep copy of the item first.
//
// The returned value can be type-asserted to a `[]uint64`.
func (item *UintItem) Values() any {
	return item.values
}

// SetValues sets the uint64 values within the UintItem.
//
// This method implements the Item.SetValues() interface. It accepts one or more values, each of which can be:
//   - An unsigned integer (`uint`, `uint8`, `uint16`, `uint32`, `uint64`).
//   - A signed integer (`int`, `int8`, `int16`, `int32`, `int64`) that is non-negative.
//   - A string representing a non-negative integer value.
//
// All provided values are converted to unsigned int64 values and stored within the new item.
//
// If an error occurs during the combination process (e.g., due to incompatible types or negative signed integer values),
// the error is returned and also stored within the item for later retrieval.
func (item *UintItem) SetValues(values ...any) error {
	var err error

	item.resetError()
	item.values, err = combineUintValues(item.byteSize, values...)
	if err != nil {
		item.setError(err)
		return item.Error()
	}

	dataBytes, _ := getDataByteLength(item.dataType(), len(item.values))
	if dataBytes > MaxByteSize {
		item.setErrorMsg("item size limit exceeded")
		return item.Error()
	}

	return nil
}

// ToBytes serializes the UintItem into a byte slice conforming to the SECS-II data format.
//
// This method implements the Item.ToBytes() interface.
//
// If an error occurs during header generation, an empty byte slice is returned,
// and the error is stored within the item for later retrieval.
func (item *UintItem) ToBytes() []byte {
	result, _ := getHeaderBytes(fmt.Sprintf("u%d", item.byteSize), item.Size(), len(item.values)*item.byteSize)

	switch item.byteSize {
	case 1:
		for _, value := range item.values {
			result = append(result, byte(value))
		}
	case 2:
		for _, value := range item.values {
			result = binary.BigEndian.AppendUint16(result, uint16(value)) //nolint:gosec
		}
	case 4:
		for _, value := range item.values {
			result = binary.BigEndian.AppendUint32(result, uint32(value)) //nolint:gosec
		}
	case 8:
		for _, value := range item.values {
			result = binary.BigEndian.AppendUint64(result, value)
		}
	}

	return result
}

// ToSML converts the UintItem into its SML representation.
//
// This method implements the Item.ToSML() interface. It generates an SML string
// that represents the unsigned integer data stored in the item.
//
// The format is as follows:
//   - If the item's data is empty, it's represented as `<UbyteSize[0]>`.
//   - Otherwise:
//   - `<UbyteSize[size] value1 value2 ...>`
//   - `byteSize`: The size of each integer in bytes (1, 2, 4, or 8).
//   - `size`: The number of integers in the list
//   - `value1`, `value2`, ...: Each integer is represented in decimal format.
func (item *UintItem) ToSML() string {
	if item.Size() == 0 {
		return fmt.Sprintf("<U%d[0]>", item.byteSize)
	}

	var sb strings.Builder
	// Estimate capacity based on average decimal representation length and other components
	sb.Grow(len(item.values)*10 + 10) // Adjust 10 based on typical uint64 values

	sb.WriteString(fmt.Sprintf("<U%d[%d] ", item.byteSize, item.Size()))

	// Reuse a buffer for strconv.AppendUint to avoid allocations
	var uintBuf [20]byte // Enough for the largest possible uint64
	for i, v := range item.values {
		if i > 0 {
			sb.WriteByte(' ') // Add space separator between values
		}
		sb.Write(strconv.AppendUint(uintBuf[:0], v, 10))
	}

	sb.WriteByte('>') // Close the SML tag
	return sb.String()
}

// Clone creates a deep copy of the UintItem.
//
// This method implements the Item.Clone() interface. It returns a new
// UintItem with a completely independent copy of the integer data.
func (item *UintItem) Clone() Item {
	return &UintItem{byteSize: item.byteSize, values: util.CloneSlice(item.values, 0)}
}

func (item *UintItem) Type() string {
	switch item.byteSize {
	case 1:
		return Uint8Type
	case 2:
		return Uint16Type
	case 4:
		return Uint32Type
	case 8:
		return Uint64Type
	default:
		return ""
	}
}

func (item *UintItem) IsUint8() bool  { return item.byteSize == 1 }
func (item *UintItem) IsUint16() bool { return item.byteSize == 2 }
func (item *UintItem) IsUint32() bool { return item.byteSize == 4 }
func (item *UintItem) IsUint64() bool { return item.byteSize == 8 }

func (item *UintItem) dataType() string {
	dataTypeStr := [9]string{"", "u1", "u2", "", "u4", "", "", "", "u8"}
	return dataTypeStr[item.byteSize]
}

func combineUintValues(byteSize int, values ...any) ([]uint64, error) { //nolint:gocyclo,cyclop
	itemValues := make([]uint64, 0, len(values))
	for _, value := range values {
		switch value := value.(type) {
		case uint:
			itemValues = append(itemValues, uint64(value))
		case []uint:
			itemValues = util.AppendUint64Slice(itemValues, value)
		case uint8:
			itemValues = append(itemValues, uint64(value))
		case []uint8:
			itemValues = util.AppendUint64Slice(itemValues, value)

		case uint16:
			itemValues = append(itemValues, uint64(value))
		case []uint16:
			itemValues = util.AppendUint64Slice(itemValues, value)

		case uint32:
			itemValues = append(itemValues, uint64(value))
		case []uint32:
			itemValues = util.AppendUint64Slice(itemValues, value)

		case uint64:
			itemValues = append(itemValues, value)
		case []uint64:
			itemValues = util.AppendUint64Slice(itemValues, value)

		case int:
			if value < 0 {
				return nil, errors.New("negative value not allowed for UintItem")
			}
			itemValues = append(itemValues, uint64(value))
		case []int:
			for _, v := range value {
				if v < 0 {
					return nil, errors.New("negative value not allowed for UintItem")
				}
				itemValues = append(itemValues, uint64(v))
			}
		case int8:
			if value < 0 {
				return nil, errors.New("negative value not allowed for UintItem")
			}
			itemValues = append(itemValues, uint64(value))
		case []int8:
			for _, v := range value {
				if v < 0 {
					return nil, errors.New("negative value not allowed for UintItem")
				}
				itemValues = append(itemValues, uint64(v))
			}

		case int16:
			if value < 0 {
				return nil, errors.New("negative value not allowed for UintItem")
			}
			itemValues = append(itemValues, uint64(value))
		case []int16:
			for _, v := range value {
				if v < 0 {
					return nil, errors.New("negative value not allowed for UintItem")
				}
				itemValues = append(itemValues, uint64(v))
			}

		case int32:
			if value < 0 {
				return nil, errors.New("negative value not allowed for UintItem")
			}
			itemValues = append(itemValues, uint64(value))
		case []int32:
			for _, v := range value {
				if v < 0 {
					return nil, errors.New("negative value not allowed for UintItem")
				}
				itemValues = append(itemValues, uint64(v))
			}

		case int64:
			if value < 0 {
				return nil, errors.New("negative value not allowed for UintItem")
			}
			itemValues = append(itemValues, uint64(value))
		case []int64:
			for _, v := range value {
				if v < 0 {
					return nil, errors.New("negative value not allowed for UintItem")
				}
				itemValues = append(itemValues, uint64(v))
			}

		case string:
			uintVal, err := strconv.ParseUint(value, 0, 0) // Use ParseUint for unsigned values
			if err != nil {
				return nil, err
			}
			itemValues = append(itemValues, uintVal)
		case []string:
			for _, v := range value {
				uintVal, err := strconv.ParseUint(v, 0, 0)
				if err != nil {
					return nil, err
				}
				itemValues = append(itemValues, uintVal)
			}

		default:
			return nil, errors.New("input argument contains invalid type for UintItem")
		}
	}

	var maxVal uint64 = 1<<(byteSize*8) - 1 // Calculate max value for unsigned based on byteSize

	for _, v := range itemValues {
		if v > maxVal {
			return nil, errors.New("value overflow")
		}
	}

	return itemValues, nil
}
