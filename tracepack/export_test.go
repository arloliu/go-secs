package tracepack

// SetEncodedHook makes w pass every encoded block body through fn before it validates and writes it,
// so a test can inject a defect the Writer's own encoding never produces.
func SetEncodedHook(w *Writer, fn func(enc []byte) []byte) {
	w.encodedHook = fn
}
