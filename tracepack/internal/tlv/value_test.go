package tlv

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The numbers, names and lengths come from the value-type table of the tracepack format specification §5.
func TestValueTypeTable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		vt       ValueType
		num      uint8
		name     string
		fixedLen int
		fixed    bool
	}{
		{TypeU8, 1, "u8", 1, true},
		{TypeBool, 2, "bool", 1, true},
		{TypeI64, 3, "i64", 8, true},
		{TypeU64, 4, "u64", 8, true},
		{TypeUUID, 5, "uuid", 16, true},
		{TypeUTF8, 6, "utf8", 0, false},
		{TypeBytes, 7, "bytes", 0, false},
		{TypeTLV, 8, "tlv", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.num, uint8(tt.vt))
			assert.Equal(t, tt.name, tt.vt.String())
			assert.True(t, tt.vt.Known())
			n, fixed := tt.vt.FixedLen()
			assert.Equal(t, tt.fixed, fixed)
			assert.Equal(t, tt.fixedLen, n)
		})
	}
}

func TestValueTypeUnknown(t *testing.T) {
	t.Parallel()

	for _, v := range []uint8{0, 9, 0x7F, 0xFF} {
		vt := ValueType(v)
		assert.False(t, vt.Known(), "value %d", v)
		n, fixed := vt.FixedLen()
		assert.False(t, fixed, "value %d", v)
		assert.Zero(t, n, "value %d", v)
	}
	assert.Equal(t, "unknown(0)", ValueType(0).String())
	assert.Equal(t, "unknown(255)", ValueType(255).String())
}

func TestConstructorsGolden(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		entry Entry
		want  string
	}{
		{"u8", U8Entry(0x0003, 1), goldenU8},
		{"bool", BoolEntry(0x0013, true), goldenBool},
		{"i64", I64Entry(0x0010, -2), goldenI64},
		{"u64", U64Entry(0x0001, 1), goldenU64},
		{"uuid", UUIDEntry(0x001B, goldenUUIDValue), goldenUUID},
		{"utf8", UTF8Entry(0x0002, "EQ-01"), goldenUTF8},
		{"bytes", BytesEntry(0x8001, []byte("ab")), goldenBytes},
		{"nested", NestedEntry(0x0026, []Entry{U64Entry(3, 45000), U64Entry(5, 10000)}), goldenNested},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, hex.EncodeToString(AppendEntry(nil, tt.entry)))
		})
	}
}

func TestBoolEntryFalse(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "130002000100000000", hex.EncodeToString(AppendEntry(nil, BoolEntry(0x0013, false))))
}

func TestAccessorsDecodeGolden(t *testing.T) {
	t.Parallel()

	b := mustHex(t, goldenU8+goldenBool+goldenI64+goldenU64+goldenUUID+goldenUTF8+goldenBytes+goldenNested)
	entries, err := Decode(b)
	require.NoError(t, err)
	require.Len(t, entries, 8)

	u8, err := entries[0].U8()
	require.NoError(t, err)
	assert.Equal(t, uint8(1), u8)

	bv, err := entries[1].Bool()
	require.NoError(t, err)
	assert.True(t, bv)

	i64, err := entries[2].I64()
	require.NoError(t, err)
	assert.Equal(t, int64(-2), i64)

	u64, err := entries[3].U64()
	require.NoError(t, err)
	assert.Equal(t, uint64(1), u64)

	id, err := entries[4].UUID()
	require.NoError(t, err)
	assert.Equal(t, goldenUUIDValue, id)

	s, err := entries[5].UTF8()
	require.NoError(t, err)
	assert.Equal(t, "EQ-01", s)

	raw, err := entries[6].Bytes()
	require.NoError(t, err)
	assert.Equal(t, []byte("ab"), raw)

	nested, err := entries[7].Nested()
	require.NoError(t, err)
	require.Len(t, nested, 2)
	assert.Equal(t, uint16(3), nested[0].Tag)
	assert.Equal(t, 0, nested[0].Offset)
	assert.Equal(t, uint16(5), nested[1].Tag)
	assert.Equal(t, 16, nested[1].Offset)
	t3, err := nested[0].U64()
	require.NoError(t, err)
	assert.Equal(t, uint64(45000), t3)
	t5, err := nested[1].U64()
	require.NoError(t, err)
	assert.Equal(t, uint64(10000), t5)
}

func TestU64Limit(t *testing.T) {
	t.Parallel()

	maxOK := Entry{Tag: 1, Type: TypeU64, Value: mustHex(t, "ffffffffffffff7f")}
	v, err := maxOK.U64()
	require.NoError(t, err)
	assert.Equal(t, uint64(1<<63-1), v)

	over := Entry{Tag: 1, Type: TypeU64, Value: mustHex(t, "0000000000000080"), Offset: 24}
	_, err = over.U64()
	require.ErrorIs(t, err, ErrLimit)
	var ee *EntryError
	require.ErrorAs(t, err, &ee)
	assert.Equal(t, uint16(1), ee.Tag)
	assert.Equal(t, 24, ee.Offset)
}

func TestAccessorErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		call func() error
		want error
	}{
		{"u8 of a u64 entry", func() error { _, err := U64Entry(1, 1).U8(); return err }, ErrType},
		{"u64 of a u8 entry", func() error { _, err := U8Entry(1, 1).U64(); return err }, ErrType},
		{"bytes of an unknown type", func() error { _, err := Entry{Tag: 1, Type: 9}.Bytes(); return err }, ErrType},
		{"utf8 of a bytes entry", func() error { _, err := BytesEntry(1, nil).UTF8(); return err }, ErrType},
		{"nested of a bytes entry", func() error { _, err := BytesEntry(1, nil).Nested(); return err }, ErrType},
		{"u8 of length 0", func() error { _, err := Entry{Tag: 1, Type: TypeU8}.U8(); return err }, ErrLength},
		{"bool of length 2", func() error { _, err := Entry{Tag: 1, Type: TypeBool, Value: []byte{0, 1}}.Bool(); return err }, ErrLength},
		{"i64 of length 7", func() error { _, err := Entry{Tag: 1, Type: TypeI64, Value: make([]byte, 7)}.I64(); return err }, ErrLength},
		{"u64 of length 9", func() error { _, err := Entry{Tag: 1, Type: TypeU64, Value: make([]byte, 9)}.U64(); return err }, ErrLength},
		{"uuid of length 15", func() error { _, err := Entry{Tag: 1, Type: TypeUUID, Value: make([]byte, 15)}.UUID(); return err }, ErrLength},
		{"bool value 2", func() error { _, err := Entry{Tag: 1, Type: TypeBool, Value: []byte{2}}.Bool(); return err }, ErrBool},
		{"invalid utf8", func() error { _, err := Entry{Tag: 1, Type: TypeUTF8, Value: []byte{'a', 0xFF}}.UTF8(); return err }, ErrUTF8},
		{"truncated utf8 sequence", func() error { _, err := Entry{Tag: 1, Type: TypeUTF8, Value: []byte{0xE2, 0x82}}.UTF8(); return err }, ErrUTF8},
		{"malformed nested value", func() error {
			_, err := Entry{Tag: 1, Type: TypeTLV, Value: []byte{0, 0, 1, 0, 1, 0, 0, 0, 1}}.Nested()
			return err
		}, ErrZeroTag},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.call()
			require.ErrorIs(t, err, tt.want)
			var ee *EntryError
			assert.ErrorAs(t, err, &ee)
		})
	}
}

// A nested decode error names the outer entry and wraps the nested entry's error.
func TestNestedErrorLocation(t *testing.T) {
	t.Parallel()

	// Outer tag 0x0026 at offset 0 holding one u64 entry cut short by one byte.
	outer := mustHex(t, "260008000f0000000300040008000000c8af0000000000")
	entries, err := Decode(outer)
	require.NoError(t, err)
	_, err = entries[0].Nested()
	require.ErrorIs(t, err, ErrTruncated)

	var ee *EntryError
	require.ErrorAs(t, err, &ee)
	assert.Equal(t, uint16(0x0026), ee.Tag)
	var inner *EntryError
	require.ErrorAs(t, ee.Err, &inner)
	assert.Equal(t, uint16(0x0003), inner.Tag)
	assert.Equal(t, 0, inner.Offset)
}

func TestUTF8EmptyAndMultibyte(t *testing.T) {
	t.Parallel()

	s, err := UTF8Entry(1, "").UTF8()
	require.NoError(t, err)
	assert.Empty(t, s)

	s, err = UTF8Entry(1, "Asia/Taipei 台北").UTF8()
	require.NoError(t, err)
	assert.Equal(t, "Asia/Taipei 台北", s)
}

// CheckValue applies the rules Validate applies to a known tag's value.
func TestCheckValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		e    Entry
		want ValueType
		err  error
	}{
		{"u8", U8Entry(1, 0xFF), TypeU8, nil},
		{"bool 0", BoolEntry(1, false), TypeBool, nil},
		{"bool 1", BoolEntry(1, true), TypeBool, nil},
		{"bool 2", Entry{Tag: 1, Type: TypeBool, Value: []byte{2}}, TypeBool, ErrBool},
		{"u64 at the limit", U64Entry(1, MaxU64), TypeU64, nil},
		{"u64 over the limit", U64Entry(1, MaxU64+1), TypeU64, ErrLimit},
		{"i64 minimum", I64Entry(1, -1<<63), TypeI64, nil},
		{"uuid", UUIDEntry(1, goldenUUIDValue), TypeUUID, nil},
		{"utf8 with a leading BOM", UTF8Entry(1, "\xef\xbb\xbfa"), TypeUTF8, nil},
		{"utf8 empty", UTF8Entry(1, ""), TypeUTF8, nil},
		{"invalid utf8", Entry{Tag: 1, Type: TypeUTF8, Value: []byte{0xFF}}, TypeUTF8, ErrUTF8},
		{"bytes", BytesEntry(1, []byte{0xFF}), TypeBytes, nil},
		{"tlv value not examined", Entry{Tag: 1, Type: TypeTLV, Value: []byte{0xFF}}, TypeTLV, nil},
		{"wrong type", U8Entry(1, 1), TypeBool, ErrType},
		{"unknown type wanted", Entry{Tag: 1, Type: 9, Value: []byte{1}}, 9, nil},
		{"wrong fixed length", Entry{Tag: 1, Type: TypeU64, Value: []byte{1}}, TypeU64, ErrLength},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.e.CheckValue(tt.want)
			if tt.err == nil {
				require.NoError(t, err)

				return
			}
			require.ErrorIs(t, err, tt.err)
			var ee *EntryError
			require.ErrorAs(t, err, &ee)
			assert.Equal(t, tt.e.Tag, ee.Tag)
		})
	}
}
