package secs2

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/arloliu/go-secs/v2/internal/framecodec"
)

// MaxListDepth caps nested-list recursion to guard against stack exhaustion on malicious input.
const MaxListDepth = 64

// Decode parses a single SECS-II data item from its wire bytes (SEMI E5 §9).
//
// It is the inverse of Item.AppendTo/ToBytes. Empty input yields NewEmptyItem().
// Returns an error on malformed input (bad format code, truncated length/payload,
// or nesting deeper than MaxListDepth).
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

// DecodeOwned is an internal-transport-only entry point.
//
// It parses a single SECS-II item from a body the library already owns, WITHOUT copying
// (no bytes.Clone): the returned item tree's leaf raw fields alias body.Bytes(), so the
// caller MUST keep that buffer alive for as long as the item is retained.
//
// Its argument is a capability token whose type lives in an internal package, so external code
// cannot construct one and therefore cannot call this function at all. External callers must use
// Decode, which copies its input and is safe with caller-owned buffers. This no-copy path exists
// solely for the in-repo transport layer that frames SECS-II message bodies; secs2.Decode([]byte)
// stays copy-only.
//
// Parameters:
//   - body: the capability token wrapping the owned SECS-II wire bytes.
//
// Returns:
//   - Item: the parsed SECS-II data item.
//   - error: syntax or boundary error if parsing fails.
func DecodeOwned(body framecodec.OwnedSECS2Body) (Item, error) {
	raw := body.Bytes()
	if len(raw) == 0 {
		return NewEmptyItem(), nil
	}

	item, _, err := decodeItem(raw, 0, 0)

	return item, err
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

		it := &ListItem{values: children}
		it.raw = owned[startPos:pos]

		return it, pos, nil

	case ASCIIFormatCode:
		if pos+length > len(owned) {
			return nil, pos, fmt.Errorf("unexpected end of data: ASCII needs %d bytes, have %d", length, len(owned)-pos)
		}

		s := string(owned[pos : pos+length])
		pos += length
		it := &ASCIIItem{value: s}
		it.raw = owned[startPos:pos]

		return it, pos, nil

	case JIS8FormatCode:
		if pos+length > len(owned) {
			return nil, pos, fmt.Errorf("unexpected end of data: JIS8 needs %d bytes, have %d", length, len(owned)-pos)
		}

		s := string(owned[pos : pos+length])
		pos += length
		it := &JIS8Item{value: s}
		it.raw = owned[startPos:pos]

		return it, pos, nil

	case BinaryFormatCode:
		if pos+length > len(owned) {
			return nil, pos, fmt.Errorf("unexpected end of data: binary needs %d bytes, have %d", length, len(owned)-pos)
		}

		payload := owned[pos : pos+length]
		pos += length
		it := &BinaryItem{values: payload}
		it.raw = owned[startPos:pos]

		return it, pos, nil

	case BooleanFormatCode:
		if pos+length > len(owned) {
			return nil, pos, fmt.Errorf("unexpected end of data: boolean needs %d bytes, have %d", length, len(owned)-pos)
		}

		bools := make([]bool, length)

		for i := range length {
			bools[i] = owned[pos+i] != 0
		}

		pos += length
		it := &BooleanItem{values: bools}
		it.raw = owned[startPos:pos]

		return it, pos, nil

	case LocalizedStrFormatCode:
		if length < 2 {
			return nil, pos, fmt.Errorf("localized string payload too short: %d bytes (minimum 2 for LSH)", length)
		}

		if pos+length > len(owned) {
			return nil, pos, fmt.Errorf("unexpected end of data: localized string needs %d bytes, have %d", length, len(owned)-pos)
		}

		lsh := uint16(owned[pos])<<8 | uint16(owned[pos+1])
		s := string(owned[pos+2 : pos+length])
		pos += length
		it := &LocalizedStrItem{lsh: lsh, value: s}
		it.raw = owned[startPos:pos]

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
	it := &IntItem{byteSize: byteSize, values: vals}
	it.raw = owned[startPos:pos]

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
	it := &UintItem{byteSize: byteSize, values: vals}
	it.raw = owned[startPos:pos]

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
	it := &FloatItem{byteSize: byteSize, values: vals}
	it.raw = owned[startPos:pos]

	return it, pos, nil
}
