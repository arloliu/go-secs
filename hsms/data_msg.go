package hsms

import (
	"encoding"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/arloliu/go-secs/secs2"
)

// WaitBit byte constants representing if wait-bit is set.
const (
	WaitBitFalse = uint8(0)
	WaitBitTrue  = uint8(1)
)

// MaxStreamCode is the maximum valid stream code value (0-127).
const MaxStreamCode = 127

// waitBitMask is the bitmask for the wait bit stored in the MSB of the stream field,
// matching the HSMS wire format for header byte 2.
const waitBitMask = 0x80

// DataMessage represents a HSMS data message.
//
// It implements the HSMSMessage and secs2.SECS2Message interfaces.
//
// The struct is laid out to fit in a single 64-byte CPU cache line. Storing
// systemBytes inline as a [4]byte array (instead of a 24-byte slice header)
// removes a per-message heap allocation and shrinks the struct further while
// still fitting in one cache line.
// The stream field packs the wait bit in its MSB (bit 7), matching the HSMS
// wire format for header byte 2. Use the StreamCode/WaitBit accessors.
type DataMessage struct {
	dataItem    secs2.Item
	systemBytes [4]byte
	err         error
	sessionID   uint16
	stream      byte // MSB = waitBit, low 7 bits = stream code
	function    byte
	freed       uint32 // accessed via sync/atomic; 0 = not freed, 1 = freed
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
	if stream > MaxStreamCode {
		return nil, ErrInvalidStreamCode
	}

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
	msg.stream &^= waitBitMask

	return msg
}

// newDataMessageWithID is like NewDataMessage but takes the message ID as a
// uint32, skipping the intermediate 4-byte slice allocation that the public
// NewDataMessage + GenerateMsgSystemBytes pair would otherwise materialize.
//
// Package-internal: callers that already have the ID as a primitive (the hot
// send path in BaseSession) use this to avoid the per-send heap alloc.
func newDataMessageWithID(stream byte, function byte, replyExpected bool, sessionID uint16, id uint32, dataItem secs2.Item) (*DataMessage, error) {
	if stream > MaxStreamCode {
		return nil, ErrInvalidStreamCode
	}

	msg := getDataMessageWithID(stream, function, replyExpected, sessionID, id, dataItem)
	if err := msg.sanityCheck(); err != nil {
		putDataMessage(msg)
		return nil, err
	}

	return msg, nil
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
	return binary.BigEndian.Uint32(msg.systemBytes[:])
}

// SetID sets the system bytes of the data message.
//
// It implements the SetID method of the HSMSMessage interface.
func (msg *DataMessage) SetID(id uint32) {
	binary.BigEndian.PutUint32(msg.systemBytes[:], id)
}

// SystemBytes returns the system bytes of the data message.
// If the system bytes was not set, it will return []byte{0, 0, 0, 0}.
//
// It implements the SystemBytes method of the HSMSMessage interface.
//
// Lifetime: the returned slice is a view into the message's internal [4]byte
// field. It remains valid only while the DataMessage itself is alive. Do NOT
// retain the slice past msg.Free(), and do NOT hand it off to a goroutine
// whose lifetime exceeds the message's: after Free, the underlying message
// can be re-issued from the sync.Pool and a subsequent ID write will silently
// overwrite the bytes you cached. If you need a stable copy, allocate one
// with append([]byte(nil), msg.SystemBytes()...) or call ID() and re-encode.
func (msg *DataMessage) SystemBytes() []byte {
	return msg.systemBytes[:]
}

// SetSystemBytes sets system bytes to the data message.
// It will return error if the systemBytes is not 4 bytes.
//
// It implements the SetSystemBytes method of the HSMSMessage interface.
func (msg *DataMessage) SetSystemBytes(systemBytes []byte) error {
	if len(systemBytes) != 4 {
		return ErrInvalidSystemBytes
	}

	// Zero first so behavior is identical to the prior CloneSlice(_, 4) when
	// input is nil or short — even though len==4 is enforced above today,
	// preserving the zero-then-copy idiom keeps the contract obvious.
	clear(msg.systemBytes[:])
	copy(msg.systemBytes[:], systemBytes)

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
	msg.stream = header[2] // already packed: MSB = waitBit, low 7 = stream
	msg.function = header[3]
	clear(msg.systemBytes[:])
	copy(msg.systemBytes[:], header[6:10])

	return nil
}

// SetStreamCode sets the stream code of the data message.
// It will return error if the stream code is invalid.
//
// Added in v1.12.0
func (msg *DataMessage) SetStreamCode(stream uint8) error {
	if stream > MaxStreamCode {
		return ErrInvalidStreamCode
	}

	msg.stream = (msg.stream & waitBitMask) | (stream & 0x7F)

	return nil
}

// StreamCode returns the stream code of the data message.
//
// It implements the StreamCode method of the secs2.SECS2Message interface.
func (msg *DataMessage) StreamCode() uint8 {
	return msg.stream & 0x7F
}

// SetFunctionCode sets the function code of the data message.
//
// Added in v1.12.0
func (msg *DataMessage) SetFunctionCode(function uint8) {
	msg.function = function
}

// FunctionCode returns the function code of the data message.
//
// It implements the FunctionCode method of the secs2.SECS2Message interface.
func (msg *DataMessage) FunctionCode() uint8 {
	return msg.function
}

