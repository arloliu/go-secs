# tracepack — Go reference implementation plan (v1)

Status: active (2026-09-27) — phase 1 in progress.
Implements: tracepack v2.8 (format 1.0) — `tracepack-format.md` [FMT], `tracepack-semantics.md` [SEM], `tracepack-storage.md` [STO] — and `tracepack-go.md`.
`main` holds go-secs PR #14 (zero-length localized strings, `W` SML grammar) since 2026-09-27;
repository integration follows G5-78 and G5-79.

## 1. Goal and scope

Deliver the Go reference library for tracepack format 1.0, plus a minimal CLI,
so that recorders, the merger and a query service can be built on it.

In scope:
- every fixed structure, TLV, codec and CRC of the spec, with golden vectors;
- `Writer`, `Reader`, `Verify`, `Repair`, `Recover`, `Merge`, `MergeIterate`, `FindTransaction`, `ExportJSONL`, and `Extract` with redaction ([SEM §8]);
- the go-secs classifier (`decode_status` from frame bytes);
- the conformance corpus generator and its first vectors;
- CLI subcommands `list`, `stats`, `dump`, `verify`, `merge`, `recover`.

Out of scope (separate designs or later phases):
the go-secs conn-wrapper recorder, eqp-hub integration, `tapconv`, the query service, its MariaDB catalog, the live-tail interface,
the deferred F-3 index tags and F-4 ([OVW §6]), CLI `grep` / `tx`, and the redaction policy file format and key management.

## 2. Module and package layout

Nested module `github.com/arloliu/go-secs/tracepack` (own `go.mod` in `tracepack/`, tags `tracepack/vX.Y.Z`, G5-77), created in phase 0.

| Package | Content | Public? |
|---|---|---|
| `tracepack` | `Record`, enums, `PackMeta`, `Writer`, `Reader`, `Filter`, `Verify`, `Repair`, `Recover`, `Merge`, `MergeIterate`, `FindTransaction`, `Extract`, `RedactionPolicy` | yes |
| `tracepack/jsonl` | canonical export | yes |
| `tracepack/classify` | go-secs-based `decode_status` classifier | yes |
| `tracepack/internal/format` | byte layouts: file header, envelope, record header, trailer, F-1, F-2; CRC; UUID byte order | no |
| `tracepack/internal/tlv` | TLV entry encoding, value types, tag registries, validation | no |
| `tracepack/internal/codec` | `none`, `zstd` | no |
| `tracepack/internal/corpus` | conformance vector generator | no |
| `tracepack/cmd/tracepack` | CLI | binary |

Rules from the repo apply: internal packages never appear in public signatures;
only `classify` and `cmd/tracepack` (for SML rendering through `sml.Encoder`) import `github.com/arloliu/go-secs/v2`.
### 2.1 Module dependencies and workspace (G5-78, G5-79)

- `tracepack/go.mod` never carries a `replace` directive:
  consumers ignore it, and it makes `go install github.com/arloliu/go-secs/tracepack/cmd/tracepack@<version>` fail.
  (`benchmarks/go.mod` uses one, but that module is never published.)
- Phases 0 and 1 import nothing from go-secs, so the first go-secs requirement appears in phase 2.
  Until a go-secs release has what tracepack needs, `tracepack/go.mod` requires a pseudo-version of a pushed `main` commit
  (`go get github.com/arloliu/go-secs/v2@<sha>` inside `tracepack/`).
- A released tracepack requires a released go-secs, at least v2.5.0:
  the classifier needs zero-length localized strings, and `dump --sml` needs the `W` SML grammar.
- `go.work` and `go.work.sum` are gitignored and never committed.
  Developers create one with `go work init . ./tracepack`; CI creates its own in the tracepack job only.
  Workspace mode hides a missing `require` in `tracepack/go.mod`, and `go mod tidy` ignores `go.work`.
  A committed `go.work` would also make the root module's builds select dependency versions from tracepack's requirements,
  versions that go-secs consumers never get.
- Root Makefile variables that call `go list` run with `GOWORK=off`, because `go list -m` prints every workspace module.
  `LATEST_GIT_TAG` matches only `v[0-9]*` tags, because `git describe --tags` otherwise returns a newer `tracepack/` tag.

## 3. Phases

Each phase ends with `make lint-tracepack`, `make test-tracepack` and an external post-implementation review before the next phase starts.

