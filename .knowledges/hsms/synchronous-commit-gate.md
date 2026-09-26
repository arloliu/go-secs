---
type: Mechanic
title: How the three synchronous commits are fenced against an ended generation
description: Why TCP-up, Select-accepted, and Select-lost commit under a lock instead of relying on the queue-side generation match, the liveness-and-identity rule every gate applies, the validated id a commit's report carries onto the queue, the refusal each producer reports, and the unnamed calls the fence cannot tell apart.
tags: [hsms, hsmsss, lifecycle, supervisor, generation, concurrency]
status: stable
generated: {by: "claude/opus-5.5", at: 2026-09-26T11:59:29Z}
verified:
  - {by: "openai/gpt-5.6-terra", at: 2026-09-26T12:04:05Z}
sources:
  - {resource: hsms/supervisor.go, digest: sha256:097ae581f965d935, revision: b43b798}
  - {resource: hsms/connection_lifecycle.go, digest: sha256:8a7a71d56304a338, revision: b43b798}
  - {resource: hsms/connection_runtime.go, digest: sha256:7340a3887598a8ba, revision: b43b798}
  - {resource: hsms/connection.go, digest: sha256:3c8a78070c937d5a, revision: b43b798}
  - {resource: hsms/epoch.go, digest: sha256:bdf3578cbc24fc48, revision: b43b798}
  - {resource: hsms/transport.go, digest: sha256:760d41a4a861ea2c, revision: b43b798}
  - {resource: hsmsss/transport.go, digest: sha256:176fff888fc8a85e, revision: b43b798}
  - {resource: hsmsss/transport_control.go, digest: sha256:84353e5b3b34860b, revision: b43b798}
  - {resource: hsmsss/transport_active.go, digest: sha256:33d63b7808dc1ed3, revision: b43b798}
  - {resource: hsmsss/transport_passive.go, digest: sha256:eaee7f67c2e994fc, revision: b43b798}
  - {resource: hsmsss/transport_procedures.go, digest: sha256:bf47bd9825ddb5da, revision: b43b798}
  - {resource: secs1/transport.go, digest: sha256:399009fc96b7bf6c, revision: b43b798}
---

# What it does

`TransportRuntime.TCPUp`, `CommitSelected`, and `SelectLost` document that a commit is refused once the current generation's teardown has begun,
and that a call made after a successor is current applies to that successor.
They do not say why these three commits need a lock rather than the queue-side generation match,
why `ended` is a separate axis from identity,
what each producer does with a refusal,
or how a commit's report is kept off a successor after the lock is released.
That is the delta this entry records;
the queue-side match itself is in [how a queued report from an ended generation is kept off its successor](/hsms/generation-report-fence.md).

# How it works

**The three SYNCHRONOUS commits are not events, so `step`'s match does not cover them, and they need a lock.**
`CommitConnected`, `CommitSelected`, and `CommitSelectLost` compare-and-swap `supervisor.state` directly on the
transport's own goroutine and only THEN enqueue their event.
That synchrony is the §7.D invariant — `IsSelected()` must be true before the responder writes `Select.rsp` —
so it cannot be moved onto the queue.
It also means there is no later point at which the generation answer can be re-taken:
whatever check the caller makes, the CAS follows it on the same goroutine.

The FSM state cannot supply the missing ordering either, which is why the trick `step` uses does not transfer.
`step` reads state BEFORE it checks the generation, and a successor's state implies a successor was published.
A commit's "state read" is fused into its CAS, so the check can only come first —
and `NotConnected -> NotSelected -> ... -> NotConnected` is a real cycle,
so the state a stale CAS expects can legitimately reappear underneath it (plain ABA).

The fence is `connection.genGate`, a `sync.RWMutex`:
`connection.commitGate` (for `CommitSelectLost`), `connection.tcpUpCommitGate` (for `CommitConnected`), or `connection.selectCommitGate` (for `CommitSelected`) takes RLock across {resolve `cur`, verify liveness, CAS},
and `epoch.markEnded` takes Lock for a single atomic store at the top of `epoch.teardown`.
The two are therefore mutually exclusive, and every successor publish is downstream of the join teardown starts,
so a commit that observed its generation un-ended completed its CAS before any successor could exist.
The gate is held across atomic operations and `epoch.connMu` only — never a log call, a channel send, or a Wait —
so it cannot close a cycle against the bounded teardown join.
`supervisor.commitFrom` issues the follow-up `injectFrom` AFTER the gate is released, because `inject` can block on a full queue.

