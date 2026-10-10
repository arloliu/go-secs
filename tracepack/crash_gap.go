package tracepack

import (
	"bytes"
	"cmp"
	"fmt"
	"slices"
)

// crashGap is one stop-unclean boundary a read found, with its representative:
// the smallest index, among the readers of the read, of a pack holding it.
type crashGap struct {
	pack int
	b    Boundary
}

// crashGapKey is the whole value of a boundary, by which a read reports it once:
// boundaries whose gap bounds differ have different keys.
type crashGapKey struct {
	capture          UUID
	seq              uint64
	kind             BoundaryKind
	ts               int64
	epoch            uint32
	hasStart, hasEnd bool
	gapStart, gapEnd int64
}

// crashGaps gathers the crash gaps of a read (the tracepack semantics specification §7.4, Crash gaps):
// the stop-unclean boundaries whose gap meets the query's time range [from, to), a nil bound being unbounded,
// each boundary value once, with the smallest pack index holding it.
// The zero value with from and to set is ready to use.
type crashGaps struct {
	from, to *int64
	gaps     []crashGap
	// seen maps the key of each boundary gathered to its index in gaps; nil until the first one.
	seen map[crashGapKey]int
	// next is the index in gaps of the first gap not yet reported (flush).
	next int
}

// cloneBoundary returns a copy of b with fresh GapStart and GapEnd pointers.
func cloneBoundary(b Boundary) Boundary {
	if b.GapStart != nil {
		b.GapStart = new(*b.GapStart)
	}
	if b.GapEnd != nil {
		b.GapEnd = new(*b.GapEnd)
	}

	return b
}

// crashGapMeets reports whether the gap interval [GapStart, GapEnd] of b meets the time range [from, to),
// by the matching rule of a coverage entry (the tracepack format specification §5):
// a nil bound of either is unbounded,
// and an interval whose GapStart exceeds its GapEnd meets every range, as an inverted coverage entry does.
func crashGapMeets(b *Boundary, from, to *int64) bool {
	return inverted(b.GapStart, b.GapEnd) || overlaps(b.GapStart, b.GapEnd, from, to)
}

// addFooter gathers the stop-unclean boundaries of the F-5 statistics of r, the Reader at index pack,
// whose footer it uses.
func (c *crashGaps) addFooter(pack int, r *Reader) {
	capture := UUID(r.hdr.CaptureID)
	for i := range r.footer.stats.boundaries {
		c.add(pack, capture, &r.footer.stats.boundaries[i])
	}
}

// addRecords gathers the stop-unclean boundaries of the capture-boundary records of d,
// a decoded block of the Reader at index pack, whose capture is capture.
// A transport-event payload that does not decode contributes nothing, as in the Writer's summary.
func (c *crashGaps) addRecords(pack int, capture UUID, d *decodedBlock) {
	for j := range d.count() {
		h := d.header(j)
		if Kind(h.Kind) != KindTransportEvent {
			continue
		}
		ev, err := UnmarshalTransportEvent(d.payload(j))
		if err != nil || ev.Event != EventCaptureBoundary || ev.BoundaryKind == nil {
			continue
		}
		c.add(pack, capture, &boundarySummary{
			seq: h.Seq, kind: *ev.BoundaryKind, ts: h.TSUTCNs, epoch: h.Epoch, gapStart: ev.GapStart, gapEnd: ev.GapEnd,
		})
	}
}

// add gathers the boundary s of capture, held by the pack at index pack,
// when it is a stop-unclean boundary whose gap meets the read's time range.
// A boundary gathered before keeps the smaller pack index.
func (c *crashGaps) add(pack int, capture UUID, s *boundarySummary) {
	if s.kind != BoundaryKindStopUnclean {
		return
	}
	b := Boundary{Capture: capture, Seq: s.seq, Kind: s.kind, TS: s.ts, Epoch: s.epoch, GapStart: s.gapStart, GapEnd: s.gapEnd}
	if !crashGapMeets(&b, c.from, c.to) {
		return
	}

	k := keyOfCrashGap(&b)
	if i, ok := c.seen[k]; ok {
		// A gap already reported keeps the pack it was reported with.
		if i >= c.next {
			c.gaps[i].pack = min(c.gaps[i].pack, pack)
		}

		return
	}
	if c.seen == nil {
		c.seen = make(map[crashGapKey]int)
	}
	c.seen[k] = len(c.gaps)
	c.gaps = append(c.gaps, crashGap{pack: pack, b: cloneBoundary(b)})
}

// flush appends to dst a ReasonCrashGap defect for each gap gathered since the last flush,
// in ascending pack index, then seq, then the boundary's other fields, and returns dst.
// Each defect carries its own copy of the boundary:
// add cloned it, and each gap is flushed once.
func (c *crashGaps) flush(dst []Defect) []Defect {
	fresh := c.gaps[c.next:]
	slices.SortFunc(fresh, compareCrashGaps)
	for i := range fresh {
		g := &fresh[i]
		c.seen[keyOfCrashGap(&g.b)] = c.next + i
		b := g.b
		dst = append(dst, Defect{
			Reason:   ReasonCrashGap,
			Pack:     g.pack,
			Block:    -1,
			Offset:   -1,
			Boundary: &b,
			Err: fmt.Errorf("tracepack: the gap of the stop-unclean boundary at seq %d of capture %s meets the query: its records may be lost",
				b.Seq, b.Capture),
		})
	}
	c.next = len(c.gaps)

	return dst
}

// keyOfCrashGap returns the key of the whole value of b.
func keyOfCrashGap(b *Boundary) crashGapKey {
	k := crashGapKey{capture: b.Capture, seq: b.Seq, kind: b.Kind, ts: b.TS, epoch: b.Epoch}
	if b.GapStart != nil {
		k.hasStart, k.gapStart = true, *b.GapStart
	}
	if b.GapEnd != nil {
		k.hasEnd, k.gapEnd = true, *b.GapEnd
	}

	return k
}

// compareCrashGaps orders gaps by pack index, then seq, boundary kind, ts, epoch, gap_start, gap_end and capture_id,
// an absent gap bound before any present one.
func compareCrashGaps(a, b crashGap) int {
	x, y := &a.b, &b.b

	return cmp.Or(
		cmp.Compare(a.pack, b.pack),
		cmp.Compare(x.Seq, y.Seq),
		cmp.Compare(x.Kind, y.Kind),
		cmp.Compare(x.TS, y.TS),
		cmp.Compare(x.Epoch, y.Epoch),
		compareOptional(x.GapStart, y.GapStart),
		compareOptional(x.GapEnd, y.GapEnd),
		bytes.Compare(x.Capture[:], y.Capture[:]),
	)
}
