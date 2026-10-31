package tracepack

import (
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// descriptorMeta returns a capture-clock raw-stream PackMeta holding every producer-owned entry a descriptor carries:
// a nonzero capture_origin_mono_ns, a zero clock_step_tolerance_ns,
// every optional site and equipment tag, and two thinning entries.
func descriptorMeta() *PackMeta {
	role := SocketRolePassive

	return &PackMeta{
		ToolID:               "tool/A.1",
		Transport:            TransportHSMSSS,
		CaptureMethod:        CaptureMethodRawStream,
		Vantage:              VantageHost,
		Recorder:             "gateway/1.2.3",
		TimeSource:           TimeSourceCaptureClock,
		CaptureOriginUTCNs:   new(int64(1_700_000_000_000_000_000)),
		CaptureOriginMonoNs:  new(int64(987_654_321)),
		LifecycleCoverage:    LifecycleCoverageSubscribed,
		QualityEvaluated:     true,
		RecorderInstanceID:   UUID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x47, 0x08, 0x89, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F, 0x10},
		PreviousCaptureID:    &UUID{0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x47, 0x18, 0x89, 0x1A, 0x1B, 0x1C, 0x1D, 0x1E, 0x1F, 0x20},
		SiteID:               new("fab-1"),
		EquipmentModel:       new("model-x"),
		EquipmentSWRev:       new("rev-2"),
		HostSoftware:         new("eap 4.5"),
		HostEndpoint:         new("10.0.0.1:5000"),
		EquipmentEndpoint:    new("10.0.0.2:5000"),
		EquipmentConnectMode: &role,
		DeviceID:             new(uint64(7)),
		HSMSTimers:           &HSMSTimers{T3: new(uint64(45_000)), T5: new(uint64(10_000))},
		ClockStepToleranceNs: new(uint64(0)),
		Thinning: []Thinning{
			{Stream: 1, Function: 3, Source: "fdc interval poll"},
			{Stream: 1, Function: 4, Source: "fdc interval reply"},
		},
	}
}

// descriptorEntries returns the TLV entries of descriptorMeta in the order EncodeDescriptor writes them,
// so a test can drop, repeat or add an entry.
func descriptorEntries(t *testing.T) []tlv.Entry {
	t.Helper()

	b, err := EncodeDescriptor(descriptorMeta())
	require.NoError(t, err)
	entries, err := tlv.Decode(b)
	require.NoError(t, err)

	return entries
}

// encodeTLV encodes entries as they are, without any registry check.
func encodeTLV(entries []tlv.Entry) []byte {
	var b []byte
	for _, e := range entries {
		b = tlv.AppendEntry(b, e)
	}

	return b
}

// replaceDescriptorEntry returns entries with every entry of tag replaced by repl, in place.
func replaceDescriptorEntry(entries []tlv.Entry, tag uint16, repl tlv.Entry) []tlv.Entry {
	out := make([]tlv.Entry, 0, len(entries))
	for _, e := range entries {
		if e.Tag == tag {
			e = repl
		}
		out = append(out, e)
	}

	return out
}

func TestDescriptorRoundTrip(t *testing.T) {
	t.Parallel()

	m := descriptorMeta()
	b, err := EncodeDescriptor(m)
	require.NoError(t, err)

	got, err := DecodeDescriptor(b)
	require.NoError(t, err)
	assert.Equal(t, m, got, "every producer-owned value decodes as encoded")
	assert.Equal(t, int64(987_654_321), *got.CaptureOriginMonoNs, "a nonzero monotonic origin is kept")
	assert.Equal(t, uint64(0), *got.ClockStepToleranceNs, "a zero tolerance is kept, no default applied")
	assert.Nil(t, got.Unknown)

	again, err := EncodeDescriptor(got)
	require.NoError(t, err)
	assert.Equal(t, b, again, "a decoded descriptor encodes to the same bytes")

	// The header carries the bytes in standard base64 with padding.
	text := base64.StdEncoding.EncodeToString(b)
	raw, err := base64.StdEncoding.Strict().DecodeString(text)
	require.NoError(t, err)
	assert.Equal(t, b, raw)
}

