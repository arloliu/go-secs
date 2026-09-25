---
type: Mechanic
title: The transaction observer's two chokepoints, its isData gate, and its outcome classifier
description: Why WithTransactionObserver instruments two call sites (not one), why the isData gate prevents an observer-path panic for control messages, and how classifyTxOutcome's six-way split relates to isCountedSendErr's binary one.
tags: [hsms, observability, send, metrics, lifecycle]
status: stable
generated: {by: "claude/sonnet-5", at: 2026-09-25T02:38:18Z}
verified:
  - {by: "openai/gpt-5.6-terra", at: 2026-09-25T03:13:36Z}
sources:
  - {resource: hsms/connection_send.go, digest: sha256:019a0edf15412086, revision: a1cdb0e}
  - {resource: hsms/transaction_observer.go, digest: sha256:a89096ae2659fb79, revision: 33e9744}
  - {resource: hsms/connection_config.go, digest: sha256:1dd3eb7cbc113324, revision: 922feb8}
  - {resource: hsms/session.go, digest: sha256:8394199f62beedfd, revision: 6c257b6}
  - {resource: hsmsss/transaction_observer_test.go, digest: sha256:cca4e16c9add5d25, revision: a7ff4a8}
  - {resource: hsmsss/transport_control.go, digest: sha256:84353e5b3b34860b, revision: 6c257b6}
  - {resource: hsms/data_msg.go, digest: sha256:32c3295c07f631df, revision: 6c257b6}
---

# What it does

`WithTransactionObserver`'s own godoc (`hsms/connection_config.go`) states the consumer contract —
which calls report an event, that the hook runs synchronously on the caller's goroutine,
and that `ReplyDataMessage` / the async sends are excluded.
It does not say *why* the exclusion is structural rather than a scope choice,
nor that the `isData` gate guarding the classifier is load-bearing: removing it can panic the observer path for a control message, crashing the process only if unrecovered,
nor how the new outcome classifier relates to the existing `isCountedSendErr` gate this unit already documents
([Send error accounting](/hsms/send-error-accounting.md)).
That is the delta this entry records.

# How it works

