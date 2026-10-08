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
  Segments are staged; a merge later folds a scope's segments into one **archive**, the scope's next generation.
- The **store** is a bucket of packs under the keys of the storage specification §3,
  `staging/` for segments, `archive/` for archives and repair patches and `commit/` for commit objects,
  and a catalog that indexes them.
  A directory written by `NewDirSink` is laid out as such a bucket.

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
  Epoch 0 means unknown and marks the record `correlation-incomplete`, which keeps a transaction lookup from proving a reply absent.
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
`SegmentWriter`, `Merge` and `Repair` are built on it.
It validates each block before writing it unless `WriterOptions.SkipValidation` is set.
For every pack but an extract it refuses a period not inside one UTC hour, and a record of another hour, with `ErrScopeBreach`.

## Reading packs

For readers and troubleshooters working on local files:
`Open`, `Reader.Iterate`, `MergeIterate`, `FindTransaction` and `NewReaderSource`.

- `Open(ctx, ra, size, opts)` opens one pack over an `io.ReaderAt`, such as an `*os.File`, which stays open while the `Reader` is used.
  `Reader.Header` returns the pack metadata, and `Reader.Iterate` yields the records a `Query` selects, in file order.
  A damaged block does not fail the read: it is reported in the `Result`, beside the records that could be read.
  See `ExampleOpen`.
- `MergeIterate` reads several packs as one read, in capture or time order.
  It compares the copies of each record across the packs, yields identical copies once, and reports copies that differ as conflicts.
  See `ExampleMergeIterate`.
- An `Item` is valid only during the callback that receives it; clone what you keep.
- `Query.Retention` gives `Iterate` and `MergeIterate` the retention boundary of the storage specification §5:
  a `Retention` whose `RetainedFrom` returns the first UTC hour still retained, numbered as `HourOf` numbers it.
  Every hour before it is removed; nil removes nothing.
  A read covers the scope hour of each pack but an extract, whatever its records' timestamps.
  For an extract it covers the hours of its indexed blocks' F-2 `ts_min` and `ts_max`,
  known before any block is read, pruned blocks included,
  and the hour of every record it decodes, filtered out or not; an extract's period does not count.
  It asks the provider before it reads, before the records of each block reach your callback,
  after a failed read, and before it returns success.
  When a covered hour is removed, the whole read ends with an error wrapping `ErrRemoved`,
  and `Result.Removed` lists the removed hours it covers.
  Discard everything the read reported of those hours, records already passed to your callback included:
  their objects may have been deleted while it ran.
  `Result.Complete()` is false while `Removed` is non-empty.
- `FindTransaction(ctx, src, key, opts)` looks up the reply to a primary message, named by a `TxKey`;
  `TxKeyOf(captureID, r.Seq, time.Unix(0, r.TSUTCNs))` builds the key from the primary's record.
  It reports `TxMatched`, `TxAmbiguous`, `TxUnmatched` or `TxIncomplete`,
  with every reason it could not establish absence as a `TxGap`.
  See `ExampleFindTransaction`.

### A lookup over local files

`NewReaderSource(readers, opts)` gives `FindTransaction` a source over packs you opened, all of one tool.
It groups them into scopes and computes each scope's view, taking every generation and patch given as committed:
give an archive or a repair patch only if it was accepted.

- Without `Complete`, every scope is not indexed and every capture's evidence is partial:
  each scope a lookup reads is a `TxGapCold` gap, and a lookup is never `TxUnmatched`.
  That is the safe default for a handful of files.
- `Complete` is an assertion about evidence, never "all the files I found".
  It asserts that the readers are every accepted pack of the tool, of every capture and every hour,
  and that the readers and `Evidence` together are every pack a catalog of the tool would hold evidence of,
  rejected registrations included.
  A set of files from which a pack was removed, by retention or otherwise, cannot be `Complete`.
  Only then can the source vouch for the `stop-unclean` barriers of the tool's other captures.
- `Evidence` holds the segments and converter archives whose registration was rejected.
  They count toward their capture's evidence and barriers, but never enter a scope's view.
  Under `Complete` the scope of each `Evidence` reader, its capture in the hour of its period, is not indexed,
  as a catalog that rejected the registration reports it, so a lookup reading it gets a `TxGapCold` gap.
