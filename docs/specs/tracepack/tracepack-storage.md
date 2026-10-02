# tracepack — storage profile

Status: current (2026-10-02) — v2.20, tracepack format 1.0.
External review consensus reached (review rounds recorded in `tracepack-spec-changelog.md`).
Normative for publishers, mergers, recorders and log converters.

References: `[FMT §n]` = `tracepack-format.md`, `[SEM §n]` = `tracepack-semantics.md`, `[STO §n]` = `tracepack-storage.md`, `[OVW §n]` = `tracepack-overview.md`; `[FMT I-n]` = invariant I-n of the format document.
Each rule is defined in exactly one document; the others only reference it.

## 1. Scope and dependencies

This profile applies to object storage (S3-compatible, e.g. MinIO).
The format does not depend on it: reading and writing packs on a local file system needs only [FMT] and [SEM],
while repair patches, recorder durability and crash recovery follow this document wherever packs are stored.

Depends on:
- [FMT I-6] immutability, [FMT I-7] capture and epoch rules, [FMT I-12] record identity and dedup, [FMT I-13] hour-aligned blocks, [FMT I-14] mergeable footer.
- [FMT §8] transport-event and annotation payload tags (boundary records, converter annotations); [FMT §16] corpus contract for the vectors of §8.
- [FMT §5] pack metadata tags (`pack_role`, `scope_generation`, `publisher_epoch`, `patch_base`, replacement-set tags, `compacted_from`, `supersedes`, `coverage`, `seq_start`, `flush_interval_ns`, `clock_step_tolerance_ns`).
- [FMT §10] footer aggregation rule and validation; [FMT §13] recovery walk, and the bootstrap that listing views perform (§5).
- [FMT §6] block envelope and block body; [FMT I-2] block integrity; [FMT §12] block validation.
- [SEM §4] clock anchor and clock-step rule; [SEM §5] capture-boundary events; [SEM §6] effective quality; [SEM §7.2] transaction lookup.
- For the log converter (§7): [SEM §2] reconstruction contract and capture model, [SEM §3] converter `decode_status` values, [SEM §4] source-log time rules,
  [FMT §7.2] record fields and `field_validity`, [FMT §9] enums.
- [FMT I-11] derived values, which a repair may re-emit with new classification (§6).
- [SEM §8] redaction, for extracts.

## 2. Scopes, roles, generations

A **scope** is one (`capture_id`, UTC hour).
Every pack except an `extract` holds records of exactly one scope, and its period lies inside that hour;
a pack's scope comes from its period, never from its record timestamps, so a pack without records (e.g. a repair holding only `coverage`) still has a scope.

| `pack_role` | Written by | Content | `scope_generation` |
|---|---|---|---|
| `segment` | recorder, per flush | records of one flush interval of one scope, all of them for a local recorder, a subset for a consumer of a durable bus (§4), which may write several segments per interval; `compaction_level = 0` | 0 |
| `archive` | merger (or a converter directly) | all records of one scope | ≥ 1 |
| `repair` | `verify --repair` | a **patch**: the recoverable records of the damaged packs it names in `supersedes`, with `coverage` for the rest | 0 |
| `extract` | query service, on request | a filtered subset of one capture (`extract_filter`), masked under a redaction policy for consumers that are not privileged ([SEM §8]); never a complete period, never stored in the tiers, never a merge input, a patch or the subject of a repair | — |

A **generation** is a replacement set (§6), the one archive of a scope with one `scope_generation` ≥ 1; only merges produce generations.
Generations are ranked by (`publisher_epoch`, `scope_generation`), compared lexicographically.
A **publisher epoch** fences publishing work across restarts and catalog rebuilds:
before it claims anything, a publisher writes a fence object (§3) numbered one higher than every fence object in the bucket
and every `publisher_epoch` on a pack the catalog indexes, committed or not (§5),
and stamps that number on every generation it writes.
Work started under an older epoch can therefore never outrank work accepted under a newer one, even if its upload completes later.
Within an epoch, a merge claims the number one higher than the highest generation claimed so far for the scope (the catalog allocates it, §5);
because an interrupted claim may leave a gap, the active generation is the highest-ranked *complete* one, which need not be the highest claimed.
Repairs are generation-0 **patches** registered against the current generation (`patch_base`); for an indexed scope (§5), the next merge folds them into a generation.

**Roles in the tiers.**
Only a pack whose `pack_role` is `segment`, `archive` or `repair` takes part in a scope's view, is a merge input, is registered by the catalog (§5),
or becomes deletable under §4 Deletion.
A pack with any other role — an unknown value, including the retired value 5 ([FMT §9]); an `extract` never enters the tiers —
is excluded from all four, removed only by §5 Retention with its hour, and reported to the operator.

## 3. Keys

