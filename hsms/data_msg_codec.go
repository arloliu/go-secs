package hsms

import (
	"encoding"
	"errors"
)

// DataMessageCodec wraps a *DataMessage to satisfy encoding.BinaryMarshaler and
// encoding.BinaryUnmarshaler, for storage layers whose contract requires those standard-library
// interfaces (v1's *DataMessage satisfied them directly; v2's DataMessage stays fully immutable —
// no method mutates it in place — so the codec is a separate wrapper rather than new methods on
// DataMessage itself).
//
// UnmarshalBinary never mutates an existing Message: it decodes data into a fresh *DataMessage and
// repoints the Message field, exactly like assigning the result of DecodeHSMSMessage yourself.
type DataMessageCodec struct {
	Message *DataMessage
}

// Ensure *DataMessageCodec satisfies encoding.BinaryMarshaler and encoding.BinaryUnmarshaler.
var (
	_ encoding.BinaryMarshaler   = (*DataMessageCodec)(nil)
	_ encoding.BinaryUnmarshaler = (*DataMessageCodec)(nil)
)

// MarshalBinary serializes the wrapped message via its ToBytes method. Returns an error if Message
// is nil.
func (c *DataMessageCodec) MarshalBinary() ([]byte, error) {
	if c.Message == nil {
		return nil, ErrNilMessage
	}

	return c.Message.ToBytes(), nil
}

// UnmarshalBinary decodes data — a length-prefixed HSMS frame, see DecodeHSMSMessage — and stores
// the result in c.Message. Returns an error if data fails to decode, or decodes to a control message
// rather than a data message.
func (c *DataMessageCodec) UnmarshalBinary(data []byte) error {
	msg, err := DecodeHSMSMessage(data)
	if err != nil {
		return err
	}

	dm, ok := msg.ToDataMessage()
	if !ok {
		return errors.New("hsms: DataMessageCodec.UnmarshalBinary: decoded message is not a data message")
	}

	c.Message = dm

	return nil
}
