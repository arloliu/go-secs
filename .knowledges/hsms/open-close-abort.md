---
type: Mechanic
title: How Close interrupts a blocked Open
description: The registration protocol that lets a Close unblock an Open parked in its Selected wait or its first dial, and how the interrupted Start failure is mapped to the error Open returns.
tags: [hsms, lifecycle, open, close, abort, dial, context]
status: stable
generated: {by: "claude/opus-5.5", at: 2026-09-25T13:05:39Z}
verified:
  - {by: "openai/gpt-5.6-terra", at: 2026-09-25T13:08:00Z}
sources:
  - {resource: hsms/connection_lifecycle.go, digest: sha256:dac8943b7389474b, revision: c00e1b5}
  - {resource: hsms/connection.go, digest: sha256:6b6b7d50cb9bae9e, revision: c00e1b5}
  - {resource: hsms/endpoint.go, digest: sha256:a01c1f116383460c, revision: c00e1b5}
  - {resource: internal/gencap/gencap.go, digest: sha256:388f19be3dd1fe52, revision: c00e1b5}
  - {resource: hsmsss/transport.go, digest: sha256:cb3594d212e03da1, revision: c00e1b5}
  - {resource: hsmsss/transport_active.go, digest: sha256:252154bd042e64d4, revision: c00e1b5}
  - {resource: secs1/transport.go, digest: sha256:17b2a87488b2f448, revision: c00e1b5}
---

# What it does

Answers: *how does a Close that cannot take `lifeMu` still stop the Open holding it,
and what does each side see afterwards?*
`Connection.Open` / `Connection.Close` (`hsms/endpoint.go`) state the contract:
the caller's ctx bounds an active transport's first dial in either mode,
and a concurrent Close fires an interrupt token.
When the Selected wait's abort arm or the bounded first dial observes it first, Open returns `ErrConnClosed`;
an already observed Selected state or a caller cancellation can win the wait instead,
and a background Open can still return nil.
The `(*connection).Open` doc and the `abortMu` / `pendingCloses` / `openAbort` / `firstDial` field docs each describe one piece.
None of them lays out the protocol as one sequence across Open, Close, and the two transports,
says what the waiting Close does once Open lets go (it differs between the two interruption points),
or names what breaks when an ordering is changed.
The public Open doc is also looser than the code in two places:
for a Start failure, a fired token wins,
and otherwise Open returns the caller's `ctx.Err()` only when the caller's bridge ran (`callerCut`)
and the Start error wraps `context.Canceled`
(the Selected wait returns `ctx.Err()` directly, independently of that mapping);
and under OpenBackground any first-dial failure seen while the caller's ctx is already done rolls back,
not only a dial that was still blocked when the ctx expired.

# How it works

**Close registers before it queues on `lifeMu`.**

1. Close takes `abortMu`, increments `pendingCloses`, fires `openAbort` if it is non-nil, and releases `abortMu`.
2. Close then blocks on `lifeMu`.
3. The moment it holds `lifeMu`, Close takes `abortMu` again and decrements `pendingCloses`,
   as a plain statement, not a defer, and before any of its own guards.
   Everything after that is the ordinary Close.

**Open publishes a one-shot token after the double-open guard.**

1. Under `lifeMu`, past the `ErrAlreadyOpen` check, Open builds a fresh `abortToken`.
2. Under `abortMu`, it pre-fires that token if `pendingCloses > 0`, then stores it in `openAbort`.
3. A defer registered after the `lifeMu` unlock defer clears `openAbort` to nil.
   Defers run in reverse order, so the clear always happens before `lifeMu` is released.

Registration and publication both run under `abortMu`, so exactly one of two orderings holds.
If Close registered first, it may have fired an earlier Open's token but cannot have seen this Open's unpublished one,
so Open pre-fires its own while the count is still positive.
If Open published first, Close finds the token and fires it.
Several parties can fire the same token:
two Closes, or Open's pre-fire and a later Close.
`abortToken.fire` closes the channel through a `sync.Once`, so the extra calls do nothing.

**Where Open observes the token.**

- *The Selected wait.*
  `waitSelected` selects on the token's channel next to the caller's ctx and `e.done`.
  On abort it returns `ErrConnClosed` without rolling anything back.
  The failed-wait contract (the lifecycle keeps running) is unchanged.
  The Close that fired the token is already blocked on `lifeMu`,
  and it does the normal, fully fenced teardown once Open returns.
