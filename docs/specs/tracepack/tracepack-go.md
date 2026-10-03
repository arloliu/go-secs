# tracepack — Go reference implementation

Status: current (2026-10-04)
Implements tracepack v2.22 (format 1.0): `tracepack-format.md` [FMT], `tracepack-semantics.md` [SEM], `tracepack-storage.md` [STO]; overview in `tracepack-overview.md`.
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
- `Writer`: `NewWriter(w io.Writer, sync Syncer, meta PackMeta, opts)`; options: codec (default zstd), block size threshold,
  and validation: a validating writer decodes each encoded block, gathers its record headers and checks I-2 before writing it,
  and on a failed check returns an error without writing the block or the trailer ([FMT §12]); nothing is committed to the pack metadata.
  The writer transposes the record headers into the header section when it assembles a block body ([FMT §6]);
  it may feed the header and payload buffers to the encoder in turn instead of concatenating them.
  `PackMeta` carries `capture_id`, the capture's next seq and every pack metadata tag;
  the `Writer` rejects `pack_role` 5 and replacement-set values other than size 1 and index 0,
  and drops retired tag numbers from the unknown entries it preserves ([FMT §5]).
  `Append(*Record)` never rejects for time, assigns the capture-scoped `Seq` ([FMT I-12])
  and closes the current block before a record from another UTC hour ([FMT I-13]);
  it rejects a record whose set `field_validity` bit names bytes its payload lacks ([FMT §7.2]).
  `Flush`; `Close` writes footer + trailer ([FMT §12]) and returns the capture's next seq for the following segment.
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
  `Removed` lists the hours that ended with the removed outcome ([STO §5] Retention), and a caller never reports records of a removed hour as a result.
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
  `TxOptions`: `MaxScopes` (the hours scheduled from `Hour`, default 2, G5-121; a conflicted one is scheduled but not read), `MaxHeldBytes` per read, `MaxConflicts` over the lookup, `MaxStateBytes` for the lookup's own state.
  `TxResult` holds the outcome (`TxMatched`, `TxAmbiguous`, `TxUnmatched`, `TxIncomplete`; zero only beside an error), the derived key with the W bit,
  the window end, every kept version as a `TxRecord` with its roles as a `TxClass` bit set (primary, candidate, possible reply, same-key primary, possible same-key primary, closing record, outcome record) and its candidate flags,
  every reason absence could not be established as a `TxGap` naming its hours and pack_id (`TxGapConflicted` for a conflicted scope not read, G5-142, and `TxGapScopeBreach` for a record outside its scope's hour, G5-146, among them), the scopes read, the conflicts and the footer errors.
  Each scope read is one `MergeIterate` in capture order; its guarantees cover the scopes read (G5-136).
  `PackSource.Observe(ctx, capture, from, to)` returns an `Observation` fixed before it returns ([STO §5] Observation of a lookup, G5-128):
  `Scope(ctx, hour)` (a `SourceScope`: the readers of the scope's view, whether it is indexed, and `Conflicted` with no readers for a conflicted view, G5-142), `Evidence(ctx)` (end state, capture-boundary entries, epoch closures, a partial flag), `Barriers(ctx, from, to)` (the tool's `stop-unclean` boundaries meeting `[from, to)` in nanoseconds, which must be non-empty and lie within the observation's hours; any other range is an error) and `Close`, which never blocks;
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
  `StoreSourceOptions`: `Prefix`, `Tool`, `Reader` (`ReaderOptions`), `MaxCommitListings`, `MaxListPages`, `MaxObjects`, `MaxSourceBytes` (the state an observation keeps, charged from what it decodes; one `Open`'s transient buffers, `ActiveView`'s workspace and the adapters' allocations outside it), and a required `OnExcluded` callback.
  `OnExcluded` reports each listed pack excluded for its role ([STO §2]), once per pack per `Observe`, after the source has closed that pack, also when `Observe` later fails;
  it is called synchronously from the goroutine running `Observe`, possibly concurrently from concurrent `Observe` calls, so it must be safe for concurrent use and return promptly;
  the value it gets is its own; ctx is checked when it returns, and a panic in it propagates after `Observe` has closed every object it opened.
  Every value the adapters return is the source's once returned; both adapters, and the source, are safe for concurrent use.
  Its guarantees hold while every hour observed stays retained until the lookup's last use; the retention boundary comes with phase 5c3.
  `EscapeToolID`, `SegmentKey`, `ArchiveKey` and `CommitKey` build the keys of [STO §3].
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
