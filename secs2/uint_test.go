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

func TestUintItem(t *testing.T) {
	tests := []struct {
		description     string   // Test case description
		input           []any    // Input
		byteSize        int      // the byte size of UintItem
		expectedSize    int      // expected result from Size()
		expectedValues  []uint64 // expected result from Values()
		expectedToBytes []byte   // expected result from ToBytes()
		expectedToSML   string   // expected result from String()
	}{
		{
			description:     "Byte size: 1, data size: 0",
			input:           []any{},
			byteSize:        1,
			expectedSize:    0,
			expectedValues:  []uint64{},
			expectedToBytes: []byte{0xa5, 0}, // 'u' for unsigned
			expectedToSML:   "<U1[0]>",
		},
		{
			description:     "Byte size: 1, data size: 3",
			input:           []any{0, 1, math.MaxUint8},
			byteSize:        1,
			expectedSize:    3,
			expectedValues:  []uint64{0, 1, math.MaxUint8},
			expectedToBytes: []byte{0xa5, 3, 0x0, 0x1, 0xff},
			expectedToSML:   "<U1[3] 0 1 255>",
		},
		{
			description:     "Byte size: 2, data size: 3",
			input:           []any{0, 1, math.MaxUint16},
			byteSize:        2,
			expectedSize:    3,
			expectedValues:  []uint64{0, 1, math.MaxUint16},
			expectedToBytes: []byte{0xa9, 0x6, 0x0, 0x0, 0x0, 0x1, 0xff, 0xff},
			expectedToSML:   "<U2[3] 0 1 65535>",
		},
		{
			description:     "Byte size: 4, data size: 3",
			input:           []any{0, 1, math.MaxUint32},
			byteSize:        4,
			expectedSize:    3,
			expectedValues:  []uint64{0, 1, math.MaxUint32},
			expectedToBytes: []byte{0xb1, 0xc, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x1, 0xff, 0xff, 0xff, 0xff},
			expectedToSML:   "<U4[3] 0 1 4294967295>",
		},
		{
			description:    "Byte size: 8, data size: 3",
			input:          []any{0, 1, uint64(math.MaxUint64)},
			byteSize:       8,
			expectedSize:   3,
			expectedValues: []uint64{0, 1, math.MaxUint64},
			expectedToBytes: []byte{
				0xa1, 0x18,
				0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0,
				0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x1,
				0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
			},
			expectedToSML: "<U8[3] 0 1 18446744073709551615>",
		},
		{
			description:     "Byte size: 2, unsigned integer string",
			input:           []any{"0", "255", "65535"},
			byteSize:        2,
			expectedSize:    3,
			expectedValues:  []uint64{0, 255, 65535},
			expectedToBytes: []byte{0xa9, 0x6, 0x0, 0x0, 0x0, 0xff, 0xff, 0xff},
			expectedToSML:   "<U2[3] 0 255 65535>",
		},
		{
			description:     "Byte size: 4, non-negative signed integers",
			input:           []any{0, 10, 2147483647}, // Max int32
			byteSize:        4,
			expectedSize:    3,
			expectedValues:  []uint64{0, 10, 2147483647},
			expectedToBytes: []byte{0xb1, 0x0c, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x0a, 0x7f, 0xff, 0xff, 0xff},
			expectedToSML:   "<U4[3] 0 10 2147483647>",
		},
		{
			description:     "Byte size: 4, mixed input types",
			input:           []any{uint32(100), []int{200, 300}, "400"},
			byteSize:        4,
			expectedSize:    4,
			expectedValues:  []uint64{100, 200, 300, 400},
			expectedToBytes: []byte{0xb1, 0x10, 0x0, 0x0, 0x0, 0x64, 0x0, 0x0, 0x0, 0xc8, 0x0, 0x0, 0x1, 0x2c, 0x0, 0x0, 0x1, 0x90},
			expectedToSML:   "<U4[4] 100 200 300 400>",
		},
	}

	require := require.New(t)

	for i, test := range tests {
		t.Logf("Test #%d: %s", i, test.description)
		item := NewUintItem(test.byteSize, test.input...)
		require.NoError(item.Error())
		require.Equal(test.expectedToBytes, item.ToBytes())
		require.Equal(test.expectedSize, item.Size())
		require.Equal(test.expectedToSML, item.ToSML())
		require.Equal(test.expectedValues, item.Values().([]uint64))

		val, err := item.ToUint()
		require.NoError(err)
		require.Equal(test.expectedValues, val)

		nestedItem, err := item.Get()
		require.NoError(err)
		require.Equal(test.expectedValues, nestedItem.Values().([]uint64))

		nestedItem, err = item.Get(0)
		require.Nil(nestedItem)
		require.ErrorContains(err, fmt.Sprintf("item is not a list, item is %s", item.ToSML()))

		// create a empty item and call SetVariables
		emptyItem := NewUintItem(test.byteSize)
		err = emptyItem.SetValues(test.input...)
		require.NoError(err)
		require.Equal(test.expectedSize, emptyItem.Size())
		require.Equal(test.expectedToBytes, emptyItem.ToBytes())
		require.Equal(test.expectedToSML, emptyItem.ToSML())
		require.Equal(test.expectedValues, emptyItem.Values().([]uint64))

		// clone a item, it should contains the same content as original item.
		clonedItem := item.Clone()
		require.Equal(test.expectedSize, clonedItem.Size())
		require.Equal(test.expectedToBytes, clonedItem.ToBytes())
		require.Equal(test.expectedToSML, clonedItem.ToSML())
		require.Equal(test.expectedValues, clonedItem.Values().([]uint64))

		// set a random string to cloned item
		randVal := genRandomUint(test.expectedSize, test.byteSize)
		err = clonedItem.SetValues(randVal)
		require.NoError(err)
		require.Equal(test.expectedSize, clonedItem.Size())
		require.Equal(randVal, clonedItem.Values().([]uint64))

		// the original item should not be modified
		require.Equal(test.expectedValues, item.Values().([]uint64))
	}
}

