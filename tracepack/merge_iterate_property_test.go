package tracepack

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"reflect"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// The reference of a read over several packs (mergeIterateReference)
// computes what MergeIterate yields and reports from the tracepack semantics specification §7.4 alone,
// without MergeIterate's code:
// it reads every block of every reader with Reader.readBlock into a buffer of its own,
// keeps each record's header row as stored, all record_header_len bytes, and its payload,
// and takes a block's ranges and counts from its F-2 entry and F-3 summary when indexed, from its records when walked.
// It chains the overlap clusters, excludes them, compares the copies of each seq, groups them into versions,
// applies the filter and orders the records and the conflicts with code of its own.
// From the Reader it takes only what Open found:
// the block index, the footer, the defects and coverage entries of the pack.
// It holds for packs whose blocks agree with their index, which every generated read has (genReadOf).

// refCopy is one copy of a record as its block stores it.
type refCopy struct {
	// h is the record header decoded from row, the record's header row as stored, all record_header_len bytes;
	// payload is the record's payload.
	h       format.RecordHeader
	row     []byte
	payload []byte
}

// refBlock is one block of a read over several packs as the reference sees it:
// what the read clusters and excludes it by, as its F-2 entry and F-3 summary state it when indexed
// and as its records give it when walked, and its records, none when its read failed.
type refBlock struct {
	pack, block int
	indexed     bool
	// failed reports a block whose read failed: it contributes no records.
	// defect is then the defect its read returned.
	failed             bool
	defect             defectKey
	first, last        uint64
	tsMin, tsMax       int64
	epochMin, epochMax uint32
	// kinds and dirs count the block's records by raw kind and dir value.
	kinds, dirs []uint32
	records     []refCopy
}

// refVersion is one version of a record of a cluster compared:
// its representative, the first block holding it in cluster order, the record there,
// and the indexes of the readers holding it, ascending, each once.
type refVersion struct {
	rep     *refBlock
	rec     refCopy
	holders []int
}

// refCluster is one overlap cluster of a capture of the read.
type refCluster struct {
	// capture is the index of the cluster's capture in ascending capture_id.
	capture  int
	first    uint64
	tsMin    int64
	excluded bool
	// blocks holds the cluster's blocks in cluster order: ascending first seq, then reader order, then block order.
	blocks []*refBlock
	// seqs holds the versions of each seq of a cluster not excluded, in ascending seq, versions in version order.
	seqs [][]refVersion
}

// mergeIterateReference is what the reference computed of a read of q over readers.
type mergeIterateReference struct {
	readers []*Reader
	q       Query
	// ids holds the read's capture_ids in ascending order, and clusters the clusters of each, in ascending first seq.
	ids      []UUID
	clusters [][]*refCluster
	// blocks holds every block of the read, by reader then by block.
	blocks [][]*refBlock
}

// defectKey is what a comparison of defects compares: the reason, the pack, the block and the offset.
type defectKey struct {
	reason IncompleteReason
	pack   int
	block  int
	offset int64
}

// refStep is one seq a capture stream resolves in capture order: its versions, and those the query selects.
type refStep struct {
	versions []refVersion
	selected []int
}

// newMergeIterateReference reads every block of readers and computes the clusters, exclusions and versions of a read of q.
// It requires every block read to agree with its index.
func newMergeIterateReference(t testing.TB, readers []*Reader, q Query) *mergeIterateReference {
	t.Helper()

	ref := &mergeIterateReference{readers: readers, q: q, blocks: make([][]*refBlock, len(readers))}
	byCapture := map[UUID][]*refBlock{}
	for i, r := range readers {
		capture := UUID(r.hdr.CaptureID)
		for j := range r.blocks {
			b := refBlockOf(t, r, i, j)
			ref.blocks[i] = append(ref.blocks[i], b)
			// A walked block whose read failed has no ranges, so it takes no part in the clusters.
			if b.indexed || !b.failed {
				byCapture[capture] = append(byCapture[capture], b)
			}
		}
	}

	for id := range byCapture {
		ref.ids = append(ref.ids, id)
	}
	slices.SortFunc(ref.ids, func(a, b UUID) int { return bytes.Compare(a[:], b[:]) })
	f := &q.Filter
	for k, id := range ref.ids {
		blocks := byCapture[id]
		slices.SortStableFunc(blocks, func(a, b *refBlock) int { return cmp.Compare(a.first, b.first) })
		var clusters []*refCluster
		var last uint64
		for _, b := range blocks {
			if n := len(clusters); n > 0 && b.first <= last {
				c := clusters[n-1]
				c.blocks = append(c.blocks, b)
				c.tsMin = min(c.tsMin, b.tsMin)
				last = max(last, b.last)

				continue
			}
			clusters = append(clusters, &refCluster{capture: k, first: b.first, tsMin: b.tsMin, blocks: []*refBlock{b}})
			last = b.last
		}
		for _, c := range clusters {
			c.excluded = !slices.ContainsFunc(c.blocks, func(b *refBlock) bool { return !refExcludes(f, b) })
			if !c.excluded {
				c.seqs = refVersionsOf(c)
			}
		}
		ref.clusters = append(ref.clusters, clusters)
	}

	return ref
}

// refBlockOf reads block j of r, the reader at index pack, in full into a buffer of its own,
// and returns it with its records and with its ranges and counts:
// those its F-2 entry and F-3 summary state when indexed, those its records give when walked.
func refBlockOf(t testing.TB, r *Reader, pack, j int) *refBlock {
	t.Helper()

	info := &r.blocks[j]
	b := &refBlock{pack: pack, block: j, indexed: info.Indexed}
	var buf blockBuf
	d, def, err := r.readBlock(j, &buf)
	require.NoError(t, err)
	if def != nil {
		require.NotEqual(t, ReasonIndexMismatch, def.Reason, "pack %d, block %d: the reference holds for blocks that agree with their index", pack, j)
	}
	b.failed = d == nil
	if b.failed {
		b.defect = defectKey{reason: def.Reason, pack: pack, block: j, offset: def.Offset}
	} else {
		rhl := int(d.env.RecordHeaderLen)
		for k := range d.count() {
			row := bytes.Clone(d.section[k*rhl : (k+1)*rhl])
			h, err := format.UnmarshalRecordHeader(row, format.RecordHeaderLen)
			require.NoError(t, err)
			b.records = append(b.records, refCopy{h: h, row: row, payload: bytes.Clone(d.payload(k))})
		}
	}

	if b.indexed {
		s := &r.footer.blocks[j]
		b.first, b.last = info.FirstSeq, info.LastSeq
		b.tsMin, b.tsMax, b.epochMin, b.epochMax = info.TSMin, info.TSMax, info.EpochMin, info.EpochMax
		b.kinds, b.dirs = slices.Clone(s.kindCounts), slices.Clone(s.dirCounts)

		return b
	}
	for k, c := range b.records {
		h := &c.h
		if k == 0 {
			b.first, b.tsMin, b.tsMax, b.epochMin, b.epochMax = h.Seq, h.TSUTCNs, h.TSUTCNs, h.Epoch, h.Epoch
		}
		b.last = h.Seq
		b.tsMin, b.tsMax = min(b.tsMin, h.TSUTCNs), max(b.tsMax, h.TSUTCNs)
		b.epochMin, b.epochMax = min(b.epochMin, h.Epoch), max(b.epochMax, h.Epoch)
		b.kinds, b.dirs = refCount(b.kinds, h.Kind), refCount(b.dirs, h.Dir)
	}

	return b
}

// refCount counts one record of value v in counts, indexed by value.
func refCount(counts []uint32, v uint8) []uint32 {
	for len(counts) <= int(v) {
		counts = append(counts, 0)
	}
	counts[v]++

	return counts
}

// refExcludes reports whether the ranges and counts of b exclude it from the query of f,
// as the tracepack format specification §10 lets them:
// its time range misses [TimeFrom, TimeTo), its epoch range holds none of Epochs,
// or it counts no record of any of Kinds or of Dirs.
func refExcludes(f *Filter, b *refBlock) bool {
	switch {
	case f.TimeFrom != nil && f.TimeTo != nil && *f.TimeFrom >= *f.TimeTo,
		f.TimeFrom != nil && b.tsMax < *f.TimeFrom,
		f.TimeTo != nil && b.tsMin >= *f.TimeTo:
		return true
	case len(f.Epochs) > 0 && !slices.ContainsFunc(f.Epochs, func(e uint32) bool { return e >= b.epochMin && e <= b.epochMax }):
		return true
	}
	counted := func(counts []uint32, v uint8) bool { return int(v) < len(counts) && counts[v] > 0 }

	return len(f.Kinds) > 0 && !slices.ContainsFunc(f.Kinds, func(k Kind) bool { return counted(b.kinds, uint8(k)) }) ||
		len(f.Dirs) > 0 && !slices.ContainsFunc(f.Dirs, func(d Dir) bool { return counted(b.dirs, uint8(d)) })
}

