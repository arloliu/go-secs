package hsms

import (
	"encoding"

	"github.com/arloliu/go-secs/secs2"
)

// Type constants representing the different types of HSMS messages.
// These constants categorize messages based on their primary function and role in the HSMS protocol.
const (
	UndefiniedMsgType = -1 // Undefeind stream session type
	DataMsgType       = 0  // Data message containing SECS-II data
	SelectReqType     = 1  // Select request control message
	SelectRspType     = 2  // Select response control message
	DeselectReqType   = 3  // Deselect request control message
	DeselectRspType   = 4  // Deselect response control message
	LinkTestReqType   = 5  // Linktest request control message
	LinkTestRspType   = 6  // Linktest response control message
	RejectReqType     = 7  // Reject request control message
	SeparateReqType   = 9  // Separate request control message
)

var hsmsMsgTypeMap = map[int]string{
	DataMsgType:       "data.msg",
	SelectReqType:     "select.req",
	SelectRspType:     "select.rsp",
	DeselectReqType:   "deselect.req",
	DeselectRspType:   "deselect.rsp",
	LinkTestReqType:   "linktest.req",
	LinkTestRspType:   "linktest.rsp",
	RejectReqType:     "reject.req",
	SeparateReqType:   "separate.req",
	UndefiniedMsgType: "undefined",
}

// HSMSMessage represents a message in the HSMS (High-Speed SECS Message Services) protocol.
// It extends the SECS2Message interface to include HSMS-specific attributes and functionalities.
//
// HSMS messages are categorized into:
//   - Data Message: Used for exchanging SECS-II data between the host and equipment.
//   - Control Message: Used for managing the HSMS connection itself (e.g., session control, link testing).
//
// This interface provides methods for accessing common HSMS message attributes and converting the message
// into its specific data or control message representation.
type HSMSMessage interface {
	encoding.BinaryMarshaler
	encoding.BinaryUnmarshaler
	secs2.SECS2Message

	// Type returns the HSMS message type, which can be one of the following constants:
	//  - hsms.DataMsgType
	//  - hsms.SelectReqType
	//  - hsms.SelectRspType
	//  - hsms.DeselectReqType
	//  - hsms.DeselectRspType
	//  - hsms.LinkTestReqType
	//  - hsms.LinkTestRspType
	//  - hsms.RejectReqType
	//  - hsms.SeparateReqType
	Type() int

	// SessionID returns the session ID for the HSMS message.
	SessionID() uint16

	// SetSessionID sets the session ID for the HSMS message.
	SetSessionID(sessionID uint16)

	// ID returns a numeric representation of the system bytes (message ID).
	ID() uint32

	// SetID sets the system bytes (message ID) for the HSMS message.
	SetID(id uint32)

	// SystemBytes returns the 4-byte system bytes (message ID).
	//
	// Lifetime: implementations are permitted (and DataMessage in particular
	// does) to return a slice that aliases the message's internal storage.
	// The returned slice remains valid only while the message itself is
	// alive. Do NOT retain it past Free(), and do NOT hand it to a goroutine
	// whose lifetime exceeds the message's: after Free, a pooled message can
	// be re-issued and a subsequent ID write will silently overwrite the
	// cached bytes. If you need a stable copy, allocate one with
	// append([]byte(nil), msg.SystemBytes()...) or use ID() and re-encode.
	SystemBytes() []byte

	// SetSystemBytes sets the system bytes (message ID) for the HSMS message.
	// The systemBytes should have length of 4.
	//
	// It will return error if the systemBytes is invalid.
	SetSystemBytes(systemBytes []byte) error

	// Error returns the error associated with the HSMS message.
	Error() error

	// SetError sets the error for the HSMS message.
	// It is used to indicate an error condition in the message.
	SetError(err error)

	// Header returns the 10-byte HSMS message header.
	Header() []byte

	// SetHeader sets the header of the HSMS message.
	//
	// It will return error if the header is invalid.
	SetHeader(header []byte) error

	// ToBytes serializes the HSMS message into its byte representation for transmission.
	ToBytes() []byte

	// IsControlMessage returns if the message is control message.
	IsControlMessage() bool
	// ToControlMessage converts the message to an HSMS control message if applicable.
	// It returns a pointer to the ControlMessage and a boolean indicating if the conversion was successful.
	ToControlMessage() (*ControlMessage, bool)

	// IsDataMessage returns if the message is data message.
	IsDataMessage() bool
	// ToDataMessage converts the message to an HSMS data message if applicable.
	// It returns a pointer to the DataMessage and a boolean indicating if the conversion was successful.
	ToDataMessage() (*DataMessage, bool)

	// Free releases the message and its associated resources back to the pool.
	// After calling Free, the message should not be accessed again.
	Free()

	// Clone creates a deep copy of the message, allowing modifications to the clone without affecting the original message.
	Clone() HSMSMessage
}

