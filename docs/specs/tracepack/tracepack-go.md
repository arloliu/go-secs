# tracepack — Go reference implementation

Status: current (2026-10-12)
Implements tracepack v2.33 (format 1.1): `tracepack-format.md` [FMT], `tracepack-jsonl.md` [JSONL], `tracepack-corpus.md` [CORPUS], `tracepack-semantics.md` [SEM], `tracepack-storage.md` [STO]; overview in `tracepack-overview.md`.
Where this document and the normative tracepack documents disagree, the normative tracepack documents wins.
Citations name a file and symbol in `github.com/arloliu/go-secs/v2` on `main`;
line numbers are avoided because they drift.

## 1. Placement and dependencies

- Nested Go module `github.com/arloliu/go-secs/tracepack` in the repository's `tracepack/` directory, with its own `go.mod` and its own tags `tracepack/vX.Y.Z` (G5-77; precedent `benchmarks/go.mod`).
  The path carries no `/v2`: for a module in `tracepack/`, the go command resolves `github.com/arloliu/go-secs/v2/tracepack` to the directory `v2/tracepack/`, so that path would not be fetchable outside the repository.
- The go-secs dependency carries no `replace` directive, and `go.work` stays local (G5-78; `tracepack-impl-plan.md` §2.1).
  A released tracepack requires a released go-secs, at least v2.5.0 (G5-79).