// refVersionsOf returns the versions of each seq of c, in ascending seq:
// the copies of the seq in the blocks of c that did not fail, in cluster order,
// equal header row and payload being one version.
func refVersionsOf(c *refCluster) [][]refVersion {
	bySeq := map[uint64][]refVersion{}
	for _, b := range c.blocks {
		for _, rec := range b.records {
			vs := bySeq[rec.h.Seq]
			k := slices.IndexFunc(vs, func(v refVersion) bool {
				return bytes.Equal(v.rec.row, rec.row) && bytes.Equal(v.rec.payload, rec.payload)
			})
			if k < 0 {
				k = len(vs)
				vs = append(vs, refVersion{rep: b, rec: rec})
			}
			vs[k].holders = append(vs[k].holders, b.pack)
			bySeq[rec.h.Seq] = vs
		}
	}

	seqs := make([]uint64, 0, len(bySeq))
	for seq := range bySeq {
		seqs = append(seqs, seq)
	}
	slices.Sort(seqs)
	out := make([][]refVersion, len(seqs))
	for i, seq := range seqs {
		out[i] = bySeq[seq]
		for k := range out[i] {
			slices.Sort(out[i][k].holders)
			out[i][k].holders = slices.Compact(out[i][k].holders)
		}
	}

	return out
}

// refRecord returns the record c as a read yields it: every field of its header as stored, its payload only with payload.
func refRecord(c *refCopy, payload bool) Record {
	h := &c.h
	r := Record{
		Seq: h.Seq, TSUTCNs: h.TSUTCNs, MonoNs: h.MonoNs, MonoPresent: RecordFlags(h.RecordFlags).Has(RecordFlagsMonoPresent),
		Epoch: h.Epoch, Kind: Kind(h.Kind), Dir: Dir(h.Dir), Fidelity: Fidelity(h.Fidelity), DecodeStatus: DecodeStatus(h.DecodeStatus),
		TrailingBytes: h.TrailingBytes, Quality: Quality(h.Quality), FieldValidity: FieldValidity(h.FieldValidity),
	}
	if payload {
		r.Payload = bytes.Clone(c.payload)
	}

	return r
}

// refSelects reports whether f selects the record c, as Filter states it:
// its time range, Kinds, Dirs and Epochs on the record header,
// and its HSMS header predicates on the fields of the whole payload,
// an unavailable field matching only with IncludeUnavailable.
func refSelects(f *Filter, c *refCopy) bool {
	h := &c.h
	if f.TimeFrom != nil && h.TSUTCNs < *f.TimeFrom || f.TimeTo != nil && h.TSUTCNs >= *f.TimeTo {
		return false
	}
	if len(f.Kinds) > 0 && !slices.Contains(f.Kinds, Kind(h.Kind)) ||
		len(f.Dirs) > 0 && !slices.Contains(f.Dirs, Dir(h.Dir)) ||
		len(f.Epochs) > 0 && !slices.Contains(f.Epochs, h.Epoch) {
		return false
	}

	rec := refRecord(c, true)
	fields := rec.HSMSHeader()
	field := func(need FieldValidity, match bool) bool {
		if fields.Available&need != need {
			return f.IncludeUnavailable
		}

		return match
	}
	sf := SF{Stream: fields.Stream, Function: fields.Function}

	return (len(f.SF) == 0 || field(FieldValidityStreamAndW|FieldValidityFunction, slices.Contains(f.SF, sf))) &&
		(len(f.SessionIDs) == 0 || field(FieldValiditySessionID, slices.Contains(f.SessionIDs, fields.SessionID))) &&
		(f.SystemBytes == nil || field(FieldValiditySystemBytes, *f.SystemBytes == fields.SystemBytes))
}

// item returns the Item of version k of vs as the read yields it, from its representative.
func (ref *mergeIterateReference) item(vs []refVersion, k int) iterItem {
	v := &vs[k]
	var extra []byte
	if len(v.rec.row) > format.RecordHeaderLen {
		extra = bytes.Clone(v.rec.row[format.RecordHeaderLen:])
	}

	return iterItem{rec: refRecord(&v.rec, ref.q.Payloads), extra: extra, pack: v.rep.pack, block: v.rep.block, conflict: len(vs) > 1}
}

// conflict returns the conflict of the seq whose versions are vs, of the capture at index capture.
func (ref *mergeIterateReference) conflict(capture int, vs []refVersion) Conflict {
	versions := make([][]UUID, len(vs))
	for k := range vs {
		versions[k] = make([]UUID, len(vs[k].holders))
		for j, i := range vs[k].holders {
			versions[k][j] = UUID(ref.readers[i].hdr.PackID)
		}
	}

	return Conflict{CaptureID: ref.ids[capture], Seq: vs[0].rec.h.Seq, Versions: versions}
}

// selected returns the indexes of the versions of vs the query selects, in version order.
func (ref *mergeIterateReference) selected(vs []refVersion) []int {
	var out []int
	for k := range vs {
		if refSelects(&ref.q.Filter, &vs[k].rec) {
			out = append(out, k)
		}
	}

	return out
}

// read returns what a read in order yields and the conflicts it lists, in their orders
// (the tracepack semantics specification §7.4);
// both are nil when empty.
func (ref *mergeIterateReference) read(order Order) ([]iterItem, []Conflict) {
	if order == OrderTime {
		return ref.timeRead()
	}

	return ref.captureRead()
}

// timeRead returns what a read in time order yields,
// every selected version sorted by (ts_utc_ns, capture_id, seq, version),
// and the conflicts it lists:
// the clusters not excluded in ascending (ts_min, capture_id, first seq), seqs ascending within each.
func (ref *mergeIterateReference) timeRead() ([]iterItem, []Conflict) {
	type yielded struct {
		ts      int64
		capture int
		seq     uint64
		version int
		item    iterItem
	}
	var all []yielded
	var clusters []*refCluster
	for _, cs := range ref.clusters {
		for _, c := range cs {
			if c.excluded {
				continue
			}
			clusters = append(clusters, c)
			for _, vs := range c.seqs {
				for _, k := range ref.selected(vs) {
					all = append(all, yielded{ts: vs[k].rec.h.TSUTCNs, capture: c.capture, seq: vs[k].rec.h.Seq, version: k, item: ref.item(vs, k)})
				}
			}
		}
	}
	slices.SortFunc(all, func(a, b yielded) int {
		return cmp.Or(cmp.Compare(a.ts, b.ts), cmp.Compare(a.capture, b.capture), cmp.Compare(a.seq, b.seq), cmp.Compare(a.version, b.version))
	})
	slices.SortFunc(clusters, func(a, b *refCluster) int {
		return cmp.Or(cmp.Compare(a.tsMin, b.tsMin), cmp.Compare(a.capture, b.capture), cmp.Compare(a.first, b.first))
	})

	items := make([]iterItem, 0, len(all))
	for _, y := range all {
		items = append(items, y.item)
	}
	var conflicts []Conflict
	for _, c := range clusters {
		for _, vs := range c.seqs {
			if len(vs) > 1 {
				conflicts = append(conflicts, ref.conflict(c.capture, vs))
			}
		}
	}

	return nilIfEmpty(items), conflicts
}

