# tracepack — Go reference implementation

Status: current (2026-10-07)
Implements tracepack v2.25 (format 1.0): `tracepack-format.md` [FMT], `tracepack-semantics.md` [SEM], `tracepack-storage.md` [STO]; overview in `tracepack-overview.md`.
Where this document and the normative tracepack documents disagree, the normative tracepack documents wins.
Citations name a file and symbol in `github.com/arloliu/go-secs/v2` on `main`;
line numbers are avoided because they drift.

## 1. Placement and dependencies

- Nested Go module `github.com/arloliu/go-secs/tracepack` in the repository's `tracepack/` directory, with its own `go.mod` and its own tags `tracepack/vX.Y.Z` (G5-77; precedent `benchmarks/go.mod`).
  The path carries no `/v2`: for a module in `tracepack/`, the go command resolves `github.com/arloliu/go-secs/v2/tracepack` to the directory `v2/tracepack/`, so that path would not be fetchable outside the repository.
- The go-secs dependency carries no `replace` directive, and `go.work` stays local (G5-78; `tracepack-impl-plan.md` §2.1).
  A released tracepack requires a released go-secs, at least v2.5.0 (G5-79).
- Reference CLI `tracepack/cmd/tracepack` in the same module is the explicit exemption from the root "no binary" rule.
- The TAP log converter lives in `veq/tools/tapconv`.
- Dependencies: `github.com/klauspost/compress/zstd` (pure Go; it offers no seekable format, so independent block bodies are the design)
  and `hash/crc32` (`crc32.ChecksumIEEE` is the spec's CRC-32/ISO-HDLC). No CGO.

## 2. Recorders and their capture descriptors

How each Go-side producer fills the spec's capture model ([SEM §2]):

| Producer | `capture_method` | `vantage` | default `fidelity` | `recorder` example |
|---|---|---|---|---|
| a go-secs application recording the frames `hsms.WithWireObserver` reports (go-secs v2.6.0 or later), before HSMS decoding | `raw-stream` | `host` or `equipment`, by the application's role | `wire-exact` | `<app>/<ver> go-secs/2.6.0` |
| eqp-hub, frames re-emitted via `ToBytes()`: today the `eqp_hsms` device → `hsms_secsjson` adapter → `tap_nats` device → JetStream path; a recorder device on the go-secs v2.6.0 observers is planned, not built | `decoded-message` | `intermediary` | `re-encoded` | `eqp-hub/<ver> tap_nats` |
| `tapconv` converting TAP SML logs | `log` | where TAP ran (normally `host`) | `reconstructed` | `tapconv/<ver>` |
| VE or test fixtures producing expected traffic | `generator` | `none` | `synthesized` | `veq/<ver>` |

A recorder on the go-secs observers also installs `hsms.WithSocketObserver`
and assigns `epoch` ([FMT I-7]) from the `Socket` value every go-secs observer event carries (§5):
a per-capture `u32` counter keyed by the connection and its `Socket` values, starting at 1,
because `Socket` is unique only within one connection.
In the durable-bus deployment ([STO §4]) the producer also assigns `capture_id`, `recorder_instance_id` and `seq`, and the writer stores them as received.
A producer without socket identity writes `epoch = 0` and `correlation-incomplete`.
As of 2026-09-28, go-secs v2.6.0 (release pending) exposes what the eqp-hub path lacked:
socket and generation identity on wire, socket, lifecycle and transaction events,
the wire and socket observers,
and event times that carry a monotonic reading (`WireEvent.At`, `SocketEvent.At`, `LifecycleEvent.At`).
eqp-hub itself still runs go-secs v2.3.0 and has no recorder device:
its path carries no generation id, no start/stop event or instance id,
and only a wall-clock string taken when the adapter converts the message (`SourceTimeStamp`, host-local time zone, no monotonic reading),
so its records get `epoch = 0`, `correlation-incomplete` and no monotonic time (`mono_present` clear) until eqp-hub moves to go-secs v2.6.0 and records through the observers.
Recorders flush `segment` packs to the staging tier; the merger (a service component using `Merge`) writes `archive` packs ([STO]).
The query service, its catalog database and the live-tail interface are designed separately.

## 3. API sketch (non-binding)

- `Record` mirrors the spec's record header (`Mono` nil when `mono_present` is clear).
  A helper returns each HSMS header field from the payload as a value and an availability, separately ([FMT §7.2]);
  it reports a field unavailable when its `field_validity` bit is clear, when the payload lacks that field's bytes, and for kinds without an HSMS frame,
  so a short frame still yields the fields it holds.
  A second helper computes the `field_validity` of a raw frame from its length, for writers of raw captures.
  `DecodeStatus.Malformed` is true only for known malformed values, and `DecodeStatus.Clean` only for known values outside the malformed set;
  an unknown value is neither, so `!Malformed()` never proves a clean decode ([SEM §6]).
- `Writer`: `NewWriter(w io.Writer, WriterOptions)`, the layer under `SegmentWriter`, `Merge` and `Repair`;
  options: the pack metadata, codec (its zero value is `none`, the wire value 0, so the zstd default lives in `SegmentWriterOptions`), block size threshold,
  and validation, on unless `SkipValidation` is set (G5-152): a validating writer decodes each encoded block, gathers its record headers and checks I-2 before writing it,
  and on a failed check returns an error without writing the block or the trailer ([FMT §12]); nothing is committed to the pack metadata.
  The writer transposes the record headers into the header section when it assembles a block body ([FMT §6]);
  it may feed the header and payload buffers to the encoder in turn instead of concatenating them.
  `PackMeta` carries `capture_id`, the capture's next seq and every pack metadata tag;
  the `Writer` rejects `pack_role` 5 and replacement-set values other than size 1 and index 0,
  and drops retired tag numbers from the unknown entries it preserves ([FMT §5]).
  For every pack but an `extract`, `NewWriter` refuses a period that is empty or not inside one UTC hour,
  and `Append(*Record)` refuses a record whose `ts_utc_ns` lies in another UTC hour than the period with `ErrScopeBreach`, the Writer still usable (G5-152);
  a record of the period's hour outside the period is accepted, since a same-hour clock step puts it there ([STO §4] Merge).
  `Append` assigns the capture-scoped `Seq` ([FMT I-12])
  and closes the current block before a record from another UTC hour ([FMT I-13]), which only an `extract` holds;
  it rejects a record whose set `field_validity` bit names bytes its payload lacks ([FMT §7.2]).
  `Flush`; `Close` writes footer + trailer ([FMT §12]) and returns the capture's next seq for the following segment.
- `CaptureDescriptor` (G5-153): the pack metadata [STO §4] gives the producer of a capture, written unchanged into every pack of it:
  `ToolID`, `Transport`, `CaptureMethod`, `Vantage`, `Recorder`, `RecorderInstanceID` (required, never generated: it links the deployment's captures across restarts, [FMT I-7]),
  `PreviousCaptureID`, `TimeSource`, `CaptureOrigin` (`time.Time`; zero takes the time `NewSegmentWriter` runs),
  `ClockStepTolerance` (`time.Duration`; zero takes a documented default), `LifecycleCoverage`, `QualityEvaluated`,
  the log source fields and the optional site and equipment fields.
  `capture_origin_utc_ns` is `CaptureOrigin`'s wall clock and `capture_origin_mono_ns` is 0 ([SEM §4], G5-151).
  `Validate()` checks the producer-side "Required when" rows of [FMT §5] without `PackFacts`.
  The Godoc of `QualityEvaluated` names what `SegmentWriter` evaluates (`correlation-incomplete` for epoch 0, the capture-boundary bit)
  and what the producer evaluates before it sets the field (`ordering-uncertain`, `direction-inferred`, [SEM §6]).
  Every `PackMeta` field gains Godoc from the registry's *Meaning* column and the roles that require it.
- `SegmentWriter` (G5-153): `NewSegmentWriter(ctx, SegmentWriterOptions) (*SegmentWriter, error)` writes one capture, in capture order, as segments ([STO §4] Flush),
  over a sink without the durable spool (G5-150), so its segments carry no `flush_interval_ns`.
  It owns `writer` (from the module's build info, with the fallback `classify.Name` uses), `classifier`, `max_frame_len`, `period_start`, `period_end`, `seq_start`,
  `pack_role = segment`, `compaction_level = 0` and `scope_generation = 0`, a `capture_id` it generates, and the capture's seq counter.
  `CaptureID()` returns that `capture_id` from construction on, so a recorder can persist it as the next capture's `PreviousCaptureID`.
  Options: `Capture` (a `CaptureDescriptor`, required), `Sink` (required), `FlushInterval` (divides one hour; default 5 minutes),
  `MaxSegmentBytes` (default 64 MiB), `Classifier` (optional), `Uncompressed` (false writes zstd, true writes `none`), `BlockThreshold`, `SkipValidation`,
  and `Now` (default `time.Now`, for tests).
  Every method that can create, commit or abort a segment takes a `ctx`:
  `AppendFrame(ctx, at, dir, epoch, frame)`, `AppendEvent(ctx, at, epoch, ev)`, `Append(ctx, r)`, `Tick(ctx, now)`, `Rotate(ctx)` and `Close(ctx)`;
  the writer stores none, checks `ctx` before its first mutation (a done `ctx` there returns its error, the writer still usable),
  passes it to the sink's `Create` and `Commit`, and checks it between sink calls.
  - Times: a time `t` is representable when `time.Unix(0, t.UnixNano()).Equal(t)` and its whole flush interval, start and exclusive end, fits in `int64` nanoseconds;
    nothing is clamped. A `CaptureOrigin` that is not representable fails `NewSegmentWriter`,
    an appended `at` that is not fails the append before any mutation (`ErrInvalidRecord`),
    and a `Now()` at `Close` that is not fails `Close` terminally: it aborts the open segment and writes no `stop`.
  - Opening: a segment is opened by the first record that needs one; its period is the flush interval holding that record's `ts_utc_ns`,
    inside the record's UTC hour because the interval divides the hour; its `seq_start` is the capture's next seq and its `pack_id` a fresh UUIDv7.
  - Boundaries: `NewSegmentWriter` appends the capture's `start` boundary at `CaptureOrigin`, so the first segment opens with it,
    and `Close` appends `stop` at `Now()` before it finalizes the last segment; both have epoch 0 and no seq range ([SEM §5], G5-149).
    Each is a transport-event record, dir `local`, fidelity and decode status `not-applicable`;
    `stop` carries `mono_ns` only for a `capture-clock` descriptor when `Now()` and `CaptureOrigin` both carry a monotonic reading, as `AppendFrame` sets it, and `start` carries `mono_ns` 0 for such a descriptor when `CaptureOrigin` carries one.
    The writer owns them: `AppendEvent` and `Append` refuse a capture-boundary of kind `start`, `stop` or `stop-unclean` with `ErrInvalidRecord`,
    since a second `stop` would close epochs early; a `gap` boundary is accepted.
  - Rolls, each finalizing and committing the open segment before the record is appended:
    a record of another UTC hour than the period (by its `ts_utc_ns`, either direction; mandatory);
    a record of the same hour at or after `period_end`;
    a segment that reached `MaxSegmentBytes`;
    `Tick(ctx, now)` with `now` at or after `period_end`, so an idle segment is not held open;
    `Rotate(ctx)`.
    The size counts the bytes the segment's `Writer` has handed to the sink — file header, pack metadata and closed blocks, not the open block —
    and is checked before each record the caller appends, so the limit is soft:
    a segment can pass it by its open block, the record that closes that block and its footer,
    and a record larger than the limit shares its segment with the records before it (the `Writer` gives it a block of its own, not a segment).
    A `clock-step` record the writer inserts is not checked on its own: it goes into the segment of the record it precedes.
    `Tick` and `Rotate` with no open segment do nothing; the next record opens a segment for its own period.
    A record of the period's hour before `period_start`, after a backward clock step, stays in the open segment.
  - Clock steps: for a `capture-clock` descriptor it applies the anchor rule of [SEM §4] to every record with a monotonic reading,
    and the anchor in force carries over from one segment to the next, so a new segment never re-detects a step an earlier one recorded.
  - Records: `AppendFrame(ctx, at time.Time, dir Dir, epoch uint32, frame []byte)` writes a data or control record of a captured frame,
    for a capture method whose bytes establish the fields they hold: `raw-stream`, `decoded-message` and `generator`.
    For a `log` capture it fails without writing, since a reconstructed frame holds placeholder bytes for identities its source lacks ([FMT §7.2]);
    a log converter uses `Append` with the record's `field_validity`, `quality` and `decode_status` set by the converter.
    The kind is control when the frame holds its SType byte and that byte is not 0, a defined SType or not, and data otherwise, a frame too short to hold SType included;
    `field_validity` follows from the frame's length (`SetCapturedFieldValidity`), so a short frame's missing fields stay unavailable;
    `fidelity` is by [SEM §2] from the capture method (`wire-exact` for `raw-stream`, `re-encoded` for `decoded-message`, `synthesized` for `generator`);
    `ts_utc_ns` is `at`'s wall clock; for a `capture-clock` descriptor, `mono_ns` is `at` − `CaptureOrigin` when both carry a monotonic reading,
    and for any other time source, or without both readings, `mono_present` is clear ([SEM §4]);
    `decode_status` and `trailing_bytes` come from the classifier when one is set, which classifies a short or malformed frame as [SEM §3] says, and are `not-attempted` and 0 otherwise.
    `AppendEvent(ctx, at, epoch, *TransportEvent)` marshals the event into a transport-event record;
    `Append(ctx, *Record)` takes a record the producer built, its `Seq` assigned by the writer and every other field as given; the classifier is not applied to it.
    Each copies what it keeps, so the caller may reuse its buffers when it returns.
  - Classifier: an interface, `Name() string`, `MaxFrameLen() uint64` (0 for none) and `Frame([]byte) (DecodeStatus, int)`,
    which `tracepack/classify` implements, so the root package does not import go-secs (impl plan §2);
    setting one writes `classifier` and, when `MaxFrameLen` is not 0, `max_frame_len` into every segment, so the caller declares no `PackFacts`.
  - Refusal: each append first checks the record without touching a segment —
    `at` representable, payload size (`ErrPayloadTooLarge`), field validity against the payload (`ErrFieldValidity`),
    the commitments the segments' metadata makes (`ErrMetadataCommitment`), the boundary kinds above (`ErrInvalidRecord`), the event's encoding,
    a transport-event or annotation record whose payload does not decode as [FMT §8] defines it or whose `field_validity` is not 0 (`ErrInvalidRecord`, G5-160),
    so `Verify` finds no writer defect in a record the writer appended,
    and seq capacity (`ErrSeqOrder`): one seq, or two when a clock-step would precede the record, up to 2^63 − 1.
    A refused record is not added, no segment is rolled or opened, and the writer stays usable;
    so a segment is opened only for a record that will be appended.
    `Close` with no seq left for `stop` finalizes the open segment without it and fails.
  - Failure: after that check, any error — of the segment's `Writer`, whatever its sentinel, of the sink, or `ctx`'s — fails the `SegmentWriter`
    and aborts the open segment exactly once, whether or not the error came from `Commit`.
    Before the sink's `Commit` renames or uploads, a failure publishes nothing of the open segment, whose records are lost;
    once `Commit` made the segment visible but before it is durable, the error wraps `ErrPublishUncertain` beside its cause, cancellation included:
    the segment is published and may not survive a system crash.
    Once `Commit` returned nil the segment is detached, so a later failure leaves it published and aborts nothing.
    The writer calls `Commit` at most once and `Abort` at most once on a segment, `Abort` exactly once after a failed `Commit`, uncertain or not;
    the error it returns joins the cause first and any `Abort` error after it.
    The seq counter is never rewound, so a loss is a seq gap, never a reused seq.
    `Close` after a failure makes no further sink call and returns the stored failure.
  A `SegmentWriter` is not safe for concurrent use.
- `SegmentSink` (G5-153): `Create(ctx, SegmentInfo) (SegmentFile, error)`;
  `SegmentFile` is an `io.Writer` with `Sync()` (called after every block and after the trailer), `Commit(ctx)` (after a successful `Close`) and `Abort()` (after any failure).
  `Write`, `Sync` and `Abort` take no context: the sink bounds each by its own deadline, as an `Object` bounds `ReadAt`.
  A `Commit` that fails after the segment became visible returns an error wrapping `ErrPublishUncertain`.
  `SegmentInfo` holds the `tool_id`, `capture_id`, `pack_id`, `seq_first` of the segment key ([STO §3], known at open), the hour and the period,
  so a sink derives the key from the segment it writes and never from a tool of its own.
  `NewDirSink(root, prefix string)` writes `<root>/<SegmentKey(prefix, info.ToolID, …)>`;
  it refuses, before creating any file, a prefix that is not empty or a relative `/`-separated path whose components are none empty, `.` or `..`,
  and one holding `\` or a `:`, so no platform reads it as another separator or a volume,
  and one whose first component is `.partial`, the directory of its temporary files, so no committed segment lies among them (G5-157),
  or, once it has created `<root>/.partial/`, names that same directory (`os.SameFile`), as a case variant does on a case-folding file system,
  and uses an accepted prefix as given, never cleaned, so the file path spells the object key;
  so a local recording is laid out as the bucket is ([STO §3]).
  `root` must exist, and its own durability is the caller's; `NewDirSink` creates `<root>/.partial/` and syncs `root`.
  It performs every file-system operation through `root` opened as an `os.Root`, so no path, `.partial/` included, resolves outside `root`:
  a symbolic link leading out of `root` fails the operation that meets it (G5-159).
  It also refuses a symbolic link inside `root` where it manages directories:
  `<root>/.partial` when it creates the sink and each segment, and every directory component of a key, checked after creating them and before the rename,
  so a link cannot alias the temporary directory and a key area; such a link fails `NewDirSink` or `Create`, or `Commit` with a plain error, nothing published (G5-161).
  These checks guard against a misconfigured root, not against a process that writes to `root` while the sink runs, which can remove segments anyway.
  `NewDirSink` fails on `js`, where `os.Root` cannot exclude a link replaced after its check.
  It creates files with mode 0640 and directories with mode 0750, so no other user reads a segment, which holds records in full ([STO §3]),
  while a group, such as one a setgid `root` gives every new file, can grant an uploader read access (G5-157).
  It writes each segment to a temporary file under `<root>/.partial/`, outside every key area of [STO §3] and on the same filesystem.
  `Commit` syncs and closes the temporary file, creates the key's missing directories, renames the file to its key,
  then syncs every directory from the key's directory up to `root`, deepest first, `root` included, on every commit,
  so a directory a failed attempt or another sink created is made durable by the next successful commit;
  an error after the rename wraps `ErrPublishUncertain`. Directory syncs are skipped on Windows, which cannot sync a directory.
  A listing of the key areas sees only committed segments, never an open or abandoned one;
  `Abort` removes the temporary file, ignoring one already gone, and a temporary file left by a crash stays under `.partial/`, which no listing reaches.
- `Reader`: `Open(ctx, ra io.ReaderAt, size, opts)` performs the [FMT §13] bootstrap, optionally seeded with a catalog footer location;
  `Header()`, `Blocks()` (F-2), `Iterate(ctx, Query, fn) (Result, error)`,
  and `Stats() (PackStats, bool)` (G5-131): the F-5 statistics of a pack whose footer it uses — counts, time and seq ranges, per-epoch summaries with `close_seq`, capture-boundary entries — as a fresh value, false without a used footer.
  `AddPackEvidence(*CaptureEvidence, *PackStats)` folds one pack's boundaries and epoch closures into a capture's per-capture evidence ([STO §5] Per capture): boundaries sorted and exact duplicates dropped, the smallest `close_seq` per epoch, the end state recomputed, and `Partial` set for a pack without statistics; which packs to fold is the catalog's registration policy.
  `Open` parses the redaction entries with the pack metadata and runs their structural checks; its cost grows with the entries.
  Records expose `Redacted` and their masked ranges, validated against the record when it is read ([SEM §8] Marking);
  a wholly masked record reports its defect, and `Header()` exposes the policy, the entries and any bootstrap defect.
  Every block read decodes the whole body and checks I-2 before any record is used ([FMT §6]);
  the reader untransposes the decoded header section into a reusable row buffer, after which every record-level path reads headers in row form.
  An implementation MAY view that row buffer without copying as a slice of a native struct (`unsafe.Slice`)
  only when the stride equals the struct's size, every field offset matches [FMT §7.1], the buffer is suitably aligned and the host is little-endian;
  the view is valid only while the buffer lives; otherwise headers are decoded field by field.
  The header section itself, stored column by column, is never such a view.
- `Query` = `Filter` plus `Payloads`, which decides only whether payloads are returned ([SEM §7.4]);
  without payloads a read over several packs still reads and compares them.
  `Result` carries `Incomplete` and `Conflicts` side by side; neither hides the other.
  `Incomplete`'s defects and every `Item` name the pack they come from (`Pack`, an index into the readers given; 0 for `Iterate`);
  `FooterErrs` lists each pack whose footer was not used, outside `Incomplete`, since a finalized pack whose walk accounts for every block is complete without its footer.
  `FindTransaction` reports a scope that is not indexed (`cold`) and the scopes it searched in `TxResult`;
  for `Iterate` and `MergeIterate`, which take readers and cannot tell whether a scope is indexed, the searched scope and a `Cold` reason wait for a query over a `PackSource` (G5-140);
  `Removed` lists the covered hours that are removed when a read ends with `ErrRemoved` (below), ascending, each once; `Complete()` reports false while it is non-empty.
- Retention (G5-162, G5-164): a `Retention` provider, `RetainedFrom(ctx) (int64, error)`, returns the first UTC hour still retained, numbered as `HourOf`; every hour before it is removed ([STO §5] Retention).
  `Query.Retention`, `TxOptions.Retention` and `StoreSourceOptions.Retention` take one; nil removes nothing.
  A provider never falls behind the deleting component — before it deletes an object of an hour, every provider a reader asks already reports the hour removed; running ahead is safe, and a cache is safe only if the deleting component waits out its staleness — never moves the boundary back, is safe for concurrent use, and is cheap, since a read asks it at every block it emits;
  a reader keeps the largest value seen, compares in `int64` hours, and ends with the provider's error, wrapped, when a call fails;
  when the failing call is the check after an I/O failure, the wrapped provider error is joined after the I/O cause, both reachable by `errors.Is`.
  A record belongs to its pack's scope hour, `HourOf(period_start)`, for every role but extract, whatever its `ts_utc_ns`; an extract's record to the hour of its `ts_utc_ns`.
  The hours a read covers are the scope hours of its non-extract packs, the F-2 `ts_min` and `ts_max` hours of its extracts' indexed blocks, and the distinct hours of the extract records it decodes, added before filtering;
  a newly covered hour is compared at once with the largest boundary seen.
  `MergeIterate`'s planning pass over walked blocks adds a walked extract block's record hours before it drops the block's buffer.
  The covered hours number at most the non-extract packs, plus two per indexed extract block, plus the distinct extract record hours decoded; they are kept outside `MaxHeldBytes`.
  `Iterate` and `MergeIterate` ask the provider before reading anything, before the first callback and before every callback whose record comes from another block than the previous callback's (for `MergeIterate`, the representative's block),
  after an open or read I/O failure, and before returning success.
  The deleting component deletes an object only after the boundary passed its hour, so a check that finds every covered hour retained proves that no read before it met an object retention deleted.
  When a check finds a covered hour removed, the read ends at once with an error wrapping `ErrRemoved`, joined after the I/O cause when it follows a failure,
  beside the `Result` found so far, with `Removed` set: its records, defects and conflicts of the removed hours, records passed to `fn` included, are superseded, and the caller discards them (G5-164).
  A callback's error, a `ctx` error and a budget error (`ErrReadLimit`) end a read as before, with no retention check after them; a block defect is still a defect.
- `Filter` uses typed slices (`[]SF`, `[]Kind`, `*[4]byte`) and a time range, with nil meaning "any"; no sentinel values.
  HSMS header fields are matched on payload values, available only where `field_validity` says so ([FMT §7.2]).
- `MergeIterate(ctx, readers, Query, MergeIterateOptions, fn) (Result, error)`: one read over several packs, as [SEM §7.4] resolves and orders it;
  the options carry the `Order` (capture or time, required), `MaxHeldBytes` and `MaxConflicts`.
  Readers must be distinct packs (no repeated `pack_id`); the guarantees cover the readers given.
  Copies of a record are compared before the filter, identical ones yielded once, and every selected version of a `conflict` yielded with `Item.Conflict` set;
  `Result.Conflicts` lists every conflict of every compared cluster in discovery order, each with its versions in version order,
  and for each version the `pack_id`s holding it, in the order the readers were given, each once.
  An `Item` names its record's representative ([SEM §7.4]) as its pack and block.
  Capture order streams: it holds, per capture, the blocks of its current cluster that are open and the representatives of the versions it has yet to yield.
  Time order holds the blocks of the cluster being read and the representative of every selected record not yet yielded;
  a cluster can span hours, since a pack's seqs may have gaps (I-12), so this is not bounded by one hour per capture.
  `MaxHeldBytes` bounds the bytes of the block buffers held at once, reserved before each block is read;
  it does not bound the process heap:
  the readers, the block descriptors and clusters it builds before reading, its queues, codec state and the conflict list lie outside it.
  `MaxConflicts` bounds how many conflicts are listed, not the size of one.
  Either limit ends the read with an error wrapping `ErrReadLimit`, returned with the `Result` found so far, instead of exceeding it or omitting a conflict.
  A block over the reader's `MaxBlockLen`, or whose stated dimensions cannot be consistent, is a defect and is skipped, as in `Iterate`.
- `FindTransaction(ctx, src PackSource, TxKey, TxOptions) (TxResult, error)`: [SEM §7.2]'s lookup from a primary named by `TxKey{Capture, Seq, Hour}` (G5-124);
  it derives the key from the primary (G5-122) and fails with `ErrNotPrimary` when the record is not a `data` record whose function is available and odd, or is absent from a complete read (G5-125).
  `TxOptions`: `MaxScopes` (the hours scheduled from `Hour`, default 2, G5-121; a conflicted one is scheduled but not read), `MaxHeldBytes` per read, `MaxConflicts` over the lookup, `MaxStateBytes` for the lookup's own state,
  and `Retention`, given to every scope read as its `Query.Retention`.
  The lookup fails with an error wrapping `ErrRemoved` when the primary's hour is removed (G5-163): the lookup checks it before `Observe`, after a failed `Observe` whose error is neither a `ctx` error nor `ErrReadLimit` (`ErrRemoved`, or a failing provider's wrapped error, joined after that cause),
  before each scheduled scope, and after evaluation and barriers, before it closes the observation and returns an outcome;
  a scope read ending with `ErrRemoved`, or a scope the source reports `Removed`, ends it too, since such a read met removed data, and, as the lookup schedules hours from the primary's forward, a removed scheduled hour implies a removed primary hour.
  An earlier error of the lookup, `ErrNotPrimary` included, stands: the checks above are the lookup's only retention checks.
  The `TxResult` beside `ErrRemoved` keeps the diagnostics found, with a zero outcome, as beside the lookup's other errors.
  A scope reported `Removed` with readers is a source error, as a conflicted one with readers is.
  `TxResult` holds the outcome (`TxMatched`, `TxAmbiguous`, `TxUnmatched`, `TxIncomplete`; zero only beside an error), the derived key with the W bit,
  the window end, every kept version as a `TxRecord` with its roles as a `TxClass` bit set (primary, candidate, possible reply, same-key primary, possible same-key primary, closing record, outcome record) and its candidate flags,
  every reason absence could not be established as a `TxGap` naming its hours and pack_id (`TxGapConflicted` for a conflicted scope not read, G5-142, and `TxGapScopeBreach` for a record outside its scope's hour, G5-146, among them), the scopes read, the conflicts and the footer errors.
  Each scope read is one `MergeIterate` in capture order; its guarantees cover the scopes read (G5-136).
  `PackSource.Observe(ctx, capture, from, to)` returns an `Observation` fixed before it returns ([STO §5] Observation of a lookup, G5-128):
  `Scope(ctx, hour)` (a `SourceScope`: the readers of the scope's view, whether it is indexed, `Conflicted` with no readers for a conflicted view, G5-142, and `Removed` with no readers for a removed hour, G5-165), `Evidence(ctx)` (end state, capture-boundary entries, epoch closures, a partial flag), `Barriers(ctx, from, to)` (the tool's `stop-unclean` boundaries meeting `[from, to)` in nanoseconds, which must be non-empty and lie within the observation's hours; any other range is an error) and `Close`, which never blocks;
  every value returned is the caller's: `Readers` a fresh slice, the evidence and the boundaries deep copies, none changed afterwards by the source or by the lookup;
  readers stay valid until `Close`.
  An `Observe` that fails returns no `Observation`; the lookup closes one it got exactly once, after its last use, whatever happened.
  The source does not retry inside an observation: a failed call ends the lookup with its error.
  A test source lives in the package's tests.
- `NewStoreSource(store ObjectStore, cat Catalog, StoreSourceOptions) (PackSource, error)` (G5-139): the source of one tool over a bucket and the caller's catalog, which also provides the per-capture evidence and barriers (G5-123).
  `ObjectStore`: `List(ctx, prefix, token)` returns one page of keys with sizes after an opaque token valid only for its traversal ("" for the first page; `Next` "" on the last);
  keys are in strictly ascending byte order across the pages of a traversal, and a complete traversal returns every key present under the prefix throughout it (it is not a snapshot);
  an error, also one returned with keys, invalidates the traversal.
  The source fails `Observe` on a key outside the prefix, a key not above the previous one, a repeated token, or a traversal over `MaxListPages` pages.
  `Open(ctx, key)` returns an `Object` (`io.ReaderAt`, `Size`, `Close`), or a nil `Object` and an error; a missing object, at `Open` or at a later `ReadAt`, is an error wrapping `ErrObjectNotFound`, which the error of a failed `Observe` keeps.
  The adapter bounds each `ReadAt` by its own deadline and supports concurrent ones, and `Close` never waits on remote I/O (G5-145); the source checks ctx between reads, never during one.
  `Catalog`: `Snapshot(ctx, capture, from, to)` — from one consistent snapshot, each hour's scope (indexed with its view, conflicted, or not indexed with a confirmation token), the capture's per-capture evidence and the tool's barriers;
  `ConfirmUnindexed(ctx, capture, hour, token)` — whether the scope was not indexed at any instant since the snapshot that issued the token, false when the catalog cannot tell.
  `Observe` fixes every scope: indexed ones from the snapshot, each pack opened and checked against its descriptor;
  the others by [STO §5]'s coherent observation — the scope's commit objects listed until two complete listings agree, then its archive packs and the capture's segments listed, each segment's head read to learn its hour, the packs opened and checked, `ActiveView` computed, and the scope confirmed not indexed after its last read.
  Every key is parsed strictly before it is classified ([STO §3], G5-141, G5-143); a pack whose key, descriptor, tool, hour, role, generation or size disagrees fails `Observe`, so do two packs of one scope with one `pack_id`, an indexed descriptor naming a pack of a role outside the tiers ([STO §2]), and a listed pack that is gone (G5-144, retriable).
  `StoreSourceOptions`: `Prefix`, `Tool`, `Reader` (`ReaderOptions`), `MaxCommitListings`, `MaxListPages`, `MaxObjects`, `MaxSourceBytes` (the state an observation keeps, charged from what it decodes; one `Open`'s transient buffers, `ActiveView`'s workspace and the adapters' allocations outside it), and a required `OnExcluded` callback;
  `NewStoreSource` rejects a `MaxCommitListings` of 1, which two agreeing traversals can never satisfy, and a non-zero `Reader.FooterOffset`, a hint for one object.
  `OnExcluded` reports each listed pack excluded for its role ([STO §2]), once per pack per `Observe`, after the source has closed that pack, also when `Observe` later fails;
  it is called synchronously from the goroutine running `Observe`, possibly concurrently from concurrent `Observe` calls, so it must be safe for concurrent use and return promptly;
  the value it gets is its own; ctx is checked when it returns, and a panic in it propagates after `Observe` has closed every object it opened.
  Every value the adapters return is the source's once returned; both adapters, and the source, are safe for concurrent use.
  Its guarantees hold while every hour observed stays retained until the lookup's last use.
  `StoreSourceOptions.Retention` (G5-165): `Observe` asks it before every list page, commit traversals included, before each open, after a scope's last listing and read and before its view is computed,
  and once more after every scope is fixed, classifying every scope against that final sample;
  every sample classifies every scope known so far against the largest boundary seen.
  A scope whose hour is removed is reported `Removed`, with no readers: removal found before its view is built skips the view and the confirmation, removal found after discards the view;
  its objects are closed once and their charges released at once, and the acquisition work only it needs stops, so no lower-generation view is formed from a half-deleted hour;
  the capture-wide staging listing and its pending opens stop once no retained scope of the observation depends on them.
  A gone object whose hour is known — a pack the catalog names for an indexed scope, an archive key, a segment whose head was read — and removed marks its scope removed;
  a gone object of an unknown hour, a segment the staging listing found whose head was not read, keeps the retriable `ErrObjectNotFound` while a retained scope depends on that listing, as does every gone object without a provider.
  The final sample promises nothing about later advances, which the lookup's own checks cover:
  give `StoreSourceOptions.Retention` and `TxOptions.Retention` the same provider; the source's alone leaves the lookup unprotected after `Observe` returns.
  `EscapeToolID`, `SegmentKey`, `ArchiveKey` and `CommitKey` build the keys of [STO §3].
  A key that cannot be built, and a listed or catalog key that does not parse as a key of [STO §3], is an error wrapping `ErrInvalidKey`, which the error of a failed `Observe` keeps (G5-148); retrying does not cure it.
- `NewReaderSource(readers []*Reader, ReaderSourceOptions) (PackSource, error)` (G5-154, G5-156): a source over packs the caller opened.
  `ReaderSourceOptions.Evidence` holds readers folded into the per-capture evidence only, never into a scope, a view or a commit set:
  the segments and converter archives whose registration was rejected, whose evidence [STO §5] Per capture keeps.
  It validates `readers` and `Evidence` together, each failure wrapping `ErrInvalidQuery`:
  a nil reader, a reader given twice in either or both, a `pack_id` repeated across both, more than one `tool_id` across both,
  a view reader whose role is not `segment`, `archive` or `repair`, an evidence reader whose role is not `segment` or `archive`,
  and an evidence reader whose period is empty or not inside one UTC hour ([FMT §5]), whose scope would otherwise be unknown.
  It groups the readers by `capture_id` and by the UTC hour of their period, computes each scope's view with `ActiveView`,
  taking every generation and patch given as committed:
  giving one asserts that it was accepted ([STO §5] commit protocol), so a caller never gives a generation whose publication failed or was rejected, nor a patch that was not registered.
  folds the statistics of every pack of a capture, view and evidence readers alike, into its per-capture evidence with `AddPackEvidence`,
  and takes as barriers the `stop-unclean` boundaries of the evidence of every capture of the readers' tool.
  It computes every scope's view once, in `NewReaderSource`; `Observe` validates its arguments as `NewStoreSource`'s `Observe` does and answers from those views.
  Without `ReaderSourceOptions.Complete`, its scopes are not indexed and every capture's evidence is `Partial`,
  so every scope a lookup reads is a `TxGapCold` gap and the lookup is never `unmatched`.
  `Complete` asserts that the view readers are every accepted pack of the tool, of every capture and every hour,
  and that the view and evidence readers together are every pack a catalog of the tool would hold evidence of, rejected registrations included (G5-156):
  a set of files from which a pack was removed, by retention or otherwise, cannot be `Complete`.
  Only then can the source vouch for the barriers of captures other than the one observed.
  With it, the scopes are indexed, except the scope of each evidence reader, its `capture_id` and the UTC hour of its period, which is not indexed (G5-158):
  a catalog rejects a registration only for a scope that is not indexed ([STO §5] No admissions outside the index),
  so a lookup reading that scope gets a `TxGapCold` gap, as it would through the catalog,
  and the scope's view still holds only view readers.
  With it, a capture's evidence is `Partial` when any reader of the tool, view or evidence, has no used footer,
  since its statistics, and the boundaries and closures they hold, are then unknown;
  `Complete` never overrides such missing evidence.
  It rejects readers of several tools, a reader given twice, and a scope `ActiveView` rejects; a conflicted view is reported `Conflicted` with no readers.
  `Close` of its observations closes nothing: the caller owns the readers.
  `HourOf(time.Time) int64` returns the UTC hour of a time as `TxKey.Hour` and the key builders number it,
  and `TxKeyOf(capture UUID, seq uint64, ts time.Time) TxKey` builds the key of a primary from its record.
- `ActiveView(packs []PackInfo, commits CommitSet) (View, error)`: [STO §4] active view of one scope from pack metadata (staging segments included) and the scope's commit objects,
  plus the packs that are deletable under [STO §4] Deletion, the packs excluded for their role ([STO §2]), and the `compacted_from` the next merge writes.
  `packs` is a complete observation of the scope: every surviving pack a reader could list, replaced patches included (a catalog snapshot or a coherent observation, [STO §5]).
  A `PackInfo` is built only from decoded bytes:
  `(*Reader).Info()` for a pack a `Reader` opened, or `NewPackInfo(packID, captureID, metadata)`, which decodes a pack's stored pack metadata as `UnmarshalPackMeta` does;
  its accessors return copies.
  Inconsistent input (no packs, a zero `PackInfo`, a period outside one hour, mixed captures or hours, a role that disagrees with its generation, a repeated `pack_id`, two members of one set) returns the zero `View` and an error,
  and two different complete committed sets of the highest rank return `ErrViewConflicted`.
  Whether a commit object may be deleted is decided by the service from its own record of deleted packs, never from packs missing in the input.
- `Merge(ctx, dst io.Writer, view View, inputs []MergeInput, opts MergeOptions) (MergeReport, error)`:
  writes the next generation of the scope of `view`, `ActiveView`'s unchanged result, from `inputs`, which are exactly `view.Packs`,
  into the claim (`ScopeGeneration`, `PublisherEpoch`, a new `ReplacementSetID`) the caller allocated ([FMT §10], [STO §4]);
  `view` supplies the lineage, since `compacted_from` names packs outside the inputs.
  Every input block is read in full.
  A block that overlaps no other block is copied verbatim with its F-3 list;
  duplicates are dropped only on equal envelope and body bytes; every other overlap is resolved record by record;
  small blocks are coalesced by the greedy grouping of [STO §4], each new encoding validated in memory before it is written.
  The output is one archive per scope, a replacement set of one member ([STO §6]).
  `ErrMergeInput`, wrapped with the pack and the cause, refuses an input that is not the pack `view.Packs` names at its position or lies outside the view's lineage,
  that is not `finalized-consistent`, breaches its scope or its own commitments, or differs from the others in a capture-level tag;
  the error of a failed read of an input, or `ErrReadLimit` for an input over a reader budget of `opts.Reader`, is returned naming the input, never as `ErrMergeInput`;
  record conflicts fail with `ErrMergeConflict` once every block was compared, and each conflict is passed, as it is found, to `opts.OnConflict`, which is required;
  exceeding `MaxOpenBlocks` or the output footer budget `MaxFooterLen` fails with `ErrMergeLimit`,
  and so does archive pack metadata longer than `opts.Reader.MaxPackMetadataLen`, the budget the next merge opens it with.
  An error found before the archive's header leaves `dst` untouched, and after any error the caller discards `dst` and publishes nothing ([STO §5] commit protocol);
  an error of the final write or sync can leave bytes that form a finalized pack, which `Merge` still reports as a failure.
- `Extract(ctx, src PackSource, Query, dst io.Writer, opts ExtractOptions) (ExtractReport, error)`: writes one `extract` pack of one capture.
  With `opts.Policy` (a compiled `RedactionPolicy`: id, version, key id, item and annotation rules; compilation rejects invalid patterns and domains) and `opts.Key`,
  it plans every mask and digest first, then writes the header, the pack metadata with `redaction_policy` and the `redaction` entries, and the masked blocks ([SEM §8]);
  the footer is computed from the masked blocks.
  Path resolution uses the module's own E5 item walker, which reports byte offsets; it does not depend on go-secs.
  A record it cannot screen, a source record with `redacted` and a source pack with `redaction_policy` fail the extract before any output.
  `ExtractReport` counts masks per rule and whole-text masks.
- `Verify(ctx, ra, size, opts) (VerifyReport, error)`: the outcome, failed blocks, disagreements, writer defects and validated prefix of [FMT §13];
  a file header or pack metadata that cannot be read is an error, as for `Open`.
- `Repair(ctx, ra, size, dst io.Writer, opts) (RepairReport, error)`:
  writes a generation-0 `repair` patch of the damaged pack, as [FMT §13] builds it ([STO §6]);
  the next `Merge` folds it into a generation, once the service admits the patch to an indexed scope ([STO §5]).
  `opts` names the patch's `writer` and its `patch_base` (the scope's current generation, supplied by the caller),
  and optionally its `pack_id`, the footer codec and a `Syncer`.
  It verifies the pack first and decides every refusal of [FMT §13] before writing a byte:
  `ErrRepairNotNeeded` for a `finalized-consistent` pack without the `seq_start` writer defect,
  `ErrNotRepairable` for the others, both with nothing written.
  It then copies the validated blocks, checking each against what the verification read.
  After any error the caller discards what was written;
  an error of the final write or sync can leave bytes that form a finalized pack, which `Repair` still reports as a failure.
  `RepairReport` holds the damaged pack's `VerifyReport` beside what the patch records:
  its `pack_id`, the blocks and records copied, and the new `coverage` entries.
- `Recover(ctx, spool, dst) (RecoverReport, error)`, deferred until a local-spool recorder is planned (G5-91):
  finalizes an unfinalized spool file as a segment of its original capture with a `stop-unclean` boundary ([STO §4]);
  `RecoverReport` holds the spool file's `VerifyReport`.
- `ExportJSONL(ctx, r, w)`: canonical export ([FMT §15]).
- Documentation (G5-155): one `tracepack` package; `tracepack/README.md` and the package documentation open with the API by audience:
  recorders (`CaptureDescriptor`, `SegmentWriter`, `SegmentSink`), readers (`Open`, `MergeIterate`, `FindTransaction`, `NewReaderSource`)
  and the storage service (`ActiveView`, `Merge`, `Repair`, `NewStoreSource`, the key builders),
  with runnable examples: `ExampleSegmentWriter`, `ExampleOpen`, `ExampleFindTransaction` over `NewReaderSource`, and `ExampleMergeIterate`.

## 4. Classifier: go-secs → decode_status

The Go classifier writes `classifier = go-secs/<version>`.

| decode_status | go-secs source |
|---|---|
| `length-mismatch` | length-field checks in `DecodeHSMSMessage` (`hsms/decode.go`) |
| `bad-ptype` | `ErrInvalidPType` in `DecodeHSMSMessage` |
| `control-with-body` | the control-frame length check in `DecodeHSMSMessage` |
| `bad-stype` | `ErrInvalidControlMsgSType` in `DecodeHSMSMessage` |
| control `ok` | `DecodeHSMSMessage` returns a `*ControlMessage` directly |
| `item-decode-error` | `(*DataMessage).DecodeErr() ≠ nil` (`hsms/data_msg.go`); the body is decoded lazily, so the writer must call `DecodeErr` explicitly |
| `ok-with-trailing`, `trailing_bytes` | `(*DataMessage).TrailingBytes()` |

For well-formed frames the data/control payload equals `(*DataMessage).ToBytes()`
and is what `DecodeHSMSMessage` consumes.
Where go-secs's strictness differs from the spec's item-validity rules, the conformance corpus decides and the classifier is adjusted.

## 5. Observer events: go-secs → transport events and records

### 5.1 Cause mapping

| go-secs `TransitionCause` (`hsms/lifecycle.go`) | spec `cause` | `timer` |
|---|---|---|
| `CauseUnknown` | unknown | — |
| `CauseLocalOpen` | local-open | — |
| `CauseLocalClose` | local-close | — |
| `CauseSelectAccepted` (also SECS-I line-up) | select-accepted | — |
| `CauseSelectRejected` | select-rejected | — |
| `CausePeerSeparate` | peer-separate | — |
| `CausePeerDeselect` | peer-deselect | — |
| `CauseT6Timeout` | timer-expiry | 6 |
| `CauseT7Timeout` | timer-expiry | 7 |
| `CauseLinktestFail` | linktest-failure | — |
| `CauseIOError` | transport-error | — |
| `CauseHandlerExit` | implementation-fault | — |
| `SocketRefused` (a `SocketEventKind`, `hsms/socket_observer.go`), which sources a refused socket's `not-connected` → `not-connected` record (§5.3) | select-rejected | — |

`cause_raw` is always written as `go-secs:<ConstantName>`.
go-secs folds an unannounced peer close into `CauseIOError`, so the Go recorder never writes `peer-close` from a lifecycle event;
only a `socket-close` record can, from `SocketEvent.Err` (§5.3).
T8 also surfaces as `CauseIOError`; T5 has no go-secs source.
go-secs emits no local-deselect or local-separate cause.

### 5.2 Lifecycle epochs and times ([SEM §5])

`hsms.LifecycleEvent` (`hsms/lifecycle.go`) carries `Socket`, `Generation` and `At`,
all taken when the supervisor fires the transition, never when the notifier delivers the event.
A notification delivered late, behind a slow state-change handler or subscriber or after a reopen (handler-concurrency note in `hsms/state.go`),
still names the socket of its own generation and the time its transition fired,
so the recorder writes the `epoch` it maps from `Socket` (§2) and takes `ts_utc_ns` and `mono_ns` from `At`, as for a wire record (§5.5).
`Socket` is 0 for a transition of a generation that never acquired a socket and when the connection has no generation at all;
it names no socket, so such a record is written with `epoch = 0` under the [SEM §5] rule.
Delivery stays best-effort:
the supervisor's state-notification buffer drops the oldest entry when full (`supervisor.emit`, `hsms/supervisor.go`),
so a coalesced bring-up can surface as one transition with `CauseSelectAccepted` rather than `CauseLocalOpen`,
and a dropped notification leaves no record, which [SEM §5] allows.
A coalesced notification carries the `At` of the transition it reports.
States `NotConnectedState` / `NotSelectedState` / `SelectedState` (`hsms/state.go`) map to the spec's `state` enum.

### 5.3 Socket events

A lifecycle cause never establishes a socket event ([SEM §5]):
`CauseLocalClose` means a local Close initiated the transition,
`CauseIOError` covers read/write failure and unannounced peer close (both documented in `hsms/lifecycle.go`),
and the transition into NotConnected is committed before the supervisor's `react` step starts teardown (`hsms/supervisor.go`).
Socket events come from `hsms.WithSocketObserver` (`hsms/socket_observer.go`), which only the HSMS-SS transport reports.
`SocketConnected`, `SocketAccepted` and `SocketClosed` become `socket-connect`, `socket-accept` and `socket-close`, timed by `SocketEvent.At`,
with `socket_role` `active` for a dialed socket and `passive` for an accepted one;
a `SocketClosed` delivered late still carries the time the socket was closed.
`SocketRefused` sources the refused socket's `state-transition` record (§5.1),
so a refused socket yields, in its own epoch, `socket-accept`, whichever of its two frames were observed (none, one or both), that transition record, and `socket-close`.

`socket-close` takes its `cause` from `SocketEvent.Cause`, with `cause_raw` `go-secs:<ConstantName>` as in §5.1:

| `SocketClosed` | spec `cause` |
|---|---|
| `CauseIOError` with `errors.Is(Err, io.EOF)`, or, on a socket reported `SocketRefused`, also with `errors.Is(Err, io.ErrUnexpectedEOF)` | peer-close |
| any other `CauseIOError` | transport-error |
| `CauseLocalClose` | local-close |
| `CauseUnknown` | unknown |
| any other cause | as in §5.1 |

`Err` and `Cause` are one close reason, taken from whichever path closed the socket first,
so a `socket-close` cause can differ from the cause of the `state-transition` that ended the same generation:
a write error recorded just before a concurrent Close wins the transition gives `transport-error` on the socket and `local-close` on the transition.
Both are recorded as reported;
the socket record says what closed the socket first, and the transition record what drove the state machine.

### 5.4 Transactions

T3 comes from `TxEvent{Outcome: TxT3Timeout}` (`hsms/transaction_observer.go`),
which carries S/F, System Bytes, `SessionID`, and the `Socket` and `Generation` the send was bound to, even when it completes after a reconnect;
a send that found no open connection reports both as 0.
It does not carry the direction: the primary is always one this connection sent, so the recorder supplies it from its role.
`TxEvent` has no time of its own;
the transaction observer runs synchronously when the outcome is known, so the recorder takes the time when it receives the event.
Outcomes mirror `TxRejected` / `TxT3Timeout` vs `TxReplied`.
The same-stream F0 rule of [SEM §7.2] matches the SxF0 note in `hsms/data_msg.go` and the mismatch rule of `replyRegistry` (`hsms/reply_registry.go`).

### 5.5 Wire records

`hsms.WithWireObserver` (`hsms/wire_observer.go`) reports every frame the HSMS-SS transport reads or writes,
control frames, frames go-secs rejects after reading them, and a refused socket's frames included.
One `WireEvent` becomes one data or control record:
`payload` = `Frame`, copied during the call; `epoch` from `Socket` (§2);
`dir` from `Direction` and the application's role; `fidelity` = `wire-exact`;
`field_validity` from the frame's length and `decode_status` from the classifier (§4);
`ts_utc_ns` = `At.UnixNano()` and `mono_ns` = `At.Sub(origin)`.
An inbound frame's `At` is the time the frame was fully read.
An outbound frame's `At` is taken immediately before its write is issued,
which is when [SEM §4] observes a frame the vantage sends itself complete: when it is handed to the socket.
The observer is still called only after the write succeeded,
so a frame whose write failed is not recorded, and the `socket-close` that follows carries the error.

go-secs makes the times causal, not the delivery:
the hooks run on different goroutines, an outbound report follows its write,
and the courtesy Separate and a refused socket's frames are reported from goroutines of their own,
so an event can reach the recorder after events that happened later, with no bound on the delay.
A recorder that sorts by `At` within a window before it assigns `seq` gets causal order for every event that arrives within the window;
one that arrives later is recorded where it arrives (§8 question 3),
and a reply recorded before its primary is reported by transaction matching as an anomaly ([SEM §7.2]).

## 6. TAP converter (`veq/tools/tapconv`)

- `sml.Parse` (`sml/parser.go`) returns nil on the first failure and may return zero or several messages without error,
  hence one entry per call and the exactly-one rule ([STO §7]).
- `sml.Parse` builds each message with SessionID 0 and zero System Bytes;
  the converter sets both through `msg.Derive().WithSessionID(…).WithSystemBytes(…).WithWaitBit(…).Build()`
  (`DataMessageBuilder`, `hsms/data_msg.go`).
  `Build` rejects W on an even function, which the converter records as `build-rejected`.
- `source_dialect` records the TAP dialect and the parser mode, `sml.Parse` or `sml.ParseStrict`.
- Input assumptions and the dialect are confirmed against the pilot's TAP sample (G3-20, R3-6).

## 7. Reference CLI (`tracepack/cmd/tracepack`)

Subcommands `list`, `stats`, `dump --sml`, `dump --jsonl`, `verify [--repair]`, `merge`, and `recover` once `Recover` is planned; `grep` and `tx` follow in a later phase;
local paths and `s3://` URLs.
Every command reports `incomplete` and `conflicted` explicitly,
and a storage-backed command whose hour becomes removed ([STO §5] Retention) ends with that outcome, never with success after the records it printed;
the output syntax stays deferred with the other output formats.
`dump --sml` renders data records through `sml.Encoder` and falls back to hex for control, event, annotation and malformed records;
`dump --jsonl` emits the spec's canonical export ([FMT §15]).
In `dump --sml`, a masked subtree is shown at its path with its domain and a digest prefix, its leaves with their item type and length but no value;
a whole-text mask is shown as masked text of its length, without inventing item types; the exact syntax is deferred with the other output formats.
`tx` prints all candidates and outcomes with their status.
Output formats of the other commands and exit codes are deferred to implementation.

## 8. Open questions

1. Answered by go-secs v2.6.0: eqp-hub can be given a socket-level observer.
   `hsms.WithWireObserver` reports every frame's wire bytes, in both directions, on the connection the `eqp_hsms` device holds,
   so a recorder device there writes `raw-stream` / `wire-exact` records instead of `decoded-message` / `re-encoded` (§5.5).
   eqp-hub still has to move to v2.6.0 and build that device.
2. Answered: go-secs v2.6.0 exposes the socket and generation on every observer event,
   and the wall and monotonic time in one `time.Time` on wire, socket and lifecycle events (§5),
   which the eqp-hub path needs for `epoch` and `mono_ns` (§2).
3. How a record that reached the recorder after its ordering window is marked (§5.5):
   its `seq` follows records of events that happened after it, so a reply can be recorded before its primary.
   Whether `ordering-uncertain` ([SEM §6]) covers such a record, or it needs a marker of its own, is open.
