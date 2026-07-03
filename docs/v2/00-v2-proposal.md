# go-secs v2 — Analysis Report & Proposal

**Status:** Draft for review
**Date:** 2026-06-28
**Scope:** A clean, breaking `/v2` of `github.com/arloliu/go-secs`, focused first on an
immutable, high-performance message model (`secs2` + `hsms`), with connection /
state-machine rearchitecture explicitly deferred until the message model is stable.

---

## 1. TL;DR

go-secs v1 documents its SECS-II items as *immutable* (`secs2/item.go:79-92`) while
shipping `SetValues`, a lazily-written `rawBytes` cache, a `sync.Pool` + `Free`
lifecycle, and in-place HSMS header rewrites (`hsms/data_msg.go:157`). That gap is the
root cause of a data race on the concurrent fan-out path and of an entire
clone/isolation apparatus (`Clone`, `CloneCodec`, `SnapshotForRelay`,
`secs2.Item.Clone`) that exists only to work around mutability.

v2 resolves the contradiction by making the model **genuinely immutable** and
**shared by reference**, and by promoting the header/body split that
`hsms/fanout_snapshot.go` already prototypes from a relay-only escape hatch to the
canonical send path — but composing a *new* per-hop header over a *shared* immutable
body, **not** by patching a shared frame in place.

Seven decisions are locked (Section 4). The work is a program of eight dependency-ordered
sub-projects (Section 6); each gets its own spec → plan → implementation cycle. The locked
decisions are *correctness/API* calls (immutability, header/body separation, error model,
`With*`/builder, **removal of the public `Free`/pool API**, D7). The *performance* calls —
the body representation and how much *internal* pooling to keep — are **not made by fiat**;
they are gated on a benchmark harness that is the first sub-project to land.

---

## 2. Goals

From `tmp/go-secs-v2-idea.md` and the downstream consumer proposal
`tmp/go-secs-v2-immutability-proposal.md` (`eqp-hub`, a software-defined virtual switch
hub that forwards SECS/HSMS equipment data):

1. **Full immutability** of `hsms`, `secs1`, `secs2` message objects. A frozen value is
   freely shareable across goroutines; mutation is impossible by construction, not by
   convention.
2. **Derive-new instead of mutate.** Replace in-place `Set*` with `With*` methods that
   return a new object sharing the original's body; the original is never altered.
3. **Rearchitect** connection, state-transition, and error handling for long-term
   maintainability — *deferred* in this proposal (Section 6), but the message model is
   designed not to block it.
4. **Rethink the message struct from scratch:**
   1. Separate the header bytes from the body bytes so per-hop header restamps need no
      lock and no body copy.
   2. Consider keeping raw bytes in the struct as a source of truth for lazy decode,
      with `secs2` items as a *view* over those bytes — **benchmark-gated**, not assumed.
5. **Reduce allocation / GC pressure** without sacrificing correctness or safety. No
   over-engineering.
6. **A reliable, extensible integration-test architecture** that secures v2's quality.

Non-negotiable framing from the idea doc: breaking changes are allowed; v1 users get an
easy migration path; the deliverable is this analysis report + proposal under `docs/`.

---

## 3. Current-state analysis

Grounded in a first-hand read of the load-bearing files plus a full-package
architecture map. Representative `file:line` anchors are given; near-duplicates are
collapsed.

### 3.1 The immutability contradiction (the root)

`secs2.Item` is documented immutable (`item.go:90-92`) but every concrete type ships a
mutating `SetValues` and a mutable lazy `rawBytes` cache. The canonical case,
`ListItem.ToBytes` (`secs2/list.go:195-218`):

```go
func (item *ListItem) ToBytes() []byte {
    if item.rawBytes != nil { return item.rawBytes } // read
    // ... compute ...
    item.rawBytes = result                            // write — races a concurrent reader
    return result
}
```

`SetValues` (`list.go:161-187`) invalidates that cache in place (`list.go:163`). Under
fan-out — one `*DataMessage` handed to multiple handler goroutines — a concurrent
`ToBytes()` is a data race. `ListItem.Clone` (`list.go:237-246`) exists specifically to
give each consumer a private cache.

The mutation surface is wider than the lazy cache: typed accessors return the **live
backing storage**, not copies — `ListItem.ToList`/`Values` return `item.values`
(`list.go:130`/`:147`), `BinaryItem.ToBinary` returns `item.values` (`binary.go:108`),
and the integer/uint/float/boolean accessors do the same. A caller can mutate the slice it
gets back and corrupt a shared (or memoized) body. There is also a standalone
`LocalizedStrItem.SetLSH` mutator (`secs2/localized_str.go:89`). True immutability must
close all of these, not just `SetValues`.

### 3.2 In-place HSMS header rewrites

`DataMessage` (`hsms/data_msg.go:37-45`) is a cache-line-packed struct whose header
fields are rewritten in place: `SetSessionID` (`:157`, confirmed `msg.sessionID = …`),
`SetID`/`SetSystemBytes`/`SetHeader` (`:171`/`:195`/`:245`),
`SetStreamCode`/`SetFunctionCode`/`SetWaitBit` (`:271`/`:291`/`:305`). On the forward
path two destinations share identical content and differ only in the session id they
stamp — but the in-place stamp aliases a shared pointer, which both races and corrupts
sibling destinations.

