# Sub-project 5a — connection core + HSMS-SS transport (design / spec)

**Status:** design approved 2026-06-30 (brainstorm + E37-grounded architecture review), ready for the
Codex review loop → implementation plan.
**Depends on:** SP1 (`secs2` immutable items), SP2a (`hsms` immutable messages + `internal/wire`), SP4
(`docs/v2/06-internal-pooling-decision-4-spec.md` — the internal-pooling contract and §5.F option (b)
default). Charter = proposal decision **D4** (`docs/v2/00-v2-proposal.md:181,488,492`).
**Module:** `github.com/arloliu/go-secs/v2`, branch `v2`, Go floor `1.26.0`. **Never merges to `main`** (D1).
**Design inputs (read for the exhaustive detail this spec summarizes):**
`tmp/2026-06-30-sp5-territory-map.md` (v1 reference + the §7 landmine catalog A–K) and
`tmp/2026-06-30-sp5a-architecture-review.md` (the E37 normative model, the KEEP/MERGE/DROP analysis, the
two adversarial passes, and the concrete type sketches). Landmine IDs (A1, F5, H2, …) below refer to the
territory map §7.

---

## 1. Goal

Rebuild the HSMS-SS connection layer on the v2 immutable, GC-owned message model (D7), as a **shared
connection-management core** (reused later by SECS-I in SP5b) plus a thin **HSMS-SS transport**, replacing
the v1 stack carried onto `v2` that no longer compiles. The rebuild is a **fresh-perspective redesign, not
a port**: SEMI E37 / E37.1 mandates only a 3-state FSM, 8 procedures, and 5 timers; everything in v1 beyond
that kernel is implementation machinery, much of it accidental under the immutable model. SP5a keeps the
E37 kernel and every hard-won concurrency invariant verbatim in *meaning*, while collapsing the accidental
machinery (a second physical FSM, embedded-base OOP, a goroutine-registry class, a transition-request
channel hatch, a lifetime send channel + send-gate, a two-map orphan-defended reply registry, a
cross-goroutine two-phase send handshake) using three structural levers the immutable model unlocks:
**per-generation context/channels via `atomic.Pointer`**, **sender-owned per-transaction reply channels**,
and a **single-owner supervisor goroutine**. Correctness is a first-class deliverable: the full
fault-injection harness suite is ported and the high-risk landmines plus the new design's own guards each
get a teeth-verified regression test.

Success = `go build ./...` and `go vet ./...` green on `v2`; a working active+passive HSMS-SS connection
that selects, exchanges W-bit/async data and control messages, link-tests, separates, reconnects, and
closes cleanly; the chosen correctness suite green under `-race`; and the public surface still passes the
proposal's no-mutation/no-`Free`/no-`usePool` CI gate.

## 2. Scope (sub-project 5a)

**In scope:**
- **Shared connection-management core**, landed in the v2 `hsms` package (which today holds only the
  message model; the deleted v1 infra lived there). Components: the per-generation `epoch` lifecycle, the
  single-owner `supervisor` (the one E37 logical FSM) + `notifier`, the `Connection` shell (Open/Close,
  reconnect), the concrete `Session`, the unexported `transport` seam, the send/reply model, `ConnectionConfig`
  + options, and `ConnectionMetrics`.
- **HSMS-SS transport** (`hsmsss` package): the framed reader (4-byte length + 10-byte header + body) over
  the **decode-owned-frame** path; the **writev** send over the raw `*net.TCPConn`; the active/passive
  connect procedures; Select / Linktest / Separate / Reject / (defensive) Deselect; timers T3/T5/T6/T7/T8.
- **The zero-copy decode-owned-frame entry point** (`secs2` + `hsms`): a decode path that takes ownership of
  an already-GC-owned frame buffer and decodes in place (no `bytes.Clone`), eliminating the raw-frame
  double-copy (SP4 forward-note (a)) and making proposal §5.B real. Read buffers are **not pooled** (§5.F
  option (b)).
- **The correctness suite** at the chosen depth (§8): ported harnesses + risk-tiered teeth-verified
  regressions + the new guards' teeth-checks + a re-derived clean-shutdown gate.

**Out of scope (later sub-projects / explicitly not built):**
- **SECS-I transport** (the half-duplex ENQ/EOT/ACK/NAK block protocol, T1–T4, contention) — **SP5b**, built
  on the SP5a core. SP5a designs the `transport` seam so SP5b is a drop-in, but builds no SECS-I code.
- **Multi-session / HSMS-GS** — dropped (§4: HSMS-SS is single-session; the session *is* the connection).
- **`gem` integration and the v1→v2 migration guide** — SP7.
- **Cross-package integration-test architecture** beyond HSMS-SS — SP6.
- No public zero-copy writer API (`WriteFrameTo`/`Buffers()` stay banned, §4.7 of the proposal); no public
  pooling/`Free`/`usePool` (D7).

## 3. The irreducible E37 / E37.1 core (the floor — implemented verbatim in meaning)

This is what SP5a MUST implement regardless of representation. Source: SEMI E37 / E37.1, cross-checked
against the §-citations embedded in the v1 code (no spec copy is in the repo); see the architecture review §1.

- **State machine (E37 §5.4–§5.6, HSMS connection state model):** exactly three logical states — `NotConnected`, `NotSelected` (TCP up, no
  session), `Selected` (data flows). Active dialing and passive listening happen *inside* `NotConnected`
  (a `Connecting` pseudo-state is an implementation refinement, used by the close-race guards). Transitions:
  TCP up → `NotSelected`; Select completes → `Selected`; TCP broken / Separate / T7 expiry / fatal comms →
  `NotConnected`. Illegal transitions are rejected (guarded FSM).
- **Procedures + SType:** 0=Data, 1/2=Select.req/rsp, 3/4=Deselect.req/rsp, 5/6=Linktest.req/rsp, 7=Reject.req,
  9=Separate.req. Required: Connect (active exponential-backoff-capped-at-T5 / passive accept, refuse 2nd conn); Select
  (req/rsp, SessionID `0xFFFF`, bounded by T6) → `Selected`; Data (primary; if W-bit, reply matched by
  System Bytes, bounded by **T3 started only after the primary is fully on the wire**, §9.4.1.2); Separate
  (SType9, no response, ignored if receiver not `Selected`, §7.9.2) → `NotConnected`; Linktest (T6);
  Reject (SType7 + reason, §7.10.3, **keep the link**); Disconnect (TCP close → `NotConnected`, T7 bounds the
  `NotSelected` dwell, §9.2.2). **Simultaneous Select (§7.4.3):** if both ends send Select.req, both accept
  — realized by committing `Selected` **synchronously before** emitting Select.rsp (landmine **H2**,
  normative — see §7.D).
- **Timers (all five mandated):** T3 reply (clock starts after primary on-wire, §9.4.1.2); T5 connect-separation;
  T6 control transaction; T7 NOT-SELECTED dwell (§9.2.2); T8 inter-character on the recv stream including
  across the length header (§9.2.3.1).
- **Pure protocol invariants kept verbatim:** sync-commit-to-Selected before Select.rsp (H2); Reject+keep-link
  on unsupported PType/SType (J3); ignore Separate when not Selected (§7.9.2); refuse data / Reject(reason 4)
  when not Selected (B1); validate frame length before allocating the body buffer (J2); orphan control
  response → Reject(TransactionNotOpen, reason 3) (§8.3.20).
- **E37.1 single-session:** one session, 1:1 with the TCP connection. Control frames carry SessionID `0xFFFF`;
  the only required artifact is the SessionID **value** (a config field), not a session object.

## 4. Architecture decisions (locked)

**Placement / shared-core (resolves proposal D4 / open-question 8.2).** The connection-management shell
lands **once** in the v2 `hsms` package; `hsmsss` (this sub-project) and `secs1` (SP5b) are thin
per-transport layers implementing an unexported `transport` seam. The half-duplex/full-duplex divergence
(K1) is absorbed by the seam, not by forcing either transport onto the other's reply idiom — the
sender-owned-channel reply model (§5.5) lets both share one correlation core.

