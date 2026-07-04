package secs2

import (
	"math"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestIntItem covers round-trip construction, exact wire bytes, and SML output for all four
// byte sizes. Vectors are ported from git show main:secs2/int_test.go.
func TestIntItem(t *testing.T) {
	t.Parallel()

	tests := []struct {
		description     string
		input           []any
		byteSize        int
		expectedSize    int
		expectedValues  []int64
		expectedToBytes []byte
		expectedToSML   string
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
			description:     "Byte size: 2, integer strings",
			input:           []any{"-255", "0", "255"},
			byteSize:        2,
			expectedSize:    3,
			expectedValues:  []int64{-255, 0, 255},
			expectedToBytes: []byte{0x69, 0x6, 0xff, 0x1, 0x0, 0x0, 0x0, 0xff},
			expectedToSML:   "<I2[3] -255 0 255>",
		},
	}

	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			t.Parallel()

			require := require.New(t)

			item := NewIntItem(test.byteSize, test.input...)
			require.NoError(item.Error())
			require.Equal(test.expectedSize, item.Size())
			require.Equal(test.expectedToBytes, item.ToBytes())
			require.Equal(test.expectedToSML, item.ToSML())

			vals, err := item.ToInt()
			require.NoError(err)
			require.Equal(test.expectedValues, vals)
		})
	}
}

// TestIntItem_Clamping verifies that values outside the byte-size range are clamped to the min/max.
// Vectors ported from git show main:secs2/int_test.go TestIntItem_Clamping.
func TestIntItem_Clamping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		byteSize      int
		inputValue    any
		expectedValue int64
	}{
		{name: "I1 max overflow clamps to 127", byteSize: 1, inputValue: int(128), expectedValue: 127},
		{name: "I1 min overflow clamps to -128", byteSize: 1, inputValue: int(-129), expectedValue: -128},
		{name: "I2 max overflow clamps to 32767", byteSize: 2, inputValue: int(32768), expectedValue: 32767},
		{name: "I2 min overflow clamps to -32768", byteSize: 2, inputValue: int(-32769), expectedValue: -32768},
		{name: "I4 max overflow clamps to MaxInt32", byteSize: 4, inputValue: int64(math.MaxInt32 + 1), expectedValue: math.MaxInt32},
		{name: "I4 min overflow clamps to MinInt32", byteSize: 4, inputValue: int64(math.MinInt32 - 1), expectedValue: math.MinInt32},
		{name: "I8 uint64 overflow clamps to MaxInt64", byteSize: 8, inputValue: uint64(math.MaxInt64) + 1, expectedValue: math.MaxInt64},
		{name: "I1 string max overflow clamps to 127", byteSize: 1, inputValue: "128", expectedValue: 127},
		{name: "I1 string min overflow clamps to -128", byteSize: 1, inputValue: "-129", expectedValue: -128},
		{name: "I4 string max overflow clamps to MaxInt32", byteSize: 4, inputValue: "2147483648", expectedValue: math.MaxInt32},
		{name: "I8 string int64 overflow clamps to MaxInt64", byteSize: 8, inputValue: "9223372036854775808", expectedValue: math.MaxInt64},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			require := require.New(t)

			item := NewIntItem(test.byteSize, test.inputValue)
			require.NoError(item.Error())

			val, err := item.ToInt()
			require.NoError(err)
			require.Equal([]int64{test.expectedValue}, val)
		})
	}
}

// TestIntItem_Errors verifies deferred-error behaviour for invalid byte sizes and invalid value types.
func TestIntItem_Errors(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	itemErr := &ItemError{}

	// Invalid byteSize must set a deferred error.
	item := NewIntItem(3)
	require.ErrorAs(item.Error(), &itemErr)

	// float64 is not a supported value type.
	item = NewIntItem(8, 3.14159)
	require.ErrorAs(item.Error(), &itemErr)

	// Non-numeric string must set a deferred error.
	item = NewIntItem(8, "invalid_int")
	require.ErrorAs(item.Error(), &itemErr)
}