### 3.3 The clone / pool / Free apparatus

Exists only because the body is mutable:

- `hsms/data_msg.go:483` `Clone` / `:517` `CloneCodec` / `:553` `SnapshotForRelay`.
- `secs2/item.go:176` `Item.Clone` (all impls).
- `sync.Pool` + `Free` for `DataMessage` (`data_msg.go:453`) and for **8 of the 9 item
  types** (List/ASCII/JIS8/Boolean/Binary/Int/Uint/Float, `secs2/pool.go:7-15`);
  `LocalizedStrItem` is heap-constructed and unpooled (`secs2/localized_str.go:32-42`) — an
  inconsistency v2 unifies. All pooling is gated by a single global `usePool` boolean
  (`secs2/pool.go:176`), with documented use-after-`Free` footguns (`data_msg.go:180-186`).

### 3.4 `snapshotMessage` — the right idea, but single-owner

`hsms/fanout_snapshot.go` is the hand-rolled header/body split v2 should generalize: it
owns a full frame and serves `ToBytes()` verbatim until structural access, decoding the
item body lazily (`fanout_snapshot.go:32-49`). **But it is explicitly not safe for
concurrent use** (`:17-18`), it patches the header *in place*
(`SetSessionID` → `frame[4:6]`, `:73`), and its lazy decode write (`s.item`,
`s.decodeErr`) is unsynchronized. It proves the decode-free relay mechanism; it must not
be copied literally into the immutable multi-consumer world, where a shared frame cannot
be patched and a lazy write must be race-free.

### 3.5 Per-type caching asymmetry

Only List/ASCII/Binary/Boolean cache `rawBytes` and receive zero-copy decoder slices;
Int/Uint/Float/JIS8/LocalizedStr regenerate on every `ToBytes` and copy on decode.
Smaller scalars are plausibly cheaper decoded inline than stored-raw-plus-decoded; large
lists/strings clearly favor raw. A universal "bytes-as-truth" model would force one
strategy; the right strategy is per-type and must be benchmark-cut, not decreed.

### 3.6 Error model

Errors live inside objects and aggregate via `errors.Join` (`item.itemErr`, `msg.err`,
`snapshot.decodeErr`), which can mask one error with another and is un-idiomatic for a
Go library. Lazy decode complicates this: a decode failure can surface only at
access-time, never at construction — so a pure `(T, error)` constructor model cannot, by
itself, express it.

### 3.7 Allocation profile (static accounting — not yet profiled) and the pooling question

The existing per-call accounting (`tmp/hsmsss-hotpath-perf-analysis-revised.md`) is an
explicit **static read of source — "no profiler run, no benchmark execution"** (its own
Method note). It *estimates* a sync send round-trip at roughly **9–11 heap allocations**
plus per-leaf encode allocs: the 4-byte system-bytes `CloneSlice`, the `EmptyItem` on the
nil-payload path, the reply-channel + `sentChan` + `sendRequest`, the framed
`make([]byte, 14, total)` and its body copy, and on recv the variable `make([]byte, msgLen)`
body read (the largest GC contributor). Pools (`DataMessage`, items, 128 KB I/O buffers,
timers via `internal/pool/timer_pool.go`) were added specifically to amortize these. These
figures are **a starting hypothesis, not a measurement** — sub-project 0 must reproduce them
with `-benchmem`/profiles. And **what pooling is actually worth under a GC-owned immutable
model is entirely unmeasured** — there is no GC-pause-with-vs-without-pools benchmark in-repo.
We therefore treat both the alloc counts and pooling-vs-GC as **open, benchmark-gated
questions** (sub-project 0 / 4), not settled facts.

### 3.8 Two connection stacks that drift

`hsmsss` and `secs1` independently implement the dual-state model (`opState` CAS +
`stateMgr` mutex), three send entry points, and context-lifecycle management — with
*different* mechanics (`hsmsss` uses `atomic.Pointer[ctxHolder]` lock-free reads; `secs1`
uses `ctxMutex` with a fragile `sendMu > ctxMutex` ordering rule and a gate-reset that
must stay outside `createContext`). The project's own history records fixes to one stack
repeatedly failing to reach the sibling. This is real and costly, but **out of scope for
this proposal** (Section 6 defers it).

---

## 4. Design decisions (locked)

