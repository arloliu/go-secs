package hsms

import (
	"encoding/binary"
	"errors"
)

// ControlMessage is an immutable HSMS control message.
//
// The header is stored as a [10]byte value — not a slice — so every copy of
// the struct owns its own header bytes. WithSessionID / WithSystemBytes return
// a new *ControlMessage; no method mutates the receiver.
type ControlMessage struct {
	header        [10]byte
	replyExpected bool
}

// Ensure *ControlMessage satisfies the Message interface.
var _ Message = (*ControlMessage)(nil)

// ────────────────────────────────────────────────────────────────
// Message interface implementation
// ────────────────────────────────────────────────────────────────

// Type returns the HSMS SType for this control message (header byte 5).
// Returns UndefinedMsgType if the SType byte is not a defined SEMI E37 value.
func (msg *ControlMessage) Type() MsgType {
	s := msg.header[5]
	if !IsValidSType(s) {
		return UndefinedMsgType
	}

	return MsgType(s)
}

// SessionID returns the session ID encoded in header bytes 0–1 (big-endian).
func (msg *ControlMessage) SessionID() uint16 {
	return binary.BigEndian.Uint16(msg.header[0:2])
}

// SystemBytes returns a copy of header bytes 6–9 as a [4]byte value.
func (msg *ControlMessage) SystemBytes() [4]byte {
	var sb [4]byte
	copy(sb[:], msg.header[6:10])

	return sb
}

// HeaderBytes returns a copy of the full 10-byte HSMS header as a value.
func (msg *ControlMessage) HeaderBytes() [10]byte {
	return msg.header
}

// ToBytes serializes the control message to its on-wire representation:
// a 4-byte length prefix (always 0x00_00_00_0A = 10) followed by the
// 10-byte header. The returned slice is always exactly 14 bytes.
func (msg *ControlMessage) ToBytes() []byte {
	out := make([]byte, 14)
	out[0] = 0x00
	out[1] = 0x00
	out[2] = 0x00
	out[3] = 0x0A // body length = 10 (the header itself; control has no payload)
	copy(out[4:], msg.header[:])

	return out
}

// ────────────────────────────────────────────────────────────────
// Additional accessors
// ────────────────────────────────────────────────────────────────

// WaitBit reports whether the reply-expected flag is set for this message.
func (msg *ControlMessage) WaitBit() bool {
	return msg.replyExpected
}

// RejectReasonCode returns the reject reason code from a Reject.req message.
// It delegates to GetRejectReasonCode and is provided as a convenience method.
// The returned code matches [RejectError.Reason] (a byte).
func (msg *ControlMessage) RejectReasonCode() (byte, error) {
	return GetRejectReasonCode(msg)
}

// ────────────────────────────────────────────────────────────────
// Immutable mutators — return a new *ControlMessage
// ────────────────────────────────────────────────────────────────

// WithSessionID returns a new *ControlMessage identical to msg except that
// the session ID in header bytes 0–1 is replaced by id.
func (msg *ControlMessage) WithSessionID(id uint16) *ControlMessage {
	n := *msg // copy header value + replyExpected
	binary.BigEndian.PutUint16(n.header[0:2], id)

	return &n
}

// WithSystemBytes returns a new *ControlMessage identical to msg except that
// header bytes 6–9 are replaced by b.
func (msg *ControlMessage) WithSystemBytes(b [4]byte) *ControlMessage {
	n := *msg
	n.header[6] = b[0]
	n.header[7] = b[1]
	n.header[8] = b[2]
	n.header[9] = b[3]

	return &n
}

// ────────────────────────────────────────────────────────────────
// Status constants
// ────────────────────────────────────────────────────────────────

