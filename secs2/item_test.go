package secs2

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestItem_getDataByteLength(t *testing.T) {
	require := require.New(t)

	for dataType, itemType := range itemTypeMap {
		dataByteLength, err := getDataByteLength(dataType, 1)
		require.NoError(err)
		require.Equal(itemType.Size, dataByteLength)
	}

	dataByteLength, err := getDataByteLength("invalid", 1)
	require.Error(err)
	require.Equal(0, dataByteLength)
}

func TestItem_getHeaderBytes(t *testing.T) {
	require := require.New(t)

	testIdx := 1
	for dataType, itemType := range itemTypeMap {
		t.Logf("Test #%d: Item item data type: %s, data byte size: %d", testIdx, dataType, itemType.Size)
		testIdx++

		for dataLen := 0xFF; dataLen*itemType.Size <= MaxByteSize; dataLen <<= 1 {
			dataByteSize := dataLen * itemType.Size

			lenByteCount := 1
			if dataByteSize>>16 > 0 {
				lenByteCount = 3
			} else if dataByteSize>>8 > 0 {
				lenByteCount = 2
			}

			header, err := getHeaderBytes(dataType, dataLen, 0)
			require.NoError(err)
			require.Equal(byte(itemType.FormatCode<<2+lenByteCount), header[0])
		}
	}

	header, err := getHeaderBytes("invalid", 0, 0)
	require.Error(err)
	require.Len(header, 0)

	header, err = getHeaderBytes(ASCIIType, MaxByteSize+1, 0)
	require.Error(err)
	require.Len(header, 0)
}

func TestItem_baseItem(t *testing.T) {
	require := require.New(t)

	item := &baseItem{}
	nestedItem, err := item.ToList()
	require.Nil(nestedItem)
	require.Error(err)
	require.Error(item.Error())

	binaryVal, err := item.ToBinary()
	require.Nil(binaryVal)
	require.Error(err)
	require.Error(item.Error())

	boolVal, err := item.ToBoolean()
	require.Nil(boolVal)
	require.Error(err)
	require.Error(item.Error())

	asciiVal, err := item.ToASCII()
	require.Empty(asciiVal)
	require.Error(err)
	require.Error(item.Error())
}

func TestItem_EmptyItem(t *testing.T) {
	require := require.New(t)

	item := NewEmptyItem()
	curItem, err := item.Get()
	require.NoError(err)
	require.Exactly(item, curItem)

	curItem, err = item.Get(1)
	require.Error(err)
	require.Nil(curItem)

	require.Equal(0, item.Size())
	require.Equal([]string{}, item.Values())
	require.NoError(item.SetValues())
	require.NoError(item.SetValues(1, 2, 3))
	require.Equal([]byte{}, item.ToBytes())
	require.Equal("", item.ToSML())
	require.IsType(&EmptyItem{}, item.Clone())
}

func TestItem_ItemError(t *testing.T) {
	require := require.New(t)

	itemErr := &ItemError{}
	strErr := errors.New("")

	err := newItemErrorWithMsg("test")
	require.ErrorAs(err, &itemErr)
	require.ErrorContains(err, "test")

	err = newItemError(errors.New("basic error"))
	require.ErrorAs(err, &itemErr)
	require.ErrorContains(err, "basic error")
	require.ErrorContains(err.Unwrap(), "basic error")
	require.ErrorAs(err.Unwrap(), &strErr)

	err = newItemError(newItemErrorWithMsg("item item error"))
	require.ErrorAs(err, &itemErr)
	require.ErrorContains(err, "item item error")

	require.ErrorContains(err.Unwrap(), "item item error")
	require.ErrorAs(err.Unwrap(), &strErr)
}
