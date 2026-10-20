# tracepack — conformance corpus

Status: current (2026-10-20) — v2.34, tracepack format 1.1, schema `tracepack-corpus/4` (draft until tracepack v1.0.0).
Normative.
[FMT §16] states what the corpus holds; this document defines its files, their schemas and the vector catalogue.
The format is documented for any reader;
the Go module is the only implementation, and the corpus binds format-level reading only (G5-199, G5-210).

Depends on: [FMT §2] portable encoding, [FMT §5] TLV registries, [FMT §7.2] HSMS header fields, [FMT §10] footer and its validation,
[FMT §13] bootstrap and verification, [FMT §16] corpus contents, [JSONL] value forms and the export,
[SEM §2] records unobserved by design, [SEM §3] `decode_status`, [SEM §8] masking and the encryption of originals.

References: `[CORPUS §n]` = this document; `[FMT §n]` = `tracepack-format.md`, `[JSONL §n]` = `tracepack-jsonl.md`, `[SEM §n]` = `tracepack-semantics.md`, `[STO §n]` = `tracepack-storage.md`, `[OVW §n]` = `tracepack-overview.md`; `[FMT I-n]` = invariant I-n of the format document.
Each rule is defined in exactly one document; the others only reference it.

## 1. Conformance

An implementation proves conformance against the committed files of the corpus; it never needs to regenerate them.
JSON files are compared as JSON values (§4); `.jsonl` and `.tpk` files are compared byte for byte.
- A **reader** conforms when, for every vector it is given:
  - of class `read` or `truncation`, it opens `pack.tpk` and its export equals `export.jsonl` ([JSONL]);
  - of class `rejection`, it rejects `pack.tpk` at bootstrap for the reason `rejection.json` names (§5.3);
  - with `fields.json`, it reads the HSMS header fields of each data and control record as listed (§5.5);
  - with `footer.json`, it accepts or rejects the footer as `accepted` says, and, where it reads footer values, reads them as `stored` lists and computes them from the records as `recomputed` lists (§5.8);
  - of class `truncation`, every cut of `pack.tpk` meets its row of `truncation.json` (§5.7).
- A **writer** conforms when a conforming reader round-trips its output.
  The corpus holds one writer behaviour as bytes, the validating writer of `validation-i2-injection` (§9.4):
  another writer conforms when its output stops before the same block and holds no trailer.
- An implementation that does not read footer values skips the `stored` and `recomputed` parts of `footer.json`;
  the other files still bind it.
- A reader's own limits, an I/O error or a cancellation are never goldens ([FMT §13], [JSONL §7]): no expectation depends on them.
- Verification reports, query results, classifications, reads over several packs and transaction lookups are tests of the Go module, not of the corpus (G5-199, G5-210).

## 2. Layout

The corpus is a directory holding `manifest.json` (§3), `primitives.json` (§5.1), a `README.md` (informative), and one directory per vector, named by the vector's `id`.
A vector directory holds the files its class gives below, under these fixed names, and no others:

| File | Present | Content |
|---|---|---|
| `pack.tpk` | every class | the vector's pack; for a `truncation` vector, the base pack every cut is taken from |
| `export.jsonl` | class `read`, `truncation` | the expected export of `pack.tpk` ([JSONL]) |
| `rejection.json` | class `rejection` | the expected bootstrap rejection (§5.3) |
| `fields.json` | optional, every class | the HSMS header fields of each data and control record (§5.5) |
| `footer.json` | optional, every class | footer expectations (§5.8) |
| `truncation.json` | class `truncation` | the expectation table of every cut (§5.7) |

A vector of class `rejection` has no `export.jsonl`: a pack rejected at bootstrap has none ([JSONL §8], [FMT §13]).

## 3. Manifest and identification

`manifest.json` identifies the corpus and lists its vectors.
The corpus is identified by its schema, the spec version and the format version together,
so goldens written under different definitions are told apart ([FMT §14]).

| Key | Type | Present | Value |
|---|---|---|---|
| `corpus` | string | always | `"tracepack-corpus/4"`, the schema of every file of this document (§10) |
| `spec_version` | string | always | the spec version the goldens follow: `"2.33"` |
| `format_version` | string | always | `"1.1"`, the format revision the corpus targets ([FMT §4], [FMT §14]); a compatibility or rejection vector may hold a pack of another version, such as `basic-future-minor` (minor 2) or the bootstrap vector of an unsupported version |
| `zstd_encoder` | string | always | the identity and version of the zstd encoder that produced the encoder-made zstd blocks and footers of the corpus, as its generator records it (§6.1) |
| `vectors` | array of objects | always | one per vector, ascending by `id` |

