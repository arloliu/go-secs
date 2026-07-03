package hsmsss

// close_race_test.go — NEW v2 regression for the historically-flaky close-timeout race (F2/F3;
// see the close-timeout-flake history: TestConnection_CloseTCPIdempotent needed -count=2000 to
// catch it). There is no direct v1 analogue to port, so this constructs an equivalent
// Open/Select/Close-under-contention loop on a real active+passive pair.
//
// Invariant: every Close() on a contended connection returns CLEAN (nil — no spurious
// ErrCloseTimeout) and BOUNDED (no hang/deadlock — teardown joins all per-generation tasks),
// leaving the connection quiesced (assertCleanShutdown). The two coupled F2/F3 defects manifested
// as Close hanging or spuriously timing out while a select/dial/re-listen task raced teardown.
//
// Two contention shapes alternate:
//   - even i: Open → wait Selected → Close (clean-path teardown after a live session; closing the
//     active drops the passive, which re-listens and races its own subsequent Close)
//   - odd  i: Open → Close mid-handshake (dial/select/accept in flight during teardown)
//
// Gate: run at -race -count=2000 (the F2/F3 teeth). A panic or leaked goroutine surfaces under
// -race; a hang trips the bound; a spurious timeout trips require.NoError.

import (
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/stretchr/testify/require"
)

func TestHSMS_CloseRace_BoundedCleanShutdown(t *testing.T) {
	const iterations = 6
	// Correct teardown on loopback completes in milliseconds; the default 10 s closeTimeout means a
	// clean Close never times out. This bound is well below that timeout, so a hang or a spurious
	// ErrCloseTimeout regression (which would return at ~closeTimeout) trips it, while normal
	// -race/-count scheduling jitter does not.
	const closeBound = 5 * time.Second

	passive, active := newEndpointPair(t)
	ctx := t.Context()

	t.Cleanup(func() {
		_ = active.conn.Close()
		_ = passive.conn.Close()
	})

	// closeBounded closes one connection and asserts it returned clean (nil — no spurious
	// ErrCloseTimeout) and bounded (below closeBound — no hang/deadlock), then that it quiesced.
	closeBounded := func(c hsms.Connection, iter int, label string) {
		t.Helper()
		start := time.Now()
		err := c.Close()
		elapsed := time.Since(start)
		require.NoErrorf(t, err, "iter %d: %s Close returned error", iter, label)
		require.Lessf(t, elapsed, closeBound, "iter %d: %s Close not bounded (%s)", iter, label, elapsed)
		assertCleanShutdown(t, c)
	}

	for i := range iterations {
		// Passive must be listening before the active dials.
		require.NoErrorf(t, passive.conn.Open(ctx, hsms.OpenBackground), "iter %d: passive open", i)
		require.NoErrorf(t, active.conn.Open(ctx, hsms.OpenBackground), "iter %d: active open", i)

		if i%2 == 0 {
			waitSelected(t, active)
			waitSelected(t, passive)
		}

		// Close the active first (drops the passive, which re-listens), then the passive. Each Close
		// must return clean+bounded and leave its connection quiesced before the next Open.
		closeBounded(active.conn, i, "active")
		closeBounded(passive.conn, i, "passive")
	}
}
