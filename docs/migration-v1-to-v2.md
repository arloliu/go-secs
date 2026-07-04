# Migrating from go-secs v1 to v2

This guide walks a Go program from **go-secs v1** (`github.com/arloliu/go-secs`) to
**go-secs v2** (`github.com/arloliu/go-secs/v2`). v2 is a breaking redesign: the public API is
smaller, safer, and organized around a single connection handle. Read section 1 for the shape of the
change, then work through the sections that touch your code. Section 13 is a symbol-by-symbol
reference table you can grep against.

Every Go example below compiles against the v2 module.

---

## 1. Overview — why v2, and what breaks

v2 rebuilds the library around three ideas:

- **Immutable messages and items.** Every `hsms.Message` and every `secs2.Item` is fully initialized
  at construction and never mutated afterward. They are safe to share across goroutines with no
  locking, no reference counting, and no cloning. There are no setters — you *derive* a new value.
- **No object pools.** The v1 pooling API (`Free`, `UsePool`, `Clone`, `CloneCodec`,
  `SnapshotForRelay`) is gone. Messages and items are ordinary GC-owned values. The whole class of
  use-after-free / double-free / retain-past-`Free` bugs is structurally impossible.
- **Connection-centric transport.** There is no separate `Session` object and no `AddSession`. The
  `hsms.Connection` *is* the SECS-II endpoint: you send, reply, and register handlers directly on it.
  `hsmsss.New` returns `hsmsss.Connection` and `secs1.New` returns `secs1.Connection`; both embed
  `hsms.Connection`, so application code that only needs the shared surface stays transport-agnostic.

### What breaks at a glance

| Area | v1 | v2 |
|------|----|----|
| Module path | `github.com/arloliu/go-secs` | `github.com/arloliu/go-secs/v2` |
| Message interface | `hsms.HSMSMessage` (mutable, pooled) | `hsms.Message` (read-only, immutable) |
| Change a field | `msg.SetSessionID(id)` / `SetStreamCode(s)` | `msg.WithSessionID(id)` / `msg.Derive().WithStream(s).Build()` |
| Release a message | `msg.Free()` | nothing — GC owns it |
| Endpoint | `sess := conn.AddSession(id)` then `sess.Send…` | `conn.Send…` directly (Connection is the endpoint) |
| Send call | `sess.SendDataMessage(s, f, w, item)` | `conn.SendDataMessage(ctx, s, f, w, item)` (context-first) |
| Open | `conn.Open(waitOpened bool)` | `conn.Open(ctx, hsms.OpenWaitSelected)` |
| Handler | `func(*DataMessage, hsms.Session)` | `func(*DataMessage, hsms.SECS2Endpoint)` |
| Build config | `hsmsss.NewConnectionConfig(...)` → `hsmsss.NewConnection(ctx, cfg)` | `hsmsss.NewConfig(...)` → `hsmsss.New(cfg)` |
| SECS-II item value | `item.Values()` | typed accessors: `ToASCII()`, `ToInt()`, `IntAt(i)`, `Ints()` … |
| Item shortcuts | `secs2.A`, `secs2.L` are variables | `secs2.A(...)`, `secs2.L(...)` are functions |
| SML parse | `sml.ParseHSMS(text)` | `sml.Parse(text)` |
| Metrics | `conn.GetMetrics()` (atomic fields) | `conn.Metrics()` (shared hsms.ConnectionMetrics) + `ControlMetrics()`/`BlockMetrics()` on transport-specific connections |
| Test logger | `logger.MockLogger` | `logger/loggertest.MockLogger` |

### Performance

A standalone benchmark module ([`benchmarks/`](../benchmarks/)) compares v2 against the latest v1
release: a real active/passive HSMS-SS connection over loopback TCP, plus `secs2.Item`
construct/encode/decode microbenchmarks. Headline result: **every full-connection round trip is
15% to 61% faster than v1**, since v2 has no pooling/`Free()` bookkeeping on the hot path. The one
place v1 has an edge is raw `secs2.Decode`, which always copies its input for immutability where
v1 aliased it — see the `secs2.DecodeOwned` notes in section 9 for the zero-copy alternative when
you already own the buffer. Reproduce the numbers yourself with
`cd benchmarks && make bench-v1 bench-v2 compare`.

Everything else is covered below.

---

## 2. Import path

v2 is a new major version, so every import path gains a `/v2` segment.

```go
// v1
import (
    "github.com/arloliu/go-secs/hsms"
    "github.com/arloliu/go-secs/hsmsss"
    "github.com/arloliu/go-secs/secs2"
    "github.com/arloliu/go-secs/sml"
)

// v2
import (
    "github.com/arloliu/go-secs/v2/hsms"
    "github.com/arloliu/go-secs/v2/hsmsss"
    "github.com/arloliu/go-secs/v2/secs2"
    "github.com/arloliu/go-secs/v2/sml"
)
```

Update `go.mod`:

```bash
go get github.com/arloliu/go-secs/v2@latest
go mod tidy
```

A fast first pass is a search-and-replace of `github.com/arloliu/go-secs/` →
`github.com/arloliu/go-secs/v2/` across your tree; then fix the API changes the compiler flags.

---

## 3. Immutability: delete-only removals

Because messages and items are immutable and GC-owned, an entire family of v1 calls has **no
replacement — you simply delete them.** Grep for each and remove it.

| v1 call | Action | Why it is safe to delete |
|---------|--------|--------------------------|
| `msg.Free()` / `item.Free()` | delete | No pool exists; the GC reclaims the value. |
| `secs2.UsePool(true)` / `secs2.IsUsePool()` | delete | Pooling is removed; items are always plain values. |
| `hsms.UsePool(...)` / `hsms.IsUsePool()` | delete | Same — the message pool is gone. |
| `hsms.GetMessageBuffer(n)` / `hsms.PutMessageBuffer(b)` | delete | Buffer recycling is internal to the transport now. |
| `msg.Clone()` | delete (share the value) | Immutable values are safe to share across goroutines directly. |
| `msg.CloneCodec()` | delete | Concurrent encode is safe on the shared immutable message. |
| `msg.SnapshotForRelay()` | delete (use the message) | Fan-out to many handlers needs no snapshot; hand them the same `*DataMessage`. |
| `item.SetValues(...)` | replace with a fresh constructor | Items are immutable; build the item you want instead of mutating. |
| `msg.SetSessionID(id)` | `msg.WithSessionID(id)` | Returns a new message sharing the body; see below. |
| `msg.SetSystemBytes(b)` | `msg.WithSystemBytes(b)` | Same restamp pattern; `b` is now a `[4]byte`. |
| `msg.SetStreamCode/SetFunctionCode/SetWaitBit(...)` | `msg.Derive().WithStream/WithFunction/WithWaitBit(...).Build()` | Structural change goes through the validating builder. |
| `msg.SetError(err)` / `msg.SetHeader(h)` / `msg.SetID(id)` | delete / rebuild | v2 messages carry no mutable error or raw-header setter. |

### Restamping envelope fields (cheap, no re-encode)

`WithSessionID` and `WithSystemBytes` return a **new** message that shares the original body. The body
is never copied and the SECS-II item is decoded at most once across all derived copies.

```go
package migcheck

import "github.com/arloliu/go-secs/v2/hsms"

func restamp(msg *hsms.DataMessage) *hsms.DataMessage {
    // v1: msg.SetSessionID(42); return msg   (mutated in place)
    // v2: returns a new message; the original is untouched.
    return msg.WithSessionID(42).WithSystemBytes([4]byte{0, 0, 0, 1})
}
```

### Structural changes go through the builder

To change stream / function / wait-bit / item, use `Derive().…Build()`. `Build` runs full validation.

