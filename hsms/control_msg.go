package hsms

import (
	"encoding/binary"
	"errors"

	"github.com/arloliu/go-secs/internal/util"
	"github.com/arloliu/go-secs/secs2"
)

// ControlMessage represents a HSMS control message.
//
// It implements the HSMSMessage and secs2.SECS2Message interfaces.
type ControlMessage struct {
	header        []byte
	replyExpected bool
}

// ensure ControlMessage implements hsms.HSMSMessage and secs2.SECS2Message interfaces.
var (
	_ HSMSMessage        = (*ControlMessage)(nil)
	_ secs2.SECS2Message = (*ControlMessage)(nil)
)

// NewControlMessage creates HSMS control message from header bytes.
// The header should have appropriate values as specified in HSMS specification.
func NewControlMessage(header []byte, replyExpected bool) HSMSMessage {
	return &ControlMessage{header: util.CloneSlice(header, 10), replyExpected: replyExpected}
}

// Type returns the message type of the HSMS control message.
//
// This method implements the HSMSMessage.Type() interface.
func (msg *ControlMessage) Type() int {
	stype := int(msg.header[5])
	_, ok := hsmsMsgTypeMap[stype]
	if !ok {
		return UndefiniedMsgType
	}

	return stype
}

// ID returns a numeric representation of the system bytes (message ID).
//
// This method implements the HSMSMessage.ID() interface.
func (msg *ControlMessage) ID() uint32 {
	return binary.BigEndian.Uint32(msg.header[6:10])
}

// SessionID returns the session id of the HSMS message.
//
// This method implements the HSMSMessage.SessionID() interface.
func (msg *ControlMessage) SessionID() uint16 {
	return binary.BigEndian.Uint16(msg.header[:2])
}

// SetSessionID sets the session id of the HSMS message.
//
// This method implements the HSMSMessage.SetSessionID() interface.
func (msg *ControlMessage) SetSessionID(sessionID uint16) {
	binary.BigEndian.PutUint16(msg.header[:2], sessionID)
}

// SystemBytes returns the 4-byte system bytes (message ID).
//
// This method implements the HSMSMessage.SystemBytes() interface.
func (msg *ControlMessage) SystemBytes() []byte {
	return msg.header[6:10]
}

// SetSystemBytes sets system bytes to the data message.
//
// It will return error if the systemBytes is not 4 bytes.
//
// This method implements the HSMSMessage.SetSystemBytes() interface.
func (msg *ControlMessage) SetSystemBytes(systemBytes []byte) error {
	if len(systemBytes) != 4 {
		return ErrInvalidSystemBytes
	}

	copy(msg.header[6:10], systemBytes)

	return nil
}

// Header returns the 10-byte SECS-II message header
//
// This method implements the HSMSMessage.Header() interface.
func (msg *ControlMessage) Header() []byte {
	return msg.header
}

// SetHeader sets the header of the HSMS message.
//
// It will return error if the header is invalid.
//
// This method implements the HSMSMessage.SetHeader() interface.
func (msg *ControlMessage) SetHeader(header []byte) error {
	if len(header) != 10 {
		return ErrInvalidHeaderLength
	}

	if header[4] != 0 {
		return ErrInvalidPType
	}

	if header[5] < 1 || header[5] > 9 {
		return ErrInvalidControlMsgSType
	}

	msg.header = util.CloneSlice(header, 10)

	return nil
}

// ToBytes returns the HSMS byte representation of the control message.
//
// This method implements the HSMSMessage.ToBytes() interface.
func (msg *ControlMessage) ToBytes() []byte {
	result := make([]byte, 0, 14)
	result = append(result, 0, 0, 0, 10)        // 4 bytes: message length, MSB first.
	result = append(result, msg.header[:10]...) // 10 bytes: message header

	return result
}

// StreamCode returns the stream code for the HSMS message.
//
// This method implements the secs2.SECS2Message.StreamCode() interface.
func (msg *ControlMessage) StreamCode() uint8 {
	return msg.header[2]
}

// FunctionCode returns the header[3] for the HSMS message, it's defined by different control message type.
//
// This method implements the secs2.SECS2Message.FunctionCode() interface.
func (msg *ControlMessage) FunctionCode() uint8 {
	return msg.header[3]
}

// WaitBit returnes the boolean representation to indicates WBit is set
//
// This method implements the secs2.SECS2Message.WaitBit() interface.
func (msg *ControlMessage) WaitBit() bool {
	return msg.replyExpected
}

// Item returns empty SECS-II data item for HSMS control message.
//
// This method implements the secs2.SECS2Message.Item() interface.
func (msg *ControlMessage) Item() secs2.Item {
	return secs2.NewEmptyItem()
}