| # | Decision | Choice | Rationale |
|---|----------|--------|-----------|
| D1 | Release strategy | **Clean `/v2` module only** (parallel-installable; v1 frozen). **Major-branch layout:** v2 lives at the repo root on the `v2` branch, v1 stays on `main`, `v2.x` tags go on the `v2` branch; module path `github.com/arloliu/go-secs/v2` (applied 2026-06-28). | One code path to design; no v1 middle-path. Major-branch fits "v1 frozen + full rewrite" with no file moves and reinforces that v2 must never merge to `main` (distinct module identity). |
| D2 | Body source of truth | **Locked invariant:** body is immutable and the header is separable from it. **Benchmark-gated (sub-project 0):** the hybrid-by-provenance representation itself, the per-type leaf strategy (zero-copy frame-view vs eager decode), the memoization mechanism, value-vs-pointer `DataMessage`, and confirmation the hybrid does not regress structural access. | The *what* (immutability + header/body separation) is committed; the *how* (representation per provenance/type) is the candidate in §5.B, decided by the gate. Matches "99% are decoded or will-be-encoded." |
| D3 | Header/body split level | **Message-level only** | HSMS already separates the 10-byte header from the item body; keep `secs2.Item` simple. |
| D4 | Connection rearchitecture | **Deferred** | Stabilize + benchmark the message model first; revisit shared-core-vs-parity afterward. |
| D5 | Error model | **Idiomatic `(T, error)` + separate decode-error accessor** | Construction/parse errors return idiomatically; lazy-decode failures surface via a distinct `DecodeErr()`, kept apart from protocol-level `Error()`. |
| D6 | Mutation API | **Value-returning `With*` for always-valid envelope fields (session id, system bytes); validated `Derive()…Build() (msg, error)` for everything validity-sensitive (W-bit, stream, function, body)** | `With*` reads correctly for returns-new and avoids the silent-no-op footgun of a returns-new `Set*`; routing validity-sensitive fields through the error-returning builder prevents minting immutable protocol-invalid messages (e.g. W-bit on a reply, E37 §8.3.3.3). |
| D7 | Public pooling API | **Removed unconditionally** — no public `Free`, no `usePool` global, no caller-visible ownership/transfer contract. Internal pooling (decode scratch, I/O read buffer, timers) is permitted but invisible; only its *extent* stays benchmark-gated (sub-project 4). | This is a **correctness** decision, not a performance one. With immutable, shared-by-reference fan-out, "when does the *last* concurrent downstream release the message?" is unanswerable — the documented source of this project's pool double-Free / use-after-`Free` / `queueSendRequest`-ownership-contract bug history. GC owns lifetime; perf (how much *internal* pooling to keep) is the separate D2/sub-project-4 question. |
| D8 | Go version floor | **`go 1.26.0`** in `go.mod`; no pinned `toolchain` directive (applied 2026-06-28). | Matches the bake-off toolchain and enables range-over-func iterators (`iter.Seq`) so immutable Item accessors can offer read-only traversal exposing **no** backing slice — the core mechanism for fixing v1's leaky accessors — plus the latest stdlib. A breaking v2 may demand a recent floor; omitting a `toolchain` pin avoids forcing toolchain downloads on library consumers. |

---

## 5. Target architecture — the v2 message model

### 5.A Immutable `secs2.Item`

An `Item` is a deep-immutable value, shared freely by reference. (Item structure and
format codes follow SEMI **E5 §9 Data Structures** and **Table 3 Data Item Dictionary** —
`~/semi_standards/markdowns/e005-00-0813/e005-00-0813.md`.)

- **Removed — every mutator:** `SetValues`, `Clone`, `Free`, **and `LocalizedStrItem.SetLSH`**
  (`secs2/localized_str.go:89`). The audit must enumerate and delete *all* in-place writers,
  not just `SetValues`.
- **Accessors must not leak mutable backing storage.** Today `ToList`/`Values`/`ToBinary`/
  `ToInt`/… return the live internal slice (`secs2/list.go:130`,`:147`; `binary.go:108`;
  `int.go:92`; etc.), which would let a caller mutate a frozen (or memoized) body. v2
  accessors are **read-only by construction** — one of: (a) return a defensive copy;
  (b) expose index/length (`At(i)`, `Len()`) and a Go 1.23 `iter.Seq` range function with no
  settable handle; (c) return an explicitly read-only view type. The bake-off (Section 5.G)
  decides copy-vs-iterator per type by cost. `ToASCII`/`ToJIS8`/`ToLocalizedStr` already
  return `string` (immutable) and are unaffected.
- **Added:** `Err() error` — reports a lazy-decode failure for view-backed items
  (Section 5.D); for fully-constructed items it is always nil because construction
  validates up front and returns `(Item, error)`.
- **Construction:** all-at-once constructors (`secs2.A(s)`, `secs2.L(children…)`,
  `secs2.U2(v…)`, …) plus a list **Builder** for incremental assembly. No
  post-construction mutation.
- **`ToBytes` memoization:** because an item's value never changes, the serialized form
  is a pure function of the value and is memoized once. The *mechanism*
  (`sync.Once` vs `atomic.Pointer` double-check vs mutex-guarded) is **benchmarked**, not
  assumed (Section 5.G) — `sync.Once` guarantees a single encode but costs a word;
  `atomic.Pointer` is smaller but may encode twice on a race unless mutex-guarded, which
  matters for large bodies.
- **Folded-in cleanups:** unify the per-type caching strategy per the bake-off
  (Section 3.5); fix `LocalizedStr`-never-pooled and the JIS8 reset inconsistency as part
  of the rewrite; replace the global `asciiStrictMode` (`atomic.Bool`) with parser/encoder
  config (Section 5.E).

### 5.B Hybrid message model — one immutable interface, two provenances

A `DataMessage` satisfies a single immutable interface but has two internal
representations chosen by how it was created:

