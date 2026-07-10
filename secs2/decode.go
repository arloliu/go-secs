package secs2

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"unsafe"

	"github.com/arloliu/go-secs/v2/internal/framecodec"
)

// MaxListDepth caps nested-list recursion to guard against stack exhaustion on malicious input.
const MaxListDepth = 64

// Decode parses a single SECS-II data item from its wire bytes (SEMI E5 §9).
//
// It is the inverse of Item.AppendTo/ToBytes. Empty input yields NewEmptyItem().
// Returns an error on malformed input (bad format code, truncated length/payload,
// or nesting deeper than MaxListDepth). Decode copies data, so the returned Item
// has no lifetime dependency on the caller's buffer; see [DecodeOwned] for a
// zero-copy variant when the caller already owns data outright.
//
// Parameters:
//   - data: the wire bytes to parse.
//
// Returns:
//   - Item: the parsed SECS-II data item.
//   - error: syntax or boundary error if parsing fails.
func Decode(data []byte) (Item, error) {
	if len(data) == 0 {
		return NewEmptyItem(), nil
	}

	owned := bytes.Clone(data)
	item, _, err := decodeItem(owned, 0, 0)

	return item, err
}

// DecodeOwned parses a single SECS-II data item from data, skipping the whole-input
// defensive copy that [Decode] always performs.
//
// Calling DecodeOwned transfers ownership of data's backing array to the returned Item, so
// the caller MUST NOT mutate or reuse data after this call, and must keep it alive for as
// long as the item is retained — the same ownership-transfer idiom as
// [encoding/json.RawMessage]. Binary, ASCII, JIS-8, and localized-string items alias data
// directly (no further copy); numeric and boolean items always build a typed value slice,
// since their values are a different representation than the wire bytes. Use this only for
// a buffer the caller already owns outright (e.g. a freshly-read file or a buffer with no
// other referents); otherwise use [Decode], which copies its input and is always safe.
//
// Parameters:
//   - data: the wire bytes to parse; ownership transfers to the returned Item on success.
//
// Returns:
//   - Item: the parsed SECS-II data item, aliasing data.
//   - error: syntax or boundary error if parsing fails.
func DecodeOwned(data []byte) (Item, error) {
	if len(data) == 0 {
		return NewEmptyItem(), nil
	}

	item, _, err := decodeItem(data, 0, 0)

	return item, err
}

// DecodeOwnedFrame is an internal-transport-only entry point.
//
// It parses a single SECS-II item from a body the library already owns, WITHOUT copying
// (no bytes.Clone): the returned item tree's leaf raw fields alias body.Bytes(), so the
// caller MUST keep that buffer alive for as long as the item is retained.
//
// Its argument is a capability token whose type lives in an internal package, so external code
// cannot construct one and therefore cannot call this function at all. External callers must use
// Decode (always copies) or DecodeOwned (zero-copy over a caller-owned []byte). This path exists
// solely for the in-repo transport layer that frames SECS-II message bodies.
//
// Parameters:
//   - body: the capability token wrapping the owned SECS-II wire bytes.
//
// Returns:
//   - Item: the parsed SECS-II data item.
//   - error: syntax or boundary error if parsing fails.
func DecodeOwnedFrame(body framecodec.OwnedSECS2Body) (Item, error) {
	return DecodeOwned(body.Bytes())
}

// ownedString reinterprets b as a string without copying, via unsafe.String. It exists only
// for decodeItem's ASCII/JIS8/LocalizedStr leaves, where b is always a sub-slice of the
// exclusively-owned buffer Decode or DecodeOwned handed to decodeItem: Decode's bytes.Clone is
// never referenced by anything outside this call tree, and DecodeOwned's caller has
// contractually transferred ownership and promised not to mutate or reuse it. decodeItem never
// retains or hands out a second, independent reference to b — the string this returns is the
// ONLY view of that memory the returned Item exposes — so no public accessor can observe a
// mutation through it as long as those two contracts hold. b must not be mutated for as long
// as the returned string is reachable.
func ownedString(b []byte) string {
	if len(b) == 0 {
		return ""
	}

	return unsafe.String(unsafe.SliceData(b), len(b))
}

