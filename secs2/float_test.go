package secs2

import (
	"math"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestFloatItem covers round-trip construction, exact wire bytes, and SML output.
// Vectors are ported from git show main:secs2/float_test.go.
func TestFloatItem(t *testing.T) {
	t.Parallel()

	tests := []struct {
		description     string
		input           []any
		byteSize        int
		expectedSize    int
		expectedValues  []float64
		expectedToBytes []byte
		expectedToSML   string
	}{
		{
			description:     "Byte size: 4, data size: 0",
			input:           []any{},
			byteSize:        4,
			expectedSize:    0,
			expectedValues:  []float64{},
			expectedToBytes: []byte{0x91, 0x0},
			expectedToSML:   "<F4[0]>",
		},
		{
			description:     "Byte size: 4, data size: 3",
			input:           []any{-1.234, 0.0, 3.14159},
			byteSize:        4,
			expectedSize:    3,
			expectedValues:  []float64{-1.234, 0.0, 3.14159},
			expectedToBytes: []byte{0x91, 0xc, 0xbf, 0x9d, 0xf3, 0xb6, 0x00, 0x00, 0x00, 0x00, 0x40, 0x49, 0x0f, 0xd0},
			expectedToSML:   "<F4[3] -1.23399997 0 3.14159012>",
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
			expectedToSML: "<F8[3] -1.7976931348623157E+308 0 1.7976931348623157E+308>",
		},
		{
			description:    "Byte size: 8, float boundary",
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
			expectedToSML: "<F8[3] -4.9406564584124654E-324 -2 -1.7976931348623157E+308>",
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
			expectedToSML: "<F8[5] 3.1415899999999999 1 2 3 4.5670000000000002>",
		},
	}

	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			t.Parallel()

			require := require.New(t)

			item := NewFloatItem(test.byteSize, test.input...)
			require.NoError(item.Error())
			require.Equal(test.expectedSize, item.Size())
			require.Equal(test.expectedToBytes, item.ToBytes())
			require.Equal(test.expectedToSML, item.ToSML())

			vals, err := item.ToFloat()
			require.NoError(err)
			require.Equal(test.expectedValues, vals)
		})
	}
}

// TestFloatItem_Errors verifies deferred-error behaviour for invalid byte sizes and invalid value types.
func TestFloatItem_Errors(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	itemErr := &ItemError{}

	// Invalid byteSize must set a deferred error.
	item := NewFloatItem(3)
	require.ErrorAs(item.Error(), &itemErr)

	// bool is not a supported value type.
	item = NewFloatItem(4, true)
	require.ErrorAs(item.Error(), &itemErr)
	require.ErrorContains(item.Error(), "invalid type")

	// Non-numeric string must set a deferred error.
	item = NewFloatItem(8, "not_a_float")
	require.ErrorAs(item.Error(), &itemErr)
}

// TestFloatItem_AppendTo verifies that AppendTo(prefix) == prefix + exact wire bytes.
// The expected bytes are computed independently of ToBytes() to avoid circular dependency.
//
// NewFloatItem(4, 1.0, 2.5):
//
//	F4 format byte 0x91, length 0x08 (2 values × 4 bytes),
//	1.0 as IEEE-754 float32 big-endian = 0x3F800000,
//	2.5 as IEEE-754 float32 big-endian = 0x40200000.
func TestFloatItem_AppendTo(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	item := NewFloatItem(4, 1.0, 2.5)
	prefix := []byte{0xDE, 0xAD}
	itemBytes := []byte{
		0x91, 0x08,
		0x3F, 0x80, 0x00, 0x00,
		0x40, 0x20, 0x00, 0x00,
	}

	expected := append(slices.Clone(prefix), itemBytes...)
	result := item.AppendTo(slices.Clone(prefix))
	require.Equal(expected, result)

	// Also test F8 independently:
	// NewFloatItem(8, 1.0, -1.5):
	//   F8 format byte 0x81, length 0x10 (2 values × 8 bytes),
	//   1.0 as float64 big-endian = 0x3FF0000000000000,
	//   -1.5 as float64 big-endian = 0xBFF8000000000000.
	item8 := NewFloatItem(8, 1.0, -1.5)
	prefix8 := []byte{0xBE, 0xEF}
	itemBytes8 := []byte{
		0x81, 0x10,
		0x3F, 0xF0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0xBF, 0xF8, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}

	expected8 := append(slices.Clone(prefix8), itemBytes8...)
	result8 := item8.AppendTo(slices.Clone(prefix8))
	require.Equal(expected8, result8)
}

