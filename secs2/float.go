package secs2

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/arloliu/go-secs/internal/util"
)

// FloatItem represents a list of floating-point data items in a SECS-II message.
//
// It implements the Item interface, providing methods to interact with and manipulate the floating-point data.
//
// For immutable operations, use the `Clone()` method to create a new, independent copy of the item.
type FloatItem struct {
	baseItem
	byteSize int       // Byte size of the floats; should be either 4 or 8
	values   []float64 // Array of floats
}

// NewFloatItem creates a new FloatItem representing floating-point data in a SECS-II message.
//
// This function implements the Item interface.
//
// It accepts two types of arguments:
//
// byteSize (int): The size of each float value in bytes (4 or 8).
//
// values (...any): One or more values to be stored in the FloatItem. Each value can be one of the following types:
//
//   - float32 or float64: A floating-point number.
//   - []float32 or []float64: A slice of floating-point numbers.
//   - string: A string that will be parsed as a floating-point number.
//   - []string: A slice of strings that will be parsed as floating-point numbers.
//   - int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64: An integer that will be converted to its floating-point representation.
//   - A slice of any of the above-mentioned integer types
//
// All provided values are combined into a single slice stored within the new item.
//
// If the `byteSize` is invalid or any of the input values cannot be converted to a valid float64
// (or are outside the representable range for the given byteSize), an error is set on the item.
//
// The newly created FloatItem is returned, potentially with an error attached.
func NewFloatItem(byteSize int, values ...any) Item {
	item := getFloatItem()
	// item := &FloatItem{}
	if byteSize != 4 && byteSize != 8 {
		item.setErrorMsg("invalid byte size")
		return item
	}

	item.byteSize = byteSize

	_ = item.SetValues(values...)

	return item
}

// Free releases the FloatItem back to the pool for reuse.
//
// After calling Free, the FloatItem should not be accessed or used again, as its underlying memory
// might be reused for other FloatItem objects.
//
// This method is essential for efficient memory management when working with a large number of FloatItem objects.
func (item *FloatItem) Free() {
	putFloatItem(item)
}

// Get retrieves the current FloatItem.
//
// This method implements the Item.Get() interface.
// It does not accept any index arguments as FloatItem represents a single item, not a list.
//
// If any indices are provided, an error is returned indicating that the item is not a list.
func (item *FloatItem) Get(indices ...int) (Item, error) {
	if len(indices) != 0 {
		err := newItemError(fmt.Errorf("item is not a list, item is %s, indices is %v", item.ToSML(), indices))
		item.setError(err)
		return nil, err
	}

	return item, nil
}

// ToFloat retrieves the float data stored within the item.
//
// This method implements a specialized version of Item.Get() for float data retrieval.
// It returns the float64 slice containing the float data.
func (item *FloatItem) ToFloat() ([]float64, error) {
	return item.values, nil
}

// Size implements Item.Size().
func (item *FloatItem) Size() int {
	return len(item.values)
}

// Values retrieves the float values stored in the item as a float64 slice.
//
// This method implements the Item.Values() interface. It returns a direct
// reference to the underlying float64 slice, allowing for potential modification.
//
// Caution: Modifying the returned slice will directly affect the data within the item.
// For immutable access, consider using `Clone()` to obtain a deep copy of the item first.
func (item *FloatItem) Values() any {
	return item.values
}

// SetValues sets the float64 values within the FloatItem.
//
// This method implements the Item.SetValues() interface. It accepts one or more values, each of which can be one of the following types:
//
//   - float32 or float64: A floating-point number.
//   - []float32 or []float64: A slice of floating-point numbers.
//   - string: A string that will be parsed as a floating-point number.
//   - []string: A slice of strings that will be parsed as floating-point numbers.
//   - int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64: An integer that will be converted to its floating-point representation.
//   - A slice of any of the above-mentioned integer types
//
// All provided values are combined into a single slice stored within the new item.
//
// If an error occurs during the combination process (e.g., due to incompatible types or values outside the representable range),
// the error is returned and also stored within the item for later retrieval.
func (item *FloatItem) SetValues(values ...any) error {
	var err error

	item.resetError()
	item.values, err = combineFloatValues(item.byteSize, values...)
	if err != nil {
		item.setError(err)
		return item.Error()
	}

	dataType := Float32Type
	if item.byteSize == 8 {
		dataType = Float64Type
	}

	dataBytes, _ := getDataByteLength(dataType, len(item.values))
	if dataBytes > MaxByteSize {
		item.setErrorMsg("item size limit exceeded")
		return item.Error()
	}

	return nil
}

// ToBytes serializes the FloatItem into a byte slice conforming to the SECS-II data format.
//
// This method implements the Item.ToBytes() interface.
//
// If an error occurs during header generation, an empty byte slice is returned,
// and the error is stored within the item for later retrieval.
func (item *FloatItem) ToBytes() []byte {
	itemSize := item.Size()
	result, _ := getHeaderBytes(fmt.Sprintf("f%d", item.byteSize), itemSize, itemSize*item.byteSize)

	if item.byteSize == 4 {
		for _, value := range item.values {
			bits := math.Float32bits(float32(value))
			result = append(result, byte(bits>>24))
			result = append(result, byte(bits>>16))
			result = append(result, byte(bits>>8))
			result = append(result, byte(bits))
		}
	} else {
		for _, value := range item.values {
			bits := math.Float64bits(value)
			result = append(result, byte(bits>>56))
			result = append(result, byte(bits>>48))
			result = append(result, byte(bits>>40))
			result = append(result, byte(bits>>32))
			result = append(result, byte(bits>>24))
			result = append(result, byte(bits>>16))
			result = append(result, byte(bits>>8))
			result = append(result, byte(bits))
		}
	}

	return result
}

