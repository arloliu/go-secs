//nolint:errcheck
package sml

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRawSMLItem(t *testing.T) {
	tests := []struct {
		description     string
		input           []any
		expectedSize    int
		expectedValues  []byte
		expectedToBytes []byte
		expectedToSML   string
	}{
		{
			description:     "Empty input",
			input:           []any{},
			expectedSize:    0,
			expectedValues:  []byte{},
			expectedToBytes: []byte{},
			expectedToSML:   "",
		},
		{
			description:     "Single string input",
			input:           []any{"<I4 255>"},
			expectedSize:    8,
			expectedValues:  []byte("<I4 255>"),
			expectedToBytes: []byte{0x71, 0x4, 0x0, 0x0, 0x0, 0xff},
			expectedToSML:   "<I4[1] 255>",
		},
		{
			description:     "Multiple string inputs",
			input:           []any{"<F4", " ", "3.14>"},
			expectedSize:    9,
			expectedValues:  []byte("<F4 3.14>"),
			expectedToBytes: []byte{0x91, 0x4, 0x40, 0x48, 0xf5, 0xc3},
			expectedToSML:   "<F4[1] 3.1400001>",
		},
	}

	require := require.New(t)

	for i, test := range tests {
		t.Logf("Test #%d: %s", i, test.description)
		item := NewRawSMLItem(nil, false)
		err := item.SetValues(test.input...)
		require.NoError(err)
		require.Equal(test.expectedSize, item.Size())
		require.Equal(test.expectedValues, item.Values().([]byte))
		require.Equalf(test.expectedToBytes, item.ToBytes(), "byte string: %s", item.ToBytes())
		require.Equal(test.expectedToSML, item.ToSML())

		// Test Clone
		clonedItem := item.Clone()
		require.Equal(test.expectedSize, clonedItem.Size())
		require.Equal(test.expectedValues, clonedItem.Values().([]byte))
		require.Equal(test.expectedToBytes, clonedItem.ToBytes())
		require.Equal(test.expectedToSML, clonedItem.ToSML())
	}

	// Test Get method
	item := NewRawSMLItem([]byte("test"), false)
	nestedItem, err := item.Get()
	require.NoError(err)
	require.Equal([]byte("test"), nestedItem.Values().([]byte))

	nestedItem, err = item.Get(0)
	require.Nil(nestedItem)
	require.ErrorContains(err, "item is not a list")

	// Test error handling in SetValues
	item = NewRawSMLItem(nil, false)
	err = item.SetValues(123)
	require.Error(err)
	require.Equal([]byte(nil), item.Values().([]byte))
}
