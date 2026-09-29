# Proposal P9 — format revision before phase 4: columnar record headers, block coalescing, retirements

Status: draft (2026-09-29) — applies owner decisions G5-91..G5-98; review round 6: ready to apply.
Source: the weight and small-message overhead review of 2026-09-29 (kept outside the repository)
and the owner discussion of the same day.

## 1. Scope and how it closes

The review asked two questions:
whether tracepack is heavier than a log service and the Virtual Equipment (VE) need,
and whether its per-record fields cost too much for small HSMS / SECS-II messages.
Its measurements are in §2.

This proposal revises the format:
a byte-columnar header section (§3.1), block coalescing in merges (§3.2), two retirements (§3.3),
a small redundancy cleanup (§3.4), the phase 4 scope (§3.6),
a record header without copy fields (§3.7), and a review of the remaining fields with every read made full (§3.8).
The owner decided it on 2026-09-29 (G5-91..G5-98).
It lands as spec v2.13 and as the format-revision step of the implementation plan, before phase 4.

The storage question the review raised — a view without commit objects — was part of this proposal until review round 1
and is now proposal P10 (G5-95).

P9 closes when it is applied or rejected:
the outcome goes to the changelog and the decision log, and this file is deleted.

## 2. Evidence

Measured with the tracepack Writer (zstd default level, block validation on) on synthetic traffic:
*idle* = Linktest every 30 s, S1F3/S1F4 every 5 s, S1F1/S1F2 every 60 s (30.7 wire bytes per message);
*busy* = idle plus S6F11 every 1–3 s, an S6F1 trace every 1 s and S2F41 every 5 min (77.7 wire bytes per message).
The benchmark code is outside the repository (`tpkbench`, kept with the review);
the format-revision step adds an in-repository benchmark that reproduces the columnar comparison on the implemented writer and reader.

Bytes per record, one block per pack:

| Traffic | Pack | tracepack | zstd(ts + dir + frame) | zstd(SML text) | Wire |
|---|---|---|---|---|---|
| idle | 5 min | 29.1 | 10.0 | 11.8 | 30.7 |
| idle | 1 h | 21.6 | 9.5 | 11.9 | 30.7 |
| busy | 1 h | 55.3 | 44.6 | 61.8 | 77.7 |

- The record header costs about 17 bytes per record after compression, whatever the message size;
  a timestamp and a direction alone cost about 5.7.
- Of the rest, `mono_ns` costs about 7 bytes per record, `seq` about 2 and the copy fields 1.5–3.
- Transposing the header section into byte columns, with every field kept,
  lowers the header cost from 17.2 to 9.6 bytes per record (idle) and from 16.7 to 9.2 (busy).
  A higher zstd level does not:
  row layout idle 21.4 → 19.8 bytes per record at the best level, columnar about 14.5 at every level.
- Per pack about 1.1 KiB is fixed (the pack metadata is 661 bytes with realistic tags):
  negligible in an hourly archive, about 25 % of an idle 5-minute segment.
- Closing a block every second multiplies the idle cost by 4.8 and the busy cost by 2.3;
  the bus consumers of G5-86 close blocks at the size threshold,
  so this matters only to a writer that closes a block per sync, and to merges that keep such blocks (§3.2).
- At G5-43's scale (about 1500 tools × 10 messages per second) and G5-57's six-month retention,
  the header costs about 22 GB per day, 4 TB per retention period.

CPU of a byte-columnar header section, single-threaded,
on one 4 MiB block of busy traffic (31,505 records, 1,764,280-byte header section):

| Operation | Row layout | Columnar |
|---|---|---|
| compressed block | 1,816,820 B | 1,532,637 B (−15.6 %) |
| encode (transpose + zstd) | 14.8 ms | 11.8 ms |
| full decode | 2.57 ms | 3.31 ms (untranspose 0.66 ms) |
| header-section decode + S6F11 filter (the header-only read that §3.8 removes; kept for comparison) | 0.73 ms | 0.83 ms scanning two columns; 1.5 ms after untransposing |

Bytes per record of a columnar block body by record-header content (one hour, timestamps with nanosecond noise):

| Record header | idle | busy |
|---|---|---|
| 1.0 today (56 B) | 14.7 | 49.3 |
| without copy fields (§3.7, the decided content) | 13.8 | 47.8 |
| without copy fields, `mono_ns` stored as a residual (§3.8) | 11.2 | 45.6 |
| without copy fields and `mono_ns` (§3.8) | 9.2 | 43.8 |
| payloads only | 3.8 | 38.5 |

