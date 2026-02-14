# SECS-I vs HSMS-SS Refactoring Notes

> This document identifies what needs to be refactored or extracted from the existing `hsms` / `hsmsss` packages to support the `secs1` package cleanly.

---

## 1. Minimal Changes to Existing Packages

The SECS-I implementation is **mostly additive**. The `secs1` package imports and reuses `hsms` types as-is. The one planned change to the existing `hsms` package is adding connection-type query methods to the `hsms.Connection` interface (see §3).

---

## 2. Potential Shared Abstractions to Extract

While no refactoring is strictly required, the following patterns appear in both `hsmsss` and `secs1` and could optionally be extracted into `hsms` or a shared `internal` package in a future refactoring pass:

### 2.1 Reply Channel Management

Both `hsmsss.Connection` and `secs1.Connection` use the same pattern:
```go
replyMsgChans *xsync.MapOf[uint32, chan *hsms.DataMessage]
replyErrs     *xsync.MapOf[uint32, error]
```

With the same `addReplyExpectedMsg()`, `removeReplyExpectedMsg()`, `dropAllReplyMsgs()`, `replyToSender()`, `replyErrToSender()` methods.

**Status**: Pattern duplicated in `secs1` as planned. Future extraction into a shared `replyManager` remains optional.

### 2.2 Connection Resource Management

`hsmsss` wraps `net.Conn` + buffered I/O in a `connectionResources` struct. `secs1` takes a simpler approach: raw `net.Conn` stored directly in the `Connection` struct, with `blockTransport` creating its own `bufio.Reader` per protocol loop lifecycle.

**Status**: Divergent by design — SECS-I's half-duplex protocol loop creates transport resources per connection, so a shared abstraction is not beneficial.

### 2.3 Protocol Loop Pattern

`hsmsss` uses separate sender and receiver goroutines (full-duplex). `secs1` uses a single `protocolLoop` goroutine that alternates between send and receive (half-duplex). Both use `TaskManager` for goroutine lifecycle management.

**Status**: Fundamental architectural difference — no shared abstraction possible.

---

## 3. `hsms.Connection` Interface Changes

The current `hsms.Connection` interface:

```go
type Connection interface {
    Open(waitOpened bool) error
    Close() error
    AddSession(sessionID uint16) Session
    GetLogger() logger.Logger
    IsSingleSession() bool
    IsGeneralSession() bool
}
```

### 3.1 Add Connection-Type Query Methods

We will add three new methods to `hsms.Connection`:

```go
type Connection interface {
    // ... existing methods ...

    // IsSECS1 returns true if the connection is a SECS-I connection.
    IsSECS1() bool

    // IsHSMSSS returns true if the connection is an HSMS-SS (Single Session) connection.
    IsHSMSSS() bool

    // IsHSMSGS returns true if the connection is an HSMS-GS (General Session) connection.
    IsHSMSGS() bool
}
```

### 3.2 Breaking-Change Analysis

Adding methods to a Go interface **is technically a breaking change** — any external type that implements `hsms.Connection` would fail to compile. However:

1. **All known implementors are inside this repo**: `hsmsss.Connection` and test mocks (`ssConn`, `gsConn` in `hsms/conn_state_test.go`).
2. **Users consume, not implement**: Library users receive a `Connection` from `hsmsss.NewConnection()` or `secs1.NewConnection()`; they don't create their own implementations.
3. **Precedent exists**: The `IsSingleSession()` / `IsGeneralSession()` methods already establish this pattern.
4. **Semantic versioning**: If the library is pre-1.0 or bumps a minor/major version, this is acceptable.

**Decision**: Add the three methods. The practical impact on downstream users is **zero** since they don't implement the interface.

### 3.3 Required Code Changes

#### `hsms/conn.go` — Add to interface
```go
// IsSECS1 returns true if the connection is a SECS-I connection, false otherwise.
IsSECS1() bool

// IsHSMSSS returns true if the connection is an HSMS-SS (Single Session) connection, false otherwise.
IsHSMSSS() bool

// IsHSMSGS returns true if the connection is an HSMS-GS (General Session) connection, false otherwise.
IsHSMSGS() bool
```

#### `hsmsss/conn.go` — Add implementations
```go
func (c *Connection) IsSECS1() bool  { return false }
func (c *Connection) IsHSMSSS() bool { return true }
func (c *Connection) IsHSMSGS() bool { return false }
```

#### `hsms/conn_state_test.go` — Update test mocks
```go
// ssConn mock
func (_ *ssConn) IsSECS1() bool  { return false }
func (_ *ssConn) IsHSMSSS() bool { return true }
func (_ *ssConn) IsHSMSGS() bool { return false }

// gsConn mock
func (_ *gsConn) IsSECS1() bool  { return false }
func (_ *gsConn) IsHSMSSS() bool { return false }
func (_ *gsConn) IsHSMSGS() bool { return true }
```

