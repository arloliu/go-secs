// Package v2 benchmarks secs2.Item construction, encode, and decode against
// the current go-secs v2 working tree. See secs2item/v1 for the v1
// counterpart — same benchmark names, same shapes, so benchstat can diff the
// two result files directly.
//
// v2 items are immutable and unpooled (no Free()); decoding a raw item goes
// through secs2.Decode directly rather than through an hsms wrapper.
package v2

import (
	"testing"

	"github.com/arloliu/go-secs/v2/secs2"
)

// BenchmarkItemConstruct_IntList1000 builds a 1000-element I8 list.
func BenchmarkItemConstruct_IntList1000(b *testing.B) {
	values := intListValues()

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = secs2.I8(values...)
	}
}

// BenchmarkItemConstruct_NestedList builds the 100x13 nested-list shape.
func BenchmarkItemConstruct_NestedList(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = nestedListItem()
	}
}

// BenchmarkItemEncode_NestedList measures a cold ToBytes() call on a fresh
// item per iteration, excluding construction from the timed section. v2's
// ToBytes always recomputes (no cache), but a fresh item per iteration keeps
// the methodology identical to secs2item/v1 (whose ListItem.ToBytes DOES
// cache its result on the item — see that file).
func BenchmarkItemEncode_NestedList(b *testing.B) {
	b.StopTimer()
	b.ReportAllocs()
	for range b.N {
		item := nestedListItem()
		b.StartTimer()
		_ = item.ToBytes()
		b.StopTimer()
	}
}

// BenchmarkItemDecode_NestedList measures decoding the nested-list wire
// bytes back into an Item tree via secs2.Decode.
func BenchmarkItemDecode_NestedList(b *testing.B) {
	item := nestedListItem()
	wire := item.ToBytes()

	b.ReportAllocs()
	b.SetBytes(int64(len(wire)))
	b.ResetTimer()
	for range b.N {
		if _, err := secs2.Decode(wire); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkItemEncode_WaferMap measures a cold ToBytes() call on a fresh
// 100k-die wafer map per iteration (see BenchmarkItemEncode_NestedList).
func BenchmarkItemEncode_WaferMap(b *testing.B) {
	b.StopTimer()
	b.ReportAllocs()
	for range b.N {
		item := waferMapItem()
		b.StartTimer()
		_ = item.ToBytes()
		b.StopTimer()
	}
}

// BenchmarkItemDecode_WaferMap measures decoding a 100k-die wafer map.
func BenchmarkItemDecode_WaferMap(b *testing.B) {
	item := waferMapItem()
	wire := item.ToBytes()

	b.ReportAllocs()
	b.SetBytes(int64(len(wire)))
	b.ResetTimer()
	for range b.N {
		if _, err := secs2.Decode(wire); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkItemEncode_Recipe measures a cold ToBytes() call on a fresh 1 MiB
// recipe transfer per iteration (see BenchmarkItemEncode_NestedList).
func BenchmarkItemEncode_Recipe(b *testing.B) {
	b.StopTimer()
	b.ReportAllocs()
	for range b.N {
		item := recipeItem()
		b.StartTimer()
		_ = item.ToBytes()
		b.StopTimer()
	}
}

// BenchmarkItemDecode_Recipe measures decoding a 1 MiB recipe transfer.
func BenchmarkItemDecode_Recipe(b *testing.B) {
	item := recipeItem()
	wire := item.ToBytes()

	b.ReportAllocs()
	b.SetBytes(int64(len(wire)))
	b.ResetTimer()
	for range b.N {
		if _, err := secs2.Decode(wire); err != nil {
			b.Fatal(err)
		}
	}
}
