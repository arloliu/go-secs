// Package v1 benchmarks secs2.Item construction, encode, and decode against
// the latest published go-secs v1 release. See secs2item/v2 for the v2
// counterpart — same benchmark names, same shapes, so benchstat can diff the
// two result files directly.
package v1

import (
	"testing"

	"github.com/arloliu/go-secs/hsms"
	"github.com/arloliu/go-secs/secs2"
)

// BenchmarkItemConstruct_IntList1000 builds a 1000-element I8 list. v1's
// items are pooled, so Free() returns each item to the pool — the intended
// steady-state usage, matching the repo's own secs2/int_bench_test.go.
func BenchmarkItemConstruct_IntList1000(b *testing.B) {
	values := intListValues()

	b.ReportAllocs()
	for b.Loop() {
		item := secs2.I8(values...)
		item.Free()
	}
}

// BenchmarkItemConstruct_NestedList builds the 100x13 nested-list shape.
func BenchmarkItemConstruct_NestedList(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		item := nestedListItem()
		item.Free()
	}
}

// BenchmarkItemEncode_NestedList measures a cold ToBytes() call on a fresh
// item per iteration. v1's ListItem.ToBytes caches its result on item.rawBytes,
// so re-encoding the SAME retained item would measure a near-free cache hit
// instead of real encode cost; building fresh (untimed) per iteration avoids
// that and keeps the comparison to v2 (which has no such cache) fair.
func BenchmarkItemEncode_NestedList(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		b.StopTimer()
		item := nestedListItem()
		b.StartTimer()
		_ = item.ToBytes()
		b.StopTimer()
		item.Free()
		b.StartTimer()
	}
}

// BenchmarkItemDecode_NestedList measures decoding the nested-list wire
// bytes back into an Item tree via hsms.DecodeSECS2Item (v1's raw-item
// decode entry point).
func BenchmarkItemDecode_NestedList(b *testing.B) {
	item := nestedListItem()
	wire := item.ToBytes()
	item.Free()

	b.ReportAllocs()
	b.SetBytes(int64(len(wire)))
	for b.Loop() {
		decoded, err := hsms.DecodeSECS2Item(wire)
		if err != nil {
			b.Fatal(err)
		}
		decoded.Free()
	}
}

// BenchmarkItemEncode_WaferMap measures a cold ToBytes() call on a fresh
// 100k-die wafer map per iteration (see BenchmarkItemEncode_NestedList for
// why: v1's ListItem.ToBytes caches its result on the item).
func BenchmarkItemEncode_WaferMap(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		b.StopTimer()
		item := waferMapItem()
		b.StartTimer()
		_ = item.ToBytes()
		b.StopTimer()
		item.Free()
		b.StartTimer()
	}
}

// BenchmarkItemDecode_WaferMap measures decoding a 100k-die wafer map.
func BenchmarkItemDecode_WaferMap(b *testing.B) {
	item := waferMapItem()
	wire := item.ToBytes()
	item.Free()

	b.ReportAllocs()
	b.SetBytes(int64(len(wire)))
	for b.Loop() {
		decoded, err := hsms.DecodeSECS2Item(wire)
		if err != nil {
			b.Fatal(err)
		}
		decoded.Free()
	}
}

// BenchmarkItemEncode_Recipe measures a cold ToBytes() call on a fresh 1 MiB
// recipe transfer per iteration (see BenchmarkItemEncode_NestedList for why).
func BenchmarkItemEncode_Recipe(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		b.StopTimer()
		item := recipeItem()
		b.StartTimer()
		_ = item.ToBytes()
		b.StopTimer()
		item.Free()
		b.StartTimer()
	}
}

// BenchmarkItemDecode_Recipe measures decoding a 1 MiB recipe transfer.
func BenchmarkItemDecode_Recipe(b *testing.B) {
	item := recipeItem()
	wire := item.ToBytes()
	item.Free()

	b.ReportAllocs()
	b.SetBytes(int64(len(wire)))
	for b.Loop() {
		decoded, err := hsms.DecodeSECS2Item(wire)
		if err != nil {
			b.Fatal(err)
		}
		decoded.Free()
	}
}
