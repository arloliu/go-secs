// Package hsms — in-package tests for decode.go.
//
// Using package hsms (not hsms_test) gives access to unexported identifiers:
// decodeOwnedFrame, DataMessage.body, and newRawFrameDataMessage, so the
// zero-copy alias proof can verify that the decoded body slice aliases the
// owned buffer directly without an exported Body accessor.
package hsms

import (
	"errors"
	"sync"
	"testing"

	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bodyRef returns the underlying raw body slice for a DataMessage by going
// through the Body.Buffers() bridge. For rawFrameBody (decode path) this is
// the zero-copy reference; no copy is made. For treeBody (constructed path)
// this returns the memoized encoded bytes (distinct backing array).
func bodyRef(msg *DataMessage) []byte {
	bufs := msg.body.Buffers()
	if len(bufs) == 0 {
		return nil
	}

	return bufs[0]
}

// ────────────────────────────────────────────────────────────────
// Round-trip: data messages
// ────────────────────────────────────────────────────────────────

func TestDecodeHSMSMessage_DataRoundTrip(t *testing.T) {
	t.Parallel()

	item := secs2.NewASCIIItem("hello SECS")
	orig, err := NewDataMessage(1, 1, true, 0x0001, [4]byte{0, 0, 0, 1}, item)
	require.NoError(t, err)

	data := orig.ToBytes()
	decoded, err := DecodeHSMSMessage(data)
	require.NoError(t, err)

	dm, ok := decoded.(*DataMessage)
	require.True(t, ok, "decoded message should be *DataMessage")

	// Header bytes must match exactly.
	assert.Equal(t, orig.HeaderBytes(), dm.HeaderBytes())

	// Body bytes must match (wire-level equality).
	assert.Equal(t, orig.AppendBodyTo(nil), dm.AppendBodyTo(nil))

	// Item() must be wire-level equal (decoded value matches the original encoding).
	origItem, origErr := orig.Item()
	require.NoError(t, origErr)
	decItem, decErr := dm.Item()
	require.NoError(t, decErr)
	assert.Equal(t, origItem.ToBytes(), decItem.ToBytes())
}

func TestDecodeHSMSMessage_DataRoundTrip_Binary(t *testing.T) {
	t.Parallel()

	item := secs2.NewBinaryItem([]byte{0x01, 0x02, 0x03, 0xAB, 0xCD})
	orig, err := NewDataMessage(2, 3, false, 0xFFFF, [4]byte{1, 2, 3, 4}, item)
	require.NoError(t, err)

	data := orig.ToBytes()
	decoded, err := DecodeHSMSMessage(data)
	require.NoError(t, err)

	dm, ok := decoded.(*DataMessage)
	require.True(t, ok)
	assert.Equal(t, orig.HeaderBytes(), dm.HeaderBytes())
	assert.Equal(t, orig.AppendBodyTo(nil), dm.AppendBodyTo(nil))
}

// ────────────────────────────────────────────────────────────────
// Round-trip: empty data message body
// ────────────────────────────────────────────────────────────────

func TestDecodeHSMSMessage_EmptyBody(t *testing.T) {
	t.Parallel()

	orig, err := NewDataMessage(1, 2, false, 0x0001, [4]byte{}, secs2.NewEmptyItem())
	require.NoError(t, err)

	data := orig.ToBytes()
	decoded, err := DecodeHSMSMessage(data)
	require.NoError(t, err)

	dm, ok := decoded.(*DataMessage)
	require.True(t, ok)
	assert.Equal(t, orig.HeaderBytes(), dm.HeaderBytes())
	assert.Equal(t, 0, dm.BodyLen())

	decItem, decErr := dm.Item()
	require.NoError(t, decErr)
	assert.True(t, decItem.IsEmpty(), "empty body should decode to EmptyItem")
}

// ────────────────────────────────────────────────────────────────
// Round-trip: every control SType
// ────────────────────────────────────────────────────────────────

func TestDecodeHSMSMessage_ControlRoundTrip(t *testing.T) {
	t.Parallel()

	sb := [4]byte{0xDE, 0xAD, 0xBE, 0xEF}

	tests := []struct {
		name string
		msg  *ControlMessage
	}{
		{"SelectReq", NewSelectReq(0x0001, sb)},
		{"SelectRsp", func() *ControlMessage {
			req := NewSelectReq(0x0001, sb)
			rsp, _ := NewSelectRsp(req, SelectStatusSuccess)

			return rsp
		}()},
		{"DeselectReq", NewDeselectReq(0x0002, sb)},
		{"DeselectRsp", func() *ControlMessage {
			req := NewDeselectReq(0x0002, sb)
			rsp, _ := NewDeselectRsp(req, DeselectStatusSuccess)

			return rsp
		}()},
		{"LinktestReq", NewLinktestReq(sb)},
		{"LinktestRsp", func() *ControlMessage {
			req := NewLinktestReq(sb)
			rsp, _ := NewLinktestRsp(req)

			return rsp
		}()},
		{"RejectReq", NewRejectReqRaw(0x0003, 0, byte(SeparateReqType), sb, RejectSTypeNotSupported)},
		{"SeparateReq", NewSeparateReq(0x0004, sb)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			data := tc.msg.ToBytes()
			decoded, err := DecodeHSMSMessage(data)
			require.NoError(t, err)

			cm, ok := decoded.(*ControlMessage)
			require.True(t, ok, "decoded control message should be *ControlMessage")
			assert.Equal(t, tc.msg.HeaderBytes(), cm.HeaderBytes(),
				"header bytes must round-trip exactly")
			assert.Equal(t, tc.msg.Type(), cm.Type(),
				"SType must match")
		})
	}
}

