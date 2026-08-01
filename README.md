# go-secs

`go-secs` is a Go library for semiconductor-equipment communication. It implements
[SECS-II](https://en.wikipedia.org/wiki/SECS-II) (SEMI E5),
[HSMS / HSMS-SS](https://en.wikipedia.org/wiki/High-Speed_SECS_Message_Services) (SEMI E37 / E37.1),
and SECS-I over TCP/IP (SEMI E4), together with an SML (SECS Message Language) parser.

![Test Status](https://github.com/arloliu/go-secs/actions/workflows/ci.yaml/badge.svg)
[![Go Reference](https://pkg.go.dev/badge/github.com/arloliu/go-secs/v2.svg)](https://pkg.go.dev/github.com/arloliu/go-secs/v2)
[![Go Report Card](https://goreportcard.com/badge/github.com/arloliu/go-secs/v2)](https://goreportcard.com/report/github.com/arloliu/go-secs/v2)

> This is the **v2** module (`github.com/arloliu/go-secs/v2`), built around an immutable-message
> model. Every message and data item is immutable and safe to share across goroutines.
>
> Need v1 (`github.com/arloliu/go-secs`, no `/v2` suffix)? It's in maintenance mode — see the
> [v1 branch](https://github.com/arloliu/go-secs/tree/v1) or
> [pkg.go.dev/github.com/arloliu/go-secs](https://pkg.go.dev/github.com/arloliu/go-secs).

## Supports

* SECS-I over TCP/IP (SEMI E4)
* SECS-II (SEMI E5)
* HSMS (SEMI E37), HSMS-SS (SEMI E37.1)
* SML — parses single/double-quoted stream-functions, an optional message name, single/double-quoted
  ASCII items, hex byte literals, and escape sequences

## Features

### SECS-II Operations

* **Comprehensive data item support:** integers, floating-point numbers, booleans, binary, lists,
  ASCII strings, JIS-8, and Localized Character Strings (FormatCode 0o22) with a configurable
  encoding scheme (UTF-8, UCS-2, Shift-JIS, and others via the Localized String Header).
* **Immutable and concurrency-safe:** every `secs2.Item` is immutable and no method exposes mutable
  internal storage, so items can be shared freely across goroutines. Constructors never panic — bad
  input yields an item carrying a deferred error you inspect with `Error()`.
* **Serialization:** encode an item to its SECS-II wire bytes with `ToBytes`, or append into an
  existing buffer with `AppendTo`.
* **SML generation:** render an item to SML text with `ToSML`.

### HSMS / HSMS-SS Communication

* **HSMS-SS (Single Session):** a streamlined implementation of the SEMI E37.1 single-session mode.
* **Active and passive modes:** connect as a TCP client (active) or listen as a TCP server (passive).
* **Connection is the endpoint:** `hsms.Connection` embeds `hsms.SECS2Endpoint`, so you send
  messages, reply, and register handlers directly on the connection. The HSMS session ID is
  configured per connection.
* **Connection-state management:** an explicit state machine (`hsms.ConnState`) with registrable
  state-change handlers.
* **Resilience:** automatic reconnection, and an auto-linktest with a configurable failure threshold
  for tolerating transient T6 timeouts. Activity-based linktest suppression (on by default) probes
  only idle links and does not count a probe timeout toward the disconnect threshold when the
  failure evaluation observes signs of life — protecting slow, aged equipment busy with a long command from probe-induced
  disconnects, while a silent dead link is still dropped within a bounded time. See the
  [linktest suppression guide](docs/guides/linktest-suppression.md) for the behavior contract,
  tuning, trade-offs, and precise detection bounds; disable with
  `hsms.WithLinktestSuppression(false)`.
* **Metrics:** live atomic counters (sent/received/error data messages, linktests, retries) via
  `Connection.Metrics()`.
* **Error handling:** a peer `Reject.req` is surfaced to the caller as an `*hsms.RejectError`.

### SECS-I over TCP/IP Communication

* **SEMI E4 compliant:** the SECS-I block-transfer and message protocols over a TCP/IP stream.
* **Half-duplex protocol:** the ENQ/EOT/ACK/NAK handshake for line-direction control.
* **Contention resolution:** Master/Slave contention per SEMI E4 (Equipment = Master, Host = Slave).
* **Multi-block messages:** large messages are split into blocks (≤ 244 body bytes each) and
  reassembled on receive.
* **Configurable timeouts:** T1 (inter-character), T2 (protocol), and T4 (inter-block) on the line,
  plus the T3 reply timeout shared with the core.
* **Duplicate detection:** duplicate blocks are detected and discarded.
* **Active and passive modes:** TCP client (active) and TCP server (passive).
* **Automatic reconnection:** the active side re-dials after a line failure.
* **Unified interface:** the same `hsms.Connection` interface as HSMS-SS, so application code is
  transport-agnostic.

### SML Operations

* **Parsing:** `sml.Parse` turns SML text into HSMS data messages.
* **Formats:** an optional message name, single- or double-quoted stream-functions and ASCII items,
  hex byte literals, and escape sequences.
* **Strict mode:** `sml.ParseStrict` adheres to the ASCII standard and treats escape characters
  literally.

> See the [SML document](sml/README.md) for details.

## Packages

* **secs1** — SECS-I over TCP/IP (SEMI E4). Returns the same `hsms.Connection` as `hsmsss`.
* **secs2** — SECS-II data items and messages.
* **hsms** — the shared message model, the `Connection`/`SECS2Endpoint` interfaces, HSMS message
  encoding/decoding, and the connection engine.
* **hsmsss** — HSMS-SS (Single Session) transport per SEMI E37.1.
* **sml** — the SML parser.
* **gem** — helpers for constructing common GEM (SEMI E30) messages.
* **logger** — a small logging façade for integrating your own logging framework.

## Performance

`v2` is benchmarked against the latest `v1` release with a standalone module (see
[`benchmarks/`](benchmarks/)): a real active/passive HSMS-SS connection over loopback TCP, plus
`secs2.Item` construct/encode/decode microbenchmarks.

* **Every full-connection round trip is faster than v1** — 15% to 61% less time, since v2 avoids
  v1's pooling/`Free()` bookkeeping on the hot path.
* **`secs2.Decode` always copies its input** (v1 aliased it), so the returned `Item` never depends
  on the caller's buffer. For a buffer the caller already owns outright (e.g. one just read from a
  socket or a file), `secs2.DecodeOwned` skips that copy and matches v1's decode performance —
  including for ASCII/JIS-8/localized-string payloads, not just binary.
* **Single-value `IntItem`/`UintItem`/`FloatItem`/`BooleanItem` decode without allocating a backing
  slice** — a scalar fast path in `secs2.Decode`/`DecodeOwned` stores the lone value inline instead.
  Roughly 18-20% faster and one fewer allocation per scalar item decoded, up to ~42% fewer
  allocations on payloads dominated by single-value items (e.g. a mixed-type record).
* Hot atomic counters in `hsms.ConnectionMetrics` / `secs1.ConnectionMetrics` are cache-line padded
  to prevent false sharing between counters under concurrent access.
* Reproduce it yourself: `cd benchmarks && make bench-v1 bench-v2 compare`, or
  `go test ./secs2item/v2/... -bench . -benchmem` against a prior commit for a focused before/after.

## Message and Item Object Model

* **`secs2.SECS2Message`** defines the core of a SECS-II message: stream code, function code, W-bit,
  and the SECS-II data item.
* **`hsms.Message`** is the read-only interface implemented by every immutable HSMS message; both
  `hsms.DataMessage` and `hsms.ControlMessage` satisfy it. `DataMessage` carries SECS-II data;
  `ControlMessage` manages the HSMS connection.
* **`secs2.Item`** is the unified interface for SECS-II data items. All data types implement it.

```text
hsms.Message (interface)
├── hsms.DataMessage
└── hsms.ControlMessage

secs2.Item (interface)
├── ASCIIItem          (NewASCIIItem)
├── BinaryItem         (NewBinaryItem)
├── BooleanItem        (NewBooleanItem)
├── FloatItem          (NewFloatItem;  shortcuts F4, F8)
├── IntItem            (NewIntItem;    shortcuts I1, I2, I4, I8)
├── UintItem           (NewUintItem;   shortcuts U1, U2, U4, U8)
├── JIS8Item           (NewJIS8Item)
├── LocalizedStrItem   (NewLocalizedStrItem, NewUTF8StrItem)
└── ListItem           (NewListItem)
```

## Usage

### Installation

```bash
go get github.com/arloliu/go-secs/v2
```

Import the packages you need under the `github.com/arloliu/go-secs/v2/` path, for example
`github.com/arloliu/go-secs/v2/hsmsss` or `github.com/arloliu/go-secs/v2/secs2`.

### Working with SECS-II data items

```go
import "github.com/arloliu/go-secs/v2/secs2"

// Build a nested list. Constructors never panic; on bad input they return an item
// carrying a deferred error you can inspect with Error().
list := secs2.NewListItem(
    secs2.NewASCIIItem("test1"),     // index 0
    secs2.NewIntItem(4, 1, 2, 3, 4), // index 1: I4 with four values
    secs2.NewListItem(               // index 2: nested list
        secs2.NewASCIIItem("test2"),
        secs2.NewASCIIItem("test3"),
    ),
)

// Numeric-type shortcut constructors are also available: I1/I2/I4/I8, U1/U2/U4/U8, F4/F8.
u := secs2.U4(256)

// Get returns (Item, error). The IsX / ToX accessors report and extract typed values.
first, err := list.Get(0)
if err == nil && first.IsASCII() {
    s, _ := first.ToASCII() // "test1"
    _ = s
}

// Reach into a nested list with multiple indices.
nested, err := list.Get(2, 1) // ASCII item "test3"
_, _ = nested, u
```

### Parsing SML

```go
import "github.com/arloliu/go-secs/v2/sml"

input := `MessageName:'S7F3' W
<L
    <A "path">
    <A "model">
    <A "version">
    <L
        <U4 256>
        <A "value">
    >
>
.`

msgs, err := sml.Parse(input)
if err != nil {
    // handle error
}

for _, msg := range msgs {
    _ = msg.Stream()   // 7
    _ = msg.Function() // 3
    _ = msg.WaitBit()  // true
}
```

### HSMS-SS host (active mode)

```go
package main

import (
    "context"
    "log"
    "time"

    "github.com/arloliu/go-secs/v2/hsms"
    "github.com/arloliu/go-secs/v2/hsmsss"
    "github.com/arloliu/go-secs/v2/secs2"
)

// handleMessage runs inline on the connection's receive goroutine. It MUST NOT block:
// reply asynchronously with ep.ReplyDataMessage, or offload slow work to your own goroutine.
func handleMessage(msg *hsms.DataMessage, ep hsms.SECS2Endpoint) {
    if msg.Stream() == 98 && msg.Function() == 1 {
        item, err := msg.Item()
        if err != nil {
            return
        }
        _ = ep.ReplyDataMessage(context.Background(), msg, item)
    }
}

func main() {
    ctx := context.Background()

    // Build the configuration. Shared core knobs (session ID, T3–T8, linktest, logger)
    // are passed through hsmsss.WithConnectionOption.
    cfg, err := hsmsss.NewConfig("127.0.0.1", 5000,
        hsmsss.WithActive(), // dial outbound
        hsmsss.WithConnectionOption(hsms.WithSessionID(1000)),
        hsmsss.WithConnectionOption(hsms.WithT3(30*time.Second)),
    )
    if err != nil {
        log.Fatal(err)
    }

    conn, err := hsmsss.New(cfg)
    if err != nil {
        log.Fatal(err)
    }
    defer conn.Close()

    // The Connection embeds hsms.SECS2Endpoint, so handlers are registered on it directly.
    conn.AddDataMessageHandler(handleMessage)

    // Open and block until the link reaches the Selected state.
    if err := conn.Open(ctx, hsms.OpenWaitSelected); err != nil {
        log.Fatal(err)
    }

    // Send S99F1 with the W-bit set and wait for the reply.
    reply, err := conn.SendDataMessage(ctx, 99, 1, true, secs2.NewASCIIItem("test"))
    if err != nil {
        log.Fatal(err)
    }
    _ = reply // process the reply
}
```

### HSMS-SS equipment (passive mode)

```go
package main

import (
    "context"
    "log"
    "time"

    "github.com/arloliu/go-secs/v2/hsms"
    "github.com/arloliu/go-secs/v2/hsmsss"
    "github.com/arloliu/go-secs/v2/secs2"
)

func handleMessage(msg *hsms.DataMessage, ep hsms.SECS2Endpoint) {
    if msg.Stream() == 99 && msg.Function() == 1 {
        item, err := msg.Item()
        if err != nil {
            return
        }
        _ = ep.ReplyDataMessage(context.Background(), msg, item)
    }
}

func main() {
    ctx := context.Background()

    cfg, err := hsmsss.NewConfig("127.0.0.1", 5000,
        hsmsss.WithPassive(), // listen for an inbound connection
        hsmsss.WithConnectionOption(hsms.WithSessionID(1000)),
        hsmsss.WithConnectionOption(hsms.WithT3(30*time.Second)),
    )
    if err != nil {
        log.Fatal(err)
    }

    conn, err := hsmsss.New(cfg)
    if err != nil {
        log.Fatal(err)
    }
    defer conn.Close()

    conn.AddDataMessageHandler(handleMessage)

    if err := conn.Open(ctx, hsms.OpenWaitSelected); err != nil {
        log.Fatal(err)
    }

    // Send S98F1 with the W-bit set and wait for the reply.
    reply, err := conn.SendDataMessage(ctx, 98, 1, true, secs2.NewASCIIItem("test"))
    if err != nil {
        log.Fatal(err)
    }
    _ = reply // process the reply
}
```

### SECS-I host (active mode)

```go
package main

import (
    "context"
    "log"
    "time"

    "github.com/arloliu/go-secs/v2/hsms"
    "github.com/arloliu/go-secs/v2/secs1"
    "github.com/arloliu/go-secs/v2/secs2"
)

func handleMessage(msg *hsms.DataMessage, ep hsms.SECS2Endpoint) {
    item, err := msg.Item()
    if err != nil {
        return
    }
    _ = ep.ReplyDataMessage(context.Background(), msg, item)
}

func main() {
    ctx := context.Background()

    cfg, err := secs1.NewConfig("127.0.0.1", 5000,
        secs1.WithActive(),       // TCP client
        secs1.WithHost(),         // host role (Slave per SEMI E4)
        secs1.WithDeviceID(1),    // 15-bit device ID
        secs1.WithT2(10*time.Second),
        secs1.WithRetryLimit(3),
        secs1.WithConnectionOption(hsms.WithT3(45*time.Second)), // reply timeout
    )
    if err != nil {
        log.Fatal(err)
    }

    conn, err := secs1.New(cfg)
    if err != nil {
        log.Fatal(err)
    }
    defer conn.Close()

    conn.AddDataMessageHandler(handleMessage)

    if err := conn.Open(ctx, hsms.OpenWaitSelected); err != nil {
        log.Fatal(err)
    }

    // Send S1F1 with the W-bit set and wait for the reply.
    reply, err := conn.SendDataMessage(ctx, 1, 1, true, secs2.NewASCIIItem("test"))
    if err != nil {
        log.Fatal(err)
    }
    _ = reply // process the reply
}
```

### SECS-I equipment (passive mode)

```go
package main

import (
    "context"
    "log"
    "time"

    "github.com/arloliu/go-secs/v2/hsms"
    "github.com/arloliu/go-secs/v2/secs1"
)

func handleMessage(msg *hsms.DataMessage, ep hsms.SECS2Endpoint) {
    item, err := msg.Item()
    if err != nil {
        return
    }
    _ = ep.ReplyDataMessage(context.Background(), msg, item)
}

func main() {
    ctx := context.Background()

    cfg, err := secs1.NewConfig("127.0.0.1", 5000,
        secs1.WithPassive(),     // TCP server
        secs1.WithEquipment(),    // equipment role (Master per SEMI E4)
        secs1.WithDeviceID(1),    // 15-bit device ID
        secs1.WithT2(10*time.Second),
        secs1.WithConnectionOption(hsms.WithT3(45*time.Second)),
    )
    if err != nil {
        log.Fatal(err)
    }

    conn, err := secs1.New(cfg)
    if err != nil {
        log.Fatal(err)
    }
    defer conn.Close()

    conn.AddDataMessageHandler(handleMessage)

    if err := conn.Open(ctx, hsms.OpenWaitSelected); err != nil {
        log.Fatal(err)
    }

    // Send S1F13 (Establish Communications Request) with the W-bit set and wait
    // for the reply. A nil item sends an empty message body.
    reply, err := conn.SendDataMessage(ctx, 1, 13, true, nil)
    if err != nil {
        log.Fatal(err)
    }
    _ = reply // process the reply
}
```
