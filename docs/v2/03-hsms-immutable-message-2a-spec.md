# Sub-project 2a — hsms immutable message core (design / spec)

**Status:** design approved 2026-06-29, ready for implementation plan.
**Depends on:** sub-project 1 (`docs/v2/02-secs2-immutable-item-spec.md`) — immutable `secs2.Item`
(`AppendTo`/`EncodedLen`/`ToBytes`, copy/iterator accessors, no `Clone`/`Free`) and
`secs2.Decode([]byte) (Item, error)` (owns its bytes). Proposal decisions D1–D8
(`docs/v2/00-v2-proposal.md` §4–§5); bake-off verdicts (`docs/v2/01-bakeoff-results.md`).
**Module:** `github.com/arloliu/go-secs/v2`, branch `v2`, Go floor `1.26.0`.

---

## 1. Goal

Replace the v1 mutable, pooled hsms message types with an **immutable** message model: a small shared
`Message` interface, a concrete immutable `*DataMessage` (hybrid-by-provenance body) and
`*ControlMessage`, value-returning envelope edits (`With*`) and validated structural edits
(`Derive().Build()`), and an `internal/wire.Body` zero-copy bridge — delivering a complete drop-in for
the HSMS-SS send path. GC owns lifetime; no `Clone`/`Free`/pooling in any public signature.

## 2. Scope (sub-project 2a — the keystone)

**In scope:** the `Message` interface; immutable `*DataMessage` (incl. the hybrid-by-provenance
`rawFrame`/`tree` internal body + `sync.Once` lazy decode and lazy encode) and `*ControlMessage`;
construction (factories + `Derive().Build()`); the error model (`Item()`/`DecodeErr()`/`Error()`); HSMS
decode — **both** the public copying `DecodeHSMSMessage([]byte)` **and** the internal **zero-copy**
decode entry that retains a caller-owned per-message buffer (eliminating the decode-time copy on the
recv hot path) — consuming `secs2.Decode`; HSMS encode (`ToBytes`); and the `internal/wire.Body` handle
(exposing `AppendTo`/`Len`/`Chunk`/`Buffers` for 2b/2c to consume). 2a builds and standalone-tests the
zero-copy entry; the live `hsmsss` recv wiring lands when `hsmsss` is rewritten (its v1 contract —
`make([]byte, msgLen)` per message — already matches).

**Out of scope (later):**
- **2b** — SECS-I block chunk interface (`wire.Chunk`, immutable `secs1.Block`, splitter). 2a only
  *defines* `wire.Body.Chunk(off,n)` so 2b can build on it; it does not implement SECS-I block logic.
- **2c** — vectored (writev/`net.Buffers`) send. 2a only *defines* `wire.Body.Buffers()` for 2c.
- Connection/state/transport rearchitecture (D4, deferred).
- `gem` and the v1→v2 codemod (sub-project 7).

## 3. Decisions adopted

