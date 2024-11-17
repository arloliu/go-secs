package hsms

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestControlMessage_Select(t *testing.T) {
	require := require.New(t)

	systemBytes := []byte{0x12, 0x34, 0x56, 0x78}
	selectReq := NewSelectReq(123, systemBytes)
	require.Equal(uint16(123), selectReq.SessionID())
	require.Equal(systemBytes, selectReq.SystemBytes())
	require.Equal(SelectReqType, selectReq.Type())
	require.True(selectReq.WaitBit())
	require.Equal([]byte{0x0, 0x0, 0x0, 0xa, 0x0, 0x7b, 0x0, 0x0, 0x0, 0x1, 0x12, 0x34, 0x56, 0x78}, selectReq.ToBytes())

	selectRsp, err := NewSelectRsp(selectReq, SelectStatusSuccess)
	require.NoError(err)
	require.Equal(uint16(123), selectRsp.SessionID())
	require.Equal(systemBytes, selectRsp.SystemBytes())
	require.Equal(SelectRspType, selectRsp.Type())
	require.False(selectRsp.WaitBit())
	require.Equal([]byte{0x0, 0x0, 0x0, 0xa, 0x0, 0x7b, 0x0, 0x0, 0x0, 0x2, 0x12, 0x34, 0x56, 0x78}, selectRsp.ToBytes())

	// Test with invalid selectReq
	invalidSelectReq := NewDeselectReq(123, systemBytes)
	_, err = NewSelectRsp(invalidSelectReq, SelectStatusSuccess)
	require.Error(err)
}

func TestControlMessage_Set(t *testing.T) {
	require := require.New(t)

	// create a new ControlMessage with initial values
	systemBytes := []byte{0x01, 0x02, 0x03, 0x04}
	msg := NewSelectReq(0, systemBytes)
	require.NotNil(msg)

	// verify initial values
	require.Equal(uint32(0x01020304), msg.ID())
	require.Equal(uint16(0), msg.SessionID())

	// set and verify SessionID
	msg.SetSessionID(123)
	require.Equal(uint16(123), msg.SessionID())

	// set and verify SystemBytes
	err := msg.SetSystemBytes([]byte{0x12, 0x34, 0x56, 0x78})
	require.NoError(err)
	require.Equal(SelectReqType, msg.Type())
	require.Equal([]byte{0x12, 0x34, 0x56, 0x78}, msg.SystemBytes())
	require.Equal([]byte{0x0, 0x0, 0x0, 0xa, 0x0, 0x7b, 0x0, 0x0, 0x0, 0x1, 0x12, 0x34, 0x56, 0x78}, msg.ToBytes())

	// attempt to set an invalid header and expect an error
	err = msg.SetHeader([]byte{0})
	require.ErrorIs(err, ErrInvalidHeaderLength)

	// set a valid header and verify the values
	err = msg.SetHeader([]byte{0x0, 0x7b, 0x0, 0x0, 0x0, 0x1, 0x12, 0x34, 0x56, 0x78})
	require.NoError(err)
	require.Equal([]byte{0x12, 0x34, 0x56, 0x78}, msg.SystemBytes())
	require.Equal(uint16(0x7b), msg.SessionID())
	require.Equal(SelectReqType, msg.Type())

	// clone the message and verify the cloned values
	cloned := msg.Clone()
	clonedDataMsg, ok := cloned.(*ControlMessage)
	require.True(ok)

	require.Equal(msg.ID(), clonedDataMsg.ID())
	require.Equal(msg.SessionID(), clonedDataMsg.SessionID())
	require.Equal(msg.SystemBytes(), clonedDataMsg.SystemBytes())
}