**Sealed-A package boundary (resolves §10 "Package boundary"; from the 2026-06-30 Codex package-boundary
consult, grounded in the real import graph).** The seam keeps **no `internal/*` type and no *nameable* seam type in
the public surface** (it is not a hard-sealed extension API — see §5.4's accurate sealing claim), using the Go
idiom *unexported interface **type** + exported **methods***: a Go interface whose method set contains an
unexported method can only be satisfied inside its defining package, but an interface whose *type name* is
unexported while all its *methods* are exported can be satisfied by **any** package (the caller passes a value
through an exported `hsms` constructor and never needs to *name* the type — which is also why that constructor is
not hard-sealed; §5.4). The earlier §5.4
sketch (unexported `transport` with **unexported** methods `send`/`sendAsync`/`connDone`) was therefore
**unbuildable** — `hsmsss` could never implement it — and is superseded by §5.4 below. v1's `hsms.Connection`
was itself an interface (`hsmsss/conn.go:167 var _ hsms.Connection = &Connection{}`) with each transport
carrying a *duplicated* engine (the documented K1 hsmsss↔secs1 drift); SP5a keeps `hsms.Connection` as the
app-held **interface** but lands the engine **once** as an *unexported* concrete `hsms` type. Import direction
stays acyclic: `secs2 ← internal/wire ← hsms ← hsmsss`, plus a **new** `internal/framecodec` imported by `secs2`
and `hsms` that imports neither (the decode-owned capability seam for `secs2.DecodeOwned`, §6.1; the HSMS frame
entry `hsms.decodeOwnedFrame` already exists and is sealed-by-visibility, so it needs no token). Config splits:
`hsms.ConnectionConfig` carries shared-core settings only (T3/T5/T6/T7/T8, queue sizes, session ID, linktest
policy, logger, close timeout, metrics knobs); transport-specific settings (TCP endpoint, active/passive role,
keepalive, dial/listen tunables) live in `hsmsss.Config`, which embeds `hsms.ConnectionConfig`.

**KEEP / MERGE / DROP** (full table + per-item landmine coverage in the architecture review §2):

- **DROP** (nothing of value lost): `BaseSession` + the three `Register*Func` (hand-rolled inheritance);
  `AddSession`/multi-session + `IsGeneralSession`; the sync/async handler split; the `stateChangeChan`
  transition-request hatch + the invalid→NotConnected self-heal; the send-gate (`sendMu`/`sendClosed`) +
  the `context.Background()` final drain; the `replyErrs` second map + all orphan-defense
  (`dropAllReplyMsgs`/`drainOrphanReply`/identity-rechecks/test hooks); the `sentChan` two-phase-send
  *mechanism*.
- **MERGE** (keep the concern, collapse the form): `AtomicOpState` → the per-generation `epoch` (§5.1);
  `TaskManager` → a per-generation `sync.WaitGroup` + one `spawn` (§5.1); the two async dispatch channels →
  one `supervisor` + one `notifier` goroutine (§5.3); the lifetime `senderMsgChan` → `epoch.sendCh`; the
  two reply maps → one sender-owned `chan replyResult` keyed by `SystemBytes() [4]byte` (§5.5).
- **KEEP** (E37-required or a real race the leaner form cannot shed): the guarded 3-state FSM; the
  two-phase-send **rule** (§9.4.1.2); the IsSelected data gate (B1–B3); T7/T8; the `DataMsgInflightCount`
  balance invariant (I1); the reconnect machinery (joined **separately**, §7.C); the simultaneous-select
  synchronous commit (H2, §7.D); the T8 framing reader; Deselect (defensive interop, below).

**Locked decisions** (from the brainstorm):
- **D5a-1 Send path = hybrid.** Direct **synchronous writev** (under `epoch.writeMu`) for W-bit + explicit
  Sync sends — the write returning *is* "on the wire," collapsing the `sentChan` handshake and tightening
  §9.4.1.2. A per-generation **async queue + sender goroutine** handles fire-and-forget (receiver-emitted
  Reject, Separate, S9F9, `SendMessageAsync`) so the receiver never blocks on a wedged peer (J3).
- **D5a-2 Read buffer = §5.F option (b).** Per-message GC-owned frame (`make([]byte, msgLen)`); the read
  buffer is **not pooled**. This is *forced* by the zero-copy decode-owned-frame entry (a decoded message's
  leaf items alias the frame; pooling would corrupt a retained reply). Confirm with an (a)-vs-(b) benchmark;
  do not silently enable both.
- **D5a-3 Notification = single supervisor + one notifier**, single public handler type
  `StateChangeHandler func(prev, next ConnState)`; user callbacks run only on the notifier (never inline on
  the protocol goroutine).
- **D5a-4 Deselect = retained (defensive interop).** HSMS-SS uses Separate; v1 implements Deselect.req/rsp
  anyway. Kept as harmless generosity (not a strict-E37.1 build). Flag in the plan if a strict build is wanted.
- **D5a-5 Linktest-interval reconfig = apply-on-next-Selected.** A mid-connection `UpdateConfigOptions`
  interval change applies when the linktest goroutine next (re)starts on entering Selected; no live
  `ticker.Reset` plumbing (the interval is `[impl]`, not E37). Document the behavior.
- **D5a-6 Test depth = full harnesses + risk-tiered regressions** (§8).
- **D5a-7 `context.Context` on blocking public methods (confirmed with Codex).** The blocking-I/O methods take
  `ctx` as the first parameter — `Open`, `SendDataMessage`, `SendDataMessageAsync`, `SendSECS2Message`,
  `ReplyDataMessage` (and the unexported `transport.send`/`sendAsync`). NOT on `Close`, `ID`, the
  `Add*Handler` registrations, or `UpdateConfigOptions`. `ctx` is **not** for hang-prevention (T3/T6 and the
  epoch ctx already bound the waits) — it is for caller-driven cancellation, per-call deadlines tighter than
  T3, and integration with the consumer's context tree. Rules: **ctx-first, no duplicate non-ctx variants**
  (clean `/v2` module); a `context.Background()` argument reproduces today's timer-only behavior
  (behavior-compatible). The protocol timers stay the **default** bound; `ctx` is an additional, usually
  tighter, caller bound. **`Close` stays ctx-free** (immediate, internally bounded, always completes teardown —
  a caller-ctx that abandoned the wait mid-teardown would release `lifeMu` over a half-torn-down generation
  and break `Open` interleaving); a graceful **`Shutdown(ctx) error`** is an optional additive method (the
  plan decides whether to include it). See §5.2/§5.4/§5.5 for signatures and the cancellation contract.

## 5. Shared core design (`hsms` package)

> The type sketches below are the **design contract**; exact field sets/signatures are refined in the plan.
> `internal/wire` is imported **directly** by the transport layers for zero-copy writev — it must never
> appear in an exported signature, Godoc, or README (proposal §4.8; rule 100).

### 5.1 Per-generation `epoch` — single-owner lifecycle without a second FSM

`epoch` owns everything scoped to ONE TCP-connection generation: socket, send channel, reply registry,
per-generation context, and the join group. Created on (re)connect, published via `Connection.cur`
(`atomic.Pointer`), torn down exactly once.

```go
type epoch struct {
    ctx    context.Context     // GENERATION-LIFETIME ctx only; set once at construction (WithCancel(parent)); see ctx guardrails below
    cancel context.CancelFunc  // called by teardown ONLY, under closeOnce — never by arbitrary helpers

    connMu sync.RWMutex        // guards conn (v1 resMutex role)
    conn   net.Conn            // raw *net.TCPConn (NO bufio wrapper — defeats writev, §6.2)
    // nil ONLY after closeSocket(), which runs AFTER the farewell-Separate decision (H1/§7.9)

    wg        sync.WaitGroup   // joins every per-generation TASK goroutine (G3) — NOT the reconnect loop (§7.C)
    spawnMu   sync.RWMutex     // Add-vs-Wait happen-before guard (§7.B); replaces task.go taskMu
    closing   bool             // set under spawnMu.Lock at teardown; post-teardown spawns become no-ops
    closeOnce sync.Once        // admits exactly ONE teardown owner (replaces ToClosing CAS, F4)
    closeErr  error            // written inside closeOnce; read after <-done
    done      chan struct{}    // closed when teardown completes (replaces isClosed() poll, removes F2 hang)

    sendCh  chan *sendRequest  // PER-GENERATION send channel; GC'd with the epoch (dissolves C1)
    replies replyRegistry      // per-generation; sender-owned channels (dissolves F5/F6)
    writeMu sync.Mutex         // serializes writev over conn
}

func (e *epoch) liveConn() net.Conn // RLock connMu; the "socket live?" fact (was opState.IsOpened())
func (e *epoch) spawn(log, name string, fn func(ctx context.Context)) bool // the ONE launch path (was 5 TaskManager variants)
func (e *epoch) teardown(timeout time.Duration) error // the whole closeConn replacement; idempotent; BOUNDED join
```

**Stored-context guardrails (why `ctx`+`cancel` live on `epoch`, and the discipline that keeps it correct).**
The stdlib "don't store a `Context` in a struct" guidance targets **request-scoped** contexts threaded through
call APIs (its harm: hidden lifetimes, lost per-call deadlines, mixed operation scopes — the bad `Worker{ctx}`
example). `epoch` is the accepted **lifecycle-owner** carve-out: an *unexported* object representing exactly one
TCP generation, whose ctx lifetime **is** the epoch lifetime. The epoch creates the ctx (`WithCancel(parent)`),
holds the `cancel` it must retain to tear down, and dies with both — and it improves on v1's separate
`atomic.Pointer[ctxHolder]` by making the generation swap atomic at the `epoch` object level
(`Connection.cur`). This is sound **only** under these binding rules (confirmed with Codex):
1. **Generation-lifetime only** — `e.ctx` carries no per-operation deadlines. T3/T6, socket write deadlines,
   dial timeouts, and the bounded farewell-Separate write derive their own timers/sub-contexts; they never
   redefine `e.ctx`.
2. **No `WithValue` payloads** — no request/session/protocol metadata on `e.ctx` (an epoch spans many messages).
3. **No broad context accessor** — there is no exported or widely-used `Context()`/`connContext()`; a broad
   accessor reintroduces v1's ambient "current context" lookup and makes stale-generation bugs hard to audit.
   Select-only helpers get a narrow `done() <-chan struct{}` instead (see §5.4).
4. **All joined generation goroutines enter through `spawn(fn func(ctx))`** which passes `e.ctx` as a
   parameter — code receives the context explicitly at the function boundary, honoring the rule's *spirit*. A
   goroutine may close over `e` for `sendCh`/`writeMu`/`conn`, but cancellation arrives as the `ctx` argument;
   raw `go func()` is allowed only for the documented transition-only exceptions (T7) and still takes `ctx`.
5. **`cancel` is invoked only by teardown ownership** (inside `closeOnce`), never by arbitrary helpers.
6. **`ctx`/`cancel` are set once at construction and never mutated** — effectively immutable fields, so the
   atomic `epoch` swap has no torn-read on them.

- `spawn`: takes `spawnMu.RLock`, returns false (no-op) if `closing`, else `wg.Add(1)` under the RLock and
  launches a panic-guarded goroutine (`recover()` → log) running `fn(e.ctx)`. **Rule: no raw `go func()` may
  touch `sendCh`** (G3); T7-style transition-only goroutines are exempt and use a plain `go func(ctx)`.
- `teardown(timeout)` is the **non-blocking initiator** (so the supervisor reaction can call it without
  stalling — §5.3): `closeOnce.Do(func(){ cancel(); closeSocket(); seal closing under spawnMu.Lock; spawn a
  SEPARATE goroutine that: joins the per-generation transport recv loop (`tr.Stop`, below), does the
  timeout-bounded join of `epoch.wg` (record `ErrCloseTimeout` + "N tasks live" on expiry), then close(done) })`;
  `teardown` **returns immediately** after `closeOnce.Do` — it does NOT `<-done`. Callers that need the teardown
  *result* call the separate `(e *epoch) wait() error { <-done; return closeErr }` (this is what `Close()` blocks
  on, §5.2 — never the supervisor). **Bounded** (§7.A); close TCP **before** the join so a parked `conn.Read`
  unblocks (J5); the join runs on a spawned goroutine so the teardown trigger is never a member of the set it
  waits on (§7.C).
- **The transport recv loop is bound to the epoch (Codex round-7 — no stale callbacks).** The recv loop
  (`transport.Start` spawns it; §6.1) reads on the epoch's socket and, on error, calls `rt.TCPDown`. It MUST be
  **joined by teardown before `e.done` closes** — teardown's separate goroutine calls `tr.Stop(ctx)` (which joins
  the recv loop; `closeSocket` having already unblocked its parked `Read`, J5). Because `tr.Stop` runs on the
  teardown goroutine (NOT the supervisor), the recv loop's *final* `rt.TCPDown → inject(evDisconnect)` is drained
  by the still-alive supervisor as a **safe no-op** (`evDisconnect` from `NotConnected` has no transition — the
  link is already terminal). Consequently **no transport goroutine outlives `e.done`**, so a stale recv loop can
  never inject into a *later* generation or a *reopened* connection. The reconnect loop additionally **`e.wait()`s
  the prior epoch (recv loop fully joined) BEFORE dialing the next generation** (§5.2), so recv loops never overlap
  within an Open cycle either. (Optional hardening: a per-generation runtime view bound to *this* epoch+supervisor,
  so even a hypothetically-leaked callback no-ops against the old stopped supervisor — not required given the join.)