- *The first dial.*
  Just before `tr.Start`, Open stores a `firstDialState` in `connection.firstDial`.
  It holds the epoch ctx it is about to pass to Start, the caller's ctx, and the token's channel.
  A deferred store clears it as soon as Start returns, including when Start panics.
  An in-module transport's active dial site asserts `t.rt` to `gencap.DialBounder`
  and passes the Start ctx and its own dial ctx to `BoundDial`.
  hsmsss's `Start` hands its ctx straight to `startActive`.
  secs1's `Start` derives `engineCtx` from its ctx,
  so it passes the original ctx to `startActive` as a separate `startCtx` argument.
  Both call `BoundDial` after applying their `WithConnectTimeout` wrap.
  `BoundDial` compares `startCtx` with the published `epochCtx` by interface identity.
  On a mismatch, or with nothing published, it returns the dial ctx unchanged with a no-op cancel.
  On a match it returns a child of the dial ctx that is also cancelled by two bridges.
  A `context.AfterFunc` on the caller's ctx sets `callerCut` and then cancels.
  A goroutine cancels on the token's channel, and exits on its own once the merged ctx is done.
  The transport defers the returned cancel until `startActive` exits, not until the dial returns.
  It then prevents a not-yet-started caller callback and cancels the merged ctx, but joins neither bridge.
  `callerCut` records that the caller callback ran, not that it was what cut the dial.

**How the Start failure is mapped.**
Only the dial is inside the bound.
The hsmsss Select procedure, secs1's synchronous `CommitSelected`, and every TCP-up commit are outside it.
So a dial cut by the bound always surfaces as a `tr.Start` error while the FSM is still NotConnected.
Open samples `ctx.Err()`, `tok.fired()`, and the local `fd.callerCut` right after Start fails.
It reads `callerCut` from its own `fd` because the connection's copy has already been cleared.
- The cold-active background retry now also requires `ctxErr == nil && !aborted`.
  Any Start failure seen after an abort, or while the caller's ctx is already done, takes the rollback instead.
- The rollback returns `mapAbortedStartErr(err, ctxErr, aborted, callerCut)`.
  A fired token gives `ErrConnClosed`, checked first.
  `callerCut` together with an error that wraps `context.Canceled` gives the caller's `ctxErr`.
  Anything else gives the raw error.
  A `WithConnectTimeout` expiry reaches the merged ctx as `DeadlineExceeded`, not `Canceled`.
  So a dial that timed out on its own keeps the transport's error,
  even if the caller's bridge fired afterwards and set `callerCut`.
- The Close that cut the dial then takes `lifeMu`.
  If no queued Open took `lifeMu` first and replaced the supervisor,
  it finds the rollback's `runDone` closed and returns the rollback's `shutdownErr` without a second teardown;
  otherwise it closes the successor cycle
  (see [how a shutdown joins the per-Open supervisor](/hsms/supervisor-join.md)).

**Why only the first dial.**
Reconnect dials use a different epoch ctx and stay unbound even if Open has not returned:
the cold-active retry loop is launched before Open returns,
and a drop during the Selected wait can start a reconnect while Open is still waiting.
Those dials run under their own generation's ctx, which that generation's teardown cancels.
Clearing `firstDial` after Start and matching by identity keep them unbound.
A reconnect generation's Start carries a different epoch ctx,
so it never matches, even while a `firstDial` is published.

# Invariants

- Lock order is `lifeMu` then `abortMu`, and never the reverse.
  Close takes `abortMu` alone before `lifeMu` and releases it before blocking.
  `abortMu` is never held across a blocking call.
- Close never returns between `pendingCloses++` and `lifeMu.Lock`.
  A fired token promises that a Close is already on its way to `lifeMu`,
  and `waitSelected`'s abort case relies on that promise to leave the lifecycle running.
- `pendingCloses` is decremented under `lifeMu`, immediately after acquiring it.
  Seen under `lifeMu`, a count above zero therefore means a registered Close has not yet acquired `lifeMu` and unregistered;
  it may still be descheduled between registering and calling `lifeMu.Lock`.
- `openAbort` is non-nil only while an Open holds `lifeMu` past the double-open guard,
  and it is cleared before that `lifeMu` is released.
- `firstDial` is non-nil only during Open's synchronous `tr.Start` call.
- The dial site passes `BoundDial` the exact ctx value `Start` received, never a derived ctx.
- `callerCut` is set only by the caller-ctx bridge, before it cancels.
  Open credits the caller only on that flag, never on a context sentinel alone.