```go
package migcheck

import (
    "github.com/arloliu/go-secs/v2/hsms"
    "github.com/arloliu/go-secs/v2/secs2"
)

func rebuild(src *hsms.DataMessage) (*hsms.DataMessage, error) {
    // v1: src.SetStreamCode(3); src.SetFunctionCode(7); src.SetWaitBit(false)
    return src.Derive().
        WithStream(3).
        WithFunction(7).
        WithWaitBit(false).
        WithItem(secs2.A("payload")).
        Build()
}
```

---

## 4. Context-first API

### Sends take a `context.Context` first

Every blocking send now takes a `context.Context` as its first argument. The context bounds the wait
for the reply (in addition to the protocol T3 timeout) and lets you cancel in-flight sends.

```go
// v1
reply, err := sess.SendDataMessage(1, 13, true, item)

// v2
reply, err := conn.SendDataMessage(ctx, 1, 13, true, item)
```

The same shift applies to `SendDataMessageAsync`, `SendSECS2Message`, and `ReplyDataMessage`.

### `Open` takes a context and a mode

v1's `Open(waitOpened bool)` becomes `Open(ctx, mode)`, where `mode` is an `hsms.OpenMode`:

| v1 | v2 |
|----|----|
| `conn.Open(true)`  (block until selected) | `conn.Open(ctx, hsms.OpenWaitSelected)` |
| `conn.Open(false)` (start in background) | `conn.Open(ctx, hsms.OpenBackground)` |

With `OpenWaitSelected`, `ctx` bounds the synchronous wait for the link to reach the Selected state.
`Close()` remains context-free and is idempotent.

---

## 5. Connection-centric model

### No `Session`, no `AddSession`

v1 required you to obtain a `hsms.Session` from the connection before sending:

```go
// v1
conn, _ := hsmsss.NewConnection(ctx, cfg)
sess := conn.AddSession(sessionID)
sess.AddDataMessageHandler(handler)
reply, _ := sess.SendDataMessage(1, 13, true, item)
```

In v2 the `Connection` embeds `hsms.SECS2Endpoint`, so you send and register handlers on it directly.
There is no `Session` type, no `BaseSession`, and no `AddSession`. The single session ID is a
connection setting (`hsms.WithSessionID`, readable via `conn.SessionID()`).

```go
package migcheck

import (
    "context"

    "github.com/arloliu/go-secs/v2/hsms"
    "github.com/arloliu/go-secs/v2/secs2"
)

func send(ctx context.Context, conn hsms.Connection, handler hsms.DataMessageHandler) error {
    conn.AddDataMessageHandler(handler)
    _, err := conn.SendDataMessage(ctx, 1, 13, true, secs2.NewEmptyItem())
    return err
}
```

### Handler signatures

| Handler | v1 | v2 |
|---------|----|----|
| Data message | `func(*hsms.DataMessage, hsms.Session)` | `func(*hsms.DataMessage, hsms.SECS2Endpoint)` |
| State change | `hsms.ConnStateChangeHandler` = `func(conn hsms.Connection, prev, next hsms.ConnState)` | `hsms.StateChangeHandler` = `func(prev, next hsms.ConnState)` |

The data-message handler's second argument is now the narrow `hsms.SECS2Endpoint` capability
interface (send / reply / register), not a concrete session. As in v1, the handler runs **inline on
the receive goroutine and must not block** — offload slow work, and reply with the asynchronous
`ReplyDataMessage` or `SendDataMessageAsync` rather than a blocking send.

```go
package migcheck

import (
    "context"

    "github.com/arloliu/go-secs/v2/hsms"
)

// v2 data-message handler. ep is the endpoint capability, not a *Session.
func onMessage(msg *hsms.DataMessage, ep hsms.SECS2Endpoint) {
    item, err := msg.Item()
    if err != nil {
        return // undecodable body
    }
    _ = ep.ReplyDataMessage(context.Background(), msg, item)
}

// v2 state-change handler: no Connection argument, just the transition.
func onState(prev, next hsms.ConnState) {
    _ = prev
    _ = next
}
```

Register them the same way, on the connection:

```go
// v2
conn.AddDataMessageHandler(onMessage)
conn.AddConnStateChangeHandler(onState)
```

---

## 6. Config construction renames

Configuration moved from a pointer `*ConnectionConfig` built by `NewConnectionConfig` to a value
`Config` built by `NewConfig`, and the connection constructor dropped its `ctx` argument (the context
now lives on `Open`).

| v1 | v2 |
|----|----|
| `hsmsss.NewConnectionConfig(host, port, opts...)` → `*ConnectionConfig` | `hsmsss.NewConfig(host, port, opts...)` → `Config` |
| `secs1.NewConnectionConfig(host, port, opts...)` → `*ConnectionConfig` | `secs1.NewConfig(host, port, opts...)` → `Config` |
| `hsmsss.NewConnection(ctx, cfg)` | `hsmsss.New(cfg)` |
| `secs1.NewConnection(ctx, cfg)` | `secs1.New(cfg)` |

### Timers lose the `Timeout` suffix and (for HSMS) move to the core

In v1 the HSMS-SS timer options lived on the `hsmsss` package with a `Timeout` suffix. In v2 the
protocol timers are **core `hsms` options** applied through `WithConnectionOption`, and the suffix is
gone:

| v1 (`hsmsss`) | v2 |
|---------------|----|
| `hsmsss.WithT3Timeout(d)` | `hsmsss.WithConnectionOption(hsms.WithT3(d))` |
| `hsmsss.WithT5Timeout(d)` | `hsmsss.WithConnectionOption(hsms.WithT5(d))` |
| `hsmsss.WithT6Timeout(d)` | `hsmsss.WithConnectionOption(hsms.WithT6(d))` |
| `hsmsss.WithT7Timeout(d)` | `hsmsss.WithConnectionOption(hsms.WithT7(d))` |
| `hsmsss.WithT8Timeout(d)` | `hsmsss.WithConnectionOption(hsms.WithT8(d))` |
| `hsmsss.WithLinktestInterval(d)` | `hsmsss.WithConnectionOption(hsms.WithLinktestInterval(d))` |
| `hsmsss.WithLinktestFailThreshold(n)` | `hsmsss.WithConnectionOption(hsms.WithLinktestFailThreshold(n))` |
| `hsmsss.WithSenderQueueSize(n)` | `hsmsss.WithConnectionOption(hsms.WithSenderQueueSize(n))` |
| `hsmsss.WithLogger(l)` | `hsmsss.WithConnectionOption(hsms.WithLogger(l))` |
| `hsmsss.WithCloseConnTimeout(d)` | `hsmsss.WithConnectionOption(hsms.WithCloseTimeout(d))` |
| `hsmsss.WithSendTimeout(d)` | `hsmsss.WithConnectionOption(hsms.WithWriteTimeout(d))` |
| `hsmsss.WithKeepAlivePeriod(d)` | `hsmsss.WithTCPKeepAlive(d)` |

For SECS-I, T1/T2/T4 stay on the `secs1` package but also drop the suffix
(`secs1.WithT1Timeout` → `secs1.WithT1`), and T3 (the reply timeout, shared with the core) is set via
`secs1.WithConnectionOption(hsms.WithT3(d))`.

The session identity term is unified to **`SessionID`** everywhere (see section 7); configure it with
`hsms.WithSessionID(id)` for HSMS-SS and `secs1.WithDeviceID(id)` for SECS-I.

### Role options

| Transport | v1 | v2 |
|-----------|----|----|
| SECS-I equipment/host role | `secs1.WithEquipRole()` / `secs1.WithHostRole()` | `secs1.WithEquipment()` / `secs1.WithHost()` |
| TCP active/passive role | `WithActive()` / `WithPassive()` | unchanged |

> HSMS-SS has no equipment/host role option in v2 (the v1 `hsmsss.WithEquipRole()` /
> `hsmsss.WithHostRole()` are removed). The HSMS role is expressed by `WithActive()` (dial) vs
> `WithPassive()` (listen).

### HSMS-SS config, before and after

