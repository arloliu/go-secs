package secs2

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/arloliu/go-secs/internal/util"
)

// IntItem represents a list of boolean items in a SECS-II message.
//
// It implements the Item interface, providing methods to interact with and manipulate the boolean data.
//
// For immutable operations (i.e., those that should not modify the original item), use the `Clone()` method to create a new, independent copy of the item.
type IntItem struct {
	baseItem
	byteSize int     // Byte size of the integers; should be either 1, 2, 4, or 8
	values   []int64 // Array of integers
}

// NewIntItem creates a new IntItem representing signed integer data in a SECS-II message.
//
// This function implements the Item interface.
//
// It accepts two types of arguments:
//
// byteSize (int): The size of each integer value in bytes (1, 2, 4, or 8).
//
// values (...any): One or more values to be stored in the IntItem. Each value can be:
//   - A signed integer (`int`, `int8`, `int16`, `int32`, `int64`).
//   - A slice of signed integers (`[]int`, `[]int8`, `[]int16`, `[]int32`, `[]int64`).
//   - An unsigned integer (`uint`, `uint8`, `uint16`, `uint32`, `uint64`) that fits within the range of a signed int64.
//   - A slice of unsigned integers (`[]uint`, `[]uint8`, `[]uint16`, `[]uint32`, `[]uint64`) that fits within the range of a signed int64.
//   - A string representing a integer value.
//
// All provided values are combined into a single slice stored within the new item.
//
// If the `byteSize` is invalid or any of the input values cannot be converted to a valid signed int64
// (or are outside the representable range for the given byteSize), an error is set on the item.
//
// The newly created IntItem is returned, potentially with an error attached.
func NewIntItem(byteSize int, values ...any) Item {
	item := getIntItem()

	if byteSize != 1 && byteSize != 2 && byteSize != 4 && byteSize != 8 {
		item.setErrorMsg("invalid byte size")
		return item
	}

	item.byteSize = byteSize

	_ = item.SetValues(values...)

	return item
}

// Free releases the IntItem back to the pool for reuse.
//
// After calling Free, the IntItem should not be accessed or used again, as its underlying memory
// might be reused for other IntItem objects.
//
// This method is essential for efficient memory management when working with a large number of IntItem objects.
func (item *IntItem) Free() {
	putIntItem(item)
}

// Get retrieves the current IntItem.
//
// This method implements the Item.Get() interface.
// It does not accept any index arguments as IntItem represents a single item, not a list.
//
// If any indices are provided, an error is returned indicating that the item is not a list.
func (item *IntItem) Get(indices ...int) (Item, error) {
	if len(indices) != 0 {
		err := NewItemError(fmt.Errorf("item is not a list, item is %s, indices is %v", item.ToSML(), indices))
		item.setError(err)
		return nil, err
	}

	return item, nil
}

// ToInt retrieves the integer data stored within the item.
//
// This method implements a specialized version of Item.Get() for signed integer data retrieval.
// It returns the int64 slice containing the signed integer data.
func (item *IntItem) ToInt() ([]int64, error) {
	return item.values, nil
}

// Size implements Item.Size().
func (item *IntItem) Size() int {
	return len(item.values)
}

// Values retrieves the signed integer values stored in the item as a int64 slice.
//
// This method implements the Item.Values() interface. It returns a direct
// reference to the underlying byte slice, allowing for potential modification.
//
// Caution: Modifying the returned slice will directly affect the data within the item.
// For immutable access, consider using `Clone()` to obtain a deep copy of the item first.
//
// The returned value can be type-asserted to a `[]int64`.
func (item *IntItem) Values() any {
	return item.values
}

// SetValues sets the int64 values within the IntItem.
//
// This method implements the Item.SetValues() interface. It accepts one or more values, each of which can be:
//   - A signed integer (`int`, `int8`, `int16`, `int32`, `int64`).
//   - An unsigned integer (`uint`, `uint8`, `uint16`, `uint32`, `uint64`) that fits within the range of a signed int64.
//   - A string representing a integer value.
//
// All provided values are converted to signed int64 values and stored within the new item.
//
// If an error occurs during the combination process (e.g., due to incompatible types),
// the error is returned and also stored within the item for later retrieval.
func (item *IntItem) SetValues(values ...any) error {
	item.resetError()

	item.values = item.values[:0]

	err := item.combineIntValues(values...)
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

// ToBytes serializes the IntItem into a byte slice conforming to the SECS-II data format.
//
// This method implements the Item.ToBytes() interface.
// It generates a byte representation of the item, consisting of:
//   - A header indicating the data type as "signed integer" (I) and the size (in bytes) of each integer element (`byteSize`).
//   - The number of integers in the list (`Size()`).
//   - The actual integer values, each encoded in big-endian byte order according to the `byteSize`.
//
// If an error occurs during header generation, an empty byte slice is returned,
// and the error is stored within the item for later retrieval.
func (item *IntItem) ToBytes() []byte {
	result, _ := getHeaderBytes(item.dataType(), item.Size(), len(item.values)*item.byteSize)

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
			result = binary.BigEndian.AppendUint64(result, uint64(value)) //nolint:gosec
		}
	}

	return result
}

