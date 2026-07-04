package hsms

import (
	"testing"

	"github.com/arloliu/go-secs/v2/secs2"
)

// Fixed shapes used across all hsms benchmarks.
// Item: L{A[8], U4[4]} inside a data message on stream 1, function 1.
var (
	dataMsgWireFrame  []byte          // complete on-wire HSMS frame for a data message
	dataMsgOwnedFrame []byte          // the [header||body] owned frame (no 4-byte length prefix)
	benchDataMsg      *DataMessage    // tree-path DataMessage (for ToBytes benchmark)
	benchControlMsg   *ControlMessage // linktest.req (for ToBytes benchmark)

	// byteSink prevents the compiler from dead-code-eliminating ToBytes calls whose
	// result would otherwise be discarded by the blank identifier.
	byteSink []byte
)

func init() {
	item := secs2.L(secs2.A("abcdefgh"), secs2.U4(uint(1), uint(2), uint(3), uint(4)))
	var sb [4]byte
	sb[0], sb[1], sb[2], sb[3] = 0x01, 0x02, 0x03, 0x04

	msg, err := NewDataMessage(1, 1, true, 0x0001, sb, item)
	if err != nil {
		panic("hsms bench init: " + err.Error())
	}
	benchDataMsg = msg
	dataMsgWireFrame = msg.ToBytes()
	dataMsgOwnedFrame = append([]byte(nil), dataMsgWireFrame[4:]...) // strip the 4-byte length prefix

	benchControlMsg = NewLinktestReq(sb)
}

// BenchmarkDecodeHSMSMessage measures DecodeHSMSMessage for a small data-message
// frame. Each call copies the frame into an owned buffer (one alloc) and wraps the
// body zero-copy. The SECS-II body is NOT decoded here — that is lazy (first Item()
// call). The allocation is inherent: the owned frame buffer escapes inside the
// returned *DataMessage.
func BenchmarkDecodeHSMSMessage(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		_, _ = DecodeHSMSMessage(dataMsgWireFrame)
	}
}

// BenchmarkDataMessage_ToBytes measures the cost of serialising a tree-path
// DataMessage to its on-wire form. The treeBody memoises the SECS-II encoding on
// the first call (the ToBytes inside init), so the once fires only once; subsequent
// calls copy the memoised bytes into a new []byte. One alloc per call (the result
// buffer escapes to the caller). byteSink prevents the compiler from eliminating
// the call via DCE.
func BenchmarkDataMessage_ToBytes(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		byteSink = benchDataMsg.ToBytes()
	}
}

// BenchmarkControlMessage_ToBytes measures ControlMessage.ToBytes, which allocates
// exactly one 14-byte slice per call (the result escapes to the caller).
// byteSink prevents the compiler from eliminating the call via DCE.
func BenchmarkControlMessage_ToBytes(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		byteSink = benchControlMsg.ToBytes()
	}
}

// BenchmarkDataMessage_Item_RawFrame measures the exported lazy raw-frame decode path:
// DecodeHSMSMessage (copy the frame into an owned buffer) followed by the first Item() call.
// Sub-project 5 wired the zero-copy decode-owned entry (data_msg.go decode → secs2.DecodeOwned),
// so Item() no longer does the old AppendTo(nil) + bytes.Clone double copy that sub-project 2a
// documented. The remaining allocations are the one DecodeHSMSMessage frame copy plus the item
// tree itself; the SP4 baseline (173.7 ns, 512 B/op, 12 allocs/op) dropped accordingly. Not pooled.
func BenchmarkDataMessage_Item_RawFrame(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		msg, _ := DecodeHSMSMessage(dataMsgWireFrame)
		dm, ok := msg.(*DataMessage)
		if !ok {
			b.Fatal("unexpected message type")
		}
		_, _ = dm.Item()
	}
}

// BenchmarkDataMessage_Item_DecodeOwned measures the PRODUCTION recv decode path:
// decodeOwnedFrame over an already-owned [header||body] frame (no DecodeHSMSMessage copy,
// because the recv loop already owns a freshly read GC-owned frame) followed by the first
// Item() call, which decodes the SECS-II body zero-copy via secs2.DecodeOwned (leaf items alias
// the frame; no AppendTo(nil), no bytes.Clone). This isolates the decode-owned win from the
// exported entry's frame copy — its alloc count is below BenchmarkDataMessage_Item_RawFrame,
// which pays that extra copy. Frame reuse across iterations is safe: each iteration builds a
// fresh *DataMessage whose decode fires once, and the result is discarded.
func BenchmarkDataMessage_Item_DecodeOwned(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		msg, _ := decodeOwnedFrame(dataMsgOwnedFrame)
		dm, ok := msg.(*DataMessage)
		if !ok {
			b.Fatal("unexpected message type")
		}
		_, _ = dm.Item()
	}
}
