package tracepack_test

import (
	"testing"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/stretchr/testify/require"
)

func TestQuality_Names(t *testing.T) {
	t.Parallel()

	// Bit positions and names transcribed from the tracepack format specification §9, `quality` row.
	tests := []struct {
		name  string
		value tracepack.Quality
		want  []string
	}{
		{"zero", 0, nil},
		{"bit0", tracepack.QualityCaptureBoundary, []string{"capture-boundary"}},
		{"bit1", tracepack.QualityOrderingUncertain, []string{"ordering-uncertain"}},
		{"bit2", tracepack.QualityCorrelationIncomplete, []string{"correlation-incomplete"}},
		{"bit3 retired", 1 << 3, nil},
		{"bit4", tracepack.QualityDirectionInferred, []string{"direction-inferred"}},
		{"bit5", tracepack.QualityRedacted, []string{"redacted"}},
		{"bit6 retired", 1 << 6, nil},
		{
			"ascending order regardless of set order",
			tracepack.QualityRedacted | tracepack.QualityCaptureBoundary,
			[]string{"capture-boundary", "redacted"},
		},
		{
			"all known bits",
			tracepack.QualityCaptureBoundary | tracepack.QualityOrderingUncertain | tracepack.QualityCorrelationIncomplete |
				tracepack.QualityDirectionInferred | tracepack.QualityRedacted,
			[]string{
				"capture-boundary", "ordering-uncertain", "correlation-incomplete",
				"direction-inferred", "redacted",
			},
		},
		{"reserved bit only", 1 << 7, nil},
		{"known bit plus reserved bit", tracepack.QualityRedacted | 1<<7, []string{"redacted"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, tt.value.Names())
		})
	}
}

func TestQuality_String(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value tracepack.Quality
		want  string
	}{
		{"zero", 0, ""},
		{"one bit", tracepack.QualityRedacted, "redacted"},
		{
			"multiple bits joined ascending",
			tracepack.QualityRedacted | tracepack.QualityCaptureBoundary | 1<<3 | 1<<6,
			"capture-boundary|redacted",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, tt.value.String())
		})
	}
}

func TestQuality_Has(t *testing.T) {
	t.Parallel()

	q := tracepack.QualityRedacted | tracepack.QualityCaptureBoundary
	require.True(t, q.Has(tracepack.QualityRedacted))
	require.True(t, q.Has(tracepack.QualityCaptureBoundary))
	require.False(t, q.Has(tracepack.QualityOrderingUncertain))
}

func TestFieldValidity_Names(t *testing.T) {
	t.Parallel()

	// Bit positions and names transcribed from the tracepack format specification §9, `field_validity` row.
	// Bit 1 covers both stream and W, because both live in payload byte 6.
	tests := []struct {
		name  string
		value tracepack.FieldValidity
		want  []string
	}{
		{"zero", 0, nil},
		{"bit0", tracepack.FieldValiditySessionID, []string{"session_id"}},
		{"bit1", tracepack.FieldValidityStreamAndW, []string{"stream_and_w"}},
		{"bit2", tracepack.FieldValidityFunction, []string{"function"}},
		{"bit3", tracepack.FieldValidityPType, []string{"ptype"}},
		{"bit4", tracepack.FieldValiditySType, []string{"stype"}},
		{"bit5", tracepack.FieldValiditySystemBytes, []string{"system_bytes"}},
		{
			"ascending order regardless of set order",
			tracepack.FieldValiditySystemBytes | tracepack.FieldValiditySessionID,
			[]string{"session_id", "system_bytes"},
		},
		{"reserved bit only", 1 << 6, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, tt.value.Names())
		})
	}
}

func TestFieldValidity_String(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value tracepack.FieldValidity
		want  string
	}{
		{"zero", 0, ""},
		{"one bit", tracepack.FieldValidityPType, "ptype"},
		{
			"multiple bits joined ascending",
			tracepack.FieldValiditySystemBytes | tracepack.FieldValiditySessionID | tracepack.FieldValidityFunction,
			"session_id|function|system_bytes",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, tt.value.String())
		})
	}
}

func TestFieldValidity_Has(t *testing.T) {
	t.Parallel()

	v := tracepack.FieldValiditySessionID | tracepack.FieldValidityPType
	require.True(t, v.Has(tracepack.FieldValiditySessionID))
	require.True(t, v.Has(tracepack.FieldValidityPType))
	require.False(t, v.Has(tracepack.FieldValidityFunction))
}

func TestRecordFlags_Names(t *testing.T) {
	t.Parallel()

	// Bit positions and names transcribed from the tracepack format specification §9, `record_flags` row.
	tests := []struct {
		name  string
		value tracepack.RecordFlags
		want  []string
	}{
		{"zero", 0, nil},
		{"bit0 retired", 1 << 0, nil},
		{"bit1", tracepack.RecordFlagsMonoPresent, []string{"mono_present"}},
		{"retired bit beside mono_present", 1<<0 | tracepack.RecordFlagsMonoPresent, []string{"mono_present"}},
		{"reserved bit only", 1 << 2, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, tt.value.Names())
		})
	}
}

func TestRecordFlags_String(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value tracepack.RecordFlags
		want  string
	}{
		{"zero", 0, ""},
		{"mono_present", tracepack.RecordFlagsMonoPresent, "mono_present"},
		{"retired bit only", 1 << 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, tt.value.String())
		})
	}
}

func TestRecordFlags_Has(t *testing.T) {
	t.Parallel()

	f := tracepack.RecordFlagsMonoPresent
	require.True(t, f.Has(tracepack.RecordFlagsMonoPresent))
	require.False(t, tracepack.RecordFlags(1<<0).Has(tracepack.RecordFlagsMonoPresent))
}
