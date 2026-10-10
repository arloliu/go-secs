# Changelog

All notable changes to the `github.com/arloliu/go-secs/tracepack` module are documented here.
Releases are tagged `tracepack/vX.Y.Z` on `main`, independently of go-secs `vX.Y.Z` tags.

## [Unreleased]

The conformance corpus covers reads over several packs and transaction lookups (spec v2.30),
so an implementation of either checks itself against the committed files as it already does for single packs.
With them every clause of the semantics specification §9 has a vector, or a test of this implementation where no result shows it,
but redaction and an extract beside its source, which come later.
The two clauses of the format specification §16 that need several packs have their vectors too.

### Upgrade notes

- The corpus manifest's `corpus` value is now `tracepack-corpus/2`, and its `spec_version` `2.31`.
  A consumer that requires `tracepack-corpus/1` must accept the new value;
  the files of the classes it already reads keep their form, so nothing else changes for it.

### Changed

- The corpus follows spec v2.31, after spec v2.29, which states how a log of several tools converts into captures and changed no golden,
  and spec v2.30, which defines the corpus part 2;
  spec v2.31 lets a log converter reuse its ids for reproducible output and write segments, and changes no golden.
- The corpus README's size budget per vector group and root file is now a ceiling a test enforces,
  and the README's budget table must state the same ceilings.

### Added

- Corpus vectors of the new class `multi-pack`: 13 of reads over several packs (`multi-`) and 19 of transaction lookups (`tx-`).
  Each holds numbered packs `pack-<n>.tpk`, each with its verification report `pack-<n>.verify.json`, and `reads.json`, `lookups.json` or both, without an export.
  A read gives its packs in a stated order, with its order, filter and optional conflict bound,
  and expects the versions yielded, each pack's `incomplete` reasons, the conflicts in discovery order and the footers not used,
  or the conflict-limit error with the conflicts found so far.
  A lookup names its primary, the hours to search and its source:
  the packs whose scopes it reads, in an order that decides the view order, the packs whose statistics count only as evidence, and whether the source is complete.
  It expects the outcome, the primary's fields, the window's end, every kept version with its role labels and flags,
  the scopes searched, their conflicts and unused footers,
  and canonical facts, one per condition that kept the lookup from establishing absence, in a total order;
  or the `not-primary` error.
  The tracepack corpus specification defines the class, the schemas of both files and how a lookup's source is read from the packs.
  `TestCorpus` runs every read with `MergeIterate` and every lookup with `FindTransaction` over `NewReaderSource`,
  and requires every record either returns to be the stored record its pack and block name.

## [0.2.0] - 2026-10-10

This release follows the format revision of spec v2.13, which redefines format 1.0 in place, and its amendment in spec v2.17.
It adds the reader and verification (spec v2.14), repair (spec v2.15 and v2.19), the active view and merge of a scope (spec v2.16 to v2.18),
one read over several packs (spec v2.20), the lookup of a transaction from its primary (spec v2.21),
and a source for that lookup over an object store and the caller's catalog (spec v2.22).
For recorders it adds a segment writer, which writes a capture as segments through a sink,
and a sink that stores them in a local directory laid out as a bucket;
for readers of local files, a source for the lookup over packs the caller opened (spec v2.23 and v2.24).
Every reader can apply a retention boundary, the first UTC hour still retained,
and ends a read that meets a removed hour (spec v2.25).
A pack exports as canonical JSONL, byte for byte the same from any conforming exporter (spec v2.26).
A conformance corpus holds packs and the results a conforming implementation produces from them,
so an implementation in any language can check itself against this one (spec v2.27 and v2.28).
A README groups the API by audience, and runnable examples show writing and reading.

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
- `WriterOptions.Validate` is replaced by `WriterOptions.SkipValidation`, and a `Writer` now validates each block by default:
  drop `Validate: true`, and set `SkipValidation: true` where `Validate` was left false and the check is not wanted.
