---
type: Mechanic
title: Parser allocation bounds
description: How the SML parser keeps attacker-controlled text from driving oversized allocations.
tags: [sml, parser, security, allocation]
status: draft
generated: {by: "claude/fable-5", at: 2026-08-12T10:45:49Z}
sources:
  - {resource: sml/parser.go, digest: sha256:2dea53bde93f34a0, revision: 007ff09}
  - {resource: sml/parser_dos_test.go, digest: sha256:51c56d2ddbd8b51d, revision: 007ff09}
---

# What it does
An SML item may carry a declared element count in a size token (`[n]` or `[n..m]`).
The parser used that count directly to size the buffer it preallocated.
`sml/doc.go` documents the public contract but says nothing about this.
The count is read off attacker-controlled text and bounded only by `math.MaxInt32`.
A 29-byte message like `<U1[2000000000] 1 2 3>` therefore drove a multi-gigabyte allocation before any payload byte was inspected.
The parser now clamps that preallocation hint.

# How it works
`parseItemSize` / `nextItemSize` parse the size token, bounding it only by `math.MaxInt32`.
`parseItem` keeps the maximum and passes it to each body parser as a hint.
The six slice-building parsers preallocate through `capHint(size, len(p.data))` rather than `make([]T, 0, size)`,
and `parseASCIIStrict` grows its `strings.Builder` through the same helper.
`p.data` is the unconsumed remainder of the input.
Every element the parser can still produce consumes at least one input byte,
so `len(p.data)` is a sound upper bound and `capHint` returns `min(size, len(p.data))`.
The clamp caps only initial capacity.
`append` and `Builder` growth still reach the true size,
so a message whose declared count matches its payload is unaffected.

Note the size token is a *hint only*, not a contract the parser enforces.
`parseItem` discards the minimum,
and no body parser reconciles the declared count against the number of values actually parsed.
That is why the bound must come from the remaining input rather than from validating the token.

The size token is not the only attacker-controlled length in this area.
`parseASCIIStrict` also reads unquoted numeric tokens,
and it slices each one out of `p.data` at its delimiter rather than accumulating it rune by rune.
Accumulating into a string was quadratic, because Go strings are immutable:
a 195 KB token copied its way to roughly 19 GB before `ParseUint` ever rejected it.

# Invariants
- Every allocation sized from a size token passes through `capHint`.
  A bare `make([]T, 0, size)` or `Grow(size)` fed by `parseItemSize` reintroduces the DoS.
- The clamp is a capacity hint only, and must never change which inputs parse or the values produced.
- Unbounded attacker text is sliced, never accumulated.
  Building a token with `+=` in a per-rune loop reintroduces quadratic copying no size clamp can catch.

# Failure modes
- A path that sizes an allocation from the raw token lets one small untrusted message OOM-kill the process.
  Rule `600-perf-sec.md` states it directly: never allocate from a raw attacker-controlled length field.
  Strict-mode ASCII was exactly this, reaching ~1.9 GB from a 25-byte input until its `Builder.Grow` was clamped too.
- `TestParse_HugeDeclaredSizeDoesNotPrealloc` guards all seven paths,
  asserting that a huge declared count keeps allocation bounded.
- `TestParseStrict_LongNumericTokenDoesNotCopyQuadratically` guards the numeric-token path,
  where the cost was quadratic in the token length rather than driven by the size token.

# Gotchas
- Recursion depth is a separate, unbounded concern.
  `parseList` recurses per child with no depth budget,
  so deeply nested input still aborts the process with a fatal stack overflow.
  `capHint` does not address it.

# Where to look
- clamp helper: `sml/parser.go` → `capHint`
- size token source: `sml/parser.go` → `parseItemSize`, `nextItemSize`
- clamped slice parsers: `sml/parser.go` → `parseList`, `parseBoolean`, `parseBinary`, `parseFloat`, `parseInt`, `parseUint`
- clamped builder and sliced numeric token: `sml/parser.go` → `parseASCIIStrict`
- regression guards: `sml/parser_dos_test.go` → `TestParse_HugeDeclaredSizeDoesNotPrealloc`, `TestParseStrict_LongNumericTokenDoesNotCopyQuadratically`
