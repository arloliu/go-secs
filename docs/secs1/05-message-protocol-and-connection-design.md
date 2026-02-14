# SECS-I Message Protocol & Connection Design

> This document details the message-level protocol (SEMI E4 §9), the connection lifecycle, and how the `secs1` package integrates with the existing `hsms` shared abstractions.

---

## 1. Message Protocol Layer

The message protocol sits above the block-transfer protocol. It is responsible for:

1. **Message assembly** — collecting multi-block messages
2. **Transaction management** — pairing primary/secondary messages via System Bytes
3. **Duplicate block detection** — discarding retransmitted blocks
4. **Timeout enforcement** — T3 (reply) and T4 (inter-block)
5. **Error reporting** — S9Fx messages for protocol violations

### 1.1 Message Assembly (`message_assembler.go`)

```go
type messageAssembler struct {
    cfg    *ConnectionConfig
    logger logger.Logger
    onMsg  messageHandler  // callback for complete messages

    mu              sync.Mutex
    openMessages    map[uint64]*openMessage // key: compositeKey
    lastAcceptedHdr [10]byte               // for duplicate detection (§9.4.2)
    hasPrevHeader   bool
}

type openMessage struct {
    blocks      []*Block
    nextBlockNo uint16
    key         uint64
    t4Timer     *time.Timer
    t4Cancel    chan struct{} // closed to signal the T4 goroutine to exit
}

// compositeKey creates a unique key from system bytes + device ID + R-bit
// for indexing open messages.
func compositeKey(systemBytes uint32, deviceID uint16, rBit bool) uint64
```

The assembler receives a `messageHandler` callback (set to `Connection.handleCompleteMessage`) instead of referencing the session directly. Complete messages are delivered as `[]*Block` slices, which the connection converts to `*hsms.DataMessage`.

### 1.2 Block Processing Algorithm (Figure 4)

When a validated block arrives from the block-transfer layer:

```
processBlock(block):
  1. Route Check (§9.4.1):
     - If block.DeviceID != cfg.deviceID AND we have no knowledge of this ID
       → discard, optionally report routing error

  2. Duplicate Detection (§9.4.2):
     - Compare block.Header with lastAcceptedHdr (full 10-byte comparison)
     - If identical → discard (duplicate due to ACK loss + sender retry)
     - If different → update lastAcceptedHdr, continue

  3. Expected Block Match:
     - Search openMessages for a match:
       key = compositeKey(block.SystemBytes, block.DeviceID, block.RBit)

     3a. NOT in openMessages:
         - Must be first block of a new primary message
         - Validate: function is odd (primary), block number is 0 or 1
         - If invalid → discard, report error (S9F7 or S9F9)

         - If E-bit = 1 (single-block message):
           → Assemble complete message, deliver to application
           → If W-bit = 1, open transaction (start T3 timer for reply)

         - If E-bit = 0 (start of multi-block):
           → Create openMessage entry
           → Set nextBlockNo = blockNumber + 1
           → Start T4 timer
           → Buffer the block

     3b. IN openMessages (expected block):
         - Validate block number == entry.nextBlockNo
           If mismatch → discard entire open message, report error

         - Cancel/reset T4 timer

         - If this is first block of a reply (secondary message):
           → Cancel T3 timer for this transaction

         - If E-bit = 1 (last block):
           → Assemble complete message from all buffered blocks
           → Remove from openMessages
           → Deliver to application

         - If E-bit = 0 (more blocks):
           → Buffer block
           → Set nextBlockNo++
           → Restart T4 timer

  4. T4 Expiry Handler:
     - Remove open message
     - Abort transaction
     - Report error

  5. T3 Expiry Handler:
     - Remove expected reply entry
     - Abort transaction
     - Report timeout to application (return ErrT3Timeout to sender)
```

### 1.3 Message Delivery

