# tracepack — storage profile

Status: current (2026-10-10) — v2.32, tracepack format 1.0.
External review consensus reached (review rounds recorded in `tracepack-spec-changelog.md`).
Normative for mergers, recorders, log converters and the service that deletes packs.

References: `[FMT §n]` = `tracepack-format.md`, `[SEM §n]` = `tracepack-semantics.md`, `[STO §n]` = `tracepack-storage.md`, `[OVW §n]` = `tracepack-overview.md`, `[JSONL §n]` = `tracepack-jsonl.md`, `[CORPUS §n]` = `tracepack-corpus.md`; `[FMT I-n]` = invariant I-n of the format document.
Each rule is defined in exactly one document; the others only reference it.

## 1. Scope and dependencies

This profile applies to object storage (S3-compatible, e.g. MinIO).
It defines the key layout, the pack roles in storage, the recorder and durable-bus contracts, merge, deletion and the converter contract;
the catalog, retention and publication bookkeeping are the service's (G5-201).
The format does not depend on it: reading and writing packs on a local file system needs only [FMT] and [SEM],
while recorder durability and crash recovery follow this document wherever packs are stored.

Depends on:
- [FMT I-6] immutability, [FMT I-7] capture and epoch rules, [FMT I-12] record identity and dedup, [FMT I-13] hour-aligned blocks, [FMT I-14] mergeable footer.
- [FMT §8] transport-event and annotation payload tags (boundary records, converter annotations).
- [FMT §5] pack metadata tags (`pack_role`, `scope_generation`, `publisher_epoch`, `patch_base`, replacement-set tags, `compacted_from`, `supersedes`, `coverage`, `seq_start`, `flush_interval_ns`, `clock_step_tolerance_ns`).
- [FMT §10] footer aggregation rule and validation; [FMT §13] verification, which merge input verification follows (§4).
- [FMT §6] block envelope and block body; [FMT I-2] block integrity; [FMT §12] block validation.
- [SEM §4] clock anchor and clock-step rule; [SEM §5] capture-boundary events; [SEM §6] effective quality; [SEM §7.2] transaction lookup; [SEM §7.4] reads over the packs a reader is given.
- For the log converter (§7): [SEM §2] reconstruction contract and capture model, [SEM §3] converter `decode_status` values, [SEM §4] source-log time rules,
  [FMT §7.2] record fields and `field_validity`, [FMT §9] enums.
- [SEM §8] redaction, for extracts.

## 2. Scopes and roles

A **scope** is one (`capture_id`, UTC hour).
Every pack except an `extract` holds records of exactly one scope, and its period lies inside that hour;
a pack's scope comes from its period, never from its record timestamps, so a pack without records (e.g. an archive holding only `coverage`) still has a scope.

| `pack_role` | Written by | Content | `scope_generation` |
|---|---|---|---|
| `segment` | recorder, per flush; or a converter (§7) | records of one flush interval of one scope, all of them for a local recorder, a subset for a consumer of a durable bus (§4), which may write several segments per interval; for a converter, records of one scope, possibly of the whole hour; `compaction_level = 0` | 0 |
| `archive` | merger (or a converter directly, §7) | the records of the verified inputs of one merge, deduplicated, all of one scope; or a converter's records of one (capture, hour); the role and the period do not certify that every pack or record of the scope was included | ≥ 1 |
| `repair` | no longer written (G5-202) | a pack written by the repair of earlier versions; a reader reads one like any pack it is given (§4), and a merge refuses it | 0 |
| `extract` | query service, on request | a filtered subset of one capture (`extract_filter`), masked under a redaction policy for consumers that are not privileged ([SEM §8]); never a complete period, never stored in the tiers, never a merge input, a patch or the subject of a repair | — |

An **archive** is a merge's output (§4 Merge) or a converter's direct archive (§7).
Its `scope_generation` (≥ 1) and `publisher_epoch` are the publisher's values, supplied by the service;
tracepack ranks nothing by them, and `publisher_epoch` has no fencing meaning.
Every archive carries a `replacement_set_id`, identity only (§6).

