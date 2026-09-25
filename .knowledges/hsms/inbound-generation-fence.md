---
type: Mechanic
title: The inbound generation fence
description: How an inbound frame or reply is bound to the generation whose recv goroutine actually read it, and the two different kinds of cutoff that enforce it.
tags: [hsms, hsmsss, generations, inbound, fan-out]
status: stable
generated: {by: "claude/sonnet-5", at: 2026-09-25T08:02:46Z}
verified:
  - {by: "openai/gpt-5.6-terra", at: 2026-09-25T08:34:25Z}
sources:
  - {resource: hsms/connection_runtime.go, digest: sha256:dce97e0bf7fc6616, revision: d244104}
  - {resource: hsms/connection_lifecycle.go, digest: sha256:45ceec38bea801db, revision: d244104}
  - {resource: hsms/connection.go, digest: sha256:29d7e54aeb61fce0, revision: a1cdb0e}
  - {resource: hsms/session.go, digest: sha256:134bcdad84cba21a, revision: d244104}
  - {resource: hsms/handler_panic.go, digest: sha256:7c995367269a8805, revision: d244104}
  - {resource: hsms/data_msg.go, digest: sha256:32c3295c07f631df, revision: 6c257b6}
  - {resource: hsmsss/transport_recv.go, digest: sha256:f54ea89029ef179c, revision: 6c257b6}
  - {resource: hsmsss/transport_control.go, digest: sha256:84353e5b3b34860b, revision: 6c257b6}
  - {resource: internal/gencap/gencap.go, digest: sha256:cf920df43a1fd5e6, revision: 6c257b6}
  - {resource: secs1/transport.go, digest: sha256:8c3142a7ada318ee, revision: d244104}
  - {resource: hsmsss/transport.go, digest: sha256:14cd2584fee0dbab, revision: 6c257b6}
---

# What it does

The godoc on the new entry points already covers almost everything a reader would ask first.
`DeliverOwnedFrameFromGeneration`/`RouteReplyFromGeneration` (`hsms/connection_runtime.go`) state the admission cutoff,
why a stale `RouteReplyFromGeneration` reports `true` rather than `false`,
and the `staleRecv` accounting.
`deliverOwnedFrameOn`/`checkSessionID` state the origin stamp and why the S9F1 enqueue is bound to the resolved epoch.
`connIdentity` and `DataMessage`'s origin fields (`hsms/connection.go`, `hsms/data_msg.go`) state why a token and not a connection pointer.
`Done`'s own doc (`hsms/connection.go`) states that no fan-out in this package selects on it any more.
`recvDataMsgOn`/`dispatchDecodeErrorOn` (`hsms/session.go`) state the observed-cancellation, recheck-per-handler contract.
What none of them lay out as one piece is the sequence:
admission and fan-out are two DIFFERENT kinds of cutoff, run one after the other,
with a window between them that three nil-by-default test hooks sit at;
that `lastRecvStamp` is written before either cutoff runs at all;
that SECS-I's plain ingress gets the origin stamp but not the admission cutoff;
and that there is no exported stale-admission-drop counter —
the Debug log identifies the refusal, but other receive-side accounting can already have occurred before it runs.
That is the delta this entry records.

# How it works

**The sequence, end to end.**
`hsmsss.recvLoop` reads a frame off the socket it was handed AT SPAWN —
never a re-read of `t.conn`; see [activity stamps](/hsmsss/activity-stamps.md) —
then does three things in order, before any generation-liveness decision has been made at all:
stores `lastRecvStamp`, fires `testHookAfterReadFrame` (nil in production), then calls `dispatchFrame`.
`dispatchFrame` routes a data frame to `t.deliverOwnedFrame(g.gen, frame)`
and a control response to `t.routeReply(g.gen, msg)` (`hsmsss/transport_control.go`),
both of which type-assert the runtime to `genRuntime` (`gencap.GenerationRuntime`) and,
when it is offered, call `DeliverOwnedFrameFromGeneration`/`RouteReplyFromGeneration` naming `g.gen` —
the generation THIS recv goroutine was spawned for, not whatever generation happens to be current when the call runs.