A vector entry:

| Key | Type | Present | Value |
|---|---|---|---|
| `id` | string | always | kebab-case ASCII, prefixed by its group (§9.4); stable, never reused for another vector |
| `title` | string | always | one line, informative |
| `cites` | array of strings | always, never empty | the clauses the vector exercises, each `FMT §n`, `FMT I-n`, `SEM §n`, `STO §n`, `JSONL §n` or `CORPUS §n`, primary clause first; informative |
| `class` | string | always | `read`, `rejection` or `truncation` (§2) |
| `labels` | array of strings | always, `[]` when none | ascending; `damaged`: the pack models damage after writing (a failed CRC, a cut, a broken codec stream); `nonconforming-writer`: the pack holds bytes, every CRC valid, that a conforming writer never writes; `pre-v2.13-sample`: the sample of the format definition before v2.13 ([FMT §14]) |
| `codec` | string | always | `none` when every block body and the footer of `pack.tpk` use codec `none`, `zstd` when they all use `zstd`, `mixed` otherwise, an unknown codec included |
| `jsonl_schema` | string | for class `read` and `truncation` | the export schema of the vector's `.jsonl` file: `"tracepack-jsonl/1"` when its pack's `format_minor` is 0, `"tracepack-jsonl/2"` otherwise ([JSONL §5]) |
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
- An optional key is omitted when it has no value, never written as `null` and never given a placeholder such as `-1`.
- A key holding an array is present as its schema's Present column says, like any other key;
  a present array with no elements is `[]`, never omitted.
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
| `digest_keys` | array of objects | always | `{key_id, key}`: the published HMAC-SHA-256 test keys ([FMT §2]), `key_id` a string and `key` base64; ascending by `key_id` |
| `encryption_keys` | array of objects | always | `{key_id, key}`: the published AES-SIV-CMAC-512 test keys ([FMT §2]), `key` base64 of 64 bytes; ascending by `key_id` |
| `hmac` | array of objects | always | `{name, key_id, domain, input, digest}`: the [SEM §8] digest of `input` (base64, the original bytes) in `domain` (a string) under the key named, `digest` base64 of 32 bytes; ascending by `name` |
| `aes_siv` | array of objects | always | `{name, key_id, capture_id, seq, first_offset, associated_data, plaintext, output}`: `capture_id` in canonical form, `seq` and `first_offset` strings, `associated_data` base64 of the 53 bytes of [SEM §8] Encryption, `plaintext` base64, and `output` base64 of the `siv` followed by the ciphertext; ascending by `name`, the first entry `label`, whose `associated_data` an implementation checks byte for byte |

### 5.2 `verify.json`

Removed in v2.32 (G5-210): the verification report and its outcomes are defined in [FMT §13]; its projection is a test of the Go module.

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

Removed in v2.32 (G5-210): when a read reports each `incomplete` reason, and with which offsets, is defined in [SEM §7.4]; query expectations are tests of the Go module.

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

Removed in v2.32 (G5-202): repair is no longer an operation of tracepack, and no rule of this section survives.

### 5.7 `truncation.json`

The expectations for every cut of `pack.tpk`: the file of its first L bytes, for every L with 0 ≤ L < `size` (G5-178).
An implementation makes each cut itself.

| Key | Type | Present | Value |
|---|---|---|---|
| `size` | string | always | the size of `pack.tpk` |
| `rows` | array of objects | always | ascending by `from`, contiguous, covering every L from 0 to `size` − 1; consecutive lengths with equal expectations are one row |

A row:

| Key | Type | Present | Value |
|---|---|---|---|
| `from`, `to` | strings | always | the cut lengths L the row covers, `from` ≤ L ≤ `to` |
| `rejection` | string | iff the cut is rejected at bootstrap | the code (§5.3) |
| `exported_records` | string | iff not rejected | the records of the cut's export |

For every cut that is not rejected, the records exported are those of the blocks that lie wholly before the cut,
and the cut's export is the header line of `export.jsonl` followed by that many of its first record lines: no block of the validated prefix is lost.

### 5.8 `footer.json`

The export leaves the footer out ([JSONL §5]), so a vector whose subject is a footer value publishes a projection of it in three parts:

| Key | Type | Present | Value |
|---|---|---|---|
| `stored` | object | iff the footer is structurally readable, below | the F-3 and F-5 values below, as stored |
| `accepted` | bool | always | the pack is finalized ([FMT I-5]) and its footer passes [FMT §10] validation, so its statistics are available |
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
  a `boundary` entry holds exactly one `seq`, a `u64`, exactly one `boundary_kind`, a `u8`, and at most one `gap_start` and one `gap_end`, each an `i64`;
