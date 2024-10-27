package logger

var defLogger = newSlog(InfoLevel, false)

func Debug(msg string, keysAndValues ...any) {
	defLogger.Debug(msg, keysAndValues...)
}

func Info(msg string, keysAndValues ...any) {
	defLogger.Info(msg, keysAndValues...)
}

func Warn(msg string, keysAndValues ...any) {
	defLogger.Warn(msg, keysAndValues...)
}

func Error(msg string, keysAndValues ...any) {
	defLogger.Error(msg, keysAndValues...)
}

func Fatal(msg string, keysAndValues ...any) {
	defLogger.Fatal(msg, keysAndValues...)
}

func SetLevel(level Level) {
	defLogger.SetLevel(level)
}

func GetLogger() Logger {
	return defLogger
}

func With(keyValues ...any) Logger {
	return defLogger.With(keyValues...)
}