1. **Decoded-from-wire.** Owns the immutable raw frame `[]byte`
   (`[4-byte length][10-byte header][item bytes]`), never mutated. Header fields are read
   decode-free from `frame[4:14]`. `Item() (secs2.Item, error)` lazily decodes `frame[14:]`
   into an immutable tree **exactly once**, race-free (this is `snapshotMessage`'s idea, made
   concurrency-safe and truly immutable) — and a decode *failure* is reported as a non-nil
   `error`, never as a silently-empty item (Section 5.D). Decoded leaf items may
   zero-copy-reference subslices of the frame — safe in v2 precisely because the frame is
   immutable and GC-owned; the v1 hazard was pooling, which is gone here.
2. **Constructed-from-code/SML.** Owns the immutable item tree and a small immutable
   header value; the wire bytes serialize once and are memoized.

Both are frozen values with identical observable behavior. The decoder produces (1); the
builder/constructor produces (2). **This is the candidate architecture, not yet locked.**
Per D2, what *is* locked is only the invariant — body immutable, header separable. The
**hybrid-by-provenance representation above, the per-type leaf strategy (zero-copy
frame-view vs eager decode-and-own, the §3.5 asymmetry), the memoization mechanism, and
value-vs-pointer `DataMessage` are all gated on sub-project 0**. If the bake-off shows the
decoded-raw-frame path regresses the structural-access arm beyond mitigation, the hybrid is
revisited before anything downstream is built. Implementers must not lock (1)/(2) ahead of
the gate.

### 5.C Header/body split + fan-out — the load-bearing mechanism

Message-level split:

- **Body** = the item tree (or, for decoded messages, the raw `frame[14:]`) plus its
  serialize-once bytes. **Shared by reference** across hops; never copied on forward.
- **Header** = a small immutable value `{sessionID, stream (waitBit MSB + code),
  function, systemBytes [4]byte}`.

Per-hop restamp composes a **new header value over the same shared body** — never an
in-place frame patch:

```go
func (m *DataMessage) WithSessionID(id uint16) *DataMessage {
    return &DataMessage{header: m.header.withSessionID(id), body: m.body} // body shared
}
```

**The crux — keep restamp O(14 bytes), not O(bodyLen).** A `ToBytes()` that returns one
`[]byte` forces an O(body) copy to concat header+body. To avoid that on the hot send
path, the message exposes the body separately — but, per 5.A's no-leak rule, **never as a
writable `[]byte`**. Public body access is read-only by construction:

```go
// Public — COPY-ONLY by construction; no internal slice (writable or not) ever escapes:
AppendBodyTo(dst []byte) []byte // copy the body into a caller-owned buffer
BodyLen() int                   // length without materializing the bytes
HeaderBytes() []byte            // a fresh 14-byte length+header prefix (caller owns the copy)
ToBytes() []byte                // convenience: allocate + assemble the whole frame
```

The zero-copy body `[]byte` (`frame[14:]` or the memoized tree bytes) is exposed **only
through a module-internal bridge**, never as a public method. The mechanism must respect Go
package boundaries: the trusted consumers — the `hsmsss`/`secs1` connection writers and the
SECS-I splitter — are **separate packages that import `hsms`**, so an *unexported* `hsms`
method is uncallable by them, while an *exported* `[]byte` method re-creates the leak. The
resolution is Go's `internal/` visibility: the immutable body lives behind an
**`internal/wire`** type (e.g. `wire.Body` with a `Bytes() []byte` accessor). Every in-repo
transport (`hsms`, `hsmsss`, `secs1` — all under `github.com/arloliu/go-secs/`) may import
`internal/wire` and take the zero-copy view; **external modules cannot import `internal/…`**,
so they never reach the raw slice. `hsms.DataMessage` holds the `wire.Body` and re-exports
only the copy-based public API (`AppendBodyTo`/`BodyLen`/`HeaderBytes`/`ToBytes`).
Consequently there is deliberately **no public `WriteBodyTo(io.Writer)`**: `Write(p []byte)`
would hand the shared slice to arbitrary (possibly buggy or hostile) caller code — a
behavioral contract, not construction-level immutability. External callers that need the
body get a **copy**; external body streaming is not a hot path, so the copy is acceptable.
(This is exactly the leak 5.A bans for items, applied at the body layer.) Sub-project 2 must
specify this `internal/wire` bridge — the exact type, who may construct it, and a
**lifetime-aware** access contract, as part of freezing the body model. That contract is:

- **Never mutate** the body bytes (always — the body is immutable).
- **Retention is allowed, but only through an owned immutable handle** (`wire.Body` and its
  `wire.Chunk` sub-views). Holding a handle/sub-slice across time is safe precisely because
  the body is immutable and GC-owned, so it can never change and stays reachable. This is
  what lets the **SECS-I splitter keep zero-copy block sub-slices across send/retry** (a
  `*Block` is retried until ACK/abort, SEMI **E4 §7.8**), with no copy.
- **No-retain applies only to the transient raw `[]byte`** handed to a one-shot writer — the
  OS/`net.Conn` write in the writev send path — which by I/O contract must not retain it. No
  caller-supplied `io.Writer` ever receives it (there is no public `WriteBodyTo`).

