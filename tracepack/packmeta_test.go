package tracepack_test

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// Golden pack metadata is hand-built from the entry layout of the tracepack format specification §5,
// hex of python3 struct.pack('<HBBI', tag, value_type, 0, len(value)) + value for each entry, concatenated in tag order.
const (
	// pmAlways15Hex holds only the 15 unconditionally required tags.
	// It passes the registry check but neither decodes nor marshals,
	// because pack_role = segment also requires scope_generation, which the metadata itself shows.
	pmAlways15Hex = "0200060004000000746f6f6c030001000100000001040001000100000001050001000100000001060006000300000072656307000600020000007772090001000100000003100003000800000" +
		"0e803000000000000110003000800000" +
		"0d007000000000000120001000100000002130002000100000001180001000100000001190001000100000000" +
		"1b00050010000000000102030405060708090a0b0c0d0e0f27000400080000000000000000000000"
	// pmMinimal16Hex adds scope_generation = 0, making the pack marshalable under PackFacts{} (PackRole = segment).
	pmMinimal16Hex = pmAlways15Hex + "2d000400080000000000000000000000"
	// pmWithUnknownHex is pmMinimal16Hex plus one entry of an unknown tag (0x0050, bytes, 0xAA 0xBB),
	// which MarshalBinary re-encodes after the typed fields.
	pmWithUnknownHex = pmMinimal16Hex + "5000070002000000aabb"
)

// mustHexBytes decodes a hex constant of these tests, failing t on a typo.
func mustHexBytes(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	require.NoError(t, err)

	return b
}

// basePackMeta returns a minimal PackMeta that satisfies every unconditional rule and,
// with PackRole segment and ScopeGeneration 0, every conditional rule too:
// the struct MarshalBinary encodes to pmMinimal16Hex.
func basePackMeta() *tracepack.PackMeta {
	var recorderID tracepack.UUID
	for i := range recorderID {
		recorderID[i] = byte(i)
	}
	scopeGen := uint64(0)

	return &tracepack.PackMeta{
		ToolID:             "tool",
		Transport:          tracepack.TransportHSMSSS,
		CaptureMethod:      tracepack.CaptureMethodRawStream,
		Vantage:            tracepack.VantageHost,
		Recorder:           "rec",
		Writer:             "wr",
		TimeSource:         tracepack.TimeSourceGenerator,
		PeriodStart:        1000,
		PeriodEnd:          2000,
		LifecycleCoverage:  tracepack.LifecycleCoverageNone,
		QualityEvaluated:   true,
		PackRole:           tracepack.PackRoleSegment,
		CompactionLevel:    0,
		RecorderInstanceID: recorderID,
		SeqStart:           0,
		ScopeGeneration:    &scopeGen,
	}
}

func uuidOfByte(b byte) tracepack.UUID {
	var u tracepack.UUID
	for i := range u {
		u[i] = b
	}

	return u
}

func TestUUIDStringParseRoundTrip(t *testing.T) {
	t.Parallel()

	u, err := tracepack.ParseUUID("f47ac10b-58cc-4372-a567-0e02b2c3d479")
	require.NoError(t, err)
	assert.Equal(t, "f47ac10b-58cc-4372-a567-0e02b2c3d479", u.String())
	assert.False(t, u.IsZero())

	var zero tracepack.UUID
	assert.True(t, zero.IsZero())
	assert.Equal(t, "00000000-0000-0000-0000-000000000000", zero.String())
}

func TestParseUUIDInvalid(t *testing.T) {
	t.Parallel()

	_, err := tracepack.ParseUUID("not-a-uuid")
	require.Error(t, err)
}

func TestPackMetaMarshalMinimalGolden(t *testing.T) {
	t.Parallel()

	got, err := basePackMeta().MarshalBinary()
	require.NoError(t, err)
	assert.Equal(t, pmMinimal16Hex, hex.EncodeToString(got))
}

func TestPackMetaMarshalRejectsValuesItsDecoderRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		edit func(m *tracepack.PackMeta)
		want error
	}{
		{"tool_id not valid UTF-8", func(m *tracepack.PackMeta) { m.ToolID = "\xff" }, tlv.ErrUTF8},
		{"device_id above 2^63-1", func(m *tracepack.PackMeta) { m.DeviceID = new(uint64(1) << 63) }, tlv.ErrLimit},
		{"seq_start above 2^63-1", func(m *tracepack.PackMeta) { m.SeqStart = uint64(1) << 63 }, tlv.ErrLimit},
		{"nested policy_id not valid UTF-8", func(m *tracepack.PackMeta) {
			m.RedactionPolicy = &tracepack.RedactionPolicy{PolicyID: "\xc3", KeyID: "k"}
		}, tlv.ErrUTF8},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := basePackMeta()
			tt.edit(m)
			b, err := m.MarshalBinary()
			require.ErrorIs(t, err, tt.want)
			assert.Nil(t, b)
		})
	}
}

