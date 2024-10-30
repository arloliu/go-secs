package secs2

import (
	"crypto/rand"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIntItem(t *testing.T) {
	tests := []struct {
		description     string  // Test case description
		input           []any   // Input
		byteSize        int     // the byte size of IntItem
		expectedSize    int     // expected result from Size()
		expectedValues  []int64 // expected result from Values()
		expectedToBytes []byte  // expected result from ToBytes()
		expectedToSML   string  // expected result from String()
	}{
		{
			description:     "Byte size: 1, data size: 0",
			input:           []any{},
			byteSize:        1,
			expectedSize:    0,
			expectedValues:  []int64{},
			expectedToBytes: []byte{0x65, 0},
			expectedToSML:   "<I1[0]>",
		},
		{
			description:     "Byte size: 1, data size: 3",
			input:           []any{-1, 0, 1},
			byteSize:        1,
			expectedSize:    3,
			expectedValues:  []int64{-1, 0, 1},
			expectedToBytes: []byte{0x65, 3, 0xff, 0, 1},
			expectedToSML:   "<I1[3] -1 0 1>",
		},
		{
			description:     "Byte size: 2, data size: 3",
			input:           []any{math.MinInt16, 0, math.MaxInt16},
			byteSize:        2,
			expectedSize:    3,
			expectedValues:  []int64{math.MinInt16, 0, math.MaxInt16},
			expectedToBytes: []byte{0x69, 0x6, 0x80, 0x0, 0x0, 0x0, 0x7f, 0xff},
			expectedToSML:   "<I2[3] -32768 0 32767>",
		},
		{
			description:     "Byte size: 4, data size: 3",
			input:           []any{math.MinInt32, 0, math.MaxInt32},
			byteSize:        4,
			expectedSize:    3,
			expectedValues:  []int64{math.MinInt32, 0, math.MaxInt32},
			expectedToBytes: []byte{0x71, 0xc, 0x80, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x7f, 0xff, 0xff, 0xff},
			expectedToSML:   "<I4[3] -2147483648 0 2147483647>",
		},
		{
			description:    "Byte size: 8, data size: 3",
			input:          []any{math.MinInt64, 0, math.MaxInt64},
			byteSize:       8,
			expectedSize:   3,
			expectedValues: []int64{math.MinInt64, 0, math.MaxInt64},
			expectedToBytes: []byte{
				0x61, 0x18,
				0x80, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0,
				0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0,
				0x7f, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
			},
			expectedToSML: "<I8[3] -9223372036854775808 0 9223372036854775807>",
		},
		{
			description:     "Byte size: 2, integer string",
			input:           []any{"-255", "0", "255"},
			byteSize:        2,
			expectedSize:    3,
			expectedValues:  []int64{-255, 0, 255},
			expectedToBytes: []byte{0x69, 0x6, 0xff, 0x1, 0x0, 0x0, 0x0, 0xff},
			expectedToSML:   "<I2[3] -255 0 255>",
		},
	}

	require := require.New(t)

	for i, test := range tests {
		t.Logf("Test #%d: %s", i, test.description)
		item := NewIntItem(test.byteSize, test.input...)
		require.NoError(item.Error())
		require.Equal(test.expectedToBytes, item.ToBytes())
		require.Equal(test.expectedSize, item.Size())
		require.Equal(test.expectedToSML, item.ToSML())
		require.Equal(test.expectedValues, item.Values().([]int64))

		val, err := item.ToInt()
		require.NoError(err)
		require.Equal(test.expectedValues, val)

		nestedItem, err := item.Get()
		require.NoError(err)
		require.Equal(test.expectedValues, nestedItem.Values().([]int64))

		nestedItem, err = item.Get(0)
		require.Nil(nestedItem)
		require.ErrorContains(err, fmt.Sprintf("item is not a list, item is %s", item.ToSML()))

		// create a empty item and call SetVariables
		emptyItem := NewIntItem(test.byteSize)
		err = emptyItem.SetValues(test.input...)
		require.NoError(err)
		require.Equal(test.expectedSize, emptyItem.Size())
		require.Equal(test.expectedToBytes, emptyItem.ToBytes())
		require.Equal(test.expectedToSML, emptyItem.ToSML())
		require.Equal(test.expectedValues, emptyItem.Values().([]int64))

		// clone a item, it should contains the same content as original item.
		clonedItem := item.Clone()
		require.Equal(test.expectedSize, clonedItem.Size())
		require.Equal(test.expectedToBytes, clonedItem.ToBytes())
		require.Equal(test.expectedToSML, clonedItem.ToSML())
		require.Equal(test.expectedValues, clonedItem.Values().([]int64))

		// set a random string to cloned item
		randVal := genRandomInt(test.expectedSize, test.byteSize)
		err = clonedItem.SetValues(randVal)
		require.NoError(err)
		require.Equal(test.expectedSize, clonedItem.Size())
		require.Equal(randVal, clonedItem.Values().([]int64))

		// the original item should not be modified
		require.Equal(test.expectedValues, item.Values().([]int64))
	}
}

func TestIntItem_SetValues(t *testing.T) {
	tests := []struct {
		description     string  // Test case description
		input           []any   // Input
		byteSize        int     // the byte size of IntItem
		setValues       []any   // The argument of SetValues method
		expectedValues  []int64 // expected result from Values() and ToInt()
		expectedToBytes []byte  // expected result from ToBytes()
		expectedToSML   string  // expected result from SML()
	}{
		{
			description:     "Empty input, Set integers",
			input:           []any{},
			byteSize:        1,
			setValues:       []any{1, 2, 3},
			expectedValues:  []int64{1, 2, 3},
			expectedToBytes: []byte{0x65, 0x3, 0x1, 0x2, 0x3},
			expectedToSML:   `<I1[3] 1 2 3>`,
		},
		{
			description:     "Empty input, Set integer slice and integers",
			input:           []any{},
			byteSize:        1,
			setValues:       []any{1, []int8{2, 3}, 4},
			expectedValues:  []int64{1, 2, 3, 4},
			expectedToBytes: []byte{0x65, 0x4, 0x1, 0x2, 0x3, 0x4},
			expectedToSML:   `<I1[4] 1 2 3 4>`,
		},
	}

	require := require.New(t)

	for i, test := range tests {
		t.Logf("Test #%d: %s", i, test.description)
		item := NewIntItem(test.byteSize, test.input...)
		err := item.SetValues(test.setValues...)
		require.NoError(err)

		result, err := item.ToInt()
		require.NoError(err)
		require.Equal(test.expectedValues, result)
		require.Equal(test.expectedValues, item.Values().([]int64))
		require.Equal(test.expectedToBytes, item.ToBytes())
		require.Equal(test.expectedToSML, item.ToSML())
	}
}

func TestIntItem_Errors(t *testing.T) {
	require := require.New(t)

	itemErr := &ItemError{}

	tests := []struct {
		byteSize      int
		overflowValue uint64
	}{
		{byteSize: 1, overflowValue: math.MaxInt8 + 1},
		{byteSize: 2, overflowValue: math.MaxInt16 + 1},
		{byteSize: 4, overflowValue: math.MaxInt32 + 1},
		{byteSize: 8, overflowValue: math.MaxInt64 + 1},
	}

	for _, test := range tests {
		item := NewIntItem(test.byteSize, test.overflowValue)
		require.ErrorAs(item.Error(), &itemErr)
	}

	var item Item
	item = NewIntItem(3)
	require.ErrorAs(item.Error(), &itemErr)

	item = NewIntItem(8, "invalid_int")
	require.ErrorAs(item.Error(), &itemErr)

	item = NewIntItem(8)
	err := item.SetValues("invalid_int")
	require.ErrorAs(err, &itemErr)
	require.ErrorAs(item.Error(), &itemErr)

	item = NewIntItem(1)
	require.NoError(item.Error())
	result, err := item.Get(0)
	require.Nil(result)
	require.ErrorAs(err, &itemErr)
	require.ErrorAs(item.Error(), &itemErr)
	require.EqualError(err, item.Error().Error())
}

func TestCombineIntValues(t *testing.T) {
	tests := []struct {
		name      string
		byteSize  int
		values    []any
		want      []int64
		wantErr   bool
		errorText string
	}{
		{
			name:     "Signed Integers",
			byteSize: 8,
			values: []any{
				int(-10),
				[]int{20, -30},
				int8(-40),
				[]int8{50, -60},
				int16(-70),
				[]int16{80, -90},
				int32(-100),
				[]int32{110, -120},
				int64(-130),
				[]int64{140, -150},
			},
			want: []int64{-10, 20, -30, -40, 50, -60, -70, 80, -90, -100, 110, -120, -130, 140, -150},
		},
		{
			name:     "Unsigned Integers (within int64 range)",
			byteSize: 4,
			values: []any{
				uint(1),
				[]uint{2, 3},
				uint8(4),
				[]uint8{5, 6},
				uint16(7),
				[]uint16{8, 9},
				uint32(10),
				[]uint32{11, 12},
				uint64(13),       // Within int64 range
				[]uint64{14, 15}, // Within int64 range
			},
			want: []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
		},
		{
			name:      "Overflow Uint (exceeds int64 range)",
			byteSize:  4,
			values:    []any{uint64(math.MaxUint64)},
			wantErr:   true,
			errorText: "value overflow",
		},
		{
			name:      "Overflow Int",
			byteSize:  1,
			values:    []any{int(128)},
			wantErr:   true,
			errorText: "value overflow",
		},
		{
			name:      "Overflow Int in Slice",
			byteSize:  2,
			values:    []any{[]int32{32768}},
			wantErr:   true,
			errorText: "value overflow",
		},
		{
			name:     "String to Int",
			byteSize: 4,
			values:   []any{"-12345", []string{"67890", "-13579"}},
			want:     []int64{-12345, 67890, -13579},
		},
		{
			name:      "Invalid String",
			byteSize:  2,
			values:    []any{"abc"},
			wantErr:   true,
			errorText: `strconv.ParseInt: parsing "abc": invalid syntax`,
		},
		{
			name:      "Unsupported Type",
			byteSize:  1,
			values:    []any{3.14159},
			wantErr:   true,
			errorText: "input argument contains invalid type for IntItem",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			item, _ := NewIntItem(test.byteSize).(*IntItem)
			err := item.combineIntValues(test.values...)
			if (err != nil) != test.wantErr {
				t.Errorf("combineIntValues() error = %v, wantErr %v", err, test.wantErr)
				return
			}

			if test.wantErr && err.Error() != test.errorText {
				t.Errorf("combineIntValues() error text = %v, wantErrorText %v", err.Error(), test.errorText)
				return
			}
			got, _ := item.ToInt()

			if !test.wantErr && !reflect.DeepEqual(got, test.want) {
				t.Errorf("combineIntValues() = %v, want %v", got, test.want)
			}
		})
	}
}

