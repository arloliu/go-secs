# Changelog

All notable changes to the `github.com/arloliu/go-secs/tracepack` module are documented here.
Releases are tagged `tracepack/vX.Y.Z` on `main`, independently of go-secs `vX.Y.Z` tags.

## [Unreleased]

This release follows the format revision of spec v2.13, which redefines format 1.0 in place, and its amendment in spec v2.17.
It adds the reader and verification (spec v2.14), repair (spec v2.15 and v2.19), the active view and merge of a scope (spec v2.16 to v2.18),
one read over several packs (spec v2.20), and the lookup of a transaction from its primary (spec v2.21).

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
  among them a first validated block whose first seq is not the pack's `seq_start` (`WriterDefectSeqStart`, spec v2.19),
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
- `MergeIterate` reads the records a `Query` selects from several packs, typically the packs of the active views of the scopes a query touches,
  in the order `MergeIterateOptions.Order` requires, and returns the read's `Result` (spec v2.20).
  It reads every pack given, and its guarantees cover those packs.
  The blocks of one capture whose seq ranges overlap, across every pack, form an overlap cluster;
  a block without an F-2 entry, from a forward walk, is read once before anything is yielded, to take the summary an index would state.
  A cluster is excluded, and not read, only when the footer or those summaries exclude every block of it;
  every block of any other cluster is read, a walked one a second time.
  In a cluster read, the copies of each seq are compared byte for byte, record header and payload, before the filter is applied, whatever `Query.Payloads`.
  Identical copies are one record, yielded once from their representative, the first block holding them, which `Item.Pack` and `Item.Block` name.
  Copies with different bytes are a conflict, reported and never hidden:
  every version the filter selects is yielded with `Item.Conflict` set,
  and the conflict is listed in `Result.Conflicts`, whether or not a version was selected.
  `OrderCapture` yields each capture's records in ascending seq, the captures interleaved block by block:
  the next record comes from the capture whose next selected record lies in the block with the smallest `ts_min`;
  `OrderTime` yields them in ascending (`ts_utc_ns`, `capture_id`, `seq`), then version,
  reading the clusters in ascending `ts_min` and yielding the records below each cluster's `ts_min` before reading it.
  Captures compare by `capture_id` as raw UUID bytes.
  `MergeIterateOptions.MaxHeldBytes` bounds the block buffers held at once, each reserved before its block is read,
  and `MaxConflicts` the length of `Result.Conflicts`;
  a read over either ends with an error wrapping `ErrReadLimit`, returned with the `Result` found so far.
  Defects are reported as `Reader.Iterate` reports them, beside the records, each naming its pack.
  `MergeIterate` never modifies a `Reader`, so readers can be shared with concurrent reads.
- Types and fields for reads over several packs:
  `Order`, with `OrderCapture`, `OrderTime` and `String`;
  `DefaultMaxHeldBytes` and `DefaultMaxConflicts`, which a zero or negative `MergeIterateOptions` limit takes;
  `MergeIterateOptions`;
  `PackError`, an error about one pack that does not make a read incomplete, with `Error` and `Unwrap`;
  `Item.Pack` and `Item.Conflict`, `Defect.Pack`, `Result.Conflicts` and `Result.FooterErrs`.
  `Conflict` describes a conflict of a read as well as one of a merge, its versions' packs then in the order the readers were given.
  `Reader.Iterate` names pack 0 in every `Item` and `Defect`, never marks a conflict,
  and sets `Result.FooterErrs` to one entry, beside `Result.FooterErr`, when the footer was not used.
- `MergeIterate` has property tests that compare each of 512 generated reads (96 under `-short`), in both orders,
  with a reference built from the stored rows and payloads of every block:
  the records yielded with their packs, blocks and conflict marks, the conflicts listed, the defects, the reads of each block, and nothing held after the read;
  and the fuzz targets `FuzzMergeIterate` and `FuzzMergeIterateGenerated`.
- `FindTransaction` looks up the reply to a primary record, named by a `TxKey` (its capture, seq and the UTC hour of its scope),
  in one observation a `PackSource` supplies,
  and returns a `TxResult` with the outcome of the semantics specification §7.2:
  `TxMatched`, `TxAmbiguous`, `TxUnmatched` or `TxIncomplete` (spec v2.21).
  It reads the primary's hour and the next `TxOptions.MaxScopes` − 1 hours, the next one by default, each once, by one `MergeIterate` in capture order,
  and every one of them even after the window closed, since a later hour can hold another version of a window record or a smaller bound.
  From the primary it derives the transaction key, the epoch, direction, SessionID and System Bytes, and the stream and function to match;
  it fails with `ErrNotPrimary` when the record is not a data record with an available odd function,
  or when a complete read of an indexed scope holds no record at that seq.
  It searches only the seqs above the primary's.
  The window ends at the smallest seq of a same-key primary, or of a closing record without a conflict, that the lookup read:
  a socket-close of the primary's epoch, or a clean capture stop.
  A footer's or the per-capture evidence's claim that an epoch closed never bounds the window,
  and a claim that the records read contradict is reported;
  so is a claim at or below the primary, since it says the primary's epoch or the capture ended before the primary.
