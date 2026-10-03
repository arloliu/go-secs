# tracepack — overview

Status: current (2026-10-04) — v2.22, tracepack format 1.0.
External review consensus reached (review rounds recorded in `tracepack-spec-changelog.md`).
Decisions: `tracepack-decisions.md` G3-20..G3-27, G4-28, G4-29, G3-PLACE, R3-4, R3-5, R3-6, G5-30..G5-146.
Change history: `tracepack-spec-changelog.md`.
This document is **informative**; the rules live in the three normative documents below.

References: `[FMT §n]` = `tracepack-format.md`, `[SEM §n]` = `tracepack-semantics.md`, `[STO §n]` = `tracepack-storage.md`, `[OVW §n]` = `tracepack-overview.md`; `[FMT I-n]` = invariant I-n of the format document.
Each rule is defined in exactly one document; the others only reference it.

## 1. Purpose and scope

A tracepack file (`.tpk`) is an immutable, compressed, self-indexed container for recorded SECS-II traffic between one tool and its host,
including HSMS control messages and transport lifecycle events.
It is intended as a long-term archival format for SECS/HSMS traffic,
written by recorders and log converters and read by any implementation, in any language, for many years.
It targets local file systems, S3-compatible object storage and HTTP range reads.

- Scope: one tool, one capture, one transport (HSMS-SS; SECS-I as a *normalised* representation, [FMT §8]) per file;
  record kinds data / control / transport-event / annotation.
- Non-goals: not a query engine (the footer prunes the queries of [SEM §7.4] by time, epoch, seq and summary counts; everything else is a scan);
  not a learned model or metrics store;
  its redaction is limited to masking extracts under a policy ([SEM §8]), whose rules are service configuration.

## 2. Document set

| Document | Content | Status | Implementation phases |
|---|---|---|---|
| `tracepack-overview.md` (this) | purpose, document map, terminology, diagrams, open questions, deferred items | informative | all |
| `tracepack-format.md` [FMT] | conventions, portable encoding, invariants, byte layouts, TLV and all registries, footer and validation, bootstrap and recovery, versioning, canonical JSONL, conformance corpus | normative | 1, 2, 3, 6, 7 |
| `tracepack-semantics.md` [SEM] | capture model, decode_status, time and clock steps, transport events, quality, indexes, transaction lookup, query mapping, redaction | normative | 2, 3, 5, 7 |
| `tracepack-storage.md` [STO] | scopes and generations, active view, commit protocol, object lifecycle and deletion, catalog contract with its window and listing views, retention, recorder durability and crash recovery, log converter | normative for publishers, recorders and converters | 4, 5, service |
| `tracepack-go.md` | Go module, API, mapping from go-secs, CLI | Go reference implementation | — |

The implementation plan, the deferred proposals and the decision log are listed in `README.md`.

The normative documents are defined only in terms of SEMI E37 (HSMS), SEMI E5 (SECS-II) and their own text.
No behaviour of any library defines a field.
Where an implementation and the text disagree, the text wins; the conformance corpus [FMT §16] is the executable arbiter.

## 3. Terminology

Four structures carry header-like data, each with one fixed name.

| Name | Count | Size | Compressed? | Describes |
|---|---|---|---|---|
| **file header** | 1 per file | 80 bytes | no | the file itself: magic, format version, identity, pack metadata length |
| **pack metadata** | 1 per file | variable (TLV) | no | the whole pack: tool, transport, capture method, time origin, lineage |
| **block envelope** | 1 per block | 40 bytes | no | one block: codec, record header length, lengths, record count, first seq, CRCs |
| **record header** | 1 per record | `record_header_len` bytes (44 known in 1.0, [FMT §7.1]) | in the header section of the decoded block body, stored column by column | one record: seq, time, epoch, lengths, kind, direction, classification, flags |

Related names:
- **HSMS message header**: the 10-byte header of SEMI E37 §8.2.
  It is part of a data/control record's **payload** and is stored exactly as captured;
  predicates read its fields (SessionID, S/F, System Bytes, …) from the payload, where `field_validity` says they are real ([FMT §7.2]).
