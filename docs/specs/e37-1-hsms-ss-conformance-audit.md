# SEMI E37.1 (HSMS-SS) Conformance Audit

**Date:** 2026-08-10

**Scope:** Every normative clause of SEMI E37.1-0702 (HSMS-SS),
checked against `hsmsss` and the shared `hsms` engine at the current branch head.

**Sources:** `~/semi_standards/markdowns/e037-01-0702/e037-01-0702.md` (E37.1-0702)
and `~/semi_standards/markdowns/e037-00-0413/e037-00-0413.md` (E37-0413, generic services).

**Method:** clause-by-clause read of E37.1,
each requirement traced to the implementing symbol,
with E37 generic consulted wherever the two differ.

**Trigger:** a field report that v2.0.1's active-role `Select.req` carried the configured session ID instead of `0xFFFF`,
so equipment configured with a device ID other than `0xFFFF` rejected the Select and closed the socket —
an endless `NotSelected` → `NotConnected` reconnect loop.

## Root cause, and why it generalizes

The bug was not a typo.
The code comment at `hsmsss/transport_active.go` argued the wrong value from E37 **generic** §8.2.6.1 and §8.3.7.1 — correctly, as far as it went.
What it missed is that E37 generic §8.3.4.3 explicitly *defers* Select.req's session ID to the subsidiary standard,
and the subsidiary standard (E37.1 §7.1.1, §7.6, §8.1) fixes it at `0xFFFF`.

(Beware §8.3.3.1, which reads identically but governs the DATA message; an earlier draft of this audit cited it by mistake.)

**E37.1 narrows E37 generic in four places, and the implementation followed the generic rule in all four.**
One was a live bug, two were latent gaps now closed, one is a deliberate deviation kept.
This is a single root cause, not four unrelated nits:

| # | Behavior | E37 generic says | E37.1 says | Status |
|---|---|---|---|---|
| 1 | `Select.req` / `Separate.req` session ID | §8.3.4.3 — deferred to the subsidiary standard | §7.1.1, §7.6, §8.1 — always `0xFFFF` | **Live bug — FIXED** |
| 1b | `Reject.req` session ID | §8.3.21.1 — echo the rejected message's | §8.1 — always `0xFFFF`, no carve-out | **Latent gap — FIXED (v1 review)** |
| 2 | `Separate.req` received while NotSelected | §7.9.2.3 — "the Separate.req is ignored" | §7.6 — close immediately on *receiving*, no SELECTED qualifier | **Latent gap — FIXED** |
| 3 | `Linktest` responder state | §7.8 — "valid anytime in the CONNECTED state" | §7.4 — "strictly limited to the SELECTED state" | **Deliberate deviation — kept, documented + tested** |

The takeaway for future work:
**when implementing an HSMS-SS procedure, read E37.1 before E37 generic.**
E37.1 §5.1 states its whole purpose is "to explicitly limit the capabilities of the HSMS Generic Services";
every clause in it is a narrowing, so the generic standard alone is never sufficient.

## Gap 1 — control-message session ID (FIXED)

E37.1 §8.1:
"In HSMS-SS Control Messages, Session ID will always assume the special value 0xFFFF (all one bits)."
§7.1.1 repeats it for the Select Procedure;
§7.6: "Separate shall always use SessionID 0xFFFF (binary, all ones)."

E37.1 §8.1 also settles what the configured value *is*:
in **data** messages the high-order bit is zero and the low-order 15 bits are the Device ID.
`hsms.WithSessionID` configures the device ID; it has no bearing on control frames.

Five outbound sites were wrong.
All are fixed:

| Site | Was | Now |
|---|---|---|
| `hsmsss/transport_active.go` → `runSelectProcedure` | `hsms.NewSelectReq(t.rt.SessionID(), sb)` | `hsms.NewSelectReq(hsms.ControlSessionID, sb)` |
| `hsms/connection_lifecycle.go` → `writeFarewellSeparate` | `NewSeparateReq(cfg.sessionID, …)` | `NewSeparateReq(ControlSessionID, …)` |
| `hsmsss/transport_control.go` → `sendReject` | `NewRejectReqRaw(frame's sessionID, …)` | `NewRejectReqRaw(hsms.ControlSessionID, …)` |
| `hsmsss/transport_control.go` → `sendRejectNotSelected` | same | same |
| `hsmsss/transport_control.go` → `sendRejectTransactionNotOpen` | same | same |

