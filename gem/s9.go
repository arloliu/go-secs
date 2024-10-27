package gem

import "github.com/arloliu/go-secs/secs2"

// s9fx is a helper function that creates a GEMMessage with the specified function code (f)
// within stream 9 (S9Fx). It sets the W-bit (wait bit) to false and includes an empty data item.
func s9fx(f uint8) secs2.SECS2Message {
	return &GEMMessage{s: 9, f: f, w: false, item: secs2.NewEmptyItem()}
}

// S9F1 creates an S9F1 (Unrecognized Device ID) message.
//
// SEMI E30 Description: This message is sent when an unrecognized Device ID is received in a message.
func S9F1() secs2.SECS2Message { return s9fx(1) }

// S9F3 creates an S9F3 (Unrecognized Stream Type) message.
//
// SEMI E30 Description: This message is sent when an unrecognized Stream Type is received in a message.
func S9F3() secs2.SECS2Message { return s9fx(3) }

// S9F5 creates an S9F5 (Unrecognized Function Type) message.
//
// SEMI E30 Description: This message is sent when an unrecognized Function Type is received in a message.
func S9F5() secs2.SECS2Message { return s9fx(5) }

// S9F7 creates an S9F7 (Illegal Data) message.
//
// SEMI E30 Description: This message is sent when the data is illegal for the Function Type.
func S9F7() secs2.SECS2Message { return s9fx(7) }

// S9F9 creates an S9F9 (Transaction Timeout) message.
//
// SEMI E30 Description: This message is sent when the equipment fails to process a message
// from the host within the timeout period.
func S9F9() secs2.SECS2Message { return s9fx(9) }

// S9F11 creates an S9F11 (Data Too Long) message.
//
// SEMI E30 Description: This message is sent when the length of the data in a message exceeds
// the maximum message length that the equipment can process.
func S9F11() secs2.SECS2Message { return s9fx(11) }

// S9F13 creates an S9F13 (Conversation Timeout) message.
//
// SEMI E30 Description: This message is sent when the host fails to respond to a message
// within the timeout period.
func S9F13() secs2.SECS2Message { return s9fx(13) }
