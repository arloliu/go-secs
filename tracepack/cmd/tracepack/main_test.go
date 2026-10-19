package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// corpusDir is the conformance corpus, from this package's directory.
const corpusDir = "../../testdata/corpus"

// corpusPack returns the path of the pack of corpus vector id.
func corpusPack(id string) string {
	return filepath.Join(corpusDir, id, "pack.tpk")
}

// runArgs runs the command line args with cfg and returns its exit code, stdout and stderr.
func runArgs(t *testing.T, cfg config, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runWith(t.Context(), cfg, args, &stdout, &stderr)

	return code, stdout.String(), stderr.String()
}

// countingOpener opens files as the command line does and counts those not yet closed.
type countingOpener struct {
	mu      sync.Mutex
	opened  int
	open    int
	maxOpen int
	// mem, when set, serves the paths it holds from memory instead of the file system.
	mem map[string][]byte
}

// config returns the default configuration with o as its opener.
func (o *countingOpener) config() config {
	cfg := defaultConfig()
	cfg.open = o.openPath

	return cfg
}

// openPath opens path, counting it.
func (o *countingOpener) openPath(path string) (packFile, int64, error) {
	var (
		f    packFile
		size int64
		err  error
	)
	if b, ok := o.mem[path]; ok {
		f, size = nopCloser{bytes.NewReader(b)}, int64(len(b))
	} else if f, size, err = openFile(path); err != nil {
		return nil, 0, err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.opened++
	o.open++
	o.maxOpen = max(o.maxOpen, o.open)

	return &countedFile{packFile: f, o: o}, size, nil
}

// counts returns the number of files opened and of those still open.
func (o *countingOpener) counts() (opened, open int) {
	o.mu.Lock()
	defer o.mu.Unlock()

	return o.opened, o.open
}

// countedFile is a packFile whose close countingOpener counts, once.
type countedFile struct {
	packFile
	o      *countingOpener
	closed bool
}

func (f *countedFile) Close() error {
	f.o.mu.Lock()
	if !f.closed {
		f.closed = true
		f.o.open--
	}
	f.o.mu.Unlock()

	return f.packFile.Close()
}

// nopCloser is a pack held in memory.
type nopCloser struct{ *bytes.Reader }

func (nopCloser) Close() error { return nil }

func TestRunUsage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       []string
		code       int
		stdout     string
		stderr     string
		stdoutNone bool
	}{
		{name: "no command", args: nil, code: exitFailed, stderr: "Usage:", stdoutNone: true},
		{name: "unknown command", args: []string{"list"}, code: exitFailed, stderr: `unknown command "list"`, stdoutNone: true},
		{name: "help", args: []string{"help"}, code: exitClean, stdout: "tracepack dump --jsonl"},
		{name: "-h", args: []string{"-h"}, code: exitClean, stdout: "Exit codes"},
		{name: "dump -h", args: []string{"dump", "-h"}, code: exitClean, stdout: "-jsonl"},
		{name: "dump unknown flag", args: []string{"dump", "--sml", "x"}, code: exitFailed, stderr: "flag provided but not defined: -sml", stdoutNone: true},
		{name: "dump without --jsonl", args: []string{"dump", "x"}, code: exitFailed, stderr: "dump needs --jsonl", stdoutNone: true},
		{name: "dump without a pack", args: []string{"dump", "--jsonl"}, code: exitFailed, stderr: "exactly one pack", stdoutNone: true},
		{name: "dump two packs", args: []string{"dump", "--jsonl", "a", "b"}, code: exitFailed, stderr: "exactly one pack", stdoutNone: true},
		{name: "verify -h", args: []string{"verify", "-h"}, code: exitClean, stdout: "tracepack verify <pack>..."},
		{name: "verify without a pack", args: []string{"verify"}, code: exitFailed, stderr: "one or more packs", stdoutNone: true},
		{name: "find -h", args: []string{"find", "-h"}, code: exitClean, stdout: "-capture uuid"},
		{name: "find without flags", args: []string{"find", "p"}, code: exitFailed, stderr: "needs --capture and --seq", stdoutNone: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			code, stdout, stderr := runArgs(t, defaultConfig(), tt.args...)
			assert.Equal(t, tt.code, code)
			assert.Contains(t, stdout, tt.stdout)
			assert.Contains(t, stderr, tt.stderr)
			if tt.stdoutNone {
				assert.Empty(t, stdout)
			}
		})
	}
}

func TestRunMissingPath(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "missing.tpk")
	for _, args := range [][]string{
		{"dump", "--jsonl", missing},
		{"verify", missing},
		{"find", "--capture", "01a0ffeb-3600-79a6-aba7-dc18d3dbe1d3", "--seq", "0", "--hour", "0", missing},
	} {
		code, stdout, stderr := runArgs(t, defaultConfig(), args...)
		assert.Equal(t, exitFailed, code, "%v", args)
		assert.Empty(t, stdout, "%v", args)
		assert.Contains(t, stderr, missing, "%v", args)
	}
}

func TestOpenFileRefusesDirectory(t *testing.T) {
	t.Parallel()

	_, _, err := openFile(t.TempDir())
	require.Error(t, err)
	assert.ErrorIs(t, err, errNotRegular)

	_, _, err = openFile(filepath.Join(t.TempDir(), "missing"))
	assert.True(t, errors.Is(err, fs.ErrNotExist))
}

func TestRunInterrupted(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, args := range [][]string{
		{"dump", "--jsonl", corpusPack("basic-codec-zstd")},
		{"verify", corpusPack("basic-codec-zstd")},
	} {
		var stdout, stderr bytes.Buffer
		code := runWith(ctx, defaultConfig(), args, &stdout, &stderr)
		assert.Equal(t, exitFailed, code, "%v", args)
		assert.Contains(t, stderr.String(), context.Canceled.Error(), "%v", args)
	}
}

func TestRunHelpWriteFails(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{{"help"}, {"-h"}, {"dump", "-h"}, {"verify", "-h"}, {"find", "-h"}} {
		for name, stdout := range map[string]io.Writer{"failing": &failingWriter{}, "short write": &shortWriter{}} {
			var stderr bytes.Buffer
			code := runWith(t.Context(), defaultConfig(), args, stdout, &stderr)
			assert.Equal(t, exitFailed, code, "%v to a %s stdout", args, name)
			assert.NotEmpty(t, stderr.String(), "%v to a %s stdout", args, name)
		}
	}
}
