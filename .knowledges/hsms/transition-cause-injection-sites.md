---
type: Mechanic
title: Where a TransitionCause is chosen, and why one transition can swallow another's cause
description: The full transition-source to cause map, why the cause is picked at the injection site rather than derived in the FSM, why the transports pass both the cause and their generation through a capability interface instead of TransportRuntime, how the generation match keeps a late transport goroutine from dropping its successor's link, why the three synchronous commits need a lock-fenced gate instead of that match, and the ways a cause never reaches a subscriber.
tags: [hsms, lifecycle, supervisor, fsm, observability]
status: stable
generated: {by: "claude/opus-5.5", at: 2026-09-26T09:40:37Z}
verified:
  - {by: "openai/gpt-5.6-terra", at: 2026-09-26T09:48:36Z}
sources:
  - {resource: hsms/lifecycle.go, digest: sha256:9c093383d23470e1, revision: d244104}
  - {resource: hsms/session.go, digest: sha256:134bcdad84cba21a, revision: d244104}
  - {resource: hsms/handler_panic.go, digest: sha256:7c995367269a8805, revision: d244104}
  - {resource: hsms/supervisor.go, digest: sha256:1717fd0875937ae8, revision: f7a5927}
  - {resource: hsms/connection_lifecycle.go, digest: sha256:e6d0d8d5bdf02ea2, revision: f7a5927}
  - {resource: hsms/connection_runtime.go, digest: sha256:990bc46123a7df1c, revision: f7a5927}
  - {resource: hsms/connection_send.go, digest: sha256:90a5e3ff7beea84c, revision: f7a5927}
  - {resource: hsms/connection.go, digest: sha256:d45005d0dcf9540c, revision: 4be2062}
  - {resource: hsms/epoch.go, digest: sha256:5d48c9656ae715cc, revision: 4be2062}
  - {resource: hsmsss/transport.go, digest: sha256:cb3594d212e03da1, revision: c00e1b5}
  - {resource: hsmsss/transport_control.go, digest: sha256:84353e5b3b34860b, revision: 6c257b6}
  - {resource: hsmsss/transport_active.go, digest: sha256:252154bd042e64d4, revision: c00e1b5}
  - {resource: hsmsss/transport_passive.go, digest: sha256:562498fdb8cfa240, revision: c00e1b5}
  - {resource: hsmsss/transport_procedures.go, digest: sha256:bf47bd9825ddb5da, revision: 6c257b6}
  - {resource: hsmsss/transport_recv.go, digest: sha256:f54ea89029ef179c, revision: 6c257b6}
  - {resource: secs1/transport.go, digest: sha256:399009fc96b7bf6c, revision: 4be2062}
  - {resource: internal/gencap/gencap.go, digest: sha256:388f19be3dd1fe52, revision: c00e1b5}
  - {resource: hsmsss/transport_control_test.go, digest: sha256:44103f361af0833e, revision: 6c257b6}
  - {resource: hsmsss/integration_lifecycle_cause_test.go, digest: sha256:50bb158dce6382ea, revision: d244104}
---

# What it does

`SubscribeLifecycle`'s godoc states the consumer contract:
callbacks run on the notifier goroutine, a subscription survives Open/Close, `cancel` is idempotent,
and `Cause` names the event that drove the transition.
It does not record WHERE each cause is picked, why the choice lives at the injection site rather than in the FSM,
or how a cause reaches `hsms` from a transport package that `hsms` must not depend on.
That is the delta this entry records.

# How it works

**The cause is data on the event, never a function of the state pair.**
`fsmCommand{ev, cause}` is what `inject` queues and `step` dequeues;
`stateChange` carries the cause to the notifier.
The transition table (`transition`) never reads it.
`step` makes exactly one substitution:
an `evTCPUp` that finds the state already `Selected` reports `CauseSelectAccepted` instead of its own cause
(the coalesced bring-up, under Failure mode).
The same state pair therefore carries different causes on different runs:
`Selected -> NotConnected` is a local Close, a peer Separate, a linktest failure, or a read error.
That is exactly the discrimination the feature exists to provide, and the reason it cannot be reconstructed after the fact from `(prev, next)`.