`trailing_bytes`, `fidelity` and `decode_status` each measured under 0.1 bytes per record;
removing them is about rules, not bytes.

## 3. Format

### 3.1 Byte-columnar header section (G5-92)

The header section of a decoded block body stores the record headers transposed:
with `n = record_count` and `L = record_header_len`,
byte `j` of record `i`'s header is at offset `j × n + i` of the header section, for 0 ≤ `j` < `L` and 0 ≤ `i` < `n`.
The section is still `n × L` bytes and still precedes the payload section,
so the payload offsets, I-2's length equation and the footer are unchanged.

Proposed wording for [FMT §6],
replacing "The header section holds the `record_count` record headers in record order, each `record_header_len` bytes":

> The header section holds the `record_count` record headers **column by column**:
> for byte `j` of the record header layout (§7.1) and record `i`, the byte is at offset `j × record_count + i`.
> A record's header is the `record_header_len` bytes gathered this way;
> every rule that names a record header, a header field or a header byte refers to these gathered bytes.

Consequences, each to be stated where it applies:

- [FMT §7.1] offsets are positions in the gathered header, not in the section.
- A `record_header_len` above the known length (44 after §3.8) transposes all `L` bytes;
  unknown bytes are byte columns like any other and are preserved
  (I-12 byte identity is over the gathered bytes and the payload).
- I-2's seq and length checks run on the gathered headers, after the whole body is decoded (§3.8: every read is full).
- A block of one record is laid out identically in both layouts.
- Merges copy blocks verbatim as before; record-level resolution and coalescing gather and re-transpose.
- The footer, the envelope and every F-3 statistic are unchanged.
- [OVW §4] Figure 2 shows the transposed section.
- A single record outside a block — the bus message body of proposal P8 §2.1 —
  is the gathered header, the same bytes as the row form.

**Version (owner decision).** Format 1.0 is redefined in place; no format number changes.
tracepack v0.1.0 wrote the row layout, and its packs are **unsupported** under the redefined layout.
Deployment precondition: no pack written by tracepack v0.1.0 is stored where a reader of the redefined format reads;
the owner confirmed none exists outside tests.
The reader cannot tell the layouts apart by version, so it does not promise to detect an old pack:
a block of one record is laid out identically and reads correctly;
a larger block usually fails I-2, because its seqs are gathered from mixed bytes, and is then reported `corrupt`,
but that detection is not guaranteed.
The tracepack changelog states this,
and [FMT §14] records the redefinition as its one exception to "changing an existing offset is a major change":
made once, in spec v2.13, before any production use.
The corpus is identified by spec version as well as by format version,
so the revised goldens are told apart from any earlier ones.
The corpus gains one vector, a known v0.1.0 pack with a multi-record block that the new reader reports `corrupt`,
labelled as a sample of the layout before v2.13, not as a detection guarantee.

**Implementation weight.**
The writer transposes once when it assembles a block body (one loop over `n × L` bytes).
The reader untransposes the decoded header section into a reusable row buffer,
after which every existing record-level path is unchanged —
the I-2 walk, header access, payload offsets, unknown header bytes.
The validating writer ([FMT §12]) checks the encoded bytes through the same path:
it decodes the encoded body, untransposes its header section, then runs the I-2 checks;
checking the buffer before the transpose would not check what is written.
Golden block bodies and the reader's per-offset corruption expectations are regenerated.

**Pros:** header cost −40 % with no field removed and no semantic change; encoding 20 % faster;
any language implements it with a nested loop (`reshape(L, n).T` in NumPy).

**Cons:** decoding costs 0.66 ms more per 4 MiB block
(full decode +29 %);
a reader holds one more buffer of the header section's size;
a record's header is no longer contiguous in the decoded body, which makes hex dumps harder to read;
the zero-copy struct view noted by proposal P1 is gone (no implementation uses it).

### 3.2 Block coalescing in merges (G5-93)

[STO §4] Merge,
"Adjacent small blocks with equal `record_header_len` MAY be decoded and re-encoded into one block", becomes:

