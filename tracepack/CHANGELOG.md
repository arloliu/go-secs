# Changelog

All notable changes to the `github.com/arloliu/go-secs/tracepack` module are documented here.
Releases are tagged `tracepack/vX.Y.Z` on `main`, independently of go-secs `vX.Y.Z` tags.

## [Unreleased]

### Added

- `MergeOptions.MaxParallelOpens`, with `DefaultMaxParallelOpens` (16), bounds how many inputs `Merge` opens at once (G5-278);
  1 opens them one after another, as earlier versions did.
- `DefaultMaxOpenBlocks` (64) names the `MergeOptions.MaxOpenBlocks` that zero selects (G5-283); the default is unchanged.
- `Item.HSMSHeader` is `Record.HSMSHeader()` of the record as stored, set whatever `Query.Payloads` (G5-289):
  a read without payloads, filtered on S/F, SessionID or System Bytes or not, now sees the fields it selected on.
  `Item.Unobserved` and `TxRecord.Unobserved` are `Reader.Unobserved` of the pack the record or version was yielded from,
  the representative's under `MergeIterate`, `FindTransaction` and `FindTransactions`.
  `Item.Clone()` returns a copy sharing no mutable storage with the read: the payload, the header extension and the Marking with its entries and defects,
  the defects' errors shared; `Record.Clone()` copies the payload.
  An `Item` is 8 bytes larger on 64-bit platforms and 12 on 32-bit ones;
  what `FindTransactions` charges `MaxCachedBytes` per cached record and `FindTransaction` charges `MaxStateBytes` per kept version is unchanged.
  `Reader.Iterate` and `MergeIterate` take about 7 ns more per record yielded.
  For tracekeep: the hand copy of an `Item` in `restore.go` becomes `it.Clone()`,
  and `r.Unobserved(&it.Record)` can become `it.Unobserved`.
- `Quality.HasAll`, `FieldValidity.HasAll` and `TxClass.HasAll` report whether every bit of the argument is set (G5-287);
  `HasAll(0)` is true, and `HasAny(0)` false.
- `ErrRefused` marks an error that a writing call's own arguments, options or records determine (G5-286),
  so the same call with the same input fails the same way:
  `NewWriter`, `Writer.Append`, `Flush` and `Close`, `WriteClose`, `NewSegmentWriter`,
  and `SegmentWriter`'s `Append`, `AppendFrame`, `AppendEvent`, `Tick`, `Rotate` and `Close` add it
  to an error wrapping `ErrSeqOrder`, `ErrPayloadTooLarge`, `ErrFieldValidity`, `ErrMetadataCommitment`, `ErrValidation`, `ErrScopeBreach`,
  `ErrInvalidRecord`, `ErrRequiredTag`, `ErrFieldValue`, `ErrFieldLength`, `ErrAnnotationText` or `ErrReservedTag`,
  and to an invalid argument or option.
  It is never added to an error of `Merge`, of `ctx`, of the writer's prior state (`ErrWriterFailed`, `ErrClosed`),
  or of the destination, a `Syncer`, a sink, a segment file or the random source;
  an error from the caller's own implementation is returned as it is.
  The error text is unchanged.
  `SegmentWriter.Close` on a failed writer still returns the stored failure, and a repeated `Close` its first result, now without the marker the failing call's error carried.
  For tracekeep: the sentinel list in `internal/ingest/flush.go` can become `errors.Is(err, tracepack.ErrRefused)`;
  `ErrMergeLimit` and `ErrReadLimit` in that list are unreachable from its flush,
  and it keeps checking `ErrReadLimit` where it reopens the working file.
- The package documentation and the README state the v1 compatibility promise in a Compatibility section (G5-280).
  Guaranteed: the exported identifiers and signatures, which sentinel an error wraps (`ErrRefused` included),
  the documented outcomes, orders and determinism and the byte-identity promises of `WriteClose`, `ExportJSONL` and `redact`'s `Mask`,
  reading every format 1.x pack, the JSONL schema values `tracepack-jsonl/1` to `/3`,
  and the CLI's flags, exit codes, stdout/stderr split and `dump --jsonl` bytes.
  Not guaranteed: error text, ctx-check cadence, read-ahead depth, goroutine and `ReadAt` counts, write piece sizes, buffer-reuse ratios,
  default budget values, zstd output bytes across library versions, and the text of the `verify` and `find` reports, excluded for good.
  Every enum may gain values in a format minor, so a `switch` needs a default.
  The section also states the bound of a pack's redaction entries, which stay in the pack metadata (G5-279):
  the readers' `ReaderOptions.MaxPackMetadataLen`, 64 MiB by default, a limit in bytes.
  The Godoc that gave such implementation numbers as contract (`MergeIterate`'s read-ahead depth, ctx cadence and buffer-reuse bound,
  `FindTransaction`'s and `Merge`'s ctx cadence, `FindTransactions`' goroutine count, `ExportJSONL`'s write pieces, `Open`'s `ReadAt` count, `PackError.Error`'s text)
  now says what the implementation currently does; no behavior changed.

### Changed

- **Breaking:** `redact.Keys` and `(*redact.Compiled).Mask` take a `context.Context` first (G5-281):
  `DigestKey(ctx, keyID)`, `EncryptionKey(ctx, capture, keyID)` and `Mask(ctx, capture, r, unobserved, keys)`.
  `Mask` checks `ctx` before each key request, returning `ctx.Err()` as is, and passes it to `Keys`; nothing else it does blocks.
  A key error is wrapped as before, and `Restore` is unchanged.
  Migration: an implementation of `redact.Keys` adds a leading `ctx context.Context` to both methods and may bound its key request with it;
  a caller of `Mask` passes its context first.
- **Breaking:** `BusLayout1` is the untyped string `"1"`, the `Tpk-Layout` value itself, instead of the integer 1 (G5-283).
  Migration: set the `Tpk-Layout` header to `bus.Layout1` instead of `strconv.Itoa(tracepack.BusLayout1)`,
  and compare a received layout with it directly;
  the constant moves to the subpackage `tracepack/bus` with the rest of the bus codec (G5-288, below).
- **Breaking:** `classify.Name` is a function, `classify.Name() string`, instead of a variable; it returns the same value (G5-283).
  Migration: write `classify.Name()` where `classify.Name` was read.
- **Breaking:** `classify.Frame` takes its frame-length limit as a `uint64`, as `classify.New` and `Classifier.MaxFrameLen` do (G5-283).
  0 still means no limit, and a limit above the largest `int` makes no frame oversized.
  Migration: pass the pack's `max_frame_len` as it is, without converting it to `int`.
- **Breaking:** the capture field of `TxKey`, `TxRange` and `Boundary` is `CaptureID`,
  as `PackHeader.CaptureID` and `CloseOptions.CaptureID` are (G5-283);
  `TxKeyOf`'s first parameter is named `captureID` to match.
  Migration: write `CaptureID` where `Capture` named the field,
  in composite literals (`TxKey{CaptureID: id, …}`) and field reads (`d.Boundary.CaptureID`).
- **Breaking:** every `IncompleteReason` value takes its type's name as a prefix,
  and `Outcome` becomes `VerifyOutcome`, with its values (G5-283).
  `VerifyReport.Outcome` keeps its field name, now of type `VerifyOutcome`.
  The `String()` values are unchanged,
  so the JSONL export, the `tracepack` CLI's output and the corpus are byte for byte the same.
  Migration: rename as follows;
  tracekeep uses `ReasonCoverage`, `ReasonCrashGap`, `OutcomeFinalizedConsistent`, `OutcomeFinalizedInconsistent` and `OutcomeUnfinalized`,
  and veq uses `OutcomeFinalizedConsistent`.

  | Before | After |
  |---|---|
  | `ReasonCorruptBlock` | `IncompleteReasonCorruptBlock` |
  | `ReasonUnknownCodec` | `IncompleteReasonUnknownCodec` |
  | `ReasonTruncated` | `IncompleteReasonTruncated` |
  | `ReasonCoverage` | `IncompleteReasonCoverage` |
  | `ReasonLimit` | `IncompleteReasonLimit` |
  | `ReasonIndexMismatch` | `IncompleteReasonIndexMismatch` |
  | `ReasonCrashGap` | `IncompleteReasonCrashGap` |
  | `Outcome` | `VerifyOutcome` |
  | `OutcomeFinalizedConsistent` | `VerifyOutcomeFinalizedConsistent` |
  | `OutcomeFinalizedInconsistent` | `VerifyOutcomeFinalizedInconsistent` |
  | `OutcomeFinalizedTruncated` | `VerifyOutcomeFinalizedTruncated` |
  | `OutcomeUnfinalized` | `VerifyOutcomeUnfinalized` |
  | `OutcomeCorruptMiddle` | `VerifyOutcomeCorruptMiddle` |

