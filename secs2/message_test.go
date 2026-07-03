package secs2

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNewMessage verifies NewMessage builds a valid SECS2Message whose body round-trips through
// Decode.
func TestNewMessage(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	item := A("hello")
	msg := NewMessage(1, 13, true, item)

	require.Equal(uint8(1), msg.StreamCode())
	require.Equal(uint8(13), msg.FunctionCode())
	require.True(msg.WaitBit())
	require.Same(item, msg.Item())

	decoded, err := Decode(msg.Item().ToBytes())
	require.NoError(err)
	got, err := decoded.ToASCII()
	require.NoError(err)
	require.Equal("hello", got)
}

// TestNewMessageStreamMask verifies StreamCode masks off the high bit (the SECS-II reverse-bit
// position) and WaitBit reflects replyExpected.
func TestNewMessageStreamMask(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	msg := NewMessage(0x81, 2, false, A("x"))
	require.Equal(uint8(1), msg.StreamCode())
	require.Equal(uint8(2), msg.FunctionCode())
	require.False(msg.WaitBit())
}

// TestNewMessageNilItem verifies a nil item becomes an empty-bodied item and never panics.
func TestNewMessageNilItem(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	msg := NewMessage(1, 1, false, nil)
	require.NotNil(msg.Item())
	require.True(msg.Item().IsEmpty())

	body := msg.Item().ToBytes()
	require.Empty(body)

	decoded, err := Decode(body)
	require.NoError(err)
	require.True(decoded.IsEmpty())
}
