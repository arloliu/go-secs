package format

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMaxConstants(t *testing.T) {
	t.Parallel()

	// The tracepack format specification §2 sets these ceilings directly:
	// 2^63-1 for every u64 offset, length, count or seq, and 2^31-1 for body_len,
	// uncompressed_len and payload_len.
	require.Equal(t, uint64(9223372036854775807), MaxU64)
	require.Equal(t, uint32(2147483647), MaxLen32)
}

func TestCheckU64(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		v       uint64
		wantErr bool
	}{
		{name: "zero", v: 0, wantErr: false},
		{name: "at limit", v: MaxU64, wantErr: false},
		{name: "over limit", v: MaxU64 + 1, wantErr: true},
		{name: "max uint64", v: 1<<64 - 1, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := checkU64(tt.v, "seq")
			if tt.wantErr {
				require.ErrorIs(t, err, ErrLimit)
				require.Contains(t, err.Error(), "seq")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestCheckLen32(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		v       uint32
		wantErr bool
	}{
		{name: "zero", v: 0, wantErr: false},
		{name: "at limit", v: MaxLen32, wantErr: false},
		{name: "over limit", v: MaxLen32 + 1, wantErr: true},
		{name: "max uint32", v: 1<<32 - 1, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := checkLen32(tt.v, "body_len")
			if tt.wantErr {
				require.ErrorIs(t, err, ErrLimit)
				require.Contains(t, err.Error(), "body_len")
			} else {
				require.NoError(t, err)
			}
		})
	}
}
