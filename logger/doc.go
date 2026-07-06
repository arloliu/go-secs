// Package logger provides a standardized way for different logging frameworks to be integrated
// into go-secs, allowing callers to choose their preferred logging implementation.
//
// [Logger] is the interface every go-secs package logs through: Debug/Info/Warn/Error/Fatal at
// [LogLevel] severities, structured key-value fields, and [Logger.With] for child loggers that
// carry accumulated context. [NewSlog] wraps the standard library's log/slog as the default
// implementation; [Default] and the package-level Debug/Info/Warn/Error/Fatal/With/Level/SetLevel
// functions read and write that default logger for callers that don't need a custom instance.
//
// Test doubles (a recording mock logger) live in the separate
// [github.com/arloliu/go-secs/v2/logger/loggertest] subpackage, keeping testify out of this
// package's import graph.
package logger
