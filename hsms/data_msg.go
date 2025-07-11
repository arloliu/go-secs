package hsms

import (
	"encoding/binary"
	"errors"
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
	dataItem    secs2.Item
	systemBytes []byte
	err         error
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

// NewDataMessageFromRawItem creates a new SECS-II message from SECS-II raw binary data.
//
// This function is similar to NewDataMessage, but it accepts raw binary data
// for the SECS-II message instead of SECS-II item.
//
// Please refer to NewDataMessage for more information.
func NewDataMessageFromRawItem(stream byte, function byte, replyExpected bool, sessionID uint16, systemBytes []byte, data []byte) (*DataMessage, error) {
	item, err := DecodeSECS2Item(data)
	if err != nil {
		return nil, err
	}

	return NewDataMessage(stream, function, replyExpected, sessionID, systemBytes, item)
}

// NewErrorDataMessage creates a new SECS-II message with an error.
//
// This function is used to create a data message that indicates an error condition.
// It sets the wait bit to false, indicating that no reply is expected.
// The session ID and system bytes are provided, and the error is set in the message.
// The function code should be set to a value that indicates an error condition,
// typically an odd number to indicate a primary message.
func NewErrorDataMessage(stream byte, function byte, sessionID uint16, systemBytes []byte, err error) *DataMessage {
	msg := getDataMessage(stream, function, false, sessionID, systemBytes, secs2.NewEmptyItem())
	msg.err = err

	// set the wait bit to false for error messages
	msg.waitBit = WaitBitFalse

	return msg
}

// Type returns the message type of the data message.
//
// It implements the Type method of the HSMSMessage interface.
func (msg *DataMessage) Type() int {
	return DataMsgType
}

// SessionID returns the session id of the data message.
// If the session id was not set, it will return -1.
//
// It implements the SessionID method of the HSMSMessage interface.
func (msg *DataMessage) SessionID() uint16 {
	return msg.sessionID
}

// SetSessionID sets the session id of the data message.
//
// It implements the SetSessionID method of the HSMSMessage interface.
func (msg *DataMessage) SetSessionID(sessionID uint16) {
	msg.sessionID = sessionID
}

// ID returns a numeric representation of the system bytes (message ID).
//
// It implements the ID method of the HSMSMessage interface.
func (msg *DataMessage) ID() uint32 {
	return binary.BigEndian.Uint32(msg.systemBytes)
}

// SetID sets the system bytes of the data message.
//
// It implements the SetID method of the HSMSMessage interface.
func (msg *DataMessage) SetID(id uint32) {
	msg.systemBytes = make([]byte, 4)
	binary.BigEndian.PutUint32(msg.systemBytes, id)
}

// SystemBytes returns the system bytes of the data message.
// If the system bytes was not set, it will return []byte{0, 0, 0, 0}.
//
// It implements the SystemBytes method of the HSMSMessage interface.
func (msg *DataMessage) SystemBytes() []byte {
	if len(msg.systemBytes) < 4 {
		return []byte{0, 0, 0, 0}
	}

	return msg.systemBytes[:4]
}

// SetSystemBytes sets system bytes to the data message.
// It will return error if the systemBytes is not 4 bytes.
//
// It implements the SetSystemBytes method of the HSMSMessage interface.
func (msg *DataMessage) SetSystemBytes(systemBytes []byte) error {
	if len(systemBytes) != 4 {
		return ErrInvalidSystemBytes
	}

	msg.systemBytes = util.CloneSlice(systemBytes, 4)

	return nil
}

// Error returns the error of the data message.
//
// It implements the Error method of the HSMSMessage interface.
func (msg *DataMessage) Error() error {
	if msg == nil {
		return nil
	}

	return msg.err
}

// SetError sets the error of the data message.
// It is used to indicate an error condition in the message.
//
// It implements the SetError method of the HSMSMessage interface.
func (msg *DataMessage) SetError(err error) {
	if msg == nil {
		return
	}

	msg.err = err
}

// Header returns the 10-byte HSMS message header.
//
// It implements the Header method of the HSMSMessage interface.
func (msg *DataMessage) Header() []byte {
	header := make([]byte, 10)
	msg.generateHeader(header)
	return header
}

// SetHeader sets the header of the data message.
// It will return error if the header is invalid.
//
// It implements the SetHeader method of the HSMSMessage interface.
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

// StreamCode returns the stream code of the data message.
//
// It implements the StreamCode method of the secs2.SECS2Message interface.
func (msg *DataMessage) StreamCode() uint8 {
	return msg.stream
}

// FunctionCode returns the function code of the data message.
//
// It implements the FunctionCode method of the secs2.SECS2Message interface.
func (msg *DataMessage) FunctionCode() uint8 {
	return msg.function
}

// WaitBit returnes the boolean representation to indicates WBit is set
//
// It implements the WaitBit method of the secs2.SECS2Message interface.
func (msg *DataMessage) WaitBit() bool {
	return msg.waitBit == WaitBitTrue
}

// Item returnes the SECS-II data item in data message.
//
// It implements the Item method of the secs2.SECS2Message interface.
func (msg *DataMessage) Item() secs2.Item {
	return msg.dataItem
}

// SMLHeader returns the message header of the data message, e.g. "S6F11 W".
//
// This method only exists on DataMessage, and it is used to generate the SML header
func (msg *DataMessage) SMLHeader() string {
	quote := StreamFunctionQuote()
	header := fmt.Sprintf("%sS%dF%d%s", quote, msg.stream, msg.function, quote)

	if msg.waitBit == WaitBitTrue {
		header += " W"
	}

	return header
}

// ToBytes returns the HSMS byte representation of the data message.
//
// It will return empty byte slice if the message can't be represented as HSMS format,
// i.e. wait bit is optional.
//
// It implements the ToBytes method of the HSMSMessage interface.
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

// MarshalBinary serializes the data message into its byte representation for transmission.
//
// It implements the MarshalBinary method of the encoding.BinaryMarshaler interface.
func (msg *DataMessage) MarshalBinary() ([]byte, error) {
	return msg.ToBytes(), nil
}

// UnmarshalBinary deserializes the byte data into the HSMS data message.
//
// It implements the UnmarshalBinary method of the encoding.BinaryUnmarshaler interface.
func (msg *DataMessage) UnmarshalBinary(data []byte) error {
	m, err := DecodeHSMSMessage(data)
	if err != nil {
		return err
	}

	dMsg, ok := m.ToDataMessage()
	if !ok {
		return errors.New("expected data message")
	}

	*msg = *dMsg

	return nil
}

// IsControlMessage returns false, indicating that the message is not a control message.
// It implements the IsControlMessage method of the HSMSMessage interface.
func (msg *DataMessage) IsControlMessage() bool {
	return false
}

// ToControlMessage attempts to convert the message to an HSMS control message.
// Since a DataMessage cannot be converted to a ControlMessage, it always returns nil and false.
//
// It implements the ToControlMessage method of the HSMSMessage interface.
func (msg *DataMessage) ToControlMessage() (*ControlMessage, bool) {
	return nil, false
}

// IsDataMessage returns true, indicating that the message is a data message.
//
// It implements the IsDataMessage method of the HSMSMessage interface.
func (msg *DataMessage) IsDataMessage() bool {
	return true
}

// ToDataMessage converts the message to an data message.
// Since the message is already a DataMessage, it returns a pointer to itself and true.
//
// It implements the ToDataMessage method of the HSMSMessage interface.
func (msg *DataMessage) ToDataMessage() (*DataMessage, bool) {
	return msg, true
}

// ToSML returns SML representation of data message.
//
// It implements the ToSML method of the secs2.SECS2Message interface.
func (msg *DataMessage) ToSML() string {
	if msg.dataItem == nil || msg.dataItem.IsEmpty() {
		return msg.SMLHeader() + "\n."
	}

	var sb strings.Builder

	header := msg.SMLHeader()
	msgBody := msg.dataItem.ToSML()

	sb.Grow(len(header) + len(msgBody) + 2)

	sb.WriteString(header)
	sb.WriteString("\n")
	sb.WriteString(msgBody)
	sb.WriteString("\n.")

	return sb.String()
}

// Free releases the message and its associated resources back to the pool.
// After calling Free, the message should not be accessed again.
//
// It implements the Free method of the HSMSMessage interface.
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
//
// It implements the Clone method of the HSMSMessage interface.
func (msg *DataMessage) Clone() HSMSMessage {
	cloned := &DataMessage{
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
