---
type: Mechanic
title: The W-bit inflight gauge
description: When a data message counts as in flight, and why a leak here silently disables liveness probing.
tags: [hsms, metrics, send, liveness]
status: stable
generated: {by: "claude/opus-5", at: 2026-08-05T12:08:31Z}
verified:
  - {by: "claude/opus-5", at: 2026-08-05T13:05:00Z}
  - {by: "codex/cli", at: 2026-08-05T13:50:00Z}
sources:
  - {resource: hsms/connection_send.go, digest: sha256:7e589dff2dee86f0, revision: 3660aa4}
  - {resource: hsms/connection.go, digest: sha256:ff66ee176cddfc56, revision: 3660aa4}
---

# What it does

The inflight gauge counts sent data messages still awaiting a reply. It looks like a metric and is documented as one, but it is load-bearing: `hsmsss` reads it through a capability interface to decide whether to skip a linktest probe. Nothing in the public docs connects those two facts, which is why a change that looks like a metrics tweak can disable liveness detection.

# How it works

The gauge is incremented exactly once, **after** the frame is on the wire, and only for a data message with the W-bit set. A single deferred decrement covers all four terminal branches of the reply wait, so no return path can skip it.

`hsmsss` reaches the gauge through `DataMsgInflight()`, deliberately routed via a package-local capability interface rather than the exported `TransportRuntime` — widening that exported interface would break external implementers. `LinktestSuppression()` travels the same way.

# Invariants

- W-bit data only. Control transactions never touch the gauge, so a linktest can never suppress itself — the probe would otherwise raise the very gauge that causes probes to be skipped.
- Increment happens after the write succeeds, not before, so a message that never reached the wire is never counted as awaiting a reply.
- Exactly one decrement per increment, via one defer covering every terminal branch. This is a correctness constraint on the control flow, not a bookkeeping preference.
- Fire-and-forget sends never touch the gauge: they short-circuit before it, having no reply to await.
- The capability interface is package-local on purpose. Moving these methods to the exported runtime interface is a breaking change for external implementers.

# Failure modes

- **A leaked increment disables linktest entirely.** Any new early return placed between the increment and the deferred decrement leaves the gauge permanently above zero; the suppression rule then skips every probe, and a dead peer goes undetected until an application send fails. The symptom appears in `hsmsss`, far from the edit. Treat any change to the send path's return structure as a liveness change — see [activity stamps](/hsmsss/activity-stamps.md).
- **Incrementing before the write** counts messages that failed to send, biasing the gauge upward and suppressing probes during exactly the failures that need probing.
- **Extending the gauge to control messages** makes the linktest suppress itself: the probe raises the gauge, so the next probe is skipped.

# Where to look

- increment and the single covering defer: `hsms/connection_send.go` → `(*connection).sendWaitReply`
- the capability methods the transport reaches through: `hsms/connection.go` → `(*connection).DataMsgInflight`, `(*connection).LinktestSuppression`
