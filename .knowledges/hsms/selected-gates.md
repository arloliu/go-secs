---
type: Mechanic
title: The B1/B2 Selected gates on the send path
description: What stops a data send when the session is not Selected, and why one check is not enough.
tags: [hsms, send, state-machine, e37]
status: stable
generated: {by: "claude/opus-5", at: 2026-08-05T12:08:31Z}
verified:
  - {by: "agy/gemini-3.1-pro-high", at: 2026-08-05T14:10:00Z}
sources:
  - {resource: hsms/connection_send.go, digest: sha256:9b1ccf21a9d24c0d, revision: 038319b}
---

# What it does

`hsms/doc.go` documents the message model and the dissolved v1 hazards but says nothing about the send path's state gates. Two of them exist, named B1 and B2 in the source, and they answer one question: what happens to a data send when the session is not Selected, including when it stops being Selected *during* the send.

# How it works

**B1** runs first, before anything is registered or written: if the message is a data message and `IsSelected()` is false, the send returns `ErrNotSelectedState` immediately. `IsSelected` reads the supervisor's lock-free atomic state, so the gate costs an atomic load on the hot path and nil-guards a supervisor that does not exist yet.

**B2** runs inside `writeFrame`, under the epoch's write mutex — the same lock that serialises frames onto the wire. It re-checks Selected at the write boundary, closing the window between B1 and the actual `writev` where a Deselect or drop could land.

Every refusal funnels through `dropNotSelected`, the single chokepoint (B3) that increments `DataMsgDropNotSelectedCount` exactly once and emits a rate-limited warning. There are four call sites, not two: the B1 gate in each of the three send entry points (`sendWaitReply`, `sendNoReply`, `SendAsync`) plus the B2 re-check inside `writeFrame`. Every one of them must route through the chokepoint or the counter silently under-reports.

# Invariants

- Control messages are never gated. Only data sends check Selected, because control traffic is what *establishes* the state the gate tests.
- A gated send is **non-fatal**: it returns an error to the caller and never tears the connection down.
- A drop is counted exactly once, at one chokepoint, and lands in its own counter — never in the general data-error counter.
- B2 must stay under the same mutex as the write. Checking Selected outside that lock reintroduces exactly the TOCTOU window B2 exists to close.

# Failure modes

- **Removing B2 as "redundant"** restores the v1-era window: a send that passed B1 can be preempted by teardown and write a data frame onto a session that is no longer Selected, which a strict peer may answer with a Reject.
- **Adding a send entry point that skips `dropNotSelected`** silently under-counts refusals, and the rate-limited warning stops firing for that path — the drop becomes invisible rather than absent.
- **Gating control messages by mistake** deadlocks the handshake: Select.req itself would be refused for not being Selected.

# Where to look

- the pre-write gate: `hsms/connection_send.go` → `(*connection).IsSelected`
- the counted chokepoint: `hsms/connection_send.go` → `(*connection).dropNotSelected`
- the write-boundary re-check, under the epoch write mutex: `hsms/connection_send.go` → `(*connection).writeFrame`
- the three gated entry points: `hsms/connection_send.go` → `(*connection).sendWaitReply`, `(*connection).sendNoReply`, `(*connection).SendAsync`
