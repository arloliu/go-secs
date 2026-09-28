# tracepack spec — change history

Status: current (2026-09-27) — spec v2.10.
Section numbers in each entry refer to the numbering of the version it describes.
The finding→fix tables below are the record of every review round;
the review reports, the texts of the applied proposals P1, P3 and P6, and the single-file v2.5 are kept outside the repository.
The deferred proposals and the decision log are listed in `README.md`.

## tracepack-go.md notes

- 2026-09-27: the eqp-hub producer row of §2 names the path that exists (`eqp_hsms` → `hsms_secsjson` → `tap_nats` → JetStream) instead of a `secs-recorder` device that was only planned,
  and records what that path lacks today (generation id, start/stop event, monotonic time); open question 2 added.
- 2026-09-28: brought up to the go-secs v2.6.0 observers, and to spec v2.10.
  §2 records through `WithWireObserver` and `WithSocketObserver` instead of a conn wrapper and maps `epoch` from `Socket`;
  the eqp-hub paragraph says what v2.6.0 exposes and what eqp-hub, still on v2.3.0 without a recorder device, lacks.
  §5.1 drops the branch note on `CauseHandlerExit` and gains the `SocketRefused` row;
  §5.2 takes lifecycle epochs and times from `LifecycleEvent.Socket` and `At`, keeping the drop-oldest caveat;
  §5.3 takes socket events from `WithSocketObserver`, with the `socket-close` cause mapping and the note that it can differ from the lifecycle cause;
  §5.4 notes the session, socket and generation `TxEvent` carries;
  the new §5.5 maps wire events to records, the outbound time being the [SEM §4] hand-to-socket time.
  Open questions 1 and 2 are answered;
  question 3, on marking a record that reached the recorder after its ordering window, is added.

## Changes vs v1 (summary)

All 15 P1 + 2 P2 findings of the v1 review were addressed in v2:
wire grammar and checksum ranges deferred with structural contracts kept (§2.2, §11);
commit sequence and validated-prefix recovery (§2.3–2.8);
true min/max time pruning, late-record packs, non-rejecting `Append` (§2.2, §3.5, §5);
transaction identity with capture/epoch/direction/F0 (§4.2);
never-exclude summaries and honest Bloom sizing (§4.1–4.2);
secondary-index completeness markers (§4.3);
full-byte PType/SType, `field_validity`, decode-status table (§3.1, §3.4);
provenance separated from grade, structured quality bits (§3.3, §3.1);
mono-time origin (§3.5);
transport-event table with frozen enums (§3.6);
converter entry contract (§7);
exclusive keys, single publisher, additive corrections (§5);
header + trailer bootstrap (§2.4);
`schema_version` frozen at 1 (§9);
module path `github.com/arloliu/go-secs/v2/tracepack` (§1.4).
The v2 review's closure table records which of these it accepted as closed or legitimately deferred.

## Changes v2 → v2.1 / v2.2 (review finding → where)

| v2 review finding | v2.1 |
|---|---|
| P0 Complete day manifest can omit later evidence | §5 publication snapshots: `sealed` + `amendment` marker, ordered unseal → upload → manifest → re-seal, crash rules |
| P0 Adjacent-pack lookup can omit a reply | §4.2 `unmatched` only after all packs of the (capture, epoch) via the manifest capture index; else `incomplete` with scope; §4.4 (d), §6 |
| P0 Repair/supersession erase incompleteness | §2.4 and §5 machine-readable `coverage`, inherited along the chain, surfaced as `incomplete`; supersession = complete replacement; overlapping replacements prohibited, `conflicted` otherwise |
| P1 Primary seq not unique across rolls | §3.1 `(pack_id, seq)` as the record reference; §3.2 annotations; §4.2 key |
| P1 Reply matching omits stream; outcomes conflated | §4.2 candidate / protocol-valid (same stream incl. F0) / observed outcome, with go-secs citations |
| P1 Lifecycle-derived epochs | I-7 epoch bound by the conn wrapper; `epoch = 0` + `correlation-incomplete` otherwise; §3.6 lifecycle limits cited; `lifecycle_coverage` header tag |
| P1 Quality-bit eligibility undefined | §3.7 effective flags, `quality_evaluated`, correction re-flagging, consumer rule |
| P1 Lifecycle causes ≠ socket events | §3.6 socket events require a socket source or are `inferred`; precedence and no double emission |
| P1 Finalization contradiction | I-5 trailer is the single commit; §2.3 no post-trailer header rewrite |
| P2 Decode-status applicability | §3.4 branches by kind/provenance before the table |
| Impl notes: complete-frame time fixed; source refs per kind; exactly one parsed message; §6 reference wording | §3.5, §3.2, §7.2, header line |

v2.2: the confirmatory review's residual P1 (lifecycle-event epoch attribution) fixed in §3.6.

## Changes v2.2 → v2.3 (owner decisions G5-30..G5-34, 2026-09-26)