- **footer**: the variable-size index near the end of the file ([FMT §10]).
  **trailer**: the fixed 64-byte pointer to the footer at the end of the file ([FMT §11]).
  Neither is a header.

Relationships:
- The file header and the pack metadata are written once, at the start, and describe the whole file.
- Blocks are batches of records, encoded independently so a reader decodes only the blocks it needs.
- Records exist only inside block bodies.
  A decoded block body holds all record headers of the block in its header section, then all payloads in its payload section;
  record i is the pair (header i, payload i) ([FMT §6]).
  The header section stores the headers column by column — byte j of every header, then byte j + 1 —
  which puts the same field of successive records side by side, so it compresses well.

## 4. Structure diagrams

Figure 1 — file layout.

```
 offset 0
 ┌────────────────────────────────────────────┐
 │ file header            80 B, 1 per file    │  magic, version, pack_id, capture_id,
 │                                            │  pack_metadata_len + crc, header_crc
 ├────────────────────────────────────────────┤ ◄─ offset 80
 │ pack metadata          TLV, 1 per file     │  tool_id, transport, capture_method,
 │                        (uncompressed)      │  vantage, time origin, supersedes, coverage, …
 ├────────────────────────────────────────────┤ ◄─ 80 + pack_metadata_len: first block
 │ block 0   ┌ block envelope (40 B)          │  magic, codec, record_header_len, body_len,
 │           └ block body (codec-encoded)     │  uncompressed_len, record_count, body_crc,
 │                                            │  first_seq, envelope_crc
 ├────────────────────────────────────────────┤
 │ block 1   ┌ block envelope                 │
 │           └ block body                     │
 ├────────────────────────────────────────────┤
 │ …         (N blocks, walkable forward      │
 │            without the footer, [FMT I-1])  │
 ├────────────────────────────────────────────┤ ◄─ footer_offset
 │ footer                 variable, 1 per     │  block index array, block summaries,
 │                        file (codec-encoded)│  pack statistics ([FMT §10])
 ├────────────────────────────────────────────┤ ◄─ size − 64
 │ trailer                64 B, 1 per file    │  footer_offset/len/codec/crc, counts,
 │                                            │  trailer_crc, magic ([FMT §11])
 └────────────────────────────────────────────┘ EOF
```

Figure 2 — one block, before and after decoding its body.

```
 on disk                              after decoding the body with `codec`
 ┌───────────────────────────┐        ┌──────────────────────────────────────────┐ 0
 │ block envelope (40 B)     │        │ header section, column by column         │ k = record_count,
 │  magic "TPKB", codec      │        │  byte 0 of headers 0 … k-1               │ L = record_header_len;
 │  record_header_len        │        │  byte 1 of headers 0 … k-1               │ header i is bytes i, k + i,
 │  body_len                 │        │  …  byte L-1 of headers 0 … k-1          │ 2k + i, … of the section
 │                           │ decode ├──────────────────────────────────────────┤ H = k × L
 │  uncompressed_len         │ ─────► │ payload section                          │
 │  record_count, body_crc   │        │  payload 0 │ payload 1 │ … │ payload k-1 │ payload i starts at
 │  first_seq, envelope_crc  │        │  (payload_len bytes each, from header i) │ H + Σ_{j<i} payload_len_j
 ├───────────────────────────┤        └──────────────────────────────────────────┘ uncompressed_len
 │ block body (body_len B)   │
 └───────────────────────────┘        record i = (header i, payload i); no padding;
                                      no record spans blocks ([FMT §6], [FMT I-2])
```

Figure 3 — one data/control record: its record header, and the HSMS header fields that predicates read from its payload.

```
 record header (44 B, [FMT §7.1])    payload (payload_len bytes, [FMT §8])
 ┌──────────────────────────────┐    ┌──────────────┬──────────────────────────────────┬──────────────┐
 │ seq, ts_utc_ns, mono_ns      │    │ length       │ HSMS message header (10 B, E37)  │ message text │
 │ epoch, payload_len,          │    │ bytes 0-3    │ bytes 4-5    SessionID           │ bytes 14 on  │
 │ trailing_bytes, quality,     │    │ (big-endian) │ byte 6       stream, W (bit 7)   │ (SECS-II     │
 │ kind, dir, fidelity,         │    │              │ byte 7       function            │  item, may   │
 │ decode_status,               │    │              │ bytes 8, 9   PType, SType        │  be empty)   │
 │ field_validity, record_flags │    │              │ bytes 10-13  System Bytes        │              │
 └──────────────────────────────┘    └──────────────┴──────────────────────────────────┴──────────────┘
                                     a field is available only when its field_validity bit is set ([FMT §7.2])
```