- **Breaking:** `NewDirSink` takes a `DirSinkOptions` struct, `NewDirSink(root, prefix string, opts DirSinkOptions)`,
  in place of the variadic `DirSinkOption`;
  `DirSinkOptions.NoReplace` replaces `DirSinkNoReplace()`, which is removed with `DirSinkOption` (G5-283).
  Migration: veq's `NewDirSink(root, prefix, tracepack.DirSinkNoReplace())` becomes `NewDirSink(root, prefix, tracepack.DirSinkOptions{NoReplace: true})`,
  and tracekeep's `NewDirSink(root, prefix)` becomes `NewDirSink(root, prefix, tracepack.DirSinkOptions{})`.
- **Breaking:** a negative budget field of `ReaderOptions`, `MergeIterateOptions`, `TxOptions` or `TxRangeOptions` is refused instead of taking its default (G5-284);
  zero still takes the default, as `MergeOptions` already did.
  `Open`, `Verify` and `Merge` (for `MergeOptions.Reader`) return a plain error naming the field before any read,
  and `MergeIterate`, `FindTransaction` and `FindTransactions` an error wrapping `ErrInvalidQuery`, before any read or call of the source;
  an earlier argument error still wins.
  Migration: pass 0, not a negative value, for a default; neither tracekeep nor veq passes a negative option.
- **Breaking:** `SegmentWriter.AppendFrame` takes the producer's quality bits as a last parameter,
  `AppendFrame(ctx, at, dir, epoch, frame, quality)` (G5-285).
  They go through the record path as `Append` treats `Record.Quality`:
  undefined bits are dropped, capture-boundary and the epoch-0 correlation-incomplete are the writer's,
  and unobserved and redacted are checked as for `Append`, with `ErrInvalidRecord` or `ErrMetadataCommitment`; no new check is added.
  0 writes the record earlier versions wrote.
  Migration: append `, 0` to every `AppendFrame` call, such as tracekeep's in `internal/sim/replay_test.go`;
  veq does not call it.
- **Breaking:** Go names that read as something else at a call site are renamed (G5-287).
  `String()` values, JSONL names, the corpus and every other error text are unchanged.
  `Item.Pack`, the index of the record's reader, becomes `Item.Input`,
  so it no longer shares a name with `TxRecord.Pack`, a pack_id;
  `Defect.Pack` and `PackError.Pack` keep their names.
  `MergeOptions.Writer`, the archive's writer tag, becomes `MergeOptions.WriterTag`,
  and the error for an empty one reads `MergeOptions.WriterTag is empty`.
  `Quality.Has`, `FieldValidity.Has` and `TxClass.Has`, which report whether any bit of the argument is set, become `HasAny`, with the same result.
  The transaction lookup names its unit as the hour it is:
  `TxOptions.MaxScopes` becomes `MaxHours`, `DefaultTxMaxScopes` becomes `DefaultTxMaxHours`,
  and `TxScope`, the type of `TxResult.Searched`, becomes `TxHour`;
  the error refusing a negative `MaxHours` names the field by its new name.
  Migration: rename as listed;
  tracekeep writes `WriterTag:` in the `MergeOptions` of `internal/compact/merge.go`
  and `Quality.HasAny` wherever it called `Quality.Has`;
  veq uses none of these names.
- **Breaking:** the bus codec moves out of the root package into the subpackage `github.com/arloliu/go-secs/tracepack/bus` (G5-288),
  and its names drop their `Bus` prefix.
  Behavior and every error text are unchanged, the `tracepack: ` prefix included;
  the root package keeps the descriptor codec, `EncodeDescriptor` and `DecodeDescriptor`.
  Migration: import `github.com/arloliu/go-secs/tracepack/bus` and rename as follows.
  tracekeep uses every name in the table except `ParseBusToolToken`:
  in `producer` (`records.go`, `lifecycle.go`, `publisher.go`, `preflight.go`, `config.go`, `descriptor.go`, `bus.go`, `producertest/bus.go`, `producertest/chaos.go`),
  `internal/ingest` (`deliver.go`, `ingester.go`, `raw.go`), `internal/admin/status.go` and `internal/compact/retention.go`, and their tests;
  veq uses none of them.

  | Before | After |
  |---|---|
  | `tracepack.BusLayout1` | `bus.Layout1` |
  | `tracepack.BusHeaderLayout` | `bus.HeaderLayout` |
  | `tracepack.BusHeaderMsgID` | `bus.HeaderMsgID` |
  | `tracepack.BusHeaderDescriptor` | `bus.HeaderDescriptor` |
  | `tracepack.BusHeaderPayloadRef` | `bus.HeaderPayloadRef` |
  | `tracepack.BusHeaderPayloadDigest` | `bus.HeaderPayloadDigest` |
  | `tracepack.BusHeaderBlockLimit` | `bus.HeaderBlockLimit` |
  | `tracepack.BusMinMaxPayload` | `bus.MinMaxPayload` |
  | `tracepack.ErrBusMalformed` | `bus.ErrMalformed` |
  | `tracepack.ErrBusNotAccepted` | `bus.ErrNotAccepted` |
  | `tracepack.ErrBusPayloadMismatch` | `bus.ErrPayloadMismatch` |
  | `tracepack.BusEnvelope` | `bus.Envelope` |
  | `tracepack.BusRecord` | `bus.Record` |
  | `tracepack.BusToolToken` | `bus.ToolToken` |
  | `tracepack.ParseBusToolToken` | `bus.ParseToolToken` |
  | `tracepack.BusSubject` | `bus.Subject` |
  | `tracepack.BusMsgID` | `bus.MsgID` |
  | `tracepack.ParseBusMsgID` | `bus.ParseMsgID` |
  | `tracepack.NewBusRecord` | `bus.NewRecord` |
  | `tracepack.EncodeBusBody` | `bus.EncodeBody` |
  | `tracepack.CheckBusMessage` | `bus.CheckMessage` |
  | `tracepack.BusPayloadDigest` | `bus.PayloadDigest` |
  | `tracepack.CheckBusPayload` | `bus.CheckPayload` |
  | `tracepack.BusInline` | `bus.Inline` |
  | `tracepack.CheckBusStart` | `bus.CheckStart` |
  | `tracepack.DeriveBusRecord` | `bus.DeriveRecord` |

- A `ReadAt` that panics in one of `Open`'s concurrent bootstrap reads makes `Open` panic on its caller with that value, once the round's calls returned;
  it used to end the process from a goroutine of its own.

### Removed

- **Breaking:** `PackInfo`, its methods, `NewPackInfo`, `NewPackInfoMinor` and `Reader.Info` are removed (G5-282).
  Migration: read `PackID`, `CaptureID` and `Meta` from `Reader.Header()`, which decodes a fresh `PackMeta` on every call;
  decode stored pack metadata with `UnmarshalPackMeta`, or `UnmarshalPackMetaMinor` when the pack's format minor is known.
- **Breaking:** `Result.FooterErr` is removed, `FooterErrs` being the one shape (G5-282).
  Migration: for `Reader.Iterate` and `ExportJSONL`, read `FooterErrs[0].Err` when `FooterErrs` is not empty (it holds at most one entry, for pack 0),
  or `Reader.Header().FooterErr`, which is unchanged.
  An hour that `FindTransaction` and `FindTransactions` cache charges `MaxCachedBytes` 16 bytes less, a `Result` being one field smaller.
- **Breaking:** `RecordFlags` and `RecordFlagsMonoPresent` are unexported, since no exported field or signature carries them (G5-282).
  Migration: read `Record.MonoPresent`.

### Performance