```go
type Connection struct {
    lifeMu        sync.Mutex            // serializes Open/Close ENTRY (replaces ToOpening CAS, H6)
    cur           atomic.Pointer[epoch] // current generation; nil ONLY before first Open. NOT cleared on Close —
                                        // it retains the torn-down epoch (done closed) so idempotent re-Close
                                        // returns that epoch's closeErr (round-8); cur==nil ⇒ never-opened ⇒ ErrNotOpen.
    sup           atomic.Pointer[supervisor] // the E37 logical FSM — RECREATED FRESH per Open (no channel reuse, §5.3)
    handlers      atomic.Pointer[[]StateChangeHandler] // user state-change handlers; live HERE (persist across Open/Close)
    dataHandlers  atomic.Pointer[[]DataMessageHandler] // (on the session; shown for parity) — registered, persist
    shutdown      atomic.Bool
    reconnectGen  atomic.Uint64
    connectLoopWg sync.WaitGroup        // SEPARATE reconnect-loop join (§7.C) — NOT epoch.wg
    supWg         sync.WaitGroup        // joins the per-Open supervisor run()+notifier() (Connection-owned, NOT epoch.wg)
    // no AtomicOpState, no senderMsgChan, no sendMu/sendClosed, no closeErr pointer, no taskMgr
}
```

### 5.2 Open / Close (shape unchanged from v1; teardown funneled through the NotConnected reaction)

