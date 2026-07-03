package secs2

import (
	"math"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestUintItem covers round-trip construction, exact wire bytes, and SML output for all four
// byte sizes. Vectors are derived from the v1 uint_test.go reference vectors.
func TestUintItem(t *testing.T) {
	t.Parallel()

	tests := []struct {
		description     string
		input           []any
		byteSize        int
		expectedSize    int
		expectedValues  []uint64
		expectedToBytes []byte
		expectedToSML   string
	}{
		{
			description:     "Byte size: 1, data size: 0",
			input:           []any{},
			byteSize:        1,
			expectedSize:    0,
			expectedValues:  []uint64{},
			expectedToBytes: []byte{0xa5, 0},
			expectedToSML:   "<U1[0]>",
		},
		{
			description:     "Byte size: 1, data size: 3",
			input:           []any{0, 1, math.MaxUint8},
			byteSize:        1,
			expectedSize:    3,
			expectedValues:  []uint64{0, 1, math.MaxUint8},
			expectedToBytes: []byte{0xa5, 3, 0x00, 0x01, 0xff},
			expectedToSML:   "<U1[3] 0 1 255>",
		},
		{
			description:     "Byte size: 2, data size: 3",
			input:           []any{0, 1, math.MaxUint16},
			byteSize:        2,
			expectedSize:    3,
			expectedValues:  []uint64{0, 1, math.MaxUint16},
			expectedToBytes: []byte{0xa9, 0x06, 0x00, 0x00, 0x00, 0x01, 0xff, 0xff},
			expectedToSML:   "<U2[3] 0 1 65535>",
		},
		{
			description:     "Byte size: 4, data size: 3",
			input:           []any{0, 1, math.MaxUint32},
			byteSize:        4,
			expectedSize:    3,
			expectedValues:  []uint64{0, 1, math.MaxUint32},
			expectedToBytes: []byte{0xb1, 0x0c, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0xff, 0xff, 0xff, 0xff},
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
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01,
				0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
			},
			expectedToSML: "<U8[3] 0 1 18446744073709551615>",
		},
		{
			description:     "Byte size: 2, unsigned integer strings",
			input:           []any{"0", "255", "65535"},
			byteSize:        2,
			expectedSize:    3,
			expectedValues:  []uint64{0, 255, 65535},
			expectedToBytes: []byte{0xa9, 0x06, 0x00, 0x00, 0x00, 0xff, 0xff, 0xff},
			expectedToSML:   "<U2[3] 0 255 65535>",
		},
	}

	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			t.Parallel()

			require := require.New(t)

			item := NewUintItem(test.byteSize, test.input...)
			require.NoError(item.Error())
			require.Equal(test.expectedSize, item.Size())
			require.Equal(test.expectedToBytes, item.ToBytes())
			require.Equal(test.expectedToSML, item.ToSML())

			vals, err := item.ToUint()
			require.NoError(err)
			require.Equal(test.expectedValues, vals)
		})
	}
}

// TestUintItem_Clamping verifies that values outside the byte-size range are clamped to the max.
// Uint has no minimum clamp (negative signed inputs cause errors, not clamping).
func TestUintItem_Clamping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		byteSize      int
		inputValue    any
		expectedValue uint64
	}{
		{name: "U1 max overflow clamps to 255", byteSize: 1, inputValue: uint(256), expectedValue: 255},
		{name: "U2 max overflow clamps to 65535", byteSize: 2, inputValue: uint(65536), expectedValue: 65535},
		{name: "U4 max overflow clamps to MaxUint32", byteSize: 4, inputValue: uint64(math.MaxUint32 + 1), expectedValue: math.MaxUint32},
		{name: "U1 positive int overflow clamps to 255", byteSize: 1, inputValue: int(300), expectedValue: 255},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			require := require.New(t)

			item := NewUintItem(test.byteSize, test.inputValue)
			require.NoError(item.Error())

			val, err := item.ToUint()
			require.NoError(err)
			require.Equal([]uint64{test.expectedValue}, val)
		})
	}
}

// TestUintItem_Errors verifies deferred-error behaviour for invalid byte sizes, invalid value
// types, non-numeric strings, and negative signed integers.
func TestUintItem_Errors(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	itemErr := &ItemError{}

	// Invalid byteSize must set a deferred error.
	item := NewUintItem(3)
	require.ErrorAs(item.Error(), &itemErr)

	// float64 is not a supported value type.
	item = NewUintItem(8, 3.14159)
	require.ErrorAs(item.Error(), &itemErr)

	// Non-numeric string must set a deferred error.
	item = NewUintItem(8, "invalid_uint")
	require.ErrorAs(item.Error(), &itemErr)

	// Negative signed integer must set a deferred error.
	item = NewUintItem(8, int(-5))
	require.ErrorAs(item.Error(), &itemErr)
}

