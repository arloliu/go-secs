---
type: Mechanic
title: Activity stamps — the state behind linktest suppression
description: Where "the line is alive" is stored, what writes it, and when it resets to zero knowledge.
tags: [hsmsss, linktest, liveness, generations]
status: stable
generated: {by: "claude/sonnet-5", at: 2026-08-12T00:00:00Z}
verified:
  - {by: "claude/opus-5", at: 2026-08-12T08:33:19Z}
sources:
  - {resource: hsmsss/transport.go, digest: sha256:838e98661f3e89d2, revision: 3660aa4}
  - {resource: hsmsss/transport_procedures.go, digest: sha256:540e993910b596e2, revision: 3660aa4}
  - {resource: hsmsss/transport_recv.go, digest: sha256:67343e11cdcaea52, revision: 3660aa4}
  - {resource: hsmsss/transport_active.go, digest: sha256:642cebd8a000d63d, revision: 3660aa4}
  - {resource: hsmsss/transport_passive.go, digest: sha256:2dedaa78b1882b8d, revision: 3660aa4}
---

# What it does

`docs/guides/linktest-suppression.md` documents the three suppression rules, the detection bounds, and the accepted races — read it for *what* suppression does. This entry covers only what the guide leaves out: the state those rules read, and the two moments it is reset or re-armed.

It also corrects one point of drift: the guide says a probe "that times out (T6)" is subject to liveness credit.
The code applies the failure reducer to **any** `WriteMessage` error that is not filtered out first — it never checks `ErrT6Timeout` specifically, so T6 is the common case, not the condition.

That filter is now two-part, not one.
`runLinktest` skips the reducer (and the `LinktestErrCount` increment) when `ctx.Err() != nil` OR the error is `hsms.ErrConnClosed`.
The `ErrConnClosed` half is new: the hsms epoch's own ctx can cancel a pending `WriteMessage` an instant before this goroutine's parent ctx (`t.genCtx`) becomes observably cancelled — two separate cancellation cascades with no ordering guarantee between them —
so a teardown-aborted write can return `ErrConnClosed` while `ctx.Err()` still reads nil.
Without that second check an ordinary teardown could count as a linktest failure and feed the consecutive-fail threshold that drives an involuntary disconnect, rather than being absorbed by the disconnect path that already signals the same event.

# How it works

Two `atomic.Int64` stamps hold the whole picture: `lastSendStamp` and `lastRecvStamp`. Both carry `monoNanos()` — nanoseconds since an immutable `clockBase` captured at construction and never re-stored. `sinceLastActivity()` is `now - max(send, recv)`.

Writes happen at exactly two sites. `Write` stamps the send side only when `bufs.WriteTo` returned no error. The recv loop stamps the receive side after a *complete* inbound frame, before dispatch.

Rule 1's skip does not simply wait another full interval: it re-arms the timer for `interval - idle`, so the probe converges on `lastActivity + interval` with no timer shared across goroutines. Rule 2's skip re-arms for the full interval instead, because an outstanding reply has no deadline of its own to converge on.

`resetActivityStamps` writes `now` to *both* stamps when a generation publishes its conn. Both roles do it identically: under `connMu`, in the same critical section that assigns `t.conn`, and before `rt.TCPUp`. A fresh generation therefore starts out believing the line was active this instant. It is a rebaseline, not a fence — see the invariant below on straggler stamps.

# Invariants

- A stamp advances only on a **proven** event: a write that returned no error, or a fully-read inbound frame. A partial read, a failed write, or an intent to send never stamps.
- The clock base is written once at construction and never re-stored, so stamps are race-free to read and immune to wall-clock jumps — no stamp can ever move backward.
- Suppression state is **per generation**, but the reset is not a hard barrier. The rebaseline sits inside the same `connMu` section that publishes the socket; a straggler goroutine from the torn-down generation can still stamp **at most once** afterwards. That is bounded and accepted: it delays or credits at most one probe, and it is the mechanism behind the guide's doubled worst-case bound immediately after a reconnect. Do not read the reset as "the new generation cannot see old activity".
- With no stamps yet, `sinceLastActivity` returns the transport's age, which correctly reads as "long idle" — the first probe is never suppressed by uninitialised state.
- The send stamp defers probes but can never forgive a failure; only the receive stamp and the inflight gauge do. A successful local write proves TCP buffering, not a live peer.

# Failure modes

- **Stamping before the event succeeds** — moving the `Write` stamp above its error check, or the recv stamp above frame completion — makes a dying link look permanently alive and suppresses probing indefinitely.
- **Dropping the per-generation reset** would let a stamp from a torn-down session suppress the first probe of a new one, extending dead-link detection past the documented post-reconnect bound.
- **Re-arming rule 1 for a full interval** instead of the remainder would let a line that goes quiet mid-window wait up to two intervals for its first probe.

# Where to look

- stamp storage, clock base, and both readers: `hsmsss/transport.go` → `(*transport).sinceLastActivity`, `(*transport).monoNanos`, `(*transport).resetActivityStamps`
- send-side stamp, gated on write success: `hsmsss/transport.go` → `(*transport).Write`
- receive-side stamp, after a complete frame: `hsmsss/transport_recv.go` → `(*transport).recvLoop`
- the re-arm arithmetic and the reducer's real trigger: `hsmsss/transport_procedures.go` → `(*transport).runLinktest`, `linktestFailureStep`
- per-generation reset at socket publish, both roles: `hsmsss/transport_active.go`, `hsmsss/transport_passive.go` → the `connMu` section assigning `t.conn`
