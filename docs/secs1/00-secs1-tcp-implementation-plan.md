# SECS-I over TCP/IP — Implementation Plan

> **Scope**: Add a `secs1` package to `github.com/arloliu/go-secs` that implements the SEMI E4 (SECS-I) block-transfer and message protocols running on top of a TCP/IP stream, reusing the existing `hsms` shared abstractions where they fit and introducing new primitives where SECS-I diverges.

---

## 1. Background & Motivation

The existing `hsmsss` package implements **SEMI E37 HSMS-SS** (High-Speed SECS Message Services — Single Session). HSMS was designed as a TCP/IP replacement for the original RS-232 SECS-I serial link, but many legacy and brownfield equipment deployments still speak **SECS-I** — either over RS-232 or (increasingly) tunneled over a raw TCP socket.

This plan describes how to add first-class SECS-I support to `go-secs`, treating a TCP connection as the underlying byte stream in place of a serial port.

### Key differences between HSMS-SS and SECS-I

| Aspect | HSMS-SS (existing) | SECS-I (new) |
|---|---|---|
| **Framing** | 4-byte length prefix + payload | 1-byte length + 10-byte header + 0-244 body + 2-byte checksum |
| **Session establishment** | Select.req / Select.rsp handshake | No session handshake — communication starts with ENQ/EOT |
| **Direction control** | Full duplex over TCP | Half-duplex: ENQ → EOT → Block → ACK/NAK |
| **Contention** | N/A (full duplex) | Master/Slave contention resolution (equipment = master) |
| **Multi-block** | Single framed message (unlimited length) | Messages split into ≤244-byte body blocks, up to 32 767 blocks |
| **Keep-alive** | Linktest control message | No explicit keep-alive (rely on T2 retry) |
| **Control messages** | Select, Deselect, Linktest, Separate, Reject | ENQ (0x05), EOT (0x04), ACK (0x06), NAK (0x15) |
| **Timeouts** | T3, T5, T6, T7, T8 | T1 (inter-char), T2 (protocol), T3 (reply), T4 (inter-block) |

---

## 2. Design Goals

1. **Mirror `hsmsss` API surface** — users should be able to swap between HSMS-SS and SECS-I connections with minimal code changes. Both implement `hsms.Connection` and use `hsms.Session`.
2. **Reuse `hsms` shared types** — `BaseSession`, `DataMessage`, `ConnStateMgr`, `AtomicOpState`, `TaskManager`, `DataMessageHandler`, connection state enums.
3. **TCP as wire** — treat the TCP stream exactly like a serial byte stream; the ENQ/EOT/ACK/NAK handshake runs identically.
4. **Clean separation of concerns** — Block-transfer protocol (Layer 2) is isolated from message protocol (Layer 4) and from the TCP transport.
5. **Full E4 compliance** — implement all logic from Figure 2 (block-transfer state machine) and Figure 4 (message-receive algorithm).
6. **Testability** — use `net.Pipe()` for unit tests; mock transport for handshake/timeout tests.

---

## 3. Package Layout

```
secs1/
├── doc.go                    # Package documentation
├── block.go                  # Block struct, pack/unpack, checksum
├── handshake_byte.go         # ENQ, EOT, ACK, NAK constants
├── conn_config.go            # ConnectionConfig + ConnOption functions
├── conn.go                   # Connection struct (implements hsms.Connection)
├── conn_active.go            # Active mode: connectLoop, state handler
├── conn_passive.go           # Passive mode: listener, accept, state handler
├── block_transport.go        # blockTransport: sendBlock/receiveBlock + handshake
├── sender.go                 # Block-transfer Send state machine
├── receiver.go               # Block-transfer Receive state machine + completion
├── message_splitter.go       # SplitMessage / AssembleMessage for multi-block
├── message_assembler.go      # Message protocol: multi-block assembly, T4
├── session.go                # Session (wraps hsms.BaseSession)
├── metric.go                 # ConnectionMetrics (mirrors hsmsss/metric.go)
├── errors.go                 # Sentinel errors
├── block_test.go             # Unit tests: block pack/unpack, checksum
├── conn_config_test.go       # Unit tests: config validation, option application
├── sender_test.go            # Unit tests: send state machine, retries
├── receiver_test.go          # Unit tests: receive state machine, T1/T2
├── message_splitter_test.go  # Unit tests: split/assemble round-trip
├── message_assembler_test.go # Unit tests: multi-block, T4, duplicate detect
├── conn_test.go              # Integration tests via net.Pipe()
└── helpers_test.go           # Test helpers: pipe-based test harnesses
```

