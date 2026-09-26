---
type: Mechanic
title: Who owns the reconnect retry after a failed Start
description: How exactly one reconnect loop ends up retrying when a transport's Start fails, whether the link never came up or came up first, and what the startReturned barrier, the TCP-up marker, and the Reconnecting gauge each contribute.
tags: [hsms, reconnect, lifecycle, generations, metrics]
status: stable
generated: {by: "claude/opus-5.5", at: 2026-09-26T07:45:27Z}
verified:
  - {by: "openai/gpt-5.6-terra", at: 2026-09-26T07:51:47Z}
sources:
  - {resource: hsms/connection_lifecycle.go, digest: sha256:55dd3be61ec99d15, revision: 4be2062}
  - {resource: hsms/epoch.go, digest: sha256:5d48c9656ae715cc, revision: 4be2062}
  - {resource: hsms/connection.go, digest: sha256:d45005d0dcf9540c, revision: 4be2062}
  - {resource: hsms/connection_metrics.go, digest: sha256:4a106e75d9f7803b, revision: 4be2062}
  - {resource: hsms/supervisor.go, digest: sha256:3eaeab8b7da4685b, revision: 4be2062}
  - {resource: hsms/transport.go, digest: sha256:4bd6612b639eca21, revision: 4be2062}
---

# What it does

Answers: *when a transport's `Start` fails, which reconnect loop retries, and how is a second one kept out?*
The godoc of `connectLoop`, `connectLoopStartFailure`, `tcpUpCommitGate`, `epoch.startReturned`,
`ConnectionMetrics.Reconnecting`, and the transport's `Start` each describes its own piece.
None of them states that the hand-off only works because `injectDisconnect` checks identity and not `ended`,
that every path which calls `Start` must close that generation's barrier or park its successor for good,
or that the hand-off loop counts as a reconnect whatever the handing-off loop's own flag was.
`Reconnecting`'s doc says it reads 1 while a loop retries;
in fact it drops to 0 for a moment at every hand-off.

# How it works

**Two kinds of failed Start.**
A `Start` can fail before the link ever came up (a dial or listen failure),
or after it already reported TCP-up and then returned an error, which only a custom transport can do.
In the second case the drop reaction may already have started a loop of its own for the same link.
The generation records which case happened:
`tcpUpCommitGate` latches `epoch.tcpUpCommitted` when an admitted TCP-up commit wins its CAS,
on the generation it validated, while still holding `genGate.RLock`.

**The failing loop decides only after teardown.**
`connectLoopStartFailure` tears the failed generation down and waits for that teardown before reading the marker.
Teardown's `markEnded` takes `genGate`'s write lock,
so it waits out a TCP-up commit still inside its read-locked section,
and any later commit is refused as not live.
The marker is therefore final when it is read.

- Marker false: the link never came up, and no drop reaction can have fired for it.
  The loop closes the generation's `startReturned` barrier and retries in the same invocation,
  still the only owner of the retry and of the gauge.
- Marker true: the link came up.
  The loop releases its gauge share, then closes the barrier,
  then reports the drop with `injectDisconnect(e.id, err, CauseUnknown)` and returns.
  If the transport's own `TCPDown` was processed first, that report does nothing:
  the event is illegal from `NotConnected`,
  or it names a generation a successor has already replaced.
  Otherwise the report drives the drop, the drop fires `react`,
  and `react` starts the loop that now owns the retry.

**The successor waits for the predecessor's `Start`.**
Every loop begins in `connectLoopAwaitBarrier`:
it waits for `prev.wait()` unconditionally — that teardown wait is not interruptible by cancellation —
and only then for `prev.startReturned` or for `reconnectCancel` to close.
Only after that does it claim the gauge, pick its starting delay, and dial.
So a loop that `react` started while the predecessor's `Start` was still running
does nothing until that `Start` has returned and the failing loop has made its decision.
At most one loop ever advances `reconnectDelay` or publishes a successor.

**Who closes `startReturned`.**
The goroutine that called `Start` for that generation closes it exactly once:

- `connectLoop`, on success, after releasing its gauge share;
- `connectLoopStartFailure`, on both branches, after teardown;
- `Open`, on success;
- `Open`'s cold-active branch, after teardown, before it starts the background loop;
- `rollbackFailedOpen`, after the reconnect fence and the `reconnectCancel` close, before the teardown.

**The initial generation is different.**
When `Open`'s own `Start` fails after TCP-up, there is no hand-off.
`Open` latches the generation ended first,
and the cold-active background retry now also requires the marker to be false,
so this case always takes `rollbackFailedOpen` and the Open fails.
The rollback sets `shutdown` and bumps `reconnectGen` under `publishMu`,
closes `reconnectCancel` so a loop waiting in its barrier or backoff wakes,
and waits on `connectLoopWg` before returning,
so a loop `react` started for that drop cannot outlive the failed Open.

