# tracepack — storage profile

Status: current (2026-10-14) — v2.34, tracepack format 1.1.
External review consensus reached (review rounds recorded in `tracepack-spec-changelog.md`).
Normative for mergers, recorders, log converters and the service that deletes packs.

References: `[FMT §n]` = `tracepack-format.md`, `[SEM §n]` = `tracepack-semantics.md`, `[STO §n]` = `tracepack-storage.md`, `[OVW §n]` = `tracepack-overview.md`, `[JSONL §n]` = `tracepack-jsonl.md`, `[CORPUS §n]` = `tracepack-corpus.md`; `[FMT I-n]` = invariant I-n of the format document.
Each rule is defined in exactly one document; the others only reference it.

## 1. Scope and dependencies

This profile applies to object storage (S3-compatible, e.g. MinIO).
It defines the key layout, the pack roles in storage, the recorder and durable-bus contracts, merge, deletion and the converter contract;
the catalog, retention and publication bookkeeping are the service's (G5-201).
The format does not depend on it: reading and writing packs on a local file system needs only [FMT] and [SEM],
while the durable-bus contract follows this document wherever packs are stored.

Depends on:
- [FMT I-6] immutability, [FMT I-7] capture and epoch rules, [FMT I-12] record identity and dedup, [FMT I-13] hour-aligned blocks, [FMT I-14] mergeable footer.
- [FMT §8] transport-event and annotation payload tags (boundary records, converter annotations).
- [FMT §5] pack metadata tags (`pack_role`, `scope_generation`, `publisher_epoch`, `patch_base`, replacement-set tags, `compacted_from`, `supersedes`, `coverage`, `seq_start`, `flush_interval_ns`, `clock_step_tolerance_ns`, `thinning`, `redaction_policy`).
- [FMT §10] footer aggregation rule and validation; [FMT §13] verification, which merge input verification follows (§4).
- [FMT §6] block envelope and block body; [FMT I-2] block integrity; [FMT §12] block validation.
- [SEM §4] clock anchor and clock-step rule; [SEM §5] capture-boundary events; [SEM §6] effective quality; [SEM §7.2] transaction lookup; [SEM §7.4] reads over the packs a reader is given.
- For the log converter (§7): [SEM §2] reconstruction contract and capture model, [SEM §3] converter `decode_status` values, [SEM §4] source-log time rules,
  [FMT §7.2] record fields and `field_validity`, [FMT §9] enums.
- [SEM §2] records unobserved by design.
- [SEM §8] redaction: extracts, and the masking a consumer of a durable bus does (§4).

## 2. Scopes and roles

A **scope** is one (`capture_id`, UTC hour).
Every pack except an `extract` holds records of exactly one scope, and its period lies inside that hour;
a pack's scope comes from its period, never from its record timestamps, so a pack without records (e.g. an archive holding only `coverage`) still has a scope.

| `pack_role` | Written by | Content | `scope_generation` |
|---|---|---|---|
| `segment` | recorder, per flush; or a converter (§7) | records of one flush interval of one scope, all of them for a local recorder, a subset for a consumer of a durable bus (§4), which may write several segments per interval; for a converter, records of one scope, possibly of the whole hour; `compaction_level = 0`; masked under the capture's policy when the log service writes it under one ([SEM §8], §4) | 0 |
| `archive` | merger (or a converter directly, §7) | the records of the verified inputs of one merge, deduplicated, all of one scope; or a converter's records of one (capture, hour); the role and the period do not certify that every pack or record of the scope was included; masked records carried as their inputs hold them (§4 Merge) | ≥ 1 |
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
  `seq_first` is the pack's first seq (its `seq_start` when empty) in 20-digit zero-padded decimal, so segments list in seq order;
  the close segment of §4 has `seq_first` 2^63 − 1, `09223372036854775807`.
  A source that checks a key against the segment it names compares `seq_first` with the first seq of the first block it finds, and with `seq_start` only when the segment is known to hold no record;
  when damage hides the first seq, it skips that comparison and the read reports the damage (G5-147).
- Every key contains the `pack_id`, so every upload has a unique key and no object is ever overwritten.
  Retried and duplicate uploads are harmless: records are deduplicated by (`capture_id`, `seq`) in every read ([FMT I-12]).
