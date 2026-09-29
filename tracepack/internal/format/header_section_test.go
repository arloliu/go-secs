package format

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestTransposeHeadersGolden checks the byte placement of the tracepack format specification §6
// on two 4-byte headers: byte j of record i at offset j × count + i.
func TestTransposeHeadersGolden(t *testing.T) {
	t.Parallel()

	rows := []byte{
		0x00, 0x01, 0x02, 0x03, // record 0
		0x10, 0x11, 0x12, 0x13, // record 1
	}
	want := []byte{0x00, 0x10, 0x01, 0x11, 0x02, 0x12, 0x03, 0x13}

	require.Equal(t, want, TransposeHeaders(nil, rows, 2, 4))
	require.Equal(t, rows, UntransposeHeaders(nil, want, 2, 4))
}

func TestTransposeHeadersRoundTrip(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct{ count, headerLen int }{
		{0, 44}, {1, 44}, {2, 44}, {3, 45}, {7, 52}, {100, 44}, {5, 1},
	} {
		rows := make([]byte, tt.count*tt.headerLen)
		for k := range rows {
			rows[k] = byte(k*7 + k/tt.headerLen)
		}

		section := TransposeHeaders([]byte{0xAA}, rows, tt.count, tt.headerLen)
		require.Equal(t, byte(0xAA), section[0], "the section is appended after dst")
		section = section[1:]
		require.Len(t, section, len(rows))
		if tt.count == 1 {
			require.Equal(t, rows, section, "a block of one record is laid out as its header's bytes in order")
		}
		for i := range tt.count {
			for j := range tt.headerLen {
				require.Equal(t, rows[i*tt.headerLen+j], section[j*tt.count+i], "count %d, record %d byte %d", tt.count, i, j)
			}
		}

		got := UntransposeHeaders(make([]byte, 0, 1), section, tt.count, tt.headerLen)
		require.Equal(t, rows, got, "count %d, header length %d", tt.count, tt.headerLen)
	}
}

// TestUntransposeHeadersReusesDst checks that a buffer with room for the rows is reused, so a reader allocates it once.
func TestUntransposeHeadersReusesDst(t *testing.T) {
	section := make([]byte, 64*44)
	dst := make([]byte, 0, len(section))

	allocs := testing.AllocsPerRun(100, func() {
		dst = UntransposeHeaders(dst[:0], section, 64, 44)
	})
	require.Zero(t, allocs)
}
