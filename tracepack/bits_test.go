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
		{"bit3", tracepack.QualityDecodeFailed, []string{"decode-failed"}},
		{"bit4", tracepack.QualityDirectionInferred, []string{"direction-inferred"}},
		{"bit5", tracepack.QualityRedacted, []string{"redacted"}},
		{"bit6", tracepack.QualityNoMono, []string{"no-mono"}},
		{
			"ascending order regardless of set order",
			tracepack.QualityNoMono | tracepack.QualityCaptureBoundary,
			[]string{"capture-boundary", "no-mono"},
		},
		{
			"all known bits",
			tracepack.QualityCaptureBoundary | tracepack.QualityOrderingUncertain | tracepack.QualityCorrelationIncomplete |
				tracepack.QualityDecodeFailed | tracepack.QualityDirectionInferred | tracepack.QualityRedacted | tracepack.QualityNoMono,
			[]string{
				"capture-boundary", "ordering-uncertain", "correlation-incomplete",
				"decode-failed", "direction-inferred", "redacted", "no-mono",
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
			tracepack.QualityNoMono | tracepack.QualityCaptureBoundary | tracepack.QualityDecodeFailed,
			"capture-boundary|decode-failed|no-mono",
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

	q := tracepack.QualityRedacted | tracepack.QualityNoMono
	require.True(t, q.Has(tracepack.QualityRedacted))
	require.True(t, q.Has(tracepack.QualityNoMono))
	require.False(t, q.Has(tracepack.QualityDecodeFailed))
}

func TestFieldValidity_Names(t *testing.T) {
	t.Parallel()

	// Bit positions and names transcribed from the tracepack format specification §9, `field_validity` row.
	// Bit 1 covers both stream and W, because both are copied from HSMS header byte 2.
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
		{"bit0", tracepack.RecordFlagsW, []string{"W"}},
		{"bit1", tracepack.RecordFlagsMonoPresent, []string{"mono_present"}},
		{
			"ascending order",
			tracepack.RecordFlagsW | tracepack.RecordFlagsMonoPresent,
			[]string{"W", "mono_present"},
		},
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
		{"one bit", tracepack.RecordFlagsW, "W"},
		{"both bits", tracepack.RecordFlagsW | tracepack.RecordFlagsMonoPresent, "W|mono_present"},
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
	require.False(t, f.Has(tracepack.RecordFlagsW))
}
