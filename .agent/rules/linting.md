# Linting & Formatting Rules

## Tooling
- **Linter**: `golangci-lint` v1.62.0 (install/update via `make update-tools`).
- **Config**: `.golangci.yaml` at project root — all linter settings and enabled linters are defined there.
- **Run**: `make lint` (linting only) or `make check` (lint + `go vet`).
- Always run `make check` before committing.

## Key Linter Settings

| Linter | Rule |
|--------|------|
| `errcheck` | Do not ignore errors. Intentional drops must use `_ = fn()` only when truly safe (e.g., `SetKeepAlive`). Type assertion errors are checked (`check-type-assertions: true`); `require.Equal`/`assert.Equal` are excluded. |
| `gocyclo` | Max complexity: 30. |
| `cyclop` | Max function complexity: 22, max package average: 15. |
| `nlreturn` | Blank line before `return`/branch unless the block is ≤ 3 lines. |
| `revive` | All rules enabled except: `add-constant`, `cyclomatic`, `cognitive-complexity`, `flag-parameter`, `line-length-limit`, `max-public-structs`, `unused-parameter`, `unused-receiver`, `var-declaration`. Function length limit: 100 lines (excluded from tests). |
| `nakedret` | No naked returns in functions > 40 lines. |
| `gosec` | Security checks enabled (excluded from test files). |
| `nolintlint` | `//nolint` directives must specify the linter name (e.g., `//nolint:errcheck`). |
| `govet` | All analyzers enabled except `fieldalignment` and `shadow`. |
| `errname` | Sentinel errors prefixed with `Err`, error types suffixed with `Error`. |

## Formatting
- `goimports` is the canonical formatter (fixes imports + gofmt-style formatting).
- No manual import grouping needed — `goimports` handles it.

## Suppressing Linters
- Use `//nolint:<linter>` with the specific linter name.
- Explanation after the directive is recommended but not required (`require-explanation: false`).
- Test files (`_test.go`) are excluded from: `bodyclose`, `dupl`, `goconst`, `gosec`, `noctx`, `wrapcheck`.
