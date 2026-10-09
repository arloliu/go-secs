# tracepack — conformance corpus

Status: current (2026-10-10) — v2.30, tracepack format 1.0, schema `tracepack-corpus/2` (draft until tracepack v1.0.0).
Normative, language-agnostic. [FMT §16] states what the corpus holds; this document defines its files, their schemas and the vector catalogue.

Depends on: [FMT §2] portable encoding, [FMT §5] TLV registries, [FMT §7.2] HSMS header fields, [FMT §10] footer and its validation,
[FMT §13] bootstrap, verification and repair, [FMT §16] corpus contents, [JSONL] value forms and the export,
[SEM §3] `decode_status`, [SEM §7.2] transaction lookup, [SEM §7.4] queries and reads over several packs,
[STO §4] active view, [STO §5] observation of a lookup, per-capture evidence and completeness.

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
  - with `patch.tpk`, it reads the patch as it reads `pack.tpk`, against `patch.jsonl` and `patch.verify.json`;
  - of class `multi-pack`, it opens every `pack-<n>.tpk`, and its verification of each projects to `pack-<n>.verify.json` (§5.2).
- An implementation of reads over several packs ([SEM §7.4]) conforms when, for every read of every `reads.json`,
  the read of the packs it names, in the order it names them, with its order, filter and conflict bound, yields the expected result or fails as expected (§5.10).
- An implementation of transaction lookup ([SEM §7.2]) conforms when, for every lookup of every `lookups.json`,
  the lookup of its primary over the source its `source` object denotes (§8) projects to the expected result or fails as expected (§5.11).
- Each record such a read or lookup returns is the record of its seq in the pack and block the result names, every record header field and, when returned, the payload, as stored.
- An implementation of repair ([FMT §13]) conforms when, for every `repair` vector and every repair row of a truncation table,
  with the options `repair.json` or `truncation.json` gives, its result is the expected one (§5.6),
  and its patch, when it writes one, has the properties of [FMT §16] Repair vectors and holds the expected blocks, records and new `coverage` entries.
  `patch.tpk` is the reference implementation's patch; another implementation's patch may differ from it in the order of its pack metadata entries and so in its footer offsets.
- A **classifier** conforms when it computes, for every frame `classify.json` lists, the listed `decode_status` and `trailing_bytes` under the listed `max_frame_len` (§5.9).
- A **writer** conforms when a conforming reader round-trips its output.
  The corpus holds one writer behaviour as bytes, the validating writer of `validation-i2-injection` (§9.4):
  another writer conforms when its output stops before the same block and holds no trailer.
- An implementation skips the files of an operation it does not offer (repair, classification, a reading of footer values, a read over several packs, a transaction lookup);
  the files of the others still bind it.
- A reader's own limits, an I/O error or a cancellation are never goldens ([FMT §13], [JSONL §7]): no expectation depends on them,
  except the conflict bound a read states (§5.10);
  an implementation that offers no conflict bound skips the reads that state one.

## 2. Layout

The corpus is a directory holding `manifest.json` (§3), `primitives.json` (§5.1), a `README.md` (informative), and one directory per vector, named by the vector's `id`.
A vector directory holds the files its class gives below, under these fixed names, and no others:

| File | Present | Content |
|---|---|---|
| `pack.tpk` | every class but `multi-pack` | the vector's pack; for a `truncation` vector, the base pack every cut is taken from |
| `export.jsonl` | class `read`, `repair`, `truncation` | the expected export of `pack.tpk` ([JSONL]) |
| `rejection.json` | class `rejection` | the expected bootstrap rejection (§5.3) |
| `verify.json` | class `read`, `repair`, `truncation` | the expected verification report of `pack.tpk` (§5.2) |
| `queries.json` | optional, every class but `multi-pack` | query vectors over `pack.tpk` (§5.4) |
| `fields.json` | optional, every class but `multi-pack` | the HSMS header fields of each data and control record (§5.5) |
| `classify.json` | optional, every class but `multi-pack` | the expected classification of byte-predicate frames (§5.9) |
| `footer.json` | optional, every class but `multi-pack` | footer expectations (§5.8) |
| `repair.json` | class `repair` | the repair options and the expected result (§5.6) |
| `patch.tpk`, `patch.jsonl`, `patch.verify.json` | class `repair`, when `repair.json`'s result is `patched` | the reference patch, its export and its verification report |
| `truncation.json` | class `truncation` | the expectation table of every cut (§5.7) |
| `pack-<n>.tpk` | class `multi-pack`, one or more | the vector's packs, numbered n = 0, 1, … without a gap, n in decimal without leading zeros |
| `pack-<n>.verify.json` | class `multi-pack`, one per `pack-<n>.tpk` | the expected verification report of `pack-<n>.tpk` (§5.2) |
| `reads.json` | class `multi-pack`, optional | reads over several packs (§5.10) |
| `lookups.json` | class `multi-pack`, optional | transaction lookups (§5.11) |

A vector of class `rejection` has no `export.jsonl` and no `verify.json`: a pack rejected at bootstrap has neither ([JSONL §8], [FMT §13]).
A vector of class `multi-pack` holds `reads.json`, `lookups.json` or both, and no `.jsonl` file:
the other classes cover the export, and each pack's verification report pins the state of the pack whose records the expected results name by pack and block.

## 3. Manifest and identification

`manifest.json` identifies the corpus and lists its vectors.
The corpus is identified by its schema, the spec version and the format version together,
so goldens written under different definitions are told apart ([FMT §14]).

