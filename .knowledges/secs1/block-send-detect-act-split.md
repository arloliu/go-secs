---
type: Mechanic
title: The block-send transaction's detect/act split and RTY counting
description: How sendBlockOnce (detect) and sendBlock (act + count) divide the ENQ/EOT handshake, contention, and RTY retry across the single line-engine goroutine, and how a failure surfaces.
tags: [secs1, e4, line-protocol, timers, contention]
status: stable
generated: {by: "claude/sonnet-5", at: 2026-09-24T18:00:36Z}
verified:
  - {by: "openai/gpt-5.6-terra", at: 2026-09-24T18:01:57Z}
sources:
  - {resource: secs1/line.go, digest: sha256:363cb924dca72a53, revision: d244104}
  - {resource: secs1/transport.go, digest: sha256:399009fc96b7bf6c, revision: 4be2062}
  - {resource: hsms/connection_lifecycle.go, digest: sha256:8a7a71d56304a338, revision: b43b798}
---

# What it does

`secs1/doc.go`'s "Half-duplex line engine" section names the outcomes — T2 bounds the EOT/length/ACK
waits, T1 bounds inter-character reads, RTY retransmits, "the master... wins; the slave yields,
delivers the master's block, then re-sends its own as a fresh transaction" — and that RTY exhaustion
"returns an error from the transport, which the core treats as a line failure."
It does not say HOW that outcome is produced: which function detects contention versus which one
acts on it, why the split exists, how the retry counter actually behaves when a contention yield's
receive itself fails, or which goroutine any of this runs on.
The real split is two plain functions and a `sendResult` classification, not a state enum.

# How it works

**The detect/act split.**
`(*lineIO).sendBlockOnce` performs exactly one ENQ-and-wait attempt: it writes ENQ, then loops reading
bytes within a single T2 deadline, ignoring every byte except EOT (grant — transmit and wait for ACK
via `sendBlockData`) and, for a slave, a contending ENQ (§7.8.2.1 noise-ignoring rule).
On a slave, a contending ENQ makes `sendBlockOnce` return `sendContention` immediately — it does
**not** grant the line, receive the block, or touch the retry counter itself; a master (`isEquip`)
never returns `sendContention` at all, it just keeps waiting past the peer's ENQ for its own EOT.
`(*lineIO).sendBlock` is the caller that owns the retry loop and, critically, owns the yield ACTION:
on seeing `sendContention` it writes EOT, calls `receiveBlock` to take the master's block, and only
THEN delivers it through the same per-generation sink `sendBlock` was handed as a parameter.

**RTY counting and anti-starvation.**
`sendBlock` loops `for retry <= retryLimit`: `ErrSendFailed` follows `retryLimit+1` *counted* failures
with no intervening successful contention yield — that is a bound on consecutive counted failures, NOT
a hard cap on total attempts or elapsed time.
`sendBlock`'s own doc comment used to read as a flat cap ("the block is sent up to retryLimit+1
times"); `a7ff4a8` corrected both it and the `retryLimit` parameter doc to say the same thing this
entry does — retryLimit+1 consecutive failed attempts, reset by a completed contention yield.
A plain `sendRetry` outcome (T2 timeout waiting for EOT/ACK, or a non-ACK byte) increments `retry` and
loops.
A successful contention yield (EOT granted, block received and delivered) resets `retry` to 0 and
restarts the postponed send as a fresh transaction, per §7.8.2.1; because the reset drops the counter
back to 0, a peer that keeps winning contention and then successfully receiving can keep this loop
going indefinitely — RTY bounds neither the total attempts nor how long the send eventually takes.
A yield whose `receiveBlock` itself fails with a *protocol* error (T1/T2 timeout, bad length, bad
checksum) does NOT reset the counter, though; it increments `retry` exactly like an ordinary
`sendRetry`, so a peer that keeps winning contention and then failing to deliver cannot pin this
side's retry budget at zero forever.
A yield whose deliver callback errors is non-fatal to the send (the link stays up) — only its own
protocol failure feeds the counter.

