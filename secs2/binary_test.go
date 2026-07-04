package secs2

import (
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestBinaryItem covers round-trip construction, exact wire bytes, and SML output. Vectors are
// ported from git show main:secs2/binary_test.go, updated to hex-literal default format.
func TestBinaryItem(t *testing.T) {
	t.Parallel()

	tests := []struct {
		description     string
		input           []any
		expectedSize    int
		expectedValues  []byte
		expectedToBytes []byte
		expectedToSML   string
	}{
		{
			description:     "empty",
			input:           []any{},
			expectedSize:    0,
			expectedValues:  []byte{},
			expectedToBytes: []byte{33, 0},
			expectedToSML:   "<B[0]>",
		},
		{
			description:     "single byte (zero)",
			input:           []any{byte(0)},
			expectedSize:    1,
			expectedValues:  []byte{0},
			expectedToBytes: []byte{33, 1, 0},
			expectedToSML:   "<B[1] 0x00>",
		},
		{
			description:     "three bytes (byte input)",
			input:           []any{byte(1), byte(2), byte(255)},
			expectedSize:    3,
			expectedValues:  []byte{1, 2, 255},
			expectedToBytes: []byte{33, 3, 1, 2, 255},
			expectedToSML:   "<B[3] 0x01 0x02 0xFF>",
		},
		{
			description:     "three bytes ([]byte input)",
			input:           []any{[]byte{1, 2, 255}},
			expectedSize:    3,
			expectedValues:  []byte{1, 2, 255},
			expectedToBytes: []byte{33, 3, 1, 2, 255},
			expectedToSML:   "<B[3] 0x01 0x02 0xFF>",
		},
		{
			description:     "three bytes (int input)",
			input:           []any{1, 2, 255},
			expectedSize:    3,
			expectedValues:  []byte{1, 2, 255},
			expectedToBytes: []byte{33, 3, 1, 2, 255},
			expectedToSML:   "<B[3] 0x01 0x02 0xFF>",
		},
		{
			description:     "three bytes (binary string input)",
			input:           []any{"0b00", "0b01", "0b11111111"},
			expectedSize:    3,
			expectedValues:  []byte{0, 1, 255},
			expectedToBytes: []byte{33, 3, 0, 1, 255},
			expectedToSML:   "<B[3] 0x00 0x01 0xFF>",
		},
		{
			description:     "seven bytes (mixed int, byte, string, []byte)",
			input:           []any{1, byte(2), "0b1111", []byte{10, 55, 255}, byte(42)},
			expectedSize:    7,
			expectedValues:  []byte{1, 2, 15, 10, 55, 255, 42},
			expectedToBytes: []byte{33, 7, 1, 2, 15, 10, 55, 255, 42},
			expectedToSML:   "<B[7] 0x01 0x02 0x0F 0x0A 0x37 0xFF 0x2A>",
		},
	}

	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			t.Parallel()

			require := require.New(t)

			item := NewBinaryItem(test.input...)
			require.NoError(item.Error())
			require.Equal(test.expectedSize, item.Size())
			require.Equal(test.expectedToBytes, item.ToBytes())
			require.Equal(test.expectedToSML, item.ToSML())

			got, err := item.ToBinary()
			require.NoError(err)
			require.Equal(test.expectedValues, got)
		})
	}
}

// TestBinaryItem_Errors verifies the deferred-error behaviour for invalid value types and
// out-of-range integer inputs.
func TestBinaryItem_Errors(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	itemErr := &ItemError{}

	// float64 is not a supported type.
	item := NewBinaryItem(3.14)
	require.ErrorAs(item.Error(), &itemErr)

	// int out of range (> 255).
	item = NewBinaryItem(256)
	require.ErrorAs(item.Error(), &itemErr)

	// int out of range (negative).
	item = NewBinaryItem(-1)
	require.ErrorAs(item.Error(), &itemErr)

	// Non-numeric string.
	item = NewBinaryItem("not-a-number")
	require.ErrorAs(item.Error(), &itemErr)

	// String value out of range.
	item = NewBinaryItem("256")
	require.ErrorAs(item.Error(), &itemErr)

	// ToBinary on an error item must return the deferred error.
	errItem := NewBinaryItem(-1)
	require.Error(errItem.Error())
	val, err := errItem.ToBinary()
	require.Nil(val)
	require.Error(err)
}

// TestBinaryItem_AppendTo verifies that AppendTo(prefix) == prefix + <exact wire bytes>.
// The expected bytes are computed independently of ToBytes() to avoid a circular dependency.
// NewBinaryItem(1, 2, 3): format byte 0x21 (Binary, 1 length byte), length 0x03 (3 data bytes),
// then bytes 1, 2, 3 — verified against the binary vector in TestBinaryItem.
func TestBinaryItem_AppendTo(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	item := NewBinaryItem(1, 2, 3)
	prefix := []byte{0xDE, 0xAD}
	itemBytes := []byte{0x21, 0x03, 0x01, 0x02, 0x03}

	expected := append(slices.Clone(prefix), itemBytes...)
	result := item.AppendTo(slices.Clone(prefix))
	require.Equal(expected, result)
}

