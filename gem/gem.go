package gem

import "github.com/arloliu/go-secs/secs2"

// GEMMessage represents a message conforming to the SEMI E30 standard, also known as the GEM
// (Generic Equipment Model) standard. It implements the secs2.SECS2Message interface.
//
// GEMMessage encapsulates the core components of a GEM message:
//   - s: The stream number, identifying the message category.
//   - f: The function number, specifying the specific function within the stream.
//   - w: The wait bit (W-bit), indicating whether a reply is expected from the receiver.
//   - item: The SECS-II data item carried by the message, containing the actual data payload.
type GEMMessage struct {
	item secs2.Item
	s    uint8
	f    uint8
	w    bool
}

// ensure GEMMessage implements secs2.SECS2Message interface.
var _ secs2.SECS2Message = &GEMMessage{}

// NewMessage creates a new GEMMessage with the specified stream code (s), function code (f),
// wait bit (w), and SECS-II data item (item).
//
// The wait bit (w) should be set to true if a reply is expected from the receiver, and false otherwise.
func NewMessage(s uint8, f uint8, w bool, item secs2.Item) *GEMMessage {
	return &GEMMessage{s: s, f: f, w: w, item: item}
}

// StreamCode returns the stream code for the SECS-II message.
func (msg *GEMMessage) StreamCode() uint8 { return msg.s & 0x7F }

// FunctionCode returns the function code for the SECS-II message.
func (msg *GEMMessage) FunctionCode() uint8 { return msg.f }

// WaitBit() returns the boolean representation of W-Bit for the SECS-II message.
func (msg *GEMMessage) WaitBit() bool { return msg.w }

// Item returns the SECS-II data item.
func (msg *GEMMessage) Item() secs2.Item { return msg.item }
