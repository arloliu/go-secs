---
type: Mechanic
title: Activity stamps — the state behind linktest suppression
description: Where "the line is alive" is stored, what writes it, and when it resets to zero knowledge.
tags: [hsmsss, linktest, liveness, generations]
status: stable
generated: {by: "claude/sonnet-5", at: 2026-09-24T11:50:00Z}
verified:
  - {by: "openai/gpt-5.6-terra", at: 2026-09-24T12:34:28Z}
sources:
  - {resource: hsmsss/transport.go, digest: sha256:cf54049476fbfafe, revision: 922feb8}
  - {resource: hsmsss/transport_procedures.go, digest: sha256:9a7bdb8ff23a8e5b, revision: a7ff4a8}
  - {resource: hsmsss/transport_recv.go, digest: sha256:f78883ced9f30422, revision: 922feb8}
  - {resource: hsmsss/transport_active.go, digest: sha256:b4a168040cd91ff8, revision: 922feb8}
  - {resource: hsmsss/transport_passive.go, digest: sha256:f4ebda2502d6b8ac, revision: 922feb8}
  - {resource: hsmsss/transport_control.go, digest: sha256:7b5042e69a84d610, revision: 4eb40d1}
  - {resource: hsms/connection_lifecycle.go, digest: sha256:221b0f7825783fad, revision: 4eb40d1}
---

# What it does

`docs/guides/linktest-suppression.md` documents the three suppression rules, the detection bounds, and the accepted races — read it for *what* suppression does. This entry covers only what the guide leaves out: the state those rules read, and the two moments it is reset or re-armed.

It also corrects one point of drift: the guide says a probe "that times out (T6)" is subject to liveness credit.
The code applies the failure reducer to **any** `WriteMessage` error that is not filtered out first — it never checks `ErrT6Timeout` specifically, so T6 is the common case, not the condition.

That filter is now two-part, not one.
`runLinktest` skips the reducer (and the `LinktestErrCount` increment) when `ctx.Err() != nil` OR the error is `hsms.ErrConnClosed`.
The `ErrConnClosed` half is new: `t.genCtx` IS the hsms epoch's own ctx —
`connection.Open` passes `e.ctx` straight into transport `Start`, which stores it unchanged —
so `runLinktest`'s ctx and the epoch's ctx sit on ONE tree, not two independent ones.
They are still distinct `Done()` channels, though.
Go's context cancellation closes a parent's own channel before it propagates to derived children,
so a teardown can have `sendWaitReplyOn` observe `e.ctx.Done()` and return `ErrConnClosed`
an instant before that same cancellation reaches far enough down the tree for `runLinktest`'s own `ctx.Err()` to read non-nil.
See [linktest teardown exclusion](/hsmsss/linktest-teardown-exemption.md) for the full mechanism;
its own "two separate cancellation cascades" framing was corrected there for the same reason.
Without that second check an ordinary teardown could count as a linktest failure and feed the consecutive-fail threshold that drives an involuntary disconnect, rather than being absorbed by the disconnect path that already signals the same event.

# How it works

Two `atomic.Int64` stamps hold the whole picture: `lastSendStamp` and `lastRecvStamp`. Both carry `monoNanos()` — nanoseconds since an immutable `clockBase` captured at construction and never re-stored. `sinceLastActivity()` is `now - max(send, recv)`.

Activity updates happen at three sites, not two.
`Write` stamps the send side only when `bufs.WriteTo` returned no error.
The recv loop stamps the receive side after a *complete* inbound frame, before dispatch.
`resetActivityStamps` rebaselines both stamps to now whenever a generation publishes its conn —
that third site is not gated on any frame I/O at all.

Rule 1's skip does not simply wait another full interval: it re-arms the timer for `interval - idle`,
so the probe converges on `lastActivity + interval` with no timer shared across goroutines.
Rule 2's skip re-arms for the full interval instead: an outstanding data reply IS T3-bounded,
but `runLinktest` only sees that through `DataMsgInflight()`'s count, not each transaction's own deadline,
so it has nothing finer than the full interval to converge on.

`resetActivityStamps` writes `now` to *both* stamps when a generation publishes its conn.
Both roles do it identically: under `connMu`, in the same critical section that assigns `t.conn`.
A fresh generation therefore starts out believing the line was active this instant.
It is a rebaseline, not a fence — see the invariant below on straggler stamps.