// TestIntItem_AppendTo verifies that AppendTo(prefix) == prefix + <exact wire bytes>.
// The expected bytes are computed independently of ToBytes() to avoid a circular dependency.
// NewIntItem(4, 1, 2, 3): format byte 0x71 (I4, 1 length byte), length 0x0C (12 data bytes),
// then three big-endian int32 values 1, 2, 3 — verified against the I4 vector in TestIntItem.
func TestIntItem_AppendTo(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	item := NewIntItem(4, 1, 2, 3)
	prefix := []byte{0xDE, 0xAD}
	itemBytes := []byte{
		0x71, 0x0C,
		0x00, 0x00, 0x00, 0x01,
		0x00, 0x00, 0x00, 0x02,
		0x00, 0x00, 0x00, 0x03,
	}

	expected := append(slices.Clone(prefix), itemBytes...)
	result := item.AppendTo(slices.Clone(prefix))
	require.Equal(expected, result)
}

// TestIntItem_TypeAccessors verifies Type(), IsInt8/16/32/64.
func TestIntItem_TypeAccessors(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	tests := []struct {
		byteSize int
		wantType string
		isInt8   bool
		isInt16  bool
		isInt32  bool
		isInt64  bool
	}{
		{1, Int8Type, true, false, false, false},
		{2, Int16Type, false, true, false, false},
		{4, Int32Type, false, false, true, false},
		{8, Int64Type, false, false, false, true},
	}

	for _, test := range tests {
		item := NewIntItem(test.byteSize)
		require.Equal(test.wantType, item.Type())
		require.Equal(test.isInt8, item.IsInt8())
		require.Equal(test.isInt16, item.IsInt16())
		require.Equal(test.isInt32, item.IsInt32())
		require.Equal(test.isInt64, item.IsInt64())
	}
}

// TestIntItem_NoLeak (teeth test) confirms that mutating the slice returned by ToInt does not
// affect the item's internal state — wire encoding and subsequent ToInt calls are unchanged.
func TestIntItem_NoLeak(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	item := NewIntItem(8, int64(42))
	origBytes := item.ToBytes()

	xs, err := item.ToInt()
	require.NoError(err)
	xs[0]++ // mutate the returned copy

	// Internal state must be unchanged.
	xs2, err := item.ToInt()
	require.NoError(err)
	require.Equal([]int64{42}, xs2)
	require.Equal(origBytes, item.ToBytes())
}

// TestIntItem_Iterator verifies Ints() and IntAt().
func TestIntItem_Iterator(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	// Pass a []int64 slice as a single any — combineIntValues handles []int64 in the fast path.
	values := []int64{-100, 0, 100, math.MaxInt32}
	item := NewIntItem(4, values)

	// Ints() must yield all values in order.
	got := slices.Collect(item.Ints())
	require.Equal(values, got)

	// IntAt valid indices.
	for i, v := range values {
		gotV, err := item.IntAt(i)
		require.NoError(err)
		require.Equal(v, gotV)
	}

	// IntAt out-of-range indices must return an error.
	_, err := item.IntAt(-1)
	require.Error(err)

	_, err = item.IntAt(len(values))
	require.Error(err)

	// Error item: Ints() yields nothing and IntAt returns an error.
	errItem := NewIntItem(3)
	require.Error(errItem.Error())
	require.Empty(slices.Collect(errItem.Ints()))

	_, err = errItem.IntAt(0)
	require.Error(err)
}

// TestIntItem_WrongType verifies that wrong-type accessors and Get return errors on an IntItem.
func TestIntItem_WrongType(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	item := NewIntItem(4, 1, 2, 3)
	require.NoError(item.Error())

	// String accessor must error.
	_, err := item.ToASCII()
	require.Error(err)

	// Bool iterator must yield nothing (baseItem default).
	bools := slices.Collect(item.Bools())
	require.Empty(bools)

	// Get is list-only in v2 — any call on a scalar item must error.
	got, err := item.Get()
	require.Nil(got)
	require.Error(err)

	got, err = item.Get(0)
	require.Nil(got)
	require.Error(err)
}

// TestIntItem_Concurrency exercises concurrent ToBytes / AppendTo / Ints reads under -race.
func TestIntItem_Concurrency(t *testing.T) {
	t.Parallel()

	item := NewIntItem(8, int64(1), int64(2), int64(3))

	const goroutines = 50

	var wg sync.WaitGroup

	wg.Add(goroutines)

	for range goroutines {
		go func() {
			defer wg.Done()

			_ = item.ToBytes()

			buf := make([]byte, 0, item.EncodedLen())
			_ = item.AppendTo(buf)

			_ = slices.Collect(item.Ints())
		}()
	}

	wg.Wait()
}

