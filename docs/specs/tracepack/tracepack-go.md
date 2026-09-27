# tracepack — Go reference implementation

Status: current (2026-09-27)
Implements tracepack v2.8 (format 1.0): `tracepack-format.md` [FMT], `tracepack-semantics.md` [SEM], `tracepack-storage.md` [STO]; overview in `tracepack-overview.md`.
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
| go-secs conn wrapper recording socket bytes before HSMS decoding | `raw-stream` | `host` or `equipment`, by the application's role | `wire-exact` | `go-secs/2.4.2 conn-wrapper` |
| eqp-hub, frames re-emitted via `ToBytes()`: today the `eqp_hsms` device → `hsms_secsjson` adapter → `tap_nats` device → JetStream path; a dedicated recorder device is planned, not built | `decoded-message` | `intermediary` | `re-encoded` | `eqp-hub/<ver> tap_nats` |
| `tapconv` converting TAP SML logs | `log` | where TAP ran (normally `host`) | `reconstructed` | `tapconv/<ver>` |
| VE or test fixtures producing expected traffic | `generator` | `none` | `synthesized` | `veq/<ver>` |

The conn wrapper is also the socket observer that assigns `epoch` ([FMT I-7]).
In the durable-bus deployment ([STO §4]) the producer also assigns `capture_id`, `recorder_instance_id` and `seq`, and the writer stores them as received.
A producer without it writes `epoch = 0` and `correlation-incomplete`.
As of 2026-09-27 the eqp-hub path carries no connection generation id, no start/stop event or instance id,
and only a wall-clock string taken when the adapter converts the message (`SourceTimeStamp`, host-local time zone, no monotonic reading),
so its records get `epoch = 0`, `correlation-incomplete` and `no-mono` until go-secs exposes a generation number and read-time timestamps on received messages and lifecycle events,
and eqp-hub carries them to the producer boundary.
Recorders flush `segment` packs to the staging tier; the merger (a service component using `Merge`) writes `archive` packs ([STO]).
The query service, its catalog database and the live-tail interface are designed separately.

## 3. API sketch (non-binding)

- `Record` mirrors the spec's record header (`Mono` nil when `mono_present` is clear).
- `Writer`: `NewWriter(w io.Writer, sync Syncer, meta PackMeta, opts)`; options: codec (default zstd), block size threshold,
  and validation: a validating writer commits `blocks_validated` in the pack metadata, decodes and checks each encoded block before writing it,
  and on a failed check returns an error without writing the trailer ([FMT §12]).
  A block is written as its header section followed by its payload section ([FMT §6]);
  the writer may feed the two buffers to the encoder in turn instead of concatenating them.
  `PackMeta` carries `capture_id`, the capture's next seq and every pack metadata tag.
  `Append(*Record)` never rejects for time, assigns the capture-scoped `Seq` ([FMT I-12])
  and closes the current block before a record from another UTC hour ([FMT I-13]).
  `Flush`; `Close` writes footer + trailer ([FMT §12]) and returns the capture's next seq for the following segment.
- `Reader`: `Open(ctx, ra io.ReaderAt, size, opts)` performs the [FMT §13] bootstrap, optionally seeded with a catalog footer location;
  `Header()`, `Blocks()` (F-2), `Iterate(ctx, Query, fn) (Result, error)`.
  `Open` parses the redaction entries with the pack metadata and runs their structural checks; its cost grows with the entries.
  Records expose `Redacted` and their masked ranges, validated against the record when it is read ([SEM §8] Marking);
  a wholly masked record reports its defect, and `Header()` exposes the policy, the entries and any bootstrap defect.
  The decode step either decodes the whole body (a full read, [FMT I-2]) or streams only the header section (a header-only read, [FMT §6]).
  A header-only iterator yields record headers with their validation level; payloads are decoded lazily, by a full read of the block.
  An implementation MAY view the header section without copying as a slice of a native struct (`unsafe.Slice`)
  only when the stride equals the struct's size, every field offset matches [FMT §7.1], the buffer is suitably aligned and the host is little-endian;
  the view is valid only while the decoded buffer lives; otherwise headers are decoded field by field.
