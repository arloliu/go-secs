---
type: Mechanic
title: How a reply wait ends with at most one result
description: The one-shot claim every result must win before it reaches a waiting sender, what each losing candidate reports, and how the sender's select arms turn the winner into a return value.
tags: [hsms, reply-correlation, concurrency, s9, sxf0]
status: stable
generated: {by: "claude/opus-5.5", at: 2026-10-23T18:00:00Z}
verified:
  - {by: "openai/gpt-6-astra", at: 2026-10-23T18:42:42Z}
sources:
  - {resource: hsms/reply_registry.go, digest: sha256:8f6b52cc0e84f7db, revision: 2a92a5e}
  - {resource: hsms/connection_send.go, digest: sha256:ea94ab4fc9d25bb9, revision: 2a92a5e}
  - {resource: hsms/connection_runtime.go, digest: sha256:7f242f475b188a1b, revision: 2a92a5e}
  - {resource: hsms/session.go, digest: sha256:04cc770917f02f59, revision: 2a92a5e}
  - {resource: hsms/connection_config.go, digest: sha256:ce0ac428789edddd, revision: 2a92a5e}
  - {resource: hsms/reply_registry_test.go, digest: sha256:46ed00a4436d8e37, revision: 2a92a5e}
  - {resource: hsms/reply_matching_test.go, digest: sha256:4fd21a7d86752c61, revision: 2a92a5e}
  - {resource: hsms/s9_correlation_test.go, digest: sha256:a3de9b08c23b6b18, revision: 2a92a5e}
  - {resource: hsms/abort_reply_error_test.go, digest: sha256:03ed70180a5e3879, revision: 2a92a5e}
---

# What it does

The `replyWaiter` and `route` comments in `hsms/reply_registry.go` define the claim states and each loser's return value,
and `WithS9Correlation`'s godoc states that a reply losing to an S9 reaches the handlers as an orphan secondary.
What neither lays out is the whole race:
a reply, a duplicate, a peer Reject.req, and an S9 error message can all target one open wait,
from the receive goroutine while the sender sits in a four-arm select,
and the claim decides which of them, at most one, is published to the sender.
Nor do they say where each loser ends up,
why mismatch detection runs before the claim,
or how `WithAbortReplyError` rewrites the winner on every arm that can return it.
Two comments are imprecise:
the `replyRegistry` comment says a reply arriving after deregister is dropped,
but a missed data secondary falls through to `routeDataOn`;
and the `RouteReply` comment explains duplicates through the buffer, while the claim state decides them.
The two-legged comparison that runs ahead of the claim is [the control exemption](/hsms/reply-matching-control-exemption.md);
the inbound path that produces the S9 claimant is [S9 correlation](/hsms/s9-correlation.md).

# How it works

**One claim per registration.**
`registerData` and `registerControl` store a fresh `*replyWaiter` with a one-slot channel and `state == replyOpen`.
The registry holds waiters by pointer, so every `Load` sees the same `state`.
A result reaches the channel only after a compare-and-swap moves `state` away from `replyOpen`:
`route` swaps to `replyClaimedByReply` for a reply or a peer Reject.req,
and `failS9` swaps to `replyClaimedByS9`.
The winner sends non-blockingly;
the claim guarantees the buffer is empty, so that send always lands.
The claim does not guarantee the sender returns that result:
the sender may already have left through the caller-context arm, a write failure, or a timeout.

**What `route` checks before it claims.**
A key miss returns `(false, false)`.
On a data registration, a `*ControlMessage` result is a miss,
and a `*DataMessage` result is compared, setting `mismatched` on a stream or function difference.
A strict-mode mismatch returns `(false, true)` without claiming.
Both returns leave the claim state unchanged; an open wait stays open for a conforming reply.
Only a candidate that survives these checks attempts the claim.

**What a loser reports.**
A candidate that loads the waiter but finds it already claimed sends nothing, and its result depends on the winner:
after a reply won, `route` reports `(true, mismatched)`, so the duplicate is consumed and dropped;
after an S9 won, it reports `(false, mismatched)`, so `routeReplyOn` returns false
and `deliverOwnedFrameOn` hands the genuine reply to `routeDataOn` as an orphan secondary.
An S9 that finds the wait claimed makes `failS9` return false,
but the S9 is routed to the data handlers in either case.
These outcomes read `state`, never the buffer, so they do not depend on whether the sender has drained the winner.
Once the sender has deregistered, a lookup misses with `(false, false)` and no comparison,
whoever won the claim, so a late data secondary falls through to the data handlers.

**Mismatch counting.**
Comparison precedes the claim, so `mismatched` is reported whatever the claim's outcome.
`routeReplyOn` increments `ReplyMismatchCount` after `route` returns,
including for a candidate that lost the claim, so a mismatched duplicate is counted.

