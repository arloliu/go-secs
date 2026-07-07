package main

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

const smokeItemsYAML = "{}\n"

const smokeS1YAML = `
stream: 1
messages:
  - function: 1
    name: Are You There Request
    mnemonic: R
    direction: bidirectional
    description: Establishes if the equipment is on-line.
    exception: None
    source: e5
    bodies:
      - actor: both
        replyExpected: true
        structure: null
`

// TestRunSmokeS1F1 exercises the full run(...) orchestration end to end: it
// writes a tiny items.yaml and a messages/s1.yaml declaring only S1F1 into a
// scratch directory, calls run, and asserts the written gem/s1.go contains
// func S1F1() and is valid Go per go/parser.
func TestRunSmokeS1F1(t *testing.T) {
	tmp := t.TempDir()

	itemsPath := filepath.Join(tmp, "items.yaml")
	require.NoError(t, os.WriteFile(itemsPath, []byte(smokeItemsYAML), 0o600))

	msgsDir := filepath.Join(tmp, "messages")
	require.NoError(t, os.MkdirAll(msgsDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(msgsDir, "s1.yaml"), []byte(smokeS1YAML), 0o600))

	outDir := filepath.Join(tmp, "out")

	require.NoError(t, run(itemsPath, msgsDir, outDir))

	s1Path := filepath.Join(outDir, "s1.go")
	src, err := os.ReadFile(s1Path)
	require.NoError(t, err)
	require.Contains(t, string(src), "func S1F1()")

	fset := token.NewFileSet()
	_, err = parser.ParseFile(fset, s1Path, src, parser.AllErrors)
	require.NoError(t, err, "generated s1.go must be valid Go")
}
