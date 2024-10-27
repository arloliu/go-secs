package queue

// Queue defines the interface for message queue.
type Queue interface {
	// Enqueue adds an item to the tail of the queue.
	Enqueue(any)
	// Dequeue removes and returns the item at the head of the queue.
	Dequeue() any
	// Peek returns the item at the head of the queue without removing it.
	Peek() any
	// Reset to an empty queue
	Reset()
	// IsEmpty returns true if the queue is empty, false otherwise.
	IsEmpty() bool
	// Length returns the number of items in the queue.
	Length() int
}
