---
type: Mechanic
title: Where a TransitionCause is chosen, and why one transition can swallow another's cause
description: The full transition-source to cause map, why the cause is picked at the injection site rather than derived in the FSM, why the transports pass both the cause and their generation through a capability interface instead of TransportRuntime, how the generation match keeps a late transport goroutine from dropping its successor's link, why the three synchronous commits need a lock-fenced gate instead of that match, and the ways a cause never reaches a subscriber.
tags: [hsms, lifecycle, supervisor, fsm, observability]
status: draft
generated: {by: "claude/opus-5", at: 2026-08-13T00:00:00Z}
verified:
  - {by: "claude/opus-5", at: 2026-08-13T00:00:00Z}
  - {by: "claude/opus-5", at: 2026-08-13T13:00:00Z}
  - {by: "claude/sonnet-5", at: 2026-08-13T23:00:00Z}
sources:
  - {resource: hsms/lifecycle.go, digest: sha256:65d429d90300b620, revision: 5a0ec1b}
  - {resource: hsms/supervisor.go, digest: sha256:6e184e41a0ff36b7, revision: 6ef4ce7}
  - {resource: hsms/connection_lifecycle.go, digest: sha256:ef39ad01716f99ef, revision: 71a7afe}
  - {resource: hsms/connection_runtime.go, digest: sha256:711c3a7e01179640, revision: 6ef4ce7}
  - {resource: hsms/connection.go, digest: sha256:77816cf32d7065e9, revision: 6ef4ce7}
  - {resource: hsms/epoch.go, digest: sha256:2bb2df9485d57850, revision: 6ef4ce7}
  - {resource: hsmsss/transport.go, digest: sha256:af794dd9a6818352, revision: 71a7afe}
  - {resource: hsmsss/transport_control.go, digest: sha256:2feb21fb4247bb55, revision: 71a7afe}
  - {resource: hsmsss/transport_active.go, digest: sha256:a9f72c23b1c5827c, revision: 71a7afe}
  - {resource: hsmsss/transport_passive.go, digest: sha256:f5c2688db6585682, revision: 71a7afe}
  - {resource: hsmsss/transport_recv.go, digest: sha256:466ba864e5586a7a, revision: 6ef4ce7}
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
`fsmCommand{ev, cause}` is what `inject` queues and `step` dequeues;
`stateChange` carries the cause to the notifier.
The transition table (`transition`) never reads it.
The same state pair therefore carries different causes on different runs:
`Selected -> NotConnected` is a local Close, a peer Separate, a linktest failure, or a read error.
That is exactly the discrimination the feature exists to provide, and the reason it cannot be reconstructed after the fact from `(prev, next)`.

**Every injection site, and the cause it names.**

| Injection site | Event | Cause |
|---|---|---|
| `connection.TCPUp` → `supervisor.CommitConnected` | `evTCPUp` | `CauseLocalOpen` |
| `connection.CommitSelected` → `supervisor.CommitSelected` | `evSelectAccepted` | `CauseSelectAccepted` |
| `connection.SelectLost` → `supervisor.CommitSelectLost` | `evSelectLost` | `CausePeerDeselect` |
| (the three above, generation-named: `*FromGeneration` → `supervisor.commitFrom` under `connection.commitGate`) | same | same |
| `connection.T7Expired` | `evT7Timeout` | `CauseT7Timeout` |
| `connection.Close` → `requestClose` | `evClose` | `CauseLocalClose` |
| `connection.Open` rollback → `requestClose` | `evClose` | `CauseLocalClose` |
| `connection.writeFrame` write failure | `evDisconnect` | `CauseIOError` |
| `connection.TCPDown` (no cause named) | `evDisconnect` | `CauseUnknown` |
| `hsmsss.handleSeparateReq` | `evDisconnect` | `CausePeerSeparate` |
| `hsmsss.runSelectProcedure` failed transaction | `evDisconnect` | `selectFailureCause`: `CauseSelectRejected` on a `*RejectError`, `CauseT6Timeout` on `ErrT6Timeout`, else `CauseIOError` |
| `hsmsss.runSelectProcedure` non-zero select-status, or a correlated non-Select.rsp | `evDisconnect` | `CauseSelectRejected` |
| `hsmsss.runLinktest` threshold reached | `evDisconnect` | `CauseLinktestFail` |
| `hsmsss.recvLoop` nil conn / read error | `evDisconnect` | `CauseIOError` |
| `secs1.lineEngine` read / EOT-write error | `evDisconnect` | `CauseIOError` |

