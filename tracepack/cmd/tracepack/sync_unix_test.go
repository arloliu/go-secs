//go:build unix

package main

import (
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSyncDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, syncDir(dir))
	require.ErrorIs(t, syncDir(filepath.Join(dir, "missing")), fs.ErrNotExist)
}
