package secs2

import (
	"testing"
	"unsafe"

	"github.com/arloliu/go-secs/v2/internal/framecodec"
	"github.com/stretchr/testify/require"
)

func TestDecodeOwnedFrame_MatchesDecode(t *testing.T) {
	src := NewListItem(NewUintItem(1, 1, 2, 3), NewASCIIItem("hi")).ToBytes()
	want, err := Decode(src)
	require.NoError(t, err)

	owned := append([]byte(nil), src...) // a buffer the library "owns"
	got, err := DecodeOwnedFrame(framecodec.AdoptSECS2Body(owned))
	require.NoError(t, err)
	require.Equal(t, want.ToBytes(), got.ToBytes())
}

func TestDecodeOwnedFrame_Empty(t *testing.T) {
	got, err := DecodeOwnedFrame(framecodec.AdoptSECS2Body(nil))
	require.NoError(t, err)
	require.Equal(t, 0, got.Size())
}

func TestDecodeOwnedFrame_NoClone(t *testing.T) {
	// DecodeOwnedFrame must NOT clone: it decodes directly over the token's bytes.
	src := NewASCIIItem("ABCDE").ToBytes()
	owned := append([]byte(nil), src...)
	allocs := testing.AllocsPerRun(50, func() {
		_, _ = DecodeOwnedFrame(framecodec.AdoptSECS2Body(owned))
	})
	plain := testing.AllocsPerRun(50, func() {
		_, _ = Decode(src)
	})
	require.Less(t, allocs, plain, "DecodeOwnedFrame must allocate fewer than Decode (no bytes.Clone)")
}

func TestDecodeOwned_MatchesDecode(t *testing.T) {
	src := NewListItem(NewUintItem(1, 1, 2, 3), NewASCIIItem("hi")).ToBytes()
	want, err := Decode(src)
	require.NoError(t, err)

	owned := append([]byte(nil), src...) // a buffer the caller owns outright
	got, err := DecodeOwned(owned)
	require.NoError(t, err)
	require.Equal(t, want.ToBytes(), got.ToBytes())
}

func TestDecodeOwned_Empty(t *testing.T) {
	got, err := DecodeOwned(nil)
	require.NoError(t, err)
	require.Equal(t, 0, got.Size())
}

func TestDecodeOwned_NoClone(t *testing.T) {
	// DecodeOwned must NOT clone: it decodes directly over the caller's buffer.
	src := NewASCIIItem("ABCDE").ToBytes()
	owned := append([]byte(nil), src...)
	allocs := testing.AllocsPerRun(50, func() {
		_, _ = DecodeOwned(owned)
	})
	plain := testing.AllocsPerRun(50, func() {
		_, _ = Decode(src)
	})
	require.Less(t, allocs, plain, "DecodeOwned must allocate fewer than Decode (no bytes.Clone)")
}

func TestDecodeOwned_AliasesInput(t *testing.T) {
	// Documents the ownership-transfer contract: mutating data after DecodeOwned
	// mutates the returned item, because a BinaryItem's values field is a []byte
	// slice directly over the input.
	owned := NewBinaryItem([]byte{0x01, 0x02, 0x03}).ToBytes()
	got, err := DecodeOwned(owned)
	require.NoError(t, err)
	bin, ok := got.(*BinaryItem)
	require.True(t, ok)
	require.Equal(t, []byte{0x01, 0x02, 0x03}, bin.values)

	owned[len(owned)-1] = 0xFF // mutate the last payload byte in place

	require.Equal(t, []byte{0x01, 0x02, 0xFF}, bin.values, "DecodeOwned must alias data, not copy it")
}

// TestDecodeOwned_AliasesInputText documents that DecodeOwned's ownership-transfer contract
// extends to ASCII/JIS8/LocalizedStr items too: they're built via ownedString's unsafe.String
// view over data, not a copy, so a contract-violating mutation after the call is visible
// through the returned string. This is the sharpest edge of the contract — mutating memory a
// live Go string points to is normally impossible — so it gets its own explicit test.
func TestDecodeOwned_AliasesInputText(t *testing.T) {
	owned := NewASCIIItem("hello").ToBytes()
	got, err := DecodeOwned(owned)
	require.NoError(t, err)
	ascii, ok := got.(*ASCIIItem)
	require.True(t, ok)
	require.Equal(t, "hello", ascii.value)

	owned[len(owned)-1] = 'H' // mutate the last payload byte in place

	require.Equal(t, "hellH", ascii.value, "DecodeOwned must alias data for text items too")
}

