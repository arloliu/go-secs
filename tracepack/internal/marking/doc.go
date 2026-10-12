// Package marking implements the Marking checks of redaction entries
// that the tracepack semantics specification §8 makes recoverable:
// the checks of an entry on its own, the order of the entries and the redaction-present flag, found at bootstrap,
// and the checks of the entries naming one record against that record, found when it is read.
//
// The checks are pure functions over plain values, so the pack reader, the writer and the merge
// share them with the restore of masked records, which must not trust a caller-built result.
// The package knows no type of the module's root package;
// it reads the value bounds of an annotation's text and raw entries through internal/tlv.
//
// Each check returns sentinel errors of this package, wrapped with the position they concern;
// a caller reports each one as a recoverable defect and treats the records it concerns as wholly masked.
package marking