var sfQuote = "'"

// UseStreamFunctionNoQuote sets the quoting style for stream and function codes in SML to use no quotes.
// This affects both the generation of SML strings (ToSML methods) and the parsing of SML strings.
func UseStreamFunctionNoQuote() {
	sfQuote = ""
}

// UseStreamFunctionSingleQuote sets the quoting style for stream and function codes in SML to use single quotes (').
// This affects both the generation of SML strings and the parsing of SML strings.
func UseStreamFunctionSingleQuote() {
	sfQuote = "'"
}

// UseStreamFunctionDoubleQuote sets the quoting style for stream and function codes in SML to use double quotes (").
// This affects both the generation of SML strings and the parsing of SML strings.
func UseStreamFunctionDoubleQuote() {
	sfQuote = "\""
}

// StreamFunctionQuote returns the current quoting character used for stream and function codes in SML.
// It returns an empty string if no quotes are used, a single quote (') if single quotes are used,
// or a double quote (") if double quotes are used.
func StreamFunctionQuote() string {
	return sfQuote
}

// IsValidSType reports whether b is an SType defined by SEMI E37.
//
// Defined STypes are 0 (data message) and 1-7, 9 (control messages).
// SType 8 and 10-255 are undefined and, per SEMI E37 §7.10.3, must be
// answered with a Reject.req.
func IsValidSType(b byte) bool {
	switch int(b) {
	case DataMsgType, SelectReqType, SelectRspType, DeselectReqType,
		DeselectRspType, LinkTestReqType, LinkTestRspType, RejectReqType, SeparateReqType:
		return true
	default:
		return false
	}
}

// MsgInfo returns a structued message information without SML string.
func MsgInfo(msg HSMSMessage, keyValues ...any) []any {
	return msgInfo(msg, false, keyValues...)
}

// MsgInfoSML returns a structued message information with SML string.
func MsgInfoSML(msg HSMSMessage, keyValues ...any) []any {
	return msgInfo(msg, true, keyValues...)
}

func msgInfo(msg HSMSMessage, sml bool, keyValues ...any) []any { //nolint:revive
	msgType, ok := hsmsMsgTypeMap[msg.Type()]
	if !ok {
		msgType = "undefined"
	}

	info := []any{
		"id", msg.ID(),
		"type", msgType,
		"s", msg.StreamCode(),
		"f", msg.FunctionCode(),
	}

	if sml && msg.Item() != nil {
		info = append(info, "sml", msg.Item().ToSML())
	}

	result := make([]any, 0, len(keyValues)+len(info))
	result = append(result, keyValues...)
	result = append(result, info...)

	return result
}

// MsgInfoFromFields builds the same key/value slice as MsgInfo but accepts
// primitive fields directly instead of an HSMSMessage. It is intended for hot
// paths that have transferred message ownership to another goroutine (so the
// HSMSMessage value can no longer be safely accessed) but still want to emit a
// structured log line on a slow / error path without paying the slice
// allocation on every successful call.
//
// Typical use: capture id / type / stream / function as locals before handing
// ownership off, then call MsgInfoFromFields only inside the error or timeout
// branch.
func MsgInfoFromFields(msgType int, id uint32, stream, function byte, keyValues ...any) []any {
	typeName, ok := hsmsMsgTypeMap[msgType]
	if !ok {
		typeName = "undefined"
	}

	info := [...]any{
		"id", id,
		"type", typeName,
		"s", stream,
		"f", function,
	}

	result := make([]any, 0, len(keyValues)+len(info))
	result = append(result, keyValues...)
	result = append(result, info[:]...)

	return result
}

// MsgHexString returns a hex string representation of the provided byte slices.
// It outputs byte by byte in hex format, separated by a space.
func MsgHexString(datas ...[]byte) string {
	// Calculate total length across all byte slices
	totalLen := 0
	for _, data := range datas {
		totalLen += len(data)
	}

	if totalLen == 0 {
		return ""
	}

	// pre-allocate buffer with exact capacity needed
	// each byte becomes 2 hex chars + 1 space, minus 1 space for the last byte
	buf := make([]byte, 0, totalLen*3-1)

	// Use a lookup table for hex digits (faster than hex.EncodeToString)
	const hexChars = "0123456789ABCDEF"

	first := true
	for _, data := range datas {
		for _, b := range data {
			if !first {
				buf = append(buf, ' ')
			}
			buf = append(buf, hexChars[b>>4], hexChars[b&0x0F])
			first = false
		}
	}

	return string(buf)
}
