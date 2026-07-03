package secs2

import (
	"bytes"
	"math"
	"testing"
)

// roundtripCase describes one cross-type round-trip test entry.
type roundtripCase struct {
	name      string
	item      Item
	checkVals func(t *testing.T, decoded Item)
}

// makeRoundtripCases returns one test case for every concrete Item type,
// plus a deep nested list that mixes several types.
//
//nolint:gocyclo,cyclop // test-table builder: complexity is purely structural (one validator per type)
func makeRoundtripCases() []roundtripCase {
	return []roundtripCase{
		{
			name: "EmptyItem",
			item: NewEmptyItem(),
			checkVals: func(t *testing.T, decoded Item) {
				t.Helper()

				if !decoded.IsEmpty() {
					t.Errorf("want IsEmpty=true, got false")
				}
			},
		},
		{
			name: "ASCIIItem",
			item: A("hello"),
			checkVals: func(t *testing.T, decoded Item) {
				t.Helper()

				s, err := decoded.ToASCII()
				if err != nil || s != "hello" {
					t.Errorf("ToASCII: got %q, %v", s, err)
				}
			},
		},
		{
			name: "JIS8Item",
			item: J("world"),
			checkVals: func(t *testing.T, decoded Item) {
				t.Helper()

				s, err := decoded.ToJIS8()
				if err != nil || s != "world" {
					t.Errorf("ToJIS8: got %q, %v", s, err)
				}
			},
		},
		{
			name: "LocalizedStrItem",
			item: W("unicode"),
			checkVals: func(t *testing.T, decoded Item) {
				t.Helper()

				s, err := decoded.ToLocalizedStr()
				if err != nil || s != "unicode" {
					t.Errorf("ToLocalizedStr: got %q, %v", s, err)
				}

				hdr, err := decoded.ToLocalizedStrHeader()
				if err != nil || hdr != LSHUTF8 {
					t.Errorf("ToLocalizedStrHeader: got %d, %v", hdr, err)
				}
			},
		},
		{
			name: "BinaryItem",
			item: B(0x01, 0xAB, 0xFF),
			checkVals: func(t *testing.T, decoded Item) {
				t.Helper()

				got, err := decoded.ToBinary()
				if err != nil {
					t.Fatalf("ToBinary: %v", err)
				}

				want := []byte{0x01, 0xAB, 0xFF}
				if !bytes.Equal(got, want) {
					t.Errorf("ToBinary: got %v, want %v", got, want)
				}
			},
		},
		{
			name: "BooleanItem",
			item: BOOLEAN(true, false, true),
			checkVals: func(t *testing.T, decoded Item) {
				t.Helper()

				got, err := decoded.ToBoolean()
				if err != nil {
					t.Fatalf("ToBoolean: %v", err)
				}

				want := []bool{true, false, true}
				if len(got) != len(want) {
					t.Fatalf("ToBoolean len: got %d, want %d", len(got), len(want))
				}

				for i := range want {
					if got[i] != want[i] {
						t.Errorf("ToBoolean[%d]: got %v, want %v", i, got[i], want[i])
					}
				}
			},
		},
		{
			name: "IntItem_I1",
			item: I1(-128, 0, 127),
			checkVals: func(t *testing.T, decoded Item) {
				t.Helper()

				got, err := decoded.ToInt()
				if err != nil {
					t.Fatalf("ToInt: %v", err)
				}

				want := []int64{-128, 0, 127}
				if len(got) != len(want) {
					t.Fatalf("ToInt len: got %d, want %d", len(got), len(want))
				}

				for i := range want {
					if got[i] != want[i] {
						t.Errorf("ToInt[%d]: got %d, want %d", i, got[i], want[i])
					}
				}
			},
		},
		{
			name: "IntItem_I2",
			item: I2(-32768, 0, 32767),
			checkVals: func(t *testing.T, decoded Item) {
				t.Helper()

				got, err := decoded.ToInt()
				if err != nil {
					t.Fatalf("ToInt: %v", err)
				}

				want := []int64{-32768, 0, 32767}
				if len(got) != len(want) {
					t.Fatalf("ToInt len: got %d, want %d", len(got), len(want))
				}

				for i := range want {
					if got[i] != want[i] {
						t.Errorf("ToInt[%d]: got %d, want %d", i, got[i], want[i])
					}
				}
			},
		},
		{
			name: "IntItem_I4",
			item: I4(-1, 0, 1, 42),
			checkVals: func(t *testing.T, decoded Item) {
				t.Helper()

				got, err := decoded.ToInt()
				if err != nil {
					t.Fatalf("ToInt: %v", err)
				}

				want := []int64{-1, 0, 1, 42}
				if len(got) != len(want) {
					t.Fatalf("ToInt len: got %d, want %d", len(got), len(want))
				}

				for i := range want {
					if got[i] != want[i] {
						t.Errorf("ToInt[%d]: got %d, want %d", i, got[i], want[i])
					}
				}
			},
		},
		{
			name: "IntItem_I8",
			item: I8(int64(math.MinInt64), -1, 0, int64(math.MaxInt64)),
			checkVals: func(t *testing.T, decoded Item) {
				t.Helper()

				got, err := decoded.ToInt()
				if err != nil {
					t.Fatalf("ToInt: %v", err)
				}

				want := []int64{math.MinInt64, -1, 0, math.MaxInt64}
				if len(got) != len(want) {
					t.Fatalf("ToInt len: got %d, want %d", len(got), len(want))
				}

				for i := range want {
					if got[i] != want[i] {
						t.Errorf("ToInt[%d]: got %d, want %d", i, got[i], want[i])
					}
				}
			},
		},
		{
			name: "UintItem_U1",
			item: U1(uint(0), uint(128), uint(255)),
			checkVals: func(t *testing.T, decoded Item) {
				t.Helper()

				got, err := decoded.ToUint()
				if err != nil {
					t.Fatalf("ToUint: %v", err)
				}

				want := []uint64{0, 128, 255}
				if len(got) != len(want) {
					t.Fatalf("ToUint len: got %d, want %d", len(got), len(want))
				}

				for i := range want {
					if got[i] != want[i] {
						t.Errorf("ToUint[%d]: got %d, want %d", i, got[i], want[i])
					}
				}
			},
		},
		{
			name: "UintItem_U2",
			item: U2(uint(0), uint(1000), uint(65535)),
			checkVals: func(t *testing.T, decoded Item) {
				t.Helper()

				got, err := decoded.ToUint()
				if err != nil {
					t.Fatalf("ToUint: %v", err)
				}

				want := []uint64{0, 1000, 65535}
				if len(got) != len(want) {
					t.Fatalf("ToUint len: got %d, want %d", len(got), len(want))
				}

				for i := range want {
					if got[i] != want[i] {
						t.Errorf("ToUint[%d]: got %d, want %d", i, got[i], want[i])
					}
				}
			},
		},
		{
			name: "UintItem_U4",
			item: U4(uint64(0), uint64(1), uint64(4294967295)),
			checkVals: func(t *testing.T, decoded Item) {
				t.Helper()

				got, err := decoded.ToUint()
				if err != nil {
					t.Fatalf("ToUint: %v", err)
				}

				want := []uint64{0, 1, 4294967295}
				if len(got) != len(want) {
					t.Fatalf("ToUint len: got %d, want %d", len(got), len(want))
				}

				for i := range want {
					if got[i] != want[i] {
						t.Errorf("ToUint[%d]: got %d, want %d", i, got[i], want[i])
					}
				}
			},
		},
		{
			name: "UintItem_U8",
			item: U8(uint64(0), uint64(math.MaxUint64)),
			checkVals: func(t *testing.T, decoded Item) {
				t.Helper()

				got, err := decoded.ToUint()
				if err != nil {
					t.Fatalf("ToUint: %v", err)
				}

				want := []uint64{0, math.MaxUint64}
				if len(got) != len(want) {
					t.Fatalf("ToUint len: got %d, want %d", len(got), len(want))
				}

				for i := range want {
					if got[i] != want[i] {
						t.Errorf("ToUint[%d]: got %d, want %d", i, got[i], want[i])
					}
				}
			},
		},
		{
			// F4 values are stored as float64(float32(v)); round-trip compares via that
			// representation to account for float32 precision loss.
			name: "FloatItem_F4",
			item: F4(float32(1.5), float32(-3.14)),
			checkVals: func(t *testing.T, decoded Item) {
				t.Helper()

				got, err := decoded.ToFloat()
				if err != nil {
					t.Fatalf("ToFloat: %v", err)
				}

				want := []float64{float64(float32(1.5)), float64(float32(-3.14))}
				if len(got) != len(want) {
					t.Fatalf("ToFloat len: got %d, want %d", len(got), len(want))
				}

				for i := range want {
					if got[i] != want[i] {
						t.Errorf("ToFloat[%d]: got %v, want %v", i, got[i], want[i])
					}
				}
			},
		},
		{
			name: "FloatItem_F8",
			item: F8(1.5, math.Pi),
			checkVals: func(t *testing.T, decoded Item) {
				t.Helper()

				got, err := decoded.ToFloat()
				if err != nil {
					t.Fatalf("ToFloat: %v", err)
				}

				want := []float64{1.5, math.Pi}
				if len(got) != len(want) {
					t.Fatalf("ToFloat len: got %d, want %d", len(got), len(want))
				}

				for i := range want {
					if got[i] != want[i] {
						t.Errorf("ToFloat[%d]: got %v, want %v", i, got[i], want[i])
					}
				}
			},
		},
		{
			name: "ListItem_flat",
			item: L(A("a"), I4(1, 2), BOOLEAN(true)),
			checkVals: func(t *testing.T, decoded Item) {
				t.Helper()

				if !decoded.IsList() {
					t.Fatalf("want IsList=true")
				}

				if decoded.Size() != 3 {
					t.Fatalf("Size: got %d, want 3", decoded.Size())
				}

				child0, err := decoded.ItemAt(0)
				if err != nil {
					t.Fatalf("ItemAt(0): %v", err)
				}

				s, err := child0.ToASCII()
				if err != nil || s != "a" {
					t.Errorf("child[0].ToASCII: got %q, %v", s, err)
				}
			},
		},
		{
			// Deep nested list mixing ASCII, I4, binary, boolean, F8, and U4.
			name: "ListItem_deep",
			item: L(
				L(A("level2"), I4(99)),
				L(
					L(B(0xDE, 0xAD), BOOLEAN(false)),
					F8(2.718281828),
				),
				U4(uint64(42)),
			),
			checkVals: func(t *testing.T, decoded Item) {
				t.Helper()

				if !decoded.IsList() {
					t.Fatalf("want IsList=true")
				}

				if decoded.Size() != 3 {
					t.Fatalf("Size: got %d, want 3", decoded.Size())
				}

				// decoded[0][0] must be ASCII "level2"
				leaf, err := decoded.Get(0, 0)
				if err != nil {
					t.Fatalf("Get(0,0): %v", err)
				}

				s, err := leaf.ToASCII()
				if err != nil || s != "level2" {
					t.Errorf("Get(0,0).ToASCII: got %q, %v", s, err)
				}

				// decoded[2] must be U4 with value 42
				u4item, err := decoded.Get(2)
				if err != nil {
					t.Fatalf("Get(2): %v", err)
				}

				vals, err := u4item.ToUint()
				if err != nil || len(vals) != 1 || vals[0] != 42 {
					t.Errorf("Get(2).ToUint: got %v, %v", vals, err)
				}
			},
		},
	}
}

