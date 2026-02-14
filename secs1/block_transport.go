package secs1

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"time"

	"github.com/arloliu/go-secs/logger"
)

// blockTransport handles the SECS-I block transfer protocol (SEMI E4 §7).
//
// It provides SendBlock and ReceiveBlock operations implementing the full
// line-control handshake, contention resolution, and retry logic.
//
// This type is NOT goroutine-safe. The caller (protocol loop) must ensure
// that only one operation is active at a time, consistent with the
// half-duplex nature of SECS-I.
type blockTransport struct {
	conn   net.Conn
	reader *bufio.Reader
	cfg    *ConnectionConfig
	logger logger.Logger

	// onRecvBlock is called when a block is successfully received.
	// This includes blocks received during contention yield (when Slave
	// defers its send to receive Master's block first).
	// The callback is invoked synchronously within the protocol loop.
	onRecvBlock func(*Block)

	// onRetry is called each time a block send is retried (T2 timeout,
	// NAK, or non-ACK response). Used for metrics collection.
	onRetry func()
}

// newBlockTransport creates a blockTransport for the given connection.
//
// onRecv is called with each successfully received block. It may be nil
// if receive delivery is not yet wired (e.g. in unit tests for send-only paths).
func newBlockTransport(
	conn net.Conn,
	cfg *ConnectionConfig,
	l logger.Logger,
	onRecv func(*Block),
	onRetry func(),
) *blockTransport {
	return &blockTransport{
		conn:        conn,
		reader:      bufio.NewReader(conn),
		cfg:         cfg,
		logger:      l,
		onRecvBlock: onRecv,
		onRetry:     onRetry,
	}
}

// --- Low-level I/O helpers ---

// readByte reads a single byte from the connection with the given timeout.
// Returns os.ErrDeadlineExceeded (or net.Error with Timeout()=true) on timeout.
func (bt *blockTransport) readByte(timeout time.Duration) (byte, error) {
	if err := bt.conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return 0, err
	}

	return bt.reader.ReadByte()
}

// readFull reads exactly len(buf) bytes, applying t1 as a per-read-call deadline.
//
// On TCP, Read() may return multiple bytes at once, so T1 is the deadline
// for each Read() call rather than per individual byte.
// The deadline is reset before each read call to implement the SEMI E4 §7.3.1
// inter-character timeout correctly: the timer restarts after each chunk of data.
func (bt *blockTransport) readFull(buf []byte, t1 time.Duration) error {
	for read := 0; read < len(buf); {
		if err := bt.conn.SetReadDeadline(time.Now().Add(t1)); err != nil {
			return err
		}

		n, err := bt.reader.Read(buf[read:])
		read += n

		if err != nil {
			return err
		}
	}

	return nil
}

// writeByte writes a single handshake byte (ENQ, EOT, ACK, or NAK).
func (bt *blockTransport) writeByte(b byte) error {
	_, err := bt.conn.Write([]byte{b})

	return err
}

// writeAll writes all bytes in data to the connection.
func (bt *blockTransport) writeAll(data []byte) error {
	for written := 0; written < len(data); {
		n, err := bt.conn.Write(data[written:])
		written += n

		if err != nil {
			return err
		}
	}

	return nil
}

// drainUntilSilence reads and discards bytes until the line is silent for T1.
//
// Per SEMI E4 §7.8.5, after detecting an error in a received block (invalid
// length or checksum mismatch), the receiver continues listening for characters
// to ensure the sender has finished transmitting. Silence is detected when no
// byte arrives within the T1 inter-character timeout.
func (bt *blockTransport) drainUntilSilence() {
	buf := make([]byte, 256)

	for {
		_ = bt.conn.SetReadDeadline(time.Now().Add(bt.cfg.t1Timeout))

		_, err := bt.reader.Read(buf)
		if err != nil {
			return // T1 expired with no data, line is silent
		}
	}
}

// --- Receive ---