// decodeItem parses one SECS-II item from owned[pos:] and returns the item, the new byte
// position, and any error. owned is the buffer cloned from the caller's input at Decode entry;
// each decoded item's raw field is set to a sub-slice of owned.
func decodeItem(owned []byte, pos, depth int) (Item, int, error) { //nolint:cyclop,gocyclo
	startPos := pos

	if pos >= len(owned) {
		return nil, pos, errors.New("unexpected end of data: need format byte")
	}

	formatByte := owned[pos]
	pos++

	formatCode := FormatCode(formatByte >> 2)
	lenByteCount := int(formatByte & 0x03)

	if lenByteCount == 0 {
		return nil, pos, errors.New("invalid item header: length-byte count is zero")
	}

	if pos+lenByteCount > len(owned) {
		return nil, pos, fmt.Errorf("unexpected end of data: need %d length bytes, have %d", lenByteCount, len(owned)-pos)
	}

	var length int

	switch lenByteCount {
	case 1:
		length = int(owned[pos])
	case 2:
		length = int(owned[pos])<<8 | int(owned[pos+1])
	case 3:
		length = int(owned[pos])<<16 | int(owned[pos+1])<<8 | int(owned[pos+2])
	default:
		// unreachable: lenByteCount is 1..3 after the zero check above.
	}

	pos += lenByteCount

	switch formatCode { //nolint:exhaustive
	case ListFormatCode:
		depth++
		if depth > MaxListDepth {
			return nil, pos, fmt.Errorf("list nesting depth exceeds maximum allowed: %d", MaxListDepth)
		}

		if length*2 > len(owned)-pos {
			return nil, pos, fmt.Errorf("list child count %d exceeds remaining bytes: need at least %d, have %d", length, length*2, len(owned)-pos)
		}

		children := make([]Item, 0, length)

		for range length {
			child, newPos, err := decodeItem(owned, pos, depth)
			if err != nil {
				return nil, pos, err
			}

			pos = newPos
			children = append(children, child)
		}

		it := &ListItem{values: children, clean: true}
		it.setRaw(owned[startPos:pos])

		return it, pos, nil

	case ASCIIFormatCode:
		if pos+length > len(owned) {
			return nil, pos, fmt.Errorf("unexpected end of data: ASCII needs %d bytes, have %d", length, len(owned)-pos)
		}

		s := ownedString(owned[pos : pos+length])
		pos += length
		it := &ASCIIItem{value: s}
		it.setRaw(owned[startPos:pos])

		return it, pos, nil

	case JIS8FormatCode:
		if pos+length > len(owned) {
			return nil, pos, fmt.Errorf("unexpected end of data: JIS8 needs %d bytes, have %d", length, len(owned)-pos)
		}

		s := ownedString(owned[pos : pos+length])
		pos += length
		it := &JIS8Item{value: s}
		it.setRaw(owned[startPos:pos])

		return it, pos, nil

	case BinaryFormatCode:
		if pos+length > len(owned) {
			return nil, pos, fmt.Errorf("unexpected end of data: binary needs %d bytes, have %d", length, len(owned)-pos)
		}

		payload := owned[pos : pos+length]
		pos += length
		it := &BinaryItem{values: payload}
		it.setRaw(owned[startPos:pos])

		return it, pos, nil

	case BooleanFormatCode:
		if pos+length > len(owned) {
			return nil, pos, fmt.Errorf("unexpected end of data: boolean needs %d bytes, have %d", length, len(owned)-pos)
		}

		if length == 1 {
			val := owned[pos] != 0
			pos++
			it := &BooleanItem{size: 1, scalar: val}
			it.setRaw(owned[startPos:pos])

			return it, pos, nil
		}

		bools := make([]bool, length)

		for i := range length {
			bools[i] = owned[pos+i] != 0
		}

		pos += length
		it := &BooleanItem{size: int32(length), values: bools}
		it.setRaw(owned[startPos:pos])

		return it, pos, nil

	case LocalizedStrFormatCode:
		if length < 2 {
			return nil, pos, fmt.Errorf("localized string payload too short: %d bytes (minimum 2 for LSH)", length)
		}

		if pos+length > len(owned) {
			return nil, pos, fmt.Errorf("unexpected end of data: localized string needs %d bytes, have %d", length, len(owned)-pos)
		}

		lsh := uint16(owned[pos])<<8 | uint16(owned[pos+1])
		s := ownedString(owned[pos+2 : pos+length])
		pos += length
		it := &LocalizedStrItem{lsh: lsh, value: s}
		it.setRaw(owned[startPos:pos])

		return it, pos, nil

	case Int8FormatCode:
		return decodeIntItem(owned, startPos, pos, 1, length)
	case Int16FormatCode:
		return decodeIntItem(owned, startPos, pos, 2, length)
	case Int32FormatCode:
		return decodeIntItem(owned, startPos, pos, 4, length)
	case Int64FormatCode:
		return decodeIntItem(owned, startPos, pos, 8, length)

	case Uint8FormatCode:
		return decodeUintItem(owned, startPos, pos, 1, length)
	case Uint16FormatCode:
		return decodeUintItem(owned, startPos, pos, 2, length)
	case Uint32FormatCode:
		return decodeUintItem(owned, startPos, pos, 4, length)
	case Uint64FormatCode:
		return decodeUintItem(owned, startPos, pos, 8, length)

	case Float32FormatCode:
		return decodeFloatItem(owned, startPos, pos, 4, length)
	case Float64FormatCode:
		return decodeFloatItem(owned, startPos, pos, 8, length)

	default:
		return nil, pos, fmt.Errorf("unknown format code: %d", formatCode)
	}
}

