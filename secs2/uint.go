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

// UintItem represents an immutable list of unsigned integer values in a SECS-II message.
//
// It implements the Item interface. All methods are safe for concurrent use and no method
// exposes mutable internal storage.
type UintItem struct {
	size     int32
	byteSize uint32
	scalar   uint64
	baseItem
	values []uint64
}

var _ Item = (*UintItem)(nil)

// NewUintItem creates a new UintItem representing unsigned integer data in a SECS-II message.
//
// byteSize must be 1, 2, 4, or 8. Each value can be an unsigned integer (uint, uint8, uint16,
// uint32, uint64), a slice of any of those types, a non-negative signed integer (int, int8,
// int16, int32, int64), a slice of any of those non-negative signed types, or a string
// containing a decimal/hex/octal unsigned integer literal.
//
// Out-of-range unsigned values are clamped to the representable maximum for the given byteSize.
// Negative signed integer values produce a deferred error. If byteSize is invalid or a value
// cannot be converted (e.g., an unsupported type or non-numeric string), a deferred error is
// stored on the returned item; call Error() to inspect it.
//
// Parameters:
//   - byteSize: the size in bytes of each unsigned integer (1, 2, 4, or 8).
//   - values: the input arguments representing unsigned integer values.
//
// Returns:
//   - Item: the created UintItem.
func NewUintItem(byteSize int, values ...any) Item {
	item := &UintItem{byteSize: uint32(byteSize)}

	if byteSize != 1 && byteSize != 2 && byteSize != 4 && byteSize != 8 {
		item.setErrorMsg("invalid byte size")

		return item
	}

	if err := item.combineUintValues(values...); err != nil {
		item.setError(err)

		return item
	}

	item.size = int32(len(item.values))
	if item.size == 1 {
		item.scalar = item.values[0]
		item.values = nil
	}

	if n := int(item.size) * int(item.byteSize); n > MaxByteSize {
		item.setErrorMsg("item size limit exceeded")
	}

	return item
}

// ToUint returns a fresh copy of the uint64-widened values.
//
// Returns an error if the item carries a deferred construction error.
//
// It returns the item's deferred error (see Error) when the item was constructed with
// one — a passing Is* predicate does NOT imply a nil error here. Always check the
// returned error; do not discard it via `v, _ := item.ToUint()`.
func (item *UintItem) ToUint() ([]uint64, error) {
	if item.itemErr != nil {
		return nil, item.itemErr
	}

	if item.size == 0 {
		return []uint64{}, nil
	}
	if item.size == 1 {
		return []uint64{item.scalar}, nil
	}

	return slices.Clone(item.values), nil
}

