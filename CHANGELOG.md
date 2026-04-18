# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

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
