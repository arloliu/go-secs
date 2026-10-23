---
type: Mechanic
title: The inbound generation fence
description: How an inbound frame or reply is bound to the generation whose recv goroutine actually read it, and the two different kinds of cutoff that enforce it.
tags: [hsms, hsmsss, generations, inbound, fan-out]
status: stable
generated: {by: "claude/opus-5.5", at: 2026-10-23T18:00:00Z}
verified:
  - {by: "openai/gpt-6-astra", at: 2026-10-23T18:42:42Z}
sources:
  - {resource: hsms/connection_runtime.go, digest: sha256:7f242f475b188a1b, revision: 2a92a5e}
  - {resource: hsms/connection_lifecycle.go, digest: sha256:84c70134ab8b14a4, revision: 2a92a5e}
  - {resource: hsms/connection.go, digest: sha256:42714e9581c0b14e, revision: 2a92a5e}
  - {resource: hsms/session.go, digest: sha256:04cc770917f02f59, revision: 2a92a5e}
  - {resource: hsms/handler_panic.go, digest: sha256:7c995367269a8805, revision: 2a92a5e}
  - {resource: hsms/data_msg.go, digest: sha256:a90443b564284b51, revision: 2a92a5e}
  - {resource: hsms/epoch.go, digest: sha256:d6cc2283dd4d5e58, revision: 2a92a5e}
  - {resource: hsms/connection_send.go, digest: sha256:ea94ab4fc9d25bb9, revision: 2a92a5e}
  - {resource: hsms/connection_metrics.go, digest: sha256:87b11ae3785c0870, revision: 2a92a5e}
  - {resource: hsms/supervisor.go, digest: sha256:597d3ffbcb56fb87, revision: 2a92a5e}
  - {resource: hsmsss/transport_recv.go, digest: sha256:f8d35637783f58a6, revision: 2a92a5e}
  - {resource: hsmsss/transport_control.go, digest: sha256:ad8c57da5652a769, revision: 2a92a5e}
  - {resource: hsmsss/transport.go, digest: sha256:038b98574e452c16, revision: 2a92a5e}
  - {resource: hsmsss/metrics.go, digest: sha256:e822f4b53757800e, revision: 2a92a5e}
  - {resource: internal/gencap/gencap.go, digest: sha256:b7a6e492698ff6d3, revision: 2a92a5e}
  - {resource: secs1/transport.go, digest: sha256:8751c0d91c45f678, revision: 2a92a5e}
  - {resource: hsmsss/transport_socket.go, digest: sha256:7eb2e5f887e7f1c3, revision: 2a92a5e}
---

# What it does

