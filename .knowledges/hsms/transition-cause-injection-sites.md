---
type: Mechanic
title: Where a TransitionCause is chosen, and why one transition can swallow another's cause
description: The full transition-source to cause map, why the cause is picked at the injection site rather than derived in the FSM, how the cause crosses from a transport package into hsms, and the ways a cause never reaches a subscriber.
tags: [hsms, lifecycle, supervisor, fsm, observability]
status: stable
generated: {by: "claude/opus-5.5", at: 2026-09-26T10:19:32Z}
verified:
  - {by: "openai/gpt-5.6-terra", at: 2026-09-26T10:24:29Z}
sources:
  - {resource: hsms/lifecycle.go, digest: sha256:9c093383d23470e1, revision: 7ae1ff0}
  - {resource: hsms/session.go, digest: sha256:ce5c99a71ad4ff0b, revision: 7ae1ff0}
  - {resource: hsms/handler_panic.go, digest: sha256:7c995367269a8805, revision: 7ae1ff0}
  - {resource: hsms/supervisor.go, digest: sha256:097ae581f965d935, revision: b43b798}
  - {resource: hsms/connection_lifecycle.go, digest: sha256:299faa7fdfbdf7fa, revision: 2041f5d}
  - {resource: hsms/connection_runtime.go, digest: sha256:7340a3887598a8ba, revision: b43b798}
  - {resource: hsms/connection_send.go, digest: sha256:90a5e3ff7beea84c, revision: 7ae1ff0}
  - {resource: hsms/connection.go, digest: sha256:3c8a78070c937d5a, revision: 7ae1ff0}
  - {resource: hsmsss/transport.go, digest: sha256:176fff888fc8a85e, revision: 7ae1ff0}
  - {resource: hsmsss/transport_control.go, digest: sha256:84353e5b3b34860b, revision: 7ae1ff0}
  - {resource: hsmsss/transport_active.go, digest: sha256:33d63b7808dc1ed3, revision: 7ae1ff0}
  - {resource: hsmsss/transport_recv.go, digest: sha256:fc971b2806f82a52, revision: 7ae1ff0}
  - {resource: hsmsss/transport_procedures.go, digest: sha256:bf47bd9825ddb5da, revision: 7ae1ff0}
  - {resource: secs1/transport.go, digest: sha256:399009fc96b7bf6c, revision: 7ae1ff0}
  - {resource: hsmsss/transport_control_test.go, digest: sha256:d7d6115539bd3043, revision: 7ae1ff0}
  - {resource: hsmsss/integration_lifecycle_cause_test.go, digest: sha256:50bb158dce6382ea, revision: 7ae1ff0}
---

# What it does

`SubscribeLifecycle`'s godoc states the consumer contract:
callbacks run on the notifier goroutine, a subscription survives Open/Close, `cancel` is idempotent,
and `Cause` names the event that drove the transition.
It does not record WHERE each cause is picked, why the choice lives at the injection site rather than in the FSM,
or how a cause reaches `hsms` from a transport package that `hsms` must not depend on.
That is the delta this entry records.

# How it works

**The cause is data on the event, never a function of the state pair.**
`fsmCommand{ev, cause, gen}` is what `inject` queues and `step` dequeues;
`stateChange` carries the cause to the notifier, together with the `gen` and `socket` the transition belongs to
and the time `at` it fired.
The transition table (`transition`) never reads the cause.
`step` makes exactly one substitution:
an `evTCPUp` that finds the state already `Selected` reports `CauseSelectAccepted` instead of its own cause
(the coalesced bring-up, under Failure mode).
The same state pair therefore carries different causes on different runs:
`Selected -> NotConnected` is a local Close, a peer Separate, a linktest failure, or a read error.
That is exactly the discrimination the feature exists to provide, and the reason it cannot be reconstructed after the fact from `(prev, next)`.

**Every injection site, and the cause it names.**

