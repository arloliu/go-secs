# tracepack — Go reference implementation plan (v1)

Status: active (2026-10-10) — phases 4, 5a, 5b, 5c1, 5c2, 5c3, 5d and 6a done (`Verify`, `Repair`, `ActiveView`, `Merge`, `MergeIterate`, `PackSource`, `FindTransaction`, `NewStoreSource`, `SegmentWriter`, `NewDirSink`, `NewReaderSource`, `Retention`, `ExportJSONL`); phase 6 split into 6a, 6b and 6c (G5-166) and 6b into 6b1, 6b2 and 6b3 (G5-173), 6b1 and 6b2 (conformance corpus, parts 1 and 2) done; 6b3, 6c and phases 7 and 8 pending.
Implements: tracepack v2.30 (format 1.0) — `tracepack-format.md` [FMT], `tracepack-jsonl.md` [JSONL], `tracepack-corpus.md` [CORPUS], `tracepack-semantics.md` [SEM], `tracepack-storage.md` [STO] — and `tracepack-go.md`.
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
the go-secs conn-wrapper recorder, equipment-gateway integration, `eapconv`, the query service, its MariaDB catalog, the live-tail interface,
indexes in the footer (no longer planned, [OVW §6]), CLI `grep` / `tx`, and the redaction policy file format and key management.

## 2. Module and package layout

Nested module `github.com/arloliu/go-secs/tracepack` (own `go.mod` in `tracepack/`, tags `tracepack/vX.Y.Z`, G5-77), created in phase 0.

| Package | Content | Public? |
|---|---|---|
| `tracepack` | `Record`, enums, `PackMeta`, `Writer`, `Reader`, `Filter`, `Verify`, `Repair`, `Recover` (deferred), `Merge`, `MergeIterate`, `FindTransaction`, `NewStoreSource`, `ExportJSONL`, `Extract`, `RedactionPolicy` | yes |
| `tracepack/classify` | go-secs-based `decode_status` classifier | yes |
| `tracepack/internal/format` | byte layouts: file header, envelope, record header, trailer, F-1, F-2; CRC; UUID byte order | no |
| `tracepack/internal/tlv` | TLV entry encoding, value types, tag registries, validation | no |
| `tracepack/internal/codec` | `none`, `zstd` | no |
| `tracepack/internal/corpus` | conformance corpus: the [CORPUS] schemas, byte surgery on packs, the vector recipes, the generator and the conformance test; imports `tracepack` and the other internal packages, never `classify` or go-secs | no |
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
| 5b — MergeIterate | done |
| 5c1 — PackSource, FindTransaction | done |
| 5c2 — store-backed PackSource, listing views | done |
| 5c3 — retention | done |
| 5d — segment writer, local source | done |
| 6a — JSONL export | done |
| 6b1 — conformance corpus, part 1 | done |
| 6b2 — conformance corpus, part 2: reads over several packs, transaction lookups | done |
| 6b3 — conformance corpus, part 3: store vectors | pending |
| 6c — CLI | pending |
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
- `seq_start` defect (done, spec v2.19, G5-115): `Verify` reports a first validated block whose first seq is not the pack's `seq_start`,
  and `Repair` corrects it in a `finalized-consistent` pack, which `Merge` refuses as an input.

Tests: truncate a pack at every byte offset and check the outcome;
the repair pack keeps the validated prefix byte-identical.

Done when: every truncation point yields the expected outcome and no validated record is lost.

Deferred until a local-spool recorder is planned (G5-91):
`Recover` (spool file + liveness anchor → segment of the original capture, same seqs, plus a `stop-unclean` boundary record with `gap_start` / `gap_end` per [STO §4]);
the writer-side spool durability helpers (liveness anchor rewritten every F, at spool rotation and after a durable clock-step, [STO §4]; segment roll at an hour change);
and their vectors (the durable clock-step → size roll → empty spool → crash vector).
The vector of a nonzero `capture_origin_mono_ns`, listed here before spec v2.27, is in 6b1 (`sem-capture-origin-mono`, [CORPUS §9]).

### Phase 5 — Merge, MergeIterate, FindTransaction

Phase 5 is delivered in steps: 5a (`ActiveView`, `Merge` and the Writer seams they need), 5b (`MergeIterate`),
and 5c in three (G5-120): 5c1 (`PackSource`, `FindTransaction`), 5c2 (store-backed `PackSource`, listing views, G5-139) and 5c3 (retention).

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

#### 5b — MergeIterate (done)

- Spec v2.20 first (G5-116..G5-119): overlap clusters across the packs read, excluded only whole, a pre-read of blocks without an F-2 entry,
  every selected version of a conflict yielded and marked, capture order interleaved block by block ([SEM §7.4]).
