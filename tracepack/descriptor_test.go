package tracepack

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validDescriptor returns a capture-clock raw-stream descriptor that passes Validate,
// with every optional field unset.
func validDescriptor() CaptureDescriptor {
	return CaptureDescriptor{
		ToolID:             "EQP-01",
		Transport:          TransportHSMSSS,
		CaptureMethod:      CaptureMethodRawStream,
		Vantage:            VantageHost,
		Recorder:           "recorder/1.0 live",
		RecorderInstanceID: UUID{1, 2, 3},
		TimeSource:         TimeSourceCaptureClock,
		LifecycleCoverage:  LifecycleCoverageSubscribed,
		QualityEvaluated:   true,
	}
}

// fullDescriptor returns validDescriptor with every optional field set.
func fullDescriptor() CaptureDescriptor {
	d := validDescriptor()
	d.PreviousCaptureID = new(UUID{9, 8, 7})
	d.CaptureOrigin = time.Date(2026, 10, 5, 8, 30, 0, 123, time.UTC)
	d.ClockStepTolerance = 250 * time.Millisecond
	d.SourceTZ = new("Asia/Taipei")
	d.SourceDialect = new("eap-log v2")
	d.SourceRefs = []string{"a.log", "b.log"}
	d.SiteID = new("fab-12")
	d.EquipmentModel = new("model-x")
	d.EquipmentSWRev = new("4.2")
	d.HostSoftware = new("eap 7")
	d.HostEndpoint = new("10.0.0.1:5000")
	d.EquipmentEndpoint = new("10.0.0.2:5000")
	d.EquipmentConnectMode = new(SocketRolePassive)
	d.DeviceID = new(uint64(7))
	d.HSMSTimers = &HSMSTimers{
		T1: new(uint64(500)), T2: new(uint64(10000)), T3: new(uint64(45000)), T4: new(uint64(10000)),
		T5: new(uint64(10000)), T6: new(uint64(5000)), T7: new(uint64(10000)), T8: new(uint64(5000)),
	}

	return d
}

func TestCaptureDescriptorValidateRules(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(d *CaptureDescriptor)
		field  string
		err    error
	}{
		{"empty tool", func(d *CaptureDescriptor) { d.ToolID = "" }, "tool_id", ErrInvalidKey},
		{"dot tool", func(d *CaptureDescriptor) { d.ToolID = "." }, "tool_id", ErrInvalidKey},
		{"dot-dot tool", func(d *CaptureDescriptor) { d.ToolID = ".." }, "tool_id", ErrInvalidKey},
		{"transport past the registry", func(d *CaptureDescriptor) { d.Transport = TransportSECS1Normalised + 1 }, "transport", ErrFieldValue},
		{"capture method past the registry", func(d *CaptureDescriptor) { d.CaptureMethod = CaptureMethodGenerator + 1 }, "capture_method", ErrFieldValue},
		{"vantage past the registry", func(d *CaptureDescriptor) { d.Vantage = VantageNone + 1 }, "vantage", ErrFieldValue},
		{"empty recorder", func(d *CaptureDescriptor) { d.Recorder = "" }, "recorder", ErrRequiredTag},
		{"time source past the registry", func(d *CaptureDescriptor) { d.TimeSource = TimeSourceGenerator + 1 }, "time_source", ErrFieldValue},
		{"source-log without source_tz", func(d *CaptureDescriptor) { d.TimeSource = TimeSourceSourceLog }, "source_tz", ErrRequiredTag},
		{
			"log without source_dialect",
			func(d *CaptureDescriptor) { d.CaptureMethod, d.SourceRefs = CaptureMethodLog, []string{"a.log"} },
			"source_dialect", ErrRequiredTag,
		},
		{
			"log without source_ref",
			func(d *CaptureDescriptor) { d.CaptureMethod, d.SourceDialect = CaptureMethodLog, new("eap-log") },
			"source_ref", ErrRequiredTag,
		},
		{
			"log with an empty source_ref list",
			func(d *CaptureDescriptor) {
				d.CaptureMethod, d.SourceDialect, d.SourceRefs = CaptureMethodLog, new("eap-log"), []string{}
			},
			"source_ref", ErrRequiredTag,
		},
		{"lifecycle coverage past the registry", func(d *CaptureDescriptor) { d.LifecycleCoverage = LifecycleCoverageNone + 1 }, "lifecycle_coverage", ErrFieldValue},
		{"nil recorder instance", func(d *CaptureDescriptor) { d.RecorderInstanceID = UUID{} }, "recorder_instance_id", ErrRequiredTag},
		{
			"equipment connect mode past the registry",
			func(d *CaptureDescriptor) { d.EquipmentConnectMode = new(SocketRoleActive + 1) },
			"equipment_connect_mode", ErrFieldValue,
		},
		{"negative clock-step tolerance", func(d *CaptureDescriptor) { d.ClockStepTolerance = -1 }, "clock_step_tolerance_ns", ErrFieldValue},
		{
			"the first rule in tag order wins",
			func(d *CaptureDescriptor) { d.ToolID, d.Recorder, d.ClockStepTolerance = "", "", -1 },
			"tool_id", ErrInvalidKey,
		},
		{
			"recorder before recorder_instance_id",
			func(d *CaptureDescriptor) { d.Recorder, d.RecorderInstanceID = "", UUID{} },
			"recorder", ErrRequiredTag,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := validDescriptor()
			tt.mutate(&d)

			err := d.Validate()
			var fe *FieldError
			require.ErrorAs(t, err, &fe)
			assert.Equal(t, tt.field, fe.Field)
			require.ErrorIs(t, err, tt.err)
		})
	}
}

