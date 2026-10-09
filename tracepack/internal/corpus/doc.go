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
package corpus
