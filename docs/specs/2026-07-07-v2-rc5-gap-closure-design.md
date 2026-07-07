# go-secs v2.0.0-rc5 Gap Closure — Design

Date: 2026-07-07
Status: Design approved. Awaiting implementation plan.

## Goal

Close the six gaps surfaced by the eqp-hub `v2.0.0-rc5` gap report (an external
Codex + Antigravity review pass that found real bugs the rc4 migration shipped),
plus two `secs1/doc.go` doc-drift items. The unifying principle:

> **Keep v2's immutable, lazy-decode, per-transport-option design intact — and
> make the correct thing the default (or at least discoverable and cheap).**

Changes are **inert-by-default** (do-nothing consumers see today's behavior) with a
small, explicitly enumerated set of source-breaking / behavior changes — *not* purely
additive, correcting the first draft's overclaim:

1. **Behavior change:** the send wrappers return `(dm, err)` instead of `(nil, err)`
   on an undecodable reply (Gap 1 reply path). Only alters observable behavior for
   replies that were already malformed.
2. **Interface addition (breaks external implementers):** `AddDecodeErrorHandler` is
   added to the exported `SECS2Endpoint` interface (`hsms/endpoint.go`). Any external
   type implementing `SECS2Endpoint` must add the method; in-tree, `hsmstest.FakeEndpoint`
   is updated (see §Gap 1 blast radius). This seam is implemented almost entirely
   in-tree, so real-world blast radius is small — but it is a break, not an addition.

Everything else (Gap 2 options, Gap 4 delegators, Gap 5 docs, Gap 6 helper, doc
drift) is genuinely additive. Gap 5 in particular is resolved **doc-only** — no new
API — precisely to avoid a third break (adding `Ok()` to the `Item` interface).

## Verification basis

All citations below were re-verified against **current `v2` HEAD `e09a98e`**
(not just the rc4 tag the report was written against), then a two-model design
review (Codex/GPT + Antigravity/Gemini 3.1 Pro) re-checked the spec itself against
source. All six gaps still hold. Review-driven corrections are folded in below and
tagged "(review correction)". Notable factual fixes the review forced:

- Gap 1's decode-error accessor is `DataMessage.DecodeErr()` (`hsms/data_msg.go:144`);
  there is **no** `DataMessage.Error()` in v2 (that was v1's eager accessor).
- The reply waiter is **not** error-less (the report's framing was wrong):
  `replyResult{msg, err}` already carries an error (`hsms/reply_registry.go:5-10`);
  the bug is that a body-decode failure never populates it.
- Gap 5's naive `Ok() = (itemErr == nil)` is **incorrect** for lists (aggregate
  `Error()`), and is not "purely additive" — reworked in §Gap 5.
- The Gap-drift `secs1/doc.go:25-28` paragraph now **contradicts** shipped code,
  not merely lagging it (see §Doc drift).

No public `MessageHeader` type exists in `hsms` (checked), which shapes the Gap 1
handler signature. Public decode entrypoints `DecodeHSMSMessage` /
`DecodeHSMSPayload` exist (`hsms/decode.go:32,63`), which the Gap 6 helper uses.

## Non-goals

- No change to the immutable `DataMessage` model, the lazy-decode default, or any
  existing exported signature.
- No `secs1.WithValidateDataMessage` toggle (Gap 3) — see §Gap 3 for why it is
  deliberately dropped rather than deferred-by-omission.
- No release-process change: rc5 lands directly on the `v2` branch and is tagged
  `v2.0.0-rc5`. There is no PR to `main` and no merge (`v2` never merges to `main`).

---

## Gap 1 (highest) — undecodable `DataMessage` reaches the handler / waiter with no delivery-time signal

### Problem

A raw inbound frame becomes a `*DataMessage` **without decoding the body**
(`hsms/decode.go:113,131-137`); the decode error only materializes when
`Item()` / `DecodeErr()` forces the deferred decode (`hsms/data_msg.go:134,144`).
Header accessors `Stream()`/`Function()`/`WaitBit()` read raw header bytes and
succeed regardless (`hsms/data_msg.go:103,106,110`), so a malformed message looks
completely routable at dispatch. Handlers are invoked directly with no forced
check (`hsms/session.go:237-239`). Two distinct failure modes result:

1. **Primary path** — a malformed request is broadcast to *both* the func
   `DataMessageHandlers` **and** the channel handlers (`recvDataMsg` fans out to
   `s.handlers` and `s.chans`, `hsms/session.go:237-247`) as ordinary traffic.
2. **Reply path** — a malformed *secondary* unblocks a pending `SendDataMessage` /
   `SendSECS2Message` waiter as a **clean success**. The waiter plumbing *can*
   already carry an error — `replyResult{msg, err}` (`hsms/reply_registry.go:5-10`),
   and `sendWaitReply` returns `res.msg, res.err` (`hsms/connection_send.go:273-275`;
   `RouteReply` already routes `RejectError` this way) — but a lazy **body**-decode
   failure never *populates* that `err`: the reply is delivered as `{msg, nil}`.
   Unless the caller manually calls `DecodeErr()` (which the eqp-hub migration
   forgot), garbage is consumed as a valid empty reply.

There is currently no option or handler for undecodable frames (`WithRejectUndecodable`,
`WithEagerDecode`, decode-error handler all absent — grep confirmed).

### Design

**New handler kind (mirrors `AddDataMessageHandler`, `hsms/session.go:167`,
`hsms/endpoint.go:111`):**

```go
// DecodeErrorHandler receives an inbound data message whose SECS-II body failed
// to decode. The header accessors (Stream/Function/WaitBit/SessionID/ID) are
// valid; the body is not — never call msg.Item() expecting success. err is the
// deferred decode error (== msg.DecodeErr()).
type DecodeErrorHandler func(msg *DataMessage, err error, ep SECS2Endpoint)

func (s *session) AddDecodeErrorHandler(handlers ...DecodeErrorHandler)
// and add AddDecodeErrorHandler to the SECS2Endpoint interface.
```

The signature passes `*DataMessage` (not a header struct) because no public header
type exists and the frame's header accessors are valid; the explicit `err`
parameter makes the "this did not decode" contract impossible to miss.

**Primary-path integration** (`hsms/session.go` `recvDataMsg`, ~224-248):
*only when ≥1 decode-error handler is registered*, force `msg.DecodeErr()` **before
either fan-out loop** (fires the decode `once`, caches the result). On error →
invoke the decode-error handlers, increment a **new** body-decode metric (see
below), and **do not** call the normal func `DataMessageHandlers` **or the channel
handlers** (`s.chans`) — both are diverted, not just the func handlers. On success →
normal routing; a later `Item()` in a handler reuses the cached decode (no
double-decode; `sync.Once` verified). **Zero handlers registered → today's lazy
path, byte-for-byte unchanged.**

*New metric (review correction).* Do **not** reuse the existing `decodeErr` counter:
`DecodeErrCount` is documented "disjoint from DataMsgRecvCount: a decode failure
never reaches the receive chokepoint" (`hsms/connection_metrics.go:52-57`), and it
counts *frame*-decode failures that occur **before** `incDataMsgRecv()`
(`hsms/connection_runtime.go:74`). A lazy **body**-decode failure happens *after*
that increment, so reusing `decodeErr` would double-count and break the documented
disjoint contract. Add a distinct counter (e.g. `bodyDecodeErr` /
`BodyDecodeErrCount()`) for undecodable messages diverted at the routing boundary.

*Metric plumbing (review correction — not trivial).* Both reviewers flagged that
`recvDataMsg` **cannot currently reach the metrics object**: the `session` holds only
a `TransportRuntime` (`newSession(id, rt TransportRuntime, sysGen)`, `hsms/session.go:54`),
and `c.metrics` lives on the `connection` (`connection_runtime.go:74`), not on the
runtime interface. Two implementation options — resolve in the plan, don't hand-wave:
- **(i)** add a small increment method to the `TransportRuntime` seam (e.g.
  `rt.incBodyDecodeErr()`), which the connection already backs — session calls it; or
- **(ii)** perform the eager-decode-and-divert one layer out, in the connection's
  `RouteData` (`connection_runtime.go:101`) where `c.metrics` is in scope, querying
  the session for decode-error-handler registration. (i) is the lighter change and
  keeps the handler-list logic in the session; (i) is the tentative default.

**Reply-path integration (the one deliberate behavior change).**
The real public methods are `SendDataMessage` and `SendSECS2Message` (there is no
`SendAndReceive`). Each already holds the typed reply (`dm, _ := reply.(*DataMessage)`,
`hsms/session.go:81,114`) but today returns `nil` on any error. The fix lives **in
these wrappers** (no change to the engine's `WriteMessage` contract): after
obtaining the typed reply, force its decode and, on failure, return the decode error
**alongside** the message:

```go
dm, _ := reply.(*DataMessage)
if dm != nil {
    if derr := dm.DecodeErr(); derr != nil {
        return dm, derr   // non-destructive: caller keeps dm for header inspection
    }
}
return dm, nil
```

Note this requires the wrappers to `return dm, derr` (not the current `return nil, err`)
so the message survives the error — the property the design depends on. This applies
**only to the synchronous reply-returning methods**; `SendDataMessageAsync` has no
reply path (`hsms/session.go:86-95`) and is untouched. Rationale for making this
**unconditional** (not gated on handler registration):

- A caller invoking a reply-returning send asked for that reply *specifically to read
  its body* — the eager decode is work they would do anyway, so lazy decode buys
  nothing here. (Lazy decode's value is for primaries that may be routed/relayed
  unread; those paths are untouched — verbatim relay via `ForwardDataMessage` uses
  `WriteMessageNoReply`, registers no waiter, and is confirmed unaffected.)
- It only changes observable behavior for replies that were **already malformed** —
  no correct program relied on a broken reply arriving as a success.
- Gating it on `AddDecodeErrorHandler` would be semantically confusing: that handler
  is *never invoked for replies* (replies go to the waiter, not the handler list),
  so a primary-oriented switch would silently control an unrelated reply behavior.

**Blast radius — `hsmstest.FakeEndpoint`.** Adding `AddDecodeErrorHandler` to the
`SECS2Endpoint` interface breaks the fake's compile assertion
(`hsms/hsmstest/endpoint.go:35-50`). The fake must implement the new method and,
where it delivers scripted replies (`endpoint.go:183-212`), mirror the reply-decode
behavior, or tests depending on it diverge from the real endpoint.

### Tests

- `recvDataMsg`: with a decode-error handler registered, a malformed primary hits
  the decode-error handler and **neither** the func handler **nor** a channel
  handler; with none registered, the malformed primary still reaches both (lazy
  behavior preserved).
- `SendDataMessage`/`SendSECS2Message`: a malformed reply returns `(dm, non-nil err)`
  with `dm != nil`; a well-formed reply returns `(dm, nil)`. Regression guard with
  teeth — reintroduce the "deliver as clean success" path and confirm the test fails.
- `BodyDecodeErrCount` increments on a diverted primary; `DecodeErrCount` does not
  (disjointness preserved).
- Uses the Gap 6 `hsmstest.MalformedDataMessage` helper (build order: Gap 6 first).

---

## Gap 2 — no first-class active-connect timeout

### Problem

v1's `WithConnectRemoteTimeout` is gone from both transports (grep confirmed no
`WithConnectTimeout`/`WithConnectRemoteTimeout` in `hsmsss`/`secs1`/`hsms`). The
active dial goes through the configured `DialFunc` (`hsms/dial.go:12`), default
`(&net.Dialer{}).DialContext` (`hsmsss/config.go:62`, `secs1/config.go:85`),
invoked at `hsmsss/transport_active.go:109` and `secs1/transport.go:250`. The
context handed to that dial is the connection **epoch** context, created from
`context.Background()` (`hsms/connection_lifecycle.go:101-105`) — **not** the
caller's `Open` context — so it carries no dial deadline. Against the default
dialer, a dial to an unreachable peer blocks for the OS connect timeout (~2 min).
Consumers hand-roll a `net.Dialer{Timeout:…}` wrap via the existing `WithDialer`
(`hsmsss/config.go:125`, `secs1/config.go:284`); eqp-hub does this in two places.

### Design

```go
func hsmsss.WithConnectTimeout(d time.Duration) Option   // active role; bounds each dial attempt
func secs1.WithConnectTimeout(d time.Duration) Option
```

Store `d` on the transport config. At each dial call site, when `d > 0`, wrap the
epoch ctx per attempt:

```go
dialCtx, cancel := context.WithTimeout(ctx, d)
conn, err := t.cfg.dial(dialCtx, "tcp", addr)
cancel()
```

Bounds **every** dial attempt (including background-reconnect attempts), so a
powered-off tool no longer hangs on the OS timeout and the start-then-reconnect
story is not delayed by an unbounded first dial. `d == 0` = today's unbounded
behavior. Composes with a caller-supplied `WithDialer` (the timeout wraps whatever
`DialFunc` is configured). Passive role needs no equivalent — the accept loop is
context-cancelled, not deadline-polled.

### Tests

Dial to an unreachable/black-hole address with `WithConnectTimeout(50ms)` returns
within a small multiple of the bound rather than the OS default; `d == 0` keeps the
existing path.

---

## Gap 4 — `DataMessageCodec` bare-vs-wrapped duality (silent drop / nil panic)

### Problem

Application payloads flow as **either** a bare `*hsms.DataMessage` **or** a
`*hsms.DataMessageCodec` wrapping one. `DataMessageCodec`'s entire method set today
is `MarshalBinary`/`UnmarshalBinary` (`hsms/data_msg_codec.go:40,51`) — it exposes
only its nilable `Message` field. So every type-switch that inspects an HSMS payload
must handle both shapes; a switch written against only `*DataMessage` lets a
`*DataMessageCodec` fall through to `default` and become "unknown" — an HSMS message
**silently dropped**. Separately, `(*DataMessage).WithSessionID` dereferences its
receiver immediately (`hsms/data_msg.go:164-168`), so `codec.Message.WithSessionID(id)`
**panics** on a zero-value codec.

### Design

Add read-only delegators + a safe unwrap to `*DataMessageCodec`, each delegating to
the inner `*DataMessage`'s own accessors so a codec is a drop-in read-compatible
wrapper. (Note: `Type()`/`SystemBytes()`/`HeaderBytes()`/`ToBytes()` are on the
`hsms.Message` interface, but `Stream()`/`Function()`/`WaitBit()`/`ID()`/`Item()`/
`DecodeErr()` are `*DataMessage`-specific — the delegators call the concrete
`*DataMessage` methods, not an interface surface.)

```go
func (c *DataMessageCodec) Stream() uint8
func (c *DataMessageCodec) Function() uint8
func (c *DataMessageCodec) WaitBit() bool
func (c *DataMessageCodec) SessionID() uint16               // uint16 (data_msg.go:57-60)
func (c *DataMessageCodec) ID() uint32                       // system bytes / message id (data_msg.go:112-115)
func (c *DataMessageCodec) SystemBytes() [4]byte             // [4]byte, NOT []byte (data_msg.go:64); relay/reply ownership depends on this
func (c *DataMessageCodec) HeaderBytes() [10]byte            // [10]byte (data_msg.go:73)
func (c *DataMessageCodec) ToBytes() []byte
func (c *DataMessageCodec) Type() MsgType                    // MsgType, NOT MessageType (message.go:5)
func (c *DataMessageCodec) DecodeErr() error                 // the Gap 1 safety signal — must be reachable without unwrapping
func (c *DataMessageCodec) Item() (secs2.Item, error)
func (c *DataMessageCodec) ToDataMessage() (*DataMessage, bool)
```

Return types above are the **verified** `*DataMessage` signatures (`SystemBytes()
[4]byte`, `HeaderBytes() [10]byte`, `Type() MsgType`) — the zero value a nil-guard
returns is the zero array / `DataMsgType`, not a nil slice.

- **Scalar/byte read delegators guard nil** `Message` and return the zero value
  (reads become panic-free).
- **`Item()` does NOT return `(nil, nil)` on nil** (review correction): a `(nil, nil)`
  result would hide an absent wrapped message. When `c == nil || c.Message == nil`,
  `Item()` returns `ErrNilMessage` — consistent with `MarshalBinary`'s existing nil
  handling (`hsms/data_msg_codec.go:38-46`) and `ForwardDataMessage`'s
  (`hsms/session.go:124-129`). `DecodeErr()` likewise reports the nil state rather
  than a misleading `nil`.
- **`ToDataMessage()` is the zero-safe probe:** `(nil, false)` when `Message == nil`,
  `(Message, true)` otherwise. A consumer that only reads never unwraps and cannot
  fall through a type switch.
- **`DecodeErr()` delegation is load-bearing:** Gap 1 makes "check `DecodeErr()`" the
  safety contract, so a codec that can't surface it without an unwrap defeats the
  ergonomic purpose.
- **No** nil-safe `With*` mutator: a nil-receiver `WithSessionID` that silently
  no-ops would *hide* the bug and is worse than a panic. `MarshalBinary`'s existing
  `ErrNilMessage` (`hsms/data_msg_codec.go:40-46`) stays as the mutation-side guard.

(All names/types above are verified against `hsms/message.go` and `hsms/data_msg.go`:
`Type() MsgType`, `SystemBytes() [4]byte`, `HeaderBytes() [10]byte`.)

### Tests

Table test: for a codec wrapping a valid message, every delegator equals the inner
`DataMessage`'s accessor; for a nil-`Message` (and nil-receiver) codec, scalar/byte
delegators return the zero value, `Item()`/`DecodeErr()` report `ErrNilMessage`, and
`ToDataMessage` returns `(nil, false)` — all without panic.

---

## Gap 5 — a passing `Is*()` predicate does not imply a nil `To*()` error

### Problem

v2 items carry a **deferred** error (`baseItem.Error()`, `secs2/item.go:356`). Type
predicates ignore it: `(*IntItem).IsInt8()` is literally `return item.byteSize == 1`
(`secs2/int.go:231`), while `(*IntItem).ToInt()` returns `item.itemErr` when set
(`secs2/int.go:75-77`). So the natural, safe-*looking* idiom

```go
case item.IsInt8() || item.IsInt16():
    v, _ := item.ToInt()   // discards a deferred error the predicate said nothing about
```

silently converts a broken item into a zero value. The iterators already do the
right thing (`Ints()` yields nothing when `itemErr != nil`, `secs2/int.go:112-115`),
so the predicates are the inconsistent surface. No `Ok()` method exists today.

### Design (corrected after review — the naive `Ok()` was wrong)

The original spec proposed `func (b *baseItem) Ok() bool { return b.itemErr == nil }`.
**The Codex review proved this incorrect** and it must not ship as written:

- **List aggregation.** `ListItem.Error()` aggregates child errors via `errors.Join`
  (`secs2/list.go:134-150`; test at `secs2/list_test.go:208-216`): a list can have
  `itemErr == nil` while `Error() != nil` because a **child** is invalid. So
  `Ok() = (itemErr == nil)` would report a broken list as OK — a correctness bug.
  `Ok()` must mean `Error() == nil` and be **aggregate-aware**, which means it cannot
  simply live on `baseItem` reading `itemErr`; `ListItem` needs its own implementation
  delegating to its aggregate `Error()`.
- **Not purely additive.** For `Ok()` to be callable on a value typed as `secs2.Item`
  (the whole point of the guarded pattern), it must be added to the **exported
  `Item` interface** (`secs2/item.go:155-324` has no `Ok()` today). Adding a method
  to an exported interface is a **breaking change** for any external implementer of
  `secs2.Item`. The earlier "purely additive" framing was wrong.

**Decision: doc-only, reuse the existing `Error()` (truly additive).** No `Ok()`
method is added. Add prominent doc callouts on the `To*()` accessors and the `Is*()`
predicates steering callers to the **existing** `Error()` method — which is already
on the `Item` interface (`secs2/item.go`) and already aggregate-aware for lists
(`secs2/list.go:134-150`). The guarded pattern the docs teach:

```go
if v, err := item.ToInt(); err == nil {
    // use v — the deferred error is checked, not discarded
}
// or, when branching on type first:
if item.IsInt8() && item.Error() == nil { … }
```

This keeps the change **zero new API, zero break, and correct for lists** — the trap
the naive `Ok()` fell into. `Is*()` semantics stay **unchanged** (not made
error-aware) — that remains a possible future opt-in semantic change, out of scope
for rc5.

### Tests

Since Gap 5 ships doc-only (no new symbol), the "test" is a doc-example that
compiles/runs: a **list** whose own `itemErr == nil` but which carries an **invalid
child** reports `Error() != nil` — the exact case the naive `itemErr`-only check got
wrong (`secs2/list_test.go:208-216` already covers the aggregation; reference it).
The doc example on the accessors demonstrates the checked pattern
(`if v, err := item.ToInt(); err == nil { … }`).

---

## Gap 3 — SECS-I assembler S9F1/S9F7 is equipment-role-only (clarification)

### Status: doc note only; no toggle

v2 SECS-I **does** validate multi-block reassembly (`secs1/assembler.go:90-98,128-149,167-180`)
and **does** auto-reply S9F1 (device-ID mismatch) / S9F7 (block/header/first-block)
via `notifyAssemblerViolation` (`secs1/transport.go:183-205`), gated on
`IsEquip() == true` (`secs1/transport.go:184`). The three assembler metrics exist
(`secs1/metrics.go:83,89,95`). The only real change from v1 is a lost ability to
*suppress* equipment-role S9Fx (v1 had a `ValidateDataMessage` toggle; v2 hardcodes
it on). Host role sends nothing in either version, so **eqp-hub impact is nil**.

**Decision — drop `secs1.WithValidateDataMessage`, add a doc note instead:**

1. No demonstrated need to suppress; adding the option is speculative surface
   (against the project's "no unasked-for flexibility" principle).
2. SEMI E5 §10.13 says equipment *should* emit these notifications; a switch whose
   only effect is to make equipment silently non-conformant is itself a footgun —
   the opposite of this report's spirit.
3. Deferral is free: the option is trivially additive later with zero migration cost
   if a real suppression need ever appears.

Add a one-line note to `secs1/doc.go`: *assembler-violation S9F1/S9F7 is
equipment-role-only and not separately configurable.* Consumers can then delete any
orphaned `validateDataMessage` config key with confidence.

---

## Gap 6 — `hsmstest` helper to construct a decode-error message

### Problem

Testing the Gap 1 fix needs a `*DataMessage` whose framing is valid but whose
SECS-II body fails to decode lazily. There is no supported way to build one;
`newRawFrameDataMessage` is unexported (`hsms/decode.go:131`), so external tests
cannot reach it and must forge wire bytes by hand. Existing `hsms/hsmstest` helpers
cover build (`build.go`), equality (`equal.go`), and a fake endpoint (`endpoint.go`)
— nothing malformed.

### Design

```go
// MalformedDataMessage returns a *hsms.DataMessage with valid header fields
// (stream/function/…) whose SECS-II body fails to decode: msg.DecodeErr() != nil,
// msg.Item() returns that error. For testing decode-error handling (Gap 1).
func hsmstest.MalformedDataMessage(stream, function uint8, waitBit bool) *hsms.DataMessage
```

Implementation forges a frame with a valid header and a deliberately corrupt body
(e.g. a leading list item-header claiming more children than the frame carries,
length prefix intact), routes it through the public `hsms.DecodeHSMSMessage`
(`hsms/decode.go:32`), and returns the resulting `*DataMessage` (type-asserted).
Byte-forging lives in this one tested helper instead of every consumer's error-path
test. Verify the returned message's `DecodeErr()` is non-nil in the helper's own test.

---

## Doc drift (two fixes in `secs1/doc.go`)

1. **Correctness (not just staleness).** `secs1/doc.go:25-28` still says runtime
   `UpdateConfigOptions` does **not** intercept `hsms.WithWriteTimeout` — but
   `secs1/new.go:92-95` now overrides `UpdateConfigOptions` to force-append
   `WithWriteTimeout(0)` (and the session ID) as trailing options, so it **is**
   intercepted. Rewrite/remove the paragraph to match shipped code.
2. **Completeness.** The metrics section (`secs1/doc.go:59-68`) omits the three
   block-assembly metrics now exported (`secs1/metrics.go:83,89,95`):
   `DeviceIDMismatchCount`, `BlockNumberMismatchCount`, `InvalidFirstBlockCount`.
   Add them — they are the observable counterpart to the Gap 3 assembler validation.

---

## Phasing

All changes are independent except where noted; each phase is buildable and testable
on its own. Per repo discipline, do not run two edits at the same source file in
parallel, and give regression guards teeth (reintroduce the bug, confirm the test
fails).

1. **Phase 1 — hsms core.**
   - Gap 6 `hsmstest.MalformedDataMessage` **first** (unblocks Gap 1 tests).
   - Gap 1 decode-error handler + reply-path change (`session.go`, `endpoint.go`,
     `connection_send.go`/reply path, the `BodyDecodeErr` metric + its plumbing, and
     the `hsmstest.FakeEndpoint` mirror). Largest, highest-risk phase.
   - Gap 4 `DataMessageCodec` delegators (`data_msg_codec.go`) — independent file.
2. **Phase 2 — secs2.** Gap 5 is **doc-only** — accessor/predicate doc callouts
   steering to the existing `Error()` (no `Ok()`, no code change beyond Godoc).
3. **Phase 3 — transport options.** Gap 2 `WithConnectTimeout` in `hsmsss` and
   `secs1` (config + dial call sites).
4. **Phase 4 — secs1 docs.** Gap 3 note + both doc-drift fixes (`secs1/doc.go`).

## Success criteria

- `make lint` and the full test suite pass (skip `^Fuzz` under high `-count` per the
  known stress-test flake).
- Each new surface has a test; each of Gaps 1, 4, 5 has a teeth-verified regression
  guard.
- Godoc for every new symbol is present and free of internal jargon codes.
- eqp-hub can delete: two hand-rolled timeout dialers (Gap 2), the `AsDataMessage`
  fall-through workarounds (Gap 4), the eight discard-the-error accessors (Gap 5),
  and the hand-forged malformed-message test bytes (Gap 6).
- Tagged `v2.0.0-rc5` on the `v2` branch.
