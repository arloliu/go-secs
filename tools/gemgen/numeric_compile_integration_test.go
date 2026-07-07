//go:build integration

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNumericLeavesCompileAndPassAgainstRealSecs2 is a permanent regression
// guard for the plain numeric fixed-leaf samples (the RSDC/S6F23 gap). It
// renders a message whose body carries one leaf of every numeric goType --
// int8..int64, uint8..uint64, float32, float64 -- then REAL `go build`s and
// `go test`s the generated builder plus its generated test against the real
// secs2 package, not a syntax-only check.
//
// The two type-correctness properties only a real build/test can prove are:
//
//   - the untyped-integer / half-integer sample literal actually converts to
//     each narrow parameter width (e.g. an int8 parameter) at the call site
//     without overflowing, and to the wide int64/uint64/float64 comparison
//     type at the assertion site, and
//   - the float32 leaf's encode -> decode (ToFloat, []float64) round-trip
//     compares equal, i.e. the half-integer sample is exactly representable.
//
// It is gated behind the `integration` build tag because it shells out to the
// go toolchain and reaches across module boundaries; run it with
// `go test -tags integration ./...`.
func TestNumericLeavesCompileAndPassAgainstRealSecs2(t *testing.T) {
	numeric := []struct {
		item, format, goType string
	}{
		{"NI1", "I1", "int8"}, {"NI2", "I2", "int16"}, {"NI4", "I4", "int32"}, {"NI8", "I8", "int64"},
		{"NU1", "U1", "uint8"}, {"NU2", "U2", "uint16"}, {"NU4", "U4", "uint32"}, {"NU8", "U8", "uint64"},
		{"NF4", "F4", "float32"}, {"NF8", "F8", "float64"},
	}

	items := map[string]Item{}
	structItems := make([]StructureNode, 0, len(numeric))
	for _, n := range numeric {
		items[n.item] = Item{Formats: []string{n.format}, Binding: BindingFixed, GoType: n.goType}
		structItems = append(structItems, StructureNode{Item: n.item})
	}

	mf := MessageFile{Stream: 2, Messages: []Message{{
		Function: 41, Name: "Numeric Sample", Mnemonic: "NS",
		Direction: "host-to-equipment", Description: "Carries one leaf of every numeric goType.",
		Exception: "None", Source: "e5",
		Bodies: []Body{{
			Actor: "host", ReplyExpected: true,
			Structure: &StructureNode{Type: "list", Items: structItems},
		}},
	}}}

	msgOut, err := renderMessages(mf, items)
	require.NoError(t, err)
	testOut, err := renderTests(mf, items)
	require.NoError(t, err)

	// Sanity-check the three unified accessors are actually emitted before
	// spending a build on the output.
	require.Contains(t, string(testOut), ".ToInt()")
	require.Contains(t, string(testOut), ".ToUint()")
	require.Contains(t, string(testOut), ".ToFloat()")

	// Locate the root go-secs module (two levels up from tools/gemgen), which
	// provides the real secs2/gem packages the generated code imports.
	wd, err := os.Getwd()
	require.NoError(t, err)
	rootDir := filepath.Clean(filepath.Join(wd, "..", ".."))
	rootMod, err := os.ReadFile(filepath.Join(rootDir, "go.mod"))
	require.NoError(t, err, "expected root go.mod at %s", rootDir)
	require.True(t, strings.Contains(string(rootMod), "module github.com/arloliu/go-secs/v2"),
		"root module at %s is not the go-secs root module", rootDir)

	// Create a throwaway package directory inside the root module so `go test`
	// resolves the real secs2 import. Rewrite the builder's `package gem` to a
	// unique name and the test's `package gem_test` to match, so the generated
	// test can call the builder directly without importing the real gem package.
	checkDir, err := os.MkdirTemp(rootDir, "gemnumericcheck-")
	require.NoError(t, err)
	defer os.RemoveAll(checkDir)

	pkg := filepath.Base(checkDir)
	pkg = strings.ReplaceAll(pkg, "-", "")

	msgSrc := strings.Replace(string(msgOut), "package gem\n", "package "+pkg+"\n", 1)
	require.NotEqual(t, string(msgOut), msgSrc, "generated builder did not declare 'package gem'")

	// The generated test lives in gem_test and prefixes calls with gem.; rewrite
	// both to the throwaway package so it becomes an internal test of the builder.
	testSrc := strings.Replace(string(testOut), "package gem_test\n", "package "+pkg+"\n", 1)
	require.NotEqual(t, string(testOut), testSrc, "generated test did not declare 'package gem_test'")
	testSrc = strings.ReplaceAll(testSrc, "gem.S2F41", "S2F41")
	testSrc = strings.ReplaceAll(testSrc, "\t\"github.com/arloliu/go-secs/v2/gem\"\n", "")

	require.NoError(t, os.WriteFile(filepath.Join(checkDir, "s2.go"), []byte(msgSrc), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(checkDir, "s2_test.go"), []byte(testSrc), 0o644))

	// Real build + test against real secs2 -- this is the whole point: catch
	// width-overflow / accessor-name / float-representability errors that a
	// syntax-only pass misses, and prove the generated assertions actually pass.
	target := "./" + filepath.Base(checkDir) + "/"
	cmd := exec.Command("go", "test", target)
	cmd.Dir = rootDir
	buildOut, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "generated numeric-leaf message/test failed against real secs2:\n%s\n--- builder ---\n%s\n--- test ---\n%s",
		string(buildOut), msgSrc, testSrc)
}
