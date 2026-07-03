package secs2

import (
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestASCIIItem covers round-trip construction, exact wire bytes, and SML output.
// Tests include empty string, normal ASCII, and a non-ASCII string (v1 default accepts all).
func TestASCIIItem(t *testing.T) {
	t.Parallel()

	tests := []struct {
		description     string
		input           string
		expectedSize    int
		expectedToBytes []byte
		expectedToSML   string
	}{
		{
			description:     "empty string",
			input:           "",
			expectedSize:    0,
			expectedToBytes: []byte{0x41, 0x00},
			expectedToSML:   `<A[0] "">`,
		},
		{
			description:     "single character",
			input:           "A",
			expectedSize:    1,
			expectedToBytes: []byte{0x41, 0x01, 0x41},
			expectedToSML:   `<A[1] "A">`,
		},
		{
			description:     "hello world",
			input:           "hello world",
			expectedSize:    11,
			expectedToBytes: []byte{0x41, 0x0b, 0x68, 0x65, 0x6c, 0x6c, 0x6f, 0x20, 0x77, 0x6f, 0x72, 0x6c, 0x64},
			expectedToSML:   `<A[11] "hello world">`,
		},
		{
			// v1 non-strict default accepts non-ASCII bytes without error.
			description:     "non-ASCII bytes accepted (non-strict default)",
			input:           "caf\xe9", // 'é' as Latin-1, non-ASCII byte 0xe9
			expectedSize:    4,
			expectedToBytes: []byte{0x41, 0x04, 0x63, 0x61, 0x66, 0xe9},
			expectedToSML:   "<A[4] \"caf\xe9\">",
		},
		{
			// control characters pass through in non-strict mode
			description:     "control character passes through",
			input:           "\n",
			expectedSize:    1,
			expectedToBytes: []byte{0x41, 0x01, 0x0a},
			expectedToSML:   "<A[1] \"\n\">",
		},
	}

	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			t.Parallel()

			require := require.New(t)

			item := NewASCIIItem(test.input)
			require.NoError(item.Error())
			require.Equal(test.expectedSize, item.Size())
			require.Equal(test.expectedToBytes, item.ToBytes())
			require.Equal(test.expectedToSML, item.ToSML())

			got, err := item.ToASCII()
			require.NoError(err)
			require.Equal(test.input, got)
		})
	}
}

// TestASCIIItem_AppendTo verifies AppendTo with an independent literal-bytes vector.
// "AB" (2 bytes): format byte 0x41 (ASCIIFormatCode 0x10 << 2 | 1 length byte = 0x41),
// length 0x02, then 'A'=0x41, 'B'=0x42.
func TestASCIIItem_AppendTo(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	item := NewASCIIItem("AB")
	prefix := []byte{0xDE, 0xAD}
	itemBytes := []byte{0x41, 0x02, 0x41, 0x42}

	expected := append(slices.Clone(prefix), itemBytes...)
	result := item.AppendTo(slices.Clone(prefix))
	require.Equal(expected, result)
}

// TestASCIIItem_NoLeak confirms that the string returned by ToASCII is immutable:
// converting it to []byte and mutating the copy does not affect the item.
func TestASCIIItem_NoLeak(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	const original = "hello"

	item := NewASCIIItem(original)

	s, err := item.ToASCII()
	require.NoError(err)
	require.Equal(original, s)

	// Mutate a []byte copy of the returned string.
	mutated := []byte(s)
	mutated[0] = 'X'

	// Item's internal state and subsequent reads must be unchanged.
	s2, err := item.ToASCII()
	require.NoError(err)
	require.Equal(original, s2)

	concrete, ok := item.(*ASCIIItem)
	require.True(ok)
	require.Equal(original, concrete.value)
	require.Equal([]byte{0x41, 0x05, 'h', 'e', 'l', 'l', 'o'}, item.ToBytes())
}

// TestASCIIItem_WrongType verifies that wrong-type accessors return errors on an ASCIIItem.
func TestASCIIItem_WrongType(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	item := NewASCIIItem("hello")
	require.NoError(item.Error())

	// Numeric accessors must error.
	_, err := item.ToInt()
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

// TestASCIIItem_TypeAccessors verifies Type() and IsASCII().
func TestASCIIItem_TypeAccessors(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	item := NewASCIIItem("test")
	require.Equal(ASCIIType, item.Type())
	require.True(item.IsASCII())
	require.False(item.IsInt8())
	require.False(item.IsBinary())
	require.False(item.IsBoolean())
	require.False(item.IsList())
}

// TestASCIIItem_Errors verifies the deferred-error behaviour for oversized strings.
func TestASCIIItem_Errors(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	itemErr := &ItemError{}

	// String exceeding MaxByteSize must set a deferred error.
	huge := make([]byte, MaxByteSize+1)
	item := NewASCIIItem(string(huge))
	require.ErrorAs(item.Error(), &itemErr)

	// ToASCII on error item returns empty string and the error.
	s, err := item.ToASCII()
	require.Equal("", s)
	require.Error(err)

	// EncodedLen and AppendTo on error item.
	require.Equal(0, item.EncodedLen())
	dst := []byte{0xAA}
	require.Equal(dst, item.AppendTo(dst))
}

// TestASCIIItem_Concurrency exercises concurrent ToBytes/AppendTo/ToASCII reads under -race.
func TestASCIIItem_Concurrency(t *testing.T) {
	t.Parallel()

	item := NewASCIIItem("concurrent test")

	const goroutines = 50

	var wg sync.WaitGroup

	wg.Add(goroutines)

	for range goroutines {
		go func() {
			defer wg.Done()

			_ = item.ToBytes()

			buf := make([]byte, 0, item.EncodedLen())
			_ = item.AppendTo(buf)

			_, _ = item.ToASCII()

			_ = item.ToSML()
		}()
	}

	wg.Wait()
}

// TestASCIIItem_Allocs asserts allocation counts for the hot encoding and accessor paths.
func TestASCIIItem_Allocs(t *testing.T) {
	item := NewASCIIItem("hello world")

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

// TestASCIIItem_EncodedLen verifies that EncodedLen() == len(ToBytes()) for all cases.
func TestASCIIItem_EncodedLen(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	cases := []string{
		"",
		"A",
		"hello world",
		"caf\xe9", // non-ASCII byte
	}

	for _, s := range cases {
		item := NewASCIIItem(s)
		require.NoError(item.Error())
		require.Equal(item.EncodedLen(), len(item.ToBytes()),
			"EncodedLen must equal len(ToBytes()) for %q", s)
	}

	// Error item: both EncodedLen and len(ToBytes) must be 0.
	huge := make([]byte, MaxByteSize+1)
	errItem := NewASCIIItem(string(huge))
	require.Error(errItem.Error())
	require.Equal(0, errItem.EncodedLen())
	require.Equal(errItem.EncodedLen(), len(errItem.ToBytes()))
}