`epoch.ended` is a SEPARATE axis from `epoch.id`, and TCP-up is where the difference shows.
`CommitConnected` CASes out of `NotConnected`, the state a generation that ended leaves behind,
and `cur` keeps pointing at a dead generation for the whole reconnect backoff — after a `Close`, forever.
So the identity of a straggler's TCP-up still MATCHES, and `ended` is what rejects it once teardown has begun.
The other two commits CAS out of states a dead generation is not SUPPOSED to be in, under the shipped
transports' own invariants — but that is an assumption about caller behavior, not a structural guarantee
`ended` enforces independently of it, and a misbehaving custom transport can break it.
`connectLoop`'s Start-error branch (`hsms/connection_lifecycle.go` → `connectLoopStartFailure`) tears the
just-published epoch down directly via `epoch.teardown`, WITHOUT routing through `requestClose`/`evClose`,
so it never resets supervisor state.
A custom `Start` that drives TCP-up (committing `NotSelected`) before returning an error therefore
leaves a DEAD generation sitting at exactly the state a live `CommitSelected` CAS expects —
IDENTITY still matches (`cur` has not yet been replaced), so only the `ended` check rejects a stale
Select CAS landing there.
That window lasts until the branch's own hand-off report (`injectDisconnect`, `CauseUnknown`) drives the drop,
unless a disconnect the transport reported already did.
`ended` must not be treated as generally redundant for a commit based on the expected FSM state alone;
a live current epoch CAN be `NotConnected` during `Open`'s own startup window —
`cur.Store(e)` runs before the TCP-up commit that CASes `NotConnected` to `NotSelected` —
and an ended epoch can remain current in that same state for the whole reconnect backoff, or forever after a `Close`.
Because both a live and a dead generation can occupy the same state pair `CommitConnected` CASes out of,
identity and FSM state cannot replace the `ended` check there either;
the check is not a property specific to some other commits' state pairs,
it is what `CommitConnected` itself relies on too.


**All three gates admit a gen of 0 on liveness, and each returns the id it validated.**
`commitGate` (Select-lost), `selectCommitGate` (Select-accepted), and `tcpUpCommitGate` (TCP-up) share one admission rule:
a non-nil, un-torn-down `cur`, plus an identity match when the caller named a generation.
A gen of 0 skips only that identity comparison.
`tcpUpCommitGate` additionally admits at most one TCP-up per generation (`epoch.tcpUpAdmitted`, latched even when the admitted CAS fails),
so a second report on a still-live generation is refused too; that refusal is logged but not counted in `staleGen`.
On a successful commit `tcpUpCommitGate` latches `epoch.tcpUpCommitted`
(see [who owns the retry after a failed Start](/hsms/reconnect-retry-ownership.md) for its reader),
and `selectCommitGate` latches `epoch.reachedSelected`
(see [reconnect backoff scope](/hsms/reconnect-backoff-scope.md) for its reader),
each on the SAME `cur` the RLock section validated, before releasing the lock.
A non-live rejection, named or unnamed, is counted in `staleGen` and enqueues nothing.
A live failed CAS, including a second TCP-up admission, also enqueues nothing but is not counted as stale.

For a live decision, each gate returns the `id` of the epoch it resolved inside its RLock section (a non-live decision returns zero),
and `supervisor.commitFrom` queues the follow-up event with that id rather than the caller's `gen`.
An unnamed commit therefore reaches the queue bound to the generation it was validated against,
and `step`'s generation match discards it if a successor has replaced that generation by the time it is processed.
Without that, a commit delayed between its gate and its enqueue would be matched against whatever generation is current when `step` reaches it:
a delayed Select-accepted report could store `Selected` on a successor that never ran its own handshake.
The gates are skipped outright only when nil, which happens only in a supervisor built without a connection (unit tests);
that path is a bare CAS and queues the caller's `gen`.

