---
type: Mechanic
title: Reconnect backoff scope — what resets it, and what doesn't
description: Where the reconnect delay is persisted across separate connectLoop invocations, and which drop actually resets it to the configured initial value.
tags: [hsms, reconnect, backoff, lifecycle, generations]
status: stable
generated: {by: "claude/opus-5.5", at: 2026-09-26T11:59:50Z}
verified:
  - {by: "openai/gpt-5.6-terra", at: 2026-09-26T12:04:05Z}
sources:
  - {resource: hsms/connection.go, digest: sha256:3c8a78070c937d5a, revision: 7ae1ff0}
  - {resource: hsms/connection_lifecycle.go, digest: sha256:8a7a71d56304a338, revision: b43b798}
  - {resource: hsms/connection_config.go, digest: sha256:e701533ea6c49f0a, revision: c00e1b5}
  - {resource: hsms/connection_runtime.go, digest: sha256:7340a3887598a8ba, revision: b43b798}
  - {resource: hsms/connection_metrics.go, digest: sha256:dae358846f98653e, revision: f7a5927}
  - {resource: hsms/supervisor.go, digest: sha256:097ae581f965d935, revision: b43b798}
  - {resource: hsms/epoch.go, digest: sha256:bdf3578cbc24fc48, revision: 7ae1ff0}
  - {resource: secs1/transport.go, digest: sha256:399009fc96b7bf6c, revision: 4be2062}
---

# What it does

`WithReconnectBackoff`'s godoc states the per-attempt shape (initial, multiplier, T5 ceiling),
and describes the wait as growing across separate reconnect attempts,
including a generation that never reaches Selected,
resetting to initial only once a connection reaches Selected.
It does not say WHERE that growth is stored, WHO resets it,
or what "reaches Selected" means operationally
when a Select commit can land without ever producing a state-change reaction.
That is the delta this entry records.

# How it works

**The delay lives on the connection, not on the loop.**
`connection.reconnectDelay` (`atomic.Int64`, a `time.Duration`) is the one piece of state that survives
across separate `connectLoop` invocations —
each involuntary drop that never selects gets its own fresh `connectLoop` call
(launched by `react` → `startConnectLoop`),
and without a connection-scoped store each call would start from nothing.