func TestPackMetaMarshalLimitValuesRoundTrip(t *testing.T) {
	t.Parallel()

	m := basePackMeta()
	m.ToolID = "\u5de5\u5177"
	m.DeviceID = new(uint64(1)<<63 - 1)
	b, err := m.MarshalBinary()
	require.NoError(t, err)

	got, err := tracepack.UnmarshalPackMeta(b)
	require.NoError(t, err)
	assert.Equal(t, m, got)
}

func TestUnmarshalPackMetaRejectsMissingIntrinsicTags(t *testing.T) {
	t.Parallel()

	// time_source (tag 0x0009) patched from generator (3) to capture-clock (1),
	// which requires capture_origin_utc_ns, capture_origin_mono_ns and clock_step_tolerance_ns.
	captureClockHex := strings.Replace(pmMinimal16Hex, "090001000100000003", "090001000100000001", 1)
	require.NotEqual(t, pmMinimal16Hex, captureClockHex)

	tests := []struct {
		name  string
		hex   string
		field string
	}{
		{"segment without scope_generation", pmAlways15Hex, "scope_generation"},
		{"capture-clock without origins", captureClockHex, "capture_origin_utc_ns"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m, err := tracepack.UnmarshalPackMeta(mustHexBytes(t, tt.hex))
			require.ErrorIs(t, err, tracepack.ErrRequiredTag)
			var fe *tracepack.FieldError
			require.ErrorAs(t, err, &fe)
			assert.Equal(t, tt.field, fe.Field)
			assert.Nil(t, m)
		})
	}
}

func TestUnmarshalPackMetaDefersFactDependentTags(t *testing.T) {
	t.Parallel()

	// pmMinimal16Hex has no classifier, max_frame_len, redaction or flush_interval_ns:
	// only facts about the records or the writer could require them.
	m, err := tracepack.UnmarshalPackMeta(mustHexBytes(t, pmMinimal16Hex))
	require.NoError(t, err)

	var fe *tracepack.FieldError
	require.ErrorAs(t, m.Validate(tracepack.PackFacts{AnyClassified: true}), &fe)
	assert.Equal(t, "classifier", fe.Field)
}

func TestUnmarshalPackMetaMinimalRoundTrip(t *testing.T) {
	t.Parallel()

	m, err := tracepack.UnmarshalPackMeta(mustHexBytes(t, pmMinimal16Hex))
	require.NoError(t, err)
	assert.Equal(t, "tool", m.ToolID)
	assert.Equal(t, tracepack.TransportHSMSSS, m.Transport)
	require.NotNil(t, m.ScopeGeneration)
	assert.Equal(t, uint64(0), *m.ScopeGeneration)
	assert.Empty(t, m.Unknown)

	again, err := m.MarshalBinary()
	require.NoError(t, err)
	assert.Equal(t, pmMinimal16Hex, hex.EncodeToString(again))
}

// TestPackMetaRetiredTags checks the retired pack metadata tags 0x0001 (schema_version), 0x0014 and 0x0030 (blocks_validated)
// (the tracepack format specification §5): a reader keeps them in Unknown like any unknown tag,
// and MarshalBinary never writes them, while it keeps a tag that is merely unknown.
func TestPackMetaRetiredTags(t *testing.T) {
	t.Parallel()

	const (
		schemaVersionHex   = "01000400080000000100000000000000"
		retired14Hex       = "1400020001000000" + "01"
		blocksValidatedHex = "3000020001000000" + "01"
	)
	m, err := tracepack.UnmarshalPackMeta(mustHexBytes(t, schemaVersionHex+pmWithUnknownHex+retired14Hex+blocksValidatedHex))
	require.NoError(t, err)
	tags := make([]uint16, 0, len(m.Unknown))
	for _, e := range m.Unknown {
		tags = append(tags, e.Tag)
	}
	assert.Equal(t, []uint16{0x0001, 0x0050, 0x0014, 0x0030}, tags, "retired tags are read as unknown entries")

	again, err := m.MarshalBinary()
	require.NoError(t, err)
	assert.Equal(t, pmWithUnknownHex, hex.EncodeToString(again), "retired tags are never written")
}

