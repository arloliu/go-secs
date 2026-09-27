# tracepack — decision log

Status: current (2026-09-27)
Owner decisions for tracepack; append-only under `.agents/rules/450-doc-lifecycle.md`.
Entries up to G5-79 were recorded in the design notes before the repository became the source of truth;
they are copied here verbatim under their original headings, and later entries are added only here.
G3 and G4 entries come from the design grilling rounds, R3 entries from the owner's answers after review round 3, G5 entries from the tracepack design sessions.
Decisions that concern only the Virtual Equipment program stay in its design notes, as do the files some entries name.

## 2026-09-26 grilling round 3 (owner accepted all recommendations)

- G3-20 TAP log (assumed until sample seen): SML text; has direction, µs timestamps, SystemBytes,
  DeviceID, W-bit; has transport events (connect, Select, T3 timeout).
- G3-21 Query priorities: index natively supports tool+time range, S/F filter, SystemBytes transaction
  lookup, transport events; optional per-blob secondary index for CEID/ALID/RPTID/SVID; full-text
  SML and cross-tool aggregation are offline scans.
- G3-PLACE Data-pack format + codec live in go-secs as a nested module (own go.mod), e.g.
  `github.com/arloliu/go-secs/tracepack`; reference CLI in the same nested module
  (`tracepack/cmd/tracepack`), exempt from go-secs "no binary" rule. VE repo holds the TAP converter.
- G3-23 Blob scope: per tool per hour, roll at 256 MB uncompressed; S3 key `tool/YYYY/MM/DD/HH/seq.pack`.
- G3-24 Content: raw frame bytes as payload + fixed-width header fields (ts, dir, S/F/W, SystemBytes,
  DeviceID, len, decode status) for indexing; undecodable frames stored with failure reason.
- G3-25 Container: custom simple format (magic, version, zstd seekable frames, footer block index);
  pure Go, no CGO; supports S3 range reads. Parquet export tool is a possible later add-on.
- G3-26 No redaction by default; format reserves a redaction marker; retention via S3 lifecycle.
  (2026-09-27: the retention part is superseded by G5-56 and G5-57 — lifecycle rules only transition `archive/` objects, retention is executed by the service;
  redaction is designed by G5-66..G5-75.)
- G3-27 Timestamps: native capture = UTC wall + monotonic delta; converted = UTC with source tz and
  "converted-from-TAP" recorded in pack header.

## 2026-09-26 grilling round 4 (owner accepted all recommendations) — frontier empty

- G4-28 Evidence grades: `reconstructed` (from TAP SML via converter) is accepted for semantic-layer
  learning (catalog, reply rules, timing, semantic quirks); transport-layer and malformed-frame
  quirks require `observed`. Pack header field `evidence: observed | reconstructed`.
  Owner note: raw-byte capture can be added inside eqp-hub — either in tap_nats or as a new
  `secs-recorder` device on a shadow channel (gRPC etc.). Caveat recorded: a shadow-channel recorder
  sees decoded messages (byte-exact re-serialisation for well-formed frames only); truly observed
  malformed frames need a conn-level tap (go-secs WithListener wrapper). Both writers emit tracepack.
- G4-29 Native recorder writes the same tracepack format with evidence=observed. Confirmed.

## 2026-09-26 after review round 3 (owner)

- R3-4 tracepack module path: `github.com/arloliu/go-secs/v2/tracepack` (owner decision;
  follows the go-secs main module major version).
  (2026-09-27: superseded by G5-77.)
- R3-5 Capture identity / publisher / corrections / recovery: owner unsure → design defaults:
  capture id = recorder-assigned UUID per (tool, recorder process start), carried across rolls
  in the pack header; v1 = single publisher per tool; corrections are additive packs referencing
  the superseded pack id, never overwrites; v1 recovery guarantees the validated prefix and
  reports incompleteness. All revisitable.
- R3-6 Pilot tools and TAP sample: provided later. **Owner emphasis: the current goal is to
  clarify the HIGH-LEVEL design direction, not to implement.** Byte-level grammar, exact
  algorithms and test vectors belong to a later implementation-spec phase; the design docs
  should state contracts and invariants, and explicitly defer encodings.

## 2026-09-26 tracepack v2.3 (owner agreed)

- G5-30 tracepack is a language-neutral, long-term archival format: the file-format part is defined only by
  SEMI E37 / E5 and the spec text; go-secs citations are informative (Annex G); a conformance corpus
  (golden `.tpk` + canonical JSONL + verify reports) is the executable arbiter.