```go
package migcheck

import (
    "time"

    "github.com/arloliu/go-secs/v2/hsms"
    "github.com/arloliu/go-secs/v2/hsmsss"
)

func buildHSMSSS() (hsms.Connection, error) {
    // v1:
    //   cfg, _ := hsmsss.NewConnectionConfig("127.0.0.1", 5000,
    //       hsmsss.WithActive(),
    //       hsmsss.WithT3Timeout(30*time.Second),
    //       hsmsss.WithLinktestInterval(30*time.Second),
    //   )
    //   conn, _ := hsmsss.NewConnection(ctx, cfg)
    cfg, err := hsmsss.NewConfig("127.0.0.1", 5000,
        hsmsss.WithActive(),
        hsmsss.WithConnectionOption(hsms.WithSessionID(1000)),
        hsmsss.WithConnectionOption(hsms.WithT3(30*time.Second)),
        hsmsss.WithConnectionOption(hsms.WithLinktestInterval(30*time.Second)),
    )
    if err != nil {
        return nil, err
    }
    return hsmsss.New(cfg)
}
```

### SECS-I config, before and after

```go
package migcheck

import (
    "time"

    "github.com/arloliu/go-secs/v2/hsms"
    "github.com/arloliu/go-secs/v2/secs1"
)

func buildSECS1() (hsms.Connection, error) {
    // v1:
    //   cfg, _ := secs1.NewConnectionConfig("127.0.0.1", 5000,
    //       secs1.WithActive(), secs1.WithHostRole(), secs1.WithDeviceID(1),
    //       secs1.WithT2Timeout(10*time.Second), secs1.WithRetryLimit(3),
    //       secs1.WithT3Timeout(45*time.Second),
    //   )
    //   conn, _ := secs1.NewConnection(ctx, cfg)
    cfg, err := secs1.NewConfig("127.0.0.1", 5000,
        secs1.WithActive(),
        secs1.WithHost(),
        secs1.WithDeviceID(1),
        secs1.WithT2(10*time.Second),
        secs1.WithRetryLimit(3),
        secs1.WithConnectionOption(hsms.WithT3(45*time.Second)),
    )
    if err != nil {
        return nil, err
    }
    return secs1.New(cfg)
}
```

Config knobs with no v2 equivalent (removed; the behavior is either automatic now or handled by the
core): `WithConnectRemoteTimeout`, `WithAcceptConnTimeout`, `WithInitialRetryDelay`,
`WithIdleReadTimeout`, `WithDataMsgQueueSize`, `WithTraceTraffic`,
`WithAutoLinktest` (linktest is controlled by the interval and fail-threshold), and the SECS-I
`WithDuplicateDetection` (duplicate blocks are always detected).

---

## 7. Message and accessor renames

The mutable, pooled `hsms.HSMSMessage` interface is replaced by the read-only `hsms.Message`
interface. Both `*hsms.DataMessage` and `*hsms.ControlMessage` satisfy it.

| v1 | v2 | Notes |
|----|----|-------|
| `hsms.HSMSMessage` | `hsms.Message` | Read-only; no setters, `Free`, or `Clone`. |
| `msg.Type() int` | `msg.Type() hsms.MsgType` | Defined type; constants like `hsms.DataMsgType`. |
| `msg.StreamCode() uint8` | `msg.Stream() uint8` | On `*DataMessage`. |
| `msg.FunctionCode() uint8` | `msg.Function() uint8` | On `*DataMessage`. |
| `msg.WaitBit() bool` | `msg.WaitBit() bool` | Unchanged. |
| `msg.Item() secs2.Item` | `msg.Item() (secs2.Item, error)` | Now returns a decode error; body is decoded lazily. |
| — | `msg.DecodeErr() error` | Cached body-decode error without re-decoding. |
| `msg.SessionID() uint16` | `msg.SessionID() uint16` | Unchanged. |
| `msg.SystemBytes() []byte` | `msg.SystemBytes() [4]byte` | Value type; no internal aliasing. |
| `msg.Header() []byte` | `msg.HeaderBytes() [10]byte` | Value type. |
| `msg.ID() uint32` / `SetID(id)` | — | No numeric message-ID accessor; use `SystemBytes() [4]byte`. |
| `msg.Error()` / `SetError()` | — (removed) | See the error-model note below. |
| `msg.IsDataMessage()` / `ToDataMessage()` | type-assert `*hsms.DataMessage` | Or switch on `msg.Type()`. |
| `msg.IsControlMessage()` / `ToControlMessage()` | type-assert `*hsms.ControlMessage` | Same. |

### Reading a data message (v2)

```go
package migcheck

import (
    "fmt"

    "github.com/arloliu/go-secs/v2/hsms"
)

func inspect(msg *hsms.DataMessage) {
    _ = msg.Stream()          // v1: msg.StreamCode()
    _ = msg.Function()        // v1: msg.FunctionCode()
    _ = msg.WaitBit()
    _ = msg.SessionID()
    _ = msg.SystemBytes()     // [4]byte value, not []byte
    _ = msg.HeaderBytes()     // [10]byte value

    item, err := msg.Item()   // v1: item := msg.Item() (no error)
    if err != nil {
        fmt.Println("undecodable body:", err)
        return
    }
    _ = item
}
```

### Error model: no `Message.Error()`

v1 carried an error *inside* the message (`Error()` / `SetError()`). v2 keeps the two real error
channels separate and removes the in-message error:

- **Body decode error** — the SECS-II body is decoded lazily. `DataMessage.Item()` returns
  `(item, error)`; the same error is cached and re-readable via `DataMessage.DecodeErr()`.
- **Peer rejection** — a `Reject.req` answering a synchronous send is returned to the caller as an
  `*hsms.RejectError`, inspected with `errors.As`:

```go
package migcheck

import (
    "context"
    "errors"

    "github.com/arloliu/go-secs/v2/hsms"
    "github.com/arloliu/go-secs/v2/secs2"
)

func sendAndClassify(ctx context.Context, conn hsms.Connection) {
    _, err := conn.SendDataMessage(ctx, 1, 1, true, secs2.NewEmptyItem())
    var re *hsms.RejectError
    if errors.As(err, &re) {
        _ = re.Reason // byte: the E37 reject reason code
    }
}
```

### Reject reason codes are now `byte`

The reject-reason getters return `byte` (matching `RejectError.Reason`), not `int`:

| v1 | v2 |
|----|----|
| `hsms.GetRejectReasonCode(msg) (int, error)` | `hsms.GetRejectReasonCode(msg hsms.Message) (byte, error)` |
| — | `ctrl.RejectReasonCode() (byte, error)` (method on `*ControlMessage`) |

Two select-status constants also lost their v1 typos: `hsms.SelectStatusActived` →
`hsms.SelectStatusAlreadyActive`, and `hsms.SelectStatusEntitActived` →
`hsms.SelectStatusEntityAlreadyActive` (values unchanged: 1 and 6). The `Undefinied`/`LinkTest`
spellings were also corrected to `hsms.UndefinedMsgType`, `hsms.LinktestReqType`,
`hsms.LinktestRspType`.

### Control-message constructors take `[4]byte` system bytes

`NewSelectReq`, `NewDeselectReq`, `NewLinktestReq`, `NewSeparateReq`, and `NewRejectReqRaw` now take a
`[4]byte` for system bytes instead of `[]byte`, and `NewSelectRsp` / `NewDeselectRsp` /
`NewLinktestRsp` take a concrete `*hsms.ControlMessage` request instead of the `HSMSMessage`
interface.

---

## 8. SML renames

The parser type and package entry points were renamed to drop the `HSMS` prefix, and a small encoder
API plus a sentinel error were added.

