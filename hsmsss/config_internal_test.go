package hsmsss

// config_internal_test.go — coverage for the WithConnectTimeout option and its wiring through the
// active dial site. The file is in package hsmsss (not hsmsss_test) so it can read the unexported
// cfg.connectTimeout field and construct the unexported *transport to exercise startActive directly.

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestWithConnectTimeout verifies the option stores a non-negative duration and rejects a
// negative one as a configuration error.
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

	if cfg.connectTimeout != 0 {
		t.Errorf("connectTimeout = %v, want 0 (unbounded) by default", cfg.connectTimeout)
	}
}

// TestStartActive_ConnectTimeoutBoundsDial verifies WithConnectTimeout wraps the dial site with a
// per-attempt deadline: a dialer that blocks until its ctx is done (simulating a black-holed peer
// that never completes the TCP handshake) is forced to return within the configured timeout rather
// than riding out the OS connect timeout (~2 min) or hanging on the unbounded generation ctx.
func TestStartActive_ConnectTimeoutBoundsDial(t *testing.T) {
	t.Parallel()

	// slowDial never returns on its own; it only unblocks when its ctx is cancelled/expires. If
	// WithConnectTimeout did not wrap the dial ctx with a deadline, this call would hang on the
	// unbounded background ctx passed to Start, and the test would time out instead of failing fast.
	slowDial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()

		return nil, ctx.Err()
	}

	cfg, err := NewConfig("127.0.0.1", 5000,
		WithActive(),
		WithDialer(slowDial),
		WithConnectTimeout(50*time.Millisecond),
	)
	require.NoError(t, err)

	tr := newTransport(cfg)

	done := make(chan error, 1)
	go func() {
		done <- tr.Start(context.Background(), nil)
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Start error = %v, want wrapping context.DeadlineExceeded", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Start did not return within 500ms — connectTimeout was not honored at the dial site")
	}
}