// captureRead simulates a read in capture order and returns what it yields and the conflicts it lists.
// Each capture resolves its seqs in ascending seq, listing a conflict as it resolves it,
// up to the next seq with a selected version, whose selected versions it queues in version order.
// The captures resolve so in ascending capture_id first;
// then the capture whose first queued version has the representative with the smallest ts_min, ties broken by capture_id,
// yields that version, and, once its queue is empty, resolves on to its next selected seq,
// until no capture has a queued version.
func (ref *mergeIterateReference) captureRead() ([]iterItem, []Conflict) {
	type stream struct {
		steps []refStep
		next  int
		vs    []refVersion
		queue []int
	}
	var conflicts []Conflict
	// fill resolves the next seqs of the capture at index k until one has a selected version.
	fill := func(k int, s *stream) bool {
		for s.next < len(s.steps) {
			step := s.steps[s.next]
			s.next++
			if len(step.versions) > 1 {
				conflicts = append(conflicts, ref.conflict(k, step.versions))
			}
			if len(step.selected) > 0 {
				s.vs, s.queue = step.versions, step.selected

				return true
			}
		}

		return false
	}

	streams := make([]*stream, len(ref.ids))
	for k := range ref.ids {
		s := &stream{}
		for _, c := range ref.clusters[k] {
			if c.excluded {
				continue
			}
			for _, vs := range c.seqs {
				s.steps = append(s.steps, refStep{versions: vs, selected: ref.selected(vs)})
			}
		}
		if fill(k, s) {
			streams[k] = s
		}
	}

	var items []iterItem
	for {
		k := -1
		for i, s := range streams {
			if s != nil && (k < 0 || s.vs[s.queue[0]].rep.tsMin < streams[k].vs[streams[k].queue[0]].rep.tsMin) {
				k = i
			}
		}
		if k < 0 {
			return items, conflicts
		}
		s := streams[k]
		items = append(items, ref.item(s.vs, s.queue[0]))
		s.queue = s.queue[1:]
		if len(s.queue) == 0 && !fill(k, s) {
			streams[k] = nil
		}
	}
}

// packDefects returns the defects every read of the readers carries,
// those Open found and the coverage defects for the query,
// in reader order, each with Pack set to its reader's index.
func (ref *mergeIterateReference) packDefects() []defectKey {
	var out []defectKey
	for i, r := range ref.readers {
		for _, d := range r.appendCoverageDefects(slices.Clone(r.openDefects), &ref.q.Filter) {
			out = append(out, defectKey{reason: d.Reason, pack: i, block: d.Block, offset: d.Offset})
		}
	}

	return out
}

// preReadDefects returns the defects of the walked blocks that fail the read made before the read yields anything,
// in reader order, then block order.
func (ref *mergeIterateReference) preReadDefects() []defectKey {
	var out []defectKey
	for _, bs := range ref.blocks {
		for _, b := range bs {
			if !b.indexed && b.failed {
				out = append(out, b.defect)
			}
		}
	}

	return out
}

// resolutionDefects returns the defects of the failed indexed blocks of the clusters not excluded,
// which a read in order reads:
// in time order, in the order it reads them,
// the clusters in ascending (ts_min, capture_id, first seq), each in cluster order;
// sorted in capture order, whose interleaving of the captures decides when it reads each cluster.
func (ref *mergeIterateReference) resolutionDefects(order Order) []defectKey {
	var clusters []*refCluster
	for _, cs := range ref.clusters {
		for _, c := range cs {
			if !c.excluded {
				clusters = append(clusters, c)
			}
		}
	}
	slices.SortStableFunc(clusters, func(a, b *refCluster) int {
		return cmp.Or(cmp.Compare(a.tsMin, b.tsMin), cmp.Compare(a.capture, b.capture), cmp.Compare(a.first, b.first))
	})

	var out []defectKey
	for _, c := range clusters {
		for _, b := range c.blocks {
			if b.indexed && b.failed {
				out = append(out, b.defect)
			}
		}
	}
	if order == OrderCapture {
		slices.SortFunc(out, compareDefectKeys)
	}

	return out
}

// reads returns the full-block reads of each block a read without error makes, by (reader, block):
// an indexed block once when its cluster is kept, never when it is excluded;
// a walked block once before the read, and once more when its read succeeded and its cluster is kept.
func (ref *mergeIterateReference) reads() map[[2]int]int {
	out := map[[2]int]int{}
	ref.eachBlock(func(b *refBlock, kept bool) {
		n := 0
		if !b.indexed {
			n++
		}
		if kept && (b.indexed || !b.failed) {
			n++
		}
		if n > 0 {
			out[[2]int{b.pack, b.block}] = n
		}
	})

	return out
}

// eachBlock calls fn with every block of the read and whether a cluster not excluded holds it.
func (ref *mergeIterateReference) eachBlock(fn func(b *refBlock, kept bool)) {
	kept := map[*refBlock]bool{}
	for _, cs := range ref.clusters {
		for _, c := range cs {
			for _, b := range c.blocks {
				kept[b] = !c.excluded
			}
		}
	}
	for _, bs := range ref.blocks {
		for _, b := range bs {
			fn(b, kept[b])
		}
	}
}

// footerErrs returns the footer error of each reader whose footer was not used, in reader order; nil when there is none.
func (ref *mergeIterateReference) footerErrs() []PackError {
	var out []PackError
	for i, r := range ref.readers {
		if r.footerErr != nil {
			out = append(out, PackError{Pack: i, Err: r.footerErr})
		}
	}

	return out
}

// compareDefectKeys orders defect keys by pack, block, reason and offset.
func compareDefectKeys(a, b defectKey) int {
	return cmp.Or(cmp.Compare(a.pack, b.pack), cmp.Compare(a.block, b.block), cmp.Compare(a.reason, b.reason), cmp.Compare(a.offset, b.offset))
}

// defectKeys returns the keys of defects, in order.
func defectKeys(defects []Defect) []defectKey {
	out := make([]defectKey, len(defects))
	for i, d := range defects {
		out[i] = defectKey{reason: d.Reason, pack: d.Pack, block: d.Block, offset: d.Offset}
	}

	return out
}

// checkedRead is what readChecked read: a copy of every Item passed to fn, the Result, the error,
// and the number of full-block reads of each block by (reader, block).
type checkedRead struct {
	items []iterItem
	res   Result
	err   error
	reads map[[2]int]int
}

// readChecked validates, plans and reads q over readers in opts.Order, as MergeIterate does,
// counting every full-block read; see readCheckedWith.
func readChecked(t *testing.T, readers []*Reader, q Query, opts MergeIterateOptions) checkedRead {
	t.Helper()

	return readCheckedWith(t, readers, q, opts, nil)
}

// readCheckedWith validates, plans and reads q over readers in opts.Order, as MergeIterate does,
// counting every full-block read, and passes each Item to call, when set, once it is copied;
// an error from call ends the read.
// It requires, while fn runs, the block of the Item's representative to be held, its buffers not dropped,
// and, once the read returns, whatever its outcome,
// nothing to stay held and the buffers of every block read to be dropped.
func readCheckedWith(t *testing.T, readers []*Reader, q Query, opts MergeIterateOptions, call func(*Item) error) checkedRead {
	t.Helper()

	ctx := t.Context()
	out := checkedRead{reads: map[[2]int]int{}}
	o, err := checkMergeIterate(ctx, readers, &q, opts, acceptItems)
	require.NoError(t, err)
	p := newMergeIteratePlan(o)
	var bufs []*blockBuf
	latest := map[[2]int]*blockBuf{}
	p.loader.readHook = func(pack, block int, buf *blockBuf) {
		out.reads[[2]int{pack, block}]++
		bufs = append(bufs, buf)
		latest[[2]int{pack, block}] = buf
	}
	fn := func(it *Item) error {
		buf := latest[[2]int{it.Pack, it.Block}]
		require.NotNil(t, buf, "pack %d, block %d: the representative was read", it.Pack, it.Block)
		require.NotNil(t, buf.raw, "pack %d, block %d: the representative is held while fn runs", it.Pack, it.Block)
		out.items = append(out.items, copyItem(it))
		if call != nil {
			return call(it)
		}

		return nil
	}

	err = p.build(ctx, readers, &q)
	if err == nil {
		if o.Order == OrderCapture {
			err = p.runCapture(ctx, &q, fn)
		} else {
			err = p.runTime(ctx, &q, fn)
		}
	}
	assert.Zero(t, p.loader.budget.held, "nothing stays held")
	for _, buf := range bufs {
		requireDropped(t, buf)
	}
	out.res, out.err = p.rs.end(p.res, err)

	return out
}

// requirePrefix requires got to be the first len(got) elements of want.
func requirePrefix[T any](t *testing.T, want, got []T, what string) {
	t.Helper()

	require.LessOrEqual(t, len(got), len(want), "%s: more than the reference", what)
	if len(got) > 0 {
		require.Equal(t, want[:len(got)], got, "%s: a prefix of the reference", what)
	}
}