func TestUintItem_SetValues(t *testing.T) {
	tests := []struct {
		description     string   // Test case description
		input           []any    // Input
		byteSize        int      // the byte size of UintItem
		setValues       []any    // The argument of SetValues method
		expectedValues  []uint64 // expected result from Values() and ToUint()
		expectedToBytes []byte   // expected result from ToBytes()
		expectedToSML   string   // expected result from SML()
	}{
		{
			description:     "Empty input, Set unsigned integers",
			input:           []any{},
			byteSize:        1,
			setValues:       []any{1, 2, 3},
			expectedValues:  []uint64{1, 2, 3},
			expectedToBytes: []byte{0xa5, 0x3, 0x1, 0x2, 0x3},
			expectedToSML:   `<U1[3] 1 2 3>`,
		},
		{
			description:     "Empty input, Set unsigned integer slice and integers",
			input:           []any{},
			byteSize:        1,
			setValues:       []any{1, []uint8{2, 3}, 4},
			expectedValues:  []uint64{1, 2, 3, 4},
			expectedToBytes: []byte{0xa5, 0x4, 0x1, 0x2, 0x3, 0x4},
			expectedToSML:   `<U1[4] 1 2 3 4>`,
		},
		{
			description:     "Set non-negative signed integers",
			input:           []any{},
			byteSize:        4,
			setValues:       []any{0, 10, 2147483647},
			expectedValues:  []uint64{0, 10, 2147483647},
			expectedToBytes: []byte{0xb1, 0x0c, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x0a, 0x7f, 0xff, 0xff, 0xff},
			expectedToSML:   "<U4[3] 0 10 2147483647>",
		},
		{
			description:     "Set values with mixed types",
			input:           []any{},
			byteSize:        2,
			setValues:       []any{uint16(1000), []int{2000, 3000}, "4000"},
			expectedValues:  []uint64{1000, 2000, 3000, 4000},
			expectedToBytes: []byte{0xa9, 0x8, 0x3, 0xe8, 0x7, 0xd0, 0xb, 0xb8, 0xf, 0xa0},
			expectedToSML:   "<U2[4] 1000 2000 3000 4000>",
		},
	}

	require := require.New(t)

	for i, test := range tests {
		t.Logf("Test #%d: %s", i, test.description)
		item := NewUintItem(test.byteSize, test.input...)
		err := item.SetValues(test.setValues...)
		require.NoError(err)

		result, err := item.ToUint()
		require.NoError(err)
		require.Equal(test.expectedValues, result)
		require.Equal(test.expectedValues, item.Values().([]uint64))
		require.Equal(test.expectedToBytes, item.ToBytes())
		require.Equal(test.expectedToSML, item.ToSML())
	}
}

func TestUintItem_Errors(t *testing.T) {
	require := require.New(t)

	itemErr := &ItemError{}

	tests := []struct {
		byteSize      int
		overflowValue uint64
	}{
		{byteSize: 1, overflowValue: math.MaxUint8 + 1},
		{byteSize: 2, overflowValue: math.MaxUint16 + 1},
		{byteSize: 4, overflowValue: math.MaxUint32 + 1},
		// No overflow test for byteSize 8 as uint64 can hold any uint64 value
	}

	for _, test := range tests {
		item := NewUintItem(test.byteSize, test.overflowValue)
		require.ErrorAs(item.Error(), &itemErr)
	}

	var item Item
	item = NewUintItem(3) // Invalid byteSize
	require.ErrorAs(item.Error(), &itemErr)

	item = NewUintItem(8, "invalid_uint")
	require.ErrorAs(item.Error(), &itemErr)

	item = NewUintItem(8)
	err := item.SetValues("invalid_uint")
	require.ErrorAs(err, &itemErr)
	require.ErrorAs(item.Error(), &itemErr)
	require.EqualError(err, item.Error().Error())

	item = NewUintItem(1, -5) // Negative signed integer
	require.ErrorAs(item.Error(), &itemErr)

	item = NewUintItem(1)
	require.NoError(item.Error())
	result, err := item.Get(0)
	require.Nil(result)
	require.ErrorAs(err, &itemErr)
	require.ErrorAs(item.Error(), &itemErr)
	require.EqualError(err, item.Error().Error())
}