| Topic | Change | Where |
|---|---|---|
| Language neutrality (G5-30) | Part A defined only by SEMI E37 / E5 and this text; go-secs citations are informative; Go placement, API and source mapping moved to Annex G; document split into Parts A / B / G | §1.1, §3.3, §3.4, §3.6, §4.2, Annex G |
| Observed vs derived | record-header Class column; payload authoritative (I-10); derived fields are classifier opinions with a `classifier` tag (I-11) | §2.3, §2.4, §3.1 |
| decode_status definitions | predicates rewritten on bytes per E37 §8 / E5 §9 instead of go-secs calls; `max_frame_len` tag for `oversized`; empty message text is `ok` | §3.4 |
| Portable encoding rules (G5-31) | integers ≤ 2^63 − 1; UUID in RFC 9562 byte order; POSIX ns time; UTF-8 strings; CRC-32/ISO-HDLC pinned; index hashes must be spec'd algorithms; enum registry | §2.7 |
| Codec (G5-32) | codec field in block envelope and trailer; `0=none`, `1=zstd` (RFC 8878, no dictionary); readers MUST support both; new codec = major bump | §2.4, §2.6, §2.7, §8 |
| Footer / trailer fields (G5-33) | field tables; footer split into F-1..F-5 with a fixed-size block index array (F-2) and an epoch table; trailer gains codec, uncompressed length, record count and its own CRC | §2.5, §2.6, I-4, I-5 |
| Bootstrap (G5-34) | trailer stays at the end (front index considered and rejected, rationale recorded); one-round bootstrap via manifest footer location or speculative tail read; whole-object read for small packs | §2.9, §5 |
| Terminology | "pack header" → **pack metadata**; §3.1 is the **record header**; "zstd frame" → **block body**; terminology table and structure diagrams | §2.1, §2.2 |
| Longevity | canonical JSONL export; conformance corpus section | §7, §9 |
| Numbering | §7 converter → §6, §8 CLI → §7, §9 versioning → §8, new §9 conformance, §6 Go API → Annex G.2 | — |

## Changes v2.3 → v2.4 (owner decisions G5-35..G5-37, 2026-09-26)

| Topic | Change | Where (v2.4) |
|---|---|---|
| Document split (G5-37) | wire spec is language-agnostic and contains no go-secs citations or VE-program references; Go placement, API, go-secs mappings, CLI and the eqp-hub question moved to `tracepack-go.md`; change history moved to this file | spec §1.1, `tracepack-go.md` |
| Capture model (G5-35) | per-record `evidence` + `provenance` replaced by per-record `fidelity` (wire-exact / re-encoded / reconstructed / synthesized) and pack-level `capture_method` (raw-stream / decoded-message / log / generator), `vantage` and `recorder`; evidence grades become a derived view | spec §3.5 |
| Fixed layouts (G5-36) | byte layouts of file header (80 B), block envelope (40 B, adds `first_seq` and `envelope_crc`), record header (56 B) and trailer (64 B); TLV entry encoding, value types and tag registry; all enum and bit registries; CRC byte ranges | spec §2.5–§2.7, §2.9, §3.1, §3.3 |
| Record header | positional copy rule identical for data and control; `record_header_len` allows appended fields in minor versions; `quality.no-mono` tied to `mono_present` | spec §3.1, §3.2 |
| Event and annotation payloads | TLV bodies with defined tags; generic `cause` enum plus `cause_raw`; `base64:` prefix convention replaced by a `bytes` tag | spec §3.4 |
| Generic names | `time_source` = capture-clock / source-log / generator; `sml_dialect` → `source_dialect`; tcp-* events → socket-* | spec §2.6, §3.3 |
| Footer | `footer_layout_version` in F-1; deferral reasons stated | spec §2.8, §11 |
| Limits | `body_len`, `uncompressed_len`, `payload_len` ≤ 2^31 − 1 | spec §2.4 |
| Versioning | readers accept any minor within their major (was: minor ≤ own), so unknown tags, enum values and record-header bytes are forward-compatible | spec §8 |
| field_validity | stream and W share one bit (same wire byte) | spec §3.3 |

## Changes v2.4 → v2.5 (owner decisions G5-38..G5-44, 2026-09-26)

Driven by the traffic-log-service scenario (MinIO storage, periodic segment flush, hourly merge, query API).

| Topic | Change | Where (v2.5) |
|---|---|---|
| Record identity (G5-38) | `seq` is capture-scoped; the global record reference is (`capture_id`, `seq`); duplicates must be byte-identical and are deduplicated | I-12, §3.2, §3.4 annotation tags, §4.2, `coverage` nested tags |
| Hour-aligned blocks (G5-39) | a block never spans a UTC hour, so merges move blocks without decoding | I-13 |
| Footer layout fixed (G5-40) | F-1 prologue 72 B, F-2 entry 80 B (adds `last_seq`, `body_crc`), F-3 and F-5 as TLV lists, merge rule; only index structures (F-3 reserved tags, F-4) stay deferred; no provisional layout | I-14, §2.8, §4.1, §11 |
| Stored vs effective quality (G5-41) | stored bits are writer-local; cross-record conclusions are computed at query time from F-5 `boundary` / `epoch` and the catalog | §3.9 |
| Pack roles and lineage (G5-42) | `pack_role`, `compaction_level`, `compacted_from`, `recorder_instance_id`, `previous_capture_id`, `extract_filter`; `stop-unclean` boundary | §2.6, §3.3 |
| Storage profile (G5-43) | two tiers (staging segments, hourly archives), unique keys with `pack_id`, block-copy merge, readiness by seq contiguity, deletion delay, catalog contract replacing manifest.json sealed/amendment | §5 |
| Troubleshooting metadata (G5-44) | optional tags: site, equipment model / software, host software, endpoints, connect mode, device id, HSMS timers; UUIDv7 recommended | §2.4, §2.6 |

### v2.5 review round 1 fixes

