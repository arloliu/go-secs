package secs2

// Message is the minimal concrete implementation of SECS2Message: a stream/function code pair, a
// wait bit, and a SECS-II data item. It carries no transport state (no session, system bytes, or
// header); those belong to the transport message types that wrap it. Once constructed it is
// immutable and safe for concurrent use.
type Message struct {
	item     Item
	stream   uint8
	function uint8
	w        bool
}

// ensure Message implements the SECS2Message interface.
var _ SECS2Message = (*Message)(nil)

// NewMessage creates a SECS2Message from the given stream and function codes, wait bit, and data
// item.
//
// When replyExpected is true the message's wait bit (W-bit) is set, indicating that a reply is
// expected from the receiver. If item is nil it is replaced with NewEmptyItem, so the returned
// message always carries a valid, empty-bodied item.
//
// Parameters:
//   - stream: the SECS-II stream code.
//   - function: the SECS-II function code.
//   - replyExpected: whether a reply is expected (sets the W-bit).
//   - item: the data item payload.
//
// Returns:
//   - SECS2Message: the constructed message.
func NewMessage(stream, function byte, replyExpected bool, item Item) SECS2Message {
	if item == nil {
		item = NewEmptyItem()
	}

	return &Message{stream: stream, function: function, w: replyExpected, item: item}
}

// StreamCode returns the stream code for the SECS-II message. The high bit, which the SECS-II
// header reuses for the reverse-bit flag, is masked off.
func (msg *Message) StreamCode() uint8 { return msg.stream & 0x7F }

// FunctionCode returns the function code for the SECS-II message.
func (msg *Message) FunctionCode() uint8 { return msg.function }

// WaitBit reports whether a reply is expected (the W-bit) for the SECS-II message.
func (msg *Message) WaitBit() bool { return msg.w }

// Item returns the SECS-II data item carried by the message.
func (msg *Message) Item() Item { return msg.item }
