package hsmsss

import (
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"github.com/arloliu/go-secs/hsms"
	"github.com/arloliu/go-secs/secs2"
	"github.com/stretchr/testify/require"
)

// buildLinktestReqFrame builds a raw HSMS linktest.req frame (length header + 10-byte header).
func buildLinktestReqFrame() []byte {
	// A linktest.req control message is exactly 10 bytes of header, no body.
	header := [10]byte{
		0xFF, 0xFF, // session ID
		0x00, 0x00, // stream=0, function=0
		0x00,                   // PType = 0
		hsms.LinkTestReqType,   // SType = 5
		0x00, 0x00, 0x00, 0x01, // system bytes = 1
	}

	frame := make([]byte, 4+len(header))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(header)))
	copy(frame[4:], header[:])

	return frame
}

// TestMessageReader_Success verifies that a valid HSMS message frame is correctly
// read and decoded.
func TestMessageReader_Success(t *testing.T) {
	require := require.New(t)

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	reader := &messageReader{t8Timeout: 5 * time.Second}

	// Write a valid linktest.req frame in the background.
	go func() {
		_, _ = server.Write(buildLinktestReqFrame())
	}()

	lenBuf := make([]byte, 4)
	msg, rawBody, err := reader.ReadMessage(client, lenBuf)
	require.NoError(err)
	require.NotNil(msg)
	require.Equal(hsms.LinkTestReqType, msg.Type())
	require.Len(rawBody, 10) // header-only control message
	// lenBuf should contain the length bytes
	require.Equal(uint32(10), binary.BigEndian.Uint32(lenBuf))
}

// TestMessageReader_ZeroLength verifies that a zero-length message is rejected.
func TestMessageReader_ZeroLength(t *testing.T) {
	require := require.New(t)

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	reader := &messageReader{t8Timeout: 5 * time.Second}

	// Write 4 zero bytes (length = 0).
	go func() {
		_, _ = server.Write([]byte{0x00, 0x00, 0x00, 0x00})
	}()

	lenBuf := make([]byte, 4)
	msg, rawBody, err := reader.ReadMessage(client, lenBuf)
	require.Error(err)
	require.Nil(msg)
	require.Nil(rawBody)
	require.Contains(err.Error(), "length is zero")
}

// TestMessageReader_Oversized verifies that a message exceeding MaxByteSize is rejected.
func TestMessageReader_Oversized(t *testing.T) {
	require := require.New(t)

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	reader := &messageReader{t8Timeout: 5 * time.Second}

	// Write a length that exceeds secs2.MaxByteSize.
	oversize := secs2.MaxByteSize + 1
	lenBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBytes, uint32(oversize))

	go func() {
		_, _ = server.Write(lenBytes)
	}()

	lenBuf := make([]byte, 4)
	msg, rawBody, err := reader.ReadMessage(client, lenBuf)
	require.Error(err)
	require.Nil(msg)
	require.Nil(rawBody)
	require.Contains(err.Error(), "exceeds maximum")
}

// TestMessageReader_ReadLengthError verifies that a connection closed before the
// length header is fully read returns an error.
func TestMessageReader_ReadLengthError(t *testing.T) {
	require := require.New(t)

	client, server := net.Pipe()
	defer client.Close()

	reader := &messageReader{t8Timeout: 5 * time.Second}

	// Close the write side immediately — reads on the client will fail.
	_ = server.Close()

	lenBuf := make([]byte, 4)
	msg, rawBody, err := reader.ReadMessage(client, lenBuf)
	require.Error(err)
	require.Nil(msg)
	require.Nil(rawBody)
	// net.Pipe returns "io: read/write on closed pipe" when peer is closed.
	require.ErrorIs(err, io.ErrClosedPipe)
}

// TestMessageReader_T8Timeout verifies that if the payload is not delivered
// within T8, the read fails with a timeout error.
func TestMessageReader_T8Timeout(t *testing.T) {
	require := require.New(t)

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	reader := &messageReader{t8Timeout: 50 * time.Millisecond}

	// Write valid length header but delay the payload past T8.
	go func() {
		lenBytes := make([]byte, 4)
		binary.BigEndian.PutUint32(lenBytes, 10) // expect 10 bytes of payload
		_, _ = server.Write(lenBytes)
		// Do NOT write the payload — let T8 expire.
	}()

	lenBuf := make([]byte, 4)
	msg, rawBody, err := reader.ReadMessage(client, lenBuf)
	require.Error(err)
	require.Nil(msg)
	require.Nil(rawBody)
	require.Contains(err.Error(), "read message payload")

	// The underlying error should be a timeout.
	var netErr net.Error
	require.ErrorAs(err, &netErr)
	require.True(netErr.Timeout())
}

// TestMessageReader_DecodeError verifies that a payload that cannot be decoded
// returns an error along with the raw body bytes for diagnostics.
func TestMessageReader_DecodeError(t *testing.T) {
	require := require.New(t)

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	reader := &messageReader{t8Timeout: 5 * time.Second}

	// Write a valid length header but garbage payload that is too short
	// for the HSMS header (< 10 bytes). Length says 5 bytes.
	go func() {
		lenBytes := make([]byte, 4)
		binary.BigEndian.PutUint32(lenBytes, 5) // 5-byte payload is invalid for HSMS (needs ≥10)
		_, _ = server.Write(lenBytes)
		_, _ = server.Write([]byte{0x01, 0x02, 0x03, 0x04, 0x05})
	}()

	lenBuf := make([]byte, 4)
	msg, rawBody, err := reader.ReadMessage(client, lenBuf)
	require.Error(err)
	require.Nil(msg)
	require.Contains(err.Error(), "decode message")

	// rawBody should be returned even on decode failure (for hex-dump logging).
	require.NotNil(rawBody)
	require.Len(rawBody, 5)
}

// TestMessageReader_PartialPayload verifies that a connection closed mid-payload
// returns an error.
func TestMessageReader_PartialPayload(t *testing.T) {
	require := require.New(t)

	client, server := net.Pipe()
	defer client.Close()

	reader := &messageReader{t8Timeout: 5 * time.Second}

	// Write length header claiming 10 bytes, then only 4 bytes and close.
	go func() {
		lenBytes := make([]byte, 4)
		binary.BigEndian.PutUint32(lenBytes, 10)
		_, _ = server.Write(lenBytes)
		_, _ = server.Write([]byte{0x01, 0x02, 0x03, 0x04})
		_ = server.Close()
	}()

	lenBuf := make([]byte, 4)
	msg, rawBody, err := reader.ReadMessage(client, lenBuf)
	require.Error(err)
	require.Nil(msg)
	require.Nil(rawBody)
	require.Contains(err.Error(), "read message payload")
}