| Finding | Fix |
|---|---|
| P0 corrections vs byte-identical duplicates | I-12 identity resolved after supersession; byte-identity required only within the active view |
| P0 CRC used as block-equality proof | §5.3: drop an overlapping block only on byte-for-byte equal codec + body; every other overlap resolved per record |
| P0 stop-unclean looks contiguous | §5.3 recovery sets gap_start/gap_end; §5.4 completeness barrier; lookups in open epochs never `unmatched` |
| P1 F-5 not derivable from per-block data | §2.8 F-3 required with `content_bytes`, `epoch`, `boundary`, `seq_range`; F-5 = aggregate of F-2 + F-3 |
| P1 CRC-valid malformed footer | §2.8 footer validation checklist, envelope vs F-2 agreement, fallback to forward walk |
| P1 unavailable copy fields could match | §3.2 zero on write, "cannot match" in filters |
| P1 singular active archive vs size-cap parts | §5.3 active archive set as merge input, atomic replacement |
| P1 `CauseHandlerExit` not on main | Go doc notes it exists only on `fix/robustness-batch2` |
| P2 CLI scope / import boundary | Go doc defers `grep`/`tx`; plan allows `cmd/tracepack` to import go-secs |

### v2.5 review round 2 fixes

| Finding | Fix |
|---|---|
| R2-1 P0 cross-hour segment retired after one hour's merge | §5.3 hour slices; a segment stays `registered` until every slice is consumed; §5.4 readers use active packs + unconsumed slices |
| R2-2 P0 stop-unclean interval not conservative | §3.7 clock-step rule (durable `clock-step` event before stepped records, `clock_step_tolerance_ns`); §5.3 gap bounds widened by the tolerance, unbounded after a step or with nothing recovered; `seq_start` tag for empty recovery |
| R2-3 P1 barriers not reconstructible from footers | F-3/F-5 `boundary` carry `gap_start`/`gap_end`; `epoch` carries `close_seq` |
| R2-4 P1 merge omits corrections/repairs | §5.3 merge inputs = active view of H regardless of role; inherited coverage |
| R2-5 P1 shard sets vs supersession conflict | §5.5 replacement sets (`replacement_set_id/size/index`), completeness of a set, conflict only between different complete sets |
| R2-6 P1 footer accepts fabricated coverage | §2.8 validation: seq-set vs record_count, count sums, F-5 recomputed; empty-pack case |
| R2-7 P1 MergeIterate ordering | Go doc: capture order streaming; time order via F-2 ts_min watermark with bounded memory |
| R2-8 P1 reply before primary can match | §4.2 eligibility window; closure defined; `unmatched` needs a closed window |
| R2-9 P1 F-5 count overflow | F-5 count arrays are u64 arrays |

### v2.5 review round 3 fixes

| Finding | Fix |
|---|---|
| R3-1 P1 cross-hour corrections/repairs lose other hours | §5.3 archive/correction/repair packs are hour-scoped; multi-hour corrections become per-hour replacement sets; coverage copied per hour |
| R3-2 P1 clock bounds not provable | §3.7 clock anchor fixed at capture origin or last durable clock-step (no accumulation); §5.3 durability contract with liveness anchor; gap bounds computed from monotonic times, never from recovery wall time; unprovable side omitted |
| R3-3 P1 deleted shard hides siblings on rebuild | §5.5 replacements always supersede whole sets; set members agree on size/supersedes/capture/hour |
| R3-4 P1 later epoch taken as closure | §4.2 closure needs evidence for the queried epoch (`close_seq` or clean stop) |
| P2 watermark ties | Go doc: strict `<` watermark, drain at end |
| P2 footer checks | §2.8 per-block bound checks, overflow checks, empty-pack F-5 |
| P2 deletion-delay anchor | §5.3 delay counted from the publication consuming the last slice |
| P2 plan alignment | u64 array helper, MergeIterate memory, replacement-set validation, recovery contract helpers |

### v2.5 review round 4 fixes

| Finding | Fix |
|---|---|
| R4-1 P1 cross-hour replacement sets contradict hour-scoped rules | root cause removed: segments roll at every UTC hour change, so every pack except an extract has one scope (capture, hour); hour slices and cross-hour sets deleted |
| R4-2 P1 recovery uses stale liveness state after a roll | liveness anchor rewritten at spool rotation and after a durable clock-step; recovery reconciles spool and anchor taking the newest of each fact |
| R4-3 P1 deleted intermediate generation breaks rebuild | supersession chains replaced by `scope_generation`: the active view is the highest complete generation plus generation-0 packs outside its cumulative `compacted_from`; deletion order no longer matters |
| P2 monotonic coordinates | clock anchor capture-relative, initial (`capture_origin_utc_ns`, 0) |
| P2 coverage-only hours | a pack's scope comes from its period; queries consult `coverage` intervals directly |

### v2.5 review round 5 fixes

| Finding | Fix |
|---|---|
| R5-1 P1 single-pack repair drops healthy siblings | repairs and corrections are generation-0 patches that replace exactly the packs they name (segments or members of the current generation); only merges create generations; patches accepted only when their targets are in the view |
| R5-2 P1 period-based selection misses records after a same-hour clock step | record-bearing packs selected by actual `ts_min`/`ts_max`; period only defines scope; coverage and barriers by their own intervals |
| P2 claim vs active generation | claimed vs highest complete generation distinguished; claims after rebuild; late completion of an interrupted upload is a valid, visible outcome |
| P2 generation-wide compacted_from | replicated identically across set members |
| P2 stale corpus wording | replaced; new vectors for R5-1, R5-2 and interrupted claims |

### v2.5 review round 6 fixes

