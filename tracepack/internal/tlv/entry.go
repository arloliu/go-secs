package tlv

import (
	"encoding/binary"
	"errors"
	"fmt"
	"iter"
	"unicode/utf8"
)

// HeaderLen is the length of a TLV entry header: tag u16, value_type u8, reserved u8, length u32.
const HeaderLen = 8

// Sentinel errors.
// Every error returned for a specific entry is an *EntryError that unwraps to one of these.
var (
	// ErrTruncated reports an entry header or value that crosses the end of the buffer.
	ErrTruncated = errors.New("tlv: entry crosses the end of the buffer")
	// ErrZeroTag reports an entry with the invalid tag 0x0000.
	ErrZeroTag = errors.New("tlv: tag 0x0000 is invalid")
	// ErrLength reports a value whose length does not match its value type or structure.
	ErrLength = errors.New("tlv: value length does not match its type")
	// ErrType reports a value_type that differs from the one the registry or accessor expects.
	ErrType = errors.New("tlv: value type does not match")
	// ErrDuplicate reports a non-repeatable tag that appears more than once.
	ErrDuplicate = errors.New("tlv: non-repeatable tag appears more than once")
	// ErrMissing reports a required tag that is absent.
	ErrMissing = errors.New("tlv: required tag is missing")
	// ErrLimit reports a u64 value above 2^63 − 1.
	ErrLimit = errors.New("tlv: u64 value exceeds 2^63-1")
	// ErrUTF8 reports a utf8 value that is not valid UTF-8.
	ErrUTF8 = errors.New("tlv: utf8 value is not valid UTF-8")
	// ErrBool reports a bool value other than 0 or 1.
	ErrBool = errors.New("tlv: bool value is not 0 or 1")
	// ErrDepth reports a tlv value nested more than MaxNestingDepth levels deep, which Validate does not descend into.
	ErrDepth = errors.New("tlv: nested tlv values exceed the nesting depth limit")
)

// Entry is one TLV entry of the tracepack format specification §5.
//
// An entry returned by Decode aliases the decoded buffer:
// Value is a sub-slice of it, so the caller must not modify the buffer while the entry is in use,
// and must copy Value to keep it beyond the buffer's lifetime.
type Entry struct {
	// Tag identifies the entry in the registry of the enclosing structure.
	Tag uint16
	// Type is the value_type; a value outside 1–8 is kept as is.
	Type ValueType
	// Value is the raw value bytes.
	Value []byte
	// Offset is the byte offset of the entry header within the buffer passed to Decode.
	// It is zero for an entry built by the caller and is never encoded.
	Offset int
}

// EntryError is an error found in one entry.
// errors.Is matches it against the package's sentinel errors.
type EntryError struct {
	// Tag is the entry's tag, or 0 when the tag bytes themselves are missing.
	Tag uint16
	// Offset is the byte offset of the entry header, or -1 for a required tag that is absent.
	Offset int
	// Err is the sentinel error describing the failure,
	// or for a nested value that does not decode or validate, the nested entry's *EntryError.
	Err error
}

// AppendEntry appends the encoding of e to dst and returns the extended slice.
//
// The reserved byte is written as zero and e.Offset is ignored.
// The value is written as given; its length must fit in a u32.
func AppendEntry(dst []byte, e Entry) []byte {
	dst = binary.LittleEndian.AppendUint16(dst, e.Tag)
	dst = append(dst, byte(e.Type), 0)
	dst = binary.LittleEndian.AppendUint32(dst, uint32(len(e.Value)))

	return append(dst, e.Value...)
}

// Decode decodes the TLV entries that fill b exactly.
//
// It rejects an entry that crosses the end of b, the tag 0x0000,
// and a value whose length does not match a fixed-length value type (1–5), whatever the tag.
// Anything else is kept for the registry check or the typed accessors:
// unknown tags, unknown value types, and value contents are not examined.
// The reserved byte of each entry header is ignored.
// The returned entries alias b; see Entry.
//
// Returns:
//   - []Entry: the entries in encoding order, nil when b is empty
//   - error: an *EntryError for the first malformed entry
func Decode(b []byte) ([]Entry, error) {
	count := 0
	for off := 0; off < len(b); count++ {
		_, next, err := decodeOne(b, off)
		if err != nil {
			return nil, err
		}
		off = next
	}
	if count == 0 {
		return nil, nil
	}

	entries := make([]Entry, 0, count)
	for off := 0; off < len(b); {
		e, next, _ := decodeOne(b, off)
		entries = append(entries, e)
		off = next
	}

	return entries, nil
}

// Entries returns an iterator over the TLV entries that fill b, in encoding order,
// applying the same checks as Decode without building an entry slice.
//
// Each well-formed entry is yielded with a nil error and aliases b, as an entry returned by Decode does.
// At the first malformed entry the iterator yields the zero Entry and the *EntryError Decode would return, then stops;
// the entries yielded before it are well formed, so a caller that must not act on a malformed list
// decides only after the iteration ends without an error.
// An empty b yields nothing.
func Entries(b []byte) iter.Seq2[Entry, error] {
	return func(yield func(Entry, error) bool) {
		for off := 0; off < len(b); {
			e, next, err := decodeOne(b, off)
			if err != nil {
				yield(Entry{}, err)

				return
			}
			if !yield(e, nil) {
				return
			}
			off = next
		}
	}
}

