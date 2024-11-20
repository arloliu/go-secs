package logger

import (
	"github.com/stretchr/testify/mock"
)

type MockLogger struct {
	mock.Mock
}

var _ Logger = (*MockLogger)(nil)

func NewMockLogger() *MockLogger {
	return &MockLogger{}
}

func (m *MockLogger) Debug(msg string, keysAndValues ...any) {
	m.Called(msg, keysAndValues)
}

func (m *MockLogger) Info(msg string, keysAndValues ...any) {
	m.Called(msg, keysAndValues)
}

func (m *MockLogger) Warn(msg string, keysAndValues ...any) {
	m.Called(msg, keysAndValues)
}

func (m *MockLogger) Error(msg string, keysAndValues ...any) {
	m.Called(msg, keysAndValues)
}

func (m *MockLogger) Fatal(msg string, keysAndValues ...any) {
	m.Called(msg, keysAndValues)
}

func (m *MockLogger) SetLevel(level LogLevel) {
	m.Called(level)
}

func (m *MockLogger) Level() LogLevel {
	args := m.Called()
	return args.Get(0).(LogLevel)
}

func (m *MockLogger) With(keyValues ...any) Logger {
	args := m.Called(keyValues...)
	return args.Get(0).(Logger)
}