func TestCombineUintValues(t *testing.T) {
	tests := []struct {
		name      string
		byteSize  int
		values    []any
		want      []uint64
		wantErr   bool
		errorText string
	}{
		{
			name:     "Unsigned Integers",
			byteSize: 8,
			values: []any{
				uint(10),
				[]uint{20, 30},
				uint8(40),
				[]uint8{50, 60},
				uint16(70),
				[]uint16{80, 90},
				uint32(100),
				[]uint32{110, 120},
				uint64(130),
				[]uint64{140, 150},
			},
			want: []uint64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100, 110, 120, 130, 140, 150},
		},
		{
			name:     "Non-negative Signed Integers",
			byteSize: 4,
			values: []any{
				int(1),
				[]int{2, 3},
				int8(4),
				[]int8{5, 6},
				int16(7),
				[]int16{8, 9},
				int32(10),
				[]int32{11, 12},
				int64(13),
				[]int64{14, 15},
			},
			want: []uint64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
		},
		{
			name:      "Negative Signed Integer",
			byteSize:  2,
			values:    []any{int(-1)},
			wantErr:   true,
			errorText: "negative value not allowed for UintItem",
		},
		{
			name:      "Overflow Uint",
			byteSize:  1,
			values:    []any{uint(256)},
			wantErr:   true,
			errorText: "value overflow",
		},
		{
			name:      "Overflow Uint in Slice",
			byteSize:  2,
			values:    []any{[]uint{65536}},
			wantErr:   true,
			errorText: "value overflow",
		},
		{
			name:     "String to Uint",
			byteSize: 4,
			values:   []any{"12345", []string{"67890", "13579"}},
			want:     []uint64{12345, 67890, 13579},
		},
		{
			name:      "Invalid String",
			byteSize:  2,
			values:    []any{"abc"},
			wantErr:   true,
			errorText: `strconv.ParseUint: parsing "abc": invalid syntax`,
		},
		{
			name:      "Unsupported Type",
			byteSize:  1,
			values:    []any{3.14159},
			wantErr:   true,
			errorText: "input argument contains invalid type for UintItem",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			item, _ := NewUintItem(test.byteSize).(*UintItem)
			err := item.combineUintValues(test.values...)
			if (err != nil) != test.wantErr {
				t.Errorf("combineUintValues() error = %v, wantErr %v", err, test.wantErr)
				return
			}

			if test.wantErr && err.Error() != test.errorText {
				t.Errorf("combineUintValues() error text = %v, wantErrorText %v", err.Error(), test.errorText)
				return
			}

			got, _ := item.ToUint()
			if !test.wantErr && !reflect.DeepEqual(got, test.want) {
				t.Errorf("combineUintValues() = %v, want %v", got, test.want)
			}
		})
	}
}

func BenchmarkUintItem_Create(b *testing.B) {
	values := genFixedUint(1000)

	b.ResetTimer()
	for i := 0; i <= b.N; i++ {
		_, _ = NewUintItem(8, values).(*UintItem)
	}
	b.StopTimer()
}

func BenchmarkUintItem_ToBytes(b *testing.B) {
	values := genFixedUint(1000)

	item, _ := NewUintItem(8, values).(*UintItem)

	b.ResetTimer()
	for i := 0; i <= b.N; i++ {
		_ = item.ToBytes()
	}
	b.StopTimer()
}

func BenchmarkUintItem_ToSML(b *testing.B) {
	values := genFixedUint(1000)
	item := NewUintItem(8, values)

	b.ResetTimer()
	for i := 0; i <= b.N; i++ {
		_ = item.ToSML()
	}
	b.StopTimer()
}

func genRandomUint(length int, byteSize int) []uint64 {
	result := make([]uint64, length)

	// Calculate the maximum value based on byteSize
	maxVal := big.NewInt(0).Lsh(big.NewInt(1), uint(byteSize*8))
	maxVal.Sub(maxVal, big.NewInt(1))

	for i := 0; i < length; i++ {
		// Generate a random big integer within the range [0, max]
		n, _ := rand.Int(rand.Reader, maxVal)

		// Convert to uint64 and store in the slice
		result[i] = n.Uint64()
	}

	return result
}

func genFixedUint(length int) []uint64 {
	result := make([]uint64, length)

	for i := 0; i < length; i++ {
		result[i] = uint64(i)
	}

	return result
}