- `NewWriter` refuses a period that is empty or not inside one UTC hour, and `Writer.Append` a record of another UTC hour than the period,
  both with `ErrScopeBreach`, for every pack but an extract.
  A writer of packs that span hours writes an extract, or one pack per hour as `SegmentWriter` does.
  A record of the period's hour outside the period is still accepted, and `Merge` and `Repair` write the same bytes as before.

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
  a validating Writer, the default unless `WriterOptions.SkipValidation` is set (below), decodes each encoded block, gathers its record headers and checks them before writing it,
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
- `Observation.Barriers` requires a non-empty range within the observation's hours,
  [from·3600·10⁹, to·3600·10⁹) for the from and to of `Observe`;
  an empty or reversed range, or one reaching outside those hours, is an error (spec v2.22).
  An `Observation` a caller implements must reject such ranges too;
  `FindTransaction` asks only for the hours it schedules.
- `FindTransaction` checks the `ts_utc_ns` hour of every version a scope read yields against the scope's hour.
  A version outside it, from a pack that does not keep to its scope, adds a `TxGapScopeBreach` gap
  and makes the outcome `TxIncomplete`, also beside a match;
  the version is still classified, and kept as any other version is,
  and in the primary's scope the breach explains a missing primary as a read defect does.
  The check applies to every source and to walked packs,
  so a pack already stored whose records lie in another hour now makes a lookup reading it `TxIncomplete`.
- `FindTransaction` does not read a scope the `Observation` reports conflicted:
  it lists a `TxGapConflicted` gap, after the `TxGapCold` gap of a scope not indexed,
  which prevents `TxUnmatched`, while a match found in another hour is still reported;
  a conflicted primary scope explains a missing primary as a cold one does,
  and a conflicted scope that comes with readers fails the lookup.
- `TxOptions.MaxScopes` is documented as the number of hours scheduled from `TxKey.Hour`, a conflicted one among them,
  and coverage and time barriers are checked over the hours scheduled.
  The lookup already used that range;
  the visible effect is that a conflicted hour takes its place in the range without being read,
  and the hours after it are still read.
- `Writer` (spec v2.23): `WriterOptions.Validate` is replaced by `SkipValidation`, so validation is on unless a caller turns it off;
  `Merge` and `Repair`, which check their blocks themselves, set it.
  For every pack but an extract, `NewWriter` refuses a period that is empty or not inside one UTC hour, writing nothing,
  and `Append` refuses a record whose `ts_utc_ns` lies in another UTC hour than the period, both with the new `ErrScopeBreach`.
  A refused record is not added, takes no seq and inserts no clock-step record, and the Writer stays usable.
  A record of the period's hour outside the period is accepted, since a clock step within the hour puts it there.
- `Result.Complete()` reports false while the new `Result.Removed` is non-empty, as while `Incomplete` is (spec v2.25).
- `FindTransaction` ends with an error wrapping `ErrRemoved` on a scope the `Observation` reports `Removed`,
  whether or not `TxOptions.Retention` is set,
  and fails on a removed scope that comes with readers, as on a conflicted one (spec v2.25).
  An `Observation` a caller implements reports a removed hour that way.
- `NewStoreSource` charges each object an `Observe` opens 32 bytes against `MaxSourceBytes` on 64-bit platforms,
  and 16 on 32-bit ones, where it charged 16 and 8, since its slot now records the object's `Reader` and hour;
  an `Observe` that opens many objects reaches `MaxSourceBytes` sooner.
- `Repair` decides that a pack has nothing to repair before any refusal (spec v2.27):
  a finalized-consistent pack without the `seq_start` defect returns `ErrRepairNotNeeded` whatever its role,
  so a finalized-consistent extract, or a pack of an unknown role, is no longer refused with `ErrNotRepairable`.
  Neither error writes anything.
- Spec v2.27 states that a query reads the HSMS header fields of a control record at their positions, as for a data record:
  `Filter.SF` matches a control record on payload bytes 6 and 7 as stored, and interpreting them by SType is the consumer's.
  `Iterate` and `Record.HSMSHeader` already did so, so no behaviour changed.
