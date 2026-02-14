# SECS-I Block-Transfer State Machine — Detailed Design

> This document provides a detailed, code-level design for the block-transfer protocol (SEMI E4 §7, Figure 2) adapted for TCP/IP, as implemented in the `secs1` package.

---

## 1. Overview

The block-transfer protocol is the **heart** of SECS-I. It is a half-duplex, handshaked protocol that manages the direction of communication and ensures reliable block delivery. The protocol has **five states**: Idle, Line Control, Send, Receive, and Completion.

Both the sender and receiver execute the **same algorithm** (Figure 2), but exit via different paths depending on whether the local application has a block to send or the remote side initiates with ENQ.

---

## 2. Wire Bytes

```
ENQ = 0x05   Request to Send
EOT = 0x04   Ready to Receive
ACK = 0x06   Correct Reception
NAK = 0x15   Incorrect Reception
```

These are **single-byte** control characters exchanged on the TCP stream, interleaved with block data.

---

## 3. State Descriptions

### 3.1 Idle State

Both sides start in Idle. Two exits:

| Trigger | Action | Next State |
|---|---|---|
| Application has a block to send | Send ENQ | Line Control |
| Received ENQ from remote | Send EOT (if ready) | Receive |

```go
type protocolState int

const (
    stateIdle protocolState = iota
    stateLineControl
    stateSend
    stateReceive
    stateCompletion
)
```

### 3.2 Line Control State (Sender side)

Entered after sending ENQ. Handles handshake negotiation and contention.

```
flowchart:
  1. Send ENQ
  2. Start T2 timer
  3. Wait for response:
     a. EOT received        → go to Send state
     b. ENQ received (contention):
        - If Master: ignore, continue waiting for EOT
        - If Slave:  yield → send EOT → go to Receive state
                     after receive completes, re-enter Line Control
     c. T2 timeout          → increment retryCount
        - retryCount <= RTY → re-send ENQ (go to step 1)
        - retryCount > RTY  → SEND FAILURE, notify app, go to Idle
     d. Any other byte      → ignore (per spec: master ignores all except EOT;
                               slave ignores all except ENQ and EOT)
```

**Implementation**:

```go
func (s *blockSender) lineControl(ctx context.Context, block *Block) error {
    retryCount := 0

    for {
        // Step 1: Send ENQ
        if err := s.writeByte(ENQ); err != nil {
            return fmt.Errorf("failed to send ENQ: %w", err)
        }

        // Step 2-3: Wait for response within T2
        b, err := s.readByteWithTimeout(s.cfg.t2Timeout)
        if err != nil {
            // T2 timeout
            retryCount++
            if retryCount > s.cfg.retryLimit {
                return ErrSendFailure
            }
            continue
        }

        switch b {
        case EOT:
            // Proceed to Send state
            return s.sendBlock(ctx, block)

        case ENQ:
            // Contention detected
            if s.cfg.IsEquip() {
                // Equipment (Master): ignore ENQ, keep waiting for EOT
                // Read again with remaining T2 time
                continue
            }
            // Slave: yield
            if err := s.writeByte(EOT); err != nil {
                return err
            }
            // Enter receive state for master's block
            if err := s.conn.receiver.receiveBlock(ctx); err != nil {
                // Receive failed, but we still need to retry our send
            }
            // After receiving master's block, retry our send from ENQ
            retryCount = 0
            continue

        default:
            // Ignore unexpected bytes per spec
            continue
        }
    }
}
```

### 3.3 Send State

Entered after receiving EOT. Transmits the block data.

```
flowchart:
  1. Send Length Byte (N = len(Header) + len(Body))
  2. Send Header (10 bytes)
  3. Send Body (0-244 bytes)
  4. Send Checksum (2 bytes, high byte first)
  5. Start T2 timer
  6. Wait for response:
     a. ACK received         → SUCCESS, notify message protocol, go to Idle
     b. NAK received         → increment retryCount, go to Line Control
     c. Non-ACK received     → increment retryCount, go to Line Control
     d. T2 timeout           → increment retryCount, go to Line Control
     e. retryCount > RTY     → SEND FAILURE, notify app, go to Idle
```

**Note**: Per §7.8.3, characters received **prior to sending the last checksum byte** can be ignored.

```go
func (s *blockSender) sendBlock(ctx context.Context, block *Block) error {
    // Pack the block into wire format
    wireData := block.Pack() // [length][header][body][checksum_hi][checksum_lo]

    // Send all bytes
    if _, err := s.write(wireData); err != nil {
        return fmt.Errorf("failed to send block: %w", err)
    }

    // After sending last checksum byte, wait for ACK within T2
    b, err := s.readByteWithTimeout(s.cfg.t2Timeout)
    if err != nil {
        // T2 timeout after sending block → retry via Line Control
        return ErrT2Timeout
    }

    if b == ACK {
        return nil // SUCCESS
    }

    // NAK or non-ACK → retry via Line Control
    return ErrNonACK
}
```

### 3.4 Receive State

