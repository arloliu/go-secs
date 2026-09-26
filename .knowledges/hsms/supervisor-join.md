---
type: Mechanic
title: How a shutdown joins the per-Open supervisor
description: Why Close joins the FSM goroutine unbounded but the notifier only up to the close timeout, and why the join signals live on the supervisor rather than the connection.
tags: [hsms, lifecycle, supervisor, close, notifier, generations]
status: stable
generated: {by: "claude/opus-5.5", at: 2026-09-26T07:35:00Z}
verified:
  - {by: "openai/gpt-5.6-terra", at: 2026-09-26T07:51:47Z}
sources:
  - {resource: hsms/connection_lifecycle.go, digest: sha256:e6d0d8d5bdf02ea2, revision: f7a5927}
  - {resource: hsms/supervisor.go, digest: sha256:1717fd0875937ae8, revision: f7a5927}
  - {resource: hsms/state.go, digest: sha256:f467c560ffea5807, revision: d244104}
  - {resource: hsms/connection.go, digest: sha256:d45005d0dcf9540c, revision: 4be2062}
  - {resource: hsms/handler_panic.go, digest: sha256:7c995367269a8805, revision: d244104}
  - {resource: hsms/endpoint.go, digest: sha256:b75d6a0370642b02, revision: cc82a06}
---

# What it does

Answers: *what does Close wait for after the epoch is torn down, and what bounds that wait?*
The `StateChangeHandler` doc (`hsms/state.go`) and the `Close` / `SubscribeLifecycle` docs state the observable contract:
a Close from inside a callback returns ErrCloseTimeout,
and an abandoned callback's goroutine keeps delivering its cycle's transitions.
They do not say which goroutine is joined how, why the FSM join is deliberately unbounded, where the deadline starts,
or how a re-Close finds the first result.
This entry records that.
The ErrCloseTimeout clause is narrower in the code than it reads:
among Closes issued from a callback, only one shutting down its own current cycle joins itself,
while a Close from any goroutine can return ErrCloseTimeout when a handler or subscriber stays blocked,
and an epoch-join error still takes precedence over the timeout.
A callback on an earlier cycle's abandoned notifier either gets that cycle's cached result
or closes the successor cycle,
and never joins itself.

# How it works

Each Open builds a fresh supervisor
and starts two goroutines on it, `run()` (the FSM) and `notifier()` (user callbacks).
Each closes its own channel on exit: `run()` closes `runDone` (and `notify`), `notifier()` closes `notifierDone`.
No connection-scoped WaitGroup joins these two supervisor goroutines —
each supervisor cycle's join lives on `runDone`/`notifierDone`, not on a shared counter.
A separate connection-scoped WaitGroup does exist for a different goroutine class:
`connection.connectLoopWg` (`hsms/connection.go`) joins reconnect loops,
and `Close` waits on it after `joinSupervisor` has joined the current cycle's FSM
and either joined or already abandoned its notifier
(see `Close`, below);
a failed Open's rollback waits on it at the same point.

`joinSupervisor(s, deadline)` is the only caller of `s.stop()` in production, and both shutdown paths go through it:

1. `s.stop()` closes `stopCh`; `run()` returns, which closes `notify` and `runDone`.
2. `<-s.runDone` — unbounded.
3. If `notifierDone` is already closed, return nil (observed completion beats an expired deadline).
4. Otherwise wait for `notifierDone` until `deadline`;
   on expiry return `errNotifierTimeout`, which wraps `ErrCloseTimeout`, and abandon the notifier.

`Close` samples `closeTimeout` from the live config right after its idempotent short-circuit,
so the notifier gets whatever remains after the epoch join (step 3 covers a remainder of zero or less).
It keeps the epoch join's error if there is one (`firstErr`), then stores the result in `s.shutdownErr`.
Open's rollback runs on a `tr.Start` failure that is not the cold-active background retry;
a Start failure seen after a Close fired this Open's abort token,
or while the caller's ctx is already done, always takes the rollback, never that retry,
and so does a Start that failed after a TCP-up commit succeeded on its generation.
The rollback (`rollbackFailedOpen`) fences reconnect the way Close does —
it bumps `reconnectGen`, sets `shutdown`, and re-pins `cur` under `publishMu`, then closes `reconnectCancel` —
and tears down the pinned generation through `requestClose`,
plus the failed generation separately if a reconnect loop had already replaced it.
It takes its own deadline just before that teardown and stores the same combined result in `s.shutdownErr`,
but does not return it.
After the supervisor join it waits on `connectLoopWg`, so no reconnect loop outlives the failed Open.
It returns the `tr.Start` error mapped by `mapAbortedStartErr`:
`ErrConnClosed` when a Close fired this Open's abort token (sampled, checked first),
otherwise the sampled caller `ctxErr` only when `callerCut` is true and the Start error wraps `context.Canceled`,
otherwise the raw error.
A Close that cut the dial is by then parked on `lifeMu`;
once the rollback releases it, that Close finds `runDone` closed and returns the rollback's `shutdownErr`
(see [how Close interrupts a blocked Open](/hsms/open-close-abort.md)).
This cached-result behavior applies only while the rolled-back supervisor remains current;
a queued Open that takes `lifeMu` first replaces it, and the Close then handles that successor cycle.

While that supervisor remains current,
a later Close sees `runDone` closed and returns `s.shutdownErr` without touching the epoch or supervisor again.