The three Reject senders were missed on the first pass, and missed for exactly the reason this audit exists:
the first draft listed them as "already correct" on the strength of E37 generic §8.3.21.1.
That clause reads "Equal to the value of the Session ID in the message being rejected" —
reasoning from the generic standard, again, in the very document written to stop that.
The v1 post-implementation review caught it.

The counter-argument is real and was weighed:
E37's §8.3 summary table marks with `*` those fields "subject to further specification by subsidiary standards",
and `Reject.req`'s session ID is **not** marked, while `Deselect.req` and `Separate.req` are.
That is genuine evidence E37 did not anticipate a subsidiary override here.
E37.1 §8.1 nonetheless admits no exception, and the subsidiary standard governs the profile:
"In HSMS-SS Control Messages, Session ID will always assume the special value 0xFFFF".
Nothing is lost: correlation rides on System Bytes (§8.3.21.4), which are still echoed verbatim,
and header byte 2 still carries the offending PType/SType (§8.3.21.2).

The remaining control frames were already correct, for a different reason at each site:

- `NewLinktestReq` hard-codes `0xFFFF` internally.
  E37 §8.3.14–19 fixes it in the generic standard too, so this one never depended on reading E37.1.
- `Select.rsp` / `Deselect.rsp` echo the request's session ID (E37 §8.3.7.1, §8.3.13.1) —
  which, from a compliant HSMS-SS peer, is `0xFFFF`.
  These are responses whose session ID the standard binds to the request, not free choices.

The **receive** path was verified clean and needed no change:
control-message replies route purely by System Bytes (`hsms.replyRegistry`),
and `WithSessionIDValidation` is gated on `*DataMessage` only (`hsms/connection_runtime.go`).
Had either compared a control frame's session ID against the configured one,
fixing the send side alone would have reproduced the same reconnect loop from the other direction.

Regression guard: `hsmsss.TestActive_SelectAndSeparateUseControlSessionID` configures device ID 1000
and asserts `Select.req` and the farewell `Separate.req` carry `0xFFFF`
**while a data message on the same connection carries 1000** —
the counter-assertion that catches an over-fix that forces `0xFFFF` everywhere.
Teeth-checked: each half of the fix was reverted independently and the matching assertion failed.

`TestReject_UsesControlSessionID` covers the Reject sites:
a data frame carrying device ID 1234 arrives while NotSelected, and the resulting `Reject.req` must carry
`0xFFFF` while still echoing the offending System Bytes.

`TestPassive_SelectRspMirrorsRequestSessionID` pins the responder's echo behavior.
It probes with a deliberately non-conformant request session ID (`0x0042`).
Against a conformant `0xFFFF` request, a responder that ignored the request entirely would pass too;
the v1 review flagged the original form as degenerate for exactly that reason.

Exported `hsms.ControlSessionID` so the value is named rather than repeated as a literal.

## Gap 2 — `Separate.req` received while NotSelected (FIXED)

**Clause.**
E37.1 §7.6:
"In HSMS-SS, the Separate.req is valid only in the TCP/IP CONNECTED state and its substates.
After either initiating or receiving a Separate.req message,
the entity shall immediately close the TCP/IP connection and transit to the TCP/IP NOT CONNECTED state."

NOT SELECTED *is* a substate of TCP/IP CONNECTED (E37 §5),
and the clause attaches no state qualifier to "receiving".
So under E37.1, a `Separate.req` arriving while NotSelected must close the connection.

E37 generic is internally inconsistent here, which is worth recording:
its §7.9 prose says the responding entity "is required to terminate communications **regardless of its local state**",
while its own §7.9.2.3 says to ignore a Separate received outside SELECTED.
E37.1 §7.6 resolves the contradiction in favor of terminating,
so the fix below is what *both* standards' stronger statement asks for.

**Was.**
`hsmsss/transport_control.go` → `handleSeparateReq` tore down only when `State() == SelectedState`;
otherwise it returned `true` and the recv loop kept reading.
That followed E37 generic §7.9.2.3 verbatim:
"If the responding entity is not in the SELECTED state, the Separate.req is ignored."

**Impact was small but real.**
The peer that sends a Separate normally closes its socket immediately after,
so our recv loop saw EOF and tore down anyway.
The observable cost was a peer that sent Separate and *held* the socket open:
teardown then waited for the T7 NotSelected dwell instead of happening at once.

