---
name: go-api-review
description: Reviews the exported Godoc and README of a top-level go-secs package for library-consumer DX (discoverability, clarity, ease of use). Source of truth is exported API only — do not read `internal/`.
---

# go-api-review

Default scope: the top-level public packages listed in `100-overview.md`. Narrow with an argument (e.g., `hsmsss`, `secs1`, `sml`). Read only that package's `.go` files (not `_test.go`), its `doc.go`, `README.md`, and `sml/README.md` if applicable.

## Checklist

**Interfaces & types**
- Exported interfaces are small, behavior-named (`Connection`, `SECS2Endpoint`, `Message`, `SECS2Message`, `Item`).
- "Accept interfaces, return structs" where applicable.
- `hsmsss.New` and `secs1.New` are visibly substitutable behind `hsms.Connection`.
- No `internal/` types in exported signatures.

**Methods & options**
- Single responsibility per exported function / method.
- Value vs. pointer receivers consistent with mutation / size.
- Functional options (`WithActive`, `WithPassive`, `WithHostRole`, `WithEquipRole`, `WithDeviceID`, `hsms.WithT3`, `sml.WithParserStrictMode`, etc.) discoverable from Godoc;
  groups and mutually-exclusive pairs documented.
- Connection lifecycle explicit (`NewConfig` → `New` → `Open` → send / handler dispatch → `Close`).
- Item construction and serialization obvious (construct → `ToBytes` / `ToSML`), and immutability stated.
- When to use `New*Item` vs. shortcut constructors is obvious.

**Errors & context**
- `context.Context` threaded idiomatically.
- `errors.Is` / `errors.As` usable for sentinel and typed errors.
- Decode / parse errors distinct from transport / I/O errors.
- Timeout errors (T1–T8) distinguishable from generic I/O errors.

**Docs & examples**
- `doc.go` states purpose, relevant SEMI standard, and relation to other packages.
- Every exported symbol has Godoc.
- README shows the common wiring (active and passive, HSMS-SS and SECS-I) and compiles.
- A new user can ship a round-trip S1F1 using only the docs.

**Misuse & concurrency**
- Documented consequences of misuse: `Send*` before `Open`, use after `Close`.
- Thread-safety guarantees stated for `Connection` (including its handler and lifecycle callbacks), `DataMessage`, and `secs2.Item`.
- `Close` is idempotent and safe during in-flight traffic.
- State transitions (NOT-CONNECTED → CONNECTED → NOT-SELECTED → SELECTED) documented with the subscribe mechanism.
- Active-mode reconnect / backoff behavior documented (trigger, interaction with `Close`).

## Output

Report findings as a bullet list, grouped by package. For each: what's missing / wrong, and (optionally) a one-line suggested Godoc edit. Do not propose broad rewrites.
