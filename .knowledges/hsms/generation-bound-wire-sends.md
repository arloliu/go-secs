---
type: Mechanic
title: How the recv path's responses and requests are bound to a generation on the wire
description: Why a response or request produced by one generation's recv dispatch or procedure goes through a generation-bound send, how the gate resolves the epoch without holding the lock across the enqueue, what a refused send does, and why the public counters follow the enqueue rather than the wire.
tags: [hsms, hsmsss, send, generation, concurrency]
status: stable
generated: {by: "claude/opus-5.5", at: 2026-09-26T10:24:15Z}
verified:
  - {by: "openai/gpt-5.6-terra", at: 2026-09-26T10:28:15Z}
sources:
  - {resource: hsms/connection_send.go, digest: sha256:0dcb81d9fccb4e5f, revision: be7a75b}
  - {resource: hsms/connection_lifecycle.go, digest: sha256:84c70134ab8b14a4, revision: be7a75b}
  - {resource: hsms/connection.go, digest: sha256:3c8a78070c937d5a, revision: be7a75b}
  - {resource: hsms/epoch.go, digest: sha256:d6cc2283dd4d5e58, revision: be7a75b}
  - {resource: hsms/reply_registry.go, digest: sha256:20d02d66955eca24, revision: be7a75b}
  - {resource: hsmsss/transport_control.go, digest: sha256:ad8c57da5652a769, revision: be7a75b}
  - {resource: hsmsss/transport_procedures.go, digest: sha256:8ef63578b72806e0, revision: be7a75b}
  - {resource: hsmsss/transport_active.go, digest: sha256:689f931cb195678f, revision: be7a75b}
  - {resource: hsmsss/transport_recv.go, digest: sha256:f8d35637783f58a6, revision: be7a75b}
  - {resource: hsmsss/metrics.go, digest: sha256:e822f4b53757800e, revision: be7a75b}
  - {resource: hsmsss/transport_passive.go, digest: sha256:f00a1ae9c47094d4, revision: be7a75b}
---

# What it does

`SendAsync` and `WriteMessage` resolve the current generation at call time, and their godoc says nothing about a caller that speaks for one generation.
This entry records why the HSMS-SS recv path cannot use them,
how `SendAsyncFromGeneration` / `WriteMessageFromGeneration` bind a frame to a named generation,
and what the counters that sit on this path actually count.
The FSM side of the same binding is in [how a report from an ended generation is kept off its successor](/hsms/generation-report-fence.md).

# How it works

**A generation-guarded FSM commit does not protect the WIRE, so the recv path binds its answers too.**
When `genRuntime` is available they go through `SendAsyncFromGeneration`; otherwise `sendResponse` falls back to the unqualified `SendAsync`.
This is a separate rule from the three commits, and the reason is that a frame cannot be retracted.
`SendAsync` resolves `c.cur` at call time — correct for a caller that speaks for whatever generation is current,
wrong for one that speaks for a named generation.
Every response produced by the ESTABLISHED generation's recv dispatch is the second kind:
`Select.rsp`, `Deselect.rsp`, `Linktest.rsp`, and the three `Reject.req` variants
(`sendReject`, `sendRejectNotSelected`, `sendRejectTransactionNotOpen`)
are all built from a request that arrived on ONE generation's socket, and all carry that request's System Bytes back.
A straggler using the unqualified send therefore hands the SUCCESSOR's peer a response to a transaction it never opened.

`connection.SendAsyncFromGeneration` is the bound entry point,
and `hsmsss`'s `sendResponse` wrapper is the site every one of THOSE responses goes through
(`g.gen` reaches it from `dispatchFrame`, which is why the Reject helpers and `handleLinktestReq` take the bundle).
The passive extra-connection refusal is a SEPARATE exchange, not routed through `sendResponse` at all:
`refuseExtraConn` (`hsmsss/transport_passive.go`) writes a status-1 Select.rsp directly to its captured
extra socket, under that socket's own deadline, because that socket never became the generation's live
connection and has no `g.gen` to bind through.
The rule is enqueue-iff-live: `connection.liveEpoch` applies the same `{id, !ended}` test under the same gate,
and a send naming a dead generation is DROPPED and counted in `connection.staleSend`, never redirected.
Dropping is the correct answer rather than a fallback:
the socket that asked the question is gone, so its answer has no destination.

The gate discipline differs from `commitGate`'s in one way that matters.
A CAS runs under the RLock; an enqueue cannot,
because `epoch.sendCh` can be full and nothing that blocks may run under the gate.
So the gate RESOLVES the epoch and is released,
and the enqueue targets that resolved epoch object rather than re-reading `c.cur` —
which is what keeps a swap after the unlock from redirecting the frame.
A teardown landing in that window is raced rather than excluded, and harmlessly:
the enqueue unblocks on the resolved epoch's own ctx,
and anything already queued on a torn-down epoch is stranded with it (`drainSendCh`, C1).

