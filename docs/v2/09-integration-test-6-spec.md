# Sub-Project 6 — Deterministic Integration-Test Architecture

## 1. Scope & goal

SP6 delivers a **deterministic, network-free integration-test architecture** that secures v2's
quality across the two transports built on the shared connection core (SP5a `hsmsss`, SP5b `secs1`).
The per-package suites already built during SP5a/SP5b stay; SP6 adds the layer they cannot cover and
retires the v1 leftovers so the module builds and tests clean end-to-end.

**SP6 delivers:**

1. A **`net.Pipe` transport seam** (`WithDialer`) so the REAL `hsmsss`/`secs1` transports run
   network-free and deterministically against a scripted in-memory peer.
2. **Component-level clock injection** (extending the existing `secs1/assembler.go` `now func()`
   precedent) so timer behavior (T1–T8) is assertable without wall-clock flakiness — no core-wide
   `Clock` refactor.
3. A new **`integration/` package** holding the `net.Pipe` harness, reusable **scripted HSMS and
   SECS-I peers**, a **cross-transport parity suite**, and the **FSM `(prev,next)` state matrix**.
4. **Fuzz consolidation** of the existing corpus.
5. **Deletion of all v1 leftovers** (`tests/`, `examples/`, `internal/bakeoff/`) and a **green
   `go build ./...` / `go vet ./...` / `go test -race ./...`** module-wide.

**Non-goals (explicit):**

- **No core-wide `Clock` interface.** Determinism of time uses per-component unexported `now`
  hooks, never a `Clock` threaded through the ~14 core timer sites.
- **No passive-side accept factory.** `WithDialer` covers the ACTIVE dial path only. A full
  two-`go-secs` active↔passive pair still runs over loopback TCP; in-memory determinism is achieved
  by driving ONE `go-secs` connection over `net.Pipe` against a scripted peer.
- **No gem, examples, migration, or codemod** — that is SP7. SP6 DELETES the v1 `examples/`; SP7
  rebuilds fresh runnable examples.

## 2. Locked decisions (user, brainstorm 2026-07-03)

- **D6-1 — Full deterministic architecture.** Cross-transport parity + `net.Pipe` + time injection +
  `(prev,next)` FSM matrix + fuzz consolidation (not a minimal cleanup, not a test-doubles-only pass).
- **D6-2 — `net.Pipe` seam + component clocks, NOT a core `Clock`.** One production dial seam plus
  per-component unexported `now` hooks where timer determinism matters. No sweep of the reviewed core
  timer call sites behind an interface.
- **D6-3 — Delete ALL v1 leftovers.** `tests/`, `examples/{device,secs1_device}`, and
  `internal/bakeoff/` are removed. The rewritten README is the usage reference until SP7. v1 originals
  remain in git history.
- **D6-4 — Public `WithDialer`.** `hsmsss.WithDialer` and `secs1.WithDialer` (a `DialContext`-style
  option) are a supported public API — useful to consumers (TLS/proxy/custom dialers) and the seam the
  in-memory suite injects `net.Pipe` through. Active/dial side only.

## 3. Production changes (two small, low-risk seams)

SP6 is overwhelmingly test code. It adds exactly two production seams, both behavior-preserving by
default.

### 3a. `WithDialer` — the active-dial seam (D6-4)

Each transport package gains a dial function type, a config field, an option, and a default.

```go
// package hsmsss (and, identically, package secs1)

// DialFunc dials the active-mode connection. It matches (&net.Dialer{}).DialContext.
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// WithDialer overrides how the active connection is established. The default is
// (&net.Dialer{}).DialContext. Passing nil is a config error. Useful for custom
// transports (TLS, proxy) and for injecting an in-memory net.Pipe in tests.
func WithDialer(dial DialFunc) Option { return func(c *Config) error { /* validate + set */ } }
```

- The `dial` field defaults to `(&net.Dialer{}).DialContext` in `NewConfig`, so the field is a
  **non-nil invariant** and the existing call sites need no nil-guard.
- Wiring: replace `(&net.Dialer{}).DialContext(ctx, "tcp", addr)` at `hsmsss/transport_active.go:107`
  and `secs1/transport.go:202` with `t.cfg.dial(ctx, "tcp", addr)`.
- Passive/listen is UNCHANGED (still `net.ListenTCP`). Out of scope per D6-2.
- Godoc must be consumer-clean (no internal codes) per the project doc rule.

