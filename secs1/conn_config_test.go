package secs1

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewConnectionConfig_Defaults(t *testing.T) {
	cfg, err := NewConnectionConfig("127.0.0.1", 5000)
	require.NoError(t, err)

	assert.Equal(t, "127.0.0.1", cfg.Host())
	assert.Equal(t, 5000, cfg.Port())
	assert.Equal(t, "127.0.0.1:5000", cfg.Addr())
	assert.Equal(t, uint16(0), cfg.DeviceID())

	assert.True(t, cfg.IsActive())
	assert.False(t, cfg.IsPassive())
	assert.False(t, cfg.IsEquip())
	assert.True(t, cfg.IsHost())
	assert.False(t, cfg.IsMaster()) // Host = Slave per SEMI E4 Section 7.5
	assert.True(t, cfg.IsSlave())

	assert.Equal(t, DefaultT1Timeout, cfg.T1Timeout())
	assert.Equal(t, DefaultT2Timeout, cfg.T2Timeout())
	assert.Equal(t, DefaultT3Timeout, cfg.T3Timeout())
	assert.Equal(t, DefaultT4Timeout, cfg.T4Timeout())
	assert.Equal(t, DefaultRetryLimit, cfg.RetryLimit())
	assert.True(t, cfg.DuplicateDetection())
	assert.False(t, cfg.ValidateDataMessage())

	assert.NotNil(t, cfg.GetLogger())
}

func TestNewConnectionConfig_WithOptions(t *testing.T) {
	cfg, err := NewConnectionConfig("127.0.0.1", 6000,
		WithPassive(),
		WithEquipRole(),
		WithDeviceID(1000),
		WithT1Timeout(1*time.Second),
		WithT2Timeout(5*time.Second),
		WithT3Timeout(60*time.Second),
		WithT4Timeout(60*time.Second),
		WithRetryLimit(5),
		WithDuplicateDetection(false),
		WithValidateDataMessage(true),
		WithSenderQueueSize(20),
		WithDataMsgQueueSize(20),
	)
	require.NoError(t, err)

	assert.Equal(t, 6000, cfg.Port())
	assert.False(t, cfg.IsActive())
	assert.True(t, cfg.IsPassive())
	assert.True(t, cfg.IsEquip())
	assert.False(t, cfg.IsHost())
	assert.True(t, cfg.IsMaster()) // Equipment = Master per SEMI E4 Section 7.5
	assert.False(t, cfg.IsSlave())
	assert.Equal(t, uint16(1000), cfg.DeviceID())
	assert.Equal(t, 1*time.Second, cfg.T1Timeout())
	assert.Equal(t, 5*time.Second, cfg.T2Timeout())
	assert.Equal(t, 60*time.Second, cfg.T3Timeout())
	assert.Equal(t, 60*time.Second, cfg.T4Timeout())
	assert.Equal(t, 5, cfg.RetryLimit())
	assert.False(t, cfg.DuplicateDetection())
	assert.True(t, cfg.ValidateDataMessage())
}

func TestNewConnectionConfig_InvalidHost(t *testing.T) {
	_, err := NewConnectionConfig("!!!invalid!!!", 5000)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid host")
}

func TestNewConnectionConfig_InvalidPort(t *testing.T) {
	_, err := NewConnectionConfig("127.0.0.1", -1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "port")

	_, err = NewConnectionConfig("127.0.0.1", 70000)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "port")
}

func TestNewConnectionConfig_Localhost(t *testing.T) {
	cfg, err := NewConnectionConfig("localhost", 5000)
	require.NoError(t, err)
	assert.Equal(t, "localhost", cfg.Host())
}

// --- Option validation tests ---

func TestWithDeviceID_Max(t *testing.T) {
	cfg, err := NewConnectionConfig("127.0.0.1", 5000, WithDeviceID(MaxDeviceID))
	require.NoError(t, err)
	assert.Equal(t, uint16(MaxDeviceID), cfg.DeviceID())
}

func TestWithDeviceID_OverMax(t *testing.T) {
	_, err := NewConnectionConfig("127.0.0.1", 5000, WithDeviceID(MaxDeviceID+1))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "device ID")
}

func TestWithT1Timeout_BoundaryValid(t *testing.T) {
	cfg, err := NewConnectionConfig("127.0.0.1", 5000, WithT1Timeout(MinT1Timeout))
	require.NoError(t, err)
	assert.Equal(t, MinT1Timeout, cfg.T1Timeout())

	cfg, err = NewConnectionConfig("127.0.0.1", 5000, WithT1Timeout(MaxT1Timeout))
	require.NoError(t, err)
	assert.Equal(t, MaxT1Timeout, cfg.T1Timeout())
}