---

## 4. Reuse from Existing `hsms` Package

The following types and utilities from the `hsms` package will be **directly reused**:

| `hsms` type | Usage in `secs1` |
|---|---|
| `hsms.Connection` interface | `secs1.Connection` will implement this; add `IsSECS1()`, `IsHSMSSS()`, `IsHSMSGS()` methods to the interface |
| `hsms.Session` interface | `secs1.Session` will implement this |
| `hsms.BaseSession` | Embedded in `secs1.Session` for `SendDataMessage`, `ReplyDataMessage`, etc. |
| `hsms.DataMessage` | The application-level message type (Stream/Function/Item) |
| `hsms.DataMessageHandler` | Handler callback type |
| `hsms.ConnState` + enums | `NotConnectedState`, `ConnectingState`, `NotSelectedState`, `SelectedState` |
| `hsms.ConnStateMgr` | State machine manager with handler callbacks |
| `hsms.ConnStateChangeHandler` | User-facing state change callback |
| `hsms.AtomicOpState` | Operation state (Closed → Opening → Opened → Closing → Closed) |
| `hsms.TaskManager` | Goroutine lifecycle management |
| `hsms.GenerateMsgSystemBytes()` | System-byte generation |
| `hsms.HSMSMessage` | Interface for messages (data messages flow through this) |

### What is NOT reused

- HSMS control messages (`SelectReq`, `LinktestReq`, `SeparateReq`, etc.) — SECS-I has no equivalent.
- HSMS message framing/decoding (`hsms.DecodeMessage`, 4-byte length prefix).
- HSMS select/deselect/linktest handshake logic.

---

## 5. Core Structures

### 5.1 Block (`block.go`)

```go
// Block represents a single SECS-I block (§7, §8 of SEMI E4).
type Block struct {
    Header   [10]byte // 10-byte header (R-bit, DevID, W-bit, Stream, Function, E-bit, BlockNo, SystemBytes)
    Body     []byte   // 0–244 bytes of SECS-II data
}

// Convenience accessors
func (b *Block) RBit() bool          // Header[0] bit 7
func (b *Block) DeviceID() uint16    // Header[0..1] (15 bits, excluding R-bit)
func (b *Block) WBit() bool          // Header[2] bit 7
func (b *Block) StreamCode() byte    // Header[2] bits 0-6
func (b *Block) FunctionCode() byte  // Header[3]
func (b *Block) EBit() bool          // Header[4] bit 7 (1 = last block)
func (b *Block) BlockNumber() uint16 // Header[4..5] (15 bits, excluding E-bit)
func (b *Block) SystemBytes() []byte // Header[6..9]

// Length returns len(Header) + len(Body), i.e. the Length Byte value.
func (b *Block) Length() byte

// Pack serializes to wire: [LengthByte][Header][Body][Checksum_Hi][Checksum_Lo]
func (b *Block) Pack() []byte

// CalculateChecksum returns the 16-bit checksum over Header + Body.
func (b *Block) CalculateChecksum() uint16

// Validate checks length range (10-254) and checksum.
func (b *Block) Validate(expectedChecksum uint16) error

// ParseBlock deserializes a block from raw bytes (after length byte).
func ParseBlock(lengthByte byte, data []byte) (*Block, error)
```

### 5.2 Handshake Bytes (`handshake_byte.go`)

```go
const (
    ENQ byte = 0x05 // Request to Send
    EOT byte = 0x04 // Ready to Receive
    ACK byte = 0x06 // Correct Reception
    NAK byte = 0x15 // Incorrect Reception
)
```

### 5.3 ConnectionConfig (`conn_config.go`)

