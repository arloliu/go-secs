package hsms

// MsgType identifies the HSMS SType field (header byte 5).
// Defined values correspond to SEMI E37 §7.10.3.
type MsgType uint8

const (
	// DataMsgType is the SType for a SECS-II data message (SType = 0).
	DataMsgType MsgType = 0
	// SelectReqType is the SType for a Select.req control message (SType = 1).
	SelectReqType MsgType = 1
	// SelectRspType is the SType for a Select.rsp control message (SType = 2).
	SelectRspType MsgType = 2
	// DeselectReqType is the SType for a Deselect.req control message (SType = 3).
	DeselectReqType MsgType = 3
	// DeselectRspType is the SType for a Deselect.rsp control message (SType = 4).
	DeselectRspType MsgType = 4
	// LinktestReqType is the SType for a Linktest.req control message (SType = 5).
	LinktestReqType MsgType = 5
	// LinktestRspType is the SType for a Linktest.rsp control message (SType = 6).
	LinktestRspType MsgType = 6
	// RejectReqType is the SType for a Reject.req control message (SType = 7).
	RejectReqType MsgType = 7
	// SeparateReqType is the SType for a Separate.req control message (SType = 9).
	SeparateReqType MsgType = 9
	// UndefinedMsgType is the sentinel value returned when the SType byte does not
	// correspond to any defined SEMI E37 message type.
	UndefinedMsgType MsgType = 255
)

// IsValidSType reports whether b is an SType defined by SEMI E37.
//
// Defined STypes are 0 (data message) and 1–7, 9 (control messages).
// SType 8 and 10–255 are undefined and, per SEMI E37 §7.10.3, must be
// answered with a Reject.req.
func IsValidSType(b byte) bool {
	switch MsgType(b) { //nolint:exhaustive // UndefinedMsgType (255) is intentionally excluded via default
	case DataMsgType, SelectReqType, SelectRspType, DeselectReqType,
		DeselectRspType, LinktestReqType, LinktestRspType, RejectReqType, SeparateReqType:
		return true
	default:
		return false
	}
}

// Message is the read-only interface implemented by all immutable HSMS messages.
// Both ControlMessage and DataMessage satisfy this interface.
//
// All methods return value types (not slices) to guarantee immutability: callers
// may retain the returned values indefinitely without risk of aliasing.
type Message interface {
	// Type returns the HSMS SType for this message.
	// Returns UndefinedMsgType if the SType byte is not a defined SEMI E37 value.
	Type() MsgType

	// SessionID returns the session identifier encoded in header bytes 0–1 (big-endian).
	SessionID() uint16

	// SystemBytes returns the four system bytes (header bytes 6–9) as a value.
	// The returned array is an independent copy; callers may retain it freely.
	SystemBytes() [4]byte

	// HeaderBytes returns a copy of the full 10-byte HSMS message header as a value.
	HeaderBytes() [10]byte

	// ToBytes serializes the message to its on-wire byte representation
	// (4-byte length prefix followed by the 10-byte header and, for data messages,
	// the encoded SECS-II item body).
	ToBytes() []byte

	// ToDataMessage narrows msg to a *DataMessage.
	// It returns (msg, true) when msg is already a *DataMessage, or (nil, false)
	// when msg is a *ControlMessage.
	ToDataMessage() (*DataMessage, bool)
}
