# go-secs v2.0.0-rc5 Gap Closure — Implementation Plan

> **For executors:** Implement task-by-task. Dispatch a **fresh subagent per task**
> using the **Model** and **Effort** noted in each task header; review between
> tasks; never run two subagents against the same source file at once (see
> Execution Order). Steps use checkbox (`- [ ]`) syntax for tracking. Design spec:
> `docs/specs/2026-07-07-v2-rc5-gap-closure-design.md` (read it before starting).

**Goal:** Close the six rc5 gaps (undecodable-message signal, active-connect
timeout, codec read-duality, `Is*()`/`To*()` deferred-error trap, SECS-I
clarification, malformed-message test helper) plus two `secs1/doc.go` doc-drift
items — keeping v2's immutable, lazy-decode design intact and making the correct
thing the default.

**Architecture:** All additive except two enumerated breaks (send-wrapper return
shape on an undecodable reply; `AddDecodeErrorHandler` on the exported
`SECS2Endpoint`). The Gap 1 primary-path divert lives in `connection.RouteData`
(which holds `c.metrics`) and calls promoted `session` helpers — the `*connection`
**embeds** the `*session` and `session.rt == the connection`, so no
`TransportRuntime` interface change is needed.

**Tech Stack:** Go 1.24+, standard `go test`. No new dependencies.

## Global Constraints

- Module path is `github.com/arloliu/go-secs/v2`; never expose `internal/*` types in
  public signatures or Godoc.
- Godoc on every new exported symbol; **no internal jargon codes** (no D5b/SP5/§4a-style
  tokens) in Godoc — public SEMI/SECS/HSMS references are fine.
- Run `make lint` and fix all findings before every commit (repo rule).
- Commit messages: Conventional Commits (`feat:`/`fix:`/`docs:`/`test:`); **never** add
  `Co-Authored-By` or any attribution trailer.
- Tests: run targeted tests per step. For any full-suite run, skip fuzz:
  `go test ./... -skip '^Fuzz'` (a known stress-test fuzz flake).
- Branch: work on `v2`. rc5 tags land directly on `v2`; no PR to `main`, no merge.
- Regression guards must have teeth: after a guard passes, transiently reintroduce the
  bug and confirm the guard fails, then revert.

---

## File Structure

| File | Task | Responsibility |
|------|------|----------------|
| `hsms/hsmstest/malformed.go` (new) | 1 | Build a valid-framing, undecodable-body `*DataMessage` for tests |
| `hsms/connection_metrics.go` | 2 | New `bodyDecodeErr` counter + `BodyDecodeErrCount()` |
| `hsms/endpoint.go` | 3 | `DecodeErrorHandler` type + interface method |
| `hsms/session.go` | 3, 5 | decode-error handler storage/dispatch; reply-path decode check |
| `hsms/hsmstest/endpoint.go` | 3 | `FakeEndpoint` mirrors the new interface method |
| `hsms/connection_runtime.go` | 4 | Primary-path eager-decode divert in `RouteData` |
| `hsms/data_msg_codec.go` | 6 | `*DataMessageCodec` read delegators + `ToDataMessage` |
| `secs2/*.go` (Godoc only) | 7 | `To*()`/`Is*()` accessor doc callouts to `Error()` |
| `hsmsss/config.go`, `hsmsss/transport_active.go` | 8 | `hsmsss.WithConnectTimeout` + dial wrap |
| `secs1/config.go`, `secs1/transport.go` | 9 | `secs1.WithConnectTimeout` + dial wrap |
| `secs1/doc.go` | 10 | Gap 3 note + two doc-drift fixes |

## Execution Order & Parallelism

Dependencies (→ = "must finish before"):

```
1 ─┐
2 ─┼→ 4 (RouteData divert)          3 → 5 (both touch session.go — sequential)
3 ─┘                                1 → 5 (test helper)
1 → 4, 3 → 4
```

- **Sequential group (hsms core):** 2 → 3 → {4, 5}. Task 4 (`connection_runtime.go`)
  and Task 5 (`session.go`) touch *different* files and may run in parallel **after**
  Task 3, but **not** alongside Task 3 (Task 3 also edits `session.go`). Task 1 first
  (unblocks 4 and 5 tests).
- **Independent (any time, parallel-safe — distinct files/packages):** Task 6
  (`data_msg_codec.go`), Task 7 (`secs2`), Task 8 (`hsmsss`), Task 9 (`secs1` config/
  transport), Task 10 (`secs1/doc.go`).
- Suggested wall-clock-optimal order: **1, 2, 6, 7, 8, 9, 10 in parallel where agents
  differ**, then **3**, then **4 and 5**.

---

## Task 1: `hsmstest.MalformedDataMessage` helper (Gap 6)

**Model:** Sonnet **Effort:** high **Depends on:** none

Wire-format forging is finicky and this unblocks every Gap 1 test, so it goes first
and warrants high effort despite being a test helper.

**Files:**
- Create: `hsms/hsmstest/malformed.go`
- Test: `hsms/hsmstest/malformed_test.go`

**Interfaces:**
- Consumes: `hsms.NewDataMessage(stream, function uint8, replyExpected bool, sessionID uint16, systemBytes [4]byte, item secs2.Item) (*hsms.DataMessage, error)`; `hsms.DecodeHSMSMessage(data []byte) (hsms.Message, error)`; `(*hsms.DataMessage).ToBytes() []byte`, `.DecodeErr() error`, `.ToDataMessage() (*hsms.DataMessage, bool)`.
- Produces: `func MalformedDataMessage(stream, function uint8, waitBit bool) *hsms.DataMessage` — a `*DataMessage` with valid header accessors but `DecodeErr() != nil`.

- [ ] **Step 1: Write the failing test**

Create `hsms/hsmstest/malformed_test.go`:

```go
package hsmstest_test

import (
	"testing"

	"github.com/arloliu/go-secs/v2/hsms/hsmstest"
)

func TestMalformedDataMessage(t *testing.T) {
	msg := hsmstest.MalformedDataMessage(6, 11, true)

	if msg == nil {
		t.Fatal("MalformedDataMessage returned nil")
	}
	// Header accessors are valid on an undecodable frame.
	if got := msg.Stream(); got != 6 {
		t.Errorf("Stream() = %d, want 6", got)
	}
	if got := msg.Function(); got != 11 {
		t.Errorf("Function() = %d, want 11", got)
	}
	if !msg.WaitBit() {
		t.Error("WaitBit() = false, want true")
	}
	// The body must fail to decode lazily.
	if err := msg.DecodeErr(); err == nil {
		t.Error("DecodeErr() = nil, want a decode error")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./hsms/hsmstest/ -run TestMalformedDataMessage -v`
