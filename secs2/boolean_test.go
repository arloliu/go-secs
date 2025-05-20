//nolint:errcheck
package secs2

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBooleanItem(t *testing.T) {
	tests := []struct {
		description     string // Test case description
		input           []any  // Input
		expectedSize    int    // expected result from Size()
		expectedGet     []bool // expected result result from Get()
		expectedToBytes []byte // expected result from ToBytes()
		expectedToSML   string // expected result from String()
	}{
		{
			description:     "Size: 0",
			input:           []any{},
			expectedSize:    0,
			expectedGet:     []bool{},
			expectedToBytes: []byte{37, 0},
			expectedToSML:   "<BOOLEAN[0]>",
		},
		{
			description:     "Size: 1",
			input:           []any{false},
			expectedSize:    1,
			expectedGet:     []bool{false},
			expectedToBytes: []byte{37, 1, 0},
			expectedToSML:   "<BOOLEAN[1] False>",
		},
		{
			description:     "Size: 3",
			input:           []any{false, true, true},
			expectedSize:    3,
			expectedGet:     []bool{false, true, true},
			expectedToBytes: []byte{37, 3, 0, 1, 1},
			expectedToSML:   "<BOOLEAN[3] False True True>",
		},
		{
			description:     "Size: 3, Bool slice",
			input:           []any{[]bool{true, false, true}},
			expectedSize:    3,
			expectedGet:     []bool{true, false, true},
			expectedToBytes: []byte{37, 3, 1, 0, 1},
			expectedToSML:   "<BOOLEAN[3] True False True>",
		},
		{
			description:     "Size: 6, Bool and bool slice",
			input:           []any{true, []bool{false, true}, false, []bool{true, false}},
			expectedSize:    6,
			expectedGet:     []bool{true, false, true, false, true, false},
			expectedToBytes: []byte{37, 6, 1, 0, 1, 0, 1, 0},
			expectedToSML:   "<BOOLEAN[6] True False True False True False>",
		},
	}

	require := require.New(t)

	for i, test := range tests {
		t.Logf("Test #%d: %s", i, test.description)
		item := NewBooleanItem(test.input...)
		require.NoError(item.Error())
		require.Equal(test.expectedToBytes, item.ToBytes())
		require.Equal(test.expectedSize, item.Size())
		require.Equal(test.expectedToSML, item.ToSML())
		require.Equal(test.expectedGet, item.Values().([]bool))

		val, err := item.ToBoolean()
		require.NoError(err)
		require.Equal(test.expectedGet, val)

		nestedItem, err := item.Get()
		require.NoError(err)
		require.Equal(test.expectedGet, nestedItem.Values().([]bool))

		nestedItem, err = item.Get(0)
		require.Nil(nestedItem)
		require.ErrorContains(err, fmt.Sprintf("item is not a list, item is %s", item.ToSML()))

		// create a empty item and call SetVariables
		emptyItem := NewBooleanItem()
		err = emptyItem.SetValues(test.input...)
		require.NoError(err)
		require.Equal(test.expectedSize, emptyItem.Size())
		require.Equal(test.expectedToBytes, emptyItem.ToBytes())
		require.Equal(test.expectedToSML, emptyItem.ToSML())
		require.Equal(test.expectedGet, emptyItem.Values().([]bool))

		// clone a item, it should contains the same content as original item.
		clonedItem := item.Clone()
		require.Equal(test.expectedSize, clonedItem.Size())
		require.Equal(test.expectedToBytes, clonedItem.ToBytes())
		require.Equal(test.expectedToSML, clonedItem.ToSML())
		require.Equal(test.expectedGet, clonedItem.Values().([]bool))

		// set a random string to cloned item
		randVal := genRandomBool(test.expectedSize)
		err = clonedItem.SetValues(randVal)
		require.NoError(err)
		require.Equal(test.expectedSize, clonedItem.Size())
		require.Equal(randVal, clonedItem.Values().([]bool))

		// the original item should not be modified
		require.Equal(test.expectedGet, item.Values().([]bool))
	}
}

func BenchmarkBooleanItem_Create(b *testing.B) {
	values := genFixedBool(1000)

	b.ResetTimer()
	for i := 0; i <= b.N; i++ {
		_, _ = NewBooleanItem(values).(*BooleanItem)
	}
	b.StopTimer()
}

func BenchmarkBooleanItem_ToBytes(b *testing.B) {
	values := genFixedBool(1000)

	item, _ := NewBooleanItem(values).(*BooleanItem)

	b.ResetTimer()
	for i := 0; i <= b.N; i++ {
		_ = item.ToBytes()
	}
	b.StopTimer()
}

func BenchmarkBooleanItem_ToSML(b *testing.B) {
	values := genFixedBool(1000)
	item := NewBooleanItem(values)

	b.ResetTimer()
	for i := 0; i <= b.N; i++ {
		_ = item.ToSML()
	}
	b.StopTimer()
}

func genRandomBool(length int) []bool {
	if length == 0 {
		return []bool{}
	}
	seededRand := rand.New(rand.NewSource(time.Now().UnixNano()))

	b := make([]bool, length)
	for i := range b {
		if seededRand.Intn(256)%2 == 0 {
			b[i] = true
		}
	}

	return b
}

func genFixedBool(length int) []bool {
	if length == 0 {
		return []bool{}
	}

	b := make([]bool, length)
	for i := range b {
		if i%2 == 0 {
			b[i] = true
		} else {
			b[i] = false
		}
	}

	return b
}
