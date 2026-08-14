---
type: Mechanic
title: Parser input bounds
description: How the SML parser bounds allocation and recursion on attacker-controlled input.
tags: [sml, parser, security, allocation, recursion]
status: draft
generated: {by: "openai/gpt-5.6-sol", at: 2026-08-14T08:11:01Z}
sources:
  - {resource: sml/parser.go, digest: sha256:6a166a179414e02e, revision: f56c67a}
  - {resource: sml/parser_depth_test.go, digest: sha256:3230469c3aa4f0fe, revision: f56c67a}
  - {resource: secs2/decode.go, digest: sha256:8ca1e530a8d03c4a, revision: f56c67a}
  - {resource: sml/parser_dos_test.go, digest: sha256:3465ac02e29b67c0, revision: 6cf2b49}
---

# What it does
An SML item may carry a declared element count in a size token (`[n]` or `[n..m]`).
The parser used that count directly to size the buffer it preallocated.
`sml/doc.go` documents the public contract but says nothing about this.
The count is read off attacker-controlled text and bounded only by `math.MaxInt32`.
A 29-byte message like `<U1[2000000000] 1 2 3>` therefore drove a multi-gigabyte allocation before any payload byte was inspected.
The parser now caps that preallocation hint at 64 elements,
and bounds list nesting by the same constant the wire decoder uses.

# How it works
`parseItemSize` / `nextItemSize` parse the size token, bounding it only by `math.MaxInt32`.
`parseItem` keeps the maximum and passes it to each body parser as a hint.
The six slice-building parsers preallocate through `capHint(size, len(p.data))` rather than `make([]T, 0, size)`,
and `parseASCIIStrict` grows its `strings.Builder` through the same helper.
`p.data` is the unconsumed remainder of the input.
Every element the parser can still produce consumes at least one input byte,
so `len(p.data)` is a sound local upper bound.
`capHint` returns `min(size, len(p.data), 64)`,
preventing unrelated trailing input from amplifying the initial allocation.
The clamp caps only initial capacity.
`append` and `Builder` growth still reach the true size,
so a message whose declared count matches its payload is unaffected.

Note the size token is a *hint only*, not a contract the parser enforces.
`parseItem` discards the minimum,
and no body parser reconciles the declared count against the number of values actually parsed.
That is why the hint must be bounded independently rather than relying on validation of the token.

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
- The parser's nesting ceiling is `secs2.MaxListDepth`, the same constant the wire decoder enforces.
  Letting them drift would let the parser accept messages that cannot be decoded from their own wire form.

# Failure modes
- A path that sizes an allocation from the raw token lets one small untrusted message OOM-kill the process.
  Rule `600-perf-sec.md` states it directly: never allocate from a raw attacker-controlled length field.
  Strict-mode ASCII was exactly this, reaching ~1.9 GB from a 25-byte input until its `Builder.Grow` was clamped too.
- `TestParse_HugeDeclaredSizeDoesNotPrealloc` guards all seven paths,
  asserting that a huge declared count keeps allocation bounded.
- `TestParse_PaddingDoesNotAmplifyPreallocation` guards against a large suffix inflating the remaining-input bound,
  asserting that list, numeric, and ASCII inputs stay below the allocation ceiling.
- `TestParseStrict_LongNumericTokenDoesNotCopyQuadratically` guards the numeric-token path,
  where the cost was quadratic in the token length rather than driven by the size token.
- `TestParse_ListDepthMatchesWireDecoder` pins the nesting ceiling to the decoder's,
  covering the boundary, sibling unwinding, and that deep input errors rather than crashing.

# Gotchas
- Recursion depth is bounded separately from the size token, by `secs2.MaxListDepth`.
  `parseList` recurses per child, so nesting in the text maps onto stack depth;
  before the bound, deeply nested input aborted the process with a stack overflow,
  which is fatal in Go and cannot be recovered by a caller.
  The parser deliberately shares the decoder's constant rather than picking its own,
  so it cannot accept a message that its own wire decoder would reject.

# Where to look
- clamp helper: `sml/parser.go` → `capHint`
- size token source: `sml/parser.go` → `parseItemSize`, `nextItemSize`
- clamped slice parsers: `sml/parser.go` → `parseList`, `parseBoolean`, `parseBinary`, `parseFloat`, `parseInt`, `parseUint`
- clamped builder and sliced numeric token: `sml/parser.go` → `parseASCIIStrict`
- nesting bound: `sml/parser.go` → `parseList`; the shared constant is `secs2/decode.go` → `MaxListDepth`
- regression guards: `sml/parser_dos_test.go` → `TestParse_HugeDeclaredSizeDoesNotPrealloc`, `TestParse_PaddingDoesNotAmplifyPreallocation`, `TestParseStrict_LongNumericTokenDoesNotCopyQuadratically`
- nesting guard: `sml/parser_depth_test.go` → `TestParse_ListDepthMatchesWireDecoder`
