---
type: Mechanic
title: The transaction observer's two chokepoints, its isData gate, and its outcome classifier
description: Why WithTransactionObserver instruments two call sites (not one), why the isData gate is load-bearing enough to crash the process without it, and how classifyTxOutcome's six-way split relates to isCountedSendErr's binary one.
tags: [hsms, observability, send, metrics, lifecycle]
status: draft
generated: {by: "claude/sonnet-5", at: 2026-08-13T13:40:00Z}
sources:
  - {resource: hsms/connection_send.go, digest: sha256:6cec3817c0e2a9de, revision: 33e9744}
  - {resource: hsms/transaction_observer.go, digest: sha256:a89096ae2659fb79, revision: 33e9744}
  - {resource: hsms/connection_config.go, digest: sha256:8a454cb02ca9582f, revision: 33e9744}
  - {resource: hsms/session.go, digest: sha256:df87568a198c41ac, revision: 33e9744}
  - {resource: hsmsss/transaction_observer_test.go, digest: sha256:aad0db840d794f9a, revision: 33e9744}
---

# What it does

`WithTransactionObserver`'s own godoc (`hsms/connection_config.go`) states the consumer contract —
which calls report an event, that the hook runs synchronously on the caller's goroutine,
and that `ReplyDataMessage` / the async sends are excluded.
It does not say *why* the exclusion is structural rather than a scope choice,
nor that the `isData` gate guarding the classifier is load-bearing enough to crash the whole process if removed,
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
`session.ReplyDataMessage` (`hsms/session.go`) is built on `rt.SendAsync`,
the same enqueue-only async primitive `SendDataMessageAsync` / `ForwardDataMessageAsync` use.
`SendAsync` only guarantees the frame was *enqueued*;
`drainSendCh` (connection_send.go) writes it later, on a different goroutine,
and a frame still queued at teardown is stranded, never flushed.
`ReplyDataMessage` therefore never reaches either chokepoint and never reports a `TxEvent`,
regardless of what the brief's method list said.

**The `isData` gate.**
`WriteMessage` is also the send path for internal control transactions —
`hsmsss/transport_active.go`'s Select.req and `hsmsss/transport_procedures.go`'s Linktest.req both call `rt.WriteMessage` with a `*ControlMessage`,
where the `*DataMessage` type assertion (`dm`) is `nil`.
Both `WriteMessage` and `WriteMessageNoReply` type-assert `msg` to `*DataMessage`
and skip the observer entirely (`if !isData { return c.sendWaitReply(ctx, msg) }`)
before ever building a `TxEvent`.
This gate was teeth-checked by deleting it:
the failure is not the "Stream=0/Function=0 rows pollute a consumer's histogram" symptom one would guess from the classifier's shape.
It is an immediate, whole-process crash —
`newTxEvent` calls `dm.WaitBit()` on a nil `*DataMessage`,
and `DataMessage.WaitBit()` dereferences `msg.header` unconditionally,
so every connection that combines `WithTransactionObserver` with a real `Open()` panics with SIGSEGV on its own Select.req handshake,
before a single data message is ever sent.
See `hsmsss/transaction_observer_test.go`'s `TestTransactionObserver_ControlTransaction_NoEvent` doc comment for the full finding.

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
and deliberately routes `ErrConnClosed` to `TxCanceled` — grouped with caller-context cancellation, not with `TxSendError` —
even though `TxCanceled`'s own enum doc (`hsms/transaction_observer.go`) only names caller-context cancellation explicitly.
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
can only unwind `WriteMessage`'s own empty stack frame; it cannot leak any of that internal state.

# Invariants

- Every sync return path in `sendWaitReply`/`sendNoReply` reachable when `isData` is true maps to exactly one `TxOutcome` —
  the full return-path table lives as `classifyTxOutcome`'s own doc comment in `hsms/connection_send.go`.
- The `isData` gate must run before any field of `dm` is read.
  `newTxEvent` assumes a non-nil `dm`; nothing downstream nil-checks it.
- `ReplyDataMessage` must never be rerouted onto `WriteMessage`/`WriteMessageNoReply` to make it observable
  without also accepting the blocking-semantics and metric-attribution break that would be —
  see [Send error accounting](/hsms/send-error-accounting.md)'s note that `ReplyDataMessage` is already classified as an async path there too.
- `TxT3Timeout` and `TxRejected` are reachable only via `WriteMessage`/`sendWaitReply` —
  `sendNoReply` arms no T3 timer and registers no reply channel.

# Failure modes

- **Deleting or weakening the `isData` gate** does not degrade gracefully into noisy data;
  it crashes the process on the next Select.req or Linktest.req sent through an observed connection,
  because `newTxEvent` reads `dm`'s fields unconditionally.
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
- the gate's teeth-check: `hsmsss/transaction_observer_test.go` →
  `TestTransactionObserver_ControlTransaction_NoEvent`