Expected: FAIL — `undefined: hsmstest.MalformedDataMessage`.

- [ ] **Step 3: Write the helper**

Create `hsms/hsmstest/malformed.go`:

```go
package hsmstest

import (
	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
)

// MalformedDataMessage returns a *hsms.DataMessage whose HSMS framing is valid — so
// its header accessors (Stream/Function/WaitBit/SessionID/ID) succeed — but whose
// SECS-II body fails to decode lazily: msg.DecodeErr() != nil and msg.Item() returns
// that error. Use it to exercise decode-error handling (see AddDecodeErrorHandler)
// without hand-forging wire bytes.
//
// It builds a well-formed single-item message, then corrupts one body byte so the
// item's declared length overruns the frame while the outer length prefix stays
// intact; DecodeHSMSMessage still yields a (lazy) *DataMessage, and the deferred body
// decode fails when first forced.
func MalformedDataMessage(stream, function uint8, waitBit bool) *hsms.DataMessage {
	// A valid 1-byte BINARY item: format/length header + one payload byte.
	item := secs2.NewBinaryItem([]byte{0x00})

	good, err := hsms.NewDataMessage(stream, function, waitBit, 0, [4]byte{0, 0, 0, 1}, item)
	if err != nil {
		panic("hsmstest.MalformedDataMessage: building base message: " + err.Error())
	}

	raw := good.ToBytes() // length-prefixed HSMS frame: [4]len | [10]header | body

	// The body starts after the 4-byte length prefix and 10-byte header. Its first
	// byte is the SECS-II item format/length-bytes descriptor; the following byte(s)
	// are the item length. Inflate the declared length far past what the frame carries
	// so the deferred decode fails, without touching the outer 4-byte length prefix.
	const bodyLenByteOffset = 4 + 10 + 1 // len prefix + header + format byte
	if len(raw) <= bodyLenByteOffset {
		panic("hsmstest.MalformedDataMessage: base frame shorter than expected")
	}
	raw[bodyLenByteOffset] = 0xFF // claim a 255-byte item where only 1 byte follows

	msg, decErr := hsms.DecodeHSMSMessage(raw)
	if decErr != nil {
		// Framing must still succeed (lazy decode); a framing error here is a bug in
		// this helper, not the caller's code.
		panic("hsmstest.MalformedDataMessage: framing unexpectedly failed: " + decErr.Error())
	}
	dm, ok := msg.ToDataMessage()
	if !ok {
		panic("hsmstest.MalformedDataMessage: decoded message is not a data message")
	}

	return dm
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./hsms/hsmstest/ -run TestMalformedDataMessage -v`
Expected: PASS. If `DecodeErr()` is still nil, widen the corruption (also set the
format byte at offset `4+10` to a list/large-length descriptor); the invariant is
"framing OK, body decode fails."

- [ ] **Step 5: Verify `secs2.NewBinaryItem` name**

Run: `grep -n "func NewBinaryItem" secs2/*.go`
Expected: a constructor exists. If the name differs (e.g. `NewBinary`), use the actual
name; any small valid item works.

- [ ] **Step 6: Lint & commit**

```bash
make lint
git add hsms/hsmstest/malformed.go hsms/hsmstest/malformed_test.go
git commit -m "test(hsmstest): add MalformedDataMessage helper for decode-error tests"
```

---

## Task 2: `BodyDecodeErr` metric (Gap 1 support)

**Model:** Sonnet **Effort:** medium **Depends on:** none

Mechanical — mirrors the existing `decodeErr` counter exactly. Kept separate from
Task 4 so the counter can be reviewed and tested on its own.

**Files:**
- Modify: `hsms/connection_metrics.go` (struct field ~35, accessor ~52-60, inc helper ~149)
- Test: `hsms/connection_metrics_test.go`

**Interfaces:**
- Produces: `func (m *ConnectionMetrics) BodyDecodeErrCount() uint64`; unexported `func (m *ConnectionMetrics) incBodyDecodeErr()`.

- [ ] **Step 1: Write the failing test**

Add to `hsms/connection_metrics_test.go` (create if absent, `package hsms`):

```go
func TestBodyDecodeErrCount(t *testing.T) {
	var m ConnectionMetrics
	if got := m.BodyDecodeErrCount(); got != 0 {
		t.Fatalf("initial BodyDecodeErrCount() = %d, want 0", got)
	}
	m.incBodyDecodeErr()
	m.incBodyDecodeErr()
	if got := m.BodyDecodeErrCount(); got != 2 {
		t.Fatalf("BodyDecodeErrCount() = %d, want 2", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./hsms/ -run TestBodyDecodeErrCount -v`
Expected: FAIL — `m.BodyDecodeErrCount undefined`.

- [ ] **Step 3: Add the field**

In `hsms/connection_metrics.go`, in the `ConnectionMetrics` struct next to `decodeErr`:

```go
	decodeErr              atomic.Uint64 // inbound frame read successfully but failed to decode/route
	bodyDecodeErr          atomic.Uint64 // frame framed OK + counted as received, but its lazy SECS-II body failed to decode and was diverted to a decode-error handler
```

- [ ] **Step 4: Add the accessor**

After `DecodeErrCount()` (~line 60):

```go
// BodyDecodeErrCount returns the number of inbound data messages that framed
// successfully and were counted by DataMsgRecvCount, but whose lazy SECS-II body
// failed to decode and were diverted to a registered DecodeErrorHandler instead of
// the normal handlers.
//
// Unlike DecodeErrCount (frame-level failures that never reach the receive
// chokepoint), a body-decode failure is counted AFTER DataMsgRecvCount — the two are
// intentionally distinct so neither double-counts the other.
func (m *ConnectionMetrics) BodyDecodeErrCount() uint64 {
	return m.bodyDecodeErr.Load()
}
```

- [ ] **Step 5: Add the inc helper**

Next to `incDecodeErr()` (~line 149):

```go
func (m *ConnectionMetrics) incBodyDecodeErr() {
	m.bodyDecodeErr.Add(1)
}
```

- [ ] **Step 6: Run test to verify it passes**

Run: `go test ./hsms/ -run TestBodyDecodeErrCount -v`
Expected: PASS.

- [ ] **Step 7: Lint & commit**

```bash
make lint
git add hsms/connection_metrics.go hsms/connection_metrics_test.go
git commit -m "feat(hsms): add BodyDecodeErrCount metric for diverted body-decode failures"
```

---

