# 500 — Workflow

## Before commit

1. `make lint` — fix all issues.
2. `make test` — must pass with race detector.
3. Touched connection state, timers, or block transport? Run `make stress-quick` (or `make stress-test` for a broader sweep).
4. Touched a decoder or parser? Run `make fuzz-test` (extend with `FUZZ_TIME=5m` if warranted).
5. Changed exported API or documented behavior? Update `README.md` / `sml/README.md` / `doc.go`.

## Commits

- Branches: `feat/`, `fix/`, `docs/`, `chore/`, `test/`, with optional package scope (`feat/hsmsss/linktest-threshold`).
- Conventional format, present tense, first line < 50 chars, package scope in parens: `fix(hsms): ConnStateMgr race`.
- No `Co-Authored-By` or other attribution trailers.

## Review checklist

- [ ] Correctness
- [ ] `hsms.ConnStateMgr` transitions remain race-free
- [ ] `DataMessage.Free` idempotency preserved
- [ ] `hsmsss` and `secs1` still satisfy `hsms.Connection` / `hsms.Session`
- [ ] No `internal/` types in public signatures or docs
- [ ] No unnecessary allocs on encode / decode / per-message paths
- [ ] Fuzz targets extended for new decoder entry points
- [ ] Docs updated for exported API changes

## Make targets

```bash
# Lint & tests
make lint update-tools
make test build-tests
make stress-test stress-quick
make fuzz-test           # FUZZ_TIME=30s default
make coverage coverage-report
make clean

# Module hygiene
make gomod-tidy gomod-vendor update-gomod
make update-pkg-cache
```
