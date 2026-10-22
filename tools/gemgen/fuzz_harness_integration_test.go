//go:build integration

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// gemQualified matches a gem-qualified exported identifier, such as gem.DecodeOption,
// capturing the first letter of its name,
// so dropping the qualifier leaves any other "gem." text untouched, such as a path like gem/fuzz_test.go.
var gemQualified = regexp.MustCompile(`\bgem\.([A-Z])`)

// plantTestSupport copies the hand-written test support the generated tests rely on into the generated package dir,
// each file renamed to package pkg:
// the fuzz harness, gem/fuzz_test.go, and the result comparison, gem/assert_decoded_test.go.
// What a file reads from gem, such as gem.DecodeOption, is pointed at the fixture's own generated package:
// an internal test package reads it unqualified,
// and an external one imports the fixture's directory under the gem name.
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

		const gemImport = "\t\"github.com/arloliu/go-secs/v2/gem\"\n"
		if strings.HasSuffix(pkg, "_test") {
			src = strings.ReplaceAll(src, gemImport, "\tgem \"github.com/arloliu/go-secs/v2/"+filepath.Base(dir)+"\"\n")
		} else {
			src = gemQualified.ReplaceAllString(strings.ReplaceAll(src, gemImport, ""), "$1")
		}

		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600))
	}
}
