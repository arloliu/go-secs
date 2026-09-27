---
type: Mechanic
title: The passive refusal exchange's absolute deadline and two-sided Stop handoff
description: Why refuseExtraConn cannot reuse readFrame, and how a token (not a bare field or interface compare) makes the Stop handoff race-free.
tags: [hsmsss, e37, passive, timers, shutdown]
status: stable
generated: {by: "claude/sonnet-5", at: 2026-08-12T00:00:00Z}
verified:
  - {by: "claude/opus-5", at: 2026-08-12T08:33:19Z}
sources:
  - {resource: hsmsss/transport_passive.go, digest: sha256:eaee7f67c2e994fc, revision: 7ae1ff0}
  - {resource: hsmsss/transport.go, digest: sha256:176fff888fc8a85e, revision: 7ae1ff0}
  - {resource: hsmsss/transport_recv.go, digest: sha256:fc971b2806f82a52, revision: 7ae1ff0}
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
`refuseExtraConn` publishes `extra`'s socket record into `t.refuseSock` only after checking `!t.refuseStopped` under
`t.refuseMu` — the same mutex and the same flag `haltRefusal` (Stop's side) sets.
If Stop already ran, `refuseExtraConn` observes `refuseStopped == true` and returns (closing `extra`
via its own defer) without reading, instead of parking for the full T7.
If `refuseExtraConn` already published before Stop runs, `haltRefusal` closes the published
`t.refuseSock` through its record's close gate, with no failure, since Stop's close is a local one.
Whichever side wins the race, the extra socket is closed and `Stop`'s `g.accept.Wait` never waits out
the refusal deadline.

**One close, through the gate.**
`refuseExtraConn` mints a socket record for `extra` on entry (reported as accepted) and reports it refused at once,
before the exchange and before the record is published to `haltRefusal`, the only other side that can close it,
so the refusal always precedes the close.
Every exit closes it through ONE deferred gate call carrying the failure retained by whichever branch returned:
the `SetDeadline` error, a read error (the absolute deadline, a short read), a malformed length (`errRefusalBadLength`),
a first frame that fails to decode or is not a Select.req (`errRefusalNotSelectReq`),
or a failed or short response write;
a completed exchange, and a helper that found `refuseStopped` already set, retain nil.
The gate reports the socket closed exactly once,
whichever of the defer and `haltRefusal` reaches it first.
The exchange itself lives in `refusalExchange`,
which reads the 4-byte prefix and the 10-byte header into one fixed 14-byte buffer.
It never calls the wire observer: the accept goroutine it runs on is one `Stop` joins without a bound,
so an observer that did not return there would hang `Stop`, hence `Close`.
Instead `captureRefusal` opens the record's report scope (`openReport`) and allocates one `refusalCapture`,
into which the exchange copies each frame as it crosses, with its `At` taken at that moment:
the peer's first frame, whole, once its header is read and before it is interpreted,
and the Select.rsp only when its write returned the full length without error.
`refusalCapture.finish`, deferred after the close defer so it runs first, on a panic too,
hands the capture to a new goroutine (`deliver`) while the scope is still held,
which reports the frames in wire order, the peer's first, with generation 0, and then releases the scope;
when no frame crossed, `finish` releases the scope inline and no goroutine is started.
Nothing joins that goroutine: an observer that never returns leaks it and delays only that socket's `SocketClosed`.
The close is reported after the frames whichever order the gate and the release run in:
if the deferred close (or a `haltRefusal` close landing mid-exchange) runs the gate while the scope is held,
the gate closes the socket at once and leaves the `SocketClosed` report to the release;
if the release ran first, the gate finds no scope open and reports the close itself, after the frames the release followed.
The report carries the time the gate closed the socket, not the time of its delivery.

**Why `refuseToken`, not comparing the published conn with `extra`.**
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
`ArmStart` resets `refuseStopped`/`refuseSock`/`refuseToken` to zero values under `refuseMu`,
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
- `refuseStopped`/`refuseSock`/`refuseToken` are generation-scoped: a value set by one generation's
  Stop must never suppress or corrupt a later generation's refusal.

# Failure modes

- Reusing `readFrame`/`readN` here would let a silent extra dialer park the accept goroutine
  indefinitely — the idle wait has no deadline to expire, so `Stop`'s `g.accept.Wait` never returns
  until the peer eventually closes or times out on its own.
- Comparing the published conn with `extra` (`==`) panics on a non-comparable `net.Conn` implementation
  from a custom `WithListener` — exactly the case `refuseToken` exists to avoid.
- Skipping the `ArmStart` reset lets one generation's Stop silently poison every subsequent
  generation's refusal into close-without-response.

# Where to look

- the one-shot exchange and its absolute deadline: `hsmsss/transport_passive.go` → `(*transport).refuseExtraConn`
- the Stop-side half of the handoff: `hsmsss/transport_passive.go` → `(*transport).haltRefusal`
- the exchange and the frames it reports: `hsmsss/transport_passive.go` → `(*transport).refusalExchange`
- the socket record and its close gate: `hsmsss/transport_socket.go` → `socketRecord`, `(*transport).mintSocket`, `(*socketRecord).gate`
- the generation-scoped fields and their reset: `hsmsss/transport.go` → `(*transport).ArmStart`
- Stop invoking the handoff before joining `g.accept`: `hsmsss/transport.go` → `(*transport).Stop`
- the idle-vs-T8 policy this path deliberately avoids: `hsmsss/transport_recv.go` → `readN`
