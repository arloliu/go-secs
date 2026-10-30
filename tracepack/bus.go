package tracepack

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// Layout 1 of the bus message, which carries one record of a durable-bus capture from its producer to the consumers
// (the tracepack storage specification §4, "Bus message" and "Large records").
const (
	// BusLayout1 is the only layout of the bus message, the value of its Tpk-Layout header.
	BusLayout1 = "1"
	// BusHeaderLayout names the header carrying the message's layout.
	BusHeaderLayout = "Tpk-Layout"
	// BusHeaderMsgID names the header carrying "<capture_id>.<seq>", by which the stream stores a retry once.
	BusHeaderMsgID = "Nats-Msg-Id"
	// BusHeaderDescriptor names the header carrying the capture descriptor, on the message of the capture's start only.
	BusHeaderDescriptor = "Tpk-Capture-Descriptor"
	// BusHeaderPayloadRef names the claim-check header naming the object that holds the payload,
	// "<bucket>/<capture_id>.<seq>".
	BusHeaderPayloadRef = "Tpk-Payload-Ref"
	// BusHeaderPayloadDigest names the claim-check header carrying the payload's digest, "SHA-256=<digest>".
	BusHeaderPayloadDigest = "Tpk-Payload-Digest"
	// BusHeaderBlockLimit is the largest encoded header block of any bus message, in bytes.
	BusHeaderBlockLimit = 4096
	// BusMinMaxPayload is the smallest max_payload a deployment may configure:
	// BusHeaderBlockLimit plus the 44-byte body of a header-only message, so every header-only message fits.
	BusMinMaxPayload = 4140
)

// Sentinel errors of the bus codec.
var (
	// ErrBusMalformed reports a bus message a consumer never writes, whose seq is a seq gap:
	// an unknown layout, a body not of the layout's length, a malformed Nats-Msg-Id or one naming seq 2^63-1
	// or disagreeing with the record header, or malformed claim-check headers
	// (the tracepack storage specification §4, "Bus message").
	// A producer's encoder and helpers return it for a message they cannot form within those rules.
	ErrBusMalformed = errors.New("tracepack: malformed bus message")
	// ErrBusNotAccepted reports a capture's start that a consumer cannot accept:
	// its descriptor header is missing, not standard base64 with padding, or does not decode (see DecodeDescriptor).
	// The consumer writes nothing for it, raises an alert, and defers the capture's later records until an operator acts
	// (the tracepack storage specification §4, "Bus message").
	ErrBusNotAccepted = errors.New("tracepack: bus start not accepted")
	// ErrBusPayloadMismatch reports a claim-check object whose length or SHA-256 digest differs from its message's:
	// the consumer defers the message and writes nothing (the tracepack storage specification §4, "Large records").
	ErrBusPayloadMismatch = errors.New("tracepack: claim-check object does not match its message")
)

// busPayloadDigestPrefix starts the value of the Tpk-Payload-Digest header.
const busPayloadDigestPrefix = "SHA-256="

// busMaxSeq is the largest seq a producer assigns: 2^63-1 is reserved for a consumer's close.
const busMaxSeq = format.MaxU64 - 1

// BusEnvelope holds the headers of one bus message as received, each empty when absent.
type BusEnvelope struct {
	// Layout is the Tpk-Layout header.
	Layout string
	// MsgID is the Nats-Msg-Id header.
	MsgID string
	// Descriptor is the Tpk-Capture-Descriptor header: the capture descriptor in standard base64 with padding.
	Descriptor string
	// PayloadRef is the Tpk-Payload-Ref header of a claim-check message.
	PayloadRef string
	// PayloadDigest is the Tpk-Payload-Digest header of a claim-check message.
	PayloadDigest string
}

