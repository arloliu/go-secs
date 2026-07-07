package hsmstest

import (
	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
)

// MalformedDataMessage returns a *hsms.DataMessage whose HSMS framing is valid — so
// its header accessors (Stream/Function/WaitBit/SessionID/ID) succeed — but whose
// SECS-II body fails to decode lazily: msg.DecodeErr() != nil and msg.Item() returns
// that error. Use it to exercise decode-error handling (see AddDecodeErrorHandler)
// without hand-forging wire bytes.
//
// It builds a well-formed single-item message, then corrupts one body byte so the
// item's declared length overruns the frame while the outer length prefix stays
// intact; DecodeHSMSMessage still yields a (lazy) *DataMessage, and the deferred body
// decode fails when first forced.
func MalformedDataMessage(stream, function uint8, waitBit bool) *hsms.DataMessage {
	item := secs2.NewBinaryItem([]byte{0x00})

	good, err := hsms.NewDataMessage(stream, function, waitBit, 0, [4]byte{0, 0, 0, 1}, item)
	if err != nil {
		panic("hsmstest.MalformedDataMessage: building base message: " + err.Error())
	}

	raw := good.ToBytes() // length-prefixed HSMS frame: [4]len | [10]header | body

	const bodyLenByteOffset = 4 + 10 + 1 // len prefix + header + format byte
	if len(raw) <= bodyLenByteOffset {
		panic("hsmstest.MalformedDataMessage: base frame shorter than expected")
	}
	raw[bodyLenByteOffset] = 0xFF // claim a 255-byte item where only 1 byte follows

	msg, decErr := hsms.DecodeHSMSMessage(raw)
	if decErr != nil {
		panic("hsmstest.MalformedDataMessage: framing unexpectedly failed: " + decErr.Error())
	}
	dm, ok := msg.ToDataMessage()
	if !ok {
		panic("hsmstest.MalformedDataMessage: decoded message is not a data message")
	}

	return dm
}