Complete messages are delivered as `[]*Block` to the connection's `handleCompleteMessage` callback, which reassembles them into `*hsms.DataMessage`:

```go
func (c *Connection) handleCompleteMessage(blocks []*Block) {
    msg, err := AssembleMessage(blocks)
    if err != nil {
        c.logger.Error("secs1: failed to assemble message", "error", err)
        c.metrics.incDataMsgErrCount()
        return
    }

    c.metrics.incDataMsgRecvCount()

    // Dispatch based on function code parity.
    if msg.FunctionCode()%2 != 0 {
        // Primary message — deliver to application handlers.
        if c.session != nil {
            c.session.recvDataMsg(msg)
        }
    } else {
        // Secondary (reply) message — match to waiting sender.
        c.replyToSender(msg)
    }
}
```

### 1.4 Transaction Management

T3 timeout is managed in `conn.go`'s `sendMsg()` method rather than in the assembler. When sending a primary message (W-bit = 1):
1. Register reply channel and start T3 timer before queuing the message
2. Protocol loop picks up the message, splits into blocks, sends via `blockTransport`
3. On reply receipt → cancel T3, deliver reply to waiting sender via reply channel
4. On T3 expiry → return `ErrT3Timeout` to sender, clean up reply channel

---

## 2. Connection Lifecycle

### 2.1 State Transitions

```
    ┌───────────┐
    │  CLOSED   │  ← initial state (opState)
    └─────┬─────┘
          │ Open()
    ┌─────▼─────┐
    │  OPENING  │
    └─────┬─────┘
          │ TCP connected
    ┌─────▼─────┐
    │  OPENED   │  (opState)
    └─────┬─────┘
          │
    ┌─────▼──────────┐
    │ NOT_CONNECTED   │  ← ConnState
    └─────┬──────────┘
          │ stateMgr.ToConnecting()
    ┌─────▼──────────┐
    │ CONNECTING      │  ← TCP dial / TCP accept
    └─────┬──────────┘
          │ TCP established
    ┌─────▼──────────┐
    │ NOT_SELECTED    │  ← transient (no HSMS select in SECS-I)
    └─────┬──────────┘
          │ auto-promote
    ┌─────▼──────────┐
    │ SELECTED        │  ← ready for communication
    └────────────────┘
```

### 2.2 Active Mode Connection (Host / TCP Client)

The active mode uses a dedicated `connectLoop` goroutine for reconnection. Only one loop runs at a time, guarded by a `connectLoopRunning` CAS flag. The loop uses a **local** delay variable for exponential backoff (no shared atomic needed). On `Close()`, the `loopCtx` is cancelled to wake the loop immediately.

```go
func (c *Connection) activeConnStateHandler(_ hsms.Connection, _ hsms.ConnState, curState hsms.ConnState) {
    switch curState {
    case hsms.NotSelectedState:
        // TCP connected — start data message tasks + protocol loop,
        // then auto-promote to Selected (SECS-I has no select handshake).
        c.session.startDataMsgTasks()
        c.startProtocolLoop()
        c.stateMgr.ToSelectedAsync()

    case hsms.SelectedState:
        // Ready for communication (no-op).

    case hsms.NotConnectedState:
        // Close TCP, restart the connect loop for auto-reconnect.
        c.closeConn(c.cfg.closeTimeout)
        if !c.shutdown.Load() {
            c.startConnectLoop()
        }

    case hsms.ConnectingState:
        // The connect loop handles reconnection; nothing to do here.
    }
}
```

**Connect loop design** (`connectLoop`):