**The refusal is not silent: it is a reported bool, all the way out to the transport.**
`publishSocket`, `commitTCPUp`, and `TCPUpFromGeneration` all return whether the socket was accepted.
It is one of the two producers among the five `genCapability` methods whose caller has real cleanup to do on a
refusal — here the socket itself, not just an event.
`hsmsss`'s `tcpUp` wrapper forwards that bool, and `startActive` / `acceptLoop`
(transport_active.go / transport_passive.go) check it:
on `false` they close the conn themselves —
no epoch will ever own a refused socket, so nobody else ever will —
and skip everything a live TCP-up would start
(the recv loop, the active Select procedure, the transport's own `t.conn` / activity-stamp bookkeeping),
so a dead generation's socket can never clobber a live successor's.
`commitTCPUp` still runs the FSM commit unconditionally even on a `publishSocket` refusal, purely so
`staleGen` keeps counting it — the commit itself is a guaranteed no-op there, since `ended` never reverts.
The plain `TCPUp` (gen 0, for a transport using only `TransportRuntime`) keeps its void signature and unconditional socket publication;
only the generation-named path can name a refusal.
Its FSM commit is gated even so (`tcpUpCommitGate`), but its socket publication is not:
`publishSocket` has no liveness check at gen 0,
so a gen-0 report landing after its generation's teardown already closed the socket can re-populate it,
and nothing closes it afterward — a limitation the `TCPUp` godoc states.

**Select-lost is the other reported refusal, and what it protects is narrow.**
`SelectLostFromGeneration` returns whether the CAS was applied,
and `hsmsss`'s `selectLost` wrapper forwards that to `handleDeselectReq`.
The work skipped on a refusal is the pair of calls that belong to a real `Selected -> NotSelected` transition,
`stopLinktest` and `armT7`.
`stopLinktest(g)` and `armT7(g)` both take the caller's bundle explicitly
and touch ONLY `g`'s own `linktestCancel`/`t7Cancel`/`timerMu` fields (see `genWG` in `hsmsss/transport.go`):
even an UNCONDITIONAL call from a straggler cannot reach a live successor's timers,
because there is no shared, transport-wide handle for it to race into.
`selectLost`'s guard on the CAS result therefore does not fence a straggler from a successor's timers —
the per-generation timer handles do that at a different layer —
it only SAVES needless work on a refused commit (arming/cancelling a stale generation's own, already-dead bundle).
The FSM cannot observe the skipped work either way:
a refused commit leaves state exactly as it was —
`NotConnected`, `NotSelected`, or `Selected`, depending on whether the refusal came from a stale generation
(a live successor legitimately Selected) or from the CAS itself failing (the state was never Selected to begin with).

The `Deselect.rsp` is still enqueued BEFORE the commit, and that ordering is deliberate.
The §7.D/I3 invariant rests on the commit being synchronous on the sequential recv goroutine, not on its position relative to the rsp —
the CAS lands before any pipelined re-`Select.req` is dispatched, and `SendAsync` only enqueues.
Deriving the status from the commit instead would break the CAPABILITY-ABSENT transport fallback
(`selectLost`'s plain `t.rt.SelectLost()` branch, taken only when the runtime does not implement
`genRuntime`), which always reports true regardless of the CAS outcome:
a Deselect answered while NOT Selected would be told status 0 and handed a `SelectLost` it never asked for.
In the concrete core, a gen-0 Select-lost goes through `commitGate` like a named one,
so it is refused once the current generation's teardown has begun;
the capability-absent fallback still reports true in that case, and its own timer calls touch only its own bundle.

The remaining two producers stay void.
A refused `TCPDownFromGeneration` or `T7ExpiredFromGeneration` is a queued event whose caller has nothing to undo.


# Failure modes

**A stale Select commit landing on a dead generation.**
A custom `Start` that drives TCP-up and then errors leaves an ended generation current at `NotSelected`,
the exact state a live `CommitSelected` CAS expects;
dropping the `ended` check from `selectCommitGate` lets a straggler's Select commit move that dead link to `Selected`.


**A late unnamed commit applied to the successor.**
An unnamed `CommitSelected()`, `SelectLost()`, or `TCPUp()` made only AFTER the successor is current is validated against the successor
and commits if its remaining guards pass (the state CAS; for TCP-up also the one-admission guard).
The fence covers the window between a commit's validation and its queued report, not the producer's own staleness.

**A gen-0 socket published onto a torn-down generation.**
The plain `TCPUp` publishes its socket without a liveness check,
so a report landing after its generation's teardown already closed the socket re-populates it,
and nothing closes it afterward.


# Where to look

- the fence: `hsms/connection.go` → `genGate`;
  `hsms/connection_lifecycle.go` → `commitGate`, `tcpUpCommitGate`, `selectCommitGate`, `publishSocket`, `commitTCPUp`, `connectLoopStartFailure`;
  `hsms/epoch.go` → `epoch.ended`, `epoch.markEnded`, `epoch.tcpUpAdmitted`, `epoch.tcpUpCommitted`
- the validated id and the follow-up: `hsms/supervisor.go` → `commitFrom`, `commitGate`, `selectGate`, `tcpUpCommitGate`, `testHookBeforeEnqueue`, `CommitConnectedFromGeneration`, `CommitSelectedFromGeneration`, `CommitSelectLostFromGeneration`;
  `hsms/connection_runtime.go` → `commitSelectAccepted`, `commitSelectLost`
- the documented contract: `hsms/transport.go` → `TransportRuntime`
- transport side: `hsmsss/transport_control.go` → `transport.tcpUp`, `transport.selectLost`, `handleDeselectReq`;
  `hsmsss/transport_active.go` → `startActive`;
  `hsmsss/transport_passive.go` → `acceptLoop`;
  `secs1/transport.go` → `causeRuntime`
- per-generation timer handles that limit what `selectLost`'s refusal protects: `hsmsss/transport.go` → `genWG.t7Cancel`, `genWG.linktestCancel`, `genWG.timerMu`;
  `hsmsss/transport_procedures.go` → `(*transport).stopLinktest`, `(*transport).armT7`
