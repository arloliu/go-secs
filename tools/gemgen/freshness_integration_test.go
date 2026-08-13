//go:build integration

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestGemDirIsFresh is the freshness gate:
// it regenerates every gem/*.go file from the real tools/gemgen/data into a temp dir and diffs each one, byte for byte, against the committed copy under gem/.
//
// Commit 6ce060c hand-reflowed gem/s1.go, s2.go, s5.go, s6.go, s9.go to semantic linefeeds without updating the generator that produces them,
// so `go generate ./gem` silently reverted 161 comment-only lines on the next regeneration;
// nothing caught the drift until an implementer noticed by hand.
// This test is that catch: it runs in `make test-gemgen-integration`, which `make ci` already invokes,
// so a future drift -- comment wrapping or otherwise -- fails CI instead of landing silently.
func TestGemDirIsFresh(t *testing.T) {
	wd, err := os.Getwd()
	require.NoError(t, err)

	rootDir := filepath.Clean(filepath.Join(wd, "..", ".."))
	committedDir := filepath.Join(rootDir, "gem")

	genDir := t.TempDir()
	err = run(filepath.Join(wd, "data", "items.yaml"), filepath.Join(wd, "data", "messages"), genDir)
	require.NoError(t, err, "regeneration itself failed -- fix the generator before the freshness gate can check its output")

	generated, err := os.ReadDir(genDir)
	require.NoError(t, err)
	require.NotEmpty(t, generated, "generator wrote no files -- freshness gate has nothing to check")

	for _, entry := range generated {
		name := entry.Name()

		wantPath := filepath.Join(genDir, name)
		want, err := os.ReadFile(wantPath)
		require.NoError(t, err)

		gotPath := filepath.Join(committedDir, name)
		got, err := os.ReadFile(gotPath)
		require.NoErrorf(t, err, "gem/ is stale -- run go generate ./gem (committed %s is missing)", name)

		if !bytes.Equal(want, got) {
			t.Errorf("gem/ is stale -- run go generate ./gem (gem/%s does not match what the generator produces)", name)
		}
	}
}