**Which goroutine, and how contention is even possible.**
`(*transport).lineEngine` is the single per-generation goroutine that owns the conn (the G-A
invariant); it multiplexes teardown, a pending outbound send taken off `sendReqCh`, and an idle poll
for an inbound ENQ, in that priority order, every loop iteration.
Contention is therefore never two goroutines racing the socket — it is this one goroutine, mid-send
inside `sendBlockOnce`'s T2 wait, observing the PEER's ENQ arrive instead of the EOT it asked for.
The idle-poll branch of `lineEngine` (byte arrives with no send pending) and the contention branch
inside `sendBlock` both end up calling the same `receiveBlock`, but only the latter is "contention" —
the two paths are structurally different entries into one function.

**How a failure surfaces from outside.**
`(*transport).Write` does NOT simply hand a `*sendReq` to `sendReqCh` and block on `req.done`: both the
handoff and the result wait are `select`s that race `genDone` (teardown). Selecting `genDone` during
EITHER wait returns `hsms.ErrConnClosed` immediately, and because Go's `select` picks pseudo-randomly
among ready cases, a teardown racing an already-ready `req.done` can win — so `Write` can return
`ErrConnClosed` even when `runSend`→`sendBlock` already produced a real result, including
`ErrSendFailed` once RTY is exhausted.
Short of that race, `Write` returns the engine's result unchanged.
`ErrSendFailed`'s dynamic type also satisfies `hsms.TransientError` (`errors.go`), so a caller doing
`hsms.IsTransient` sees the same sentinel as always via `errors.Is`, just marked retryable; the caller
never sees which specific block or retry count failed.

# Invariants

- `sendBlockOnce` never mutates `retry` and never performs the contention ACTION — only `sendBlock`
  does both, so counting and yielding cannot drift out of sync across the two functions.
- Exactly one goroutine (`lineEngine`) ever reads or writes the conn per generation; contention is
  therefore always this goroutine observing the peer preempt its own outstanding ENQ, never a
  cross-goroutine race.
- The retry counter resets to 0 only on a SUCCESSFUL contention yield (deliver reached, even if the
  deliver callback itself errored) — a failed receive during a yield still counts as a retry.

# Failure modes

- `retryLimit+1` counted failures with no intervening successful contention yield returns
  `ErrSendFailed` from `Write`; the core treats this as a line failure and tears the generation down.
  Unless shutdown intervenes, recovery calls `(*transport).Start` again: an active transport re-dials,
  a passive transport re-listens and accepts.
  `secs1/doc.go` used to say "the reconnect loop re-dials", an overgeneralization that only fit the
  active side; `a7ff4a8` corrected it to "starts the transport again (an active transport re-dials;
  a passive one re-listens)" (see `hsms/connection_lifecycle.go` → `(*connection).connectLoop`, which
  just re-invokes `Start`).
- A peer that repeatedly wins contention and then times out or sends a bad block on the yield receive
  looks, from outside, like ordinary retry exhaustion — nothing distinguishes "lost every race" from
  "won every race but failed to deliver" in the returned error.
- Treating `sendContention` as if it reset the counter unconditionally (rather than only on a
  successful receive) would let a peer that always wins contention but never actually sends starve
  this side's RTY budget indefinitely — the anti-starvation branch in `sendBlock` is what prevents it,
  but only once `receiveBlock` itself returns.
  A peer that keeps the line continuously noisy (never silent for T1) can keep `drainUntilSilence`
  from returning at all, so the counter never advances either way — this guard bounds repeated
  *completed* protocol failures, not the overall time to send completion.

# Where to look

- one ENQ attempt, detect-only contention: `secs1/line.go` → `(*lineIO).sendBlockOnce`
- block transmit + ACK wait: `secs1/line.go` → `(*lineIO).sendBlockData`
- RTY loop + the yield ACTION + anti-starvation counting: `secs1/line.go` → `(*lineIO).sendBlock`
- inbound receive path the yield reuses: `secs1/line.go` → `(*lineIO).receiveBlock`
- the single goroutine and its duty priority: `secs1/transport.go` → `(*transport).lineEngine`
- where a send enters and `ErrSendFailed` exits to the caller: `secs1/transport.go` → `(*transport).Write`
- active-vs-passive dispatch on a returned send failure's recovery: `secs1/transport.go` → `(*transport).Start`
- the core's generic reconnect loop that re-invokes `Start` after a line failure: `hsms/connection_lifecycle.go` → `(*connection).connectLoop`