| Key | Type | Present | Value |
|---|---|---|---|
| `corpus` | string | always | `"tracepack-corpus/2"`, the schema of every file of this document (§10) |
| `spec_version` | string | always | the spec version the goldens follow: `"2.30"` |
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
| `class` | string | always | `read`, `rejection`, `truncation`, `repair` or `multi-pack` (§2) |
| `labels` | array of strings | always, `[]` when none | ascending; `damaged`: the pack models damage after writing (a failed CRC, a cut, a broken codec stream); `nonconforming-writer`: the pack holds bytes, every CRC valid, that a conforming writer never writes; `pre-v2.13-sample`: the sample of the format definition before v2.13 ([FMT §14]); for class `multi-pack`, the labels of any of its packs |
| `codec` | string | always | `none` when every block body and the footer of the vector's packs (`pack.tpk`, or every `pack-<n>.tpk`) use codec `none`, `zstd` when they all use `zstd`, `mixed` otherwise, an unknown codec included |
| `source` | string | always | `generated`, or `fixed` for a file kept as committed and never regenerated |
| `files` | array of strings | always | the vector's files, in the order of the table of §2; for class `multi-pack`, by ascending n each `pack-<n>.tpk` then `pack-<n>.verify.json`, then `reads.json`, then `lookups.json` |
| `cases` | array of objects | optional, never empty, never in class `multi-pack` | single-record sub-vectors, ascending by `seq`: `{id, seq, cites}`, `id` kebab-case and unique within the vector, `seq` the record's seq (string), `cites` as above |

Vectors that differ only in codec (`basic-codec-none`, `basic-codec-zstd` and `basic-codec-mixed`; the truncation base packs under both codecs) carry the same `pack_id`, `capture_id` and other identifiers,
so their exports are identical.

## 4. JSON conventions

Every JSON file of the corpus is UTF-8 JSON text (RFC 8259) holding one object or array.
- Values take the forms of [JSONL §3]: `u64` and `i64` values as decimal strings (G5-167); `u8`, `u16` and `u32` values as numbers; bytes as standard base64 with padding;
  UUIDs in the canonical lowercase form; enum values and bits by their [JSONL §3] names, `unknown(<n>)` and `bit(<n>)` included.
- A block index and a count of blocks are numbers; a count of records is a string, since records are counted in `u64`.
- A **block index** is the 0-based position of a block among the blocks the reader locates ([FMT §13]), in file order.
- An **offset** is a file offset, in bytes from the first byte of the file the object describes (`pack.tpk`, `patch.tpk`, a `pack-<n>.tpk`, or a cut of `pack.tpk`), as a string.
- A **pack number** names the pack `pack-<n>.tpk` of a `multi-pack` vector as the number n.
  The results of reads and lookups (§5.10, §5.11) name every pack by its number, never by its `pack_id`;
  the packs of a vector have distinct `pack_id`s, so an implementation maps one to the other by opening the packs.
- An **hour** is a UTC hour numbered in whole hours from 1970-01-01T00:00Z, negative before it, as a string (`i64`):
  the hour of a `ts_utc_ns` t is ⌊t / 3 600 000 000 000⌋, the scope hour of [STO §2].
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

### 5.10 `reads.json`

An array of reads over several packs of a `multi-pack` vector ([SEM §7.4]), ascending by `id`.
Each read gives the packs it names to one read, in the order it names them, and states the expected result.

| Key | Type | Present | Value |
|---|---|---|---|
| `id` | string | always | kebab-case, unique within the vector |
| `cites` | array of strings | always, never empty | as in the manifest (§3) |
| `packs` | array of numbers | always, never empty | the packs read, by number (§4), in the order the read is given them; no number twice. Two reads that name the same packs in two orders read them in both orders ([SEM §7.4] Conflicts) |
| `order` | string | always | `capture` or `time`, the order of the read ([SEM §7.4]) |
| `payloads` | bool | only as `true` | the read returns each record's payload; no other value of the read changes |
| `max_conflicts` | number | when the read states a conflict bound | the bound, at least 1: the read fails when it finds one conflict more ([SEM §7.4] Conflicts) |
| `filter` | object | always | the selection, as in §5.4 |
| `expect` | object | always | the success form or the error form, below |

A read without `max_conflicts` states no bound, and its expectation depends on none a reader keeps (§1).
A read has no retention boundary, so no hour is removed ([STO §5] Retention).

`expect`, the success form, `{items, incomplete, conflicts, footer_errors}`:

| Key | Type | Present | Elements and order | Value |
|---|---|---|---|---|
| `items` | array of objects | always | one per version yielded, in the order the read yields them, never sorted; `[]` when none | `{seq, pack, block, conflict}`, below |
| `incomplete` | array of objects | always | the reasons of each pack, packs in the order of `packs`, each pack's reasons as §5.4 orders them; `[]` when none | `{pack, reason, block?, offset?, coverage?}`: `pack` the pack's number, the other keys as in §5.4 |
| `conflicts` | array of objects | always | one per conflict, in discovery order ([SEM §7.4]); `[]` when none | `{capture_id, seq, versions}`, below |
| `footer_errors` | array of numbers | always | one per pack whose footer the read did not use, in the order of `packs`; `[]` when none | the packs whose verification report has `footer_valid` false (§5.2) |

An item:

| Key | Type | Present | Value |
|---|---|---|---|
| `seq` | string | always | the record's seq |
| `pack` | number | always | the pack of the version's representative ([SEM §7.4] Conflicts) |
| `block` | number | always | the representative's block index in that pack (§4) |
| `conflict` | bool | always | the record's copies differ ([SEM §7.4] Conflicts) |

A version's capture is its pack's, a pack holding one capture.

A conflict:

| Key | Type | Present | Value |
|---|---|---|---|
| `capture_id` | string | always | the record's capture (UUID) |
| `seq` | string | always | the record's seq |
| `versions` | array of arrays of numbers | always, never empty | one array per version, in version order ([SEM §7.4]): the packs holding that version, each once, in the order of `packs` |