```go
type ConnectionConfig struct {
    host     string
    port     int
    deviceID uint16

    isActive bool   // true = Active (TCP client, typically Host)
    isEquip  bool   // true = Equipment role (Master per SEMI E4 Section 7.5)

    t1Timeout  time.Duration // Inter-character timeout (default: 0.5s)
    t2Timeout  time.Duration // Protocol timeout (default: 10s)
    t3Timeout  time.Duration // Reply timeout (default: 45s)
    t4Timeout  time.Duration // Inter-block timeout (default: 45s)
    retryLimit int           // RTY (default: 3)

    // TCP-specific
    connectRemoteTimeout time.Duration // Dial timeout for active mode
    acceptConnTimeout    time.Duration // Accept deadline for passive mode
    closeConnTimeout     time.Duration
    sendTimeout          time.Duration

    // Feature toggles
    duplicateDetection bool // Enable duplicate block detection (§9.4.2)

    // Queue sizes
    senderQueueSize  int
    dataMsgQueueSize int

    logger logger.Logger
}
```

The `ConnOption` pattern mirrors `hsmsss.ConnOption`:

```go
func WithActive() ConnOption
func WithPassive() ConnOption
func WithEquipRole() ConnOption    // Equipment role (Master per SEMI E4 Section 7.5)
func WithHostRole() ConnOption     // Host role (Slave per SEMI E4 Section 7.5, default)
func WithDeviceID(id uint16) ConnOption
func WithT1Timeout(d time.Duration) ConnOption
func WithT2Timeout(d time.Duration) ConnOption
func WithT3Timeout(d time.Duration) ConnOption
func WithT4Timeout(d time.Duration) ConnOption
func WithRetryLimit(n int) ConnOption
func WithDuplicateDetection(enabled bool) ConnOption
// ... etc
```

### 5.4 Connection (`conn.go`)

The `Connection` struct implements `hsms.Connection`:

```go
type Connection struct {
    pctx      context.Context
    ctx       context.Context
    ctxCancel context.CancelFunc
    cfg       *ConnectionConfig
    logger    logger.Logger

    // TCP state (passive mode).
    listener      net.Listener
    listenerMutex sync.Mutex

    opState   hsms.AtomicOpState
    connCount atomic.Int32  // passive mode only
    session   *Session

    // TCP resources.
    connMutex sync.RWMutex
    tcpConn   net.Conn

    // State management.
    stateMgr *hsms.ConnStateMgr
    taskMgr  *hsms.TaskManager
    shutdown atomic.Bool

    // Reconnect (active mode).
    connectLoopRunning atomic.Bool      // CAS guard: only one connect loop
    reconnectGen       atomic.Uint64    // incremented on Close() to invalidate stale loops
    loopCtx            context.Context  // cancelled on Close() to wake the connect loop
    loopCancel         context.CancelFunc

    // Message assembler for multi-block message assembly.
    // Created when the protocol loop starts, closed when it stops.
    assembler *messageAssembler

    // Application-level messaging.
    senderMsgChan chan *hsms.DataMessage
    replyMsgChans *xsync.MapOf[uint32, chan *hsms.DataMessage]
    replyErrs     *xsync.MapOf[uint32, error]

    metrics ConnectionMetrics
}

// Compile-time check: Connection implements hsms.Connection.
var _ hsms.Connection = (*Connection)(nil)
```

**Key design choices vs. `hsmsss`**:
- **No separate `connectionResources` wrapper** — raw `net.Conn` stored directly; `blockTransport` creates its own `bufio.Reader` per connection.
- **No `blockSender` / `blockReceiver` goroutines** — a single `protocolLoop` goroutine alternates between send and receive (half-duplex).
- **`connectLoopRunning` + `loopCtx`** — active mode reconnect uses a single `connectLoop` goroutine with local backoff delay; `loopCtx` is cancelled on `Close()` to wake the loop without waiting for the full backoff timer.

---

## 6. State Machine Design

### 6.1 Connection-Level States (reuse `hsms.ConnState`)

```
NotConnected ──► Connecting ──► NotSelected ──► Selected
       ▲                              │               │
       └──────────────────────────────┴───────────────┘
```

