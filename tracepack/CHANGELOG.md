# Changelog

All notable changes to the `github.com/arloliu/go-secs/tracepack` module are documented here.
Releases are tagged `tracepack/vX.Y.Z` on `main`, independently of go-secs `vX.Y.Z` tags.

## [Unreleased]

### Added

- Reader:
  `Open` bootstraps a `Reader` over an `io.ReaderAt` of known size in rounds of concurrent reads:
  one round when the pack metadata and the footer fit the head and tail windows or a catalog hint locates the footer, two otherwise.
  It validates the file header, the format version, the pack metadata and its `schema_version` before any block is read.
  `ReaderOptions` sets the windows, the whole-read threshold and the reader budgets `MaxPackMetadataLen`, `MaxFooterLen` and `MaxBlockLen`,
  each checked before the structure is allocated.
  `Header` returns a fresh `PackHeader` with the pack metadata, `Finalized`, `Attested`, `FooterErr` and the trailer,
  and `Blocks` returns the block index.
  A footer that fails its CRC, its validation or a budget is never trusted:
  `FooterErr` reports it, and the forward walk finds the blocks instead.
- `Reader.Iterate` yields the records a `Query` selects, in file order, and returns the read's `Result`:
  a `Filter` on time, kind, direction, epoch and the copy fields (stream and function, session ID, System Bytes),
  where an unavailable copy field matches only on request;
  pruning by the footer's time and epoch ranges and kind and direction counts;
  the provisional and authoritative query modes,
  with `HeaderValidated` listing the blocks whose unvalidated header copies a result relied on;
  and `Incomplete` for corrupt, unknown-codec, truncated and over-budget blocks and for coverage entries that intersect the query,
  reported beside the records that were read.
  Each `Item` carries the record as stored, the record-header bytes a newer minor version appends, its block and its read level
  (`ReadHeaderOnly`, `ReadAttested`, `ReadFull`);
  `CopyMismatch` reports a full read whose header copies disagree with the payload.
  A `Reader` is safe for concurrent `Iterate` calls.
- Sentinel errors `ErrNotTracepack`, `ErrUnsupportedFormat`, `ErrChecksum`, `ErrReadLimit`, `ErrInvalidFooter` and `ErrInvalidQuery`.

### Changed

- The format specification moved to tracepack v2.11 (format 1.0, no byte changes):
  it defines how a reader matches a coverage entry against a query.

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
