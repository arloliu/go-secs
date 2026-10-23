---
type: Unit
title: gem
description: SEMI E30 (GEM) role-message builders and the matching strict-shape body decoders, both transport-agnostic.
---

# Responsibility

Pure value builders for GEM role messages, plus a decoder per builder that reads a received body back into a result struct.
Every builder returns a `secs2.SECS2Message` sendable over any established connection via `SendSECS2Message`;
every decoder takes the body as a `secs2.Item`, never a message, so the package stays free of transport imports.
Equipment-defined identifiers are taken as `secs2.Item` so callers choose the SECS-II type.
Repeated lists are named group structs shared across messages, decoders take options such as `AllowTrailing`,
and a catalog (`Lookup`, `AcceptReply`) describes, decodes, and accepts any message by stream and function.

# Boundary

No transport, no session state, no I/O — builders are pure. Stream/function pairs not covered here are built with `secs2.NewMessage` directly.

# Entries

* [How a nil item becomes a send failure](/gem/nil-item-send-failure.md) - the two places a nil item vanished silently, which generated values are wrapped, and how the error reaches the send.

# Entry points

- base builder: `secs2/` → `NewMessage`
- generated builders: `gem/` (see `tools/gemgen`, which generates much of this package)
- groups: `gem/groups.go`; decode options: `gem/decode.go`; catalog: `gem/catalog.go` (all generated)
- the hand-written SxF0 builder: `gem/abort.go` → `Abort`

# Read first

`gem/doc.go` for the contract. Much of this package is generated — `docs/specs/2026-07-07-gem-codegen-design.md` and `docs/plans/gem-codegen/` describe the generator, whose source lives in the excluded `tools/gemgen` unit.
