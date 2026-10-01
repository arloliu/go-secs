# tracepack — Go reference implementation plan (v1)

Status: active (2026-10-01) — phases 4 and 5a done (`Verify`, `Repair`, `ActiveView`, `Merge`); phase 5b next (`MergeIterate`).
Implements: tracepack v2.18 (format 1.0) — `tracepack-format.md` [FMT], `tracepack-semantics.md` [SEM], `tracepack-storage.md` [STO] — and `tracepack-go.md`.
`main` holds go-secs PR #14 (zero-length localized strings, `W` SML grammar) since 2026-09-27;
repository integration follows G5-78 and G5-79.

## 1. Goal and scope

Deliver the Go reference library for tracepack format 1.0, plus a minimal CLI,
so that recorders, the merger and a query service can be built on it.

In scope:
- every fixed structure, TLV, codec and CRC of the spec, with golden vectors;
- `Writer`, `Reader`, `Verify`, `Repair`, `Recover` (deferred, G5-91), `Merge`, `MergeIterate`, `FindTransaction`, `ExportJSONL`, and `Extract` with redaction ([SEM §8]);
- the go-secs classifier (`decode_status` from frame bytes);
- the conformance corpus generator and its first vectors;
- CLI subcommands `list`, `stats`, `dump`, `verify`, `merge`, and `recover` once `Recover` is planned (G5-91).

Out of scope (separate designs or later phases):
the go-secs conn-wrapper recorder, eqp-hub integration, `tapconv`, the query service, its MariaDB catalog, the live-tail interface,
indexes in the footer (no longer planned, [OVW §6]), CLI `grep` / `tx`, and the redaction policy file format and key management.

## 2. Module and package layout

Nested module `github.com/arloliu/go-secs/tracepack` (own `go.mod` in `tracepack/`, tags `tracepack/vX.Y.Z`, G5-77), created in phase 0.

| Package | Content | Public? |
|---|---|---|
| `tracepack` | `Record`, enums, `PackMeta`, `Writer`, `Reader`, `Filter`, `Verify`, `Repair`, `Recover` (deferred), `Merge`, `MergeIterate`, `FindTransaction`, `Extract`, `RedactionPolicy` | yes |
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
| 1 — Format primitives | done |
| 2 — Writer and classifier | done |
| 3 — Reader | done |
| Format revision (spec v2.13) | done |
| 4 — Verify, Repair | done |
| 5a — ActiveView, Merge, Writer seams | done |
| 5b — MergeIterate | pending |
| 5c — FindTransaction, PackSource, listing views, retention | pending |
| 6 — JSONL export, conformance corpus, CLI | pending |
| 7 — Extract and redaction | pending |
| 8 — Producer API for a durable-bus capture | pending |

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
  file header (80 B), block envelope (40 B, `record_header_len` at offset 6), record header (44 known B since the format revision), trailer (64 B), F-1 (72 B), F-2 entry (80 B, `record_header_len` at offset 76).
  Each validates magic, CRC, reserved-zero on write, limits (≤ 2^63 − 1, ≤ 2^31 − 1) on read.
- TLV: entry encode/decode, value types, per-registry type checks, repeatable and required tags, unknown-tag preservation,
  nested TLV, *u32 array* and *u64 array* helpers; registries for pack metadata, transport-event, annotation, F-3, F-5, `coverage`, `epoch`, `boundary`, `hsms_timers`.
- Enum types with `String()` returning `unknown(<n>)` for unknown values; bit-set types.

Tests: one table test per layout asserting every field offset; golden byte vectors; round trips;
fuzz targets for every decoder (no panic, allocation bounded by input length).

Done when: all layouts round-trip, every spec offset is asserted by a test, fuzzers run 10 minutes clean.

### Phase 2 — Writer and classifier

- `PackMeta` builder that enforces the "required when" column of the tag registry ([FMT §5]).
- Block builder: capture-scoped seq assignment (I-12) or acceptance of producer-assigned seqs with a second open segment for a record below a closed block ([STO §4] Recorder over a durable bus); strictly increasing seq, hour alignment (I-13), clock-step detection against the fixed anchor ([SEM §4]) or preservation of producer-detected steps,
  size threshold (default 4 MiB), a record larger than the threshold alone in a block,
  body as header section + payload section ([FMT §6]), codec encode, envelope with `record_header_len`, `first_seq` and CRCs.