- `Query` = `Filter` plus a mode, `Provisional` or `Authoritative` ([SEM §7.4]).
  `Result` carries `Incomplete`, `Conflict` and the validation status (`HeaderValidated` with the blocks concerned) side by side;
  none of them hides another.
  `Incomplete` carries the searched scope and a `Reason` (`Cold` among them, [STO §5] Completeness), for `Iterate` and `MergeIterate` alike;
  `Removed` lists the hours that ended with the removed outcome ([STO §5] Retention), and a caller never reports records of a removed hour as a result.
- `Filter` uses typed slices (`[]SF`, `[]Kind`, `*[4]byte`) and a time range, with nil meaning "any"; no sentinel values.
- `MergeIterate(ctx, readers, Query, order, fn)` with two orders:
  capture order (by `seq` within each capture, captures interleaved by their F-2 `ts_min`), which streams without buffering;
  and time order (`ts_utc_ns`, `capture_id`, `seq`), which timestamps inside a pack do not provide, so it uses a watermark:
  blocks are loaded in ascending F-2 `ts_min`, decoded records wait in a heap, and a record is emitted once its timestamp is strictly < the smallest `ts_min` of the blocks not yet loaded
  (the heap drains completely at the end), so equal timestamps are ordered by (`capture_id`, `seq`) correctly.
  Memory is bounded by the records of blocks whose [`ts_min`, `ts_max`] overlap the watermark — at most one UTC hour per capture, because blocks are hour-aligned ([FMT I-13]);
  a caller-set limit returns an error instead of exceeding it.
  Both orders deduplicate by (`capture_id`, `seq`) ([FMT I-12]).
- `FindTransaction(ctx, src PackSource, TxKey, mode) (TxResult, error)` returning `Matched | Ambiguous | Unmatched | Incomplete{SearchedScope, Reason}` (`Reason` including `Cold`)
  plus candidates, outcome records and the validation status of [SEM §7.4];
  `PackSource` supplies, per scope, the packs, whether the scope is indexed and the seq coverage it can establish,
  and per capture the end states, barriers and epoch closures of [STO §5], including every barrier that meets the queried range whatever capture it belongs to ([SEM §7.2]);
  a listing-backed implementation takes coherent observations ([STO §5]).
- `ActiveView(packs []PackInfo, commits CommitSet) (View, error)`: [STO §4] active view of one scope from pack metadata (staging segments included) and the scope's commit objects,
  plus the packs that are deletable under [STO §4] Deletion.
  Whether a commit object may be deleted is decided by the service from its own record of deleted packs, never from packs missing in the input.
- `Merge(ctx, dst io.Writer, inputs []MergeInput, opts) (MergeReport, error)`:
  block-copy merge of one scope's active view into the allocated claim number under the publisher's epoch, with dedup and footer aggregation ([FMT §10], [STO §4]).
  Blocks are copied verbatim; duplicates are dropped only on equal envelope and body bytes; only blocks of equal `record_header_len` are coalesced.
  An `Attest` option validates every block of a member before its pack metadata is written ([STO §4]):
  `Merge` validates or stages the whole member first, so the destination never needs a restart.
  `MergeReport` lists the members written without `blocks_validated` and the defects found.
- `Extract(ctx, src PackSource, Query, dst io.Writer, opts ExtractOptions) (ExtractReport, error)`: writes one `extract` pack of one capture.
  With `opts.Policy` (a compiled `RedactionPolicy`: id, version, key id, item and annotation rules; compilation rejects invalid patterns and domains) and `opts.Key`,
  it plans every mask and digest first, then writes the header, the pack metadata with `redaction_policy` and the `redaction` entries, and the masked blocks ([SEM §8]);
  the footer is computed from the masked blocks.
  Path resolution uses the module's own E5 item walker, which reports byte offsets; it does not depend on go-secs.
  A record it cannot screen, a source record with `redacted` and a source pack with `redaction_policy` fail the extract before any output.
  `ExtractReport` counts masks per rule and whole-text masks.
