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

// TestByteArrayLeavesCompileAgainstRealSecs2 is a permanent regression guard
// for the byte-array/byte-slice leaf body expressions (constraint 14). It
// exercises the exact two call shapes that syntax-only checks (gofmt /
// go/parser) cannot distinguish from a compile error:
//
//   - a fixed [N]byte leaf, whose body expression slices the array before the
//     constructor call (secs2.B(mhead[:])), and
//   - a variable-length []byte leaf, passed straight through (secs2.B(abs)).
//
// The dangerous mistake this catches is spreading either value into secs2.B's
// `values ...any` signature (secs2.B(mhead[:]...) / secs2.B(abs...)): spreading
// a concrete []byte into ...any does not type-check, yet is invisible to a
// syntax-only pass -- the same class of bug that Phase 1's packed node hit.
//
// It renders the message and performs a REAL `go build` of the output against
// the real secs2 package (resolved from the root go-secs module), not a
// syntax-only check. It is gated behind the `integration` build tag because it
// shells out to the go toolchain and reaches across module boundaries; run it
// with `go test -tags integration ./...`.
func TestByteArrayLeavesCompileAgainstRealSecs2(t *testing.T) {
	// One S9F1 body carrying both a fixed [10]byte header leaf (MHEAD) and a
	// variable-length []byte blob leaf (ABS), so a single build exercises the
	// [:]-sliced call and the passthrough call together.
	items := map[string]Item{
		"MHEAD": {Formats: []string{"B"}, Binding: BindingFixed, GoType: "[10]byte"},
		"ABS":   {Formats: []string{"B"}, Binding: BindingFixed, GoType: "[]byte"},
	}
	mf := MessageFile{Stream: 9, Messages: []Message{{
		Function: 1, Name: "Unrecognized Device ID", Mnemonic: "UDN",
		Direction: "equipment-to-host", Description: "Equipment reports an unrecognized device ID.",
		Exception: "None", Source: "e5",
		Bodies: []Body{{
			Actor: "both", ReplyExpected: false,
			Structure: &StructureNode{Type: "list", Items: []StructureNode{
				{Item: "MHEAD"},
				{Item: "ABS"},
			}},
		}},
	}}}
	out, err := renderMessages(mf, items)
	require.NoError(t, err)

	// Sanity-check that both target call shapes actually made it into the
	// rendered source before spending a build on them.
	require.Contains(t, string(out), "secs2.B(mhead[:])")
	require.Contains(t, string(out), "secs2.B(abs)")

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
	require.NoError(t, os.WriteFile(filepath.Join(checkDir, "s9.go"), []byte(src), 0o644))

	// Real compile against real secs2 -- this is the whole point: catch type
	// errors (like spreading []byte into ...any) that syntax-only checks miss.
	target := "./" + filepath.Base(checkDir) + "/"
	cmd := exec.Command("go", "build", target)
	cmd.Dir = rootDir
	buildOut, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "generated S9F1 failed to compile against real secs2:\n%s\n--- source ---\n%s",
		string(buildOut), src)
}
