package hsms

import (
	"os"
	"testing"

	"github.com/arloliu/go-secs/secs2"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	UseStreamFunctionSingleQuote()
	os.Exit(m.Run())
}

func TestDataMessage_EmptyItem(t *testing.T) {
	require := require.New(t)

	msg, err := NewDataMessage(0, 1, true, 123, []byte{}, secs2.NewEmptyItem())
	require.NoError(err)
	require.Equal(uint8(0), msg.StreamCode())
	require.Equal(uint8(1), msg.FunctionCode())
	require.Equal(true, msg.WaitBit())
	require.Equal(uint16(123), msg.SessionID())
	require.Equal([]byte{0, 0, 0, 0}, msg.SystemBytes())
	require.Equal("'S0F1' W", msg.SMLHeader())
	require.Equal("'S0F1' W\n.", msg.ToSML())
}

func TestDataMessage(t *testing.T) {
	require := require.New(t)

	tests := []struct {
		description        string // Test case description
		inputStreamCode    uint8
		inputFunctionCode  uint8
		inputReplyExpected bool
		inputDataItem      secs2.Item
		inputSessionID     uint16
		inputSystemBytes   []byte
		expectedToBytes    []byte // expected result from ToBytes()
		expectedToSML      string // expected result from ToSML()
	}{
		{
			description:        "S1F1 W, ASCII node",
			inputStreamCode:    1,
			inputFunctionCode:  1,
			inputReplyExpected: true,
			inputDataItem:      secs2.A("text"),
			inputSessionID:     1,
			inputSystemBytes:   []byte{0, 0, 0, 1},
			expectedToBytes: []byte{
				0, 0, 0, 16, 0, 1, 0x81, 1, 0, 0, 0, 0, 0, 1,
				0x41, 4, 0x74, 0x65, 0x78, 0x74,
			},
			expectedToSML: "'S1F1' W\n<A[4] \"text\">\n.",
		},
		{
			description:        "S64F128, boolean node",
			inputStreamCode:    64,
			inputFunctionCode:  128,
			inputReplyExpected: false,
			inputDataItem:      secs2.BOOLEAN(true, false),
			inputSessionID:     256,
			inputSystemBytes:   []byte{0x12, 0x34, 0x56, 0x78},
			expectedToBytes: []byte{
				0, 0, 0, 14, 0x01, 0x00, 0x40, 0x80, 0, 0, 0x12, 0x34, 0x56, 0x78,
				37, 2, 1, 0,
			},
			expectedToSML: "'S64F128'\n<BOOLEAN[2] T F>\n.",
		},
		{
			description:        "S127F255 W, nested list node",
			inputStreamCode:    127,
			inputFunctionCode:  255,
			inputReplyExpected: true,
			inputDataItem:      secs2.L(secs2.L(), secs2.L(secs2.I1(64, 127))),
			inputSessionID:     0xFFFF,
			inputSystemBytes:   []byte{0xf1, 0xf2, 0xf3, 0xf4},
			expectedToBytes: []byte{
				0, 0, 0, 0x14, 0xff, 0xff, 0xff, 0xff, 0, 0, 0xf1, 0xf2, 0xf3, 0xf4,
				0x1, 0x2, 0x1, 0x0, 0x1, 0x1, 0x65, 0x2, 0x40, 0x7f,
			},
			expectedToSML: `'S127F255' W
<L[2]
  <L[0]>
  <L[1]
    <I1[2] 64 127>
  >
>
.`,
		},
	}

	for i, test := range tests {
		t.Logf("Test #%d: %s", i, test.description)
		msg, err := NewDataMessage(
			test.inputStreamCode,
			test.inputFunctionCode,
			test.inputReplyExpected,
			test.inputSessionID,
			test.inputSystemBytes,
			test.inputDataItem,
		)
		require.NoError(err)
		require.Equal(test.expectedToBytes, msg.ToBytes())
		require.Equal(test.expectedToSML, msg.ToSML())
		require.True(msg.IsDataMessage())

		msg2, err := NewDataMessageFromRawItem(
			test.inputStreamCode,
			test.inputFunctionCode,
			test.inputReplyExpected,
			test.inputSessionID,
			test.inputSystemBytes,
			test.inputDataItem.ToBytes(),
		)
		require.NoError(err)
		require.Equal(test.expectedToBytes, msg2.ToBytes())
		require.Equal(test.expectedToSML, msg2.ToSML())
		require.True(msg2.IsDataMessage())
	}
}

func TestDataMessage_Set(t *testing.T) {
	require := require.New(t)

	// create a new DataMessage with initial values
	msg, err := NewDataMessage(1, 1, true, 0, []byte{}, secs2.NewEmptyItem())
	require.NoError(err)
	require.NotNil(msg)

	// verify initial values
	require.Equal(uint32(0), msg.ID())
	require.Equal(uint16(0), msg.SessionID())
	require.Equal([]byte{0, 0, 0, 0}, msg.SystemBytes())
	require.Equal("", msg.Name())

	// set and verify SessionID
	msg.SetSessionID(123)
	require.Equal(uint16(123), msg.SessionID())

	// set and verify SystemBytes
	err = msg.SetSystemBytes([]byte{0x12, 0x34, 0x56, 0x78})
	require.NoError(err)
	require.Equal([]byte{0x12, 0x34, 0x56, 0x78}, msg.SystemBytes())

	// set and verify Name
	msg.SetName("test")
	require.Equal("test", msg.Name())

	// attempt to set an invalid header and expect an error
	err = msg.SetHeader([]byte{0})
	require.ErrorIs(err, ErrInvalidHeaderLength)

	// set a valid header and verify the values
	err = msg.SetHeader([]byte{0, 0x7b, 0x81, 0x1, 0x0, 0x0, 0x12, 0x34, 0x56, 0x78})
	require.NoError(err)
	require.Equal([]byte{0x12, 0x34, 0x56, 0x78}, msg.SystemBytes())
	require.Equal(uint16(0x7b), msg.SessionID())
	require.Equal(uint8(1), msg.StreamCode())
	require.Equal(uint8(1), msg.FunctionCode())

	// clone the message and verify the cloned values
	cloned := msg.Clone()
	clonedDataMsg, ok := cloned.(*DataMessage)
	require.True(ok)

	require.Equal(msg.ID(), clonedDataMsg.ID())
	require.Equal(msg.StreamCode(), clonedDataMsg.StreamCode())
	require.Equal(msg.FunctionCode(), clonedDataMsg.FunctionCode())
	require.Equal(msg.WaitBit(), clonedDataMsg.WaitBit())
	require.Equal(msg.SessionID(), clonedDataMsg.SessionID())
	require.Equal(msg.SMLHeader(), clonedDataMsg.SMLHeader())
	require.Equal(msg.SystemBytes(), clonedDataMsg.SystemBytes())
	require.Equal(msg.Name(), clonedDataMsg.Name())
	require.Equal(msg.Name(), clonedDataMsg.Name())
}