**Cutoff #1: admission, one-shot, behind a lock.**
`DeliverOwnedFrameFromGeneration`/`RouteReplyFromGeneration` resolve `gen` via `connection.liveEpoch(gen)` (`hsms/connection_lifecycle.go`),
which takes `genGate.RLock`, checks `cur.Load()` against `{id == gen, !ended}`, and returns either the live `*epoch` or `nil`.
This is a single yes/no decision made once, at one point in the call chain.
A refusal here is not a decode error and not visible to `hsmsss` at all:
the frame is dropped, `connection.staleRecv` is incremented,
and a `Debug`-level log line is emitted naming the reported and current generation —
`dispatchFrame` sees only that the call "completed"
(a dropped frame returns `nil` from the deliver path; a dropped reply reports `true`,
a deliberate choice so `dispatchFrame`'s HIT branch —
not its Reject-sending MISS branch — is the one that runs against a generation already known to be dead).

**Once admitted, one resolved epoch carries the rest.**
`deliverOwnedFrameOn(e, frame)` decodes, stamps `dm.originIdent`/`dm.originGen` from `e` (never from a fresh `c.cur.Load()`),
runs `checkSessionID(e, dm)` — which enqueues an S9F1 through `e` specifically, via `enqueueAsync`,
so a mismatch admitted by a generation that ends before the enqueue runs drops the S9F1 with the rest of that generation's stranded queue rather than landing on a successor —
fires `testHookBeforeFanout` (nil in production),
and then calls `routeReplyOn(e, dm)` / `routeDataOn(e, dm)`.
None of these steps re-resolves `c.cur`; they all thread the SAME `e` cutoff #1 already picked.

**Cutoff #2: fan-out, ongoing, no lock.**
`routeDataOn`/`dispatchDecodeErrorOn` (`hsms/connection_runtime.go`, `hsms/session.go`)
do not gate on `liveEpoch` again.
They compute `done := epochDone(e)` — `e.ctx.Done()`, never `e.done` —
and `recvDataMsgOn`/`dispatchDecodeErrorOn` recheck that channel with a non-blocking `select`
before EACH handler call and EACH channel send, not just once at entry.
This is a fundamentally different kind of cutoff from #1:
liveEpoch is a point-in-time decision made once behind `genGate`;
`e.ctx.Done()` is a channel a long-running fan-out can observe becoming ready partway through,
so fan-out stops when a per-step check observes cancellation;
a handler or channel delivery whose check already passed can still proceed, without ever re-taking a lock.
`Done()`'s own doc explains why the fan-out cannot use `e.done` instead:
`e.done` only closes after `tr.Stop`'s bounded join completes,
and that join waits (via the recv goroutine) for THIS SAME fan-out to return —
selecting on `e.done` here would be a circular wait.