- Packs are removed only by §4 Deletion and by the service's retention, which removes whole hours (§5).
  Storage lifecycle rules (e.g. S3 or MinIO ILM) MAY transition objects under `archive/` to another storage class, provided they stay readable and listable;
  they MUST NOT expire any object, and MUST NOT apply to `staging/`.
- Objects under `staging/` and `archive/` hold records in full, or masked under their capture's policy with the originals encrypted in place ([SEM §8]);
  only privileged components read them (recorders, the merger, the deleting component, the query service, privileged consumers),
  and the keys that restore an original are held by the log service (G5-221).
  How access is controlled is service design; consumers that are not privileged receive masked extracts ([SEM §8]).

## 4. Reads, flush, merge, deletion

**Packs a reader is given.**
A reader is given packs by its caller — the service's catalog, a listing of the key prefixes (§3), or local files — and reads their union,
deduplicated by (`capture_id`, `seq`) ([FMT I-12]), with conflicts reported ([SEM §7.4]).
A segment and an archive holding its records may both be given; the result is the same.
Which packs to give is the caller's choice; tracepack defines no view.

- **Flush**: a recorder finalizes a segment at a configured interval that divides one hour (e.g. 5 or 10 minutes), at a size threshold,
  and whenever the next record's UTC hour differs from the current segment's, so that every segment has one scope.
  Recorders SHOULD validate each block before writing it ([FMT §12]); the cost is one decode per block written.
- **Standalone recorder** (G5-150, G5-227): a standalone recorder, one that is not a consumer of a durable bus (below), writes segments as Flush says, so every segment has one scope;
  it keeps no spool and writes no `flush_interval_ns`.
  A crash loses the records not yet in a finalized segment:
  the capture has no end boundary, its lost records are seq gaps, and its successor's `start` and `previous_capture_id` remain the evidence of the restart.
  It writes a `stop` boundary on every orderly shutdown, so the case is limited to crashes.
