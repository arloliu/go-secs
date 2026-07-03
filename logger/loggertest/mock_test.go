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
