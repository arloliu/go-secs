# tracepack — decision log

Status: current (2026-09-29)
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

## 2026-09-27 log service review (owner answers to the log-service checkpoints)

- G5-80 Writer boundary (2026-09-27): producers (eqp-hub, tap converters and others) push records to the log service,
  and the log service is the traffic-log recorder in the sense of [STO §4] — the role of a log shipper such as fluentbit or vector.
  It assigns `capture_id` and capture-scoped `seq`, keeps the spool and liveness anchor, and applies the clock-anchor rule (superseded by G5-86);
  the observed fields of a record (`ts_utc_ns`, `mono_ns`, direction, epoch) come from the producer.
  The producer-to-service transport (ordering, retransmission, idempotency, restart detection) is service design, outside the tracepack specification.
- G5-81 Single source of truth per tool (2026-09-27): the log service records one producer per tool;
  no policy for reconciling two captures of the same traffic from different vantages is needed.
- G5-82 Live tail (2026-09-27): deferred, an advanced feature;
  the intended design is that the log service discovers eqp-hub instances through the eqp-hub controller and queries them over gRPC,
  not a tracepack read path.
- G5-83 Catalog durability (2026-09-27): the catalog database is deployed with backup and high availability by default,
  so a catalog rebuild from the bucket is a disaster path, and the per-capture end evidence that a rebuild cannot recover (P5) is accepted as a residual risk.
- G5-84 Cold-scope admissions (2026-09-27): the rejection of admissions for scopes outside the catalog window (P4) is a known limitation;
  converter archives that arrive after the window are read through listing views only.
- G5-85 go-secs support for the eqp-hub producer (2026-09-27): after the format primitives land, a go-secs proposal is written first,
  exposing the connection generation number and the read-time wall and monotonic timestamps on received messages and lifecycle events.
  Design premise: eqp-hub will emit traffic-log records either through a new `secs-recorder` device or by adding record emission to `tap_nats`;
  the proposal serves both, and the log service ingests those records (G5-80).
- G5-86 Stateless recorder over a durable bus (2026-09-27; supersedes the identity-assignment part of G5-80):
  the producer (eqp-hub's `eqp_hsms` device, a converter, or any other) assigns `capture_id` and `recorder_instance_id` per process start,
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
