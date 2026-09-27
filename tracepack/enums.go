package tracepack

import "fmt"

// Kind classifies what a record header describes.
//
// Value 0 means unknown: the writer did not classify the record.
// See the tracepack format specification §9 for the full registry.
type Kind uint8

const (
	// KindUnknown means the writer did not classify the record.
	KindUnknown Kind = iota
	// KindData means the record carries an HSMS data message.
	KindData
	// KindControl means the record carries an HSMS control message.
	KindControl
	// KindTransportEvent means the record carries a transport lifecycle event.
	KindTransportEvent
	// KindAnnotation means the record carries an annotation.
	KindAnnotation
)

// String returns the tracepack format specification's name for k, or "unknown(<n>)" for a value outside the registry.
func (k Kind) String() string {
	switch k {
	case KindUnknown:
		return "unknown"
	case KindData:
		return "data"
	case KindControl:
		return "control"
	case KindTransportEvent:
		return "transport-event"
	case KindAnnotation:
		return "annotation"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(k))
	}
}

// Dir classifies the direction a record traveled, as observed at the capture vantage point.
//
// Value 0 means unknown.
// See the tracepack format specification §9 for the full registry.
type Dir uint8

const (
	// DirUnknown means the direction was not observed.
	DirUnknown Dir = iota
	// DirHostToEquipment means the message traveled from the host to the equipment.
	DirHostToEquipment
	// DirEquipmentToHost means the message traveled from the equipment to the host.
	DirEquipmentToHost
	// DirLocal means the record originates locally rather than on the wire.
	DirLocal
)

// String returns the tracepack format specification's name for d, or "unknown(<n>)" for a value outside the registry.
func (d Dir) String() string {
	switch d {
	case DirUnknown:
		return "unknown"
	case DirHostToEquipment:
		return "host-to-equipment"
	case DirEquipmentToHost:
		return "equipment-to-host"
	case DirLocal:
		return "local"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(d))
	}
}

// Fidelity classifies how faithfully a record's payload reproduces the bytes as transmitted.
//
// Value 0 means unknown.
// See the tracepack format specification §9 for the full registry.
type Fidelity uint8

const (
	// FidelityUnknown means the fidelity of the payload was not classified.
	FidelityUnknown Fidelity = iota
	// FidelityWireExact means the payload is the bytes as transmitted, including malformed frames.
	FidelityWireExact
	// FidelityReEncoded means the payload was produced by re-encoding a decoded message.
	FidelityReEncoded
	// FidelityReconstructed means the payload was regenerated from a non-binary source.
	FidelityReconstructed
	// FidelitySynthesized means the payload was produced by a generator and is never evidence of real traffic.
	FidelitySynthesized
	// FidelityNotApplicable means the record is a transport-event or annotation record, which have no wire payload.
	FidelityNotApplicable
)

// String returns the tracepack format specification's name for f, or "unknown(<n>)" for a value outside the registry.
func (f Fidelity) String() string {
	switch f {
	case FidelityUnknown:
		return "unknown"
	case FidelityWireExact:
		return "wire-exact"
	case FidelityReEncoded:
		return "re-encoded"
	case FidelityReconstructed:
		return "reconstructed"
	case FidelitySynthesized:
		return "synthesized"
	case FidelityNotApplicable:
		return "not-applicable"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(f))
	}
}

// DecodeStatus classifies what a classifier concluded about a record's captured bytes.
//
// Value 0 means not-attempted: the writer did not classify the record.
// See the tracepack format specification §9 for the full registry.
// See the tracepack semantics specification §3 for the decision table that produces each value.
type DecodeStatus uint8

