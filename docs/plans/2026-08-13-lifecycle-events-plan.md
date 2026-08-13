# Implementation plan: causal, cancellable lifecycle subscriptions (v2.4)

Today `StateChangeHandler func(prev, next ConnState)` carries no cause,
and registrations are permanent
("persist across Open/Close cycles and are never removed" — `hsms/endpoint.go`).
A consumer cannot distinguish a local `Close` from a peer `Separate` from a T6 expiry without correlating logs afterwards,
and any component shorter-lived than the connection leaks its handler.

## API

```go
// On Connection (not SECS2Endpoint — lifecycle is a connection concern):
SubscribeLifecycle(fn func(LifecycleEvent)) (cancel func())

type LifecycleEvent struct {
    Previous ConnState
    Current  ConnState
    Cause    TransitionCause
}

type TransitionCause uint8

const (
    CauseUnknown        TransitionCause = iota
    CauseLocalOpen                      // Open initiated the transition
    CauseLocalClose                     // Close initiated it
    CauseSelectAccepted                 // select handshake completed
    CauseSelectRejected                 // peer refused the select
    CausePeerSeparate                   // peer sent Separate.req
    CauseT6Timeout                      // control transaction timer expiry
    CauseT7Timeout                      // NOT-SELECTED dwell expiry
    CauseLinktestFail                   // linktest failure threshold reached
    CauseIOError                        // transport read/write failure
)
```

`Cause` is a **closed enumerated set**, not a retained error —
carrying arbitrary errors through the notifier would create a retention hazard on the notifier goroutine.
The exact constant list is finalized during the inventory step below;
`CauseUnknown` is the honest fallback, never a bug marker.

## Design

1. **Delivery rides the existing notifier goroutine**, exactly like `StateChangeHandler`
   (handlers run on the notifier, never inline on a protocol goroutine, panic-isolated by the existing supervisor guard).
   No new goroutine, no ordering divergence between the old and new surfaces.
2. **Cause is derived where the transition is injected.**
   The supervisor already receives distinct events per source
   (peer separate, T7 expiry, select-lost, TCP down with its error, local close),
   so the mapping is event-site → constant.
   The inventory step enumerates every injection site and assigns each a cause;
   any site that cannot name one gets `CauseUnknown` explicitly, with a comment.
3. **Cancellable storage is new and separate** from the legacy append-only handler slice:
   a CAS-maintained slice of `{id, fn}` entries on the connection
   (same atomic-pointer pattern as the existing handlers),
   with `cancel` removing by id.
   The legacy `AddConnStateChangeHandler` path is not touched.
4. Subscriptions persist across Open/Close cycles until canceled —
   same lifetime rule as the legacy handlers, but now escapable.
5. Placement on `Connection` is an interface addition, like `AddDataMessageChan`;
   fold both into the same CHANGELOG compatibility note.

## Risk and review

This touches supervisor event plumbing — connection-lifecycle territory.
Per the project's established practice,
substantial lifecycle changes go through the external review pipeline
(plan review before implementation, post-implementation review after).
Budget for that; do not fast-track this one.

The mitigation that keeps the change small:
causes are read at the injection sites and carried alongside the existing event values —
the FSM's states, transitions, and channel structure do not change.
If the inventory step reveals
that threading a cause through some path requires restructuring the supervisor's event type,
stop and reconsider scope before proceeding (a `CauseUnknown` for that path is cheaper than an FSM refactor).

## Steps

1. Inventory: read `hsms/supervisor.go`, `hsms/connection_lifecycle.go`,
   and every `inject`/`Commit*` call site;
   produce the transition-source → cause table in the PR description.
2. Implement the subscription storage + `cancel`.
3. Thread causes from the injection sites to the notifier.
4. Godoc on `SubscribeLifecycle` and each cause constant
   (public wording — SEMI timer names are fine, internal invariant codes are not).
5. Tests, lint, CHANGELOG.

## Tests

Use the loopback pair harness to provoke real transitions:

- Local close → `CauseLocalClose`; peer sends Separate → `CausePeerSeparate`;
  peer disappears (socket close) → `CauseIOError`; select success → `CauseSelectAccepted`.
- `cancel` is effective: no events after cancel, concurrent-safe under `-race`,
  idempotent (double cancel is a no-op).
- Subscription persists across Close → re-Open.
- Legacy `AddConnStateChangeHandler` observes the same transitions in the same order
  (no regression on the old surface).
- Cancel during delivery (from inside the callback) does not deadlock.

## Acceptance

- Transition-source table complete: every injection site assigned a cause on purpose.
- `make test-all` green, `-race` clean, lint clean.
- External review pipeline completed for the supervisor-touching commit.
