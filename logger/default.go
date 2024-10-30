package logger

var defLogger = NewSlog(InfoLevel, false)

// Debug uses the default logger to log a message at DebugLevel.
// The message includes any fields passed at the log site, as well as any fields accumulated on the logger.
func Debug(msg string, keysAndValues ...any) {
	defLogger.Debug(msg, keysAndValues...)
}

// Info uses the default logger to log a message at InfoLevel.
// The message includes any fields passed at the log site, as well as any fields accumulated on the logger.
func Info(msg string, keysAndValues ...any) {
	defLogger.Info(msg, keysAndValues...)
}

// Warn uses the default logger to log a message at WarnLevel.
// The message includes any fields passed at the log site, as well as any fields accumulated on the logger.
func Warn(msg string, keysAndValues ...any) {
	defLogger.Warn(msg, keysAndValues...)
}

// Error uses the default logger to log a message at ErrorLevel.
// The message includes any fields passed at the log site, as well as any fields accumulated on the logger.
func Error(msg string, keysAndValues ...any) {
	defLogger.Error(msg, keysAndValues...)
}

// Fatal uses the default logger to log a message at FatalLevel.
// The message includes any fields passed at the log site, as well as any fields accumulated on the logger.
//
// The logger then calls os.Exit(1), even if logging at FatalLevel is disabled.
func Fatal(msg string, keysAndValues ...any) {
	defLogger.Fatal(msg, keysAndValues...)
}

// SetLevel sets the minimum enabled level for the default logger.
func SetLevel(level LogLevel) {
	defLogger.SetLevel(level)
}

// Level returns the minimum enabled level for the default logger.
func Level() LogLevel {
	return defLogger.Level()
}

// GetLogger retrieves the default logger of go-secs.
func GetLogger() Logger {
	return defLogger
}

// With creates a child logger and adds structured context to it.
// Key-values added to the child don't affect the parent, and vice versa.
func With(keyValues ...any) Logger {
	return defLogger.With(keyValues...)
}