In capture order a conflict is discovered per seq,
as a capture advances to its next selected record ([SEM §7.4] Order of a read over several packs):
a capture whose selected versions are all yielded resolves its next seqs in ascending order, the conflicting ones among them,
up to its next selected record or its last seq, before the read yields another record;
a conflict above that record, in the same cluster or a later one, is not yet discovered.

`incomplete` applies §5.4's decision table to each pack read, the read's filter its query; a reader's limit is never a reason (§1).
A read's filter never lets the F-2 entries, F-3 summaries or computed summaries exclude a whole cluster that holds a conflict, a defect or a selected record,
since whether a reader excludes such a cluster is not part of the contract ([SEM §7.4] Exclusion, §5.4):
so every cluster that decides an expected value is read whatever a reader prunes.

`expect`, the error form, `{error, conflicts}`, for a read that states `max_conflicts` and finds more conflicts:

| Key | Type | Present | Value |
|---|---|---|---|
| `error` | string | always | `conflict-limit` |
| `conflicts` | array of objects | always | the first `max_conflicts` conflicts in discovery order, the conflicts found before the one that failed the read, as in the success form |

The items and `incomplete` reasons a read yields before it fails are not part of the error form ([SEM §9]: "the conflicts found so far").

### 5.11 `lookups.json`

An array of transaction lookups from a primary over the packs of a `multi-pack` vector ([SEM §7.2]), ascending by `id`.

| Key | Type | Present | Value |
|---|---|---|---|
| `id` | string | always | kebab-case, unique within the vector |
| `cites` | array of strings | always, never empty | as in the manifest (§3) |
| `source` | object | always | `{view, evidence, complete}`, the source of the lookup's observation ([STO §5]), below and §8 |
| `key` | object | always | `{capture_id, seq, hour}`, the primary, below |
| `max_scopes` | number | always | the hours scheduled, `key.hour` and the ones after it, at least 1 ([SEM §7.2] Scopes read) |
| `expect` | object | always | the error form or the result, below |

`source`, per lookup, so that one set of packs serves several sources:

| Key | Type | Present | Value |
|---|---|---|---|
| `view` | array of numbers | always, never empty | the packs whose scopes the source serves, in the order that decides each scope's view order (§8); no number twice |
| `evidence` | array of numbers | always, `[]` when none | the packs whose statistics are per-capture evidence only (§8); none of them in `view`, no number twice |
| `complete` | bool | always | the source packs, `view` and `evidence` together, are every pack of the tool the source holds evidence of (§8) |

`key`:

| Key | Type | Present | Value |
|---|---|---|---|
| `capture_id` | string | always | the primary's capture (UUID) |
| `seq` | string | always | the primary's seq, p |
| `hour` | string | always | the hour of the primary's scope (§4), as the vector names it: the hour of the primary's `ts_utc_ns` ([SEM §7.2]), except where a vector names a scope whose pack breaches its hour; an implementation uses it as given |

A lookup states no budget of a reader — the bytes it holds, its own state, the conflicts it counts — and no retention boundary; none of them is a golden (§1).

`expect`, the error form, `{error}`: `error` is `not-primary`, the lookup's failure when its key names no primary ([SEM §7.2] Definitions, Primary); nothing else is compared.

`expect`, the result, `{outcome, key?, window_end?, records, gaps, searched, conflicts, footer_errors}`:

| Key | Type | Present | Elements and order | Value |
|---|---|---|---|---|
| `outcome` | string | always | — | `matched`, `ambiguous`, `unmatched` or `incomplete` ([SEM §7.2] Result) |
| `key` | object | iff the lookup kept a version of the primary | — | the primary's fields, below |
| `window_end` | string | iff a record read bounds the window | — | e, the window's end (Facts, below) |
| `records` | array of objects | always | one per kept version ([SEM §7.2] Identity across the scopes read), ascending by seq, then by the hour of the read that yielded it, then in version order within that read ([SEM §7.4]); `[]` when none | below |
| `gaps` | array of objects | always | one per distinct fact, in the order of the comparator (Facts, below); `[]` when none | below |
| `searched` | array of objects | always | one per scope read to its end, ascending by hour; `[]` when none | `{hour, indexed, packs}`: the scope's hour; whether it is indexed (§8); its view's packs by number, in view order (§8), `[]` for a scope without packs |
| `conflicts` | array of objects | always | the conflicts each scope read listed, scope reads in the order read, each read's in discovery order ([SEM §7.4]); `[]` when none | as in §5.10, `versions` holding packs of that scope's view, in view order |
| `footer_errors` | array of objects | always | ascending by `hour`, then in view order; `[]` when none | `{hour, pack}`: a pack of a scope read whose footer the read did not use (§5.10) |

A scope read reads the packs of the scope's view in view order, in capture order, with a filter that selects every record ([SEM §7.4]);
it covers every seq of its scope, so its conflicts include those below p.

`key`, the primary's fields ([SEM §7.2] Definitions), read from the first kept version of the primary in `records` order, also when the primary has several versions, as a diagnostic:

| Key | Type | Present | Value |
|---|---|---|---|
| `epoch` | number | always | its `epoch` |
| `dir` | string | always | its direction, by its [JSONL §3] name, `unknown(<n>)` included |
| `session_id` | number | iff available ([FMT §7.2]) | payload bytes 4–5 |
| `system_bytes` | string | iff available | payload bytes 10–13 (base64) |
| `stream` | number | iff `stream_and_w` is available | payload byte 6, bits 0–6 |
| `function` | number | iff available | payload byte 7 |
| `w` | bool | iff `stream_and_w` is available | payload byte 6, bit 7 |

A primary whose versions conflict is settled before its first version is checked as a primary, so that version's function can be unavailable.

A record, one kept version:

