---
type: Mechanic
title: Send error accounting — which outcomes count
description: Why a normal Close mid-transaction does not inflate the error counter, and what does.
tags: [hsms, metrics, send, lifecycle]
status: stable
generated: {by: "claude/opus-5", at: 2026-08-05T12:08:31Z}
verified:
  - {by: "agy/gemini-3.1-pro-high", at: 2026-08-05T14:10:00Z}
sources:
  - {resource: hsms/connection_send.go, digest: sha256:e534fecb0a49c465, revision: bc97919}
---

# What it does

No package doc explains how send outcomes are attributed to counters. The distinction is deliberate and easy to break: a failed send is not automatically a *transaction error*, because some failures are lifecycle events the application caused.

# How it works

`isCountedSendErr` is the whole policy. Three classes of error are excluded from the data-error counter: a NotSelected drop (which has its own dedicated counter), connection teardown, and caller-context cancellation or deadline. Anything else — a genuine transport write failure — counts.

Separately, a T3 expiry while waiting for a reply counts, and when the auto-S9F9 knob is on it also sends an S9F9 (Transaction Timeout) notification carrying the timed-out message's 10-byte header as SHEAD. That notification is fire-and-forget and its own failure is deliberately swallowed, because the caller already has the timeout error to report.

The four terminal branches of the reply wait attribute differently: reply (no error), protocol timeout, epoch teardown (`ErrConnClosed`, does not count), caller cancellation (returns the caller's error, does not count).

The timeout branch is narrower than it looks. Both T3 (data) and T6 (control) return their timeout error, but the counter increment sits **inside an `isData` check** — so a control-transaction T6 expiry increments nothing. The data-error counter is exactly that: a *data* counter.

# Invariants

- Closing a connection mid-transaction must never increment the cumulative data-error counter. This is the reason `isCountedSendErr` exists instead of a bare `err != nil`.
- A NotSelected drop is counted once, in its own counter, and never in the error counter — the two answer different questions ("was the peer unreachable?" vs "did the app send while down?").
- A fire-and-forget (W-bit clear) data send records send+1, error+0 by construction: it returns as soon as the frame is on the wire and can never reach a timeout branch.
- Only data transactions touch the data-error counter. A control T6 expiry is a protocol event, surfaced to the caller as `ErrT6Timeout` and counted nowhere in this counter — moving the increment outside the `isData` check would make linktest failures look like application errors.
- The S9F9 notification is best-effort by design; failing to send it must not replace or mask the timeout error the caller receives.

# Failure modes

- **Adding a new error path without classifying it** defaults it to "counts", so a lifecycle event starts inflating the transaction error rate — the metric drifts in the direction that looks like equipment trouble.
- **Counting caller cancellation** makes every client-side abort look like a peer failure, which is backwards for diagnosing a slow tool.
- **Surfacing an S9F9 send failure** would replace an actionable T3 timeout with a confusing secondary error.

# Where to look

- the classification policy: `hsms/connection_send.go` → `isCountedSendErr`
- the four terminal branches and their attribution: `hsms/connection_send.go` → `(*connection).sendWaitReply`
- the timeout notification: `hsms/connection_send.go` → `(*connection).sendAutoS9F9`
