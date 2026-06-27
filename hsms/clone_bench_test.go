package hsms

import (
	"testing"

	"github.com/arloliu/go-secs/secs2"
)

// The Clone vs CloneCodec benchmarks quantify the overhead of the CloneCodec
// wrapper relative to the underlying Clone. CloneCodec is a thin wrapper that
// only differs by an interface-to-interface conversion of the result, so any
// gap between the two pairs is purely that conversion (Clone itself dominates
// the cost via the deep secs2.Item copy).

// cloneBenchItems returns representative payloads spanning the realistic range:
// a small ASCII reply, a moderately nested list, and a large recipe blob.
func cloneBenchPayloads() []struct {
	name string
	item secs2.Item
} {
	// Large recipe: ~64 KiB ASCII blob wrapped in a 2-element list.
	recipe := make([]byte, 64*1024)
	for i := range recipe {
		recipe[i] = byte('A' + (i % 26))
	}

	// Nested list: 100 mixed leaf items.
	leaves := make([]secs2.Item, 0, 100)
	for i := range 100 {
		switch i % 4 {
		case 0:
			leaves = append(leaves, secs2.A("item"))
		case 1:
			leaves = append(leaves, secs2.U4(uint32(i)))
		case 2:
			leaves = append(leaves, secs2.I2(int16(i)))
		default:
			leaves = append(leaves, secs2.BOOLEAN(i%2 == 0))
		}
	}

	return []struct {
		name string
		item secs2.Item
	}{
		{"Small", secs2.A("ACK")},
		{"List100", secs2.L(leaves...)},
		{"Recipe64K", secs2.L(secs2.A("RECIPE_NAME"), secs2.A(string(recipe)))},
	}
}

func BenchmarkDataMessage_Clone(b *testing.B) {
	for _, p := range cloneBenchPayloads() {
		b.Run(p.name, func(b *testing.B) {
			msg, err := NewDataMessage(1, 1, true, 1234, GenerateMsgSystemBytes(), p.item.Clone())
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			for b.Loop() {
				cloned := msg.Clone()
				cloned.Free()
			}
		})
	}
}

func BenchmarkDataMessage_CloneCodec(b *testing.B) {
	for _, p := range cloneBenchPayloads() {
		b.Run(p.name, func(b *testing.B) {
			msg, err := NewDataMessage(1, 1, true, 1234, GenerateMsgSystemBytes(), p.item.Clone())
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			for b.Loop() {
				codec := msg.CloneCodec()
				cloned, _ := codec.(*DataMessage)
				cloned.Free()
			}
		})
	}
}
