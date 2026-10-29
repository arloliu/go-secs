//go:build amd64 && !purego

package format

// simdCapable reports whether this host runs the AVX-512 transpose kernels (avx512Usable), read once at start-up;
// simdHeaders, whether TransposeHeaders and UntransposeHeaders use them, which SetHeaderKernels may turn off.
var (
	simdCapable = avx512Usable(cpuid, xcr0)
	simdHeaders = simdCapable
)

// cpuid executes CPUID for leaf and sub-leaf sub.
//
//nolint:revive // CPUID writes four registers, each returned as it is, as Go's own internal/cpu does
func cpuid(leaf, sub uint32) (a, b, c, d uint32)

// xgetbv returns the low and high halves of XCR0; it may be executed only when CPUID reports OSXSAVE.
func xgetbv() (eax, edx uint32)

// xcr0 returns XCR0 (xgetbv).
func xcr0() uint64 {
	eax, edx := xgetbv()

	return uint64(edx)<<32 | uint64(eax)
}

// transposeKernel transposes the first colBlocks × 8 header bytes of the first tiles × 64 records of rows into section.
//
//go:noescape
func transposeKernel(section, rows *byte, count, headerLen, tiles, colBlocks int, idx *[64]int64, masks *[2][64]byte)

// untransposeKernel gathers the first colBlocks × 8 header bytes of the first tiles × 64 records of section into rows.
//
//go:noescape
func untransposeKernel(rows, section *byte, count, headerLen, tiles, colBlocks int, idx *[64]int64)

// deinterleave holds the VPSHUFB indexes that gather, within each 16-byte lane,
// the even then the odd bytes (0) and the even then the odd 16-bit words (1).
var deinterleave = func() (m [2][64]byte) {
	for lane := 0; lane < 64; lane += 16 {
		for i := range 8 {
			m[0][lane+i], m[0][lane+8+i] = byte(2*i), byte(2*i+1)
		}
		for i := range 4 {
			m[1][lane+2*i], m[1][lane+2*i+1] = byte(4*i), byte(4*i+1)
			m[1][lane+8+2*i], m[1][lane+8+2*i+1] = byte(4*i+2), byte(4*i+3)
		}
	}

	return m
}()

// checkTileLens panics unless rows and section both hold count × headerLen bytes:
// the kernels read and write by offset alone, without the bounds checks of Go code.
func checkTileLens(rows, section []byte, count, headerLen int) {
	if n := count * headerLen; len(rows) < n || len(section) < n {
		panic("format: header section shorter than count × headerLen")
	}
}

// transposeTiles64 transposes, with the kernel, the first 8 × ⌊headerLen/8⌋ bytes of each of the first ⌊count/64⌋ × 64 records,
// and returns the number of records it covered, which transposeFrom then completes; 0 when count < 64 or headerLen < 8.
func transposeTiles64(section, rows []byte, count, headerLen int) int {
	tiles := count / 64
	if tiles == 0 || headerLen < 8 {
		return 0
	}
	checkTileLens(rows, section, count, headerLen)
	idx := tileIndex(headerLen)
	transposeKernel(&section[0], &rows[0], count, headerLen, tiles, headerLen/8, &idx, &deinterleave)

	return tiles * 64
}

// untransposeTiles64 is transposeTiles64 in reverse.
func untransposeTiles64(rows, section []byte, count, headerLen int) int {
	tiles := count / 64
	if tiles == 0 || headerLen < 8 {
		return 0
	}
	checkTileLens(rows, section, count, headerLen)
	idx := tileIndex(headerLen)
	untransposeKernel(&rows[0], &section[0], count, headerLen, tiles, headerLen/8, &idx)

	return tiles * 64
}
