package tlv

import (
	"encoding/binary"
	"strconv"
)

// MaxU64 is the largest u64 value the format allows (the tracepack format specification §2).
const MaxU64 = 1<<63 - 1

// Value types of a TLV entry, from the value-type table of the tracepack format specification §5.
const (
	TypeU8    ValueType = 1 // u8, 1 byte: enums and small integers
	TypeBool  ValueType = 2 // bool, 1 byte: 0 or 1
	TypeI64   ValueType = 3 // i64, 8 bytes, little-endian two's complement
	TypeU64   ValueType = 4 // u64, 8 bytes, little-endian, at most 2^63 − 1
	TypeUUID  ValueType = 5 // uuid, 16 bytes in RFC 9562 byte order
	TypeUTF8  ValueType = 6 // utf8, any length, valid UTF-8 without BOM
	TypeBytes ValueType = 7 // bytes, any length
	TypeTLV   ValueType = 8 // tlv, any length: nested entries
)

// ValueType is the u8 value_type field of a TLV entry.
//
// A value outside 1–8 is not an error by itself:
// the entry stays opaque and is preserved,
// and only a registry that knows the tag can reject it.
type ValueType uint8

type valueTypeInfo struct {
	name     string
	fixedLen int // 0 for a variable-length type
}

// U8Entry returns a u8 entry.
func U8Entry(tag uint16, v uint8) Entry {
	return Entry{Tag: tag, Type: TypeU8, Value: []byte{v}}
}

// BoolEntry returns a bool entry holding 1 for true and 0 for false.
func BoolEntry(tag uint16, v bool) Entry {
	var b byte
	if v {
		b = 1
	}

	return Entry{Tag: tag, Type: TypeBool, Value: []byte{b}}
}

// I64Entry returns an i64 entry.
func I64Entry(tag uint16, v int64) Entry {
	return Entry{Tag: tag, Type: TypeI64, Value: binary.LittleEndian.AppendUint64(nil, uint64(v))}
}

// U64Entry returns a u64 entry.
// The value is encoded as given;
// a writer must keep it at or below MaxU64, because a reader rejects anything larger.
func U64Entry(tag uint16, v uint64) Entry {
	return Entry{Tag: tag, Type: TypeU64, Value: binary.LittleEndian.AppendUint64(nil, v)}
}

// UUIDEntry returns a uuid entry; id is in RFC 9562 byte order.
func UUIDEntry(tag uint16, id [16]byte) Entry {
	return Entry{Tag: tag, Type: TypeUUID, Value: id[:]}
}

// UTF8Entry returns a utf8 entry.
// The string is encoded as given;
// a writer must pass valid UTF-8 without BOM and use BytesEntry for anything else.
func UTF8Entry(tag uint16, s string) Entry {
	return Entry{Tag: tag, Type: TypeUTF8, Value: []byte(s)}
}

// BytesEntry returns a bytes entry whose value aliases b.
func BytesEntry(tag uint16, b []byte) Entry {
	return Entry{Tag: tag, Type: TypeBytes, Value: b}
}

// NestedEntry returns a tlv entry whose value is the encoding of entries, in order.
func NestedEntry(tag uint16, entries []Entry) Entry {
	n := 0
	for _, e := range entries {
		n += HeaderLen + len(e.Value)
	}
	value := make([]byte, 0, n)
	for _, e := range entries {
		value = AppendEntry(value, e)
	}

	return Entry{Tag: tag, Type: TypeTLV, Value: value}
}

// Known reports whether v is one of the value types defined by the format.
func (v ValueType) Known() bool {
	_, ok := v.info()

	return ok
}

// FixedLen returns the value length a fixed-length type requires.
//
// The second result is false for the variable-length types utf8, bytes and tlv,
// and for unknown types, whose length is only known from the entry header.
func (v ValueType) FixedLen() (int, bool) {
	info, ok := v.info()
	if !ok || info.fixedLen == 0 {
		return 0, false
	}

	return info.fixedLen, true
}

// String returns the specification name of v, or "unknown(<n>)" for an unknown type.
func (v ValueType) String() string {
	info, ok := v.info()
	if !ok {
		return "unknown(" + strconv.Itoa(int(v)) + ")"
	}

	return info.name
}

func (v ValueType) info() (valueTypeInfo, bool) {
	switch v {
	case TypeU8:
		return valueTypeInfo{"u8", 1}, true
	case TypeBool:
		return valueTypeInfo{"bool", 1}, true
	case TypeI64:
		return valueTypeInfo{"i64", 8}, true
	case TypeU64:
		return valueTypeInfo{"u64", 8}, true
	case TypeUUID:
		return valueTypeInfo{"uuid", 16}, true
	case TypeUTF8:
		return valueTypeInfo{"utf8", 0}, true
	case TypeBytes:
		return valueTypeInfo{"bytes", 0}, true
	case TypeTLV:
		return valueTypeInfo{"tlv", 0}, true
	default:
		return valueTypeInfo{}, false
	}
}
