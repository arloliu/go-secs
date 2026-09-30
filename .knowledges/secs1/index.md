---
type: Unit
title: secs1
description: SECS-I (SEMI E4) over TCP — block framing and the half-duplex line engine, on the shared hsms core.
---

# Responsibility

Splits an immutable SECS-II body into ≤244-byte transport blocks and reassembles inbound blocks, and owns the SECS-I connection: ENQ/EOT/ACK/NAK block handshakes, T1/T2/T4 timers, RTY retransmission, master/slave contention, and the S9Fx assembler-violation replies.

# Boundary

Does not own the message model, reply routing, generation lifecycle, or T3 — those are `hsms`.
`secs1.Connection` embeds `hsms.Connection`, so it substitutes for `hsmsss` (prime directive 4).

# Entries

* [The block-send transaction's detect/act split and RTY counting](/secs1/block-send-detect-act-split.md) - how sendBlockOnce/sendBlock divide contention detection from the yield action, and how ErrSendFailed surfaces.
* [The inbound assembler's accept order, lazy T4 check, and frame-ownership handoff](/secs1/assembler-accept-algorithm.md) - the fixed per-block check order, why T4 is a lazy gap check not a timer, and the owned-buffer handoff to DeliverOwnedFrame.

# Entry points

- construction: `secs1/` → `New`, `NewConfig`
- consumer surface: `secs1/` → `Connection`, `Connection.BlockMetrics`
- block counters: `secs1/` → `ConnectionMetrics`

# Read first

`secs1/doc.go` documents the public contract in depth — framing, the write-timeout override, the inline-handler deadlock rule, `SelectedState` naming, metrics, and assembler violations. Design history is in `docs/secs1/`.
