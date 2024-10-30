package secs2

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBinaryItem(t *testing.T) {
	tests := []struct {
		description     string // Test case description
		input           []any  // Input
		expectedSize    int    // expected result from Size()
		expectedValues  []byte // expected result from Values()
		expectedToBytes []byte // expected result from ToBytes()
		expectedToSML   string // expected result from String()
	}{
		{
			description:     "Size: 0",
			input:           []any{},
			expectedSize:    0,
			expectedValues:  []byte{},
			expectedToBytes: []byte{33, 0},
			expectedToSML:   "<B[0]>",
		},
		{
			description:     "Size: 1, Byte input",
			input:           []any{byte(0)},
			expectedSize:    1,
			expectedValues:  []byte{0},
			expectedToBytes: []byte{33, 1, 0},
			expectedToSML:   "<B[1] 0b0>",
		},
		{
			description:     "Size: 3, Byte input",
			input:           []any{byte(1), byte(2), byte(255)},
			expectedSize:    3,
			expectedValues:  []byte{1, 2, 255},
			expectedToBytes: []byte{33, 3, 1, 2, 255},
			expectedToSML:   "<B[3] 0b1 0b10 0b11111111>",
		},
		{
			description:     "Size: 3, Byte slice input",
			input:           []any{[]byte{1, 2, 255}},
			expectedSize:    3,
			expectedValues:  []byte{1, 2, 255},
			expectedToBytes: []byte{33, 3, 1, 2, 255},
			expectedToSML:   "<B[3] 0b1 0b10 0b11111111>",
		},
		{
			description:     "Size: 3, Integer input",
			input:           []any{1, 2, 255},
			expectedSize:    3,
			expectedValues:  []byte{1, 2, 255},
			expectedToBytes: []byte{33, 3, 1, 2, 255},
			expectedToSML:   "<B[3] 0b1 0b10 0b11111111>",
		},
		{
			description:     "Size: 3, Binary string input",
			input:           []any{"0b00", "0b01", "0b11111111"},
			expectedSize:    3,
			expectedValues:  []byte{0, 1, 255},
			expectedToBytes: []byte{33, 3, 0, 1, 255},
			expectedToSML:   "<B[3] 0b0 0b1 0b11111111>",
		},
		{
			description:     "Size: 7, Integer, byte and binary string input",
			input:           []any{1, byte(2), "0b1111", []byte{10, 55, 255}, byte(42)},
			expectedSize:    7,
			expectedValues:  []byte{1, 2, 15, 10, 55, 255, 42},
			expectedToBytes: []byte{33, 7, 1, 2, 15, 10, 55, 255, 42},
			expectedToSML:   "<B[7] 0b1 0b10 0b1111 0b1010 0b110111 0b11111111 0b101010>",
		},
	}

	require := require.New(t)

	for i, test := range tests {
		t.Logf("Test #%d: %s", i, test.description)
		item := NewBinaryItem(test.input...)
		require.NoError(item.Error())
		require.Equal(test.expectedToBytes, item.ToBytes())
		require.Equal(test.expectedSize, item.Size())
		require.Equal(test.expectedToSML, item.ToSML())
		require.Equal(test.expectedValues, item.Values().([]byte))

		val, err := item.ToBinary()
		require.NoError(err)
		require.Equal(test.expectedValues, val)

		nestedItem, err := item.Get()
		require.NoError(err)
		require.Equal(test.expectedValues, nestedItem.Values().([]byte))

		nestedItem, err = item.Get(0)
		require.Nil(nestedItem)
		require.ErrorContains(err, fmt.Sprintf("item is not a list, item is %s", item.ToSML()))

		// create a empty item and call SetVariables
		emptyItem := NewBinaryItem()
		err = emptyItem.SetValues(test.input...)
		require.NoError(err)
		require.Equal(test.expectedSize, emptyItem.Size())
		require.Equal(test.expectedToBytes, emptyItem.ToBytes())
		require.Equal(test.expectedToSML, emptyItem.ToSML())
		require.Equal(test.expectedValues, emptyItem.Values().([]byte))

		// clone a item, it should contains the same content as original item.
		clonedItem := item.Clone()
		require.Equal(test.expectedSize, clonedItem.Size())
		require.Equal(test.expectedToBytes, clonedItem.ToBytes())
		require.Equal(test.expectedToSML, clonedItem.ToSML())
		require.Equal(test.expectedValues, clonedItem.Values().([]byte))

		// set a random string to cloned item
		randVal := genRandomBinary(test.expectedSize)
		err = clonedItem.SetValues(randVal)
		require.NoError(err)
		require.Equal(test.expectedSize, clonedItem.Size())
		require.Equal(randVal, clonedItem.Values().([]byte))

		// the original item should not be modified
		require.Equal(test.expectedValues, item.Values().([]byte))
	}
}

func BenchmarkBinaryItem_Create(b *testing.B) {
	values := genFixedBinary(1000)

	b.ResetTimer()
	for i := 0; i <= b.N; i++ {
		_, _ = NewBinaryItem(values).(*BinaryItem)
	}
	b.StopTimer()
}

func BenchmarkBinaryItem_ToBytes(b *testing.B) {
	values := genFixedBinary(1000)

	item, _ := NewBinaryItem(values).(*BinaryItem)

	b.ResetTimer()
	for i := 0; i <= b.N; i++ {
		_ = item.ToBytes()
	}
	b.StopTimer()
}

func BenchmarkBinaryItem_ToSML(b *testing.B) {
	values := genFixedBinary(1000)
	item := NewBinaryItem(values)

	b.ResetTimer()
	for i := 0; i <= b.N; i++ {
		_ = item.ToSML()
	}
	b.StopTimer()
}

func genRandomBinary(length int) []byte {
	if length == 0 {
		return []byte{}
	}
	seededRand := rand.New(rand.NewSource(time.Now().UnixNano()))

	b := make([]byte, length)
	for i := range b {
		b[i] = byte(seededRand.Intn(256))
	}

	return b
}

func genFixedBinary(length int) []byte {
	if length == 0 {
		return []byte{}
	}
	b := make([]byte, length)
	for i := range b {
		b[i] = byte(i % 0xff)
	}

	return b
}
