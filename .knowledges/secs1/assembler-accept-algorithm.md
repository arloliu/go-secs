---
type: Mechanic
title: The inbound assembler's accept order, lazy T4 check, and frame-ownership handoff
description: The exact per-block check order in (*assembler).accept, why the T4 timeout is never a real timer, and how a completed message becomes an owned buffer handed to rt.DeliverOwnedFrame.
tags: [secs1, e4, assembler, timers, ownership]
status: stable
generated: {by: "claude/sonnet-5", at: 2026-09-24T15:45:31Z}
verified:
  - {by: "openai/gpt-5.6-terra", at: 2026-09-24T15:53:52Z}
sources:
  - {resource: secs1/assembler.go, digest: sha256:d10f1f694da2b9ba, revision: a7ff4a8}
  - {resource: secs1/message.go, digest: sha256:ede565d46963ed93, revision: ed4665e}
---

# What it does

`secs1/doc.go`'s "Metrics" section names the counters this algorithm produces — duplicate-block
drops, T4 partial-message timeouts, wrong-direction drops, `DeviceIDMismatchCount`,
`BlockNumberMismatchCount`, `InvalidFirstBlockCount` — but not the order in which `accept` evaluates
them, that a T4 expiry does NOT gate the later checks (so one arriving block can increment the T4
counter together with a duplicate-drop or an invalid-first-block count), or that "T4" is not a timer
at all.
`docs/secs1/05-message-protocol-and-connection-design.md` describes an earlier, superseded design —
a `map[uint64]*openMessage` keyed by system-bytes/device/R-bit, each entry carrying its own
`*time.Timer` and a `t4Cancel` channel closed to stop a dedicated T4 goroutine — that the shipped code
does not use; the real assembler holds at most one partial and checks the T4 gap lazily, with no
timer and no goroutine of its own.
Neither doc mentions `rt.DeliverOwnedFrame` or the ownership contract a completed message must
satisfy before reaching it.

# How it works

**Accept's fixed check order.**
`(*assembler).accept` runs a single already-ACK'd block through five ordered steps: device-ID routing
(1, §9.4.1, wrong ID → `ErrDeviceIDMismatch`), R-bit direction (2, §8.2, a block not addressed to this
role → dropped uncounted-as-violation, only a metric), the lazy T4 gap check (3), duplicate-header
detection (4, §9.4.2), then expected-block continuation or a fresh-first-block re-evaluation (5,
§9.4.4.2).
A drop — steps 1 and 2, and step 4 when the block matches the duplicate check — returns nil without
requesting link teardown.
Step 1 also reports a violation (`ErrDeviceIDMismatch`); steps 2 and 4 only increment a metric, with
no report.
Step 5's own abort-then-re-evaluate sub-path (see below) can likewise report a violation before
continuing — and for the equipment role, a reported violation can enqueue an S9F1/S9F7 notification
(`notifyAssemblerViolation`), so "dropped" does not mean "silent."
Step 5's continuation and fresh-start paths are not drops at all: they can run all the way to
`complete` → `deliverFrame` on an E-bit block, and a completion error from that path propagates back
through `accept`'s own return value, which both call sites discard (`transport.go`'s `_ = sink(blk)`
and `line.go`'s `_ = deliver(recv)`).
Only a step-5 completion (E-bit block) calls `complete`, which is the sole path to `deliverFrame`.

**T4 is a gap check, not a timer.**
There is no `time.Timer`, no T4 goroutine, and no cancellation channel:
`a.lastBlockTime` records when the last block was accepted, and the next block that passes device-ID
and R-bit routing (steps 1-2) is what evaluates `a.now().Sub(a.lastBlockTime) > a.timers().T4` —
a block rejected at those earlier steps never reaches the check and never expires the partial.
The check itself never returns: whether or not it fires, `accept` always falls through into duplicate
detection and the continuation/fresh-first evaluation for that SAME arriving block.
A T4 expiry discards the stale partial and increments `PartialTimeoutCount` but reports no violation,
so it can be counted together with a later step's duplicate-drop or invalid-first-block violation on
that one arrival — T4 does not "win" against or suppress those later checks, it just runs first.
A partial that never receives a next block (peer vanished) is never T4-discarded at all;
it dies only when the generation tears down, since nothing else drives the check.

**Abort-then-re-evaluate, without double-counting.**
An unexpected continuation block (wrong block number, or a header field mismatch against the open
message) aborts the partial, reports EXACTLY ONE violation (`ErrBlockNumberMismatch` or
`ErrHeaderMismatch`, chosen by which field actually differs), and then re-evaluates the SAME block as
a possible fresh first block via `startMessage(blk, false)` —
the `false` suppresses a second violation if it also fails the first-block check,
since one malformed block must produce one notification, not two.
A genuinely fresh invalid first block (arriving via `beginMessage`, not the abort path) still notifies normally.