- An `Evidence` reader's period must lie inside one UTC hour, which names its scope;
  `NewReaderSource` refuses one that does not with `ErrInvalidQuery`.
- Even with `Complete`, every capture's evidence is partial while any reader of the tool has no usable footer.
- The source never closes the readers' files: keep them open until the lookups are done.

## Operating a store

For the storage service:
the key builders, `ActiveView`, `Merge`, `Verify`, `Repair` and `NewStoreSource`.

- `SegmentKey`, `ArchiveKey` and `CommitKey` build the object keys of the storage specification §3,
  and `EscapeToolID` spells a tool as a key does.
- `ActiveView` computes a scope's view from a complete observation of its packs and commit objects:
  its current generation, the packs a read uses, the lineage the next merge writes, and the packs that may be deleted.
- `Merge` writes a scope's next generation, one archive, from the packs of its view.
  Publish the archive only after `Merge` returned nil, under the commit protocol of the storage specification §5.
- `Verify` reads every block of a pack and reports its outcome, from finalized-consistent to corrupt;
  `Repair` writes a repair patch of a damaged pack, which the next merge folds in.
- `NewStoreSource` gives `FindTransaction` a source over an `ObjectStore`, the bucket, and a `Catalog`, your catalog of one tool.
  Their Godoc states the contracts both adapters must keep.

### Retention

Retention removes whole UTC hours: every hour before a boundary that only moves forward (the storage specification §5).
Deleting the packs and commit objects of a removed hour is the storage service's job;
this package applies the boundary on the reader side, through a `Retention` provider.
Give `StoreSourceOptions.Retention` and `TxOptions.Retention` the same provider:

- `StoreSourceOptions.Retention` makes `Observe` tolerate deletion.
  It reports a removed hour as `SourceScope.Removed`, with no readers,
  instead of failing on its deleted objects or forming a view from a half-deleted hour.
  Given alone, it leaves the lookup unprotected after `Observe` returns.
- `TxOptions.Retention` makes the lookup's guarantee:
  the lookup checks the primary's hour, each scope read checks the hours it covers,
  and a lookup whose primary's hour is removed fails with an error wrapping `ErrRemoved` instead of returning an outcome.
  Given alone, a deletion during `Observe` can fail `Observe`;
  the lookup then checks the boundary again, and returns `ErrRemoved` when the primary's hour is removed.
- A scope the source reports `Removed` ends the lookup with `ErrRemoved`, with or without `TxOptions.Retention`.

A provider must keep this contract:

- It never falls behind the deleting component:
  before the component deletes any object of an hour, every provider a reader asks already reports the hour removed,
  so a read's check sees every hour whose objects may be gone.
  A provider that runs ahead of the deletions is safe.
  A cached value, such as a boundary the component publishes for readers in other processes to load,
  is safe only if the component waits out the cache's staleness before it deletes:
  publishing the boundary first is not enough on its own.
- The boundary never moves back.
- It is safe for concurrent use.
- It is cheap, an atomic load rather than a remote call: a read asks it at every block it emits.

```go
// retainedFrom is a Retention over the first UTC hour retained, as tracepack.HourOf numbers it.
type retainedFrom struct{ hour atomic.Int64 }

func (r *retainedFrom) RetainedFrom(context.Context) (int64, error) { return r.hour.Load(), nil }

// advance moves the boundary to hour, never back.
// The deleting component advances every reader's provider to hour before it deletes any object of the hours before hour;
// a reader that loads a published copy of the boundary must report hour too,
// so the component waits out the copy's staleness before it deletes.
func (r *retainedFrom) advance(hour int64) {
	for {
		cur := r.hour.Load()
		if hour <= cur || r.hour.CompareAndSwap(cur, hour) {
			return
		}
	}
}
```

## Specification

The documents in [docs/specs/tracepack/](../docs/specs/tracepack/) are the reference:
the format, the record semantics, the storage profile, and the mapping to this Go package (`tracepack-go.md`).
