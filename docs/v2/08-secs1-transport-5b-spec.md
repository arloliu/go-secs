# Sub-Project 5b — SECS-I (SEMI E4) Transport on the Shared Connection Core

**Status:** SPEC (pre-plan). Base = `v2` tip after SP5a (`74dd23a`). Depends on: SP5a shared
connection core (`hsms` engine + `transport`/`TransportRuntime` seam), SP2 `secs1` immutable
block-framing layer.

**Grounding docs:** `docs/v2/07-connection-core-hsmsss-5a-spec.md` (the shared-core spec and the
sealed-A seam), `docs/v2/04-secs1-block-chunk-2b-spec.md` (the SP2 framing layer), the SEMI E4
standard at `/home/arlo/semi_standards/markdowns/e004-00-0699-0612r/e004-00-0699-0612r.md`, the
v1 `secs1` stack on the `main` branch (the port source), and the SP5b design-input map
(`scratchpad/sp5b-design-input.md`, from a 6-agent research workflow).

---

## 1. Scope & goal

SP5b delivers the **SECS-I (SEMI E4) connection/transport** for go-secs v2: the half-duplex
ENQ/EOT/ACK/NAK block protocol over TCP/IP, with T1–T4 timers, master/slave line contention,
per-block retry (RTY), multi-block message assembly, and T3 reply correlation — built **on the
proven SP5a shared core**, reusing `hsms.Connection` as the app-held interface and exposing a
single constructor `secs1.New(...) (hsms.Connection, error)`.

Concretely, SP5b:

- Implements the unexported `hsms.transport` seam (6 methods) as a concrete `secs1` transport,
  wired into the engine via the in-module `hsms.NewConnection(cfg, tr transport)` constructor and
  the cross-package compile assertion (exactly as `hsmsss/transport.go` does).
- Builds the SECS-I **byte/line-control engine** behind that seam: the per-block half-duplex
  transaction (Idle → line control → send/receive → completion → Idle, E4 §7.8) with
  ENQ/EOT/ACK/NAK, T1/T2, contention arbitration, and RTY.
- Builds the **message/transaction layer**: multi-block split/assemble over time, T4 inter-block
  policing, duplicate-block detection, and E-bit termination — reusing the SP2 immutable framing
  primitives (`block`, `splitBody`, `assembleBlocks`, `parseBlock`).
- Adds a `secs1.Config` embedding `hsms.ConnectionConfig` (shared-core settings) plus
  SECS-I-specific settings (T1–T4, RetryLimit, `IsEquip`/master-slave, DeviceID).
- Ports the v1 correctness suite (contention, RTY-exhaustion, T4 abort, duplicate-block,
  multi-block reassembly, single/multi-block round-trip) onto the new architecture.

**Out of scope:** any change to the SP5a engine's public contract or state model (see §5 — the FSM
is reused unchanged per the locked decision); the RS-232 physical transport (v2 SECS-I is
TCP-only, matching v1); HSMS-GS multi-session; `gem` integration (SP7); porting the v1 examples/
integration harness wholesale (SP6/SP7 — the M8 `go build ./...` v1-leftovers stay deferred).

**Prime directive (from SP5a §4/§5.4):** the connection-management engine lives **once** in the
`hsms` package. `hsmsss` (SP5a) and `secs1` (SP5b) are *thin* per-transport seam clients. SP5b
builds **no** FSM, reply-correlation registry, supervisor, or reconnect logic — the half-duplex/
full-duplex divergence (proposal K1) is absorbed by the seam, not by forking the engine. **SP5b
must not modify or fork the shared engine.**

---

## 2. Locked decisions

User-confirmed brainstorm decisions (2026-07-02), plus the recommendations SP5b adopts for the
remaining design points. These are binding for the plan; the Codex plan-review may refine wording
but not reverse a locked user decision without re-consulting.

- **D5b-1 (USER-LOCKED) — the ENQ/EOT/ACK line transaction runs INSIDE the seam's `Write`.**
  The core hands `Write(ctx, conn, bufs)` an **HSMS frame** (not SECS-I blocks); `secs1`'s `Write`
  runs the §4a send adapter (HSMS-header → SECS-I `messageHeader` + `splitBody`) and then the FULL
  line-control transaction for each resulting block: ENQ → wait EOT (T2) → write block → wait ACK
  (T2) → contention/RTY as needed → returns `nil` only when the LAST block is ACK'd, or an error on
  RTY-exhaustion / line failure. **An HSMS control message (SType≠0) is dropped with a `nil` return
  (§4a step 2) — SECS-I has no control plane.** The core keeps owning T3, reply-correlation, and
  metrics — it still "thinks" it did one send. The half-duplex divergence is confined to the seam.
  (Verified: `WriteMessage`/`sendWaitReply` already tolerate a `Write` that blocks — HSMS `Write`
  can already block on TCP backpressure — so a `Write` that blocks for the full `T2 × (RTY+1)`
  budget is contract-legal. **But the core's per-write `writeTimeout` must not preempt RTY — see
  D5b-11.**)