> Coalescing is the one exception to copying non-overlapping blocks verbatim.
> A merger SHOULD coalesce small blocks.
> Taking the blocks it outputs in ascending seq order, it groups them greedily:
> a group grows while the next block has the same `record_header_len`
> and the group's decoded bodies, that block's included, stay within the merger's block size threshold (4 MiB decoded by default);
> a block whose decoded body alone reaches the threshold, such as a block holding one oversized record, is a group of its own.
> A group of one block is copied verbatim;
> every larger group is encoded as one block, its records byte-identical and in their order.
> All blocks of a scope lie in one UTC hour ([FMT I-13]), so no group crosses an hour.
> Each new encoding is validated in memory before it is written;
> a failing one is discarded and its group's blocks are copied verbatim, as for every new encoding.
> The merger's validation rules above apply unchanged.

The rationale sentence names the cost of §2:
small blocks multiply the per-record cost, and the archive is what retention keeps.
[FMT §6] keeps "A writer closes a block at a size threshold or on flush";
a writer's durability sync is a flush only for writers that must make records durable in the pack itself
(the local spool recorder, deferred by G5-91).

### 3.3 Retirements (G5-94)

- **Correction patches.**
  `pack_role` value 5 (`correction`) is retired.
  A writer never writes it; the Go `Writer` rejects it.
  A reader preserves and reports it as `unknown(5)`, like any unknown enum value ([FMT §1]).
  [STO §4] and [STO §5] gain a role rule:
  only a pack whose `pack_role` is `segment`, `archive` or `repair` takes part in a scope's view, is a merge input, is registered by the catalog,
  or becomes deletable under [STO §4] Deletion.
  A pack with any other role (an `extract` never enters the tiers) is excluded from all four,
  removed only by Retention with its hour, and reported to the operator.
  [FMT I-11]'s last clause ("a correction pack MAY re-emit records with a newer classifier's values") is removed:
  a consumer that wants a newer classification recomputes it at read time, as I-11 already allows.
  Every other mention of corrections is removed, or narrowed to repairs where the sentence is about patches:
  [FMT] I-6, I-12, §5 (`coverage` and `pack_role` rows), §9;
  [SEM] §1 dependency list, §6 last paragraph;
  [STO] §1 dependency list, §2 roles table and patch sentence, §6 heading and patch text;
  [OVW] §3 terminology table, §6 deferred table.
  What a repair may re-emit is unchanged here (proposal P10 addresses it).
- **Multi-member replacement sets.**
  A merge writes one archive per scope, whatever its size ([FMT §2] limits apply per block, not per pack);
  [STO §4]'s 256 MiB split is removed.
  The replacement-set tags stay, with `replacement_set_size` = 1 and `replacement_set_index` = 0 wherever they appear;
  pack metadata with any other value is invalid and rejects the file ([FMT §5]).
  Proposal P10 may retire the tags altogether.
- **Vectors.** [STO §8] vectors that use a correction to exercise a patch use a repair instead;
  vectors whose subject is a correction as such, or a set of more than one member, are removed
  (the correction of a repaired archive, the size-capped set, members disagreeing on `publisher_epoch`, members differing in attestation);
  the two-member generation vector becomes a one-member generation with a repaired member.

### 3.4 Redundancy cleanup (G5-96)

| Item | Decision | Reason |
|---|---|---|
| `schema_version` (tag 0x0001) | retire | frozen at 1 and checked together with `format_major` ([FMT §14]); the format version alone says the same |
| F-3 `content_bytes` (0x0006) | retire | must equal F-2 `uncompressed_len` ([FMT §10]); F-5 `content_bytes` aggregates F-2 instead |
| `time_source` | keep | not implied by `capture_method`: a TCP stream reassembled from a packet capture is `raw-stream` with the capturing machine's timestamps and no mono |
| derived quality bits (`no-mono`, `decode-failed`, `capture-boundary`) | decided in §3.8 | §3.8 keeps `capture-boundary` and retires `no-mono` and `decode-failed` |
| trailer `block_count`, `record_count`, `last_seq`; F-1 `block_count` | keep | 20 bytes per file; checked before the footer is trusted |

Contract for each retired tag:

