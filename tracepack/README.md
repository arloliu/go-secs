# tracepack

`github.com/arloliu/go-secs/tracepack` is the Go reference implementation of tracepack (`.tpk`):
an immutable, compressed, self-indexed container for recorded SECS-II traffic between one tool and its host,
HSMS control messages and transport lifecycle events included.

```sh
go get github.com/arloliu/go-secs/tracepack
```

The module is versioned on its own, with tags `tracepack/vX.Y.Z`.
It is pre-v1: its API may still change between minor versions, as [CHANGELOG.md](CHANGELOG.md) records.
The specification in [docs/specs/tracepack/](../docs/specs/tracepack/) defines every field and rule;
where this package and the specification disagree, the specification wins.
This page is a map by audience; the Godoc states each contract in full.

## Concepts

- A **record** is one observation: a captured HSMS frame (a data or control record), a transport event, or an annotation.
- A **capture** is one run of one recorder over one tool, named by its `capture_id`.
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

## Recording a capture

For a recorder, or a converter of logs:
`CaptureDescriptor`, `SegmentWriter`, `Classifier`, `SegmentSink` and `NewDirSink`.

A `CaptureDescriptor` holds the pack metadata the producer owns:
the tool, the transport, how and where the capture was taken, the recorder's identity and the time source.
`NewSegmentWriter` checks it with `CaptureDescriptor.Validate` and writes the capture's `start` boundary;
from then on it writes the records you append as segments, through a `SegmentSink`:

- `AppendFrame(ctx, at, dir, epoch, frame)` for a captured HSMS frame, length prefix included,
  classified by the `Classifier` when one is set;
- `AppendEvent(ctx, at, epoch, ev)` for a transport event;
- `Append(ctx, r)` for a record you built yourself;
- `Tick(ctx, now)` and `Rotate(ctx)` to commit the open segment;
- `Close(ctx)` to write the `stop` boundary and commit the last segment.

The open segment is committed when its period ends and the next record or a `Tick` shows it,
when the next record lies in another hour,
when it reaches `MaxSegmentBytes` (a soft limit),
and on `Rotate`.
`classify.New(maxFrameLen)` returns the go-secs-backed classifier;
a frame longer than its ceiling is recorded with decode status `oversized` and is not decoded.
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
  it aborts the open segment, every later call returns an error wrapping `ErrWriterFailed`, and `Close` returns the failure.
  `Close` also fails it when no seq is left for the `stop` boundary:
  it commits the open segment, if any, without `stop`, then returns an error wrapping `ErrSeqOrder`.
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

`NewDirSink(root, prefix)` stores each segment as the file `<root>/<SegmentKey(prefix, …)>`,
so a recording can be uploaded to a bucket as it is, its file paths for object keys.

- `root` must be an existing directory, and its own durability is yours.
- The sink opens `root` as an `os.Root` and makes every file-system call through it,
  so nothing it writes lies outside `root`:
  a symbolic link leading out of `root`, such as `root/staging` or `root/.partial`, fails the call that meets it.
  The sink keeps `root` open for as long as it is reachable; there is nothing to close.
- The sink refuses a symbolic link inside `root` where it manages directories,
  so no link, such as `root/staging` leading to `.partial`, makes the temporary files and a key area one directory:
  `.partial` itself fails `NewDirSink` or `Create`,
  and any directory of a key, `prefix` included, fails `Commit` before the rename, nothing published.
  These checks guard against a misconfigured `root`,
  not against a process that writes to `root` while the sink runs, which can remove segments anyway.
- `NewDirSink` fails on `js`, where `os.Root` cannot exclude a link replaced after its check.
- Files are created with mode 0640 and directories with mode 0750, less the process umask.
  No other user reads a segment, which holds records in full;
  a group, such as one a setgid `root` gives every new file, can give an uploader read access.
- A segment is written under `<root>/.partial/` and renamed to its key on commit;
  every directory from the key's up to `root` is then synced, except on Windows, which cannot sync a directory.
  A listing of the key areas sees only committed segments.
  A file a crash left under `.partial/` is never listed;
  remove such leftovers only while no writer uses `root`.
- `NewDirSink(root, prefix, DirSinkNoReplace())` never replaces a file at a key, as reproducible output needs:
  it hard-links a segment to its key instead of renaming it,
  and when the key exists, `Commit` returns an error wrapping `ErrSegmentExists` for identical bytes
  or `ErrSegmentConflict` for others,
  the existing file kept.
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
It validates each block before writing it unless `WriterOptions.SkipValidation` is set.
For every pack but an extract it refuses a period not inside one UTC hour, and a record of another hour, with `ErrScopeBreach`.

## Reading packs

For readers and troubleshooters working on local files:
`Open`, `Reader.Iterate`, `MergeIterate`, `FindTransaction`, `NewReaderSource`, `ExportJSONL` and `Verify`.

- `Open(ctx, ra, size, opts)` opens one pack over an `io.ReaderAt`, such as an `*os.File`, which stays open while the `Reader` is used.
  `Reader.Header` returns the pack metadata, and `Reader.Iterate` yields the records a `Query` selects, in file order.
  A damaged block does not fail the read: it is reported in the `Result`, beside the records that could be read.
  See `ExampleOpen`.
