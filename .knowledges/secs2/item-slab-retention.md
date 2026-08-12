---
type: Mechanic
title: Item slab carving and its retention model
description: What retaining one decoded leaf actually pins in memory, and the property that bounds it.
tags: [secs2, decode, allocation, memory]
status: draft
generated: {by: "claude/sonnet-5", at: 2026-08-12T00:00:00Z}
sources:
  - {resource: secs2/decode_slab.go, digest: sha256:77b6508240e72230, revision: 3660aa4}
  - {resource: secs2/decode.go, digest: sha256:8ca1e530a8d03c4a, revision: 3660aa4}
---

# What it does

No package doc mentions the slab. It is invisible from the API — item semantics are identical with or without it — and exists purely to cut per-leaf allocation count on the decode hot path. What it costs is a retention rule: holding one decoded leaf pins more than that leaf.

# How it works

Each decode call builds its own `decodeSlab`, a bundle of per-type slabs, and threads it by pointer through the recursion. A slab hands out `*T` from its current chunk and bumps a position; when the chunk is full, `next` allocates a **brand-new** chunk rather than growing the existing one. Chunk sizes follow 1, 4, 16, 64, then cap at 128, giving cumulative capacities 1, 5, 21, 85, 213, 341…

Retaining one carved struct therefore pins its whole chunk — at most 128 structs — plus the wire buffer the item tree already pins. It does not transitively pin a sibling's value slice or subtree.

# Invariants

- **No slabbed type may gain a pointer-bearing field referencing a separately-allocated object on the slab-carving branch.** This is the property that bounds retention. Scalar numerics and scalar booleans set only a raw pointer into the wire buffer; ASCII/JIS-8/localized-string hold an unsafe view over that same buffer; binary aliases a sub-slice of it. Add a field pointing somewhere else and one retained leaf starts pinning arbitrary memory.
- **Chunks are never grown in place.** Any rewrite of `next` that reallocates and copies an existing chunk invalidates every pointer already handed out — a use-after-free class bug in a language that otherwise prevents them.
- A slab belongs to exactly one decode call and is never shared, so it adds no concurrency surface and needs no synchronization.

# Failure modes

- **Retention amplification.** Break the pointer-free property and a single retained leaf pins whatever its new field references, turning a bounded pin (one chunk + one buffer) into an unbounded one across a whole decoded tree. Nothing fails loudly; the process just holds memory it appears to have released.
- **In-place chunk growth** would produce dangling `*T` values held by already-returned items — corruption whose blast radius is every item carved before the growth.
- **Sharing a slab across calls** to "save allocations" would introduce a data race on `pos` and hand the same struct to two decoders.

# Where to look

- carving, chunk growth, and the size schedule: `secs2/decode_slab.go` → `(*itemSlab[T]).next`, `slabChunkSizes`
- the retention model, stated at the type: `secs2/decode_slab.go` → `itemSlab`
- the per-call bundle and its accessors: `secs2/decode_slab.go` → `decodeSlab`
- where a slab is created and threaded: `secs2/decode.go` → `decodeFirstItem`, `decodeItem` — `Decode` and `DecodeOwned` reach it only by delegating to `decodeFirstItem`, which is where `slab := &decodeSlab{}` actually lives