- **Recorder over a durable bus** (G5-86): a deployment in which producers publish records to a message bus that persists each record before acknowledging the publish
  (the pilot uses NATS JetStream), and the recorder is a set of stateless consumers of that bus.
  - *Scope of the bus* (G5-249). Layout 1 of the bus message (below) serves captures whose `capture_method` is `raw-stream` or `decoded-message` and whose `time_source` is `capture-clock`:
    the frames of both establish every header field they hold, so a consumer derives `field_validity` from the payload.
    A log conversion writes packs by the converter contract of §7 and never publishes over the bus.
    Every capture a consumer closes therefore has `clock_step_tolerance_ns`.
  - *Producer duties.* The producer keeps a stable `recorder_instance_id` for its deployment across restarts,
    assigns a new `capture_id` each time it starts producing — at process start, and at each leadership change for a producer chosen by leader election (G5-200) —
    and names the immediately preceding capture in `previous_capture_id` ([FMT I-7]),
    assigns the capture-scoped `seq` ([FMT I-12]), and stamps `epoch`, `ts_utc_ns` and `mono_ns` at the observation point.
    It assigns `seq` to a record when it observes it, before placing it in any buffer from which it may drop records, so that every record it drops is a seq gap (G5-218).
    Seq 2^63 − 1 is reserved for a consumer's close (*Closing a capture left open*, below); a producer never assigns it.
    A producer keeps two seqs for the end of a capture, one for a `clock-step` the `stop` may need and one for the `stop`:
    it assigns an ordinary record, a `clock-step` before it included, only up to seq 2^63 − 4,
    and before assigning one that would pass that limit it ends the capture with an orderly `stop`, preceded by a `clock-step` if the anchor rule needs one, at most at seq 2^63 − 2,
    then continues in a new capture whose `previous_capture_id` names the ended one ([FMT I-7]); no seq is renumbered (G5-231).
    With the capture's `start` boundary it publishes a **capture descriptor**:
    the capture descriptor defined below, which the consumer copies into the pack metadata of every segment of the capture.
    It keeps the clock anchor of [SEM §4] and detects clock steps itself:
    it publishes the `clock-step` record and waits for the bus's acknowledgement before publishing the first record under the new anchor,
    so every record the bus holds was accepted after an anchor the bus also holds.
    It publishes every record of one capture, as the bus message below, on one bus subject that names the capture.
    Two producers of one `recorder_instance_id` may overlap briefly during a handover, each publishing only its own capture, never one capture from two processes (G5-200, G5-242).
    A consumer stores all of these as received and never reassigns, synthesizes or moves any of them,
    except that it masks under the capture's policy (*Masking*, below) and closes a capture its producer left open (*Closing a capture left open*, below).
  - *Bus message* (G5-216, G5-219, G5-241).
    The subject is `<prefix>.<tool token>.<capture_id>`.
    The **tool token** is `tool_id` escaped as §3 escapes it, with `.` also written `%2E`:
    every byte outside `A`–`Z`, `a`–`z`, `0`–`9`, `-`, `_` and `~` is `%XX` with two uppercase hex digits,
    so the token holds no `.`, `*`, `>`, whitespace or unescaped `%`, and percent-decoding restores `tool_id`.
    `capture_id` is in the canonical lowercase form (§3).
    One subject names one capture; this concerns the published subject:
    a stream may store a message under a transformed subject (e.g. with a partition token), and a consumer does not assume the stored subject equals the published one.
    The body of layout 1 is the 44-byte record header of [FMT §7.1], its fields in layout order (the bytes a block of one record holds, [FMT §6]),
    followed by the `payload_len` bytes of payload ([FMT §8]) inline, or by nothing on the claim-check path (*Large records*); no extension area travels on the bus.
    The producer fills `seq`, `ts_utc_ns`, `mono_ns`, `record_flags`, `epoch`, `kind`, `dir`, `fidelity`, `quality` (its declared bits, `unobserved` included) and `payload_len`,
    and never sets `redacted`.
    `fidelity` follows [SEM §2] by kind:
    for a data or control record, the default of its `capture_method` (`wire-exact` for `raw-stream`, `re-encoded` for `decoded-message`) unless the producer knows better for the record,
    e.g. `wire-exact` for a frame a `decoded-message` producer kept as raw bytes;
    for a transport-event or annotation record, `not-applicable`.
    On the bus the producer writes `decode_status`, `trailing_bytes` and `field_validity` as zero (G5-216); the consumer stores them derived by kind:
    for a data or control record not carrying `unobserved`, `field_validity` from the frame's captured length ([FMT §7.2]),
    and `decode_status` and `trailing_bytes` from the capture's classifier configuration (*Masking*);
    for a data record carrying `unobserved`, `field_validity` from its captured header, `decode_status` `not-attempted` and `trailing_bytes` 0 ([SEM §2]);
    for a transport-event or annotation record, `field_validity` 0, `decode_status` `not-applicable` and `trailing_bytes` 0 ([FMT §7.2], [SEM §3]).
    The message carries the header `Tpk-Layout: 1`;
    `Nats-Msg-Id: <capture_id>.<seq>`, `seq` in decimal without a leading zero, so the stream stores a retry once within its duplicate window;
    and, on the message of the capture's `start` record only, `Tpk-Capture-Descriptor`:
    the producer-owned entries of the capture descriptor (*Capture descriptor and pack metadata*, below) in the TLV encoding of [FMT §5], in standard base64 with padding (RFC 4648 §4).
    A message's size is its encoded header block plus its body, as the bus's `max_payload` counts them.
    The encoded header block of every message — `Nats-Msg-Id`, `Tpk-Layout`, the descriptor on a `start`, and the claim-check headers when present — is at most 4096 bytes.
    A deployment's `max_payload` is at least 4140 bytes, that bound plus the 44-byte body of a header-only message, so every header-only message fits;
    a producer configured with less starts no capture and reports the error.
    Before it publishes a capture's `start`, a producer encodes the final `start` message in the form it will publish (inline, or header-only with the claim-check headers, *Large records*)
    and checks its header block against 4096 bytes and the whole message against `max_payload`;
    when the check fails it does not start the capture and reports the error, so no capture exists whose descriptor cannot be published.
    A consumer takes a record's `capture_id` from `Nats-Msg-Id` and checks that its seq equals the record header's `seq`.
    A message is never written, and its seq is a seq gap, when its `Tpk-Layout` is unknown,
    its body does not have the layout's length (44 + `payload_len` without `Tpk-Payload-Ref`, 44 with it),
    its `Nats-Msg-Id` is not of the form above, names seq 2^63 − 1 (which no producer assigns) or disagrees with the header,
    or its claim-check headers are malformed (*Large records*).
    Terminating such a message on the bus and raising an alert is informative.
    A `start` the consumer cannot accept — its descriptor does not decode, or names a `capture_method` other than `raw-stream` or `decoded-message` or a `time_source` other than `capture-clock` —
    is never written and raises an alert, and the capture's later records stay deferred on the bus until an operator acts (G5-251).
  - *Large records* (G5-217).
    A record travels inline when its body is at most `max_payload` − 4096 bytes; otherwise by claim-check:
    1. the producer puts the payload as the object `<capture_id>.<seq>` into an object bucket and waits until the put completes;
    2. it publishes the record's message with a body of the 44-byte header alone, `payload_len` the payload's true length,
       and the headers `Tpk-Payload-Ref: <bucket>/<capture_id>.<seq>` and `Tpk-Payload-Digest: SHA-256=<digest>`,
       `<digest>` the base64url encoding with padding (RFC 4648 §5, 44 characters) of the SHA-256 digest of the payload bytes, the form the NATS object store records;
    3. the consumer fetches the object (except as *Already staged* below allows), computes SHA-256 over the fetched bytes and compares it with the header,
       and compares their length with `payload_len`;
       it writes the record like an inline one only when both match, and never writes a record header without its payload;
    4. it defers the message (a negative acknowledgement with a delay, and an alert) when the object is missing or fails either check;
    5. it deletes the object only after a confirmed acknowledgement of the message, and only once the lifetime invariant below allows it.

    **Lifetime invariant**: an object stays available while any message naming it may still need it:
    every message naming it that the bus has accepted and that is not yet acknowledged, a duplicate accepted outside the duplicate window included,
    and every publish attempt naming it that the producer may still make, until the producer's **retry horizon** (a deployment setting) has passed.
    **Already staged**, an exception to steps 3 and 4: a consumer that receives a message whose (`capture_id`, `seq`) it knows to be durably staged
    may acknowledge it without fetching or checking the object, the duplicate being harmless ([FMT I-12]); such a message no longer needs the object.
    How the log service meets the invariant is its own (G5-217);
    for example, a sweeper deletes an acknowledged object only after a grace period longer than the producers' retry horizon,
    and only when no unacknowledged message still names it or every such message will be acknowledged as already staged.
    Uploading the object again before a retry does not meet it, since an earlier consumer may delete the object meanwhile.
    A claim-check message whose `Tpk-Payload-Ref` or `Tpk-Payload-Digest` is missing, names another object, has another prefix, or does not decode to 32 bytes is malformed: it is never written (G5-241).
    The object bucket has no `max_age`, `max_bytes` or `max_msgs` limit, as the traffic stream (G5-213), and *Clear-text protection* applies to it.
    The seq of an object never published is a seq gap; the record of an object left after its acknowledgement is staged; removing either is the log service's.
    `max_payload` is a deployment setting, not a tracepack value.
  - *Durability.* The bus's acknowledgement is a record's durability point;
    the producer decides how long it buffers an unacknowledged record and what it does when the bus is unreachable,
    and a record it drops is a seq gap: a lookup reports it inside its window ([SEM §7.2]); detecting gaps over a whole capture is the service's ([STO §5]).
    F = `flush_interval_ns` is the consumer's maximum normal segment-flush interval, not the time from observation to acknowledgement;
    the consumer writes it into every segment ([FMT §5]).
    A consumer holds unflushed records in memory and acknowledges a record to the bus only after the segment holding it is durable in `staging/`;
    a consumer that stops before that acknowledges nothing, and the bus redelivers its records to another consumer.
    A listing of `staging/` sees a record only once a segment holds it.
    A restart of the bus server loses the records the producer could not buffer meanwhile,
    and, with the server's default disk sync interval, acknowledged records not yet synced; both read as seq gaps (informative).
  - *Clear-text protection* (G5-207, G5-213).
    The traffic stream and the claim-check object bucket hold clear-text records,
    so the bus that carries them uses TLS for every connection of producers, consumers and the service, and JetStream encryption at rest for the stream and the bucket;
    neither has an age, size or count limit.
    These are required deployment protections, apart from the tuning below.
  - *Stream settings* (informative).
    Work-queue retention, file storage, no `max_age`, `max_bytes` or `max_msgs` limit, and `compression: s2`;
    an `ack_wait` longer than F plus the upload, or in-progress acknowledgements while a consumer holds records;
    and a `max_ack_pending` above the peak record rate × (F + upload time), e.g. 120 000 at 2 000 records per second and F = 60 s.
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
    the always-required `tool_id`, `transport`, `capture_method`, `vantage`, `recorder`, `time_source`, `lifecycle_coverage`, `quality_evaluated` and `recorder_instance_id`,
    `capture_method` being `raw-stream` or `decoded-message` (*Scope of the bus*);
    `capture_origin_utc_ns`, `capture_origin_mono_ns` and `clock_step_tolerance_ns`, required, since the bus serves capture-clock captures ([FMT §5]);
    `previous_capture_id` and the optional site and equipment tags when it has them;
    and the `thinning` entries when it thins records ([FMT §5], *Unobserved records*).
    The consumer copies these values as decoded, applying no default: a `capture_origin_mono_ns` other than 0 and a `clock_step_tolerance_ns` of 0 are kept.
    A descriptor holding a top-level tag the consumer does not know, an unknown nested tag inside `thinning` or `hsms_timers`, or a `thinning` entry whose stream is above 127 or whose function is above 255,
    does not decode, and its `start` is not accepted (G5-251, G5-252, G5-254, G5-256);
    a reader of a stored pack and a merge still skip an `hsms_timers` nested tag they do not know ([FMT §5], Merge).
    The consumer owns `writer`, `classifier`, `max_frame_len`, `period_start`, `period_end`, `seq_start`, `pack_role`, `compaction_level`, `scope_generation`, `flush_interval_ns` and `redaction_policy`.
    The traffic stream is configured so that an accepted message is removed only by acknowledgement, never by a size or age limit,
    so an unacknowledged `start` stays available until its descriptor is registered and a deferred record until its descriptor exists.
    The consumer that receives a capture's `start` record records the descriptor with the service (e.g. in its catalog) before acknowledging it;
    a consumer holding a record of a capture whose descriptor the service does not have yet defers the record
    (a negative acknowledgement with a delay, so the bus redelivers it later) instead of writing a segment without the required metadata.
  - *Masking* (G5-207, G5-214, G5-222, G5-250).
    The log service chooses one classifier configuration, masking policy, digest key and encryption key per capture, for example when it registers the descriptor,
    and every consumer writes the capture's records only with them; a new configuration, policy or key applies from the next capture.
    The **classifier configuration** is the classifier's implementation and every setting that changes its result, `max_frame_len` and the absence of a limit included (G5-250).
    A consumer that cannot classify with it, apply the policy or keys, or write the format features they need defers the record (a negative acknowledgement with a delay),
    so a consumer of another version joins a capture only by applying the configuration the capture already has.
    A consumer classifies a record from its payload as received, then masks it ([SEM §8]), so `decode_status` and `trailing_bytes` describe the source bytes.
    Every pack of a capture masked at write — a pack whose role is not `extract` and that carries `redaction_policy`, the **masked-at-write profile** of format 1.1 —
    carries the capture's `redaction_policy`, with the encryption tags (G5-243), and the capture's `classifier` and `max_frame_len` values,
    or no `max_frame_len` when the classifier has no limit, whether or not it holds a classified record;
    so no capture masked at write has an unmasked copy in storage, and every pack of it names the same configuration.
    Masking is deterministic, so a redelivered record is written with the bytes of its first write ([FMT I-12]).
    A consumer that cannot finish screening a record defers it, and never writes it unscreened ([SEM §8]).
    A record carrying `quality.unobserved` is never masked (G5-237).
  - *Unobserved records* (G5-224, G5-225).
    A producer thins a record only when it identifies it, by a means other than stream and function (for example the connection or channel the poll travels on, or a flag from the component that sends it),
    as an S1F3 of the FDC service's interval polling, or as the S1F4 reply to such a poll, paired with it by System Bytes,
    **and** its descriptor declares that stream and function in `thinning`: the declaration is necessary, never sufficient.
    It zero-fills the record's message text and sets `quality.unobserved` ([SEM §2]).
    Event-triggered FDC queries, a reply whose message text is empty, and every other S1F3 and S1F4 are kept in full.
    How the gateway identifies its FDC polls is the gateway project's choice (G5-225).
  - *Closing a capture left open* (G5-223, G5-231, G5-240).
    A producer publishes a `stop` boundary on every orderly shutdown, so the case is limited to crashes;
    the successor's `start` and `previous_capture_id` remain the evidence of the restart.
    The log service may close a capture whose producer stopped without a `stop` boundary, when it decides to (e.g. when a later capture of the same tool starts, or after inactivity).
    The close is one `stop-unclean` capture-boundary record with seq 2^63 − 1, the largest seq format major 1 permits ([FMT §2]), which no producer assigns,
    written alone in a segment of the capture's scope for its `ts_utc_ns`;
    it is the one record a consumer writes that its producer did not publish, and the capture's last seq: no seq follows it, and no pack's `seq_start` is 2^63.
    Its record is a transport-event, `dir` local, `epoch` 0, `fidelity` and `decode_status` `not-applicable`,
    `quality` `capture-boundary` and `correlation-incomplete` (for epoch 0, [FMT §7.2]), `mono_present` clear;
    its payload holds `event` capture-boundary, `boundary_kind` stop-unclean, `gap_start` and `gap_end`;
    its `ts_utc_ns` is the **trigger time**: the successor's `start` `ts_utc_ns` when a successor triggers the close, else the log service's wall clock.
    Its bounds are computed in checked `i64` arithmetic:
    `gap_start` is the `ts_utc_ns` of the highest seq the service staged minus `clock_step_tolerance_ns`, absent when no record of the capture is staged;
    `gap_end` is the trigger time plus `clock_step_tolerance_ns`;
    a bound whose computation leaves the `i64` range is absent, unbounded on that side.
    A tolerance of 0 gives the bare times, and a gap whose `gap_start` exceeds its `gap_end` meets every time range ([SEM §7.4]).
    Its segment carries the capture's descriptor, and the consumer-owned values every segment of the capture carries
    (`flush_interval_ns`, and `redaction_policy`, `classifier` and `max_frame_len` as *Masking* requires);
    its `seq_start` is 2^63 − 1, and its `format_minor` follows [FMT §14].
    At most one close is written per capture: the log service records the close, its trigger time and its bounds (e.g. in its catalog) before writing it, and a retry writes the same bytes,
    so two closes of one capture never conflict ([FMT I-12]); how it keeps this record is the service's bookkeeping.
    A capture whose `start` never reached the bus has no descriptor and stays open.
    A record of the capture delivered after the close is staged and read like any other, and a read whose time range the gap meets reports `incomplete` ([SEM §7.4], G5-209);
    the close ends no epoch ([FMT §10]).
    The service's detection of seq gaps over a whole capture treats the seqs between the last staged record and 2^63 − 1 as the capture's unknown tail, not as lost records.
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
  a classified record without a `classifier` in that input's metadata, an `oversized` record without a `max_frame_len` there,
  a `quality.redacted` record in an input without `redaction_policy`;
  in an input with it, any record that carries `redacted` or is named by a `redaction` entry and fails the checks of [SEM §8] Marking,
  or `redaction-present` set other than exactly when the input has a `redaction` entry;
  or a `quality.unobserved` record whose stream and function its input's `thinning` does not declare.
  A reader treats such redaction defects as recoverable ([SEM §8]); a merge, which writes a new pack, refuses them.
  The merge also fails, publishing nothing, when an input's `format_minor` exceeds the newest minor the merger implements, since a merge drops unknown top-level entries (below),
  or when an input's preserved bytes would gain, under the output's `format_minor`, a meaning they lack under the input's own:
  a `quality` bit 7 in a minor-0 input merged into a minor-1 archive ([FMT §14]).
  Every input carries the same value of each **capture-level tag**:
  `tool_id`, `transport`, `capture_method`, `vantage`, `recorder`, `time_source`, `lifecycle_coverage`, `quality_evaluated`, `recorder_instance_id`,
  `capture_origin_utc_ns`, `capture_origin_mono_ns`, `clock_step_tolerance_ns`, `previous_capture_id`,
  `site_id`, `equipment_model`, `equipment_sw_rev`, `host_software`, `host_endpoint`, `equipment_endpoint`, `equipment_connect_mode`, `device_id`, `hsms_timers`,
  `source_tz`, `source_dialect`, `source_ref` and `thinning`:
  absent in all inputs, or equal values in all, compared as decoded values;
  for `hsms_timers`, the same timers T1–T8, each absent in all or equal, while nested entries a reader does not know are neither compared nor carried;
  for `source_ref`, the same ordered list, duplicates included,
  because an annotation's `source_index` is a position in that list ([FMT §8]) and a merge never rewrites record bytes;
  for `thinning`, the same ordered list of entries (G5-239).
  Otherwise the merge fails.
  Inputs that carry `redaction_policy` carry equal values of it, the same single `classifier`, and the same `max_frame_len` value or its absence in all (G5-222, G5-232, G5-250);
  an input with `redaction_policy` beside one without, of the same capture, fails the merge (G5-235);
  and an input whose role is not `extract` and whose `redaction_policy` lacks either encryption tag fails the merge, whatever its minor (G5-253).
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
  Its file header carries the capture of S, a new `pack_id`, the largest `format_minor` of the inputs ([FMT §14]), and the `redaction-present` flag exactly when it carries a `redaction` entry.
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
  for inputs that carry `redaction_policy`, that `redaction_policy`, their one `classifier` and their `max_frame_len` value or its absence,
  and the `redaction` entries of the records it writes, each once, ordered by `seq` then first masked offset ([FMT §5]):
  the entries of a record kept from several inputs are equal, since its copies are byte-identical ([FMT I-12]),
  and differing entries for one record fail the merge as a conflict does, even when the record bytes are equal;
  for inputs without it, every distinct `classifier` and `max_frame_len` value of the inputs (G5-108);
  the merger's own `writer`;
  and `seq_start` = the first record's seq, or for an archive without records the largest `seq_start` of the inputs (G5-112).
  It carries no `flush_interval_ns` (an archive is not written by a recorder), `notes`, `patch_base` or `extract_filter`,
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
   Such segments come from finite input, so the recorder's flush contract (§4) does not apply to them, and they write no `flush_interval_ns`;
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
- the bus message (§4): round trips of a `start`, socket events, a `clock-step`, an orderly `stop`, ordinary frames and unobserved frames,
  each stored with the `fidelity`, `field_validity`, `decode_status` and `trailing_bytes` its kind gives; a `decoded-message` capture holding a frame kept as `wire-exact`;
  a tool token that needs escaping, `.` included, round-tripping; a descriptor round-tripping a nonzero `capture_origin_mono_ns` and a zero `clock_step_tolerance_ns`;
  a `start` whose header block exceeds 4096 bytes, and a `max_payload` below 4140: no capture started;
  each malformed message — an unknown `Tpk-Layout`, a body of the wrong length, a `Nats-Msg-Id` of another form, naming seq 2^63 − 1 or disagreeing with the header,
  missing or malformed claim-check headers — never written; a claim-check object whose length or digest does not match, an equal-length corrupted one included: deferred;
