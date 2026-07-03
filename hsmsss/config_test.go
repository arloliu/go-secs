package hsmsss_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/hsmsss"
)

// TestNewConfig_Defaults verifies that NewConfig produces a Config whose
// embedded hsms.ConnectionConfig carries the E37-recommended defaults and
// that the default role is active.
func TestNewConfig_Defaults(t *testing.T) {
	cfg, err := hsmsss.NewConfig("127.0.0.1", 5000)
	if err != nil {
		t.Fatalf("NewConfig returned unexpected error: %v", err)
	}

	// Verify embedded shared defaults via the promoted Timers()/SessionID().
	defaults := hsms.DefaultConnectionConfig()

	timers := cfg.Timers()
	wantTimers := defaults.Timers()

	if timers != wantTimers {
		t.Errorf("embedded timers do not match defaults:\n got  %+v\nwant %+v", timers, wantTimers)
	}

	if cfg.SessionID() != defaults.SessionID() {
		t.Errorf("SessionID() = 0x%04X, want 0x%04X", cfg.SessionID(), defaults.SessionID())
	}

	// T3 and T6 must be > 0 (sanity that they are populated, not zero).
	if timers.T3 <= 0 {
		t.Errorf("T3 must be > 0, got %v", timers.T3)
	}

	if timers.T6 <= 0 {
		t.Errorf("T6 must be > 0, got %v", timers.T6)
	}

	// Session ID must be 0xFFFF (E37.1 single-session default).
	if cfg.SessionID() != 0xFFFF {
		t.Errorf("SessionID() = 0x%04X, want 0xFFFF", cfg.SessionID())
	}
}

// TestNewConfig_DefaultRoleIsActive verifies that the default role is active.
func TestNewConfig_DefaultRoleIsActive(t *testing.T) {
	cfg, err := hsmsss.NewConfig("127.0.0.1", 5000)
	if err != nil {
		t.Fatalf("NewConfig returned unexpected error: %v", err)
	}

	if !cfg.Active() {
		t.Error("expected default role to be active, got passive")
	}
}

// TestNewConfig_WithPassive verifies that WithPassive sets the passive role.
func TestNewConfig_WithPassive(t *testing.T) {
	cfg, err := hsmsss.NewConfig("127.0.0.1", 5000, hsmsss.WithPassive())
	if err != nil {
		t.Fatalf("NewConfig returned unexpected error: %v", err)
	}

	if cfg.Active() {
		t.Error("expected passive role after WithPassive(), got active")
	}
}

// TestNewConfig_WithActive verifies that WithActive restores the active role
// when applied after WithPassive, demonstrating that active/passive are
// controlled by a single bool (last option wins — they are mutually exclusive
// by construction).
func TestNewConfig_WithActive(t *testing.T) {
	cfg, err := hsmsss.NewConfig("127.0.0.1", 5000, hsmsss.WithPassive(), hsmsss.WithActive())
	if err != nil {
		t.Fatalf("NewConfig returned unexpected error: %v", err)
	}

	if !cfg.Active() {
		t.Error("expected active role after WithPassive then WithActive, got passive")
	}
}

// TestNewConfig_HostPort verifies that host and port are stored correctly.
func TestNewConfig_HostPort(t *testing.T) {
	cfg, err := hsmsss.NewConfig("192.0.2.1", 7777)
	if err != nil {
		t.Fatalf("NewConfig returned unexpected error: %v", err)
	}

	if cfg.Host() != "192.0.2.1" {
		t.Errorf("Host() = %q, want %q", cfg.Host(), "192.0.2.1")
	}

	if cfg.Port() != 7777 {
		t.Errorf("Port() = %d, want %d", cfg.Port(), 7777)
	}
}

// TestNewConfig_TCPKeepAlive verifies WithTCPKeepAlive accepts valid durations
// and rejects negative ones.
func TestNewConfig_TCPKeepAlive(t *testing.T) {
	tests := []struct {
		name    string
		d       time.Duration
		wantErr bool
	}{
		{"zero (OS default)", 0, false},
		{"positive", 30 * time.Second, false},
		{"negative", -1 * time.Second, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := hsmsss.NewConfig("127.0.0.1", 5000, hsmsss.WithTCPKeepAlive(tt.d))
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error for negative TCPKeepAlive, got nil")
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if cfg.TCPKeepAlive() != tt.d {
				t.Errorf("TCPKeepAlive() = %v, want %v", cfg.TCPKeepAlive(), tt.d)
			}
		})
	}
}