- `Merge` opens its inputs concurrently and checks them in input order, so its archive, report, conflicts and error are those of opening them one after another,
  while ctx is live and each input's `ReaderAt` answers independently of the others'; inputs after a failing one may have been read.
  The busy hour's 12 segments, each read through a 10 ms `ReadAt`, merge in 19.6 ms instead of 132 ms (−85 %);
  in memory, an idle hour's merge takes about 3 % less time and a busy hour's the same (GOMAXPROCS 1).

## [0.11.0] - 2026-10-30

This release keeps spec v2.38 and format 1.2.
It adds `CompressionLevel`;
at the zero-value options it writes the bytes tracepack/v0.10.0 wrote and returns the same results and errors, faster and with less allocated.

### Added

- `CompressionLevel` on `WriterOptions`, `SegmentWriterOptions` and `MergeOptions` selects the zstd level of the block bodies and the footer a writer encodes (G5-276):
  `CompressionDefault`, the zero value, writes what every earlier version wrote;
  `CompressionFastest` writes a busy hour in about 74 % of the default's time (Merge 79 %) for 1.3 % larger output;
  `CompressionBest` writes 6.2 % smaller output in about 2.5 to 2.9 times the time.
  Merge applies it to the blocks it resolves or coalesces and keeps the bytes of a block it copies verbatim; `WriteClose` stays uncompressed.
  An undefined value fails `NewWriter`, `NewSegmentWriter` and `Merge` before any I/O, whether or not `Uncompressed` is set.

### Changed

- `MergeIterate`, and the reads of `FindTransaction` and `FindTransactions`, reuse the buffers of held blocks across reads (G5-277).
  `MaxHeldBytes` still reserves each block's exact sizes and decides admission on them alone, so a read fails with `ErrReadLimit` exactly where it did;
  a reused slice serves a block only when it is at most an eighth larger than the block needs,
  so the buffers of the held blocks may take up to 9/8 of `MaxHeldBytes`.

### Performance

No written byte, result or error changes at the zero-value options; figures are at GOMAXPROCS 1 on the benchmark machine, against tracepack/v0.10.0.

- `FindTransaction` takes about 13 % less time and allocates 23 KB instead of 5.2 MB:
  held blocks reuse their buffers, and a compressed block within the bytes a Reader retains is decoded from them in place instead of copied first.
  `MergeIterate` over a day of busy hours allocates 29 MB instead of 80 MB and takes about 4 % less time.
- `Iterate` over a busy hour takes about 9 % less time: each `Item` is built in place, and on amd64 hosts with AVX-512 F and BW
  the header section is transposed by assembly kernels, about three times as fast as the pure-Go code, which every other host and the `purego` build tag use.
  The Writer and `Merge` take about 2 % less time from the same kernels.

## [0.10.0] - 2026-10-27

This release keeps spec v2.38, format 1.2 and the public API.
It changes no written byte, result or error; it only makes reads, merges and writes faster and allocate less.

### Performance

Figures are at GOMAXPROCS 1 on the benchmark machine.

- A block read parses each record header once it is checked, and gathers the header section eight records at a time:
  `Iterate` over a busy hour (577 KB, zstd) takes about 21 % less time, a `FindTransaction` about 17 % less, and `Merge` of an hour's segments about 17 % less.
- `FindTransactions` passes each lookup the cached records of an hour without copying them and tests their hour against bounds computed once:
  about 13 % less time on a busy hour (6,202 primaries) and 17 % less when primaries share their keys heavily.
- `Iterate`, `ExportJSONL` and `Verify` reuse their block buffer across calls, `Merge` reuses its buffers across merges,
  and a closed `Writer` hands its buffers to the next one, which also runs about 12 % faster:
  a busy hour allocates 0.58 MB instead of 3.2 MB to iterate, 1 MB instead of 21 MB to merge, and 0.13 MB instead of 14 MB to write.
  A buffer kept for reuse lives until the garbage collector drops it, and a buffer over 16 MiB (64 MiB for a merge) is not kept.
  `MergeIterate` and `FindTransaction` keep allocating each held block's buffers at its exact sizes, so `MaxHeldBytes` bounds what they hold, as before.

## [0.9.0] - 2026-10-26

This release implements spec v2.38 and keeps format 1.2, every pack and its canonical export unchanged.

### Upgrade notes

- A capture close that tracepack v0.8.0 or earlier began writes other bytes when this version retries it:
  those versions wrote the close segment with zstd by default, and this one always writes it uncompressed.
  Before upgrading a log service, finish every close it has recorded but not yet committed with the version that began it,
  or keep the bytes of its first attempt and write those on a retry, so that two closes of one capture never conflict.

### Added

- `FindTransactions` pairs every primary of one capture in a time range with its reply, reading each hour once (G5-271, G5-273).
  Each `TxPairing` holds the primary's key and the `TxResult` that `FindTransaction` returns for it over the same packs,
  passed to a callback in ascending hour, then seq, as the caller's own copy.
  It holds at most `MaxScopes` hours, bounded by `TxRangeOptions.MaxCachedBytes` (default 256 MiB, `DefaultTxMaxCachedBytes`).
  It runs an hour's lookups on up to `GOMAXPROCS` goroutines, none when that is 1 or less, and calls the callback from the calling goroutine (G5-275);
  up to that many lookups' `MaxStateBytes` are charged at once.
  On the busy benchmark hour, its 6,202 primaries pair in about 0.25 s at `GOMAXPROCS` 16 and 2.2 s at 1, with 60 MB allocated,
  against about 36 s and 37 GB by one `FindTransaction` each;
  each item's HSMS header is decoded once when its hour is read, not again by every lookup replaying it.

### Changed

- **Breaking:** `WriteClose` always writes the close segment's block and footer with `CodecNone`, and `CloseOptions.Uncompressed` is removed (G5-272).
  Equal options with a fixed `PackID` and `Now` now write equal bytes whichever tracepack version, from this one on, runs a retried close,
  since no compressor takes part; a close begun by v0.8.0 or earlier is the exception (see Upgrade notes).
  A caller that set `Uncompressed` drops the field; one that left it false gets an uncompressed close segment instead of a zstd one.

## [0.8.0] - 2026-10-25

This release implements spec v2.37, format 1.2, and keeps every format 1.0 and 1.1 pack and its canonical export unchanged.
Format 1.2 adds the pack metadata tag `recorder_instance_name`, the human-readable name of the deployment `recorder_instance_id` identifies (G5-270).
tracepack never interprets it.
The conformance corpus, now `tracepack-corpus/5`, gains 2 vectors.

### Upgrade notes

- Upgrade durable-bus consumers and mergers before producers.
  A consumer of v0.7.x or earlier refuses a capture descriptor carrying `recorder_instance_name`, so the capture's `start` stays deferred on the bus;
  a merger of v0.7.x or earlier refuses a pack of format 1.2.
  A reader of v0.5.0 to v0.7.x reads such a pack, keeps the tag as an unknown entry, and exports it under `tracepack-jsonl/2`.
- `ExportJSONL` writes `tracepack-jsonl/3` for a pack of `format_minor` 2 or later, where earlier versions wrote `tracepack-jsonl/2`;
  a pack of minor 1 still exports `tracepack-jsonl/2`.

### Added

- `CaptureDescriptor.RecorderInstanceName` and `PackMeta.RecorderInstanceName` (`*string`).
  A `Writer` writes `format_minor` 2 exactly when the pack metadata carries it.
- `EncodeDescriptor` and `DecodeDescriptor` carry `recorder_instance_name` as a producer-owned value.
- `Merge` treats `recorder_instance_name` as a capture-level tag: every input carries the same value or none, and the archive carries it.
- The corpus vectors `basic-recorder-instance-name` and `basic-new-tags-segment-1-1`.

### Changed

- The corpus vector `basic-future-minor` holds a pack of minor 3 with the unassigned tag 0x0035, since format 1.2 assigns 0x0034 and defines minor 2.

## [0.7.1] - 2026-10-25

This release implements spec v2.36 and changes no library API, no byte of a pack, no canonical export and no dependency.
Spec v2.36 needs no code change:
an operator's termination of a record version covers every message of that version on a durable bus,
and such a message no longer needs its claim-check object (G5-269).
The rule binds the log service's consumer, which tracepack does not implement.