- Segment: `<prefix>/staging/<tool_id>/<capture_id>/<seq_first>-<pack_id>.tpk`.
- Fence object: `<prefix>/fence/<publisher_epoch>-<uuid>` (epoch in 20-digit zero-padded decimal); its content is irrelevant.
  Publishing is single-publisher per tool (§5): a new publisher acquires its epoch after the previous one has stopped or its lease (below) has expired,
  so fences need no conditional write; concurrent publishers sharing an epoch are outside the supported configuration.
  A service that spreads publishing over several instances keeps that rule with **tool leases** recorded in the catalog (G5-87):
  an instance publishes only for tools it holds a lease on.
  Acquiring a lease atomically allocates a publisher epoch above every retained fence object and every indexed `publisher_epoch`,
  and the holder writes the fence object durably before it claims any work,
  so a tool's generations rank in publication order even when the tool moves between instances and back.
  Every admission and publication carries the lease token and epoch;
  catalog validation and installation (§5 steps 2 and 4) reject a stale token or epoch.
  A former holder's commit-object write (§5 step 3), already attempted when its lease expired, may still land:
  before the new holder admits any work for the tool it recovers every interrupted admission of the tool's scopes (§5),
  rolling a landed commit forward and cancelling an uncertain one under its higher epoch, which is what protects the accepted view from such a late write.
- Commit object: `<prefix>/commit/<tool_id>/<capture_id>/<YYYYMMDDHH>/<id>`, where `<id>` is the generation's `replacement_set_id` or the patch's `pack_id`;
  its content is irrelevant. It is the durable evidence that the catalog accepted the generation or patch (§5).
- Other packs: `<prefix>/archive/<tool_id>/<YYYY>/<MM>/<DD>/<HH>/<capture_id>-<pack_id>.tpk`, hour in UTC.
- `tool_id` is percent-encoded; `seq_first` is the pack's first seq (its `seq_start` when empty) in 20-digit zero-padded decimal, so segments list in seq order.
- Every key contains the `pack_id`, so every upload has a unique key and no object is ever overwritten.
  Retried and duplicate uploads are harmless: the catalog registers each `pack_id` of an indexed scope once (§5),
  and records are deduplicated by (`capture_id`, `seq`) in every view ([FMT I-12]).
- Packs and commit objects are removed only by §4 Deletion and §5 Retention.
  Storage lifecycle rules (e.g. S3 or MinIO ILM) MAY transition objects under `archive/` to another storage class, provided they stay readable and listable;
  they MUST NOT expire any object, and MUST NOT apply to `staging/`, `commit/` or `fence/`.
- The highest-numbered fence object is never deleted; lower-numbered fence objects MAY be deleted by the component that performs §4 Deletion.
- Objects under `staging/`, `archive/`, `commit/` and `fence/`, and the catalog, hold records in full;
  only privileged components read them (recorders, the merger, the deleting component, the query service, privileged consumers).
  How access is controlled is service design; consumers that are not privileged receive masked extracts ([SEM §8]).

## 4. Active view, flush, merge

**Active view of a scope** — the records a reader uses for it, computable from the surviving packs and commit objects alone,
among the packs whose role takes part in it (§2):
1. G = the highest-ranked (§2) generation of the scope with a complete replacement set (§6) **and** a commit object (§3); none if there is no such generation.
   Two different complete sets with the same highest rank make the scope `conflicted`, never a silent choice.
2. The view is the member of generation G,
   plus every generation-0 pack of the scope that is not listed in generation G's `compacted_from`,
   and, for a patch, that has a commit object and whose `patch_base` is G's `replacement_set_id` (absent when there is no G),
   minus every pack named in the `supersedes` of a **patch based on G**,
   one that has a commit object and a `patch_base` matching G as above,
   whether or not a later patch names that patch in turn.
   A patch may name segments, the member of generation G and patches in the view;
   it replaces exactly the packs it names, so the other packs stay in the view.
   Supersession is therefore transitive: when a later patch replaces a patch, the packs the earlier patch replaced stay replaced.
   Patches are registered one at a time and name only packs in the view (§5), so no chain of `supersedes` is circular.
   A patch whose base is not G (stale, or rejected at registration) is outside the view.
3. Records are deduplicated by (`capture_id`, `seq`) ([FMT I-12]); the `coverage` of every pack in the view applies.

Lower generations and consumed generation-0 packs are outside the view whether or not they still exist,
so deleting deletable packs in any order never changes the view and never needs lineage chains to be intact,
provided each commit object is deleted after the packs it commits.

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
  The boundary is a **completeness barrier** (§5).
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
    and a record it drops is a seq gap that §5 Completeness reports.
    F = `flush_interval_ns` is the consumer's maximum normal segment-flush interval, not the time from observation to acknowledgement;
    the consumer writes it into every segment ([FMT §5]).
    A consumer holds unflushed records in memory and acknowledges a record to the bus only after the segment holding it is durable in `staging/`;
    a consumer that stops before that acknowledges nothing, and the bus redelivers its records to another consumer.
    The liveness anchor and spool recovery do not apply.
    A catalog rebuild (§5) sees a record only once a segment holds it.
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
  - *Capture descriptor and pack metadata.* The descriptor carries every always-required pack-metadata value the producer owns
    (`tool_id`, `transport`, `capture_method`, `vantage`, `recorder`, `time_source`, `lifecycle_coverage`, `quality_evaluated`,
    `recorder_instance_id`, `capture_origin_utc_ns`, `capture_origin_mono_ns`, `clock_step_tolerance_ns`, and `previous_capture_id` and the optional site and equipment tags when it has them);
    a log producer also supplies `source_tz`, `source_dialect` and `source_ref`;
    the consumer owns `writer`, `classifier`, `max_frame_len`, `period_start`, `period_end`, `seq_start`, `pack_role`, `compaction_level`, `scope_generation` and `flush_interval_ns`.
    The traffic stream is configured so that an accepted message is removed only by acknowledgement, never by a size or age limit,
    so an unacknowledged `start` stays available until its descriptor is registered and a deferred record until its descriptor exists.
    The consumer that receives a capture's `start` record registers the descriptor in the catalog before acknowledging it;
    a consumer holding a record of a capture whose descriptor the catalog does not have yet defers the record
    (a negative acknowledgement with a delay, so the bus redelivers it later) instead of writing a segment without the required metadata.
  - *A producer that stops without a `stop` boundary.* Its capture stays `open` ([STO §5]) until proposal P7 defines how the service closes it;
    the successor's `start` and `previous_capture_id` remain the evidence of the restart,
    and every record the bus accepted is staged as long as the stream retains it for redelivery (above).
    A producer publishes a `stop` boundary on every orderly shutdown, so the case is limited to crashes.
