// Package corpus holds the JSON schemas of the tracepack conformance corpus
// (the tracepack corpus specification §3 and §5)
// and the conversions from what the tracepack package reports into them.
//
// Every schema type encodes, through Marshal, to the one fixed form the corpus specification §4 states:
// two-space indentation, keys in schema order, arrays in the order their schema defines,
// u64 and i64 values as decimal strings, absent optional keys omitted, and a final LF.
// The conversions sort into those orders themselves;
// they rely on the order of the tracepack package's slices only for an order a read or a lookup defines,
// its yield, discovery or version order, which they keep and check where they can.
// The facts of a lookup are normalized from its gaps (NormalizeGaps), and any inconsistency fails the conversion.
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
//
// Generate builds the corpus from the recipes of the vectors_<group>.go files.
// Each recipe writes its pack with the public Writer under the corpus clock and identifiers derived from an identity seed,
// then edits it where the vector needs bytes a Writer does not write;
// the generator reads the pack as a reader of the corpus does
// and checks every read against the recipe's expectation, written by hand from the specification,
// before it returns any file.
// TestCorpus compares the generated files with testdata/corpus, as the corpus specification §6.1 states,
// and reads every committed vector again;
// go test ./internal/corpus -run '^TestCorpus$' -update regenerates the committed files.
package corpus