## [0.7.0] - 2026-10-25

This release implements spec v2.35 and changes no library API, no byte of a pack and no canonical export.
Spec v2.35 needs no code change:
it lets a consumer of a durable bus defer a message by moving it to a parking stream of the same bus (G5-267),
and it states that the output of `redact.Mask` is fixed byte for byte by the format within format major 1 (G5-268),
which the `redact-` corpus vectors already pin.
A later correction that changes masking output will be stated in this changelog as such.

### Changed

- The module requires go-secs v2.8.0, up from v2.6.0.

## [0.6.0] - 2026-10-20

This release implements spec v2.34, as v0.5.1, and changes no library API, no byte of a pack and no canonical export.
It adds the `tracepack` command, the reference command line for reading packs on local paths.
The command is tested on Linux, and on Windows only compiled.

### Added

- The `tracepack` command, `cmd/tracepack`, reads packs on local paths with three subcommands:
  `dump --jsonl` writes the canonical JSONL export, to stdout or, with `-o`, to a file replaced only when the export succeeds;
  `verify` reports each pack's verification outcome and evidence;
  `find` looks up the reply to a primary over the packs and directories it is given.
  It exits 0 for a clean report, 1 for a report that is not clean (an incomplete read or lookup, a `verify` outcome other than `finalized-consistent`), and 2 when it could not run.
  It adds no dependency to the module.

## [0.5.1] - 2026-10-14

This release implements spec v2.34 and changes no byte of format 1.0 or 1.1, and no canonical export.

### Upgrade notes

- `DecodeDescriptor` refuses a capture descriptor whose `hsms_timers` entry holds a nested tag other than 1 to 8 (T1 to T8),
  which v0.5.0 skipped, so `CheckBusMessage` and `CheckBusPayload` return `ErrBusNotAccepted` for a `start` carrying one.
  A producer newer than its consumers is seen while it can still be fixed, as for an unknown `thinning` nested tag.
  Readers of a stored pack and `Merge` still skip such a tag.

## [0.5.0] - 2026-10-13

This release implements spec v2.33, format 1.1, and keeps every format 1.0 pack and its canonical export unchanged.
A pack of format 1.1 can be masked at write under a redaction policy that encrypts the originals of data records in place with AES-SIV,
and can hold records unobserved by design, of the streams and functions its `thinning` entries declare.
Readers decode pack metadata under the pack's `format_minor`, report the defects of redaction entries without failing `Open`,
and give the Marking of each masked record in `Item.Redaction`.
The new package `redact` masks records, computes their digests and restores their originals.
The bus codec and the descriptor codec carry a durable-bus capture from its producer to its consumers,
and `WriteClose` writes the close of a capture its producer left open, at seq 2^63 − 1, the last seq of a capture.
`Writer.Close` returns a `Continuation`, so a pack that ends at that seq reports that no pack follows.
`Merge` merges format 1.1 packs, with the refusals of the storage specification §4.
The conformance corpus, now `tracepack-corpus/4`, gains 24 vectors.

### Upgrade notes

- `Writer.Close` returns `(Continuation, error)` instead of `(uint64, error)`.
  `Continuation.Next() (seq uint64, ok bool)` gives the seq the capture's next pack starts at,
  and `ok` is false after a pack whose last record has seq 2^63 − 1, and with the zero `Continuation` returned beside an error.
  Replace `next, err := w.Close()` with `cont, err := w.Close()` and `next, ok := cont.Next()`, and start no further pack when `ok` is false;
  a call that discards the result, `_, err := w.Close()`, compiles unchanged.
- A record carrying `quality` bit 7 is refused where v0.4.0 cleared the bit silently.
  `Writer.Append`, and so every `SegmentWriter` append, accepts the bit only as `QualityUnobserved`,
  on a data record unobserved by design of a stream and function the pack metadata's `thinning` declares.
  A record whose own fields break that rule is refused with `ErrInvalidRecord`,
  and one of a stream and function `thinning` does not declare, or in a pack without `thinning`,
  with a `*FieldError` for `thinning` wrapping `ErrMetadataCommitment`;
  clear bit 7 of a record that is not unobserved before appending it.
- `ErrInvalidRecord` now covers every writer, `Writer.Append` included, and its text is "tracepack: record refused by the writer";
  test for it with `errors.Is`, never by its text.
- `NewWriter` refuses pack metadata that breaks a writer rule of format 1.1 (the format specification §5).
  A pack that is not an extract and carries `redaction_policy` is masked at write:
  its policy must carry both `encryption_algorithm` and `encryption_key_id`, its metadata exactly one `classifier` and at most one `max_frame_len`,
  each breach refused with a `*FieldError` wrapping `ErrMetadataCommitment`.
  v0.4.0 wrote such a segment under a policy without encryption, and its merge dropped the policy.
  Such a pack is written with given seqs: `AssignSeq`, and so `DetectClockSteps`, which requires it, is refused with a plain error, no sentinel.
  An extract's policy may carry neither encryption tag, and its entries no `siv`, each refused with a `*FieldError` wrapping `ErrMetadataCommitment`.
  A `thinning` entry whose stream is above 127 or whose function is above 255 is refused with a `*FieldError` for `thinning` wrapping `ErrFieldValue`.
- Readers decode the pack metadata under the format minor of the pack's file header:
  `Open`, `Reader.Header().Meta` and `Reader.Info().Meta()`, `PackInfo` keeping the minor it was built with.
  In a pack of minor 0 a number of format 1.1 keeps its format 1.0 status:
  tag 0x0033 stays in `PackMeta.Unknown`, the nested format 1.1 tags of `redaction_policy` and `redaction` are skipped,
  and `PackMeta.MarshalBinary` of such a value returns `ErrReservedTag`, so nothing written from it gives those bytes a meaning.
  `UnmarshalPackMeta` and `NewPackInfo` decode with the newest registry, as for a pack of an unknown newer minor;
  a caller that knows the pack's minor calls `UnmarshalPackMetaMinor` or `NewPackInfoMinor`.
- `Open`, `UnmarshalPackMeta` and `UnmarshalPackMetaMinor` no longer fail on a malformed `masked_ranges`:
  a length not a multiple of 8, an odd count, or an element above 2^63 − 1.
  The value is kept in `RedactionEntry.RawMaskedRanges`, `PackHeader.RedactionDefects` reports the entry,
  and the records it names read as wholly masked.
  No redaction defect fails `Open` or a read or makes a read incomplete; `Verify` is unchanged.
- `PackHeader` is no longer comparable, since its new field `RedactionDefects` is a slice:
  code that compares `PackHeader` values with `==`, or uses one as a map key, compares the fields it needs instead.
- `Merge` refuses with `ErrMergeInput`, before it writes anything to `dst`, inputs v0.4.0 merged or that format 1.1 adds:
  an input of a `format_minor` newer than this module implements;
  a minor-0 input holding a record that carries `quality` bit 7, as its footer shows, beside an input of minor 1 or more;
  an input whose `redaction_policy` lacks either encryption tag, whatever its minor;
  a policy-bearing input with a number of `classifier` values other than one, or more than one `max_frame_len`;
  inputs carrying `redaction_policy` that differ in policy, `classifier` or `max_frame_len`, or a policy-bearing input beside one without a policy;
  inputs whose `thinning` lists differ;
  redaction entries without a policy, any bootstrap defect of the entries or of the `redaction-present` flag,
  which includes a minor-0 segment that sets `redaction-present` and holds no entry, and an entry naming a seq its input does not hold.
  Inputs without `redaction_policy` still merge with differing `classifier` and `max_frame_len` values, the archive carrying their union.
  Differing redaction entries for one seq fail with `ErrMergeConflict`, also before `dst` is touched.
  After the archive's header, `Merge` also refuses with `ErrMergeInput` a block holding a record that fails a check of Marking,
  or a record carrying `quality.unobserved` that the Writer would refuse;
  such an error leaves `dst` holding a partial, unfinalized archive, which the caller discards, as after any error of `Merge`.
- The refusal of a redacted record in an input without `redaction_policy` now reads "a redacted record without redaction_policy in the pack metadata".
- Merge packs of format 1.1 only with tracepack v0.5.0 or later (the format specification §14):
  an earlier version knows nothing of format 1.1 and does not carry its masks, policies and `thinning` into the archive as this one does.