- **D5b-2 (USER-LOCKED) — the FSM is REUSED unchanged; `secs1` auto-commits `Selected` at TCPUp.**
  SECS-I has no Select layer ("connected == usable"). The `secs1` transport drives the shared
  E37 FSM to the send-gate state by calling `TransportRuntime.CommitSelected()` itself, from
  inside its `TCPUp` path, immediately after `TCPUp` commits `NotConnected → NotSelected`. No
  Select frame ever touches the wire. The send-gate (`IsSelected()`, B1/B3) and everything above
  it are reused verbatim. **Zero core change.** (Option B — collapsing the core FSM to
  Connected/NotConnected — was rejected: it forks the shared engine, the one thing SP5b must not
  do.) See §5 for the naming resolution.

- **D5b-3 (USER-LOCKED) — v1↔v2 wire compatibility is a strong SHOULD; v2↔v2 is a MUST.**
  Target byte-identical wire behavior with the v1 `secs1` stack (the SP2 golden block-vector
  already pins the block format). v2↔v2 active/passive interop is a hard acceptance criterion; a
  v1↔v2 interop smoke test is added if feasible (see §11 open item O3).

- **D5b-4 — T3 owned by the core; T1/T2/T4 owned by `secs1`.** The core reply registry/timeout
  owns T3 (reply correlation) exactly as for HSMS; D5b-1's "return from `Write` only after the
  last block is ACK'd" makes the core start T3 at the E4-correct instant automatically. T1
  (inter-character), T2 (protocol), and T4 (inter-block) have no core analog and live entirely
  inside `secs1` (private config fields + block/message-layer timers). `secs1.Config` maps T3 →
  the core reply timeout; T1/T2/T4 → `secs1`-private fields. See §7, §8.

- **D5b-5 — HSMS-only runtime methods are inert for SECS-I.** `secs1` never drives `SelectLost`
  or `T7Expired` (the core tolerates their absence — they are event injectors). It never sends a
  Select/Deselect/Linktest/Separate control frame, so `handleSelectReq`/`handleDeselectReq`/
  `handleLinktestReq`/`handleSeparateReq` (all `hsmsss`-only) are simply never in the `secs1`
  code path. `LinktestInterval()` returns `0` so the shared engine's auto-linktest path is inert
  (confirm D5a-5: `interval == 0` fully disables it — see O1). `SessionID()` is not on the E4
  wire; `secs1` supplies a benign value.

- **D5b-6 — master/slave role is `secs1.Config.IsEquip`, consumed only inside the seam.** Role is
  static (equipment = master, host = slave) and drives both the header R-bit direction and the
  contention winner. It is independent of the active/passive TCP role (a passive TCP listener can
  be equipment or host). The core is role-agnostic.

- **D5b-7 — framing symbols stay package-private; the connection code lives in `secs1`.** The SP2
  framing (`block`/`splitBody`/`assembleBlocks`/`parseBlock`) exports nothing but sentinel errors;
  SP5b adds its connection code IN the same `secs1` package (no promotion, no public-surface
  growth). `secs1.New` and `secs1.Config` are the only new exports.

- **D5b-8 — read buffers are owned per held block, never pooled.** `parseBlock`'s body aliases its
  `rest` argument until `assembleBlocks` coalesces it into an independent owned buffer; multi-block
  assembly holds several blocks at once. `secs1` allocates a fresh owned read buffer per retained
  block (mirroring the SP2 `splitParse` per-block pattern) and does NOT pool them (SP5a §5.F option
  (b): GC-owned frames, no read-buffer pool). Per-block buffers are freed only after
  `assembleBlocks` returns.

- **D5b-9 — contention resets RTY to 0.** E4 §7.8.2.1: the yielding slave resets its retry counter
  to 0 after a contention loss. v1 does this (`sendContention`); SP5b preserves it exactly, with a
  teeth-checked regression guard.

- **D5b-10 — RTY-exhaustion drives teardown+reconnect (resolves review O6, no core change).** The
  core treats ANY `tr.Write` error as a line problem: `writeFrame` calls `c.TCPDown(err)` on a
  seam-`Write` error (`connection_send.go:148`) — which marks the generation comms-failed and drives
  teardown → the reconnect loop re-dials. SP5b **accepts this contract**: an RTY-exhausted block
  (peer not ACKing after RTY retries) returns an error from `Write` → the caller sees the send fail
  AND the link tears down and reconnects. This is E4-defensible (a peer that will not ACK after RTY
  retries is effectively down) and, crucially, requires **no core change** (the prime directive). A
  "send-error-only, keep-the-link" model would require the core to distinguish a soft send failure
  from a dead line — a core-contract change explicitly out of SP5b scope.

- **D5b-11 — SECS-I disables the core per-write `writeTimeout` (resolves review P1).** The core arms
  `writeTimeout` (default 30s) around the whole seam `Write` (`connection_send.go:139`). A legitimate
  SECS-I line transaction can take up to `T2 × (RTY+1)` (default `10s × 4 = 40s`), which would let
  the 30s core deadline preempt RTY. `secs1.New` therefore sets **`writeTimeout = 0`** (disabled) —
  SECS-I's `Write` self-bounds every await with T2 and the RTY count, so the core write deadline is
  redundant and must not fire. (Setting `writeTimeout ≥ T2×(RTY+1)` would also work; `0` is cleaner
  since SP5b owns all send-side timing.)