| Injection site | Event | Cause |
|---|---|---|
| `connection.TCPUp` → `supervisor.CommitConnectedFromGeneration(0, …)` | `evTCPUp` | `CauseLocalOpen`; `step` reports `CauseSelectAccepted` instead when the Select commit already landed |
| `connection.CommitSelected` → `supervisor.CommitSelectedFromGeneration(0, …)` | `evSelectAccepted` | `CauseSelectAccepted` |
| `connection.SelectLost` → `supervisor.CommitSelectLostFromGeneration(0, …)` | `evSelectLost` | `CausePeerDeselect` |
| (the three above, generation-named: `*FromGeneration` → `supervisor.commitFrom`; `CommitConnected` under `connection.tcpUpCommitGate`, `CommitSelected` under `connection.selectCommitGate`, `CommitSelectLost` under `connection.commitGate`) | same | same |
| `connection.T7Expired` | `evT7Timeout` | `CauseT7Timeout` |
| `connection.Close` → `requestClose` | `evClose` | `CauseLocalClose` |
| `connection.Open` rollback → `requestClose` | `evClose` | `CauseLocalClose` |
| `connection.writeFrame` write failure | `evDisconnect` | `CauseIOError` |
| `connection.TCPDown` (no cause named) | `evDisconnect` | `CauseUnknown` |
| `connection.connectLoopStartFailure` hand-off, after a reconnect `Start` failed on a generation whose TCP-up committed → `injectDisconnect` | `evDisconnect` | `CauseUnknown` |
| `hsmsss.handleSeparateReq` | `evDisconnect` | `CausePeerSeparate` |
| `hsmsss.runSelectProcedure` failed transaction | `evDisconnect` | `selectFailureCause`: `CauseSelectRejected` on a `*RejectError`, `CauseT6Timeout` on `ErrT6Timeout`, else `CauseIOError` |
| `hsmsss.runSelectProcedure` a genuine select-status refusal (see below), or a correlated non-Select.rsp | `evDisconnect` | `CauseSelectRejected` |
| `hsmsss.runLinktest` threshold reached | `evDisconnect` | `CauseLinktestFail` |
| `hsmsss.recvLoop` read error | `evDisconnect` | `CauseIOError` |
| `secs1.lineEngine` read / EOT-write error | `evDisconnect` | `CauseIOError` |
| `session.callDataHandler` / `callDecodeErrorHandler` / `connection.callAsyncSendErrorHandler` Goexit → `connection.disconnectHandlerGeneration` | `evDisconnect` | `CauseHandlerExit` |

**`CauseHandlerExit` identifies callback-induced termination, using the generation supplied to that callback.**
`CauseHandlerExit` reports on the generation a USER CALLBACK was running for, when that callback
ends via `runtime.Goexit` instead of returning — `DataMessageHandler`/`DecodeErrorHandler`
(`session.callDataHandler`/`callDecodeErrorHandler`, run on the recv goroutine) or the async send error
handler (`connection.callAsyncSendErrorHandler`, run on the per-generation async-sender goroutine).
All three route through `hsms/handler_panic.go`'s `disconnectHandlerGeneration`, which calls
`c.TCPDownFromGeneration(gen, errHandlerGoexit, CauseHandlerExit)` — the SAME generation-named entry
point `hsmsss` uses for its own disconnects, though `secs1` does not (see below).
`disconnectHandlerGeneration` separately rejects `gen == 0` itself, before ever calling
`TCPDownFromGeneration`.
A nonzero, mismatched `gen` is instead caught by `injectDisconnect`/`step`'s own generation-identity
match, which compares IDENTITY only, not `ended`: an ended-but-still-current
generation's report still passes it
(see [the generation report fence](/hsms/generation-report-fence.md)).
`gen` is always the generation `routeDataOn`/`recvDataMsgOn`/`dispatchDecodeErrorOn` (or
`drainSendCh`) resolved BEFORE calling the handler, threaded straight through rather than re-resolved
in the defer that detects the Goexit — see [the callback panic/Goexit isolation
entry](/hsms/handler-panic-goexit-isolation.md) for why re-resolving there would name the wrong
generation.
`secs1` does not name a generation for its OWN disconnects (row above, `secs1.lineEngine`) — but a
`secs1`-hosted `DataMessageHandler`/`DecodeErrorHandler` still resolves a real, nonzero `gen`: plain
`DeliverOwnedFrame` loads `c.cur.Load()` unconditionally and `routeDataOn` computes `gen` from that
epoch's own `id` (0 only when `cur` was nil).
So a `secs1` handler's Goexit CAN drive a `CauseHandlerExit` disconnect naming the connection's current generation,
despite `secs1` having no generation-aware transport capability of its own.
SECS-I's own transport disconnect reports are unqualified;
shared-core write failures (`connection.writeFrame`, which every `secs1` write goes through)
and callback Goexit use generation-named disconnects.

