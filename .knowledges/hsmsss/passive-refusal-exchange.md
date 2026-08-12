---
type: Mechanic
title: The passive refusal exchange's absolute deadline and two-sided Stop handoff
description: Why refuseExtraConn cannot reuse readFrame, and how a token (not a bare field or interface compare) makes the Stop handoff race-free.
tags: [hsmsss, e37, passive, timers, shutdown]
status: draft
generated: {by: "claude/sonnet-5", at: 2026-08-12T00:00:00Z}
sources:
  - {resource: hsmsss/transport_passive.go, digest: sha256:2dedaa78b1882b8d, revision: 3660aa4}
  - {resource: hsmsss/transport.go, digest: sha256:838e98661f3e89d2, revision: 3660aa4}
  - {resource: hsmsss/transport_recv.go, digest: sha256:67343e11cdcaea52, revision: 3660aa4}
---

# What it does

`docs/specs/secs2-hsms-conformance-audit.md` (D6) covers WHY E37 §9.2.4.1.1 option 1 was chosen and
names the machinery at a high level — an absolute deadline, a fixed 4-byte-then-10-byte read, "Stop
publishes a stop flag and closes whatever socket is already in flight".
It does not explain WHY the transport's normal frame reader is structurally wrong for this one-shot
exchange, or the mechanics of the handoff itself: specifically why a monotonic token, rather than a
bare boolean or an interface equality check, is what keeps it race-free.

# How it works

**Why not `readFrame`/`readN`.**
`readN`'s idle-first-byte policy (`transport_recv.go`) deliberately CLEARS the read deadline while
waiting for a frame's first byte — correct for an adopted, live session where the peer may sit idle
indefinitely between messages, but wrong here: an extra dialer that connects and sends nothing would
park the serial refuse loop forever.
T8 (`readN`'s inter-byte bound) does not save it either — T8 bounds only gaps *between* bytes, not a
total connect-to-close budget, so a slow trickle could still hold the socket open indefinitely.
`refuseExtraConn` instead arms ONE absolute deadline (`extra.SetDeadline(clock() + T7)`) before the
first read and never clears or extends it, reading via raw `io.ReadFull` rather than `readN`.

**The two-sided handoff.**
`refuseExtraConn` publishes `extra` into `t.refuseConn` only after checking `!t.refuseStopped` under
`t.refuseMu` — the same mutex and the same flag `haltRefusal` (Stop's side) sets.
If Stop already ran, `refuseExtraConn` observes `refuseStopped == true` and returns (closing `extra`
via its own defer) without reading, instead of parking for the full T7.
If `refuseExtraConn` already published before Stop runs, `haltRefusal` closes the published
`t.refuseConn` directly.
Whichever side wins the race, the extra socket is closed and `Stop`'s `g.accept.Wait` never waits out
the refusal deadline.

**Why `refuseToken`, not `t.refuseConn == extra`.**
`WithListener` lets a caller supply a custom `net.Conn` whose dynamic type may be non-comparable
(e.g. a struct embedding `net.Conn` plus a slice field) — comparing two such interface values with
`==` panics at runtime.
`refuseExtraConn`'s cleanup defer instead increments and captures a monotonic `refuseToken` at
publish time, then clears the slot only if the token still matches at cleanup time.
That makes cleanup a no-op — never a comparison, never a panic risk — whether or not `haltRefusal`
already cleared the slot first.
The token only needs to distinguish "this publish" from "a later one", which the documented
serial-refusal invariant guarantees: at most one token is ever live within one generation.

**The `ArmStart` reset.**
`ArmStart` resets `refuseStopped`/`refuseConn`/`refuseToken` to zero values under `refuseMu`,
generation-scoped exactly like the `startGate.stopping` seal it sits beside.
Without this reset, a prior generation's Stop would leave `refuseStopped == true`, and every later
generation's `refuseExtraConn` calls would silently close-without-responding instead of running the
option-1 exchange — indistinguishable from option 3 (refuse to accept) rather than the intended
option 1 (accept, then answer Communication Already Active).

# Invariants

- `refuseExtraConn` runs serially inside `acceptLoop` (never a goroutine per dialer), and `Stop`
  joins `g.accept` — the WaitGroup leg `acceptLoop` belongs to — before `ArmStart` lets a successor
  generation publish.
  So within one generation at most one `refuseToken` is ever live at cleanup time.
- The deadline is armed once, before the first read, and never cleared or extended — the one
  difference from `readN`'s policy that makes this loop bounded at all.
- `refuseStopped`/`refuseConn`/`refuseToken` are generation-scoped: a value set by one generation's
  Stop must never suppress or corrupt a later generation's refusal.

# Failure modes

- Reusing `readFrame`/`readN` here would let a silent extra dialer park the accept goroutine
  indefinitely — the idle wait has no deadline to expire, so `Stop`'s `g.accept.Wait` never returns
  until the peer eventually closes or times out on its own.
- Comparing `t.refuseConn == extra` directly panics on a non-comparable `net.Conn` implementation
  from a custom `WithListener` — exactly the case `refuseToken` exists to avoid.
- Skipping the `ArmStart` reset lets one generation's Stop silently poison every subsequent
  generation's refusal into close-without-response.

# Where to look

- the one-shot exchange and its absolute deadline: `hsmsss/transport_passive.go` → `(*transport).refuseExtraConn`
- the Stop-side half of the handoff: `hsmsss/transport_passive.go` → `(*transport).haltRefusal`
- the generation-scoped fields and their reset: `hsmsss/transport.go` → `(*transport).ArmStart`
- Stop invoking the handoff before joining `g.accept`: `hsmsss/transport.go` → `(*transport).Stop`
- the idle-vs-T8 policy this path deliberately avoids: `hsmsss/transport_recv.go` → `readN`
