# 200 — Coding Standards

## Go style

- Follow Effective Go. Format with `goimports`.
- Use `any`, not `interface{}`.
- Use `slices` / `maps` from stdlib.
- Use `context.Context` for cancellation / request scope.
- Prefer `sync/atomic` for simple counters and flags.

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