// BusRecord is one record of a bus message:
// the record a consumer received, normalized as CheckBusMessage describes,
// and what the message says about it beyond the record header.
// EncodeBusBody encodes it, so decoding and encoding are symmetric.
type BusRecord struct {
	// Record is the record: its header fields, and its payload when the message carries it inline
	// or once CheckBusPayload has checked the fetched object.
	Record Record
	// CaptureID is the capture the message's Nats-Msg-Id names.
	CaptureID UUID
	// PayloadLen is the payload_len the record header declares, which a claim-check record keeps without a payload.
	PayloadLen uint32
	// ClaimCheck reports that the payload travels as an object by claim-check, so the body is the header alone.
	ClaimCheck bool
	// Bucket is the object bucket the Tpk-Payload-Ref header of a claim-check message names.
	Bucket string
	// Digest is the SHA-256 digest of the payload the Tpk-Payload-Digest header of a claim-check message carries.
	Digest [32]byte
	// Descriptor is the capture descriptor of a start message, decoded from its header's base64,
	// which DecodeDescriptor decodes; nil for every other record.
	Descriptor []byte

	// descriptorHeader is the Tpk-Capture-Descriptor header of a claim-check message,
	// checked by CheckBusPayload once the payload shows whether the record is a start.
	descriptorHeader string
}

// BusToolToken returns the tool token of toolID, the subject token that names its tool
// (the tracepack storage specification §4, "Bus message"):
// toolID escaped as a key escapes it, with '.' also written %2E, so every byte outside A-Z, a-z, 0-9, '-', '_' and '~'
// is %XX with uppercase hex digits and the token holds no '.', '*', '>', whitespace or unescaped '%'.
//
// Returns:
//   - string: the token; empty on error.
//   - error: an error wrapping ErrBusMalformed for a toolID that is empty, "." or "..", which no key can hold.
func BusToolToken(toolID string) (string, error) {
	if err := checkKeyTool(toolID); err != nil {
		return "", fmt.Errorf("%w: %w", ErrBusMalformed, err)
	}

	var b strings.Builder
	for i := range len(toolID) {
		c := toolID[i]
		if isBusTokenByte(c) {
			b.WriteByte(c)

			continue
		}
		b.WriteByte('%')
		b.WriteByte(upperHex[c>>4])
		b.WriteByte(upperHex[c&0x0F])
	}

	return b.String(), nil
}

// ParseBusToolToken returns the tool_id a tool token spells, accepting only the one spelling BusToolToken writes.
//
// Returns:
//   - string: the tool_id; empty on error.
//   - error: an error wrapping ErrBusMalformed for a byte that is neither kept nor an escape,
//     an escape that is cut short, not two uppercase hex digits, or encodes a byte the token keeps,
//     or a tool_id that is empty, "." or "..".
func ParseBusToolToken(token string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(token); i++ {
		c := token[i]
		if isBusTokenByte(c) {
			b.WriteByte(c)

			continue
		}
		if c != '%' {
			return "", fmt.Errorf("%w: tool token %q: byte %#02x at %d is neither kept nor an escape", ErrBusMalformed, token, c, i)
		}
		if i+2 >= len(token) {
			return "", fmt.Errorf("%w: tool token %q: escape at %d is cut short", ErrBusMalformed, token, i)
		}
		hi, lo := strings.IndexByte(upperHex, token[i+1]), strings.IndexByte(upperHex, token[i+2])
		if hi < 0 || lo < 0 {
			return "", fmt.Errorf("%w: tool token %q: escape at %d is not two uppercase hex digits", ErrBusMalformed, token, i)
		}
		v := byte(hi<<4 | lo) // two hex digits, below 256
		if isBusTokenByte(v) {
			return "", fmt.Errorf("%w: tool token %q: escape at %d encodes the kept byte %q", ErrBusMalformed, token, i, v)
		}
		b.WriteByte(v)
		i += 2
	}

	tool := b.String()
	if err := checkKeyTool(tool); err != nil {
		return "", fmt.Errorf("%w: %w", ErrBusMalformed, err)
	}

	return tool, nil
}

// BusSubject returns the subject a capture's records are published on, "<prefix>.<tool token>.<capture_id>"
// (the tracepack storage specification §4, "Bus message").
//
// Parameters:
//   - prefix: the deployment's subject prefix, one or more '.'-separated tokens,
//     none empty and none holding whitespace, a control byte, '*' or '>'.
//   - toolID: the capture's tool_id, written as BusToolToken writes it.
//   - capture: the capture_id, in canonical lowercase form.
//
// Returns:
//   - string: the subject; empty on error.
//   - error: an error wrapping ErrBusMalformed for a prefix outside the rule above or a toolID BusToolToken refuses.
func BusSubject(prefix, toolID string, capture UUID) (string, error) {
	for tok := range strings.SplitSeq(prefix, ".") {
		if tok == "" || strings.ContainsFunc(tok, func(r rune) bool { return r <= ' ' || r == 0x7F || r == '*' || r == '>' }) {
			return "", fmt.Errorf("%w: subject prefix %q", ErrBusMalformed, prefix)
		}
	}
	token, err := BusToolToken(toolID)
	if err != nil {
		return "", err
	}

	return prefix + "." + token + "." + capture.String(), nil
}