**Every injection site, and the cause it names.**

| Injection site | Event | Cause |
|---|---|---|
| `connection.TCPUp` → `supervisor.CommitConnectedFromGeneration(0, …)` | `evTCPUp` | `CauseLocalOpen`; `step` reports `CauseSelectAccepted` instead when the Select commit already landed |
| `connection.CommitSelected` → `supervisor.CommitSelectedFromGeneration(0, …)` | `evSelectAccepted` | `CauseSelectAccepted` |
| `connection.SelectLost` → `supervisor.CommitSelectLostFromGeneration(0, …)` | `evSelectLost` | `CausePeerDeselect` |
| (the three above, generation-named: `*FromGeneration` → `supervisor.commitFrom`; `CommitConnected` under `connection.tcpUpCommitGate`, `CommitSelected` under `connection.selectCommitGate`, `CommitSelectLost` under `connection.commitGate`) | same | same |
| `connection.T7Expired` | `evT7Timeout` | `CauseT7Timeout` |
| `connection.Close` → `requestClose` | `evClose` | `CauseLocalClose` |
| `connection.Open` rollback → `requestClose` | `evClose` | `CauseLocalClose` |
| `connection.writeFrame` write failure | `evDisconnect` | `CauseIOError` |
| `connection.TCPDown` (no cause named) | `evDisconnect` | `CauseUnknown` |
| `connection.connectLoopStartFailure` hand-off, after a reconnect `Start` failed on a generation whose TCP-up committed → `injectDisconnect` | `evDisconnect` | `CauseUnknown` |
| `hsmsss.handleSeparateReq` | `evDisconnect` | `CausePeerSeparate` |
| `hsmsss.runSelectProcedure` failed transaction | `evDisconnect` | `selectFailureCause`: `CauseSelectRejected` on a `*RejectError`, `CauseT6Timeout` on `ErrT6Timeout`, else `CauseIOError` |
| `hsmsss.runSelectProcedure` a genuine select-status refusal (see below), or a correlated non-Select.rsp | `evDisconnect` | `CauseSelectRejected` |
| `hsmsss.runLinktest` threshold reached | `evDisconnect` | `CauseLinktestFail` |
| `hsmsss.recvLoop` read error | `evDisconnect` | `CauseIOError` |
| `secs1.lineEngine` read / EOT-write error | `evDisconnect` | `CauseIOError` |
| `session.callDataHandler` / `callDecodeErrorHandler` / `connection.callAsyncSendErrorHandler` Goexit → `connection.disconnectHandlerGeneration` | `evDisconnect` | `CauseHandlerExit` |

**`CauseHandlerExit` identifies callback-induced termination, using the generation supplied to that callback.**
`CauseHandlerExit` reports on the generation a USER CALLBACK was running for, when that callback
ends via `runtime.Goexit` instead of returning — `DataMessageHandler`/`DecodeErrorHandler`
(`session.callDataHandler`/`callDecodeErrorHandler`, run on the recv goroutine) or the async send error
handler (`connection.callAsyncSendErrorHandler`, run on the per-generation async-sender goroutine).
All three route through `hsms/handler_panic.go`'s `disconnectHandlerGeneration`, which calls
`c.TCPDownFromGeneration(gen, errHandlerGoexit, CauseHandlerExit)` — the SAME generation-named entry
point `hsmsss` uses for its own disconnects, though `secs1` does not (see below).
`disconnectHandlerGeneration` separately rejects `gen == 0` itself, before ever calling
`TCPDownFromGeneration`.
A nonzero, mismatched `gen` is instead caught by `injectDisconnect`/`step`'s own generation-identity
match, which — as documented above — compares IDENTITY only, not `ended`: an ended-but-still-current
generation's report still passes it.
`gen` is always the generation `routeDataOn`/`recvDataMsgOn`/`dispatchDecodeErrorOn` (or
`drainSendCh`) resolved BEFORE calling the handler, threaded straight through rather than re-resolved
in the defer that detects the Goexit — see [the callback panic/Goexit isolation
entry](/hsms/handler-panic-goexit-isolation.md) for why re-resolving there would name the wrong
generation.
`secs1` does not name a generation for its OWN disconnects (row above, `secs1.lineEngine`) — but a
`secs1`-hosted `DataMessageHandler`/`DecodeErrorHandler` still resolves a real, nonzero `gen`: plain
`DeliverOwnedFrame` loads `c.cur.Load()` unconditionally and `routeDataOn` computes `gen` from that
epoch's own `id` (0 only when `cur` was nil).
So a `secs1` handler's Goexit CAN drive a `CauseHandlerExit` disconnect naming the connection's current generation,
despite `secs1` having no generation-aware transport capability of its own.
SECS-I's own transport disconnect reports are unqualified;
shared-core write failures (`connection.writeFrame`, which every `secs1` write goes through)
and callback Goexit use generation-named disconnects.