```go
func (c *Connection) connectLoop(loopCtx context.Context, gen uint64) {
    defer c.connectLoopRunning.Store(false)
    delay := initialRetryDelay  // local variable, no sharing

    for {
        timer := time.NewTimer(delay)
        select {
        case <-loopCtx.Done(): timer.Stop(); return
        case <-timer.C:
        }

        // Guard: generation changed or shutdown
        if c.reconnectGen.Load() != gen || c.shutdown.Load() { return }

        // Transition Closed → Opening, create fresh context, try dial
        if err := c.tryConnect(c.ctx); err != nil {
            c.opState.Set(hsms.ClosedState)
            delay *= retryDelayFactor
            if delay > maxRetryDelay { delay = maxRetryDelay }
            continue
        }

        c.metrics.resetConnRetryGauge()
        return  // connected; future disconnects restart a fresh loop
    }
}
```

Constants: `initialRetryDelay = 100ms`, `retryDelayFactor = 2`, `maxRetryDelay = 30s`.

### 2.3 Passive Mode Connection (Equipment / TCP Server)

The passive mode listens on a TCP port and accepts one connection at a time. The listener is created once and reused across reconnects; it is only closed on shutdown. The `acceptConnTask` blocks on `Accept()` with a deadline, returning `false` (stop) when a connection is accepted.

```go
func (c *Connection) passiveConnStateHandler(_ hsms.Connection, _ hsms.ConnState, curState hsms.ConnState) {
    switch curState {
    case hsms.ConnectingState:
        // Open listener + start accept task
        c.doOpen(false)

    case hsms.NotSelectedState:
        // TCP accepted — start data message tasks + protocol loop,
        // then auto-promote to Selected.
        c.session.startDataMsgTasks()
        c.startProtocolLoop()
        c.stateMgr.ToSelectedAsync()

    case hsms.SelectedState:
        // Ready for communication.

    case hsms.NotConnectedState:
        isShutdown := c.shutdown.Load()
        if isShutdown {
            c.closeListener()
        }
        c.closeConn(c.cfg.closeTimeout)
        if !isShutdown {
            c.stateMgr.ToConnectingAsync()
        }
    }
}
```

Key functions:
- **`ensureListener()`** — creates TCP listener under lock if nil.
- **`acceptConnTask()`** — task func that blocks on `Accept()` with deadline; rejects duplicate connections; stops after accepting one.
- **`handleAcceptError()`** — classifies accept errors (timeout → retry, shutdown → stop, closed listener → suppress log).

### 2.4 Protocol Loop Lifecycle

The protocol loop goroutine implements the half-duplex SECS-I protocol. It creates a `blockTransport` for the current TCP connection, then alternates between sending outgoing messages and polling for incoming ENQ bytes.

```go
func (c *Connection) startProtocolLoop() error {
    conn := c.getTCPConn()
    bt := newBlockTransport(conn, c.cfg, c.logger, c.handleReceivedBlock)
    c.assembler = newMessageAssembler(c.cfg, c.handleCompleteMessage)

    return c.taskMgr.Start("protocolLoop", func() bool {
        return c.protocolLoopIteration(bt)
    })
}

func (c *Connection) protocolLoopIteration(bt *blockTransport) bool {
    // Priority: check for outgoing messages first.
    select {
    case <-c.ctx.Done():
        c.assembler.close()
        return false

    case msg := <-c.senderMsgChan:
        c.handleOutgoingMessage(bt, msg)
        return true

    default:
        // No outgoing message — poll for incoming ENQ.
    }

    return c.pollForIncoming(bt)
}
```

The `blockTransport` is **not goroutine-safe** — only the protocol loop goroutine uses it, consistent with the half-duplex nature of SECS-I.

---

## 3. Message Sending Flow

### 3.1 Application → Wire

