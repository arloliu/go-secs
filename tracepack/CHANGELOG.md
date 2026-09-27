# Changelog

All notable changes to the `github.com/arloliu/go-secs/tracepack` module are documented here.
Releases are tagged `tracepack/vX.Y.Z` on `main`, independently of go-secs `vX.Y.Z` tags.

## [Unreleased]

### Added

- Module skeleton: `github.com/arloliu/go-secs/tracepack`, nested in `tracepack/`, with Makefile targets, a dedicated CI job and the release flow.
  The format specification (tracepack v2.8, format 1.0) lives in `docs/specs/tracepack/`.
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