Entered after sending EOT (in response to remote's ENQ).

```
flowchart:
  1. Start T2 timer (waiting for length byte)
  2. Read length byte:
     a. T2 timeout           → send NAK, go to Idle
     b. Received length byte → validate: 10 <= N <= 254
        - Invalid length     → drain until T1 silence, send NAK, go to Idle
        - Valid length       → continue
  3. Start T1 timer
  4. Read N + 2 bytes (Header + Body + 2 Checksum), resetting T1 after each byte/read:
     a. T1 timeout between reads → send NAK, go to Idle
  5. Validate checksum:
     a. Match      → go to Completion (good)
     b. Mismatch   → drain until T1 silence, go to Completion (bad)
```

**TCP Adaptation**: On TCP, `Read()` may return multiple bytes at once. We handle T1 as the deadline for each `Read()` call, not per individual byte. This is acceptable because TCP guarantees in-order delivery; if bytes are available, they arrive together.

```go
func (r *blockReceiver) receiveBlock(ctx context.Context) (*Block, error) {
    // Step 1-2: Read length byte with T2 timeout
    r.conn.SetReadDeadline(time.Now().Add(r.cfg.t2Timeout))
    lengthByte, err := r.readByte()
    if err != nil {
        r.writeByte(NAK)
        return nil, ErrT2Timeout
    }

    // Validate length byte
    length := int(lengthByte)
    if length < 10 || length > 254 {
        r.drainUntilT1Silence()
        r.writeByte(NAK)
        return nil, ErrInvalidLength
    }

    // Step 3-4: Read remaining bytes (length + 2 checksum) with T1 deadline per read
    remaining := length + 2 // header+body + 2 checksum bytes
    buf := make([]byte, 0, remaining)

    for len(buf) < remaining {
        r.conn.SetReadDeadline(time.Now().Add(r.cfg.t1Timeout))
        chunk := make([]byte, remaining-len(buf))
        n, err := r.conn.Read(chunk)
        if err != nil {
            r.writeByte(NAK)
            return nil, ErrT1Timeout
        }
        buf = append(buf, chunk[:n]...)
    }

    // Step 5: Parse and validate
    block, err := ParseBlock(lengthByte, buf)
    if err != nil {
        r.drainUntilT1Silence()
        r.writeByte(NAK)
        return nil, err
    }

    // Checksum validation
    expectedChecksum := binary.BigEndian.Uint16(buf[length:length+2])
    actualChecksum := block.CalculateChecksum()
    if expectedChecksum != actualChecksum {
        r.drainUntilT1Silence()
        r.writeByte(NAK)
        return nil, ErrChecksumMismatch
    }

    // SUCCESS: send ACK
    r.writeByte(ACK)
    return block, nil
}
```

### 3.5 Completion (Receive side)

**Good block**: Send ACK, pass to message protocol.

**Bad block** (invalid length or checksum mismatch):
1. **Continue listening** for characters to drain the line (the sender may still be transmitting).
2. Reset a timer `t=0` on each received character.
3. When T1 expires with no new characters (line is silent) → send NAK.
4. Discard all received data for this block.

```go
// drainUntilT1Silence reads and discards bytes until no byte arrives within T1.
func (r *blockReceiver) drainUntilT1Silence() {
    buf := make([]byte, 256)
    for {
        r.conn.SetReadDeadline(time.Now().Add(r.cfg.t1Timeout))
        _, err := r.conn.Read(buf)
        if err != nil {
            // T1 expired with no data → line is silent
            return
        }
        // Got data, keep draining
    }
}
```

---

## 4. Contention Resolution

Contention occurs when both sides send ENQ simultaneously.

### 4.1 Detection

The Slave detects contention when, after sending ENQ, it receives ENQ instead of EOT.

### 4.2 Master Behavior (Equipment)

After sending ENQ:
- **Ignores all characters except EOT.**
- The received ENQ from Slave is treated as noise.
- Continues waiting for EOT within T2.

### 4.3 Slave Behavior (Host)

After sending ENQ, if ENQ is received:
1. **Stop sending** (postpone its block).
2. **Send EOT** (accept Master's request to send).
3. **Enter Receive state** for Master's block.
4. After receiving Master's block successfully, **retry its own send** from the beginning (ENQ).

### 4.4 Implementation Notes

```go
type halfDuplexCtl struct {
    mu              sync.Mutex
    state           atomic.Uint32 // 0=idle, 1=sending, 2=receiving
    pendingSendChan chan *Block    // blocks postponed due to contention
}

func (h *halfDuplexCtl) acquireSend() bool {
    return h.state.CompareAndSwap(0, 1)
}

func (h *halfDuplexCtl) acquireReceive() bool {
    return h.state.CompareAndSwap(0, 2)
}

func (h *halfDuplexCtl) release() {
    h.state.Store(0)
}
```

The half-duplex control ensures that at any time, only one goroutine is actively reading from / writing to the TCP stream for protocol purposes.

---

## 5. Retry Mechanism

| Condition | Action |
|---|---|
| T2 timeout after ENQ (no EOT) | Retry: re-send ENQ, increment retryCount |
| T2 timeout after block sent (no response) | Retry: re-enter Line Control, re-send ENQ |
| Non-ACK received after block sent | Retry: re-enter Line Control, re-send ENQ |
| retryCount > RTY | **Send Failure**: abort, notify application |

```go
func (s *blockSender) sendBlock(ctx context.Context, block *Block) error {
    for retryCount := 0; retryCount <= s.cfg.retryLimit; retryCount++ {
        err := s.lineControlAndSend(ctx, block)
        if err == nil {
            return nil // success
        }

        if errors.Is(err, ErrSendFailure) {
            return err // non-retryable
        }

        s.logger.Debug("block send retry",
            "retry", retryCount+1,
            "maxRetry", s.cfg.retryLimit,
            "error", err,
        )
    }

    return ErrSendFailure
}
```

---

## 6. TCP Stream Considerations

### 6.1 Buffered I/O

Use `bufio.Reader` for reads and `bufio.Writer` for writes to minimize syscalls:

```go
type connectionResources struct {
    conn   net.Conn
    reader *bufio.Reader
    writer *bufio.Writer
}
```

### 6.2 Single-Byte vs Chunk Reads

On serial, ENQ/EOT/ACK/NAK arrive as single bytes. On TCP, they may arrive bundled with block data in a single `Read()` call. The receiver must:

1. Read one byte at a time when expecting handshake characters (ENQ, EOT, ACK, NAK).
2. Use `bufio.Reader.ReadByte()` for this purpose.
3. Read block data in chunks after the length byte.

### 6.3 Read/Write Interleaving

Since the protocol is half-duplex and uses the same TCP stream for both directions, reads and writes are interleaved but never concurrent for block data. The handshake bytes (ENQ, EOT, ACK, NAK) serve as direction switches.

---

## 7. Goroutine Model

```
┌─────────────────────────────────────────────────┐
│                protocolLoop goroutine             │
│                                                   │
│  ┌──── IDLE ────┐                                │
│  │              │                                │
│  │  select {                                     │
│  │    case block := <-sendChan:                  │
│  │        lineControl(block)                     │
│  │        sendBlock(block)                       │
│  │                                               │
│  │    case enq := readByte():                    │
│  │        if enq == ENQ {                        │
│  │            writeByte(EOT)                     │
│  │            receiveBlock()                     │
│  │            passToAssembler(block)             │
│  │        }                                      │
│  │  }                                            │
│  └──────────────┘                                │
└─────────────────────────────────────────────────┘
```

**Single-goroutine protocol loop**: Because SECS-I is half-duplex, we can run the entire block-transfer protocol in a **single goroutine** that multiplexes between send requests (from a channel) and incoming ENQ bytes (from the TCP stream). This avoids complex locking.

The challenge is that `Read()` is blocking. Solution: use a **short read deadline** in Idle state to poll for both incoming bytes and outgoing send requests:

```go
func (c *Connection) protocolLoop(ctx context.Context) {
    for {
        select {
        case <-ctx.Done():
            return
        default:
        }

        // Check if we have a block to send
        select {
        case msg := <-c.senderMsgChan:
            // Application wants to send → enter Line Control
            blocks := c.splitIntoBlocks(msg)
            for _, block := range blocks {
                if err := c.sender.sendBlock(ctx, block); err != nil {
                    c.handleSendFailure(msg, err)
                    break
                }
            }
        default:
            // No send pending → listen for incoming ENQ
            c.conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
            b, err := c.reader.ReadByte()
            if err != nil {
                continue // timeout, loop back
            }
            if b == ENQ {
                c.writeByte(EOT)
                block, err := c.receiver.receiveBlock(ctx)
                if err == nil {
                    c.assembler.processBlock(block)
                }
            }
        }
    }
}
```

---

## 8. Error Sentinel Values

```go
var (
    ErrSendFailure     = errors.New("secs1: block send failure (retries exhausted)")
    ErrT1Timeout       = errors.New("secs1: T1 inter-character timeout")
    ErrT2Timeout       = errors.New("secs1: T2 protocol timeout")
    ErrT3Timeout       = errors.New("secs1: T3 reply timeout")
    ErrT4Timeout       = errors.New("secs1: T4 inter-block timeout")
    ErrInvalidLength   = errors.New("secs1: invalid block length (must be 10-254)")
    ErrChecksumMismatch = errors.New("secs1: checksum mismatch")
    ErrContention      = errors.New("secs1: contention detected")
    ErrDuplicateBlock  = errors.New("secs1: duplicate block detected")
    ErrConnClosed      = errors.New("secs1: connection closed")
    ErrDeviceIDMismatch = errors.New("secs1: device ID mismatch")
)
```

---

## 9. Metrics

```go
type ConnectionMetrics struct {
    BlockSendCount     atomic.Uint64
    BlockRecvCount     atomic.Uint64
    BlockRetryCount    atomic.Uint64
    BlockErrCount      atomic.Uint64
    ContentionCount    atomic.Uint64
    MsgSendCount       atomic.Uint64
    MsgRecvCount       atomic.Uint64
    MsgErrCount        atomic.Uint64
    ConnRetryGauge     atomic.Uint32
}
```
