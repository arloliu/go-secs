# 500 — Workflow

## Before commit

1. `make lint` — fix all issues.
2. `make test` — must pass with race detector.
3. Touched `tracepack/` or `tools/gemgen/`?
   Steps 1–2 never reach them; run that module's lint and test targets below (`make ci` runs every gate).
4. Touched connection state, timers, or block transport?
   Run `make stress-quick` (or `make stress-test` for a broader sweep).
5. Touched a decoder or parser?
   Run `make fuzz-test` (extend with `FUZZ_TIME=5m` if warranted).
6. Changed exported API or documented behavior?
   Update `README.md` / `sml/README.md` / `doc.go`.

## Commits

See `550-git-conventions.md` for branch, commit-message, and pull-request conventions.

## Review checklist

- [ ] Correctness
- [ ] `hsms.ConnState` transitions and their lifecycle reporting remain race-free
- [ ] `hsms` messages and `secs2` items stay immutable once built
- [ ] `hsmsss.New` and `secs1.New` still return a `Connection` that satisfies `hsms.Connection`
- [ ] No `internal/` types in public signatures or docs
- [ ] No unnecessary allocs on encode / decode / per-message paths
- [ ] Fuzz targets extended for new decoder entry points
- [ ] Docs updated for exported API changes
- [ ] New comments and prose use semantic linefeeds; no stable text rewrapped (`400-documentation.md`)

## Make targets

```bash
# First-time setup (or after mise.toml changes): Go + golangci-lint
mise install

# Lint, format, vet
make lint                # pinned golangci-lint run
make fmt                 # pinned golangci-lint fmt (goimports etc.)
make vet
make check               # lint + vet
make docs-check          # docs/ against 450-doc-lifecycle.md (Status lines, subject READMEs, links)

# Tests
make test build-tests    # -short suite; compile-only
make test-all            # full suite (no -short)
make bench
make stress-test stress-quick
make fuzz-test           # FUZZ_TIME=30s default
make coverage coverage-report
make clean clean-coverage

# Module hygiene
make gomod-tidy gomod-vendor update-gomod mod-verify
make update-pkg-cache

# tools/gemgen (separate Go module -- root lint/test/ci never reach it on their own)
make lint-gemgen          # pinned linter, default + integration build tag
make test-gemgen          # gemgen's own unit tests
make test-gemgen-integration  # real-compile guard against secs2 (build tag: integration)

# tracepack (nested module github.com/arloliu/go-secs/tracepack -- root lint/test/ci never reach it)
make work                # create the local go.work (gitignored) for root + tracepack development
make lint-tracepack      # pinned linter, root .golangci.yaml
make test-tracepack      # tracepack's tests with -race
make check-tracepack-consumer  # GOWORK=off: tidy go.mod, build, -race tests; must pass before any tracepack/ tag
make fuzz-tracepack      # FUZZ_TIME=30s default
make update-pkg-cache-tracepack

# CI entry point
make ci                  # check + docs-check + test + gemgen and tracepack lint/test gates
```

## tracepack module rules

- `tracepack/go.mod` never carries a `replace` directive;
  it requires go-secs by a released version,
  or by a pseudo-version of a pushed `main` commit until a release has what it needs.
- `go.work` and `go.work.sum` are gitignored and never committed.
- Release steps for both modules: `docs/guides/releasing.md`.

Run `make` with no target for a categorized help list.
