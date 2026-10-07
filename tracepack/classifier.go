package tracepack

// Classifier classifies a captured HSMS frame for a writer that records decode_status and trailing_bytes.
//
// This package does not import go-secs;
// the tracepack/classify package implements Classifier on top of it.
type Classifier interface {
	// Name returns the value of the classifier tag in the pack metadata.
	Name() string
	// MaxFrameLen returns the configured maximum frame length, counted with the length prefix and header,
	// which the max_frame_len tag records.
	// 0 means no ceiling: no frame is classified oversized, and no max_frame_len tag is written.
	MaxFrameLen() uint64
	// Frame classifies frame, length prefix and header included, per the tracepack semantics specification §3.
	// It returns the decode_status and trailing_bytes,
	// the latter meaningful only when the status is DecodeStatusOK or DecodeStatusOKWithTrailing.
	// Frame never panics, whatever the bytes, and does not keep frame.
	Frame(frame []byte) (DecodeStatus, int)
}
