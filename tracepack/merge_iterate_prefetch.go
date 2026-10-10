package tracepack

import (
	"context"
	"sync"
	"sync/atomic"
)

// prefetchDepth is the most blocks the prefetch queue of a run holds, whether a Reader's retained bytes serve them or not.
// Every ReadAt the prefetch issues is for a queued block, so it is also the most such calls in flight at once.
const prefetchDepth = 8

// blockRef names one block of a read over several packs:
// the index of its Reader among the readers of the read, and its index into that Reader's blocks.
type blockRef struct {
	pack, block int
}

// prefetchEntry is a block admitted to the prefetch queue before the run demands it.
// It holds the block's full loader reservation from its admission until the run demands it, or the prefetch closes.
type prefetchEntry struct {
	// size is the bytes reserved for the block, its blockSizes.total; 0 for a block failing its preflight,
	// which reserves nothing and is never read.
	size int64
	// raw is the buffer the speculative read fills, at the block's on_disk_len;
	// nil for a block the Reader's retained bytes serve, read only when the run demands it.
	raw []byte
	// done is closed once the speculative read returned, err holding its error; nil without a speculative read.
	done chan struct{}
	err  error
}

// prefetcher reads the blocks a run of a read over several packs will demand next ahead of the demand,
// so that the ReadAt calls of several blocks overlap.
//
// The run's demand is unchanged:
// it loads the same blocks in the same order through heldLoader.load, which takes the queue's head.
// frontier is that demand, known before the run starts, every block the run will load in order.
// The queue is always the next blocks of frontier from the next demand on, contiguous, at most prefetchDepth of them.
// A block is admitted only when its full reservation fits the budget beside every reservation held, demanded or queued,
// and nothing after a block that does not fit is admitted before it; nothing queued is ever evicted.
// A demand is therefore either the queue's head, whose reservation it takes over, or made while the queue is empty,
// beside exactly the reservations of the run without prefetch,
// so a demand fails its reservation exactly when it would without prefetch.
//
// A speculative read only fills the block's raw bytes:
// the block is checked and decoded, and its defect or read error reported, when the run demands it.
// The prefetcher is used by the run's goroutine only; its speculative reads run on goroutines of their own,
// each writing only its entry's raw and err before closing done.
type prefetcher struct {
	// ctx is the read's; no read is issued once it is done.
	ctx     context.Context
	readers []*Reader
	budget  *heldBudget
	// frontier is every block the run will load, in the order it loads them.
	frontier []blockRef
	// pos is the index into frontier of the run's next demand.
	pos int
	// queue holds the entries of frontier[pos:pos+len(queue)].
	queue []*prefetchEntry
	// stopped is set once the prefetch admits nothing more.
	stopped bool
	wg      sync.WaitGroup
	// inflight counts the speculative reads issued and not returned.
	inflight atomic.Int32
}

// startPrefetch starts the prefetch of the run whose every load, in order, is frontier,
// and admits its first blocks; the loader's loads take them.
// It returns nil, and the run reads each block only when it demands it, when frontier is empty or prefetch is off.
// The caller closes the prefetcher returned before the run's other cleanup.
func (p *mergeIteratePlan) startPrefetch(ctx context.Context, frontier []blockRef) *prefetcher {
	if p.noPrefetch || len(frontier) == 0 {
		return nil
	}
	pf := &prefetcher{ctx: ctx, readers: p.readers, budget: &p.loader.budget, frontier: frontier}
	p.loader.pf = pf
	pf.fill()

	return pf
}

// timeDemand returns every block a run in time order over units, in their order, loads:
// each cluster's blocks in cluster order, which is the order its seqCursor loads them in.
func timeDemand(units []timeUnit) []blockRef {
	var out []blockRef
	for _, u := range units {
		out = u.c.appendDemand(out)
	}

	return out
}

// captureDemand returns every block a run in capture order over p's captures loads, in order,
// when at most one capture has a cluster not excluded, the run then advancing that capture's stream alone;
// nil otherwise, since which capture advances next depends on the records the filter selects.
func (p *mergeIteratePlan) captureDemand() []blockRef {
	var kept *planCapture
	for k := range p.captures {
		pc := &p.captures[k]
		for c := range pc.clusters {
			if pc.clusters[c].excluded {
				continue
			}
			if kept != nil && kept != pc {
				return nil
			}
			kept = pc
		}
	}
	if kept == nil {
		return nil
	}

	var out []blockRef
	for c := range kept.clusters {
		if cl := &kept.clusters[c]; !cl.excluded {
			out = cl.appendDemand(out)
		}
	}

	return out
}