**The Select procedure is the only site with a non-constant cause, and the reason is the reply registry.**
An active Select.req is an ordinary registered transaction, so THREE different things can come back through `WriteMessage`.
A peer Reject.req correlated to its System Bytes is delivered as a `*hsms.RejectError` (`connection.RouteReply`) — the peer answered and refused, and no read or write failed,
so classifying it as `CauseIOError` would report a link fault for a protocol answer.
A T6 expiry returns a bare `ErrT6Timeout` from `connection.sendWaitReplyOn`'s control-transaction reply wait —
`writeFrame` performs only the synchronous write and has no reply wait of its own —
meaning the peer never answered at all.
Everything else is the transport failing.
`selectFailureCause` splits those three; a correlated response of the wrong TYPE (a Linktest.rsp, a Deselect.rsp) is handled separately on the success path and also reports `CauseSelectRejected`,
because the peer answered and the select was not granted — which is what `CauseSelectRejected` is defined to mean.
The distinction between "refused" and "answered with the wrong frame" survives in the error value (`errSelectRejected` vs `errSelectBadResponse`), not in the cause.

A non-zero select-status only reaches this cause when it is a genuine refusal.
Status 2+ always is one;
status 1 (`SelectStatusAlreadyActive`) is one only when this generation's `genWG.selectedOnce` latch still reads false.
A status 1 answer while that latch reads true is the legitimate simultaneous-select case instead,
and drives no transition at all.

`CommitSelected` names `CauseSelectAccepted` for `secs1` too, which runs no select handshake:
the auto-commit reaches `Selected` for the same reason a handshake does, so the cause is honest rather than approximate.

**`CauseUnknown` on bare `TCPDown` is deliberate.**
`TransportRuntime.TCPDown(cause error)` carries an error for the farewell decision, not a classification.
Defaulting it to `CauseIOError` would mislabel every drop a transport using only `TransportRuntime` reports for a non-I/O reason.
The shipped transports name a cause through the optional `TCPDownWithCause` capability, reached by type assertion;
a runtime reached without that capability reports `CauseUnknown`.

**HSMS-SS injection sites also name the generation they speak for, and that is a separate axis from the cause.**
SECS-I carries the cause only.
Local Close queues its event with generation 0,
and a base-runtime report names no generation of its own
(an unnamed disconnect or T7 expiry is bound to one at report time, below).
A transport goroutine can outlive the generation that spawned it:
the teardown join is bounded,
so one wedged past the close timeout is abandoned and may resume after a reconnect has established and selected a new link.
Because the plain `TCPDown` entry points resolve `c.cur` and `c.sup` at call time, such a goroutine used to drop the SUCCESSOR and report ITS cause to persistent subscribers.
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
`commitGate` (now used only by `CommitSelectLost`) is skipped outright whenever `gen == 0`,
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
the same outcome a refused named commit gets, where it used to take the bare CAS.
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

