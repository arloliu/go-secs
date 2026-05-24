# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.16.1] - 2026-05-24

Stability release focused on connection-lifecycle correctness in HSMS-SS and
back-porting the same fixes to SECS-I. The dominant themes are: closing the
race window between `Close` and producers still holding `senderMsgChan`,
making `UpdateConfigOptions` transactional, tightening SEMI-E37 conformance
on the wire (Reject.req for unsupported PType/SType, T8 across the length
header, S9F7 for malformed continuation blocks), and stopping a handful of
goroutine / channel / pool leaks that accumulated across reconnect cycles.
Adds direct chaos coverage for the `hsms` dispatcher and a post-close
leak-detection harness; the dispatcher work surfaced a latent panic-
propagation bug that's now recovered.

No public API additions; behavior changes are noted under **Changed**.

### Build

- `Makefile`: `stress-test` skips Fuzz targets and the per-package
  timeout is bumped to 45 m. The skip works around a Go-runtime cgo
  DNS pile-up that hangs `FuzzConnectionLifecycle` at high count;
  fuzz coverage stays in `make fuzz-test`. The timeout bump absorbs
  ~7 m of new chaos-test runtime in `tests/hsmsss_integration`.

### Changed

- `hsmsss`, `secs1`: `Connection.UpdateConfigOptions` is now a single
  transaction — every option is validated against a snapshot first, errors
  are aggregated via `errors.Join`, and live config is only mutated if every
  option succeeds. Previously, the first failure left earlier options
  half-applied. Non-runtime options (mode, role, queue sizes, logger
  identity) are now explicitly rejected with a clear error instead of being
  silently accepted into a half-updated state. Idempotent same-value calls
  for non-runtime options (e.g. `WithActive()` on an already-active
  connection) succeed; real value transitions are still flagged.
- `hsmsss`: `Connection.Close` now returns the first non-nil error from
  teardown (captured via an atomic `closeErr`), so callers can distinguish a
  clean close from one where task teardown exceeded `CloseConnTimeout`.
  Previous behavior returned `nil` from the second-caller path even when the
  owning `closeConn` had timed out.
- `hsms`: async connection-state handlers no longer observe idempotent
  `(prev == next)` events. The forced `ToNotConnected()` cleanup path used
  to deliver `(NotConnected, NotConnected)` to async subscribers; sync
  handlers are unaffected so internal forced-cleanup invariants are
  preserved.

### Fixed

#### hsms — async dispatcher robustness

- `ConnStateMgr.dispatchAsyncEvent` now wraps each handler call in
  `recover()`. Previously, a panicking user-supplied handler
  (registered via `AddAsyncHandler` or `Session.AddConnStateChangeHandler`)
  crashed the dispatcher goroutine and silently stopped all future
  state-event delivery. Panics are now logged with the `(prev, next)`
  pair and the dispatcher continues with sibling handlers / follow-up
  transitions.

#### hsmsss — connection close & teardown

- Gate sends during connection close via an `RWMutex`-guarded `sendClosed`
  flag, so external producers can no longer strand a frame in
  `senderMsgChan` past `closeConn`'s drain. The gate is closed in
  `closeConn` and re-opened in `doOpen` / `renewConnCtx`.
- Drain `senderMsgChan` a second time after `taskMgr.Wait()` returns. A
  send queued between the step-3 drain and `connCtx` cancellation could
  otherwise survive `closeConn` and replay on the next connection with
  stale system bytes and session ID.
- Join `selectSession` via `taskMgr` instead of launching it as a raw
  goroutine. Without this, `closeConn`'s `taskMgr.Wait()` could return
  while `selectSession` still held a producer reference to
  `senderMsgChan`, breaking the post-Wait drain's "every producer has
  exited" invariant.
- Complete teardown on host-initiated `Deselect.rsp(success)`: mark the
  connection as deselected and transition to `NotConnected`. Without this,
  an active-initiated graceful Deselect left the connection in `Selected`
  until the peer eventually closed TCP, at which point a stray
  `Separate.req` was sent on a just-gracefully-torn-down link, violating
  SEMI E37 §7.7. Path is currently unreachable from user code; the fix is
  defensive for any future public API exposing active-initiated Deselect.
- Reporting `Close` teardown errors is now safe under concurrent callers.
  The previous `CompareAndSwap`-from-nil guard let a racing `Close()` read
  `nil` if it arrived before the owning `closeConn` stored the error;
  concurrent callers now spin on `isClosed()` (which requires `opState`
  Closed) and always observe the stored result.
- Clear stranded `replyErrs` entries in `dropAllReplyMsgs`. A transaction
  error stranded by a `connCtx`-done race in `replyErrToSender` could
  outlive the connection and surface on the next send.

#### hsmsss — SEMI-E37 wire conformance