| Finding | Fix |
|---|---|
| R6-1 P1 deleting a patched member hides siblings | §5.3 deletion separated from view exclusion: members stay until a higher complete generation exists; generation-0 packs until listed in G's `compacted_from` |
| R6-2 P1 pre-rebuild upload outranks newer generation | §5.1 publisher epochs fenced by exclusive fence objects; generations ranked by (`publisher_epoch`, `scope_generation`) |
| P2 serialization across merge construction | §5.4 claim records the view; publication revalidates or retries; patches carry `patch_base` and stale patches are outside the view |
| P2 patch coverage inheritance | §5.5 a patch carries its targets' existing coverage |
| P2 companion alignment | Go doc and plan: allocated claim number, patch-aware view, identical `compacted_from` |

### v2.5 review round 7 fixes

| Finding | Fix |
|---|---|
| R7-1 P1 rejected merge upload activates on rebuild | §5.2 commit objects; §5.4 commit protocol (upload → validate → commit object → visible/deletable); §5.3 view admits only committed generations and patches |
| P2 rejected patch with matching base | covered by the commit protocol: rejected patches never get a commit object |
| P2 set members agree on publisher_epoch | §5.5 agreement list; plan validation |
| P2 fence-collision claim | §5.2 narrowed: single publisher per tool, epochs acquired after the previous publisher stopped |

### v2.5 review round 8 fixes

| Finding | Fix |
|---|---|
| R8-1 P1 abandoning after the commit object exists | §5.4 commit object is the commit point; serialization spans validation to installation; recovery rolls forward when the object exists and, when the write outcome is unknown, cancels by committing a new generation under the new epoch; rebuild equals the last committed view |
| P2 reconstruction inputs | §5.4 catalog derivable from packs plus commit and fence objects; `ActiveView` takes commit objects |
| P2 step-4 deletion wording | §5.4 installation makes deletable only packs satisfying §5.3 Deletion |

### v2.5 review round 9 — VERDICT: ready

Consensus reached. Non-blocking refinements applied: recovery treats lost attempt state as an unknown outcome, fixes the cancellation input to the last committed view,
re-applies itself to an interrupted cancellation; crash vectors split by generation / patch / no earlier generation / crash during cancellation;
§5.3 wording "surviving packs and commit objects".

## v2.5 split into four documents (2026-09-26)

Content unchanged; section numbers changed. Old → new:
§1 → [OVW §1]; §1.1 → [OVW §2]; §1.2 → [FMT §1]; §2.1–§2.2 → [OVW §3–§4]; §2.3 → [FMT §3]; §2.4 → [FMT §2];
§2.5–§2.7 → [FMT §4–§6]; §2.8 → [FMT §10]; §2.9–§2.11 → [FMT §11–§13]; §3.1–§3.2 → [FMT §7.1–§7.2]; §3.3 → [FMT §9]; §3.4 → [FMT §8];
§3.5–§3.9 → [SEM §2–§6]; §4.x → [SEM §7.x]; §5.1–§5.5 → [STO §2–§6]; §6 → [STO §7]; §7 → [FMT §15]; §8 → [FMT §14];
§9 → [FMT §16] (vectors distributed to [FMT §16], [SEM §8], [STO §8]); §10–§11 → [OVW §5–§6]; Appendix A → [FMT Appendix A].
Invariants keep their numbers (I-1..I-14) in [FMT §3]. The single-file version is kept outside the repository.
Split review S1: no P1; three P2 Depends-on list gaps fixed ([FMT] now lists its semantic/storage dependencies; [SEM] and [STO] lists completed).
Split review S2: VERDICT ready; two remaining P2 Depends-on refinements applied ([FMT] adds [STO §5], [STO §7], [SEM §7.1–§7.3], corpus parts; [STO] lists I-7, [FMT §8], [FMT §16] as shared dependencies and qualifies the local-file-system sentence).

## Changes v2.5 → v2.6: block body split (proposal P1, owner decisions G5-49..G5-55, 2026-09-26)

Source: proposal P1 revision 7.1 (kept outside the repository); seven review rounds (round 7 ready).
Format version stays 1.0, because nothing is released.

Summary:
- [FMT §6] the decoded block body is a header section (all record headers, `record_header_len` bytes each) followed by a payload section;
  the codec still covers the whole body with one frame.
- [FMT §6] envelope offset 6 and [FMT §10] F-2 offset 76 carry `u16 record_header_len` (≥ 56); pack metadata tag 0x0014 is retired and not reused.
  Blocks are self-describing, which fixes a latent misparse of blocks copied between packs of different header lengths.
- [FMT I-2] states the length equation with the stride and requires codec completion;
  [FMT §6] adds the header-only read level and attested blocks.
- [FMT §5] tag 0x0030 `blocks_validated`; [FMT §12] the commitment precedes the blocks, each block is validated before it is written, and a failed check withholds the trailer.
- [SEM §7.4] provisional and authoritative query modes and the `header-validated` status; [SEM §7.1] and [SEM §7.2] follow them.
- [STO §4] merge: verbatim copy, duplicates only on equal envelope and body, equal-stride coalescing, record-level resolution by stride,
  in-memory validation of new encodings, and optional attestation that never changes what a merge copies, publishes or makes deletable;
  recorders SHOULD attest their segments. [STO §6] `blocks_validated` is per pack.
- [FMT I-10], [FMT I-13], [FMT I-14], [FMT §2], [FMT §7], [FMT §13], [FMT §14], [FMT §16], [STO §8], [OVW §3], [OVW §4] Figures 1–2,
  `tracepack-go.md` and `tracepack-impl-plan.md` updated to match.
- Moved out: storage handling of defective merge inputs (proposal P2, deferred by the owner to after the pilot); no per-record extension area (annotations instead).
- Benchmark (tpkbench, kept outside the repository; illustrative): a header-prefix decode takes ≈ 0.55 ms per 4 MiB block against ≈ 3.0–3.3 ms for a whole-body decode.

### P1 review round 1 fixes

