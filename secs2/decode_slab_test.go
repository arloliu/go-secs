package secs2

import (
	"fmt"
	"testing"
	"unsafe"

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

// TestDecode_SlabTypesChunkBoundaries decodes lists of ASCII, JIS8, LocalizedStr, Binary, and
// scalar Boolean leaves at every slab chunk-boundary count, under both Decode and DecodeOwned,
// and verifies every child is retained with its distinct value intact after the full decode,
// carries no error, occupies a pointer distinct from every sibling, and the whole tree
// round-trips byte-for-byte through ToBytes.
func TestDecode_SlabTypesChunkBoundaries(t *testing.T) {
	t.Parallel()

	type spec struct {
		name  string
		build func(i int) Item
		check func(t *testing.T, i int, child Item)
		ptr   func(child Item) uintptr
	}

	specs := []spec{
		{
			name:  "ASCII",
			build: func(i int) Item { return A(fmt.Sprintf("a%d", i)) },
			check: func(t *testing.T, i int, child Item) {
				t.Helper()
				s, err := child.ToASCII()
				require.NoError(t, err)
				require.Equal(t, fmt.Sprintf("a%d", i), s)
			},
			ptr: func(child Item) uintptr {
				v, _ := child.(*ASCIIItem)

				return uintptr(unsafe.Pointer(v))
			},
		},
		{
			name:  "JIS8",
			build: func(i int) Item { return J(fmt.Sprintf("j%d", i)) },
			check: func(t *testing.T, i int, child Item) {
				t.Helper()
				s, err := child.ToJIS8()
				require.NoError(t, err)
				require.Equal(t, fmt.Sprintf("j%d", i), s)
			},
			ptr: func(child Item) uintptr {
				v, _ := child.(*JIS8Item)

				return uintptr(unsafe.Pointer(v))
			},
		},
		{
			name: "LocalizedStr",
			build: func(i int) Item {
				return NewLocalizedStrItem(uint16(i), fmt.Sprintf("l%d", i)) //nolint:gosec // i is bounded by scalarSlabBoundaryCounts, well within uint16
			},
			check: func(t *testing.T, i int, child Item) {
				t.Helper()
				s, err := child.ToLocalizedStr()
				require.NoError(t, err)
				require.Equal(t, fmt.Sprintf("l%d", i), s)

				lsh, err := child.ToLocalizedStrHeader()
				require.NoError(t, err)
				require.Equal(t, uint16(i), lsh, "LSH value must survive the decode") //nolint:gosec
			},
			ptr: func(child Item) uintptr {
				v, _ := child.(*LocalizedStrItem)

				return uintptr(unsafe.Pointer(v))
			},
		},
		{
			name:  "Binary",
			build: func(i int) Item { return B(byte(i)) }, //nolint:gosec // i is bounded well within byte range
			check: func(t *testing.T, i int, child Item) {
				t.Helper()
				b, err := child.ToBinary()
				require.NoError(t, err)
				require.Equal(t, []byte{byte(i)}, b) //nolint:gosec
			},
			ptr: func(child Item) uintptr {
				v, _ := child.(*BinaryItem)

				return uintptr(unsafe.Pointer(v))
			},
		},
		{
			name:  "Boolean",
			build: func(i int) Item { return BOOLEAN(i%2 == 0) },
			check: func(t *testing.T, i int, child Item) {
				t.Helper()
				v, err := child.BoolAt(0)
				require.NoError(t, err)
				require.Equal(t, i%2 == 0, v)
			},
			ptr: func(child Item) uintptr {
				v, _ := child.(*BooleanItem)

				return uintptr(unsafe.Pointer(v))
			},
		},
	}

	for _, sp := range specs {
		for _, n := range scalarSlabBoundaryCounts {
			t.Run(fmt.Sprintf("%s/n=%d/Decode", sp.name, n), func(t *testing.T) {
				t.Parallel()
				testSlabTypeBoundary(t, n, Decode, sp.build, sp.check, sp.ptr)
			})
			t.Run(fmt.Sprintf("%s/n=%d/DecodeOwned", sp.name, n), func(t *testing.T) {
				t.Parallel()
				testSlabTypeBoundary(t, n, DecodeOwned, sp.build, sp.check, sp.ptr)
			})
		}
	}
}

// testSlabTypeBoundary builds an outer list of n same-type leaves via build, decodes it with
// decodeFn, and asserts every child survives with a distinct, correct value, no error, a
// pointer distinct from every sibling (via ptr), and a byte-for-byte ToBytes round trip against
// the original wire encoding.
func testSlabTypeBoundary(
	t *testing.T,
	n int,
	decodeFn func([]byte) (Item, error),
	build func(i int) Item,
	check func(t *testing.T, i int, child Item),
	ptr func(child Item) uintptr,
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

	seen := make(map[uintptr]bool, n)

	for i, child := range gotChildren {
		require.NoError(t, child.Error())
		check(t, i, child)

		p := ptr(child)
		require.False(t, seen[p], "item %d shares its struct pointer with a previous sibling", i)
		seen[p] = true
	}

	require.Equal(t, wire, got.ToBytes())
}

// TestDecode_SlabTypesEmptyMinimumLength decodes zero-payload ASCII, JIS8, and Binary items,
// plus the minimum two-byte LocalizedStr (empty string, LSH-only payload), under both Decode
// and DecodeOwned. All four still take the slab-carving branch in decodeItem (ASCII, JIS8,
// Binary, and LocalizedStr carve unconditionally, regardless of payload length) — this guards
// against a boundary bug at the zero-length edge. Each case also decodes a two-leaf list of the
// same empty shape to confirm the two children get distinct struct pointers.
func TestDecode_SlabTypesEmptyMinimumLength(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		build     func() Item
		checkType func(t *testing.T, got Item) uintptr
	}{
		{
			name:  "ASCII",
			build: func() Item { return A("") },
			checkType: func(t *testing.T, got Item) uintptr {
				t.Helper()
				v, ok := got.(*ASCIIItem)
				require.True(t, ok, "expected *ASCIIItem, got %T", got)
				require.Equal(t, "", v.value)

				return uintptr(unsafe.Pointer(v))
			},
		},
		{
			name:  "JIS8",
			build: func() Item { return J("") },
			checkType: func(t *testing.T, got Item) uintptr {
				t.Helper()
				v, ok := got.(*JIS8Item)
				require.True(t, ok, "expected *JIS8Item, got %T", got)
				require.Equal(t, "", v.value)

				return uintptr(unsafe.Pointer(v))
			},
		},
		{
			name:  "Binary",
			build: func() Item { return NewBinaryItem() },
			checkType: func(t *testing.T, got Item) uintptr {
				t.Helper()
				v, ok := got.(*BinaryItem)
				require.True(t, ok, "expected *BinaryItem, got %T", got)
				require.Empty(t, v.values)

				return uintptr(unsafe.Pointer(v))
			},
		},
		{
			name:  "LocalizedStrMinimum",
			build: func() Item { return NewLocalizedStrItem(LSHASCII, "") },
			checkType: func(t *testing.T, got Item) uintptr {
				t.Helper()
				v, ok := got.(*LocalizedStrItem)
				require.True(t, ok, "expected *LocalizedStrItem, got %T", got)
				require.Equal(t, "", v.value)
				require.Equal(t, LSHASCII, v.lsh)

				return uintptr(unsafe.Pointer(v))
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
			tt.checkType(t, got)
			require.Equal(t, wire, got.ToBytes())
		})
		t.Run(tt.name+"/DecodeOwned", func(t *testing.T) {
			t.Parallel()

			wire := tt.build().ToBytes()
			owned := append([]byte(nil), wire...)
			got, err := DecodeOwned(owned)
			require.NoError(t, err)
			require.NoError(t, got.Error())
			tt.checkType(t, got)
			require.Equal(t, wire, got.ToBytes())
		})
		t.Run(tt.name+"/DistinctPointers", func(t *testing.T) {
			t.Parallel()

			wire := NewListItem(tt.build(), tt.build()).ToBytes()
			got, err := Decode(wire)
			require.NoError(t, err)
			require.NoError(t, got.Error())

			children, err := got.ToList()
			require.NoError(t, err)
			require.Len(t, children, 2)

			p0 := tt.checkType(t, children[0])
			p1 := tt.checkType(t, children[1])
			require.NotEqual(t, p0, p1, "the two empty-shape siblings must not share a struct pointer")
		})
	}
}

