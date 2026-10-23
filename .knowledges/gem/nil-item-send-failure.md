---
type: Mechanic
title: How a nil item becomes a send failure
description: Why a nil secs2.Item would otherwise shorten a gem body silently, where the generated code replaces it with an error item, and how that error travels up to the send.
tags: [gem, secs2, hsms, builders, errors, gemgen]
status: draft
generated: {by: "claude/opus-5.5", at: 2026-10-23T09:00:00Z}
sources:
  - {resource: gem/decode.go, digest: sha256:2ba173a8cbb55619, revision: da56326}
  - {resource: tools/gemgen/params.go, digest: sha256:6681c5ec78d656cf, revision: da56326}
  - {resource: tools/gemgen/gen_groups.go, digest: sha256:ade3e2cce35af25d, revision: da56326}
  - {resource: secs2/list.go, digest: sha256:efff5c617ecb5e4f, revision: da56326}
  - {resource: secs2/item.go, digest: sha256:6b4559e64be0d9a0, revision: da56326}
  - {resource: hsms/data_msg.go, digest: sha256:a90443b564284b51, revision: da56326}
---

# What it does

`gem/doc.go` states the contract:
a nil item, typed nil included, makes the message carry an error wrapping `ErrNilItem` that names where,
and `hsms.NewDataMessage` rejects it.
The `secs2.NewErrorItem` godoc states that the error surfaces from every enclosing list.
Neither says why the check has to live in the generated builders,
which values the generator wraps and which it leaves alone,
or why `NewErrorItem` bypasses `NewItemError`.
That is the delta this entry records.

# How it works

**Two silent sinks.**
A nil item is lost in two different places without the check.
Inside a list, `secs2.NewListItem`, which `secs2.L` calls, skips a nil child,
so the list goes out one element short.
As a whole body, `hsms.NewDataMessage` treats a nil or typed-nil item as an empty body,
so the message goes out header-only.
Neither is an error at those layers, which is why the check sits in `gem`, where the item's place is still known.

**The generator decides from the emitted type.**
`nodeExpr` and `elemItems` in `tools/gemgen/params.go` wrap a value only when the Go type it is emitted as is `secs2.Item`
(`itemType`) or a slice of them:
a single item goes through `present(ref, where)`, a slice through `presentEach(ref, where, at...)`.
Fixed leaves, optional pointers, group references, and packed groups are emitted as other types and are not wrapped.
`where` is the message code, or in `groupItemExpr` the group name, followed by the node's decode label from `nodeNames`,
so a send error and a decode error name the same field, as in `ProcessJob.material`.
A repeat of repeats passes each enclosing element's index through `indexedItems`,
so a nested nil reads `S1F20 OBJECTS[1][1]`.
Hand-written code such as `Abort`, and short forms that take no items, call neither helper.

**The runtime helpers.**
`isNilLike` treats an untyped nil, and a typed nil of any nil-capable kind found through reflection, as nil.
`present` returns the item unchanged, or `nilItem(where)`.
`presentEach` is copy-on-write:
it returns the caller's slice itself, without allocating, when nothing is nil,
and otherwise clones it once and replaces each nil element, never modifying the caller's slice.
`nilItem` appends `[i]` to where for each index it is given,
and returns `secs2.NewErrorItem` of an error that wraps `ErrNilItem` and names that place.

**How the error reaches the send.**
`NewErrorItem` returns a childless `*ListItem` whose `itemErr` holds the supplied error directly.
It skips `NewItemError`, which reuses the inner error of a direct `*ItemError`,
so whatever the caller passed keeps its identity.
Its `clean` flag stays false, so a parent list built over it by `NewListItem` is not clean either,
and the parent's `Error()` takes its full walk, joining each child's error with `errors.Join`.
At the send, `NewDataMessage` calls `item.Error()` and returns it,
so `errors.Is(err, gem.ErrNilItem)` holds on what `SendSECS2Message` returns.

# Invariants

- Every generated builder parameter and group field whose emitted type is `secs2.Item` or `[]secs2.Item` goes through `present` or `presentEach`.
- `presentEach` never writes to the caller's slice, and returns that same slice when no element is nil.
- An error item is never clean, so no enclosing list can take the clean fast path past it.
- `NewItemError` flattens only a direct `*ItemError`; a wrapped or joined one is stored whole.

# Failure modes

- **A hand-written builder that passes a caller's `secs2.Item` straight to `secs2.L`** reintroduces the silent shortening;
  only generated code gets the wrap.
- **A generator change that emits a new type for a node** without updating `itemType` or `elemType`
  either drops the wrap or wraps a value that cannot be nil;
  the gemgen tests on `BodyExpr` and group item expressions pin which expressions are wrapped.
- **Building the error item through `NewItemError`** would flatten a supplied `*ItemError` to its inner error and lose its identity.

# Where to look

- the helpers: `gem/decode.go` → `isNilLike`, `nilItem`, `present`, `presentEach`, `indexedItems`
- what the generator wraps, and the error's field name: `tools/gemgen/params.go` → `nodeExpr`, `elemItems`, `itemType`, `elemType`, `nodeNames`;
  `tools/gemgen/gen_groups.go` → `groupItemExpr`
- the silent skip and the error item: `secs2/list.go` → `NewListItem`, `NewErrorItem`, `childClean`
- the narrowed flattening: `secs2/item.go` → `NewItemError`
- the send-side rejection and the nil-body path: `hsms/data_msg.go` → `NewDataMessage`
