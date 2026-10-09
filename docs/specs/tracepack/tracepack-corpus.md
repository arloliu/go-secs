# tracepack — conformance corpus

Status: current (2026-10-10) — v2.29, tracepack format 1.0, schema `tracepack-corpus/1` (draft until tracepack v1.0.0).
Normative, language-agnostic. [FMT §16] states what the corpus holds; this document defines its files, their schemas and the vector catalogue.

Depends on: [FMT §2] portable encoding, [FMT §5] TLV registries, [FMT §7.2] HSMS header fields, [FMT §10] footer and its validation,
[FMT §13] bootstrap, verification and repair, [FMT §16] corpus contents, [JSONL] value forms and the export,
[SEM §3] `decode_status`, [SEM §7.4] queries.

References: `[CORPUS §n]` = this document; `[FMT §n]` = `tracepack-format.md`, `[JSONL §n]` = `tracepack-jsonl.md`, `[SEM §n]` = `tracepack-semantics.md`, `[STO §n]` = `tracepack-storage.md`, `[OVW §n]` = `tracepack-overview.md`; `[FMT I-n]` = invariant I-n of the format document.
Each rule is defined in exactly one document; the others only reference it.

## 1. Conformance

An implementation proves conformance against the committed files of the corpus; it never needs to regenerate them.
JSON files are compared as JSON values (§4); `.jsonl` and `.tpk` files are compared byte for byte.
- A **reader** conforms when, for every vector it is given:
  - of class `read`, `repair` or `truncation`, it opens `pack.tpk`, its export equals `export.jsonl` ([JSONL]),
    and its verification of the pack ([FMT §13]) projects to `verify.json` (§5.2);
  - of class `rejection`, it rejects `pack.tpk` at bootstrap for the reason `rejection.json` names (§5.3);
  - with `queries.json`, each query yields the expected records and `incomplete` reasons (§5.4);
  - with `fields.json`, it reads the HSMS header fields of each data and control record as listed (§5.5);
  - with `footer.json`, it accepts or rejects the footer as `accepted` says, and, where it reads footer values, reads them as `stored` lists and computes them from the records as `recomputed` lists (§5.8);
  - of class `truncation`, every cut of `pack.tpk` meets its row of `truncation.json` (§5.7);
  - with `patch.tpk`, it reads the patch as it reads `pack.tpk`, against `patch.jsonl` and `patch.verify.json`.
- An implementation of repair ([FMT §13]) conforms when, for every `repair` vector and every repair row of a truncation table,
  with the options `repair.json` or `truncation.json` gives, its result is the expected one (§5.6),
  and its patch, when it writes one, has the properties of [FMT §16] Repair vectors and holds the expected blocks, records and new `coverage` entries.
  `patch.tpk` is the reference implementation's patch; another implementation's patch may differ from it in the order of its pack metadata entries and so in its footer offsets.
- A **classifier** conforms when it computes, for every frame `classify.json` lists, the listed `decode_status` and `trailing_bytes` under the listed `max_frame_len` (§5.9).
- A **writer** conforms when a conforming reader round-trips its output.
  The corpus holds one writer behaviour as bytes, the validating writer of `validation-i2-injection` (§9.4):
  another writer conforms when its output stops before the same block and holds no trailer.
- An implementation skips the files of an operation it does not offer (repair, classification, a reading of footer values);
  the files of the others still bind it.
- A reader's own limits, an I/O error or a cancellation are never goldens ([FMT §13], [JSONL §7]): no expectation depends on them.

## 2. Layout

The corpus is a directory holding `manifest.json` (§3), `primitives.json` (§5.1), a `README.md` (informative), and one directory per vector, named by the vector's `id`.
A vector directory holds files with these fixed names, and no others:

| File | Present | Content |
|---|---|---|
| `pack.tpk` | always | the vector's pack; for a `truncation` vector, the base pack every cut is taken from |
| `export.jsonl` | class `read`, `repair`, `truncation` | the expected export of `pack.tpk` ([JSONL]) |
| `rejection.json` | class `rejection` | the expected bootstrap rejection (§5.3) |
| `verify.json` | class `read`, `repair`, `truncation` | the expected verification report of `pack.tpk` (§5.2) |
| `queries.json` | optional | query vectors over `pack.tpk` (§5.4) |
| `fields.json` | optional | the HSMS header fields of each data and control record (§5.5) |
| `classify.json` | optional | the expected classification of byte-predicate frames (§5.9) |
| `footer.json` | optional | footer expectations (§5.8) |
| `repair.json` | class `repair` | the repair options and the expected result (§5.6) |
| `patch.tpk`, `patch.jsonl`, `patch.verify.json` | class `repair`, when `repair.json`'s result is `patched` | the reference patch, its export and its verification report |
| `truncation.json` | class `truncation` | the expectation table of every cut (§5.7) |

A vector of class `rejection` has no `export.jsonl` and no `verify.json`: a pack rejected at bootstrap has neither ([JSONL §8], [FMT §13]).

## 3. Manifest and identification

`manifest.json` identifies the corpus and lists its vectors.
The corpus is identified by its schema, the spec version and the format version together,
so goldens written under different definitions are told apart ([FMT §14]).

| Key | Type | Present | Value |
|---|---|---|---|
| `corpus` | string | always | `"tracepack-corpus/1"`, the schema of every file of this document (§10) |
| `spec_version` | string | always | the spec version the goldens follow: `"2.29"` |
| `format_version` | string | always | `"1.0"` ([FMT §4]) |
| `jsonl_schema` | string | always | the export schema of every `.jsonl` file: `"tracepack-jsonl/1"` ([JSONL §9]) |
| `zstd_encoder` | string | always | the identity and version of the zstd encoder that produced the encoder-made zstd blocks and footers of the corpus, as its generator records it (§6.1) |
| `vectors` | array of objects | always | one per vector, ascending by `id` |

A vector entry:

| Key | Type | Present | Value |
|---|---|---|---|
| `id` | string | always | kebab-case ASCII, prefixed by its group (§9.4); stable, never reused for another vector |
| `title` | string | always | one line, informative |
| `cites` | array of strings | always, never empty | the clauses the vector exercises, each `FMT §n`, `FMT I-n`, `SEM §n`, `STO §n`, `JSONL §n` or `CORPUS §n`, primary clause first; informative |
| `class` | string | always | `read`, `rejection`, `truncation` or `repair` (§2) |
| `labels` | array of strings | always, `[]` when none | ascending; `damaged`: the pack models damage after writing (a failed CRC, a cut, a broken codec stream); `nonconforming-writer`: the pack holds bytes, every CRC valid, that a conforming writer never writes; `pre-v2.13-sample`: the sample of the format definition before v2.13 ([FMT §14]) |
| `codec` | string | always | `none` when every block body and the footer of `pack.tpk` use codec `none`, `zstd` when they all use `zstd`, `mixed` otherwise, an unknown codec included |
| `source` | string | always | `generated`, or `fixed` for a file kept as committed and never regenerated |
| `files` | array of strings | always | the vector's files, in the order of the table of §2 |
| `cases` | array of objects | optional, never empty | single-record sub-vectors, ascending by `seq`: `{id, seq, cites}`, `id` kebab-case and unique within the vector, `seq` the record's seq (string), `cites` as above |