**Now.**
`handleSeparateReq` tears down in any connected substate.
It is reachable only from the recv loop of a live generation,
so the transport's socket is always up when it runs
and §7.6's "TCP/IP CONNECTED state and its substates" precondition needs no FSM check.
`SeparateRecvCount` now counts a Separate received in any substate, and its godoc says so.

What it *does* need — and the v1 review was right that the first cut lacked it — is the **C1 straggler guard**.
`connection.TCPDown` resolves the CURRENT epoch and supervisor at call time and injects an untagged `evDisconnect`.
So a Separate read by a generation whose teardown already began could land on a SUCCESSOR generation
and knock it out of NotSelected.
`recvLoop` already applied that guard to read errors; dispatch did not.
The generation ctx is now threaded through `dispatchFrame` into `handleSeparateReq`, which returns early
when it is cancelled — teardown owns the disconnect in that case.
Widening the teardown to every substate is what made this worth closing:
before, only a Selected-state Separate reached `TCPDown` at all.

`TestSeparate_CancelledGenerationSkipsTCPDown` pins it, teeth-checked by removing the ctx check.

Be precise about what the guard buys.
An earlier revision of this section over-claimed it, and the v2 review was right to call that out.
The check NARROWS the window; it is not an epoch barrier.
Cancellation can still land between the check and the `TCPDown` call, on this sequence:
the handler observes an uncancelled `genCtx` and is descheduled;
a concurrent T7 expiry or Select failure tears down generation N;
the bounded join abandons this recv goroutine;
reconnect publishes N+1;
the handler then resumes and injects `evDisconnect` into N+1.
The test proves the early-return branch, not that sequence.

Closing it properly requires the queued FSM event to carry epoch identity and be revalidated at PROCESSING time,
the pattern `supervisor.requestClose` already uses via `closeEpoch`.
That is a core change touching all seven `TCPDown` / `T7Expired` producers, and it belongs on its own branch:
the identical check-then-call shape has been `recvLoop`'s read-error guard since long before this phase,
where it is far more reachable than a NotSelected Separate ever is.

The broader hazard is pre-existing architecture with six other call sites, and is NOT addressed here:
`evDisconnect` and `evT7Timeout` carry no epoch identity at all,
so *any* delayed producer could in principle cross a generation boundary.
It deserves its own change.

Two tests were rewritten from asserting the old behavior to asserting the new one,
both teeth-checked by restoring the `State() == SelectedState` guard:
`TestSeparate_TearsDownWhileNotSelected` (unit, was `TestSeparate_IgnoredWhileNotSelected`)
and `TestReader_PeerSeparate/while_not_selected_disconnects_no_farewell` (recv loop, was `…/while_not_selected_ignored`).
The recv-loop case additionally asserts we send nothing back —
no farewell Separate and no Reject — since the peer announced its exit.

## Gap 3 — Linktest responder outside SELECTED (deliberate deviation, kept)

**Clause.**
E37.1 §7.4:
"As defined by HSMS. Under HSMS-SS, the use of Linktest is strictly limited to the SELECTED state."
Table 3 lists Link Test as allowed in "HSMS Selected" and scopes the whole *transaction*, not merely the request,
so the restriction is not cleanly initiator-only.
§7.7 makes any violation of a §7 restriction a communications failure.

**Classification vs. remedy.**
§7.7 governs how an out-of-state probe is *classified*; it does not by itself prescribe the consequence.
E37 §9.1.1 supplies the remedy, and it is permissive:
"If a communications failure is detected, the entity **should** terminate the TCP/IP connection" — should, not shall.
E37 §7.8.2's responder procedure is meanwhile unconditional:
"The responding entity receives the Linktest.req from the initiator.
The responding entity sends a Linktest.rsp."
So the *remedy* is discretionary: disconnect-on-probe is recommended, not mandated.

That is as far as the argument goes.
The v1 review was right to push back on an earlier draft that stretched it into "not a violation".
It is one.
§7.4 limits the *use* of the Linktest procedure to SELECTED,
and sending a Linktest.rsp outside SELECTED is use of that procedure outside SELECTED.
What §9.1.1 buys is freedom in how we respond to the peer's violation — not permission for our own.
This is a deliberate profile deviation, knowingly taken for interoperability, and it should be read as one.