- G5-31 Portable encoding rules fixed now (not deferred): integers ≤ 2^63−1, UUID in RFC 9562 byte order,
  POSIX ns time, UTF-8 strings, CRC-32/ISO-HDLC, index hashes are spec'd algorithms with test vectors.
- G5-32 Codec field per block and for the footer: `0=none`, `1=zstd` (RFC 8878, no dictionary); readers
  MUST support both; writers default to zstd; a new codec is a major bump. Deflate and LZ4 rejected.
- G5-33 Footer and trailer fields defined at the semantic level (footer F-1..F-5 with a fixed-size block
  index array; trailer with footer codec, record count and its own CRC).
- G5-34 Trailer stays at the end of the file; index-at-front rejected (footer is variable; would break
  streaming write, crash safety and I-5). One-round bootstrap via manifest footer location or a
  speculative tail read; small packs are read whole.
- G5-35 Capture model generalised: per-record `fidelity` (wire-exact / re-encoded / reconstructed /
  synthesized / not-applicable) replaces `evidence` + `provenance`; pack-level `capture_method`
  (raw-stream / decoded-message / log / generator), `vantage` (host / equipment / intermediary /
  network / none) and a free-text `recorder`. Product names never appear in enums.
- G5-36 Byte layouts of the fixed structures (file header 80 B, block envelope 40 B, record header 56 B,
  trailer 64 B), the TLV encoding and all enum registries are fixed in the spec now (partially
  reverses R3-6); footer and index internals stay deferred until the pilot sample exists, because
  they are data-dependent and derivable (I-3).
- G5-37 Documents split: `tracepack-spec.md` is the language-agnostic wire spec; `tracepack-go.md` is
  the Go reference implementation; change history lives in `tracepack-spec-changelog.md`.

## 2026-09-26 tracepack v2.5 — traffic log service scenario (owner agreed)

- G5-38 Record identity is (capture_id, seq) with capture-scoped seq; merges never renumber; duplicates byte-identical.
- G5-39 Blocks are hour-aligned (one UTC hour per block) so hourly merges copy blocks without decoding.
- G5-40 Footer layout fixed now (F-1/F-2/F-5, TLV-extensible F-3); only data-dependent index structures deferred.
- G5-41 Stored quality bits are writer-local; effective (cross-record) quality is computed at query time.
- G5-42 Pack roles segment / archive / extract / repair / correction with compaction lineage; restart = new capture
  linked by recorder_instance_id + previous_capture_id; crash recovery closes the old capture with stop-unclean.
- G5-43 Storage profile: staging segments (5–10 min flush) → hourly archive per (capture, hour); unique keys; catalog
  contract (service uses MariaDB) replaces manifest.json; one recorder per tool; scale target ~1500 tools × 10 msg/s,
  ~1 month retention (TBD). Download formats: native tpk or JSONL/SML.
- G5-44 Optional troubleshooting metadata tags; UUIDv7 recommended for pack_id / capture_id.

## 2026-09-26 tracepack v2.5 review consensus (Codex gpt-6-astra, 9 rounds; round 1 ran on gpt-5.6-terra)

- G5-45 Every pack except an extract has one scope (capture, UTC hour); segments roll at hour changes.
- G5-46 Active view = highest-ranked committed complete generation, ranked by (publisher_epoch, scope_generation),
  plus uncommitted-to-generation gen-0 packs outside its cumulative compacted_from, plus committed patches whose
  patch_base is that generation. Repairs/corrections are generation-0 patches; only merges create generations.
- G5-47 Publisher epochs fenced by fence objects; admissions committed by commit objects (commit point) with
  roll-forward / cancel-by-new-generation recovery; deletion eligibility separate from view exclusion.
- G5-48 Recorder durability contract: flush interval F, liveness anchor, fixed clock anchor with clock-step events;
  stop-unclean gap bounds from monotonic times.

## 2026-09-26 tracepack v2.6 — block body split (proposal P1, Codex gpt-6-astra, 7 rounds; owner agreed)

- G5-49 The decoded block body is a header section followed by a payload section, under one codec frame;
  the 56-byte record header layout is unchanged.
- G5-50 `record_header_len` moves into the block envelope (offset 6) and the F-2 entry (offset 76);
  pack metadata tag 0x0014 is retired and its number is not reused.
- G5-51 Queries run in provisional or authoritative mode; results that relied on unvalidated header copies carry `header-validated`,
  reported next to `incomplete` and `conflict`, never instead of them.
