package corpus

import (
	"testing"

	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// TestMain runs the package's tests on the pure-Go transpose when format.HeaderKernelsEnv is "off".
func TestMain(m *testing.M) {
	format.ApplyHeaderKernelsEnv()
	m.Run()
}