**Roles in the tiers.**
Only a pack whose `pack_role` is `segment` or `archive` is a merge input or becomes deletable under §4 Deletion.
A pack with any other role — `repair`, an unknown value, including the retired value 5 ([FMT §9]); an `extract` never enters the tiers —
is read like any pack a reader is given (§4), fails a merge that is given it as an input,
is never deleted by §4 Deletion, and is removed only with its hour by the service's retention (§5).

## 3. Keys

- Segment: `<prefix>/staging/<tool_id>/<capture_id>/<seq_first>-<pack_id>.tpk`.
- Other packs: `<prefix>/archive/<tool_id>/<YYYY>/<MM>/<DD>/<HH>/<capture_id>-<pack_id>.tpk`, hour in UTC.
- `tool_id` is percent-encoded: every byte of it outside the RFC 3986 unreserved set (`A`–`Z`, `a`–`z`, `0`–`9`, `-`, `.`, `_`, `~`) is written `%XX` with two uppercase hex digits, and no other byte is (G5-141),
  so each `tool_id` has one spelling and never yields `/`; a `tool_id` that is empty, `.` or `..` cannot be stored.
  `capture_id` and `pack_id` are written in the canonical lowercase 8-4-4-4-12 form of RFC 9562, the only form a reader accepts (G5-143).
  `seq_first` is the pack's first seq (its `seq_start` when empty) in 20-digit zero-padded decimal, so segments list in seq order.
  A source that checks a key against the segment it names compares `seq_first` with the first seq of the first block it finds, and with `seq_start` only when the segment is known to hold no record;
  when damage hides the first seq, it skips that comparison and the read reports the damage (G5-147).
- Every key contains the `pack_id`, so every upload has a unique key and no object is ever overwritten.
  Retried and duplicate uploads are harmless: records are deduplicated by (`capture_id`, `seq`) in every read ([FMT I-12]).
- Packs are removed only by §4 Deletion and by the service's retention, which removes whole hours (§5).
  Storage lifecycle rules (e.g. S3 or MinIO ILM) MAY transition objects under `archive/` to another storage class, provided they stay readable and listable;
  they MUST NOT expire any object, and MUST NOT apply to `staging/`.
- Objects under `staging/` and `archive/` hold records in full;
  only privileged components read them (recorders, the merger, the deleting component, the query service, privileged consumers).
  How access is controlled is service design; consumers that are not privileged receive masked extracts ([SEM §8]).

## 4. Reads, flush, merge, deletion

**Packs a reader is given.**
A reader is given packs by its caller — the service's catalog, a listing of the key prefixes (§3), or local files — and reads their union,
deduplicated by (`capture_id`, `seq`) ([FMT I-12]), with conflicts reported ([SEM §7.4]).
A segment and an archive holding its records may both be given; the result is the same.
Which packs to give is the caller's choice; tracepack defines no view.

- **Flush**: a recorder finalizes a segment at a configured interval that divides one hour (e.g. 5 or 10 minutes), at a size threshold,
  and whenever the next record's UTC hour differs from the current segment's, so that every segment has one scope.
  A recorder keeps unflushed records in a durable local spool under a **durability contract** with interval F = `flush_interval_ns`:
  (1) every record is durable within F of monotonic time after it is appended;
  (2) the recorder durably rewrites a **liveness anchor** next to the spool at least every F of monotonic time,
  when it starts a new spool (before the spool receives a record), and when a `clock-step` event becomes durable.
  The liveness anchor holds `capture_id`, the next seq, the seq and `mono_ns` of the last durable record, the clock anchor in force ([SEM §4]), and the current `mono_ns`.
  Recorders SHOULD validate each block before writing it ([FMT §12]); the cost is one decode per block written.
