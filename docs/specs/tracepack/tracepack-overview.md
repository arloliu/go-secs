# tracepack — overview

Status: current (2026-09-27) — v2.9, tracepack format 1.0.
External review consensus reached (review rounds recorded in `tracepack-spec-changelog.md`).
Decisions: `tracepack-decisions.md` G3-20..G3-27, G4-28, G4-29, G3-PLACE, R3-4, R3-5, R3-6, G5-30..G5-86.
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
- Non-goals: not a query engine (the indexes serve the queries of [SEM §7.4]; everything else is a scan);
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
| **pack metadata** | 1 per file | variable (TLV) | no | the whole pack: tool, transport, capture method, time origin, corrections |
| **block envelope** | 1 per block | 40 bytes | no | one block: codec, record header length, lengths, record count, first seq, CRCs |
| **record header** | 1 per record | `record_header_len` bytes (56 known in 1.0, [FMT §7.1]) | in the header section of the decoded block body | one record: seq, time, direction, S/F, System Bytes, flags |

Related names:
- **HSMS message header**: the 10-byte header of SEMI E37 §8.2.
  It is part of a data/control record's **payload** and is stored exactly as captured;
  several record-header fields are positional copies of it ([FMT §7.2], [FMT I-10]).
- **footer**: the variable-size index near the end of the file ([FMT §10]).
  **trailer**: the fixed 64-byte pointer to the footer at the end of the file ([FMT §11]).
  Neither is a header.

Relationships:
- The file header and the pack metadata are written once, at the start, and describe the whole file.
- Blocks are batches of records, encoded independently so a reader decodes only the blocks it needs.
- Records exist only inside block bodies.
  A decoded block body holds all record headers of the block in its header section, then all payloads in its payload section;
  record i is the pair (header i, payload i) ([FMT §6]).

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
 │                        file (codec-encoded)│  secondary index, pack statistics ([FMT §10])
 ├────────────────────────────────────────────┤ ◄─ size − 64
 │ trailer                64 B, 1 per file    │  footer_offset/len/codec/crc, counts,
 │                                            │  trailer_crc, magic ([FMT §11])
 └────────────────────────────────────────────┘ EOF
```

Figure 2 — one block, before and after decoding its body.

```
 on disk                              after decoding the body with `codec`
 ┌───────────────────────────┐        ┌──────────────────────────────────────────┐ 0
 │ block envelope (40 B)     │        │ header section                           │
 │  magic "TPKB", codec      │        │  header 0 │ header 1 │ … │ header k-1    │ k = record_count,
 │  record_header_len        │        │  (record_header_len bytes each)          │ each record_header_len B
 │  body_len                 │ decode ├──────────────────────────────────────────┤ H = k × record_header_len
 │  uncompressed_len         │ ─────► │ payload section                          │
 │  record_count, body_crc   │        │  payload 0 │ payload 1 │ … │ payload k-1 │ payload i starts at
 │  first_seq, envelope_crc  │        │  (payload_len bytes each, from header i) │ H + Σ_{j<i} payload_len_j
 ├───────────────────────────┤        └──────────────────────────────────────────┘ uncompressed_len
 │ block body (body_len B)   │
 └───────────────────────────┘        record i = (header i, payload i); no padding;
                                      no record spans blocks ([FMT §6], [FMT I-2])
```

Figure 3 — one data/control record, and the record-header fields copied positionally from the HSMS message header.

```
 record header (56 B, [FMT §7.1])                          payload (payload_len bytes, [FMT §8])
 ┌──────────────────────────────┐   ┌──────────────┬──────────────────────────────────┬──────────────┐
 │ seq, ts_utc_ns, mono_ns      │   │ length (4 B, │ HSMS message header (10 B, E37)  │ message text │
 │ epoch, payload_len, …        │   │ big-endian)  │                                  │ (SECS-II     │
 │ session_id ◄─────────────────┼───┼──────────────┼─ bytes 0-1                       │  item, may   │
 │ stream (bits 0-6) ◄──────────┼───┼──────────────┼─ byte 2 ─┐                       │  be empty)   │
 │ record_flags.W   ◄───────────┼───┼──────────────┼──────────┘ bit 7                 │              │
 │ function ◄───────────────────┼───┼──────────────┼─ byte 3                          │              │
 │ ptype ◄──────────────────────┼───┼──────────────┼─ byte 4                          │              │
 │ stype ◄──────────────────────┼───┼──────────────┼─ byte 5                          │              │
 │ system_bytes ◄───────────────┼───┼──────────────┼─ bytes 6-9                       │              │
 │ kind, dir, fidelity, …       │   └──────────────┴──────────────────────────────────┴──────────────┘
 └──────────────────────────────┘    the payload is AUTHORITATIVE; the ◄── fields are index copies ([FMT I-10])
```

For transport-event and annotation records the payload is a TLV body ([FMT §8]),
the copy fields are zero and `field_validity` is 0.

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
| F-3 index structures (S/F presence, System Bytes locator including the Bloom hash algorithm, seeds and sizing, session-id set) and the F-4 secondary index | Their thresholds and encodings depend on real traffic (S/F cardinality, System Bytes per block, id distributions), which the pilot sample will provide; fixing them now is guessing. They are added later as new F-3 tags or F-4 content without changing any layout, and a pack without them is still correct (absent means "may match"). The footer is derivable ([FMT I-3]), so existing packs of indexed scopes can gain them by being re-merged |
| Source reference for data/control records | needs the pilot's converter inputs |
| Exhaustive item-validity checklist ([SEM §3]) | delivered as conformance vectors |
| Byte-exact JSONL schema and catalog schema | follow the implementation; the catalog window and the retention period are service parameters (35 days and 6 months in the pilot plan), not format properties |
| Recovery beyond a corrupt middle block | needs a resynchronisation design |
| Live-tail interface and query-service API | service design, outside the format |
| Compression, latency and throughput numbers | measured on the pilot sample |
| Admissions for scopes the catalog does not index (corrections, repairs and merges of hours outside the window) | the listing-view contract of [STO §5] holds only while no admission reaches such a scope; admitting one needs a consistency design with interrupted-admission recovery (proposal P4, `proposals/p4-cold-scope-admissions.md`) |
| Capture end evidence beyond the retained packs, and catalog rebuilds that read only the window | each design explored so far left an interval in which end evidence was missing or premature (proposal P5, `proposals/p5-capture-registry.md`; owner decisions G5-61..G5-65) |
| Redaction policy file format and key management | service design; the format records only the policy id, version and key id ([SEM §8]) |
| Capture-time redaction with an escrowed encrypted original | a cost analysis, not a rule: recorders would hold only a public key (asymmetric encryption); a sidecar keyed by (`capture_id`, `seq`, item path) keeps merges untouched; a decrypt service needs approval and audit; private-key custody, backup and rotation dominate the cost; its unique benefit is a separate retention for sensitive items (crypto-shredding). The redaction entry of [FMT §5] is already shaped to become an annotation body |
