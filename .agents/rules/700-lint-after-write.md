---
trigger: always_on
glob: "**/*.go"
description: Run linter after modifying Go files
---

# Lint After Write

After modifying any `.go` file:

1. **Modernize:** `go fix ./...` to keep the codebase on current stdlib idioms
   (range-over-int, `min`/`max` builtins, `strings.Builder`, `t.Context()`,
   `slices.Contains`, …). Review the diff — every change must be
   behavior-preserving.
2. **Run:** `make lint`
3. **Fix:** All reported issues before committing.
4. **Re-run:** Until clean.

> **Note:** `go fix` modernizers can introduce new lint findings. For example,
> rewriting `for i := 0; i < N; i++` to `for range N` makes the literal bound
> explicit, so `prealloc` then flags slices appended in the loop (use
> `make([]T, 0, N)`); and `strings.Builder` rewrites can trigger staticcheck
> `QF1012` (prefer `fmt.Fprint(&b, …)` over `b.WriteString(fmt.Sprint(…))`).
> Always run `make lint` after `go fix`.

## Common Fixes
| Lint Error | Fix |
|------------|-----|
| `goimports` | Run `goimports -w file.go` |
| `errcheck` | Handle or explicitly ignore with `_ =` |
| `unused` | Remove dead code |
| `govet` | Fix type/format mismatches |