- `Open(ctx context.Context, mode OpenMode) error` (D5a-7): `lifeMu` (one opener). `mode` is a typed flag
  (`OpenWaitSelected` — block until Selected/fatal; `OpenBackground` — start the lifecycle and return,
  letting connect/select/reconnect run in the background; passive HSMS-SS in particular needs background
  open since selection depends on the peer). `ctx` bounds the synchronous wait in `OpenWaitSelected`
  (`select { Selected | connect-fatal | <-ctx.Done() }`); `ctx` and `mode` are **orthogonal**. **Double-open
  guard (H6):** if a *live* epoch already exists (`cur.Load() != nil` and its `done` is not yet closed), Open
  is a no-op returning `ErrAlreadyOpen` — it must **never** `<-e.done` on a live generation (that would hang
  forever). The `<-e.done` wait applies **only** to a *prior* generation already in teardown. Then:
  `shutdown=false`; join a dying reconnect loop (`connectLoopWg.Wait()`, the separate group) **before** fresh
  contexts (G1); create + `cur.Store` a new epoch; **create a FRESH `supervisor` (fresh `events`/`notify`/`stopCh`/
  `runDone`, `state=NotConnected`, `lastReacted=NotConnected`) and `sup.Store` it, then start its `run()`+`notifier()`
  as CONNECTION-OWNED goroutines joined by a `supWg` (NOT `epoch.spawn` — Codex round-6: the supervisor must
  outlive any single epoch so reconnect keeps working; an epoch-spawned supervisor would be killed by the first
  disconnect's epoch-ctx cancel)** — the supervisor is per-Open-cycle, NOT reused, so the channels a prior `Close`
  closed are never re-sent-on (round-5); the fresh supervisor reads user handlers from the **Connection's**
  `handlers` pointer (which persists). Then dial-or-listen.
- `Close() error` (**ctx-free, D5a-7**): `lifeMu`; **entry guards (Codex round-7/8): (i) NEVER-OPENED — if
  `cur.Load() == nil` return `ErrNotOpen` (no `requestClose`/`e.wait` on a nil epoch/supervisor); (ii) ALREADY-CLOSED
  idempotent re-Close — `cur` is NOT cleared on Close, so a re-Close sees a non-nil but torn-down epoch (its `done`
  closed / `sup` stopped) and returns that epoch's retained `closeErr` WITHOUT a second `requestClose`** (the nil
  guard and the idempotent-re-Close contract are thus distinct and non-contradictory: nil ⇒ never-opened ⇒
  `ErrNotOpen`; non-nil-but-done-closed ⇒ already-closed ⇒ prior `closeErr`); bump `reconnectGen`; `shutdown=true`
  (reconnect/Connecting reactions re-check it, F3);
  capture `e := cur.Load()`; `sup.requestClose(e)` — pins `e` then injects `evClose`
  (a GUARANTEED enqueue, §5.3). The supervisor's `evClose` handling **initiates teardown of the pinned epoch `e`
  from EVERY state** (§5.3): from `Selected` via the reaction (farewell-Separate decision + teardown init); from
  `NotSelected` via the reaction (no farewell); from `NotConnected` via the unconditional `e.teardown` (no
  reaction fires, no farewell needed). Then block on **`e.wait()`** (`<-e.done; return closeErr` — no
  `isClosed()` poll, no F2 hang; the supervisor was never blocked — §5.3); **then `sup.stop()`** — closes `stopCh`
  so `run()` exits (closing `notify`+`runDone`, which ends `notifier()`) and **joins both via `supWg`**. Ordering
  is deliberate: `requestClose` → `e.wait()` (the pinned-epoch teardown completes while the supervisor is still
  alive and draining `events`) → `sup.stop()` (only now tear down the supervisor itself). The supervisor outlives
  the epoch, never the reverse. Because teardown is initiated for **any** starting state, a `Close()` during active-dial /
  passive-wait (when `Open(OpenBackground)` returned before selection) **cannot hang** (Codex round-2). Close is
  **internally bounded** (the §7.A bounded join returns a close-timeout error) and **always completes teardown** —
  deliberately no caller `ctx`, because a caller-ctx that abandoned the wait mid-teardown would release `lifeMu`
  over a half-torn-down generation and break `Open` interleaving (Codex C). **`Close()` does NOT call
  `e.teardown()` directly** — teardown is initiated **only** from inside the supervisor's `evClose`/NotConnected
  handling (next bullet + §5.3), so the farewell-Separate decision provably happens-before `closeSocket()` (see
  §7.E). The single-owner `closeOnce` still makes teardown idempotent for any other path (drop/reconnect) that
  also reaches NotConnected. **Idempotent re-`Close()`:** a second `Close()` (after the first already ran
  `sup.stop()`, so the supervisor's `run` has returned and `runDone` is closed) MUST short-circuit — under
  `lifeMu`, if the supervisor is already stopped it returns the prior `closeErr` **without** calling
  `requestClose` again (and even if it did, `inject`'s `select { events<-ev | <-runDone }` makes a post-stop
  inject a safe no-op rather than a deadlock on the now-unread `events` channel).
- **`Shutdown(ctx context.Context) error` (optional additive, plan decides):** the graceful-with-deadline
  variant (`http.Server` precedent — `Close()` immediate vs `Shutdown(ctx)` graceful). It initiates the same
  teardown and waits for clean completion bounded by `ctx`; if `ctx` fires first it returns `ctx.Err()` **but
  teardown still completes in the background** via `closeOnce` (the resource teardown is never abandoned —
  only the *wait* is). Not required for SP5a since `Close()` is already bounded; include only if a
  caller-chosen close deadline is wanted.
- The supervisor's NotConnected reaction (handling the `prev → NotConnected` transition), running on the
  supervisor goroutine, is the **sole initiator of teardown**. The farewell Separate is **best-effort and
  MUST NEVER block teardown** (it caused a deadlock in an earlier draft: a wedged W-bit writer holds
  `e.writeMu`, the farewell write waits on it, and `closeSocket()` — the very thing that would unblock the
  writer, J5 — sits behind the farewell write).
  - **Teardown cause matters (E37 §9.1.1).** On a **communication-failure / dropped-link** teardown (the
    reason for entering NotConnected is a network error, a read/write failure, or a peer Separate already
    received), **no orderly Separate is attempted** — the link is already gone; terminate the TCP
    connection immediately (§9.1.1). Only a **graceful local** teardown (user `Close()` / clean reconnect)
    from a live Selected link attempts the courtesy Separate.
  - **The graceful farewell Separate is a BOUNDED best-effort synchronous write.** When `prev == Selected &&
    e.liveConn() != nil && !deselected && cause == graceful`: attempt `writeFrame(Separate.req)` acquiring
    `e.writeMu` with a **bounded wait** (`TryLock` / short deadline) and a **short write deadline**. If the
    lock is not acquired promptly or the write errors, **skip it and proceed** — it is a courtesy, not a
    correctness requirement.
  - **`closeSocket()` is UNCONDITIONAL and always runs** (inside `e.teardown` → `closeOnce`), regardless of
    whether the Separate was sent — and closing the socket is what unblocks any parked/wedged writer (J5),
    so teardown always makes progress. Ordering within the reaction: (1) attempt the bounded best-effort
    Separate; (2) `e.teardown(timeout)`; (3) if `!shutdown`, start the connect loop (`connectLoopWg.Add(1)`).
  - **Generations never overlap (Codex round-7).** The connect loop's FIRST action for a reconnect is to
    **`e.wait()` the just-torn-down epoch** (so its recv loop and all generation goroutines are fully joined)
    BEFORE dialing the next generation and calling `tr.Start` (a fresh recv loop). A gen-N recv loop is therefore
    gone before gen-N+1's recv loop exists — no two recv loops, no stale `evDisconnect` from gen N corrupting gen
    N+1. (Within an Open cycle the single supervisor persists; serialized generations keep its event stream clean.)
  - **G2 fence (binding; ATOMICS ONLY — Codex round-8).** The reconnect loop, immediately before it `cur.Store`s a
    freshly-created epoch, **re-checks `shutdown` and its captured `reconnectGen` using ATOMICS ONLY (never
    `lifeMu`)** — e.g. `shutdown.Load()` + a CAS/compare on `reconnectGen`; if `shutdown` is set or `reconnectGen`
    advanced (a `Close()`/new `Open()` happened since this retry was scheduled), it abandons the new epoch and
    exits instead of publishing it. **The fence MUST NOT acquire `lifeMu`:** `Open` holds `lifeMu` while it
    `connectLoopWg.Wait()`s a dying reconnect loop (G1), so a reconnect loop that blocked on `lifeMu` at its fence
    would deadlock against `Open`. This guarantees no new generation appears after `Close()` set `shutdown` +
    bumped `reconnectGen`, so the epoch `Close` pinned via `requestClose(e)` is the last one and `e.wait()` cannot
    miss a sneaked-in successor.
  - **E37 state ordering (no contradiction with §5.3).** The supervisor's internal `state` advances
    `Selected → NotConnected` in one atomic step as FSM bookkeeping, and the terminal `NotConnected` user
    notification is emitted once (§5.3). The *externally observable* E37 sequence is still honored by the
    reaction's action order: the courtesy Separate.req is written **while the link is still effectively
    SELECTED** (`prev == Selected`, socket live — §7.9.1), and the TCP close happens **after**, i.e. once
    the peer has been told to leave SELECTED (§6.4). There is no protocol activity and no T7 to honor
    between NOT SELECTED and NOT CONNECTED during teardown, so collapsing them in the internal FSM value is
    safe; the on-wire order remains strictly Separate-then-close. A normal (non-teardown) user-requested
    Separate may still use the async `sendCh` path.

### 5.3 Notification — single supervisor + one notifier

```go
type ConnState uint32 // EXPORTED public state: NotConnected, NotSelected, Selected (the public handler names it)
type fsmEvent uint8   // unexported internal event
const ( evTCPUp fsmEvent = iota; evSelectAccepted; evSelectLost; evDisconnect; evClose )
type StateChangeHandler func(prev, next ConnState) // the ONLY public handler type

type supervisor struct { // ONE per Open/Close cycle — created fresh in Open, stopped in Close (no channel reuse)
    state        atomic.Uint32 // stores a ConnState; lock-free reads for the hot-path send/recv gates + State()
    lastReacted  ConnState      // supervisor-owned; dedups reactions/notify (H3; tolerates the H2 pre-commit)
    events       chan fsmEvent   // supervisor is the SOLE reader; GUARANTEED command queue (inject blocks, never drops)
    notify       chan stateChange // ALL transitions; supervisor is SOLE sender; NON-BLOCKING drop-OLDEST (coalescing)
    droppedNotify atomic.Uint64  // count of coalesced/dropped notifications (Warn, rate-limited)
    react        func(prev, next ConnState) // for transitions INTO NotConnected: farewell decision + c.cur.Load().teardown()
    closeEpoch   atomic.Pointer[epoch]      // set by requestClose(e) BEFORE injecting evClose; the epoch to ensure-tear-down
    stopCh       chan struct{}              // closed by sup.stop() (from Close, AFTER e.wait()) → run() exits
    runDone      chan struct{}              // closed when run() returns; makes inject a safe no-op after the supervisor stops
    // NOTE 1: user StateChangeHandlers are NOT stored here — they live on the Connection (atomic.Pointer to an
    // immutable slice) so they persist across Open/Close cycles while the supervisor is recreated per Open.
    // NOTE 2 (CRITICAL, Codex round-6): the supervisor's LIFETIME is the whole Open/Close CYCLE — it spans
    // reconnect generations. Its run()/notifier() goroutines are started under a CONNECTION-owned signal
    // (this stopCh), NOT via epoch.spawn / the epoch ctx — an involuntary disconnect cancels the epoch ctx but
    // MUST NOT kill the supervisor (reconnect needs it). epoch.spawn is for PER-GENERATION goroutines only.
}

// CRITICAL — the supervisor must have NO indefinite blocking point, or a blocking inject(evClose) from Close
// deadlocks against it (Codex rounds 4-5). THEREFORE **every send the supervisor makes is non-blocking**:
// `notify` uses NON-BLOCKING drop-OLDEST coalescing (the SOLE sender drops the oldest buffered notification to
// make room, never blocks). Drop-oldest GUARANTEES the LATEST state always reaches the handler eventually
// (only intermediate transitions may be coalesced under a stalled consumer); a single cap-1 "terminal slot"
// was rejected because the supervisor spans reconnect generations, so a stalled notifier could see two terminal
// emits and a cap-1 slot would then force a blocking send (round-5). **F1 is an ORDERING guarantee** (emit the
// NotConnected transition BEFORE stopping the notifier — never skip it), NOT a never-drop-under-stalled-handler
// guarantee: the AUTHORITATIVE current state is always `conn.State()` (lock-free), and StateChangeHandler is a
// best-effort push (handlers MUST NOT block; document this). Drop-oldest ensures even a momentarily-slow handler,
// once it catches up, observes the current state.
//
// requestClose(e) pins the exact epoch (so Close and the supervisor agree on WHICH generation), then injects
// evClose. On evClose the supervisor runs the normal transition+dedup'd react AND THEN, unconditionally,
// `e.teardown(timeout)` (idempotent) — covering the no-reaction NotConnected case so Close cannot hang.
func (s *supervisor) requestClose(e *epoch) // s.closeEpoch.Store(e); s.inject(evClose)
func (s *supervisor) inject(ev fsmEvent)     // select { events <- ev | <-runDone } — blocking-guaranteed while the
                                             // supervisor runs; a safe NO-OP once run() has returned (idempotent
                                             // re-Close after sup.stop() cannot deadlock). Close also short-circuits
                                             // when already closed (see §5.2) so a re-close returns the prior result.
```

- `run()`: single writer for async transitions; **lifetime = the Open/Close cycle, NOT an epoch**. `select` on
  `<-stopCh` (→ return; closed by `sup.stop()` from `Close` AFTER `e.wait()`) / `events`. **There is NO
  `ctx.Done()→evClose` synthesis** (Codex round-6): `evClose` is ONLY ever the *pinned* one from `requestClose(e)`,
  so the `evClose` handler's `s.closeEpoch.Load().teardown()` is never a nil deref, and an involuntary
  epoch-ctx cancellation (a disconnect) never kills the supervisor. On a dequeued `ev`:
  `cur := ConnState(state.Load()); next, ok := transition(cur, ev)` (pure E37 §5.4–§5.6 state table; illegal ⇒
  safe no-op `continue`); store on `next != cur` (a no-op when `CommitSelected` already pre-stored — see below).
  **Reaction/notify dedups on `lastReacted`, and the reported `prev` is `lastReacted`, NOT the atomic's current
  value** (binding — H2/H3, fixes the dropped-reaction bug): `if next != lastReacted { prev := lastReacted;
  emit(prev,next); react(prev,next); lastReacted = next }`. **Every send the supervisor makes is non-blocking**
  (all transitions, including the terminal NotConnected, go to the single `notify` via drop-OLDEST coalescing) —
  the supervisor has NO indefinite blocking point, so it always returns to draining `events` (Codex rounds 4-5).
  `defer close(notify); defer close(runDone)` (single-owner close drains the notifier — H4/H5 for free; the
  supervisor is recreated per Open so these channels are never reused after close). `react` must be
  **non-blocking** on this goroutine: it **schedules** the bounded teardown (a non-blocking `epoch.teardown`
  initiation, §5.1 — which runs the join on a *separate* goroutine and closes `done`), and **never** inline-`Wait`s
  or `<-done`s (§7.A/C; the supervisor stays responsive — only `Close()` waits on `done`).
- **`evSelectAccepted` is valid from BOTH `NotSelected` AND `Selected`** in the transition table, both yielding
  `Selected` (binding — H2). The `Selected → Selected` case tolerates the §7.D pre-commit: `CommitSelected`
  CAS-stores `Selected` *before* enqueuing `evSelectAccepted`, so when `run` dequeues it the atomic is already
  `Selected`; `transition(Selected, evSelectAccepted) = (Selected, true)` then makes the store a no-op while the
  `lastReacted`-keyed dedup still fires the entering-Selected reaction/notify exactly once as `(NotSelected →
  Selected)`. A true duplicate `evSelectAccepted` (`lastReacted` already `Selected`) is a clean no-op. Without
  this table entry the reaction is silently dropped (the bug Codex round-1 caught); without the `prev =
  lastReacted` rule the notify would report `prev == next == Selected`, violating the idempotent-async property.
- **`evClose` is valid from ALL THREE states** (`NotConnected`/`NotSelected`/`Selected`), all → `NotConnected`
  (binding — fixes the Close-hang Codex round-2 caught). From `Selected` the transition fires the reaction
  (farewell + teardown init); from `NotSelected` it fires the reaction (no farewell, `prev != Selected`) →
  teardown init; from `NotConnected` no transition fires (already terminal, `lastReacted == NotConnected`), so
  the supervisor's `evClose` handler **additionally and unconditionally calls `s.closeEpoch.Load().teardown(timeout)`**
  (the epoch pinned by `requestClose(e)`; idempotent `closeOnce`), AFTER the dedup'd reaction. Because
  `Open(OpenBackground)` returns before selection, a `Close()` during active-dial / passive-wait arrives while
  `NotSelected` or `NotConnected`; without the all-states `evClose` + this unconditional ensure-teardown, the
  reaction would no-op and `Close`'s `e.wait()` would block on `done` forever. The ensure-teardown is idempotent
  and never races the farewell: a farewell is only ever sent on the `Selected → NotConnected` reaction (which
  initiates teardown first, winning `closeOnce`); at `NotConnected` there is no farewell, so the ensure-call is
  simply the initiator. Pinning the epoch (not reading `c.cur`) guarantees the supervisor tears down the SAME
  generation `Close` is waiting on, even if a reconnect were racing (the G2 fence, §5.2, prevents a new `cur` after
  `shutdown`, but pinning is belt-and-suspenders and removes the read-race entirely).
- **The `events` channel is the supervisor's COMMAND queue — enqueues are GUARANTEED, never dropped** (binding —
  fixes the lossy-`evSelectAccepted` Codex round-2 caught). `inject(ev)` uses a **blocking bounded** send (or a
  channel sized so a command cannot be lost); dropping `evSelectAccepted` after `CommitSelected` pre-committed
  `Selected` would strand `lastReacted == NotSelected` and lose the entering-Selected reaction; dropping
  `evClose` would hang `Close`. The drop-OLDEST coalescing + Warn discipline applies **only to the `notify`
  channel** (best-effort state notifications), NOT to `events`.