## Task 3: `DecodeErrorHandler` type, registration, and fake mirror (Gap 1)

**Model:** Opus **Effort:** high **Depends on:** none (but 4 and 5 depend on it)

Touches an exported interface (`SECS2Endpoint`) — a break for external implementers —
plus concurrency-safe registration and the in-tree fake. Opus/high for correctness of
the interface change and the RWMutex snapshot discipline.

**Files:**
- Modify: `hsms/endpoint.go` (add type + interface method)
- Modify: `hsms/session.go` (struct field + `AddDecodeErrorHandler` + `hasDecodeErrorHandlers` + `dispatchDecodeError`)
- Modify: `hsms/hsmstest/endpoint.go` (implement `AddDecodeErrorHandler` on `FakeEndpoint`)
- Test: `hsms/session_test.go`

**Interfaces:**
- Produces:
  - `type DecodeErrorHandler func(msg *DataMessage, err error, ep SECS2Endpoint)`
  - `SECS2Endpoint.AddDecodeErrorHandler(handlers ...DecodeErrorHandler)`
  - unexported `(*session).hasDecodeErrorHandlers() bool`
  - unexported `(*session).dispatchDecodeError(msg *DataMessage, err error)`

- [ ] **Step 1: Write the failing test**

Add to `hsms/session_test.go` (`package hsms`):

```go
func TestAddDecodeErrorHandler_registrationAndDispatch(t *testing.T) {
	s := newSession(1, nil, newSysBytesGen()) // rt nil is fine; we only test registration+dispatch

	if s.hasDecodeErrorHandlers() {
		t.Fatal("hasDecodeErrorHandlers() = true before any registration")
	}

	var gotErr error
	var gotMsg *DataMessage
	s.AddDecodeErrorHandler(func(msg *DataMessage, err error, _ SECS2Endpoint) {
		gotMsg, gotErr = msg, err
	})

	if !s.hasDecodeErrorHandlers() {
		t.Fatal("hasDecodeErrorHandlers() = false after registration")
	}

	want := errors.New("boom")
	dm := &DataMessage{}
	s.dispatchDecodeError(dm, want)

	if gotErr != want {
		t.Errorf("handler err = %v, want %v", gotErr, want)
	}
	if gotMsg != dm {
		t.Error("handler received the wrong message pointer")
	}
}
```

Confirm the sysBytesGen constructor name first: `grep -n "func newSysBytesGen\|sysBytesGen{" hsms/*.go` and use the actual constructor/literal.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./hsms/ -run TestAddDecodeErrorHandler_registrationAndDispatch -v`
Expected: FAIL — `s.hasDecodeErrorHandlers undefined`.

- [ ] **Step 3: Add the handler type + interface method** in `hsms/endpoint.go`

After the `DataMessageHandler` definition (~line 19):

```go
// DecodeErrorHandler is the callback for an inbound data message whose SECS-II body
// failed to decode. The header accessors (Stream/Function/WaitBit/SessionID/ID) are
// valid; the body is not — do NOT call msg.Item() expecting success. err is the
// deferred decode error (equal to msg.DecodeErr()).
//
// A DecodeErrorHandler is invoked only for PRIMARY messages (and orphan secondaries
// that miss the reply registry). A malformed reply to a synchronous SendDataMessage /
// SendSECS2Message is surfaced to that call's error return instead.
type DecodeErrorHandler func(msg *DataMessage, err error, ep SECS2Endpoint)
```

In the `SECS2Endpoint` interface, after `AddDataMessageHandler`:

```go
	// AddDecodeErrorHandler appends one or more handlers for inbound data messages
	// whose SECS-II body fails to decode. When at least one is registered, an
	// undecodable primary is delivered to these handlers instead of the normal
	// DataMessageHandlers/channel handlers. With none registered, decoding stays lazy
	// and the message routes normally (today's behavior).
	//
	// Registration is not blocking I/O and does not take a context.
	AddDecodeErrorHandler(handlers ...DecodeErrorHandler)
```

- [ ] **Step 4: Add storage + methods** in `hsms/session.go`

Add a field to the `session` struct (under `handlers`/`chans`, still guarded by `mu`):

```go
	handlers     []DataMessageHandler
	chans        []chan *DataMessage
	decodeErrHandlers []DecodeErrorHandler
```

Add the methods (near `AddDataMessageHandler`, ~line 167):

```go
// AddDecodeErrorHandler appends inbound decode-error handlers under mu.Lock.
// RouteData snapshots the slice under RLock, so registration and delivery are
// race-free.
func (s *session) AddDecodeErrorHandler(handlers ...DecodeErrorHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.decodeErrHandlers = append(s.decodeErrHandlers, handlers...)
}

// hasDecodeErrorHandlers reports whether at least one decode-error handler is
// registered. Read under RLock.
func (s *session) hasDecodeErrorHandlers() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.decodeErrHandlers) > 0
}

// dispatchDecodeError delivers (msg, err) to every registered decode-error handler.
// Handlers are snapshotted under RLock and invoked with the lock released, matching
// recvDataMsg's fan-out discipline.
func (s *session) dispatchDecodeError(msg *DataMessage, err error) {
	s.mu.RLock()
	handlers := s.decodeErrHandlers
	s.mu.RUnlock()
	for _, h := range handlers {
		h(msg, err, s)
	}
}
```

- [ ] **Step 5: Implement the method on `FakeEndpoint`** in `hsms/hsmstest/endpoint.go`

Add a field to the `FakeEndpoint` struct next to `dataHandlers`:

```go
	dataHandlers      []hsms.DataMessageHandler
	decodeErrHandlers []hsms.DecodeErrorHandler
```

Add, near the existing `AddDataMessageHandler` (find it: `grep -n "func (f \*FakeEndpoint) AddDataMessageHandler" hsms/hsmstest/endpoint.go`):

```go
// AddDecodeErrorHandler records decode-error handlers so FakeEndpoint satisfies the
// hsms.SECS2Endpoint interface. DeliverDecodeError invokes them.
func (f *FakeEndpoint) AddDecodeErrorHandler(handlers ...hsms.DecodeErrorHandler) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.decodeErrHandlers = append(f.decodeErrHandlers, handlers...)
}

