// Package tracepack is the Go reference implementation of the tracepack container format (.tpk):
// an immutable, compressed, self-indexed archive of recorded SECS-II traffic between one tool and its host,
// including HSMS control messages and transport lifecycle events.
//
// The format is specified in docs/specs/tracepack/ of the go-secs repository.
// The specification, not this package, defines every field;
// the conformance corpus is the executable arbiter where the two disagree.
//
// This module is versioned independently of go-secs, with tags of the form tracepack/vX.Y.Z.
// The package holds the record and pack-metadata types, the typed event payloads, the Writer, the Reader and Verify;
// repair, merge and extraction follow.
package tracepack
