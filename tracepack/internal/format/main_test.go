package format

import (
	"testing"
)

func TestMain(m *testing.M) {
	ApplyHeaderKernelsEnv()
	m.Run()
}
