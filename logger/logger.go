// Package logger provides a standardized way for different logging frameworks to be integrated into go-secs,
// allowing users to choose their preferred logging implementation.
//
// The Logger interface defines methods for logging messages at various severity levels (Debug, Info, Warn, Error, Fatal)
// and supports structured logging with key-value pairs.
//
// Log Levels:
//
//   - DebugLevel:  Detailed debug information, typically disabled in production.
//   - InfoLevel:  General informational messages.
//   - WarnLevel:  Warnings about potential issues.
//   - ErrorLevel:  Errors that require attention.
//   - FatalLevel:  Critical errors that cause program termination.
package logger

// LogLevel indicates the logging severity level.
type LogLevel = int8

const (
	// DebugLevel logs are typically voluminous, and are usually disabled in production.
	DebugLevel LogLevel = iota - 1
	// InfoLevel is the default logging priority.
	InfoLevel
	// WarnLevel logs are more important than Info, but don't need individual
	// human review.
	WarnLevel
	// ErrorLevel logs are high-priority. If an application is running smoothly,
	// it shouldn't generate any error-level logs.
	ErrorLevel
	// FatalLevel logs a message, then calls os.Exit(1).
	FatalLevel
)

// Logger defines a common interface for logging.
// This interface is used throughout the go-secs packages, enabling integration with various logging frameworks.
type Logger interface {
	// Debug logs a message at DebugLevel.
	// The message includes any fields passed at the log site, as well as any fields accumulated on the logger.
	Debug(msg string, keysAndValues ...any)
	// Info logs a message at InfoLevel.
	// The message includes any fields passed at the log site, as well as any fields accumulated on the logger.
	Info(msg string, keysAndValues ...any)
	// Warn logs a message at WarnLevel.
	// The message includes any fields passed at the log site, as well as any fields accumulated on the logger.
	Warn(msg string, keysAndValues ...any)
	// Error logs a message at ErrorLevel
	// The message includes any fields passed at the log site, as well as any fields accumulated on the logger.
	Error(msg string, keysAndValues ...any)
	// Fatal logs a message at FatalLevel
	// The message includes any fields passed at the log site, as well as any fields accumulated on the logger.
	//
	// The logger then calls os.Exit(1), even if logging at FatalLevel is disabled.
	Fatal(msg string, keysAndValues ...any)
	// With creates a child logger and adds structured context to it.
	// Key-values added to the child don't affect the parent, and vice versa.
	With(keyValues ...any) Logger
	// Level returns the minimum enabled level for this logger.
	Level() LogLevel
	// SetLevel sets the minimum enabled level for this logger.
	SetLevel(level LogLevel)
}