const (
	// SelectStatusSuccess indicates that communication is successfully established.
	SelectStatusSuccess = 0
	// SelectStatusAlreadyActive indicates that communication is already active.
	SelectStatusAlreadyActive = 1
	// SelectStatusNotReady indicates that communication is not ready.
	SelectStatusNotReady = 2
	// SelectStatusAlreadyUsed indicates that the TCP/IP port is exhausted;
	// another connection is already established.
	SelectStatusAlreadyUsed = 3
	// SelectStatusEntityUnknown indicates that the entity (session) is not supported.
	// This status is only relevant for HSMS-GS.
	SelectStatusEntityUnknown = 4
	// SelectStatusEntityAlreadyUsed indicates that the entity (session) is already used by others.
	SelectStatusEntityAlreadyUsed = 5
	// SelectStatusEntityAlreadyActive indicates that the entity (session) is already active.
	SelectStatusEntityAlreadyActive = 6

	// DeselectStatusSuccess indicates that communication is successfully ended.
	DeselectStatusSuccess = 0
	// DeselectStatusNotEstablished indicates that HSMS communication has not yet been
	// established with a Select, or has already been ended with a previous Deselect.
	DeselectStatusNotEstablished = 1
	// DeselectStatusBusy indicates that the session is still in use and cannot
	// yet be relinquished gracefully.
	DeselectStatusBusy = 2
)

// Reject reason code constants for Reject.req control messages (SEMI E37 §7.9).
const (
	// RejectSTypeNotSupported indicates the received message's SType is not supported.
	RejectSTypeNotSupported = 1
	// RejectPTypeNotSupported indicates the received message's PType is not supported.
	RejectPTypeNotSupported = 2
	// RejectTransactionNotOpen indicates the transaction is not open (response received without request).
	RejectTransactionNotOpen = 3
	// RejectNotSelected indicates a data message was received in non-selected state.
	RejectNotSelected = 4
)

// ────────────────────────────────────────────────────────────────
// Factories
// ────────────────────────────────────────────────────────────────

// NewSelectReq creates an HSMS Select.req control message.
func NewSelectReq(sessionID uint16, systemBytes [4]byte) *ControlMessage {
	var header [10]byte
	binary.BigEndian.PutUint16(header[0:2], sessionID)
	header[5] = byte(SelectReqType)
	header[6] = systemBytes[0]
	header[7] = systemBytes[1]
	header[8] = systemBytes[2]
	header[9] = systemBytes[3]

	return &ControlMessage{header: header, replyExpected: true}
}

// NewSelectRsp creates an HSMS Select.rsp control message from a Select.req.
//
// req must be non-nil.
//
// selectStatus 0 = communication successfully established;
// 1 = already active; 2 = not ready; 3 = TCP/IP port exhausted;
// 4–255 = reserved failure codes.
func NewSelectRsp(req *ControlMessage, selectStatus byte) (*ControlMessage, error) {
	if req.Type() != SelectReqType {
		return nil, errors.New("expected select.req message")
	}

	var header [10]byte
	header[0] = req.header[0]
	header[1] = req.header[1]
	header[3] = selectStatus
	header[5] = byte(SelectRspType)
	header[6] = req.header[6]
	header[7] = req.header[7]
	header[8] = req.header[8]
	header[9] = req.header[9]

	return &ControlMessage{header: header, replyExpected: false}, nil
}

// NewDeselectReq creates an HSMS Deselect.req control message.
func NewDeselectReq(sessionID uint16, systemBytes [4]byte) *ControlMessage {
	var header [10]byte
	binary.BigEndian.PutUint16(header[0:2], sessionID)
	header[5] = byte(DeselectReqType)
	header[6] = systemBytes[0]
	header[7] = systemBytes[1]
	header[8] = systemBytes[2]
	header[9] = systemBytes[3]

	return &ControlMessage{header: header, replyExpected: true}
}

// NewDeselectRsp creates an HSMS Deselect.rsp control message from a Deselect.req.
//
// req must be non-nil.
//
// deselectStatus 0 = connection successfully ended;
// 1 = communication not yet established;
// 2 = busy, cannot yet relinquish;
// 3–255 = reserved failure codes.
func NewDeselectRsp(req *ControlMessage, deselectStatus byte) (*ControlMessage, error) {
	if req.Type() != DeselectReqType {
		return nil, errors.New("expected deselect.req message")
	}

	var header [10]byte
	header[0] = req.header[0]
	header[1] = req.header[1]
	header[3] = deselectStatus
	header[5] = byte(DeselectRspType)
	header[6] = req.header[6]
	header[7] = req.header[7]
	header[8] = req.header[8]
	header[9] = req.header[9]

	return &ControlMessage{header: header, replyExpected: false}, nil
}

