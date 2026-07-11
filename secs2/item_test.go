package secs2

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

// refItemType is the parity oracle's copy of the pre-refactor itemType encoding metadata (format
// code + bytes per element). It is a test-local copy so the production string-keyed
// itemTypeMap/getDataByteLength/appendHeaderBytes could be deleted from item.go without losing
// parity coverage.
type refItemType struct {
	FormatCode FormatCode
	Size       int
}

// refItemTypeMap is the parity oracle's copy of the pre-refactor itemTypeMap.
var refItemTypeMap = map[string]*refItemType{
	ListType:         {FormatCode: ListFormatCode, Size: 1},
	BinaryType:       {FormatCode: BinaryFormatCode, Size: 1},
	BooleanType:      {FormatCode: BooleanFormatCode, Size: 1},
	ASCIIType:        {FormatCode: ASCIIFormatCode, Size: 1},
	JIS8Type:         {FormatCode: JIS8FormatCode, Size: 1},
	LocalizedStrType: {FormatCode: LocalizedStrFormatCode, Size: 1},
	Int64Type:        {FormatCode: Int64FormatCode, Size: 8},
	Int8Type:         {FormatCode: Int8FormatCode, Size: 1},
	Int16Type:        {FormatCode: Int16FormatCode, Size: 2},
	Int32Type:        {FormatCode: Int32FormatCode, Size: 4},
	Float64Type:      {FormatCode: Float64FormatCode, Size: 8},
	Float32Type:      {FormatCode: Float32FormatCode, Size: 4},
	Uint64Type:       {FormatCode: Uint64FormatCode, Size: 8},
	Uint8Type:        {FormatCode: Uint8FormatCode, Size: 1},
	Uint16Type:       {FormatCode: Uint16FormatCode, Size: 2},
	Uint32Type:       {FormatCode: Uint32FormatCode, Size: 4},
}

// refGetDataByteLength is the parity oracle's copy of the pre-refactor getDataByteLength.
func refGetDataByteLength(dataType string, size int) (int, error) {
	it, ok := refItemTypeMap[dataType]
	if !ok {
		return 0, fmt.Errorf("invalid item type %s", dataType)
	}

	return size * it.Size, nil
}

// refAppendHeaderBytes is the parity oracle's copy of the pre-refactor appendHeaderBytes. It
// reproduces the string-map lookup logic byte-for-byte so appendHeaderBytesFC's output can be
// checked against it without keeping the production string-keyed helpers around.
func refAppendHeaderBytes(dst []byte, dataType string, size int) ([]byte, error) {
	it, ok := refItemTypeMap[dataType]
	if !ok {
		return dst, fmt.Errorf("invalid item type: %s", dataType)
	}

	dataByteLength, err := refGetDataByteLength(dataType, size)
	if err != nil {
		return dst, err
	}

	if dataByteLength > MaxByteSize {
		return dst, errors.New("size limit exceeded")
	}

	lenBytes := [3]byte{byte(dataByteLength >> 16), byte(dataByteLength >> 8), byte(dataByteLength)}
	lenByteCount := 3

	if lenBytes[0] == 0 {
		lenByteCount--
		if lenBytes[1] == 0 {
			lenByteCount--
		}
	}

	dst = append(dst, byte(it.FormatCode)<<2+byte(lenByteCount))
	dst = append(dst, lenBytes[3-lenByteCount:]...)

	return dst, nil
}

// TestAppendHeaderBytesFC_ParityWithReference verifies that appendHeaderBytesFC produces
// byte-identical output to the pre-refactor string-map-based reference encoder, across every
// registered type and an exponential sweep of element counts (mirroring the v1 boundary sweep),
// plus the size-limit-exceeded error case for every type.
func TestAppendHeaderBytesFC_ParityWithReference(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	for dataType, it := range refItemTypeMap {
		for _, dataLen := range []int{0, 1} {
			wantDst, wantErr := refAppendHeaderBytes(nil, dataType, dataLen)
			gotDst, gotErr := appendHeaderBytesFC(nil, it.FormatCode, dataLen*it.Size)

			require.NoError(wantErr, "type=%s dataLen=%d", dataType, dataLen)
			require.NoError(gotErr, "type=%s dataLen=%d", dataType, dataLen)
			require.Equal(wantDst, gotDst, "type=%s dataLen=%d", dataType, dataLen)
		}

		for dataLen := 0xFF; dataLen*it.Size <= MaxByteSize; dataLen <<= 1 {
			wantDst, wantErr := refAppendHeaderBytes(nil, dataType, dataLen)
			gotDst, gotErr := appendHeaderBytesFC(nil, it.FormatCode, dataLen*it.Size)

			require.NoError(wantErr, "type=%s dataLen=%d", dataType, dataLen)
			require.NoError(gotErr, "type=%s dataLen=%d", dataType, dataLen)
			require.Equal(wantDst, gotDst, "type=%s dataLen=%d", dataType, dataLen)
		}

		// Size-limit-exceeded parity: one element past the type's max representable count.
		oversized := MaxByteSize/it.Size + 1

		wantDst, wantErr := refAppendHeaderBytes(nil, dataType, oversized)
		gotDst, gotErr := appendHeaderBytesFC(nil, it.FormatCode, oversized*it.Size)

		require.Error(wantErr, "type=%s oversized=%d", dataType, oversized)
		require.Error(gotErr, "type=%s oversized=%d", dataType, oversized)
		require.Empty(wantDst)
		require.Empty(gotDst)
	}
}

