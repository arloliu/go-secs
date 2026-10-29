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

// TestUntransposeHeadersReusesDst checks that a buffer with room for the rows is reused, so a reader allocates it once,
// on every path of the host, in both directions.
func TestUntransposeHeadersReusesDst(t *testing.T) {
	section := make([]byte, 200*44)
	dst := make([]byte, 0, len(section))

	for _, simd := range headerPaths() {
		allocs := testing.AllocsPerRun(100, func() {
			dst = untransposeWith(dst[:0], section, 200, 44, simd)
		})
		require.Zero(t, allocs, "untranspose, simd %v", simd)
		allocs = testing.AllocsPerRun(100, func() {
			dst = transposeWith(dst[:0], section, 200, 44, simd)
		})
		require.Zero(t, allocs, "transpose, simd %v", simd)
	}
}

// The transposes on every path read only the count × headerLen bytes of their input and write only that many after dst,
// wherever the buffers start: the bytes around them, canaries, stay as they were.
func TestTransposeHeadersStayInBounds(t *testing.T) {
	t.Parallel()

	for _, simd := range headerPaths() {
		for _, shape := range []struct{ count, headerLen int }{{64, 44}, {200, 44}, {129, 52}, {1489, 44}, {300, 100}} {
			n := shape.count * shape.headerLen
			for off := range 8 {
				in := make([]byte, n+16)
				for k := range in {
					in[k] = byte(k*13 + 1)
				}
				src := in[off : off+n]
				out := make([]byte, n+32)
				for k := range out {
					out[k] = 0xC5
				}
				dst := out[off : off+3 : off+3+n+8]
				want := refTransposeHeaders(src, shape.count, shape.headerLen)

				got := transposeWith(dst, src, shape.count, shape.headerLen, simd)
				require.Equal(t, want, got[3:], "transpose %v, offset %d, simd %v", shape, off, simd)
				for k := off + 3 + n; k < len(out); k++ {
					require.Equal(t, byte(0xC5), out[k], "transpose wrote byte %d past its section, simd %v", k-off-3-n, simd)
				}

				back := untransposeWith(out[off:off+3:off+3+n+8], want, shape.count, shape.headerLen, simd)
				require.Equal(t, src, back[3:], "untranspose %v, offset %d, simd %v", shape, off, simd)
				for k := off + 3 + n; k < len(out); k++ {
					require.Equal(t, byte(0xC5), out[k], "untranspose wrote byte %d past its rows, simd %v", k-off-3-n, simd)
				}
			}
		}
	}
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

// headerPaths are the paths a transpose may take on this host: the pure-Go code, and the AVX-512 kernel where simdHeaders is set.
// No test selects the kernel on a host without it, whose instructions would fault.
func headerPaths() []bool {
	if simdHeaders {
		return []bool{false, true}
	}

	return []bool{false}
}

// checkTransposeHeaders compares both functions, on every path of the host, with the references
// on the rows seed fills, appended after a prefix byte.
func checkTransposeHeaders(t *testing.T, count, headerLen int, seed uint64) {
	t.Helper()

	rng := rand.New(rand.NewPCG(seed, uint64(count)<<16|uint64(headerLen)))
	rows := make([]byte, count*headerLen)
	for k := range rows {
		rows[k] = byte(rng.Uint32())
	}
	want := refTransposeHeaders(rows, count, headerLen)
	for _, simd := range headerPaths() {
		section := transposeWith([]byte{0xAA}, rows, count, headerLen, simd)
		require.Equal(t, byte(0xAA), section[0])
		require.Equal(t, want, section[1:], "transpose: count %d, header length %d, simd %v", count, headerLen, simd)

		got := untransposeWith([]byte{0xBB}, want, count, headerLen, simd)
		require.Equal(t, byte(0xBB), got[0])
		require.Equal(t, refUntransposeHeaders(want, count, headerLen), got[1:],
			"untranspose: count %d, header length %d, simd %v", count, headerLen, simd)
		require.Equal(t, rows, got[1:])
	}
}

// TestTransposeHeadersMatchesReference sweeps counts and header lengths around the 8-record, 8-byte tiles.
func TestTransposeHeadersMatchesReference(t *testing.T) {
	t.Parallel()

	counts := make([]int, 0, 26)
	for c := range 21 {
		counts = append(counts, c)
	}
	counts = append(counts, 63, 64, 65, 127, 128, 129, 191, 200, 1000, 1489, 12345, 12644)
	for _, count := range counts {
		for _, headerLen := range []int{1, 4, 7, 8, 9, 15, 16, 17, 44, 45, 48, 52, 60, 64, 100, 300} {
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
