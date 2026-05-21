package hsmsss

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/arloliu/go-secs/hsms"
	"github.com/arloliu/go-secs/secs2"
)

// headerRejectError signals that an HSMS frame was received with an unsupported
// PType or SType. SEMI E37 §7.10.3 requires answering such a frame with a
// Reject.req; receiverTask peels this error off with errors.As, builds the
// Reject.req from its fields, and keeps the connection open.
type headerRejectError struct {
	sessionID   uint16
	pType       byte
	sType       byte
	systemBytes [4]byte
	reasonCode  byte
}

func (e *headerRejectError) Error() string {
	return fmt.Sprintf("unsupported HSMS header: pType=%d sType=%d (reject reason %d)",
		e.pType, e.sType, e.reasonCode)
}

// messageReader reads and decodes individual HSMS messages from a net.Conn.
//
// It implements the HSMS message framing protocol (SEMI E37 §7):
//  1. Read 4-byte big-endian message length (periodic deadline to avoid infinite blocking)
//  2. Validate length (non-zero, ≤ secs2.MaxByteSize)
//  3. Set T8 timeout and read the message payload
//  4. Decode into an HSMSMessage via hsms.DecodeMessage
//
// messageReader is NOT goroutine-safe. The caller must ensure that only one
// ReadMessage call is active at a time, consistent with the single-receiver
// design of an HSMS connection.
type messageReader struct {
	t8Timeout       time.Duration
	idleReadTimeout time.Duration
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
	// Phase 1: read the 4-byte length header with periodic deadline.
	//
	// Uses raw conn.Read with manual offset tracking instead of io.ReadFull
	// to correctly handle partial reads across deadline expirations.
	//
	// On each iteration a fresh deadline is set. If the deadline fires:
	//   - With 0 bytes read in that iteration → idle timeout, safe to loop
	//     (this lets runTaskLoop's ctx.Done() check fire between iterations).
	//   - With >0 bytes read → partial data arrived, continue reading remainder.
	// Any non-timeout error (EOF, connection reset, etc.) is fatal.
	idleTimeout := mr.idleReadTimeout
	if idleTimeout <= 0 {
		idleTimeout = 10 * time.Second
	}

	totalRead := 0
	for totalRead < 4 {
		if err = conn.SetReadDeadline(time.Now().Add(idleTimeout)); err != nil {
			return nil, nil, fmt.Errorf("set read deadline: %w", err)
		}

		n, readErr := conn.Read(lenBuf[totalRead:])
		totalRead += n

		if readErr != nil {
			if totalRead >= 4 {
				break // got all 4 bytes despite error
			}

			var netErr net.Error
			if errors.As(readErr, &netErr) && netErr.Timeout() {
				// Timeout — if some data arrived (n > 0), keep looping to read
				// remaining header bytes. If no data arrived (n == 0), this is a
				// clean idle timeout; loop to allow ctx.Done() check.
				continue
			}

			// Non-timeout error (EOF, reset, closed) → fatal
			return nil, nil, fmt.Errorf("read message length: %w", readErr)
		}
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

	if _, err = readFull(conn, rawBody, mr.t8Timeout); err != nil {
		return nil, nil, fmt.Errorf("read message payload: %w", err)
	}

	// Phase 3.5: detect unsupported PType/SType. Per SEMI E37 §7.10.3 these
	// must be answered with a Reject.req, not treated as a fatal decode error.
	// A frame shorter than the 10-byte header has no usable header and falls
	// through to DecodeMessage, which rejects it as a decode error.
	if len(rawBody) >= hsms.HeaderSize {
		pType := rawBody[4]
		sType := rawBody[5]
		if pType != 0 || !hsms.IsValidSType(sType) {
			reason := byte(hsms.RejectSTypeNotSupported)
			if pType != 0 {
				reason = hsms.RejectPTypeNotSupported
			}
			hre := &headerRejectError{
				sessionID:  binary.BigEndian.Uint16(rawBody[0:2]),
				pType:      pType,
				sType:      sType,
				reasonCode: reason,
			}
			copy(hre.systemBytes[:], rawBody[6:10])

			return nil, rawBody, hre
		}
	}

	// Phase 4: decode.
	msg, err = hsms.DecodeMessage(msgLen, rawBody)
	if err != nil {
		return nil, rawBody, fmt.Errorf("decode message: %w", err)
	}

	return msg, rawBody, nil
}

// readFull reads exactly len(buf) bytes from r into buf.
// Unlike io.ReadFull, it does not wrap a short-read EOF into
// io.ErrUnexpectedEOF — it returns the raw error from Read.
func readFull(r net.Conn, buf []byte, deadlineTimeout time.Duration) (int, error) {
	total := 0
	var n int
	var err error
	for total < len(buf) && err == nil {
		if deadlineTimeout > 0 {
			if errD := r.SetReadDeadline(time.Now().Add(deadlineTimeout)); errD != nil {
				return total, errD
			}
		}

		n, err = r.Read(buf[total:])
		total += n
	}
	if err != nil {
		return total, err
	}

	return total, nil
}