- The module requires `github.com/tink-crypto/tink-go/v2` v2.8.0, for its raw AES-SIV primitive (`daead/subtle`) only.
  Only `redact` and an internal package link it:
  a program that does not import `redact` links no Tink code, whatever else of the module it uses.
- The corpus manifest's `corpus` value is now `tracepack-corpus/4`, its `spec_version` `2.33` and its `format_version` `1.1`.
  `jsonl_schema` moves from the manifest into each vector of class `read` and `truncation`:
  `tracepack-jsonl/1` when its pack's `format_minor` is 0, as for every earlier vector, and `tracepack-jsonl/2` otherwise.
  `primitives.json` gains the keys `digest_keys`, `encryption_keys`, `hmac` and `aes_siv`,
  and `footer.json` each boundary's `gap_start` and `gap_end` and the F-5 `seq_ranges`; every earlier key keeps its value.

### Added

- Format 1.1 in the pack metadata:
  `Thinning` and `PackMeta.Thinning` (tag 0x0033);
  `RedactionPolicy.EncryptionAlgorithm` (`*EncryptionAlgorithm`) and `RedactionPolicy.EncryptionKeyID` (`*string`), each nil exactly when its tag is absent;
  `RedactionEntry.SIV` and `RedactionEntry.HasSIV`, a present empty `siv` told apart from an absent one and any length kept;
  `RedactionEntry.RawMaskedRanges`, which `MarshalBinary` writes as `masked_ranges` and refuses beside `MaskedRanges`;
  the `EncryptionAlgorithm` enum, `EncryptionAlgorithmUnknown` and `EncryptionAlgorithmAESSIVCMAC512` (1);
  and `QualityUnobserved`, bit 7 (`unobserved`).
  A decoded value is written back by `MarshalBinary` as it was decoded.
- `UnmarshalPackMetaMinor(b, minor)` and `NewPackInfoMinor(packID, captureID, minor, metadata)`.
- `(*Reader).Unobserved(rec)`, true only for a record carrying bit 7 in a pack of minor 1 or later;
  with `len(Record.Payload)` and `Record.DecodeStatus` it tells an unobserved record from a genuinely empty message text and from an abnormal reply.
  `Filter` gains no predicate for it.
- `PackHeader.RedactionDefects`: the recoverable defects of the redaction entries found at bootstrap,
  each entry on its own (its ranges array, zero lengths, unsorted, overlapping or adjacent ranges, a present `siv` that is not 16 bytes),
  the order of the entries, and a `redaction-present` flag that disagrees with them.
- `Item.Redaction` (`*RecordRedaction`), from `Reader.Iterate` and `MergeIterate`,
  for every record that carries `quality.redacted` or that an entry names:
  copies of its entries, its defects and whether it is `WhollyMasked`, the same with or without `Query.Payloads`; nil for every other record.
  `RecordRedaction`, `RedactionDefect` and `ErrRedactionDefect`, which every defect wraps.
- The package `redact`: masking at write under a redaction policy, the in-place AES-SIV encryption of the originals of data records,
  HMAC-SHA-256 digests comparable across captures, and an all-or-nothing restore.
  - `Policy`, `Encryption`, `ItemRule`, `AnnotationRule`, `Compiled`, and `Keys`, an interface through which the caller keeps custody of the keys.
  - `Compile` rejects, before use, a domain that is not 1 to 65535 bytes of UTF-8, a malformed path, a stream above 127,
    and, under encryption, two item rules of one stream and function that can select overlapping items.
  - `(*Compiled).Mask(capture, r, unobserved, keys)` masks one record and returns its entries;
    the caller says whether the record is unobserved, which is never masked.
    It refuses a record already carrying `quality.redacted`, and text nested deeper than 64 lists, which the consumer defers.
  - `(*Compiled).PackPolicy`, the `redaction_policy` value of the pack metadata.
  - `Restore(capture, r, rr, policy, key)` gives back the original payload of a masked data record, all or nothing;
    it reruns every check of Marking on its own inputs and trusts no value its caller built.
    An annotation's masks keep no original, so it restores none.
  - `Digest`, `DigestSize`, `KeySize` and `DomainMessageText`.
  - Errors: `ErrInvalidPolicy`, `ErrOverlappingRules` and `ErrDomain` from `Compile`;
    `ErrCannotScreen`, `ErrNotUnobserved`, `ErrRedacted` and `ErrKeySize` from `Mask`;
    `ErrNoEncryption`, `ErrUnsupportedAlgorithm`, `ErrKeySize`, `ErrNotRestorable` and `ErrAuthentication` from `Restore`,
    which also returns errors wrapping `tracepack.ErrRedactionDefect`.
- The bus codec, layout 1 of the durable-bus message (the storage specification §4):
  - the constants `BusLayout1`, `BusHeaderLayout`, `BusHeaderMsgID`, `BusHeaderDescriptor`, `BusHeaderPayloadRef`, `BusHeaderPayloadDigest`,
    `BusHeaderBlockLimit` (4096) and `BusMinMaxPayload` (4140);
    the errors `ErrBusMalformed`, `ErrBusNotAccepted` and `ErrBusPayloadMismatch`; the types `BusEnvelope` and `BusRecord`.
  - `BusToolToken`, `ParseBusToolToken` and `BusSubject`, the subject `<prefix>.<tool token>.<capture_id>`;
    `BusMsgID` and `ParseBusMsgID`, the id `<capture_id>.<seq>`, which refuse seq 2^63 − 1, reserved for a consumer's close.
  - `NewBusRecord` and `EncodeBusBody`, the producer's side: the 44-byte record header, then the payload inline or none for a claim-check record,
    `decode_status`, `trailing_bytes` and `field_validity` written as zero;
    it refuses a record carrying `quality.redacted`, seq 2^63 − 1 and an inline length that differs from the payload's.
  - `CheckBusMessage`, the consumer's check, which returns the record normalized:
    `ErrBusMalformed` for a message never written, `ErrBusNotAccepted` for a capture's start it cannot accept;
    a claim-check record keeps its declared length without a payload being allocated.
  - `BusPayloadDigest` and `CheckBusPayload` for the object of a claim-check record, which also check a claim-check start;
    `BusInline` and `CheckBusStart`, the producer's sizing preflight;
    and `DeriveBusRecord`, the values a consumer derives by kind, from the capture's `Classifier`.
- The descriptor codec: `EncodeDescriptor` and `DecodeDescriptor`,
  the producer-owned entries of a capture's pack metadata, `thinning` included, with no default applied.
  `DecodeDescriptor` refuses a top-level tag the descriptor does not carry, single or repeated,
  a nested tag `thinning` does not define, a `thinning` entry above stream 127 or function 255,
  and a `capture_method` or `time_source` layout 1 does not serve.
- `Continuation` (see Upgrade notes).
- `WriteClose(w, CloseOptions)`, a consumer's close of a capture left open:
  one `stop-unclean` record at seq 2^63 − 1, alone in a segment, its gap bounds computed in checked `int64` arithmetic and its `format_minor` by feature.
  `CloseOptions` holds `Meta`, `CaptureID`, `Trigger`, `HighestSeqTS`, `PackID`, `Uncompressed`, `Sync` and `Now`;
  equal options with a fixed `PackID` and `Now` write equal bytes, and the pack id is returned with an error, once the options are valid.
- The export schema `tracepack-jsonl/2`, for packs of `format_minor` 1 or later:
  it names `thinning` and its nested tags, `encryption_algorithm` (by name, such as `aes-siv-cmac-512`) and `encryption_key_id` of `redaction_policy`,
  `siv` of `redaction`, and the `quality` bit `unobserved`; a tag a later minor adds stays under `unknown`.
- `classify.NewNamed(name, maxFrameLen)`, a classifier whose `Name` is the one given,
  which a consumer of a durable bus sets for every pack of a capture;
  `classify.New` keeps the name from the build info.
