# Sub-project 7 — API Freeze, gem Expansion & v1→v2 Migration Guide

Status: SPEC. Branch `v2` (long-lived; never merges to `main`). Module `github.com/arloliu/go-secs/v2`.

## 1. Overview

SP7 is the finalize-for-release sub-project. It has three workstreams:

1. **API freeze changes** — apply the pre-v2.0 breaking + non-breaking API fixes surfaced by the
   two independent API-stability reviews (opus per-package fan-out + fable holistic;
   consolidated in `tmp/sp7_api_review_consolidated.md`), so the public surface is coherent
   before it is frozen.
2. **`gem` E30 builder expansion** — grow `gem` from S9-only into a bounded SEMI E30/GEM core
   builder set, and relocate the transport-agnostic base message builder into `secs2`.
3. **v1→v2 migration guide** — a prose migration document under `docs/`, written last against the
   final (post-fix) surface.

**Versioning and tagging are OUT OF SCOPE** (prep only). Because SP7 does not cut the tag, the
freeze is not literally locked at SP7 end — any freeze item not decided here can still be revisited
before the eventual `v2.0.0` tag.

**Sequencing.** W1 (API changes) → W2 (gem) → W3 (migration guide). W2's gem changes depend on the
W1 gem-related changes (generic-message move, S9 E5-faithful), so the gem-touching work is
co-scheduled. The migration guide is written last so it documents the final surface.

## 2. Global constraints (every task inherits)

- Go floor `go 1.26.0`; module path `github.com/arloliu/go-secs/v2`.
- Public packages only change as specified; `internal/*` types never appear in exported signatures.
- Godoc and commit messages carry **no internal codes** (SP7/D7/§/P0-P1/"sub-project"); design docs
  under `docs/v2/` and `.superpowers/` may keep them.
- Authoritative lint = the pinned `go tool -modfile=.linter.go.mod golangci-lint run` → **0 issues**.
- Every change is TDD: failing test → minimal impl → green → gofmt/vet/`-race`/pinned-lint gate.
- No `Co-Authored-By` / attribution trailers on commits.

## 3. Locked decisions

- **D7-1** — gem expands to the bounded E30 core set (§5); the nested define/link/host-command
  families are deferred (no consumer demand; the eqp-hub proposal is stream/function-agnostic).
- **D7-2** — migration deliverable = a prose guide doc; no automated codemod tool.
- **D7-3** — a formal public-API stability review was run (two independent models) and consolidated;
  §4 is its adjudicated output.
- **D7-4** — versioning/tagging deferred; SP7 is release-prep only.
- **D7-5** — the user is open to pre-freeze breaking changes; all of §4 is approved.
- **D7-6** — identity term unified to **`SessionID()`** across `hsms`/`hsmsss`/`secs1`
  (rename `SECS2Endpoint.ID()`); `secs1` reports the real wire **deviceID** through it.
- **D7-7** — `gem.S9Fx` builders are made **E5-faithful** (carry the offending header).
- **D7-8** — the transport-agnostic base message builder (`gem.NewMessage`/`gem.Message`) moves to
  **`secs2`**; `gem` keeps only E30-role builders.
- **D7-9** — `secs2.DecodeOwned` is **kept as a documented sealed capability** (Option 1, §4.4):
  a naive unexport breaks the build (`hsms` is a legitimate cross-package caller), so it stays
  exported with godoc marking it internal-transport-only, and the CI-gate exception is retained.
- **D7-10** — `secs1.WithIsEquip(bool)` → `WithEquipment()`/`WithHost()`; `logger.GetLogger()` →
  `logger.Default()`.

## 4. Workstream 1 — API freeze changes

### 4.1 Breaking changes (approved)

