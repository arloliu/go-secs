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

// FloatItem represents an immutable list of IEEE-754 floating-point values in a SECS-II message.
//
// It implements the Item interface. byteSize must be 4 (F4, single-precision) or 8 (F8,
// double-precision). All values are stored internally as float64 regardless of byteSize; F4 items
// apply float32 precision only during wire encoding and SML rendering. All methods are safe for
// concurrent use and no method exposes mutable internal storage.
type FloatItem struct {
	size     int32
	byteSize uint32
	scalar   float64
	baseItem
	values []float64
}

var _ Item = (*FloatItem)(nil)

// NewFloatItem creates a new FloatItem representing IEEE-754 floating-point data in a SECS-II
// message.
//
// byteSize must be 4 (F4, single-precision) or 8 (F8, double-precision). Each value can be a
// float32, float64, a signed or unsigned integer (int, int8, int16, int32, int64, uint, uint8,
// uint16, uint32, uint64), a slice of any of those types, or a string containing a decimal
// floating-point literal.
//
// For byteSize 4, float64 values whose magnitude exceeds math.MaxFloat32 are clamped to
// ±math.MaxFloat32. NaN and ±Inf pass through unclamped. Signed and unsigned integer values
// whose magnitude exceeds 2^53 produce a deferred error, as they cannot be represented exactly in
// float64.
//
// If byteSize is invalid or a value cannot be converted, a deferred error is stored on the
// returned item; call Error() to inspect it.
//
// Parameters:
//   - byteSize: the size in bytes of each float (4 or 8).
//   - values: the input arguments representing floating-point values.
//
// Returns:
//   - Item: the created FloatItem.
func NewFloatItem(byteSize int, values ...any) Item {
	item := &FloatItem{byteSize: uint32(byteSize)}

	if byteSize != 4 && byteSize != 8 {
		item.setErrorMsg("invalid byte size")

		return item
	}

	if err := item.combineFloatValues(values...); err != nil {
		item.setError(err)

		return item
	}

	item.size = int32(len(item.values))
	if item.size == 1 {
		item.scalar = item.values[0]
		item.values = nil
	}

	if n, _ := getDataByteLength(item.dataType(), int(item.size)); n > MaxByteSize {
		item.setErrorMsg("item size limit exceeded")
	}

	return item
}

// ToFloat returns a fresh copy of the float64-widened values.
//
// Returns an error if the item carries a deferred construction error.
//
// It returns the item's deferred error (see Error) when the item was constructed with
// one — a passing Is* predicate does NOT imply a nil error here. Always check the
// returned error; do not discard it via `v, _ := item.ToFloat()`.
func (item *FloatItem) ToFloat() ([]float64, error) {
	if item.itemErr != nil {
		return nil, item.itemErr
	}

	if item.size == 0 {
		return []float64{}, nil
	}
	if item.size == 1 {
		return []float64{item.scalar}, nil
	}

	return slices.Clone(item.values), nil
}

// FloatAt returns the value at index i.
//
// Returns an error if the item carries a deferred error or i is out of range.
func (item *FloatItem) FloatAt(i int) (float64, error) {
	if item.itemErr != nil {
		return 0, item.itemErr
	}

	if i < 0 || i >= int(item.size) {
		return 0, NewItemErrorWithMsg("index out of range")
	}

	if item.size == 1 {
		return item.scalar, nil
	}

	return item.values[i], nil
}

// Floats returns an iterator over the float64-widened values.
//
// Yields nothing if the item carries a deferred error.
func (item *FloatItem) Floats() iter.Seq[float64] {
	return func(yield func(float64) bool) {
		if item.itemErr != nil {
			return
		}

		if item.size == 1 {
			_ = yield(item.scalar)
			return
		}

		for _, v := range item.values {
			if !yield(v) {
				return
			}
		}
	}
}

// Size returns the number of floating-point values in this item.
func (item *FloatItem) Size() int { return int(item.size) }

// EncodedLen returns the total SECS-II wire byte length (header + payload).
// Returns 0 for items with deferred errors.
func (item *FloatItem) EncodedLen() int {
	if item.itemErr != nil {
		return 0
	}

	if item.rawPtr != nil {
		return item.rawLen
	}

	n := int(item.size) * int(item.byteSize)

	return headerLen(n) + n
}

