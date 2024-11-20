package queue

import (
	"sync/atomic"
	"unsafe"
)

// itemNode represents a node in the lock free queue.
type itemNode struct {
	value any
	next  unsafe.Pointer
}

// lockFreeQueue is a lock-free, concurrent queue implementation.
// It provides efficient and thread-safe operations for enqueuing, dequeuing, and peeking at items.
//
// It implements the Queue interface.
type lockFreeQueue struct {
	head   unsafe.Pointer
	tail   unsafe.Pointer
	length atomic.Int32
}

var _ Queue = (*lockFreeQueue)(nil)

// NewLockFreeQueue creates a new lockFreeQueue and returns it as a Queue interface.
//
// lockFreeQueue is a lock-free, concurrent queue implementation.
// It provides efficient and thread-safe operations for enqueuing, dequeuing, and peeking at items.
func NewLockFreeQueue() Queue {
	n := unsafe.Pointer(&itemNode{})
	return &lockFreeQueue{head: n, tail: n}
}

func (q *lockFreeQueue) Reset() {
	n := unsafe.Pointer(&itemNode{})
	q.head = n
	q.tail = n
	q.length.Store(0)
}

// Enqueue adds an item to the tail of the queue.
func (q *lockFreeQueue) Enqueue(item any) {
	n := &itemNode{value: item}
retry:
	tail := loadQueueItem(&q.tail)
	next := loadQueueItem(&tail.next)
	// Are tail and next consistent?
	if tail == loadQueueItem(&q.tail) {
		if next == nil {
			// Try to link node at the end of the linked list.
			if casQueueItem(&tail.next, next, n) { // enqueue is done.
				// Try to swing tail to the inserted node.
				casQueueItem(&q.tail, tail, n)
				q.length.Add(1)
				return
			}
		} else { // tail was not pointing to the last node
			// Try to swing tail to the next node.
			casQueueItem(&q.tail, tail, next)
		}
	}

	goto retry
}

// Dequeue removes and returns the item at the head of the queue.
// It returns nil if the queue is empty.
func (q *lockFreeQueue) Dequeue() any {
retry:
	head := loadQueueItem(&q.head)
	tail := loadQueueItem(&q.tail)
	next := loadQueueItem(&head.next)

	// Are head, tail, and next consistent?
	if head == loadQueueItem(&q.head) {
		// Is queue empty or tail falling behind?
		if head == tail {
			// Is queue empty?
			if next == nil {
				return nil
			}
			casQueueItem(&q.tail, tail, next) // tail is falling behind, try to advance it.
		} else {
			// Read value before CAS, otherwise another dequeue might free the next node.
			data := next.value
			if casQueueItem(&q.head, head, next) { // dequeue is done, return value.
				q.length.Add(-1)
				return data
			}
		}
	}

	goto retry
}

// Peek returns the item at the head of the queue without removing it.
// It returns nil if the queue is empty.
func (q *lockFreeQueue) Peek() any {
retry:
	head := loadQueueItem(&q.head)
	tail := loadQueueItem(&q.tail)
	next := loadQueueItem(&head.next)

	// Are head, tail, and next consistent?
	if head == loadQueueItem(&q.head) {
		// Is queue empty or tail falling behind?
		if head != tail {
			return next.value
		}

		// Is queue empty?
		if next == nil {
			return nil
		}
		casQueueItem(&q.tail, tail, next) // tail is falling behind, try to advance it.
	}

	goto retry
}

// IsEmpty returns true if the queue is empty, false otherwise.
func (q *lockFreeQueue) IsEmpty() bool {
	return q.length.Load() == 0
}

// Length returns the number of items in the queue.
func (q *lockFreeQueue) Length() int {
	return int(q.length.Load())
}

// loadQueueItem atomically loads a node from a given pointer.
func loadQueueItem(p *unsafe.Pointer) (n *itemNode) {
	return (*itemNode)(atomic.LoadPointer(p))
}

// casQueueItem performs an atomic compare-and-swap operation on a node pointer.
func casQueueItem(p *unsafe.Pointer, oldItem, newItem *itemNode) bool {
	return atomic.CompareAndSwapPointer(p, unsafe.Pointer(oldItem), unsafe.Pointer(newItem))
}
