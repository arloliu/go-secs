---
type: Mechanic
title: The W-bit inflight gauge
description: When a data message counts as in flight, and why a leaked increment suppresses automatic probing when linktest suppression is enabled.
tags: [hsms, metrics, send, liveness]
status: stable
generated: {by: "claude/sonnet-5", at: 2026-09-24T11:50:00Z}
verified:
  - {by: "openai/gpt-5.6-terra", at: 2026-09-24T12:34:28Z}
sources:
  - {resource: hsms/connection_send.go, digest: sha256:37c6bd273ed11699, revision: 922feb8}
  - {resource: hsms/connection.go, digest: sha256:82d716dbf253b02c, revision: 922feb8}
  - {resource: hsms/connection_config.go, digest: sha256:1dd3eb7cbc113324, revision: 4eb40d1}
  - {resource: hsmsss/transport_procedures.go, digest: sha256:ae651d8a0289a097, revision: 4eb40d1}
  - {resource: hsmsss/transport_recv.go, digest: sha256:f78883ced9f30422, revision: 4eb40d1}
---

# What it does

The inflight gauge counts sent data messages still awaiting a reply.
`WithLinktestSuppression`'s own godoc (`hsms/connection_config.go`) already connects the two facts at the policy level:
no Linktest.req is sent while a sent data message is awaiting its reply.
What the public docs do not say is the mechanics behind that connection —
exactly when the gauge increments and decrements relative to the write,
and how `hsmsss` reaches a `hsms.connection`'s private counter without widening the exported `TransportRuntime` interface.
That narrower mechanic is the delta this entry records.

# How it works

The gauge is incremented exactly once, **after** the frame is on the wire, and only for a data message with the W-bit set.
A single deferred decrement covers all four terminal branches of the reply wait, so no return path can skip it.

This logic lives in `sendWaitReplyOn`, the body `sendWaitReply` runs after resolving the live epoch.
`WriteMessageFromGeneration` — the generation-bound entry point the in-module Select.req/Linktest.req initiators use —
calls `sendWaitReplyOn` directly against a caller-named generation, so both entry points get the same gauge accounting for free.

`hsmsss` reaches the gauge through `DataMsgInflight()`, deliberately routed via a package-local capability interface rather than the exported `TransportRuntime` — widening that exported interface would break external implementers. `LinktestSuppression()` travels the same way.

# Invariants

- W-bit data only.
  Control transactions never touch the gauge —
  including them would misrepresent outstanding *data* replies with a control round trip.
  This is a correctness-of-meaning argument, not a self-suppression one.
  Probes run sequentially on one goroutine,
  and a probe's own deferred decrement completes before the next iteration's suppression check —
  so even a control-counting gauge would not suppress its own next probe.
- Increment happens after the write succeeds, not before, so a message that never reached the wire is never counted as awaiting a reply.
- Exactly one decrement per increment, via one defer covering every terminal branch. This is a correctness constraint on the control flow, not a bookkeeping preference.
- Fire-and-forget sends never touch the gauge: they short-circuit before it, having no reply to await.
- The capability interface is package-local on purpose. Moving these methods to the exported runtime interface is a breaking change for external implementers.

# Failure modes

- **A leaked increment disables automatic probing, but only with suppression enabled.**
  Any new early return placed between the increment and the deferred decrement leaves the gauge permanently above zero.
  With `WithLinktestSuppression` on, the suppression rule then skips every subsequent probe.
  With suppression off, the leak has no effect: `runLinktest` never reads the gauge in that mode.
  Receive-side errors (`hsmsss/transport_recv.go`'s `recvLoop`) and other disconnect mechanisms still operate independently of the gauge,
  so a socket-level failure is still caught.
  The leak's real risk is a peer that stops answering without ever producing a read error, which then goes undetected until an application send fails.
  The symptom appears in `hsmsss`, far from the edit.
  Treat any change to the send path's return structure as a liveness change —
  see [activity stamps](/hsmsss/activity-stamps.md).
- **Incrementing before write success is not itself a leak.**
  It would temporarily count a pending write as a reply wait and could suppress a probe during a stalled write.
  A lasting upward bias still requires a missing decrement on the failure path — the ordering change alone does not cause one.
- **Extending the gauge to control messages is wrong for a different reason than self-suppression.**
  It would misrepresent outstanding data replies with control traffic.
  It would *not* make a sequential linktest suppress its own next probe —
  the probe's deferred decrement runs before the next iteration's suppression check.

# Where to look

- increment and the single covering defer: `hsms/connection_send.go` → `(*connection).sendWaitReplyOn` (called by `(*connection).sendWaitReply` and `(*connection).WriteMessageFromGeneration`)
- the capability methods the transport reaches through: `hsms/connection.go` → `(*connection).DataMsgInflight`, `(*connection).LinktestSuppression`
- the sequential probe loop and its two suppression checks: `hsmsss/transport_procedures.go` → `(*transport).runLinktest`
- the independent, gauge-unrelated disconnect trigger: `hsmsss/transport_recv.go` → `(*transport).recvLoop`
- the public suppression contract this entry narrows: `hsms/connection_config.go` → `WithLinktestSuppression`
