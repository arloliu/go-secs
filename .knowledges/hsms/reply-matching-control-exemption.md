---
type: Mechanic
title: The reply registry's two-legged control exemption
description: Why registration-side isData and result-side *DataMessage assertion are two independent gates, not one, and what breaks when only one survives.
tags: [hsms, reply-correlation, e37, control]
status: stable
generated: {by: "claude/sonnet-5", at: 2026-09-25T02:38:18Z}
verified:
  - {by: "openai/gpt-5.6-terra", at: 2026-09-25T03:13:36Z}
sources:
  - {resource: hsms/connection_send.go, digest: sha256:e485168d1a431fe7, revision: d244104}
  - {resource: hsms/reply_registry.go, digest: sha256:20d02d66955eca24, revision: a7ff4a8}
  - {resource: hsms/connection_runtime.go, digest: sha256:dce97e0bf7fc6616, revision: d244104}
  - {resource: hsmsss/integration_reply_matching_test.go, digest: sha256:e3ff71163fa0ec13, revision: a7ff4a8}
---

# What it does

`docs/specs/secs2-hsms-conformance-audit.md` (D1) and `reply_registry.go`'s own `route` comment now
state the two-legged shape directly:
a candidate is compared only when the registration is for a DATA primary (leg 1, `want.isData`)
AND the routed result itself carries a `*DataMessage` (leg 2),
and a DATA registration answered by a `*ControlMessage` is reported as a miss rather than compared.
What neither states is which test catches which SINGLE-leg removal,
or what the specific historical mutant that leg 2 alone does not catch actually did.
That is the delta this entry records:
dropping leg 2 alone is invisible to the two obvious round-trip tests;
dropping leg 1 alone is not,
since leg 1 also gates the DATA-registration `*ControlMessage`-miss check those same tests exercise;
dropping both at once reproduces the shape of the v2.0.1 field bug this mechanism fixed,
by way of a documented historical mutant that classifies a candidate lacking data fields as a
mismatch outright rather than genuinely comparing one.

# How it works

**Registration leg.**
`sendWaitReply` (`connection_send.go`) resolves the current epoch and delegates to `sendWaitReplyOn`,
which computes `dm, isData := msg.(*DataMessage)`.
For a control primary (`dm == nil`, e.g. Select.req/Deselect.req/Linktest.req) the `stream`/`function`
values passed to `register` are left at their zero defaults — they are stored but never read, because
`isData` is false.
`register` stores `{ch, stream, function, isData}` in a `replyWaiter`.

