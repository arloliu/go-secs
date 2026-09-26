---
type: Mechanic
title: How a report from an ended generation is kept off its successor
description: How a disconnect, T7, TCP-up, Select, or send that speaks for one generation is bound to it — the identity carried on the queue, the lock-fenced gate the three synchronous commits need instead, the refusal each producer reports, and the unnamed reports that still bypass part of it.
tags: [hsms, hsmsss, lifecycle, supervisor, generation, concurrency]
status: stable
generated: {by: "claude/opus-5.5", at: 2026-09-26T10:19:32Z}
verified:
  - {by: "openai/gpt-5.6-terra", at: 2026-09-26T10:28:15Z}
sources:
  - {resource: hsms/supervisor.go, digest: sha256:1878d33bac78df60, revision: 7ae1ff0}
  - {resource: hsms/connection_lifecycle.go, digest: sha256:21abfedc85afb70a, revision: 7ae1ff0}
  - {resource: hsms/connection_runtime.go, digest: sha256:990bc46123a7df1c, revision: 7ae1ff0}
  - {resource: hsms/connection_send.go, digest: sha256:90a5e3ff7beea84c, revision: 7ae1ff0}
  - {resource: hsms/connection.go, digest: sha256:3c8a78070c937d5a, revision: 7ae1ff0}
  - {resource: hsms/epoch.go, digest: sha256:bdf3578cbc24fc48, revision: 7ae1ff0}
  - {resource: hsms/handler_panic.go, digest: sha256:7c995367269a8805, revision: 7ae1ff0}
  - {resource: hsms/transport.go, digest: sha256:dbd8d51fb2fc62ba, revision: 7ae1ff0}
  - {resource: hsmsss/transport.go, digest: sha256:176fff888fc8a85e, revision: 7ae1ff0}
  - {resource: hsmsss/transport_control.go, digest: sha256:84353e5b3b34860b, revision: 7ae1ff0}
  - {resource: hsmsss/transport_active.go, digest: sha256:33d63b7808dc1ed3, revision: 7ae1ff0}
  - {resource: hsmsss/transport_passive.go, digest: sha256:eaee7f67c2e994fc, revision: 7ae1ff0}
  - {resource: hsmsss/transport_procedures.go, digest: sha256:bf47bd9825ddb5da, revision: 7ae1ff0}
  - {resource: hsmsss/transport_recv.go, digest: sha256:fc971b2806f82a52, revision: 7ae1ff0}
  - {resource: secs1/transport.go, digest: sha256:399009fc96b7bf6c, revision: 7ae1ff0}
  - {resource: internal/gencap/gencap.go, digest: sha256:388f19be3dd1fe52, revision: 7ae1ff0}
  - {resource: hsmsss/transport_control_test.go, digest: sha256:d7d6115539bd3043, revision: 7ae1ff0}
---

# What it does

