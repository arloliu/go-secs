# tracepack — record semantics

Status: current (2026-10-25) — v2.37, tracepack format 1.2.
External review consensus reached (review rounds recorded in `tracepack-spec-changelog.md`).
Normative, documented for any reader;
the Go module is the only implementation (G5-199).

References: `[FMT §n]` = `tracepack-format.md`, `[SEM §n]` = `tracepack-semantics.md`, `[STO §n]` = `tracepack-storage.md`, `[OVW §n]` = `tracepack-overview.md`, `[JSONL §n]` = `tracepack-jsonl.md`, `[CORPUS §n]` = `tracepack-corpus.md`; `[FMT I-n]` = invariant I-n of the format document.
Each rule is defined in exactly one document; the others only reference it.

## 1. Depends on

- [FMT §7] record header fields and their classes, and the HSMS header fields read from the payload with their `field_validity` availability ([FMT §7.2]);
  [FMT §8] payload TLV bodies, [FMT §9] enum registries.
- [FMT I-7] epoch binding, [FMT I-11] derived values, [FMT I-12] record identity, [FMT I-13] hour-aligned blocks.
- [FMT §10] footer F-2 / F-3 / F-5 content used by pruning and lookup; [FMT §13] bootstrap, which a reader performs before using that content.
- [FMT §6] full block reads, used by every query of §7.4.
- [FMT §2] portable encoding rules; [FMT §5] pack metadata tags used by capture, time and quality semantics;
  [FMT I-6] immutability; [FMT §16] corpus contract for the vectors of §9, and [CORPUS] for the corpus's files, its item-validity checklist (§3) and its vector catalogue.
- [FMT §5] `redaction_policy` and `redaction` entries, [FMT §4] `redaction-present`, used by §8.
- [STO §2] scopes; [STO §4] the durable-bus contract and its clock anchor, the packs a reader is given, and merge;
  [STO §8] the Go module's implementation tests, beside which §9 lists those of lookup and of reads over several packs.

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
They apply only to values that are not masked (§8): a masked value of a `redacted` record is unavailable for every analysis, and its `fidelity` describes the capture, not the masked bytes.

**Records unobserved by design** (G5-208, G5-224, G5-225, G5-237).
A `data` record whose message text was not captured by design keeps its `payload_len`, its 4-byte length and 10-byte HSMS header as captured, and its direction and time;
its message text is zero-filled, and it carries `quality.unobserved`.
Its body is unavailable, like a masked value (§8): no conclusion rests on it.
It is not classified: its `decode_status` is `not-attempted` and its `trailing_bytes` 0, and its `field_validity` describes the captured header as for any record ([FMT §7.2]).
It has no `redaction` entry and no digest: it is never masked.
Which records a producer may thin is [STO §4] *Unobserved records*: the FDC service's interval polls, identified other than by stream and function,
of a stream and function a `thinning` entry of its pack declares ([FMT §5]); the entry says where the sampling comes from, never a rule.
A record whose message text is genuinely empty (`payload_len` 14) is not unobserved, and is kept.
A reader of format 1.0 preserves the bit without knowing it ([FMT §14]), so a consumer that ignores both `decode_status` and the bit reads the zeros as traffic.

**Reconstruction contract** (`capture_method = log`):
the writer builds each message from its source text,
then sets SessionID / DeviceID, System Bytes and the W-bit from the source's per-entry metadata, never from defaults:
an identity the metadata does not carry keeps whatever placeholder bytes encoding the frame requires,
and its `field_validity` bit stays clear, so it is never available to a query ([FMT §7.2], [STO §7]).
A W-bit on an even (reply) function is invalid (SEMI E5 §7.2: replies never request a reply);
such an entry is stored as an annotation with `decode_status = build-rejected`, never as a silently repaired message.

## 3. decode_status (decision table)

`decode_status` is derived ([FMT I-11]): it records what a classifier the pack names in `classifier` concluded about the payload (for a `redacted` record, the source payload, §8).
Predicates are defined on the captured bytes, per SEMI E37 §8 and E5 §9.

