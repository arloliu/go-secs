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

// TestPackedGroupsCompileAgainstRealSecs2 is a permanent regression guard for
// the packed multi-value item (S1F10) body expression. Earlier fixes verified
// only the generated *source text* or its syntactic validity (gofmt / go/parser)
// and so twice missed genuine compile errors -- most recently spreading a
// []byte-typed parameter into secs2.B's `values ...any` signature
// (secs2.B(tsips...)), which does not type-check because []byte is not
// assignable to []any.
//
// This test renders the S1F10 message and performs a REAL `go build` of the
// output against the real secs2 package (resolved from the root go-secs module),
// not a syntax-only check. It is gated behind the `integration` build tag
// because it shells out to the go toolchain and reaches across module
// boundaries; run it with `go test -tags integration ./...`.
func TestPackedGroupsCompileAgainstRealSecs2(t *testing.T) {
	// Render S1F10: two sibling packed groups over the fixed byte items TSIP/TSOP.
	items := paramsItems()
	mf := MessageFile{Stream: 1, Messages: []Message{{
		Function: 10, Name: "Port Transfer Status Data", Mnemonic: "PTSD",
		Direction: "equipment-to-host", Description: "Equipment reports port transfer status.",
		Exception: "None", Source: "e5",
		Bodies: []Body{{
			Actor: "both", ReplyExpected: false,
			Structure: &StructureNode{Type: "list", Items: []StructureNode{
				{Type: "list", Packed: "tsips", Of: &StructureNode{Item: "TSIP"}},
				{Type: "list", Packed: "tsops", Of: &StructureNode{Item: "TSOP"}},
			}},
		}},
	}}}
	out, err := renderMessages(mf, items)
	require.NoError(t, err)

	// Locate the root go-secs module (two levels up from tools/gemgen), which
	// provides the real secs2 package the generated code imports.
	wd, err := os.Getwd()
	require.NoError(t, err)
	rootDir := filepath.Clean(filepath.Join(wd, "..", ".."))
	rootMod, err := os.ReadFile(filepath.Join(rootDir, "go.mod"))
	require.NoError(t, err, "expected root go.mod at %s", rootDir)
	require.True(t, strings.Contains(string(rootMod), "module github.com/arloliu/go-secs/v2"),
		"root module at %s is not the go-secs root module", rootDir)

	// Create a throwaway package directory inside the root module so `go build`
	// resolves the real secs2 import, then rename the package clause to avoid
	// colliding with the real gem package.
	checkDir, err := os.MkdirTemp(rootDir, "gemcompilecheck-")
	require.NoError(t, err)
	defer os.RemoveAll(checkDir)

	src := strings.Replace(string(out), "package gem\n", "package gemcompilecheck\n", 1)
	require.NotEqual(t, string(out), src, "generated source did not declare 'package gem'")
	require.NoError(t, os.WriteFile(filepath.Join(checkDir, "s1.go"), []byte(src), 0o644))

	// Real compile against real secs2 -- this is the whole point: catch type
	// errors (like spreading []byte into ...any) that syntax-only checks miss.
	target := "./" + filepath.Base(checkDir) + "/"
	cmd := exec.Command("go", "build", target)
	cmd.Dir = rootDir
	buildOut, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "generated S1F10 failed to compile against real secs2:\n%s\n--- source ---\n%s",
		string(buildOut), src)
}