// requireReference requires out, a read of ref's query over ref's readers with opts, to agree with ref.
// A read without error yields every record of the reference and lists every conflict, in their orders;
// its defects are those of the packs, then those of the walked blocks whose first read failed,
// then those of the indexed blocks whose read failed,
// in the order a read in time order reads them, in any order in capture order;
// its reads are the reference's, and its footer errors those of the readers.
// A read ended by ErrReadLimit yields a prefix of the records, lists a prefix of the conflicts,
// and reports the defects of the packs, then a prefix of those of the walked blocks,
// then, once those are all reported, some of those of the indexed blocks:
// a prefix of them in time order, any of them, each once, in capture order.
func requireReference(t *testing.T, ref *mergeIterateReference, opts MergeIterateOptions, out checkedRead) {
	t.Helper()

	items, conflicts := ref.read(opts.Order)
	packDefects := ref.packDefects()
	got := defectKeys(out.res.Incomplete)
	if out.err != nil {
		require.ErrorIs(t, out.err, ErrReadLimit)
		requirePrefix(t, items, out.items, "items")
		requirePrefix(t, conflicts, out.res.Conflicts, "conflicts")
		require.GreaterOrEqual(t, len(got), len(packDefects))
		require.Equal(t, packDefects, nilIfEmpty(got[:len(packDefects)]), "the defects of the packs first, in reader order")
		pre, rest := ref.preReadDefects(), got[len(packDefects):]
		n := min(len(rest), len(pre))
		requirePrefix(t, pre, rest[:n], "the defects of walked blocks")
		resolved := nilIfEmpty(slices.Clone(rest[n:]))
		if opts.Order == OrderTime {
			requirePrefix(t, ref.resolutionDefects(OrderTime), resolved, "the defects of indexed blocks")

			return
		}
		all := ref.resolutionDefects(OrderCapture)
		slices.SortFunc(resolved, compareDefectKeys)
		require.Len(t, slices.Compact(slices.Clone(resolved)), len(resolved), "the defects of indexed blocks, each once")
		for _, d := range resolved {
			require.Contains(t, all, d, "the defects of indexed blocks")
		}

		return
	}

	require.Equal(t, items, out.items)
	require.Equal(t, conflicts, out.res.Conflicts)
	require.GreaterOrEqual(t, len(got), len(packDefects))
	require.Equal(t, packDefects, nilIfEmpty(got[:len(packDefects)]), "the defects of the packs first, in reader order")
	pre := ref.preReadDefects()
	rest := got[len(packDefects):]
	require.GreaterOrEqual(t, len(rest), len(pre))
	require.Equal(t, pre, nilIfEmpty(rest[:len(pre)]), "the defects of walked blocks next, in reader then block order")
	resolved := nilIfEmpty(slices.Clone(rest[len(pre):]))
	if opts.Order == OrderCapture {
		slices.SortFunc(resolved, compareDefectKeys)
	}
	require.Equal(t, ref.resolutionDefects(opts.Order), resolved, "the defects of the indexed blocks read")
	require.Equal(t, ref.reads(), out.reads, "full-block reads by (reader, block)")
	require.Equal(t, ref.footerErrs(), out.res.FooterErrs)
	require.NoError(t, out.res.FooterErr)
}

// requirePublicRead requires MergeIterate's own read of q over readers with opts to yield and return what out did.
func requirePublicRead(t *testing.T, readers []*Reader, q Query, opts MergeIterateOptions, out checkedRead) {
	t.Helper()

	items, res, err := collectMergeIterate(t.Context(), readers, q, opts)
	require.Equal(t, out.err == nil, err == nil, "errors %v and %v", out.err, err)
	if err != nil {
		require.Equal(t, out.err.Error(), err.Error())
	}
	require.Equal(t, out.items, items)
	require.Equal(t, out.res.Conflicts, res.Conflicts)
	require.Equal(t, defectKeys(out.res.Incomplete), defectKeys(res.Incomplete))
	require.Equal(t, out.res.FooterErrs, res.FooterErrs)
}

// genRead is a generated read over several packs: the pack files in reader order, the query,
// a MaxHeldBytes, 0 for the default, and what the generator made of the packs.
type genRead struct {
	files     [][]byte
	q         Query
	heldLimit int64
	made      genReadFacts
}

// genReadFacts counts what a generated read holds, for the checks that the generated reads exercise what they should.
type genReadFacts struct {
	walkedPacks, invalidFooters, byteCopies, codecCopies int
	flipped, emptyPacks, coverage, zstd, none            int
}

// genReadRecord returns the record of seq at ts in epoch of a generated capture, drawn from rng:
// mostly a data record with a message text of up to 40 bytes, none of them zero,
// a session_id, stream, function and system bytes from small sets, now and then some of its fields unavailable;
// now and then a control record, a socket-close event or an annotation.
func genReadRecord(t testing.TB, rng *rand.Rand, seq uint64, ts int64, epoch uint32) Record {
	t.Helper()

	dirs := [...]Dir{DirHostToEquipment, DirEquipmentToHost}
	switch rng.IntN(10) {
	case 0:
		return controlRecord(seq, ts, epoch, dirs[rng.IntN(2)])
	case 1:
		return testEventRecord(t, seq, ts, epoch, &TransportEvent{Event: EventSocketClose})
	case 2:
		r := noteRecord(t, epoch)
		r.Seq, r.TSUTCNs = seq, ts

		return r
	default:
		r := bigDataRecord(seq, ts, rng.IntN(40))
		r.Epoch, r.Dir = epoch, dirs[rng.IntN(2)]
		p := r.Payload
		binary.BigEndian.PutUint16(p[4:], uint16(1+rng.IntN(2)))
		p[6] = byte(1+rng.IntN(3)) | byte(rng.IntN(2))<<7
		p[7] = byte(1 + rng.IntN(4))
		p[13] = byte(rng.IntN(3))
		for i := len(blockTestFrame); i < len(p); i++ {
			p[i] = byte(1 + rng.IntN(255))
		}
		if rng.IntN(6) == 0 {
			r.FieldValidity &= FieldValidity(rng.IntN(64))
		}

		return r
	}
}

// genReadCapture returns n records of a generated capture, in ascending seq from a random start with a gap now and then,
// at random times within the first 48 ns of the test hour, or, for half the captures, of it and the next,
// switching hours now and then or every other record or so;
// its timestamps step backward and repeat, and its epoch steps up now and then.
func genReadCapture(t testing.TB, rng *rand.Rand, n int) []Record {
	t.Helper()

	seq, epoch, hour := 1+rng.Uint64N(1000), uint32(rng.IntN(2)), int64(0)
	// switches is how often in 12 records the capture switches hours: never, now and then, or every other record.
	switches := [...]int{0, 0, 2, 6}[rng.IntN(4)]
	out := make([]Record, 0, n)
	for range n {
		if rng.IntN(12) < switches {
			hour = 1 - hour
		}
		out = append(out, genReadRecord(t, rng, seq, blockTestHour+hour*hourNs+rng.Int64N(48), epoch))
		seq += 1 + uint64(max(0, rng.IntN(10)-7))
		if rng.IntN(15) == 0 {
			epoch++
		}
	}

	return out
}

// genVersion changes rec, a record of pack k of its capture, into another version of it, drawn from rng:
// a payload byte changed, its time moved, its message text zeroed, or its direction changed.
func genVersion(rng *rand.Rand, k int, rec *Record) {
	data := rec.Kind == KindData && len(rec.Payload) > len(blockTestFrame)
	switch v := rng.IntN(4); {
	case v == 0 && data:
		p := slices.Clone(rec.Payload)
		p[len(p)-1] += byte(1 + k)
		rec.Payload = p
	case v == 1 && data:
		p := slices.Clone(rec.Payload)
		clear(p[len(blockTestFrame):])
		rec.Payload = p
	case v == 2:
		rec.Dir ^= 1
	default:
		rec.TSUTCNs += 1 + rng.Int64N(5)
	}
}

// genReadOf returns the read generated from seed:
// one to three captures of genReadCapture, each in one to four packs,
// each pack a window of its capture, half the windows sparse,
// or, for a third of the captures, a consumer's segment of a durable bus;
// a few records of a third of the packs changed into other versions (genVersion),
// written with a random codec in blocks closed at random, a fourth of them left unfinalized;
// in a third of the captures, every block from a random seq on carries a header extension of seqExtra,
// which a pack now and then leaves out, so its records differ in record_header_len,
// or changes for one record; a reserved record_flags bit set in one record of a pack now and then;
// now and then a pack with a footer that is not valid, a coverage entry, an exact copy of a pack under another pack_id,
// a copy written with the other codec, or a pack without records.
// A fourth of the reads have a body byte of one block flipped, so that block fails its read.
// The readers are shuffled,
// and the query takes a random time range, kinds, dirs, epochs, HSMS header predicates and Payloads;
// a sixth of the reads take a small MaxHeldBytes.
func genReadOf(t testing.TB, seed uint64) genRead {
	t.Helper()

	return genReadWith(t, seed, nil)
}

