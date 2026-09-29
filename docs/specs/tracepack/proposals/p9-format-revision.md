# Proposal P9 — format revision before phase 4: columnar record headers, block coalescing, retirements

Status: draft (2026-09-29) — applies owner decisions G5-91..G5-96; review round 3 findings fixed, ready to apply.
Source: the weight and small-message overhead review of 2026-09-29 (kept outside the repository)
and the owner discussion of the same day.

## 1. Scope and how it closes

The review asked two questions:
whether tracepack is heavier than a log service and the Virtual Equipment (VE) need,
and whether its per-record fields cost too much for small HSMS / SECS-II messages.
Its measurements are in §2.

This proposal revises the format:
a byte-columnar header section (§3.1), block coalescing in merges (§3.2), two retirements (§3.3),
a small redundancy cleanup (§3.4), and the phase 4 scope (§3.6).
The owner decided it on 2026-09-29 (G5-91..G5-96).
It lands as spec v2.13 and as the format-revision step of the implementation plan, before phase 4.

The storage question the review raised — a view without commit objects — was part of this proposal until review round 1
and is now proposal P10 (G5-95).

P9 closes when it is applied or rejected:
the outcome goes to the changelog and the decision log, and this file is deleted.

## 2. Evidence

Measured with the tracepack Writer (zstd default level, attested) on synthetic traffic:
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
| header-section decode + S6F11 filter | 0.73 ms | 0.83 ms scanning two columns; 1.5 ms after untransposing |

## 3. Format

### 3.1 Byte-columnar header section (G5-92)

The header section of a decoded block body stores the record headers transposed:
with `n = record_count` and `L = record_header_len`,
byte `j` of record `i`'s header is at offset `j × n + i` of the header section, for 0 ≤ `j` < `L` and 0 ≤ `i` < `n`.
The section is still `n × L` bytes and still precedes the payload section,
so the payload offsets, I-2's length equation, the header-only read level and the footer are unchanged.

Proposed wording for [FMT §6],
replacing "The header section holds the `record_count` record headers in record order, each `record_header_len` bytes":

> The header section holds the `record_count` record headers **column by column**:
> for byte `j` of the record header layout (§7.1) and record `i`, the byte is at offset `j × record_count + i`.
> A record's header is the `record_header_len` bytes gathered this way;
> every rule that names a record header, a header field or a header byte refers to these gathered bytes.

Consequences, each to be stated where it applies:

- [FMT §7.1] offsets are positions in the gathered header, not in the section.
- A `record_header_len` above 56 transposes all `L` bytes;
  unknown bytes are byte columns like any other and are preserved
  (I-12 byte identity is over the gathered bytes and the payload).
- A header-only read decodes the whole header section before any record's header is complete, as today;
  I-2's seq checks run on the gathered headers.
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
the I-2 walk, header access, payload offsets, unknown header bytes;
a filter may instead scan columns directly.
The validating writer ([FMT §12]) checks the encoded bytes through the same path:
it decodes the encoded body, untransposes its header section, then runs the I-2 and I-10 checks;
checking the buffer before the transpose would not check what is written.
Golden block bodies and the reader's per-offset corruption expectations are regenerated.

**Pros:** header cost −40 % with no field removed and no semantic change; encoding 20 % faster;
column scans for S/F, direction and kind filters;
any language implements it with a nested loop (`reshape(L, n).T` in NumPy).

**Cons:** decoding costs 0.66 ms more per 4 MiB block
(full decode +29 %; the header-only path stays 2–3 times faster than a full decode);
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
> Attestation follows the rules above unchanged.

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
| derived quality bits (`no-mono`, `decode-failed`, `capture-boundary`) | keep | cost no bytes; `quality_union` and bit-mask filters use them, and the VE's candidate rule reads the bit set alone |
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

## 4. Spec changes

| Document | Changes |
|---|---|
| [FMT] | §3 I-6, I-11, I-12 correction wording; §5 `schema_version` retired, `coverage` and `pack_role` rows, replacement-set values fixed to 1 and 0, retired numbers never written from preserved entries; §6 columnar header section; §7.1 offset wording; §9 `pack_role` value 5 retired; §10 F-3 `content_bytes` retired, footer validation, F-5 aggregation of F-2 or F-3 values, copied F-3 lists; §12 validation of the transposed encoding; §13 bootstrap wording; §14 `schema_version` bullet and the one-time redefinition; §16 framing vectors regenerated, corpus identified by spec version, the pre-v2.13 layout sample |
| [SEM] | §1 dependency list; §6 last paragraph |
| [STO] | §1 dependency list; §2 roles table and patch sentence; §4 role rule, coalescing rule, the 256 MiB split removed, `schema_version` dropped from the values a bus consumer owns; §5 role rule at registration; §6 heading and patch text, sets of one member; §8 vectors per §3.3 |
| [OVW] | §3 terminology (header section, pack metadata row), §4 Figure 2, §6 deferred table |
| tracepack-go.md | `Recover` marked deferred; the zero-copy struct view of the header section applies only to headers gathered into a row buffer |
| impl plan | the format-revision step; phases 2 and 3 (done) point to it for the footer builder, the validating writer and the bootstrap; phase 4 = `Verify`, then `Repair` after P10, `Recover` deferred; phase 5 coalescing rule, no size-capped sets, correction tests replaced by repair tests |
| tracepack/CHANGELOG.md | the breaking changes: columnar header section, v0.1.0 packs unsupported, retired tags and value |
