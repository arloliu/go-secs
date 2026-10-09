# tracepack — canonical JSONL export

Status: current (2026-10-10) — v2.29, tracepack format 1.0, schema `tracepack-jsonl/1` (draft until tracepack v1.0.0).
Normative, language-agnostic. [FMT §15] states what the export is for; this document defines its bytes.

Depends on: [FMT §1] conventions, [FMT §2] portable encoding, [FMT §4] file header, [FMT §5] TLV encoding and registries,
[FMT §7.1] record header, [FMT §8] payloads, [FMT §9] enums and bit sets, [FMT §13] bootstrap and validated blocks.

References: `[JSONL §n]` = this document; `[FMT §n]` = `tracepack-format.md`, `[SEM §n]` = `tracepack-semantics.md`, `[OVW §n]` = `tracepack-overview.md`, `[CORPUS §n]` = `tracepack-corpus.md`.
Each rule is defined in exactly one document; the others only reference it.

## 1. Conformance

An exporter conforms when, for every pack its reader opens, it writes exactly the bytes this document defines,
so that any two conforming exporters produce identical output for the same pack ([FMT §15]).
The output depends on the pack's bytes alone:
an exporter that cannot read a block, the footer or a part of the block region because of its own resource limits, rather than damage,
fails the export instead of leaving that part out (§7).
The conformance corpus ([FMT §16], [CORPUS]) holds the expected export of each of its vectors.

## 2. Text form

- The export is UTF-8 JSON text (RFC 8259): a sequence of lines, each one JSON object followed by one LF (0x0A), the last line included.
- There is no insignificant whitespace: no space, tab, CR or LF outside strings, other than each line's final LF.
- Line 1 is the **header line** (§5); every further line is a **record line** (§6), one per exported record.
  A pack that opens but has no exported record yields the header line alone.
- Each object's keys appear in the order this document gives for that object.
  An optional key is omitted when its condition does not hold; no key is ever written with the value `null`.

## 3. Values

| Source value | JSON form |
|---|---|
| `u8` that is not an enum, `u16`, `u32` | number: decimal digits, no sign, no leading zero except `0` itself, no fraction or exponent |
| `u64`, `i64` | string holding the decimal form: `-` before a negative `i64`, no `+`, no leading zero except `"0"` (64-bit values exceed what IEEE-double JSON parsers hold, G5-167) |
| `bool` | `true` or `false` |
| `uuid` | string, the 36-character canonical form in lowercase hex, digits in [FMT §2] byte order |
| `utf8` | string (below) |
| `bytes` | string, standard base64 (RFC 4648 §4) with padding; an empty value is `""` |
| enum | string: the value's name (§3.1); a value without a name is `unknown(<n>)`, `n` in decimal ([FMT §1]) |
| bit set | array of strings: the name of each set bit (§3.2), in ascending bit order; a set bit without a name is `bit(<n>)`, `n` in decimal; no bit set is `[]` |

