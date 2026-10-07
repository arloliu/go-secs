package tracepack

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// DefaultClockStepTolerance is the clock-step tolerance a CaptureDescriptor with a zero ClockStepTolerance takes.
//
// An operating system corrects a small clock offset by slewing the clock gradually and steps it only for a larger one,
// beyond 128 ms by default for ntpd.
// One second keeps slewed corrections from being recorded as steps;
// a step between that threshold and one second is absorbed within the tolerance, not recorded,
// while a step of a second or more is recorded.
// A larger tolerance would also widen the interval by which crash recovery bounds a gap (the tracepack storage specification §4).
const DefaultClockStepTolerance = time.Second

// CaptureDescriptor is the pack metadata the producer of one capture owns,
// which a SegmentWriter writes unchanged into every segment of the capture
// (the tracepack storage specification §4, "Capture descriptor and pack metadata").
//
// The SegmentWriter owns the rest of the pack metadata:
// writer, classifier, max_frame_len, period_start, period_end, seq_start, pack_role, compaction_level and scope_generation,
// and the capture_id it generates.
// It writes no flush_interval_ns, since it writes over a sink without a durable spool.
//
// A pointer or slice field is written only when it is non-nil or non-empty.
// Validate checks the descriptor before a SegmentWriter takes it.
type CaptureDescriptor struct {
	// ToolID is the tool this capture belongs to (tool_id); required.
	// It names the key areas the capture's packs are stored under,
	// so it must be a tool a key can hold: not empty, "." or "..".
	ToolID string
	// Transport is the transport protocol the capture recorded (transport).
	Transport Transport
	// CaptureMethod is the representation the capture's bytes were taken from (capture_method,
	// the tracepack semantics specification §2), which decides the fidelity of every frame record.
	CaptureMethod CaptureMethod
	// Vantage is where the capture's observation point was (vantage, the tracepack semantics specification §2).
	Vantage Vantage
	// Recorder names the recorder or converter product, its version and its mode (recorder); required.
	// It is for display and tracing; no reader branches on it.
	Recorder string
	// RecorderInstanceID is the stable identity of the recorder or converter deployment across restarts (recorder_instance_id);
	// required.
	// It is never generated:
	// the deployment persists it and gives the same value to every capture it runs,
	// which links its captures across restarts (the tracepack format specification I-7).
	RecorderInstanceID UUID
	// PreviousCaptureID is the capture this deployment ran immediately before this one (previous_capture_id); nil for none.
	// A recorder persists each SegmentWriter's CaptureID to give it here to the next capture.
	PreviousCaptureID *UUID
	// TimeSource says where the capture's timestamps come from (time_source).
	TimeSource TimeSource
	// CaptureOrigin is the start of the capture: the time of its start boundary,
	// and for a capture-clock capture the wall clock written as capture_origin_utc_ns,
	// with capture_origin_mono_ns 0, so mono_ns is measured from this instant (the tracepack semantics specification §4).
	// The zero value takes the time NewSegmentWriter runs.
	// A capture-clock recorder keeps the monotonic reading of the time.Time it gives,
	// since mono_ns is measured from it.
	CaptureOrigin time.Time
	// ClockStepTolerance is the drift between the wall and monotonic clocks, measured against the clock anchor,
	// that a capture-clock capture tolerates before it records a clock step
	// (clock_step_tolerance_ns, the tracepack semantics specification §4);
	// it is written for a capture-clock capture only, and must not be negative.
	// Zero takes DefaultClockStepTolerance.
	ClockStepTolerance time.Duration
	// LifecycleCoverage says whether the capture sees every transport lifecycle event (lifecycle_coverage,
	// the tracepack semantics specification §5).
	LifecycleCoverage LifecycleCoverage
	// QualityEvaluated declares that a clear stored quality bit of a record means its condition was checked absent
	// (quality_evaluated, the tracepack semantics specification §6).
	// The SegmentWriter derives two bits on every record, whichever method appends it:
	// capture-boundary, set on a capture-boundary event and cleared on any other record,
	// and correlation-incomplete, added on epoch 0.
	// The producer evaluates ordering-uncertain, direction-inferred
	// and the other causes of correlation-incomplete, such as missing source metadata, before it sets the field:
	// AppendFrame sets none of them, so a producer that uses it asserts that those conditions do not occur,
	// and a record given to Append carries the producer's bits plus the derived ones.
	QualityEvaluated bool

	// SourceTZ is the IANA time-zone name the source timestamps are parsed in (source_tz);
	// required for a source-log time source.
	SourceTZ *string
	// SourceDialect is the log format and parser mode used (source_dialect, the tracepack storage specification §7);
	// required for a log capture method.
	SourceDialect *string
	// SourceRefs identifies each source file (source_ref, one entry per value);
	// at least one is required for a log capture method.
	SourceRefs []string

	// SiteID is the site or fab identifier (site_id).
	SiteID *string
	// EquipmentModel is the equipment model (equipment_model).
	EquipmentModel *string
	// EquipmentSWRev is the equipment software revision (equipment_sw_rev).
	EquipmentSWRev *string
	// HostSoftware is the host-side software, such as an EAP, with its name and version (host_software).
	HostSoftware *string
	// HostEndpoint is the host side of the connection, "address:port" (host_endpoint).
	HostEndpoint *string
	// EquipmentEndpoint is the equipment side of the connection, "address:port" (equipment_endpoint).
	EquipmentEndpoint *string
	// EquipmentConnectMode says whether the equipment side is passive or active (equipment_connect_mode).
	EquipmentConnectMode *SocketRole
	// DeviceID is the configured SessionID or DeviceID (device_id).
	DeviceID *uint64
	// HSMSTimers are the configured HSMS timers (hsms_timers).
	HSMSTimers *HSMSTimers
}