- G5-52 Block attestation is kept: tag 0x0030 `blocks_validated`, committed before the blocks, each block validated before it is written;
  attested packs serve authoritative queries through header copies and indexes.
- G5-53 Scope rule: attestation never changes what a merge copies, where members end, what is published or what becomes deletable.
- G5-54 Storage handling of defective merge inputs is split out as proposal P2 (after review round 6);
  the owner deferred P2 to after the pilot when scheduling the P1 application (2026-09-26).
- G5-55 No per-record extension area; annotation records carry per-record metadata instead.

## 2026-09-26 cold storage and catalog window (question Q2, proposal P3; owner agreed)

- G5-56 No daily rollup tier: hourly archive objects stay; MinIO ILM only transitions `archive/` objects to a cold tier.
  Lifecycle rules never expire objects and never touch `staging/`, `commit/` or `fence/`.
- G5-57 Retention 6 months, executed by the service's deleting component (packs, then commit objects, then catalog entries);
  catalog window 35 days. Both are service parameters. Supersedes the "~1 month retention (TBD)" of G5-43.
- G5-58 Commit objects are deleted only after every pack they commit; the highest-numbered fence object is never deleted;
  a new publisher epoch also exceeds every `publisher_epoch` the catalog holds (option O1).
- G5-59 Outside the catalog window, readers compute listing views (staging included);
  cold results are `incomplete` with reason `cold`, and cold transaction lookups never return `unmatched` (no per-capture coverage beyond the window).
- G5-60 No admissions for scopes outside the catalog window; designing them is split out as proposal P4 and deferred.
- G5-61 Barrier evidence with a window (Q6, option a) — deferred to P5 by G5-65: a capture whose end cannot be established from indexed scopes but has a later linked capture
  is treated as ended `stop-unclean` with an unbounded gap interval; without a later linked capture it is `open`. No per-capture barrier rows beyond a rebuild.
- G5-62 Successor discovery (Q7, option a) — deferred to P5 by G5-65: a recorder writes a capture registry object `captures/<tool_id>/<capture_id>/<previous_capture_id>`
  (content irrelevant) before uploading the capture's first segment; registry objects are never deleted.
  A rebuild lists `captures/<tool_id>/` so "no later linked capture" (G5-61) means actual absence, not absence from the window.
- G5-63 Capture end evidence (Q8, option a) — deferred to P5 by G5-65: when a capture ends with a `stop` or `stop-unclean` boundary, its writer (the recorder, or recovery)
  writes an end object `captures/<tool_id>/<capture_id>/end/<kind>/<gap_start>/<gap_end>` after the pack holding the boundary is durable,
  and before any successor's registry object; end objects are never deleted.
  End state and barriers therefore survive eviction, retention and window-only rebuilds; the G5-61 fallback remains only for captures whose end object is missing.
- G5-64 Write-ahead registration (Q9, option a; amends G5-62 and G5-63 after P3 review round 4) — deferred to P5 by G5-65:
  a registry object is written and then registered with the catalog before the capture's first pack is uploaded;
  an end object is written and registered before the pack holding its boundary is uploaded (recovery computes the boundary first),
  and before any successor's registry object.
  A rebuild starts after the lost catalog stops acknowledging registrations, lists the registry completely and adds the registrations it receives meanwhile.
- G5-65 Scope split (Q10, option a, after P3 review round 5 returned a new class of P1): P3 keeps object lifecycle and cold reads.
  The catalog records per-capture end evidence (boundaries, gap bounds, epoch closures) from every segment or converter archive presented for registration, indexed or not,
  and never removes it; a rebuild bootstraps every retained pack and MAY index only the window.
  G5-61..G5-64, rebuilds that read only the window, and end evidence already removed by retention move to proposal P5 (`p5-capture-registry.md`), deferred.

## 2026-09-27 redaction (question Q3, proposal P6; owner agreed, grilling rounds 1–3)

- G5-66 Store in full, control access, mask on egress.
  Archives, segments, the bucket and the catalog are open only to privileged components; the storage profile states this requirement.
  Masking happens only when an extract is written: masked JSONL and SML are renderings of a masked extract,
  and a masked extract is never a merge input or a patch.
  In format 1.0 only extract writers set the `redacted` bit.
- G5-67 Masking method: per SECS-II item, length-preserving, zero fill of the contents of every leaf item under the rule's path;
  every item header is kept, so structure, `decode_status` and framing stay valid, while the shape and lengths leak.
  A message text that cannot be decoded is zero-filled whole, keeping the HSMS header.
  Rules match S/F on payload values ([FMT I-10]), never on header copies.
