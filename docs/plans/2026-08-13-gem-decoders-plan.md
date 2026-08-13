# Implementation plan: generated GEM reply decoders (v2.4)

Extend `tools/gemgen` to emit the decode half of every message it already builds.
The DSL already encodes each message's shape for the builders;
decoders are the symmetric output, not a new subsystem.

## API shape

```go
// One decoder per generated message, taking the transport-agnostic item.
// gem MUST NOT import hsms (hsms imports gem — a cycle); decoders accept secs2.Item.
func DecodeS1F14(item secs2.Item) (S1F14Reply, error)

type S1F14Reply struct {
    CommAck byte          // enumerated fields use the E5 variable name
    Model   string
    SoftRev string
}
```

Result types are named `<SxFy>Reply` (secondary) / `<SxFy>Body` (primary decode, where useful),
which avoids colliding with the builder function names.

## Scope

- Generate decoders for the streams gemgen already covers (S1, S2, S5, S6, S9),
  for every message whose body shape the DSL defines.
- Fields whose SECS-II type is equipment-defined (the DSL's opaque kind) stay `secs2.Item` in the result struct —
  the decoder does not guess.
- Messages with no defined body (header-only) get no decoder.
- Follow the existing gemgen conventions for provenance comments and legacy exclusions
  (read `tools/gemgen` and the gem-codegen design doc,
  `docs/plans/2026-07-07-gem-codegen-design.md`, before changing the generator).

## Decode semantics

1. **Strict shape**: list arity and item types must match the DSL shape;
   a mismatch returns an error naming the E5 field and the got/want types —
   never a zero value with nil error.
2. **Variable-arity sections** (e.g. report lists) decode into slices;
   the DSL already distinguishes fixed and repeated groups for the builders.
3. Generated bodies should be straight-line code using `secs2.Cursor`
   (sequenced after the cursor plan) — no reflection, per `600-perf-sec.md`.
4. Enumerated single-byte fields (ACKC5, COMMACK, …) decode to their existing generated constant types
   where gemgen already defines them.

## Round-trip test generation

For every builder/decoder pair, generate a table-driven round-trip test:
build with representative values → decode → require equality.
This keeps the two halves from drifting and costs nothing to maintain —
the generator owns it.
Hand-write (not generate) the negative cases:
wrong arity, wrong type at each position, trailing items, empty body.

## Steps

1. Read `tools/gemgen` (DSL definitions, emitter, existing conventions) and
   `docs/plans/2026-07-07-gem-codegen-design.md`.
2. Add decoder emission to the generator; regenerate `gem/`.
3. Add round-trip test emission; hand-write negative-case tests for a representative message per shape class.
4. Update `gem/doc.go`: the package now provides builders *and* decoders;
   document the strict-shape error contract.
5. Lint, CHANGELOG (`### Added`).

## Risks / tensions

- **Generator changes fan out**: a bug in the emitter touches every generated file.
  Mitigation:
  the generated round-trip tests, plus reviewing the regenerated diff for one message per shape class rather than eyeballing all of it.
- **Shape strictness vs. real equipment**: some equipment pads or extends bodies.
  The decoder is strict by design;
  the escape hatch is the existing manual path (`msg.Item()` + cursor), not a lenient mode.
  Revisit only with a concrete field report.

## Acceptance

- Every generated builder has a matching decoder and passing round-trip test.
- `make test-all` green, lint clean.
- `gem/doc.go` documents the decode contract; pkg.go.dev shows a decode example.
