---
type: Mechanic
title: Linktest teardown exclusion covers cancellation propagation and stale generations
description: Why runLinktest's two-part ErrConnClosed guard exists, and how far "teardown-exclusive" actually reaches.
tags: [hsmsss, linktest, metrics, shutdown, race]
status: stable
generated: {by: "claude/sonnet-5", at: 2026-09-25T02:38:18Z}
verified:
  - {by: "openai/gpt-6-astra", at: 2026-09-25T02:55:36Z}
  - {by: "openai/gpt-5.6-terra", at: 2026-09-25T03:13:36Z}
sources:
  - {resource: hsmsss/transport_procedures.go, digest: sha256:bf47bd9825ddb5da, revision: 6c257b6}
  - {resource: hsmsss/metrics.go, digest: sha256:e822f4b53757800e, revision: 922feb8}
  - {resource: hsms/connection_send.go, digest: sha256:d2e809d7d0d95711, revision: a7ff4a8}
  - {resource: hsms/errors.go, digest: sha256:3057101139d08434, revision: a7ff4a8}
  - {resource: hsms/connection_lifecycle.go, digest: sha256:221b0f7825783fad, revision: 4eb40d1}
  - {resource: hsms/epoch.go, digest: sha256:d97677e4f16e9462, revision: 4eb40d1}
  - {resource: hsmsss/transport.go, digest: sha256:14cd2584fee0dbab, revision: 6c257b6}
  - {resource: hsmsss/transport_active.go, digest: sha256:80daec469fc1444e, revision: 6c257b6}
  - {resource: hsmsss/transport_passive.go, digest: sha256:baa34d672a03a889, revision: 6c257b6}
---

# What it does

`LinktestErrCount`'s own godoc (`hsmsss/metrics.go`) says a linktest aborted by connection
teardown is excluded — that outcome is already signaled through the disconnect path, not this
counter — though the godoc overstates this as an unconditional property of the sentinel; see the
correction below.
[Activity stamps](/hsmsss/activity-stamps.md) narrowly corrected the claim that the exclusion tests
only `ctx.Err() != nil`, noting the guard is actually `ctx.Err() != nil || errors.Is(err,
hsms.ErrConnClosed)`.
Neither states WHY the second half of that guard exists, or exactly how far "teardown-exclusive"
reaches.
This entry is that mechanism.

# How it works

**Why one ctx check is not enough.**
`g.ctx` (`genWG.ctx`) IS the hsms epoch's own ctx, not a separate tree: `connection.Open`
(`hsms/connection_lifecycle.go`) passes `e.ctx` straight into transport `Start`
(`hsmsss/transport.go`), and `startActive`/`startPassive` stamp it, unchanged, onto the generation's
own bundle (`g.ctx = ctx`) before spawning `recvLoop` — since the R10 inbound-generation-fence work
this replaced an earlier single transport-wide `t.genCtx` field with the same value, now scoped per
generation on `genWG` instead.
`startLinktest` derives the linktest goroutine's ctx as `context.WithCancel(g.ctx)`, and the
actual `WriteMessage` call inside `runLinktest` runs over a further child,
`context.WithTimeout(ctx, t6)` — so `ctx`, `lctx`, and `e.ctx` all sit on ONE context tree, rooted
at the same epoch, not on two independent trees.
The two `Done()` channels this guard cares about are still distinct, though: `ctx.Done()`, which
`runLinktest` polls via `ctx.Err()`, and `e.ctx.Done()`, which `sendWaitReplyOn`'s reply-wait select
observes directly.
Go's `context.cancelCtx.cancel` closes a parent's own `Done` channel before it walks its children
map and cancels each of them.
A teardown that cancels the epoch's ctx can therefore have `sendWaitReplyOn` observe `e.ctx.Done()`
and return `ErrConnClosed` an instant before that same cancellation propagates down through
`g.ctx`'s child far enough for `runLinktest`'s own `ctx.Err()` — read immediately after
`WriteMessage` returns — to observe a non-nil value.
The code comment in `runLinktest` used to call this "two separate cancellation cascades with no
ordering guarantee between them"; `a7ff4a8` corrected it to say the same thing this entry does —
one context tree, with the parent's `Done` closing before it reaches the child, not two independent
cascades.

