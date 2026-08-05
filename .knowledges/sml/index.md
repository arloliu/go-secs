---
type: Unit
title: sml
description: SML (SECS Message Language) parsing and encoding, strict and non-strict.
---

# Responsibility

Parses SML text into HSMS data messages and renders `secs2.Item` values back to canonical SML. Owns the scanner, the strict-mode `0xHH` token handling on both directions, and `*ParseError` with byte offset, line, and column.

# Boundary

Does not own the item model (`secs2`) or message construction rules (`hsms`). Parser mode is per-`Parser`; the package-level ASCII/quote mode setters named in `100-overview.md` are startup configuration, not per-message knobs.

# Entries

None yet — this unit fills on miss via `/memex capture`.

# Entry points

- zero-config parse: `sml/` → `Parse`, `ParseStrict`
- reusable parser: `sml/` → `NewParser`, `Parser.ParseMessage`
- encoding: `sml/` → `Encode`, `NewEncoder`, `Encoder.EncodeMessage`

# Read first

`sml/doc.go` covers the public contract, including the concurrency split that matters most here: `*Parser` holds mutable scan state and is not safe for concurrent use, while `*Encoder` is immutable after construction and is.