const (
	// DecodeStatusNotAttempted means the writer did not classify the record.
	DecodeStatusNotAttempted DecodeStatus = iota
	// DecodeStatusOK means the message text held one valid item with no excess, or was empty or a well-formed control message.
	DecodeStatusOK
	// DecodeStatusOKWithTrailing means the item was valid but the message text continued after it.
	DecodeStatusOKWithTrailing
	// DecodeStatusShortFrame means the captured bytes were shorter than the minimum HSMS frame.
	DecodeStatusShortFrame
	// DecodeStatusLengthMismatch means the length field did not match the captured length.
	DecodeStatusLengthMismatch
	// DecodeStatusBadPType means the PType was not the SECS-II value defined by SEMI E37.
	DecodeStatusBadPType
	// DecodeStatusBadSType means the SType was not one of the values SEMI E37 defines.
	DecodeStatusBadSType
	// DecodeStatusControlWithBody means the frame was a control message whose length field indicated a message text.
	DecodeStatusControlWithBody
	// DecodeStatusOversized means the frame length exceeded the pack's configured maximum frame length.
	DecodeStatusOversized
	// DecodeStatusItemDecodeError means the message text did not begin with one complete, valid SECS-II item.
	DecodeStatusItemDecodeError
	// DecodeStatusReconstructedOK means a log writer parsed and built the message successfully.
	DecodeStatusReconstructedOK
	// DecodeStatusParseFailed means a log writer could not parse the source entry.
	DecodeStatusParseFailed
	// DecodeStatusBuildRejected means a log writer parsed the entry but the resulting message was invalid.
	DecodeStatusBuildRejected
	// DecodeStatusNotApplicable means the record is a transport-event or annotation record
	// that the decision table does not cover.
	DecodeStatusNotApplicable
)

// String returns the tracepack format specification's name for s, or "unknown(<n>)" for a value outside the registry.
func (s DecodeStatus) String() string {
	switch s {
	case DecodeStatusNotAttempted:
		return "not-attempted"
	case DecodeStatusOK:
		return "ok"
	case DecodeStatusOKWithTrailing:
		return "ok-with-trailing"
	case DecodeStatusShortFrame:
		return "short-frame"
	case DecodeStatusLengthMismatch:
		return "length-mismatch"
	case DecodeStatusBadPType:
		return "bad-ptype"
	case DecodeStatusBadSType:
		return "bad-stype"
	case DecodeStatusControlWithBody:
		return "control-with-body"
	case DecodeStatusOversized:
		return "oversized"
	case DecodeStatusItemDecodeError:
		return "item-decode-error"
	case DecodeStatusReconstructedOK:
		return "reconstructed-ok"
	case DecodeStatusParseFailed:
		return "parse-failed"
	case DecodeStatusBuildRejected:
		return "build-rejected"
	case DecodeStatusNotApplicable:
		return "not-applicable"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(s))
	}
}

// Malformed reports whether s is one of the malformed classifications of the tracepack semantics specification §3:
// short-frame, length-mismatch, bad-ptype, bad-stype, control-with-body, oversized, or item-decode-error.
//
// A malformed classification is what sets a record's quality.decode-failed bit;
// not-attempted and not-applicable are never malformed.
func (s DecodeStatus) Malformed() bool {
	switch s {
	case DecodeStatusShortFrame,
		DecodeStatusLengthMismatch,
		DecodeStatusBadPType,
		DecodeStatusBadSType,
		DecodeStatusControlWithBody,
		DecodeStatusOversized,
		DecodeStatusItemDecodeError:
		return true
	case DecodeStatusNotAttempted,
		DecodeStatusOK,
		DecodeStatusOKWithTrailing,
		DecodeStatusReconstructedOK,
		DecodeStatusParseFailed,
		DecodeStatusBuildRejected,
		DecodeStatusNotApplicable:
		return false
	}

	// Outside the registry: preserved but never malformed.
	return false
}

// Transport classifies which transport protocol a capture recorded.
//
// Value 0 means unknown.
// See the tracepack format specification §9 for the full registry.
type Transport uint8

const (
	// TransportUnknown means the transport was not classified.
	TransportUnknown Transport = iota
	// TransportHSMSSS means the capture is an HSMS-SS session (SEMI E37.1).
	TransportHSMSSS
	// TransportSECS1Normalised means the capture is a SECS-I session, reassembled into HSMS framing.
	TransportSECS1Normalised
)

// String returns the tracepack format specification's name for t, or "unknown(<n>)" for a value outside the registry.
func (t Transport) String() string {
	switch t {
	case TransportUnknown:
		return "unknown"
	case TransportHSMSSS:
		return "hsms-ss"
	case TransportSECS1Normalised:
		return "secs1-normalised"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(t))
	}
}

// CaptureMethod classifies which representation a capture's bytes were taken from.
//
// Value 0 means unknown.
// See the tracepack format specification §9 for the full registry.
type CaptureMethod uint8