- `emit(sc)`: a **non-blocking drop-OLDEST** send onto the single `notify` channel — the sole sender, on a full
  buffer, drops the oldest buffered notification (`select { notify<-sc default: { <-notify (nonblock); notify<-sc } }`,
  incrementing `droppedNotify` + rate-limited Warn). The supervisor **never blocks**. Drop-oldest guarantees the
  **latest** state always reaches the handler eventually (so a momentarily-slow consumer, once caught up, sees the
  current state — including a final NotConnected). **F1 = the ordering guarantee** (the NotConnected transition is
  *emitted* before the notifier is stopped, never skipped); under a *pathologically* stalled handler an intermediate
  notification may coalesce, but `conn.State()` is the authoritative current state (lock-free) — handlers MUST NOT
  block, and that is documented.
- `notifier`: `for sc := range notify { for each handler { recover-guarded h(sc.prev, sc.next) } }` — single
  channel, panic-isolated (H4); handlers read from the **Connection's** `atomic.Pointer` to an immutable slice
  (persist across Open/Close). A slow/blocked user handler delays delivery but, because every supervisor send is
  non-blocking, never blocks the supervisor or the `events` drain (so a concurrent `Close()`'s `inject(evClose)`
  always makes progress).

### 5.4 Public API surface + the sealed core↔transport seam (package boundary — RESOLVED)

The engine is an **unexported** concrete `hsms` type; the app holds the **`hsms.Connection` interface**; the
seam is two module-private interfaces (bidirectional engine↔transport). The exact method sets below are the
**design contract** — refined in the plan/Codex loop (the §5 header note applies).

**Two seam interfaces.** `transport` (what the core drives) is an **unexported type with exported methods**, so
`hsmsss`/`secs1` implement it cross-package yet consumers cannot name it. `TransportRuntime` (what the transport
calls back into) is an **exported type** (`hsmsss` must spell it in its `Start` signature) but **sealed**:
`DeliverOwnedFrame` takes an owned `[]byte` frame that only the in-module recv loop produces, and
`WriteMessage`/`SendAsync` take `hsms.Message` whose body is the unexported `wire.Body` — so an external type can
*name* `TransportRuntime` but cannot usefully implement or drive it (it can never obtain an instance, which the
unexported engine alone constructs and hands only to the unexported `transport.Start`).

```go
package hsms

type transport interface { // UNEXPORTED type, EXPORTED methods (implementable cross-package, unnameable by consumers)
    Start(ctx context.Context, rt TransportRuntime) error // dial/listen + spawn recv loop; drives rt
    Stop(ctx context.Context) error
    Write(ctx context.Context, bufs net.Buffers) error    // low-level writev byte sink over the raw *net.TCPConn (§6.2)
    SetReadDeadline(t time.Time) error                    // T8 / idle deadline control
    SetWriteDeadline(t time.Time) error
}

type TransportRuntime interface { // exported (hsmsss spells it) but SEALED by internal-token / wire.Body params
    TCPUp(conn net.Conn)                 // → evTCPUp
    TCPDown(cause error)                 // → evDisconnect; cause classifies graceful vs comms-failure (§5.2, §9.1.1)
    CommitSelected() (committed bool)    // H2 synchronous responder commit before Select.rsp (§7.D) — guarded CAS in core
    SelectLost()                         // → evSelectLost
    DeliverOwnedFrame(frame []byte) error // owned-frame recv entry → existing unexported hsms.decodeOwnedFrame (§6.1)
    RouteReply(msg Message) bool         // reply-correlation hit? (§5.5)
    RouteData(msg *DataMessage) error    // → session fan-out (recvDataMsg)
    WriteMessage(ctx context.Context, msg Message) (Message, error) // core-owned W-bit/sync writev (uses wire.Body)
    SendAsync(ctx context.Context, msg Message) error               // core-owned async enqueue (sendCh)
    State() ConnState
    Done() <-chan struct{}               // generation cancellation, SELECT-ONLY (J5) — replaces the old connDone()
    Timers() TimerConfig
    SessionID() uint16
}
// Transport-detected transitions enter via these NAMED methods (TCPUp/TCPDown/CommitSelected/SelectLost), so the
// fsmEvent type (§5.3) stays UNEXPORTED — the transport never names a raw event constant. WriteMessage/SendAsync
// live on the core (not the transport) precisely so framing+writev can touch the unexported wire.Body /
// net.Buffers without a public Buffers() (§4.7 ban); transport.Write is only the post-framing byte sink.
```

**App-facing public API (all in `hsms`).** `hsms.Connection` is the held/swapped interface (v1's portability role,
trimmed); the concrete engine (`connection`) and session (`session`) are unexported — `connection` implements
both `Connection` and `TransportRuntime`, `session` implements `SECS2Endpoint`.