// TestRoundtrip_AllTypes encodes every concrete Item type to bytes, decodes it back, and
// verifies that the decoded bytes are identical to the original and that typed accessor values
// match. It also checks that EncodedLen() == len(ToBytes()) for every entry.
func TestRoundtrip_AllTypes(t *testing.T) {
	for _, tc := range makeRoundtripCases() {
		t.Run(tc.name, func(t *testing.T) {
			orig := tc.item

			if err := orig.Error(); err != nil {
				t.Fatalf("item has construction error: %v", err)
			}

			origBytes := orig.ToBytes()

			if orig.EncodedLen() != len(origBytes) {
				t.Errorf("EncodedLen()=%d != len(ToBytes())=%d", orig.EncodedLen(), len(origBytes))
			}

			decoded, err := Decode(origBytes)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}

			if err := decoded.Error(); err != nil {
				t.Fatalf("decoded item has error: %v", err)
			}

			decodedBytes := decoded.ToBytes()
			if !bytes.Equal(origBytes, decodedBytes) {
				t.Errorf("bytes not equal after round-trip\norig:    %x\ndecoded: %x", origBytes, decodedBytes)
			}

			tc.checkVals(t, decoded)
		})
	}
}

// TestAllocsPerRun verifies the allocation contracts from spec §11.
// Tests run serially; no t.Parallel().
//
// Note on Ints() iterator: calling Ints() through the Item interface allocates 3 times
// (1 for the closure + 2 for the range-over-func yield infrastructure). Using the concrete
// *IntItem, the compiler inlines the method and elides all allocations, achieving 0 allocs
// as spec §11 requires. The spec says "over an IntItem" (the concrete type), so the test
// below correctly uses *IntItem for that check.
func TestAllocsPerRun(t *testing.T) {
	item := I4(1, 2, 3, 4)

	// 1. AppendTo into a reused, pre-sized buffer → 0 allocs.
	buf := make([]byte, 0, item.EncodedLen())

	got := testing.AllocsPerRun(100, func() {
		buf = buf[:0]
		buf = item.AppendTo(buf)
	})
	if got != 0 {
		t.Errorf("AppendTo allocs: want 0, got %v", got)
	}

	// 2. for range x.Ints() over a concrete *IntItem → 0 allocs.
	// The compiler inlines Ints() on the concrete type and stack-allocates the closure.
	// Through the Item interface, it would be 3 allocs (closure + range-func overhead).
	concreteItem, ok := I4(1, 2, 3, 4).(*IntItem)
	if !ok {
		t.Fatalf("I4 did not return *IntItem")
	}

	var sink int64

	got = testing.AllocsPerRun(100, func() {
		for v := range concreteItem.Ints() {
			sink = v
		}
	})
	_ = sink

	if got != 0 {
		t.Errorf("Ints() range allocs: want 0, got %v", got)
	}

	// 3. ToBytes() → exactly 1 alloc.
	got = testing.AllocsPerRun(100, func() {
		_ = item.ToBytes()
	})
	if got != 1 {
		t.Errorf("ToBytes allocs: want 1, got %v", got)
	}

	// 4. ToInt() → exactly 1 alloc.
	got = testing.AllocsPerRun(100, func() {
		_, _ = item.ToInt()
	})
	if got != 1 {
		t.Errorf("ToInt allocs: want 1, got %v", got)
	}
}
