package secs2

// SECS2Message is the transport-agnostic view of a SECS-II message (SEMI E5). It exposes only the
// attributes intrinsic to the message itself and leaves any transport framing to the concrete
// message types that carry it.
//
// The interface consists of:
//   - StreamCode: the 8-bit stream code identifying the message category.
//   - FunctionCode: the 8-bit function code identifying the function within the stream.
//   - WaitBit: whether a reply is expected (the W-bit).
//   - Item: the SECS-II data item carried by the message.
type SECS2Message interface {
	// StreamCode returns the stream code for the SECS-II message.
	StreamCode() uint8

	// FunctionCode returns the function code for the SECS-II message.
	FunctionCode() uint8

	// WaitBit() returns the boolean representation of W-Bit for the SECS-II message.
	WaitBit() bool

	// Item returns the SECS-II data item.
	Item() Item
}
