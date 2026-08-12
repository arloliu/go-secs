package loggertest_test

import (
	"testing"

	"github.com/arloliu/go-secs/v2/logger"
	"github.com/arloliu/go-secs/v2/logger/loggertest"
	"github.com/stretchr/testify/require"
)

// TestMockLogger_SatisfiesInterface verifies that MockLogger implements logger.Logger
// and that NewMockLogger returns a non-nil instance.
func TestMockLogger_SatisfiesInterface(t *testing.T) {
	m := loggertest.NewMockLogger()
	require.NotNil(t, m)

	// Compile-time check — MockLogger must satisfy logger.Logger.
	var _ logger.Logger = m
}

// TestMockLogger_LoggingMethods_RecordCalls drives each of MockLogger's message-logging
// methods and verifies the call (message plus the keysAndValues slice as a single
// recorded argument) is dispatched through testify's mock.Mock as configured.
func TestMockLogger_LoggingMethods_RecordCalls(t *testing.T) {
	tests := []struct {
		name   string
		method string
		msg    string
		invoke func(m *loggertest.MockLogger)
	}{
		{
			name:   "Debug",
			method: "Debug",
			msg:    "debug msg",
			invoke: func(m *loggertest.MockLogger) { m.Debug("debug msg", "k", "v") },
		},
		{
			name:   "Info",
			method: "Info",
			msg:    "info msg",
			invoke: func(m *loggertest.MockLogger) { m.Info("info msg", "k", "v") },
		},
		{
			name:   "Warn",
			method: "Warn",
			msg:    "warn msg",
			invoke: func(m *loggertest.MockLogger) { m.Warn("warn msg", "k", "v") },
		},
		{
			name:   "Error",
			method: "Error",
			msg:    "error msg",
			invoke: func(m *loggertest.MockLogger) { m.Error("error msg", "k", "v") },
		},
		{
			name:   "Fatal",
			method: "Fatal",
			msg:    "fatal msg",
			invoke: func(m *loggertest.MockLogger) { m.Fatal("fatal msg", "k", "v") },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := loggertest.NewMockLogger()
			// MockLogger forwards keysAndValues as a single []any argument (it calls
			// m.Called(msg, keysAndValues) without spreading), so the expectation
			// matches on the message string plus that slice.
			m.On(tt.method, tt.msg, []any{"k", "v"}).Return()

			tt.invoke(m)

			m.AssertExpectations(t)
		})
	}
}

// TestMockLogger_SetLevel verifies SetLevel forwards its argument to the mock's call
// recorder.
func TestMockLogger_SetLevel(t *testing.T) {
	m := loggertest.NewMockLogger()
	m.On("SetLevel", logger.WarnLevel).Return()

	m.SetLevel(logger.WarnLevel)

	m.AssertExpectations(t)
}

// TestMockLogger_Level verifies Level returns the mocked value.
func TestMockLogger_Level(t *testing.T) {
	m := loggertest.NewMockLogger()
	m.On("Level").Return(logger.WarnLevel)

	require.Equal(t, logger.WarnLevel, m.Level())
	m.AssertExpectations(t)
}

// TestMockLogger_With verifies With spreads its keyValues into the call recorder
// (unlike the message-logging methods, MockLogger.With calls m.Called(keyValues...))
// and returns the configured child logger.
func TestMockLogger_With(t *testing.T) {
	m := loggertest.NewMockLogger()
	child := loggertest.NewMockLogger()
	m.On("With", "k", "v").Return(child)

	got := m.With("k", "v")

	require.Same(t, child, got)
	m.AssertExpectations(t)
}