**The Select procedure is the only site with a non-constant cause, and the reason is the reply registry.**
An active Select.req is an ordinary registered transaction, so THREE different things can come back through `WriteMessage`.
A peer Reject.req correlated to its System Bytes is delivered as a `*hsms.RejectError` (`connection.RouteReply`) — the peer answered and refused, and no read or write failed,
so classifying it as `CauseIOError` would report a link fault for a protocol answer.
A T6 expiry returns a bare `ErrT6Timeout` from `connection.sendWaitReplyOn`'s control-transaction reply wait —
`writeFrame` performs only the synchronous write and has no reply wait of its own —
meaning the peer never answered at all.
Everything else is the transport failing.
`selectFailureCause` splits those three; a correlated response of the wrong TYPE (a Linktest.rsp, a Deselect.rsp) is handled separately on the success path and also reports `CauseSelectRejected`,
because the peer answered and the select was not granted — which is what `CauseSelectRejected` is defined to mean.
The distinction between "refused" and "answered with the wrong frame" survives in the error value (`errSelectRejected` vs `errSelectBadResponse`), not in the cause.

A non-zero select-status only reaches this cause when it is a genuine refusal.
Status 2+ always is one;
status 1 (`SelectStatusAlreadyActive`) is one only when this generation's `genWG.selectedOnce` latch still reads false.
A status 1 answer while that latch reads true is the legitimate simultaneous-select case instead,
and drives no transition at all.

`CommitSelected` names `CauseSelectAccepted` for `secs1` too, which runs no select handshake:
the auto-commit reaches `Selected` for the same reason a handshake does, so the cause is honest rather than approximate.

**`CauseUnknown` on bare `TCPDown` is deliberate.**
`TransportRuntime.TCPDown(cause error)` carries an error for the farewell decision, not a classification.
Defaulting it to `CauseIOError` would mislabel every drop a transport using only `TransportRuntime` reports for a non-I/O reason.
The shipped transports name a cause through the optional `TCPDownWithCause` capability, reached by type assertion;
a runtime reached without that capability reports `CauseUnknown`.

**Where a report names the generation it speaks for, that is a separate axis from its cause.**
`hsmsss` names one on each of its own reports when the runtime offers the generation capability,
and `secs1`'s own reports name none;
the cause says WHY a transition happened,
the generation says WHICH link it is allowed to happen to,
so a transport goroutine that outlives its generation cannot drop a successor and report its cause to persistent subscribers.
The generation is also reported now, not only fenced:
when an event drives a transition, `step` snapshots the transition's identity with `transitionIdentity`
and the time it fired with `time.Now()`,
and hands `fireTransition` a whole `stateChange`,
and `notifySubs` copies its `gen`, `socket` and `at` into `LifecycleEvent.Generation`, `LifecycleEvent.Socket` and `LifecycleEvent.At`.
The time is taken there, on the supervisor goroutine, and never on the notifier,
whose delivery trails the transition by as long as a slow handler or subscriber holds it;
the coalesced bring-up is stamped at that same site, when it fires.
A Close reports the epoch `requestClose` pinned (`closeEpoch`), not whichever epoch is current;
any other event reports the `fsmCommand.gen` it carries,
and reads the socket from the current epoch (`curEpoch`) only when that epoch's id matches it.
A generation that never acquired a socket reports socket 0,
and an event carrying generation 0, which only a supervisor built without a connection queues, reports 0 for both.
How that identity is carried, bound, and checked — on the queue, at the three synchronous commits, and on the wire —
is recorded in [how a report from an ended generation is kept off its successor](/hsms/generation-report-fence.md).