// receiveBlock reads and validates a single SECS-I block from the wire.
//
// The caller must have already sent EOT (in response to a remote ENQ) before
// calling this method. receiveBlock handles:
//
//  1. Reading the length byte with T2 timeout (SEMI E4 §7.8.4).
//  2. Reading header + body + checksum with T1 per-read timeout (SEMI E4 §7.3.1).
//  3. Validating the block via ParseBlock (length range + checksum).
//  4. Sending ACK on success or NAK on failure (SEMI E4 §7.8.5).
//
// On invalid length or checksum mismatch, the receiver drains the line until
// T1 silence before sending NAK, per SEMI E4 §7.8.5.
func (bt *blockTransport) receiveBlock(ctx context.Context) (*Block, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	// Step 1: Read length byte with T2 timeout.
	// Per SEMI E4 §7.8.5: "If T2 is exceeded while waiting for the length
	// character ... an NAK is sent."
	lengthByte, err := bt.readByte(bt.cfg.t2Timeout)
	if err != nil {
		_ = bt.writeByte(NAK)

		return nil, fmt.Errorf("%w: waiting for length byte: %w", ErrT2Timeout, err)
	}

	length := int(lengthByte)

	// Validate length range per SEMI E4 §7.6: 10 <= N <= 254.
	// Per §7.8.5: "If the length byte is invalid ... the receiver continues
	// to listen for characters" (drain), then sends NAK.
	if length < MinBlockLength || length > MaxBlockLength {
		bt.drainUntilSilence()
		_ = bt.writeByte(NAK)

		return nil, fmt.Errorf("%w: got %d, want %d-%d", ErrInvalidLength, length, MinBlockLength, MaxBlockLength)
	}

	// Step 2: Read remaining bytes (header + body + 2 checksum bytes).
	// T1 is applied per read call, per SEMI E4 §7.3.1.
	// Per §7.8.5: "if T1 is exceeded between characters being received,
	// then an NAK is sent."
	remaining := length + checksumSize
	buf := make([]byte, remaining)

	if err := bt.readFull(buf, bt.cfg.t1Timeout); err != nil {
		_ = bt.writeByte(NAK)

		return nil, fmt.Errorf("%w: reading block data: %w", ErrT1Timeout, err)
	}

	// Step 3: Parse and validate (includes checksum verification).
	// Per §7.8.5: "If the received checksum does not agree ... the receiver
	// continues to listen for characters" (drain), then sends NAK.
	block, err := ParseBlock(lengthByte, buf)
	if err != nil {
		bt.drainUntilSilence()
		_ = bt.writeByte(NAK)

		return nil, err
	}

	// Step 4: Valid block — send ACK.
	// Per §7.8.5: "After a block is correctly received, an ACK character is sent."
	if err := bt.writeByte(ACK); err != nil {
		return block, fmt.Errorf("secs1: failed to send ACK: %w", err)
	}

	return block, nil
}

// --- Send ---

// sendResult classifies the outcome of a single lineControlAndSend attempt
// so the retry loop can decide whether to retry, reset, or abort.
type sendResult int

const (
	sendOK         sendResult = iota // Block sent and ACK'd.
	sendRetry                        // Retryable failure (T2 timeout, NAK, non-ACK).
	sendContention                   // Contention detected; Slave yielded and received Master's block.
	sendAbort                        // Non-retryable failure (write error, context cancelled).
)

// sendBlock sends a single SECS-I block using the full block transfer protocol
// (SEMI E4 §7).
//
// The method performs line control (ENQ/EOT handshake), transmits the block data,
// waits for ACK, and retries up to cfg.retryLimit times on retryable failures
// (T2 timeout after ENQ, T2 timeout after block, NAK, non-ACK response).
//
// On contention (this end is Slave per SEMI E4 §7.5), it yields to receive the
// Master's block, then retries the send with the retry counter reset to zero,
// per §7.8.2.1: "the postponed block Send may be sent as if it were a new send
// request."
//
// Returns nil on success or ErrSendFailure if all retries are exhausted.
func (bt *blockTransport) sendBlock(ctx context.Context, block *Block) error {
	retry := 0

	for retry <= bt.cfg.retryLimit {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		result, err := bt.lineControlAndSend(ctx, block)

		switch result {
		case sendOK:
			return nil

		case sendContention:
			// Per §7.8.2.1: retry as a new send (reset counter).
			bt.logger.Debug("secs1: contention resolved, retrying send",
				"deviceID", block.DeviceID(),
				"systemBytes", block.SystemBytesUint32(),
			)

			retry = 0

			continue

		case sendRetry:
			retry++
			if bt.onRetry != nil {
				bt.onRetry()
			}
			bt.logger.Debug("secs1: send retry",
				"retry", retry,
				"maxRetry", bt.cfg.retryLimit,
				"error", err,
			)

			continue

		case sendAbort:
			return err
		}
	}

	return ErrSendFailure
}