- each F-5 `seq_range` entry holds 16 bytes.

No other value is checked, and when a check fails `stored` is omitted.

The projection: `{blocks, f5}`.
`blocks` holds one object per F-2 entry (in `stored`) or per validated block (in `recomputed`), ascending by `block`:
`{block, close_seqs, boundaries}`, `close_seqs` the F-3 `epoch` entries' `close_seq` values as `{epoch, seq}`, entries without `close_seq` left out,
and `boundaries` the F-3 `boundary` entries as `{seq, kind, gap_start?, gap_end?}`, `kind` the `boundary_kind` name and each bound present iff the entry has it.
`f5` is `{epochs, boundaries, seq_ranges}`: `epochs` one `{epoch, close_seq?}` per F-5 `epoch` entry; `boundaries` as above;
`seq_ranges` one `{first, last}` per F-5 `seq_range` entry.
Every entry gives one object, so entries that repeat an epoch or a seq are all listed.
`close_seqs` are ascending by `epoch`, then by `seq`;
`epochs` ascending by `epoch`, an object without `close_seq` before those with one, then by `close_seq`;
`boundaries` ascending by `seq`, then by the numeric `boundary_kind`, then by `gap_start`, then by `gap_end`, an absent bound before every present one and present bounds compared as `i64`;
`seq_ranges` ascending by `first`, then by `last`;
objects equal in every key are interchangeable.
`seq`, `close_seq`, `gap_start`, `gap_end`, `first` and `last` are strings, `epoch` and `block` numbers; arrays are `[]` when empty.
Other footer fields are outside the projection.
F-3's `close_seq` is the block-local minimum and F-5's the pack-wide minimum ([FMT §10]):
an epoch ended at seq 4 in block 0 and at seq 7 in block 1 has `close_seq` 4 and 7 in the blocks' F-3 entries and 4 in F-5.

### 5.9 `classify.json`

Removed in v2.32 (G5-210): the classifier's rules are in [SEM §3] and [FMT §7.2], and the Go tests check them, guided by the checklist of §7.

### 5.10 `reads.json`

Removed in v2.32 (G5-199): reads over several packs are defined in [SEM §7.4] and tested by the Go module ([STO §8]).

### 5.11 `lookups.json`

Removed in v2.32 (G5-199, G5-203, G5-204): transaction lookup, its results and its early returns are defined in [SEM §7.2] and tested by the Go module ([STO §8]).

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
    and its `truncation.json`, whose cut lengths follow the compressed sizes, is not compared;
    its footer, whose F-2 offsets, lengths and CRCs follow the compressed sizes, is not compared either, the read-path checks of the committed files covering it;
    every other file is compared byte for byte, the manifest except its `zstd_encoder` value, and the generator reports that the corpus is due for regeneration.
- A vector **depends on the encoder** when its `pack.tpk` holds a block body or a footer of codec `zstd` that the encoder produced.
  The manifest's `codec` (§3) lists the codecs a vector's pack holds, not this dependence.
  Vectors without such a body or footer — codec `none`, the hand-built frames of §6.2, `basic-unknown-codec`, whose other blocks and footer are codec `none`, and the `fixed` packs —
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

Informative guidance for the Go tests of the classifier (G5-210): [SEM §3] decides every status, and these tables only list the cases worth checking.
The cases of the item predicate of [SEM §3] (`item-decode-error`, `ok-with-trailing`, `ok`) are each a frame of `sem-item-validity` or `sem-decode-status-classified` (§9.4), in the vector's pack.
E5 §9 is the base of [SEM §3]; format codes are written in octal as in E5 Table 1, `0o22` for the localized string.
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
- **A pack truncated at every byte offset**: one base pack and its `truncation.json` (§5.7), under codec `none` and codec `zstd`.
- **Control frames**: a `Select.rsp`, a `Deselect.rsp`, a `Reject.req` and a `Linktest.req`, with `fields.json` listing their bytes 6 and 7, read positionally ([FMT §7.2]).

## 9. Vector catalogue