| v1 | v2 |
|----|----|
| `sml.HSMSParser` | `sml.Parser` |
| `sml.NewHSMSParser()` | `sml.NewParser(opts ...sml.ParserOption)` |
| `sml.ParseHSMS(text)` | `sml.Parse(text)` |
| `sml.ParseHSMSSlow(text) ([]*hsms.DataMessage, []error)` | `sml.ParseStrict(text) ([]*hsms.DataMessage, error)` |
| `p.ParseMessage(text, lazy bool)` | `p.ParseMessage(text)` (no `lazy` argument) |
| `p.ParseMessageHeader(text)` | `p.ParseHeader(text)` |
| `sml.WithStrictMode(true)` (package global) / `p.WithStrictMode(true)` | `sml.NewParser(sml.WithParserStrictMode(true))` |
| `sml.NewRawSMLItem(...)` / `sml.RawSMLItem` | — (removed; no equivalent) |
| — | `sml.ErrNoMessage` (sentinel; use `errors.Is`) |
| — | `sml.Encode`, `sml.EncodeStrict`, `sml.NewEncoder(...)` |

```go
package migcheck

import (
    "errors"

    "github.com/arloliu/go-secs/v2/hsms"
    "github.com/arloliu/go-secs/v2/sml"
)

func parseSML(text string) ([]*hsms.DataMessage, error) {
    // v1: msgs, err := sml.ParseHSMS(text)
    msgs, err := sml.Parse(text)
    if errors.Is(err, sml.ErrNoMessage) {
        return nil, nil // empty input
    }
    return msgs, err
}

func parseStrictWithParser(text string) (*hsms.DataMessage, error) {
    // v1: p := sml.NewHSMSParser(); p.WithStrictMode(true); p.ParseMessage(text, false)
    p := sml.NewParser(sml.WithParserStrictMode(true))
    return p.ParseMessage(text)
}
```

Rendering back to SML text: v1 used the global quote toggles (`hsms.UseStreamFunctionSingleQuote()`,
`secs2.UseASCIISingleQuote()`, …). Those package-global toggles are gone. Configure quoting per
`sml.Encoder` instead:

```go
package migcheck

import (
    "github.com/arloliu/go-secs/v2/hsms"
    "github.com/arloliu/go-secs/v2/secs2"
    "github.com/arloliu/go-secs/v2/sml"
)

func render(msg *hsms.DataMessage) (string, error) {
    enc := sml.NewEncoder(
        sml.WithASCIIQuote(sml.QuoteSingle),
        sml.WithSFQuote(sml.QuoteSingle),
        sml.WithIndent("    "),
    )
    return enc.EncodeMessage(msg)
}

// A single item still renders with the package helper (equivalent to item.ToSML()).
func renderItem(item secs2.Item) string {
    return sml.Encode(item)
}
```

> `sml.Parse` maps to the v1 default (non-strict) parse, and `sml.ParseStrict` decodes non-printable
> bytes written as `0xHH` tokens. If you relied on the v1 `ParseHSMSSlow` reference parser, migrate to
> `ParseStrict`; note the return shape changed from `([]*hsms.DataMessage, []error)` to a single
> `error`.

---

## 9. secs2 item changes

### Reading values: `Values()` is gone

The v1 catch-all `item.Values() any` (which returned an untyped `[]byte` / `[]int64` / `string` / …)
is removed in favor of typed accessors. Pick the family that fits:

- **Copy accessors** return a fresh slice/value: `ToList`, `ToBinary`, `ToBoolean`, `ToASCII`,
  `ToJIS8`, `ToLocalizedStr`, `ToInt`, `ToUint`, `ToFloat`.
- **Indexed accessors** return one element by value: `ItemAt(i)`, `ByteAt(i)`, `BoolAt(i)`,
  `IntAt(i)`, `UintAt(i)`, `FloatAt(i)`.
- **Zero-copy iterators** yield values from internal storage: `Items()`, `Bools()`, `Ints()`,
  `Uints()`, `Floats()`.

```go
package migcheck

import "github.com/arloliu/go-secs/v2/secs2"

func readValues(item secs2.Item) {
    // v1: raw := item.Values()   // any; caller type-asserts to []int64
    if vals, err := item.ToInt(); err == nil { // []int64 copy
        _ = vals
    }
    if v, err := item.IntAt(0); err == nil { // single element
        _ = v
    }
    for v := range item.Ints() { // zero-copy iteration
        _ = v
    }
}
```

`item.Get(indices...)` and `item.Type() string` are unchanged. `item.Size()` and `item.ToBytes()` are
unchanged. `item.SetValues(...)`, `item.Clone()`, and `item.Free()` are removed (immutable value).
The `LocalizedStrItem.SetLSH(...)` mutator is removed — pass the header to
`secs2.NewLocalizedStrItem(lsh, value)` at construction.

### Shortcut constructors are now functions

In v1, `L`, `A`, `J`, `W`, `B`, and `BOOLEAN` were package **variables** (function-typed aliases),
while `I1..I8`, `U1..U8`, `F4`, `F8` were already functions. In v2 **all** shortcuts are plain
functions. Call sites are unaffected; only code that took the address of a shortcut
(`&secs2.A`) or reassigned it (`secs2.A = myFn`) needs to change.

```go
package migcheck

import "github.com/arloliu/go-secs/v2/secs2"

func build() secs2.Item {
    // These calls are identical in v1 and v2:
    return secs2.L(
        secs2.A("model"),
        secs2.U4(256),
        secs2.B(0x01, 0x02),
        secs2.BOOLEAN(true, false),
    )
}
```

### The `*WithBytes` constructors are removed

`NewASCIIItemWithBytes`, `NewBinaryItemWithBytes`, `NewBooleanItemWithBytes`, and
`NewListItemWithBytes` are gone. To build an item from wire bytes, use `secs2.Decode`.

### `secs2.Decode` — decode an item from wire bytes

Item decoding lives in `secs2` now (v1 used `hsms.DecodeSECS2Item`):

```go
package migcheck

import "github.com/arloliu/go-secs/v2/secs2"

func decodeItem(wire []byte) (secs2.Item, error) {
    // v1: item, err := hsms.DecodeSECS2Item(wire)
    return secs2.Decode(wire) // copies wire; empty input yields NewEmptyItem()
}
```

`hsms.DecodeSECS2Item` in v1 aliased its input buffer (near-zero-alloc). `secs2.Decode`
always copies instead, since v2 items are immutable and must not have a lifetime dependency
on a caller-owned buffer — this shows up as a real cost decoding large binary/ASCII payloads.
If you're decoding a buffer you already own outright (e.g. one you just read from a file, with
no other referents), `secs2.DecodeOwned(wire)` skips that top-level copy and transfers
ownership of `wire` to the returned `Item`, so you must not mutate or reuse `wire` afterward.
This restores v1-level performance: Binary, ASCII, JIS-8, and localized-string payloads all
alias `wire` directly with no further copy.

### `secs2.NewMessage` — the transport-agnostic base builder

v2 adds a generic SECS-II message builder in `secs2` (it moved here from `gem`; see section 10):

```go
package migcheck

import "github.com/arloliu/go-secs/v2/secs2"

func generic() secs2.SECS2Message {
    // stream, function, replyExpected (W-bit), item
    return secs2.NewMessage(1, 13, true, secs2.A("MDLN"))
}
```

Send it with `conn.SendSECS2Message(ctx, msg)`.

### `FormatCode` is a defined type

`secs2.FormatCode` changed from a type **alias** (`= int`) to a **defined type** (`uint8`). Code that
stored a format code in an `int`, or did untyped arithmetic on it, needs an explicit conversion. The
`secs2.*FormatCode` constants are unchanged in value.

```go
package migcheck

import "github.com/arloliu/go-secs/v2/secs2"

func formatCode(item secs2.Item) {
    // Compare against typed constants:
    if item.IsASCII() {
        var fc secs2.FormatCode = secs2.ASCIIFormatCode
        _ = uint8(fc) // explicit conversion needed if you want the numeric value
    }
}
```

---

## 10. gem changes

The v1 `gem` package exposed a generic message builder (`gem.NewMessage` / `gem.Message`) plus seven
no-argument S9 factories. v2 moves the generic builder into `secs2`, makes the S9 factories faithful
to SEMI E5 (they carry the offending header), and adds a bounded set of SEMI E30 (GEM) role builders.

