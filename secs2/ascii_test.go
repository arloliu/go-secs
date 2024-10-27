package secs2

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestASCIIItem_Create_StrictMode(t *testing.T) {
	require := require.New(t)

	tests := []struct {
		desc            string // Test case description
		input           string // ASCII string input
		expectedSize    int    // Expected result from Size()
		expectedToBytes []byte // Expected result from ToBytes()
		expectedToSML   string // Expected result from SML()
	}{
		{
			desc:            "Length: 0, empty string",
			input:           "",
			expectedSize:    0,
			expectedToBytes: []byte{0x41, 0},
			expectedToSML:   `<A[0]>`,
		},
		{
			desc:            "Length: 1",
			input:           "A",
			expectedSize:    1,
			expectedToBytes: []byte{0x41, 1, 65},
			expectedToSML:   `<A[1] "A">`,
		},
		{
			desc:            "Length: 11",
			input:           "hello world",
			expectedSize:    11,
			expectedToBytes: []byte{0x41, 0xb, 0x68, 0x65, 0x6c, 0x6c, 0x6f, 0x20, 0x77, 0x6f, 0x72, 0x6c, 0x64},
			expectedToSML:   `<A[11] "hello world">`,
		},
		{
			desc:            "Length: 1, non-printable char only",
			input:           "\n",
			expectedSize:    1,
			expectedToBytes: []byte{0x41, 1, 0x0A},
			expectedToSML:   `<A[1] 0x0A>`,
		},
		{
			desc:            "Length: 6, non-printable chars at head",
			input:           "\r\ntext",
			expectedSize:    6,
			expectedToBytes: []byte{0x41, 6, 0x0D, 0x0A, 0x74, 0x65, 0x78, 0x74},
			expectedToSML:   `<A[6] 0x0D 0x0A "text">`,
		},
		{
			desc:            "Length: 6, non-printable chars at tail",
			input:           "text\n\x00",
			expectedSize:    6,
			expectedToBytes: []byte{0x41, 6, 0x74, 0x65, 0x78, 0x74, 0x0A, 0x00},
			expectedToSML:   `<A[6] "text" 0x0A 0x00>`,
		},
		{
			desc:            "Length: 6, non-printable chars in between string",
			input:           "te\x09\x7Fxt",
			expectedSize:    6,
			expectedToBytes: []byte{0x41, 0x6, 0x74, 0x65, 0x09, 0x7F, 0x78, 0x74},
			expectedToSML:   `<A[6] "te" 0x09 0x7F "xt">`,
		},
		{
			desc:            "Length: 7, non-printable chars in between string",
			input:           "te\nxt",
			expectedSize:    5,
			expectedToBytes: []byte{0x41, 0x5, 0x74, 0x65, 0x0a, 0x78, 0x74},
			expectedToSML:   `<A[5] "te" 0x0A "xt">`,
		},
	}

	WithStrictMode(true)
	for i, test := range tests {
		t.Logf("Test #%d: %s", i, test.desc)
		item := NewASCIIItem(test.input)
		require.Equal(test.expectedSize, item.Size())
		require.Equal(test.expectedToBytes, item.ToBytes())
		require.Equal(test.expectedToSML, item.ToSML())

		val, err := item.ToASCII()
		require.NoError(err)
		require.Equal(test.input, val)

		nestedItem, err := item.Get()
		require.NoError(err)
		require.Equal(test.input, nestedItem.Values().(string))

		nestedItem, err = item.Get(0)
		require.Nil(nestedItem)
		require.ErrorContains(err, fmt.Sprintf("item is not a list, item is %s", item.ToSML()))

		// clone a item, it should contains the same content as original item.
		clonedItem := item.Clone()
		require.Equal(test.expectedSize, clonedItem.Size())
		require.Equal(test.expectedToBytes, clonedItem.ToBytes())
		require.Equal(test.expectedToSML, clonedItem.ToSML())
		require.Equal(test.input, clonedItem.Values().(string))

		// set a random string to cloned item
		randVal := genRandomASCIIString(test.expectedSize)
		err = clonedItem.SetValues(randVal)
		require.NoError(err)
		require.Equal(test.expectedSize, clonedItem.Size())
		require.Equal(randVal, clonedItem.Values().(string))

		// the original item should not be modified
		require.Equal(test.input, item.(*ASCIIItem).value)
	}

	data := make([]byte, MaxByteSize+1)
	item := NewASCIIItem(string(data))
	require.Error(item.Error())

	for i := 0; i < 100; i++ {
		item := NewASCIIItem(genRandomASCIIString(i + 1))
		require.NoError(item.Error())
	}
}