Vectors that differ only in codec (`basic-codec-none`, `basic-codec-zstd` and `basic-codec-mixed`; the truncation base packs under both codecs) carry the same `pack_id`, `capture_id` and other identifiers,
so their exports are identical.

## 4. JSON conventions

Every JSON file of the corpus is UTF-8 JSON text (RFC 8259) holding one object or array.
- Values take the forms of [JSONL §3]: `u64` and `i64` values as decimal strings (G5-167); `u8`, `u16` and `u32` values as numbers; bytes as standard base64 with padding;
  UUIDs in the canonical lowercase form; enum values and bits by their [JSONL §3] names, `unknown(<n>)` and `bit(<n>)` included.
- A block index and a count of blocks are numbers; a count of records is a string, since records are counted in `u64`.
- A **block index** is the 0-based position of a block among the blocks the reader locates ([FMT §13]), in file order.
- An **offset** is a file offset, in bytes from the first byte of the file the object describes (`pack.tpk`, `patch.tpk`, or a cut of `pack.tpk`), as a string.
- A **coverage object** is a `coverage` entry rendered as a [JSONL §4] TLV object under the `coverage` registry ([FMT §5]).
- An optional key is omitted when it has no value, never written as `null` and never given a placeholder such as `-1`.
- A key holding an array is present as its schema's Present column says, like any other key;
  a present array with no elements is `[]`, never omitted.
  A coverage object follows [JSONL §4] instead, so its `unknown` key is omitted when there is no unknown entry.
- Every array is in the order its schema states, so two conforming files are equal as JSON values;
  a key order is given for each object, which a comparison of JSON values ignores.
- The reference generator writes one fixed form — two-space indentation, keys in the order of the schema, no escaping beyond [JSONL §3]'s, a final LF —
  so it also compares its own output byte for byte.

## 5. Schemas

Each table lists an object's keys in order.

### 5.1 `primitives.json`

Values an implementation checks before it reads a pack.

| Key | Type | Present | Value |
|---|---|---|---|
| `crc` | array of objects | always | `{name, input, crc}`: a name (string), the input bytes (base64) and their CRC ([FMT §2]) as a number; the first entry is `check-value`, the input `123456789` in ASCII and the CRC 3421780262 (0xCBF43926), then the bytes 0–75 of a file header and the bytes 0–35 of a block envelope with their stored CRCs |
| `uuids` | array of objects | always | `{bytes, string, pack?}`: 16 UUID bytes in [FMT §2] byte order (base64), the canonical string they denote, and, when a vector's pack has that UUID as its `pack_id`, the vector's id; ascending by the 16 bytes, compared as unsigned bytes from the first |

### 5.2 `verify.json`

The verification report of [FMT §13], for a pack it opens.

| Key | Type | Present | Value |
|---|---|---|---|
| `outcome` | string | always | the outcome's [FMT §13] name |
| `finalized` | bool | always | I-5 holds |
| `footer_valid` | bool | always | the pack is finalized and its footer passes [FMT §10] validation |
| `blocks_located` | number | always | the blocks located ([FMT §13]) |
| `blocks_validated` | number | always | the located blocks that did not fail |
| `records` | string | always | the records of the validated blocks |
| `prefix_end` | string | always | where the validated prefix ends ([FMT §13]), an offset |
| `failed_blocks` | array of objects | always | `{block, offset, cause}` per failed block, ascending by `block`; `offset` the block's envelope, `cause` `corrupt-block` or `unknown-codec`, as [FMT §13] assigns them |
| `walk_stop` | string | when the forward walk stopped at an envelope it could not account for | the offset of that envelope |
| `disagreeing_blocks` | array of objects | always | `{block, offset}` per validated block whose records disagree with its F-2 entry or its F-3 summary, once per block, ascending by `block`; the two disagreements are not told apart |
| `trailer_totals_disagree` | bool | always | `true` when [FMT §13] compares the walked blocks with the trailer's `block_count` and `record_count` and they disagree; `false` otherwise, also when no comparison is made |
| `writer_defects` | array of objects | always | `{kind, block, offset, seq}` per writer defect, in the order [FMT §13] gives; `kind` its [FMT §13] name, `offset` its block's envelope, `seq` the record's, or the block's first seq for `seq-start`, `seq-order` and `hour-span` |
| `lost` | array of coverage objects | always | with a valid footer, one per seq range of each failed block, from its F-3 summary (or `first_seq`..`last_seq` without `seq_range`), ordered by block, then by `seq_first`: `capture_id` the pack's, `seq_first`, `seq_last`, and `time_start` / `time_end` the block's F-2 `ts_min` / `ts_max`; `[]` without a valid footer |

### 5.3 `rejection.json`

`{"rejection": "<code>"}` for a pack rejected at bootstrap, one code per reason of [FMT §13]:

| Code | [FMT §13] reason |
|---|---|
| `short-object` | an object shorter than the file header |
| `bad-magic` | a bad `magic` |
| `header-crc` | a bad `header_crc` |
| `unsupported-version` | a version the reader does not support ([FMT §14]) |
| `metadata-past-object` | pack metadata extending past the object |
| `metadata-crc` | pack metadata failing `pack_metadata_crc` |
| `metadata-entry-list` | pack metadata whose entry list breaks one of [FMT §5]'s rules for entry lists: framing, the type, value and repetition rules of known tags, a nested list, a nested registry's required tag |
| `metadata-required-tag` | a missing top-level tag required `always`, or by a condition the pack metadata alone decides |
| `replacement-set` | a `replacement_set_size` other than 1 or a `replacement_set_index` other than 0 |

Every rejection vector has exactly one of these causes, so no precedence among them is defined or tested.

### 5.4 `queries.json`

An array of query vectors over `pack.tpk`, ascending by `id`, each a read of that one pack ([SEM §7.4]):

| Key | Type | Present | Value |
|---|---|---|---|
| `id` | string | always | kebab-case, unique within the vector |
| `cites` | array of strings | always | as in the manifest (§3) |
| `filter` | object | always | the selection, below; `{}` selects every record |
| `expect` | object | always | `{seqs, incomplete}`, below |