func TestCaptureDescriptorValidateAccepts(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(d *CaptureDescriptor)
	}{
		{"defaults left zero", func(*CaptureDescriptor) {}},
		{"every optional field", func(d *CaptureDescriptor) { *d = fullDescriptor() }},
		{"unknown enum values", func(d *CaptureDescriptor) {
			d.Transport, d.CaptureMethod, d.Vantage = TransportUnknown, CaptureMethodUnknown, VantageUnknown
			d.TimeSource, d.LifecycleCoverage, d.EquipmentConnectMode = TimeSourceUnknown, LifecycleCoverageUnknown, new(SocketRoleUnknown)
		}},
		{"last registered enum values", func(d *CaptureDescriptor) {
			d.Transport, d.CaptureMethod, d.Vantage = TransportSECS1Normalised, CaptureMethodGenerator, VantageNone
			d.TimeSource, d.LifecycleCoverage, d.EquipmentConnectMode = TimeSourceGenerator, LifecycleCoverageNone, new(SocketRoleActive)
		}},
		{"a log converter", func(d *CaptureDescriptor) {
			d.CaptureMethod, d.TimeSource = CaptureMethodLog, TimeSourceSourceLog
			d.SourceTZ, d.SourceDialect, d.SourceRefs = new("UTC"), new("eap-log"), []string{"a.log"}
		}},
		{"a tool needing escapes", func(d *CaptureDescriptor) { d.ToolID = "fab 12/EQP.01" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := validDescriptor()
			tt.mutate(&d)
			require.NoError(t, d.Validate())
		})
	}
}

func TestCaptureDescriptorResolveDefaults(t *testing.T) {
	now := time.Now()

	d := validDescriptor()
	r := d.resolve(now)
	assert.True(t, r.CaptureOrigin.Equal(now))
	assert.Equal(t, now, r.CaptureOrigin, "the monotonic reading of now is kept")
	assert.Equal(t, time.Second, r.ClockStepTolerance)
	assert.True(t, d.CaptureOrigin.IsZero(), "resolve does not change its receiver")
	assert.Zero(t, d.ClockStepTolerance)

	d = fullDescriptor()
	r = d.resolve(now)
	assert.Equal(t, d.CaptureOrigin, r.CaptureOrigin)
	assert.Equal(t, 250*time.Millisecond, r.ClockStepTolerance)
}

func TestCaptureDescriptorResolveSharesNothing(t *testing.T) {
	d := fullDescriptor()
	r := d.resolve(time.Now())
	fd := fullDescriptor()
	want := fd.packMeta(testSegmentParams(""))
	before := r.packMeta(testSegmentParams(""))
	require.Equal(t, want, before)

	// Zero every value d refers to; TestFullDescriptorSetsEveryReference makes each a change.
	walkReferences(reflect.ValueOf(&d).Elem(), "", func(_ string, v reflect.Value) {
		v.Set(reflect.Zero(v.Type()))
	})

	assert.Equal(t, want, r.packMeta(testSegmentParams("")))
}

