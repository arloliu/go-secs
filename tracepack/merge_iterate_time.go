package tracepack

import (
	"cmp"
	"context"
	"fmt"
	"slices"
)

// timeUnit is one overlap cluster of a read in time order that the query does not exclude,
// with the rank of its capture: the capture's index among the plan's captures, which are in ascending capture_id.
type timeUnit struct {
	rank int
	c    *planCluster
}

// pendingCandidate is a candidate of a read in time order not yet yielded, with the rank of its record's capture.
type pendingCandidate struct {
	candidate
	rank int
}

// pendingHeap holds the pending candidates of a read in time order,
// as a binary min-heap by ts_utc_ns, then capture rank, then seq, then version.
type pendingHeap []pendingCandidate

// runTime yields the records of the read of q that p planned to fn, in time order
// (the tracepack semantics specification §7.4).
//
// The clusters not excluded are taken in ascending (ts_min, capture_id, first seq).
// Before each cluster is resolved,
// every pending candidate whose ts_utc_ns is strictly below the cluster's ts_min is yielded;
// then the cluster is resolved whole, its candidates added to the pending ones.
// Once no cluster is left, every pending candidate is yielded.
// Pending candidates are yielded in ascending (ts_utc_ns, capture_id, seq, version),
// so a record whose ts_utc_ns lies before its block's ts_min, as only a misstated index allows,
// is yielded when the order reaches it.
// fn receives one Item, reused, valid during the call; its payload and header extension alias the representative's block.
//
// Returns:
//   - error: fn's error, as is; ctx's error, wrapped;
//     an error wrapping ErrReadLimit when a block's reservation would exceed MaxHeldBytes
//     or one more conflict would exceed MaxConflicts;
//     a ReadAt error, wrapped with the pack and block index.
//     p.res holds the defects and conflicts found until then.
//     Nothing stays held after runTime, whatever its outcome.
func (p *mergeIteratePlan) runTime(ctx context.Context, q *Query, fn func(*Item) error) error {
	run := newMergeIterateRun(p, q)
	var units []timeUnit
	for k := range p.captures {
		for c := range p.captures[k].clusters {
			if cl := &p.captures[k].clusters[c]; !cl.excluded {
				units = append(units, timeUnit{rank: k, c: cl})
			}
		}
	}
	slices.SortFunc(units, func(a, b timeUnit) int {
		return cmp.Or(cmp.Compare(a.c.tsMin, b.c.tsMin), cmp.Compare(a.rank, b.rank), cmp.Compare(a.c.first, b.c.first))
	})

	var (
		pending pendingHeap
		cr      *clusterResolver
		it      Item
	)
	defer func() {
		for i := range pending {
			pending[i].release(&p.loader)
		}
		if cr != nil {
			cr.close()
		}
	}()

	// yieldBelow yields every pending candidate, while the cut-off below is set those with ts_utc_ns strictly below it.
	// A candidate stays in the heap until fn returns nil for it, so the deferred release covers it on an error.
	yieldBelow := func(below *int64) error {
		for len(pending) > 0 && (below == nil || pending[0].ts < *below) {
			if err := run.yield(ctx, &pending[0].candidate, &it, fn); err != nil {
				return err
			}
			pending[0].release(&p.loader)
			pending.pop()
		}

		return nil
	}

	var batch []candidate
	for _, u := range units {
		if err := yieldBelow(&u.c.tsMin); err != nil {
			return err
		}

		cr = newClusterResolver(run, p.captures[u.rank].id, u.c)
		for more := true; more; {
			// next appends a candidate only with no error, so batch holds nothing to release after an error,
			// and none once the cluster is resolved.
			var err error
			if batch, more, err = cr.next(ctx, batch[:0]); err != nil {
				return err
			}
			for i := range batch {
				pending.push(pendingCandidate{candidate: batch[i], rank: u.rank})
			}
		}
		cr.close()
		cr = nil
	}
	if err := yieldBelow(nil); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("tracepack: merge iterate: %w", err)
	}

	return nil
}

// less reports whether the candidate at i comes before the one at j:
// by ts_utc_ns, then capture rank, then seq, then version.
func (h pendingHeap) less(i, j int) bool {
	a, b := &h[i], &h[j]

	return cmp.Or(cmp.Compare(a.ts, b.ts), cmp.Compare(a.rank, b.rank), cmp.Compare(a.seq, b.seq), cmp.Compare(a.version, b.version)) < 0
}

// push adds c to the heap.
func (h *pendingHeap) push(c pendingCandidate) {
	*h = append(*h, c)
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

// pop removes the heap's first candidate, which the caller released; the heap is not empty.
func (h *pendingHeap) pop() {
	q := *h
	n := len(q) - 1
	q[0], q[n] = q[n], pendingCandidate{}
	*h = q[:n]
	q = *h
	for i := 0; ; {
		c := 2*i + 1
		if c >= n {
			return
		}
		if r := c + 1; r < n && q.less(r, c) {
			c = r
		}
		if !q.less(c, i) {
			return
		}
		q[i], q[c] = q[c], q[i]
		i = c
	}
}