### Generic builder moved to secs2

| v1 | v2 |
|----|----|
| `gem.NewMessage(s, f uint8, w bool, item) *gem.Message` | `secs2.NewMessage(stream, function byte, replyExpected bool, item) secs2.SECS2Message` |
| `gem.Message` (type) | `secs2.Message` (type) |

### S9 factories now carry the offending header

The v1 S9 factories were no-argument and sent an empty body. Per SEMI E5, an S9 error message carries
the header of the message that caused it. The v2 builders take that header (from the offending
message's `HeaderBytes() [10]byte`):

| v1 | v2 |
|----|----|
| `gem.S9F1()` … `gem.S9F7()`, `gem.S9F11()` | `gem.S9F1(mhead [10]byte)` … `gem.S9F7`, `gem.S9F11` (MHEAD body) |
| `gem.S9F9()` | `gem.S9F9(shead [10]byte)` (SHEAD body) |
| `gem.S9F13()` | `gem.S9F13(mexp string, edid secs2.Item)` |

```go
package migcheck

import (
    "context"

    "github.com/arloliu/go-secs/v2/gem"
    "github.com/arloliu/go-secs/v2/hsms"
)

// Reply to an unrecognized message with S9F1 (Unrecognized Device ID), passing the
// offending message's header as required by SEMI E5.
func rejectUnknown(ctx context.Context, conn hsms.Connection, offending *hsms.DataMessage) error {
    _, err := conn.SendSECS2Message(ctx, gem.S9F1(offending.HeaderBytes()))
    return err
}
```

### New E30 role builders

v2 adds pure value builders for the common GEM messages — every one returns a
`secs2.SECS2Message` you send via `conn.SendSECS2Message`. Equipment-defined IDs (CEID, RPTID, ALID,
DATAID, SVID) are passed as `secs2.Item` so you control the SECS-II type.

Available: `S1F1`, `S1F2`, `S1F2Host`, `S1F13`, `S1F13Host`, `S1F14`, `S1F14Host`, `S2F17`, `S2F18`,
`S2F31`, `S2F32`, `S2F37`, `S2F38`, `S5F1`, `S5F2`, `S6F11`, `S6F12`, and the `Report` composition
helper.

```go
package migcheck

import (
    "context"

    "github.com/arloliu/go-secs/v2/gem"
    "github.com/arloliu/go-secs/v2/hsms"
    "github.com/arloliu/go-secs/v2/secs2"
)

func gemExamples(ctx context.Context, conn hsms.Connection) {
    // Establish Communications Request (equipment form).
    _, _ = conn.SendSECS2Message(ctx, gem.S1F13("MODEL-A", "1.0.0"))

    // Alarm report S5F1: alarm code + equipment-defined ALID + text.
    _, _ = conn.SendSECS2Message(ctx, gem.S5F1(0x80, secs2.U4(1001), "OVER TEMP"))

    // Event report S6F11: DATAID, CEID, and one report built with the helper.
    report := gem.Report(secs2.U4(1), secs2.A("value"))
    _, _ = conn.SendSECS2Message(ctx, gem.S6F11(secs2.U4(0), secs2.U4(200), report))
}
```

If you built S-type messages by hand in v1 with `gem.NewMessage`, switch those call sites to
`secs2.NewMessage` (or a gem role builder where one now exists).

---

## 11. Metrics

v2 relocates and expands the metrics taxonomy across `hsms`, `hsmsss`, and `secs1`. The accessor is renamed from `GetMetrics()` to `Metrics()`, and the shape changed from exported atomic fields to reader methods. Transport-specific metrics now live on package-specific `ConnectionMetrics` types, accessed via package-specific methods on the returned connection.

| v1 | v2 | Status | Note |
|----|----|--------|------|
| `hsmsss.ConnectionMetrics.{DataMsgSendCount,DataMsgRecvCount,DataMsgErrCount}` | `hsms.ConnectionMetrics.{DataMsgSendCount,DataMsgRecvCount,DataMsgErrCount}` (via `Connection.Metrics()`) | retained | Same names, now on the shared type used by both transports. |
| `hsmsss.ConnectionMetrics.{LinktestSendCount,LinktestRecvCount,LinktestErrCount}` | `hsmsss.ConnectionMetrics.{LinktestSendCount,LinktestRecvCount,LinktestErrCount}` (via `hsmsss.Connection.ControlMetrics()`) | changed | Same names, still HSMS-SS-owned — but `hsmsss.New` now returns `hsmsss.Connection` (an `hsms.Connection` plus `ControlMetrics()`) instead of bare `hsms.Connection`, so reaching these from a value typed `hsms.Connection` needs `conn.(hsmsss.Connection).ControlMetrics()`. |
| — | `hsmsss.ConnectionMetrics.{SelectEstablishedCount,SeparateRecvCount,RejectSentCount,RejectRecvCount,LinktestReqRecvCount}` | new | HSMS-SS control-plane counters with no v1 equivalent. |
| `hsmsss.ConnectionMetrics.ConnRetryGauge` | `hsms.ConnectionMetrics.Reconnecting()` (renamed from `ConnRetryCount`) | changed | Same 0/1 gauge; renamed because "Count" implied cumulative. Pair with the new `Reconnects()` for the cumulative count. |
| — | `hsms.ConnectionMetrics.Reconnects()` | new | Cumulative count of successful re-establishments; there was no v1 equivalent to this specific cumulative metric. |
| `secs1.ConnectionMetrics.{BlockSendCount,BlockRecvCount,BlockRetryCount}` | `secs1.ConnectionMetrics.{BlockSendCount,BlockRecvCount,BlockRetryCount}` (via `secs1.Connection.BlockMetrics()`) | changed | Same names, still secs1-owned — but `secs1.New` now returns `secs1.Connection` instead of bare `hsms.Connection`, so reaching these needs `conn.(secs1.Connection).BlockMetrics()`. |
| — | `secs1.ConnectionMetrics.{BlockSendFailedCount,BlockNAKSentCount,ContentionYieldCount,BlockDupDropCount,PartialTimeoutCount,BlockDirDropCount}` | new | SECS-I line-level counters with no v1 equivalent. |

### Reading metrics in v2

```go
package migcheck

import (
    "github.com/arloliu/go-secs/v2/hsms"
    "github.com/arloliu/go-secs/v2/hsmsss"
    "github.com/arloliu/go-secs/v2/secs1"
)

// HSMS-SS: shared data metrics + HSMS-specific control metrics
func readHSMSSMetrics(conn hsms.Connection) {
    m := conn.Metrics() // *hsms.ConnectionMetrics (shared)
    _ = m.DataMsgSendCount()
    _ = m.DataMsgRecvCount()
    _ = m.DataMsgErrCount()
    _ = m.Reconnecting()  // 0 or 1
    _ = m.Reconnects()    // cumulative

    // For HSMS-SS control metrics, type-assert to hsmsss.Connection:
    if hsmsConn, ok := conn.(hsmsss.Connection); ok {
        ctrl := hsmsConn.ControlMetrics()
        _ = ctrl.LinktestSendCount()
        _ = ctrl.LinktestRecvCount()
        _ = ctrl.LinktestErrCount()
        _ = ctrl.SelectEstablishedCount()
        _ = ctrl.SeparateRecvCount()
        _ = ctrl.RejectSentCount()
        _ = ctrl.RejectRecvCount()
        _ = ctrl.LinktestReqRecvCount()
    }
}

// SECS-I: shared data metrics + SECS-I-specific block metrics
func readSECS1Metrics(conn hsms.Connection) {
    m := conn.Metrics() // *hsms.ConnectionMetrics (shared)
    _ = m.DataMsgSendCount()
    _ = m.DataMsgRecvCount()
    _ = m.DataMsgErrCount()
    _ = m.Reconnecting()
    _ = m.Reconnects()

    // For SECS-I block metrics, type-assert to secs1.Connection:
    if s1Conn, ok := conn.(secs1.Connection); ok {
        block := s1Conn.BlockMetrics()
        _ = block.BlockSendCount()
        _ = block.BlockRecvCount()
        _ = block.BlockRetryCount()
        _ = block.BlockSendFailedCount()
        _ = block.BlockNAKSentCount()
        _ = block.ContentionYieldCount()
        _ = block.BlockDupDropCount()
        _ = block.PartialTimeoutCount()
        _ = block.BlockDirDropCount()
    }
}
```

---

## 12. Removed subsystems reference

These v1 exports were implementation plumbing that v2 no longer exposes. If you referenced any of
them, drop the dependency (the behavior is now internal to the connection engine).

| v1 export | Where | Replacement |
|-----------|-------|-------------|
| `hsms.Session`, `hsms.BaseSession`, `hsms.NewBaseSession` | `session.go`, `base_session.go` | Send on `hsms.Connection` (embeds `SECS2Endpoint`). |
| `hsms.ConnStateMgr`, `hsms.NewConnStateMgr` | `conn_state.go` | The FSM is internal; observe via `AddConnStateChangeHandler` and `conn.State()`. |
| `hsms.TaskManager` (+ `TaskFunc`, `TaskRecvFunc`, `TaskMsgFunc`, `TaskDataMsgFunc`, `TaskCancelFunc`) | `task.go` | Internal goroutine management; no public equivalent. |
| `hsms.OpState`, `hsms.AtomicOpState` (+ `ClosedState`…`OpenedState`) | `op_state.go` | Internal; use `conn.State()` (`hsms.ConnState`). |
| `hsms.GenerateMsgID`, `hsms.ToSystemBytes` | `id_gen.go` | Retained. `ToSystemBytes` now returns `[4]byte` (was `[]byte`). |
| `hsms.GenerateMsgSystemBytes` | `id_gen.go` | Removed; use `hsms.ToSystemBytes(hsms.GenerateMsgID())`. |
| `hsms.UsePool`, `hsms.IsUsePool`, `hsms.GetMessageBuffer`, `hsms.PutMessageBuffer`, `hsms.DefaultMessageBufferSize` | `pool.go` | No pool; messages are GC-owned. |
| `hsms.DataMessage.SnapshotForRelay`, `hsms.DataMessage.CloneCodec` | `data_msg.go` | Share the immutable `*DataMessage` directly. |
| `hsms.NewControlMessage`, `NewDataMessageFromRawItem`, `NewErrorDataMessage` | `data_msg.go`, `control_msg.go` | Use the typed factories / `NewDataMessage` / `secs2.Decode`. |
| `hsms.MsgInfo`, `MsgInfoSML`, `MsgInfoFromFields`, `MsgHexString` | `hsms_msg.go` | No equivalent; log fields yourself. |
| `hsms.UseStreamFunction*Quote`, `secs2.UseASCII*Quote`, `secs2.UseJIS8*Quote`, `secs2.UseHexLiteral`, `secs2.UseBinaryLiteral` | various | Configure per `sml.Encoder` (`WithSFQuote`, `WithASCIIQuote`, …). |
| `secs2.StringToBytes`, `secs2.BytesToString` | `ascii.go` | No equivalent; use the standard library. |
| `secs1.Block`, `secs1.ParseBlock`, `secs1.SplitMessage`, `secs1.AssembleMessage` | `block.go`, `message.go` | Block framing is internal; work at the `hsms.DataMessage` level. |
| `logger.MockLogger`, `logger.NewMockLogger` | `logger/mock.go` | Moved to `logger/loggertest` (keeps testify out of the `logger` import graph). |
| `logger.GetLogger()` | `logger/default.go` | `logger.Default()`. |

### Test logger moved to `logger/loggertest`

The mock logger left the production package (so importing `logger` no longer pulls in testify). Import
it from the subpackage in your tests:

```go
package migcheck

import (
    "github.com/arloliu/go-secs/v2/logger"
    "github.com/arloliu/go-secs/v2/logger/loggertest"
)

func newTestLogger() logger.Logger {
    // v1: logger.NewMockLogger()
    return loggertest.NewMockLogger()
}

func processLogger() logger.Logger {
    // v1: logger.GetLogger()
    return logger.Default()
}
```

`logger.LogLevel` also changed from a type **alias** (`= int8`) to a **defined type** (`int8`). The
level constants (`logger.DebugLevel`…`logger.FatalLevel`) and their values are unchanged; only code
that assigned a bare `int8` to a `LogLevel` without conversion needs an explicit cast.

---

## 13. Symbol-by-symbol API diff

One sub-table per package. `kind` is one of removed / renamed / changed / new. Every *removed* row
names a replacement or states "no equivalent".

### hsms

| v1 symbol | v2 symbol | kind | migration action |
|-----------|-----------|------|------------------|
| `HSMSMessage` (interface) | `Message` | renamed | Read-only interface; drop setters/`Free`/`Clone`. |
| `Message.Type() int` | `Message.Type() MsgType` | changed | Defined type; compare to `hsms.*MsgType`. |
| `HSMSMessage.SetSessionID/SetID/SetSystemBytes/SetHeader/SetError` | — | removed | Immutable; use `WithSessionID`/`WithSystemBytes` or `Derive().Build()`. |
| `HSMSMessage.ID()/SetID() uint32` | — | removed | No numeric ID; use `SystemBytes() [4]byte`. |
| `HSMSMessage.Error()/SetError()` | — | removed | Body error via `DataMessage.Item()`/`DecodeErr()`; reject via `*RejectError`. |
| `HSMSMessage.Header() []byte` | `Message.HeaderBytes() [10]byte` | changed | Value type. |
| `HSMSMessage.SystemBytes() []byte` | `Message.SystemBytes() [4]byte` | changed | Value type (no aliasing). |
| `HSMSMessage.Free()` | — | removed | No pool; GC-owned. |
| `HSMSMessage.Clone()` | — | removed | Share the immutable value. |
| `HSMSMessage.IsDataMessage/ToDataMessage/IsControlMessage/ToControlMessage` | — | removed | Type-assert `*DataMessage`/`*ControlMessage`, or switch on `Type()`. |
| `DataMessage.StreamCode()/FunctionCode()` | `DataMessage.Stream()/Function()` | renamed | — |
| `DataMessage.Item() secs2.Item` | `DataMessage.Item() (secs2.Item, error)` | changed | Handle the decode error. |
| `DataMessage.SetStreamCode/SetFunctionCode/SetWaitBit` | `Derive().WithStream/WithFunction/WithWaitBit().Build()` | renamed | Through the validating builder. |
| — | `DataMessage.Derive() *DataMessageBuilder` | new | Builder for structural derivation. |
| — | `DataMessage.WithSessionID/WithSystemBytes` | new | O(header) restamp; shares body. |
| — | `DataMessage.DecodeErr()/BodyLen()/AppendBodyTo()` | new | — |
| `NewDataMessage(…, systemBytes []byte, …)` | `NewDataMessage(…, systemBytes [4]byte, …)` | changed | Pass `[4]byte`; nil item allowed (empty body). |
| `NewDataMessageFromRawItem` | — | removed | `secs2.Decode` then `NewDataMessage`. |
| `NewErrorDataMessage` | — | removed | No error-carrying messages. |
| `NewControlMessage(header, replyExpected)` | — | removed | Use the typed `NewSelectReq`/… factories. |
| `NewSelectRsp(HSMSMessage, byte)` | `NewSelectRsp(*ControlMessage, byte)` | changed | Concrete request type; `[4]byte` sysbytes on the Req factories. |
| `GetRejectReasonCode(HSMSMessage) (int, error)` | `GetRejectReasonCode(Message) (byte, error)` | changed | Returns `byte`. |
| — | `ControlMessage.RejectReasonCode() (byte, error)` | new | Method form. |
| — | `RejectError` (`Reason byte`) | new | `errors.As` on a synchronous send. |
| `SelectStatusActived` / `SelectStatusEntitActived` | `SelectStatusAlreadyActive` / `SelectStatusEntityAlreadyActive` | renamed | Values unchanged (1, 6). |
| `UndefiniedMsgType` / `LinkTestReqType` / `LinkTestRspType` | `UndefinedMsgType` / `LinktestReqType` / `LinktestRspType` | renamed | Spelling fixes. |
| `DataMessageHandler = func(*DataMessage, Session)` | `func(*DataMessage, SECS2Endpoint)` | changed | Second arg is the endpoint capability. |
| `ConnStateChangeHandler = func(Connection, prev, next ConnState)` | `StateChangeHandler = func(prev, next ConnState)` | renamed | No `Connection` argument. |
| `Connection.Open(waitOpened bool)` | `Connection.Open(ctx, OpenMode)` | changed | `OpenWaitSelected` / `OpenBackground`. |
| `Connection.AddSession(id) Session` | — | removed | Connection embeds `SECS2Endpoint`; send directly. |
| `Connection.GetLogger/IsSingleSession/IsGeneralSession/IsSECS1` | — | removed | No equivalent (internal role/logging). |
| — | `Connection.State()/Metrics()/UpdateConfigOptions()` + `SECS2Endpoint` | new | Now on the interface. |
| `Session`, `BaseSession`, `NewBaseSession` | `SECS2Endpoint` | removed | Send/reply/register on the Connection. |
| `Session.SendMessage/SendMessageAsync/SendMessageSync(HSMSMessage)` | — | removed | Use `SendSECS2Message`/`SendDataMessage`. |
| `Session.SendSECS2MessageAsync` | — | removed | `SendDataMessageAsync`, or `SendSECS2Message`. |
| `Session.SendDataMessage(s,f,w,item)` | `SECS2Endpoint.SendDataMessage(ctx,s,f,w,item)` | changed | Context-first. |
| `Session.ReplyDataMessage(primary,item)` | `SECS2Endpoint.ReplyDataMessage(ctx,primary,item)` | changed | Context-first. |
| `Session.ID() uint16` | `SECS2Endpoint.SessionID() uint16` | renamed | Unified identity term. |
| `ConnStateMgr`, `NewConnStateMgr`, `OpState`, `AtomicOpState`, `TaskManager`, `Task*` funcs | — | removed | Internal engine; no equivalent. |
| `GenerateMsgID`, `ToSystemBytes` | `GenerateMsgID`, `ToSystemBytes` | retained | `ToSystemBytes` returns `[4]byte`. |
| `GenerateMsgSystemBytes` | — | removed | Use `ToSystemBytes(GenerateMsgID())`. |
| `UsePool`, `IsUsePool`, `GetMessageBuffer`, `PutMessageBuffer`, `DefaultMessageBufferSize` | — | removed | No pool. |
| `MsgInfo`, `MsgInfoSML`, `MsgInfoFromFields`, `MsgHexString` | — | removed | No equivalent. |
| `UseStreamFunctionNoQuote/SingleQuote/DoubleQuote`, `StreamFunctionQuote` | — | removed | Per-`sml.Encoder` `WithSFQuote`. |
| `DecodeHSMSMessage(data) (HSMSMessage, error)` | `DecodeHSMSMessage(data) (Message, error)` | changed | Same call, new return type. |
| `DecodeSECS2Item(data)` | `secs2.Decode(data)` | renamed | Moved to `secs2`. |
| `DecodeMessage(msgLen, input)` | — | removed | Internal; use `DecodeHSMSMessage`. |
| `ControlMessage` reason-code / status / type constants | same names (typo fixes noted above) | unchanged | Values preserved. |

### hsmsss

| v1 symbol | v2 symbol | kind | migration action |
|-----------|-----------|------|------------------|
| `NewConnectionConfig(...) (*ConnectionConfig, error)` | `NewConfig(...) (Config, error)` | renamed | Value config. |
| `NewConnection(ctx, cfg) (*Connection, error)` | `New(cfg) (hsmsss.Connection, error)` | renamed | Drops `ctx`; returns an interface embedding `hsms.Connection`. |
| `WithT3Timeout`…`WithT8Timeout` | `WithConnectionOption(hsms.WithT3)`…`WithT8` | renamed | Core options; suffix dropped. |
| `WithLinktestInterval/WithLinktestFailThreshold/WithSenderQueueSize/WithLogger` | `WithConnectionOption(hsms.With…)` | renamed | Core options. |
| `WithCloseConnTimeout` | `WithConnectionOption(hsms.WithCloseTimeout)` | renamed | — |
| `WithSendTimeout` | `WithConnectionOption(hsms.WithWriteTimeout)` | renamed | — |
| `WithKeepAlivePeriod` | `WithTCPKeepAlive` | renamed | — |
| `WithEquipRole/WithHostRole` | — | removed | HSMS role is `WithActive`/`WithPassive`. |
| `WithAutoLinktest(bool)` | — | removed | Controlled by interval + fail-threshold. |
| `WithValidateDataMessage` | `hsms.WithSessionIDValidation` | changed | v1 enabled inbound SessionID-mismatch rejection (auto-S9F1 + drop) BY DEFAULT; v2 ships with it OFF and no equivalent at all until `WithSessionIDValidation` was added back as an explicit opt-in (default still off, to avoid a silent behavior change for anyone already on v2). If your v1 code called `WithValidateDataMessage(false)` to tolerate non-compliant equipment, no action is needed — v2's default already matches that. If you relied on the v1 default (`true`), call `hsms.WithSessionIDValidation(true)` to restore it. |
| `WithConnectRemoteTimeout/WithAcceptConnTimeout/WithInitialRetryDelay/WithIdleReadTimeout/WithDataMsgQueueSize/WithTraceTraffic` | — | removed | No equivalent (automatic now / no direct replacement). |
| — | `WithDialer(DialFunc)` | new | Inject a custom dialer. |
| `Connection.GetMetrics() *hsmsss.ConnectionMetrics` | `Connection.Metrics() *hsms.ConnectionMetrics` | renamed | Reader methods; see section 11. |
| `Connection.AddSession(id)` | — | removed | Send on the Connection. |
| `Session`, `NewSession` | — | removed | No session type. |
| `ConnectionMetrics` (exported atomic fields) | `hsms.ConnectionMetrics` (reader methods) + `hsmsss.ConnectionMetrics` (via `ControlMetrics()`) | changed | Data-message counters on shared `hsms.ConnectionMetrics` via `Metrics()`; linktest counters on HSMS-SS-specific `hsmsss.ConnectionMetrics` via `Connection.ControlMetrics()` (type-assert to `hsmsss.Connection`). See section 11 for full details. |

### secs1

| v1 symbol | v2 symbol | kind | migration action |
|-----------|-----------|------|------------------|
| `NewConnectionConfig(...) (*ConnectionConfig, error)` | `NewConfig(...) (Config, error)` | renamed | Value config. |
| `NewConnection(ctx, cfg) (*Connection, error)` | `New(cfg) (secs1.Connection, error)` | renamed | Drops `ctx`; returns an interface embedding `hsms.Connection`. |
| `WithEquipRole()/WithHostRole()` | `WithEquipment()/WithHost()` | renamed | Same roles. |
| `secs1.WithT1/WithT2/WithT4` (build-time) | `secs1.WithT1/WithT2/WithT4` (build-time, unchanged) or `secs1.WithConnectionOption(hsms.WithT1(...))` via `Connection.UpdateConfigOptions` (NEW: now live-updatable) | changed | v1 had no live-update path for T1/T2/T4; v2 promotes them into the shared `hsms.TimerConfig`, so they now ride the same live-update rail as T3/T5-T8. |
| `WithT3Timeout` | `WithConnectionOption(hsms.WithT3)` | renamed | Reply timeout is core. |
| `WithDeviceID/WithRetryLimit` | same | unchanged | — |
| `WithValidateDataMessage` | `hsms.WithSessionIDValidation` | changed | v1 enabled inbound SessionID-mismatch rejection (auto-S9F1 + drop) BY DEFAULT; v2 ships with it OFF and no equivalent at all until `WithSessionIDValidation` was added back as an explicit opt-in (default still off, to avoid a silent behavior change for anyone already on v2). If your v1 code called `WithValidateDataMessage(false)` to tolerate non-compliant equipment, no action is needed — v2's default already matches that. If you relied on the v1 default (`true`), call `hsms.WithSessionIDValidation(true)` to restore it. |
| `WithConnectTimeout` (deprecated) / `WithConnectRemoteTimeout/WithAcceptConnTimeout/WithCloseConnTimeout/WithSendTimeout/WithMaxRetryDelay/WithInitialRetryDelay/WithDuplicateDetection/WithSenderQueueSize/WithDataMsgQueueSize` | — | removed | No equivalent, or `hsms.WithCloseTimeout`/`hsms.WithWriteTimeout` via `WithConnectionOption`. |
| `WithLogger(l)` | `WithConnectionOption(hsms.WithLogger(l))` | renamed | Core option. |
| `WithKeepAlivePeriod` | `WithTCPKeepAlive` | renamed | — |
| — | `WithDialer(DialFunc)` | new | Custom dialer. |
| `Connection.Open(waitOpened bool)` | `Connection.Open(ctx, OpenMode)` | changed | Via `hsms.Connection`. |
| `Connection.AddSession(id) hsms.Session` | — | removed | Connection is the endpoint. |
| `Session.ID() uint16` | `Connection.SessionID() uint16` | renamed | Reports the wire device ID. |
| `Session.SendMessage/SendMessageAsync/SendMessageSync` | — | removed | Context-first `SendDataMessage`/`SendSECS2Message`. |
| `Connection.GetMetrics() *secs1.ConnectionMetrics` | `Connection.Metrics() *hsms.ConnectionMetrics` (shared) + `secs1.ConnectionMetrics` (via `BlockMetrics()` after type-assert to `secs1.Connection`) | changed | Block counters are surfaced but require type-asserting the connection to `secs1.Connection` and calling `BlockMetrics()`. See section 11. |
| `Block`, `ParseBlock`, `SplitMessage`, `AssembleMessage`, block/control-byte consts | — | removed | Framing is internal; work at `hsms.DataMessage`. |
| `Connection.GetLogger/IsSingleSession/IsGeneralSession/IsSECS1` | — | removed | No equivalent. |

### secs2

| v1 symbol | v2 symbol | kind | migration action |
|-----------|-----------|------|------------------|
| `FormatCode = int` (alias) | `FormatCode uint8` (defined type) | changed | Explicit conversion where used as `int`. |
| `Item.Values() any` | typed accessors (`ToInt`, `IntAt`, `Ints`, …) | removed | Pick the copy/indexed/iterator accessor. |
| `Item.SetValues(...)` | — | removed | Build a new item. |
| `Item.Clone()` | — | removed | Immutable; share directly. |
| `Item.Free()` | — | removed | No pool. |
| `LocalizedStrItem.SetLSH(uint16)` | — | removed | Pass `lsh` to `NewLocalizedStrItem`. |
| `L`, `A`, `J`, `W`, `B`, `BOOLEAN` (vars) | same names (funcs) | changed | Call sites unchanged; no `&secs2.A`/reassignment. |
| `I1..I8`, `U1..U8`, `F4`, `F8` (funcs) | same | unchanged | — |
| `NewASCIIItemWithBytes/NewBinaryItemWithBytes/NewBooleanItemWithBytes/NewListItemWithBytes` | — | removed | `secs2.Decode`. |
| `UsePool/IsUsePool` | — | removed | No pool. |
| `UseASCIISingleQuote/…/ASCIIQuote/WithASCIIStrictMode/UseHexLiteral/UseBinaryLiteral/UseJIS8*Quote/JS88Quote` | — | removed | Configure `sml.Encoder`. |
| `StringToBytes/BytesToString` | — | removed | Standard library. |
| — | `Decode(data) (Item, error)` | new | Item decode (was `hsms.DecodeSECS2Item`). |
| — | `NewMessage(...) SECS2Message`, `Message` (type) | new | Moved from `gem`. |
| — | `ItemAt/ByteAt/BoolAt/IntAt/UintAt/FloatAt`, `Items/Bools/Ints/Uints/Floats`, `AppendTo/EncodedLen/AppendBinaryTo` | new | Indexed / iterator / zero-copy encode accessors. |
| `SECS2Message` interface, `Get`, `Type()`, `Size()`, `ToBytes()`, `ToSML()`, `NewXxxItem`, `NewUTF8StrItem` | same | unchanged | — |

### sml

| v1 symbol | v2 symbol | kind | migration action |
|-----------|-----------|------|------------------|
| `HSMSParser` | `Parser` | renamed | — |
| `NewHSMSParser()` | `NewParser(opts...)` | renamed | Options via `ParserOption`. |
| `ParseHSMS(text)` | `Parse(text)` | renamed | — |
| `ParseHSMSSlow(text) ([]*DataMessage, []error)` | `ParseStrict(text) ([]*DataMessage, error)` | renamed | Single `error` now. |
| `Parser.ParseMessage(text, lazy bool)` | `Parser.ParseMessage(text)` | changed | No `lazy` arg. |
| `Parser.ParseMessageHeader(text)` | `Parser.ParseHeader(text)` | renamed | — |
| `WithStrictMode(bool)` (pkg + method) | `WithParserStrictMode(bool)` (option) | renamed | Pass to `NewParser`; or use `ParseStrict`. |
| `RawSMLItem`, `NewRawSMLItem` | — | removed | No equivalent. |
| — | `ErrNoMessage` | new | `errors.Is` for empty input. |
| — | `Encode`, `EncodeStrict`, `Encoder`, `NewEncoder`, `EncoderOption`, `WithASCIIQuote/WithSFQuote/WithIndent/WithEncoderStrictMode`, `QuoteStyle`/`QuoteSingle`/`QuoteDouble` | new | Configurable SML rendering. |

### gem

| v1 symbol | v2 symbol | kind | migration action |
|-----------|-----------|------|------------------|
| `NewMessage(s, f uint8, w bool, item) *Message` | `secs2.NewMessage(stream, function byte, replyExpected bool, item) secs2.SECS2Message` | renamed | Moved to `secs2`. |
| `Message` (type) | `secs2.Message` | renamed | Moved to `secs2`. |
| `S9F1()`…`S9F7()`, `S9F11()` | `S9F1(mhead [10]byte)`…`S9F7`, `S9F11` | changed | Carry the offending MHEAD. |
| `S9F9()` | `S9F9(shead [10]byte)` | changed | Carry the offending SHEAD. |
| `S9F13()` | `S9F13(mexp string, edid secs2.Item)` | changed | E5-faithful body. |
| — | `S1F1/S1F2/S1F2Host/S1F13/S1F13Host/S1F14/S1F14Host/S2F17/S2F18/S2F31/S2F32/S2F37/S2F38/S5F1/S5F2/S6F11/S6F12/Report` | new | E30 role builders. |

### logger

| v1 symbol | v2 symbol | kind | migration action |
|-----------|-----------|------|------------------|
| `LogLevel = int8` (alias) | `LogLevel int8` (defined type) | changed | Explicit cast from bare `int8`. |
| `GetLogger()` | `Default()` | renamed | — |
| `MockLogger`, `NewMockLogger` | `loggertest.MockLogger`, `loggertest.NewMockLogger` | renamed | Import `logger/loggertest` in tests. |
| `Logger`, `NewSlog`, `With`, `SetLevel`, `Level`, level constants, package `Debug/Info/Warn/Error/Fatal` | same | unchanged | — |