- `messageReader` now enforces T8 across the 4-byte length header, not just
  the payload. Per SEMI E37 §9.2.3.1, T8 must govern the gap between any two
  successive bytes once reception of a message has begun; the per-iteration
  deadline now switches from `idleTimeout` to `t8Timeout` after the first
  header byte arrives.
- T8 payload-read timeout is wrapped with `hsms.ErrT8Timeout` so
  `errors.Is(err, hsms.ErrT8Timeout)` succeeds for payload-phase T8 expiry,
  matching the header-phase behavior.
- Reply `Reject.req` instead of disconnecting on unsupported PType
  (reason 2) and undefined SType (reason 1) per SEMI E37 §7.10.3.
  `messageReader` inspects the 10-byte header before `DecodeMessage` and
  returns a typed `headerRejectError`; `receiverTask` peels it off,
  sends the `Reject.req` via the existing `senderTask` path, and keeps
  the connection alive.

#### hsmsss — pool / channel leaks & accounting

- Close `replyMsgChans` entries on every terminal path of `sendMsg`.
  Receiving `replyMsg == nil` on the peer `Reject.req` path previously
  returned without `removeReplyExpectedMsg`, leaking the transaction
  entry for the connection lifetime.
- Stop double-decrementing `DataMsgInflightCount` on reply.
  `replyToSender` called `decDataMsgInflightCount()` independently of
  `sendMsg`, driving the gauge to -1 after one W-bit round trip.
- Locked hot-path getters (`IsActive`, `IsEquip`, `SendTimeout`,
  `TraceTraffic`, `ValidateDataMessage`) eliminate the data races
  `-race` flagged in `sendMsg` / `sendMsgSync` / `receiverTask` /
  `validateMsg`.

#### secs1 — back-ports of the above

- Gate sends during connection close (`sendClosed` / `sendMu`). Without
  this, a frame queued between the pre-cancel drain and `connCtx`
  cancellation survived into the next connection generation and was
  transmitted as its first frame instead of the expected protocol-init
  exchange. Gate is reset under `sendMu.Lock()` outside `createContext()`
  to avoid `ctxMutex`/`sendMu` lock-order inversion.
- Join the active reconnect loop via a `connectLoopWg` `WaitGroup` before
  installing fresh contexts in `Open`. The pre-existing atomic-CAS guard
  could race with a rapid Close→Open cycle when the old goroutine's
  deferred `Store(false)` had not yet run, causing the replacement
  retry loop to be skipped and the reconnect to stall.
- Transition to `NotConnected` *before* stopping the state manager on
  `Close`. SECS-I had inherited the wrong ordering from an earlier
  HSMS-SS revision, so the final state change fired after the async
  handler dispatcher had already exited via `stateMgr.Stop()` and was
  never observed by user-registered `ConnStateChangeHandler`s.
- Send `S9F7` (Illegal Data) when `ErrHeaderMismatch` is raised by the
  multi-block assembler. A host sending a malformed continuation block
  (mismatched W-bit, stream, or function) previously never learned the
  message was rejected, and any outstanding W-bit transaction hung until
  T3 expiry.

### Tests

- `hsmsss`: regression tests for the close/send race
  (`TestConnection_CloseDrainsStrandedSenderFrame`), the gate
  (`TestConnection_CloseGateRefusesSendsAfterClose`), the Close vs in-flight
  `selectSessionTask` race (`TestHSMS_SelectCloseRace_NoPanicNoZombie`,
  ×100), the partial-header T8 deadline
  (`TestMessageReader_PartialHeader_T8Timeout`), the peer-Reject reply-chan
  leak (`TestConnection_SendMsg_PeerReject_NoReplyChanLeak`), the W-bit
  in-flight balance (`TestConnection_DataMsgInflightCountBalanced`), and a
  property test for idempotent-async transition suppression.
- `hsmsss` integration: P0 round-trip coverage for `Reject.req` reason
  codes 3 (TransactionNotOpen) and 4 (NotSelected) — including
  `SendMessage` surfacing `ErrRejectNotSelected` — plus decode-layer
  coverage for codes 1 (STypeNotSupported) and 2 (PTypeNotSupported), and
  a mid-sequence-recovery test for the auto-linktest fail-threshold.
- `secs1`: a 30-cycle wire-observable regression
  (`TestSECS1_CloseReopen_NoStaleBlockAcrossGeneration`) guards the
  send-gate fix by asserting the gen-2 peer never sees a stale gen-1
  block; a 20-iteration end-to-end cycle plus a unit-level invariant
  guard the connect-loop `WaitGroup` join; a 5-cycle lifecycle test
  asserts user handlers receive all three anchor transitions per
  open/close, mirroring the HSMS-SS coverage.