// TestUintItem_AppendTo verifies that AppendTo(prefix) == prefix + <exact wire bytes>.
// The expected bytes are computed independently of ToBytes() to avoid a circular dependency.
// NewUintItem(4, 1, 2, 3): format byte 0xb1 (U4, 1 length byte), length 0x0C (12 data bytes),
// then three big-endian uint32 values 1, 2, 3 — verified against the U4 vector in TestUintItem.
func TestUintItem_AppendTo(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	item := NewUintItem(4, 1, 2, 3)
	prefix := []byte{0xDE, 0xAD}
	itemBytes := []byte{
		0xb1, 0x0C,
		0x00, 0x00, 0x00, 0x01,
		0x00, 0x00, 0x00, 0x02,
		0x00, 0x00, 0x00, 0x03,
	}

	expected := append(slices.Clone(prefix), itemBytes...)
	result := item.AppendTo(slices.Clone(prefix))
	require.Equal(expected, result)
}

// TestUintItem_TypeAccessors verifies Type(), IsUint8/16/32/64.
func TestUintItem_TypeAccessors(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	tests := []struct {
		byteSize int
		wantType string
		isUint8  bool
		isUint16 bool
		isUint32 bool
		isUint64 bool
	}{
		{1, Uint8Type, true, false, false, false},
		{2, Uint16Type, false, true, false, false},
		{4, Uint32Type, false, false, true, false},
		{8, Uint64Type, false, false, false, true},
	}

	for _, test := range tests {
		item := NewUintItem(test.byteSize)
		require.Equal(test.wantType, item.Type())
		require.Equal(test.isUint8, item.IsUint8())
		require.Equal(test.isUint16, item.IsUint16())
		require.Equal(test.isUint32, item.IsUint32())
		require.Equal(test.isUint64, item.IsUint64())
	}
}

// TestUintItem_NoLeak (teeth test) confirms that mutating the slice returned by ToUint does not
// affect the item's internal state — wire encoding and subsequent ToUint calls are unchanged.
func TestUintItem_NoLeak(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	item := NewUintItem(8, uint64(42))
	origBytes := item.ToBytes()

	xs, err := item.ToUint()
	require.NoError(err)
	xs[0]++ // mutate the returned copy

	// Internal state must be unchanged.
	xs2, err := item.ToUint()
	require.NoError(err)
	require.Equal([]uint64{42}, xs2)
	require.Equal(origBytes, item.ToBytes())
}

// TestUintItem_Iterator verifies Uints() and UintAt().
func TestUintItem_Iterator(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	// Pass a []uint64 slice as a single any — combineUintValues handles []uint64 in the fast path.
	values := []uint64{0, 100, math.MaxUint32, math.MaxUint64}
	item := NewUintItem(8, values)

	// Uints() must yield all values in order.
	got := slices.Collect(item.Uints())
	require.Equal(values, got)

	// UintAt valid indices.
	for i, v := range values {
		gotV, err := item.UintAt(i)
		require.NoError(err)
		require.Equal(v, gotV)
	}

	// UintAt out-of-range indices must return an error.
	_, err := item.UintAt(-1)
	require.Error(err)

	_, err = item.UintAt(len(values))
	require.Error(err)

	// Error item: Uints() yields nothing and UintAt returns an error.
	errItem := NewUintItem(3)
	require.Error(errItem.Error())
	require.Empty(slices.Collect(errItem.Uints()))

	_, err = errItem.UintAt(0)
	require.Error(err)
}