| id | pkg | before → after | test |
|----|-----|----------------|------|
| C1 | hsms | `SelectStatusActived` (=1) → `SelectStatusAlreadyActive`; `SelectStatusEntitActived` (=6) → `SelectStatusEntityAlreadyActive` (parallel with the siblings `SelectStatusAlreadyUsed`, `SelectStatusEntityAlreadyUsed`) | const values unchanged; grep no old name |
| C2 | secs2 | `type FormatCode = int` → `type FormatCode uint8` (defined type) | assignment/switch sites compile; format-code round-trips |
| C3 | logger | `type LogLevel = int8` → `type LogLevel int8` (defined type) | Logger impls + SetLevel compile; levels compare |
| C4 | hsms | remove `Error()` from the `Message` interface and both concrete types; delete dead `ControlMessage.err` | interface no longer declares Error(); reject→`*RejectError`, decode→`DecodeErr` still work |
| C5 | hsms | reject-reason getters `GetRejectReasonCode()`/`RejectReasonCode()` return `byte` (match `RejectError.Reason`) | getter type is byte; value parity with RejectError.Reason |
| C6 | secs2 | shortcut `L/A/J/W/B/BOOLEAN` change from `var = NewXxx` to funcs (match `I1..F8`) | call sites unaffected; `&secs2.A` no longer compiles (intended) |
| C7 | logger | move `MockLogger`/`NewMockLogger` out of the package build into a `logger/loggertest` subpackage; drop the testify dependency from the `logger` import graph | `go list -deps logger` excludes testify; loggertest still usable in tests |
| C8 | hsms | `NewDataMessage`/`Derive().Build()` treat nil `item` as `secs2.NewEmptyItem()` (no panic); fix `README.md:415` | nil item → empty-body message, no panic; README example compiles & runs |
| C9 | hsms, hsmsss, secs1 | rename `SECS2Endpoint.ID()` → `SessionID()`; update `hsms.Connection`/session/endpoint; `secs1` reports the wire **deviceID** through `SessionID()` — **(a)** `secs1.New` seeds the core session id from `cfg.DeviceID()`; **(b)** `secs1/message.go:assembleFrame` (which synthesizes the inbound HSMS frame; today it hard-codes `buf[0],buf[1] = 0xFF,0xFF` at line ~132) stamps the session-id field with the **deviceID carried in the received SECS-I blocks** (available from `assembleBlocks`' returned `messageHeader.deviceID`, i.e. the on-wire value), so inbound `msg.SessionID()`==deviceID; the outbound path `secs1/adapter.go:splitFrame` already uses `cfg.DeviceID()`; `hsms.WithSessionID` documented as overridden for SECS-I | SessionID() present, ID() gone; secs1 SessionID()==deviceID; BOTH outbound and inbound `msg.SessionID()` carry the wire deviceID |
| C10 | secs2 | **keep** `secs2.DecodeOwned` + the `framecodec.OwnedSECS2Body` capability token (no signature change); expand its godoc to state it is an internal-transport-only entry point — external callers cannot construct the token, so `Decode` is the public path. Keep the whitelisted CI-gate exception. (See §4.4; a naive unexport breaks the build.) | godoc documents the sealed capability; `hsms/data_msg.go:315` recv path unchanged; `-race` inbound still green |
| C11 | gem, secs2 | move `gem.NewMessage`/`gem.Message` → `secs2.NewMessage`/`secs2.Message` (implements `secs2.SECS2Message`); param names `(stream, function byte, replyExpected bool, item Item)`; `gem` builders return `secs2.SECS2Message` and construct via the secs2 base | `secs2.NewMessage` builds a valid SECS2Message; gem no longer exports a generic Message |
| C12 | secs1 | `WithIsEquip(bool)` → no-arg `WithEquipment()`/`WithHost()` pair (mirror `WithActive()`/`WithPassive()`) | both options set role correctly; old option gone |
| C13 | logger | `GetLogger()` → `Default()` (avoid the `Get-` prefix; `Logger()` would collide with the type) | Default() returns the process logger; old name gone |

Note (C1 naming): match the existing sibling constant spelling in `control_msg.go`; the consolidated
report notes `SelectStatusEntityAlreadyUsed` as the alignment anchor.

### 4.2 Non-breaking fixes (apply; no decision)

Godoc / doc-drift / additive — land alongside W1:

- hsms: wire the 5 decode sentinels into `DecodeHSMSMessage` via `%w` (so `errors.Is` works);
  prefix all sentinel error strings with `hsms:`; document `Linktest*` counters + `ConnState` as
  HSMS-SS-only (zero/no-op for SECS-I); drop the nil-receiver guard on `ControlMessage.Error()`'s
  successor (moot after C4) / fix related godoc; document `req/rejected` must be non-nil on the
  Rsp/Reject factories; document the partial `ConnectionConfig` accessor set; ConnState/OpenMode
  constants get identifier-leading doc comments.
- secs2: rewrite `SECS2Message` godoc to the 4 declared methods (drop SessionID/SystemBytes/Header/
  ToBytes claims); fix the fake-generic `IntItem[int8]` notation in item godoc; document
  `AppendBinaryTo`/iterator return-unchanged/yield-nothing on concrete-type mismatch.
- hsmsss/secs1: fix the `Config` "read via the same field names" godoc → promoted accessors; secs1
  `WithConnectionOption` godoc enumerates applicable core knobs + flags inert ones; document (or
  nil-guard) that a hand-built `Config` must originate from `NewConfig` (dial nil-guard both siblings).
- sml: README error-model reword (only syntax = `*ParseError`); empty-input sentinel gets `sml:`
  prefix; `WithASCIIQuote` localized-quote note; `ParseError.Msg` doc comment.
- logger: `NewSlog` godoc (stdout, dev console handler, `addSource` ignored); `SetLevel`
  child-shares-parent `LevelVar` note.
- secs1: optional non-breaking hardening — an engine/wrapper guard for the runtime
  `WithWriteTimeout` trap instead of only documenting it.

### 4.3 Deferred API items (NOT in SP7)

`DialFunc` unification; hsmsss/secs1 protocol-timer convenience wrappers; `hsms.NewConnection`/
`TransportRuntime` sealed exports (by-design, keep the note); `ConnRetryCount` gauge naming;
`secs2.Item` interface breadth; `logger.Fatal`→`os.Exit`. Sealing `secs2.Item` (unexported marker)
was considered and left open — revisit before the tag if desired.

### 4.4 C10 — `secs2.DecodeOwned` sealed-capability resolution (RESOLVED: Option 1)

Codex spec-review P0: a naive unexport of `secs2.DecodeOwned` breaks the build. `hsms/data_msg.go:315`
is a legitimate cross-package caller (`secs2.DecodeOwned(framecodec.AdoptSECS2Body(raw))`), and the
`framecodec.OwnedSECS2Body` token is the SP5a-era compile-enforced capability that lets ONLY in-repo
callers use the no-copy decode path (external callers cannot construct the token because `framecodec`
is `internal/`). A public package cannot expose a repo-only function any other way. The options:

- **Option 1 (recommended) — keep the sealed export, document it.** Retain `secs2.DecodeOwned` +
  the capability token; expand its godoc to state it is an internal-transport-only entry point that
  external callers cannot invoke (`Decode` is the public path), and keep the whitelisted CI-gate
  exception. Minimum change; preserves the compile-enforced safety. Cosmetic-only cost: it renders as
  an uncallable symbol on pkg.go.dev.
- **Option 2 — downgrade to a documented ownership contract.** Change to `secs2.DecodeOwned([]byte)`
  (no internal type in the signature) with godoc "caller donates the slice; must not mutate/retain it."
  Removes the internal type from the public API but replaces the compile-seal with a doc-contract, and
  the no-copy path becomes callable by external code.

**Resolved: Option 1** (keep the sealed export, document it). C1–C13 are now all locked.

## 5. Workstream 2 — gem E30 builder expansion

**Model.** Every gem builder is a **pure constructor** (no I/O, no state, no globals) returning
`secs2.SECS2Message`, consumed via `hsms.SECS2Endpoint.SendSECS2Message`. Equipment-defined IDs
(CEID/RPTID/ALID/DATAID/SVID) pass as `secs2.Item` so the caller owns the A/I/U format; gem stays
function-only with no new exported struct types (one composition helper, `Report`). gem imports only
`secs2` (a `[10]byte` header param avoids any `hsms` dependency).

### 5.1 New builders (bounded core set)

| S/F | signature | body (E5) |
|-----|-----------|-----------|
| S1F1 | `S1F1() SECS2Message` (W=1) | header-only (empty item) |
| S1F2 | `S1F2(mdln, softrev string) SECS2Message` (W=0) | `L[2]{ A[mdln] A[softrev] }` (equipment On Line Data) |
| S1F2Host | `S1F2Host() SECS2Message` (W=0) | `L[0]` (host reply to S1F1 — E5 host form) |
| S1F13 | `S1F13(mdln, softrev string) SECS2Message` (W=1) | `L[2]{ A[mdln] A[softrev] }` (equipment Establish Comms Req) |
| S1F13Host | `S1F13Host() SECS2Message` (W=1) | `L[0]` (host Establish Comms Req — E5 host form) |
| S1F14 | `S1F14(commack byte, mdln, softrev string) SECS2Message` (W=0) | `L[2]{ B[commack] L[2]{ A[mdln] A[softrev] } }` (equipment ack) |
| S1F14Host | `S1F14Host(commack byte) SECS2Message` (W=0) | `L[2]{ B[commack] L[0] }` (host ack — E5 host form) |
| S2F17 | `S2F17() SECS2Message` (W=1) | header-only |
| S2F18 | `S2F18(t string) SECS2Message` (W=0) | `A[t]` (12/16-byte TIME) |
| S2F31 | `S2F31(t string) SECS2Message` (W=1) | `A[t]` |
| S2F32 | `S2F32(tiack byte) SECS2Message` (W=0) | `B[tiack]` |
| S5F1 | `S5F1(alcd byte, alid secs2.Item, altx string) SECS2Message` (W=1) | `L[3]{ B[alcd] <alid> A[altx] }` |
| S5F2 | `S5F2(ackc5 byte) SECS2Message` (W=0) | `B[ackc5]` |
| S2F37 | `S2F37(enable bool, ceids ...secs2.Item) SECS2Message` (W=1) | `L[2]{ BOOLEAN[enable] L[n]{ <ceid>… } }` (0 ceids = all) |
| S2F38 | `S2F38(erack byte) SECS2Message` (W=0) | `B[erack]` |
| S6F11 | `S6F11(dataid, ceid secs2.Item, reports ...secs2.Item) SECS2Message` (W=1) | `L[3]{ <dataid> <ceid> L[a]{ <report>… } }` |
| S6F12 | `S6F12(ackc6 byte) SECS2Message` (W=0) | `B[ackc6]` |
| helper | `Report(rptid secs2.Item, values ...secs2.Item) secs2.Item` | `L[2]{ <rptid> L[b]{ <v>… } }` (one S6F11 report element) |

Builders are **dumb** (no TIME-length / COMMACK-range validation); godoc states expected formats.

**W-bit policy.** Replies (even functions) are fixed `W=0` — E5-mandated. Primaries carry the
**common-case** W the GEM workflow expects, hardcoded and stated in each builder's godoc: liveness/
handshake/clock/enable/report-request primaries (S1F1, S1F13, S2F17, S2F31, S2F37, S6F11) use `W=1`.
For **S5F1** E5 leaves the reply optional; the builder emits `W=1` (the GEM norm — the S5F2 ack is
usually wanted) and its godoc says so and points to `secs2.NewMessage` for a no-reply alarm report.
Any non-standard W is reached via `secs2.NewMessage(stream, function, replyExpected, item)` — the gem
builders are convenience wrappers over that base, not the only path.

### 5.2 S9Fx made E5-faithful (D7-7)

The existing S9 factories change to carry the offending header per E5 §10.13 (breaking; v2 is
pre-release):

- `S9F1/F3/F5/F7/F11(mhead [10]byte) SECS2Message` — body `B[10]{mhead}` (MHEAD).
- `S9F9(shead [10]byte) SECS2Message` — body `B[10]{shead}` (SHEAD).
- `S9F13(mexp string, edid secs2.Item) SECS2Message` — body `L[2]{ A[mexp] <edid> }`.

Callers pass the offending message's header via `msg.HeaderBytes()` ([10]byte). All remain W=0.
Note: this is a signature change to the current no-arg S9Fx — it is a v2-internal break (gem was
never released), captured in the migration guide's gem section as additive-since-v1-had-only-S9-empty.

### 5.3 gem package framing

`gem/doc.go` states: pure value builders, E30 role messages, payloads as `secs2.Item`, consumed via
`Connection.SendSECS2Message`, base generic builder now lives in `secs2.NewMessage`. Full godoc on
every new symbol. The deferred families (S1F3/4, S1F11/12, S2F33-36, S2F41/42, S5F3-6, S6F15/16) are
listed in doc.go as intentionally out-of-scope pending demand.

## 6. Workstream 3 — v1→v2 migration guide

File: `docs/migration-v1-to-v2.md`. Written last, against the post-W1/W2 surface. Sections:

1. Overview (why v2: immutable model, connection-centric, no pools) + what breaks at a glance.
2. Import path `/v2` bump.
3. Immutability & delete-only removals: `Free`/`Clone`/`CloneCodec`/`SnapshotForRelay`/`UsePool`/
   `SetValues`/`Set*` → `With*`/`Derive().Build()`; a grep-target deletion checklist with a
   "why safe to delete" line each.
4. Context-first API: ctx-first send calls; `Open(waitOpened bool)` → `Open(ctx, OpenMode)`.
5. Connection-centric model: no `Session`/`BaseSession`/`AddSession`; message via the embedded
   `SECS2Endpoint`; handler signatures `DataMessageHandler(msg, ep)` / `StateChangeHandler(prev,next)`.
6. Config construction renames (`NewConnectionConfig`→`NewConfig`, `NewConnection`→`New`, timer
   `Timeout`-suffix drop, `SessionID` unification, role options).
7. Message/accessor renames (`HSMSMessage`→`Message`, header/systembytes value types, `Stream`/
   `Function`, `Item()(Item,error)`, `Type() MsgType`, removed `Error()`).
8. SML renames (`HSMSParser`→`Parser`, `ParseHSMS`→`Parse`, `ParseHSMSSlow`→`ParseStrict`, etc.).
9. secs2 item changes (`Values()`→indexed/iter accessors; `*WithBytes` removed; `secs2.Decode`;
   the new `secs2.NewMessage` base builder; shortcut ctors now funcs; `FormatCode` defined type).
10. gem changes (import-path-only for the S9 concept; S9 now E5-faithful; new E30 builders;
    generic message moved to secs2).
11. Metrics (`GetMetrics()`→`Metrics()`; HSMS-vs-SECS-I validity caveat).
12. Removed subsystems reference (ConnStateMgr, TaskManager, OpState, id_gen, pool, MockLogger→loggertest).
13. **Symbol-by-symbol API-diff table** — one sub-table per package (hsms/hsmsss/secs1/secs2/sml/gem/
    logger); columns `v1 symbol | v2 symbol | kind (removed/renamed/changed/new) | migration action`;
    every "removed" row cites a replacement or "no equivalent".

## 7. Success criteria

- Every §4.1 change applied with a teeth-checked test; no old symbol remains (grep-clean).
- All §4.2 non-breaking fixes landed.
- gem builds the §5 set + `Report` + E5-faithful S9; each has a golden-body test decoded via
  `secs2.Decode` and, for the round-trip cases, sent through a real `Connection.SendSECS2Message`.
- `secs2.NewMessage` is the generic base builder; `gem` no longer exports a generic message type.
- Migration guide compiles every fenced Go example against the module; the API-diff table covers all
  removed/renamed/changed/new public symbols.
- Full gate green: `go build ./...`, gofmt, `go vet ./...`, pinned golangci-lint 0 issues,
  `go test -race ./...` all packages, integration `-race`, fuzz seeds.

## 8. Out of scope

Versioning/tagging (D7-4); the deferred gem families (D7-1); the §4.3 deferred API items; any
automated codemod tool (D7-2); connection/transport behavioral changes beyond the identity-term and
nil-item fixes.
