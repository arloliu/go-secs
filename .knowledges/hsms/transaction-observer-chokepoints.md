---
type: Mechanic
title: The transaction observer's two chokepoints, its isData gate, and its outcome classifier
description: Why WithTransactionObserver instruments two call sites (not one), why the isData gate prevents an observer-path panic for control messages, and how classifyTxOutcome's eight-way split relates to isCountedSendErr's binary one.
tags: [hsms, observability, send, metrics, lifecycle]
status: draft
generated: {by: "claude/opus-5.5", at: 2026-10-23T18:00:00Z}
sources:
  - {resource: hsms/connection_send.go, digest: sha256:ea94ab4fc9d25bb9, revision: 2a92a5e}
  - {resource: hsms/transaction_observer.go, digest: sha256:e76d57a56b7a83f0, revision: 2a92a5e}
  - {resource: hsms/connection_config.go, digest: sha256:ce0ac428789edddd, revision: 2a92a5e}
  - {resource: hsms/session.go, digest: sha256:04cc770917f02f59, revision: 2a92a5e}
  - {resource: hsms/connection_lifecycle.go, digest: sha256:84c70134ab8b14a4, revision: 2a92a5e}
  - {resource: hsmsss/transaction_observer_test.go, digest: sha256:498f47d6b3971437, revision: 2a92a5e}
  - {resource: hsmsss/transport_control.go, digest: sha256:ad8c57da5652a769, revision: 2a92a5e}
  - {resource: hsmsss/transport_active.go, digest: sha256:689f931cb195678f, revision: 2a92a5e}
  - {resource: hsmsss/transport_procedures.go, digest: sha256:8ef63578b72806e0, revision: 2a92a5e}
  - {resource: hsms/data_msg.go, digest: sha256:a90443b564284b51, revision: 2a92a5e}
  - {resource: hsms/epoch.go, digest: sha256:d6cc2283dd4d5e58, revision: 2a92a5e}
  - {resource: hsmsss/transport_recv.go, digest: sha256:f8d35637783f58a6, revision: 2a92a5e}
  - {resource: hsms/connection_runtime.go, digest: sha256:7f242f475b188a1b, revision: 2a92a5e}
  - {resource: hsms/handler_panic.go, digest: sha256:7c995367269a8805, revision: 2a92a5e}
  - {resource: hsms/connection.go, digest: sha256:42714e9581c0b14e, revision: 2a92a5e}
---

# What it does

`WithTransactionObserver`'s own godoc (`hsms/connection_config.go`) states the consumer contract —
which calls report an event,
that the hook runs synchronously on the caller's goroutine,
and that `ReplyDataMessage` / the async sends are excluded.
It does not say *why* the exclusion is structural rather than a scope choice,
nor that the `isData` gate guarding the classifier is load-bearing:
removing it can panic the observer path for a control message,
crashing the process only if unrecovered,
nor how the outcome classifier relates to the `isCountedSendErr` gate
([Send error accounting](/hsms/send-error-accounting.md)).
That is the delta this entry records.

# How it works

**Two chokepoints, not one.**
Synchronous data sends converge on two call sites:
`WriteMessage` (backs `SendDataMessage` / `SendSECS2Message`)
and `WriteMessageNoReply` (backs `ForwardDataMessage`).
With no observer installed, or for a control message,
each simply returns `sendWaitReply` / `sendNoReply`.
On the observed path each loads `c.cur` itself, reports `ErrNotOpen` when it is nil,
and otherwise runs `sendWaitReplyOn` / `sendNoReplyOn` on that pinned epoch,
so the `TxEvent` names the generation the send ran on and that epoch's socket (`epoch.socketID`),
even when the send returns after a reconnect.
`ReplyWaited` is `dm.WaitBit()` for `WriteMessage`
and always `false` for `WriteMessageNoReply`, regardless of the forwarded message's W-bit.

