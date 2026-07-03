package secs2

import (
	"testing"

	"github.com/arloliu/go-secs/v2/internal/framecodec"
	"github.com/stretchr/testify/require"
)

func TestDecodeOwned_MatchesDecode(t *testing.T) {
	src := NewListItem(NewUintItem(1, 1, 2, 3), NewASCIIItem("hi")).ToBytes()
	want, err := Decode(src)
	require.NoError(t, err)

	owned := append([]byte(nil), src...) // a buffer the library "owns"
	got, err := DecodeOwned(framecodec.AdoptSECS2Body(owned))
	require.NoError(t, err)
	require.Equal(t, want.ToBytes(), got.ToBytes())
}

func TestDecodeOwned_Empty(t *testing.T) {
	got, err := DecodeOwned(framecodec.AdoptSECS2Body(nil))
	require.NoError(t, err)
	require.Equal(t, 0, got.Size())
}

func TestDecodeOwned_NoClone(t *testing.T) {
	// DecodeOwned must NOT clone: it decodes directly over the token's bytes.
	src := NewASCIIItem("ABCDE").ToBytes()
	owned := append([]byte(nil), src...)
	allocs := testing.AllocsPerRun(50, func() {
		_, _ = DecodeOwned(framecodec.AdoptSECS2Body(owned))
	})
	plain := testing.AllocsPerRun(50, func() {
		_, _ = Decode(src)
	})
	require.Less(t, allocs, plain, "DecodeOwned must allocate fewer than Decode (no bytes.Clone)")
}