| Finding | Fix |
|---|---|
| P1 duplicate-block rule unsafe with block-local stride | [STO §4] duplicates only on equal envelope + body; [FMT §16] different-framing vector |
| P1 header-only results declared valid without a validation level | [FMT I-2], [FMT §6] full / header-only / attested; [SEM §7.4] `header-validated`; [FMT I-10] filter semantics; full reads for payloads and multi-pack seqs; merger validation and `blocks_validated` |
| P2 mixed-stride coalescing | [STO §4] equal-stride coalescing; new output block on stride change; no normalisation |
| P2 benchmark defects | proposal only (benchmark fixed, claims narrowed) |
| P2/P3 overstated benefits | proposal only |
| Stride placement | [FMT §6], [FMT §10] block-local stride with bounds, overflow checks and preserved unknown bytes; tag 0x0014 retired |
| Extension area | not reserved (G5-55) |

### P1 review round 2 fixes

| Finding | Fix |
|---|---|
| P1 header-copy filtering not payload-authoritative | [SEM §7.4] provisional and authoritative modes; non-attested blocks never excluded by copies or derived indexes; empty results still carry the status; [FMT §16] S6F11 / S1F3 vectors |
| P2 attestation success and finalization semantics | [FMT §5], [FMT §12] commitment before blocks, validation before each write, trailer withheld, no byte repair; [FMT §6] attested only when finalized; [STO §4] new merge encodings validated |
| Attestation justified? | owner kept it (G5-52) |
| P2 benchmark "every path" claim | proposal only |
| P2 affected text for merge validation | [FMT I-13], [FMT I-14], [FMT §10], [SEM §7.2], Go API, impl plan |
| Multi-pack seq rule | [SEM §7.4] scoped by (`capture_id`, `seq`), applied before filters |

### P1 review round 3 fixes

| Finding | Fix |
|---|---|
| P1 mandatory validation can block the [STO §5] cancellation merge | [STO §4] a readable I-10 defect publishes non-attested output; [STO §8] vector |
| P2 index-only exclusions escape `header-validated` | [SEM §7.4] status triggered by reliance, read or not; [FMT §16] paired vectors with and without the index |
| P2 affected-text omissions | [SEM §7.1], [STO §8], [FMT §16] conformance criterion, impl-plan phases 2, 3 and 5 |

### P1 review rounds 4–6

Rounds 4–6 found that the storage profile does not define how a merge handles input blocks that fail I-2, a gap that pre-dates P1.
Owner decision after round 6 (G5-54): the storage rules they introduced (failure classes, member boundaries at undecodable blocks,
finalization-based membership, repair-first recovery) were removed from P1 and recorded in `proposals/p2-merge-input-defects.md`;
member restart and the per-attempt destination factory were removed as unnecessary, because attestation is decided before a member's metadata is written.
What remained:

| Finding | Fix |
|---|---|
| R4 P1 failed replacement encodings could make healthy inputs deletable | [STO §4] new encodings validated in memory; a failed coalesced encoding falls back to verbatim copies, a failed resolution publishes nothing; [STO §8] injection vectors |
| R4 P2 phase-6 corpus | impl plan phase 6: expected query results and statuses generated and checked |
| R5/R6 P2 phase-5 completion criterion | impl plan phase 5 "Done when" replaced |
| R4 P2 restarting a started member | no longer needed: attestation is decided before a member's metadata (`tracepack-go.md` `Merge` stages the member) |

### P1 review round 7 — VERDICT: ready

Refinements applied before the normative edit:
conformance vectors with the disagreement only in a non-attested pack, an attested control, a rejected attestation attempt and an optional labelled nonconforming fixture ([FMT §16]);
a zero-encode assertion with coalescing disabled and `Verify` exactness only for validating merges (impl plan phase 5);
the record-level-resolution failure vector ([STO §8]);
the whole [FMT §7.1] sentence replaced, including the tag reference;
the query mode in the CLI and the staged `Merge` member (`tracepack-go.md`).

### P1 application review A1 — VERDICT: ready

| Finding | Fix |
|---|---|
| P2 impl-plan phase 3 disagreement fixtures "attested and not" | replaced by the non-attested disagreement, the attested control and the labelled nonconforming fixture of [FMT §16] |
| P2 Go zero-copy guidance stronger than its source | `tracepack-go.md` states the full conditions: stride equals struct size, matching field offsets, alignment, little-endian host, buffer lifetime |
| P3 history narration in FMT | [FMT §13] trailer rationale reworded in the present tense; the informative codec comparison in [FMT Appendix A] is kept as rationale |
| P3 changelog dispositions and P2 deferral provenance | round 4–6 entry separates transferred rules from removed ones; G5-54 names when the owner deferred P2 |

## Changes v2.6 → v2.7: object lifecycle and cold reads (proposal P3, owner decisions G5-56..G5-65, 2026-09-26)

Source: proposal P3 revision 7 (kept outside the repository); six review rounds (round 6 ready).
Format version stays 1.0; [FMT] content is unchanged.

Summary:
- [STO §3] packs, commit objects and fence objects are removed only by Deletion and Retention;
  storage lifecycle rules may only transition `archive/` objects to another storage class, never expire objects, and never touch `staging/`, `commit/` or `fence/`;
  the highest-numbered fence object is never deleted.
- [STO §2] a new publisher epoch also exceeds every `publisher_epoch` on a pack the catalog indexes (O1).
- [STO §4] Deletion: a commit object is deleted only by the component that deleted every pack it commits, right after the last one;
  commit objects it cannot account for wait for retention, and the deleting component acts only for indexed scopes.
  Readiness is defined for captures with scopes outside the index.