`filter` keys, each optional; an absent key places no constraint, and an array is never empty:

| Key | Type | Value |
|---|---|---|
| `time_from`, `time_to` | strings (`i64`) | `ts_utc_ns` in the half-open range [`time_from`, `time_to`) |
| `kinds` | array of strings | `kind` among these names, ascending by the enum's numeric value ([FMT §9]) |
| `dirs` | array of strings | `dir` among these names, ascending by the enum's numeric value ([FMT §9]) |
| `epochs` | array of numbers | `epoch` among these, ascending |
| `sf` | array of objects | `{stream, function}` numbers, ascending by stream, then function: stream and function among these pairs |
| `session_ids` | array of numbers | SessionID among these, ascending |
| `system_bytes` | string | the 4 System Bytes (base64) |
| `include_unavailable` | bool | present only as `true`: the explicit request for records with unavailable fields ([FMT §7.2]) |

A record is selected when it satisfies every key present.
The HSMS header predicates (`sf`, `session_ids`, `system_bytes`) read the payload's fields at their positions, with their availability, as [FMT §7.2] defines.

`expect` keys:

| Key | Type | Value |
|---|---|---|
| `seqs` | array of strings | the seqs of the selected records, in the order the read yields them: blocks in file order, records in block order |
| `incomplete` | array of objects | `{reason, block?, offset?, coverage?}`, by the decision table below; first every `coverage` entry in pack metadata order, then the pack-level `truncated`, then the block defects ascending by `block` |

When a query reports each reason (decision table):

| Reason | Reported when | Keys |
|---|---|---|
| `coverage` | for every `coverage` entry of the pack metadata that meets the filter ([FMT §5] matching) | `coverage` |
| `truncated` | at most once, the first case that applies: (1) the forward walk stopped at an envelope it could not account for; (2) else the pack is not finalized; (3) else the walked blocks disagree with the trailer's `block_count` or `record_count`, as [FMT §13] compares them | `offset`: (1) that envelope; (2) the end of the last walked block, or the end of the pack metadata without blocks; (3) none |
| `corrupt-block`, `unknown-codec` | for every block the read reads that fails ([FMT §13]), by its cause | `block`, `offset` (the envelope) |
| `index-mismatch` | for every block the read reads whose records disagree with its F-2 entry ([FMT §10]) | `block`, `offset` |

A block whose records disagree only with its F-3 summary reports nothing.
A finalized pack whose footer is rejected, but whose forward walk accounts for every block and agrees with the trailer's totals, reports nothing: the footer's rejection alone is not incomplete.
Whether a reader skips a block that F-2 or F-3 excludes is not part of the contract,
so a query over a pack with a failed or disagreeing block uses a filter that no F-2 entry or F-3 summary excludes, and every block is read whatever the reader prunes.
A reader's own limits never appear in `incomplete` (§1).

### 5.5 `fields.json`

An array with one object per data or control record of `pack.tpk`, ascending by `seq`, giving the HSMS header fields of its payload as [FMT §7.2] reads them:

| Key | Type | Present | Value |
|---|---|---|---|
| `seq` | string | always | the record's seq |
| `available` | array of strings | always | the [JSONL §3.2] `field_validity` names of the available fields, ascending by bit; `[]` when none |
| `session_id` | number | iff `session_id` is available | payload bytes 4–5 |
| `stream`, `w` | number, bool | iff `stream_and_w` is available | payload byte 6, bits 0–6 and bit 7 |
| `function` | number | iff `function` is available | payload byte 7 |
| `ptype`, `stype` | numbers | each iff available | payload bytes 8 and 9 |
| `system_bytes` | string | iff `system_bytes` is available | payload bytes 10–13 (base64) |

The fields are positional for data and control records alike ([FMT §7.2]):
a control record's `stream`, `w` and `function` are its bytes 6 and 7 as stored.

### 5.6 `repair.json`

| Key | Type | Present | Value |
|---|---|---|---|
| `options` | object | always | `{writer, pack_id, patch_base?, codec, writer_start_utc_ns}`: the patch's `writer` tag, its `pack_id`, its `patch_base` when the scope has a generation, its footer codec (`"none"`), and the time it stamps as `writer_start_utc_ns` (string) |
| `result` | string | always | `patched`; `not-needed` when the pack has nothing to repair; `not-repairable` for any other refusal ([FMT §13]) |
| `blocks` | number | iff `patched` | the blocks copied |
| `records` | string | iff `patched` | the records copied |
| `coverage_added` | array of coverage objects | iff `patched` | the new `coverage` entries of the patch ([FMT §13]), not the entries it inherits; when [FMT §13] takes them from the footer, by failed block in file order, then ascending by `seq_first`; when it takes them from the lost runs, one per lost run in file order, the tail last |

The result is coarse: a refusal's cause is not part of it.
A refusal vector satisfies exactly one refusal condition of [FMT §13] where the conditions can be separated;
where they cannot, the vector cites each condition it satisfies, and the coarse result needs no winner among them.

### 5.7 `truncation.json`

The expectations for every cut of `pack.tpk`: the file of its first L bytes, for every L with 0 ≤ L < `size` (G5-178).
An implementation makes each cut itself.

| Key | Type | Present | Value |
|---|---|---|---|
| `size` | string | always | the size of `pack.tpk` |
| `repair_options` | object | iff the rows hold `repair` | the repair options, as `repair.json`'s `options` |
| `rows` | array of objects | always | ascending by `from`, contiguous, covering every L from 0 to `size` − 1; consecutive lengths with equal expectations are one row |

A row:

| Key | Type | Present | Value |
|---|---|---|---|
| `from`, `to` | strings | always | the cut lengths L the row covers, `from` ≤ L ≤ `to` |
| `rejection` | string | iff the cut is rejected at bootstrap | the code (§5.3) |
| `outcome` | string | iff not rejected | the cut's verification outcome |
| `exported_records` | string | iff not rejected | the records of the cut's export |
| `verify` | object | iff not rejected | `{blocks_validated, records, prefix_end, walk_stop?}`, as in `verify.json` |
| `repair` | object | iff not rejected, in a table with `repair_options` | `{result, blocks?, coverage_added?}`, as in `repair.json` |

For every cut that is not rejected, the records exported are those of the blocks that lie wholly before the cut,
and the cut's export is the header line of `export.jsonl` followed by that many of its first record lines: no block of the validated prefix is lost.

### 5.8 `footer.json`

The export leaves the footer out ([JSONL §5]), so a vector whose subject is a footer value publishes a projection of it in three parts:

