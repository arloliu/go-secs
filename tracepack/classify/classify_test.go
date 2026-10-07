package classify

import (
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
)

// Frames below are hand-built per the tracepack format specification §8:
// a 4-byte big-endian length prefix, the 10-byte HSMS header, and message text.
// They are computed with python3 struct, never derived from the code under test.
//
// The SECS-II item bytes inside a body follow SEMI E5 §9.2:
// a format byte ((format code << 2) | length-byte count) followed by 1-3 big-endian length bytes and the payload.
// u1Item below is a single-element U1 item (format code 0o51) with value 5: {0xa5, 0x01, 0x05}.
var (
	// shortFrame13 is 13 bytes, one short of the 14-byte minimum (length prefix + header).
	shortFrame13 = []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}

	// okEmptyS1F1 is a 14-byte S1F1 data message with an empty body (header-only data message):
	// the 13-vs-14-byte boundary and the "empty message text" ok row.
	okEmptyS1F1 = []byte{0x00, 0x00, 0x00, 0x0a, 0x00, 0x01, 0x01, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01}

	// lengthMismatchTooBig repeats okEmptyS1F1's header but sets the length field to 11:
	// one more than the 10 bytes that actually follow the prefix.
	lengthMismatchTooBig = []byte{0x00, 0x00, 0x00, 0x0b, 0x00, 0x01, 0x01, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01}

	// lengthMismatchTooSmall repeats okEmptyS1F1's header but sets the length field to 9:
	// one less than the 10 bytes that actually follow the prefix.
	lengthMismatchTooSmall = []byte{0x00, 0x00, 0x00, 0x09, 0x00, 0x01, 0x01, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01}

	// badPType repeats okEmptyS1F1's header with PType (header byte 4, frame[8]) set to 1.
	badPType = []byte{0x00, 0x00, 0x00, 0x0a, 0x00, 0x01, 0x01, 0x01, 0x01, 0x00, 0x00, 0x00, 0x00, 0x01}

	// badSType8 is a control-shaped frame whose SType (header byte 5, frame[9]) is 8, a value SEMI E37 Table 5 does not define.
	badSType8 = []byte{0x00, 0x00, 0x00, 0x0a, 0xff, 0xff, 0x00, 0x00, 0x00, 0x08, 0x00, 0x00, 0x00, 0x01}

	// badSType10 is the same shape as badSType8 with SType 10, also undefined.
	badSType10 = []byte{0x00, 0x00, 0x00, 0x0a, 0xff, 0xff, 0x00, 0x00, 0x00, 0x0a, 0x00, 0x00, 0x00, 0x01}

	// okControlSeparate is a well-formed Separate.req (SType 9, no body):
	// control records reach ok as soon as the frame parses.
	okControlSeparate = []byte{0x00, 0x00, 0x00, 0x0a, 0xff, 0xff, 0x00, 0x00, 0x00, 0x09, 0x00, 0x00, 0x00, 0x02}

	// controlWithBody is a Select.req (SType 1) whose length field (21 = 10 + 11) says an 11-byte body follows the header,
	// which SEMI E37 §9.3.3.1 forbids for a control frame.
	controlWithBody = []byte{
		0x00, 0x00, 0x00, 0x15, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x03,
		0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a,
	}

	// itemDecodeErrorTruncated is a data message whose body is a truncated ASCII item:
	// the item header claims a 5-byte payload but only 2 bytes ("AB") follow.
	itemDecodeErrorTruncated = []byte{
		0x00, 0x00, 0x00, 0x0e, 0x00, 0x01, 0x81, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x04,
		0x41, 0x05, 0x41, 0x42,
	}

	// itemDecodeErrorNestedList is a data message whose body is a list item declaring 2 direct children but carrying only 1 (a u1Item),
	// so the second element is missing.
	itemDecodeErrorNestedList = []byte{
		0x00, 0x00, 0x00, 0x0f, 0x00, 0x01, 0x81, 0x03, 0x00, 0x00, 0x00, 0x00, 0x00, 0x05,
		0x01, 0x02, 0xa5, 0x01, 0x05,
	}

	// okSingleItem is a data message whose body is exactly one valid u1Item, with no excess bytes.
	okSingleItem = []byte{
		0x00, 0x00, 0x00, 0x0d, 0x00, 0x01, 0x81, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x06,
		0xa5, 0x01, 0x05,
	}

	// okWithTrailing is a data message whose body is a valid u1Item followed by 3 extra bytes.
	okWithTrailing = []byte{
		0x00, 0x00, 0x00, 0x10, 0x00, 0x01, 0x81, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x07,
		0xa5, 0x01, 0x05, 0xff, 0xff, 0xff,
	}

	// Order vectors below each match two decision-table predicates at once,
	// so a Frame that checked its own rows out of order would return the later-matching status instead of the earlier one.

	// badPTypeAndBadSType repeats badPType's header with SType (frame[9]) also set to 8:
	// both bad-ptype and bad-stype match, and bad-ptype must win
	// because it is checked first.
	badPTypeAndBadSType = []byte{0x00, 0x00, 0x00, 0x0a, 0x00, 0x01, 0x01, 0x01, 0x01, 0x08, 0x00, 0x00, 0x00, 0x01}

	// lengthMismatchAndBadPType repeats lengthMismatchTooBig's header with PType (frame[8]) also set to 1:
	// both length-mismatch and bad-ptype match, and length-mismatch must win.
	lengthMismatchAndBadPType = []byte{0x00, 0x00, 0x00, 0x0b, 0x00, 0x01, 0x01, 0x01, 0x01, 0x00, 0x00, 0x00, 0x00, 0x01}

	// badSTypeAndControlWithBody repeats controlWithBody with SType (frame[9]) changed from 1 to 8:
	// both bad-stype and control-with-body match, and bad-stype must win.
	badSTypeAndControlWithBody = []byte{
		0x00, 0x00, 0x00, 0x15, 0x00, 0x01, 0x00, 0x00, 0x00, 0x08, 0x00, 0x00, 0x00, 0x03,
		0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a,
	}
)

