package secs2

import (
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestLocalizedStrItem covers round-trip construction, exact wire bytes, and SML output.
// Vectors are ported from git show main:secs2/localized_str_test.go.
//
// Wire layout: [format_byte][len_byte(s)][lsh_hi][lsh_lo][string_bytes...]
// Format byte for LocalizedStrFormatCode (0o22=18): (18<<2)|1 = 73 = 0x49.
// len_byte(s) encodes len(value)+2 (the +2 accounts for the 2-byte LSH header).
func TestLocalizedStrItem(t *testing.T) {
	t.Parallel()

	tests := []struct {
		description     string
		item            Item
		expectedSize    int
		expectedToBytes []byte
		expectedToSML   string
	}{
		{
			description:     "empty UTF-8 string",
			item:            NewUTF8StrItem(""),
			expectedSize:    2, // 0 string bytes + 2 LSH bytes
			expectedToBytes: []byte{0x49, 0x02, 0x00, 0x02},
			expectedToSML:   `<W "">`,
		},
		{
			description:     "short UTF-8 string hi",
			item:            NewUTF8StrItem("hi"),
			expectedSize:    4, // 2 + 2
			expectedToBytes: []byte{0x49, 0x04, 0x00, 0x02, 'h', 'i'},
			expectedToSML:   `<W "hi">`,
		},
		{
			description:     "hello world UTF-8",
			item:            NewUTF8StrItem("hello world"),
			expectedSize:    13, // 11 + 2
			expectedToBytes: []byte{0x49, 0x0d, 0x00, 0x02, 0x68, 0x65, 0x6c, 0x6c, 0x6f, 0x20, 0x77, 0x6f, 0x72, 0x6c, 0x64},
			expectedToSML:   `<W "hello world">`,
		},
		{
			description:  "custom LSH ASCII",
			item:         NewLocalizedStrItem(LSHASCII, "test"),
			expectedSize: 6, // 4 + 2
			// 0x49 = format byte, 0x06 = length, 0x00 0x03 = LSH 3 (ASCII), "test"
			expectedToBytes: []byte{0x49, 0x06, 0x00, 0x03, 't', 'e', 's', 't'},
			expectedToSML:   `<W "test">`,
		},
	}

	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			t.Parallel()

			require := require.New(t)

			require.NoError(test.item.Error())
			require.Equal(test.expectedSize, test.item.Size())
			require.Equal(test.expectedToBytes, test.item.ToBytes())
			require.Equal(test.expectedToSML, test.item.ToSML())

			_, err := test.item.ToLocalizedStr()
			require.NoError(err)
		})
	}
}

// TestLocalizedStrItem_AppendTo verifies AppendTo with an independent literal-bytes vector.
// NewLocalizedStrItem(LSHUTF8, "AB") — LSH=2 (UTF-8), value "AB" (2 bytes):
//
//	format byte: (18<<2)|1 = 0x49
//	length: 2+2 = 4 = 0x04
//	LSH big-endian: 0x00, 0x02
//	'A'=0x41, 'B'=0x42
func TestLocalizedStrItem_AppendTo(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	item := NewLocalizedStrItem(LSHUTF8, "AB")
	prefix := []byte{0xDE, 0xAD}
	itemBytes := []byte{0x49, 0x04, 0x00, 0x02, 0x41, 0x42}

	expected := append(slices.Clone(prefix), itemBytes...)
	result := item.AppendTo(slices.Clone(prefix))
	require.Equal(expected, result)
}

// TestLocalizedStrItem_Header verifies that ToLocalizedStrHeader returns the correct LSH and
// that NewUTF8StrItem sets LSHUTF8.
func TestLocalizedStrItem_Header(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	// NewUTF8StrItem must use LSHUTF8 = 2.
	item := NewUTF8StrItem("hello")
	lsh, err := item.ToLocalizedStrHeader()
	require.NoError(err)
	require.Equal(LSHUTF8, lsh)

	// NewLocalizedStrItem preserves the given LSH.
	item2 := NewLocalizedStrItem(LSHShiftJIS, "test")
	lsh2, err := item2.ToLocalizedStrHeader()
	require.NoError(err)
	require.Equal(LSHShiftJIS, lsh2)

	// ToLocalizedStr returns the string part.
	str, err := item2.ToLocalizedStr()
	require.NoError(err)
	require.Equal("test", str)
}

// TestLocalizedStrItem_NoLeak confirms that the string returned by ToLocalizedStr is immutable.
func TestLocalizedStrItem_NoLeak(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	const original = "hello"

	item := NewUTF8StrItem(original)

	s, err := item.ToLocalizedStr()
	require.NoError(err)
	require.Equal(original, s)

	// Mutate a []byte copy of the returned string.
	mutated := []byte(s)
	mutated[0] = 'X'

	// Item's internal state and subsequent reads must be unchanged.
	s2, err := item.ToLocalizedStr()
	require.NoError(err)
	require.Equal(original, s2)

	concrete, ok := item.(*LocalizedStrItem)
	require.True(ok)
	require.Equal(original, concrete.value)
	require.Equal(LSHUTF8, concrete.lsh)
}