- A conflict on a seq at or above the primary's, a block whose records disagree with its F-2 entry, or a primary without a key makes the outcome `TxIncomplete`.
  Otherwise one valid match is `TxMatched` and several are `TxAmbiguous`, each with every gap found listed beside it.
  Without a valid match the outcome is `TxUnmatched` only when nothing prevents establishing absence:
  the lookup read a bound and every seq of the window below it;
  no scope read is cold, and no pack read is damaged, unevaluated, or has a coverage entry that meets the window;
  the per-capture evidence is complete and no claim of it is contradicted;
  no stop-unclean barrier applies, one of the capture unless the lookup read a closing record of the primary's epoch, or one of the tool whose gap meets the hours read;
  no other capture boundary and no ordering-uncertain record of the primary's epoch was found, and the primary is not correlation-incomplete;
  and no possible reply, undecidable candidate, or candidate at or after a possible same-key primary lies in the window.
  Each condition found is a `TxGap` in `TxResult.Gaps`, naming its hours, pack, block and seq where it has them.
- The comparison of a record's copies covers the scopes the lookup reads:
  another version of a record in an hour the lookup did not read is not compared,
  and such a version is not found by verifying one pack or merging one scope either.
  A seq of the window with no version in the hours read is a seq gap, never an absence.
- `TxResult.Records` holds every version the lookup kept, with its roles, its window and candidate flags,
  `Searched` the scopes read to their end, `Conflicts` the conflicts the reads listed, and `FooterErrs` the footer errors of the packs read;
  every record, coverage entry and boundary it holds is a copy, valid after the lookup.
  `TxOptions.MaxHeldBytes` bounds the block buffers of each read as `MergeIterateOptions.MaxHeldBytes` does,
  `MaxConflicts` the conflicts of the whole lookup, those between hours included,
  and `MaxStateBytes` the lookup's own state, the records it keeps with their payloads among them;
  a lookup over any of them ends with an error wrapping `ErrReadLimit`, returned with the `TxResult` found so far and no outcome.
  `ErrInvalidQuery` rejects, before anything is read, a nil source, a zero capture, a seq above 2^63−1, an hour outside [`MinTxHour`, `MaxTxHour`],
  and a last hour read past `MaxTxHour`.
  `ctx` is checked before each call to the source, during each read, and every 4096 iterations of the lookup's own loops.
- `PackSource` and `Observation` are the source of a lookup, the storage specification's observation of a lookup:
  `PackSource.Observe` fixes, before it returns, the view of every scope of the hours asked, the capture's per-capture evidence and the tool's stop-unclean barriers;
  `Observation.Scope`, `Evidence` and `Barriers` answer from what it fixed, every value returned being the caller's,
  and `Close` releases it without blocking.
  `FindTransaction` closes the observation it opened exactly once, whatever happened.
  The module provides no `PackSource`: a caller implements one over its catalog and storage.
- Types for a transaction lookup:
  `TxKey`; `TxOptions`, with `DefaultTxMaxScopes` and `DefaultTxMaxStateBytes`, which a zero or negative limit takes; `MinTxHour` and `MaxTxHour`;
  `TxOutcome`, with `TxMatched`, `TxAmbiguous`, `TxUnmatched`, `TxIncomplete` and `String`;
  `TxClass`, one bit per role (`TxPrimary`, `TxCandidate`, `TxPossibleReply`, `TxSameKeyPrimary`, `TxPossiblePrimary`, `TxClosing`, `TxOutcomeRecord`),
  with `Has`, `Names` and `String`, which name undefined bits too, unlike those of `Quality`;
  `TxRecord`; `TxGapReason`, one value per reason of the semantics specification §7.2, with `String`; `TxGap`; `TxScope`;
  `TxPackError`, with `Error` and `Unwrap`; `TxResult`;
  `SourceScope`, `CaptureEvidence`, `EpochClosure`, `Boundary`, and `EndState`, with `EndOpen`, `EndStopped`, `EndStoppedUnclean` and `String`.
- `FindTransaction` has a property test that looks up every record of 96 generated captures (24 under `-short`)
  and requires each result to equal one computed from the scopes read independently of the lookup,
  and each `TxUnmatched`, and each `TxMatched` without a gap, to hold over the capture as written, versions in hours not read excluded;
  and the fuzz targets `FuzzFindTransaction` and `FuzzFindTransactionGenerated`.
- `Merge` has property tests over hand-written vectors and 64 generated scopes,
  which recompute F-2, F-3 and F-5 from each archive's records independently of the Writer,
  and check the record union, the size and greedy-coalescing rules and the report's counts;
  and the fuzz targets `FuzzMerge` and `FuzzMergeGenerated`.
- `Record.HSMSHeader` and `HSMSHeader`, the HSMS header fields of a payload with their availability kept separate;
  `Record.SetCapturedFieldValidity`.
- `DecodeStatus.Clean`, true only for a known classification outside the malformed set, so an unknown value is neither clean nor malformed.
- Sentinel errors `ErrNotTracepack`, `ErrUnsupportedFormat`, `ErrChecksum`, `ErrReadLimit`, `ErrInvalidFooter`, `ErrInvalidQuery`,
  `ErrFieldValidity`, `ErrFieldValue`, `ErrRepairNotNeeded`, `ErrNotRepairable`,
  `ErrViewConflicted`, `ErrMergeInput`, `ErrMergeConflict`, `ErrMergeLimit` and `ErrNotPrimary`.

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