// TestPackMetaRetiredPackRole checks the retired pack_role 5 (the tracepack format specification §9):
// read and reported as unknown(5), never written.
func TestPackMetaRetiredPackRole(t *testing.T) {
	t.Parallel()

	// pack_role is the u8 entry 18 00 01 00 01000000 01 (segment); make its value 5.
	b := strings.Replace(pmMinimal16Hex, "18000100010000000"+"1", "18000100010000000"+"5", 1)
	require.NotEqual(t, pmMinimal16Hex, b)
	m, err := tracepack.UnmarshalPackMeta(mustHexBytes(t, b))
	require.NoError(t, err)
	assert.Equal(t, "unknown(5)", m.PackRole.String())

	_, err = m.MarshalBinary()
	var fe *tracepack.FieldError
	require.ErrorAs(t, err, &fe)
	assert.Equal(t, "pack_role", fe.Field)
	require.ErrorIs(t, err, tracepack.ErrFieldValue)
}

// TestPackMetaReplacementSetValues checks that a replacement set has exactly one member
// (the tracepack storage specification §6): any other size or index is rejected on write and on read.
func TestPackMetaReplacementSetValues(t *testing.T) {
	t.Parallel()

	generation := func() *tracepack.PackMeta {
		m := basePackMeta()
		m.PackRole = tracepack.PackRoleArchive
		m.CompactionLevel = 1
		m.CompactedFrom = []tracepack.UUID{uuidOfByte(1)}
		m.ScopeGeneration = new(uint64(1))
		m.PublisherEpoch = new(uint64(1))
		m.ReplacementSetID = new(uuidOfByte(2))
		m.ReplacementSetSize = new(uint64(1))
		m.ReplacementSetIndex = new(uint64(0))

		return m
	}
	good, err := generation().MarshalBinary()
	require.NoError(t, err)

	for _, tt := range []struct {
		field string
		mut   func(m *tracepack.PackMeta)
		from  string
		to    string
	}{
		{"replacement_set_size", func(m *tracepack.PackMeta) { m.ReplacementSetSize = new(uint64(2)) },
			"2a0004000800000001", "2a0004000800000002"},
		{"replacement_set_index", func(m *tracepack.PackMeta) { m.ReplacementSetIndex = new(uint64(1)) },
			"2b0004000800000000", "2b0004000800000001"},
	} {
		m := generation()
		tt.mut(m)
		_, err := m.MarshalBinary()
		var fe *tracepack.FieldError
		require.ErrorAs(t, err, &fe, tt.field)
		assert.Equal(t, tt.field, fe.Field)
		require.ErrorIs(t, err, tracepack.ErrFieldValue, tt.field)

		bad := strings.Replace(hex.EncodeToString(good), tt.from, tt.to, 1)
		require.NotEqual(t, hex.EncodeToString(good), bad, tt.field)
		_, err = tracepack.UnmarshalPackMeta(mustHexBytes(t, bad))
		require.ErrorIs(t, err, tracepack.ErrFieldValue, tt.field)
	}
}

func TestPackMetaUnknownEntryRoundTrip(t *testing.T) {
	t.Parallel()

	m, err := tracepack.UnmarshalPackMeta(mustHexBytes(t, pmWithUnknownHex))
	require.NoError(t, err)
	require.Len(t, m.Unknown, 1)
	assert.Equal(t, uint16(0x0050), m.Unknown[0].Tag)
	assert.Equal(t, uint8(7), m.Unknown[0].Type)
	assert.Equal(t, []byte{0xAA, 0xBB}, m.Unknown[0].Value)

	again, err := m.MarshalBinary()
	require.NoError(t, err)
	assert.Equal(t, pmWithUnknownHex, hex.EncodeToString(again))
}

func TestPackMetaMarshalRejectsUnknownEntryNamingKnownTag(t *testing.T) {
	t.Parallel()

	m := basePackMeta()
	m.Unknown = []tracepack.RawEntry{{Tag: 0x0002, Type: 6, Value: []byte("x")}}
	_, err := m.MarshalBinary()
	require.ErrorIs(t, err, tracepack.ErrReservedTag)
}

