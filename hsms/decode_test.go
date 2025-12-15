//nolint:errcheck
package hsms

import (
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/arloliu/go-secs/secs2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecode_DataMessage(t *testing.T) {
	tests := []struct {
		description          string // test case description
		input                []byte // input
		expectedType         int
		expectedStreamCode   uint8
		expectedFunctionCode uint8
		expectedWaitBit      bool
		expectedSessionID    uint16
		expectedSystemBytes  []byte
		expectedToSML        string
	}{
		{
			description:          "S0F0 empty data item",
			input:                []byte{0, 0, 0, 10, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
			expectedType:         DataMsgType,
			expectedStreamCode:   0,
			expectedFunctionCode: 0,
			expectedWaitBit:      false,
			expectedSessionID:    0,
			expectedSystemBytes:  []byte{0, 0, 0, 0},
			expectedToSML:        "S0F0\n.",
		},
		{
			description: `S1F1 W <A[11] "lorem ipsum">`,
			input: []byte{
				0, 0, 0, 23, 0, 1, 129, 1, 0, 0, 0, 0, 0, 1,
				0x41, 11, 0x6C, 0x6F, 0x72, 0x65, 0x6D, 0x20, 0x69, 0x70, 0x73, 0x75, 0x6D,
			},
			expectedType:         DataMsgType,
			expectedStreamCode:   1,
			expectedFunctionCode: 1,
			expectedWaitBit:      true,
			expectedSessionID:    1,
			expectedSystemBytes:  []byte{0, 0, 0, 1},
			expectedToSML:        "S1F1 W\n<A[11] \"lorem ipsum\">\n.",
		},
		{
			description: `S50F50 <B[0]>`,
			input: []byte{
				0, 0, 0, 12, 0, 2, 50, 50, 0, 0, 0, 0, 0, 2,
				33, 0,
			},
			expectedType:         DataMsgType,
			expectedStreamCode:   50,
			expectedFunctionCode: 50,
			expectedWaitBit:      false,
			expectedSessionID:    2,
			expectedSystemBytes:  []byte{0, 0, 0, 2},
			expectedToSML:        "S50F50\n<B[0]>\n.",
		},
		{
			description: `S126F254 <BOOLEAN[2] True False>`,
			input: []byte{
				0, 0, 0, 14, 0xFE, 0xFE, 126, 254, 0, 0, 0xFE, 0xFE, 0xFE, 0xFE,
				37, 2, 1, 0,
			},
			expectedType:         DataMsgType,
			expectedStreamCode:   126,
			expectedFunctionCode: 254,
			expectedWaitBit:      false,
			expectedSessionID:    65278,
			expectedSystemBytes:  []byte{0xFE, 0xFE, 0xFE, 0xFE},
			expectedToSML:        "S126F254\n<BOOLEAN[2] True False>\n.",
		},
		{
			description: `S127F255 W <F4[3] -1.0 0.0 3.141592>`,
			input: []byte{
				0, 0, 0, 24, 0xFF, 0xFE, 255, 255, 0, 0, 0xFF, 0xFF, 0xFF, 0xFE,
				0x91, 12,
				0xBF, 0x80, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00,
				0x40, 0x49, 0x0F, 0xD8,
			},
			expectedType:         DataMsgType,
			expectedStreamCode:   127,
			expectedFunctionCode: 255,
			expectedWaitBit:      true,
			expectedSessionID:    65534,
			expectedSystemBytes:  []byte{0xFF, 0xFF, 0xFF, 0xFE},
			expectedToSML:        "S127F255 W\n<F4[3] -1 0 3.14159203>\n.",
		},
		{
			description: `S0F0 <F8[3] -1 0 1>`,
			input: []byte{
				0, 0, 0, 36, 0xFF, 0xFF, 0, 0, 0, 0, 0xFF, 0xFF, 0xFF, 0xFF,
				0x81, 24,
				0xBF, 0xF0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x3F, 0xF0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			},
			expectedType:         DataMsgType,
			expectedStreamCode:   0,
			expectedFunctionCode: 0,
			expectedWaitBit:      false,
			expectedSessionID:    65535,
			expectedSystemBytes:  []byte{0xFF, 0xFF, 0xFF, 0xFF},
			expectedToSML:        "S0F0\n<F8[3] -1 0 1>\n.",
		},
		{
			description: `S0F0, nested list`,
			input: []byte{
				0, 0, 0, 88, 0xFF, 0xFF, 0, 0, 0, 0, 0xFF, 0xFF, 0xFF, 0xFF,
				0x01, 3, // L[3]
				0x01, 0, //   L[0]
				0x01, 4, //   L[4]
				0x65, 0,
				0x69, 2, 0x80, 0x00,
				0x71, 8,
				0xFF, 0xFF, 0xFF, 0xFF,
				0, 0, 0, 0,
				0x61, 32,
				0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFE,
				0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF,
				0, 0, 0, 0, 0, 0, 0, 0,
				0, 0, 0, 0, 0, 0, 0, 0x2A,
				0x01, 4, //   L[4]
				0xA5, 4, 0, 1, 0xFE, 0xFF,
				0xA9, 4, 0, 0, 0xFF, 0xFF,
				0xB1, 4, 0, 0, 0, 0x2A,
				0xA1, 0,
			},
			expectedType:         DataMsgType,
			expectedStreamCode:   0,
			expectedFunctionCode: 0,
			expectedWaitBit:      false,
			expectedSessionID:    65535,
			expectedSystemBytes:  []byte{0xFF, 0xFF, 0xFF, 0xFF},
			expectedToSML: `S0F0
<L[3]
  <L[0]>
  <L[4]
    <I1[0]>
    <I2[1] -32768>
    <I4[2] -1 0>
    <I8[4] -2 -1 0 42>
  >
  <L[4]
    <U1[4] 0 1 254 255>
    <U2[2] 0 65535>
    <U4[1] 42>
    <U8[0]>
  >
>
.`,
		},
	}

	require := require.New(t)
	assert := assert.New(t)

	UseStreamFunctionNoQuote()
	for i, test := range tests {
		t.Logf("Test #%d: %s", i, test.description)
		msgLen := decodeMessageLength(test.input)
		msg, err := DecodeMessage(msgLen, test.input[4:])
		require.NoError(err)
		assert.Equal(test.expectedType, msg.Type())
		assert.Equal(test.input, msg.ToBytes())
		assert.Equal(test.expectedStreamCode, msg.(*DataMessage).StreamCode())
		assert.Equal(test.expectedFunctionCode, msg.(*DataMessage).FunctionCode())
		assert.Equal(test.expectedWaitBit, msg.(*DataMessage).WaitBit())
		assert.Equal(test.expectedSessionID, msg.(*DataMessage).SessionID())
		assert.Equal(test.expectedSystemBytes, msg.(*DataMessage).SystemBytes())
		assert.Equal(test.expectedToSML, msg.(*DataMessage).ToSML())

		msg2, err := DecodeHSMSMessage(msg.ToBytes())
		require.NoError(err)
		assert.Equal(test.expectedType, msg2.Type())
		assert.Equal(test.input, msg2.ToBytes())
		assert.Equal(test.expectedStreamCode, msg2.(*DataMessage).StreamCode())
		assert.Equal(test.expectedFunctionCode, msg2.(*DataMessage).FunctionCode())
		assert.Equal(test.expectedWaitBit, msg2.(*DataMessage).WaitBit())
		assert.Equal(test.expectedSessionID, msg2.(*DataMessage).SessionID())
		assert.Equal(test.expectedSystemBytes, msg2.(*DataMessage).SystemBytes())
		assert.Equal(test.expectedToSML, msg2.(*DataMessage).ToSML())

		item, err := DecodeSECS2Item(msg.Item().ToBytes())
		require.NoError(err)
		assert.Equal(msg.Item().ToSML(), item.ToSML())
	}
}

func TestDecode_ControlMessage(t *testing.T) {
	tests := []struct {
		input        []byte // input to the parser
		expectedType int    // expected message type
	}{
		{
			input:        []byte{0, 0, 0, 10, 0xba, 0xd3, 0, 0, 0, 1, 0, 0, 0, 0},
			expectedType: SelectReqType,
		},
		{
			input:        []byte{0, 0, 0, 10, 0x0d, 0xd9, 0, 1, 0, 2, 0, 0, 0, 1},
			expectedType: SelectRspType,
		},
		{
			input:        []byte{0, 0, 0, 10, 1, 0, 0, 0, 0, 3, 3, 2, 1, 0},
			expectedType: DeselectReqType,
		},
		{
			input:        []byte{0, 0, 0, 10, 0x03, 0x04, 0, 2, 0, 4, 0x01, 0xfd, 0xca, 0xff},
			expectedType: DeselectRspType,
		},
		{
			input:        []byte{0, 0, 0, 10, 0xa1, 0xc2, 0, 0, 0, 5, 0xff, 0xd9, 0xff, 0x8f},
			expectedType: LinkTestReqType,
		},
		{
			input:        []byte{0, 0, 0, 10, 0xff, 0xff, 0, 0, 0, 6, 0xff, 0xff, 0xff, 0xff},
			expectedType: LinkTestRspType,
		},
		{
			input:        []byte{0, 0, 0, 10, 0x12, 0x34, 9, 3, 0, 7, 0xfc, 0xfd, 0xfe, 0x75},
			expectedType: RejectReqType,
		},
		{
			input:        []byte{0, 0, 0, 10, 0xfe, 0xfe, 0, 0, 0, 9, 0xfe, 0xd9, 0x8f, 0xfe},
			expectedType: SeparateReqType,
		},
	}

	require := require.New(t)
	assert := assert.New(t)

	for _, test := range tests {
		msgLen := decodeMessageLength(test.input)
		msg, err := DecodeMessage(msgLen, test.input[4:])
		require.NoError(err)
		assert.Equal(test.expectedType, msg.Type())
		assert.Equal(test.input, msg.ToBytes())

		msg2, err := DecodeHSMSMessage(msg.ToBytes())
		require.NoError(err)
		assert.Equal(test.expectedType, msg2.Type())
		assert.Equal(test.input, msg2.ToBytes())

		item, err := DecodeSECS2Item(msg.Item().ToBytes())
		require.NoError(err)
		assert.True(item.IsEmpty())
		assert.Equal(msg.Item().ToSML(), item.ToSML())
	}
}

func TestDecodeMessage_LargeData(t *testing.T) {
	require := require.New(t)

	expectedSize := 1 << 16
	bigValues := make([]secs2.Item, 0, expectedSize)
	for i := 0; i < expectedSize; i++ {
		bigValues = append(bigValues, secs2.NewASCIIItem(fmt.Sprintf("%d", i)))
	}
	msg, err := NewDataMessage(1, 1, true, 1234, GenerateMsgSystemBytes(), secs2.L(bigValues...))
	require.NoError(err)
	require.NotNil(msg)

	input := msg.ToBytes()[4:]
	decodedMsg, err := DecodeMessage(uint32(len(input)), input)
	require.NoError(err)
	require.NotNil(decodedMsg)

	listItem := decodedMsg.Item()
	require.Equal(expectedSize, listItem.Size())

	items, err := listItem.ToList()
	require.NoError(err)
	require.NotNil(items)
	require.Equal(expectedSize, len(items))

	for i, item := range items {
		str, err := item.ToASCII()
		require.NoError(err)
		require.Equal(fmt.Sprintf("%d", i), str)
	}
}

func TestDecode_ListItemWithBytes(t *testing.T) {
	require := require.New(t)
	msg, err := NewDataMessage(1, 1, true, 1234, GenerateMsgSystemBytes(),
		secs2.L(
			secs2.I8(1),
			secs2.B([]byte{0x01, 0x02, 0x03}),
			secs2.L(
				secs2.I4(10),
				secs2.B([]byte{0x04, 0x05, 0x06}),
			),
		),
	)
	require.NoError(err)
	require.NotNil(msg)

	input := msg.ToBytes()[4:]
	decodedMsg, err := DecodeMessage(uint32(len(input)), input)
	require.NoError(err)
	require.NotNil(decodedMsg)

	listItem := decodedMsg.Item()
	require.Equal(
		input[10:],
		listItem.ToBytes(),
	)
}

// decodeMessageLength decodes the message length from the first 4 bytes of an HSMS message.
// The message length is encoded as a 32-bit unsigned integer in big-endian order.
func decodeMessageLength(input []byte) uint32 {
	return binary.BigEndian.Uint32(input[:4])
}

func TestDecode_Errors(t *testing.T) {
	t.Run("MessageTooShort", func(t *testing.T) {
		// Less than MinHSMSSize (14 bytes)
		_, err := DecodeHSMSMessage([]byte{0, 0, 0, 10, 0, 0, 0, 0, 0, 0, 0, 0, 0})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid hsms message length")
	})

	t.Run("MessageLengthMismatch", func(t *testing.T) {
		// msgLen says 20 but only 10 bytes provided
		_, err := DecodeMessage(20, []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "length mismatch")
	})

	t.Run("InvalidPType", func(t *testing.T) {
		// PType (byte 4) is 1 instead of 0
		_, err := DecodeMessage(10, []byte{0, 0, 0, 0, 1, 0, 0, 0, 0, 0})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid PType")
	})

	t.Run("InvalidSType", func(t *testing.T) {
		// SType (byte 5) is 99 (undefined)
		_, err := DecodeMessage(10, []byte{0, 0, 0, 0, 0, 99, 0, 0, 0, 0})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "undefined SType")
	})

	t.Run("TruncatedASCIIItem", func(t *testing.T) {
		// ASCII item header claims 10 bytes but only 5 provided
		// Format byte: 0x41 (ASCII, 1 length byte), length: 10, data: only 5 bytes
		input := []byte{
			0, 0, 0, 0, 0, 0, 0, 0, 0, 0, // header
			0x41, 10, 'h', 'e', 'l', 'l', 'o', // truncated
		}
		_, err := DecodeMessage(uint32(len(input)), input)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unexpected end of message")
	})

	t.Run("TruncatedIntItem", func(t *testing.T) {
		// I4 item claims 8 bytes (2 integers) but only 4 provided
		// Format byte: 0x71 (I4, 1 length byte), length: 8, data: only 4 bytes
		input := []byte{
			0, 0, 0, 0, 0, 0, 0, 0, 0, 0, // header
			0x71, 8, 0, 0, 0, 1, // truncated - only 4 bytes instead of 8
		}
		_, err := DecodeMessage(uint32(len(input)), input)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unexpected end of message")
	})

	t.Run("TruncatedListItem", func(t *testing.T) {
		// List claims 5 items but only 1 item's worth of data
		// Format byte: 0x01 (List, 1 length byte), length: 5
		input := []byte{
			0, 0, 0, 0, 0, 0, 0, 0, 0, 0, // header
			0x01, 5, // list with 5 items
			0x41, 1, 'a', // only 1 ASCII item
		}
		_, err := DecodeMessage(uint32(len(input)), input)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "list claims")
	})

	t.Run("ZeroLengthBytes", func(t *testing.T) {
		// Format byte with 0 length bytes (invalid)
		input := []byte{
			0, 0, 0, 0, 0, 0, 0, 0, 0, 0, // header
			0x40, // ASCII format but 0 length bytes (lower 2 bits = 0)
		}
		_, err := DecodeMessage(uint32(len(input)), input)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "length bytes count is zero")
	})

	t.Run("InvalidFormatCode", func(t *testing.T) {
		// Format code 0x3F (format code 15, invalid)
		input := []byte{
			0, 0, 0, 0, 0, 0, 0, 0, 0, 0, // header
			0x3D, 0, // format code 15, 1 length byte, length 0
		}
		_, err := DecodeMessage(uint32(len(input)), input)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid format")
	})

	t.Run("InvalidIntItemLength", func(t *testing.T) {
		// I4 item with length 5 (not divisible by 4)
		input := []byte{
			0, 0, 0, 0, 0, 0, 0, 0, 0, 0, // header
			0x71, 5, 0, 0, 0, 1, 0, // 5 bytes, not divisible by 4
		}
		_, err := DecodeMessage(uint32(len(input)), input)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid message length")
	})

	t.Run("DecodeSECS2Item_Truncated", func(t *testing.T) {
		// ASCII item with insufficient data
		input := []byte{0x41, 10, 'h', 'e', 'l', 'l', 'o'} // claims 10, has 5
		_, err := DecodeSECS2Item(input)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unexpected end of message")
	})
}