```go
package hsms

type Connection interface { // the app holds THIS; hsmsss.New / secs1.New return it
    SECS2Endpoint
    Open(ctx context.Context, mode OpenMode) error
    Close() error
    State() ConnState // connection.State() returns NotConnectedState when sup.Load()==nil (before first Open / after
                      // Close); else the supervisor's atomic state. NEVER nil-derefs (Codex round-7).
    Metrics() *ConnectionMetrics
    UpdateConfigOptions(opts ...ConnOption) error
}

type ConnState uint32 // NotConnected, NotSelected, Selected
type StateChangeHandler func(prev, next ConnState)
type DataMessageHandler func(msg *DataMessage, ep SECS2Endpoint) // ep (interface), NOT *session — keeps session unexported

type SECS2Endpoint interface { // the handler's capability surface AND the cross-transport swap contract
    ID() uint16 // no ctx — not blocking I/O
    SendDataMessage(ctx context.Context, stream, function byte, replyExpected bool, item secs2.Item) (*DataMessage, error)
    SendDataMessageAsync(ctx context.Context, stream, function byte, replyExpected bool, item secs2.Item) error
    SendSECS2Message(ctx context.Context, msg secs2.SECS2Message) (*DataMessage, error)
    ReplyDataMessage(ctx context.Context, primary *DataMessage, item secs2.Item) error
    AddDataMessageHandler(handlers ...DataMessageHandler)     // no ctx — registration
    AddConnStateChangeHandler(handlers ...StateChangeHandler) // no ctx — registration
}
```

`hsmsss.New(cfg hsmsss.Config) (hsms.Connection, error)` builds the HSMS-SS transport and wires it to a fresh
engine through an **exported `hsms` constructor** (e.g. `NewConnection(cfg *ConnectionConfig, tr transport)
(Connection, error)`) that takes the unexported `transport` param; `hsmsss` passes its concrete value. **Accurate
sealing claim (Codex round-8):** because `transport`'s methods are exported, a consumer *could in principle* pass
their own conforming value to `NewConnection` (Go does not require naming the parameter type) — so this is NOT
hard-sealed. What IS guaranteed: (a) no `internal/*` type appears in the signature (the rule that matters — the
public-surface CI gate is unaffected; `transport` is unexported-in-`hsms`, not an `internal/*` type, and a consumer
cannot *name* it to declare conforming types or satisfaction asserts); (b) the **supported, documented** entry
points are `hsmsss.New` / `secs1.New` (SP5b) — `NewConnection` is an undocumented in-module wiring seam, not an
advertised extension API. (If a hard seal is later wanted, gate `NewConnection` behind an `internal/` capability
token and add it to the CI-gate whitelist — deferred; not required for SP5a.) The earlier "no app-level extension
API / consumers cannot call it" phrasing is corrected to this.

> **Change from the earlier sketch:** `DataMessageHandler` now receives `SECS2Endpoint` (interface) instead of a
> concrete `*Session`, so the session type need not be exported. The handler keeps the full send/reply capability
> surface; it loses only access to concrete session methods that were never part of the portability contract.

- `recvDataMsg` fan-out delivers the **same immutable pointer** to every handler (Clone gone under D7);
  every `select` includes `<-rt.Done()` so it never blocks on a full handler channel (J5); plain returns at
  the empty-handler / done exits. All per-handler fan-out goroutines are lifecycle-joined before close (G3).
- `selectSession` / `separateSession` are control procedures on the active-open / teardown path and run as
  **joined one-shot tasks** via `epoch.spawn`, never raw `go func()` (G3, the `27b0a7f` fix).

### 5.5 Send / reply model

- **Send (hybrid, D5a-1):** no lifetime channel, no send-gate. `epoch.sendCh` dies with the generation, so a
  frame stranded by a send racing teardown sits in the *old* epoch's channel (consumed if still draining,
  else GC'd) and **cannot** be flushed as generation N+1's first frame — C1 dissolves structurally, and with
  it `sendMu`/`sendClosed`, the `context.Background()` drain, and the `sendMu > ctxMutex` lock-order class
  (A1). Context is stored via `atomic.Pointer` uniformly.
- **B1 IsSelected gate is BINDING on BOTH send entry points (not deferred).** Every *data*-message send —
  the synchronous-writev W-bit/Sync path (`sendWaitReply`) **and** the async `sendCh` path (`sendAsync`) —
  MUST refuse `DataMsgType` while `!IsSelected()` **before** committing/enqueuing the send, incrementing the
  single `dropNotSelected` chokepoint (B3). Control messages are deliberately **not** gated (the Select
  handshake needs them while NotSelected). A message that passes the gate but finds the link no longer
  Selected at the write boundary yields a **counted, non-fatal** `ErrNotSelectedState` (B2) — never a
  teardown. This chokepoint is what prevents the NotSelected→NotConnected reconnect livelock.
- **Reply correlation:** v2 has no `msg.ID()`; the key is `SystemBytes() [4]byte`. One per-generation
  registry, one **unified** result channel; the **sender owns the channel's whole lifecycle** (register +
  defer-delete; nobody else touches it). The receiver only does a **non-blocking** send into the cap-1
  channel. A late send lands in the buffer or hits `default` and is GC'd — no `close()` anywhere, so F5's
  send-on-closed panic is unreachable and all orphan defense (F6) vanishes. This **erases the hsmsss/secs1
  `close(ch)` asymmetry (K1)** — the same core serves both transports.
- **System Bytes uniqueness is BINDING (E37 §8.2.6.8/§8.2.6.9).** The `Store(key,ch)` + `defer Delete(key)`
  registry is correct **only** if no two concurrently-open transactions share `SystemBytes()`. The core's
  System-Bytes generator MUST guarantee the request System Bytes of every **outbound** Primary Data
  message, Select.req, Deselect.req, Linktest.req, **and Separate.req** (E37 Table 6; §8.3.22 — Separate.req
  is identical to Deselect.req except SType — so **both** normal and the teardown farewell Separate are
  covered) are **unique among all currently-open transactions and the most recently completed transaction**
  (§8.2.6.8); and every **reply** MUST reuse the originating request's System Bytes (§8.2.6.9) — which is how
  `routeReply` correlates. (v1 had the same requirement; SP5a makes it an explicit core invariant with a
  teeth-test.)