| Key | Type | Present | Value |
|---|---|---|---|
| `stored` | object | iff the footer is structurally readable, below | the F-3 and F-5 values below, as stored |
| `accepted` | bool | always | the footer passes [FMT §10] validation, so its statistics are available; equal to `verify.json`'s `footer_valid` |
| `recomputed` | object | always | the same projection computed from the records of the validated blocks by the rules of [FMT §10] |

The footer is **structurally readable** when every check below holds;
it may still fail [FMT §10] validation, its aggregates disagreeing for instance, and `stored` then shows what it states:
- the pack is finalized ([FMT I-5]), and the footer passes `footer_crc` and decodes under `footer_codec` to `footer_uncompressed_len` bytes ([FMT §11]);
- the decoded footer holds the 72-byte F-1 prologue, `f2_entry_len` is at least 80,
  and F-2 (`block_count` × `f2_entry_len` bytes from `f2_offset`), F-3 (`f3_len` bytes from `f3_offset`) and F-5 (`f5_len` bytes from `f5_offset`) each lie inside the decoded footer;
- F-2 is read as its fixed layout ([FMT §10]), the first 80 bytes of each entry, and each entry's `summary_offset` and `summary_len` lie inside F-3;
- each F-3 list and F-5 pass the framing rules of [FMT §5];
- each `epoch` and `boundary` entry of these lists is of value type `tlv`, its value passes the framing rules,
  and the nested tags the projection reads pass the type rules of [FMT §5] for their registry type:
  an `epoch` entry holds exactly one nested `epoch`, a `u64` at most 2^32 − 1, and at most one `close_seq`, a `u64`;
  a `boundary` entry holds exactly one `seq`, a `u64`, and exactly one `boundary_kind`, a `u8`.

No other value is checked, and when a check fails `stored` is omitted.

The projection: `{blocks, f5}`.
`blocks` holds one object per F-2 entry (in `stored`) or per validated block (in `recomputed`), ascending by `block`:
`{block, close_seqs, boundaries}`, `close_seqs` the F-3 `epoch` entries' `close_seq` values as `{epoch, seq}`, entries without `close_seq` left out,
and `boundaries` the F-3 `boundary` entries as `{seq, kind}`, `kind` the `boundary_kind` name.
`f5` is `{epochs, boundaries}`: `epochs` one `{epoch, close_seq?}` per F-5 `epoch` entry; `boundaries` as above.
Every entry gives one object, so entries that repeat an epoch or a seq are all listed.
`close_seqs` are ascending by `epoch`, then by `seq`;
`epochs` ascending by `epoch`, an object without `close_seq` before those with one, then by `close_seq`;
`boundaries` ascending by `seq`, then by the numeric `boundary_kind`; objects equal in every key are interchangeable.
`seq` and `close_seq` are strings, `epoch` and `block` numbers; arrays are `[]` when empty.
Other footer fields are outside the projection.
F-3's `close_seq` is the block-local minimum and F-5's the pack-wide minimum ([FMT §10]):
an epoch ended at seq 4 in block 0 and at seq 7 in block 1 has `close_seq` 4 and 7 in the blocks' F-3 entries and 4 in F-5.

### 5.9 `classify.json`

The classification of the frames whose `decode_status` is a byte predicate of [SEM §3] (`short-frame` to `ok`):

| Key | Type | Present | Value |
|---|---|---|---|
| `max_frame_len` | string | always | the pack's single `max_frame_len`, the ceiling every frame is classified under; a pack with `classify.json` carries exactly one ([SEM §3]) |
| `frames` | array of objects | always | `{seq, rule, decode_status, trailing_bytes}` per frame, ascending by `seq`: the record's seq, the id of the case it exercises (§7, or a [SEM §3] row by its status name), the expected status name and `trailing_bytes` (number) |

The expectations are written from [SEM §3] and §7, never computed by a classifier.
`trailing_bytes` is the excess for `ok-with-trailing` and 0 for every other status:
[FMT §7.2] gives the field no meaning outside `ok` and `ok-with-trailing`, so a classifier's count for a failing status is compared as 0.
The stored `trailing_bytes` of the records, and so their export, are not changed by this comparison rule.
The statuses that are not byte predicates (`not-attempted`, `reconstructed-ok`, `parse-failed`, `build-rejected`, `not-applicable`) are stored values only:
their records are in the pack and its export, never in `classify.json`.

## 6. zstd vectors

Every vector uses codec `none` for its blocks and footer unless a codec is its subject.

### 6.1 Encoder versions

RFC 8878 fixes what a zstd frame decodes to, not the bytes an encoder produces: two versions of one encoder can encode the same input differently.
- **Reading** never depends on an encoder: every check of §1 runs against the committed files, whatever encoder produced them.
- **Regeneration**, the generator checking its own output against the committed files, compares by the encoder that produced them, which the manifest records as `zstd_encoder`:
  - when the generator runs with that encoder, every regenerated file equals the committed one byte for byte, the manifest included;
  - when it runs with another encoder, or cannot establish which one it runs with,
    each vector that depends on the encoder is compared by decoded equivalence — the same file header and pack metadata bytes;
    block by block, each encoder-made zstd body equal once decoded and every other body byte for byte; and an identical export —
    and its files that hold offsets (`verify.json`, `queries.json`, `truncation.json`, `patch.tpk` and `patch.verify.json`) are not compared;
    its footer, whose F-2 offsets, lengths and CRCs follow the compressed sizes, is not compared either, the read-path checks of the committed files covering it;
    every other file is compared byte for byte, the manifest except its `zstd_encoder` value, and the generator reports that the corpus is due for regeneration.
- A vector **depends on the encoder** when its `pack.tpk` or `patch.tpk` holds a block body or a footer of codec `zstd` that the encoder produced.
  The manifest's `codec` (§3) lists the codecs a pack holds, not this dependence.
  Vectors without such a body or footer — codec `none`, the hand-built frames of §6.2, `basic-unknown-codec`, whose other blocks and footer are codec `none`, and the `fixed` sample —
  are compared byte for byte in every case.

### 6.2 Hand-built damaged frames

A vector whose subject is a zstd stream that is short or malformed after an intact header section ([FMT §16] block framing vectors) holds a frame built by hand, never an encoder's output:
- an RFC 8878 frame, single segment, without a content checksum unless the vector says otherwise,
  whose first block is a Raw block holding the whole header section, `record_count × record_header_len` bytes ([FMT §6]),
  so that decoding the first block alone yields the header section;
- then, for a **malformed** stream, a Compressed block whose content is invalid, such as a literals section header that declares more bytes than the block holds;
  for a **short** stream, a last Raw block that ends the frame before `uncompressed_len` bytes are produced.