…internally, the **connection writer emits the shared body via `net.Buffers` (writev)** —
one syscall, the large body never recopied per hop. Fan-out to N destinations becomes N
~24-byte header envelopes over one shared body:

```go
for _, dev := range devices {
    conn.Send(msg.WithSessionID(dev.ID)) // N tiny headers, ONE shared body, zero body copies
}
```

This is the "N clones → 0 clones + 1 serialize + share" win. Consequently `Clone`,
`CloneCodec`, and `SnapshotForRelay` **retire** — sharing-by-reference replaces them.
(The 4-byte length prefix depends on body *length*, not contents, so a header restamp
never re-lengths or re-serializes the body — confirmed against SEMI **E37 §8**,
`~/semi_standards/markdowns/e037-00-0413/e037-00-0413.md`.)

**The send path must actually change — the guaranteed body copy is in `ToBytes()`.** Today
`hsmsss` builds one slice (`buf := msg.ToBytes()`, `hsmsss/conn.go:948`) and `ToBytes`
allocates `make([]byte, 14, total)` then **copies the whole body in via `append`**
(`hsms/data_msg.go:347-368`) — that concat is the unavoidable O(body) copy. (The subsequent
`bufio.Writer.Write` is *not* the culprit: `bufio` writes large payloads straight through
when its buffer is empty.) The internal body view is necessary but not sufficient: sub-project
2 must eliminate the `ToBytes` concat on the hot path by writing a prefix-then-body
**vectored write directly over the `net.Conn`** (e.g. `net.Buffers{prefix, bodyView}` →
`WriteTo`, falling back to two `Write`s) under the existing `writeMutex`, and
**benchmark-prove the body slice is not copied**. Small control frames may still use the
single-slice path; the win is specifically the large-body data path.

**Control messages are in scope too.** The goal names immutable `hsms` messages, not just
`DataMessage`. Today the `HSMSMessage` interface carries mutating setters + `Free` + `Clone`
(`hsms/hsms_msg.go`), and `ControlMessage` mutates and aliases its header/system-byte
storage (`hsms/control_msg.go`). v2 makes **`ControlMessage` immutable too** —
Select/Deselect/Linktest/Separate/Reject (SEMI **E37 §8**, incl. Table 8 Deselect Status /
Table 9 ReasonCode) become frozen values with copy/value header access and `With*`
derivation for the session/transaction (`ID`) fields. This lands in sub-project 2.

**The body interface is transport-neutral.** SECS-II item bytes are identical on HSMS and
SECS-I (E5 is transport-independent); only the 10-byte protocol header differs. So the
immutable shared body must serve **both**: HSMS frames it whole, while SECS-I splits it into
≤244-byte blocks (SEMI **E4 §7/§8**) and reassembles. The internal bridge lets SECS-I's
splitter take **zero-copy block sub-views as owned `wire.Chunk` handles** of the shared body
(safe to retain across send/retry per the lifetime-aware contract above) — eliminating
today's per-block `make`+`copy` (`secs1/message.go:48-49`) — and reassembly concatenates
received block bodies into the body bytes before lazy decode. This body byte/chunk interface is
designed and frozen in **sub-project 2** (so `DataMessage` is not frozen before SECS-I's
needs are known); the SECS-I *connection/state* rewrite itself stays deferred (sub-project 5).

**`secs1.Block` becomes internal and immutable.** Today it is an exported, mutable framing
type — public `Header`/`Body` fields plus in-place setters (`secs1/block.go:41-48`,`:120-159`).
A `Block` is a SECS-I *transport-framing* detail, not a user-facing message object (the
user-facing immutable message is the same `DataMessage`). v2 makes it **unexported and
immutable**, constructed only by the block splitter/assembler from the shared body via the
chunk interface above. The immutable `Block` type lands in **sub-project 2** (it is part of
the body/chunk interface); the block-transfer *protocol* (ENQ/EOT/ACK/NAK state machine, T1–T4)
stays in the deferred **sub-project 5**. This satisfies the goal's "`secs1` message objects
immutable" without un-deferring the connection rewrite.

### 5.D Error model

- Constructors, parsers, and decoders return idiomatic `(T, error)`:
  `secs2.NewListItem(...) (Item, error)`, `hsms.DecodeHSMSMessage([]byte) (HSMSMessage, error)`.
- **Structural access on a view-backed message returns `(secs2.Item, error)`** — the
  decode error is surfaced *positionally*, not only through a side accessor. This is the fix
  for a real ambiguity: a **header-only / empty data message is legitimate** under SEMI
  **E37 §8** (and `hsms/decode.go` accepts it), so an empty item must mean "validly empty,"
  never "decode failed." On success `Item()` returns a real `EmptyItem`/tree with a nil
  error; on failure it returns a nil (or explicitly-invalid) item with a non-nil error that
  **cannot be mistaken for valid empty content**. A `DecodeErr() error` convenience returns
  the cached error for callers that already triggered decode. The relay path
  (`ToBytes()`/`AppendBodyTo`, or the internal writev) still serves the raw bytes
  regardless, so a forward never has to decode.
- The protocol-level `Error()` (message-level error, e.g. an HSMS Reject) stays distinct
  from the decode error.
