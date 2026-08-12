---
type: Mechanic
title: The reply registry's two-legged control exemption
description: Why registration-side isData and result-side *DataMessage assertion are two independent gates, not one, and what breaks when only one survives.
tags: [hsms, reply-correlation, e37, control]
status: draft
generated: {by: "claude/sonnet-5", at: 2026-08-12T00:00:00Z}
sources:
  - {resource: hsms/connection_send.go, digest: sha256:7e589dff2dee86f0, revision: 3660aa4}
  - {resource: hsms/reply_registry.go, digest: sha256:2e471a8c9b60839e, revision: 3660aa4}
  - {resource: hsms/connection_runtime.go, digest: sha256:a18a4022d747b1a9, revision: 3660aa4}
---

# What it does

`docs/specs/secs2-hsms-conformance-audit.md` (D1) and `reply_registry.go`'s own comments already
cover WHY the stream/function/isData check exists and WHAT the default-vs-strict posture does with
a mismatch.
Neither states the internal shape of the control exemption itself: it is not one condition but two
independent legs — a registration-side flag and a result-side type assertion — each closing a
different door the other leg cannot reach on its own.
Dropping either leg alone is invisible to the two obvious round-trip tests; dropping both at once
reproduces the exact NotSelected→NotConnect loop shape of the v2.0.1 field bug this mechanism fixed.

# How it works

**Registration leg.**
`sendWaitReply` (`connection_send.go`) computes `dm, isData := msg.(*DataMessage)`.
For a control primary (`dm == nil`, e.g. Select.req/Deselect.req/Linktest.req) the `stream`/`function`
values passed to `register` are left at their zero defaults — they are stored but never read, because
`isData` is false.
`register` stores `{ch, stream, function, isData}` in a `replyWaiter`.

**Result leg.**
`route` (`reply_registry.go`) only enters the comparison branch when **both** hold at once:
`w.isData` is true, AND `dm, ok := res.msg.(*DataMessage)` succeeds against the routed result.
A field-less `*RejectError` result (`res.msg == nil` — `RouteReply`'s `RejectReqType` branch in
`connection_runtime.go` builds exactly this for a peer Reject.req) and a `*ControlMessage` result
(Select.rsp/Deselect.rsp/Linktest.rsp) both fail this assertion regardless of `isData`.

**Which door each leg alone closes** (from `hsmsss/integration_reply_matching_test.go`'s teeth
comment and `hsms/reply_matching_test.go`'s
`TestReplyMatching_ControlRegistration_DataSecondaryDeliveredUncompared`):

- Leg 2 alone (the type assertion) closes the "reply to a CONTROL primary" door: Select.rsp and
  Linktest.rsp always decode as `*ControlMessage`, never `*DataMessage`, so they fail leg 2 no
  matter what `isData` says.
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

- A comparison runs only when both legs hold; either leg failing delivers the result uncompared, with no mismatch recorded.
- A strict-mode miss does not consume the registration — a later conforming reply can still complete
  the transaction before its T3/T6 timer fires.
- SessionID is deliberately excluded from this registry-level check; `WithSessionIDValidation` is a
  separate, earlier seam (`DeliverOwnedFrame`'s `checkSessionID`).

# Failure modes

- **Removing leg 1 (`isData`) alone** is undetected by
  `TestReplyMatching_StrictMode_SelectReachesSelected` and
  `TestReplyMatching_DefaultMode_LinktestRoundTripZeroMismatch` — leg 2 alone still excludes every
  control-typed result.
  It only bites through a data/control System-Bytes collision, exercised by
  `TestReplyMatching_ControlRegistration_DataSecondaryDeliveredUncompared` and
  `TestReplyRegistry_ControlExemption_RegistrationLeg`.
- **Removing leg 2 (the `*DataMessage` assertion) alone** is undetected by those same two round-trip
  tests too — leg 1 alone still keeps every control registration from ever reaching the
  type-assertion code.
- **Removing both** — see the v2.0.1 shape under How it works: an end-to-end failure to reach
  Selected under strict mode, or a `ReplyMismatchCount` that climbs on every successful control
  round trip under the default.

# Where to look

- registration site, `isData` computed by type assertion: `hsms/connection_send.go` → `(*connection).sendWaitReply`
- storage and the two-legged gate: `hsms/reply_registry.go` → `replyWaiter`, `(replyRegistry).register`, `(replyRegistry).route`
- the field-less Reject result leg 2 must exclude: `hsms/connection_runtime.go` → `(*connection).RouteReply`
