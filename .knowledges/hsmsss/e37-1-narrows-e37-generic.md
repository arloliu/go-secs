---
type: Mechanic
title: Where the HSMS-SS profile overrides the generic core
description: The four sites whose value or state check comes from E37.1 rather than E37, and what silently breaks if one is "simplified" back.
tags: [hsmsss, e37-1, select, separate, linktest, reject, session-id]
status: stable
generated: {by: "claude/opus-5", at: 2026-08-10T00:00:00Z}
verified:
  - {by: "claude/opus-5", at: 2026-08-12T08:33:19Z}
sources:
  - {resource: hsmsss/transport_active.go, digest: sha256:642cebd8a000d63d, revision: 3660aa4}
  - {resource: hsms/connection_lifecycle.go, digest: sha256:c3f5abfffb491404, revision: 3660aa4}
  - {resource: hsms/control_msg.go, digest: sha256:847dad3406c4d87c, revision: 3660aa4}
  - {resource: hsmsss/transport_control.go, digest: sha256:d0b89bf94d8c792b, revision: 3660aa4}
  - {resource: hsmsss/transport_recv.go, digest: sha256:67343e11cdcaea52, revision: 3660aa4}
---

# What it does

Four places in this transport take a value or a state check from the HSMS-SS profile instead of from the generic HSMS rules the surrounding code follows.
Each one looks arbitrary in isolation, and each has already been written the generic way once — the v2.0.1 Select.req regression was exactly that.

`docs/specs/e37-1-hsms-ss-conformance-audit.md` holds the clause-by-clause reasoning and the deviations we chose to keep.
This entry records only where the four live and how they fail, so the next reader recognizes them as load-bearing rather than incidental.

# How it works

**Control frames never carry the configured session ID.** `hsms.ControlSessionID` is the profile constant; `Connection.SessionID()` is the device ID and belongs only in data messages.
Five construction sites pass the constant: `runSelectProcedure` (`transport_active.go`), `writeFarewellSeparate` (`hsms/connection_lifecycle.go`), and the three Reject senders in `transport_control.go`.
`NewLinktestReq` hard-codes it internally.

The generic constructors — `hsms.NewSelectReq`, `NewSeparateReq`, `NewRejectReqRaw` — deliberately keep taking an arbitrary session ID.
They implement generic E37, so the profile value is applied at the HSMS-SS call sites, not baked into the shared message layer.
`Select.rsp` / `Deselect.rsp` are responses whose session ID the standard binds to the request; they echo and are not call sites.

**`writeFarewellSeparate` lives in the shared core, which `secs1` also reaches.** `secs1.New` wraps `hsms.NewConnection` and its transport commits the shared FSM to Selected, so a graceful SECS-I Close lands there too.
No HSMS frame reaches a SECS-I peer only because `secs1`'s writer drops every non-zero SType before the wire — the constant is inert there, not correct there.

**`handleSeparateReq` tears down in any connected substate, and needs the C1 straggler guard to do it.** `connection.TCPDown` resolves the *current* epoch and supervisor at call time and injects an untagged `evDisconnect`.
`recvLoop` guards its read-error path against that with a `genCtx.Err()` check; the dispatch path had no such guard, which only became reachable once teardown stopped being restricted to Selected.
`genCtx` is threaded `recvLoop` → `dispatchFrame` → `handleSeparateReq` for that check alone.

**`handleLinktestReq` answers regardless of state, on purpose.** The initiator side is bracketed by `startLinktest` / `stopLinktest`; only the responder is lenient.
See its comment and the audit's Gap 3.

# Failure modes

A wrong control-frame session ID passes every loopback test and the whole existing suite, because the default device ID *is* `0xFFFF` — it only breaks against equipment that enforces the rule, and only when a device ID was configured.
The symptom is an endless `NotSelected` → `NotConnect` reconnect loop with no error naming the session ID.

That is why the guard tests carry counter-assertions rather than single-sided ones: `TestActive_SelectAndSeparateUseControlSessionID` asserts `0xFFFF` on control frames **and** the configured device ID on a data message, so an over-fix that forces `0xFFFF` everywhere fails too.
`TestPassive_SelectRspMirrorsRequestSessionID` probes with a non-conformant `0x0042` because a conformant `0xFFFF` request cannot distinguish echoing from emitting a constant.

Dropping the straggler guard fails differently and far more rarely: a Separate arriving while a bounded `Stop` has abandoned the recv goroutine disconnects the *successor* generation, which presents as a spurious reconnect with no peer-side cause.

# Where to look

- profile constant: `hsms/control_msg.go` → `ControlSessionID`
- Select initiator: `hsmsss/transport_active.go` → `runSelectProcedure`
- farewell Separate: `hsms/connection_lifecycle.go` → `writeFarewellSeparate`
- Reject senders: `hsmsss/transport_control.go` → `sendReject`, `sendRejectNotSelected`, `sendRejectTransactionNotOpen`
- Separate / Linktest responders: `hsmsss/transport_control.go` → `handleSeparateReq`, `handleLinktestReq`
- genCtx threading: `hsmsss/transport_recv.go` → `recvLoop`, `dispatchFrame`
