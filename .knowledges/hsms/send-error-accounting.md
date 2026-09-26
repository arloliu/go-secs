---
type: Mechanic
title: Send error accounting — which outcomes count
description: Which synchronous data-send outcomes count, and why every teardown-observed outcome — including a write that fails after teardown starts — reports ErrConnClosed and is excluded.
tags: [hsms, metrics, send, lifecycle]
status: stable
generated: {by: "claude/sonnet-5", at: 2026-09-25T03:51:29Z}
verified:
  - {by: "openai/gpt-5.6-terra", at: 2026-09-25T06:13:16Z}
sources:
  - {resource: hsms/connection_send.go, digest: sha256:e485168d1a431fe7, revision: d244104}
  - {resource: hsms/connection_metrics.go, digest: sha256:4a106e75d9f7803b, revision: 4be2062}
  - {resource: hsms/connection_send_metrics_test.go, digest: sha256:457aeefcd9801abc, revision: a1cdb0e}
  - {resource: hsms/epoch.go, digest: sha256:5d48c9656ae715cc, revision: 4be2062}
  - {resource: hsmsss/transport.go, digest: sha256:cb3594d212e03da1, revision: c00e1b5}
---

# What it does

`DataMsgErrCount`'s godoc (`hsms/connection_metrics.go`) covers attribution more fully as of
`a1cdb0e` than it used to: it lists three outcomes it deliberately does not count — a peer Reject,
an async-path failure, and `ErrMessageTooLarge` — and now also names the teardown exclusion
directly, including that a write which fails after teardown has started is wrapped in
`ErrConnClosed` rather than counted on its own.
It still does not spell out caller-context cancellation as an exclusion in its own right — "caller
cancellation keeps its existing behavior" names the case without saying what that behavior is —
nor that a control-transaction T6 expiry is counted nowhere in `DataMsgErrCount` (a T6 expiry on the
auto-linktest path can still increment hsmsss's own `LinktestErrCount`; see
[linktest teardown exclusion](/hsmsss/linktest-teardown-exemption.md)),
nor the increment-site count (three sites across two functions, not one per function).
That is the delta this entry records: a failed send is not automatically a *transaction error*,
because some failures are lifecycle events the application caused, and — since `a1cdb0e` — a
teardown-interrupted write is one of those events even though the write itself failed on the
socket.

**Round 1 of this entry recorded one godoc-vs-code disagreement — *which paths* the counter covers —
and that one is now resolved.**
The godoc used to say the counter "counts only the synchronous send path (sendWaitReply)", which was
false — `sendNoReply` (the forward/relay path behind `ForwardDataMessage`) increments it too, through
the same `isCountedSendErr` gate.
The godoc still does not spell out the increment-site count: three sites across those two
functions, not one per function — `sendWaitReply` counts on both its `writeFrame`-failure branch
and its T3-timeout branch, `sendNoReply` on its single `writeFrame`-failure branch.

**Round 3 found a second, distinct godoc imprecision in the same block, on the fire-and-forget
bullet — round 4 fixed it in both doc comments.**
The list of what is "Deliberately NOT counted here" used to include "a fire-and-forget (non-W-bit)
send, which returns before any reply wait" — a blanket exclusion the code never enforced.
`sendWaitReply` computes `fireAndForget`, but its `writeFrame`-failure increment is guarded on
`isData && isCountedSendErr(err)`, never on the W bit, so a !W data send whose write fails with a countable error records
DataMsgSend+0 and DataMsgErr+**1** — the same outcome as any other data send whose write fails, even
one that already partially reached the wire before the failure (a deadline can truncate a frame
mid-writev, so a failed write says nothing about what the peer received).
What is actually exempt is narrower: a !W send can never reach the *T3* branch, because it
short-circuits the moment a write that SUCCEEDS puts the frame on the wire.
Round 4 rewrote `DataMsgErrCount`'s godoc to say exactly that — the summary line now reads "the
total number of synchronous data sends that failed locally" (dropping "reply-expected", which used
to exclude `sendNoReply`), the fire-and-forget bullet is gone from the "Deliberately NOT counted"
list, and the body states a write failure counts on either `sendWaitReply` outcome (W-bit-set or
fire-and-forget) and on `sendNoReply`, while only a W-bit `sendWaitReply` transaction ever reaches
T3.
`sendWaitReply`'s fire-and-forget short-circuit comment (`connection_send.go`) was scoped the same
way: it now says a !W send that *reached the wire* (write already succeeded) records send+1/err+0
*at that point*, not that a !W send is exempt from error counting in general.