// ────────────────────────────────────────────────────────────────
// Lazy decode-once: concurrent Item() calls must not race
// ────────────────────────────────────────────────────────────────

func TestDecodeHSMSMessage_LazyDecodeOnce(t *testing.T) {
	t.Parallel()

	item := secs2.NewASCIIItem("race-test")
	orig, err := NewDataMessage(1, 1, true, 0x0001, [4]byte{}, item)
	require.NoError(t, err)

	data := orig.ToBytes()
	decoded, err := DecodeHSMSMessage(data)
	require.NoError(t, err)

	dm, ok := decoded.(*DataMessage)
	require.True(t, ok)

	const n = 200
	var wg sync.WaitGroup
	wg.Add(n)

	for range n {
		go func() {
			defer wg.Done()
			it, err := dm.Item()
			assert.NoError(t, err)
			assert.NotNil(t, it)
		}()
	}

	wg.Wait()
}

// ────────────────────────────────────────────────────────────────
// Malformed input: error cases
// ────────────────────────────────────────────────────────────────

func TestDecodeHSMSMessage_Errors(t *testing.T) {
	t.Parallel()

	// validFrame builds a minimal 14-byte valid data-message frame (empty body).
	validFrame := func() []byte {
		msg, _ := NewDataMessage(1, 2, false, 0x0001, [4]byte{}, secs2.NewEmptyItem())

		return msg.ToBytes()
	}

	// patchHeader returns a copy of a valid frame with header byte i set to v.
	patchHeader := func(i int, v byte) []byte {
		frame := validFrame()
		out := append([]byte(nil), frame...)
		out[4+i] = v // skip 4-byte length prefix

		return out
	}

	tests := []struct {
		name string
		data []byte
	}{
		{
			name: "too short (< 14 bytes)",
			data: []byte{0, 0, 0, 10, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		},
		{
			name: "msgLen below minimum (< 10)",
			data: func() []byte {
				// 4-byte length = 9; total data = 13 bytes but msgLen < 10
				d := make([]byte, 13)
				d[0], d[1], d[2], d[3] = 0, 0, 0, 9

				return d
			}(),
		},
		{
			name: "msgLen above maximum",
			data: func() []byte {
				tooLarge := uint32(secs2.MaxByteSize + 1)
				d := make([]byte, 4+10)
				d[0] = byte(tooLarge >> 24)
				d[1] = byte(tooLarge >> 16)
				d[2] = byte(tooLarge >> 8)
				d[3] = byte(tooLarge)

				return d
			}(),
		},
		{
			name: "length mismatch (data longer than declared)",
			data: func() []byte {
				frame := validFrame()      // 14 bytes, msgLen=10
				return append(frame, 0xFF) // one extra byte
			}(),
		},
		{
			name: "length mismatch (data shorter than declared)",
			data: func() []byte {
				frame := validFrame() // 14 bytes, msgLen=10
				return frame[:13]     // truncate one byte
			}(),
		},
		{
			name: "PType != 0",
			data: patchHeader(4, 0x01),
		},
		{
			name: "undefined SType 8",
			data: patchHeader(5, 8),
		},
		{
			name: "undefined SType 10",
			data: patchHeader(5, 10),
		},
		{
			name: "undefined SType 255",
			data: patchHeader(5, 255),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			msg, err := DecodeHSMSMessage(tc.data)
			assert.Error(t, err, "expected an error for malformed input")
			assert.Nil(t, msg)
		})
	}
}