- Corpus vectors, 24 of them:
  format 1.0 packs holding the numbers of format 1.1, legacy extracts under a policy, and a pack of minor 2;
  footer boundaries and seq ranges equal in their first keys; a redaction entry missing a required nested tag;
  masking at write, digests, encryption, recoverable and overlapping defects, a merged masked archive,
  and a masked and thinned base pack cut at every offset and with a corrupt block;
  thinned FDC polls; and the consumer's close of a plain, a masked and thinned, and an empty capture, also merged.
  `primitives.json` publishes the test keys `test-digest-1`, `test-digest-2`, `test-enc-1` and `test-enc-2`, with HMAC and AES-SIV known-answer cases,
  the `label` case holding the 53 bytes of associated data.

### Changed

- The Writer writes `format_minor` 1 exactly when the pack holds a feature of format 1.1:
  a redaction entry in a pack that is not an extract, a policy with the encryption tags, an entry with `siv`, or a `thinning` entry;
  every other pack keeps minor 0 and exports `tracepack-jsonl/1`, byte for byte as before.
  A pack that is not an extract and holds entries but no policy is written as minor 1; its redacted records are refused as before.
- A pack masked at write may hold records carrying `quality.redacted` and sets `redaction-present`.
  The Writer checks their Marking:
  `NewWriter` refuses entries a reader would report at bootstrap;
  `Append` refuses, both ways, a record whose Marking does not fit the entries naming its seq, before it takes a seq or closes a block;
  and `Close` refuses an entry naming a seq no appended record carried, leaving the pack unfinalized.
  Each refusal of these Marking checks is a `*FieldError` for `redaction` or `redaction-present` wrapping both `ErrMetadataCommitment` and `ErrRedactionDefect`.
  A record that breaks a commitment before the Marking checks run, such as a redacted record in a pack whose file header does not set `redaction-present`,
  is refused, as before, with a `*FieldError` wrapping `ErrMetadataCommitment` alone,
  so test a refusal of either kind with `errors.Is(err, ErrMetadataCommitment)`.
  Extracts keep the rules of format 1.0.
- The Writer stores bit 7 of an unobserved record it accepts.
- A `SegmentWriter` whose capture reaches seq 2^63 − 1 finalizes that segment, then refuses every record with `ErrSeqOrder`; it never restarts at seq 0.
- A reader treats a record as wholly masked when an entry naming it is defective, when it is named by either entry of a pair out of order,
  or when it fails a check of its own:
  ranges inside the payload, past the 14-byte frame prefix of a data record, or inside an annotation's `text` or `raw` value;
  the `redacted` bit agreeing with the entries;
  no entry and no `redacted` bit on a control record, a transport event or a record of an unknown kind, which are never masked;
  under a policy carrying `encryption_algorithm`, `siv` on every entry of a data record and none on an annotation's;
  and encrypted entries that do not overlap, overlapping zero masks staying allowed.
- `Merge` merges format 1.1 packs.
  The archive of inputs carrying `redaction_policy` carries that policy, their one `classifier` and `max_frame_len`, and their redaction entries,
  each seq's once, ordered by seq, then by first masked offset;
  the archive of inputs without it carries the union of their `classifier` and `max_frame_len` values, as before.
  Either carries the inputs' `thinning`;
  it sets `redaction-present` exactly when it carries an entry,
  and writes the largest `format_minor` of its inputs, also for an archive without a feature of format 1.1.
  Where an input's pack metadata commits its records to Marking or `thinning`,
  a block copied verbatim is checked against them as a resolved or coalesced block is.
- Entry conflicts: `ErrMergeConflict` also reports redaction entries that differ for one seq,
  found from the pack metadata before any block is read.
  `MergeOptions.OnConflict` receives each, its versions the entry lists of the inputs;
  `MergeReport.Conflicts` counts them, and `ConflictsComplete` stays false, since no record was compared.
- The Godoc of `PackFacts.DurableRecorder`, `BoundaryKindStopUnclean`, `CaptureDescriptor` and `DefaultClockStepTolerance` describes the consumer of a durable bus,
  no longer a recorder with a durable spool.
- The conformance corpus follows spec v2.33, schema `tracepack-corpus/4` (see Upgrade notes);
  `footer.json` lists boundaries and seq ranges in their full order.
  Every earlier vector keeps its `pack.tpk` and `export.jsonl` byte for byte.
  121 vectors, in 267 files.

### Fixed

- A read over several packs, `MergeIterate` and each hour a transaction lookup reads, reports a `coverage` entry that one pack holds for another capture the read also reads,
  the defect naming the pack holding the entry, where it skipped it.
  A read of one pack still reports only the entries of its own capture.
- A transaction lookup evaluates every `coverage` entry naming the primary's capture in every pack it reads,
  also in an hour whose read holds no pack of that capture, so such an entry can explain a missing primary or leave the lookup incomplete.

### Performance

- `(*redact.Compiled).Mask` of an S7F3 under a policy with encryption, AES-SIV over its PPBODY and one HMAC-SHA-256 digest,
  takes about 8 µs for a 4 KiB PPBODY and 1.7 ms for 1 MiB, about 500 and 610 MB/s, with 38 allocations and about three payloads of bytes.
- The copy `Writer.Append` makes of a payload a producer has already copied costs about 1.2 % of the append time of a 4 KiB frame with zstd and validation,
  and about 6.2 % without either, so no appending API that takes ownership of the payload is added.
- `Open` and `Merge` of format 1.0 packs cost what they did in v0.4.0, within noise, and a format 1.0 `Open` makes as many allocations:
  validating an entry list looks up only the registry fields of missing tags, and the per-record checks of format 1.1 run only on blocks
  whose summary shows a redacted or unobserved record or an entry's seq.
  `Merge` without coalescing measures about 3 % slower in its benchmark, which follows the stack offset of its block-summary loop, not added work.

## [0.4.0] - 2026-10-11

This release follows spec v2.32, which keeps format 1.0, every pack it describes and every canonical export unchanged,
and leaves to the caller which packs a merge or a read takes.
`Merge` merges the inputs its caller gives, and `FindTransaction` reads the packs its caller gives for each hour;
the active view, the commit objects, the store source, retention and repair are gone.
A lookup reports a match, an ambiguity or no match found, with every reason it is incomplete beside the outcome,
and every read reports the crash gaps of the `stop-unclean` boundaries whose gap meets its time range.
A `Reader` serves blocks from the bytes it read while opening the pack, so a pack of up to 4 MiB costs one request,
and `MergeIterate` reads the next blocks ahead.
`NewWriter` and `Merge` write zstd unless the options ask for none.
The conformance corpus, now `tracepack-corpus/3`, asserts format reading only.

### Upgrade notes

- The corpus manifest's `corpus` value is now `tracepack-corpus/3`, and its `spec_version` `2.32`.
  A consumer of `tracepack-corpus/2` reads `export.jsonl`, `rejection.json`, `fields.json`, `footer.json` and `truncation.json` as before;
  it drops its checks of verification reports, queries, classifications, repairs, reads over several packs and lookups,
  and accepts `truncation.json` rows that hold only `from`, `to`, `rejection` and `exported_records`, adjacent equal rows merged.
- `Merge(ctx, dst, inputs, opts)` takes no `View`: give it the segments and archives of one scope, in the order their lineage is to list them.
  `MergeOptions.ScopeGeneration`, at least 1, and `MergeOptions.PublisherEpoch` are written as given,
  never compared with an input's, so the caller numbers the scope's generations.
  An input of a `pack_role` other than `segment` or `archive` is refused with `ErrMergeInput`, a `repair` input included.
- `WriterOptions.Codec` and `MergeOptions.Codec` are replaced by `Uncompressed bool`, as `SegmentWriterOptions` already had,
  and options that leave it false write zstd:
  delete `Codec: CodecZstd`, write `Uncompressed: true` for `Codec: CodecNone`,
  and set `Uncompressed: true` where the codec was left unset and uncompressed packs are still wanted.
- `NewWriter` refuses `pack_role` `repair` (4) with a `*FieldError` wrapping `ErrFieldValue`, as it refuses the retired 5.
  Every reader still reads a pack of role 4.
- `PackSource` has one method, `Readers(ctx, hour int64) ([]*Reader, error)`, which returns the readers of every pack given for one UTC hour, of any capture;
  a source of your own implements it in place of `Observe`.
  `NewReaderSource(readers)` takes no options:
  drop `ReaderSourceOptions`, and give it every pack the lookup is to read, `Complete` and `Evidence` having no replacement.
