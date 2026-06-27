package hsms

import (
	"strings"
	"testing"

	"github.com/arloliu/go-secs/secs2"
)

// snapshotRelayPayloads builds the five canonical payloads for the SnapshotForRelay
// gate benchmark.  Each payload is constructed once per benchmark binary run.
func snapshotRelayPayloads() []struct {
	name string
	item secs2.Item
} {
	// ascii64k: 2-element list with a 64 KiB ASCII string leaf.
	ascii64k := secs2.L(secs2.A("RECIPE_NAME"), secs2.A(strings.Repeat("A", 64*1024)))

	// bin64k: 2-element list with a 64 KiB binary blob.
	binData := make([]byte, 64*1024)
	bin64k := secs2.L(secs2.A("MAP"), secs2.B(binData))

	// list100: flat list of 100 ASCII items.
	list100Items := make([]secs2.Item, 100)
	for i := range list100Items {
		list100Items[i] = secs2.A("item0001")
	}
	list100 := secs2.L(list100Items...)

	// list1000: flat list of 1000 ASCII items.
	list1000Items := make([]secs2.Item, 1000)
	for i := range list1000Items {
		list1000Items[i] = secs2.A("item0001")
	}
	list1000 := secs2.L(list1000Items...)

	// nested256: depth-4, branching-factor-4 tree of secs2.A("leaf").
	// Total leaves: 4^4 = 256.
	nested256 := buildFanoutNestedList(4, 4)

	return []struct {
		name string
		item secs2.Item
	}{
		{"ascii64k", ascii64k},
		{"bin64k", bin64k},
		{"list100", list100},
		{"list1000", list1000},
		{"nested256", nested256},
	}
}

// buildFanoutNestedList builds a tree of ListItems with the given branching
// factor and depth.  Leaves are secs2.A("leaf") items.
func buildFanoutNestedList(depth, branch int) secs2.Item {
	if depth == 0 {
		return secs2.A("leaf")
	}
	children := make([]secs2.Item, branch)
	for i := range children {
		children[i] = buildFanoutNestedList(depth-1, branch)
	}

	return secs2.L(children...)
}

// BenchmarkSnapshotForRelay_Deep_CloneSerialize is the Phase-1 baseline: a full
// structural deep clone followed by serialization.  This is the cost that
// SnapshotForRelay must beat on every payload.
func BenchmarkSnapshotForRelay_Deep_CloneSerialize(b *testing.B) {
	for _, p := range snapshotRelayPayloads() {
		b.Run(p.name, func(b *testing.B) {
			msg, err := NewDataMessage(1, 1, true, 1234, GenerateMsgSystemBytes(), p.item.Clone())
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			for b.Loop() {
				c := msg.Clone()
				_ = c.ToBytes()
				c.Free()
			}
		})
	}
}

// BenchmarkSnapshotForRelay_Snapshot_CloneSerialize is the relay path: SnapshotForRelay
// followed by ToBytes.  The snapshot owns the pre-serialized frame; ToBytes
// returns it verbatim without re-encoding.  Must beat Deep_CloneSerialize in
// both ns/op and allocs/op on every payload — this is the hard gate.
func BenchmarkSnapshotForRelay_Snapshot_CloneSerialize(b *testing.B) {
	for _, p := range snapshotRelayPayloads() {
		b.Run(p.name, func(b *testing.B) {
			msg, err := NewDataMessage(1, 1, true, 1234, GenerateMsgSystemBytes(), p.item.Clone())
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			for b.Loop() {
				c := msg.SnapshotForRelay()
				_ = c.ToBytes()
				c.Free()
			}
		})
	}
}

// BenchmarkSnapshotForRelay_Snapshot_Structural measures the decode-on-access cost:
// the documented trade-off when a fan-out consumer needs item structure rather
// than just forwarding the wire bytes.
func BenchmarkSnapshotForRelay_Snapshot_Structural(b *testing.B) {
	for _, p := range snapshotRelayPayloads() {
		b.Run(p.name, func(b *testing.B) {
			msg, err := NewDataMessage(1, 1, true, 1234, GenerateMsgSystemBytes(), p.item.Clone())
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			for b.Loop() {
				c := msg.SnapshotForRelay()
				_, _ = c.Item().Get(0)
				c.Free()
			}
		})
	}
}
