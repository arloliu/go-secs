package secs1

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
)

// SECS-I line-control characters (SEMI E4 §7.8). These single-byte handshake characters coordinate
// the half-duplex block transfer direction on the wire.
const (
	enq byte = 0x05 // ENQ: request to send (initiator asks for line control)
	eot byte = 0x04 // EOT: ready to receive (granted in response to ENQ)
	ack byte = 0x06 // ACK: block correctly received
	nak byte = 0x15 // NAK: block incorrectly received (length/checksum/timeout)
)

// lineIO owns the raw half-duplex line for one connection generation: the captured conn, the SINGLE
// bufio.Reader that serves EVERY read on it, and the E4 timers/role the block transfer needs.
//
// G-A invariant (load-bearing): exactly ONE goroutine per generation reads and writes this conn,
// through this ONE reader, for ALL reads (poll, receive, and the send ACK wait). A second reader
// would steal buffered bytes from the first. bufio.Reader is safe here only because it is the sole
// reader and SECS-I is half-duplex — the peer never transmits past its ENQ before our EOT, so there
// is no stray read-ahead to lose. T3 constructs exactly one lineIO per generation and drives it from
// one goroutine; T1's tests construct it directly on a loopback conn.
//
// lineIO is NOT goroutine-safe; the single-goroutine ownership above is what makes it correct.
type lineIO struct {
	conn    net.Conn
	reader  *bufio.Reader           // THE sole reader for this conn/generation (see G-A invariant)
	now     func() time.Time        // injectable clock for the T1/T2 conn read deadlines (default time.Now)
	timers  func() hsms.TimerConfig // LIVE T1/T2 source; re-read on every use so UpdateConfigOptions(WithT1/WithT2) reaches the line engine
	isEquip bool                    // true = equipment (master); false = host (slave)
}

// newLineIO builds a lineIO over conn using timers' T1/T2 and cfg's equipment/host role. The bufio
// reader created here is the ONE reader for this conn (G-A invariant). The clock defaults to
// time.Now; tests inject l.now to drive the T1/T2 read deadlines deterministically. timers is called
// fresh on every I/O operation (never cached), so a live hsms.Connection.UpdateConfigOptions(WithT1(...))
// / WithT2(...)) takes effect on the NEXT block transaction of this generation, mirroring how
// hsmsss's readFrame reads T8 live via rt.Timers().
func newLineIO(conn net.Conn, cfg Config, timers func() hsms.TimerConfig) *lineIO {
	return &lineIO{
		conn:    conn,
		reader:  bufio.NewReader(conn),
		now:     time.Now,
		timers:  timers,
		isEquip: cfg.IsEquip(),
	}
}

// --- Low-level byte primitives (SEMI E4 §7.3) ---

// readByte reads one byte with the given timeout as an absolute read deadline. A timeout surfaces as
// os.ErrDeadlineExceeded / a net.Error with Timeout()==true from the underlying read.
func (l *lineIO) readByte(timeout time.Duration) (byte, error) {
	if err := l.conn.SetReadDeadline(l.now().Add(timeout)); err != nil {
		return 0, err
	}

	return l.reader.ReadByte()
}

// readFull reads exactly len(buf) bytes, resetting the T1 deadline before EACH read call. On TCP a
// single Read may return several bytes, so T1 is the SEMI E4 §7.3.1 inter-character deadline per
// read chunk (the timer restarts after each chunk), not per individual byte.
func (l *lineIO) readFull(buf []byte) error {
	for read := 0; read < len(buf); {
		if err := l.conn.SetReadDeadline(l.now().Add(l.timers().T1)); err != nil {
			return err
		}

		n, err := l.reader.Read(buf[read:])
		read += n

		if err != nil {
			return err
		}
	}

	return nil
}

// writeByte writes a single handshake character (ENQ/EOT/ACK/NAK).
func (l *lineIO) writeByte(b byte) error {
	_, err := l.conn.Write([]byte{b})

	return err
}

// writeAll writes all of data to the conn, looping over short writes.
func (l *lineIO) writeAll(data []byte) error {
	for written := 0; written < len(data); {
		n, err := l.conn.Write(data[written:])
		written += n

		if err != nil {
			return err
		}
	}

	return nil
}

// drainUntilSilence reads and discards bytes until the line is silent for T1. Per SEMI E4 §7.8.5,
// after detecting a bad block (invalid length or checksum mismatch) the receiver keeps listening
// until the sender stops; silence is a T1 gap with no byte arriving.
func (l *lineIO) drainUntilSilence() {
	buf := make([]byte, 256)
	for {
		_ = l.conn.SetReadDeadline(l.now().Add(l.timers().T1))
		if _, err := l.reader.Read(buf); err != nil {
			return // T1 elapsed with no data — the line is silent
		}
	}
}

// --- Receive (SEMI E4 §7.8.4–5) ---

