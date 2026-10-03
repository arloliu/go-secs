# tracepack

tracepack (`.tpk`) is an immutable, compressed, self-indexed container for recorded SECS-II traffic between one tool and its host,
intended as a long-term archival format that any implementation, in any language, can read.
The specification is at v2.21, format 1.0; the Go reference implementation is the nested module `github.com/arloliu/go-secs/tracepack` in `tracepack/`.

Read in this order: overview, format, semantics, storage, then the Go mapping.
Documents here follow `.agents/rules/450-doc-lifecycle.md`.

| File | Class | Status | Purpose |
|---|---|---|---|
| `tracepack-overview.md` | living | current | purpose, document map, terminology, diagrams, open questions, deferred items (informative) |
| `tracepack-format.md` | living | current | byte format, registries, validation, versioning, canonical JSONL, conformance corpus (normative) |
| `tracepack-semantics.md` | living | current | record semantics, transport events, quality, indexes, transaction lookup, redaction (normative) |
| `tracepack-storage.md` | living | current | storage profile: scopes, generations, commit protocol, catalog, retention, recovery (normative) |
| `tracepack-go.md` | living | current | Go module, API, mapping from go-secs, CLI |
| `tracepack-spec-changelog.md` | living | current | every spec version and review round, with finding→fix tables |
| `tracepack-decisions.md` | living | current | owner decisions, append-only |
| `tracepack-impl-plan.md` | finite | active | phased Go implementation plan with per-phase status |
| `proposals/p2-merge-input-defects.md` | finite | deferred | defective merge inputs (issue record) |
| `proposals/p4-cold-scope-admissions.md` | finite | deferred | admissions for scopes outside the catalog window (issue record) |
| `proposals/p5-capture-registry.md` | finite | deferred | capture end evidence beyond the retained packs (issue record) |
| `proposals/p7-durable-bus-capture-closure.md` | finite | deferred | closing a crashed producer's capture over a durable bus (issue record) |
| `proposals/p8-bus-record-transport.md` | finite | draft | what one bus message carries, and how a record larger than the bus message limit crosses the bus |

Next proposal ID: P11.
P1, P3, P6 and P9 were applied as spec v2.6, v2.7, v2.8 and v2.13 (see the changelog); P10 was rejected (G5-101).