| Status | Predicate (evaluated in this order) | Malformed? |
|---|---|---|
| `not-attempted` | the writer did not classify | n/a |
| `short-frame` | captured bytes < 14 (length + header) | yes |
| `length-mismatch` | length field ≠ captured length − 4 | yes |
| `bad-ptype` | PType ≠ 0 (E37 §8.2.6; 0 = SECS-II) | yes |
| `bad-stype` | SType not defined by E37 Table 5 (defined: 0–7, 9) | yes |
| `control-with-body` | SType ≠ 0 and length field ≠ 10 | yes |
| `oversized` | frame length (the captured bytes, the 4-byte length field included) exceeds the `max_frame_len` in force where the record was classified | yes |
| `item-decode-error` | SType = 0, message text non-empty, and it does not begin with one complete, valid SECS-II item or list per E5 §9.2–9.3 (defined format code, 1–3 length bytes, body length consistent with the format's element size, every nested list element present) | yes |
| `ok-with-trailing` | item valid and message text continues after it; `trailing_bytes` = the excess | quirk, not malformed |
| `ok` | item valid with no excess, or empty message text (header-only data message), or a well-formed control message | no |
| `reconstructed-ok` | `log` writer parsed and built the message | no |
| `parse-failed` | `log` writer could not parse the entry; stored as an `unparsed-entry` annotation | n/a |
| `build-rejected` | `log` writer parsed the entry but the message is invalid (e.g. W on an even function, §2) | n/a |
| `not-applicable` | transport-event and annotation records not covered by the rows above | n/a |

A pack whose records were classified under several `max_frame_len` values carries all of them and does not say which one a record was classified under ([FMT §5]);
so a conformance vector whose records are checked against `oversized` carries exactly one `max_frame_len` ([CORPUS §7]).
A format-22 item (a localized string, E5 §9.4) is valid with a length of 0, or of 2 or more whatever the parity of that length,
and invalid with a length of 1, because a non-empty localized string starts with its 2-byte header;
no check depends on the encoding that header names (G5-181).
The item-validity checklist, one case per rule of the `item-decode-error` predicate, is [CORPUS §7];
E5 §9 is its normative base.
Decoders differ in strictness on edge cases;
because the bytes and the `classifier` are both stored, a consumer can always reclassify, except over masked ranges (§8) and the message text of a record unobserved by design (§2).

**Applicability by kind and capture method**, before the table is consulted:
control records reach `ok` when the frame parses (there is no SECS-II item);
`log` writers use only `reconstructed-ok`, `parse-failed` and `build-rejected`;
other transport-event and annotation records use `not-applicable`;
a record carrying `quality.unobserved` is `not-attempted` (§2).
**Malformed predicate**: `decode_status ∈ {short-frame, length-mismatch, bad-ptype, bad-stype, control-with-body, oversized, item-decode-error}`.
No quality bit mirrors the predicate; readers offer the decode state of §6 instead.
`fidelity` is independent of `decode_status`.

## 4. Time

- `ts_utc_ns`: for `time_source = capture-clock`, the wall clock at the vantage point when the **complete frame** was observed.
  A frame the vantage sends itself is observed complete when it is handed to the socket, not when the send returns,
  because the peer can read and answer it before the send returns.
  A first-byte time, if ever wanted, is an additional field in a minor version, never a reinterpretation.
  For `time_source = source-log`, the source timestamp parsed in `source_tz` and converted to UTC.
- `mono_ns`: capture-clock only; elapsed monotonic ns since `capture_origin_mono_ns`,
  whose wall-clock counterpart is `capture_origin_utc_ns` (both identical across all packs of one capture).
  Latency between two records of one capture is the `mono_ns` difference, immune to wall-clock steps.
  A recorder restart starts a new capture id, so mono values are never compared across captures.
  Only differences of monotonic readings carry meaning, so any `capture_origin_mono_ns` value is valid, 0 included:
  a producer without a raw monotonic reading writes 0 and measures `mono_ns` from its origin instant (G5-151).
- Source-log records: `mono_present` clear;
  DST-ambiguous or nonexistent local times are resolved to the earlier instant and flagged `ordering-uncertain`.
- Clock steps: a capture-clock writer keeps a **clock anchor** (wall, mono):
  (`capture_origin_utc_ns`, 0) until the first `clock-step` event, then (`ts_utc_ns`, `mono_ns`) of the latest `clock-step` event;
  anchor mono values are capture-relative, like `mono_ns`.
  The anchor moves only at a `clock-step` event, never per record, so tolerated drift cannot accumulate.
  For every new record the component that observes both clocks — the writer, or for a capture carried over a durable bus ([STO §4]) the producer — computes the drift `(wall − anchor_wall) − (mono − anchor_mono)`;
  when |drift| exceeds `clock_step_tolerance_ns`, a writer closes the current block, writes a `clock-step` transport event (`clock_step_ns` = the drift),
  and takes it as the new anchor before appending the record;
  a producer over a durable bus, which manages no blocks, has the `clock-step` acknowledged first:
  it publishes it and waits for the bus's acknowledgement before publishing records under the new anchor.
  Hence every record satisfies |`ts_utc_ns` − (anchor_wall + `mono_ns` − anchor_mono)| ≤ `clock_step_tolerance_ns` for the anchor in force when it was written.
  A consumer's close of a capture its producer left open ([STO §4] *Closing a capture left open*) is the exception:
  it has the `ts_utc_ns` that section gives it and no monotonic time, no producer clock-step detection applies to it, and the inequality above does not hold for it.
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
| state-transition | a state-change notification from the HSMS implementation (previous state, current state, cause), or, for an accepted-then-refused socket ([FMT I-7]), the implementation's socket-refusal notification carrying the socket's identity, which sources the `not-connected` to `not-connected` record with cause `select-rejected` | **epoch attribution**: a notification that does not itself identify its originating socket or connection generation may arrive late from an earlier connection, so the writer's *current* socket cannot be assumed to be its origin. A nonzero epoch is written only when the writer has independently established the originating socket (e.g. exactly one socket has ever existed in this capture, or the notification carries a generation id); otherwise the record gets `epoch = 0` and `correlation-incomplete`, keeping the transition and its real cause. The same rule applies to notification-derived timer records | one record per delivered notification, stored as a transition with its real cause; `lifecycle_coverage` says whether notifications were subscribed, and absence of a transition record is never proof that none occurred (notifications may be dropped or coalesced) |
| timer-expiry T3 | the HSMS implementation's per-transaction reply-timeout outcome | the primary's session, S/F and System Bytes ([FMT §8] tags), from the writer's context when the outcome report lacks them | one per timed-out transaction |
| timer-expiry T6 / T7 | state-change notifications whose cause is a T6 or T7 timeout | an implementation may aggregate repeated linktest failures into one `linktest-failure` cause rather than one T6 per probe; epoch attribution as for state-transition | one per notification |
| timer-expiry T5 / T8 | only when a `raw-stream` observer can attribute them | — | never synthesised |
| linktest / select / deselect / separate / reject | the control frames themselves (kind=control), plus the resulting state-transition | — | control frames are **not** duplicated as events |
| socket-accept / socket-connect / socket-close | **socket observation only**. A state-change cause never establishes a socket event: "local close" means teardown was initiated, and a transport-error cause may cover several socket outcomes | `socket_role` | one per observed socket event; a writer without socket observation writes none, or, if it derives one, sets `inferred` and uses the notification timestamp. When both sources exist, the observed record is the socket event and the notification stays a transition; nothing is emitted twice |
| clock-step | the comparison of wall and monotonic clocks by the component that observes them, the writer or the producer that feeds it (§4) | `clock_step_ns` | one per detected step, written before the next record; over a durable bus, acknowledged by the bus before the next record is published ([STO §4]) |
| capture-boundary | writer start / stop / gap; a consumer's close of a capture its producer left open (`stop-unclean`, [STO §4]); a `stop-unclean` of earlier spec versions, written by spool recovery, is read the same way | `boundary_kind`; for a gap, its seq range and `gap_start` / `gap_end` (including packet loss in a network capture and recorder downtime between linked captures); a `start`, `stop` or `stop-unclean` boundary carries no seq range, since its record's own seq places it (G5-149) | one per boundary |

Causes the source does not name are stored as `unknown` with the source's own name in `cause_raw`,
never mapped to a plausible value.

## 6. Stored and effective quality

Stored `quality` bits record only what the writer knew **when it wrote the record**:
`epoch = 0` or missing source metadata (`correlation-incomplete`), clock or DST problems (`ordering-uncertain`),
`direction-inferred`, `redacted`, `unobserved`, declared by the producer (§2), and `capture-boundary` on the boundary record itself.
Every stored bit is declared by the writer; none mirrors another field.
Clear stored bits mean checked absence of those conditions only when the pack metadata declares `quality_evaluated = true`.
A writer never revisits a record already written, so stored bits never depend on later records ([FMT I-6], [FMT I-12]).
`redacted` is set by a writer that masks under a policy (§8): an extract writer, or a consumer of a durable bus ([STO §4]).

**Effective** quality is computed by readers at query time from stored bits plus context:
- a primary whose lookup meets the epoch-boundary condition or reads an `ordering-uncertain` record of its epoch, both as §7.2 defines them, is `correlation-incomplete`,
  and its lookup is `incomplete`;
  the lookup finds both only in the records it reads (G5-130);
- a record inside a `coverage` range of a pack read is `incomplete`;
- a record from a pack with `quality_evaluated = false` never yields clean evidence.

F-5 `boundary` entries ([FMT §10]) let a reader, after the [FMT §13] bootstrap, find the crash gaps a read reports without reading blocks (§7.4);
a transaction lookup bounds its window only by records it reads (§7.2, G5-135).

**Derived predicates.**
Readers offer two predicates computed from other fields, in place of quality bits:
- a record's **decode state** is *malformed* when its `decode_status` is in the malformed set of §3,
  *clean* when it is a known value outside that set,
  and *unknown* for a value the reader does not know ([FMT §1]), which never counts as clean;
- a record has **no monotonic time** iff `record_flags.mono_present` is clear.

A consumer that needs clean evidence requires a clean decode state; `not malformed` is not enough.
F-3 and F-5 `quality_union` cover the stored bits only.
A block may be excluded as holding no malformed record only when its `decode_status_counts` are zero for every malformed value
and for every element beyond the values the reader knows.

## 7. Indexes and lookup

### 7.1 Per-block summary (footer F-3)

Defined ([FMT §10]): counts per kind, direction and `decode_status`, and `quality_union`.
Time, epoch and seq ranges are in F-2.
No summary of HSMS header fields (S/F, SessionID, System Bytes) is defined or planned ([OVW §6]);
predicates on them are evaluated on payloads (§7.4).
**Contract**: a summary may produce false positives and must never exclude a matching record;
an absent tag or an absent summary means "may match".
Exclusion by `decode_status_counts` follows §6.

### 7.2 Transaction identity and lookup

Transaction key = (`capture_id`, `epoch`, initiating direction, SessionID, System Bytes, primary `seq`),
SessionID and System Bytes read from the primary's payload ([FMT §7.2]).
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
candidate association, the eligibility window and the protocol-valid match use the record header's seq, epoch and direction
and the payloads' SessionID, System Bytes and S/F, each available only when its `field_validity` bit is set ([FMT §7.2]);
observed runtime outcomes need payloads: the T3 `timer-expiry` primary identifiers are transport-event TLV fields ([FMT §8]).

**Unavailable key fields.**
A field is unavailable when its `field_validity` bit is clear or the payload lacks its bytes ([FMT §7.2]);
an unavailable field's bytes are never compared.
A primary whose SessionID or System Bytes is unavailable has no transaction key; a lookup from it is `incomplete`.
A record that could be the reply because a field is unavailable — a possible reply, an undecidable candidate, or a candidate at or after a possible same-key primary, in the window, as defined below —
makes the result `incomplete`, and the record is reported with the anomalies.
A record whose available fields already differ from the primary's (another System Bytes, say) cannot be the reply, whatever else is unavailable, and changes nothing.
`matched` and `ambiguous` are decided on the candidates whose fields are available.

Results (G5-204): `matched` (exactly one valid match), `ambiguous` (several), `not-found` (none);
any of them carries an `incomplete` flag, with its reasons, when a condition of *`incomplete` reasons* (below) holds.
`not-found` means no valid match among the records the lookup read;
it states nothing about records outside the packs it was given.

**Lookup from a primary** (G5-121, G5-122, G5-124..G5-127, G5-129..G5-132, G5-134..G5-136, G5-146, G5-204).
A lookup names its primary by (`capture_id`, `seq`, UTC hour): the hour of the primary's `ts_utc_ns`, which is its scope ([STO §2], [FMT I-13]) (G5-124).
It reads the primary and derives the key from it (G5-122), so a caller never supplies a key that disagrees with the record.
It reads the packs its caller gives for that hour and for the following hours, as many hours as the caller allows (Hours read, below),
and decides with the definitions and rules below.
`p` is the primary's seq.
Each hour's read takes every pack given for that hour, of any capture (Hours read),
but the lookup decides on the primary's capture only:
its primary, the roles and epochs this section names, its window, its gaps, the versions it keeps and the conflicts that make it `incomplete` are of that capture,
and a seq this section compares across reads is compared as (`capture_id`, `seq`) ([FMT I-12]).
Records of other captures take no part in these;
the diagnostics this section defines for every pack read or for any capture — read defects, scope breaches, packs with `quality_evaluated` false, and crash gaps (G5-209) — stay as defined.

*Definitions.*
- **Primary** (G5-125): a `data` record whose function is available and odd.
  A lookup that names any other record — of another kind, with an even function, or absent from its hour's read where nothing in that read explains it (Hours read) — fails as an error, not as a result.
  Its **association key** is (`epoch`, direction, SessionID, System Bytes); the direction is `host-to-equipment` or `equipment-to-host`, and SessionID and System Bytes are available.
  A primary whose direction is neither, or whose SessionID or System Bytes is unavailable, has no key: the lookup is `incomplete`.
  Its **match fields** are stream (which may be unavailable) and function.
  The W bit takes no part; a lookup reports it.
- **Candidate**: a `data` record of the primary's capture and epoch with a seq above `p`, in the opposite direction, SessionID and System Bytes available and equal to the primary's.
  It is **decidable** when its stream and function are available and the primary's stream is available.
- **Possible reply**: a `data` record of the primary's capture and epoch with a seq above `p`, not a candidate, but one that could be:
  its direction opposite or `unknown`, each of its SessionID and System Bytes unavailable or equal to the primary's, and at least one of the three unknown or unavailable.
- **Same-key primary**: a `data` record with a seq above `p`, of the primary's capture, epoch and direction, function available and odd, SessionID and System Bytes available and equal to the primary's (G5-125).
- **Possible same-key primary**: a `data` record with a seq above `p` of the primary's capture and epoch, not a same-key primary, but one that could be:
  its direction the primary's or `unknown`, its function unavailable or odd, each of its SessionID and System Bytes unavailable or equal, and at least one of these unknown or unavailable.
  A record in `unknown` direction can be both a possible reply and a possible same-key primary.
- **Closing record** (G5-135): a record the lookup read, with a seq above `p` and without a conflict, that ends the primary's epoch or the capture:
  a `transport-event` of the primary's epoch with `event` = `socket-close` ([FMT §10] "ends an epoch"),
  or a capture-boundary record with `boundary_kind` = `stop`, of the primary's capture and any of its epochs (it ends the capture, and also the primary's epoch when it is in it).
  Only a record the lookup read is a closing record:
  a footer's `close_seq` and `boundary` entries never bound the window, because footer validation proves their structure, not that the record they name is a closing event ([FMT §10]).
  The existence of a later epoch ends nothing, because accepted-then-refused sockets get their own epochs while an earlier connection stays open ([FMT I-7]).
