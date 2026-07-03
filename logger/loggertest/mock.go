// Package loggertest provides test helpers for the logger package.
// Import this package only in test code; it pulls in testify/mock.
package loggertest

import (
	"github.com/arloliu/go-secs/v2/logger"
	"github.com/stretchr/testify/mock"
)

// MockLogger is a testify-based mock that satisfies logger.Logger.
// Use NewMockLogger to create an instance, then set up expectations with On.
type MockLogger struct {
	mock.Mock
}

var _ logger.Logger = (*MockLogger)(nil)

// NewMockLogger returns a new MockLogger with no expectations set.
func NewMockLogger() *MockLogger {
	return &MockLogger{}
}

// Debug records a call at DebugLevel.
func (m *MockLogger) Debug(msg string, keysAndValues ...any) {
	m.Called(msg, keysAndValues)
}

// Info records a call at InfoLevel.
func (m *MockLogger) Info(msg string, keysAndValues ...any) {
	m.Called(msg, keysAndValues)
}

// Warn records a call at WarnLevel.
func (m *MockLogger) Warn(msg string, keysAndValues ...any) {
	m.Called(msg, keysAndValues)
}

// Error records a call at ErrorLevel.
func (m *MockLogger) Error(msg string, keysAndValues ...any) {
	m.Called(msg, keysAndValues)
}

// Fatal records a call at FatalLevel.
func (m *MockLogger) Fatal(msg string, keysAndValues ...any) {
	m.Called(msg, keysAndValues)
}

// SetLevel records a SetLevel call.
func (m *MockLogger) SetLevel(level logger.LogLevel) {
	m.Called(level)
}

// Level records a Level call and returns the mocked value.
func (m *MockLogger) Level() logger.LogLevel {
	args := m.Called()
	return args.Get(0).(logger.LogLevel) //nolint:errcheck,revive
}

// With records a With call and returns the mocked child logger.
func (m *MockLogger) With(keyValues ...any) logger.Logger {
	args := m.Called(keyValues...)
	return args.Get(0).(logger.Logger) //nolint:errcheck,revive
}
