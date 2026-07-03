package secs1

// new_test.go — TDD coverage for the secs1.New consumer entry point (SP5b T0). New must build the
// shared hsms engine around the secs1 transport skeleton and return the app-facing hsms.Connection.
// T0 never Opens the connection, so the transport stubs are not exercised: these tests assert only
// that construction succeeds and that a freshly-built connection reports NotConnectedState.

import (
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/stretchr/testify/require"
)

// TestNew_ReturnsUsableConnection verifies New builds a non-nil hsms.Connection (nil error) for both
// roles and that a freshly-built, not-yet-opened connection reports NotConnectedState.
func TestNew_ReturnsUsableConnection(t *testing.T) {
	t.Parallel()

	activeCfg, err := NewConfig("127.0.0.1", 5000)
	require.NoError(t, err)
	active, err := New(activeCfg)
	require.NoError(t, err)
	require.NotNil(t, active)
	require.Equal(t, hsms.NotConnectedState, active.State(), "unopened connection is NotConnected")

	passiveCfg, err := NewConfig("127.0.0.1", 5000, WithPassive())
	require.NoError(t, err)
	passive, err := New(passiveCfg)
	require.NoError(t, err)
	require.NotNil(t, passive)
	require.Equal(t, hsms.NotConnectedState, passive.State(), "unopened connection is NotConnected")
}

// TestUpdateConfigOptions_SessionIDStaysDeviceID proves the SECS-I identity invariant survives a
// live reconfiguration. NewConfig forces the core session ID to the device ID; a later
// UpdateConfigOptions(hsms.WithSessionID(...)) must NOT override it, so SessionID() keeps reporting
// the wire device ID that the SECS-I block header carries. An unrelated live update must likewise
// preserve the device identity.
func TestUpdateConfigOptions_SessionIDStaysDeviceID(t *testing.T) {
	t.Parallel()

	const deviceID uint16 = 0x1234
	cfg, err := NewConfig("127.0.0.1", 5000, WithDeviceID(deviceID))
	require.NoError(t, err)
	conn, err := New(cfg)
	require.NoError(t, err)
	require.Equal(t, deviceID, conn.SessionID(), "construction seeds SessionID from the device ID")

	// A caller attempting to re-arm the HSMS session ID at runtime must not divert the SECS-I
	// identity away from the device ID.
	require.NoError(t, conn.UpdateConfigOptions(hsms.WithSessionID(0xFFFF)))
	require.Equal(t, deviceID, conn.SessionID(), "live WithSessionID must not override the SECS-I device ID")

	// An unrelated live update still applies and continues to preserve the device identity.
	require.NoError(t, conn.UpdateConfigOptions(hsms.WithT3(30*time.Second)))
	require.Equal(t, deviceID, conn.SessionID(), "unrelated live update preserves the device ID")
}