- **`schema_version`.**
  Writers never write it.
  The number 0x0001 is reserved, as 0x0014 is, and never reused.
  A reader that finds it treats it as an unknown tag: skipped, preserved in the canonical export ([FMT §5], §15).
  [FMT §13] bootstrap validates "the file header and format version" before any block, instead of "the file header, format version and `schema_version`";
  [FMT §14] drops the `schema_version` bullet and keeps "an unknown major is rejected before any block is read".
  [STO §4] drops it from the values a bus consumer owns in a segment's pack metadata.
  The Go metadata decoder stops requiring and checking it, and the encoder stops writing it.
- **F-3 `content_bytes`.**
  Writers never write it; 0x0006 is reserved in the F-3 registry.
  A reader treats it as an unknown F-3 tag.
  [FMT §10] footer validation drops "F-3 `content_bytes` = `uncompressed_len`";
  the aggregation rule computes F-5 `content_bytes` as the sum of the blocks' F-2 `uncompressed_len`,
  and its closing sentence becomes "A statistic that is not an aggregate of per-block F-2 or F-3 values MUST NOT be added to F-5".
  A merge still copies each block's F-3 list verbatim, retired and unknown tags included:
  a retired tag in a copied list is data the reader skips, not a tag the merger writes.
  A merger that builds a new F-3 list (a coalesced or resolved block) never includes a retired tag.
- **Retired metadata tags in preserved entries.**
  A writer never writes a retired pack-metadata tag number (0x0001, 0x0014), including one held among preserved unknown entries;
  the Go encoder drops such numbers from `PackMeta.Unknown` when it writes,
  while the canonical export of the pack they were read from still shows them ([FMT §15]).

### 3.5 Considered and not proposed

- `mono_ns` stored as a residual against (`ts_utc_ns` − `capture_origin_utc_ns`):
  about 3.5 bytes per record more, estimated with an assumed drift model,
  at the cost of a semantic transform every reader applies.
  Revisit with real traffic after columnar headers.
- A shorter TLV entry header: the pack metadata is about 1 KiB per pack, negligible in archives.
- A different zstd level: does not close the gap (§2).

### 3.6 Phase 4 scope (G5-91, G5-95)

Phase 4 starts with `Verify`.
`Repair` waits for the outcome of proposal P10, because a repair pack's metadata (`patch_base`) depends on it;
the writer-side requiredness of `coverage`'s nested tags (left open by G5-89) is settled with it.
`Recover`, the liveness anchor and the spool durability helpers wait until a local-spool recorder is planned:
G5-86 replaced the spool contract for the bus deployment the pilot uses ([STO §4]),
so nothing in the pilot writes a spool.

### 3.7 Record header without copy fields (G5-97)

The access patterns that set the bar (owner, 2026-09-29):
records are read on demand to troubleshoot one tool or a few tools of one type,
and the VE reads a tool type's packs for offline iteration —
it pools a few tools of one model and firmware range (G1-6, G1-9),
reads every record of its window, and filters in memory or through its Evidence Index.
No consumer scans the fleet.
Under these patterns the copy fields' one benefit, filtering by S/F, System Bytes or session without decoding the payload section, has no consumer.
The I/O is the same, because a header-only read fetches the whole block body to check `body_crc`;
the difference is the CPU of decoding the payload section, and a full decode of one busy tool's four-week window takes about a second.

Changes:

- The record header no longer holds `session_id`, `stream`, `function`, `ptype`, `stype`, `system_bytes` or `record_flags.W`.
  The payload of a data or control record is still the whole captured frame ([FMT §8]), so nothing is lost:
  a predicate on an HSMS header field reads the field from the payload —
  `session_id` from bytes 4–5, `stream` and W from byte 6 (bits 0–6 and bit 7), `function` from byte 7, `ptype` and `stype` from bytes 8 and 9, System Bytes from bytes 10–13 —
  and only when the field's `field_validity` bit is set.
- `field_validity` stays, with the same bits; it says which HSMS header fields of the payload are real.
  For a raw capture a writer sets a bit iff the payload holds that field's bytes.
  For a log conversion the converter clears the bits of identities its source did not carry,
  although the reconstructed frame holds placeholder bytes there ([STO §7] item 4);
  this is why the field survives, and a writer never sets a bit merely because bytes are present.
  Transport-event and annotation records carry a TLV payload and always have `field_validity` 0.
  Availability follows the bit and the value comes from the payload, as G5-90 decided:
  a clear bit cannot match even where placeholder bytes would.
  A set bit whose bytes the payload lacks is a writer defect.
  Every writer checks it when a record is appended, whether or not it validates encoded blocks, and rejects the record;
  a reader treats the field as unavailable, and `Verify` reports the defect.
  A bus consumer that fetches a claim-checked payload (P8) checks after the fetch.
