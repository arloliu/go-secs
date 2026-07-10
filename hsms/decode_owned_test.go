package hsms

import (
	"testing"

	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// maxItemTreeAllocsForFixture is the pinned upper bound on the alloc count for
// decoding a fresh raw-frame DataMessage (ASCII "hello" body) and calling Item()
// on it via the owned decode path.
//
// Measured: new path = 5 allocs (DataMessage, decodeState, rawFrameBody interface
// boxing, ASCIIItem struct, string copy). Old path = 7 allocs (+AppendTo(nil)
// result slice + bytes.Clone inside secs2.Decode). Teeth-check: reverting
// decode() to the old path yields 7 > 5, failing this assertion.
const maxItemTreeAllocsForFixture = 5

// buildOwnedDataFrameForTest builds a [header||body] owned buffer that
// decodeOwnedFrame accepts, containing a DataMessage with an ASCII item body.
func buildOwnedDataFrameForTest(t *testing.T) []byte {
	t.Helper()

	body := secs2.NewASCIIItem("hello").ToBytes()
	frame := make([]byte, 10+len(body))

	// Header layout (10 bytes):
	//   [0-1]  SessionID = 0x0001
	//   [2]    Stream 1, no wait bit = 0x01
	//   [3]    Function 1 = 0x01
	//   [4]    PType = 0x00 (required)
	//   [5]    SType = 0x00 (DataMsgType)
	//   [6-9]  SystemBytes = 0x00000001
	frame[0] = 0x00
	frame[1] = 0x01
	frame[2] = 0x01
	frame[3] = 0x01
	frame[4] = 0x00
	frame[5] = 0x00
	frame[6] = 0x00
	frame[7] = 0x00
	frame[8] = 0x00
	frame[9] = 0x01

	copy(frame[10:], body)

	return frame
}

// decodeOwnedFrameMustData calls decodeOwnedFrame and asserts the result is a
// *DataMessage. Used inside the AllocsPerRun closure to measure per-decode cost.
func decodeOwnedFrameMustData(t *testing.T, frame []byte) *DataMessage {
	t.Helper()

	msg, err := decodeOwnedFrame(frame)
	require.NoError(t, err)

	dm, ok := msg.(*DataMessage)
	require.True(t, ok, "expected *DataMessage")

	return dm
}

// TestDataMessage_RawFrame_Item_NoBodyDoubleCopy asserts that the raw-frame
// decode path does NOT double-copy the body (no AppendTo(nil) + no bytes.Clone).
// It is a red test against the OLD decode() that did both copies; the new owned
// path must stay at or below maxItemTreeAllocsForFixture.
func TestDataMessage_RawFrame_Item_NoBodyDoubleCopy(t *testing.T) {
	frame := buildOwnedDataFrameForTest(t)

	msg, err := decodeOwnedFrame(frame)
	require.NoError(t, err)

	dm, ok := msg.(*DataMessage)
	require.True(t, ok, "expected *DataMessage from decodeOwnedFrame")

	allocs := testing.AllocsPerRun(50, func() {
		fresh := decodeOwnedFrameMustData(t, frame) // re-decode a fresh msg each run
		_, _ = fresh.Item()
	})

	// Baseline guard: the OLD decode() did body.AppendTo(nil) + secs2.Decode's bytes.Clone
	// = 2 extra body allocations. The owned path drops BOTH. This assertion must stay
	// strictly below the old path's count (which the Step-9 teeth-check re-measures).
	require.LessOrEqual(t, allocs, float64(maxItemTreeAllocsForFixture),
		"raw-frame Item() must not double-copy the body (no AppendTo(nil)+Clone)")

	_ = dm
}

// buildOwnedNumericDataFrameForTest builds a [header||body] owned buffer whose body is a list
// containing one scalar Int, one scalar Uint, and one scalar Float item — unlike the ASCII-only
// fixture above, this shape reaches secs2's decode-side scalar numeric slab.
func buildOwnedNumericDataFrameForTest(t *testing.T) []byte {
	t.Helper()

	body := secs2.NewListItem(
		secs2.I8(int64(-42)),
		secs2.U8(uint64(42)),
		secs2.F8(3.5),
	).ToBytes()
	frame := make([]byte, 10+len(body))

	frame[0] = 0x00
	frame[1] = 0x01
	frame[2] = 0x01
	frame[3] = 0x01
	frame[4] = 0x00
	frame[5] = 0x00
	frame[6] = 0x00
	frame[7] = 0x00
	frame[8] = 0x00
	frame[9] = 0x01

	copy(frame[10:], body)

	return frame
}

// TestDataMessage_RawFrame_Item_ScalarNumeric decodes a raw frame whose body contains scalar
// Int/Uint/Float leaves via DataMessage.Item(), exercising secs2's decode-side scalar slab on
// the production raw-frame path.
func TestDataMessage_RawFrame_Item_ScalarNumeric(t *testing.T) {
	frame := buildOwnedNumericDataFrameForTest(t)

	msg, err := decodeOwnedFrame(frame)
	require.NoError(t, err)

	dm, ok := msg.(*DataMessage)
	require.True(t, ok, "expected *DataMessage from decodeOwnedFrame")

	item, err := dm.Item()
	require.NoError(t, err)
	require.NoError(t, item.Error())

	children, err := item.ToList()
	require.NoError(t, err)
	require.Len(t, children, 3)

	intVal, err := children[0].IntAt(0)
	require.NoError(t, err)
	require.Equal(t, int64(-42), intVal)

	uintVal, err := children[1].UintAt(0)
	require.NoError(t, err)
	require.Equal(t, uint64(42), uintVal)

	floatVal, err := children[2].FloatAt(0)
	require.NoError(t, err)
	require.InDelta(t, 3.5, floatVal, 1e-9)
}
