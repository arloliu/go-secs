package secs2

import (
	"testing"

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
