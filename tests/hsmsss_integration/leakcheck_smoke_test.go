package hsmsssintegration

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/go-secs/hsms"
	"github.com/stretchr/testify/require"
)

// TestLeakcheck_SmokeOpenCloseSettles verifies that snapshotLeaks +
// assertSettled compile and produce the expected advisory output on a
// vanilla open/close cycle. This is the integration-layer counterpart of
// hsmsss.TestAssertCleanShutdown_HappyPath: it does not assert any hard
// invariant (the helper logs advisory warnings, not test failures per
// chaos plan §7 #7), but it exercises the helper end to end so any future
// regression in the helper itself surfaces under `go test ./tests/...`.
//
// The wrap pattern demonstrated here — snapshot before, assertSettled via
// t.Cleanup — is the template the broader chaos suite should adopt as P0.3
// / P1.x add new scenarios.
func TestLeakcheck_SmokeOpenCloseSettles(t *testing.T) {
	require := require.New(t)
	port := getFreePort(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	before := snapshotLeaks()
	t.Cleanup(func() { assertSettled(t, before) })

	passive := newEndpoint(t, ctx, port, true, false, nil, echoHandler)
	active := newEndpoint(t, ctx, port, false, true, nil)
	defer closeEndpoint(t, passive)
	defer closeEndpoint(t, active)

	require.NoError(passive.conn.Open(false))
	require.NoError(active.conn.Open(false))
	waitState(t, active, hsms.SelectedState)
	waitState(t, passive, hsms.SelectedState)

	// Brief idle period to confirm no spontaneous goroutine growth.
	time.Sleep(50 * time.Millisecond)

	require.NoError(active.conn.Close())
	require.NoError(passive.conn.Close())
}