**The Select procedure is the only site with a non-constant cause, and the reason is the reply registry.**
An active Select.req is an ordinary registered transaction, so THREE different things can come back through `WriteMessage`.
A peer Reject.req correlated to its System Bytes is delivered as a `*hsms.RejectError` (`connection.RouteReply`) — the peer answered and refused, and no read or write failed,
so classifying it as `CauseIOError` would report a link fault for a protocol answer.
A T6 expiry returns a bare `ErrT6Timeout` (`connection.writeFrame`'s reply wait), meaning the peer never answered at all.
Everything else is the transport failing.
`selectFailureCause` splits those three; a correlated response of the wrong TYPE (a Linktest.rsp, a Deselect.rsp) is handled separately on the success path and also reports `CauseSelectRejected`,
because the peer answered and the select was not granted — which is what `CauseSelectRejected` is defined to mean.
The distinction between "refused" and "answered with the wrong frame" survives in the error value (`errSelectRejected` vs `errSelectBadResponse`), not in the cause.

`CommitSelected` names `CauseSelectAccepted` for `secs1` too, which runs no select handshake:
the auto-commit reaches `Selected` for the same reason a handshake does, so the cause is honest rather than approximate.

**`CauseUnknown` on bare `TCPDown` is deliberate.**
`TransportRuntime.TCPDown(cause error)` carries an error for the farewell decision, not a classification.
Defaulting it to `CauseIOError` would mislabel every drop an out-of-module transport reports for a non-I/O reason.
Only the in-module transports name a cause, through `connection.TCPDownWithCause`.

**Every injection site also names the generation it speaks for, and that is a separate axis from the cause.**
A transport goroutine can outlive the generation that spawned it:
the teardown join is bounded,
so one wedged past the close timeout is abandoned and may resume after a reconnect has established and selected a new link.
Because the plain `TCPDown` entry points resolve `c.cur` and `c.sup` at call time, such a goroutine used to drop the SUCCESSOR and report ITS cause to persistent subscribers.
Every ctx check in `hsmsss` narrows that window and none of them closes it —
`recvLoop`'s read-error branch, `handleSeparateReq`, the Select and linktest procedures all share the shape,
and cancellation can always land between the check and the call.

The identity is `epoch.id`, minted from the connection-scoped `genSeq` before the epoch is published on `cur` and never reset per Open cycle.
`hsmsss` reads it once per `Start` (`CurrentGeneration`) and stamps it on that generation's `genWG`,
which already reaches every goroutine it spawns;
each producer passes `g.gen` back through `TCPDownFromGeneration` / `T7ExpiredFromGeneration`.
It is checked in TWO places, and both are load-bearing:

- `connection.injectDisconnect` drops a report whose generation has ended.
  This is what keeps a straggler from setting the SUCCESSOR's `commsFailure`,
  which would suppress that generation's courtesy farewell Separate on a later graceful Close.
- `fsmCommand.gen` carries the identity onto the queue,
  and `step` re-checks it AFTER `state.Load()` and BEFORE the transition.
  The ordering is the whole barrier:
  a successor's state can only be committed after that successor was published to `cur`,
  so reading such a state guarantees the `cur` read that follows observes the successor and discards the event.
  Reversing the two reverts it to a narrowed window.

The match cannot suppress a legitimate disconnect, and the reason is an invariant elsewhere:
`cur` is published in exactly two places (`Open`, `connectLoop`) and the loop only runs from the entering-NotConnected reaction,
so `cur` advances only after the previous generation ended.
A mismatch therefore means the reporting generation's link was already torn down.
`s.curGen` must stay a bare atomic load.
`step` runs on the FSM goroutine, which an epoch teardown join can be waiting behind,
so a lock the teardown path holds would close a cycle:
`Stop` holds `startGate`, and `connectLoop` wants `startGate` in `ArmStart` while holding `publishMu`.

`secs1` does NOT name a generation — its line engine runs under a derived ctx and its own bundle —
so its reports keep the pre-barrier behavior.
Out-of-module transports likewise pass no identity: a `gen` of 0 skips the match everywhere.

**The three SYNCHRONOUS commits are not events, so `step`'s match does not cover them, and they need a lock.**
`CommitConnected`, `CommitSelected`, and `CommitSelectLost` compare-and-swap `supervisor.state` directly on the
transport's own goroutine and only THEN enqueue their event.
That synchrony is the §7.D invariant — `IsSelected()` must be true before the responder writes `Select.rsp` —
so it cannot be moved onto the queue.
It also means there is no later point at which the generation answer can be re-taken:
whatever check the caller makes, the CAS follows it on the same goroutine.

The FSM state cannot supply the missing ordering either, which is why the trick `step` uses does not transfer.
`step` reads state BEFORE it checks the generation, and a successor's state implies a successor was published.
A commit's "state read" is fused into its CAS, so the check can only come first —
and `NotConnected -> NotSelected -> ... -> NotConnected` is a real cycle,
so the state a stale CAS expects can legitimately reappear underneath it (plain ABA).

The fence is `connection.genGate`, a `sync.RWMutex`:
`connection.commitGate` takes RLock across {resolve `cur`, verify the identity and that `epoch.ended` is false, CAS},
and `epoch.markEnded` takes Lock for a single atomic store at the top of `epoch.teardown`.
The two are therefore mutually exclusive, and every successor publish is downstream of the join teardown starts,
so a commit that observed its generation un-ended completed its CAS before any successor could exist.
The gate is held across atomic operations and `epoch.connMu` only — never a log call, a channel send, or a Wait —
so it cannot close a cycle against the bounded teardown join.
`supervisor.commitFrom` issues the follow-up `injectFrom` AFTER the gate is released, because `inject` can block on a full queue.

`epoch.ended` is a SEPARATE axis from `epoch.id`, and TCP-up is where the difference shows.
`CommitConnected` CASes out of `NotConnected`, the state a generation that ended leaves behind,
and `cur` keeps pointing at a dead generation for the whole reconnect backoff — after a `Close`, forever.
So the identity of a straggler's TCP-up still MATCHES, and only `ended` rejects it.
The other two commits CAS out of states a dead generation cannot be in, so for them `ended` is redundant but harmless.

`connection.publishSocket` takes the same gate for the same reason:
an abandoned accept goroutine must not hang its socket on an epoch whose teardown already closed its own.
It resolves the epoch by identity and writes to THAT epoch, so a swap can never redirect it to a successor.

**The refusal is not silent: it is a reported bool, all the way out to the transport.**
`publishSocket`, `commitTCPUp`, and `TCPUpFromGeneration` all return whether the socket was accepted.
This is the one producer among the five `genCapability` methods whose caller has actual cleanup to do on a
refusal — the socket itself, not just an event.
`hsmsss`'s `tcpUp` wrapper forwards that bool, and `startActive` / `acceptLoop`
(transport_active.go / transport_passive.go) check it:
on `false` they close the conn themselves —
no epoch will ever own a refused socket, so nobody else ever will —
and skip everything a live TCP-up would start
(the recv loop, the active Select procedure, the transport's own `t.conn` / activity-stamp bookkeeping),
so a dead generation's socket can never clobber a live successor's.
`commitTCPUp` still runs the FSM commit unconditionally even on a `publishSocket` refusal, purely so
`staleGen` keeps counting it — the commit itself is a guaranteed no-op there, since `ended` never reverts.
The plain `TCPUp` (gen 0, out-of-module) keeps its void signature and unconditional-accept behavior;
only the generation-named path can name a refusal.

**How the cause crosses the package boundary.**
`hsmsss` and `secs1` reach `TCPDownWithCause` by type-asserting `t.rt` to a package-local `causeRuntime` interface, then fall back to plain `TCPDown`.
`hsmsss` additionally asserts a `genRuntime` interface carrying the three generation-aware methods,
and prefers it when present.
This is the same pattern `suppressionRuntime` uses for `LinktestSuppression`,
and for the same documented reason (`hsms/connection.go`):
widening the exported `TransportRuntime` would break external implementers.
A mock runtime that implements only `TransportRuntime` — every mock in the suite — therefore reports `CauseUnknown`,
which is why the hsms-package tests drive causes through the concrete `*connection` rather than a mock.

# Failure mode

**A cause silently lost to dedup.**
`step` fires only when `next != s.lastReacted`.
An `evDisconnect(CauseIOError)` that has already driven `Selected -> NotConnected` leaves `lastReacted == NotConnected`;
a `Close` behind it transitions `NotConnected -> NotConnected`, does not fire,
and its `CauseLocalClose` is never reported.
Observed from outside: a subscriber sees the drop's cause and no close event at all.
This is correct — the FSM really did make one transition —
but a consumer that treats the newest event as "why the link is down right now" reads the right answer only because the FIRST cause is the true one.

**A cause lost to coalescing.**
`emit` is a non-blocking drop-OLDEST send.
Under a subscriber that does not drain, an intermediate `stateChange` is discarded with its cause.
The latest state always survives; an intermediate cause may not.

**The bring-up pair collapsing into one event.**
`CommitConnected` CAS-stores `NotSelected` and then enqueues `evTCPUp`.
If the select handshake commits before `run()` drains that `evTCPUp`,
`step` finds the state already `Selected`, `transition(Selected, evTCPUp)` is illegal, and nothing fires;
the subsequent `evSelectAccepted` then reports `NotConnected -> Selected` with `CauseSelectAccepted`.
A bring-up therefore legitimately reports either two events or one.
A test that asserts an exact three-event Open/Select/Close sequence is flaky by construction;
see `hsmsss.causeLog.waitBringUp`, which tolerates both shapes.

# Where to look

- causes and subscription storage: `hsms/lifecycle.go` →
  `TransitionCause`, `LifecycleEvent`, `connection.SubscribeLifecycle`, `connection.cancelLifecycle`
- event plumbing: `hsms/supervisor.go` → `fsmCommand`, `inject`, `injectFrom`, `step`, `fireTransition`, `notifySubs`
- cause-carrying disconnect: `hsms/connection_lifecycle.go` → `TCPDownWithCause`, `TCPDownFromGeneration`, `injectDisconnect`
- generation identity: `hsms/epoch.go` → `epoch.id`;
  `hsms/connection.go` → `genSeq`;
  `hsms/connection_lifecycle.go` → `CurrentGeneration` and the two `cur.Store` sites
- the barrier itself: `hsms/supervisor.go` → `step`'s generation match, `curGen`, `staleGen`
- the synchronous-commit fence: `hsms/connection.go` → `genGate`;
  `hsms/connection_lifecycle.go` → `commitGate`, `publishSocket`, `commitTCPUp`, `genCapability`;
  `hsms/epoch.go` → `epoch.ended`, `epoch.markEnded`;
  `hsms/supervisor.go` → `commitFrom` and the three `Commit*FromGeneration` methods;
  `hsms/connection_runtime.go` → `commitSelectAccepted`, `commitSelectLost`
- transport capability: `hsmsss/transport_control.go` → `causeRuntime`, `genRuntime`,
  `transport.tcpDown`, `transport.t7Expired`, `transport.tcpUp`, `transport.commitSelected`, `transport.selectLost`;
  `secs1/transport.go` → `causeRuntime` only
- the generation token's carrier: `hsmsss/transport.go` → `genWG.gen`, stamped in `startActive` / `startPassive`
