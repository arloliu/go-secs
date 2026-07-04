package secs2

import (
	"bytes"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDecode_EmptyInput verifies that an empty byte slice and EmptyItem.ToBytes() both decode
// to an EmptyItem without error.
func TestDecode_EmptyInput(t *testing.T) {
	t.Parallel()

	got, err := Decode([]byte{})
	require.NoError(t, err)
	assert.True(t, got.IsEmpty(), "Decode([]byte{}) should return EmptyItem")

	got2, err := Decode(NewEmptyItem().ToBytes())
	require.NoError(t, err)
	assert.True(t, got2.IsEmpty(), "Decode(EmptyItem.ToBytes()) should return EmptyItem")
}

// TestDecode_RoundTrip checks that encoding an item and decoding it back produces
// byte-for-byte and type/size equal results for every SECS-II item type.
func TestDecode_RoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		item Item
	}{
		{"ASCIIEmpty", NewASCIIItem("")},
		{"ASCII", NewASCIIItem("hello world")},
		{"JIS8Empty", NewJIS8Item("")},
		{"JIS8", NewJIS8Item("jis8-value")},
		{"BinaryEmpty", NewBinaryItem()},
		{"Binary", NewBinaryItem([]byte{0x00, 0x01, 0x7F, 0xFF})},
		{"BooleanEmpty", NewBooleanItem()},
		{"Boolean", NewBooleanItem([]bool{true, false, true, false})},
		{"Int8", NewIntItem(1, int8(-42), int8(0), int8(127))},
		{"Int16", NewIntItem(2, int16(-1000), int16(1000))},
		{"Int32", NewIntItem(4, int32(-100000), int32(100000))},
		{"Int64", NewIntItem(8, int64(-9999999999), int64(9999999999))},
		{"Uint8", NewUintItem(1, uint8(0), uint8(255))},
		{"Uint16", NewUintItem(2, uint16(0), uint16(65535))},
		{"Uint32", NewUintItem(4, uint32(0), uint32(4294967295))},
		{"Uint64", NewUintItem(8, uint64(0), uint64(18446744073709551615))},
		{"Float32", NewFloatItem(4, float32(3.14), float32(-2.71))},
		{"Float64", NewFloatItem(8, 3.141592653589793, -2.718281828459045)},
		{"LocalizedStr", NewLocalizedStrItem(LSHUTF8, "hello")},
		{"LocalizedStrEmpty", NewLocalizedStrItem(LSHNone, "")},
		{"ListEmpty", NewListItem()},
		{"List", NewListItem(NewASCIIItem("a"), NewIntItem(4, int32(42)))},
		{"NestedList", NewListItem(
			NewListItem(NewASCIIItem("nested"), NewIntItem(2, int16(7))),
			NewBooleanItem(true),
		)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			original := tt.item.ToBytes()
			got, err := Decode(original)

			require.NoError(t, err)
			assert.Equal(t, original, got.ToBytes(), "round-trip byte equality")
			assert.Equal(t, tt.item.Type(), got.Type(), "type mismatch")
			assert.Equal(t, tt.item.Size(), got.Size(), "size mismatch")
		})
	}
}