// TestDecodeHSMSMessage_ErrorsWrapSentinels verifies that DecodeHSMSMessage wraps
// its reachable failure paths with the exported sentinel errors so errors.Is works.
//
// Structurally-impossible cases carry no sentinel because they cannot occur: a data
// message never has a non-zero SType (SType 0 is data by dispatch), and system bytes
// are a fixed [4]byte, so no "wrong length" or "wrong data SType" error can be produced.
func TestDecodeHSMSMessage_ErrorsWrapSentinels(t *testing.T) {
	t.Parallel()

	validFrame := func() []byte {
		msg, _ := NewDataMessage(1, 2, false, 0x0001, [4]byte{}, secs2.NewEmptyItem())

		return msg.ToBytes()
	}
	patchHeader := func(i int, v byte) []byte {
		out := append([]byte(nil), validFrame()...)
		out[4+i] = v // skip 4-byte length prefix

		return out
	}

	tests := []struct {
		name    string
		data    []byte
		wantErr error
	}{
		{
			name:    "too short",
			data:    []byte{0, 0, 0, 10, 0, 0, 0, 0, 0, 0, 0, 0, 0},
			wantErr: ErrInvalidHeaderLength,
		},
		{
			name: "msgLen below minimum",
			data: func() []byte {
				d := make([]byte, 13)
				d[3] = 9

				return d
			}(),
			wantErr: ErrInvalidHeaderLength,
		},
		{
			name:    "length mismatch",
			data:    append(validFrame(), 0xFF),
			wantErr: ErrInvalidHeaderLength,
		},
		{
			name:    "PType != 0",
			data:    patchHeader(4, 0x01),
			wantErr: ErrInvalidPType,
		},
		{
			name:    "undefined SType",
			data:    patchHeader(5, 8),
			wantErr: ErrInvalidControlMsgSType,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			msg, err := DecodeHSMSMessage(tc.data)
			require.Error(t, err)
			assert.Nil(t, msg)
			assert.True(t, errors.Is(err, tc.wantErr), "error %q must wrap %q", err, tc.wantErr)
		})
	}
}

// ────────────────────────────────────────────────────────────────
// Zero-copy alias proof (in-package: accesses DataMessage.body)
// ────────────────────────────────────────────────────────────────

// TestDecodeOwnedFrame_ZeroCopyAlias verifies that decodeOwnedFrame does NOT
// copy the body bytes: the decoded DataMessage's body backing array must be
// the exact same memory as ownedBuf[10:].
func TestDecodeOwnedFrame_ZeroCopyAlias(t *testing.T) {
	t.Parallel()

	// Build a DataMessage with a non-empty body so that &slice[0] is valid.
	item := secs2.NewASCIIItem("zero-copy-proof")
	orig, err := NewDataMessage(1, 1, true, 0x0042, [4]byte{0, 0, 0, 1}, item)
	require.NoError(t, err)

	raw := orig.ToBytes() // [4-byte len | 10-byte header | body…]
	require.True(t, len(raw) > 14, "need a non-empty body for a valid pointer check")

	// ownedBuf is [header || body] — the input decodeOwnedFrame expects.
	ownedBuf := append([]byte(nil), raw[4:]...)

	decoded, err := decodeOwnedFrame(ownedBuf)
	require.NoError(t, err)

	dm, ok := decoded.(*DataMessage)
	require.True(t, ok)

	ref := bodyRef(dm)
	require.True(t, len(ref) > 0, "body must be non-empty for pointer comparison")

	// The first byte of the body must live at the same address as ownedBuf[10].
	// This proves rawFrameBody retains owned[10:] zero-copy.
	assert.True(t, &ref[0] == &ownedBuf[10],
		"decoded body must alias ownedBuf[10:] (zero-copy)")
}