// ToSML converts the IntItem into its SML representation.
//
// This method implements the Item.ToSML() interface. It generates an SML string
// that represents the signed integer data stored in the item.
//
// The format is as follows:
//   - If the item's data is empty, it's represented as `<IbyteSize[0]>`.
//   - Otherwise:
//   - `<IbyteSize[size] value1 value2 ...>`
//   - `byteSize`: The size of each integer in bytes (1, 2, 4, or 8).
//   - `size`: The number of integers in the list
//   - `value1`, `value2`, ...: Each integer is represented in decimal format.
func (item *IntItem) ToSML() string {
	if item.Size() == 0 {
		return fmt.Sprintf("<I%d[0]>", item.byteSize)
	}

	var sb strings.Builder
	// Estimate capacity based on average decimal representation length and other components
	sb.Grow(len(item.values)*10 + 10) // Adjust 10 based on typical int64 values

	sb.WriteString(fmt.Sprintf("<I%d[%d] ", item.byteSize, item.Size()))

	// Reuse a buffer for strconv.AppendInt to avoid allocations
	var intBuf [20]byte // Enough for the largest possible int64
	for i, v := range item.values {
		if i > 0 {
			sb.WriteByte(' ') // Add space separator between values
		}
		sb.Write(strconv.AppendInt(intBuf[:0], v, 10))
	}

	sb.WriteByte('>') // Close the SML tag

	return sb.String()
}

// Clone creates a deep copy of the IntItem.
//
// This method implements the Item.Clone() interface. It returns a new
// IntItem with a completely independent copy of the integer data.
func (item *IntItem) Clone() Item {
	return &IntItem{byteSize: item.byteSize, values: util.CloneSlice(item.values, 0)}
}

// Type returns "i1", "i2", "i4", or "i8" string depends on the byte size.
func (item *IntItem) Type() string {
	switch item.byteSize {
	case 1:
		return Int8Type
	case 2:
		return Int16Type
	case 4:
		return Int32Type
	case 8:
		return Int64Type
	default:
		return ""
	}
}

// IsInt8 returns true, indicating that IntItem is a 8-bit signed integer data item.
func (item *IntItem) IsInt8() bool { return item.byteSize == 1 }

// IsInt16 returns true, indicating that IntItem is a 16-bit signed integer data item.
func (item *IntItem) IsInt16() bool { return item.byteSize == 2 }

// IsInt32 returns true, indicating that IntItem is a 32-bit signed integer data item.
func (item *IntItem) IsInt32() bool { return item.byteSize == 4 }

// IsInt64 returns true, indicating that IntItem is a 64-bit signed integer data item.
func (item *IntItem) IsInt64() bool { return item.byteSize == 8 }

func (item *IntItem) dataType() string {
	dataTypeStr := [9]string{"", "i1", "i2", "", "i4", "", "", "", "i8"}
	return dataTypeStr[item.byteSize]
}

func (item *IntItem) combineIntValues(values ...any) error { //nolint:cyclop
	if cap(item.values) < len(values) {
		capacity := len(values)
		if len(values) == 1 {
			switch v := values[0].(type) {
			case []int:
				capacity = len(v)
			case []int8:
				capacity = len(v)
			case []int16:
				capacity = len(v)
			case []int32:
				capacity = len(v)
			case []int64:
				capacity = len(v)
			case []uint:
				capacity = len(v)
			case []uint8:
				capacity = len(v)
			case []uint16:
				capacity = len(v)
			case []uint32:
				capacity = len(v)
			case []uint64:
				capacity = len(v)
			case []string:
				capacity = len(v)
			}
		}
		item.values = make([]int64, 0, capacity)
	} else {
		item.values = item.values[:0]
	}

	var (
		maxVal int64
		minVal int64
	)

	if item.byteSize == 8 {
		minVal = math.MinInt64
		maxVal = math.MaxInt64
	} else {
		shift := item.byteSize*8 - 1
		maxVal = (1 << shift) - 1
		minVal = -1 << shift
	}

	for _, value := range values {
		switch value := value.(type) {
		case int:
			item.values = append(item.values, clampInt64(int64(value), minVal, maxVal))
		case []int:
			for _, v := range value {
				item.values = append(item.values, clampInt64(int64(v), minVal, maxVal))
			}
		case int64:
			item.values = append(item.values, clampInt64(value, minVal, maxVal))
		case []int64:
			for _, v := range value {
				item.values = append(item.values, clampInt64(v, minVal, maxVal))
			}
		default:
			if err := item.combineIntValuesSlow(value, minVal, maxVal); err != nil {
				return err
			}
		}
	}

	return nil
}