// TestDecode_ValueAccessors verifies that typed accessors on decoded items return
// the correct values for each SECS-II type.
func TestDecode_ValueAccessors(t *testing.T) {
	t.Parallel()

	t.Run("ASCII", func(t *testing.T) {
		t.Parallel()

		got, err := Decode(NewASCIIItem("test-value").ToBytes())
		require.NoError(t, err)
		s, err := got.ToASCII()
		require.NoError(t, err)
		assert.Equal(t, "test-value", s)
	})

	t.Run("JIS8", func(t *testing.T) {
		t.Parallel()

		got, err := Decode(NewJIS8Item("jis8-string").ToBytes())
		require.NoError(t, err)
		s, err := got.ToJIS8()
		require.NoError(t, err)
		assert.Equal(t, "jis8-string", s)
	})

	t.Run("Binary", func(t *testing.T) {
		t.Parallel()

		got, err := Decode(NewBinaryItem([]byte{0x01, 0x02, 0x03}).ToBytes())
		require.NoError(t, err)
		b, err := got.ToBinary()
		require.NoError(t, err)
		assert.Equal(t, []byte{0x01, 0x02, 0x03}, b)
	})

	t.Run("Boolean", func(t *testing.T) {
		t.Parallel()

		got, err := Decode(NewBooleanItem([]bool{true, false, true}).ToBytes())
		require.NoError(t, err)
		bools, err := got.ToBoolean()
		require.NoError(t, err)
		assert.Equal(t, []bool{true, false, true}, bools)
	})

	t.Run("Int8", func(t *testing.T) {
		t.Parallel()

		got, err := Decode(NewIntItem(1, int8(-42)).ToBytes())
		require.NoError(t, err)
		vals, err := got.ToInt()
		require.NoError(t, err)
		assert.Equal(t, []int64{-42}, vals)
	})

	t.Run("Int16", func(t *testing.T) {
		t.Parallel()

		got, err := Decode(NewIntItem(2, int16(1000), int16(-1000)).ToBytes())
		require.NoError(t, err)
		vals, err := got.ToInt()
		require.NoError(t, err)
		assert.Equal(t, []int64{1000, -1000}, vals)
	})

	t.Run("Int32", func(t *testing.T) {
		t.Parallel()

		got, err := Decode(NewIntItem(4, int32(-100000), int32(100000)).ToBytes())
		require.NoError(t, err)
		vals, err := got.ToInt()
		require.NoError(t, err)
		assert.Equal(t, []int64{-100000, 100000}, vals)
	})

	t.Run("Int64", func(t *testing.T) {
		t.Parallel()

		got, err := Decode(NewIntItem(8, int64(-9999999999), int64(9999999999)).ToBytes())
		require.NoError(t, err)
		vals, err := got.ToInt()
		require.NoError(t, err)
		assert.Equal(t, []int64{-9999999999, 9999999999}, vals)
	})

	t.Run("Uint8", func(t *testing.T) {
		t.Parallel()

		got, err := Decode(NewUintItem(1, uint8(0), uint8(255)).ToBytes())
		require.NoError(t, err)
		vals, err := got.ToUint()
		require.NoError(t, err)
		assert.Equal(t, []uint64{0, 255}, vals)
	})

	t.Run("Uint16", func(t *testing.T) {
		t.Parallel()

		got, err := Decode(NewUintItem(2, uint16(60000)).ToBytes())
		require.NoError(t, err)
		vals, err := got.ToUint()
		require.NoError(t, err)
		assert.Equal(t, []uint64{60000}, vals)
	})

	t.Run("Uint32", func(t *testing.T) {
		t.Parallel()

		got, err := Decode(NewUintItem(4, uint32(0), uint32(4294967295)).ToBytes())
		require.NoError(t, err)
		vals, err := got.ToUint()
		require.NoError(t, err)
		assert.Equal(t, []uint64{0, 4294967295}, vals)
	})

	t.Run("Uint64", func(t *testing.T) {
		t.Parallel()

		got, err := Decode(NewUintItem(8, uint64(0), uint64(18446744073709551615)).ToBytes())
		require.NoError(t, err)
		vals, err := got.ToUint()
		require.NoError(t, err)
		assert.Equal(t, []uint64{0, 18446744073709551615}, vals)
	})

	t.Run("Float32", func(t *testing.T) {
		t.Parallel()

		// Use a float32 value so the float64→float32→float64 round-trip is exact.
		f := float32(3.14)
		got, err := Decode(NewFloatItem(4, f).ToBytes())
		require.NoError(t, err)
		vals, err := got.ToFloat()
		require.NoError(t, err)
		require.Len(t, vals, 1)
		assert.Equal(t, float64(f), vals[0])
	})

	t.Run("Float64", func(t *testing.T) {
		t.Parallel()

		got, err := Decode(NewFloatItem(8, 3.141592653589793).ToBytes())
		require.NoError(t, err)
		vals, err := got.ToFloat()
		require.NoError(t, err)
		require.Len(t, vals, 1)
		assert.InDelta(t, 3.141592653589793, vals[0], 1e-15)
	})

	t.Run("LocalizedStr", func(t *testing.T) {
		t.Parallel()

		got, err := Decode(NewLocalizedStrItem(LSHUTF8, "hello").ToBytes())
		require.NoError(t, err)
		s, err := got.ToLocalizedStr()
		require.NoError(t, err)
		assert.Equal(t, "hello", s)
		lsh, err := got.ToLocalizedStrHeader()
		require.NoError(t, err)
		assert.Equal(t, LSHUTF8, lsh)
	})

	t.Run("List", func(t *testing.T) {
		t.Parallel()

		original := NewListItem(NewASCIIItem("a"), NewIntItem(4, int32(42)))
		got, err := Decode(original.ToBytes())
		require.NoError(t, err)

		items, err := got.ToList()
		require.NoError(t, err)
		require.Len(t, items, 2)

		s, err := items[0].ToASCII()
		require.NoError(t, err)
		assert.Equal(t, "a", s)

		vals, err := items[1].ToInt()
		require.NoError(t, err)
		assert.Equal(t, []int64{42}, vals)
	})
}

