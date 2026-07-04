package hsms

import (
	"encoding/binary"
	"fmt"
	"sync"

	"github.com/arloliu/go-secs/v2/internal/framecodec"
	"github.com/arloliu/go-secs/v2/internal/wire"
	"github.com/arloliu/go-secs/v2/secs2"
)

// MaxStreamCode is the maximum valid HSMS stream code (SEMI E37 §7.5).
const MaxStreamCode = uint8(127)

// decodeState holds the lazily-decoded SECS-II item and any decode error for a DataMessage.
//
// It is allocated on the heap and shared across [DataMessage.WithSessionID] and
// [DataMessage.WithSystemBytes] copies so that the embedded [sync.Once] is never
// copied (copying a sync.Once has undefined behaviour and is caught by go vet).
//
// For tree-path (constructed) messages [DataMessage.dec].once is pre-fired during
// [NewDataMessage] so that [DataMessage.Item] returns the pre-seeded item without
// re-encoding. For raw-frame messages (added in a later task) the once fires on
// the first call to [DataMessage.Item].
type decodeState struct {
	once sync.Once
	item secs2.Item
	err  error
}

// DataMessage is an immutable HSMS data message carrying a SECS-II item body.
//
// The header is stored as a [10]byte value and the body is referenced via a
// [wire.Body] interface; [DataMessage.WithSessionID] and [DataMessage.WithSystemBytes]
// return a new *DataMessage that shares the same body and [decodeState] pointer —
// the body is never copied and the SECS-II item is decoded at most once across all
// derived copies.
//
// DataMessage is safe for concurrent use after construction.
type DataMessage struct {
	header [10]byte
	body   wire.Body
	dec    *decodeState
}

// Ensure *DataMessage satisfies the Message interface.
var _ Message = (*DataMessage)(nil)

// ────────────────────────────────────────────────────────────────
// Message interface implementation
// ────────────────────────────────────────────────────────────────

// Type returns [DataMsgType] for all DataMessage instances.
func (msg *DataMessage) Type() MsgType { return DataMsgType }

// SessionID returns the session ID encoded in header bytes 0–1 (big-endian).
func (msg *DataMessage) SessionID() uint16 {
	return binary.BigEndian.Uint16(msg.header[0:2])
}

// SystemBytes returns a copy of header bytes 6–9 as a [4]byte value.
// The returned array is an independent copy; callers may retain it freely.
func (msg *DataMessage) SystemBytes() [4]byte {
	var sb [4]byte
	copy(sb[:], msg.header[6:10])

	return sb
}

// HeaderBytes returns a copy of the full 10-byte HSMS header as a value.
// The returned array is an independent copy; callers may retain it freely.
func (msg *DataMessage) HeaderBytes() [10]byte {
	return msg.header
}

// ToBytes serializes the message to its on-wire representation:
// a 4-byte big-endian length prefix (= 10 + body length) followed by the
// 10-byte header and the encoded SECS-II body. Performs exactly one allocation.
func (msg *DataMessage) ToBytes() []byte {
	n := msg.body.Len()
	dst := make([]byte, 0, 4+10+n)
	length := uint32(10 + n)
	dst = append(dst,
		byte(length>>24),
		byte(length>>16),
		byte(length>>8),
		byte(length),
	)
	dst = append(dst, msg.header[:]...)

	return msg.body.AppendTo(dst)
}

// ────────────────────────────────────────────────────────────────
// DataMessage-specific accessors
// ────────────────────────────────────────────────────────────────

// Stream returns the stream code stored in the low 7 bits of header byte 2.
func (msg *DataMessage) Stream() uint8 { return msg.header[2] & 0x7F }

// Function returns the function code stored in header byte 3.
func (msg *DataMessage) Function() uint8 { return msg.header[3] }

// WaitBit reports whether the wait bit (MSB of header byte 2) is set,
// indicating that a reply is expected.
func (msg *DataMessage) WaitBit() bool { return msg.header[2]>>7 != 0 }

// ID returns the message's System Bytes decoded as a uint32 (big-endian), the
// application-level message identifier. Equivalent to
// FromSystemBytes(msg.SystemBytes()).
func (msg *DataMessage) ID() uint32 { return FromSystemBytes(msg.SystemBytes()) }

// NewEmptyDataMessage returns a zero-value DataMessage: stream 0, function 0, no wait bit,
// session ID 0, zero System Bytes, and an empty SECS-II body. It never returns an error and
// exists so tests can construct a baseline message without the six-argument NewDataMessage call
// (DataMessage's fields are unexported, so &DataMessage{} does not compile outside this package).
func NewEmptyDataMessage() *DataMessage {
	msg, _ := NewDataMessage(0, 0, false, 0, [4]byte{}, nil) //nolint:errcheck // arguments are statically valid
	return msg
}

