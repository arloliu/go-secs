# tracepack — decision log

Status: current (2026-10-10)
Owner decisions for tracepack; append-only under `.agents/rules/450-doc-lifecycle.md`.
Entries up to G5-79 were recorded in the design notes before the repository became the source of truth;
they are copied here verbatim under their original headings, and later entries are added only here.
G3 and G4 entries come from the design grilling rounds, R3 entries from the owner's answers after review round 3, G5 entries from the tracepack design sessions.
Decisions that concern only the Virtual Equipment program stay in its design notes, as do the files some entries name.
From G5-173 on, a Rationale or Rejected line marked *(editorial)* was written when the entry was logged, to explain it;
it is not part of the owner's recorded decision or of a review's finding.

## 2026-09-26 grilling round 3 (owner accepted all recommendations)

- G3-20 EAP log (assumed until sample seen): SML text; has direction, µs timestamps, SystemBytes,
  DeviceID, W-bit; has transport events (connect, Select, T3 timeout).
- G3-21 Query priorities: index natively supports tool+time range, S/F filter, SystemBytes transaction
  lookup, transport events; optional per-blob secondary index for CEID/ALID/RPTID/SVID; full-text
  SML and cross-tool aggregation are offline scans.
- G3-PLACE Data-pack format + codec live in go-secs as a nested module (own go.mod), e.g.
  `github.com/arloliu/go-secs/tracepack`; reference CLI in the same nested module
  (`tracepack/cmd/tracepack`), exempt from go-secs "no binary" rule. VE repo holds the EAP converter.
- G3-23 Blob scope: per tool per hour, roll at 256 MB uncompressed; S3 key `tool/YYYY/MM/DD/HH/seq.pack`.
- G3-24 Content: raw frame bytes as payload + fixed-width header fields (ts, dir, S/F/W, SystemBytes,
  DeviceID, len, decode status) for indexing; undecodable frames stored with failure reason.
- G3-25 Container: custom simple format (magic, version, zstd seekable frames, footer block index);
  pure Go, no CGO; supports S3 range reads. Parquet export tool is a possible later add-on.
- G3-26 No redaction by default; format reserves a redaction marker; retention via S3 lifecycle.
  (2026-09-27: the retention part is superseded by G5-56 and G5-57 — lifecycle rules only transition `archive/` objects, retention is executed by the service;
  redaction is designed by G5-66..G5-75.)
- G3-27 Timestamps: native capture = UTC wall + monotonic delta; converted = UTC with source tz and
  "converted-from-EAP" recorded in pack header.

## 2026-09-26 grilling round 4 (owner accepted all recommendations) — frontier empty

- G4-28 Evidence grades: `reconstructed` (from EAP SML via converter) is accepted for semantic-layer
  learning (catalog, reply rules, timing, semantic quirks); transport-layer and malformed-frame
  quirks require `observed`. Pack header field `evidence: observed | reconstructed`.
  Owner note: raw-byte capture can be added inside the equipment gateway — either in its EAP bus adapter or as a new
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
- R3-6 Pilot tools and EAP sample: provided later. **Owner emphasis: the current goal is to
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
  (2026-10-10: a direct converter archive publication creates a generation too, G5-193.)
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

## 2026-09-27 log service review (owner answers to the log-service checkpoints)

- G5-80 Writer boundary (2026-09-27): producers (equipment gateways, EAP converters and others) push records to the log service,
  and the log service is the traffic-log recorder in the sense of [STO §4] — the role of a log shipper such as fluentbit or vector.
  It assigns `capture_id` and capture-scoped `seq`, keeps the spool and liveness anchor, and applies the clock-anchor rule (superseded by G5-86);
  the observed fields of a record (`ts_utc_ns`, `mono_ns`, direction, epoch) come from the producer.
  The producer-to-service transport (ordering, retransmission, idempotency, restart detection) is service design, outside the tracepack specification.
- G5-81 Single source of truth per tool (2026-09-27): the log service records one producer per tool;
  no policy for reconciling two captures of the same traffic from different vantages is needed.
- G5-82 Live tail (2026-09-27): deferred, an advanced feature;
  the intended design is that the log service discovers equipment-gateway instances through the gateway controller and queries them over gRPC,
  not a tracepack read path.
- G5-83 Catalog durability (2026-09-27): the catalog database is deployed with backup and high availability by default,
  so a catalog rebuild from the bucket is a disaster path, and the per-capture end evidence that a rebuild cannot recover (P5) is accepted as a residual risk.
- G5-84 Cold-scope admissions (2026-09-27): the rejection of admissions for scopes outside the catalog window (P4) is a known limitation;
  converter archives that arrive after the window are read through listing views only.
  (2026-10-10: corrected by G5-193 — a rejected converter archive gives per-capture evidence only and no listing view reads its records; converter segments are read through listing views.)
- G5-85 go-secs support for the equipment-gateway producer (2026-09-27): after the format primitives land, a go-secs proposal is written first,
  exposing the connection generation number and the read-time wall and monotonic timestamps on received messages and lifecycle events.
  Design premise: the equipment gateway will emit traffic-log records either through a new `secs-recorder` device or by adding record emission to its EAP bus adapter;
  the proposal serves both, and the log service ingests those records (G5-80).