func TestFullDescriptorSetsEveryReference(t *testing.T) {
	d := fullDescriptor()
	n := 0
	walkReferences(reflect.ValueOf(&d).Elem(), "", func(path string, v reflect.Value) {
		n++
		if assert.Truef(t, v.IsValid(), "%s is nil or empty in fullDescriptor", path) {
			assert.Falsef(t, v.IsZero(), "%s refers to a zero value in fullDescriptor", path)
		}
	})
	assert.Equal(t, 20, n, "pointer and slice fields of CaptureDescriptor and HSMSTimers")
}

// walkReferences calls fn for every pointer or slice field of the struct v, under path:
// with the value a pointer refers to, or a slice's first element,
// or the zero reflect.Value for a nil pointer or an empty slice.
// A pointer to a struct is followed into that struct's fields instead.
func walkReferences(v reflect.Value, path string, fn func(path string, v reflect.Value)) {
	for i := range v.NumField() {
		f, name := v.Field(i), path+v.Type().Field(i).Name
		isPtr, isSlice := f.Kind() == reflect.Pointer, f.Kind() == reflect.Slice
		switch {
		case isPtr && f.IsNil(), isSlice && f.Len() == 0:
			fn(name, reflect.Value{})
		case isPtr && f.Elem().Kind() == reflect.Struct:
			walkReferences(f.Elem(), name+".", fn)
		case isPtr:
			fn(name, f.Elem())
		case isSlice:
			fn(name, f.Index(0))
		default:
			// A field of any other kind refers to nothing the descriptor could share.
		}
	}
}

func TestCaptureDescriptorValidateEncoding(t *testing.T) {
	const invalid = "\xff"
	tooLarge := uint64(1) << 63

	tests := []struct {
		name   string
		mutate func(d *CaptureDescriptor)
		field  string
	}{
		{"tool_id", func(d *CaptureDescriptor) { d.ToolID = invalid }, "tool_id"},
		{"recorder", func(d *CaptureDescriptor) { d.Recorder = invalid }, "recorder"},
		{"source_tz", func(d *CaptureDescriptor) { d.SourceTZ = new(invalid) }, "source_tz"},
		{"source_dialect", func(d *CaptureDescriptor) { d.SourceDialect = new(invalid) }, "source_dialect"},
		{"source_ref", func(d *CaptureDescriptor) { d.SourceRefs = []string{"a.log", invalid} }, "source_ref"},
		{"site_id", func(d *CaptureDescriptor) { d.SiteID = new(invalid) }, "site_id"},
		{"equipment_model", func(d *CaptureDescriptor) { d.EquipmentModel = new(invalid) }, "equipment_model"},
		{"equipment_sw_rev", func(d *CaptureDescriptor) { d.EquipmentSWRev = new(invalid) }, "equipment_sw_rev"},
		{"host_software", func(d *CaptureDescriptor) { d.HostSoftware = new(invalid) }, "host_software"},
		{"host_endpoint", func(d *CaptureDescriptor) { d.HostEndpoint = new(invalid) }, "host_endpoint"},
		{"equipment_endpoint", func(d *CaptureDescriptor) { d.EquipmentEndpoint = new(invalid) }, "equipment_endpoint"},
		{"device_id", func(d *CaptureDescriptor) { d.DeviceID = new(tooLarge) }, "device_id"},
		{"hsms_timers", func(d *CaptureDescriptor) { d.HSMSTimers = &HSMSTimers{T8: new(tooLarge)} }, "hsms_timers"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := validDescriptor()
			tt.mutate(&d)

			err := d.Validate()
			var fe *FieldError
			require.ErrorAs(t, err, &fe)
			assert.Equal(t, tt.field, fe.Field)
			require.ErrorIs(t, err, ErrFieldValue)
		})
	}

	t.Run("rules before encoding", func(t *testing.T) {
		d := validDescriptor()
		d.Recorder, d.DeviceID = "", new(tooLarge)
		var fe *FieldError
		require.ErrorAs(t, d.Validate(), &fe)
		assert.Equal(t, "recorder", fe.Field)
	})

	t.Run("largest encodable values", func(t *testing.T) {
		d := fullDescriptor()
		d.DeviceID = new(tooLarge - 1)
		d.HSMSTimers.T8 = new(tooLarge - 1)
		require.NoError(t, d.Validate())
	})
}

