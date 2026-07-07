package secs1

// config_test.go — TDD coverage for secs1.Config / NewConfig (SP5b T0). These tests assert the
// configuration surface only: functional-option validation, transactional all-or-nothing apply,
// the D5b-11 writeTimeout==0 invariant, shared-knob pass-through via WithConnectionOption, and the
// value-receiver accessors. No network and no time.Sleep — the transport seam is never opened here.

import (
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/stretchr/testify/require"
)

// TestNewConfig_Defaults verifies the SECS-I and embedded-core defaults a bare NewConfig produces.
func TestNewConfig_Defaults(t *testing.T) {
	t.Parallel()

	cfg, err := NewConfig("127.0.0.1", 5000)
	require.NoError(t, err)

	require.Equal(t, "127.0.0.1", cfg.Host())
	require.Equal(t, 5000, cfg.Port())
	require.True(t, cfg.Active(), "default role is active")
	require.Equal(t, 500*time.Millisecond, cfg.T1(), "default T1")
	require.Equal(t, 10*time.Second, cfg.T2(), "default T2")
	require.Equal(t, 45*time.Second, cfg.T4(), "default T4")
	require.Equal(t, 3, cfg.RetryLimit(), "default RetryLimit")
	require.False(t, cfg.IsEquip(), "default role is host (slave)")
	require.Equal(t, uint16(0), cfg.DeviceID(), "default deviceID")
}

// TestNewConfig_WriteTimeoutInvariant asserts the D5b-11 invariant: the embedded core writeTimeout
// is forced to 0 so the core write deadline can never preempt the SECS-I line transaction — even
// when the user explicitly re-arms it through WithConnectionOption (the forced-last override wins).
func TestNewConfig_WriteTimeoutInvariant(t *testing.T) {
	t.Parallel()

	cfg, err := NewConfig("127.0.0.1", 5000)
	require.NoError(t, err)
	require.Equal(t, time.Duration(0), cfg.WriteTimeout(), "writeTimeout must be forced to 0 (D5b-11)")

	// A user re-arming writeTimeout via the shared-knob wrapper must NOT survive: NewConfig forces
	// writeTimeout=0 as its last construction step.
	cfg2, err := NewConfig("127.0.0.1", 5000,
		WithConnectionOption(hsms.WithWriteTimeout(5*time.Second)),
	)
	require.NoError(t, err)
	require.Equal(t, time.Duration(0), cfg2.WriteTimeout(), "forced writeTimeout=0 overrides a user re-arm (D5b-11)")
}

// TestNewConfig_SharedKnobPassThrough asserts WithConnectionOption reaches the embedded
// hsms.ConnectionConfig: T3 keeps its 45s core default and is tunable via hsms.WithT3.
func TestNewConfig_SharedKnobPassThrough(t *testing.T) {
	t.Parallel()

	cfg, err := NewConfig("127.0.0.1", 5000)
	require.NoError(t, err)
	require.Equal(t, 45*time.Second, cfg.Timers().T3, "default core T3")

	cfg2, err := NewConfig("127.0.0.1", 5000,
		WithConnectionOption(hsms.WithT3(30*time.Second)),
	)
	require.NoError(t, err)
	require.Equal(t, 30*time.Second, cfg2.Timers().T3, "T3 tunable via WithConnectionOption")
}

// TestNewConfig_Accessors builds a fully-customised config and asserts every accessor returns the
// configured value (and that WithPassive flips the role).
func TestNewConfig_Accessors(t *testing.T) {
	t.Parallel()

	cfg, err := NewConfig("10.0.0.5", 6000,
		WithPassive(),
		WithEquipment(),
		WithDeviceID(0x1234),
		WithT1(200*time.Millisecond),
		WithT2(3*time.Second),
		WithT4(20*time.Second),
		WithT5(2*time.Second),
		WithRetryLimit(5),
		WithTCPKeepAlive(15*time.Second),
	)
	require.NoError(t, err)

	require.Equal(t, "10.0.0.5", cfg.Host())
	require.Equal(t, 6000, cfg.Port())
	require.False(t, cfg.Active(), "WithPassive sets role to passive")
	require.True(t, cfg.IsEquip())
	require.Equal(t, uint16(0x1234), cfg.DeviceID())
	require.Equal(t, 200*time.Millisecond, cfg.T1())
	require.Equal(t, 3*time.Second, cfg.T2())
	require.Equal(t, 20*time.Second, cfg.T4())
	require.Equal(t, 2*time.Second, cfg.Timers().T5)
	require.Equal(t, 5, cfg.RetryLimit())
	require.Equal(t, 15*time.Second, cfg.TCPKeepAlive())
}