- `MergeIterate`: capture order (streaming) and time order (watermark over the clusters' `ts_min`), with held block bytes and listed conflicts bounded by caller limits (`tracepack-go.md` §3).

Tests: time-order iteration with backward timestamps within and across blocks and packs; the multi-pack read vectors of [SEM §9].

Done (2026-10-02): every test above passes, the [SEM §9] vector of an extract beside its source excepted:
it waits for the extract writer (phase 7),
and a test that zeroes the message text of two records in a second pack of the capture, as an extract masks them, stands in for it.
Every read of generated multi-pack inputs, walked packs, failed blocks, duplicates and conflicts included,
equals a reference built from the stored rows and payloads of every block, in both orders:
the records yielded with their packs, blocks and conflict marks, the conflicts listed, the defects, the full-block reads of each block and the footer errors.
The read counts hold for every kind of block:
an indexed block is read once when its cluster is kept and not at all when it is excluded;
a walked block is read before the read, and once more only when its cluster is kept and that first read succeeded;
a block over `MaxBlockLen` or with dimensions that cannot be consistent is never read.
The bytes held never exceed `MaxHeldBytes`, a reservation over it fails with `ErrReadLimit` before anything is allocated,
and nothing is held after any return, success, `fn` error, cancellation, limit or `ReadAt` failure.
`FuzzMergeIterate` and `FuzzMergeIterateGenerated` ran 10 minutes each without a failure.
The implementation settled what the text above leaves open:
- A held block's buffers are allocated for it alone, at the exact sizes its F-2 entry or envelope states, and dropped when its last owner lets go;
  nothing is pooled.
  The decoded body is reserved for every block but allocated only for a compressed one: a block stored with codec `none` is used in place.
- A block is dropped as soon as it is read, as `Merge` drops one,
  when its bytes, envelope and on-disk body, equal those of an open block of its cluster with the same first seq;
  its pack is added to that block's holders, and it holds no memory.
- The defects follow the order of the read:
  those of each pack's `Open` and its coverage entries for the query, in reader order;
  then those of the walked blocks' first reads, in reader then file order;
  then those of the clusters' reads, in the order the blocks are loaded.
- Captures rank by `capture_id` as raw UUID bytes, the order of their text form too, in both orders and for the order streams are started in.
- `ctx` is checked after a seq group is gathered, whenever the count of copies gathered reaches or passes a multiple of 4096, every copy counted, identical ones included;
  a group taking the count across several multiples gets one check;
  a block dropped as a duplicate adds none.
  Calls of `fn` are counted apart, and `ctx` is checked before the 4096th call, the 8192nd and so on.

#### 5c1 — PackSource, FindTransaction (done)

- Spec v2.21 first (G5-120..G5-136): [SEM §7.2]'s lookup from a primary — its definitions, closing records read and checked against the evidence (G5-135),
  conflicts and block-index disagreements making it `incomplete` (G5-132..G5-134), the comparison covering the scopes read (G5-136) —
  and [STO §5]'s observation of a lookup (G5-128).
- `PackSource` and `Observation`: one observation per lookup, fixed before any record is read; scopes, per-capture evidence and barriers from it.
  A source for tests only, in the package's tests: views from `ActiveView`, per-capture evidence as a registration history independent of the packs present.
- `FindTransaction(ctx, src, TxKey{Capture, Seq, Hour}, TxOptions)`: the primary's scope and the next ones up to `MaxScopes`, each read once by one `MergeIterate` in capture order;
  `TxResult` with the outcome, the derived key, every kept version and its roles, every reason absence could not be established with its hours and pack_id, the scopes read and the conflicts.
  An unexported conflict-reservation seam in `MergeIterate` lets one budget cover the conflicts of every read and those across them.

Tests: the [SEM §9] lookup vectors; the barrier and closure effects of the [STO §8] end-evidence service vectors through the test source;
an observation mutated after it was fixed; a reference built from the stored rows of the scopes read, and a whole-capture safety check that every `unmatched` lookup read every seq of its window.

Done (2026-10-03): the tests above pass.
The four [STO §8] end-evidence vectors (a cold recovered boundary, a clean end evicted, a bounded unclean end evicted, an entirely cold capture) are unit tests,
and so is a source changed during `Observe` and before each `Scope` call, whose lookup answers from what `Observe` fixed.
The [SEM §9] lookup vectors each have a named unit test, repeated System Bytes and transaction keys, a refused socket between epochs and transactions across packs included; they also arise in the generated captures.
Every record of 96 generated captures is looked up
(several epochs, backward clock steps, replies across hours, conflicts within and across hours, walked, unevaluated, damaged and staged packs, coverage, false and partial evidence, barriers),
and each result equals one computed independently from the stored rows of the scopes read:
the outcome, the key, the window end, every kept version with its roles and flags, the gaps, the scopes read, the conflicts and the footer errors.
Each result also holds over the capture as written, versions in hours not read excluded (G5-136):
an `unmatched` lookup read a bound the capture confirms and every seq of the window below it, and nothing the capture holds prevents absence;
a `matched` lookup without a gap found the capture's one valid match.
That check rejects a lookup whose seq-gap detection is disabled (a window seq moved into the unread hour before, a dropped annotation, a seq only inside a block's F-2 range)
and one that takes an evidence closure as a bound (a valid footer whose `close_seq` names an annotation).
After every lookup, failed ones included, the state recounted from what the lookup holds equals what it charged against `MaxStateBytes`.
`FuzzFindTransaction` and `FuzzFindTransactionGenerated` passed 60-second runs under `-race` (113,421 and 8,385 executions).
`make fuzz-tracepack` now names each target by an anchored pattern, since `FuzzMerge` also matched `FuzzMergeIterate` and the loop stopped there,
and bounds the minimization of a new input to one second (`-fuzzminimizetime=1s`): with inputs of several KiB, the default minute left the workers minimizing instead of executing.
The owner settled two points during implementation, as spec v2.21 amendments:
a stop-unclean interval whose `gap_start` exceeds its `gap_end` meets every time range (G5-137),
and a closure at a seq the lookup read is contradicted only when no version there is the closing event it names, a conflicted closing seq being reported as the conflict alone (G5-138).
The implementation settled what the text above leaves open:
- A primary that fails with `ErrNotPrimary` or has no key ends the lookup after its scope, without evaluation: the result is decided.
- Copies of the primary that a block disagreeing with its F-2 entry splits across overlap clusters arrive uncompared,
  so the lookup compares them byte for byte, as stored, `record_flags` included:
  identical ones are one version, and different ones are a conflict (`conflict` and `no-key`), counted once against `MaxConflicts` unless `MergeIterate` listed it.
  A conflict at the primary that `MergeIterate` listed stands whatever the lookup's comparison finds.
- Roles are given by the bytes, a conflicted version's too.
  A closing record bounds the window only without a conflict, within its read or across reads;
  a same-key primary bounds it, and the first possible same-key primary limits eligibility, whether or not it conflicts,
  since any conflict at or above the primary makes the outcome `incomplete`.
- `Decidable` is availability alone, independent of the window, and an `unavailable` gap is listed per kept version, not per seq.
- A version without a role that is its seq's only version in a read is not copied,
  so under an index gap a role-less copy that precedes a copy with a role is not listed; the outcome is `incomplete` anyway.
- The epoch barrier: every `stop-unclean` boundary of the capture's evidence is a barrier,
  unless the lookup read an unconflicted closing record of the primary's epoch above the primary, any one, not only the bound.
  A `stop-unclean` of the primary's capture whose gap meets the hours read is listed both as an epoch and as a time barrier.
- An empty window (`e = p + 1`) has a `coverage` gap only for an inverted coverage entry.
- A `close_seq` of the primary's epoch is borne out by a socket-close or a clean `stop` of the epoch at that seq, the records that end an epoch ([FMT §10]);
  a clean `stop` entry, by a capture-boundary record of kind `stop` of the same epoch.
  An evidence closure at or below the primary is a contradiction, a clean `stop` below the primary included, since the capture ended before it.
  A repeated evidence entry gives a repeated gap.
- The seqs that the primary's scope yields above the primary before any version of it, which only an index gap allows,
  are not checked against the evidence's claims, never bound the window and never exempt the epoch barrier,
  and a capture-boundary record among them that the evidence lacks is not listed; the index gap makes the outcome `incomplete`.
- `TxKey.Seq` above 2^63 − 1 is an invalid query, so `p + 1` never overflows.
- `TxResult.Records` is sorted only when evaluation completes; beside an error it keeps the order of arrival.
- Gaps are listed per read, its mapped defects then its conflicts in arrival order,
  then the window gap, `unavailable`, `coverage`, the epoch and time barriers, `capture-boundary`, `ordering-uncertain`, `evidence` and `contradiction`.
- `MaxStateBytes` charges each item its Go size rounded up to 16 bytes, a record also its payload and header extension, so the charges differ between 64-bit and 32-bit platforms.
  `ctx` is checked every 4096 iterations of one counter that every loop of the lookup shares, and right before `Barriers`.
- `MergeIterate` lists a conflict through an unexported reservation when the lookup reads,
  so one `MaxConflicts` allowance covers the conflicts of every read and those between them; `MergeIterate` itself behaves as before.
- Generated captures hold no block that disagrees with its F-2 entry, since the reference reads each block by its entry,
  so `index` gaps and the seqs before a late primary are covered by unit tests only.

#### 5c2 — store-backed PackSource, listing views (done)

- Spec v2.22 first (G5-139..G5-148): the key encodings of [STO §3] (G5-141, G5-143) and the segment `seq_first` check (G5-147);
  in [STO §5], a listed segment that is gone fails the observation (G5-144), premise (i) confirmed by the catalog per scope (G5-139), a conflicted scope reported as such (G5-142);
  in [SEM §7.2], a conflicted scope not read (G5-142), a record outside its scope's hour (G5-146), the time range of the hours scheduled.
- `NewStoreSource(ObjectStore, Catalog, StoreSourceOptions)`, a `PackSource` (G5-139):
  indexed scopes from one catalog snapshot, each pack opened and checked against its descriptor;
  the others by a coherent observation ([STO §5]) — the commit listing repeated until two complete listings agree, then the `archive/` and `staging/` listings, each segment's head read for its hour, the packs opened and checked, `ActiveView` — confirmed not indexed after the scope's last read.
  Strict key parsing, pack validation, limits (`MaxCommitListings`, `MaxListPages`, `MaxObjects`, `MaxSourceBytes`), `OnExcluded`, cleanup on every failure.
- The catalog is the per-capture evidence provider (G5-123); `(*Reader).Stats()` (G5-131) and `AddPackEvidence`.
- `FindTransaction`: the `conflicted` gap (G5-142) and the `scope-breach` gap (G5-146).
- Not in 5c2: the searched scope and `Cold` reason in `Result` for `Iterate` and `MergeIterate`, which wait for a query over a `PackSource` (G5-140, pending, unscheduled).

Tests: the coherent-observation, listing-view and window-boundary vectors of [STO §8], through `FindTransaction` over a `StoreSource`, checked against an acquisition model built from the store's history.

Done (2026-10-04): the tests above pass.
The external post-implementation review took four rounds and ended merge-clean;
its fixes charge the listing's own state while it is held, charge each `PackInfo` before decoding it, check the context while a view's `PackInfo` are built,
and take the model's witness after a traversal returns.
`FuzzFindTransaction` (20,241,071 executions) and `FuzzFindTransactionGenerated` (339,115) passed 10-minute runs,
and so did `FuzzStoreSource` (8,607,709) after its harness kept late mutations within the coherent observation's premises.
G5-139..G5-146 were settled before implementation, in spec v2.22.
The owner settled two points during it, as spec v2.22 amendments:
a segment key's `seq_first` is not compared when damage hides the segment's first seq, the reads reporting the damage (G5-147),
and a key that cannot be built, or a listed or catalog key that does not parse, is an error wrapping an exported `ErrInvalidKey`,
which a failed `Observe` keeps (G5-148).
The [STO §8] vectors are unit tests through a `StoreSource`:
a paginated commit listing whose late writes land behind and ahead of the cursor,
so the second traversal differs and the view is never G1 plus the uncertain patch;
late commit objects and segments before, during and after the traversals;
a settled and an unsettled scope whose listing views equal the catalog's;
a late segment of a cold scope, alone and beside an indexed hour, and a lookup across the window boundary;
an observation fixed before an indexed scope is evicted, a reply registered or rejected, and the evidence and barriers changed;
supersession through listing views
(patch of a patch, an incomplete committed set, stale and uncommitted patches, a coverage-only repair, predecessors deleted in each order);
the confirmation failures (a scope indexed before its confirmation, a rebuild, lost history, another scope's token),
and a scope indexed after its own confirmation keeping its listing view;
listed packs deleted between listing and read, each failing with `ErrObjectNotFound`, and the retry that succeeds.
An acquisition model, independent of the source, derives from the store's and the catalog's logs of what they returned and when
each listed hour's witness interval between its two agreeing commit traversals,
the commit set and the packs present then, and the segments a later listing may add.
It checks every observation of 1000 generated schedules (300 under `-short`):
each scope's readers, the evidence, the barriers and the excluded packs;
every failure the schedule forces (an open or read of a gone object fails with `ErrObjectNotFound`);
and the lookup over the source against the lookup over a `memSource` replay of the model's observation, an indexed scope's view taken as given.
Non-vacuity: at least 40% of the schedules must succeed with a listed hour, and 10% each with a late commit object and with a late segment in a view;
a source whose every `Observe` fails passes the check but fails that requirement.
Of 300 schedules, the check rejects 187 under a source that takes one commit traversal, 170 under an empty listed view,
28 under a dropped committed patch, and 14 under a refused confirmation accepted;
the last two kills are structural, from the model's witness and confirmation rules.
The lookup's generated captures now hold conflicted scopes, indexed or not, conflicted primary scopes,
and packs breaching their scope, with a valid footer and walked, in the primary's scope too.
The observation-bounded check rejects 82 mutated lookups that read a conflicted scope,
8 that leave a breach's outcome complete, and 127 that ignore a breach below the primary.
After every `Observe`, failed ones included, the charges recomputed from what the observation keeps equal what it charged against `MaxSourceBytes`,
and a sufficiency test compares each cost function with the bytes its structure holds.
`FuzzStoreSource` mutates keys, pages, schedules, pack bytes and segment heads, and checks every observation against the model.
At chosen points of every test acquisition, what the listing holds is measured against its charges.
The implementation settled what the text above leaves open:
- Observe's order: the indexed packs opened; every listed hour's commit traversals; every listed hour's archive listing; the one staging listing;
  segment heads and full opens in key order; the archives hour by hour; `ActiveView` per listed hour; the confirmation per listed hour.
  A cancellation during the last confirmation is not observed.
- A segment head of an hour not observed gets the full metadata check of a listed pack (tool, capture, pack_id, role, period in one hour, generation),
  since the segment could belong to a listed hour.
  The head read takes `min(size, HeadWindow)` bytes, so a small pack's footer bytes are read but never decoded.
- An excluded pack is closed, then reported through `OnExcluded`, also when its close fails; that failure then fails `Observe`.
- `NewStoreSource` rejects `MaxCommitListings` 1 (zero or negative takes the default)
  and a non-zero `Reader.FooterOffset`, a hint for one object;
  its error for a tool no key can hold wraps `ErrInvalidKey`.
- Listing charges (listed hours, listed keys, opened packs, commit ids, object slots) are held while their state is, a higher peak for the same final charges;
  a scope's token is released at its confirmation, its descriptor once the observation is built.
- `Reader.Stats` keeps the F-5 count arrays as stored, trailing zeros included (the owner's go decision):
  trimming would not bound the worst case, since a non-zero element at a high index keeps the zeros before it and extra elements must be preserved;
  a used footer's F-5 is held for the Reader's lifetime, and each call copies it.
- `AddPackEvidence` orders boundaries by (seq, kind, ts, epoch, gap_start, gap_end) and then capture_id,
  so that entries differing only in capture are both kept and the result does not depend on the order of the folds.
- A `scope-breach` gap is listed per version, in discovery order, straight into the result,
  so it precedes the mapped gaps of its read (a missing primary ends the reads and drops that read's pending gaps).
- A conflicted scope that comes with readers fails the lookup.
- A barrier's `Kind` is not validated, and a token on an indexed scope is not rejected.
- Not covered by a test: the context checks while `ActiveView`'s pack list is matched to the readers, which no double can reach between iterations.

#### 5c3 — retention (done)

From spec v2.25 (G5-162..G5-165); the API is `tracepack-go.md` §3. Plan: the phase 5c3 retention plan, kept outside the repository (ready after plan reviews r1–r3).

- A `Retention` provider, given to `Iterate` and `MergeIterate` (`Query.Retention`), `FindTransaction` (`TxOptions.Retention`) and `NewStoreSource` (`StoreSourceOptions.Retention`).
- Reads check it before reading, at every block they emit, after an I/O failure and before success; a covered hour removed ends the read with `ErrRemoved` beside `Result.Removed`.
- The lookup fails with `ErrRemoved` when the primary's hour is removed; the store source reports a removed hour as `SourceScope.Removed`.
- Reader side only: deleting a removed hour's objects is the service's work.

Tests: the removed-outcome vector of [STO §8], driven by an event schedule independent of the implementation's checks.

Done (2026-10-08): the tests above pass.
`TestIterateRetentionScripted` and `TestMergeIterateRetentionScripted` move the boundary at a block read or a callback the seed chooses,
over generated reads with packs of three hours and extracts, and check from the logged events alone
that a check precedes each block's first callback, that nothing follows a check finding a removal, and what `Result.Removed` lists;
`FuzzIterate`, `FuzzMergeIterate` and `FuzzMergeIterateGenerated` take a schedule seed.
The store source and the lookup have unit tests for each check point, the four boundary configurations among them.
The implementation settled what the text above leaves open:
the lookup's retention state covers the primary's hour alone, and an earlier error of the lookup, `ErrNotPrimary` included, stands (spec v2.25 amended to say so);
`Observe` never fails with `ErrRemoved`, a removed hour showing only as `SourceScope.Removed`;
`Observe` samples the boundary when a listed hour reaches `MaxCommitListings`, so an hour removed during its last commit traversal is reported removed, not `ErrReadLimit`;
a gone object of a removed hour is skipped, but an error closing it still fails `Observe`;
an object's slot records its `Reader` and hour, so its `MaxSourceBytes` charge grew from 16 to 32 bytes on 64-bit platforms;
an indexed extract block the footer prunes still covers its F-2 hours;
and in `Iterate` a block whose records are all filtered out gets no check of its own, the check before success covering it.
Open: `MergeIterate` treats any load error wrapping `ErrReadLimit` as a refused reservation,
so a `ReadAt` error of the caller's that wraps `ErrReadLimit` skips the check after an I/O failure, which `Iterate` makes.

#### 5d — segment writer, local source (done)

From proposal P11, applied as spec v2.23 (G5-149..G5-155), with the contracts of spec v2.24 (G5-156); the API is `tracepack-go.md` §3.
Plan: the phase 5d writing plan, kept outside the repository (ready after plan reviews r1–r3).

- `Writer`: the scope-breach and period checks and `ErrScopeBreach`; `Validate` becomes `SkipValidation` (G5-152).
  A seam lets a `Writer` start from a clock anchor other than (`capture_origin_utc_ns`, 0), for `SegmentWriter` and later the phase 8 producer.
- `CaptureDescriptor` and its `Validate`; Godoc on every `PackMeta` field.
- `SegmentWriter`, the classifier interface (implemented by `tracepack/classify`), `SegmentSink`, `SegmentInfo` and `NewDirSink`.
- `NewReaderSource`, `HourOf`, `TxKeyOf`.
- `tracepack/README.md`, `example_test.go`, the package documentation grouped by audience (G5-155).
- `tracepack/CHANGELOG.md`: the breaking changes (`SkipValidation`, the `Writer` refusals) under Changed.

Tests: a record of another hour refused by `Append` and the Writer still usable; a period across an hour refused by `NewWriter`; a same-hour record outside the period accepted;
every roll trigger, a backward clock step across an hour and within one, seq continuity and one `capture_id` across segments, no empty segment;
the anchor carried across segments, so a step recorded in one segment is not detected again in the next;
`start` and `stop` boundaries, their epoch and the absent seq range; a failed sink aborting its segment once and leaving a seq gap, a failed `Commit` followed by exactly one `Abort` and then `Close` with no sink call;
`CaptureID()` right after construction equal to every segment's `capture_id`, seeding a successor descriptor's `PreviousCaptureID`;
`MaxSegmentBytes`: a small buffered record followed by one over the limit, a compressible record over it, one that triggers a clock-step;
`AppendFrame`: frames of 0–9 bytes and an undefined SType, a `log` capture refused, no `mono_ns` outside `capture-clock`; `Uncompressed` against the zstd default;
`DirSink`: invalid prefixes refused (`..`, empty components, `\`, `C:`), a listing while a segment is open and with an abandoned `.partial/` file, a key whose tool comes from `SegmentInfo`;
`DirSink` keys parsing as [STO §3] segment keys and `NewStoreSource` over a `DirSink` root through a test `ObjectStore`;
`NewReaderSource` with and without `Complete` (cold gaps and partial evidence, never `unmatched` without it), with `Complete` and another capture's reader without a used footer (partial),
a conflicted scope, readers of two tools rejected;
the examples run as tests.

Done when: a recorder writes a capture through `SegmentWriter` and `FindTransaction` over `NewReaderSource` finds its transactions,
and segments written across an hour boundary merge without `ErrMergeInput`.

Done (2026-10-06): the tests above pass, the four runnable examples among them, and so do both criteria:
`TestReaderSourceEndToEnd` records a capture through `SegmentWriter` into a `NewDirSink` directory, finds a reply across an hour boundary with `FindTransaction` over `NewReaderSource`,
and merges each hour's segments without `ErrMergeInput`.
`Merge` and `Repair` write the same bytes as before the `Writer` changes, which a digest test pins.
The implementation settled what the text above leaves open:
`AppendFrame` refuses an `unknown` capture method, which has no fidelity rule, as it refuses `log`;
`DefaultClockStepTolerance` (1 s), `DefaultFlushInterval` and `DefaultMaxSegmentBytes` are exported;
`CaptureDescriptor.Validate` also refuses a value the encoding would refuse, such as invalid UTF-8 or a `u64` above 2^63 − 1;
`NewSegmentWriter` refuses a classifier whose `MaxFrameLen` exceeds 2^63 − 1 or whose `Name` is empty;
and an `ActiveView` error other than a conflict fails `NewReaderSource` as it is, not wrapped in `ErrInvalidQuery`.
The owner settled two points during it, as spec v2.24 amendments:
`NewDirSink` creates files with mode 0640 and directories with mode 0750, and refuses a prefix whose first component is `.partial` (G5-157);
under `Complete`, the scope of each evidence reader is not indexed (G5-158).
[STO §4] no longer calls the capture-clock tags always-required, a wording fix of spec v2.24.

### Phase 6 — JSONL export, conformance corpus, CLI

Split into three sub-phases, each with its own plan, reviews and PR, in the order 6a → 6b → 6c, before phase 7 (G5-166).
6b is split again into 6b1, 6b2 and 6b3 (G5-173): 6c may start once 6b1 is done, and 6b3 may come after phase 7.

#### 6a — JSONL export (done)

- `ExportJSONL` in package `tracepack` (G5-168), writing the byte form of [JSONL]:
  64-bit integers as decimal strings (G5-167); the validated blocks of a damaged pack, without a summary line (G5-169);
  a reader budget that keeps a block out is an error (G5-171); lines written in bounded pieces (G5-172).
- TLV lists are walked over their raw bytes, so the export's own memory does not grow with a payload, a value or an entry list;
  a registry pin test binds the names of `tracepack-jsonl/1` to [JSONL].

Done when: exact-bytes tests written from [JSONL] pass, including damaged packs, unknown tags, enum values and bits, extension areas, invalid bodies and string escaping;
the export agrees with `Iterate` on records and `Result`, and with `verify` on body validity; the fuzz target runs clean.
No corpus files and no CLI in 6a.

Done (2026-10-09): the tests above pass.
`TestExportJSONL_RecordLines`, `TestJSONLHeaderLine_EveryKnownTag` and `TestExportJSONL_SpecExample` hold lines written by hand from [JSONL];
`TestExportJSONL_SameAsIterate`, `TestExportJSONL_ParseBackProperty` and `TestExportJSONL_TruncationSweep` compare the export with `Iterate` and with the record header bytes over damaged, random and truncated packs;
`TestJSONLValidBody_RandomAgreement` and `FuzzJSONLValidBody` compare body validity with `verify`'s;
`FuzzExportJSONL` covers every reader budget, and `ExampleExportJSONL` exports the [JSONL §10] record.
The implementation settled what the text above leaves open:
`Iterate` and `ExportJSONL` build their starting `Result` with one shared helper;
a footer or walk budget fails the export with the `Result` of `Open`'s defects and the coverage entries, no `ReasonLimit` defect among them,
and a block budget with the `Result` found before that block.
`FuzzExportJSONL` ran 10 minutes clean.
A damaged zstd stream can be described with different error text from one read to the next:
a pooled decoder's history decides whether the library's assembly or generic sequence decoder runs, and they word the same failure differently.
Whether a block decodes, and the bytes it decodes to, never vary, so tests compare defects by reason, block and offset, never by the codec's text;
the two inputs that showed it are regression seeds of `FuzzExportJSONL`.

#### 6b1 — conformance corpus, part 1 (done)

From spec v2.27 (G5-173..G5-183); the corpus is [CORPUS]. Plan: the phase 6b1 corpus plan, kept outside the repository (ready after plan reviews r1–r3).
- The [FMT §16] vectors except the redaction vectors (phase 7) and the two that need the result of a read over several packs (G5-182),
  the single-pack vectors of [SEM §9] (one per `decode_status` value, one per item-validity case of [CORPUS §7], a nonzero `capture_origin_mono_ns`),
  the query-vector format and every other JSON schema of [CORPUS §5], under `tracepack/testdata/corpus/`, shipped in the module zip, under 2,000,000 bytes (G5-177).
- Injectable clocks: `WriterOptions.Now`, passed down by `SegmentWriter` (G5-174), and `RepairOptions.Now` and `MergeOptions.Now` (G5-179), so the corpus regenerates byte-identically under pinned dependency versions (G5-175).
- `Repair` reports nothing to repair before any refusal (G5-183).
- `internal/corpus` (§2): schema conversions written by hand from [CORPUS]; `packedit`, byte surgery on packs, either raw (edits stored bytes in place, recomputes only the CRCs and lengths it names, keeps reserved bits, extension areas and reserved TLV bytes) or re-encoding where a recipe says so, every function error-returning;
  recipes whose expectations are written by hand from the spec, never from output, checked before any golden is written;
  `TestCorpus` regenerates and compares per [CORPUS §6.1] and runs the read path over the committed files;
  `go test ./internal/corpus -run '^TestCorpus$' -update` regenerates the corpus, also run by the Make target `corpus-tracepack`.
  Satellites: `classify/corpus_test.go` runs the classifier on every frame of `classify.json` (the corpus decides, §5 risks); `tracepack/corpus_writer_test.go` reproduces the I-2 injection vector with the validating `Writer`.
- Deterministic inputs: a fixed hour-aligned time base for every clock, a fixed `writer` and `classifier` name,
  and identifiers derived from the vector's id, with two exceptions:
  the vectors that differ only in codec, the truncation base packs included ([CORPUS §3]), derive theirs from one shared identity seed,
  so their identifiers and exports are equal; the UUID byte-order vector uses the literal UUID of `primitives.json`;
  the v0.1.0 sample copied from `testdata/v0.1.0-rows.tpk`; `manifest.json`'s `zstd_encoder` names the `klauspost/compress` module the generating test links, as `go list -m` names it from the package directory.

Done when: `make lint-tracepack`, `make test-tracepack` and `make test-tracepack-386` pass and `go fix -diff ./...` is clean;
`-update` produces no diff and the corpus regenerates byte-identically under the pinned `go.mod`;
`ExportJSONL`, `Verify`, `Iterate` and `Repair` match every golden; every bootstrap vector is rejected with its code; every query vector yields its results;
the classifier agrees with every case of `classify.json`; the corpus is under 2,000,000 bytes; the external post-implementation review is clean.
No CLI, redaction, multi-pack or transaction vectors in 6b1.

Done (2026-10-09): the tests above pass, and so do the criteria they check.
`TestCorpus` regenerates the 112 vectors byte for byte under the recorded `klauspost/compress` version,
reads every committed vector again with `ExportJSONL`, `Verify`, `Iterate` and `Repair`, rejects every bootstrap vector with its code and sweeps every cut of the truncation vectors;
it also checks the layout and the 2,000,000-byte limit, whose boundary `TestCheckSize` tests.
`TestGenerateDeterministic` and `TestGenerateFreshProcesses` generate the corpus twice, in one process and in two;
`TestCompareCorpus` and `TestEncoderProvenance` cover regeneration under another encoder ([CORPUS §6.1]).
`TestRecipeIDsAreCatalogued` binds the vector ids to the catalogue of [CORPUS §9.4],
and `TestRecipesHaveExpectations` and `TestGenerateFailsOnDisagreement` show that every expectation is checked before any golden is written.
`TestCorpusClassification` agrees with every case of `classify.json`, `TestCorpusValidatingWriter` reproduces the I-2 injection vector,
and the committed packs seed `FuzzExportJSONL`.
`TestWriterClock`, `TestSegmentWriterClock`, `TestMergeClock` and `TestRepairClock` cover the clocks (G5-174, G5-179), and `TestRepairRefusals` the order of G5-183.
The corpus holds 400 files and about 669,000 bytes; its README keeps a size budget per group.
The vectors raised one question, which the owner decided (G5-184, spec v2.28):
a CRC-valid envelope with a field out of range stops the forward walk without a valid footer.
The implementation settled what the text above leaves open:
a failed block's envelope `record_count` counts toward the trailer comparison of the walked blocks ([FMT §13]);
the records of a log conversion without connect lines are in epoch 0 ([STO §7]);
and a `build-rejected` record is an `unparsed-entry` annotation holding the entry's text, since [SEM §2] names no kind for it.

#### 6b2 — conformance corpus, part 2 (done)

From spec v2.30; the corpus is [CORPUS], schema `tracepack-corpus/2`.
Plan: the phase 6b2 corpus plan, kept outside the repository (ready after plan reviews r1–r3); its rules await the owner's ratification.
- The rest of [SEM §9]: reads over several packs ([SEM §7.4], `MergeIterate`) and transaction lookups ([SEM §7.2], `FindTransaction` over `NewReaderSource`),
  in vectors of class `multi-pack` ([CORPUS §2]) whose results are `reads.json` and `lookups.json` ([CORPUS §5.10], [CORPUS §5.11]);
  and the two [FMT §16] vectors moved here (G5-182): a payload-only identity conflict between two packs (`multi-payload-only-conflict`) and transaction candidate selection (`tx-candidate-selection`).
- A lookup's source is a set of the vector's packs, given to `NewReaderSource` ([CORPUS §8]); store snapshots, catalogs and listings stay in 6b3.
- Every pack is codec `none`.
  A `multi-pack` vector has no export, for the corpus's size: the single-pack vectors cover the export, and each pack's `verify` report is a golden.
- `internal/corpus`: multi-pack recipes and layout, a clock that can place a pack in any hour, and the read and lookup harness,
  which compares every record a read or lookup returns with the stored record its pack and block name,
  and turns `FindTransaction`'s gaps into the canonical facts of [CORPUS §5.11];
  the size budget of each vector group and root file becomes a ceiling a test enforces.
- Not visible in a result, so tests of `MergeIterate` rather than vectors ([CORPUS §9.2]):
  a cluster excluded whole, not read and its conflict not listed (`TestRunCaptureExcluded`), and a walked block read once only (`TestMergeIterateReadCounts`).
- The read of an extract beside its source moves to phase 7.

Done when: `make lint-tracepack`, `make test-tracepack` and `make test-tracepack-386` pass and `go fix -diff ./...` is clean;
`-update` produces no diff and the corpus regenerates byte-identically under the pinned `go.mod`, in one process and in two;
every pack of every `multi-pack` vector matches its `.verify.json`; `MergeIterate` matches every read of `reads.json`, and every record it returns matches the stored record it names;
`FindTransaction` over `NewReaderSource` matches every lookup of `lookups.json`, its gaps turned into facts, with the same record check;
every [SEM §9] clause and the two [FMT §16] clauses of G5-182 map to a vector, a test of `MergeIterate` or phase 7, and every catalogued id is in the manifest and the reverse;
the corpus is under 2,000,000 bytes and every group and root file within its enforced ceiling;
two independent Python checkers, kept outside the repository and written from the specification alone, without the goldens or the Go code,
one for reads and one for lookups, agree with every committed golden,
the lookups checker on the whole fact set of every lookup;
the external post-implementation review is clean.
No store, catalog, retention, redaction or zstd vector in 6b2.

Done (2026-10-10): the tests above pass, and so do the criteria they check.
The corpus gains 32 vectors of class `multi-pack`, every pack codec `none`:

| Group | Vectors | Packs | Reads or lookups | Files | Bytes |
|---|---:|---:|---:|---:|---:|
| `multi-` | 13 | 35 | 25 reads | 83 | 79,053 |
| `tx-` | 19 | 57 | 53 lookups | 133 | 192,141 |
| Both | 32 | 92 | 78 | 216 | 271,194 |

The whole corpus holds 144 vectors in 616 files and about 964,000 bytes, `manifest.json` 82,830 of them, under the 2,000,000-byte limit.
`TestCorpus` regenerates every vector byte for byte, opens every `pack-<n>.tpk` against its `.verify.json`,
runs every read with `MergeIterate` and every lookup with `FindTransaction` over `NewReaderSource`,
and checks every returned record against the stored record its pack and block name.
`TestGenerateDeterministic` and `TestGenerateFreshProcesses` regenerate the corpus in one process and in two;
`TestRecipeIDsAreCatalogued` binds the `multi-` and `tx-` ids to [CORPUS §9.4];
`TestGroupBudgets` enforces the ceiling of every group and root file, and requires the corpus README's budget table to state them;
`TestMultiRecipesAreChecked` and `TestLookupRecipesAreChecked` show that every read and lookup expectation is checked before any golden is written,
and `TestLookupViewOrder` covers the view order of [CORPUS §8], a patch replacing a segment or the generation's member included.
Two Python checkers, written from the specification alone, agree with all 25 reads and all 53 lookups, the latter on each lookup's whole fact set.
Open questions, which no vector depends on:
- Records the primary's scope read yields before the primary, through a block disagreeing with its F-2 entry, are neither classified nor kept by `FindTransaction`,
  while [SEM §7.2] lets them bound the window and checks a closure claim at their seq.
  The outcome is `incomplete` either way, from the block's `index` fact;
  either [SEM §7.2] states that such records are not classified, or `FindTransaction` classifies them.
- `FindTransaction` picks the clean `stop` that bounds the window by seq and kind, not by epoch,
  so an evidence `stop` claim of the primary's epoch at that seq gives no `capture-boundary` fact when a `stop` of another epoch bounds the window there,
  where [CORPUS §5.11] and [SEM §7.2] identify a capture-boundary by its epoch too.
  The outcome is `incomplete` either way, and only a nonconforming footer reaches the case;
  comparing the epoch too, then adding the case as a vector, would close it.
- The rules of the corpus, part 2, stated in [CORPUS] v2.30, await the owner's ratification.

#### 6b3 — conformance corpus, part 3

- The corpus vectors of [STO §8] and a store snapshot format added to [CORPUS]; it may come after phase 7 (G5-173).
  The recovery vectors among them stay deferred with `Recover` (G5-91); the Service vectors of [STO §8] are outside the corpus.

Done when (provisional, set when 6b3 is planned): the store-backed source, `ActiveView`, `Merge` and retention match every golden, under the same regeneration and size rules as 6b1.

#### 6c — CLI

- `list`, `stats`, `dump --sml | --jsonl`, `verify [--repair]`, `merge`, and `recover` once `Recover` is planned (G5-91), on local paths (G5-170).
  `s3://` sources are deferred until the query service or another design decides where the object-store dependency lives;
  a storage-backed command then ends an hour that becomes removed with the removed outcome ([STO §5] Retention).

Starts once 6b1 is done (G5-173).

Done when: `dump --jsonl` matches every JSONL golden of the corpus, rejects every vector rejected at bootstrap, and the CLI works on the corpus.

### Phase 7 — Extract and redaction

- Reader prerequisite: a redaction entry with a recoverable defect ([SEM §8]), such as a `masked_ranges` array whose length is not a multiple of 8 or whose element count is odd,
  must not fail `Open`; the reader reports it and treats the records concerned as wholly masked.
  Until then such a pack is rejected at bootstrap by the Go reader, and the corpus holds no rejection golden for it ([JSONL §8]).
- E5 item walker with byte offsets and the item-validity predicate of [SEM §3], its format-22 rule included (G5-181), checked against the checklist of [CORPUS §7].
- Policy compilation: S/F, `*` paths, domains (1–65535 bytes), annotation kinds; invalid patterns and domains rejected.
- Two-pass `Extract`: plan every mask and digest (captured message text, total path resolution, no-content and whole-text rules), then write header, pack metadata and masked blocks;
  footer from the masked blocks; masked inputs rejected before any output.
- HMAC-SHA-256 with the corpus test keys; `redaction_policy` and `redaction` entries.
- Reader: entries parsed and structurally checked at `Open`, validated per record at read time, wholly masked records reported.
- `dump --sml` rendering of masked content.

Tests: the redaction vectors of [FMT §16], generated with their goldens and expected reports by the phase-6 corpus generator extended here,
with the bootstrap vector of a missing required nested tag, which only the `redaction_policy` and `redaction` registries have ([CORPUS §9]),
and the [SEM §9] read of an extract beside its source, each masked record a `conflict` (a `multi-pack` vector with `reads.json`, [CORPUS §9.2]);
a sweep over the corpus asserting, for every record, that each target byte is zero, each other byte equals the source,
the payload length and every record-header field except `quality.redacted` are unchanged, and `Verify` reports exactly the source's defects and no new one.

Done when: every redaction vector reproduces its bytes, entries and digests exactly, and the sweep passes.

### Phase 8 — Producer API for a durable-bus capture

A recorder prototype built on go-secs v2.6.0 (2026-09-28) wrote valid packs through `Writer`,
but a producer that publishes one record per bus message ([STO §4], proposal P8) cannot be written with the module as it is:

- No exported encoding of one record outside a block:
  `canonicalHeader` and the record header layout are unexported, so the bus message of P8 §2.1 has no implementation to share between producer and consumer.
- No encoding of a capture descriptor:
  phase 5d adds the `CaptureDescriptor` type (G5-153), but `PackMeta.MarshalBinary` validates the whole pack metadata,
  including values the consumer owns (`scope_generation` among them), so a producer cannot yet encode only the entries it owns.
- Clock-step detection is tied to seq assignment:
  `DetectClockSteps` requires `AssignSeq`, but a durable-bus producer assigns seq and detects clock steps itself ([SEM §4]),
  so it needs the detector on its own.
- Resolved by spec v2.23: a Go producer writes `capture_origin_mono_ns` = 0 (G5-151),
  and a `stop` boundary carries no seq range, so it need not name its seq in advance (G5-149).
- `classify.Name` falls back to `go-secs/unknown` when go-secs is replaced by a local path, so a producer sets the classifier name explicitly.
- `Writer.Append` copies the payload a second time after the producer's own copy of a wire frame.

The record encoding, the descriptor encoding and the stand-alone detector depend on proposal P8's outcome and are designed with it;
the classifier name and the second payload copy are small fixes, and phase 5d's anchor seam serves the detector.
Tests and done criteria are set when P8 is decided.

## 4. Cross-cutting requirements

- Performance targets (checked with benchmarks, not tuned before measurement): writer ≥ 50 MB/s uncompressed per core with zstd level 3;
  merge throughput measured with and without coalescing; reader bootstrap allocations independent of file size, apart from the pack metadata (an extract's redaction entries).
- Memory: the writer holds at most one block; the reader holds at most the pack metadata and the footer plus one decoded block per concurrent iterator,
  except `MergeIterate`, which holds the blocks of its open overlap clusters and of the records it has yet to yield, bounded in bytes by a caller limit (`tracepack-go.md` §3).
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
| zstd output differs across library versions | the committed goldens are always read strictly, which no encoder affects; a regenerated zstd vector is compared byte for byte under the `klauspost/compress` version the manifest records, and by decoded equivalence under another, its offset-bearing goldens then skipped with a note that `-update` is due (G5-180, [CORPUS §6.1]); damaged zstd blocks are hand-built frames; every vector uses codec `none` unless the codec is its subject. If zstd output differs across GOARCH under one version, the rule extends to a GOARCH other than the generating one |