- The `errors.Join`-on-mutation accumulation is removed; there is no post-construction
  mutation to accumulate.

### 5.E Construction / mutation API (resolves the "no `Set*`" question)

The common case — *receive a `*hsms.DataMessage`, override the session id / wait bit,
forward it* — is expressed by derivation, which is strictly safer than v1's in-place
`Set*` (no aliasing, no race):

```go
// single envelope field — cannot violate HSMS validity, so value-returning + chainable
fwd := msg.WithSessionID(42)

// two envelope fields — chain
fwd := msg.WithSessionID(42).WithSystemBytes(sb)

// anything validity-sensitive (W-bit, stream/function, body) — validated builder
fwd, err := msg.Derive().SessionID(42).WaitBit(false).Build() // returns (msg, error)
```

`msg` is never altered, so any other consumer holding it still sees the original. Scope:

- **Cheap chainable `With*` is restricted to the always-valid envelope fields** —
  `WithSessionID`, `WithSystemBytes`/`WithID`. These cannot produce a protocol-invalid
  message, so they return a value directly with no error.
- **W-bit, stream, function, and the body are validity-sensitive and go through
  `Derive()…Build() (msg, error)`.** Critically, the wait bit is **not** a free envelope
  field: SEMI **E37 §8.3.3.3** requires reply (even-function) messages to carry W-bit = 0,
  and `sanityCheck` already rejects W-bit on even functions (`hsms/data_msg.go:580-582`). A
  value-returning `WithWaitBit(true)` could mint an immutable *invalid* reply with nowhere to
  surface the error — so W-bit changes must flow through the validated builder. ("Change =
  new message," same rule as stream/function/body.) The eqp-hub relay case (override session
  id) stays cheap; toggling W-bit is rarer and validated.
- **Migration is near-mechanical:** `msg.SetSessionID(42)` (statement) →
  `msg = msg.WithSessionID(42)` (assignment) for envelope fields; `msg.SetWaitBit(false)`
  → `msg, err = msg.Derive().WaitBit(false).Build()`.

One cost knob to settle in sub-project 2: whether `DataMessage` is a **value type** (small
`header + *body`; `With*` is stack-allocated and chains alloc-free, but interface
assignment boxes it) or a **pointer type** (`*DataMessage`; interface-friendly, but each
`With*` heap-allocates the ~24-byte envelope). Either way the **body is never copied**;
only the envelope cost differs. Benchmarked in the bake-off.

### 5.F Pooling — public API removed (locked); internal extent benchmark-gated

Two separable questions, decided separately (D7):

1. **Public pooling API — removed unconditionally (correctness, locked now).** No public
   `Free`, no `usePool` global, no caller-visible ownership/transfer contract. The driver is
   not performance but **safety under concurrent fan-out**: once an immutable message is
   shared by reference to N downstreams (handlers, shadows, relay destinations), no single
   caller can know when the *last* reader is done, so there is no correct place to call
   `Free`. v1's answer — explicit ownership transfer (`queueSendRequest` transfers on
   success; callers must not `Free`) plus atomic double-`Free` guards and a build-tagged
   content-aware double-`Free` detector — is exactly the recurring bug surface this removes.
   Messages and items become **GC-owned**; lifetime is the garbage collector's job.

2. **Internal pooling extent — benchmark-gated (performance, sub-project 4).** `sync.Pool`
   may be retained **purely internally**, invisible to the public API, **only where the
   bake-off proves a win** — candidate sites are decode scratch buffers, the 128 KB I/O read
   buffer, and timers. How much survives is a measurement call (Section 3.7), but it can
   never resurrect a public `Free`/ownership contract.

**Constraint — pooled read buffers vs zero-copy decoded frames are mutually exclusive.** A
decoded message owns its raw frame and leaf items may zero-copy-reference it (5.B); that is
only safe if the frame is **GC-owned**. Today recv allocates a fresh `make([]byte, msgLen)`
(`hsmsss/message_reader.go`), which is GC-owned and safe to retain. If sub-project 4 instead
**pools** the read buffer (`hsms/pool.go` returns buffers for reuse), a zero-copy frame
backed by that buffer would be corrupted on reuse. So sub-project 4 must pick one per the
benchmark, not both: either **(a)** pooled read buffers are *scratch-only* and the body is
**copied into an owned frame** before any sharing/zero-copy view; or **(b)** decoded frames
are zero-copy and the read buffer is **not** pooled (GC-owned per message). Benchmark (a)
copy-cost vs (b) GC-cost separately; do not silently enable both.

### 5.G The benchmark gate (sub-project 0, runs first)

A harness measuring **throughput, p50/p99 latency, allocs/op, and GC pause** across two
workload arms:

- **Relay-heavy:** decode → restamp header → forward, no structural access (eqp-hub's
  profile).
- **Structural-heavy:** decode → inspect items → logic (gem / equipment-side profile).

It prototypes the hybrid body model (raw-view vs item-tree) for one large type
(List/ASCII) and one scalar (Int), benchmarks the memoization mechanism (`sync.Once` vs
`atomic.Pointer` vs mutex), benchmarks value-vs-pointer `DataMessage`, and **reproduces
the known `SnapshotForRelay` relay-win / structural-loss result** to validate the harness
against reality. Its verdicts gate D2's per-type strategy, 5.A's memoization, 5.E's
envelope type, and 5.F's pooling decision.