// TestDecode_BooleanZeroLengthNotScalar guards the scalar-slab boundary condition for Boolean:
// a zero-length BOOLEAN item (count == 0) must take the existing multi-value decode branch, not
// the scalar-slab branch, so it must never enter the bools slab in decodeSlab. The multi-value
// branch is distinguishable from the scalar branch by its non-nil (possibly zero-length) values
// field; the scalar branch never populates values at all.
func TestDecode_BooleanZeroLengthNotScalar(t *testing.T) {
	t.Parallel()

	wire := BOOLEAN().ToBytes()

	t.Run("Decode", func(t *testing.T) {
		t.Parallel()

		got, err := Decode(wire)
		require.NoError(t, err)
		require.NoError(t, got.Error())

		b, ok := got.(*BooleanItem)
		require.True(t, ok, "expected *BooleanItem, got %T", got)
		require.Equal(t, int32(0), b.size)
		require.NotNil(t, b.values, "zero-length BOOLEAN must take the non-scalar branch (non-nil values)")
		require.Equal(t, 0, got.Size())
		require.Equal(t, wire, got.ToBytes())
	})

	t.Run("DecodeOwned", func(t *testing.T) {
		t.Parallel()

		owned := append([]byte(nil), wire...)
		got, err := DecodeOwned(owned)
		require.NoError(t, err)
		require.NoError(t, got.Error())

		b, ok := got.(*BooleanItem)
		require.True(t, ok, "expected *BooleanItem, got %T", got)
		require.Equal(t, int32(0), b.size)
		require.NotNil(t, b.values, "zero-length BOOLEAN must take the non-scalar branch (non-nil values)")
		require.Equal(t, 0, got.Size())
		require.Equal(t, wire, got.ToBytes())
	})
}