The envelope's `body_len` and `body_crc` and the block's F-2 entry are written for the new body, so the block fails I-2 alone ([FMT I-2]).
The generator checks each such frame as it builds it: decoding the first block yields the header section, and decoding the whole frame fails.

## 7. Item-validity checklist

The cases of the item predicate of [SEM §3] (`item-decode-error`, `ok-with-trailing`, `ok`), each a frame of `sem-item-validity` or `sem-decode-status-classified` (§9.4) and a row of its `classify.json`.
E5 §9 is the normative base; format codes are written in octal as in E5 Table 1, `0o22` for the localized string.
An item's format byte is its format code × 4 plus its number of length bytes (E5 §9.2).

| Format code | Item | Element width (bytes) |
|---|---|---|
| `0o00` | list | — (the length counts elements) |
| `0o10` | binary | 1 |
| `0o11` | boolean | 1 |
| `0o20` | ASCII | 1 |
| `0o21` | JIS-8 | 1 |
| `0o22` | localized string | 1, under the format-22 rule of [SEM §3] |
| `0o30`, `0o31`, `0o32`, `0o34` | I8, I1, I2, I4 | 8, 1, 2, 4 |
| `0o40`, `0o44` | F8, F4 | 8, 4 |
| `0o50`, `0o51`, `0o52`, `0o54` | U8, U1, U2, U4 | 8, 1, 2, 4 |

Every other of the 64 format codes is undefined.
Each case is a data message (SType 0) whose message text is the item described:

| Case | Message text | `decode_status` |
|---|---|---|
| `undefined-format` | an item of an undefined format code | `item-decode-error` |
| `length-bytes-zero` | a format byte whose length-byte count is 0 | `item-decode-error` |
| `length-bytes-cut-1`, `length-bytes-cut-2`, `length-bytes-cut-3` | a format byte declaring 1, 2 or 3 length bytes, the text ending before the last of them | `item-decode-error` |
| `body-short` | a body shorter than its declared length | `item-decode-error` |
| `width-i2`, `width-i4`, `width-i8`, `width-u2`, `width-u4`, `width-u8`, `width-f4`, `width-f8` | a body whose length is not a multiple of the element width | `item-decode-error` |
| `list-missing-element` | a list whose text ends before a declared element | `item-decode-error` |
| `list-invalid-nested` | a list holding an invalid item | `item-decode-error` |
| `localized-length-1` | a format-22 item of length 1 (G5-181) | `item-decode-error` |
| `empty-list` | a list of length 0 | `ok` |
| `zero-length-binary`, `zero-length-boolean`, `zero-length-ascii`, `zero-length-jis8`, `zero-length-localized`, `zero-length-i1`, `zero-length-i2`, `zero-length-i4`, `zero-length-i8`, `zero-length-u1`, `zero-length-u2`, `zero-length-u4`, `zero-length-u8`, `zero-length-f4`, `zero-length-f8` | an item of length 0 of each format | `ok` |
| `whole-width-i2`, `whole-width-i4`, `whole-width-i8`, `whole-width-u2`, `whole-width-u4`, `whole-width-u8`, `whole-width-f4`, `whole-width-f8` | an item of two elements | `ok` |
| `length-bytes-1`, `length-bytes-2`, `length-bytes-3` | one small item with 1, 2 and 3 length bytes, the longer forms non-minimal | `ok` |
| `boolean-other-values` | a boolean whose bytes are other than 0 and 1 | `ok` |
| `ascii-high-bytes` | an ASCII item holding bytes ≥ 0x80 | `ok` |
| `localized-length-2`, `localized-length-3` | format-22 items of length 2 and 3 (G5-181) | `ok` |
| `trailing` | a valid item followed by more bytes | `ok-with-trailing`, `trailing_bytes` the excess |
| `empty-text` | no message text (a header-only data message) | `ok` |

The order in which [SEM §3] evaluates its predicates is exercised by frames that satisfy two at once,
the `ok` of a control record by a well-formed control message,
and `max_frame_len` by two messages at its boundary:

| Case | Frame | `decode_status` |
|---|---|---|
| `short-and-bad-ptype` | fewer than 14 captured bytes whose PType byte is not 0 | `short-frame` |
| `length-mismatch-and-bad-ptype` | a length field that disagrees with the capture, PType not 0 | `length-mismatch` |
| `length-mismatch-and-bad-stype` | a length field that disagrees with the capture, SType undefined | `length-mismatch` |
| `bad-stype-and-control-with-body` | SType undefined with a body | `bad-stype` |
| `control-with-body-and-oversized` | SType not 0 with a body, longer than `max_frame_len` | `control-with-body` |
| `oversized-and-item-decode-error` | a data message longer than `max_frame_len` whose message text does not begin with a valid item | `oversized` |
| `control-ok` | a well-formed control message, a `Linktest.req` | `ok` |
| `max-frame-len-equal` | a valid data message whose frame length is M, the vector's `max_frame_len` | `ok` |
| `max-frame-len-exceeded` | a valid data message whose frame length is M + 1 | `oversized` |

Each vector carries one `max_frame_len` M ([SEM §3]), so the two boundary cases are two messages of M and M + 1 bytes under the same M.
A decoder's own limits, such as a nesting depth or a frame size it accepts, are implementation limits outside the checklist.

## 8. Vector readings

How the corpus realizes [FMT §16] clauses whose wording leaves a choice:
- **Empty pack**: the file header and the pack metadata only: not finalized, no blocks.
  **Pack with zero records**: finalized, `block_count` = 0 ([FMT §10]).
- **A block holding one oversized record**: a record larger than the block size threshold, written alone in its own block ([FMT §2]);
  the `oversized` status is a [SEM §3] case (§7).
- **The CRC check value** and **UUID byte order** are in `primitives.json` (§5.1); the UUID is also the `pack_id` of `basic-uuid-byte-order`.
- **Maximum-value integers** ([FMT §2]):
  at the limit, seqs up to 2^63 − 1, `mono_ns` at both `i64` extremes, `epoch` and `trailing_bytes` 2^32 − 1, metadata `u64` tags at 2^63 − 1,
  `clock_step_ns` and `gap_start` at both `i64` extremes, and `ts_utc_ns` at both `i64` extremes, in an extract, whose blocks may lie in any hour ([STO §2]);
  past the limit, a block whose first seq is 2^63 (the block fails) and a metadata `u64` of 2^63 (rejected, `metadata-entry-list`).
  A `payload_len` of 2^31 − 1 is not materialized, for the corpus's size.
- **A pack truncated at every byte offset**: one base pack and its `truncation.json` (§5.7), under codec `none` and codec `zstd`; the repair vectors' table also holds each cut's repair.
- **Control frames**: a `Select.rsp`, a `Deselect.rsp`, a `Reject.req` and a `Linktest.req`, with `fields.json` and queries over their bytes 6 and 7, read positionally ([FMT §7.2]).