Every clause of [FMT §16] and of [SEM §9] that the corpus serves is mapped below to the vectors that exercise it.
A clause that the format-reading assertions of §1 cannot show —
a verification report, a query, a classification, a read over several packs or a transaction lookup —
is a test of the Go module instead of a vector (G5-199, G5-210).
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
| identical decoded body bytes under two framings | `framing-two-framings` |
| a block of several records checked on gathered headers | `framing-gathered-headers` |
| `record_header_len` below 44 | `framing-header-len-below-44` |
| `record_count × record_header_len` overflow | `framing-header-section-overflow` |
| envelope / F-2 `record_header_len` mismatch | `framing-header-len-index-mismatch` |
| a pack mixing header lengths | `framing-header-extension` |
| codec stream short or malformed after a valid header section | `framing-zstd-malformed`, `framing-zstd-short`, `framing-zstd-short-checksum` |
| a block holding one oversized record | `framing-record-over-threshold` |
| a validating writer whose encoded block has an injected I-2 defect | `validation-i2-injection` |
| a finalized pack whose blocks all pass | `basic-codec-none`, `basic-codec-zstd` |
| a pack truncated at every byte offset | `verify-truncation-none`, `verify-truncation-zstd` |
| a finalized pack whose last block fails | `verify-last-block-failed` |
| a failed block between validated ones, with and without a valid footer | `basic-corrupt-middle`, `verify-corrupt-middle-walked` |
| a finalized pack with an invalid footer, or an F-3 summary that disagrees, every block passing | the `footer-invalid-*` vectors, `verify-f3-disagrees`, `verify-f2-disagrees` |
| a writer defect of each kind | `hsms-system-bytes-set-short` (`field-validity`), `verify-defect-event-field-validity`, `verify-defect-event-payload`, `verify-defect-seq-order`, `verify-defect-hour-span`, `verify-seq-start-above` (`seq-start`) |
| the `seq_start` defect: above, below, after a failed block | `verify-seq-start-above`, `verify-seq-start-below`, `verify-seq-start-after-failed` |
| seq-order and hour-span defects of a finalized pack | `verify-defect-seq-order`, `verify-defect-hour-span` |
| one bootstrap vector per rejection code | `bootstrap-short-object`, `bootstrap-bad-magic`, `bootstrap-header-crc`, `bootstrap-unsupported-version`, `bootstrap-metadata-past-object`, `bootstrap-metadata-crc`, `bootstrap-entry-framing`, `bootstrap-entry-type`, `bootstrap-entry-value`, `bootstrap-entry-repetition`, `bootstrap-entry-nested`, `bootstrap-required-tag`, `bootstrap-conditional-tag`, `bootstrap-replacement-set-size`, `bootstrap-replacement-set-index`; a missing required nested tag: `bootstrap-entry-nested-required` |
| a missing tag whose requirement depends on the records: the pack opens | `bootstrap-record-conditioned-tag` |
| S7F3 PPBODY masked: length, item headers, `decode_status` and HSMS header unchanged; entry, digest and `siv` as published | `redact-encrypted-segment` (`ppbody`) |
| S7F3 and S7F6 carrying one process program in one domain: equal digests | `redact-encrypted-segment` (`ppbody`, `s7f6-same-program`) |
| the same S7F3 under the second digest key and key id: another digest | `redact-second-digest-key` |
| one PPBODY in two captures: equal digests, different ciphertexts and `siv`s | `redact-capture-a`, `redact-capture-b` (one pack each; each pins its own bytes, and a Go test compares the two) |
| S7F23 through path `4`: every CCODE and PPARM leaf masked, every list header kept | `redact-encrypted-segment` (`s7f23`) |
| an S7F3 and an S6F11 with the same message text: only the S7F3 masked | `redact-encrypted-segment` (`s6f11-unmasked`) |
| S7F3 declared shorter and longer than captured: the captured text screened | `redact-encrypted-segment` (`declared-short`, `declared-long`) |
| an oversized S7F3; payloads shorter than 14 bytes | `redact-encrypted-segment` (`oversized`, `short-payloads`) |
| not one valid item; trailing bytes; a valid text whose path does not resolve: one whole-text entry each, its ciphertext opaque | `redact-encrypted-segment` (`whole-text-invalid`, `whole-text-trailing`, `whole-text-unresolved`) |
| S13F6 fourth item not a list; S16F15 job lacking `4/3`: whole-text | `redact-encrypted-segment` (`s13f6-not-list`, `s16f15-missing-branch`) |
| S13F6 with an empty data list; empty-list denials of S7F6, S7F26, S7F36; a zero-length PPBODY; a header-only S7F3: no entry | `redact-encrypted-segment` (the `no-target-*` cases) |
| S16F11 format-22 leaf: header kept; a 2-byte leaf: no target bytes; a 1-byte leaf: whole-text | `redact-encrypted-segment` (the `format22-*` cases) |
| S13F6 data set: every FILDAT masked | `redact-encrypted-segment` (`s13f6-fildat`) |
| a PPBODY already all zero: ranges list it, the restore gives it back | `redact-encrypted-segment` (`already-zero`) |
| an `unparsed-entry` annotation masked through `raw`: zeros, entry without `siv` | `redact-annotation-zeroed` |
| a pack under a policy that matched nothing: `redaction_policy` present, `redaction-present` clear, minor 1 | `redact-matched-nothing` |
| malformed entries (empty or odd ranges array, zero length, unsorted or adjacent ranges, a range past `payload_len`, `seq` of a record without `redacted`, a `redacted` record without entry, a missing and an ill-sized `siv`): the pack opens | `redact-recoverable-defects` (one case each) |
| overlapping encrypted entries of one record, an exact duplicate among them, and only adjacent ones: the pack opens | `redact-overlapping-encrypted` |
| a valid masked record early and a defective entry for a later record: the pack opens | `redact-recoverable-defects` (`isolation`) |
| masked records merged, one record in two inputs | `redact-encrypted-archive` |
| a masked pack truncated at every byte offset; one with a corrupt block | `redact-truncation` (class `truncation`), `redact-corrupt-block` |
| an extract requested from an extract or a pack holding `redacted`; one from an unscreened extract; an extract without a policy | deferred with extracts (G5-244) |
| records unobserved by design, an empty-text reply and an abnormal reply kept in full | `thin-fdc-polls` |
| a consumer's close of a plain capture, of a masked and thinned one, of one with no staged record, and merged with its records | `close-plain`, `close-masked-thinned`, `close-no-start-bound`, `close-merged-archive` |
| the version-aware cases: a format 1.0 pack holding the numbers format 1.1 assigns, format 1.0 extracts under a policy, and a pack of minor 2 | `basic-new-tags-segment-1-0`, `basic-new-tags-extract-1-0`, `basic-extract-policy-no-classifier-1-0`, `basic-extract-policy-multi-classifier-1-0`, `basic-future-minor` |
| a sample of the definition before v2.13 | `sample-v010-rows` |

