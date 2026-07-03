package secs2

import (
	"encoding/binary"
	"errors"
	"fmt"
	"iter"
	"math"
	"slices"
	"strconv"
	"strings"
)

// IntItem represents an immutable list of signed integer values in a SECS-II message.
//
// It implements the Item interface. All methods are safe for concurrent use and no method
// exposes mutable internal storage.
type IntItem struct {
	baseItem
	byteSize int
	values   []int64
}

var _ Item = (*IntItem)(nil)

// NewIntItem creates a new IntItem representing signed integer data in a SECS-II message.
//
// byteSize must be 1, 2, 4, or 8. Each value can be a signed integer (int, int8, int16, int32,
// int64), an unsigned integer (uint, uint8, uint16, uint32, uint64), a slice of any of those
// types, or a string containing a decimal/hex/octal integer literal.
//
// Out-of-range values are clamped to the representable range for the given byteSize. If byteSize
// is invalid or a value cannot be converted (e.g., an unsupported type or non-numeric string), a
// deferred error is stored on the returned item; call Error() to inspect it.
func NewIntItem(byteSize int, values ...any) Item {
	item := &IntItem{byteSize: byteSize}

	if byteSize != 1 && byteSize != 2 && byteSize != 4 && byteSize != 8 {
		item.setErrorMsg("invalid byte size")

		return item
	}

	if err := item.combineIntValues(values...); err != nil {
		item.setError(err)

		return item
	}

	if n, _ := getDataByteLength(item.dataType(), len(item.values)); n > MaxByteSize {
		item.setErrorMsg("item size limit exceeded")
	}

	return item
}

// ToInt returns a fresh copy of the int64-widened values. Returns an error if the item carries a
// deferred construction error.
func (item *IntItem) ToInt() ([]int64, error) {
	if item.itemErr != nil {
		return nil, item.itemErr
	}

	return slices.Clone(item.values), nil
}

// IntAt returns the int64-widened value at index i. Returns an error if the item carries a
// deferred error or i is out of range.
func (item *IntItem) IntAt(i int) (int64, error) {
	if item.itemErr != nil {
		return 0, item.itemErr
	}

	if i < 0 || i >= len(item.values) {
		return 0, NewItemErrorWithMsg("index out of range")
	}

	return item.values[i], nil
}

// Ints returns an iterator over the int64-widened values. Yields nothing if the item carries a
// deferred error.
func (item *IntItem) Ints() iter.Seq[int64] {
	return func(yield func(int64) bool) {
		if item.itemErr != nil {
			return
		}

		for _, v := range item.values {
			if !yield(v) {
				return
			}
		}
	}
}

// Size returns the number of integer values in this item.
func (item *IntItem) Size() int { return len(item.values) }

// EncodedLen returns the total SECS-II wire byte length (header + payload).
// Returns 0 for items with deferred errors.
func (item *IntItem) EncodedLen() int {
	if item.itemErr != nil {
		return 0
	}

	if item.raw != nil {
		return len(item.raw)
	}

	n := len(item.values) * item.byteSize

	return headerLen(n) + n
}

// AppendTo appends the SECS-II wire encoding of this item into dst and returns the result.
// Returns dst unchanged for items with deferred errors.
func (item *IntItem) AppendTo(dst []byte) []byte {
	if item.itemErr != nil {
		return dst
	}

	if item.raw != nil {
		return append(dst, item.raw...)
	}

	dst, _ = appendHeaderBytes(dst, item.dataType(), len(item.values)) //nolint:errcheck

	switch item.byteSize {
	case 1:
		for _, v := range item.values {
			dst = append(dst, byte(v)) //nolint:gosec
		}
	case 2:
		for _, v := range item.values {
			dst = binary.BigEndian.AppendUint16(dst, uint16(v)) //nolint:gosec
		}
	case 4:
		for _, v := range item.values {
			dst = binary.BigEndian.AppendUint32(dst, uint32(v)) //nolint:gosec
		}
	case 8:
		for _, v := range item.values {
			dst = binary.BigEndian.AppendUint64(dst, uint64(v)) //nolint:gosec
		}
	default:
		// unreachable: byteSize validity is guarded by itemErr check at the top of AppendTo.
	}

	return dst
}