- **Recovery**: after a crash, recovery finalizes the spool's validated prefix as a segment of the **old** capture,
  appends a `stop-unclean` capture-boundary to it, uploads it, and only then starts a new capture ([FMT I-7]).
  It reconciles the spool and the liveness anchor, taking the newest of each fact:
  the boundary's seq is the largest of the spool's last record + 1, the spool's `seq_start` and the anchor's next seq;
  `m_d` is the later `mono_ns` of the last durable record in the spool or in the anchor;
  the clock anchor is the later of the anchor's clock anchor and the last `clock-step` in the spool;
  `m_l` is the anchor's current `mono_ns`.
  Records after the last durable record may have been lost, and their number is unknown,
  so seq contiguity up to a `stop-unclean` boundary proves nothing about what followed.
  Lost records were appended after `m_d` and before `m_l` + F under that clock anchor ([SEM §4]), so the boundary carries
  `gap_start` = anchor_wall + (`m_d` − anchor_mono) − `clock_step_tolerance_ns` and
  `gap_end` = anchor_wall + (`m_l` + F − anchor_mono) + `clock_step_tolerance_ns`,
  with all monotonic values capture-relative ([SEM §4]).
  A bound that recovery cannot establish — no readable liveness anchor, or a recorder without the contract — is omitted, meaning unbounded on that side.
  Recovery never uses its own wall clock, which may have changed while the recorder was down.
  The boundary is reported by readers whose time range its gap meets ([SEM §7.4]).
- **Recorder without a durable spool** (G5-150): a standalone recorder, one that is not a consumer of a durable bus (below), MAY write segments without the spool, the liveness anchor and recovery above.
  It keeps every other Flush rule, so every segment still has one scope, and it writes no `flush_interval_ns`, which would state a contract it does not keep.
  A crash loses the records not yet in a finalized segment, and no recovery appends a `stop-unclean` boundary:
  the capture has no end boundary, its lost records are seq gaps, and its successor's `start` and `previous_capture_id` remain the evidence of the restart,
  as for a producer over a durable bus that stops without a `stop` boundary (below).
  It writes a `stop` boundary on every orderly shutdown, so the case is limited to crashes.