**Conn-type generalization (RESOLVES review P0).** Both active paths today reject a non-TCP conn —
`conn, ok := netConn.(*net.TCPConn); if !ok { return error }` at `hsmsss/transport_active.go:112` and
`secs1/transport.go:209` — and hsmsss stores `conn *net.TCPConn` (`hsmsss/transport.go:70`). A
`net.Pipe` conn from `WithDialer` would be rejected. The change is **bounded and touches NO core code**
because the `hsms.transport` seam already uses `net.Conn` (`Write`/`SetReadDeadline`/`SetWriteDeadline`/
`TCPUp` at `hsms/transport.go:49/56/61/79`):

- **secs1 (minimal):** its `conn` field and the line engine are ALREADY `net.Conn` (`secs1/transport.go:62,115,419`).
  Only two edits: drop the `*net.TCPConn` rejection at the dial site (accept any `net.Conn`), and change
  `applyKeepAlive(conn *net.TCPConn)` (`secs1/transport.go:727`) to take `net.Conn` and apply keepalive
  only when `conn.(*net.TCPConn)` succeeds (skip silently for `net.Pipe`).
- **hsmsss:** generalize the stored field `conn *net.TCPConn` → `conn net.Conn` (`hsmsss/transport.go:70`),
  drop the dial-site rejection, and gate `applyKeepAlive` the same way. Audit every `t.conn` use for a
  `*net.TCPConn`-specific method (only keepalive is expected); the passive side still assigns a real
  `*net.TCPConn` from `AcceptTCP`, which satisfies `net.Conn`.
- The `Write` seam's writev fast path is TCP-only; over `net.Pipe` the write is a plain `WriteTo` (correct,
  just not vectored). Update the seam/`applyKeepAlive` doc comments to say "typically `*net.TCPConn`
  (writev fast path); a custom dialer may supply any `net.Conn`."
- **Acceptance tests (both transports):** a `net.Pipe` dialer completes a real Open→Selected→round-trip;
  keepalive is silently skipped for a non-TCP conn; `WithDialer(nil)` is a config error; the default
  dialer still yields a working TCP connection.

### 3b. Component clock hooks — deterministic timers (D6-2, RESOLVES review P1)

The core's timers are heterogeneous — pooled (`pool.GetTimer`/`PutTimer` for T3 at
`connection_send.go:280`, T6/T7 at `hsmsss/transport_procedures.go:65,144`), raw (`time.NewTimer` for
T5 at `connection_lifecycle.go:440`), and context-based (`context.WithTimeout` for the T6 linktest
bound at `hsmsss/transport_procedures.go:79`). Threading a uniform clock through a FIRED pooled or
context timer is exactly the invasive core-wide `Clock` refactor D6-2 rejects (it entangles the pool
`Put`/cleanup contract and context deadlines). SP6 therefore scopes clock injection NARROWLY, using
ONLY the safe `secs1/assembler.go` precedent:

- **Hook ONLY deadline-COMPARISON sites** — an unexported `now func() time.Time` (default `time.Now`)
  where code computes `deadline := now().Add(d)` and later compares against `now()`, or arms a conn
  `SetReadDeadline`/`SetWriteDeadline`. These are side-effect-free to hook and byte-identical by default.
  Targets: SECS-I inter-block T4 (already done), the SECS-I line T1/T2 conn deadlines (`secs1/line.go`),
  and the T8 recv-idle deadline (`readFrame`). Each conn deadline over `net.Pipe` behaves as over TCP
  (see O6-3, empirically confirmed).
- **Do NOT hook fired pooled/raw/context timers** (T3 reply, T5 reconnect backoff, T6/T7). Their
  determinism comes from configuring SHORT durations via the existing public `WithT3`/`WithT5`/`WithT6`/
  `WithT7` options plus scripted-peer event ordering — no new hook, no `Put`/cleanup contract, no risk
  to the reviewed core. A test that must assert "T3 fires" sets `WithT3(20ms)` and has the scripted peer
  simply withhold the reply.
