package secs2

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestJIS8Item_Create(t *testing.T) {
	require := require.New(t)

	tests := []struct {
		desc            string // Test case description
		input           string // JIS-8 string input
		expectedSize    int    // Expected result from Size()
		expectedToBytes []byte // Expected result from ToBytes()
		expectedToSML   string // Expected result from SML()
	}{
		{
			desc:            "Length: 0, empty string",
			input:           "",
			expectedSize:    0,
			expectedToBytes: []byte{0x45, 0},
			expectedToSML:   `<J[0] "">`,
		},
		{
			desc:            "Length: 1",
			input:           "A",
			expectedSize:    1,
			expectedToBytes: []byte{0x45, 1, 65},
			expectedToSML:   `<J[1] "A">`,
		},
		{
			desc:            "Length: 11",
			input:           "hello world",
			expectedSize:    11,
			expectedToBytes: []byte{0x45, 0xb, 0x68, 0x65, 0x6c, 0x6c, 0x6f, 0x20, 0x77, 0x6f, 0x72, 0x6c, 0x64},
			expectedToSML:   `<J[11] "hello world">`,
		},
		{
			desc:            "Length: 1, non-printable char only",
			input:           "\n",
			expectedSize:    1,
			expectedToBytes: []byte{0x45, 1, 0x0A},
			expectedToSML:   "<J[1] \"\n\">",
		},
		{
			desc:            "Length: 15, JIS-8 string",
			input:           "こんにちは",
			expectedSize:    15,
			expectedToBytes: []byte{0x45, 0xf, 0xe3, 0x81, 0x93, 0xe3, 0x82, 0x93, 0xe3, 0x81, 0xab, 0xe3, 0x81, 0xa1, 0xe3, 0x81, 0xaf},
			expectedToSML:   `<J[15] "こんにちは">`,
		},
		{
			desc:            "Length: 6, JIS-8 string with non-printable chars",
			input:           "こん\x09\x7Fにちは",
			expectedSize:    17,
			expectedToBytes: []byte{0x45, 0x11, 0xe3, 0x81, 0x93, 0xe3, 0x82, 0x93, 0x9, 0x7f, 0xe3, 0x81, 0xab, 0xe3, 0x81, 0xa1, 0xe3, 0x81, 0xaf},
			expectedToSML:   "<J[17] \"こん\t\x7fにちは\">",
		},
		{
			desc:            "Length: 5, non-printable chars in between string",
			input:           "te\nxt",
			expectedSize:    5,
			expectedToBytes: []byte{0x45, 0x5, 0x74, 0x65, 0x0a, 0x78, 0x74},
			expectedToSML:   "<J[5] \"te\nxt\">",
		},
	}

	for i, test := range tests {
		t.Logf("Test #%d: %s", i, test.desc)
		item := NewJIS8Item(test.input)
		require.Equal(test.expectedSize, item.Size())
		require.Equal(test.expectedToBytes, item.ToBytes())
		require.Equal(test.expectedToSML, item.ToSML())

		val, err := item.ToJIS8()
		require.NoError(err)
		require.Equal(test.input, val)

		nestedItem, err := item.Get()
		require.NoError(err)
		nestedStr, ok := nestedItem.Values().(string)
		require.True(ok)
		require.Equal(test.input, nestedStr)

		nestedItem, err = item.Get(0)
		require.Nil(nestedItem)
		require.ErrorContains(err, fmt.Sprintf("item is not a list, item is %s", item.ToSML()))

		// clone a item, it should contains the same content as original item.
		clonedItem := item.Clone()
		require.Equal(test.expectedSize, clonedItem.Size())
		require.Equal(test.expectedToBytes, clonedItem.ToBytes())
		require.Equal(test.expectedToSML, clonedItem.ToSML())
		clonedStr, ok := clonedItem.Values().(string)
		require.True(ok)
		require.Equal(test.input, clonedStr)

		// set a random string to cloned item
		randVal := genRandomASCIIString(test.expectedSize)
		err = clonedItem.SetValues(randVal)
		require.NoError(err)
		require.Equal(test.expectedSize, clonedItem.Size())
		clonedStr, ok = clonedItem.Values().(string)
		require.True(ok)
		require.Equal(randVal, clonedStr)

		// the original item should not be modified
		oriStr, ok := item.Values().(string)
		require.True(ok)
		require.Equal(test.input, oriStr)
	}

	data := make([]byte, MaxByteSize+1)
	item := NewJIS8Item(string(data))
	require.Error(item.Error())

	for i := 0; i < 100; i++ {
		item := NewJIS8Item(genRandomASCIIString(i + 1))
		require.NoError(item.Error())
	}
}

func TestJIS8Item_SetValues(t *testing.T) {
	tests := []struct {
		desc            string // Test case description
		input           string // JIS-8 string input
		setValues       []any  // The argument of SetValues method
		expectedSize    int    // expected result from Size()
		expectedToBytes []byte // expected result from ToBytes()
		expectedToSML   string // expected result from SML()
	}{
		{
			desc:            "Empty input, Single JIS-8 string are set",
			input:           "",
			setValues:       []any{"こんにちは 世界"},
			expectedSize:    22,
			expectedToBytes: []byte{0x45, 0x16, 0xe3, 0x81, 0x93, 0xe3, 0x82, 0x93, 0xe3, 0x81, 0xab, 0xe3, 0x81, 0xa1, 0xe3, 0x81, 0xaf, 0x20, 0xe4, 0xb8, 0x96, 0xe7, 0x95, 0x8c},
			expectedToSML:   `<J[22] "こんにちは 世界">`,
		},
		{
			desc:            "Empty input, 3 JIS-8 strings are set",
			input:           "",
			setValues:       []any{"こんにちは", " ", "世界"},
			expectedSize:    22,
			expectedToBytes: []byte{0x45, 0x16, 0xe3, 0x81, 0x93, 0xe3, 0x82, 0x93, 0xe3, 0x81, 0xab, 0xe3, 0x81, 0xa1, 0xe3, 0x81, 0xaf, 0x20, 0xe4, 0xb8, 0x96, 0xe7, 0x95, 0x8c},
			expectedToSML:   `<J[22] "こんにちは 世界">`,
		},
	}

	require := require.New(t)

	for i, test := range tests {
		t.Logf("Test #%d: %s", i, test.desc)
		item := NewJIS8Item(test.input)
		err := item.SetValues(test.setValues...)
		require.NoError(err)
		require.Equal(test.expectedSize, item.Size())
		require.Equal(test.expectedToBytes, item.ToBytes())
		require.Equal(test.expectedToSML, item.ToSML())
	}
}

func BenchmarkJIS8Item_Create(b *testing.B) {
	values := genRandomASCIIString(1000)

	b.ResetTimer()
	for i := 0; i <= b.N; i++ {
		_, _ = NewJIS8Item(values).(*JIS8Item)
	}
	b.StopTimer()
}

func BenchmarkJIS8Item_ToBytes(b *testing.B) {
	values := genRandomASCIIString(1000)

	item, _ := NewJIS8Item(values).(*JIS8Item)

	b.ResetTimer()
	for i := 0; i <= b.N; i++ {
		_ = item.ToBytes()
	}
	b.StopTimer()
}

func BenchmarkJIS8Item_ToSML(b *testing.B) {
	values := genRandomASCIIString(1000)

	item, _ := NewJIS8Item(values).(*JIS8Item)

	b.ResetTimer()
	for i := 0; i <= b.N; i++ {
		_ = item.ToSML()
	}
	b.StopTimer()
}