`TestSendMetrics_DataMsgErr_OnWriteError_FireAndForget`
(`hsms/connection_send_metrics_test.go`) now pins the intersection this entry used to flag as
uncovered: a !W (fire-and-forget) `sendWaitReply` call whose transport write fails.
It asserts `DataMsgErrCount` goes to 1 and `DataMsgSendCount` stays 0, the same pairing
`TestSendMetrics_DataMsgErr_OnWriteError` asserts for the W-bit-set case.
Both tests' own doc comments record why: the increment sits on the `writeFrame`-failure branch,
guarded on `isData` and the error value (never the W bit), before `fireAndForget`'s short-circuit is ever reached, so a regression
that gated it on the W bit would let a !W write failure vanish from the counter.

**Round 5 (`a1cdb0e`) reversed the "teardown-interrupted write still counts" finding rounds 1-4
built on.**
It did so by fixing the RACE the earlier rounds only documented.
`writeFrame` now checks
`e.ctx.Err()` after `c.tr.Write` fails: when this epoch's teardown has already started it returns
`ErrConnClosed` — wrapping the raw transport error so `errors.Is` still matches it, or passing an
already-`ErrConnClosed` result straight through — instead of the raw socket error `isCountedSendErr`
used to see and count. `DataMsgErrCount`'s godoc was rewritten to state this exclusion directly
("a write that fails while on the socket... is wrapped in ErrConnClosed... rather than counted on
its own"), closing the gap rounds 1-4 flagged as undocumented. `sendWaitReplyOn`'s reply-wait select
also gained a priority re-check: a reply that already arrived wins over a teardown or a protocol
timeout, and a teardown wins over a protocol timeout, so a T3/T6 timeout only counts (and only
triggers auto-S9F9) when it is genuinely a timeout, not a teardown that happened to lose Go's random
pick among ready select cases.

# How it works

`isCountedSendErr` is the value-level policy, but it is no longer the whole story.
Since `a1cdb0e`, `writeFrame` does a cause-to-value conversion FIRST: a write failure it observes
after this epoch's `e.ctx` is already cancelled (teardown under way) is classified `ErrConnClosed`
before it is ever handed to `isCountedSendErr`.
`isCountedSendErr` then excludes by value, same as always — it cannot tell a reclassified teardown
error from an `ErrConnClosed` `writeFrame` returned some other way, and does not need to.
Four classes of error are excluded from the data-error counter:
a NotSelected drop (which has its own dedicated counter), connection teardown, caller-context cancellation or deadline,
and `ErrMessageTooLarge` — a caller-side message-construction error `buildFrameBuffers` returns
when a data message's frame would exceed `MaxMessageSize` (SEMI E37 §10.1 item 4).
`writeFrame` catches it before `writeMu` is even acquired,
so it never reaches the transport and is never a link failure.
Any transport write error that matches none of these values counts.
That assumes the transport reserves `ErrConnClosed` for lifecycle outcomes:
a custom connection that returns the sentinel on a live generation is excluded too.
Both synchronous write paths apply it identically, each under an `isData` guard: `sendWaitReplyOn`
(reply-correlated — the shared body `sendWaitReply` and the generation-bound `WriteMessageFromGeneration` both call)
and `sendNoReply` (forward/relay). The async drain applies no classification at
all — see [the MaxMessageSize ceiling](/hsms/max-message-size-ceiling.md).

Separately, a GENUINE T3 expiry while waiting for a reply counts,
and when the auto-S9F9 knob is on it also sends an S9F9 (Transaction Timeout) notification carrying the timed-out message's 10-byte header as SHEAD.
That notification is fire-and-forget and its own failure is deliberately swallowed, because the caller already has the timeout error to report.

The four terminal branches of the reply wait attribute differently:
reply (returned verbatim as `(res.msg, res.err)` and counted nowhere —
a peer `*RejectError` arrives on this branch and never touches the counter),
protocol timeout,
epoch teardown (`ErrConnClosed`, does not count),
caller cancellation (returns the caller's error, does not count).

Since `a1cdb0e` the timer and teardown arms are no longer independent of each other or of the reply.
Go's select picks among whichever of the reply/timer/teardown/caller-ctx channels are ready at random,
so a reply that already arrived, or a teardown that already started, can lose that pick to a lower-priority arm.
Both the timer arm and the teardown arm re-check, in the SAME priority order (reply, then teardown), before committing to their own outcome:
a non-blocking `tryReply` first, then — timer arm only — `e.ctx.Err() != nil`.
So the T3/T6 timeout branch, and its DataMsgErr increment and auto-S9F9 send, are reached only when neither a reply nor a teardown was already available;
a teardown that merely lost the random pick to the timer still reports (and does not count) as `ErrConnClosed`, not as a timeout.
This priority holds only among these three arms — reply, teardown, timer — not for the fourth:
`callerCtx.Done()` has no `tryReply` re-check, so a reply that arrived at the same instant as caller cancellation can still lose to the caller's own `ctx.Err()`.

The timeout branch is narrower than it looks, in two ways.
A T3 (data) or T6 (control) timeout only returns its timeout error once the priority recheck above finds neither a reply nor a teardown already available —
the timer arm returns an available reply first, otherwise `ErrConnClosed` if teardown is observed, otherwise the protocol timeout;
the teardown arm likewise prefers an available reply;
caller cancellation has no such recheck.
And even a genuine timeout's counter increment sits **inside an `isData` check** —
so a control-transaction T6 expiry increments nothing.
The data-error counter is exactly that: a *data* counter.

# Invariants

- `isCountedSendErr` excludes the `ErrConnClosed` *value* from the cumulative data-error counter,
  not concurrent Close as a general event — but as of `a1cdb0e` that distinction has narrowed
  to the point where, whenever teardown is the observed terminal outcome of a synchronous data send
  (a write failure observed after teardown starts, the timer arm's re-check, the teardown arm itself),
  the send returns `ErrConnClosed` and is excluded.
  A concurrent reply or caller cancellation can still win instead; neither increments `DataMsgErrCount`.
  `(*epoch).teardown` still closes the socket unconditionally and does not coordinate with an
  in-flight `writeMu`-held write — that race is unchanged — but `writeFrame` no longer lets the
  loser of that race surface as a raw, uninspected error.
  `hsmsss`'s `(*transport).Write` still returns a transport write error unchanged, uninspected
  and unwrapped; `writeFrame` is the one that now inspects it.
  After `c.tr.Write` fails, `writeFrame` checks `e.ctx.Err()`: when this epoch's teardown has
  already started (`epoch.teardown` cancels `e.ctx` BEFORE closing the socket — `markEnded` ->
  `cancel` -> `closeSocket`, see `hsms/epoch.go`), it returns `ErrConnClosed`, wrapping the raw
  transport error (`errors.Is` still matches it) or passing an already-`ErrConnClosed` result
  through unwrapped.
  A write that fails on a still-live generation (`e.ctx.Err() == nil`) is unaffected: it still
  returns the raw transport error, still tears the generation down via `TCPDownFromGeneration`,
  and counts unless the error itself matches an excluded value (a custom connection returning `ErrConnClosed`, for example).
  `connection_send.go`'s own comment on the reply-wait select's teardown branch used to carry the
  overstatement this entry corrected in an earlier round
  ("a normal Close mid-transaction never inflates the cumulative error counter"),
  then — after `ed4665e` — the opposite correction ("a write a teardown interrupts can still fail
  with the raw socket error, which does count").
  `a1cdb0e` removed that second comment along with the behavior it described: a write a teardown
  interrupts no longer counts, because `writeFrame` reclassifies it before `isCountedSendErr` ever
  sees it.
- A NotSelected drop is counted once, in its own counter, and never in the error counter — the two answer different questions ("was the peer unreachable?" vs "did the app send while down?").
- A fire-and-forget (W-bit clear) data send that reaches the wire records send+1, error+0: it returns
  at once and can never reach a timeout branch.
  The exemption is the *timeout* branch only — it is not an exemption from error counting.
  The W bit does not affect error accounting: a !W send whose `writeFrame` fails with a countable error
  records send+0/error+1 like any other data send, because that increment is guarded on
  `isData && isCountedSendErr(err)` and never consults the W bit.
  Do not read this invariant (or the counter's godoc) as "a !W send never increments the error
  counter"; a diff that made the write-failure increment W-bit-conditional would be a behaviour
  change, not a fix.
- Only data transactions touch the data-error counter. A control T6 expiry is a protocol event, surfaced to the caller as `ErrT6Timeout` and counted nowhere in this counter — moving the increment outside the `isData` check would make linktest failures look like application errors.
- The S9F9 notification is best-effort by design; failing to send it must not replace or mask the timeout error the caller receives.
- Every synchronous write path that can increment the counter routes its `writeFrame` error through
  `isCountedSendErr` under an `isData` guard.
  A new sync send path that increments directly, or that forgets the `isData` guard, is the diff to
  reject — the counter's godoc now names both existing paths (`sendWaitReply`, `sendNoReply`), so a
  third path appearing here without a matching godoc update is itself a regression to catch.
- `ErrMessageTooLarge` is excluded the same way `ErrNotSelectedState` is: it is a local, caller-side construction error caught before `writeMu` is acquired, never a transport/link event,
  so it must not inflate a counter that exists to signal protocol/link health.

# Failure modes

- **Adding a new error path without classifying it** defaults it to "counts", so a lifecycle event starts inflating the transaction error rate — the metric drifts in the direction that looks like equipment trouble.
- **Counting caller cancellation** makes every client-side abort look like a peer failure, which is backwards for diagnosing a slow tool.
- **Surfacing an S9F9 send failure** would replace an actionable T3 timeout with a confusing secondary error.

# Where to look

- the classification policy: `hsms/connection_send.go` → `isCountedSendErr`
- the write-failure teardown reclassification, ahead of `isCountedSendErr`: `hsms/connection_send.go` → `(*connection).writeFrame`
- the four terminal branches, their priority re-check, and their attribution: `hsms/connection_send.go` → `(*connection).sendWaitReplyOn` (called by `(*connection).sendWaitReply` and `(*connection).WriteMessageFromGeneration`), `tryReply`
- the second synchronous path, now also named in the godoc: `hsms/connection_send.go` → `(*connection).sendNoReply`
- the timeout notification: `hsms/connection_send.go` → `(*connection).sendAutoS9F9`
- the counter's public contract, now accurate on which paths it counts: `hsms/connection_metrics.go` → `(*ConnectionMetrics).DataMsgErrCount`
- the fire-and-forget write-error regression test: `hsms/connection_send_metrics_test.go` → `TestSendMetrics_DataMsgErr_OnWriteError_FireAndForget`, `TestSendMetrics_DataMsgErr_OnWriteError`
- the unconditional, write-uncoordinated socket close: `hsms/epoch.go` → `(*epoch).teardown`
- the raw, unwrapped write-error passthrough: `hsmsss/transport.go` → `(*transport).Write`