- **Window** (`p`, `e`), open on both sides: `e` is the smallest seq of a same-key primary or a closing record the lookup read; the window is unbounded while it read none.
- **Candidate flags**:
  a candidate is **eligible** when its seq lies in the window and below the first possible same-key primary in the window;
  it is a **valid match** when it is eligible and decidable, has the primary's stream, and its function is the primary's function + 1, or 0.
  An eligible candidate that is not decidable, and a candidate in the window at or after a possible same-key primary, could each be the reply: like a possible reply in the window, it makes the result `incomplete`.
- **Outcome records**, reported when in the window, never a match:
  a `control` record with SType available and 7 (`Reject.req`) in the opposite direction, of the primary's epoch, with SessionID and System Bytes available and equal to the primary's (SEMI E37 §8.3.20: both are the rejected message's);
  a `transport-event` of the primary's epoch whose payload decodes with `event` = `timer-expiry`, `timer` = T3, and `primary_session_id` and `primary_system_bytes` present and equal to the primary's,
  with `primary_stream` and `primary_function`, each where present on the event and available on the primary, equal to the primary's.

*Identity across the hours read.*
Within one hour's read, copies of each (`capture_id`, `seq`) are compared as §7.4 says.
Where packs keep to their scopes, two copies of one record in different hours are never identical, since identical copies share `ts_utc_ns` and so an hour (a pack that does not keep to its scope is a scope breach, below);
whatever the reason, a (`capture_id`, `seq`) of the primary's capture yielded by the reads of two hours is a conflict;
the same seq of two different captures is two records, never a conflict.
**Any conflict on a seq of the primary's capture at or above `p` among the records read makes the result `incomplete`** (G5-126, G5-132),
whatever role its versions play and wherever it lies, inside the window or after it, within one hour's read or across hours:
the versions disagree in their bytes or in the hours that yielded them, so neither the window nor the candidates are established.
A conflict at `p` leaves the primary, and so the key, unestablished;
found in a later hour's read, it leaves the key derived from the first version, and what it classified, reported as diagnostics beside the conflict.
A conflict across hours is reported by its seq and the hours whose reads yielded it.
The lookup reports the versions it kept of the records that play a role defined above,
each with the roles and candidate flags its own bytes give, so the versions of a conflicting candidate can differ in them.
A record whose block disagrees with its F-2 entry can arrive out of seq order and escape comparison (§7.4),
so such a block in an hour read also makes the result `incomplete`, even when a match was found (G5-134);
so does any other disagreement an hour's read detects, between a block it read and its F-3 summary or between another footer value and the records it describes (§7.4).
The lookup then guarantees only that it is `incomplete` with that defect:
its outcome, its other reasons, the versions it reports and every other fact are best-effort (G5-203).
A record whose `ts_utc_ns` lies outside the hour of the scope it was read from breaches that scope ([FMT I-13], [STO §2]): it is reported as a **scope breach**, with its seq and hour, and makes the result `incomplete`, even when a match was found;
in the primary's hour it also leaves a missing primary explained, as a read defect does (G5-146).
**The comparison covers the hours the lookup read** (G5-136), as §7.4's guarantees cover the packs read:
a version of a window seq that lies only in an hour the lookup did not read — after the last hour it read, or before the primary's hour after a backward clock step (§4) — is not compared.
Two versions of one record are a pipeline defect, found only by an operation that compares both:
`verify` checks one pack, and a merge compares the packs of one scope ([STO §4]), so neither is certain to compare versions that lie in different hours.
A seq of the primary's capture in a bounded window with no version of that capture in the hours read is a gap, which makes the result `incomplete`;
a record of another capture never fills it.

*Hours read* (G5-121, G5-129).
The lookup reads the primary's hour and the following hours, as many as the caller allows: these are the **hours scheduled**,
and their time range runs from the start of the primary's hour to the end of the last hour scheduled.
An hour's read is one read over several packs (§7.4), in capture order, of the packs the caller gives for that hour.
The lookup reads each hour once, every one of them even after its window is closed:
a later hour can hold another version of a window seq or a smaller bound.
Each hour's read selects every record, with no filter,
so it sees the read defects and scope breaches of its packs, the `ordering-uncertain` records of the primary's epoch at any seq, and every version of `p`;
the conflicts it lists include those below `p` and those of other captures (§7.4), which do not make the result `incomplete`.
`seq > p` restricts which records are candidates, possible replies, same-key primaries or possible same-key primaries, never what is read.
A window still unbounded after the last hour read makes the result `incomplete`; a match found is still reported as one (G5-127).

A primary that is missing, conflicting within its own hour's read, or without a key ends the lookup after that one hour, since its result is then decided.
Such an **early return** is `not-found` and `incomplete`, has no window, and keeps only the versions of the primary.
It reports:
- the primary's failure:
  a **missing primary**, when the read of its hour yielded no version of `p` and that read has a read defect other than a `coverage` entry or a crash gap, a scope breach,
  or a pack whose `coverage` entry of the primary's capture (its `capture_id`, else the capture of the pack holding it) meets `p` and the primary's hour ([FMT §5]), which explains the missing primary and is not reported on its own;
  a crash gap of the primary's capture that meets the primary's hour explains a missing primary the same way a read defect of that hour does, and is reported as the crash gap below;
  a **conflicting primary**, its versions in conflict within that read, reported as the conflict at `p`, with the key derived from the first version as a diagnostic;
  a **primary without a key**, with its `correlation-incomplete` when it carries it, and its fields as a diagnostic;
- what the read of the primary's hour finds before the primary is established, at any seq:
  its read defects other than `coverage` entries and crash gaps (§7.4), a block or footer value it finds disagreeing with its records included, its scope breaches, and its packs with `quality_evaluated` false;
- the crash gaps that meet the primary's hour, each evaluated once, against that hour alone (`incomplete` reasons, below).
It reports no other reason, since it does not evaluate a window:
no conflict above `p`, `coverage` entry, crash gap outside the primary's hour, epoch-boundary condition, `ordering-uncertain` record, seq gap, unbounded window or record that could be the reply.
A primary whose versions conflict only across hours is not an early return:
the lookup goes on through every hour scheduled, with the key derived from the first version,
and reports the conflict at `p` with the primary's hour and each later hour whose read yielded `p`.
A primary that is missing where nothing in its hour's read explains it, or a record that is not a primary (Definitions), fails the lookup as an error, not as a result.

*`incomplete` reasons* (G5-203, G5-204, G5-209) — each reported, side by side, with the hours the lookup read:
- every incomplete-read cause of the packs read, as §7.4 reports it — a failed block, an unknown codec, a truncation, a reader limit, a block or footer value found disagreeing with its records —
  except `coverage` entries and crash gaps, which the lookup evaluates once, against its own bounds, by the items below;
- a `coverage` entry of a pack read whose capture — its `capture_id`, else the capture of the pack holding it — is the primary's capture, and that meets by [FMT §5] both the seqs of the window,
  (`p`, `e`), or (`p`, ∞) while it is unbounded, and the time range of the hours scheduled;
- a scope breach; a conflict on a seq of the primary's capture at or above `p`;
- a pack read with `quality_evaluated` false (§6);
- a seq of the primary's capture in a bounded window that no hour's read yielded (a gap);
- a window still unbounded after the last hour read;
- the **epoch-boundary condition**:
  a capture-boundary record of the primary's epoch, read with a seq above `p`, other than the clean `stop` that bounds the window, a `stop-unclean` included;
- a **crash gap**:
  a `stop-unclean` boundary of any capture, from the sources §7.4 names (G5-209), whose gap meets the time range of the hours scheduled by §7.4's rule.
  It is distinct from the epoch-boundary condition.
  The time range it is evaluated against is that of the hours scheduled, never the unbounded selection of the hours' reads;
  an early return evaluates it against the primary's hour only;
- an `ordering-uncertain` record of the primary's epoch read, at any seq (§6, G5-130); `correlation-incomplete` on the primary;
- a possible reply, an undecidable eligible candidate, or a candidate at or after a possible same-key primary, in the window;
- a primary without a key.

`ordering-uncertain` is detected only in the hours read: no index records it per epoch, so a lookup that reads part of a long epoch can miss one (G5-130).
An I/O error, a cancellation or a limit that ends the lookup is an error, not a result.

*Result* (G5-126, G5-127, G5-204):
one valid match → `matched`, several → `ambiguous`, none → `not-found`;
the `incomplete` flag and its reasons stand beside any of them, none hiding another (§7.4).
When a block read disagrees with its F-2 entry, or the lookup detects another footer value disagreeing with its records, only `incomplete` and that defect are guaranteed (Identity across the hours read).

### 7.3 Secondary index

Format 1.0 defines no secondary index: F-4 is an absent, undefined section ([FMT §10]).
Well-known ids (CEID, ALID, RPTID, SVID, ECID, RCMD) are found by decoding and scanning payloads (§7.4 (c)).
A later index would use the F-4 slot, per block and self-contained ([FMT I-14]).

### 7.4 Query mapping

| Query | Mechanism |
|---|---|
| (a) tool + time | the packs the caller gives for the scopes whose hour overlaps (the service's catalog, or a listing of the key prefixes, [STO §4]) → overlap clusters across those packs, excluded whole by F-2 / F-3 (Evaluation) → range reads of the blocks of the other clusters → scan |
| (b) + S/F | scan; the S/F of each data record is read from its payload ([FMT §7.2]) |
| (c) CEID / ALID / … | decode-and-scan |
| (d) System Bytes → pair | scan payloads for the System Bytes to find a primary; then §7.2's lookup from it over the packs given for its hour and the following hours |
| (e) transport events | kind counts exclude clusters (Evaluation); scan; control frames are records, so Linktest / Select queries read kind=control |
| (f) SML full text, (g) cross-tool | scan |

**Evaluation.**
Every block a query reads is read in full ([FMT §6]); only F-2 and the F-3 summaries (§7.1) exclude a block without reading it.
A predicate on an HSMS header field — SessionID, S/F, W, PType, SType or System Bytes — is evaluated on the payload's value,
and the field is available only when its stored `field_validity` bit is set ([FMT §7.2]).

A read over several packs, such as the packs given for the scopes a query selects, resolves record identity ([FMT I-12]) before it filters:
- **Selection.** Scopes are chosen by capture and hour, and every pack the caller gives for a chosen scope is read;
  no pack of that set is left out for its own time range or summaries,
  since a pack outside the query's time range can hold another version of a record inside it.
- **Overlap clusters.** The blocks of one capture whose seq ranges overlap, across every pack read, form an overlap cluster, as for a merge ([STO §4]).
  A block without an F-2 entry, in a pack that is not finalized or whose footer is not valid, is read once beforehand, and released,
  to take from its records what its index would state: its seq range, time range, epoch range and kind and direction counts (G5-117).
  That **computed summary** stands in for the missing F-2 entry and F-3 summary in clustering, exclusion and both orders.
  A block that fails that read is reported and takes no further part.
- **Exclusion.** F-2 and the F-3 summaries, or the computed summaries, exclude a cluster only when they exclude every block of it (G5-118).
  A cluster excluded whole is not read (a block with a computed summary is not read a second time), and a conflict inside it is not reported.
  A cluster's `ts_min` is the smallest `ts_min` of its blocks.
- **Comparison.** In every other cluster each block is read,
  and the copies of each (`capture_id`, `seq`) are compared byte for byte, the record header as stored (all `record_header_len` bytes) and the payload,
  before any filter discards one; copies with different header lengths differ ([STO §4]).
  So byte identity and `conflict` are decided for every record of a compared cluster.
- **Conflicts.** Identical copies are one record, yielded once.
  Each distinct version, the one version of a record without a conflict included, has a **representative**:
  the first block holding it in cluster order (ascending first seq, then the order in which the packs were given, then file order).
  The record is yielded from its representative, which the read names as the record's pack and block,
  and the representative's `ts_min` schedules it in capture order.
  Differing copies are a `conflict`: each version the filter selects is yielded once, marked as a conflict,
  and the read lists every conflict of every cluster it compared, whether or not the filter selected a version (G5-119).
  A reader that bounds that list fails the read when one more conflict is found; it never omits one from a successful read.
  An extract and a pack of its own capture conflict on every masked record (§8).
- **Scope of the guarantees.** They cover the packs read: a copy in a pack the read was not given is neither read nor compared.
  The read trusts a valid footer's F-2 ranges and F-3 summaries ([FMT §10]),
  so the guarantees hold, for the packs read as a whole, only when every block read agrees with its F-2 entry
  and every block excluded agrees with its F-2 entry and F-3 summary;
  a false F-3 count can exclude a cluster whose records the filter would select, and they are then missing unreported.
  A block whose records lie outside its F-2 seq range can place a seq in the wrong cluster,
  so a copy of that seq in another pack, even a truthful one, is not compared and can be yielded twice, unmarked;
  a record before its block's F-2 `ts_min` can be yielded out of time order.
  A read checks every block it reads against its F-2 entry, and MAY check it against its F-3 summary;
  it is not required to recompute any other footer value.
  When a read detects a disagreement — a block it read against its F-2 entry or its F-3 summary, or another footer value against the records it describes —
  it reports `incomplete` with that defect (Reporting of a read, below),
  and the read then guarantees only that:
  its other reasons, its conflicts, its order and every other fact it reports are best-effort (G5-203).
  Every other defect — a failed block, an unknown codec, a truncated tail, a block over a reader limit — keeps the guarantees of [FMT §13]:
  validated blocks are read, and failed or unread ranges are reported (Reporting of a read, below).
  A misstated block that is never read is not detected, so the absence of such a report does not certify the index.
  A merge refuses such a pack ([STO §4]), and `verify` finds it ([FMT §13]).

A read of a single pack holds no two copies of a seq (I-12), so it excludes blocks one by one.

**Order of a read over several packs.**
- **Capture order**: each capture's records in ascending `seq`.
  Captures are interleaved block by block (G5-116):
  the next record yielded is the next record the filter selects from the capture whose next selected record has the representative with the smallest `ts_min`,
  ties broken by `capture_id`; `ts_min` is the block's F-2 value or its computed summary's.
  A record the filter rejects never delays its capture, so a filter can change how captures interleave, never the order within one.
  The selected versions of a conflicting record are yielded in version order, each scheduled by its own representative,
  before the capture's next seq; records of other captures can come between them.
- **Time order**: by (`ts_utc_ns`, `capture_id`, `seq`), then version.
  A pack's records are in write order, not time order (§4), so the read takes the clusters it does not exclude
  in ascending (`ts_min`, `capture_id`, first seq), and before it reads each one,
  yields every selected record already read whose `ts_utc_ns` is strictly below that cluster's `ts_min`;
  it reads each cluster whole before yielding any record of it,
  and once no cluster is left, yields every remaining record.
  An excluded cluster takes no part in this.
- The **version order** of a conflicting record is the order of the first block holding each version,
  blocks taken in cluster order: ascending first seq, then the order in which the packs were given, then file order.
- **Conflicts are listed in discovery order**, independent of the order records are yielded:
  in time order, clusters in the order they are read, seqs ascending within each;
  in capture order, the captures are started in ascending `capture_id`, each up to its first selected record, then in the order the read advances them.
  The same packs, given in the same order with unchanged bytes, with the same query, order and limits, list the same conflicts in the same order,
  and a conflict limit ends the read on the same conflict.
  Giving the packs in another order keeps which records conflict and which versions they have,
  but may change the representatives, the order of the packs holding each version, and so the interleaving of captures.

**Reporting of a read** (G5-203, G5-209).
A read reports `incomplete` beside the records it yields, never instead of them, for every cause below that it finds in the packs it reads ([FMT §13]):

| Reason | Reported when | Located by |
|---|---|---|
| `coverage` | for every `coverage` entry of a pack's metadata whose capture is the capture of a pack the read reads, and that meets the query by [FMT §5]'s matching rule (G5-215) | the entry and the pack holding it |
| `truncated` | at most once per pack, the first case that applies: (1) the forward walk stopped at an envelope it could not account for; (2) else the pack is not finalized; (3) else the walked blocks disagree with the trailer's `block_count` or `record_count`, as [FMT §13] compares them | the offset: (1) of that envelope; (2) of the end of the last walked block, or of the end of the pack metadata without blocks; (3) none |
| `corrupt-block`, `unknown-codec` | for every block the read reads that fails ([FMT §13]), by its cause | the block and its envelope's offset |
| `index-mismatch` | for every block the read reads whose records disagree with its F-2 entry ([FMT §10]), and for every other disagreement it detects between a block it read and its F-3 summary or between another footer value and the records it describes (Scope of the guarantees) | the block and its envelope's offset; for a footer value of no single block, the pack |
| `limit` | for every block, or the rest of a pack's walk, that the read leaves unread because of a reader limit | the block and its envelope's offset, or where the walk stopped |
| `crash-gap` | for every `stop-unclean` boundary whose gap meets the query's time range (Crash gaps, below) | the boundary's capture and seq |

A block whose records disagree only with its F-3 summary reports `index-mismatch` when the read checks that summary, and nothing otherwise:
a reader need not check F-3 summaries or recompute footer values, so the absence of the report does not certify them.
A finalized pack whose footer is rejected, but whose forward walk accounts for every block and agrees with the trailer's totals, reports nothing:
the footer's rejection alone does not make the read `incomplete`.
Whether a reader skips a block that F-2 or the F-3 summaries exclude is not part of this contract,
so a failed or disagreeing block that the query excludes may go unreported (Scope of the guarantees).
A reader's limits are its own parameters, not properties of the format:
which blocks reach them is not part of this contract, and a limit, an I/O error or a cancellation that ends the read is an error, not a result.

**Crash gaps** (G5-209).
A read takes the `stop-unclean` boundaries from the F-5 `boundary` entries of every pack it is given, of any capture, read at the [FMT §13] bootstrap without decoding a block,
and from the capture-boundary records it decodes in packs without a valid footer;
so pruning never hides a boundary of a pack with a valid footer.
A boundary's gap is the interval [`gap_start`, `gap_end`] its entry or record carries ([STO §4] *Closing a capture left open*; a `stop-unclean` of earlier spec versions carries the bounds spool recovery gave it),
unbounded on a side whose bound is absent.
It meets a time range by the matching rule of `coverage` ([FMT §5]):
an interval whose `gap_start` exceeds its `gap_end` meets every time range, as an inverted `coverage` entry does (G5-137),
and a query without a time range meets every gap.
A boundary that several of the packs given hold is reported once.
Deduplicating it never discards a differing gap:
boundaries whose gap bounds differ are each evaluated.
A footer's `boundary` entry can only add `incomplete`; it never hides a record.

`incomplete`, `conflict` and the transaction outcomes of §7.2 are reported side by side; none replaces or hides another.

## 8. Redaction

Records are stored in full, or masked when written under a policy:
in format 1.0 only an extract writer masks;
from format 1.1 a consumer of a durable bus masks the records of a capture under the capture's policy and keeps each original encrypted in place (Encryption, below; [STO §4]).
Recorders other than such a consumer, mergers and converters never mask; a merger carries the masks of its inputs ([STO §4] Merge).
A consumer that is not privileged receives an `extract` ([STO §2]) written under a **redaction policy**, or the canonical export of such an extract ([FMT §15]).

**Policy.**
A policy has an id, a version, a digest key id and, from format 1.1, an encryption algorithm and an encryption key id (Encryption), recorded in `redaction_policy` ([FMT §5]).
It holds **item rules** — each an S/F, one item path pattern and a digest domain; several rules may share an S/F — and **annotation rules**, each an `annotation_kind` and a digest domain.
An item rule's stream is at most 127, bits 0–6 of payload byte 6 ([FMT §7.2]); a policy with a larger one is rejected before use (G5-262).
A domain is a UTF-8 string of 1 to 65535 bytes; a policy with any other domain is rejected before use.
A policy used with encryption is rejected before use when two of its item rules for one S/F can select overlapping items, which a comparison of their path patterns decides (G5-236):
identical patterns, a pattern that is a prefix of another, and patterns whose `*` and numbered steps can select one item all overlap;
the whole-text fallback at run time is no way to accept such a policy.
How policies are written, stored and distributed is not part of this specification; the rule set is service configuration.

**Message text.**
For a `data` record, the message text screened here is every captured byte after the 4-byte length and the 10-byte HSMS message header, up to `payload_len`,
whatever the declared length, `max_frame_len` or the stored classification say.
A record whose payload is shorter than 14 bytes has no message text and is never masked.
The text is **one valid item** when it holds exactly one item or list, valid per the item predicate of §3 (a format-22 item of length 1 is not), ending at the last captured byte.
The writer decides validity by decoding the text itself; the stored `decode_status` ([FMT I-11]) is not consulted.

**Paths.**
An item path is a sequence of 1-based element indexes into nested lists, separated by `/`; the empty path is the root item of the message text;
in a pattern, `*` stands for every element of a list.
Each step, numbered or `*`, applies to a list and fails on any other item; a numbered step also fails when the list has fewer elements.
A `*` step expands to every element of the list, and to none when the list is empty.
A pattern **resolves** when no step of any branch of its expansion fails; it then selects the items its branches reach, possibly none.

**Matching.**
An item rule applies to a `data` record whose stream and function, read from payload bytes 6 and 7 ([FMT §7.2]), equal the rule's,
whatever `field_validity` says: like the screened text, matching never relies on a stored field.
An annotation rule applies to an `annotation` record of that `annotation_kind`.
Control records and transport events are never masked.

**Masking.**
The bytes a match masks are its **target bytes**:
under a policy without encryption they are set to zero; under a policy with encryption they are replaced by their ciphertext (Encryption).
A match with no target bytes yields no entry and changes nothing.
- A message text that is one valid item with no content byte in any leaf (e.g. the empty list of a denial reply) is never masked: it holds nothing to hide.
- Otherwise, when the text is one valid item and the pattern of every rule matching its S/F resolves, each rule's target bytes are the content bytes of every leaf item in the subtrees it selects;
  every item header (format byte and length bytes) is kept, and a format-22 leaf also keeps its 2-byte localized string header, which names the encoding.
  Under either policy the message text keeps its length and stays one valid item, since no item header is a target byte.
- **Whole-text mask**: when some item rule matches the S/F and the text is not one valid item, or the pattern of any rule for that S/F does not resolve,
  the target bytes are the whole message text; the length field and the HSMS message header are kept.
  The record then has exactly one `redaction` entry, without `item_path` and in the domain `message-text`, and no entry of any other item rule, under either policy.
  Its `decode_status` describes the source bytes, not the masked bytes.
- An annotation rule's target bytes are the value bytes of the record's `text` or `raw` entry; the TLV entry headers are kept ([FMT §8]),
  and zero bytes are valid UTF-8, so a `text` value stays valid.
  Under a policy with encryption an annotation rule still sets its target bytes to zero and keeps no original, since a `text` value cannot hold ciphertext (G5-238).
- Several matches of one record may overlap, except under a policy with encryption (Policy); each yields its own entry.
- A writer that cannot finish screening a selected record — a resource limit, an internal error — fails the extract,
  or, for a consumer of a durable bus, defers the record ([STO §4] *Masking*); it never writes a selected record unscreened.
  A record to be screened that already carries `redacted` is not in full and cannot be screened (G5-262).

**Digest.**
For each entry the writer computes HMAC-SHA-256 (RFC 2104, with SHA-256 of FIPS 180-4) under the policy's key; writers of format 1.0 use no other algorithm.
Its input is the domain's byte length as a little-endian `u16`, the domain's UTF-8 bytes, then the original bytes:
the encoded item (item header included) for a resolved path, the whole original message text for a whole-text mask, the original value bytes for an annotation.
Two digests are comparable only when their algorithm, key id and domain are equal, domains compared byte for byte without normalisation;
equal digests mean equal encoded bytes, so the same value in another item format compares unequal.
A policy gives items that should be comparable one domain.
The digest's input is the original encoded item, item header included, for a resolved path, while the plaintext encrypted for the same entry is its target bytes alone (Encryption).

**Encryption** (G5-220, G5-221, G5-228..G5-230, G5-234, G5-243).
From format 1.1 a policy may carry an encryption algorithm and an encryption key id ([FMT §5]); a pack masked at write always does ([STO §4] *Masking*).
- Algorithm: AES-SIV-CMAC of RFC 5297 with a 64-byte key (`aes-siv-cmac-512`), under the capture's encryption key.
- Plaintext of an entry: its target bytes, concatenated in the order of its `masked_ranges`, which list exactly the target bytes, bytes already zero included.
- Associated data: one byte string, 53 bytes long, given to S2V as its single associated-data component:
  the label `tracepack-original-v1`, its 21 ASCII bytes `74 72 61 63 65 70 61 63 6b 2d 6f 72 69 67 69 6e 61 6c 2d 76 31`, with no terminator, padding or length prefix;
  then the record's `capture_id`, 16 bytes in [FMT §2] order; its `seq`, a little-endian `u64`; and the offset of the entry's first masked range, a little-endian `u64`.
- Output: the 16-byte synthetic IV, stored as the entry's `siv`, followed by a ciphertext of the plaintext's length, written over the target bytes in the same range order.
- Structure: a resolved item mask leaves every item header in place, so the message text keeps its length and stays one valid item;
  a whole-text mask's ciphertext is opaque: the record keeps its length field and HSMS header, and its message text need not parse.
- Restore: the holder of the key decrypts each entry with its `siv`, the record's identity and the entry's first offset, and writes the plaintext back into the ranges.
  Restore is per record and all-or-nothing:
  when any entry of the record fails — an authentication failure, a missing or ill-sized `siv`, a range map that fails the checks of Marking, cross-entry disjointness included —
  no byte of the record is restored, the record stays masked, and the defect is reported (G5-243).
- Equal originals in two records, or in two entries of one record, encrypt differently, while a redelivered record encrypts identically (G5-214);
  equal originals under one `key_id` and domain keep equal digests, across captures.
- AES-SIV takes two passes, so an original is encrypted and restored whole, in memory; the format sets no length limit for its sake.

**Masked record.**
A masked record keeps its identity (`capture_id`, `seq`), every record-header field — `fidelity`, `decode_status` and `trailing_bytes` included — and gains `quality.redacted`.
A value is **masked** when any byte its interpretation depends on is a target byte of an entry.
`redacted` takes precedence over `fidelity`: a masked value is unavailable.
No conclusion may rest on it — neither byte-level (wire-exact analysis, byte identity, malformed-frame analysis) nor semantic:
a masked parameter, zeroed or encrypted, is not an observed value, and the evidence grades of §2 apply only to values that are not masked.
A consumer compares masked content only by digest, and only a holder of the key restores it (Encryption); `fidelity` still describes how the source was captured.
A masked record and its source differ in bytes by design;
a reader given both reports a `conflict` ([FMT I-12]), and consumers do not combine an extract with other packs of its capture.
A capture masked at write has no unmasked copy in storage ([STO §4] *Masking*), so its packs never conflict with a source.

**Marking.**
A pack written under a policy carries `redaction_policy`, even when nothing matched,
and one `redaction` entry per match with target bytes, ordered by seq and then by first masked offset.
An entry's `masked_ranges` list exactly its target bytes — bytes that were already zero included —
as (offset, length) pairs counted from the start of the record payload, sorted, each non-empty, none overlapping or adjacent to another.
A record carries `redacted` exactly when it has at least one entry; the header flag `redaction-present` is set when at least one record carries `redacted`.
A missing required nested tag or a wrong value type rejects the file, as for any TLV ([FMT §5]); every other defect below is recoverable.
At bootstrap a reader checks each entry on its own: a non-empty ranges array of whole pairs, every length positive, the pairs ascending, none overlapping or adjacent,
and the entries in the order above; it also checks that `redaction-present` is set exactly when there is at least one entry.
An entry that fails a check of its own is defective, and so are both entries of each pair of consecutive entries out of order;
a `redaction-present` flag that disagrees is a defect of the pack, which masks no record by itself (G5-260).
When it reads a record, it checks the entries naming that record's seq against it:
the record carries `redacted` exactly when some entry names it, and each range lies inside the payload (offset ≤ `payload_len` and length ≤ `payload_len` − offset),
past the 14-byte frame prefix for item and whole-text masks, and inside the value of a `text` or `raw` entry for annotation masks.
A record is **wholly masked** when it carries `redacted` or is named by an entry, and any of these checks fails for it or for an entry naming it:
for a `data` record the whole message text is unavailable; for an annotation, the values of its `text` and `raw` entries,
or its whole payload when its TLV entry boundaries cannot be established; one defective entry discards every entry of that record.
The reader reports each defect; a defect of one record never changes another record, so results already returned stay valid.
An unusable range map is never taken as proof that a byte is unmasked.
Two recoverable checks concern encryption (G5-236, G5-238, G5-243).
The target ranges of the entries of one record that carry `siv` are pairwise disjoint, adjacent ranges of two entries allowed;
a record whose encrypted entries overlap is wholly masked, and none of its originals is restored; entries without `siv`, zero masks as format 1.0 extracts hold, may still overlap.
Under a policy with encryption every entry of a data record carries `siv`, and an annotation's entry none:
a present `siv` whose length is not 16 is found at bootstrap, with the other entry checks;
a data record's entry without `siv`, and an annotation's entry with one, are found when the record is read, its kind then known;
either way the record stays masked and its original cannot be restored.
An extract without `redaction_policy` was not screened and is as sensitive as the packs it came from.
An extract is written only from records in full:
a writer rejects a source record that carries `redacted`, or a source pack with `redaction_policy`, before it writes any output, because the masks of an earlier extract could not be carried forward;
so an extract is never written from a pack masked at write, whose records are not in full (G5-244).

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

The following vectors belong to the corpus of [FMT §16]; [CORPUS §9.2] maps each of them to the corpus's vectors or marks it as planned:
- one vector per `decode_status` value and per item-validity rule (§3);
- a nonzero `capture_origin_mono_ns` (§4);
- redaction (§8): the vectors of [FMT §16] "Redaction vectors".

Transaction lookup (§7.2) and reads over several packs (§7.4) are tested by the Go module's own tests, as the cases of [STO §8] are;
they are not corpus vectors (G5-199, G5-210).
Those tests cover:
- repeated System Bytes; cross-pack transactions;
- a repeated transaction key: one completed transaction followed by an unanswered primary (§7.2);
- an outstanding primary on epoch E1, a refused socket E2, then the E1 reply (§7.2 closing record);
- a primary without System Bytes, and an otherwise unanswered primary followed in its window by a reply-direction record whose System Bytes are unavailable: both `incomplete` (§7.2);
- lookups from a primary (§7.2):
  a reply in the next hour; a window bounded by a same-key primary, by a socket-close of the epoch, by a clean `stop` of another epoch, and not within the hours read;
  a footer whose `close_seq` names an annotation, with its F-2 entry true (never a bound);
  a pack of hour H whose truthful blocks lie in hour K, holding the primary and its reply (a scope breach: incomplete), and a same-hour clock step that puts records outside a segment's period (no breach);
  a valid match, a wrong stream, an F0 abort, a wrong-stream F0, a control record whose bytes 6–7 look like F + 1, two valid matches (`ambiguous`);
  a possible same-key primary with System Bytes unavailable, a reply before it (valid) and after it (not eligible);
  an unknown-direction record that is both a possible reply and a possible same-key primary;
  a conflict within an hour's read on a candidate and on an unrelated record after the window, and across hours on the primary and on a candidate: each `incomplete`;
  a block whose records disagree with its F-2 entry: `incomplete` with that defect, every other fact unchecked (G5-203);
  another version of a window seq in the hour before the primary's, after a backward clock step: outside the comparison, and the same seq with no version in the hours read: a gap;
  a `coverage` entry whose seq range meets the window and whose time interval does not meet the hours scheduled, and the reverse;
  a `stop-unclean` boundary of another capture, from a footer, whose gap meets the hours scheduled, one whose gap does not, and one with an inverted gap;
  the same boundary decoded from its record in a pack without a valid footer;
  each early return: a missing primary explained by a read defect, and one nothing explains (an error); a conflicting primary;
  a primary without a key, beside a crash gap meeting its hour and one meeting only a later hour;
  a primary whose versions conflict only across hours (not an early return);
  an `ordering-uncertain` record of the epoch below the primary;
  a capture-boundary record of the epoch above the primary, other than the clean `stop` that bounds the window, and one outside the hours read (not found);
  a pack with `quality_evaluated` false; a `Reject.req` with SType unavailable; a T3 `timer-expiry` without identifiers;
- reads over several packs (§7.4):
  two captures interleaved block by block in capture order, with a filter that rejects one capture's next record;
  backward timestamps within a block, across the blocks of a pack and across packs, read in time order;
  equal timestamps across captures, across clusters and across the versions of one record (two and three versions, yielded in version order);
  a record held identically by two packs, yielded once;
  a conflict of two versions with both, one and no version selected by the filter (the conflict listed in each case);
  copies differing only in a reserved `record_flags` bit, only in the record header's extension area, in `record_header_len`, and only in the payload with payloads not requested: each a `conflict`;
  two packs holding versions of one seq with different timestamps, and a query whose time range selects only one (both read, the conflict listed);
  a cluster with one block the summaries exclude and one they do not (both read), and a cluster excluded whole (not read, no conflict listed);
  a cluster spanning two hours;
  a conflict limit reached exactly (the read succeeds) and exceeded by one (an error, with the conflicts found so far);
  a walked block whose computed summary excludes its cluster (read once only);
  a pack that is not finalized beside its archive, in both orders;
  a query whose filter excludes the block holding a `stop-unclean` boundary whose gap meets the query's time range: `incomplete` from the footer's `boundary` entry;
  an extract beside its source (each masked record a `conflict`), deferred with extracts (G5-198, G5-244).