- **Merge** of scope S: inputs are the packs of the active view of S.
  **Input verification** (G5-109): the merger reads every block of every input in full, as verification does ([FMT §13]),
  and fails, publishing nothing, when an input is not `finalized-consistent`:
  no valid footer, a failed block, or a block that disagrees with its footer (F-2 or F-3).
  The damaged input is repaired first ([FMT §13], §6), and the patch joins the view in its place.
  The merge also fails, publishing nothing, when an input breaches its scope or its own pack metadata:
  its period is empty or not inside the UTC hour of S;
  a block's records lie outside that hour (records of the hour outside a segment's period are allowed, since a same-hour clock step puts them there, §5);
  a pack with records has a `seq_start` other than its first record's seq ([FMT §5]);
  or its records breach its own commitments:
  a classified record without a `classifier` in that input's metadata, an `oversized` record without a `max_frame_len` there, or a `quality.redacted` record.
  Repair of a `finalized-consistent` input corrects a `seq_start` other than its first record's seq ([FMT §13], G5-115),
  but writes nothing for any other breach,
  so a scope holding a `finalized-consistent` input with another such breach stays unmerged until an operator acts.
  Every input carries the same value of each **capture-level tag**:
  `tool_id`, `transport`, `capture_method`, `vantage`, `recorder`, `time_source`, `lifecycle_coverage`, `quality_evaluated`, `recorder_instance_id`,
  `capture_origin_utc_ns`, `capture_origin_mono_ns`, `clock_step_tolerance_ns`, `previous_capture_id`,
  `site_id`, `equipment_model`, `equipment_sw_rev`, `host_software`, `host_endpoint`, `equipment_endpoint`, `equipment_connect_mode`, `device_id`, `hsms_timers`,
  `source_tz`, `source_dialect` and `source_ref`:
  absent in all inputs, or equal values in all, compared as decoded values;
  for `hsms_timers`, the same timers T1–T8, each absent in all or equal, while nested entries a reader does not know are neither compared nor carried
  (a patch rewrites its metadata and may have dropped them, §6);
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
  both versions stay in the view, where readers report the `conflict`, until an operator resolves it.
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
  The output is the claimed generation of S (§2), a replacement set of one member (§6).
  Its file header carries the capture of S and a new `pack_id`, without the `redaction-present` flag.
  Its pack metadata:
  `pack_role = archive`; the claim's `scope_generation`, `publisher_epoch` and `replacement_set_id`, a new `replacement_set_id` for every claim, a retry under a new claim included;
  the UTC hour of S as the period, a bound that does not fit in an i64 clamped to the i64 range;
  `compaction_level` = 1 + the highest input level (an input at level 255 fails the merge);
  the capture-level tags of the inputs;
  the union of the inputs' `coverage`, each entry carried as data ([FMT §5]) and only once:
  an entry is not carried again when an entry already carried holds the same nested entries,
  known nested tags compared by value and unknown nested entries in their stored order;
  a **cumulative** `compacted_from` (generation G's list plus every generation-0 pack in the view
  and every generation-0 pack that a patch based on G names, whether or not a later patch replaced that patch),
  absent when the list is empty (G5-111, [FMT §5]);
  `supersedes` = the member of generation G (lineage only), absent when there is no G;
  every distinct `classifier` and `max_frame_len` value of the inputs (G5-108);
  the merger's own `writer`;
  and `seq_start` = the first record's seq, or for an archive without records the largest `seq_start` of the inputs (G5-112).
  It carries no `flush_interval_ns` (an archive is not written by a recorder), `notes`, `patch_base`, `redaction_policy`, `redaction` or `extract_filter`,
  and no unknown top-level entry of an input: such an entry belongs to the pack that held it, and the archive is a new pack.
  Its footer follows [FMT §10].
  A merge writes one archive per scope, whatever its size: the limits of [FMT §2] apply per block, not per pack.
  It never repairs record bytes.
