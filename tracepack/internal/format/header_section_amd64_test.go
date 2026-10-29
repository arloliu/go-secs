//go:build amd64 && !purego

package format

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// On a host whose CPU and OS offer AVX-512 F and BW, as Linux reports them, the kernels are selected:
// a detection that wrongly refused them would leave the tests of the kernels skipped.
func TestSIMDHeadersOnCapableHost(t *testing.T) {
	t.Parallel()

	info, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		t.Skip("no /proc/cpuinfo")
	}
	var flags []string
	for line := range strings.SplitSeq(string(info), "\n") {
		if strings.HasPrefix(line, "flags") {
			flags = strings.Fields(line)

			break
		}
	}
	has := func(f string) bool {
		return slices.Contains(flags, f)
	}
	if !has("avx512f") || !has("avx512bw") {
		require.False(t, simdHeaders, "the kernels are selected on a host without AVX-512 F and BW")
		t.Skip("this host has no AVX-512 F and BW")
	}
	require.True(t, simdHeaders, "a host with AVX-512 F and BW selects the kernels")
}