- Validating writer option: decodes each encoded block and checks it before writing it,
  and withholds the trailer on a failed check ([FMT §12]); the format-revision step below revises what it checks and commits.
- Durability: `Syncer` called after each block and after the trailer ([FMT §12]).
- Footer builder: F-2 entries, F-3 lists (`kind_counts`, `dir_counts`, `decode_status_counts`, `quality_union`),
  plus the per-block `epoch`, `boundary` and (when needed) `seq_range` tags;
  F-5 computed only by the aggregation rule over F-2 and F-3 ([FMT §10]).
  The format-revision step retires the two other F-3 tags this phase wrote ([FMT §10]).
- `Close` returns the next seq.
- `classify.Frame(b []byte, maxFrameLen int) (DecodeStatus, trailing int)` per [SEM §3] using the go-secs mapping of `tracepack-go.md` §4.
  `tracepack/go.mod` gains its first go-secs requirement here: a `main` commit at or after PR #14, or v2.5.0 once released (§2.1).

Tests: round trip through a minimal reader; hour-boundary split; oversized record; codec `none` vs `zstd` equivalence after decode;
validating writer: a failed check returns an error and leaves no trailer;
F-5 against a brute-force recomputation; one classifier test per `decode_status` value.
The format-revision step replaces the tests of the retired record-header fields.

Done when: written packs pass Phase 1 decoders, footers match brute-force recomputation, classifier covers every status.

### Phase 3 — Reader

- Bootstrap: `Open(ctx, io.ReaderAt, size, opts)` with an optional footer hint, speculative head and tail reads,
  version checks before any block ([FMT §13]); the format-revision step drops a retired metadata check.
- Footer validation ([FMT §10]) with fallback to the forward walk when the footer is invalid.
- Block access through F-2; envelope checked against its F-2 entry, `record_header_len` included; decode with I-2 checks;
  unknown bytes of a longer `record_header_len` preserved and `f2_entry_len` > 80 skipped correctly.
- Filters treat unavailable fields as "cannot match" unless partial records are requested.
- `Iterate(ctx, Query, fn)`: time pruning by F-2; `Incomplete` for corrupt blocks, unknown codec, truncated tail.
- This phase also built a read of the header section alone and query modes over header fields;
  the format-revision step removes them (every read full, [FMT §6]; predicates on payload values, [SEM §7.4]).

Tests: a counting `ReaderAt` asserting request rounds for the three bootstrap paths;
corrupt-byte injection at every offset of a small pack (every result is either correct or `Incomplete`, never silently wrong);
CRC-valid but structurally invalid footers (fallback to forward walk);
forward-compatibility vectors (unknown tags, enums, longer record header and F-2 entries);
the query vectors of [FMT §16] as they stood; the format-revision step replaces them.

Done when: bootstrap costs one round in the hint and small-window paths, and the corruption sweep finds no silent error.

### Format revision (spec v2.13)

Applies spec v2.13 (G5-92..G5-98) to the code before phase 4 (G5-91); the spec changelog's v2.13 entry lists every change.
- Writer: transpose the record headers into the header section when a block body is assembled ([FMT §6]).
- Reader: untranspose the decoded header section into a reusable row buffer; every record-level path stays as it is.
- The 44-byte record header ([FMT §7.1]): `Record` without the fields that copied the HSMS message header;
  a payload helper returning each HSMS header field's value and availability ([FMT §7.2]);
  a helper computing the `field_validity` of a raw frame, replacing `Record.SetHeaderCopies`;
  the append-time `field_validity` check in every writer; `quality` bits 3 and 6 and `record_flags` bit 0 retired ([FMT §9]).