// SetWaitBit sets the wait bit of the data message.
//
// Added in v1.12.0
func (msg *DataMessage) SetWaitBit(wait bool) {
	if wait {
		msg.stream |= waitBitMask
	} else {
		msg.stream &^= waitBitMask
	}
}

// WaitBit returnes the boolean representation to indicates WBit is set
//
// It implements the WaitBit method of the secs2.SECS2Message interface.
func (msg *DataMessage) WaitBit() bool {
	return msg.stream&waitBitMask != 0
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
	header := fmt.Sprintf("%sS%dF%d%s", quote, msg.StreamCode(), msg.function, quote)

	if msg.WaitBit() {
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
		if !atomic.CompareAndSwapUint32(&msg.freed, 0, 1) {
			return
		}
		debugLogFree(msg)
		item := msg.Item()
		if item != nil {
			item.Free()
		}
		putDataMessage(msg)
	}
}

// Clone returns an independent deep copy of the message.
//
// The clone shares no mutable state with the original: the system bytes are
// value-copied (no shared backing array) and the data item is deep-cloned via
// secs2.Item.Clone. A nil data item becomes an empty item in the clone. This
// makes the result safe to hand to a concurrent consumer (for example a fan-out
// or broadcast path) and to mutate independently of the source.
//
// The clone is a fresh instance, not drawn from the message pool, with its freed
// state reset; calling Free on it returns it (and its item) to the pool. The
// source message's error (see SetError) is not propagated to the clone.
//
// It implements the Clone method of the HSMSMessage interface.
//
// Returns:
//   - HSMSMessage: a new *DataMessage independent of the receiver.
func (msg *DataMessage) Clone() HSMSMessage {
	cloned := &DataMessage{
		stream:      msg.stream,
		function:    msg.function,
		sessionID:   msg.sessionID,
		systemBytes: msg.systemBytes, // array value-copy: no shared backing
	}

	if msg.dataItem == nil {
		cloned.dataItem = secs2.NewEmptyItem()
	} else {
		cloned.dataItem = msg.dataItem.Clone()
	}

	return cloned
}

// CloneCodec returns an independent deep copy of the message typed only as a
// binary codec (encoding.BinaryMarshaler + encoding.BinaryUnmarshaler).
//
// It is a thin wrapper over [DataMessage.Clone]; the deep-copy and concurrency
// guarantees documented there apply unchanged. Its sole purpose is the return
// type: a caller that knows only the codec contract — not the concrete
// *DataMessage type — can obtain an independent copy without importing this
// package, which lets external code declare a matching capability interface and
// satisfy it covariantly.
//
// Because the copy is deep (the data item, and hence its lazily populated
// serialized-bytes cache, is cloned), serializing the result is safe to do
// concurrently with serializing the original; neither observes the other's
// cache writes.
//
// Returns:
//   - a new *DataMessage, independent of the receiver, exposed as the codec pair.
func (msg *DataMessage) CloneCodec() interface {
	encoding.BinaryMarshaler
	encoding.BinaryUnmarshaler
} {
	return msg.Clone()
}

// SnapshotForRelay returns an independent, relay-optimized snapshot of the
// message. It serializes the message once via ToBytes and serves those bytes
// from the returned message's ToBytes without re-encoding; the item structure is
// decoded lazily only if a structural method (Item, ToDataMessage, …) is called
// on the result.
//
// Use it only for the relay / broadcast / shadow path — clone then
// serialize-and-forward, without inspecting the item structure. For that pattern
// it is dramatically cheaper than Clone (no structural deep copy, no re-encode).
// Use Clone for a structural deep copy: if the consumer accesses items,
// SnapshotForRelay is slower than Clone, because it pays serialize-at-clone plus
// lazy-decode-at-access where Clone simply deep-copies the live item tree.
//
// Item() on the returned message yields a lazily-decoded item — use the
// secs2.Item interface, not concrete-type assertions, to remain compatible with
// future implementations.
//
// Returns an HSMSMessage backed by an internal snapshot type. The dynamic type
// is not *DataMessage: a consumer that needs a concrete *DataMessage (for
// example the in-process handler fan-out path, whose channels carry
// *DataMessage) must call ToDataMessage, which is more expensive than Clone.
//
// The returned message is intended for a single consumer; create one snapshot
// per concurrent recipient.
//
// Lifetime: unlike DataMessage.ToBytes (which allocates a fresh buffer each
// call), the returned message's ToBytes may return its internal frame buffer
// directly until structure is accessed. Treat the ToBytes result as read-only
// and do not retain it for mutation.
func (msg *DataMessage) SnapshotForRelay() HSMSMessage {
	return &snapshotMessage{frame: msg.ToBytes()} // ToBytes returns a fresh owned buffer
}

func (msg *DataMessage) generateHeader(header []byte) {
	// Header byte 0-1: session(device) ID
	header[0] = byte(msg.sessionID >> 8)
	header[1] = byte(msg.sessionID)

	// Header byte 2-3: wait bit + stream code (already packed in stream field), function code
	header[2] = msg.stream
	header[3] = msg.function

	// Header byte 4-5: PType, SType, should set to zero for data message
	header[4] = 0
	header[5] = 0
	// Header byte 6-9: system bytes
	copy(header[6:], msg.systemBytes[:4])
}

func (msg *DataMessage) sanityCheck() error {
	if msg.dataItem != nil {
		if err := msg.dataItem.Error(); err != nil {
			return err
		}
	}

	if msg.WaitBit() && msg.function%2 == 0 {
		return ErrInvalidRspMsg
	}

	return nil
}
