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
	for b.Loop() {
		_ = secs2.I8(values...)
	}
}

// BenchmarkItemConstruct_NestedList builds the 100x13 nested-list shape.
func BenchmarkItemConstruct_NestedList(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		_ = nestedListItem()
	}
}

// BenchmarkItemEncode_NestedList measures a cold ToBytes() call on a fresh
// item per iteration, excluding construction from the timed section. v2's
// ToBytes always recomputes (no cache), but a fresh item per iteration keeps
// the methodology identical to secs2item/v1 (whose ListItem.ToBytes DOES
// cache its result on the item — see that file).
func BenchmarkItemEncode_NestedList(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		b.StopTimer()
		item := nestedListItem()
		b.StartTimer()
		_ = item.ToBytes()
		b.StopTimer()
		b.StartTimer()
	}
}

// BenchmarkItemDecode_NestedList measures decoding the nested-list wire
// bytes back into an Item tree via secs2.Decode.
func BenchmarkItemDecode_NestedList(b *testing.B) {
	item := nestedListItem()
	wire := item.ToBytes()

	b.ReportAllocs()
	b.SetBytes(int64(len(wire)))
	for b.Loop() {
		if _, err := secs2.Decode(wire); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkItemEncode_WaferMap measures a cold ToBytes() call on a fresh
// 100k-die wafer map per iteration (see BenchmarkItemEncode_NestedList).
func BenchmarkItemEncode_WaferMap(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		b.StopTimer()
		item := waferMapItem()
		b.StartTimer()
		_ = item.ToBytes()
		b.StopTimer()
		b.StartTimer()
	}
}

// BenchmarkItemDecode_WaferMap measures decoding a 100k-die wafer map.
func BenchmarkItemDecode_WaferMap(b *testing.B) {
	item := waferMapItem()
	wire := item.ToBytes()

	b.ReportAllocs()
	b.SetBytes(int64(len(wire)))
	for b.Loop() {
		if _, err := secs2.Decode(wire); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkItemDecodeOwned_WaferMap measures decoding a 100k-die wafer map via
// secs2.DecodeOwned (see BenchmarkItemDecodeOwned_NestedList for methodology).
func BenchmarkItemDecodeOwned_WaferMap(b *testing.B) {
	item := waferMapItem()
	wire := item.ToBytes()

	b.ReportAllocs()
	b.SetBytes(int64(len(wire)))
	for b.Loop() {
		b.StopTimer()
		owned := append([]byte(nil), wire...)
		b.StartTimer()
		if _, err := secs2.DecodeOwned(owned); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkItemDecodeOwned_NestedList measures decoding the nested-list wire
// bytes via secs2.DecodeOwned, the zero-copy variant for a buffer the caller
// already owns. The clone into an "owned" buffer happens outside the timed
// section (real callers already have such a buffer, e.g. a socket read).
func BenchmarkItemDecodeOwned_NestedList(b *testing.B) {
	item := nestedListItem()
	wire := item.ToBytes()

	b.ReportAllocs()
	b.SetBytes(int64(len(wire)))
	for b.Loop() {
		b.StopTimer()
		owned := append([]byte(nil), wire...)
		b.StartTimer()
		if _, err := secs2.DecodeOwned(owned); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkItemEncode_Recipe measures a cold ToBytes() call on a fresh 1 MiB
// recipe transfer per iteration (see BenchmarkItemEncode_NestedList).
func BenchmarkItemEncode_Recipe(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		b.StopTimer()
		item := recipeItem()
		b.StartTimer()
		_ = item.ToBytes()
		b.StopTimer()
		b.StartTimer()
	}
}

// BenchmarkItemDecode_Recipe measures decoding a 1 MiB recipe transfer.
func BenchmarkItemDecode_Recipe(b *testing.B) {
	item := recipeItem()
	wire := item.ToBytes()

	b.ReportAllocs()
	b.SetBytes(int64(len(wire)))
	for b.Loop() {
		if _, err := secs2.Decode(wire); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkItemDecodeOwned_Recipe measures decoding a 1 MiB recipe transfer via
// secs2.DecodeOwned (see BenchmarkItemDecodeOwned_NestedList for methodology).
func BenchmarkItemDecodeOwned_Recipe(b *testing.B) {
	item := recipeItem()
	wire := item.ToBytes()

	b.ReportAllocs()
	b.SetBytes(int64(len(wire)))
	for b.Loop() {
		b.StopTimer()
		owned := append([]byte(nil), wire...)
		b.StartTimer()
		if _, err := secs2.DecodeOwned(owned); err != nil {
			b.Fatal(err)
		}
	}
}