// Validate checks d against the "Required when" rules of the pack metadata tag registry that the producer's values decide
// (the tracepack format specification §5),
// and against the values the registry and its encoding allow,
// so a descriptor that passes is one every segment of the capture can be written with.
//
// A zero CaptureOrigin passes, since NewSegmentWriter replaces it with the time it runs,
// and a zero ClockStepTolerance passes, since it takes its default.
//
// Returns:
//   - error: nil, or a *FieldError for the first rule violated, in the order of the registry's tags:
//     tool_id that no key can hold (wrapping ErrInvalidKey, as NewStoreSource refuses such a tool);
//     transport, capture_method or vantage outside the registry (ErrFieldValue);
//     an empty recorder (ErrRequiredTag);
//     time_source outside the registry (ErrFieldValue);
//     source_tz missing for a source-log time source,
//     source_dialect or source_ref missing for a log capture method (ErrRequiredTag);
//     lifecycle_coverage outside the registry (ErrFieldValue);
//     a nil recorder_instance_id (ErrRequiredTag);
//     equipment_connect_mode outside the registry (ErrFieldValue);
//     a negative clock_step_tolerance_ns (ErrFieldValue).
//     Then, for a value the encoding refuses (the tracepack format specification §2),
//     such as a string that is not valid UTF-8 or a device_id or hsms_timers value above 2^63-1,
//     a *FieldError naming the field, whose Err wraps ErrFieldValue and the encoding's reason.
func (d *CaptureDescriptor) Validate() error {
	if err := checkKeyTool(d.ToolID); err != nil {
		return &FieldError{Field: "tool_id", Err: keyArgError(err)}
	}

	checks := []struct {
		field string
		bad   bool
		err   error
	}{
		{"transport", d.Transport > TransportSECS1Normalised, ErrFieldValue},
		{"capture_method", d.CaptureMethod > CaptureMethodGenerator, ErrFieldValue},
		{"vantage", d.Vantage > VantageNone, ErrFieldValue},
		{"recorder", d.Recorder == "", ErrRequiredTag},
		{"time_source", d.TimeSource > TimeSourceGenerator, ErrFieldValue},
		{"source_tz", d.TimeSource == TimeSourceSourceLog && d.SourceTZ == nil, ErrRequiredTag},
		{"source_dialect", d.CaptureMethod == CaptureMethodLog && d.SourceDialect == nil, ErrRequiredTag},
		{"source_ref", d.CaptureMethod == CaptureMethodLog && len(d.SourceRefs) == 0, ErrRequiredTag},
		{"lifecycle_coverage", d.LifecycleCoverage > LifecycleCoverageNone, ErrFieldValue},
		{"recorder_instance_id", d.RecorderInstanceID.IsZero(), ErrRequiredTag},
		{"equipment_connect_mode", d.EquipmentConnectMode != nil && *d.EquipmentConnectMode > SocketRoleActive, ErrFieldValue},
		{"clock_step_tolerance_ns", d.ClockStepTolerance < 0, ErrFieldValue},
	}

	for _, c := range checks {
		if c.bad {
			return &FieldError{Field: c.field, Err: c.err}
		}
	}

	return d.checkEncoding()
}

