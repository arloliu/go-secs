---
type: Mechanic
title: Where the HSMS-SS profile overrides the generic core
description: The four sites whose value or state check comes from E37.1 rather than E37, and what silently breaks if one is "simplified" back.
tags: [hsmsss, e37-1, select, separate, linktest, reject, session-id]
status: stable
generated: {by: "claude/sonnet-5", at: 2026-09-25T02:38:18Z}
verified:
  - {by: "openai/gpt-5.6-terra", at: 2026-09-25T03:13:36Z}
sources:
  - {resource: hsmsss/transport_active.go, digest: sha256:80daec469fc1444e, revision: 6c257b6}
  - {resource: hsms/connection_lifecycle.go, digest: sha256:221b0f7825783fad, revision: 922feb8}
  - {resource: hsms/control_msg.go, digest: sha256:847dad3406c4d87c, revision: 3660aa4}
  - {resource: hsmsss/transport_control.go, digest: sha256:84353e5b3b34860b, revision: 6c257b6}
  - {resource: hsmsss/transport_recv.go, digest: sha256:f54ea89029ef179c, revision: 6c257b6}
  - {resource: hsms/supervisor.go, digest: sha256:291a3c8397ed511d, revision: a7ff4a8}
---

# What it does

Four places in this transport take a value or a state check from the HSMS-SS profile instead of from the generic HSMS rules the surrounding code follows.
Each one looks arbitrary in isolation, and each has already been written the generic way once — the v2.0.1 Select.req regression was exactly that.

`docs/specs/e37-1-hsms-ss-conformance-audit.md` holds the clause-by-clause reasoning and the deviations we chose to keep.
This entry records only where the four live and how they fail, so the next reader recognizes them as load-bearing rather than incidental.

# How it works

**Five construction sites explicitly pass the profile constant instead of the configured device ID.**
`hsms.ControlSessionID` is the profile constant;
`Connection.SessionID()` is the device ID and belongs only in data messages.
`runSelectProcedure` (`transport_active.go`), the farewell `writeFarewellSeparate` (`hsms/connection_lifecycle.go`),
and the three Reject senders in `transport_control.go` all pass `ControlSessionID` explicitly.
`NewLinktestReq` hard-codes it internally.

The generic constructors — `hsms.NewSelectReq`, `NewSeparateReq`, `NewRejectReqRaw` — deliberately keep taking an arbitrary session ID.
They implement generic E37, so the profile value is applied at the HSMS-SS call sites,
not baked into the shared message layer.

`Select.rsp` and `Deselect.rsp` are NOT call sites and do not enforce the profile value:
`NewSelectRsp`/`NewDeselectRsp` (`hsms/control_msg.go`) copy the request's own session-ID header bytes verbatim, whatever they were.
"Control frames never carry the configured session ID" is therefore true only at these five sending sites,
not as a wire-level guarantee —
a peer (or a construction bug) that puts the configured device ID on a Select.req or Deselect.req
gets it echoed straight back on the matching response, including a nonconformant value equal to the configured ID.

**`writeFarewellSeparate` lives in the shared core, which `secs1` also reaches.** `secs1.New` wraps `hsms.NewConnection` and its transport commits the shared FSM to Selected, so a graceful SECS-I Close lands there too.
No HSMS frame reaches a SECS-I peer only because `secs1`'s writer drops every non-zero SType before the wire — the constant is inert there, not correct there.

**`handleSeparateReq` tears down in any connected substate, and two guards now keep that teardown inside the reporting generation.**
It reports through `t.tcpDown(g.gen, errPeerSeparate, hsms.CausePeerSeparate)`, which resolves the target generation by identity (`connection.TCPDownFromGeneration`) rather than whichever epoch happens to be current when the report lands.
The `g.ctx.Err()` check ahead of it is only an early exit — cancellation can land between the check and the call — but `g.gen` is a real barrier: it travels with the queued event onto the FSM and is re-checked when `supervisor.step` applies it, so a stale report can no longer disconnect a successor generation even when the early exit misses it.
This closes what the conformance audit recorded as Gap 2 (a narrowed-but-open window, not a fence); see [transition-cause-injection-sites](/hsms/transition-cause-injection-sites.md) for the full generation-identity mechanism this now rests on.
Since the R10 inbound-generation-fence work, `genCtx` is no longer a separate parameter threaded alongside `g`:
`startActive`/`startPassive` stamp `g.ctx` once, before spawning the receive loop, and `recvLoop` only reads it;
`dispatchFrame` forwards that stored `g.ctx` straight to `handleSeparateReq` —
one bundle carries both the early-exit ctx and the `g.gen` barrier, not two separately-threaded values.

