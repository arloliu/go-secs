package classify

import (
	"encoding/binary"
	"runtime/debug"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/v2/hsms"
)

// Byte layout of a captured HSMS frame, per the tracepack format specification §8:
// a 4-byte big-endian length prefix followed by the 10-byte HSMS header (SEMI E37 §8.2).
const (
	lengthPrefixLen = 4
	headerLen       = 10
	minFrameLen     = lengthPrefixLen + headerLen

	ptypeOffset = lengthPrefixLen + 4 // header byte 4
	stypeOffset = lengthPrefixLen + 5 // header byte 5
)

// goSecsModulePath is the module go-secs/v2 [hsms.DecodeHSMSMessage] is imported from;
// [Name] reports its resolved version.
const goSecsModulePath = "github.com/arloliu/go-secs/v2"

// unknownName is what [Name] reports when the go-secs/v2 module version cannot be determined.
const unknownName = "go-secs/unknown"

// Name identifies this classifier for the pack metadata's classifier tag of the tracepack format specification.
//
// It is set once, at package initialization, from the go-secs/v2 module version reported by runtime/debug.ReadBuildInfo.
// It falls back to "go-secs/unknown" when build info is unavailable or does not name that dependency.
// A Go workspace build hits that fallback: it resolves the dependency to a local directory instead of a versioned module.
var Name = classifierName()

// Frame classifies a captured HSMS frame per the tracepack semantics specification §3 decision table.
// It evaluates the table's rows in the table's own order.
//
// maxFrameLen is the pack's configured maximum frame length; zero or negative means no ceiling,
// so the oversized row never matches.
// Frame never panics: every input, however malformed, yields a decode_status and a trailing-byte count.
//
// Parameters:
//   - frame: the captured bytes, length prefix and header included.
//   - maxFrameLen: the pack's configured maximum frame length, counted like frame (prefix and header included);
//     0 means no ceiling.
//
// Returns:
//   - tracepack.DecodeStatus: the decision-table row that matched.
//   - int: trailing_bytes, meaningful only when the status is DecodeStatusOK or DecodeStatusOKWithTrailing.
func Frame(frame []byte, maxFrameLen int) (tracepack.DecodeStatus, int) {
	var ceiling uint64
	if maxFrameLen > 0 {
		ceiling = uint64(maxFrameLen)
	}

	return classifyFrame(frame, ceiling)
}

// New returns a [tracepack.Classifier] that classifies frames as [Frame] does, under the ceiling maxFrameLen.
//
// The classifier compares frame lengths with maxFrameLen as uint64 values,
// so a ceiling above the largest int, such as 2^32 or more on a 32-bit platform, is never truncated.
// Its Name method returns [Name], and its MaxFrameLen method returns maxFrameLen.
//
// Parameters:
//   - maxFrameLen: the maximum frame length, counted like a frame (prefix and header included);
//     0 means no ceiling.
//
// Returns:
//   - tracepack.Classifier: the classifier, safe for concurrent use.
func New(maxFrameLen uint64) tracepack.Classifier {
	return classifier{maxFrameLen: maxFrameLen}
}

// classifier is the [tracepack.Classifier] that [New] returns.
type classifier struct {
	maxFrameLen uint64
}

// Name returns [Name].
func (classifier) Name() string {
	return Name
}

// MaxFrameLen returns the ceiling given to [New].
func (c classifier) MaxFrameLen() uint64 {
	return c.maxFrameLen
}

// Frame classifies frame as [Frame] does, under the ceiling given to [New].
func (c classifier) Frame(frame []byte) (tracepack.DecodeStatus, int) {
	return classifyFrame(frame, c.maxFrameLen)
}

// classifyFrame evaluates the decision table for [Frame] and the classifier [New] returns.
// ceiling is the maximum frame length, 0 for none; it is compared with the frame length as uint64 values.
func classifyFrame(frame []byte, ceiling uint64) (tracepack.DecodeStatus, int) {
	if len(frame) < minFrameLen {
		return tracepack.DecodeStatusShortFrame, 0
	}

	length := binary.BigEndian.Uint32(frame[0:lengthPrefixLen])
	if int(length) != len(frame)-lengthPrefixLen {
		return tracepack.DecodeStatusLengthMismatch, 0
	}

	if frame[ptypeOffset] != 0 {
		return tracepack.DecodeStatusBadPType, 0
	}

	// SEMI E37 Table 5 defines SType 0-7 and 9; 8 is not assigned.
	stype := frame[stypeOffset]
	if !hsms.IsValidSType(stype) {
		return tracepack.DecodeStatusBadSType, 0
	}

	if stype != 0 && length != headerLen {
		return tracepack.DecodeStatusControlWithBody, 0
	}

	if ceiling > 0 && uint64(len(frame)) > ceiling {
		return tracepack.DecodeStatusOversized, 0
	}

	return classifyParsed(frame)
}

// classifyParsed decodes frame with go-secs and maps the result to the remaining decision-table rows.
//
// By the time it is called, classifyFrame's own checks already guarantee a well-formed frame shape:
// a length field that matches the captured length, PType 0, a defined SType, and a control frame with no body.
// So [hsms.DecodeHSMSMessage] fails only when the frame trips a go-secs limit outside the decision table, such as its own whole-frame size ceiling.
// That residual case is reported as an item decode error, the closest row to "the bytes did not decode".
func classifyParsed(frame []byte) (tracepack.DecodeStatus, int) {
	msg, err := hsms.DecodeHSMSMessage(frame)
	if err != nil {
		return tracepack.DecodeStatusItemDecodeError, 0
	}

	data, ok := msg.ToDataMessage()
	if !ok {
		// A control message decoded successfully:
		// the tracepack semantics specification §3 applicability rule reaches ok as soon as the frame parses,
		// since a control message carries no SECS-II item.
		return tracepack.DecodeStatusOK, 0
	}

	if data.DecodeErr() != nil {
		return tracepack.DecodeStatusItemDecodeError, 0
	}

	if trailing := data.TrailingBytes(); trailing > 0 {
		return tracepack.DecodeStatusOKWithTrailing, trailing
	}

	return tracepack.DecodeStatusOK, 0
}

// classifierName resolves the go-secs/v2 module version from the running binary's build info.
func classifierName() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return unknownName
	}

	for _, dep := range info.Deps {
		if dep.Path == goSecsModulePath {
			return "go-secs/" + dep.Version
		}
	}

	return unknownName
}
