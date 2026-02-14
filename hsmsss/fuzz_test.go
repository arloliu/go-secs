package hsmsss

import (
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/arloliu/go-secs/secs2"
)

// FuzzMessageReader_ReadMessage fuzzes the HSMS message framing reader.
//
// It pipes arbitrary bytes through a net.Pipe into messageReader.ReadMessage
// and verifies the method never panics. The write end is closed after writing
// to ensure ReadMessage always returns (no blocking on incomplete data).
//
// This covers:
//   - 4-byte length header parsing (zero, oversized, partial)
//   - T8 deadline enforcement on payload read
//   - Decode path through hsms.DecodeMessage
//   - rawBody return behavior on decode errors
func FuzzMessageReader_ReadMessage(f *testing.F) {
	// Seed: valid linktest.req frame (reuses helper from message_reader_test.go)
	f.Add(buildLinktestReqFrame())

	// Seed: zero-length message
	f.Add([]byte{0x00, 0x00, 0x00, 0x00})

	// Seed: incomplete length header (< 4 bytes)
	f.Add([]byte{0x00, 0x01})

	// Seed: empty input
	f.Add([]byte{})

	// Seed: valid length but payload too short for HSMS header decode
	short := make([]byte, 9)
	binary.BigEndian.PutUint32(short[:4], 5)
	copy(short[4:], []byte{0x01, 0x02, 0x03, 0x04, 0x05})
	f.Add(short)

	// Seed: oversized length (0xFFFFFFFF)
	oversized := make([]byte, 4)
	binary.BigEndian.PutUint32(oversized, 0xFFFFFFFF)
	f.Add(oversized)

	// Seed: length at MaxByteSize boundary
	boundary := make([]byte, 4)
	binary.BigEndian.PutUint32(boundary, secs2.MaxByteSize)
	f.Add(boundary)

	// Seed: valid frame with data message (S1F1 + ASCII item)
	s1f1 := []byte{
		0x00, 0x01, 0x81, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x03,
		0x41, 0x05, 'h', 'e', 'l', 'l', 'o',
	}
	frame := make([]byte, 4+len(s1f1))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(s1f1)))
	copy(frame[4:], s1f1)
	f.Add(frame)

	f.Fuzz(func(t *testing.T, data []byte) {
		client, server := net.Pipe()

		// Write fuzz data and close write end to prevent blocking.
		go func() {
			defer server.Close()
			_, _ = server.Write(data)
		}()
		defer client.Close()

		reader := &messageReader{t8Timeout: 50 * time.Millisecond}
		lenBuf := make([]byte, 4)

		// Must not panic.
		msg, rawBody, err := reader.ReadMessage(client, lenBuf)
		if err == nil && msg == nil {
			t.Fatal("ReadMessage returned nil msg and nil err")
		}

		// If decode succeeded, rawBody must be non-nil.
		if err == nil && rawBody == nil {
			t.Fatal("ReadMessage returned nil rawBody on success")
		}
	})
}