// AppendTo appends the SECS-II wire encoding of this item into dst and returns the result.
// Returns dst unchanged for items with deferred errors.
func (item *FloatItem) AppendTo(dst []byte) []byte {
	if item.itemErr != nil {
		return dst
	}

	if item.rawPtr != nil {
		return append(dst, item.raw()...)
	}

	dst, _ = appendHeaderBytes(dst, item.dataType(), int(item.size)) //nolint:errcheck

	if item.size == 0 {
		return dst
	}

	if item.size == 1 {
		if item.byteSize == 4 {
			dst = binary.BigEndian.AppendUint32(dst, math.Float32bits(float32(item.scalar)))
		} else {
			dst = binary.BigEndian.AppendUint64(dst, math.Float64bits(item.scalar))
		}

		return dst
	}

	if item.byteSize == 4 {
		for _, v := range item.values {
			dst = binary.BigEndian.AppendUint32(dst, math.Float32bits(float32(v)))
		}
	} else {
		for _, v := range item.values {
			dst = binary.BigEndian.AppendUint64(dst, math.Float64bits(v))
		}
	}

	return dst
}

// ToBytes allocates a single buffer and returns the SECS-II wire encoding.
// Equivalent to AppendTo(make([]byte, 0, EncodedLen())).
func (item *FloatItem) ToBytes() []byte {
	return item.AppendTo(make([]byte, 0, item.EncodedLen()))
}

// Type returns the type-string constant for this item: "f4" or "f8".
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

// IsFloat32 returns true if this is a 32-bit float item (byteSize == 4).
//
// It reflects the DECLARED type only and does not consult the item's deferred error:
// a true result does not imply the item is usable. Gate value extraction on Error()
// (or the To* accessor's returned error), not on Is* alone.
func (item *FloatItem) IsFloat32() bool { return item.byteSize == 4 }

// IsFloat64 returns true if this is a 64-bit float item (byteSize == 8).
//
// It reflects the DECLARED type only and does not consult the item's deferred error:
// a true result does not imply the item is usable. Gate value extraction on Error()
// (or the To* accessor's returned error), not on Is* alone.
func (item *FloatItem) IsFloat64() bool { return item.byteSize == 8 }

// ToSML returns the SML (SECS Message Language) text representation of this item.
//
// Empty items are rendered as <FbyteSize[0]>. Values are formatted using G9 for F4 (matching
// float32 round-trip precision) and G17 for F8 (matching float64 round-trip precision).
func (item *FloatItem) ToSML() string {
	if item.size == 0 {
		return fmt.Sprintf("<F%d[0]>", item.byteSize)
	}

	var sb strings.Builder

	sb.Grow(int(item.size)*(int(item.byteSize)*2+3) + 10) //nolint:mnd

	fmt.Fprintf(&sb, "<F%d[%d] ", item.byteSize, item.size)

	prec := 9
	if item.byteSize == 8 {
		prec = 17
	}

	var buf [64]byte

	if item.size == 1 {
		sb.Write(strconv.AppendFloat(buf[:0], item.scalar, 'G', prec, int(item.byteSize)*8))
	} else {
		for i, v := range item.values {
			if i > 0 {
				sb.WriteByte(' ')
			}

			sb.Write(strconv.AppendFloat(buf[:0], v, 'G', prec, int(item.byteSize)*8))
		}
	}

	sb.WriteByte('>')

	return sb.String()
}

// dataType returns the SECS-II type-string for the current byteSize.
// Callers are responsible for ensuring byteSize is in {4, 8} before calling.
func (item *FloatItem) dataType() string {
	dataTypeStr := [9]string{"", "", "", "", "f4", "", "", "", "f8"}

	return dataTypeStr[item.byteSize]
}