// testSegmentParams returns segment parameters for a period of the hour of fullDescriptor's CaptureOrigin,
// with the classifier named, or none for "".
func testSegmentParams(classifier string) segmentMetaParams {
	startNs := time.Date(2026, 10, 5, 8, 30, 0, 0, time.UTC).UnixNano()

	return segmentMetaParams{
		writer:      "tracepack/test",
		classifier:  classifier,
		periodStart: startNs,
		periodEnd:   startNs + int64(5*time.Minute),
		seqStart:    42,
	}
}

func TestCaptureDescriptorPackMeta(t *testing.T) {
	origin := time.Date(2026, 10, 5, 8, 30, 1, 5, time.UTC)
	timeSources := []TimeSource{TimeSourceUnknown, TimeSourceCaptureClock, TimeSourceSourceLog, TimeSourceGenerator}
	methods := []CaptureMethod{CaptureMethodUnknown, CaptureMethodRawStream, CaptureMethodDecodedMessage, CaptureMethodLog, CaptureMethodGenerator}
	classifiers := []struct {
		name        string
		classifier  string
		maxFrameLen uint64
	}{
		{"no classifier", "", 0},
		{"classifier", "secs2/v2.6.0", 0},
		{"classifier with a ceiling", "secs2/v2.6.0", 1 << 20},
	}

	for _, ts := range timeSources {
		for _, cm := range methods {
			for _, cl := range classifiers {
				t.Run(fmt.Sprintf("%s/%s/%s", ts, cm, cl.name), func(t *testing.T) {
					d := validDescriptor()
					d.TimeSource, d.CaptureMethod, d.CaptureOrigin = ts, cm, origin
					if ts == TimeSourceSourceLog {
						d.SourceTZ = new("Asia/Taipei")
					}
					if cm == CaptureMethodLog {
						d.SourceDialect, d.SourceRefs = new("eap-log"), []string{"a.log"}
					}
					require.NoError(t, d.Validate())
					r := d.resolve(time.Now())

					p := testSegmentParams(cl.classifier)
					p.maxFrameLen = cl.maxFrameLen
					m := r.packMeta(p)
					checkSegmentMeta(t, &r, p, m)
				})
			}
		}
	}
}

func TestCaptureDescriptorPackMetaEveryOptionalField(t *testing.T) {
	d := fullDescriptor()
	require.NoError(t, d.Validate())
	r := d.resolve(time.Now())
	p := testSegmentParams("secs2/v2.6.0")
	p.maxFrameLen = 1 << 20
	m := r.packMeta(p)
	checkSegmentMeta(t, &r, p, m)

	assert.Equal(t, d.PreviousCaptureID, m.PreviousCaptureID)
	assert.Equal(t, d.SourceTZ, m.SourceTZ)
	assert.Equal(t, d.SourceDialect, m.SourceDialect)
	assert.Equal(t, d.SourceRefs, m.SourceRefs)
	assert.Equal(t, d.SiteID, m.SiteID)
	assert.Equal(t, d.EquipmentModel, m.EquipmentModel)
	assert.Equal(t, d.EquipmentSWRev, m.EquipmentSWRev)
	assert.Equal(t, d.HostSoftware, m.HostSoftware)
	assert.Equal(t, d.HostEndpoint, m.HostEndpoint)
	assert.Equal(t, d.EquipmentEndpoint, m.EquipmentEndpoint)
	assert.Equal(t, d.EquipmentConnectMode, m.EquipmentConnectMode)
	assert.Equal(t, d.DeviceID, m.DeviceID)
	assert.Equal(t, d.HSMSTimers, m.HSMSTimers)
	assert.Equal(t, int64(250*time.Millisecond), int64(*m.ClockStepToleranceNs))
}