- `FindTransaction`'s outcomes are `TxMatched` (1), `TxAmbiguous` (2) and `TxNotFound` (3, String `not-found`).
  `TxNotFound` takes the value `TxUnmatched` had, and means only that no valid match was found in the packs read.
  `TxIncomplete` is gone: a lookup is incomplete when `len(TxResult.Incomplete) > 0`, beside any outcome.
- Renamed: `TxResult.Gaps` is `TxResult.Incomplete`, `TxGap.Barrier` is `TxGap.Boundary`, and `TxGapBarrier` is `TxGapCrashGap` (String `crash-gap`).
- `TxGapReason` values are renumbered, so a value stored or compared as a number must be mapped:
  `TxGapNoKey` 1, `TxGapConflict` 2 and `TxGapIndex` 3 keep theirs;
  `TxGapRead` 5 → 4, `TxGapCoverage` 6 → 5, `TxGapUnevaluated` 7 → 6, `TxGapSeqGap` 9 → 7, `TxGapOpenWindow` 10 → 8,
  `TxGapCrashGap` (was `TxGapBarrier`) 11 → 9, `TxGapCaptureBoundary` 12 → 10, `TxGapOrderingUncertain` 13 → 11,
  `TxGapCorrelation` 14 → 12, `TxGapUnavailable` 15 → 13 and `TxGapScopeBreach` 18 → 14.
  `TxGapCold`, `TxGapEvidence`, `TxGapContradiction` and `TxGapConflicted` are removed.
- Reads apply no retention boundary: `Query.Retention`, `TxOptions.Retention`, `Result.Removed`, `Retention` and `ErrRemoved` are removed.
  Retention is the storage service's; give a read only the packs of the hours still retained.
- Packs are chosen by the caller: `ActiveView`, `View`, `Generation`, `CommitSet`, `ErrViewConflicted` and `CommitKey` are removed, with no replacement.
- The store source is removed: open the packs a listing or your catalog names with `Open`, and give them to `NewReaderSource`.
- `Repair`, `RepairOptions`, `RepairReport`, `ErrRepairNotNeeded` and `ErrNotRepairable` are removed, with no replacement:
  a read reports a damaged pack's defects beside the records it could read, and `Verify` stays.
- `Reader.Iterate`, `MergeIterate` and `ExportJSONL` report a `ReasonCrashGap` defect for every `stop-unclean` boundary whose gap meets the query's time range,
  so a read of a pack holding one can be incomplete where it was complete; the bytes `ExportJSONL` writes do not change.
- `DefaultWholeReadThreshold` rises from 256 KiB to 4 MiB,
  and a `Reader` keeps up to max(`WholeReadThreshold`, `HeadWindow` + `TailWindow`) bytes for its lifetime, 4 MiB with the default options,
  outside `MergeIterateOptions.MaxHeldBytes`, `TxOptions.MaxHeldBytes` and `TxOptions.MaxStateBytes`.
  A process that holds many Readers open sets a smaller `ReaderOptions.WholeReadThreshold`.
  A block served from those bytes is not read again, so a change to the object after `Open` no longer shows in it.
- `MergeIterate` may issue several `ReadAt` calls at once on a Reader's `io.ReaderAt`, as `Open` already did,
  so that `io.ReaderAt` must allow parallel calls, as the `io.ReaderAt` contract requires.

### Removed

- The active view and the commit objects:
  `ActiveView`, `View`, `Generation`, `CommitSet`, `ErrViewConflicted` and `CommitKey`.
  `SegmentKey`, `ArchiveKey`, `EscapeToolID` and `HourOf` stay.
- The store source:
  `NewStoreSource`, `StoreSourceOptions`, `ObjectStore`, `Object`, `ObjectInfo`, `ObjectPage`, `ErrObjectNotFound`,
  `Catalog`, `CatalogSnapshot`, `CatalogScope`, `CatalogPack`, `ExcludedPack`,
  `DefaultMaxCommitListings`, `DefaultMaxListPages`, `DefaultMaxObjects` and `DefaultMaxSourceBytes`.
- Observations and per-capture evidence:
  `PackSource.Observe`, `Observation`, `SourceScope`, `CaptureEvidence`, `EpochClosure`,
  `EndState` with `EndOpen`, `EndStopped` and `EndStoppedUnclean`, `AddPackEvidence` and `ReaderSourceOptions`.
- Lookup outcomes, reasons and fields:
  `TxUnmatched`, `TxIncomplete`, `TxGapCold`, `TxGapEvidence`, `TxGapContradiction`, `TxGapConflicted`, `TxScope.Indexed` and `TxOptions.Retention`.
- Retention: `Retention`, `ErrRemoved`, `Query.Retention` and `Result.Removed`.
- Repair: `Repair`, `RepairOptions`, `RepairReport`, `ErrRepairNotNeeded` and `ErrNotRepairable`.
- `WriterOptions.Codec` and `MergeOptions.Codec`, and the option error for a codec a writer may not use.
  `Codec`, `CodecNone` and `CodecZstd` stay, for what readers report.
- From the corpus: the classes `repair` and `multi-pack`;
  the files `verify.json`, `queries.json`, `classify.json`, `repair.json`, `patch.tpk`, `patch.jsonl`, `patch.verify.json`,
  `pack-<n>.tpk`, `pack-<n>.verify.json`, `reads.json` and `lookups.json`;
  the 13 `multi-` vectors, the 19 `tx-` vectors and 15 of the 18 `repair-` vectors.
  What those files asserted is now checked by tests of the Go module.

### Changed

- `Merge(ctx, dst, inputs []MergeInput, opts MergeOptions)` merges the inputs it is given into one archive of the first input's scope, its capture and UTC hour.
  - `compacted_from` lists each input's `compacted_from` in input order, then the `pack_id` of each segment input, each pack once, at its first occurrence, and is absent when it names none;
    `supersedes` lists the `pack_id` of each archive input in input order, and is absent without one.
  - `ErrMergeInput`, before `dst` is touched, refuses a `pack_role` other than `segment` or `archive` (`repair`, the retired 5 and unknown values included),
    a `scope_generation` that contradicts the role (a segment not at 0, an archive at 0),
    a `pack_id` an earlier input has, and an input of another capture or UTC hour than the first.
  - No inputs at all is a plain error, and an input that cannot be opened is named by its position.
  - `MergeOptions.PackID` must differ from every input's `pack_id` and every pack an input's `compacted_from` or `supersedes` names,
    and `MergeOptions.ReplacementSetID` from every archive input's `replacement_set_id`;
    both are plain errors, checked after the inputs are opened.
- `FindTransaction` reads, for each hour scheduled, every pack the source gives for that hour, of any capture, in one `MergeIterate` in capture order.
  - It decides on the primary's capture only, comparing seqs as (`capture_id`, `seq`),
    and reports read defects, scope breaches, packs with `quality_evaluated` false and crash gaps for every pack it read;
    another capture's conflicts are listed in `TxResult.Conflicts` without making the lookup incomplete.
  - Crash gaps come from the reads and replace the evidence barriers:
    each boundary is reported once across the hours, as a `TxGapCrashGap` when its gap meets the time range of the hours scheduled, or of the primary's hour on an early return.
    A crash gap of the primary's capture that meets its hour explains a missing primary.
  - A missing primary that nothing in its hour's read explains, an empty hour included, fails with `ErrNotPrimary`;
    a missing, conflicting or keyless primary otherwise ends the lookup as `TxNotFound` and incomplete.
  - When a read detects that a block disagrees with its F-2 entry, or another footer value with its records,
    only the `TxGapIndex` gap and the lookup's being incomplete are guaranteed; every other fact is best-effort.
    Records above the primary that arrive before it, which only a block disagreeing with its F-2 entry can yield, are counted and no longer classified,
    and an hour is no longer read a second time.
- `NewReaderSource(readers)` groups the readers by the UTC hour of their period, whatever their capture, and accepts a pack of any role but `extract`,
  `repair`, the retired 5 and unknown values included.
  It rejects with `ErrInvalidQuery` a nil reader, a Reader given twice, two readers of one `pack_id`, readers of more than one `tool_id`,
  an `extract`, and a pack whose period is empty or not inside one UTC hour.
  It opens and closes no reader: the caller owns them.
