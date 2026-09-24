---
type: Mechanic
title: The B1/B2 Selected gates on the send path
description: What stops a data send when the session is not Selected, and why one check is not enough.
tags: [hsms, send, state-machine, e37]
status: stable
generated: {by: "claude/sonnet-5", at: 2026-09-24T11:50:00Z}
verified:
  - {by: "openai/gpt-5.6-terra", at: 2026-09-24T12:34:28Z}
sources:
  - {resource: hsms/connection_send.go, digest: sha256:37c6bd273ed11699, revision: 922feb8}
  - {resource: hsms/supervisor.go, digest: sha256:1a4e9385175f90e9, revision: 4eb40d1}
---

# What it does

`hsms/doc.go` documents the message model and the dissolved v1 hazards but says nothing about the send path's state gates. Two of them exist, named B1 and B2 in the source, and they answer one question: what happens to a data send when the session is not Selected, including when it stops being Selected *during* the send.

# How it works

**B1** runs first among the *Selected* checks, after epoch resolution.
`sendWaitReply` and `enqueueAsync`'s plain entry points return `ErrNotOpen` first when no epoch is open at all,
and `WriteMessageFromGeneration`/`SendAsyncFromGeneration` reject a caller-named generation that is no longer live with `ErrConnClosed` before B1 ever runs.
Once an epoch is resolved, B1 checks first:
if the message is a data message and `IsSelected()` is false, the send returns `ErrNotSelectedState` immediately.
`IsSelected` reads the supervisor's lock-free atomic state,
so the gate costs an atomic load on the hot path and nil-guards a supervisor that does not exist yet.

**B2** runs inside `writeFrame`, under the epoch's write mutex — the same lock that serialises frames onto the wire.
It re-checks Selected after acquiring that mutex,
catching a state change that happened before the check, including one that landed while this write was waiting for another writer to release the mutex.
It narrows the B1-to-write race; it does not eliminate it.
`(*supervisor).commitFrom` changes state with a bare CAS on an atomic, not under `writeMu`,
so a Deselect can still land in the instructions between B2's `IsSelected()` check and the actual `tr.Write` call, both of which execute while `writeMu` is held.

Every refusal funnels through `dropNotSelected`, the single chokepoint (B3) that increments `DataMsgDropNotSelectedCount` exactly once and emits a rate-limited warning.
There are four B1/B2 gate *code sites*, not two:
`sendWaitReplyOn` (the shared body behind `sendWaitReply` and the generation-bound `WriteMessageFromGeneration`), `sendNoReply`, `enqueueAsync` (the shared body behind `SendAsync` and the generation-bound `SendAsyncFromGeneration`), plus the B2 re-check inside `writeFrame`.
Every one of them must route through the chokepoint or the counter silently under-reports.

# Invariants

- Control messages are never gated. Only data sends check Selected, because control traffic is what *establishes* the state the gate tests.
- A gated send is **non-fatal**: it never tears the connection down.
  A synchronous refusal (`sendWaitReplyOn`/`sendNoReply`, B1 or B2) and an async B1 refusal (`enqueueAsync`) both return `ErrNotSelectedState` to the caller directly.
  An async B2 refusal is different: it happens later, inside `drainSendCh`, after `enqueueAsync` already returned success to the caller.
  It increments `AsyncSendErrCount` and invokes the configured async error handler instead of returning anything to the original caller.
- A drop is counted exactly once, at one chokepoint, and lands in its own counter — never in the general data-error counter.
- B2 must stay under the same mutex as the write.
  Checking Selected outside that lock would drop even the narrowing B2 provides against a concurrent writer.

# Failure modes

- **Removing B2 as "redundant"** restores the v1-era window: a send that passed B1 can be preempted by teardown and write a data frame onto a session that is no longer Selected, which a strict peer may answer with a Reject.
- **Adding a send entry point that skips `dropNotSelected`** silently under-counts refusals, and the rate-limited warning stops firing for that path — the drop becomes invisible rather than absent.
- **Gating control messages by mistake** deadlocks the handshake: Select.req itself would be refused for not being Selected.

# Where to look

- the pre-write gate: `hsms/connection_send.go` → `(*connection).IsSelected`
- the counted chokepoint: `hsms/connection_send.go` → `(*connection).dropNotSelected`
- the write-boundary re-check, under the epoch write mutex: `hsms/connection_send.go` → `(*connection).writeFrame`
- the gate code sites: `hsms/connection_send.go` → `(*connection).sendWaitReplyOn`, `(*connection).sendNoReply`, `(*connection).enqueueAsync`
- the entry points that reuse them: `hsms/connection_send.go` → `(*connection).sendWaitReply`, `(*connection).WriteMessageFromGeneration`, `(*connection).SendAsync`, `(*connection).SendAsyncFromGeneration`
- the async B2 refusal path, which reports to the error handler rather than the enqueue caller: `hsms/connection_send.go` → `(*connection).drainSendCh`
- the atomic CAS that moves state without `writeMu`, the reason B2 narrows rather than closes the race: `hsms/supervisor.go` → `(*supervisor).commitFrom`