- I-10 is removed: nothing is copied, so nothing can disagree.
- The query modes go ([SEM §7.4], G5-51): no provisional or authoritative mode, no `header-validated` status;
  every predicate on an HSMS header field is evaluated on payload values.
  [SEM §7.2] transaction lookup takes S/F, session and System Bytes from payloads;
  its candidate steps no longer claim to use record-header fields only.
- `blocks_validated` (0x0030) is retired, superseding G5-52, and the header-only read level goes with it (§3.8).
- Unaffected: redaction `masked_ranges` are offsets from the start of the payload, which is unchanged,
  and redaction already matches S/F on payload values;
  normalised SECS-I records are stored in HSMS framing ([FMT §8]), so the same payload positions apply, DeviceID in bytes 4–5;
  the transport-event `primary_*` tags stay.
- The copy fields leave the list of bytes a consumer derives (P10 §3 item 4).
  The bus body of P8 §2.1 carries the new record header;
  which of its remaining bytes the producer supplies (`field_validity`, `decode_status`) is settled in P8 and P10, not here.
- Go API: `Record` loses the copy fields;
  a helper returns each HSMS header field from the payload as a value and an availability, separately,
  and reports unavailable for short frames and for kinds without an HSMS frame.

### 3.8 Field review (G5-98)

Every remaining field was judged by the same access patterns.
The owner asked an external reviewer for an opinion on every retirement before deciding (the opinion is kept outside the repository);
the decisions below follow it where it disagreed.

| Field | Bytes per record | Decision | Reason |
|---|---|---|---|
| header-only read level and `blocks_validated` | — | **removed**: every block read is a full read (I-2) | attestation also gave header-only reads the guarantees of a full read (codec completion, decoded length); without it a header-only result could count records a full read rejects. Under the access patterns a header-only read saves only CPU, and F-3 / F-5 already give per-block counts without reading a block |
| `mono_ns`, `record_flags.mono_present` | 4.6 / 4.0 (idle / busy) | **kept** | step-resistant latency within a capture for the VE, and the clock anchor rule of [SEM §4]. A residual encoding (11.2 / 45.6) and dropping it (9.2 / 43.8) were considered |
| `quality.no-mono` | 0 | **retired** | a mirror of `record_flags.mono_present`, which stays |
| `quality.decode-failed` | 0 | **retired** | a mirror of a malformed `decode_status` |
| `quality.capture-boundary` | 0 | **kept** | `kind` and `decode_status` cannot tell a boundary event from other transport events; the VE's candidate rule reads it per record, and footer `boundary` entries are block summaries |
| `trailing_bytes` | < 0.1 | **kept** | the exact excess length cannot be recomputed from a masked extract, whose `decode_status` and `trailing_bytes` describe the source ([SEM §8]) |
| F-3 `max_payload_len` | < 0.1 (per block) | **retired** | no consumer in SEM, STO or tracepack-go.md |
| F-4 secondary index, [SEM §7.3], `extraction_version` | — | **extraction design removed; slot kept** | the extraction rule set has no consumer. F-4 stays an absent, undefined section: F-1 keeps `f4_offset` = `f5_offset`, `f4_len` = 0, `extraction_version` = 0 and flag bit 1 clear, so the footer layout and its validation do not change, and a later index can still use the slot |
| F-3 range 0x0010–0x001F (S/F presence, System Bytes locator) | — | stays reserved; no longer planned ([OVW §6]) | — |
| `fidelity` | < 0.1 | kept | a decoded-message recorder that also keeps the raw bytes of undecodable frames writes them `wire-exact` ([SEM §2]) |
| `decode_status` | < 0.1 | kept | capture-time classification survives masking; who computes it for a bus capture is P10's premise item 3 |
| `field_validity`, `time_source`, `quality_evaluated`, `writer_start_utc_ns`, trailer counts | — | kept | — |

