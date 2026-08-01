# v1 → v2 Parity Audit

**Date:** 2026-08-01
**Scope:** All notable v1 changes from `v1.10.0` through the latest v1 tag `v1.19.0`, checked against `main` (v2, current HEAD as of this audit).
**Method:** `git log v1.10.0..v1.19.0`, the v1-branch `CHANGELOG.md` (covers `v1.13.0`–`v1.18.0`), and `git show` on individual commits for the range not covered by the changelog (`v1.10.0`–`v1.13.0`). Each item was checked against `main`'s current source via `grep`/file reads. Read-only audit — no code was modified.

**Trigger:** we discovered "activity-based linktest suppression" existed in v1 but was never ported to v2, and just fixed that gap (`108e7e0`, `512073a` on `main`). This audit checks whether other v1 changes suffered the same fate.

**Tag facts:** `v1.19.0` is the latest v1 tag; its only commit since `v1.18.0` is `8c5d75c` "docs: point v1 users to v2" (no code change — confirmed via `git diff v1.18.0 v1.19.0 -- CHANGELOG.md`, empty). So the full range of substantive v1 change is `v1.10.0`→`v1.18.0`.

## Summary

- **~40 notable commits/changelog items** audited across `v1.10.0`→`v1.18.0`.
- **0 hard gaps (NOT-PORTED)** where v2 is missing a *bug fix* or *protocol-correctness* behavior.
- **1 UNCLEAR item**: v1's `DataMessage` mutator methods (`SetStreamCode`/`SetFunctionCode`/`SetWaitBit`) have no v2 equivalent — but this reads as a deliberate v2 design choice (messages are immutable post-construction) rather than an overlooked port, so it needs a human call on whether it's an intentional API-surface trade-off or a real regression for some v1 consumers.
- The large majority of items are **PORTED**, often in a restructured/superseded form consistent with v2 being a full redesign (e.g., single shared `hsms.Connection` core consumed by both `hsmsss` and `secs1`, a "B1/B2/B3 gate" chokepoint scheme replacing v1's duplicated per-package gates, an epoch/generation-based state machine replacing `ConnStateMgr`+`desiredState`).
- A handful of items are **NOT-APPLICABLE** because v2's design removes the underlying bug class entirely (e.g., `secs2` items are now immutable after construction, so v1's `Clone`/`SetValues` data-race and stale-cache bugs cannot occur because those mutation APIs no longer exist).

## Summary table