// genRetentionReadOf returns the read genReadOf generates from seed,
// each pack but the packs without records given a period an hour before, at or after the test hour,
// and a third of them the extract role, drawn from a source of their own,
// so the read covers several hours, and the hours of an extract's blocks and records.
func genRetentionReadOf(t testing.TB, seed uint64) genRead {
	t.Helper()

	return genReadWith(t, seed, rand.New(rand.NewPCG(seed, 0x686f757273)))
}

// genReadWith returns the read genReadOf generates from seed,
// with the period and role of each pack drawn from roles when set (genRetentionReadOf).
// roles draws from a source of its own, so a read generated with it holds the same records as one without.
func genReadWith(t testing.TB, seed uint64, roles *rand.Rand) genRead {
	t.Helper()

	rng := rand.New(rand.NewPCG(seed, 0x726561642d6d))
	var g genRead
	made := &g.made
	nextID := byte(0)
	packID := func() UUID {
		nextID++
		return UUID{0x70, nextID}
	}
	codecs := [...]Codec{CodecNone, CodecZstd}

	for c := range 1 + rng.IntN(3) {
		capture := UUID{byte(rng.IntN(256)), 0xCA, byte(c)}
		recs := genReadCapture(t, rng, 6+rng.IntN(40))
		n := len(recs)
		wideFrom := uint64(1<<63 - 1)
		if rng.IntN(3) == 0 {
			wideFrom = recs[rng.IntN(n)].Seq
		}

		// parts holds the records of each pack: windows of the capture, or, for a third of the captures,
		// the segments of two or three consumers of a durable bus taking turns of one or two records.
		var parts [][]Record
		if rng.IntN(3) == 0 {
			consumers, turn := 2+rng.IntN(2), 1+rng.IntN(2)
			parts = make([][]Record, consumers)
			for i := range recs {
				parts[i/turn%consumers] = append(parts[i/turn%consumers], recs[i])
			}
		} else {
			windows := make([][]Record, 1+rng.IntN(4))
			for k := range windows {
				start := rng.IntN(n)
				windows[k] = genSparse(rng, recs[start:min(n, start+1+rng.IntN(n))])
			}
			parts = windows
		}

		var clean [][]byte
		for k, part := range parts {
			if len(part) == 0 {
				continue
			}
			window := slices.Clone(part)
			if rng.IntN(3) == 0 {
				for range 1 + rng.IntN(3) {
					genVersion(rng, k, &window[rng.IntN(len(window))])
				}
			}

			open := rng.IntN(4) == 0
			codec := codecs[rng.IntN(2)]
			if codec == CodecZstd {
				made.zstd++
			} else {
				made.none++
			}
			var meta func(m *PackMeta)
			if rng.IntN(10) == 0 {
				from := blockTestHour + rng.Int64N(2*hourNs)
				to := from + rng.Int64N(hourNs)
				meta = func(m *PackMeta) { m.Coverage = []Coverage{{TimeStart: &from, TimeEnd: &to}} }
				made.coverage++
			}
			meta = genRole(roles, meta)
			p := 0.1 + 0.5*rng.Float64()
			steps := make([]footerTestStep, len(window))
			for i := range window {
				steps[i] = footerTestStep{rec: window[i], flush: rng.Float64() < p}
			}
			file := writeRepairPack(t, codec, meta, open, cutAt(steps, wideFrom)).file
			file = genRows(t, rng, file, open, wideFrom)
			clean = append(clean, file)

			if !open && rng.IntN(10) == 0 {
				file = invalidFooterFile(t, file)
				made.invalidFooters++
			}
			if open {
				made.walkedPacks++
			}
			g.files = append(g.files, withIDs(t, file, packID(), capture))
		}
		if rng.IntN(3) == 0 {
			g.files = append(g.files, withIDs(t, clean[rng.IntN(len(clean))], packID(), capture))
			made.byteCopies++
		}
		if rng.IntN(5) == 0 {
			g.files = append(g.files, withIDs(t, otherCodec(t, clean[rng.IntN(len(clean))]), packID(), capture))
			made.codecCopies++
		}
		if rng.IntN(12) == 0 {
			seqStart := recs[rng.IntN(n)].Seq
			empty := writeRepairPack(t, CodecZstd, func(m *PackMeta) { m.SeqStart = seqStart }, false, nil).file
			g.files = append(g.files, withIDs(t, empty, packID(), capture))
			made.emptyPacks++
		}
	}

	if rng.IntN(4) == 0 {
		i := rng.IntN(len(g.files))
		if blocks := mustOpen(t, g.files[i], ReaderOptions{}).Blocks(); len(blocks) > 0 {
			b := blocks[rng.IntN(len(blocks))]
			g.files[i] = flipByte(g.files[i], b.Offset+format.EnvelopeLen+uint64(rng.IntN(int(b.OnDiskLen)-format.EnvelopeLen)))
			made.flipped++
		}
	}
	rng.Shuffle(len(g.files), func(i, j int) { g.files[i], g.files[j] = g.files[j], g.files[i] })

	g.q = genQuery(rng)
	if rng.IntN(6) == 0 {
		g.heldLimit = 512 + rng.Int64N(4<<10)
	}

	return g
}

// genRole returns meta, when set, followed by a change drawn from rng, when set:
// the period moved by an hour back, none or an hour forward, and a third of the time the extract role.
func genRole(rng *rand.Rand, meta func(m *PackMeta)) func(m *PackMeta) {
	if rng == nil {
		return meta
	}
	shift, extract := (rng.Int64N(3)-1)*hourNs, rng.IntN(3) == 0

	return func(m *PackMeta) {
		if meta != nil {
			meta(m)
		}
		m.PeriodStart, m.PeriodEnd = m.PeriodStart+shift, m.PeriodEnd+shift
		if extract {
			m.PackRole, m.ScopeGeneration, m.ExtractFilter = PackRoleExtract, nil, new("generated")
		}
	}
}

// genRows returns file, a pack written by writeRepairPack, open when not finalized,
// with its header rows changed, drawn from rng:
// every block from the seq wideFrom on given seqExtra as its header extension, unless the pack leaves it out now and then,
// one record's extension changed now and then, and a reserved record_flags bit set in one record now and then.
// A finalized pack is reindexed after any change, so its footer states its blocks.
func genRows(t testing.TB, rng *rand.Rand, file []byte, open bool, wideFrom uint64) []byte {
	t.Helper()

	widen := rng.IntN(8) != 0
	apart, reserved := rng.IntN(4) == 0, rng.IntN(8) == 0
	blocks := mustOpen(t, file, ReaderOptions{}).Blocks()
	seqs := make([]uint64, 0, len(blocks))
	for _, b := range blocks {
		seqs = append(seqs, b.FirstSeq)
	}
	if len(seqs) == 0 {
		return file
	}
	pick := seqs[rng.IntN(len(seqs))]
	// wide is the first seq of a block given an extension, whose first record's extension changes when apart is set.
	var wide uint64
	if k := slices.IndexFunc(seqs, func(seq uint64) bool { return seq >= wideFrom }); k >= 0 {
		wide = seqs[k+rng.IntN(len(seqs)-k)]
	}

	changed := false
	file = rebuildPack(t, file, func(_ int, b *testBlock) {
		if widen && b.headers[0].Seq >= wideFrom {
			for j := range b.headers {
				b.headers[j].Extra = seqExtra(b.headers[j].Seq)
			}
			if apart && b.headers[0].Seq == wide {
				b.headers[0].Extra[0]++
			}
			changed = true
		}
		if reserved && b.headers[0].Seq == pick {
			b.patchRows = func(rows []byte, _ int) { rows[43] ^= 1 << 5 }
			changed = true
		}
	})
	if changed && !open {
		file = reindexed(t, file)
	}

	return file
}