func TestFrame(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		frame        []byte
		maxFrameLen  int
		wantStatus   tracepack.DecodeStatus
		wantTrailing int
	}{
		// A zero-length frame panics a naive slice-index implementation, but must not panic here.
		{name: "short-frame: nil", frame: nil, wantStatus: tracepack.DecodeStatusShortFrame},
		{name: "short-frame: empty", frame: []byte{}, wantStatus: tracepack.DecodeStatusShortFrame},
		{name: "short-frame: 13 bytes", frame: shortFrame13, wantStatus: tracepack.DecodeStatusShortFrame},
		{name: "short-frame boundary: 14 bytes passes through to ok", frame: okEmptyS1F1, wantStatus: tracepack.DecodeStatusOK},
		{name: "length-mismatch: field one bigger than captured", frame: lengthMismatchTooBig, wantStatus: tracepack.DecodeStatusLengthMismatch},
		{name: "length-mismatch: field one smaller than captured", frame: lengthMismatchTooSmall, wantStatus: tracepack.DecodeStatusLengthMismatch},
		{name: "bad-ptype: PType 1", frame: badPType, wantStatus: tracepack.DecodeStatusBadPType},
		{name: "bad-stype: SType 8 rejected", frame: badSType8, wantStatus: tracepack.DecodeStatusBadSType},
		{name: "bad-stype: SType 10 rejected", frame: badSType10, wantStatus: tracepack.DecodeStatusBadSType},
		{name: "bad-stype boundary: SType 9 accepted, control parses to ok", frame: okControlSeparate, wantStatus: tracepack.DecodeStatusOK},
		{name: "control-with-body: SType 1 with an 11-byte body", frame: controlWithBody, wantStatus: tracepack.DecodeStatusControlWithBody},
		{name: "oversized: exactly at the limit is not oversized", frame: okEmptyS1F1, maxFrameLen: 14, wantStatus: tracepack.DecodeStatusOK},
		{name: "oversized: one over the limit", frame: okEmptyS1F1, maxFrameLen: 13, wantStatus: tracepack.DecodeStatusOversized},
		{name: "item-decode-error: truncated item", frame: itemDecodeErrorTruncated, wantStatus: tracepack.DecodeStatusItemDecodeError},
		{name: "item-decode-error: nested list missing an element", frame: itemDecodeErrorNestedList, wantStatus: tracepack.DecodeStatusItemDecodeError},
		{name: "ok: single valid item, no trailing", frame: okSingleItem, wantStatus: tracepack.DecodeStatusOK},
		{name: "ok-with-trailing: item followed by 3 trailing bytes", frame: okWithTrailing, wantStatus: tracepack.DecodeStatusOKWithTrailing, wantTrailing: 3},

		// Order: each row below matches two predicates at once; the earlier row in the decision table must win.
		{name: "order: bad-ptype wins over bad-stype", frame: badPTypeAndBadSType, wantStatus: tracepack.DecodeStatusBadPType},
		{name: "order: length-mismatch wins over bad-ptype", frame: lengthMismatchAndBadPType, wantStatus: tracepack.DecodeStatusLengthMismatch},
		{name: "order: bad-stype wins over control-with-body", frame: badSTypeAndControlWithBody, wantStatus: tracepack.DecodeStatusBadSType},
		{name: "order: control-with-body wins over oversized", frame: controlWithBody, maxFrameLen: 24, wantStatus: tracepack.DecodeStatusControlWithBody},
		{name: "order: oversized wins over item-decode-error", frame: itemDecodeErrorTruncated, maxFrameLen: 17, wantStatus: tracepack.DecodeStatusOversized},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			status, trailing := Frame(tt.frame, tt.maxFrameLen)
			require.Equal(t, tt.wantStatus, status)
			require.Equal(t, tt.wantTrailing, trailing)
		})
	}
}

