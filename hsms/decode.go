package hsms

import (
	"encoding/binary"
	"fmt"

	"github.com/arloliu/go-secs/v2/internal/wire"
	"github.com/arloliu/go-secs/v2/secs2"
)

// maxHSMSMsgLen is the upper bound on the HSMS message-length field: a whole-frame DoS cap
// (10-byte header + body), NOT a per-item bound. It reuses secs2.MaxByteSize (the max SECS-II
// single-item payload, SEMI E5 §9.2) as a convenient, generous ceiling — a value chosen to
// reject an attacker-controlled length before allocating the frame buffer, not to bound the
// SECS-II tree.
//
// Consequence (M6): a LEGITIMATE deeply-nested message whose total on-wire length exceeds this
// cap is rejected at the framing layer (the link is dropped, not answered with a Reject) rather
// than decoded — HSMS has no "message too long" Reject reason, and a frame this large is treated
// as hostile. In practice single-item bodies dominate and stay well under the cap; the header's
// 10 bytes are counted within the cap (the bound is compared against the full msgLen field).
const maxHSMSMsgLen = secs2.MaxByteSize

// DecodeHSMSMessage decodes a complete on-wire HSMS frame from data.
//
// data must contain a 4-byte big-endian length prefix followed by exactly
// msgLen bytes (10-byte header + optional SECS-II body). All length fields are
// validated before any slice operation to prevent panics on malformed input.
//
// DecodeHSMSMessage copies the payload into an owned buffer so the caller may
// reuse or free data immediately after this call returns.
func DecodeHSMSMessage(data []byte) (Message, error) {
	const minFrame = 4 + 10 // length prefix + header

	if len(data) < minFrame {
		return nil, fmt.Errorf("hsms message too short: %d bytes (minimum %d): %w", len(data), minFrame, ErrInvalidHeaderLength)
	}

	msgLen := binary.BigEndian.Uint32(data[0:4])

	if msgLen < 10 {
		return nil, fmt.Errorf("hsms message length field too small: %d (minimum 10): %w", msgLen, ErrInvalidHeaderLength)
	}

	if uint64(msgLen) > uint64(maxHSMSMsgLen) {
		return nil, fmt.Errorf("hsms message length exceeds maximum: %d > %d", msgLen, maxHSMSMsgLen)
	}

	if len(data) != 4+int(msgLen) {
		return nil, fmt.Errorf("hsms message length mismatch, expected: %d, actual: %d: %w", msgLen, len(data)-4, ErrInvalidHeaderLength)
	}

	// Copy data[4:] into an owned buffer so decodeOwnedFrame may retain it
	// zero-copy and the caller's buffer is never aliased.
	owned := append([]byte(nil), data[4:4+msgLen]...)

	return decodeOwnedFrame(owned)
}

// DecodeHSMSPayload decodes an HSMS message from a payload that is a 10-byte header followed by
// the optional SECS-II body, with NO 4-byte length prefix (unlike DecodeHSMSMessage). It copies
// payload into an owned buffer, so the caller may reuse or free payload immediately after return.
func DecodeHSMSPayload(payload []byte) (Message, error) {
	if len(payload) < 10 {
		return nil, fmt.Errorf("hsms payload too short: %d bytes (minimum 10): %w", len(payload), ErrInvalidHeaderLength)
	}
	if len(payload) > maxHSMSMsgLen {
		return nil, fmt.Errorf("hsms payload exceeds maximum: %d > %d", len(payload), maxHSMSMsgLen)
	}

	owned := append([]byte(nil), payload...)

	return decodeOwnedFrame(owned)
}

// DecodeOwnedHSMSPayload is DecodeHSMSPayload without the defensive copy: it transfers ownership
// of payload to the returned message (for a data message the body aliases payload zero-copy), so
// the caller MUST NOT mutate or reuse payload and must keep it alive while the message is retained
// — the same ownership-transfer contract as secs2.DecodeOwned.
func DecodeOwnedHSMSPayload(payload []byte) (Message, error) {
	if len(payload) < 10 {
		return nil, fmt.Errorf("hsms payload too short: %d bytes (minimum 10): %w", len(payload), ErrInvalidHeaderLength)
	}
	if len(payload) > maxHSMSMsgLen {
		return nil, fmt.Errorf("hsms payload exceeds maximum: %d > %d", len(payload), maxHSMSMsgLen)
	}

	return decodeOwnedFrame(payload)
}

// decodeOwnedFrame decodes a Message from an owned, immutable [header || body]
// buffer. owned must be at least 10 bytes (the header); owned[10:] is the body.
//
// For DataMsgType the body slice owned[10:] is stored inside the returned
// *DataMessage zero-copy — no allocation is made for the body bytes and the
// backing array of owned is kept alive by the sub-slice reference.
//
// This function is unexported. The recv path and in-package tests call it
// directly to avoid the copy in DecodeHSMSMessage.
func decodeOwnedFrame(owned []byte) (Message, error) {
	if len(owned) < 10 {
		return nil, fmt.Errorf("hsms frame too short: %d bytes (minimum 10): %w", len(owned), ErrInvalidHeaderLength)
	}

	var h [10]byte
	copy(h[:], owned[0:10])

	if h[4] != 0 {
		return nil, fmt.Errorf("invalid PType: %d: %w", h[4], ErrInvalidPType)
	}

	switch MsgType(h[5]) { //nolint:exhaustive // UndefinedMsgType (255) is a sentinel, not a wire value; handled by default
	case DataMsgType:
		return newRawFrameDataMessage(h, owned[10:]), nil

	case SelectReqType, SelectRspType, DeselectReqType, DeselectRspType,
		LinktestReqType, LinktestRspType, RejectReqType, SeparateReqType:
		return &ControlMessage{header: h, replyExpected: false}, nil

	default:
		return nil, fmt.Errorf("undefined SType: %d: %w", h[5], ErrInvalidControlMsgSType)
	}
}

// newRawFrameDataMessage constructs a raw-frame *DataMessage from an already-parsed
// 10-byte header value and the body sub-slice of an owned buffer.
//
// The body slice is wrapped with wire.AdoptBody — no copy is made.
// The decodeState.once is left unfired so that DataMessage.Item fires it exactly
// once on the first call, running secs2.Decode on the body bytes.
func newRawFrameDataMessage(h [10]byte, body []byte) *DataMessage {
	return &DataMessage{
		header: h,
		body:   wire.AdoptBody(body),
		dec:    &decodeState{},
	}
}