- `secs1` integration: new `tests/secs1_integration/` package with a
  raw-block peer helper and table-driven coverage for multi-block error
  paths (`BlockNumberGap`, `OutOfOrder`, `HeaderMismatch_{Wbit,Stream,
  Function}` → S9F7), T4 inter-block timeout abort, and clean recovery
  for the next message.
- `secs1`: ownership-contract regression guard. Fixed
  `TestConnection_SendGateReopensAfterCloseAndReopen` which violated
  `queueSendRequest`'s success-transfers-ownership invariant by
  Free-ing `msg` unconditionally; the resulting double-Free poisoned
  `hsms.DataMessage`'s `sync.Pool` and surfaced as cross-test races
  under `-count=50 -race -p $(nproc)`. Added
  `TestSession_MultiHandlerDispatch_ConcurrentSendStressor` for multi-
  handler dispatch coverage; the corrected
  `TestConnection_SendGateReopensAfterCloseAndReopen` exercised under
  `make stress-test` is the primary regression guard.
- `hsms`: architectural invariant tests
  (`TestDataMessage_Free_PutResetsFreedAndNilsDataItem`,
  `TestDataMessage_Free_IdempotentWithinSingleCycle`) lock in the two
  pool properties — `freed=0` reset and `dataItem=nil` before
  `pool.Put` — that the new build-tagged content-aware stale-Free
  detector (`hsms/pool_debug.go`, activated via `-tags debugfree`)
  depends on. The detector is zero-cost in default builds and is
  retained as a tool for future hard-to-reproduce pool double-Free
  investigations.
- `hsms`: direct chaos coverage for `ConnStateMgr`
  (`connstate_chaos_test.go`) — handler panic survival, `Stop` while
  a handler is mid-execution, and concurrent handler registration
  during dispatch. A randomized property variant is gated behind
  `//go:build stress`. Fills the gap of previously reaching the
  dispatcher only indirectly through `hsmsss` / `secs1` integration
  tests.
- `hsmsss`: `assertCleanShutdown(t, conn)` polls the five
  authoritative post-close internals (`replyMsgChans`, `replyErrs`,
  `DataMsgInflightCount`, `taskMgr.TaskCount`, `senderMsgChan`) in
  one shot, so chaos / lifecycle tests gate on one line instead of
  five. Teeth verified via a temporary in-tree revert of the
  duplicate-`decDataMsgInflightCount` fix, which made the smoke test
  fail with `DataMsgInflight=-2` as expected.
- `tests/hsmsss_integration`: advisory `snapshotLeaks` /
  `assertSettled` helper for coarse goroutine + (Linux-only) fd
  comparison via `t.Cleanup`. Drift logs `t.Logf` warnings rather
  than failing the test; the package-internal counter gate above is
  authoritative, this layer is defense-in-depth against scheduler /
  fd noise.
- `tests/hsmsss_integration`: `ByteChaosProxy` parallel to the
  existing `ChaosProxy` for faults the filter-driven pump cannot
  express — partial length-header writes, partial payload writes,
  `payload[4]` (PType) / `payload[5]` (SType) substitution, and
  length-header override. Five scenarios drive the matching SEMI-E37
  conformance fixes above (per-byte T8, unsupported-PType/SType
  Reject.req, length-too-large rejection) end-to-end through the
  live wire.

## [1.16.0] - 2026-04-18

Connection-state handler dispatch is split into synchronous and asynchronous
models so that user handlers can safely perform reply-expecting I/O, and the
HSMS-SS Select.req path commits to `SelectedState` before responding on both
sides of the connection.

### Added

- `hsms.ConnStateMgr.AddAsyncHandler` — register connection-state handlers
  that run on a dedicated dispatcher goroutine after the transition commits.
  Async handlers may perform blocking work, including `SendDataMessage` with
  the W-bit set, and may call the synchronous `ToX` state-change methods
  without deadlocking. The existing `AddHandler` remains for library-internal,
  invariant-preserving bookkeeping and is unchanged.
- `make help`, `make fmt`, `make vet`, `make test-all` targets; lint tool
  installation is now gated on availability.

### Changed

- Public `Session.AddConnStateChangeHandler` now dispatches handlers on the
  async path. Handlers that previously observed `cs.State()` inline during
  the transition should rely on the `(prev, new)` arguments they receive —
  live state may have advanced by the time the handler runs.

### Fixed

- `hsmsss` passive Select.req now commits to `SelectedState` synchronously
  before sending Select.rsp. Previously, data messages arriving immediately
  after Select.rsp could be rejected with `RejectNotSelected` while the
  async transition was still in flight.
- `hsmsss` active simultaneous-select branch (SEMI E37 §7.4.3) applies the
  same synchronous commit; on transition failure the peer now receives
  `SelectStatusNotReady` instead of a falsely successful reply.