- A pack of up to `ReaderOptions.WholeReadThreshold` bytes, 4 MiB by default, costs one `ReadAt` for `Open` and none for any later read of it:
  the `Reader` keeps the bytes of `Open`'s first round that hold blocks, the whole object or else the head and tail windows,
  and copies each block it reads from them when they hold it whole.
  It keeps at most max(`WholeReadThreshold`, `HeadWindow` + `TailWindow`) bytes for its lifetime, 4 MiB with the defaults,
  outside the budgets of `MergeIterateOptions.MaxHeldBytes`, `TxOptions.MaxHeldBytes` and `TxOptions.MaxStateBytes`;
  lower `WholeReadThreshold` when many Readers stay open at once.
- `MergeIterate` reads several packs as one read, in capture or time order.
  It compares the copies of each record across the packs, yields identical copies once, and reports copies that differ as conflicts.
  It reads up to 8 blocks ahead of the one it resolves, within `MaxHeldBytes`,
  in time order, and in capture order when at most one capture has a cluster to read,
  so it may issue several `ReadAt` calls at once on a Reader's `io.ReaderAt`, as `Open` does;
  whatever its outcome, it waits for every call it issued before it returns.
  See `ExampleMergeIterate`.
- An `Item` is valid only during the callback that receives it; clone what you keep.
- A read reports a **crash gap**, a `ReasonCrashGap` defect whose `Defect.Boundary` holds the boundary,
  for every `stop-unclean` capture boundary whose gap meets the query's time range (the semantics specification §7.4):
  records of that time may be missing.
  It takes them from the footer of every pack whose footer it uses, whatever the filter excludes,
  and from the boundary records of the blocks it walks in a pack without one.
- `FindTransaction(ctx, src, key, opts)` looks up the reply to a primary message, named by a `TxKey`;
  `TxKeyOf(captureID, r.Seq, time.Unix(0, r.TSUTCNs))` builds the key from the primary's record.
  It reports `TxMatched`, `TxAmbiguous` or `TxNotFound`,
  and lists in `TxResult.Incomplete`, beside any outcome, every reason the lookup is incomplete, each a `TxGap`.
  `TxNotFound` states nothing about records outside the packs it read.
  See `ExampleFindTransaction`.
- `ExportJSONL(ctx, r, w)` writes the canonical JSONL export of one pack,
  schema `tracepack-jsonl/1` of the [JSONL specification](../docs/specs/tracepack/tracepack-jsonl.md):
  a header line, then one line per record, the same bytes any conforming exporter writes for the pack.
  A damaged pack exports the records of its validated blocks, and the returned `Result` reports what was lost exactly as `Iterate`'s does:
  a failed block or a crash gap makes it incomplete, while a footer that is not used, with every block found by the forward walk, only sets `FooterErr`.
  A reader budget that keeps part of the pack out fails the export with an error wrapping `ErrReadLimit` instead;
  after any error, discard what was written.
  Each line is written in pieces of at most 64 KiB, so one line may take several `Write` calls.
  See `ExampleExportJSONL`.
- `Verify` reads every block of a pack and reports its outcome, from finalized-consistent to corrupt.

### A lookup over local files

`NewReaderSource(readers)` gives `FindTransaction` a source over packs you opened, all of one tool.
It groups them by the UTC hour of their period, whatever their capture,
and a lookup reads, for each hour it searches, every pack of that hour.

- Give every pack the lookup should read, segments and archives alike:
  a pack not given is not read, and the lookup states nothing about its records.
  A segment and an archive holding the same records may both be given; their copies are compared and yielded once.
- A pack of any role but `extract` is accepted, if its period is not empty and lies inside one UTC hour, the hour it is read for.
- `NewReaderSource` refuses, with `ErrInvalidQuery`, a nil reader, a `Reader` given twice, two readers of one `pack_id`,
  readers of more than one tool, an `extract`, and a pack whose period is empty or spans hours.
- The source never opens or closes the readers: keep them open until the lookups are done.

## Merging and storage

For the storage service:
`Merge` and the key builders.

- `SegmentKey` and `ArchiveKey` build the object keys of the storage specification §3,
  and `EscapeToolID` spells a tool as a key does.
- `Merge(ctx, dst, inputs, opts)` writes one archive of a scope from the segments and archives you give it, of one capture and one UTC hour,
  with the `ScopeGeneration` and `PublisherEpoch` you set in `MergeOptions`, which it compares with no input's.
  Its `compacted_from` lists the inputs' own lineage and then every segment input, and its `supersedes` every archive input, in the order you give them.
  It refuses with `ErrMergeInput`, before writing anything, an input whose role is neither `segment` nor `archive`, and an input of another capture or hour.
  Which packs a merge takes, and when a merged pack may be deleted, is yours to decide (the storage specification §4);
  publish the archive only after `Merge` returned nil.

## Specification

The documents in [docs/specs/tracepack/](../docs/specs/tracepack/) are the reference:
the format, the record semantics, the storage profile, the canonical JSONL export, and the mapping to this Go package (`tracepack-go.md`).

The conformance corpus in [testdata/corpus/](testdata/corpus/) holds packs and what a conforming reader produces from them:
exports, bootstrap rejections, header fields, footer checks and truncation tables.
An implementation in any language checks itself against the committed files, without Go;
its [README](testdata/corpus/README.md) says how,
and the [corpus specification](../docs/specs/tracepack/tracepack-corpus.md) defines every file.
