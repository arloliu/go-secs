# Changelog

All notable changes to the `github.com/arloliu/go-secs/tracepack` module are documented here.
Releases are tagged `tracepack/vX.Y.Z` on `main`, independently of go-secs `vX.Y.Z` tags.

## [Unreleased]

This release follows the format revision of spec v2.13, which redefines format 1.0 in place,
and adds the reader.

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

### Changed

- Format (spec v2.13, format 1.0 redefined once, before any production use):
  the header section of a block body stores the record headers column by column,
  byte j of record i at offset j × record_count + i;
  the record header is 44 bytes, its extension area starting at byte 44;
  the quality bits decode-failed and no-mono and the record_flags W bit are retired and written as zero.
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

### Added

- Reader:
  `Open` bootstraps a `Reader` over an `io.ReaderAt` of known size in rounds of concurrent reads:
  one round when the pack metadata and the footer fit the head and tail windows or a catalog hint locates the footer, two otherwise.
  It validates the file header, the format version and the pack metadata before any block is read.
  `ReaderOptions` sets the windows, the whole-read threshold and the reader budgets `MaxPackMetadataLen`, `MaxFooterLen` and `MaxBlockLen`,
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
- `Record.HSMSHeader` and `HSMSHeader`, the HSMS header fields of a payload with their availability kept separate;
  `Record.SetCapturedFieldValidity`.
- `DecodeStatus.Clean`, true only for a known classification outside the malformed set, so an unknown value is neither clean nor malformed.
- Sentinel errors `ErrNotTracepack`, `ErrUnsupportedFormat`, `ErrChecksum`, `ErrReadLimit`, `ErrInvalidFooter`, `ErrInvalidQuery`,
  `ErrFieldValidity` and `ErrFieldValue`.

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
