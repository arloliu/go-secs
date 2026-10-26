package format

import (
	"math/rand/v2"
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

// refTransposeHeaders and refUntransposeHeaders place every byte by the rule of §6 one at a time,
// the reference the tiled functions are compared with.
func refTransposeHeaders(rows []byte, count, headerLen int) []byte {
	section := make([]byte, len(rows))
	for i := range count {
		for j := range headerLen {
			section[j*count+i] = rows[i*headerLen+j]
		}
	}

	return section
}

func refUntransposeHeaders(section []byte, count, headerLen int) []byte {
	rows := make([]byte, len(section))
	for i := range count {
		for j := range headerLen {
			rows[i*headerLen+j] = section[j*count+i]
		}
	}

	return rows
}

// checkTransposeHeaders compares both functions with the references on the rows seed fills, appended after a prefix byte.
func checkTransposeHeaders(t *testing.T, count, headerLen int, seed uint64) {
	t.Helper()

	rng := rand.New(rand.NewPCG(seed, uint64(count)<<16|uint64(headerLen)))
	rows := make([]byte, count*headerLen)
	for k := range rows {
		rows[k] = byte(rng.Uint32())
	}
	want := refTransposeHeaders(rows, count, headerLen)
	section := TransposeHeaders([]byte{0xAA}, rows, count, headerLen)
	require.Equal(t, byte(0xAA), section[0])
	require.Equal(t, want, section[1:], "transpose: count %d, header length %d", count, headerLen)

	got := UntransposeHeaders([]byte{0xBB}, want, count, headerLen)
	require.Equal(t, byte(0xBB), got[0])
	require.Equal(t, refUntransposeHeaders(want, count, headerLen), got[1:], "untranspose: count %d, header length %d", count, headerLen)
	require.Equal(t, rows, got[1:])
}

// TestTransposeHeadersMatchesReference sweeps counts and header lengths around the 8-record, 8-byte tiles.
func TestTransposeHeadersMatchesReference(t *testing.T) {
	t.Parallel()

	counts := make([]int, 0, 26)
	for c := range 21 {
		counts = append(counts, c)
	}
	counts = append(counts, 63, 64, 65, 1000, 12345)
	for _, count := range counts {
		for _, headerLen := range []int{1, 4, 7, 8, 9, 15, 16, 17, 44, 45, 52, 64, 300} {
			checkTransposeHeaders(t, count, headerLen, 1)
		}
	}
}

func FuzzTransposeHeaders(f *testing.F) {
	f.Add(uint16(9), uint16(44), uint64(1))
	f.Add(uint16(64), uint16(45), uint64(2))
	f.Add(uint16(7), uint16(300), uint64(3))
	f.Fuzz(func(t *testing.T, count, headerLen uint16, seed uint64) {
		// Bound the section to 1 MiB, enough for every tile shape.
		c, hl := int(count%4096), int(headerLen%512)+1
		if c*hl > 1<<20 {
			t.Skip()
		}
		checkTransposeHeaders(t, c, hl, seed)
	})
}

func BenchmarkUntransposeHeaders(b *testing.B) {
	const count = 12404
	rows := make([]byte, count*RecordHeaderLen)
	for k := range rows {
		rows[k] = byte(k * 7)
	}
	section := TransposeHeaders(nil, rows, count, RecordHeaderLen)
	dst := make([]byte, 0, len(section))
	b.SetBytes(int64(len(section)))
	for b.Loop() {
		dst = UntransposeHeaders(dst[:0], section, count, RecordHeaderLen)
	}
}

func BenchmarkTransposeHeaders(b *testing.B) {
	const count = 12404
	rows := make([]byte, count*RecordHeaderLen)
	for k := range rows {
		rows[k] = byte(k * 7)
	}
	dst := make([]byte, 0, len(rows))
	b.SetBytes(int64(len(rows)))
	for b.Loop() {
		dst = TransposeHeaders(dst[:0], rows, count, RecordHeaderLen)
	}
}