// genQuery returns a query drawn from rng: Payloads half the time, and, each now and then,
// a time range over the two test hours, kinds, dirs, epochs, and a predicate on SF, session_id or system bytes,
// with IncludeUnavailable half the time.
func genQuery(rng *rand.Rand) Query {
	q := Query{Payloads: rng.IntN(2) == 0}
	f := &q.Filter
	at := func() *int64 { return new(blockTestHour + rng.Int64N(2)*hourNs + rng.Int64N(56)) }
	if rng.IntN(3) == 0 {
		f.TimeFrom = at()
	}
	if rng.IntN(3) == 0 {
		f.TimeTo = at()
		if f.TimeFrom != nil && *f.TimeFrom > *f.TimeTo {
			f.TimeFrom, f.TimeTo = f.TimeTo, f.TimeFrom
		}
	}
	if rng.IntN(5) == 0 {
		kinds := []Kind{KindData, KindControl, KindTransportEvent, KindAnnotation}
		rng.Shuffle(len(kinds), func(i, j int) { kinds[i], kinds[j] = kinds[j], kinds[i] })
		f.Kinds = kinds[:1+rng.IntN(2)]
	}
	if rng.IntN(5) == 0 {
		f.Dirs = []Dir{[...]Dir{DirHostToEquipment, DirEquipmentToHost, DirLocal}[rng.IntN(3)]}
	}
	if rng.IntN(5) == 0 {
		f.Epochs = []uint32{uint32(rng.IntN(4)), uint32(rng.IntN(4))}
	}
	if rng.IntN(4) == 0 {
		switch rng.IntN(3) {
		case 0:
			f.SF = []SF{{Stream: uint8(1 + rng.IntN(3)), Function: uint8(1 + rng.IntN(4))}}
		case 1:
			f.SessionIDs = []uint16{uint16(1 + rng.IntN(2))}
		default:
			f.SystemBytes = &[4]byte{0xDE, 0xAD, 0xBE, byte(rng.IntN(3))}
		}
		f.IncludeUnavailable = rng.IntN(2) == 0
	}

	return q
}

// genReadStats sums what the generated reads exercised, as the reference computed it.
type genReadStats struct {
	made                                                    genReadFacts
	conflicts, partlySelected, deduplicated, rejected       int
	excluded, kept, spanningHours, walkedBlocks, failedRead int
	// keptExcludable counts the kept clusters with a block the query excludes,
	// and excludedConflicts the excluded clusters holding a conflict, which no read lists.
	keptExcludable, excludedConflicts int
	// apart counts the conflicts by what tells a version from the first.
	apart                                         map[versionsApart]int
	failedWalked, failedIndexedKept               int
	fieldPredicates, includeUnavailable, payloads int
	heldLimitErrors, conflictLimits               int
}

// add adds what ref and g exercise to s.
func (s *genReadStats) add(g *genRead, ref *mergeIterateReference) {
	m, f := &s.made, &g.made
	m.walkedPacks += f.walkedPacks
	m.invalidFooters += f.invalidFooters
	m.byteCopies += f.byteCopies
	m.codecCopies += f.codecCopies
	m.flipped += f.flipped
	m.emptyPacks += f.emptyPacks
	m.coverage += f.coverage
	m.zstd += f.zstd
	m.none += f.none

	if g.q.Filter.hasFieldPredicate() {
		s.fieldPredicates++
		if g.q.Filter.IncludeUnavailable {
			s.includeUnavailable++
		}
	}
	if g.q.Payloads {
		s.payloads++
	}
	for _, cs := range ref.clusters {
		for _, c := range cs {
			if c.excluded {
				s.excluded++
				if slices.ContainsFunc(refVersionsOf(c), func(vs []refVersion) bool { return len(vs) > 1 }) {
					s.excludedConflicts++
				}

				continue
			}
			s.kept++
			if slices.ContainsFunc(c.blocks, func(b *refBlock) bool { return refExcludes(&g.q.Filter, b) }) {
				s.keptExcludable++
			}
			hours := map[int64]bool{}
			for _, b := range c.blocks {
				hours[(b.tsMin-blockTestHour)/hourNs] = true
			}
			if len(hours) > 1 {
				s.spanningHours++
			}
			for _, vs := range c.seqs {
				s.addSeq(ref, vs)
			}
		}
	}
	ref.eachBlock(func(b *refBlock, kept bool) {
		if !b.indexed {
			s.walkedBlocks++
		}
		if b.failed {
			s.failedRead++
			switch {
			case !b.indexed:
				s.failedWalked++
			case kept:
				s.failedIndexedKept++
			default:
				// A failed block of an excluded cluster, never read.
			}
		}
	})
}

// versionsApart holds what tells two versions of a record apart, a bit each.
type versionsApart uint8

// What tells two versions of a record apart.
const (
	// apartHeaderLen: their record_header_len.
	apartHeaderLen versionsApart = 1 << iota
	// apartExtension: their header extension alone.
	apartExtension
	// apartStoredBits: bits of their first 44 header bytes that no field of the record shows, a reserved bit.
	apartStoredBits
	// apartTime: their ts_utc_ns.
	apartTime
	// apartField: another field of their record headers.
	apartField
	// apartMasked: their payloads alone, the message text of one zeroed.
	apartMasked
	// apartPayload: their payloads alone, otherwise; the last bit.
	apartPayload
)

// String names what the bit tells apart.
func (v versionsApart) String() string {
	switch v {
	case apartHeaderLen:
		return "record_header_len"
	case apartExtension:
		return "extension"
	case apartStoredBits:
		return "stored bits"
	case apartTime:
		return "time"
	case apartField:
		return "header field"
	case apartMasked:
		return "masked text"
	case apartPayload:
		return "payload"
	default:
		return fmt.Sprintf("versionsApart(%d)", uint8(v))
	}
}

// apartIn returns what tells a and b, two versions of one record, apart.
func apartIn(a, b *refCopy) versionsApart {
	var out versionsApart
	ra, rb := refRecord(a, false), refRecord(b, false)
	switch {
	case len(a.row) != len(b.row):
		out |= apartHeaderLen
	case !bytes.Equal(a.row[format.RecordHeaderLen:], b.row[format.RecordHeaderLen:]):
		out |= apartExtension
	default:
		// The same extension, or none.
	}
	switch {
	case ra.TSUTCNs != rb.TSUTCNs:
		out |= apartTime
	case !reflect.DeepEqual(ra, rb):
		out |= apartField
	case !bytes.Equal(a.row[:format.RecordHeaderLen], b.row[:format.RecordHeaderLen]):
		out |= apartStoredBits
	default:
		// The same first 44 header bytes.
	}
	if bytes.Equal(a.payload, b.payload) {
		return out
	}
	text := len(blockTestFrame)
	masked := func(p []byte) bool {
		return len(p) > text && !slices.ContainsFunc(p[text:], func(c byte) bool { return c != 0 })
	}
	if bytes.Equal(a.payload[:min(text, len(a.payload))], b.payload[:min(text, len(b.payload))]) && masked(a.payload) != masked(b.payload) {
		return out | apartMasked
	}

	return out | apartPayload
}

// addSeq adds to s what the seq of a compared cluster whose versions are vs exercises.
func (s *genReadStats) addSeq(ref *mergeIterateReference, vs []refVersion) {
	n := len(ref.selected(vs))
	if n == 0 {
		s.rejected++
	}
	switch {
	case len(vs) > 1:
		s.conflicts++
		if n < len(vs) {
			s.partlySelected++
		}
		var seen versionsApart
		for k := 1; k < len(vs); k++ {
			seen |= apartIn(&vs[0].rec, &vs[k].rec)
		}
		for bit := versionsApart(1); bit <= apartPayload; bit <<= 1 {
			if seen&bit != 0 {
				if s.apart == nil {
					s.apart = map[versionsApart]int{}
				}
				s.apart[bit]++
			}
		}
	case len(vs[0].holders) > 1:
		s.deduplicated++
	default:
		// A seq one pack holds.
	}
}

// checkGeneratedRead reads g in each order and requires the reads to agree with the reference (requireReference)
// and with MergeIterate's own (requirePublicRead);
// with the default MaxHeldBytes, it also reads under a MaxConflicts of the number of conflicts, which succeeds,
// and of one less, which ends with ErrReadLimit, the conflicts listed a prefix of the reference's.
// It returns the number of reads that ended with ErrReadLimit for MaxHeldBytes, and those under a conflict limit.
func checkGeneratedRead(t *testing.T, g *genRead, ref *mergeIterateReference, orders []Order) (heldErrs, limited int) {
	t.Helper()

	readers := ref.readers
	for _, order := range orders {
		opts := MergeIterateOptions{Order: order, MaxHeldBytes: g.heldLimit}
		out := readChecked(t, readers, g.q, opts)
		requireReference(t, ref, opts, out)
		requirePublicRead(t, readers, g.q, opts, out)
		if out.err != nil {
			heldErrs++
			continue
		}

		n := len(out.res.Conflicts)
		if g.heldLimit != 0 || n < 2 {
			continue
		}
		limited++
		opts.MaxConflicts = n
		exact := readChecked(t, readers, g.q, opts)
		require.NoError(t, exact.err, "a conflict limit reached exactly")
		require.Equal(t, out.items, exact.items)
		require.Equal(t, out.res, exact.res)

		opts.MaxConflicts = n - 1
		over := readChecked(t, readers, g.q, opts)
		require.ErrorIs(t, over.err, ErrReadLimit, "a conflict limit exceeded by one")
		require.ErrorContains(t, over.err, fmt.Sprintf("exceeds MaxConflicts %d", n-1))
		require.Equal(t, out.res.Conflicts[:n-1], over.res.Conflicts)
		requireReference(t, ref, opts, over)
	}

	return heldErrs, limited
}

