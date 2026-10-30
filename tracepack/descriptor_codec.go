package tracepack

import (
	"errors"
	"fmt"

	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// errNotDescriptorTag reports a top-level tag a capture descriptor does not carry,
// or a nested tag a thinning entry does not define.
var errNotDescriptorTag = errors.New("tracepack: tag not in a capture descriptor")

// Largest stream and function a SECS-II message carries, which bound a thinning entry.
const (
	maxThinningStream   = 127
	maxThinningFunction = 255
)

// descriptorRegistry is the tag registry of a capture descriptor:
// the pack metadata tags the producer owns (the tracepack storage specification §4, "Capture descriptor and pack metadata"),
// each as the pack metadata registry defines it,
// with capture_origin_utc_ns, capture_origin_mono_ns and clock_step_tolerance_ns required,
// since the bus serves capture-clock captures only.
var descriptorRegistry = func() tlv.Registry {
	required := []uint16{
		tagToolID, tagTransport, tagCaptureMethod, tagVantage, tagRecorder, tagTimeSource,
		tagLifecycleCoverage, tagQualityEvaluated, tagRecorderInstanceID,
		tagCaptureOriginUTCNs, tagCaptureOriginMonoNs, tagClockStepToleranceNs,
	}
	optional := []uint16{
		tagPreviousCaptureID,
		tagSiteID, tagEquipmentModel, tagEquipmentSWRev, tagHostSoftware, tagHostEndpoint, tagEquipmentEndpoint,
		tagEquipmentConnectMode, tagDeviceID, tagHSMSTimers,
		tagThinning, tagRecorderInstanceName,
	}

	reg := make(tlv.Registry, len(required)+len(optional))
	for _, tag := range required {
		f := tlv.PackMetadata[tag]
		f.Required = true
		reg[tag] = f
	}
	for _, tag := range optional {
		reg[tag] = tlv.PackMetadata[tag]
	}

	return reg
}()

// EncodeDescriptor encodes the capture descriptor of the capture m describes:
// the producer-owned entries of its pack metadata, in the TLV encoding of the tracepack format specification §5
// (the tracepack storage specification §4, "Capture descriptor and pack metadata").
// They are tool_id, transport, capture_method, vantage, recorder, time_source, lifecycle_coverage, quality_evaluated,
// recorder_instance_id, capture_origin_utc_ns, capture_origin_mono_ns and clock_step_tolerance_ns,
// previous_capture_id and the site and equipment tags when m holds them
// (site_id, equipment_model, equipment_sw_rev, host_software, host_endpoint, equipment_endpoint,
// equipment_connect_mode, device_id and hsms_timers),
// every thinning entry, and recorder_instance_name when m holds it, in tag order and thinning in its own order.
// Every other value of m, the consumer-owned ones and m.Unknown included, is not encoded;
// m's values are written as they are, with no default applied.
//
// A producer publishes the result in the Tpk-Capture-Descriptor header of its start message
// in standard base64 with padding (RFC 4648 §4), as base64.StdEncoding encodes it.
// The result decodes with DecodeDescriptor to the values encoded.
//
// Returns:
//   - []byte: the descriptor; nil on error.
//   - error: a *FieldError wrapping ErrFieldValue for a capture_method other than raw-stream or decoded-message,
//     a time_source other than capture-clock, which layout 1 of the bus does not serve,
//     or a thinning entry whose stream is above 127 or whose function is above 255;
//     an error reporting a missing required tag for a nil CaptureOriginUTCNs, CaptureOriginMonoNs or ClockStepToleranceNs;
//     or an error naming the first value the decoder would reject, such as a string that is not valid UTF-8.
func EncodeDescriptor(m *PackMeta) ([]byte, error) {
	if m == nil {
		return nil, errors.New("tracepack: encode capture descriptor: nil pack metadata")
	}
	if err := checkDescriptorValues(m); err != nil {
		return nil, err
	}

	entries := []tlv.Entry{
		tlv.UTF8Entry(tagToolID, m.ToolID),
		tlv.U8Entry(tagTransport, uint8(m.Transport)),
		tlv.U8Entry(tagCaptureMethod, uint8(m.CaptureMethod)),
		tlv.U8Entry(tagVantage, uint8(m.Vantage)),
		tlv.UTF8Entry(tagRecorder, m.Recorder),
		tlv.U8Entry(tagTimeSource, uint8(m.TimeSource)),
	}
	entries = appendPresent(entries, []optionalEntry{
		{m.CaptureOriginUTCNs != nil, func() tlv.Entry { return tlv.I64Entry(tagCaptureOriginUTCNs, *m.CaptureOriginUTCNs) }},
		{m.CaptureOriginMonoNs != nil, func() tlv.Entry { return tlv.I64Entry(tagCaptureOriginMonoNs, *m.CaptureOriginMonoNs) }},
	})
	entries = append(entries,
		tlv.U8Entry(tagLifecycleCoverage, uint8(m.LifecycleCoverage)),
		tlv.BoolEntry(tagQualityEvaluated, m.QualityEvaluated),
		tlv.UUIDEntry(tagRecorderInstanceID, [16]byte(m.RecorderInstanceID)),
	)
	entries = appendPresent(entries, []optionalEntry{
		{m.PreviousCaptureID != nil, func() tlv.Entry { return tlv.UUIDEntry(tagPreviousCaptureID, [16]byte(*m.PreviousCaptureID)) }},
		{m.SiteID != nil, func() tlv.Entry { return tlv.UTF8Entry(tagSiteID, *m.SiteID) }},
		{m.EquipmentModel != nil, func() tlv.Entry { return tlv.UTF8Entry(tagEquipmentModel, *m.EquipmentModel) }},
		{m.EquipmentSWRev != nil, func() tlv.Entry { return tlv.UTF8Entry(tagEquipmentSWRev, *m.EquipmentSWRev) }},
		{m.HostSoftware != nil, func() tlv.Entry { return tlv.UTF8Entry(tagHostSoftware, *m.HostSoftware) }},
		{m.HostEndpoint != nil, func() tlv.Entry { return tlv.UTF8Entry(tagHostEndpoint, *m.HostEndpoint) }},
		{m.EquipmentEndpoint != nil, func() tlv.Entry { return tlv.UTF8Entry(tagEquipmentEndpoint, *m.EquipmentEndpoint) }},
		{m.EquipmentConnectMode != nil, func() tlv.Entry { return tlv.U8Entry(tagEquipmentConnectMode, uint8(*m.EquipmentConnectMode)) }},
		{m.DeviceID != nil, func() tlv.Entry { return tlv.U64Entry(tagDeviceID, *m.DeviceID) }},
		{m.HSMSTimers != nil, func() tlv.Entry { return m.HSMSTimers.entry() }},
		{m.ClockStepToleranceNs != nil, func() tlv.Entry { return tlv.U64Entry(tagClockStepToleranceNs, *m.ClockStepToleranceNs) }},
	})
	for _, th := range m.Thinning {
		entries = append(entries, th.entry())
	}
	if m.RecorderInstanceName != nil {
		entries = append(entries, tlv.UTF8Entry(tagRecorderInstanceName, *m.RecorderInstanceName))
	}

	return encodeEntries(entries, descriptorRegistry, "capture descriptor")
}

// DecodeDescriptor decodes the capture descriptor b, as EncodeDescriptor encodes it,
// into a PackMeta holding only the producer-owned values, each as decoded, with no default applied:
// a capture_origin_mono_ns other than 0 and a clock_step_tolerance_ns of 0 are kept
// (the tracepack storage specification §4, "Capture descriptor and pack metadata").
// Every other field of the result is zero, so a consumer copies the producer's values
// and fills the values it owns itself.
// CaptureDescriptor, the recorder-facing type, applies its defaults; DecodeDescriptor applies none.
//
// A descriptor the consumer cannot accept does not decode:
// a top-level tag a descriptor does not carry, single or repeated,
// whether unknown, retired, or a pack metadata tag the producer does not own, such as writer, seq_start or source_tz;
// a nested tag a thinning entry does not define, or a nested tag of hsms_timers that no timer defines;
// a thinning entry whose stream is above 127 or whose function is above 255;
// a capture_method other than raw-stream or decoded-message, or a time_source other than capture-clock;
// a missing required value, capture_origin_utc_ns, capture_origin_mono_ns and clock_step_tolerance_ns included;
// and any frame or value the TLV encoding refuses (the tracepack format specification §5).
// The reserved byte of each entry header is ignored, and entries may come in any order.
// A reader of the pack metadata, unlike DecodeDescriptor, skips the nested tags of hsms_timers that no timer defines.
//
// Returns:
//   - *PackMeta: the producer-owned values; nil on error.
//   - error: a *FieldError naming the tag or field the descriptor breaks,
//     or an error reporting the first entry that the TLV encoding or the descriptor's tag registry refuses.
func DecodeDescriptor(b []byte) (*PackMeta, error) {
	entries, err := tlv.Decode(b)
	if err != nil {
		return nil, fmt.Errorf("tracepack: decode capture descriptor: %w", err)
	}
	if err := checkDescriptorTags(entries); err != nil {
		return nil, err
	}
	if err := tlv.Validate(entries, descriptorRegistry); err != nil {
		return nil, fmt.Errorf("tracepack: validate capture descriptor: %w", err)
	}

	m := &PackMeta{}
	for _, e := range entries {
		if err := packMetaSetters[e.Tag](m, e); err != nil {
			return nil, fmt.Errorf("tracepack: decode capture descriptor: %w", err)
		}
	}
	if err := checkDescriptorValues(m); err != nil {
		return nil, err
	}

	return m, nil
}

// checkDescriptorTags reports the first entry of a descriptor whose tag a descriptor does not carry,
// or the first nested tag of a thinning or hsms_timers entry that its registry does not define.
func checkDescriptorTags(entries []tlv.Entry) error {
	for _, e := range entries {
		if _, known := descriptorRegistry[e.Tag]; !known {
			return &FieldError{Field: descriptorTagName(e.Tag), Err: errNotDescriptorTag}
		}
		var name string
		var nestedRegistry tlv.Registry
		switch e.Tag {
		case tagThinning:
			name, nestedRegistry = "thinning", tlv.Thinning
		case tagHSMSTimers:
			name, nestedRegistry = "hsms_timers", tlv.HSMSTimers
		default:
			continue
		}
		// The registry check rejects a nested value that is not a TLV list; this scan looks only at well-framed ones.
		nested, err := tlv.Decode(e.Value)
		if err != nil {
			continue
		}
		for _, n := range nested {
			if _, known := nestedRegistry[n.Tag]; !known {
				return &FieldError{Field: fmt.Sprintf("%s 0x%04X", name, n.Tag), Err: errNotDescriptorTag}
			}
		}
	}

	return nil
}

// descriptorTagName returns the specification name of a pack metadata tag, or "0x%04X" for an unknown one.
func descriptorTagName(tag uint16) string {
	if f, known := tlv.PackMetadata[tag]; known {
		return f.Name
	}

	return fmt.Sprintf("0x%04X", tag)
}

// checkDescriptorValues reports the first producer-owned value of m that layout 1 of the bus cannot carry:
// a capture method or time source it does not serve, or a thinning entry no SECS-II message can match.
func checkDescriptorValues(m *PackMeta) error {
	if m.CaptureMethod != CaptureMethodRawStream && m.CaptureMethod != CaptureMethodDecodedMessage {
		return &FieldError{Field: "capture_method", Err: fmt.Errorf("%w: %s is not served by the bus", ErrFieldValue, m.CaptureMethod)}
	}
	if m.TimeSource != TimeSourceCaptureClock {
		return &FieldError{Field: "time_source", Err: fmt.Errorf("%w: %s is not served by the bus", ErrFieldValue, m.TimeSource)}
	}
	for i, th := range m.Thinning {
		if th.Stream > maxThinningStream || th.Function > maxThinningFunction {
			return &FieldError{
				Field: "thinning",
				Err:   fmt.Errorf("%w: entry %d names S%dF%d, outside S0-S127, F0-F255", ErrFieldValue, i, th.Stream, th.Function),
			}
		}
	}

	return nil
}