**How the cause crosses the package boundary.**
`hsmsss` and `secs1` reach `TCPDownWithCause` by type-asserting `t.rt` to a package-local `causeRuntime` interface, then fall back to plain `TCPDown`.
`hsmsss` additionally asserts a generation-aware `genRuntime` interface and prefers it when present
(see the generation report fence for what it carries).
This is the same pattern `suppressionRuntime` uses for `LinktestSuppression`,
and for the same documented reason (`hsms/connection.go`):
widening the exported `TransportRuntime` would break external implementers.
A mock runtime that implements only `TransportRuntime` reports `CauseUnknown`.
Not every mock in the suite is one of those, though:
`hsmsss/transport_control_test.go`'s `genRecRT` additionally implements the generation capability,
specifically to exercise generation-aware transport behavior in tests,
so the hsms-package tests' reliance on the concrete `*connection` (rather than a mock) is about
exercising the REAL `causeRuntime`/`TCPDownWithCause` wiring, not about no test mock offering `genRuntime` at all.

# Failure modes

**A cause silently lost to dedup.**
`step` fires when `next != s.lastReacted`, or when the event really drops the link:
it stores `NotConnected` over a live state (`left != NotConnected`, where `left` is what the `Swap` replaced).
An `evDisconnect(CauseIOError)` that has already driven and reported `Selected -> NotConnected` leaves `lastReacted == NotConnected`;
a `Close` behind it transitions `NotConnected -> NotConnected`, drops nothing, does not fire,
and its `CauseLocalClose` is never reported.
Observed from outside: a subscriber sees the drop's cause and no close event at all.
This is correct — the FSM really did make one transition —
but a consumer that treats the newest event as "why the link is down right now" reads the right answer only because the FIRST cause is the true one.