**Two writers, one reader-then-decider.**
`Open` stores `cfg.reconnectBackoffInitial` right after loading `cfg`, unconditionally, on every Open cycle —
including the cold-active background retry (Gap 1) that Open itself launches when the very first dial fails.
`connectLoop` is the other writer:
after every successful `reconnectSleep`, it advances the delay with `nextBackoffDelay`
(using that iteration's own config snapshot) and stores the result back immediately, before the next fence check.
`connectLoop` is also the reader:
right after `prev.wait()` returns and the predecessor's `startReturned` barrier has released
(the predecessor's teardown completion signal has arrived, and its own `Start` call has returned;
task joins use deadlines, so timed-out goroutines may remain, and `connectLoop` ignores the returned error,
though teardown completion itself has no unconditional time bound —
the transport's `Stop` runs synchronously and its unbounded accept join can delay it;
the marker it reads is stable regardless, because `ended` was latched under `genGate`),
it decides the STARTING delay for this invocation —
either the persisted value, or a reset to the freshly-loaded initial delay.

**The reset condition is `prev.reachedSelected`, not a state-change reaction.**
`epoch.reachedSelected` (`atomic.Bool`) latches true the instant a Select commit succeeds for that generation.
`connectLoop` resets to initial when `prev != nil && prev.reachedSelected.Load()`,
or defensively when the persisted delay is `<= 0`.
A generation that comes up and drops again WITHOUT ever setting this —
a peer that answers every Select.req with a non-zero status, for instance —
leaves the delay exactly where the last iteration left it,
so the ramp keeps climbing across that fresh `connectLoop` invocation instead of restarting.

**Why the marker is not driven off `react`'s deduped reaction.**
`react` only fires for a transition `step` dedupes against `lastReacted`,
and a successful Select commit does not always produce one:
the Select commit's CAS can land in the window between `step`'s own state load for a disconnect and that
`step` call's store —
the commit still lands, but the store (a `Swap`) then overwrites it before `step` ever reports entering `Selected`;
the drop is reported with `prev == Selected`, but no entering-`Selected` reaction ever fires,
and the commit's own `evSelectAccepted` later finds `NotConnected` and fires nothing —
or a Deselect-then-reselect can coalesce against an already-`Selected` `lastReacted`.
Keying the reset on the reaction would silently under-reset in exactly those cases.
The marker is instead set INSIDE THE SAME `genGate.RLock` critical section as the Select CAS itself —
see `connection.selectCommitGate` —
on the captured `cur` that section validated, never by re-resolving `c.cur` and never by calling `liveEpoch`.
A successor is published only after this generation's teardown has both latched `ended`
(under the same gate) and completed its join,
and after this generation's own `Start` call has returned (the `startReturned` barrier),
so a marker set while `ended` reads false is guaranteed to be on the generation `connectLoop` is about to read
`prev.reachedSelected` from — there is no window where it lands on the wrong epoch.

**The gen-0 liveness gate.**
`selectCommitGate` gates a gen of 0 on liveness, as `commitGate` and `tcpUpCommitGate` do
(see [how the three synchronous commits are fenced](/hsms/synchronous-commit-gate.md)).
secs1 and any transport using only `TransportRuntime` call `CommitSelected()` with gen 0,
so gating only named generations would leave the reset marker unreachable for them.
`selectCommitGate` admits a gen-0 commit whenever a live, un-torn-down `cur` exists,
skipping only the identity comparison a named generation still has to pass.
A gen-0 commit against an already-ended generation is refused and counted in `staleGen`,
the same outcome a refused named commit gets.

**Config sampling.**
The persisted delay is never rewritten by `UpdateConfigOptions`.
Each `connectLoop` iteration loads one config snapshot,
sleeps `min(persisted, T5)` under it,
and advances with THAT snapshot's multiplier and T5 —
a config change mid-sleep never shortens the pending timer,
and a changed `initial` only takes effect at the next reset (a selected predecessor, or `Open`).

**Passive re-listen damping.**
For a passive connection, `tr.Start` is a re-listen, not a dial.
The persisted delay rate-limits how fast a NotConnected → re-listen cycle repeats —
for example a peer that connects and never selects, dropped by the T7 dwell —
exactly the same damping a repeated dial failure already gets.
This is intentional, not an oversight:
without it, a peer that never selects would drive a re-listen cycle
as fast as the T7 dwell and the accept/handshake round-trip allow.

# Invariants

- `connection.reconnectDelay` is written only by `Open` (before any loop can be running, because `Open` first joins any dying reconnect loop)
  and by `connectLoop`.
  Successive retry/publication sequences are serialized for any transport,
  although loop goroutine lifetimes can overlap:
  `react` can launch the next loop while the preceding one is still inside its `tr.Start`,
  but the new loop does not read or write the delay until that predecessor's `startReturned` barrier releases.
  A loop that published the dropped generation closes that barrier only on a path that then returns
  (success, or the hand-off below), so the close follows its last delay write.
  A loop whose `Start` fails after TCP-up hands the retry to the drop reaction's loop and returns
  instead of retrying (see [who owns the retry after a failed Start](/hsms/reconnect-retry-ownership.md)).
- `epoch.reachedSelected` is set only inside the `genGate.RLock` section that also runs the Select CAS,
  on the epoch that section validated, and is never cleared.
- The predecessor's `reachedSelected` marker is checked only once, before the retry loop begins.
  A `connectLoop` invocation's own subsequent failed `Start` attempts retain the advanced local delay
  without repeating that check,
  even when the invocation's own predecessor did reach Selected.
  Whether the delay actually grows or plateaus across those attempts depends on the configured
  multiplier and the T5 ceiling —
  a multiplier of 1.0, or a delay already at T5, holds it flat instead of growing further.

# Failure modes

- Removing the `prev.reachedSelected` reset check:
  a predecessor that DID reach Selected no longer resets the ramp,
  so the delay keeps growing from where the dead predecessor left it —
  mistaking a selected generation for one that never selected.
  It does not introduce the reverse mistake:
  an unselected predecessor still leaves the delay exactly where it was, growth unaffected.
  Covered by the "selected predecessor resets" and "reset on a Select commit without a reaction" tests.
- Removing `Open`'s reseed: a fresh Open cycle inherits a prior cycle's grown delay instead of starting over.
- Moving the marker STORE outside the `genGate.RLock` section the CAS runs in
  does not by itself redirect it onto the wrong generation —
  the store still writes through the SAME `*epoch` pointer the CAS captured under the lock,
  and delaying the write does not change which pointer that is.
  What it opens instead is a timing race:
  teardown can latch `ended` and complete its join —
  unblocking `connectLoop`'s `prev.wait()` —
  before the delayed store runs,
  so `connectLoop` reads `prev.reachedSelected` as false and fails to reset a ramp that should have reset.
  Landing the marker on a SUCCESSOR generation instead would additionally require the delayed code to
  re-resolve `c.cur` rather than writing through the pointer already captured.
  The READ in `connectLoop` is deliberately outside that section and is safe:
  it happens after the predecessor's teardown has signalled completion,
  and once teardown has latched `ended` no further Select commit can succeed on that epoch,
  so the marker can no longer change.

# Gotchas

- **A `tr.Start` that fails after TCP-up no longer leaves two loops sharing the delay.**
  `connectLoop`'s Start-failure branch used to retry after tearing down an epoch whose `Start`
  had already driven TCP-up and a disconnect,
  while `react` had already launched a second loop for that drop;
  both then advanced `connection.reconnectDelay` and raced to publish a successor.
  Now the failing loop tears the generation down first, reads its `tcpUpCommitted` marker,
  and, when the link had come up, hands the retry to the drop reaction's loop and returns;
  that loop waits at the failed generation's `startReturned` barrier,
  so only one loop ever advances the delay
  (see [who owns the retry after a failed Start](/hsms/reconnect-retry-ownership.md)).
  `Open`'s cold-active branch now also requires that no TCP-up commit succeeded on the generation,
  so the same shape fails the Open instead of starting a background retry.
  Neither shipped transport reaches this shape at all:
  hsmsss's active `Start` returns nil once `tcpUp` accepts the generation,
  hsmsss's passive `Start` has no error exit after it spawns the accept goroutine,
  secs1's active `Start` returns nil right after its synchronous TCP-up-plus-Select-commit,
  and secs1's passive `Start` returns nil right after spawning `acceptLoop`,
  which performs that same TCP-up-plus-Select-commit asynchronously once a peer is accepted,
  potentially before or after `Start` returns;
  what matters is that `startPassive` has no error return after launching it.
  Only a custom or mock transport whose `Start` can fail after already driving TCP-up takes the hand-off path.

# Where to look

- the persisted delay: `hsms/connection.go` → `connection.reconnectDelay`;
  `hsms/connection_lifecycle.go` → `Open`, `connectLoop`, `nextBackoffDelay`
- the barrier that serializes the delay's readers and writers: `hsms/connection_lifecycle.go` → `connectLoopAwaitBarrier`,
  `connectLoopStartFailure`; `hsms/epoch.go` → `epoch.startReturned`
- the reset marker: `hsms/epoch.go` → `epoch.reachedSelected`;
  `hsms/connection_lifecycle.go` → `connection.selectCommitGate`
- the gate CommitSelected goes through even at gen 0: `hsms/supervisor.go` → `supervisor.selectGate`,
  `supervisor.commitFrom`, `CommitSelected`, `CommitSelectedFromGeneration`;
  `hsms/connection_runtime.go` → `commitSelectAccepted`
- config knobs: `hsms/connection_config.go` → `WithReconnectBackoff`, `WithT5`
- the counted-reconnect distinction this mechanic does not change: `hsms/connection_metrics.go` →
  `ConnectionMetrics.Reconnects`
- secs1's active-vs-passive `Start` shape referenced in Gotchas: `secs1/transport.go` →
  `(*transport).startActive`, `(*transport).startPassive`, `(*transport).acceptLoop`
