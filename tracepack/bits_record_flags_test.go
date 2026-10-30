package tracepack

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRecordFlagSet_Names(t *testing.T) {
	t.Parallel()

	// Bit positions and names transcribed from the tracepack format specification §9, `record_flags` row.
	tests := []struct {
		name  string
		value recordFlagSet
		want  []string
	}{
		{"zero", 0, nil},
		{"bit0 retired", 1 << 0, nil},
		{"bit1", recordFlagMonoPresent, []string{"mono_present"}},
		{"retired bit beside mono_present", 1<<0 | recordFlagMonoPresent, []string{"mono_present"}},
		{"reserved bit only", 1 << 2, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, tt.value.Names())
		})
	}
}

func TestRecordFlagSet_String(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value recordFlagSet
		want  string
	}{
		{"zero", 0, ""},
		{"mono_present", recordFlagMonoPresent, "mono_present"},
		{"retired bit only", 1 << 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, tt.value.String())
		})
	}
}

func TestRecordFlagSet_Has(t *testing.T) {
	t.Parallel()

	f := recordFlagMonoPresent
	require.True(t, f.Has(recordFlagMonoPresent))
	require.False(t, recordFlagSet(1<<0).Has(recordFlagMonoPresent))
}
