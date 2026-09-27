// FuzzFrame fuzzes Frame with arbitrary bytes and an arbitrary maxFrameLen.
//
// Frame must never panic.
// Its returned status must be one the tracepack.DecodeStatus registry names (never "unknown(<n>)").
// Trailing must never exceed the frame's own length.
package classify

import (
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
		f.Add(seed, 0)
		f.Add(seed, len(seed))
	}

	f.Fuzz(func(t *testing.T, frame []byte, maxFrameLen int) {
		status, trailing := Frame(frame, maxFrameLen)

		if trailing < 0 || trailing > len(frame) {
			t.Fatalf("trailing %d out of range for frame of %d bytes", trailing, len(frame))
		}

		if strings.HasPrefix(status.String(), "unknown(") {
			t.Fatalf("status %v (%d) is outside the DecodeStatus registry", status, status)
		}
	})
}
