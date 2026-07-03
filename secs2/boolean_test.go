package secs2

import (
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestBooleanItem covers round-trip construction, exact wire bytes, and SML output. Vectors are
// ported from git show main:secs2/boolean_test.go.
func TestBooleanItem(t *testing.T) {
	t.Parallel()

	tests := []struct {
		description     string
		input           []any
		expectedSize    int
		expectedValues  []bool
		expectedToBytes []byte
		expectedToSML   string
	}{
		{
			description:     "empty",
			input:           []any{},
			expectedSize:    0,
			expectedValues:  []bool{},
			expectedToBytes: []byte{37, 0},
			expectedToSML:   "<BOOLEAN[0]>",
		},
		{
			description:     "single false",
			input:           []any{false},
			expectedSize:    1,
			expectedValues:  []bool{false},
			expectedToBytes: []byte{37, 1, 0},
			expectedToSML:   "<BOOLEAN[1] False>",
		},
		{
			description:     "three bools (false, true, true)",
			input:           []any{false, true, true},
			expectedSize:    3,
			expectedValues:  []bool{false, true, true},
			expectedToBytes: []byte{37, 3, 0, 1, 1},
			expectedToSML:   "<BOOLEAN[3] False True True>",
		},
		{
			description:     "slice input (true, false, true)",
			input:           []any{[]bool{true, false, true}},
			expectedSize:    3,
			expectedValues:  []bool{true, false, true},
			expectedToBytes: []byte{37, 3, 1, 0, 1},
			expectedToSML:   "<BOOLEAN[3] True False True>",
		},
		{
			description:     "mixed bool and []bool",
			input:           []any{true, []bool{false, true}, false, []bool{true, false}},
			expectedSize:    6,
			expectedValues:  []bool{true, false, true, false, true, false},
			expectedToBytes: []byte{37, 6, 1, 0, 1, 0, 1, 0},
			expectedToSML:   "<BOOLEAN[6] True False True False True False>",
		},
	}

	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			t.Parallel()

			require := require.New(t)

			item := NewBooleanItem(test.input...)
			require.NoError(item.Error())
			require.Equal(test.expectedSize, item.Size())
			require.Equal(test.expectedToBytes, item.ToBytes())
			require.Equal(test.expectedToSML, item.ToSML())

			got, err := item.ToBoolean()
			require.NoError(err)
			require.Equal(test.expectedValues, got)
		})
	}
}

// TestBooleanItem_Errors verifies the deferred-error behaviour for invalid value types.
func TestBooleanItem_Errors(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	itemErr := &ItemError{}

	// int is not a supported type for BooleanItem.
	item := NewBooleanItem(1)
	require.ErrorAs(item.Error(), &itemErr)

	// string is not a supported type.
	item = NewBooleanItem("true")
	require.ErrorAs(item.Error(), &itemErr)

	// float64 is not a supported type.
	item = NewBooleanItem(3.14)
	require.ErrorAs(item.Error(), &itemErr)

	// ToBoolean on an error item must return the deferred error.
	errItem := NewBooleanItem(42)
	require.Error(errItem.Error())
	val, err := errItem.ToBoolean()
	require.Nil(val)
	require.Error(err)
}

// TestBooleanItem_AppendTo verifies that AppendTo(prefix) == prefix + <exact wire bytes>.
// The expected bytes are computed independently of ToBytes() to avoid a circular dependency.
// NewBooleanItem(true, false, true): format byte 0x25 (BOOLEAN, 1 length byte), length 0x03,
// then bytes 0x01, 0x00, 0x01 for true, false, true.
func TestBooleanItem_AppendTo(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	item := NewBooleanItem(true, false, true)
	prefix := []byte{0xDE, 0xAD}
	// 0x25 = BooleanFormatCode(9) << 2 | 1 length byte = 37
	itemBytes := []byte{0x25, 0x03, 0x01, 0x00, 0x01}

	expected := append(slices.Clone(prefix), itemBytes...)
	result := item.AppendTo(slices.Clone(prefix))
	require.Equal(expected, result)
}

// TestBooleanItem_NoLeak (teeth test) confirms that mutating the slice returned by ToBoolean
// does not affect the item's internal state — wire encoding and subsequent ToBoolean calls are
// unchanged.
func TestBooleanItem_NoLeak(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	item := NewBooleanItem(true, false, true)
	origBytes := item.ToBytes()

	got, err := item.ToBoolean()
	require.NoError(err)
	got[0] = false // mutate the returned copy

	// Internal state must be unchanged.
	got2, err := item.ToBoolean()
	require.NoError(err)
	require.Equal([]bool{true, false, true}, got2)
	require.Equal(origBytes, item.ToBytes())
}

// TestBooleanItem_BoolAt verifies bounds-checked single-value access.
func TestBooleanItem_BoolAt(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	item := NewBooleanItem(true, false, true)

	// Valid indices.
	b, err := item.BoolAt(0)
	require.NoError(err)
	require.True(b)

	b, err = item.BoolAt(1)
	require.NoError(err)
	require.False(b)

	b, err = item.BoolAt(2)
	require.NoError(err)
	require.True(b)

	// Out-of-range indices.
	_, err = item.BoolAt(-1)
	require.Error(err)

	_, err = item.BoolAt(3)
	require.Error(err)

	// Error item: BoolAt returns the deferred error regardless of index.
	errItem := NewBooleanItem(42)
	require.Error(errItem.Error())
	_, err = errItem.BoolAt(0)
	require.Error(err)
}

