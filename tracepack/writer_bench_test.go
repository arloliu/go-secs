package tracepack

import (
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

// BenchmarkWriterAppendFrame appends 4 KiB data frames to a Writer, as a consumer does:
// with the zstd codec and validation on, and with CodecNone and validation off, the Writer's cheapest path.
// Under copy each frame is first copied into a buffer of the producer's, as a consumer copies a received message;
// the difference between copy and nocopy is the cost of one payload copy, beside Append's own copy into the open block.
func BenchmarkWriterAppendFrame(b *testing.B) {
	const payloadLen = 4 << 10
	frame := hsmsFrame(6, 11, make([]byte, payloadLen-14))
	for i := range frame[14:] {
		frame[14+i] = byte(i * 7)
	}

	for _, codec := range []struct {
		name         string
		uncompressed bool
	}{{"zstd", false}, {"none", true}} {
		for _, producerCopy := range []bool{false, true} {
			name := codec.name + "/nocopy"
			if producerCopy {
				name = codec.name + "/copy"
			}
			b.Run(name, func(b *testing.B) {
				w, err := NewWriter(io.Discard, WriterOptions{
					Meta: repairTestMeta(), AssignSeq: true, Uncompressed: codec.uncompressed, SkipValidation: codec.uncompressed,
				})
				require.NoError(b, err)
				r := frameRecord(0, frame)
				buf := make([]byte, 0, len(frame))
				b.SetBytes(int64(len(frame)))
				b.ReportAllocs()
				for b.Loop() {
					if producerCopy {
						buf = append(buf[:0], frame...)
						r.Payload = buf
					}
					if err := w.Append(&r); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