- **D5b-12 — block numbering: outbound strict 1..N, inbound lenient {0,1} for a single block
  (resolves review P1).** E4 (§8.7 / §9.4.4.4) permits a **single-block** message to carry block
  number **0 OR 1**. SP2's `splitBody` emits E4-strict 1..N (single block = 1) on the SEND side —
  keep that (conformant, and what v1's block-numbering fix adopted). On the RECEIVE side, SP5b must
  **accept a lone E-bit block numbered 0 or 1** (for interop with implementations — including v1 —
  that send 0), normalizing before/inside `assembleBlocks`. Multi-block inbound stays strict
  (expected = previous+1, first = 1).

---

## 3. Architecture: what SP5b reuses vs. builds

### 3a. Reused UNCHANGED from the `hsms` shared core (do NOT re-implement in `secs1`)

| Concern | Provided by the core |
|---|---|
| Per-generation **epoch** (ctx/conn/writeMu/reply-registry), socket publish, bounded teardown+join | `epoch`, `connection_lifecycle.go` Open/reconnect |
| **Reconnect**: exponential Close-interruptible backoff capped at T5, generation serialization (`prev.wait()`), publishMu/reconnectGen/shutdown fences, ArmStart/Stop-seal | `connectLoop`, `startConnectLoop`, `reconnectSleep` |
| **Send/reply correlation registry** (sender-owned per-transaction channel, §5.5) | core registry + `RouteReply` seam |
| **Config container** + live-update discipline (`UpdateConfigOptions`, transactional) | `hsms.ConnectionConfig` (SP5b embeds it) |
| **Metrics** counters | `ConnectionMetrics` |
| **Open/Close** teardown ordering (`requestClose → e.wait → sup.stop`), bounded Close | `Close`, `Open` |
| **FSM + supervisor + notifier** (3-state E37 machine, `StateChangeHandler`) | `supervisor`, `CommitConnected`, `TCPDown` |
| **System-bytes generator** (monotonic, concurrency-safe) | `NextSystemBytes()` |
| **Farewell courtesy write** via the seam (bounded, cause-aware) | `writeFarewellSeparate` |
| **Session fan-out** of inbound primaries to `DataMessageHandler`s | `session`, `RouteData` |

The transport only reports lifecycle up/down and delivers assembled frames; epoch/generation
bookkeeping, reconnect, correlation, and the FSM are core-provided.

### 3b. Built INSIDE `secs1` (behind the seam)

- The **byte/line-control engine** (§6b): ENQ/EOT/ACK/NAK, T1/T2, per-block send/receive.
- **Contention arbitration** (§6c) + RTY (§6d), role from `IsEquip`.
- The **message/transaction layer** (§6e): multi-block split (have `splitBody`) / assemble (have
  `assembleBlocks`), T4 inter-block policing, duplicate-block detection, E-bit termination.
- `secs1.Config` (§7) and `secs1.New` (§7).
- The **transport lifecycle** (§9): `Start`/`Stop`/`ArmStart`, the per-generation recv/line-control
  loop, `TCPUp` (with the D5b-2 auto-commit) / `TCPDown`.

### 3c. Reused from the SP2 `secs1` framing layer (package-private, in-package)

- `block` value type (`header [10]byte` + `body wire.Chunk`), accessors (`deviceID`, `rBit`,
  `stream`, `waitBit`, `function`, `blockNumber`, `eBit`, `systemBytes`, `messageHeader`).
- `splitBody(body wire.Body, h messageHeader) (iter.Seq[block], error)` — ≤244 body bytes/block,
  block# 1..N, E-bit on last, zero-copy sub-views, empty body → one header-only block.
- `assembleBlocks(blocks []block) (messageHeader, wire.Body, error)` — validates contiguous 1..N,
  E-bit exactly on last, identical invariant header; returns an **independent owned** `wire.Body`
  (for the `DecodeOwned` zero-extra-copy path).
- `block.appendTo(dst)` — alloc-free encode with in-place checksum; `parseBlock(lengthByte, rest)`
  — validates length range/agreement/checksum, body **aliases `rest`** (D5b-8 ownership).
- Golden wire vector pinned (`block_test.go`); checksum = 16-bit arithmetic sum of header+body,
  **length byte excluded**, big-endian.

---

## 4. The transport seam SP5b implements

`hsms.transport` — unexported type, exported methods, implemented cross-package via the compile
assertion (`hsmsss/transport.go` pattern). **Six methods:**

| Method | `secs1` obligation |
|---|---|
| `Start(ctx, rt TransportRuntime) error` | Dial (active) / listen+accept (passive); bind `rt` **write-once** (F7); spawn the per-generation recv/line-control loop; **must not block on a peer** (Open calls Start synchronously — passive accept runs on a goroutine, `OpenBackground`). Dial/listen failure → return err → core reconnect retries (exponential backoff capped at T5). |
| `Stop(ctx) error` | Seal the Add-vs-Wait guard, close the socket (J5 — unblocks a wedged reader), then bounded-join the per-generation goroutines; `ctx` expiry → `ErrCloseTimeout` + abandon straggler. Mirror `hsmsss` `Stop` (per-generation `genWG` bundle, NEW-1). |
| `ArmStart()` | Clear the Stop-seal + install a **fresh** per-generation join bundle. Called by the core *before* it publishes each generation. |
| `Write(ctx, conn net.Conn, bufs net.Buffers) error` | Receives an **HSMS frame** from the core; runs the §4a send adapter (drops control frames; splits a data frame into SECS-I blocks) then hands off to the line engine (§9), which runs the full ENQ/EOT/ACK/RTY/contention transaction on the epoch `conn` (never re-resolve — I1). Returns `nil` only on ACK of the LAST block; error on RTY-exhaustion / line failure (→ core `TCPDown`, D5b-10). |
| `SetReadDeadline(conn, t)` / `SetWriteDeadline(conn, t)` | Conn-bound (I1 symmetry). SECS-I mostly arms its own T1/T2 deadlines on its captured conn inside the line engine; keep these for seam symmetry and any core-armed write deadline. |

### Callbacks `secs1` drives on `TransportRuntime`

**Generic (SECS-I drives):** `TCPUp(conn)` (then D5b-2 auto-commit), `TCPDown(cause)`,
`DeliverOwnedFrame(frame []byte) error` (the REASSEMBLED multi-block SECS-II frame, ownership
transferred), `RouteReply(msg) bool`, `RouteData(*DataMessage) error`, `WriteMessage`/`SendAsync`
(core-owned send entry — routes into the seam `Write` per D5b-1), `State()`, `Done()`, `Timers()`
(T3 = reply timeout; see §8), `NextSystemBytes()`.

**HSMS-only (SECS-I does NOT drive):** `SelectLost()`, `T7Expired()` (never injected);
`CommitSelected()` **is** driven — but for the D5b-2 auto-commit, not a Select handshake;
`SessionID()`, `LinktestInterval()` (→ 0), `LinktestFailThreshold()` supply benign/inert values.

### 4a. The HSMS↔SECS-I frame adapter (RESOLVES review P0-1 / P0-2)

The core is HSMS-framed end to end: `sendWaitReply → writeFrame → buildFrameBuffers` hands the
seam's `Write` an **HSMS frame** (`net.Buffers` = `[4-byte length][10-byte HSMS header][body]`),
and `DeliverOwnedFrame([]byte)` expects a `[10-byte HSMS header || body]` frame to decode. SECS-I
is NOT pre-framed by the core — the earlier "Write receives pre-framed SECS-I blocks" wording was
wrong. So `secs1`'s `Write` and its receive path each run a small **header adapter** between the
core's HSMS representation and the SECS-I wire.

**Header alignment (why the adapter is cheap):** the SECS-II message identity lives at IDENTICAL
offsets in both 10-byte headers — **byte 2 = W-bit|stream, byte 3 = function, bytes 6–9 = system
bytes** (verified: `hsms/data_msg.go:103/106/110/63` vs `secs1/block.go:66-72`). Only the
framing-specific bytes differ: HSMS bytes 0–1 = Session ID and bytes 4–5 = PType/SType, vs. SECS-I
bytes 0–1 = R-bit|deviceID and bytes 4–5 = E-bit|blockNo.

**Send adapter (`Write`, HSMS frame → SECS-I blocks):**

1. Strip the 4-byte length prefix; read the 10-byte HSMS header + body sub-slices.
2. **Control-message guard (RESOLVES P0-2):** if the HSMS SType (header byte 5) ≠ 0, the message is
   an HSMS control frame (Select/Deselect/Linktest/Separate/Reject). SECS-I has **no control
   plane**, so `Write` **returns `nil` WITHOUT touching the wire** (drops it). This is exactly what
   neutralizes the core's graceful-close `writeFarewellSeparate` (an HSMS Separate.req, SType 9,
   `connection_lifecycle.go:263`) — no HSMS control bytes ever leak onto the SECS-I line, and
   because the farewell is best-effort/errors-ignored, a `nil` return lets teardown proceed cleanly.