| Topic | Decision | Source |
|-------|----------|--------|
| Envelope | concrete **`*DataMessage`** / `*ControlMessage` (pointer); shared narrow `Message` interface | D2/bake-off V4-V5; Q1 |
| Body representation | **hybrid-by-provenance**, internal to `*DataMessage`: decoded → raw-frame view over the owned recv buffer `rawBody` (header `rawBody[0:10]`, body `rawBody[10:]` zero-copy; the 4-byte HSMS length is NOT in `rawBody` — recomputed on encode); constructed → item-tree + small header value | D2/V2; bake-off |
| Header/body split | at the **message level** (message owns a 10-byte header value + a body representation); `secs2.Item` stays simple | D3 |
| Memoization | **`sync.Once`** for lazy decode (decoded path) and lazy encode (constructed path) | bake-off; D2 |
| Public body access | copy-only: `AppendBodyTo(dst)`, `BodyLen()`, `HeaderBytes()`, `ToBytes()`. **No public `WriteBodyTo` / no `io.Writer`** | §5.A/5.C |
| Zero-copy bridge | **`internal/wire.Body`** handles-only — `AppendTo`/`Len`/`Chunk`/`Buffers`; **never a bare `[]byte`** | §5.C; Q4 |
| Mutation API | `With*` (always-valid: session id, system bytes) returns a new message; validity-sensitive (W-bit, stream, function, body) via `Derive()…Build() (msg, error)` | D6; Q2 |
| Build validation | `Build()` errors iff (1) `body.Error() != nil` (recursive **aggregate**, SP1 contract), (2) W=1 on a reply / even-function message (E37 §8.3.3.3), or (3) stream > 127 / function overflow. `With*` never validates. | SP1 contract + E37; Q3 |
| Error model | `Item() (secs2.Item, error)` (lazy-decode error, positional); `DecodeErr()` cached convenience; protocol `Error()` (Reject) **distinct**; no `errors.Join` accumulation | D5; Q6 |
| Lifetime | GC-owned; no `Clone`/`Free`/`usePool`. Retain only via owned immutable `wire.Body`; never mutate body bytes | D7; §5.C |
| Decode ownership | **Two entries: public `DecodeHSMSMessage([]byte)` COPIES** the input to own it (D7-safe, no caller ownership contract; not the hot path). The **internal zero-copy** entry (an **unexported** in-repo function — no internal type in any exported signature; external modules cannot reach it) **retains** a caller-owned per-message buffer with no copy. 2a proves it via an in-package alias test; the **cross-package recv bridge is designed with the recv rewrite** (later), which already allocates a fresh GC-owned `make([]byte, msgLen)`. Adopts §5.F **option (b)** (per-message GC-owned read buffer, not pooled) as the default; sub-project 4 may still benchmark (a) pool+copy and switch only if it wins. | §5.F(b); D7; investigation 2026-06-29 |

## 4. Public API

```go
package hsms

// MsgType distinguishes the wire message kind (Data vs the control STypes).
type MsgType uint8 // Data, SelectReq, SelectRsp, DeselectReq, DeselectRsp, LinktestReq, LinktestRsp, RejectReq, SeparateReq

// Message is the small surface common to all HSMS messages. The 10-byte header is
// protocol-neutral (shared with SECS-I); ToBytes is HSMS-framed.
type Message interface {
	Type() MsgType
	SessionID() uint16
	SystemBytes() [4]byte    // value, never an alias
	HeaderBytes() [10]byte   // the 10-byte HSMS/SECS-II header (no length prefix), value
	ToBytes() []byte         // full HSMS wire frame: 4-byte length + 10-byte header + body
	Error() error            // protocol-level error (e.g. carried by Reject.req); NOT decode error
}

// DataMessage is an immutable SECS-II data message (SType 0). Safe for concurrent use and fan-out.
type DataMessage struct{ /* unexported: header [10]byte value + body representation (hybrid) */ }

func (m *DataMessage) Type() MsgType            // == Data
func (m *DataMessage) Stream() uint8
func (m *DataMessage) Function() uint8
func (m *DataMessage) WaitBit() bool
func (m *DataMessage) SessionID() uint16
func (m *DataMessage) SystemBytes() [4]byte
func (m *DataMessage) HeaderBytes() [10]byte

// Item lazily decodes (decoded path) or returns (constructed path) the body item. A header-only /
// empty body returns (NewEmptyItem(), nil) — "validly empty", never a decode error. The error is
// non-nil only when the wire body bytes fail to decode.
func (m *DataMessage) Item() (secs2.Item, error)
func (m *DataMessage) DecodeErr() error   // cached: the error Item() would return after first decode
func (m *DataMessage) BodyLen() int       // encoded body byte length (no decode needed)
func (m *DataMessage) AppendBodyTo(dst []byte) []byte // copy body bytes into dst (no internal exposure)
func (m *DataMessage) ToBytes() []byte
func (m *DataMessage) Error() error

// Envelope edits — always valid, return a new message sharing the same body (O(header)).
func (m *DataMessage) WithSessionID(id uint16) *DataMessage
func (m *DataMessage) WithSystemBytes(b [4]byte) *DataMessage

// Derive starts a validated structural edit; Build re-validates and returns (msg, error).
func (m *DataMessage) Derive() *DataMessageBuilder
type DataMessageBuilder struct{ /* ... */ }
func (b *DataMessageBuilder) WithStream(s uint8) *DataMessageBuilder
func (b *DataMessageBuilder) WithFunction(f uint8) *DataMessageBuilder
func (b *DataMessageBuilder) WithWaitBit(w bool) *DataMessageBuilder
func (b *DataMessageBuilder) WithItem(it secs2.Item) *DataMessageBuilder
func (b *DataMessageBuilder) Build() (*DataMessage, error)

// ControlMessage is an immutable HSMS control message (SType 1-9, header-only, no body).
type ControlMessage struct{ /* unexported: header [10]byte value */ }

func (m *ControlMessage) Type() MsgType
func (m *ControlMessage) SessionID() uint16
func (m *ControlMessage) SystemBytes() [4]byte
func (m *ControlMessage) HeaderBytes() [10]byte
func (m *ControlMessage) ToBytes() []byte
func (m *ControlMessage) Error() error
// Reject.req carries status/reason in header bytes 2-3:
func (m *ControlMessage) RejectReasonCode() byte // (Reject.req only; 0 otherwise)
func (m *ControlMessage) WithSessionID(id uint16) *ControlMessage
func (m *ControlMessage) WithSystemBytes(b [4]byte) *ControlMessage

var _ Message = (*DataMessage)(nil)
var _ Message = (*ControlMessage)(nil)
```

