# Changelog

All notable changes to the `github.com/arloliu/go-secs/tracepack` module are documented here.
Releases are tagged `tracepack/vX.Y.Z` on `main`, independently of go-secs `vX.Y.Z` tags.

## [Unreleased]

This release follows the format revision of spec v2.13, which redefines format 1.0 in place, and its amendment in spec v2.17.
It adds the reader and verification (spec v2.14), repair (spec v2.15 and v2.19), and the active view and merge of a scope (spec v2.16 to v2.18).

### Upgrade notes

- **Packs written by tracepack v0.1.0 are not supported.**
  The header section now stores the record headers column by column, and the record header shrank from 56 to 44 bytes.
  A reader cannot tell the two layouts apart:
  a block of several records usually fails its integrity check and reads as corrupt,
  and a block of one record reads with its fields from byte 36 on misread.
  Do not keep v0.1.0 packs where this version reads.
- `Record` no longer holds copies of the HSMS message header:
  read `SessionID`, stream, W, function, PType, SType and System Bytes with `Record.HSMSHeader`,
  and set the `field_validity` of a raw capture with `Record.SetCapturedFieldValidity` instead of `SetHeaderCopies`.
- `Writer.Append` rejects a data or control record whose `field_validity` marks bytes its payload lacks, with `ErrFieldValidity`.
- `PackMeta.Classifier *string` is now `Classifiers []string`, and `PackMeta.MaxFrameLen *uint64` is now `MaxFrameLens []uint64`:
  the two tags are repeatable, so a merged archive names every classifier and `max_frame_len` of its inputs.
  A recorder or converter still writes one value:
  set `Classifiers: []string{c}` where it set `Classifier: &c`, and test `len(m.Classifiers) > 0` where it tested `m.Classifier != nil`.
- `PackMeta.Validate`, `UnmarshalPackMeta` and `MarshalBinary` accept a `compaction_level` of 1 or more without `compacted_from`:
  the tag is required only when the pack's lineage list is non-empty, which the pack metadata alone does not show.
- `MarshalBinary` writes `classifier` and `max_frame_len` among the repeatable tags,
  which moves their bytes in newly written pack metadata, so a golden of encoded metadata changes.

### Changed

- Format (spec v2.13, format 1.0 redefined in place, before any production use):
  the header section of a block body stores the record headers column by column,
  byte j of record i at offset j × record_count + i;
  the record header is 44 bytes, its extension area starting at byte 44;
  the quality bits decode-failed and no-mono and the record_flags W bit are retired and written as zero.
- Format (spec v2.17, format 1.0 redefined in place again, under the same pre-release exception of the format specification §14):
  `classifier` and `max_frame_len` are repeatable,
  and `compacted_from` is present only when the pack's lineage list is non-empty, whatever its `compaction_level`.
  A reader written for spec v2.13 to v2.16 rejects a pack that uses either change, while every pack those versions wrote stays valid.
- Spec v2.18 states that an epoch's `close_seq` is the lowest seq among the records that end the epoch,
  and that a capture-boundary record is a transport event whose payload's `event` is capture-boundary, whatever its quality bit 0;
  the Writer, footer validation and `Verify` already followed both rules, so no behaviour changed.
- Removed from the API: `Record.SessionID`, `Stream`, `W`, `Function`, `PType`, `SType`, `SystemBytes` and `SetHeaderCopies`;
  `QualityDecodeFailed`, `QualityNoMono`, `RecordFlagsW`;
  `PackRoleCorrection`, whose value 5 now prints `unknown(5)`;
  `PackMeta.BlocksValidated` and `ErrSchemaVersion`.
- `Writer`:
  `WriterOptions.Validate` decodes each encoded block, gathers its record headers and checks them before writing it,
  but commits nothing to the pack metadata;
  the Writer no longer derives decode-failed or no-mono,
  never writes `schema_version`, `blocks_validated` or any other retired pack-metadata tag, also not from `PackMeta.Unknown`,
  and no longer writes the F-3 `max_payload_len` and `content_bytes` tags.