**Result leg.**
`route` (`reply_registry.go`) only enters the comparison branch when **both** hold at once:
`w.isData` is true, AND `dm, ok := res.msg.(*DataMessage)` succeeds against the routed result.
A field-less `*RejectError` result (`res.msg == nil`
— `routeReplyOn`'s `RejectReqType` branch in `connection_runtime.go`, reached via both `RouteReply` and `RouteReplyFromGeneration`, builds exactly this for a peer Reject.req)
bypasses comparison regardless of `isData` (delivery is a non-blocking send into the waiter's one-slot buffer) —
a peer Reject.req is a terminal rejection of the primary (E37 §8.3.11), not a reply to validate.
A `*ControlMessage` result (Select.rsp/Deselect.rsp/Linktest.rsp) is treated differently depending on registration direction:
a control registration (`isData` false) delivers it uncompared.
A DATA registration (`isData` true) treats it as a miss —
not delivered, the registration not consumed, and not counted as a mismatch —
because a control response has no Stream()/Function() to validate against a data primary in the first place (E37 §9.4.1), pinned by `TestReplyRegistry_DataRegistration_ControlResultMustMiss` and `TestReplyMatching_DataPrimary_ControlResponseMustMiss`.

**Which door each leg alone closes** (from `hsmsss/integration_reply_matching_test.go`'s teeth
comment and `hsms/reply_matching_test.go`'s
`TestReplyMatching_ControlRegistration_DataSecondaryDeliveredUncompared`):

- Leg 2 alone (the `*DataMessage` type assertion) does not decide whether a `*ControlMessage` result reaches delivery:
  the gate ahead of it already reports a miss for one on a DATA registration,
  so leg 2 never sees a `*ControlMessage` there,
  and the whole `if w.isData` block is skipped for a CONTROL registration regardless of leg 2 (leg 1 false).
  What leg 2 alone still does is separate a genuine `*DataMessage` secondary (compared against the registered primary)
  from a field-less `*RejectError` result (`res.msg == nil`, delivered uncompared) —
  a comparison switch inside the DATA-registration path, not a door of its own.
- Leg 1 alone (`isData`) closes the "reply to a DATA primary whose System Bytes collide with an
  open control transaction" door: a genuine `*DataMessage` secondary passes leg 2 cleanly, so only
  a control registration's `isData == false` keeps it uncompared.
- Removing **both** reproduces the v2.0.1 field-bug shape, but not through an actual stream/function
  comparison: a `*ControlMessage` has no `Stream()`/`Function()` to compare in the first place.
  The documented historical mutant (`hsmsss/integration_reply_matching_test.go`'s TEETH comment)
  instead replaces the exemption logic with unconditional MISMATCH CLASSIFICATION for any result
  lacking data fields — a `*ControlMessage`, or a field-less `*RejectError` — and additionally
  bypasses the DATA-registration `*ControlMessage`-miss gate ahead of it.
  That mutant rejects every Select.rsp under strict mode (`CommitSelected` never runs, so the FSM
  loops NotSelected → NotConnect) and floods `ReplyMismatchCount` under the default posture.
  The outcome depends on that explicit classification and on bypassing the miss gate;
  merely removing the two conditions does not by itself specify equivalent behavior.

The function match itself admits primary+1 **or** 0 — the SxF0 abort secondary (E5 §7.2/§10.4.1).
That exception is function-only: a wrong-stream F0 still mismatches on stream.

# Invariants

- A comparison runs only when both legs hold;
  a RejectError result, or a `*ControlMessage` result answering a CONTROL registration, bypasses comparison, with no mismatch recorded; `route` then attempts a non-blocking send into the waiter's one-slot buffer.
- The one exception: a `*ControlMessage` result answering a DATA registration is always a miss —
  not delivered, registration not consumed, no mismatch recorded —
  for a DATA registration (leg 1 true), in either strict mode.
- A strict-mode miss does not consume the registration — a later conforming reply can still complete
  the transaction before its T3/T6 timer fires.
- SessionID is deliberately excluded from this registry-level check; `WithSessionIDValidation` is a
  separate, earlier seam (`DeliverOwnedFrame`'s `checkSessionID`).

# Failure modes

- **Removing leg 1 (`isData`) alone** — e.g. replacing `if w.isData` with `if true` — is caught immediately:
  the DATA-registration `*ControlMessage`-miss gate then runs unconditionally,
  so a Select.rsp or Linktest.rsp answering its OWN control registration is intercepted ahead of delivery and reported as a miss instead.
  `TestReplyMatching_StrictMode_SelectReachesSelected` and `TestReplyMatching_DefaultMode_LinktestRoundTripZeroMismatch`
  (`hsmsss/integration_reply_matching_test.go`) both hit their 10-second timeouts,
  since neither Select nor Linktest ever completes.
- **Removing leg 2 (the `*DataMessage` assertion) alone** only matters for the field-less `*RejectError` result:
  a peer Reject.req answering a data primary (`res.msg == nil`) would then reach the stream/function comparison unguarded instead of falling through uncompared, caught by `TestReplyRegistry_ControlExemption_ResultLeg`.
  A `*ControlMessage` result does not depend on leg 2 at all —
  the gate ahead of it already reports a miss for one on a DATA registration regardless of leg 2's presence.
- **Removing both** — see the v2.0.1 shape under How it works: an end-to-end failure to reach
  Selected under strict mode, or a `ReplyMismatchCount` that climbs on every successful control
  round trip under the default.
- **Removing the DATA-registration `*ControlMessage` miss gate alone**
  (the check `route` runs ahead of leg 2, before the `*DataMessage` type assertion)
  is undetected by the two control-round-trip integration tests above
  (`TestReplyMatching_StrictMode_SelectReachesSelected`, `TestReplyMatching_DefaultMode_LinktestRoundTripZeroMismatch`) —
  leg 1 and leg 2 both still hold exactly as described,
  so neither of those two tests catches its removal.
  It bites when a peer answers a data primary's System Bytes with a control response (Select.rsp/Linktest.rsp):
  `SendDataMessage` then returns `(nil, nil)`, a false success.
  A genuine reply arriving after the sender's deferred deregistration then becomes unsolicited;
  one arriving earlier can instead hit the still-registered waiter and be consumed or discarded by it.
  Detected instead by `TestReplyRegistry_DataRegistration_ControlResultMustMiss`, `TestReplyMatching_DataPrimary_ControlResponseMustMiss`,
  and `TestReplyMatching_ControlResponseOnDataPrimary_RejectedThenGenuineReplyCompletes`.

# Where to look

- registration site, `isData` computed by type assertion: `hsms/connection_send.go` → `(*connection).sendWaitReplyOn` (called by `(*connection).sendWaitReply`)
- storage and the two-legged gate: `hsms/reply_registry.go` → `replyWaiter`, `(replyRegistry).register`, `(replyRegistry).route`
- the field-less Reject result leg 2 must exclude: `hsms/connection_runtime.go` → `(*connection).routeReplyOn`
  (the shared body behind `RouteReply` and the generation-bound `RouteReplyFromGeneration`)
