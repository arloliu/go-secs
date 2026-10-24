# tracepack — file format

Status: current (2026-10-25) — v2.35, tracepack format 1.1.
External review consensus reached (review rounds recorded in `tracepack-spec-changelog.md`).
Normative, language-agnostic. Terminology and diagrams are in [OVW §3] and [OVW §4].

Depends on (the byte layouts, TLV encoding, footer and validation rules are self-contained; these rules give meaning to some fields and are defined elsewhere):
- [SEM §2] capture model (`fidelity`, `capture_method`, `vantage`), [SEM §3] `decode_status`, [SEM §4] time and clock anchor (`mono_ns`, `clock_step_tolerance_ns`),
  [SEM §5] transport events, [SEM §6] quality bits and `quality_evaluated`.
- [STO §2] scopes and roles, [STO §4] merge and deletion, [STO §6] archive identity
  (meaning of `pack_role`, `scope_generation`, `publisher_epoch`, `patch_base`, `replacement_set_*`, `compacted_from`, `supersedes`, `coverage`),
  [STO §4] the durable-bus contract (`flush_interval_ns`, `thinning`, the `stop-unclean` close),
  [STO §7] log converter and source provenance (`source_ref`, `source_dialect`).
- [SEM §7.4] reads of the packs a caller gives (I-12 compares identity within them).
- [SEM §7.1] the contract of the F-3 block summaries.
- [SEM §8] redaction: meaning of `redaction-present`, `redaction_policy`, `redaction` entries and `quality.redacted`, the encryption of originals, and the validation of redaction entries.
- [SEM §2] records unobserved by design (`quality.unobserved`, `thinning`).
- The conformance corpus of §16 also contains the single-pack vectors of [SEM §9]; its files, their schemas and the vector catalogue are [CORPUS].

References: `[FMT §n]` = `tracepack-format.md`, `[SEM §n]` = `tracepack-semantics.md`, `[STO §n]` = `tracepack-storage.md`, `[OVW §n]` = `tracepack-overview.md`, `[JSONL §n]` = `tracepack-jsonl.md`, `[CORPUS §n]` = `tracepack-corpus.md`; `[FMT I-n]` = invariant I-n of the format document.
Each rule is defined in exactly one document; the others only reference it.

## 1. Conventions

- `MUST`, `SHOULD`, `MAY` carry their RFC 2119 meaning.
- Integer types: `u8`, `u16`, `u32`, `u64` unsigned; `i64` signed two's complement; all little-endian (I-8).
  `[n]byte` is n raw bytes.
- Offsets in layout tables are in bytes from the start of the structure.
  Structures have no implicit padding; every byte is listed.
- **Reserved** fields and bits are zero on write and ignored on read (I-9).
- **Enums** are `u8` values from the registries of §9 and §5.
  Value 0 is each enum's "no information" value: `unknown` by default, `none` for `codec` and `timer`, `not-attempted` for `decode_status`.
  A reader preserves an unknown value and reports it as `unknown(<n>)`; it never maps it to a known value.
- **CRC** means CRC-32/ISO-HDLC (§2).

## 2. Portable encoding rules

These rules let any mainstream language implement the format from this text alone (Appendix A lists per-language support).

- **Integers**: fixed width, little-endian.
  Every `u64` offset, length, count or seq MUST be ≤ 2^63 − 1, so languages without unsigned 64-bit types hold it in a signed 64-bit value.
  `body_len`, `uncompressed_len` and `payload_len` MUST be ≤ 2^31 − 1 (the largest array in several runtimes);
  a record larger than the block size threshold is written alone in its own block.
  A value outside these limits is a decode error.
- **UUIDs** (`pack_id`, `capture_id`, references): 16 bytes in RFC 9562 byte order,
  i.e. the order of the hex digits in the canonical string, left to right.
  Implementations whose native UUID type uses another byte order MUST convert.
  Writers SHOULD generate `pack_id` and `capture_id` as UUIDv7 (time-ordered), so keys and the service's catalog entries sort by creation time;
  a converter that reuses its ids for reproducible output derives them deterministically instead ([STO §7] item 8, Reproducible output),
  so readers never depend on an id's order reflecting creation time.
- **Time**: `i64` nanoseconds since 1970-01-01T00:00:00Z on the POSIX time scale (no leap seconds);
  a clock reading during a leap second is stored as the clock reported it.
  Monotonic values (`mono_ns`) are `i64` nanoseconds relative to `capture_origin_mono_ns` ([SEM §4]).
- **Strings**: UTF-8, length given by the enclosing structure, not NUL-terminated.
  A writer writes no BOM; a reader accepts a leading U+FEFF and keeps it as part of the string.
  Invalid UTF-8 in the `utf8` value of a known tag is a decode error for that entry (§5); data that may not be UTF-8 uses a `bytes` value.
- **Checksums**: every CRC is **CRC-32/ISO-HDLC** (the IEEE 802.3 / zlib / PNG CRC):
  polynomial 0x04C11DB7 reflected (0xEDB88320), initial value 0xFFFFFFFF, final XOR 0xFFFFFFFF;
  check value over ASCII `123456789` = 0xCBF43926.
  CRC-32C is not used, because it is missing from some standard libraries.
  Each CRC's byte range is stated where the CRC field is defined.
- **Codecs** (block bodies and the footer only; never the file header, pack metadata, block envelope or trailer):

  | Value | Codec | Rules |
  |---|---|---|
  | 0 | `none` | the on-disk body is the decoded body (§6) verbatim |
  | 1 | `zstd` | exactly one Zstandard frame per RFC 8878; no dictionary; the frame content checksum MAY be present and is not relied on; the frame declares no window larger than the larger of 8 MiB and its decoded length, and never above 64 MiB, and a reader MAY reject a frame that does (G5-212) |
  | other | — | unknown: the block is unreadable and reported `incomplete`; adding a codec is a major version change (§14) |

  Readers MUST support `none` and `zstd`; writers SHOULD use `zstd` and MAY use `none`.
  Compression is per block, not per record:
  individual SECS messages are often under 1 KiB, but a block holds many structurally repetitive records.
  The codec set is deliberately small, because every codec allowed is one that every reader must implement.
- **Redaction digests** ([SEM §8]): HMAC-SHA-256 per RFC 2104 with SHA-256 per FIPS 180-4; the corpus (§16) publishes two test keys and the expected digests.
- **Encryption of originals** ([SEM §8]): AES-SIV-CMAC per RFC 5297 with a 64-byte key; the corpus (§16) publishes test keys and the expected outputs.

## 3. Invariants

- **I-1 Self-delimiting blocks.** Every block is a block envelope followed by a block body of `body_len` bytes.
  A reader can walk blocks forward from the end of the pack metadata without the footer;
  the envelope CRC protects the lengths that walk depends on.
- **I-2 Block integrity beyond the CRC.** After decoding the whole body, the reader verifies that the codec stream is complete and decodes to exactly `uncompressed_len` bytes,
  that `record_count × record_header_len` + Σ `payload_len` equals `uncompressed_len`,
  that the first record's seq equals `first_seq`, and that seqs strictly increase;
  a mismatch marks the block `corrupt` even if every CRC passed.
  The seq and length checks read the record headers gathered from the header section (§6).
  Every block read is a full read: a reader uses no record of a block before these checks pass.
- **I-3 Footer is derivable.** The footer contains nothing that cannot be rebuilt by walking the blocks.
- **I-4 Trailer locates the footer.** The 64-byte trailer at end of file holds the footer's offset, lengths, codec and CRC,
  the block count, the record count, the last seq, its own CRC and a magic (§11).
- **I-5 Finalized means trailer valid.** A file is finalized iff the trailer magic matches, the trailer CRC matches,
  the trailer places the footer at or after the end of the pack metadata and ending where the trailer starts (§11), and the footer CRC matches.
  This is the single logical finalization point; no byte is rewritten after it (§12).
  A crash after a durable trailer leaves a finalized file even if the writer's close operation never returned.
- **I-6 Immutable once finalized.** No byte of a finalized file is ever modified;
  a merge writes its archive as a new file ([STO §4]).
- **I-7 Capture id across rolls; epoch bound at the transport boundary.**
  `capture_id` is a UUID assigned per tool when the recorder starts
  — or, over a durable bus, each time the producer that feeds it starts producing: at its start and at each leadership change,
  overlapping producers each with their own capture ([STO §4], G5-242); and for a converter, when it establishes a logical conversion ([STO §7] item 8) —
  and written into every pack it rolls.
  `epoch` is assigned by a component that observes sockets directly, one per socket the HSMS implementation actually uses for a session,
  and stamped on every record from that socket independently of lifecycle notifications ([SEM §5]);
  accepted-then-refused sockets get their own epoch and a transition record.
  A recorder that cannot observe sockets writes `epoch = 0` (unknown) and sets `correlation-incomplete` on every record.
  Thus (`capture_id`, `epoch`) identifies one transport connection globally.
  A recorder restart always starts a new capture, because neither the monotonic clock nor the seq counter survives a process restart;
  consecutive captures of one recorder deployment are linked by `recorder_instance_id` and `previous_capture_id` (§5).
- **I-8 Little-endian for tracepack's own integers; wire bytes untouched, except for masking ([SEM §8]) and the zero-filled message text of a record unobserved by design ([SEM §2]).**
  HSMS payloads keep their big-endian length prefix and header (SEMI E37 §8.2).
- **I-9 Reserved is zero on write, ignored on read.**
- **I-10** Retired in spec v2.13; the number is not reused.
  The record header no longer copies bytes of the payload,
  so the HSMS header fields of a record exist only in its payload (§7.2).
- **I-11 Derived values are opinions, not facts.** `decode_status` and `trailing_bytes` are a classifier's judgement over the captured bytes.
  The pack metadata names the classifiers that computed them (`classifier` tag, one value per classifier, §5);
  a consumer MAY recompute them from the payload, except over the masked ranges of a masked record, where the stored values describe the source bytes ([SEM §8]),
  and over the message text of a record unobserved by design, which is not classified ([SEM §2]).
- **I-12 Record identity is (`capture_id`, `seq`).** `seq` is capture-scoped:
  it is assigned in capture order, starting at 0 and increasing by one per record, across every pack of the capture;
  a consumer's close of a capture its producer left open has seq 2^63 − 1, the capture's last ([STO §4]).
  Seqs are assigned by the writer or by the producer that hands the writer its records ([STO §4] Recorder over a durable bus);
  a writer never renumbers a seq it received.
  Merging packs never renumbers records, so a reference to (`capture_id`, `seq`) stays valid whichever file holds the record;
  `pack_id` identifies a file, never a record.
  Within a pack, records appear in strictly increasing seq order; gaps are allowed where other packs hold the missing records.
  Writers and mergers MUST write every copy of one identity byte-identical (record header and payload),
  whichever writer, consumer or merge writes it, except the masked records of an extract ([SEM §8]).
  A reader compares identity **within the packs a read is given** ([SEM §7.4]):
  it keeps one copy of identical records, and reports differing copies of one identity as a `conflict`, yielding every version, marked, never resolving it ([SEM §7.4]);
  a merge that finds one fails ([STO §4]).
  Such a difference is reported, and makes no file invalid:
  a `repair` patch written before v2.32 may hold a record re-emitted under its original identity with new stored bits or classification (G5-202),
  and an extract's masked records keep their identity while their bytes differ from the source's ([SEM §8]);
  a read given both such a file and the pack it derives from reports the conflict, and each file stays readable.
  A capture masked at write has no unmasked copy to conflict with ([STO §4] *Masking*).