// descriptorTagNames names the pack metadata tags of a CaptureDescriptor whose values the encoding can refuse.
var descriptorTagNames = map[uint16]string{
	tagToolID:            "tool_id",
	tagRecorder:          "recorder",
	tagSourceTZ:          "source_tz",
	tagSourceDialect:     "source_dialect",
	tagSourceRef:         "source_ref",
	tagSiteID:            "site_id",
	tagEquipmentModel:    "equipment_model",
	tagEquipmentSWRev:    "equipment_sw_rev",
	tagHostSoftware:      "host_software",
	tagHostEndpoint:      "host_endpoint",
	tagEquipmentEndpoint: "equipment_endpoint",
	tagDeviceID:          "device_id",
	tagHSMSTimers:        "hsms_timers",
}

// checkEncoding encodes the pack metadata of a segment of d, as a SegmentWriter will,
// and reports a value the encoding refuses as a *FieldError naming its field.
func (d *CaptureDescriptor) checkEncoding() error {
	r := d.resolve(time.Unix(0, 0))
	_, err := r.packMeta(segmentMetaParams{}).MarshalBinary()
	if err == nil {
		return nil
	}

	// The rules MarshalBinary reports as a *FieldError hold for a descriptor that passed the checks above,
	// so the error comes from the encoding and names the tag; any other error is returned as it is.
	var ee *tlv.EntryError
	if !errors.As(err, &ee) {
		return err
	}
	field, known := descriptorTagNames[ee.Tag]
	if !known {
		field = fmt.Sprintf("0x%04X", ee.Tag)
	}

	return &FieldError{Field: field, Err: fmt.Errorf("%w: %w", ErrFieldValue, ee.Err)}
}

// clone returns a copy of d that shares no slice or pointer with it,
// so a SegmentWriter keeps the descriptor it was given whatever the caller changes afterwards.
func (d *CaptureDescriptor) clone() CaptureDescriptor {
	c := *d
	c.PreviousCaptureID = clonePtr(d.PreviousCaptureID)
	c.SourceTZ = clonePtr(d.SourceTZ)
	c.SourceDialect = clonePtr(d.SourceDialect)
	c.SourceRefs = slices.Clone(d.SourceRefs)
	c.SiteID = clonePtr(d.SiteID)
	c.EquipmentModel = clonePtr(d.EquipmentModel)
	c.EquipmentSWRev = clonePtr(d.EquipmentSWRev)
	c.HostSoftware = clonePtr(d.HostSoftware)
	c.HostEndpoint = clonePtr(d.HostEndpoint)
	c.EquipmentEndpoint = clonePtr(d.EquipmentEndpoint)
	c.EquipmentConnectMode = clonePtr(d.EquipmentConnectMode)
	c.DeviceID = clonePtr(d.DeviceID)
	if d.HSMSTimers != nil {
		t := *d.HSMSTimers
		for _, p := range []**uint64{&t.T1, &t.T2, &t.T3, &t.T4, &t.T5, &t.T6, &t.T7, &t.T8} {
			*p = clonePtr(*p)
		}
		c.HSMSTimers = &t
	}

	return c
}