// TestBinaryItem_NoLeak (teeth test) confirms that mutating:
//  1. the slice returned by ToBinary
//  2. the extended slice returned by AppendBinaryTo
//
// does not affect the item's internal state.
func TestBinaryItem_NoLeak(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	item := NewBinaryItem(byte(1), byte(2), byte(3))
	origBytes := item.ToBytes()

	// Mutate ToBinary result — source must be unchanged.
	got, err := item.ToBinary()
	require.NoError(err)
	got[0]++ // mutate the returned copy

	got2, err := item.ToBinary()
	require.NoError(err)
	require.Equal([]byte{1, 2, 3}, got2)
	require.Equal(origBytes, item.ToBytes())

	// Mutate AppendBinaryTo result — source must still be unchanged.
	buf := item.AppendBinaryTo(nil)
	buf[1]++ // mutate

	got3, err := item.ToBinary()
	require.NoError(err)
	require.Equal([]byte{1, 2, 3}, got3)
	require.Equal(origBytes, item.ToBytes())
}

// TestBinaryItem_ByteAt verifies bounds-checked single-byte access.
func TestBinaryItem_ByteAt(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	item := NewBinaryItem(byte(10), byte(20), byte(30))

	// Valid indices.
	b, err := item.ByteAt(0)
	require.NoError(err)
	require.Equal(byte(10), b)

	b, err = item.ByteAt(2)
	require.NoError(err)
	require.Equal(byte(30), b)

	// Out-of-range indices.
	_, err = item.ByteAt(-1)
	require.Error(err)

	_, err = item.ByteAt(3)
	require.Error(err)

	// Error item: ByteAt returns the deferred error regardless of index.
	errItem := NewBinaryItem(-1)
	require.Error(errItem.Error())
	_, err = errItem.ByteAt(0)
	require.Error(err)
}

// TestBinaryItem_AppendBinaryTo verifies that AppendBinaryTo appends into a non-empty buffer and
// leaves the prefix intact.
func TestBinaryItem_AppendBinaryTo(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	prefix := []byte{0xDE, 0xAD}
	item := NewBinaryItem(byte(10), byte(20), byte(30))

	result := item.AppendBinaryTo(slices.Clone(prefix))
	require.Equal([]byte{0xDE, 0xAD, 10, 20, 30}, result)

	// Error item: AppendBinaryTo must return dst unchanged.
	errItem := NewBinaryItem(-1)
	require.Error(errItem.Error())
	dst := []byte{0xFF}
	out := errItem.AppendBinaryTo(dst)
	require.Equal([]byte{0xFF}, out)
}

// TestBinaryItem_WrongType verifies that wrong-type accessors return errors on a BinaryItem.
func TestBinaryItem_WrongType(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	item := NewBinaryItem(byte(1), byte(2))
	require.NoError(item.Error())

	// ToInt must error on a binary item.
	_, err := item.ToInt()
	require.Error(err)

	// ToASCII must error.
	_, err = item.ToASCII()
	require.Error(err)

	// Get is list-only — any call must error.
	got, err := item.Get()
	require.Nil(got)
	require.Error(err)

	got, err = item.Get(0)
	require.Nil(got)
	require.Error(err)
}

// TestBinaryItem_Concurrency exercises concurrent ToBinary / AppendBinaryTo / ToBytes reads
// under -race.
func TestBinaryItem_Concurrency(t *testing.T) {
	t.Parallel()

	item := NewBinaryItem(byte(1), byte(2), byte(3))

	const goroutines = 50

	var wg sync.WaitGroup

	wg.Add(goroutines)

	for range goroutines {
		go func() {
			defer wg.Done()

			_ = item.ToBytes()

			buf := make([]byte, 0, item.Size())
			_ = item.AppendBinaryTo(buf)

			_, _ = item.ToBinary()
		}()
	}

	wg.Wait()
}

// TestBinaryItem_Allocs asserts allocation counts for the hot encoding and accessor paths.
func TestBinaryItem_Allocs(t *testing.T) {
	item := NewBinaryItem(byte(1), byte(2), byte(3))

	// AppendBinaryTo into a pre-sized buffer must not allocate.
	buf := make([]byte, 0, item.Size())

	allocs := testing.AllocsPerRun(100, func() {
		buf = buf[:0]
		buf = item.AppendBinaryTo(buf)
	})

	if allocs > 0 {
		t.Errorf("AppendBinaryTo into pre-sized buffer: got %v allocs, want 0", allocs)
	}

	// ToBytes must allocate exactly once (the output slice).
	allocs = testing.AllocsPerRun(100, func() {
		_ = item.ToBytes()
	})

	if allocs != 1 {
		t.Errorf("ToBytes: got %v allocs, want 1", allocs)
	}

	// ToBinary must allocate exactly once (slices.Clone).
	allocs = testing.AllocsPerRun(100, func() {
		_, _ = item.ToBinary()
	})

	if allocs != 1 {
		t.Errorf("ToBinary: got %v allocs, want 1", allocs)
	}
}

// TestBinaryItem_EncodedLen verifies that EncodedLen() == len(ToBytes()) for all valid
// configurations and for an error item.
func TestBinaryItem_EncodedLen(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	cases := [][]any{
		nil,
		{byte(1), byte(2), byte(3)},
		{[]byte{0x00, 0xFF}},
		{"0b10101010", "0xFF"},
	}

	for _, vals := range cases {
		var item Item
		if vals == nil {
			item = NewBinaryItem()
		} else {
			item = NewBinaryItem(vals...)
		}

		require.NoError(item.Error())
		require.Equal(item.EncodedLen(), len(item.ToBytes()))
	}

	// Error item: both EncodedLen() and len(ToBytes()) must be 0.
	errItem := NewBinaryItem(-1)
	require.Error(errItem.Error())
	require.Equal(0, errItem.EncodedLen())
	require.Equal(errItem.EncodedLen(), len(errItem.ToBytes()))
}