// recorder_instance_name, of format 1.2, is a producer-owned value: it round-trips, the empty string and a "/" included,
// and its entry follows thinning in tag order.
func TestDescriptorRecorderInstanceName(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"eqp", "", "hub/eqp"} {
		m := descriptorMeta()
		m.RecorderInstanceName = new(name)
		b, err := EncodeDescriptor(m)
		require.NoError(t, err)

		got, err := DecodeDescriptor(b)
		require.NoError(t, err)
		assert.Equal(t, m, got, "name %q", name)

		entries, err := tlv.Decode(b)
		require.NoError(t, err)
		last := entries[len(entries)-1]
		assert.Equal(t, tagRecorderInstanceName, last.Tag, "name %q", name)
		assert.Equal(t, tlv.TypeUTF8, last.Type, "name %q", name)
	}
}

func TestDescriptorRoundTripMinimal(t *testing.T) {
	t.Parallel()

	m := &PackMeta{
		ToolID:               "t",
		Transport:            TransportHSMSSS,
		CaptureMethod:        CaptureMethodDecodedMessage,
		Recorder:             "r",
		TimeSource:           TimeSourceCaptureClock,
		CaptureOriginUTCNs:   new(int64(-5)),
		CaptureOriginMonoNs:  new(int64(0)),
		RecorderInstanceID:   UUID{1},
		ClockStepToleranceNs: new(uint64(1)),
	}
	b, err := EncodeDescriptor(m)
	require.NoError(t, err)
	got, err := DecodeDescriptor(b)
	require.NoError(t, err)
	assert.Equal(t, m, got)
}

func TestEncodeDescriptorOmitsConsumerValues(t *testing.T) {
	t.Parallel()

	m := descriptorMeta()
	m.Writer = "consumer/1"
	m.Classifiers = []string{"classify/1"}
	m.MaxFrameLens = []uint64{1 << 20}
	m.PeriodStart, m.PeriodEnd = 10, 20
	m.SeqStart = 42
	m.PackRole = PackRoleSegment
	m.CompactionLevel = 1
	m.ScopeGeneration = new(uint64(0))
	m.FlushIntervalNs = new(uint64(60_000_000_000))
	m.RedactionPolicy = &RedactionPolicy{PolicyID: "p", PolicyVersion: 1, KeyID: "k", DigestAlgorithm: DigestAlgorithmHMACSHA256}
	m.Notes = new("note")
	m.SourceTZ = new("UTC")
	m.Unknown = []RawEntry{{Tag: 0x7F00, Type: uint8(tlv.TypeU8), Value: []byte{1}}}

	b, err := EncodeDescriptor(m)
	require.NoError(t, err)
	want, err := EncodeDescriptor(descriptorMeta())
	require.NoError(t, err)
	assert.Equal(t, want, b, "consumer-owned values and unknown entries are never encoded")
}

func TestEncodeDescriptorRefuses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(m *PackMeta)
	}{
		{"capture method log", func(m *PackMeta) { m.CaptureMethod = CaptureMethodLog }},
		{"capture method unknown", func(m *PackMeta) { m.CaptureMethod = CaptureMethodUnknown }},
		{"time source source-log", func(m *PackMeta) { m.TimeSource = TimeSourceSourceLog }},
		{"no capture_origin_utc_ns", func(m *PackMeta) { m.CaptureOriginUTCNs = nil }},
		{"no capture_origin_mono_ns", func(m *PackMeta) { m.CaptureOriginMonoNs = nil }},
		{"no clock_step_tolerance_ns", func(m *PackMeta) { m.ClockStepToleranceNs = nil }},
		{"thinning stream 128", func(m *PackMeta) { m.Thinning[1].Stream = 128 }},
		{"thinning function 256", func(m *PackMeta) { m.Thinning[0].Function = 256 }},
		{"device_id above 2^63-1", func(m *PackMeta) { m.DeviceID = new(uint64(1 << 63)) }},
		{"invalid UTF-8", func(m *PackMeta) { m.SiteID = new("\xff") }},
		{"invalid UTF-8 in recorder_instance_name", func(m *PackMeta) { m.RecorderInstanceName = new("\xff") }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := descriptorMeta()
			tt.mutate(m)
			b, err := EncodeDescriptor(m)
			require.Error(t, err)
			assert.Nil(t, b)
		})
	}

	_, err := EncodeDescriptor(nil)
	require.Error(t, err)
}