- **Caller-`ctx` cancellation contract (D5a-7, Codex D).** A W-bit send's wait ends at the earliest of
  {reply, protocol timer (T3/T6), connection drop (`epoch.ctx`), **caller `ctx`**}, with **distinguishable
  errors** (caller-cancelled `ctx.Err()` vs `ErrT3Timeout`/`ErrT6Timeout` vs `ErrConnClosed`). **Cancelling
  the wait does NOT un-send the primary or abort the protocol transaction** — once `writeFrame` returned the
  bytes are on the wire, and E37 has no "cancel transaction" procedure. The sender's `defer Delete(key)`
  deregisters its reply channel on return; a reply that arrives later misses the registry and routes as an
  **unsolicited** secondary (or is dropped) — **exactly the existing T3-timeout behavior**; ctx-cancel is
  just a caller-configurable early T3. The library **MUST NOT** send a Reject or Separate to the peer merely
  because the local caller gave up — that would misrepresent a local scheduling decision as a peer protocol
  error. No leak (immutable messages; sender-owned channel GC'd); System-Bytes uniqueness keeps the late
  reply from colliding with a new transaction.

```go
type replyResult struct { msg hsms.Message; err error }
type replyRegistry struct { m xsync.MapOf[[4]byte, chan replyResult] }
// sendWaitReply(callerCtx, msg): B1-gate (data only); key=SystemBytes(); ch=make(chan replyResult,1); Store; defer Delete;
//   synchronous writeFrame (== on-wire, §9.4.1.2);
//   if DATA W-bit: incDataMsgInflightCount (I1, once, after on-wire) and dec in ALL FOUR terminal branches.
//   if CONTROL (T6 transaction): the inflight gauge is a DATA metric — DO NOT touch it (I1).
//   select { ch (reply) | timer(T3 data | T6 control) | epoch.ctx.Done() (conn drop → ErrConnClosed) | callerCtx.Done() (→ ctx.Err()) }
//   — four distinguishable outcomes; routeReply never touches the gauge.
// routeReply: Load(SystemBytes); hit ⇒ non-blocking send; miss ⇒ routeUnsolicited (control-rsp ⇒ Reject(TransactionNotOpen,3) §8.3.20; data-secondary ⇒ session.recvDataMsg).
```

### 5.6 Config & metrics

- `ConnectionConfig` + the `With*` options carry over largely as-is (model-independent); drop the
  multi-session / `IsGeneralSession` surface; add `WithSessionID(id)`. `UpdateConfigOptions` keeps the
  **transactional** apply (validate-all against a scratch snapshot, `errors.Join` on failure, commit
  atomically, never half-mutate the live config — landmine D).
- `ConnectionMetrics` (atomic counters) carries over: `Linktest{Send,Recv,Err}Count`,
  `DataMsg{Send,Recv,Err}Count`, `DataMsgDropNotSelectedCount` (one chokepoint, exactly-once, rate-limited
  Warn — B3), `DataMsgInflightCount` (I1), `ConnRetryGauge`.

## 6. HSMS-SS transport (`hsmsss` package)

### 6.1 Framed reader + decode-owned-frame

- The recv loop reads SEMI E37 §7 frames: 4-byte BE length, then 10-byte header + body. Idle-vs-T8 deadline
  split (idle timeout before the first byte; **T8 governs every gap after `totalRead>0`, including across the
  length header**, §9.2.3.1 / J1); **validate `10 ≤ len ≤ secs2.MaxByteSize` before allocating** the body
  buffer — the lower bound is the mandatory 10-byte message header (E37 §8.2.4 / §8.3.2); a length `< 10` is a
  protocol error, not a zero-body data message (J2: never allocate or trust an attacker-controlled length);
  peek PType/SType, and on PType≠0 or unsupported SType build a typed reject and answer Reject.req while
  keeping the link (J3, §7.10.3).
- **Control frames are header-only (E37 §9.3.3.1, §8.3.x).** For the standard non-data control STypes
  (Select.req/rsp, Deselect.req/rsp, Linktest.req/rsp, Separate.req, Reject.req), PType MUST be 0 and the
  Message Length MUST be exactly **10** (header only — no message text). A standard control frame with
  `len != 10` or `PType != 0` is malformed: reject it (Reject.req with the appropriate reason, keeping the
  link) and do not deliver it as if it carried a body. Only data messages (SType 0) carry a body
  (`len > 10`).
- The frame buffer is a fresh `make([]byte, msgLen)` (GC-owned); **the read buffer is NOT pooled** (D5a-2 /
  §5.F option (b)). **The HSMS frame entry is already built and already sealed-by-visibility:**
  `hsms.decodeOwnedFrame([]byte) (Message, error)` exists today (`hsms/decode.go:60`), is unexported, validates
  `len ≥ 10` (J2), and retains the body zero-copy via `wire.AdoptBody` — so it needs **no capability token**. The
  recv loop in `hsmsss` allocates a fresh `make([]byte, msgLen)`, reads the frame into it, and hands it to the core
  via `TransportRuntime.DeliverOwnedFrame(frame []byte) error`, which calls the existing `decodeOwnedFrame` (byte
  ownership stays inside the module — `TransportRuntime` is only usefully implementable in-module). The **one**
  capability token genuinely needed is `framecodec.OwnedSECS2Body` (a **new `internal/framecodec` package**, built
  only via `framecodec.AdoptSECS2Body([]byte)`), because the remaining clone is at *item-decode* time:
  `DataMessage.decode()` today does `secs2.Decode(msg.body.AppendTo(nil))` — **two** body copies (`AppendTo(nil)`
  alloc + `bytes.Clone`). SP5a adds `secs2.DecodeOwned(framecodec.OwnedSECS2Body) (Item, error)` (no `bytes.Clone`)
  and reroutes `decode()` through it so leaf items zero-copy-reference the retained frame body (proposal §5.B).
  `secs2.DecodeOwned` may be a **name-exported** function, but it is **capability-sealed**: external callers cannot
  construct a `framecodec.OwnedSECS2Body`, so it creates **no public caller-owned aliasing contract** (D7 / the
  public copy-only body rule hold). `secs2.Decode([]byte)` is unchanged — it still `bytes.Clone`s
  (`secs2/decode.go:22`), and the public API stays copy-only. `framecodec` imports neither `hsms` nor `secs2` (it
  would cycle — `secs2` imports `framecodec`); the token therefore **cannot** live in `internal/wire`, which already
  imports `secs2`. Routing the recv path through the owned variant eliminates the raw-frame
  double-copy (SP4 forward-note (a)) and makes proposal §5.B (leaf items zero-copy-reference the frame) real. The
  aliasing discipline (E3) lives entirely on the in-repo `framecodec`/`wire` seam — never on an external caller: a
  decoded message that aliases the frame must not outlive that frame's buffer. `ReplyDataMessage` is safe because
  `SystemBytes()`/`HeaderBytes()` are `[N]byte` **values** in v2, not retained slices.

### 6.2 writev send (mandated, §4.11)

`writeFrame` writes `net.Buffers{prefix, body.Buffers()...}` over the **raw `*net.TCPConn`** (a `bufio.Writer`
defeats `writev` — the v1 64 KB writer must go), where `body` comes from `internal/wire` imported **directly**
by `hsmsss` (the public `hsms` API stays copy-only — no `WriteFrameTo`, no public `Buffers()`, the
zero-copy-writer BAN §4.7). Bind a **fresh** `Buffers()` per call (it is consumed/advanced, so a retransmit is
another call). Small control frames may use the single-slice path; the win is the large-body data path. The R2
release-gate benchmark (restamp + `net.Buffers` is O(14), not O(body); body not copied) is part of SP5a.

### 6.3 Procedures, roles, timers

- **Active/passive roles** stay static config (not negotiated). Active dials (exponential backoff capped at T5), runs the
  Select procedure (Select.req, SessionID `0xFFFF`, T6-bound) on reaching NotSelected, and reconnects.
  Passive listens/accepts (refuse a 2nd connection), does not initiate Select, reconnects through the FSM.
- **Procedures:** Select / Linktest (auto-linktest on a configurable interval while Selected, D5a-5) /
  Separate / Reject / Deselect (D5a-4), per §3.
- **Deselect (D5a-4, responder-only, §7.7 / §9.3.1).** HSMS-SS normally uses Separate; SP5a implements
  Deselect only far enough to answer a peer gracefully (ignoring a peer's Deselect.req would strand it on
  T6). On receiving **Deselect.req** (SType3): if currently `Selected` for that SessionID → reply
  **Deselect.rsp** (SType4) with status **0 (success)** and transition `Selected → NotSelected`; if **not**
  Selected → reply Deselect.rsp with a non-zero (failure) status and do not transition. SP5a does **not**
  initiate the Deselect procedure (no outbound Deselect.req — it uses Separate for teardown); the inbound
  responder path is bounded by the peer's T6, not ours. (A strict-E37.1 build that drops Deselect entirely
  is a future option, §10.)
- **Timers** T3/T5/T6/T7/T8 wired to the epoch ctx; timers use `internal/pool.GetTimer`/`PutTimer` (the sole
  sanctioned internal pool). **T7 (NOT-SELECTED dwell, §9.2.2) is armed on *entering* `NotSelected` and MUST
  be cancelled/fenced on *leaving* it** — i.e. on the `NotSelected → Selected` transition (and on any move to
  `NotConnected`). It is a one-shot `go func(ctx)` (transition-only, G3-exempt) that fires `evDisconnect` →
  `NotConnected` **only if** the connection is still `NotSelected` when it expires; a Select that completes
  before T7 cancels it. Implementation: cancel via a per-NotSelected-entry sub-context (or a generation+state
  fence the timer re-checks before firing) so a validly-Selected session is never torn down by a stale T7.
  E37 §9.2.2 permits the T7 disconnect *only* while still NOT SELECTED.

## 7. Binding concurrency guards (from the adversarial review — non-negotiable)

- **A. Bounded teardown join.** The join reports a `"close timeout"` error (with live-task count) rather than
  hanging — never an inline unbounded `wg.Wait()`. (Regresses v1 robustness otherwise.)
- **B. Add-vs-Wait happen-before guard.** `spawnMu` + `closing` (≡ v1 `task.go:68` `taskMu`) makes a 0→1
  `wg.Add` concurrent with `Wait` impossible; the live trigger is reconnect-spawn racing `Close()`. Deleting
  this guard is a real `WaitGroup` misuse — keep it; teeth-check under `-race -count=N`.
- **C. Reconnect-loop join stays SEPARATE** from `epoch.wg`, and the teardown join runs on a *spawned*
  goroutine, so the goroutine that *triggers* teardown never blocks inline on a set it could belong to
  (avoids a deadlock cycle).
- **D. H2 §7.4.3 synchronous responder commit (highest-risk item).** The supervisor is the sole writer for
  every transition **except** the Select-**responder** commit: on accepting an **inbound Select.req** (the
  responder path — passive HSMS-SS, or simultaneous-select §7.4.3) the **receiver goroutine** performs a guarded
  CAS `NotSelected→Selected` **directly** on `state`, synchronously, **before** writing Select.rsp, then enqueues
  `evSelectAccepted` for reactions + notification. **The active *initiator* path ALSO calls `CommitSelected`**
  (corrected 2026-07-01, T21 build + review): when the initiator's recv loop routes a success (status-0)
  Select.rsp, it calls `CommitSelected()` **on the recv goroutine, before dispatching the next frame** — because
  a compliant peer may pipeline a data message immediately after its Select.rsp, so the initiator is equally
  exposed to the efb220b race, and the recv goroutine is the *only* place the commit is guaranteed to
  happen-before the next-frame dispatch (a commit on the separate Select-sender goroutine, or a plain async
  `evSelectAccepted` enqueue, races the recv loop and loses — teeth-verified 10/10 spurious Rejects). Both
  responder (on inbound Select.req, before writing Select.rsp) and initiator (on inbound status-0 Select.rsp,
  before the next frame) commit via the same synchronous `CommitSelected` CAS on the recv goroutine. The
  earlier "initiator does not need the commit" wording was wrong. **Exactly-once reaction is guaranteed by the §5.3 mechanics, not by chance:** the
  transition table makes `evSelectAccepted` valid from BOTH `NotSelected` and `Selected` (the latter tolerating
  the pre-commit), and the supervisor dedups on its own `lastReacted` with the reported `prev = lastReacted`
  (NOT prev==next of the atomic). Without the `Selected + evSelectAccepted` table entry the supervisor reads the
  pre-committed `state==Selected`, finds no legal transition, and **silently drops the entering-Selected
  reaction/notification** (the bug Codex round-1 caught). The naive "supervisor is the sole writer" instead
  reintroduces the `efb220b` spurious-Reject(NotSelected) bug on a peer that pipelines data after Select.rsp.
  **The teeth-test for this guard MUST be the responder scenario** (a passive side, or a peer that sends
  Select.rsp *then* immediately a data message) — an active-initiator-only test does not exercise the commit.
  (Fallback if a CAS-on-`state` from the receiver is deemed too invasive: a separate synchronously-set
  `selectAccepted atomic.Bool` consulted by the data gate as `IsSelected() || selectAccepted` — keeps `state`
  strictly single-writer; CAS approach ratified for the plan.)