**`handleLinktestReq` answers regardless of state, on purpose.** The initiator side is bracketed by `startLinktest` / `stopLinktest`; only the responder is lenient.
See its comment and the audit's Gap 3.

# Failure modes

A wrong control-frame session ID slips past any test that only configures the default device ID, `0xFFFF`,
because a control frame and a data frame then carry the same value,
and nothing distinguishes echoing the profile constant from emitting the configured ID.
For the Select/Separate call sites (`runSelectProcedure`, `writeFarewellSeparate`),
this only shows up against equipment that enforces the rule,
and only when a nondefault device ID was configured —
which is exactly the condition the regression tests below now cover on purpose.
The three Reject senders are different: they never read the configured device ID at all,
so reverting them to echo the OFFENDING frame's own SessionID can fail even with the default local ID configured,
since that SessionID comes from the peer's frame, not from local configuration.
The symptom also depends on the procedure:
a rejected Select handshake produces an endless `NotSelected` → `NotConnect` reconnect loop with no error naming the session ID;
a wrong SessionID on a Reject or the farewell Separate does not feed back into the local FSM the same way.

That is why the guard tests carry counter-assertions rather than single-sided ones: `TestActive_SelectAndSeparateUseControlSessionID` asserts `0xFFFF` on control frames **and** the configured device ID on a data message, so an over-fix that forces `0xFFFF` everywhere fails too.
`TestPassive_SelectRspMirrorsRequestSessionID` probes with a non-conformant `0x0042` because a conformant `0xFFFF` request cannot distinguish echoing from emitting a constant.

Dropping the `genCtx` early exit alone still leaves successor protection through the `g.gen` barrier,
but the visible effect is not always just a counted stale report.
`injectDisconnect` (`hsms/connection_lifecycle.go`) counts an identity mismatch (`e.id != gen`) as `staleGen` and drops it,
but it never checks whether the CURRENT epoch itself has already started ending.
If that epoch is still `c.cur` when the report lands,
`injectDisconnect` sets `commsFailure` and injects the event anyway — a redundant disconnect report rather than a dropped one.
And if the event reaches a supervisor that has already latched closed,
`supervisor.step` (`hsms/supervisor.go`) returns immediately on `s.closed` without incrementing `staleGen` at all — a silent, uncounted no-op.
Dropping the `g.gen` barrier itself — reporting through the plain, ungapped path instead of `TCPDownFromGeneration` — reopens the original hazard: a Separate read by a recv goroutine a bounded `Stop` has abandoned disconnects the *successor* generation, presenting as a spurious reconnect with no peer-side cause.

# Where to look

- profile constant: `hsms/control_msg.go` → `ControlSessionID`
- Select initiator: `hsmsss/transport_active.go` → `runSelectProcedure`
- farewell Separate: `hsms/connection_lifecycle.go` → `writeFarewellSeparate`
- Reject senders: `hsmsss/transport_control.go` → `sendReject`, `sendRejectNotSelected`, `sendRejectTransactionNotOpen`
- Separate / Linktest responders: `hsmsss/transport_control.go` → `handleSeparateReq`, `handleLinktestReq`
- genCtx + generation-identity threading: `hsmsss/transport_recv.go` → `recvLoop`, `dispatchFrame`
- the generation barrier the report resolves against: `hsms/connection_lifecycle.go` → `TCPDownFromGeneration`, `injectDisconnect`
- the closed-supervisor no-op: `hsms/supervisor.go` → `(*supervisor).step`
