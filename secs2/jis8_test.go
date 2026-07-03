package secs2

import (
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestJIS8Item covers round-trip construction, exact wire bytes, and SML output.
// Vectors are ported from git show main:secs2/jis8_test.go.
func TestJIS8Item(t *testing.T) {
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
			expectedToBytes: []byte{0x45, 0x00},
			expectedToSML:   `<J[0] "">`,
		},
		{
			description:     "single character",
			input:           "A",
			expectedSize:    1,
			expectedToBytes: []byte{0x45, 0x01, 0x41},
			expectedToSML:   `<J[1] "A">`,
		},
		{
			description:     "hello world",
			input:           "hello world",
			expectedSize:    11,
			expectedToBytes: []byte{0x45, 0x0b, 0x68, 0x65, 0x6c, 0x6c, 0x6f, 0x20, 0x77, 0x6f, 0x72, 0x6c, 0x64},
			expectedToSML:   `<J[11] "hello world">`,
		},
		{
			description:     "newline control character",
			input:           "\n",
			expectedSize:    1,
			expectedToBytes: []byte{0x45, 0x01, 0x0a},
			expectedToSML:   "<J[1] \"\n\">",
		},
		{
			// JIS-8 accepts multi-byte UTF-8; size is byte length, not char count.
			description:     "JIS-8 multibyte string",
			input:           "こんにちは",
			expectedSize:    15,
			expectedToBytes: []byte{0x45, 0x0f, 0xe3, 0x81, 0x93, 0xe3, 0x82, 0x93, 0xe3, 0x81, 0xab, 0xe3, 0x81, 0xa1, 0xe3, 0x81, 0xaf},
			expectedToSML:   `<J[15] "こんにちは">`,
		},
	}

	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			t.Parallel()

			require := require.New(t)

			item := NewJIS8Item(test.input)
			require.NoError(item.Error())
			require.Equal(test.expectedSize, item.Size())
			require.Equal(test.expectedToBytes, item.ToBytes())
			require.Equal(test.expectedToSML, item.ToSML())

			got, err := item.ToJIS8()
			require.NoError(err)
			require.Equal(test.input, got)
		})
	}
}

// TestJIS8Item_AppendTo verifies AppendTo with an independent literal-bytes vector.
// "AB" (2 bytes): format byte 0x45 (JIS8FormatCode 0o21=17, 17<<2|1=69=0x45),
// length 0x02, then 'A'=0x41, 'B'=0x42.
func TestJIS8Item_AppendTo(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	item := NewJIS8Item("AB")
	prefix := []byte{0xDE, 0xAD}
	itemBytes := []byte{0x45, 0x02, 0x41, 0x42}

	expected := append(slices.Clone(prefix), itemBytes...)
	result := item.AppendTo(slices.Clone(prefix))
	require.Equal(expected, result)
}

// TestJIS8Item_NoLeak confirms that the string returned by ToJIS8 is immutable:
// converting it to []byte and mutating the copy does not affect the item.
func TestJIS8Item_NoLeak(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	const original = "hello"

	item := NewJIS8Item(original)

	s, err := item.ToJIS8()
	require.NoError(err)
	require.Equal(original, s)

	// Mutate a []byte copy of the returned string.
	mutated := []byte(s)
	mutated[0] = 'X'

	// Item's internal state and subsequent reads must be unchanged.
	s2, err := item.ToJIS8()
	require.NoError(err)
	require.Equal(original, s2)

	concrete, ok := item.(*JIS8Item)
	require.True(ok)
	require.Equal(original, concrete.value)
	require.Equal([]byte{0x45, 0x05, 'h', 'e', 'l', 'l', 'o'}, item.ToBytes())
}

// TestJIS8Item_WrongType verifies that wrong-type accessors return errors on a JIS8Item.
func TestJIS8Item_WrongType(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	item := NewJIS8Item("hello")
	require.NoError(item.Error())

	// String accessor for a different type must error.
	_, err := item.ToASCII()
	require.Error(err)

	_, err = item.ToLocalizedStr()
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

// TestJIS8Item_TypeAccessors verifies Type() and IsJIS8().
func TestJIS8Item_TypeAccessors(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	item := NewJIS8Item("test")
	require.Equal(JIS8Type, item.Type())
	require.True(item.IsJIS8())
	require.False(item.IsASCII())
	require.False(item.IsLocalizedStr())
	require.False(item.IsInt8())
	require.False(item.IsBinary())
	require.False(item.IsBoolean())
	require.False(item.IsList())
}

// TestJIS8Item_Errors verifies the deferred-error behaviour for oversized strings.
func TestJIS8Item_Errors(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	itemErr := &ItemError{}

	// String exceeding MaxByteSize must set a deferred error.
	huge := make([]byte, MaxByteSize+1)
	item := NewJIS8Item(string(huge))
	require.ErrorAs(item.Error(), &itemErr)

	// ToJIS8 on error item returns empty string and the error.
	s, err := item.ToJIS8()
	require.Equal("", s)
	require.Error(err)

	// EncodedLen and AppendTo on error item.
	require.Equal(0, item.EncodedLen())
	dst := []byte{0xAA}
	require.Equal(dst, item.AppendTo(dst))
}

// TestJIS8Item_Concurrency exercises concurrent ToBytes/AppendTo/ToJIS8 reads under -race.
func TestJIS8Item_Concurrency(t *testing.T) {
	t.Parallel()

	item := NewJIS8Item("concurrent test")

	const goroutines = 50

	var wg sync.WaitGroup

	wg.Add(goroutines)

	for range goroutines {
		go func() {
			defer wg.Done()

			_ = item.ToBytes()

			buf := make([]byte, 0, item.EncodedLen())
			_ = item.AppendTo(buf)

			_, _ = item.ToJIS8()

			_ = item.ToSML()
		}()
	}

	wg.Wait()
}

// TestJIS8Item_Allocs asserts allocation counts for the hot encoding and accessor paths.
func TestJIS8Item_Allocs(t *testing.T) {
	item := NewJIS8Item("hello world")

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

// TestJIS8Item_EncodedLen verifies that EncodedLen() == len(ToBytes()) for all cases.
func TestJIS8Item_EncodedLen(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	cases := []string{
		"",
		"A",
		"hello world",
		"こんにちは", // multi-byte UTF-8
	}

	for _, s := range cases {
		item := NewJIS8Item(s)
		require.NoError(item.Error())
		require.Equal(item.EncodedLen(), len(item.ToBytes()),
			"EncodedLen must equal len(ToBytes()) for %q", s)
	}

	// Error item: both EncodedLen and len(ToBytes) must be 0.
	huge := make([]byte, MaxByteSize+1)
	errItem := NewJIS8Item(string(huge))
	require.Error(errItem.Error())
	require.Equal(0, errItem.EncodedLen())
	require.Equal(errItem.EncodedLen(), len(errItem.ToBytes()))
}