| Key | Type | Present | Value |
|---|---|---|---|
| `seq` | string | always | its seq |
| `hour` | string | always | the hour of the scope read that yielded it |
| `pack` | number | always | the pack of its representative in that read ([SEM §7.4] Conflicts) |
| `block` | number | always | the representative's block index in that pack (§4) |
| `conflict` | bool | always | the lookup found its seq in conflict, within a scope read or across scope reads; `true` for every kept version of such a seq |
| `roles` | array of strings | always | its role labels, by the role table, in its order; `[]` for a version kept only as another version of a seq that qualified |
| `in_window` | bool | always | its seq lies in the window: above p and below e, or above p while no record read bounds the window |
| `bound` | bool | always | its seq is e and the version bounds the window (Facts, below) |
| `decidable`, `eligible`, `valid` | bools | iff `roles` holds `candidate` | the candidate flags of [SEM §7.2] Definitions |

The roles of a version come from its own bytes, compared with the primary's fields, so the versions of one seq can differ in them.
Role table, the order of `roles`:

| Label | A version that is, against the primary's key |
|---|---|
| `primary` | a version of the primary's seq p, in the primary's scope or a later one |
| `candidate` | a candidate ([SEM §7.2] Definitions) |
| `possible-reply` | a possible reply |
| `same-key-primary` | a same-key primary |
| `possible-primary` | a possible same-key primary |
| `closing` | a closing event: above p, a `transport-event` of the primary's epoch with `event` = `socket-close`, or a capture-boundary record with `boundary_kind` = `stop` of any epoch, whether or not its seq is in conflict |
| `outcome` | an outcome record ([SEM §7.2] Definitions) |

The labels are diagnostics of this document.
`closing` says what the version's bytes are: a closing record of [SEM §7.2] is a version labelled `closing` without `conflict`,
and whether a version bounds the window or closes the primary's epoch is what `bound` and the facts report.

#### Facts

A **fact** states one condition of [SEM §7.2] or [STO §5] Completeness that keeps the lookup from establishing absence or makes it `incomplete`;
its keys locate the condition, nothing else.
Facts equal in every key are one fact: `gaps` holds each distinct fact once.

A fact, its keys in this order, each present as the fact table says:

| Key | Type | Value |
|---|---|---|
| `reason` | string | the fact's reason |
| `hours` | array of strings | the hours the fact concerns, ascending |
| `pack` | number | the pack the fact concerns |
| `block` | number | the block the fact concerns, its index in that pack |
| `offset` | string | the file offset the fact's reason gives (§4): for an `index` fact, the block's envelope; for a `read` fact, the offset §5.4 gives its reason |
| `seq` | string | the seq the fact concerns |
| `defect` | string | the reason of a read defect, a §5.4 name: `truncated`, `corrupt-block` or `unknown-codec` |
| `coverage` | coverage object | the `coverage` entry (§4) |
| `boundary` | object | the boundary, below |
| `basis` | array of strings | the bases of a barrier, never empty: `epoch`, `time`, in that order |

Fact table, the order of its rows the order of reasons; the first four make the outcome `incomplete`:

