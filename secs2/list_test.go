package secs2

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestListItem(t *testing.T) {
	tests := []struct {
		description     string // Test case description
		input           []Item // Input
		expectedSize    int    // expected result from Size()
		expectedToBytes []byte // expected result from ToBytes()
		expectedToSML   string // expected result from String()
	}{
		{
			description:     "Size: 0",
			input:           []Item{},
			expectedSize:    0,
			expectedToBytes: []byte{0x01, 0},
			expectedToSML:   `<L[0]>`,
		},

		{
			description:     "Size: 3, Contains items",
			input:           []Item{NewASCIIItem("text"), NewIntItem(1, 1, 2, 3), NewIntItem(1, 4, 5, 6)},
			expectedSize:    3,
			expectedToBytes: []byte{0x1, 0x3, 0x41, 0x4, 0x74, 0x65, 0x78, 0x74, 0x65, 0x3, 0x1, 0x2, 0x3, 0x65, 0x3, 0x4, 0x5, 0x6},
			expectedToSML: `<L[3]
  <A[4] "text">
  <I1[3] 1 2 3>
  <I1[3] 4 5 6>
>`,
		},
		{
			description: "Size: 2, Nested list with items",
			input: []Item{
				NewListItem(),
				NewListItem(NewIntItem(1, 100, 101)),
			},
			expectedSize:    2,
			expectedToBytes: []byte{0x1, 0x2, 0x1, 0x0, 0x1, 0x1, 0x65, 0x2, 0x64, 0x65},
			expectedToSML: `<L[2]
  <L[0]>
  <L[1]
    <I1[2] 100 101>
  >
>`,
		},
		{
			description: "Size: 2, mixed items and nested list with items",
			input: []Item{
				NewListItem(
					NewIntItem(2, 100, 200),
					NewListItem(NewASCIIItem("text")),
				),
				NewFloatItem(4, 33.3, 56.13),
			},
			expectedSize: 2,
			expectedToBytes: []byte{
				0x1, 0x2,
				0x1, 0x2,
				0x69, 0x4, 0x0, 0x64, 0x0, 0xc8,
				0x1, 0x1,
				0x41, 0x4, 0x74, 0x65, 0x78, 0x74,
				0x91, 0x8, 0x42, 0x5, 0x33, 0x33, 0x42, 0x60, 0x85, 0x1f,
			},
			expectedToSML: `<L[2]
  <L[2]
    <I2[2] 100 200>
    <L[1]
      <A[4] "text">
    >
  >
  <F4[2] 33.3 56.13>
>`,
		},
	}

	require := require.New(t)

	for i, test := range tests {
		t.Logf("Test #%d: %s", i, test.description)
		node := NewListItem(test.input...)
		require.NoError(node.Error())
		require.Equal(test.expectedSize, node.Size())
		require.Equal(test.expectedToBytes, node.ToBytes())
		require.Equal(test.expectedToSML, node.ToSML())
	}
}

func TestListItem_Methods(t *testing.T) {
	require := require.New(t)

	item := BOOLEAN(true, false, true)
	listItem := L(item)
	values := listItem.Values()
	items, ok := values.([]Item)
	require.True(ok)
	require.Equal(item, items[0])

	items2, err := listItem.ToList()
	require.NoError(err)
	require.Equal(items2, items)

	newItem := I4(1, 2, 3, 4)
	err = listItem.SetValues(newItem)
	require.NoError(err)

	values = listItem.Values()
	items, ok = values.([]Item)
	require.True(ok)
	require.Equal(newItem, items[0])

	listItem.Free()
}

func TestListItem_Get(t *testing.T) {
	require := require.New(t)

	nestedItem := L(
		L(
			I1(1, 2, 3),
			L(
				I4(4, 5, 6),
				L(
					F8(1.1, 2.2),
				),
			),
		),
	)
	_, err := nestedItem.Get(1)
	require.Error(err)

	item, err := nestedItem.Get(0, 0)
	require.NoError(err)
	values, err := item.ToInt()
	require.NoError(err)
	require.Equal([]int64{1, 2, 3}, values)
	require.Equal([]int64{1, 2, 3}, item.Values())

	item, err = nestedItem.Get(0, 1)
	require.NoError(err)
	require.True(item.IsList())

	item, err = nestedItem.Get(0, 1, 0)
	require.NoError(err)
	require.True(item.IsInt32())
	values, err = item.ToInt()
	require.NoError(err)
	require.Equal([]int64{4, 5, 6}, values)
	require.Equal([]int64{4, 5, 6}, item.Values())

	item, err = nestedItem.Get(0, 1, 1, 0)
	require.NoError(err)
	require.True(item.IsFloat64())
	floatValues, err := item.ToFloat()
	require.NoError(err)
	require.Equal([]float64{1.1, 2.2}, floatValues)
	require.Equal([]float64{1.1, 2.2}, item.Values())
}

func TestListItem_Errors(t *testing.T) {
	require := require.New(t)

	item := NewListItem(NewIntItem(1, "string"))
	require.Error(item.Error())

	item = NewListItem(
		NewListItem(
			NewIntItem(1, 255),
			NewIntItem(1, "string"),
			NewListItem(
				NewIntItem(2, math.MaxInt32),
			),
		),
	)
	require.Error(item.Error())
}