**Select-lost is the other reported refusal, and what it protects has narrowed since R10.**
`SelectLostFromGeneration` returns whether the CAS was applied,
and `hsmsss`'s `selectLost` wrapper forwards that to `handleDeselectReq`.
The work skipped on a refusal is the pair of calls that belong to a real `Selected -> NotSelected` transition,
`stopLinktest` and `armT7`.
Before the R10 inbound-generation-fence work neither was generation-scoped —
`stopLinktest` cancelled whatever the single transport-wide `t.linktestCancel` held,
and `armT7` derived its dwell ctx from the current `t.genCtx`,
while registering the goroutine on the STALE generation's bundle —
so running them after a refused commit could leave a live successor logically Selected
with its auto-linktest cancelled and a stale dwell attached.
Since R10 / D4, `stopLinktest(g)` and `armT7(g)` both take the caller's bundle explicitly
and touch ONLY `g`'s own `linktestCancel`/`t7Cancel`/`timerMu` fields (see `genWG` in `hsmsss/transport.go`):
even an UNCONDITIONAL call from a straggler can no longer reach a live successor's timers,
because there is no shared, transport-wide handle left for it to race into.
`selectLost`'s guard on the CAS result therefore no longer fences a straggler from a successor's timers —
D4's g-scoping does that at a different layer —
it now only SAVES needless work on a refused commit (arming/cancelling a stale generation's own, already-dead bundle).
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

**A generation-guarded FSM commit does not protect the WIRE, so the recv path binds its answers too.**
When `genRuntime` is available they go through `SendAsyncFromGeneration`; otherwise `sendResponse` falls back to the unqualified `SendAsync`.
This is a separate rule from the three commits, and the reason is that a frame cannot be retracted.
`SendAsync` resolves `c.cur` at call time — correct for a caller that speaks for whatever generation is current,
wrong for one that speaks for a named generation.
Every response produced by the ESTABLISHED generation's recv dispatch is the second kind:
`Select.rsp`, `Deselect.rsp`, `Linktest.rsp`, and the three `Reject.req` variants
(`sendReject`, `sendRejectNotSelected`, `sendRejectTransactionNotOpen`)
are all built from a request that arrived on ONE generation's socket, and all carry that request's System Bytes back.
A straggler using the unqualified send therefore hands the SUCCESSOR's peer a response to a transaction it never opened.

`connection.SendAsyncFromGeneration` is the bound entry point,
and `hsmsss`'s `sendResponse` wrapper is the site every one of THOSE responses goes through
(`g.gen` reaches it from `dispatchFrame`, which is why the Reject helpers and `handleLinktestReq` take the bundle).
The passive extra-connection refusal is a SEPARATE exchange, not routed through `sendResponse` at all:
`refuseExtraConn` (`hsmsss/transport_passive.go`) writes a status-1 Select.rsp directly to its captured
extra socket, under that socket's own deadline, because that socket never became the generation's live
connection and has no `g.gen` to bind through.
The rule is enqueue-iff-live: `connection.liveEpoch` applies the same `{id, !ended}` test under the same gate,
and a send naming a dead generation is DROPPED and counted in `connection.staleSend`, never redirected.
Dropping is the correct answer rather than a fallback:
the socket that asked the question is gone, so its answer has no destination.

The gate discipline differs from `commitGate`'s in one way that matters.
A CAS runs under the RLock; an enqueue cannot,
because `epoch.sendCh` can be full and nothing that blocks may run under the gate.
So the gate RESOLVES the epoch and is released,
and the enqueue targets that resolved epoch object rather than re-reading `c.cur` —
which is what keeps a swap after the unlock from redirecting the frame.
A teardown landing in that window is raced rather than excluded, and harmlessly:
the enqueue unblocks on the resolved epoch's own ctx,
and anything already queued on a torn-down epoch is stranded with it (`drainSendCh`, C1).

**A REQUEST needs the same binding, and needs it more, because it opens a transaction.**
`runSelectProcedure`'s `Select.req` and `runLinktest`'s `Linktest.req` go through `WriteMessage`, which is reply-correlated rather than queued.
The unqualified path resolves `c.cur` at call time and then does everything on that epoch:
it REGISTERS the reply channel in that epoch's registry and writes through that epoch's socket.
A straggler therefore opens a transaction on the SUCCESSOR.
Neither existing guard stops it.
`writeFrame`'s I1 conn binding protects a sender already PINNED to its own epoch — the stale caller never pinned one, it resolved a fresh one.
The cancelled generation ctx does not either: `writeFrame` checks the RESOLVED epoch's ctx, which is live, and the hsmsss transport ignores the caller ctx entirely.

The damage is worse than a stray response.
If the answer beats the caller's cancellation-deregistration, `RouteReply` finds the successor's registration,
and the reply can reach it and trigger the successor's Select commit,
even if the waiting caller ultimately returns cancellation —
`sendWaitReplyOn`'s `callerCtx.Done()` arm does not re-check a ready reply.
hsmsss's `transport.dispatchFrame` commits whenever routing a status-0 `Select.rsp` succeeds,
so the successor's own recv loop commits to Selected over a handshake that link never ran.
If cancellation wins instead, the answer is orphaned and the successor answers it with a `Reject.req` (§8.3.20) — a Reject to a peer that did nothing wrong.
A stale `Linktest.req` can also arrive while the successor is still NotSelected, which E37.1 §7.4 classifies as a communications failure the peer may answer by closing the new link.

`connection.WriteMessageFromGeneration` binds it, and one resolution covers both halves because the epoch owns both the registry and the socket:
`liveEpoch(gen)` under the gate, then `sendWaitReplyOn(ctx, e, msg)` — the shared body `sendWaitReply` now calls with `c.cur` — so registration, write, and the `ErrConnClosed` wait arm all name the same epoch.
A refused request is dropped and counted before anything is registered, and reported as `ErrConnClosed`,
which both procedures ALREADY read as their own generation's death (`runLinktest` had that branch for the teardown race; `runSelectProcedure` gained it).
That is what makes a refusal exit quietly instead of counting a linktest failure or reporting a TCPDown for a frame that was never sent.

**A refused response is not an enqueued one, and the public counters follow the enqueue, not the wire.**
`RejectSentCount` (Rejects emitted) and `LinktestReqRecvCount` (probes answered) increment when `sendResponse`
returns nil — that is, when `enqueueAsync` accepts the message onto `epoch.sendCh`, NOT when
`drainSendCh`'s later `writeFrame` actually puts it on the wire.
An enqueue that succeeds can still be followed by a write failure or a teardown that strands the queued
frame (`drainSendCh`, C1), so these counters are proof of acceptance into the send queue, not of
successful wire delivery.
`LinktestSendCount` is the opposite shape: it is an ATTEMPT counter, incremented in `runLinktest` BEFORE
the synchronous `writeMessage` call that actually sends the probe, which its godoc now states outright.

**How the cause crosses the package boundary.**
`hsmsss` and `secs1` reach `TCPDownWithCause` by type-asserting `t.rt` to a package-local `causeRuntime` interface, then fall back to plain `TCPDown`.
`hsmsss` additionally asserts a `genRuntime` interface carrying the generation-aware methods,
and prefers it when present.
`genRuntime` and hsms's `genCapability` are local aliases of one
`gencap.GenerationRuntime[hsms.Message, hsms.TransitionCause]` instantiation —
ten methods since the R10 inbound-generation-fence work added two:
the `CurrentGeneration` accessor, five `*FromGeneration` report/commit methods,
two generation-named send methods (`SendAsyncFromGeneration` / `WriteMessageFromGeneration`),
and two generation-named INBOUND methods (`DeliverOwnedFrameFromGeneration` / `RouteReplyFromGeneration`,
which fence admission of a frame/reply to the generation whose recv goroutine actually read it) —
and hsms asserts at compile time that `*connection` satisfies it —
see [the inbound generation fence](/hsms/inbound-generation-fence.md) for how the two newer inbound methods admit or route on behalf of a named generation,
so a method-set drift between the offering and the consuming side is a build failure rather than a silent fallback.
This is the same pattern `suppressionRuntime` uses for `LinktestSuppression`,
and for the same documented reason (`hsms/connection.go`):
widening the exported `TransportRuntime` would break external implementers.
A mock runtime that implements only `TransportRuntime` reports `CauseUnknown`.
Not every mock in the suite is one of those, though:
`hsmsss/transport_control_test.go`'s `genRecRT` additionally implements the generation capability,
specifically to exercise generation-aware transport behavior in tests,
so the hsms-package tests' reliance on the concrete `*connection` (rather than a mock) is about
exercising the REAL `causeRuntime`/`TCPDownWithCause` wiring, not about no test mock offering `genRuntime` at all.

# Failure modes

**A cause silently lost to dedup.**
`step` fires when `next != s.lastReacted`, or when the event really drops the link:
it stores `NotConnected` over a live state (`left != NotConnected`, where `left` is what the `Swap` replaced).
An `evDisconnect(CauseIOError)` that has already driven and reported `Selected -> NotConnected` leaves `lastReacted == NotConnected`;
a `Close` behind it transitions `NotConnected -> NotConnected`, drops nothing, does not fire,
and its `CauseLocalClose` is never reported.
Observed from outside: a subscriber sees the drop's cause and no close event at all.
This is correct — the FSM really did make one transition —
but a consumer that treats the newest event as "why the link is down right now" reads the right answer only because the FIRST cause is the true one.

The swallow needs the earlier drop to have been processed first.
`lastReacted` can lag the stored state,
because the synchronous commits CAS the state before their report reaches the queue,
so it can read `NotConnected` while the link is actually `NotSelected` or `Selected`.
Dedup on `lastReacted` alone would absorb the drop that ends such an unreported bring-up.
The drop exception makes that drop fire anyway, with `prev` set to the replaced state rather than `lastReacted`;
a `Close` that overtakes both bring-up reports fires `Selected -> NotConnected` with `CauseLocalClose`.
The reaction depends on that `prev` too, and the SubscribeLifecycle godoc does not say how:
`connection.react` sends the courtesy Separate only when `prev == Selected`,
starts the reconnect loop unless the connection is shutting down, and initiates teardown.
Without the exception, an absorbed involuntary disconnect would lose the reaction-driven teardown and reconnect,
not just the notification, leaving a link down with nothing driving it back up.
An absorbed Close would lose the courtesy farewell and the notification,
but its explicit teardown (`step`'s `evClose` branch, via `closeEpoch`) still runs,
and it never reconnects anyway.

**A cause lost to coalescing.**
`emit` is a non-blocking drop-OLDEST send.
Under a subscriber that does not drain, an intermediate `stateChange` is discarded with its cause.
The latest state always survives; an intermediate cause may not.
The drop exception guarantees only the enqueue:
under a subscriber that stays behind across further reconnects, a drop's own notification can be the one discarded.

**The bring-up pair collapsing into one event.**
`CommitConnected` CAS-stores `NotSelected` and then enqueues `evTCPUp`.
If the select handshake commits before `run()` drains that `evTCPUp`,
`step` finds the state already `Selected`.
`transition(Selected, evTCPUp)` is a legal same-state entry, so nothing is stored,
but `Selected` differs from `lastReacted`,
so the `evTCPUp` itself fires `NotConnected -> Selected`, reported with the substituted `CauseSelectAccepted`.
The `evSelectAccepted` behind it then finds `Selected == lastReacted` and fires nothing.
A bring-up therefore legitimately reports either two events or one.
A test that asserts an exact three-event Open/Select/Close sequence is flaky by construction;
see `hsmsss.causeLog.waitBringUp`, which tolerates both shapes.

**A late TCP-up report overtaken by a drop.**
A disconnect can reach the queue between `CommitConnected`'s CAS and its `evTCPUp`.
The disconnect then fires `NotSelected -> NotConnected` under the drop exception above,
and the `evTCPUp` behind it finds `NotConnected`.
`transition(NotConnected, evTCPUp)` is illegal, so that report is ignored.
If it were legal, it would store `NotSelected` on a generation that has already dropped.
The successor's own `CommitConnected` CAS expects `NotConnected`, so it would then fail,
and the successor's TCP-up CAS and its report would be lost.
In that hypothetical, the successor's later Select commit could still succeed,
since `connection.commitTCPUp` returns socket acceptance regardless of the CAS result
and `CommitSelectedFromGeneration` would CAS out of the resurrected `NotSelected`.
As implemented, the late report is ignored and the state stays `NotConnected`, so nothing is resurrected.

# Where to look

- causes and subscription storage: `hsms/lifecycle.go` →
  `TransitionCause`, `LifecycleEvent`, `connection.SubscribeLifecycle`, `connection.cancelLifecycle`
- event plumbing: `hsms/supervisor.go` → `fsmCommand`, `inject`, `injectFrom`, `step`, `fireTransition`, `notifySubs`
- dedup, the drop exception, and the late-TCP-up rejection: `hsms/supervisor.go` → `step`, `lastReacted`, `transition`;
  the reaction that reads the reported `prev`: `hsms/connection_lifecycle.go` → `(*connection).react`
- cause-carrying disconnect: `hsms/connection_lifecycle.go` → `TCPDownWithCause`, `TCPDownFromGeneration`, `injectDisconnect`;
  the unnamed T7 report and its report-time binding: `hsms/connection_runtime.go` → `T7Expired`, `injectT7Expiry`;
  the reconnect hand-off report: `connectLoopStartFailure`
- `CauseHandlerExit`, the callback-exit injection site, and the generation it names: `hsms/handler_panic.go` →
  `(*connection).disconnectHandlerGeneration`, `errHandlerGoexit`;
  `hsms/session.go` → `(*session).callDataHandler`, `(*session).callDecodeErrorHandler`;
  `hsms/connection_send.go` → `(*connection).callAsyncSendErrorHandler`;
  `hsms/connection_runtime.go` → `(*connection).routeDataOn`, `(*connection).DeliverOwnedFrame`
- generation identity: `hsms/epoch.go` → `epoch.id`;
  `hsms/connection.go` → `genSeq`;
  `hsms/connection_lifecycle.go` → `CurrentGeneration` and the two `cur.Store` sites
- the barrier itself: `hsms/supervisor.go` → `step`'s generation match, `curGen`, `staleGen`
- the synchronous-commit fence: `hsms/connection.go` → `genGate`;
  `hsms/connection_lifecycle.go` → `commitGate`, `tcpUpCommitGate`, `selectCommitGate`, `publishSocket`, `commitTCPUp`, `genCapability`;
  `hsms/epoch.go` → `epoch.ended`, `epoch.markEnded`, `epoch.tcpUpAdmitted`, `epoch.tcpUpCommitted`;
  `hsms/supervisor.go` → `commitFrom`, `selectGate`, `tcpUpCommitGate`, `CommitConnectedFromGeneration`, `CommitSelectedFromGeneration`, `CommitSelectLostFromGeneration`;
  `hsms/connection_runtime.go` → `commitSelectAccepted`, `commitSelectLost`
- the shared capability type: `internal/gencap/gencap.go` → `GenerationRuntime`
- transport capability: `hsmsss/transport_control.go` → `causeRuntime`, `genRuntime`,
  `transport.tcpDown`, `transport.t7Expired`, `transport.tcpUp`, `transport.commitSelected`, `transport.selectLost`;
  `secs1/transport.go` → `causeRuntime` only
- the generation token's carrier: `hsmsss/transport.go` → `genWG.gen`, stamped in `startActive` / `startPassive`
- the D4 timer g-scoping that narrowed what `selectLost`'s refusal protects: `hsmsss/transport.go` → `genWG.t7Cancel`, `genWG.linktestCancel`, `genWG.timerMu`;
  `hsmsss/transport_procedures.go` → `(*transport).stopLinktest`, `(*transport).armT7`
- the simultaneous-select exemption for status 1: `hsmsss/transport.go` → `genWG.selectedOnce`;
  `hsmsss/transport_active.go` → `runSelectProcedure`;
  `hsmsss/transport_control.go` → `handleSelectReq`;
  `hsmsss/transport_recv.go` → `dispatchFrame`'s routed Select.rsp branch
- a test mock that DOES implement the generation capability: `hsmsss/transport_control_test.go` → `genRecRT`
- the bring-up-pair-tolerant wait helper: `hsmsss/integration_lifecycle_cause_test.go` →
  `(*causeLog).waitBringUp`