**Two chokepoints, not one.**
The brief this feature was built from assumed the four in-scope sync send calls converge on a single chokepoint.
They converge on two: `WriteMessage`
(backs `SendDataMessage` / `SendSECS2Message`, wraps `sendWaitReply`)
and `WriteMessageNoReply` (backs `ForwardDataMessage`, wraps `sendNoReply`).
`ReplyDataMessage` is a third, structurally different case —
`session.ReplyDataMessage` (`hsms/session.go`) is built on `rt.SendAsync`
or, since the R10 inbound-generation-fence work, `rt.SendAsyncFromGeneration`
when the primary's origin token matches this connection's own identity
(see the origin-token check in `ReplyDataMessage`'s own doc comment) —
a matching token selects the generation-bound path even after that generation has ended;
`SendAsyncFromGeneration` itself then checks liveness
and refuses a stale generation rather than falling back to unbound sending.
Both are the same enqueue-only async primitive family `SendDataMessageAsync` / `ForwardDataMessageAsync` use —
`SendAsyncFromGeneration` differs only in binding the enqueue to a caller-named generation instead of resolving `c.cur` at call time, not in reaching `WriteMessage`/`WriteMessageNoReply` or `newTxEvent`.
Either way the frame is only guaranteed *enqueued*;
`drainSendCh` (connection_send.go) writes it later, on a different goroutine,
and a frame still queued at teardown is stranded, never flushed.
`ReplyDataMessage` therefore never reaches either chokepoint and never reports a `TxEvent`,
regardless of what the brief's method list said or which of the two enqueue paths it took.

**The `isData` gate, and the structural bypass that now sits in front of it.**
As of the generation-gated send commits, `hsmsss`'s internal control initiators no longer call `rt.WriteMessage` directly.
`hsmsss/transport_active.go`'s Select.req and `hsmsss/transport_procedures.go`'s Linktest.req both call the `writeMessage` helper in `hsmsss/transport_control.go`,
which type-asserts the runtime to the `genRuntime` capability and, when present, calls `WriteMessageFromGeneration` instead of `WriteMessage`.
The production `hsms.connection` implements `genRuntime`,
so for it a control send never reaches `WriteMessage`, `WriteMessageNoReply`, `newTxEvent`, or the `isData` gate at all.
It goes straight to `sendWaitReplyOn` on the caller-named generation,
which reports no `TxEvent` by construction (see `WriteMessageFromGeneration`'s own doc comment).
The `isData` gate inside `WriteMessage`/`WriteMessageNoReply`
(`if !isData { return c.sendWaitReply(ctx, msg) }` and its `WriteMessageNoReply` counterpart)
is still the only thing protecting a caller that reaches `WriteMessage` with a `*ControlMessage` some other way —
a runtime that does not implement `genRuntime` (an external `TransportRuntime`, or an internal call with `gen == 0`).
That gate was teeth-checked by deleting it, at the earlier revision when control sends still ran through `WriteMessage` unconditionally.
The failure was not the "Stream=0/Function=0 rows pollute a consumer's histogram" symptom one would guess from the classifier's shape.
It was an immediate crash, unless a caller recovered it —
but not inside `newTxEvent` itself, and not for both chokepoints the same way.
`WriteMessage` evaluates `dm.WaitBit()` as `newTxEvent`'s own call argument,
so a nil `dm` panics there, before `newTxEvent` is even entered
(`(*DataMessage).WaitBit` dereferences `msg.header` unconditionally, with no nil check).
`WriteMessageNoReply` passes the literal `false` instead of calling `WaitBit()`,
so a nil `dm` reaches `newTxEvent`'s body unharmed and panics one line later, at `dm.Stream()` —
the first field `newTxEvent`'s `TxEvent{...}` literal evaluates.
Either way the panic fires after the inner `sendWaitReply`/`sendNoReply` call has already returned,
and it propagates up the caller's stack like any other panic —
a caller with its own `recover()` catches it; one without lets it crash the process.

**`hsmsss/transaction_observer_test.go`'s `TestTransactionObserver_ControlTransaction_NoEvent` doc comment was stale,
and `a7ff4a8` rewrote it.**
It used to describe an obsolete teeth-check (deleting `WriteMessage`'s `isData` gate crashes the process) as proving today's behavior,
when the real Select.req/Linktest.req traffic this test exercises now bypasses `WriteMessage` entirely via `WriteMessageFromGeneration`.
The rewritten comment now names both barriers — the structural bypass and the `isData` gate —
and states that only removing both reaches `newTxEvent` unsafely, matching what this entry already found.
The test's own assertion (`rec.snapshot()` stays empty across several linktest round trips)
is unchanged and still meaningful.

**The classifier vs. the counter.**
`classifyTxOutcome` (`hsms/connection_send.go`, next to `isCountedSendErr`)
maps `(replyWaited bool, err error)` to one of six `TxOutcome` values.
It partitions the *same* error set `isCountedSendErr` already classifies,
but the two disagree on granularity and partly on philosophy.
`isCountedSendErr` is binary (counts toward `DataMsgErrCount` or does not)
and excludes `ErrNotSelectedState`, `ErrConnClosed`, `ErrMessageTooLarge`,
and caller-context cancellation/deadline from the count —
see [Send error accounting](/hsms/send-error-accounting.md) for why.
`classifyTxOutcome` has six buckets,
and deliberately routes `ErrConnClosed` to `TxCanceled` — grouped with caller-context cancellation, not with `TxSendError`.
`TxCanceled`'s own enum doc (`hsms/transaction_observer.go`) already names both cases explicitly, including connection closure —
so the grouping is not a silent extension of an underspecified doc; the doc and the classifier agree.
The grouping is intentional:
it mirrors `isCountedSendErr`'s "teardown and caller-cancel are both lifecycle events, not transport failures" philosophy,
extended to a new place.
`ErrNotSelectedState` and `ErrMessageTooLarge`, by contrast, land in `TxSendError` under `classifyTxOutcome`
even though `isCountedSendErr` also excludes them from the *counter* —
the two functions answer different questions
("does this count as link-health noise?" vs. "how did this transaction end?"),
so agreement on one axis (lifecycle exclusion) does not imply agreement on the other (does it count as a *failure* at all).

**Observer timing.**
The observer runs as a plain statement *after* the inner `sendWaitReply`/`sendNoReply` call fully returns —
not via a `defer` registered inside those functions.
Neither `sendWaitReply` nor `sendNoReply` is modified by this feature at all.
By the time the observer runs,
reply-registry deregistration, the I1 inflight-gauge decrement, and timer-pool cleanup have already completed,
so an observer panic (deliberately not recovered — see `WithTransactionObserver`'s godoc)
cannot bypass that cleanup or leak any of that internal state.
The panic itself is not confined to `WriteMessage`'s own frame, though:
like any unrecovered panic it keeps unwinding through the calling frames until something recovers it, or the process crashes.

# Invariants

- Every sync return path in `sendWaitReply`/`sendNoReply` reachable when `isData` is true maps to exactly one `TxOutcome` —
  the full return-path table lives as `classifyTxOutcome`'s own doc comment in `hsms/connection_send.go`.
- The `isData` gate must run before any field of `dm` is read.
  `newTxEvent` assumes a non-nil `dm`; nothing downstream nil-checks it.
- `ReplyDataMessage` must never be rerouted onto `WriteMessage`/`WriteMessageNoReply` to make it observable
  without also accepting the blocking-semantics and metric-attribution break that would be —
  see [Send error accounting](/hsms/send-error-accounting.md)'s note that `ReplyDataMessage` is already classified as an async path there too.
- `sendNoReply` arms no T3 timer and registers no reply channel,
  so it never generates a `TxT3Timeout` or `TxRejected` outcome itself.
  But `classifyTxOutcome` classifies purely on the returned error, without checking `replyWaited` —
  a transport `Write` that itself returns `ErrT3Timeout` or a `*RejectError`
  produces the matching outcome through `WriteMessageNoReply` too.

# Failure modes

- **Deleting or weakening the `isData` gate** does not degrade gracefully into noisy data.
  Against the production `hsms.connection` it currently has no observable effect at all, because real Select.req/Linktest.req traffic already bypasses `WriteMessage` via `WriteMessageFromGeneration` and never reaches the gate.
  With a non-nil `txObserver` installed,
  it panics the moment a caller reaches `WriteMessage`/`WriteMessageNoReply` with a `*ControlMessage` —
  a `genRuntime`-incapable `TransportRuntime`, or an internal call made with `gen == 0` —
  because a nil `dm` reaches an unconditional field/method dereference:
  `dm.WaitBit()` at `WriteMessage`'s own call site, or `dm.Stream()` inside `newTxEvent` for `WriteMessageNoReply`.
  With no observer installed, both functions return before ever reaching the gate, so that fast path is unaffected.
  The panic fires after the send itself has already completed and propagates up the caller's stack,
  so an uncaught one crashes the process — but a caller with its own `recover()` survives it.
  A change that removes the gate can therefore look safe against the shipped transports (no observer installed by default)
  and still be a live landmine for any caller that also installs an observer.
- **Assuming `classifyTxOutcome` and `isCountedSendErr` agree on what "counts"**
  is wrong on one axis (both exclude teardown/cancel as lifecycle noise) but not the other
  (`classifyTxOutcome` still names `ErrNotSelectedState`/`ErrMessageTooLarge` as a `TxOutcome`,
  where `isCountedSendErr` simply drops them from the counter).
  A third error-classifying function added to this file should decide its own stance on both axes
  rather than assume either existing one generalizes.
- **Rerouting `ReplyDataMessage` through a sync chokepoint** to make it observable
  would change its blocking behavior and error surface — a breaking API change, not an observability fix.

# Where to look

- the two chokepoints: `hsms/connection_send.go` → `(*connection).WriteMessage`,
  `(*connection).WriteMessageNoReply`
- the classifier and the return-path table: `hsms/connection_send.go` → `classifyTxOutcome`
- event construction: `hsms/connection_send.go` → `newTxEvent`
- the excluded async path: `hsms/session.go` → `(*session).ReplyDataMessage`
- the option and field: `hsms/connection_config.go` → `WithTransactionObserver`
- the types: `hsms/transaction_observer.go` → `TxEvent`, `TxOutcome`
- the gate's teeth-check, its doc comment corrected in a7ff4a8 to match the current wiring:
  `hsmsss/transaction_observer_test.go` → `TestTransactionObserver_ControlTransaction_NoEvent`
- the structural bypass for a `genRuntime`-capable runtime: `hsmsss/transport_control.go` → `(*transport).writeMessage`, `(*transport).sendResponse`, `genRuntime`
- the generation-bound entry points it calls: `hsms/connection_send.go` → `(*connection).WriteMessageFromGeneration`, `(*connection).SendAsyncFromGeneration`
- the unconditional, nil-panicking accessors behind the `isData` gate: `hsms/data_msg.go` → `(*DataMessage).WaitBit`, `(*DataMessage).Stream`