func (item *IntItem) combineIntValuesSlow(value any, minVal, maxVal int64) error { //nolint:gocyclo,cyclop
	switch value := value.(type) {
	case int8:
		item.values = append(item.values, clampInt64(int64(value), minVal, maxVal))
	case []int8:
		for _, v := range value {
			item.values = append(item.values, clampInt64(int64(v), minVal, maxVal))
		}

	case int16:
		item.values = append(item.values, clampInt64(int64(value), minVal, maxVal))
	case []int16:
		for _, v := range value {
			item.values = append(item.values, clampInt64(int64(v), minVal, maxVal))
		}

	case int32:
		item.values = append(item.values, clampInt64(int64(value), minVal, maxVal))
	case []int32:
		for _, v := range value {
			item.values = append(item.values, clampInt64(int64(v), minVal, maxVal))
		}

	case uint8:
		item.values = append(item.values, clampInt64(int64(value), minVal, maxVal))
	case []uint8:
		for _, v := range value {
			item.values = append(item.values, clampInt64(int64(v), minVal, maxVal))
		}

	case uint16:
		item.values = append(item.values, clampInt64(int64(value), minVal, maxVal))
	case []uint16:
		for _, v := range value {
			item.values = append(item.values, clampInt64(int64(v), minVal, maxVal))
		}

	case uint32:
		item.values = append(item.values, clampInt64(int64(value), minVal, maxVal))
	case []uint32:
		for _, v := range value {
			item.values = append(item.values, clampInt64(int64(v), minVal, maxVal))
		}

	case uint:
		//nolint:gosec // maxVal is always positive
		if uint64(value) > uint64(maxVal) {
			item.values = append(item.values, maxVal)
		} else {
			//nolint:gosec // value is checked to be <= maxVal, which fits in int64
			item.values = append(item.values, int64(value))
		}
	case []uint:
		for _, v := range value {
			//nolint:gosec // maxVal is always positive
			if uint64(v) > uint64(maxVal) {
				item.values = append(item.values, maxVal)
			} else {
				//nolint:gosec // value is checked to be <= maxVal, which fits in int64
				item.values = append(item.values, int64(v))
			}
		}
	case uint64:
		//nolint:gosec // maxVal is always positive
		if value > uint64(maxVal) {
			item.values = append(item.values, maxVal)
		} else {
			//nolint:gosec // value is checked to be <= maxVal, which fits in int64
			item.values = append(item.values, int64(value))
		}
	case []uint64:
		for _, v := range value {
			//nolint:gosec // maxVal is always positive
			if v > uint64(maxVal) {
				item.values = append(item.values, maxVal)
			} else {
				//nolint:gosec // value is checked to be <= maxVal, which fits in int64
				item.values = append(item.values, int64(v))
			}
		}

	case string:
		intVal, err := strconv.ParseInt(value, 0, 64)
		if err != nil {
			var numErr *strconv.NumError
			if !errors.As(err, &numErr) || !errors.Is(numErr.Err, strconv.ErrRange) {
				return err
			}
		}
		item.values = append(item.values, clampInt64(intVal, minVal, maxVal))

	case []string:
		for _, v := range value {
			intVal, err := strconv.ParseInt(v, 0, 64)
			if err != nil {
				var numErr *strconv.NumError
				if !errors.As(err, &numErr) || !errors.Is(numErr.Err, strconv.ErrRange) {
					return err
				}
			}
			item.values = append(item.values, clampInt64(intVal, minVal, maxVal))
		}

	default:
		return errors.New("input argument contains invalid type for IntItem")
	}

	return nil
}

func clampInt64(v, minVal, maxVal int64) int64 {
	if v < minVal {
		return minVal
	}
	if v > maxVal {
		return maxVal
	}

	return v
}