- Decode state ([SEM §6]): `DecodeStatus.Malformed` true only for known malformed values, a new `DecodeStatus.Clean` true only for known values outside the malformed set.
- Every read full ([FMT §6]): removed are `ReadLevel` with `ReadHeaderOnly` and `ReadAttested`, `QueryMode` and `Query.Mode`, `PackHeader.Attested`, `Item.Level`, `Item.CopyMismatch`,
  `Result.HeaderValidated`, `PackMeta.BlocksValidated`, `ErrSchemaVersion`, `QualityDecodeFailed`, `QualityNoMono`, `RecordFlagsW`, and `PackRoleCorrection` with its `correction` name;
  `Query.Payloads` decides only whether payloads are returned.
- Validating writer option ([FMT §12]): decode the encoded body and untranspose its header section before the I-2 checks, with no metadata commitment.
- Retired numbers ([FMT §5], [FMT §10]): pack-metadata tags 0x0001, 0x0014 and 0x0030 never written, also not from preserved unknown entries;
  F-3 tags 0x0004 and 0x0006 never written, F-5 `content_bytes` aggregated from F-2 `uncompressed_len`; F-4 written absent.
- Retired role and set values ([FMT §5], [FMT §9]): `pack_role` 5 rejected by the `Writer` and read as `unknown(5)`; replacement-set size 1 and index 0, other values rejected.
- An in-repository benchmark that reproduces the columnar comparison of the v2.13 changelog entry on the implemented writer and reader.
- `tracepack/CHANGELOG.md` records the breaking changes: the columnar header section and the 44-byte record header, tracepack v0.1.0 packs unsupported ([FMT §14]),
  the retired tags, bits and value, and the removed API.
- Tests: golden block bodies and the reader's per-offset corruption expectations regenerated;
  a tracepack v0.1.0 sample pack with a multi-record block read as `corrupt` ([FMT §16]); round trips with `record_header_len` > 44, including 45–55;
  validated multi-record blocks under both codecs, with I-2 defects injected after encoding;
  `field_validity` availability for each short-field threshold, clear bits over placeholder bytes, a set bit beyond the payload rejected at append, control-frame fields read from the payload;
  retired tags present in read metadata and footers: skipped on read, kept in the canonical export, never written.
- Run the tracepack fuzz targets before committing the decoder change.

Done when: the writer and reader implement the revised layout, and the corruption sweep finds no silent error.

### Phase 4 — Verify, Repair

- `Verify` (done): forward block walk (I-1, I-2), the outcomes and writer defects of [FMT §13] (G5-100), report with offsets.
- `Repair` (done): the patch of [FMT §13] and [STO §6] (G5-102..G5-106):
  every validated block copied verbatim, the footer rebuilt from the records, `supersedes` = the damaged pack,
  the damaged pack's `coverage` carried as data plus new `coverage` from the trusted footer or the neighbour bounds, and every refusal decided before any output.
  Proposal P10 was rejected (G5-101), so `patch_base` is the current generation's `replacement_set_id`, supplied by the caller ([STO §2], [STO §5]).

Tests: truncate a pack at every byte offset and check the outcome;
the repair pack keeps the validated prefix byte-identical.

Done when: every truncation point yields the expected outcome and no validated record is lost.

Deferred until a local-spool recorder is planned (G5-91):
`Recover` (spool file + liveness anchor → segment of the original capture, same seqs, plus a `stop-unclean` boundary record with `gap_start` / `gap_end` per [STO §4]);
the writer-side spool durability helpers (liveness anchor rewritten every F, at spool rotation and after a durable clock-step, [STO §4]; segment roll at an hour change);
and their vectors (the durable clock-step → size roll → empty spool → crash vector; nonzero `capture_origin_mono_ns`).

### Phase 5 — Merge, MergeIterate, FindTransaction

Phase 5 is delivered in three steps: 5a (`ActiveView`, `Merge` and the Writer seams they need), 5b (`MergeIterate`),
and 5c (`FindTransaction`, `PackSource`, listing views and retention).

#### 5a — ActiveView, Merge, Writer seams (done)

- Spec v2.17 first: `classifier` and `max_frame_len` become repeatable (G5-108), an API break of `PackMeta`,
  and `compacted_from` is required only when a pack's lineage list is non-empty (G5-111).
  A block's epoch summaries are built in time linear in its records, since the Writer, `Verify`, `Repair` and `Merge` all build them.
