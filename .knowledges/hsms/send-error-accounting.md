---
type: Mechanic
title: Send error accounting — which outcomes count
description: Which synchronous data-send outcomes count in DataMsgErrCount, and why every teardown-observed outcome — including a write that fails after teardown starts — reports ErrConnClosed and is excluded.
tags: [hsms, metrics, send, lifecycle]
status: stable
generated: {by: "claude/opus-5.5", at: 2026-10-23T18:00:00Z}
verified:
  - {by: "openai/gpt-6-astra", at: 2026-10-23T18:42:42Z}
sources:
  - {resource: hsms/connection_send.go, digest: sha256:ea94ab4fc9d25bb9, revision: 2a92a5e}
  - {resource: hsms/connection_metrics.go, digest: sha256:87b11ae3785c0870, revision: 2a92a5e}
  - {resource: hsms/connection_send_metrics_test.go, digest: sha256:457aeefcd9801abc, revision: 2a92a5e}
  - {resource: hsms/connection_runtime.go, digest: sha256:7f242f475b188a1b, revision: 2a92a5e}
  - {resource: hsms/epoch.go, digest: sha256:d6cc2283dd4d5e58, revision: 2a92a5e}
  - {resource: hsms/session.go, digest: sha256:04cc770917f02f59, revision: 2a92a5e}
  - {resource: hsmsss/transport.go, digest: sha256:038b98574e452c16, revision: 2a92a5e}
  - {resource: hsmsss/transport_procedures.go, digest: sha256:8ef63578b72806e0, revision: 2a92a5e}
  - {resource: gem/s9.go, digest: sha256:3fcf895ac2d6e4d9, revision: 2a92a5e}
  - {resource: hsms/connection_send_test.go, digest: sha256:be3c9cf11c739e99, revision: 2a92a5e}
  - {resource: hsms/harness_mock_transport_test.go, digest: sha256:60c294d49c079b14, revision: 2a92a5e}
---

# What it does

`DataMsgErrCount` counts synchronous data sends that failed locally:
a countable write failure on either synchronous path, or a genuine T3 expiry awaiting the reply.
A failed send is not automatically a *transaction error*:
some failures are lifecycle events the application caused,
and a teardown-interrupted write is one of those events even though the write itself failed on the socket.

Its godoc (`hsms/connection_metrics.go`) lists five outcomes it deliberately does not count:
a peer Reject, a peer SxF0 abort, a correlated S9 error (`WithS9Correlation`),
an async-path failure, and an oversized message (`ErrMessageTooLarge`).
It also states the teardown exclusion directly,
including that a write which fails after teardown has started is wrapped in `ErrConnClosed` rather than counted on its own.

