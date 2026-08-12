package pool

import (
	"sync"
	"time"
)

var timerPool sync.Pool

// GetTimer returns a timer for the given duration d from the pool.
//
// Return back the timer to the pool with Put.
func GetTimer(d time.Duration) *time.Timer {
	if v := timerPool.Get(); v != nil {
		t, _ := v.(*time.Timer) // Type assertion is safe here since we only put *time.Timer into the pool

		// A timer only ever reaches the pool through PutTimer,
		// which stops it and drains a late tick before handing it to timerPool.Put.
		// A pooled timer is therefore already inactive,
		// so Reset cannot report that it was still running and its result is ignored.
		t.Reset(d)

		return t
	}

	return time.NewTimer(d)
}

// PutTimer returns timer to the pool.
//
// t cannot be accessed after returning to the pool.
func PutTimer(t *time.Timer) {
	if !t.Stop() {
		// Drain t.C if it wasn't obtained by the caller yet.
		select {
		case <-t.C:
		default:
		}
	}
	timerPool.Put(t)
}
