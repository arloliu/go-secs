package logger

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
)

// captureDefault redirects the package-level default logger's output into an in-memory
// buffer, using the same swap technique as newCapturingSlogLogger in slog_test.go, and
// restores the original *slog.Logger and level via t.Cleanup so other tests never
// observe the mutated global state.
func captureDefault(t *testing.T, level LogLevel) *bytes.Buffer {
	t.Helper()

	sl, ok := Default().(*slogLogger)
	require.True(t, ok, "Default() must return *slogLogger")

	origLogger := sl.logger
	origLevel := sl.level.Level()
	t.Cleanup(func() {
		sl.logger = origLogger
		sl.level.Set(origLevel)
	})

	sl.level.Set(toSlogLevel(level))
	buf := &bytes.Buffer{}
	sl.logger = slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: sl.level}))

	return buf
}

func TestDefault_SetLevel_Level_RoundTrip(t *testing.T) {
	captureDefault(t, InfoLevel)

	original := Level()
	t.Cleanup(func() { SetLevel(original) })

	SetLevel(WarnLevel)
	require.Equal(t, WarnLevel, Level())

	SetLevel(DebugLevel)
	require.Equal(t, DebugLevel, Level())
}

func TestDefault_Wrappers_OutputAndWithSharing(t *testing.T) {
	buf := captureDefault(t, InfoLevel)

	original := Level()
	t.Cleanup(func() { SetLevel(original) })

	SetLevel(WarnLevel)
	require.Equal(t, WarnLevel, Level())

	Debug("suppressed debug")
	require.Empty(t, buf.Bytes(), "Debug must be suppressed once the default level is raised to Warn")

	child := With("component", "default-child")
	require.Equal(t, WarnLevel, child.Level(), "With() child must share the default logger's level state")

	Warn("default warn")
	child.Warn("child warn")
	Error("default error")

	records := decodeLines(t, buf)
	require.Len(t, records, 3)

	require.Equal(t, "default warn", records[0]["msg"])
	require.NotContains(t, records[0], "component", "With() fields must not leak onto the default logger")

	require.Equal(t, "child warn", records[1]["msg"])
	require.Equal(t, "default-child", records[1]["component"])

	require.Equal(t, "default error", records[2]["msg"])
	require.Equal(t, "ERROR", records[2]["level"])
}

func TestDefault_Info(t *testing.T) {
	buf := captureDefault(t, InfoLevel)

	original := Level()
	t.Cleanup(func() { SetLevel(original) })

	Info("default info")

	records := decodeLines(t, buf)
	require.Len(t, records, 1)
	require.Equal(t, "default info", records[0]["msg"])
	require.Equal(t, "INFO", records[0]["level"])
}
