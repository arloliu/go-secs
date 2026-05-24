package hsmsssintegration

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// leakSnapshot captures coarse resource gauges at a single point in time.
// It is intentionally cheap — runtime.NumGoroutine and an opendir-of-fd
// (Linux only) — so tests can take a snapshot before any setup and compare
// after teardown without measurably perturbing the schedule.
type leakSnapshot struct {
	goroutines int
	fds        int
	hasFds     bool
}

// leakOptions tunes the assertion tolerance.
//
//   - goroutineTolerance: maximum allowed delta over `before.goroutines`.
//     Default 2 (background runtime goroutines come and go).
//   - fdTolerance: maximum allowed delta over `before.fds`. Default 0
//     (every fd opened during the test should be closed by teardown).
//   - pollEvery / pollFor: how often / how long to poll before declaring
//     a leak. Defaults to 200ms × 5 (= 1s total) per chaos plan §3 P0.4.
type leakOptions struct {
	goroutineTolerance int
	fdTolerance        int
	pollEvery          time.Duration
	pollFor            time.Duration
}

func defaultLeakOptions() leakOptions {
	return leakOptions{
		goroutineTolerance: 2,
		fdTolerance:        0,
		pollEvery:          200 * time.Millisecond,
		pollFor:            1 * time.Second,
	}
}

// snapshotLeaks captures the current resource gauges. Pair with assertSettled
// at the end of the test (typically via t.Cleanup so the check fires after
// every defer-Close has run).
func snapshotLeaks() leakSnapshot {
	s := leakSnapshot{goroutines: runtime.NumGoroutine()}
	if runtime.GOOS == "linux" {
		if n, ok := readOpenFDCount(); ok {
			s.fds = n
			s.hasFds = true
		}
	}

	return s
}

// assertSettled polls the current resource gauges against the pre-test
// snapshot and surfaces any drift larger than the tolerance. Per chaos
// plan §7 #7 this is the **advisory** layer: failures log a warning via
// t.Logf rather than failing the test, because background goroutines /
// fd churn are inherently noisy and chasing tolerance bickering here
// undermines the primary internal-counter gate in
// hsmsss.assertCleanShutdown.
//
// Tests that need a hard failure on leaks should call the package-internal
// assertCleanShutdown helper instead (only available in-package).
func assertSettled(t testing.TB, before leakSnapshot) {
	t.Helper()

	opts := defaultLeakOptions()

	deadline := time.Now().Add(opts.pollFor)
	var (
		now leakSnapshot
		ok  bool
	)

	for {
		now = snapshotLeaks()
		ok = checkSettled(before, now, opts)
		if ok {
			return
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(opts.pollEvery)
	}

	// Advisory failure — log, do not fail the test. The primary leak gate
	// (assertCleanShutdown) lives at the package-internal layer.
	t.Logf("[leakcheck] post-test resource drift exceeded tolerance: %s",
		formatLeakDelta(before, now, opts))
}

func checkSettled(before, now leakSnapshot, opts leakOptions) bool {
	if now.goroutines-before.goroutines > opts.goroutineTolerance {
		return false
	}
	if before.hasFds && now.hasFds &&
		now.fds-before.fds > opts.fdTolerance {
		return false
	}

	return true
}

func formatLeakDelta(before, now leakSnapshot, opts leakOptions) string {
	out := fmt.Sprintf(
		"goroutines before=%d now=%d delta=%+d (tolerance=%d)",
		before.goroutines, now.goroutines,
		now.goroutines-before.goroutines, opts.goroutineTolerance,
	)
	if before.hasFds && now.hasFds {
		out += fmt.Sprintf("; fds before=%d now=%d delta=%+d (tolerance=%d)",
			before.fds, now.fds,
			now.fds-before.fds, opts.fdTolerance,
		)
	}

	return out
}

// readOpenFDCount counts /proc/self/fd entries. Linux-only — the returned
// ok flag is false on other platforms or when /proc is not readable.
func readOpenFDCount() (int, bool) {
	entries, err := os.ReadDir(filepath.Join("/proc", "self", "fd"))
	if err != nil {
		return 0, false
	}

	return len(entries), true
}