// decodeOne decodes the entry whose header starts at b[off:] and returns it with the offset of the next entry.
func decodeOne(b []byte, off int) (Entry, int, error) {
	rest := b[off:]
	if len(rest) < HeaderLen {
		var tag uint16
		if len(rest) >= 2 {
			tag = binary.LittleEndian.Uint16(rest)
		}

		return Entry{}, 0, &EntryError{Tag: tag, Offset: off, Err: ErrTruncated}
	}

	tag := binary.LittleEndian.Uint16(rest)
	vt := ValueType(rest[2])
	length := binary.LittleEndian.Uint32(rest[4:])
	if tag == 0 {
		return Entry{}, 0, &EntryError{Tag: tag, Offset: off, Err: ErrZeroTag}
	}
	// Compare in 64 bits before converting, so a huge length cannot overflow int.
	if uint64(length) > uint64(len(rest)-HeaderLen) {
		return Entry{}, 0, &EntryError{Tag: tag, Offset: off, Err: ErrTruncated}
	}
	if n, fixed := vt.FixedLen(); fixed && int(length) != n {
		return Entry{}, 0, &EntryError{Tag: tag, Offset: off, Err: ErrLength}
	}

	end := HeaderLen + int(length)
	e := Entry{Tag: tag, Type: vt, Value: rest[HeaderLen:end:end], Offset: off}

	return e, off + end, nil
}

// U8 returns the value of a u8 entry.
func (e Entry) U8() (uint8, error) {
	if err := e.check(TypeU8); err != nil {
		return 0, err
	}

	return e.Value[0], nil
}

// Bool returns the value of a bool entry; a value other than 0 or 1 is an error.
func (e Entry) Bool() (bool, error) {
	if err := e.CheckValue(TypeBool); err != nil {
		return false, err
	}

	return e.Value[0] == 1, nil
}

// I64 returns the value of an i64 entry.
func (e Entry) I64() (int64, error) {
	if err := e.check(TypeI64); err != nil {
		return 0, err
	}

	return int64(binary.LittleEndian.Uint64(e.Value)), nil
}

// U64 returns the value of a u64 entry; a value above MaxU64 is an error.
func (e Entry) U64() (uint64, error) {
	if err := e.CheckValue(TypeU64); err != nil {
		return 0, err
	}

	return binary.LittleEndian.Uint64(e.Value), nil
}

// UUID returns the value of a uuid entry in RFC 9562 byte order.
func (e Entry) UUID() ([16]byte, error) {
	if err := e.check(TypeUUID); err != nil {
		return [16]byte{}, err
	}

	return [16]byte(e.Value), nil
}

// UTF8 returns the value of a utf8 entry as a string; invalid UTF-8 is an error.
// A leading BOM is valid UTF-8 and is returned as part of the string.
func (e Entry) UTF8() (string, error) {
	if err := e.CheckValue(TypeUTF8); err != nil {
		return "", err
	}

	return string(e.Value), nil
}

// Bytes returns the value of a bytes entry; the result aliases e.Value.
func (e Entry) Bytes() ([]byte, error) {
	if err := e.check(TypeBytes); err != nil {
		return nil, err
	}

	return e.Value, nil
}

// Nested decodes the value of a tlv entry with Decode.
// A malformed nested entry is reported as an *EntryError for e that wraps the nested entry's *EntryError,
// whose offset is relative to the start of e.Value.
func (e Entry) Nested() ([]Entry, error) {
	if err := e.check(TypeTLV); err != nil {
		return nil, err
	}
	entries, err := Decode(e.Value)
	if err != nil {
		return nil, e.errorf(err)
	}

	return entries, nil
}

// CheckValue reports whether e is a valid value of the value type want:
// e has value type want, a fixed-length type has its length,
// and the value passes the content rule of want (the tracepack format specification §5):
// a bool is 0 or 1, a u64 is at most MaxU64 (§2), and a utf8 value is valid UTF-8.
// A leading BOM passes, because it is valid UTF-8:
// the specification's "UTF-8 without BOM" binds writers, while a reader rejects only a value that is not UTF-8.
// Any u8, i64, uuid or bytes value of the right length is valid,
// a tlv value is checked where it is decoded,
// and an unknown value type has no content rule.
// These are the rules the typed accessors and Validate apply to a known tag's value.
//
// Returns:
//   - error: nil, or an *EntryError for e wrapping ErrType, ErrLength, ErrBool, ErrLimit or ErrUTF8.
func (e Entry) CheckValue(want ValueType) error {
	if err := e.check(want); err != nil {
		return err
	}

	if want == TypeBool && e.Value[0] > 1 {
		return e.errorf(ErrBool)
	}
	if want == TypeU64 && binary.LittleEndian.Uint64(e.Value) > MaxU64 {
		return e.errorf(ErrLimit)
	}
	if want == TypeUTF8 && !utf8.Valid(e.Value) {
		return e.errorf(ErrUTF8)
	}

	return nil
}

// Error implements the error interface.
func (e *EntryError) Error() string {
	return fmt.Sprintf("tlv: tag 0x%04X at offset %d: %v", e.Tag, e.Offset, e.Err)
}

// Unwrap returns the sentinel error.
func (e *EntryError) Unwrap() error {
	return e.Err
}

// check verifies that e has value type want and, for a fixed-length type, the matching value length.
func (e Entry) check(want ValueType) error {
	if e.Type != want {
		return e.errorf(ErrType)
	}
	if n, fixed := want.FixedLen(); fixed && len(e.Value) != n {
		return e.errorf(ErrLength)
	}

	return nil
}

func (e Entry) errorf(err error) error {
	return &EntryError{Tag: e.Tag, Offset: e.Offset, Err: err}
}