- **I-13 Hour-aligned blocks.** All records of a block have `ts_utc_ns` in the same UTC hour;
  a writer closes the current block before appending a record from a different UTC hour.
  A block therefore belongs to exactly one hour and moves between packs without being re-encoded;
  in the storage profile every pack except an extract also belongs to one hour ([STO §2]).
- **I-14 Mergeable footer.** Every per-block footer entry is self-contained and position-independent,
  and every pack-level statistic is a mergeable aggregate (sum, min, max, union),
  so the footer of a merged pack is computed from its inputs' footers without decoding any block body (§10).
  A merger still reads and checks every input block in full, as verification does (§13), and re-encodes only the blocks it resolves or coalesces ([STO §4]).

## 4. File header (80 bytes, offset 0)

| Off | Type | Field | Meaning |
|---|---|---|---|
| 0 | [8]byte | `magic` | `89 54 50 4B 0D 0A 1A 0A` (`\x89 T P K \r \n \x1A \n`); detects non-tracepack input and text-mode transfer damage |
| 8 | u16 | `format_major` | 1 (§14) |
| 10 | u16 | `format_minor` | 0 or 1 (§14) |
| 12 | u32 | `flags` | bit 0 `redaction-present`: set exactly when at least one record carries `quality.redacted` ([SEM §8]): in an extract, or from format 1.1 in a pack masked at write ([STO §4]); bits 1–31 reserved. There is no finalized flag (§12) |
| 16 | u32 | `pack_metadata_len` | byte length of the pack metadata; the first block starts at 80 + this value |
| 20 | u32 | `pack_metadata_crc` | CRC over the pack metadata bytes |
| 24 | i64 | `writer_start_utc_ns` | wall clock when the writer opened the file; for a converter's reproducible output, a fixed time its conversion selects ([STO §7] Reproducible output); never the time of the capture |
| 32 | [16]byte | `pack_id` | UUID of this file |
| 48 | [16]byte | `capture_id` | UUID of the capture (I-7) |
| 64 | [12]byte | reserved | |
| 76 | u32 | `header_crc` | CRC over bytes 0–75 |

A reader checks `magic`, then `header_crc`, then the version (§14), before reading anything else.

## 5. Pack metadata (TLV)

The pack metadata is a sequence of TLV entries filling exactly `pack_metadata_len` bytes.
The same entry encoding is used for transport-event and annotation payloads (§8) and for nested values.

**Entry layout** (8-byte entry header, then the value):

| Off | Type | Field | Meaning |
|---|---|---|---|
| 0 | u16 | `tag` | registry below; 0x0000 invalid; 0x8000–0xFFFF private (skipped by readers that do not know them) |
| 2 | u8 | `value_type` | table below |
| 3 | u8 | reserved | |
| 4 | u32 | `length` | byte length of the value |
| 8 | [length]byte | `value` | |

**Value types**:

| Value | Type | Length |
|---|---|---|
| 1 | `u8` (enums and small integers) | 1 |
| 2 | `bool` (0 or 1) | 1 |
| 3 | `i64` | 8 |
| 4 | `u64` | 8 |
| 5 | `uuid` | 16 |
| 6 | `utf8` | any |
| 7 | `bytes` | any |
| 8 | `tlv` (nested entries; tags in the nested structure's own registry) | any |

Rules (an entry list that breaks one is invalid: invalid pack metadata rejects the file, §13,
and an invalid transport-event or annotation payload is not a valid TLV body, §8, §13):
- Framing, for every entry, known or not: `tag` is not 0; the entry header and its value lie within the list;
  a value type of fixed length (`u8`, `bool`, `i64`, `u64`, `uuid`) has exactly that length.
  A value type outside the table has no fixed length.
- A known tag's `value_type` equals the registry's, and its value passes the rule of its type:
  a `bool` is 0 or 1, a `u64` is at most 2^63 − 1 (§2), a `utf8` value is valid UTF-8 (§2),
  and a `tlv` value is itself an entry list valid under these rules and its nested registry.
  A reader descends at most 32 levels of known nested `tlv` values below the list it checks, and a known `tlv` value deeper than that makes the list invalid;
  the registries of format 1.0 nest one level, so no conforming list reaches the limit, and an unknown `tlv` value is never descended into.
- A known tag appears at most once unless the registry marks it repeatable.
- A required tag that is missing makes the list invalid;
  of the pack metadata's conditional requirements, a reader checks at bootstrap only those its metadata alone decides (§13).
- Unknown tags, private ones included, pass once framed: they may repeat, they are skipped using `length`,
  and their value is never interpreted, whatever their `value_type`;
  their raw value is preserved in the canonical export (§15).
- A tag, nested tag, enum value or bit that a minor version adds is known only in a pack of that minor or later (§14);
  in a pack of an earlier minor its number keeps the status that minor gave it — an unknown tag, a reserved bit —
  so the format 1.0 pack that holds it opens and exports as before.
- A **retired** tag number is never reused.
  A writer never writes it, also not from the unknown entries it preserved when it read another pack;
  a reader treats it as an unknown tag, and the canonical export of the pack that holds it still shows it (§15).

**Pack metadata tag registry**:

| Tag | Name | Type | Required when | Meaning |
|---|---|---|---|---|
| 0x0001 | — | — | — | retired; the format version alone identifies the layout (§14) |
| 0x0002 | `tool_id` | utf8 | always | the tool this capture belongs to |
| 0x0003 | `transport` | u8 enum | always | §9 |
| 0x0004 | `capture_method` | u8 enum | always | [SEM §2] |
| 0x0005 | `vantage` | u8 enum | always | [SEM §2] |
| 0x0006 | `recorder` | utf8 | always | recorder or converter product, version and mode; for display and tracing, never branched on |
| 0x0007 | `writer` | utf8 | always | tracepack writer implementation and version |
| 0x0008 | `classifier` | utf8, repeatable | any record's `decode_status` is not `not-attempted` / `not-applicable` | implementation and version that computed derived values (I-11); one value per classifier: a recorder or converter writes one, a merger every distinct value of its inputs ([STO §4]); a `repair` patch, no longer written (G5-202), holds those of the pack it repaired; in the masked-at-write profile of format 1.1 — a pack whose role is not `extract` and that carries `redaction_policy` — a writer writes exactly one value, its capture's classifier, whatever its records (G5-250, [STO §4] *Masking*), a writer rule that a merge checks and never a bootstrap rejection, and a merge of such packs carries that one value; every other pack, an extract under a policy included, keeps the rules above |
| 0x0009 | `time_source` | u8 enum | always | §9 |
| 0x000A | `capture_origin_utc_ns` | i64 | `time_source = capture-clock` | wall-clock counterpart of the mono origin ([SEM §4]) |
| 0x000B | `capture_origin_mono_ns` | i64 | `time_source = capture-clock` | mono origin ([SEM §4]) |
| 0x000C | `source_tz` | utf8 | `time_source = source-log` | IANA time-zone name the source timestamps are parsed in |
| 0x000D | `source_dialect` | utf8 | `capture_method = log` | log format and parser mode used ([STO §7]) |
| 0x000E | `source_ref` | utf8, repeatable | `capture_method = log` | identity of each source file |
| 0x000F | `max_frame_len` | u64, repeatable | any record is classified `oversized` | the configured maximum frame length ([SEM §3]); one value per configuration: a recorder or converter writes one, a merger every distinct value of its inputs ([STO §4]); a `repair` patch, no longer written (G5-202), holds those of the pack it repaired; with several values the pack does not say which one a record was classified under; in the masked-at-write profile a writer writes at most one value, its capture's limit, and none when the classifier has no limit (G5-250), a writer rule that a merge checks, and a merge of such packs carries that value or its absence; every other pack keeps the rules above |
| 0x0010 | `period_start` | i64 | always | start of the period this pack covers: the flush interval of a segment, the UTC hour of an archive ([STO]) |
| 0x0011 | `period_end` | i64 | always | end of that period (exclusive) |
| 0x0012 | `lifecycle_coverage` | u8 enum | always | §9, [SEM §5] |
| 0x0013 | `quality_evaluated` | bool | always | [SEM §6] |
| 0x0014 | — | — | — | retired (the record header length is per block, §6) |
| 0x0015 | `supersedes` | uuid, repeatable | patches; generations ≥ 1 with a predecessor | lineage only: for an archive written by a merge, the `pack_id` of every `archive` input, its predecessors, and none when it has none ([STO §4] Merge); for a `repair` patch, no longer written (G5-202), the packs it was written to replace; it never removes a pack from a read |
| 0x0016 | `coverage` | tlv, repeatable | a repair lost data | one declared lost range per entry, carried by merges ([STO §4] Merge); nested tags below; only repair wrote new entries, so the reference implementation writes none of its own (G5-202) |
| 0x0017 | `notes` | utf8 | optional | free text; never the sole record of data loss |
| 0x0018 | `pack_role` | u8 enum | always | segment / archive / extract / repair (§9, [STO §2]) |
| 0x0019 | `compaction_level` | u8 | always | 0 = written by a recorder or converter; n ≥ 1 = produced by merging packs of level < n |
| 0x001A | `compacted_from` | uuid, repeatable | its lineage list is non-empty | lineage list, cumulative, never a deletion criterion ([STO §4] Deletion): for an archive written by a merge, the lists of its inputs plus every input segment ([STO §4] Merge); for a `repair` patch, no longer written (G5-202), the list of the pack it repaired; absent when that list is empty, whatever `compaction_level` is (a converter's archive, [STO §7], and a merge whose inputs are only such archives); never in a segment or a patch of one |
| 0x001B | `recorder_instance_id` | uuid | always | stable identity of the recorder or converter deployment across restarts |
| 0x001C | `previous_capture_id` | uuid | optional | the capture this recorder deployment ran immediately before this one (I-7) |
| 0x001D | `extract_filter` | utf8 | `pack_role = extract` | description of the filter that selected the records; an extract is never a complete period |
| 0x001E | `site_id` | utf8 | optional | site or fab identifier |
| 0x001F | `equipment_model` | utf8 | optional | equipment model |
| 0x0020 | `equipment_sw_rev` | utf8 | optional | equipment software revision |
| 0x0021 | `host_software` | utf8 | optional | host-side software (e.g. EAP) name and version |
| 0x0022 | `host_endpoint` | utf8 | optional | host side of the connection, `address:port` |
| 0x0023 | `equipment_endpoint` | utf8 | optional | equipment side of the connection, `address:port` |
| 0x0024 | `equipment_connect_mode` | u8 enum (`socket_role`) | optional | whether the equipment side is passive or active |
| 0x0025 | `device_id` | u64 | optional | configured SessionID / DeviceID |
| 0x0026 | `hsms_timers` | tlv | optional | configured timers; nested tag *n* (1–8) = T*n* in milliseconds, `u64` |
| 0x0027 | `seq_start` | u64 | always | the first record's seq, or for a pack without records the capture's next seq (for a `repair` patch without records, no longer written (G5-202), the damaged pack's `seq_start`; for an archive without records, the largest `seq_start` of the merge's inputs, [STO §4]) |
| 0x0028 | `clock_step_tolerance_ns` | u64 | `time_source = capture-clock` | wall-clock drift the writer tolerates against its clock anchor before marking a step ([SEM §4]) |
| 0x002C | `flush_interval_ns` | u64 | a consumer of a durable bus | a durable-bus consumer's maximum normal segment-flush interval ([STO §4]); never written by a standalone recorder (G5-150) or by a converter ([STO §7]); spool recorders of earlier spec versions, never implemented, would have written it (G5-227) |
| 0x002D | `scope_generation` | u64 | every pack except `extract` | 0 for segments and patches, ≥ 1 for archives, produced by merges or written by a converter directly; an archive's value is supplied by its publisher, and tracepack ranks nothing by it ([STO §2], [STO §7]) |
| 0x002E | `publisher_epoch` | u64 | `scope_generation` ≥ 1 | the publisher's value, with no fencing meaning in tracepack ([STO §2]) |
| 0x002F | `patch_base` | uuid | a patch, when its scope has a generation | written only by repair, no longer written (G5-202): the `replacement_set_id` of the archive the patch was registered against; read and exported like any tag |
| 0x0029 | `replacement_set_id` | uuid | `scope_generation` ≥ 1 | identity only: every archive carries one ([STO §6]), and a merge writes a new value for its archive ([STO §4] Merge) |
| 0x002A | `replacement_set_size` | u64 | with `replacement_set_id` | number of packs in the set: always 1 ([STO §6]); any other value rejects the file |
| 0x002B | `replacement_set_index` | u64 | with `replacement_set_id` | this pack's index in the set: always 0; any other value rejects the file |
| 0x0031 | `redaction_policy` | tlv | the pack was written under a redaction policy | the policy applied ([SEM §8]); nested tags below |
| 0x0032 | `redaction` | tlv, repeatable | a record was masked | one mask per entry ([SEM §8]); nested tags below; ordered by `seq`, then first masked offset |
| 0x0030 | — | — | — | retired (block validation commits nothing to the pack metadata, §12) |
| 0x0033 | `thinning` | tlv, repeatable | a record carries `quality.unobserved` | from format 1.1: one thinned stream and function per entry, declared by the producer in the capture descriptor ([SEM §2], [STO §4]); a capture-level tag ([STO §4] Merge); nested tags below |

Nested tags of `coverage`: 0x0001 `capture_id` uuid, 0x0002 `seq_first` u64, 0x0003 `seq_last` u64,
0x0004 `time_start` i64, 0x0005 `time_end` i64.
A reader matches a `coverage` entry against a query by the entry's own bounds, never by the records around it:
the seq range [`seq_first`, `seq_last`] against the queried seqs, the time interval [`time_start`, `time_end`] against the queried time range, both inclusive.
A side whose bound is absent is unbounded, so an entry without a time interval intersects every time range of its capture, and one without a seq range every seq range;
an entry whose first bound exceeds its last intersects every query of its capture;
an entry without `capture_id` belongs to the pack's capture.
A read reports an entry when its capture is the capture of a pack the read reads ([SEM §7.4], G5-215),
so a read over several packs also reports an entry one pack holds for another capture it reads.
The rule errs toward `incomplete` (§13), because a missed intersection would present lost records as records that do not exist.
A writer writes `capture_id`, equal to the pack's own capture, `seq_first`, `time_start` and `time_end` in every `coverage` entry it declares,
and `seq_last` whenever the upper bound of the lost seqs is known;
an absent `seq_last` means the loss extends past the last seq the writer could establish.
The entries a merge carries from its inputs ([STO §4] Merge), and those a `repair` patch inherited, are carried as data, whatever nested tags they hold:
every entry, in its order, with every nested tag, type and value, unknown tags included;
the nested entries may be re-encoded in another order, since their order carries no meaning, with reserved bytes zero (I-9).

Nested tags of `redaction_policy`: 0x0001 `policy_id` utf8, 0x0002 `policy_version` u64, 0x0003 `key_id` utf8, 0x0004 `digest_algorithm` u8 enum (§9), all required;
and from format 1.1, 0x0005 `encryption_algorithm` u8 enum (§9) and 0x0006 `encryption_key_id` utf8, both present or both absent.
When `redaction_policy` is present in a pack of the masked-at-write profile (format 1.1, role not `extract`), both are present (G5-243);
an extract's `redaction_policy` holds neither, and a pack without `redaction_policy` has neither requirement.
This is a writer rule that a merge checks, never a bootstrap rejection, like the profile's `classifier` and `max_frame_len` rules;
a merge checks it on every input whose role is not `extract` and that carries `redaction_policy`, whatever its minor (G5-253, [STO §4] Merge).

Nested tags of `redaction` (a **redaction entry**): 0x0001 `seq` u64 (required), 0x0002 `item_path` utf8 (absent for a whole-text or annotation mask; empty for the root item),
0x0003 `masked_ranges` bytes (required: a *u64 array* of (offset, length) pairs, offsets from the start of the record payload, [SEM §8]),
0x0004 `domain` utf8 (required), 0x0005 `digest` bytes (required; 32 bytes for `hmac-sha256`),
and from format 1.1, 0x0006 `siv` bytes (16 bytes): present in every entry that masks a data record when the policy carries `encryption_algorithm`,
absent from an annotation's entries (G5-238, G5-243); its defects are recoverable ([SEM §8]).

Nested tags of `thinning`, from format 1.1: 0x0001 `stream` u64, 0x0002 `function` u64, 0x0003 `source` utf8 (where the sampling comes from, descriptive text), all required.

## 6. Block envelope (40 bytes) and block body

| Off | Type | Field | Meaning |
|---|---|---|---|
| 0 | [4]byte | `magic` | `54 50 4B 42` (`TPKB`) |
| 4 | u8 | `codec` | §2 |
| 5 | [1]byte | reserved | |
| 6 | u16 | `record_header_len` | bytes per record header in this block; 44 ≤ value ≤ 65535 (§7.1) |
| 8 | u32 | `body_len` | on-disk length of the block body |
| 12 | u32 | `uncompressed_len` | length of the decoded body |
| 16 | u32 | `record_count` | records in the block, ≥ 1 |
| 20 | u32 | `body_crc` | CRC over the on-disk block body bytes |
| 24 | u64 | `first_seq` | seq of the block's first record |
| 32 | [4]byte | reserved | |
| 36 | u32 | `envelope_crc` | CRC over bytes 0–35 |

The block body follows immediately.
Decoded, the block body is a **header section** followed by a **payload section**, with no padding.
The header section holds the `record_count` record headers **column by column**:
for byte `j` of the record header layout (§7.1) and record `i`, the byte is at offset `j × record_count + i` of the header section,
for 0 ≤ `j` < `record_header_len` and 0 ≤ `i` < `record_count`.
A record's header is the `record_header_len` bytes gathered this way;
every rule that names a record header, a header field or a header byte refers to these gathered bytes.
The section is `record_count × record_header_len` bytes long,
and a block of one record is laid out as its header's bytes in order.
The payload section holds the `record_count` payloads in record order;
record i's payload starts at offset `record_count × record_header_len + Σ_{j<i} payload_len_j` of the decoded body.
Record i is the pair (header i, payload i).
All products and sums are computed with overflow checks before any allocation or slicing.
A `record_header_len` below 44 makes the block `corrupt`.
A writer closes a block at a size threshold (default 4 MiB uncompressed) or on flush.

**Every block read is full.**
A reader decodes the whole block body and checks I-2 on the gathered headers before it uses any record of the block;
there is no read of the header section alone.
`verify` and recovery (§13) read blocks the same way.

## 7. Record header

A record is a (**record header**, payload) pair stored in the two sections of a decoded block body (§6, [OVW] Figure 3):
its record header gathered from the header section, its `payload_len` bytes of payload in the payload section.
Records exist only inside decoded block bodies.

### 7.1 Record header layout (44 bytes known in format 1.0)

Offsets are positions in the gathered record header (§6), not in the header section.

| Off | Type | Field |
|---|---|---|
| 0 | u64 | `seq` |
| 8 | i64 | `ts_utc_ns` |
| 16 | i64 | `mono_ns` |
| 24 | u32 | `epoch` |
| 28 | u32 | `payload_len` |
| 32 | u32 | `trailing_bytes` |
| 36 | u16 | `quality` (bit set, §9) |
| 38 | u8 | `kind` (enum) |
| 39 | u8 | `dir` (enum) |
| 40 | u8 | `fidelity` (enum) |
| 41 | u8 | `decode_status` (enum) |
| 42 | u8 | `field_validity` (bit set) |
| 43 | u8 | `record_flags` (bit set) |

Each record header is `record_header_len` bytes, from the block envelope (§6).
Format 1.0 writers write 44.
A value below 44 makes the block `corrupt` (§6);
for a larger value a reader reads the 44 bytes it knows and preserves the rest, the **extension area** from byte 44 on,
which is part of the record's bytes for byte identity (I-12).
The extension area is transposed with the rest of the header (§6): its bytes are byte columns like any other.
A pack may mix blocks of different header lengths.
Appending fields behind offset 44 is a minor version change; changing any existing offset is a major one (§14).

### 7.2 Record header semantics

Class column:
`observed` = a captured fact;
`declared` = assigned or declared by the writer, a fact about the capture that cannot be recomputed from the payload;
`derived` = a classifier's judgement, recomputable (I-11) except over masked ranges ([SEM §8]) and the message text of an unobserved record ([SEM §2]);
`structural` = framing and capture extent, computed from the captured bytes.

| Field | Class | Semantics |
|---|---|---|
| `seq` | declared | capture-scoped record number (I-12); the global record reference is (`capture_id`, `seq`) |
| `ts_utc_ns` | observed | wall clock at the observation point ([SEM §4]) |
| `mono_ns` | observed | elapsed monotonic ns since `capture_origin_mono_ns`; valid iff `record_flags.mono_present` |
| `epoch` | declared | transport connection number within the capture (I-7); 0 = unknown, which forces `correlation-incomplete` |
| `kind` | declared | data / control / transport-event / annotation |
| `dir` | observed | direction; `quality.direction-inferred` marks an inferred value |
| `fidelity` | declared | how faithfully the payload reproduces the transmitted bytes ([SEM §2]; for a `redacted` record, the source payload, [SEM §8]; for an `unobserved` record, the captured header, [SEM §2]) |
| `quality` | declared | stored quality flags ([SEM §6]) |
| `decode_status` | derived | [SEM §3] |
| `trailing_bytes` | derived | message-text bytes after the first complete SECS-II item; meaningful only when `decode_status` is `ok` or `ok-with-trailing`; for a `redacted` record, of the source message text; 0 for an `unobserved` record |
| `field_validity` | structural | which HSMS header fields of the payload are real: present in the captured bytes, or, for a log conversion, established from the source's metadata ([STO §7]) |
| `payload_len` | structural | payload length in bytes |

**HSMS header fields.**
The record header holds no field of the HSMS message header;
the payload of a data or control record holds the whole captured frame (§8), and a predicate on such a field reads it there.
Positions count from the start of the payload:

| Field | Payload bytes | `field_validity` bit (§9) |
|---|---|---|
| SessionID, or DeviceID for normalised SECS-I, as observed, never normalised | 4–5, big-endian | 0 |
| stream; W | 6, bits 0–6; 6, bit 7 | 1 |
| function | 7 | 2 |
| PType, full byte value including values E37 does not define | 8 | 3 |
| SType, likewise | 9 | 4 |
| System Bytes, as on the wire | 10–13 | 5 |

The positions are identical for data and control records, and a predicate reads each field at its position for both kinds alike:
for control messages, bytes 6 and 7 carry status or reason codes (E37 §8.3), which a stream, W or function predicate tests as the bytes stored there;
interpreting them according to SType is the consumer's, not the predicate's.
A field is **available** only when its `field_validity` bit is set and the payload holds every byte of the field;
a record whose kind is neither data nor control has no available field.
A filter or index MUST treat an unavailable field as "cannot match" on that field unless the caller explicitly asks for records with unavailable fields.
A predicate on several fields, such as S/F (stream and function), needs each of them available:
a record with any of them unavailable cannot match it, whatever its available fields hold,
and when the caller asks for records with unavailable fields, such a record satisfies the whole predicate.
Payload presence alone never makes a field available:
a clear bit is unavailable even where the payload holds the field's bytes,
as a log conversion's payload does for an identity its source did not carry ([STO §7]).

**Setting `field_validity`.**
For a raw capture a writer sets a bit iff the payload holds that field's bytes.
For a log conversion the converter clears the bits of identities its source did not carry,
although the reconstructed frame holds placeholder bytes there ([STO §7] item 4);
a writer never sets a bit merely because bytes are present.
Transport-event and annotation records carry a TLV payload and have `field_validity` 0.
A set bit requires the payload to hold every byte of its field;
a set bit whose bytes the payload lacks is a writer defect.
Every writer checks this when a record is appended, whether or not it validates its encoded blocks (§12),
and rejects the record;
a reader treats the field as unavailable, and `verify` reports the defect.

## 8. Payload by kind

- **data / control**: the full HSMS frame as captured ([OVW] Figure 3):
  4-byte big-endian length, 10-byte HSMS message header, message text (SEMI E37 §8.2).
  Short or over-long captures are stored as captured, with `field_validity` and `decode_status` saying what is missing;
  the writer never pads, truncates or repairs bytes.
  In a pack written under a redaction policy, the payload is the captured frame with the masking of [SEM §8] applied, and nothing else changed.
  A record unobserved by design holds its captured length field and HSMS header and a zero-filled message text ([SEM §2]).
  For `transport = secs1-normalised`: the reassembled SECS-I message in HSMS framing;
  SECS-I block-level evidence is not preserved in format 1.0 ([OVW §5]).
- **transport-event**: a TLV body (§5 entry encoding) with these tags:

  | Tag | Name | Type | Used for |
  |---|---|---|---|
  | 0x0001 | `event` | u8 enum | required |
  | 0x0002 | `prev_state` | u8 enum | state-transition |
  | 0x0003 | `cur_state` | u8 enum | state-transition |
  | 0x0004 | `cause` | u8 enum | state-transition, socket-close |
  | 0x0005 | `cause_raw` | utf8 | the implementation's own name for the cause, e.g. `vendor:CauseName` |
  | 0x0006 | `timer` | u8 enum | timer-expiry, and state-transitions caused by a timer |
  | 0x0007 | `socket_role` | u8 enum | socket events |
  | 0x0008 | `inferred` | bool | a socket event derived from a lifecycle notification rather than observed ([SEM §5]) |
  | 0x0009 | `primary_session_id` | u64 | T3: SessionID of the timed-out primary |
  | 0x000A | `primary_stream` | u64 | T3: stream of the primary |
  | 0x000B | `primary_function` | u64 | T3: function of the primary |
  | 0x000C | `primary_system_bytes` | bytes (4) | T3: System Bytes of the primary |
  | 0x000D | `boundary_kind` | u8 enum | capture-boundary |
  | 0x000E | `boundary_seq_first` | u64 | capture-boundary `gap`: first seq of the range it delimits; absent from a `start`, `stop` or `stop-unclean` boundary, which its record's seq places ([SEM §5]) |
  | 0x000F | `boundary_seq_last` | u64 | capture-boundary `gap`: last seq of that range |
  | 0x0010 | `gap_start` | i64 | capture-boundary gap: start of the estimated missing interval |
  | 0x0011 | `gap_end` | i64 | capture-boundary gap: end of that interval |
  | 0x0012 | `detail` | utf8 | free text |
  | 0x0013 | `clock_step_ns` | i64 | clock-step: wall-clock step observed, positive = forward ([SEM §4]) |

- **annotation**: a TLV body with these tags:

  | Tag | Name | Type | Used for |
  |---|---|---|---|
  | 0x0001 | `annotation_kind` | u8 enum | required |
  | 0x0002 | `ref_capture_id` | uuid | capture of the referenced records; absent = refers to no record |
  | 0x0003 | `ref_seq_first` | u64 | first referenced seq in `ref_capture_id` |
  | 0x0004 | `ref_seq_last` | u64 | last referenced seq |
  | 0x0005 | `text` | utf8 | exactly one of `text` / `raw` |
  | 0x0006 | `raw` | bytes | source bytes that are not valid UTF-8; in a `redacted` annotation the value is masked and no longer reproduces them ([SEM §8]) |
  | 0x0007 | `source_index` | u64 | which `source_ref` entry (0-based) the range refers to |
  | 0x0008 | `source_offset` | u64 | byte offset in that source file ([STO §7]) |
  | 0x0009 | `source_len` | u64 | byte length in that source file |

- A transport-event or annotation payload is a **valid TLV body** when it is an entry list that fills the payload exactly and passes the rules of §5 under its kind's registry above,
  a present `primary_system_bytes` holds exactly 4 bytes, and an annotation holds exactly one of `text` and `raw`, either of which may be empty.

- Every record produced from a source file must be able to carry a source reference.
  Annotations carry it as above; the representation for data/control records is deferred ([OVW §6]).

## 9. Enum and bit registries

Record-header enums:

| Enum | Values |
|---|---|
| `kind` | 0 unknown, 1 data, 2 control, 3 transport-event, 4 annotation |
| `dir` | 0 unknown, 1 host-to-equipment, 2 equipment-to-host, 3 local |
| `fidelity` | 0 unknown, 1 wire-exact, 2 re-encoded, 3 reconstructed, 4 synthesized, 5 not-applicable |
| `decode_status` | 0 not-attempted, 1 ok, 2 ok-with-trailing, 3 short-frame, 4 length-mismatch, 5 bad-ptype, 6 bad-stype, 7 control-with-body, 8 oversized, 9 item-decode-error, 10 reconstructed-ok, 11 parse-failed, 12 build-rejected, 13 not-applicable |

Record-header bit sets (bit 0 = least significant):

| Field | Bits |
|---|---|
| `quality` (u16) | 0 capture-boundary, 1 ordering-uncertain, 2 correlation-incomplete, 4 direction-inferred, 5 redacted, 7 unobserved (from format 1.1, §5); 3 and 6 retired; 8–15 reserved |
| `field_validity` (u8) | 0 SessionID, 1 stream and W (payload byte 6), 2 function, 3 PType, 4 SType, 5 System Bytes (§7.2); 6–7 reserved |
| `record_flags` (u8) | 1 mono_present; 0 retired; 2–7 reserved |

A **retired** bit is reserved (I-9) and never reused.
A record without monotonic time is one whose `record_flags.mono_present` is clear ([SEM §6]).

Pack-metadata enums:

| Enum | Values |
|---|---|
| `transport` | 0 unknown, 1 hsms-ss, 2 secs1-normalised |
| `capture_method` | 0 unknown, 1 raw-stream, 2 decoded-message, 3 log, 4 generator ([SEM §2]) |
| `vantage` | 0 unknown, 1 host, 2 equipment, 3 intermediary, 4 network, 5 none ([SEM §2]) |
| `time_source` | 0 unknown, 1 capture-clock, 2 source-log, 3 generator |
| `lifecycle_coverage` | 0 unknown, 1 subscribed, 2 none |
| `pack_role` | 0 unknown, 1 segment, 2 archive, 3 extract, 4 repair (no longer written, G5-202; read and exported like any role); 5 retired: never written, reported `unknown(5)` like any unknown value (§1), and handled by [STO §4] as any role other than segment or archive |
| `digest_algorithm` | 0 unknown, 1 hmac-sha256 (nested in `redaction_policy`, [SEM §8]) |
| `encryption_algorithm` | from format 1.1: 0 unknown, 1 aes-siv-cmac-512 (nested in `redaction_policy`, [SEM §8]) |

Payload enums (§8, [SEM §5]):

| Enum | Values |
|---|---|
| `event` | 0 unknown, 1 state-transition, 2 timer-expiry, 3 socket-accept, 4 socket-connect, 5 socket-close, 6 capture-boundary, 7 clock-step |
| `state` | 0 unknown, 1 not-connected, 2 not-selected, 3 selected (E37 connection states) |
| `timer` | 0 none; 1–8 = T1..T8 (the value is the timer number; SECS-I T1 / T2 / T4 included) |
| `cause` | 0 unknown, 1 local-open, 2 local-close, 3 select-accepted, 4 select-rejected, 5 local-deselect, 6 peer-deselect, 7 local-separate, 8 peer-separate, 9 timer-expiry, 10 linktest-failure, 11 transport-error, 12 peer-close, 13 implementation-fault |
| `socket_role` | 0 unknown, 1 passive, 2 active |
| `boundary_kind` | 0 unknown, 1 start, 2 stop, 3 gap, 4 stop-unclean (the capture ended without a clean stop; written by a consumer closing a capture its producer left open, at seq 2^63 − 1 ([STO §4]), and at other seqs by the spool recovery of earlier spec versions, which stay valid; a read or lookup whose time range its gap meets reports `incomplete`, [SEM §7.4], G5-209) |
| `annotation_kind` | 0 unknown, 1 note, 2 unparsed-entry, 3 skipped-bytes, 4 unrecognised-line |

## 10. Footer

The footer is encoded as a whole with the trailer's `footer_codec`.
Offsets inside the footer are relative to the start of the **decoded** footer.
Its layout is fixed by `footer_layout_version = 1`;
a reader that finds another value treats the footer as not valid and reads the pack by the forward walk (§13, §14);
extension happens through TLV tags inside F-3, F-4 and F-5, so a later index structure is a minor addition, not a new layout.

```
 decoded footer
 ┌───────────────────────────┐ 0
 │ F-1 prologue (72 B)       │  layout version, flags, block_count, section offsets
 ├───────────────────────────┤ f2_offset
 │ F-2 block index           │  block_count × f2_entry_len, fixed-size entries in file order
 ├───────────────────────────┤ f3_offset
 │ F-3 block summaries       │  one TLV entry list per block, located by its F-2 entry
 ├───────────────────────────┤ f4_offset
 │ F-4 (undefined)           │  a slot for a later index; absent in format 1.0 writers
 ├───────────────────────────┤ f5_offset
 │ F-5 pack statistics (TLV) │  mergeable aggregates
 └───────────────────────────┘
```

**F-1 prologue** (72 bytes):

| Off | Type | Field | Meaning |
|---|---|---|---|
| 0 | u16 | `footer_layout_version` | 1 |
| 2 | u16 | `flags` | bit 0 F-3 present (always set in layout 1), bit 1 F-4 present; others reserved |
| 4 | u32 | `block_count` | entries in F-2; equals the trailer's `block_count` |
| 8 | u32 | `f2_entry_len` | bytes per F-2 entry; 80 in layout 1; readers read the fields they know and skip the rest |
| 12 | u32 | `extraction_version` | 0; kept for the rule set of a later F-4 index; readers of format 1.0 ignore it |
| 16 | u64 | `f2_offset` | F-2 length is `block_count × f2_entry_len` |
| 24 | u64 | `f3_offset` | |
| 32 | u64 | `f3_len` | |
| 40 | u64 | `f4_offset` | |
| 48 | u64 | `f4_len` | 0 when F-4 is absent |
| 56 | u64 | `f5_offset` | |
| 64 | u64 | `f5_len` | |

**F-2 block index entry** (80 bytes in layout 1), one per block in file order, which is also ascending seq order (I-12):

| Off | Type | Field | Meaning |
|---|---|---|---|
| 0 | u64 | `offset` | file offset of the block envelope |
| 8 | u32 | `on_disk_len` | envelope + body length |
| 12 | u32 | `uncompressed_len` | as in the envelope |
| 16 | u32 | `record_count` | as in the envelope |
| 20 | u32 | `body_crc` | copy of the envelope's `body_crc`; a merger uses it only as a candidate filter, never as proof of equality ([STO §4]) |
| 24 | u64 | `first_seq` | seq of the block's first record |
| 32 | u64 | `last_seq` | seq of the block's last record |
| 40 | i64 | `ts_min` | **true** minimum `ts_utc_ns` in the block, not the first ([SEM §4]) |
| 48 | i64 | `ts_max` | true maximum; `ts_min` and `ts_max` lie in the same UTC hour (I-13) |
| 56 | u32 | `epoch_min` | |
| 60 | u32 | `epoch_max` | |
| 64 | u64 | `summary_offset` | offset of this block's F-3 entry list, relative to `f3_offset` |
| 72 | u32 | `summary_len` | length of the block's F-3 entry list; never 0 in layout 1 |
| 76 | u16 | `record_header_len` | as in the envelope; ≥ 44 |
| 78 | [2]byte | reserved | |

F-2 is scanned linearly for time or epoch pruning, because timestamps are not ordered across blocks ([SEM §4]);
it can be binary-searched by seq.

**F-3 block summaries** (required; flag bit 0 set): for each block, a TLV entry list (§5 encoding) of `summary_len` bytes.
Every F-5 statistic is an aggregate of F-2 and F-3 values, so the F-3 tags marked *required* below exist for every block.
*u32 array* below means a `bytes` value holding little-endian `u32` counts indexed by enum value (a block holds fewer than 2^32 records);
a reader treats missing trailing elements as 0 and preserves extra elements.

| Tag | Name | Type | Meaning |
|---|---|---|---|
| 0x0001 | `kind_counts` | u32 array | required; records per `kind` |
| 0x0002 | `dir_counts` | u32 array | required; records per `dir` |
| 0x0003 | `decode_status_counts` | u32 array | required; records per `decode_status` |
| 0x0004 | — | — | retired |
| 0x0005 | `quality_union` | u64 | required; union of the records' stored `quality` bits (low 16 bits) |
| 0x0006 | — | — | retired (F-2 `uncompressed_len` holds the block's record header + payload bytes) |
| 0x0007 | `epoch` | tlv, repeatable | required, one per epoch present in the block; nested tags below |
| 0x0008 | `boundary` | tlv, repeatable | one per capture-boundary record in the block; nested tags below |
| 0x0009 | `seq_range` | bytes (16), repeatable | present only when the block's seqs are not contiguous: its exact seqs as sorted, maximal ranges; absent means exactly `first_seq`..`last_seq` |
| 0x0010–0x001F | reserved | — | no structure is planned for them ([OVW §6]) |

A **retired** F-3 tag number is never reused:
a writer never writes it into an F-3 list it builds, and a reader treats it as an unknown F-3 tag.

**F-4**: undefined in format 1.0.
A writer writes F-4 absent: F-1 flag bit 1 clear, `f4_offset` = `f5_offset`, `f4_len` = 0 and `extraction_version` = 0.
A reader that finds F-4 present checks its placement (footer validation, below) and otherwise ignores its bytes;
a merger writes F-4 absent.
The slot is kept so that a later index needs no new footer layout;
such an index MUST be per block and self-contained (I-14).

**F-5 pack statistics**: a TLV entry list.

| Tag | Name | Type | Meaning |
|---|---|---|---|
| 0x0001 | `record_count` | u64 | records in the pack |
| 0x0002 | `kind_counts` | u64 array | |
| 0x0003 | `dir_counts` | u64 array | |
| 0x0004 | `decode_status_counts` | u64 array | |
| 0x0005 | `ts_min` | i64 | whole-pack minimum `ts_utc_ns` |
| 0x0006 | `ts_max` | i64 | whole-pack maximum |
| 0x0007 | `content_bytes` | u64 | record header + payload bytes: the sum of the blocks' F-2 `uncompressed_len` |
| 0x0008 | `quality_union` | u64 | |
| 0x0009 | `seq_range` | bytes (16: `first` u64, `last` u64), repeatable | the exact seqs present, as sorted, non-overlapping, maximal ranges |
| 0x000A | `epoch` | tlv, repeatable | one per epoch present; nested tags below |
| 0x000B | `boundary` | tlv, repeatable | one per capture-boundary record in the pack; nested tags below |

Nested tags of `epoch` (F-3 and F-5): 0x0001 `epoch` u64, 0x0002 `record_count` u64, 0x0003 `seq_first` u64, 0x0004 `seq_last` u64,
0x0005 `ts_min` i64, 0x0006 `ts_max` i64,
0x0007 `close_seq` u64 (optional: the seq of a record of this block or pack that ends this epoch, as defined below).
Nested tags of `boundary`: 0x0001 `seq` u64, 0x0002 `boundary_kind` u8, 0x0003 `ts` i64, 0x0004 `epoch` u64,
0x0005 `gap_start` i64 and 0x0006 `gap_end` i64 (copied from the boundary record's payload; an absent `gap_start` on a `stop-unclean` boundary means unbounded, [STO §4]).
*u64 array* is like *u32 array* with `u64` elements; pack-level counts use it so they cannot overflow.

A **capture-boundary record** is a transport-event record whose payload is a valid TLV body (§8) with `event` = capture-boundary.
Each one has a `boundary` entry, whose `boundary_kind` is 0 (unknown) when the payload carries none.
Quality bit 0 (`capture-boundary`, §9) does not make a record one:
a record that carries the bit without being a capture-boundary record has no `boundary` entry.
A record **ends an epoch** when it is a socket-close event (a transport-event record whose payload is a valid TLV body with `event` = socket-close)
or a clean `stop` (a capture-boundary record with `boundary_kind` = stop; `stop-unclean` ends no epoch),
and the epoch it ends is the one in its own record header.
An F-3 `epoch` entry carries `close_seq` when a record of the block ends that epoch, with the lowest seq among those records;
an F-5 `epoch` entry carries the minimum `close_seq` of the blocks' entries for that epoch, and none when no block's entry has one.
A clean `stop` therefore sets `close_seq` only in the entry of its own epoch:
that the capture ended is shown by its `boundary` entry, never by `close_seq` in the entries of other epochs;
in a transaction lookup, an epoch or capture closure bounds the window only through a closing record actually decoded during the lookup's reads,
and footer entries are never chased ([SEM §7.2]).
A transport-event record whose payload is not a valid TLV body is neither a capture-boundary record nor the end of an epoch:
it adds no `boundary` entry and no `close_seq`, and `verify` reports it as a writer defect (§13).

**Aggregation rule** (I-14): F-5 of any pack equals the aggregate of its blocks' F-2 and F-3 values —
counts summed, `content_bytes` summed from F-2 `uncompressed_len`, minima and maxima taken, seq ranges unioned and coalesced,
`epoch` entries combined per epoch (counts summed, seq and ts extremes taken, `close_seq` the minimum of those present), `boundary` entries unioned.
A merged pack's F-2 is recomputed from the copied blocks' new offsets, each copied block's F-3 entry list is copied verbatim,
and F-5 is recomputed by this rule, so constructing the footer reads no block body;
a merge may still decode blocks for other reasons ([STO §4]).
A copied F-3 list keeps its retired and unknown tags: they are data the reader skips, not tags the merger writes.
A merger that builds a new F-3 list (a coalesced or resolved block) never includes a retired tag.
A statistic that is not an aggregate of per-block F-2 or F-3 values MUST NOT be added to F-5.

**Footer validation.** Before trusting a footer, a reader checks:
`f2_offset = 72`; `f2_entry_len ≥ 80`; F-2, F-3, F-4 and F-5 lie inside the decoded footer, in that order, without overlap;
F-2 entries are contiguous on disk (the first starts at 80 + `pack_metadata_len`, each starts where the previous ends, the last ends at `footer_offset`);
`first_seq ≤ last_seq` and seqs ascend across entries; every `summary_offset` / `summary_len` lies inside F-3;
each block's seq set is consistent with its `record_count`: an F-3 `seq_range` list is sorted, non-overlapping, starts at `first_seq`, ends at `last_seq` and has exactly `record_count` members,
and without it `last_seq − first_seq + 1 = record_count`;
every F-3 count array and the F-3 `epoch` record counts sum to the block's `record_count`;
F-5 equals the aggregate of F-2 and F-3 (§10 aggregation rule), which the reader recomputes;
the sum of F-2 `record_count` equals F-5 `record_count` and the trailer's `record_count`;
the trailer's `last_seq` equals the last entry's `last_seq`.
Per block also: `record_header_len ≥ 44`, `ts_min ≤ ts_max` in the same UTC hour, `epoch_min ≤ epoch_max`,
every F-3 `epoch` entry inside the block's epoch, seq and time bounds, and every `boundary` or `close_seq` seq inside the block's seq set.
All sums are computed with overflow checks; an overflow invalidates the footer.
A pack without blocks has `block_count = 0`, an empty F-2 and F-3, `record_count = 0`, `footer_offset = 80 + pack_metadata_len`,
and no F-5 `ts_min`, `ts_max`, `seq_range`, `epoch` or `boundary` entries; its trailer `last_seq` is ignored.
When a block is read, its envelope MUST agree with its F-2 entry, `record_header_len` included, or the block is `corrupt`.
A block whose envelope agrees but whose records disagree with its F-2 entry (last seq, time or epoch range) is not `corrupt`:
its records are used, and the read is `incomplete`, because the same index pruned the blocks it did not read (§13).
A reader that prunes blocks by a valid footer trusts it:
an index that misstates a block while staying consistent with itself passes these checks,
so a read that prunes by it can miss that block's records without reporting it.
Such an index is a writer defect that only reading every block reveals; `verify` reports it (§13, `finalized-inconsistent`).
A footer failing any check is **invalid**: the reader reports it and falls back to the forward walk of I-1, never trusting the index.

## 11. Trailer (64 bytes, at size − 64)

| Off | Type | Field | Meaning |
|---|---|---|---|
| 0 | u64 | `footer_offset` | file offset of the footer; at or after the end of the pack metadata (80 + `pack_metadata_len`) |
| 8 | u64 | `footer_len` | on-disk footer length; `footer_offset + footer_len = size − 64` |
| 16 | u64 | `footer_uncompressed_len` | decoded footer length |
| 24 | u32 | `block_count` | blocks in the file |
| 28 | u32 | `footer_crc` | CRC over the on-disk footer bytes |
| 32 | u64 | `record_count` | records in the file |
| 40 | u64 | `last_seq` | seq of the last record; meaningful iff `record_count > 0` |
| 48 | u16 | `trailer_version` | 1 |
| 50 | u8 | `footer_codec` | §2 |
| 51 | u8 | `flags` | reserved |
| 52 | u32 | `trailer_crc` | CRC over bytes 0–51 |
| 56 | [8]byte | `magic` | `54 50 4B 45 4E 44 0D 0A` (`TPKEND\r\n`) |

## 12. Writer commit sequence (contract)

1. Write the file header and the pack metadata.
2. For each block: a writer SHOULD validate the block after encoding it and before writing it:
   decode the encoded body, gather the record headers from its header section (§6), and check I-2 on them.
   Checking the records before the encoding would not check what is written.
   Then write envelope + body;
   if durability is enabled, flush to stable storage after the whole block is written.
   A writer's output target and its durability operation are separate capabilities;
   a seekable stream alone is not a durability contract.
3. On close: write the footer, then the trailer, then flush to stable storage.
   The trailer is the commit (I-5).
   No byte is rewritten after the trailer:
   a "finalized" hint, if a publisher wants one, goes into the stored object's metadata, never into the file.
   A crash before the trailer is durable leaves an unfinalized file;
   a crash after it leaves a finalized file whose close operation did not return.

If a validation check of step 2 fails, the writer does not write the block and MUST NOT write the trailer,
so the pack never becomes finalized.
A writer never alters record bytes to make a check pass.
Validation commits nothing to the pack metadata:
a reader checks every block it reads (§6) whether or not the writer validated it.
The `field_validity` check of §7.2 runs when a record is appended, not here.

## 13. Reader bootstrap and recovery (contract)

The object size is known before reading (file system stat, object listing, the service's catalog, or a HEAD request).

```
                      ┌─ size ≤ whole-read threshold? ── yes ──► read the whole object (1 request)
                      │
 reader ── size ──────┼─ footer location given?       ── yes ─► round 1, in parallel:
                      │                                           head read  +  footer..EOF read
                      │                                           (footer + trailer; trailer verified)
                      │
                      └─ otherwise ────────────────────────────► round 1, in parallel:
                                                                  head read  +  tail read (last W bytes,
                                                                  holds trailer + usually the footer)
                                                                  round 2 only if the footer or the pack
                                                                  metadata is larger than the read window
```

- **Bootstrap**: obtain the file header, pack metadata, trailer and footer as above.
  The head read is a speculative prefix read that normally covers the pack metadata too.
  A footer location the caller gives (e.g. from the service's catalog) is a hint: the footer read extends to end of file,
  so it always includes the trailer, which is still parsed and verified (I-5);
  a reader never treats a pack as finalized from that hint alone.
  Validate the file header and the format version **before** reading any block.
  Typical cost is one round; the worst case is two.
  Suffix range reads (`bytes=-W`) work on S3 and GCS;
  stores without them use an absolute range computed from the known size.
  Window W and the whole-read threshold are reader parameters, not format properties.
- **Point queries** after bootstrap cost one round per group of blocks.
- **Why the trailer is at the end** rather than an index at the front.
  Only the trailer is fixed-size; the footer is variable and grows with the block count.
  A trailer at the front would still need a second read for the footer,
  and a footer at the front requires buffering or rewriting the whole file,
  which breaks streaming writes, multipart upload while recording, crash safety, and I-5.
- **Verification and recovery** (`verify`): walk blocks forward from the end of the pack metadata applying I-1 / I-2,
  decoding and validating whole bodies.
  Without a valid footer (§10), each block's header length comes from its envelope alone and there is no F-2 entry to compare;
  the walk stops at an envelope it cannot account for, and no block after it is located.
  An envelope the walk cannot account for is one that the bytes left before the end of the block region cannot hold,
  one whose body ends past that end,
  or one that fails any check of the block envelope (§6, §2):
  its magic, its CRC, or a field outside its range, such as a `first_seq` above 2^63 − 1 or a `record_header_len` below 44 (G5-184);
  an envelope whose CRC holds is no exception, since without a footer nothing confirms its fields.
  The block region ends at the end of the object, or at `footer_offset` when the trailer is valid.
  With a valid footer, each block's envelope is also checked against its F-2 entry,
  and its records against that entry and the block's F-3 summary (I-3);
  a block whose envelope fails is passed by its F-2 entry, so the blocks after it are still checked.
  A block **fails** when its envelope, its body CRC, I-2 or the agreement of its envelope with its F-2 entry fails,
  or when its codec is outside the registry (§2);
  a block that does not fail is **validated**.
  The **outcome** of a pack is the first of these that applies:
  1. `corrupt-middle`: a block fails, and a validated block follows it.
  2. `unfinalized`: I-5 does not hold.
  3. `finalized-truncated`: a block fails, or the walk stops before `footer_offset`.
  4. `finalized-inconsistent`: the footer is invalid, a block's records disagree with its F-2 entry or its F-3 summary,
     or the walked blocks disagree with the trailer's `block_count` or `record_count`;
     every block is validated, so no record is lost.
  5. `finalized-consistent`: none of the above.

  Validated blocks are also checked for **writer defects**, which are reported and never change the outcome:
  a set `field_validity` bit whose bytes the payload lacks (§7.2);
  a transport-event or annotation record whose `field_validity` is not 0 or whose payload is not a valid TLV body (§8);
  a block whose first seq is not above the last seq of the validated block before it (I-12);
  a block whose records lie in more than one UTC hour (I-13);
  and a first validated block whose first seq is not the pack's `seq_start` (§5),
  or, when a failed block precedes it, is not above `seq_start`, since a failed block holds at least one record.
  Footer validation (§10) rejects an index that states the I-12 or the I-13 defect,
  so in a finalized pack they come with an invalid or disagreeing footer:
  the outcome is `finalized-inconsistent` when no earlier outcome applies, for that reason and not for the defect.
  It does not compare `seq_start` with the index, so a finalized pack whose only fault is the `seq_start` defect is `finalized-consistent`.
  The report names, by the offset of the block envelope and the block's position among the located blocks:
  every failed block, in file order, with its cause:
  `unknown-codec` for a block whose envelope, its agreement with its F-2 entry where it has one, and its body CRC hold, and whose codec is outside the registry,
  and `corrupt-block` for every other failure, so a block that fails one of those checks is `corrupt-block` whatever its codec;
  the point where the walk stopped, as the offset of the envelope it could not account for;
  every validated block whose records disagree with its F-2 entry or its F-3 summary, once per block, in file order;
  whether the walked blocks disagree with the trailer's `block_count` or `record_count`,
  compared only for a finalized pack without a valid footer whose forward walk did not stop, and reported as no disagreement in every other pack;
  and every writer defect, by kind — `field-validity` and `event-field-validity` (§7.2), `event-payload` (§8), `seq-order` (I-12), `hour-span` (I-13), `seq-start` (§5) —
  with the seq of its record, or of its block's first record for `seq-start`, `seq-order` and `hour-span`;
  writer defects are listed block by block in file order,
  within a block `seq-start`, then `seq-order`, then each record's `field-validity`, `event-field-validity` and `event-payload` in record order, then `hour-span`.
  It states whether the pack is finalized (I-5) and where the validated prefix ends:
  at the first failed block or the point where the walk stopped, else at the end of the last block, or at the end of the pack metadata for a pack without blocks.
  With a valid footer it also gives the seq ranges and time intervals of the failed blocks, from their F-2 entries and F-3 summaries.
  A file header or pack metadata that cannot be read (§4, §5, §14) is an error, not an outcome: there is no block region to walk.
  A reader rejects a pack at bootstrap for exactly these reasons of format:
  an object shorter than the file header; a bad `magic`, `header_crc` or version (§4, §14);
  pack metadata extending past the object, or failing `pack_metadata_crc`;
  pack metadata whose entry list breaks one of §5's rules for entry lists (framing, the type, value and repetition rules of known tags, nested lists and the required tags of their registries);
  a missing tag whose "Required when" condition the pack metadata alone decides (`always`, or a condition on another metadata value, such as `time_source = capture-clock`);
  and a `replacement_set_size` other than 1 or a `replacement_set_index` other than 0.
  [CORPUS §5.3] names each of these reasons by a code.
  Every other requirement of §5, a condition on the records or a value tied to the role (`scope_generation` 0 for a segment) among them,
  binds writers and never rejects a pack at bootstrap; the retired `pack_role` 5 is read as `unknown(5)` (§9).
  The checks [SEM §8] makes at bootstrap on redaction entries report recoverable defects, a present `siv` whose length is not 16 among them; they are not rejections.
  A missing `siv` on an encrypted data record's entry, and a `siv` on an annotation's entry, are checked when the record is read and its kind is known ([SEM §8]), and are recoverable too.
  A reader's own limits (a metadata size budget), an I/O error or a cancellation are failures of the read, not rejections of the pack.
  Guarantee: every validated block is readable, and the validated prefix is readable without the footer;
  every failed block and every byte range the walk cannot account for is reported `incomplete` with offset and cause.
  Locating blocks after an envelope the walk cannot account for, in a pack without a valid footer, is deferred ([OVW §6]).
- **Normal reads never silently skip.** A corrupt block, a truncated tail, an unknown codec, a block whose records disagree with its F-2 entry or a `coverage` hit yields partial results
  **with** an `incomplete` status the caller must inspect.
  [SEM §7.4] defines when a read of one pack reports each of these, and with which offset.
  When a read or lookup detects that a block's records disagree with its F-2 entry, or that a footer value disagrees with the records it describes,
  it guarantees only that its result is `incomplete` and names that defect;
  its outcome, its other reasons and every other fact it reports are best-effort (G5-203).
  Every other defect, such as a failed block, an unknown codec, a truncated tail or a block over a reader limit, keeps the guarantee above:
  validated blocks are read, and failed or unread ranges are reported.
  A false summary in a block that is never read is not detected ([SEM §7.4]).

## 14. Versioning

- `format_major.format_minor` in the file header.
  Readers accept any minor within their major;
  a newer minor may contain tags, enum values and record-header bytes the reader does not know,
  which it handles per §5 (skip unknown tags), §1 (preserve unknown enum values) and §7.1 (preserve unknown record-header bytes).
- An unknown major is rejected before any block is read.
- `trailer_version` (§11) and `footer_layout_version` (§10) are 1 within format major 1, and changing either is a major change;
  a reader that finds another value treats the trailer or the footer as not valid, and reads the pack by the forward walk (§13, G5-212).
- Minor changes: new TLV tags, new enum values (readers preserve unknown values), meanings for reserved bits,
  new record-header fields appended behind the known prefix, up to `record_header_len` = 65535 (§7.1),
  new F-3 / F-5 tags, a longer `f2_entry_len` (§10).
- Major changes: changing any existing offset or type, and **new codec values** (§2), because an old reader cannot decode the content at all.
- A new `classifier` is not a format change.
- The masked record and the `redaction` entries [SEM §8] gives for one record as received, one policy and one key set are fixed within a major:
  changing them is a major change; a correction that brings a masker back to [SEM §8] is not a format change, and the spec changelog states it (G5-268).
- Spec v2.32 changes no byte of format 1.0 and no canonical export (§15):
  no tag, enum value or bit is retired, and no requirement of §5 changes.
  It changes only the meaning tracepack attaches to some tags
  and which of them the reference implementation still writes (§5, §9; G5-201, G5-202).
- Spec v2.33 defines **format 1.1**: it changes no byte of format 1.0 and no export of a format 1.0 pack (§15).
  It adds the pack metadata tag `thinning`, the nested tags `encryption_algorithm` and `encryption_key_id` of `redaction_policy` and `siv` of `redaction` (§5),
  the enum `encryption_algorithm` and the `quality` bit 7 `unobserved` (§9),
  and it lets a pack whose role is not `extract` hold masked records ([SEM §8], [STO §4]).
  Each addition is known only in a pack of format 1.1 or later (§5); a reader reads a pack of a newer minor with the newest registry it knows.
- A writer writes `format_minor` 1 exactly when the pack holds a **format 1.1 feature**, and 0 otherwise:
  a record carrying `quality.redacted`, or a `redaction` entry, in a pack whose `pack_role` is not `extract`;
  a `redaction_policy` with the encryption tags, or a `redaction` entry with `siv`;
  a `thinning` entry; or a record carrying `quality.unobserved`.
  The seq 2^63 − 1 and the `stop-unclean` boundary of a consumer's close need no minor 1 by themselves ([STO §4]);
  a close segment's minor follows this rule like any pack's.
  A pack of minor 1 that holds no format 1.1 feature breaks this writer rule, but is valid, and is read as minor 1.
- A merge writes the largest `format_minor` of its inputs.
  It refuses an input of a minor newer than it implements, since it drops unknown top-level entries ([STO §4] Merge), which a newer minor may define as capture-level;
  and an input whose preserved bytes would gain, under the output's minor, a meaning they lack under its own:
  in format 1.1, a record of a minor-0 input carrying `quality` bit 7, reserved there and `unobserved` in minor 1.
  A merge never clears a bit or rewrites record bytes, so it refuses instead.
- Deployment precondition: tracepack v0.4.0 and earlier merge without these checks.
  Given 1.1 inputs, such a merge writes a minor-0 archive that silently drops a `thinning` entry;
  it fails on an input holding a `redacted` record, a commitment breach to it,
  but merges a policy-bearing input that holds none — a segment whose policy matched nothing, or the close segment of a masked capture — and drops its `redaction_policy`.
  So packs of format 1.1 are merged only by a merger of format 1.1, tracepack v0.5.0 or later.
- *Informative*: what a reader of format 1.0, tracepack v0.4.0, does with a pack of format 1.1, which it accepts:

  | Format 1.1 feature | A reader of format 1.0 |
  |---|---|
  | masked records in a segment or archive | reads `quality.redacted` and the entries' ranges as stored; it checks no range against its record and marks nothing wholly masked; the ciphertext is exported as payload bytes |
  | `encryption_algorithm`, `encryption_key_id`, `siv` | unknown nested tags, skipped and never checked: a missing or ill-sized `siv` goes unseen |
  | `thinning` | an unknown top-level tag, preserved and exported under `unknown`; its merge drops it |
  | `quality.unobserved` | a reserved bit, preserved, exported as `bit(7)`; the zero-filled message text reads as data with `decode_status` `not-attempted` |
  | a `masked_ranges` value whose length is not a multiple of 8, whose element count is odd, or holding an element above 2^63 − 1 | rejects the pack at bootstrap, where a reader of format 1.1 reports a recoverable defect ([SEM §8]) |
  | every other entry defect of [SEM §8] — an empty array, a zero length, unsorted, overlapping or adjacent ranges, a range past the payload, a redacted record without entry, an entry naming a record without `redacted`, overlapping encrypted entries of one record | not detected |
  | a policy-bearing pack with no `redacted` record, in its merge | merges it and drops `redaction_policy` |

  So its acceptance of a pack of format 1.1 is no evidence that the defect handling of format 1.1 took place.
  The unobserved row is the one silent degradation: a consumer of a reader of format 1.0 that ignores `decode_status` can take the zeros for traffic.
- **One exception, before any production use, now closed.**
  In spec v2.13, format 1.0 was redefined in place, without a new format number:
  the header section became byte-columnar (§6),
  and the record header lost its copies of the HSMS message header and became 44 bytes (§7.1).
  Packs written under the earlier definition — by tracepack v0.1.0, the only writer of it — are **unsupported**.
  Deployment precondition: no such pack is stored where a reader of the redefined format reads.
  A reader cannot tell the two definitions apart by version, so it does not promise to detect an old pack.
  An old block of one record passes I-2, because `seq` and `payload_len` kept their offsets,
  but its fields from byte 36 on are misread;
  a larger old block usually fails I-2, because its seqs are gathered from mixed bytes, and is then reported `corrupt`,
  but that detection is not guaranteed.
  The redefinition stays open until the first published release of a reader of the redefined format:
  until then a spec version may change format 1.0 in place again,
  and its changelog entry records the dated evidence that no such reader has been published.
  Spec v2.17 did so: `classifier` and `max_frame_len` became repeatable,
  and `compacted_from` became absent from a pack whose lineage list is empty (§5).
  A reader written for spec v2.13 to v2.16 rejects a pack that uses either change (§5 rules),
  while every pack a writer of those versions wrote is a valid pack of v2.17.
  Once a reader of the redefined format is published, the exception is closed, and no later change uses it.
  It closed on 2026-10-10, when tracepack v0.2.0, a reader of the redefined format, was published (G5-212).

## 15. Canonical JSONL export

The canonical JSONL export is the language-agnostic text form of a pack and its migration path.
- The first line holds the file header and every pack metadata entry, unknown tags included (tag number, value type, raw value as base64).
- Then one JSON object per record carrying every §7.1 field by its spec name,
  enum values by name (`unknown(<n>)` for unknown values), bit sets as arrays of names,
  the payload as base64, and transport-event / annotation bodies as objects keyed by tag name.
- The schema is versioned: `tracepack-jsonl/1` for a pack of format 1.0, `tracepack-jsonl/2` for every later minor (G5-233);
  its byte-exact form is defined in `tracepack-jsonl.md` [JSONL].
- It holds the records of the validated blocks; a failed block is left out without a mark, and the `verify` report says what was lost ([JSONL §7]).
  A pack rejected at bootstrap (§13) has no export.
- A query service offering downloads returns either native packs (`archive` packs, or `extract` packs with `extract_filter`, one per capture) or this export;
  to a consumer that is not privileged it returns only extracts written under a redaction policy, or their export ([SEM §8]).
- It is the expected output of the conformance corpus (§16),
  so any two conforming readers produce identical JSONL for the same pack.

## 16. Conformance corpus

The corpus binds format-level reading (G5-199):
it pins what a reader of format 1.0 and 1.1 reads from each of its packs, so that drift of the format is caught over years of storage.
The format is documented for any reader, and the Go module is the only implementation (G5-198);
diagnostics, reads over several packs, transaction lookups and store behaviour are tested by the Go module's own tests, not by the corpus ([SEM §9], [STO §8]).
Its layout, the JSON schemas of its expectations and the catalogue that maps each clause below to its vectors are [CORPUS];
[CORPUS §8] states how the corpus reads the clauses whose wording leaves a choice, such as the empty pack and the maximum-value integers.
- Contents: golden `.tpk` files and the expected results of reading them; the files of each vector are those [CORPUS §2] gives for its class (G5-210).
  A vector holds the expected canonical JSONL (§15) of its pack,
  or, for a vector a reader rejects at bootstrap (§13), the expected rejection instead ([JSONL §8]);
  a truncation vector holds one pack and the expected rejection or export of each cut.
  A vector may also hold the HSMS header fields of its data and control records (§7.2)
  and a projection of its footer (§10): the values the footer stores, whether a reader accepts it, and the values recomputed from the records.
  Rejections, fields and footer projections are canonical JSON that holds codes, offsets, seqs and counts, never error text ([CORPUS §5]).
- The corpus holds no `verify` report, query result or classification (G5-210).
  Where a clause below states a `verify` outcome or writer defect, the result of a query or predicate, or a `decode_status`,
  the corpus holds the pack with the expectations above, and the Go module's tests check the stated result.
- Vectors: empty pack; pack with zero records; codec `none` and codec `zstd` of the same records;
  truncated tail; corrupt middle block; bad envelope CRC; unknown codec; unknown TLV tag and enum value;
  `record_header_len` > 44, including 45–55, with the extension bytes preserved; unordered timestamps;
  maximum-value integers (§2), at the limits and past them; UUID byte order; the CRC check value;
  CRC-valid but structurally invalid footers, one per clause of the footer validation, and a block whose absent `seq_range` hides a missing seq (§10 footer validation);
  a footer with retired F-3 tags and with a present F-4, read with both ignored (§10);
  an epoch ended twice, by a socket-close event and then a clean `stop`, in one block and across two blocks:
  an F-3 entry's `close_seq` is the lowest closing seq of its own block, and F-5's the lowest of the pack (§10),
  so in one block F-3 and F-5 state the lower seq, and across two blocks each block's F-3 entry states its own record's seq and F-5 the lower one;
  an F-5 that states the higher one is invalid (§10);
  in one block, an F-3 entry that states the higher one, with F-5 recomputed from it, makes `verify` report `finalized-inconsistent` (§10, §13);
  a record that carries quality bit 0 without being a capture-boundary record: no `boundary` entry (§10);
  a transport-event record whose payload is not a valid TLV body: no `boundary` entry, no `close_seq`, and a writer defect (§10, §13).
- HSMS header field vectors (§7.2):
  short captures ending before and at the last byte of each field (payloads of 5 to 14 bytes):
  each field available exactly when its bit is set, and an unavailable field never matched unless unavailable fields are requested;
  a data record whose System Bytes bit is clear while its payload holds them (a log conversion without source System Bytes),
  queried for those payload bytes: never matched unless unavailable fields are requested;
  a record whose System Bytes bit is set while its payload ends before them: rejected by a writer at append,
  and, in a nonconforming-writer fixture labelled as such, unavailable to a query and reported by `verify` as a writer defect;
  control frames (`Select.rsp`, `Deselect.rsp`, `Reject.req`, `Linktest.req`) whose payload bytes 6 and 7 a query reads at their positions, as stream, W and function, whatever SType says (§7.2);
  S/F, SessionID and System Bytes predicates evaluated on payload values,
  and an S/F predicate on a record with one of the two fields unavailable, with and without the request for unavailable fields.
- Block framing vectors (§6, §7.1):
  identical decoded body bytes under different framing, as one record with a long header or as several records with 44-byte headers;
  a block of several records whose seqs, lengths and extension bytes are checked on the gathered headers;
  `record_header_len` below 44; `record_count × record_header_len` overflow; an envelope / F-2 `record_header_len` mismatch;
  a pack mixing blocks of different header lengths;
  a block whose header section is valid but whose codec stream is short or malformed after it: `corrupt` (the frames are built by hand, [CORPUS §6.2]);
  a block holding one oversized record.
- Validation vectors (§12):
  a validating writer whose encoded block has an I-2 defect injected after encoding:
  the block and the trailer are not written, and recovery reports the pack unfinalized.
- Verification vectors (§13), each with its expected outcome, which the Go module's tests check;
  each pack's export shows what a reader recovers from it:
  a finalized pack whose blocks all pass: `finalized-consistent`;
  a pack truncated at every byte offset, stored as one pack and a table of the expectations of each cut, which an implementation makes itself ([CORPUS §5.7]):
  rejected at bootstrap (§13) when the cut falls inside the file header or the pack metadata,
  else exported with no block of the validated prefix lost (its outcome `unfinalized`);
  a finalized pack whose last block fails: `finalized-truncated`;
  a failed block between validated ones, with a valid footer and, for a block whose envelope holds, without one: `corrupt-middle`;
  a finalized pack whose footer is invalid, or whose F-3 summary disagrees with its block's records, while every block passes: `finalized-inconsistent`;
  a writer defect of each kind in a validated block: reported, never changing the outcome by itself;
  a first block whose first seq is above, and one whose first seq is below, `seq_start`,
  and a first validated block after a failed one whose first seq equals `seq_start`: the `seq_start` defect;
  the seq-order and hour-span defects of a finalized pack come with an invalid or disagreeing footer, so `finalized-inconsistent`.
- Bootstrap vectors (§13): for each reason a reader rejects a pack at bootstrap, a pack with that reason alone,
  the reason of an entry list breaking §5's rules once per kind of rule (framing, type, value, repetition, nested list),
  and a missing tag once required `always` and once by a condition on another metadata value;
  and a pack missing a tag that only its records require (a classified record without `classifier`): it opens, and its export holds its records.
- Redaction vectors ([SEM §8]), written with the published test keys, by a writer masking at write under a policy with encryption unless stated:
  S7F3 with its PPBODY masked (length, item headers, `decode_status` and HSMS header unchanged; entry, digest and `siv` as published);
  S7F3 and S7F6 carrying the same process program, in one domain: equal digests; the same S7F3 under the second digest key and key id: a different digest;
  one PPBODY in two captures: equal digests, and different ciphertexts and `siv`s;
  a formatted process program (S7F23) masked through path `4`: every CCODE and PPARM leaf masked, every list header kept;
  an S7F3 and an S6F11 carrying the same message text: only the S7F3 is masked, selected by the S/F of its payload;
  S7F3 frames whose declared length is shorter than the capture (an item after the declared end) and longer (a truncated capture): the captured text is screened, never the declared extent;
  an oversized S7F3, and records whose payload is shorter than 14 bytes: screened by the same rule, and never masked when there is no message text;
  an S7F3 whose message text is not one valid item, one with trailing bytes, and a well-formed one whose rule path does not resolve:
  one whole-text entry each, in `message-text`, without `item_path`, its ciphertext opaque;
  an S13F6 whose fourth item is not a list, and an S16F15 in which one job lacks element `4/3`: whole-text masks (a failed branch fails the pattern);
  an S13F6 whose data list is empty: nothing masked, no entry;
  empty-list denials of S7F6, S7F26 and S7F36: not masked;
  a zero-length PPBODY: no entry; a header-only S7F3 data message: no entry;
  an S16F11 whose RCPPARVAL is a list holding a format-22 leaf: the localized string header kept, the rest masked; a format-22 leaf of 2 bytes: no target bytes; one of 1 byte: whole-text mask;
  an S13F6 carrying a data set: every FILDAT masked;
  a PPBODY whose content is already all zero: masked ranges still list it, and its restore gives it back;
  an `unparsed-entry` annotation masked through its `raw` value: zeros, its entry without `siv`;
  a pack under a policy that matched nothing: `redaction_policy` present, `redaction-present` clear, no entries;
  malformed entries (empty or odd ranges array, zero length, unsorted or adjacent ranges, a range past `payload_len`, `seq` of a record without `redacted`, a `redacted` record without entry,
  a missing and an ill-sized `siv`): the pack opens, and exactly the records concerned are treated as wholly masked;
  encrypted entries of one record that overlap, one an exact duplicate, and entries that are only adjacent: the pack opens, and the overlapping records are wholly masked;
  a valid masked record in an early block and a defective annotation entry for a record in a later block: the early record keeps its ranges, the later one is wholly masked;
  masked records merged from two segments, one record in both: one entry;
  a masked pack truncated at every byte offset, and one with a corrupt block;
  a `redaction` entry without `seq`: rejected at bootstrap.
  Deferred with extracts (G5-198, G5-244): an extract written without a policy;
  an extract requested from an extract written under a policy, or from a pack holding a `redacted` record, rejected before any output;
  and one requested from an unscreened extract, written because its records are in full.
- Format 1.1 vectors beside the redaction vectors:
  records unobserved by design with their `thinning` entries, beside a reply whose message text is empty and an abnormal reply, both kept in full ([SEM §2]);
  a consumer's close at seq 2^63 − 1 of a plain capture, of a masked and thinned one, of one whose capture staged no record, and merged with its capture's records ([STO §4]);
  and the version-aware cases (§5, §14): a format 1.0 segment holding tag 0x0033 and `quality` bit 7, a format 1.0 extract holding the nested tag numbers format 1.1 assigns,
  format 1.0 extracts under a policy without a `classifier` and with several, and a readable pack of minor 2.
- [SEM §9] adds the single-pack vectors of its rules to the same corpus;
  reads over several packs, transaction lookups and store behaviour have no corpus vectors (G5-199).
- Conformance is [CORPUS §1]: in short, a reader produces the expected files [CORPUS §1] lists for each vector's class
  and the expected rejection for every vector rejected at bootstrap (§13),
  and a writer's output round-trips through a conforming reader.
- The corpus is identified by the spec version as well as the format version,
  so the goldens of spec v2.13 and later are told apart from earlier ones (§14) ([CORPUS §3]).
  It holds one sample of the definition before v2.13:
  a tracepack v0.1.0 pack with a multi-record block, which a reader reports `corrupt`,
  labelled as a sample of that layout, not as a detection guarantee.

## Appendix A. Cross-language support (informative, as of 2026-09)

Informative history: no implementation in another language is planned (G5-198);
the table records the survey made when the format was designed.

| Need | Go | Java | .NET | Python |
|---|---|---|---|---|
| zstd (RFC 8878) | `klauspost/compress/zstd` (pure Go) | `zstd-jni` (native; used by Kafka, Parquet, Spark) or `aircompressor` (pure Java; its pure-Java compressor supports only the default level) | .NET 11 BCL `ZstandardStream` (GA expected 2026-11); `ZstdSharp` (pure C#) for .NET 8 / 10 LTS | stdlib `compression.zstd` (3.14+) |
| CRC-32/ISO-HDLC | `hash/crc32` IEEE | `java.util.zip.CRC32` | `System.IO.Hashing.Crc32` | `zlib.crc32` |
| UUID byte order (§2) | `[16]byte` as-is | `UUID` most / least significant longs, written big-endian | **not** `Guid.ToByteArray()` (mixed-endian); use `Guid.TryWriteBytes(span, bigEndian: true, out _)` (.NET 8+) | `uuid.UUID.bytes` |
| unsigned 64-bit | native | `long` within the §2 ≤ 2^63 − 1 limit | native | native |
| AES-SIV-CMAC, RFC 5297 (added 2026-10) | Tink's raw primitive `daead/subtle` of `github.com/tink-crypto/tink-go/v2`, the module's choice (G5-245), 64-byte keys only; none in the standard library | Tink `subtle.AesSiv`; none in the standard library |  | Tink `tink.daead` |

Codec alternatives considered: Deflate/gzip is in every standard library but compresses clearly worse and slower;
LZ4 is fast but compresses less, and its main Java library changed maintainers in 2025 after CVEs,
which weighs against it for archival use.