- Spec v2.28 states where the forward walk of a pack without a valid footer stops:
  at an envelope that fails any of its checks, its magic, its CRC or a field outside its range;
  where too few bytes are left for an envelope; and at an envelope whose body ends past the block region.
  The reader already stopped there, so no behaviour changed.

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
- A conflict on a seq at or above the primary's, a block whose records disagree with its F-2 entry, a version outside its scope's hour,
  or a primary without a key makes the outcome `TxIncomplete`.
  Otherwise one valid match is `TxMatched` and several are `TxAmbiguous`, each with every gap found listed beside it.
  Without a valid match the outcome is `TxUnmatched` only when nothing prevents establishing absence:
  the lookup read a bound and every seq of the window below it;
  no scope is cold or conflicted, and no pack read is damaged, unevaluated, or has a coverage entry that meets the window;
  the per-capture evidence is complete and no claim of it is contradicted;
  no stop-unclean barrier applies, one of the capture unless the lookup read a closing record of the primary's epoch, or one of the tool whose gap meets the hours scheduled;
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
  `NewStoreSource` returns one over an object store and the caller's catalog, and a caller may implement its own.
- Types for a transaction lookup:
  `TxKey`; `TxOptions`, with `DefaultTxMaxScopes` and `DefaultTxMaxStateBytes`, which a zero or negative limit takes; `MinTxHour` and `MaxTxHour`;
  `TxOutcome`, with `TxMatched`, `TxAmbiguous`, `TxUnmatched`, `TxIncomplete` and `String`;
  `TxClass`, one bit per role (`TxPrimary`, `TxCandidate`, `TxPossibleReply`, `TxSameKeyPrimary`, `TxPossiblePrimary`, `TxClosing`, `TxOutcomeRecord`),
  with `Has`, `Names` and `String`, which name undefined bits too, unlike those of `Quality`;
  `TxRecord`; `TxGapReason`, one value per reason of the semantics specification §7.2, with `String`; `TxGap`; `TxScope`;
  `TxPackError`, with `Error` and `Unwrap`; `TxResult`;
  `SourceScope`, `CaptureEvidence`, `EpochClosure`, `Boundary`, and `EndState`, with `EndOpen`, `EndStopped`, `EndStoppedUnclean` and `String`.
- `FindTransaction` has a property test that looks up every record of 96 generated captures (24 under `-short`),
  conflicted scopes and packs that breach their scope among them,
  and requires each result to equal one computed from the scopes read independently of the lookup,
  and each `TxUnmatched`, and each `TxMatched` without a gap, to hold over the capture as written, versions in hours not read excluded;
  and the fuzz targets `FuzzFindTransaction` and `FuzzFindTransactionGenerated`.
- `NewStoreSource` returns a `PackSource` of one tool over an `ObjectStore`, the bucket, and a `Catalog`, the caller's catalog,
  which also provides the per-capture evidence and the tool's barriers (spec v2.22).
  Each `Observe` takes one `CatalogSnapshot`.
  A scope the catalog indexes is fixed from it:
  the source opens each `CatalogPack` of the scope's view, in view order, and checks it against its key and its descriptor.
  A scope the catalog does not index is fixed by the coherent observation of the storage specification §5:
  its commit objects are listed until two consecutive complete listings agree,
  then its archive packs and the capture's staging segments are listed once;
  each segment's head is read to learn its hour, and a segment of an hour not observed is closed, its footer never decoded;
  the packs are opened and checked, `ActiveView` computes the view,
  and last the catalog must confirm that the scope stayed not indexed since the snapshot, or `Observe` fails.
  A conflicted view, in the snapshot or in a listing view, is fixed as conflicted, without readers.
  A listed pack whose role takes no part in a view is closed,
  then reported as an `ExcludedPack` to the required `StoreSourceOptions.OnExcluded`, once per pack per `Observe`.
- Every pack the source opens must agree with its key, and with its descriptor for an indexed scope, on pack_id and capture_id;
  its tool_id is the source's tool, and its period lies in the scope's hour;
  a key under `staging/` holds a segment, and one under `archive/` a pack of another role;
  its size is the one the listing or the catalog gives, and its generation suits its role.
  A segment key's `seq_first` must be the first seq of the segment's first block,
  or the `seq_start` of a segment known to hold no record;
  it is not compared when damage hides the first seq, which the reads over the scope then report.
  Every key a listing returns is parsed before it is classified,
  and a listing that breaks the `ObjectStore` contract fails the `Observe`:
  a key outside its prefix or not above the one before it, or a token that comes again.
  A pack that is gone when opened or read fails the `Observe` with an error wrapping `ErrObjectNotFound`,
  which a caller may retry, unless `StoreSourceOptions.Retention` shows its hour removed (below);
  a listed or catalog key that does not parse fails it with an error wrapping `ErrInvalidKey`, which a retry does not cure.
  An `Observe` that fails closes every object it opened.
