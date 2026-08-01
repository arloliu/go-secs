---
name: doc-sync
description: Syncs go-secs docs against the current exported API — corrects verifiable drift (wrong signatures, renamed options, phantom symbols, stale behavior), and on request reflows exported Godoc to semantic linefeeds. Use when the user wants docs checked against source, reports doc drift, or asks to reflow Godoc line breaking.
---

# doc-sync

Two modes, picked from the invocation.
**drift** is the default when the user does not say.

- **drift** — docs reflect current source.
  Correct factual drift, and leave wording and line breaking exactly as they are.
  The rest of this file covers it.
- **reflow** — rewrite exported Godoc to semantic linefeeds, wording untouched.
  Runs only when asked.
  Follow [REFLOW.md](REFLOW.md) instead of the procedure below.

Default scope: `README.md`, `sml/README.md`, and `doc.go` of each public package listed in `100-overview.md`. Narrow with an argument. `docs/secs1/*.md` design notes are read-only history — flag only if they misrepresent *current* behavior.

## Procedure (drift)

1. **Inventory source.** For each package in scope, list exported symbols, functional options, and the package-level comment. Do not derive public-API facts from `internal/`.
2. **Inventory docs.** Extract every code block, signature, option name, shortcut constructor (`A`, `I4`, `L`, …), and SEMI-standard reference in the scoped files.
3. **Diff.** For each mismatch, classify:
   - **stale** — exists but described differently (wrong signature, option, default, behavior).
   - **missing** — source symbol / option / error / behavior not mentioned.
   - **phantom** — doc describes something that no longer exists.
4. **Fix.** Targeted edits only. Match signatures verbatim (parameter names, types, variadic). Correct option names everywhere. For phantom symbols, delete the section (and add a one-line "renamed to X" where a replacement exists). For missing symbols, add a stub entry per `400-documentation.md`.
5. **Don't fix** when it requires author intent (ambiguous grammar, design-note decisions, multiple plausible rewordings) — flag as "needs attention."

## Rules (drift)

- Read the source file before editing a doc claim.
- Verify SML snippets parse under the current parser mode (strict / non-strict).
- Don't touch prose tone, section order, or design notes.
- Don't fabricate API behavior. If source is ambiguous, quote both the claim and the source.

## Report

```
## doc-sync report

### Applied
- README.md: corrected SendDataMessage signature; added WithValidateDataMessage to the secs1 example.
- hsmsss/doc.go: documented exponential-backoff reconnect; added WithLinktestFailureThreshold.

### Needs attention
- README.md L300: SML round-trip example ambiguous between strict / non-strict — author input needed.

### No changes
- gem/doc.go, logger/doc.go
```