// TestUintItem_WrongType verifies that wrong-type accessors and Get return errors on a UintItem.
func TestUintItem_WrongType(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	item := NewUintItem(4, 1, 2, 3)
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

// TestUintItem_Concurrency exercises concurrent ToBytes / AppendTo / Uints reads under -race.
func TestUintItem_Concurrency(t *testing.T) {
	t.Parallel()

	item := NewUintItem(8, uint64(1), uint64(2), uint64(3))

	const goroutines = 50

	var wg sync.WaitGroup

	wg.Add(goroutines)

	for range goroutines {
		go func() {
			defer wg.Done()

			_ = item.ToBytes()

			buf := make([]byte, 0, item.EncodedLen())
			_ = item.AppendTo(buf)

			_ = slices.Collect(item.Uints())
		}()
	}

	wg.Wait()
}

// TestUintItem_Allocs asserts allocation counts for the hot encoding and accessor paths.
func TestUintItem_Allocs(t *testing.T) {
	item := NewUintItem(8, uint64(1), uint64(2), uint64(3))

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

	// ToUint must allocate exactly once (slices.Clone).
	allocs = testing.AllocsPerRun(100, func() {
		_, _ = item.ToUint()
	})

	if allocs != 1 {
		t.Errorf("ToUint: got %v allocs, want 1", allocs)
	}
}

// TestUintItem_EncodedLen verifies that EncodedLen() == len(ToBytes()) for all valid configurations.
func TestUintItem_EncodedLen(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	cases := []struct {
		byteSize int
		values   []any
	}{
		{1, nil},
		{1, []any{0, 1, 255}},
		{2, []any{0, 1, math.MaxUint16}},
		{4, []any{0, math.MaxUint32}},
		{8, []any{0, uint64(math.MaxUint64)}},
	}

	for _, c := range cases {
		var item Item
		if c.values == nil {
			item = NewUintItem(c.byteSize)
		} else {
			item = NewUintItem(c.byteSize, c.values...)
		}

		require.NoError(item.Error())
		require.Equal(item.EncodedLen(), len(item.ToBytes()))
	}

	// Error item: the itemErr short-circuit path must satisfy the same invariant.
	// byteSize 3 is invalid, so NewUintItem(3) sets a deferred error.
	// Both EncodedLen() and len(ToBytes()) must be 0.
	errItem := NewUintItem(3)
	require.Error(errItem.Error())
	require.Equal(0, errItem.EncodedLen())
	require.Equal(errItem.EncodedLen(), len(errItem.ToBytes()))
}

// TestCombineUintValues exercises the full type matrix that combineUintValues must handle,
// ported from git show main:secs2/uint_test.go TestCombineUintValues. Tested via NewUintItem
// to avoid internal type assertions.
func TestCombineUintValues(t *testing.T) { //nolint:cyclop
	t.Parallel()

	tests := []struct {
		name      string
		byteSize  int
		values    []any
		want      []uint64
		wantErr   bool
		errSubstr string
	}{
		{
			name:     "Unsigned Integers",
			byteSize: 8,
			values: []any{
				uint(10), []uint{20, 30},
				uint8(40), []uint8{50, 60},
				uint16(70), []uint16{80, 90},
				uint32(100), []uint32{110, 120},
				uint64(130), []uint64{140, 150},
			},
			want: []uint64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100, 110, 120, 130, 140, 150},
		},
		{
			name:     "Non-negative Signed Integers (within range)",
			byteSize: 4,
			values: []any{
				int(1), []int{2, 3},
				int8(4), []int8{5, 6},
				int16(7), []int16{8, 9},
				int32(10), []int32{11, 12},
				int64(13), []int64{14, 15},
			},
			want: []uint64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
		},
		{
			name:      "Negative Signed Integer",
			byteSize:  2,
			values:    []any{int(-1)},
			wantErr:   true,
			errSubstr: "negative value not allowed for UintItem",
		},
		{
			name:     "Overflow uint clamps to U1 max",
			byteSize: 1,
			values:   []any{uint(256)},
			want:     []uint64{255},
		},
		{
			name:     "Overflow uint64 clamps to U4 max",
			byteSize: 4,
			values:   []any{uint64(math.MaxUint64)},
			want:     []uint64{math.MaxUint32},
		},
		{
			name:     "Overflow uint in slice clamps to U2 max",
			byteSize: 2,
			values:   []any{[]uint{65536}},
			want:     []uint64{65535},
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
			errSubstr: `strconv.ParseUint: parsing "abc": invalid syntax`,
		},
		{
			name:      "Unsupported Type",
			byteSize:  1,
			values:    []any{3.14159},
			wantErr:   true,
			errSubstr: "input argument contains invalid type for UintItem",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			item := NewUintItem(test.byteSize, test.values...)

			if test.wantErr {
				require.Error(t, item.Error())
				require.ErrorContains(t, item.Error(), test.errSubstr)

				return
			}

			require.NoError(t, item.Error())

			got, err := item.ToUint()
			require.NoError(t, err)
			require.Equal(t, test.want, got)
		})
	}
}
