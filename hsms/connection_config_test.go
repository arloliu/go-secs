package hsms

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestConnectionConfig_Defaults(t *testing.T) {
	c := DefaultConnectionConfig()
	require.Positive(t, c.timers.T3)
	require.Positive(t, c.timers.T6)
	require.Equal(t, uint16(0xFFFF), c.sessionID) // control SessionID default for HSMS-SS
}

func TestConnectionConfig_Apply_AllOrNothing(t *testing.T) {
	c := DefaultConnectionConfig()
	origT3 := c.timers.T3
	// One valid + one invalid option: NOTHING must commit (transactional, landmine D).
	err := c.apply(WithT3(2*time.Second), WithT6(-1))
	require.Error(t, err)
	require.Equal(t, origT3, c.timers.T3, "valid option must NOT commit when a sibling option fails")
}

func TestConnectionConfig_Apply_CommitsWhenAllValid(t *testing.T) {
	c := DefaultConnectionConfig()
	require.NoError(t, c.apply(WithT3(2*time.Second), WithSessionID(7)))
	require.Equal(t, 2*time.Second, c.timers.T3)
	require.Equal(t, uint16(7), c.sessionID)
}

func TestConnectionConfig_WithT5(t *testing.T) {
	c := DefaultConnectionConfig()
	require.NoError(t, c.apply(WithT5(20*time.Second)))
	require.Equal(t, 20*time.Second, c.timers.T5)

	err := c.apply(WithT5(-1))
	require.Error(t, err)
}

func TestWithReconnectBackoff_Validation(t *testing.T) {
	cfg := DefaultConnectionConfig()

	require.NoError(t, cfg.apply(WithReconnectBackoff(50*time.Millisecond, 1.5)))

	require.Error(t, cfg.apply(WithReconnectBackoff(0, 2.0)), "initial must be > 0")
	require.Error(t, cfg.apply(WithReconnectBackoff(-time.Second, 2.0)), "initial must be > 0")
	require.Error(t, cfg.apply(WithReconnectBackoff(time.Second, 0.5)), "multiplier must be >= 1.0")

	require.NoError(t, cfg.apply(WithReconnectBackoff(time.Second, 1.0)), "multiplier == 1.0 is valid (flat)")
}

func TestConnectionConfig_WithT7(t *testing.T) {
	c := DefaultConnectionConfig()
	require.NoError(t, c.apply(WithT7(15*time.Second)))
	require.Equal(t, 15*time.Second, c.timers.T7)

	err := c.apply(WithT7(0))
	require.Error(t, err)
}

func TestConnectionConfig_WithT8(t *testing.T) {
	c := DefaultConnectionConfig()
	require.NoError(t, c.apply(WithT8(3*time.Second)))
	require.Equal(t, 3*time.Second, c.timers.T8)

	err := c.apply(WithT8(-5 * time.Second))
	require.Error(t, err)
}

func TestConnectionConfig_WithLinktestInterval(t *testing.T) {
	c := DefaultConnectionConfig()
	// 0 is valid (disables linktest)
	require.NoError(t, c.apply(WithLinktestInterval(0)))
	require.Equal(t, time.Duration(0), c.linktestInterval)

	require.NoError(t, c.apply(WithLinktestInterval(30*time.Second)))
	require.Equal(t, 30*time.Second, c.linktestInterval)

	err := c.apply(WithLinktestInterval(-1))
	require.Error(t, err)
}

func TestConnectionConfig_WithLinktestFailThreshold(t *testing.T) {
	c := DefaultConnectionConfig()
	require.NoError(t, c.apply(WithLinktestFailThreshold(5)))
	require.Equal(t, 5, c.linktestFailThreshold)

	err := c.apply(WithLinktestFailThreshold(0))
	require.Error(t, err)
}

func TestConnectionConfig_WithSenderQueueSize(t *testing.T) {
	c := DefaultConnectionConfig()
	require.NoError(t, c.apply(WithSenderQueueSize(128)))
	require.Equal(t, 128, c.senderQueueSize)

	err := c.apply(WithSenderQueueSize(0))
	require.Error(t, err)
}

func TestConnectionConfig_WithCloseTimeout(t *testing.T) {
	c := DefaultConnectionConfig()
	require.NoError(t, c.apply(WithCloseTimeout(5*time.Second)))
	require.Equal(t, 5*time.Second, c.closeTimeout)

	err := c.apply(WithCloseTimeout(0))
	require.Error(t, err)
}

func TestConnectionConfig_WithLogger(t *testing.T) {
	c := DefaultConnectionConfig()
	require.NotNil(t, c.logger)

	err := c.apply(WithLogger(nil))
	require.Error(t, err)
}

func TestConnectionConfig_Logger(t *testing.T) {
	cfg := DefaultConnectionConfig()
	require.NotNil(t, cfg.Logger(), "default config has a non-nil logger")
}

func TestConnectionConfig_Apply_MultipleErrors(t *testing.T) {
	c := DefaultConnectionConfig()
	origT3 := c.timers.T3
	origT5 := c.timers.T5
	// Two invalid options: both errors returned, nothing commits.
	err := c.apply(WithT3(-1), WithT5(-2))
	require.Error(t, err)
	require.Equal(t, origT3, c.timers.T3)
	require.Equal(t, origT5, c.timers.T5)
}

func TestConnectionConfig_WithT1(t *testing.T) {
	c := DefaultConnectionConfig()
	require.NoError(t, c.apply(WithT1(200*time.Millisecond)))
	require.Equal(t, 200*time.Millisecond, c.timers.T1)

	err := c.apply(WithT1(-1))
	require.Error(t, err)

	err = c.apply(WithT1(0))
	require.Error(t, err)
}

func TestConnectionConfig_WithT2(t *testing.T) {
	c := DefaultConnectionConfig()
	require.NoError(t, c.apply(WithT2(5*time.Second)))
	require.Equal(t, 5*time.Second, c.timers.T2)

	err := c.apply(WithT2(-1))
	require.Error(t, err)
}

func TestConnectionConfig_WithT4(t *testing.T) {
	c := DefaultConnectionConfig()
	require.NoError(t, c.apply(WithT4(30*time.Second)))
	require.Equal(t, 30*time.Second, c.timers.T4)

	err := c.apply(WithT4(-1))
	require.Error(t, err)
}

func TestDefaultConnectionConfig_SECS1Timers(t *testing.T) {
	c := DefaultConnectionConfig()
	require.Equal(t, 500*time.Millisecond, c.timers.T1)
	require.Equal(t, 10*time.Second, c.timers.T2)
	require.Equal(t, 45*time.Second, c.timers.T4)
}

func TestConnectionConfig_WithAutoS9F9(t *testing.T) {
	cfg := DefaultConnectionConfig()
	require.False(t, cfg.AutoS9F9(), "default is disabled")

	require.NoError(t, cfg.apply(WithAutoS9F9(true)))
	require.True(t, cfg.AutoS9F9())

	require.NoError(t, cfg.apply(WithAutoS9F9(false)))
	require.False(t, cfg.AutoS9F9())
}

func TestConnectionConfig_WithTraceTraffic(t *testing.T) {
	cfg := DefaultConnectionConfig()
	require.False(t, cfg.TraceTraffic(), "default is disabled")

	require.NoError(t, cfg.apply(WithTraceTraffic(true)))
	require.True(t, cfg.TraceTraffic())
}
