package hsms_test

import (
	"encoding"
	"testing"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

func TestDataMessageCodec_ImplementsBinaryCodec(t *testing.T) {
	var _ encoding.BinaryMarshaler = (*hsms.DataMessageCodec)(nil)
	var _ encoding.BinaryUnmarshaler = (*hsms.DataMessageCodec)(nil)
}

func TestDataMessageCodec_MarshalUnmarshalRoundTrip(t *testing.T) {
	msg, err := hsms.NewDataMessage(1, 1, false, 0x1234, [4]byte{0, 0, 0, 42}, secs2.NewASCIIItem("PING"))
	require.NoError(t, err)

	codec := &hsms.DataMessageCodec{Message: msg}
	data, err := codec.MarshalBinary()
	require.NoError(t, err)
	require.Equal(t, msg.ToBytes(), data)

	var decoded hsms.DataMessageCodec
	require.NoError(t, decoded.UnmarshalBinary(data))
	require.Equal(t, msg.SessionID(), decoded.Message.SessionID())
	require.Equal(t, msg.SystemBytes(), decoded.Message.SystemBytes())
	require.Equal(t, msg.Stream(), decoded.Message.Stream())
	require.Equal(t, msg.Function(), decoded.Message.Function())
}

func TestDataMessageCodec_MarshalBinary_NilMessage(t *testing.T) {
	var codec hsms.DataMessageCodec
	_, err := codec.MarshalBinary()
	require.Error(t, err)
	require.ErrorIs(t, err, hsms.ErrNilMessage)
}

func TestDataMessageCodec_UnmarshalBinary_NotADataMessage(t *testing.T) {
	ctrl := hsms.NewLinktestReq([4]byte{0, 0, 0, 1})
	var codec hsms.DataMessageCodec
	require.Error(t, codec.UnmarshalBinary(ctrl.ToBytes()))
}

func TestDataMessageCodec_UnmarshalBinary_BadData(t *testing.T) {
	var codec hsms.DataMessageCodec
	require.Error(t, codec.UnmarshalBinary([]byte{0x01}))
}

func TestDataMessage_Codec(t *testing.T) {
	msg, err := hsms.NewDataMessage(1, 1, false, 0x1234, [4]byte{0, 0, 0, 42}, secs2.NewASCIIItem("PING"))
	require.NoError(t, err)

	codec := msg.Codec()
	require.Same(t, msg, codec.Message, "Codec must wrap the exact message, not a copy")

	data, err := codec.MarshalBinary()
	require.NoError(t, err)
	require.Equal(t, msg.ToBytes(), data)
}

func TestDataMessage_Codec_NilReceiver(t *testing.T) {
	var msg *hsms.DataMessage

	codec := msg.Codec()
	_, err := codec.MarshalBinary()
	require.ErrorIs(t, err, hsms.ErrNilMessage, "a nil *DataMessage must still wrap safely and fail at MarshalBinary, not panic in Codec")
}

func TestDataMessageCodec_ReadDelegators(t *testing.T) {
	msg, err := hsms.NewDataMessage(6, 11, true, 42, [4]byte{0, 0, 0, 7}, secs2.NewBinaryItem([]byte{1}))
	require.NoError(t, err)

	c := msg.Codec()

	require.Equal(t, uint8(6), c.Stream())
	require.Equal(t, uint8(11), c.Function())
	require.True(t, c.WaitBit())
	require.Equal(t, uint16(42), c.SessionID())
	require.Equal(t, msg.ID(), c.ID())
	require.Equal(t, msg.SystemBytes(), c.SystemBytes())
	require.Equal(t, msg.HeaderBytes(), c.HeaderBytes())
	require.Equal(t, msg.ToBytes(), c.ToBytes())
	require.Equal(t, hsms.DataMsgType, c.Type())

	item, err := c.Item()
	require.NoError(t, err)
	require.NotNil(t, item)

	require.NoError(t, c.DecodeErr())

	got, ok := c.ToDataMessage()
	require.True(t, ok)
	require.Same(t, msg, got)
}

func TestDataMessageCodec_NilMessageIsSafe(t *testing.T) {
	c := &hsms.DataMessageCodec{Message: nil}

	require.Equal(t, uint8(0), c.Stream())
	require.Equal(t, uint8(0), c.Function())
	require.False(t, c.WaitBit())
	require.Equal(t, uint16(0), c.SessionID())
	require.Equal(t, uint32(0), c.ID())
	require.Equal(t, [4]byte{}, c.SystemBytes())
	require.Equal(t, [10]byte{}, c.HeaderBytes())
	require.Nil(t, c.ToBytes())
	require.Equal(t, hsms.DataMsgType, c.Type())

	_, err := c.Item()
	require.ErrorIs(t, err, hsms.ErrNilMessage)

	require.ErrorIs(t, c.DecodeErr(), hsms.ErrNilMessage)

	_, ok := c.ToDataMessage()
	require.False(t, ok)
}
