package logger

import (
	"context"
	"io"
	"log/slog"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/phsym/console-slog"
)

type slogLogger struct {
	mu     sync.Mutex
	logger *slog.Logger
	level  *slog.LevelVar
	output io.Writer
}

// NewSlog create a slog instance
func NewSlog(level LogLevel, addSource bool) Logger {
	inst := &slogLogger{
		output: os.Stdout,
	}

	inst.level = &slog.LevelVar{}
	inst.level.Set(toSlogLevel(level))

	var handler slog.Handler
	if os.Getenv("ENV") == "development" {
		opts := &console.HandlerOptions{
			AddSource: true,
			Level:     inst.level,
		}
		handler = console.NewHandler(inst.output, opts)
	} else {
		opts := &slog.HandlerOptions{
			AddSource: addSource,
			Level:     inst.level,
			ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
				if a.Key == slog.TimeKey {
					a.Key = "ts"
				}

				return a
			},
		}
		handler = slog.NewJSONHandler(inst.output, opts)
	}
	inst.logger = slog.New(handler)

	return inst
}

// Debug logs a message at DebugLevel
// The message includes any fields passed at the log site, as well as any fields accumulated on the logger.
func (l *slogLogger) Debug(msg string, keysAndValues ...any) {
	l.log(context.Background(), slog.LevelDebug, msg, keysAndValues...)
}

// Info logs a message at InfoLevel
// The message includes any fields passed at the log site, as well as any fields accumulated on the logger.
func (l *slogLogger) Info(msg string, keysAndValues ...any) {
	l.log(context.Background(), slog.LevelInfo, msg, keysAndValues...)
}

// Warn logs a message at WarnLevel
// The message includes any fields passed at the log site, as well as any fields accumulated on the logger.
func (l *slogLogger) Warn(msg string, keysAndValues ...any) {
	l.log(context.Background(), slog.LevelWarn, msg, keysAndValues...)
}

// Error logs a message at ErrorLevel
// The message includes any fields passed at the log site, as well as any fields accumulated on the logger.
func (l *slogLogger) Error(msg string, keysAndValues ...any) {
	l.log(context.Background(), slog.LevelError, msg, keysAndValues...)
}

// Fatal logs a message at FatalLevel
// The message includes any fields passed at the log site, as well as any fields accumulated on the logger.
//
// The logger then calls os.Exit(1), even if logging at FatalLevel is disabled.
func (l *slogLogger) Fatal(msg string, keysAndValues ...any) {
	l.log(context.Background(), slog.LevelError, msg, keysAndValues...)
	os.Exit(1) //nolint:revive
}

// With creates a child logger and adds structured context to it.
// Key-values added to the child don't affect the parent, and vice versa.
func (l *slogLogger) With(keyValues ...any) Logger {
	newLog := l.logger.With(keyValues...)

	return &slogLogger{
		logger: newLog,
		level:  l.level,
	}
}

// Level returns the minimum enabled level for this logger.
func (l *slogLogger) Level() LogLevel {
	levelMap := map[slog.Level]LogLevel{
		slog.LevelDebug: DebugLevel,
		slog.LevelInfo:  InfoLevel,
		slog.LevelWarn:  WarnLevel,
		slog.LevelError: ErrorLevel,
	}
	lv := l.level.Level()
	if level, ok := levelMap[lv]; ok {
		return level
	}

	return ErrorLevel
}

// SetLevel sets the minimum enabled level for this logger.
func (l *slogLogger) SetLevel(level LogLevel) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.level.Set(toSlogLevel(level))
}

// log is the low-level logging method for methods that take ...any.
// It must always be called directly by an exported logging method
// or function, because it uses a fixed call depth to obtain the pc.
func (l *slogLogger) log(ctx context.Context, level slog.Level, msg string, args ...any) {
	if !l.logger.Enabled(ctx, level) {
		return
	}
	var pc uintptr
	var pcs [1]uintptr
	// skip [runtime.Callers, this function, this function's caller]
	runtime.Callers(3, pcs[:])
	pc = pcs[0]
	r := slog.NewRecord(time.Now(), level, msg, pc)
	r.Add(args...)
	if ctx == nil {
		ctx = context.Background()
	}
	_ = l.logger.Handler().Handle(ctx, r)
}

func toSlogLevel(level LogLevel) slog.Level {
	levelMap := map[LogLevel]slog.Level{
		DebugLevel: slog.LevelDebug,
		InfoLevel:  slog.LevelInfo,
		WarnLevel:  slog.LevelWarn,
		ErrorLevel: slog.LevelError,
	}
	if slogLevel, ok := levelMap[level]; ok {
		return slogLevel
	}

	return slog.LevelError
}