For SECS-I over TCP:
- **NotConnected → Connecting**: TCP dial (active) or TCP listen/accept (passive)
- **Connecting → NotSelected**: TCP connected; skip HSMS select handshake — go directly to ready
- **NotSelected → Selected**: Immediately (SECS-I has no select.req) — OR we can transition to `Selected` right after TCP connect is established, since there is no HSMS select concept.

> **Design Decision**: Since SECS-I has no session-selection handshake, we transition directly from `NotSelected` → `Selected` upon TCP connection establishment. The `NotSelected` state exists only transiently for API compatibility.

### 6.2 Block-Transfer State Machine (SEMI E4 Figure 2)

This is the **core new logic** not present in HSMS. It has five states:

```
                    ┌─────────┐
          ┌────────►│  IDLE   │◄────────┐
          │         └────┬────┘         │
          │              │              │
     ACK/NAK sent   ┌───┴───┐    Block sent or
     (completion)   │       │    retry exhausted
          │    ┌────▼──┐  ┌─▼────────┐  │
          │    │RECEIVE│  │LINE CTRL │  │
          │    └───┬───┘  └─┬────┬───┘  │
          │        │        │    │      │
          │   Block rcvd  EOT  ENQ     │
          │        │      rcvd  sent   │
          │   ┌────▼────┐  │    │      │
          └───┤COMPLETION│ ┌▼────▼──┐   │
              └─────────┘ │  SEND  │───┘
                          └────────┘
```

**States**:
1. **Idle** — waiting; exits on: (A) app wants to send → Line Control, (B) ENQ received → send EOT, enter Receive
2. **Line Control** — send ENQ, wait for EOT; handle contention, retries
3. **Send** — transmit Length + Header + Body + Checksum; wait for ACK
4. **Receive** — receive Length byte, then N + 2 bytes (block + checksum)
5. **Completion** — validate received block (checksum, length); send ACK or NAK; return to Idle

### 6.3 Message Protocol (SEMI E4 Figure 4 / §9)

Runs above the block-transfer layer:

- **Message Assembly**: collect blocks belonging to the same message (matched by System Bytes + Device ID + R-bit), verify block-number sequence.
- **Duplicate Block Detection** (§9.4.2): compare full 10-byte header of current block with previously accepted block header.
- **Transaction Matching**: T3 timer per open transaction; T4 timer per open multi-block receive.
- **Reply Linking**: primary (odd function) → open transaction; secondary (even function) → close transaction via System Bytes match.

---

## 7. Concurrency Architecture

Unlike HSMS-SS (which uses separate sender and receiver goroutines for full-duplex), SECS-I uses a **single protocol loop** goroutine that alternates between send and receive, consistent with the half-duplex nature of the protocol.

```
┌──────────────────────────────────────────────────┐
│                   Connection                      │
│                                                    │
│  ┌──────────────────────────────────────────┐  │
│  │         protocolLoop (single goroutine)         │  │
│  │                                                  │  │
│  │  1. Check senderMsgChan (non-blocking)           │  │
│  │     └── msg available:                           │  │
│  │        SplitMessage → for each block:            │  │
│  │          bt.sendBlock (ENQ→EOT→Block→ACK)       │  │
│  │                                                  │  │
│  │  2. Poll for incoming (50ms read timeout)        │  │
│  │     └── ENQ received:                            │  │
│  │        send EOT → bt.receiveBlock → ACK         │  │
│  │        → assembler.processBlock                  │  │
│  └──────────────────────────────────────────┘  │
│        │                   │                        │
│        ▼                   ▼                        │
│  ┌──────────────┐  ┌─────────────────┐       │
│  │ blockTransport │  │ msgAssembler      │       │
│  │ (sendBlock +   │  │ (multi-block      │       │
│  │  receiveBlock + │  │  assembly, T4,    │       │
│  │  contention)    │  │  dup detection)   │       │
│  └───────┬──────┘  └───────┬─────────┘       │
│          │                  │                      │
│          ▼                  ▼                      │
│    ┌─────────┐     ┌───────────────┐          │
│    │ TCP Conn │     │ DataMsg Handler│          │
│    └─────────┘     └───────────────┘          │
└──────────────────────────────────────────────────┘
```

