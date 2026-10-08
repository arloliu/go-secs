package tracepack

import (
	"context"
)

// captureStream is one capture of a read over several packs in capture order:
// its records in ascending seq, resolved through one cluster at a time,
// with a queue of the candidates of the seq last resolved
// (the tracepack semantics specification §7.4).
type captureStream struct {
	// rank is the capture's index among the plan's captures, which are in ascending capture_id.
	rank int
	pc   *planCapture
	// nextCluster is the index into pc.clusters of the next cluster to resolve.
	nextCluster int
	// cr resolves the current cluster; nil when no cluster is being resolved.
	cr *clusterResolver
	// queue holds the candidates of the selected versions of the seq last resolved, in version order.
	// Those from head on are pending, each owning its representative's block; queue[head] is the stream's head.
	queue []candidate
	head  int
}

// streamHeap holds the streams of a read in capture order that have a head,
// as a binary min-heap by the ts_min of their head's representative block, then by capture_id.
type streamHeap []*captureStream

// runCapture yields the records of the read of q that p planned to fn, in capture order
// (the tracepack semantics specification §7.4).
//
// Each capture is a stream over its clusters not excluded, in ascending first seq, one cluster resolved at a time.
// The selected versions of the seq it resolved last are queued in version order, the first its head;
// once the queue is empty it resolves its next seqs until one gives a candidate.
// The streams are started in ascending capture_id, each resolving up to its first head.
// Then the stream whose head's representative block has the smallest ts_min, ties broken by capture_id,
// yields its head and is scheduled again by its next head, until no stream has one.
// fn receives one Item, reused, valid during the call; its payload and header extension alias the representative's block.
//
// Returns:
//   - error: fn's error, as is; ctx's error, wrapped;
//     an error wrapping ErrReadLimit when a block's reservation would exceed MaxHeldBytes
//     or one more conflict would exceed MaxConflicts;
//     the error of the conflict reservation, wrapped, when it fails;
//     a ReadAt error, wrapped with the pack and block index, joined with the error of the retention check after it;
//     an error wrapping ErrRemoved when a covered hour is removed;
//     q.Retention's error, wrapped.
//     p.res holds the defects and conflicts found until then.
//     Nothing stays held after runCapture, whatever its outcome.
func (p *mergeIteratePlan) runCapture(ctx context.Context, q *Query, fn func(*Item) error) error {
	run := newMergeIterateRun(p, q)
	streams := make([]captureStream, len(p.captures))
	defer func() {
		for i := range streams {
			streams[i].close(&p.loader)
		}
	}()

	h := make(streamHeap, 0, len(streams))
	for k := range p.captures {
		s := &streams[k]
		s.rank, s.pc = k, &p.captures[k]
		ok, err := s.fill(ctx, run)
		if err != nil {
			return err
		}
		if ok {
			h.push(s)
		}
	}

	var it Item
	for len(h) > 0 {
		s := h[0]
		c := &s.queue[s.head]
		if err := run.yield(ctx, c, &it, fn); err != nil {
			return err
		}
		c.release(&p.loader)
		s.head++
		if s.head == len(s.queue) {
			ok, err := s.fill(ctx, run)
			if err != nil {
				return err
			}
			if !ok {
				h.pop()

				continue
			}
		}
		h.down(0)
	}

	return run.finish(ctx)
}

// fill resolves the stream's next seqs until one gives candidates, which it queues, the first the stream's head.
// The stream's queue holds no pending candidate when fill is called:
// the caller released each before, so fill reuses the queue and close, after an error of fill, releases nothing twice.
// It takes the stream's clusters in order, skipping those excluded,
// and closes each cluster's resolver once the cluster is resolved.
//
// Returns:
//   - bool: whether the stream has a head; false once its clusters are all resolved.
//   - error: the resolver's error (clusterResolver.next); the stream's resolver is then left for close.
func (s *captureStream) fill(ctx context.Context, run *mergeIterateRun) (bool, error) {
	s.queue, s.head = s.queue[:0], 0
	for {
		if s.cr == nil {
			for s.nextCluster < len(s.pc.clusters) && s.pc.clusters[s.nextCluster].excluded {
				s.nextCluster++
			}
			if s.nextCluster == len(s.pc.clusters) {
				return false, nil
			}
			s.cr = newClusterResolver(run, s.pc.id, &s.pc.clusters[s.nextCluster])
			s.nextCluster++
		}

		var ok bool
		var err error
		s.queue, ok, err = s.cr.next(ctx, s.queue)
		if err != nil {
			return false, err
		}
		if !ok {
			s.cr.close()
			s.cr = nil

			continue
		}
		if len(s.queue) > 0 {
			return true, nil
		}
	}
}

// close releases, through l, the stream's pending candidates, and closes its resolver.
// It is called on every exit from the read, and does nothing for a stream with nothing pending nor resolving,
// the zero captureStream included.
func (s *captureStream) close(l *heldLoader) {
	for i := s.head; i < len(s.queue); i++ {
		s.queue[i].release(l)
	}
	s.queue, s.head = s.queue[:0], 0
	if s.cr != nil {
		s.cr.close()
		s.cr = nil
	}
}

// less reports whether the stream at i comes before the one at j:
// its head's representative block has a smaller ts_min, or an equal one with a smaller capture_id.
func (h streamHeap) less(i, j int) bool {
	a, b := &h[i].queue[h[i].head], &h[j].queue[h[j].head]

	return a.blockTSMin < b.blockTSMin || (a.blockTSMin == b.blockTSMin && h[i].rank < h[j].rank)
}

// push adds s, which has a head, to the heap.
func (h *streamHeap) push(s *captureStream) {
	*h = append(*h, s)
	q := *h
	for i := len(q) - 1; i > 0; {
		parent := (i - 1) / 2
		if !q.less(i, parent) {
			break
		}
		q[i], q[parent] = q[parent], q[i]
		i = parent
	}
}

// pop removes the heap's first stream; the heap is not empty.
func (h *streamHeap) pop() {
	q := *h
	n := len(q) - 1
	q[0], q[n] = q[n], nil
	*h = q[:n]
	h.down(0)
}

// down moves the stream at i down the heap to its place.
// For the first stream, it restores the heap after any change of that stream's head.
func (h streamHeap) down(i int) {
	n := len(h)
	for {
		c := 2*i + 1
		if c >= n {
			return
		}
		if r := c + 1; r < n && h.less(r, c) {
			c = r
		}
		if !h.less(c, i) {
			return
		}
		h[i], h[c] = h[c], h[i]
		i = c
	}
}