// TestDecode_OwnsBytes confirms that mutating the caller's input buffer after Decode
// does not affect the decoded item's encoding or values.
func TestDecode_OwnsBytes(t *testing.T) {
	t.Parallel()

	item := NewIntItem(4, int32(12345678))
	data := item.ToBytes()
	original := bytes.Clone(data)

	got, err := Decode(data)
	require.NoError(t, err)

	// Mutate the caller's input buffer — the decoded item must remain unchanged.
	for i := range data {
		data[i] ^= 0xFF
	}

	assert.Equal(t, original, got.ToBytes(), "decoded item was not isolated from caller's buffer mutation")
}

// TestDecode_OwnsBytesBinary specifically tests that binary payload bytes are not
// aliased to the caller's buffer.
func TestDecode_OwnsBytesBinary(t *testing.T) {
	t.Parallel()

	data := NewBinaryItem([]byte{0x01, 0x02, 0x03}).ToBytes()
	original := bytes.Clone(data)

	got, err := Decode(data)
	require.NoError(t, err)

	for i := range data {
		data[i] ^= 0xFF
	}

	assert.Equal(t, original, got.ToBytes())

	b, err := got.ToBinary()
	require.NoError(t, err)
	assert.Equal(t, []byte{0x01, 0x02, 0x03}, b)
}

// TestDecode_OwnsBytesText specifically tests that ASCII/JIS8/LocalizedStr decoded values are
// not aliased to the caller's buffer, despite decodeItem using unsafe.String internally over
// its own private bytes.Clone (see ownedString in decode.go). If Decode's clone were ever
// skipped or reused, this test would catch it via a mismatched decoded value after mutation.
func TestDecode_OwnsBytesText(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		item Item
		want string
	}{
		{"ASCII", NewASCIIItem("hello world"), "hello world"},
		{"JIS8", NewJIS8Item("hello"), "hello"},
		{"LocalizedStr", NewUTF8StrItem("hello"), "hello"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			data := tt.item.ToBytes()

			got, err := Decode(data)
			require.NoError(t, err)

			// Mutate the caller's input buffer after Decode returns.
			for i := range data {
				data[i] ^= 0xFF
			}

			switch v := got.(type) {
			case *ASCIIItem:
				assert.Equal(t, tt.want, v.value, "decoded ASCII value was not isolated from caller's buffer mutation")
			case *JIS8Item:
				assert.Equal(t, tt.want, v.value, "decoded JIS8 value was not isolated from caller's buffer mutation")
			case *LocalizedStrItem:
				assert.Equal(t, tt.want, v.value, "decoded LocalizedStr value was not isolated from caller's buffer mutation")
			default:
				t.Fatalf("unexpected item type %T", got)
			}
		})
	}
}

// TestDecode_ConcurrentReadsText guards the ownedString (unsafe.String) path added to
// decodeItem's ASCII/JIS8/LocalizedStr leaves: concurrent readers of a Decode'd text item
// must never race, since decodeItem never mutates its private clone after construction.
func TestDecode_ConcurrentReadsText(t *testing.T) {
	t.Parallel()

	data := NewASCIIItem("concurrent decode test").ToBytes()
	item, err := Decode(data)
	require.NoError(t, err)

	ascii, ok := item.(*ASCIIItem)
	require.True(t, ok)

	const goroutines = 50

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for range goroutines {
		go func() {
			defer wg.Done()

			_ = item.ToBytes()
			s, err := ascii.ToASCII()
			assert.NoError(t, err)
			assert.Equal(t, "concurrent decode test", s)
		}()
	}

	wg.Wait()
}

