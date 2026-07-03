package hsmsss

import (
	"fmt"
	"sync"
	"testing"
)

// frameSink prevents the compiler from dead-code-eliminating the allocated frame buffers.
var frameSink []byte

// readBufSizes are representative on-wire frame sizes (10-byte header + body): a tiny control-ish
// frame, a typical data frame, and two large payloads.
var readBufSizes = []int{10 + 16, 10 + 1024, 10 + (64 << 10), 10 + (1 << 20)}

// BenchmarkReadBuf_OwnedPerMessage measures the LIVE §5.F option (b) read path: a fresh GC-owned
// buffer per message (make([]byte, msgLen), transport.makeFrame) that the read fills, which the
// decoded message then owns zero-copy. The copy models the socket read filling the buffer. This
// is the production strategy: option (b) is forced by the zero-copy decode-owned-frame entry
// (a decoded message owns its raw frame; leaf items may zero-copy-reference it, so the frame must
// be GC-owned, not returned to a pool).
func BenchmarkReadBuf_OwnedPerMessage(b *testing.B) {
	for _, n := range readBufSizes {
		src := make([]byte, n) // stand-in for the bytes arriving on the socket
		b.Run(fmt.Sprintf("size=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(n))
			b.ResetTimer()
			for range b.N {
				frame := make([]byte, n) // option (b): fresh GC-owned frame
				copy(frame, src)         // the read fills it
				frameSink = frame
			}
		})
	}
}

// BenchmarkReadBuf_PooledScratch measures a SYNTHESIZED §5.F option (a) alternative (NOT in
// production): a pooled scratch read buffer (a *[]byte pooled to avoid interface boxing, so the
// scratch is reused after warm-up) plus the copy of the body into a fresh owned buffer required
// before any zero-copy sharing. Option (a) still pays one owned allocation of the body PLUS a
// second copy, so it cannot beat option (b) on the decode-owned model — this bench records the
// (a)-vs-(b) comparison the spec (§5.F / D5a-2) asked for.
func BenchmarkReadBuf_PooledScratch(b *testing.B) {
	pool := sync.Pool{New: func() any { s := make([]byte, 0); return &s }}

	for _, n := range readBufSizes {
		src := make([]byte, n)
		bodyLen := n - 10
		b.Run(fmt.Sprintf("size=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(n))
			b.ResetTimer()
			for range b.N {
				bp, _ := pool.Get().(*[]byte) // New always returns *[]byte
				scratch := *bp
				if cap(scratch) < n {
					scratch = make([]byte, n)
				} else {
					scratch = scratch[:n]
				}
				copy(scratch, src) // the read into pooled scratch

				owned := make([]byte, bodyLen) // must own the shared body before decode
				copy(owned, scratch[10:])      // second copy (the (a) tax)

				*bp = scratch
				pool.Put(bp)
				frameSink = owned
			}
		})
	}
}
