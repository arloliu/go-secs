---
type: Unit
title: hsms
description: Immutable HSMS message model (SEMI E37) plus the shared connection engine both transports run on.
---

# Responsibility

Owns the message layer — `ControlMessage`, `DataMessage`, construction with Q3 validation, encode/decode, lazy body decode — and the connection engine every transport reuses: generations, reply routing, protocol timers, the send path, and connection metrics.

# Boundary

Owns no sockets and no wire framing. `hsmsss` and `secs1` supply the transport and must both keep satisfying `hsms.Connection` / `hsms.Session` (prime directive 4). Item semantics belong to `secs2`.

# Entries

* [The B1/B2 Selected gates on the send path](/hsms/selected-gates.md) - what stops a data send when not Selected, and why one check is not enough.
* [Send error accounting — which outcomes count](/hsms/send-error-accounting.md) - why a normal Close mid-transaction does not inflate the error counter.
* [The W-bit inflight gauge](/hsms/inflight-gauge.md) - when a message counts as in flight, and why a leak here disables liveness probing.
* [The I1 stale-epoch write guard](/hsms/stale-epoch-write-guard.md) - how a sender stalled across a reconnect is kept off the successor's socket.
* [The reply registry's two-legged control exemption](/hsms/reply-matching-control-exemption.md) - why registration-side isData and result-side *DataMessage are two independent gates, and the v2.0.1 shape breaking both reproduces.
* [The send-side MaxMessageSize ceiling's enforcement topology](/hsms/max-message-size-ceiling.md) - where the check runs relative to writeMu, why control frames are structurally exempt, and why async accounting has no exclusion list at all.
* [The transaction observer's two chokepoints, its isData gate, and its outcome classifier](/hsms/transaction-observer-chokepoints.md) - why WithTransactionObserver instruments two call sites (not one), why the isData gate is load-bearing enough to crash the process without it, and how classifyTxOutcome relates to isCountedSendErr.
* [Where a TransitionCause is chosen, and why one transition can swallow another's cause](/hsms/transition-cause-injection-sites.md) - the full injection-site to cause map, why the transports pass a cause through a capability interface, and the three ways a cause never reaches a subscriber.

# Entry points

- frame decode: `hsms/` → `DecodeHSMSMessage`
- construction: `hsms/` → `NewDataMessage`, `NewSelectReq`, `NewLinktestReq`, `DataMessage.Derive`
- consumer surface: `hsms/` → `Connection`, `Session`, `SECS2Endpoint`
- engine knobs: `hsms/connection_config.go` → `ConnOption` constructors

# Read first

`hsms/doc.go` is exceptionally thorough — message model, the two error channels, immutability and fan-out, and a "Dissolved v1 landmines" section explaining why four v1 hazards are now structurally impossible. Read it before any entry here, and do not write an entry that restates it.