- G5-68 Digest: HMAC-SHA-256 over a domain string followed by the original encoded item bytes (item header included), full 32 bytes.
  Each rule names its domain, and only items of the same domain are comparable (e.g. S7F3 and S7F6 PPBODY share one).
  The key is held only by the export service; one key id per policy version; digests are comparable only under equal key ids.
- G5-69 Placement: pack metadata `redaction_policy` (policy id, version, key id) whenever a policy was applied, even if nothing matched;
  repeatable nested `redaction` entries (seq, concrete item path, masked byte ranges, domain, digest), with an empty path for a whole-text mask;
  header flag `redaction-present` only when at least one record carries `redacted`.
  Rule paths are 1-based with `*` for variable-length lists; entries always hold concrete paths.
  An extract without `redaction_policy` was not screened.
- G5-70 Record semantics: `fidelity` and `decode_status` keep the source values;
  `redacted` takes precedence — byte-level claims (wire-exact analysis, byte identity) exclude masked ranges, which compare only by digest.
- G5-71 Converter annotations: a policy may mask the `raw` / `text` of chosen annotation kinds whole (zero fill, same length, digest);
  the baseline masks `unparsed-entry`, `skipped-bytes` and `unrecognised-line`.
  Transport-event `detail` and `cause_raw` are not masked.
- G5-72 Rule sets are service configuration identified by `redaction_policy`;
  the spec lists an informative baseline (S7 PPBODY and formatted process programs, S15 recipe bodies, S16 RCPPARVAL)
  and per-site candidates off by default (S10 terminal text, S14 attribute data, S2F41 / S2F49 CPVAL).
  The policy file format is deferred (service design).
- G5-73 VE reads unmasked data as a privileged consumer (G1-11 unchanged); its derived models and profiles inherit the same access control.
  Recorded at G1-11, not in the tracepack spec.
- G5-74 Capture-time redaction with an escrowed encrypted original is deferred; its cost analysis goes to [OVW §6].
  The `redaction` entry structure is defined so that it can later become an annotation body written by a recorder.
- G5-75 `dump --sml` shows a masked leaf's item type, length, domain and a digest prefix; the exact syntax is deferred with the other CLI output formats.
  Process: proposal P6, external review rounds until ready, then application as v2.8 with a consistency review.
- G5-76 Extract sources (2026-09-27, after the P6 application review A1): an extract is written only from records in full.
  An unscreened extract (no `redaction_policy`, no `redacted` record) is a valid source; an extract written under a policy, or any pack holding a `redacted` record, is rejected before output.
- G5-77 tracepack module path (2026-09-27; supersedes R3-4): `github.com/arloliu/go-secs/tracepack`, a nested module in `tracepack/` with tags `tracepack/vX.Y.Z`.
  Reason: for a module in `tracepack/`, the go command maps `github.com/arloliu/go-secs/v2/tracepack` to the repository directory `v2/tracepack/`
  (Go 1.26 `cmd/go/internal/modfetch/coderepo.go`: without a trailing `/vN` there is no major-version fallback),
  so R3-4's path would not be fetchable outside the repository; tracepack is consumed by other repositories.
  A released tracepack requires a released go-secs v2; local development uses `go.work`.
- G5-78 tracepack repository integration (2026-09-27): `go.work` and `go.work.sum` are gitignored and never committed;
  developers run `go work init . ./tracepack` locally, and CI creates its own in the tracepack job.
  `tracepack/go.mod` never carries a `replace`;
  until a go-secs release has what tracepack needs, it requires a pseudo-version of a pushed `main` commit.
  CI gets a separate tracepack job: the workspace leg blocks PRs, so the root and tracepack may change in one PR;
  the `GOWORK=off` consumer leg is advisory on PRs and must pass before any `tracepack/` tag.
  Root Makefile variables built on `go list` or `git describe` become safe under a local `go.work` and `tracepack/` tags.
- G5-79 Release order (2026-09-27): go-secs v2.5.0 (PR #14, merged to `main`) is released first and does not wait for tracepack;
  the two modules release independently.
  The first tracepack release, `tracepack/v0.1.0`, requires go-secs v2.5.0 or later;
  go-secs changes that tracepack needs afterwards land in v2.5.x or v2.6.0.
  GitHub releases for `tracepack/` tags are created with `--latest=false`.
