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

	reader := &messageReader{t8Timeout: 5 * time.Second, idleReadTimeout: 10 * time.Second}

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

	reader := &messageReader{t8Timeout: 5 * time.Second, idleReadTimeout: 10 * time.Second}

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

	reader := &messageReader{t8Timeout: 5 * time.Second, idleReadTimeout: 10 * time.Second}

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

	reader := &messageReader{t8Timeout: 5 * time.Second, idleReadTimeout: 10 * time.Second}

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

	reader := &messageReader{t8Timeout: 50 * time.Millisecond, idleReadTimeout: 10 * time.Second}

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

	reader := &messageReader{t8Timeout: 5 * time.Second, idleReadTimeout: 10 * time.Second}

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

	reader := &messageReader{t8Timeout: 5 * time.Second, idleReadTimeout: 10 * time.Second}

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

// TestMessageReader_IdleTimeout_Clean verifies that when an idle timeout occurs (0 bytes read),
// the reader continues seamlessly and eventually reads the message.
func TestMessageReader_IdleTimeout_Clean(t *testing.T) {
	require := require.New(t)

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	// Set a very short idle timeout
	reader := &messageReader{t8Timeout: 5 * time.Second, idleReadTimeout: 10 * time.Millisecond}

	go func() {
		// Wait long enough for multiple idle timeouts to fire
		time.Sleep(50 * time.Millisecond)
		_, _ = server.Write(buildLinktestReqFrame())
	}()

	lenBuf := make([]byte, 4)
	var msg hsms.HSMSMessage
	var rawBody []byte
	var err error

	// Use require.Eventually to repeatedly poll until the reader successfully receives the message.
	// This covers up any timing sensitivity around when exactly the idle timeouts fire.
	// It spins a goroutine for the ReadMessage to avoid blocking Eventually.
	msgCh := make(chan struct{})
	go func() {
		msg, rawBody, err = reader.ReadMessage(client, lenBuf)
		close(msgCh)
	}()

	require.Eventually(func() bool {
		select {
		case <-msgCh:
			return true
		default:
			return false
		}
	}, 1*time.Second, 10*time.Millisecond, "Should eventually read message across idle timeouts")
	require.NoError(err)
	require.NotNil(msg)
	require.Equal(hsms.LinkTestReqType, msg.Type())
	require.Len(rawBody, 10)
}

// TestMessageReader_IdleTimeout_Partial verifies that when a timeout occurs mid-header,
// the reader correctly queues the remaining bytes in the next iteration.
func TestMessageReader_IdleTimeout_Partial(t *testing.T) {
	require := require.New(t)

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	reader := &messageReader{t8Timeout: 5 * time.Second, idleReadTimeout: 10 * time.Millisecond}

	go func() {
		frame := buildLinktestReqFrame()
		// Write first 2 bytes of the length header
		_, _ = server.Write(frame[:2])
		// Wait for timeout to fire on the remaining 2 bytes
		time.Sleep(50 * time.Millisecond)
		// Write the rest of the frame
		_, _ = server.Write(frame[2:])
	}()

	lenBuf := make([]byte, 4)
	var msg hsms.HSMSMessage
	var rawBody []byte
	var err error

	msgCh := make(chan struct{})
	go func() {
		msg, rawBody, err = reader.ReadMessage(client, lenBuf)
		close(msgCh)
	}()

	require.Eventually(func() bool {
		select {
		case <-msgCh:
			return true
		default:
			return false
		}
	}, 1*time.Second, 10*time.Millisecond, "Should eventually read message across partial header timeouts")

	require.NoError(err)
	require.NotNil(msg)
	require.Equal(hsms.LinkTestReqType, msg.Type())
	require.Len(rawBody, 10)
}

// TestMessageReader_ConnCloseDuringIdleRead verifies that closing the connection
// while the reader is looping on idle timeouts immediately unblocks and returns an error.
func TestMessageReader_ConnCloseDuringIdleRead(t *testing.T) {
	require := require.New(t)

	client, server := net.Pipe()
	defer server.Close()

	reader := &messageReader{t8Timeout: 5 * time.Second, idleReadTimeout: 50 * time.Millisecond}

	// Close the client side after it has entered the idle read loop.
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = client.Close()
	}()

	lenBuf := make([]byte, 4)
	msg, rawBody, err := reader.ReadMessage(client, lenBuf)
	require.Error(err)
	require.Nil(msg)
	require.Nil(rawBody)
}