- **Readiness**: S is merged after its hour has ended plus a grace period,
  once the catalog shows contiguous seq coverage from the capture's start —
  or, when the capture has scopes that are not indexed (§5), from the lowest seq in the views of its indexed scopes whose hour lies in the window —
  through the first record whose `ts_utc_ns` is at or after the end of the hour plus the grace period,
  or the capture has ended (`stop` or `stop-unclean`) per its per-capture entry (§5).
  When the capture has scopes that are not indexed and none of its indexed scopes lies in the window, S is ready only once the capture has so ended.
  Segments of S that arrive later (clock steps, delayed uploads) are generation-0 packs outside `compacted_from`,
  so they join the view and, while S is indexed (§5), trigger another merge;
  a later starting point of the seq coverage therefore never loses records, because readiness governs only when a merge starts.
- **Deletion**: being outside the view (excluded from record reads) is not the same as being deletable.
  The rules below apply to a **complete observation** of the scope:
  every surviving pack of the scope that a reader could list, replaced patches included, and the scope's commit objects
  (a catalog snapshot, or a coherent observation, §5).
  A pack becomes deletable only when the current generation G makes it redundant:
  a generation-0 pack (segment or patch) when G's `compacted_from` lists it;
  a generation's member when its rank is below G's, whether or not its set was committed (an uncommitted member's rank is read from its own pack metadata);
  a patch, committed or not, whose base ranks below G:
  its `patch_base` names a generation ranked below G,
  or names no surviving generation and is not G's `replacement_set_id`
  (a generation's member is deleted only when a higher-ranked generation is G, and G never ranks lower afterwards),
  or is absent while G exists (the patch was registered before the scope had any generation).
  No other pack is deletable; in particular:
  G's own member stays while G is current, also when a patch replaced it, until the next merge folds the patch, because G's completeness depends on it;
  an uncommitted patch whose `patch_base` matches G (absent when there is no G) stays, because it may be an admission in progress;
  an incomplete set has no member to delete, and its commit object is handled as below;
  and a pack whose role takes no part in the tiers is never deletable (§2).
  A commit object is deleted only by the component that deleted every pack it commits, immediately after deleting the last of them:
  for a generation, its member, registered in the catalog; for a patch, the patch.
  A commit object whose packs that component cannot account for this way (e.g. one written late for an admission that was cancelled)
  is never deleted on the evidence of a listing; it is removed with its hour by §5 Retention.
  The deleting component deletes packs and commit objects only on the catalog's decisions for indexed scopes, and by §5 Retention.
  Deletion happens after a configured delay counted from the publication that made the pack deletable, so queries in flight complete.
  With these rules the view computed from the surviving packs and commit objects never changes when deletable packs are removed, in any order,
  and commit objects after them.

## 5. Catalog contract

The catalog is the index a reader or query service uses instead of listing the bucket, for the scopes it indexes; listing views (below) serve the others.
It is a cache: it can be rebuilt from the retained objects — the packs' file headers, pack metadata and footers plus the commit and fence objects (§3) —
by listing the bucket and reading footers, and the rebuilt active views of the scopes it indexes equal the last committed ones (§4).
A rebuild reads the footer of every retained pack, `staging/` included, and MAY index only the window's scopes.
A catalog that is not rebuilt also keeps per-capture evidence whose source packs Retention (below) has removed; a rebuild cannot recover that evidence.
Its storage technology is not part of this specification.
- **Window**: the catalog MAY index only a window of scopes (e.g. the most recent 35 days of hours).
  A scope is **indexed** when the catalog holds its per-scope and per-pack entries; admissions are accepted only for indexed scopes, whatever their age.
  The catalog removes a scope from the index (**eviction**) only when the scope is **settled**:
  it has a committed complete generation G, no generation-0 pack of the scope is outside G's `compacted_from`,
  no merge claim is open, no interrupted admission awaits recovery, and every pack and commit object of the scope that is deletable under §4 has been deleted.
  Eviction is one catalog transaction, serialized with every admission of the scope — segment and patch registration, merge claims and publication, interrupted-admission recovery —
  and it re-checks the settled condition inside that transaction.
  A catalog read of a scope returns whether it is indexed together with its pack entries, per-scope state, seq coverage and the per-capture entries of its captures in one consistent snapshot;
  a reader that cannot obtain one — the scope was evicted between its reads — uses the scope's listing view instead.
  The observation a query evaluates also includes, in the same snapshot, every per-capture barrier whose interval meets the queried tool and time range,
  including barriers of captures none of whose scopes the query selects or the catalog indexes.
- **Coherent observation** of a scope that is not indexed, taken by a reader without the catalog.
  It rests on two premises that the rules of this section establish for every scope that is not indexed:
  (i) the catalog validates no admission for the scope, so a commit object can appear there only as the late write of an admission validated while the scope was indexed;
  (ii) no pack or commit object of the scope is deleted except by Retention, which makes the hour removed before it deletes anything —
  eviction requires the scope's deletion work to be finished, the deleting component acts only for indexed scopes (§4 Deletion),
  and a rebuild starts only after the deleting work of the lost catalog has stopped (Restart and rebuild).
  Under these premises the scope's commit objects only accumulate while it is observed, and none of its packs is removed.
  Steps:
  (1) list the commit objects under `commit/<tool_id>/<capture_id>/<YYYYMMDDHH>/`, all pages, and repeat until two consecutive complete listings return the same keys;
  (2) then list the scope's packs under `archive/<tool_id>/<YYYY>/<MM>/<DD>/<HH>/` (keys carrying the `capture_id`)
  and its segments under `staging/<tool_id>/<capture_id>/` (period in the scope's hour);
  (3) bootstrap each listed pack per [FMT §13]: its file header and pack metadata determine the view,
  and its validated footer supplies the coverage, boundaries, epoch closure and pruning statistics that queries use.
  Why the view is coherent: let L1 and L2 be the two equal listings of (1), and t an instant after L1 ends and before L2 starts.
  Every key L1 returned still exists at t, because nothing is deleted; every key that exists at t exists throughout L2, so L2 returns it.
  Hence L1 ⊆ C(t) ⊆ L2, and L1 = L2 gives C(t) = L1, the set of commit objects at t.
  The view at t reads only packs that exist at t — a pack is uploaded before its commit object (commit protocol step 1 before step 3) and is never deleted here —
  namely G's member and the generation-0 packs outside G's `compacted_from`,
  every patch based on G among them, also one that a later patch replaced; so (2) lists every one of them.
  A listed pack whose commit object is not in C(t) is uncommitted in the view, whenever it was uploaded.
  A set whose commit object is in C(t) but whose member is not listed is incomplete and outside the view, as in §4.
  Segments uploaded after t may be listed too, and join the view because segments need no commit object;
  the result is the committed view at t plus some of the segments uploaded after t, not a snapshot of any later instant.
  A listed pack that is gone when read is gone because its hour was removed (reported as removed) or because a component did not conform;
  either way the result is never silently smaller.
  The observation is defined only for scopes that are not indexed: on an indexed scope admissions and deletions run, and it is no snapshot.
- **Listing view**: for a scope that is not indexed, a reader computes the active view of §4 from a coherent observation.
  A reader finds the captures and packs of a tool in a time range by listing `staging/<tool_id>/` and `archive/<tool_id>/<YYYY>/<MM>/<DD>/` for the days concerned,
  and takes end states and barriers from the catalog's per-capture entries.
- **Roles**: the catalog registers only packs whose role takes part in the tiers (§2),
  and it reports any other pack presented to it to the operator.
- **No admissions outside the index**: registering a segment or a patch, and claiming a merge, for a scope that is not indexed are rejected.
  A rejected segment stays in `staging/`, where listing views read it; the recorder treats the rejection as final.
  The catalog still records the per-capture evidence of a rejected segment or converter archive (Per capture, below).
- **Per pack**, for indexed scopes: key, `pack_id`, `capture_id`, `tool_id`, scope, `pack_role`, `scope_generation`, replacement set, `compaction_level`, cumulative `compacted_from`,
  `supersedes`, period, object size, `pack_metadata_len`, footer location (`footer_offset`, `footer_len`, `footer_codec`), F-5 statistics,
  optionally the F-2 entries (for block range reads without a bootstrap), and whether it is in its scope's active view.
- **Per scope**, for indexed scopes: highest claimed and highest complete generation, active view, `conflicted` flag.
- **Per capture**: `tool_id`, `recorder_instance_id`, `previous_capture_id`, seq coverage (union of the seq ranges of the active views of its indexed scopes),
  the capture-boundary entries and epoch closures (F-5 `boundary`, `epoch` `close_seq`, [FMT §10]) of every segment or converter archive presented for registration, indexed scope or not,
  and the end state (`open`, `stopped`, `stopped-unclean`) and barriers that follow from them (Completeness, below).
  Per-capture entries are never removed by eviction or retention.
- **Transactions**: registering a pack (or rejecting it and recording its per-capture evidence), claiming a merge (allocating generation G + 1 of a scope),
  publishing its result (recording the complete set and the new active view), and evicting a scope are each atomic.
- **Completeness**: a query result for a (capture, epoch) or time range is complete only when the catalog shows contiguous seq coverage over it,
  no `coverage` entry of a pack in the view intersects it, and it does not reach past a completeness barrier;
  otherwise it is `incomplete` with the searched scope.
  Time queries select the scopes whose UTC hour meets the range and read every pack of their active views ([SEM §7.4]);
  inside those packs, the actual record times (F-2, F-5) decide what a read excludes, cluster by cluster, never a pack's period, because a same-hour clock step can put records outside a segment's flush interval;
  they consult `coverage` entries ([FMT §5] matching rule) and completeness barriers by their own intervals, including those of packs without records.
  A `stop-unclean` boundary is a barrier for every epoch still open when the capture ended — one for which the per-capture entry records no closure —
  and for the time interval [`gap_start`, `gap_end`], unbounded on a side whose bound is absent;
  a transaction lookup whose primary lies in such an epoch never returns `unmatched`.
  A gap between linked captures is recorder downtime and is always reported when the earlier capture carries an end boundary;
  a capture left `open` (§4 Recorder over a durable bus) shows the restart through its successor's `start` but supplies no downtime barrier and no gap bounds.
  - A result that touches a scope that is not indexed is `incomplete` with reason `cold` and the searched scope;
    a transaction lookup whose eligibility window touches such a scope never returns `unmatched` ([SEM §7.2]).
  - End states, barriers and epoch closures come from the per-capture entries (above), whether or not the scope holding the evidence is indexed,
    and barriers apply by their own intervals whether or not any scope of their capture is indexed.
    A capture without `stop` or `stop-unclean` evidence in its per-capture entry is `open`.
- **Serialization**: per indexed scope, the catalog serializes patch registration, merge claims and merge publication;
  eviction is serialized with every admission of the scope (Window, above).
  A patch is accepted only when its `patch_base` is the current G and every pack it names is in the view.
  A merge records the view it read (G and the generation-0 packs) at claim time;
  publication succeeds only if that view is unchanged, otherwise the merge is retried under a new claim.
- **Commit protocol** (generations and patches): (1) upload the packs; (2) the catalog validates and reserves the admission;
  (3) the publisher writes the commit object — **the commit point**; (4) the catalog installs the result: visible to readers and to patch registration,
  and packs that now satisfy §4 Deletion become deletable (a patch's target generation member does not; it waits for the folding merge).
  The scope's serialization (above) spans (2) through (4): no other admission for the scope is validated in between.
  A rejected admission never gets a commit object, so a rebuild — which admits only committed generations and patches — never activates it.
  The rebuilt view equals the last **committed** view, which may include an admission committed at (3) but not yet installed at (4).
- **Recovery of an interrupted admission**, before any other admission for the scope:
  if the commit object exists, roll forward: perform (4);
  if it was provably never attempted, cancel;
  otherwise — the write was attempted, or the attempt state was lost, and the object is absent (a late write may still land) —
  cancel by committing a new generation of the scope under the new publisher epoch, built by an ordinary merge of the last committed view
  (which excludes the uncertain admission); it outranks an uncertain generation, and an uncertain patch no longer matches its base.
  If the cancellation itself is interrupted, this recovery rule applies to it again, under a still newer epoch; the scope stays closed to other admissions until a recovery completes.
  A cancellation merge that §4 refuses — an input refused by the input checks, capture-level tags that differ, or a record conflict — cannot complete,
  so the scope stays closed until an operator acts (proposal P2 records the open design).
  Only then may patches be registered or merges published for the scope.
  Segments need no commit object: a segment takes part in its scope's view by the rules of §4, and the catalog registers it while the scope is indexed.
- **Restart and rebuild**: a publisher or catalog that loses its claim state starts a new publisher epoch (§2) before claiming again,
  and a rebuilt catalog continues generation numbering from the highest generation found in the new epoch.
  A rebuilding catalog acquires that epoch after it has indexed the window, so that the window's epochs are known;
  if every fence object were lost, the indexed packs would give only a lower bound, and the fence rule of §3 is what preserves the global high-water mark.
  Uploads of older epochs that complete later rank below every generation of the new epoch.
  The service's deleting component stops with the catalog that directed it; a rebuild starts only after it has stopped, so no deletion decided by the lost catalog runs afterwards.
  A rebuild recomputes per-capture entries from the footers of every retained pack, `archive/` and `staging/`, before it serves results.
  A scope outside the rebuilt window is read through listing views; merge and deletion work it still had pending waits for the retention of its hour.
- **Retention**: an hour is **removed** once it is older than the configured retention period, a logical boundary every reader applies.
  A reader checks it before each listing and before emitting each block's records.
  When an hour it is reading becomes removed, the reader stops reading it and ends the hour with the outcome **removed**:
  never an empty, partial or complete result, also when it already emitted records of that hour, which the outcome supersedes.
  The component that performs §4 Deletion then deletes the hour physically: for each scope, its packs (segments in `staging/` included, identified by their period) in any order,
  then its commit objects, then its per-scope and per-pack catalog entries if the scope is indexed.
  Because readers stop at the logical boundary, the order among the packs of a removed hour does not matter,
  and commit objects left without packs between the two steps are never evaluated.
  A segment of a removed hour uploaded later is deleted by the same component when found.
  Per-capture entries are not removed.

## 6. Replacement sets and repairs

A **replacement set** is the archive a merge publishes as one generation, identified by its `replacement_set_id`.
Every set has exactly one member: `replacement_set_size` = 1 and `replacement_set_index` = 0 ([FMT §5]).
A set is complete when its member exists; an incomplete or uncommitted set is ignored (an interrupted or rejected publication), and the view falls back to the highest-ranked committed complete generation.
Every `archive` belongs to a set.

A patch (a `repair`, generation 0) completely replaces the packs it names in `supersedes`, all in its scope:
it contains every record of those packs (possibly with new stored bits or classification, [FMT I-11]),
carries the union of those packs' existing `coverage` entries (coverage is metadata, so a patch that omitted it would silently drop it),
and declares any newly lost (`capture_id`, `seq`) ranges and time intervals as further `coverage` entries,
which the next merge of the scope carries into the generation (union) and every later generation inherits;
a view that still holds the patch applies them directly; every query touching them reports `incomplete`.
Other packs of the view are untouched.
A patch keeps the scope and capture of the pack it repairs:
its `capture_id`, its period and its capture-level tags, and for an archive its `compaction_level` and `compacted_from`,
since the patch's records still represent the generation-0 packs that list names (§4 Merge takes the list cumulatively);
its `scope_generation` is 0, and it carries no `publisher_epoch` or replacement-set tags.
A patch without records takes the damaged pack's `seq_start` ([FMT §5]).
How a patch is built from a damaged pack, and when repair is refused, is [FMT §13].
Because the lost records of a pack belong to its scope, their time intervals lie within its hour, except for completeness barriers (§4), which are handled separately.
Readers default to the active view and can include packs outside it on request.

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
7. **Nothing dropped**: every input byte range is covered by exactly one record or annotation;
   the converter reports counts per `decode_status`.
8. **Epoch**: increments on each explicit connect / accept line;
   a dialect without them yields `epoch = 0` + `correlation-incomplete` ([FMT I-7]).
   `capture_id` is one per converter run per source file set; `seq` is capture-scoped ([FMT I-12]).
9. **Output**: a converter MAY write `archive` packs directly, one per (capture, UTC hour), with hour-aligned blocks ([FMT I-13]), `compaction_level = 0` and no `compacted_from`,
   since they represent no generation-0 pack ([FMT §5]); a merge whose view holds only such an archive writes none either (§4, G5-111).

## 8. Conformance vectors

The following vectors belong to the corpus of [FMT §16]:
- converter failures (§7);
- a merge of several segments into an archive, including overlapping inputs, an hour change forcing two segments, and a late segment producing a new generation (§4);
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
  a record whose capture descriptor is not yet registered, deferred and redelivered after the `start`;
  a successor whose `start` is staged while the earlier capture stays `open`;
  a rebuild from `staging/` while bus records are still unstaged;
  a lease expiring during each step of the commit protocol, with the former holder's late commit handled by takeover recovery (§3, §5);
- a repair followed by a late merge, and a repair holding only `coverage` (§4, §6);
- a generation whose member is repaired, published, predecessors deleted (packs in either order, each commit object after its packs), then rebuild:
  the repaired prefix and all inherited coverage remain (§4);
- a patch replacing one generation member, an attempted deletion before the next merge, and a rebuild; then the folding merge and deletions (packs in either order, each commit object after its packs) (§4);
- a patch of a patch, S1 ← P1 ← P2 for a segment S1 and M ← P1' ← P2' for generation G's member M:
  the view holds P2 and P2' but none of S1, P1, M and P1', also after a rebuild;
  the folding merge lists S1, P1, P2, P1' and P2' in `compacted_from`,
  and deletions (packs in either order, each commit object after its packs) and a rebuild leave the view unchanged (§4, §5);
- generations A → B → C of one scope with B deleted before A (packs in either order, each commit object after its packs), then catalog rebuild: the view is exactly C with its inherited coverage (§4);
- a backward clock step across a flush-period boundary within one hour, queried before and after merging (§5);
- an invisible high-number claim, a rebuild with a new publisher epoch, a lower-number merge absorbing a late segment, deletion of its inputs,
  then completion of the old upload and another rebuild: the newer records and coverage remain (§2, §5);
- a patch registered between a merge's claim and its publication (§5);
- a merge claim, an accepted repair, a complete stale upload, rejected publication, a crash before the retry, rebuild, merge and deletion:
  the repair and its coverage survive (§5 commit protocol);
- a rejected patch whose upload survives, then rebuild (§5);
- a commit object written, a crash before installation, recovery, an attempted repair against the old base, rebuild, merge and deletion;
  and the same with an unknown commit-write outcome, for an uncertain generation and for an uncertain patch, with and without an earlier generation,
  and with a crash during the cancellation itself (§5 recovery);
- pack metadata with `replacement_set_size` ≠ 1 or `replacement_set_index` ≠ 0: rejected ([FMT §5], §6);
- a pack whose `pack_role` is 5 or another unknown value beside a segment and a generation: outside the view, no merge input, not deletable, and reported, before and after a rebuild (§2);
- merge framing: two overlapping blocks with equal body bytes but different envelopes (different `record_header_len` or `record_count`),
  resolved record by record, never dropped as duplicates;
  adjacent blocks of unequal `record_header_len` left uncoalesced; two records with the same identity and different header lengths: a `conflict`, so the merge fails (§4);
- coalescing: three small blocks of which only the first two fit the threshold together, a block holding one oversized record,
  and adjacent blocks of unequal `record_header_len`: grouped as §4 says (§4);
- a coalesced block whose new encoding is validated in memory before it is written (§4);
- an I-2 failure injected into a newly encoded (coalesced) merge block: the encoding is discarded and the original blocks are copied verbatim (§4);
- an I-2 failure injected into a newly encoded record-level-resolution block that cannot be rebuilt: nothing is published and no input becomes deletable (§4);
- defective merge inputs: an input with a failed block, one without a valid footer, and one with a block that disagrees with its F-3 summary:
  the merge fails and publishes nothing; after a repair, the patch is merged in its place (§4);
- inputs that breach their scope or their own metadata: a block of another hour, correctly indexed;
  a classified record in an input without a `classifier` of its own, beside an input that has one;
  a pack with records whose `seq_start` differs from its first record's seq, alone and beside a valid input:
  the merge fails and publishes nothing (§4);
- three versions of one (`capture_id`, `seq`) in three inputs: the merge fails, publishes nothing and reports each version with the inputs that hold it,
  and a reader of the view reports the `conflict` (§4);
- inputs classified under two `classifier` values and two `max_frame_len` values: the archive carries every distinct value ([FMT §5], §4);
- interleaved segments of several durable-bus consumers, gaps filled by other segments and one record written twice: one archive holding every record once (§4);
- inputs whose `source_ref` lists hold the same files in another order, or one file twice: the merge fails (§4);
- a converter's archive merged twice: archives at levels 1 and 2, neither with `compacted_from` ([FMT §5], §4, §7);
- an incomplete set, a commit object whose member is missing: outside the view, and no pack deletable on its account (§4 Deletion);
- a stale patch whose base generation no longer survives: deletable; an uncommitted patch whose `patch_base` matches G: not deletable (§4 Deletion);
- a view holding only segments without records and a patch holding only `coverage`:
  an archive without blocks, carrying the `coverage`, whose `seq_start` is the largest `seq_start` of the inputs ([FMT §5], §4);
- a generation G with a predecessor G−1 whose member is deletable but not yet deleted, and G's commit object removed first: the view falls back to G−1;
  the vector shows why §4 Deletion orders commit objects after their packs, and conforming deletion never produces this state (§4);
- deletion of G−1's member, then G−1's commit object: the view is G throughout (§4);
- coherent observation with pagination: an uncertain patch P against G1 and an uncertain cancellation generation G2, both validated while the scope was indexed, with late commit writes;
  the commit enumeration paused at a page boundary, G2's commit landing behind the cursor and P's commit ahead of it, then finished:
  the second listing differs, the reader repeats, and the view is never G1 plus P (§5);
- a late commit object landing before, during and after the commit listings, and a segment uploaded after them:
  each view is a committed view, plus the segment in the last case (§5);
- a settled scope: listing view equal to the catalog view;
  an unsettled scope with a committed patch not yet folded and a superseded generation awaiting deletion, dropped from a rebuild's window:
  the listing view equals the last committed view (§5);
- a late segment of a scope that is not indexed: the listing view includes it, and the result is `incomplete` with reason `cold` (§5);
- a query spanning the window boundary: the indexed part follows the normal rules, the whole result is `incomplete` with reason `cold`,
  and a transaction lookup across the boundary never returns `unmatched` (§5);
- a query started just before an hour is removed, with retention advancing between each listing, read and output step:
  the reader ends the hour with the outcome removed, also after emitting some of its records, and emits no record of a lower generation (§5 Retention).

**Service vectors.**
The following vectors test a catalog and its deleting component against the contracts of §3–§5.
They are not part of the corpus of [FMT §16]:
the corpus vectors exercise views and what a reader or pack source computes from given objects,
while these exercise catalog transactions and lifecycle execution, which belong to the service.

End evidence:
- cold recovered boundary: capture C has contiguous records in indexed hour H; its clock steps back to hour K outside the window,
  where recovery uploads its `stop-unclean` segment with a gap that covers H;
  the registration is rejected, the per-capture entry records the boundary, and a query of H is `incomplete`;
- the same after a rebuild, with the segment still in `staging/`, and again after it was merged into an archive of K while K was indexed and K was then evicted:
  the rebuild reads K's pack and a query of H is `incomplete`;
- clean end evicted: a capture's final hour with a `stop` boundary is evicted;
  queries whose records and coverage lie in scopes that stay indexed give the same results as before;
  a transaction lookup whose primary lies in an indexed hour and whose eligibility window reaches the evicted hour becomes `incomplete` with reason `cold`, although the clean end is kept;
- bounded unclean end evicted: the time barrier keeps its gap bounds, and an epoch whose closure was recorded stays exempt from the epoch barrier after the closing scope is evicted;
- a converter archive of a scope that is not indexed, with boundaries: rejected, its per-capture evidence recorded;
- entirely cold capture: capture C has no indexed scope, and its recorded gap meets indexed hour H, which holds only capture D's records:
  a time-range query of H is `incomplete`, with C's barrier recorded at registration and again after a rebuild;
- rebuild ordering: a retained boundary segment that was uploaded but never presented for registration is read by the rebuild before it serves results;
  with the indexed records processed first and the cold boundary last, no result is served in between;
- registration during a rebuild: a boundary registered while the rebuild runs, including a retry after a lost response, is kept when the rebuilt entries are published.

Window and lifecycle:
- eviction between a reader's routing and its pack lookup, and between coverage lookup and pack lookup: the reader falls back to the listing view with `cold`;
- eviction racing a segment registration, a patch registration, a merge claim and an interrupted-admission recovery: eviction happens only in a settled state, inside the transaction;
- a scope with an open claim or a pending deletion stays indexed;
- commit cleanup: a live admission whose commit object appears while the deleting component works on the scope is never deleted;
  a late commit object of a cancelled admission whose packs are already deleted is kept until retention;
- a rebuild while the old deleting component still holds issued deletions: the rebuild waits until it has stopped;
- retention interrupted after the last pack deletion and before the commit-object deletion: the hour is reported removed,
  and a later sweep deletes the remaining commit objects and any segment uploaded later;
- all fence objects but the highest deleted, then a publisher restart: the new epoch outranks every surviving generation;
  all fence objects deleted, then a restart after a rebuild: the new epoch exceeds every epoch on indexed packs.