**Where `ErrConnClosed` actually originates.**
Tracing every `return …ErrConnClosed` in `hsms/connection_send.go`'s send path: `writeFrame`
returns it when the epoch's captured conn is nil, or when its own `e.ctx.Done()` fast-fail fires;
`sendWaitReplyOn`'s reply-wait select returns it on the `e.ctx.Done()` branch.
A nil conn is not exclusively a teardown symptom: `newEpoch` (`hsms/epoch.go`) starts a fresh epoch
with no socket at all, so a control send issued before the transport ever publishes one — before
any `TCPUp` — reaches the same `conn == nil` branch and the same `ErrConnClosed`, with no teardown
in progress.
Auto-linktest only ever starts via `startLinktest` on a committed Selected session, and a socket is
always published by then, so on its own normal path every `ErrConnClosed` origin IS in practice a
lifecycle exclusion — teardown, or a stale generation losing the race in `WriteMessageFromGeneration`.
But the sentinel itself is not globally teardown-exclusive; `ErrConnClosed`'s own godoc
(`hsms/errors.go`) now says so too, naming the not-yet-published-socket case explicitly (`a7ff4a8`).
There remains no code path in `hsms` that returns `ErrConnClosed` for a live, merely slow or broken
link on an already-published socket: a write timeout, a reset connection, or any other
transport-level failure on a still-live epoch comes back from `c.tr.Write` as the RAW transport
error and propagates through `writeFrame`/`sendWaitReplyOn` unchanged — never wrapped or replaced
with `ErrConnClosed`.
That is why `errors.Is(err, hsms.ErrConnClosed)` is a safe test on auto-linktest's own call pattern:
matching it can only mean "this epoch is tearing down, or this generation already ended," never
"the write failed on a live link."

**A fourth origin, added alongside generation isolation.**
`runLinktest`'s `WriteMessage` call now goes through `t.writeMessage(lctx, g.gen, ...)`, which — when the runtime
offers the generation-aware capability — resolves to `connection.WriteMessageFromGeneration`.
That function returns `ErrConnClosed` directly, without ever reaching `writeFrame` or `sendWaitReplyOn`,
when the generation named by the caller (`g.gen`) is no longer the live one (`connection.liveEpoch` misses).
This is still teardown-exclusive from the reporting goroutine's own point of view: a stale linktest
goroutine's generation has ended, which is exactly the condition this guard exists to treat as teardown
rather than as a link failure, even while some other, newer generation's link may be running fine.

**What the guard buys.**
Without the `ErrConnClosed` half, a teardown that wins the race described above would fall through to
`t.metrics.incLinktestErr()` and feed `linktestFailureStep` — double-signaling a teardown that the
disconnect path already reports, and, worse, feeding the consecutive-failure counter that drives an
involuntary disconnect during what should be an orderly shutdown.

# Invariants

- `errors.Is(err, hsms.ErrConnClosed)` on auto-linktest's own send is a proxy for
  "this epoch tore down, or this generation already ended" — never for "the write failed on a live link."
  The sentinel is not globally teardown-exclusive — a control send before the first `TCPUp` hits the
  same nil-conn branch with no teardown in progress — but auto-linktest never sends before Selected
  commits, so its own path never observes that case.
- The two-part guard (`ctx.Err() != nil || errors.Is(err, hsms.ErrConnClosed)`) is required because
  `ctx` and `e.ctx` are two distinct `Done()` channels on the SAME context tree, and a cancelling
  parent's `Done` channel closes before its children's — checking only one leaves a window where
  the other fires first.
- `LinktestErrCount` never influences the linktest-fail-threshold disconnect decision by itself (see
  its own godoc) — this exclusion governs whether an event reaches the counter and the reducer at
  all, not how the reducer weighs it once it does.

# Failure modes

- Dropping the `errors.Is(err, hsms.ErrConnClosed)` half of the guard reintroduces the race: an
  ordinary teardown occasionally counts as a linktest failure and feeds
  `linktestFailureStep`, which can push `fails` past `linktestFailThreshold` and fire a redundant
  `TCPDown` during a shutdown that was already tearing the link down cleanly.
- Widening the match to any `errors.Is(err, ...)` teardown-*adjacent* sentinel without verifying it
  is equally origin-exclusive would reopen the hole this guard closes: an error also produced by a
  genuine live-link failure would suppress `LinktestErrCount` and keep that failure from ever
  reaching `linktestFailureStep`, hiding a real dead link instead of reporting it through the
  disconnect path.
- Treating `ctx.Err() != nil` alone as sufficient (removing the `ErrConnClosed` check) is exactly the
  bug [activity stamps](/hsmsss/activity-stamps.md) records as corrected drift.

# Where to look

- the two-part guard: `hsmsss/transport_procedures.go` → `(*transport).runLinktest`
- the ctx derivation that makes the two Done channels distinct: `hsmsss/transport_procedures.go` → `(*transport).startLinktest`, `(*transport).stopLinktest`
- `g.ctx`'s origin as the epoch's own ctx: `hsms/connection_lifecycle.go` → `(*connection).Open`;
  `hsmsss/transport_active.go` → `startActive`;
  `hsmsss/transport_passive.go` → `startPassive`;
  `hsmsss/transport.go` → `genWG.ctx`
- every origin of the sentinel, including the not-yet-published-socket case: `hsms/connection_send.go` → `(*connection).writeFrame`, `(*connection).sendWaitReplyOn`, `(*connection).WriteMessageFromGeneration`; `hsms/epoch.go` → `newEpoch`, `(*epoch).liveConn`
- the sentinel itself: `hsms/errors.go` → `ErrConnClosed`
- the counter's public contract: `hsmsss/metrics.go` → `(*ConnectionMetrics).LinktestErrCount`