**Half-Duplex Coordination**

Since SECS-I is half-duplex, the protocol loop naturally ensures only one direction is active at a time. The `blockTransport` type is **not goroutine-safe** — only the protocol loop goroutine accesses it. The ENQ/EOT handshake is the mechanism that switches direction. Contention resolution is handled within `sendBlock`:
- **Master** (Equipment): after sending ENQ, ignores everything except EOT.
- **Slave** (Host): after sending ENQ, if it receives ENQ → yields, sends EOT, receives Master's block first, then retries its own send.

**Additional goroutines**:
- **Active mode**: `connectLoop` goroutine (at most one, CAS-guarded) handles reconnection with exponential backoff.
- **Passive mode**: `acceptConnTask` goroutine blocks on `Accept()` with a deadline.

---

## 8. Detailed File Plans

### 8.1 `block.go` — Block Encoding/Decoding

- `Block` struct with Header [10]byte and Body []byte
- `Pack()` → `[length][header][body][checksum_hi][checksum_lo]`
- `ParseBlock(lengthByte, rawData)` → `*Block, error`
- `CalculateChecksum(header, body)` → `uint16`
- Accessor methods for all header fields (R-bit, Device ID, W-bit, Stream, Function, E-bit, Block Number, System Bytes)
- Builder functions for constructing blocks from a `DataMessage`

### 8.2 `block_transport.go` + `sender.go` + `receiver.go` — Block Transfer

The block-transfer protocol is implemented across three files:

- **`block_transport.go`** — `blockTransport` struct that wraps `net.Conn` + `bufio.Reader` and provides `sendBlock()` / `receiveBlock()` / `readByte()` / `writeByte()`. It also holds an `onRecvBlock` callback for contention-yield receives. Created per-connection by the protocol loop.
- **`sender.go`** — `sendBlock()` implementation: ENQ → wait EOT (T2) → send block bytes → wait ACK (T2) → retry on failure. Handles contention resolution (Master ignores ENQ; Slave yields, sends EOT, receives, retries).
- **`receiver.go`** — `receiveBlock()` implementation: read length byte (T2) → read header+body (T1 inter-char) → validate checksum → send ACK/NAK.

```go
type blockTransport struct {
    conn        net.Conn
    reader      *bufio.Reader
    cfg         *ConnectionConfig
    logger      logger.Logger
    onRecvBlock func(*Block)  // contention-yield callback
}

// sendBlock sends a single block with full ENQ/EOT/ACK handshake and retry.
func (bt *blockTransport) sendBlock(ctx context.Context, block *Block) error

// receiveBlock receives a single block after EOT has been sent.
func (bt *blockTransport) receiveBlock(ctx context.Context) (*Block, error)
```

**Retry logic** (unchanged from spec):
1. Send ENQ
2. Wait for response within T2
   - EOT received → proceed to Send state
   - ENQ received (contention) →
     - If Slave: yield, send EOT, receive Master's block, retry from step 1
     - If Master: ignore, keep waiting for EOT
   - T2 expires → increment retry, go to step 1
3. Send block data (length + header + body + checksum)
4. Wait for response within T2
   - ACK → success
   - NAK or non-ACK → increment retry, go to step 1
   - T2 expires → increment retry, go to step 1
5. If retries > RTY → return send failure

**TCP adaptation for T1**: Since TCP delivers bytes in chunks, T1 is implemented as the read deadline between `Read()` calls via `conn.SetReadDeadline()`.

### 8.3 `message_splitter.go` — Message Split/Assemble

Provides `SplitMessage()` (DataMessage → blocks) and `AssembleMessage()` (blocks → DataMessage):

```go
// SplitMessage splits a DataMessage into SECS-I blocks (≤244-byte body each).
func SplitMessage(msg *hsms.DataMessage, deviceID uint16, isEquip bool) []*Block

// AssembleMessage reassembles a complete message from ordered blocks.
func AssembleMessage(blocks []*Block) (*hsms.DataMessage, error)
```

### 8.4 `message_assembler.go` — Message Protocol