### 9.2 [SEM §9] clauses

| Clause | Vectors |
|---|---|
| one vector per `decode_status` value | `sem-decode-status-classified`, `sem-decode-status-log` |
| one vector per item-validity rule ([SEM §3]) | `sem-item-validity`, with the precedence cases in `sem-decode-status-classified` (§7) |
| a nonzero `capture_origin_mono_ns` | `sem-capture-origin-mono` |
| redaction ([SEM §8]) | the redaction vectors of §9.1 |
| an extract beside its source | deferred with extracts (G5-244) |

The clauses of [SEM §9] on transaction lookup and on reads over several packs are tests of the Go module ([STO §8]);
no vector of the corpus serves them (G5-199).

### 9.3 [STO §8] vectors

None: [STO §8] lists implementation tests of the Go module, not corpus vectors (G5-199).

### 9.4 Vectors

Group prefixes: `basic-`, `framing-`, `footer-`, `hsms-`, `validation-`, `verify-`, `bootstrap-`, `sem-`, `redact-`, `thin-`, `close-`, `sample-`,
and `repair-` for a vector kept from the repair vectors (below).
Files beyond those of the class (§2) are named where a vector has them.

| Vector | Class | Subject |
|---|---|---|
| `basic-empty-pack` | read | §8 |
| `basic-zero-records` | read | §8 |
| `basic-codec-none` | read | several blocks of data, control, transport-event and annotation records, `finalized-consistent` |
| `basic-codec-zstd` | read | the same records under codec `zstd`, the same identifiers (§3) |
| `basic-codec-mixed` | read | the same records, the same identifiers, with blocks of codec `none` and `zstd` in turn |
| `basic-truncated-tail` | read | a pack cut inside its last block |
| `basic-corrupt-middle` | read | a middle block failing its body CRC, valid footer |
| `basic-bad-envelope-crc-indexed` | read | a block failing its envelope CRC, passed by its F-2 entry |
| `basic-bad-envelope-crc-walked` | read | the same in a finalized pack whose footer is rejected: the walk stops there, so the walked blocks are not compared with the trailer's totals ([FMT §13]) |
| `basic-unknown-codec` | read | a block of a codec outside the registry, its envelope, F-2 entry and body CRC valid and its body no encoder's output, every other block and the footer codec `none` |
| `basic-unknown-tags-and-enums` | read | unknown and private metadata tags, unknown enum values in the metadata, the record header and event bodies |
| `basic-unordered-timestamps` | read | `ts_utc_ns` not ascending within and across blocks |
| `basic-max-int` | read | the at-limit values of §8 but `ts_utc_ns` |
| `basic-max-int-extract` | read | an extract with `ts_utc_ns` at both `i64` extremes |
| `basic-seq-over-limit` | read | a block whose first seq is 2^63 |
| `basic-uuid-byte-order` | read | `pack_id` the UUID of `primitives.json` |
| `basic-new-tags-segment-1-0` | read | a format 1.0 segment holding tag 0x0033 as `bytes` twice and a record carrying `quality` bit 7: the bit reserved, exported under `/1` with `bit(7)` and `unknown` ([FMT §5]) |
| `basic-new-tags-extract-1-0` | read | a format 1.0 extract whose `redaction_policy` holds nested 0x0005 and 0x0006 as opaque `bytes` and whose `redaction` entry holds nested 0x0006 twice: exported under `/1` ([FMT §5]) |
| `basic-extract-policy-no-classifier-1-0` | read | a format 1.0 extract under a policy that matched nothing, holding only unclassified records and no `classifier` ([FMT §5]) |
| `basic-extract-policy-multi-classifier-1-0` | read | a format 1.0 extract under a policy with two `classifier` and two `max_frame_len` values inherited from its sources ([FMT §5]) |
| `basic-future-minor` | read | a readable pack of minor 2 holding an unknown tag: read with the newest registry and exported under `/2` ([FMT §14]) |
| `framing-header-extension` | read | blocks of `record_header_len` 44 and 45 to 55, each with its own extension bytes |
| `framing-two-framings` | read | two blocks whose decoded bodies are equal: one record with a long header, several records with 44-byte headers |
| `framing-gathered-headers` | read | a block of several records with extension bytes, seqs and lengths read from the gathered headers |
| `framing-header-len-below-44` | read | a block of `record_header_len` 43 |
| `framing-header-section-overflow` | read | an envelope, its CRCs valid and its F-2 entry agreeing where the footer is valid, with `record_count` 2^18 and `record_header_len` 16384: the product 2^32 exceeds every valid `uncompressed_len` ([FMT §2]), so the block fails (`corrupt-block`), while 32-bit arithmetic wraps it to 0, a header section that would fit |
| `framing-header-len-index-mismatch` | read | an envelope whose `record_header_len` disagrees with its F-2 entry |
| `framing-zstd-malformed`, `framing-zstd-short`, `framing-zstd-short-checksum` | read | the hand-built frames of §6.2, the last with a content checksum; every other block and the footer codec `none` |
| `framing-record-over-threshold` | read | a record larger than the block size threshold, alone in its block |
| `footer-invalid-f2-offset`, `footer-invalid-f2-entry-len`, `footer-invalid-sections`, `footer-invalid-f2-contiguity`, `footer-invalid-seq-order`, `footer-invalid-summary-bounds`, `footer-invalid-seq-set`, `footer-invalid-count-sums`, `footer-invalid-f5-aggregate`, `footer-invalid-trailer-last-seq`, `footer-invalid-header-len`, `footer-invalid-ts-range`, `footer-invalid-epoch-range`, `footer-invalid-epoch-entry`, `footer-invalid-boundary-seq`, `footer-invalid-overflow`, `footer-invalid-empty-stats` | read | one footer validation clause of [FMT §10] each, CRC valid |
| `footer-invalid-trailer-record-count` | read | the trailer's `record_count` off by one, every block walked |
| `footer-seq-range-hides-gap` | read | a block missing a seq whose F-3 summary has no `seq_range` |
| `footer-retired-f3-tags-and-f4` | read | retired F-3 tags and a present F-4, both ignored |
| `footer-close-twice-one-block`, `footer-close-twice-two-blocks` | read | an epoch ended by a socket-close, then a clean `stop`, in one block and across two; `footer.json` |
| `footer-f5-higher-close-seq` | read | F-5 stating the higher `close_seq`: not accepted; `footer.json` |
| `footer-f3-higher-close-seq` | read | in one block, F-3 stating the higher `close_seq`, F-5 recomputed from it: accepted, `finalized-inconsistent`; `footer.json` |
| `footer-epoch-without-closure` | read | an epoch no record ends; `footer.json` |
| `footer-invalid-socket-close-body`, `footer-invalid-stop-body` | read | a socket-close and a clean-stop capture-boundary event whose body breaks another rule: no boundary, no closure, a writer defect; `footer.json` |
| `footer-quality-bit0-data` | read | quality bit 0 on a data record: no boundary; `footer.json` |
| `footer-boundary-ties` | read | a readable footer whose boundary entries are equal in `seq` and kind but differ in their bounds (`nonconforming-writer`); `footer.json` and its ordering (§5.8) |
| `footer-seq-range-ties` | read | a readable footer whose F-5 ranges are equal in `first` but differ in `last` (`nonconforming-writer`); `footer.json` and its ordering (§5.8) |
| `hsms-short-captures` | read | data records of 5 to 14 captured bytes, each field available exactly when its bit is set; `fields.json`, `cases` |
| `hsms-system-bytes-unavailable` | read | a data record whose System Bytes bit is clear while its payload holds them; `fields.json` |
| `hsms-system-bytes-set-short` | read | a data record whose System Bytes bit is set while its payload ends before them (`nonconforming-writer`); `fields.json` |
| `hsms-control-frames` | read | §8; `fields.json` |
| `hsms-predicates` | read | records for the S/F, SessionID and System Bytes predicates, including S/F with one field unavailable and the other differing; `fields.json` |
| `validation-i2-injection` | read | the output of a validating writer stopped before a block with an injected I-2 defect: no such block, no trailer, `unfinalized` |
| `verify-truncation-none`, `verify-truncation-zstd` | truncation | a base pack of two or three blocks under each codec |
| `verify-last-block-failed` | read | `finalized-truncated` |
| `verify-corrupt-middle-walked` | read | a middle block failing its body CRC, its envelope intact, the footer rejected: `corrupt-middle` |
| `verify-f2-disagrees` | read | a block whose records disagree with its F-2 entry, the footer valid |
| `verify-f3-disagrees` | read | a block whose records disagree with its F-3 summary only |
| `verify-defect-event-field-validity` | read | a transport-event record with `field_validity` not 0 |
| `verify-defect-event-payload` | read | a `state-transition` event whose body is not valid: the writer defect alone |
| `verify-defect-seq-order`, `verify-defect-hour-span` | read | the I-12 and I-13 defects in a finalized pack: `finalized-inconsistent` |
| `verify-seq-start-above`, `verify-seq-start-below`, `verify-seq-start-after-failed` | read | the `seq_start` defect: a first block above and below `seq_start`, and a first validated block that starts at `seq_start` after a failed one |
| `bootstrap-short-object`, `bootstrap-bad-magic`, `bootstrap-header-crc`, `bootstrap-unsupported-version`, `bootstrap-metadata-past-object`, `bootstrap-metadata-crc` | rejection | one cause each (§5.3) |
| `bootstrap-entry-framing`, `bootstrap-entry-type`, `bootstrap-entry-value`, `bootstrap-entry-repetition`, `bootstrap-entry-nested` | rejection | `metadata-entry-list`: an entry past the list's end, a known tag of the wrong value type, a metadata `u64` of 2^63, a non-repeatable known tag twice, a `coverage` entry whose nested list breaks framing |
| `bootstrap-required-tag`, `bootstrap-conditional-tag` | rejection | `metadata-required-tag`: a tag required `always` missing; `capture_origin_utc_ns` missing under `time_source = capture-clock` |
| `bootstrap-replacement-set-size`, `bootstrap-replacement-set-index` | rejection | `replacement-set` |
| `bootstrap-record-conditioned-tag` | read | a classified record in a pack without `classifier`: the pack opens ([FMT §13]) |
| `bootstrap-entry-nested-required` | rejection | `metadata-entry-list`: a `redaction` entry without its required nested `seq` |
| `sem-decode-status-classified` | read | a record of each byte-predicate status, `not-attempted` and `not-applicable`, the precedence, control and `max_frame_len` cases of §7; `cases` |
| `sem-decode-status-log` | read | a `log` capture with `reconstructed-ok`, `parse-failed` and `build-rejected` records; `cases` |
| `sem-item-validity` | read | the checklist of §7; `cases` |
| `sem-capture-origin-mono` | read | a capture-clock pack whose `capture_origin_mono_ns` is not 0 ([SEM §4]) |
| `redact-encrypted-segment` | read | a segment masked at write under a policy with encryption, one case per redaction clause of §9.1 that names it; `cases` |
| `redact-second-digest-key` | read | the `ppbody` record of `redact-encrypted-segment` under the second digest key and key id |
| `redact-capture-a`, `redact-capture-b` | read | one PPBODY masked in two captures, one pack each |
| `redact-annotation-zeroed` | read | an `unparsed-entry` annotation masked through `raw` under a policy with encryption: zeros, an entry without `siv` |
| `redact-matched-nothing` | read | a segment under a policy with encryption that matched nothing: minor 1, `redaction_policy`, no entry |
| `redact-recoverable-defects` | read | one case per recoverable entry defect of [SEM §8] Marking, and `isolation`; `cases` |
| `redact-overlapping-encrypted` | read | encrypted entries of one record that overlap, one an exact duplicate, and entries that are only adjacent; `cases` |
| `redact-encrypted-archive` | read | `redact-encrypted-segment`'s records merged from two segments, one record in both |
| `redact-truncation` | truncation | a masked base pack of two or three blocks |
| `redact-corrupt-block` | read | a masked pack with one block failing its body CRC, label `damaged` |
| `thin-fdc-polls` | read | a `raw-stream` capture's S1F3 and S1F4 records carrying `unobserved`, zero-filled, `not-attempted`, with `thinning` entries; an S1F4 with an empty message text, and an S9F7, both kept in full; exported under `/2` |
| `close-plain` | read | a segment holding only a consumer's close at seq 2^63 − 1 of a plain capture: minor 0, `/1`; `footer.json` |
| `close-masked-thinned` | read | the close segment of a masked and thinned `decoded-message` capture: `redaction_policy`, `classifier`, `max_frame_len` and `thinning` inherited, minor 1, `/2` |
| `close-merged-archive` | read | a capture's records and its close merged: two seq ranges, the close counted as one record; `footer.json` |
| `close-no-start-bound` | read | a close whose capture staged no record: `gap_start` absent; `footer.json` |
| `sample-v010-rows` | read | the tracepack v0.1.0 pack of a multi-record block, reported `corrupt`; `source` `fixed`, label `pre-v2.13-sample` ([FMT §14]) |
| `repair-of-patch` | read | a pack of `pack_role` `repair` whose last block fails its body CRC, read like any pack a reader is given ([STO §2]); `source` `fixed`, label `damaged` |
| `repair-inconsistent` | read | a `finalized-inconsistent` archive of generation 1: the only export of an archive's pack metadata, `publisher_epoch` and `replacement_set_*` included ([FMT §5]) |
| `repair-not-needed-commitments` | read | a segment holding a record of quality `redacted`: the only export of that quality bit ([FMT §9]) |