`TransportRuntime`'s godoc says only that an unnamed `TCPDown` or `T7Expired` is discarded once the generation current at the call has been replaced,
`TransportRuntime.TCPUp`'s godoc describes its guarded FSM commit,
and the gen-0 socket-publication limitation is documented on `(*connection).TCPUp`, not on the interface method.
Neither says how a named generation travels from a transport goroutine to the FSM,
why the three synchronous commits need a lock rather than the queue-side match,
That is the delta this entry records;
[how the recv path's responses and requests are bound on the wire](/hsms/generation-bound-wire-sends.md) records the same binding for frames;
[where a TransitionCause is chosen](/hsms/transition-cause-injection-sites.md) records the orthogonal question of which cause each report carries.

# How it works

**HSMS-SS reports name the generation they speak for.**
SECS-I's own reports do not.
Local Close queues its event with generation 0,
and a base-runtime report names no generation of its own
(an unnamed disconnect or T7 expiry is bound to one at report time, below).
A transport goroutine can outlive the generation that spawned it:
the teardown join is bounded,
so one wedged past the close timeout is abandoned and may resume after a reconnect has established and selected a new link.
Because the plain `TCPDown` entry points resolve `c.cur` and `c.sup` at call time, without a generation identity such a goroutine would drop the SUCCESSOR and report ITS cause to persistent subscribers.
Every ctx check in `hsmsss` narrows that window and none of them closes it —
`recvLoop`'s read-error branch, `handleSeparateReq`, the Select and linktest procedures all share the shape,
and cancellation can always land between the check and the call.

The identity is `epoch.id`, minted from the connection-scoped `genSeq` before the epoch is published on `cur` and never reset per Open cycle.
`hsmsss` reads it once per `Start` (`CurrentGeneration`) and stamps it on that generation's `genWG`,
which already reaches every goroutine it spawns;
each producer passes `g.gen` back through `TCPDownFromGeneration` / `T7ExpiredFromGeneration`.
It is checked in TWO places, and both are load-bearing:

- `connection.injectDisconnect` rejects a nonzero reported generation that differs in IDENTITY
  from the currently published epoch's `id` — it does not inspect `ended`.
  A gen of 0 skips that comparison and is bound, after it, to the `id` of the epoch that same `cur.Load()` returned,
  so the event it queues always names a generation.
  This is what keeps a straggler from setting the SUCCESSOR's `commsFailure`,
  which would suppress that generation's courtesy farewell Separate on a later graceful Close.
  A report whose generation still MATCHES the current (but already-ended) epoch passes this check regardless:
  it can still mark that epoch `commsFailure` and queue its event, same as `step` below;
  neither operation is thereby redirected onto a successor, since a successor cannot yet be published
  while this epoch's teardown is in flight.
- `fsmCommand.gen` carries the identity onto the queue,
  and `step` re-checks it AFTER `state.Load()` and BEFORE the transition.
  The ordering is the whole barrier:
  a successor's state can only be committed after that successor was published to `cur`,
  so reading such a state guarantees the `cur` read that follows observes the successor and discards the event.
  Reversing the two reverts it to a narrowed window.

The match cannot suppress a legitimate disconnect, and the reason is an invariant elsewhere:
`cur` is published in exactly two places (`Open`, `connectLoop`).
`connectLoop` itself starts from either the entering-NotConnected reaction or `Open`'s own
OpenBackground cold-active background retry (Gap 1) after that generation's own first-dial failure.
Either path publishes its successor only after the predecessor's own
teardown and join (`prev.wait()`) completed and the predecessor's own `Start` call returned (`epoch.startReturned`),
so `cur` advances only after the previous generation ended.
A mismatch therefore means the reporting generation's link was already torn down.
This holds for a custom transport whose `Start` errors after already driving TCP-up too:
the failing loop hands the retry to the drop reaction's loop instead of retrying itself,
so only one loop ever publishes a successor
(see [who owns the retry after a failed Start](/hsms/reconnect-retry-ownership.md)).
`s.curGen` must stay a bare atomic load.
`step` runs on the FSM goroutine, which an epoch teardown join can be waiting behind,
so a lock the teardown path holds risks stalling `step` behind it:
`Stop` holds `startGate` only while sealing starts and capturing its generation;
it releases the lock before socket closure and every join,
so it does not hold `startGate` during a join `step` could be waiting behind.

`secs1` does NOT name a generation — its line engine runs under a derived ctx and its own bundle.
Transports using only `TransportRuntime` likewise pass no identity.
Their unnamed disconnect and T7 reports are still fenced on the queue:
`injectDisconnect` and `injectT7Expiry` bind a gen of 0, at report time,
to the `id` of the generation current then (a nil `cur` enqueues nothing),
so `step`'s match discards the event once a successor is current, however long it sat queued.
The binding does not cover a report made AFTER the successor was published:
that call finds the successor current and binds to it,
which is why `hsmsss` names the generation it captured at its own `Start` instead.
Only two kinds of event still reach the queue with gen 0 and skip `step`'s identity match:
`evClose`, and the follow-up event of an unnamed synchronous commit (TCP-up, Select-accepted, or Select-lost).
A delayed unnamed Select-accepted or Select-lost follow-up event can therefore still be applied to a successor generation.
Commit-time admission is a separate question from that queued bypass.
`commitGate` (the Select-lost commit) is skipped entirely at gen 0 — a bare CAS, no liveness check —
but `tcpUpCommitGate` (the TCP-up commit)
and `selectCommitGate` (the Select-accepted commit) still require a non-nil, un-torn-down current epoch even for a gen of 0, and
`disconnectHandlerGeneration` rejects a gen of 0 outright rather than letting it through as a
wildcard.

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
`tcpUpCommitGate` also admits at most one TCP-up per generation (`epoch.tcpUpAdmitted`, latched even when the admitted CAS fails),
so a second report on a still-live generation is refused too; that refusal is logged but not counted in `staleGen`.
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

**`CommitSelected`'s and `CommitConnected`'s gates do not bypass a gen of 0, unlike `CommitSelectLost`'s.**
`commitGate` (used only by `CommitSelectLost`) is skipped outright whenever `gen == 0`,
exactly the pre-generation behavior secs1 and every transport using only `TransportRuntime` get:
no lock, no liveness check, a bare CAS.
`tcpUpCommitGate` skips identity matching for a gen-0 TCP-up, as `selectCommitGate` below does,
but still requires liveness and an unused per-generation admission (`epoch.tcpUpAdmitted`),
and committing additionally requires its state CAS to succeed;
on a successful commit it latches `epoch.tcpUpCommitted` on the generation it validated
(see [who owns the retry after a failed Start](/hsms/reconnect-retry-ownership.md) for its reader).
`selectCommitGate` is a sibling gate `CommitSelected`/`CommitSelectedFromGeneration` use instead,
and it admits a gen of 0 on liveness alone — a non-nil, un-torn-down `cur` — skipping only the identity comparison a named generation still has to pass.
A gen-0 Select commit against an already-ended generation is therefore refused and counted in `staleGen`,
the same outcome a refused named commit gets.
On a successful commit `selectCommitGate` also latches `epoch.reachedSelected` on the SAME `cur` the RLock section validated, before releasing the lock —
see the reconnect-backoff-scope entry for what reads that marker and why.


`connection.publishSocket` takes the same gate for the same reason:
a passive accept can RACE teardown rather than be abandoned by it —
`hsmsss.Stop` joins the accept goroutine (`g.accept.Wait`) WITHOUT a timeout, ahead of its own
deadline-bounded joins (procedure, receive, linktest, T7), so an accept in flight is never abandoned
by the close timeout the way those other goroutines can be.
The race publishSocket guards is narrower: an accept that adopts a peer and reaches the publish gate
just as (or just after) teardown has already latched `ended` on that same epoch, so its socket must be
refused rather than published onto a generation that is already dying.
It resolves the epoch by identity and writes to THAT epoch, so a swap can never redirect it to a successor.

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
The concrete core's own gen-0 Select-lost path is unaffected by this —
`commitFrom` routes `evSelectLost` through `commitGate`, never `selectGate`,
so a gen of 0 there bypasses the gate entirely and returns its bare CAS result directly;
`selectCommitGate` applies only to `evSelectAccepted` (Select-accepted), never to Select-lost.

The remaining two producers stay void.
A refused `TCPDownFromGeneration` or `T7ExpiredFromGeneration` is a queued event whose caller has nothing to undo.

**How the generation capability crosses the package boundary.**
`hsmsss` reaches the generation-aware methods by type-asserting `t.rt` to a package-local `genRuntime` interface,
and prefers it over the cause-only `causeRuntime` path when present.
`genRuntime` and hsms's `genCapability` are local aliases of one
`gencap.GenerationRuntime[hsms.Message, hsms.TransitionCause]` instantiation —
ten methods:
the `CurrentGeneration` accessor, five `*FromGeneration` report/commit methods,
two generation-named send methods (`SendAsyncFromGeneration` / `WriteMessageFromGeneration`),
and two generation-named INBOUND methods (`DeliverOwnedFrameFromGeneration` / `RouteReplyFromGeneration`,
which fence admission of a frame/reply to the generation whose recv goroutine actually read it) —
and hsms asserts at compile time that `*connection` satisfies it —
see [the inbound generation fence](/hsms/inbound-generation-fence.md) for how the two newer inbound methods admit or route on behalf of a named generation,
so a method-set drift between the offering and the consuming side is a build failure rather than a silent fallback.
Widening the exported `TransportRuntime` instead would break external implementers,
which is why `secs1` and every transport using only `TransportRuntime` keep the unnamed paths above.
`hsmsss/transport_control_test.go`'s `genRecRT` is a test runtime that implements this capability,
so generation-aware transport behavior can be exercised without a concrete `*connection`.

# Failure modes

**A straggler dropping the successor.**
Without the named generation, an abandoned `hsmsss` goroutine resuming after a reconnect reports through `TCPDown`,
which resolves `c.cur` at call time:
it drops the SUCCESSOR, sets the successor's `commsFailure` (suppressing that link's courtesy Separate on a later graceful Close),
and reports its cause to persistent subscribers.
Dropping either check reopens part of it:
without the report-side check a straggler can still set the successor's `commsFailure`;
without the queue-side re-check, an event queued before the successor was published drops that successor when it is processed.

