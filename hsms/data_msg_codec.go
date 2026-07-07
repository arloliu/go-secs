package hsms

import (
	"encoding"
	"errors"

	"github.com/arloliu/go-secs/v2/secs2"
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

// Codec wraps msg in a *DataMessageCodec, for storing msg in a slot whose contract requires
// encoding.BinaryMarshaler / encoding.BinaryUnmarshaler (see DataMessageCodec) — e.g. a
// transport-agnostic message envelope in application code. Returns *DataMessageCodec, not a
// value: UnmarshalBinary has a pointer receiver (it mutates c.Message in place), and Go does not
// promote pointer-receiver methods into a value's method set for interface satisfaction — a
// DataMessageCodec value assigned into an encoding.BinaryUnmarshaler-typed slot would silently
// fail to satisfy it. msg may be nil; a subsequent MarshalBinary on the result then returns
// ErrNilMessage exactly as calling it on a zero-value DataMessageCodec already does.
func (msg *DataMessage) Codec() *DataMessageCodec {
	return &DataMessageCodec{Message: msg}
}

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

// The following read-only delegators let a *DataMessageCodec be inspected without
// unwrapping Message, so a consumer that only reads never falls through a type switch
// to "unknown". Scalar/byte reads on a nil Message return the zero value (panic-free);
// Item and DecodeErr report ErrNilMessage rather than a misleading nil.

// Stream returns the wrapped message's stream, or 0 if Message is nil.
func (c *DataMessageCodec) Stream() uint8 {
	if c.Message == nil {
		return 0
	}
	return c.Message.Stream()
}

// Function returns the wrapped message's function, or 0 if Message is nil.
func (c *DataMessageCodec) Function() uint8 {
	if c.Message == nil {
		return 0
	}
	return c.Message.Function()
}

// WaitBit returns the wrapped message's W-bit, or false if Message is nil.
func (c *DataMessageCodec) WaitBit() bool {
	if c.Message == nil {
		return false
	}
	return c.Message.WaitBit()
}

// SessionID returns the wrapped message's session ID, or 0 if Message is nil.
func (c *DataMessageCodec) SessionID() uint16 {
	if c.Message == nil {
		return 0
	}
	return c.Message.SessionID()
}

// ID returns the wrapped message's ID (system bytes), or 0 if Message is nil.
func (c *DataMessageCodec) ID() uint32 {
	if c.Message == nil {
		return 0
	}
	return c.Message.ID()
}

// SystemBytes returns the wrapped message's System Bytes, or the zero array if Message is nil.
func (c *DataMessageCodec) SystemBytes() [4]byte {
	if c.Message == nil {
		return [4]byte{}
	}
	return c.Message.SystemBytes()
}

// HeaderBytes returns the wrapped message's 10-byte header, or the zero array if Message is nil.
func (c *DataMessageCodec) HeaderBytes() [10]byte {
	if c.Message == nil {
		return [10]byte{}
	}
	return c.Message.HeaderBytes()
}

// ToBytes returns the wrapped message's wire bytes, or nil if Message is nil.
func (c *DataMessageCodec) ToBytes() []byte {
	if c.Message == nil {
		return nil
	}
	return c.Message.ToBytes()
}

// Type returns the wrapped message's HSMS message type, or DataMsgType if Message is nil.
func (c *DataMessageCodec) Type() MsgType {
	if c.Message == nil {
		return DataMsgType
	}
	return c.Message.Type()
}

// DecodeErr returns the wrapped message's deferred decode error, or ErrNilMessage if
// Message is nil.
func (c *DataMessageCodec) DecodeErr() error {
	if c.Message == nil {
		return ErrNilMessage
	}
	return c.Message.DecodeErr()
}

// Item returns the wrapped message's decoded item, or ErrNilMessage if Message is nil
// (never (nil, nil), which would hide an absent message).
func (c *DataMessageCodec) Item() (secs2.Item, error) {
	if c.Message == nil {
		return nil, ErrNilMessage
	}
	return c.Message.Item()
}

// ToDataMessage returns the wrapped message and true, or (nil, false) if Message is nil.
// This is the zero-safe unwrap probe.
func (c *DataMessageCodec) ToDataMessage() (*DataMessage, bool) {
	if c.Message == nil {
		return nil, false
	}
	return c.Message, true
}