**As of `d244104`, cutoff #2 also threads `e`'s own generation past the fan-out, into the handler call itself.**
`routeDataOn` derives `gen` from `e.id` (0 when `e` is nil) and passes it as a new leading parameter to
`recvDataMsgOn`/`dispatchDecodeErrorOn` (`hsms/session.go`), which pass it straight through to
`callDataHandler`/`callDecodeErrorHandler` — the same generation cutoff #1 already resolved, never
re-derived.
Those two call sites use `gen` for something outside this entry's own scope: if the handler they invoke
ends via `runtime.Goexit` instead of returning, they disconnect exactly `gen` — the admitted epoch's
own ID — rather than whatever generation happens to be current once the handler finally exits.
That disconnect report is a no-op only for the specific condition `disconnectHandlerGeneration`
checks: `gen == 0` is ignored outright, and a nonzero `gen` that no longer matches the current
epoch's identity is rejected by `injectDisconnect`.
This is a DIFFERENT condition from either cutoff above: it compares identity only, not `ended`, so a
report naming an ended-but-still-current epoch (the same epoch cutoff #1 admitted `e` under) still
gets through — see [the transition-cause injection-site entry](/hsms/transition-cause-injection-sites.md)
for that check's exact shape.
See [the callback panic/Goexit isolation entry](/hsms/handler-panic-goexit-isolation.md) for that
mechanism; the fact this entry records is only that the plumbing for it rides the SAME resolved `e`
cutoff #1 already picked, one parameter deeper than before.
A more consequential change rides along with it: before `d244104`, `callDataHandler` ran a
`DataMessageHandler` with NO recover at all — a panicking handler crashed the process.
It is now recovered (and counted) through the same `runCallback` isolation, so the "per-step recheck"
this section already documents (does the fan-out stop before the NEXT handler/channel) is no longer the
only thing standing between one misbehaving handler and the rest of the fan-out; a PANIC inside a single
handler no longer takes down the others either.

**The window between the two cutoffs, and where the test hooks sit.**
Three nil-by-default hooks mark the three points in this sequence a test can pause at:
`testHookAfterReadFrame` (`hsmsss/transport.go`, fired in `recvLoop`) sits BEFORE cutoff #1 —
between the read and `dispatchFrame`, i.e. before admission is even attempted;
`testHookBeforeS9F1Enqueue` (`hsms/connection.go`, fired in `checkSessionID`) sits AFTER cutoff #1,
between admission and the S9F1's `enqueueAsync` on the resolved epoch;
`testHookBeforeFanout` (`hsms/connection.go`, fired in `deliverOwnedFrameOn`) sits AFTER cutoff #1 and `checkSessionID`,
immediately before cutoff #2's fan-out begins.
Each one lets a test advance the generation (publish a successor, end the admitting one)
in exactly the window that hook marks,
to exercise what admission or fan-out does when the generation dies right there.

**The reply side reuses the SAME origin, through a token rather than a pointer.**
`ReplyDataMessage` (`hsms/session.go`) compares `primary.originIdent` against `os.originIdentity()` by pointer identity
(`*connIdentity`, a one-byte, non-zero-size struct — see its own doc for why zero-size would make two different connections' tokens alias)
and, on a match, calls `SendAsyncFromGeneration(ctx, primary.originGen, dm)` instead of the unbound `SendAsync`.
The token is a POINTER identity check, not a `*connection` or `*epoch` field:
a `DataMessage` that instead held a `*connection` would keep that connection's whole handler/channel registration and its current epoch reachable
for as long as an application retains the message —
the token exists for that retention reason as much as for correctness.

**SECS-I's plain ingress gets the stamp but not cutoff #1.**
`secs1`'s assembler is wired at construction to `t.rt.DeliverOwnedFrame` (`secs1/transport.go` → `newTransport`'s `t.newSink`) —
the plain, gen-less entry point, never `DeliverOwnedFrameFromGeneration`.
`DeliverOwnedFrame` resolves `c.cur.Load()` unconditionally and always proceeds to `deliverOwnedFrameOn`:
there is no `liveEpoch` admission decision, no `staleRecv` accounting, and no refusal path for SECS-I frames at all.
Plain ingress stamps the resolved epoch's identity only when that epoch is non-nil;
otherwise the message remains unstamped.
When it is stamped, `ReplyDataMessage`'s generation-bound reply still works for SECS-I —
but the protection cutoff #1 gives HSMS-SS (a straggler naming a DEAD generation gets refused before decode)
has no SECS-I counterpart in this fence;
SECS-I's own straggler protection is structural, in its line engine's own per-generation ctx/bundle, entirely outside this mechanism.

# Invariants

- Admission (cutoff #1) is decided exactly once per inbound frame/reply, under `genGate.RLock`, at `liveEpoch`.
  Nothing downstream re-decides it or re-reads `c.cur`.
- The fan-out (cutoff #2) never takes `genGate`;
  it only ever selects on the already-resolved `e.ctx.Done()`, rechecked before each step.
- `lastRecvStamp` is written on every complete frame read, before dispatch and therefore before cutoff #1 runs —
  a frame cutoff #1 goes on to refuse is still counted as proof of link liveness.
- `RouteReplyFromGeneration` reports `true` (not `false`) for a stale generation, deliberately,
  so a stale Select.rsp lands on `dispatchFrame`'s HIT branch (where `commitSelected` itself refuses it)
  rather than the MISS branch (which would attempt a pointless `Reject.req`).
- At this revision, SECS-I's INBOUND ingress bypasses admission checking entirely: `secs1`'s assembler
  is wired at construction directly to the plain `DeliverOwnedFrame` (`secs1/transport.go` →
  `newTransport`'s `t.newSink`), never to `DeliverOwnedFrameFromGeneration`, so it never reaches
  `liveEpoch` and never takes that function's own `if gen == 0` branch (which exists for OTHER
  gen-less callers that do go through the generation-aware entry point).
  A stamped SECS-I reply is not exempt the same way on the OUTBOUND leg: when a live generation
  admitted the primary, `ReplyDataMessage` binds the reply through `SendAsyncFromGeneration`, which
  resolves that nonzero `originGen` through `liveEpoch` like any other generation-named send.
- `connection.staleRecv` is incremented on every cutoff-#1 refusal (frame or reply)
  but is not part of `ConnectionMetrics` — it has no exported accessor.
  The Debug log line identifies the refusal,
  but other receive-side accounting (e.g. `RejectRecvCount` for a stale Reject.req) can already have run before the refusal.

# Failure modes

- Code that expects `staleRecv` to be readable through `ConnectionMetrics`
  (by analogy with `DataMsgRecvCount` or `ReplyMismatchCount`) will find no such method;
  there is no exported stale-admission-drop counter.
  The Debug log's `reported_generation`/`current_generation` fields identify the refusal,
  but a stale Reject.req is already counted in `RejectRecvCount` before that refusal runs.
- A new fan-out path that selects on `e.done` instead of `epochDone(e)` (`e.ctx.Done()`) creates a circular dependency:
  `e.done` cannot close until `tr.Stop`'s bounded join completes,
  and that join is waiting on the very recv goroutine driving the fan-out —
  delivery stalls until the join's own timeout fires, rather than stopping promptly on cancellation.
- Assuming SECS-I gets the same stale-generation protection as HSMS-SS
  because both go through `DeliverOwnedFrame`/`deliverOwnedFrameOn` is wrong:
  SECS-I's calls never pass through `DeliverOwnedFrameFromGeneration`, so they never hit `liveEpoch`'s refusal at all.

# Where to look

- the two admission cutoffs and the resolved-epoch propagation: `hsms/connection_runtime.go` →
  `(*connection).DeliverOwnedFrameFromGeneration`, `(*connection).deliverOwnedFrameOn`,
  `(*connection).RouteReplyFromGeneration`, `(*connection).routeReplyOn`, `(*connection).checkSessionID`
- the one-shot admission gate: `hsms/connection_lifecycle.go` → `(*connection).liveEpoch`
- the per-step recheck during fan-out: `hsms/session.go` → `(*session).recvDataMsgOn`, `(*session).dispatchDecodeErrorOn`
- the generation `gen` now carries into the handler call, and the panic isolation it feeds: `hsms/connection_runtime.go` →
  `(*connection).routeDataOn`; `hsms/session.go` → `(*session).callDataHandler`, `(*session).callDecodeErrorHandler`;
  `hsms/handler_panic.go` → `(*connection).disconnectHandlerGeneration`
- why the fan-out watches `ctx.Done()` and not the join-complete signal: `hsms/connection.go` →
  `(*connection).Done`, `epochDone`
- the reply-side origin token and its retention rationale: `hsms/connection.go` → `connIdentity`, `(*connection).originIdentity`;
  `hsms/data_msg.go` → `DataMessage.originIdent`, `DataMessage.originGen`
- the origin-token check that routes a reply through the SAME generation: `hsms/session.go` →
  `(*session).ReplyDataMessage`, `originSender`
- the three test-hook windows: `hsmsss/transport.go` → `transport.testHookAfterReadFrame`;
  `hsms/connection.go` → `connection.testHookBeforeFanout`, `connection.testHookBeforeS9F1Enqueue`
- hsmsss's generation-bound dispatch wrappers: `hsmsss/transport_control.go` →
  `(*transport).deliverOwnedFrame`, `(*transport).routeReply`, `genRuntime`
- the read-then-dispatch sequence, and where the stamp/hook precede admission: `hsmsss/transport_recv.go` →
  `(*transport).recvLoop`, `(*transport).dispatchFrame`
- the shared capability method set: `internal/gencap/gencap.go` → `GenerationRuntime`
- SECS-I's unfenced plain-path ingress: `secs1/transport.go` → `newTransport`'s `t.newSink` wiring to `t.rt.DeliverOwnedFrame`