func TestPackMetaNestedRepeatableRoundTrip(t *testing.T) {
	t.Parallel()

	m := basePackMeta()
	m.PackRole = tracepack.PackRoleRepair
	m.CompactionLevel = 1
	m.CaptureMethod = tracepack.CaptureMethodLog
	m.SourceDialect = new("sml")
	m.SourceRefs = []string{"a.log", "b.log"}
	m.Supersedes = []tracepack.UUID{uuidOfByte(1)}
	m.CompactedFrom = []tracepack.UUID{uuidOfByte(2), uuidOfByte(3)}
	m.Coverage = []tracepack.Coverage{
		{CaptureID: new(uuidOfByte(4)), SeqFirst: new(uint64(10)), SeqLast: new(uint64(20))},
		{TimeStart: new(int64(-5)), TimeEnd: new(int64(5))},
	}
	m.RedactionPolicy = &tracepack.RedactionPolicy{
		PolicyID:        "p1",
		PolicyVersion:   3,
		KeyID:           "k1",
		DigestAlgorithm: tracepack.DigestAlgorithmHMACSHA256,
	}
	m.Redaction = []tracepack.RedactionEntry{
		{
			Seq:          7,
			ItemPath:     new("4"),
			MaskedRanges: []tracepack.MaskedRange{{Offset: 0, Length: 4}, {Offset: 10, Length: 2}},
			Domain:       "process-program",
			Digest:       []byte{1, 2, 3, 4},
		},
		{
			Seq:          8,
			MaskedRanges: []tracepack.MaskedRange{{Offset: 0, Length: 1}},
			Domain:       "message-text",
			Digest:       []byte{5, 6, 7, 8},
		},
	}
	m.HSMSTimers = &tracepack.HSMSTimers{T1: new(uint64(5000)), T3: new(uint64(45000))}

	facts := tracepack.PackFacts{CoverageLoss: true, RedactionPolicyApplied: true, AnyRedacted: true}
	require.NoError(t, m.Validate(facts))

	b, err := m.MarshalBinary()
	require.NoError(t, err)

	got, err := tracepack.UnmarshalPackMeta(b)
	require.NoError(t, err)
	assert.Equal(t, m, got)
}