// receiveBlock reads and validates one SECS-I block. The caller must have ALREADY sent EOT in
// response to the peer's ENQ. The steps, per SEMI E4 §7.8.5:
//
//  1. Read the length byte with T2. On timeout: NAK, return ErrT2Timeout.
//  2. Validate the length range [minBlockLength, maxBlockLength]. On failure: drain, NAK,
//     return ErrInvalidLength.
//  3. Read header+body+checksum into a FRESH owned buffer with a per-read T1 deadline. On timeout:
//     NAK, return ErrT1Timeout.
//  4. parseBlock (length + checksum). On failure: drain, NAK, return the parse error.
//  5. On success: ACK and return the block.
//
// D5b-8: the read buffer is freshly make'd per call and parseBlock aliases it (zero-copy body); the
// assembler coalesces the body in T5, so the alias is safe until then.
func (l *lineIO) receiveBlock(ctx context.Context) (block, error) {
	select {
	case <-ctx.Done():
		return block{}, ctx.Err()
	default:
	}

	// Step 1: length byte with T2 (§7.8.5: "If T2 is exceeded while waiting for the length
	// character ... an NAK is sent").
	lengthByte, err := l.readByte(l.timers().T2)
	if err != nil {
		_ = l.writeByte(nak)

		return block{}, fmt.Errorf("%w: waiting for length byte: %w", ErrT2Timeout, err)
	}

	// Step 2: validate the length range (§7.6: 10 <= N <= 254). On an invalid length the receiver
	// keeps listening (drain) before the NAK (§7.8.5).
	n := int(lengthByte)
	if n < minBlockLength || n > maxBlockLength {
		l.drainUntilSilence()
		_ = l.writeByte(nak)

		return block{}, fmt.Errorf("%w: length byte %d out of [%d, %d]", ErrInvalidLength, n, minBlockLength, maxBlockLength)
	}

	// Step 3: header+body+checksum with a per-read T1 deadline (§7.3.1). §7.8.5: "if T1 is exceeded
	// between characters being received, then an NAK is sent".
	buf := make([]byte, n+checksumSize)
	if err := l.readFull(buf); err != nil {
		_ = l.writeByte(nak)

		return block{}, fmt.Errorf("%w: reading block data: %w", ErrT1Timeout, err)
	}

	// Step 4: parse + checksum. §7.8.5: on a checksum mismatch the receiver keeps listening (drain)
	// before the NAK.
	blk, err := parseBlock(lengthByte, buf)
	if err != nil {
		l.drainUntilSilence()
		_ = l.writeByte(nak)

		return block{}, err
	}

	// Step 5: valid block — ACK (§7.8.5).
	if err := l.writeByte(ack); err != nil {
		return blk, fmt.Errorf("secs1: failed to send ACK: %w", err)
	}

	return blk, nil
}

// --- Send (SEMI E4 §7.8.2–3) ---

// sendResult classifies the outcome of one sendBlockOnce attempt so the T2 RTY loop can decide
// whether to retry, yield (slave contention), or abort.
//
// T1/T2 SEAM: sendBlockOnce performs a SINGLE line-control-and-send attempt and returns one of
// these. T2 wraps it with the retry loop:
//   - sendOK         => success; return.
//   - sendRetry      => retryable (T2 timeout, NAK, non-ACK); increment the retry counter and
//     re-attempt up to RetryLimit, then fail.
//   - sendContention => this end is the slave and the peer contended (ENQ). sendBlockOnce only
//     DETECTS this; T2 owns the yield ACTION (send EOT, receiveBlock, deliver the peer's block,
//     then re-attempt this send with the retry counter reset per §7.8.2.1).
//   - sendAbort      => non-retryable (write error or context cancellation); return the error.
type sendResult int

const (
	sendOK         sendResult = iota // block sent and ACK'd
	sendRetry                        // retryable failure (T2 timeout, NAK, non-ACK)
	sendContention                   // slave detected contention (T2 performs the yield)
	sendAbort                        // non-retryable failure (write error, context cancelled)
)

// sendBlockOnce performs ONE line-control handshake and block transmission attempt (SEMI E4 §7.8.2):
//
//   - Send ENQ.
//   - Wait, bounded by T2, for the peer's response, ignoring noise per §7.8.2.1:
//   - EOT  => transmit the block and wait for ACK (see sendBlockData).
//   - ENQ  => contention. A master (isEquip) IGNORES it and keeps waiting for EOT; a slave returns
//     sendContention WITHOUT acting (the yield is T2's — see sendResult).
//   - any other byte => ignored; keep waiting within the remaining T2 budget.
//   - T2 elapses with no EOT => sendRetry (ErrT2Timeout).
//
// It does NOT retry and does NOT perform the slave yield — those are T2. If ctx is cancelled while
// waiting, it returns sendAbort with ctx.Err().
func (l *lineIO) sendBlockOnce(ctx context.Context, blk block) (sendResult, error) {
	// Step 1: request line control.
	if err := l.writeByte(enq); err != nil {
		return sendAbort, fmt.Errorf("secs1: send ENQ: %w", err)
	}

	// Step 2: wait for the response within T2, looping past ignored bytes (§7.8.2.1).
	deadline := l.now().Add(l.timers().T2)
	for {
		select {
		case <-ctx.Done():
			return sendAbort, ctx.Err()
		default:
		}

		remaining := deadline.Sub(l.now())
		if remaining <= 0 {
			return sendRetry, ErrT2Timeout
		}

		b, err := l.readByte(remaining)
		if err != nil {
			return sendRetry, fmt.Errorf("%w: waiting for EOT after ENQ: %w", ErrT2Timeout, err)
		}

		switch {
		case b == eot:
			// Line granted — transmit and wait for ACK.
			return l.sendBlockData(blk)

		case b == enq && !l.isEquip:
			// Slave contention (§7.8.2.1): DETECT only. T2 performs the yield.
			return sendContention, nil

		default:
			// Master ignores everything but EOT; slave ignores everything but ENQ/EOT (§7.8.2.1).
			continue
		}
	}
}