# Failure modes

- **Decrementing `pendingCloses` before acquiring `lifeMu`.**
  An Open that publishes in that window sees zero and does not pre-fire.
  It then parks in its wait or dial, and the Close parks behind it.
  This is the hang the mechanism exists to remove.
- **Decrementing after `lifeMu` is released.**
  A queued Open can take `lifeMu` while the count still includes a Close that has already finished.
  That Open pre-fires itself with no Close waiting,
  so its first dial is cut or its Selected wait returns a spurious `ErrConnClosed`,
  unless it observes Selected first or a concurrent caller cancellation wins with `ctx.Err()`.
- **Clearing `openAbort` after `lifeMu` is released.**
  The clear can wipe the next Open's freshly published token.
  A Close that registers after that finds nil, fires nothing, and blocks behind the Open again.
- **Closing the token channel directly instead of through `sync.Once`.**
  Two Closes, or Open's pre-fire plus a later Close, panic with a double close.
- **Handing `BoundDial` `engineCtx` in secs1**, or any derived ctx in a transport.
  The identity check never matches, so neither the caller's ctx nor Close can cut the first dial.
  Nothing reports an error: Close waits behind the dial for the connect timeout, or the OS timeout without one.
- **Crediting the caller on `ctx.Err() != nil` alone.**
  A connect-timeout expiry, or a dialer error unrelated to cancellation, is reported as the caller's own cancellation.
- **Losing both the post-Start clear and the identity match.**
  Reconnect dials become bound to the original Open's ctx.
  Callers routinely cancel that ctx once Open returns,
  so reconnect dials become exposed to the original caller's cancellation;
  whether a given dial is cut depends on the bridge's scheduling racing the dial's success.

# Gotchas

- One Close can abort two Opens in a row when it cuts the first one's dial.
  That rollback sets `shutdown`, so a queued second Open passes the double-open guard.
  If that Open wins `lifeMu` before the Close does, it sees the still-positive count and pre-fires its own token.
  An Open cut in its Selected wait leaves the lifecycle running instead,
  so a queued Open behind it returns `ErrAlreadyOpen` before it publishes anything.
- A pre-fired token does not guarantee that Open fails.
  It only enables interruption at the two observation points;
  it does not make the whole Open nonblocking, which still performs startup and any rollback joins.
  Under OpenBackground, a passive Open, or an active dial that completes before the abort bridge cancels, still returns nil.
  Under OpenWaitSelected, the pre-fired token makes `waitSelected`'s abort arm ready,
  but a Selected state it observes first returns nil (secs1 can commit Selected synchronously inside Start),
  and a concurrent caller cancellation can instead return `ctx.Err()`.
  The pending Close then tears that cycle down.
- A `StateChangeHandler` that calls Close during an Open's Selected wait now releases that Open at once,
  with `ErrConnClosed`
  unless the wait observes Selected first (nil) or a concurrent caller cancellation wins (`ctx.Err()`).
  The Close then joins the notifier it is running on, so it still ends in `ErrCloseTimeout` after the close timeout.
- A custom `DialFunc` that ignores its ctx is not interruptible,
  and a custom `TransportRuntime` that does not implement `gencap.DialBounder` is not bound at all.
  `hsms/endpoint.go` states both limits.

# Where to look

- the token: `hsms/connection_lifecycle.go` → `abortToken`, `newAbortToken`, `(*abortToken).fire`, `(*abortToken).fired`
- registration state: `hsms/connection.go` → `connection.abortMu`, `connection.pendingCloses`,
  `connection.openAbort`, `connection.firstDial`
- publish, pre-fire, clear, and Start-error handling: `hsms/connection_lifecycle.go` → `(*connection).Open`
- register and unregister: `hsms/connection_lifecycle.go` → `(*connection).Close`
- the wait's abort case: `hsms/connection_lifecycle.go` → `(*connection).waitSelected`
- the dial bound: `hsms/connection_lifecycle.go` → `firstDialState`, `(*connection).BoundDial`
- the error mapping: `hsms/connection_lifecycle.go` → `mapAbortedStartErr`
- the capability: `internal/gencap/gencap.go` → `DialBounder`
- hsmsss dial site: `hsmsss/transport.go` → `(*transport).Start`;
  `hsmsss/transport_active.go` → `(*transport).startActive`
- secs1 dial site: `secs1/transport.go` → `(*transport).Start`, `(*transport).startActive`
- public contract: `hsms/endpoint.go` → `Connection`