// TestNewConfig_Transactional is the teeth-check for all-or-nothing behaviour:
// a mix of one valid and one invalid option must produce an error and return
// a zero Config (no partial mutation).
func TestNewConfig_Transactional(t *testing.T) {
	validOpt := hsmsss.WithTCPKeepAlive(5 * time.Second)
	invalidOpt := hsmsss.WithTCPKeepAlive(-1 * time.Second) // invalid: negative

	cfg, err := hsmsss.NewConfig("127.0.0.1", 5000, validOpt, invalidOpt)
	if err == nil {
		t.Fatal("expected error from mix of valid + invalid options, got nil")
	}

	// NewConfig returns Config{} (zero value) on error — verify no partial state. Config carries a
	// func field (the dialer), so it is no longer comparable with ==; DeepEqual against the zero
	// value keeps the full-struct teeth (nil func fields compare equal).
	if !reflect.DeepEqual(cfg, hsmsss.Config{}) {
		t.Errorf("expected zero Config on error, got non-zero: host=%q port=%d active=%v keepAlive=%v",
			cfg.Host(), cfg.Port(), cfg.Active(), cfg.TCPKeepAlive())
	}
}

// TestNewConfig_TransactionalSharedKnob is the teeth-check for the shared-knob
// path: a valid hsms.ConnOption + an invalid one must not commit either.
func TestNewConfig_TransactionalSharedKnob(t *testing.T) {
	validShared := hsmsss.WithConnectionOption(hsms.WithT3(10 * time.Second))
	invalidShared := hsmsss.WithConnectionOption(hsms.WithT3(-1 * time.Second)) // invalid: ≤ 0

	cfg, err := hsmsss.NewConfig("127.0.0.1", 5000, validShared, invalidShared)
	if err == nil {
		t.Fatal("expected error from mix of valid + invalid shared options, got nil")
	}

	if !reflect.DeepEqual(cfg, hsmsss.Config{}) {
		t.Errorf("expected zero Config on error, got non-zero: host=%q port=%d",
			cfg.Host(), cfg.Port())
	}
}

// TestNewConfig_WithConnectionOption verifies that shared knobs are applied
// correctly via the WithConnectionOption adapter.
func TestNewConfig_WithConnectionOption(t *testing.T) {
	cfg, err := hsmsss.NewConfig("127.0.0.1", 5000,
		hsmsss.WithConnectionOption(hsms.WithT3(30*time.Second)),
		hsmsss.WithConnectionOption(hsms.WithSessionID(0x0001)),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Timers().T3 != 30*time.Second {
		t.Errorf("T3 = %v, want %v", cfg.Timers().T3, 30*time.Second)
	}

	if cfg.SessionID() != 0x0001 {
		t.Errorf("SessionID() = 0x%04X, want 0x0001", cfg.SessionID())
	}
}

// TestConnection_SessionID verifies the endpoint SessionID accessor on a live hsmsss connection
// still reports the configured session ID after the SECS2Endpoint identity method was unified to
// SessionID.
func TestConnection_SessionID(t *testing.T) {
	cfg, err := hsmsss.NewConfig("127.0.0.1", 5000,
		hsmsss.WithConnectionOption(hsms.WithSessionID(0x0007)),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	conn, err := hsmsss.New(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if conn.SessionID() != 0x0007 {
		t.Errorf("conn.SessionID() = 0x%04X, want 0x0007", conn.SessionID())
	}
}

// TestApplyOptions_Transactional verifies that ApplyOptions also enforces
// all-or-nothing: a valid option mixed with an invalid one leaves the Config
// unchanged.
func TestApplyOptions_Transactional(t *testing.T) {
	cfg, err := hsmsss.NewConfig("127.0.0.1", 5000)
	if err != nil {
		t.Fatalf("NewConfig returned unexpected error: %v", err)
	}

	origKeepAlive := cfg.TCPKeepAlive() // 0 (OS default)

	err = cfg.ApplyOptions(
		hsmsss.WithTCPKeepAlive(10*time.Second), // valid
		hsmsss.WithTCPKeepAlive(-1*time.Second), // invalid
	)
	if err == nil {
		t.Fatal("expected error from mix of valid + invalid options in ApplyOptions, got nil")
	}

	// The Config must be unchanged after the failed apply.
	if cfg.TCPKeepAlive() != origKeepAlive {
		t.Errorf("TCPKeepAlive changed despite failed apply: got %v, want %v",
			cfg.TCPKeepAlive(), origKeepAlive)
	}
}