- `MergeIterate` lists its crash gaps after the defects of the walked blocks' first reads and before those of the clusters' reads, by pack, then by seq;
  the crash gaps found before a read fails at its first cluster are in the `Result` returned with the error.
  A boundary that several packs hold is reported once, with the smallest index of a pack holding it; boundaries whose gap bounds differ are each reported.
- `NewWriter` and `Merge` write block bodies and the footer with zstd unless `Uncompressed` is set, as the format specification §2 recommends;
  a block `Merge` copies verbatim keeps its own codec.
  `SegmentWriter` writes the same bytes as before.
- `NewWriter` refuses `pack_role` `repair` before it validates the pack metadata and before it writes anything;
  `PackMeta.MarshalBinary` still encodes a pack metadata of role 4.
- `DefaultWholeReadThreshold` rises from 256 KiB to 4 MiB.
- A `Reader` keeps, for its lifetime, the bytes of `Open`'s first round that hold blocks:
  the whole object, or else the head and tail windows, never the suffix read from a `FooterOffset` hint.
  It serves every block read and forward-walk envelope read lying wholly inside them with a copy, never an alias, and validates the block as before.
  The other buffers `Open` reads into, a `FooterOffset` suffix, the second round's reads and the chunks of a streamed footer CRC, are dropped when it returns.
- `Result.Complete` reports whether `Incomplete` is empty.
- `PackInfo`, `NewPackInfo` and `Reader.Info` stay; their Godoc no longer refers to views.
- Tests now pin how readers treat an unknown `trailer_version` or `footer_layout_version` (the footer is not used and the pack is read by the forward walk),
  and the zstd window bound: a frame declares a window of at most max(8 MiB, its decoded length), never above 64 MiB, and a larger window is rejected.
  Neither changed behaviour.
- The corpus follows spec v2.32 and asserts format reading only.
  The kept `pack.tpk`, `export.jsonl`, `rejection.json`, `fields.json`, `footer.json` and `primitives.json` files are byte for byte those of `tracepack-corpus/2`.
  `footer.json`'s `accepted` means that the pack is finalized and its footer passes validation, decided on its own; no byte changed.
  Three `repair-` vectors stay as class `read`, since each pack serves a format clause no other vector serves:
  `repair-of-patch`, whose committed pack the generator copies as it is, `repair-inconsistent` and `repair-not-needed-commitments`.
  97 vectors remain, in 213 files.

### Added

- `ReasonCrashGap` (`crash-gap`), a defect of `Reader.Iterate`, `MergeIterate` and `ExportJSONL` for every `stop-unclean` capture boundary whose gap meets the query's time range,
  taken from the F-5 boundary entries of every pack whose footer is used, whatever the filter excludes,
  and from the boundary records of the walked blocks of a pack without one.
- `Defect.Boundary`, the boundary of a `ReasonCrashGap` defect, whose `Block` and `Offset` are -1.
- `TxNotFound`, `TxGapCrashGap`, `TxResult.Incomplete` and `TxGap.Boundary` (see Upgrade notes).
- `WriterOptions.Uncompressed` and `MergeOptions.Uncompressed`.

### Performance

- A pack of up to `WholeReadThreshold` bytes costs exactly one `ReadAt` for `Open` and none for every later read of it:
  `Reader.Iterate`, `ExportJSONL`, `MergeIterate` and the forward walk.
  Opening and reading one busy hour (577,346 bytes) takes 1 `ReadAt` of 577,346 bytes where it took 3 of 904,444.
  A day of 24 hourly packs takes 24 requests to open instead of 48 and none to read instead of 24;
  with 10 ms per request, its `MergeIterate` takes about 90 ms instead of 340 ms.
- A footer over `MaxFooterLen` that `Open`'s first round holds whole has its CRC computed from those bytes instead of streamed.
- `MergeIterate` reads up to 8 of the next blocks it will read ahead of the one it resolves, concurrently and within `MaxHeldBytes`,
  in time order, and in capture order when at most one capture has a cluster to read, so also for a lookup hour of one capture.
  A read ends with `ErrReadLimit` exactly where it would without reading ahead, defects and `ReadAt` errors keep their order,
  and a complete read issues no more `ReadAt` calls than before, an early stop at most 8 more.
  Six packs of 256 KiB blocks at 10 ms per request read in about 57 ms instead of 399 ms, with the same 36 requests.
  `Merge`, `Reader.Iterate` and `ExportJSONL` read as before.
- With zstd by default, merging one hour of twelve five-minute segments writes a busy archive of 577,708 bytes instead of 1,533,515,
  and an idle one of 24,450 bytes instead of 135,893.

## [0.3.0] - 2026-10-10

The conformance corpus covers reads over several packs and transaction lookups (spec v2.30),
so an implementation of either checks itself against the committed files as it already does for single packs.
With them every clause of the semantics specification §9 has a vector, or a test of this implementation where no result shows it,
but redaction and an extract beside its source, which come later.
The two clauses of the format specification §16 that need several packs have their vectors too.
Spec v2.31 lets a log converter reuse its ids for reproducible output and write segments;
`FindTransaction` classifies every version it reads, also from a pack whose index disagrees with its blocks,
and a directory sink can refuse to replace an existing segment.

### Upgrade notes

- The corpus manifest's `corpus` value is now `tracepack-corpus/2`, and its `spec_version` `2.31`.
  A consumer that requires `tracepack-corpus/1` must accept the new value;
  the files of the classes it already reads keep their form, so nothing else changes for it.
- `NewDirSink` takes options after its prefix: `NewDirSink(root, prefix string, opts ...DirSinkOption)`.
  A call compiles unchanged; code that stores `NewDirSink` as a value of type `func(string, string) (SegmentSink, error)` must wrap it.

### Changed

- The corpus follows spec v2.31, after spec v2.29, which states how a log of several tools converts into captures and changed no golden,
  and spec v2.30, which defines the corpus part 2;
  spec v2.31 lets a log converter reuse its ids for reproducible output and write segments, and changes no golden.
- The corpus README's size budget per vector group and root file is now a ceiling a test enforces,
  and the README's budget table must state the same ceilings.
- `FindTransaction` classifies the versions that the read of the primary's scope yields before the primary,
  which only a block disagreeing with its F-2 entry can deliver out of seq order:
  they take their roles, can bound the window and give their boundaries,
  and a version with a role keeps every version of its seq that the scope's read yielded, before the primary or after it.
  An evidence closure claim at their seq is checked, a claim they do not hold being reported as a contradiction.
  Until the primary is read they are held as copies charged to `MaxStateBytes`, so such a damaged pack can reach that limit sooner.
  The outcome stays `incomplete`, from the block's index defect.
  Lookups over packs whose index agrees with their blocks are unchanged.
- `FindTransaction` reports a capture-boundary of the primary's epoch at the window's end
  unless the clean `stop` that bounds the window is of that same epoch:
  a `stop` of another epoch still bounds the window, but no longer hides a footer's `stop` claim of the primary's epoch at its seq,
  which is reported as a `TxGapCaptureBoundary` beside the contradiction.
  Only a footer that does not match its records reaches this case.
- `FindTransaction` reads a scope's view a second time when its read yields one seq in two places,
  as a block disagreeing with its F-2 entry beside another pack's block holding that seq does after the primary,
  and keeps every version of that seq of which one version has a role, where it kept only that one.
  The second read reports nothing again; a scope whose packs' index agrees with their blocks is still read once.
- The `tx-index-mismatch` and `tx-closure-contradiction` corpus vectors gain lookups of these cases.

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
- `NewDirSink` takes options,
  and `DirSinkNoReplace()` makes it never replace a file at a key, as reproducible converter output requires:
  `Commit` publishes by hard-linking the segment to its key, which fails when the key exists,
  even for a file another writer published meanwhile.
  It then compares the existing file with the segment
  and returns an error wrapping the new `ErrSegmentExists` for identical bytes or `ErrSegmentConflict` for different ones,
  the existing file kept and the segment finished.
  Without the option the sink still renames over an existing file;
  a file system without hard links fails such a `Commit`, nothing published.

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