// bufferAddrRange returns the inclusive-exclusive address range [start, end) covered by buf's
// backing array, for asserting that a decoded value's own backing memory falls inside a
// specific buffer rather than being an independent copy.
func bufferAddrRange(buf []byte) (start, end uintptr) {
	if len(buf) == 0 {
		return 0, 0
	}

	start = uintptr(unsafe.Pointer(unsafe.SliceData(buf)))
	end = start + uintptr(len(buf))

	return start, end
}

// requireStringAliasesRange asserts that s's backing bytes fall within [start, end) — i.e. s is
// a view over that memory, not a copy. Empty strings carry no backing pointer worth checking.
func requireStringAliasesRange(t *testing.T, s string, start, end uintptr, msg string) {
	t.Helper()

	if len(s) == 0 {
		return
	}

	ptr := uintptr(unsafe.Pointer(unsafe.StringData(s)))
	require.GreaterOrEqual(t, ptr, start, msg)
	require.LessOrEqual(t, ptr+uintptr(len(s)), end, msg)
}

// requireBytesAliasRange asserts that b's backing array falls within [start, end) — i.e. b is a
// sub-slice of that memory, not a copy. Empty slices carry no backing pointer worth checking.
func requireBytesAliasRange(t *testing.T, b []byte, start, end uintptr, msg string) {
	t.Helper()

	if len(b) == 0 {
		return
	}

	ptr := uintptr(unsafe.Pointer(unsafe.SliceData(b)))
	require.GreaterOrEqual(t, ptr, start, msg)
	require.LessOrEqual(t, ptr+uintptr(len(b)), end, msg)
}

// TestDecodeOwned_SlabTypesAliasTransferredBuffer proves, via an explicit address-range check
// rather than the mutate-and-compare technique used above, that DecodeOwned's ASCII, JIS8,
// LocalizedStr, and Binary results alias the transferred buffer: their value/values fields'
// backing memory falls entirely inside the owned buffer's address range, for both a single-leaf
// shape and a 64-leaf same-type list (the shape that reaches the decode-side item slab).
func TestDecodeOwned_SlabTypesAliasTransferredBuffer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		build func() Item
		check func(t *testing.T, got Item, start, end uintptr)
	}{
		{
			name:  "ASCII",
			build: func() Item { return NewASCIIItem("hello world") },
			check: func(t *testing.T, got Item, start, end uintptr) {
				t.Helper()
				v, ok := got.(*ASCIIItem)
				require.True(t, ok, "expected *ASCIIItem, got %T", got)
				requireStringAliasesRange(t, v.value, start, end, "ASCII value must alias the transferred buffer")
			},
		},
		{
			name:  "JIS8",
			build: func() Item { return NewJIS8Item("hello") },
			check: func(t *testing.T, got Item, start, end uintptr) {
				t.Helper()
				v, ok := got.(*JIS8Item)
				require.True(t, ok, "expected *JIS8Item, got %T", got)
				requireStringAliasesRange(t, v.value, start, end, "JIS8 value must alias the transferred buffer")
			},
		},
		{
			name:  "LocalizedStr",
			build: func() Item { return NewUTF8StrItem("hello") },
			check: func(t *testing.T, got Item, start, end uintptr) {
				t.Helper()
				v, ok := got.(*LocalizedStrItem)
				require.True(t, ok, "expected *LocalizedStrItem, got %T", got)
				requireStringAliasesRange(t, v.value, start, end, "LocalizedStr value must alias the transferred buffer")
			},
		},
		{
			name:  "Binary",
			build: func() Item { return NewBinaryItem([]byte{0x01, 0x02, 0x03, 0x04, 0x05}) },
			check: func(t *testing.T, got Item, start, end uintptr) {
				t.Helper()
				v, ok := got.(*BinaryItem)
				require.True(t, ok, "expected *BinaryItem, got %T", got)
				requireBytesAliasRange(t, v.values, start, end, "Binary values must alias the transferred buffer")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name+"/SingleLeaf", func(t *testing.T) {
			t.Parallel()

			owned := tt.build().ToBytes()
			start, end := bufferAddrRange(owned)

			got, err := DecodeOwned(owned)
			require.NoError(t, err)
			require.NoError(t, got.Error())
			tt.check(t, got, start, end)
		})

		t.Run(tt.name+"/64LeafSlabbed", func(t *testing.T) {
			t.Parallel()

			children := make([]Item, 64)
			for i := range children {
				children[i] = tt.build()
			}

			owned := NewListItem(children...).ToBytes()
			start, end := bufferAddrRange(owned)

			got, err := DecodeOwned(owned)
			require.NoError(t, err)
			require.NoError(t, got.Error())

			gotChildren, err := got.ToList()
			require.NoError(t, err)
			require.Len(t, gotChildren, 64)

			for _, child := range gotChildren {
				tt.check(t, child, start, end)
			}
		})
	}
}