- **Invariant:** every hook is unexported, set only from in-package `_test.go`, defaults to real time,
  and changes NO production behavior. No hook is added without a consuming deterministic test (teeth:
  deleting the test's set-hook line restores real-time behavior).
- These in-package timer tests stay in-package (they need the unexported hook). The cross-package
  `integration/` suite controls only the TRANSPORT (`net.Pipe`) and event ordering (scripted peers) and
  never asserts a precise fire instant.

## 4. The `integration/` package

New top-level package `integration/` (replaces the deleted v1 `tests/`). It imports the PUBLIC
surface only: `hsms`, `hsmsss`, `secs1`, `secs2`, `sml`. It never imports `internal/*`.

### 4a. `net.Pipe` harness

- `net.Pipe()` returns two connected in-memory endpoints. The `go-secs` **active** connection is
  configured with a `WithDialer` **factory** that, on EACH dial, mints a fresh `net.Pipe()` pair,
  spawns a scripted peer goroutine on `endB`, and returns `endA`. It is a factory (not a captured
  single conn) precisely so reconnect scenarios work: the core's reconnect loop re-invokes the dialer,
  and each generation must get its own pipe + peer. The harness records the per-dial peers so a test
  can address the current generation.
- Because `net.Pipe` is synchronous and unbuffered, the harness peer must read and write concurrently
  with the connection under test; helpers wrap this with deadlines and a result channel (mirroring the
  loopback-peer idiom already used in `secs1`/`hsmsss` tests).
- A loopback variant (`newLoopbackPair`) is provided for the few scenarios that genuinely need TWO
  `go-secs` connections (e.g. simultaneous-select between two real peers), which `WithDialer` alone
  cannot wire in memory.

### 4b. Scripted peers

Reusable, deterministic peers speaking each wire protocol on `endB`:

- **`hsmsPeer`** — Select.req/rsp, Linktest.req/rsp, Data frames, Separate; can inject Reject,
  mis-ordered control, delayed/omitted replies, and byte-level faults. Answers or scripts each step.
- **`secs1Peer`** — the ENQ/EOT/ACK/NAK block handshake, block assembly/split, contention, NAK/retry,
  and T4 gaps; can inject bad checksums, wrong block numbers, and dropped EOT/ACK.

**Serialization requirement (RESOLVES review P2).** Because SECS-I is a strict half-duplex line
protocol, `secs1Peer` MUST run as a SINGLE protocol state machine that owns the `endB` bytes — the
ENQ/EOT/ACK/NAK sequencing is serialized, never split across competing reader/writer goroutines racing
for the line. A peer MAY use a goroutine purely for `net.Pipe` liveness (so an unbuffered write does not
deadlock a concurrent read), but byte ownership and line-turn ordering stay in the one state machine
(mirroring the real `secs1/transport.go` line engine). `hsmsPeer` is full-duplex and has no such
constraint.

Where possible these ADAPT the raw-peer helpers already present in the per-package tests rather than
reinventing them.

### 4c. Cross-transport parity suite (§5)

### 4d. FSM `(prev,next)` state matrix (§6)

## 5. Cross-transport parity suite

A single transport-agnostic scenario table, defined ONCE and run against BOTH transports through the
`hsms.Connection` interface, proving the sealed-A shared core behaves identically regardless of
transport.

```go
// scriptedPeer is the peer abstraction the harness stores; concrete peers (hsmsPeer, secs1Peer)
// add their own scripting methods. dialFactory is generic over peer type — it mints a fresh
// net.Pipe + peer per dial (so reconnect works) and exposes latest() for the current generation.
type scriptedPeer interface{ close() }

type dialFactory struct {
    spawn func(net.Conn) scriptedPeer // builds the peer on endB
    peers []scriptedPeer              // one per dial (generation)
    // dial(ctx, network, addr) mints net.Pipe(), spawns peer on endB, returns endA; latest() → newest peer
}

type transportFactory struct {
    name  string                                           // "hsmsss" | "secs1"
    open  func(*testing.T) (hsms.Connection, *dialFactory) // dial over net.Pipe, Open→Selected, return conn + factory
    close func(*testing.T, hsms.Connection)
}
```

**Scenarios (each asserted identically for both transports):**

1. Open → reach Selected (HSMS: after Select.rsp; SECS-I: auto-commit at TCP-up) → clean Close.
2. W-bit primary → matching secondary reply correlated and returned.
3. Reply from an inbound handler via `ReplyDataMessage` (async path) round-trips.
4. Fire-and-forget `SendDataMessageAsync` reaches the peer.
5. Concurrent bidirectional W-bit exchange (both directions in flight) — no reply cross-talk
   (guards the SP5a System-Bytes reply-correlation fix).
6. Reply timeout (T3) surfaces as an error; the connection stays usable.
7. Involuntary peer drop → the core tears down and (active) reconnects to Selected.
8. Metrics: `DataMsgSend`/`DataMsgRecv`/`DataMsgErr` move identically through both transports.
9. State-change handlers fire the correct `(prev,next)` sequence for the connect/close cycle — the
   expected sequence is **transport-specific**, NOT identical across transports (RESOLVES review P1):
   HSMS-SS observes NotConnected→NotSelected (TCP up) then NotSelected→Selected (Select accepted);
   SECS-I observes a single NotConnected→Selected (auto-commit at TCP-up, no Select frame — SP5b
   `secs1/transport.go:248`). The parity assertion is "each transport emits ITS defined sequence," not
   "both emit the same one."
10. **Generation isolation across Close/reopen** (guards the SP5a/SP5b stale-epoch regression class —
    RESOLVES review P1): fire concurrent async sends WHILE Close tears the generation down, then
    reconnect a fresh generation; assert no stale frame from the old generation is delivered to the new
    peer, and a clean W-bit round-trip succeeds on the new generation afterward.

Scenarios that are inherently transport-specific (HSMS Select/Deselect/Linktest control; SECS-I
contention/RTY/multi-block) stay in their per-package suites and are NOT forced into the parity table.

## 6. FSM `(prev,next)` state-transition matrix (RESOLVES review P1)

Coverage of the core `ConnState` machine via the public `StateChangeHandler(prev, next)`, split by what
each transport can actually drive (SECS-I has no Select layer, so a single "through both transports"
matrix would be wrong):

- **Shared-core transition table (unit-level, `hsms/supervisor_test.go`):** every legal transition among
  `NotConnected`/`NotSelected`/`Selected` and every no-op event, with events synthesized directly. SP6
  does not move these; it references them as the authority for the abstract table.
- **hsmsss end-to-end matrix** (via the scripted HSMS peer): NotConnected→NotSelected (TCP up),
  NotSelected→Selected (Select accepted), Selected→NotSelected (Select lost / Deselect),
  NotSelected→NotConnected (T7 dwell), any→NotConnected (disconnect / Close). Assert each observed
  `(prev,next)` and that no-op events (T7 from Selected, duplicate Select) emit nothing.
- **secs1 end-to-end matrix** (via the scripted SECS-I peer): NotConnected→Selected (auto-commit at
  TCP-up) and Selected→NotConnected (involuntary drop / Close). SECS-I NEVER drives Select accepted/lost
  or T7, so those rows are ABSENT by design — asserting their absence is part of the coverage.

This proves the transitions are observable end-to-end at the public boundary per transport; it does not
replace the unit-level table.

## 7. Fuzz consolidation

- Inventory the existing fuzz targets (`hsmsss` lifecycle fuzz, `sml`/`secs2` decode fuzz) and gather
  the connection/transport-facing ones under the `integration/` package where they exercise the public
  surface; leave decoder fuzz in its owning package.
- Keep the `-skip '^Fuzz'` convention for `-count` stress runs (a known `FuzzConnectionLifecycle`
  hang under high `-count`); `make fuzz-test` runs the targets for a bounded time.
- Add fuzz where thin: a cross-transport send/receive round-trip fuzz (random S/F + item) that asserts
  the parity invariant holds under arbitrary inputs.

## 8. Leftover deletion & green build (D6-3)

Delete outright:

- `tests/` (the v1 integration suite — already ported into the per-package suites; superseded by
  `integration/`), including the standalone `active_host`/`passive_eqp`/`passive_host` binaries and
  the `*.sh` scripts.
- `examples/{device,secs1_device}` (v1 API; SP7 rebuilds examples).
- `internal/bakeoff/` (a one-time perf bench; its verdicts are recorded in
  `docs/v2/01-bakeoff-results.md`).

Makefile updates:

- `STRESS_DIRS` : drop `./tests/...`; add `./integration/...`.
- `stress-quick` : repoint the `./tests/hsmsss_integration/...` selector at the new package (or drop
  it if the flake-prone tests now live in `hsmsss`).
- Confirm `vet` (`go vet ./...`), `test` (`go test ./... -short -race`), and `test-all` are green
  once the non-compiling dirs are gone.

## 9. Acceptance criteria

- `go build ./...` — clean (no excluded dirs).
- `go vet ./...` — clean.
- `make test` (`./... -short -race`) and `make test-all` — green.
- `make lint` (pinned golangci-lint) — **0 issues** across `./...`.
- `WithDialer` present + documented (consumer-clean godoc) on `hsmsss` and `secs1`, with a unit test
  proving a custom dialer is honored and `nil` is rejected.
- Component clock hooks: every hook added has ≥1 deterministic in-package test consuming it; production
  default path unchanged (a teeth-check that removing the hook's test-set leaves behavior identical).
- `integration/` cross-transport parity table passes for BOTH `hsmsss` and `secs1`.
- FSM `(prev,next)` matrix complete and green.
- No real network in the `integration/` suite except the explicitly-named loopback-pair scenarios.
- `tests/`, `examples/`, `internal/bakeoff/` gone; Makefile updated.

## 10. Task decomposition (subagent-driven, mirroring SP5a/SP5b)

- **T0 — Green foundation.** Delete `tests/`, `examples/`, `internal/bakeoff/`; create a MINIMAL
  `integration/` package (a `doc.go` stub — enough to compile and satisfy `go test ./integration/...`)
  so the Makefile rewiring is valid immediately (RESOLVES review P2); update the Makefile (`STRESS_DIRS`
  drop `./tests/...` add `./integration/...`; repoint/drop `stress-quick`; confirm `FUZZ_PKGS`); confirm
  `go build ./...` + `go vet ./...` + `make test` green. (Do FIRST: every later task builds on a green
  `./...`.)
- **T1 — `WithDialer` seam.** `DialFunc` type, config field + default, `WithDialer` option, wire the
  two dial sites, consumer-clean godoc; unit tests (custom dialer honored; nil rejected; default
  preserved).
- **T2 — Component clock hooks.** Add the MINIMAL set of unexported `now func() time.Time` hooks at the
  deadline-COMPARISON sites ONLY — the SECS-I line T1/T2 and inter-block T4 (`secs1/line.go`,
  `secs1/assembler.go`) and the T8 recv-idle `SetReadDeadline` site — with in-package deterministic
  tests; teeth-check default-unchanged. Do NOT hook the fired T3/T5/T6/T7 timers (§3b); their
  determinism comes from short configured durations (`WithT3`/`WithT5`/`WithT6`/`WithT7`) + scripted-peer
  ordering.
- **T3 — `integration/` scaffold + `net.Pipe` harness + `hsmsPeer`.** Package skeleton, pipe/loopback
  harness, the scripted HSMS peer, and the first HSMS parity scenario end-to-end.
- **T4 — `secs1Peer`.** The scripted SECS-I block-protocol peer + the first SECS-I parity scenario.
- **T5 — Cross-transport parity table.** All §5 scenarios over both transports via the factory.
- **T6 — FSM `(prev,next)` matrix.** §6.
- **T7 — Fuzz consolidation.** §7.
- **T8 — Final gate + docs.** Full `./...` gate, Makefile/CI confirmation, package `doc.go` for
  `integration/`, ledger.

Concurrency-critical tasks (T1 dial-seam threading, T2 clock hooks in the core) get a Codex review;
mechanical/test tasks get a standard task review. Every task: fresh implementer → controller
independent gate (build/vet/`-race`/lint) → review → fix loop → ledger.

## 11. Open items for the PLAN (not spec blockers)

- **O6-1 — Exact clock-hook set.** Which timer owners genuinely need a hook vs. can be covered by
  scripted-peer ordering + generous bounds. Minimize the core surface touched.
- **O6-2 — Scripted-peer reuse.** How much of the existing per-package raw-peer helpers can be lifted
  into `integration/` vs. written fresh (they currently live in `_test.go`, not importable across
  packages — likely a small copy/adapt, not a shared export).
- **O6-3 — `net.Pipe` deadline semantics — CONFIRMED.** An isolated spike proved `net.Pipe` survives the
  exact patterns the transports use: read-deadline→timeout→reset→read-succeeds (the secs1 idle-poll
  cycle), write-deadline timeout on the unbuffered pipe, concurrent bidirectional I/O, and peer-`Close`→
  EOF (the involuntary-drop signal). No deadline-capable wrapper is needed. (Not a plan blocker.)
- **O6-4 — `integration/` package name & layout.** Single package vs. sub-packages
  (`integration/parity`, `integration/fsm`); one package is simpler and preferred unless it grows large.
- **O6-5 — stress-quick repoint.** Where the known flake-prone selectors live after `tests/` is gone.
