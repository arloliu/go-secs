package tracepack

// SetEncodedHook makes w pass every encoded block body through fn before it validates and writes it,
// so a test can inject a defect the Writer's own encoding never produces.
func SetEncodedHook(w *Writer, fn func(enc []byte) []byte) {
	w.encodedHook = fn
}

// PadMeta adds to m the unknown entry RewriteMeta resizes, and returns m.
var PadMeta = padMeta

// RewriteMeta returns a copy of a pack file written over metadata PadMeta padded,
// whose pack metadata is the stored one decoded under the file's minor and changed by fn,
// re-encoded at the stored length, so a test can store metadata the Writer refuses.
var RewriteMeta = rewriteMeta