// IsControlMessage returns true, indicating that a ControlMessage is a control message.
func (msg *ControlMessage) IsControlMessage() bool {
	return true
}

// ToControlMessage converts the message to an HSMS control message.
// Since the message is already a ControlMessage, it returns a pointer to itself and true.
func (msg *ControlMessage) ToControlMessage() (*ControlMessage, bool) {
	return msg, true
}

// IsDataMessage returns false, indicating that a ControlMessage is not a data message.
func (msg *ControlMessage) IsDataMessage() bool {
	return false
}

// ToDataMessage attempts to convert the message to an HSMS data message.
// Since a ControlMessage cannot be converted to a DataMessage, it always returns nil and false.
func (msg *ControlMessage) ToDataMessage() (*DataMessage, bool) {
	return nil, false
}

// Free takes no action for HSMS control message
//
// This method implements the HSMSMessage.Free() interface.
func (msg *ControlMessage) Free() {}

// Clone creates a deep copy of the message, allowing modifications to the clone without affecting the original message.
//
// This method implements the HSMSMessage.Clone() interface.
func (msg *ControlMessage) Clone() HSMSMessage {
	cloned := &ControlMessage{header: make([]byte, 10), replyExpected: msg.replyExpected}
	copy(cloned.header, msg.header)
	return cloned
}

// NewSelectReq creates HSMS Select.req control message.
// systemBytes should have length of 4.
func NewSelectReq(sessionID uint16, systemBytes []byte) *ControlMessage {
	header := make([]byte, 10)
	header[0] = byte(sessionID >> 8)
	header[1] = byte(sessionID)
	header[5] = SelectReqType
	header[6] = systemBytes[0]
	header[7] = systemBytes[1]
	header[8] = systemBytes[2]
	header[9] = systemBytes[3]

	return &ControlMessage{header, true}
}

const (
	// SelectStatusSuccess indicates that communication is successfully established.
	SelectStatusSuccess = 0
	// SelectStatusActived indicates that communication is already actived.
	SelectStatusActived = 1
	// SelectStatusNotReady indicates that communication is not ready.
	SelectStatusNotReady = 2
	// SelectStatusAlreadyUsed indicates that TCP/IP port is exhausted, another connection already established.
	SelectStatusAlreadyUsed = 3

	// SelectStatusEntityUnknown indicates that entity(session) is not supported.
	// this status only relevant for HSMS-GS.
	SelectStatusEntityUnknown = 4
	// SelectStatusEntityAlreadyUsed indicates that entity(session) is already used by others.
	SelectStatusEntityAlreadyUsed = 5
	// SelectStatusEntitActived indicates that entity(session) is already actived.
	SelectStatusEntitActived = 6
)

// NewSelectRsp creates HSMS Select.rsp control message from Select.req message.
// selectStatus 0 means that communication is successfully established,
// 1 means that communication is already actived,
// 2 means that communication is not ready,
// 3 means that connection that TCP/IP port is exhausted (for HSMS-SS passive mode),
// 4-255 are reserved failure reason codes.
func NewSelectRsp(selectReq HSMSMessage, selectStatus byte) (*ControlMessage, error) {
	if selectReq.Type() != SelectReqType {
		return nil, errors.New("expected select.req message")
	}

	header := make([]byte, 10)
	msg, _ := selectReq.(*ControlMessage)
	header[0] = msg.header[0]
	header[1] = msg.header[1]
	header[3] = selectStatus
	header[5] = SelectRspType
	header[6] = msg.header[6]
	header[7] = msg.header[7]
	header[8] = msg.header[8]
	header[9] = msg.header[9]

	return &ControlMessage{header, false}, nil
}

// NewDeselectReq creates HSMS Deselect.req control message.
// systemBytes should have length of 4.
func NewDeselectReq(sessionID uint16, systemBytes []byte) *ControlMessage {
	header := make([]byte, 10)
	header[0] = byte(sessionID >> 8)
	header[1] = byte(sessionID)
	header[5] = DeselectReqType
	header[6] = systemBytes[0]
	header[7] = systemBytes[1]
	header[8] = systemBytes[2]
	header[9] = systemBytes[3]

	return &ControlMessage{header, true}
}

// NewDeselectRsp creates HSMS Deselect.rsp control message from Deselect.req message.
// deselectStatus 0 means that the connection is successfully ended,
// 1 means that communication is not yet established,
// 2 means that communication is busy and cannot yet be relinquished,
// 3-255 are reserved failure reason codes.
func NewDeselectRsp(deselectReq HSMSMessage, deselectStatus byte) (*ControlMessage, error) {
	if deselectReq.Type() != DeselectReqType {
		return nil, errors.New("expected deselect.req message")
	}

	header := make([]byte, 10)
	msg, _ := deselectReq.(*ControlMessage)
	header[0] = msg.header[0]
	header[1] = msg.header[1]
	header[3] = deselectStatus
	header[5] = DeselectRspType
	header[6] = msg.header[6]
	header[7] = msg.header[7]
	header[8] = msg.header[8]
	header[9] = msg.header[9]

	return &ControlMessage{header, false}, nil
}