// genReads is the number of reads TestMergeIterateGeneratedReads generates, and genReadsLong the number without -short.
const (
	genReads     = 96
	genReadsLong = 512
)

// TestMergeIterateGeneratedReads generates reads over several packs (genReadOf), reads each in both orders,
// and requires every read to agree with the reference (checkGeneratedRead):
// the records yielded with their packs, blocks and conflict marks, the conflicts listed, the defects,
// the full-block reads of each block, the footer errors, nothing held after the read and every block buffer dropped.
// Together the reads exercise every kind of version, duplicate, exclusion, walked and failed block, filter and limit,
// which it checks when every read ran, not when a -run pattern selects some of them.
func TestMergeIterateGeneratedReads(t *testing.T) {
	t.Parallel()

	n := genReadsLong
	if testing.Short() {
		n = genReads
	}
	var mu sync.Mutex
	var sum genReadStats
	ran := 0
	t.Cleanup(func() {
		if ran != n {
			return
		}
		counts := map[string]int{
			"walked packs": sum.made.walkedPacks, "invalid footers": sum.made.invalidFooters,
			"byte copies": sum.made.byteCopies, "codec copies": sum.made.codecCopies,
			"flipped blocks": sum.made.flipped, "empty packs": sum.made.emptyPacks, "coverage entries": sum.made.coverage,
			"zstd packs": sum.made.zstd, "none packs": sum.made.none,
			"conflicts": sum.conflicts, "partly selected conflicts": sum.partlySelected, "deduplicated seqs": sum.deduplicated,
			"kept clusters with an excludable block": sum.keptExcludable, "excluded clusters with a conflict": sum.excludedConflicts,
			"failed walked blocks": sum.failedWalked, "failed indexed blocks read": sum.failedIndexedKept,
			"rejected seqs": sum.rejected, "excluded clusters": sum.excluded, "kept clusters": sum.kept,
			"clusters spanning hours": sum.spanningHours, "walked blocks": sum.walkedBlocks, "failed blocks": sum.failedRead,
			"field predicates": sum.fieldPredicates, "IncludeUnavailable": sum.includeUnavailable, "payloads": sum.payloads,
			"MaxHeldBytes errors": sum.heldLimitErrors, "conflict limits": sum.conflictLimits,
		}
		for bit := versionsApart(1); bit <= apartPayload; bit <<= 1 {
			counts["conflicts apart in "+bit.String()] = sum.apart[bit]
		}
		t.Logf("over %d reads: %v", n, counts)
		for name, v := range counts {
			assert.Positive(t, v, name)
		}
	})
	for seed := range uint64(n) {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			t.Parallel()

			g := genReadOf(t, seed)
			ref := newMergeIterateReference(t, openAll(t, g.files...), g.q)
			heldErrs, limited := checkGeneratedRead(t, &g, ref, []Order{OrderCapture, OrderTime})

			mu.Lock()
			defer mu.Unlock()
			ran++
			sum.add(&g, ref)
			sum.heldLimitErrors += heldErrs
			sum.conflictLimits += limited
		})
	}
}

// Bits of the control byte of FuzzMergeIterate and FuzzMergeIterateGenerated, which chooses the read's options.
const (
	// fuzzReadTime reads in time order, else in capture order.
	fuzzReadTime = 1 << 0
	// fuzzReadPayloads flips Query.Payloads.
	fuzzReadPayloads = 1 << 1
	// fuzzReadSmallHeld sets MaxHeldBytes to 4 KiB, and fuzzReadOneConflict MaxConflicts to 1.
	fuzzReadSmallHeld   = 1 << 2
	fuzzReadOneConflict = 1 << 3
	// fuzzReadQueryShift places the bits that choose FuzzMergeIterate's query among fuzzIterateQueries.
	fuzzReadQueryShift = 4
)

// fuzzReadOptions returns the read options the control byte ctl chooses, and q with Payloads flipped when ctl says so.
func fuzzReadOptions(q Query, ctl uint8) (Query, MergeIterateOptions) {
	opts := MergeIterateOptions{Order: OrderCapture}
	if ctl&fuzzReadTime != 0 {
		opts.Order = OrderTime
	}
	if ctl&fuzzReadPayloads != 0 {
		q.Payloads = !q.Payloads
	}
	if ctl&fuzzReadSmallHeld != 0 {
		opts.MaxHeldBytes = 4 << 10
	}
	if ctl&fuzzReadOneConflict != 0 {
		opts.MaxConflicts = 1
	}

	return q, opts
}

// fuzzReadSeeds returns seed packs of FuzzMergeIterate, three per input:
// those of a few generated reads, and packs already damaged beside valid ones:
// a footer that misstates its block, a walked block that fails its read,
// and blocks stating dimensions that cannot be consistent.
func fuzzReadSeeds(t testing.TB) [][3][]byte {
	t.Helper()

	var out [][3][]byte
	for seed := range uint64(6) {
		var in [3][]byte
		copy(in[:], genReadOf(t, seed).files)
		out = append(out, in)
	}

	h := blockTestHour
	walked := planTestPack(t, seg1, captureLow, true, slices.Concat(timedBlocks(h, []uint64{1, 2}), timedBlocks(h+hourNs, []uint64{3, 4})))
	info := mustOpen(t, walked, ReaderOptions{}).Blocks()[1]
	archive := planTestPack(t, seg0, captureLow, false, slices.Concat(timedBlocks(h, []uint64{1, 2, 3}), timedBlocks(h+hourNs, []uint64{4})))

	return append(out,
		[3][]byte{misindexedPack(t, seg2, captureLow), planTestPack(t, seg0, captureLow, false, seqBlocks([]uint64{9, 10, 11}, []uint64{12, 13}))},
		[3][]byte{flipByte(walked, info.Offset+format.EnvelopeLen+2), archive},
		[3][]byte{withIDs(t, hugeSectionPack(t, false), seg2, captureHigh), withIDs(t, hugeSectionPack(t, true), seg3, captureHigh), archive},
	)
}

// FuzzMergeIterate reads up to three mutated packs with options and a query a control byte chooses.
// Packs that do not open are left to FuzzOpen, and inputs with two packs of one pack_id are skipped.
// Every read ends without error or with ErrReadLimit, holding nothing after it, its block buffers dropped;
// every Item names a pack given and a block of it;
// in time order the records come in ascending (ts_utc_ns, capture_id, seq),
// and in capture order each capture's in ascending seq, unless a block read disagrees with its index;
// a second read yields and returns the same.
// A non-zero sched draws a schedule of the retention boundary (newBoundaryScript),
// under which the read agrees with the reference of a read under a boundary (requireScriptedRead).
// Every section of a pack is under a CRC, so most mutations end at Open;
// FuzzMergeIterateGenerated mutates valid reads instead.
func FuzzMergeIterate(f *testing.F) {
	for i, in := range fuzzReadSeeds(f) {
		f.Add(in[0], in[1], in[2], uint8(i), uint64(0))
		f.Add(in[0], in[1], in[2], uint8(i), uint64(1+i))
	}

	queries := fuzzIterateQueries()
	f.Fuzz(func(t *testing.T, a, b, c []byte, ctl uint8, sched uint64) {
		var readers []*Reader
		var ats []*scriptedReaderAt
		ids := map[UUID]bool{}
		for _, data := range [][]byte{a, b, c} {
			if len(data) == 0 {
				continue
			}
			r, at, err := openScripted(t.Context(), data, len(readers), fuzzMergeReader)
			if err != nil {
				continue
			}
			id := UUID(r.hdr.PackID)
			if ids[id] {
				return
			}
			ids[id] = true
			readers, ats = append(readers, r), append(ats, at)
		}
		if len(readers) == 0 {
			return
		}
		q, opts := fuzzReadOptions(queries[int(ctl>>fuzzReadQueryShift)%len(queries)], ctl)

		out := readChecked(t, readers, q, opts)
		if out.err != nil {
			require.ErrorIs(t, out.err, ErrReadLimit)
		}
		disordered := slices.ContainsFunc(out.res.Incomplete, func(d Defect) bool { return d.Reason == ReasonIndexMismatch })
		last := map[UUID]uint64{}
		for i, it := range out.items {
			require.True(t, it.pack >= 0 && it.pack < len(readers), "item %d: pack %d", i, it.pack)
			require.True(t, it.block >= 0 && it.block < len(readers[it.pack].blocks), "item %d: block %d of pack %d", i, it.block, it.pack)
			if disordered {
				continue
			}
			capture := UUID(readers[it.pack].hdr.CaptureID)
			if opts.Order == OrderCapture {
				require.GreaterOrEqual(t, it.rec.Seq, last[capture], "item %d: seqs of capture %s ascending", i, capture)
				last[capture] = it.rec.Seq

				continue
			}
			if i > 0 {
				prev := out.items[i-1]
				prevCapture := UUID(readers[prev.pack].hdr.CaptureID)
				require.LessOrEqual(t, cmp.Or(
					cmp.Compare(prev.rec.TSUTCNs, it.rec.TSUTCNs),
					bytes.Compare(prevCapture[:], capture[:]),
					cmp.Compare(prev.rec.Seq, it.rec.Seq),
				), 0, "item %d: in time order", i)
			}
		}

		again := readChecked(t, readers, q, opts)
		require.Equal(t, out.err == nil, again.err == nil)
		require.Equal(t, out.items, again.items)
		require.Equal(t, out.res.Conflicts, again.res.Conflicts)
		require.Equal(t, defectKeys(out.res.Incomplete), defectKeys(again.res.Incomplete))
		require.Equal(t, out.reads, again.reads)

		if sched != 0 {
			checkScripted(t, retentionFactsOf(t, readers), mergeScripted(t, readers, ats, q, opts), sched)
		}
	})
}