// decodeIntItem parses byteSize-wide signed integers from owned[pos:pos+length], builds
// an IntItem with the result, and sets its raw field to owned[startPos:pos+length].
func decodeIntItem(owned []byte, startPos, pos, byteSize, length int) (Item, int, error) {
	if length%byteSize != 0 {
		return nil, pos, fmt.Errorf("invalid payload length %d for I%d item: not a multiple of %d", length, byteSize*8, byteSize)
	}

	if pos+length > len(owned) {
		return nil, pos, fmt.Errorf("unexpected end of data: I%d needs %d bytes, have %d", byteSize*8, length, len(owned)-pos)
	}

	count := length / byteSize
	if count == 1 {
		var val int64
		switch byteSize {
		case 1:
			val = int64(int8(owned[pos]))
		case 2:
			val = int64(int16(binary.BigEndian.Uint16(owned[pos:]))) //nolint:gosec // sign-extending a fixed-width wire value into int64 is intentional
		case 4:
			val = int64(int32(binary.BigEndian.Uint32(owned[pos:]))) //nolint:gosec // sign-extending a fixed-width wire value into int64 is intentional
		case 8:
			val = int64(binary.BigEndian.Uint64(owned[pos:])) //nolint:gosec // reinterpreting the wire's 8 bytes as int64 is intentional
		default:
			// unreachable: byteSize is 1, 2, 4, or 8.
		}
		pos += length
		it := &IntItem{byteSize: uint32(byteSize), size: 1, scalar: val}
		it.setRaw(owned[startPos:pos])

		return it, pos, nil
	}

	vals := make([]int64, count)

	for i := range count {
		start := pos + i*byteSize

		switch byteSize {
		case 1:
			vals[i] = int64(int8(owned[start]))
		case 2:
			vals[i] = int64(int16(binary.BigEndian.Uint16(owned[start:]))) //nolint:gosec
		case 4:
			vals[i] = int64(int32(binary.BigEndian.Uint32(owned[start:]))) //nolint:gosec
		case 8:
			vals[i] = int64(binary.BigEndian.Uint64(owned[start:])) //nolint:gosec
		default:
			// unreachable: byteSize is 1, 2, 4, or 8 by construction.
		}
	}

	pos += length
	it := &IntItem{byteSize: uint32(byteSize), size: int32(count), values: vals}
	it.setRaw(owned[startPos:pos])

	return it, pos, nil
}

