---
type: Mechanic
title: How a nil item becomes a send failure
description: Why a nil secs2.Item would otherwise shorten a gem body silently, where the generated code replaces it with an error item, and how that error travels up to the send.
tags: [gem, secs2, hsms, builders, errors, gemgen]
status: stable
generated: {by: "claude/opus-5.5", at: 2026-10-23T18:00:00Z}
verified:
  - {by: "openai/gpt-6-astra", at: 2026-10-23T18:42:42Z}
sources:
  - {resource: gem/decode.go, digest: sha256:2ba173a8cbb55619, revision: 2a92a5e}
  - {resource: tools/gemgen/params.go, digest: sha256:6681c5ec78d656cf, revision: 2a92a5e}
  - {resource: tools/gemgen/gen_groups.go, digest: sha256:ade3e2cce35af25d, revision: 2a92a5e}
  - {resource: secs2/list.go, digest: sha256:efff5c617ecb5e4f, revision: 2a92a5e}
  - {resource: secs2/item.go, digest: sha256:6b4559e64be0d9a0, revision: 2a92a5e}
  - {resource: hsms/data_msg.go, digest: sha256:a90443b564284b51, revision: 2a92a5e}
  - {resource: hsms/session.go, digest: sha256:04cc770917f02f59, revision: 2a92a5e}
  - {resource: gem/doc.go, digest: sha256:1a98f80e93867d15, revision: 2a92a5e}
  - {resource: gem/abort.go, digest: sha256:3b2e4453c0a02420, revision: 2a92a5e}
  - {resource: gem/groups.go, digest: sha256:87d916e032f6b263, revision: 2a92a5e}
  - {resource: gem/s1.go, digest: sha256:80d126b2f06c50ca, revision: 2a92a5e}
  - {resource: secs2/shortcut.go, digest: sha256:76af285cd5ebab5b, revision: 2a92a5e}
  - {resource: tools/gemgen/nil_item_test.go, digest: sha256:e6669cda2b123ccd, revision: 2a92a5e}
  - {resource: secs2/message.go, digest: sha256:1c99b28a5c5d991f, revision: 2a92a5e}
  - {resource: tools/gemgen/gen_messages.go, digest: sha256:7797b472f54f7465, revision: 2a92a5e}
  - {resource: tools/gemgen/templates/messages.go.tmpl, digest: sha256:f12a59c1b5e2e135, revision: 2a92a5e}
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
Inside a list, `secs2.NewListItem`, which `secs2.L` calls, skips a nil interface child,
so the list goes out one element short.
A typed-nil child is kept, and the list's `Error()` reports it.
As a whole body, a nil or typed-nil item becomes an empty body,
first in `secs2.NewMessage`, which every generated builder calls, and again in `hsms.NewDataMessage`,
so the message goes out header-only.
`Message.Item` returns an error item placed there unchanged, so it reaches the send.
Neither is an error at those layers, which is why the check sits in `gem`, where the item's place is still known.

**The generator decides from the emitted type.**
`nodeExpr` and `elemItems` in `tools/gemgen/params.go` wrap a value only when the Go type it is emitted as is `secs2.Item`
(`itemType`) or a slice of them:
a single item goes through `present(ref, where)`, a slice through `presentEach(ref, where, at...)`.
Fixed leaves outside repeats and fixed scalar repeat elements are emitted as concrete values and are not wrapped,
nor are optional pointers, group references, and packed groups.
A fixed byte-array repeat element is emitted as `secs2.Item` and goes through `presentEach`.
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
`NewErrorItem` returns a childless `*ListItem` whose `itemErr` is a new `*ItemError` wrapping the supplied error.
It skips `NewItemError`, which reuses the inner error of a direct `*ItemError`,
so whatever the caller passed keeps its identity inside that wrapper.
Its `clean` flag stays false, so a parent list built over it by `NewListItem` is not clean either,
and the parent's `Error()` takes its full walk, joining each child's error with `errors.Join`.
At the send, `NewDataMessage` calls `item.Error()` and returns it.
`SendSECS2Message` checks a nil message, an even function, and the stream first,
so for a primary that passes those checks `errors.Is(err, gem.ErrNilItem)` holds on what it returns.

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
  `TestBodyExprWrapsEveryItem` and `TestGroupItemExprWrapsEveryItem` pin which expressions are wrapped.
- **Building the error item through `NewItemError`** would flatten a supplied `*ItemError` to its inner error and lose its identity.

# Where to look

- the helpers: `gem/decode.go` → `isNilLike`, `nilItem`, `present`, `presentEach`, `indexedItems`
- what the generator wraps, and the error's field name: `tools/gemgen/params.go` → `nodeExpr`, `elemItems`, `itemType`, `elemType`, `nodeNames`;
  `tools/gemgen/gen_groups.go` → `groupItemExpr`
- the silent skip and the error item: `secs2/list.go` → `NewListItem`, `NewErrorItem`, `childClean`
- the narrowed flattening: `secs2/item.go` → `NewItemError`
- the send-side rejection and the nil-body path: `hsms/data_msg.go` → `NewDataMessage`
- the checks ahead of it: `hsms/session.go` → `(*session).SendSECS2Message`
- the sentinel and its contract: `gem/decode.go` → `ErrNilItem`; package guidance: `gem/doc.go`
- the list shortcut: `secs2/shortcut.go` → `L`
- hand-written code without the wrap: `gem/abort.go` → `Abort`
- generated examples: `gem/groups.go` → `ProcessJob`; `gem/s1.go` → `S1F20`
- the wrap tests: `tools/gemgen/nil_item_test.go` → `TestBodyExprWrapsEveryItem`, `TestGroupItemExprWrapsEveryItem`
- whole-body nil normalization: `secs2/message.go` → `NewMessage`, `(*Message).Item`
- generated builders call `NewMessage`: `tools/gemgen/gen_messages.go` → `renderMessages`; `tools/gemgen/templates/messages.go.tmpl` → `NewMessage`