---

## 6. Program decomposition

The effort is eight dependency-ordered sub-projects. Each gets its own
spec → plan → implementation cycle; this document is the umbrella proposal.

| # | Sub-project | Depends on | Status |
|---|-------------|------------|--------|
| 0 | **Benchmark / measurement harness + bytes-as-truth bake-off** | — | **Active (gate)** |
| 1 | **`secs2` immutable Item core** (Set*/Clone/Free removed, unified caching, error model, config-not-globals) | 0 | Active |
| 2 | **`hsms` immutable message + message-level header/body split** — covers **`DataMessage` and `ControlMessage`** (new header over shared body; race-free lazy decode returning `(Item, error)`; **transport-neutral body byte/chunk interface** that serves HSMS framing *and* SECS-I ≤244-byte block split/reassembly; vectored prefix-then-body send replacing the `ToBytes()` concat copy, benchmark-proven) | 1, 0 | Active |
| 3 | **`sml` parser/encoder adaptation** (all-at-once construction; strict-mode via config) | 1 | Active |
| 4 | **Internal allocation strategy** (public `Free`/pool already removed per D7; this only tunes *internal* `sync.Pool` extent — keep `sync.Pool` only where measured to win) | 0, 1, 2 | Active |
| 5 | **Connection / state / error rearchitecture** (collapse dual-state; unify send entry points; context-based gating; shared-core-vs-parity for hsmsss+secs1) | 2 | **Deferred (D4)** |
| 6 | **Integration-test architecture** (`net.Pipe` deterministic, T1–T8 time injection, (prev,next) state matrix, fuzz) | 5 | **Deferred** |
| 7 | **`gem` + public API + `/v2` migration / versioning + codemod** | 1, 2, 5 | Deferred |

Sequencing: **0 → 1 → (2 ∥ 3) → 4**, then the deferred 5 → 6 → 7 once the message model
is stable and benchmarked.

**SECS-I scope split (important):** the *immutable body model* affects SECS-I and must be
co-designed, but the *connection rewrite* does not. So the **transport-neutral body
byte/chunk interface** (block split/reassembly needs) is part of **sub-project 2** and is
validated against SECS-I *before* `DataMessage` is frozen; the SECS-I **connection / state /
block-transfer rewrite** stays in the deferred **sub-project 5**. This honors D4 (defer
connection work) without letting the body model be frozen blind to SECS-I.

---

## 7. Migration path

- **Module path:** `github.com/arloliu/go-secs/v2`, parallel-installable with v1, so
  consumers migrate on their own schedule; v1 enters maintenance/freeze.
- **Codemod:** a documented, mostly-mechanical `Set*` → `With*` transform
  (`msg.SetSessionID(x)` → `msg = msg.WithSessionID(x)` or inline at forward sites).
- **D7 delete-only migration:** v1 `defer msg.Free()`, `item.Free()`, `hsms.UsePool`,
  `secs2.UsePool`, and every pool toggle / ownership-transfer dance are **deleted with no
  public replacement** — lifetime becomes GC-owned and internal pooling is not
  caller-configurable. This is removal, not rewrite: callers simply drop these lines.
- **`gem` builder API** over immutable messages preserves the high-level construction
  ergonomics consumers rely on.
- **Stable public surface** pinned for `Connection`, `Session`, `Item`, and the message
  types, aligned with the forward-path needs in the downstream proposal before freeze.

---

## 8. Risks & open questions (carried into sub-project specs)

- **R1 — bytes-as-truth regressing structural consumers.** Mitigated by D2 (hybrid by
  provenance) + the bake-off gate; bytes-as-truth is never the default for constructed
  messages.
- **R2 — monolithic blob regressing per-hop forward.** Mitigated by 5.C: new header over
  shared body + `net.Buffers`, so restamp is O(14) not O(body). Forward-path benchmark is
  a release gate for sub-project 2.
- **R3 — removing *internal* pooling inflating GC.** (The *public* `Free`/pool API is removed
  unconditionally per D7 — a correctness call; this risk is only about how much internal
  pooling to keep.) Unmeasured today (Section 3.7); resolved by the
  bake-off, not assumed. Pools retained internally where they demonstrably win.
- **R4 — lazy decode reintroducing a race.** Mitigated by the race-free memoization
  (5.A/5.B) chosen by benchmark; the v1 `snapshotMessage` non-thread-safety is explicitly
  fixed.
- **R5 — value-vs-pointer `DataMessage`.** Open; benchmarked in sub-project 2.
- **R6 — `hsmsss`/`secs1` parity drift during a long v2.** Acknowledged; addressed only
  when sub-project 5 is undertaken (shared-core vs enforced parity matrix is itself an
  open question deferred with D4).
- **Q1 — structural sharing for item transforms.** When a body *is* transformed (rare on
  forward, common in adapters), can the new tree structurally share unchanged subtrees, or
  is it always a fresh build? Sets the cost ceiling for transforming forwards; decide in
  sub-project 1/2.

---

## 9. Success criteria

v2's message model is accepted when, on the benchmark harness (sub-project 0):

