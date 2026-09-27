# tracepack — record semantics

Status: current (2026-09-27) — v2.9, tracepack format 1.0.
External review consensus reached (review rounds recorded in `tracepack-spec-changelog.md`).
Normative, language-agnostic.

References: `[FMT §n]` = `tracepack-format.md`, `[SEM §n]` = `tracepack-semantics.md`, `[STO §n]` = `tracepack-storage.md`, `[OVW §n]` = `tracepack-overview.md`; `[FMT I-n]` = invariant I-n of the format document.
Each rule is defined in exactly one document; the others only reference it.

## 1. Depends on

- [FMT §7] record header fields and their classes, [FMT §8] payload TLV bodies, [FMT §9] enum registries.
- [FMT I-7] epoch binding, [FMT I-10] payload authority, [FMT I-11] derived values, [FMT I-12] record identity, [FMT I-13] hour-aligned blocks.
- [FMT §10] footer F-2 / F-3 / F-5 content used by pruning and lookup; [FMT §13] bootstrap, which a reader of a listing view performs before using that content.
- [FMT §6] block read levels (full, header-only) and attested blocks, with [FMT §5] `blocks_validated`, used by the query modes of §7.4.
- [FMT §2] portable encoding rules, including the index-hash requirements; [FMT §5] pack metadata tags used by capture, time and quality semantics;
  [FMT I-6] immutability; [FMT §16] corpus contract for the vectors of §9.
- [FMT §5] `redaction_policy` and `redaction` entries, [FMT §4] `redaction-present`, used by §8.
- [STO §4] recorder durability contract and durable clock anchor; [STO §5] catalog completeness (seq coverage, barriers) and listing views used by transaction lookup and effective quality;
  [STO §6] corrections (patches) that re-emit records with new stored bits.

## 2. Capture model: fidelity, capture method, vantage

Independent facts describe where the bytes came from.

| Field | Where | Question it answers |
|---|---|---|
| `capture_method` | pack metadata | which representation the bytes were taken from |
| `vantage` | pack metadata | where the observation happened |
| `fidelity` | each record | how faithfully this record's payload reproduces the transmitted bytes (for a `redacted` record, the source payload, §8) |
| `recorder` | pack metadata (utf8) | which product did it (tracing only; never branched on) |

`capture_method` values:

| Value | Bytes taken from | Default `fidelity` |
|---|---|---|
| `raw-stream` | the transport's raw byte stream before any protocol decoding: socket reads/writes inside an endpoint or relay, or TCP payload reassembled from a packet capture | `wire-exact` |
| `decoded-message` | messages after an HSMS or SECS-I implementation decoded them, re-encoded for storage | `re-encoded` |
| `log` | a log written by other software (SML text or any other log format) | `reconstructed` |
| `generator` | a program that produced the messages without transmitting them (simulator output, test fixtures) | `synthesized` |

`vantage` values:
`host` and `equipment` = inside that endpoint or on its machine;
`intermediary` = a relay, proxy or hub between the endpoints;
`network` = passive observation of the link (port mirror, network tap, packet capture);
`none` = nothing was observed (`capture_method = generator`).
For `capture_method = log`, `vantage` is where the logging software ran.
The vantage determines what `ts_utc_ns` and `dir` mean physically (§4).

`fidelity` values:

| Value | Meaning |
|---|---|
| `wire-exact` | the bytes as transmitted, including malformed frames |
| `re-encoded` | produced by re-encoding a decoded message; identical to the transmitted bytes only for well-formed frames, and malformed frames cannot appear |
| `reconstructed` | regenerated from a non-binary source; header identities come from source metadata; cannot reproduce non-canonical item encodings, trailing bytes, malformed headers or byte-level anomalies |
| `synthesized` | produced by a generator; never evidence of real traffic |
| `not-applicable` | transport-event and annotation records |

A writer uses the default fidelity of its `capture_method` unless it knows better for a record,
e.g. a `decoded-message` recorder that also keeps the raw bytes of frames it failed to decode writes those records as `wire-exact`.

**Consumer rule**:
semantic-level analysis MAY use `wire-exact`, `re-encoded` and `reconstructed` records;
transport-level and malformed-frame analysis requires `wire-exact`;
`synthesized` records are never counted as evidence of real traffic.
The evidence grades `observed` (`wire-exact` or `re-encoded`) and `reconstructed` are views derived from `fidelity`, not stored fields.
They apply only to values that are not masked (§8): a masked value of a `redacted` record is unavailable for every analysis, and its `fidelity` describes the capture, not the zeros.

