package logger

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLogLevel_String(t *testing.T) {
	tests := []struct {
		level LogLevel
		want  string
	}{
		{DebugLevel, "Debug"},
		{InfoLevel, "Info"},
		{WarnLevel, "Warn"},
		{ErrorLevel, "Error"},
		{FatalLevel, "Fatal"},
	}
	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			require.Equal(t, tc.want, tc.level.String())
		})
	}
}

func TestDefault_NonNil(t *testing.T) {
	require.NotNil(t, Default())
}