| Phase | Status |
|---|---|
| 0 — Module skeleton | done |
| 1 — Format primitives | in-progress |
| 2 — Writer and classifier | pending |
| 3 — Reader | pending |
| 4 — Verify, Repair, Recover | pending |
| 5 — Merge, MergeIterate, FindTransaction | pending |
| 6 — JSONL export, conformance corpus, CLI | pending |
| 7 — Extract and redaction | pending |

### Phase 0 — Module skeleton

- Move the four tracepack documents, `tracepack-go.md` and the changelog to `docs/specs/tracepack/`,
  together with this plan, the decision log and the deferred proposals, under `.agents/rules/450-doc-lifecycle.md`.
- Create `tracepack/go.mod` with module path `github.com/arloliu/go-secs/tracepack` (same Go version as the root);
  it starts with no requirement, because `go mod tidy -diff` in the consumer leg removes an unused one;
  `github.com/klauspost/compress/zstd` arrives with the first importer of `internal/codec`.
  `.gitignore` gains `go.work` and `go.work.sum` (§2.1).
- Makefile, following the `tools/gemgen` targets:
  `lint-tracepack` (pinned linter, root `.golangci.yaml`), `test-tracepack` (`-race`), `fuzz-tracepack`,
  `check-tracepack-consumer` (the consumer leg's commands), `work` (creates the local `go.work`) and `update-pkg-cache-tracepack`.
  `ci` gains `lint-tracepack` and `test-tracepack`; `MODULE_PATH`, `TEST_DIRS` and `LATEST_GIT_TAG` change per §2.1.
- CI: a separate `tracepack` job in `.github/workflows/ci.yaml`,
  because the root jobs never reach the nested module.
  Like the root jobs, it installs Go and golangci-lint from the repository's `mise.toml` (`jdx/mise-action`, `GOTOOLCHAIN=local`);
  `setup-go` with `cache-dependency-path` fails while `tracepack/go.sum` does not exist.
  The job has two legs:
  - workspace leg, blocking: `go work init . ./tracepack`, then lint and `-race` tests,
    so one PR may change the root and tracepack together;
  - consumer leg, advisory on PRs and blocking on pushes to `main`: `GOWORK=off`, `go mod tidy -diff`,
    then build and test against the requirements in `tracepack/go.mod`.
    It must pass before any `tracepack/` tag.
- Release flow for the module (G5-79), added to the go-secs release recipe:
  - annotated tags `tracepack/vX.Y.Z` on `main`, independent of go-secs `v2.x.y` tags;
    the first release is `v0.1.0`, and a later major version is `github.com/arloliu/go-secs/tracepack/v2` in the same directory;
  - before tagging: `tracepack/go.mod` requires a released go-secs (at least v2.5.0), the consumer leg passes,
    and `gorelease` runs inside `tracepack/` against the previous tracepack version (none before the first release);
  - notes from `tracepack/CHANGELOG.md`; `gh release create tracepack/vX.Y.Z --latest=false`,
    so the go-secs release stays "Latest";
  - `make update-pkg-cache-tracepack` warms `proxy.golang.org/github.com/arloliu/go-secs/tracepack/@v/vX.Y.Z.info`.
- Not in phase 0: the root job's Go matrix (1.23, 1.24, 1.25) runs go1.26.0 in every leg (run 36294169819),
  because the root `go.mod` says `go 1.26.0`; fixing it is a separate change.

Done when: an empty module builds, lints and passes both CI legs,
and a scratch module outside the repository resolves it by a pseudo-version of the pushed branch.
That check runs with `GOPRIVATE=github.com/arloliu/go-secs`,
so the branch commit reaches neither the public proxy nor the checksum database.

### Phase 1 — Format primitives

- CRC-32/ISO-HDLC helper with the check-value test (`123456789` → 0xCBF43926).
- UUID: RFC 9562 byte order encode/decode; UUIDv7 generator.
- Fixed layouts with `MarshalBinary` / `UnmarshalBinary`-style functions:
  file header (80 B), block envelope (40 B, `record_header_len` at offset 6), record header (56 known B), trailer (64 B), F-1 (72 B), F-2 entry (80 B, `record_header_len` at offset 76).
  Each validates magic, CRC, reserved-zero on write, limits (≤ 2^63 − 1, ≤ 2^31 − 1) on read.
- TLV: entry encode/decode, value types, per-registry type checks, repeatable and required tags, unknown-tag preservation,
  nested TLV, *u32 array* and *u64 array* helpers; registries for pack metadata, transport-event, annotation, F-3, F-5, `coverage`, `epoch`, `boundary`, `hsms_timers`.
- Enum types with `String()` returning `unknown(<n>)` for unknown values; bit-set types.

Tests: one table test per layout asserting every field offset; golden byte vectors; round trips;
fuzz targets for every decoder (no panic, allocation bounded by input length).

Done when: all layouts round-trip, every spec offset is asserted by a test, fuzzers run 10 minutes clean.

### Phase 2 — Writer and classifier

- `PackMeta` builder that enforces the "required when" column of the tag registry ([FMT §5]).
- Block builder: capture-scoped seq assignment (I-12), strictly increasing seq, hour alignment (I-13), clock-step detection against the fixed anchor ([SEM §4]),
  size threshold (default 4 MiB), a record larger than the threshold alone in a block,
  body as header section + payload section ([FMT §6]), codec encode, envelope with `record_header_len`, `first_seq` and CRCs.
- Validating writer option: commits `blocks_validated` in the pack metadata, decodes each encoded block and checks I-2 and I-10 before writing it,
  and withholds the trailer on a failed check ([FMT §12]).
- Durability: `Syncer` called after each block and after the trailer ([FMT §12]).
- Footer builder: F-2 entries, F-3 lists (`kind_counts`, `dir_counts`, `decode_status_counts`, `max_payload_len`, `quality_union`),
  plus the per-block `content_bytes`, `epoch`, `boundary` and (when needed) `seq_range` tags;
  F-5 computed only by the aggregation rule over F-2 and F-3 ([FMT §10]).
- `Close` returns the next seq.
- `classify.Frame(b []byte, maxFrameLen int) (DecodeStatus, trailing int)` per [SEM §3] using the go-secs mapping of `tracepack-go.md` §4.
  `tracepack/go.mod` gains its first go-secs requirement here: a `main` commit at or after PR #14, or v2.5.0 once released (§2.1).

Tests: round trip through a minimal reader; hour-boundary split; oversized record; codec `none` vs `zstd` equivalence after decode;
validating writer: an agreeing pack is attested, an I-10 disagreement returns an error and leaves no trailer;
F-5 against a brute-force recomputation; unavailable copy fields written as zero; one classifier test per `decode_status` value.

Done when: written packs pass Phase 1 decoders, footers match brute-force recomputation, classifier covers every status.

### Phase 3 — Reader

- Bootstrap: `Open(ctx, io.ReaderAt, size, opts)` with an optional footer hint, speculative head and tail reads,
  version and `schema_version` checks before any block ([FMT §13]).
- Footer validation ([FMT §10]) with fallback to the forward walk when the footer is invalid.
- Block access through F-2; envelope checked against its F-2 entry, `record_header_len` included; decode with I-2 checks;
  unknown bytes of a `record_header_len` > 56 preserved and `f2_entry_len` > 80 skipped correctly.
- Header-only reads ([FMT §6]): decode the header-section prefix, with the header-only checks; full reads for payloads.
- Filters treat unavailable copy fields as "cannot match" unless partial records are requested.
- `Iterate(ctx, Query, fn)`: time pruning by F-2; the two query modes of [SEM §7.4] with their validation statuses:
  provisional filtering on header copies reporting `header-validated`,
  authoritative filtering on payload values or attested header copies with the full-read fallback for non-attested blocks;
  `Incomplete` for corrupt blocks, unknown codec, truncated tail.

Tests: a counting `ReaderAt` asserting request rounds for the three bootstrap paths;
corrupt-byte injection at every offset of a small pack (every result is either correct or `Incomplete`, never silently wrong);
CRC-valid but structurally invalid footers (fallback to forward walk);
forward-compatibility vectors (unknown tags, enums, longer record header and F-2 entries);
the query-mode vectors of [FMT §16] in both modes:
the S6F11 / S1F3 disagreement in a non-attested pack, the agreeing record in an attested pack,
and the labelled nonconforming false-attestation fixture, for which a header-only read is not expected to detect the false assertion.

Done when: bootstrap costs one round in the hint and small-window paths, and the corruption sweep finds no silent error.

### Phase 4 — Verify, Repair, Recover

- `Verify`: forward block walk (I-1, I-2), outcomes `finalized-consistent`, `finalized-truncated`, `unfinalized`, `corrupt-middle`, report with offsets.
- `Repair`: rejects an extract ([STO §2]); writes a generation-0 `repair` patch with `supersedes` = the damaged pack, `patch_base`, the damaged pack's inherited `coverage` plus new `coverage` (capture_id + seq ranges + time intervals).
- `Recover`: spool file + liveness anchor → segment of the original capture, same seqs, plus a `stop-unclean` boundary record with `gap_start` / `gap_end` computed per [STO §4].
- Writer-side spool durability contract helpers: clock anchor and drift check ([SEM §4]), liveness anchor file rewritten every F, at spool rotation and after a durable clock-step ([STO §4]);
  segment roll at an hour change.
- Tests: the durable clock-step → size roll → empty spool → crash vector; nonzero `capture_origin_mono_ns`.

Tests: truncate a pack at every byte offset and check the outcome;
recovery keeps the validated prefix byte-identical and continues seq correctly.

Done when: every truncation point yields the expected outcome and no validated record is lost.

### Phase 5 — Merge, MergeIterate, FindTransaction

- `ActiveView(packs, commits)`: the [STO §4] view of a scope from pack metadata and commit objects
  (highest-ranked complete generation by (`publisher_epoch`, `scope_generation`), unconsumed generation-0 packs, patches with a matching `patch_base` naming segments or generation members)
  and the packs deletable under [STO §4]; staging segments are part of the input.
  Whether a commit object may be deleted is the service's decision from its record of deleted packs, outside `ActiveView` ([STO §4] Deletion).
- Listing-backed `PackSource` for scopes the catalog does not index: a coherent observation ([STO §5]) —
  the commit listing repeated until two complete listings agree, then the `archive/` and `staging/` listings, then a [FMT §13] bootstrap of each listed pack —
  feeding `ActiveView`; results touching such scopes are `Incomplete` with `Reason: Cold`.
- Retention boundary, supplied by the caller: checked before each listing and before emitting each block's records;
  an hour that becomes removed ends with the removed outcome in `Result.Removed` ([STO §5] Retention).
- `Merge` for one scope (capture, UTC hour): inputs are its active view; output is the allocated claim number under the publisher epoch, with cumulative `compacted_from` and union `coverage`;
  plan from input footers only; copy blocks of the hour in `first_seq` order by range reads;
  copy non-overlapping blocks verbatim; drop an overlapping block only when envelope and body bytes are byte-for-byte equal (`body_crc` is only a candidate filter);
  decode-and-resolve every other overlap, starting a new output block when `record_header_len` changes; report `conflict` for same seq with different bytes;
  inputs include the whole active archive set;
  optional coalescing of small blocks with equal `record_header_len`; every new encoding validated in memory before it is written ([STO §4]);
  optional attestation: every block of a member validated before its pack metadata is written, `blocks_validated` committed only when all pass; F-5 by the aggregation rule ([FMT §10]); lineage tags (`compaction_level`, `compacted_from`, `supersedes`, inherited `coverage`);
  replacement sets for size-capped output, validated for consistent size, `supersedes`, identical `compacted_from`, capture, scope, `publisher_epoch`, generation and unique indexes ([STO §6]);
  commit-object handling in `ActiveView` ([STO §3], [STO §5]).
- `MergeIterate`: capture order (streaming) and time order (watermark over F-2 `ts_min`, bounded memory with a caller limit) with dedup (`tracepack-go.md` §3).
- `FindTransaction` over a `PackSource` interface (catalog abstraction; an in-memory implementation for tests), [SEM §7.2] results,
  honouring completeness barriers (`stop-unclean`, [STO §5]) and returning `Incomplete` with `Reason: Cold` when the eligibility window touches a scope that is not indexed.

Tests: merge of generated segments equals a directly written archive record-for-record;
generations A → B → C with B deleted before A (packs in either order, each commit object after its packs), then view rebuild,
and the fallback to G−1 when G's commit object is removed first; correction → late merge; repair → late merge; coverage-only repair;
time-order iteration with backward timestamps within and across blocks and packs;
transaction eligibility with a reused key;
merged F-5 equals brute-force recomputation; overlapping and duplicated inputs; conflict detection;
a transaction split across packs; `incomplete` when seq coverage has a gap, and with reason `cold` when a scope is not indexed;
the coherent-observation, listing-view, window-boundary and removed-outcome vectors of [STO §8];
through an in-memory `PackSource`, the barrier and closure effects of the [STO §8] end-evidence service vectors
(registration, rejection, eviction and rebuild themselves are service work, §1);
the merge-framing, failed-encoding and attestation vectors of [STO §8].

Done when: output records equal the de-duplicated union of the inputs;
with optional coalescing disabled, no block of the non-overlap path is re-encoded (asserted by an encode counter);
attested output passes `Verify`;
for a merge that validated its output but could not attest it, `Verify` reports exactly the input defects the merger reported.

### Phase 6 — JSONL export, conformance corpus, CLI

- `jsonl.Export` per [FMT §15]; write the draft byte-exact schema (`tracepack-jsonl/1`: key order, number and base64 formatting) alongside.
- Corpus generator producing the [FMT §16] vectors except the redaction vectors (phase 7), with golden `.tpk`, `.jsonl` and verify reports under `testdata/corpus/`,
  plus the expected query results and validation statuses of the query vectors,
  regenerated only with an explicit `-update` flag.
- CLI: `list`, `stats`, `dump --sml | --jsonl`, `verify [--repair]`, `merge`, `recover`; local paths first, `s3://` through an `io.ReaderAt` adapter;
  on `s3://` sources, `list`, `stats` and `dump` end an hour that becomes removed with the removed outcome ([STO §5] Retention).

Done when: the corpus regenerates byte-identically, `dump --jsonl` matches every golden,
every query vector yields its expected results and validation statuses, and the CLI works on the corpus.

### Phase 7 — Extract and redaction

- E5 item walker with byte offsets and the item-validity predicate of [SEM §3], plus the format-22 rule of [SEM §8].
- Policy compilation: S/F, `*` paths, domains (1–65535 bytes), annotation kinds; invalid patterns and domains rejected.
- Two-pass `Extract`: plan every mask and digest (captured message text, total path resolution, no-content and whole-text rules), then write header, pack metadata and masked blocks;
  footer from the masked blocks; masked inputs rejected before any output.
- HMAC-SHA-256 with the corpus test keys; `redaction_policy` and `redaction` entries.
- Reader: entries parsed and structurally checked at `Open`, validated per record at read time, wholly masked records reported.
- `dump --sml` rendering of masked content.
- F-4 itself stays out of scope; the F-4 rule of [SEM §8] binds whoever builds it later.

Tests: the redaction vectors of [FMT §16], generated with their goldens and expected reports by the phase-6 corpus generator extended here; a sweep over the corpus asserting, for every record, that each target byte is zero, each other byte equals the source,
the payload length and every record-header field except `quality.redacted` are unchanged, and `Verify` reports exactly the source's defects and no new one.

Done when: every redaction vector reproduces its bytes, entries and digests exactly, and the sweep passes.

## 4. Cross-cutting requirements

- Performance targets (checked with benchmarks, not tuned before measurement): writer ≥ 50 MB/s uncompressed per core with zstd level 3;
  merge throughput measured with and without attestation; reader bootstrap allocations independent of file size, apart from the pack metadata (an extract's redaction entries).
- Memory: the writer holds at most one block; the reader holds at most the pack metadata and the footer plus one decoded block per concurrent iterator,
  except time-order `MergeIterate`, whose heap is bounded by the blocks overlapping its watermark and by a caller limit.
  An extract's pack metadata grows with its redaction entries, and the extract writer's plan grows with the matches of the selected records.
- Errors: sentinel errors per failure class (bad magic, CRC, limit, unknown codec, schema) wrapped with offsets.
- Concurrency: `Reader` safe for concurrent `Iterate`; `Writer` single-goroutine by contract.

## 5. Risks

| Risk | Mitigation |
|---|---|
| A spec ambiguity surfaces during implementation | stop, record it in the changelog, amend the spec before coding past it |
| Footer merge rule misses a statistic that cannot be restricted to copied blocks | brute-force recomputation tests in phases 2 and 5 |
| go-secs classification diverges from [SEM §3] on edge cases | the corpus decides; classifier adjusted, `classifier` tag versioned |
| tracepack needs a go-secs change after v2.5.0 | the change lands in go-secs first; tracepack requires its `main` pseudo-version until the next go-secs release (§2.1) |
| zstd output differs across library versions | nothing depends on compressed bytes being reproducible; goldens compare decoded content plus codec `none` vectors for byte-exactness |