// FuzzMergeIterateGenerated reads the read genReadOf generates from a seed, with options a control byte chooses,
// so that mutations explore valid reads, which the CRCs of FuzzMergeIterate's packs leave it few of,
// and requires the read to agree with the reference (requireReference).
// A non-zero sched reads the read genRetentionReadOf generates instead,
// and also under the schedule of the retention boundary sched draws (newBoundaryScript),
// requiring it to agree with the reference of a read under a boundary (requireScriptedRead).
func FuzzMergeIterateGenerated(f *testing.F) {
	for i, ctl := range []uint8{0, fuzzReadTime, fuzzReadPayloads, fuzzReadTime | fuzzReadPayloads,
		fuzzReadSmallHeld, fuzzReadTime | fuzzReadSmallHeld, fuzzReadOneConflict, fuzzReadTime | fuzzReadOneConflict,
	} {
		f.Add(uint64(i), ctl, uint64(0))
		f.Add(uint64(i), ctl, uint64(1+i))
	}

	f.Fuzz(func(t *testing.T, seed uint64, ctl uint8, sched uint64) {
		g := genReadOf(t, seed)
		if sched != 0 {
			g = genRetentionReadOf(t, seed)
		}
		q, opts := fuzzReadOptions(g.q, ctl)
		if opts.MaxHeldBytes == 0 {
			opts.MaxHeldBytes = g.heldLimit
		}
		readers, ats := openScriptedAll(t, g.files...)
		ref := newMergeIterateReference(t, readers, q)
		requireReference(t, ref, opts, readChecked(t, readers, q, opts))
		if sched != 0 {
			checkScripted(t, retentionFactsOf(t, readers), mergeScripted(t, readers, ats, q, opts), sched)
		}
	})
}

// TestMergeIterateConflictLimit reads, through MergeIterate in each order,
// a pack beside a second whose copies of seqs 2, 4 and 6 differ,
// under a MaxConflicts of three, the number of conflicts, and of two.
// Under three the read succeeds and lists the three.
// Under two it ends with ErrReadLimit at seq 6, the Result listing the two conflicts found before.
// Capture order has yielded the records before seq 6.
// Time order has yielded the records of the first cluster, seqs 1 to 3, all before the ts_min of the second,
// and none of the second, which it reads whole before yielding any.
func TestMergeIterateConflictLimit(t *testing.T) {
	t.Parallel()

	steps := seqBlocks([]uint64{1, 2, 3}, []uint64{4, 5, 6})
	readers := openAll(t,
		planTestPack(t, seg0, captureLow, false, steps),
		planTestPack(t, seg1, captureLow, false, changed(steps, 1, 2, 4, 6)),
	)
	conflictAt := func(seq uint64) Conflict {
		return Conflict{CaptureID: captureLow, Seq: seq, Versions: [][]UUID{{seg0}, {seg1}}}
	}
	versions := func(block int, seq uint64) []yieldView {
		return []yieldView{{block: block, seq: seq, conflict: true}, {pack: 1, block: block, seq: seq, conflict: true}}
	}
	all := slices.Concat([]yieldView{{seq: 1}}, versions(0, 2), []yieldView{{seq: 3}}, versions(1, 4), []yieldView{{block: 1, seq: 5}}, versions(1, 6))

	for _, order := range []Order{OrderCapture, OrderTime} {
		t.Run(order.String(), func(t *testing.T) {
			t.Parallel()

			items, res, err := collectMergeIterate(t.Context(), readers, Query{}, MergeIterateOptions{Order: order, MaxConflicts: 3})
			require.NoError(t, err)
			assert.Equal(t, all, yieldViews(items))
			assert.Equal(t, []Conflict{conflictAt(2), conflictAt(4), conflictAt(6)}, res.Conflicts)
			assert.True(t, res.Complete())

			items, res, err = collectMergeIterate(t.Context(), readers, Query{}, MergeIterateOptions{Order: order, MaxConflicts: 2})
			require.ErrorIs(t, err, ErrReadLimit)
			require.ErrorContains(t, err, "the conflict at seq 6 of capture "+captureLow.String()+" exceeds MaxConflicts 2:")
			assert.Equal(t, []Conflict{conflictAt(2), conflictAt(4)}, res.Conflicts)
			want := all[:len(all)-2]
			if order == OrderTime {
				want = all[:4]
			}
			assert.Equal(t, want, yieldViews(items))
		})
	}
}

// TestMergeIterateMaskedCopies reads, in each order, a pack beside a second pack of its capture
// whose copies of seqs 2 and 5 have their message text zeroed, the frame and its length kept,
// as an extract masks a record (the tracepack semantics specification §8):
// each masked seq is a conflict of two versions, both yielded and marked, the masked one from the second pack,
// and every other record is yielded once, unmarked, from the first.
func TestMergeIterateMaskedCopies(t *testing.T) {
	t.Parallel()

	var steps []footerTestStep
	for seq := uint64(1); seq <= 6; seq++ {
		steps = append(steps, footerTestStep{rec: bigDataRecord(seq, blockTestHour+int64(seq), 6), flush: seq%3 == 0})
		copy(steps[len(steps)-1].rec.Payload[len(blockTestFrame):], "secret")
	}
	masked := slices.Clone(steps)
	for i := range masked {
		if seq := masked[i].rec.Seq; seq == 2 || seq == 5 {
			p := slices.Clone(masked[i].rec.Payload)
			clear(p[len(blockTestFrame):])
			masked[i].rec.Payload = p
		}
	}
	readers := openAll(t, planTestPack(t, seg0, captureLow, false, steps), planTestPack(t, seg1, captureLow, false, masked))
	versions := func(block int, seq uint64) []yieldView {
		return []yieldView{{block: block, seq: seq, conflict: true}, {pack: 1, block: block, seq: seq, conflict: true}}
	}
	want := slices.Concat([]yieldView{{seq: 1}}, versions(0, 2), []yieldView{{seq: 3}, {block: 1, seq: 4}}, versions(1, 5), []yieldView{{block: 1, seq: 6}})

	for _, order := range []Order{OrderCapture, OrderTime} {
		t.Run(order.String(), func(t *testing.T) {
			t.Parallel()

			items, res, err := collectMergeIterate(t.Context(), readers, Query{Payloads: true}, MergeIterateOptions{Order: order})
			require.NoError(t, err)
			assert.Equal(t, want, yieldViews(items))
			for _, it := range items {
				text := string(it.rec.Payload[len(blockTestFrame):])
				if it.pack == 1 {
					assert.Equal(t, "\x00\x00\x00\x00\x00\x00", text, "seq %d", it.rec.Seq)
				} else {
					assert.Equal(t, "secret", text, "seq %d", it.rec.Seq)
				}
			}
			assert.Equal(t, []Conflict{
				{CaptureID: captureLow, Seq: 2, Versions: [][]UUID{{seg0}, {seg1}}},
				{CaptureID: captureLow, Seq: 5, Versions: [][]UUID{{seg0}, {seg1}}},
			}, res.Conflicts)
			assert.True(t, res.Complete())
		})
	}
}