## 9. Vector catalogue

Every clause of [FMT §16] and every single-pack clause of [SEM §9] is mapped below to the vectors that exercise it, or marked as planned elsewhere:
`6b2` (reads over several packs and transaction lookups, with their result formats), `6b3` (the corpus vectors of [STO §8]), `phase 7` (extracts and redaction),
phases of `tracepack-impl-plan.md` that extend this corpus under a later schema value or the same one.
A vector may serve several clauses.

### 9.1 [FMT §16] clauses

| Clause | Vectors |
|---|---|
| empty pack | `basic-empty-pack` |
| pack with zero records | `basic-zero-records` |
| codec `none` and codec `zstd` of the same records | `basic-codec-none`, `basic-codec-zstd`, `basic-codec-mixed` |
| truncated tail | `basic-truncated-tail` |
| corrupt middle block | `basic-corrupt-middle` |
| bad envelope CRC | `basic-bad-envelope-crc-indexed`, `basic-bad-envelope-crc-walked` |
| unknown codec | `basic-unknown-codec` |
| unknown TLV tag and enum value | `basic-unknown-tags-and-enums` |
| `record_header_len` > 44, 45–55, extension bytes preserved | `framing-header-extension` |
| unordered timestamps | `basic-unordered-timestamps` |
| maximum-value integers | `basic-max-int`, `basic-max-int-extract`, `basic-seq-over-limit`, `bootstrap-entry-value` |
| UUID byte order; the CRC check value | `basic-uuid-byte-order`, `primitives.json` |
| CRC-valid but structurally invalid footers, one per [FMT §10] validation clause | `footer-invalid-f2-offset`, `footer-invalid-f2-entry-len`, `footer-invalid-sections`, `footer-invalid-f2-contiguity`, `footer-invalid-seq-order`, `footer-invalid-summary-bounds`, `footer-invalid-seq-set`, `footer-invalid-count-sums`, `footer-invalid-f5-aggregate`, `footer-invalid-trailer-record-count`, `footer-invalid-trailer-last-seq`, `footer-invalid-header-len`, `footer-invalid-ts-range`, `footer-invalid-epoch-range`, `footer-invalid-epoch-entry`, `footer-invalid-boundary-seq`, `footer-invalid-overflow`, `footer-invalid-empty-stats` |
| absent `seq_range` hiding a missing seq | `footer-seq-range-hides-gap` |
| retired F-3 tags and a present F-4 | `footer-retired-f3-tags-and-f4` |
| an epoch ended twice, in one block and across two | `footer-close-twice-one-block`, `footer-close-twice-two-blocks` |
| F-5 stating the higher `close_seq` | `footer-f5-higher-close-seq` |
| F-3 stating the higher `close_seq` in one block, F-5 recomputed | `footer-f3-higher-close-seq` |
| quality bit 0 without a capture-boundary record | `footer-quality-bit0-data` |
| a transport-event payload that is not a valid TLV body | `footer-invalid-socket-close-body`, `footer-invalid-stop-body`, `verify-defect-event-payload` |
| HSMS short captures of 5 to 14 bytes | `hsms-short-captures` |
| System Bytes bit clear, bytes present | `hsms-system-bytes-unavailable` |
| System Bytes bit set, bytes absent (nonconforming-writer fixture) | `hsms-system-bytes-set-short`; the append refusal is a writer test, not a vector |
| control frames, bytes 6 and 7 read positionally | `hsms-control-frames` |
| S/F, SessionID and System Bytes predicates | `hsms-predicates` |
| transaction candidate selection ([SEM §7.2]) | 6b2 |
| identical decoded body bytes under two framings | `framing-two-framings` |
| a block of several records checked on gathered headers | `framing-gathered-headers` |
| `record_header_len` below 44 | `framing-header-len-below-44` |
| `record_count × record_header_len` overflow | `framing-header-section-overflow` |
| envelope / F-2 `record_header_len` mismatch | `framing-header-len-index-mismatch` |
| a pack mixing header lengths | `framing-header-extension` |
| codec stream short or malformed after a valid header section | `framing-zstd-malformed`, `framing-zstd-short`, `framing-zstd-short-checksum` |
| a block holding one oversized record | `framing-record-over-threshold` |
| a validating writer whose encoded block has an injected I-2 defect | `validation-i2-injection` |
| a payload-only identity conflict between two packs | 6b2 |
| a finalized pack whose blocks all pass | `basic-codec-none`, `basic-codec-zstd` |
| a pack truncated at every byte offset | `verify-truncation-none`, `verify-truncation-zstd` |
| a finalized pack whose last block fails | `verify-last-block-failed` |
| a failed block between validated ones, with and without a valid footer | `basic-corrupt-middle`, `verify-corrupt-middle-walked` |
| a finalized pack with an invalid footer, or an F-3 summary that disagrees, every block passing | the `footer-invalid-*` vectors, `verify-f3-disagrees`, `verify-f2-disagrees` |
| a writer defect of each kind | `hsms-system-bytes-set-short` (`field-validity`), `verify-defect-event-field-validity`, `verify-defect-event-payload`, `verify-defect-seq-order`, `verify-defect-hour-span`, `verify-seq-start-above` (`seq-start`) |
| the `seq_start` defect: above, below, after a failed block | `verify-seq-start-above`, `verify-seq-start-below`, `verify-seq-start-after-failed` |
| seq-order and hour-span defects of a finalized pack | `verify-defect-seq-order`, `verify-defect-hour-span` |
| one bootstrap vector per rejection code | `bootstrap-short-object`, `bootstrap-bad-magic`, `bootstrap-header-crc`, `bootstrap-unsupported-version`, `bootstrap-metadata-past-object`, `bootstrap-metadata-crc`, `bootstrap-entry-framing`, `bootstrap-entry-type`, `bootstrap-entry-value`, `bootstrap-entry-repetition`, `bootstrap-entry-nested`, `bootstrap-required-tag`, `bootstrap-conditional-tag`, `bootstrap-replacement-set-size`, `bootstrap-replacement-set-index`; a missing required nested tag: phase 7 |
| a missing tag whose requirement depends on the records: the pack opens | `bootstrap-record-conditioned-tag` |
| repair: a failed middle block with a valid footer | `repair-middle-trusted-footer` |
| repair: a footer a validated block disagrees with; no valid footer | `repair-middle-disagreeing-footer`, `repair-middle-no-footer` |
| repair: truncated at every byte offset after the pack metadata | `repair-truncation` |
| repair: every block failed | `repair-all-failed` |
| repair: a `finalized-inconsistent` pack | `repair-inconsistent` |
| repair of a patch | `repair-of-patch` |
| repair: the `seq_start` defect above and below | `repair-seq-start-above`, `repair-seq-start-below` |
| refusal: nothing to repair (G5-183) | `repair-not-needed`, `repair-not-needed-extract`, `repair-not-needed-period`, `repair-not-needed-commitments` |
| refusal: an extract | `repair-refused-extract` |
| refusal: a seq-order or hour-span defect | `repair-refused-seq-order`, `repair-refused-hour-span` |
| refusal: a lost run without a seq between its neighbours | `repair-refused-lost-run` |
| refusal: a block outside the scope's hour | `repair-refused-scope-breach`, `repair-refused-hour-span` |
| redaction vectors | phase 7 |
| each query vector carries its expected results | `queries.json` (§5.4) |
| a sample of the definition before v2.13 | `sample-v010-rows` |

