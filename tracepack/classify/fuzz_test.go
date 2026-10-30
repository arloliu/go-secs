// FuzzFrame fuzzes Frame with arbitrary bytes and an arbitrary maxFrameLen.
//
// Frame must never panic.
// Its returned status must be one the tracepack.DecodeStatus registry names (never "unknown(<n>)").
// Trailing must never exceed the frame's own length.
// The classifier New returns must classify the frame as Frame does under the same ceiling.
package classify

import (
	"math"
	"strings"
	"testing"
)

// FuzzFrame fuzzes Frame.
func FuzzFrame(f *testing.F) {
	seeds := [][]byte{
		shortFrame13,
		okEmptyS1F1,
		lengthMismatchTooBig,
		lengthMismatchTooSmall,
		badPType,
		badSType8,
		badSType10,
		okControlSeparate,
		controlWithBody,
		itemDecodeErrorTruncated,
		itemDecodeErrorNestedList,
		okSingleItem,
		okWithTrailing,
		badPTypeAndBadSType,
		lengthMismatchAndBadPType,
		badSTypeAndControlWithBody,
		nil,
		{},
	}
	for _, seed := range seeds {
		f.Add(seed, uint64(0))
		f.Add(seed, uint64(len(seed)))
		f.Add(seed, uint64(math.MaxUint64))
	}
	// The frames of the conformance corpus, each under its vector's max_frame_len.
	for _, cf := range corpusFrames(f) {
		f.Add(cf.frame, cf.maxFrameLen)
	}

	f.Fuzz(func(t *testing.T, frame []byte, maxFrameLen uint64) {
		status, trailing := Frame(frame, maxFrameLen)

		if trailing < 0 || trailing > len(frame) {
			t.Fatalf("trailing %d out of range for frame of %d bytes", trailing, len(frame))
		}

		if strings.HasPrefix(status.String(), "unknown(") {
			t.Fatalf("status %v (%d) is outside the DecodeStatus registry", status, status)
		}

		if s, n := New(maxFrameLen).Frame(frame); s != status || n != trailing {
			t.Fatalf("New(%d).Frame = (%v, %d), Frame(_, %d) = (%v, %d)", maxFrameLen, s, n, maxFrameLen, status, trailing)
		}
	})
}