- `ActiveView(packs, commits)`: the [STO §4] view of a scope from pack metadata and commit objects
  (highest-ranked complete generation by (`publisher_epoch`, `scope_generation`), unconsumed generation-0 packs, patches with a matching `patch_base` naming segments, generation members or patches)
  and the packs deletable under [STO §4]; staging segments are part of the input.
  Supersession is transitive (G5-107): every patch based on G removes the packs it names, also one that a later patch replaced,
  and Merge's `compacted_from` lists every generation-0 pack those patches name ([STO §4]).
  The input is a complete observation of the scope ([STO §4] Deletion), and each pack's `PackInfo` is built only from decoded pack metadata.
  Whether a commit object may be deleted is the service's decision from its record of deleted packs, outside `ActiveView` ([STO §4] Deletion).
- Writer seams: a copied block keeps its F-3 list verbatim ([FMT §10]);
  a block is built from raw record-header rows and payloads and checked in memory before it is written;
  the output footer is budgeted as blocks are appended.
- `Merge` for one scope (capture, UTC hour): inputs are its active view, and `Merge` takes the `View` for the lineage;
  output is the allocated claim number under the publisher epoch, with cumulative `compacted_from` and union `coverage`;
  every input read in full and refused unless it is `finalized-consistent`, inside its scope and within its own commitments, with capture-level tags equal to the others' (G5-109, [STO §4]);
  plan from input footers: clusters of overlapping F-2 seq ranges, in ascending first seq;
  copy a single-block cluster verbatim with its F-3 list;
  drop an overlapping block only when envelope and body bytes are byte-for-byte equal (`body_crc` is only a candidate filter);
  decode-and-resolve every other overlap, starting a new output block when `record_header_len` changes or the block size threshold is reached;
  a record conflict fails the merge, and every conflict is reported (G5-110);
  inputs include the whole active archive set;
  coalescing of small blocks by the greedy grouping of [STO §4] (an option; the rule is a SHOULD); every new encoding validated in memory before it is written ([STO §4]);
  F-5 by the aggregation rule ([FMT §10]); lineage tags (`compaction_level`, `compacted_from`, `supersedes`, inherited `coverage`),
  every distinct `classifier` and `max_frame_len` of the inputs (G5-108), and `seq_start` of an archive without records (G5-112);
  one archive per scope, a replacement set of one member ([STO §6]);
  only packs whose role takes part in the tiers ([STO §2]) are inputs;
  commit-object handling in `ActiveView` ([STO §3], [STO §5]).

Tests: merge of generated segments equals a directly written archive record-for-record;
generations A → B → C with B deleted before A (packs in either order, each commit object after its packs), then view rebuild,
and the fallback to G−1 when G's commit object is removed first; repair → late merge; coverage-only repair;
merged F-5 equals brute-force recomputation; overlapping and duplicated inputs; conflict detection;
the merge-framing, coalescing, failed-encoding and role vectors of [STO §8],
and its merge-input, conflict, lineage and deletion vectors of v2.17.

Done when: output records equal the de-duplicated union of the inputs;
with coalescing disabled, no block of the non-overlap path is re-encoded (asserted by an encode counter);
with it enabled, only groups of more than one block are;
output passes `Verify`;
a defective input and a record conflict each fail the merge with nothing a caller could publish.

Done (2026-10-01): every criterion above holds,
and a property check recomputes F-2, F-3 and F-5 of every merged archive from its records, independently of the Writer's footer code and `Verify`.
`FuzzMerge`, `FuzzMergeGenerated` and the pack-metadata, footer, open, iterate and verify fuzz targets ran 10 minutes each without a failure.
The implementation settled what the text above leaves open:
- Memory: a pending coalescing group holds one block summary per block in it, besides their records and on-disk bytes:
  about 0.6 KiB for a block of one record, at most threshold/44 of them, together no more than about the inputs' footer indexes, which the merge already holds.
  Coalescing encodes in its own encode and check buffers, kept for the whole merge beside the resolution's:
  about 3× the threshold with zstd, 1–2× with `none`.