**A stale Select commit landing on a dead generation.**
A custom `Start` that drives TCP-up and then errors leaves an ended generation current at `NotSelected`,
the exact state a live `CommitSelected` CAS expects;
dropping the `ended` check from `selectCommitGate` lets a straggler's Select commit move that dead link to `Selected`.

**An unnamed synchronous-commit follow-up applied to a successor.**
The follow-up event of an unnamed TCP-up, Select-accepted, or Select-lost commit reaches the queue with gen 0 and skips `step`'s identity match,
and an unnamed Select-lost commit skips `commitGate` entirely.
A delayed one can therefore still be applied to a successor generation.
This affects `secs1` and transports using only `TransportRuntime`; `hsmsss` names its generation and is not exposed.

**A gen-0 socket published onto a torn-down generation.**
The plain `TCPUp` publishes its socket without a liveness check,
so a report landing after its generation's teardown already closed the socket re-populates it,
and nothing closes it afterward.

# Where to look

- generation identity: `hsms/epoch.go` → `epoch.id`;
  `hsms/connection.go` → `genSeq`;
  `hsms/connection_lifecycle.go` → `CurrentGeneration` and the two `cur.Store` sites
- report-side check and report-time binding: `hsms/connection_lifecycle.go` → `TCPDownFromGeneration`, `injectDisconnect`;
  `hsms/connection_runtime.go` → `T7ExpiredFromGeneration`, `injectT7Expiry`;
  `hsms/handler_panic.go` → `(*connection).disconnectHandlerGeneration`