// UintAt returns the uint64-widened value at index i.
//
// Returns an error if the item carries a deferred error or i is out of range.
func (item *UintItem) UintAt(i int) (uint64, error) {
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

// Uints returns an iterator over the uint64-widened values.
//
// Yields nothing if the item carries a deferred error.
func (item *UintItem) Uints() iter.Seq[uint64] {
	return func(yield func(uint64) bool) {
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

// Size returns the number of unsigned integer values in this item.
func (item *UintItem) Size() int { return int(item.size) }

// EncodedLen returns the total SECS-II wire byte length (header + payload).
// Returns 0 for items with deferred errors.
func (item *UintItem) EncodedLen() int {
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
func (item *UintItem) AppendTo(dst []byte) []byte {
	if item.itemErr != nil {
		return dst
	}

	if item.rawPtr != nil {
		return append(dst, item.raw()...)
	}

	fc, ok := item.formatCode()
	if !ok {
		return dst
	}

	dst, _ = appendHeaderBytesFC(dst, fc, int(item.size)*int(item.byteSize)) //nolint:errcheck

	if item.size == 0 {
		return dst
	}

	if item.size == 1 {
		switch item.byteSize {
		case 1:
			dst = append(dst, byte(item.scalar)) //nolint:gosec // truncation to the item's declared byte width is intentional
		case 2:
			dst = binary.BigEndian.AppendUint16(dst, uint16(item.scalar)) //nolint:gosec // truncation to the item's declared byte width is intentional
		case 4:
			dst = binary.BigEndian.AppendUint32(dst, uint32(item.scalar)) //nolint:gosec // truncation to the item's declared byte width is intentional
		case 8:
			dst = binary.BigEndian.AppendUint64(dst, item.scalar)
		default:
			// unreachable
		}

		return dst
	}

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
			dst = binary.BigEndian.AppendUint64(dst, v)
		}
	default:
		// unreachable: byteSize validity is guarded by itemErr check at the top of AppendTo.
	}

	return dst
}

// ToBytes allocates a single buffer and returns the SECS-II wire encoding.
// Equivalent to AppendTo(make([]byte, 0, EncodedLen())).
func (item *UintItem) ToBytes() []byte {
	return item.AppendTo(make([]byte, 0, item.EncodedLen()))
}

// Type returns the type-string constant for this item: "u1", "u2", "u4", or "u8".
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

// IsUint8 returns true if this is an 8-bit unsigned integer item (byteSize == 1).
//
// It reflects the DECLARED type only and does not consult the item's deferred error:
// a true result does not imply the item is usable. Gate value extraction on Error()
// (or the To* accessor's returned error), not on Is* alone.
func (item *UintItem) IsUint8() bool { return item.byteSize == 1 }

// IsUint16 returns true if this is a 16-bit unsigned integer item (byteSize == 2).
//
// It reflects the DECLARED type only and does not consult the item's deferred error:
// a true result does not imply the item is usable. Gate value extraction on Error()
// (or the To* accessor's returned error), not on Is* alone.
func (item *UintItem) IsUint16() bool { return item.byteSize == 2 }

// IsUint32 returns true if this is a 32-bit unsigned integer item (byteSize == 4).
//
// It reflects the DECLARED type only and does not consult the item's deferred error:
// a true result does not imply the item is usable. Gate value extraction on Error()
// (or the To* accessor's returned error), not on Is* alone.
func (item *UintItem) IsUint32() bool { return item.byteSize == 4 }

// IsUint64 returns true if this is a 64-bit unsigned integer item (byteSize == 8).
//
// It reflects the DECLARED type only and does not consult the item's deferred error:
// a true result does not imply the item is usable. Gate value extraction on Error()
// (or the To* accessor's returned error), not on Is* alone.
func (item *UintItem) IsUint64() bool { return item.byteSize == 8 }

// ToSML returns the SML (SECS Message Language) text representation of this item.
func (item *UintItem) ToSML() string {
	if item.size == 0 {
		return fmt.Sprintf("<U%d[0]>", item.byteSize)
	}

	var sb strings.Builder

	sb.Grow(int(item.size)*10 + 10) //nolint:mnd

	fmt.Fprintf(&sb, "<U%d[%d] ", item.byteSize, item.size)

	var uintBuf [20]byte

	if item.size == 1 {
		sb.Write(strconv.AppendUint(uintBuf[:0], item.scalar, 10))
	} else {
		for i, v := range item.values {
			if i > 0 {
				sb.WriteByte(' ')
			}

			sb.Write(strconv.AppendUint(uintBuf[:0], v, 10))
		}
	}

	sb.WriteByte('>')

	return sb.String()
}

// formatCode returns the SECS-II format code for the item's byteSize, and false if byteSize is
// outside the valid set {1, 2, 4, 8}. A zero-value UintItem{} (byteSize 0, as can be constructed
// directly since UintItem is exported) is the primary case this guards: FormatCode has no invalid
// sentinel and the zero FormatCode is the valid List code, so callers must check ok before using
// the returned code.
func (item *UintItem) formatCode() (FormatCode, bool) {
	switch item.byteSize {
	case 1:
		return Uint8FormatCode, true
	case 2:
		return Uint16FormatCode, true
	case 4:
		return Uint32FormatCode, true
	case 8:
		return Uint64FormatCode, true
	default:
		return 0, false
	}
}

// combineUintValues converts the variadic values into uint64 and appends them to item.values,
// clamping each unsigned value to the representable maximum for item.byteSize. Negative signed
// integer values produce an error. The fast path handles the most-common types (uint, []uint,
// uint64, []uint64); all other types fall through to combineUintValuesSlow.
func (item *UintItem) combineUintValues(values ...any) error { //nolint:cyclop
	capacity := len(values)
	if len(values) == 1 {
		switch v := values[0].(type) {
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
		case []string:
			capacity = len(v)
		default:
			// Scalar or unknown type: capacity = 1 (already set above).
			_ = v
		}
	}

	item.values = make([]uint64, 0, capacity)

	var maxVal uint64
	if item.byteSize == 8 {
		maxVal = math.MaxUint64
	} else {
		maxVal = (1 << (item.byteSize * 8)) - 1
	}

	for _, value := range values {
		switch value := value.(type) {
		case uint:
			item.values = append(item.values, clampUint64(uint64(value), maxVal))
		case []uint:
			for _, v := range value {
				item.values = append(item.values, clampUint64(uint64(v), maxVal))
			}
		case uint64:
			item.values = append(item.values, clampUint64(value, maxVal))
		case []uint64:
			for _, v := range value {
				item.values = append(item.values, clampUint64(v, maxVal))
			}
		default:
			if err := item.combineUintValuesSlow(value, maxVal); err != nil {
				return err
			}
		}
	}

	return nil
}

// combineUintValuesSlow handles the less-common unsigned types, non-negative signed integer
// types, and string parsing.
func (item *UintItem) combineUintValuesSlow(value any, maxVal uint64) error { //nolint:gocyclo,cyclop
	switch value := value.(type) {
	case uint8:
		item.values = append(item.values, clampUint64(uint64(value), maxVal))
	case []uint8:
		for _, v := range value {
			item.values = append(item.values, clampUint64(uint64(v), maxVal))
		}
	case uint16:
		item.values = append(item.values, clampUint64(uint64(value), maxVal))
	case []uint16:
		for _, v := range value {
			item.values = append(item.values, clampUint64(uint64(v), maxVal))
		}
	case uint32:
		item.values = append(item.values, clampUint64(uint64(value), maxVal))
	case []uint32:
		for _, v := range value {
			item.values = append(item.values, clampUint64(uint64(v), maxVal))
		}
	case int:
		if value < 0 {
			return errors.New("negative value not allowed for UintItem")
		}

		item.values = append(item.values, clampUint64(uint64(value), maxVal)) //nolint:gosec
	case []int:
		for _, v := range value {
			if v < 0 {
				return errors.New("negative value not allowed for UintItem")
			}

			item.values = append(item.values, clampUint64(uint64(v), maxVal)) //nolint:gosec
		}
	case int8:
		if value < 0 {
			return errors.New("negative value not allowed for UintItem")
		}

		item.values = append(item.values, clampUint64(uint64(value), maxVal)) //nolint:gosec
	case []int8:
		for _, v := range value {
			if v < 0 {
				return errors.New("negative value not allowed for UintItem")
			}

			item.values = append(item.values, clampUint64(uint64(v), maxVal)) //nolint:gosec
		}
	case int16:
		if value < 0 {
			return errors.New("negative value not allowed for UintItem")
		}

		item.values = append(item.values, clampUint64(uint64(value), maxVal)) //nolint:gosec
	case []int16:
		for _, v := range value {
			if v < 0 {
				return errors.New("negative value not allowed for UintItem")
			}

			item.values = append(item.values, clampUint64(uint64(v), maxVal)) //nolint:gosec
		}
	case int32:
		if value < 0 {
			return errors.New("negative value not allowed for UintItem")
		}

		item.values = append(item.values, clampUint64(uint64(value), maxVal)) //nolint:gosec
	case []int32:
		for _, v := range value {
			if v < 0 {
				return errors.New("negative value not allowed for UintItem")
			}

			item.values = append(item.values, clampUint64(uint64(v), maxVal)) //nolint:gosec
		}
	case int64:
		if value < 0 {
			return errors.New("negative value not allowed for UintItem")
		}

		item.values = append(item.values, clampUint64(uint64(value), maxVal)) //nolint:gosec
	case []int64:
		for _, v := range value {
			if v < 0 {
				return errors.New("negative value not allowed for UintItem")
			}

			item.values = append(item.values, clampUint64(uint64(v), maxVal)) //nolint:gosec
		}
	case string:
		uintVal, err := strconv.ParseUint(value, 0, 64)
		if err != nil {
			var numErr *strconv.NumError
			if !errors.As(err, &numErr) || !errors.Is(numErr.Err, strconv.ErrRange) {
				return err
			}
		}

		item.values = append(item.values, clampUint64(uintVal, maxVal))
	case []string:
		for _, v := range value {
			uintVal, err := strconv.ParseUint(v, 0, 64)
			if err != nil {
				var numErr *strconv.NumError
				if !errors.As(err, &numErr) || !errors.Is(numErr.Err, strconv.ErrRange) {
					return err
				}
			}

			item.values = append(item.values, clampUint64(uintVal, maxVal))
		}
	default:
		return errors.New("input argument contains invalid type for UintItem")
	}

	return nil
}

// clampUint64 returns v clamped to [0, maxVal].
func clampUint64(v, maxVal uint64) uint64 {
	if v > maxVal {
		return maxVal
	}

	return v
}
