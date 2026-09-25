---
type: Mechanic
title: How a user callback's panic or Goexit is contained
description: The two-frame detection runCallback needs to tell a panic from a Goexit apart even when one interrupts the other, which goroutine each callback site runs on, and what this isolation does not cover.
tags: [hsms, panic-isolation, goroutines, lifecycle, generations]
status: stable
generated: {by: "claude/sonnet-5", at: 2026-09-25T08:02:46Z}
verified:
  - {by: "openai/gpt-5.6-terra", at: 2026-09-25T08:37:47Z}
sources:
  - {resource: hsms/handler_panic.go, digest: sha256:7c995367269a8805, revision: d244104}
  - {resource: hsms/handler_panic_test.go, digest: sha256:def868b2ecc8930c, revision: d244104}
  - {resource: hsms/endpoint.go, digest: sha256:b75d6a0370642b02, revision: cc82a06}
  - {resource: hsms/state.go, digest: sha256:f467c560ffea5807, revision: d244104}
  - {resource: hsms/connection_config.go, digest: sha256:e701533ea6c49f0a, revision: c00e1b5}
  - {resource: hsms/session.go, digest: sha256:134bcdad84cba21a, revision: d244104}
  - {resource: hsms/supervisor.go, digest: sha256:bffb8f4562c8b494, revision: cc82a06}
  - {resource: hsms/connection_send.go, digest: sha256:e485168d1a431fe7, revision: d244104}
  - {resource: hsms/connection_lifecycle.go, digest: sha256:dac8943b7389474b, revision: c00e1b5}
  - {resource: hsms/connection_runtime.go, digest: sha256:dce97e0bf7fc6616, revision: d244104}
  - {resource: hsmsss/transport_recv.go, digest: sha256:f54ea89029ef179c, revision: d244104}
  - {resource: hsmsss/transport.go, digest: sha256:cb3594d212e03da1, revision: c00e1b5}
  - {resource: hsms/hsmstest/endpoint.go, digest: sha256:f695c5a80165861a, revision: d244104}
  - {resource: hsms/hsmstest/panic_test.go, digest: sha256:5503e2cd947250bc, revision: d244104}
  - {resource: secs1/config.go, digest: sha256:c424cc48eb804c68, revision: c00e1b5}
---

# What it does

`handler_panic.go`'s own comments already state most of the WHAT: `runCallback` recovers a panic
(counted, logged) and separately detects a `runtime.Goexit` (logged only); a nested panic raised
while a Goexit unwinds still fires both reports; `disconnectHandlerGeneration` treats `gen == 0` as
a no-op, runs under its own recover, and disconnects before it logs.
`endpoint.go`/`state.go`/`connection_config.go`'s public godoc restate the observable per-site
contract (panic recovered+counted+logged, Goexit logged-only-and-disconnects-or-ends-the-notifier).
None of the five states WHY a single `defer` cannot do what `runCallback`'s two frames do — the
comments describe the outcome, not the structural reason a naive one-frame version fails.
None collects, in one place, which goroutine each of the five callback sites actually runs on, or
draws the line around what this isolation does NOT cover.
That is the delta this entry records.

# How it works