// TestAppendHeaderBytesFC_LengthFieldBoundaries exercises the exact 0-, 255/256-, 65535/65536-,
// and MaxByteSize/MaxByteSize+1-length-field boundaries directly on appendHeaderBytesFC, verifying
// the 1-, 2-, and 3-length-byte header encodings and the size-limit-exceeded error.
func TestAppendHeaderBytesFC_LengthFieldBoundaries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		lengthField      int
		wantLenByteCount byte
		wantErr          bool
	}{
		{"zero", 0, 1, false},
		{"one-length-byte-max", 255, 1, false},
		{"two-length-byte-min", 256, 2, false},
		{"two-length-byte-max", 65535, 2, false},
		{"three-length-byte-min", 65536, 3, false},
		{"MaxByteSize", MaxByteSize, 3, false},
		{"MaxByteSize+1", MaxByteSize + 1, 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dst, err := appendHeaderBytesFC(nil, ASCIIFormatCode, tt.lengthField)

			if tt.wantErr {
				require.Error(t, err)
				require.Empty(t, dst)

				return
			}

			require.NoError(t, err)
			require.Len(t, dst, 1+int(tt.wantLenByteCount))
			require.Equal(t, byte(ASCIIFormatCode)<<2+tt.wantLenByteCount, dst[0])

			lenBytes := [3]byte{byte(tt.lengthField >> 16), byte(tt.lengthField >> 8), byte(tt.lengthField)}
			require.Equal(t, lenBytes[3-int(tt.wantLenByteCount):], dst[1:])
		})
	}
}

// TestAppendHeaderBytesFC_zeroAllocs verifies that appending into a pre-sized buffer costs no heap
// allocations.
func TestAppendHeaderBytesFC_zeroAllocs(t *testing.T) {
	buf := make([]byte, 0, 4)

	allocs := testing.AllocsPerRun(100, func() {
		buf = buf[:0]
		buf, _ = appendHeaderBytesFC(buf, ASCIIFormatCode, 42)
	})

	if allocs > 0 {
		t.Errorf("appendHeaderBytesFC into pre-sized buffer: got %v allocs, want 0", allocs)
	}
}

// TestGetHeaderBytes_boundaries exercises the 0-, 1-, 2-, and 3-length-byte boundaries for every
// registered item type and confirms the format byte is encoded correctly.
func TestGetHeaderBytes_boundaries(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	testIdx := 1

	for dataType, it := range refItemTypeMap {
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

			header, err := getHeaderBytes(it.FormatCode, dataByteSize, 0)
			require.NoError(err)
			require.Equal(byte(it.FormatCode)<<2+byte(wantLenByteCount), header[0])
		}
	}

	// size limit exceeded
	header, err := getHeaderBytes(ASCIIFormatCode, MaxByteSize+1, 0)
	require.Error(err)
	require.Empty(header)
}

// numericBoundaryCounts returns the per-element-width count boundaries called out in the task
// brief: the last count that still fits in 1 length byte, the first that needs 2, the last that
// fits in 2, the first that needs 3, and the largest count representable at MaxByteSize.
func numericBoundaryCounts(width int) []int {
	return []int{
		255 / width,
		255/width + 1,
		65535 / width,
		65535/width + 1,
		MaxByteSize / width,
	}
}

// verifyNumericHeader asserts that dst starts with the SECS-II header expected for fc and a
// length-field value of dataByteLength (element count × width), matching today's 1→2→3
// length-byte transitions.
func verifyNumericHeader(t *testing.T, dst []byte, fc FormatCode, dataByteLength int) {
	t.Helper()

	lenBytes := [3]byte{byte(dataByteLength >> 16), byte(dataByteLength >> 8), byte(dataByteLength)}
	wantLenByteCount := 3

	if lenBytes[0] == 0 {
		wantLenByteCount--
		if lenBytes[1] == 0 {
			wantLenByteCount--
		}
	}

	require.GreaterOrEqual(t, len(dst), 1+wantLenByteCount)
	require.Equal(t, byte(fc)<<2+byte(wantLenByteCount), dst[0])
	require.Equal(t, lenBytes[3-wantLenByteCount:], dst[1:1+wantLenByteCount])
}