- [STO §5] the catalog MAY index only a window of scopes: eviction of settled scopes is one transaction serialized with every admission, and catalog reads return one consistent snapshot.
  Scopes that are not indexed are read through listing views built from a coherent observation, accept no admissions, and make results `incomplete` with reason `cold`.
  Per-capture end evidence is recorded from every segment or converter archive presented for registration, indexed or not, and never removed;
  a rebuild reads every retained pack, MAY index only the window, and starts after the lost catalog's deleting component has stopped.
  Retention is a logical boundary with the terminal outcome **removed**, followed by physical deletion of packs, then commit objects, then catalog entries.
- [STO §6] the coverage of a patch not yet folded applies directly in the view.
- [STO §8] library vectors join the corpus; service vectors (end evidence, window and lifecycle) are listed separately and are not part of the [FMT §16] corpus.
- [SEM §1], [SEM §6], [SEM §7.2] and [SEM §7.4] take completeness from [STO §5], read listing views where the catalog does not index a scope, and report `cold`.
- [OVW §2], [OVW §5] (clock reset past retention; end evidence removed by retention), [OVW §6] (window and retention as service parameters; P4; P5),
  `tracepack-go.md` (`Result.Incomplete` reason and scope, `Result.Removed`, `PackSource`, `ActiveView`, CLI removed outcome) and `tracepack-impl-plan.md` phases 5 and 6 updated to match.
- Moved out: admissions for scopes that are not indexed (proposal P4); capture end evidence beyond the retained packs and rebuilds that read only the window (proposal P5, G5-61..G5-64 deferred by G5-65).
- Cost: a rebuild bootstraps every retained pack, about 6.5 million for 1500 tools over 6 months; it is a disaster path.

### P3 review round 1

| Finding | Resolution (v2.7) |
|---|---|
| P1 pack and commit listings can activate a cancelled patch during concurrent recovery | [STO §5] No admissions outside the index, Coherent observation; cold admissions → P4 |
| P1 neighbour completeness interval unsound; distant barriers undiscoverable | neighbour rule dropped; [STO §5] Completeness `cold`; [STO §5] Per capture evidence from every presented segment |
| P1 re-import before registration too late to ignore staging | [STO §5] listing views include `staging/`; registration outside the index rejected |
| P1 window-only rebuild loses unsettled old scopes | [STO §5] Restart and rebuild: listing views; pending work waits for retention (or P4) |
| P1 re-importing an old scope blocks readiness | no re-import; [STO §4] Readiness starts on indexed scopes in the window |
| P2 retention order can expose an older generation | [STO §5] Retention: logical removal boundary |
| P2 re-import numbering and claim identity | P4 |
| P2 affected-text inventory | proposal §4.7 |

### P3 review round 2

| Finding | Resolution (v2.7) |
|---|---|
| P1 paginated commit listing can activate a cancelled patch | [STO §5] Coherent observation step (1); [STO §8] pagination vector |
| P1 orphan-commit cleanup from an unordered absence observation | [STO §4] Deletion: commit objects deleted by the component that deleted their packs; orphans left to retention |
| P1 eviction does not hand readers and admissions off atomically | [STO §5] Window (serialized eviction, consistent snapshot, listing-view fallback), Transactions, Serialization |
| P1 cold barriers can invalidate indexed results | [STO §5] Per capture, Completeness, Restart and rebuild (revision 6); the fallback of revisions 3–5 → P5 |
| P2 retention in-flight contract; false orphan invariant | [STO §5] Retention |
| P2 definitions and vectors vs the narrowed model | [STO §5] Coherent observation (late segments), Listing view (capture discovery); [STO §4] Readiness; [STO §8] settled and unsettled vectors |
| O1 scope | [STO §2] uncommitted packs included; [STO §5] Restart and rebuild states the all-fences-lost lower bound |
| P2 quote-level inventory | applied per the checklist |

### P3 review round 3

| Finding | Resolution (v2.7) |
|---|---|
| P1 a cold-only successor hides the condition that activates the conservative barrier | P5 for rebuilds that read only the window; [STO §5] Restart and rebuild reads the boundary from every retained pack; [STO §8] cold recovered boundary |
| P2 observation proof valid only for append-only commits | [STO §5] Coherent observation premises (i) and (ii); [STO §4] Deletion only for indexed scopes; rebuild after the deleting component stops |
| P2 retained end evidence must precede the synthetic fallback | no fallback; [STO §5] Completeness takes end evidence from per-capture entries |
| P2 serialization wording | [STO §5] Window, Serialization |
| P2 readiness with no indexed scope in the window | [STO §4] Readiness |
| P2 metadata-only view vs validated footer evidence | [STO §5] Coherent observation step (3); [SEM §6] |
| P2 `Result.Incomplete` reason for ordinary queries | `tracepack-go.md` `Result`, `FindTransaction` |
| P2 commit-deletion authorization inferred from `ActiveView` | `tracepack-go.md` `ActiveView`; impl plan phase 5 |
| P2 removed-hour terminal outcome for streaming output | [STO §5] Retention; `tracepack-go.md` `Result.Removed` and §7; impl plan phase 6 |

### P3 review round 4