- `MaxOpenBlocks` counts a block from when it is read and kept until its last record is resolved;
  a block dropped as a duplicate never counts, so a merge holds at most `MaxOpenBlocks` blocks and the one it is reading.
  The bound is checked after the block was read and checked, so a defective block fails with `ErrMergeInput` first.
- `Conflict.Versions` orders the versions by the first block holding each, blocks taken in ascending first seq, then in `View.Packs` order;
  each version lists its packs in `View.Packs` order, each once, those whose blocks were dropped as duplicates included.
- On error `MergeReport` holds only `Conflicts` and `ConflictsComplete`; every other field is zero.
- `ctx` is checked before each block read, every 4096 records resolved, and before `Close`.
  Once every block was compared, a merge that found a conflict returns `ErrMergeConflict`, complete, even if `ctx` was cancelled since;
  one without a conflict writes its pending coalescing group before that last check.
- `MergeReport.Coalesced` counts the input blocks coalesced into a new block,
  so `Copied`, `Duplicates`, `Resolved` and `Coalesced` sum to the number of input blocks.
- The archive's pack metadata must fit `MergeOptions.Reader.MaxPackMetadataLen`, the budget the next merge opens it with;
  longer metadata fails with `ErrMergeLimit` before `dst` is touched.

#### 5b — MergeIterate (pending)

- `MergeIterate`: capture order (streaming) and time order (watermark over F-2 `ts_min`, bounded memory with a caller limit) with dedup (`tracepack-go.md` §3).

Tests: time-order iteration with backward timestamps within and across blocks and packs.

#### 5c — FindTransaction, PackSource, listing views, retention (pending)

- Listing-backed `PackSource` for scopes the catalog does not index: a coherent observation ([STO §5]) —
  the commit listing repeated until two complete listings agree, then the `archive/` and `staging/` listings, then a [FMT §13] bootstrap of each listed pack —
  feeding `ActiveView`; results touching such scopes are `Incomplete` with `Reason: Cold`.
- Retention boundary, supplied by the caller: checked before each listing and before emitting each block's records;
  an hour that becomes removed ends with the removed outcome in `Result.Removed` ([STO §5] Retention).
- `FindTransaction` over a `PackSource` interface (catalog abstraction; an in-memory implementation for tests), [SEM §7.2] results,
  honouring completeness barriers (`stop-unclean`, [STO §5]) and returning `Incomplete` with `Reason: Cold` when the eligibility window touches a scope that is not indexed.

Tests: transaction eligibility with a reused key; a primary without System Bytes and a window holding a reply-direction record without them ([SEM §7.2]);
a transaction split across packs; `incomplete` when seq coverage has a gap, and with reason `cold` when a scope is not indexed;
the coherent-observation, listing-view, window-boundary and removed-outcome vectors of [STO §8];
through an in-memory `PackSource`, the barrier and closure effects of the [STO §8] end-evidence service vectors
(registration, rejection, eviction and rebuild themselves are service work, §1).

### Phase 6 — JSONL export, conformance corpus, CLI

- `jsonl.Export` per [FMT §15]; write the draft byte-exact schema (`tracepack-jsonl/1`: key order, number and base64 formatting) alongside.
- Corpus generator producing the [FMT §16] vectors except the redaction vectors (phase 7), with golden `.tpk`, `.jsonl` and verify reports under `testdata/corpus/`,
  plus the expected query results of the query vectors,
  regenerated only with an explicit `-update` flag.
- CLI: `list`, `stats`, `dump --sml | --jsonl`, `verify [--repair]`, `merge`, and `recover` once `Recover` is planned (G5-91); local paths first, `s3://` through an `io.ReaderAt` adapter;
  on `s3://` sources, `list`, `stats` and `dump` end an hour that becomes removed with the removed outcome ([STO §5] Retention).

Done when: the corpus regenerates byte-identically, `dump --jsonl` matches every golden,
every query vector yields its expected results, and the CLI works on the corpus.

### Phase 7 — Extract and redaction