### 9.2 [SEM §9] clauses

| Clause | Vectors |
|---|---|
| one vector per `decode_status` value | `sem-decode-status-classified`, `sem-decode-status-log` |
| one vector per item-validity rule ([SEM §3]) | `sem-item-validity`, with the precedence cases in `sem-decode-status-classified` (§7) |
| a nonzero `capture_origin_mono_ns` | `sem-capture-origin-mono` |
| repeated System Bytes; cross-pack transactions; every lookup from a primary ([SEM §7.2]) | 6b2 |
| reads over several packs ([SEM §7.4]) | 6b2 |
| redaction ([SEM §8]) | phase 7 |

### 9.3 [STO §8] vectors

Every corpus vector of [STO §8] is planned in 6b3, except the pack metadata whose replacement-set values are rejected, which is `bootstrap-replacement-set-size` and `bootstrap-replacement-set-index`,
and the recovery vectors (`stop-unclean` recovery, the crash with an empty spool), which are deferred with recovery itself (G5-91).
The Service vectors of [STO §8] are not part of the corpus, as [STO §8] states, and no phase of this corpus plans them.

### 9.4 Vectors

Group prefixes: `basic-`, `framing-`, `footer-`, `hsms-`, `validation-`, `verify-`, `repair-`, `bootstrap-`, `sem-`, `sample-`.
Files beyond those of the class (§2) are named where a vector has them.

