---
type: Unit
title: gem
description: SEMI E30 (GEM) role-message builders returning transport-agnostic SECS-II messages.
---

# Responsibility

Pure value builders for GEM role messages. Every builder returns a `secs2.SECS2Message` sendable over any established connection via `SendSECS2Message`. Equipment-defined identifiers are taken as `secs2.Item` so callers choose the SECS-II type.

# Boundary

No transport, no session state, no I/O — builders are pure. Stream/function pairs not covered here are built with `secs2.NewMessage` directly.

# Entries

None yet — this unit fills on miss via `/memex capture`.

# Entry points

- base builder: `secs2/` → `NewMessage`
- generated builders: `gem/` (see `tools/gemgen`, which generates much of this package)

# Read first

`gem/doc.go` for the contract. Much of this package is generated — `docs/specs/2026-07-07-gem-codegen-design.md` and `docs/plans/gem-codegen/` describe the generator, whose source lives in the excluded `tools/gemgen` unit.