### Tests

- Async handler dispatcher now has regression coverage for ordering,
  FIFO delivery across transitions, handler-initiated `ToX` calls, and the
  buffer-overflow drop-newest path.
- Added coverage for the already-Selected SelectReq branch on both
  active and passive sides.

## [1.15.1] - 2026-03-09

### Fixed

- `hsmsss`, `secs1`: hardened `isNetError` classification and cleaned up the
  shared error helpers to avoid misclassifying expected shutdown-path errors
  as connection faults.
- Flaky `testAsyncMsgSuccess` made resilient to stale replies arriving from
  a prior iteration.

## [1.15.0] - 2026-03-08

Concurrency hardening release. Focused on eliminating races and deadlocks
around `ConnStateMgr`, message pooling, and reconnect paths, with new chaos
infrastructure to keep the guarantees honest. One new SECS-II type and one
new HSMS-SS tunable.

### Added

- `secs2`: Localized Character String support (FormatCode `0o22`).
- `hsmsss`: configurable linktest failure threshold so operators can tune
  how many consecutive linktest errors trigger a disconnect.
- `hsmsss`: chaos-testing proxy and edge-case scenarios exercising
  partial reads, slow peers, and mid-handshake teardown.

### Changed

- `hsmsss`: replaced `sync.RWMutex`-guarded context fields with
  `atomic.Pointer`, removing a contention hotspot on the hot receive path.
- Upgraded `golangci-lint` to v2 and addressed the new warnings.

### Fixed

- `hsms.ConnStateMgr`: resolved a race/deadlock between `Stop()` and
  in-flight `changeStateAsync` callers, plus a flaky exponential-backoff
  timing test.
- `hsms.DataMessage.Free` is now idempotent; prevents a double-free race
  when a message is returned to the pool along multiple paths.
- `hsmsss` / `secs1`: pooled messages are now freed on every
  drop / reject / drain / queue-fail path to stop the leak of pool-backed
  buffers under error conditions.
- `hsmsss`: `DataMessage` is cloned per handler so concurrent subscribers
  can no longer race over a shared pooled pointer after one of them frees.
- `hsmsss`, `secs1`: fixed a data race in the `sendMsg` timeout handling.
- `hsmsss`: prevented overlapping connect loops with a dedicated
  `connectLoopWg`; a rapid reconnect cycle could previously start a
  second connect loop before the first had exited.
- Multiple smaller fixes for timing flakes and fuzz-test lifecycle issues.

## [1.14.0] - 2026-02-28

HSMS-SS / SECS-I reconnect stability release. Active and passive
connections were reworked to share a single state model, TCP half-open
detection was added, and the decoding edge cases surfaced by last
release's fuzz work were closed out.

### Changed

- `hsmsss`: active and passive connection flows now share the same
  state-machine shape as `secs1`, simplifying reconnect logic and
  eliminating drift between the two sides.
- Documented `opState` and `stateMgr` architecture.

### Fixed

- `hsmsss`: TCP half-open detection via periodic read deadlines and
  TCP keep-alive; a peer that silently disappeared would previously
  leave the connection stuck in `Selected` forever.
- `hsmsss`: resolved reconnect deadlocks surfaced by the active/passive
  alignment refactor.
- `hsms`, `hsmsss`, `secs1`: `loopCtx` accessed under the proper mutex;
  addresses a handful of review-fix items.
- Connection and decoding edge cases caught by the new fuzz / integration
  suites.

## [1.13.2] - 2026-02-15

### Fixed

- `secs1`: improved disconnect detection and added support for runtime
  configuration updates without tearing the connection down.

## [1.13.1] - 2026-02-14

### Added

- `hsmsss`: extracted `messageReader` and added fuzz + integration tests
  around its framing logic.

### Fixed

- `hsmsss`: remaining SEMI E37 compliance gaps identified after the
  v1.13.0 release (deselect and control-message edge cases).

## [1.13.0] - 2026-02-14

### Added

- `hsmsss`: SEMI E37 deselect support and control-message handling —
  Deselect.req / Deselect.rsp / Separate.req are now honoured end-to-end
  and take the session through the documented state transitions.

[1.16.0]: https://github.com/arloliu/go-secs/releases/tag/v1.16.0
[1.15.1]: https://github.com/arloliu/go-secs/releases/tag/v1.15.1
[1.15.0]: https://github.com/arloliu/go-secs/releases/tag/v1.15.0
[1.14.0]: https://github.com/arloliu/go-secs/releases/tag/v1.14.0
[1.13.2]: https://github.com/arloliu/go-secs/releases/tag/v1.13.2
[1.13.1]: https://github.com/arloliu/go-secs/releases/tag/v1.13.1
[1.13.0]: https://github.com/arloliu/go-secs/releases/tag/v1.13.0