What the godoc does not spell out:
- Caller-context cancellation as an exclusion in its own right —
  "caller cancellation keeps its existing behavior" names the case without saying what that behavior is
  (it returns the caller's `ctx.Err()` and does not count).
- That a control-transaction T6 expiry is counted nowhere in `DataMsgErrCount`.
  A T6 expiry on the auto-linktest path can still increment hsmsss's own `LinktestErrCount`
  (`runLinktest` skips only a cancelled ctx or `ErrConnClosed`);
  see [linktest teardown exclusion](/hsmsss/linktest-teardown-exemption.md).
- The increment-site count: three sites across two functions —
  write failure and T3 timeout in `sendWaitReplyOn`, and write failure in `sendNoReplyOn`.
  The wrappers `sendWaitReply` and `sendNoReply` only resolve the current epoch and delegate.

# How it works

Two steps decide whether a write failure counts.
First, `writeFrame` converts cause to value:
a transport write failure it observes after this epoch's `e.ctx` is already cancelled (teardown under way)
is returned as `ErrConnClosed`,
wrapping the raw transport error so `errors.Is` still matches it,
or passing an already-`ErrConnClosed` result through unwrapped.
Second, `isCountedSendErr` excludes by value;
it cannot tell a reclassified teardown error from any other `ErrConnClosed`, and does not need to.

Five error values are excluded from the data-error counter:
`ErrNotSelectedState` (a NotSelected drop, which has its own dedicated counter),
`ErrConnClosed` (connection teardown),
`context.Canceled` and `context.DeadlineExceeded` (caller cancellation or deadline),
and `ErrMessageTooLarge` —
a caller-side construction error `buildFrameBuffers` returns when a data message's frame would exceed `MaxMessageSize` (SEMI E37 §10.1 item 4).
`writeFrame` catches it before `writeMu` is acquired,
so it never reaches the transport and is never a link failure.
Any transport write error that matches none of these values counts.
That assumes the transport reserves `ErrConnClosed` for lifecycle outcomes:
a custom connection that returns the sentinel on a live generation is excluded too.

Both synchronous write paths apply the gate identically, as `isData && isCountedSendErr(err)`:
`sendWaitReplyOn` (reply-correlated; called by `sendWaitReply`, by `WriteMessage`'s observed path, and by `WriteMessageFromGeneration`)
and `sendNoReplyOn` (forward/relay; called by `sendNoReply` and by `WriteMessageNoReply`'s observed path —
`WriteMessageNoReply` backs `session.ForwardDataMessage`).
The async drain applies no classification at all —
see [the MaxMessageSize ceiling](/hsms/max-message-size-ceiling.md).

Separately, a GENUINE T3 expiry while waiting for a reply counts,
and when the auto-S9F9 knob is on it also sends an S9F9 (Transaction Timeout) notification
carrying the timed-out message's 10-byte header as SHEAD (`gem.S9F9`).
That notification goes through `SendAsync` and its own failure is deliberately swallowed,
because the caller already has the timeout error to report.

The four terminal branches of the reply wait attribute differently:
- reply, returned through `replyOutcome` and counted nowhere —
  a peer `*RejectError` (delivered by `routeReplyOn`)
  and an `*S9Error` under `WithS9Correlation` (delivered by `correlateS9On`) arrive on this branch and never touch the counter,
  and an SxF0 reply that `WithAbortReplyError` turns into `ErrTransactionAborted` is not counted either;
- protocol timeout (T3 counts, T6 does not);
- epoch teardown (`ErrConnClosed`, does not count);
- caller cancellation (returns the caller's error, does not count).

Go's select picks at random among whichever of the reply/timer/teardown/caller-ctx channels are ready,
so a reply that already arrived, or a teardown that already started, can lose that pick to a lower-priority arm.
The timer arm re-checks before committing:
a non-blocking `tryReply` first, then `e.ctx.Err() != nil` (returning `ErrConnClosed`),
and only then the protocol timeout.
The teardown arm re-checks `tryReply` before returning `ErrConnClosed`.
So the timeout branch, with its DataMsgErr increment and auto-S9F9 send,
is reached only when neither a reply nor a teardown was already available;
a teardown that merely lost the random pick to the timer still reports (and does not count) as `ErrConnClosed`.
This priority holds only among reply, teardown, and timer:
the `callerCtx.Done()` arm has no `tryReply` re-check,
so a reply that arrived at the same instant as caller cancellation can still lose to the caller's `ctx.Err()`.

Even a genuine timeout's counter increment sits **inside an `isData` check**,
so a control-transaction T6 expiry increments nothing here.
The data-error counter is exactly that: a *data* counter.

# Invariants

- `isCountedSendErr` excludes the `ErrConnClosed` *value*, not concurrent Close as a general event.
  In practice, whenever teardown is the observed terminal outcome of a synchronous data send
  (a write failure observed after teardown starts, the timer arm's re-check, the teardown arm itself),
  the send returns `ErrConnClosed` and is excluded.
  A concurrent reply or caller cancellation can still win instead; neither increments `DataMsgErrCount`.
- `(*epoch).teardown` closes the socket unconditionally and does not coordinate with an in-flight `writeMu`-held write.
  It cancels `e.ctx` BEFORE closing the socket (`markEnded` → `cancel` → `closeSocket`),
  which is what lets `writeFrame` attribute a write failure that observes a cancelled ctx to teardown.
- hsmsss's `(*transport).Write` returns `bufs.WriteTo`'s error unchanged, uninspected, and unwrapped;
  `writeFrame` is the one that inspects it.
- A write that fails on a still-live generation (`e.ctx.Err() == nil`) returns the raw transport error,
  tears the generation down via `TCPDownFromGeneration`,
  and counts unless the error itself matches an excluded value.
- A failed write says nothing about what the peer received:
  a deadline or teardown can truncate a frame mid-writev,
  so some bytes may already have reached the peer.
- A NotSelected drop is counted once, in its own counter, and never in the error counter —
  the two answer different questions ("was the peer unreachable?" vs "did the app send while down?").
- The W bit does not affect write-failure accounting.
  A fire-and-forget (W-bit clear) data send whose write succeeds records send+1, error+0:
  it returns at once and can never reach the timeout branch.
  A !W send whose `writeFrame` fails with a countable error records send+0, error+1, like any other data send,
  because that increment is guarded on `isData && isCountedSendErr(err)` and never consults the W bit.
- Only data transactions touch the data-error counter.
  A control T6 expiry is surfaced to the caller as `ErrT6Timeout` and counted nowhere in this counter —
  moving the increment outside the `isData` check would make linktest failures look like application errors.
- The S9F9 notification is best-effort by design;
  failing to send it must not replace or mask the timeout error the caller receives.
- Every synchronous write path that can increment the counter routes its `writeFrame` error through `isCountedSendErr` under an `isData` guard.
  The counter's godoc names both existing paths (`sendWaitReply`, `sendNoReply`).
- `ErrMessageTooLarge` and `ErrNotSelectedState` are both excluded from `DataMsgErrCount`:
  they are local, caller-side errors, never transport/link events,
  so they must not inflate a counter that signals protocol/link health.
  `ErrMessageTooLarge` is detected before `writeFrame` acquires `writeMu`;
  `ErrNotSelectedState` can arise at the pre-register gate, before acquisition, or at the write-boundary re-check while holding it.

# Failure modes

- **Adding a new error path without classifying it** defaults it to "counts",
  so a lifecycle event starts inflating the transaction error rate —
  the metric drifts in the direction that looks like equipment trouble.
- **Counting caller cancellation** makes every client-side abort look like a peer failure,
  which is backwards for diagnosing a slow tool.
- **Surfacing an S9F9 send failure** would replace an actionable T3 timeout with a confusing secondary error.

# Gotchas

Code comments that disagree with the code:
- `sendWaitReplyOn`'s write-failure comment says "The frame never reached the wire."
  That is not true for every `writeFrame` failure:
  `writeFrame`'s own comment notes a write may have sent part of the frame before failing,
  and `(*transport).Write` returns `bufs.WriteTo`'s error without requiring zero bytes written.
- `TestSendMetrics_DataMsgErr_OnWriteError_FireAndForget`'s doc comment says the increment is "guarded on isData alone";
  the guard is `isData && isCountedSendErr(err)`.
  Both write-error tests' comments also describe the failure as "before the wire",
  which holds for the test harness but not for a real partial write:
  the tests use `mockTransport`, whose `captureWrite` returns the configured `writeErr` without recording a frame.

The regression tests `TestSendMetrics_DataMsgErr_OnWriteError` (W-bit set)
and `TestSendMetrics_DataMsgErr_OnWriteError_FireAndForget` (!W)
both assert error+1 / send+0 for a failing transport write through `sendWaitReply`.

# Where to look

- the classification policy: `hsms/connection_send.go` → `isCountedSendErr`
- the write-failure teardown reclassification, ahead of `isCountedSendErr`: `hsms/connection_send.go` → `(*connection).writeFrame`
- the three increment sites and the four terminal branches with their priority re-check: `hsms/connection_send.go` → `(*connection).sendWaitReplyOn`, `(*connection).sendNoReplyOn`, `tryReply`, `replyOutcome`
- the callers that resolve the epoch: `hsms/connection_send.go` → `(*connection).sendWaitReply`, `(*connection).WriteMessage`, `(*connection).WriteMessageFromGeneration`, `(*connection).sendNoReply`, `(*connection).WriteMessageNoReply`
- the relay entry point: `hsms/session.go` → `(*session).ForwardDataMessage`
- Reject and S9 delivery onto the reply channel: `hsms/connection_runtime.go` → `(*connection).routeReplyOn`, `(*connection).correlateS9On`
- the timeout notification: `hsms/connection_send.go` → `(*connection).sendAutoS9F9`; `gem/s9.go` → `S9F9`
- the counter's public contract: `hsms/connection_metrics.go` → `(*ConnectionMetrics).DataMsgErrCount`
- the write-error regression tests: `hsms/connection_send_metrics_test.go` → `TestSendMetrics_DataMsgErr_OnWriteError`, `TestSendMetrics_DataMsgErr_OnWriteError_FireAndForget`
- the cancel-before-close teardown order: `hsms/epoch.go` → `(*epoch).teardown`, `(*epoch).markEnded`, `(*epoch).closeSocket`
- the raw, unwrapped write-error passthrough: `hsmsss/transport.go` → `(*transport).Write`
- the linktest error counting that a T6 expiry can still reach: `hsmsss/transport_procedures.go` → `(*transport).runLinktest`
- the write-error test harness: `hsms/connection_send_test.go` → `newTestSendConn`; `hsms/harness_mock_transport_test.go` → `captureWrite`