const (
	// CaptureMethodUnknown means the capture method was not classified.
	CaptureMethodUnknown CaptureMethod = iota
	// CaptureMethodRawStream means the bytes were taken from the transport's raw byte stream before any protocol decoding.
	CaptureMethodRawStream
	// CaptureMethodDecodedMessage means the bytes were taken from messages after an HSMS or SECS-I implementation decoded them,
	// then re-encoded for storage.
	CaptureMethodDecodedMessage
	// CaptureMethodLog means the bytes were taken from a log written by other software.
	CaptureMethodLog
	// CaptureMethodGenerator means the bytes were produced by a program without being transmitted.
	CaptureMethodGenerator
)

// String returns the tracepack format specification's name for m, or "unknown(<n>)" for a value outside the registry.
func (m CaptureMethod) String() string {
	switch m {
	case CaptureMethodUnknown:
		return "unknown"
	case CaptureMethodRawStream:
		return "raw-stream"
	case CaptureMethodDecodedMessage:
		return "decoded-message"
	case CaptureMethodLog:
		return "log"
	case CaptureMethodGenerator:
		return "generator"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(m))
	}
}

// Vantage classifies where a capture's observation point was.
//
// Value 0 means unknown.
// See the tracepack format specification §9 for the full registry.
type Vantage uint8

const (
	// VantageUnknown means the vantage point was not classified.
	VantageUnknown Vantage = iota
	// VantageHost means the observation happened inside the host endpoint or on its machine.
	VantageHost
	// VantageEquipment means the observation happened inside the equipment endpoint or on its machine.
	VantageEquipment
	// VantageIntermediary means the observation happened at a relay, proxy or hub between the endpoints.
	VantageIntermediary
	// VantageNetwork means the observation was a passive tap of the link.
	VantageNetwork
	// VantageNone means nothing was observed, as with a generated capture.
	VantageNone
)

// String returns the tracepack format specification's name for v, or "unknown(<n>)" for a value outside the registry.
func (v Vantage) String() string {
	switch v {
	case VantageUnknown:
		return "unknown"
	case VantageHost:
		return "host"
	case VantageEquipment:
		return "equipment"
	case VantageIntermediary:
		return "intermediary"
	case VantageNetwork:
		return "network"
	case VantageNone:
		return "none"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(v))
	}
}

// TimeSource classifies where a capture's timestamps came from.
//
// Value 0 means unknown.
// See the tracepack format specification §9 for the full registry.
type TimeSource uint8

const (
	// TimeSourceUnknown means the time source was not classified.
	TimeSourceUnknown TimeSource = iota
	// TimeSourceCaptureClock means timestamps come from the recorder's own clock at capture time.
	TimeSourceCaptureClock
	// TimeSourceSourceLog means timestamps were parsed from a source log's own timestamps.
	TimeSourceSourceLog
	// TimeSourceGenerator means timestamps were produced by a generator rather than observed.
	TimeSourceGenerator
)

// String returns the tracepack format specification's name for s, or "unknown(<n>)" for a value outside the registry.
func (s TimeSource) String() string {
	switch s {
	case TimeSourceUnknown:
		return "unknown"
	case TimeSourceCaptureClock:
		return "capture-clock"
	case TimeSourceSourceLog:
		return "source-log"
	case TimeSourceGenerator:
		return "generator"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(s))
	}
}

// LifecycleCoverage classifies whether a capture's pack metadata claims to see every transport lifecycle event.
//
// Value 0 means unknown.
// See the tracepack format specification §9 for the full registry.
type LifecycleCoverage uint8

const (
	// LifecycleCoverageUnknown means the coverage was not classified.
	LifecycleCoverageUnknown LifecycleCoverage = iota
	// LifecycleCoverageSubscribed means the recorder subscribed to lifecycle notifications and reports them as observed.
	LifecycleCoverageSubscribed
	// LifecycleCoverageNone means the recorder does not observe transport lifecycle events at all.
	LifecycleCoverageNone
)

// String returns the tracepack format specification's name for c, or "unknown(<n>)" for a value outside the registry.
func (c LifecycleCoverage) String() string {
	switch c {
	case LifecycleCoverageUnknown:
		return "unknown"
	case LifecycleCoverageSubscribed:
		return "subscribed"
	case LifecycleCoverageNone:
		return "none"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(c))
	}
}