// NewLinktestReq creates HSMS Linktest.req control message.
// systemBytes should have length of 4.
func NewLinktestReq(systemBytes []byte) *ControlMessage {
	header := make([]byte, 10)
	header[0] = 0xFF
	header[1] = 0xFF
	header[5] = LinkTestReqType
	header[6] = systemBytes[0]
	header[7] = systemBytes[1]
	header[8] = systemBytes[2]
	header[9] = systemBytes[3]

	return &ControlMessage{header, true}
}

// NewLinktestRsp creates HSMS Linktest.rsp control message from Linktest.req message.
func NewLinktestRsp(linktestReq HSMSMessage) (*ControlMessage, error) {
	if linktestReq.Type() != LinkTestReqType {
		return nil, errors.New("expected linktest.req message")
	}

	header := make([]byte, 10)
	msg, _ := linktestReq.(*ControlMessage)
	header[0] = 0xFF
	header[1] = 0xFF
	header[5] = LinkTestRspType
	header[6] = msg.header[6]
	header[7] = msg.header[7]
	header[8] = msg.header[8]
	header[9] = msg.header[9]

	return &ControlMessage{header, false}, nil
}

// Reject code contstants defining reason codes of Reject.req control message.
const (
	RejectSTypeNotSupported  = 1 // received message's sType is not supported,
	RejectPTypeNotSupported  = 2 // received message's pType is not supported,
	RejectTransactionNotOpen = 3 // transaction is not open, i.e. response message was received without request,
	RejectNotSelected        = 4 // data message is received in non-selected state,
)

// NewRejectReq creates HSMS Reject.req control message.
//
// recvMsg should be the HSMS message being rejected.
//
// reasonCode should be non-zero,
//   - 1 means that received message's sType is not supported,
//   - 2 means that received message's pType is not supported,
//   - 3 means that transaction is not open, i.e. response message was received without request,
//   - 4 means that data message is received in non-selected state,
//   - 5-255 are reserved reason codes.
func NewRejectReq(recvMsg HSMSMessage, reasonCode byte) *ControlMessage {
	header := make([]byte, 10)
	if recvMsg.Type() == DataMsgType {
		msg, _ := recvMsg.ToDataMessage()
		header[0] = byte(msg.sessionID >> 8)
		header[1] = byte(msg.sessionID)
		header[2] = 0 // the sType and pType of data message is always zero
		copy(header[6:10], msg.systemBytes)
	} else {
		msg, _ := recvMsg.ToControlMessage()
		header[0] = msg.header[0]
		header[1] = msg.header[1]
		if reasonCode == RejectPTypeNotSupported {
			header[2] = msg.header[4]
		} else {
			header[2] = msg.header[5]
		}
		copy(header[6:10], msg.header[6:10])
	}

	header[3] = reasonCode
	header[5] = RejectReqType

	return &ControlMessage{header, false}
}

// NewRejectReqRaw creates HSMS Reject.req control message.
//
// sessionID, pType, sType, and systemBytes should be same as the HSMS message being rejected.
// systemBytes should have length of 4.
//
// reasonCode should be non-zero,
//   - 1 means that received message's sType is not supported,
//   - 2 means that received message's pType is not supported,
//   - 3 means that transaction is not open, i.e. response message was received without request,
//   - 4 means that data message is received in non-selected state,
//   - 5-255 are reserved reason codes.
func NewRejectReqRaw(sessionID uint16, pType, sType byte, systemBytes []byte, reasonCode byte) *ControlMessage {
	header := make([]byte, 10)
	header[0] = byte(sessionID >> 8)
	header[1] = byte(sessionID)
	if reasonCode == 2 {
		header[2] = pType
	} else {
		header[2] = sType
	}
	header[3] = reasonCode
	header[5] = RejectReqType
	header[6] = systemBytes[0]
	header[7] = systemBytes[1]
	header[8] = systemBytes[2]
	header[9] = systemBytes[3]

	return &ControlMessage{header, false}
}

// NewSeparateReq creates HSMS Separate.req control message.
// systemBytes should have length of 4.
func NewSeparateReq(sessionID uint16, systemBytes []byte) *ControlMessage {
	header := make([]byte, 10)
	header[0] = byte(sessionID >> 8)
	header[1] = byte(sessionID)
	header[5] = SeparateReqType
	header[6] = systemBytes[0]
	header[7] = systemBytes[1]
	header[8] = systemBytes[2]
	header[9] = systemBytes[3]

	return &ControlMessage{header, false}
}