**Reconstruction contract** (`capture_method = log`):
the writer builds each message from its source text,
then sets SessionID / DeviceID, System Bytes and the W-bit from the source's per-entry metadata, never from defaults.
A W-bit on an even (reply) function is invalid (SEMI E5 §7.2: replies never request a reply);
such an entry is stored as an annotation with `decode_status = build-rejected`, never as a silently repaired message.

## 3. decode_status (decision table)

`decode_status` is derived ([FMT I-11]): it records what the `classifier` concluded about the payload (for a `redacted` record, the source payload, §8).
Predicates are defined on the captured bytes, per SEMI E37 §8 and E5 §9.

| Status | Predicate (evaluated in this order) | Malformed? |
|---|---|---|
| `not-attempted` | the writer did not classify | n/a |
| `short-frame` | captured bytes < 14 (length + header) | yes |
| `length-mismatch` | length field ≠ captured length − 4 | yes |
| `bad-ptype` | PType ≠ 0 (E37 §8.2.6; 0 = SECS-II) | yes |
| `bad-stype` | SType not defined by E37 Table 5 (defined: 0–7, 9) | yes |
| `control-with-body` | SType ≠ 0 and length field ≠ 10 | yes |
| `oversized` | frame length exceeds `max_frame_len` | yes |
| `item-decode-error` | SType = 0, message text non-empty, and it does not begin with one complete, valid SECS-II item or list per E5 §9.2–9.3 (defined format code, 1–3 length bytes, body length consistent with the format's element size, every nested list element present) | yes |
| `ok-with-trailing` | item valid and message text continues after it; `trailing_bytes` = the excess | quirk, not malformed |
| `ok` | item valid with no excess, or empty message text (header-only data message), or a well-formed control message | no |
| `reconstructed-ok` | `log` writer parsed and built the message | no |
| `parse-failed` | `log` writer could not parse the entry; stored as an `unparsed-entry` annotation | n/a |
| `build-rejected` | `log` writer parsed the entry but the message is invalid (e.g. W on an even function, §2) | n/a |
| `not-applicable` | transport-event and annotation records not covered by the rows above | n/a |

The exhaustive item-validity checklist is part of the conformance corpus ([FMT §16]), one vector per rule;
E5 §9 is its normative base.
Decoders differ in strictness on edge cases;
because the bytes and the `classifier` are both stored, a consumer can always reclassify, except over masked ranges (§8).

**Applicability by kind and capture method**, before the table is consulted:
control records reach `ok` when the frame parses (there is no SECS-II item);
`log` writers use only `reconstructed-ok`, `parse-failed` and `build-rejected`;
other transport-event and annotation records use `not-applicable`.
**Malformed predicate**: `decode_status ∈ {short-frame, length-mismatch, bad-ptype, bad-stype, control-with-body, oversized, item-decode-error}`.
`quality.decode-failed` is set iff malformed; `not-attempted` and `not-applicable` never set it.
`fidelity` is independent of `decode_status`.

## 4. Time

- `ts_utc_ns`: for `time_source = capture-clock`, the wall clock at the vantage point when the **complete frame** was observed.
  A first-byte time, if ever wanted, is an additional field in a minor version, never a reinterpretation.
  For `time_source = source-log`, the source timestamp parsed in `source_tz` and converted to UTC.
- `mono_ns`: capture-clock only; elapsed monotonic ns since `capture_origin_mono_ns`,
  whose wall-clock counterpart is `capture_origin_utc_ns` (both identical across all packs of one capture).
  Latency between two records of one capture is the `mono_ns` difference, immune to wall-clock steps.
  A recorder restart starts a new capture id, so mono values are never compared across captures.
- Source-log records: `mono_present` clear and `quality.no-mono` set;
  DST-ambiguous or nonexistent local times are resolved to the earlier instant and flagged `ordering-uncertain`.
- Clock steps: a capture-clock writer keeps a **clock anchor** (wall, mono):
  (`capture_origin_utc_ns`, 0) until the first `clock-step` event, then (`ts_utc_ns`, `mono_ns`) of the latest `clock-step` event;
  anchor mono values are capture-relative, like `mono_ns`.
  The anchor moves only at a durable `clock-step` event, never per record, so tolerated drift cannot accumulate.
  For every new record the component that observes both clocks — the writer, or for a capture carried over a durable bus ([STO §4]) the producer — computes the drift `(wall − anchor_wall) − (mono − anchor_mono)`;
  when |drift| exceeds `clock_step_tolerance_ns`, it closes and durably flushes the current block, writes a `clock-step` transport event (`clock_step_ns` = the drift),
  makes it durable, and takes it as the new anchor before appending the record.
  Hence every record satisfies |`ts_utc_ns` − (anchor_wall + `mono_ns` − anchor_mono)| ≤ `clock_step_tolerance_ns` for the anchor in force when it was written,
  and that anchor is durable ([STO §4]).
  A local writer flushes its block before writing the `clock-step`;
  a producer over a durable bus publishes the `clock-step` and waits for the bus's acknowledgement before publishing records under the new anchor, and manages no blocks.
  A pack writer that receives records from a producer preserves the `clock-step` records it receives and never synthesizes or moves one;
  no component re-checks the rule, which holds by the producer's construction.
- Records within a file are in **write order**, which is capture order;
  timestamps may be unordered (clock steps, late input).
  Time queries therefore prune by `ts_min` / `ts_max` and scan every overlapping block (§7.4).

## 5. Transport events

States are the E37 connection states plus `unknown`.
The table says what a writer may record and from what kind of source.

| Event | Source requirement | Required context | Duplication policy |
|---|---|---|---|
| state-transition | a state-change notification from the HSMS implementation (previous state, current state, cause) | **epoch attribution**: a notification that does not itself identify its originating socket or connection generation may arrive late from an earlier connection, so the writer's *current* socket cannot be assumed to be its origin. A nonzero epoch is written only when the writer has independently established the originating socket (e.g. exactly one socket has ever existed in this capture, or the notification carries a generation id); otherwise the record gets `epoch = 0` and `correlation-incomplete`, keeping the transition and its real cause. The same rule applies to notification-derived timer records | one record per delivered notification, stored as a transition with its real cause; `lifecycle_coverage` says whether notifications were subscribed, and absence of a transition record is never proof that none occurred (notifications may be dropped or coalesced) |
| timer-expiry T3 | the HSMS implementation's per-transaction reply-timeout outcome | the primary's session, S/F and System Bytes ([FMT §8] tags), from the writer's context when the outcome report lacks them | one per timed-out transaction |
| timer-expiry T6 / T7 | state-change notifications whose cause is a T6 or T7 timeout | an implementation may aggregate repeated linktest failures into one `linktest-failure` cause rather than one T6 per probe; epoch attribution as for state-transition | one per notification |
| timer-expiry T5 / T8 | only when a `raw-stream` observer can attribute them | — | never synthesised |
| linktest / select / deselect / separate / reject | the control frames themselves (kind=control), plus the resulting state-transition | — | control frames are **not** duplicated as events |
| socket-accept / socket-connect / socket-close | **socket observation only**. A state-change cause never establishes a socket event: "local close" means teardown was initiated, and a transport-error cause may cover several socket outcomes | `socket_role` | one per observed socket event; a writer without socket observation writes none, or, if it derives one, sets `inferred` and uses the notification timestamp. When both sources exist, the observed record is the socket event and the notification stays a transition; nothing is emitted twice |
| clock-step | the comparison of wall and monotonic clocks by the component that observes them, the writer or the producer that feeds it (§4) | `clock_step_ns` | one per detected step, durable before the next record |
| capture-boundary | writer start / stop / gap; recovery after a crash (`stop-unclean`) | `boundary_kind`; seq range; `gap_start` / `gap_end` for gaps (including packet loss in a network capture and recorder downtime between linked captures) | one per boundary |

Causes the source does not name are stored as `unknown` with the source's own name in `cause_raw`,
never mapped to a plausible value.

## 6. Stored and effective quality

Stored `quality` bits record only what the writer knew **when it wrote the record**:
`epoch = 0` or missing source metadata (`correlation-incomplete`), clock or DST problems (`ordering-uncertain`),
`decode-failed`, `direction-inferred`, `redacted`, `no-mono`, and `capture-boundary` on the boundary record itself.
Clear stored bits mean checked absence of those conditions only when the pack metadata declares `quality_evaluated = true`.
A writer never revisits a record already written, so stored bits never depend on later records ([FMT I-6], [FMT I-12]).
`redacted` is set only by an extract writer (§8).

**Effective** quality is computed by readers at query time from stored bits plus context:
- a primary that is `unmatched` (§7.2) in an epoch that also contains a `capture-boundary` or an `ordering-uncertain` record is `correlation-incomplete`;
- a record inside a `coverage` range, or in a seq gap that [STO §5] Completeness cannot fill, or in a scope outside the catalog's index ([STO §5]), is `incomplete`;
- a record from a pack with `quality_evaluated = false` never yields clean evidence.

F-5 `boundary` and `epoch` entries ([FMT §10]) let a catalog, or a reader of a listing view after the [FMT §13] bootstrap, evaluate these conditions without reading blocks.
A correction pack may still re-emit records with new stored bits ([STO §6]); it never changes records in place.

## 7. Indexes and lookup

### 7.1 Per-block summary (footer F-3)

Defined now ([FMT §10]): counts per kind, direction and `decode_status`, `max_payload_len`, `quality_union`.
Deferred index structures ([OVW §6]), in the reserved F-3 tags:
an S/F presence structure (exact set when ≤ 64 distinct pairs, bitmap otherwise),
a System Bytes locator (§7.2), and a session-id set with overflow flag.
Time, epoch and seq ranges are in F-2.
**Contract**: a summary may produce false positives and must never exclude a matching record;
an absent tag, an absent summary or an overflowed structure means "may match".
The contract holds for matching on copy fields.
In authoritative mode (§7.4), a structure derived from copy fields prunes only attested blocks ([FMT §6]).

### 7.2 Transaction identity and lookup

Transaction key = (`capture_id`, `epoch`, initiating direction, `session_id`, `system_bytes`, primary `seq`).
Three relationships stay distinct:
- **Candidate association**: same capture and epoch, opposite direction, same session and System Bytes.
  Candidates are always returned so anomalies stay visible.
- **Eligibility window**: only a candidate whose seq is greater than the primary's seq,
  and smaller than the seq of the next primary with the same (epoch, direction, session, System Bytes) if one exists, can match;
  candidates outside the window are reported as anomalies and never match.
- **Protocol-valid match**: a candidate with the **same stream** and function + 1 (E5 §7.2) or function 0 (E5 §10.4.1 abort);
  a wrong-stream F0 is still a mismatch.
- **Observed runtime outcome**: a `Reject.req` control record or a T3 `timer-expiry` event attached to the primary (§5);
  never promoted to a conforming reply.

Which steps need what:
candidate association, the eligibility window and the protocol-valid match use only record-header fields (seq, epoch, direction, SessionID, System Bytes, S/F);
candidate selection by SessionID and System Bytes follows the query modes of §7.4.
Establishing `unmatched` also needs completeness evidence per [STO §5] (below),
and observed runtime outcomes need payloads: the T3 `timer-expiry` primary identifiers are transport-event TLV fields ([FMT §8]).

Results: `matched` (exactly one valid match), `ambiguous`, `unmatched`, `incomplete`;
each is reported together with the validation status of §7.4.
**`unmatched` requires that every record of the eligibility window was searched**:
the seq coverage over the eligibility window is contiguous per [STO §5] Completeness (never the case when the window touches a scope that is not indexed),
and the window is closed — by the next same-key primary, or by evidence that the queried epoch itself ended
(an `epoch` entry for it has `close_seq`, or the capture ended with a clean `stop`);
the existence of a later epoch is **not** closure evidence, because accepted-then-refused sockets get their own epochs while an earlier connection stays open ([FMT I-7]);
if absence cannot be established (packs missing from the catalog or a listing view, an unevaluated pack, a `capture-boundary` in the epoch,
`correlation-incomplete` on the primary) the result is `incomplete` **with the searched scope**.
Per-block System Bytes locator: sorted distinct values when ≤ 4096,
else a Bloom filter sized for ≤ 1 % false positives at the block's cardinality
(FP ≈ (1 − e^(−k·n/m))^k; n = 1500, k = 4 needs m ≈ 2 KiB, not 256 bytes),
hashed per [FMT §2].

### 7.3 Secondary index (optional, footer F-4)

Extracted well-known ids (CEID, ALID, RPTID, SVID, ECID, RCMD) from a **versioned extraction rule set** (`extraction_version`).
Per (block, id kind) a completeness marker: `complete`, `partial` (some records failed extraction or, in an extract, carry `redacted`, §8), `absent`.
Readers scan whenever the marker is not `complete`.
Values carry a type discriminator (unsigned / ASCII);
ASCII ids are stored verbatim (bounded length), not hashed.
The index answers "which records contain id X in a documented position" and nothing broader.

### 7.4 Query mapping

| Query | Mechanism |
|---|---|
| (a) tool + time | catalog, or listing views for scopes it does not index ([STO §5]) → packs whose `ts_min` / `ts_max` overlap → F-2 entries whose range overlaps → range reads of those blocks → scan |
| (b) + S/F | F-3 S/F presence prunes; exact match by record-header scan |
| (c) CEID / ALID / … | §7.3 when `complete`, else decode-and-scan |
| (d) System Bytes → pair | locator prunes (when present); scan; §7.2 matching across all packs of the (capture, epoch) per the catalog or listing views; `incomplete` with scope when coverage is not contiguous ([STO §5]) |
| (e) transport events | kind count prunes; scan; control frames are records, so Linktest / Select queries read kind=control |
| (f) SML full text, (g) cross-tool | scan, via listing views where not indexed; catalog statistics (F-5) cover indexed scopes, and an aggregate over a range with scopes that are not indexed is `incomplete` with reason `cold` |

**Query modes.**
A query runs in one of two modes.
- **Provisional**: filters on copy fields, and the F-3 / F-4 structures derived from them, may be applied to unvalidated header copies.
  If the result relied on the header copies of a non-attested block ([FMT §6]), or on F-3 / F-4 structures derived from them —
  to include its records or to exclude the block, whether or not the block was read —
  the result carries the validation status `header-validated` with the blocks concerned, also when the result is empty.
  `header-validated` means "selected by unvalidated copies"; for a block excluded by an index alone, none of its bytes were read.
- **Authoritative**: a copy-field predicate is evaluated either on the values in the payload ([FMT I-10])
  or on header copies of attested blocks.
  A non-attested block is never excluded by its header copies or by F-3 / F-4 structures derived from them;
  it is read in full and its records are selected by payload values.

In both modes, a query that returns payloads reads those records' blocks in full.
Where two packs in the active view hold the same (`capture_id`, `seq`) ([FMT I-12]; visible from F-5 `seq_range`, located through F-2),
the blocks holding it are read in full and compared byte for byte before any filter discards a representation,
so byte identity and `conflict` are decided in both modes; attestation proves neither cross-pack equality nor the absence of conflicts.
The validation status is independent of `incomplete`, `conflict` and the transaction outcomes of §7.2:
a result reports each that applies, and `header-validated` never replaces or hides another status.

## 8. Redaction

Records are stored in full: recorders, mergers and converters never mask, and in format 1.0 only an extract writer sets `quality.redacted`.
A consumer that is not privileged receives an `extract` ([STO §2]) written under a **redaction policy**, or the canonical export of such an extract ([FMT §15]).

**Policy.**
A policy has an id, a version and a key id, recorded in `redaction_policy` ([FMT §5]).
It holds **item rules** — each an S/F, one item path pattern and a digest domain; several rules may share an S/F — and **annotation rules**, each an `annotation_kind` and a digest domain.
A domain is a UTF-8 string of 1 to 65535 bytes; a policy with any other domain is rejected before use.
How policies are written, stored and distributed is not part of this specification; the rule set is service configuration.

**Message text.**
For a `data` record, the message text screened here is every captured byte after the 4-byte length and the 10-byte HSMS message header, up to `payload_len`,
whatever the declared length, `max_frame_len` or the stored classification say.
A record whose payload is shorter than 14 bytes has no message text and is never masked.
The text is **one valid item** when it holds exactly one item or list, valid per the item predicate of §3, ending at the last captured byte;
for this purpose a format-22 item whose body is 1 byte long is not valid (E5 §9.4: a non-empty localized string starts with its 2-byte header).
The writer decides validity by decoding the text itself; the stored `decode_status` ([FMT I-11]) is not consulted.

**Paths.**
An item path is a sequence of 1-based element indexes into nested lists, separated by `/`; the empty path is the root item of the message text;
in a pattern, `*` stands for every element of a list.
Each step, numbered or `*`, applies to a list and fails on any other item; a numbered step also fails when the list has fewer elements.
A `*` step expands to every element of the list, and to none when the list is empty.
A pattern **resolves** when no step of any branch of its expansion fails; it then selects the items its branches reach, possibly none.

**Matching.**
An item rule applies to a `data` record whose stream and function, read from the HSMS message header in the payload ([FMT I-10]), equal the rule's;
header copies are never used for matching.
An annotation rule applies to an `annotation` record of that `annotation_kind`.
Control records and transport events are never masked.

**Masking.**
The bytes a match sets to zero are its **target bytes**; a match with no target bytes yields no entry and changes nothing.
- A message text that is one valid item with no content byte in any leaf (e.g. the empty list of a denial reply) is never masked: it holds nothing to hide.
- Otherwise, when the text is one valid item and the pattern of every rule matching its S/F resolves, each rule's target bytes are the content bytes of every leaf item in the subtrees it selects;
  every item header (format byte and length bytes) is kept, and a format-22 leaf also keeps its 2-byte localized string header, which names the encoding.
  The message text keeps its length and stays one valid item.
- **Whole-text mask**: when some item rule matches the S/F and the text is not one valid item, or the pattern of any rule for that S/F does not resolve,
  the target bytes are the whole message text; the length field and the HSMS message header are kept.
  The record then has exactly one `redaction` entry, without `item_path` and in the domain `message-text`, and no entry of any other item rule.
  Its `decode_status` describes the source bytes, not the zeros.
- An annotation rule's target bytes are the value bytes of the record's `text` or `raw` entry; the TLV entry headers are kept ([FMT §8]),
  and zero bytes are valid UTF-8, so a `text` value stays valid.
- Several matches of one record may overlap; each yields its own entry.
- A writer that cannot finish screening a selected record — a resource limit, an internal error — fails the extract; it never writes a selected record unscreened.

**Digest.**
For each entry the writer computes HMAC-SHA-256 (RFC 2104, with SHA-256 of FIPS 180-4) under the policy's key; writers of format 1.0 use no other algorithm.
Its input is the domain's byte length as a little-endian `u16`, the domain's UTF-8 bytes, then the original bytes:
the encoded item (item header included) for a resolved path, the whole original message text for a whole-text mask, the original value bytes for an annotation.
Two digests are comparable only when their algorithm, key id and domain are equal, domains compared byte for byte without normalisation;
equal digests mean equal encoded bytes, so the same value in another item format compares unequal.
A policy gives items that should be comparable one domain.

**Masked record.**
A masked record keeps its identity (`capture_id`, `seq`), every record-header field — `fidelity`, `decode_status`, `trailing_bytes` and the copy fields included — and gains `quality.redacted`.
A value is **masked** when any byte its interpretation depends on is a target byte of an entry.
`redacted` takes precedence over `fidelity`: a masked value is unavailable.
No conclusion may rest on it — neither byte-level (wire-exact analysis, byte identity, malformed-frame analysis) nor semantic:
a zeroed parameter is not an observed zero, and the evidence grades of §2 apply only to values that are not masked.
A consumer compares masked content only by digest; `fidelity` still describes how the source was captured.
A masked record and its source differ in bytes by design;
a reader given both reports a `conflict` ([FMT I-12]), and consumers do not combine an extract with other packs of its capture.
An extract writer that builds the secondary index (F-4, §7.3, deferred in format 1.0) indexes no value of a `redacted` record and marks every id kind `partial` for a block holding one,
so the footer stays derivable from the blocks ([FMT I-3]).

**Marking.**
An extract written under a policy carries `redaction_policy`, even when nothing matched,
and one `redaction` entry per match with target bytes, ordered by seq and then by first masked offset.
An entry's `masked_ranges` list exactly its target bytes — bytes that were already zero included —
as (offset, length) pairs counted from the start of the record payload, sorted, each non-empty, none overlapping or adjacent to another.
A record carries `redacted` exactly when it has at least one entry; the header flag `redaction-present` is set when at least one record carries `redacted`.
A missing required nested tag or a wrong value type rejects the file, as for any TLV ([FMT §5]); every other defect below is recoverable.
At bootstrap a reader checks each entry on its own: a non-empty ranges array of whole pairs, every length positive, the pairs ascending, none overlapping or adjacent,
and the entries in the order above; it also checks that `redaction-present` is set exactly when there is at least one entry.
When it reads a record, it checks the entries naming that record's seq against it:
the record carries `redacted` exactly when some entry names it, and each range lies inside the payload (offset ≤ `payload_len` and length ≤ `payload_len` − offset),
past the 14-byte frame prefix for item and whole-text masks, and inside the value of a `text` or `raw` entry for annotation masks.
A record is **wholly masked** when it carries `redacted` or is named by an entry, and any of these checks fails for it or for an entry naming it:
for a `data` record the whole message text is unavailable; for an annotation, the values of its `text` and `raw` entries,
or its whole payload when its TLV entry boundaries cannot be established; one defective entry discards every entry of that record.
The reader reports each defect; a defect of one record never changes another record, so results already returned stay valid.
An unusable range map is never taken as proof that a byte is unmasked.
An extract without `redaction_policy` was not screened and is as sensitive as the packs it came from.
An extract is written only from records in full:
a writer rejects a source record that carries `redacted`, or a source pack with `redaction_policy`, before it writes any output, because the masks of an earlier extract could not be carried forward.

**Baseline rules** (informative, paths per SEMI E5-0813).
A policy normally covers recipe content (a row with two paths is two rules):

| Messages | Item | Path | Domain |
|---|---|---|---|
| S7F3, S7F6 | PPBODY | `2` | `ppbody` |
| S7F36 | PPBODY | `3` | `ppbody` |
| S7F23, S7F26, S7F31 | formatted process program body (CCODE and PPARM) | `4` | `formatted-pp` |
| S13F6 | FILDAT: data set content, which carries large process programs and recipes (S7F37–F43, S15F49, S15F51) and PDE transfers (S19) | `4/*` | `dataset` |
| S15F13, S15F27 | RCPATTRDATA; RCPBODY | `4/*/2`; `5` | `rcpattr`; `rcpbody` |
| S15F32 | RCPATTRDATA; RCPBODY | `2/*/2`; `3` | `rcpattr`; `rcpbody` |
| S15F15 | recipe sections | `4` | `rcp-sections` |
| S15F18 | recipe sections | `1` | `rcp-sections` |
| S15F28 | RCPATTRDATA | `2/*/2` | `rcpattr` |
| S15F30 | RCPATTRDATA | `4/*/2` | `rcpattr` |
| S6F25 | RCPATTRDATA | `6/*/2` | `rcpattr` |
| S15F25 | RCPPARVAL | `4/*/2` | `rcpparval` |
| S15F33 | RCPPARVAL | `3/*/2/*/2` | `rcpparval` |
| S16F3 | RCPPARVAL | `4/3/*/2` | `rcpparval` |
| S16F11 | RCPPARVAL | `5/3/*/2` | `rcpparval` |
| S16F15 | RCPPARVAL | `2/*/4/3/*/2` | `rcpparval` |
| S16F23 | RCPPARVAL | `2/*/2` | `rcpparval` |

Process program ids, recipe ids, RCPPARNM and RCPSECCODE stay visible: they are names and selectors, and replies echo them.
Denial replies such as the empty list of S7F6, S7F26 or S7F36 hold no content and are not masked.
Candidates to evaluate per site, off by default:

| Messages | Item | Path | Domain |
|---|---|---|---|
| S10F1, S10F3 | TEXT | `2` | `terminal-text` |
| S10F5 | TEXT | `2/*` | `terminal-text` |
| S10F9 | TEXT | the root item | `terminal-text` |
| S14F1, S14F3 | ATTRDATA | `4/*/2` | `attrdata` |
| S14F2, S14F4 | ATTRDATA | `1/*/2/*/2` | `attrdata` |
| S2F41 | CPVAL | `2/*/2` | `cpval` |
| S2F49 | CEPVAL (nested parameter names included) | `4/*/2` | `cpval` |

Annotation rules: `unparsed-entry`, `skipped-bytes` and `unrecognised-line`, whose text may hold any source content, in domain `source-text`.

## 9. Conformance vectors

The following vectors belong to the corpus of [FMT §16]:
- one vector per `decode_status` value and per item-validity rule (§3);
- repeated System Bytes; cross-pack transactions;
- a repeated transaction key: one completed transaction followed by an unanswered primary (§7.2);
- an outstanding primary on epoch E1, a refused socket E2, then the E1 reply (§7.2 closure);
- a nonzero `capture_origin_mono_ns` (§4);
- redaction (§8): the vectors of [FMT §16] "Redaction vectors".