- Reference CLI `tracepack/cmd/tracepack` in the same module is the explicit exemption from the root "no binary" rule.
- The EAP log converter lives in `veq/cmd/eapconv`.
- Dependencies: `github.com/klauspost/compress/zstd` (pure Go; it offers no seekable format, so independent block bodies are the design)
  and `hash/crc32` (`crc32.ChecksumIEEE` is the spec's CRC-32/ISO-HDLC);
  and, for the encryption of originals, `github.com/tink-crypto/tink-go/v2`, of which the module uses only the raw AES-SIV primitive `daead/subtle`,
  so Tink's keyset format never enters a pack (G5-228, G5-245).
  The dependency is the tracepack module's own and does not reach the go-secs root module;
  the module's tests check Wycheproof's AES-SIV-CMAC vectors for 512-bit keys and the corpus's cases.
  No CGO.

## 2. Recorders and their capture descriptors

How each Go-side producer fills the spec's capture model ([SEM §2]):

| Producer | `capture_method` | `vantage` | default `fidelity` | `recorder` example |
|---|---|---|---|---|
| a go-secs application recording the frames `hsms.WithWireObserver` reports (go-secs v2.6.0 or later), before HSMS decoding | `raw-stream` | `host` or `equipment`, by the application's role | `wire-exact` | `<app>/<ver> go-secs/2.6.0` |
| an equipment gateway, frames re-emitted via `ToBytes()`: today its HSMS device → message-encoding adapter → EAP bus adapter → JetStream path; a recorder device on the go-secs v2.6.0 observers is planned, not built; the gateway is the capture's producer over the durable bus (G5-200), publishing either wire frames through the observers (`raw-stream`) or its devices' decoded messages re-encoded (`decoded-message`, G5-249), and thinning the FDC service's polls (G5-225) | `decoded-message` | `intermediary` | `re-encoded` | `<gateway>/<ver> <bus-adapter>` |
| `eapconv` converting EAP SML logs | `log` | where the EAP ran (normally `host`) | `reconstructed` | `eapconv/<ver>` |
| VE or test fixtures producing expected traffic | `generator` | `none` | `synthesized` | `veq/<ver>` |

A recorder on the go-secs observers also installs `hsms.WithSocketObserver`
and assigns `epoch` ([FMT I-7]) from the `Socket` value every go-secs observer event carries (§5):
a per-capture `u32` counter keyed by the connection and its `Socket` values, starting at 1,
because `Socket` is unique only within one connection.
In the durable-bus deployment ([STO §4]) the producer also assigns `capture_id`, `recorder_instance_id` and `seq`, and the writer stores them as received;
the producer publishes each record as the bus message of [STO §4], for a frame capture (`raw-stream` or `decoded-message`) on the capture clock (G5-249).
A producer without socket identity writes `epoch = 0` and `correlation-incomplete`.
As of 2026-09-28, go-secs v2.6.0 (release pending) exposes what the equipment gateway's path lacked:
socket and generation identity on wire, socket, lifecycle and transaction events,
the wire and socket observers,
and event times that carry a monotonic reading (`WireEvent.At`, `SocketEvent.At`, `LifecycleEvent.At`).
The equipment gateway itself still runs go-secs v2.3.0 and has no recorder device:
its path carries no generation id, no start/stop event or instance id,
and only a wall-clock string taken when the adapter converts the message (`SourceTimeStamp`, host-local time zone, no monotonic reading),
so its records get `epoch = 0`, `correlation-incomplete` and no monotonic time (`mono_present` clear) until the gateway moves to go-secs v2.6.0 and records through the observers.
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
- `Writer`: `NewWriter(w io.Writer, WriterOptions)`, the layer under `SegmentWriter` and `Merge`;
  options: the pack metadata, `Uncompressed` (false writes block bodies and the footer with `zstd`, true with `none`, as in `SegmentWriterOptions` and `MergeOptions`, G5-211), block size threshold,
  `Now` (G5-174: the clock that stamps `writer_start_utc_ns` and gives the time part of a `pack_id` or `capture_id` the writer generates as a UUIDv7; nil means `time.Now`),
  and validation, on unless `SkipValidation` is set (G5-152): a validating writer decodes each encoded block, gathers its record headers and checks I-2 before writing it,
  and on a failed check returns an error without writing the block or the trailer ([FMT §12]); nothing is committed to the pack metadata.
  The writer transposes the record headers into the header section when it assembles a block body ([FMT §6]);
  it may feed the header and payload buffers to the encoder in turn instead of concatenating them.
  `PackMeta` carries `capture_id`, the capture's next seq and every pack metadata tag;
  the `Writer` rejects `pack_role` 5, refuses `pack_role` 4 (`repair`) for new output ([FMT §9], G5-202), and rejects replacement-set values other than size 1 and index 0;
  readers keep role 4 a known role and read packs that carry it.
  The `Writer` drops retired tag numbers from the unknown entries it preserves ([FMT §5]).
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
- Bus codec (G5-226): encodes and decodes the 44-byte header of one record outside a block, and the body of bus layout 1, the header followed by the payload or the header alone ([STO §4] *Bus message*);
  the module imports no NATS client, and the header names, the layout number and the digest form are constants.
  The codec refuses a producer seq of 2^63 − 1, which only a consumer's close carries.
- Descriptor codec (G5-226): encodes and decodes the producer-owned entries of a capture descriptor (TLV, [FMT §5]), `thinning` included, as raw values with no default applied:
  the decoder yields the producer-owned pack-metadata values, a nonzero `capture_origin_mono_ns` and a zero `clock_step_tolerance_ns` kept, which the consumer copies;
  `CaptureDescriptor` stays the recorder-facing type, which resolves its defaults before it encodes.
- Masking preparation (G5-246), shaped by the metadata-first commit of [FMT §12]:
  a function masks one record under a policy and its keys, key access going through an interface so that custody stays with the caller,
  and returns the masked record and its `redaction` entries ([SEM §8]);
  the caller masks its whole flush batch, which a bus consumer already holds ([STO §4]), collects the entries into the pack metadata, and then constructs the `Writer`.
  A screening failure in preparation leaves no output.
  The `Writer` checks Marking at append, both ways, for every record that carries `redacted` or is named by an entry, and checks that the commitments hold;
  it still writes the metadata first and holds one block.
- `Writer`, format 1.1: the existing `AssignSeq = false` path keeps each record's given seq, with its ordering checks (G5-246);
  it gains the classification option for given-seq records, accepts unobserved records against the pack's `thinning`, and writes `format_minor` by [FMT §14].
  Placing records into segments by [STO §4] *Scope and order* stays with the log service.
  `Close` of a pack whose last record has seq 2^63 − 1 finalizes it successfully and reports "no continuation" as a result distinct from any error (e.g. a next seq with a `false` flag), never 2^63;
  its callers — `SegmentWriter`, `Merge`, the close writer — treat it as success and publish the pack.
  `NewWriter` refuses a `seq_start` or `NextSeq` above 2^63 − 1.
- Close writer (G5-223, G5-231): writes the close segment of [STO §4] *Closing a capture left open* from the capture's descriptor, its consumer-owned values, the trigger time and the staged bounds;
  its `format_minor` follows [FMT §14].
- Restore (G5-247): restores a record's originals, given its `redaction` entries and the encryption key, all or nothing per record ([SEM §8] Encryption);
  the caller keeps custody of the key.
- `SegmentWriter` (G5-153): `NewSegmentWriter(ctx, SegmentWriterOptions) (*SegmentWriter, error)` writes one capture, in capture order, as segments ([STO §4] Flush),
  as a standalone recorder (G5-150), so its segments carry no `flush_interval_ns`; it keeps its clock-anchor code, and the module exports no stand-alone clock-step detector (G5-226).
  It owns `writer` (from the module's build info, with the fallback `classify.Name` uses), `classifier`, `max_frame_len`, `period_start`, `period_end`, `seq_start`,
  `pack_role = segment`, `compaction_level = 0` and `scope_generation = 0`, a `capture_id` it generates, and the capture's seq counter.
  `CaptureID()` returns that `capture_id` from construction on, so a recorder can persist it as the next capture's `PreviousCaptureID`.
  Options: `Capture` (a `CaptureDescriptor`, required), `Sink` (required), `FlushInterval` (divides one hour; default 5 minutes),
  `MaxSegmentBytes` (default 64 MiB), `Classifier` (optional), `Uncompressed` (false writes zstd, true writes `none`), `BlockThreshold`, `SkipValidation`,
  and `Now` (default `time.Now`, for tests).
  `Now` also reaches each segment `Writer` as its `WriterOptions.Now` (G5-174),
  and gives the time part of the `capture_id` and of every segment `pack_id` the writer generates, also those it generates before a segment's `Writer` exists.
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
  `Write`, `Sync` and `Abort` take no context: the sink bounds each by its own deadline.
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
  `NewDirSink(root, prefix, DirSinkNoReplace())` never replaces a file at a key (G5-197), as reproducible converter output requires ([STO §7] Reproducible output):
  `Commit` publishes by hard-linking the temporary file to its key, which fails when the key exists, even for a file another writer published meanwhile, then removes the temporary file;
  when the key exists, it compares that file's bytes with the segment's, after the link failed, never before,
  and returns an error wrapping `ErrSegmentExists` when they are identical or `ErrSegmentConflict` when they differ,
  the existing file untouched and the temporary file removed in both cases, the segment finished and a later `Abort` doing nothing;
  should that removal fail, the error also wraps its cause and the segment stays abortable, so `Abort` removes the file.
  A key that is not a regular file, such as a symbolic link, which could lead to the temporary file itself,
  fails `Commit` with a plain error once the link has failed, nothing compared or published, as a link among the key's directories does.
  Both errors arise only from a link that did not happen, and `ErrPublishUncertain` only after a link that did, so the three never overlap;
  the default, without the option, still renames over an existing file.
  A file system without hard links fails such a `Commit` with the link's error, nothing published.
- `Reader`: `Open(ctx, ra io.ReaderAt, size, opts)` performs the [FMT §13] bootstrap, optionally seeded with a footer location the caller kept (e.g. from an earlier `Open` of the same object);
  the bytes it reads at bootstrap serve later block reads, so a pack up to the whole-read threshold (4 MiB by default) costs one request (G5-211).
  The `Reader` keeps, for its lifetime, only the first round's buffers that hold blocks, the whole object or else the head and tail windows, never a footer-hint suffix:
  at most max(`WholeReadThreshold`, `HeadWindow` + `TailWindow`) bytes, outside `MergeIterateOptions.MaxHeldBytes`, `TxOptions.MaxHeldBytes` and `TxOptions.MaxStateBytes`;
  it copies a block lying wholly inside them, never aliasing them, and reads any other block with one `ReadAt`.
  `Header()`, `Blocks()` (F-2), `Iterate(ctx, Query, fn) (Result, error)`,
  and `Stats() (PackStats, bool)` (G5-131): the F-5 statistics of a pack whose footer it uses — counts, time and seq ranges, per-epoch summaries with `close_seq`, capture-boundary entries — as a fresh value, false without a used footer.
  `Open` decodes the pack metadata against the registry of the pack's `format_minor` ([FMT §5]),
  parses the redaction entries with it and runs their bootstrap checks, whose defects are recoverable and never fail `Open` ([SEM §8]); its cost grows with the entries.
  Records expose `Redacted`, `Unobserved` and their masked ranges, and entries their `siv`, validated against the record when it is read ([SEM §8] Marking);
  tracepack v0.4.0 rejected some of these defects at bootstrap and checked no range against its record ([FMT §14]);
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
  `Incomplete`'s defects and every `Item` name the pack they come from (`Pack`, an index into the readers given; 0 for `Iterate`),
  and a read over several packs reports a `coverage` entry one pack holds for another capture it reads, located at the pack that holds it (G5-215);
  `FooterErrs` lists each pack whose footer was not used, outside `Incomplete`, since a finalized pack whose walk accounts for every block is complete without its footer.
- `Filter` uses typed slices (`[]SF`, `[]Kind`, `*[4]byte`) and a time range, with nil meaning "any"; no sentinel values.
  HSMS header fields are matched on payload values, available only where `field_validity` says so ([FMT §7.2]).
- `MergeIterate(ctx, readers, Query, MergeIterateOptions, fn) (Result, error)`: one read over several packs, as [SEM §7.4] resolves and orders it;
  the options carry the `Order` (capture or time, required), `MaxHeldBytes` and `MaxConflicts`;
  it prefetches the blocks of the packs it will read next instead of reading one block at a time (G5-211),
  in time order, and in capture order only when at most one capture has a cluster to read, since with several the filter decides which capture advances next.
  Prefetched blocks count against `MaxHeldBytes`, each admitted only when its whole reservation fits, so a read fails with `ErrReadLimit` exactly where it would without prefetch.
  It may issue several `ReadAt` calls concurrently on a Reader's `io.ReaderAt`, as `Open` does, and waits for all of them before it returns, whatever its outcome.
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
  the readers, with the bootstrap bytes each retains, the block descriptors and clusters it builds before reading, its queues, codec state and the conflict list lie outside it.
  `MaxConflicts` bounds how many conflicts are listed, not the size of one.
  Either limit ends the read with an error wrapping `ErrReadLimit`, returned with the `Result` found so far, instead of exceeding it or omitting a conflict.
  A block over the reader's `MaxBlockLen`, or whose stated dimensions cannot be consistent, is a defect and is skipped, as in `Iterate`.
- `FindTransaction(ctx, src PackSource, key TxKey, opts TxOptions) (TxResult, error)`: [SEM §7.2]'s lookup from a primary named by `TxKey{Capture, Seq, Hour}` (G5-124), over the packs its caller gives (G5-201).
  `PackSource` is `interface { Readers(ctx context.Context, hour int64) ([]*Reader, error) }`:
  the readers of every pack given for one UTC hour, of any capture, distinct `pack_id`s, in a fresh slice,
  such as `NewReaderSource` returns over packs the caller opened.
  It derives the key from the primary (G5-122) and fails with `ErrNotPrimary` when the record is not a `data` record whose function is available and odd, or is absent from a complete read (G5-125).
  `TxOptions`: `MaxScopes` (the hours scheduled from `Hour`, default 2, G5-121), `MaxHeldBytes` per read, `MaxConflicts` over the lookup and `MaxStateBytes` for the lookup's own state.
  `TxResult` holds the outcome (`TxMatched`, `TxAmbiguous`, `TxNotFound`; zero only beside an error), the derived key with the W bit,
  the window end, every kept version as a `TxRecord` with its roles as a `TxClass` bit set (primary, candidate, possible reply, same-key primary, possible same-key primary, closing record, outcome record) and its candidate flags,
  `Incomplete` with every reason of [SEM §7.2], beside any outcome and none hiding another, the hours read, the conflicts and the footer errors.
  `TxNotFound` states nothing about records outside the packs given;
  when it detects that a block it read disagrees with its F-2 entry or its F-3 summary, or that another footer value disagrees with the records it describes,
  only `Incomplete` and that defect are guaranteed, every other fact best-effort (G5-203).
  Each hour read is one `MergeIterate` in capture order over that hour's readers.
- `NewReaderSource(readers []*Reader) (PackSource, error)` (G5-154): the readers of a lookup, opened by the caller, grouped by the UTC hour of their period, whatever their capture.
  It accepts a pack of any role but `extract`, `repair`, the retired 5 and unknown values included ([STO §2]).
  It rejects a nil reader, a reader given twice, a repeated `pack_id`, readers of more than one `tool_id`, an `extract`, and a pack whose period is empty or not inside one UTC hour,
  each failure wrapping `ErrInvalidQuery`.
  It has no `Close` and opens or closes no reader: the caller owns the readers.
  `HourOf(time.Time) int64` returns the UTC hour of a time as `TxKey.Hour` and the key builders number it,
  and `TxKeyOf(capture UUID, seq uint64, ts time.Time) TxKey` builds the key of a primary from its record.
- `EscapeToolID`, `SegmentKey` and `ArchiveKey` build the keys of [STO §3];
  a key that cannot be built is an error wrapping `ErrInvalidKey` (G5-148), and retrying does not cure it.
- `Merge(ctx, dst io.Writer, inputs []MergeInput, opts MergeOptions) (MergeReport, error)`:
  writes one archive of one scope from the `inputs` the caller gives, normally every segment and archive of the scope it knows ([STO §4] Merge),
  with the `ScopeGeneration` and `PublisherEpoch` the caller supplies in `opts`, values tracepack ranks nothing by, and a new `ReplacementSetID` ([STO §2], [FMT §5]).
  The lineage comes from the inputs: `compacted_from` lists the inputs' own lists and every input segment, and `supersedes` names every `archive` input;
  both are lineage only, never the evidence for a deletion ([STO §4] Deletion).
  Every input block is read in full.
  A block that overlaps no other block is copied verbatim with its F-3 list;
  duplicates are dropped only on equal envelope and body bytes; every other overlap is resolved record by record;
  small blocks are coalesced by the greedy grouping of [STO §4], each new encoding validated in memory before it is written.
  The output is one archive per scope, with `replacement_set_size` 1 and `replacement_set_index` 0 ([STO §6]).
  `MergeOptions.Now` (G5-179) is the clock of the archive's `Writer`, which stamps its `writer_start_utc_ns`,
  and of the `ReplacementSetID` and `PackID` that `Merge` generates for a zero option; an id the caller supplies is kept.
  `ErrMergeInput`, wrapped with the pack and the cause, refuses an input whose role is not `segment` or `archive`, whose `scope_generation` contradicts its role, whose `pack_id` another input has, or whose scope differs from the first input's,
  one that is not `finalized-consistent`, breaches its scope or its own commitments, or differs from the others in a capture-level tag;
  the error of a failed read of an input, or `ErrReadLimit` for an input over a reader budget of `opts.Reader`, is returned naming the input, never as `ErrMergeInput`;
  record conflicts fail with `ErrMergeConflict` once every block was compared, and each conflict is passed, as it is found, to `opts.OnConflict`, which is required;
  exceeding `MaxOpenBlocks` or the output footer budget `MaxFooterLen` fails with `ErrMergeLimit`,
  and so does archive pack metadata longer than `opts.Reader.MaxPackMetadataLen`, the budget the next merge opens it with.
  An error found before the archive's header leaves `dst` untouched, and after any error the caller discards `dst` and publishes nothing;
  an error of the final write or sync can leave bytes that form a finalized pack, which `Merge` still reports as a failure.
  From format 1.1 it carries the masks and `thinning` of its inputs, and refuses the inputs [STO §4] Merge refuses:
  a minor newer than it implements, preserved bytes that would gain a meaning under the output's minor,
  masked inputs that differ in policy, classifier configuration or `thinning`, a masked input beside an unmasked one, and differing entries of one record.
- `Verify(ctx, ra, size, opts) (VerifyReport, error)`: the outcome, failed blocks, disagreements, writer defects and validated prefix of [FMT §13];
  a file header or pack metadata that cannot be read is an error, as for `Open`.
- `ExportJSONL(ctx, r *Reader, w io.Writer) (Result, error)`: writes the canonical export of the pack `r` reads ([FMT §15], [JSONL]), a function of package `tracepack` (G5-168).
  It exports the records `r.Iterate(ctx, Query{Payloads: true}, …)` yields, in the same order, and returns the `Result` that call returns:
  open defects, the `coverage` entries that intersect the query, the crash gaps ([SEM §7.4] Crash gaps), and every failed block and every block or footer value found disagreeing with its records ([SEM §7.4]) in `Incomplete`, whose records are exported;
  `FooterErr` and `FooterErrs` as `Iterate`, a footer error alone not making the result incomplete.
  A record whose body is not valid ([JSONL §6]) is exported without `body` and adds nothing to `Result`.
  Where `Iterate` lists a reader budget as a `ReasonLimit` defect, `ExportJSONL` fails instead (G5-171, [JSONL §7]):
  with an error wrapping `ErrReadLimit`, before writing anything, when the footer was not used because it is over `MaxFooterLen` or the forward walk stopped at `MaxWalkedBlocks`,
  and when it reaches a block over `MaxBlockLen`.
  Other errors: `ctx`'s error, wrapped, checked before the header line and before each block; a `ReadAt` error, wrapped, as `Iterate` returns it;
  a write error of `w`, wrapped, `io.ErrShortWrite` for a short write. On an error the `Result` holds what was found so far, and the bytes written stay written for the caller to discard.
  It writes each line in pieces of at most 64 KiB, so a line may take several `Write` calls (G5-172);
  beyond the reader's decoded block and the `Result`'s diagnostics, its memory does not grow with the length of a payload, a value or an entry list.
- Documentation (G5-155): one `tracepack` package; `tracepack/README.md` and the package documentation open with the API by audience:
  recorders (`CaptureDescriptor`, `SegmentWriter`, `SegmentSink`), readers (`Open`, `MergeIterate`, `FindTransaction`, `NewReaderSource`, `ExportJSONL`, `Verify`)
  and the storage service (`Merge`, the key builders),
  with runnable examples: `ExampleSegmentWriter`, `ExampleOpen`, `ExampleFindTransaction` over `NewReaderSource`, `ExampleMergeIterate` and `ExampleExportJSONL`.

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
Where go-secs's strictness differs from the spec's item-validity rules, [SEM §3] decides, the Go tests check it, and the classifier is adjusted (G5-210).

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

## 6. EAP converter (`veq/cmd/eapconv`)

- `sml.Parse` (`sml/parser.go`) returns nil on the first failure and may return zero or several messages without error,
  hence one entry per call and the exactly-one rule ([STO §7]).
- `sml.Parse` builds each message with SessionID 0 and zero System Bytes;
  the converter sets both through `msg.Derive().WithSessionID(…).WithSystemBytes(…).WithWaitBit(…).Build()`
  (`DataMessageBuilder`, `hsms/data_msg.go`).
  `Build` rejects W on an even function, which the converter records as `build-rejected`.
- `source_dialect` records the EAP dialect and the parser mode, `sml.Parse` or `sml.ParseStrict`.
- Input assumptions and the dialect are confirmed against the pilot's EAP sample (G3-20, R3-6).
- An EAP log holds the traffic of several tools: the converter writes one capture per tool,
  and one more for the lines of no tool, such as the EAP's own, whose `tool_id` is the EAP's identity as configured in the converter ([STO §7] items 7 and 8).

## 7. Reference CLI (`tracepack/cmd/tracepack`)

Subcommands `dump --jsonl`, `verify` and `find`, after phase 8 (G5-206), on local paths only.
`list`, `stats`, `dump --sml` and `merge` are not in the minimal CLI; whether any of them returns is open (G5-206).
`s3://` URLs are deferred until the query service or another design decides where the object-store dependency lives,
since the CLI shares the library's `go.mod` (G5-170).
Every command reports `incomplete` explicitly;
the output syntax stays deferred with the other output formats.
`dump --jsonl` emits the spec's canonical export ([FMT §15]).
`find` prints the lookup's outcome, its `incomplete` reasons and every kept version with its roles.
Deferred with `dump --sml`, and not part of the CLI of G5-206:
`dump --sml` renders data records through `sml.Encoder` and falls back to hex for control, event, annotation and malformed records;
in it, a masked subtree is shown at its path with its domain and a digest prefix, its leaves with their item type and length but no value;
a whole-text mask is shown as masked text of its length, without inventing item types; the exact syntax is deferred with the other output formats.
Output formats of the other commands and exit codes are deferred to implementation.

## 8. Open questions

1. Answered by go-secs v2.6.0: the equipment gateway can be given a socket-level observer.
   `hsms.WithWireObserver` reports every frame's wire bytes, in both directions, on the connection the gateway's HSMS device holds,
   so a recorder device there writes `raw-stream` / `wire-exact` records instead of `decoded-message` / `re-encoded` (§5.5).
   The gateway still has to move to v2.6.0 and build that device.
2. Answered: go-secs v2.6.0 exposes the socket and generation on every observer event,
   and the wall and monotonic time in one `time.Time` on wire, socket and lifecycle events (§5),
   which the equipment gateway's path needs for `epoch` and `mono_ns` (§2).
3. How a record that reached the recorder after its ordering window is marked (§5.5):
   its `seq` follows records of events that happened after it, so a reply can be recorded before its primary.
   Whether `ordering-uncertain` ([SEM §6]) covers such a record, or it needs a marker of its own, is open.
