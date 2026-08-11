package hsmstest

import (
	"encoding/binary"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
)

// PaddedDataMessage returns a *hsms.DataMessage whose body is a single well-formed SECS-II item
// followed by padding — extra bytes the outer HSMS length prefix counts as part of the body,
// but that are not part of the item's own encoding.
// Use it to exercise [hsms.DataMessage.TrailingBytes] (see AddDecodeErrorHandler-adjacent tests)
// without hand-forging wire bytes.
//
// It builds a well-formed single-item message, then re-frames it with padding appended to the
// body and the outer length prefix recomputed to match — so DecodeHSMSMessage's length-consistency
// check still passes and the decoded message's first item decodes cleanly (DecodeErr() stays nil),
// exactly the field shape this diagnostic targets (SEMI E5 §10.3.1.3(2) forbids it, but fixed-buffer
// equipment firmware produces it routinely).
func PaddedDataMessage(stream, function uint8, waitBit bool, padding []byte) *hsms.DataMessage {
	item := secs2.NewASCIIItem("padded-body")

	good, err := hsms.NewDataMessage(stream, function, waitBit, 0, [4]byte{0, 0, 0, 1}, item)
	if err != nil {
		panic("hsmstest.PaddedDataMessage: building base message: " + err.Error())
	}

	header := good.HeaderBytes()
	body := good.AppendBodyTo(nil)
	body = append(body, padding...)

	length := uint32(len(header) + len(body)) //nolint:gosec // body length is test-controlled, never near uint32 overflow
	frame := make([]byte, 0, 4+len(header)+len(body))
	frame = binary.BigEndian.AppendUint32(frame, length)
	frame = append(frame, header[:]...)
	frame = append(frame, body...)

	msg, decErr := hsms.DecodeHSMSMessage(frame)
	if decErr != nil {
		panic("hsmstest.PaddedDataMessage: framing unexpectedly failed: " + decErr.Error())
	}
	dm, ok := msg.ToDataMessage()
	if !ok {
		panic("hsmstest.PaddedDataMessage: decoded message is not a data message")
	}

	return dm
}