- the queue-side barrier: `hsms/supervisor.go` → `step`'s generation match, `injectFrom`, `curGen`, `staleGen`
- the synchronous-commit fence: `hsms/connection.go` → `genGate`;
  `hsms/connection_lifecycle.go` → `commitGate`, `tcpUpCommitGate`, `selectCommitGate`, `publishSocket`, `commitTCPUp`, `connectLoopStartFailure`, `genCapability`;
  `hsms/epoch.go` → `epoch.ended`, `epoch.markEnded`, `epoch.tcpUpAdmitted`, `epoch.tcpUpCommitted`;
  `hsms/supervisor.go` → `commitFrom`, `selectGate`, `tcpUpCommitGate`, `CommitConnectedFromGeneration`, `CommitSelectedFromGeneration`, `CommitSelectLostFromGeneration`;
  `hsms/connection_runtime.go` → `commitSelectAccepted`, `commitSelectLost`
- the shared capability type: `internal/gencap/gencap.go` → `GenerationRuntime`
- transport side: `hsmsss/transport_control.go` → `genRuntime`, `transport.tcpUp`, `transport.selectLost`;
  `hsmsss/transport.go` → `genWG.gen`;
  `hsmsss/transport_active.go` → `(*transport).startActive`;
  `hsmsss/transport_passive.go` → `(*transport).startPassive`, `acceptLoop`
- per-generation timer handles that limit what `selectLost`'s refusal protects: `hsmsss/transport.go` → `genWG.t7Cancel`, `genWG.linktestCancel`, `genWG.timerMu`;
  `hsmsss/transport_procedures.go` → `(*transport).stopLinktest`, `(*transport).armT7`
- a test runtime implementing the generation capability: `hsmsss/transport_control_test.go` → `genRecRT`