```
Application
    │
    ▼
session.SendDataMessage(stream, func, wBit, item)
    │
    ▼
conn.sendMsg(msg)
    │  if W-bit: register reply channel, start T3 timer
    │  put msg on senderMsgChan
    │  wait for reply or T3 timeout
    ▼
protocolLoop picks up from senderMsgChan
    │
    ▼
conn.handleOutgoingMessage(bt, msg)
    │  SplitMessage(msg, deviceID, isEquip)
    │    Split body into ≤244-byte chunks
    │    Set headers: R-bit, DeviceID, W-bit, Stream, Function
    │    Set Block Number (1, 2, 3, ...)
    │    Set E-bit on last block
    │    Set System Bytes (same for all blocks)
    ▼
for each block:
    bt.sendBlock(block)
        │ ENQ → wait EOT → send block → wait ACK
        │ (with retry and contention handling)
        ▼
    On error → replyErrToSender(), abort remaining blocks
```

### 3.2 Message Splitting (`message_splitter.go`)

```go
const MaxBlockBodySize = 244

// SplitMessage splits a DataMessage into SECS-I blocks.
// The R-bit is set automatically based on the isEquip flag.
func SplitMessage(msg *hsms.DataMessage, deviceID uint16, isEquip bool) []*Block

// AssembleMessage reassembles a complete message from ordered blocks.
func AssembleMessage(blocks []*Block) (*hsms.DataMessage, error)
```

---

## 4. Message Receiving Flow

### 4.1 Wire → Application

```
TCP stream
    │
    ▼
protocolLoop: pollForIncoming(bt)
    │  readByte(pollTimeout) detects ENQ
    │  send EOT
    ▼
bt.receiveBlock(ctx)
    │  read length + header + body + checksum
    │  validate checksum
    │  send ACK
    ▼
conn.handleReceivedBlock(block)
    │  incr metrics
    ▼
assembler.processBlock(block)
    │  duplicate detection
    │  expected block matching
    │  multi-block assembly
    │  T4 timer management
    ▼
When message complete:
    conn.handleCompleteMessage(blocks)
        │  AssembleMessage(blocks) → *hsms.DataMessage
        ▼
    if primary (odd function):
        session.recvDataMsg(msg)
            │  broadcast to dataMsgHandlers
            ▼
        DataMessageHandler(msg, session)
    if secondary (even function):
        conn.replyToSender(msg)
            │  match to reply channel by System Bytes
```

---

## 5. Comparison: hsmsss vs secs1 Connection

| Component | `hsmsss.Connection` | `secs1.Connection` |
|---|---|---|
| **TCP framing** | 4-byte length prefix | ENQ/EOT handshake + length byte |
| **receiverTask** | Read 4-byte len → read payload → decode | protocolLoop: detect ENQ → EOT → receive block |
| **senderTask** | Write 4+N bytes to TCP directly | ENQ → wait EOT → write block → wait ACK |
| **sendMsg** | Put on senderMsgChan, wait reply chan | Same pattern, but block-level send underneath |
| **State: NotSelected** | Wait for select.req/select.rsp | Auto-promote to Selected (no handshake) |
| **Linktest** | Periodic linktest.req/rsp | None (no SECS-I equivalent) |
| **Contention** | N/A (full duplex) | Master/Slave ENQ contention resolution |
| **Multi-block** | N/A (single message frame) | Block splitting + assembly + T4 timer |
| **Session** | `hsmsss.Session` wrapping `BaseSession` | `secs1.Session` wrapping `BaseSession` |
| **ConnStateMgr** | Shared (4 states) | Shared (4 states, NotSelected is transient) |
| **TaskManager** | Shared | Shared |

---

## 6. Duplicate Block Detection (§9.4.2)

A duplicate block occurs when:
1. Receiver sends ACK
2. ACK is lost or delayed beyond sender's T2
3. Sender retries the same block
4. Receiver gets the same block again

**Detection**: Compare the **full 10-byte header** of the newly received block with the header of the last accepted (non-duplicate) block. If identical → duplicate → discard.

```go
func (a *messageAssembler) isDuplicate(block *Block) bool {
    if !a.hasPrevHeader {
        return false
    }
    return block.Header == a.lastAcceptedHdr
}

func (a *messageAssembler) updateLastHeader(block *Block) {
    a.lastAcceptedHdr = block.Header
    a.hasPrevHeader = true
}
```