// TestNumericAppendTo_ElementCountBoundaries verifies that IntItem/UintItem/FloatItem.AppendTo
// convert element count × byte width into the correct length-field header at the same
// element-count boundaries as before the refactor (the count×width conversion now lives at each
// AppendTo call site instead of inside a shared string-keyed helper).
//
// Items are constructed directly via struct literals (this file is in package secs2) with a
// nil values slice: AppendTo writes the header from size/byteSize alone before it ranges over
// values, so the boundary assertion below — which only inspects the header — does not need real
// payload data, letting the largest counts (up to MaxByteSize) run without huge allocations.
func TestNumericAppendTo_ElementCountBoundaries(t *testing.T) {
	t.Parallel()

	t.Run("Int", func(t *testing.T) {
		t.Parallel()

		for _, width := range []int{1, 2, 4, 8} {
			for _, count := range numericBoundaryCounts(width) {
				item := &IntItem{size: int32(count), byteSize: uint32(width)} //nolint:gosec
				fc, ok := item.formatCode()
				require.True(t, ok)

				dst := item.AppendTo(nil)
				verifyNumericHeader(t, dst, fc, count*width)
			}
		}
	})

	t.Run("Uint", func(t *testing.T) {
		t.Parallel()

		for _, width := range []int{1, 2, 4, 8} {
			for _, count := range numericBoundaryCounts(width) {
				item := &UintItem{size: int32(count), byteSize: uint32(width)} //nolint:gosec
				fc, ok := item.formatCode()
				require.True(t, ok)

				dst := item.AppendTo(nil)
				verifyNumericHeader(t, dst, fc, count*width)
			}
		}
	})

	t.Run("Float", func(t *testing.T) {
		t.Parallel()

		for _, width := range []int{4, 8} {
			for _, count := range numericBoundaryCounts(width) {
				item := &FloatItem{size: int32(count), byteSize: uint32(width)} //nolint:gosec
				fc, ok := item.formatCode()
				require.True(t, ok)

				dst := item.AppendTo(nil)
				verifyNumericHeader(t, dst, fc, count*width)
			}
		}
	})
}

// TestListItem_AppendTo_ChildCountBoundaries verifies that ListItem.AppendTo's header encodes the
// direct-child count (not payload bytes) at the same child-count boundaries as the leaf-payload
// boundaries, and that each child's encoded bytes follow the header unchanged.
func TestListItem_AppendTo_ChildCountBoundaries(t *testing.T) {
	t.Parallel()

	childCounts := []int{0, 1, 255, 256, 65535, 65536}

	for _, count := range childCounts {
		t.Run(fmt.Sprintf("children=%d", count), func(t *testing.T) {
			t.Parallel()

			children := make([]Item, count)
			for i := range children {
				children[i] = NewBooleanItem(true)
			}

			list := NewListItem(children...)
			require.NoError(t, list.Error())

			got := list.AppendTo(nil)

			wantHeader, err := appendHeaderBytesFC(nil, ListFormatCode, count)
			require.NoError(t, err)
			require.True(t, len(got) >= len(wantHeader))
			require.Equal(t, wantHeader, got[:len(wantHeader)])

			// Each child follows the header unchanged, in order.
			want := slices.Clone(wantHeader)
			for _, child := range children {
				want = child.AppendTo(want)
			}
			require.Equal(t, want, got)
		})
	}
}

// TestZeroValueNumericItems locks the exact current observable triple for a directly-constructed
// zero-value IntItem{}/UintItem{}/FloatItem{} (byteSize 0, an invalid width these exported structs
// permit since all fields are unexported): AppendTo(prefix) returns prefix unchanged, ToBytes() is
// empty, and EncodedLen() == 2. All three are asserted together so a change cannot satisfy one
// while silently breaking another.
func TestZeroValueNumericItems(t *testing.T) {
	t.Parallel()

	prefix := []byte{0xDE, 0xAD, 0xBE, 0xEF}

	t.Run("IntItem", func(t *testing.T) {
		t.Parallel()

		item := &IntItem{}
		require.Equal(t, prefix, item.AppendTo(slices.Clone(prefix)))
		require.Empty(t, item.ToBytes())
		require.Equal(t, 2, item.EncodedLen())
	})

	t.Run("UintItem", func(t *testing.T) {
		t.Parallel()

		item := &UintItem{}
		require.Equal(t, prefix, item.AppendTo(slices.Clone(prefix)))
		require.Empty(t, item.ToBytes())
		require.Equal(t, 2, item.EncodedLen())
	})

	t.Run("FloatItem", func(t *testing.T) {
		t.Parallel()

		item := &FloatItem{}
		require.Equal(t, prefix, item.AppendTo(slices.Clone(prefix)))
		require.Empty(t, item.ToBytes())
		require.Equal(t, 2, item.EncodedLen())
	})
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