func TestWithT1Timeout_OutOfRange(t *testing.T) {
	_, err := NewConnectionConfig("127.0.0.1", 5000, WithT1Timeout(50*time.Millisecond))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "T1")

	_, err = NewConnectionConfig("127.0.0.1", 5000, WithT1Timeout(11*time.Second))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "T1")
}

func TestWithT2Timeout_OutOfRange(t *testing.T) {
	_, err := NewConnectionConfig("127.0.0.1", 5000, WithT2Timeout(100*time.Millisecond))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "T2")

	_, err = NewConnectionConfig("127.0.0.1", 5000, WithT2Timeout(30*time.Second))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "T2")
}

func TestWithT3Timeout_OutOfRange(t *testing.T) {
	_, err := NewConnectionConfig("127.0.0.1", 5000, WithT3Timeout(500*time.Millisecond))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "T3")

	_, err = NewConnectionConfig("127.0.0.1", 5000, WithT3Timeout(121*time.Second))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "T3")
}

func TestWithT4Timeout_OutOfRange(t *testing.T) {
	_, err := NewConnectionConfig("127.0.0.1", 5000, WithT4Timeout(500*time.Millisecond))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "T4")

	_, err = NewConnectionConfig("127.0.0.1", 5000, WithT4Timeout(121*time.Second))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "T4")
}

func TestWithRetryLimit_OutOfRange(t *testing.T) {
	_, err := NewConnectionConfig("127.0.0.1", 5000, WithRetryLimit(-1))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "retry limit")

	_, err = NewConnectionConfig("127.0.0.1", 5000, WithRetryLimit(32))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "retry limit")
}

func TestWithRetryLimit_Boundaries(t *testing.T) {
	cfg, err := NewConnectionConfig("127.0.0.1", 5000, WithRetryLimit(0))
	require.NoError(t, err)
	assert.Equal(t, 0, cfg.RetryLimit())

	cfg, err = NewConnectionConfig("127.0.0.1", 5000, WithRetryLimit(MaxRetryLimit))
	require.NoError(t, err)
	assert.Equal(t, MaxRetryLimit, cfg.RetryLimit())
}

func TestWithConnectTimeout_Invalid(t *testing.T) {
	_, err := NewConnectionConfig("127.0.0.1", 5000, WithConnectTimeout(0))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "connect timeout")

	_, err = NewConnectionConfig("127.0.0.1", 5000, WithConnectTimeout(-1*time.Second))
	require.Error(t, err)
}

func TestWithSendTimeout_Invalid(t *testing.T) {
	_, err := NewConnectionConfig("127.0.0.1", 5000, WithSendTimeout(0))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "send timeout")
}

func TestWithLogger_Nil(t *testing.T) {
	_, err := NewConnectionConfig("127.0.0.1", 5000, WithLogger(nil))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "logger")
}

func TestWithSenderQueueSize_Invalid(t *testing.T) {
	_, err := NewConnectionConfig("127.0.0.1", 5000, WithSenderQueueSize(0))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sender queue size")
}

func TestWithDataMsgQueueSize_Invalid(t *testing.T) {
	_, err := NewConnectionConfig("127.0.0.1", 5000, WithDataMsgQueueSize(0))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "data message queue size")
}

// --- Role switching tests ---

func TestActivePassive_Toggle(t *testing.T) {
	// Default is active.
	cfg, err := NewConnectionConfig("127.0.0.1", 5000)
	require.NoError(t, err)
	assert.True(t, cfg.IsActive())

	// Switch to passive, then back to active.
	cfg, err = NewConnectionConfig("127.0.0.1", 5000, WithPassive(), WithActive())
	require.NoError(t, err)
	assert.True(t, cfg.IsActive())
}

func TestEquipHost_Toggle(t *testing.T) {
	// Default is Host (Slave).
	cfg, err := NewConnectionConfig("127.0.0.1", 5000)
	require.NoError(t, err)
	assert.True(t, cfg.IsHost())
	assert.True(t, cfg.IsSlave())

	// Switch to Equip, then back to Host - last option wins.
	cfg, err = NewConnectionConfig("127.0.0.1", 5000, WithEquipRole(), WithHostRole())
	require.NoError(t, err)
	assert.True(t, cfg.IsHost())
	assert.True(t, cfg.IsSlave())
	assert.False(t, cfg.IsEquip())
	assert.False(t, cfg.IsMaster())

	// Equip role sets Master.
	cfg, err = NewConnectionConfig("127.0.0.1", 5000, WithEquipRole())
	require.NoError(t, err)
	assert.True(t, cfg.IsEquip())
	assert.True(t, cfg.IsMaster())
	assert.False(t, cfg.IsHost())
	assert.False(t, cfg.IsSlave())
}