- **Recorder over a durable bus** (G5-86): a deployment in which producers publish records to a message bus that persists each record before acknowledging the publish
  (the pilot uses NATS JetStream), and the recorder is a set of stateless consumers of that bus.
  - *Producer duties.* The producer keeps a stable `recorder_instance_id` for its deployment across restarts,
    assigns a new `capture_id` per process start and names the immediately preceding capture in `previous_capture_id` ([FMT I-7]),
    assigns the capture-scoped `seq` ([FMT I-12]), and stamps `epoch`, `ts_utc_ns` and `mono_ns` at the observation point.
    With the capture's `start` boundary it publishes a **capture descriptor**:
    the capture descriptor defined below, which the consumer copies into the pack metadata of every segment of the capture.
    It keeps the clock anchor of [SEM §4] and detects clock steps itself:
    it publishes the `clock-step` record and waits for the bus's acknowledgement before publishing the first record under the new anchor,
    so every record the bus holds was accepted after an anchor the bus also holds.
    It publishes every record of one capture on one bus subject that names the capture,
    and at most one producer process runs per `recorder_instance_id` at a time;
    a process that keeps publishing after its successor started is outside the supported configuration.
    A consumer stores all of these as received and never reassigns, synthesizes or moves any of them.
  - *Durability.* The bus's acknowledgement is a record's durability point;
    the producer decides how long it buffers an unacknowledged record and what it does when the bus is unreachable,
    and a record it drops is a seq gap: a lookup reports it inside its window ([SEM §7.2]); detecting gaps over a whole capture is the service's ([STO §5]).
    F = `flush_interval_ns` is the consumer's maximum normal segment-flush interval, not the time from observation to acknowledgement;
    the consumer writes it into every segment ([FMT §5]).
    A consumer holds unflushed records in memory and acknowledges a record to the bus only after the segment holding it is durable in `staging/`;
    a consumer that stops before that acknowledges nothing, and the bus redelivers its records to another consumer.
    The liveness anchor and spool recovery do not apply.
    A listing of `staging/` sees a record only once a segment holds it.
  - *Scope and order.* A consumer assigns a record to the scope of its `ts_utc_ns` and writes it in a segment whose period lies in that UTC hour,
    whenever the record is delivered or redelivered; the flush timer decides when a segment closes, never which scope a record belongs to.
    Any consumer may take any record, so the segments of one scope interleave with gaps that other segments fill (allowed by [FMT I-12]).
    A consumer may hold several open segments for one scope.
    Within an open segment blocks are closed in ascending, non-overlapping seq order;
    a record is placed in the first open segment of its scope where it does not precede a closed block,
    and the consumer opens another segment when there is none,
    so every segment satisfies the file-level order of [FMT I-12] and [FMT §10] without reopening a block.
    At flush the consumer finalizes every open segment of the scope.
    A record may be written twice, into segments of different consumers; [FMT I-12] deduplicates it, byte-identical.
    The merge below normalizes the interleaved segments of a scope, decoding the overlapping blocks as it does for any overlap.
  - *Capture descriptor and pack metadata.* The descriptor carries every pack-metadata value the producer owns:
    the always-required `tool_id`, `transport`, `capture_method`, `vantage`, `recorder`, `time_source`, `lifecycle_coverage`, `quality_evaluated` and `recorder_instance_id`;
    `capture_origin_utc_ns`, `capture_origin_mono_ns` and `clock_step_tolerance_ns`, required when `time_source = capture-clock` ([FMT §5]);
    and `previous_capture_id` and the optional site and equipment tags when it has them;
    a log producer also supplies `source_dialect` and `source_ref`, required when `capture_method = log`, and `source_tz`, required when `time_source = source-log` ([FMT §5]);
    the consumer owns `writer`, `classifier`, `max_frame_len`, `period_start`, `period_end`, `seq_start`, `pack_role`, `compaction_level`, `scope_generation` and `flush_interval_ns`.
    The traffic stream is configured so that an accepted message is removed only by acknowledgement, never by a size or age limit,
    so an unacknowledged `start` stays available until its descriptor is registered and a deferred record until its descriptor exists.
    The consumer that receives a capture's `start` record records the descriptor with the service (e.g. in its catalog) before acknowledging it;
    a consumer holding a record of a capture whose descriptor the service does not have yet defers the record
    (a negative acknowledgement with a delay, so the bus redelivers it later) instead of writing a segment without the required metadata.
  - *A producer that stops without a `stop` boundary.* Its capture has no end boundary until proposal P7 defines how the consumer side closes it;
    the successor's `start` and `previous_capture_id` remain the evidence of the restart,
    and every record the bus accepted is staged as long as the stream retains it for redelivery (above).
    A producer publishes a `stop` boundary on every orderly shutdown, so the case is limited to crashes.