// TestFloatItem_TypeAccessors verifies Type(), IsFloat32/64.
func TestFloatItem_TypeAccessors(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	tests := []struct {
		byteSize  int
		wantType  string
		isFloat32 bool
		isFloat64 bool
	}{
		{4, Float32Type, true, false},
		{8, Float64Type, false, true},
	}

	for _, test := range tests {
		item := NewFloatItem(test.byteSize)
		require.Equal(test.wantType, item.Type())
		require.Equal(test.isFloat32, item.IsFloat32())
		require.Equal(test.isFloat64, item.IsFloat64())
	}
}

// TestFloatItem_NoLeak (teeth test) confirms that mutating the slice returned by ToFloat does not
// affect the item's internal state — wire encoding and subsequent ToFloat calls are unchanged.
func TestFloatItem_NoLeak(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	item := NewFloatItem(8, 42.0)
	origBytes := item.ToBytes()

	xs, err := item.ToFloat()
	require.NoError(err)
	xs[0] = 999.0 // mutate the returned copy

	// Internal state must be unchanged.
	xs2, err := item.ToFloat()
	require.NoError(err)
	require.Equal([]float64{42.0}, xs2)
	require.Equal(origBytes, item.ToBytes())
}

// TestFloatItem_Iterator verifies Floats() and FloatAt().
func TestFloatItem_Iterator(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	values := []float64{-1.0, 0.0, 1.0, 2.5}
	item := NewFloatItem(8, values)

	// Floats() must yield all values in order.
	got := slices.Collect(item.Floats())
	require.Equal(values, got)

	// FloatAt valid indices.
	for i, v := range values {
		gotV, err := item.FloatAt(i)
		require.NoError(err)
		require.Equal(v, gotV)
	}

	// FloatAt out-of-range indices must return an error.
	_, err := item.FloatAt(-1)
	require.Error(err)

	_, err = item.FloatAt(len(values))
	require.Error(err)

	// Error item: Floats() yields nothing and FloatAt returns an error.
	errItem := NewFloatItem(3)
	require.Error(errItem.Error())
	require.Empty(slices.Collect(errItem.Floats()))

	_, err = errItem.FloatAt(0)
	require.Error(err)

	// ToFloat on error item must return error.
	_, err = errItem.ToFloat()
	require.Error(err)
}