func TestDescriptorScope(t *testing.T) {
	t.Parallel()

	t.Run("accepted capture methods", func(t *testing.T) {
		t.Parallel()

		for _, cm := range []CaptureMethod{CaptureMethodRawStream, CaptureMethodDecodedMessage} {
			b := encodeTLV(replaceDescriptorEntry(descriptorEntries(t), tagCaptureMethod, tlv.U8Entry(tagCaptureMethod, uint8(cm))))
			m, err := DecodeDescriptor(b)
			require.NoError(t, err, "capture_method %s", cm)
			assert.Equal(t, cm, m.CaptureMethod)
		}
	})

	refused := []struct {
		name    string
		entries func(t *testing.T) []tlv.Entry
	}{}
	for _, v := range []uint8{0, 3, 4, 5, 200} {
		refused = append(refused, struct {
			name    string
			entries func(t *testing.T) []tlv.Entry
		}{fmt.Sprintf("capture_method %d", v), func(t *testing.T) []tlv.Entry {
			return replaceDescriptorEntry(descriptorEntries(t), tagCaptureMethod, tlv.U8Entry(tagCaptureMethod, v))
		}})
	}
	for _, v := range []uint8{0, 2, 3, 4} {
		refused = append(refused, struct {
			name    string
			entries func(t *testing.T) []tlv.Entry
		}{fmt.Sprintf("time_source %d", v), func(t *testing.T) []tlv.Entry {
			return replaceDescriptorEntry(descriptorEntries(t), tagTimeSource, tlv.U8Entry(tagTimeSource, v))
		}})
	}

	unknown := tlv.U64Entry(0x7F00, 1)
	thinningWith := func(sub ...tlv.Entry) tlv.Entry { return tlv.NestedEntry(tagThinning, sub) }
	stream, function, source := tlv.U64Entry(tagThinningStream, 1), tlv.U64Entry(tagThinningFunction, 3), tlv.UTF8Entry(tagThinningSource, "s")
	for _, c := range []struct {
		name  string
		extra []tlv.Entry
	}{
		{"single unknown top-level tag", []tlv.Entry{unknown}},
		{"repeated unknown top-level tag", []tlv.Entry{unknown, unknown}},
		{"retired top-level tag", []tlv.Entry{tlv.U64Entry(0x0001, 1)}},
		{"consumer-owned writer", []tlv.Entry{tlv.UTF8Entry(tagWriter, "w")}},
		{"consumer-owned seq_start", []tlv.Entry{tlv.U64Entry(tagSeqStart, 0)}},
		{"consumer-owned redaction_policy", []tlv.Entry{tlv.NestedEntry(tagRedactionPolicy, []tlv.Entry{
			tlv.UTF8Entry(1, "p"), tlv.U64Entry(2, 1), tlv.UTF8Entry(3, "k"), tlv.U8Entry(4, 1),
		})}},
		{"log tag source_tz", []tlv.Entry{tlv.UTF8Entry(tagSourceTZ, "UTC")}},
		{"notes", []tlv.Entry{tlv.UTF8Entry(tagNotes, "n")}},
		{"unknown nested tag in thinning", []tlv.Entry{thinningWith(stream, function, source, tlv.U64Entry(0x0004, 1))}},
		{"thinning stream 128", []tlv.Entry{thinningWith(tlv.U64Entry(tagThinningStream, 128), function, source)}},
		{"thinning function 256", []tlv.Entry{thinningWith(stream, tlv.U64Entry(tagThinningFunction, 256), source)}},
		{"thinning without source", []tlv.Entry{thinningWith(stream, function)}},
		{"duplicate tool_id", []tlv.Entry{tlv.UTF8Entry(tagToolID, "again")}},
	} {
		extra := c.extra
		refused = append(refused, struct {
			name    string
			entries func(t *testing.T) []tlv.Entry
		}{c.name, func(t *testing.T) []tlv.Entry { return append(descriptorEntries(t), extra...) }})
	}
	for _, tag := range []uint16{9, 0x8000} {
		refused = append(refused, struct {
			name    string
			entries func(t *testing.T) []tlv.Entry
		}{fmt.Sprintf("hsms_timers nested 0x%04X", tag), func(t *testing.T) []tlv.Entry {
			timers := tlv.NestedEntry(tagHSMSTimers, []tlv.Entry{tlv.U64Entry(3, 45_000), tlv.U64Entry(tag, 1)})

			return replaceDescriptorEntry(descriptorEntries(t), tagHSMSTimers, timers)
		}})
	}
	for _, tag := range []uint16{tagToolID, tagRecorderInstanceID, tagCaptureOriginUTCNs, tagCaptureOriginMonoNs, tagClockStepToleranceNs, tagQualityEvaluated} {
		refused = append(refused, struct {
			name    string
			entries func(t *testing.T) []tlv.Entry
		}{fmt.Sprintf("missing 0x%04X", tag), func(t *testing.T) []tlv.Entry {
			var out []tlv.Entry
			for _, e := range descriptorEntries(t) {
				if e.Tag != tag {
					out = append(out, e)
				}
			}

			return out
		}})
	}

	for _, tt := range refused {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			b := encodeTLV(tt.entries(t))
			m, err := DecodeDescriptor(b)
			require.Error(t, err)
			assert.Nil(t, m)
		})
	}

	t.Run("malformed frame", func(t *testing.T) {
		t.Parallel()

		b := descriptorBytes(t)
		for _, cut := range [][]byte{b[:len(b)-1], b[:3], append(append([]byte{}, b...), 0x01)} {
			_, err := DecodeDescriptor(cut)
			require.Error(t, err)
		}
		_, err := DecodeDescriptor(nil)
		require.Error(t, err, "an empty descriptor misses every required tag")
	})
}

