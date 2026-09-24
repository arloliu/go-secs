---
type: Mechanic
title: Reconnect backoff scope — what resets it, and what doesn't
description: Where the reconnect delay is persisted across separate connectLoop invocations, and which drop actually resets it to the configured initial value.
tags: [hsms, reconnect, backoff, lifecycle, generations]
status: draft
generated: {by: "claude/sonnet-5", at: 2026-09-24T12:00:00Z}
sources:
  - {resource: hsms/connection.go, digest: sha256:82d716dbf253b02c, revision: b0c8081}
  - {resource: hsms/connection_lifecycle.go, digest: sha256:221b0f7825783fad, revision: b0c8081}
  - {resource: hsms/connection_config.go, digest: sha256:1dd3eb7cbc113324, revision: b0c8081}
  - {resource: hsms/connection_runtime.go, digest: sha256:31ed96e7c5aa678b, revision: b0c8081}
  - {resource: hsms/connection_metrics.go, digest: sha256:b7722fa7d5fa1dc7, revision: b0c8081}
  - {resource: hsms/supervisor.go, digest: sha256:1a4e9385175f90e9, revision: b0c8081}
  - {resource: hsms/epoch.go, digest: sha256:bc9fdafa3dff0a36, revision: b0c8081}
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
right after `prev.wait()` returns (the predecessor generation is fully torn down and joined),
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
`step` call's plain Store —
the commit still lands, but `step`'s plain Store then overwrites it before `step` ever reports `Selected` —
or a Deselect-then-reselect can coalesce against an already-`Selected` `lastReacted`.
Keying the reset on the reaction would silently under-reset in exactly those cases.
The marker is instead set INSIDE THE SAME `genGate.RLock` critical section as the Select CAS itself —
see `connection.selectCommitGate` —
on the captured `cur` that section validated, never by re-resolving `c.cur` and never by calling `liveEpoch`.
Because a successor can only be published after this generation's teardown has both latched `ended`
(under the same gate) and completed its join,
a marker set while `ended` reads false is guaranteed to be on the generation `connectLoop` is about to read
`prev.reachedSelected` from — there is no window where it lands on the wrong epoch.

**The gen-0 liveness gate.**
`selectCommitGate` gates a gen of 0 too,
unlike `commitGate` (still used by `CommitConnected` / `CommitSelectLost`, which bypass gen 0 entirely —
the pre-generation behavior).
secs1 and any out-of-module transport always call `CommitSelected()` with gen 0,
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

- `connection.reconnectDelay` is written only by `Open` (before any loop can be running, per the G1 join)
  and by `connectLoop` (one loop runs at a time per connection cycle, except the overlap described in Gotchas).
- `epoch.reachedSelected` is set only inside the `genGate.RLock` section that also runs the Select CAS,
  on the epoch that section validated, and is never cleared.
- A `connectLoop` invocation's OWN in-loop dial failures always grow
  (their predecessor, if any, never selected by construction) —
  this is unchanged from before this mechanic existed.

# Failure modes

- Removing the `prev.reachedSelected` reset check:
  the backoff mistakes a generation that never selected for one that did, or vice versa —
  verified by mutation testing
  (removing it fails the "selected predecessor resets" and "reset on a Select commit without a reaction" cases).
- Removing `Open`'s reseed: a fresh Open cycle inherits a prior cycle's grown delay instead of starting over.
- Moving the marker STORE outside the `genGate.RLock` section the CAS runs in
  reopens the exact ABA window the gate exists to close:
  a teardown could latch `ended` and a successor could be published between the CAS and the marker store,
  landing the marker on the wrong generation.
  The READ in `connectLoop` is deliberately outside that section and is safe:
  it happens after the predecessor is joined,
  and once teardown has latched `ended` no further Select commit can succeed on that epoch,
  so the marker can no longer change.

# Gotchas

- **Overlapping reconnect loops when `tr.Start` fails after TCP-up (pre-existing, not fixed by this mechanic).**
  `connectLoop`'s dial-failure branch retries (`continue`) after tearing down an epoch whose `Start`
  returned an error;
  if that `Start` had already driven TCP-up and a subsequent disconnect,
  `react` has already launched a SECOND loop by the time the first one's `continue` runs,
  and both loops end up sharing `connection.reconnectDelay` and racing to publish a successor.
  `Open`'s cold-active branch has the same precondition
  (`State() == NotConnectedState` observed after a TCP-up-then-drop).
  Neither shipped transport can actually reach this:
  hsmsss's active `Start` returns nil once `tcpUp` accepts the generation,
  hsmsss's passive `Start` has no error exit after it spawns the accept goroutine,
  and secs1's `Start` returns nil right after its synchronous TCP-up-plus-Select-commit.
  Only a custom or mock transport whose `Start` can fail after already driving TCP-up can hit this,
  and there two loops sharing the persisted delay is no worse than the competing-publish races that
  already exist for that shape of transport.

# Where to look

- the persisted delay: `hsms/connection.go` → `connection.reconnectDelay`;
  `hsms/connection_lifecycle.go` → `Open`, `connectLoop`, `nextBackoffDelay`
- the reset marker: `hsms/epoch.go` → `epoch.reachedSelected`;
  `hsms/connection_lifecycle.go` → `connection.selectCommitGate`
- the gate CommitSelected now goes through even at gen 0: `hsms/supervisor.go` → `supervisor.selectGate`,
  `supervisor.commitFrom`, `CommitSelected`, `CommitSelectedFromGeneration`;
  `hsms/connection_runtime.go` → `commitSelectAccepted`
- config knobs: `hsms/connection_config.go` → `WithReconnectBackoff`, `WithT5`
- the counted-reconnect distinction this mechanic does not change: `hsms/connection_metrics.go` →
  `ConnectionMetrics.Reconnects`