- `PackMeta`: `MarshalBinary` rejects `pack_role` 5 with the new `ErrFieldValue`,
  and a `replacement_set_size` other than 1 or a `replacement_set_index` other than 0 is rejected on read and on write with it;
  `UnmarshalPackMeta` keeps a retired tag in `Unknown` like any unknown tag.
- `Coverage` gains `Unknown`: the nested entries of a coverage entry that this package does not know are kept in stored order,
  where they were dropped, and `MarshalBinary` writes them after the known nested tags, which it writes in tag order.
- `Writer`: `Close` rejects a block summary too long for the footer's u32 summary length instead of narrowing it unchecked,
  and the documentation no longer promises that a failed Writer leaves the pack unfinalized:
  a failed footer-and-trailer write, or the sync after it, can come after the trailer reached the output.
- The Writer, `Verify` and `Repair` summarize a block's epochs in time linear in its records,
  where a block of n records in n epochs took time quadratic in n.
- Reader: `Open` no longer uses a footer whose blocks' F-3 lists, each counted once per block that locates it,
  total more than `ReaderOptions.MaxFooterLen`;
  `PackHeader.FooterErr` wraps `ErrReadLimit`,
  and the blocks are found by the forward walk, as for a footer over the budget.
  The format lets lists overlap, and every block parses its own,
  so blocks sharing one long count array took memory in proportion to their number times the array's length.
  Every reader entry point follows:
  `Iterate` reports it in `Result.FooterErr`, `Verify` and `Repair` fail with it,
  and `Merge` fails with it naming the input.
  No pack whose lists lie end to end, as the Writer and `Merge` write them, is affected.

### Added

- Reader:
  `Open` bootstraps a `Reader` over an `io.ReaderAt` of known size in rounds of concurrent reads:
  one round when the pack metadata and the footer fit the head and tail windows or a catalog hint locates the footer, two otherwise.
  It validates the file header, the format version and the pack metadata before any block is read.
  `ReaderOptions` sets the windows, the whole-read threshold and the reader budgets `MaxPackMetadataLen`, `MaxFooterLen`, `MaxBlockLen`
  and `MaxWalkedBlocks`, the last bounding the block index of a forward walk,
  each checked before the structure is allocated.
  `Header` returns a fresh `PackHeader` with the pack metadata, `Finalized`, `FooterErr` and the trailer,
  and `Blocks` returns the block index.
  A footer that fails its CRC, its validation or a budget is never trusted:
  `FooterErr` reports it, and the forward walk finds the blocks instead.
- `Reader.Iterate` yields the records a `Query` selects, in file order, and returns the read's `Result`.
  Every block it reads is decoded in full and checked before any of its records is used.
  A `Filter` selects on time, kind, direction and epoch,
  and on the stream and function, session ID and System Bytes read from each payload,
  a field being available only when its stored `field_validity` bit is set and the payload holds its bytes;
  an unavailable field matches only on request.
  The footer's time and epoch ranges and kind and direction counts prune blocks, trusting a footer that passed validation;
  `Incomplete` reports corrupt, unknown-codec, truncated and over-budget blocks and coverage entries that intersect the query,
  beside the records that were read,
  and a block whose records disagree with its index entry, whose records are still yielded.
  Each `Item` carries the record as stored, with its payload when `Query.Payloads` is set,
  the record header's extension area that a newer minor version appends, and its block.
  A `Reader` is safe for concurrent `Iterate` calls.
- `Verify` reads every block of a pack in full and returns a `VerifyReport` with its `Outcome`:
  the first that applies of corrupt-middle, unfinalized, finalized-truncated, finalized-inconsistent and finalized-consistent.
  A valid footer locates the blocks, passing a block whose envelope fails; without one, the forward walk does.
  The report lists failed blocks and the walk's stop with their offsets, the end of the validated prefix,
  footer disagreements with the records or the trailer (`ReasonIndexMismatch`),
  `WriterDefect`s that never change the outcome,
  among them a first validated block whose first seq is not the pack's `seq_start` (spec v2.19),
  and, with a valid footer, the seq and time ranges of the failed blocks as `Coverage` entries.
  An unreadable file header or pack metadata is an error, and so is a block or a finalized pack's footer over the reader budgets.