- `Verify(ctx, ra, size) Report` and `Repair(ctx, src, dst) Report`: writes a generation-0 `repair` patch naming the damaged pack, with `coverage` ([STO §6]);
  the next `Merge` folds it into a generation, once the service admits the patch to an indexed scope ([STO §5]).
  `Repair` rejects an extract ([STO §2]).
- `Recover(ctx, spool, dst) Report`: finalizes an unfinalized spool file as a segment of its original capture with a `stop-unclean` boundary ([STO §4]).
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

## 5. Lifecycle and transaction events: go-secs → transport events

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
| `CauseHandlerExit` (on branch `fix/robustness-batch2`, not yet on `main`) | implementation-fault | — |

`cause_raw` is always written as `go-secs:<ConstantName>`.
go-secs folds an unannounced peer close into `CauseIOError`, so the Go recorder never writes `peer-close` from a lifecycle event;
only the conn wrapper's socket observation can.
T8 also surfaces as `CauseIOError`; T5 has no go-secs source.
go-secs emits no local-deselect or local-separate cause.

### 5.2 Why lifecycle epochs are unknown by default ([SEM §5])

`hsms.LifecycleEvent{Previous, Current, Cause}` (`hsms/lifecycle.go`) carries no generation id;
the supervisor's state-notification buffer drops the oldest entry when full (`supervisor.notify`, `hsms/supervisor.go`),
so a coalesced bring-up can surface as one transition with `CauseSelectAccepted` rather than `CauseLocalOpen`;
an old cycle's handler can still deliver after a reopen (handler-concurrency note in `hsms/state.go`).
States `NotConnectedState` / `NotSelectedState` / `SelectedState` (`hsms/state.go`) map to the spec's `state` enum.

### 5.3 Socket events

`CauseLocalClose` means a local Close initiated the transition,
`CauseIOError` covers read/write failure and unannounced peer close (both documented in `hsms/lifecycle.go`),
and the transition into NotConnected is committed before the supervisor's `react` step starts teardown (`hsms/supervisor.go`).
Hence socket-accept / socket-connect / socket-close come only from the conn wrapper.

### 5.4 Transactions

T3 comes from `TxEvent{Outcome: TxT3Timeout}` (`hsms/transaction_observer.go`),
which carries S/F and System Bytes but not SessionID or direction; the recorder supplies those.
Outcomes mirror `TxRejected` / `TxT3Timeout` vs `TxReplied`.
The same-stream F0 rule of [SEM §7.2] matches the SxF0 note in `hsms/data_msg.go` and the mismatch rule of `replyRegistry` (`hsms/reply_registry.go`).

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

Subcommands `list`, `stats`, `dump --sml`, `dump --jsonl`, `verify [--repair]`, `merge`, `recover`; `grep` and `tx` follow in a later phase;
local paths and `s3://` URLs.
Every command reports `incomplete` and `conflicted` explicitly,
and a storage-backed command whose hour becomes removed ([STO §5] Retention) ends with that outcome, never with success after the records it printed;
the output syntax stays deferred with the other output formats.
Query commands take the query mode (`--mode provisional|authoritative`, [SEM §7.4])
and print the validation status next to `incomplete` and `conflict`.
`dump --sml` renders data records through `sml.Encoder` and falls back to hex for control, event, annotation and malformed records;
`dump --jsonl` emits the spec's canonical export ([FMT §15]).
In `dump --sml`, a masked subtree is shown at its path with its domain and a digest prefix, its leaves with their item type and length but no value;
a whole-text mask is shown as masked text of its length, without inventing item types; the exact syntax is deferred with the other output formats.
`tx` prints all candidates and outcomes with their status.
Output formats of the other commands and exit codes are deferred to implementation.

## 8. Open questions

1. Whether eqp-hub can be given a socket-level observer (a recorder device or the conn wrapper),
   which would let it write `raw-stream` / `wire-exact` records instead of `decoded-message` / `re-encoded`.
2. Which go-secs release exposes the connection generation number and the read-time wall and monotonic timestamps
   that the eqp-hub path needs for `epoch` and `mono_ns` (see §2).
