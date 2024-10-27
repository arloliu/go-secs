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

type SlogLogger struct {
	mu     sync.Mutex
	logger *slog.Logger
	level  *slog.LevelVar
	output io.Writer
}

// NewSlog create a slog instance
func NewSlog(level Level, addSource bool) Logger {
	inst := &SlogLogger{
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
			ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
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

func (l *SlogLogger) Debug(msg string, keysAndValues ...any) {
	l.log(context.Background(), slog.LevelDebug, msg, keysAndValues...)
}

func (l *SlogLogger) Info(msg string, keysAndValues ...any) {
	l.log(context.Background(), slog.LevelInfo, msg, keysAndValues...)
}

func (l *SlogLogger) Warn(msg string, keysAndValues ...any) {
	l.log(context.Background(), slog.LevelWarn, msg, keysAndValues...)
}

func (l *SlogLogger) Error(msg string, keysAndValues ...any) {
	l.log(context.Background(), slog.LevelError, msg, keysAndValues...)
}

func (l *SlogLogger) Fatal(msg string, keysAndValues ...any) {
	l.log(context.Background(), slog.LevelError, msg, keysAndValues...)
	os.Exit(1)
}

func (l *SlogLogger) With(keyValues ...any) Logger {
	newLog := l.logger.With(keyValues...)
	return &SlogLogger{
		logger: newLog,
		level:  l.level,
	}
}

func (l *SlogLogger) Level() Level {
	levelMap := map[slog.Level]Level{
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

func (l *SlogLogger) SetLevel(level Level) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.level.Set(toSlogLevel(level))
}

// log is the low-level logging method for methods that take ...any.
// It must always be called directly by an exported logging method
// or function, because it uses a fixed call depth to obtain the pc.
func (l *SlogLogger) log(ctx context.Context, level slog.Level, msg string, args ...any) {
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

func toSlogLevel(level Level) slog.Level {
	levelMap := map[Level]slog.Level{ //nolint: exhaustive
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
