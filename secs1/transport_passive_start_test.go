package secs1

// Coverage for startPassive's two error paths: the listen-failure early return,
// and the I1 stopping guard firing after a successful listen.
// Both are driven deterministically through a WithListener func, with no sleep and no real race.

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// closeTrackingListener records whether Close was called.
// Asserting the flag fails fast when the production close is removed,
// where asserting through Accept would block until the test suite times out.
type closeTrackingListener struct {
	net.Listener
	closed atomic.Bool
}

func (l *closeTrackingListener) Close() error {
	l.closed.Store(true)

	return l.Listener.Close()
}

// TestStartPassive_ListenFailureWrapsError verifies that when the configured ListenFunc (WithListener)
// fails, startPassive calls engineCancel and returns a wrapped error rather than storing a listener or
// spawning the accept goroutine.
// Reached via tr.Start (the sole entry point into startPassive for a passive-role transport, mirrored
// from TestStartActive_ConnectTimeoutBoundsDial's tr.Start usage).
func TestStartPassive_ListenFailureWrapsError(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("boom: bind refused")
	listen := func(_ context.Context, _, _ string) (net.Listener, error) {
		return nil, errBoom
	}

	cfg, err := NewConfig("127.0.0.1", 5000, WithPassive(), WithListener(listen))
	require.NoError(t, err)

	tr := newTransport(cfg)
	tr.ArmStart()

	err = tr.Start(t.Context(), newMockRuntime())
	require.Error(t, err)
	require.ErrorIs(t, err, errBoom, "startPassive must wrap the listen error rather than replacing it")
	require.Contains(t, err.Error(), "secs1: listen 127.0.0.1:5000:")

	// engineCancel fires on this path but is never published, because the store happens only after
	// a successful listen.
	// Both fields staying nil is what confirms the branch returned before reaching those stores.
	require.Nil(t, tr.engineCancel, "engineCancel must not be stored when listen fails before publish")
	require.Nil(t, tr.listener, "listener must not be stored when listen fails")
}

// TestStartPassive_StoppingGuardAfterListenClosesListenerAndReturnsErrStartSealed drives the I1
// Add-vs-Wait guard deterministically.
// The injected ListenFunc seals the transport under startGate exactly as Stop does,
// then hands back a real listener, so the stopping check fires after a successful listen.
// That is the guard this covers, distinct from the pre-listen failure path above.
// This proves the guard closes the just-created listener and returns errStartSealed without a Sleep
// or a real race window.
func TestStartPassive_StoppingGuardAfterListenClosesListenerAndReturnsErrStartSealed(t *testing.T) {
	t.Parallel()

	fakeLn := &closeTrackingListener{Listener: newPipeListener()}
	t.Cleanup(func() { _ = fakeLn.Close() })

	var tr *transport // assigned below; captured by the closure, read only once listen() runs (after Start calls it)
	listen := func(_ context.Context, _, _ string) (net.Listener, error) {
		// Simulate a Stop racing in between listener creation and the I1 guard check: seal the
		// transport under the SAME lock discipline Stop uses (t.startGate.Lock / t.stopping = true),
		// then hand back a real listener so the guard fires POST-listen.
		tr.startGate.Lock()
		tr.stopping = true
		tr.startGate.Unlock()

		return fakeLn, nil
	}

	cfg, err := NewConfig("127.0.0.1", 5000, WithPassive(), WithListener(listen))
	require.NoError(t, err)

	tr = newTransport(cfg)
	tr.ArmStart()

	err = tr.Start(t.Context(), newMockRuntime())
	require.ErrorIs(t, err, errStartSealed)

	require.Nil(t, tr.listener, "the guard must clear t.listener after closing it")

	require.True(t, fakeLn.closed.Load(), "the guard must have closed the just-created listener")
}
