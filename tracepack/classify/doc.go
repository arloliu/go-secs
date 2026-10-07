// Package classify applies the tracepack semantics specification §3 decision table to a captured HSMS frame.
// It reports the resulting decode_status.
//
// [Frame] runs the classifier's own structural checks before calling go-secs:
// short frame, length mismatch, bad PType, bad SType, a control frame carrying a body, and an oversized frame.
// The decision table evaluates those rows first, and go-secs rejects some of the same malformed shapes on its own terms.
// Once a frame clears those checks, [Frame] calls github.com/arloliu/go-secs/v2/hsms to decode it
// and classifies the result: an item decode error, trailing bytes after a valid item, or ok.
//
// [Name] identifies this classifier for the pack metadata's classifier tag of the tracepack format specification;
// it is derived once, at package initialization, from the go-secs/v2 module version reported by runtime/debug.ReadBuildInfo.
//
// [New] returns the same classifier as a tracepack.Classifier, with a uint64 ceiling, for a writer that takes one.
package classify