3. For a data message (SType 0): build a `secs1.messageHeader{deviceID (config), stream
   (byte2&0x7F), function (byte3), waitBit (byte2&0x80), systemBytes (bytes6-9)}` + R-bit (from
   `IsEquip`), then `splitBody(body, messageHeader)` → `iter.Seq[block]` (E-bit/blockNo added by
   `buildHeader`).
4. Line-transact each block in order (§6b) on the single line-engine goroutine (§9); return `nil`
   only when the LAST block is ACK'd; error on RTY-exhaustion (§6d / D5b-10).

**Receive adapter (recv path, SECS-I blocks → HSMS frame → `DeliverOwnedFrame`):**

1. Reassemble the inbound blocks (§6e) → `assembleBlocks` → (`secs1.messageHeader`, owned
   `wire.Body`).
2. Build a `[10-byte HSMS header || body]` **owned** frame: bytes 0–1 = Session ID (0xFFFF
   sentinel, O5); byte 2 = W-bit|stream, byte 3 = function, bytes 6–9 = system bytes (copied
   straight from the SECS-I header — same offsets); bytes 4–5 = 0 (PType = SECS-II, SType = data).
   Append the body.
3. `rt.DeliverOwnedFrame(frame)` — the core decodes zero-copy and routes primary→`RouteData` /
   secondary→`RouteReply` via the shared registry (SP5b owns no correlation).

