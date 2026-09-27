// Package codec implements the block-body and footer codecs of the tracepack format specification §2:
// `none` (verbatim) and `zstd` (a single RFC 8878 frame, no dictionary).
//
// Encode and Decode are append-style:
// the caller supplies a destination buffer, and the returned slice reuses its backing array when it has enough capacity.
// A codec value outside the registry is reported through ErrUnknownCodec;
// growing that registry is a major tracepack format version change.
package codec