**Why two frames, not one.**
A single `defer func() { if r := recover(); r != nil { onPanic(r) } else if !returned { onGoexit() }
}()` looks equivalent to `runCallback`'s actual two frames — an inner `defer recover()` wrapping
`fn()`, with an outer `defer` that checks `returned` — but it is not, for exactly the case
`runCallback`'s own doc names: a deferred call INSIDE `fn` that itself panics while a `runtime.Goexit`
`fn` started is still unwinding.
Go resumes an interrupted Goexit once the panic that interrupted it is recovered — but recovery
resumes the PENDING GOEXIT LOOP itself, not normal execution in the recovering frame.
The runtime's own recovery path explicitly refuses to jump past a pending Goexit
(`runtime.recovery`'s `if p.goexit` branch); instead it jumps back into Goexit's own defer-walking
loop, which can revisit stack frames it already visited while searching for the next
NOT-YET-INVOKED deferred call.
An already-invoked deferred call is never invoked a second time — each one is consumed the moment
it runs and cannot be found again.
In the one-frame version, that single `defer` IS the frame catching the nested panic: its `recover()`
call returns non-nil, it takes the `onPanic` branch, and there is no second, not-yet-invoked `defer`
left for the resumed Goexit loop to reach — `onGoexit` never fires, even though the goroutine is
(correctly) still exiting via Goexit.
`runCallback`'s inner `defer` is a SEPARATE frame from its outer one specifically so this nested
panic is caught and answered (`onPanic`) BEFORE the Goexit it interrupted resumes into the outer
frame, where `returned` is still `false` and `onGoexit` fires as the Goexit continues past it.
`TestRunCallback_GoexitWithDeferredPanic` (`hsms/handler_panic_test.go`) is the teeth-check: `fn`
calls `runtime.Goexit()` after registering `defer panic("nested panic raised while Goexit
unwinds")`, and the test asserts BOTH `onPanic` and `onGoexit` fired exactly once — the case a
one-frame `recover()` cannot reproduce.

**Five call sites, three execution roles.**

| Call site | Runs on | On Goexit |
|---|---|---|
| `DataMessageHandler` (`session.callDataHandler`) | the ingress caller — normally `hsmsss.recvLoop`, or a transport's own line-reading goroutine; a custom `TransportRuntime` can call `DeliverOwnedFrame`/`DeliverOwnedFrameFromGeneration` from any goroutine | disconnects the generation it ran for |
| `DecodeErrorHandler` (`session.callDecodeErrorHandler`) | same ingress-caller role | disconnects the generation it ran for |
| async send error handler (`connection.callAsyncSendErrorHandler`) | that generation's own per-generation async-sender goroutine | disconnects the generation it ran for |
| `StateChangeHandler` (`supervisor.callHandler`) | the supervisor's notifier goroutine | logs only — no per-generation transport to drop from the notifier's own vantage |
| lifecycle subscriber (`supervisor.callSub`) | the supervisor's notifier goroutine | logs only, same reason |

`callDataHandler`/`callDecodeErrorHandler` reach this isolation only when `s.rt` (the session's
`TransportRuntime`) also satisfies `handlerHost` via a type assertion — true for a real `*connection`.
Every in-package mock `TransportRuntime` a session-level unit test constructs, and a session built
with a nil `rt`, fails that assertion and calls the handler directly, unrecovered, exactly as before
this isolation existed.

The first three disconnect through `disconnectHandlerGeneration` → `connection.TCPDownFromGeneration(gen,
errHandlerGoexit, CauseHandlerExit)` — see [the TransitionCause injection-site
map](/hsms/transition-cause-injection-sites.md) for how that generation-identity match works and why
`gen` is threaded in from the caller (cutoff #1's already-resolved epoch, or `drainSendCh`'s own `e.id`)
rather than re-resolved inside `runCallback`'s `onGoexit` closure: by the time a Goexit-ending callback's
defer chain runs, the CURRENT generation may already be a live successor, and re-resolving there would
disconnect the WRONG one.
The notifier-side two log-only, because `notifier()` itself IS the goroutine `fn` runs on inline — its
own Goexit unwinds `notifier()`, whose `defer close(s.notifierDone)` still fires as part of that
unwind, ending the goroutine on its own; there is no separate generation to name from there.
`s.reportPanic`/`s.reportGoexit` are plain supervisor field writes `connection.Open` makes before `go
s.run()`/`go s.notifier()` start — the `go` statement's happens-before is what lets `callHandler`/`callSub`
read them without a lock; a supervisor built without a connection (unit tests) leaves both nil and
falls back to a no-op report.

**The recv goroutine's own join survives a Goexit too.**
`hsmsss.recvLoop` (`hsmsss/transport_recv.go`) opens with `defer g.recv.Done()`, registered before
any handler runs, so it is the LAST defer to fire as a `DataMessageHandler`/`DecodeErrorHandler`
Goexit unwinds the entire call stack back through `dispatchFrame` → `recvLoop`.
It still fires — the goroutine does not leak — but only after every intervening defer between the
handler and `recvLoop` has run: `runCallback`'s own frames, `disconnectHandlerGeneration`'s
`TCPDownFromGeneration` call (which, for a matching generation, can itself queue onto the FSM's
event channel), and the site's `logHandlerGoexit` call.
Completed Goexit unwinding therefore releases `g.recv`'s WaitGroup count, but it does not by itself
guarantee prompt completion of `tr.Stop`'s overall bounded join: that single ctx-bounded join also
waits, IN SEQUENCE, on `g.proc`, `g.linktest`, and `g.t7` (`hsmsss/transport.go` →
`(*transport).Stop`), so all four groups — not `g.recv` alone — must finish inside the one deadline.

**Disconnect-before-log, and why it needs its own guard.**
Every call site that can disconnect invokes `disconnectHandlerGeneration` (or its
`logHandlerGoexit`-only notifier counterpart) BEFORE the corresponding `logHandlerGoexit` call —
`connection_send.go`, `session.go`, and `supervisor.go` all order it that way.
`disconnectHandlerGeneration` wraps its own `TCPDownFromGeneration` call in `defer recover()`
specifically so a panicking user-supplied logger cannot escape and crash a goroutine that is already
unwinding via `runtime.Goexit`.
Two different log calls can panic here, with two different disconnect outcomes behind them.
For a MATCHING generation, `injectDisconnect` marks `commsFailure` and enqueues `evDisconnect`
(asynchronously — `injectFrom` only queues the event, it does not wait for teardown) before
`disconnectHandlerGeneration` returns, and only then does the call site's own `logHandlerGoexit`
call run.
For a STALE generation, `injectDisconnect`'s own Debug log fires instead, and no disconnect action
runs at all — `injectDisconnect` returns early, before touching `commsFailure` or `injectFrom`.
The recover changes nothing about either ordering; it only keeps whichever logger panics from
turning a Goexit report into a crash.
`logHandlerEvent`, the single guarded log call every report funnels through, carries the SAME
`defer recover()` pattern for the same reason, one layer further out.

# Invariants

- With `onPanic`/`onGoexit` reporters that return normally, `runCallback` itself never panics and
  never calls `runtime.Goexit`; the goroutine running it leaves in exactly one of three ways
  (return, panic recovered and reported, Goexit reported), never a fourth.
  Neither reporter call is recovered by `runCallback` itself: an `onPanic`/`onGoexit` that panics
  lets that panic escape `runCallback` uncaught.
  An `onPanic` that calls `runtime.Goexit` is then detected by the outer `!returned` defer, as a callback's own Goexit would be;
  an `onGoexit` that calls `runtime.Goexit` is already running inside that outer defer, so nothing reports it again.
- With normally returning reporters, a plain panic never also reports a Goexit, and a plain Goexit
  never also reports a panic; only a panic raised WHILE a Goexit unwinds reports both.
  A callback panic whose `onPanic` reporter itself calls `runtime.Goexit` also reports both, for the
  same structural reason.
- `gen == 0` is always a no-op for `disconnectHandlerGeneration` — 0 means the injection site named no
  generation (`TCPDownFromGeneration` treats 0 as "whatever is current," exactly wrong for a report
  that must target only the generation the exiting callback ran for).
- The disconnect action for a given report always completes before that report's log call is even
  attempted, regardless of whether the log call itself panics or Goexits.

# Failure modes

- Collapsing `runCallback`'s two frames into one silently drops `onGoexit` for the
  panic-raised-during-Goexit-unwind case.
  The goroutine still exits via Goexit exactly as before, and the nested panic itself is still
  reported — the single remaining `defer`'s `recover()` still catches it and still calls
  `onPanic` — but the Goexit-specific report, and the disconnect it drives at the three
  generation-disconnecting sites, are both lost.
  The concrete loss differs by site: the two ingress-caller sites (`DataMessageHandler`,
  `DecodeErrorHandler`) leave the generation with no receiver; the async-send-error-handler site
  leaves it with no sender draining `e.sendCh`.
  Neither loss stops an unrelated failure (a read error, a linktest timeout, ...) from still
  driving a reconnect through a different path — what is lost is specifically the signal this
  mechanism exists to provide, not every possible path to reconnect.
- Re-resolving the generation inside the Goexit-detecting defer instead of threading it in from the
  caller risks naming a live successor's generation rather than the one that actually exited.

# What this isolation does not cover

- **The transaction observer.** `WithTransactionObserver`'s own godoc (`hsms/connection_config.go`)
  states the hook runs synchronously on the CALLER's own goroutine.
  `WriteMessage`/`WriteMessageNoReply` (`hsms/connection_send.go`) call it directly, with no
  dedicated recovery boundary of their own — a different design choice from every site in the
  table above, not an oversight this entry's mechanism should be expected to reach.
  Whether a panicking observer is caught at all therefore depends entirely on the CALLER's own
  context: a `SendDataMessage` call made synchronously from INSIDE an already-isolated
  `DataMessageHandler` (`session.SendDataMessage` → `s.rt.WriteMessage`) has its observer's panic
  caught incidentally by that handler's own `runCallback` frame; the same call made from a bare,
  unwrapped goroutine crashes the process.
- **`WithDialer`/`WithListener`.** `secs1/config.go`'s godoc for both now states a panic there is an
  infrastructure-hook failure, not a callback failure: it propagates to `Open`'s caller during `Open`,
  or ends the process during a background reconnect — neither goes through `runCallback`.
- **A send on a closed registered channel.** `AddDataMessageChan`'s doc (`hsms/endpoint.go`) states the
  channel delivery itself is not a callback and is not recovered the way a panicking `DataMessageHandler`
  is: a send on a closed `ch` still panics the receive goroutine outright.
- **The logger itself, beyond its own guard.** `logHandlerEvent`'s `defer recover()` keeps a panicking
  logger from crashing the goroutine making ONE report; it is not a general guarantee that every logger
  call anywhere in `hsms` is isolated the same way.
- **`hsmstest.FakeEndpoint`.** `Deliver`/`DeliverDecodeError` (`hsms/hsmstest/endpoint.go`) deliberately do
  NOT recover a panicking handler — the fake exists to let a handler panic surface to the test, the
  opposite choice from the real connection's fan-out — pinned by
  `TestFakeEndpoint_Deliver_HandlerPanicPropagates` and
  `TestFakeEndpoint_DeliverDecodeError_HandlerPanicPropagates` (`hsms/hsmstest/panic_test.go`).

# Where to look

- the shared isolation primitive and its two frames: `hsms/handler_panic.go` → `runCallback`
- the teeth-check for the nested-panic-during-Goexit case: `hsms/handler_panic_test.go` →
  `TestRunCallback_GoexitWithDeferredPanic`
- the panic counter, the guarded log helpers, and the generation disconnect: `hsms/handler_panic.go` →
  `countHandlerPanic`, `logHandlerGoexit`, `logHandlerEvent`, `(*connection).disconnectHandlerGeneration`,
  `handlerHost`
- the three generation-disconnecting call sites: `hsms/session.go` → `(*session).callDataHandler`,
  `(*session).callDecodeErrorHandler`; `hsms/connection_send.go` → `(*connection).callAsyncSendErrorHandler`
- the two notifier-side, log-only call sites and the hook install: `hsms/supervisor.go` →
  `(*supervisor).callHandler`, `(*supervisor).callSub`, `supervisor.reportPanic`, `supervisor.reportGoexit`;
  `hsms/connection_lifecycle.go` → `(*connection).Open`
- the generation `gen` is resolved from before the handler call: `hsms/connection_runtime.go` →
  `(*connection).routeDataOn`
- the recv goroutine's own bounded join, surviving a handler Goexit via its own defer:
  `hsmsss/transport_recv.go` → `(*transport).recvLoop`
- the overall sequential join `g.recv`'s completion feeds into: `hsmsss/transport.go` →
  `(*transport).Stop`
- what stays outside this mechanism: `hsms/connection_config.go` → `WithTransactionObserver`;
  `secs1/config.go` → `WithDialer`, `WithListener`; `hsms/endpoint.go` → `AddDataMessageChan`;
  `hsms/hsmstest/endpoint.go` → `FakeEndpoint.Deliver`, `FakeEndpoint.DeliverDecodeError`