// TestDecode_SeededIdentity verifies that a decoded item's ToBytes() equals the
// input slice it was decoded from (byte-for-byte).
func TestDecode_SeededIdentity(t *testing.T) {
	t.Parallel()

	originals := []Item{
		NewASCIIItem("secs2"),
		NewIntItem(2, int16(777)),
		NewListItem(NewASCIIItem("a"), NewListItem(NewBooleanItem(true))),
		NewFloatItem(4, float32(1.5)),
		NewLocalizedStrItem(LSHUTF8, "utf8"),
		NewUintItem(4, uint32(99999)),
	}

	for _, orig := range originals {
		data := orig.ToBytes()
		got, err := Decode(data)

		require.NoError(t, err)
		assert.Equal(t, data, got.ToBytes(), "seeded identity failed for type=%s", orig.Type())
	}
}

// TestDecode_DepthBoundary proves there is no off-by-one in the MaxListDepth check:
// exactly MaxListDepth nested single-child lists succeed, MaxListDepth+1 fails.
func TestDecode_DepthBoundary(t *testing.T) {
	t.Parallel()

	// MaxListDepth (64) nested single-child lists → success.
	deepOK := buildDeepList(MaxListDepth).ToBytes()
	_, err := Decode(deepOK)
	require.NoError(t, err, "should succeed at exactly MaxListDepth=%d", MaxListDepth)

	// MaxListDepth+1 (65) nested single-child lists → error.
	deepFail := buildDeepList(MaxListDepth + 1).ToBytes()
	_, err = Decode(deepFail)
	require.Error(t, err, "should fail at MaxListDepth+1=%d", MaxListDepth+1)
	assert.Contains(t, err.Error(), "nesting depth")
}

// buildDeepList builds an Item with exactly depth nested single-child lists whose
// leaf is a scalar ASCII item. At MaxListDepth the innermost list is at recursion
// depth MaxListDepth (≤ 64) — allowed. At MaxListDepth+1 the innermost list is at
// depth 65 (> 64) — rejected.
func buildDeepList(depth int) Item {
	if depth == 0 {
		return NewASCIIItem("leaf")
	}

	return NewListItem(buildDeepList(depth - 1))
}

// TestDecode_MultiByteLength verifies that items whose length requires 2-byte (or 3-byte) length
// headers encode and decode correctly, exercising the lenByteCount==2 (and ==3) decode paths.
func TestDecode_MultiByteLength(t *testing.T) {
	t.Parallel()

	t.Run("List256Children", func(t *testing.T) {
		t.Parallel()

		// Build a list with 256 children — length field must use a 2-byte header (256 > 255).
		children := make([]Item, 256)
		for i := range children {
			children[i] = NewIntItem(1, int8(0))
		}
		original := NewListItem(children...)

		data := original.ToBytes()
		got, err := Decode(data)
		require.NoError(t, err)
		assert.Equal(t, data, got.ToBytes(), "List256: round-trip byte equality")
		assert.Equal(t, 256, got.Size(), "List256: size mismatch")
	})

	t.Run("Binary300Bytes", func(t *testing.T) {
		t.Parallel()

		// Build a binary item with a 300-byte payload — length field must use a 2-byte header.
		payload := make([]byte, 300)
		for i := range payload {
			payload[i] = byte(i % 256)
		}
		original := NewBinaryItem(payload)

		data := original.ToBytes()
		got, err := Decode(data)
		require.NoError(t, err)
		assert.Equal(t, data, got.ToBytes(), "Binary300: round-trip byte equality")
		assert.Equal(t, 300, got.Size(), "Binary300: size mismatch")

		b, err := got.ToBinary()
		require.NoError(t, err)
		assert.Equal(t, payload, b)
	})

	t.Run("MalformedListHugeChildCount", func(t *testing.T) {
		t.Parallel()

		// Craft a list header claiming 0xFFFFFF (16,777,215) children with no following bytes.
		// ListFormatCode==0; format byte = (0<<2)|3 = 0x03 (3-byte length field).
		// The Fix-1 guard must reject this before any allocation.
		data := []byte{0x03, 0xFF, 0xFF, 0xFF}
		_, err := Decode(data)
		require.Error(t, err, "huge child-count list must return an error")
		assert.Contains(t, err.Error(), "child count")
	})
}