- **Merge** of scope S: inputs are the packs of S that the caller gives (normally every segment and archive of S it knows).
  An input whose `pack_role` is not `segment` or `archive` fails the merge, which publishes nothing (§2).
  **Input verification** (G5-109): the merger reads every block of every input in full, as verification does ([FMT §13]),
  and fails, publishing nothing, when an input is not `finalized-consistent`:
  no valid footer, a failed block, or a block that disagrees with its footer (F-2 or F-3).
  A damaged input fails the merge until an operator acts, e.g. by leaving it out after re-converting its source.
  The merge also fails, publishing nothing, when an input breaches its scope or its own pack metadata:
  its period is empty or not inside the UTC hour of S;
  a block's records lie outside that hour (records of the hour outside a segment's period are allowed, since a same-hour clock step puts them there, [SEM §4]);
  a pack with records has a `seq_start` other than its first record's seq ([FMT §5]);
  or its records breach its own commitments:
  a classified record without a `classifier` in that input's metadata, an `oversized` record without a `max_frame_len` there, or a `quality.redacted` record.
  Every input carries the same value of each **capture-level tag**:
  `tool_id`, `transport`, `capture_method`, `vantage`, `recorder`, `time_source`, `lifecycle_coverage`, `quality_evaluated`, `recorder_instance_id`,
  `capture_origin_utc_ns`, `capture_origin_mono_ns`, `clock_step_tolerance_ns`, `previous_capture_id`,
  `site_id`, `equipment_model`, `equipment_sw_rev`, `host_software`, `host_endpoint`, `equipment_endpoint`, `equipment_connect_mode`, `device_id`, `hsms_timers`,
  `source_tz`, `source_dialect` and `source_ref`:
  absent in all inputs, or equal values in all, compared as decoded values;
  for `hsms_timers`, the same timers T1–T8, each absent in all or equal, while nested entries a reader does not know are neither compared nor carried;
  for `source_ref`, the same ordered list, duplicates included,
  because an annotation's `source_index` is a position in that list ([FMT §8]) and a merge never rewrites record bytes.
  Otherwise the merge fails.
  **Overlaps.** Taking every input block by its F-2 seq range [`first_seq`, `last_seq`],
  a **cluster** is a maximal set of blocks connected by overlaps, each overlapping another member directly or through a chain of overlapping ranges;
  two ranges overlap when they share at least one seq, so containment and a shared endpoint are overlaps,
  while adjacent ranges that share no seq, such as [1, 5] and [6, 10], do not.
  The merger writes the clusters in ascending first seq; they are disjoint seq intervals, so the archive keeps the order of [FMT I-12].
  A cluster of one block is copied verbatim: its on-disk bytes are not re-encoded, and its F-3 list is copied ([FMT §10]).
  In a larger cluster, a block is dropped only
  when its complete block envelope (40 bytes) and its on-disk body are **byte-for-byte equal** to those of a block already kept
  (`body_crc` only selects candidates), and only after it has itself been read in full and checked against its own footer;
  if one block remains, it is copied verbatim;
  otherwise the cluster's records are decoded and resolved record by record: identical records are kept once, differing records are a `conflict` ([FMT I-12]).
  **Conflicts** (G5-110): a conflict fails the merge, which publishes nothing and reports every conflict it finds;
  both versions stay in their packs, where readers report the `conflict`, until an operator resolves it.
  In record-level resolution, output records keep their header bytes unchanged, unknown bytes included,
  and the output starts a new block whenever the next record's `record_header_len` differs from the current block's.
  Two records with the same identity and different header lengths are different bytes, hence a `conflict`, never normalised.
  A new block holds records while its decoded size (Σ `record_header_len` + `payload_len`) stays within the merger's block size threshold;
  a record whose decoded size alone reaches the threshold is written whole, in a block of its own ([FMT §2]).
  **Coalescing** is the one exception to copying non-overlapping blocks verbatim.
  A merger SHOULD coalesce small blocks, because small blocks multiply the per-record cost and the archive is what retention keeps.
  Taking the blocks it outputs in ascending seq order, it groups them greedily:
  a group grows while the next block has the same `record_header_len`
  and the group's decoded bodies, that block's included, stay within the merger's block size threshold (4 MiB decoded by default);
  a block whose decoded body alone reaches the threshold, such as a block holding one oversized record, is a group of its own.
  A group of one block is copied verbatim;
  every larger group is encoded as one block, its records byte-identical and in their order.
  All blocks of a scope lie in one UTC hour ([FMT I-13]), so no group crosses an hour.
  The merger validates every new encoding in memory before writing it ([FMT I-2], and byte identity of every record with its source).
  A failing coalesced encoding is discarded and its group's blocks are copied verbatim instead;
  if record-level resolution cannot produce a correct encoding, the merge fails and publishes nothing.
  The output is one archive of S (§2), a replacement set of one member (§6).
  Its file header carries the capture of S and a new `pack_id`, without the `redaction-present` flag.
  Its pack metadata:
  `pack_role = archive`; the `scope_generation` and `publisher_epoch` the caller supplies (§2);
  a new `replacement_set_id` for every merge, a retry included;
  the UTC hour of S as the period, a bound that does not fit in an i64 clamped to the i64 range;
  `compaction_level` = 1 + the highest input level (an input at level 255 fails the merge);
  the capture-level tags of the inputs;
  the union of the inputs' `coverage`, each entry carried as data ([FMT §5]) and only once:
  an entry is not carried again when an entry already carried holds the same nested entries,
  known nested tags compared by value and unknown nested entries in their stored order;
  a **cumulative** `compacted_from` (the `compacted_from` lists of the inputs plus every input segment),
  absent when the list is empty (G5-111, [FMT §5]), lineage only (§4 Deletion);
  `supersedes` = the `pack_id` of every `archive` input, absent when there is none,
  lineage only: it never removes a pack from any read;
  every distinct `classifier` and `max_frame_len` value of the inputs (G5-108);
  the merger's own `writer`;
  and `seq_start` = the first record's seq, or for an archive without records the largest `seq_start` of the inputs (G5-112).
  It carries no `flush_interval_ns` (an archive is not written by a recorder), `notes`, `patch_base`, `redaction_policy`, `redaction` or `extract_filter`,
  and no unknown top-level entry of an input: such an entry belongs to the pack that held it, and the archive is a new pack.
  Its footer follows [FMT §10].
  A merge writes one archive per scope, whatever its size: the limits of [FMT §2] apply per block, not per pack.
  It never alters record bytes.
