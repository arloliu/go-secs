# 200 — Coding Standards

## Go style

- Follow Effective Go. Format with `goimports`.
- Use `any`, not `interface{}`.
- Use `slices` / `maps` from stdlib.
- Use `context.Context` for cancellation / request scope.
- Prefer `sync/atomic` for simple counters and flags.

## Comments

- Prefer `//` line comments.
  Reserve block comments (`/* */`) for generated headers or long usage text.
- A comment states the invariant, the *why*, or a non-obvious constraint — not what the code already says.
- Break comment lines by meaning, not by column.
  Default to one sentence per line, and break long sentences at clause boundaries.
  No linter checks line length here, so there is nothing to wrap defensively against.
  See [400-documentation.md](400-documentation.md#line-breaking-semantic-linefeeds) for the full rule.

## Errors

- Static: `errors.New`.
- Wrap: `fmt.Errorf("context: %w", err)`.
- Check: `errors.Is` / `errors.As`.
- Naming: sentinel `ErrX`, typed `XError`.
- Type assert with comma-ok: `v, ok := x.(T)`.
- Errors are the last return value. Use early returns.

## Interface assertions

`var _ Interface = (*Type)(nil)` immediately after the type definition.

## File layout (enforced by `decorder`)

1. Package
2. Imports (stdlib, external, internal)
3. Constants (exported first)
4. Variables (exported first)
5. Types (exported first) + interface assertions
6. Factory functions
7. Exported functions
8. Unexported functions
9. Exported methods (grouped by receiver)
10. Unexported methods (grouped by receiver)

## Function limits

- Max lines: 100 (prefer < 50).
- Max cyclomatic complexity: 22; package-average ≤ 15.
- Naked returns only in functions ≤ 40 lines.

## Naming

- Packages: short, lowercase.
- Exported: CamelCase. Unexported: camelCase.
- Receivers: short and consistent with the file (e.g., `c` for `*Connection`, `s` for `*Session`, `b` for `*Block`).

## Loops (Go 1.22+)

- Index needed: `for i := range slice`
- No index: `for range slice`
- Count: `for range N`
- Benchmarks: `for b.Loop()`
