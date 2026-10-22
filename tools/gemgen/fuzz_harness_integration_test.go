//go:build integration

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// plantTestSupport copies the hand-written test support the generated tests rely on into the generated package dir,
// each file renamed to package pkg:
// the fuzz harness, gem/fuzz_test.go, and the result comparison, gem/assert_decoded_test.go.
//
// The init of every generated decode test file registers its decoders through the harness,
// and every generated round trip compares its result through assertDecoded,
// so a fixture package that compiles generated decode tests needs both.
// pkg is the fixture's internal package or its external _test package, whichever its generated tests use.
// Planting the real files rather than stubs also runs the fixture's decoders over the harness's malformed seeds,
// and runs the comparison's own test.
func plantTestSupport(t *testing.T, rootDir string, dir string, pkg string) {
	t.Helper()

	for _, name := range []string{"fuzz_test.go", "assert_decoded_test.go"} {
		data, err := os.ReadFile(filepath.Join(rootDir, "gem", name))
		require.NoError(t, err)

		src := strings.Replace(string(data), "package gem_test\n", "package "+pkg+"\n", 1)
		require.NotEqualf(t, string(data), src, "gem/%s did not declare package gem_test", name)

		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600))
	}
}