// DeliverDecodeError invokes every registered DecodeErrorHandler with (msg, err, f),
// as if an undecodable message had arrived on the wire. Handlers are snapshotted under
// the lock and invoked with it released, matching Deliver's contract.
func (f *FakeEndpoint) DeliverDecodeError(msg *hsms.DataMessage, err error) {
	f.mu.Lock()
	handlers := slices.Clone(f.decodeErrHandlers)
	f.mu.Unlock()
	for _, h := range handlers {
		h(msg, err, f)
	}
}
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./hsms/ -run TestAddDecodeErrorHandler_registrationAndDispatch -v`
Then confirm the fake still compiles against the interface:
Run: `go build ./hsms/... && go vet ./hsms/hsmstest/`
Expected: PASS / clean. The `var _ hsms.SECS2Endpoint = (*FakeEndpoint)(nil)` assertion
(`endpoint.go:49`) now requires the new method — build fails if it's missing.

- [ ] **Step 7: Lint & commit**

```bash
make lint
git add hsms/endpoint.go hsms/session.go hsms/hsmstest/endpoint.go hsms/session_test.go
git commit -m "feat(hsms): add DecodeErrorHandler registration to SECS2Endpoint"
```

---

## Task 4: Primary-path eager-decode divert in `RouteData` (Gap 1)

**Model:** Opus **Effort:** xhigh **Depends on:** Task 1, Task 2, Task 3

The correctness core of Gap 1: concurrency-sensitive routing on the inbound hot path,
interacting with the decode `sync.Once` and metrics. Highest effort.

**Files:**
- Modify: `hsms/connection_runtime.go` (`RouteData`, ~10-15)
- Test: `hsms/connection_runtime_test.go` (or `hsms/session_test.go`)

**Interfaces:**
- Consumes: `(*session).hasDecodeErrorHandlers()`, `(*session).dispatchDecodeError()` (Task 3); `(*ConnectionMetrics).incBodyDecodeErr()` (Task 2); `hsmstest.MalformedDataMessage` (Task 1); `(*DataMessage).DecodeErr()`.
- Produces: divert behavior — no new exported symbol.

- [ ] **Step 1: Write the failing test**

Add to `hsms/connection_runtime_test.go` (`package hsms`). This drives `RouteData`
directly on a real connection built by the test harness. First find the harness
constructor: `grep -n "func newTestConnection\|func newHarness\|connectFake" hsms/*_test.go | head`.
Use it to obtain a `*connection` `c` whose embedded session you can register on:

```go
func TestRouteData_divertsUndecodableWhenHandlerRegistered(t *testing.T) {
	c := newTestConnection(t) // harness helper — see existing *_test.go

	var normalCalled, decodeErrCalled bool
	c.AddDataMessageHandler(func(_ *DataMessage, _ SECS2Endpoint) { normalCalled = true })
	c.AddDecodeErrorHandler(func(_ *DataMessage, _ error, _ SECS2Endpoint) { decodeErrCalled = true })

	bad := hsmstest.MalformedDataMessage(6, 11, false)
	_ = c.RouteData(bad)

	if normalCalled {
		t.Error("normal handler was called for an undecodable message")
	}
	if !decodeErrCalled {
		t.Error("decode-error handler was NOT called")
	}
	if got := c.Metrics().BodyDecodeErrCount(); got != 1 {
		t.Errorf("BodyDecodeErrCount() = %d, want 1", got)
	}
}

func TestRouteData_normalRoutingWhenNoDecodeHandler(t *testing.T) {
	c := newTestConnection(t)

	var normalCalled bool
	c.AddDataMessageHandler(func(_ *DataMessage, _ SECS2Endpoint) { normalCalled = true })

	bad := hsmstest.MalformedDataMessage(6, 11, false)
	_ = c.RouteData(bad)

	if !normalCalled {
		t.Error("with no decode-error handler, the message must route normally (lazy)")
	}
	if got := c.Metrics().BodyDecodeErrCount(); got != 0 {
		t.Errorf("BodyDecodeErrCount() = %d, want 0", got)
	}
}
```

Import `"github.com/arloliu/go-secs/v2/hsms/hsmstest"` — but note `hsmstest` imports
`hsms`, so this test must live in an **external** test package to avoid an import
cycle. If `connection_runtime_test.go` is `package hsms`, put these two tests in a new
`hsms/decode_divert_ext_test.go` with `package hsms_test` and use only exported API
(`c` must be reachable as an `hsms.SECS2Endpoint` + `*hsms.Connection` with `Metrics()`
and `RouteData`). If `RouteData` is unexported/not reachable externally, instead
deliver through the exported inbound path the harness exposes (e.g. `DeliverOwnedFrame`
with `bad.ToBytes()`), which funnels into `RouteData`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./hsms/ -run 'TestRouteData_diverts|TestRouteData_normalRouting' -v`
Expected: FAIL — the malformed message reaches the normal handler / metric stays 0.

- [ ] **Step 3: Implement the divert** in `hsms/connection_runtime.go`

Replace the body of `RouteData` (currently `c.recvDataMsg(msg); return nil`):

```go
func (c *connection) RouteData(msg *DataMessage) error {
	// Gap 1: when decode-error handling is enabled, force the lazy body decode here
	// (where c.metrics is in scope) and divert an undecodable message to the
	// decode-error handlers instead of the normal fan-out. hasDecodeErrorHandlers and
	// dispatchDecodeError are promoted from the embedded session.
	if c.hasDecodeErrorHandlers() {
		if derr := msg.DecodeErr(); derr != nil { // fires the decode sync.Once, caches result
			c.metrics.incBodyDecodeErr()
			c.dispatchDecodeError(msg, derr)
			return nil // do NOT fan out to normal func/channel handlers
		}
	}

	c.recvDataMsg(msg) // promoted from the embedded session
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./hsms/ -run 'TestRouteData_diverts|TestRouteData_normalRouting' -v`
Expected: PASS. On a successful decode, note the cached result means a later handler
`Item()` call does not re-decode (`sync.Once`).

- [ ] **Step 5: Teeth check**

Temporarily change the divert `return nil` to fall through to `c.recvDataMsg(msg)` and
confirm `TestRouteData_divertsUndecodableWhenHandlerRegistered` FAILS
(`normalCalled == true`). Revert.

- [ ] **Step 6: Full hsms suite (no fuzz) + race**

Run: `go test ./hsms/ -race -skip '^Fuzz'`
Expected: PASS — confirms no data race on the new `decodeErrHandlers` snapshot under
concurrent registration/delivery.

- [ ] **Step 7: Lint & commit**

```bash
make lint
git add hsms/connection_runtime.go hsms/*_test.go
git commit -m "feat(hsms): divert undecodable primaries to DecodeErrorHandler in RouteData"
```

---

## Task 5: Reply-path decode check in send wrappers (Gap 1)

**Model:** Opus **Effort:** high **Depends on:** Task 1, Task 3 (shares `session.go`)

The one deliberate behavior change: the send wrappers must return `(dm, err)` rather
than `(nil, err)` on an undecodable reply. Correctness-sensitive; Opus/high.

**Files:**
- Modify: `hsms/session.go` (`SendDataMessage` ~69-84, `SendSECS2Message` ~100-117)
- Test: `hsms/session_test.go`

**Interfaces:**
- Consumes: `(*DataMessage).DecodeErr()`; `hsmstest.MalformedDataMessage` (Task 1).
- Produces: changed return behavior of `SendDataMessage`/`SendSECS2Message` on a malformed reply — same signatures.

- [ ] **Step 1: Write the failing test**

This needs a `TransportRuntime` whose `WriteMessage` returns a malformed reply. Reuse
the harness mock (`grep -n "WriteMessage" hsms/harness_mock_transport_test.go`). If it
supports scripting a reply, script `hsmstest.MalformedDataMessage(...)`; otherwise add
a minimal stub `rt` implementing `TransportRuntime` whose `WriteMessage` returns
`(malformed, nil)`. Then (`package hsms_test` to use `hsmstest`):

```go
func TestSendDataMessage_malformedReplyReturnsErrAndMsg(t *testing.T) {
	bad := hsmstest.MalformedDataMessage(1, 14, false)
	ep := newEndpointWithScriptedReply(t, bad) // harness: WriteMessage returns bad, nil

	dm, err := ep.SendDataMessage(context.Background(), 1, 13, true, secs2.NewListItem())
	if err == nil {
		t.Fatal("SendDataMessage returned nil error for an undecodable reply")
	}
	if dm == nil {
		t.Fatal("SendDataMessage returned nil message; must return the reply alongside the error")
	}
	if dm.Stream() != 1 || dm.Function() != 14 {
		t.Errorf("returned msg = S%dF%d, want S1F14", dm.Stream(), dm.Function())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./hsms/ -run TestSendDataMessage_malformedReply -v`
Expected: FAIL — current code returns `(nil, nil)` (the malformed reply is delivered as
a clean success).

- [ ] **Step 3: Add the decode check** in `SendDataMessage` (`hsms/session.go`)

Replace the tail (from `dm, _ := reply.(*DataMessage)` to `return dm, nil`):

```go
	// nil interface → (nil, false); typed-nil is returned as nil.
	dm, _ := reply.(*DataMessage)

	// Gap 1 reply path: a reply whose body fails to decode must not unblock the caller
	// as a clean success. Surface the decode error, but return the message alongside it
	// (non-destructive: the header stays available to the caller).
	if dm != nil {
		if derr := dm.DecodeErr(); derr != nil {
			return dm, derr
		}
	}

	return dm, nil
```

- [ ] **Step 4: Mirror the change in `SendSECS2Message`**

Replace its tail (`dataReply, _ := reply.(*DataMessage)` … `return dataReply, nil`):

```go
	dataReply, _ := reply.(*DataMessage)

	if dataReply != nil {
		if derr := dataReply.DecodeErr(); derr != nil {
			return dataReply, derr
		}
	}

	return dataReply, nil
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./hsms/ -run 'TestSendDataMessage_malformedReply|TestSendSECS2' -v`
Expected: PASS. A well-formed reply still returns `(dm, nil)`; a timeout/control-message
reply is unaffected (`dm == nil` → skips the block).

- [ ] **Step 6: Teeth check**

Temporarily revert Step 3's block to `return dm, nil` and confirm the test FAILS
(`err == nil`). Revert.

- [ ] **Step 7: Lint & commit**

```bash
make lint
git add hsms/session.go hsms/session_test.go
git commit -m "fix(hsms): surface undecodable-reply error from SendDataMessage/SendSECS2Message"
```

---

## Task 6: `DataMessageCodec` read delegators (Gap 4)

**Model:** Sonnet **Effort:** medium **Depends on:** none (independent file)

Well-specified additive delegation with careful nil semantics.

**Files:**
- Modify: `hsms/data_msg_codec.go`
- Test: `hsms/data_msg_codec_test.go`

**Interfaces:**
- Produces (all on `*DataMessageCodec`): `Stream() uint8`, `Function() uint8`, `WaitBit() bool`, `SessionID() uint16`, `ID() uint32`, `SystemBytes() [4]byte`, `HeaderBytes() [10]byte`, `ToBytes() []byte`, `Type() MsgType`, `DecodeErr() error`, `Item() (secs2.Item, error)`, `ToDataMessage() (*DataMessage, bool)`.

- [ ] **Step 1: Write the failing test**

Add to `hsms/data_msg_codec_test.go` (`package hsms`):

```go
func TestDataMessageCodec_readDelegators(t *testing.T) {
	dm, err := NewDataMessage(6, 11, true, 42, [4]byte{0, 0, 0, 7}, secs2.NewBinaryItem([]byte{1}))
	if err != nil {
		t.Fatal(err)
	}
	c := dm.Codec()

	if c.Stream() != 6 || c.Function() != 11 || !c.WaitBit() {
		t.Errorf("header delegation mismatch: S%dF%d W=%v", c.Stream(), c.Function(), c.WaitBit())
	}
	if c.SessionID() != 42 {
		t.Errorf("SessionID() = %d, want 42", c.SessionID())
	}
	if _, derr := c.Item(); derr != nil {
		t.Errorf("Item() on a valid codec returned error: %v", derr)
	}
	if got, ok := c.ToDataMessage(); !ok || got != dm {
		t.Error("ToDataMessage() did not return the wrapped message")
	}
}

func TestDataMessageCodec_nilMessageIsSafe(t *testing.T) {
	c := &DataMessageCodec{Message: nil}

	if c.Stream() != 0 || c.SessionID() != 0 { // zero-value reads, no panic
		t.Error("nil-Message scalar reads should return zero values")
	}
	if _, err := c.Item(); err != ErrNilMessage {
		t.Errorf("Item() on nil Message = %v, want ErrNilMessage", err)
	}
	if err := c.DecodeErr(); err != ErrNilMessage {
		t.Errorf("DecodeErr() on nil Message = %v, want ErrNilMessage", err)
	}
	if _, ok := c.ToDataMessage(); ok {
		t.Error("ToDataMessage() on nil Message should return ok=false")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./hsms/ -run TestDataMessageCodec_ -v`
Expected: FAIL — `c.Stream undefined`.

- [ ] **Step 3: Add the delegators** in `hsms/data_msg_codec.go` (after `Codec()`):

```go
// The following read-only delegators let a *DataMessageCodec be inspected without
// unwrapping Message, so a consumer that only reads never falls through a type switch
// to "unknown". Scalar/byte reads on a nil Message return the zero value (panic-free);
// Item and DecodeErr report ErrNilMessage rather than a misleading nil.

// Stream returns the wrapped message's stream, or 0 if Message is nil.
func (c *DataMessageCodec) Stream() uint8 {
	if c.Message == nil {
		return 0
	}
	return c.Message.Stream()
}

// Function returns the wrapped message's function, or 0 if Message is nil.
func (c *DataMessageCodec) Function() uint8 {
	if c.Message == nil {
		return 0
	}
	return c.Message.Function()
}

// WaitBit returns the wrapped message's W-bit, or false if Message is nil.
func (c *DataMessageCodec) WaitBit() bool {
	if c.Message == nil {
		return false
	}
	return c.Message.WaitBit()
}

// SessionID returns the wrapped message's session ID, or 0 if Message is nil.
func (c *DataMessageCodec) SessionID() uint16 {
	if c.Message == nil {
		return 0
	}
	return c.Message.SessionID()
}

// ID returns the wrapped message's ID (system bytes), or 0 if Message is nil.
func (c *DataMessageCodec) ID() uint32 {
	if c.Message == nil {
		return 0
	}
	return c.Message.ID()
}

// SystemBytes returns the wrapped message's System Bytes, or the zero array if Message is nil.
func (c *DataMessageCodec) SystemBytes() [4]byte {
	if c.Message == nil {
		return [4]byte{}
	}
	return c.Message.SystemBytes()
}

// HeaderBytes returns the wrapped message's 10-byte header, or the zero array if Message is nil.
func (c *DataMessageCodec) HeaderBytes() [10]byte {
	if c.Message == nil {
		return [10]byte{}
	}
	return c.Message.HeaderBytes()
}

// ToBytes returns the wrapped message's wire bytes, or nil if Message is nil.
func (c *DataMessageCodec) ToBytes() []byte {
	if c.Message == nil {
		return nil
	}
	return c.Message.ToBytes()
}

// Type returns the wrapped message's HSMS message type, or DataMsgType if Message is nil.
func (c *DataMessageCodec) Type() MsgType {
	if c.Message == nil {
		return DataMsgType
	}
	return c.Message.Type()
}

// DecodeErr returns the wrapped message's deferred decode error, or ErrNilMessage if
// Message is nil.
func (c *DataMessageCodec) DecodeErr() error {
	if c.Message == nil {
		return ErrNilMessage
	}
	return c.Message.DecodeErr()
}

// Item returns the wrapped message's decoded item, or ErrNilMessage if Message is nil
// (never (nil, nil), which would hide an absent message).
func (c *DataMessageCodec) Item() (secs2.Item, error) {
	if c.Message == nil {
		return nil, ErrNilMessage
	}
	return c.Message.Item()
}

// ToDataMessage returns the wrapped message and true, or (nil, false) if Message is nil.
// This is the zero-safe unwrap probe.
func (c *DataMessageCodec) ToDataMessage() (*DataMessage, bool) {
	if c.Message == nil {
		return nil, false
	}
	return c.Message, true
}
```

Add `"github.com/arloliu/go-secs/v2/secs2"` to the imports (for `secs2.Item`).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./hsms/ -run TestDataMessageCodec_ -v`
Expected: PASS. Confirm the exact accessor names/return types on `*DataMessage` first
if any mismatch: `grep -nE "func \(msg \*DataMessage\) (Stream|Function|WaitBit|SessionID|ID|Item|DecodeErr)\(" hsms/data_msg.go`.

- [ ] **Step 5: Lint & commit**

```bash
make lint
git add hsms/data_msg_codec.go hsms/data_msg_codec_test.go
git commit -m "feat(hsms): add read delegators and ToDataMessage to DataMessageCodec"
```

---

## Task 7: `Is*()`/`To*()` deferred-error doc callouts (Gap 5)

**Model:** Sonnet **Effort:** low **Depends on:** none (independent)

Doc-only. No new symbol — steer callers to the existing, aggregate-aware `Error()`.

**Files:**
- Modify: Godoc on the typed accessors/predicates in `secs2/int.go`, `secs2/uint.go`,
  `secs2/float.go`, `secs2/bool.go`, `secs2/ascii.go`, `secs2/binary.go` (and any other
  `To*`/`Is*` sites — enumerate with the grep below).
- Test: `secs2/example_error_test.go` (runnable doc example)

**Interfaces:** none (documentation + one example test).

- [ ] **Step 1: Enumerate the accessor/predicate sites**

Run: `grep -rnE "func \(item \*[A-Za-z]+Item\) (To[A-Za-z]+|Is[A-Za-z]+)\(" secs2/*.go | grep -v _test`
Record the list; each gets a one-line Godoc callout.

- [ ] **Step 2: Add a callout to each `To*()` accessor**

On each `To*()` method's Godoc, append (adjust the "returns" clause to match the
method), e.g. on `(*IntItem).ToInt`:

```go
// ToInt returns the item's values as []int64.
//
// It returns the item's deferred error (see Error) when the item was constructed with
// one — a passing Is* predicate does NOT imply a nil error here. Always check the
// returned error; do not discard it via `v, _ := item.ToInt()`.
```

- [ ] **Step 3: Add a callout to each `Is*()` predicate**

On each `Is*()` method's Godoc, append:

```go
// IsInt8 reports whether the item's declared type is a 1-byte signed integer.
//
// It reflects the DECLARED type only and does not consult the item's deferred error:
// a true result does not imply the item is usable. Gate value extraction on Error()
// (or the To* accessor's returned error), not on Is* alone.
```

- [ ] **Step 4: Add a runnable example demonstrating the safe pattern**

Create `secs2/example_error_test.go`:

```go
package secs2_test

import (
	"fmt"

	"github.com/arloliu/go-secs/v2/secs2"
)

// ExampleItem_deferredError shows the safe pattern: check the accessor's error (or
// Error()) rather than relying on a type predicate.
func ExampleItem_deferredError() {
	item := secs2.NewIntItem(1, 42)

	// Correct: the accessor's error is checked, not discarded.
	if v, err := item.ToInt(); err == nil {
		fmt.Println(v[0])
	}

	// For a list, Error() aggregates child errors even when the list's own error is nil.
	_ = item.Error()
	// Output: 42
}
```

Confirm `secs2.NewIntItem`'s real signature first (`grep -n "func NewIntItem" secs2/int.go`)
and adjust the constructor call; the example must compile and print `42`.

- [ ] **Step 5: Run the example**

Run: `go test ./secs2/ -run ExampleItem_deferredError -v`
Expected: PASS.

- [ ] **Step 6: Lint & commit**

```bash
make lint
git add secs2/*.go
git commit -m "docs(secs2): warn that Is*/To* do not surface an item's deferred error"
```

---

## Task 8: `hsmsss.WithConnectTimeout` (Gap 2)

**Model:** Sonnet **Effort:** medium **Depends on:** none (independent package)

**Files:**
- Modify: `hsmsss/config.go` (config field + option, near `WithDialer` ~116-135; default `dial` ~62)
- Modify: `hsmsss/transport_active.go` (dial site ~109)
- Test: `hsmsss/config_test.go` and/or `hsmsss/transport_active_test.go`

**Interfaces:**
- Produces: `func WithConnectTimeout(d time.Duration) Option`.

- [ ] **Step 1: Write the failing test**

Add to `hsmsss/config_test.go`:

```go
func TestWithConnectTimeout(t *testing.T) {
	cfg := newTestConfig(t) // existing helper; else build a *Config via the package's constructor
	if err := WithConnectTimeout(3 * time.Second)(cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.connectTimeout != 3*time.Second {
		t.Errorf("connectTimeout = %v, want 3s", cfg.connectTimeout)
	}
	if err := WithConnectTimeout(-1)(cfg); err == nil {
		t.Error("negative timeout must be rejected")
	}
}
```

Confirm how a `*Config` is obtained in existing tests: `grep -n "Config{" hsmsss/*_test.go | head`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./hsmsss/ -run TestWithConnectTimeout -v`
Expected: FAIL — `cfg.connectTimeout undefined` / `WithConnectTimeout undefined`.

- [ ] **Step 3: Add the config field** in `hsmsss/config.go`

In the `Config` struct, near `dial`:

```go
	dial           DialFunc
	connectTimeout time.Duration // 0 = unbounded (OS default); >0 bounds each active dial attempt
```

- [ ] **Step 4: Add the option** (after `WithDialer`):

```go
// WithConnectTimeout bounds each active-role dial attempt to d. The default (0) leaves
// the dial unbounded, so a dial to an unreachable peer blocks for the OS connect
// timeout (~2 minutes). A positive d wraps every dial attempt — including background
// reconnect attempts — in a per-attempt deadline.
//
// This affects the active (dialing) role only. It composes with WithDialer: the
// deadline wraps whatever DialFunc is configured. A negative d is a configuration error.
func WithConnectTimeout(d time.Duration) Option {
	return func(c *Config) error {
		if d < 0 {
			return errors.New("WithConnectTimeout: timeout must not be negative")
		}
		c.connectTimeout = d
		return nil
	}
}
```

Ensure `"time"` is imported.

- [ ] **Step 5: Wrap the dial site** in `hsmsss/transport_active.go` (~109)

Replace `conn, err := t.cfg.dial(ctx, "tcp", addr)` with:

```go
	dialCtx := ctx
	if t.cfg.connectTimeout > 0 {
		var cancel context.CancelFunc
		dialCtx, cancel = context.WithTimeout(ctx, t.cfg.connectTimeout)
		defer cancel()
	}
	conn, err := t.cfg.dial(dialCtx, "tcp", addr)
```

If this dial runs in a loop (per-attempt), do not use `defer` inside the loop — call
`cancel()` immediately after the dial returns instead:

```go
	dialCtx, cancel := context.WithTimeout(ctx, t.cfg.connectTimeout) // guard: only when >0
	conn, err := t.cfg.dial(dialCtx, "tcp", addr)
	cancel()
```

Inspect the surrounding code (`sed -n '90,120p' hsmsss/transport_active.go`) and pick
the form that guarantees `cancel()` runs on every path with no per-iteration leak.

- [ ] **Step 6: Add a dial-timeout behavior test**

```go
func TestConnectTimeout_boundsDial(t *testing.T) {
	// 10.255.255.1 is a non-routable address that black-holes the SYN.
	slowDial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", "10.255.255.1:5000")
	}
	// Build an active transport with WithDialer(slowDial) + WithConnectTimeout(50ms),
	// invoke its dial path, and assert it returns within, say, 500ms with a
	// deadline-exceeded / timeout error rather than hanging.
}
```

Fill in the transport construction from the nearest existing active-transport test
(`grep -n "startActive\|newActiveTransport\|WithDialer" hsmsss/*_test.go | head`).

- [ ] **Step 7: Run tests to verify they pass**

Run: `go test ./hsmsss/ -run 'TestWithConnectTimeout|TestConnectTimeout_boundsDial' -v`
Expected: PASS (the bound test returns well under the OS timeout).

- [ ] **Step 8: Lint & commit**

```bash
make lint
git add hsmsss/config.go hsmsss/transport_active.go hsmsss/*_test.go
git commit -m "feat(hsmsss): add WithConnectTimeout to bound active dial attempts"
```

---

## Task 9: `secs1.WithConnectTimeout` (Gap 2)

**Model:** Sonnet **Effort:** medium **Depends on:** none (parallel-safe with Task 8; different package)

Mirror of Task 8 in `secs1`. Repeat the structure against `secs1`'s own files (the code
differs only in package/paths), so it is spelled out fully here.

**Files:**
- Modify: `secs1/config.go` (config field + option, near `WithDialer` ~284; default `dial` ~85)
- Modify: `secs1/transport.go` (dial site ~250)
- Test: `secs1/config_test.go` and/or `secs1/transport_test.go`

**Interfaces:**
- Produces: `func WithConnectTimeout(d time.Duration) Option`.

- [ ] **Step 1: Write the failing test**

```go
func TestWithConnectTimeout(t *testing.T) {
	cfg := newTestConfig(t) // existing helper; else construct a *Config as other tests do
	if err := WithConnectTimeout(3 * time.Second)(cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.connectTimeout != 3*time.Second {
		t.Errorf("connectTimeout = %v, want 3s", cfg.connectTimeout)
	}
	if err := WithConnectTimeout(-1)(cfg); err == nil {
		t.Error("negative timeout must be rejected")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./secs1/ -run TestWithConnectTimeout -v`
Expected: FAIL — undefined field/option.

- [ ] **Step 3: Add the config field** in `secs1/config.go` (near `dial`, ~35):

```go
	dial           DialFunc
	connectTimeout time.Duration // 0 = unbounded; >0 bounds each active dial attempt
```

- [ ] **Step 4: Add the option** (after `WithDialer`, ~284):

```go
// WithConnectTimeout bounds each active-role dial attempt to d. The default (0) leaves
// the dial unbounded (OS connect timeout, ~2 minutes). A positive d wraps every dial
// attempt — including background reconnect attempts — in a per-attempt deadline.
//
// It composes with WithDialer (the deadline wraps whatever DialFunc is configured). A
// negative d is a configuration error.
func WithConnectTimeout(d time.Duration) Option {
	return func(c *Config) error {
		if d < 0 {
			return errors.New("WithConnectTimeout: timeout must not be negative")
		}
		c.connectTimeout = d
		return nil
	}
}
```

Ensure `"time"` and `"errors"` are imported.

- [ ] **Step 5: Wrap the dial site** in `secs1/transport.go` (~250)

Replace `conn, err := t.cfg.dial(engineCtx, "tcp", addr)` with the guarded, leak-free form:

```go
	dialCtx := engineCtx
	if t.cfg.connectTimeout > 0 {
		var cancel context.CancelFunc
		dialCtx, cancel = context.WithTimeout(engineCtx, t.cfg.connectTimeout)
		defer cancel()
	}
	conn, err := t.cfg.dial(dialCtx, "tcp", addr)
```

Inspect `sed -n '235,260p' secs1/transport.go`; if this is inside a retry loop, use the
immediate-`cancel()` form (as in Task 8 Step 5) rather than `defer`.

- [ ] **Step 6: Add a dial-timeout behavior test** (mirror Task 8 Step 6 against `secs1`'s active-transport test helpers).

- [ ] **Step 7: Run tests to verify they pass**

Run: `go test ./secs1/ -run 'TestWithConnectTimeout|TestConnectTimeout' -v`
Expected: PASS.

- [ ] **Step 8: Lint & commit**

```bash
make lint
git add secs1/config.go secs1/transport.go secs1/*_test.go
git commit -m "feat(secs1): add WithConnectTimeout to bound active dial attempts"
```

---

## Task 10: `secs1/doc.go` Gap 3 note + doc-drift fixes (Gap 3 + Completeness)

**Model:** Sonnet **Effort:** low **Depends on:** none (independent)

Doc-only. Fixes a doc that now *contradicts* shipped code, plus two additions.

**Files:**
- Modify: `secs1/doc.go` (~25-28 WriteTimeout paragraph; ~59-68 metrics list; add S9Fx note)

**Interfaces:** none.

- [ ] **Step 1: Fix the WriteTimeout contradiction** (~25-28)

The paragraph claims runtime `UpdateConfigOptions` does NOT intercept
`hsms.WithWriteTimeout`, but `secs1/new.go:92-95` now force-appends `WithWriteTimeout(0)`
on that path. Rewrite it to state the shipped behavior:

```go
// WithWriteTimeout is forced to 0 (unbounded) by secs1: a blocking framed write must
// not be capped by a wall-clock deadline. This is enforced both at construction and at
// runtime — UpdateConfigOptions re-appends WithWriteTimeout(0) after caller options
// (see New), so a WithWriteTimeout supplied at runtime is intercepted and neutralized
// rather than taking effect.
```

Confirm the exact current wording first: `sed -n '20,32p' secs1/doc.go`, and replace the
stale sentence(s) in place.

- [ ] **Step 2: Add the missing assembly metrics** to the metrics list (~59-68)

After the existing block-assembly metric bullets, add:

```go
//   - DeviceIDMismatchCount: multi-block reassembly dropped a block whose device ID
//     did not match the open partial (auto S9F1 for the equipment role).
//   - BlockNumberMismatchCount: a block arrived out of sequence for the open partial.
//   - InvalidFirstBlockCount: a first block was neither block 1 nor a valid single-block
//     message (block/header violations auto-reply S9F7 for the equipment role).
```

Match the surrounding comment style (`sed -n '55,70p' secs1/doc.go`).

- [ ] **Step 3: Add the Gap 3 equipment-role S9Fx note**

Where the doc describes assembler validation / S9Fx (near the metrics or in the role
section), add one line:

```go
// Assembler-violation notifications (S9F1 for a device-ID mismatch, S9F7 for
// block/header/first-block violations) are sent for the EQUIPMENT role only and are
// not separately configurable — there is no ValidateDataMessage toggle in v2.
```

- [ ] **Step 4: Verify it builds and the doc renders**

Run: `go build ./secs1/ && go doc ./secs1/ | head -40`
Expected: clean build; the doc text reflects the edits.

- [ ] **Step 5: Lint & commit**

```bash
make lint
git add secs1/doc.go
git commit -m "docs(secs1): fix WriteTimeout-intercept contradiction; document assembly metrics and equipment-only S9Fx"
```

---

## Final Verification (after all tasks)

- [ ] Full suite, no fuzz: `go test ./... -skip '^Fuzz'` → PASS
- [ ] Race on the touched packages: `go test ./hsms/ ./hsmsss/ ./secs1/ ./secs2/ -race -skip '^Fuzz'` → PASS
- [ ] `make lint` clean
- [ ] `go doc ./hsms/ | grep -i DecodeError` shows the new handler; `go doc ./hsmsss/ | grep ConnectTimeout` and `go doc ./secs1/ | grep ConnectTimeout` show the new options
- [ ] Spec cross-check: each of Gaps 1–6 + both doc-drift items maps to a committed task
- [ ] Tag `v2.0.0-rc5` on `v2` (release step — do only when explicitly asked)

## Model / Effort Summary

| Task | Gap | Model | Effort | Rationale |
|------|-----|-------|--------|-----------|
| 1 | 6 | Sonnet | high | Wire-format forging; unblocks all Gap 1 tests |
| 2 | 1 | Sonnet | medium | Mechanical counter mirroring existing pattern |
| 3 | 1 | Opus | high | Exported-interface change + concurrency-safe registration + fake mirror |
| 4 | 1 | Opus | xhigh | Inbound hot-path routing correctness; `sync.Once`/metric interaction |
| 5 | 1 | Opus | high | The one deliberate behavior change; reply correctness |
| 6 | 4 | Sonnet | medium | Additive delegation with careful nil semantics |
| 7 | 5 | Sonnet | low | Doc-only + one example test |
| 8 | 2 | Sonnet | medium | Additive option + dial wrap |
| 9 | 2 | Sonnet | medium | Mirror of Task 8 in secs1 |
| 10 | 3 + drift | Sonnet | low | Doc-only |

Opus is reserved for the three correctness/concurrency/interface-critical `hsms` tasks
(3, 4, 5); everything else is well-specified additive or doc work suited to Sonnet, with
effort scaled to risk (xhigh for the inbound routing core, low for pure docs).