**Current behavior.**
Our *initiator* side is unambiguously compliant:
the auto-linktest goroutine is started by `startLinktest` on the commit to Selected
and stopped by `stopLinktest` on `SelectLost`, so we never probe outside Selected.
Only the *responder* is lenient —
`handleLinktestReq` answers a `Linktest.req` with a `Linktest.rsp` regardless of state,
which is E37 generic §7.8's "valid anytime in the CONNECTED state."

**Decision: keep the lenient responder, documented and tested.**
Unlike Gap 2, strict conformance here *adds* a way to drop a live TCP connection:
a peer that probes before completing Select —
permitted by the generic standard many stacks implement against —
would trigger a communications failure and a reconnect.
Note the peer is not HSMS-SS-conformant in that exchange;
the argument is that we should not punish it, not that it is right.

The lenient response has no FSM effect and does not extend T7:
nothing in `handleLinktestReq` transitions state, and the dwell keeps running,
so a peer cannot hold an unselected session open by probing — it must still complete Select before T7 expires.
It does consume receive, enqueue, and write capacity, so it is not literally free.

The asymmetry with Gap 2 is the point:
Gap 2's strict behavior removes a delay, Gap 3's strict behavior introduces a disconnect.

**Rejected alternatives.**
A `WithStrictLinktestState`-style opt-in was considered and rejected:
it enlarges an already broad public surface for a behavior almost no caller wants,
and it raises the question of which *other* profile deviations such an option should govern.
A dedicated out-of-state counter or an unconditional warning log was likewise deferred —
`LinktestReqRecvCount` and `WithTraceTraffic` already give partial visibility,
and a warning on the recv goroutine would be noisy against a periodically-probing peer.
If field evidence later shows the distinction matters operationally,
the next step is a narrow `LinktestReqOutOfStateCount` that increments while the responder still answers —
not a disconnect option.

**Guards.**
`handleLinktestReq` carries a comment recording the trade and naming the clauses, so it is not "fixed" into a disconnect later.
`TestLinktest_InboundReqAnsweredWhileNotSelected` pins the three properties the deviation rests on:
the rsp is sent, `TCPDown` is not called, and the probe is still counted.
`LinktestReqRecvCount`'s godoc records that it counts any TCP-connected substate.

## Deviations reviewed and accepted (no action)

**§7.1.1 — non-zero select-status must close the connection.**
"Immediately following any Select Procedure which fails to complete successfully with a zero Select Status,
each Entity must close the TCP/IP connection and transit to the NOT CONNECTED state."

We treat select-status **1** (`SelectStatusAlreadyActive`, E37 Table 7) as success on both sides:
`runSelectProcedure` does not tear down on it,
and `handleSelectReq` answers 1 to a duplicate `Select.req` while staying Selected.
This is the simultaneous-select design (internal M5 / H2):
in that race our own Select.req is answered 1 by a peer we are *already* validly Selected with,
and E37 §7.4.1.3 gives a non-zero status no state transition
(§9.4 is SECS-II considerations — an earlier draft cited it in error).
Tearing down would drop a good link.
Statuses 2, 3, and 4+ are treated as genuine failures and do drive the close.
We never *emit* 2 or 3, so the deviation is confined to status 1.
Deliberate, tested, keep.

**§4.1.1 / §8.1 — Device ID is 15 bits.**
"device ID — a 15-bit field";
in data messages "the high-order bit of Session ID is zero."
`hsms.WithSessionID` accepts any `uint16` and documents that, and the default is `0xFFFF` —
which has the high bit set and is therefore not a valid *device ID*.
Flagged, not changed:
the default matches v1,
and validating the range or changing the default would break existing configurations for a value that only matters if equipment enforces the bit.
Documented in the `WithSessionID` godoc instead.

**§9.1 — single-block messages should not exceed 254 bytes.**
A "should", and an application-layer concern about SECS-I compatibility, not a transport requirement.
Not enforced; correct to leave to the caller.

## Clauses verified conformant

