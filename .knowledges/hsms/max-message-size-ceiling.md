---
type: Mechanic
title: The send-side MaxMessageSize ceiling's enforcement topology
description: Where the size check actually runs relative to writeMu and the send paths, and why async accounting has no exclusion list at all.
tags: [hsms, send, metrics, limits]
status: stable
generated: {by: "claude/sonnet-5", at: 2026-08-12T08:33:19Z}
verified:
  - {by: "claude/sonnet-5", at: 2026-08-12T08:43:44Z}
sources:
  - {resource: hsms/connection_send.go, digest: sha256:9b1ccf21a9d24c0d, revision: 038319b}
  - {resource: hsms/errors.go, digest: sha256:ffa4b24a88de6662, revision: 0965bba}
  - {resource: internal/wire/body.go, digest: sha256:2fa6355ec9459b7c, revision: b1bb17b}
---

# What it does

`hsms/doc.go` (item 5) and the `AsyncSendErrCount`/`DataMsgErrCount`/`WithAsyncSendErrorHandler`
godocs already cover the WHAT thoroughly: one enforcement point at the wire-framing layer, sync
paths return `ErrMessageTooLarge` directly, async paths surface it through the async counter and
handler instead of the call's return value, and `DataMsgErrCount` deliberately excludes it as a
caller-side construction error.
None of that states WHERE the single check actually sits relative to `writeMu`, WHY a control frame
cannot structurally reach it at all, or WHY the async path's accounting has no exclusion list to
begin with — it isn't that `ErrMessageTooLarge` was deliberately added to some async allow-list, it's
that the async path never had a classification function in the first place.

# How it works

**One check, one function, before the lock.**
`buildFrameBuffers` is the sole enforcement point — every send path (`sendWaitReply`, `sendNoReply`,
`SendAsync`'s eventual drain, and the internal control-message async sends) funnels through
`writeFrame`, which calls `buildFrameBuffers` as its very first step, BEFORE `e.writeMu.Lock()`.
An oversized message therefore never contends the write mutex at all — the rejection takes no lock,
no conn access, and no interaction with the I1 stale-epoch guard or the B2 Selected re-check that
live inside the locked section.

It is not free, though.
`buildFrameBuffers` derives the length by summing `dm.body.Buffers()`, and for a constructed
(tree) body that call is what materializes the encoding: `Buffers` returns the memoized encoding and
memoizes it on first use (`internal/wire/body.go` → `(*treeBody).encoded`).
So the first send of an oversized constructed message encodes and allocates the whole body before the
check rejects it; only a re-send, or a raw-frame body that is already bytes, is a pure length sum.
The alternative the comment on `buildFrameBuffers` rejects is worse still — `Body.Len` is an
un-memoized recursive `EncodedLen` walk.

**Why a control frame cannot trip it.**
`buildFrameBuffers` only measures a body length when `msg` type-asserts to `*DataMessage` AND that
body's summed buffer length is `n > 0`.
A `*ControlMessage` fails the type assertion outright and falls straight to the header-only,
fixed-10-byte path; the size branch is structurally unreachable for it, not merely satisfied.
The comment on `buildFrameBuffers` states this exemption follows from the frame arithmetic itself: a
control frame is always the fixed 14 bytes, orders of magnitude under any realistic ceiling.

**Why the sync/async divergence is a queueing fact, not a policy choice.**
`SendAsync` enqueues `msg` onto `e.sendCh` and returns `nil` on a successful enqueue — it performs
no size check itself, so a caller can receive a nil error for a message that is already doomed.
The rejection only happens later, whenever `drainSendCh` dequeues that request and calls `writeFrame`
on the per-generation async-sender goroutine — an interval bounded only by how backed up the queue
is.
That is the entire reason the failure surfaces through `AsyncSendErrCount` /
`WithAsyncSendErrorHandler` instead of `SendAsync`'s return value: by the time the check runs, the
call that would report it synchronously has already returned.

**Why async accounting has no exclusion list.**
The sync paths route every `writeFrame` failure through `isCountedSendErr` before counting it —
a policy function that excludes `ErrNotSelectedState`, `ErrConnClosed`, `ErrMessageTooLarge`, and
caller-context cancellation from `DataMsgErrCount` (see
[send error accounting](/hsms/send-error-accounting.md)).
`drainSendCh` has no equivalent call: it unconditionally increments `AsyncSendErrCount` and invokes
the handler for ANY non-nil `writeFrame` error — a genuine transport failure, a B2 `ErrNotSelectedState`
drop that raced the queue, or `ErrMessageTooLarge`, all counted identically.
The godoc's "or were rejected before the write for exceeding MaxMessageSize" phrasing describes one
member of that unfiltered set, not a special case carved out for it.

# Invariants

- The size check runs exactly once per frame, inside `buildFrameBuffers`, before any lock or conn
  access — an oversized message costs no mutex contention and touches no live connection state.
  It does cost the body encode, since the `Buffers()` call the check sums is also the call that
  memoizes that encoding.
- A `*ControlMessage` can never reach the size branch; the type assertion that guards it is the whole
  exemption, not a separate check.
- `SendAsync`'s return value reports only enqueue-boundary outcomes (`ErrNotOpen`,
  `ErrNotSelectedState`, `ErrConnClosed`, caller-ctx cancellation) — it can never report
  `ErrMessageTooLarge`, because the check has not run yet when it returns.
- `drainSendCh`'s counting is unconditional by construction (no `isCountedSendErr`-style gate), so
  any future change that gives the async path a classification function is itself the mechanic
  change, not a bug fix to this one.

# Failure modes

- Moving the size check to after `e.writeMu.Lock()` (e.g., "simplify by checking inside the locked
  section like B2 does") would make every oversized send attempt pay for lock acquisition it can
  never use productively — a self-inflicted contention cost on the hot path for a message that was
  always going to be rejected.
- Reading the pre-lock position as "an oversized send is nearly free" is wrong on the constructed
  path: the encode already happened inside the `Buffers()` call the check reads.
  A caller looping on oversized sends pays a full encode + allocation per attempt, with no counter
  (`DataMsgErrCount` excludes `ErrMessageTooLarge`) to show for it.
- Assuming `SendAsync`'s `nil` return means the message is well-formed and will reach the wire is
  incorrect for size specifically — only `WithAsyncSendErrorHandler` or a rising `AsyncSendErrCount`
  reveals a size rejection on that path.
- Adding a new `drainSendCh`-only error class that should NOT count (mirroring one of
  `isCountedSendErr`'s sync-path exclusions) requires adding a real filter there — there is currently
  no hook to special-case anything, so it would silently count until one is added.

# Where to look

- the single check, before the lock: `hsms/connection_send.go` → `buildFrameBuffers`, `(*connection).writeFrame`
- the structural control-frame exemption: `hsms/connection_send.go` → `buildFrameBuffers`
- the enqueue-then-drain split: `hsms/connection_send.go` → `(*connection).SendAsync`, `(*connection).drainSendCh`
- the unconditional async counting: `hsms/connection_send.go` → `(*connection).drainSendCh`
- the encode the check pays for: `internal/wire/body.go` → `(*treeBody).encoded`, `(*treeBody).Buffers`
- the sentinel: `hsms/errors.go` → `ErrMessageTooLarge`
