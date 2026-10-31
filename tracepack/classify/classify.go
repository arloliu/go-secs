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

// buildName is what [Name] returns, resolved once, at package initialization.
var buildName = classifierName()

// Name identifies this classifier for the pack metadata's classifier tag of the tracepack format specification.
//
// Its value is set once, at package initialization, from the go-secs/v2 module version reported by runtime/debug.ReadBuildInfo,
// so every call returns the same string.
// It falls back to "go-secs/unknown" when build info is unavailable or does not name that dependency.
// A Go workspace build reports "go-secs/(devel)", the version a workspace gives a module it resolves to a local directory;
// a local replace keeps the required version.
func Name() string {
	return buildName
}

// Frame classifies a captured HSMS frame per the tracepack semantics specification §3 decision table.
// It evaluates the table's rows in the table's own order.
//
// maxFrameLen is the pack's configured maximum frame length; zero means no ceiling,
// so the oversized row never matches.
// It is compared with the frame's length as uint64 values, as the classifier [New] returns compares it,
// so a ceiling above the largest int is never truncated and makes no frame oversized.
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
func Frame(frame []byte, maxFrameLen uint64) (tracepack.DecodeStatus, int) {
	return classifyFrame(frame, maxFrameLen)
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
	return classifier{name: buildName, maxFrameLen: maxFrameLen}
}

// NewNamed returns a [tracepack.Classifier] that classifies frames as [New] does, under the ceiling maxFrameLen,
// and whose Name method returns name instead of [Name].
//
// A consumer of a durable bus writes every pack of a capture under one classifier configuration
// (the tracepack storage specification §4, Masking), so its classifier tag must not change with the binary's build info:
// it names the configuration explicitly.
//
// Parameters:
//   - name: the value of the classifier tag, as the pack metadata records it.
//   - maxFrameLen: the maximum frame length, counted like a frame (prefix and header included);
//     0 means no ceiling.
//
// Returns:
//   - tracepack.Classifier: the classifier, safe for concurrent use.
func NewNamed(name string, maxFrameLen uint64) tracepack.Classifier {
	return classifier{name: name, maxFrameLen: maxFrameLen}
}

// classifier is the [tracepack.Classifier] that [New] and [NewNamed] return.
type classifier struct {
	name        string
	maxFrameLen uint64
}

// Name returns [Name] for a classifier of [New], and the name given to [NewNamed].
func (c classifier) Name() string {
	return c.name
}

// MaxFrameLen returns the ceiling given to [New] or [NewNamed].
func (c classifier) MaxFrameLen() uint64 {
	return c.maxFrameLen
}

// Frame classifies frame as [Frame] does, under the ceiling given to [New] or [NewNamed].
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