The adapter is a header-byte remap + `splitBody`/`assembleBlocks` (both already built, SP2). Cost
accounting (review P2): the SEND side is copy-free (the HSMS body sub-slices flow straight into
`splitBody`'s zero-copy block views). The RECEIVE side needs ONE contiguous `[10-byte HSMS header
|| body]` buffer for `DeliverOwnedFrame` — `assembleBlocks` already coalesces the blocks into one
owned body buffer, so the frame is that buffer with the 10-byte header prepended. The plan should
have `assembleBlocks` (or a secs1 variant) **reserve 10 leading bytes** so the synthesized HSMS
header is written in place — making the whole receive path a single allocation with no second body
copy. If that reservation is not adopted, the fallback is one extra `header || body` concatenation
(acceptable, one copy per inbound message).

---

## 5. FSM resolution (D5b-2): auto-commit `Selected` at TCPUp

The core supervisor is E37-shaped: `NotConnected → NotSelected → Selected`, and the send-gate keys
on `Selected`. SECS-I has no Select layer; once TCP is up both ends are ready. **Resolution
(locked):**

1. On TCP established, `secs1`'s line loop calls `rt.TCPUp(conn)` — the core commits
   `NotConnected → NotSelected` synchronously (`CommitConnected` guarded CAS).
2. **Immediately, on the same goroutine, `secs1` calls `rt.CommitSelected()`** — the core commits
   `NotSelected → Selected` via the same guarded CAS the H2 Select responder uses, with **no
   Select frame on the wire**. Because this is synchronous and on the recv/line goroutine before
   any send is dispatched, `State() == Selected` (the send-gate is open) deterministically before
   the first send — NO async `ToSelectedAsync` race like v1.
3. Sends gate on `IsSelected()` exactly as HSMS. `SelectLost`/`T7Expired` stay dead.

**Naming resolution (D5b-2 guardrail, no enum change):** document that for a link-only transport
(`secs1`) the `Selected` state means **"link established and usable"** (there is no Select
procedure). Add this to the `secs1` package `doc.go` and a one-line note at `ConnState`'s
`SelectedState` godoc pointing out the two readings (HSMS: Select complete; SECS-I: link usable).
**No change to the `ConnState` enum or the send-gate predicate** — the public SP5a surface is
untouched. (A neutral `StateReady` alias was considered and rejected as a public-surface change
for a naming nicety; revisit only if plan-review deems the overload unacceptable.)

**Correctness note (H2 analog):** the SP5a H2 invariant — commit Selected SYNCHRONOUSLY on the
recv goroutine before dispatching an inbound frame that could be Rejected for `NotSelected` — is
satisfied trivially here: the auto-commit happens at TCPUp, strictly before the recv loop reads
any block, so no inbound SECS-I block is ever seen while `NotSelected`.

---

## 6. The SECS-I protocol (mapped onto the SP2 block layer)

All wire-format work is done by SP2; SP5b adds the **stateful line and message engine** around it.

### 6a. Block/wire format (have it — SP2, E4 §6/§8)

Wire block = `[length(1)][header(10)][body(0–244)][cksum_hi][cksum_lo]`; `length = 10 + len(body)`,
range 10–254. Checksum = 16-bit arithmetic sum of header+body (**length byte excluded**),
big-endian. Header (E4 §6.4): byte0 = R-bit(0x80) | deviceID-hi(0x7F); byte1 = deviceID-lo; byte2 =
W-bit(0x80) | stream(0x7F); byte3 = function; byte4 = E-bit(0x80) | blockNumber-hi(0x7F); byte5 =
blockNumber-lo; bytes6–9 = system bytes. **No byte-stuffing** (TCP transport). Encode
`block.appendTo`; decode `parseBlock`. **Block numbering 1..N** (header-only message = block 1;
E4-strict, noted in the SP2 spec as a v1 conformance fix — v1 used 0).

### 6b. Line control (build — E4 §7.8, control chars ENQ=0x05, EOT=0x04, ACK=0x06, NAK=0x15)

Per-block half-duplex handshake; both ends start Idle.

- **Send one block** (v1 `sendBlock`/`lineControlAndSend`/`sendBlockData`): write **ENQ** → await
  **EOT** within **T2** (per role, ignore stray chars — see §6c) → write the packed block bytes →
  await a single response byte within **T2**: **ACK** → block sent; **anything else / T2 timeout**
  → treat as NAK → retry (§6d). *(Contention: receiving ENQ instead of EOT after our ENQ → §6c.)*
- **Receive one block** (v1 `receiveBlock`): on reading **ENQ**, send **EOT** → read the length
  byte within **T2**, then read `header+body+cksum` (length−? — read `length+2` bytes total after
  the length byte, i.e. 10-byte header + body + 2-byte checksum) with **T1** per read →
  `parseBlock` validates length range/agreement/checksum → on success write **ACK** and hand the
  block to the message layer; on bad length/checksum/T1 gap → `drainUntilSilence` (read until a T1
  read gap) then write **NAK** (the peer retries).
- Both paths run on the **single per-generation line-engine goroutine** (§9, the O4 resolution):
  the send path is entered via the seam `Write`'s hand-off (D5b-1/§4a), the receive path on an
  inbound ENQ. One goroutine is the sole reader/writer of the half-duplex conn.

### 6c. Contention arbitration (build — E4 §7.8.2.1)

Both ends may write ENQ simultaneously. Resolution is by **static role** (`IsMaster == IsEquip`):

- **Master (equipment)** wins: after writing ENQ it ignores everything but **EOT**; on receiving a
  contending ENQ it does NOT yield — it keeps waiting for EOT and proceeds to send.
- **Slave (host)** yields: on receiving a contending ENQ (instead of EOT) after its own ENQ, it
  abandons its send attempt, sends **EOT**, **receives the master's block first** (§6b receive),
  then re-issues its own send as a fresh transaction — with **RTY reset to 0** (D5b-9).
- While Idle, a master ignores all but ENQ/EOT; a slave ignores all but ENQ/EOT. Role is fixed for
  the connection's life (from `IsEquip`), independent of the TCP active/passive role.

### 6d. Retry / RTY (build — E4 §7.4, §7.8.2.2)

`RetryLimit` (RTY) = max retransmissions per block (0–31, default 3). A T2 timeout, no-EOT,
no-response, or non-ACK response increments the per-block retry count; count ≤ RTY → retry from
ENQ; count > RTY → **failed send** → return `ErrSendFailed` from `Write` (D5b-1) → the core surfaces
it as the send error and (for a persistent line failure) `TCPDown` drives teardown/reconnect.
**Contention loss resets the count to 0** (§6c, D5b-9).

### 6e. Message / transaction layer (build — on SP2 split/assemble)

- **Send (outbound):** `splitBody(body, header)` → `iter.Seq[block]`; emit each block through the
  line engine (§6b) in order; the transaction succeeds only when the **last** block (E-bit set) is
  ACK'd — at which point `Write` returns `nil` and the core starts T3 (D5b-1/D5b-4). A block that
  RTY-exhausts fails the whole message.
- **Receive (inbound):** accumulate parsed blocks per open inbound message, keyed by the invariant
  header + system bytes; the FIRST block is number 1 (**or 0 for a lone E-bit single block**, D5b-12
  interop leniency), thereafter **expected block number = previous + 1** (E4 §9.4.4); enforce
  **T4** between successive blocks (expiry → cancel the partial message, discard, `TCPDown` not
  required — a partial-message abort is recoverable); on the **E-bit** block, `assembleBlocks` →
  independent owned `wire.Body` → build the owned frame → `rt.DeliverOwnedFrame(frame)` (which
  decodes zero-copy and routes primary→`RouteData` / secondary→`RouteReply`, reusing the SP5a
  reply registry — SP5b does NOT own correlation).
- **Duplicate-block detection** (E4 §9.4.2): a re-sent block (peer missed our ACK — same full
  10-byte header) is **ACK'd again but not re-delivered / not re-appended**.
- **Reply correlation:** system bytes link a reply to its primary; delegated entirely to the core
  `RouteReply` (the shared registry). T3 is the core reply timeout.

### 6f. Direction / R-bit

R-bit sets direction (0 = to equipment, 1 = to host). SP5b sets the R-bit on outbound headers and
validates it on inbound per `IsEquip`; the SP2 layer only decodes `rBit()`.

---

## 7. `secs1.Config` and `secs1.New`

- `secs1.Config` **embeds `hsms.ConnectionConfig`** (shared-core settings: T3/T5 (reconnect
  backoff)/T6/T7/T8 where applicable, closeTimeout, writeTimeout, logger, metrics, handlers via the
  connection). Adds SECS-I-specific fields with functional options (no package globals, mirroring
  the SP5a/sml convention):
  - `T1` (inter-character, default 0.5s), `T2` (protocol, default 10s), `T4` (inter-block, default
    45s) — `secs1`-private (D5b-4). **T3** (reply, default 45s) maps onto the core reply timeout.
  - `RetryLimit` (RTY, default 3, range 0–31).
  - `IsEquip bool` (master/slave role, D5b-6) — default host (slave) unless set; document the
    equipment side sets it.
  - `DeviceID uint16` (E4 header device id, 0–0x7FFF).
  - active/passive TCP role + host:port (like `hsmsss.Config`).
- `secs1.New(opts ...Option) (hsms.Connection, error)` — builds the `secs1` transport, constructs
  the engine via `hsms.NewConnection(&cfg.ConnectionConfig, tr)` (the core takes a
  `*ConnectionConfig`; embed by value + pass its address, mirroring `hsmsss.New`), returns the `hsms.Connection`
  interface. Validation returns `(nil, error)` on bad config (mirrors `hsmsss.New`).
- **Timer mapping:** `secs1.Config` fills the `hsms.ConnectionConfig.timers` such that
  `Timers().T3` is the SECS-I reply timeout; T1/T2/T4/RetryLimit/IsEquip/DeviceID are read only
  inside the `secs1` transport (the core never sees them). `LinktestInterval() == 0` (auto-linktest
  inert). **`writeTimeout == 0`** (disabled — SECS-I's `Write` self-bounds via T2×RTY; D5b-11).

---

## 8. Timer ownership & T3 correctness (D5b-4)

| Timer | Meaning (E4) | Owner | Mechanism |
|---|---|---|---|
| **T1** | inter-character (per read while receiving a block) | `secs1` | `conn.SetReadDeadline(now+T1)` per read in the block reader / `drainUntilSilence` |
| **T2** | protocol (ENQ→EOT, EOT→length, block→response) | `secs1` | read deadline armed around each line-control await |
| **T3** | reply (last-block-on-wire → first-reply-block) | **core** | the shared reply registry timeout; D5b-1 makes `Write` return only after the last block ACK, so the core starts T3 at the E4-correct instant |
| **T4** | inter-block (between received blocks of one inbound message) | `secs1` | a per-open-inbound-message deadline; expiry discards the partial |

The critical correctness point: **T3 must start when the last primary block is on the wire, not at
enqueue.** D5b-1's "`Write` blocks until the last block is ACK'd, then returns" gives the core the
correct T3 start instant for free — `sendWaitReply` registers the reply channel and then calls
`writeFrame`→seam `Write`; the reply-wait timer (T3) effectively runs from `Write`'s return. (Plan
must confirm the exact `sendWaitReply` sequencing so T3 does not start before the last block ACK —
see open item O2.)

---

## 9. Transport lifecycle (mirror `hsmsss`, no reconnect logic)

Reuse the SP5a `transport_active.go` / `transport_passive.go` / `transport_recv.go` structure as
the template — but SECS-I has **no Select procedure**, so `startActive` does NOT run a
`runSelectProcedure`; instead:

- **`Start` (active):** ctx-aware dial (`DialContext`, I6); on success capture the conn, `TCPUp` +
  auto-commit Selected (§5), spawn the per-generation **line-engine goroutine** (below; registered
  on the fresh `genWG`, NEW-1). Return without blocking.
- **`Start` (passive):** listen; the accept runs on a goroutine (`OpenBackground`, tracked by
  `genWG.accept`); on accept, `TCPUp` + auto-commit, spawn the line-engine goroutine.
- **`Stop`/`ArmStart`:** identical discipline to `hsmsss` (seal → closeSocket → bounded join of the
  line-engine goroutine; fresh `genWG` per generation). closeSocket unblocks the engine's blocking
  read (J5).
- **Reconnect, Open, Close, teardown ordering:** entirely core-provided (§3a); SP5b writes none of
  it.

**Single line-engine goroutine — the O4 resolution (REQUIRED architecture, not plan-deferred).**
Review P1 was right that a line-ownership mutex + an independent recv loop is unsafe: two
goroutines reading the same half-duplex stream would steal each other's ENQ/EOT/ACK bytes. So:

- **Exactly ONE goroutine per generation owns the conn as its sole reader AND writer** (the "line
  engine"). Unlike HSMS (full-duplex — a recv loop reads while senders write concurrently under
  `writeMu`), SECS-I is half-duplex: at most one line transaction (send OR receive) is in flight,
  and §6c contention arbitrates when both ends want the line.
- **The seam `Write` does NOT read or write DATA bytes on the conn** (only the line engine does I/O
  on the half-duplex stream — that is what prevents two readers stealing each other's bytes). It
  hands off a send-request (the adapted SECS-I blocks, §4a) to the line engine over a per-generation
  `sendReqCh` and blocks on a per-request done channel until the engine reports ACK / error. (The
  one exception is the optional `SetReadDeadline(now)` wake-poke below, which arms a deadline but
  reads/writes no bytes — see the mechanism note.) **The `sendReqCh` is NEVER closed** (a `Write`
  sending on a closed channel would panic — the SP5a sender-owned-channel discipline). Teardown is
  signalled by a per-generation broadcast `genDone chan struct{}` (closed by `Stop`); the `Write`
  selects on `genDone` for BOTH the hand-off and the wait, and each `done` is buffered (cap 1) so the
  engine never blocks delivering a result to an abandoned writer. A stale-epoch `Write` (I1) whose
  generation's engine has stopped observes `genDone` and returns `ErrConnClosed` — mirroring the HSMS
  nil-conn path.
- **Line-engine loop:** while idle it watches BOTH the conn (for an inbound **ENQ** — the peer
  initiating) and `sendReqCh` (for an outbound request):
  - inbound ENQ → run the receive path (§6b) → feed the message layer (§6e).
  - pending outbound request → run the send path (§6b) with §6c contention handling + §6d RTY;
    signal the request's result (nil on the LAST block ACK).
  - On a read/line error → `TCPDown(cause)` (generation-ctx-guarded, C1 straggler discipline) and
    exit.
- **The idle "watch both" MECHANISM is a plan-level choice** (a short read-deadline poll, or a
  `SetReadDeadline(now)` "poke" from the `Write` goroutine to interrupt a blocking read so the engine
  checks `sendReqCh`); the **architecture** — one conn-owning goroutine, `Write` hands off — is fixed
  here. This is SECS-I's equivalent of the SP5a supervisor cluster and carries the §7 guards A–E.

---

## 10. Task decomposition (subagent-driven, mirroring SP5a)

Sequence: **T0 → {T1,T2} → T3 → T4 → T5 → T6 → T7 → T8.** Do not dispatch two agents at the same
`secs1` source file concurrently ([[parallel-agents-shared-file]]); sequence the line-engine tasks
that share the transport file. Each task: fresh implementer → controller-independent scoped gate
(`./secs1/ ./hsms/ ./internal/...`, NOT `./...` — examples/tests are SP6/SP7 leftovers) → task
review (opus for the concurrency/line-engine tasks) → teeth-check → ledger.

- **T0 — Config & wiring skeleton.** `secs1.Config` (embed `hsms.ConnectionConfig` + T1–T4/RTY/
  `IsEquip`/`DeviceID` options); `secs1.New`; the concrete `secs1` transport type with the 6 seam
  methods stubbed + the cross-package compile assertion. `doc.go` with the `Selected`-means-usable
  note (§5). Scoped gate green. (Resolves D5b-7, wires D5b-2 naming.)
- **T1 — Block-transfer line engine (send).** ENQ/EOT/block/ACK with T2 + RTY + RTY-reset-on-
  contention; the seam `Write` runs the full transaction (D5b-1). Port v1 `block_transport.go`
  send path onto `block.appendTo`. (Resolves D5b-1, D5b-9 send side.)
- **T2 — Block-transfer line engine (receive).** EOT/length(T2)/data(T1)/checksum → ACK/NAK,
  `drainUntilSilence`, duplicate-block ACK-without-redeliver; one owned buffer per block (D5b-8).
- **T3 — Contention arbitration + master/slave.** Static role from `IsEquip`; master-wins/
  slave-yields; per-role ignore rules. (Resolves D5b-6, D5b-9 contention side.)
- **T4 — Message/transaction layer.** `splitBody` on send; multi-block accumulate + expected-
  block-number + E-bit + `assembleBlocks` on receive; T4 inter-block; duplicate-block detection;
  `DeliverOwnedFrame`. (Resolves D5b-4 T4.)
- **T5 — Transport lifecycle.** Start/Stop/ArmStart + recv loop + `TCPUp` (auto-commit Selected,
  §5) + `TCPDown`; the **single line-engine goroutine** (§9, the half-duplex O4 resolution) — the
  highest-risk design; opus.
  (Resolves D5b-2, and O4.)
- **T6 — T3 integration.** Confirm `Write` returns only after the last block ACK so the core starts
  T3 correctly (§8/O2); map `secs1.Config` T3 → core reply timeout; confirm `LinktestInterval()==0`
  inertia (O1). (Resolves D5b-4 T3.)
- **T7 — Correctness suite (port + teeth-check).** Port v1 tests: single/multi-block round-trip,
  contention, RTY-exhaustion → `ErrSendFailed`, T3 reply-timeout, T4 partial-message abort,
  duplicate-block, active↔passive; teeth-check the contention-reset and duplicate-block guards
  ([[regression-guard-teeth-check]]). Optional v1↔v2 interop smoke test (D5b-3/O3).
- **T8 — Metrics + doc + review loop.** SECS-I metric counters onto the core surface; finalize
  `doc.go`/README; run the plan → Codex → subagent → Codex review loop ([[review-pipeline]]);
  by-scope squash; v2 branch only (D8: never `main`).

---

## 11. Acceptance criteria

1. `secs1.New(...)` returns a non-nil `hsms.Connection`; `NewConnection(cfg, secs1Transport)`
   compiles (sealed-A cross-package assertion).
2. A v2 `secs1` **active** peer and a v2 `secs1` **passive** peer complete single- and multi-block
   S/F round-trips over TCP loopback under `-race` (the SP5a integration-harness style).
3. Line-control correctness (ported v1 tests, teeth-checked where a guard exists): contention
   (both roles), RTY-exhaustion → `ErrSendFailed`, T3 reply-timeout, T4 partial-message abort,
   duplicate-block ACK-without-redeliver, expected-block-number enforcement.
4. **Zero engine duplication:** `secs1` contains no FSM, supervisor, reply registry, or reconnect
   logic; it only implements the 6 seam methods + the SECS-I line/message engine (grep-verifiable
   against the shared-core symbols).
5. FSM: `State()` reaches `Selected` deterministically (synchronously at TCPUp) before the first
   send; no async promotion race.
6. Scoped gate green: `go build`/`vet`/`golangci-lint` (0 issues) + `-race` on
   `./secs1/ ./hsms/ ./internal/...`; gofmt clean. (Module-wide `go build ./...` stays red on the
   SP6/SP7 v1-leftovers — the acceptance is scoped, per SP5a precedent.)
7. Wire compatibility: the SP2 golden block-vector still passes; a v1↔v2 interop smoke test passes
   if implemented (D5b-3, strong-should).

---

## 12. Open items for the PLAN (not spec blockers)

**Resolved by the round-2 spec-review (Codex) — no longer open:**
- ~~O1~~ — CONFIRMED: `LinktestInterval() <= 0` disables the auto-linktest
  (`hsmsss/transport_procedures.go:16`); no core change needed.
- ~~O2~~ — CONFIRMED: the core starts the reply timer only after `writeFrame` (→ seam `Write`)
  returns (`connection_send.go:238`), so D5b-1's "return after last-block ACK" gives the E4-correct
  T3 start for free.
- ~~O4~~ — RESOLVED into §9 as a fixed architecture (single line-engine goroutine, `Write` hands
  off); no longer a plan choice.
- ~~O6~~ — RESOLVED into D5b-10 (RTY-exhaustion → `Write` error → core `TCPDown`/reconnect; aligns
  with the core contract, no core change).

**Still open for the plan:**
- **O3 — v1↔v2 interop smoke-test feasibility.** Whether a v1 `secs1` peer (on `main`) can be
  driven from a v2 test is a tooling question; if infeasible, rely on the SP2 golden vector + a
  hand-authored v1-wire-trace replay. (D5b-3 makes this a strong-should, not a blocker.)
- **O5 — `SessionID()` value for SECS-I.** The E4 header carries a device id, not an HSMS session
  id; `SessionID()` is not on the SECS-I wire, so any value is safe. Pick for least surprise
  (default 0xFFFF sentinel vs. `DeviceID`) in the plan.
- **O7 — idle "watch both" mechanism for the line engine (§9).** Short read-deadline poll vs.
  `SetReadDeadline(now)` poke-to-interrupt. An implementation choice inside the fixed single-engine
  architecture; pick in the plan with a latency/CPU trade-off note.