```go
type messageAssembler struct {
    cfg    *ConnectionConfig
    logger logger.Logger
    onMsg  messageHandler  // callback for complete messages

    mu              sync.Mutex
    openMessages    map[uint64]*openMessage // key: compositeKey
    lastAcceptedHdr [10]byte               // for duplicate detection
    hasPrevHeader   bool
}

type openMessage struct {
    blocks      []*Block
    nextBlockNo uint16
    key         uint64
    t4Timer     *time.Timer
    t4Cancel    chan struct{} // closed to signal the T4 goroutine to exit
}
```

**Logic** (implements Figure 4):

1. Receive validated block from `blockReceiver`
2. **Duplicate Detection** (§9.4.2): compare 10-byte header with `lastAcceptedHdr`; if identical → discard
3. Check if block matches any expected block (by System Bytes, Device ID, R-bit, Block Number)
   - **Yes, expected block**:
     - First block of reply → cancel T3 timer
     - E-bit = 1 → message complete, assemble and deliver to application
     - E-bit = 0 → reset T4 timer, update expected next block number
   - **No, not expected**:
     - Must be first block of a new primary message (odd function, block number 0 or 1)
     - Otherwise → discard (send error, or S9Fx)
     - E-bit = 1 → single-block message, deliver immediately
     - E-bit = 0 → start T4 timer, register expected next block
4. When T4 expires → discard partial message, abort transaction
5. When T3 expires → remove expected reply block, abort transaction

### 8.5 `session.go` — Session

```go
type Session struct {
    hsms.BaseSession
    id       uint16
    conn     *Connection
    cfg      *ConnectionConfig
    logger   logger.Logger

    mu              sync.RWMutex
    dataMsgChans    []chan *hsms.DataMessage
    dataMsgHandlers []hsms.DataMessageHandler
}
```

Nearly identical to `hsmsss.Session`. Delegates `SendMessage` to `Connection.sendMsg` which splits into blocks via the sender.

### 8.6 `conn_active.go` / `conn_passive.go`

**Active mode** (typically Host/Slave):
- TCP client, dials to remote
- Reconnect via `connectLoop` goroutine with exponential backoff (100ms → 30s)
- CAS-guarded (`connectLoopRunning`) — at most one loop runs
- `loopCtx` cancelled on `Close()` to wake immediately
- Is **Slave** in contention resolution by default

**Passive mode** (typically Equipment/Master):
- TCP server, listens and accepts via `acceptConnTask`
- Listener created once via `ensureListener()`, reused across reconnects
- Listener closed only on shutdown
- Only accepts one connection at a time (`connCount` check)
- Is **Master** in contention resolution by default

State transitions on connect:
```
ConnectingState → (TCP connected) → NotSelectedState → SelectedState
```

The `NotSelected → Selected` transition happens immediately since SECS-I has no select handshake. This keeps the `ConnStateMgr` handlers compatible with `hsmsss`.

---

## 9. Timeout Implementation

| Timer | Where | Mechanism |
|---|---|---|
| **T1** (inter-char, 0.5s) | `receiver.go` via `blockTransport` | `conn.SetReadDeadline(now + T1)` between byte reads during block reception |
| **T2** (protocol, 10s) | `sender.go`, `receiver.go` via `blockTransport` | `time.Timer` — after ENQ wait for EOT; after EOT wait for length byte; after checksum wait for ACK |
| **T3** (reply, 45s) | `conn.go` (`sendMsg`) | `time.Timer` per open transaction — started when W-bit message is queued, cancelled on reply receipt |
| **T4** (inter-block, 45s) | `message_assembler.go` | `time.Timer` per open multi-block receive — reset on each block, cancelled via `t4Cancel` channel |

---

## 10. API Compatibility with `hsmsss`

### User code comparison:

**HSMS-SS (current)**:
```go
cfg, _ := hsmsss.NewConnectionConfig("127.0.0.1", 5000,
    hsmsss.WithActive(),
    hsmsss.WithHostRole(),
    hsmsss.WithT3Timeout(30*time.Second),
)
conn, _ := hsmsss.NewConnection(ctx, cfg)
session := conn.AddSession(1000)
session.AddDataMessageHandler(handler)
conn.Open(true)
defer conn.Close()
```

