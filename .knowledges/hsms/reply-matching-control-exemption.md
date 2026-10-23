---
type: Mechanic
title: The reply registry's two-legged control exemption
description: Why registration-side isData and result-side *DataMessage assertion are two independent gates, not one, and what breaks when only one survives.
tags: [hsms, reply-correlation, e37, control]
status: stable
generated: {by: "claude/opus-5.5", at: 2026-10-23T18:00:00Z}
verified:
  - {by: "openai/gpt-6-astra", at: 2026-10-23T18:42:42Z}
sources:
  - {resource: hsms/connection_send.go, digest: sha256:ea94ab4fc9d25bb9, revision: 2a92a5e}
  - {resource: hsms/reply_registry.go, digest: sha256:8f6b52cc0e84f7db, revision: 2a92a5e}
  - {resource: hsms/connection_runtime.go, digest: sha256:7f242f475b188a1b, revision: 2a92a5e}
  - {resource: hsms/session.go, digest: sha256:04cc770917f02f59, revision: 2a92a5e}
  - {resource: hsms/reply_registry_test.go, digest: sha256:46ed00a4436d8e37, revision: 2a92a5e}
  - {resource: hsms/reply_matching_test.go, digest: sha256:4fd21a7d86752c61, revision: 2a92a5e}
  - {resource: hsmsss/integration_reply_matching_test.go, digest: sha256:e3ff71163fa0ec13, revision: 2a92a5e}
  - {resource: hsmsss/integration_orphan_control_rsp_test.go, digest: sha256:630da2a164739532, revision: 2a92a5e}
  - {resource: hsmsss/harness_endpoint_test.go, digest: sha256:4db1d08cd272c0fc, revision: 2a92a5e}
  - {resource: hsmsss/transport_recv.go, digest: sha256:f8d35637783f58a6, revision: 2a92a5e}
  - {resource: hsmsss/transport_control.go, digest: sha256:ad8c57da5652a769, revision: 2a92a5e}
  - {resource: hsmsss/transport_active.go, digest: sha256:689f931cb195678f, revision: 2a92a5e}
---

# What it does

`route`'s comment in `hsms/reply_registry.go` states the two-legged shape:
a candidate is compared only when the registration is for a DATA primary (leg 1, `want.isData`)
AND the routed result itself carries a `*DataMessage` (leg 2),
and a DATA registration answered by a `*ControlMessage` is reported as a miss rather than compared.
What it does not state is which test catches which single-leg removal,
or what a mutation that drops both legs does.
That is the delta this entry records:
dropping leg 2 alone is invisible to the two obvious round-trip tests;
dropping leg 1 alone is not,
since leg 1 also gates the DATA-registration `*ControlMessage`-miss check those same tests exercise.
The comment's "Both always deliver uncompared" is imprecise:
both bypass comparison, but only a result that wins [the reply-wait claim](/hsms/reply-wait-claim.md) is delivered.

# How it works

**Registration leg.**
`sendWaitReply` (`connection_send.go`) resolves the current epoch and delegates to `sendWaitReplyOn`,
which computes `dm, isData := msg.(*DataMessage)`.
A data primary registers through `registerData(dm.HeaderBytes())`,
which derives the key, `stream`, and `function` from that one sent header, stores the header, and sets `isData`.
A control primary (`dm == nil`, e.g. Select.req/Deselect.req/Linktest.req) registers through `registerControl(key)`,
whose `*replyWaiter` carries no stream, function, or header, and has `isData` false.