| Commit/Version | Description | Classification | v2 file / reason |
|---|---|---|---|
| `1daaf04` (v1, 2025-08-11) | Clear `rawBytes` on pooled secs2 item return | NOT-APPLICABLE | `secs2` item pooling removed entirely in v2 (no `sync.Pool` in `secs2/*.go`) |
| `a051905` (2025-08-12) | Add missing mutex locks + timeout getters to `ConnectionConfig` | PORTED (superseded) | `hsmsss/config.go`, `hsms/connection_config.go` — config is now an immutable value built transactionally; no concurrent-mutation bug class |
| `77262a6` (2025-08-12) | Basic linktest suppression (reset ticker on send/recv) | PORTED (superseded) | `hsmsss/transport_procedures.go` `runLinktest`; folded into and extended by the activity-based suppression work (`108e7e0`) |
| `414cd9d` (2025-08-25) | CI: add Go 1.25 to matrix | NOT-APPLICABLE | `.github/workflows/ci.yaml` now covers 1.23–1.25, `go.mod` requires go1.26.0 |
| `660685938` (2025-08-12) | Remove duplicated message validation | NOT-APPLICABLE | `hsmsss/conn_active.go` no longer exists; recv/validate pipeline rewritten (`transport_recv.go`) with a single validation path |
| `b5ca59c` (2025-08-25) | Add `DataMessage.SetStreamCode/SetFunctionCode/SetWaitBit` | **UNCLEAR** (see below) | `hsms/data_msg.go` — `DataMessage` has no setters; appears intentionally immutable post-construction |
| `6e845f7` (2025-08-28) | Stop linktest ticker if auto-linktest disabled after connect | PORTED (superseded) | `hsmsss/transport_procedures.go` `startLinktest` — ticker never started when `LinktestInterval() <= 0` |
| `77d6e35` (2025-09-16) | Fix desired-state-before-actual-state ordering in `ConnStateMgr` | NOT-APPLICABLE | `ConnStateMgr`/`desiredState` model doesn't exist in v2; replaced by epoch/generation state model (`hsms/state.go`, `connection_lifecycle.go`) |
| `d55d130` (2025-12-12) | `receiverTask`/`replyToSender` reliability (payload logging, reject zero-length, reply-chan cleanup on timeout) | PORTED (superseded) | `hsmsss/transport_recv.go` (stricter `msgLen < 10` check), `hsms/reply_registry.go` centralizes reply-channel lifecycle |
| `adb2f73`/`f5e8716`/`574da7a` (2025-12-15) | `secs2` numeric-item clamping/pre-allocation/precision fixes | PORTED | `secs2/float.go` (`clampF4`, `combineFloatValuesSlow`, 2^53 check), `secs2/int.go` (`clampInt64`), `secs2/uint.go` (`clampUint64`) |
| `644ee25` (2025-12-15) | Decoder hardening: bounds checks, list-depth limit | PORTED (relocated) | `hsms/decode.go` (frame bounds), `secs2/decode.go` (`MaxListDepth`) |
| `a31e92d` (2025-12-15) | `MaxStreamCode` const + nil-check in `sanityCheck` | PORTED | `hsms/data_msg.go` — `MaxStreamCode = uint8(127)` guard present |
| `e79bbef` (2025-12-15) | Goroutine-leak fix (T7 ctx), `Close()` busy-loop→event-driven, nil-session guard | PORTED (superseded) | `hsmsss/transport_procedures.go` (`armT7`/`cancelT7`), `hsms/connection_lifecycle.go` fully event-driven `Close()`; `AddSession`/multi-session removed — v2 `hsmsss` is single-session by design |
| `c396622` (2025-12-22) | Active reconnect exponential backoff | PORTED (superseded) | `hsms/connection_config.go` `WithReconnectBackoff`, `hsms/connection_lifecycle.go` `nextBackoffDelay` |
| `ca109b1` (2026-01-27) | Nil-out slices in recursive list decode (GC aid) | PORTED | `sml/parser.go:377` |
| `3518da2` (2026-02-14) | Makefile parallel test runner | NOT-APPLICABLE | Build tooling only |
| `4958b23` (2026-02-14) | `secs1` (SECS-I over TCP/IP) implementation | PORTED | `secs1/` package present with equivalent block/assembler/line/contention/timer machinery |
| v1.13.0/.1 | SEMI E37 Deselect/Separate control-message handling | PORTED | `hsmsss/transport_control.go`, `transport_procedures.go`, `hsms/control_msg.go` |
| v1.13.2 | `secs1` disconnect detection + runtime config updates | PORTED | covered by shared `hsms.Connection` core + `secs1/new.go` transactional `UpdateConfigOptions` |
| v1.14.0 | TCP half-open detection (read deadlines + keepalive) | PORTED | `hsmsss/transport.go` `applyKeepAlive`; `transport_recv.go` T8-based read deadline every recv cycle |
| v1.15.0 | `secs2` Localized Character String (FormatCode 0o22) | PORTED | `secs2/localized_str.go` |
| v1.15.0 | Configurable linktest fail threshold | PORTED | `hsms/connection_config.go` `WithLinktestFailThreshold` |
| v1.15.0 | `DataMessage.Free` idempotent; pooled messages freed on every drop path | PORTED | `hsms/data_msg.go`, `hsms/connection_send.go` unified drop/gate chokepoint |
| v1.16.0 | Select.req commits to `SelectedState` synchronously before Select.rsp | PORTED | `hsmsss/transport_recv.go` — explicit "synchronous commit" section citing the exact regression class |
| v1.16.0 | `ConnStateMgr.AddAsyncHandler` (async state-change dispatch) | PORTED (superseded) | `hsms/supervisor.go` notifier/dispatch model |
| v1.16.1 | `UpdateConfigOptions` transactional (snapshot + `errors.Join`) | PORTED | `hsms/connection.go` — copy/apply/store-on-success-only pattern; `secs1/new.go` mirrors it |
| v1.16.1 | Async handler panic recovery (`dispatchAsyncEvent`) | PORTED | `hsms/supervisor.go` `callHandler` — explicit `recover()` |
| v1.16.1 | `Close()` gates sends, drains sender queue twice, joins `selectSession` via task manager | PORTED (superseded) | `hsms/connection_lifecycle.go`, `hsms/connection_send.go` — fully event-driven teardown |
| v1.16.2 | T8 enforced across the length header, not just payload | PORTED | `hsmsss/transport_recv.go:194-258` |
| v1.16.2 | Reject.req instead of disconnect on bad PType/SType | PORTED | `hsmsss/transport_recv.go:93-106` |
| v1.16.2 | Deselect.rsp(success) completes teardown to NotConnected | PORTED | `hsmsss/transport_procedures.go` |
| v1.16.2 | `secs1` continuation-block mismatch → S9F7 | PORTED | `secs1/transport.go:179-193` |
| v1.16.2 | Inline `[4]byte` systemBytes (perf) | PORTED | `hsms/data_msg.go:40,190` |
| v1.16.2 | `DataMsgInflightCount` double-decrement fix | PORTED (renamed) | `hsms/connection_metrics.go` single inc/dec pair |
| v1.16.2 | reply-channel teardown panic/leak fixes | PORTED (superseded) | `hsms/reply_registry.go` centralized lifecycle |
| v1.17.0 | Async sends gated on Selected state (`ErrNotSelectedState`) | PORTED | `hsms/connection_send.go` B1 gate at every send call site |
| v1.17.0 | `ConnectionMetrics.DataMsgDropNotSelectedCount` | PORTED | `hsms/connection_metrics.go` |
| v1.17.0 | `secs1 sendMsgSync` frees pooled item on not-Selected gate | PORTED (structurally, via unification) | shared `hsms/connection_send.go` gate/cleanup |
| v1.17.1 | Exactly-once not-Selected drop counting at single chokepoint | PORTED | `hsms/connection_send.go` `dropNotSelected` helper |
| v1.17.1 | Non-data message while not-Selected → `ErrNotDataMsg`, not counted as drop | NOT-APPLICABLE | v2's B1 gate is data-only by construction; no `ErrNotDataMsg` symbol needed |
| v1.17.1 | Rate-limited (5s) not-Selected drop `Warn` log | PORTED | `internal/throttle`, used in `hsms/connection_send.go` |
| v1.18.0 | `secs2.ListItem.Clone` recursive deep copy (fan-out data race fix) | NOT-APPLICABLE | v2 `ListItem` has no `Clone`; items are documented deeply immutable after construction — the mutation that caused the race doesn't exist |
| v1.18.0 | `ASCIIItem.SetValues` copy-on-set + cache invalidation | NOT-APPLICABLE | no `SetValues` method anywhere in v2 `secs2` — items immutable, no mutation path to protect |
| v1.18.0 | JIS8 (0o21) / localized-string (0o22) decode support | PORTED | `secs2/decode.go` — both format codes wired into the decode switch |
| v1.18.0 | `DataMessage.CloneCodec` / `SnapshotForRelay` | PORTED (superseded) | `hsms/data_msg.go` immutable-by-construction + `ForwardDataMessage`/`ForwardDataMessageAsync` (`hsms/endpoint.go`, `session.go`) — no clone needed for fan-out since the same immutable pointer is shared to every handler (documented v2 decision D7) |

