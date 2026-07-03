package gem

import "github.com/arloliu/go-secs/v2/secs2"

// S9F1 creates an S9F1 (Unrecognized Device ID) message.
//
// Per SEMI E5 §10.13 the body carries MHEAD, the 10-byte header of the offending message.
// Obtain those bytes from the offending message via HeaderBytes().
//
// The message is sent with W=0 (no reply expected).
func S9F1(mhead [10]byte) secs2.SECS2Message {
	return secs2.NewMessage(9, 1, false, secs2.B(mhead[:]))
}

// S9F3 creates an S9F3 (Unrecognized Stream Type) message.
//
// Per SEMI E5 §10.13 the body carries MHEAD, the 10-byte header of the offending message.
// Obtain those bytes from the offending message via HeaderBytes().
//
// The message is sent with W=0 (no reply expected).
func S9F3(mhead [10]byte) secs2.SECS2Message {
	return secs2.NewMessage(9, 3, false, secs2.B(mhead[:]))
}

// S9F5 creates an S9F5 (Unrecognized Function Type) message.
//
// Per SEMI E5 §10.13 the body carries MHEAD, the 10-byte header of the offending message.
// Obtain those bytes from the offending message via HeaderBytes().
//
// The message is sent with W=0 (no reply expected).
func S9F5(mhead [10]byte) secs2.SECS2Message {
	return secs2.NewMessage(9, 5, false, secs2.B(mhead[:]))
}

// S9F7 creates an S9F7 (Illegal Data) message.
//
// Per SEMI E5 §10.13 the body carries MHEAD, the 10-byte header of the offending message.
// Obtain those bytes from the offending message via HeaderBytes().
//
// The message is sent with W=0 (no reply expected).
func S9F7(mhead [10]byte) secs2.SECS2Message {
	return secs2.NewMessage(9, 7, false, secs2.B(mhead[:]))
}

// S9F9 creates an S9F9 (Transaction Timeout) message.
//
// Per SEMI E5 §10.13 the body carries SHEAD, the 10-byte header of the timed-out message.
// Obtain those bytes from the timed-out message via HeaderBytes().
//
// The message is sent with W=0 (no reply expected).
func S9F9(shead [10]byte) secs2.SECS2Message {
	return secs2.NewMessage(9, 9, false, secs2.B(shead[:]))
}

// S9F11 creates an S9F11 (Data Too Long) message.
//
// Per SEMI E5 §10.13 the body carries MHEAD, the 10-byte header of the offending message.
// Obtain those bytes from the offending message via HeaderBytes().
//
// The message is sent with W=0 (no reply expected).
func S9F11(mhead [10]byte) secs2.SECS2Message {
	return secs2.NewMessage(9, 11, false, secs2.B(mhead[:]))
}

// S9F13 creates an S9F13 (Conversation Timeout) message.
//
// Per SEMI E5 §10.13 the body is L[2]{ A[mexp], edid } where mexp is the expected message
// name (e.g. "S1F2") and edid is an equipment-defined identifier item supplied by the caller.
//
// The message is sent with W=0 (no reply expected).
func S9F13(mexp string, edid secs2.Item) secs2.SECS2Message {
	return secs2.NewMessage(9, 13, false, secs2.L(secs2.A(mexp), edid))
}