// TestFloatItem_WrongType verifies that wrong-type accessors and Get return errors on a FloatItem.
func TestFloatItem_WrongType(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	item := NewFloatItem(4, 1.0, 2.0)
	require.NoError(item.Error())

	// String accessor must error.
	_, err := item.ToASCII()
	require.Error(err)

	// Int accessor must error.
	_, err = item.ToInt()
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

// TestFloatItem_Concurrency exercises concurrent ToBytes / AppendTo / Floats reads under -race.
func TestFloatItem_Concurrency(t *testing.T) {
	t.Parallel()

	item := NewFloatItem(8, 1.0, 2.0, 3.0)

	const goroutines = 50

	var wg sync.WaitGroup

	wg.Add(goroutines)

	for range goroutines {
		go func() {
			defer wg.Done()

			_ = item.ToBytes()

			buf := make([]byte, 0, item.EncodedLen())
			_ = item.AppendTo(buf)

			_ = slices.Collect(item.Floats())
		}()
	}

	wg.Wait()
}

// TestFloatItem_Allocs asserts allocation counts for the hot encoding and accessor paths.
func TestFloatItem_Allocs(t *testing.T) {
	item := NewFloatItem(8, 1.0, 2.0, 3.0)

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

	// ToFloat must allocate exactly once (slices.Clone).
	allocs = testing.AllocsPerRun(100, func() {
		_, _ = item.ToFloat()
	})

	if allocs != 1 {
		t.Errorf("ToFloat: got %v allocs, want 1", allocs)
	}
}

// TestFloatItem_EncodedLen verifies that EncodedLen() == len(ToBytes()) for all valid configurations.
func TestFloatItem_EncodedLen(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	cases := []struct {
		byteSize int
		values   []any
	}{
		{4, nil},
		{4, []any{1.0, 2.5, -1.5}},
		{8, []any{-math.MaxFloat64, 0.0, math.MaxFloat64}},
		{8, []any{1.0, 2.0}},
	}

	for _, c := range cases {
		var item Item
		if c.values == nil {
			item = NewFloatItem(c.byteSize)
		} else {
			item = NewFloatItem(c.byteSize, c.values...)
		}

		require.NoError(item.Error())
		require.Equal(item.EncodedLen(), len(item.ToBytes()))
	}

	// Error item: both EncodedLen() and len(ToBytes()) must be 0.
	errItem := NewFloatItem(3)
	require.Error(errItem.Error())
	require.Equal(0, errItem.EncodedLen())
	require.Equal(errItem.EncodedLen(), len(errItem.ToBytes()))
}

// TestCombineFloatValues exercises the full type matrix for combineFloatValues, including
// NaN/Inf passthrough, int precision errors, F4 clamping, and string parsing.
func TestCombineFloatValues(t *testing.T) {
	t.Parallel()

	t.Run("Accept NaN and Inf for F4", func(t *testing.T) {
		t.Parallel()

		item := NewFloatItem(4, float32(math.NaN()), float32(math.Inf(1)), float32(math.Inf(-1)))
		require.NoError(t, item.Error())

		vals, err := item.ToFloat()
		require.NoError(t, err)
		require.Equal(t, 3, len(vals))
		require.True(t, math.IsNaN(vals[0]))
		require.True(t, math.IsInf(vals[1], 1))
		require.True(t, math.IsInf(vals[2], -1))
	})

	t.Run("Accept NaN and Inf for F8", func(t *testing.T) {
		t.Parallel()

		item := NewFloatItem(8, math.NaN(), math.Inf(1), math.Inf(-1))
		require.NoError(t, item.Error())

		vals, err := item.ToFloat()
		require.NoError(t, err)
		require.Equal(t, 3, len(vals))
		require.True(t, math.IsNaN(vals[0]))
		require.True(t, math.IsInf(vals[1], 1))
		require.True(t, math.IsInf(vals[2], -1))
	})

	t.Run("Int64 precision boundary", func(t *testing.T) {
		t.Parallel()

		// 2^53 is exactly representable.
		val := int64(1) << 53
		item := NewFloatItem(8, val)
		require.NoError(t, item.Error())
		vals, err := item.ToFloat()
		require.NoError(t, err)
		require.Equal(t, float64(val), vals[0])

		// 2^53 + 1 overflows float64 precision.
		overflowVal := (int64(1) << 53) + 1
		item = NewFloatItem(8, overflowVal)
		require.Error(t, item.Error())
		require.ErrorContains(t, item.Error(), "value overflow")
	})

	t.Run("Int precision boundary", func(t *testing.T) {
		t.Parallel()

		valInt := int(1) << 53
		item := NewFloatItem(8, valInt)
		require.NoError(t, item.Error())

		overflowValInt := (int(1) << 53) + 1
		item = NewFloatItem(8, overflowValInt)
		require.Error(t, item.Error())
		require.ErrorContains(t, item.Error(), "value overflow")
	})

	t.Run("Uint64 precision boundary", func(t *testing.T) {
		t.Parallel()

		val := uint64(1) << 53
		item := NewFloatItem(8, val)
		require.NoError(t, item.Error())
		vals, err := item.ToFloat()
		require.NoError(t, err)
		require.Equal(t, float64(val), vals[0])

		overflowVal := (uint64(1) << 53) + 1
		item = NewFloatItem(8, overflowVal)
		require.Error(t, item.Error())
		require.ErrorContains(t, item.Error(), "value overflow")
	})

	t.Run("Uint precision boundary", func(t *testing.T) {
		t.Parallel()

		valUint := uint(1) << 53
		item := NewFloatItem(8, valUint)
		require.NoError(t, item.Error())

		overflowValUint := (uint(1) << 53) + 1
		item = NewFloatItem(8, overflowValUint)
		require.Error(t, item.Error())
		require.ErrorContains(t, item.Error(), "value overflow")
	})

	t.Run("F4 float64 overflow clamped", func(t *testing.T) {
		t.Parallel()

		maxF32 := float64(math.MaxFloat32)

		item := NewFloatItem(4, maxF32)
		require.NoError(t, item.Error())

		// Over-range float64 is clamped (not an error).
		item = NewFloatItem(4, maxF32*1.1)
		require.NoError(t, item.Error())
		vals, err := item.ToFloat()
		require.NoError(t, err)
		require.Equal(t, maxF32, vals[0])

		item = NewFloatItem(4, -maxF32*1.1)
		require.NoError(t, item.Error())
		vals, err = item.ToFloat()
		require.NoError(t, err)
		require.Equal(t, -maxF32, vals[0])
	})

	t.Run("F4 string overflow clamped", func(t *testing.T) {
		t.Parallel()

		maxF32 := float64(math.MaxFloat32)

		item := NewFloatItem(4, "3.4e38")
		require.NoError(t, item.Error())

		item = NewFloatItem(4, "3.5e38")
		require.NoError(t, item.Error())
		vals, err := item.ToFloat()
		require.NoError(t, err)
		require.Equal(t, maxF32, vals[0])
	})

	t.Run("F4 always accepts float32", func(t *testing.T) {
		t.Parallel()

		item := NewFloatItem(4, float32(math.MaxFloat32))
		require.NoError(t, item.Error())
	})

	t.Run("F8 accepts large values", func(t *testing.T) {
		t.Parallel()

		val := float64(math.MaxFloat32) * 2
		item := NewFloatItem(8, val)
		require.NoError(t, item.Error())
		vals, err := item.ToFloat()
		require.NoError(t, err)
		require.Equal(t, val, vals[0])
	})

	t.Run("Slice inputs", func(t *testing.T) {
		t.Parallel()

		// []int8, []int16, []int32 accepted.
		item := NewFloatItem(8,
			int8(-10), []int8{20, -30},
			int16(-70), []int16{80, -90},
			int32(-100), []int32{110, -120},
		)
		require.NoError(t, item.Error())
		vals, err := item.ToFloat()
		require.NoError(t, err)
		require.Equal(t, []float64{-10, 20, -30, -70, 80, -90, -100, 110, -120}, vals)

		// []uint8, []uint16, []uint32 accepted.
		item = NewFloatItem(8,
			uint8(4), []uint8{5, 6},
			uint16(7), []uint16{8, 9},
			uint32(10), []uint32{11, 12},
		)
		require.NoError(t, item.Error())
		vals, err = item.ToFloat()
		require.NoError(t, err)
		require.Equal(t, []float64{4, 5, 6, 7, 8, 9, 10, 11, 12}, vals)

		// []int64 with overflow must error.
		vals64 := []int64{1, (1 << 53) + 1}
		item = NewFloatItem(8, vals64)
		require.Error(t, item.Error())

		// []float64 over-range for F4 — should be clamped.
		fVals := []float64{1.0, float64(math.MaxFloat32) * 2}
		item = NewFloatItem(4, fVals)
		require.NoError(t, item.Error())
		vals, err = item.ToFloat()
		require.NoError(t, err)
		require.Equal(t, float64(math.MaxFloat32), vals[1])
	})

	t.Run("String inputs", func(t *testing.T) {
		t.Parallel()

		item := NewFloatItem(8, "3.14159", []string{"1.0", "2.0"})
		require.NoError(t, item.Error())
		vals, err := item.ToFloat()
		require.NoError(t, err)
		require.Equal(t, []float64{3.14159, 1.0, 2.0}, vals)

		// Invalid string must error.
		item = NewFloatItem(8, "not_a_float")
		require.Error(t, item.Error())
	})

	t.Run("Invalid type", func(t *testing.T) {
		t.Parallel()

		item := NewFloatItem(4, true)
		require.Error(t, item.Error())
		require.ErrorContains(t, item.Error(), "invalid type")
	})
}