// ────────────────────────────────────────────────────────────────
// Public-copy proof: caller may reuse/mutate data after decode
// ────────────────────────────────────────────────────────────────

// TestDecodeHSMSMessage_PublicCopyProof verifies that DecodeHSMSMessage copies
// its input: mutating data after the call must leave the decoded message unchanged.
func TestDecodeHSMSMessage_PublicCopyProof(t *testing.T) {
	t.Parallel()

	item := secs2.NewASCIIItem("copy-proof")
	orig, err := NewDataMessage(1, 1, true, 0x0001, [4]byte{0, 0, 0, 2}, item)
	require.NoError(t, err)

	// Work on a mutable copy.
	data := append([]byte(nil), orig.ToBytes()...)

	decoded, err := DecodeHSMSMessage(data)
	require.NoError(t, err)

	// Capture the original header and body before mutation.
	wantHeader := decoded.HeaderBytes()
	dm, ok := decoded.(*DataMessage)
	require.True(t, ok)
	wantBody := dm.AppendBodyTo(nil)

	// Poison every byte of the input buffer.
	for i := range data {
		data[i] = 0xFF
	}

	// The decoded message must be unchanged.
	assert.Equal(t, wantHeader, decoded.HeaderBytes(),
		"header must not be affected by mutation of input buffer")
	assert.Equal(t, wantBody, dm.AppendBodyTo(nil),
		"body must not be affected by mutation of input buffer")

	// Item() must still decode correctly.
	decItem, decErr := dm.Item()
	require.NoError(t, decErr)
	origItem, _ := orig.Item()
	assert.Equal(t, origItem.ToBytes(), decItem.ToBytes())
}

// ────────────────────────────────────────────────────────────────
// DecodeHSMSPayload / DecodeOwnedHSMSPayload / NewDataMessageFromHeader (item 4)
// ────────────────────────────────────────────────────────────────

// TestDecodeHSMSPayload_RoundTrip verifies that DecodeHSMSPayload decodes a
// [header || body] payload (no length prefix) back to an Equal message.
func TestDecodeHSMSPayload_RoundTrip(t *testing.T) {
	t.Parallel()

	item := secs2.NewASCIIItem("payload-round-trip")
	orig, err := NewDataMessage(1, 1, true, 0x0001, [4]byte{0, 0, 0, 9}, item)
	require.NoError(t, err)

	payload := orig.ToBytes()[4:] // strip the 4-byte length prefix

	decoded, err := DecodeHSMSPayload(payload)
	require.NoError(t, err)

	dm, ok := decoded.(*DataMessage)
	require.True(t, ok)
	assert.True(t, orig.Equal(dm))
}

func TestDecodeHSMSPayload_ControlMessage(t *testing.T) {
	t.Parallel()

	orig := NewLinktestReq([4]byte{0, 0, 0, 1})
	payload := orig.ToBytes()[4:]

	decoded, err := DecodeHSMSPayload(payload)
	require.NoError(t, err)

	cm, ok := decoded.(*ControlMessage)
	require.True(t, ok)
	assert.Equal(t, orig.HeaderBytes(), cm.HeaderBytes())
}

func TestDecodeHSMSPayload_TooShort(t *testing.T) {
	t.Parallel()

	_, err := DecodeHSMSPayload(make([]byte, 9))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidHeaderLength)
}

func TestDecodeHSMSPayload_ExceedsMax(t *testing.T) {
	t.Parallel()

	_, err := DecodeHSMSPayload(make([]byte, maxHSMSMsgLen+1))
	require.Error(t, err)
}

