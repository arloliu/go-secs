package format

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// cpuState is a host's CPUID leaves and XCR0, as avx512Usable reads them.
type cpuState struct {
	maxLeaf  uint32
	ecx1     uint32
	ebx7     uint32
	xcr0     uint64
	xgetbvOK bool // whether xgetbv may be called: OSXSAVE is set
}

// capable is a host avx512Usable accepts.
var capable = cpuState{
	maxLeaf: 7, ecx1: cpuidOSXSAVE, ebx7: cpuidAVX512F | cpuidAVX512BW, xcr0: xcr0AVX512Regs, xgetbvOK: true,
}

// usable runs avx512Usable on s, failing the test if XGETBV is executed without OSXSAVE.
func usable(t *testing.T, s cpuState) bool {
	t.Helper()

	cpuid := func(leaf, _ uint32) (a, b, c, d uint32) {
		switch {
		case leaf == 0:
			return s.maxLeaf, 0, 0, 0
		case leaf > s.maxLeaf:
			return 0, 0, 0, 0
		case leaf == 1:
			return 0, 0, s.ecx1, 0
		case leaf == 7:
			return 0, s.ebx7, 0, 0
		}

		return 0, 0, 0, 0
	}
	xgetbv := func() uint64 {
		if s.ecx1&cpuidOSXSAVE == 0 {
			t.Fatal("XGETBV executed without OSXSAVE")
		}

		return s.xcr0
	}

	return avx512Usable(cpuid, xgetbv)
}

// avx512Usable accepts a host only when every condition holds, each tested alone,
// and executes XGETBV only once CPUID reports OSXSAVE.
func TestAVX512Usable(t *testing.T) {
	t.Parallel()

	assert.True(t, usable(t, capable))

	s := capable
	s.maxLeaf = 6
	assert.False(t, usable(t, s), "no leaf 7")

	s = capable
	s.ecx1 = 0
	assert.False(t, usable(t, s), "no OSXSAVE")

	for _, bit := range []uint{1, 2, 5, 6, 7} {
		s = capable
		s.xcr0 &^= 1 << bit
		assert.False(t, usable(t, s), "XCR0 bit %d clear", bit)
	}
	for name, bit := range map[string]uint32{"AVX512F": cpuidAVX512F, "AVX512BW": cpuidAVX512BW} {
		s = capable
		s.ebx7 &^= bit
		assert.False(t, usable(t, s), "no %s", name)
	}
	s = capable
	s.xcr0 |= 1<<9 | 1<<0 // other state bits change nothing
	assert.True(t, usable(t, s))
}

// tileIndex's offsets are those of the records the interleave network leaves in each vector element.
func TestTileIndex(t *testing.T) {
	t.Parallel()

	idx := tileIndex(44)
	seen := map[int64]bool{}
	for k := range 8 {
		for e := range 8 {
			off := idx[k*8+e]
			assert.Zero(t, off%44)
			seen[off/44] = true
		}
	}
	assert.Len(t, seen, 64, "every record of the tile once")
	assert.Equal(t, int64(0), idx[0])
	assert.Equal(t, int64(1*44), idx[1])
	assert.Equal(t, int64(16*44), idx[2])
	assert.Equal(t, int64(2*44), idx[8])
}