**Strings** hold the stored code points in stored order, without Unicode normalization; a leading U+FEFF is kept ([FMT §2]).
Escaping is the minimal escaping of RFC 8785 §3.2.2.2:
`"` is written `\"`, `\` is written `\\`,
U+0008 `\b`, U+0009 `\t`, U+000A `\n`, U+000C `\f`, U+000D `\r`,
every other code point below U+0020 `\u00xx` with two lowercase hex digits;
every other code point is written as its UTF-8 bytes, `/`, `<`, `>`, `&`, U+007F, U+2028 and U+2029 included.

### 3.1 Names

Schema `tracepack-jsonl/1` names exactly the enum values, bits, tags and nested tags that format 1.0 defines as of spec v2.26:
- enum values: the names of [FMT §9]; `timer` values are named `none` and `T1` … `T8`;
  the retired `pack_role` 5 has no name, so it is written `unknown(5)`;
- tags and nested tags: the names of the registries in [FMT §5] and [FMT §8];
  the nested tags 1 … 8 of `hsms_timers` are named `T1` … `T8`.

A value, bit or tag that a later version of the format adds is unknown under `tracepack-jsonl/1`, whatever the exporter knows:
naming it changes the output, so it is named only under a new schema value (§9).

Which enum each enum-typed tag holds:

| Registry | Tag | Enum |
|---|---|---|
| pack metadata | `transport`, `capture_method`, `vantage`, `time_source`, `lifecycle_coverage`, `pack_role` | the enum of the same name |
| pack metadata | `equipment_connect_mode` | `socket_role` |
| `redaction_policy` | `digest_algorithm` | `digest_algorithm` |
| transport-event | `event`, `cause`, `timer`, `socket_role`, `boundary_kind` | the enum of the same name |
| transport-event | `prev_state`, `cur_state` | `state` |
| annotation | `annotation_kind` | `annotation_kind` |

Every other `u8` tag (`compaction_level`) is a number.

### 3.2 Bit names

| Bit set | Names by bit |
|---|---|
| header `flags` ([FMT §4]) | 0 `redaction-present` |
| `quality` ([FMT §9]) | 0 `capture-boundary`, 1 `ordering-uncertain`, 2 `correlation-incomplete`, 4 `direction-inferred`, 5 `redacted` |
| `field_validity` ([FMT §7.2]) | 0 `session_id`, 1 `stream_and_w`, 2 `function`, 3 `ptype`, 4 `stype`, 5 `system_bytes` |
| `record_flags` ([FMT §9]) | 1 `mono_present` |

Every other bit, retired or reserved, is written `bit(<n>)` when set.

## 4. TLV objects

A **TLV object** renders an entry list (pack metadata, a nested `tlv` value, a transport-event or annotation body)
that passed the acceptance rules of [FMT §5], under the registry that applies to the list:
- one key per known tag present, named by §3.1, in ascending tag-number order; a known tag that is absent has no key;
- a present value is always written, also when it is zero, `false`, an empty string, empty bytes or an empty nested list (`{}`);
- the value of a non-repeatable tag is its JSON form (§3); a repeatable tag's key holds an array of its values in stored order,
  also when it holds one value, equal values kept;
- a known `tlv` value is itself a TLV object under its nested registry ([FMT §5]: `coverage`, `hsms_timers`, `redaction_policy`, `redaction`);
- every entry whose tag the registry does not know (§3.1), retired and private tags included, is listed, in stored order,
  in a final key `unknown`: an array of objects `{"tag":<n>,"value_type":<n>,"value":"<base64>"}`,
  `tag` and `value_type` as numbers, `value` the entry's raw value bytes;
  `unknown` is omitted when there is none.
  An unknown entry's value is never interpreted, also when its `value_type` names `utf8`, `bool` or `tlv`.
- The reserved byte of each entry header is not exported ([FMT I-9]).

## 5. Header line

Keys, in order:

| Key | Value |
|---|---|
| `schema` | `"tracepack-jsonl/1"` |
| `format_major`, `format_minor` | numbers ([FMT §4]) |
| `flags` | bit set (§3.2) |
| `writer_start_utc_ns` | `i64` string |
| `pack_id`, `capture_id` | UUID strings |
| `metadata` | the pack metadata as a TLV object (§4) |

The file header's reserved bytes and CRCs, the footer and the trailer are not exported:
reserved bytes are ignored on read ([FMT I-9]), and the rest is derived from the blocks or checked by `verify`.

## 6. Record lines

Keys, in order:

| Key | Value |
|---|---|
| `seq`, `ts_utc_ns`, `mono_ns` | `u64` / `i64` strings |
| `epoch`, `payload_len`, `trailing_bytes` | numbers |
| `quality` | bit set |
| `kind`, `dir`, `fidelity`, `decode_status` | enum names |
| `field_validity`, `record_flags` | bit sets |
| `record_header_len` | number: the block envelope's `record_header_len` ([FMT §6]) |
| `header_extra` | optional, present iff `record_header_len` > 44: base64 of the extension area, the record header's bytes from offset 44 on ([FMT §7.1]) |
| `payload` | base64 of the payload |
| `body` | optional (below) |

The [FMT §7.1] fields are written as stored in the record header:
`mono_ns` whatever `mono_present` says, and `quality`, `field_validity` and `record_flags` with any reserved or retired bits set.
`record_header_len` is on every record line because two copies that differ only in it are different records ([SEM §7.4]).

`body` is present iff the record's `kind` is `transport-event` or `annotation` and its payload is a valid TLV body ([FMT §8]),
the same test by which `verify` reports an invalid body as a writer defect ([FMT §13]);
nothing else decides `body`: not unknown enum values, not a record header writer defect, not the record's other fields.
A valid body is written as a TLV object (§4).
When the payload is not a valid body, `body` is omitted; the payload still holds every byte.

## 7. Records exported

The export holds the records of every validated block ([FMT §13]) among the blocks a reader locates,
in the order the blocks lie in the file and the records in their block:
blocks are located through the footer when the pack is finalized (I-5) and its footer valid ([FMT §10]), else by the forward walk ([FMT §13]).
A block whose records disagree with its F-2 entry or its F-3 summary is validated, so its records are exported.
A failed block is left out, and nothing marks its place (G5-169);
the `verify` report of the same pack ([FMT §13], a separate golden in the corpus) says what was lost.

An exporter whose own resource limits keep it from reading the footer, from locating a block by the forward walk, or from reading a located block
fails the export with an error instead of leaving those blocks out (G5-171):
the output it wrote is then not an export, and the caller discards it.

## 8. Packs without an export

A pack whose file header or pack metadata a reader rejects has no export ([FMT §13]):
there is no header line to write, and none is invented.
The corpus records the expected rejection of such a vector instead of JSONL ([FMT §16], [CORPUS §5.3]).

## 9. Schema versions

The `schema` value names the byte form.
`tracepack-jsonl/1` is a draft until tracepack v1.0.0: a change to the bytes it defines before then bumps the value, and the change is recorded in the spec changelog.
Every later change to the output of a pack, naming a value the format adds (§3.1) included, takes a new schema value.

## 10. Example (informative)

An annotation record with seq 7, written by a writer that sets `mono_present`, with a note "a<b":

```
{"seq":"7","ts_utc_ns":"1791000000000000000","mono_ns":"5000","epoch":1,"payload_len":20,"trailing_bytes":0,"quality":[],"kind":"annotation","dir":"local","fidelity":"not-applicable","decode_status":"not-applicable","field_validity":[],"record_flags":["mono_present"],"record_header_len":44,"payload":"AQABAAEAAAABBQAGAAMAAABhPGI=","body":{"annotation_kind":"note","text":"a<b"}}
```

The payload holds two entries: `annotation_kind` (tag 1, `u8`, value 1 `note`) and `text` (tag 5, `utf8`, `a<b`).
