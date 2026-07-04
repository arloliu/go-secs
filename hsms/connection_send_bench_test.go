package hsms

import (
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/arloliu/go-secs/v2/secs2"
)

// bufsSink prevents the compiler from dead-code-eliminating buildFrameBuffers calls whose
// net.Buffers result would otherwise be discarded.
var bufsSink net.Buffers

// BenchmarkBuildFrameBuffers is the R2 writev gate (success criterion #5): it proves the
// send-side buffer build is O(14), NOT O(body). buildFrameBuffers (send.go) assembles a fresh
// net.Buffers of {14-byte length+header prefix, body chunks} where the body chunks are zero-copy
// sub-slices of the immutable body (treeBody.Buffers returns net.Buffers{memoized-encoding}); the
// body bytes are REFERENCED for the vectored writev, never copied into one contiguous buffer.
//
// The bench sweeps the body size across 16 B .. 1 MiB. The memoized SECS-II encoding is warmed
// with one untimed call before ResetTimer so the loop measures the steady-state per-send cost
// (the memoization fires once, amortized to zero over a real connection's lifetime). The
// expectation — verified by the recorded allocs/op and B/op — is that BOTH are constant across
// body sizes: the send path does not scale with the body.
func BenchmarkBuildFrameBuffers(b *testing.B) {
	var sb [4]byte
	sb[0], sb[1], sb[2], sb[3] = 0x01, 0x02, 0x03, 0x04

	for _, n := range []int{16, 1 << 10, 64 << 10, 1 << 20} {
		item := secs2.A(strings.Repeat("x", n))
		msg, err := NewDataMessage(1, 1, false, 0x0001, sb, item)
		if err != nil {
			b.Fatalf("body=%d: %v", n, err)
		}

		// Warm the memoized encoding so the timed loop measures only the prefix build + the
		// zero-copy body-slice references (not the one-time item encode).
		bufsSink = buildFrameBuffers(msg)

		b.Run(fmt.Sprintf("body=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				bufsSink = buildFrameBuffers(msg)
			}
		})
	}
}

// BenchmarkBuildFrameBuffers_ListLeaves complements the body-SIZE sweep above with a leaf-COUNT
// sweep: a SECS-II list of N tiny leaves. It records that the send-side build stays at a CONSTANT
// allocs/op and B/op (same 3/88 as the size sweep) even for a 4096-leaf list — the immutable body's
// memoized encoding is referenced as a COMPACT net.Buffers (it does not fan out to one chunk per
// leaf), so the writev build allocates O(1) regardless of structural complexity as well as payload
// size. (Wall-time does grow with leaf count — assembling the buffers walks the structure — but that
// is a time cost, not an allocation/copy cost; the R2 "no per-message body copy" contract holds
// across both the size and structure dimensions.)
func BenchmarkBuildFrameBuffers_ListLeaves(b *testing.B) {
	var sb [4]byte
	sb[0], sb[1], sb[2], sb[3] = 0x01, 0x02, 0x03, 0x04

	for _, leaves := range []int{1, 16, 256, 4096} {
		items := make([]secs2.Item, leaves)
		for i := range items {
			items[i] = secs2.A("x")
		}
		item := secs2.L(items...)

		msg, err := NewDataMessage(1, 1, false, 0x0001, sb, item)
		if err != nil {
			b.Fatalf("leaves=%d: %v", leaves, err)
		}

		// Warm the memoized encoding so the timed loop measures only the buffer assembly.
		bufsSink = buildFrameBuffers(msg)

		b.Run(fmt.Sprintf("leaves=%d", leaves), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				bufsSink = buildFrameBuffers(msg)
			}
		})
	}
}