- G5-86 Stateless recorder over a durable bus (2026-09-27; supersedes the identity-assignment part of G5-80):
  the producer (the equipment gateway's HSMS device, a converter, or any other) assigns `capture_id` and `recorder_instance_id` per process start,
  assigns the capture-scoped `seq`, stamps the socket epoch and the wall and monotonic timestamps, detects its own clock steps,
  and publishes each record to NATS JetStream, which persists it before acknowledging the publish.
  The log service is a set of stateless consumers on one work-queue consumer with a queue group:
  any instance takes any record, keeps one open block per scope with closed blocks compressed in memory,
  flushes a segment every F, and acknowledges a record only after the segment holding it is durable in `staging/`.
  Segments of one scope written by different instances interleave; the hourly merge normalizes them, and the first release does not partition by tool.
  Spec: [FMT I-12] allows producer-assigned seqs; [STO §4] "Recorder over a durable bus" replaces the spool contract for this deployment.
  (The service-side closure of a crashed producer's capture described in the corrections below is superseded by G5-88.)
- G5-87 Tool ownership for publishing (2026-09-27): the log service's merge and publish work is distributed by **tool leases in the catalog**:
  a publisher instance acquires unowned or expired tools up to its share, releases tools above it, and renews its leases;
  every acquisition of a tool mints a new publisher epoch (fence object, number allocated by the catalog),
  so the generations of a tool always rank in the order they were published, whichever instance published them.
  Merges run on a bounded worker pool per instance; a scope's merge holds only the blocks of its current seq window, so memory does not grow with the tools owned.
  Partitioning the ingest by tool (a NATS partition per tool set, subscribed by the instance that leases it, so segments stop interleaving and merges copy blocks verbatim)
  is a second-phase optimization: the partition becomes the leased unit, and the lease stays in the catalog.
- G5-86 correction (2026-09-27, after the v2.9 review): `recorder_instance_id` stays stable across the producer's restarts, as [FMT I-7] requires;
  only `capture_id` is new per process start, and the producer persists its last `capture_id` to fill `previous_capture_id`.
  The producer publishes each `clock-step` and waits for its acknowledgement before the records under the new anchor.
  A consumer that receives a record below a closed block opens a second segment for the scope instead of sorting per block.
  The service closes a crashed producer's capture only after the successor's `start` is durable, the bus holds nothing of the old capture,
  and the catalog has recorded the closure atomically; `gap_start` follows the recovery formula, `gap_end` is omitted (closure rule superseded by G5-88).
- G5-86 second correction (2026-09-27, after the second v2.9 review): one bus subject per capture, and at most one producer process per `recorder_instance_id` at a time;
  a producer publishes a capture descriptor with its `start` so segments carry the capture's pack metadata;
  a consumer may hold several open segments per scope; closure of a crashed producer's capture requires the successor's `start` staged,
  zero messages on the old capture's subject, and two catalog transactions (reserve, then register the boundary segment); post-closure segments are quarantined
  (closure rule superseded by G5-88).
- G5-88 Scope split (2026-09-27, proposed after the third v2.9 review round under the treadmill rule; owner agreed the same day):
  the service-side closure of a crashed producer's capture leaves v2.9 and becomes proposal P7 (deferred);
  until it is designed such a capture stays `open`, and producers publish a `stop` boundary on every orderly shutdown.
  The capture descriptor is registered in the catalog by the consumer that receives the `start`, and a record without a known descriptor is deferred.

## 2026-09-28 reader

- G5-89 Coverage matching (2026-09-28): a reader matches a `coverage` entry by its own bounds and errs toward `incomplete`:
  an absent seq or time bound is unbounded, so an entry without a time interval intersects every time query of its capture;
  an inverted entry intersects every query of its capture; an entry without `capture_id` belongs to the pack's capture.
  Rationale: `coverage` is rare (a stored pack damaged beyond repair), and a missed intersection would turn lost records into "no match",
  e.g. a transaction reported `unmatched`; over-reporting is bounded by the pack's hour.
  Writer-side requiredness of the nested tags is left to `Repair`, which writes all of them.
  Spec: [FMT §5] (v2.11).
- G5-90 Copy-field availability (2026-09-29): a copy field's availability follows its stored `field_validity` bit, never the payload's extent,
  in every query mode, attested or not; authoritative evaluation takes the field's value from the payload and its availability from the bit,
  so a set bit whose bytes the payload lacks (a writer defect) is unavailable to it.
  The writer keeps the caller's `field_validity`, because a log converter clears the bits of identities its source did not carry ([STO §7]).
  Rationale: the query result no longer depends on whether a pack is attested, the writer's and reader's I-10 checks agree,
  and the v0.1.0 writer's behavior is unchanged.
  Rejected: the writer computing `field_validity` from the payload length (breaks [STO §7] item 4).
  Spec: [FMT §7.2], [SEM §7.4], [FMT §16] (v2.12).

## 2026-09-29 weight and small-message overhead review (proposal P9)

- G5-91 Sequencing and phase 4 scope (2026-09-29): the format revision of P9 comes before phase 4;
  phase 4 is `Verify` and `Repair`.
  `Recover`, the liveness anchor and the spool durability helpers wait until a local-spool recorder is planned,
  because G5-86 replaced the spool contract for the bus deployment the pilot uses.
  Rationale: a format change is cheapest before more code and packs depend on the layout.
- G5-92 Byte-columnar header section (2026-09-29): the header section of a decoded block body stores the record headers transposed,
  byte `j` of record `i` at offset `j × record_count + i`; every field is kept.
  Format 1.0 is redefined in place: packs written by tracepack v0.1.0 are not readable by the new reader
  (a one-record block is laid out identically; larger blocks usually read `corrupt`).
  Rationale: measured header cost −40 % (about 17 → 10 bytes per record after compression) and encoding 20 % faster,
  for 0.66 ms more decoding per 4 MiB block; no production packs exist yet.
  Spec: P9 §3.1.
- G5-93 Merge coalescing (2026-09-29): a merger SHOULD coalesce runs of adjacent small blocks with equal `record_header_len` into blocks up to the size threshold.
  Rationale: small blocks multiply the per-record cost (a block per second: ×4.8 idle, ×2.3 busy), and the archive is what retention keeps.
  Spec: P9 §3.2.
- G5-94 Retirements (2026-09-29): correction patches and multi-member replacement sets leave format 1.0;
  a merge writes one archive per scope whatever its size.
  Rationale: no planned tool writes corrections; at the stated load a scope holds a few MB to about 40 MB per hour, far below the 256 MiB split.
  Spec: P9 §3.3.
- G5-95 Scope split (2026-09-29, after P9 review round 1 under the treadmill rule; owner agreed the same day):
  the storage part of P9 — a view defined by the packs alone, without commit objects, fence objects, publisher epochs or generation ranking — becomes proposal P10 (draft),
  because the review found four P0 gaps (witness eligibility after a repair, containment proof, listing coherence against deletion, catalog visibility of durable segments).
  P9 keeps the format revision.
  Phase 4 starts with `Verify`; `Repair` waits for P10's outcome, because a repair pack's metadata (`patch_base`) depends on it.
  For a catalog rebuild alone (deleting component stopped), the packs suffice once every copy of a record is byte-identical; live operation needs P10's redesign.
- G5-96 Redundancy cleanup (2026-09-29): pack-metadata `schema_version` (0x0001) and F-3 `content_bytes` (0x0006) are retired;
  writers never write them, readers treat them as unknown tags, and their numbers stay reserved.
  `time_source`, the derived quality bits and the trailer and F-1 counts are kept.
  Rationale: `schema_version` repeats the format version and F-3 `content_bytes` must equal F-2 `uncompressed_len`;
  `time_source` is not implied by `capture_method` (a packet-capture stream is `raw-stream` with another machine's timestamps).
  Spec: P9 §3.4.
- G5-97 Record header without copy fields (2026-09-29): the record header no longer copies HSMS header fields
  (`session_id`, `stream`, `function`, `ptype`, `stype`, `system_bytes`, `record_flags.W`); predicates read them from the payload,
  which still holds the whole frame.
  `field_validity` stays, and a writer rejects a set bit whose bytes the payload lacks.
  I-10, the provisional and authoritative query modes with `header-validated` (G5-51), and `blocks_validated` attestation (G5-52) are removed with the copies;
  a writer SHOULD still validate each block before writing it.
  Rationale: records are read on demand for one tool or a few tools of one type, and the VE reads whole windows offline;
  no consumer needs S/F or System Bytes filtering without decoding payloads, while the copies carried the most review-heavy rules of the format.
  Spec: P9 §3.7.
- G5-98 Field review (2026-09-29, after an external opinion on each retirement): every block read is a full read —
  the header-only read level and `blocks_validated` are removed, because without attestation a header-only result could count records a full read rejects,
  and under the access patterns of G5-97 a header-only read saves only CPU.
  `mono_ns` with `record_flags.mono_present`, `trailing_bytes` and `quality.capture-boundary` are kept;
  `quality.no-mono`, `quality.decode-failed` and F-3 `max_payload_len` are retired (the first two are mirrors; readers offer derived predicates instead).
  SEM §7.3's extraction design is removed while the F-4 slot stays an absent, undefined section, so the footer layout does not change.
  The record header becomes 44 bytes.
  Rejected: dropping `mono_ns` or storing it as a residual (VE latency), retiring `trailing_bytes` (not recomputable from masked extracts)
  and `quality.capture-boundary` (not derivable per record from `kind` and `decode_status`).
  Spec: P9 §3.8.
- P9 closed (2026-09-29): applied as spec v2.13 (`tracepack-spec-changelog.md`); its last text is at commit `cd5c171`.
- G5-99 Transaction lookup with unavailable key fields (2026-09-29, after the first application review of P9): a primary whose SessionID or System Bytes is unavailable has no key and its lookup is `incomplete`;
  a reply-direction data record in the primary's window whose SessionID, System Bytes, stream or function is unavailable could be the reply,
  so it turns `unmatched` into `incomplete` and is reported as an anomaly; `matched` and `ambiguous` are decided on the candidates with available fields.
  Rationale: `unmatched` must never rest on a record that could not be compared (the direction of G5-89);
  a stricter rule, `incomplete` even beside a valid match, would make every lookup of a log conversion without System Bytes incomplete.
  Spec: [SEM §7.2] (v2.13).
- G5-100 Verification outcomes (2026-09-29, before implementing `verify`): [FMT §13] named four outcomes without defining them.
  A block fails on its envelope, its body CRC, its codec, I-2 or the agreement of its envelope with its F-2 entry.
  The outcome is the first that applies of `corrupt-middle` (a failed block followed by a validated one), `unfinalized` (I-5 does not hold),
  `finalized-truncated` (a failed block or a stopped walk, with no validated block after it),
  the new `finalized-inconsistent` (every block validated, while the footer is invalid or disagrees with the records or the trailer)
  and `finalized-consistent`.
  Writer defects in a validated block, such as a `field_validity` bit beyond the payload, are reported beside the outcome and never change it,
  because a reader treats such a field as unavailable and still uses the record.
  A file header or pack metadata that cannot be read is an error, as for `Open`.
  Rationale: `corrupt-middle` comes first because the report also states finalization, so both facts stay visible;
  `finalized-inconsistent` tells a pack that lost no record, whose footer a rewrite rebuilds, from one that needs a repair.
  Rejected: `unfinalized` before `corrupt-middle`; a footer defect reported under `finalized-consistent` or `finalized-truncated`;
  a writer defect failing its block; an unreadable file header or pack metadata reported as an outcome.
  Spec: [FMT §13], [FMT §16] (v2.14).
- G5-101 Proposal P10 rejected (2026-09-29): the storage view without commit and fence objects is not pursued.
  The catalog is already a cache rebuilt from the packs plus the commit and fence objects ([STO §5]),
  so P10's gain was only retiring those objects and the rules around them;
  three of its four open P0s were live-operation coherence gaps that the commit protocol exists to close,
  and closing them meant designing a protocol of equal strength.
  Consequences: the commit protocol, publisher epochs, generation ranking and `patch_base` stay as [STO §2], §4, §5 and §6 define them;
  `Repair` writes a generation-0 patch whose `patch_base` is the current generation's `replacement_set_id`, supplied by its caller;
  P4 stays deferred and P7 is unchanged.
  Not adopted with it: a rule that every repair re-emits its records byte for byte; [STO §6] keeps "possibly with new stored bits or classification".
- P10 closed (2026-09-29): rejected (G5-101); its last text is at commit `69767cf`.

## 2026-09-30 repair

- G5-102 `coverage` nested tags on write (2026-09-30, planning `Repair`; closes the writer-side requiredness G5-89 left open):
  a writer writes `capture_id`, `seq_first`, `time_start` and `time_end` in every `coverage` entry it declares,
  and `seq_last` whenever the upper bound of the lost seqs is known;
  an absent `seq_last` means the loss extends past the last seq the writer could establish,
  which the G5-89 reading rule already treats as unbounded.
  Entries a patch inherits are carried as data, whatever nested tags they hold.
  Rejected: writing `seq_last` = 2^63−1 as a sentinel for an unknown bound.
  Spec: [FMT §5] (v2.15).
- G5-103 Repair refusals (2026-09-30): repair writes nothing, and the damaged pack stays, when no faithful and consistent patch can be made:
  a validated block carries a seq-order (I-12) or hour-span (I-13) writer defect,
  which footer validation would reject in the patch's own index;
  a lost run holding a failed block has no seq between its validated neighbours,
  which shows that I-12 breaks inside the failed region, so the lost seqs cannot be bounded;
  the pack is not a stored pack (`pack_role` other than `segment`, `archive` or `repair`);
  its period is not inside one UTC hour; a validated block lies outside the scope's hour;
  or the validated records breach the pack metadata's commitments:
  a classified record without `classifier`, an `oversized` record without `max_frame_len`, or a `quality.redacted` record.
  The other writer defects do not affect the footer, so their blocks are copied with the defects.
  Rejected: copying such blocks anyway (the patch would itself be `finalized-inconsistent`);
  dropping them into `coverage` (validated records would be lost);
  declaring a capture-wide `coverage` entry when the lost seqs cannot be bounded.
  Spec: [FMT §13] (v2.15).
- G5-104 Repair of a patch (2026-09-30): `supersedes` names only the damaged pack, also when that pack is itself a `repair` patch.
  Whether the packs an in-view patch removed come back when a later patch names that patch is a question of the active view ([STO §4]),
  left to the design of `ActiveView`.
  Rejected: also naming the damaged patch's own `supersedes`,
  which [STO §5] would reject at registration, since those packs are no longer in the view;
  refusing to repair a patch.
  Spec: [FMT §13] (v2.15); impl plan phase 5.
- G5-105 Blocks after a failed block (2026-09-30): repair copies every validated block,
  including the validated blocks a valid footer locates after a failed one,
  as the [FMT §13] guarantee (every validated block is readable) requires.
  Rejected: copying only the validated prefix.
  Spec: [FMT §13] (v2.15).
- G5-106 Trust in a valid footer for lost ranges (2026-09-30, after the first plan review of `Repair`):
  with a valid footer, a failed block's lost records are its F-3 seq ranges within its F-2 time range,
  the same trust a read that prunes by that footer places in it ([FMT §10]);
  when a validated block of the pack disagrees with the footer, the footer has shown itself false,
  and every failed block of the pack is bounded as in a pack without a valid footer:
  by the seqs of its validated neighbours, within the scope's hour.
  Rationale: a footer that stays consistent with itself while misstating a block is a writer defect that only reading the block reveals,
  and the failed block cannot be read;
  bounding every failed block by its neighbours instead would report the seq gaps between blocks and every time query of the hour as lost.
  Rejected: always trusting the footer; always the neighbour bounds.
  Spec: [FMT §13] (v2.15).

## 2026-09-30 transitive supersession

- G5-107 Transitive supersession (2026-09-30):
  a patch based on G, one with a commit object whose `patch_base` is G's `replacement_set_id`,
  removes the packs its `supersedes` names from the view, also when a later patch names that patch in turn,
  so the packs a repaired patch replaced stay replaced.
  This settles the view question that G5-104 left to `ActiveView`.
  A patch may name patches in the view.
  Merge's cumulative `compacted_from` lists every generation-0 pack that a patch based on G names,
  so the packs of such a chain are folded, and become deletable, together.
  Rationale: patch registration is serialized and a patch names only packs in the view,
  so chains of `supersedes` are acyclic and the view needs no fixpoint;
  the later patch holds the earlier patch's recoverable records and carries its `coverage` ([STO §6]),
  so nothing the earlier patch replaced is needed again.
  Rejected: applying only the `supersedes` of patches still in the view,
  under which a later patch would bring back the damaged pack an earlier patch replaced, and the next merge would fold it;
  the transitive view without the `compacted_from` change,
  under which a replaced segment would miss the list, return to the next generation's view and never become deletable.
  Spec: [STO §4], [STO §5], [STO §8], [FMT §5], [FMT §13] (v2.16).

## 2026-09-30 merge

- G5-108 Repeatable `classifier` and `max_frame_len` (2026-09-30, planning `Merge`):
  both tags are repeatable.
  A recorder or converter writes one value of each, as before;
  a merger writes every distinct value its inputs carry,
  so an archive whose segments were classified by different consumer versions names all of them.
  `classifier` stays required when any record is classified, and `max_frame_len` when any record is `oversized`, now with one or more values.
  A record's `oversized` status was decided against the `max_frame_len` in force where it was classified;
  with several values the pack does not say which one.
  Rationale: the consumers of a durable bus are upgraded one by one, so the segments of one scope can carry different classifier versions,
  and a merge must still produce an archive that describes every record it holds.
  Rejected: failing the merge when the inputs' values differ, which blocks merges during every rolling upgrade of the consumers;
  writing one representative value, which misdescribes the records classified by the others.
  Spec: [FMT §5], [FMT I-11], [FMT §14], [SEM §3], [STO §4] (v2.17).
- G5-109 Defective merge inputs (2026-09-30):
  a merger reads every block of every input in full, as verification does ([FMT §13]),
  and fails, publishing nothing, when an input is not `finalized-consistent`:
  no valid footer, a failed block, or a block that disagrees with its footer (F-2 or F-3).
  The damaged input is repaired first ([FMT §13], [STO §6]), and the patch joins the view in its place.
  This settles the first two items of proposal P2 §1; P2 stays deferred for its recovery items.
  Rationale: a block copied verbatim is never decoded by the copy itself,
  so only a full read keeps a CRC-valid block that fails I-2, or one its footer misstates, out of a generation that makes its inputs deletable.
  Rejected: trusting footers and CRCs alone, under which a CRC-valid undecodable block could enter a generation.
  Spec: [STO §4], [STO §8] (v2.17); proposal P2.
- G5-110 Record conflicts in a merge (2026-09-30):
  two records of the view with the same (`capture_id`, `seq`) and different bytes fail the merge,
  which publishes nothing and reports every conflict;
  both versions stay in the view, where readers report the `conflict` ([FMT I-12]), until an operator resolves it.
  Rationale: the merge's output makes its inputs deletable,
  so any version it did not carry would be lost with them.
  Rejected: keeping one version and reporting the conflict, under which the other version is lost once the inputs are deleted.
  Spec: [FMT I-12], [STO §4], [STO §8] (v2.17).
- G5-111 Lineage of a pack without generation-0 ancestors (2026-09-30):
  `compacted_from` lists the generation-0 packs a pack's records represent;
  it is present when there is at least one, and absent otherwise, whatever `compaction_level` is.
  A merge of a scope whose view holds only a converter's archive ([STO §7] item 9: `compaction_level` 0, no `compacted_from`)
  writes an archive at level 1 without `compacted_from`.
  The rule replaces "required when `compaction_level` ≥ 1".
  Rationale: a converter's archive represents no generation-0 pack, so a merge of it has none to list,
  and an invented entry would name a pack that never existed.
  Rejected: forbidding the merge of such a scope,
  which [STO §5] needs when it cancels an interrupted admission by an ordinary merge of the view.
  Spec: [FMT §5], [FMT §14], [STO §4], [STO §7] (v2.17).
- G5-112 `seq_start` of an archive without records (2026-09-30):
  the largest `seq_start` of the merge's inputs,
  the latest position the inputs knew of the capture's next seq.
  Rationale: every input's `seq_start` is at or below the capture's next seq, so the largest is the closest to it.
  Rejected: the smallest, which lies further from the capture's next seq.
  Spec: [FMT §5], [STO §4] (v2.17).

## 2026-10-01 epoch closure and capture-boundary records

- G5-113 `close_seq` of an epoch ended twice (2026-10-01):
  an F-3 `epoch` entry carries the lowest seq among the block's records that ended that epoch, socket-close events and clean `stop`s;
  the F-5 aggregation takes the minimum `close_seq` present among the blocks' entries for that epoch.
  The rule replaces "`close_seq` kept when present", which did not say which closing record wins.
  Rationale: a minimum is a mergeable aggregate ([FMT I-14]), and it is what the Go writer and reader already compute.
  A reader recomputes F-5 from the F-3 entries, and `verify` recomputes each F-3 entry from its block's records,
  so every writer must follow one rule:
  a writer that kept the last closing record would write an F-5 that footer validation rejects when the two records lie in different blocks,
  and an F-3 entry that `verify` finds disagreeing with its block's records (`finalized-inconsistent`) when they share one.
  Rejected: the last closing record, under which the footers the Go writer has written for such epochs would be invalid;
  leaving the choice to the writer, which a reader that recomputes the footer by one rule cannot accept.
  Spec: [FMT §10], [FMT §16] (v2.18).
- G5-114 Capture-boundary records and the epoch a clean `stop` ends (2026-10-01):
  a capture-boundary record is a transport-event record whose payload is a valid TLV body ([FMT §8]) with `event` = capture-boundary.
  Quality bit 0 does not make a record one; an absent `boundary_kind` is written as 0 (unknown) in its `boundary` entry;
  a transport-event record whose payload is not a valid TLV body adds no `boundary` entry and no `close_seq`.
  A clean `stop` sets `close_seq` only in the `epoch` entry of its own record's epoch;
  that the capture ended is shown by its `boundary` entry ([SEM §7.2]), not by `close_seq` in other epochs.
  Both write down what the Go writer, reader and `verify` already do.
  Rationale: a summary entry follows from what a record's payload says, which every reader can recompute,
  while quality bit 0 is a stored bit its writer declares ([SEM §6]);
  an `epoch` entry describes its epoch's records,
  and an epoch still open at a clean `stop` need not have a record in the block or pack that holds the `stop`.
  Rejected: deriving `boundary` entries from quality bit 0;
  setting `close_seq` at a clean `stop` in every epoch still open, which the `stop` `boundary` entry already covers for the whole capture.
  Spec: [FMT §10], [FMT §16], [SEM §7.2] (v2.18).

## 2026-10-01 `seq_start` of a pack with records

- G5-115 A `seq_start` other than the first record's seq (2026-10-01):
  `verify` reports it as a writer defect of the first validated block:
  a first seq other than `seq_start`, or, when a failed block precedes that block, a first seq not above `seq_start`.
  Like every writer defect it never changes the outcome, and footer validation does not compare `seq_start` with the index,
  so a finalized pack whose only fault it is stays `finalized-consistent`.
  Repair writes a patch for such a pack, as for a `finalized-inconsistent` one:
  every block copied, the footer rebuilt, no new `coverage`, and the patch's `seq_start` its first record's seq.
  The seqs between the damaged `seq_start` and the first record are not reported lost.
  Rationale: a merge refuses an input whose `seq_start` is not its first record's seq ([STO §4]),
  so without a repair such a pack could not be merged,
  and verification did not even name the reason.
  Unlike the seq-order and hour-span defects, the records are sound and only the pack metadata is wrong,
  which a patch writes anew, so the patch is faithful.
  `seq_start` states the first record's seq ([FMT §5]); a mismatch is a fault of the metadata, not evidence of lost records.
  Rejected: reporting the defect without a repair, which leaves the pack stuck between `verify` and a merge;
  having footer validation reject it, which would change how every reader opens such a pack;
  recording the seqs below the first record as lost `coverage`.
  Spec: [FMT §13], [FMT §16], [STO §4] (v2.19).

## 2026-10-01 reads over several packs

- G5-116 Capture order interleaves block by block (2026-10-01):
  each capture is a stream of its records in ascending `seq`,
  and the next record yielded comes from the capture whose next selected record lies in the block with the smallest `ts_min`, ties broken by `capture_id`;
  `ts_min` is the block's F-2 value or, without one, the smallest `ts_utc_ns` of its records.
  Rationale: the output follows time roughly while each capture stays in seq order, without buffering beyond each capture's open blocks.
  Rejected: concatenating whole captures in the order of their first block's `ts_min`, which is not an interleaving.
  Spec: [SEM §7.4] (v2.20).
- G5-117 Blocks without an F-2 entry (2026-10-01):
  a read over several packs reads each block of a pack that is not finalized, or whose footer is not valid, once beforehand,
  takes its seq and time ranges from its records and releases it, then reads it again when the order reaches it.
  Rationale: clusters and the time-order watermark need each block's ranges, and memory stays bounded; such a pack is read twice.
  Rejected: refusing such packs in time order, which would keep a live segment out of a time-ordered read;
  loading every such block at once, which a large live segment would push past any memory limit.
  Spec: [SEM §7.4] (v2.20).
- G5-118 Exclusion by overlap cluster (2026-10-01):
  the blocks of one capture whose seq ranges overlap, across every pack read, form an overlap cluster, as for a merge;
  F-2 and the F-3 summaries exclude a cluster only when they exclude every block of it, and otherwise every block of it is read and its copies compared before the filter.
  A cluster excluded whole is not read, and a conflict inside it is not reported, since none of its records is selected.
  Rationale: excluding one block while reading an overlapping one would let a filter discard a version unseen, so a conflict would go unreported.
  Rejected: never excluding a block that overlaps another, which reads more for no selected record.
  Spec: [SEM §7.4] (v2.20).
- G5-119 Every selected version of a conflict is yielded (2026-10-01):
  each distinct version the filter selects is yielded once, marked as a conflict,
  and the read lists every conflict of every compared cluster, whether or not the filter selected a version.
  This replaces "readers keep one copy and report a `conflict`" of [FMT I-12], which did not say which copy.
  Rationale: the records exist, and yielding one would be the silent choice I-12 forbids.
  Rejected: yielding no version, which hides evidence; yielding the first version, a silent choice.
  Spec: [FMT I-12], [SEM §7.4] (v2.20).

## 2026-10-02 transaction lookup

- G5-120 Phase 5c in three steps (2026-10-02):
  5c1 the `PackSource` interface, a test source and `FindTransaction`; 5c2 a listing-backed `PackSource` and the object-store interface it needs; 5c3 the retention boundary and `Result.Removed`.
  Rationale: the lookup is the first user of `PackSource` and shapes it; each step is reviewable alone.
  Rejected: infrastructure first; one step.
  Spec: impl plan "5c" (v2.21).
- G5-121 A caller bounds the lookup's forward search (2026-10-02):
  the eligibility window has no upper bound while no later same-key primary and no closure is known,
  so a lookup reads a caller-given number of scopes from the primary's, and a window still unbounded after them prevents `unmatched` (a match found is still reported, G5-127).
  Rationale: an unanswered primary on a long connection could otherwise scan the rest of the capture.
  Rejected: walking to the capture's end; a fixed two-hour window in the spec.
  Spec: [SEM §7.2] (v2.21).
- G5-122 A lookup starts from the primary record (2026-10-02):
  the caller names the primary, and the lookup reads it and derives `epoch`, direction, SessionID, System Bytes, stream and function from it.
  Rationale: §7.2 needs the primary itself (its quality bits, the availability of its fields), and a caller-given key could disagree with it.
  Rejected: a caller-given six-field key, verified or not.
  Spec: [SEM §7.2] (v2.21).
- G5-123 A listing-backed source takes per-capture evidence from a provider (2026-10-02):
  end states, barriers and epoch closures come from the catalog's per-capture entries ([STO §5] Listing view), which the caller supplies.
  Rationale: the bucket alone cannot show the evidence of other hours or of rejected registrations.
  Rejected: deriving it from the F-5 of the listed packs alone.
  Spec: [STO §5], `tracepack-go.md` §3 (v2.21; designed in 5c2).
- G5-124 A lookup names the primary's hour (2026-10-02):
  the key is (`capture_id`, `seq`, UTC hour of the primary's `ts_utc_ns`), the hour being its scope.
  Rationale: a seq does not show its hour, and archive keys carry no seq, so a listing could not find it.
  Rejected: a seq → hour lookup on the source.
  Spec: [SEM §7.2] (v2.21).
- G5-125 What a primary is (2026-10-02):
  a `data` record with an available, odd function; a lookup naming another record fails as an error.
  The next same-key primary is a `data` record with an odd function of the primary's capture, epoch and direction, with its SessionID and System Bytes.
  The W bit takes no part and is reported.
  Rejected: any same-direction data record; requiring W.
  Spec: [SEM §7.2] (v2.21).
- G5-126 A conflicting primary or candidate makes a lookup `incomplete` (2026-10-02):
  its versions are reported, and so is every conflict found.
  Rejected: evaluating each version as a candidate of its own.
  Spec: [SEM §7.2] (v2.21).
- G5-127 `matched` and `ambiguous` are reported as found (2026-10-02):
  every reason absence could not be established is reported beside them; only a lookup that would be `unmatched` becomes `incomplete`.
  Rationale: [SEM §7.4] reports conditions side by side, none hiding another.
  Rejected: any gap turning the result into `incomplete`.
  Spec: [SEM §7.2] (v2.21).
- G5-128 One observation per lookup (2026-10-02):
  a lookup reads scopes, per-capture evidence and barriers from one observation its source fixes before the lookup reads any record.
  Rationale: evidence and views taken at different instants can disagree, and a lookup cannot tell.
  Rejected: separate source calls with the inconsistency documented.
  Spec: [STO §5] Observation of a lookup (v2.21).
- G5-129 Only seqs above the primary are searched (2026-10-02):
  each scope is read once; the primary arrives before every higher seq of its capture, so the records after it are classified with the key known;
  candidates below the primary are outside every window and are not searched for.
  Rejected: reading the primary's scope twice behind a seq filter.
  Spec: [SEM §7.2] (v2.21).
- G5-130 Epoch-wide conditions (2026-10-02):
  a capture-boundary in the primary's epoch comes from the per-capture evidence, over the whole epoch;
  an `ordering-uncertain` record of the epoch is found only in the scopes a lookup reads, since no index records it per epoch.
  Rejected: every epoch not read in full counting against `unmatched`; both conditions only in the scopes read.
  Spec: [SEM §6], [SEM §7.2] (v2.21).
- G5-131 `(*Reader).Stats()` waits for 5c2 (2026-10-02):
  the F-5 statistics a catalog builds per-capture evidence from are designed with the evidence provider.
  Spec: `tracepack-go.md` §3 (v2.21).
- G5-132 Any conflict at or above the primary makes a lookup `incomplete` (2026-10-02):
  a conflict on a seq at or above the primary's, within a scope or across scopes, inside the window or after it, whatever its versions are.
  Rationale: deciding which conflicts matter kept leaving cases where a disputed record bounded or emptied a window; conflicts are pipeline defects and rare.
  Rejected: refining which conflicts are relevant.
  Spec: [SEM §7.2] (v2.21).
- G5-133 A conflict across scopes is reported by seq and hours (2026-10-02):
  a lookup reports the hours whose reads yielded the seq and the versions it kept;
  a version it judged irrelevant before the other one appeared is not kept.
  This relaxes G5-126 for conflicts across scopes.
  Rejected: keeping every body and holder across scopes.
  Spec: [SEM §7.2] (v2.21).
- G5-134 A block whose records disagree with its F-2 entry makes a lookup `incomplete` (2026-10-02):
  also when a match was found, because such a block can break seq order and identity ([SEM §7.4]); an exception to G5-127.
  Rejected: reporting it beside the result only.
  Spec: [SEM §7.2] (v2.21).
- G5-135 Only records read establish a window bound or epoch closure (2026-10-03):
  a footer's `close_seq` and `boundary` entries, and the per-capture evidence built from them, are claims whose structure footer validation proves, not their meaning;
  a lookup's window is bounded only by records it read — a same-key primary, or a closing record (a socket-close of the primary's epoch, or a clean `stop` of any epoch) —
  and only a closing record of the primary's own epoch that it read exempts that epoch from the `stop-unclean` barrier;
  a claim that a record it read contradicts is reported.
  Rationale: a false `close_seq` could empty a window and make a lookup `unmatched`.
  Rejected: trusting the evidence and checking it only when its record happens to be read.
  Spec: [SEM §7.2], [STO §5] Completeness, [STO §8] (v2.21).
- G5-136 A lookup compares the copies in the scopes it reads (2026-10-03):
  a version of a record can lie in any hour, its `ts_utc_ns` deciding its scope, so no bounded read excludes another version of a window seq;
  `unmatched` and `matched` hold for the scopes read, as [SEM §7.4]'s guarantees hold for the packs read, and the spec and the Go documentation say so.
  Rationale: two versions of one record are a pipeline defect; a seq with no version read still counts as missing.
  Such versions in different hours are compared by no per-pack `verify` and no per-scope merge, so they can stay unreported.
  Rejected: reading hours before the primary's, which reads more without closing the gap; never returning `unmatched`.
  Spec: [SEM §7.2] (v2.21).
- G5-137 An inverted barrier interval meets every time range (2026-10-03):
  a `stop-unclean` boundary whose `gap_start` exceeds its `gap_end` is a time barrier for every time range,
  as an inverted `coverage` entry intersects every query ([FMT §5]); the format does not check the order of the two bounds.
  Rationale: the rule errs toward `incomplete`, never toward a false absence.
  Rejected: swapping the bounds, which guesses the writer's intent; treating the interval as empty, which can drop a barrier.
  Spec: [STO §5] Completeness, [SEM §7.2] (v2.21).
- G5-138 A contradiction is judged by the bytes of the versions read (2026-10-03):
  an evidence closure at a seq the lookup read contradicts the records only when no version's bytes there are the closing event it names;
  when the versions conflict and one of them is that event, the lookup reports the conflict, which already makes it `incomplete` (G5-132), and no contradiction.
  Rationale: the evidence then names a record that exists; the disagreement lies between the copies, and the conflict reports it.
  Rejected: a contradiction whenever the seq holds no closing record, which reports one defect twice.
  Spec: [SEM §7.2] (v2.21).
- G5-139 A store-backed source over the caller's object store and catalog (2026-10-03):
  the reference library defines an object-store interface and a catalog interface, and implements a `PackSource` over them;
  a scope the catalog indexes takes its view from one catalog snapshot, a scope it does not index from the coherent observation and listing view of [STO §5], which the library runs.
  Premise (i) of that procedure is confirmed by the catalog per scope: the scope was not indexed at any instant from the snapshot to a check made after the scope's last listing and pack read.
  Rationale: the library cannot see a catalog's index state, and a check only before and after the listings misses a scope indexed and evicted in between.
  Rejected: a listing-only source, leaving indexed scopes to the caller; only building blocks.
  Spec: [STO §5] Observation of a lookup, `tracepack-go.md` §3 (v2.22).
- G5-140 The searched scope and `Cold` of other reads wait for a query over a source (2026-10-03):
  `Iterate` and `MergeIterate` take readers and cannot tell whether a scope is indexed; the transaction lookup reports both since v2.21.
  Rationale: the reason belongs to the component that routes a query to scopes.
  Rejected: a `Cold` reason the caller attaches; a scope-level query in phase 5c2.
  Spec: `tracepack-go.md` §3, impl plan "5c2" (v2.22).
- G5-141 The percent-encoding of `tool_id` in keys (2026-10-03):
  every byte outside the RFC 3986 unreserved set is written `%XX` with uppercase hex digits, and no other byte is; an empty `tool_id`, `.` and `..` cannot be stored.
  Rationale: one spelling per `tool_id`, reversible, never a `/`, the same in every language.
  Rejected: a language library's path escaping, which keeps some delimiters.
  Spec: [STO §3] (v2.22).
- G5-142 A conflicted scope is reported, not read (2026-10-03):
  an observation reports a scope whose view is `conflicted` as such, and a lookup reads none of its packs, lists it, and is never `unmatched`; a match found in another hour is still reported.
  A conflicted scope of the primary's hour explains a missing primary, as a scope that is not indexed does.
  Rationale: [STO §4] forbids choosing one of the two sets silently; failing the whole lookup would lose what the other hours show.
  Rejected: failing the observation.
  Spec: [STO §5], [SEM §7.2] (v2.22).
- G5-143 UUIDs in keys are canonical lowercase text (2026-10-03):
  `capture_id`, `pack_id` and a commit object's `<id>` appear in keys in the lowercase 8-4-4-4-12 form of RFC 9562, the only form a reader accepts.
  Rationale: listings select by key prefix, so one spelling is required.
  Rejected: accepting any case when reading, which a prefix listing cannot honour.
  Spec: [STO §3] (v2.22).
- G5-144 A listed pack that is gone fails the observation (2026-10-03, revised 2026-10-04):
  a coherent observation lists every segment of the capture; a segment's hour is known only once it is read, so one that is gone when read cannot be shown to lie outside the observed scope,
  and the observation fails, retriably.
  Rationale: skipping it is sound only while every component conforms, and a result is never silently smaller ([STO §5]); the race needs a deletion between a listing and a read, so a new observation normally succeeds.
  Rejected: skipping it (the first version of this decision); asking the catalog to classify the missing pack.
  Spec: [STO §5] Coherent observation (v2.22).
- G5-145 The object store bounds its own reads (2026-10-03):
  the reference library reads objects through `io.ReaderAt`; the adapter bounds each read by its own deadline and supports concurrent reads, and closing an object never waits on remote I/O.
  Rationale: the reader API takes no context per read; threading one through it changes every read path.
  Rejected: a context-aware read interface.
  Spec: `tracepack-go.md` §3 (v2.22).
- G5-146 A record outside its scope's hour makes a lookup `incomplete` (2026-10-04):
  a record whose `ts_utc_ns` lies outside the hour of the scope it was read from breaches the scope ([FMT I-13], [STO §2]);
  the lookup reports it and returns `incomplete`, even beside a match, and in the primary's scope it explains a missing primary as a read defect does.
  Rationale: such a record could be read as the primary or the reply of the wrong hour; a merge refuses such a pack, but a read takes records as they come.
  Rejected: failing the lookup with an error; rejecting such packs when the observation is taken, which cannot see the records of a pack without a used footer.
  Spec: [SEM §7.2] (v2.22).
- G5-147 A segment whose first seq damage hides is not checked against its key's `seq_first` (2026-10-04):
  a source that checks a segment's key against the pack compares `seq_first` with the first seq of the first block it finds, and with `seq_start` only when the pack is known to hold no record;
  when no block is found and the pack is not known to be empty, it skips the comparison, and the read reports the damage.
  Rationale: a damaged segment is a gap of the reads over it ([STO §5] Completeness), not a failure of the whole observation, which a retry would not cure.
  Rejected: comparing with `seq_start` whenever no block is found; failing the observation whenever the first seq is unknown.
  Spec: [STO §3] (v2.22).
- G5-148 An exported `ErrInvalidKey` (2026-10-04):
  a key the builders cannot build, and a key a source lists or a catalog names that does not parse as a key of [STO §3], is an error wrapping `ErrInvalidKey`, kept by the error of a failed `Observe`.
  Rationale: a caller tells malformed bucket contents, which a retry does not cure, from a missing object (`ErrObjectNotFound`), which it may; the package exports a sentinel for each kind of malformed input it reads.
  Rejected: an unexported error; wrapping `ErrInvalidQuery`, which names invalid query arguments.
  Spec: `tracepack-go.md` §3 (v2.22).
- G5-149 A `start`, `stop` or `stop-unclean` boundary carries no seq range (2026-10-04, proposal P11 Q1):
  the record's own seq places such a boundary, and its payload holds neither `boundary_seq_first` nor `boundary_seq_last`; the seq range stays with a `gap`.
  Rationale: [SEM §5] asked for a seq range without saying what a start or stop delimits; a writer that marshals the payload before the seq is assigned, or before a clock-step takes the seq ahead of it, cannot name one.
  Rejected: the range of the capture's records, which a `start` cannot know and a `stop` would have to predict.
  Spec: [SEM §5], [FMT §8] (v2.23).
- G5-150 A recorder without a durable spool (2026-10-04, proposal P11 Q2):
  a recorder may write segments without the durable spool and liveness anchor of [STO §4] Flush;
  it keeps every other Flush rule, writes no `flush_interval_ns`, and a crash loses the records not yet in a finalized segment:
  its capture stays `open`, with no `stop-unclean` barrier and no gap bounds, and the lost records are seq gaps.
  Rationale: the reference library offers a segment writer before `Recover` exists (G5-91); the losses it allows are ones [STO §5] Completeness already reports, as for a durable-bus producer that crashes.
  Rejected: writing `flush_interval_ns` without the contract it states; refusing to offer a segment writer until `Recover` is built.
  Spec: [STO §4] Flush, [STO §5] Completeness (v2.23).
- G5-151 Any `capture_origin_mono_ns` value is valid, 0 included (2026-10-04, proposal P11 Q3):
  only differences of monotonic readings carry meaning, so a producer without a raw monotonic reading writes 0 and measures `mono_ns` from its origin instant.
  Rationale: Go's `time.Time` carries a monotonic reading it never exposes; the question was open since impl plan phase 8.
  Rejected: requiring a raw reading, which a Go producer cannot supply.
  Spec: [SEM §4] (v2.23).
- G5-152 The `Writer` refuses a scope breach and validates by default (2026-10-04, proposal P11):
  for every pack but an `extract`, `NewWriter` refuses a period that is empty or not inside one UTC hour,
  and `Append` refuses a record whose `ts_utc_ns` lies in another UTC hour than the period, with `ErrScopeBreach`, the Writer still usable;
  `WriterOptions.Validate` becomes `SkipValidation`.
  Rationale: the merge refuses such a pack and a lookup over it is `incomplete` (G5-146), so accepting the record moved the failure to another component; the zero value should be the recommended setting ([FMT §12]).
  Rejected: checking the record against the period rather than its hour, which a same-hour clock step breaks ([STO §4] Merge); flipping the `Codec` default, whose zero value is the wire value of `none`.
  Spec: `tracepack-go.md` §3 (v2.23).
- G5-153 A capture descriptor and a rolling segment writer (2026-10-04, proposal P11):
  `CaptureDescriptor` holds the pack metadata [STO §4] gives the producer, with `recorder_instance_id` required and never generated;
  `SegmentWriter` writes one capture as segments, owning every value [STO §4] gives the writing component, the `capture_id` and the seq counter;
  it opens a segment at the first record that needs one, rolls by the Flush rules, writes `start` before the capture's first record and `stop` at `Close`,
  defaults to zstd and validation, takes the classifier as an interface, and writes through a `SegmentSink` (`NewDirSink` for a local directory laid out as the bucket, its unfinished files outside every key area);
  its frame helper serves capture methods whose bytes establish the fields they hold, never a `log` capture, and sets `mono_ns` only for a `capture-clock` capture.
  Rationale: a recorder built on `Writer` alone had to compute the period, the hour rolls, the seq threading and the commitments by hand, and every mistake surfaced in another component.
  Rejected: presets on `PackMeta`, which leave the rolls to the caller; a durable spool in this step (G5-91).
  Spec: `tracepack-go.md` §3 (v2.23).
- G5-154 A `PackSource` over open readers (2026-10-04, proposal P11 Q4, Q5):
  `NewReaderSource` serves a lookup from the `Reader`s the caller opened;
  its scopes are not indexed and its evidence partial, so a lookup over them is never `unmatched`,
  unless the caller asserts `Complete`, that the readers hold every accepted pack of the tool, which the barriers of other captures need;
  even then a capture's evidence is partial while any reader of the tool has no used footer;
  every generation and patch given counts as committed, and giving one asserts that it was accepted.
  Rationale: local files have no catalog to vouch for completeness, and the caller who gathered them is the only one who can; an unasserted set must not prove absence.
  Rejected: indexed by default; requiring commit objects next to local files.
  Spec: `tracepack-go.md` §3 (v2.23).
- G5-155 One package, grouped by audience in its documentation (2026-10-04, proposal P11 Q6):
  the module keeps one `tracepack` package; its README and package documentation group the API for recorders, readers and the storage service.
  Rationale: the storage-service types share unexported state with the reader, so a split costs more than it saves before v1.0.0.
  Rejected: sub-packages per audience.
  Spec: `tracepack-go.md` §3 (v2.23).
- G5-156 A complete set of local packs includes the evidence a catalog would hold (2026-10-05, phase 5d plan review):
  `NewReaderSource` takes, beside the readers of its views, readers whose statistics it folds into the evidence only (`Evidence`), never into a view;
  `Complete` asserts that the view readers are every accepted pack of the tool and that both sets together are every pack a catalog of the tool would hold evidence of, rejected registrations included,
  and a set from which a pack was removed cannot be `Complete`.
  Rationale: [STO §5] Per capture keeps the evidence of every pack presented for registration, rejected ones included, and keeps it after the packs are gone;
  a `stop-unclean` boundary in a rejected segment is still a barrier, so a set of accepted files alone could let a lookup return `unmatched` (plan review r1).
  Rejected: folding a rejected segment into a view, which would admit its records; treating every accepted file set as complete (G5-154 as first written).
  Spec: `tracepack-go.md` §3 (v2.24).
- G5-157 `NewDirSink` modes and the `.partial` prefix (2026-10-06, phase 5d implementation review):
  `NewDirSink` creates files with mode 0640 and directories with mode 0750, and refuses a prefix whose first component is `.partial`.
  Rationale: a segment holds records in full, so no other user may read it; group read lets an uploader or merger running as another user read the files through a setgid group or a default ACL, which an owner-only mode would mask.
  A committed segment under `.partial/` would be deleted with the crash leftovers there.
  Rejected: 0600/0700, which rules out group and ACL access; 0644/0755, world-readable under a usual umask; a Godoc caution instead of the refusal.
  Spec: `tracepack-go.md` §3 (v2.24).
- G5-158 An evidence reader's scope is not indexed (2026-10-06, phase 5d implementation review):
  under `Complete`, `NewReaderSource` reports the scope of each evidence reader, its `capture_id` and the UTC hour of its period, as not indexed; the scope's view still holds only view readers.
  Rationale: a catalog rejects a registration only for a scope that is not indexed ([STO §5] No admissions outside the index), and a lookup through the catalog reads such a scope as `cold`;
  reporting it indexed claims more than the catalog would, and a rejected segment holding another version of a seq in the view could let a lookup return `unmatched`.
  Rejected: keeping the scope indexed with a Godoc note.
  Spec: `tracepack-go.md` §3 (v2.24).
- G5-159 `NewDirSink` stays inside its root (2026-10-06, phase 5d post-implementation review):
  `NewDirSink` performs every file-system operation through its root opened as an `os.Root`, so a symbolic link leading out of the root fails the operation that meets it.
  Rationale: a lexical join alone follows an existing link such as `root/staging` or `root/.partial` out of the root, publishing a segment, or keeping a temporary file, where the sink claims none lies.
  Rejected: documenting the lexical join and leaving the root's contents to the operator; a link check before each operation, which a replacement between check and use defeats.
  Spec: `tracepack-go.md` §3 (v2.24).
- G5-160 `SegmentWriter` refuses a malformed event or annotation payload (2026-10-06, phase 5d post-implementation review):
  each append decodes the payload of a transport-event or annotation record and refuses, with `ErrInvalidRecord` and before any mutation, one that does not decode or whose `field_validity` is not 0.
  Rationale: the writer otherwise commits a payload `Verify` reports as a writer defect, so a recorder learns of its mistake only when a reader checks the pack;
  and it writes 0 for such a record's `field_validity`, so a nonzero value the caller set would be dropped silently.
  Rejected: documenting the payload as the caller's responsibility.
  Spec: `tracepack-go.md` §3 (v2.24).
- G5-161 `NewDirSink` refuses links where it manages directories (2026-10-06, phase 5d post-implementation review r2):
  `NewDirSink` refuses a symbolic link at `<root>/.partial` and at every directory component of a key, checked after creating them and before the rename, and fails on `js`.
  Its checks guard against a misconfigured root, not against a concurrent writer of `root`, which can remove segments anyway.
  Rationale: `os.Root` keeps every operation inside `root` but follows links within it, so a link such as `staging` → `.partial` would publish a segment among the temporary files;
  on `js` `os.Root` checks a link before using the path, so a replaced link escapes `root`.
  Rejected: a traversal holding each directory handle and refusing links at every step, which needs platform-specific code to guard against an actor that can delete segments anyway; documenting links as the operator's concern.
  Spec: `tracepack-go.md` §3 (v2.24).
- G5-162 The retention boundary is a provider the caller supplies (2026-10-07, phase 5c3):
  a `Retention` interface, `RetainedFrom(ctx) (int64, error)`, returns the first UTC hour still retained; `Query.Retention`, `TxOptions.Retention` and `StoreSourceOptions.Retention` take one, nil removing nothing; it is asked again at every check.
  Rationale: the boundary advances while a read runs, so a fixed value given at the start cannot express [STO §5]'s checks before each listing and each block emitted.
  Rejected: a fixed hour per read.
  Spec: `tracepack-go.md` §3 (v2.25).
- G5-163 A lookup whose primary's hour is removed fails with `ErrRemoved` (2026-10-07, phase 5c3):
  the lookup schedules hours from the primary's forward and the boundary never moves back, so every removal a lookup meets implies a removed primary hour; there is no removed gap and no removed list in `TxResult`.
  Rationale: without its primary a lookup has no result to report.
  Rejected: an incomplete outcome for a removed later hour, which cannot occur.
  Spec: `tracepack-go.md` §3 (v2.25).
- G5-164 A read that meets a removed hour ends whole (2026-10-07, phase 5c3, re-decided after plan review r1):
  `Iterate` and `MergeIterate` end with an error wrapping `ErrRemoved` as soon as an hour the read covers is removed, beside the `Result` found so far with `Result.Removed`; [STO §5] lets a reader end its whole read this way.
  Rationale: ending only the hour and reading the others on needs every held block, queued record, conflict version and defect of the hour invalidated mid-read.
  Rejected: listing the hour and reading the other hours on (the first choice, withdrawn after plan review r1).
  Spec: `tracepack-go.md` §3, `tracepack-storage.md` §5 (v2.25).
- G5-165 Retention in the library is the reader side only (2026-10-07, phase 5c3):
  reads, the lookup and `NewStoreSource`'s `Observe` check the boundary, and `Observe` reports a removed hour as `SourceScope.Removed` instead of failing; deleting a removed hour's objects stays the service's work, and `NewReaderSource` takes no provider.
  Rationale: the library reads; the deleting component owns the order of physical deletion.
  Rejected: a deletion helper over `ObjectStore`, which needs a `Delete` method.
  Spec: `tracepack-go.md` §3 (v2.25).
- G5-166 Phase 6 splits into three sub-phases (2026-10-09, phase 6a):
  6a JSONL export and schema, 6b conformance corpus, 6c CLI, each with its own plan, reviews and pull request, in the order 6a → 6b → 6c → phase 7;
  whether 6b also covers the vectors of [SEM §9] and [STO §8] is decided when 6b is planned.
  Rationale: the corpus needs the export, and the CLI needs both; one plan for all three is too large to review.
  Rejected: one phase and one pull request; phase 7 before the corpus (its vectors and its sweep are built on the corpus generator).
  Spec: `tracepack-impl-plan.md` §3 (v2.26).
- G5-167 64-bit integers are decimal strings in the JSONL export (2026-10-09, phase 6a):
  every `u64` and `i64` value is a JSON string of its decimal form; `u8`, `u16` and `u32` values are JSON numbers.
  Rationale: nanosecond timestamps exceed 2^53, which parsers that read JSON numbers as IEEE doubles (JavaScript, jq) cannot hold.
  Rejected: JSON numbers throughout.
  Spec: [JSONL §3] (v2.26).
- G5-168 `ExportJSONL` lives in package `tracepack` (2026-10-09, phase 6a):
  there is no `tracepack/jsonl` package.
  Rationale: the Go API contract already named it there, it keeps the one-package documentation of G5-155, and the export reads stored values `Record` does not expose (the reserved bits of `record_flags`).
  Rejected: a `tracepack/jsonl` package, which needs new exported accessors for those values.
  Spec: `tracepack-go.md` §3, `tracepack-impl-plan.md` §2 (v2.26).
- G5-169 The export of a damaged pack holds its validated blocks only (2026-10-09, phase 6a):
  the header line, then the records of every validated block in file order; a failed block is left out without a mark, and there is no summary line.
  Rationale: the `verify` report, a separate golden of the corpus, says what was lost, and `ExportJSONL`'s `Result` says the export is incomplete.
  Rejected: a trailing summary line; refusing packs that are not `finalized-consistent`, which leaves the damaged corpus vectors without JSONL.
  Spec: [FMT §15], [JSONL §7] (v2.26).
- G5-170 The CLI reads local paths only for now (2026-10-09, phase 6a):
  `s3://` sources are deferred until the query service or another design decides where the object-store dependency lives.
  Rationale: the CLI shares the library's `go.mod`, so an object-store SDK would reach every library consumer's module graph.
  Rejected: `s3://` in phase 6c; a separate module for the CLI, which reverses G5-77's single nested module.
  Spec: `tracepack-go.md` §7, `tracepack-impl-plan.md` §3 (v2.26).
- G5-171 A reader budget that keeps part of a pack out fails the export (2026-10-09, phase 6a plan review r1):
  an exporter that cannot read the footer, locate a block by the forward walk or read a block because of its own resource limits fails instead of leaving them out;
  `ExportJSONL` returns an error wrapping `ErrReadLimit`.
  Rationale: an export that succeeds is then determined by the pack's bytes alone, whatever the reader's budgets; damage stays under G5-169.
  Rejected: listing the refused block as `incomplete` as `Iterate` does, which makes two successful exports of one pack differ.
  Spec: [JSONL §1], [JSONL §7], `tracepack-go.md` §3 (v2.26).
- G5-172 Export lines are written in bounded pieces (2026-10-09, phase 6a plan review r1):
  a line is passed to the writer in pieces of at most 64 KiB, so its length does not bound the memory used, and every line can be exported on 32-bit platforms.
  Rationale: a payload of NULs escapes to six bytes per byte, so one line can pass 2 GiB.
  Rejected: a limit on a line's length with an error, which leaves some valid packs without an export.
  Spec: `tracepack-go.md` §3 (v2.26).
- G5-173 Phase 6b splits into three (2026-10-09, phase 6b1):
  6b1 holds the [FMT §16] vectors except the redaction vectors and the two of G5-182, the single-pack vectors of [SEM §9] and the query-vector format;
  6b2 the rest of [SEM §9] (reads over several packs, transaction lookups) with their result formats;
  6b3 the [STO §8] store vectors and a store snapshot format, and it may come after phase 7; 6c may start once 6b1 is done.
  Rationale *(editorial)*: the multi-pack, lookup and store vectors each need a result format of their own, and one plan for all of them is too large to review; the CLI needs only single-pack goldens.
  Rejected *(editorial)*: one 6b over [FMT §16], [SEM §9] and [STO §8]; 6b limited to [FMT §16], leaving the single-pack [SEM §9] vectors to a later phase.
  Spec: [CORPUS §9], `tracepack-impl-plan.md` §3 (v2.27).
- G5-174 Injectable writer clock (2026-10-09, phase 6b1):
  `WriterOptions.Now func() time.Time`, nil meaning `time.Now`, stamps `writer_start_utc_ns` and gives the time part of a `pack_id` or `capture_id` the writer generates (UUIDv7);
  `SegmentWriterOptions.Now` reaches each segment's `Writer`, and the `capture_id` and segment `pack_id`s the segment writer generates before that `Writer` exists.
  Rationale *(editorial)*: `writer_start_utc_ns` and the generated ids are in every pack's bytes, so without a clock of the caller's no pack regenerates byte-identically (G5-175).
  Rejected *(editorial)*: patching the file header and its CRC after writing, which compares a pack the `Writer` did not write.
  Spec: `tracepack-go.md` §3 (v2.27).
- G5-175 The corpus regenerates deterministically (2026-10-09, phase 6b1):
  with pinned dependency versions it regenerates byte-identically, `.tpk` files included;
  across dependency versions its decoded content and its codec-`none` packs stay identical, as G5-180 refines.
  Rationale *(editorial)*: a byte-identical regeneration shows the generator deterministic and keeps `-update` diffs reviewable; a zstd encoder's output is not fixed across versions.
  Rejected *(editorial)*: byte-identical goldens under every dependency version, which no zstd encoder promises; decoded comparison alone, which leaves the stored bytes unchecked.
  Spec: [CORPUS §6.1] (v2.27).
- G5-176 Canonical JSON goldens (2026-10-09, phase 6b1):
  verify reports, query results and bootstrap rejections are canonical JSON whose schemas the spec defines: codes, offsets, seqs and counts, never error text.
  Rationale *(editorial)*: error text differs between implementations, and between two reads of one damaged zstd stream (phase 6a), so only a schema lets every language compare.
  Rejected *(editorial)*: goldens of error text or of the Go values' printed form.
  Spec: [CORPUS §4], [CORPUS §5] (v2.27).
- G5-177 The corpus lives in `tracepack/testdata/corpus/` (2026-10-09, phase 6b1):
  it ships in the module zip, and stays under 2,000,000 bytes.
  Rationale *(editorial)*: an implementation in another language takes the corpus from the module or the repository without another download; the bound keeps the module zip small.
  Rejected *(editorial)*: a separate repository or release asset; a directory left out of the module zip.
  Spec: `tracepack-impl-plan.md` §3 (v2.27).
- G5-178 A truncation vector is a base pack and a table (2026-10-09, phase 6b1):
  one base pack plus the expectations of every cut length, consecutive lengths with equal expectations merged; implementations make the cuts themselves.
  Rationale *(editorial)*: the cuts are derived from the base pack, and one file per cut multiplies the corpus by the pack's size.
  Rejected *(editorial)*: one stored pack per cut.
  Spec: [FMT §16], [CORPUS §5.7] (v2.27).
- G5-179 Repair and merge clocks (2026-10-09, phase 6b1):
  `RepairOptions.Now` and `MergeOptions.Now` follow G5-174's contract, so a patch's and a generation's `writer_start_utc_ns` are reproducible;
  `Merge` also takes the time part of the replacement-set and pack ids it generates from it, and keeps the ids a caller supplies.
  Rationale: the repair vectors hold the patch's bytes, which carry the time it was written.
  Rejected *(editorial)*: leaving the patch's file header out of the comparison.
  Spec: `tracepack-go.md` §3 (v2.27).
- G5-180 Offsets in zstd goldens (2026-10-09, phase 6b1):
  the committed goldens are always checked strictly against the committed `.tpk` on the read path, which does not depend on the zstd encoder;
  when the `klauspost/compress` version differs from the one the manifest records, a regenerated zstd pack is compared by decoded equivalence only
  (the same metadata bytes, the same decoded block bodies, an identical `export.jsonl`) and its offset-bearing goldens are skipped, with a note that `-update` is due;
  every vector uses codec `none` unless the codec is its subject, and damaged zstd blocks are frames built by hand, never encoder output.
  Rationale: the offsets of a zstd pack follow its compressed sizes, which change with the encoder version, while what a reader must produce from the committed bytes does not.
  Rejected: altering encoder output to damage it, which cannot prove that the header section stays readable (plan review r2).
  Rejected *(editorial)*: failing the corpus test whenever the encoder version changes.
  Spec: [CORPUS §6], `tracepack-impl-plan.md` §5 (v2.27).
- G5-181 Format-22 items in item validity (2026-10-09, phase 6b1):
  a format-22 (localized string) item of length 0 is valid, of length 1 invalid, of length 2 or more valid whatever its parity; no check depends on the encoding.
  Rationale: a non-empty localized string starts with its 2-byte header (E5 §9.4), as [SEM §8] already said for redaction.
  Rationale *(editorial)*: the encodings are open-ended, so an encoding check would tie validity to a table.
  Rejected *(editorial)*: an even length, which only 2-byte encodings imply; validating the string against the encoding its header names.
  Spec: [SEM §3], [SEM §8], [CORPUS §7] (v2.27).
- G5-182 Two [FMT §16] vectors move to 6b2 (2026-10-09, phase 6b1):
  "a payload-only identity conflict between two packs" and "transaction candidate selection ([SEM §7.2])" need 6b2's result formats;
  6b1 keeps the single-pack S/F, SessionID and System Bytes predicates.
  Rationale: both are reads over several packs or lookups, whose results have no canonical form before 6b2.
  Rejected *(editorial)*: defining those result formats in 6b1.
  Spec: [FMT §16], [CORPUS §9] (v2.27).
- G5-183 Repair reports nothing to repair before its refusals (2026-10-09, phase 6b1 plan review r1):
  a `finalized-consistent` pack without the `seq_start` defect is reported as having nothing to repair (`ErrRepairNotNeeded`) before any refusal condition is checked, an extract's role included, in the order [FMT §13] lists them;
  among the refusals no precedence is defined, so each refusal vector meets one refusal condition where the conditions can be separated,
  and an hour-span vector, whose block also lies outside the scope's hour, cites both, the coarse `not-repairable` result needing no winner (plan review r2).
  Rationale *(editorial)*: a pack with nothing to repair is not a failure, and every `finalized-consistent` pack then gets one answer whatever its role.
  Rejected: freezing the implementation's order, which checked the role first while the spec stated none (plan review r1); a precedence among the refusals.
  Spec: [FMT §13], `tracepack-go.md` §3 (v2.27).
- G5-184 The forward walk stops at any failing envelope (2026-10-09, phase 6b1):
  without a valid footer, the forward walk stops at an envelope that fails any check of the block envelope — its magic, its CRC, or a field outside its range (`first_seq` above 2^63 − 1, `record_header_len` below 44, and the like) —
  so an envelope whose CRC holds but whose field is out of range stops the walk, and no block after it is located.
  Rationale *(editorial)*: without a valid footer, an envelope's fields have nothing to confirm them, and locating blocks after an envelope the walk cannot account for is deferred ([FMT §13]);
  the rule is also the reader's existing behaviour.
  *(editorial)* The walk also stops where the bytes left before the end of the block region cannot hold an envelope, and at an envelope whose body ends past that end;
  both stops were already in force, and [FMT §13] names them beside this rule.
  Rejected: continuing the walk by `body_len` past a CRC-valid envelope with an out-of-range field, failing only that block.
  Spec: [FMT §13] (v2.28).
- G5-185 Neutral names for the log source and the gateway (2026-10-10):
  the specification names the log source and the producers that feed the log service in neutral terms:
  the equipment automation program's log is the EAP log, and its converter is `eapconv` in `veq/cmd/eapconv`;
  the site's gateway is the equipment gateway,
  and its parts are named for what they do: its HSMS device, a message-encoding adapter and the EAP bus adapter.
  Earlier entries of this log are reworded to these names, their decisions unchanged:
  G3-20, G3-PLACE, G3-27, G4-28, R3-6, G5-80, G5-82, G5-85 and G5-86;
  generic uses of "tap" (a network tap, a connection-level tap) stay.
  Rationale *(editorial)*: the earlier names were a site's own, and the specification is meant to be read outside that site.
  Spec: `tracepack-go.md` §1, §2, §6 and §8, `tracepack-impl-plan.md` §1, proposal P8 (editorial, spec version unchanged).
- G5-186 A log of several tools converts into one capture per tool (2026-10-10):
  a converter run over an input that holds the traffic of several tools writes one capture per tool, each holding only its own tool's entries and lines;
  entries and lines that belong to no tool, such as the logging program's own, form one more capture,
  whose `tool_id` is the logging program's identity as configured in the converter, distinct from every tool's.
  The nothing-dropped rule holds over the run: every input byte range is covered by exactly one record or annotation of exactly one of its captures,
  and no capture repeats another's bytes as annotations.
  `capture_id` is one per `tool_id` per run per source file set, as [FMT I-7] already assigns it per tool;
  the corpus `spec_version` follows the spec version although no golden changes.
  (2026-10-10: the per-run part is superseded by G5-192 — a reproducible re-run MAY reuse the capture.)
  Rationale *(editorial)*: a capture belongs to one tool ([FMT §5] `tool_id`), so a log of several tools cannot be one capture,
  and copying every other tool's lines into each capture would multiply the stored bytes by the number of tools.
  Rejected: every capture covering the whole input, with the other tools' and the unrelated lines as annotations;
  leaving the lines of no tool out of every capture with a count only; one capture per run.
  Spec: [STO §7] items 7 and 8, [STO §8], `tracepack-go.md` §6 (v2.29).
- G5-187 The captures of one run list the same source files (2026-10-10):
  every pack of every capture a converter run writes lists the run's source files as the same `source_ref` entries in the same order,
  so an annotation's `source_index` names the same file in every capture and the run-wide coverage of G5-186 can be checked.
  Rationale *(editorial)*: without it, `source_index` is only meaningful within one pack, and coverage across captures cannot be related.
  Rejected *(editorial)*: leaving the order to each converter, with the check matching files by their `source_ref` text.
  Spec: [STO §7] item 8, [STO §8] (v2.29).
- G5-188 The `multi-pack` corpus class (2026-10-10, ratifying the corpus part 2 as delivered in v2.30):
  a vector of reads over several packs or of transaction lookups is of class `multi-pack`:
  numbered packs `pack-<n>.tpk`, each with its verification report `pack-<n>.verify.json`, and `reads.json`, `lookups.json` or both, with no export;
  group prefixes `multi-` and `tx-`; the corpus schema becomes `tracepack-corpus/2`, so a `/1` consumer does not read the new class silently.
  A pack is named by its number n in every result of the class;
  the numbers are local to the vector — never a `pack_id`, a position in an API's reader list or a storage key — and an implementation maps them by opening the packs, so no runtime API or key changes.
  The vector-directory budgets (`multi-` 120,000 bytes, `tx-` 330,000) and the raised root rows are repository maintenance ceilings a test enforces, never reader or format limits.
  [FMT §16] leaves the files of a vector to [CORPUS]'s rules per class.
  Spec v2.30 is accepted for what it delivered; the rulings below are recorded in v2.31.
  Rationale: self-contained vectors keep the corpus usable by other implementations, and a per-pack export would multiply its size for little coverage, per-pack verification and the returned records' bytes being checked instead.
  Rejected: one class per file kind; reusing the packs of other vectors by reference; `pack_id` strings in the results, which a read given its packs in two orders would name two ways.
  Spec: [CORPUS §2], [CORPUS §3], [CORPUS §4], [CORPUS §10], [FMT §16] (v2.30).
- G5-189 Reads over several packs in the corpus (2026-10-10, ratifying v2.30):
  `reads.json` gives each read's packs in a stated order, its order, filter and optional conflict bound,
  and expects the items in yield order, each pack's `incomplete` reasons, the conflicts in discovery order and the footers not used,
  or the `conflict-limit` error with the conflicts found so far, items yielded before the error not compared.
  The conflict bound is a parameter the read states, the one exception to [CORPUS §1]'s rule that a reader's own limits are never goldens;
  the exception covers `reads.json` `max_conflicts` only: at bound N, N conflicts succeed and an N + 1-th fails, reporting the first N;
  it sets no precedent for a lookup's state, held bytes or default reader limits.
  Pruning stays optional: no read depends on a whole cluster being excluded;
  read counts and a cluster excluded whole stay tests of the reference implementation, and the catalogue says so.
  Rationale: [SEM §7.4] defines the bound's outcome deterministically, while pruning is an optimisation [SEM] permits and does not require.
  Rejected: leaving the bounded reads out of the corpus; a [SEM §7.4] rule requiring exclusion by the F-2 time range, which would make conforming readers nonconforming.
  Spec: [CORPUS §1], [CORPUS §5.10], [CORPUS §9] (v2.30).
- G5-190 Lookups over a set of packs (2026-10-10, ratifying v2.30):
  a lookup vector names its source as packs of the vector — ordered view packs, evidence packs and whether the source is complete — read as [CORPUS §8] states,
  only `view ∪ evidence` supplying scopes, evidence and barriers, other packs of the vector having no effect;
  the source's assumed commits are premises of the fixture, not a way around a real admission;
  snapshots, listings and other catalog or bucket behaviour stay with the store vectors.
  `lookups.json` expects the outcome, the primary's fields each present as available, the window's end, every kept version in a stated order with its role labels and flags,
  the scopes searched, their conflicts and unused footers, and the facts of G5-191.
  The role labels, `closing` among them, are diagnostics [CORPUS §5.11] defines from a version's bytes; [SEM §7.2]'s predicates, conflict-free bounding and closure of the primary's epoch, stay separate and unchanged.
  An extract read beside its source moves to phase 7, with redaction.
  Rationale: the lookup's semantics are tested apart from the store, whose fixtures belong to the store vectors.
  Rejected: a store snapshot consumed by a store source in this part; an extract written without the redaction entries a conforming extract has.
  Spec: [CORPUS §5.11], [CORPUS §8], [SEM §9] (v2.30).
- G5-191 Canonical lookup facts and early returns (2026-10-10, ratifying v2.30 with two qualifications):
  a lookup's facts are canonical: one per condition of [SEM §7.2] and [STO §5] Completeness that keeps the lookup from establishing absence, identical facts collapsed;
  a barrier is one fact with its bases; claim-specific contradictions; the existence facts carry no witness; a `capture-boundary` is identified by capture, seq, kind and epoch; one total order.
  Canonicalization may drop duplicate representations and witness details, never a semantic fact.
  This is ratified with its implementation closed by G5-194 and G5-195 and their regression vectors.
  A lookup whose primary is missing, conflicts within its scope's read, or has no association key returns early:
  the result reports the listed facts of the completed read of the primary's scope and the primary's failure,
  and does not perform the completeness evaluation of a keyed lookup, whether or not an omitted fact depends on classifying versions (partial evidence does not);
  the scope read's conflicts and unused footers stay in their own result fields.
  Rationale: canonical facts compare across implementations without freezing one implementation's diagnostics.
  Rejected: weakening a fact to match an implementation's omission or hiding it in normalization; describing every omitted fact as one that depends on classification.
  Spec: [CORPUS §5.11] Facts and Early-return results, [SEM §7.2] (v2.30, v2.31).
- G5-192 Reproducible converter identities (2026-10-10):
  a converter MAY reuse a `capture_id` for a reproducible re-run, when every input that decides the capture's records, seqs and capture-level metadata is unchanged,
  the deterministic derivation of the id covering at least:
  a digest of each source file's contents, not only its name, in the source order; the `tool_id` and tool attribution; the dialect, time zone and every other interpretation setting; and the converter's, parser's and classifier's versions.
  It MAY reuse a `pack_id` only for byte-identical finalized output:
  the inputs behind that identity also cover the capture identity, the hour and partition, every metadata value, the writer's, tracepack's and codec's versions and options, the block partitioning, and a `writer_start_utc_ns` the conversion fixes.
  An existing id that names different bytes fails the conversion, which never overwrites the file.
  A deterministic id is an exception to [FMT §2]'s UUIDv7 recommendation, and readers do not depend on ids being ordered by creation time.
  A conversion whose inputs change is a new import with a new capture; no rule reconciles two captures of the same traffic.
  This supersedes the "per converter run" of G5-186: the capture is one per `tool_id` per source file set per logical conversion.
  Rationale *(editorial)*: a retried import then neither duplicates captures nor files, while a reused id still names one capture's records and one file's bytes.
  Rejected *(editorial)*: fresh ids on every run, which store a retry's traffic twice; a hash of the source names only, which cannot see a changed file.
  Spec: [STO §7] item 8 and Reproducible output, [FMT §2], [FMT §4], [FMT I-7] (v2.31).
- G5-193 Converter segments; a direct converter archive is a generation (2026-10-10):
  a converter MAY write generation-0 `segment` packs, each of one capture and UTC hour, possibly covering the whole hour,
  with `compaction_level` 0 and without `compacted_from`, publisher-epoch, replacement-set or `flush_interval_ns` tags;
  they are outside the recorder's flush, spool and recovery contract and use the segment keys, admission rules and active-view rules of [STO §3]–[STO §5].
  A converter MAY instead write an archive directly; it is the first generation of a scope with no accepted packs and follows a merge's generation claim, fencing and commit protocol,
  so merges and direct converter archives produce generations.
  This corrects G5-84: an archive enters a view only with a commit object ([STO §4]), and an archive rejected for a scope that is not indexed gives per-capture evidence only, its records outside every view, a listing view included.
  A converter emits start and stop once per capture, not per hourly pack, and its seqs are capture-wide.
  Rationale *(editorial)*: segments already enter listing views without commit objects, so historic imports need no new publication model.
  Rejected *(editorial)*: requiring archives, which would need historic scopes indexed before admission; an implicitly committed or generation-0 archive, a second publication model.
  Spec: [STO §2], [STO §5], [STO §6], [STO §7] item 9, [STO §8], [FMT §5] (v2.31).
- G5-194 Records yielded before the primary are classified (2026-10-10):
  when a block disagreeing with its F-2 entry yields versions of the primary's scope before the primary, the lookup classifies them once the primary is established,
  as every other version read, and checks a closure claim at their seqs;
  the disagreeing block stays an index defect that makes the result `incomplete`.
  The early returns of G5-191 stay as they are.
  Rationale *(editorial)*: what a lookup reads must not depend on the order a false index delivered it in, and the kept records, the window and the facts are conformance outputs.
  Rejected *(editorial)*: amending [SEM §7.2] to exempt those versions from classification and their seqs from claim checking, as the reference implementation did.
  Spec: [SEM §7.2] (v2.31); the reference implementation changes, no golden of v2.30 moves.
- G5-195 Only a bounding stop of the primary's epoch answers its boundary evidence (2026-10-10):
  the clean `stop` that bounds the window keeps a capture-boundary of the primary's epoch at e, from the per-capture evidence or read, from being reported only when that stop is itself of the primary's epoch;
  a bounding `stop` of another epoch still bounds the window,
  and an evidence entry of the primary's epoch claiming a `stop` at e is then reported as a `capture-boundary`, beside the contradiction its claim makes.
  Rationale *(editorial)*: a boundary is identified with its epoch ([CORPUS §5.11]), so a stop of another epoch is not the boundary the evidence names.
  Rejected *(editorial)*: ignoring the epoch for this exemption, which would give boundary identity two meanings.
  Spec: no rule changes ([SEM §7.2], [CORPUS §5.11]); the reference implementation changes.
- G5-196 A scope read that yields a seq twice reads its view again (2026-10-10):
  [SEM §7.2]'s kept set holds every version of a seq that one scope's read yielded,
  also when a block disagreeing with its F-2 entry and another block yield that seq in two places, both after the primary.
  The reference implementation, on finding a seq yielded a second time within one scope read, reads that scope's view again
  and keeps every version of each such seq of which one version has a role, in the order the read yielded them;
  the second pass belongs to the same scope read, over the same fixed view, and reports no fact, conflict or gap twice.
  Packs whose index agrees with their blocks never yield a seq twice, so a lookup over them reads each scope once and holds no more state than before.
  Rationale *(editorial)*: the result is exactly the one [SEM §7.2] defines, and only a damaged index costs a second read.
  Rejected: keeping a copy of every version without a role until the scope read ends, chosen first and replaced once its cost was shown —
  a conforming scope, up to a pack's size, charged to `MaxStateBytes`, 64 MiB by default; keeping those copies outside the budget.
  Spec: [SEM §7.2] Scopes read (v2.31).