- `Repair` writes a generation-0 repair patch of a damaged stored pack to an `io.Writer`, returning a `RepairReport`.
  It verifies the pack first and decides every refusal before writing:
  `ErrRepairNotNeeded` for a finalized-consistent pack,
  unless its first record's seq is not its `seq_start`, which the patch corrects so that `Merge` accepts it (spec v2.19),
  and `ErrNotRepairable` for a pack that is not stored, a period or validated block outside one UTC hour,
  a seq-order or hour-span writer defect, a lost run with no seq between its neighbours,
  or records that breach the pack metadata's commitments.
  The patch copies every validated block byte for byte and rebuilds the footer from the copied records;
  its metadata names the damaged pack in `supersedes`, takes `patch_base` and `writer` from `RepairOptions`,
  and carries the damaged pack's coverage followed by entries for the lost records:
  a valid footer's ranges when no validated block disagrees with it, else the seqs between each lost run's validated neighbours within the scope's hour.
- `ActiveView` returns the `View` of one scope, one capture in one UTC hour,
  from a complete observation of the scope's packs and commit objects:
  its current `Generation`, the packs whose records the view reads, the `compacted_from` the next merge writes,
  the packs deletable under the storage specification's deletion rules, and the packs excluded for their role.
  Supersession is transitive (spec v2.16):
  a patch based on the current generation removes the packs it names, also once a later patch replaces it.
  Each pack is a `PackInfo`, built only from decoded pack metadata by `Reader.Info` or `NewPackInfo`,
  and a `CommitSet` holds the ids of the commit objects.
  For packs that do not form one consistent scope it returns the zero `View` and an error,
  and for two different complete committed sets of the highest rank an error wrapping `ErrViewConflicted`.
- `Merge` writes the next generation of a scope as one archive, from its `View` and a `MergeInput` for each pack `View.Packs` names,
  configured by `MergeOptions` and returning a `MergeReport`.
  Before it writes a byte it checks the options, the claimed rank and ids against the view,
  and each input from its pack metadata and footer:
  a pack of the view, finalized with a valid footer, inside the scope, within the commitments of its own pack metadata,
  and carrying the same capture-level tags as the other inputs.
  It reads every input block in full, once, and checks it before it writes any of its bytes.
  A block that no other block overlaps is copied verbatim with its F-3 list, an exact duplicate is dropped,
  and every other overlap is resolved record by record into new blocks within `MergeOptions.BlockThreshold`, each checked in memory before it is written.
  Unless `MergeOptions.NoCoalesce` is set, it coalesces small blocks greedily:
  consecutive blocks of one `record_header_len` while their decoded size stays within the threshold,
  a block reaching the threshold alone left as it is.
  A coalesced encoding that fails its codec or its check is discarded, and the blocks are written as they were.
  A group waits for the block after it, and is not written at all when an error or a conflict comes first.
  The archive's pack metadata holds the claim, the lineage (`compaction_level`, `compacted_from`, `supersedes`), the inputs' capture-level tags,
  their coverage entries each once, and every distinct `classifier` and `max_frame_len` of the inputs;
  an archive without records takes the largest `seq_start` of its inputs.
  `MergeReport` counts the input blocks `Copied`, `Duplicates`, `Resolved` and `Coalesced`, which sum to the number of input blocks,
  the new blocks of the resolution (`ResolvedEncodings`) and of coalescing (`CoalescedEncodings`), and the fallbacks (`CoalesceFallbacks`);
  `ResolvedEncodings` also counts a block of the resolution that coalescing then encodes again.
