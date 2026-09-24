---
type: Mechanic
title: How a shutdown joins the per-Open supervisor
description: Why Close joins the FSM goroutine unbounded but the notifier only up to the close timeout, and why the join signals live on the supervisor rather than the connection.
tags: [hsms, lifecycle, supervisor, close, notifier, generations]
status: draft
generated: {by: "claude/opus-5.5", at: 2026-09-24T04:19:05Z}
sources:
  - {resource: hsms/connection_lifecycle.go, digest: sha256:0104b09786d9206c, revision: fb8d9cd}
  - {resource: hsms/supervisor.go, digest: sha256:3ad82a5b74e7ff78, revision: fb8d9cd}
  - {resource: hsms/state.go, digest: sha256:b0c58d8c774973d2, revision: fb8d9cd}
---

# What it does

Answers: *what does Close wait for after the epoch is torn down, and what bounds that wait?*
The `StateChangeHandler` doc (`hsms/state.go`) and the `Close` / `SubscribeLifecycle` docs state the observable contract:
a Close from inside a callback returns ErrCloseTimeout,
and an abandoned callback's goroutine keeps delivering its cycle's transitions.
They do not say which goroutine is joined how, why the FSM join is deliberately unbounded, where the deadline starts,
or how a re-Close finds the first result.
This entry records that.

# How it works

Each Open builds a fresh supervisor
and starts two goroutines on it, `run()` (the FSM) and `notifier()` (user callbacks).
Each closes its own channel on exit: `run()` closes `runDone` (and `notify`), `notifier()` closes `notifierDone`.
There is no connection-scoped WaitGroup.

`joinSupervisor(s, deadline)` is the only caller of `s.stop()` in production, and both shutdown paths go through it:

1. `s.stop()` closes `stopCh`; `run()` returns, which closes `notify` and `runDone`.
2. `<-s.runDone` — unbounded.
3. If `notifierDone` is already closed, return nil (observed completion beats an expired deadline).
4. Otherwise wait for `notifierDone` until `deadline`;
   on expiry return `errNotifierTimeout`, which wraps `ErrCloseTimeout`, and abandon the notifier.

`Close` samples `closeTimeout` from the live config right after its idempotent short-circuit,
so the notifier gets whatever remains after the epoch join (step 3 covers a remainder of zero or less).
It keeps the epoch join's error if there is one (`firstErr`), then stores the result in `s.shutdownErr`.
Open's rollback runs on a `tr.Start` failure that is not the cold-active background retry.
It takes its own deadline at rollback start, stores the same combined result in `s.shutdownErr`,
and still returns the `tr.Start` error.

A later Close sees `runDone` closed and returns `s.shutdownErr` without touching the epoch or supervisor again.

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
- rollback's use of the join: `hsms/connection_lifecycle.go` → `(*connection).Open`
- the timeout error: `hsms/connection_lifecycle.go` → `errNotifierTimeout`
- completion signals: `hsms/supervisor.go` → `(*supervisor).run`, `(*supervisor).notifier`
- stop signal: `hsms/supervisor.go` → `(*supervisor).stop`
- public contract: `hsms/state.go` → `StateChangeHandler`
