package hsms

import (
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/arloliu/go-secs/secs2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeMessage_DataMessage(t *testing.T) {
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
			description: `S126F254 <BOOLEAN[2] T F>`,
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
			expectedToSML:        "S126F254\n<BOOLEAN[2] T F>\n.",
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
			expectedToSML:        "S127F255 W\n<F4[3] -1 0 3.141592>\n.",
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
	}
}

func TestDecodeMessage_ControlMessage(t *testing.T) {
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

func BenchmarkDecodeMessage_DataMessage_10_UsePool(b *testing.B) {
	UsePool(true)
	benchmarkDecodeDataMessage(b, 10)
}

func BenchmarkDecodeMessage_DataMessage_10_NoPool(b *testing.B) {
	UsePool(false)
	benchmarkDecodeDataMessage(b, 10)
}

func BenchmarkDecodeMessage_DataMessage_100_UsePool(b *testing.B) {
	UsePool(true)
	benchmarkDecodeDataMessage(b, 100)
}

func BenchmarkDecodeMessage_DataMessage_100_NoPool(b *testing.B) {
	UsePool(false)
	benchmarkDecodeDataMessage(b, 100)
}

func BenchmarkDecodeMessage_DataMessage_1000_UsePool(b *testing.B) {
	UsePool(true)
	benchmarkDecodeDataMessage(b, 1000)
}

func BenchmarkDecodeMessage_DataMessage_1000_NoPool(b *testing.B) {
	UsePool(false)
	benchmarkDecodeDataMessage(b, 1000)
}

func benchmarkDecodeDataMessage(b *testing.B, testSize int) {
	intItems := make([]secs2.Item, 0, testSize)
	for i := 0; i < testSize; i++ {
		intItems = append(intItems, secs2.I8(int64(i)))
	}

	floatItems := make([]secs2.Item, 0, testSize)
	for i := 0; i < testSize; i++ {
		floatItems = append(floatItems, secs2.F8(i))
	}

	item := secs2.L(
		secs2.L(intItems...),
		secs2.L(floatItems...),
	)
	msg, err := NewDataMessage(1, 1, true, 1234, GenerateMsgSystemBytes(), item)
	if err != nil {
		b.Logf("error:%v", err)
		b.FailNow()
	}

	input := msg.ToBytes()[4:]

	decodedMsg, err := DecodeMessage(uint32(len(input)), input)
	if err != nil {
		b.FailNow()
	}
	decodedMsg.Free()

	b.ResetTimer()
	for i := 0; i <= b.N; i++ {
		msg, err := DecodeMessage(uint32(len(input)), input)
		if err != nil {
			b.FailNow()
		}
		_ = msg
		msg.Free()
	}
	b.StopTimer()
}

// decodeMessageLength decodes the message length from the first 4 bytes of an HSMS message.
// The message length is encoded as a 32-bit unsigned integer in big-endian order.
func decodeMessageLength(input []byte) uint32 {
	return binary.BigEndian.Uint32(input[:4])
}