func BenchmarkIntItem_Create(b *testing.B) {
	values := genFixedInt(1000)

	b.ResetTimer()
	for i := 0; i <= b.N; i++ {
		_, _ = NewIntItem(8, values).(*IntItem)
	}
	b.StopTimer()
}

func BenchmarkIntItem_ToBytes(b *testing.B) {
	values := genFixedInt(1000)

	item, _ := NewIntItem(8, values).(*IntItem)

	b.ResetTimer()
	for i := 0; i <= b.N; i++ {
		_ = item.ToBytes()
	}
	b.StopTimer()
}

func BenchmarkIntItem_ToSML(b *testing.B) {
	values := genFixedInt(1000)
	item := NewIntItem(8, values)

	b.ResetTimer()
	for i := 0; i <= b.N; i++ {
		_ = item.ToSML()
	}
	b.StopTimer()
}

func genRandomInt(length int, intSize int) []int64 {
	result := make([]int64, length)

	for i := 0; i < length; i++ {
		// Calculate the maximum and minimum values based on intSize
		maxVal := big.NewInt(1)
		maxVal.Lsh(maxVal, uint(intSize*8-1))
		maxVal.Sub(maxVal, big.NewInt(1))

		minVal := big.NewInt(0)
		minVal.Sub(maxVal, big.NewInt(1))
		minVal.Neg(minVal)

		// Generate a random big integer within the range
		n, _ := rand.Int(rand.Reader, maxVal)

		// Adjust to include negative values
		n.Add(n, minVal)

		// Convert to int64 and store in the slice
		result[i] = n.Int64()
	}

	return result
}

func genFixedInt(length int) []int64 {
	result := make([]int64, length)

	for i := 0; i < length; i++ {
		result[i] = int64(i % math.MaxInt64)
	}

	return result
}