**Derived predicates.**
A record's decode state has three values:
*malformed* when its `decode_status` is in the malformed set of [SEM §3],
*clean* when it is a known value outside that set,
and *unknown* for a value the reader does not know ([FMT §1]), which never counts as clean.
A record has no monotonic time iff `record_flags.mono_present` is clear.
Readers offer both as predicates in place of the retired bits.
The VE's candidate rule becomes: none of `capture-boundary`, `ordering-uncertain`, `correlation-incomplete`, and a clean decode state.
F-3 and F-5 `quality_union` covers the stored bits only.
A block may be excluded as holding no malformed record only when its `decode_status_counts` are zero for every malformed value
and for every element beyond the values the reader knows.

**Retired numbers.**
Pack-metadata tag 0x0030 (`blocks_validated`) joins 0x0001 and 0x0014, and F-3 tag 0x0004 (`max_payload_len`) joins 0x0006:
a writer never writes them, a reader treats them as unknown tags, and copied F-3 lists keep them as data (§3.4).
A pack whose F-4 is present, nonzero and undefined is still read: its F-4 bytes are checked for placement as today and otherwise ignored,
and a merger writes F-4 absent.

**Every read is full.**
[FMT §6]'s block read levels go: a reader decodes the whole block body and checks I-2 before it uses any record of the block.
A writer SHOULD validate each encoded block before writing it ([FMT §12], without a metadata commitment),
and a merger validates every new encoding in memory as before ([STO §4]).
The header section stays in front of the payload section, which the columnar layout of §3.1 needs for compression.
Proposal P10's deletion witness, which required attestation, needs its own verification.
Go API, removed: `ReadLevel` with `ReadHeaderOnly` and `ReadAttested`, `QueryMode` and `Query.Mode`, `PackHeader.Attested`, `Item.Level`, `Item.CopyMismatch`, `Result.HeaderValidated`,
`PackMeta.BlocksValidated`, `ErrSchemaVersion`, `QualityDecodeFailed`, `QualityNoMono`, `RecordFlagsW`, `PackRoleCorrection` with its `correction` name (value 5 then prints as `unknown(5)`), and `Record.SetHeaderCopies`,
which becomes a helper that computes the captured-field `field_validity` of a raw frame for writers of raw captures.
Kept with changed behavior: `Query.Payloads` decides only whether payloads are returned;
`WriterOptions.Validate` validates each encoded block without committing anything to the pack metadata;
`DecodeStatus.Malformed` stays true only for known malformed values, and a new `DecodeStatus.Clean` is true only for known values outside the malformed set;
an unknown value is neither, so `!Malformed()` never proves a clean decode.
In tracepack-go.md, `FindTransaction` loses its mode argument and validation status, `Merge` loses its `Attest` option and report,
and the CLI loses `--mode` and the validation status.

**Record header layout, 44 bytes.**
Retired bits keep their numbers, reserved: `quality` bits 3 (`decode-failed`) and 6 (`no-mono`), `record_flags` bit 0 (W).

| Off | Type | Field |
|---|---|---|
| 0 | u64 | `seq` |
| 8 | i64 | `ts_utc_ns` |
| 16 | i64 | `mono_ns` |
| 24 | u32 | `epoch` |
| 28 | u32 | `payload_len` |
| 32 | u32 | `trailing_bytes` |
| 36 | u16 | `quality` |
| 38 | u8 | `kind` |
| 39 | u8 | `dir` |
| 40 | u8 | `fidelity` |
| 41 | u8 | `decode_status` |
| 42 | u8 | `field_validity` |
| 43 | u8 | `record_flags` (bit 1 `mono_present`) |

The minimum `record_header_len` becomes 44 in [FMT §6], the envelope and F-2 offset 76;
[FMT §7.1]'s extension area, which readers preserve as unknown bytes, starts at byte 44.

## 4. Spec changes

