// Package framecodec holds capability-token types that gate the library's
// zero-copy "decode over an already-owned buffer" entry points. The tokens
// wrap a []byte the library itself allocated; external callers cannot
// construct one (the field is unexported and the only constructors are in
// this internal package), so an exported decode that accepts a token creates
// no public caller-owned aliasing contract. This package imports nothing —
// it must not import secs2 (secs2 imports it) or wire (which imports secs2).
package framecodec

// OwnedSECS2Body is a capability token wrapping a SECS-II body the library
// owns. Holding one authorizes a zero-copy (no bytes.Clone) decode.
type OwnedSECS2Body struct{ b []byte }

// AdoptSECS2Body wraps b in an OwnedSECS2Body without copying. The caller
// transfers ownership of b's backing array; b must not be mutated afterward.
func AdoptSECS2Body(b []byte) OwnedSECS2Body { return OwnedSECS2Body{b: b} }

// Bytes returns the owned slice (no copy).
func (o OwnedSECS2Body) Bytes() []byte { return o.b }

// Len reports the owned body length.
func (o OwnedSECS2Body) Len() int { return len(o.b) }
