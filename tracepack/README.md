# tracepack

`github.com/arloliu/go-secs/tracepack` is the Go reference implementation of tracepack (`.tpk`):
an immutable, compressed, self-indexed container for recorded SECS-II traffic between one equipment and its host,
HSMS control messages and transport lifecycle events included.

```sh
go get github.com/arloliu/go-secs/tracepack
```

The module is versioned on its own, with tags `tracepack/vX.Y.Z`.
It is pre-v1: its API may still change between minor versions, as [CHANGELOG.md](CHANGELOG.md) records.
The specification in [docs/specs/tracepack/](../docs/specs/tracepack/) defines every field and rule;
where this package and the specification disagree, the specification wins.
This page is a map by audience; the Godoc states each contract in full,
and [Compatibility](#compatibility) says what the v1 promise covers.

## Concepts

- A **record** is one observation: a captured HSMS frame (a data or control record), a transport event, or an annotation.
- A **capture** is one run of one recorder over one device of one equipment, named by its `capture_id`.
  Its records are numbered by `seq`, which only grows within the capture.
- A **pack** is one `.tpk` file: a header, pack metadata, compressed blocks of records, and a footer that indexes them.
  A pack is never modified once written.
- A **scope** is one capture in one UTC hour.
  Every pack but an extract holds the records of one scope.
- A **segment** is a pack a recorder writes for one flush interval of a scope, five minutes by default.
  Segments are staged; a merge later folds the packs of a scope it is given into one **archive**.
- The **store** is a bucket of packs under the keys of the storage specification §3,
  `staging/` for segments and `archive/` for every other pack.
  A directory written by `NewDirSink` is laid out as such a bucket.
  Which packs a read or a merge takes is always its caller's choice.
- Format 1.1, a pack's `format_minor` 1, adds packs **masked at write**,
  whose records are masked under a redaction policy that encrypts the originals of data records (the semantics specification §8),
  and records **unobserved by design**, data records whose message text the producer did not capture,
  of the streams and functions the pack's `thinning` entries declare.
  A `Writer` writes minor 0 for a pack holding neither, as for every pack before format 1.1;
  a pack of minor 1 holding neither is still read as minor 1, and `Merge` keeps its minor.
- Format 1.2, `format_minor` 2, adds `recorder_instance_name`,
  the name of the source that records the series `recorder_instance_id` identifies,
  and `device`, the device of the equipment the capture records; tracepack interprets neither.
  A `Writer` writes minor 2 exactly when the pack metadata carries either.

## Recording a capture

For a recorder, or a converter of logs:
`CaptureDescriptor`, `SegmentWriter`, `Classifier`, `SegmentSink` and `NewDirSink`.

A `CaptureDescriptor` holds the pack metadata the producer owns:
the equipment and optionally its device, the transport, how and where the capture was taken, the recording series' identity, with an optional source name beside it, and the time source.
`NewSegmentWriter` checks it with `CaptureDescriptor.Validate` and writes the capture's `start` boundary;
from then on it writes the records you append as segments, through a `SegmentSink`:

- `AppendFrame(ctx, at, dir, epoch, frame, quality)` for a captured HSMS frame, length prefix included,
  classified by the `Classifier` when one is set,
  with the quality bits you evaluated for it, kept as `Append` keeps a record's, or 0;
- `AppendEvent(ctx, at, epoch, ev)` for a transport event;
- `Append(ctx, r)` for a record you built yourself;
- `Tick(ctx, now)` and `Rotate(ctx)` to commit the open segment;
- `Close(ctx)` to write the `stop` boundary and commit the last segment.

The open segment is committed when its period ends and the next record or a `Tick` shows it,
when the next record lies in another hour,
when it reaches `MaxSegmentBytes` (a soft limit),
and on `Rotate`.
`classify.New(maxFrameLen)` returns the go-secs-backed classifier;
a frame longer than its ceiling is recorded with decode status `oversized` and is not decoded,
unless an earlier row of the decision table matches.
`ExampleSegmentWriter` in [example_test.go](example_test.go) records a capture into a directory.

### Recorder checklist

- **Persist two identities.**
  `RecorderInstanceID` identifies the deployment across restarts:
  generate it once, store it, and give the same value to every capture; the package never generates it.
  `CaptureID()` returns the capture's id from construction on.
  Persist it at once and give it to the next capture as `PreviousCaptureID`,
  so the next capture names this one even after a crash.
- **One writer per capture, one call at a time.**
  A `SegmentWriter` is not safe for concurrent use.
  The go-secs wire observer can report both directions at the same time:
  serialize the observer callbacks into the writer, for example through one channel.
- **Map sockets to nonzero epochs.**
  An epoch numbers a connection within the capture.
  Epoch 0 means unknown and marks the record `correlation-incomplete`, which makes a transaction lookup from such a primary incomplete (`TxGapCorrelation`).
  The go-secs `Socket` value is unique only within one connection,
  so keep a per-capture counter keyed by the connection and its `Socket`, starting at 1.
- **Keep the monotonic reading of your times.**
  For a `capture-clock` capture, `mono_ns` is `at − CaptureOrigin`, written only when both carry a monotonic reading;
  without it a record has no `mono_ns`, and clock steps cannot be detected.
  `time.Now()` and the `At` of the go-secs `WireEvent`, `SocketEvent` and `LifecycleEvent` carry one,
  and `Add` and plain copies keep it.
  `UTC`, `Local`, `In`, `Round` (also `Round(0)`), `Truncate`, `AddDate`, `time.Unix` and unmarshalling all drop it:
  pass the time as you received it, and convert it only for display.
  A zero `CaptureOrigin` takes `Now()` when `NewSegmentWriter` runs, its reading included.
- **Drive `Tick` while idle.**
  A segment's period ends on the clock, but nothing commits the segment until a record or a `Tick` shows it,
  so on a quiet connection the open segment stays open.
  Call `Tick(ctx, time.Now())` from a timer, every few seconds.
  `FlushInterval` bounds how long a record stays in an open segment only while `Tick` is driven;
  it is no durability guarantee:
  a crash loses the open segment's records, and the capture stays open, without a `stop`.
- **Check every error, `Close`'s included.**
  An append, `AppendFrame`, `AppendEvent` or `Append`, checks its record before it touches a segment.
  A record refused there is not added, and the writer stays usable:
  `ErrInvalidRecord`, `ErrPayloadTooLarge`, `ErrFieldValidity`, `ErrMetadataCommitment` or `ErrSeqOrder`,
  or, from `AppendEvent`, the error of encoding the event.
  A call that would touch the sink while its `ctx` is done returns `ctx`'s error before it does, and the writer stays usable too;
  a `Close` refused so can be retried.
  Any other error fails the writer, a `ctx` done during the call's sink calls included:
  it aborts the open segment, and every later call, `Close` included, returns an error wrapping `ErrWriterFailed` and the failure, with no sink call.
  After a `Close` that completed without error, every later call, `Close` included, returns `ErrClosed`.
  Act on an error by the writing calls' table under [Errors](#writing-calls).
  `Close` also fails it when no seq is left for the `stop` boundary:
  it commits the open segment, if any, without `stop`, then returns an error wrapping `ErrSeqOrder`.
  A `Now()` at `Close` that is not representable fails it as well, a refusal wrapping `ErrInvalidRecord`:
  `Close` aborts the open segment and writes no `stop`.
- **A failure does not mean nothing was published.**
  Segments committed before a failure stay published, even ones the failing call committed:
  an append or `Close` that rolls commits the open segment before it opens the next.
  Check `errors.Is(err, ErrPublishUncertain)` on every append, `Tick`, `Rotate` and `Close`:
  it means the segment of the failed commit is visible under its key but may not survive a system crash.
  Without it, the segment the failure aborted, if any, is not visible, and its records are lost and leave a seq gap;
  other segments may still have been published, the failing call's included.
- **A log converter appends its own records.**
  `AppendFrame` refuses a `log` capture method with `ErrInvalidRecord`,
  since a reconstructed frame holds placeholder bytes for identities its source lacks.
  Build each record and give it to `Append`,
  with the `FieldValidity`, `Quality`, `DecodeStatus` and `TrailingBytes` you established;
  the writer assigns `Seq` and applies no classifier.
  A record with a classified decode status needs a `classifier` tag in its segment, and an `oversized` one a `max_frame_len` tag;
  without them `Append` refuses it with `ErrMetadataCommitment`.
  A transport-event or annotation record needs a `FieldValidity` of 0 and a payload that decodes;
  `Append` refuses another with `ErrInvalidRecord`, so `Verify` finds no writer defect in what the writer appended.
  So if the converter classified frames itself, set `SegmentWriterOptions.Classifier`
  to a `Classifier` whose `Name` and `MaxFrameLen` describe that classification, and the writer records both tags.
- **Leave the capture boundaries to the writer.**
  It writes `start` and `stop`;
  `AppendEvent` and `Append` refuse a capture boundary of kind `start`, `stop` or `stop-unclean`, and accept a `gap`.

### Storing segments in a directory

`NewDirSink(root, prefix, DirSinkOptions{})` returns a `*DirSink` that stores each segment as the file `<root>/<SegmentKey(prefix, …)>`,
so a recording can be uploaded to a bucket as it is, its file paths for object keys.

- `root` must be an existing directory, and its own durability is yours.
- The sink opens `root` as an `os.Root` and makes every file-system call through it,
  so nothing it writes lies outside `root`:
  a symbolic link leading out of `root`, such as `root/staging` or `root/.partial`, fails the call that meets it.
  The sink keeps `root` open until its `Close`, which you call after the `SegmentWriter` using it is closed.
  `Close` waits for every `Create`, `Commit` and `Abort` in progress, so a `Commit` ends with its own result, `ErrPublishUncertain` included, before the root closes.
  After `Close`, `Create` and the `Commit` of a segment not yet committed fail with an error wrapping `fs.ErrClosed`, nothing published,
  and the `Abort` of such a segment closes its temporary file, which stays under `.partial/`, and says so wrapping `fs.ErrClosed`;
  a second `Close` does nothing.
- The sink refuses a symbolic link inside `root` where it manages directories,
  so no link, such as `root/staging` leading to `.partial`, makes the temporary files and a key area one directory:
  `.partial` itself fails `NewDirSink` or `Create`,
  and any directory of a key, `prefix` included, fails `Commit` before the rename, nothing published.
  These checks guard against a misconfigured `root`,
  not against a process that writes to `root` while the sink runs, which can remove segments anyway.
- `NewDirSink` fails on `js`, where `os.Root` cannot exclude a link replaced after its check.
- A `prefix` that is not empty or a relative `/`-separated path of clean components, or that starts with `.partial`, is your mistake:
  `NewDirSink` refuses it with an error wrapping `ErrInvalidArgument`, as a segment does a `Write`, `Sync` or `Commit` after its `Commit` or `Abort`.
  A symbolic link the sink refuses, a key that is not a regular file, a first `prefix` component that the file system folds into `.partial`, and `js`
  fail with plain errors: they report the environment, and retrying does not cure them.
- Files are created with mode 0640 and directories with mode 0750, less the process umask.
  No other user reads a segment, which holds records in full;
  a group, such as one a setgid `root` gives every new file, can give an uploader read access.
- A segment is written under `<root>/.partial/` and renamed to its key on commit;
  every directory from the key's up to `root` is then synced, except on Windows, which cannot sync a directory.
  A listing of the key areas sees only committed segments.
  A file a crash left under `.partial/` is never listed;
  remove such leftovers only while no writer uses `root`.
- `NewDirSink(root, prefix, DirSinkOptions{NoReplace: true})` never replaces a file at a key, as reproducible output needs:
  it hard-links a segment to its key instead of renaming it,
  and when the key exists, `Commit` returns an error wrapping `ErrSegmentExists` for identical bytes
  or `ErrSegmentConflict` for others,
  the existing file kept.
  Should removing the temporary file fail after the link, `Commit` still syncs the key's directories
  and returns the removal's error, wrapping `ErrPublishUncertain`, joined with any sync error;
  the temporary file, a second link to the segment, stays under `.partial/`.
  A file system without hard links fails such a `Commit`.
- `prefix` is empty or a relative `/`-separated path, used as given.
  It may not hold an empty, `.` or `..` component, a `\` or a `:`, and its first component may not be `.partial`,
  nor, when the sink is created, name that same directory, as a case variant does on a file system that folds case.
- On a file system that folds case or rewrites names, as macOS and Windows can, two keys may share a path component.
  Nothing escapes `root` even then, and no file is overwritten, since every key holds its segment's unique `pack_id`.

To store segments elsewhere, implement `SegmentSink`:
`Create` returns a `SegmentFile` from a `SegmentInfo`, which holds what the segment key needs.
Nothing may be visible under the key before `Commit`,
and a `Commit` that fails after the segment became visible returns an error wrapping `ErrPublishUncertain`.

### The Writer underneath

`NewWriter` writes one pack to an `io.Writer` from pack metadata you assemble;
`SegmentWriter` and `Merge` are built on it.
It writes block bodies and the footer with zstd, as the format specification §2 recommends, unless `WriterOptions.Uncompressed` is set;
`Merge` does the same unless `MergeOptions.Uncompressed` is set.
`CompressionLevel` on `WriterOptions`, `SegmentWriterOptions` and `MergeOptions` trades CPU for size:
`CompressionFastest` encodes in about two thirds of the default's time for slightly larger output,
`CompressionBest` writes the smallest output in three to four times the time;
the zero value, `CompressionDefault`, writes what earlier versions wrote.
It validates each block before writing it unless `WriterOptions.SkipValidation` is set.
For every pack but an extract it refuses a period not inside one UTC hour, and a record of another hour, with `ErrScopeBreach`.
`Append` refuses a transport-event or annotation record whose payload does not decode with `ErrInvalidRecord`;
a payload holding a tag or an enum value this module does not know still decodes, and is stored as given.
`Merge` copies such a record of another writer as it is, and `Verify` reports it as a writer defect.
It writes `format_minor` 2 when the pack metadata carries `recorder_instance_name` or `device` (format 1.2),
else 1 when it holds a feature of format 1.1, and 0 otherwise.
`Close` writes the footer and trailer and returns a `Continuation`:
`next, ok := cont.Next()` gives the seq the capture's next pack starts at,
and `ok` is false after a pack whose last record has seq 2^63 − 1, the last seq of a capture, since no pack follows it.

Its errors, and those of every writing call, are classified under [Errors](#writing-calls).

## Publishing a capture over a durable bus

For a producer, such as an equipment gateway, that publishes each record of a capture as one bus message (the storage specification §4):
the bus codec, in the subpackage `github.com/arloliu/go-secs/tracepack/bus`, and the descriptor codec in the root package.
The module imports no NATS client; it encodes and checks what a message carries.

- The producer assigns `capture_id`, `recorder_instance_id` and every `seq`; seq 2^63 − 1 is reserved for the consumer's close.
- `EncodeDescriptor(meta)` encodes the producer-owned entries of the capture's pack metadata, its `thinning` entries, `recorder_instance_name` and `device` included, with no default applied.
  A consumer before v0.8.0 refuses a descriptor carrying `recorder_instance_name`, so upgrade consumers and mergers before producers.
  Publish it in the `Tpk-Capture-Descriptor` header of the capture's `start`, in standard base64 with padding (`base64.StdEncoding`).
- `bus.NewRecord(r)` and `bus.EncodeBody(&br)` encode one record as the body of layout 1: the 44-byte record header, then the payload.
  A record whose body is too large to travel inline, as `bus.Inline(bodyLen, maxPayload)` says, travels by claim-check:
  set `ClaimCheck`, `Bucket` and `Digest`, the payload's SHA-256 digest, and keep `PayloadLen`;
  the body is then the header alone, and the `Tpk-Payload-Digest` header carries `bus.PayloadDigest(payload)`.
  The body holds `decode_status`, `trailing_bytes` and `field_validity` as zero, since the consumer derives them,
  and the encoder refuses (`bus.ErrMalformed`) a record carrying `redacted` or seq 2^63 − 1,
  and a record whose kind layout 1 does not carry: anything but data, control, transport-event or annotation, kind 0 included.
- `bus.Subject(prefix, toolID, capture)` and `bus.MsgID(capture, seq)` give the subject and the `Nats-Msg-Id`, and constants such as `bus.HeaderPayloadRef` name the headers.
- `bus.CheckStart(headerBlockLen, bodyLen, maxPayload)` is the preflight:
  the header block, as your client encodes it, holds at most `bus.HeaderBlockLimit` bytes on every message,
  and the message fits the deployment's `max_payload`, itself at least `bus.MinMaxPayload`.
  A message over a bound fails with `bus.ErrMalformed`: do not publish it.
  A `max_payload` below `bus.MinMaxPayload`, a negative length or a body shorter than 44 bytes is your mistake, an error wrapping `tracepack.ErrInvalidArgument`.
- Every message carries `Tpk-Layout: 1` (`bus.Layout1`), and layout 1 carries data, control, transport-event and annotation records only;
  a record of another kind needs a new layout, which consumers must know before you emit it.
- A producer leaves a data record unobserved by design only when it identifies the record, by means other than its stream and function,
  as an FDC interval-poll S1F3 or the S1F4 reply paired with it by System Bytes;
  the descriptor's `thinning` must declare that stream and function, a declaration that is necessary, never sufficient.
  Event-triggered queries, empty replies and every other S1F3 and S1F4 stay complete.
  An unobserved record carries `QualityUnobserved`, and its message text is zero-filled.

## Writing the packs of a bus capture

For the log service, the consumer that writes a durable-bus capture's packs:
`bus.CheckMessage`, `bus.DeriveRecord`, the package `redact`, the `Writer`'s masked-at-write path, `WriteClose` and `Merge`.

- `bus.CheckMessage(env, body)` checks one message and returns its `bus.Record`, normalized.
  For a claim-check record, fetch the object and check it with `bus.CheckPayload`.
  `DecodeDescriptor` gives the producer's values from a `start`, to which you add the values you own.
  Act on its errors by the table under [Errors](#bus-messages).
- To resolve a message before checking it, check its `Tpk-Layout` first, then read its record header with `bus.PeekHeader(body)`:
  the fields as stored, `Payload` nil, false only for a body shorter than `bus.RecordHeaderLen` (44, untyped, so `[bus.RecordHeaderLen]byte` holds a header).
  It checks nothing else and allocates nothing, so it answers for a body `bus.CheckMessage` may then refuse.
  `bus.ParsePayloadDigest` decodes a `Tpk-Payload-Digest` value, the inverse of `bus.PayloadDigest`, its error wrapping `bus.ErrMalformed`.
- `bus.DeriveRecord(&br.Record, cls)` sets the values you derive by kind:
  `field_validity`, and `decode_status` and `trailing_bytes` from the capture's classifier, or `not-attempted` for an unobserved record.
  Its `ErrInvalidRecord` for a kind outside the four is never reached by a record `bus.CheckMessage` accepted;
  for a classifier's `trailing_bytes` out of range, it is a classifier bug.
  Classify before masking, since the classification describes the payload as received.
  `classify.NewNamed(name, maxFrameLen)` gives the classifier the name of its configuration, which stays fixed for the capture.
- Under a redaction policy, compile it once with `redact.Compile`, then mask every record of a flush batch with `(*Compiled).Mask(ctx, capture, r, unobserved, keys)`;
  `Compile` refuses, with `redact.ErrInvalidPolicy`, a policy whose `PackPolicy` the pack metadata cannot hold,
  an id that is not valid UTF-8 or a `Version` above 2^63 − 1, so `NewWriter` never refuses a compiled policy;
  `unobserved` is true for a record carrying bit 7, which is never masked.
  Your `redact.Keys` hands out the digest and encryption keys, whose custody stays yours;
  `Mask` checks `ctx` before each key request and passes it to `Keys`, so a key service can bound its call.
  A record that cannot be screened, `redact.ErrCannotScreen`, is deferred, never written unscreened.
  Act on an error of `Compile`, `Mask` or `redact.Digest` by the table under [Errors](#redaction).
  Collect the entries into `PackMeta.Redaction`, ordered by seq, then by first masked offset,
  set `RedactionPolicy` to `(*Compiled).PackPolicy()`, and only then call `NewWriter`, which writes the pack metadata first.
  See the `redact` package's example.
- Such a pack, one that is not an extract and carries `redaction_policy`, is masked at write:
  its policy carries both encryption tags, its metadata exactly one `classifier` and at most one `max_frame_len`,
  each breach refused by `NewWriter` with a `*FieldError` wrapping `ErrMetadataCommitment`;
  it is written with the seqs you give, and `NewWriter` refuses `AssignSeq` for it with an error wrapping `ErrInvalidArgument`.
  `NewWriter` checks the entries, and `Append` and `Close` every record's Marking against them,
  each refusing a mismatch with a `*FieldError` wrapping both `ErrMetadataCommitment` and `ErrRedactionDefect`.
  A record refused by an earlier commitment check, such as a redacted record when the file header does not set `redaction-present`,
  wraps `ErrMetadataCommitment` alone, so test refusals with `errors.Is(err, ErrMetadataCommitment)`.
- An unobserved record needs the pack metadata's `thinning` entry of its stream and function;
  `Append` refuses any other record carrying bit 7, with `ErrInvalidRecord` for its own fields.
- `WriteClose(w, opts)` writes the close of a capture its producer left open:
  one `stop-unclean` record at seq 2^63 − 1, alone in a segment, from the `CloseOptions` you give.
  A retry with the same `PackID` and `Now` writes the same bytes, and the pack id is returned with an error once the options are valid.
- `redact.Restore` gives the holder of a capture's encryption key the original payload of a masked data record, all or nothing.
  Its errors are deterministic, so none is retried; the table under [Errors](#redaction) says what each means.

## Reading packs

For readers and troubleshooters working on local files:
`Open`, `Reader.Iterate`, `MergeIterate`, `FindTransaction`, `FindTransactions`, `NewReaderSource`, `ExportJSONL` and `Verify`.

- `Open(ctx, ra, size, opts)` opens one pack over an `io.ReaderAt`, such as an `*os.File`, which stays open while the `Reader` is used.
  `Reader.Header` returns the pack metadata, and `Reader.Iterate` yields the records a `Query` selects, in file order.
  A damaged block does not fail the read: it is reported in the `Result`, beside the records that could be read.
  See `ExampleOpen`.
- A pack of up to `ReaderOptions.WholeReadThreshold` bytes, 4 MiB by default, currently costs one `ReadAt` for `Open` and none for any later read of it:
  the `Reader` keeps the bytes of `Open`'s first round that hold blocks, the whole object or else the head and tail windows,
  and reads each block from them when they hold it whole:
  a compressed block is decoded from them in place, any other block copied first.
  It keeps at most max(`WholeReadThreshold`, `HeadWindow` + `TailWindow`) bytes for its lifetime, 4 MiB with the defaults,
  outside the budgets of `MergeIterateOptions.MaxHeldBytes`, `TxOptions.MaxHeldBytes` and `TxOptions.MaxStateBytes`;
  lower `WholeReadThreshold` when many Readers stay open at once.
- A zero option field takes its default, and a negative budget field is refused with an error wrapping `ErrInvalidArgument`:
  from `MergeIterate`, `FindTransaction` and `FindTransactions` through `ErrInvalidQuery`, which matches it.
- `MergeIterate` reads several packs as one read, in capture or time order.
  It compares the copies of each record across the packs, yields identical copies once, and reports copies that differ as conflicts.
  It reads blocks ahead of the one it resolves, currently up to 8, within `MaxHeldBytes`,
  in time order, and in capture order when at most one capture has a cluster to read,
  so it may issue several `ReadAt` calls at once on a Reader's `io.ReaderAt`, as `Open` does;
  whatever its outcome, it waits for every call it issued before it returns.
  See `ExampleMergeIterate`.
- An `Item` is valid only during the callback that receives it, its payload and header extension aliasing the read's buffers;
  keep `item.Clone()`, which copies them and the Marking; `Record.Clone()` copies a record with its payload.
- `Item.HSMSHeader` holds the record's HSMS header fields, stream, function, SessionID, System Bytes and the rest, and which of them are available,
  with or without `Query.Payloads`, so a read that filters on S/F without payloads still sees them.
- Readers decode the pack metadata under the pack's `format_minor`, so a number of format 1.1 means nothing in a pack of minor 0.
  `Reader.Unobserved(r)` says whether a record is unobserved by design, its bit 7 meaning `unobserved` only in a pack of minor 1 or later;
  with `len(r.Payload)` and `r.DecodeStatus` it tells such a record from an empty message text and from an abnormal reply.
  `Item.Unobserved` and `TxRecord.Unobserved` hold that answer for the pack each record was read from,
  so a `MergeIterate` or `FindTransaction` caller needs no Reader to ask.
- A record that carries `quality.redacted` or that a redaction entry names has its Marking in `Item.Redaction`:
  copies of its entries, its defects, and whether it is wholly masked, the same with or without `Query.Payloads`.
  `PackHeader.RedactionDefects` lists the defects of the entries found at bootstrap.
  A defect never fails `Open` or a read and never makes it incomplete; the records it concerns read as wholly masked.
- A read reports a **crash gap**, an `IncompleteReasonCrashGap` defect whose `Defect.Boundary` holds the boundary,
  for every `stop-unclean` capture boundary whose gap meets the query's time range (the semantics specification §7.4):
  records of that time may be missing.
  It takes them from the footer of every pack whose footer it uses, whatever the filter excludes,
  and from the boundary records of the blocks it walks in a pack without one.
- `FindTransaction(ctx, src, key, opts)` looks up the reply to a primary message, named by a `TxKey`;
  `TxKeyOf(captureID, r.Seq, time.Unix(0, r.TSUTCNs))` builds the key from the primary's record.
  It reports `TxMatched`, `TxAmbiguous` or `TxNotFound`,
  and lists in `TxResult.Incomplete`, beside any outcome, every reason the lookup is incomplete, each a `TxGap`.
  `TxNotFound` states nothing about records outside the packs it read.
  `TxResult.Complete()` reports an empty `Incomplete`:
  a conflict that bears on the lookup is a `TxGapConflict` gap there, while an unrelated one is listed only in `Conflicts`.
  `Matches()` returns the valid matches and `PrimaryVersions()` the primary's versions, as pointers into `Records`, nil when none;
  `Records` keeps every version the lookup kept, the diagnostic ones included.
  See `ExampleFindTransaction`.
- `FindTransactions(ctx, src, r, opts, fn)` pairs every primary of one capture whose timestamp lies in `[r.From, r.To)`, reading each hour once,
  where a `FindTransaction` per primary reads its hours again for every primary.
  Each `TxPairing` passed to `fn`, in ascending hour, then seq, holds the primary's key and the result `FindTransaction` gives it, as your own copy.
  It holds at most `MaxHours` hours at once, bounded by `TxRangeOptions.MaxCachedBytes`;
  a primary near the end of the hours read can report an open window when a later hour is not written yet.
  It may run an hour's lookups on several goroutines, currently up to `GOMAXPROCS`, and calls `fn` from yours, one pairing at a time;
  today a `GOMAXPROCS` of 1 runs them on your goroutine alone.
  See `ExampleFindTransactions`.
- `ExportJSONL(ctx, r, w)` writes the canonical JSONL export of one pack,
  as the [JSONL specification](../docs/specs/tracepack/tracepack-jsonl.md) defines it:
  a header line, then one line per record, the same bytes any conforming exporter writes for the pack.
  A pack of format minor 0 exports schema `tracepack-jsonl/1`, byte for byte as earlier versions wrote it,
  a pack of minor 1 `tracepack-jsonl/2`, which names the additions of format 1.1,
  and any other pack `tracepack-jsonl/3`, which also names `recorder_instance_name` and `device`.
  A damaged pack exports the records of its validated blocks, and the returned `Result` reports what was lost exactly as `Iterate`'s does:
  a failed block, a block that disagrees with its F-2 entry, a crash gap, a coverage entry or an unfinalized pack makes it incomplete;
  a footer not used in a finalized pack whose walk accounts for every block adds only an entry to `FooterErrs`.
  A reader budget that keeps part of the pack out fails the export with an error wrapping `ErrReadLimit` instead;
  after any error, discard what was written.
  A line may be written in several pieces, currently of at most 64 KiB each, so one line may take several `Write` calls.
  See `ExampleExportJSONL`.
- `Verify` reads every block of a pack and reports its outcome, from finalized-consistent to corrupt.
  `Reader.Verify` does the same for a `Reader` already open, under its options, and leaves it usable,
  so a pack you verify and then read is opened once.
- `Record.CaptureBoundary` returns the kind of a capture's start, stop or gap from a transport-event record, and false for any other record.

### A lookup over local files

`NewReaderSource(readers)` gives `FindTransaction` a source over packs you opened, all of one equipment.
It groups them by the UTC hour of their period, whatever their capture,
and a lookup reads, for each hour it searches, every pack of that hour.

- Give every pack the lookup should read, segments and archives alike:
  a pack not given is not read, and the lookup states nothing about its records.
  A segment and an archive holding the same records may both be given; their copies are compared and yielded once.
- A pack of any role but `extract` is accepted, if its period is not empty and lies inside one UTC hour, the hour it is read for.
- `NewReaderSource` refuses, with `ErrInvalidQuery`, a nil reader, a `Reader` given twice, two readers of one `pack_id`,
  readers of more than one equipment, an `extract`, and a pack whose period is empty or spans hours.
- The source never opens or closes the readers: keep them open until the lookups are done.

### Reading many packs

A `Reader` is safe for concurrent use, and `Open` shares no state between Readers,
so the packs of a query can be opened on goroutines of your own.
Where each `ReadAt` waits on a network or object store, the opens one after another are most of a read's time,
and opening them at once removes that wait on any number of CPUs.

- Opening in parallel changes nothing a read returns:
  `MergeIterate`, `FindTransaction` and `FindTransactions` take the Readers however they were opened.
  See `ExampleOpen_parallel`, which keeps the errors by position, so the one it reports does not depend on timing.
- A `PackSource` of your own may open an hour's packs in parallel inside `Readers(ctx, hour)`;
  a lookup asks for its hours one at a time.
  A nil reader it gives for an hour fails the lookup with an error wrapping `ErrInvalidQuery`, wrapped with the hour,
  as a `Reader` or `pack_id` it gives twice for one hour does.
- Such a source may close an hour's objects, such as its files, once the lookup moves on:
  `FindTransaction` and `FindTransactions` read the packs `Readers` returned for an hour only during that hour's read,
  so once a lookup enters its next `Readers` call, or once it returns, no `ReadAt` of them is issued or in flight,
  on success, error and cancellation alike.
  A cancelled `ctx` alone is not that point, since a `ReadAt` in flight takes no context.
  The `Reader` values stay valid until the call returns, which still uses their decoded header and metadata;
  an object you also give for another hour, or share with another lookup, stays open while those may read it.
- Work done hour by hour, such as a batch over settled hours, can run one read per hour on several goroutines;
  with a single CPU, one read of all the hours costs less.
  For packs a `Writer` wrote, other than extracts, every record lies in its pack's hour,
  so the `OrderTime` reads of the hours, taken in ascending hour, yield the records of one `OrderTime` read of all of them, in the same order.
  The rest of a read is its own:
  - each read's `Result` names packs by their index among that read's Readers,
    and reports a crash gap once per read, so a gap held by the packs of two hours is reported by both;
  - copies of a seq are compared only among the packs of one read,
    so a seq held by the packs of two hours is not reported as a conflict;
  - `OrderCapture` over several captures keeps seq order within each capture while the captures interleave, not in hour order;
  - `MaxHeldBytes` and `MaxConflicts` bound each read, so reads in parallel hold up to that many times `MaxHeldBytes`.

## Merging and storage

For the storage service:
`Merge` and the key builders.

- `SegmentKey` and `ArchiveKey` build the object keys of the storage specification §3,
  and `EscapeEquipmentID` spells an equipment_id as a key does.
- `Merge(ctx, dst, inputs, opts)` writes one archive of a scope from the segments and archives you give it, of one capture and one UTC hour,
  with the `ScopeGeneration` and `PublisherEpoch` you set in `MergeOptions`, which it compares with no input's.
  Its `compacted_from` lists the inputs' own lineage and then every segment input, and its `supersedes` every archive input, in the order you give them.
  It refuses with `ErrMergeInput`, before writing anything, an input whose role is neither `segment` nor `archive`, and an input of another capture or hour.
  An option it cannot take, a `WriterTag` that is not valid UTF-8 (beside a `*FieldError` naming `writer`) included,
  and a `ReplacementSetID` or `PackID` an input already uses, fail it with an error wrapping `ErrInvalidArgument`, never `ErrRefused`.
  An error of the archive's own blocks, such as a new block of the resolution that fails its check, reports a defect of this package.
  It opens up to `MaxParallelOpens` inputs at once, 16 by default, and checks them in your order,
  so, while ctx is not cancelled and each input's `ReaderAt` answers independently of the others',
  an error names the input opening them one after another would name; inputs after a failing one may have been read.
  Once `ctx` is done, `Merge`, `MergeIterate` and `FindTransactions` may return `ctx`'s error even when a read or a lookup also failed;
  without cancellation their error does not depend on timing.
  An archive over its budget, or over a limit of the format its blocks reach, such as an F-3 entry list longer than the u32 `summary_len`, fails with `ErrMergeLimit`.
  Which packs a merge takes, and when a merged pack may be deleted, is yours to decide (the storage specification §4);
  publish the archive only after `Merge` returned nil.
- `Merge` merges packs of format 1.2: it carries the inputs' redaction policy, their redaction entries, their `thinning`, their `recorder_instance_name` and their `device`,
  and writes the largest `format_minor` of its inputs.
  Before it writes anything, it refuses with `ErrMergeInput` an input of a newer minor, a policy without both encryption tags,
  inputs carrying `redaction_policy` that differ in policy, `classifier` or `max_frame_len`, a masked input beside an unmasked one,
  and defective redaction entries;
  inputs without a policy still merge with differing `classifier` and `max_frame_len` values, the archive carrying their union.
  Differing entries for one seq fail with `ErrMergeConflict`, also before anything is written.
  A record that fails a Marking or unobserved check is found only once its block is read, after the archive's header,
  and its `ErrMergeInput` leaves partial output in `dst`, which you discard, as after any error.
  Merge packs of format 1.1 only with tracepack v0.5.0 or later, since an earlier one does not carry what format 1.1 adds;
  a merger before v0.8.0 refuses a pack of format 1.2.

## Command line

The `tracepack` command reads packs on local paths:

```sh
go install github.com/arloliu/go-secs/tracepack/cmd/tracepack@<version>

tracepack dump --jsonl [-o <file>] <pack>
tracepack verify <pack>...
tracepack find --capture <uuid> --seq <n> (--hour <n> | --at <time>) <path>...
```

- `dump --jsonl` writes the canonical JSONL export to stdout, or to `<file>` with `-o`, replaced only when the export succeeds,
  through a synced temporary file whose rename is synced too on a Unix system;
  the read's incomplete reasons, footer errors and redaction defects go to stderr.
  An exit 2 after `-o` may come with `<file>` published, when only writing the diagnostics or syncing its directory failed.
- `verify` reports each pack's verification outcome and evidence, and `complete`: no block failed and no block disagrees with the index.
- `find` looks up the reply to a primary over the packs the paths name, a directory searched for `*.tpk` files,
  and prints the outcome, every kept version with its roles, and every reason the lookup is incomplete.

It exits 0 when the report is clean, 1 when it is not (an incomplete read or lookup, a `verify` outcome other than `finalized-consistent`),
and 2 when the command could not run.
The flags, the exit codes, the split between stdout and stderr and the bytes of `dump --jsonl` are contract;
the text of the `verify` and `find` reports is not, and may change in any release.
The command's Godoc states the rest.

## Errors

Match an error with `errors.Is` and `errors.As`, never by its text, which [Compatibility](#compatibility) leaves out of the contract:
each line of an error's text names its namespace, `tracepack: ` or `redact: `, once, and nothing else about the text is promised.
Each family of calls below has a decision table:
act on the first row whose sentinel the error matches, before any cause it wraps,
since one error may carry the sentinels of several rows.

Some sentinels belong to a family, and also match its parent under `errors.Is`:

| Parent | Members | Meaning |
|---|---|---|
| `ErrCorrupt` | `ErrChecksum`, `ErrInvalidFooter` | bytes that are not a valid tracepack encoding |
| `ErrInvalidArgument` | `ErrInvalidQuery`, `ErrInvalidKey` | an argument or option the call cannot take: a caller's mistake |

Each member keeps its identity and text, so a test of the member matches as before,
and a caller that handles the whole family tests the parent.
Every other sentinel stands alone: `ErrNotFinalized`, a pack with no valid trailer, is not `ErrCorrupt`,
and the sentinels of `bus` and `redact` belong to no root family, `redact.ErrInvalidPolicy` included.

Some errors carry the sentinels of two classes by design, and the tables decide between them:
a damaged trailer is `ErrNotFinalized` beside `ErrChecksum` or `ErrCorrupt`;
`ErrMergeInput` wraps the cause that made the input unusable, and `ErrWriterFailed` the failure that made the writer unusable;
`bus.ErrMalformed`, `bus.ErrNotAccepted` and `redact.ErrCannotScreen` may wrap a decoder's `ErrCorrupt` or `*FieldError`;
a redaction mismatch is `ErrMetadataCommitment` beside `ErrRedactionDefect`;
and `ErrPublishUncertain` comes with the cause of the failed commit, `ctx`'s error included.

### Writing calls

`NewWriter`, the `Writer`'s `Append`, `Flush` and `Close`, `WriteClose`, `NewSegmentWriter`,
and the `SegmentWriter`'s appends, `Tick`, `Rotate` and `Close`.
The first two rows concern a later call on a writer that already reported its end; the rest, the call that fails:

| Error | Meaning | Action |
|---|---|---|
| `ErrClosed` | a call after a `Close` that completed without error | stop using the writer; its output, a finalized pack or the committed segments, is complete and kept |
| `ErrWriterFailed` | a call after a failure, which the error wraps | stop using the writer; the output's fate was decided by that failure |
| `ErrPublishUncertain` | a segment visible under its key whose durability is unknown, beside the commit's cause, `ctx`'s error included | keep the segment: verify it, or commit the same bytes again later; never discard it |
| `ErrSegmentExists` | the same bytes already lie at the key (`DirSinkOptions.NoReplace`) | treat the segment as committed |
| `ErrSegmentConflict` | other bytes lie at the key | alert; do not retry |
| `ctx`'s error | | stop |
| `ErrRefused` | the call's arguments, options or records determine the error; the sentinel beside it says which | park or fix the input; do not retry it unchanged |
| anything else | an error of your `io.Writer`, `Syncer`, `SegmentSink` or `SegmentFile`, returned wrapped with its chain intact | may be transient: retry, with a new writer once the old one failed |

Whether the writer is still usable is the sentinel's to say, as the [Recorder checklist](#recorder-checklist) and each sentinel's Godoc state:
a record an append refuses before it touches the output, and a `ctx` done before any sink call, leave it usable;
any other error fails it.
After a failure, a `Writer`'s output is not finalized, or not vouched for: discard it and start a new pack.
A `SegmentWriter`'s committed segments stay valid, its open segment was aborted unless `ErrPublishUncertain` says it was published,
and the records after the last committed segment are written again through a new `SegmentWriter`.

`ErrRefused` marks an error the call's own input determines:
`errors.Is(err, ErrRefused)` means the same sequence of calls with the same arguments, options and records fails the same way,
so a service retrying a flush parks that batch instead.
An error such as `ErrValidation` comes from every record of the block, not only the call's own.
An error of checking or encoding the options of `NewWriter`, `WriteClose` or `NewSegmentWriter`, or an `AppendEvent` event, also wraps `ErrInvalidArgument`:
beside its own sentinel, such as `ErrRequiredTag` or `ErrScopeBreach`, or alone for one with none, such as a nil `WriterOptions.Meta`.
An error of a record never does, nor one for a format limit the records reach, such as a block's F-3 list too long for the footer.
Never marked are your implementations' errors, `ctx`'s, `Merge`'s, and the `ErrWriterFailed` or `ErrClosed` of a later call;
`ctx`'s error comes back wrapped, so test it with `errors.Is`, not `==`.
The marker leaves the error's text unchanged.

### Reading calls

`Open`, `Reader.Iterate`, `Verify`, `Reader.Verify`, `MergeIterate`, `FindTransaction`, `FindTransactions`, `NewReaderSource` and `ExportJSONL`.
The table applies to the error a call returns, and to the errors of its diagnostics:
`PackHeader.FooterErr`, a `FooterErrs` element's `Err`, `Defect.Err`, and a `TxGap`'s `Err`.

| Error | Meaning | Action |
|---|---|---|
| `ctx`'s error | | stop |
| `ErrSourceRead` | your `ReaderAt` returned an error, or a `ReadAt` did not return; nothing is known of the pack's bytes | may be transient: retry by your source's policy |
| `ErrInvalidArgument`, `ErrInvalidQuery` among it | an argument or option the call cannot take, such as a negative budget or a nil reader | fix the call |
| `ErrNotPrimary` | `FindTransaction`'s key names no primary | look up the primary instead |
| `ErrNotTracepack` | an object that is not a pack | skip it |
| `ErrUnsupportedFormat` | a `format_major` this module does not read | skip it and alert: it needs a newer module |
| `ErrReadLimit` | a structure, or a read, over a budget of the options | raise the budget, or skip |
| `ErrNotFinalized` | a pack with no valid trailer: still being written, cut short, or its trailer damaged | retry later |
| `ErrCorrupt`, `ErrChecksum` and `ErrInvalidFooter` among it | damaged bytes | report the pack and skip it |
| anything else | `io.ErrUnexpectedEOF` of a short read, an object shorter than the size you passed, or the error of `ExportJSONL`'s `w` | may be transient |

`ErrSourceRead` comes second because the module adds no class of its own to a failed read,
but your `ReaderAt`'s error keeps its chain and matches whatever it matches, `ctx`'s error or even `ErrCorrupt`.

Every read path reports each fault of a pack with the same sentinel, the cause kept beside it,
and `Merge` reports a fault of an input's bytes beside `ErrMergeInput`:

| Fault | Sentinel |
|---|---|
| a CRC mismatch: file header, pack metadata, footer, trailer, block envelope or block body | `ErrChecksum`, which matches `ErrCorrupt` |
| pack metadata or a block that does not decode or validate; pack metadata past the end of the object, beside `io.ErrUnexpectedEOF`; records, summaries or a walk that disagree with the index or a valid trailer | `ErrCorrupt` |
| no trailer: a pack still being written or cut short | `ErrNotFinalized` |
| a trailer present but damaged | `ErrNotFinalized`, beside `ErrChecksum` or `ErrCorrupt` |
| a footer that fails its checks | `ErrInvalidFooter`, which matches `ErrCorrupt` |
| a structure over a reader budget | `ErrReadLimit` |
| your `ReaderAt`'s error, or a `ReadAt` that did not return | `ErrSourceRead`, beside your error; never `ErrCorrupt` |
| a short read: fewer bytes than asked, with `io.EOF` or no error | `io.ErrUnexpectedEOF` alone, never `ErrCorrupt` |

Only a fault of the file header or the pack metadata fails `Open`.
A trailer or footer that cannot be used is reported by `PackHeader.FooterErr`, and by the read's `FooterErrs`, as `PackError.Err` or `TxPackError.Err`;
`PackError` and `TxPackError` appear only there and are values, so `errors.As` takes `var pe tracepack.PackError` or `var te tracepack.TxPackError`, not a pointer.
A block or walk fault is a `Defect` in `Result.Incomplete`, or the `TxGapRead` or `TxGapIndex` gap of a lookup, whose `Defect` field holds the reason;
its `Err` wraps, by `Defect.Reason`:

| `Defect.Reason` | `Defect.Err` wraps |
|---|---|
| `IncompleteReasonCorruptBlock` | `ErrChecksum` for an envelope or body CRC mismatch, else `ErrCorrupt` |
| `IncompleteReasonUnknownCodec`, `IncompleteReasonIndexMismatch` | `ErrCorrupt` |
| `IncompleteReasonTruncated` | `ErrNotFinalized` at the end of a pack with no valid trailer, or where such a pack ends inside a block; `ErrChecksum` or `ErrCorrupt` at a bad envelope, a block past a valid trailer's `footer_offset`, or a walk that disagrees with the trailer; `ErrCorrupt` at the end of a pack whose trailer is valid but whose footer CRC fails, which was finished, so it is damaged rather than still being written |
| `IncompleteReasonLimit` | `ErrReadLimit` |
| `IncompleteReasonCoverage`, `IncompleteReasonCrashGap` | no sentinel: `Reason`, with `Coverage` or `Boundary`, classifies it |

Diagnostics that do not fail a call are read as a whole:
read "not finalized" from `PackHeader.Finalized` and `FooterErr`, not from the presence of a truncated defect,
since a forward walk that stops early, at a damaged envelope or at `MaxWalkedBlocks`, reports that stop and no separate defect for the end of the pack.
`Verify`'s `WriterDefect.Err` wraps `ErrFieldValidity` for `WriterDefectFieldValidity`,
and `ErrInvalidRecord`, the refusal a writer gives such a record, for `WriterDefectEventFieldValidity` and `WriterDefectEventPayload`;
the other kinds are descriptive, `Kind` classifying them, and none is `ErrCorrupt`, since the block holding it passed every check.

### Merge

| Error | Meaning | Action |
|---|---|---|
| `ctx`'s error | | stop |
| `ErrSourceRead` | a failed read of an input, whether `Merge` was opening it or reading a block: its `ReaderAt` returned an error, or a `ReadAt` did not return | may be transient: retry by your source's policy |
| `ErrInvalidArgument` | an option `Merge` cannot take, a `WriterTag` that is not valid UTF-8 beside a `*FieldError` naming `writer`, or a `ReplacementSetID` or `PackID` an input already uses | fix the options |
| `ErrMergeLimit`, `ErrReadLimit` | the archive over a budget of `MergeOptions` or a limit of the format, or an input over a budget of `MergeOptions.Reader` | raise the budget, or an operator decision (format limit) |
| `ErrMergeConflict` | records, or redaction entries, that differ | as `OnConflict` decided |
| `ErrMergeInput` | an input `Merge` refuses: the cause beside it, such as `ErrNotFinalized`, `ErrCorrupt`, `ErrNotTracepack`, `ErrUnsupportedFormat`, or the check it fails, says why | the input is unusable as it is: leave it out, or act on its cause by the reading calls' table |
| anything else | a block read of an input cut short after it was opened, or an error writing or syncing `dst` | may be transient |

A failed read is never `ErrMergeInput`, so a sound pack is not taken for a defective one;
`Merge` adds no class of its own to it, but your `ReaderAt`'s error keeps its chain, so test `ErrSourceRead` first.
an object shorter than its `MergeInput.Size`, whose `ReaderAt` reports the end with `io.EOF`, is `ErrMergeInput`.
After any error, discard what was written to `dst`.

### Bus messages

`bus.CheckMessage`, `bus.CheckPayload` and `bus.DeriveRecord` for the consumer;
`bus.EncodeBody` and `bus.CheckStart` for the producer, which publishes no message they refuse:

| Error | Meaning | Action |
|---|---|---|
| `bus.ErrMalformed` | never written, a layout-1 record of a kind layout 1 does not carry (kind 0 included) and a transport-event or annotation payload that does not decode included | the seq is a seq gap: terminate the message, alert |
| `bus.ErrUnsupportedLayout` | a `Tpk-Layout` this module does not know, from a newer producer | defer the message and alert, keeping a claim-check object it names, until a consumer that knows the layout takes it; never a seq gap |
| `bus.ErrNotAccepted` | a `start` whose descriptor cannot be accepted | alert, and defer the capture's later records until an operator acts |
| `bus.ErrPayloadMismatch` | a claim-check object whose length or digest differs | defer the message, or fetch the object again |
| `tracepack.ErrInvalidArgument` | your mistake, such as `bus.CheckPayload` on an inline record or `bus.CheckStart` with a `max_payload` below `bus.MinMaxPayload` | fix the caller |
| `tracepack.ErrInvalidRecord` | from `bus.DeriveRecord`, a classifier's `trailing_bytes` the record cannot hold | fix the classifier |
| anything else | | may be transient: retry |

Deploy consumers that know a layout before any producer emits it, so `bus.ErrUnsupportedLayout` defers messages and never drops them.

### Redaction

`redact.Compile`, `(*redact.Compiled).Mask` and `redact.Digest`:

| Error | Meaning | Action |
|---|---|---|
| `redact.ErrCannotScreen`, `redact.ErrRedacted`, `redact.ErrNotUnobserved` | a record `Mask` cannot screen, one already redacted, or one called unobserved that does not carry bit 7 | defer the record, never write it unscreened |
| `redact.ErrKeySize`, `redact.ErrInvalidPolicy`, `redact.ErrDomain` | an encryption key that is not 64 bytes, a policy `Compile` refuses, a domain `Digest` refuses | fix the configuration |
| `ctx`'s error | returned as it is | stop |
| anything else: an error of your `Keys`, returned wrapped | | may be transient: retry |

Every error but ctx's and your `Keys`' is deterministic: the same record under the same policy and keys fails the same way.

`redact.Restore`'s errors are deterministic, so none is retried:

| Error | Meaning |
|---|---|
| `redact.ErrNoEncryption`, `redact.ErrUnsupportedAlgorithm`, `redact.ErrNotRestorable` | the record cannot be restored under this policy, or holds no original |
| `redact.ErrKeySize` | wrong key material |
| `redact.ErrAuthentication` | a wrong key, or masked bytes or a siv that changed |
| `tracepack.ErrRedactionDefect` | the stored entries are defective: the error joins every listed defect, or every failed Marking check |

### Codecs and keys

The codecs report a fault of one TLV entry as a `*FieldError`, found with `var fe *tracepack.FieldError` and `errors.As(err, &fe)`:
`fe.Field` names the field, dotted for a nested tag such as `redaction.seq`,
and `fe.Err` wraps `ErrRequiredTag`, `ErrFieldLength` or `ErrFieldValue`, beside the encoding's own reason.
So do `MarshalBinary` and the `Unmarshal` functions of `PackMeta`, `TransportEvent` and `Annotation`, `EncodeDescriptor` and `DecodeDescriptor`,
and the writing calls that encode pack metadata or an event, which also mark the error `ErrInvalidArgument` and `ErrRefused`.
A decode also wraps `ErrCorrupt`, the bytes not being a valid encoding, and bytes that end inside an entry name no field;
an encode never wraps `ErrCorrupt`, and neither does a writing call refusing a record whose payload does not decode, which wraps `ErrInvalidRecord`.
`EncodeDescriptor(nil)`, and `ParseUUID` of a string that is not a canonical UUID, wrap `ErrInvalidArgument`;
the codecs and `Validate` otherwise return the field's sentinel without it, since only the call that takes a value as its argument says so.
`SegmentKey`, `ArchiveKey`, a `DirSink`'s `Create` and `CaptureDescriptor.Validate` refuse a key component no key can hold with `ErrInvalidKey`, which matches `ErrInvalidArgument`.

| Error | Meaning | Action |
|---|---|---|
| `ErrInvalidArgument`, `ErrInvalidKey` among it | an argument the call cannot take | fix the call |
| `ErrCorrupt` | bytes a decoder does not decode | report them |
| a `*FieldError` with `ErrRequiredTag`, `ErrFieldLength` or `ErrFieldValue` | a value the encoding or `Validate` refuses | fix the value |

### Errors that match no sentinel

Some errors are descriptive: no sentinel classifies them, and the field or the source named here does.

- A `Defect` of `IncompleteReasonCoverage` or `IncompleteReasonCrashGap`, and a `TxGap` of a reason other than `TxGapRead` and `TxGapIndex`:
  the `Reason` classifies it, with the fields it sets, such as `Coverage` or `Boundary`.
- A `WriterDefect` of a kind other than `WriterDefectFieldValidity`, `WriterDefectEventFieldValidity` and `WriterDefectEventPayload`:
  its `Kind` classifies it.
- An error of a block `Merge` builds itself, a new block of the resolution that fails its check or a block the archive's `Writer` refuses,
  reports a defect of this package, never one of an input: report it.
- The faults of a `DirSink`'s environment, under [Storing segments in a directory](#storing-segments-in-a-directory),
  are plain errors: retrying does not cure them.
- An error of your own implementation, an `io.Writer`, `Syncer`, `SegmentSink`, `SegmentFile`, `PackSource` or `redact.Keys`,
  is returned wrapped with its chain intact, its sentinels its own.
  A `ReaderAt`'s error is wrapped the same way and also matches `ErrSourceRead`, under [Reading calls](#reading-calls).
  So does a `ReadAt` the module calls on a goroutine of its own that ends the goroutine instead of returning, through `runtime.Goexit` as `t.FailNow` does:
  its bytes are never read as the pack's, and under `Merge` it is a failed read of that input, not `ErrMergeInput`.

## Compatibility

From tracepack v1.0.0 on, every v1 release keeps what the Godoc states as contract:

- the exported identifiers and their signatures;
- which sentinel error an error wraps, `ErrRefused` included;
- the outcomes, orders and determinism the Godoc states, and its byte-identity promises:
  the close `WriteClose` writes, the export `ExportJSONL` writes, and the output of `redact`'s `Mask`;
- reading every pack of format 1.x;
- the JSONL schema values `tracepack-jsonl/1` to `tracepack-jsonl/3`, each naming fixed bytes:
  any change to the bytes of an export takes a new schema value;
- the flags and exit codes of the `tracepack` command, its split of output between stdout and stderr, and the bytes of `dump --jsonl`.

These are not guaranteed, and may change in any release:

- the text of error messages,
  which follows one rule only: each line of the text of an error this module builds names the namespace `tracepack: ` at most once.
  A later `tracepack: ` on a line, one in the text of a wrapped error included, is left out of the text;
  the wrapped error itself is kept, and `errors.Is` and `errors.As` see it unchanged;
- how often a call checks `ctx`, how far a read reads ahead, how many goroutines a call runs and how many `ReadAt` calls it issues,
  the size of the pieces a write is made of, and how much larger than needed a reused buffer may be;
  where the Godoc or this page gives such a number, it says what the implementation currently does;
- the default values of the budgets, which may be tuned;
- the bytes zstd compresses to, across versions of this module and of the zstd library it uses;
- the text of the `verify` and `find` reports of the `tracepack` command.

Every enum may gain values in a later format minor, so a `switch` over one needs a `default` case.

A pack's redaction entries are part of its pack metadata,
which a `Reader` opens only within `ReaderOptions.MaxPackMetadataLen`, `DefaultMaxPackMetadataLen` (64 MiB) by default.
So 64 MiB is the default interoperability budget, since every reader of a deployment's packs must agree on the budget;
`Merge` refuses an archive whose pack metadata would exceed the budget of `MergeOptions.Reader`, with `ErrMergeLimit`.
The limit is in bytes, since an entry's size varies with its item path, domain and ranges:
64 MiB holds about 450,000 entries of 147 bytes, as an example.

## Specification

The documents in [docs/specs/tracepack/](../docs/specs/tracepack/) are the reference:
the format, the record semantics, the storage profile, the canonical JSONL export, and the mapping to this Go package (`tracepack-go.md`).

The conformance corpus in [testdata/corpus/](testdata/corpus/) holds packs and what a conforming reader produces from them:
exports, bootstrap rejections, header fields, footer checks and truncation tables.
Its packs are of format 1.0, 1.1 and 1.2, masked, encrypted and thinned packs and a consumer's close among them,
and its `primitives.json` publishes test keys with known-answer cases for the digests and AES-SIV.
An implementation in any language checks itself against the committed files, without Go;
its [README](testdata/corpus/README.md) says how,
and the [corpus specification](../docs/specs/tracepack/tracepack-corpus.md) defines every file.