// TestNewConfig_Validation covers every option's rejection path plus the transactional guarantee.
func TestNewConfig_Validation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opt  Option
	}{
		{"deviceID too large", WithDeviceID(0x8000)},
		{"retryLimit above 31", WithRetryLimit(32)},
		{"retryLimit negative", WithRetryLimit(-1)},
		{"T1 zero", WithT1(0)},
		{"T2 zero", WithT2(0)},
		{"T4 zero", WithT4(0)},
		{"T5 zero", WithT5(0)},
		{"T1 negative", WithT1(-1 * time.Second)},
		{"keepAlive negative", WithTCPKeepAlive(-1 * time.Second)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewConfig("127.0.0.1", 5000, tc.opt)
			require.Error(t, err, "invalid option must be rejected")
		})
	}

	// Boundary values that MUST be accepted.
	_, err := NewConfig("127.0.0.1", 5000, WithDeviceID(0x7FFF), WithRetryLimit(0))
	require.NoError(t, err, "deviceID=0x7FFF and retryLimit=0 are valid boundaries")

	_, err = NewConfig("127.0.0.1", 5000, WithRetryLimit(31), WithTCPKeepAlive(0))
	require.NoError(t, err, "retryLimit=31 and keepAlive=0 are valid boundaries")
}

// TestNewConfig_Transactional asserts all-or-nothing apply: a batch containing a bad option returns
// an error and yields no partially-mutated config (NewConfig returns the zero Config on failure).
func TestNewConfig_Transactional(t *testing.T) {
	t.Parallel()

	cfg, err := NewConfig("127.0.0.1", 5000,
		WithDeviceID(0x0011), // valid, would mutate
		WithRetryLimit(99),   // invalid → whole batch must fail
	)
	require.Error(t, err, "a bad option fails the whole batch")
	require.Equal(t, Config{}, cfg, "failed NewConfig returns the zero Config (no partial mutation)")
}

// TestWithEquipment_SetsRole verifies WithEquipment() sets the equipment (master) role.
func TestWithEquipment_SetsRole(t *testing.T) {
	t.Parallel()

	cfg, err := NewConfig("127.0.0.1", 5000, WithEquipment())
	require.NoError(t, err)
	require.True(t, cfg.IsEquip(), "WithEquipment must set the equipment role (isEquip=true)")
}

// TestWithHost_SetsRole verifies WithHost() sets the host (slave) role, which is also the default.
func TestWithHost_SetsRole(t *testing.T) {
	t.Parallel()

	cfg, err := NewConfig("127.0.0.1", 5000, WithHost())
	require.NoError(t, err)
	require.False(t, cfg.IsEquip(), "WithHost must set the host role (isEquip=false)")

	// Round-trip: Equipment then Host.
	cfg2, err := NewConfig("127.0.0.1", 5000, WithEquipment(), WithHost())
	require.NoError(t, err)
	require.False(t, cfg2.IsEquip(), "last option wins: WithHost overrides a preceding WithEquipment")
}

// TestNew_NilDialerGuard verifies that New rejects an active Config with a nil dialer (i.e. a
// hand-built Config not originating from NewConfig) with a clear error rather than a panic.
func TestNew_NilDialerGuard(t *testing.T) {
	t.Parallel()

	// Construct a minimal active Config without going through NewConfig so dial stays nil.
	cfg := Config{}
	cfg.active = true // unexported; accessible from within package secs1

	_, err := New(cfg)
	require.Error(t, err, "New with an active Config and nil dialer must return an error")
	require.Contains(t, err.Error(), "NewConfig", "error message must mention NewConfig")
}

