package format

import "os"

// HeaderKernelsEnv is the environment variable a test binary reads through ApplyHeaderKernelsEnv:
// the value "off" runs every transpose of the binary on the pure-Go path.
const HeaderKernelsEnv = "TRACEPACK_HEADER_KERNELS"

// SetHeaderKernels turns the AVX-512 kernels of TransposeHeaders and UntransposeHeaders off, or back on where the host runs them;
// it never turns them on where the host does not.
// Tests call it before any transpose runs, so that a whole package's tests take the pure-Go path on a host with AVX-512;
// it is not safe to call while a transpose runs.
func SetHeaderKernels(on bool) {
	simdHeaders = on && simdCapable
}

// HeaderKernels reports whether TransposeHeaders and UntransposeHeaders use the AVX-512 kernels.
func HeaderKernels() bool {
	return simdHeaders
}

// ApplyHeaderKernelsEnv turns the kernels off when HeaderKernelsEnv is "off"; a test binary's TestMain calls it.
func ApplyHeaderKernelsEnv() {
	if os.Getenv(HeaderKernelsEnv) == "off" {
		SetHeaderKernels(false)
	}
}

// The CPUID and XCR0 bits avx512Usable tests, as the Intel SDM and Go's own internal/cpu name them.
const (
	cpuidOSXSAVE   = 1 << 27 // leaf 1, ECX: the OS enabled XGETBV
	cpuidAVX512F   = 1 << 16 // leaf 7, EBX
	cpuidAVX512BW  = 1 << 30 // leaf 7, EBX
	xcr0AVX512Regs = 1<<1 | 1<<2 | 1<<5 | 1<<6 | 1<<7
	// xcr0AVX512Regs: the OS saves the XMM (1), YMM (2), opmask (5), ZMM_Hi256 (6) and Hi16_ZMM (7) state.
)

// avx512Usable reports whether a host runs the AVX-512 transpose kernels, which need AVX-512 F and BW,
// from the results of cpuid and xgetbv, passed in so a test can drive every case:
// CPUID leaf 7 exists, the OS enabled XGETBV (leaf 1 OSXSAVE), which only then is executed,
// the OS saves the XMM, YMM, opmask and ZMM state (XCR0), and leaf 7 reports AVX512F and AVX512BW.
func avx512Usable(cpuid func(leaf, sub uint32) (a, b, c, d uint32), xgetbv func() uint64) bool {
	if maxLeaf, _, _, _ := cpuid(0, 0); maxLeaf < 7 {
		return false
	}
	if _, _, ecx, _ := cpuid(1, 0); ecx&cpuidOSXSAVE == 0 {
		return false
	}
	if xgetbv()&xcr0AVX512Regs != xcr0AVX512Regs {
		return false
	}
	_, ebx, _, _ := cpuid(7, 0)

	return ebx&cpuidAVX512F != 0 && ebx&cpuidAVX512BW != 0
}

// tileIndex returns the byte offsets, within a tile of 64 records of headerLen bytes, of the 8-byte pieces the kernels move:
// element e of vector k is record (e/2)·16 + 2k + e%2, the order the kernels' interleave network leaves the records in.
func tileIndex(headerLen int) [64]int64 {
	var idx [64]int64
	for k := range 8 {
		for e := range 8 {
			idx[k*8+e] = int64(((e>>1)*16 + (e & 1) + 2*k) * headerLen)
		}
	}

	return idx
}