// ToSML converts the FloatItem into its SML representation.
//
// This method implements the Item.ToSML() interface. It generates an SML string
// that represents the floating-point data stored in the item.
//
// The format is as follows:
//   - If the item's data is empty, it's represented as `<FbyteSize[0]>`.
//   - Otherwise:
//   - `<FbyteSize[size] value1 value2 ...>`
//   - `byteSize`: The size of each float in bytes (4 or 8).
//   - `size`: The number of floats in the list.
//   - `value1`, `value2`, ...: Each float is represented in decimal format.
func (item *FloatItem) ToSML() string {
	if item.Size() == 0 {
		return fmt.Sprintf("<F%d[0]>", item.byteSize)
	}

	var sb strings.Builder
	sb.Grow(len(item.values)*(item.byteSize*2+3) + 10)

	sb.WriteString(fmt.Sprintf("<F%d[%d] ", item.byteSize, item.Size()))

	// Use a buffer for FormatFloat to avoid allocations in the loop
	var buf [64]byte // Adjust size if needed for very large float values
	for i, v := range item.values {
		if i > 0 {
			sb.WriteByte(' ')
		}
		// sb.WriteString(strconv.FormatFloat(v, 'g', -1, item.byteSize*8))
		sb.Write(strconv.AppendFloat(buf[:0], v, 'g', -1, item.byteSize*8))
	}

	sb.WriteByte('>')
	return sb.String()
}

// Clone creates a deep copy of the FloatItem.
//
// This method implements the Item.Clone() interface. It returns a new
// FloatItem with a completely independent copy of the float data.
func (item *FloatItem) Clone() Item {
	clonedValues := util.CloneSlice(item.values, 0)
	return &FloatItem{byteSize: item.byteSize, values: clonedValues}
}

// Type returns "f4" or "f8" string depends on the byte size.
func (item *FloatItem) Type() string {
	switch item.byteSize {
	case 4:
		return Float32Type
	case 8:
		return Float64Type
	default:
		return ""
	}
}

// IsFloat32 returns true, indicating that FloatItem is a 32-bit float data item.
func (item *FloatItem) IsFloat32() bool { return item.byteSize == 4 }

// IsFloat64 returns true, indicating that FloatItem is a 64-bit float data item.
func (item *FloatItem) IsFloat64() bool { return item.byteSize == 8 }

func combineFloatValues(byteSize int, values ...any) ([]float64, error) { //nolint:gocyclo,cyclop
	itemValues := make([]float64, 0, len(values))
	for _, value := range values {
		switch value := value.(type) {
		case float32:
			itemValues = append(itemValues, float64(value))
		case float64:
			itemValues = append(itemValues, value)
		case []float32:
			itemValues = util.AppendFloat64Slice(itemValues, value)
		case []float64:
			itemValues = util.AppendFloat64Slice(itemValues, value)

		case int:
			itemValues = append(itemValues, float64(value))
		case []int:
			for _, v := range value {
				itemValues = append(itemValues, float64(v))
			}

		case int8:
			itemValues = append(itemValues, float64(value))
		case []int8:
			itemValues = util.AppendFloat64Slice(itemValues, value)

		case int16:
			itemValues = append(itemValues, float64(value))
		case []int16:
			itemValues = util.AppendFloat64Slice(itemValues, value)

		case int32:
			itemValues = append(itemValues, float64(value))
		case []int32:
			itemValues = util.AppendFloat64Slice(itemValues, value)

		case int64:
			itemValues = append(itemValues, float64(value))
		case []int64:
			for _, v := range value {
				itemValues = append(itemValues, float64(v))
			}

		case uint8:
			itemValues = append(itemValues, float64(value))
		case []uint8:
			itemValues = util.AppendFloat64Slice(itemValues, value)

		case uint16:
			itemValues = append(itemValues, float64(value))
		case []uint16:
			itemValues = util.AppendFloat64Slice(itemValues, value)

		case uint32:
			itemValues = append(itemValues, float64(value))
		case []uint32:
			itemValues = util.AppendFloat64Slice(itemValues, value)

		case uint:
			if value > 1<<53 {
				return nil, errors.New("value overflow")
			}
			itemValues = append(itemValues, float64(value))
		case []uint:
			for _, v := range value {
				if v > 1<<53 {
					return nil, errors.New("value overflow")
				}
				itemValues = append(itemValues, float64(v))
			}
		case uint64:
			if value > 1<<53 {
				return nil, errors.New("value overflow")
			}
			itemValues = append(itemValues, float64(value))
		case []uint64:
			for _, v := range value {
				if v > 1<<53 {
					return nil, errors.New("value overflow")
				}
				itemValues = append(itemValues, float64(v))
			}

		case string:
			floatVal, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return nil, err
			}

			itemValues = append(itemValues, floatVal)
		case []string:
			for _, v := range value {
				floatVal, err := strconv.ParseFloat(v, 64)
				if err != nil {
					return nil, err
				}
				itemValues = append(itemValues, floatVal)
			}

		default:
			return nil, errors.New("input argument contains invalid type for FloatItem")
		}
	}

	maxVal := math.MaxFloat64
	if byteSize == 4 {
		maxVal = math.MaxFloat32
	}

	for _, v := range itemValues {
		if math.IsInf(v, 0) || math.IsNaN(v) {
			return nil, errors.New("invalid float value")
		}

		if v < -maxVal || v > maxVal {
			return nil, errors.New("value overflow")
		}
	}

	return itemValues, nil
}
