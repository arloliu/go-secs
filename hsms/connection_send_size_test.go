// Package hsms — in-package tests for buildFrameBuffers' send-side frame-size cap
// (connection_send.go). Using package hsms (not hsms_test) gives access to the unexported
// buildFrameBuffers and maxHSMSMsgLen so the boundary can be exercised exactly.
package hsms

import (
	"testing"

	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// oversizedLenItem wraps a trivial real secs2.Item and overrides EncodedLen/AppendTo to report
// an arbitrary on-wire length without materializing that many bytes: AppendTo extends dst to its
// full pre-sized capacity (the zero bytes already there from make are sufficient — nothing reads
// them) rather than writing real content, so exercising a MaxMessageSize-scale body costs one
// make() of untouched pages, not a real multi-megabyte string build or encode.
type oversizedLenItem struct {
	secs2.Item
	encodedLen int
}

// EncodedLen reports the stubbed length instead of the wrapped item's real (trivial) one.
func (it oversizedLenItem) EncodedLen() int { return it.encodedLen }

// AppendTo extends dst to its full pre-sized capacity (make(..., 0, EncodedLen()) in
// treeBody.encoded) without writing real content, so len(t.enc) after encoding equals the
// stubbed EncodedLen — matching what buildFrameBuffers' cap check sums over.
func (it oversizedLenItem) AppendTo(dst []byte) []byte { return dst[:cap(dst)] }

// fakeBodyDataMessage builds a *DataMessage whose body reports exactly bodyLen bytes on the
// wire via oversizedLenItem, for exercising buildFrameBuffers' cap check at exact boundary
// values without allocating a real MaxMessageSize-sized SECS-II item.
func fakeBodyDataMessage(t *testing.T, bodyLen int) *DataMessage {
	t.Helper()

	item := oversizedLenItem{Item: secs2.NewASCIIItem("x"), encodedLen: bodyLen}
	msg, err := NewDataMessage(1, 1, false, 0x0001, [4]byte{0, 0, 0, 1}, item)
	require.NoError(t, err)

	return msg
}

// TestBuildFrameBuffers_RejectsOversizedBody proves the send-side cap: a data message body one
// byte past MaxMessageSize-10 (the largest body that still fits within the 10-byte-header +
// body msgLen ceiling) is rejected with ErrMessageTooLarge rather than framed and returned.
func TestBuildFrameBuffers_RejectsOversizedBody(t *testing.T) {
	t.Parallel()

	msg := fakeBodyDataMessage(t, maxHSMSMsgLen-10+1)

	bufs, err := buildFrameBuffers(msg)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrMessageTooLarge)
	assert.Nil(t, bufs)
}

// TestBuildFrameBuffers_SucceedsAtBoundary proves the cap is inclusive: a body of exactly
// MaxMessageSize-10 bytes (msgLen == MaxMessageSize, matching the receive-side ceiling in
// decode.go) is framed successfully, not rejected.
func TestBuildFrameBuffers_SucceedsAtBoundary(t *testing.T) {
	t.Parallel()

	msg := fakeBodyDataMessage(t, maxHSMSMsgLen-10)

	bufs, err := buildFrameBuffers(msg)
	require.NoError(t, err)

	total := 0
	for _, b := range bufs {
		total += len(b)
	}
	assert.Equal(t, 4+maxHSMSMsgLen, total, "14-byte prefix + boundary body must equal 4 + maxHSMSMsgLen")
}

// TestBuildFrameBuffers_ControlMessageNeverTripsCheck proves a control message — always a fixed
// 14-byte on-wire frame (4-byte length prefix + 10-byte header, no body) — never reaches the
// data-message size check regardless of the cap's value.
func TestBuildFrameBuffers_ControlMessageNeverTripsCheck(t *testing.T) {
	t.Parallel()

	msg := NewLinktestReq([4]byte{0, 0, 0, 1})

	bufs, err := buildFrameBuffers(msg)
	require.NoError(t, err)

	total := 0
	for _, b := range bufs {
		total += len(b)
	}
	assert.Equal(t, 14, total, "control frame must always be the fixed 14-byte prefix, never touching the body cap check")
}
