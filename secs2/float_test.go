//nolint:errcheck
package secs2

import (
	"fmt"
	"math"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFloatItem(t *testing.T) {
	tests := []struct {
		description     string    // Test case description
		input           []any     // Input
		byteSize        int       // The byte size of FloatItem
		expectedSize    int       // Expected result from Size()
		expectedValues  []float64 // Expected result from Values()
		expectedToBytes []byte    // Expected result from ToBytes()
		expectedToSML   string    // Expected result from ToSML()
	}{
		{
			description:     "Byte size: 4, data size: 0",
			input:           []any{},
			byteSize:        4,
			expectedSize:    0,
			expectedValues:  []float64{},
			expectedToBytes: []byte{0x91, 0x0}, // 's' for 4-byte float (single-precision)
			expectedToSML:   "<F4[0]>",
		},
		{
			description:     "Byte size: 4, data size: 3",
			input:           []any{-1.234, 0.0, 3.14159},
			byteSize:        4,
			expectedSize:    3,
			expectedValues:  []float64{-1.234, 0.0, 3.14159},
			expectedToBytes: []byte{0x91, 0xc, 0xbf, 0x9d, 0xf3, 0xb6, 0x00, 0x00, 0x00, 0x00, 0x40, 0x49, 0x0f, 0xd0},
			expectedToSML:   "<F4[3] -1.234 0 3.14159>",
		},
		{
			description:    "Byte size: 8, data size: 3, min/max",
			input:          []any{-math.MaxFloat64, 0.0, math.MaxFloat64},
			byteSize:       8,
			expectedSize:   3,
			expectedValues: []float64{-math.MaxFloat64, 0.0, math.MaxFloat64},
			expectedToBytes: []byte{
				0x81, 0x18,
				0xff, 0xef, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x7f, 0xef, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
			},
			expectedToSML: "<F8[3] -1.7976931348623157e+308 0 1.7976931348623157e+308>",
		},
		{
			description:    "Byte size: 8, float bounary",
			input:          []any{-math.SmallestNonzeroFloat64, -2.0, -math.MaxFloat64},
			byteSize:       8,
			expectedSize:   3,
			expectedValues: []float64{-math.SmallestNonzeroFloat64, -2.0, -math.MaxFloat64},
			expectedToBytes: []byte{
				0x81, 0x18,
				0x80, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01,
				0xC0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0xFF, 0xEF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF,
			},

			expectedToSML: fmt.Sprintf("<F8[3] %g -2 %g>", -math.SmallestNonzeroFloat64, -math.MaxFloat64),
		},
		{
			description:    "Byte size: 8, mixed input types",
			input:          []any{3.14159, []int{1, 2, 3}, "4.567"},
			byteSize:       8,
			expectedSize:   5,
			expectedValues: []float64{3.14159, 1.0, 2.0, 3.0, 4.567},
			expectedToBytes: []byte{
				0x81, 0x28,
				0x40, 0x9, 0x21, 0xf9, 0xf0, 0x1b, 0x86, 0x6e,
				0x3f, 0xf0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0,
				0x40, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0,
				0x40, 0x8, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0,
				0x40, 0x12, 0x44, 0x9b, 0xa5, 0xe3, 0x53, 0xf8,
			},
			expectedToSML: "<F8[5] 3.14159 1 2 3 4.567>",
		},
	}

	require := require.New(t)

	for i, test := range tests {
		t.Logf("Test #%d: %s", i, test.description)
		item := NewFloatItem(test.byteSize, test.input...)
		require.NoError(item.Error())
		require.Equal(test.expectedToBytes, item.ToBytes())
		require.Equal(test.expectedSize, item.Size())
		require.Equal(test.expectedToSML, item.ToSML())
		require.Equal(test.expectedValues, item.Values().([]float64))

		val, err := item.ToFloat()
		require.NoError(err)
		require.Equal(test.expectedValues, val)

		nestedItem, err := item.Get()
		require.NoError(err)
		require.Equal(test.expectedValues, nestedItem.Values().([]float64))

		nestedItem, err = item.Get(0)
		require.Nil(nestedItem)
		require.ErrorContains(err, fmt.Sprintf("item is not a list, item is %s", item.ToSML()))

		// create a empty item and call SetVariables
		emptyItem := NewFloatItem(test.byteSize)
		err = emptyItem.SetValues(test.input...)
		require.NoError(err)
		require.Equal(test.expectedSize, emptyItem.Size())
		require.Equal(test.expectedToBytes, emptyItem.ToBytes())
		require.Equal(test.expectedToSML, emptyItem.ToSML())
		require.Equal(test.expectedValues, emptyItem.Values().([]float64))

		// clone a item, it should contains the same content as original item.
		clonedItem := item.Clone()
		require.Equal(test.expectedSize, clonedItem.Size())
		require.Equal(test.expectedToBytes, clonedItem.ToBytes())
		require.Equal(test.expectedToSML, clonedItem.ToSML())
		require.Equal(test.expectedValues, clonedItem.Values().([]float64))

		// set a random string to cloned item
		randVal := genRandomFloat(test.expectedSize, test.byteSize)
		err = clonedItem.SetValues(randVal)
		require.NoError(err)
		require.Equal(test.expectedSize, clonedItem.Size())
		require.Equal(randVal, clonedItem.Values().([]float64)) // Compare float slices

		// the original item should not be modified
		require.Equal(test.expectedValues, item.Values().([]float64))
	}
}

func TestFloatItem_Errors(t *testing.T) {
	require := require.New(t)

	itemErr := &ItemError{}

	var item Item
	item = NewFloatItem(3) // Invalid byteSize
	require.ErrorAs(item.Error(), &itemErr)

	item = NewFloatItem(4, math.NaN())
	require.ErrorAs(item.Error(), &itemErr)

	item = NewFloatItem(4)
	require.NoError(item.Error())
	result, err := item.Get(0)
	require.Nil(result)
	require.ErrorAs(err, &itemErr)
	require.ErrorAs(item.Error(), &itemErr)
	require.EqualError(err, item.Error().Error())
}

func BenchmarkFloatItem_Create(b *testing.B) {
	values := genFixedFloat(1000, 8)

	b.ResetTimer()
	for i := 0; i <= b.N; i++ {
		_, _ = NewFloatItem(8, values).(*FloatItem)
	}
	b.StopTimer()
}

func BenchmarkFloatItem_ToBytes(b *testing.B) {
	values := genFixedFloat(1000, 8)

	item, _ := NewFloatItem(8, values).(*FloatItem)

	b.ResetTimer()
	for i := 0; i <= b.N; i++ {
		_ = item.ToBytes()
	}
	b.StopTimer()
}

func BenchmarkFloatItem_ToSML(b *testing.B) {
	values := genFixedFloat(1000, 8)
	item := NewFloatItem(8, values)

	b.ResetTimer()
	for i := 0; i <= b.N; i++ {
		_ = item.ToSML()
	}
	b.StopTimer()
}

func genRandomFloat(length int, byteSize int) []float64 {
	result := make([]float64, length)

	var minVal float64
	var maxVal float64
	if byteSize == 4 {
		minVal = -math.MaxFloat32 / 2
		maxVal = math.MaxFloat32 / 2
	} else {
		minVal = -math.MaxFloat64 / 2
		maxVal = math.MaxFloat64 / 2
	}

	for i := 0; i < length; i++ {
		// Generate a random float64 between -max and max
		result[i] = rand.Float64()*(maxVal-minVal) + minVal
	}

	return result
}

func genFixedFloat(length int, byteSize int) []float64 {
	result := make([]float64, length)

	var maxVal float64
	if byteSize == 4 {
		maxVal = math.MaxFloat32
	} else {
		maxVal = math.MaxFloat64
	}

	for i := 0; i < length; i++ {
		result[i] = float64(i) / float64(length) * maxVal
	}

	return result
}