// TestLocalizedStrItem_WrongType verifies that wrong-type accessors return errors.
func TestLocalizedStrItem_WrongType(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	item := NewUTF8StrItem("hello")
	require.NoError(item.Error())

	// String accessors for other types must error.
	_, err := item.ToASCII()
	require.Error(err)

	_, err = item.ToJIS8()
	require.Error(err)

	// Numeric accessors must error.
	_, err = item.ToInt()
	require.Error(err)

	_, err = item.ToUint()
	require.Error(err)

	_, err = item.ToFloat()
	require.Error(err)

	// Get is list-only in v2 — any call on a scalar item must error.
	got, err := item.Get()
	require.Nil(got)
	require.Error(err)

	got, err = item.Get(0)
	require.Nil(got)
	require.Error(err)
}

// TestLocalizedStrItem_TypeAccessors verifies Type() and IsLocalizedStr().
func TestLocalizedStrItem_TypeAccessors(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	item := NewUTF8StrItem("test")
	require.Equal(LocalizedStrType, item.Type())
	require.True(item.IsLocalizedStr())
	require.False(item.IsASCII())
	require.False(item.IsJIS8())
	require.False(item.IsInt8())
	require.False(item.IsBinary())
	require.False(item.IsBoolean())
	require.False(item.IsList())
}

// TestLocalizedStrItem_Errors verifies the deferred-error behaviour for oversized strings.
func TestLocalizedStrItem_Errors(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	itemErr := &ItemError{}

	// String where len(value)+2 exceeds MaxByteSize must set a deferred error.
	huge := make([]byte, MaxByteSize)
	item := NewLocalizedStrItem(LSHUTF8, string(huge))
	require.ErrorAs(item.Error(), &itemErr)

	// ToLocalizedStr on error item returns empty string and the error.
	s, err := item.ToLocalizedStr()
	require.Equal("", s)
	require.Error(err)

	// ToLocalizedStrHeader on error item returns 0 and the error.
	lsh, err := item.ToLocalizedStrHeader()
	require.Equal(uint16(0), lsh)
	require.Error(err)

	// EncodedLen and AppendTo on error item.
	require.Equal(0, item.EncodedLen())
	dst := []byte{0xAA}
	require.Equal(dst, item.AppendTo(dst))
}

// TestLocalizedStrItem_Concurrency exercises concurrent reads under -race.
func TestLocalizedStrItem_Concurrency(t *testing.T) {
	t.Parallel()

	item := NewUTF8StrItem("concurrent test")

	const goroutines = 50

	var wg sync.WaitGroup

	wg.Add(goroutines)

	for range goroutines {
		go func() {
			defer wg.Done()

			_ = item.ToBytes()

			buf := make([]byte, 0, item.EncodedLen())
			_ = item.AppendTo(buf)

			_, _ = item.ToLocalizedStr()
			_, _ = item.ToLocalizedStrHeader()

			_ = item.ToSML()
		}()
	}

	wg.Wait()
}

// TestLocalizedStrItem_Allocs asserts allocation counts for the hot encoding paths.
func TestLocalizedStrItem_Allocs(t *testing.T) {
	item := NewUTF8StrItem("hello world")

	// AppendTo into a pre-sized buffer must not allocate.
	buf := make([]byte, 0, item.EncodedLen())

	allocs := testing.AllocsPerRun(100, func() {
		buf = buf[:0]
		buf = item.AppendTo(buf)
	})

	if allocs > 0 {
		t.Errorf("AppendTo into pre-sized buffer: got %v allocs, want 0", allocs)
	}

	// ToBytes must allocate exactly once (the output slice).
	allocs = testing.AllocsPerRun(100, func() {
		_ = item.ToBytes()
	})

	if allocs != 1 {
		t.Errorf("ToBytes: got %v allocs, want 1", allocs)
	}
}

// TestLocalizedStrItem_EncodedLen verifies that EncodedLen() == len(ToBytes()) for all cases.
func TestLocalizedStrItem_EncodedLen(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	cases := []struct {
		lsh   uint16
		value string
	}{
		{LSHUTF8, ""},
		{LSHUTF8, "hello"},
		{LSHASCII, "test"},
		{LSHShiftJIS, "こんにちは"}, // multi-byte value
	}

	for _, c := range cases {
		item := NewLocalizedStrItem(c.lsh, c.value)
		require.NoError(item.Error())
		require.Equal(item.EncodedLen(), len(item.ToBytes()),
			"EncodedLen must equal len(ToBytes()) for lsh=%d value=%q", c.lsh, c.value)
	}

	// Error item: both EncodedLen and len(ToBytes) must be 0.
	huge := make([]byte, MaxByteSize)
	errItem := NewLocalizedStrItem(LSHUTF8, string(huge))
	require.Error(errItem.Error())
	require.Equal(0, errItem.EncodedLen())
	require.Equal(errItem.EncodedLen(), len(errItem.ToBytes()))
}