- masked merges (§4): entries carried, deduplicated and ordered, and the originals restored from the archive, through copied, coalesced and resolved blocks;
  inputs differing in `redaction_policy`, `classifier` or `max_frame_len` (one with a limit, one without), a masked input beside an unmasked one,
  and an input holding a record that an entry names while its `redacted` bit is clear: the merge fails and publishes nothing;
  an unmasked merge of two classifiers still succeeds;
  an input of a newer `format_minor`, and a minor-0 input with a record carrying `quality` bit 7 beside a minor-1 input: the merge fails;
  inputs whose `thinning` lists differ: the merge fails;
- the close (§4): a close-only segment at seq 2^63 − 1 counted as one record; the close merged with its capture's archive, the seq ranges kept exact;
  a record delivered after the close; a retried close byte-identical and read once; its gap read as a crash gap from the footer and from the record;
  bounds at the `i64` extremes absent; the close segment of a masked and thinned capture carrying its inherited values and merging.

The claim-check crash windows and the lifetime invariant of §4 — a stop after the object put and before the publish, after the segment upload and before the acknowledgement,
after the acknowledgement and before the delete; a lost publish acknowledgement and a retry outside the duplicate window; a retry concurrent with the acknowledgement of an earlier message;
several accepted messages naming one object — are tests of the log service, not of the Go module;
so are its TLS and encryption at rest, which are checks of a deployment.