1. **Non-transforming forward** (decode → restamp session id → forward) drops from
   *N body clones* to *0 clones + 1 serialize + share*, with allocs/op and GC pause at
   **parity-or-better** vs v1's `SnapshotForRelay`.
2. **Structural-access** workloads (decode → inspect) are **at parity-or-better** vs v1's
   `Clone` path — the hybrid model must not regress them.
3. **Concurrent fan-out** to N consumers is race-free under `-race` with **zero** clones,
   and the data race in `secs2.ListItem.ToBytes` (Section 3.1) is provably gone (a
   regression test with teeth).
4. The public `secs2`/`hsms`/`secs1` surface compiles with **no mutating method**, **no
   `Free`/pool**, and **no `usePool` global** in any exported signature (D7) — verified by a
   grep/`go doc` check in CI, so internal pooling can never leak back into the public API.

---

## 10. Appendix — key current-code anchors

- Immutability contradiction: `secs2/item.go:79-92` (doc) vs `secs2/list.go:161-187`
  (`SetValues`), `:195-218` (racy lazy `ToBytes`), `:237-246` (`Clone`).
- HSMS header mutation: `hsms/data_msg.go:157` `SetSessionID`, `:195` `SetSystemBytes`,
  `:245` `SetHeader`; `:347` `ToBytes`; `:453` `Free`; `:483`/`:517`/`:553`
  `Clone`/`CloneCodec`/`SnapshotForRelay`; `:180-186` use-after-`Free` note.
- Header/body prototype: `hsms/fanout_snapshot.go:19` (`snapshotMessage`), `:32-49` (lazy
  decode), `:73` (in-place header patch), `:17-18` (not concurrency-safe).
- Pooling: `secs2/pool.go:176` (`usePool` global), 8 item pools; `internal/pool/timer_pool.go`
  (timer pool); 128 KB message-buffer pool in `hsms`.
- Allocation accounting: `tmp/hsmsss-hotpath-perf-analysis-revised.md` (~9–11 allocs /
  sync round-trip — **static estimate, not profiled**; to be confirmed in sub-project 0).
- Dual connection stacks: `hsmsss` (`atomic.Pointer[ctxHolder]`, three send entry points,
  `IsSelected` gate at three sites) vs `secs1` (`sendMu > ctxMutex`, gate-reset outside
  `createContext`).

---

## 11. SEMI standard references

The normative source of truth during the rewrite. Local copies (extracted markdown) live
under `~/semi_standards/markdowns/`. Cite section numbers — not page numbers — since these
are stable across editions. Verify any protocol behavior against these before encoding it.

| Standard | File | Verify against it when working on |
|----------|------|-----------------------------------|
| **SEMI E5** — SECS-II (Message Content) | `e005-00-0813/e005-00-0813.md` | sub-projects 1, 2, 3 |
| **SEMI E37** — HSMS (High-Speed SECS Message Services) | `e037-00-0413/e037-00-0413.md` | sub-projects 2, 5 |
| **SEMI E4** — SECS-I (Message Transfer) | `e004-00-0699-0612r/e004-00-0699-0612r.md` | sub-projects 2 (body/block interface), 5 |

**Section map (the parts this v2 actually depends on):**

- **E5 / SECS-II** — `§9 Data Structures` (item format codes, length-byte encoding, list
  structure) and `Table 3 Data Item Dictionary` are the contract for the immutable
  `secs2.Item` model (5.A, sub-project 1). `§6 Message Transfer Protocol`, `§7 Streams and
  Functions`, `§8 Transaction and Conversation Protocols`, and `§10 Message Detail` define
  message/SF semantics consumed by `sml` (sub-project 3) and `hsms` (sub-project 2).
- **E37 / HSMS** — `§8 HSMS Message Format` is the reference for the 10-byte header
  (session id, byte 2 = W-bit+stream, byte 3 = function, byte 4 = PType, byte 5 = SType,
  bytes 6–9 = system bytes), the 4-byte length prefix (header + text; depends on length not
  contents — this is *why* a header restamp shares the body, 5.C), the control-message
  formats (Select/Deselect/Linktest/Separate/Reject), and `Table 8 Deselect Status` /
  `Table 9 ReasonCode` (immutable `ControlMessage`, sub-project 2). Header-only data messages
  are permitted here — see the empty-vs-decode-error distinction in 5.D. `§5 Overview & State
  Diagram` + `Table 1 State Transition`, `§7 Message Exchange Procedures`, `§6 Use of TCP/IP`,
  and `§9 Special Considerations` (T3–T8) govern the deferred connection rewrite (sub-project 5).
- **E4 / SECS-I** — `§7 Block Transfer Protocol` (ENQ/EOT/ACK/NAK, contention, the 244-byte
  max block-data size), `§8 Header Structure` (device id, block number, E-bit, system bytes),
  and `§9 Message Protocol` (multi-block assembly, T1–T4) define how the **shared immutable
  body** is chunked and reassembled — the transport-neutral body interface in sub-project 2
  must satisfy these. `R1-5 System Bytes` is relevant to system-bytes/transaction handling on
  the relay path. The full block-transfer/connection rewrite is sub-project 5.
