//go:build unix

package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mainEnv, set in the environment of the test binary, makes it run main instead of the tests,
// so a test can run the command as a process of its own.
const mainEnv = "TRACEPACK_TEST_RUN_MAIN"

func TestMain(m *testing.M) {
	if os.Getenv(mainEnv) == "1" {
		main()
	}
	m.Run()
}

// TestDumpClosedStdout runs dump with a stdout pipe whose reader is closed:
// the write fails with EPIPE instead of a SIGPIPE killing the process, so it exits 2 and says the output is not an export.
func TestDumpClosedStdout(t *testing.T) {
	t.Parallel()

	r, w, err := os.Pipe()
	require.NoError(t, err)
	require.NoError(t, r.Close())
	defer w.Close()

	cmd := exec.Command(os.Args[0], "dump", "--jsonl", corpusPack("basic-codec-zstd"))
	cmd.Env = append(os.Environ(), mainEnv+"=1")
	cmd.Stdout = w
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err = cmd.Run()
	var exit *exec.ExitError
	require.True(t, errors.As(err, &exit), "the command fails: %v", err)
	assert.Equal(t, exitFailed, exit.ExitCode(), "exit status %v", exit.ProcessState)
	assert.Contains(t, stderr.String(), "the output written is not an export")
}

// TestOpenFileRefusesFIFO opens a FIFO no process writes to: the open does not block, and the FIFO is refused.
func TestOpenFileRefusesFIFO(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "fifo.tpk")
	require.NoError(t, syscall.Mkfifo(path, 0o600))
	_, _, err := openFile(path)
	require.ErrorIs(t, err, errNotRegular)

	code, stdout, stderr := runArgs(t, defaultConfig(), "dump", "--jsonl", path)
	assert.Equal(t, exitFailed, code)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, errNotRegular.Error())
}