| Reason | One fact per | Keys | Basis |
|---|---|---|---|
| `no-key` | lookup whose primary has no key: missing where a condition of its scope explains it, in conflict within its scope's read or with a version a later scope's read yields, or without an association key | — | [SEM §7.2] Definitions (Primary), Identity across the scopes read, Scopes read |
| `conflict` | seq at or above p that one scope read yields in conflict; seq at or above p that the reads of several scopes yield | `hours` (the read's hour, or every hour whose read yielded the seq), `seq` | [SEM §7.2] Identity across the scopes read |
| `index` | block of a scope read whose records disagree with its F-2 entry | `hours`, `pack`, `block`, `offset` | [SEM §7.2] Identity across the scopes read |
| `scope-breach` | version a scope read yields whose `ts_utc_ns` lies outside the scope's hour | `hours`, `pack`, `block` (its representative's), `seq` | [SEM §7.2] Identity across the scopes read |
| `cold` | hour scheduled whose scope is not indexed, read or not read because conflicted | `hours` | [SEM §7.2] What prevents `unmatched`; [STO §5] Completeness |
| `conflicted` | hour scheduled whose scope is conflicted, so not read | `hours` | [SEM §7.2] Scopes read, What prevents `unmatched` |
| `read` | read defect of a scope read other than a `coverage` entry and an index mismatch: a §5.4 `truncated`, `corrupt-block` or `unknown-codec` reason of a pack read | `hours`, `pack`, `block` and `offset` as §5.4 gives them, `defect` | [SEM §7.2] What prevents `unmatched` |
| `coverage` | pair of a pack read and a `coverage` entry of it, of the primary's capture, that meets by [FMT §5] the seqs (p, e), or (p, ∞) while the window is unbounded, and the time range of the hours scheduled | `hours` (the read's hour), `pack`, `coverage` | [SEM §7.2] What prevents `unmatched` |
| `unevaluated` | pack read whose `quality_evaluated` is false | `hours`, `pack` | [SEM §7.2] What prevents `unmatched` |
| `evidence` | lookup whose per-capture evidence is partial (§8) | — | [SEM §7.2] What prevents `unmatched` |
| `barrier` | `stop-unclean` boundary that is a completeness barrier on at least one basis (below) | `boundary`, `basis` | [SEM §7.2] What prevents `unmatched`; [STO §5] Completeness |
| `capture-boundary` | capture-boundary of the primary's epoch, by an entry of the per-capture evidence or a capture-boundary record of that epoch read above p, other than the clean `stop` that bounds the window, identified by its capture, seq, `boundary_kind` and epoch | `boundary` (those four keys) | [SEM §7.2] What prevents `unmatched` |
| `ordering-uncertain` | lookup that reads a record of the primary's epoch carrying `ordering-uncertain`, at any seq | — | [SEM §7.2] What prevents `unmatched` |
| `correlation` | lookup whose primary carries `correlation-incomplete` | — | [SEM §7.2] What prevents `unmatched` |
| `seq-gap` | lookup whose bounded window holds a seq that no scope read yielded | — | [SEM §7.2] Identity across the scopes read, What prevents `unmatched` |
| `open-window` | lookup whose window no record read bounds | — | [SEM §7.2] Scopes read, What prevents `unmatched` |
| `unavailable` | kept version in the window that is a possible reply, an eligible candidate that is not decidable, or a candidate at or after the first possible same-key primary in the window | `hours` (the read's hour), `pack`, `block`, `seq` | [SEM §7.2] Definitions (Candidate flags), What prevents `unmatched` |
| `contradiction` | seq at which a claim of the per-capture evidence is contradicted (below); the claims at one seq give one fact | `seq` | [SEM §7.2] Definitions (Closing record) |

The outcome follows from the facts ([SEM §7.2] Result):
`incomplete` with a `no-key`, `conflict`, `index` or `scope-breach` fact;
otherwise `matched` with one valid match and `ambiguous` with several, beside every fact;
otherwise `unmatched` when `gaps` is `[]`, `incomplete` when it is not.

**Window.** The window's end e is the smallest seq above p of a kept version that is a same-key primary, with or without a conflict,
or labelled `closing` without a conflict ([SEM §7.2] Definitions, Window).
It is not a fact: `window_end` and `bound` report it, and `seq-gap` or `open-window` the window it leaves.

**Barrier.** One `stop-unclean` boundary is a barrier on two bases, each with its own predicate ([STO §5] Completeness, [SEM §7.2] What prevents `unmatched`):
- `epoch`: the boundary is in the per-capture evidence of the primary's capture,
  and the lookup read no closing record of the primary's epoch: no kept version above p, without `conflict`, labelled `closing`, whose epoch is the primary's;
  a capture-boundary `stop` of another epoch bounds the window but does not lift this basis;
- `time`: its gap interval [`gap_start`, `gap_end`], unbounded on a side whose bound is absent, meets the time range of the hours scheduled,
  [`key.hour` × 3 600 000 000 000, (`key.hour` + `max_scopes`) × 3 600 000 000 000) in nanoseconds; an interval whose `gap_start` exceeds its `gap_end` meets every range.
The boundary is one fact whose `basis` lists the bases that hold.
Boundaries that differ in any field are distinct facts; the per-capture evidence keeps one of boundaries equal in every field (§8).

**Contradiction.** The per-capture evidence makes two kinds of claim ([SEM §7.2] Definitions, Closing record), each judged by its own predicate before the facts of one seq collapse:
- the `close_seq` c of the primary's epoch: contradicted when c ≤ p, or when the lookup read c and no kept version of c is labelled `closing` with the primary's epoch;
- each capture-boundary entry of kind `stop`, of any epoch e, at seq s:
  contradicted when s ≤ p, or when the lookup read s and no kept version of s is a capture-boundary record of kind `stop` in epoch e.
The lookup read a seq when a scope read yielded a version of it.
A seq whose versions conflict, one of them the event a claim names, gives no contradiction, only the conflict; a claim at a seq the lookup did not read is not checked.

**Existence facts.** `no-key`, `evidence`, `ordering-uncertain`, `correlation`, `seq-gap` and `open-window` state a condition of the lookup once;
[SEM §7.2] names no record or seq as their witness, so they carry none.

`boundary`, [FMT §10]'s nested `boundary` names and the capture:

| Key | Type | Present | Value |
|---|---|---|---|
| `capture_id` | string | always | the capture of the boundary (UUID) |
| `seq` | string | always | the boundary record's seq |
| `boundary_kind` | string | always | its [JSONL §3] name |
| `ts` | string | in a `barrier` fact | the boundary record's `ts_utc_ns` |
| `epoch` | number | always | the boundary record's epoch |
| `gap_start`, `gap_end` | strings | in a `barrier` fact, each when present | the gap bounds |

**Comparator.** Facts are ordered by `reason`, by the fact table;
then by `hours`, element by element, an array before a longer one that it begins;
then by each of `pack`, `block`, `offset`, `seq`, `defect`, `coverage`, `boundary` and `basis`, in that order, a fact without the key before one with it, then by value.
Numbers and decimal strings compare numerically, an `i64` signed; UUIDs by their 16 bytes;
`defect` in the order `truncated`, `corrupt-block`, `unknown-codec`; `basis` element by element, `epoch` before `time`, an array before a longer one that it begins;
a coverage object by `capture_id`, `seq_first`, `seq_last`, `time_start` and `time_end`, each absent before present,
  then by its [JSONL §4] `unknown` list entry by entry, an absent list first and a list before a longer one that it begins,
  an entry by `tag`, `value_type`, then its value's bytes, unsigned, a value before a longer one that it begins;
a boundary by `capture_id`, `seq`, `boundary_kind` (its numeric value), `epoch`, `ts`, `gap_start` and `gap_end`, each absent before present.
Two distinct facts differ in some key, so the order is total.

#### Early-return results

A lookup whose primary is missing, conflicts within its scope's read, or has no association key ends after the scope of the primary's hour ([SEM §7.2] Scopes read).
Its result is `incomplete`, has no `window_end`, holds in `records` only the versions of the primary, and searches no scope after the primary's;
no version is in the window or bounds it.
Its facts are of two kinds:
- **kept**, the facts its read of the primary's scope records before the primary is settled, about the scope, a pack, a block or a version at any seq:
  `cold`, `conflicted`, `unevaluated`, `read`, `index`, and `scope-breach` above or below p;
- **not reported**, the facts that depend on classifying versions against the primary's key:
  `conflict` above p, `coverage`, `barrier`, `capture-boundary`, `ordering-uncertain`, `evidence`, `seq-gap`, `open-window`, `unavailable` and `contradiction`.

The three early returns:
- **missing primary**: the primary's scope yielded no version of p, and the scope is not indexed, conflicted, read with a defect other than a `coverage` entry, read with a scope breach,
  or a `coverage` entry of a pack read in it meets p and the primary's hour.
  Facts: `no-key` and the kept facts.
  No `key`; `records` is `[]`; the explaining `coverage` entry is no fact of its own.
- **conflicting primary**: the primary's versions conflict within its scope's read.
  Facts: `conflict` (`hours` [`key.hour`], `seq` p), `no-key` and the kept facts.
  Every version of p is kept, `conflict` true, `roles` [`primary`]; `key` is the first version's.
- **primary without a key**: its direction is neither `host-to-equipment` nor `equipment-to-host`, or its SessionID or System Bytes is unavailable.
  Facts: `no-key`, `correlation` when it carries `correlation-incomplete`, and the kept facts; `key` is a diagnostic.

A primary whose versions conflict only across scope reads is not an early return:
the lookup goes on through every hour scheduled, its `key` is the first version's, and its result follows the rules above,
its `no-key` fact beside the `conflict` at p whose `hours` hold the primary's hour and each later hour whose read yielded p.
So a result is an early return exactly when it holds `no-key` and no `conflict` fact at p naming more than one hour.

A primary that is missing where nothing explains it, or that is no primary, is the error form.

## 6. zstd vectors

Every vector uses codec `none` for its blocks and footer unless a codec is its subject.

### 6.1 Encoder versions

RFC 8878 fixes what a zstd frame decodes to, not the bytes an encoder produces: two versions of one encoder can encode the same input differently.
- **Reading** never depends on an encoder: every check of §1 runs against the committed files, whatever encoder produced them.
- **Regeneration**, the generator checking its own output against the committed files, compares by the encoder that produced them, which the manifest records as `zstd_encoder`:
  - when the generator runs with that encoder, every regenerated file equals the committed one byte for byte, the manifest included;
  - when it runs with another encoder, or cannot establish which one it runs with,
    each vector that depends on the encoder is compared by decoded equivalence — the same file header and pack metadata bytes;
    block by block, each encoder-made zstd body equal once decoded and every other body byte for byte; and an identical export, where the vector has one —
    and its files that hold offsets (`verify.json`, `queries.json`, `truncation.json`, `patch.tpk`, `patch.verify.json`, `pack-<n>.verify.json`, `reads.json` and `lookups.json`) are not compared;
    its footer, whose F-2 offsets, lengths and CRCs follow the compressed sizes, is not compared either, the read-path checks of the committed files covering it;
    every other file is compared byte for byte, the manifest except its `zstd_encoder` value, and the generator reports that the corpus is due for regeneration.
- A vector **depends on the encoder** when one of its packs (`pack.tpk`, `patch.tpk` or a `pack-<n>.tpk`) holds a block body or a footer of codec `zstd` that the encoder produced.
  The manifest's `codec` (§3) lists the codecs a vector's packs hold, not this dependence.
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

How the corpus realizes [FMT §16] and [SEM §9] clauses whose wording leaves a choice:
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
- **A read over several packs** ([SEM §7.4]): the read is given exactly the packs a read of `reads.json` names, in the order it names them;
  they stand for the active views of the scopes a query selects, and no active view is computed from them.
- **The source of a lookup**: a `source` object (§5.11) denotes one observation of one tool ([STO §5] Observation of a lookup), fixed from its **source packs** alone:
  exactly the packs `view` and `evidence` list, together.
  A pack of the vector that neither lists takes no part in the lookup: not in a scope, not in the evidence, not in a barrier.
  The packs of a vector have one `tool_id`; a `view` pack is a segment, an archive or a repair pack, an `evidence` pack a segment or an archive whose period lies inside one UTC hour.
  - **Scopes**: the `view` packs grouped by `capture_id` and the hour of `period_start`.
    Each scope's view is its active view ([STO §4]), every archive's replacement set and every repair pack taken as committed;
    a scope whose highest rank holds two different complete sets is `conflicted`, and the lookup reads none of its packs.
    An hour without a `view` pack of the capture is a scope without packs.
  - **Indexed**: with `complete` true, every scope but the scope (`capture_id`, hour of `period_start`) of each `evidence` pack, which a catalog would not index ([STO §5]);
    with `complete` false, no scope.
  - **Per-capture evidence** ([STO §5] Per capture): folded from the F-5 statistics of every source pack of the capture, `view` and `evidence` alike:
    every distinct `boundary` entry, those equal in every field kept once, and the smallest `close_seq` of each epoch.
    It is partial when `complete` is false or when any source pack has no valid footer.
  - **Barriers**: the `stop-unclean` boundaries of the per-capture evidence of every capture of the source packs.
  - **View order**: the generation's member first, unless a patch based on the generation replaced it, then the scope's other view packs in the order `view` lists them.
    [STO §4] defines the members of a view, not their order; the corpus fixes it, as a reader fixes the order of the packs it reads.
    The order decides the representatives ([SEM §7.4] Conflicts), so the `pack` and `block` of each record, and the order of `searched`'s `packs`.

## 9. Vector catalogue

Every clause of [FMT §16] and of [SEM §9] is mapped below to the vectors that exercise it, or marked as planned elsewhere:
`6b3` (the corpus vectors of [STO §8]), `phase 7` (extracts and redaction),
phases of `tracepack-impl-plan.md` that extend this corpus under a later schema value or the same one.
A clause whose subject no result shows, how often a read reads a block or whether it excludes a cluster, is a test of the reference implementation instead of a vector:
the results of §5.10 hold neither, since pruning is outside the contract (§5.4, §5.10).
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
| transaction candidate selection ([SEM §7.2]) | `tx-candidate-selection` |
| identical decoded body bytes under two framings | `framing-two-framings` |
| a block of several records checked on gathered headers | `framing-gathered-headers` |
| `record_header_len` below 44 | `framing-header-len-below-44` |
| `record_count × record_header_len` overflow | `framing-header-section-overflow` |
| envelope / F-2 `record_header_len` mismatch | `framing-header-len-index-mismatch` |
| a pack mixing header lengths | `framing-header-extension` |
| codec stream short or malformed after a valid header section | `framing-zstd-malformed`, `framing-zstd-short`, `framing-zstd-short-checksum` |
| a block holding one oversized record | `framing-record-over-threshold` |
| a validating writer whose encoded block has an injected I-2 defect | `validation-i2-injection` |
| a payload-only identity conflict between two packs | `multi-payload-only-conflict` |
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
| repeated System Bytes; a repeated transaction key: a completed transaction, then an unanswered primary | `tx-repeated-key` |
| cross-pack transactions; a reply in the next hour's scope | `tx-cross-pack` |
| an outstanding primary on epoch E1, a refused socket E2, then the E1 reply | `tx-refused-socket` |
| a primary without System Bytes; an otherwise unanswered primary followed in its window by a reply-direction record whose System Bytes are unavailable | `tx-unavailable-fields` |
| a window closed by a same-key primary, by a socket-close of the epoch, by a clean `stop` of another epoch, and not within the scopes read | `tx-window-bounds` |
| a `close_seq` naming an annotation, its F-2 entry true; closure evidence at or below the primary's seq | `tx-closure-contradiction` |
| a `close_seq` naming a seq with two versions, one of them the socket-close | `tx-closure-conflict` |
| a conflicted scope in the primary's hour and in a later hour | `tx-conflicted-scope` |
| a pack of hour H whose truthful blocks lie in hour K; a same-hour clock step that puts records outside a segment's period | `tx-scope-breach` |
| a valid match, a wrong stream, an F0 abort, a wrong-stream F0, a control record whose bytes 6–7 look like F + 1, two valid matches | `tx-candidate-selection` |
| a possible same-key primary with System Bytes unavailable, a reply before and after it; an unknown-direction record | `tx-possible-primary` |
| a conflict within a scope on a candidate and on an unrelated record after the window, and across scopes on the primary and on a candidate | `tx-conflicts` |
| a block whose records disagree with its F-2 entry | `tx-index-mismatch` |
| another version of a window seq in the hour before the primary's, after a backward clock step; a seq of the window with no version in the scopes read | `tx-clock-step` |
| a `coverage` entry meeting the window but not the hours scheduled, and the reverse; a coverage entry and a barrier meeting only a conflicted hour | `tx-coverage-barrier` |
| an `ordering-uncertain` record of the epoch below the primary; a capture-boundary of the epoch outside the hours read | `tx-epoch-evidence` |
| a pack with `quality_evaluated` false; a `Reject.req` with SType unavailable; a T3 `timer-expiry` without identifiers | `tx-outcome-records` |
| (beyond the clauses) the epoch barrier, lifted only by a closing record of the primary's epoch ([SEM §7.2], [STO §5] Completeness) | `tx-epoch-barrier` |
| (beyond the clauses) a source that is not complete, and a pack whose evidence alone counts ([STO §5]) | `tx-unindexed-source` |
| two captures interleaved block by block in capture order, with a filter that rejects one capture's next record | `multi-capture-interleave` |
| backward timestamps within a block, across the blocks of a pack and across packs, in time order | `multi-time-backward` |
| equal timestamps across captures, across clusters and across the versions of one record (two and three versions) | `multi-equal-timestamps` |
| a record held identically by two packs, yielded once | `multi-identical-copy` |
| a conflict of two versions with both, one and no version selected | `multi-conflict-selection` |
| copies differing only in a reserved `record_flags` bit, only in the extension area, in `record_header_len` | `multi-header-conflicts` |
| copies differing only in the payload, payloads not requested | `multi-payload-only-conflict` |
| versions of one seq with different timestamps, a time range selecting one | `multi-conflict-time-range` |
| a cluster with one block the summaries exclude and one they do not (both read) | `multi-cluster-mixed-exclusion` |
| a cluster excluded whole (not read, no conflict listed) | a test of the reference implementation (§9) |
| a cluster spanning two hours | `multi-cluster-two-hours` |
| a conflict limit reached exactly and exceeded by one | `multi-conflict-limit` |
| a walked block whose computed summary excludes its cluster (read once only) | `multi-walked-excluded`; that it is read once only, a test of the reference implementation (§9) |
| a pack that is not finalized beside its archive, in both orders | `multi-unfinalized-beside-archive` |
| an extract beside its source | phase 7 |
| redaction ([SEM §8]) | phase 7 |

### 9.3 [STO §8] vectors

Every corpus vector of [STO §8] is planned in 6b3, except the pack metadata whose replacement-set values are rejected, which is `bootstrap-replacement-set-size` and `bootstrap-replacement-set-index`,
and the recovery vectors (`stop-unclean` recovery, the crash with an empty spool), which are deferred with recovery itself (G5-91).
The Service vectors of [STO §8] are not part of the corpus, as [STO §8] states, and no phase of this corpus plans them.

### 9.4 Vectors

Group prefixes: `basic-`, `framing-`, `footer-`, `hsms-`, `validation-`, `verify-`, `repair-`, `bootstrap-`, `sem-`, `sample-`,
and, for class `multi-pack`, `multi-` (reads over several packs) and `tx-` (transaction lookups).
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
| `multi-capture-interleave` | multi-pack | two captures whose blocks interleave by `ts_min` in capture order, and a filter that rejects one capture's next record; `reads.json` |
| `multi-time-backward` | multi-pack | timestamps running backward within a block, across blocks and across packs, read in time order; `reads.json` |
| `multi-equal-timestamps` | multi-pack | equal timestamps across captures, equal `ts_min` across clusters, and two and three versions of one record at one timestamp; `reads.json` |
| `multi-identical-copy` | multi-pack | one block held identically by two packs, read in both orders: yielded once, no conflict; `reads.json` |
| `multi-payload-only-conflict` | multi-pack | copies differing in one message-text byte, read with and without payloads: a conflict; `reads.json` |
| `multi-conflict-selection` | multi-pack | a conflict of two versions differing in `dir`, with both, one and no version selected, listed each time; `reads.json` |
| `multi-header-conflicts` | multi-pack | copies differing only in a reserved `record_flags` bit, in an extension byte and in `record_header_len`: three conflicts; `reads.json` |
| `multi-conflict-time-range` | multi-pack | two versions of one seq with different timestamps and a time range selecting one: both read, the conflict listed; `reads.json` |
| `multi-cluster-mixed-exclusion` | multi-pack | a cluster of a block the time range excludes and one it does not: both read, the conflict listed; `reads.json` |
| `multi-cluster-two-hours` | multi-pack | a cluster spanning two hours, in capture and time order; `reads.json` |
| `multi-conflict-limit` | multi-pack | three conflicts, read with a bound of three (success) and of two (`conflict-limit`); `reads.json` |
| `multi-walked-excluded` | multi-pack | an unfinalized pack whose walked block's computed summary excludes its cluster; `reads.json` |
| `multi-unfinalized-beside-archive` | multi-pack | an unfinalized segment beside its archive, read in both orders; `reads.json` |
| `tx-repeated-key` | multi-pack | a completed transaction, then an unanswered primary of the same key; `lookups.json` |
| `tx-cross-pack` | multi-pack | replies in another segment of the primary's hour and in the next hour's scope; `lookups.json` |
| `tx-refused-socket` | multi-pack | an outstanding primary on E1, a refused socket E2, then the E1 reply; `lookups.json` |
| `tx-unavailable-fields` | multi-pack | a primary without System Bytes, and a reply-direction record whose System Bytes are unavailable in a window; `lookups.json` |
| `tx-window-bounds` | multi-pack | windows bounded by a same-key primary, a socket-close, a clean `stop` of another epoch, and by no record read; `lookups.json` |
| `tx-epoch-barrier` | multi-pack | a `stop-unclean` barrier beside a `stop` of another epoch, which bounds the window but keeps the barrier, and beside a socket-close of the primary's epoch, which lifts it; `lookups.json` |
| `tx-closure-contradiction` | multi-pack | footer claims naming an annotation, the primary's seq, a `stop` of another epoch, a socket-close as a `stop`, and a seq below the primary: contradictions; `lookups.json` |
| `tx-closure-conflict` | multi-pack | a `close_seq` naming a seq with two versions, one of them the socket-close: the conflict, no contradiction; `lookups.json` |
| `tx-conflicted-scope` | multi-pack | two archives of equal rank in the primary's hour (no key) and in a later hour (a match beside it); `lookups.json` |
| `tx-scope-breach` | multi-pack | a pack of hour H whose blocks lie in another hour, a same-hour clock step, and a breach that explains a missing primary; `lookups.json` |
| `tx-candidate-selection` | multi-pack | a valid match, a wrong stream, an F0 abort, a wrong-stream F0, a control record that looks like F + 1, two valid matches, and a key naming a reply (`not-primary`); `lookups.json` |
| `tx-possible-primary` | multi-pack | a possible same-key primary between two replies, and an unknown-direction record; `lookups.json` |
| `tx-conflicts` | multi-pack | conflicts within a scope and across scopes, on the primary, a candidate and a record after the window, and a conflicting primary whose first version has no function; `lookups.json` |
| `tx-index-mismatch` | multi-pack | a block whose records disagree with its F-2 entry beside a match; `lookups.json` |
| `tx-clock-step` | multi-pack | versions of window seqs in the hour before the primary's, after a backward clock step: outside the comparison, and a seq gap; `lookups.json` |
| `tx-coverage-barrier` | multi-pack | `coverage` entries meeting the window or the hours alone, and both; a barrier meeting only a conflicted hour; a barrier on both bases; `lookups.json` |
| `tx-epoch-evidence` | multi-pack | an `ordering-uncertain` record below the primary, and a capture-boundary in an hour not read; `lookups.json` |
| `tx-outcome-records` | multi-pack | a pack with `quality_evaluated` false, `Reject.req` records with SType unavailable and available, and a T3 `timer-expiry` without identifiers; `lookups.json` |
| `tx-unindexed-source` | multi-pack | a source that is not complete, and a pack whose evidence alone counts, its scope not indexed; `lookups.json` |

## 10. Schema versions

The `corpus` value names the schemas of §3 and §5 together.
It is a draft until tracepack v1.0.0: a change to a schema before then bumps the value, and the change is recorded in the spec changelog.
Adding a vector, a query or a case keeps the value; changing what an existing file states, a schema, or the classes and files a schema allows takes a new one.
- `tracepack-corpus/1` (spec v2.27): the classes `read`, `rejection`, `truncation` and `repair`, and the schemas of §5.1–§5.9.
- `tracepack-corpus/2` (spec v2.30): adds the class `multi-pack` and its files (§2, §3), the pack numbers and hours of §4,
  the schemas of `reads.json` (§5.10) and `lookups.json` (§5.11), and the source a lookup's `source` denotes (§8).
  The files of the other classes keep their form; only the manifest's `corpus` and `spec_version` values differ from those of `/1`.