**A REQUEST needs the same binding, and needs it more, because it opens a transaction.**
`runSelectProcedure`'s `Select.req` and `runLinktest`'s `Linktest.req` go through `WriteMessage`, which is reply-correlated rather than queued.
The unqualified path resolves `c.cur` at call time and then does everything on that epoch:
it REGISTERS the reply channel in that epoch's registry and writes through that epoch's socket.
A straggler therefore opens a transaction on the SUCCESSOR.
Neither existing guard stops it.
`writeFrame`'s I1 conn binding protects a sender already PINNED to its own epoch — the stale caller never pinned one, it resolved a fresh one.
The cancelled generation ctx does not either: `writeFrame` checks the RESOLVED epoch's ctx, which is live, and the hsmsss transport ignores the caller ctx entirely.

The damage is worse than a stray response.
If the answer beats the caller's cancellation-deregistration, `RouteReply` finds the successor's registration,
and the reply can reach it and trigger the successor's Select commit,
even if the waiting caller ultimately returns cancellation —
`sendWaitReplyOn`'s `callerCtx.Done()` arm does not re-check a ready reply.
hsmsss's `transport.dispatchFrame` commits whenever routing a status-0 `Select.rsp` succeeds,
so the successor's own recv loop commits to Selected over a handshake that link never ran.
If cancellation wins instead, the answer is orphaned and the successor answers it with a `Reject.req` (§8.3.20) — a Reject to a peer that did nothing wrong.
A stale `Linktest.req` can also arrive while the successor is still NotSelected, which E37.1 §7.4 classifies as a communications failure the peer may answer by closing the new link.

`connection.WriteMessageFromGeneration` binds it, and one resolution covers both halves because the epoch owns both the registry and the socket:
`liveEpoch(gen)` under the gate, then `sendWaitReplyOn(ctx, e, msg)` — the shared body `sendWaitReply` also calls, with `c.cur` — so registration, write, and the `ErrConnClosed` wait arm all name the same epoch.
A refused request is dropped and counted before anything is registered, and reported as `ErrConnClosed`,
which both procedures read as their own generation's death.
That is what makes a refusal exit quietly instead of counting a linktest failure or reporting a TCPDown for a frame that was never sent.

**A refused response is not an enqueued one, and the public counters follow the enqueue, not the wire.**
`RejectSentCount` (Rejects emitted) and `LinktestReqRecvCount` (probes answered) increment when `sendResponse`
returns nil — that is, when `enqueueAsync` accepts the message onto `epoch.sendCh`, NOT when
`drainSendCh`'s later `writeFrame` actually puts it on the wire.
An enqueue that succeeds can still be followed by a write failure or a teardown that strands the queued
frame (`drainSendCh`, C1), so these counters are proof of acceptance into the send queue, not of
successful wire delivery.
`LinktestSendCount` is the opposite shape: it is an ATTEMPT counter, incremented in `runLinktest` BEFORE
the synchronous `writeMessage` call that actually sends the probe, as its godoc states.

# Failure modes

**A stale request opening a transaction on the successor.**
An unqualified `WriteMessage` from a straggler registers its reply on the successor's registry and writes on the successor's socket.
The answer can then drive the successor's Select commit over a handshake that link never ran,
or be orphaned and answered with a `Reject.req` the peer did nothing to earn.

**A straggler's response reaching the successor's peer.**
An unqualified `SendAsync` from a straggler's recv dispatch hands the SUCCESSOR's peer a `Select.rsp`, `Deselect.rsp`, `Linktest.rsp`, or `Reject.req`
carrying System Bytes of a transaction that peer never opened.

# Where to look

- the wire-side binding: `hsms/connection_send.go` → `SendAsyncFromGeneration`, `WriteMessageFromGeneration`, `sendWaitReplyOn`;
  `hsms/connection_lifecycle.go` → `liveEpoch`;
  `hsms/connection.go` → `staleSend`;
  `hsmsss/transport_control.go` → `sendResponse`, `handleDeselectReq`;
  `hsmsss/transport_procedures.go` → `runLinktest`;
  `hsmsss/transport_active.go` → `runSelectProcedure`;
  `hsmsss/transport_recv.go` → `dispatchFrame`;
  `hsmsss/transport_passive.go` → `refuseExtraConn`
- the counters: `hsmsss/transport_control.go` → `sendResponse`; `hsmsss/transport_procedures.go` → `runLinktest`
