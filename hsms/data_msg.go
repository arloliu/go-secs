package hsms

import (
	"encoding/binary"
	"fmt"
	"strings"

	"github.com/arloliu/go-secs/internal/util"
	"github.com/arloliu/go-secs/secs2"
)

// WaitBit byte constants representing if wait-bit is set.
const (
	WaitBitFalse = uint8(0)
	WaitBitTrue  = uint8(1)
)

// DataMessage represents a HSMS data message.
//
// It implements the HSMSMessage and secs2.SECS2Message interfaces.
type DataMessage struct {
	name        string
	dataItem    secs2.Item
	systemBytes []byte
	sessionID   uint16
	stream      byte
	function    byte
	waitBit     uint8
}

// ensure DataMessage implements hsms.HSMSMessage and secs2.SECS2Message interfaces.
var (
	_ HSMSMessage        = (*DataMessage)(nil)
	_ secs2.SECS2Message = (*DataMessage)(nil)
)

// NewDataMessage creates a new SECS-II message.
//
// The message can't be converted to HSMS format until session id, system
// bytes and wait bit is set (if it had value of optional).
//
// # Input argument specifications
//
// stream is a stream code of this message and should be in range of [0, 127].
//
// function is a function code of this message and should be in range of [0, 255].
//
// replyExpected specify if the primary message should excpect a reply message.
// it sets W-Bit to 1 if true, 0 else.
// replyExpected cannot true when the function code is a even number(reply message).
//
// sessionID is the session id in HSMS message, it should be in range of [0, 65535].
//
// systemBytes should have 4 bytes.
//
// dataItem is the contents of this message.
func NewDataMessage(stream byte, function byte, replyExpected bool, sessionID uint16, systemBytes []byte, dataItem secs2.Item) (*DataMessage, error) {
	msg := getDataMessage(stream, function, replyExpected, sessionID, systemBytes, dataItem)
	if err := msg.sanityCheck(); err != nil {
		putDataMessage(msg)
		return nil, err
	}

	return msg, nil
}

// Type returns HSMS message type.
//
// This method implements the HSMSMessage.Type() interface.
func (msg *DataMessage) Type() int {
	return DataMsgType
}

// SessionID returns the session id of the SECS-II message.
//
// This method implements the HSMSMessage.SessionID() interface.
// If the session id was not set, it will return -1.
func (msg *DataMessage) SessionID() uint16 {
	return msg.sessionID
}

// SetSessionID sets the session id of the SECS-II message.
//
// This method implements the HSMSMessage.SetSessionID() interface.
func (msg *DataMessage) SetSessionID(sessionID uint16) {
	msg.sessionID = sessionID
}

// ID returns a numeric representation of the system bytes (message ID).
//
// This method implements the HSMSMessage.ID() interface.
func (msg *DataMessage) ID() uint32 {
	return binary.BigEndian.Uint32(msg.systemBytes)
}

// SystemBytes returns the system bytes of the SECS-II message.
// If the system bytes was not set, it will return []byte{0, 0, 0, 0}.
//
// This method implements the HSMSMessage.SystemBytes() interface.
func (msg *DataMessage) SystemBytes() []byte {
	return msg.systemBytes[:4]
}

// SetSystemBytes sets system bytes to the data message.
//
// It will return error if the systemBytes is not 4 bytes.
//
// This method implements the HSMSMessage.SetSystemBytes() interface.
func (msg *DataMessage) SetSystemBytes(systemBytes []byte) error {
	if len(systemBytes) != 4 {
		return ErrInvalidSystemBytes
	}

	msg.systemBytes = util.CloneSlice(systemBytes, 4)

	return nil
}

// Header returns the 10-byte HSMS message header.
//
// This method implements the HSMSMessage.Header() interface.
func (msg *DataMessage) Header() []byte {
	header := make([]byte, 10)
	msg.generateHeader(header)
	return header
}

// SetHeader sets the header of the SECS-II message.
//
// It will return error if the header is invalid.
//
// This method implements the HSMSMessage.SetHeader() interface.
func (msg *DataMessage) SetHeader(header []byte) error {
	if len(header) != 10 {
		return ErrInvalidHeaderLength
	}

	if header[4] != 0 {
		return ErrInvalidPType
	}

	if header[5] != DataMsgType {
		return ErrInvalidDataMsgSType
	}

	msg.sessionID = binary.BigEndian.Uint16(header[:2])
	msg.stream = header[2] & 0x7F
	msg.function = header[3]
	msg.systemBytes = util.CloneSlice(header[6:], 4)
	msg.waitBit = header[2] >> 7

	return nil
}

// Name returns the optional message name of the SECS-II message.
func (msg *DataMessage) Name() string {
	return msg.name
}

// SetName sets the optional message name of the SECS-II message.
func (msg *DataMessage) SetName(name string) {
	msg.name = name
}

// StreamCode returns the stream code of the SECS-II message.
//
// This method implements the secs2.SECS2Message.StreamCode() interface.
func (msg *DataMessage) StreamCode() uint8 {
	return msg.stream
}

// FunctionCode returns the function code of the SECS-II message.
//
// This method implements the secs2.SECS2Message.FunctionCode() interface.
func (msg *DataMessage) FunctionCode() uint8 {
	return msg.function
}

// WaitBit returnes the boolean representation to indicates WBit is set
//
// This method implements the secs2.SECS2Message.WaitBit() interface.
func (msg *DataMessage) WaitBit() bool {
	return msg.waitBit == WaitBitTrue
}