func TestDecode_MaxListDepth(t *testing.T) {
	t.Run("ExceedsMaxDepth", func(t *testing.T) {
		// Build a deeply nested list structure that exceeds MaxListDepth
		// Each nested list: 0x01 (List, 1 length byte), 0x01 (1 item)
		nestedBytes := make([]byte, 0, 10+(MaxListDepth+2)*2)
		nestedBytes = append(nestedBytes, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0) // header

		for i := 0; i < MaxListDepth+2; i++ {
			nestedBytes = append(nestedBytes, 0x01, 0x01) // List with 1 item
		}
		// Add an empty ASCII item at the bottom
		nestedBytes = append(nestedBytes, 0x41, 0x00) // Empty ASCII

		_, err := DecodeMessage(uint32(len(nestedBytes)), nestedBytes)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "nesting depth exceeds maximum")
	})

	t.Run("AtMaxDepth_Succeeds", func(t *testing.T) {
		// Build a list structure exactly at MaxListDepth - should succeed
		nestedBytes := make([]byte, 0, 10+MaxListDepth*2+2)
		nestedBytes = append(nestedBytes, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0) // header

		for i := 0; i < MaxListDepth; i++ {
			nestedBytes = append(nestedBytes, 0x01, 0x01) // List with 1 item
		}
		// Add an empty ASCII item at the bottom
		nestedBytes = append(nestedBytes, 0x41, 0x00) // Empty ASCII

		_, err := DecodeMessage(uint32(len(nestedBytes)), nestedBytes)
		require.NoError(t, err)
	})
}

func TestDecode_ListAllocationCheck(t *testing.T) {
	t.Run("ListClaimsMoreItemsThanBytes", func(t *testing.T) {
		// List claims 1000 items but has very few bytes remaining
		input := []byte{
			0, 0, 0, 0, 0, 0, 0, 0, 0, 0, // header
			0x01, 0x02, 0x03, 0xe8, // List with 3-byte length = 1000 items
			0x41, 0x00, // only one empty ASCII item
		}
		_, err := DecodeMessage(uint32(len(input)), input)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "list claims")
	})
}