// BusMsgID returns the Nats-Msg-Id of the record seq of capture, "<capture_id>.<seq>",
// seq in decimal without a leading zero (the tracepack storage specification §4, "Bus message").
//
// Returns:
//   - string: the id; empty on error.
//   - error: an error wrapping ErrBusMalformed for seq 2^63-1, which no producer assigns, or a larger one.
func BusMsgID(capture UUID, seq uint64) (string, error) {
	if seq > busMaxSeq {
		return "", fmt.Errorf("%w: seq %d is not a producer's (at most %d)", ErrBusMalformed, seq, busMaxSeq)
	}

	return capture.String() + "." + strconv.FormatUint(seq, 10), nil
}

// ParseBusMsgID returns the capture and seq a Nats-Msg-Id names, accepting only the form BusMsgID writes:
// the canonical lowercase capture_id, '.', and the seq in decimal without a sign or a leading zero.
//
// Returns:
//   - UUID: the capture_id; the nil UUID on error.
//   - uint64: the seq; 0 on error.
//   - error: an error wrapping ErrBusMalformed for any other form, or a seq of 2^63-1 or more.
func ParseBusMsgID(s string) (UUID, uint64, error) {
	const uuidLen = 36
	if len(s) < uuidLen+2 || s[uuidLen] != '.' {
		return UUID{}, 0, fmt.Errorf("%w: %s %q is not <capture_id>.<seq>", ErrBusMalformed, BusHeaderMsgID, s)
	}
	capture, err := parseKeyUUID(s[:uuidLen])
	if err != nil {
		return UUID{}, 0, fmt.Errorf("%w: %s %q: %w", ErrBusMalformed, BusHeaderMsgID, s, err)
	}
	digits := s[uuidLen+1:]
	seq, err := strconv.ParseUint(digits, 10, 64)
	if err != nil || (digits[0] == '0' && len(digits) > 1) {
		return UUID{}, 0, fmt.Errorf("%w: %s %q: seq is not decimal without a leading zero", ErrBusMalformed, BusHeaderMsgID, s)
	}
	if seq > busMaxSeq {
		return UUID{}, 0, fmt.Errorf("%w: %s %q: seq %d is not a producer's", ErrBusMalformed, BusHeaderMsgID, s, seq)
	}

	return capture, seq, nil
}

// NewBusRecord returns the inline BusRecord of r a producer encodes:
// a copy of r, with PayloadLen the length of its payload.
// A producer that sends r by claim-check then sets ClaimCheck, Bucket and Digest, and keeps PayloadLen.
// A payload longer than 2^32-1 bytes, which no record holds, gives PayloadLen 2^32-1, which EncodeBusBody refuses.
func NewBusRecord(r *Record) BusRecord {
	n := uint64(len(r.Payload))

	return BusRecord{Record: *r, PayloadLen: uint32(min(n, math.MaxUint32))}
}