- **Readiness** (informative): when to merge a scope is the service's choice,
  and a segment of a merged scope that arrives later (clock steps, delayed uploads) is merged again with the archive.
- **Deletion** (G5-201): a pack may be deleted only after a merge that took it as an input has succeeded,
  having verified every input and published its archive (Merge, above),
  and while that archive is retained or has itself been deleted under this same rule.
  The relation "was an input of a successful merge whose output is retained" is acyclic, because a merge's output is created after its inputs:
  two archives merged from the same segment never authorize deleting each other, since neither was an input of the other's merge.
  Which merges succeeded, with which inputs, is recorded by the service (e.g. in its catalog); this profile defines the condition, not the bookkeeping.
  `compacted_from` and `supersedes` are lineage for diagnostics and audit, never the evidence for a deletion:
  they cannot show that an archive holds every record of a pack they list.
  A merge copies every record of every input or fails, so deleting its inputs never removes a record that a read of the remaining packs would yield.
  An input's `coverage` travels into the archive, so a declared loss is never forgotten.
  A pack whose role is not `segment` or `archive` is never deletable under this rule (§2).
  The service deletes a deletable pack after a configured delay, so that reads in flight complete (informative).

## 5. Catalog contract

Removed in v2.32 (G5-201): the catalog, its window, listing views, coherent observation, per-capture evidence, completeness over a capture and retention are the service's design.
The rule by which a `stop-unclean` gap meets a time range is now in [SEM §7.4] (G5-209); the deletion condition is §4 Deletion.

*Informative.*
A service catalog typically records per pack its key, ids, scope, role, period, footer location and F-5 statistics,
and per merge its inputs and output (§4 Deletion).
It can be rebuilt by listing the key prefixes (§3) and reading footers, the merge records excepted, which the lineage tags (`compacted_from`, `supersedes`) help reconstruct.
Retention removes whole hours: the packs of an hour under `archive/` and its segments in `staging/` alike.

## 6. Archive identity

A **replacement set** is one archive, published by a merge (§4) or by a converter directly (§7), and identified by its `replacement_set_id`, identity only (§2).
Every set has exactly one member: an archive's `replacement_set_size` is 1 and its `replacement_set_index` 0 ([FMT §5]).
Every `archive` carries a `replacement_set_id`.

## 7. Log converter contract

Applies to every writer with `capture_method = log`.
Assumed input: per entry a direction, a timestamp, System Bytes, DeviceID / SessionID and the W-bit, plus the message in a text form (e.g. SML);
transport events as lines.
A source dialect lacking some of these is still convertible, with the effects stated below.
1. **Entry boundary**: a per-dialect tokenizer yields entries (metadata line + possibly multi-line body),
   each with its source byte range ([FMT §8]).
2. **Exactly one message per entry**: each entry is parsed on its own;
   a parse error, or a result that is not exactly one message, → `parse-failed` with the entry preserved as an `unparsed-entry` annotation.
