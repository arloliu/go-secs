package queue

// sliceQueue implements the Queue interface using a slice.
type sliceQueue struct {
	items []any
}

// NewSliceQueue creates a new sliceQueue.
func NewSliceQueue(prealloc int) Queue {
	return &sliceQueue{items: make([]any, 0, prealloc)}
}

// Enqueue adds an item to the tail of the queue.
func (q *sliceQueue) Enqueue(item any) {
	q.items = append(q.items, item)
}

// Dequeue removes and returns the item at the head of the queue.
func (q *sliceQueue) Dequeue() any {
	if len(q.items) == 0 {
		return nil
	}
	item := q.items[0]
	q.items = q.items[1:]
	return item
}

// Peek returns the item at the head of the queue without removing it.
func (q *sliceQueue) Peek() any {
	if len(q.items) == 0 {
		return nil
	}
	return q.items[0]
}

// Reset resets the queue to an empty state.
func (q *sliceQueue) Reset() {
	q.items = q.items[:0] // Reslice to 0 length to reuse the underlying array
}

// IsEmpty returns true if the queue is empty, false otherwise.
func (q *sliceQueue) IsEmpty() bool {
	return len(q.items) == 0
}

// Length returns the number of items in the queue.
func (q *sliceQueue) Length() int {
	return len(q.items)
}
