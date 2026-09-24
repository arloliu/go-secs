---
type: Mechanic
title: The reply registry's two-legged control exemption
description: Why registration-side isData and result-side *DataMessage assertion are two independent gates, not one, and what breaks when only one survives.
tags: [hsms, reply-correlation, e37, control]
status: draft
generated: {by: "claude/sonnet-5", at: 2026-09-24T05:09:56Z}
sources:
  - {resource: hsms/connection_send.go, digest: sha256:37c6bd273ed11699, revision: dec5f46}
  - {resource: hsms/reply_registry.go, digest: sha256:6d50e421b3006d65, revision: 922feb8}
  - {resource: hsms/connection_runtime.go, digest: sha256:31ed96e7c5aa678b, revision: 922feb8}
---

# What it does

`docs/specs/secs2-hsms-conformance-audit.md` (D1) and `reply_registry.go`'s own comments already
cover WHY the stream/function/isData check exists and WHAT the default-vs-strict posture does with
a mismatch.
Neither states the internal shape of the control exemption itself:
it is not one condition but two independent legs — a registration-side flag and a result-side type assertion —
and leg 1 closes a door leg 2 cannot reach on its own,
while leg 2 is a comparison switch inside leg 1's path (plus the DATA-registration `*ControlMessage`-miss gate ahead of it).
Dropping leg 2 alone is invisible to the two obvious round-trip tests; dropping leg 1 alone is not,
since leg 1 also gates the DATA-registration `*ControlMessage`-miss check those same tests exercise;
dropping both at once reproduces the exact NotSelected→NotConnect loop shape of the v2.0.1 field bug this mechanism fixed.

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
A field-less `*RejectError` result (`res.msg == nil` — `RouteReply`'s `RejectReqType` branch in `connection_runtime.go` builds exactly this for a peer Reject.req) always delivers uncompared, regardless of `isData` —
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
- Removing **both** — comparing every candidate unconditionally — reproduces the v2.0.1 field-bug
  shape: every Select.rsp becomes an unconditional stream/function mismatch (0 vs. whatever the peer
  actually sent), which stalls Select to T6 under strict mode (`CommitSelected` never runs, so the
  FSM loops NotSelected → NotConnect) or floods `ReplyMismatchCount` under the default posture.

The function match itself admits primary+1 **or** 0 — the SxF0 abort secondary (E5 §7.2/§10.4.1).
That exception is function-only: a wrong-stream F0 still mismatches on stream.

# Invariants

- A comparison runs only when both legs hold;
  a RejectError result, or a `*ControlMessage` result answering a CONTROL registration, always delivers the result uncompared, with no mismatch recorded.
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
- **Removing the DATA-registration `*ControlMessage` miss gate alone** (the check `route` runs ahead of leg 2, before the `*DataMessage` type assertion) is undetected by every test above —
  leg 1 and leg 2 both still hold exactly as described,
  so nothing else in this file catches its removal.
  It only bites when a peer answers a data primary's System Bytes with a control response (Select.rsp/Linktest.rsp):
  `SendDataMessage` then returns `(nil, nil)`, a false success,
  while the real data reply lands later as unsolicited.
  Exercised by `TestReplyRegistry_DataRegistration_ControlResultMustMiss`, `TestReplyMatching_DataPrimary_ControlResponseMustMiss`, and `TestReplyMatching_ControlResponseOnDataPrimary_RejectedThenGenuineReplyCompletes`.

# Where to look

- registration site, `isData` computed by type assertion: `hsms/connection_send.go` → `(*connection).sendWaitReplyOn` (called by `(*connection).sendWaitReply`)
- storage and the two-legged gate: `hsms/reply_registry.go` → `replyWaiter`, `(replyRegistry).register`, `(replyRegistry).route`
- the field-less Reject result leg 2 must exclude: `hsms/connection_runtime.go` → `(*connection).RouteReply`