| Clause | Requirement | Where |
|---|---|---|
| §5.1.1 | Deselect not used to end communications; Separate instead | No outbound `NewDeselectReq` anywhere; teardown uses `writeFarewellSeparate` |
| §5.1.1 | Reject procedure optional | Implemented — `sendReject`, `sendRejectNotSelected`, `sendRejectTransactionNotOpen` |
| §5.3.1 | SelectionCounter not required | Not implemented; single-session model |
| §7.1 | Select initiated only by the active entity; passive must not initiate | `runSelectProcedure` is spawned only from `startActive`; no Select initiation in `transport_passive.go` |
| §7.2 | Data valid for any SessionID matching a supported Device ID | Single-session equality check, `hsms/connection_runtime.go` (opt-in via `WithSessionIDValidation`) |
| §7.3 | Deselect shall not be used | Responder-only (`handleDeselectReq`), never initiated — answering rather than failing is deliberate leniency so a peer is not stranded on T6 |
| §7.5 | Reject optional; unsupported situations are communications failures | Reject implemented, so the fallback does not apply |
| §7.6 | Separate.req always `0xFFFF` | **Fixed** — see Gap 1 |
| §7.6 | Close immediately after *initiating* a Separate | `writeFarewellSeparate` runs inside the teardown path; the socket closes unconditionally after |
| §8.1 | Control messages always `0xFFFF` | **Fixed** — see Gap 1 |
| §8.2 | All HSMS-SS messages are PType 0 | Non-zero PType → `Reject(PTypeNotSupported)`, `dispatchFrame` |
| §8.3 | Only HSMS-defined STypes; user-defined not permitted | `IsValidSType` → `Reject(STypeNotSupported)`, `dispatchFrame` |
| §10.1 | Documentation requirements (device IDs, teardown mode, host/equipment) | **Now** covered by the "E37.1 §10.1 implementation documentation" section of `hsmsss/doc.go`. The v1 review correctly found the earlier claim false: `doc.go` documented construction and lifecycle, and the README gives examples, but none of the three required declarations were stated anywhere |

## Actions

- [x] Gap 1 — `Select.req` and `Separate.req` carry `hsms.ControlSessionID`;
      regression test with counter-assertion;
      misleading `SessionID()` / `WithSessionID` doc comments corrected.
- [x] Gap 2 — `handleSeparateReq` tears down in any connected substate;
      two tests rewritten and teeth-checked;
      `SeparateRecvCount` godoc updated.
- [x] Gap 1b — the three `Reject.req` senders carry `hsms.ControlSessionID`;
      `TestReject_UsesControlSessionID` pins it.
- [x] Gap 2 follow-up — C1 straggler guard threaded into the Separate dispatch path;
      `TestSeparate_CancelledGenerationSkipsTCPDown` pins the already-cancelled branch;
      the residual check/use race is deferred, see Gap 2.
- [x] §10.1 — the three required declarations are stated in `hsmsss/doc.go`.
- [x] Gap 3 — behavior deliberately unchanged.
      `handleLinktestReq` carries the rationale and the clause citations;
      `TestLinktest_InboundReqAnsweredWhileNotSelected` pins the deviation;
      `LinktestReqRecvCount` godoc records that it counts any substate.

## Review

The Gap 3 decision was checked by an external reviewer (Codex, `gpt-5.6-sol`).
It concurred with keeping the lenient responder and supplied three corrections now folded in:
E37 §9.1.1's remedy is "should terminate", not "shall", so §7.7 classifies without prescribing the consequence;
"healthy link" overstated the case, since the probing peer is not itself conformant;
and "costs nothing" should read as "no FSM effect and does not extend T7".
It also flagged that the deviation had no test — `TestLinktest_InboundReqAnsweredWhileNotSelected` closes that.

A subsequent **post-implementation review** returned *fix-then-merge* and drove the rest of the corrections recorded above
(Codex, `gpt-5.6-sol`, `xhigh`; report at `tmp/e37-1-conformance_post_implementation_review_v1.md`).
Those were: the `Reject.req` session ID (Gap 1b), the missing C1 straggler guard, the absent §10.1 documentation,
the over-claimed Gap 3 wording, the `§8.3.3.1` → `§8.3.4.3` and `§9.4` → `§7.4.1.3` citations,
the degenerate passive mirror test, and a farewell-assertion race in the active test.
That last one: an async data send establishes no happens-before with the `TryLock` farewell,
so the test could legitimately observe no Separate at all.

It also disproved an assumption stated in an earlier draft of this audit:
that `secs1` cannot reach `hsms.writeFarewellSeparate`.
It can.
`secs1.New` wraps `hsms.NewConnection`, the SECS-I transport commits the shared FSM to Selected,
and a graceful Close from Selected lands there.
No HSMS frame reaches a SECS-I peer only because `secs1`'s writer drops every non-zero SType before the wire,
so the hard-coded profile constant is inert there rather than correct there.
The comment asserting "hsmsss is the only transport over this core" has been corrected.