The `multi-` and `tx-` vectors of `tracepack-corpus/2`,
whose files were verification reports, reads over several packs and lookups only, are removed in v0.4.0 (G5-199, G5-210).
The `repair-` vectors lose their repair, patch and verification files (G5-202, G5-210).
v0.4.0 keeps three of them as class `read`, with their pack and export, each for a format clause no other vector serves:
`repair-of-patch`, the corpus's only `repair`-role pack,
kept as committed since no operation of tracepack writes it any more;
`repair-inconsistent`, the only archive whose export the corpus pins;
and `repair-not-needed-commitments`, the only record exported with quality `redacted`.
The other fifteen are removed, `repair-truncation` among them:
its base pack and rows are those of `verify-truncation-none` under another identity.

## 10. Schema versions

The `corpus` value names the schemas of §3 and §5 together.
It is a draft until tracepack v1.0.0: a change to a schema before then bumps the value, and the change is recorded in the spec changelog.
Adding a vector or a case keeps the value; changing what an existing file states, a schema, or the classes and files a schema allows takes a new one.
- `tracepack-corpus/1` (spec v2.27): the classes `read`, `rejection`, `truncation` and `repair`, and the schemas of §5.1–§5.9.
- `tracepack-corpus/2` (spec v2.30): adds the class `multi-pack` and its files (§2, §3), the pack numbers and hours of §4,
  the schemas of `reads.json` (§5.10) and `lookups.json` (§5.11), and the source a lookup's `source` denotes (§8).
  The files of the other classes keep their form; only the manifest's `corpus` and `spec_version` values differ from those of `/1`.