// lineControlAndSend performs one attempt of the line-control handshake followed
// by block transmission.
//
// Per SEMI E4 §7.8.2:
//   - Send ENQ.
//   - Wait for response within T2:
//   - EOT → proceed to Send state.
//   - ENQ (contention): Master ignores; Slave yields.
//   - Any other byte: Master ignores all except EOT; Slave ignores all except ENQ/EOT.
//   - T2 timeout → retryable failure.
func (bt *blockTransport) lineControlAndSend(ctx context.Context, block *Block) (sendResult, error) {
	// Step 1: Send ENQ.
	if err := bt.writeByte(ENQ); err != nil {
		return sendAbort, fmt.Errorf("secs1: send ENQ: %w", err)
	}

	// Step 2: Wait for response within T2, with a read loop to handle
	// ignored bytes per §7.8.2.1.
	deadline := time.Now().Add(bt.cfg.t2Timeout)

	for {
		select {
		case <-ctx.Done():
			return sendAbort, ctx.Err()
		default:
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return sendRetry, ErrT2Timeout
		}

		b, err := bt.readByte(remaining)
		if err != nil {
			// T2 timeout waiting for EOT.
			return sendRetry, ErrT2Timeout
		}

		switch {
		case b == EOT:
			// Proceed to Send state.
			return bt.sendBlockData(block)

		case b == ENQ && !bt.cfg.IsEquip():
			// Slave: contention detected (§7.8.2.1).
			// Yield: send EOT, receive Master's block, then retry our send.
			return bt.handleContentionAsSlave(ctx)

		default:
			// Per §7.8.2.1:
			//   Master: "can ignore all characters except an EOT"
			//   Slave:  "can ignore all characters except an ENQ or EOT"
			// Loop to keep reading with remaining T2 time.
			continue
		}
	}
}

// handleContentionAsSlave handles the contention case for the Slave (Host) end.
//
// Per SEMI E4 §7.8.2.1:
//  1. Send EOT (accept Master's request to send).
//  2. Receive Master's block.
//  3. Return sendContention so the outer retry loop resets and retries.
func (bt *blockTransport) handleContentionAsSlave(ctx context.Context) (sendResult, error) {
	// Step 1: Send EOT to accept Master's transmission.
	if err := bt.writeByte(EOT); err != nil {
		return sendAbort, fmt.Errorf("secs1: send EOT (contention yield): %w", err)
	}

	// Step 2: Receive Master's block.
	block, err := bt.receiveBlock(ctx)
	if err != nil {
		bt.logger.Debug("secs1: failed to receive master's block during contention", "error", err)
		// Per §7.8.2.1: "postpones the send of its block until it receives
		// a block from the master." If receive fails, do NOT reset retry
		// counter — treat as a normal retry to prevent infinite starvation.
		return sendRetry, nil //nolint:nilerr
	}

	// Step 3: Deliver the received block (if successful).
	if block != nil && bt.onRecvBlock != nil {
		bt.onRecvBlock(block)
	}

	return sendContention, nil
}

// sendBlockData transmits the block data and waits for ACK.
//
// Per SEMI E4 §7.8.3:
//  1. Send Length Byte + Header + Body + Checksum (the Pack'd wire format).
//  2. Wait for ACK within T2.
//  3. ACK → success. Non-ACK or T2 timeout → retryable failure.
//
// Per §7.8.3: "characters received prior to sending the last checksum byte
// can be ignored."
func (bt *blockTransport) sendBlockData(block *Block) (sendResult, error) {
	wireData := block.Pack()

	if err := bt.writeAll(wireData); err != nil {
		return sendAbort, fmt.Errorf("secs1: send block data: %w", err)
	}

	// Wait for ACK within T2.
	// Per §7.8.2.2: "the time between sending the second checksum byte and
	// receiving any character exceeds T2" → retry.
	b, err := bt.readByte(bt.cfg.t2Timeout)
	if err != nil {
		return sendRetry, ErrT2Timeout
	}

	if b == ACK {
		return sendOK, nil
	}

	// Per §7.8.2.2: "a non-ACK character is received within time T2 after
	// sending the second checksum byte" → retry.
	return sendRetry, fmt.Errorf("secs1: expected ACK (0x%02X), got 0x%02X", ACK, b)
}