**Result leg.**
`route` (`reply_registry.go`) only enters the comparison branch when **both** hold at once:
`w.isData` is true, AND `dm, ok := res.msg.(*DataMessage)` succeeds against the routed result.
A field-less `*RejectError` result (`res.msg == nil`
— `routeReplyOn`'s `RejectReqType` branch in `connection_runtime.go`, reached via both `RouteReply` and `RouteReplyFromGeneration`, builds exactly this for a peer Reject.req)
bypasses comparison regardless of `isData` —
a peer Reject.req is a terminal rejection of the primary (E37 §8.3.11), not a reply to validate.
A `*ControlMessage` result (Select.rsp/Deselect.rsp/Linktest.rsp) is treated differently depending on registration direction:
a control registration (`isData` false) passes it uncompared to the claim.
A DATA registration (`isData` true) treats it as a miss —
not delivered, the claim state unchanged, and not counted as a mismatch —
because a control response has no Stream()/Function() to validate against a data primary in the first place (E37 §9.4.1),
pinned by `TestReplyRegistry_DataRegistration_ControlResultMustMiss` and `TestReplyMatching_DataPrimary_ControlResponseMustMiss`.

**Which door each leg alone closes** (from `hsmsss/integration_reply_matching_test.go`'s
comment and `hsms/reply_matching_test.go`'s
`TestReplyMatching_ControlRegistration_DataSecondaryDeliveredUncompared`):

- Leg 2 alone (the `*DataMessage` type assertion) does not decide whether a `*ControlMessage` result reaches the claim:
  the gate ahead of it already reports a miss for one on a DATA registration,
  so leg 2 never sees a `*ControlMessage` there,
  and the whole `if w.isData` block is skipped for a CONTROL registration regardless of leg 2 (leg 1 false).
  What leg 2 alone still does is separate a genuine `*DataMessage` secondary (compared against the registered primary)
  from a field-less `*RejectError` result (`res.msg == nil`, passed uncompared) —
  a comparison switch inside the DATA-registration path, not a door of its own.
- Leg 1 alone (`isData`) closes the "reply to a DATA primary whose System Bytes collide with an
  open control transaction" door: a genuine `*DataMessage` secondary passes leg 2 cleanly, so only
  a control registration's `isData == false` keeps it uncompared.
- Classifying every result lacking data fields as mismatched,
  while also bypassing the DATA-registration control-result miss gate,
  rejects control responses in strict mode and counts them as mismatches in observe-only mode.
  Under strict mode the active initiator cannot commit Selected through Select.rsp,
  so its failed Select procedure calls `tcpDown`,
  while the passive responder still commits Selected when it handles Select.req;
  under the default posture `ReplyMismatchCount` climbs on every control round trip.
  Merely deleting the two conditions does not produce this:
  a `*ControlMessage` has no `Stream()`/`Function()` to compare.

The function match itself admits primary+1 **or** 0 — the SxF0 abort secondary (E5 §7.2/§10.4.1).
That exception is function-only: a wrong-stream F0 still mismatches on stream.

# Invariants

- A comparison runs only when both legs hold;
  a RejectError result, or a `*ControlMessage` result answering a CONTROL registration, bypasses comparison, with no mismatch recorded,
  then attempts the claim:
  only an open waiter receives it; after an earlier reply won it is consumed as a duplicate, and after an S9 won it misses.
- Leg 1 also gates `failS9`: an S9 error message never matches a control registration, so it cannot end a Select, Deselect, or Linktest wait.
- The one exception: a `*ControlMessage` result answering a DATA registration is always a miss —
  not delivered, claim state unchanged, no mismatch recorded —
  for a DATA registration (leg 1 true), in either strict mode.
- A strict-mode mismatch leaves the claim state unchanged;
  if the waiter is still open, a later conforming reply can complete it before its T3/T6 timer fires.
- SessionID is deliberately excluded from this registry-level check; `WithSessionIDValidation` is a
  separate, earlier seam (`DeliverOwnedFrame`'s `checkSessionID`).

# Failure modes

- **Removing leg 1 (`isData`) alone** — e.g. replacing `if w.isData` with `if true` — is caught immediately:
  the DATA-registration `*ControlMessage`-miss gate then runs unconditionally,
  so a Select.rsp or Linktest.rsp answering its OWN control registration is intercepted ahead of delivery and reported as a miss instead.
  `TestReplyMatching_StrictMode_SelectReachesSelected` and `TestReplyMatching_DefaultMode_LinktestRoundTripZeroMismatch`
  (`hsmsss/integration_reply_matching_test.go`) both hit their 10-second state waits (`hsmsss/harness_endpoint_test.go`),
  since neither Select nor Linktest ever completes.
- **Removing leg 2 (the `*DataMessage` assertion) alone** only matters for the field-less `*RejectError` result:
  a peer Reject.req answering a data primary (`res.msg == nil`) would then reach the stream/function comparison unguarded instead of passing uncompared,
  caught by `TestReplyRegistry_ControlExemption_ResultLeg`.
  A `*ControlMessage` result does not depend on leg 2 at all —
  the gate ahead of it already reports a miss for one on a DATA registration regardless of leg 2's presence.
- **Mismatch-classifying every field-less result** — see the last bullet under How it works: the active side's failure to reach
  Selected under strict mode, or a `ReplyMismatchCount` that climbs on every successful control
  round trip under the default.
- **Removing the DATA-registration `*ControlMessage` miss gate alone**
  (the check `route` runs ahead of leg 2, before the `*DataMessage` type assertion)
  is undetected by the two control-round-trip integration tests above —
  leg 1 and leg 2 both still hold exactly as described,
  so neither of those two tests catches its removal.
  It bites when a peer answers a data primary's System Bytes with a control response (Select.rsp/Linktest.rsp):
  `session.SendDataMessage` then returns `(nil, nil)`, a false success.
  A genuine reply arriving after the sender's deferred deregistration then becomes unsolicited;
  one arriving earlier can instead hit the still-registered waiter and be consumed or discarded by it.
  Detected instead by `TestReplyRegistry_DataRegistration_ControlResultMustMiss`, `TestReplyMatching_DataPrimary_ControlResponseMustMiss`,
  and `TestReplyMatching_ControlResponseOnDataPrimary_RejectedThenGenuineReplyCompletes`.

# Where to look

- registration site, `isData` computed by type assertion: `hsms/connection_send.go` → `(*connection).sendWaitReplyOn` (called by `(*connection).sendWaitReply`)
- storage and the two-legged gate: `hsms/reply_registry.go` → `replyWaiter`, `(replyRegistry).registerData`, `(replyRegistry).registerControl`, `(replyRegistry).route`
- leg 1 on the S9 path: `hsms/reply_registry.go` → `(replyRegistry).failS9`
- the field-less Reject result leg 2 must exclude: `hsms/connection_runtime.go` → `(*connection).routeReplyOn`
  (the shared body behind `RouteReply` and the generation-bound `RouteReplyFromGeneration`)
- the false success with the gate removed: `hsms/session.go` → `(*session).SendDataMessage`
- unit tests: `hsms/reply_registry_test.go` → `TestReplyRegistry_ControlExemption_ResultLeg`; `hsms/reply_matching_test.go` → `TestReplyMatching_DataPrimary_ControlResponseMustMiss`
- integration tests: `hsmsss/integration_reply_matching_test.go` → `TestReplyMatching_StrictMode_SelectReachesSelected`
- orphan control response test: `hsmsss/integration_orphan_control_rsp_test.go` → `TestReplyMatching_ControlResponseOnDataPrimary_RejectedThenGenuineReplyCompletes`
- the state wait timeout: `hsmsss/harness_endpoint_test.go` → `waitState`
- the response-side commit gate: `hsmsss/transport_recv.go` → `(*transport).dispatchFrame`
- the passive side's commit: `hsmsss/transport_control.go` → `(*transport).handleSelectReq`
- the active side's failure: `hsmsss/transport_active.go` → `(*transport).runSelectProcedure`
