package secs2

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// scalarSlabBoundaryCounts are the leaf counts exercised by TestDecode_ScalarSlabChunkBoundaries:
// 0 (empty list — exercises list decode only, no scalar leaf), then five pairs that each
// straddle a cumulative slab chunk capacity (1, 5, 21, 85, 213). The first of each pair fills
// the current chunk exactly; the second forces a fresh chunk allocation.
var scalarSlabBoundaryCounts = []int{0, 1, 2, 5, 6, 21, 22, 85, 86, 213, 214}

// TestDecode_ScalarSlabChunkBoundaries decodes lists of scalar numeric leaves (Int, Uint,
// Float) at every slab chunk-boundary count, under both Decode and DecodeOwned, and verifies
// every child is retained with its distinct value intact after the full decode, carries no
// error, and the whole tree round-trips byte-for-byte through ToBytes.
func TestDecode_ScalarSlabChunkBoundaries(t *testing.T) {
	t.Parallel()

	type spec struct {
		name  string
		build func(i int) Item
		check func(t *testing.T, i int, child Item)
	}

	specs := []spec{
		{
			name:  "Int",
			build: func(i int) Item { return I8(int64(i)) },
			check: func(t *testing.T, i int, child Item) {
				t.Helper()
				require.True(t, child.IsInt64())
				v, err := child.IntAt(0)
				require.NoError(t, err)
				require.Equal(t, int64(i), v)
			},
		},
		{
			name:  "Uint",
			build: func(i int) Item { return U8(uint64(i)) },
			check: func(t *testing.T, i int, child Item) {
				t.Helper()
				require.True(t, child.IsUint64())
				v, err := child.UintAt(0)
				require.NoError(t, err)
				require.Equal(t, uint64(i), v)
			},
		},
		{
			name:  "Float",
			build: func(i int) Item { return F8(float64(i) + 0.5) },
			check: func(t *testing.T, i int, child Item) {
				t.Helper()
				require.True(t, child.IsFloat64())
				v, err := child.FloatAt(0)
				require.NoError(t, err)
				require.InDelta(t, float64(i)+0.5, v, 1e-9)
			},
		},
	}

	for _, sp := range specs {
		for _, n := range scalarSlabBoundaryCounts {
			t.Run(fmt.Sprintf("%s/n=%d/Decode", sp.name, n), func(t *testing.T) {
				t.Parallel()
				testScalarSlabBoundary(t, n, Decode, sp.build, sp.check)
			})
			t.Run(fmt.Sprintf("%s/n=%d/DecodeOwned", sp.name, n), func(t *testing.T) {
				t.Parallel()
				testScalarSlabBoundary(t, n, DecodeOwned, sp.build, sp.check)
			})
		}
	}
}

// testScalarSlabBoundary builds an outer list of n scalar leaves via build, decodes it with
// decodeFn, and asserts every child survives with a distinct, correct value, no error, and a
// byte-for-byte ToBytes round trip against the original wire encoding.
func testScalarSlabBoundary(
	t *testing.T,
	n int,
	decodeFn func([]byte) (Item, error),
	build func(i int) Item,
	check func(t *testing.T, i int, child Item),
) {
	t.Helper()

	children := make([]Item, n)
	for i := range children {
		children[i] = build(i)
	}

	wire := NewListItem(children...).ToBytes()
	owned := append([]byte(nil), wire...) // decodeFn may take ownership (DecodeOwned); never hand it wire itself

	got, err := decodeFn(owned)
	require.NoError(t, err)
	require.NoError(t, got.Error())

	gotChildren, err := got.ToList()
	require.NoError(t, err)
	require.Len(t, gotChildren, n)

	for i, child := range gotChildren {
		require.NoError(t, child.Error())
		check(t, i, child)
	}

	require.Equal(t, wire, got.ToBytes())
}

// TestDecode_ScalarNumericZeroPayload guards against an accidental count<=1 slab condition: a
// zero-payload numeric item (count == 0) must take the existing multi-value decode branch, not
// the scalar slab branch, and must decode to the concrete numeric type — never EmptyItem, which
// is reserved for genuinely empty input (secs2/decode.go's len(data)==0 fast path).
func TestDecode_ScalarNumericZeroPayload(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		build func() Item
		check func(t *testing.T, got Item)
	}{
		{
			name:  "Int",
			build: func() Item { return I8() },
			check: func(t *testing.T, got Item) {
				t.Helper()
				_, ok := got.(*IntItem)
				require.True(t, ok, "expected *IntItem, got %T", got)
			},
		},
		{
			name:  "Uint",
			build: func() Item { return U8() },
			check: func(t *testing.T, got Item) {
				t.Helper()
				_, ok := got.(*UintItem)
				require.True(t, ok, "expected *UintItem, got %T", got)
			},
		},
		{
			name:  "Float",
			build: func() Item { return F8() },
			check: func(t *testing.T, got Item) {
				t.Helper()
				_, ok := got.(*FloatItem)
				require.True(t, ok, "expected *FloatItem, got %T", got)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name+"/Decode", func(t *testing.T) {
			t.Parallel()

			wire := tt.build().ToBytes()
			got, err := Decode(wire)
			require.NoError(t, err)
			require.NoError(t, got.Error())
			require.Equal(t, 0, got.Size())
			tt.check(t, got)
			require.Equal(t, wire, got.ToBytes())
		})
		t.Run(tt.name+"/DecodeOwned", func(t *testing.T) {
			t.Parallel()

			wire := tt.build().ToBytes()
			owned := append([]byte(nil), wire...)
			got, err := DecodeOwned(owned)
			require.NoError(t, err)
			require.NoError(t, got.Error())
			require.Equal(t, 0, got.Size())
			tt.check(t, got)
			require.Equal(t, wire, got.ToBytes())
		})
	}
}