func TestDecodeHSMSPayload_AtMax(t *testing.T) {
	t.Parallel()

	// Exactly maxHSMSMsgLen bytes: a valid data-message header followed by padding that
	// decodes to an item error (irrelevant — only the length-cap boundary is under test).
	payload := make([]byte, maxHSMSMsgLen)
	// header[4], header[5] left 0 (PType/SType = data message); body is empty-ish garbage
	// but the length cap check must not reject a payload exactly at the boundary.
	_, err := DecodeHSMSPayload(payload)
	require.NoError(t, err, "a payload of exactly maxHSMSMsgLen bytes must not be rejected by the length cap")
}

// TestDecodeOwnedHSMSPayload_ZeroCopyAlias verifies DecodeOwnedHSMSPayload transfers
// ownership without copying: the decoded body aliases the input buffer directly.
func TestDecodeOwnedHSMSPayload_ZeroCopyAlias(t *testing.T) {
	t.Parallel()

	item := secs2.NewASCIIItem("owned-payload-zero-copy")
	orig, err := NewDataMessage(1, 1, true, 0x0042, [4]byte{0, 0, 0, 1}, item)
	require.NoError(t, err)

	payload := append([]byte(nil), orig.ToBytes()[4:]...)
	require.True(t, len(payload) > 10, "need a non-empty body for a valid pointer check")

	decoded, err := DecodeOwnedHSMSPayload(payload)
	require.NoError(t, err)

	dm, ok := decoded.(*DataMessage)
	require.True(t, ok)

	ref := bodyRef(dm)
	require.True(t, len(ref) > 0, "body must be non-empty for pointer comparison")
	assert.True(t, &ref[0] == &payload[10], "decoded body must alias payload[10:] (zero-copy)")
}

func TestDecodeOwnedHSMSPayload_TooShort(t *testing.T) {
	t.Parallel()

	_, err := DecodeOwnedHSMSPayload(make([]byte, 9))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidHeaderLength)
}

func TestDecodeOwnedHSMSPayload_ExceedsMax(t *testing.T) {
	t.Parallel()

	_, err := DecodeOwnedHSMSPayload(make([]byte, maxHSMSMsgLen+1))
	require.Error(t, err)
}

func TestDecodeOwnedHSMSPayload_AtMax(t *testing.T) {
	t.Parallel()

	payload := make([]byte, maxHSMSMsgLen)
	_, err := DecodeOwnedHSMSPayload(payload)
	require.NoError(t, err, "a payload of exactly maxHSMSMsgLen bytes must not be rejected by the length cap")
}

// ────────────────────────────────────────────────────────────────
// NewDataMessageFromHeader (item 4)
// ────────────────────────────────────────────────────────────────

func TestNewDataMessageFromHeader_Valid(t *testing.T) {
	t.Parallel()

	built, err := NewDataMessage(5, 3, true, 0x1234, [4]byte{0, 0, 0, 42}, secs2.A("from-header"))
	require.NoError(t, err)

	fromHeader, err := NewDataMessageFromHeader(built.HeaderBytes(), secs2.A("from-header"))
	require.NoError(t, err)

	assert.True(t, built.Equal(fromHeader))
}

func TestNewDataMessageFromHeader_InvalidPType(t *testing.T) {
	t.Parallel()

	var header [10]byte
	header[4] = 1 // non-zero PType

	_, err := NewDataMessageFromHeader(header, secs2.NewEmptyItem())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidPType)
}

func TestNewDataMessageFromHeader_InvalidSType(t *testing.T) {
	t.Parallel()

	var header [10]byte
	header[5] = 1 // non-zero SType (not a data message)

	_, err := NewDataMessageFromHeader(header, secs2.NewEmptyItem())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidControlMsgSType)
}

func TestNewDataMessageFromHeader_WBitOnEvenFunction(t *testing.T) {
	t.Parallel()

	var header [10]byte
	header[2] = 0x80 // W-bit set, stream 0
	header[3] = 2    // even function

	_, err := NewDataMessageFromHeader(header, secs2.NewEmptyItem())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidRspMsg)
}
