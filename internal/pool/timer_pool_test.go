package pool

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestTimerPool(t *testing.T) {
	assert := assert.New(t)

	t.Run("Get and Put", func(t *testing.T) {
		timer1 := GetTimer(1 * time.Second)
		assert.NotNil(timer1)

		PutTimer(timer1)

		timer2 := GetTimer(2 * time.Second)
		assert.NotNil(timer2)
		// Since timerPool is a sync.Pool, we can't guarantee that timer2 is the same as timer1

		<-timer2.C // Wait for the timer to expire
	})

	t.Run("Stop Active Timer", func(t *testing.T) {
		timer1 := GetTimer(1000 * time.Millisecond)
		assert.NotNil(timer1)

		time.Sleep(50 * time.Millisecond) // Make timer1 active
		assert.True(timer1.Stop())        // stop timer1

		timer2 := GetTimer(500 * time.Millisecond)
		assert.NotNil(timer2)

		assert.NotSame(timer1, timer2)

		select {
		case <-timer1.C:
			t.Error("timer1 should stopped and not fire")
		case <-timer2.C:
			// timer2 should fire
		}
	})

	t.Run("Put Active Timer", func(t *testing.T) {
		timer1 := GetTimer(100 * time.Millisecond)
		assert.NotNil(timer1)

		time.Sleep(50 * time.Millisecond) // Make timer1 active

		PutTimer(timer1) // Put the active timer back into the pool

		begin := time.Now()
		timer2 := GetTimer(300 * time.Millisecond)
		assert.NotNil(t, timer2)

		select {
		case tt := <-timer2.C: // timer2 should fire after 300ms
			if tt.Sub(begin) < 200*time.Millisecond {
				t.Error("timer2 should fire after roughly 300ms, fired too early")
			}
		case <-time.After(500 * time.Millisecond):
			t.Error("timer2 should have fired within 500ms")
		}
	})

	// PutTimer owns leaving a timer safe to re-arm.
	// This asserts that on the exact object handed to PutTimer,
	// so it holds whether or not sync.Pool chooses to retain it.
	// The timer is left unread, which is the only case where there can be anything to clean up.
	t.Run("PutTimer Leaves No Pending Tick On An Unread Timer", func(t *testing.T) {
		timer := time.NewTimer(time.Millisecond)

		// Injecting the delay is the scenario: the timer expires while nobody reads it.
		time.Sleep(20 * time.Millisecond)

		PutTimer(timer)

		select {
		case <-timer.C:
			t.Error("PutTimer pooled a timer that still had a tick waiting")
		default:
		}
	})

	// GetTimer must hand back an armed timer, which is what re-arming a pooled timer buys.
	//
	// A single round trip proves nothing: sync.Pool guarantees no retention, and under -race it
	// deliberately drops a quarter of the values handed to Put, so one Put/Get pair may never
	// reach the recycling path at all.
	// Repeating the round trip makes at least one pool hit a certainty in practice, while every
	// iteration asserts the same contract, so a GetTimer that stopped re-arming pooled timers
	// cannot slip through on a lucky miss.
	//
	// Stop is the probe because it answers the question directly and without waiting: it reports
	// true for a timer that is still running, and false for the expired, drained timer that a
	// re-arm-less GetTimer would return.
	t.Run("GetTimer Arms The Timer It Returns", func(t *testing.T) {
		const rounds = 64

		for range rounds {
			expired := time.NewTimer(time.Millisecond)
			<-expired.C // fired and drained, exactly the state PutTimer pools

			PutTimer(expired)

			got := GetTimer(time.Hour)
			assert.NotNil(got)
			assert.True(got.Stop(), "GetTimer must return an armed timer, not the stopped one it pooled")
		}
	})

	t.Run("Concurrency", func(t *testing.T) {
		var wg sync.WaitGroup
		for range 100 {
			wg.Go(func() {
				timer := GetTimer(10 * time.Millisecond)
				defer PutTimer(timer)
				<-timer.C
			})
		}
		wg.Wait()
	})
}