The swallow needs the earlier drop to have been processed first.
`lastReacted` can lag the stored state,
because the synchronous commits CAS the state before their report reaches the queue,
so it can read `NotConnected` while the link is actually `NotSelected` or `Selected`.
Dedup on `lastReacted` alone would absorb the drop that ends such an unreported bring-up.
The drop exception makes that drop fire anyway, with `prev` set to the replaced state rather than `lastReacted`;
a `Close` that overtakes both bring-up reports fires `Selected -> NotConnected` with `CauseLocalClose`.
The reaction depends on that `prev` too, and the SubscribeLifecycle godoc does not say how:
`connection.react` sends the courtesy Separate only when `prev == Selected`,
starts the reconnect loop unless the connection is shutting down, and initiates teardown.
Without the exception, an absorbed involuntary disconnect would lose the reaction-driven teardown and reconnect,
not just the notification, leaving a link down with nothing driving it back up.
An absorbed Close would lose the courtesy farewell and the notification,
but its explicit teardown (`step`'s `evClose` branch, via `closeEpoch`) still runs,
and it never reconnects anyway.

**A cause lost to coalescing.**
`emit` is a non-blocking drop-OLDEST send.
Under a subscriber that does not drain, an intermediate `stateChange` is discarded with its cause.
The latest state always survives; an intermediate cause may not.
The drop exception guarantees only the enqueue:
under a subscriber that stays behind across further reconnects, a drop's own notification can be the one discarded.

**The bring-up pair collapsing into one event.**
`CommitConnected` CAS-stores `NotSelected` and then enqueues `evTCPUp`.
If the select handshake commits before `run()` drains that `evTCPUp`,
`step` finds the state already `Selected`.
`transition(Selected, evTCPUp)` is a legal same-state entry, so nothing is stored,
but `Selected` differs from `lastReacted`,
so the `evTCPUp` itself fires `NotConnected -> Selected`, reported with the substituted `CauseSelectAccepted`.
The `evSelectAccepted` behind it then finds `Selected == lastReacted` and fires nothing.
A bring-up therefore legitimately reports either two events or one.
A test that asserts an exact three-event Open/Select/Close sequence is flaky by construction;
see `hsmsss.causeLog.waitBringUp`, which tolerates both shapes.

**A late TCP-up report overtaken by a drop.**
A disconnect can reach the queue between `CommitConnected`'s CAS and its `evTCPUp`.
The disconnect then fires `NotSelected -> NotConnected` under the drop exception above,
and the `evTCPUp` behind it finds `NotConnected`.
`transition(NotConnected, evTCPUp)` is illegal, so that report is ignored.
If it were legal, it would store `NotSelected` on a generation that has already dropped.
The successor's own `CommitConnected` CAS expects `NotConnected`, so it would then fail,
and the successor's TCP-up CAS and its report would be lost.
In that hypothetical, the successor's later Select commit could still succeed,
since `connection.commitTCPUp` returns socket acceptance regardless of the CAS result
and `CommitSelectedFromGeneration` would CAS out of the resurrected `NotSelected`.
As implemented, the late report is ignored and the state stays `NotConnected`, so nothing is resurrected.

# Where to look

- causes and subscription storage: `hsms/lifecycle.go` →
  `TransitionCause`, `LifecycleEvent`, `connection.SubscribeLifecycle`, `connection.cancelLifecycle`
- event plumbing: `hsms/supervisor.go` → `fsmCommand`, `stateChange`, `inject`, `injectFrom`, `step`, `fireTransition` (takes a `stateChange`), `notifySubs`
- the identity a transition reports: `hsms/supervisor.go` → `transitionIdentity`, `supervisor.curEpoch`, `supervisor.closeEpoch`
- dedup, the drop exception, and the late-TCP-up rejection: `hsms/supervisor.go` → `step`, `lastReacted`, `transition`;
  the reaction that reads the reported `prev`: `hsms/connection_lifecycle.go` → `(*connection).react`
- cause-carrying disconnect: `hsms/connection_lifecycle.go` → `TCPDownWithCause`, `TCPDownFromGeneration`, `injectDisconnect`;
  the unnamed T7 report and its report-time binding: `hsms/connection_runtime.go` → `T7Expired`, `injectT7Expiry`;
  the reconnect hand-off report: `connectLoopStartFailure`
- `CauseHandlerExit`, the callback-exit injection site, and the generation it names: `hsms/handler_panic.go` →
  `(*connection).disconnectHandlerGeneration`, `errHandlerGoexit`;
  `hsms/session.go` → `(*session).callDataHandler`, `(*session).callDecodeErrorHandler`;
  `hsms/connection_send.go` → `(*connection).callAsyncSendErrorHandler`;
  `hsms/connection_runtime.go` → `(*connection).routeDataOn`, `(*connection).DeliverOwnedFrame`
- generation identity and every fence on it: [the generation report fence](/hsms/generation-report-fence.md)
- transport capability: `hsmsss/transport_control.go` → `causeRuntime`, `genRuntime`,
  `transport.tcpDown`, `transport.t7Expired`, `transport.tcpUp`, `transport.commitSelected`, `transport.selectLost`;
  `secs1/transport.go` → `causeRuntime` only
- the simultaneous-select exemption for status 1: `hsmsss/transport.go` → `genWG.selectedOnce`;
  `hsmsss/transport_active.go` → `runSelectProcedure`;
  `hsmsss/transport_control.go` → `handleSelectReq`;
  `hsmsss/transport_recv.go` → `dispatchFrame`'s routed Select.rsp branch
- a test mock that DOES implement the generation capability: `hsmsss/transport_control_test.go` → `genRecRT`
- the bring-up-pair-tolerant wait helper: `hsmsss/integration_lifecycle_cause_test.go` →
  `(*causeLog).waitBringUp`
