package tlv

import (
	"encoding/hex"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Golden entries are hand-built from the entry layout of the tracepack format specification §5.
// Each is the hex of python3 struct.pack('<HBBI', tag, value_type, 0, len(value)) + value.
const (
	goldenU8     = "030001000100000001"
	goldenBool   = "130002000100000001"
	goldenI64    = "1000030008000000feffffffffffffff"
	goldenU64    = "01000400080000000100000000000000"
	goldenUUID   = "1b00050010000000000102030405060708090a0b0c0d0e0f"
	goldenUTF8   = "020006000500000045512d3031"
	goldenBytes  = "01800700020000006162"
	goldenEmpty  = "1700060000000000"
	goldenNested = "26000800200000000300040008000000c8af00000000000005000400080000001027000000000000"
	// Tag 0x7FFF, value_type 9 (unknown), reserved byte 0xAA, value "xyz".
	goldenUnknown = "ff7f09aa0300000078797a"
)

var goldenUUIDValue = [16]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	require.NoError(t, err)

	return b
}

func TestAppendEntryGolden(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		entry Entry
		want  string
	}{
		{"u8", Entry{Tag: 0x0003, Type: TypeU8, Value: []byte{1}}, goldenU8},
		{"bool", Entry{Tag: 0x0013, Type: TypeBool, Value: []byte{1}}, goldenBool},
		{"utf8", Entry{Tag: 0x0002, Type: TypeUTF8, Value: []byte("EQ-01")}, goldenUTF8},
		{"bytes private tag", Entry{Tag: 0x8001, Type: TypeBytes, Value: []byte("ab")}, goldenBytes},
		{"empty value", Entry{Tag: 0x0017, Type: TypeUTF8}, goldenEmpty},
		{"unknown type", Entry{Tag: 0x7FFF, Type: 9, Value: []byte("xyz")}, "ff7f09000300000078797a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := AppendEntry(nil, tt.entry)
			assert.Equal(t, tt.want, hex.EncodeToString(got))
		})
	}
}

func TestAppendEntryAppends(t *testing.T) {
	t.Parallel()

	dst := []byte{0xEE}
	got := AppendEntry(dst, Entry{Tag: 0x0003, Type: TypeU8, Value: []byte{1}})
	assert.Equal(t, "ee"+goldenU8, hex.EncodeToString(got))
}

func TestDecodeGolden(t *testing.T) {
	t.Parallel()

	b := mustHex(t, goldenU8+goldenUTF8+goldenBytes+goldenEmpty)
	entries, err := Decode(b)
	require.NoError(t, err)
	require.Len(t, entries, 4)

	assert.Equal(t, Entry{Tag: 0x0003, Type: TypeU8, Value: []byte{1}, Offset: 0}, entries[0])
	assert.Equal(t, Entry{Tag: 0x0002, Type: TypeUTF8, Value: []byte("EQ-01"), Offset: 9}, entries[1])
	assert.Equal(t, Entry{Tag: 0x8001, Type: TypeBytes, Value: []byte("ab"), Offset: 22}, entries[2])
	assert.Equal(t, uint16(0x0017), entries[3].Tag)
	assert.Equal(t, 32, entries[3].Offset)
	assert.Empty(t, entries[3].Value)
}

func TestDecodeEmptyInput(t *testing.T) {
	t.Parallel()

	entries, err := Decode(nil)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

// An unknown value_type is opaque:
// the entry is kept with its raw value, and the reserved byte is ignored.
func TestDecodeUnknownValueTypePreserved(t *testing.T) {
	t.Parallel()

	entries, err := Decode(mustHex(t, goldenUnknown))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, Entry{Tag: 0x7FFF, Type: 9, Value: []byte("xyz")}, entries[0])
}

func TestDecodeValueAliasesInput(t *testing.T) {
	t.Parallel()

	b := mustHex(t, goldenBytes)
	entries, err := Decode(b)
	require.NoError(t, err)
	b[8] = 'Z'
	assert.Equal(t, []byte("Zb"), entries[0].Value)
}

func TestDecodeErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		in     string
		want   error
		tag    uint16
		offset int
	}{
		{"header shorter than 8 bytes", "0300010001", ErrTruncated, 0x0003, 0},
		{"one byte", "03", ErrTruncated, 0, 0},
		{"value crosses the end", "02000600050000004551", ErrTruncated, 0x0002, 0},
		{"second entry crosses the end", goldenU8 + "0200060005000000", ErrTruncated, 0x0002, 9},
		{"length 0xFFFFFFFF", "02000700ffffffff00", ErrTruncated, 0x0002, 0},
		{"tag 0x0000", "000001000100000001", ErrZeroTag, 0x0000, 0},
		{"zero tag after a valid entry", goldenU8 + "000007000000000000", ErrZeroTag, 0x0000, 9},
		{"u8 of length 2", "03000100020000000101", ErrLength, 0x0003, 0},
		{"bool of length 0", "1300020000000000", ErrLength, 0x0013, 0},
		{"i64 of length 7", "100003000700000001020304050607", ErrLength, 0x0010, 0},
		{"u64 of length 9", "010004000900000001000000000000000000", ErrLength, 0x0001, 0},
		{"uuid of length 15", "1b0005000f000000000102030405060708090a0b0c0d0e", ErrLength, 0x001B, 0},
		{"fixed-length check on an unknown tag", "ff7f010002000000ffff", ErrLength, 0x7FFF, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			entries, err := Decode(mustHex(t, tt.in))
			require.ErrorIs(t, err, tt.want)
			assert.Nil(t, entries)

			var ee *EntryError
			require.True(t, errors.As(err, &ee))
			assert.Equal(t, tt.tag, ee.Tag)
			assert.Equal(t, tt.offset, ee.Offset)
		})
	}
}

func TestEntryErrorMessage(t *testing.T) {
	t.Parallel()

	err := &EntryError{Tag: 0x002A, Offset: 17, Err: ErrLength}
	assert.Equal(t, "tlv: tag 0x002A at offset 17: "+ErrLength.Error(), err.Error())
	assert.ErrorIs(t, err, ErrLength)
}

// Entries built with the typed constructors survive an encode and decode unchanged, apart from Offset.
func TestEntriesRoundTrip(t *testing.T) {
	t.Parallel()

	in := []Entry{
		U64Entry(0x0001, 1),
		UTF8Entry(0x0002, "EQ-01"),
		U8Entry(0x0003, 1),
		I64Entry(0x0010, -1<<63),
		BoolEntry(0x0013, false),
		UUIDEntry(0x001B, goldenUUIDValue),
		UTF8Entry(0x000E, "a.log"),
		UTF8Entry(0x000E, "b.log"),
		U64Entry(0x0027, MaxU64),
		NestedEntry(0x0026, []Entry{U64Entry(1, 45000)}),
		BytesEntry(0x9000, []byte{0, 1, 2}),
		{Tag: 0x7FFF, Type: 200, Value: []byte("opaque")},
	}
	var b []byte
	offsets := make([]int, 0, len(in))
	for _, e := range in {
		offsets = append(offsets, len(b))
		b = AppendEntry(b, e)
	}

	out, err := Decode(b)
	require.NoError(t, err)
	require.Len(t, out, len(in))
	for i := range in {
		want := in[i]
		want.Offset = offsets[i]
		assert.Equal(t, want, out[i], "entry %d", i)
	}
}