| Document | Changes |
|---|---|
| [FMT] | §3 I-6, I-11, I-12 correction wording; §5 `schema_version` retired, `coverage` and `pack_role` rows, replacement-set values fixed to 1 and 0, retired numbers never written from preserved entries; §6 columnar header section; §7.1 offset wording; §9 `pack_role` value 5 retired; §10 F-3 `content_bytes` retired, footer validation, F-5 aggregation of F-2 or F-3 values, copied F-3 lists; §12 validation of the transposed encoding; §13 bootstrap wording; §14 `schema_version` bullet and the one-time redefinition; §16 framing vectors regenerated, corpus identified by spec version, the pre-v2.13 layout sample |
| [SEM] | §1 dependency list; §6 last paragraph |
| [STO] | §1 dependency list; §2 roles table and patch sentence; §4 role rule, coalescing rule, the 256 MiB split removed, `schema_version` dropped from the values a bus consumer owns; §5 role rule at registration; §6 heading and patch text, sets of one member; §8 vectors per §3.3 |
| [OVW] | §3 terminology (header section, pack metadata row), §4 Figure 2, §6 deferred table |
| tracepack-go.md | `Recover` marked deferred; the zero-copy struct view of the header section applies only to headers gathered into a row buffer |
| impl plan | the format-revision step; phases 2 and 3 (done) point to it for the footer builder, the validating writer and the bootstrap; phase 4 = `Verify`, then `Repair` after P10, `Recover` deferred; phase 5 coalescing rule, no size-capped sets, correction tests replaced by repair tests |
| §3.7–§3.8 in [FMT] | the dependency list's F-3 / F-4 index line; §2 the Bloom-hash rule removed; §3 I-2's pointer to a weaker header-only level, I-10 removed, I-11's `decode-failed` mention, I-14's attesting merger; §5 `blocks_validated` retired; §6 block read levels removed (every read full), the minimum `record_header_len` 44; §7.1 the 44-byte layout; §7.2 the copy class removed, the `field_validity` rule rewritten with the append-time check, the sentence naming `decode-failed` as the only derived quality bit; §9 `quality` bits 3 and 6 and `record_flags` bit 0 reserved; §10 F-3 `max_payload_len` retired, the F-3 range 0x0010–0x001F reserved without a planned structure, F-4 an absent undefined section; §12 the validation SHOULD without a commitment; §10 F-2 and footer-validation `record_header_len` ≥ 44; §13 the read validation status and header-only wording; §14 the `extraction_version` sentence; §16 the query-mode and attestation vectors removed, the redaction vector built on disagreeing copies rewritten to select by payload S/F, and availability vectors kept for each short-field threshold, clear validity over placeholder bytes, a set bit beyond the payload, control-frame fields and a 45–55-byte header, without validation-status expectations in the corpus contract |
| §3.7–§3.8 in [SEM] | §1 dependency list; §3 the `decode-failed` mirror sentence; §4 the `no-mono` mention; §6 quality bits and the derived predicates; §7.1 F-3 summary without the deferred S/F presence and System Bytes locator structures; §7.2 transaction lookup from payloads and the System Bytes locator removed; §7.3 extraction design removed; §7.4 without query modes or header-only reads, its S/F, System Bytes and well-known-id paths scanning payload fields instead of S/F presence, the locator and §7.3; §8 the clause on copy fields of masked records, the I-10 citation, and the F-4 `partial` marking |
| §3.7–§3.8 in [STO], [OVW] | [STO] §4 recorder and merger validation without attestation, §6 `blocks_validated` per pack, §8 attestation vectors; [OVW] §3 record header size, §4 Figure 3 without copy arrows, §6 deferred table (F-3 index structures no longer planned) |
| §3.7–§3.8 elsewhere | tracepack-go.md: §2 the eqp-hub `no-mono` note and the recorder's `blocks_validated` commitment, §3 the Go API of §3.7 and §3.8 (header-only iterator, `Query` mode, `Result` status, transaction and merge), §5.5 copy fields, §7 CLI; P8: the bus body; P10: premise items 3 (no `quality.decode-failed`) and 4, and the deletion witness; the VE design's candidate rule |
| impl plan | the format-revision step also carries the 44-byte record header, the payload helper, and the removal of query modes, header-only reads and attestation; phases 2 and 3 (done) point to it; §1's deferred F-3 index tags and F-4; phase 5's attestation items and acceptance criteria; phase 6's validation-status goldens and acceptance; phase 7's F-4 obligation; §4's attestation throughput |
| tracepack/CHANGELOG.md | the breaking changes: columnar header section, v0.1.0 packs unsupported, retired tags and value |

Applying P9 ends with a sweep of [FMT], [SEM], [STO], [OVW], tracepack-go.md and the implementation plan for these terms:
copy field, I-10, header-only, attested and attestation, `blocks_validated`, `header-validated`, provisional, authoritative, validation status,
`decode-failed`, `no-mono`, `record_flags.W`, `max_payload_len`, F-3 `content_bytes`, `schema_version`, `extraction_version`, F-4 content, locator, Bloom, S/F presence,
correction, and a 56-byte record header.
Every occurrence is revised or removed; the table lists the known ones.