- `tracepack-corpus/3` (spec v2.32): the corpus asserts format reading only (G5-199, G5-210).
  Removes the classes `repair` and `multi-pack` with their files,
  and the files `verify.json`, `queries.json` and `classify.json` (§2, §5.2, §5.4, §5.6, §5.9–§5.11);
  removes the offsets, pack numbers, hours and coverage objects of §4, which no remaining schema uses;
  removes `repair_options` and the row keys `outcome`, `verify` and `repair` from `truncation.json` (§5.7),
  so adjacent rows whose remaining expectations are equal become one row;
  and defines `footer.json`'s `accepted` on its own instead of by `verify.json` (§5.8).
  The kept files of the remaining classes keep their form otherwise; the manifest's `corpus` and `spec_version` values change.
- `tracepack-corpus/4` (spec v2.33): format 1.1 (G5-233).
  The manifest's `format_version` names the format revision the corpus targets, and its `jsonl_schema` moves into each vector of class `read` or `truncation` (§3);
  `primitives.json` gains `digest_keys`, `encryption_keys`, `hmac` and `aes_siv` (§5.1);
  `footer.json` gains the bounds of a boundary and the F-5 seq ranges, with their order (§5.8), so every `footer.json` is regenerated;
  and the redaction, unobserved, close and version-aware vectors are added (§9).
  Every earlier vector keeps its `pack.tpk` and its `export.jsonl` byte for byte under `/1`.
