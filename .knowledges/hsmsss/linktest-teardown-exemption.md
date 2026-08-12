---
type: Mechanic
title: LinktestErrCount's teardown exclusion is two independent cancellation cascades, not one
description: Why runLinktest checks both ctx.Err() and errors.Is(err, hsms.ErrConnClosed), and why ErrConnClosed is teardown-exclusive.
tags: [hsmsss, linktest, metrics, shutdown, race]
status: stable
generated: {by: "claude/sonnet-5", at: 2026-08-12T00:00:00Z}
verified:
  - {by: "claude/opus-5", at: 2026-08-12T08:33:19Z}
sources:
  - {resource: hsmsss/transport_procedures.go, digest: sha256:540e993910b596e2, revision: 0965bba}
  - {resource: hsmsss/metrics.go, digest: sha256:cf910bb5649e2dd4, revision: 0965bba}
  - {resource: hsms/connection_send.go, digest: sha256:9b1ccf21a9d24c0d, revision: 038319b}
  - {resource: hsms/errors.go, digest: sha256:ffa4b24a88de6662, revision: 0965bba}
---

# What it does

`LinktestErrCount`'s own godoc (`hsmsss/metrics.go`) already states the WHAT: "a linktest aborted by
connection teardown … is excluded — that outcome is already signaled through the disconnect path,
not this counter."
[Activity stamps](/hsmsss/activity-stamps.md) narrowly corrected the claim that the exclusion tests
only `ctx.Err() != nil`, noting the guard is actually `ctx.Err() != nil || errors.Is(err,
hsms.ErrConnClosed)`.
Neither states WHY the second half of that guard exists, or WHY it is safe to treat any
`hsms.ErrConnClosed` as a teardown signal rather than a genuine link failure that should still count.
This entry is that mechanism.

# How it works

**Why one ctx check is not enough.**
`startLinktest` derives the linktest goroutine's ctx as `context.WithCancel(t.genCtx)` — a value in
the `hsmsss` package's own context tree, rooted at `t.genCtx` (the transport's generation ctx).
The actual `WriteMessage` call inside `runLinktest` runs over a further child,
`context.WithTimeout(ctx, t6)`, so the cancellation signal `runLinktest` checks (`ctx.Err()`) and the
cancellation signal `sendWaitReply` selects on (`e.ctx.Done()`, the `hsms`-package epoch's own ctx)
are two distinct `Done()` channels reached through two distinct trees, both ultimately driven by the
same teardown but with no ordering guarantee on which one a given goroutine observes first.
A teardown can close the epoch's `e.ctx` and have `sendWaitReply`'s select fire on that branch —
returning `ErrConnClosed` — before propagation reaches down through `t.genCtx` → the linktest
goroutine's own child ctx far enough for `ctx.Err()` (read immediately after `WriteMessage` returns)
to observe a non-nil value.
That is what the code comment calls "two separate cancellation cascades with no ordering guarantee
between them."

**Where `ErrConnClosed` actually originates.**
Tracing every `return …ErrConnClosed` in `hsms/connection_send.go`'s send path: `writeFrame` returns
it when the epoch's captured conn is nil (teardown niled the socket before capture) or when its own
`e.ctx.Done()` fast-fail fires; `sendWaitReply`'s reply-wait select returns it on the `e.ctx.Done()`
branch.
Every one of these sites fires only when the CURRENT epoch's ctx is cancelled or its socket already
torn down — i.e., only on connection teardown.
There is no code path in `hsms` that returns `ErrConnClosed` for a live, merely slow or broken link;
a write timeout, a reset connection, or any other transport-level failure on a still-live epoch comes
back from `c.tr.Write` as the RAW transport error and propagates through `writeFrame`/`sendWaitReply`
unchanged — never wrapped or replaced with `ErrConnClosed`.
That is why `errors.Is(err, hsms.ErrConnClosed)` is a safe, teardown-exclusive test: matching it can
only mean "this epoch is tearing down," never "the write failed."

**What the guard buys.**
Without the `ErrConnClosed` half, a teardown that wins the race described above would fall through to
`t.metrics.incLinktestErr()` and feed `linktestFailureStep` — double-signaling a teardown that the
disconnect path already reports, and, worse, feeding the consecutive-failure counter that drives an
involuntary disconnect during what should be an orderly shutdown.

# Invariants

- `errors.Is(err, hsms.ErrConnClosed)` is a proxy for "this epoch tore down," never for "the write
  failed" — no live-link failure in `hsms`'s send path returns or wraps `ErrConnClosed`.
- The two-part guard (`ctx.Err() != nil || errors.Is(err, hsms.ErrConnClosed)`) is required precisely
  because `runLinktest`'s own ctx and the epoch's ctx are cancelled by independent cascades; checking
  only one leaves a window where the other fires first.
- `LinktestErrCount` never influences the linktest-fail-threshold disconnect decision by itself (see
  its own godoc) — this exclusion governs whether an event reaches the counter and the reducer at
  all, not how the reducer weighs it once it does.

# Failure modes

- Dropping the `errors.Is(err, hsms.ErrConnClosed)` half of the guard reintroduces the race: an
  ordinary teardown occasionally counts as a linktest failure and feeds
  `linktestFailureStep`, which can push `fails` past `linktestFailThreshold` and fire a redundant
  `TCPDown` during a shutdown that was already tearing the link down cleanly.
- Widening the match to any `errors.Is(err, ...)` teardown-*adjacent* sentinel without verifying it is
  equally origin-exclusive would reopen the same hole `ErrConnClosed` closes here — the guard's safety
  depends on `ErrConnClosed` never being returned for a live-link condition, not on its name.
- Treating `ctx.Err() != nil` alone as sufficient (removing the `ErrConnClosed` check) is exactly the
  bug [activity stamps](/hsmsss/activity-stamps.md) records as corrected drift.

# Where to look

- the two-part guard: `hsmsss/transport_procedures.go` → `(*transport).runLinktest`
- the ctx derivation that makes the two cascades independent: `hsmsss/transport_procedures.go` → `(*transport).startLinktest`, `(*transport).stopLinktest`
- every teardown-only origin of the sentinel: `hsms/connection_send.go` → `(*connection).writeFrame`, `(*connection).sendWaitReply`
- the sentinel itself: `hsms/errors.go` → `ErrConnClosed`
- the counter's public contract: `hsmsss/metrics.go` → `(*ConnectionMetrics).LinktestErrCount`