// TestName checks only the "go-secs/" prefix:
// a go test binary does not embed a resolved dependency version here (confirmed under both go.work and GOWORK=off),
// so classifierName falls back to "go-secs/unknown";
// a real consumer binary built with go build resolves the full version.
// No test may depend on the version part.
func TestName(t *testing.T) {
	t.Parallel()

	require.True(t, strings.HasPrefix(Name, "go-secs/"), "Name = %q, want a go-secs/<version> prefix", Name)
	require.NotEmpty(t, strings.TrimPrefix(Name, "go-secs/"))
}

// TestNewCeilings checks the classifier New returns under ceilings beyond the range of a 32-bit int:
// each ceiling is kept exactly, and a 14-byte frame is oversized only under a ceiling below 14.
// On a 32-bit platform, 2^32 + 13 truncated to an int is 13, which would wrongly make that frame oversized.
func TestNewCeilings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		ceiling    uint64
		wantStatus tracepack.DecodeStatus
	}{
		{name: "no ceiling", ceiling: 0, wantStatus: tracepack.DecodeStatusOK},
		{name: "one under the frame length", ceiling: 13, wantStatus: tracepack.DecodeStatusOversized},
		{name: "the frame length", ceiling: 14, wantStatus: tracepack.DecodeStatusOK},
		{name: "largest 32-bit int", ceiling: math.MaxInt32, wantStatus: tracepack.DecodeStatusOK},
		{name: "2^32 + 13", ceiling: 1<<32 + 13, wantStatus: tracepack.DecodeStatusOK},
		{name: "largest max_frame_len value", ceiling: 1<<63 - 1, wantStatus: tracepack.DecodeStatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := New(tt.ceiling)
			require.Equal(t, Name, c.Name())
			require.Equal(t, tt.ceiling, c.MaxFrameLen())

			status, trailing := c.Frame(okEmptyS1F1)
			require.Equal(t, tt.wantStatus, status)
			require.Zero(t, trailing)
		})
	}
}

// TestNewMatchesFrame checks that the classifier New returns classifies every fixture as Frame does,
// under no ceiling and under ceilings around each fixture's length,
// and that the fixtures reach every decode_status Frame can return.
func TestNewMatchesFrame(t *testing.T) {
	t.Parallel()

	frames := [][]byte{
		nil,
		shortFrame13,
		okEmptyS1F1,
		lengthMismatchTooBig,
		badPType,
		badSType8,
		okControlSeparate,
		controlWithBody,
		itemDecodeErrorTruncated,
		okSingleItem,
		okWithTrailing,
	}

	seen := make(map[tracepack.DecodeStatus]bool)
	for _, frame := range frames {
		for _, ceiling := range []int{0, len(frame) - 1, len(frame), len(frame) + 1} {
			if ceiling < 0 {
				continue
			}

			wantStatus, wantTrailing := Frame(frame, ceiling)
			status, trailing := New(uint64(ceiling)).Frame(frame)
			require.Equal(t, wantStatus, status, "frame %x, ceiling %d", frame, ceiling)
			require.Equal(t, wantTrailing, trailing, "frame %x, ceiling %d", frame, ceiling)
			seen[status] = true
		}
	}

	for _, status := range []tracepack.DecodeStatus{
		tracepack.DecodeStatusOK,
		tracepack.DecodeStatusOKWithTrailing,
		tracepack.DecodeStatusShortFrame,
		tracepack.DecodeStatusLengthMismatch,
		tracepack.DecodeStatusBadPType,
		tracepack.DecodeStatusBadSType,
		tracepack.DecodeStatusControlWithBody,
		tracepack.DecodeStatusOversized,
		tracepack.DecodeStatusItemDecodeError,
	} {
		require.True(t, seen[status], "no fixture reached %v", status)
	}
}