// appendDemand appends the blocks of the cluster to dst in cluster order.
func (c *planCluster) appendDemand(dst []blockRef) []blockRef {
	for k := range c.blocks {
		dst = append(dst, blockRef{pack: c.blocks[k].pack, block: c.blocks[k].block})
	}

	return dst
}

// fill admits the next blocks of the frontier to the queue, in order, while the queue has room:
// a block failing its preflight with no reservation and no read;
// any other only when its full reservation fits the budget, which it then reserves,
// a block outside the Reader's retained bytes also issuing its speculative read.
// It stops at the first block that does not fit, so the queue stays contiguous,
// and before any block that needs a reservation once the read's ctx is done.
// It does nothing once the prefetcher is stopped.
func (pf *prefetcher) fill() {
	if pf == nil || pf.stopped {
		return
	}
	for len(pf.queue) < prefetchDepth && pf.pos+len(pf.queue) < len(pf.frontier) {
		ref := pf.frontier[pf.pos+len(pf.queue)]
		r := pf.readers[ref.pack]
		e := &prefetchEntry{}
		if s, def := r.preflightBlock(ref.block); def == nil {
			if !pf.budget.fits(s.total) || pf.ctx.Err() != nil {
				return
			}
			_ = pf.budget.reserve(s.total) // fits checked it
			e.size = s.total
			info := &r.blocks[ref.block]
			if r.heldRange(int64(info.Offset), int64(info.OnDiskLen)) == nil {
				pf.issue(r, e, int64(info.Offset), s.raw)
			}
		}
		pf.queue = append(pf.queue, e)
	}
}

// issue starts the speculative read of the n bytes at off of r into a new buffer of e.
func (pf *prefetcher) issue(r *Reader, e *prefetchEntry, off int64, n int) {
	e.raw = make([]byte, n)
	e.done = make(chan struct{})
	pf.wg.Add(1)
	pf.inflight.Add(1)
	go func() {
		defer pf.wg.Done()
		e.err = r.fetch(e.raw, off)
		pf.inflight.Add(-1)
		close(e.done)
	}()
}

// take records the run's demand of block i of the Reader at index pack,
// and returns the queue's head when the queue holds it; the caller then owns the entry and its reservation.
// It returns nil when the queue is empty, and the demand reserves and reads as without prefetch.
// A demand other than the frontier's next, which a correct frontier never meets, stops the prefetch first,
// releasing every queued reservation, so the demand still finds only the reservations of the run without prefetch.
func (pf *prefetcher) take(pack, i int) *prefetchEntry {
	if pf == nil || pf.stopped {
		return nil
	}
	if pf.pos == len(pf.frontier) || pf.frontier[pf.pos] != (blockRef{pack: pack, block: i}) {
		pf.close()

		return nil
	}
	pf.pos++
	if len(pf.queue) == 0 {
		return nil
	}
	e := pf.queue[0]
	n := copy(pf.queue, pf.queue[1:])
	pf.queue[n] = nil
	pf.queue = pf.queue[:n]

	return e
}

// close stops the prefetch: it admits nothing more, waits for every speculative read in flight,
// then releases the reservation of every block still queued, discarding its bytes and its error.
// No ReadAt of the prefetch is in flight once it returns.
// A nil prefetcher, or one closed before, does nothing more.
func (pf *prefetcher) close() {
	if pf == nil {
		return
	}
	pf.stopped = true
	pf.wg.Wait()
	for k, e := range pf.queue {
		pf.budget.release(e.size)
		pf.queue[k] = nil
	}
	pf.queue = pf.queue[:0]
}

// bufFor returns the buffers of a block of sizes s whose raw bytes e's speculative read filled:
// raw is e's, the others empty at exactly the capacities of s, as newBuf allocates them.
func (s *blockSizes) bufFor(e *prefetchEntry) *blockBuf {
	return &blockBuf{
		raw:  e.raw,
		rows: make([]byte, 0, s.rows),
		offs: make([]uint32, 0, s.offs),
	}
}