// TestBooleanItem_Iterator verifies Bools() and BoolAt().
func TestBooleanItem_Iterator(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	values := []bool{true, false, false, true}
	item := NewBooleanItem(values)

	// Bools() must yield all values in order.
	got := slices.Collect(item.Bools())
	require.Equal(values, got)

	// BoolAt valid indices.
	for i, v := range values {
		gotV, err := item.BoolAt(i)
		require.NoError(err)
		require.Equal(v, gotV)
	}

	// BoolAt out-of-range indices must return an error.
	_, err := item.BoolAt(-1)
	require.Error(err)

	_, err = item.BoolAt(len(values))
	require.Error(err)

	// Error item: Bools() yields nothing and BoolAt returns an error.
	errItem := NewBooleanItem(42)
	require.Error(errItem.Error())
	require.Empty(slices.Collect(errItem.Bools()))

	_, err = errItem.BoolAt(0)
	require.Error(err)
}

// TestBooleanItem_WrongType verifies that wrong-type accessors return errors on a BooleanItem.
func TestBooleanItem_WrongType(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	item := NewBooleanItem(true, false)
	require.NoError(item.Error())

	// ToInt must error on a boolean item.
	_, err := item.ToInt()
	require.Error(err)

	// ToASCII must error.
	_, err = item.ToASCII()
	require.Error(err)

	// Get is list-only in v2 — any call on a scalar item must error.
	got, err := item.Get()
	require.Nil(got)
	require.Error(err)

	got, err = item.Get(0)
	require.Nil(got)
	require.Error(err)
}

// TestBooleanItem_Concurrency exercises concurrent ToBytes / AppendTo / Bools reads under -race.
func TestBooleanItem_Concurrency(t *testing.T) {
	t.Parallel()

	item := NewBooleanItem(true, false, true)

	const goroutines = 50

	var wg sync.WaitGroup

	wg.Add(goroutines)

	for range goroutines {
		go func() {
			defer wg.Done()

			_ = item.ToBytes()

			buf := make([]byte, 0, item.EncodedLen())
			_ = item.AppendTo(buf)

			_ = slices.Collect(item.Bools())
		}()
	}

	wg.Wait()
}

// TestBooleanItem_Allocs asserts allocation counts for the hot encoding and accessor paths.
func TestBooleanItem_Allocs(t *testing.T) {
	item := NewBooleanItem(true, false, true)

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

	// ToBoolean must allocate exactly once (slices.Clone).
	allocs = testing.AllocsPerRun(100, func() {
		_, _ = item.ToBoolean()
	})

	if allocs != 1 {
		t.Errorf("ToBoolean: got %v allocs, want 1", allocs)
	}
}

// TestBooleanItem_EncodedLen verifies that EncodedLen() == len(ToBytes()) for all valid
// configurations and for an error item.
func TestBooleanItem_EncodedLen(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	cases := [][]any{
		nil,
		{true},
		{false, true, false},
		{[]bool{true, false, true, false}},
	}

	for _, vals := range cases {
		var item Item
		if vals == nil {
			item = NewBooleanItem()
		} else {
			item = NewBooleanItem(vals...)
		}

		require.NoError(item.Error())
		require.Equal(item.EncodedLen(), len(item.ToBytes()))
	}

	// Error item: both EncodedLen() and len(ToBytes()) must be 0.
	errItem := NewBooleanItem(42)
	require.Error(errItem.Error())
	require.Equal(0, errItem.EncodedLen())
	require.Equal(errItem.EncodedLen(), len(errItem.ToBytes()))
}

// TestCombineBoolValues exercises the full type matrix that combineBoolValues must handle.
func TestCombineBoolValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		values    []any
		want      []bool
		wantErr   bool
		errSubstr string
	}{
		{
			name:   "single bool true",
			values: []any{true},
			want:   []bool{true},
		},
		{
			name:   "single bool false",
			values: []any{false},
			want:   []bool{false},
		},
		{
			name:   "multiple bools",
			values: []any{true, false, true},
			want:   []bool{true, false, true},
		},
		{
			name:   "[]bool slice",
			values: []any{[]bool{false, true, false}},
			want:   []bool{false, true, false},
		},
		{
			name:   "mixed bool and []bool",
			values: []any{true, []bool{false, true}, false},
			want:   []bool{true, false, true, false},
		},
		{
			name:      "int not accepted",
			values:    []any{1},
			wantErr:   true,
			errSubstr: "input argument contains invalid type for BooleanItem",
		},
		{
			name:      "string not accepted",
			values:    []any{"true"},
			wantErr:   true,
			errSubstr: "input argument contains invalid type for BooleanItem",
		},
		{
			name:      "float64 not accepted",
			values:    []any{1.0},
			wantErr:   true,
			errSubstr: "input argument contains invalid type for BooleanItem",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			item := NewBooleanItem(test.values...)

			if test.wantErr {
				require.Error(t, item.Error())
				require.ErrorContains(t, item.Error(), test.errSubstr)

				return
			}

			require.NoError(t, item.Error())

			got, err := item.ToBoolean()
			require.NoError(t, err)
			require.Equal(t, test.want, got)
		})
	}
}