- **E. Farewell Separate is BOUNDED best-effort and never blocks teardown; `closeSocket()` is unconditional**
  (§5.2). On a *graceful* teardown from a live Selected link, attempt a synchronous `writeFrame(Separate.req)`
  with a **bounded `writeMu` acquisition + short write deadline**; on failure, skip and proceed. On a
  *communication-failure* teardown, send **no** Separate and terminate immediately (E37 §9.1.1). `closeSocket()`
  always runs (inside `closeOnce`) and is what unblocks any wedged writer (J5) — it is **never** gated behind
  the farewell write, so teardown cannot deadlock behind a concurrent `writeMu` holder. On the wire the order
  is still Separate-then-close when a courtesy Separate is sent (§7.9.1 from SELECTED, §6.4 close in NOT
  SELECTED; peer ignores Separate when not Selected, §7.9.2). Funneling teardown through the single
  NotConnected reaction (never a racing direct `Close()`→`teardown()`) is what licenses replacing `IsOpened()`
  with `liveConn()!=nil`. Document alongside J5 (close TCP before the join).

**Landmine coverage map** (each catalogued landmine A–K → where it is handled): carried verbatim from the
architecture review §2 table into the plan's Global Constraints, so every fix's invariant is an explicit
plan requirement (not rediscovered during the build).

## 8. Correctness-test strategy (D5a-6: full harnesses + risk-tiered regressions)

The connection-layer test infra exists on `v2` but does not compile (42 `.Free()` sites + references to the
not-yet-rewritten types). SP5a ports + re-grounds it. Rules: `.agents/rules/300-testing.md` (never
`time.Sleep` to wait for state — subscribe to state-change events / `require.Eventually` on counters;
`t.Context()`; `for b.Loop()` benches; testify; in-package `mock_*_test.go` patterns, no mock framework).

**Ported harnesses (model-independent, re-pointed to the v2 types):**
- `MockConn` / `MockSession` (recreated against the v2 `transport`/`Session` seams).
- `ChaosProxy` + `ByteChaosProxy` (filter-driven MITM TCP proxy: Forward-delayed/Drop/CloseTCP/Truncate +
  sub-frame byte faults → T3/T6/T8/Reject scenarios).
- `FuzzConnectionLifecycle` (byte→op, real active+passive pair, per-iteration watchdog → `t.Skip` on hang)
  and `FuzzMessageReader` (arbitrary bytes → reader; never-panic + err==nil⇒msg≠nil invariant). `make
  stress-test` keeps `-skip '^Fuzz'`; `make fuzz-test` runs them.
- The `idempotent_async` property test (no async handler ever sees prev==next across open/select/close cycles).
- The integration two-Connection real-TCP harness (short-timeout baseline).

**Clean-shutdown gate (re-derived for v2 fields, §6.7 of the territory map):** `assertCleanShutdown` polls to
zero: no live generation (`cur.Load()==nil` or `<-done` closed), `DataMsgInflightCount==0`, per-generation
`wg`/live-task-count==0, per-generation reply registry empty. (`replyErrs` is gone; the v1 five-counter set is
replaced.) Hard-fail, teeth-verified.

**Risk-tiered teeth-verified regressions** (each reintroduces the bug to confirm the test bites): the
high-risk cluster — close/shutdown (F1 terminal-event delivery, F2/F3 close-timeout, F4 single-owner, F5/F6
reply teardown), reconnect (G1 join-before-fresh-ctx, G2 generation fence, G3 joined producers), the
IsSelected gate (B1/B2/B3), stale-frame (C1), simultaneous-select (H2), inflight balance (I1), T8/recv-stall
(J1/J5), reject-on-unsupported (J3), orphan-control Reject (§8.3.20). Plus the **new design's own guards**:
the §7.B Add-vs-Wait race, the §7.A bounded-close timeout reporting (port the `CloseTCPIdempotent`/close-timeout
teeth-check — historically needed `-count=2000`), the §7.D H2 receiver-commit, and the per-generation
channel-GC C1 dissolution. Landmines that dissolve under D7/immutability (the E-cluster Free/double-Free
machinery) get a **documented note**, not a test.

## 9. Success criteria (acceptance)

1. `go build ./...` and `go vet ./...` green on `v2` (today they fail); `golangci-lint` clean.
2. A working HSMS-SS connection: active+passive pair selects, exchanges W-bit (reply matched, T3 enforced)
   and async data, control messages (Linktest/Separate/Reject), reconnects after a drop, and `Close()`
   returns promptly (bounded) and cleanly.
3. The ported harnesses + the chosen regressions green under `make test` (`-race`); the clean-shutdown gate
   passes; the fuzzers run via `make fuzz-test`; `make stress-test` (two scheduler regimes, `-count`, `-race`,
   `-skip '^Fuzz'`) passes.
4. **Public-surface CI gate (proposal success criterion #4):** the public `secs2`/`hsms`/`secs1`/`hsmsss`
   surface compiles with no mutating method, no `Free`/pool, no `usePool` global; **no `internal/*` type appears
   in any exported signature/Godoc/README, with EXACTLY ONE whitelisted exception — `secs2.DecodeOwned(framecodec.
   OwnedSECS2Body)`** (the capability seal, §6.1; the token is unconstructable by consumers since `internal/
   framecodec` is unimportable outside the module). The gate must FAIL on any other internal-type leak (incl. any
   second `framecodec`/`wire`/`pool` signature); no public `WriteFrameTo`/`Buffers()`.
5. The writev path is benchmark-proven: restamp + `net.Buffers` send is O(14) not O(body), body not copied
   (R2 gate). The decode-owned-frame path eliminates the raw-frame double-copy (re-run the SP4
   `BenchmarkDataMessage_Item_RawFrame` — alloc count drops from the SP4 baseline). The §5.F (a)-vs-(b)
   benchmark is recorded.
6. Every landmine in the §7 coverage map is either guarded-with-a-test (risk-tier) or carries a documented
   dissolution note; no catalogued invariant is silently dropped.

## 10. Open items for the implementation plan

Resolved during the Codex review loop (round 1) and now binding above — no longer open: the **decode-owned-frame
entry is module-private** (§6.1, not exported); **B1 gates both send entry points** (§5.5); the **hybrid send
boundary** is specified (§5.5: sync-writev for W-bit/Sync, async `sendCh` for fire-and-forget); **double-`Open()`
no-ops** instead of hanging (§5.2); **`Close()` funnels teardown through the single NotConnected reaction** so
the farewell-Separate decision happens-before `closeSocket()` (§5.2/§7.E); **T7 is cancelled on leaving
NotSelected** (§6.3); the **frame-length lower bound is ≥ 10** (§6.1); **Deselect is responder-only and fully
specified** (§6.3). The **package boundary is resolved — "sealed-A"** (§4/§5.4, from the 2026-06-30 Codex
package-boundary consult): the engine lands once in `hsms` as an **unexported concrete type**, `hsms.Connection`
is the app-held **interface**, the seam is an unexported-type/exported-methods `transport` plus a sealed exported
`TransportRuntime`, decode-owned lives in a new `internal/framecodec` (capability tokens), config splits
`hsms.ConnectionConfig` ⊂ `hsmsss.Config`, and `DataMessageHandler` takes `SECS2Endpoint` (not `*Session`).

Ratified by the implementation plan (`tmp/2026-06-30-sp5a-implementation-plan.md`) + its Codex review loop — no
longer open: **H2 commit form** = guarded CAS-on-`state` via `CommitSelected` (the `selectAccepted atomic.Bool`
is the documented fallback only); **`OpenMode`** = the typed `OpenWaitSelected`/`OpenBackground`; **`Shutdown(ctx)`**
= NOT built (the ctx-free `Close()` is already internally bounded); **exact seam method sets** = the §5.4 lists
(`transport`: Start/Stop/Write/SetReadDeadline/SetWriteDeadline; `TransportRuntime`: TCPUp/TCPDown/CommitSelected/
SelectLost/DeliverOwnedFrame/RouteReply/RouteData/WriteMessage/SendAsync/State/Done/Timers/SessionID).

Remaining for the plan / build to ratify:
- **`assertCleanShutdown` counter set (§8)** — confirm the re-derived v2 counters are complete and sufficient.
- **Strict-E37.1 build option (D5a-4)** — decide if a build that drops the responder Deselect is worth offering.

## 11. References

- `docs/v2/00-v2-proposal.md` — D1/D4/D6/D7/D8, §4.7 zero-copy-writer ban, §4.8 internal/wire bridge,
  §5.B/§5.F, §4.11 writev mandate, scope line 488, success criterion #4.
- `docs/v2/06-internal-pooling-decision-4-spec.md` — the internal-pooling contract, §5.F option (b),
  the raw-frame double-copy forward-note.
- `tmp/2026-06-30-sp5-territory-map.md` — v1 reference design + the §7 landmine catalog A–K.
- `tmp/2026-06-30-sp5a-architecture-review.md` — the E37 normative model, the KEEP/MERGE/DROP analysis with
  per-item landmine coverage, the two adversarial passes, the concrete type sketches, and the residual-risk
  list this spec's §10 draws from.
- SEMI E37 / E37.1 (HSMS / HSMS-SS) — the normative standard (no repo copy; cited via §-numbers).