// decodeUintItem parses byteSize-wide unsigned integers from owned[pos:pos+length], builds
// a UintItem with the result, and sets its raw field to owned[startPos:pos+length].
func decodeUintItem(owned []byte, startPos, pos, byteSize, length int) (Item, int, error) { //nolint:dupl
	if length%byteSize != 0 {
		return nil, pos, fmt.Errorf("invalid payload length %d for U%d item: not a multiple of %d", length, byteSize*8, byteSize)
	}

	if pos+length > len(owned) {
		return nil, pos, fmt.Errorf("unexpected end of data: U%d needs %d bytes, have %d", byteSize*8, length, len(owned)-pos)
	}

	count := length / byteSize
	if count == 1 {
		var val uint64
		switch byteSize {
		case 1:
			val = uint64(owned[pos])
		case 2:
			val = uint64(binary.BigEndian.Uint16(owned[pos:]))
		case 4:
			val = uint64(binary.BigEndian.Uint32(owned[pos:]))
		case 8:
			val = binary.BigEndian.Uint64(owned[pos:])
		default:
			// unreachable: byteSize is 1, 2, 4, or 8.
		}
		pos += length
		it := &UintItem{byteSize: uint32(byteSize), size: 1, scalar: val}
		it.setRaw(owned[startPos:pos])

		return it, pos, nil
	}

	vals := make([]uint64, count)

	for i := range count {
		start := pos + i*byteSize

		switch byteSize {
		case 1:
			vals[i] = uint64(owned[start])
		case 2:
			vals[i] = uint64(binary.BigEndian.Uint16(owned[start:]))
		case 4:
			vals[i] = uint64(binary.BigEndian.Uint32(owned[start:]))
		case 8:
			vals[i] = binary.BigEndian.Uint64(owned[start:])
		default:
			// unreachable: byteSize is 1, 2, 4, or 8 by construction.
		}
	}

	pos += length
	it := &UintItem{byteSize: uint32(byteSize), size: int32(count), values: vals}
	it.setRaw(owned[startPos:pos])

	return it, pos, nil
}

// decodeFloatItem parses byteSize-wide IEEE-754 floats from owned[pos:pos+length], builds
// a FloatItem with the result, and sets its raw field to owned[startPos:pos+length].
func decodeFloatItem(owned []byte, startPos, pos, byteSize, length int) (Item, int, error) {
	if length%byteSize != 0 {
		return nil, pos, fmt.Errorf("invalid payload length %d for F%d item: not a multiple of %d", length, byteSize*8, byteSize)
	}

	if pos+length > len(owned) {
		return nil, pos, fmt.Errorf("unexpected end of data: F%d needs %d bytes, have %d", byteSize*8, length, len(owned)-pos)
	}

	count := length / byteSize
	if count == 1 {
		var val float64
		if byteSize == 4 {
			val = float64(math.Float32frombits(binary.BigEndian.Uint32(owned[pos:])))
		} else {
			val = math.Float64frombits(binary.BigEndian.Uint64(owned[pos:]))
		}
		pos += length
		it := &FloatItem{byteSize: uint32(byteSize), size: 1, scalar: val}
		it.setRaw(owned[startPos:pos])

		return it, pos, nil
	}

	vals := make([]float64, count)

	for i := range count {
		start := pos + i*byteSize

		if byteSize == 4 {
			vals[i] = float64(math.Float32frombits(binary.BigEndian.Uint32(owned[start:])))
		} else {
			vals[i] = math.Float64frombits(binary.BigEndian.Uint64(owned[start:]))
		}
	}

	pos += length
	it := &FloatItem{byteSize: uint32(byteSize), size: int32(count), values: vals}
	it.setRaw(owned[startPos:pos])

	return it, pos, nil
}