#### `secs1/conn.go` — New implementations
```go
func (c *Connection) IsSECS1() bool       { return true }
func (c *Connection) IsHSMSSS() bool      { return false }
func (c *Connection) IsHSMSGS() bool      { return false }
func (c *Connection) IsSingleSession() bool { return true }
func (c *Connection) IsGeneralSession() bool { return false }
```

### 3.4 Relationship Between Old and New Methods

| Connection type | `IsSingleSession` | `IsGeneralSession` | `IsSECS1` | `IsHSMSSS` | `IsHSMSGS` |
|---|---|---|---|---|---|
| SECS-I | `true` | `false` | `true` | `false` | `false` |
| HSMS-SS | `true` | `false` | `false` | `true` | `false` |
| HSMS-GS | `false` | `true` | `false` | `false` | `true` |

Note: `IsSingleSession()` returns `true` for **both** SECS-I and HSMS-SS, since both are point-to-point single-session protocols. The new methods provide finer-grained type discrimination.

---

## 4. `hsms.Session` Interface Compatibility

The full `hsms.Session` interface is fully implementable by `secs1.Session`:

| Method | SECS-I Implementation |
|---|---|
| `ID()` | Returns device ID |
| `SendMessage(msg)` | Via `conn.sendMsg` → block splitting → ENQ/EOT/ACK |
| `SendMessageAsync(msg)` | Via `conn.sendMsgAsync` |
| `SendMessageSync(msg)` | Via `conn.sendMsgSync` |
| `SendSECS2Message(msg)` | Via `BaseSession` |
| `SendSECS2MessageAsync(msg)` | Via `BaseSession` |
| `SendDataMessage(...)` | Via `BaseSession` |
| `SendDataMessageAsync(...)` | Via `BaseSession` |
| `ReplyDataMessage(msg, item)` | Via `BaseSession` |
| `AddConnStateChangeHandler(...)` | Via `stateMgr.AddHandler` |
| `AddDataMessageHandler(...)` | Direct implementation |

---

## 5. `hsms.DataMessage` Reuse

`hsms.DataMessage` maps cleanly to SECS-I:

| DataMessage field | SECS-I block header field |
|---|---|
| `sessionID` | Device ID (15-bit) |
| `stream` | Stream / Upper Message ID (byte 2, bits 0-6) |
| `function` | Function / Lower Message ID (byte 3) |
| `waitBit` | W-bit (byte 2, bit 7) |
| `systemBytes` | System Bytes (bytes 6-9) |
| `dataItem` | Body (SECS-II encoded, may span multiple blocks) |

The key difference: in HSMS, `sessionID` is separate from Device ID and carried in the HSMS header. In SECS-I, the Device ID **is** the session identifier. `secs1.Session.ID()` returns the Device ID.

---

## 6. System Bytes Generation

Reuse `hsms.GenerateMsgSystemBytes()` for generating unique system bytes. The SECS-I spec requirement (§8.8) is the same as HSMS: system bytes must be distinct among open transactions.

---

## 7. GEM Package (`gem/`)

The `gem` package generates S9Fx error messages. These are directly applicable to SECS-I:

- `gem.S9F1()` — Unknown Device ID (§9.4.1)
- `gem.S9F3()` — Unrecognized Stream Type
- `gem.S9F5()` — Unrecognized Function Type
- `gem.S9F7()` — Illegal Data
- `gem.S9F9()` — Transaction Timeout (T3)

`secs1.Connection` will use these identically to `hsmsss.Connection`.

---

## 8. Logger Package

No changes needed. `secs1` uses `logger.Logger` interface directly.

---

## 9. Internal Packages

### 9.1 `internal/pool`

`pool.GetTimer` / `pool.PutTimer` — reused for T1, T2, T3, T4 timer pooling.

### 9.2 `internal/queue`

May be useful for the message assembler's block buffer, but standard slices are likely sufficient.

---

## 10. Summary

| Area | Action |
|---|---|
| `hsms` package | **Minor change** — add `IsSECS1()`, `IsHSMSSS()`, `IsHSMSGS()` to `Connection` interface |
| `hsmsss` package | **Minor change** — add `IsSECS1()`, `IsHSMSSS()`, `IsHSMSGS()` method stubs |
| `hsms/conn_state_test.go` | **Minor change** — update test mock implementations |
| `gem` package | **No changes** — S9Fx reused |
| `logger` package | **No changes** |
| `secs2` package | **No changes** — SECS-II encoding/decoding reused |
| `sml` package | **No changes** — SML parsing still works |
| `internal/pool` | **No changes** — timer pool reused |
| New `secs1` package | **New package** — all new code |
| New examples | `examples/secs1_device/` |
| New tests | `tests/secs1_batch_test.sh` |