- E5 item walker with byte offsets and the item-validity predicate of [SEM §3], plus the format-22 rule of [SEM §8].
- Policy compilation: S/F, `*` paths, domains (1–65535 bytes), annotation kinds; invalid patterns and domains rejected.
- Two-pass `Extract`: plan every mask and digest (captured message text, total path resolution, no-content and whole-text rules), then write header, pack metadata and masked blocks;
  footer from the masked blocks; masked inputs rejected before any output.
- HMAC-SHA-256 with the corpus test keys; `redaction_policy` and `redaction` entries.
- Reader: entries parsed and structurally checked at `Open`, validated per record at read time, wholly masked records reported.
- `dump --sml` rendering of masked content.

Tests: the redaction vectors of [FMT §16], generated with their goldens and expected reports by the phase-6 corpus generator extended here; a sweep over the corpus asserting, for every record, that each target byte is zero, each other byte equals the source,
the payload length and every record-header field except `quality.redacted` are unchanged, and `Verify` reports exactly the source's defects and no new one.

Done when: every redaction vector reproduces its bytes, entries and digests exactly, and the sweep passes.

### Phase 8 — Producer API for a durable-bus capture

A recorder prototype built on go-secs v2.6.0 (2026-09-28) wrote valid packs through `Writer`,
but a producer that publishes one record per bus message ([STO §4], proposal P8) cannot be written with the module as it is:

- No exported encoding of one record outside a block:
  `canonicalHeader` and the record header layout are unexported, so the bus message of P8 §2.1 has no implementation to share between producer and consumer.
- No capture-descriptor type:
  `PackMeta.MarshalBinary` validates the whole pack metadata, including values the consumer owns (`scope_generation` among them),
  so a producer cannot encode only the entries it owns.
- Clock-step detection is tied to seq assignment:
  `DetectClockSteps` requires `AssignSeq`, but a durable-bus producer assigns seq and detects clock steps itself ([SEM §4]),
  so it needs the detector on its own.
- `capture_origin_mono_ns`: Go exposes no raw monotonic reading, so a Go producer writes 0 and measures `mono_ns` from its origin `time.Time`;
  the spec should say whether 0 is an acceptable origin value or what a producer without a raw reading writes.
- A `stop` boundary cannot name its seq in advance when a clock-step may be inserted before it.
- `classify.Name` falls back to `go-secs/unknown` when go-secs is replaced by a local path, so a producer sets the classifier name explicitly.
- `Writer.Append` copies the payload a second time after the producer's own copy of a wire frame.

The first four depend on proposal P8's outcome and are designed with it; the last three are small fixes.
Tests and done criteria are set when P8 is decided.

## 4. Cross-cutting requirements

- Performance targets (checked with benchmarks, not tuned before measurement): writer ≥ 50 MB/s uncompressed per core with zstd level 3;
  merge throughput measured with and without coalescing; reader bootstrap allocations independent of file size, apart from the pack metadata (an extract's redaction entries).
- Memory: the writer holds at most one block; the reader holds at most the pack metadata and the footer plus one decoded block per concurrent iterator,
  except time-order `MergeIterate`, whose heap is bounded by the blocks overlapping its watermark and by a caller limit.
  An extract's pack metadata grows with its redaction entries, and the extract writer's plan grows with the matches of the selected records.
- Errors: sentinel errors per failure class (bad magic, CRC, limit, unknown codec, unsupported version) wrapped with offsets.
- Concurrency: `Reader` safe for concurrent `Iterate`; `Writer` single-goroutine by contract.

## 5. Risks

| Risk | Mitigation |
|---|---|
| A spec ambiguity surfaces during implementation | stop, record it in the changelog, amend the spec before coding past it |
| Footer merge rule misses a statistic that cannot be restricted to copied blocks | brute-force recomputation tests in phases 2 and 5 |
| go-secs classification diverges from [SEM §3] on edge cases | the corpus decides; classifier adjusted, `classifier` tag versioned |
| tracepack needs a go-secs change after v2.5.0 | the change lands in go-secs first; tracepack requires its `main` pseudo-version until the next go-secs release (§2.1) |
| zstd output differs across library versions | nothing depends on compressed bytes being reproducible; goldens compare decoded content plus codec `none` vectors for byte-exactness |