// combineFloatValues converts the variadic values into float64 and appends them to item.values.
// The fast path handles float32, float64, []float32, and []float64. All other types fall through
// to combineFloatValuesSlow.
func (item *FloatItem) combineFloatValues(values ...any) error { //nolint:cyclop
	capacity := len(values)
	if len(values) == 1 {
		switch v := values[0].(type) {
		case []float32:
			capacity = len(v)
		case []float64:
			capacity = len(v)
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
			_ = v
		}
	}

	item.values = make([]float64, 0, capacity)

	checkOverflow := item.byteSize == 4

	for _, value := range values {
		switch value := value.(type) {
		case float32:
			item.values = append(item.values, float64(value))
		case float64:
			if checkOverflow {
				value = clampF4(value)
			}
			item.values = append(item.values, value)
		case []float32:
			for _, v := range value {
				item.values = append(item.values, float64(v))
			}
		case []float64:
			if checkOverflow {
				for _, v := range value {
					item.values = append(item.values, clampF4(v))
				}
			} else {
				item.values = append(item.values, value...)
			}
		default:
			if err := item.combineFloatValuesSlow(value); err != nil {
				return err
			}
		}
	}

	return nil
}

// combineFloatValuesSlow handles integer types and string parsing for combineFloatValues.
func (item *FloatItem) combineFloatValuesSlow(value any) error { //nolint:gocyclo,cyclop
	checkOverflow := item.byteSize == 4

	switch value := value.(type) {
	case int:
		if int64(value) > 1<<53 || int64(value) < -(1<<53) {
			return errors.New("value overflow")
		}
		item.values = append(item.values, float64(value))
	case []int:
		for _, v := range value {
			if int64(v) > 1<<53 || int64(v) < -(1<<53) {
				return errors.New("value overflow")
			}
			item.values = append(item.values, float64(v))
		}

	case int8:
		item.values = append(item.values, float64(value))
	case []int8:
		for _, v := range value {
			item.values = append(item.values, float64(v))
		}

	case int16:
		item.values = append(item.values, float64(value))
	case []int16:
		for _, v := range value {
			item.values = append(item.values, float64(v))
		}

	case int32:
		item.values = append(item.values, float64(value))
	case []int32:
		for _, v := range value {
			item.values = append(item.values, float64(v))
		}

	case int64:
		if value > 1<<53 || value < -(1<<53) {
			return errors.New("value overflow")
		}
		item.values = append(item.values, float64(value))
	case []int64:
		for _, v := range value {
			if v > 1<<53 || v < -(1<<53) {
				return errors.New("value overflow")
			}
			item.values = append(item.values, float64(v))
		}

	case uint8:
		item.values = append(item.values, float64(value))
	case []uint8:
		for _, v := range value {
			item.values = append(item.values, float64(v))
		}

	case uint16:
		item.values = append(item.values, float64(value))
	case []uint16:
		for _, v := range value {
			item.values = append(item.values, float64(v))
		}

	case uint32:
		item.values = append(item.values, float64(value))
	case []uint32:
		for _, v := range value {
			item.values = append(item.values, float64(v))
		}

	case uint:
		if uint64(value) > 1<<53 {
			return errors.New("value overflow")
		}
		item.values = append(item.values, float64(value))
	case []uint:
		for _, v := range value {
			if uint64(v) > 1<<53 {
				return errors.New("value overflow")
			}
			item.values = append(item.values, float64(v))
		}

	case uint64:
		if value > 1<<53 {
			return errors.New("value overflow")
		}
		item.values = append(item.values, float64(value))
	case []uint64:
		for _, v := range value {
			if v > 1<<53 {
				return errors.New("value overflow")
			}
			item.values = append(item.values, float64(v))
		}

	case string:
		floatVal, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return err
		}
		if checkOverflow {
			floatVal = clampF4(floatVal)
		}
		item.values = append(item.values, floatVal)
	case []string:
		for _, v := range value {
			floatVal, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return err
			}
			if checkOverflow {
				floatVal = clampF4(floatVal)
			}
			item.values = append(item.values, floatVal)
		}

	default:
		return errors.New("input argument contains invalid type for FloatItem")
	}

	return nil
}

// clampF4 clamps v to the representable float32 magnitude range [−MaxFloat32, +MaxFloat32].
// NaN and ±Inf pass through unchanged.
func clampF4(v float64) float64 {
	if math.IsInf(v, 0) || math.IsNaN(v) {
		return v
	}

	const maxVal = float64(math.MaxFloat32)
	if v > maxVal {
		return maxVal
	}
	if v < -maxVal {
		return -maxVal
	}

	return v
}