// TestDecode_NeverClamps documents the invariant that wire decode never clamps a value: a valid
// SECS-II byte stream cannot carry an out-of-range fixed-width integer, so decodeIntItem/
// decodeUintItem never need clampInt64/clampUint64. This is a regression marker, not a behavior
// under threat — see the item-8 integer clamp audit.
func TestDecode_NeverClamps(t *testing.T) {
	t.Parallel()

	data := []byte{0xA5, 0x01, 0xFF} // U1[1], format 0xA5 (u1, 1 length byte), value 0xFF
	got, err := Decode(data)
	require.NoError(t, err)

	vals, err := got.ToUint()
	require.NoError(t, err)
	assert.Equal(t, []uint64{255}, vals)
}

// TestDecode_Errors checks that malformed wire bytes return errors with non-nil errors.
func TestDecode_Errors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		data []byte
	}{
		{
			// format code 1 (0x05 >> 2 = 1) is not a valid SECS-II format code.
			name: "unknown format code",
			data: []byte{0x05, 0x00},
		},
		{
			// ASCIIFormatCode (lenByteCount=1) with no length byte following.
			name: "truncated length bytes",
			data: []byte{0x41},
		},
		{
			// ASCII, length=5 but no payload bytes follow.
			name: "truncated payload",
			data: []byte{0x41, 0x05},
		},
		{
			// Int32 (byteSize=4), length=3: 3 % 4 != 0.
			// Int32FormatCode=28=0x1C; format byte = (28<<2)|1 = 0x71; length byte = 0x03.
			name: "int length not divisible by byteSize",
			data: []byte{0x71, 0x03, 0x00, 0x00, 0x00},
		},
		{
			// LocalizedStr (length=1) is too short — needs at least 2 bytes for the LSH.
			// LocalizedStrFormatCode=18=0x12; format byte = (18<<2)|1 = 0x49; length = 0x01.
			name: "localized string too short",
			data: []byte{0x49, 0x01, 0xFF},
		},
		{
			// lenByteCount=0 is always invalid (format byte & 0x03 == 0).
			// 0x40 = ASCIIFormatCode<<2 | 0 = 0x40; lenByteCount=0.
			name: "zero length byte count",
			data: []byte{0x40},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := Decode(tt.data)
			assert.Error(t, err, "expected error for case %q", tt.name)
		})
	}
}

// FuzzDecode_OwnsBytes is the general-purpose regression guard for Decode's isolation
// invariant across every item format, not just the ASCII/JIS8/LocalizedStr cases
// TestDecode_OwnsBytesText hand-picks: whatever Decode returns must never change after the
// caller mutates its input, and Decode must never panic on malformed input.
func FuzzDecode_OwnsBytes(f *testing.F) {
	f.Add(NewASCIIItem("hello world").ToBytes())
	f.Add(NewJIS8Item("hello").ToBytes())
	f.Add(NewUTF8StrItem("hello").ToBytes())
	f.Add(NewBinaryItem([]byte{0x01, 0x02, 0x03}).ToBytes())
	f.Add(NewIntItem(4, int32(-12345)).ToBytes())
	f.Add(NewBooleanItem(true, false, true).ToBytes())
	f.Add(NewListItem(NewUintItem(1, 1, 2, 3), NewASCIIItem("hi")).ToBytes())
	f.Add([]byte{0x40}) // malformed: zero length-byte count

	f.Fuzz(func(t *testing.T, data []byte) {
		got, err := Decode(data)
		if err != nil {
			return
		}

		before := got.ToBytes()

		for i := range data {
			data[i] ^= 0xFF
		}

		assert.Equal(t, before, got.ToBytes(), "decoded item was not isolated from caller's buffer mutation")
	})
}
