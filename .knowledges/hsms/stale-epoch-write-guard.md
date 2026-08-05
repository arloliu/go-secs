---
type: Mechanic
title: The I1 stale-epoch write guard
description: How a sender stalled across a reconnect is prevented from writing onto the successor generation's socket.
tags: [hsms, send, generations, lifecycle, race]
status: stable
generated: {by: "claude/opus-5", at: 2026-08-05T13:20:00Z}
verified:
  - {by: "claude/opus-5", at: 2026-08-05T13:22:00Z}
sources:
  - {resource: hsms/connection_send.go, digest: sha256:e534fecb0a49c465, revision: bc97919}
  - {resource: hsms/epoch.go, digest: sha256:ae43d006ecec624d, revision: bc97919}
---

# What it does

`hsms/doc.go` has a "Dissolved v1 landmines" section covering *stale frame carried across generations* — but that entry is about the **asynchronous** path: each generation gets a fresh send channel, so a frame queued in generation N cannot reach generation N+1's sender goroutine.

This is the other half, and no doc mentions it. A **synchronous** sender does not go through the send channel: it writes on its own goroutine, holding a pointer to the epoch it loaded when the call began. Nothing about a fresh send channel protects it. The I1 guard is what stops that sender from writing onto a successor's socket.

# How it works

`sendWaitReply` loads `c.cur` exactly once, pinning that call to epoch N. It may then block — on the write mutex, on scheduling — long enough for a full teardown and reconnect to complete, so that by the time it acquires `writeMu`, epoch N+1 owns the connection.

`writeFrame` therefore captures the socket from **the epoch it was handed**, not from any live/dynamic transport field: `conn := e.liveConn()`, taken up front under `writeMu`, reading `e.conn` under the epoch's own `connMu`. The write below targets exactly epoch N's socket — which teardown already closed, so the write errors — and can never resolve epoch N+1's live socket.

A nil capture means teardown niled the socket first: the guard fails closed with `ErrConnClosed`, so a stale sender never reaches the wire and its reply wait unwinds exactly as if the context guard had fired. The `e.ctx.Done()` check that follows is a cheap early-out on the common teardown case, **not** the race fix.

# Invariants

- The conn is captured from the epoch parameter, under `writeMu`, before anything else in the write. Reading a dynamic transport field instead reopens the exact TOCTOU this closes: a sender descheduled after passing a context check could writev onto the successor's socket, outside that generation's `writeMu`, with only the OS writev lock protecting byte integrity.
- A context check alone is insufficient and always was. Cancellation can land between the check and the write; the binding cannot.
- Failing closed on a nil conn is required. Falling through to "no socket, skip the write, report success" would silently drop a frame the caller believes was sent.
- The test seam runs **after** the conn capture, specifically so a teeth-test can prove the write still targets the captured epoch's socket rather than the successor's.

# Failure modes

- **Replacing the captured conn with a live lookup** ("it's the same socket anyway") restores a cross-generation write: a stalled sender injects a frame from a dead session onto a fresh connection, where it arrives with stale System Bytes and, at worst, interleaved with another writer's bytes.
- **Moving the test seam above the capture** makes the I1 teeth-test vacuous — it would then exercise a window the guard doesn't cover, and pass whether or not the guard exists.
- **Treating `ErrConnClosed` from the nil-conn path as a transport failure** would inflate the data-error counter on ordinary teardown; see [send error accounting](/hsms/send-error-accounting.md).

# Where to look

- the capture, the seam, and the fail-closed path: `hsms/connection_send.go` → `(*connection).writeFrame`
- the sender's single pin to its epoch: `hsms/connection_send.go` → `(*connection).sendWaitReply`
- the guarded socket read: `hsms/epoch.go` → `(*epoch).liveConn`
- per-generation state this rides on: `hsms/epoch.go` → `newEpoch`