For transport-event and annotation records the payload is a TLV body ([FMT §8]) and `field_validity` is 0.

## 5. Open design questions

1. Whether SECS-I block-level evidence (retries, NAK) is ever needed (a `secs1-block` kind in a minor version).
2. Retention is by data hour ([STO §5]): a tool whose clock resets to a much older date writes records that are already past retention,
   so readers report their hours removed and the deleting component deletes them.
   Pre-existing and independent of the catalog window; whether recorders or the service should guard against it is open.
3. Once retention removes the pack holding a capture's boundary, a catalog rebuild cannot recover the boundary and the capture reads as `open`;
   a barrier whose gap reaches retained hours is then lost ([STO §5]).
   A catalog that is not rebuilt keeps it.
   Proposal P5 (`proposals/p5-capture-registry.md`) records the design space.

## 6. Deferred

| Item | Why deferred |
|---|---|
| Indexes of HSMS header fields or message content in the footer (F-3 structures, an F-4 secondary index) | no longer planned (spec v2.13): records are read on demand for one tool or a few tools of one type, and the Virtual Equipment reads whole windows, so no consumer needs to filter without decoding payloads. The F-3 range 0x0010–0x001F and the F-4 slot stay reserved ([FMT §10]), so an index can still be added as a minor change; the footer is derivable ([FMT I-3]), so re-merged packs could gain it |
| Source reference for data/control records | needs the pilot's converter inputs |
| Exhaustive item-validity checklist ([SEM §3]) | delivered as conformance vectors |
| Byte-exact JSONL schema and catalog schema | follow the implementation; the catalog window and the retention period are service parameters (35 days and 6 months in the pilot plan), not format properties |
| Locating blocks after an envelope the forward walk cannot account for, in a pack without a valid footer ([FMT §13]) | needs a resynchronisation design |
| Live-tail interface and query-service API | service design, outside the format |
| Compression, latency and throughput numbers | measured on the pilot sample |
| Admissions for scopes the catalog does not index (repairs and merges of hours outside the window) | the listing-view contract of [STO §5] holds only while no admission reaches such a scope; admitting one needs a consistency design with interrupted-admission recovery (proposal P4, `proposals/p4-cold-scope-admissions.md`) |
| Capture end evidence beyond the retained packs, and catalog rebuilds that read only the window | each design explored so far left an interval in which end evidence was missing or premature (proposal P5, `proposals/p5-capture-registry.md`; owner decisions G5-61..G5-65) |
| Closing a crashed producer's capture over a durable bus | the service-side boundary needs a publish fence, interrupted-closure recovery, metadata for an empty capture and a rule for post-closure arrivals (proposal P7, `proposals/p7-durable-bus-capture-closure.md`); until then such a capture stays `open` |
| Ingest partitioned by tool (a bus partition per leased tool set, so one instance receives, writes and merges a tool's records and segments stop interleaving) | second-phase optimization (G5-87): the first release ingests through one work-queue consumer and lets the hourly merge normalize interleaved segments; a partition, when introduced, is the unit the catalog leases, so ownership stays in one place |
| Redaction policy file format and key management | service design; the format records only the policy id, version and key id ([SEM §8]) |
| Capture-time redaction with an escrowed encrypted original | a cost analysis, not a rule: recorders would hold only a public key (asymmetric encryption); a sidecar keyed by (`capture_id`, `seq`, item path) keeps merges untouched; a decrypt service needs approval and audit; private-key custody, backup and rotation dominate the cost; its unique benefit is a separate retention for sensitive items (crypto-shredding). The redaction entry of [FMT §5] is already shaped to become an annotation body |