3. **Resynchronisation**: after a failure skip to the next entry boundary; skipped bytes become one `skipped-bytes` annotation.
4. **Identity**: DeviceID / SessionID, System Bytes and W applied per [SEM §2];
   missing metadata → the corresponding `field_validity` bits clear + `correlation-incomplete`;
   absent direction → `dir = unknown` unless direction inference is enabled, which sets `direction-inferred`.
   A W-bit already invalid in the source text fails at parse time and is preserved as `parse-failed`.
5. **Time**: parse in `source_tz`, DST rule per [SEM §4]; unparseable → previous entry's time + `ordering-uncertain`.
6. **Transport lines**: [SEM §5]; anything else → `unrecognised-line` annotation with the raw line.
7. **Nothing dropped**: every input byte range is covered by exactly one record or annotation of exactly one capture the run writes.
   An input that holds the traffic of several tools yields one capture per tool, each holding only its own tool's entries and lines,
   and no capture repeats another's bytes as annotations.
   Entries and lines that belong to no tool, such as the logging program's own lines, form one more capture when there are any,
   whose `tool_id` is the logging program's own identity as configured in the converter, distinct from every tool's `tool_id`.
   The converter reports counts per `decode_status` for each capture.
8. **Epoch**: increments on each explicit connect / accept line;
   a dialect without them yields `epoch = 0` + `correlation-incomplete` ([FMT I-7]).
   `capture_id` is one per `tool_id` per source file set per logical conversion ([FMT I-7]); `seq` is capture-scoped ([FMT I-12]), counting across all the capture's hours,
   and the capture-boundary records that start and stop a capture appear once per capture, never once per pack or hour.
   A converter MAY reuse a `capture_id` for a reproducible re-run only when the source contents, their order, the interpretation inputs ([FMT §5] `tool_id` and its attribution, dialect, time zone and every other setting),
   the converter's, parser's and classifier's versions, the capture-level metadata, the seq assignment and the record bytes are all unchanged (Reproducible output, below);
   a change to any of them needs a new `capture_id`.
   A converter that reuses its `capture_id`s derives each deterministically from at least a digest of each source file's contents, in the source order,
   the `tool_id` and its attribution, the dialect, the time zone, every other interpretation setting, and the converter's, parser's and classifier's versions;
   a source file's name alone never stands for its contents.
   The source file requirements below apply to every run.
   Every pack of every capture a run writes lists the run's source files as the same `source_ref` entries in the same order ([FMT §5]),
   so a `source_index` ([FMT §8]) names the same file in each and the coverage of item 7 can be checked across the captures.
9. **Output**: a converter MAY write generation-0 `segment` packs, each holding records of one capture and one UTC hour, possibly covering the whole hour,
   with `compaction_level = 0` and without `compacted_from`, `publisher_epoch` or replacement-set tags ([FMT §5]).
   Such segments come from finite input, so the recorder's flush, spool and recovery contract (§4) does not apply to them, and they write no `flush_interval_ns`;
   they use the segment keys of §3.
   A converter MAY instead write `archive` packs directly, one per (capture, UTC hour), with hour-aligned blocks ([FMT I-13]), `compaction_level = 0` and no `compacted_from`,
   since they represent no generation-0 pack ([FMT §5]); a merge whose inputs are only such archives writes none either (§4, G5-111).
   A direct archive carries the `scope_generation`, `publisher_epoch` and `replacement_set_id` of §2, its `scope_generation` and `publisher_epoch` supplied by the service.

**Reproducible output.**
A converter MAY reuse a `pack_id` only for byte-identical finalized output.
A converter that reuses its `pack_id`s derives each deterministically from at least every input that decides the output's bytes:
the capture identity (item 8), the hour and the division of the capture into packs, every metadata value,
the writer's, tracepack's and codec's implementations, versions and options, the block partitioning, and `writer_start_utc_ns` ([FMT §4]).
When an existing pack's id names different bytes, the converter MUST fail without overwriting it.
A conversion whose inputs change writes a new capture and is a new import; nothing reconciles two captures of the same traffic.

## 8. Implementation tests