// PackRole classifies what a pack is for within a capture's lineage.
//
// Value 0 means unknown.
// See the tracepack format specification §9 for the full registry.
type PackRole uint8

const (
	// PackRoleUnknown means the role was not classified.
	PackRoleUnknown PackRole = iota
	// PackRoleSegment means the pack is one flush interval of an ongoing capture.
	PackRoleSegment
	// PackRoleArchive means the pack covers one complete period, such as a UTC hour.
	PackRoleArchive
	// PackRoleExtract means the pack was selected from other packs by a filter, and is never a complete period.
	PackRoleExtract
	// PackRoleRepair means the pack replaces a damaged pack after recovery.
	PackRoleRepair
	// PackRoleCorrection means the pack re-emits records under a newer classification.
	PackRoleCorrection
)

// String returns the tracepack format specification's name for r, or "unknown(<n>)" for a value outside the registry.
func (r PackRole) String() string {
	switch r {
	case PackRoleUnknown:
		return "unknown"
	case PackRoleSegment:
		return "segment"
	case PackRoleArchive:
		return "archive"
	case PackRoleExtract:
		return "extract"
	case PackRoleRepair:
		return "repair"
	case PackRoleCorrection:
		return "correction"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(r))
	}
}

// DigestAlgorithm classifies which algorithm computed a redaction digest.
//
// Value 0 means unknown.
// See the tracepack format specification §9 for the full registry.
type DigestAlgorithm uint8

const (
	// DigestAlgorithmUnknown means the algorithm was not classified.
	DigestAlgorithmUnknown DigestAlgorithm = iota
	// DigestAlgorithmHMACSHA256 means the digest is HMAC-SHA-256 per RFC 2104 with SHA-256 per FIPS 180-4.
	DigestAlgorithmHMACSHA256
)

// String returns the tracepack format specification's name for a, or "unknown(<n>)" for a value outside the registry.
func (a DigestAlgorithm) String() string {
	switch a {
	case DigestAlgorithmUnknown:
		return "unknown"
	case DigestAlgorithmHMACSHA256:
		return "hmac-sha256"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(a))
	}
}

// Event classifies a transport-event record's kind of transport lifecycle event.
//
// Value 0 means unknown.
// See the tracepack format specification §9 for the full registry.
type Event uint8

const (
	// EventUnknown means the event was not classified.
	EventUnknown Event = iota
	// EventStateTransition means the connection's logical state changed.
	EventStateTransition
	// EventTimerExpiry means an HSMS or SECS-I timer expired.
	EventTimerExpiry
	// EventSocketAccept means a passive socket accepted a peer connection.
	EventSocketAccept
	// EventSocketConnect means an active socket connected to a peer.
	EventSocketConnect
	// EventSocketClose means a socket closed.
	EventSocketClose
	// EventCaptureBoundary means the event marks a boundary of the capture itself, such as its start, stop or a gap.
	EventCaptureBoundary
	// EventClockStep means the writer's wall clock stepped against its durable anchor.
	EventClockStep
)

// String returns the tracepack format specification's name for e, or "unknown(<n>)" for a value outside the registry.
func (e Event) String() string {
	switch e {
	case EventUnknown:
		return "unknown"
	case EventStateTransition:
		return "state-transition"
	case EventTimerExpiry:
		return "timer-expiry"
	case EventSocketAccept:
		return "socket-accept"
	case EventSocketConnect:
		return "socket-connect"
	case EventSocketClose:
		return "socket-close"
	case EventCaptureBoundary:
		return "capture-boundary"
	case EventClockStep:
		return "clock-step"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(e))
	}
}

// State classifies an HSMS connection state carried by a state-transition event (SEMI E37 §5.4–§5.6).
//
// Value 0 means unknown.
// See the tracepack format specification §9 for the full registry.
type State uint8

const (
	// StateUnknown means the state was not classified.
	StateUnknown State = iota
	// StateNotConnected means the transport link is down.
	StateNotConnected
	// StateNotSelected means the transport link is up but the session is not yet selected.
	StateNotSelected
	// StateSelected means the session is selected and data flows.
	StateSelected
)