`ReplyDataMessage` is a third, structurally different case.
`session.reply` (`hsms/session.go`, shared with `ReplyAbort`) calls `rt.SendAsyncFromGeneration`
when the primary's origin token matches this connection's own identity,
and `rt.SendAsync` otherwise.
A matching token selects the generation-bound path even after that generation has ended;
`SendAsyncFromGeneration` then checks liveness through `liveEpoch`
and refuses a stale generation rather than falling back to unbound sending.
Both are the enqueue-only async primitive family that `SendDataMessageAsync` / `ForwardDataMessageAsync` use;
`SendAsyncFromGeneration` differs only in binding the enqueue to a caller-named generation
instead of resolving `c.cur` at call time.
Either way the frame is only guaranteed *enqueued*;
`drainSendCh` writes it later, on a different goroutine,
and a frame still queued at teardown is stranded, never flushed.
`ReplyDataMessage` therefore never reaches either chokepoint and never reports a `TxEvent`,
whichever enqueue path it takes.

**The `isData` gate, and the structural bypass in front of it.**
`hsmsss`'s control initiators do not call `rt.WriteMessage` directly.
`runSelectProcedure` (`hsmsss/transport_active.go`) and `runLinktest` (`hsmsss/transport_procedures.go`)
both call the `writeMessage` helper in `hsmsss/transport_control.go`,
which type-asserts the runtime to the `genRuntime` capability
and, when present, calls `WriteMessageFromGeneration` instead of `WriteMessage`.
The production `hsms.connection` implements `genRuntime`,
so for it a control request with a non-zero generation never reaches
`WriteMessage`, `WriteMessageNoReply`, `newTxEvent`, or the `isData` gate.
It goes straight to `sendWaitReplyOn` on the caller-named generation,
which reports no `TxEvent` by construction (see `WriteMessageFromGeneration`'s doc comment).
The `isData` gate inside `WriteMessage` / `WriteMessageNoReply`
(`if !isData { return c.sendWaitReply(ctx, msg) }` and its `WriteMessageNoReply` counterpart)
is the only thing protecting a caller that reaches `WriteMessage` with a `*ControlMessage` some other way —
a runtime that does not implement `genRuntime` (an external `TransportRuntime`),
or `WriteMessageFromGeneration` called with `gen == 0`, which falls through to `WriteMessage`.

Without the gate, a control message gives `dm == nil`,
and the failure is a panic, not "Stream=0/Function=0 rows in a consumer's histogram".
The two chokepoints panic at different points, neither inside the send itself.
`WriteMessage` evaluates `dm.WaitBit()` as `newTxEvent`'s call argument,
so a nil `dm` panics there, before `newTxEvent` is entered
(`(*DataMessage).WaitBit` dereferences `msg.header` with no nil check).
`WriteMessageNoReply` passes the literal `false`,
so a nil `dm` reaches `newTxEvent`'s body and panics at `dm.Stream()`,
the first field its `TxEvent{...}` literal evaluates.
Either way the panic fires during event construction:
after the epoch-pinned send body returns when an epoch exists,
or after `ErrNotOpen` is set, without calling either send body, when no epoch is current.
It propagates up the calling goroutine until an enclosing recovery catches it.
A send from a connection-backed data handler is covered by the library's `runCallback` recovery;
the process crashes only if the panic stays unrecovered.

**What the control-transaction test covers.**
`TestTransactionObserver_ControlTransaction_NoEvent` (`hsmsss/transaction_observer_test.go`)
asserts that `rec.snapshot()` stays empty across several completed auto-linktest round trips on both connections.
Those linktests run through the generation-bound bypass,
so the test verifies the combined exclusion,
not the `isData` gate independently.
Its leading doc comment describes both barriers correctly;
the inline comment inside the test body still claims it "exercises the gate"
on each connection's "own WriteMessage(Linktest.req) calls", which is inaccurate.

**The classifier vs. the counter.**
`classifyTxOutcome` (`hsms/connection_send.go`, next to `isCountedSendErr`)
maps `(replyWaited bool, err error)` to one of eight `TxOutcome` values.
`isCountedSendErr` is binary (counts toward `DataMsgErrCount` or not)
and excludes `ErrNotSelectedState`, `ErrConnClosed`, `ErrMessageTooLarge`,
and caller-context cancellation/deadline from the count —
see [Send error accounting](/hsms/send-error-accounting.md) for why.
`classifyTxOutcome` routes errors matching `ErrConnClosed` or caller cancellation to `TxCanceled`,
unless an earlier check matches first:
it tests `*RejectError` and `*S9Error` with `errors.As`,
then `ErrT3Timeout`, then `ErrTransactionAborted`,
and only then the lifecycle sentinels.
That order matters because `writeFrame` wraps a write error observed after teardown started
as `fmt.Errorf("%w: %w", ErrConnClosed, err)`;
if the raw error (from a custom transport) matches one of the earlier checks,
the outcome is that earlier bucket,
while `isCountedSendErr` still excludes it through `ErrConnClosed`.
`TxCanceled`'s own enum doc (`hsms/transaction_observer.go`) names both caller cancellation and connection closure,
so the doc and the classifier agree on that grouping.
`ErrNotSelectedState` and `ErrMessageTooLarge`, by contrast,
land in `TxSendError` under `classifyTxOutcome`
even though `isCountedSendErr` excludes them from the counter —
the two functions answer different questions
("does this count as link-health noise?" vs. "how did this transaction end?").
`TxAborted` (`ErrTransactionAborted`, with `WithAbortReplyError`)
and `TxPeerError` (an `*S9Error`, with `WithS9Correlation`)
are normally peer answers:
when either comes from a correlated peer reply, the reply branch bypasses `isCountedSendErr` and does not increment `DataMsgErrCount`.
A transport-write error matching either check produces the same outcome
and is counted unless it also matches a counter exclusion.

**Observer timing.**
The observer runs as a plain statement *after* `sendWaitReplyOn` / `sendNoReplyOn` fully returns,
not via a `defer` registered inside those functions.
The chokepoint only resolves the epoch they run on, as the plain `sendWaitReply` / `sendNoReply` wrappers do,
so it can read the event's generation and socket from that same epoch afterwards.
By the time the observer runs,
reply-registry deregistration, the W-bit inflight-gauge decrement, and timer-pool cleanup have completed,
so an observer panic (deliberately not recovered — see `WithTransactionObserver`'s godoc)
cannot bypass that cleanup or leak that internal state.
The panic keeps unwinding through the calling frames until something recovers it, or the process crashes.
The observer runs inline on whatever goroutine called the synchronous send,
including the receive loop when a data handler sends (see Gotchas).

# Invariants

- Every sync return path in `sendWaitReplyOn` / `sendNoReplyOn` reachable when `isData` is true,
  plus the chokepoint's own `ErrNotOpen` when no epoch is current, maps to exactly one `TxOutcome` —
  the full return-path table is `classifyTxOutcome`'s doc comment in `hsms/connection_send.go`.
  That table's transport-write rows omit classifier precedence:
  a raw or teardown-wrapped error matching the Reject, S9, T3, or abort checks receives that outcome before lifecycle classification.
- The `isData` gate must run before any field of `dm` is read.
  `newTxEvent` assumes a non-nil `dm`; nothing downstream nil-checks it.
- `ReplyDataMessage` and `ReplyAbort` share the async `session.reply` path and are not observed;
  routing them through `WriteMessage` / `WriteMessageNoReply` would change their blocking behavior and error accounting —
  see [Send error accounting](/hsms/send-error-accounting.md), which classifies `ReplyDataMessage` as an async path too.
- `sendNoReply` arms no T3 timer and registers no reply channel,
  so it never generates a `TxT3Timeout` or `TxRejected` outcome itself.
  But `classifyTxOutcome` uses `replyWaited` only for the `err == nil` split —
  a transport `Write` that itself returns `ErrT3Timeout` or a `*RejectError`
  produces the matching outcome through `WriteMessageNoReply` too.

# Failure modes

- **Deleting or weakening the `isData` gate** does not degrade gracefully into noisy data.
  Against the production `hsms.connection` it has no observable effect on in-module control traffic,
  because Select.req / Linktest.req go through `WriteMessageFromGeneration` and never reach the gate.
  With a non-nil `txObserver` installed,
  it panics during event construction, with or without a current epoch, when a caller reaches `WriteMessage` / `WriteMessageNoReply` with a `*ControlMessage` —
  a `genRuntime`-incapable `TransportRuntime`, or a `gen == 0` call —
  because a nil `dm` reaches an unconditional dereference:
  `dm.WaitBit()` at `WriteMessage`'s call site, or `dm.Stream()` inside `newTxEvent` for `WriteMessageNoReply`.
  With no observer installed, both functions return before reaching the gate, so that fast path is unaffected.
  A change that removes the gate can therefore look safe against the shipped transports
  and still be a live landmine for any caller that also installs an observer;
  the control-transaction test does not catch it alone.
- **Assuming `classifyTxOutcome` and `isCountedSendErr` agree on what "counts"**:
  they agree on ordinary teardown and caller-cancellation errors,
  but differ on `ErrNotSelectedState` and `ErrMessageTooLarge` —
  the counter excludes them, while the observer reports `TxSendError` —
  and on teardown-wrapped errors that match an earlier classifier check.
- **Rerouting `ReplyDataMessage` through a sync chokepoint** to make it observable
  would change its blocking behavior and error surface — a breaking API change, not an observability fix.

# Gotchas

- `TxEvent.ReplyWaited`'s field comment (`hsms/transaction_observer.go`) says
  "true when the message's W-bit asked the peer for a reply";
  `WriteMessageNoReply` reports `false` even for a forwarded W-bit message.
  `newTxEvent`'s doc comment states the actual rule.
- `WithTransactionObserver`'s godoc says the hook "never runs on a protocol goroutine".
  HSMS-SS dispatches inbound data synchronously from `recvLoop`, through `deliverOwnedFrameOn` and `routeDataOn`, into `recvDataMsgOn`,
  so a handler that calls `SendDataMessage` runs the observer on that receive goroutine.

# Where to look

- the two chokepoints: `hsms/connection_send.go` → `(*connection).WriteMessage`,
  `(*connection).WriteMessageNoReply`
- the classifier and the return-path table: `hsms/connection_send.go` → `classifyTxOutcome`;
  the counter it is compared with: `hsms/connection_send.go` → `isCountedSendErr`;
  the teardown wrapping: `hsms/connection_send.go` → `(*connection).writeFrame`
- event construction: `hsms/connection_send.go` → `newTxEvent`
- the epoch-pinned bodies the observed path runs: `hsms/connection_send.go` → `(*connection).sendWaitReplyOn`, `(*connection).sendNoReplyOn`;
  the socket identity an epoch keeps after teardown: `hsms/epoch.go` → `(*epoch).socketID`
- the excluded async path: `hsms/session.go` → `(*session).ReplyDataMessage`, `(*session).reply`;
  `hsms/connection_send.go` → `(*connection).SendAsyncFromGeneration`, `(*connection).drainSendCh`;
  `hsms/connection_lifecycle.go` → `(*connection).liveEpoch`
- the inline handler dispatch: `hsms/session.go` → `(*session).recvDataMsgOn`
- the option: `hsms/connection_config.go` → `WithTransactionObserver`
- the types: `hsms/transaction_observer.go` → `TxEvent`, `TxOutcome`
- the combined-exclusion test: `hsmsss/transaction_observer_test.go` → `TestTransactionObserver_ControlTransaction_NoEvent`
- the structural bypass for a `genRuntime`-capable runtime: `hsmsss/transport_control.go` → `(*transport).writeMessage`, `(*transport).sendResponse`, `genRuntime`
- the control initiators: `hsmsss/transport_active.go` → `(*transport).runSelectProcedure`;
  `hsmsss/transport_procedures.go` → `(*transport).runLinktest`
- the generation-bound sync entry point: `hsms/connection_send.go` → `(*connection).WriteMessageFromGeneration`
- the unconditional, nil-panicking accessors behind the `isData` gate: `hsms/data_msg.go` → `(*DataMessage).WaitBit`, `(*DataMessage).Stream`
- the handler recovery that can catch an observer panic: `hsms/handler_panic.go` → `runCallback`
- the connection identity behind origin tokens: `hsms/connection.go` → `NewConnection`, `originIdentity`