**The sender's side.**
`sendWaitReplyOn` selects over the reply channel, the T3/T6 timer, the epoch's `ctx.Done()`, and the caller's context.
The timer and teardown arms first call `tryReply`, so a result already published beats a timeout or teardown that won Go's random select pick.
The caller-context arm does not re-check: it returns `callerCtx.Err()` even when a result is waiting in the buffer.
Every arm that returns a received result goes through `replyOutcome`.
When the send registered a data primary and `WithAbortReplyError` was on as the wait began (`abortErr`, read once),
`replyOutcome` returns a function-0 `*DataMessage` with `ErrTransactionAborted`;
it reads only the header, and leaves a `*RejectError` or `*S9Error` result untouched.
`session.SendDataMessage` and `session.SendSECS2Message` then return the SxF0 message with the abort error before their own body-decode check,
so a malformed SxF0 body never replaces the abort.

**Deregistration does not close.**
The sender's deferred `deregister` deletes the key and never closes the channel.
A receiver that loaded the waiter just before that delete can still claim the open wait and send into the abandoned buffer;
it reports delivered, and the result is collected with the channel.

# Invariants

- At most one result is ever sent on a waiter's channel, so the one-slot buffer never holds a second one.
- The waiter is stored and loaded by pointer; a value copy would carry its own `state` and let two results complete one wait.
- A strict-mode mismatch and a data registration's `*ControlMessage` miss return before the claim and leave the claim state unchanged.
- `failS9` matches only a data waiter (`w.isData`), so an S9 never ends a Select, Deselect, or Linktest wait.
- A reply that loses to an S9 is reported as a miss, never as delivered, so it is not silently dropped.
- `abortErr` is computed once per wait, before the select, and only for a data transaction.

# Failure modes

- **Dropping the claim** lets two results race into the one-slot buffer:
  the loser's non-blocking send hits a full buffer and vanishes, or, after the sender drained the winner, lands unread.
  A genuine reply then disappears instead of reaching the handlers;
  `TestReplyRegistry_ConcurrentCandidatesSendOnce` and `TestReplyRegistry_S9AfterReply` catch it.
- **Reporting every lost claim as delivered** swallows a genuine reply that loses to an S9;
  `TestReplyRegistry_CandidateAfterClaim` and `TestReplyMatching_GenuineReplyAfterS9ReachesHandler` catch it.
- **Skipping comparison for an already-claimed waiter** makes a mismatched duplicate invisible to `ReplyMismatchCount`,
  caught by `TestReplyMatching_MismatchAfterClaimStillCounted`.
- **Dropping the timer or teardown arm's `tryReply` re-check** returns `ErrT3Timeout` or `ErrConnClosed` for a send whose reply, abort, or S9 already won,
  caught by `TestS9Correlation_ArmThenS9`, `TestAbortReplyError_TimerArmThenAbort`, and `TestAbortReplyError_TeardownArmThenAbort`.
- **The caller-context arm selected while a result is buffered** returns `callerCtx.Err()` without consuming that result,
  which is discarded with the channel.
  Cancellation after publication does not guarantee this outcome: the ready reply arm can win the select instead.
- **Returning a function-0 reply through a path that skips `replyOutcome`** reports an SxF0 as a plain reply with `WithAbortReplyError` on;
  the transaction observer then reports `TxReplied` instead of `TxAborted`.

# Where to look

- the claim states, the claim, and each loser's report: `hsms/reply_registry.go` → `replyWaiter`, `(replyRegistry).route`, `(replyRegistry).failS9`
- registration by pointer: `hsms/reply_registry.go` → `(replyRegistry).registerData`, `(replyRegistry).registerControl`, `(replyRegistry).store`
- the select, the arms' re-check, and the abort rewrite: `hsms/connection_send.go` → `(*connection).sendWaitReplyOn`, `tryReply`, `replyOutcome`
- the mismatch count and where a missed reply falls through: `hsms/connection_runtime.go` → `(*connection).routeReplyOn`, `(*connection).deliverOwnedFrameOn`
- the abort's precedence over a decode error: `hsms/session.go` → `(*session).SendDataMessage`, `(*session).SendSECS2Message`
- the orphan-secondary contract: `hsms/connection_config.go` → `WithS9Correlation`
- claim tests: `hsms/reply_registry_test.go` → `TestReplyRegistry_CandidateAfterClaim`, `TestReplyRegistry_S9AfterReply`
- mismatch and orphan tests: `hsms/reply_matching_test.go` → `TestReplyMatching_MismatchAfterClaimStillCounted`
- arm re-check tests: `hsms/s9_correlation_test.go` → `TestS9Correlation_ArmThenS9`
- abort arm tests: `hsms/abort_reply_error_test.go` → `TestAbortReplyError_TimerArmThenAbort`, `TestAbortReplyError_TeardownArmThenAbort`