// ToBytes allocates a single buffer and returns the SECS-II wire encoding.
// Equivalent to AppendTo(make([]byte, 0, EncodedLen())).
func (item *IntItem) ToBytes() []byte {
	return item.AppendTo(make([]byte, 0, item.EncodedLen()))
}

// Type returns the type-string constant for this item: "i1", "i2", "i4", or "i8".
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

// IsInt8 returns true if this is an 8-bit signed integer item (byteSize == 1).
func (item *IntItem) IsInt8() bool { return item.byteSize == 1 }

// IsInt16 returns true if this is a 16-bit signed integer item (byteSize == 2).
func (item *IntItem) IsInt16() bool { return item.byteSize == 2 }

// IsInt32 returns true if this is a 32-bit signed integer item (byteSize == 4).
func (item *IntItem) IsInt32() bool { return item.byteSize == 4 }

// IsInt64 returns true if this is a 64-bit signed integer item (byteSize == 8).
func (item *IntItem) IsInt64() bool { return item.byteSize == 8 }

// ToSML returns the SML (SECS Message Language) text representation of this item.
func (item *IntItem) ToSML() string {
	if item.Size() == 0 {
		return fmt.Sprintf("<I%d[0]>", item.byteSize)
	}

	var sb strings.Builder

	sb.Grow(len(item.values)*10 + 10) //nolint:mnd

	fmt.Fprintf(&sb, "<I%d[%d] ", item.byteSize, item.Size())

	var intBuf [20]byte

	for i, v := range item.values {
		if i > 0 {
			sb.WriteByte(' ')
		}

		sb.Write(strconv.AppendInt(intBuf[:0], v, 10))
	}

	sb.WriteByte('>')

	return sb.String()
}

// dataType returns the SECS-II type-string for the current byteSize.
// Callers are responsible for ensuring byteSize is in {1, 2, 4, 8} before calling.
func (item *IntItem) dataType() string {
	dataTypeStr := [9]string{"", "i1", "i2", "", "i4", "", "", "", "i8"}

	return dataTypeStr[item.byteSize]
}

// combineIntValues converts the variadic values into int64 and appends them to item.values,
// clamping each value to the representable range for item.byteSize. The fast path handles the
// most-common types (int, []int, int64, []int64); all other types fall through to
// combineIntValuesSlow.
func (item *IntItem) combineIntValues(values ...any) error { //nolint:cyclop
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
		default:
			// Scalar or unknown type: capacity = 1 (already set above).
			_ = v
		}
	}

	item.values = make([]int64, 0, capacity)

	var maxVal, minVal int64

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

// combineIntValuesSlow handles the less-common integer types and string parsing.
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
		//nolint:gosec // maxVal is always non-negative; the comparison is safe
		if uint64(value) > uint64(maxVal) {
			item.values = append(item.values, maxVal)
		} else {
			//nolint:gosec // value ≤ maxVal, which is a valid int64
			item.values = append(item.values, int64(value))
		}
	case []uint:
		for _, v := range value {
			//nolint:gosec // maxVal is always non-negative; the comparison is safe
			if uint64(v) > uint64(maxVal) {
				item.values = append(item.values, maxVal)
			} else {
				//nolint:gosec // v ≤ maxVal, which is a valid int64
				item.values = append(item.values, int64(v))
			}
		}
	case uint64:
		//nolint:gosec // maxVal is always non-negative; the comparison is safe
		if value > uint64(maxVal) {
			item.values = append(item.values, maxVal)
		} else {
			//nolint:gosec // value ≤ maxVal, which is a valid int64
			item.values = append(item.values, int64(value))
		}
	case []uint64:
		for _, v := range value {
			//nolint:gosec // maxVal is always non-negative; the comparison is safe
			if v > uint64(maxVal) {
				item.values = append(item.values, maxVal)
			} else {
				//nolint:gosec // v ≤ maxVal, which is a valid int64
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

// clampInt64 returns v clamped to [minVal, maxVal].
func clampInt64(v, minVal, maxVal int64) int64 {
	if v < minVal {
		return minVal
	}

	if v > maxVal {
		return maxVal
	}

	return v
}