// String returns the tracepack format specification's name for s, or "unknown(<n>)" for a value outside the registry.
func (s State) String() string {
	switch s {
	case StateUnknown:
		return "unknown"
	case StateNotConnected:
		return "not-connected"
	case StateNotSelected:
		return "not-selected"
	case StateSelected:
		return "selected"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(s))
	}
}

// Timer identifies which HSMS or SECS-I timer a timer-expiry event, or a timer-caused state transition, names.
//
// Value 0 means none.
// The value is the timer number, so [TimerT1] through [TimerT8] are 1 through 8;
// see the tracepack format specification §9 for the full registry.
type Timer uint8

const (
	// TimerNone means no timer is named.
	TimerNone Timer = iota
	// TimerT1 is HSMS/SECS-I T1.
	TimerT1
	// TimerT2 is HSMS/SECS-I T2.
	TimerT2
	// TimerT3 is HSMS T3.
	TimerT3
	// TimerT4 is HSMS/SECS-I T4.
	TimerT4
	// TimerT5 is HSMS T5.
	TimerT5
	// TimerT6 is HSMS T6.
	TimerT6
	// TimerT7 is HSMS T7.
	TimerT7
	// TimerT8 is HSMS T8.
	TimerT8
)

// String returns "none" for [TimerNone], "T1" through "T8" for [TimerT1] through [TimerT8],
// or "unknown(<n>)" for a value outside the registry.
func (t Timer) String() string {
	switch t {
	case TimerNone:
		return "none"
	case TimerT1:
		return "T1"
	case TimerT2:
		return "T2"
	case TimerT3:
		return "T3"
	case TimerT4:
		return "T4"
	case TimerT5:
		return "T5"
	case TimerT6:
		return "T6"
	case TimerT7:
		return "T7"
	case TimerT8:
		return "T8"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(t))
	}
}

// Cause classifies why a state transition or socket close happened.
//
// Value 0 means unknown.
// See the tracepack format specification §9 for the full registry.
type Cause uint8

const (
	// CauseUnknown means the cause was not classified.
	CauseUnknown Cause = iota
	// CauseLocalOpen means a local open established the transport link.
	CauseLocalOpen
	// CauseLocalClose means a local close initiated the transition.
	CauseLocalClose
	// CauseSelectAccepted means the select handshake completed and the session became selected.
	CauseSelectAccepted
	// CauseSelectRejected means the peer did not grant the select.
	CauseSelectRejected
	// CauseLocalDeselect means a local deselect left the transport link up but not selected.
	CauseLocalDeselect
	// CausePeerDeselect means the peer deselected the session while the transport link stayed up.
	CausePeerDeselect
	// CauseLocalSeparate means a local separate ended the transport link.
	CauseLocalSeparate
	// CausePeerSeparate means the peer announced it was leaving and the transport link ended.
	CausePeerSeparate
	// CauseTimerExpiry means an HSMS or SECS-I timer expiry drove the transition.
	CauseTimerExpiry
	// CauseLinktestFailure means consecutive linktest probes failed up to the configured threshold.
	CauseLinktestFailure
	// CauseTransportError means a transport read or write failed.
	CauseTransportError
	// CausePeerClose means the peer closed the socket without announcing it.
	CausePeerClose
	// CauseImplementationFault means the implementation itself caused the transition,
	// such as by exiting a callback abnormally.
	CauseImplementationFault
)

// String returns the tracepack format specification's name for c, or "unknown(<n>)" for a value outside the registry.
func (c Cause) String() string {
	switch c {
	case CauseUnknown:
		return "unknown"
	case CauseLocalOpen:
		return "local-open"
	case CauseLocalClose:
		return "local-close"
	case CauseSelectAccepted:
		return "select-accepted"
	case CauseSelectRejected:
		return "select-rejected"
	case CauseLocalDeselect:
		return "local-deselect"
	case CausePeerDeselect:
		return "peer-deselect"
	case CauseLocalSeparate:
		return "local-separate"
	case CausePeerSeparate:
		return "peer-separate"
	case CauseTimerExpiry:
		return "timer-expiry"
	case CauseLinktestFailure:
		return "linktest-failure"
	case CauseTransportError:
		return "transport-error"
	case CausePeerClose:
		return "peer-close"
	case CauseImplementationFault:
		return "implementation-fault"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(c))
	}
}