// Item returns the SECS-II item body of this message.
//
// For tree-path (constructed) messages the item is returned directly from the
// pre-seeded [decodeState] without any re-encoding. For raw-frame (wire-decoded)
// messages the body bytes are decoded from wire on the first call; subsequent
// calls return the cached result.
//
// An empty body (zero length) returns ([secs2.NewEmptyItem], nil).
func (msg *DataMessage) Item() (secs2.Item, error) {
	msg.dec.once.Do(msg.decode)

	return msg.dec.item, msg.dec.err
}

// DecodeErr returns any error produced during the lazy body decode.
// It fires the decode once if it has not already run.
// Returns nil for tree-path messages (item is pre-seeded) and for raw-frame
// messages whose body decodes without error.
func (msg *DataMessage) DecodeErr() error {
	msg.dec.once.Do(msg.decode)

	return msg.dec.err
}

// BodyLen returns the byte length of the encoded message body.
func (msg *DataMessage) BodyLen() int { return msg.body.Len() }

// AppendBodyTo appends the encoded body bytes into dst and returns the extended slice.
// No allocation is made when dst has sufficient capacity.
func (msg *DataMessage) AppendBodyTo(dst []byte) []byte { return msg.body.AppendTo(dst) }

// ────────────────────────────────────────────────────────────────
// Immutable mutators
// ────────────────────────────────────────────────────────────────

// WithSessionID returns a new *DataMessage identical to msg except that
// header bytes 0–1 are replaced by id (big-endian). The body and decodeState
// pointer are shared; no body copy or re-encode is performed.
func (msg *DataMessage) WithSessionID(id uint16) *DataMessage {
	n := &DataMessage{header: msg.header, body: msg.body, dec: msg.dec}
	binary.BigEndian.PutUint16(n.header[0:2], id)

	return n
}

// WithSystemBytes returns a new *DataMessage identical to msg except that
// header bytes 6–9 are replaced by b. The body and decodeState pointer are
// shared; no body copy or re-encode is performed.
func (msg *DataMessage) WithSystemBytes(b [4]byte) *DataMessage {
	n := &DataMessage{header: msg.header, body: msg.body, dec: msg.dec}
	n.header[6] = b[0]
	n.header[7] = b[1]
	n.header[8] = b[2]
	n.header[9] = b[3]

	return n
}

// ────────────────────────────────────────────────────────────────
// Builder
// ────────────────────────────────────────────────────────────────

// DataMessageBuilder is a mutable builder for deriving a new [DataMessage] from
// an existing one. Obtain it via [DataMessage.Derive]; call [DataMessageBuilder.Build]
// to produce the derived message after applying overrides.
type DataMessageBuilder struct {
	sessionID   uint16
	systemBytes [4]byte
	stream      uint8
	function    uint8
	waitBit     bool
	item        secs2.Item
}

// Derive returns a new [DataMessageBuilder] seeded with the stream, function,
// wait-bit, item, session ID, and system bytes of msg.
func (msg *DataMessage) Derive() *DataMessageBuilder {
	// Fire dec.once to obtain the item. For tree-path messages the once is
	// pre-fired and item is never nil. For raw-frame messages with a malformed
	// body, secs2.Decode may return (nil, error); the nil guard below ensures
	// the builder always receives a valid item even in that case.
	item, _ := msg.Item()
	if item == nil {
		item = secs2.NewEmptyItem()
	}

	return &DataMessageBuilder{
		sessionID:   msg.SessionID(),
		systemBytes: msg.SystemBytes(),
		stream:      msg.Stream(),
		function:    msg.Function(),
		waitBit:     msg.WaitBit(),
		item:        item,
	}
}

// WithStream sets the stream code for the derived message.
func (b *DataMessageBuilder) WithStream(stream uint8) *DataMessageBuilder {
	b.stream = stream

	return b
}

// WithFunction sets the function code for the derived message.
func (b *DataMessageBuilder) WithFunction(function uint8) *DataMessageBuilder {
	b.function = function

	return b
}

// WithWaitBit sets the wait bit for the derived message.
func (b *DataMessageBuilder) WithWaitBit(wait bool) *DataMessageBuilder {
	b.waitBit = wait

	return b
}

// WithItem sets the SECS-II item body for the derived message.
func (b *DataMessageBuilder) WithItem(item secs2.Item) *DataMessageBuilder {
	b.item = item

	return b
}

// Build constructs and validates a new [DataMessage] using the builder's current
// field values. It runs the full Q3 validation (item error, W-bit vs even function,
// stream range).
func (b *DataMessageBuilder) Build() (*DataMessage, error) {
	return NewDataMessage(b.stream, b.function, b.waitBit, b.sessionID, b.systemBytes, b.item)
}

