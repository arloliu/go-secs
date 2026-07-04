package hsms_test

import (
	"testing"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testSystemBytes is a fixed system-bytes value used across all factory tests.
var testSystemBytes = [4]byte{0x01, 0x02, 0x03, 0x04}

// assertMsgInterface asserts that *ControlMessage satisfies the Message interface at compile-time.
// The var _ declaration in control_msg.go covers this; this test makes the intent explicit.
func TestControlMessage_ImplementsMessageInterface(t *testing.T) {
	var _ hsms.Message = (*hsms.ControlMessage)(nil)
}

// ────────────────────────────────────────────────────────────────
// NewSelectReq
// ────────────────────────────────────────────────────────────────

func TestNewSelectReq_HeaderBytes(t *testing.T) {
	msg := hsms.NewSelectReq(0x1234, testSystemBytes)

	// header[0:2] = sessionID big-endian, [5] = SelectReqType(1), [6:10] = systemBytes
	want := [10]byte{0x12, 0x34, 0x00, 0x00, 0x00, 0x01, 0x01, 0x02, 0x03, 0x04}
	assert.Equal(t, want, msg.HeaderBytes())
}

func TestNewSelectReq_ToBytes(t *testing.T) {
	msg := hsms.NewSelectReq(0x1234, testSystemBytes)

	// 4-byte length prefix (always 10 for control) + 10-byte header = 14 bytes total
	want := []byte{0x00, 0x00, 0x00, 0x0A, 0x12, 0x34, 0x00, 0x00, 0x00, 0x01, 0x01, 0x02, 0x03, 0x04}
	assert.Equal(t, want, msg.ToBytes())
}

func TestNewSelectReq_Accessors(t *testing.T) {
	msg := hsms.NewSelectReq(0x1234, testSystemBytes)

	assert.Equal(t, hsms.SelectReqType, msg.Type())
	assert.Equal(t, uint16(0x1234), msg.SessionID())
	assert.Equal(t, testSystemBytes, msg.SystemBytes())
	assert.True(t, msg.WaitBit(), "select.req must have WaitBit=true")
}

// ────────────────────────────────────────────────────────────────
// NewSelectRsp
// ────────────────────────────────────────────────────────────────

func TestNewSelectRsp_HeaderBytes(t *testing.T) {
	req := hsms.NewSelectReq(0x1234, testSystemBytes)
	rsp, err := hsms.NewSelectRsp(req, hsms.SelectStatusSuccess)
	require.NoError(t, err)

	// Copies session ID and system bytes from req; status in header[3]; sType=2
	want := [10]byte{0x12, 0x34, 0x00, 0x00, 0x00, 0x02, 0x01, 0x02, 0x03, 0x04}
	assert.Equal(t, want, rsp.HeaderBytes())
}

func TestNewSelectRsp_ToBytes(t *testing.T) {
	req := hsms.NewSelectReq(0x1234, testSystemBytes)
	rsp, err := hsms.NewSelectRsp(req, hsms.SelectStatusSuccess)
	require.NoError(t, err)

	want := []byte{0x00, 0x00, 0x00, 0x0A, 0x12, 0x34, 0x00, 0x00, 0x00, 0x02, 0x01, 0x02, 0x03, 0x04}
	assert.Equal(t, want, rsp.ToBytes())
}

func TestNewSelectRsp_Accessors(t *testing.T) {
	req := hsms.NewSelectReq(0x1234, testSystemBytes)
	rsp, err := hsms.NewSelectRsp(req, hsms.SelectStatusAlreadyActive)
	require.NoError(t, err)

	assert.Equal(t, hsms.SelectRspType, rsp.Type())
	assert.Equal(t, uint16(0x1234), rsp.SessionID())
	assert.Equal(t, testSystemBytes, rsp.SystemBytes())
	assert.False(t, rsp.WaitBit(), "select.rsp must have WaitBit=false")
	// Status byte is in HeaderBytes()[3]
	assert.Equal(t, byte(hsms.SelectStatusAlreadyActive), rsp.HeaderBytes()[3])
}

func TestNewSelectRsp_ErrorOnWrongType(t *testing.T) {
	notReq := hsms.NewSeparateReq(0x0001, testSystemBytes)
	_, err := hsms.NewSelectRsp(notReq, 0)
	assert.Error(t, err)
}

// ────────────────────────────────────────────────────────────────
// NewDeselectReq
// ────────────────────────────────────────────────────────────────

func TestNewDeselectReq_HeaderBytes(t *testing.T) {
	sb := [4]byte{0xAA, 0xBB, 0xCC, 0xDD}
	msg := hsms.NewDeselectReq(0x5678, sb)

	want := [10]byte{0x56, 0x78, 0x00, 0x00, 0x00, 0x03, 0xAA, 0xBB, 0xCC, 0xDD}
	assert.Equal(t, want, msg.HeaderBytes())
}

func TestNewDeselectReq_ToBytes(t *testing.T) {
	sb := [4]byte{0xAA, 0xBB, 0xCC, 0xDD}
	msg := hsms.NewDeselectReq(0x5678, sb)

	want := []byte{0x00, 0x00, 0x00, 0x0A, 0x56, 0x78, 0x00, 0x00, 0x00, 0x03, 0xAA, 0xBB, 0xCC, 0xDD}
	assert.Equal(t, want, msg.ToBytes())
}

func TestNewDeselectReq_Accessors(t *testing.T) {
	sb := [4]byte{0xAA, 0xBB, 0xCC, 0xDD}
	msg := hsms.NewDeselectReq(0x5678, sb)

	assert.Equal(t, hsms.DeselectReqType, msg.Type())
	assert.Equal(t, uint16(0x5678), msg.SessionID())
	assert.Equal(t, sb, msg.SystemBytes())
	assert.True(t, msg.WaitBit(), "deselect.req must have WaitBit=true")
}

// ────────────────────────────────────────────────────────────────
// NewDeselectRsp
// ────────────────────────────────────────────────────────────────

func TestNewDeselectRsp_HeaderBytes(t *testing.T) {
	sb := [4]byte{0xAA, 0xBB, 0xCC, 0xDD}
	req := hsms.NewDeselectReq(0x5678, sb)
	rsp, err := hsms.NewDeselectRsp(req, hsms.DeselectStatusSuccess)
	require.NoError(t, err)

	want := [10]byte{0x56, 0x78, 0x00, 0x00, 0x00, 0x04, 0xAA, 0xBB, 0xCC, 0xDD}
	assert.Equal(t, want, rsp.HeaderBytes())
}

func TestNewDeselectRsp_StatusByte(t *testing.T) {
	req := hsms.NewDeselectReq(0x5678, testSystemBytes)
	rsp, err := hsms.NewDeselectRsp(req, hsms.DeselectStatusBusy)
	require.NoError(t, err)

	assert.Equal(t, byte(hsms.DeselectStatusBusy), rsp.HeaderBytes()[3])
	assert.False(t, rsp.WaitBit())
}

func TestNewDeselectRsp_ErrorOnWrongType(t *testing.T) {
	notReq := hsms.NewSelectReq(0x0001, testSystemBytes)
	_, err := hsms.NewDeselectRsp(notReq, 0)
	assert.Error(t, err)
}

// ────────────────────────────────────────────────────────────────
// NewLinktestReq — session ID must be 0xFFFF
// ────────────────────────────────────────────────────────────────

func TestNewLinktestReq_HeaderBytes(t *testing.T) {
	msg := hsms.NewLinktestReq(testSystemBytes)

	// header[0:2] = 0xFFFF, header[5] = LinktestReqType(5)
	want := [10]byte{0xFF, 0xFF, 0x00, 0x00, 0x00, 0x05, 0x01, 0x02, 0x03, 0x04}
	assert.Equal(t, want, msg.HeaderBytes())
}

func TestNewLinktestReq_ToBytes(t *testing.T) {
	msg := hsms.NewLinktestReq(testSystemBytes)

	want := []byte{0x00, 0x00, 0x00, 0x0A, 0xFF, 0xFF, 0x00, 0x00, 0x00, 0x05, 0x01, 0x02, 0x03, 0x04}
	assert.Equal(t, want, msg.ToBytes())
}

func TestNewLinktestReq_SessionID(t *testing.T) {
	msg := hsms.NewLinktestReq(testSystemBytes)

	assert.Equal(t, uint16(0xFFFF), msg.SessionID(), "linktest.req session ID must be 0xFFFF")
	assert.Equal(t, hsms.LinktestReqType, msg.Type())
	assert.True(t, msg.WaitBit())
}

// ────────────────────────────────────────────────────────────────
// NewLinktestRsp
// ────────────────────────────────────────────────────────────────

func TestNewLinktestRsp_HeaderBytes(t *testing.T) {
	req := hsms.NewLinktestReq(testSystemBytes)
	rsp, err := hsms.NewLinktestRsp(req)
	require.NoError(t, err)

	// session still 0xFFFF; sType=6; system bytes copied from req
	want := [10]byte{0xFF, 0xFF, 0x00, 0x00, 0x00, 0x06, 0x01, 0x02, 0x03, 0x04}
	assert.Equal(t, want, rsp.HeaderBytes())
}

func TestNewLinktestRsp_ToBytes(t *testing.T) {
	req := hsms.NewLinktestReq(testSystemBytes)
	rsp, err := hsms.NewLinktestRsp(req)
	require.NoError(t, err)

	want := []byte{0x00, 0x00, 0x00, 0x0A, 0xFF, 0xFF, 0x00, 0x00, 0x00, 0x06, 0x01, 0x02, 0x03, 0x04}
	assert.Equal(t, want, rsp.ToBytes())
}

func TestNewLinktestRsp_Accessors(t *testing.T) {
	req := hsms.NewLinktestReq(testSystemBytes)
	rsp, err := hsms.NewLinktestRsp(req)
	require.NoError(t, err)

	assert.Equal(t, hsms.LinktestRspType, rsp.Type())
	assert.Equal(t, uint16(0xFFFF), rsp.SessionID())
	assert.Equal(t, testSystemBytes, rsp.SystemBytes())
	assert.False(t, rsp.WaitBit())
}

func TestNewLinktestRsp_ErrorOnWrongType(t *testing.T) {
	notReq := hsms.NewSelectReq(0x0001, testSystemBytes)
	_, err := hsms.NewLinktestRsp(notReq)
	assert.Error(t, err)
}

// ────────────────────────────────────────────────────────────────
// NewSeparateReq
// ────────────────────────────────────────────────────────────────

func TestNewSeparateReq_HeaderBytes(t *testing.T) {
	msg := hsms.NewSeparateReq(0x0001, testSystemBytes)

	want := [10]byte{0x00, 0x01, 0x00, 0x00, 0x00, 0x09, 0x01, 0x02, 0x03, 0x04}
	assert.Equal(t, want, msg.HeaderBytes())
}

func TestNewSeparateReq_ToBytes(t *testing.T) {
	msg := hsms.NewSeparateReq(0x0001, testSystemBytes)

	want := []byte{0x00, 0x00, 0x00, 0x0A, 0x00, 0x01, 0x00, 0x00, 0x00, 0x09, 0x01, 0x02, 0x03, 0x04}
	assert.Equal(t, want, msg.ToBytes())
}

func TestNewSeparateReq_Accessors(t *testing.T) {
	msg := hsms.NewSeparateReq(0x0001, testSystemBytes)

	assert.Equal(t, hsms.SeparateReqType, msg.Type())
	assert.Equal(t, uint16(0x0001), msg.SessionID())
	assert.Equal(t, testSystemBytes, msg.SystemBytes())
	assert.False(t, msg.WaitBit(), "separate.req must have WaitBit=false")
}

// ────────────────────────────────────────────────────────────────
// NewRejectReq — sType vs pType byte placement
// ────────────────────────────────────────────────────────────────

func TestNewRejectReq_STypeNotSupported(t *testing.T) {
	// rejected is a select.req: header[4]=pType=0x00, header[5]=sType=0x01
	rejected := hsms.NewSelectReq(0x1234, testSystemBytes)
	msg := hsms.NewRejectReq(rejected, hsms.RejectSTypeNotSupported)

	// reasonCode != 2 → header[2] = sType of rejected = 0x01
	want := [10]byte{0x12, 0x34, 0x01, 0x01, 0x00, 0x07, 0x01, 0x02, 0x03, 0x04}
	assert.Equal(t, want, msg.HeaderBytes())
	assert.Equal(t, hsms.RejectReqType, msg.Type())
	assert.False(t, msg.WaitBit())
}

func TestNewRejectReq_PTypeNotSupported(t *testing.T) {
	// rejected is a select.req: header[4]=pType=0x00, header[5]=sType=0x01
	rejected := hsms.NewSelectReq(0x1234, testSystemBytes)
	msg := hsms.NewRejectReq(rejected, hsms.RejectPTypeNotSupported)

	// reasonCode == 2 → header[2] = pType of rejected = 0x00
	want := [10]byte{0x12, 0x34, 0x00, 0x02, 0x00, 0x07, 0x01, 0x02, 0x03, 0x04}
	assert.Equal(t, want, msg.HeaderBytes())
}

func TestNewRejectReq_ToBytes(t *testing.T) {
	rejected := hsms.NewSelectReq(0x1234, testSystemBytes)
	msg := hsms.NewRejectReq(rejected, hsms.RejectSTypeNotSupported)

	want := []byte{0x00, 0x00, 0x00, 0x0A, 0x12, 0x34, 0x01, 0x01, 0x00, 0x07, 0x01, 0x02, 0x03, 0x04}
	assert.Equal(t, want, msg.ToBytes())
}

// ────────────────────────────────────────────────────────────────
// NewRejectReqRaw
// ────────────────────────────────────────────────────────────────

func TestNewRejectReqRaw_STypeReason(t *testing.T) {
	// reasonCode=1 (not 2) → header[2] = sType
	msg := hsms.NewRejectReqRaw(0x1234, 0x00, 0x01, testSystemBytes, hsms.RejectSTypeNotSupported)

	want := [10]byte{0x12, 0x34, 0x01, 0x01, 0x00, 0x07, 0x01, 0x02, 0x03, 0x04}
	assert.Equal(t, want, msg.HeaderBytes())
}

func TestNewRejectReqRaw_PTypeReason(t *testing.T) {
	// reasonCode=2 → header[2] = pType
	msg := hsms.NewRejectReqRaw(0x1234, 0x01, 0x05, testSystemBytes, hsms.RejectPTypeNotSupported)

	// header[2] = pType = 0x01
	want := [10]byte{0x12, 0x34, 0x01, 0x02, 0x00, 0x07, 0x01, 0x02, 0x03, 0x04}
	assert.Equal(t, want, msg.HeaderBytes())
}

// ────────────────────────────────────────────────────────────────
// RejectReasonCode accessor + GetRejectReasonCode
// ────────────────────────────────────────────────────────────────

func TestRejectReasonCode(t *testing.T) {
	rejected := hsms.NewSelectReq(0x1234, testSystemBytes)
	msg := hsms.NewRejectReq(rejected, hsms.RejectSTypeNotSupported)

	code, err := msg.RejectReasonCode()
	require.NoError(t, err)
	assert.Equal(t, byte(hsms.RejectSTypeNotSupported), code)
}

func TestGetRejectReasonCode(t *testing.T) {
	rejected := hsms.NewSelectReq(0x1234, testSystemBytes)
	msg := hsms.NewRejectReq(rejected, hsms.RejectNotSelected)

	code, err := hsms.GetRejectReasonCode(msg)
	require.NoError(t, err)
	assert.Equal(t, byte(hsms.RejectNotSelected), code)
}

func TestGetRejectReasonCode_ErrorOnNonReject(t *testing.T) {
	msg := hsms.NewSelectReq(0x1234, testSystemBytes)
	_, err := hsms.GetRejectReasonCode(msg)
	assert.Error(t, err)
}

// ────────────────────────────────────────────────────────────────
// WithSessionID / WithSystemBytes — immutability
// ────────────────────────────────────────────────────────────────

func TestWithSessionID_ReturnsNewMessage(t *testing.T) {
	original := hsms.NewSelectReq(0x1234, testSystemBytes)
	updated := original.WithSessionID(0x5678)

	assert.Equal(t, uint16(0x5678), updated.SessionID(), "updated must have new session ID")
	assert.Equal(t, uint16(0x1234), original.SessionID(), "original must be unchanged")
	assert.NotSame(t, original, updated, "must be a distinct pointer")
}

func TestWithSystemBytes_ReturnsNewMessage(t *testing.T) {
	newSB := [4]byte{0xAA, 0xBB, 0xCC, 0xDD}
	original := hsms.NewSelectReq(0x1234, testSystemBytes)
	updated := original.WithSystemBytes(newSB)

	assert.Equal(t, newSB, updated.SystemBytes(), "updated must have new system bytes")
	assert.Equal(t, testSystemBytes, original.SystemBytes(), "original must be unchanged")
}

func TestWithSessionID_PreservesType(t *testing.T) {
	original := hsms.NewLinktestReq(testSystemBytes)
	updated := original.WithSessionID(0x0001)

	assert.Equal(t, hsms.LinktestReqType, updated.Type())
	assert.True(t, updated.WaitBit())
}

// ────────────────────────────────────────────────────────────────
// SystemBytes / HeaderBytes return value copies (not aliases)
// ────────────────────────────────────────────────────────────────

func TestSystemBytes_IsCopy(t *testing.T) {
	msg := hsms.NewSelectReq(0x1234, testSystemBytes)
	sb := msg.SystemBytes()
	sb[0] = 0xFF

	// The write to the local copy must not affect the original.
	assert.Equal(t, byte(0xFF), sb[0], "local copy was written")
	assert.Equal(t, testSystemBytes, msg.SystemBytes(), "original must be unaffected")
}

func TestHeaderBytes_IsCopy(t *testing.T) {
	msg := hsms.NewSelectReq(0x1234, testSystemBytes)
	hdr := msg.HeaderBytes()
	hdr[0] = 0xFF

	// The write to the local copy must not affect the original.
	assert.Equal(t, byte(0xFF), hdr[0], "local copy was written")
	assert.Equal(t, byte(0x12), msg.HeaderBytes()[0], "original must be unaffected")
}

// ────────────────────────────────────────────────────────────────
// ToBytes length invariant
// ────────────────────────────────────────────────────────────────

func TestToBytes_AlwaysFourteenBytes(t *testing.T) {
	msgs := []*hsms.ControlMessage{
		hsms.NewSelectReq(0x0001, testSystemBytes),
		hsms.NewDeselectReq(0x0001, testSystemBytes),
		hsms.NewLinktestReq(testSystemBytes),
		hsms.NewSeparateReq(0x0001, testSystemBytes),
		hsms.NewRejectReq(hsms.NewSelectReq(0x0001, testSystemBytes), hsms.RejectSTypeNotSupported),
	}
	for _, msg := range msgs {
		assert.Len(t, msg.ToBytes(), 14, "ToBytes for %v must be 14 bytes", msg.Type())
		// first 4 bytes encode length = 10
		assert.Equal(t, []byte{0x00, 0x00, 0x00, 0x0A}, msg.ToBytes()[:4])
	}
}

// TestControlMessage_ID verifies ID() decodes the message's System Bytes as a
// big-endian uint32, matching FromSystemBytes(msg.SystemBytes()).
func TestControlMessage_ID(t *testing.T) {
	msg := hsms.NewLinktestReq(testSystemBytes)

	assert.Equal(t, uint32(0x01020304), msg.ID())
	assert.Equal(t, hsms.FromSystemBytes(testSystemBytes), msg.ID())
}