`Item()` off `ControlMessage` by construction (it isn't on the type) — the Q1 win.

## 5. Construction

```go
// NewDataMessage builds a data message. replyExpected pins the W-bit at construction. Returns an
// error if the body item is invalid (body.Error() aggregate), W/stream/function are invalid, etc.
func NewDataMessage(stream, function uint8, replyExpected bool, sessionID uint16,
	systemBytes [4]byte, item secs2.Item) (*DataMessage, error)

// Control factories (header-only):
func NewSelectReq(sessionID uint16, systemBytes [4]byte) *ControlMessage
func NewSelectRsp(sessionID uint16, systemBytes [4]byte, status byte) *ControlMessage
func NewDeselectReq(...); NewDeselectRsp(...)
func NewLinktestReq(systemBytes [4]byte) *ControlMessage; func NewLinktestRsp(...)
func NewSeparateReq(sessionID uint16, systemBytes [4]byte) *ControlMessage
func NewRejectReq(sessionID uint16, systemBytes [4]byte, reasonCode byte) *ControlMessage
```

- Construction validates via the **Q3 rule** (body aggregate error, W-bit-on-reply, stream/function
  range). `With*` never re-validates (envelope fields are always valid). `Derive().Build()` re-runs the
  full Q3 validation because it can change W-bit/stream/function/body.
- A constructed `*DataMessage` takes the **tree** representation (owns the `secs2.Item` + a 10-byte
  header value; body bytes lazily encoded once via `sync.Once` using `item.AppendTo`).

## 6. Internals — hybrid by provenance

`*DataMessage` holds a 10-byte header value plus one of two internal body representations behind an
unexported interface; both yield the identical observable behaviour:

- **raw-frame** (produced by decode): owns an immutable per-message buffer `rawBody` ( = `[10-byte
  header][body]`, the layout the recv path already allocates); header is `rawBody[0:10]` (read once into
  a `[10]byte` value at construction), body bytes are `rawBody[10:]` (zero-copy). `Item()` lazily runs
  `secs2.Decode(rawBody[10:])` under a `sync.Once`, caching the item and any decode error (`DecodeErr()`).
  `BodyLen()` = `len(rawBody)-10` (no decode). `AppendBodyTo` copies `rawBody[10:]` into `dst`. The 4-byte
  HSMS length prefix is NOT stored — `ToBytes()` recomputes it. **Zero-copy ownership:** the internal
  zero-copy decode entry retains the caller's `rawBody` directly (no copy); it is owned by the message
  and never mutated. The public `DecodeHSMSMessage([]byte)` instead copies its input into an owned
  `rawBody` first (D7-safe for arbitrary callers).
- **tree** (produced by constructors / `Build`): owns the `secs2.Item`; body bytes are lazily encoded
  once via `sync.Once` (`item.AppendTo`), cached for repeat `ToBytes`/`AppendBodyTo`/`BodyLen`. `Item()`
  returns the item directly (error is the item's `body.Error()` aggregate, surfaced at construction not
  here).
- The `internal/wire.Body` handle wraps whichever representation and exposes `Len()`/`AppendTo(dst)`/
  `Chunk(off,n) wire.Chunk`/`Buffers() net.Buffers` — never a bare slice. `*DataMessage` exposes only the
  copy-only public subset (`AppendBodyTo`/`BodyLen`); in-repo transports (2b/2c) reach the zero-copy
  handle via an `internal/`-visibility accessor.
- §5.B "revisit hybrid" was NOT triggered by the bake-off; this representation stands.

## 7. Decode / encode

**Two decode entries (same internal builder, different byte ownership):**
- **Public `DecodeHSMSMessage(data []byte) (Message, error)`** — for arbitrary external callers whose
  buffer lifetime we don't control. **Copies** `data` into an owned `rawBody` so the caller may reuse
  `data` freely. No ownership/transfer contract (D7-safe). Not the hot path.
- **Internal zero-copy decode** — an **unexported** in-repo function (no `internal/*` type in any
  exported signature; external modules cannot reach it) — takes a caller-**owned** per-message buffer
  and **retains it with no copy**. 2a builds and in-package-tests it; the cross-package bridge that lets
  the recv path call it is designed when `hsmsss` is rewritten (it already allocates a fresh GC-owned
  `make([]byte, msgLen)` and reads exactly one message into it — `hsmsss/message_reader.go:137`). This
  eliminates the decode-time copy on the receive hot path (mechanism in 2a; live wiring at the recv
  rewrite).

Both: validate the 10-byte header (`len(rawBody) >= 10`); dispatch on PType/SType → `*DataMessage`
(raw-frame: header `rawBody[0:10]`, body `rawBody[10:]`, lazy `secs2.Decode`) or `*ControlMessage`
(header-only). An empty / header-only data body is valid → `Item()` yields `NewEmptyItem()`.

- HSMS framing: the wire frame is `[4-byte length][10-byte header][body]`, length = `10 + BodyLen()`.
  The 4-byte length is read/written at the framing boundary; the message's `rawBody` holds only
  `[header][body]`. SECS-I uses the same 10-byte header but its own framing (2b) — `ToBytes()` here is
  HSMS-specific.
- Encode: `ToBytes()` assembles length + header + body. Decoded (raw-frame) path emits `len(rawBody)+4`
  bytes directly (header + body already contiguous in `rawBody`). Constructed (tree) path memoizes the
  encoded body via `sync.Once`. `AppendBodyTo` enables writev (2c) to avoid the concat.
- §5.F **option (b)** is adopted for the recv read buffer (per-message GC-owned, not pooled), making the
  zero-copy retain correct. Sub-project 4 may benchmark (a) pooled-read-buffer + copy and switch only if
  it wins; the two paths are mutually exclusive (§5.F) and (b) is the bake-off-favored default.

## 8. Error model (D5)

Three distinct channels, never conflated:
- **`Item() (Item, error)`** — lazy wire→item decode failure (raw-frame path). Empty body ⇒ `(empty, nil)`.
- **`DecodeErr()`** — the cached decode error after `Item()`/the `sync.Once` fired (convenience).
- **`Error()`** — protocol-level (e.g. a Reject.req's meaning); unrelated to decode.
- Construction/`Build` errors are returned idiomatically `(msg, error)` — including the `body.Error()`
  aggregate gate.

## 9. SEMI ground truth (must satisfy)

- **10-byte header (E37 §8.2.5):** [0:2] session id (uint16 MSB-first); [2] W-bit (bit7) + stream
  (bits6-0); [3] function; [4] PType (0 = SECS-II); [5] SType; [6:10] system bytes.
- **SType (E37 §8.2.6.6):** 0 Data, 1 Select.req, 2 Select.rsp, 3 Deselect.req, 4 Deselect.rsp,
  5 Linktest.req, 6 Linktest.rsp, 7 Reject.req, 9 Separate.req.
- **W-bit reply rule (E37 §8.3.3.3):** primary expecting reply ⇒ W=1; otherwise W=0; **a reply (even
  function) must set W=0** — enforced by `Build()`.
- **SECS-I (E4):** same 10-byte header; body bytes + system bytes + W-bit identical across all blocks
  and retransmissions → the immutable shared body is load-bearing (consumed in 2b via `wire.Chunk`).

## 10. Migration (v1 → v2, message layer)

| v1 | v2 |
|----|----|
| `msg.SetSessionID(x)` / `SetSystemBytes(x)` (in-place restamp) | `msg = msg.WithSessionID(x)` / `WithSystemBytes(x)` (new msg, shared body) |
| `msg.SetStream/SetFunction/SetWaitBit/SetItem` | `msg, err = msg.Derive().With*(…).Build()` |
| `msg.Clone()` for fan-out | share `*DataMessage` directly (immutable) |
| `msg.Free()`, pooling toggles | drop — GC-owned |
| `SystemBytes()` returning an alias | returns `[4]byte` value (no alias) |
| `DecodeMessage` body→item inline | `Item()` lazily calls `secs2.Decode` |
| recv `make([]byte, msgLen)` then `DecodeMessage(rawBody)` | recv hands the owned `rawBody` to the internal **zero-copy** decode entry (no copy); the message retains it |

## 11. Success criteria

- **Round-trip:** `DecodeHSMSMessage(m.ToBytes())` reproduces an equal message (header bytes equal; body
  bytes equal; `Item()` value-equal) for data and every control SType.
- **Immutability / no leak:** `SystemBytes()`/`HeaderBytes()` return values (mutating them doesn't affect
  the message); `AppendBodyTo` exposes no internal slice; `WithSessionID` yields a new message while the
  original is unchanged **and the body is shared** (assert same underlying body, O(header) restamp).
- **Validation:** `Build()`/constructors reject a body whose `Error()` aggregate is non-nil (incl. a
  nested-list errored child), W=1 on a reply/even function, and out-of-range stream/function; valid
  inputs succeed.
- **Lazy decode:** a decoded message decodes its body exactly once under a concurrent (`-race`,
  N-goroutine barrier) `Item()` storm; `DecodeErr()` matches.
- **Fan-out:** N goroutines sharing one `*DataMessage` calling `Item()`/`ToBytes()`/`AppendBodyTo`/`With*`
  are `-race` clean; restamp allocates O(header), not O(body).
- **wire bridge:** `wire.Body` exposes no bare `[]byte`; `AppendTo` into a reused buffer is 0 allocs.
- **Zero-copy decode (no decode-time copy):** the internal zero-copy entry, given an owned `rawBody`,
  produces a `*DataMessage` whose body **aliases** that buffer — assert `&body[0] == &rawBody[10]` (the
  message did not copy). The public `DecodeHSMSMessage([]byte)` instead does NOT alias its input — assert
  mutating the caller's slice after decode leaves the message unchanged (it copied). Allocation check:
  the zero-copy path performs no body copy (only the lazy item decode allocates, on first `Item()`).
- Builds in isolation: `go build ./hsms/ ./internal/wire/`, `go test -race ./hsms/ ./internal/wire/`,
  scoped `golangci-lint` — green. (hsmsss/secs1 are rewritten later; module-wide `make ci` stays red.)

## 12. Open implementation details (resolve in the plan)
- Exact unexported body-representation wiring and the `internal/wire.Body` concrete types.
- Whether `MsgType` and the SType constants live in `hsms` or a shared spot reused by `secs1` (2b).
- The control-message `status`/`reason` byte mapping per SType (Select.rsp status, Reject.req reason).

**Deferred to later sub-projects (not built in 2a):** the cross-package zero-copy bridge — how the recv
rewrite / 2b / 2c reach the unexported zero-copy decode core and the message's internal `wire.Body`.
2a keeps `wire.Body` internal and the zero-copy decode core unexported (no public ownership contract, no
`internal/*` type in a public signature). Those later sub-projects **will modify the `hsms` package** to
add a genuinely-internal accessor at that time — a deliberate revisit, not a plug-in. 2a proves the
zero-copy mechanism via an in-package test; the public `DecodeHSMSMessage` copies until the bridge lands.