**Configuration**: Duplicate detection can be disabled via `WithDuplicateDetection(false)` for compatibility with older equipment per §9.4.2 NOTE 6.

---

## 7. R-Bit and Device ID Handling

### 7.1 R-Bit Direction

| R-Bit | Direction | Device ID means |
|---|---|---|
| 0 | Host → Equipment | Destination |
| 1 | Equipment → Host | Source |

### 7.2 In Practice

- **Host sending**: sets R-bit = 0, Device ID = target equipment ID
- **Equipment sending**: sets R-bit = 1, Device ID = own equipment ID
- **Host receiving**: expects R-bit = 1
- **Equipment receiving**: expects R-bit = 0, Device ID = own ID

The `secs1.Connection` automatically sets the R-bit based on role:
```go
func (c *Connection) buildBlockHeader(msg *hsms.DataMessage, blockNum uint16, isLast bool) [10]byte {
    var header [10]byte

    // R-bit and Device ID (bytes 0-1)
    if c.cfg.IsEquip() {
        // Equipment (Master) sending TO host: R-bit = 1
        header[0] = byte(c.cfg.deviceID>>8) | 0x80  // R-bit = 1
    } else {
        // Host (Slave) sending TO equipment: R-bit = 0
        header[0] = byte(c.cfg.deviceID >> 8) // R-bit = 0
    }
    header[1] = byte(c.cfg.deviceID)

    // W-bit + Stream (byte 2)
    // Function (byte 3)
    // E-bit + Block Number (bytes 4-5)
    // System Bytes (bytes 6-9)
    // ... (standard header construction)

    return header
}
```

---

## 8. Error Message Generation (S9Fx)

Reuse the `gem` package for S9Fx error messages:

| Condition | S9Fx | Description |
|---|---|---|
| Unknown Device ID | S9F1 | Block has unrecognized device ID |
| Unexpected block (not first of primary) | S9F3 | Unrecognized stream type |
| Block number mismatch in multi-block | S9F7 | Illegal data |
| T3 timeout (no reply) | S9F9 | Transaction timeout |

---

## 9. Testing Strategy

### 9.1 Unit Tests (no network)

| Test file | Coverage |
|---|---|
| `block_test.go` | Pack/unpack, checksum, header accessors, edge cases |
| `message_assembler_test.go` | Single-block, multi-block, duplicate detection, T3/T4 |

### 9.2 Protocol Tests (`net.Pipe()`)

| Test file | Coverage |
|---|---|
| `sender_test.go` | ENQ→EOT→Block→ACK flow, T2 timeout, retries, contention |
| `receiver_test.go` | ENQ detection, block receive, checksum failure, T1/T2 timeout |
| `handshake_test.go` | Full handshake sequences, contention scenarios |

### 9.3 Integration Tests

| Test file | Coverage |
|---|---|
| `conn_test.go` | Active↔Passive full flow, reconnect, close/reopen |
| `conn_config_test.go` | Config validation, option application |

### 9.4 Interoperability

- Test SECS-I Host (active, slave) connecting to SECS-I Equipment (passive, master)
- Send/receive S1F1/S1F2 (Are You There)
- Multi-block message transfer
- Contention scenario with both sides trying to send simultaneously
- Connection drop and reconnect

---

## 10. Configuration Defaults & Ranges (per SEMI E4 Table 4)

| Parameter | Default | Range | Resolution |
|---|---|---|---|
| T1 | 500ms | 100ms – 10s | 100ms |
| T2 | 10s | 200ms – 25s | 200ms |
| T3 | 45s | 1s – 120s | 1s |
| T4 | 45s | 1s – 120s | 1s |
| RTY | 3 | 0 – 31 | 1 |
| Device ID | 0 | 0 – 32767 | 1 |
| Master/Slave | (per role) | Master / Slave | — |
