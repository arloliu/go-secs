---
type: Mechanic
title: Where a TransitionCause is chosen, and why one transition can swallow another's cause
description: The full transition-source to cause map, why the cause is picked at the injection site rather than derived in the FSM, why the transports pass it through a capability interface instead of TransportRuntime, and the two ways a cause never reaches a subscriber.
tags: [hsms, lifecycle, supervisor, fsm, observability]
status: draft
generated: {by: "claude/opus-5", at: 2026-08-13T00:00:00Z}
sources:
  - {resource: hsms/lifecycle.go, digest: sha256:65d429d90300b620, revision: 5a0ec1b}
  - {resource: hsms/supervisor.go, digest: sha256:ce4e1df37ed32013, revision: 590fe16}
  - {resource: hsms/connection_lifecycle.go, digest: sha256:57d569ac8df3b2a0, revision: 590fe16}
  - {resource: hsms/connection_runtime.go, digest: sha256:85422b08b8884b47, revision: 590fe16}
  - {resource: hsmsss/transport_control.go, digest: sha256:8220b01d9d017d57, revision: 590fe16}
  - {resource: hsmsss/transport_active.go, digest: sha256:b67d832db547d0b4, revision: 5a0ec1b}
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

**How the cause crosses the package boundary.**
`hsmsss` and `secs1` reach `TCPDownWithCause` by type-asserting `t.rt` to a package-local `causeRuntime` interface, then fall back to plain `TCPDown`.
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
- event plumbing: `hsms/supervisor.go` → `fsmCommand`, `inject`, `step`, `fireTransition`, `notifySubs`
- cause-carrying disconnect: `hsms/connection_lifecycle.go` → `TCPDownWithCause`
- transport capability: `hsmsss/transport_control.go` → `causeRuntime`, `transport.tcpDown`;
  `secs1/transport.go` → the same pair
