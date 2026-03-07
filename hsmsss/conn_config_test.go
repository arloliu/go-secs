package hsmsss

import (
	"testing"
	"time"

	"github.com/arloliu/go-secs/hsms"
	"github.com/stretchr/testify/require"
)

func TestNewConnectionConfig(t *testing.T) {
	require := require.New(t)

	t.Run("Valid Configuration", func(t *testing.T) {
		cfg, err := NewConnectionConfig("192.168.1.1", 5000,
			WithPassive(),
			WithT3Timeout(60*time.Second),
			WithT5Timeout(60*time.Second),
			WithT6Timeout(60*time.Second),
			WithT7Timeout(60*time.Second),
			WithT8Timeout(60*time.Second),
			WithConnectRemoteTimeout(30*time.Second),
		)
		require.NoError(err)
		require.Equal("192.168.1.1", cfg.host)
		require.Equal(5000, cfg.port)
		require.False(cfg.isActive)
		require.Equal(60*time.Second, cfg.t3Timeout)

		require.NoError(WithActive().apply(cfg))
		require.True(cfg.isActive)
	})

	t.Run("Default KeepAlivePeriod and IdleReadTimeout", func(t *testing.T) {
		cfg, err := NewConnectionConfig("192.168.1.1", 5000)
		require.NoError(err)
		require.Equal(30*time.Second, cfg.KeepAlivePeriod())
		require.Equal(10*time.Second, cfg.IdleReadTimeout())
	})

	t.Run("Invalid IP Address", func(t *testing.T) {
		_, err := NewConnectionConfig("invalid-ip", 5000)
		require.Error(err)
		require.EqualError(err, "invalid host")
	})

	t.Run("Invalid Port - Below Range", func(t *testing.T) {
		_, err := NewConnectionConfig("192.168.1.1", -1)
		require.Error(err)
		require.EqualError(err, "port is out of range [1, 65535]")
	})

	t.Run("Invalid Port - Above Range", func(t *testing.T) {
		_, err := NewConnectionConfig("192.168.1.1", 65536)
		require.Error(err)
		require.EqualError(err, "port is out of range [1, 65535]")
	})

	// Add more test cases for other options and their error scenarios

	t.Run("Invalid T3 Timeout - Out of Range", func(t *testing.T) {
		_, err := NewConnectionConfig("192.168.1.1", 5000, WithT3Timeout(0))
		require.Error(err)
		require.EqualError(err, "t3 timeout out of range [1, 600]")

		_, err = NewConnectionConfig("192.168.1.1", 5000, WithT3Timeout(121))
		require.Error(err)
		require.EqualError(err, "t3 timeout out of range [1, 600]")

		err = WithT3Timeout(5).apply(nil)
		require.Error(err)
		require.ErrorIs(hsms.ErrConnConfigNil, err)
	})

	t.Run("Invalid T5 Timeout - Below Range", func(t *testing.T) {
		_, err := NewConnectionConfig("192.168.1.1", 5000, WithT5Timeout(0))
		require.Error(err)
		require.EqualError(err, "t5 timeout out of range [0.01, 240]")

		_, err = NewConnectionConfig("192.168.1.1", 5000, WithT5Timeout(241))
		require.Error(err)
		require.EqualError(err, "t5 timeout out of range [0.01, 240]")

		err = WithT5Timeout(5).apply(nil)
		require.Error(err)
		require.ErrorIs(hsms.ErrConnConfigNil, err)
	})

	t.Run("Invalid T6 Timeout", func(t *testing.T) {
		_, err := NewConnectionConfig("192.168.1.1", 5000, WithT6Timeout(0))
		require.Error(err)
		require.EqualError(err, "t6 timeout out of range [1, 240]")

		_, err = NewConnectionConfig("192.168.1.1", 5000, WithT6Timeout(241*time.Second))
		require.Error(err)
		require.EqualError(err, "t6 timeout out of range [1, 240]")

		err = WithT6Timeout(5).apply(nil)
		require.Error(err)
		require.ErrorIs(hsms.ErrConnConfigNil, err)
	})

	t.Run("Invalid T7 Timeout", func(t *testing.T) {
		_, err := NewConnectionConfig("192.168.1.1", 5000, WithT7Timeout(0))
		require.Error(err)
		require.EqualError(err, "t7 timeout out of range [1, 240]")

		_, err = NewConnectionConfig("192.168.1.1", 5000, WithT7Timeout(241*time.Second))
		require.Error(err)
		require.EqualError(err, "t7 timeout out of range [1, 240]")

		err = WithT7Timeout(5).apply(nil)
		require.Error(err)
		require.ErrorIs(hsms.ErrConnConfigNil, err)
	})

	t.Run("Invalid T8 Timeout", func(t *testing.T) {
		_, err := NewConnectionConfig("192.168.1.1", 5000, WithT8Timeout(0))
		require.Error(err)
		require.EqualError(err, "t8 timeout out of range [1, 120]")

		_, err = NewConnectionConfig("192.168.1.1", 5000, WithT8Timeout(121*time.Second))
		require.Error(err)
		require.EqualError(err, "t8 timeout out of range [1, 120]")

		err = WithT8Timeout(5).apply(nil)
		require.Error(err)
		require.ErrorIs(hsms.ErrConnConfigNil, err)
	})

	t.Run("Invalid ConnectRemote Timeout", func(t *testing.T) {
		_, err := NewConnectionConfig("192.168.1.1", 5000, WithConnectRemoteTimeout(0))
		require.Error(err)
		require.EqualError(err, "connect remote timeout out of range [1, 30]")

		_, err = NewConnectionConfig("192.168.1.1", 5000, WithConnectRemoteTimeout(31*time.Second))
		require.Error(err)
		require.EqualError(err, "connect remote timeout out of range [1, 30]")

		err = WithConnectRemoteTimeout(5).apply(nil)
		require.Error(err)
		require.ErrorIs(hsms.ErrConnConfigNil, err)
	})

	t.Run("Invalid KeepAlivePeriod", func(t *testing.T) {
		_, err := NewConnectionConfig("192.168.1.1", 5000, WithKeepAlivePeriod(-1))
		require.Error(err)
		require.EqualError(err, "keep alive period out of range [0, 120]")

		_, err = NewConnectionConfig("192.168.1.1", 5000, WithKeepAlivePeriod(121*time.Second))
		require.Error(err)
		require.EqualError(err, "keep alive period out of range [0, 120]")

		err = WithKeepAlivePeriod(5 * time.Second).apply(nil)
		require.Error(err)
		require.ErrorIs(hsms.ErrConnConfigNil, err)
	})

	t.Run("Invalid IdleReadTimeout", func(t *testing.T) {
		_, err := NewConnectionConfig("192.168.1.1", 5000, WithIdleReadTimeout(0))
		require.Error(err)
		require.EqualError(err, "idle read timeout out of range [1, 120]")

		_, err = NewConnectionConfig("192.168.1.1", 5000, WithIdleReadTimeout(121*time.Second))
		require.Error(err)
		require.EqualError(err, "idle read timeout out of range [1, 120]")

		err = WithIdleReadTimeout(5 * time.Second).apply(nil)
		require.Error(err)
		require.ErrorIs(hsms.ErrConnConfigNil, err)
	})

	t.Run("Default LinktestFailThreshold", func(t *testing.T) {
		cfg, err := NewConnectionConfig("192.168.1.1", 5000)
		require.NoError(err)
		require.Equal(1, cfg.LinktestFailThreshold())
	})

	t.Run("Valid LinktestFailThreshold", func(t *testing.T) {
		cfg, err := NewConnectionConfig("192.168.1.1", 5000, WithLinktestFailThreshold(3))
		require.NoError(err)
		require.Equal(3, cfg.LinktestFailThreshold())
	})

	t.Run("Invalid LinktestFailThreshold", func(t *testing.T) {
		_, err := NewConnectionConfig("192.168.1.1", 5000, WithLinktestFailThreshold(0))
		require.Error(err)
		require.EqualError(err, "linktest fail threshold must be at least 1")

		_, err = NewConnectionConfig("192.168.1.1", 5000, WithLinktestFailThreshold(-1))
		require.Error(err)
		require.EqualError(err, "linktest fail threshold must be at least 1")

		err = WithLinktestFailThreshold(1).apply(nil)
		require.Error(err)
		require.ErrorIs(hsms.ErrConnConfigNil, err)
	})
}