// EncodeBusBody returns the body of layout 1 of br's message (the tracepack storage specification §4, "Bus message"):
// the 44-byte record header of the tracepack format specification §7.1, its fields in layout order,
// then the payload inline, or nothing when br.ClaimCheck is set.
//
// The header holds br.Record's seq, ts_utc_ns, mono_ns, epoch, kind, dir and fidelity as they are,
// record_flags from MonoPresent, quality kept to its defined bits, payload_len from br.PayloadLen,
// and decode_status, trailing_bytes and field_validity zero, since the consumer derives them;
// so encoding a record CheckBusMessage returned gives its normalized body.
// A claim-check body needs no payload in memory: a payload br.Record holds is not written and not checked.
//
// Returns:
//   - []byte: the body; nil on error.
//   - error: an error wrapping ErrBusMalformed for a record carrying quality.redacted, which a producer never sets,
//     a seq of 2^63-1 or more, a payload_len above 2^31-1,
//     or an inline record whose PayloadLen differs from its payload's length.
func EncodeBusBody(br *BusRecord) ([]byte, error) {
	r := &br.Record
	if r.Quality.Has(QualityRedacted) {
		return nil, fmt.Errorf("%w: seq %d carries quality.redacted, which a producer never sets", ErrBusMalformed, r.Seq)
	}
	if r.Seq > busMaxSeq {
		return nil, fmt.Errorf("%w: seq %d is not a producer's (at most %d)", ErrBusMalformed, r.Seq, busMaxSeq)
	}
	if br.PayloadLen > format.MaxLen32 {
		return nil, fmt.Errorf("%w: payload_len %d exceeds %d", ErrBusMalformed, br.PayloadLen, format.MaxLen32)
	}
	if !br.ClaimCheck && uint64(br.PayloadLen) != uint64(len(r.Payload)) {
		return nil, fmt.Errorf("%w: payload_len %d, payload of %d bytes", ErrBusMalformed, br.PayloadLen, len(r.Payload))
	}

	h := format.RecordHeader{
		Seq:        r.Seq,
		TSUTCNs:    r.TSUTCNs,
		MonoNs:     r.MonoNs,
		Epoch:      r.Epoch,
		PayloadLen: br.PayloadLen,
		Quality:    uint16(r.Quality),
		Kind:       uint8(r.Kind),
		Dir:        uint8(r.Dir),
		Fidelity:   uint8(r.Fidelity),
	}
	if r.MonoPresent {
		h.RecordFlags = uint8(recordFlagMonoPresent)
	}

	n := format.RecordHeaderLen
	if !br.ClaimCheck {
		var err error
		if n, err = busInlineBodyLen(len(r.Payload)); err != nil {
			return nil, err
		}
	}
	body := format.AppendRecordHeader(make([]byte, 0, n), &h)
	if !br.ClaimCheck {
		body = append(body, r.Payload...)
	}

	return body, nil
}

// CheckBusMessage checks one bus message of layout 1, its headers env and its body,
// as a consumer receives it (the tracepack storage specification §4, "Bus message" and "Large records"),
// and returns its record.
//
// The message is malformed, never written and its seq a seq gap, when
// its Tpk-Layout is not "1";
// its body is not 44 + payload_len bytes long without Tpk-Payload-Ref, or 44 with it;
// its Nats-Msg-Id is not of the form ParseBusMsgID accepts, names seq 2^63-1, or disagrees with the body's seq;
// its Tpk-Payload-Ref is not "<bucket>/<Nats-Msg-Id>" with a non-empty bucket;
// its Tpk-Payload-Digest is missing beside Tpk-Payload-Ref, present without it,
// or not "SHA-256=" followed by the padded base64url encoding (RFC 4648 §5) of 32 bytes.
//
// The record is returned normalized: decode_status, trailing_bytes and field_validity are zero,
// whatever the producer wrote, since the consumer derives them (DeriveBusRecord);
// quality is kept to its defined bits and record_flags to mono_present;
// every other field is as decoded, mono_ns included when mono_present is clear.
// A body is not refused for those bits.
// The payload of an inline message aliases body; a claim-check record has none until CheckBusPayload sets it.
//
// The record is a start when it is a transport event whose payload decodes to a capture-boundary event of kind start.
// A start the consumer cannot accept returns ErrBusNotAccepted, never ErrBusMalformed:
// its Tpk-Capture-Descriptor is missing, not standard base64 with padding (RFC 4648 §4), or fails DecodeDescriptor.
// The decoded descriptor bytes are then BusRecord.Descriptor.
// The header on any other record is ignored.
// A claim-check record's payload is not known yet, so CheckBusPayload makes the start check for it.
//
// Returns:
//   - BusRecord: the record; the zero BusRecord on error.
//   - error: an error wrapping ErrBusMalformed or ErrBusNotAccepted.
func CheckBusMessage(env BusEnvelope, body []byte) (BusRecord, error) {
	if env.Layout != BusLayout1 {
		return BusRecord{}, fmt.Errorf("%w: %s %q is not a known layout", ErrBusMalformed, BusHeaderLayout, env.Layout)
	}
	if len(body) < format.RecordHeaderLen {
		return BusRecord{}, fmt.Errorf("%w: body of %d bytes, shorter than the record header", ErrBusMalformed, len(body))
	}
	h, err := format.UnmarshalRecordHeader(body, format.RecordHeaderLen)
	if err != nil {
		return BusRecord{}, fmt.Errorf("%w: %w", ErrBusMalformed, err)
	}
	capture, seq, err := ParseBusMsgID(env.MsgID)
	if err != nil {
		return BusRecord{}, err
	}
	if seq != h.Seq {
		return BusRecord{}, fmt.Errorf("%w: %s names seq %d, the record header %d", ErrBusMalformed, BusHeaderMsgID, seq, h.Seq)
	}

	br := BusRecord{CaptureID: capture, PayloadLen: h.PayloadLen, ClaimCheck: env.PayloadRef != ""}
	if err := br.checkBody(env, len(body)); err != nil {
		return BusRecord{}, err
	}

	var payload []byte
	if !br.ClaimCheck {
		payload = body[format.RecordHeaderLen:]
	}
	br.Record = storedRecord(&h, payload)
	br.Record.DecodeStatus, br.Record.TrailingBytes, br.Record.FieldValidity = 0, 0, 0
	br.Record.Quality &= qualityMask

	if br.ClaimCheck {
		br.descriptorHeader = env.Descriptor

		return br, nil
	}
	if err := br.checkStart(env.Descriptor); err != nil {
		return BusRecord{}, err
	}

	return br, nil
}