// Item returnes the SECS-II data item in DataMessage.
//
// This method implements the secs2.SECS2Message.Item() interface.
func (msg *DataMessage) Item() secs2.Item {
	return msg.dataItem
}

// SMLHeader returns the message header of the SECS-II message, e.g. "S6F11 W".
func (msg *DataMessage) SMLHeader() string {
	quote := StreamFunctionQuote()
	header := fmt.Sprintf("%sS%dF%d%s", quote, msg.stream, msg.function, quote)

	if msg.waitBit == WaitBitTrue {
		header += " W"
	}

	return header
}

// ToBytes returns the HSMS byte representation of the SECS-II message.
//
// It will return empty byte slice if the message can't be represented as HSMS format,
// i.e. wait bit is optional.
//
// This method implements the HSMSMessage.ToBytes() interface.
func (msg *DataMessage) ToBytes() []byte {
	var itemBytes []byte
	if msg.dataItem != nil {
		itemBytes = msg.dataItem.ToBytes()
	}
	totalBytes := len(itemBytes) + 14 // dataItem  + 4 length  + 10 header

	result := make([]byte, 14, totalBytes)
	// Message length bytes, MSB first
	// message length = total length - 4 length bytes
	msgLength := (totalBytes - 4)
	result[0] = byte(msgLength >> 24)
	result[1] = byte(msgLength >> 16)
	result[2] = byte(msgLength >> 8)
	result[3] = byte(msgLength)

	msg.generateHeader(result[4:14])

	// Message text
	result = append(result, itemBytes...)

	return result
}

// IsControlMessage returns false, indicating that a DataMessage is not a control message.
func (msg *DataMessage) IsControlMessage() bool {
	return false
}

// ToControlMessage attempts to convert the message to an HSMS control message.
// Since a DataMessage cannot be converted to a ControlMessage, it always returns nil and false.
func (msg *DataMessage) ToControlMessage() (*ControlMessage, bool) {
	return nil, false
}

// IsDataMessage returns true, indicating that a DataMessage is a data message.
func (msg *DataMessage) IsDataMessage() bool {
	return true
}

// ToDataMessage converts the message to an HSMS data message.
// Since the message is already a DataMessage, it returns a pointer to itself and true.
func (msg *DataMessage) ToDataMessage() (*DataMessage, bool) {
	return msg, true
}

// ToSML returns SML representation of data message.
func (msg *DataMessage) ToSML() string {
	if msg.dataItem == nil {
		if msg.name != "" {
			return msg.name + ":" + msg.SMLHeader() + "\n."
		}

		return msg.SMLHeader() + "\n."
	}

	if _, ok := msg.dataItem.(*secs2.EmptyItem); ok {
		if msg.name != "" {
			return msg.name + ":" + msg.SMLHeader() + "\n."
		}

		return msg.SMLHeader() + "\n."
	}
	var sb strings.Builder

	header := msg.SMLHeader()
	msgBody := msg.dataItem.ToSML()

	sb.Grow(len(msg.name) + len(header) + len(msgBody) + 2)

	if msg.name != "" {
		sb.WriteString(msg.name)
		sb.WriteString(":")
	}
	sb.WriteString(header)
	sb.WriteString("\n")
	sb.WriteString(msgBody)
	sb.WriteString("\n.")

	return sb.String()
}

// Free releases the message and its associated resources back to the pool.
// After calling Free, the message should not be accessed again.
func (msg *DataMessage) Free() {
	if usePool {
		item := msg.Item()
		if item != nil {
			item.Free()
		}
		putDataMessage(msg)
	}
}

// Clone returns a duplicated Message
func (msg *DataMessage) Clone() HSMSMessage {
	cloned := &DataMessage{
		name:        msg.name,
		stream:      msg.stream,
		function:    msg.function,
		waitBit:     msg.waitBit,
		sessionID:   msg.sessionID,
		systemBytes: util.CloneSlice(msg.systemBytes, 4),
	}

	if msg.dataItem == nil {
		cloned.dataItem = secs2.NewEmptyItem()
	} else {
		cloned.dataItem = msg.dataItem.Clone()
	}

	return cloned
}

func (msg *DataMessage) generateHeader(header []byte) {
	// Header byte 0-1: session(device) ID
	header[0] = byte(msg.sessionID >> 8)
	header[1] = byte(msg.sessionID)

	// Header byte 2-3: wait bit + stream code, function code
	header[2] = msg.stream
	if msg.WaitBit() {
		header[2] += 0b_1000_0000
	}
	header[3] = msg.function

	// Header byte 4-5: PType, SType, should set to zero for data message
	header[4] = 0
	header[5] = 0
	// Header byte 6-9: system bytes
	copy(header[6:], msg.systemBytes[:4])
}

func (msg *DataMessage) sanityCheck() error {
	if err := msg.dataItem.Error(); err != nil {
		return err
	}

	if msg.stream >= 128 {
		return ErrInvalidStreamCode
	}

	if msg.waitBit == WaitBitTrue && msg.function%2 == 0 {
		return ErrInvalidRspMsg
	}

	if msg.waitBit > 1 {
		return ErrInvalidWaitBit
	}

	if len(msg.systemBytes) != 4 {
		return ErrInvalidSystemBytes
	}

	return nil
}
