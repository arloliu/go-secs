package hsms

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// isCleanShutdown returns true when c has reached a fully-quiesced state: no in-flight data
// messages, the current generation's done channel closed (torn down), no live per-generation
// task goroutines, and an empty per-generation reply registry (spec §8 / territory map §6.7,
// re-derived for the v2 fields). A never-opened connection (cur == nil) is trivially clean.
//
// The predicate is extracted so the TEETH test can assert failure deterministically without
// self-failing — assertCleanShutdown wraps it in require.Eventually; the teeth-test calls it
// directly.
func isCleanShutdown(c *connection) bool {
	if c.Metrics().DataMsgInflightCount() != 0 {
		return false
	}

	e := c.cur.Load()
	if e == nil {
		return true // never opened — trivially clean
	}

	select {
	case <-e.done: // generation torn down
	default:
		return false
	}

	return e.liveTasks.Load() == 0 && e.replies.len() == 0
}

// assertCleanShutdown hard-fails unless c has reached a fully-quiesced state: no in-flight
// data, the current generation torn down (done closed), no live per-generation task goroutines,
// and an empty per-generation reply registry (spec §8 / territory map §6.7, re-derived for the
// v2 fields). It polls (require.Eventually) so a teardown still winding down converges rather
// than flaking. A never-opened connection (cur == nil) is trivially clean.
func assertCleanShutdown(t *testing.T, c *connection) {
	t.Helper()

	require.Eventually(t, func() bool {
		return isCleanShutdown(c)
	}, 2*time.Second, 5*time.Millisecond,
		"connection did not reach clean shutdown (inflight/live-tasks/replies/generation)")
}

// ── Happy-path tests ────────────────────────────────────────────────────────────

// TestCleanShutdown_NeverOpened: a connection that was never opened (cur == nil) is trivially
// clean — the gate must pass immediately without blocking.
func TestCleanShutdown_NeverOpened(t *testing.T) {
	_, c := newTestConn(t)
	require.Nil(t, c.cur.Load(), "pre-condition: cur must be nil before Open")
	assertCleanShutdown(t, c)
}

// TestCleanShutdown_AfterOpenClose: a connection that has completed Open→Selected→Close must
// pass the gate: generation torn down (done closed), zero live tasks, empty reply registry.
func TestCleanShutdown_AfterOpenClose(t *testing.T) {
	c, _ := newLifeConn(t, withMockTransport())
	require.NoError(t, c.Open(t.Context(), OpenBackground))
	requireSelected(t, c)
	require.NoError(t, c.Close())

	assertCleanShutdown(t, c)
}

// ── TEETH test ──────────────────────────────────────────────────────────────────

// TestCleanShutdown_Teeth verifies that the gate is not vacuous: it returns false when the epoch
// carries a leaked reply entry after Close, and true only after the entry is cleaned up.
//
// Approach: Open→Selected, register a system-bytes key on the epoch's reply registry without
// deregistering (simulating a W-bit sender that leaked without deregister), then Close. After
// Close the epoch is torn down (done closed, liveTasks == 0) but replies.len() == 1, so
// isCleanShutdown must return false. After deregister it returns true.
//
// The predicate (isCleanShutdown) is called directly so the enclosing test does not self-fail —
// a require.Eventually timeout would make the test itself fail, defeating the teeth-check.
func TestCleanShutdown_Teeth(t *testing.T) {
	c, _ := newLifeConn(t, withMockTransport())
	require.NoError(t, c.Open(t.Context(), OpenBackground))
	requireSelected(t, c)

	e := c.cur.Load()
	require.NotNil(t, e, "pre-condition: cur must be non-nil after Open")

	// Simulate a W-bit sender that registered but never deregistered (leaked reply entry).
	leakedKey := [4]byte{9, 9, 9, 9}
	_ = e.replies.register(leakedKey)

	// Close tears down the generation (done closes, liveTasks → 0), but the reply registry
	// is NOT cleared by teardown — the leaked entry persists.
	require.NoError(t, c.Close())

	// Wait for done to close (Close blocks on wait(), so this is guaranteed; belt-and-suspenders).
	select {
	case <-e.done:
	case <-time.After(2 * time.Second):
		t.Fatal("epoch done did not close within 2s after Close")
	}

	// TEETH: with the leaked reply entry, the predicate must return false.
	require.False(t, isCleanShutdown(c),
		"TEETH: gate predicate must return false when replies.len() != 0 after teardown")

	// After deregistering the leaked entry, all conditions pass — predicate must return true.
	e.replies.deregister(leakedKey)
	require.True(t, isCleanShutdown(c),
		"TEETH: gate predicate must return true after the leaked reply entry is cleaned up")
}