**A callback ending notifier() early was always possible; as of `d244104` it is reported instead of silent.**
`callHandler`/`callSub` now run the `StateChangeHandler`/subscriber `fn` through `runCallback`
(`hsms/handler_panic.go`) instead of a bare `defer recover()`.
Provided the panic-report hook and the configured logger return normally,
a recovered panic does not touch the join: it is counted (`ConnectionMetrics.HandlerPanicCount`)
and logged, `notifier()`'s `for sc := range s.notify` loop continues, and the rest of the cycle's
callbacks still run.
A logger that calls `runtime.Goexit` while reporting the panic instead ends the notifier through `runCallback`'s outer Goexit path,
and `notifierDone` then closes — see [the callback panic/Goexit isolation entry](/hsms/handler-panic-goexit-isolation.md)
for the mechanism.
A `runtime.Goexit` from `fn` ends `notifier()` exactly as it always did — `runCallback` cannot recover a
Goexit, only detect it — so `notifier()`'s own `defer close(s.notifierDone)` still fires as the
goroutine unwinds.
What changed is only that this is now logged (`reportGoexit`, not counted) instead of ending silently.
Because `notifierDone` closes on the callback's own unwind rather than on a Close timeout, once
Goexit unwinding reaches `notifier()`'s own completion defer, a `joinSupervisor` call already in
flight (or one that starts moments later) can find step 3's `notifierDone` already closed instead of
reaching step 4's timeout — this is the concrete reason `SubscribeLifecycle`'s and
`StateChangeHandler`'s own godoc can now say "Close still completes".
That is not a guarantee: the unwind between the Goexit and `notifierDone`'s close still runs
`reportCallbackGoexit` → `reportGoexit` → a guarded, but user-supplied, logger call, which can block.
A notifier-side Goexit performs no generation disconnect — only the data, decode-error, and async-send-error sites do.
If that unwind has not finished by the deadline, the notifier join can still time out exactly like
the "Abandoned notifier delivering late" failure mode below, which is about Close's timeout giving up
on a *still-running* notifier.

# Invariants

- `run()` never calls a `StateChangeHandler` or lifecycle subscriber; all of them run on `notifier()`.
  That split is what lets one join be unbounded and the other bounded.
- The FSM join stays unbounded.
  `react` loads `c.cur`, so an abandoned `run()` could react against the next cycle's epoch.
- Join signals are per supervisor.
  A notifier abandoned in cycle N must not be counted by cycle N+1's join.
  A shared `WaitGroup` fails here:
  a timeout wrapper leaves N's `Done` pending, so N+1's Close waits on N's straggler.
- `shutdownErr` is written once and read only under `lifeMu`.
  `runDone` closes inside `joinSupervisor` before the write, so it does not publish the value.
  Only the lock orders the two.
- Every path that closes `runDone` writes `shutdownErr` before releasing `lifeMu`.
  A new path that calls `s.stop()` without doing so makes a re-Close return nil after a timeout.
- `s.reportPanic`/`s.reportGoexit` are plain field writes in `Open`, made before `go s.run()`/`go
  s.notifier()` start — the `go` statement itself is what gives those writes happens-before
  visibility to the two new goroutines, so `callHandler`/`callSub` never observe a nil hook on a
  production supervisor.
  A supervisor built without a connection (unit tests) leaves both nil, and
  `callHandler`/`callSub` fall back to a no-op report — the same silent isolation these sites had
  before `d244104`.

# Failure modes

- Unbounded notifier join: a callback that calls Close waits for itself forever while holding `lifeMu`,
  and every later Open and Close hangs.
  This was the pre-`fb8d9cd` behavior.
- Connection-scoped join counter: after an abandoned notifier,
  the next cycle's clean Close returns ErrCloseTimeout (or waits indefinitely)
  because it is waiting for the previous cycle's goroutine.
- Re-Close returning `e.wait()` instead of `shutdownErr`:
  after a notifier timeout the epoch joined cleanly,
  so a second Close reports nil although the first reported ErrCloseTimeout.
- Abandoned notifier delivering late:
  after its callback returns, it drains its closed `notify` buffer through the connection-persistent handler and subscription slices.
  It can run the same callback concurrently with a reopened cycle's notifier,
  and its transitions can arrive after the new cycle's.
  This is documented public behavior, not a bug.

# Where to look

- the join: `hsms/connection_lifecycle.go` → `(*connection).joinSupervisor`
- Close's deadline, precedence, and cached result: `hsms/connection_lifecycle.go` → `(*connection).Close`
- rollback's use of the join, its reconnect fence, and its `connectLoopWg` wait: `hsms/connection_lifecycle.go` → `(*connection).rollbackFailedOpen`
- the error rollback returns: `hsms/connection_lifecycle.go` → `mapAbortedStartErr`
- the timeout error: `hsms/connection_lifecycle.go` → `errNotifierTimeout`
- completion signals: `hsms/supervisor.go` → `(*supervisor).run`, `(*supervisor).notifier`
- the separate reconnect-loop join: `hsms/connection.go` → `connection.connectLoopWg`
- stop signal: `hsms/supervisor.go` → `(*supervisor).stop`
- public contract: `hsms/state.go` → `StateChangeHandler`
- the callback isolation `callHandler`/`callSub` now run under, and the hook install: `hsms/supervisor.go` →
  `(*supervisor).callHandler`, `(*supervisor).callSub`, `supervisor.reportPanic`, `supervisor.reportGoexit`;
  `hsms/connection_lifecycle.go` → `(*connection).Open`
- the shared panic/Goexit isolation primitive itself: `hsms/handler_panic.go` → `runCallback`
