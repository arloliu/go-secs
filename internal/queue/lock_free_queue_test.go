package queue

import (
	"context"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

type msgItem struct {
	Data string
}

func TestLockFreeQueue(t *testing.T) {
	assert := assert.New(t)
	t.Run("Empty Queue", func(t *testing.T) {
		q := NewLockFreeQueue()

		assert.True(q.IsEmpty())
		assert.Equal(0, q.Length())
		assert.Nil(q.Dequeue())
		assert.Nil(q.Peek())
	})

	t.Run("Enqueue and Dequeue", func(t *testing.T) {
		q := NewLockFreeQueue()

		item1 := &msgItem{"data1"}
		q.Enqueue(item1)
		assert.False(q.IsEmpty())
		assert.Equal(1, q.Length())

		item2 := &msgItem{"data2"}
		q.Enqueue(item2)
		assert.Equal(2, q.Length())

		dequeuedItem1 := q.Dequeue()
		assert.Equal(item1, dequeuedItem1)
		assert.Equal(1, q.Length())

		dequeuedItem2 := q.Dequeue()
		assert.Equal(item2, dequeuedItem2)
		assert.True(q.IsEmpty())

		dequeuedItem3 := q.Dequeue()
		assert.Nil(dequeuedItem3)
		assert.True(q.IsEmpty())
	})

	t.Run("Peek", func(t *testing.T) {
		q := NewLockFreeQueue()

		item1 := &msgItem{"data1"}
		item2 := &msgItem{"data2"}
		q.Enqueue(item1)

		assert.Equal(item1, q.Peek())
		assert.Equal(1, q.Length()) // Length should not change after peek

		q.Enqueue(item2)

		assert.Equal(item1, q.Peek())
		assert.Equal(2, q.Length())

		q.Dequeue()
		assert.Equal(item2, q.Peek())
		assert.Equal(1, q.Length())

		q.Dequeue()
		assert.Nil(q.Peek())
		assert.Equal(0, q.Length())
	})

	t.Run("Concurrency", func(t *testing.T) {
		q := NewLockFreeQueue()

		var wg sync.WaitGroup
		for i := 0; i < 1000; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				q.Enqueue(&msgItem{strconv.Itoa(i)})
			}(i)
		}
		wg.Wait()

		assert.Equal(1000, q.Length())

		wg.Add(1000)
		for i := 0; i < 1000; i++ {
			go func() {
				defer wg.Done()
				q.Dequeue()
			}()
		}
		wg.Wait()

		assert.True(q.IsEmpty())
	})
}

func BenchmarkLockFreeQueue_100(b *testing.B) {
	benchLockFreeQueue(b, 100)
}

func BenchmarkChannelUnbuffered_100(b *testing.B) {
	benchChannel(b, 100, false)
}

func BenchmarkChannelBuffered_100(b *testing.B) {
	benchChannel(b, 100, true)
}

func benchLockFreeQueue(b *testing.B, iterCount int) {
	ctx := context.Background()
	q := NewLockFreeQueue()

	// warm up queue
	for i := 0; i < iterCount; i++ {
		q.Enqueue(i)
	}

	for i := 0; i < iterCount; i++ {
		_ = q.Dequeue()
	}

	b.ResetTimer()
	for i := 0; i <= b.N; i++ {
		stopCh := make(chan struct{})
		go func(ctx context.Context, q Queue) {
			for {
				select {
				case <-ctx.Done():
					return
				default:
					item := q.Dequeue()
					if item == nil {
						break
					}
					if item.(int) == iterCount {
						close(stopCh)
						return
					}
				}
			}
		}(ctx, q)

		for i := 0; i < iterCount; i++ {
			q.Enqueue(i + 1)
		}
		<-stopCh
	}
	b.StopTimer()
}

func benchChannel(b *testing.B, iterCount int, buffered bool) {
	ctx := context.Background()
	var input chan int
	if buffered {
		input = make(chan int, iterCount)
	} else {
		input = make(chan int)
	}
	b.ResetTimer()
	for i := 0; i <= b.N; i++ {
		stopCh := make(chan struct{})
		go func(ctx context.Context, input chan int) {
			for {
				select {
				case <-ctx.Done():
					return
				case data := <-input:
					if data == iterCount {
						close(stopCh)
						return
					}
				}
			}
		}(ctx, input)

		for i := 0; i < iterCount; i++ {
			input <- (i + 1)
		}
		<-stopCh
	}
	b.StopTimer()
}
