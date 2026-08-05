---
type: Bundle Conventions
title: go-secs memex conventions
unit: go-package
unit_globs: ["*/"]
exclude: [internal, tools, benchmarks, integration, docs, tmp, .worktrees, .agents, .claude, .github, .vscode, .codex, .superpowers, .antigravitycli, .knowledges]
scope: mechanics
pointer_style: symbol
tracked: true
hook: .agents/rules/150-memex.md
propose_threshold_loc: 150
declined: []
---

# Units

The seven public packages named in `.agents/rules/100-overview.md`: `gem`, `hsms`, `hsmsss`, `logger`, `secs1`, `secs2`, `sml`.

`unit_globs` is `*/` rather than an explicit list so a **new** public package is proposed by the coverage sweep instead of staying invisible. Everything not a public package is excluded: `internal/` (private by prime directive 3), `tools/gemgen` (generator, not library surface), `benchmarks/`, `integration/`, and the `*test` helper subpackages (`hsms/hsmstest`, `logger/loggertest`), which sit below the glob's depth anyway.

# What earns an entry

This repo's `doc.go` files are unusually thorough — they already document the public contract, the immutability model, the error channels, and the dissolved v1 landmines. An entry that restates a `doc.go` section is pure duplication and must not be written.

An entry earns its place when it records mechanics **below** the public contract:

- How a procedure is actually sequenced across goroutines, channels, and generations.
- Which invariant the implementation relies on that no exported signature expresses.
- What the failure looks like from the outside when that invariant breaks.
- Where the mechanic lives, by symbol.

# What does not

- Signatures, options, and constructors — `doc.go` owns these, and `/doc-sync` already guards them against drift.
- Design rationale and rejected alternatives — `docs/v2/`, `docs/specs/`, and `docs/plans/` own these.
- SEMI standard requirements as such — cite the clause, don't restate the standard.
- Anything derivable in one grep.
