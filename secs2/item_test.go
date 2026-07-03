package secs2

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestGetDataByteLength verifies that getDataByteLength returns the correct byte count for
// every registered type and rejects unknown types.
func TestGetDataByteLength(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	for dataType, it := range itemTypeMap {
		n, err := getDataByteLength(dataType, 1)
		require.NoError(err)
		require.Equal(it.Size, n)
	}

	n, err := getDataByteLength("invalid", 1)
	require.Error(err)
	require.Equal(0, n)
}

// TestGetHeaderBytes_boundaries exercises the 0-, 1-, 2-, and 3-length-byte boundaries for every
// registered item type and confirms the format byte is encoded correctly.
// The table loop is ported from the v1 item_test.go boundary sweep.
func TestGetHeaderBytes_boundaries(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	testIdx := 1

	for dataType, it := range itemTypeMap {
		t.Logf("Test #%d: type=%s byteSize=%d", testIdx, dataType, it.Size)
		testIdx++

		for dataLen := 0xFF; dataLen*it.Size <= MaxByteSize; dataLen <<= 1 {
			dataByteSize := dataLen * it.Size

			wantLenByteCount := 1
			if dataByteSize>>16 > 0 {
				wantLenByteCount = 3
			} else if dataByteSize>>8 > 0 {
				wantLenByteCount = 2
			}

			header, err := getHeaderBytes(dataType, dataLen, 0)
			require.NoError(err)
			require.Equal(byte(it.FormatCode)<<2+byte(wantLenByteCount), header[0])
		}
	}

	// invalid type
	header, err := getHeaderBytes("invalid", 0, 0)
	require.Error(err)
	require.Empty(header)

	// size limit exceeded
	header, err = getHeaderBytes(ASCIIType, MaxByteSize+1, 0)
	require.Error(err)
	require.Empty(header)
}

// TestAppendHeaderBytes_specificBoundaries verifies exact wire bytes at 1-, 2-, and 3-length-byte
// boundaries and the 0-element case.
func TestAppendHeaderBytes_specificBoundaries(t *testing.T) {
	t.Parallel()

	t.Run("zero-elements", func(t *testing.T) {
		t.Parallel()

		dst, err := appendHeaderBytes(nil, ASCIIType, 0)
		require.NoError(t, err)
		require.Len(t, dst, 2)
		require.Equal(t, byte(ASCIIFormatCode<<2+1), dst[0])
		require.Equal(t, byte(0x00), dst[1])
	})

	t.Run("one-length-byte", func(t *testing.T) {
		t.Parallel()

		// size=0xFF → dataByteLength=0xFF → fits in 1 length byte
		dst, err := appendHeaderBytes(nil, ASCIIType, 0xFF)
		require.NoError(t, err)
		require.Len(t, dst, 2)
		require.Equal(t, byte(ASCIIFormatCode<<2+1), dst[0])
		require.Equal(t, byte(0xFF), dst[1])
	})

	t.Run("two-length-bytes", func(t *testing.T) {
		t.Parallel()

		// size=0x100 → dataByteLength=0x100 → needs 2 length bytes
		dst, err := appendHeaderBytes(nil, ASCIIType, 0x100)
		require.NoError(t, err)
		require.Len(t, dst, 3)
		require.Equal(t, byte(ASCIIFormatCode<<2+2), dst[0])
		require.Equal(t, byte(0x01), dst[1])
		require.Equal(t, byte(0x00), dst[2])
	})

	t.Run("three-length-bytes", func(t *testing.T) {
		t.Parallel()

		// size=0x10000 → dataByteLength=0x10000 → needs 3 length bytes
		dst, err := appendHeaderBytes(nil, ASCIIType, 0x10000)
		require.NoError(t, err)
		require.Len(t, dst, 4)
		require.Equal(t, byte(ASCIIFormatCode<<2+3), dst[0])
		require.Equal(t, byte(0x01), dst[1])
		require.Equal(t, byte(0x00), dst[2])
		require.Equal(t, byte(0x00), dst[3])
	})

	t.Run("invalid-type", func(t *testing.T) {
		t.Parallel()

		dst, err := appendHeaderBytes(nil, "invalid", 0)
		require.Error(t, err)
		require.Empty(t, dst)
	})

	t.Run("size-limit-exceeded", func(t *testing.T) {
		t.Parallel()

		dst, err := appendHeaderBytes(nil, ASCIIType, MaxByteSize+1)
		require.Error(t, err)
		require.Empty(t, dst)
	})
}

// TestAppendHeaderBytes_zeroAllocs verifies that appending into a pre-sized buffer costs no heap
// allocations.
func TestAppendHeaderBytes_zeroAllocs(t *testing.T) {
	buf := make([]byte, 0, 4)

	allocs := testing.AllocsPerRun(100, func() {
		buf = buf[:0]
		buf, _ = appendHeaderBytes(buf, ASCIIType, 42)
	})

	if allocs > 0 {
		t.Errorf("appendHeaderBytes into pre-sized buffer: got %v allocs, want 0", allocs)
	}
}

// TestEmptyItem checks all Item interface methods on EmptyItem.
func TestEmptyItem(t *testing.T) {
	t.Parallel()

	item := NewEmptyItem()

	require.Equal(t, EmptyType, item.Type())
	require.Equal(t, 0, item.Size())
	require.Equal(t, 0, item.EncodedLen())
	require.Equal(t, []byte{}, item.ToBytes())
	require.Equal(t, "", item.ToSML())
	require.True(t, item.IsEmpty())
	require.NoError(t, item.Error())

	// AppendTo must return dst unchanged
	buf := []byte{0xAA, 0xBB}
	result := item.AppendTo(buf)
	require.Equal(t, []byte{0xAA, 0xBB}, result)

	// Get() with no indices returns self
	got, err := item.Get()
	require.NoError(t, err)
	require.True(t, item == got, "Get() should return the same Item value")

	// Get() with any index returns an error
	got, err = item.Get(0)
	require.Error(t, err)
	require.Nil(t, got)
}

// TestBaseItemDefaults verifies that baseItem default methods return type-mismatch errors and
// empty iterators, tested indirectly through EmptyItem.
func TestBaseItemDefaults(t *testing.T) {
	t.Parallel()

	item := NewEmptyItem()

	// Accessor methods that EmptyItem does not override must return an error.
	_, err := item.ToInt()
	require.Error(t, err)

	_, err = item.IntAt(0)
	require.Error(t, err)

	// Iterator methods must yield nothing.
	collected := slices.Collect(item.Ints())
	require.Empty(t, collected)
}