// resolve returns a copy of d, as clone does, with its defaults applied:
// a zero CaptureOrigin becomes now, and a zero ClockStepTolerance becomes DefaultClockStepTolerance.
func (d *CaptureDescriptor) resolve(now time.Time) CaptureDescriptor {
	c := d.clone()
	if c.CaptureOrigin.IsZero() {
		c.CaptureOrigin = now
	}
	if c.ClockStepTolerance == 0 {
		c.ClockStepTolerance = DefaultClockStepTolerance
	}

	return c
}

// segmentMetaParams are the pack metadata values a SegmentWriter owns for one segment.
type segmentMetaParams struct {
	// writer is the writer tag.
	writer string
	// classifier is the classifier tag; empty when the capture has no classifier.
	classifier string
	// maxFrameLen is the max_frame_len tag; 0 when the classifier has no ceiling.
	maxFrameLen uint64
	// periodStart and periodEnd are the segment's period, in ns since the Unix epoch, end exclusive.
	periodStart int64
	periodEnd   int64
	// seqStart is the capture's next seq when the segment opens.
	seqStart uint64
}

// facts returns the PackFacts a segment written with p is validated against:
// a classifier may classify any record, and a classifier with a ceiling may classify one oversized.
func (p segmentMetaParams) facts() PackFacts {
	return PackFacts{
		AnyClassified: p.classifier != "",
		AnyOversized:  p.maxFrameLen > 0,
	}
}

// packMeta returns the pack metadata of one segment of the capture d describes, with the values p gives:
// d's values unchanged, and pack_role segment, compaction_level 0 and scope_generation 0.
// For a capture-clock capture it writes capture_origin_utc_ns from CaptureOrigin, capture_origin_mono_ns 0
// and clock_step_tolerance_ns from ClockStepTolerance;
// for any other time source it writes none of the three.
//
// d is a descriptor resolve returned, so its CaptureOrigin and ClockStepTolerance are set,
// and the SegmentWriter owns it, so the result may share d's slices and pointers.
func (d *CaptureDescriptor) packMeta(p segmentMetaParams) *PackMeta {
	m := &PackMeta{
		ToolID:               d.ToolID,
		Transport:            d.Transport,
		CaptureMethod:        d.CaptureMethod,
		Vantage:              d.Vantage,
		Recorder:             d.Recorder,
		Writer:               p.writer,
		TimeSource:           d.TimeSource,
		SourceTZ:             d.SourceTZ,
		SourceDialect:        d.SourceDialect,
		SourceRefs:           d.SourceRefs,
		PeriodStart:          p.periodStart,
		PeriodEnd:            p.periodEnd,
		LifecycleCoverage:    d.LifecycleCoverage,
		QualityEvaluated:     d.QualityEvaluated,
		PackRole:             PackRoleSegment,
		CompactionLevel:      0,
		RecorderInstanceID:   d.RecorderInstanceID,
		PreviousCaptureID:    d.PreviousCaptureID,
		SiteID:               d.SiteID,
		EquipmentModel:       d.EquipmentModel,
		EquipmentSWRev:       d.EquipmentSWRev,
		HostSoftware:         d.HostSoftware,
		HostEndpoint:         d.HostEndpoint,
		EquipmentEndpoint:    d.EquipmentEndpoint,
		EquipmentConnectMode: d.EquipmentConnectMode,
		DeviceID:             d.DeviceID,
		HSMSTimers:           d.HSMSTimers,
		SeqStart:             p.seqStart,
		ScopeGeneration:      new(uint64(0)),
	}
	if p.classifier != "" {
		m.Classifiers = []string{p.classifier}
	}
	if p.maxFrameLen > 0 {
		m.MaxFrameLens = []uint64{p.maxFrameLen}
	}
	if d.TimeSource == TimeSourceCaptureClock {
		m.CaptureOriginUTCNs = new(d.CaptureOrigin.UnixNano())
		m.CaptureOriginMonoNs = new(int64(0))
		m.ClockStepToleranceNs = new(uint64(d.ClockStepTolerance))
	}

	return m
}