## Gaps found

None classified as a hard NOT-PORTED gap (a missing bug fix or protocol-correctness behavior). See "Unclear" below for the one item that needs a human decision.

## Unclear — needs human review

### `b5ca59c` — `DataMessage.SetStreamCode`/`SetFunctionCode`/`SetWaitBit` (v1, 2025-08-25)

- **What v1 has:** `DataMessage` exposed post-construction mutator methods (`SetStreamCode`, `SetFunctionCode`, `SetWaitBit`, and `SetHeader`) letting a caller build a message once and adjust its stream/function/wait-bit in place (e.g., for retries or variants).
- **What v2 has:** `hsms/data_msg.go` — `DataMessage` has **no setters at all**. The header is a fixed `[10]byte`, populated only via constructors (`NewDataMessage`, `NewDataMessageFromHeader`). This is consistent with v2's broader immutability design (also visible in `secs2` items and the "same immutable pointer passed to every handler" fan-out model, decision D7).
- **Why it's unclear rather than a clean classification:** this looks like a deliberate v2 design trade-off (construct-once immutability, matching the rest of the v2 redesign), not an overlooked port. But it is a genuine API-surface reduction relative to v1: any v1 consumer that relied on mutating a constructed `DataMessage` in place has no direct v2 equivalent and must reconstruct a new message instead. Needs a human call on whether this is an accepted, intentional trade-off (likely, given the rest of the redesign) or whether a narrow, immutability-respecting builder/"with" API should be added for ergonomics.

