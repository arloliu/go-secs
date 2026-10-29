package tracepack

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// TestMain runs the package's tests on the pure-Go transpose when format.HeaderKernelsEnv is "off".
func TestMain(m *testing.M) {
	format.ApplyHeaderKernelsEnv()
	m.Run()
}

// With format.HeaderKernelsEnv "off", the package's tests run without the AVX-512 kernels.
func TestHeaderKernelsEnvApplied(t *testing.T) {
	t.Parallel()

	if os.Getenv(format.HeaderKernelsEnv) == "off" {
		require.False(t, format.HeaderKernels())
	}
}