- After any error of `Merge` the caller discards what it wrote and publishes nothing;
  it publishes the archive only after `Merge` returns nil, under the storage specification's commit protocol.
  An error before the archive's header leaves the output untouched, and one after it leaves no trailer;
  only an error of the footer-and-trailer write or of the sync after it can leave bytes that form a finalized pack.
  `ErrMergeInput`, wrapped with the pack and the cause, refuses an input that is not a pack of the view,
  that is not finalized-consistent, breaches its scope or its own commitments, or differs from the others in a capture-level tag;
  a block that fails its read or disagrees with its footer is refused as it is read, after the header.
  A failed read of an input, and `ErrReadLimit` for an input over a budget of `MergeOptions.Reader`, are returned naming the input, never as `ErrMergeInput`,
  so a caller retries the read or raises the budget instead of repairing a sound pack.
  Records of one seq with different bytes are a conflict:
  `MergeOptions.OnConflict`, which is required, receives each `Conflict` in ascending seq,
  its `Versions` in the order of the first block holding each, each version's packs in the order of `View.Packs`;
  `Merge` returns `ErrMergeConflict` once it compared every block, with `MergeReport.ConflictsComplete` set.
  On error the report holds only `Conflicts` and `ConflictsComplete`.
  `ErrMergeLimit` reports a merge over its budget:
  archive pack metadata longer than `MergeOptions.Reader.MaxPackMetadataLen`, the budget the next merge opens the archive with,
  which is checked before the output is touched;
  an archive footer, or the block summaries kept for it, over `MergeOptions.MaxFooterLen`;
  or an overlap whose resolution would hold more than `MergeOptions.MaxOpenBlocks` blocks at once.
  `ctx` is checked before each block read, after every 4096 records resolved, and before the footer.
- `Merge` has property tests over hand-written vectors and 64 generated scopes,
  which recompute F-2, F-3 and F-5 from each archive's records independently of the Writer,
  and check the record union, the size and greedy-coalescing rules and the report's counts;
  and the fuzz targets `FuzzMerge` and `FuzzMergeGenerated`.
- `Record.HSMSHeader` and `HSMSHeader`, the HSMS header fields of a payload with their availability kept separate;
  `Record.SetCapturedFieldValidity`.
- `DecodeStatus.Clean`, true only for a known classification outside the malformed set, so an unknown value is neither clean nor malformed.
- Sentinel errors `ErrNotTracepack`, `ErrUnsupportedFormat`, `ErrChecksum`, `ErrReadLimit`, `ErrInvalidFooter`, `ErrInvalidQuery`,
  `ErrFieldValidity`, `ErrFieldValue`, `ErrRepairNotNeeded`, `ErrNotRepairable`,
  `ErrViewConflicted`, `ErrMergeInput`, `ErrMergeConflict` and `ErrMergeLimit`.

## [0.1.0] - 2026-09-28

The first release: the format primitives, the `Writer`, and the go-secs-backed classifier.
It writes packs but does not read them yet;
the reader, verification, merge, and extraction follow in later releases.
The module requires go-secs v2.6.0.

### Added

- Module skeleton: `github.com/arloliu/go-secs/tracepack`, nested in `tracepack/`, with Makefile targets, a dedicated CI job and the release flow.
  The format specification (tracepack v2.10, format 1.0) lives in `docs/specs/tracepack/`.
- Format primitives (internal, no public API beyond the enums yet):
  CRC-32/ISO-HDLC, RFC 9562 UUIDs with a UUIDv7 generator, the portable integer limits and sentinel errors;
  the fixed byte layouts (file header, block envelope, record header, trailer, footer prologue, block index entry) with encoders, decoders and offset-reporting errors;
  the TLV entry encoding with every tag registry and the registry validation rules;
  the block codecs `none` and `zstd` with a decoded-length-bounded decoder;
  and the public enums and bit sets of the format specification, with `unknown(<n>)` for values from a newer minor version.
  Every decoder has golden vectors from the specification and a fuzz target.
- Writer and classifier:
  `PackMeta` with every required-when rule, typed `TransportEvent` and `Annotation` payloads,
  `Record` with the positional copy helper for HSMS header fields,
  a `Writer` that commits the header and metadata first, closes blocks at the size threshold and the UTC hour,
  validates each block before writing it when asked and attests the pack, builds the footer by the aggregation rule,
  detects clock steps for a capture-clock pack, and returns the capture's next seq on close;
  and `classify.Frame`, the go-secs-backed decode-status classifier, which brings the module's first go-secs requirement.
  The writer rejects, with `ErrMetadataCommitment`, a record the committed metadata cannot describe,
  forces `correlation-incomplete` on epoch 0, owns the `capture-boundary` bit, and fails on a short write;
  the typed marshalers reject what their decoders would reject.