**What the hand-off loop inherits.**
It is `react`'s loop, started with `countReconnect` true,
so its success counts in `Reconnects` even when the loop that handed off was the cold-start retry that does not count.
It resets or keeps the backoff from the failed generation's `reachedSelected`, like any other loop,
and otherwise carries on from the delay the failing loop already stored.
`react` skips spawning it when it observes `shutdown` set,
but the check and `startConnectLoop` are separate steps, so a Close landing between them still gets a loop.
That loop captured the pre-Close `reconnectGen` and `reconnectCancel`:
the closed cancel wakes it from its barrier or backoff,
the shutdown and generation fence refuses it before it publishes,
and Close's `connectLoopWg.Wait` joins it.

# Invariants

- The failing loop reads `tcpUpCommitted` only after `teardown` has latched `ended` on that generation.
  A read before teardown can see false for a commit still inside its read-locked section,
  and the loop then retries a link that did come up —
  the two-loop race, or a `NotSelected` state left on a dead link, that this mechanism exists to prevent.
- The loop handing off releases its gauge share before it closes its generation's barrier.
  A successor claims its share only after the barrier releases, so the gauge never reads 2.
- The hand-off report names the failed generation,
  and `injectDisconnect` checks only that the name matches `cur`, not whether that generation has ended.
  At hand-off time the generation is ended but still current, so the report is accepted.
- Every path that calls `tr.Start` closes that generation's `startReturned` exactly once.
- `tcpUpCommitGate` admits at most one TCP-up per generation, and gates a gen of 0 on liveness,
  so `tcpUpCommitted` is set only by a commit on a live generation.

# Failure modes

- **`injectDisconnect` gains an `ended` check.**
  The hand-off report is dropped as stale.
  If no `TCPDown` had already driven the drop, nothing fires `react`:
  no successor loop starts, the state stays at `NotSelected` on a dead link, and `Reconnecting` reads 0.
- **A new path calls `Start` but never closes `startReturned`.**
  The next loop for that generation's drop waits at the barrier indefinitely.
  Only `reconnectCancel`, closed by Close or a failed Open's rollback, releases it.
  From outside, the link stays down after a drop and `Reconnecting` reads 0.
- **The failing loop retries whatever the marker says.**
  This is the old behaviour:
  two loops share `reconnectDelay`, both publish a successor,
  and the second overwrites a live generation that Close then never tears down.
- **The gauge is released after the barrier instead of before.**
  A successor that is already waiting can claim its share first, and `Reconnecting` briefly reads 2.

# Gotchas

- `Reconnecting` can read 0 for a moment while a retry is still pending.
  Between the failing loop's release and the successor's claim,
  the drop must still go through the supervisor queue and `react`, or through the successor's own barrier wait.
  A test that samples the gauge across a hand-off has to allow this gap.
- An unnamed (gen 0) TCP-up, as secs1 and any transport using only `TransportRuntime` report it,
  is charged to whichever generation is current.
  The generation-aware methods are exported on the runtime handed to `Start`,
  so an out-of-module transport can reach them by a structural type assertion and name its generation instead.
  A late report from an older link, landing while a fresh generation is live and still `NotConnected`,
  commits `NotSelected` there, sets that generation's marker, and uses up its one TCP-up admission.
  If that generation's own `Start` then fails, the loop takes the hand-off path for what was really a plain dial failure.
  The retry still happens, now owned by the drop reaction's loop.
- The hand-off branch never runs for the shipped transports:
  hsmsss and secs1 never return an error from `Start` after TCP-up was accepted,
  so their failed `Start` always takes the marker-false branch.
  The barrier wait and the gauge ordering apply to every transport.

# Where to look

- the decision: `hsms/connection_lifecycle.go` → `(*connection).connectLoopStartFailure`
- the barrier wait and gauge claim: `hsms/connection_lifecycle.go` →
  `(*connection).connectLoopAwaitBarrier`, `(*connection).connectLoop`
- the marker and barrier fields: `hsms/epoch.go` → `epoch.tcpUpAdmitted`, `epoch.tcpUpCommitted`, `epoch.startReturned`
- where the marker is set: `hsms/connection_lifecycle.go` → `(*connection).tcpUpCommitGate`;
  `hsms/supervisor.go` → `(*supervisor).commitFrom`, `supervisor.tcpUpCommitGate`
- the hand-off report: `hsms/connection_lifecycle.go` →
  `(*connection).injectDisconnect`, `(*connection).react`, `(*connection).startConnectLoop`
- the initial-generation path: `hsms/connection_lifecycle.go` → `(*connection).Open`, `(*connection).rollbackFailedOpen`
- the cancel channel and loop join: `hsms/connection.go` → `connection.reconnectCancel`, `connection.connectLoopWg`
- the gauge: `hsms/connection_metrics.go` → `ConnectionMetrics.Reconnecting`
- the transport contract: `hsms/transport.go` → `transport`