// SocketRole classifies whether a socket event's side of the connection is passive or active.
//
// Value 0 means unknown.
// See the tracepack format specification §9 for the full registry.
type SocketRole uint8

const (
	// SocketRoleUnknown means the role was not classified.
	SocketRoleUnknown SocketRole = iota
	// SocketRolePassive means the side listens and accepts a peer connection.
	SocketRolePassive
	// SocketRoleActive means the side dials out to a peer.
	SocketRoleActive
)

// String returns the tracepack format specification's name for r, or "unknown(<n>)" for a value outside the registry.
func (r SocketRole) String() string {
	switch r {
	case SocketRoleUnknown:
		return "unknown"
	case SocketRolePassive:
		return "passive"
	case SocketRoleActive:
		return "active"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(r))
	}
}

// BoundaryKind classifies which capture boundary a capture-boundary event marks.
//
// Value 0 means unknown.
// See the tracepack format specification §9 for the full registry.
type BoundaryKind uint8

const (
	// BoundaryKindUnknown means the boundary kind was not classified.
	BoundaryKindUnknown BoundaryKind = iota
	// BoundaryKindStart means the capture started.
	BoundaryKindStart
	// BoundaryKindStop means the capture stopped cleanly.
	BoundaryKindStop
	// BoundaryKindGap means the capture is missing an interval of records.
	BoundaryKindGap
	// BoundaryKindStopUnclean means the capture ended without a clean stop, as written by recovery.
	BoundaryKindStopUnclean
)

// String returns the tracepack format specification's name for k, or "unknown(<n>)" for a value outside the registry.
func (k BoundaryKind) String() string {
	switch k {
	case BoundaryKindUnknown:
		return "unknown"
	case BoundaryKindStart:
		return "start"
	case BoundaryKindStop:
		return "stop"
	case BoundaryKindGap:
		return "gap"
	case BoundaryKindStopUnclean:
		return "stop-unclean"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(k))
	}
}

// AnnotationKind classifies what an annotation record records.
//
// Value 0 means unknown.
// See the tracepack format specification §9 for the full registry.
type AnnotationKind uint8

const (
	// AnnotationKindUnknown means the annotation kind was not classified.
	AnnotationKindUnknown AnnotationKind = iota
	// AnnotationKindNote means the annotation is free text.
	AnnotationKindNote
	// AnnotationKindUnparsedEntry means a log writer could not parse the source entry.
	AnnotationKindUnparsedEntry
	// AnnotationKindSkippedBytes means the writer skipped bytes it could not attribute to any record.
	AnnotationKindSkippedBytes
	// AnnotationKindUnrecognisedLine means a log writer did not recognize the source line's format.
	AnnotationKindUnrecognisedLine
)

// String returns the tracepack format specification's name for k, or "unknown(<n>)" for a value outside the registry.
func (k AnnotationKind) String() string {
	switch k {
	case AnnotationKindUnknown:
		return "unknown"
	case AnnotationKindNote:
		return "note"
	case AnnotationKindUnparsedEntry:
		return "unparsed-entry"
	case AnnotationKindSkippedBytes:
		return "skipped-bytes"
	case AnnotationKindUnrecognisedLine:
		return "unrecognised-line"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(k))
	}
}

// Codec classifies which compression codec encodes a block body or the footer.
//
// Value 0 means none: the encoded bytes are the decoded bytes verbatim.
// See the tracepack format specification §2 for the full registry.
type Codec uint8

const (
	// CodecNone means the encoded bytes are the decoded bytes verbatim.
	CodecNone Codec = iota
	// CodecZstd means the encoded bytes are exactly one Zstandard frame per RFC 8878, with no dictionary.
	CodecZstd
)

// String returns the tracepack format specification's name for c, or "unknown(<n>)" for a value outside the registry.
//
// An unknown codec value is not itself an error: it means the reader cannot decode the block or footer it encodes,
// which the reader reports as unreadable rather than rejecting outright.
func (c Codec) String() string {
	switch c {
	case CodecNone:
		return "none"
	case CodecZstd:
		return "zstd"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(c))
	}
}