- `StoreSourceOptions` sets the bucket prefix, the tool, the `ReaderOptions` of every pack, the callback and four limits,
  with defaults that a zero or negative value takes:
  `MaxCommitListings` (`DefaultMaxCommitListings`, 8 traversals of a scope's commit objects; 1 is rejected),
  `MaxListPages` (`DefaultMaxListPages`, 10000 pages per traversal),
  `MaxObjects` (`DefaultMaxObjects`, 65536 keys listed and objects opened per `Observe`)
  and `MaxSourceBytes` (`DefaultMaxSourceBytes`, 256 MiB of state an `Observe` holds and its observation keeps,
  charged from the decoded structures;
  one `Open`'s transient buffers, the workspace of `ActiveView` and the adapters' allocations excepted).
  A limit exceeded fails the `Observe` with an error wrapping `ErrReadLimit`.
- The adapters' contracts, stated in their Godoc:
  `ObjectStore` and `Catalog` are safe for concurrent use,
  and every value they return is the source's from the moment the call returns;
  a complete listing traversal returns, in strictly ascending byte order,
  every key that exists under its prefix throughout the traversal;
  an `Object`'s reads are bounded by the store's own deadline and may run concurrently, and its `Close` never waits on remote I/O;
  `Catalog.ConfirmUnindexed` answers false when it cannot tell whether the scope stayed not indexed.
  The source's guarantees hold while every hour observed stays retained until the lookup's last use of the observation,
  which the caller ensures.
  The types: `ObjectStore`, `ObjectPage`, `ObjectInfo`, `Object`, `Catalog`, `CatalogSnapshot`, `CatalogScope`, `CatalogPack`,
  `ExcludedPack` and `StoreSourceOptions`.
- `SegmentKey`, `ArchiveKey` and `CommitKey` build the object keys of the storage specification §3 under a bucket prefix used byte for byte,
  the UUIDs in canonical lowercase,
  and `EscapeToolID` spells a tool_id as a key does: every byte outside the RFC 3986 unreserved set is written `%XX` in uppercase hex,
  so each tool has one spelling and none holds `/`.
  A tool "", "." or "..", a `seq_first` above 2^63−1 and an hour outside the int64 nanosecond range are errors wrapping `ErrInvalidKey`.
- `Reader.Stats` returns the F-5 statistics of a pack whose footer the Reader uses,
  as a fresh `PackStats` with `SeqRange` and `EpochStats` entries;
  false when the footer is not used.
  `AddPackEvidence` folds one pack's statistics into a `CaptureEvidence`, or marks it partial for a pack without them,
  so that the same packs folded in any order, any of them repeated, give the same evidence.
- `SourceScope.Conflicted` reports a scope whose view is conflicted,
  and `SourceScope.Removed` one whose hour is removed by retention (spec v2.25);
  `TxGapConflicted` reports such a scope in a lookup, and `TxGapScopeBreach` a version a scope read yielded outside the scope's hour.
- `NewStoreSource` has an acquisition model that recomputes, from the store's and the catalog's logs of what they returned and when,
  the view each scope may take, the evidence, the barriers and the excluded packs;
  a property test checks 1000 generated schedules against it (300 under `-short`),
  with late commit objects and segments, deletions, index changes, evidence and barriers,
  and requires each lookup over the source to equal the lookup over a replay of the model's observation.
  The schedules must give successful observations with a listed hour, a late commit object and a late segment at fixed shares,
  and the check must reject a source that drops a committed patch, returns an empty view,
  takes one commit traversal or accepts a refused confirmation.
  The fuzz target `FuzzStoreSource` mutates the schedules' keys, pages and packs against the same model, and `FuzzKeys` the key parsers.
- `Merge` has property tests over hand-written vectors and 64 generated scopes,
  which recompute F-2, F-3 and F-5 from each archive's records independently of the Writer,
  and check the record union, the size and greedy-coalescing rules and the report's counts;
  and the fuzz targets `FuzzMerge` and `FuzzMergeGenerated`.
- `SegmentWriter` writes one capture, in capture order, as segments through a `SegmentSink` (spec v2.23 and v2.24).
  `NewSegmentWriter` checks `SegmentWriterOptions` and the capture's descriptor before any sink call, then writes the capture's `start` boundary at `CaptureOrigin`;
  `Close` writes its `stop` boundary at `Now()` and commits the last segment.
  `AppendFrame` records a captured HSMS frame, its kind, `field_validity` and fidelity derived from the frame and the capture method,
  `mono_ns` from the monotonic readings of its time and `CaptureOrigin` for a `capture-clock` capture,
  and its decode status from the `Classifier` when one is set;
  `AppendEvent` records a transport event, and `Append` a record the producer built, such as a log converter's.
  `CaptureID` returns the capture's generated `capture_id` from construction on, for the next capture's `PreviousCaptureID`.
  A segment's period is the `FlushInterval` holding its first record, `DefaultFlushInterval` (5 minutes) unless set, which must divide one hour.
  The open segment is committed before a record of another hour or at or after its period's end,
  once it handed `MaxSegmentBytes` to the sink (`DefaultMaxSegmentBytes`, 64 MiB unless set; a soft limit),
  on `Tick` at or after its period's end, and on `Rotate`.
  For a `capture-clock` capture it detects clock steps and carries the clock anchor from one segment to the next.
  Each method that can touch a segment takes a `ctx`, checked before the first sink call, between sink calls and before an append reports success.
- A record the `SegmentWriter` refuses is checked before it touches a segment, and the writer stays usable:
  a nil record or event, a time that cannot be represented, an `AppendFrame` for a `log` or unknown capture method,
  a capture boundary of kind `start`, `stop` or `stop-unclean`,
  or a transport-event or annotation record whose payload does not decode or whose `field_validity` is not 0,
  so `Verify` finds no writer defect in an appended record, all with the new `ErrInvalidRecord`;
  a payload too large, a `field_validity` the payload contradicts, a record the segments' metadata cannot describe, no seq left,
  or an event `AppendEvent` cannot encode.
  Any later error fails the writer and aborts the open segment exactly once;
  segments committed before it stay published, those the failing call committed included.
  the error of a `Commit` that failed after the segment became visible wraps the new `ErrPublishUncertain`.
  The seq counter is never rewound, so lost records leave a seq gap.
- `CaptureDescriptor`, the pack metadata the producer of a capture owns, written unchanged into every segment,
  and `CaptureDescriptor.Validate`, which checks the producer's "Required when" rules and the values the encoding allows, returning a `*FieldError`.
  `DefaultClockStepTolerance` (1 s) is the tolerance a zero `ClockStepTolerance` takes.
  Every `PackMeta` field has Godoc from the tag registry.
- `Classifier`, the interface a `SegmentWriter` classifies frames with and takes the `classifier` and `max_frame_len` tags from,
  and `classify.New(maxFrameLen)`, which returns the go-secs-backed classifier as one, comparing frame lengths with its ceiling as `uint64` values.
- `SegmentSink`, `SegmentFile` and `SegmentInfo`, the sink a `SegmentWriter` writes through:
  `SegmentInfo` holds what the segment's key needs, and nothing of a segment is visible before its `Commit`.
- `NewDirSink(root, prefix)` stores each segment as the file `<root>/<SegmentKey(prefix, …)>`, so a local recording is laid out as a bucket.
  `root` must exist.
  The sink opens `root` as an `os.Root` and makes every file-system call through it,
  so a symbolic link leading out of `root`, `.partial` included, fails the call that meets it;
  it keeps `root` open while it is reachable, and has nothing to close.
  It refuses a symbolic link inside `root` where it manages directories, so no link joins `.partial/` and a key area:
  at `.partial` when the sink and each segment are created, and at every directory of a key before the rename, nothing published;
  these checks guard against a misconfigured `root`, not against a process writing to `root` while the sink runs.
  `NewDirSink` fails on `js`, where `os.Root` cannot exclude a link replaced after its check.
  A segment is written under `<root>/.partial/`, which no listing of the key areas reaches,
  then synced, renamed to its key, and the directories from the key's up to `root` synced, except on Windows;
  an error after the rename wraps `ErrPublishUncertain`.
  Files are created with mode 0640 and directories with mode 0750, less the umask.
  A prefix that is neither empty nor a relative `/`-separated path, or that holds an empty, `.` or `..` component, a `\` or a `:`,
  or whose first component is `.partial`, is refused before any file is created;
  once `.partial/` exists, so is a prefix whose first component names that same directory, as a case variant does on a case-folding file system.
- `NewReaderSource(readers, ReaderSourceOptions)` returns a `PackSource` over packs the caller opened, all of one tool,
  computing each scope's view once with `ActiveView`, every generation and patch given taken as committed.
  Without `ReaderSourceOptions.Complete`, its scopes are not indexed and every capture's evidence is partial, so a lookup is never `TxUnmatched`.
  `Complete` asserts that the readers are every accepted pack of the tool,
  and that they and `ReaderSourceOptions.Evidence`, the packs whose registration was rejected, are every pack a catalog of the tool would hold evidence of;
  the scope of each `Evidence` reader is then reported not indexed, and `Evidence` readers never enter a view.
  An `Evidence` reader whose period is empty or not inside one UTC hour is refused with `ErrInvalidQuery`.
- The retention boundary of the storage specification §5 (spec v2.25):
  a `Retention` provider's `RetainedFrom` returns the first UTC hour still retained, numbered as `HourOf` numbers it,
  and every hour before it is removed.
  `Query.Retention` gives one to `Iterate` and `MergeIterate`, `TxOptions.Retention` to `FindTransaction`,
  and `StoreSourceOptions.Retention` to `NewStoreSource`; nil removes nothing.
  A provider never falls behind the component deleting removed hours: it reports an hour removed before any object of it is deleted, and may run ahead.
  It never moves the boundary back, is safe for concurrent use, and is cheap, since a read asks it at every block it emits.
  Deleting a removed hour's objects is the storage service's work, outside this module.
- A read covers the scope hour of each pack but an extract, whatever its records' timestamps,
  and for an extract the hours of its indexed blocks' F-2 `ts_min` and `ts_max`
  and of every record it decodes, filtered out or not.
  `Iterate` and `MergeIterate` ask the provider before they read any block, before the records of each block reach the callback,
  after a failed read, and before they return success.
  A covered hour found removed ends the whole read with an error wrapping the new `ErrRemoved`,
  joined after the read error when it follows one,
  beside the `Result` found so far, `Result.Removed` listing the removed hours the read covers:
  everything the read reported of those hours, records passed to the callback included, is superseded, and the caller discards it.
  A callback's error, a `ctx` error and a limit of the read itself
  (a reservation over `MergeIterateOptions.MaxHeldBytes`, one conflict over `MaxConflicts`)
  end a read as before, with no check after them;
  a failed `ReadAt` is checked whatever its error wraps, `ErrReadLimit` included.
- `FindTransaction` checks the primary's hour before `Observe`,
  after a failed `Observe` whose error is neither a `ctx` error nor `ErrReadLimit`, before each scope it schedules,
  and before it returns an outcome, and gives the provider to every scope read.
  A removed primary hour, or a scope read that ends with `ErrRemoved`, ends the lookup with an error wrapping `ErrRemoved`,
  its `TxResult` keeping the diagnostics found, with no outcome;
  an earlier error of the lookup, `ErrNotPrimary` included, stands.
- `NewStoreSource`'s `Observe` asks the provider before every list page, before every object it opens,
  before each listed scope's view is computed, and once after every scope is fixed.
  A scope whose hour is removed is reported `SourceScope.Removed`, with no readers:
  its objects are closed and their charges released at once, and nothing more is listed or opened for it.
  An object gone from the store whose hour is known and removed marks its scope removed instead of failing `Observe`,
  though an error closing it still fails `Observe`;
  a staging segment gone before its head was read keeps the retriable `ErrObjectNotFound`
  while a scope the catalog does not index is retained.
  `Observe` never fails with `ErrRemoved`, and promises nothing about the boundary after it returns:
  give `StoreSourceOptions.Retention` and `TxOptions.Retention` the same provider,
  since the source's alone leaves the lookup unprotected after `Observe` returns.
- `HourOf` returns the UTC hour of a time as `TxKey.Hour` and the key builders number it,
  and `TxKeyOf` builds the `TxKey` of a primary from its capture, seq and time.
- `ExportJSONL(ctx, r, w)` writes the canonical JSONL export of the pack a `Reader` reads,
  schema `tracepack-jsonl/1` of the tracepack JSONL specification (spec v2.26):
  a header line from the file header and the pack metadata as stored,
  then one line per record that `Iterate` yields with `Query{Payloads: true}`, in the same order,
  each record header field as stored, reserved bits and unknown enum values included,
  with the block's `record_header_len`, the extension area and the payload,
  and for a transport-event or annotation record whose payload is a valid TLV body, the body by name.
  64-bit values are decimal strings, bytes are base64, enum values and bits are named, and unknown entries are kept opaque.
  It returns the `Result` that `Iterate` call returns:
  a damaged pack exports the records of its validated blocks, and nothing marks the place of a failed block, which `Result.Incomplete` lists.
  A reader budget that keeps part of the pack out fails the export with an error wrapping `ErrReadLimit` instead of a `ReasonLimit` defect,
  before anything is written for the footer and walk budgets,
  so an export that succeeds does not depend on `ReaderOptions`.
  Each line is written in pieces of at most 64 KiB, and the export's own memory does not grow with a payload, a value or an entry list.
  Tests check its lines against bytes written by hand from the specification, its records and `Result` against `Iterate`'s over random, truncated and damaged packs,
  and its body validity against `Verify`'s; the fuzz targets are `FuzzExportJSONL`, `FuzzJSONLPackMetadata` and `FuzzJSONLValidBody`.
- `WriterOptions.Now`, `RepairOptions.Now` and `MergeOptions.Now` (spec v2.27), the clock of the pack each writes:
  it stamps the file header's `writer_start_utc_ns` and gives the time part of every `pack_id`, `capture_id` or `replacement_set_id` generated as a UUIDv7;
  nil means `time.Now`, and a supplied id is kept.
  `SegmentWriterOptions.Now` now also reaches each segment's `Writer`, so it stamps every segment's `writer_start_utc_ns`,
  and gives the time part of the `capture_id` and of every segment `pack_id` the `SegmentWriter` generates.
  With a fixed clock and supplied ids, a test writes a pack whose file header is the same on every run.
- The conformance corpus in `testdata/corpus/`, shipped in the module zip (spec v2.27 and v2.28).
  Each vector is a pack with what a conforming implementation produces from it:
  its JSONL export and verification report, or its bootstrap rejection,
  and where the vector needs them, query results, HSMS header fields, footer values, classifications, a repair result or a truncation table.
  It holds the vectors of the format specification §16, except the redaction vectors and two that need the result of a read over several packs,
  and the single-pack vectors of the semantics specification §9.
  The tracepack corpus specification defines its layout and the canonical JSON schemas of its files,
  schema `tracepack-corpus/1`, a draft until tracepack v1.0.0: a change to a schema before then takes a new value.
  Its README says how an implementation in any language checks itself against the committed files.
  `TestCorpus` regenerates the corpus and requires every file, and every read of the committed files, to match;
  the `classify` classifier and the validating `Writer` are checked against it too.
- Runnable examples: `ExampleSegmentWriter`, `ExampleOpen`, `ExampleFindTransaction` over `NewReaderSource`, `ExampleMergeIterate` and `ExampleExportJSONL`;
  and `README.md`, the API by audience.
- `SegmentWriter` and `NewDirSink` have tests that inject a failure or a cancellation at each sink and file-system call, before and after publication,
  and count the commits and aborts; an end-to-end test records a capture into a directory and finds a reply across an hour over `NewReaderSource`,
  then merges each hour's segments.
- `Record.HSMSHeader` and `HSMSHeader`, the HSMS header fields of a payload with their availability kept separate;
  `Record.SetCapturedFieldValidity`.
- `DecodeStatus.Clean`, true only for a known classification outside the malformed set, so an unknown value is neither clean nor malformed.
- Sentinel errors `ErrNotTracepack`, `ErrUnsupportedFormat`, `ErrChecksum`, `ErrReadLimit`, `ErrInvalidFooter`, `ErrInvalidQuery`,
  `ErrFieldValidity`, `ErrFieldValue`, `ErrRepairNotNeeded`, `ErrNotRepairable`,
  `ErrViewConflicted`, `ErrMergeInput`, `ErrMergeConflict`, `ErrMergeLimit`, `ErrNotPrimary`, `ErrObjectNotFound`, `ErrInvalidKey`,
  `ErrScopeBreach`, `ErrInvalidRecord`, `ErrPublishUncertain` and `ErrRemoved`.

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
