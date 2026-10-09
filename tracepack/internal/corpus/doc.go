// Package corpus holds the JSON schemas of the tracepack conformance corpus
// (the tracepack corpus specification §3 and §5)
// and the conversions from what the tracepack package reports into them.
//
// Every schema type encodes, through Marshal, to the one fixed form the corpus specification §4 states:
// two-space indentation, keys in schema order, arrays in the order their schema defines,
// u64 and i64 values as decimal strings, absent optional keys omitted, and a final LF.
// The conversions sort into those orders themselves; they never rely on the order of the tracepack package's slices.
// Expectations are classified by sentinel errors and typed fields, never by error text.
//
// The package reads packs only through the public tracepack API,
// except the structural footer reader, which reads the stored footer bytes directly
// so that a footer's stated values can be shown apart from the package's footer validation.
//
// It also edits pack bytes, to derive the packs a Writer cannot produce from packs a Writer wrote.
// Its edits come in two kinds, named apart.
// Raw surgery (Patch*, DamageZstd, FlipByte, FooterParts) edits stored bytes in place
// and recomputes only the CRCs, lengths and offsets each operation names,
// so reserved bits, retired bits, extension areas and reserved TLV bytes survive any number of edits.
// Re-encoding (ReencodeBlocks) decodes blocks into typed structures and encodes them again,
// which writes reserved bits and bytes as zero.
// Every edit takes a pack's bytes and returns new bytes or an error; none panics on short or malformed input.
package corpus