// sendBlockData transmits the packed block and waits for ACK within T2 (SEMI E4 §7.8.3):
//
//  1. Write the wire frame [lengthByte][header][body][checksum].
//  2. Wait up to T2 for one byte: ACK => sendOK; any non-ACK byte => sendRetry; a read error or T2
//     timeout => sendRetry (ErrT2Timeout).
//
// A write error is non-retryable (sendAbort). Per §7.8.3, characters received before the last
// checksum byte are ignored — the caller drained them within the T2 wait for EOT.
func (l *lineIO) sendBlockData(blk block) (sendResult, error) {
	if err := l.writeAll(blk.appendTo(nil)); err != nil {
		return sendAbort, fmt.Errorf("secs1: send block data: %w", err)
	}

	b, err := l.readByte(l.timers().T2)
	if err != nil {
		return sendRetry, fmt.Errorf("%w: waiting for ACK: %w", ErrT2Timeout, err)
	}

	if b == ack {
		return sendOK, nil
	}

	// Non-ACK within T2 (§7.8.2.2) — retryable.
	return sendRetry, fmt.Errorf("secs1: expected ACK (0x%02X), got 0x%02X", ack, b)
}

// sendBlock sends one block with the full SEMI E4 §7.8.2 retry/contention discipline, wrapping the
// single-attempt sendBlockOnce in the RTY loop and owning the slave-yield ACTION that sendBlockOnce
// only detects (see sendResult).
//
// The block is attempted up to retryLimit+1 times before ErrSendFailed (the loop bound is
// retry <= retryLimit — load-bearing). On slave contention the yield is performed here, not in
// sendBlockOnce: grant the master line control (EOT), take its block, deliver it, and reset the
// retry counter per §7.8.2.1 ("the postponed block Send may be sent as if it were a new send
// request"). A master (isEquip) never returns sendContention, so it never yields.
//
// Parameters:
//   - blk:        the block to transmit.
//   - retryLimit: the engine's cfg.RetryLimit() (lineIO never re-reads config); the block is sent
//     up to retryLimit+1 times.
//   - deliver:    the engine's inbound sink (T3). A block received while yielding to a contending
//     master is a REAL inbound message block and MUST be delivered, not dropped (P1).
//
// Returns:
//   - error: nil on ACK; ErrSendFailed once the retries are exhausted; ctx.Err() on cancellation; a
//     wrapped write error (non-retryable) otherwise. A deliver error during a contention yield is
//     NON-FATAL (it never fails the send) — see the sendContention branch.
func (l *lineIO) sendBlock(ctx context.Context, blk block, retryLimit int, deliver func(block) error) error {
	retry := 0
	for retry <= retryLimit {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		result, err := l.sendBlockOnce(ctx, blk)
		switch result {
		case sendOK:
			return nil

		case sendContention:
			// Slave-yield ACTION (§7.8.2.1): sendBlockOnce DETECTED the master's contending ENQ
			// but did not act. Grant the master line control with EOT, then take its block.
			if werr := l.writeByte(eot); werr != nil {
				return fmt.Errorf("secs1: send EOT (contention yield): %w", werr)
			}

			recv, rerr := l.receiveBlock(ctx)
			if rerr != nil {
				// Anti-starvation (§7.8.2.1): a failed receive of the master's block is a normal
				// retry — do NOT reset the counter and do NOT deliver.
				retry++

				continue
			}

			// P1: the block received during the yield is a REAL inbound message block; deliver it. A
			// deliver (decode/route) error is NON-FATAL — the link stays up, mirroring the idle inbound
			// path (transport.lineEngine's `_ = sink(blk)`): an inbound decode/route failure must not
			// fail this OUTBOUND send, which would drive a spurious D5b-10 line teardown.
			_ = deliver(recv)

			// §7.8.2.1: the postponed send restarts as a new send request.
			retry = 0

			continue

		case sendRetry:
			retry++

			continue

		case sendAbort:
			return err

		default:
			// Unreachable: sendResult is a closed enum fully handled above.
		}
	}

	return ErrSendFailed
}