// NewLinktestReq creates an HSMS Linktest.req control message.
// Per SEMI E37, the session ID for Linktest messages is always 0xFFFF.
func NewLinktestReq(systemBytes [4]byte) *ControlMessage {
	var header [10]byte
	header[0] = 0xFF
	header[1] = 0xFF
	header[5] = byte(LinktestReqType)
	header[6] = systemBytes[0]
	header[7] = systemBytes[1]
	header[8] = systemBytes[2]
	header[9] = systemBytes[3]

	return &ControlMessage{header: header, replyExpected: true}
}

// NewLinktestRsp creates an HSMS Linktest.rsp control message from a Linktest.req.
// req must be non-nil.
func NewLinktestRsp(req *ControlMessage) (*ControlMessage, error) {
	if req.Type() != LinktestReqType {
		return nil, errors.New("expected linktest.req message")
	}

	var header [10]byte
	header[0] = 0xFF
	header[1] = 0xFF
	header[5] = byte(LinktestRspType)
	header[6] = req.header[6]
	header[7] = req.header[7]
	header[8] = req.header[8]
	header[9] = req.header[9]

	return &ControlMessage{header: header, replyExpected: false}, nil
}

// NewSeparateReq creates an HSMS Separate.req control message.
func NewSeparateReq(sessionID uint16, systemBytes [4]byte) *ControlMessage {
	var header [10]byte
	binary.BigEndian.PutUint16(header[0:2], sessionID)
	header[5] = byte(SeparateReqType)
	header[6] = systemBytes[0]
	header[7] = systemBytes[1]
	header[8] = systemBytes[2]
	header[9] = systemBytes[3]

	return &ControlMessage{header: header, replyExpected: false}
}

// NewRejectReq creates an HSMS Reject.req control message for a received message.
//
// rejected must be non-nil; it is the message being rejected. reasonCode values:
//   - 1 (RejectSTypeNotSupported): SType not supported; header[2] = sType of rejected.
//   - 2 (RejectPTypeNotSupported): PType not supported; header[2] = pType of rejected.
//   - 3 (RejectTransactionNotOpen): transaction not open.
//   - 4 (RejectNotSelected): data message received in non-selected state.
func NewRejectReq(rejected Message, reasonCode byte) *ControlMessage {
	var header [10]byte
	binary.BigEndian.PutUint16(header[0:2], rejected.SessionID())

	if rejected.Type() == DataMsgType {
		// pType and sType are always 0 for data messages
		header[2] = 0
	} else {
		hdr := rejected.HeaderBytes()
		if reasonCode == RejectPTypeNotSupported {
			header[2] = hdr[4] // pType
		} else {
			header[2] = hdr[5] // sType
		}
	}

	header[3] = reasonCode
	header[5] = byte(RejectReqType)
	sb := rejected.SystemBytes()
	header[6] = sb[0]
	header[7] = sb[1]
	header[8] = sb[2]
	header[9] = sb[3]

	return &ControlMessage{header: header, replyExpected: false}
}

// NewRejectReqRaw creates an HSMS Reject.req control message from raw field values.
//
// sessionID, pType, sType, and systemBytes should be copied from the rejected message.
// reasonCode values: 1–4 as for NewRejectReq; 5–255 are reserved.
func NewRejectReqRaw(sessionID uint16, pType, sType byte, systemBytes [4]byte, reasonCode byte) *ControlMessage {
	var header [10]byte
	binary.BigEndian.PutUint16(header[0:2], sessionID)

	if reasonCode == RejectPTypeNotSupported {
		header[2] = pType
	} else {
		header[2] = sType
	}

	header[3] = reasonCode
	header[5] = byte(RejectReqType)
	header[6] = systemBytes[0]
	header[7] = systemBytes[1]
	header[8] = systemBytes[2]
	header[9] = systemBytes[3]

	return &ControlMessage{header: header, replyExpected: false}
}

// GetRejectReasonCode extracts and validates the reason code from a Reject.req message.
// Returns ErrInvalidRejectMsg if msg is not a Reject.req, or ErrInvalidRejectReason
// if the reason code is outside [1, 4]. The returned code is a byte, matching
// [RejectError.Reason].
func GetRejectReasonCode(msg Message) (byte, error) {
	if msg.Type() != RejectReqType {
		return 0, ErrInvalidRejectMsg
	}

	hdr := msg.HeaderBytes()
	reasonCode := hdr[3]

	if reasonCode < RejectSTypeNotSupported || reasonCode > RejectNotSelected {
		return 0, ErrInvalidRejectReason
	}

	return reasonCode, nil
}