// checkBody checks that a body of bodyLen bytes has the layout's length for br.PayloadLen,
// and, for a claim-check message, its claim-check headers in env, setting br.Bucket and br.Digest from them.
func (br *BusRecord) checkBody(env BusEnvelope, bodyLen int) error {
	if !br.ClaimCheck {
		if env.PayloadDigest != "" {
			return fmt.Errorf("%w: %s without %s", ErrBusMalformed, BusHeaderPayloadDigest, BusHeaderPayloadRef)
		}
		if uint64(bodyLen) != uint64(format.RecordHeaderLen)+uint64(br.PayloadLen) {
			return fmt.Errorf("%w: inline body of %d bytes, payload_len %d", ErrBusMalformed, bodyLen, br.PayloadLen)
		}

		return nil
	}

	if bodyLen != format.RecordHeaderLen {
		return fmt.Errorf("%w: claim-check body of %d bytes, not the record header alone", ErrBusMalformed, bodyLen)
	}
	bucket, object, found := strings.Cut(env.PayloadRef, "/")
	if !found || bucket == "" || object != env.MsgID {
		return fmt.Errorf("%w: %s %q does not name the object %s/%s", ErrBusMalformed, BusHeaderPayloadRef, env.PayloadRef, "<bucket>", env.MsgID)
	}
	digest, err := parseBusPayloadDigest(env.PayloadDigest)
	if err != nil {
		return err
	}
	br.Bucket, br.Digest = bucket, digest

	return nil
}

// parseBusPayloadDigest decodes a Tpk-Payload-Digest value: "SHA-256=" and the padded base64url encoding of 32 bytes.
func parseBusPayloadDigest(s string) ([32]byte, error) {
	enc, ok := strings.CutPrefix(s, busPayloadDigestPrefix)
	// base64 decoding skips line breaks, which the one spelling of a digest never holds.
	if !ok || strings.ContainsAny(enc, "\r\n") {
		return [32]byte{}, fmt.Errorf("%w: %s %q is not %s<digest>", ErrBusMalformed, BusHeaderPayloadDigest, s, busPayloadDigestPrefix)
	}
	b, err := base64.URLEncoding.Strict().DecodeString(enc)
	if err != nil || len(b) != sha256.Size {
		return [32]byte{}, fmt.Errorf("%w: %s %q does not decode to 32 bytes", ErrBusMalformed, BusHeaderPayloadDigest, s)
	}

	return [32]byte(b), nil
}

