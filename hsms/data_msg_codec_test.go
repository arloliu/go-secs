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
