---
type: Unit
title: hsmsss
description: HSMS-SS (SEMI E37.1) transport — TCP role, control procedures, linktest, reconnect.
---

# Responsibility

The concrete HSMS-SS transport over the shared `hsms` engine: dial (active) or listen (passive), the framed reader/writer, the E37.1 Select/Deselect/Linktest/Separate procedures, per-generation activity stamps, and the HSMS-SS control-plane metrics.

# Boundary

Owns no message types, reply routing, or T3 — those are `hsms`. Single-session by design: there is no `AddSession`, the `Connection` *is* its own SECS-II endpoint. No pooling or `Free` API exists in v2.

# Entries

* [Activity stamps — the state behind linktest suppression](/hsmsss/activity-stamps.md) - where "the line is alive" is stored, what writes it, when it resets.
* [Where the HSMS-SS profile overrides the generic core](/hsmsss/e37-1-narrows-e37-generic.md) - the four sites whose value or state check comes from E37.1, not E37, and what silently breaks if one is "simplified" back.
* [The passive refusal exchange's absolute deadline and two-sided Stop handoff](/hsmsss/passive-refusal-exchange.md) - why refuseExtraConn cannot reuse readFrame, and why the Stop handoff needs a token, not a bare field.

# Entry points

- construction: `hsmsss/` → `NewConfig`, `New`, `WithActive`, `WithPassive`, `WithConnectionOption`
- lifecycle: `hsmsss/` → `Connection.Open`, `Connection.Close`, `Connection.UpdateConfigOptions`
- control metrics: `hsmsss/metrics.go` → `ConnectionMetrics`
- transport internals: `hsmsss/transport.go` → `transport`; procedures in `hsmsss/transport_procedures.go`

# Read first

`hsmsss/doc.go` for the public contract and the inherited dissolved-landmine guarantees. `docs/guides/linktest-suppression.md` covers the feature at guide level; the entry above covers the mechanics beneath it.
