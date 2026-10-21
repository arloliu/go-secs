//go:build integration

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// plantFuzzHarness copies the hand-written fuzz harness, gem/fuzz_test.go, into the generated package dir,
// renamed to package pkg.
//
// The init of every generated decode test file registers its decoders through that harness,
// so a fixture package that compiles generated decode tests needs it too.
// Planting the real file rather than a stub also runs the fixture's decoders over the harness's malformed seeds.
func plantFuzzHarness(t *testing.T, rootDir string, dir string, pkg string) {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(rootDir, "gem", "fuzz_test.go"))
	require.NoError(t, err)

	src := strings.Replace(string(data), "package gem_test\n", "package "+pkg+"\n", 1)
	require.NotEqual(t, string(data), src, "gem/fuzz_test.go did not declare package gem_test")

	require.NoError(t, os.WriteFile(filepath.Join(dir, "fuzz_test.go"), []byte(src), 0o600))
}