// TestIntItem_Allocs asserts allocation counts for the hot encoding and accessor paths.
func TestIntItem_Allocs(t *testing.T) {
	item := NewIntItem(8, int64(1), int64(2), int64(3))

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

	// ToInt must allocate exactly once (slices.Clone).
	allocs = testing.AllocsPerRun(100, func() {
		_, _ = item.ToInt()
	})

	if allocs != 1 {
		t.Errorf("ToInt: got %v allocs, want 1", allocs)
	}
}

// TestIntItem_EncodedLen verifies that EncodedLen() == len(ToBytes()) for all valid configurations.
func TestIntItem_EncodedLen(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	cases := []struct {
		byteSize int
		values   []any
	}{
		{1, nil},
		{1, []any{1, 2, 3}},
		{2, []any{math.MinInt16, 0, math.MaxInt16}},
		{4, []any{math.MinInt32, math.MaxInt32}},
		{8, []any{math.MinInt64, 0, math.MaxInt64}},
	}

	for _, c := range cases {
		var item Item
		if c.values == nil {
			item = NewIntItem(c.byteSize)
		} else {
			item = NewIntItem(c.byteSize, c.values...)
		}

		require.NoError(item.Error())
		require.Equal(item.EncodedLen(), len(item.ToBytes()))
	}

	// Error item: the itemErr short-circuit path must satisfy the same invariant.
	// byteSize 3 is invalid, so NewIntItem(3) sets a deferred error.
	// Both EncodedLen() and len(ToBytes()) must be 0.
	errItem := NewIntItem(3)
	require.Error(errItem.Error())
	require.Equal(0, errItem.EncodedLen())
	require.Equal(errItem.EncodedLen(), len(errItem.ToBytes()))
}

// TestCombineIntValues exercises the full type matrix that combineIntValues must handle,
// ported from git show main:secs2/int_test.go TestCombineIntValues. Tested via NewIntItem
// to avoid internal type assertions.
func TestCombineIntValues(t *testing.T) { //nolint:cyclop
	t.Parallel()

	tests := []struct {
		name      string
		byteSize  int
		values    []any
		want      []int64
		wantErr   bool
		errSubstr string
	}{
		{
			name:     "Signed Integers",
			byteSize: 8,
			values: []any{
				int(-10), []int{20, -30},
				int8(-40), []int8{50, -60},
				int16(-70), []int16{80, -90},
				int32(-100), []int32{110, -120},
				int64(-130), []int64{140, -150},
			},
			want: []int64{-10, 20, -30, -40, 50, -60, -70, 80, -90, -100, 110, -120, -130, 140, -150},
		},
		{
			name:     "Unsigned Integers (within range)",
			byteSize: 4,
			values: []any{
				uint(1), []uint{2, 3},
				uint8(4), []uint8{5, 6},
				uint16(7), []uint16{8, 9},
				uint32(10), []uint32{11, 12},
				uint64(13), []uint64{14, 15},
			},
			want: []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
		},
		{
			name:     "Overflow uint64 clamps to I4 max",
			byteSize: 4,
			values:   []any{uint64(math.MaxUint64)},
			want:     []int64{math.MaxInt32},
		},
		{
			name:     "Overflow int clamps to I1 max",
			byteSize: 1,
			values:   []any{int(128)},
			want:     []int64{127},
		},
		{
			name:     "Overflow int32 slice clamps to I2 max",
			byteSize: 2,
			values:   []any{[]int32{32768}},
			want:     []int64{32767},
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
			errSubstr: `strconv.ParseInt: parsing "abc": invalid syntax`,
		},
		{
			name:      "Unsupported Type",
			byteSize:  1,
			values:    []any{3.14159},
			wantErr:   true,
			errSubstr: "input argument contains invalid type for IntItem",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			item := NewIntItem(test.byteSize, test.values...)

			if test.wantErr {
				require.Error(t, item.Error())
				require.ErrorContains(t, item.Error(), test.errSubstr)

				return
			}

			require.NoError(t, item.Error())

			got, err := item.ToInt()
			require.NoError(t, err)
			require.Equal(t, test.want, got)
		})
	}
}
