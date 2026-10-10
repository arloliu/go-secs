# tracepack

tracepack (`.tpk`) is an immutable, compressed, self-indexed container for recorded SECS-II traffic between one tool and its host,
intended as a long-term archival format documented for any reader.
The specification is at v2.32, format 1.0;
the Go module `github.com/arloliu/go-secs/tracepack` in `tracepack/` is its only implementation, and the conformance corpus binds format-level reading only (G5-199).

Read in this order: overview, format, JSONL, corpus, semantics, storage, then the Go mapping.
Documents here follow `.agents/rules/450-doc-lifecycle.md`.

| File | Class | Status | Purpose |
|---|---|---|---|
| `tracepack-overview.md` | living | current | purpose, document map, terminology, diagrams, open questions, deferred items (informative) |
| `tracepack-format.md` | living | current | byte format, registries, validation, versioning, canonical JSONL, conformance corpus contents (normative) |
| `tracepack-jsonl.md` | living | current | byte form of the canonical JSONL export, schema `tracepack-jsonl/1` (normative) |
| `tracepack-corpus.md` | living | current | conformance corpus of format-level reading: layout, manifest, JSON schemas of the goldens, item-validity checklist, vector catalogue, schema `tracepack-corpus/3` (normative) |
| `tracepack-semantics.md` | living | current | record semantics, transport events, quality, indexes, transaction lookup, redaction (normative) |
| `tracepack-storage.md` | living | current | storage profile: scopes and roles, keys, reads, merge and deletion, recorder and durable-bus contracts, recovery, converter (normative); the catalog and retention are the service's |
| `tracepack-go.md` | living | current | Go module, API, mapping from go-secs, CLI |
| `tracepack-spec-changelog.md` | living | current | every spec version and review round, with finding→fix tables |
| `tracepack-decisions.md` | living | current | owner decisions, append-only |
| `tracepack-impl-plan.md` | finite | active | phased Go implementation plan with per-phase status |
| `proposals/p7-durable-bus-capture-closure.md` | finite | deferred | closing a crashed producer's capture over a durable bus (issue record) |
| `proposals/p8-bus-record-transport.md` | finite | draft | what one bus message carries, and how a record larger than the bus message limit crosses the bus |

Next proposal ID: P12.
P1, P3, P6, P9 and P11 were applied as spec v2.6, v2.7, v2.8, v2.13 and v2.23 (see the changelog); P10 was rejected (G5-101);
P2, P4 and P5 were closed with spec v2.32, superseded by G5-201 and G5-202.