// checkStart checks the descriptor header of br's record when the record is a start,
// setting br.Descriptor to the decoded descriptor, and ignores the header on any other record.
func (br *BusRecord) checkStart(header string) error {
	if !isBusStart(&br.Record) {
		return nil
	}
	if header == "" {
		return fmt.Errorf("%w: start without %s", ErrBusNotAccepted, BusHeaderDescriptor)
	}
	// base64 decoding skips line breaks, which standard base64 with padding never holds.
	if strings.ContainsAny(header, "\r\n") {
		return fmt.Errorf("%w: %s holds a line break", ErrBusNotAccepted, BusHeaderDescriptor)
	}
	desc, err := base64.StdEncoding.Strict().DecodeString(header)
	if err != nil {
		return fmt.Errorf("%w: %s is not standard base64 with padding: %w", ErrBusNotAccepted, BusHeaderDescriptor, err)
	}
	if _, err := DecodeDescriptor(desc); err != nil {
		return fmt.Errorf("%w: %w", ErrBusNotAccepted, err)
	}
	br.Descriptor = desc

	return nil
}

// isBusStart reports whether r is a capture's start:
// a transport event whose payload decodes to a capture-boundary event of kind start.
func isBusStart(r *Record) bool {
	if r.Kind != KindTransportEvent {
		return false
	}
	ev, err := UnmarshalTransportEvent(r.Payload)

	return err == nil && ev.Event == EventCaptureBoundary && ev.BoundaryKind != nil && *ev.BoundaryKind == BoundaryKindStart
}

// BusPayloadDigest returns the Tpk-Payload-Digest value of payload:
// "SHA-256=" and the padded base64url encoding (RFC 4648 §5, 44 characters) of its SHA-256 digest,
// the form the NATS object store records (the tracepack storage specification §4, "Large records").
func BusPayloadDigest(payload []byte) string {
	sum := sha256.Sum256(payload)

	return busPayloadDigestPrefix + base64.URLEncoding.EncodeToString(sum[:])
}

// CheckBusPayload checks the object fetched for the claim-check record br, payload,
// against the length and digest its message declares (the tracepack storage specification §4, "Large records"),
// and on a match sets br.Record.Payload to payload, which it aliases.
//
// It then makes the start check CheckBusMessage makes for an inline record:
// when the record is a start, its message's Tpk-Capture-Descriptor must decode,
// and br.Descriptor is set to the decoded descriptor.
//
// Returns:
//   - error: an error wrapping ErrBusPayloadMismatch for an object of another length or digest,
//     on which the consumer defers the message, and br is left unchanged;
//     an error wrapping ErrBusNotAccepted for a start whose descriptor CheckBusMessage would refuse,
//     with br.Record.Payload set to the checked object and br.Descriptor nil;
//     or an error for a record that is not claim-check.
func CheckBusPayload(br *BusRecord, payload []byte) error {
	if !br.ClaimCheck {
		return errors.New("tracepack: bus record is not claim-check")
	}
	if uint64(len(payload)) != uint64(br.PayloadLen) {
		return fmt.Errorf("%w: object of %d bytes, payload_len %d", ErrBusPayloadMismatch, len(payload), br.PayloadLen)
	}
	if sha256.Sum256(payload) != br.Digest {
		return fmt.Errorf("%w: object digest differs from %s", ErrBusPayloadMismatch, BusHeaderPayloadDigest)
	}

	br.Record.Payload = payload

	return br.checkStart(br.descriptorHeader)
}

// busInlineBodyLen returns the length of an inline body holding a payload of payloadLen bytes,
// or an error when that length does not fit an int, as on a 32-bit platform near 2^31 bytes.
func busInlineBodyLen(payloadLen int) (int, error) {
	if payloadLen > math.MaxInt-format.RecordHeaderLen {
		return 0, fmt.Errorf("%w: an inline body of a %d-byte payload does not fit in memory", ErrBusMalformed, payloadLen)
	}

	return format.RecordHeaderLen + payloadLen, nil
}

// BusInline reports whether a record whose body is bodyLen bytes long travels inline under the deployment's maxPayload:
// when the body is at most maxPayload − 4096 bytes; otherwise it travels by claim-check
// (the tracepack storage specification §4, "Large records").
// A maxPayload below 4140 admits no message, so nothing travels inline under it.
func BusInline(bodyLen, maxPayload int) bool {
	return maxPayload >= BusMinMaxPayload && bodyLen <= maxPayload-BusHeaderBlockLimit
}