**SECS-I (new, equivalent)**:
```go
cfg, _ := secs1.NewConnectionConfig("127.0.0.1", 5000,
    secs1.WithActive(),
    secs1.WithHostRole(),           // Host = Slave per SEMI E4 Section 7.5
    secs1.WithDeviceID(1000),
    secs1.WithT3Timeout(30*time.Second),
)
conn, _ := secs1.NewConnection(ctx, cfg)
session := conn.AddSession(1000)   // same interface
session.AddDataMessageHandler(handler)  // same handler type
conn.Open(true)
defer conn.Close()
```

---

## 11. Implementation Phases

### Phase 1: Core Block Layer
- [x] `block.go` — Block struct, pack/unpack, checksum, header accessors
- [x] `handshake_byte.go` — ENQ, EOT, ACK, NAK constants
- [x] `errors.go` — Sentinel errors
- [x] `block_test.go` — Full unit test coverage
- [x] `conn_config.go` — Configuration with all SECS-I parameters

### Phase 2: Block-Transfer State Machine
- [x] `sender.go` — Send + Line Control state machine
- [x] `receiver.go` — Receive + Completion state machine
- [x] `block_transport.go` — Unified block transport (replaces halfDuplexCtl)
- [x] Contention resolution (Master/Slave)
- [x] `sender_test.go`, `receiver_test.go` — with `net.Pipe()` mock

### Phase 3: Message Protocol
- [x] `message_assembler.go` — Multi-block assembly, T4, duplicate detection
- [x] `message_splitter.go` — SplitMessage / AssembleMessage
- [x] Transaction matching (System Bytes)
- [x] `message_assembler_test.go`, `message_splitter_test.go`

### Phase 4: Connection & Session
- [x] `conn.go` — Connection struct implementing `hsms.Connection`
- [x] `conn_active.go` — Active mode (connectLoop, reconnect)
- [x] `conn_passive.go` — Passive mode (listener, accept)
- [x] `session.go` — Session implementing `hsms.Session`
- [x] `metric.go` — Connection metrics
- [x] `doc.go` — Package documentation
- [x] `conn_test.go` — Full integration tests (128 tests + subtests, race-clean)

### Phase 5: Integration & Examples
- [x] `conn_test.go` — Full integration tests
- [x] `examples/secs1_device/` — Example active host ↔ passive equipment
- [x] `tests/secs1_batch_conn_test.sh` — Batch test script
- [x] Update `README.md` with SECS-I documentation

---

## 12. Open Design Questions

1. **`NotSelected` state duration**: Should we expose a transient `NotSelected` state (for API symmetry) or go straight to `Selected`? Recommendation: transient, auto-promote to `Selected` immediately after TCP connect — zero user action needed.

2. **Master/Slave vs Active/Passive**: **Resolved.** Per SEMI E4 Section 7.5 and Section 10.1, Master/Slave is not independently configurable: Equipment is always Master, Host is always Slave. We use `WithEquipRole()` / `WithHostRole()` (like `hsmsss`), and `IsMaster()` / `IsSlave()` are derived read-only getters. Active/Passive (TCP client/server) remains an independent axis.

3. **T1 on TCP**: Since TCP delivers bytes in chunks, `T1` may fire differently than on serial. Should we make T1 configurable as "read deadline per chunk" vs "per byte"? Recommendation: per-read-call deadline; document the behavior.

4. **Message interleaving**: SEMI E4 §9.2.4 allows but does not require interleaving multi-block messages. Should we support it in v1? Recommendation: no interleaving in v1; send multi-block messages atomically.

5. **S9Fx error messages**: Should the SECS-I layer auto-generate S9F1 (Unknown Device ID), S9F3 (Unrecognized Stream), etc. like `hsmsss` does? Recommendation: yes, reuse the `gem` package for S9Fx generation.

6. **`hsms.Connection` interface additions**: Adding `IsSECS1()`, `IsHSMSSS()`, `IsHSMSGS()` to the interface is technically a Go interface breaking change, but all implementors are internal to this repo and users only consume the interface. Decision: **accepted** — see `06-refactoring-and-reuse-notes.md` §3 for full analysis.