An inbound HSMS-SS data frame or control reply is bound to the generation whose recv goroutine read it,
not to whatever generation is current when the call runs.
Two cutoffs enforce that in sequence: one-shot admission under `genGate` (cutoff #1),
then a per-step cancellation check during fan-out (cutoff #2).
One resolved `*epoch` carries an admitted frame through to fan-out and the handler's reply.
SECS-I's plain ingress gets the origin stamp but not the admission cutoff.

# How it works

**The sequence, end to end.**
`hsmsss.recvLoop` reads off the socket it was handed at spawn —
never a re-read of `t.conn`; see [activity stamps](/hsmsss/activity-stamps.md) —
through `readObserved`, which stamps `lastRecvStamp` for every complete frame
and attempts wire-observer reporting only when there is no socket record or it acquired that socket's report scope.
`recvLoop` then fires `testHookAfterReadFrame` (nil in production) and calls `dispatchFrame`.
All of this precedes any generation-liveness decision.
`dispatchFrame` can answer a frame without ever reaching admission:
a bad PType/SType or a control frame with a body gets a Reject.req,
a data frame while not Selected gets a Reject (reason 4),
Select.req, Deselect.req, and Linktest.req go to `handleControlReq`,
Separate.req goes to `handleSeparateReq`, and Reject.req follows reply routing.
Otherwise it routes a data frame to `t.deliverOwnedFrame(g.gen, frame)`
and a control response to `t.routeReply(g.gen, msg)` (`hsmsss/transport_control.go`).
Both type-assert the runtime to `genRuntime` (`gencap.GenerationRuntime`) and,
when it is offered, call `DeliverOwnedFrameFromGeneration`/`RouteReplyFromGeneration` naming `g.gen` —
the generation this recv goroutine was spawned for.

**Cutoff #1: admission, one-shot, behind a lock.**
`DeliverOwnedFrameFromGeneration`/`RouteReplyFromGeneration` first hand a `gen` of 0
to their plain counterparts (`DeliverOwnedFrame`/`RouteReply`);
`liveEpoch` itself has no zero-generation special case.
A nonzero `gen` is resolved via `connection.liveEpoch(gen)` (`hsms/connection_lifecycle.go`),
which takes `genGate.RLock`, checks `cur.Load()` against `{id == gen, !ended}`,
and returns either the live `*epoch` or `nil`.
A refusal is not a decode error and is not visible to `hsmsss`:
the frame is dropped, `connection.staleRecv` is incremented,
and a `Debug` log line names the reported and current generation.
`dispatchFrame` sees only that the call completed:
a dropped frame returns `nil`;
a dropped reply reports `true`,
deliberately, so `dispatchFrame`'s HIT branch runs rather than its Reject-sending MISS branch.

**Once admitted, one resolved epoch carries the rest.**
`deliverOwnedFrameOn(e, frame)` decodes,
then, whenever `e` is non-nil, stamps `dm.originIdent` from `c.ident` and `dm.originGen` from `e.id` (never from a fresh `c.cur.Load()`).
The stamp precedes the primary/secondary split, so secondaries are stamped too.
When session-ID validation is on, it runs `checkSessionID(e, dm)`,
which drops a mismatch in either role.
Only in the equipment role (`cfg.equipmentRole`) does it first enqueue an S9F1 through `e`, via `enqueueAsync`,
so a mismatch admitted by a generation that ends before the enqueue runs
drops the S9F1 with that generation's stranded queue rather than landing on a successor.
An inbound S9F1 is exempt from the check and never dropped.
Next it fires `testHookBeforeFanout` (nil in production),
calls `correlateS9On(e, dm)`,
which can end a waiting send through `e.replies` (see [S9 correlation](/hsms/s9-correlation.md)),
and then calls `routeReplyOn(e, dm)` for a secondary and `routeDataOn(e, dm)` for a primary or a reply miss.
None of these steps re-resolves `c.cur`.

**Cutoff #2: fan-out, ongoing, never under `genGate`.**
`routeDataOn` (`hsms/connection_runtime.go`) does not consult `liveEpoch` again.
It computes `done := epochDone(e)` — `e.ctx.Done()`, never `e.done` —
and passes it to `recvDataMsgOn`/`dispatchDecodeErrorOn` (`hsms/session.go`).
Each snapshots its registrations under `s.mu.RLock`, releases it,
then rechecks `done` with a non-blocking `select` before EACH handler call and EACH channel send.
Unlike #1, this observes cancellation partway through a long fan-out;
a handler whose check already passed is invoked,
and a channel delivery whose check passed still selects between the send and `done`, so it is not guaranteed to deliver.
`Done()`'s own doc (`hsms/connection.go`) explains why the fan-out cannot use `e.done`:
`e.done` closes only after `(*epoch).join` completes the bounded join,
and that join waits (via the recv goroutine) for this same fan-out to return —
selecting on `e.done` here would be a circular wait.

**The resolved generation also reaches the handler call.**
`routeDataOn` derives `gen` from `e.id` (0 when `e` is nil)
and passes it through `recvDataMsgOn`/`dispatchDecodeErrorOn` to `callDataHandler`/`callDecodeErrorHandler`, never re-derived.
On the connection-backed path (`s.rt` implements `handlerHost`),
those run the handler under `runCallback`: a panic is recovered and counted,
and a handler that ends via `runtime.Goexit` reports `disconnectHandlerGeneration(gen)` —
the admitted epoch's own ID, not whatever is current once the handler exits.
A zero `gen` (`disconnectHandlerGeneration`), no current epoch, or a mismatched ID (`injectDisconnect`) injects nothing.
A matching epoch is marked as a communications failure,
and a disconnect event is injected if a supervisor exists;
`ended` is never checked —
see [the generation report fence](/hsms/generation-report-fence.md) and [callback isolation](/hsms/handler-panic-goexit-isolation.md).

**Where the test hooks sit.**
One nil-by-default hook precedes cutoff #1: `testHookAfterReadFrame`, fired in `recvLoop` before `dispatchFrame`.
Two follow it: `testHookBeforeS9F1Enqueue` (in `checkSessionID`, equipment role only, before the S9F1 enqueue)
and `testHookBeforeFanout` (in `deliverOwnedFrameOn`, before S9 correlation and reply/data routing).
Each lets a test advance the generation in exactly that window.

**The reply side reuses the same origin, through a token rather than a pointer.**
`session.reply` (`hsms/session.go`), the shared body of `ReplyDataMessage` and `ReplyAbort`,
compares `primary.originIdent` against `os.originIdentity()` by pointer identity
(`*connIdentity`, one byte so distinct tokens never alias)
and, on a match, calls `SendAsyncFromGeneration(ctx, primary.originGen, dm)` instead of the unbound `SendAsync`.
A token rather than a `*connection` keeps a retained message from pinning the connection's handlers and epoch.

**SECS-I's plain ingress gets the stamp but not cutoff #1.**
`secs1`'s assembler is wired in `newTransport` (`t.newSink`) to `t.rt.DeliverOwnedFrame` —
the plain, gen-less entry point, never `DeliverOwnedFrameFromGeneration`.
`DeliverOwnedFrame` passes `c.cur.Load()` straight to `deliverOwnedFrameOn`:
no `liveEpoch` decision, no `staleRecv` accounting, no refusal path.
A stamped SECS-I message still gets a generation-bound reply,
but SECS-I's straggler protection lives in its line engine's per-generation ctx/bundle, not in this fence.

# Invariants

- Each call reaching a generation-aware inbound entry point with a nonzero `gen`
  performs exactly one `liveEpoch` admission check, under `genGate.RLock`.
  Decode, origin stamping, S9 correlation, reply routing, and fan-out all reuse that resolved epoch.
  Separate commit and disconnect operations (`selectCommitGate`, `injectDisconnect`) perform their own current-generation checks.
- The fan-out never takes `genGate`;
  it takes only `s.mu.RLock` to snapshot registrations,
  and selects on the already-resolved `e.ctx.Done()`, rechecked before each step.
- `lastRecvStamp` is written on every complete frame read (in `readObserved`), before dispatch and therefore before cutoff #1 —
  a frame cutoff #1 goes on to refuse still counts as proof of link liveness.
- `RouteReplyFromGeneration` reports `true` (not `false`) for a stale generation,
  so a stale Select.rsp lands on `dispatchFrame`'s HIT branch, not the Reject-sending MISS branch.
  On HIT, `commitSelected(g.gen)` → `CommitSelectedFromGeneration` → `supervisor.commitFrom` → `selectCommitGate` refuses the stale commit.
- SECS-I's inbound ingress never reaches `liveEpoch`.
  Its outbound reply is not exempt the same way: when a live generation admitted the primary,
  `ReplyDataMessage` binds the reply through `SendAsyncFromGeneration`,
  which resolves that nonzero `originGen` through `liveEpoch` like any other generation-named send.
- `connection.staleRecv` is incremented on every cutoff-#1 refusal (frame or reply)
  but is not part of `ConnectionMetrics`; it has no exported accessor.

# Failure modes

- Code expecting `staleRecv` through `ConnectionMetrics`
  (by analogy with `DataMsgRecvCount` or `ReplyMismatchCount`) finds no such method.
  The Debug log's `reported_generation`/`current_generation` fields identify the refusal,
  but other receive-side accounting can already have run:
  a stale Reject.req is counted in `hsmsss` `RejectRecvCount` before `routeReply` is called.
- A new fan-out path that selects on `e.done` instead of `epochDone(e)` (`e.ctx.Done()`) creates a circular dependency:
  `e.done` cannot close until the bounded join completes,
  and that join is waiting on the very recv goroutine driving the fan-out —
  delivery stalls until the join's timeout fires, rather than stopping promptly on cancellation.
- Assuming SECS-I gets HSMS-SS's stale-generation refusal because both reach `deliverOwnedFrameOn` is wrong.

# Gotchas

- Two code comments disagree with the code.
  `AddDataMessageChan` (`hsms/session.go`) says channel delivery selects on `rt.Done()`;
  `recvDataMsgOn` actually selects on the `done` it is passed, the resolved epoch's `e.ctx.Done()`.
  The `DataMessage` origin-field comment (`hsms/data_msg.go`) says the fields record an inbound PRIMARY;
  `deliverOwnedFrameOn` stamps every inbound data message with a non-nil epoch, secondaries included.

# Where to look

- the admission entry points and the resolved-epoch propagation: `hsms/connection_runtime.go` →
  `(*connection).DeliverOwnedFrame`, `(*connection).DeliverOwnedFrameFromGeneration`, `(*connection).deliverOwnedFrameOn`,
  `(*connection).RouteReplyFromGeneration`, `(*connection).routeReplyOn`, `(*connection).checkSessionID`, `(*connection).correlateS9On`
- the one-shot admission gate and the separate commit/disconnect checks: `hsms/connection_lifecycle.go` →
  `(*connection).liveEpoch`, `(*connection).selectCommitGate`, `(*connection).injectDisconnect`
- the stale Select.rsp commit refusal: `hsmsss/transport_control.go` → `(*transport).commitSelected`; `hsms/supervisor.go` → `(*supervisor).commitFrom`
- the per-step recheck during fan-out: `hsms/session.go` → `(*session).recvDataMsgOn`, `(*session).dispatchDecodeErrorOn`
- the generation carried into the handler call: `hsms/connection_runtime.go` → `(*connection).routeDataOn`;
  `hsms/session.go` → `(*session).callDataHandler`, `(*session).callDecodeErrorHandler`;
  `hsms/handler_panic.go` → `(*connection).disconnectHandlerGeneration`
- why the fan-out watches `ctx.Done()` and not the join-complete signal: `hsms/connection.go` →
  `(*connection).Done`, `epochDone`; `hsms/epoch.go` → `(*epoch).join`
- the reply-side origin token: `hsms/connection.go` → `connIdentity`, `(*connection).originIdentity`;
  `hsms/data_msg.go` → `DataMessage`
- the origin-token check that routes a reply through the same generation: `hsms/session.go` →
  `(*session).ReplyDataMessage`, `(*session).ReplyAbort`, `(*session).reply`, `originSender`;
  `hsms/connection_send.go` → `(*connection).SendAsyncFromGeneration`, `(*connection).enqueueAsync`
- the unexported stale counter: `hsms/connection.go` → `connection.staleRecv`;
  `hsms/connection_metrics.go` → `ConnectionMetrics`; `hsmsss/metrics.go` → `(*ConnectionMetrics).RejectRecvCount`
- the test-hook windows: `hsmsss/transport.go` → `transport.testHookAfterReadFrame`;
  `hsms/connection.go` → `connection.testHookBeforeFanout`, `connection.testHookBeforeS9F1Enqueue`
- hsmsss's generation-bound dispatch wrappers: `hsmsss/transport_control.go` →
  `(*transport).deliverOwnedFrame`, `(*transport).routeReply`, `genRuntime`
- the read-then-dispatch sequence, where the stamp and hook precede admission: `hsmsss/transport_recv.go` →
  `(*transport).recvLoop`, `(*transport).readObserved`, `(*transport).dispatchFrame`
- the shared capability method set: `internal/gencap/gencap.go` → `GenerationRuntime`
- SECS-I's unfenced plain-path ingress: `secs1/transport.go` → `newTransport`
- the observer report scope: `hsmsss/transport_socket.go` → `(*socketRecord).openReport`