**Duplicate memory outlives the message it protects.**
`lastHeader`/`haveLast` are compared before a message is even open and persist across `reset` — `reset`
clears `open`/`header`/`blocks`/`expected`/`lastBlockTime` but deliberately leaves `lastHeader` alone —
so a retransmitted last block, arriving AFTER the message it terminated was already delivered and the
partial reset, is still recognized and dropped rather than re-delivered as a new message.

**Frame synthesis and ownership.**
`complete` calls `assembleFrame`, which validates the same invariants as the stateless `assembleBlocks`
— E-bit exactly on the last block, identical block-invariant header fields — EXCEPT numbering:
`assembleBlocks` always requires the contiguous sequence 1..N and rejects a lone block numbered 0,
while `assembleFrame` accepts a lone block numbered 0 as a valid single-block message (D5b-12 interop
leniency).
Beyond that difference, `assembleFrame` produces a single OWNED `[10-byte HSMS header || body]` buffer
instead of a `wire.Body`: the 10 header bytes are reserved up front so the body appends with no extra
copy, and the device ID / stream / function / W-bit / system bytes are packed directly onto the HSMS
header layout (SEMI E37 §8.2.5).
`complete` calls `a.reset()` BEFORE checking `assembleFrame`'s error, so the partial state is always
cleared even if assembly itself fails; the returned buffer is independent of the input blocks' storage
(the concatenation copies each block body), so it is safe to hand to `deliverFrame`
(`rt.DeliverOwnedFrame`), which takes ownership of it — while the input `blocks` slice itself remains
reusable by the caller (see Failure modes).
`complete`'s own doc comment, and the numbered step-5 comment in `report`, used to describe this as
assemble-then-deliver-then-reset;
`a7ff4a8` corrected both to state the reset-before-error-check order this entry records.

# Invariants

- `accept`'s five checks run in a fixed order per block, but only steps 1, 2, and (conditionally) 4
  are true drop gates — the step-3 T4 check never returns, so a T4 expiry and a later duplicate-drop
  or invalid-first-block count are independent metrics that can both fire on the same block, never
  competing outcomes where one "wins."
- At most one partial message is ever open per assembler instance — there is no map, so a second
  message cannot be interleaved with an open one; an interleaving attempt is what the "abort the open
  partial" branch exists to handle.
- Within the abort-then-re-evaluate path (step 5), a single malformed continuation block produces
  exactly one violation report, never two, even when it is evaluated twice (once as an aborted
  continuation, once as a candidate first block) — `startMessage(blk, false)` suppresses the second.
  This scoping is local to that one path: it does not make T4 (step 3) mutually exclusive with the
  later steps (see "T4 is a gap check, not a timer").
- `reset` clears only the partial-message accumulation fields (`open`, `header`, `blocks`, `expected`,
  `lastBlockTime`); `lastHeader`/`haveLast` (duplicate-detection memory) and every configuration or
  dependency field (role, device ID, callbacks, clock, timers, metrics) are left untouched — duplicate
  memory is message-boundary-independent by design.

# Failure modes

- A partial whose peer never sends another block is never actively discarded — it sits in `a.open`
  until either a new block's arrival trips the lazy T4 check or the generation tears down; there is no
  standalone symptom for "stuck partial," only the eventual next-block or teardown behavior.
- If `complete`'s `reset()` call were reordered after the `assembleFrame` error check, a failed
  assembly would leave a stale partial for the next block to inherit instead of starting clean.
- Mutating or reusing `assembleFrame`'s returned buffer after handing it to `deliverFrame` would
  corrupt a frame already handed off under ownership, since `deliverFrame` assumes exclusive control
  of that buffer from that point on.
  The input `blocks` slice and its block-body storage are a separate concern: `assembleFrame` copies
  every block body into the new buffer, so the `blocks` slice and its backing array are safe to reuse
  once assembly finishes — `reset`'s own truncate-not-nil of `a.blocks` relies on exactly this.

# Where to look

- the fixed five-step per-block algorithm: `secs1/assembler.go` → `(*assembler).accept`
- first-block validation and D5b-12 leniency: `secs1/assembler.go` → `(*assembler).startMessage`
- completion and the reset-before-error-check ordering: `secs1/assembler.go` → `(*assembler).complete`
- the reset that spares duplicate-detection memory: `secs1/assembler.go` → `(*assembler).reset`
- owned-buffer synthesis handed to `DeliverOwnedFrame`: `secs1/message.go` → `assembleFrame`
