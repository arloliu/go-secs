---
type: Mechanic
title: Send error accounting — which outcomes count
description: Why a normal Close mid-transaction does not inflate the error counter, and what does.
tags: [hsms, metrics, send, lifecycle]
status: stable
generated: {by: "claude/sonnet-5", at: 2026-08-12T08:55:39Z}
verified:
  - {by: "codex/gpt-5.6-sol", at: 2026-08-12T09:03:31Z}
sources:
  - {resource: hsms/connection_send.go, digest: sha256:9b1ccf21a9d24c0d, revision: b1bb17b}
  - {resource: hsms/connection_metrics.go, digest: sha256:44481eac3475bc65, revision: b1bb17b}
---

# What it does

`DataMsgErrCount`'s godoc (`hsms/connection_metrics.go`) does cover attribution, but only partly:
it lists three outcomes it deliberately does not count — a peer Reject, an async-path failure, and
`ErrMessageTooLarge`.
It never names the two lifecycle exclusions `isCountedSendErr` actually enforces (connection teardown
and caller-context cancellation), nor that a control-transaction T6 expiry is counted nowhere.
That is the delta this entry records: a failed send is not automatically a *transaction error*,
because some failures are lifecycle events the application caused.

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
`isData` alone, never on the W bit, so a !W data send whose write genuinely fails records
DataMsgSend+0 and DataMsgErr+**1** — the same outcome as any other data send that never reaches the
wire.
What is actually exempt is narrower: a !W send can never reach the *T3* branch, because it
short-circuits the moment the frame is on the wire.
Round 4 rewrote `DataMsgErrCount`'s godoc to say exactly that — the summary line now reads "the
total number of synchronous data sends that failed locally" (dropping "reply-expected", which used
to exclude `sendNoReply`), the fire-and-forget bullet is gone from the "Deliberately NOT counted"
list, and the body states a write failure counts on either `sendWaitReply` outcome (W-bit-set or
fire-and-forget) and on `sendNoReply`, while only a W-bit `sendWaitReply` transaction ever reaches
T3.
`sendWaitReply`'s fire-and-forget short-circuit comment (`connection_send.go`) was scoped the same
way: it now says a !W send that *reached the wire* (write already succeeded) records send+1/err+0
*at that point*, not that a !W send is exempt from error counting in general.

No test currently pins a write failure on a !W (fire-and-forget) `sendWaitReply` call.
`TestSendMetrics_DataMsgErr_OnWriteError` (`hsms/connection_send_metrics_test.go`) sets `tr.writeErr`
but sends with the W bit set; `TestSendMetrics_FireAndForget_SendPlusOne_ErrZero` sends with the W
bit clear but leaves `tr.writeErr` unset.
No test exercises the intersection — a !W send whose write fails — so nothing in the suite would
catch a regression that made the write-failure increment W-bit-conditional.

# How it works

`isCountedSendErr` is the whole policy.
Four classes of error are excluded from the data-error counter:
a NotSelected drop (which has its own dedicated counter), connection teardown, caller-context cancellation or deadline,
and `ErrMessageTooLarge` — a caller-side message-construction error `buildFrameBuffers` returns
when a data message's frame would exceed `MaxMessageSize` (SEMI E37 §10.1 item 4).
`writeFrame` catches it before `writeMu` is even acquired,
so it never reaches the transport and is never a link failure.
Anything else — a genuine transport write failure — counts.
Both synchronous write paths apply it identically, each under an `isData` guard: `sendWaitReply`
(reply-correlated) and `sendNoReply` (forward/relay). The async drain applies no classification at
all — see [the MaxMessageSize ceiling](/hsms/max-message-size-ceiling.md).

Separately, a T3 expiry while waiting for a reply counts, and when the auto-S9F9 knob is on it also sends an S9F9 (Transaction Timeout) notification carrying the timed-out message's 10-byte header as SHEAD. That notification is fire-and-forget and its own failure is deliberately swallowed, because the caller already has the timeout error to report.

The four terminal branches of the reply wait attribute differently:
reply (returned verbatim as `(res.msg, res.err)` and counted nowhere — a peer `*RejectError` arrives on this branch and never touches the counter),
protocol timeout, epoch teardown (`ErrConnClosed`, does not count), caller cancellation (returns the caller's error, does not count).

The timeout branch is narrower than it looks. Both T3 (data) and T6 (control) return their timeout error, but the counter increment sits **inside an `isData` check** — so a control-transaction T6 expiry increments nothing. The data-error counter is exactly that: a *data* counter.

# Invariants

- Closing a connection mid-transaction must never increment the cumulative data-error counter. This is the reason `isCountedSendErr` exists instead of a bare `err != nil`.
- A NotSelected drop is counted once, in its own counter, and never in the error counter — the two answer different questions ("was the peer unreachable?" vs "did the app send while down?").
- A fire-and-forget (W-bit clear) data send that reaches the wire records send+1, error+0: it returns
  at once and can never reach a timeout branch.
  The exemption is the *timeout* branch only — it is not an exemption from error counting.
  A !W send whose `writeFrame` fails is counted like any other data send, because that increment is
  guarded on `isData` alone and never consults the W bit.
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
- the four terminal branches and their attribution: `hsms/connection_send.go` → `(*connection).sendWaitReply`
- the second synchronous path, now also named in the godoc: `hsms/connection_send.go` → `(*connection).sendNoReply`
- the timeout notification: `hsms/connection_send.go` → `(*connection).sendAutoS9F9`
- the counter's public contract, now accurate on which paths it counts: `hsms/connection_metrics.go` → `(*ConnectionMetrics).DataMsgErrCount`
