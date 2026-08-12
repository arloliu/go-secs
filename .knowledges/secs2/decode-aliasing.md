---
type: Mechanic
title: What a decoded item aliases
description: Which leaves point into the wire buffer and which copy out of it — the part the doc comment gets wrong.
tags: [secs2, decode, zero-copy, unsafe]
status: stable
generated: {by: "claude/sonnet-5", at: 2026-08-12T00:00:00Z}
verified:
  - {by: "claude/opus-5", at: 2026-08-12T08:33:19Z}
sources:
  - {resource: secs2/decode.go, digest: sha256:8ca1e530a8d03c4a, revision: 3660aa4}
  - {resource: secs2/item.go, digest: sha256:39618013ea8f34c1, revision: 3660aa4}
---

# What it does

`secs2/doc.go` covers the `Decode` (copies) versus `DecodeOwned` (transfers ownership) contract — read it there.
It still frames decoding as that pair; it never names `DecodeOwnedFrame` (the third public entry point) at all, even though it now also names `hsms.DataMessage.TrailingBytes` as the way to observe trailing bytes.
What `doc.go` does not give correctly, for the pair it does cover, is which decoded leaves actually alias the buffer.

**The doc comment is imprecise on one point.** It says numeric and boolean items "always build a typed value slice regardless". They do not: the split is by *count*, not by type.

# How it works

A single-value numeric takes the `count == 1` branch, which stores the decoded value in the item's `scalar` field and leaves `values` nil — no slice is allocated. Only multi-value numerics and booleans build a typed slice, because their representation genuinely differs from the wire bytes.

Everything else aliases the one buffer the decode was handed: ASCII, JIS-8 and localized-string leaves hold a string built by `ownedString` (an `unsafe.String` view), and binary leaves alias a sub-slice.

The third entry point, `DecodeOwnedFrame`, aliases exactly like `DecodeOwned` — same leaf-by-leaf rules above — over a buffer wrapped in a capability token whose type lives in an internal package.
External code cannot construct that token, so it cannot call the function at all — the parameter type *is* the access control.
It exists for the in-repo transport, which already owns its frame buffers.

# Invariants

- `ownedString`'s safety rests on two contracts holding at once: `Decode`'s clone is referenced by nothing outside the call tree, and `DecodeOwned`'s caller genuinely transferred ownership. The decoder never hands out a second independent reference, so the returned string is the only view of those bytes.
- Ownership transfer means "must not mutate or reuse", not "must keep a variable in scope". Leaves retain the backing array through a pointer, string, or sub-slice, so the GC keeps it alive on its own.
- `DecodeOwnedFrame` stays uncallable externally only while its parameter type remains in an internal package. Moving that type out silently makes an unchecked zero-copy path public.
- Recursion is capped at `MaxListDepth` (64) on every path.

# Failure modes

- **Reusing a buffer passed to `DecodeOwned` corrupts live items with no error.** Strings mutate under readers that assume immutability, so the damage surfaces far from the cause — a wrong SML render, or a comparison whose result changes between two reads of the same item.
- **Trusting the doc comment's "always"** leads to reasoning that every numeric leaf owns a slice; a change that relies on that (say, mutating `values` in place for scalars) would hit a nil slice or, worse, appear to work while scalars silently diverge.
- **Raising or removing `MaxListDepth`** reopens stack exhaustion on adversarial nesting.

# Where to look

- the copying and ownership-transfer entry points: `secs2/decode.go` → `Decode`, `DecodeOwned`
- the capability-gated transport path: `secs2/decode.go` → `DecodeOwnedFrame`
- the unsafe view and its two contracts: `secs2/decode.go` → `ownedString`
- the count==1 scalar branch: `secs2/decode.go` → `decodeIntItem`
- the raw-bytes retention field every leaf shares: `secs2/item.go` → `baseItem`
- recursion and the depth cap: `secs2/decode.go` → `decodeItem`, `MaxListDepth`