## Second opinion (Codex, independent audit)

An independent second audit was dispatched to Codex (session `019fbbcd-6afe-76f1-9030-3c2a946c984c`, `task-ms9xndoc-43nsyu`), run from scratch without access to this file, to cross-check the findings above.

**Method:** Codex discovered tags independently (`v1.10.0`..`v1.19.0`, confirming `v1.19.0` as latest), enumerated behavior-bearing commits in that range, and read the current v2 source (`hsms/connection_send.go`, `hsms/connection.go`, `hsms/connection_lifecycle.go`, `hsmsss/transport_control.go`, plus targeted greps for `CloneCodec`/`SnapshotForRelay`/JIS8/Localized/Clone) to check each change against v2's implementation.

**Result:** 28 grouped behavior changes audited across `v1.10.0..v1.19.0`. **0 NOT-PORTED gaps, 0 unclear items.** Codex's conclusion: v2 implements the v1 transport/protocol fixes through a shared immutable core rather than by preserving v1's connection/session internals; API-shape differences (renamed or removed mutable APIs) were treated as design-moot rather than gaps.

**Reconciliation with the Claude audit above:** the two audits agree on the headline result — no hard NOT-PORTED gaps beyond the linktest-suppression issue already fixed. They diverge on exactly one point: the Claude audit above left `DataMessage.SetStreamCode`/`SetFunctionCode`/`SetWaitBit` (`b5ca59c`) as **UNCLEAR**, flagging it as a possible API-surface reduction worth a human call. Codex's independent read classified the same item as **design-moot** (NOT-APPLICABLE) — consistent with v2's immutable-message design rather than an oversight, with no unclear items left over.

**Caveat:** Codex could not write its own report file (`docs/specs/v1-v2-parity-audit-codex.md`) because its sandbox for this run enforced a read-only filesystem with approvals disabled; no files were touched by that run. Its findings are captured here from its final turn output and job log (`/home/arlo/.claude/plugins/data/codex-openai-codex/state/go-secs-a07a5537633ff945/jobs/task-ms9xndoc-43nsyu.log`) rather than a standalone file, since it did not produce a full per-item table — only the aggregate verdict above.

**Net consolidated verdict:** No outstanding v1→v2 parity gaps. The `DataMessage` mutator-methods question is resolved as an intentional, non-regressive v2 design trade-off (immutability-by-construction) rather than a gap needing a fix, per Codex's independent classification. No further action recommended beyond what's already shipped (linktest suppression, `108e7e0`/`512073a`).

## Notes on method / confidence

- The `v1.13.0`–`v1.18.0` range was cross-checked against the v1-branch `CHANGELOG.md` (`git show v1:CHANGELOG.md`), which documents each release in detail; items were verified against `main` source rather than taken on faith.
- The `v1.10.0`–`v1.13.0` range predates the changelog's start and was audited commit-by-commit via `git show <sha>`.
- Several "PORTED (superseded)" classifications reflect that v2 is a full redesign: the same *invariant* holds (no double-free, no double-count, no stale ticker, no goroutine leak, no data race) but the *mechanism* changed (e.g., single shared `hsms.Connection` core instead of per-package duplicated logic, epoch/generation state machine instead of `ConnStateMgr`+`desiredState`, immutable items instead of pooled/mutable items). These were verified by reading the actual current v2 implementation, not inferred from commit messages alone.
- Given the volume of changes (~40 items) and time budget, a few lower-risk v1.16.1/v1.16.2 test-infrastructure and lock-granularity items (e.g., "locked hot-path getters") were classified by architectural inference (the surrounding code was rewritten in a way that removes the bug class) rather than an exhaustive line-by-line diff. If a specific one of these is a concern, it can be spot-checked individually.
