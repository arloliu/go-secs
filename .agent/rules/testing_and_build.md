# Testing & Build Rules

## Testing

### Frameworks
- Standard `testing` package + `github.com/stretchr/testify/require` (preferred for fatal assertions) and `assert` (for soft checks).
- Mock support via `github.com/stretchr/testify/mock` (see `logger/mock.go`, `hsms/mock_*_test.go`).

### Commands
| Command | Purpose |
|---------|---------|
| `make test` | Clean cache → run all tests with `-race`, `-short`, parallel. Output in `test.log`. |
| `make build-tests` | Compile tests without running them — quick syntax/type check. |
| `make coverage` | Run tests with per-package coverage profiles under `.coverage/`. |
| `make coverage-report` | Generate HTML coverage report from combined profiles. |

### Conventions
- Test files: `*_test.go` alongside the source file they test.
- Test functions: `TestXxx` with descriptive subtests via `t.Run("description", ...)`.
- Prefer `require.New(t)` at the top of test functions to reduce boilerplate.
- Tests **must** pass cleanly with `-race` and **no deadlocks**.
- Use `net.Pipe()` for in-process TCP-like connection tests (see `hsmsss/message_reader_test.go`).
- Use `time.Sleep` sparingly in tests; prefer `require.Eventually` for async assertions.

### Integration Tests
- Located under `tests/` (e.g., `tests/hsmsss_integration/`, `tests/secs1_integration/`).
- Batch test scripts: `tests/batch_conn_test.sh`, `tests/secs1_batch_conn_test.sh`.
- Integration tests use real TCP connections and are not run by `make test` (`-short` flag).

## Build & Dependencies
- **Go version**: See `go.mod` (currently Go 1.24.1).
- **Dependency management**: `make update-gomod` runs `go mod tidy` + `go mod vendor`. Always run this after adding or changing dependencies.
- **Vendoring**: The project vendors dependencies. Committed vendor directory must be up-to-date.
- **Pre-commit checklist**: `make check` (lint + vet) → `make test` → `make update-gomod` if deps changed.