| Finding | Resolution (v2.7) |
|---|---|
| P1 a crash between the boundary pack and its end object hides the barrier | P5 |
| P2 registry caching needs an observation and refresh contract | P5; [STO §5] Window puts per-capture entries in the scope snapshot |
| P2 bounded-end vector contradicts the epoch rule | [STO §5] Per capture (epoch closures, kept after eviction), Completeness; [STO §8] bounded unclean end vector |
| P2 segment view sentence | [STO §5] recovery bullet |
| P2 per-capture fields from registry-only evidence | P5 |
| P2 successor ordering for every writer | P5 |
| P2 epoch barrier and closure sentences not inventoried | [STO §5] Completeness ("still open") |
| P2 removed outcome through the CLI | `tracepack-go.md` §7; impl plan phase 6 |
| P2 converter end object | P5; [STO §5] Per capture includes converter archives presented for registration |

### P3 review round 5

Round 5 returned a new class of P1 introduced by the round-4 fix; the owner split the scope (G5-65).

| Finding | Resolution (v2.7) |
|---|---|
| P1 a write-ahead clean end closes epochs before the final records are uploaded | P5; end evidence enters the catalog only with the pack that holds the boundary ([STO §5] Per capture) |
| P2 registry observation and crashed-writer qualification | P5 |
| P2 disputed clean-stop `close_seq` | P5 |

### P3 review round 6 — VERDICT: ready

| Finding | Resolution (v2.7) |
|---|---|
| P2 the catalog observation must include barriers of captures outside the selected scopes | [STO §5] Window; `tracepack-go.md` `PackSource`; [STO §8] entirely cold capture |
| P2 clean-end eviction vector needs an eligibility-window qualification | [STO §8] clean end evicted, with the companion `cold` transaction lookup |
| P3 unconditional derivability sentence left standing | [STO §5] opening paragraph replaced whole |
| Recommended vectors | [STO §8] rebuild ordering, registration during a rebuild |

### P3 application review A1 — VERDICT: ready

| Finding | Fix |
|---|---|
| P3 the service-vector exclusion gave an inaccurate reason ("rather than bytes"), although corpus vectors already include rebuilds, paginated observations and moving retention boundaries | [STO §8] the split is stated by ownership: corpus vectors exercise views and what a reader or pack source computes; service vectors exercise catalog transactions and lifecycle execution |
| owner follow-up (2026-09-27): STO §3 named no actor for deleting lower-numbered fence objects, which Deletion and Retention do not cover | [STO §3] the removal rule covers packs and commit objects; lower-numbered fence objects MAY be deleted by the component that performs §4 Deletion |

## Changes v2.8 → v2.9: stateless recorder over a durable bus (owner decisions G5-80, G5-86, 2026-09-27)

Source: the log-service review of the specification and the owner's answers; no proposal document.
Format version stays 1.0: no byte changes.

Summary:
- [STO §4] (new bullet) Recorder over a durable bus: producers assign capture identity and seq and publish to a bus that persists before acknowledging;
  the recorder is a set of stateless consumers that acknowledge only after the segment is durable; redelivery replaces spool recovery;
  interleaved segments of one scope are normalized by the merge;
  the capture descriptor supplies the producer-owned pack metadata through the catalog, and a record whose descriptor is unknown is deferred;
  closing a crashed producer's capture is split out as proposal P7 after review rounds 2 and 3, and such a capture stays `open` until then.
- [SEM §4] and [SEM §5] name the producer as the clock-step actor for a durable-bus capture; the pack writer preserves the events and nothing re-checks the rule.
- [FMT I-7] and [FMT §5] `flush_interval_ns` follow the producer's role and the consumer's flush interval; [STO §2] a bus consumer writes a subset per segment and may write several; [STO §3] takeover on lease expiry and the late commit-object write.
- [STO §8] durable-bus vectors.
- [SEM §5] a socket-refusal notification may source the transition record [FMT I-7] requires for an accepted-then-refused socket.
- [FMT I-12] seq may be assigned by the producer that hands the writer its records; a writer never renumbers a received seq.
- [STO §3] tool leases in the catalog for multi-instance publishing, a new publisher epoch per acquisition (G5-87); [OVW §6] ingest partitioned by tool deferred as a second-phase optimization.
- `tracepack-go.md` §2 and `tracepack-impl-plan.md` phase 2 updated: the writer accepts producer-assigned seqs.

## Changes v2.7 → v2.8: redaction by masking extracts (proposal P6, owner decisions G5-66..G5-76, 2026-09-27)

Source: proposal P6 revision 3 (kept outside the repository); two review rounds (round 2 ready).
Format version stays 1.0, because nothing is released.
E5 message structures and paths: the E5 redaction-paths research note, from SEMI E5-0813 (kept outside the repository).

Summary:
- [SEM §8] (new) redaction: records are stored in full; a consumer that is not privileged receives an extract written under a redaction policy.
  The policy's item rules (S/F, path pattern, domain) match on payload values; the screened message text is every captured byte after the frame prefix.
  Leaf contents are zero-filled with item headers and localized-string headers kept; an invalid text or an unresolved pattern gets one whole-text mask; texts without content are never masked.
  Each mask has an entry with an HMAC-SHA-256 digest; masked values are unavailable for every analysis; readers validate entries per record.
  An informative baseline covers S7, S13F6, S15 and S16 recipe content, with S10, S14 and S2F41 / S2F49 as per-site candidates.
  The conformance vectors move to [SEM §9].
- [FMT §5] tags 0x0031 `redaction_policy` and 0x0032 `redaction` with their nested registries; [FMT §9] `digest_algorithm`;
  [FMT §4] `redaction-present` defined; [FMT §2] redaction digests; [FMT §16] redaction vectors.