// checkSegmentMeta checks m, the pack metadata r.packMeta(p) built:
// the descriptor's values, the segment's own, the capture-clock tags for a capture-clock capture only,
// Validate with p's facts, and a round trip through MarshalBinary and UnmarshalPackMeta, the encoding a Writer runs.
func checkSegmentMeta(t *testing.T, r *CaptureDescriptor, p segmentMetaParams, m *PackMeta) {
	t.Helper()

	assert.Equal(t, r.ToolID, m.ToolID)
	assert.Equal(t, r.Transport, m.Transport)
	assert.Equal(t, r.CaptureMethod, m.CaptureMethod)
	assert.Equal(t, r.Vantage, m.Vantage)
	assert.Equal(t, r.Recorder, m.Recorder)
	assert.Equal(t, r.RecorderInstanceID, m.RecorderInstanceID)
	assert.Equal(t, r.TimeSource, m.TimeSource)
	assert.Equal(t, r.LifecycleCoverage, m.LifecycleCoverage)
	assert.Equal(t, r.QualityEvaluated, m.QualityEvaluated)

	assert.Equal(t, p.writer, m.Writer)
	assert.Equal(t, p.periodStart, m.PeriodStart)
	assert.Equal(t, p.periodEnd, m.PeriodEnd)
	assert.Equal(t, p.seqStart, m.SeqStart)
	assert.Equal(t, PackRoleSegment, m.PackRole)
	assert.Zero(t, m.CompactionLevel)
	require.NotNil(t, m.ScopeGeneration)
	assert.Zero(t, *m.ScopeGeneration)
	assert.Nil(t, m.FlushIntervalNs, "a writer without a durable spool writes no flush_interval_ns")
	assert.Nil(t, m.Supersedes)
	assert.Nil(t, m.CompactedFrom)
	assert.Nil(t, m.ReplacementSetID)
	assert.Nil(t, m.PublisherEpoch)

	if p.classifier != "" {
		assert.Equal(t, []string{p.classifier}, m.Classifiers)
	} else {
		assert.Nil(t, m.Classifiers)
	}
	if p.maxFrameLen > 0 {
		assert.Equal(t, []uint64{p.maxFrameLen}, m.MaxFrameLens)
	} else {
		assert.Nil(t, m.MaxFrameLens)
	}

	if r.TimeSource == TimeSourceCaptureClock {
		require.NotNil(t, m.CaptureOriginUTCNs)
		assert.Equal(t, r.CaptureOrigin.UnixNano(), *m.CaptureOriginUTCNs)
		require.NotNil(t, m.CaptureOriginMonoNs)
		assert.Zero(t, *m.CaptureOriginMonoNs)
		require.NotNil(t, m.ClockStepToleranceNs)
		assert.Equal(t, uint64(r.ClockStepTolerance), *m.ClockStepToleranceNs)
	} else {
		assert.Nil(t, m.CaptureOriginUTCNs)
		assert.Nil(t, m.CaptureOriginMonoNs)
		assert.Nil(t, m.ClockStepToleranceNs)
	}

	require.NoError(t, m.Validate(p.facts()))
	b, err := m.MarshalBinary()
	require.NoError(t, err)
	got, err := UnmarshalPackMeta(b)
	require.NoError(t, err)
	assert.Equal(t, m, got)
}

func TestSegmentMetaParamsFacts(t *testing.T) {
	assert.Equal(t, PackFacts{}, testSegmentParams("").facts())
	assert.Equal(t, PackFacts{AnyClassified: true}, testSegmentParams("c").facts())

	p := testSegmentParams("c")
	p.maxFrameLen = 1
	assert.Equal(t, PackFacts{AnyClassified: true, AnyOversized: true}, p.facts())

	// The facts make the classifier tags required, so metadata without them fails.
	d := validDescriptor()
	r := d.resolve(time.Now())
	m := r.packMeta(testSegmentParams(""))
	err := m.Validate(p.facts())
	var fe *FieldError
	require.ErrorAs(t, err, &fe)
	assert.Equal(t, "classifier", fe.Field)
	require.ErrorIs(t, err, ErrRequiredTag)
}