The ordering around that section is inverted from an earlier revision of this entry.
`startActive`/`acceptLoop` now call the generation-gated `t.tcpUp(g.gen, conn)` (publish-socket-and-commit) *before* the `connMu` section, not after —
a refused generation (its epoch already ended) must never reach `t.conn` or the stamp reset at all.
On acceptance the order is: `tcpUp` succeeds, *then* `connMu.Lock(); t.conn = conn; resetActivityStamps(); connMu.Unlock()`.
That is two separate steps, not one atomic publication:
`tcpUp`'s acceptance is the core's socket/state update, while `t.conn` and the stamps are this transport's own state, assigned afterward under `connMu`.
`sinceLastActivity` reads the stamps lock-free, so it is never excluded by that `connMu` section anyway —
a concurrent reader can observe either the pre-reset or post-reset value at any point, race-free but with no ordering tie to `tcpUp`'s acceptance.

# Invariants

- A stamp advances via a **proven** event —
  a write that returned no error, or a fully-read inbound frame —
  or via socket publication, which rebaselines both stamps to now regardless of I/O.
  A partial read, a failed write, or an intent to send never stamps outside that rebaseline.
- The clock base is written once at construction and never re-stored,
  so elapsed-time samples are immune to wall-clock jumps.
  Atomic stores make each individual write race-free, but they are plain stores, not max-updates:
  an overlapping reset or a generation straggler can still overwrite a newer stamp with an older sample,
  so a stamp CAN move backward under those races.
- `lastSendStamp` and `lastRecvStamp` are transport-wide atomics, rebaselined when a generation publishes its socket, not generation-tagged; the reset is not a hard barrier:
  it rebaselines the stamps, it does not fence abandoned goroutines.
  `recvLoop` captures `t.conn` and `t.genCtx` only once, when it starts executing,
  without validating `g.gen` against the live generation —
  a goroutine delayed before that capture can resume after a reconnect,
  capture the successor's socket, and repeatedly read and stamp it.
  The implementation does not establish a universal one-straggler-stamp or one-probe bound;
  do not trust the field comment in `transport.go` that asserts one,
  and do not read the reset as "the new generation cannot see old activity".
- With no stamps yet, `sinceLastActivity` returns the transport's age, which correctly reads as "long idle" — the first probe is never suppressed by uninitialised state.
- The send stamp defers probes but can never forgive a failure; only the receive stamp and the inflight gauge do. A successful local write proves TCP buffering, not a live peer.

# Failure modes

- **Stamping before the event succeeds** — moving the `Write` stamp above its error check, or the recv stamp above frame completion — makes a dying link look permanently alive and suppresses probing indefinitely.
- **Dropping the per-generation reset** would carry the previous generation's activity baseline into the new one,
  instead of starting activity age at socket publication.
  That baseline is always older than the publish time,
  so this makes probing eligible *sooner*, not later — the opposite of "extending detection past the bound".
  It also does not eliminate a late straggler store (see the invariant above);
  it only changes what a straggler's post-reset stamp gets compared against.
- **Re-arming rule 1 for a full interval** instead of the remainder would let a line that goes quiet mid-window wait up to two intervals for its first probe.

# Where to look

- stamp storage, clock base, and both readers: `hsmsss/transport.go` → `(*transport).sinceLastActivity`, `(*transport).monoNanos`, `(*transport).resetActivityStamps`
- send-side stamp, gated on write success: `hsmsss/transport.go` → `(*transport).Write`
- receive-side stamp, after a complete frame: `hsmsss/transport_recv.go` → `(*transport).recvLoop`
- the re-arm arithmetic and the reducer's real trigger: `hsmsss/transport_procedures.go` → `(*transport).runLinktest`, `linktestFailureStep`
- per-generation reset at socket publish, both roles: `hsmsss/transport_active.go`, `hsmsss/transport_passive.go` → the `connMu` section assigning `t.conn`
- the generation-gated publish that now runs before that section (call sites): `hsmsss/transport_active.go`, `hsmsss/transport_passive.go`
- that publish's definition: `hsmsss/transport_control.go` → `(*transport).tcpUp`
- `t.genCtx`'s origin as the epoch's own ctx: `hsms/connection_lifecycle.go` → `(*connection).Open`
