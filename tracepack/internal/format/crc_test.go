package format

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Expected values are the tracepack format specification's own check value (§2) and independent computations with Python's zlib.crc32.
// They are never derived from CRC itself.
func TestCRC(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   []byte
		want uint32
	}{
		{name: "spec check value", in: []byte("123456789"), want: 0xCBF43926},
		{name: "empty input", in: []byte{}, want: 0x00000000},
		{name: "nil input", in: nil, want: 0x00000000},
		{name: "arbitrary text", in: []byte("hello tracepack"), want: 0x60A90F56},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tt.want, CRC(tt.in))
		})
	}
}
