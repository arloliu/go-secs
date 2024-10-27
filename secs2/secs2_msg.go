package secs2

// SECS2Message represents a SECS-II message, defining a common interface for various SECS-II and HSMS
// message types.
//
// It provides methods for accessing essential attributes of a SECS-II message, including:
//   - Session ID: A 16-bit unsigned integer identifying the SECS-II session.
//   - Stream and Function Codes: 8-bit unsigned integers specifying the message category and function.
//   - Wait Bit: A boolean value indicating whether a reply is expected.
//   - System Bytes: A 4-byte array serving as the message ID.
//   - Header: The 10-byte header of the SECS-II message.
//   - Item: The SECS-II data item carried by the message.
//
// It also includes a `ToBytes` method to serialize the message into its raw byte representation for
// transmission.
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