// ────────────────────────────────────────────────────────────────
// Factory
// ────────────────────────────────────────────────────────────────

// NewDataMessage creates an immutable HSMS data message.
//
// A nil item is treated as an empty body ([secs2.NewEmptyItem]), consistent with
// the decode path where a zero-length body is legal.
//
// Q3 validation (SEMI E37 §8.3.3.3) is performed before construction:
//
//   - item.Error() must be nil, including recursive aggregate errors from list children.
//   - replyExpected may not be true when function is even (W=1 on a reply
//     function is rejected with [ErrInvalidRspMsg]).
//   - stream must be in [0, 127]; values > 127 return [ErrInvalidStreamCode].
//
// On success the returned DataMessage is immutable and safe for concurrent use.
func NewDataMessage(stream, function uint8, replyExpected bool, sessionID uint16, systemBytes [4]byte, item secs2.Item) (*DataMessage, error) {
	if stream > MaxStreamCode {
		return nil, ErrInvalidStreamCode
	}

	// A nil item is treated as an empty body, mirroring the decode path where an
	// empty (zero-length) body is legal and yields secs2.NewEmptyItem. This avoids
	// a nil-interface panic in the item.Error() gate below.
	if item == nil {
		item = secs2.NewEmptyItem()
	}

	if err := item.Error(); err != nil {
		return nil, err
	}

	if replyExpected && function%2 == 0 {
		return nil, ErrInvalidRspMsg
	}

	var header [10]byte

	binary.BigEndian.PutUint16(header[0:2], sessionID)

	header[2] = stream & 0x7F
	if replyExpected {
		header[2] |= 0x80
	}

	header[3] = function
	// header[4] = 0 (PType = SECS-II)
	// header[5] = 0 (SType = data message)
	header[6] = systemBytes[0]
	header[7] = systemBytes[1]
	header[8] = systemBytes[2]
	header[9] = systemBytes[3]

	dec := &decodeState{item: item}
	dec.once.Do(func() {}) // pre-fire: item is already seeded; Item() returns it directly

	return &DataMessage{
		header: header,
		body:   wire.FromItem(item),
		dec:    dec,
	}, nil
}

// NewDataMessageFromHeader builds a DataMessage from an already-formed 10-byte HSMS header and a
// separately-decoded SECS-II item, for callers that have a validated header and want to attach a
// body without re-deriving stream/function/session/System Bytes by hand. The header's PType (byte 4)
// and SType (byte 5) must both be 0 (a SECS-II data message); the stream, function, wait bit,
// session ID, and System Bytes are read from the header and revalidated via the same Q3 rules as
// NewDataMessage. This differs from the raw-frame decode path (decodeOwnedFrame), which does not
// run Q3 validation because it trusts the wire.
func NewDataMessageFromHeader(header [10]byte, item secs2.Item) (*DataMessage, error) {
	if header[4] != 0 {
		return nil, fmt.Errorf("invalid PType: %d: %w", header[4], ErrInvalidPType)
	}
	if header[5] != 0 {
		return nil, fmt.Errorf("expected data message SType 0, got %d: %w", header[5], ErrInvalidControlMsgSType)
	}

	stream := header[2] & 0x7F
	replyExpected := header[2]>>7 != 0
	function := header[3]
	sessionID := binary.BigEndian.Uint16(header[0:2])

	var sb [4]byte
	copy(sb[:], header[6:10])

	return NewDataMessage(stream, function, replyExpected, sessionID, sb, item)
}

// ────────────────────────────────────────────────────────────────
// Internal helpers
// ────────────────────────────────────────────────────────────────

// decode is the payload for [decodeState].once.Do. For tree-path messages the
// once is pre-fired during [NewDataMessage], so this function never executes for
// those messages. For raw-frame messages (a later task) it decodes the body bytes
// exactly once and stores the result in dec.
func (msg *DataMessage) decode() {
	if msg.body.Len() == 0 {
		msg.dec.item = secs2.NewEmptyItem()

		return
	}

	if raw, ok := wire.OwnedBytes(msg.body); ok {
		// Raw-frame path: decode in place over the owned frame body — leaf items
		// alias the frame (kept alive by msg.body), no extra copy (§5.B).
		msg.dec.item, msg.dec.err = secs2.DecodeOwnedFrame(framecodec.AdoptSECS2Body(raw))

		return
	}

	// Fallback (should not occur — tree bodies pre-fire the once): copy-decode.
	msg.dec.item, msg.dec.err = secs2.Decode(msg.body.AppendTo(nil))
}