func TestPackMetaValidateRequiredWhen(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		field string
		setup func(m *tracepack.PackMeta, facts *tracepack.PackFacts)
	}{
		{
			"classifier required when any record classified",
			"classifier",
			func(_ *tracepack.PackMeta, facts *tracepack.PackFacts) { facts.AnyClassified = true },
		},
		{
			"capture_origin_utc_ns required when time_source capture-clock",
			"capture_origin_utc_ns",
			func(m *tracepack.PackMeta, _ *tracepack.PackFacts) { m.TimeSource = tracepack.TimeSourceCaptureClock },
		},
		{
			"capture_origin_mono_ns required when time_source capture-clock",
			"capture_origin_mono_ns",
			func(m *tracepack.PackMeta, _ *tracepack.PackFacts) {
				m.TimeSource = tracepack.TimeSourceCaptureClock
				m.CaptureOriginUTCNs = new(int64(1))
			},
		},
		{
			"source_tz required when time_source source-log",
			"source_tz",
			func(m *tracepack.PackMeta, _ *tracepack.PackFacts) { m.TimeSource = tracepack.TimeSourceSourceLog },
		},
		{
			"source_dialect required when capture_method log",
			"source_dialect",
			func(m *tracepack.PackMeta, _ *tracepack.PackFacts) { m.CaptureMethod = tracepack.CaptureMethodLog },
		},
		{
			"source_ref required when capture_method log",
			"source_ref",
			func(m *tracepack.PackMeta, _ *tracepack.PackFacts) {
				m.CaptureMethod = tracepack.CaptureMethodLog
				m.SourceDialect = new("sml")
			},
		},
		{
			"max_frame_len required when any record oversized",
			"max_frame_len",
			func(m *tracepack.PackMeta, facts *tracepack.PackFacts) {
				facts.AnyOversized = true
				m.Classifier = new("go-secs/test")
			},
		},
		{
			"supersedes required for a patch",
			"supersedes",
			func(m *tracepack.PackMeta, _ *tracepack.PackFacts) { m.PackRole = tracepack.PackRoleRepair },
		},
		{
			"supersedes required for a generation with a predecessor",
			"supersedes",
			func(m *tracepack.PackMeta, facts *tracepack.PackFacts) {
				m.ScopeGeneration = new(uint64(1))
				facts.GenerationHasPredecessor = true
			},
		},
		{
			"coverage required when a repair lost data",
			"coverage",
			func(m *tracepack.PackMeta, facts *tracepack.PackFacts) {
				m.PackRole = tracepack.PackRoleRepair
				m.Supersedes = []tracepack.UUID{uuidOfByte(1)}
				facts.CoverageLoss = true
			},
		},
		{
			"compacted_from required when compaction_level >= 1",
			"compacted_from",
			func(m *tracepack.PackMeta, _ *tracepack.PackFacts) { m.CompactionLevel = 1 },
		},
		{
			"extract_filter required when pack_role extract",
			"extract_filter",
			func(m *tracepack.PackMeta, _ *tracepack.PackFacts) { m.PackRole = tracepack.PackRoleExtract },
		},
		{
			"clock_step_tolerance_ns required when time_source capture-clock",
			"clock_step_tolerance_ns",
			func(m *tracepack.PackMeta, _ *tracepack.PackFacts) {
				m.TimeSource = tracepack.TimeSourceCaptureClock
				m.CaptureOriginUTCNs = new(int64(1))
				m.CaptureOriginMonoNs = new(int64(2))
			},
		},
		{
			"flush_interval_ns required for a durable recorder",
			"flush_interval_ns",
			func(_ *tracepack.PackMeta, facts *tracepack.PackFacts) { facts.DurableRecorder = true },
		},
		{
			"scope_generation required unless pack_role extract",
			"scope_generation",
			func(m *tracepack.PackMeta, _ *tracepack.PackFacts) { m.ScopeGeneration = nil },
		},
		{
			"publisher_epoch required when scope_generation >= 1",
			"publisher_epoch",
			func(m *tracepack.PackMeta, _ *tracepack.PackFacts) { m.ScopeGeneration = new(uint64(1)) },
		},
		{
			"patch_base required for a patch whose scope has a generation",
			"patch_base",
			func(m *tracepack.PackMeta, facts *tracepack.PackFacts) {
				m.PackRole = tracepack.PackRoleRepair
				m.Supersedes = []tracepack.UUID{uuidOfByte(1)}
				facts.ScopeHasGeneration = true
			},
		},
		{
			"replacement_set_id required when scope_generation >= 1",
			"replacement_set_id",
			func(m *tracepack.PackMeta, _ *tracepack.PackFacts) {
				m.ScopeGeneration = new(uint64(1))
				m.PublisherEpoch = new(uint64(1))
			},
		},
		{
			"replacement_set_size required with replacement_set_id",
			"replacement_set_size",
			func(m *tracepack.PackMeta, _ *tracepack.PackFacts) {
				m.ReplacementSetID = new(uuidOfByte(5))
			},
		},
		{
			"replacement_set_index required with replacement_set_id",
			"replacement_set_index",
			func(m *tracepack.PackMeta, _ *tracepack.PackFacts) {
				m.ReplacementSetID = new(uuidOfByte(5))
				m.ReplacementSetSize = new(uint64(1))
			},
		},
		{
			"redaction_policy required when written under a redaction policy",
			"redaction_policy",
			func(_ *tracepack.PackMeta, facts *tracepack.PackFacts) { facts.RedactionPolicyApplied = true },
		},
		{
			"redaction required when a record was masked",
			"redaction",
			func(_ *tracepack.PackMeta, facts *tracepack.PackFacts) { facts.AnyRedacted = true },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := basePackMeta()
			var facts tracepack.PackFacts
			tt.setup(m, &facts)

			err := m.Validate(facts)
			require.Error(t, err)
			var fe *tracepack.FieldError
			require.ErrorAs(t, err, &fe)
			assert.Equal(t, tt.field, fe.Field)
			assert.ErrorIs(t, err, tracepack.ErrRequiredTag)
		})
	}
}

func TestPackMetaValidatePasses(t *testing.T) {
	t.Parallel()

	require.NoError(t, basePackMeta().Validate(tracepack.PackFacts{}))
}

func TestMarshalRejectsUnknownEntriesTheDecoderRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		entry tracepack.RawEntry
		want  error
	}{
		{"tag zero", tracepack.RawEntry{Tag: 0, Type: 7, Value: []byte{1}}, tlv.ErrZeroTag},
		{"u8 with two bytes", tracepack.RawEntry{Tag: 0x8001, Type: 1, Value: []byte{1, 2}}, tlv.ErrLength},
		{"uuid with one byte", tracepack.RawEntry{Tag: 0x8002, Type: 5, Value: []byte{1}}, tlv.ErrLength},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := basePackMeta()
			m.Unknown = []tracepack.RawEntry{tt.entry}
			_, err := m.MarshalBinary()
			require.ErrorIs(t, err, tt.want)

			ev := &tracepack.TransportEvent{Event: tracepack.EventClockStep, Unknown: []tracepack.RawEntry{tt.entry}}
			_, err = ev.MarshalBinary()
			require.ErrorIs(t, err, tt.want)

			text := "note"
			an := &tracepack.Annotation{AnnotationKind: tracepack.AnnotationKindNote, Text: &text, Unknown: []tracepack.RawEntry{tt.entry}}
			_, err = an.MarshalBinary()
			require.ErrorIs(t, err, tt.want)
		})
	}
}