The following cases are tests of the Go module, not vectors of the corpus of [FMT §16] (G5-199):
- converter failures (§7);
- a log holding the traffic of two tools and lines of neither, converted into three captures whose packs list the same `source_ref` entries and whose records and annotations together cover every input byte exactly once (§7);
- a merge of several segments into an archive, including overlapping inputs, an hour change forcing two segments, and a late segment merged again with the archive (§4);
- two records with the same (`capture_id`, `seq`) but different bytes: readers report a `conflict`, and a merge fails and publishes nothing ([FMT I-12], §4);
- overlapping blocks with the same seq range but different codec or compressed bytes, and a forced `body_crc` collision (§4);
- a `stop-unclean` recovery followed by a linked capture, recovery with zero recovered records, and a clock step before the crash (§4);
- cumulative sub-threshold backward clock steps before a crash, and a clock rollback while the recorder is down ([SEM §4], §4);
- a durable clock-step, a size-triggered spool roll, then a crash with an empty new spool: boundary seq and gap bounds (§4);
- a record from the next UTC hour forcing a segment roll (§4);
- durable-bus recorder (§4): a redelivered record below a closed block landing in a second segment of the scope,
  and a second late record below both open segments;
  a byte-identical redelivery written into two segments with different `pack_id`s;
  a record delivered in a later hour than its `ts_utc_ns`, placed in its own hour's scope;
  a record whose capture descriptor is not yet recorded, deferred and redelivered after the `start`;
  a successor whose `start` is staged while the earlier capture has no end boundary;
- a backward clock step across a flush-period boundary within one hour, read before and after merging (§4);
- pack metadata with `replacement_set_size` ≠ 1 or `replacement_set_index` ≠ 0: rejected ([FMT §5], §6);
- a merge given a `repair` input, or one whose `pack_role` is 5 or another unknown value, beside a segment: the merge fails and publishes nothing;
  a reader given the same packs reads every one of them (§2, §4);
- merge framing: two overlapping blocks with equal body bytes but different envelopes (different `record_header_len` or `record_count`),
  resolved record by record, never dropped as duplicates;
  adjacent blocks of unequal `record_header_len` left uncoalesced; two records with the same identity and different header lengths: a `conflict`, so the merge fails (§4);
- coalescing: three small blocks of which only the first two fit the threshold together, a block holding one oversized record,
  and adjacent blocks of unequal `record_header_len`: grouped as §4 says (§4);
- a coalesced block whose new encoding is validated in memory before it is written (§4);
- an I-2 failure injected into a newly encoded (coalesced) merge block: the encoding is discarded and the original blocks are copied verbatim (§4);
- an I-2 failure injected into a newly encoded record-level-resolution block that cannot be rebuilt: nothing is published and no input becomes deletable (§4);
- defective merge inputs: an input with a failed block, one without a valid footer, and one with a block that disagrees with its F-3 summary:
  the merge fails and publishes nothing (§4);
- inputs that breach their scope or their own metadata: a block of another hour, correctly indexed;
  a classified record in an input without a `classifier` of its own, beside an input that has one;
  a pack with records whose `seq_start` differs from its first record's seq, alone and beside a valid input:
  the merge fails and publishes nothing (§4);
- three versions of one (`capture_id`, `seq`) in three inputs: the merge fails, publishes nothing and reports each version with the inputs that hold it,
  and a reader given the inputs reports the `conflict` (§4);
- inputs classified under two `classifier` values and two `max_frame_len` values: the archive carries every distinct value ([FMT §5], §4);
- interleaved segments of several durable-bus consumers, gaps filled by other segments and one record written twice: one archive holding every record once (§4);
- inputs whose `source_ref` lists hold the same files in another order, or one file twice: the merge fails (§4);
- a converter's archive merged twice: archives at levels 1 and 2, neither with `compacted_from` ([FMT §5], §4, §7);
- inputs without records, one of them carrying `coverage`:
  an archive without blocks, carrying the `coverage`, whose `seq_start` is the largest `seq_start` of the inputs ([FMT §5], §4);
- deletion (§4 Deletion): two archives merged from one segment do not authorize deleting each other;
  a converter archive merged with a segment, then a merge of the segment alone:
  the second merge makes neither the first archive nor the converter archive deletable.