| Vector | Class | Subject |
|---|---|---|
| `basic-empty-pack` | read | §8; `queries.json` (`truncated` at the end of the pack metadata) |
| `basic-zero-records` | read | §8 |
| `basic-codec-none` | read | several blocks of data, control, transport-event and annotation records, `finalized-consistent`; `queries.json` over time, kinds, directions and epochs |
| `basic-codec-zstd` | read | the same records under codec `zstd`, the same identifiers (§3) |
| `basic-codec-mixed` | read | the same records, the same identifiers, with blocks of codec `none` and `zstd` in turn |
| `basic-truncated-tail` | read | a pack cut inside its last block |
| `basic-corrupt-middle` | read | a middle block failing its body CRC, valid footer; `queries.json` |
| `basic-bad-envelope-crc-indexed` | read | a block failing its envelope CRC, passed by its F-2 entry |
| `basic-bad-envelope-crc-walked` | read | the same in a finalized pack whose footer is rejected: the walk stops there, so the walked blocks are not compared with the trailer's totals ([FMT §13]); `queries.json` |
| `basic-unknown-codec` | read | a block of a codec outside the registry, its envelope, F-2 entry and body CRC valid and its body no encoder's output, every other block and the footer codec `none`; `queries.json` |
| `basic-unknown-tags-and-enums` | read | unknown and private metadata tags, unknown enum values in the metadata, the record header and event bodies |
| `basic-unordered-timestamps` | read | `ts_utc_ns` not ascending within and across blocks |
| `basic-max-int` | read | the at-limit values of §8 but `ts_utc_ns` |
| `basic-max-int-extract` | read | an extract with `ts_utc_ns` at both `i64` extremes |
| `basic-seq-over-limit` | read | a block whose first seq is 2^63 |
| `basic-uuid-byte-order` | read | `pack_id` the UUID of `primitives.json` |
| `framing-header-extension` | read | blocks of `record_header_len` 44 and 45 to 55, each with its own extension bytes |
| `framing-two-framings` | read | two blocks whose decoded bodies are equal: one record with a long header, several records with 44-byte headers |
| `framing-gathered-headers` | read | a block of several records with extension bytes, seqs and lengths read from the gathered headers |
| `framing-header-len-below-44` | read | a block of `record_header_len` 43 |
| `framing-header-section-overflow` | read | an envelope, its CRCs valid and its F-2 entry agreeing where the footer is valid, with `record_count` 2^18 and `record_header_len` 16384: the product 2^32 exceeds every valid `uncompressed_len` ([FMT §2]), so the block fails (`corrupt-block`), while 32-bit arithmetic wraps it to 0, a header section that would fit |
| `framing-header-len-index-mismatch` | read | an envelope whose `record_header_len` disagrees with its F-2 entry |
| `framing-zstd-malformed`, `framing-zstd-short`, `framing-zstd-short-checksum` | read | the hand-built frames of §6.2, the last with a content checksum; every other block and the footer codec `none` |
| `framing-record-over-threshold` | read | a record larger than the block size threshold, alone in its block |
| `footer-invalid-f2-offset`, `footer-invalid-f2-entry-len`, `footer-invalid-sections`, `footer-invalid-f2-contiguity`, `footer-invalid-seq-order`, `footer-invalid-summary-bounds`, `footer-invalid-seq-set`, `footer-invalid-count-sums`, `footer-invalid-f5-aggregate`, `footer-invalid-trailer-last-seq`, `footer-invalid-header-len`, `footer-invalid-ts-range`, `footer-invalid-epoch-range`, `footer-invalid-epoch-entry`, `footer-invalid-boundary-seq`, `footer-invalid-overflow`, `footer-invalid-empty-stats` | read | one footer validation clause of [FMT §10] each, CRC valid; `footer-invalid-f5-aggregate` has `queries.json` (a complete walk: nothing reported) |
| `footer-invalid-trailer-record-count` | read | the trailer's `record_count` off by one, every block walked; `queries.json` (`truncated` without an offset) |
| `footer-seq-range-hides-gap` | read | a block missing a seq whose F-3 summary has no `seq_range` |
| `footer-retired-f3-tags-and-f4` | read | retired F-3 tags and a present F-4, both ignored |
| `footer-close-twice-one-block`, `footer-close-twice-two-blocks` | read | an epoch ended by a socket-close, then a clean `stop`, in one block and across two; `footer.json` |
| `footer-f5-higher-close-seq` | read | F-5 stating the higher `close_seq`: not accepted; `footer.json` |
| `footer-f3-higher-close-seq` | read | in one block, F-3 stating the higher `close_seq`, F-5 recomputed from it: accepted, `finalized-inconsistent`; `footer.json` |
| `footer-epoch-without-closure` | read | an epoch no record ends; `footer.json` |
| `footer-invalid-socket-close-body`, `footer-invalid-stop-body` | read | a socket-close and a clean-stop capture-boundary event whose body breaks another rule: no boundary, no closure, a writer defect; `footer.json` |
| `footer-quality-bit0-data` | read | quality bit 0 on a data record: no boundary; `footer.json` |
| `hsms-short-captures` | read | data records of 5 to 14 captured bytes, each field available exactly when its bit is set; `fields.json`, `queries.json`, `cases` |
| `hsms-system-bytes-unavailable` | read | a data record whose System Bytes bit is clear while its payload holds them; `fields.json`, `queries.json` |
| `hsms-system-bytes-set-short` | read | a data record whose System Bytes bit is set while its payload ends before them (`nonconforming-writer`); `fields.json`, `queries.json` |
| `hsms-control-frames` | read | §8; `fields.json`, `queries.json` (S/F, SessionID and System Bytes) |
| `hsms-predicates` | read | S/F, SessionID and System Bytes predicates with and without `include_unavailable`, including S/F with one field unavailable and the other differing; `fields.json`, `queries.json` |
| `validation-i2-injection` | read | the output of a validating writer stopped before a block with an injected I-2 defect: no such block, no trailer, `unfinalized`; `queries.json` (`truncated` at the end of the last block) |
| `verify-truncation-none`, `verify-truncation-zstd` | truncation | a base pack of two or three blocks under each codec |
| `verify-last-block-failed` | read | `finalized-truncated` |
| `verify-corrupt-middle-walked` | read | a middle block failing its body CRC, its envelope intact, the footer rejected: `corrupt-middle` |
| `verify-f2-disagrees` | read | a block whose records disagree with its F-2 entry, the footer valid; `queries.json` (`index-mismatch`) |
| `verify-f3-disagrees` | read | a block whose records disagree with its F-3 summary only; `queries.json` (nothing reported) |
| `verify-defect-event-field-validity` | read | a transport-event record with `field_validity` not 0 |
| `verify-defect-event-payload` | read | a `state-transition` event whose body is not valid: the writer defect alone |
| `verify-defect-seq-order`, `verify-defect-hour-span` | read | the I-12 and I-13 defects in a finalized pack: `finalized-inconsistent` |
| `verify-seq-start-above`, `verify-seq-start-below`, `verify-seq-start-after-failed` | read | the `seq_start` defect: a first block above and below `seq_start`, and a first validated block that starts at `seq_start` after a failed one |
| `bootstrap-short-object`, `bootstrap-bad-magic`, `bootstrap-header-crc`, `bootstrap-unsupported-version`, `bootstrap-metadata-past-object`, `bootstrap-metadata-crc` | rejection | one cause each (§5.3) |
| `bootstrap-entry-framing`, `bootstrap-entry-type`, `bootstrap-entry-value`, `bootstrap-entry-repetition`, `bootstrap-entry-nested` | rejection | `metadata-entry-list`: an entry past the list's end, a known tag of the wrong value type, a metadata `u64` of 2^63, a non-repeatable known tag twice, a `coverage` entry whose nested list breaks framing |
| `bootstrap-required-tag`, `bootstrap-conditional-tag` | rejection | `metadata-required-tag`: a tag required `always` missing; `capture_origin_utc_ns` missing under `time_source = capture-clock` |
| `bootstrap-replacement-set-size`, `bootstrap-replacement-set-index` | rejection | `replacement-set` |
| `bootstrap-record-conditioned-tag` | read | a classified record in a pack without `classifier`: the pack opens ([FMT §13]) |
| `repair-middle-trusted-footer`, `repair-middle-disagreeing-footer`, `repair-middle-no-footer` | repair | a failed middle block; new `coverage` from the footer's ranges, or from the neighbours' seqs and the scope hour |
| `repair-truncation` | truncation | a base pack of two or three blocks; every cut's verification and repair (a tail entry without `seq_last`) |
| `repair-all-failed` | repair | every block failed: a patch holding only `coverage` |
| `repair-inconsistent` | repair | every block copied, no new `coverage` |
| `repair-of-patch` | repair | a damaged patch: its `coverage` inherited; `queries.json` (`coverage`) |
| `repair-seq-start-above`, `repair-seq-start-below` | repair | a `finalized-consistent` pack with the `seq_start` defect: `patched`, every block copied, the patch's `seq_start` its first record's seq |
| `repair-not-needed` | repair | a `finalized-consistent` segment: `not-needed` |
| `repair-not-needed-extract` | repair | a `finalized-consistent` extract: `not-needed` (G5-183) |
| `repair-not-needed-period`, `repair-not-needed-commitments` | repair | `finalized-consistent` packs whose period is not inside one hour, and whose records breach the metadata's commitments: `not-needed` (G5-183) |
| `repair-refused-extract` | repair | a `finalized-inconsistent` extract: `not-repairable` |
| `repair-refused-seq-order` | repair | the I-12 defect: `not-repairable` |
| `repair-refused-hour-span` | repair | the I-13 defect, its block also outside the scope's hour (both conditions cited): `not-repairable` |
| `repair-refused-lost-run` | repair | a lost run without a seq between its neighbours: `not-repairable` |
| `repair-refused-scope-breach` | repair | a block of one hour other than the scope's, without the I-13 defect: `not-repairable` |
| `sem-decode-status-classified` | read | a record of each byte-predicate status, `not-attempted` and `not-applicable`, the precedence, control and `max_frame_len` cases of §7; `classify.json`, `cases` |
| `sem-decode-status-log` | read | a `log` capture with `reconstructed-ok`, `parse-failed` and `build-rejected` records; `cases` |
| `sem-item-validity` | read | the checklist of §7; `classify.json`, `cases` |
| `sem-capture-origin-mono` | read | a capture-clock pack whose `capture_origin_mono_ns` is not 0 ([SEM §4]) |
| `sample-v010-rows` | read | the tracepack v0.1.0 pack of a multi-record block, reported `corrupt`; `source` `fixed`, label `pre-v2.13-sample` ([FMT §14]) |

## 10. Schema versions

The `corpus` value names the schemas of §3 and §5 together.
`tracepack-corpus/1` is a draft until tracepack v1.0.0: a change to a schema before then bumps the value, and the change is recorded in the spec changelog.
Adding a vector, a query or a case keeps the value; changing what an existing file states, or a schema, takes a new one.
