package gem

import "github.com/arloliu/go-secs/v2/secs2"

// Abort creates the SxF0 Abort Transaction message of stream (SEMI E5 §10.4.1):
// header only, with the W-bit clear.
// Stream is 1 through 127;
// stream 0 is never used (E5 §10.4).
//
// Function 0 exists in every stream and closes a transaction in lieu of the expected reply,
// so the originator need not wait for its reply timer to expire.
// As a reply it must carry the primary's System Bytes, which a [secs2.SECS2Message] does not hold.
// A caller that frames its own messages, such as a relay,
// builds the frame from this message with the primary's System Bytes,
// for instance with hsms.NewDataMessage, and sends it with ForwardDataMessage.
// A connection answering a primary it received calls hsms.ReplyAbort with that primary instead:
// it does not take this message, but builds the same SxF0 itself from the primary's stream and System Bytes.
// Connection.SendSECS2Message refuses this message, since it refuses function 0.
// When to refuse a message is described under "Refusing a message" in the package documentation.
func Abort(stream byte) secs2.SECS2Message {
	return secs2.NewMessage(stream, 0, false, nil)
}
