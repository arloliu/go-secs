package secs2

import "testing"

// Fixed item shapes used across all secs2 benchmarks.
// Small and deterministic so -benchtime=1x is a valid smoke run.
//
//   - u4Item:   U4[4] — one UintItem, 4 values (encoded: 2+4*4 = 18 bytes)
//   - ascItem:  A[32] — 32-char ASCII string (encoded: 2+32 = 34 bytes)
//   - nestItem: L{A[8], U4[4], L{A[4], U4[2]}} — shallow nested list
var (
	wireU4   []byte // U4[4] wire encoding
	wireASC  []byte // A[32] wire encoding
	wireNest []byte // nested-list wire encoding

	benchItem Item // = nestItem (used for ToBytes / AppendTo benchmarks)
)

func init() {
	// U4[4]: four unsigned 32-bit values.
	wireU4 = U4(uint(1), uint(2), uint(3), uint(4)).ToBytes()

	// A[32]: exactly 32-character ASCII string.
	wireASC = A("abcdefghijklmnopqrstuvwxyzabcdef").ToBytes()

	// Nested list: L{A[8], U4[4], L{A[4], U4[2]}}.
	inner := L(A("abcd"), U4(uint(10), uint(20)))
	outer := L(A("abcdefgh"), U4(uint(1), uint(2), uint(3), uint(4)), inner)
	wireNest = outer.ToBytes()
	benchItem = outer
}

// BenchmarkDecode measures secs2.Decode for three representative item shapes.
// Each call allocates the returned Item tree plus one owned bytes.Clone of the
// input (the decoder's backing buffer) — all inherent escaping allocs; nothing
// is poolable here.
func BenchmarkDecode(b *testing.B) {
	b.Run("U4x4", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_, _ = Decode(wireU4)
		}
	})
	b.Run("ASCII32", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_, _ = Decode(wireASC)
		}
	})
	b.Run("NestedL", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_, _ = Decode(wireNest)
		}
	})
}

// BenchmarkToBytes measures Item.ToBytes, which allocates exactly one result
// buffer per call (escapes to the caller — not poolable).
func BenchmarkToBytes(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		_ = benchItem.ToBytes()
	}
}

// BenchmarkAppendTo_ReusedBuffer measures Item.AppendTo with a pre-sized,
// caller-owned buffer reused across iterations. This is the v2 answer to
// "encode buffer reuse": the caller manages the buffer; the library adds no
// internal pool. Expected result: ~0 allocs/op.
func BenchmarkAppendTo_ReusedBuffer(b *testing.B) {
	buf := make([]byte, 0, benchItem.EncodedLen())
	b.ReportAllocs()
	for b.Loop() {
		buf = benchItem.AppendTo(buf[:0])
	}
	_ = buf
}

// flat100kChildren builds 100K valid ASCII leaf children, shared by the ListItem.Error and
// NewListItem construction benchmarks below.
func flat100kChildren() []Item {
	const n = 100_000

	children := make([]Item, n)
	for i := range children {
		children[i] = NewASCIIItem("x")
	}

	return children
}

// BenchmarkListItemError_Flat100k measures ListItem.Error() on a pre-built, known-clean 100K-leaf
// list. This is the O(1) fast path this task adds: expect a handful of nanoseconds and 0
// allocs/op, regardless of child count, since a clean list never re-walks its children.
func BenchmarkListItemError_Flat100k(b *testing.B) {
	list := NewListItem(flat100kChildren()...)

	b.ReportAllocs()
	for b.Loop() {
		_ = list.Error()
	}
}

// BenchmarkNewListItem_Flat100k measures constructing the same 100K-leaf list, quantifying the
// per-child type-switch cost that now runs once at construction instead of on every Error() call.
func BenchmarkNewListItem_Flat100k(b *testing.B) {
	children := flat100kChildren()

	b.ReportAllocs()
	for b.Loop() {
		_ = NewListItem(children...)
	}
}

// One-scalar-leaf wire encodings, used by BenchmarkDecodeScalar* below. Each is the shape that
// hits the count==1 branch of decodeIntItem/decodeUintItem/decodeFloatItem, i.e. the shape the
// decode-side slab carves structs for.
var (
	wireScalarI8 = I8(int64(-42)).ToBytes()
	wireScalarU8 = U8(uint64(42)).ToBytes()
	wireScalarF8 = F8(3.5).ToBytes()
)

// BenchmarkDecodeScalarInt measures Decode on a single scalar I8 item — the shape that
// allocates from the IntItem slab chunk instead of one heap allocation per item.
func BenchmarkDecodeScalarInt(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		_, _ = Decode(wireScalarI8)
	}
}

// BenchmarkDecodeScalarUint measures Decode on a single scalar U8 item — the shape that
// allocates from the UintItem slab chunk instead of one heap allocation per item.
func BenchmarkDecodeScalarUint(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		_, _ = Decode(wireScalarU8)
	}
}

// BenchmarkDecodeScalarFloat measures Decode on a single scalar F8 item — the shape that
// allocates from the FloatItem slab chunk instead of one heap allocation per item.
func BenchmarkDecodeScalarFloat(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		_, _ = Decode(wireScalarF8)
	}
}
