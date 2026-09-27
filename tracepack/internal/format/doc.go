// Package format implements the fixed-width byte layouts of the tracepack container format:
// checksums, UUIDs, portable integer limits, and the sentinel errors a decoder reports.
//
// Every rule this package encodes is normative text from the tracepack format specification, never an implementation choice;
// see docs/specs/tracepack/tracepack-format.md §1 and §2 for the conventions and portable encoding rules this package follows.
// Callers outside this module never see it directly: internal/format has no dependency on the public tracepack package,
// and its raw integer fields carry enum values as plain numbers, not the public enum types built on top of them.
package format