func TestASCIIItem_SetValues_StrictMode(t *testing.T) {
	tests := []struct {
		desc            string // Test case description
		input           string // ASCII string input
		setValues       []any  // The argument of SetValues method
		expectedSize    int    // expected result from Size()
		expectedToBytes []byte // expected result from ToBytes()
		expectedToSML   string // expected result from SML()
	}{
		{
			desc:            "Empty input, Single ASCII string are set",
			input:           "",
			setValues:       []any{"hello world"},
			expectedSize:    11,
			expectedToBytes: []byte{0x41, 0xb, 0x68, 0x65, 0x6c, 0x6c, 0x6f, 0x20, 0x77, 0x6f, 0x72, 0x6c, 0x64},
			expectedToSML:   `<A[11] "hello world">`,
		},
		{
			desc:            "Empty input, 3 ASCII strings are set",
			input:           "",
			setValues:       []any{"hello", " ", "world"},
			expectedSize:    11,
			expectedToBytes: []byte{0x41, 0xb, 0x68, 0x65, 0x6c, 0x6c, 0x6f, 0x20, 0x77, 0x6f, 0x72, 0x6c, 0x64},
			expectedToSML:   `<A[11] "hello world">`,
		},
		{
			desc:            "Empty input, non-printable chars in between string",
			input:           "",
			setValues:       []any{"te", "\n", "st"},
			expectedSize:    5,
			expectedToBytes: []byte{0x41, 0x5, 0x74, 0x65, 0x0a, 0x73, 0x74},
			expectedToSML:   `<A[5] "te" 0x0A "st">`,
		},
	}

	require := require.New(t)

	WithStrictMode(true)
	for i, test := range tests {
		t.Logf("Test #%d: %s", i, test.desc)
		item := NewASCIIItem(test.input)
		err := item.SetValues(test.setValues...)
		require.NoError(err)
		require.Equal(test.expectedSize, item.Size())
		require.Equal(test.expectedToBytes, item.ToBytes())
		require.Equal(test.expectedToSML, item.ToSML())
	}
}

func TestASCIIItem_Errors_StrictMode(t *testing.T) {
	WithStrictMode(true)

	require := require.New(t)

	itemErr := &ItemError{}
	nonAsciiBytes := []byte{0xE4, 0xB8, 0xAD, 0xE6, 0x96, 0x87}

	item := NewASCIIItem(string(nonAsciiBytes))
	require.ErrorAs(item.Error(), &itemErr)
	require.Equal("", item.Values().(string))

	item = NewASCIIItem("test")
	err := item.SetValues(100)
	require.ErrorAs(err, &itemErr)
	require.Equal("test", item.Values().(string))
}

func BenchmarkASCIIItem_Create(b *testing.B) {
	WithStrictMode(true)

	values := genRandomASCIIString(1000)

	b.ResetTimer()
	for i := 0; i <= b.N; i++ {
		_, _ = NewASCIIItem(values).(*ASCIIItem)
	}
	b.StopTimer()
}

func BenchmarkASCIIItem_ToBytes(b *testing.B) {
	WithStrictMode(true)

	values := genRandomASCIIString(1000)

	item, _ := NewASCIIItem(values).(*ASCIIItem)

	b.ResetTimer()
	for i := 0; i <= b.N; i++ {
		_ = item.ToBytes()
	}
	b.StopTimer()
}

func BenchmarkASCIIItem_ToSML_StrictMode(b *testing.B) {
	WithStrictMode(true)

	values := genRandomASCIIString(1000)

	item, _ := NewASCIIItem(values).(*ASCIIItem)

	b.ResetTimer()
	for i := 0; i <= b.N; i++ {
		_ = item.ToSML()
	}
	b.StopTimer()
}

func BenchmarkASCIIItem_ToSML_NonStrictMode(b *testing.B) {
	WithStrictMode(false)

	values := genRandomASCIIString(1000)

	item, _ := NewASCIIItem(values).(*ASCIIItem)

	b.ResetTimer()
	for i := 0; i <= b.N; i++ {
		_ = item.ToSML()
	}
	b.StopTimer()
}

func genRandomASCIIString(length int) string {
	if length == 0 {
		return ""
	}

	var seededRand *rand.Rand = rand.New(rand.NewSource(time.Now().UnixNano()))

	randomBytes := make([]byte, length)
	_, err := seededRand.Read(randomBytes)
	if err != nil {
		return ""
	}

	// Convert each byte to a printable ASCII character and insert some new lines
	const printableRange = 127 - 20 + 1 // 96 printable characters
	for i := range randomBytes {
		if (i + 1%10) == 0 {
			if i%2 == 0 {
				randomBytes[i] = '\n'
			} else {
				randomBytes[i] = '\r'
			}
		} else {
			randomBytes[i] = 20 + randomBytes[i]%printableRange
		}
	}

	return string(randomBytes)
}
