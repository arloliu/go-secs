---
type: Mechanic
title: How a queued report from an ended generation is kept off its successor
description: How a disconnect, T7 expiry, or commit follow-up that speaks for one generation is bound to it on the FSM queue — the epoch identity, the report-side and queue-side checks, the report-time binding of unnamed reports, and how the generation capability reaches hsms from a transport package.
tags: [hsms, hsmsss, lifecycle, supervisor, generation, concurrency]
status: stable
generated: {by: "claude/opus-5.5", at: 2026-09-26T11:59:29Z}
verified:
  - {by: "openai/gpt-5.6-terra", at: 2026-09-26T12:04:05Z}
sources:
  - {resource: hsms/supervisor.go, digest: sha256:597d3ffbcb56fb87, revision: be7a75b}
  - {resource: hsms/connection_lifecycle.go, digest: sha256:84c70134ab8b14a4, revision: be7a75b}
  - {resource: hsms/connection_runtime.go, digest: sha256:7340a3887598a8ba, revision: be7a75b}
  - {resource: hsms/connection.go, digest: sha256:3c8a78070c937d5a, revision: be7a75b}
  - {resource: hsms/epoch.go, digest: sha256:d6cc2283dd4d5e58, revision: be7a75b}
  - {resource: hsms/handler_panic.go, digest: sha256:7c995367269a8805, revision: be7a75b}
  - {resource: hsms/transport.go, digest: sha256:8e0ca0744b8527b9, revision: be7a75b}
  - {resource: hsmsss/transport.go, digest: sha256:038b98574e452c16, revision: be7a75b}
  - {resource: hsmsss/transport_control.go, digest: sha256:ad8c57da5652a769, revision: be7a75b}
  - {resource: hsmsss/transport_active.go, digest: sha256:689f931cb195678f, revision: be7a75b}
  - {resource: hsmsss/transport_passive.go, digest: sha256:f00a1ae9c47094d4, revision: be7a75b}
  - {resource: secs1/transport.go, digest: sha256:399009fc96b7bf6c, revision: be7a75b}
  - {resource: internal/gencap/gencap.go, digest: sha256:b7a6e492698ff6d3, revision: be7a75b}
  - {resource: hsmsss/transport_control_test.go, digest: sha256:d7d6115539bd3043, revision: be7a75b}
---

# What it does

