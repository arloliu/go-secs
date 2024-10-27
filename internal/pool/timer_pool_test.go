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
			if tt.Sub(begin) < 270*time.Millisecond {
				t.Error("timer2 should fire after 300ms")
			}
		case <-time.After(330 * time.Millisecond):
			t.Error("timer2 should have fired within 330ms")
		}
	})

	t.Run("Concurrency", func(t *testing.T) {
		var wg sync.WaitGroup
		for i := 0; i < 100; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				timer := GetTimer(10 * time.Millisecond)
				defer PutTimer(timer)
				<-timer.C
			}()
		}
		wg.Wait()
	})
}