func TestDecodeDescriptorKeepsNonzeroReservedByte(t *testing.T) {
	t.Parallel()

	b := descriptorBytes(t)
	b[3] = 0xA5 // the reserved byte of the first entry header
	m, err := DecodeDescriptor(b)
	require.NoError(t, err)
	assert.Equal(t, descriptorMeta(), m)
}

func TestDecodeDescriptorReordered(t *testing.T) {
	t.Parallel()

	entries := descriptorEntries(t)
	reversed := make([]tlv.Entry, 0, len(entries))
	for i := len(entries) - 1; i >= 0; i-- {
		reversed = append(reversed, entries[i])
	}
	m, err := DecodeDescriptor(encodeTLV(reversed))
	require.NoError(t, err)

	want := descriptorMeta()
	want.Thinning[0], want.Thinning[1] = want.Thinning[1], want.Thinning[0]
	assert.Equal(t, want, m, "thinning keeps its stored order; every other value is order-free")
}

func TestDecodeDescriptorEveryTimer(t *testing.T) {
	t.Parallel()

	want := descriptorMeta()
	want.HSMSTimers = &HSMSTimers{
		T1: new(uint64(1)), T2: new(uint64(2)), T3: new(uint64(3)), T4: new(uint64(4)),
		T5: new(uint64(5)), T6: new(uint64(6)), T7: new(uint64(7)), T8: new(uint64(8)),
	}
	b, err := EncodeDescriptor(want)
	require.NoError(t, err)
	m, err := DecodeDescriptor(b)
	require.NoError(t, err)
	assert.Equal(t, want, m, "every timer T1 to T8 is a nested tag of hsms_timers")
}

// descriptorBytes returns the encoding of descriptorMeta.
func descriptorBytes(t testing.TB) []byte {
	t.Helper()

	b, err := EncodeDescriptor(descriptorMeta())
	require.NoError(t, err)

	return b
}