`TransportRuntime`'s godoc says an unnamed `TCPDown` or `T7Expired` is discarded once the generation current at the call has been replaced.
It does not say how a named generation travels from a transport goroutine to the FSM,
which two checks enforce it and why both are needed,
or where the report-time binding of an unnamed report stops helping.
That is the delta this entry records.
[How the three synchronous commits are fenced](/hsms/synchronous-commit-gate.md) records the commit-time side,
[how the recv path's responses and requests are bound on the wire](/hsms/generation-bound-wire-sends.md) the frame side,
and [where a TransitionCause is chosen](/hsms/transition-cause-injection-sites.md) which cause each report carries.

# How it works

**HSMS-SS reports name the generation they speak for.**
SECS-I's own reports do not.
Local Close queues its event with generation 0,
and a base-runtime report names no generation of its own
(an unnamed disconnect or T7 expiry is bound to one at report time, below).
A transport goroutine can outlive the generation that spawned it:
the teardown join is bounded,
so one wedged past the close timeout is abandoned and may resume after a reconnect has established and selected a new link.
Because the plain `TCPDown` entry points resolve `c.cur` and `c.sup` at call time, without a generation identity such a goroutine would drop the SUCCESSOR and report ITS cause to persistent subscribers.
Every ctx check in `hsmsss` narrows that window and none of them closes it —
`recvLoop`'s read-error branch, `handleSeparateReq`, the Select and linktest procedures all share the shape,
and cancellation can always land between the check and the call.

The identity is `epoch.id`, minted from the connection-scoped `genSeq` before the epoch is published on `cur` and never reset per Open cycle.
`hsmsss` reads it once per `Start` (`CurrentGeneration`) and stamps it on that generation's `genWG`,
which already reaches every goroutine it spawns;
each producer passes `g.gen` back through `TCPDownFromGeneration` / `T7ExpiredFromGeneration`.
It is checked in TWO places, and both are load-bearing:

- `connection.injectDisconnect` rejects a nonzero reported generation that differs in IDENTITY
  from the currently published epoch's `id` — it does not inspect `ended`.
  A gen of 0 skips that comparison and is bound, after it, to the `id` of the epoch that same `cur.Load()` returned,
  so the event it queues always names a generation.
  This is what keeps a straggler from setting the SUCCESSOR's `commsFailure`,
  which would suppress that generation's courtesy farewell Separate on a later graceful Close.
  A report whose generation still MATCHES the current (but already-ended) epoch passes this check regardless:
  it can still mark that epoch `commsFailure` and queue its event, same as `step` below;
  neither operation is thereby redirected onto a successor, since a successor cannot yet be published
  while this epoch's teardown is in flight.
- `fsmCommand.gen` carries the identity onto the queue,
  and `step` re-checks it AFTER `state.Load()` and BEFORE the transition.
  The ordering is the whole barrier:
  a successor's state can only be committed after that successor was published to `cur`,
  so reading such a state guarantees the `cur` read that follows observes the successor and discards the event.
  Reversing the two reverts it to a narrowed window.

The match cannot suppress a legitimate disconnect, and the reason is an invariant elsewhere:
`cur` is published in exactly two places (`Open`, `connectLoop`).
`connectLoop` itself starts from either the entering-NotConnected reaction or `Open`'s own
OpenBackground cold-active background retry (Gap 1) after that generation's own first-dial failure.
Either path publishes its successor only after the predecessor's own
teardown and join (`prev.wait()`) completed and the predecessor's own `Start` call returned (`epoch.startReturned`),
so `cur` advances only after the previous generation ended.
A mismatch therefore means the reporting generation's link was already torn down.
This holds for a custom transport whose `Start` errors after already driving TCP-up too:
the failing loop hands the retry to the drop reaction's loop instead of retrying itself,
so only one loop ever publishes a successor
(see [who owns the retry after a failed Start](/hsms/reconnect-retry-ownership.md)).
`s.curGen` must stay a bare atomic load.
`step` runs on the FSM goroutine, which an epoch teardown join can be waiting behind,
so a lock the teardown path holds risks stalling `step` behind it:
`Stop` holds `startGate` only while sealing starts and capturing its generation;
it releases the lock before socket closure and every join,
so it does not hold `startGate` during a join `step` could be waiting behind.

`secs1` does NOT name a generation — its line engine runs under a derived ctx and its own bundle.
Transports using only `TransportRuntime` likewise pass no identity.
Their unnamed disconnect and T7 reports are still fenced on the queue:
`injectDisconnect` and `injectT7Expiry` bind a gen of 0, at report time,
to the `id` of the generation current then (a nil `cur` enqueues nothing),
so `step`'s match discards the event once a successor is current, however long it sat queued.
The binding does not cover a report made AFTER the successor was published:
that call finds the successor current and binds to it,
which is why `hsmsss` names the generation it captured at its own `Start` instead.
In a connection-backed supervisor, only `evClose` reaches the queue with gen 0 and skips `step`'s identity match;
a nil-gate supervisor built without a connection (unit tests) may also enqueue gen 0.
The follow-up event of a synchronous commit (TCP-up, Select-accepted, Select-lost) carries the id of the epoch its gate validated, named or not,
so it is matched like any named report
(see [how the three synchronous commits are fenced](/hsms/synchronous-commit-gate.md)).
`disconnectHandlerGeneration` rejects a gen of 0 outright rather than letting it through as a wildcard.

**How the generation capability crosses the package boundary.**
`hsmsss` reaches the generation-aware methods by type-asserting `t.rt` to a package-local `genRuntime` interface,
and prefers it over the cause-only `causeRuntime` path when present.
`genRuntime` and hsms's `genCapability` are local aliases of one
`gencap.GenerationRuntime[hsms.Message, hsms.TransitionCause]` instantiation —
ten methods:
the `CurrentGeneration` accessor, five `*FromGeneration` report/commit methods,
two generation-named send methods (`SendAsyncFromGeneration` / `WriteMessageFromGeneration`),
and two generation-named INBOUND methods (`DeliverOwnedFrameFromGeneration` / `RouteReplyFromGeneration`,
which fence admission of a frame/reply to the generation whose recv goroutine actually read it) —
and hsms asserts at compile time that `*connection` satisfies it —
see [the inbound generation fence](/hsms/inbound-generation-fence.md) for how the two newer inbound methods admit or route on behalf of a named generation,
so a method-set drift between the offering and the consuming side is a build failure rather than a silent fallback.
Widening the exported `TransportRuntime` instead would break external implementers,
which is why `secs1` and every transport using only `TransportRuntime` keep the unnamed paths above.
`hsmsss/transport_control_test.go`'s `genRecRT` is a test runtime that implements this capability,
so generation-aware transport behavior can be exercised without a concrete `*connection`.


# Failure modes

**A straggler dropping the successor.**
Without the named generation, an abandoned `hsmsss` goroutine resuming after a reconnect reports through `TCPDown`,
which resolves `c.cur` at call time:
it drops the SUCCESSOR, sets the successor's `commsFailure` (suppressing that link's courtesy Separate on a later graceful Close),
and reports its cause to persistent subscribers.
Dropping either check reopens part of it:
without the report-side check a straggler can still set the successor's `commsFailure`;
without the queue-side re-check, an event queued before the successor was published drops that successor when it is processed.


**A late unnamed report bound to the successor.**
An unnamed disconnect or T7 report made only AFTER the successor was published binds to the successor, and nothing can tell it apart from a report for that link.
`secs1` and transports using only `TransportRuntime` are exposed; `hsmsss` names the generation it captured at its own `Start` and is not.

# Where to look

- generation identity: `hsms/epoch.go` → `epoch.id`;
  `hsms/connection.go` → `genSeq`;
  `hsms/connection_lifecycle.go` → `CurrentGeneration` and the two `cur.Store` sites
- report-side check and report-time binding: `hsms/connection_lifecycle.go` → `TCPDownFromGeneration`, `injectDisconnect`;
  `hsms/connection_runtime.go` → `T7ExpiredFromGeneration`, `injectT7Expiry`;
  `hsms/handler_panic.go` → `(*connection).disconnectHandlerGeneration`
- the queue-side barrier: `hsms/supervisor.go` → `step`'s generation match, `injectFrom`, `curGen`, `staleGen`
- the shared capability type: `internal/gencap/gencap.go` → `GenerationRuntime`; `hsms/connection_lifecycle.go` → `genCapability`
- transport side: `hsmsss/transport_control.go` → `genRuntime`, `transport.tcpDown`, `transport.t7Expired`;
  `hsmsss/transport.go` → `genWG.gen`;
  `hsmsss/transport_active.go` → `(*transport).startActive`;
  `hsmsss/transport_passive.go` → `(*transport).startPassive`;
  `secs1/transport.go` → `causeRuntime`
- a test runtime implementing the generation capability: `hsmsss/transport_control_test.go` → `genRecRT`
