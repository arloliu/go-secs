package logger

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// newCapturingSlogLogger builds a slogLogger as NewSlog does,
// then swaps its handler for one writing into an in-memory buffer instead of os.Stdout.
// NewSlog hardcodes os.Stdout and takes no writer, so this is the seam these tests use;
// it works because the tests live in package logger and can reach the unexported fields.
//
// The replacement handler reuses the LevelVar NewSlog created,
// so SetLevel, Level, and the sharing With's godoc promises behave as they do in production.
// Only the destination of the bytes changes.
//
// Because the handler itself is replaced, this seam cannot see how NewSlog wired its own.
// TestNewSlog_ProductionHandlerHonorsLevel covers that by capturing os.Stdout instead.
func newCapturingSlogLogger(t *testing.T, level LogLevel) (*slogLogger, *bytes.Buffer) {
	t.Helper()

	l := NewSlog(level, false)
	sl, ok := l.(*slogLogger)
	require.True(t, ok, "NewSlog must return *slogLogger")

	buf := &bytes.Buffer{}
	handler := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: sl.level})
	sl.logger = slog.New(handler)
	sl.output = buf

	return sl, buf
}

// decodeLines decodes a buffer of newline-delimited JSON log records emitted by the
// standard library's JSON handler.
func decodeLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()

	var records []map[string]any
	dec := json.NewDecoder(buf)
	for dec.More() {
		var rec map[string]any
		require.NoError(t, dec.Decode(&rec))
		records = append(records, rec)
	}

	return records
}

func TestNewSlog_ConstructsForAllOptionCombinations(t *testing.T) {
	tests := []struct {
		name      string
		env       string
		addSource bool
	}{
		{"json no source", "", false},
		{"json with source", "", true},
		{"console development", "development", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.env != "" {
				t.Setenv("ENV", tt.env)
			}

			l := NewSlog(InfoLevel, tt.addSource)
			require.NotNil(t, l)
			require.Equal(t, InfoLevel, l.Level())
		})
	}
}

// TestNewSlog_WritesJSONToActualStdout captures NewSlog's real, unmodified output path:
// os.Stdout redirected to a pipe, with no handler swap.
// This is the only way to observe the JSON handler's ReplaceAttr closure (which renames
// the "time" key to "ts"), since that closure is baked into the handler NewSlog
// constructs and is bypassed entirely by the handler-swap helper used elsewhere in this
// file.
func TestNewSlog_WritesJSONToActualStdout(t *testing.T) {
	r, w, err := os.Pipe()
	require.NoError(t, err)

	origStdout := os.Stdout
	os.Stdout = w
	t.Cleanup(func() {
		os.Stdout = origStdout
		_ = r.Close()
		_ = w.Close()
	})

	l := NewSlog(InfoLevel, false)
	l.Info("hello from stdout")

	require.NoError(t, w.Close())
	os.Stdout = origStdout

	data, err := io.ReadAll(r)
	require.NoError(t, err)

	var rec map[string]any
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(data), &rec))
	require.Equal(t, "hello from stdout", rec["msg"])
	require.Contains(t, rec, "ts", "ReplaceAttr must rename the time key to ts")
	require.NotContains(t, rec, "time")
}

func TestSlogLogger_LevelFiltering(t *testing.T) {
	sl, buf := newCapturingSlogLogger(t, InfoLevel)

	sl.Debug("debug message")
	require.Empty(t, buf.Bytes(), "Debug must be suppressed below InfoLevel")

	sl.Warn("warn message")
	records := decodeLines(t, buf)
	require.Len(t, records, 1)
	require.Equal(t, "warn message", records[0]["msg"])
	require.Equal(t, "WARN", records[0]["level"])
}

func TestSlogLogger_Debug_Info_Warn_Error_EmitAtDebugLevel(t *testing.T) {
	sl, buf := newCapturingSlogLogger(t, DebugLevel)

	sl.Debug("debug message", "k", "v")
	sl.Info("info message")
	sl.Warn("warn message")
	sl.Error("error message")

	records := decodeLines(t, buf)
	require.Len(t, records, 4)

	require.Equal(t, "debug message", records[0]["msg"])
	require.Equal(t, "DEBUG", records[0]["level"])
	require.Equal(t, "v", records[0]["k"])

	require.Equal(t, "info message", records[1]["msg"])
	require.Equal(t, "INFO", records[1]["level"])

	require.Equal(t, "warn message", records[2]["msg"])
	require.Equal(t, "WARN", records[2]["level"])

	require.Equal(t, "error message", records[3]["msg"])
	require.Equal(t, "ERROR", records[3]["level"])
}

func TestSlogLogger_SetLevel_Level_RoundTrip(t *testing.T) {
	tests := []struct {
		name string
		set  LogLevel
		want LogLevel
	}{
		{"debug", DebugLevel, DebugLevel},
		{"info", InfoLevel, InfoLevel},
		{"warn", WarnLevel, WarnLevel},
		{"error", ErrorLevel, ErrorLevel},
		{"fatal maps to error", FatalLevel, ErrorLevel},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sl, _ := newCapturingSlogLogger(t, InfoLevel)

			sl.SetLevel(tt.set)
			require.Equal(t, tt.want, sl.Level())
		})
	}
}