// TestConfig_WithConnectionOption_T1T2T4 verifies that T1/T2/T4 timers can be set via the runtime
// WithConnectionOption wrapper form, not just build-time WithT1/WithT2/WithT4.
func TestConfig_WithConnectionOption_T1T2T4(t *testing.T) {
	t.Parallel()

	cfg, err := NewConfig("127.0.0.1", 5000,
		WithConnectionOption(hsms.WithT1(750*time.Millisecond)),
		WithConnectionOption(hsms.WithT2(15*time.Second)),
		WithConnectionOption(hsms.WithT4(60*time.Second)),
	)
	require.NoError(t, err)
	require.Equal(t, 750*time.Millisecond, cfg.T1())
	require.Equal(t, 15*time.Second, cfg.T2())
	require.Equal(t, 60*time.Second, cfg.T4())
}

// TestWithT5_BuildTimeAndConnectionOption verifies that T5 can be set both via the build-time
// secs1.WithT5 option and via the runtime WithConnectionOption(hsms.WithT5(...)) wrapper form,
// and that both spellings set the same shared timer.
func TestWithT5_BuildTimeAndConnectionOption(t *testing.T) {
	t.Parallel()

	cfg, err := NewConfig("127.0.0.1", 5000, WithT5(7*time.Second))
	require.NoError(t, err)
	require.Equal(t, 7*time.Second, cfg.Timers().T5, "secs1.WithT5 must set the shared T5 timer")

	cfg2, err := NewConfig("127.0.0.1", 5000, WithConnectionOption(hsms.WithT5(9*time.Second)))
	require.NoError(t, err)
	require.Equal(t, 9*time.Second, cfg2.Timers().T5, "the WithConnectionOption(hsms.WithT5(...)) wrapper form must keep working")
}

func TestWithEquipment_AlsoEnablesAutoS9F9(t *testing.T) {
	cfg, err := NewConfig("127.0.0.1", 5000, WithEquipment())
	require.NoError(t, err)
	require.True(t, cfg.IsEquip())
	require.True(t, cfg.AutoS9F9(), "WithEquipment must also enable the shared AutoS9F9 primitive")
}

func TestWithHost_AlsoDisablesAutoS9F9(t *testing.T) {
	cfg, err := NewConfig("127.0.0.1", 5000, WithEquipment(), WithHost())
	require.NoError(t, err)
	require.False(t, cfg.IsEquip())
	require.False(t, cfg.AutoS9F9(), "WithHost must also disable the shared AutoS9F9 primitive")
}

// TestWithConnectTimeout verifies the option stores a non-negative duration and rejects a negative
// one as a configuration error.
func TestWithConnectTimeout(t *testing.T) {
	t.Parallel()

	cfg, err := NewConfig("127.0.0.1", 5000)
	require.NoError(t, err)

	if err := WithConnectTimeout(3 * time.Second)(&cfg); err != nil {
		t.Fatalf("WithConnectTimeout(3s): unexpected error: %v", err)
	}

	if cfg.connectTimeout != 3*time.Second {
		t.Errorf("connectTimeout = %v, want 3s", cfg.connectTimeout)
	}
	require.Equal(t, 3*time.Second, cfg.ConnectTimeout(), "ConnectTimeout accessor must reflect the stored value")

	if err := WithConnectTimeout(-1)(&cfg); err == nil {
		t.Error("negative timeout must be rejected")
	}
}

// TestWithConnectTimeout_ZeroIsUnbounded verifies the default (zero value, never applying the
// option) leaves connectTimeout at zero — i.e. unbounded, today's behavior.
func TestWithConnectTimeout_ZeroIsUnbounded(t *testing.T) {
	t.Parallel()

	cfg, err := NewConfig("127.0.0.1", 5000)
	require.NoError(t, err)

	require.Zero(t, cfg.ConnectTimeout(), "connectTimeout must be 0 (unbounded) by default")
}
