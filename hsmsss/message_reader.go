package hsmsss

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/arloliu/go-secs/hsms"
	"github.com/arloliu/go-secs/secs2"
)

// messageReader reads and decodes individual HSMS messages from a net.Conn.
//
// It implements the HSMS message framing protocol (SEMI E37 §7):
//  1. Read 4-byte big-endian message length (no timeout — allows idle connections)
//  2. Validate length (non-zero, ≤ secs2.MaxByteSize)
//  3. Set T8 timeout and read the message payload
//  4. Decode into an HSMSMessage via hsms.DecodeMessage
//
// messageReader is NOT goroutine-safe. The caller must ensure that only one
// ReadMessage call is active at a time, consistent with the single-receiver
// design of an HSMS connection.
type messageReader struct {
	t8Timeout time.Duration
}

// ReadMessage reads one complete HSMS message from conn.
//
// lenBuf must be a 4-byte scratch buffer reused across calls to avoid
// per-message allocations. It is overwritten on each call.
//
// On success it returns the decoded message and the raw payload bytes
// (for trace logging via hsms.MsgHexString). The caller retains ownership
// of lenBuf, which contains the 4-byte length header after the call.
//
// On error the caller should inspect the error with isNetError to decide
// the appropriate log level. When decoding fails, rawBody is still returned
// to allow hex-dump logging of the malformed payload.
func (mr *messageReader) ReadMessage(conn net.Conn, lenBuf []byte) (msg hsms.HSMSMessage, rawBody []byte, err error) {
	// Phase 1: read the 4-byte length header.
	// No timeout — the connection is allowed to idle indefinitely between messages.
	if err = conn.SetReadDeadline(time.Time{}); err != nil {
		return nil, nil, fmt.Errorf("clear read deadline: %w", err)
	}

	if _, err = io.ReadFull(conn, lenBuf); err != nil {
		return nil, nil, fmt.Errorf("read message length: %w", err)
	}

	// Phase 2: validate the length.
	msgLen := binary.BigEndian.Uint32(lenBuf)

	if msgLen == 0 {
		return nil, nil, fmt.Errorf("HSMS message length is zero")
	}

	if msgLen > secs2.MaxByteSize {
		return nil, nil, fmt.Errorf("HSMS message length %d exceeds maximum %d", msgLen, secs2.MaxByteSize)
	}

	// Phase 3: read the payload with T8 timeout.
	if err = conn.SetReadDeadline(time.Now().Add(mr.t8Timeout)); err != nil {
		return nil, nil, fmt.Errorf("set T8 deadline: %w", err)
	}

	rawBody = make([]byte, msgLen)

	if _, err = io.ReadFull(conn, rawBody); err != nil {
		return nil, nil, fmt.Errorf("read message payload: %w", err)
	}

	// Phase 4: decode.
	msg, err = hsms.DecodeMessage(msgLen, rawBody)
	if err != nil {
		return nil, rawBody, fmt.Errorf("decode message: %w", err)
	}

	return msg, rawBody, nil
}