- [FMT I-8], [FMT I-11], [FMT §7.2], [FMT §8], [FMT §13] and [FMT §15] scoped for masked extracts: stored packs keep every byte; extracts are never repaired into patches; downloads to consumers that are not privileged are masked extracts.
- [SEM §2], [SEM §3], [SEM §6] and [SEM §7.3] follow: evidence grades and reclassification apply only to values that are not masked.
- [STO §2] `extract` row and [STO §3] access requirement; [OVW §1], [OVW §2] and [OVW §6] (policy file format and key management; capture-time redaction with escrow, as a cost analysis);
  `tracepack-go.md` (`Extract`, `Reader` validation, `Repair` rejects extracts, `dump --sml` rendering) and `tracepack-impl-plan.md` (new phase 7) updated to match.
- Moved out: the policy file format, key management, the access-control model and capture-time redaction with an escrowed original (G5-72, G5-74).

### P6 review round 1

| Finding | Resolution (v2.8) |
|---|---|
| P1 the whole-text range was undefined for over-long captures, so bytes beyond the declared length could escape | [SEM §8] Message text: every captured byte after the 14-byte prefix, whatever the declared length; a writer that cannot screen fails the extract; [FMT §16] declared-length vectors |
| P2 path resolution and localized-string validity not total | [SEM §8] Paths: every step requires a list, a failed branch fails the pattern, an empty `*` succeeds; a 1-byte format-22 body is invalid |
| P2 empty matches and range validation underspecified | [SEM §8] Masking (target bytes, empty targets yield no entry) and Marking (canonical ranges, already-zero bytes included, reader validation) |
| P2 C8/C11 contradicted by denial replies | [SEM §8] a valid text without content bytes is never masked; baseline note; denial vectors |
| P2 digest representability and comparison | [SEM §8] Policy (domain length) and Digest (algorithm, key id, byte-equal domain, encoding-sensitive equality); two test keys ([FMT §2], [FMT §16]) |
| P2 precedence for interpreted values and rendering | [SEM §8] Masked record; [SEM §2] evidence grades; `tracepack-go.md` §7 rendering |
| P2 F-4 rule vs [FMT I-3] | [SEM §8] F-4 skips `redacted` records and marks the block `partial`; [SEM §7.3] |
| P2 inventory gaps and contradictory acceptance criteria | [FMT I-8], [FMT I-11], [FMT §7.2], [FMT §8], [FMT §13]; [SEM §1]–[SEM §3]; impl plan §4 and phase 7 criteria; [OVW §2] phases; repair rejects extracts |
| P3 "a policy that names the wrong path" | proposal only (limits narrowed to unresolved paths) |

### P6 review round 2 — VERDICT: ready

| Finding | Resolution (v2.8) |
|---|---|
| P2 the reader's checklist and the fallback scope were incomplete | [SEM §8] Marking: structural checks at bootstrap, per-record checks at read time, wholly masked records, annotation scope, FMT rejection rules kept separate |
| P2 when validation happens was unclear | [SEM §8] Marking; `tracepack-go.md` `Reader`: a defect affects only its records; [FMT §16] early/late vector |
| P2 extraction from masked inputs | [SEM §8] Marking; `tracepack-go.md` `Extract`: masked inputs rejected before output; [FMT §16] vector |
| P2 remaining inventory rows | [FMT §8] `raw`; [SEM §7.3] `partial`; `tracepack-go.md` `Reader`; impl plan phase 7 keeps F-4 out of scope |

### P6 application review A1 — VERDICT: ready

| Finding | Fix |
|---|---|
| P2 `digest_algorithm` listed among the payload enums | moved to the pack-metadata enums of [FMT §9], noted as nested in `redaction_policy` |
| P2 FMT Depends-on omitted the redaction contract | [FMT] Depends on adds [SEM §8] |
| P2 impl plan phase 6 corpus scope reached phase-7 work | phase 6 generates the [FMT §16] vectors except the redaction vectors; phase 7 generates and accepts them |
| Proposal gap: the extract-input vector rejected any extract, the rule only masked ones | vector aligned with the rule in [FMT §16] and P6: an unscreened extract holds records in full and is a valid source (owner decision G5-76) |

## Go reference implementation: module path (owner decision G5-77, 2026-09-27)

Specification version unchanged (v2.8); only `tracepack-go.md` and `tracepack-impl-plan.md` change.
- Module path `github.com/arloliu/go-secs/v2/tracepack` (R3-4) → `github.com/arloliu/go-secs/tracepack`, nested in `tracepack/`, tags `tracepack/vX.Y.Z`:
  the go command resolves the old path to the directory `v2/tracepack/`, so it would not be fetchable outside the repository.
- Local development through `go.work`; a released tracepack requires a released go-secs v2 version; phase 0 verifies resolution from outside the repository.

## Go reference implementation: repository integration and release order (owner decisions G5-78, G5-79, 2026-09-27)

Specification version unchanged (v2.8); only `tracepack-go.md` and `tracepack-impl-plan.md` change.
- `go.work` stays local and gitignored;
  `tracepack/go.mod` has no `replace` and requires a `main` pseudo-version until a go-secs release suffices (impl plan §2.1).
- Phase 0 gains the Makefile targets, a separate CI job (blocking workspace leg, advisory `GOWORK=off` consumer leg) and the `tracepack/vX.Y.Z` release steps.
- go-secs v2.5.0 is released first; the first tracepack release requires v2.5.0 or later.

## Changes v2.9 → v2.10: outbound observation time (2026-09-28)

Source: the go-secs v2.6.0 recorder prototype, which timed the frames it sent when their write returned
and so recorded replies up to 170 µs before the frames they answered; no proposal document.
Format version stays 1.0: no byte changes.

Summary:
- [SEM §4] a frame the vantage sends itself is observed complete when it is handed to the socket, not when the send returns,
  so its `ts_utc_ns` never follows that of a reply the peer sends in answer.