func TestSlogLogger_SetLevel_SuppressesBelowNewLevel(t *testing.T) {
	sl, buf := newCapturingSlogLogger(t, InfoLevel)

	sl.SetLevel(WarnLevel)
	require.Equal(t, WarnLevel, sl.Level())

	sl.Debug("suppressed debug")
	sl.Info("suppressed info")
	require.Empty(t, buf.Bytes(), "Debug and Info must be suppressed once level is raised to Warn")

	sl.Warn("emitted warn")
	require.NotEmpty(t, buf.Bytes())
}

// TestSlogLogger_With_SharesLevelState is the headline assertion for this coverage pass.
//
// logger.Logger's SetLevel godoc states: "Child loggers created with With share the same
// log-level state as their parent; only key-value fields are isolated per child."
// This works because slogLogger.With copies the parent's *slog.LevelVar pointer (not its
// value) into the child, while the child's *slog.Logger is a fresh value built via
// slog.Logger.With(keyValues...) that carries its own attributes.
// This test proves both halves of that contract: the LevelVar is shared in both
// directions, and the attached fields never cross between parent and child.
func TestSlogLogger_With_SharesLevelState(t *testing.T) {
	parent, buf := newCapturingSlogLogger(t, InfoLevel)

	child := parent.With("component", "child")

	// Level state is shared: a SetLevel call on the parent is visible through the
	// child's Level(), because both hold the same *slog.LevelVar.
	parent.SetLevel(ErrorLevel)
	require.Equal(t, ErrorLevel, parent.Level())
	require.Equal(t, ErrorLevel, child.Level(), "child must observe the parent's level change")

	// The sharing is bidirectional: a SetLevel call from the child side is visible
	// on the parent too, since it's the same LevelVar, not an independent copy.
	child.SetLevel(WarnLevel)
	require.Equal(t, WarnLevel, parent.Level(), "parent must observe a level change made via the child")
	require.Equal(t, WarnLevel, child.Level())

	// Fields, in contrast, must NOT leak in either direction.
	// The child derives its *slog.Logger from the parent's handler chain via
	// slog.Logger.With, so both write into the same captured buffer but must carry
	// distinct attribute sets.
	buf.Reset()
	parent.Warn("parent message")
	child.Warn("child message")

	records := decodeLines(t, buf)
	require.Len(t, records, 2)

	require.Equal(t, "parent message", records[0]["msg"])
	require.NotContains(t, records[0], "component", "With() fields must not leak onto the parent")

	require.Equal(t, "child message", records[1]["msg"])
	require.Equal(t, "child", records[1]["component"], "With() fields must be present on the child")
}

// TestSlogLogger_Level_UnknownSlogLevelFallsBackToError exercises Level's default
// fallback: a *slog.LevelVar holding a raw slog.Level outside the four mapped values
// (Debug/Info/Warn/Error) must be reported as ErrorLevel.
func TestSlogLogger_Level_UnknownSlogLevelFallsBackToError(t *testing.T) {
	sl, _ := newCapturingSlogLogger(t, InfoLevel)

	sl.level.Set(slog.Level(99))
	require.Equal(t, ErrorLevel, sl.Level())
}

// TestToSlogLevel_UnknownLogLevelFallsBackToError exercises toSlogLevel's default
// fallback: a LogLevel outside the five known constants must map to slog.LevelError.
func TestToSlogLevel_UnknownLogLevelFallsBackToError(t *testing.T) {
	require.Equal(t, slog.LevelError, toSlogLevel(LogLevel(99)))
}

func TestSlogLogger_With_MultipleChildrenAreIndependent(t *testing.T) {
	parent, buf := newCapturingSlogLogger(t, InfoLevel)

	childA := parent.With("who", "a")
	childB := parent.With("who", "b")

	childA.Info("from a")
	childB.Info("from b")

	records := decodeLines(t, buf)
	require.Len(t, records, 2)
	require.Equal(t, "a", records[0]["who"])
	require.Equal(t, "b", records[1]["who"])
}

// TestNewSlog_ProductionHandlerHonorsLevel drives the handler NewSlog actually builds,
// rather than the replacement handler newCapturingSlogLogger installs.
// The capturing seam wires its own handler to the same LevelVar,
// so it would keep passing if NewSlog ever wired its production handler to a fixed level
// or to a different LevelVar than the one SetLevel mutates.
// Capturing os.Stdout is what makes that wiring observable.
func TestNewSlog_ProductionHandlerHonorsLevel(t *testing.T) {
	t.Setenv("ENV", "") // force the JSON handler branch rather than the console one

	reader, writer, err := os.Pipe()
	require.NoError(t, err)

	original := os.Stdout
	os.Stdout = writer

	restore := func() { os.Stdout = original }
	t.Cleanup(func() {
		restore()
		_ = reader.Close()
		_ = writer.Close()
	})

	logger := NewSlog(InfoLevel, false)

	logger.Debug("below the initial level")
	logger.Info("at the initial level")

	logger.SetLevel(ErrorLevel)
	logger.Info("below the raised level")
	logger.Error("at the raised level")

	require.NoError(t, writer.Close())
	restore()

	out, err := io.ReadAll(reader)
	require.NoError(t, err)

	emitted := string(out)
	require.NotContains(t, emitted, "below the initial level", "Debug must be filtered at InfoLevel")
	require.Contains(t, emitted, "at the initial level")
	require.NotContains(t, emitted, "below the raised level", "SetLevel must retune the production handler")
	require.Contains(t, emitted, "at the raised level")
}
