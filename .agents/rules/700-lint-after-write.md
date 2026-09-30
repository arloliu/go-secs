---
trigger: always_on
glob: "**/*.go"
description: Run linter after modifying Go files
---

# Lint After Write

After modifying any `.go` file, work in the module that owns it.
Root commands never reach the nested `tracepack/` and `tools/gemgen/` modules.

1. **Modernize:** `go fix ./...` from that module's directory to keep the codebase on current stdlib idioms
   (range-over-int, `min`/`max` builtins, `strings.Builder`, `t.Context()`, `slices.Contains`, …).
   Review the diff — every change must be behavior-preserving.
2. **Run:** `make lint`, `make lint-tracepack`, or `make lint-gemgen`
3. **Fix:** All reported issues before committing.
4. **Re-run:** Until clean.

> **Note:** `go fix` modernizers can introduce new lint findings.
> For example, rewriting `for i := 0; i < N; i++` to `for range N` makes the literal bound explicit,
> so `prealloc` then flags slices appended in the loop (use `make([]T, 0, N)`);
> and `strings.Builder` rewrites can trigger staticcheck `QF1012` (prefer `fmt.Fprint(&b, …)` over `b.WriteString(fmt.Sprint(…))`).
> Always run the module's lint target after `go fix`.

Fix `goimports` / formatting findings with `make fmt`.
