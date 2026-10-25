# tracepack

tracepack (`.tpk`) is an immutable, compressed, self-indexed container for recorded SECS-II traffic between one tool and its host,
intended as a long-term archival format documented for any reader.
The specification is at v2.38, format 1.2;
the Go module `github.com/arloliu/go-secs/tracepack` in `tracepack/` is its only implementation, and the conformance corpus binds format-level reading only (G5-199).

Read in this order: overview, format, JSONL, corpus, semantics, storage, then the Go mapping.
Documents here follow `.agents/rules/450-doc-lifecycle.md`.

| File | Class | Status | Purpose |
|---|---|---|---|
| `tracepack-overview.md` | living | current | purpose, document map, terminology, diagrams, open questions, deferred items (informative) |
| `tracepack-format.md` | living | current | byte format, registries, validation, versioning, canonical JSONL, conformance corpus contents (normative) |
| `tracepack-jsonl.md` | living | current | byte form of the canonical JSONL export, schemas `tracepack-jsonl/1`, `tracepack-jsonl/2` and `tracepack-jsonl/3` (normative) |
| `tracepack-corpus.md` | living | current | conformance corpus of format-level reading: layout, manifest, JSON schemas of the goldens, item-validity checklist, vector catalogue, schema `tracepack-corpus/5` (normative) |
| `tracepack-semantics.md` | living | current | record semantics, transport events, quality, indexes, transaction lookup, redaction (normative) |
| `tracepack-storage.md` | living | current | storage profile: scopes and roles, keys, reads, merge and deletion, recorder and durable-bus contracts, masking at write, the close of a capture left open, converter (normative); the catalog and retention are the service's |
| `tracepack-go.md` | living | current | Go module, API, mapping from go-secs, CLI |
| `tracepack-spec-changelog.md` | living | current | every spec version and review round, with finding→fix tables |
| `tracepack-decisions.md` | living | current | owner decisions, append-only |

Next proposal ID: P12.
P1, P3, P6, P9 and P11 were applied as spec v2.6, v2.7, v2.8, v2.13 and v2.23 (see the changelog); P10 was rejected (G5-101);
P2, P4 and P5 were closed with spec v2.32, superseded by G5-201 and G5-202;
P7 and P8 were closed with spec v2.33 (G5-248): P8's rules are in the storage profile §4 (G5-216..G5-219), and P7's points are answered by G5-201, G5-231 and G5-240;
their last texts are in the repository history at commit `e77da98`.
The implementation plan was closed on 2026-10-20 with its work done in tracepack v0.6.0; its last text is at commit `7aec66d`.
