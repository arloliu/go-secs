---
type: Unit
title: hsms
description: Immutable HSMS message model (SEMI E37) plus the shared connection engine both transports run on.
---

# Responsibility

Owns the message layer — `ControlMessage`, `DataMessage`, construction with data-message validation, encode/decode, lazy body decode — and the connection engine every transport reuses: generations, reply routing, protocol timers, the send path, and connection metrics.

# Boundary

Owns no sockets and no wire framing.
`hsmsss` and `secs1` supply the transport through the unexported `transport` seam,
and `hsmsss.New` / `secs1.New` must both keep returning a `Connection` that satisfies `hsms.Connection` (prime directive 4).
Item semantics belong to `secs2`.

# Entries

* [The B1/B2 Selected gates on the send path](/hsms/selected-gates.md) - what stops a data send when not Selected, and why one check is not enough.
* [Send error accounting — which outcomes count](/hsms/send-error-accounting.md) - which synchronous data-send outcomes count, and why ErrConnClosed is excluded without making a concurrent Close a blanket exclusion.
* [The W-bit inflight gauge](/hsms/inflight-gauge.md) - when a message counts as in flight, and why a leaked increment suppresses automatic probing when linktest suppression is enabled.
* [The I1 stale-epoch write guard](/hsms/stale-epoch-write-guard.md) - how a sender stalled across a reconnect is kept off the successor's socket.
* [The reply registry's two-legged control exemption](/hsms/reply-matching-control-exemption.md) - why registration-side isData and result-side *DataMessage are two independent gates, and the v2.0.1 shape breaking both reproduces.
* [The send-side MaxMessageSize ceiling's enforcement topology](/hsms/max-message-size-ceiling.md) - where the check runs relative to writeMu, why control frames are structurally exempt, and why async accounting has no exclusion list at all.
* [The transaction observer's two chokepoints, its isData gate, and its outcome classifier](/hsms/transaction-observer-chokepoints.md) - why WithTransactionObserver instruments two call sites (not one), why the isData gate prevents an observer-path panic for control messages, and how classifyTxOutcome relates to isCountedSendErr.
* [Where a TransitionCause is chosen, and why one transition can swallow another's cause](/hsms/transition-cause-injection-sites.md) - the full injection-site to cause map, why the transports pass a cause through a capability interface, and the four ways a cause never reaches a subscriber.
* [How a queued report from an ended generation is kept off its successor](/hsms/generation-report-fence.md) - the epoch identity, the report-side and queue-side checks, the report-time binding of unnamed reports, and how the generation capability reaches hsms.
* [How the three synchronous commits are fenced against an ended generation](/hsms/synchronous-commit-gate.md) - why TCP-up, Select, and Select-lost commit under a lock, the gate rule and the validated id their reports carry, and the refusal each producer reports.
* [How the recv path's responses and requests are bound to a generation on the wire](/hsms/generation-bound-wire-sends.md) - why a straggler's response or request must not reach the successor's socket or registry, how the bound send resolves its epoch, and what the counters on that path count.
* [How a shutdown joins the per-Open supervisor](/hsms/supervisor-join.md) - why the FSM join is unbounded but the notifier join ends at the close timeout, and why the join signals live on the supervisor.
* [Reconnect backoff scope — what resets it, and what doesn't](/hsms/reconnect-backoff-scope.md) - where the persisted reconnect delay lives, the marker that resets it, and why that marker is not the reaction react fires.
* [Who owns the reconnect retry after a failed Start](/hsms/reconnect-retry-ownership.md) - how a Start failure after TCP-up hands the retry to the drop reaction's loop, the startReturned barrier that keeps a second loop out, and the gauge ordering.
* [The inbound generation fence](/hsms/inbound-generation-fence.md) - how an inbound frame/reply is bound to the generation whose recv goroutine read it, the two different kinds of cutoff that enforce it, and the SECS-I asymmetry.
* [How a user callback's panic or Goexit is contained](/hsms/handler-panic-goexit-isolation.md) - the two-frame detection runCallback needs, which goroutine each of the five callback sites runs on, and what this isolation does not cover.
* [How Close interrupts a blocked Open](/hsms/open-close-abort.md) - the pending-close registration and abort token that unblock Open's Selected wait or first dial, and how the interrupted Start failure maps to Open's error.

# Entry points

- frame decode: `hsms/` → `DecodeHSMSMessage`
- construction: `hsms/` → `NewDataMessage`, `NewSelectReq`, `NewLinktestReq`, `DataMessage.Derive`
- consumer surface: `hsms/` → `Connection`, `SECS2Endpoint`
- engine knobs: `hsms/connection_config.go` → `ConnOption` constructors

# Read first

`hsms/doc.go` is exceptionally thorough — message model, the two error channels, immutability and fan-out, and a "Dissolved v1 landmines" section explaining why four v1 hazards are now structurally impossible. Read it before any entry here, and do not write an entry that restates it.