// CheckBusStart is the producer's preflight of a bus message, a capture's start before it starts the capture:
// it checks the encoded header block against 4096 bytes and the whole message against the deployment's max_payload
// (the tracepack storage specification §4, "Bus message").
//
// The bound on the header block holds for every message, not only a start:
// the claim-check headers count, so a long bucket name in Tpk-Payload-Ref can take any message over it,
// and a producer checks every message it encodes with CheckBusStart.
// The module imports no NATS client, so the producer computes headerBlockLen as its client encodes the header block.
//
// Parameters:
//   - headerBlockLen: the length of the message's encoded header block.
//   - bodyLen: the length of its body, 44 bytes or more.
//   - maxPayload: the deployment's max_payload.
//
// Returns:
//   - error: an error wrapping ErrBusMalformed for a maxPayload below 4140, a header block above 4096 bytes,
//     a message longer than maxPayload, or a negative length or a body shorter than 44 bytes.
func CheckBusStart(headerBlockLen, bodyLen, maxPayload int) error {
	switch {
	case maxPayload < BusMinMaxPayload:
		return fmt.Errorf("%w: max_payload %d is below %d", ErrBusMalformed, maxPayload, BusMinMaxPayload)
	case headerBlockLen < 0 || bodyLen < format.RecordHeaderLen:
		return fmt.Errorf("%w: header block of %d bytes, body of %d bytes", ErrBusMalformed, headerBlockLen, bodyLen)
	case headerBlockLen > BusHeaderBlockLimit:
		return fmt.Errorf("%w: header block of %d bytes exceeds %d", ErrBusMalformed, headerBlockLen, BusHeaderBlockLimit)
	case bodyLen > maxPayload-headerBlockLen: // no overflow: maxPayload is at least 4140 and headerBlockLen at most 4096
		return fmt.Errorf("%w: header block of %d bytes and body of %d bytes exceed max_payload %d", ErrBusMalformed, headerBlockLen, bodyLen, maxPayload)
	}

	return nil
}

// DeriveBusRecord sets the values a consumer derives for r, a record CheckBusMessage returned with its payload,
// by kind (the tracepack storage specification §4, "Bus message"):
// for a data or control record not carrying quality.unobserved,
// field_validity from the frame's captured length (the tracepack format specification §7.2),
// and decode_status and trailing_bytes from cls, the capture's classifier, or not-attempted and 0 when cls is nil;
// for a data record carrying quality.unobserved, field_validity from its captured header,
// decode_status not-attempted and trailing_bytes 0 (the tracepack semantics specification §2);
// for a transport-event or annotation record, field_validity 0, decode_status not-applicable and trailing_bytes 0.
// A claim-check record is derived once CheckBusPayload has set its payload.
//
// Returns:
//   - error: an error wrapping ErrInvalidRecord for a kind outside the four,
//     or a classifier's trailing_bytes outside 0 to 2^32-1; r is unchanged then.
func DeriveBusRecord(r *Record, cls Classifier) error {
	if r.Kind == KindTransportEvent || r.Kind == KindAnnotation {
		r.FieldValidity, r.DecodeStatus, r.TrailingBytes = 0, DecodeStatusNotApplicable, 0

		return nil
	}
	if !r.hasHSMSFrame() {
		return fmt.Errorf("tracepack: bus record of kind %s: %w", r.Kind, ErrInvalidRecord)
	}

	status, trailing := DecodeStatusNotAttempted, uint32(0)
	if cls != nil && (r.Kind != KindData || !r.Quality.Has(QualityUnobserved)) {
		s, n := cls.Frame(r.Payload)
		status = s
		if s == DecodeStatusOK || s == DecodeStatusOKWithTrailing {
			if n < 0 || uint64(n) > math.MaxUint32 {
				return fmt.Errorf("tracepack: classifier trailing_bytes %d outside 0 to %d: %w", n, uint32(math.MaxUint32), ErrInvalidRecord)
			}
			trailing = uint32(n)
		}
	}
	r.SetCapturedFieldValidity()
	r.DecodeStatus, r.TrailingBytes = status, trailing

	return nil
}

// isBusTokenByte reports whether a tool token keeps c unescaped: A-Z, a-z, 0-9, '-', '_' and '~'.
func isBusTokenByte(c byte) bool {
	return c != '.' && isUnreserved(c)
}
