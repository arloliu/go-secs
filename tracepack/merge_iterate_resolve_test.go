package tracepack

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"math/big"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// candView is what a candidate names: its record's seq, its representative's pack and block, its version and conflict flag.
type candView struct {
	seq     uint64
	pack    int
	block   int
	version int
	// conflict is the candidate's Conflict flag.
	conflict bool
}

// planResolve plans the read of q over files in capture order, MaxConflicts set to maxConflicts when positive,
// and returns its run and the full-block reads of each block by (reader, block), counted on.
// Each read is passed to hook when hook is set.
func planResolve(t testing.TB, ctx context.Context, q Query, maxConflicts int, hook func(pack, block int), files ...[]byte,
) (*mergeIterateRun, map[[2]int]int) {
	t.Helper()

	opts := MergeIterateOptions{Order: OrderCapture, MaxConflicts: maxConflicts}
	p, reads, err := planReads(t, ctx, openAll(t, files...), q, opts, hook)
	require.NoError(t, err)

	return newMergeIterateRun(p, &q), reads
}

// resolvedCluster is what resolveCluster resolved of a cluster.
type resolvedCluster struct {
	cr *clusterResolver
	// cands holds the candidates made, still pending.
	cands []candidate
	// seqs is the number of seqs resolved.
	seqs int
}

// resolveCluster resolves the first cluster of the first capture of run's plan to its end or its first error,
// and returns what it resolved, with the error.
func resolveCluster(ctx context.Context, run *mergeIterateRun) (resolvedCluster, error) {
	pc := &run.p.captures[0]
	rs := resolvedCluster{cr: newClusterResolver(run, pc.id, &pc.clusters[0])}
	for {
		var ok bool
		var err error
		rs.cands, ok, err = rs.cr.next(ctx, rs.cands)
		if err != nil || !ok {
			return rs, err
		}
		rs.seqs++
	}
}

// releaseAll releases every candidate, closes cr and requires nothing of the read to stay held.
func releaseAll(t *testing.T, run *mergeIterateRun, cr *clusterResolver, cands []candidate) {
	t.Helper()

	for i := range cands {
		cands[i].release(&run.p.loader)
	}
	cr.close()
	cr.close()
	assert.Zero(t, run.p.loader.budget.held, "nothing stays held")
	for pos, h := range cr.held {
		assert.Nil(t, h, "position %d", pos)
	}
}

// itemOf returns a copy of the Item of c for run's query.
func itemOf(run *mergeIterateRun, c *candidate) iterItem {
	var it Item
	c.item(run.q, &it)

	return copyItem(&it)
}

// viewsOf returns what each candidate names, in order.
func viewsOf(run *mergeIterateRun, cands []candidate) []candView {
	out := make([]candView, len(cands))
	for i := range cands {
		it := itemOf(run, &cands[i])
		out[i] = candView{seq: it.rec.Seq, pack: it.pack, block: it.block, version: cands[i].version, conflict: it.conflict}
	}

	return out
}

// plainViews returns the views of candidates without conflict, version 0, of the seqs of seqs, each from pack and block.
func plainViews(pack, block int, seqs ...uint64) []candView {
	out := make([]candView, len(seqs))
	for i, seq := range seqs {
		out[i] = candView{seq: seq, pack: pack, block: block}
	}

	return out
}

// misindexedPack returns a finalized pack id of capture with a valid footer that misstates its one block:
// the block holds seqs 10, 12, 14 and 16, the record of seq s at blockTestHour + s,
// while its F-2 entry, its F-3 summary and the trailer state seqs 10 to 13.
func misindexedPack(t testing.TB, id, capture UUID) []byte {
	t.Helper()

	file := reindexedWith(t, planTestPack(t, id, capture, false, seqBlocks([]uint64{10, 12, 14, 16})), func(_ int, s *blockSummary) {
		s.lastSeq = 13
		s.seqRanges = []seqRange{{first: 10, last: 13}}
		require.Len(t, s.epochs, 1)
		s.epochs[0].seqLast = 13
	})
	tr := layoutOf(t, file).tr
	tr.LastSeq = 13

	return format.AppendTrailer(file[:len(file)-format.TrailerLen], &tr)
}

// heldSize returns the bytes the loader reserves for the first block of file.
func heldSize(t testing.TB, file []byte) int64 {
	t.Helper()

	s, def := mustOpen(t, file, ReaderOptions{}).preflightBlock(0)
	require.Nil(t, def)

	return s.total
}

// TestClusterResolverIdenticalCopies resolves records that two packs hold identically:
// each is one candidate, without conflict, from its representative, the first block holding it in cluster order;
// a block equal byte for byte to an open one is dropped at load and holds nothing.
func TestClusterResolverIdenticalCopies(t *testing.T) {
	t.Parallel()

	t.Run("blocks of other boundaries", func(t *testing.T) {
		t.Parallel()

		// Cluster order: a0 (10..13), b0 (10..12), b1 (13..15), a1 (14..17), b2 (16, 17).
		a := mergePack(t, seg0, nil, hourSteps(10, 17, 4))
		b := mergePack(t, seg1, nil, hourSteps(10, 17, 3))
		run, reads := planResolve(t, t.Context(), Query{Payloads: true}, 0, nil, a, b)
		require.Len(t, run.p.captures, 1)
		require.Len(t, run.p.captures[0].clusters, 1)

		rs, err := resolveCluster(t.Context(), run)
		cr, cands, n := rs.cr, rs.cands, rs.seqs
		require.NoError(t, err)
		assert.Equal(t, 8, n)
		want := slices.Concat(plainViews(0, 0, 10, 11, 12, 13), plainViews(1, 1, 14, 15), plainViews(0, 1, 16, 17))
		assert.Equal(t, want, viewsOf(run, cands))
		records := recordsOf(t, a)
		for i := range cands {
			it := itemOf(run, &cands[i])
			assert.Equal(t, records[i].rec, it.rec, "seq %d: the record as stored", records[i].rec.Seq)
			assert.Equal(t, records[i].extra, it.extra)
		}
		assert.Empty(t, run.p.res.Conflicts)
		assert.Empty(t, run.p.res.Incomplete)
		assert.Equal(t, map[[2]int]int{{0, 0}: 1, {0, 1}: 1, {1, 0}: 1, {1, 1}: 1, {1, 2}: 1}, reads, "each block read once")
		releaseAll(t, run, cr, cands)
	})

	t.Run("a whole duplicate block", func(t *testing.T) {
		t.Parallel()

		a := mergePack(t, seg0, nil, hourSteps(10, 13, 4))
		dup := withIDs(t, a, seg1, viewCapture)
		run, reads := planResolve(t, t.Context(), Query{}, 0, nil, a, dup)
		pc := &run.p.captures[0]
		cr := newClusterResolver(run, pc.id, &pc.clusters[0])

		cands, ok, err := cr.next(t.Context(), nil)
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, map[[2]int]int{{0, 0}: 1, {1, 0}: 1}, reads, "both blocks read")
		assert.Equal(t, heldSize(t, a), run.p.loader.budget.held, "the duplicate holds nothing")
		assert.NotNil(t, cr.held[0])
		assert.Nil(t, cr.held[1], "the duplicate is not open")
		require.Len(t, cr.cur.open, 1)
		assert.Equal(t, []int{0, 1}, cr.cur.open[0].origins, "the duplicate's pack holds the block's records")

		for ok {
			cands, ok, err = cr.next(t.Context(), cands)
			require.NoError(t, err)
		}
		assert.Equal(t, plainViews(0, 0, 10, 11, 12, 13), viewsOf(run, cands))
		assert.Empty(t, run.p.res.Conflicts)
		releaseAll(t, run, cr, cands)
	})
}

// TestClusterResolverConflicts resolves seqs whose copies differ:
// each difference of the header row as stored or of the payload is a version,
// the conflict lists the packs holding each version once, in reader order, versions in the order of their representatives,
// and each version is a candidate from its representative, marked as a conflict.
func TestClusterResolverConflicts(t *testing.T) {
	t.Parallel()

	steps := hourSteps(10, 13, 4)
	a := mergePack(t, seg0, nil, steps)
	conflictAt := func(seq uint64, versions ...[]UUID) Conflict {
		return Conflict{CaptureID: viewCapture, Seq: seq, Versions: versions}
	}
	// patchSeq returns file as the pack id, rebuilt with patch applied to the header row of seq 12, then reindexed.
	patchSeq := func(id UUID, file []byte, patch func(row []byte)) []byte {
		return reindexed(t, rebuildPack(t, withIDs(t, file, id, viewCapture), func(_ int, b *testBlock) {
			b.patchRows = func(rows []byte, rhl int) {
				for off := 0; off < len(rows); off += rhl {
					if binary.LittleEndian.Uint64(rows[off:]) == 12 {
						patch(rows[off : off+rhl])
					}
				}
			}
		}))
	}
	// extended returns file with every record header given seqExtra as its extension area, that of seq 12 changed by delta.
	extended := func(file []byte, delta byte) []byte {
		return reindexed(t, rebuildPack(t, file, func(_ int, b *testBlock) {
			for j := range b.headers {
				b.headers[j].Extra = seqExtra(b.headers[j].Seq)
				if b.headers[j].Seq == 12 {
					b.headers[j].Extra[0] += delta
				}
			}
		}))
	}
	plain := func(seqs ...uint64) []candView { return plainViews(0, 0, seqs...) }
	at12 := func(versions ...candView) []candView {
		return slices.Concat(plain(10, 11), versions, plain(13))
	}

	tests := []struct {
		name      string
		files     [][]byte
		conflicts []Conflict
		views     []candView
	}{
		{
			name:      "two versions, the payload alone apart",
			files:     [][]byte{a, mergePack(t, seg1, nil, changed(steps, 1, 12))},
			conflicts: []Conflict{conflictAt(12, []UUID{seg0}, []UUID{seg1})},
			views:     at12(candView{seq: 12, conflict: true}, candView{seq: 12, pack: 1, version: 1, conflict: true}),
		},
		{
			// seg3 holds seg1's version in blocks of other boundaries, so its blocks are compared, not dropped.
			name: "three versions, one held twice",
			files: [][]byte{
				a,
				mergePack(t, seg1, nil, changed(steps, 1, 12)),
				mergePack(t, seg2, nil, changed(steps, 2, 12)),
				mergePack(t, seg3, nil, changed(hourSteps(10, 13, 2), 1, 12)),
			},
			conflicts: []Conflict{conflictAt(12, []UUID{seg0}, []UUID{seg1, seg3}, []UUID{seg2})},
			views: at12(
				candView{seq: 12, conflict: true},
				candView{seq: 12, pack: 1, version: 1, conflict: true},
				candView{seq: 12, pack: 2, version: 2, conflict: true},
			),
		},
		{
			// seg1's block is seg0's byte for byte, dropped at load as its duplicate; seg1 still holds the version.
			name:      "a duplicate's pack",
			files:     [][]byte{a, withIDs(t, a, seg1, viewCapture), mergePack(t, seg2, nil, changed(steps, 1, 12))},
			conflicts: []Conflict{conflictAt(12, []UUID{seg0, seg1}, []UUID{seg2})},
			views:     at12(candView{seq: 12, conflict: true}, candView{seq: 12, pack: 2, version: 1, conflict: true}),
		},
		{
			// The blocks start at 10 (seg2), 11 (seg1) and 12 (seg0):
			// seg2's version comes first, held by seg0 too, and the ids follow the readers' order.
			name: "versions by first block, holders by reader order",
			files: [][]byte{
				mergePack(t, seg0, nil, seqBlocks([]uint64{12, 13})),
				mergePack(t, seg1, nil, changed(seqBlocks([]uint64{11, 12}), 1, 12)),
				mergePack(t, seg2, nil, seqBlocks(seqRun(10, 13))),
			},
			conflicts: []Conflict{conflictAt(12, []UUID{seg0, seg2}, []UUID{seg1})},
			views: []candView{
				{seq: 10, pack: 2}, {seq: 11, pack: 2},
				{seq: 12, pack: 2, conflict: true}, {seq: 12, pack: 1, version: 1, conflict: true},
				{seq: 13, pack: 2},
			},
		},
		{
			name:      "a reserved record_flags bit apart",
			files:     [][]byte{a, patchSeq(seg1, a, func(row []byte) { row[43] ^= 1 << 5 })},
			conflicts: []Conflict{conflictAt(12, []UUID{seg0}, []UUID{seg1})},
			views:     at12(candView{seq: 12, conflict: true}, candView{seq: 12, pack: 1, version: 1, conflict: true}),
		},
		{
			name:      "the header extension alone apart",
			files:     [][]byte{extended(a, 0), withIDs(t, extended(a, 1), seg1, viewCapture)},
			conflicts: []Conflict{conflictAt(12, []UUID{seg0}, []UUID{seg1})},
			views:     at12(candView{seq: 12, conflict: true}, candView{seq: 12, pack: 1, version: 1, conflict: true}),
		},
		{
			name:  "another record_header_len",
			files: [][]byte{a, widenFile(t, mergePack(t, seg1, nil, steps), allBlocks)},
			conflicts: []Conflict{
				conflictAt(10, []UUID{seg0}, []UUID{seg1}), conflictAt(11, []UUID{seg0}, []UUID{seg1}),
				conflictAt(12, []UUID{seg0}, []UUID{seg1}), conflictAt(13, []UUID{seg0}, []UUID{seg1}),
			},
			views: []candView{
				{seq: 10, conflict: true}, {seq: 10, pack: 1, version: 1, conflict: true},
				{seq: 11, conflict: true}, {seq: 11, pack: 1, version: 1, conflict: true},
				{seq: 12, conflict: true}, {seq: 12, pack: 1, version: 1, conflict: true},
				{seq: 13, conflict: true}, {seq: 13, pack: 1, version: 1, conflict: true},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Payloads is false: the payload is compared all the same.
			run, _ := planResolve(t, t.Context(), Query{}, 0, nil, tt.files...)
			require.Len(t, run.p.captures[0].clusters, 1)
			rs, err := resolveCluster(t.Context(), run)
			cr, cands := rs.cr, rs.cands
			require.NoError(t, err)
			assert.Equal(t, tt.conflicts, run.p.res.Conflicts)
			assert.Equal(t, tt.views, viewsOf(run, cands))
			for i := range cands {
				assert.Nil(t, itemOf(run, &cands[i]).rec.Payload, "no payload without Payloads")
			}
			releaseAll(t, run, cr, cands)
		})
	}
}

// TestClusterResolverFiltersAfterComparing resolves a seq of two versions of other timestamps
// under queries that select both, one or neither version:
// the conflict is listed each time, and only the selected versions are candidates, marked, with their own version index.
func TestClusterResolverFiltersAfterComparing(t *testing.T) {
	t.Parallel()

	steps := hourSteps(10, 13, 4)
	moved := slices.Clone(steps)
	moved[2].rec.TSUTCNs += 100
	a, b := mergePack(t, seg0, nil, steps), mergePack(t, seg1, nil, moved)
	ts12 := steps[2].rec.TSUTCNs
	v0 := candView{seq: 12, conflict: true}
	v1 := candView{seq: 12, pack: 1, version: 1, conflict: true}
	tests := []struct {
		name     string
		from, to int64
		views    []candView
	}{
		{name: "both", from: ts12 - 2, to: ts12 + 101, views: []candView{{seq: 10}, {seq: 11}, v0, v1, {seq: 13}}},
		{name: "the first", from: ts12, to: ts12 + 1, views: []candView{v0}},
		{name: "the second", from: ts12 + 1, to: ts12 + 101, views: []candView{v1, {seq: 13}}},
		{name: "neither", from: ts12 - 2, to: ts12, views: []candView{{seq: 10}, {seq: 11}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			q := Query{Filter: Filter{TimeFrom: new(tt.from), TimeTo: new(tt.to)}}
			run, _ := planResolve(t, t.Context(), q, 0, nil, a, b)
			require.Len(t, run.p.captures[0].clusters, 1)
			require.False(t, run.p.captures[0].clusters[0].excluded)
			rs, err := resolveCluster(t.Context(), run)
			cr, cands, n := rs.cr, rs.cands, rs.seqs
			require.NoError(t, err)
			assert.Equal(t, 4, n, "every seq resolved")
			assert.Equal(t, []Conflict{{CaptureID: viewCapture, Seq: 12, Versions: [][]UUID{{seg0}, {seg1}}}}, run.p.res.Conflicts)
			assert.Equal(t, tt.views, viewsOf(run, cands))
			releaseAll(t, run, cr, cands)
		})
	}

	t.Run("a field predicate", func(t *testing.T) {
		t.Parallel()

		// No record holds these system bytes: the seq is compared and listed, and nothing selected.
		q := Query{Filter: Filter{SystemBytes: &[4]byte{1, 2, 3, 4}}}
		run, _ := planResolve(t, t.Context(), q, 0, nil, a, b)
		rs, err := resolveCluster(t.Context(), run)
		cr, cands := rs.cr, rs.cands
		require.NoError(t, err)
		assert.Len(t, run.p.res.Conflicts, 1)
		assert.Empty(t, cands)
		releaseAll(t, run, cr, cands)
	})
}

// TestClusterResolverMaxConflicts resolves a cluster of three conflicts under limits of three and two:
// reaching the limit is admitted, one more conflict fails the resolution with ErrReadLimit,
// the conflicts found before it kept, and everything held released.
func TestClusterResolverMaxConflicts(t *testing.T) {
	t.Parallel()

	steps := hourSteps(10, 14, 5)
	files := [][]byte{mergePack(t, seg0, nil, steps), mergePack(t, seg1, nil, changed(steps, 1, 11, 12, 13))}
	conflictAt := func(seq uint64) Conflict {
		return Conflict{CaptureID: viewCapture, Seq: seq, Versions: [][]UUID{{seg0}, {seg1}}}
	}

	run, _ := planResolve(t, t.Context(), Query{}, 3, nil, files...)
	rs, err := resolveCluster(t.Context(), run)
	cr, cands, n := rs.cr, rs.cands, rs.seqs
	require.NoError(t, err)
	assert.Equal(t, 5, n)
	assert.Equal(t, []Conflict{conflictAt(11), conflictAt(12), conflictAt(13)}, run.p.res.Conflicts)
	releaseAll(t, run, cr, cands)

	run, _ = planResolve(t, t.Context(), Query{}, 2, nil, files...)
	rs, err = resolveCluster(t.Context(), run)
	cr, cands, n = rs.cr, rs.cands, rs.seqs
	require.ErrorIs(t, err, ErrReadLimit)
	require.ErrorContains(t, err, "seq 13 of capture "+viewCapture.String()+" exceeds MaxConflicts 2:")
	assert.Equal(t, 3, n, "seqs 10 to 12 resolved")
	assert.Equal(t, []Conflict{conflictAt(11), conflictAt(12)}, run.p.res.Conflicts)
	assert.Len(t, cands, 5, "seq 10, then both versions of 11 and 12")
	assert.NotZero(t, run.p.loader.budget.held, "the pending candidates and the cursor hold the blocks")
	releaseAll(t, run, cr, cands)
}

// TestClusterResolverFailedBlocks resolves clusters with a block that fails its read and one that disagrees with its F-2 entry:
// the failed block's defect is added and its seqs resolved from the other blocks;
// the disagreeing block's defect is added and its records resolved, those past the cluster's last seq included.
func TestClusterResolverFailedBlocks(t *testing.T) {
	t.Parallel()

	t.Run("a corrupt block", func(t *testing.T) {
		t.Parallel()

		a := mergePack(t, seg0, nil, hourSteps(10, 17, 4))
		info := mustOpen(t, a, ReaderOptions{}).Blocks()[1]
		a = flipByte(a, info.Offset+format.EnvelopeLen+3)
		b := mergePack(t, seg1, nil, hourSteps(10, 17, 3))
		run, _ := planResolve(t, t.Context(), Query{}, 0, nil, a, b)
		rs, err := resolveCluster(t.Context(), run)
		cr, cands := rs.cr, rs.cands
		require.NoError(t, err)
		want := slices.Concat(plainViews(0, 0, 10, 11, 12, 13), plainViews(1, 1, 14, 15), plainViews(1, 2, 16, 17))
		assert.Equal(t, want, viewsOf(run, cands))
		require.Len(t, run.p.res.Incomplete, 1)
		d := run.p.res.Incomplete[0]
		assert.Equal(t, ReasonCorruptBlock, d.Reason)
		assert.Equal(t, [2]int{0, 1}, [2]int{d.Pack, d.Block})
		releaseAll(t, run, cr, cands)
	})

	t.Run("records past the cluster's last seq", func(t *testing.T) {
		t.Parallel()

		// a's one block holds 10, 12, 14 and 16, while its index states seqs 10 to 13, the range of b's one block.
		a := misindexedPack(t, seg0, viewCapture)
		b := mergePack(t, seg1, nil, hourSteps(10, 13, 4))
		run, _ := planResolve(t, t.Context(), Query{}, 0, nil, a, b)
		require.Empty(t, run.p.res.FooterErrs, "the footer must stay valid")
		require.Len(t, run.p.captures[0].clusters, 1)
		cl := &run.p.captures[0].clusters[0]
		require.Equal(t, [2]uint64{10, 13}, [2]uint64{cl.first, cl.last})

		rs, err := resolveCluster(t.Context(), run)
		cr, cands, n := rs.cr, rs.cands, rs.seqs
		require.NoError(t, err)
		assert.Equal(t, 6, n)
		want := []candView{{seq: 10}, {seq: 11, pack: 1}, {seq: 12}, {seq: 13, pack: 1}, {seq: 14}, {seq: 16}}
		assert.Equal(t, want, viewsOf(run, cands))
		require.Len(t, run.p.res.Incomplete, 1)
		d := run.p.res.Incomplete[0]
		assert.Equal(t, ReasonIndexMismatch, d.Reason)
		assert.Equal(t, [2]int{0, 0}, [2]int{d.Pack, d.Block})
		releaseAll(t, run, cr, cands)
	})
}

// TestClusterResolverErrorExits ends the resolution of a cluster on each error after candidates were made:
// the failed load of a block, the reservation of one over MaxHeldBytes and ctx done at a load.
// Releasing the candidates and closing the resolver leaves nothing held.
func TestClusterResolverErrorExits(t *testing.T) {
	t.Parallel()

	// Cluster order: a0 (10..13), b0 (10..12), b1 (13..15), a1 (14..17), b2 (16, 17).
	a := mergePack(t, seg0, nil, hourSteps(10, 17, 4))
	b := mergePack(t, seg1, nil, hourSteps(10, 17, 3))
	b1 := mustOpen(t, b, ReaderOptions{}).Blocks()[1]

	t.Run("ReadAt fails", func(t *testing.T) {
		t.Parallel()

		g := newGatedReader(b)
		rb, err := openReader(t.Context(), g, int64(len(b)), ReaderOptions{}, false)
		require.NoError(t, err)
		g.fail = func(off int64, _ int) bool { return off == int64(b1.Offset) }
		q := Query{}
		p, reads, err := planReads(t, t.Context(), []*Reader{mustOpen(t, a, ReaderOptions{}), rb}, q,
			MergeIterateOptions{Order: OrderCapture}, nil)
		require.NoError(t, err)
		run := newMergeIterateRun(p, &q)

		rs, err := resolveCluster(t.Context(), run)
		cr, cands, n := rs.cr, rs.cands, rs.seqs
		require.ErrorIs(t, err, errInjected)
		require.ErrorContains(t, err, "tracepack: pack 1, block 1:")
		assert.Equal(t, 3, n, "seqs 10 to 12 resolved")
		assert.Len(t, cands, 3)
		assert.Zero(t, reads[[2]int{0, 1}], "no read after the failure")
		releaseAll(t, run, cr, cands)
	})

	t.Run("MaxHeldBytes", func(t *testing.T) {
		t.Parallel()

		// A first resolution finds the bytes held once a1 is reserved, the readHook running after the reservation;
		// a limit one byte below them admits every load before a1's.
		q := Query{}
		var run *mergeIterateRun
		var heldAt []int64
		run, _ = planResolve(t, t.Context(), q, 0, func(int, int) { heldAt = append(heldAt, run.p.loader.budget.held) }, a, b)
		rs, err := resolveCluster(t.Context(), run)
		require.NoError(t, err)
		releaseAll(t, run, rs.cr, rs.cands)
		require.Len(t, heldAt, 5)
		limit := heldAt[3] - 1
		require.LessOrEqual(t, slices.Max(heldAt[:3]), limit)

		p, reads, err := planReads(t, t.Context(), openAll(t, a, b), q, MergeIterateOptions{Order: OrderCapture, MaxHeldBytes: limit}, nil)
		require.NoError(t, err)
		run = newMergeIterateRun(p, &q)
		rs, err = resolveCluster(t.Context(), run)
		cr, cands, n := rs.cr, rs.cands, rs.seqs
		require.ErrorIs(t, err, ErrReadLimit)
		assert.Equal(t, 4, n, "seqs 10 to 13 resolved")
		assert.Len(t, cands, 4)
		assert.Zero(t, reads[[2]int{0, 1}], "a1 is not read")
		releaseAll(t, run, cr, cands)
	})

	t.Run("ctx done at a load", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		run, reads := planResolve(t, ctx, Query{}, 0, func(pack, block int) {
			if pack == 1 && block == 0 {
				cancel()
			}
		}, a, b)

		rs, err := resolveCluster(ctx, run)
		cr, cands, n := rs.cr, rs.cands, rs.seqs
		require.ErrorIs(t, err, context.Canceled)
		assert.Equal(t, 3, n, "seqs 10 to 12 resolved")
		assert.Len(t, cands, 3)
		assert.Zero(t, reads[[2]int{1, 1}], "no read once ctx is done")
		releaseAll(t, run, cr, cands)
	})
}

// TestClusterResolverCopiesCounter cancels ctx at the last load of a cluster whose copies number 4095 or 4096,
// with a filter that selects none:
// the check every 4096 copies stops the resolution at the group that makes the 4096th copy, and only then.
func TestClusterResolverCopiesCounter(t *testing.T) {
	t.Parallel()

	q := Query{Filter: Filter{SystemBytes: &[4]byte{1, 2, 3, 4}}}
	tests := []struct {
		last    uint64
		stopped bool
	}{
		{last: 2047, stopped: false},
		{last: 2048, stopped: true},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%d copies", 2048+tt.last), func(t *testing.T) {
			t.Parallel()

			// Blocks of other boundaries, so no block is a duplicate and every copy is gathered.
			a := mergePack(t, seg0, nil, hourSteps(1, 2048, 512))
			b := mergePack(t, seg1, nil, hourSteps(1, tt.last, 500))
			lastBlock := len(mustOpen(t, b, ReaderOptions{}).Blocks()) - 1
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			run, _ := planResolve(t, ctx, q, 0, func(pack, block int) {
				if pack == 1 && block == lastBlock {
					cancel()
				}
			}, a, b)
			cl := &run.p.captures[0].clusters[0]
			last := cl.blocks[len(cl.blocks)-1]
			require.Equal(t, [2]int{1, lastBlock}, [2]int{last.pack, last.block}, "b's last block is the cluster's last")

			rs, err := resolveCluster(ctx, run)
			cr, cands, n := rs.cr, rs.cands, rs.seqs
			if tt.stopped {
				require.ErrorIs(t, err, context.Canceled)
				assert.Equal(t, 2047, n, "the group of seq 2048 makes the 4096th copy")
			} else {
				require.NoError(t, err)
				assert.Equal(t, 2048, n)
			}
			assert.Empty(t, cands)
			releaseAll(t, run, cr, cands)
		})
	}
}

// TestRunCaptureCopiesCounterCrossing reads, in capture order,
// three packs holding the same records in blocks of other boundaries,
// so every seq group gathers three copies and the count of copies goes from 4095 after seq 1365 to 4098 at seq 1366,
// passing 4096 without reaching it.
// fn cancels ctx when called for seq 1365 and returns nil:
// the check after the group of seq 1366 is gathered stops the read with ctx's error, and fn is not called again.
func TestRunCaptureCopiesCounterCrossing(t *testing.T) {
	t.Parallel()

	files := [][]byte{
		mergePack(t, seg0, nil, hourSteps(1, 2000, 1000)),
		mergePack(t, seg1, nil, hourSteps(1, 2000, 999)),
		mergePack(t, seg2, nil, hourSteps(1, 2000, 998)),
	}
	readers := openAll(t, files...)
	for i, r := range readers {
		for _, b := range r.Blocks() {
			require.True(t, b.Indexed)
			require.NotEqual(t, uint64(1366), b.FirstSeq, "pack %d: no block load at seq 1366, whose ctx check would come first", i)
		}
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	items, _, err := captureRead(t, ctx, readers, Query{}, MergeIterateOptions{}, func(it *Item) error {
		if it.Record.Seq == 1365 {
			cancel()
		}

		return nil
	})
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorContains(t, err, "tracepack: merge iterate:")
	require.Len(t, items, 1365, "no call of fn after the check fails")
	assert.Equal(t, uint64(1365), items[len(items)-1].rec.Seq)
}

// TestRunCaptureCopiesCounterCrossingConflicts reads, in capture order,
// three packs holding each seq in a version of its own, the System Bytes of its payload differing,
// in blocks of other boundaries,
// with a filter selecting pack 0's version, so each seq group gathers three copies, lists a conflict,
// and passes one record to fn.
// fn cancels ctx when called for seq 1365 and returns nil:
// the check after the group of seq 1366 is gathered, its count of copies going from 4095 to 4098,
// stops the read with ctx's error before that group's conflict is listed, and fn is not called again.
func TestRunCaptureCopiesCounterCrossingConflicts(t *testing.T) {
	t.Parallel()

	seqs := make([]uint64, 2000)
	for i := range seqs {
		seqs[i] = uint64(i + 1)
	}
	files := [][]byte{
		mergePack(t, seg0, nil, hourSteps(1, 2000, 1000)),
		mergePack(t, seg1, nil, changed(hourSteps(1, 2000, 999), 1, seqs...)),
		mergePack(t, seg2, nil, changed(hourSteps(1, 2000, 998), 2, seqs...)),
	}
	readers := openAll(t, files...)
	for i, r := range readers {
		for _, b := range r.Blocks() {
			require.True(t, b.Indexed)
			require.NotEqual(t, uint64(1366), b.FirstSeq, "pack %d: no block load at seq 1366, whose ctx check would come first", i)
		}
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	q := Query{Filter: Filter{SystemBytes: &[4]byte{0xDE, 0xAD, 0xBE, 0xEF}}}
	items, res, err := captureRead(t, ctx, readers, q, MergeIterateOptions{}, func(it *Item) error {
		if it.Record.Seq == 1365 {
			cancel()
		}

		return nil
	})
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorContains(t, err, "tracepack: merge iterate:")
	require.Len(t, items, 1365, "one call of fn for each seq, none after the check fails")
	for k, it := range items {
		require.Equal(t, uint64(k+1), it.rec.Seq)
		require.Equal(t, 0, it.pack, "seq %d: pack 0's version", it.rec.Seq)
		require.True(t, it.conflict, "seq %d", it.rec.Seq)
	}
	require.Len(t, res.Conflicts, 1365, "the conflicts of seqs 1 to 1365, not that of seq 1366")
	for k, c := range res.Conflicts {
		require.Equal(t, viewCapture, c.CaptureID)
		require.Equal(t, uint64(k+1), c.Seq)
		require.ElementsMatch(t, [][]UUID{{seg0}, {seg1}, {seg2}}, c.Versions, "seq %d", c.Seq)
	}
	assert.Empty(t, res.Incomplete)
}

// TestRunCaptureCopiesCounterLargeGroup reads, in capture order with a step of 5 copies,
// twelve packs holding each seq of 1 to 10 in a version of its own, in blocks starting at seqs 1, 5 and 9,
// with a filter selecting pack 0's version, so each seq group gathers twelve copies, lists a conflict,
// and passes one record to fn.
// fn cancels ctx when called for seq 2 and returns nil:
// the group of seq 3, which loads no block and takes the count of copies from 24 to 36 across three multiples of 5,
// is checked once after it is gathered,
// which stops the read with ctx's error before that group's conflict is listed, and fn is not called again.
func TestRunCaptureCopiesCounterLargeGroup(t *testing.T) {
	t.Parallel()

	const packs = 12
	seqs := []uint64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	files := make([][]byte, packs)
	ids := make([][]UUID, packs)
	for i := range files {
		id := UUID{0x50 + byte(i)}
		files[i] = mergePack(t, id, nil, changed(hourSteps(1, 10, 4), byte(i), seqs...))
		ids[i] = []UUID{id}
	}
	readers := openAll(t, files...)
	for i, r := range readers {
		for _, b := range r.Blocks() {
			require.True(t, b.Indexed)
			require.NotEqual(t, uint64(3), b.FirstSeq, "pack %d: no block load at seq 3, whose ctx check would come first", i)
		}
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	q := Query{Filter: Filter{SystemBytes: &[4]byte{0xDE, 0xAD, 0xBE, 0xEF}}}
	var items []iterItem
	fn := func(it *Item) error {
		items = append(items, copyItem(it))
		if it.Record.Seq == 2 {
			cancel()
		}

		return nil
	}
	o, err := checkMergeIterate(ctx, readers, &q, MergeIterateOptions{Order: OrderCapture}, fn)
	require.NoError(t, err)
	p := newMergeIteratePlan(o)
	p.copiesStep = 5
	require.NoError(t, p.build(ctx, readers, &q))
	err = p.runCapture(ctx, &q, fn)

	require.ErrorIs(t, err, context.Canceled)
	require.ErrorContains(t, err, "tracepack: merge iterate:")
	require.Len(t, items, 2, "one call of fn for each of seqs 1 and 2, none after the check fails")
	for k, it := range items {
		require.Equal(t, uint64(k+1), it.rec.Seq)
		require.Equal(t, 0, it.pack, "seq %d: pack 0's version", it.rec.Seq)
		require.True(t, it.conflict, "seq %d", it.rec.Seq)
	}
	require.Len(t, p.res.Conflicts, 2, "the conflicts of seqs 1 and 2, not that of seq 3")
	for k, c := range p.res.Conflicts {
		require.Equal(t, viewCapture, c.CaptureID)
		require.Equal(t, uint64(k+1), c.Seq)
		require.ElementsMatch(t, ids, c.Versions, "seq %d", c.Seq)
	}
	assert.Empty(t, p.res.Incomplete)
	assert.Zero(t, p.loader.budget.held, "nothing stays held")
}

// TestCtxCounterAdd adds counts from 0 up to math.MaxInt to counters of several steps
// and requires each addition to report a check exactly when the exact total reaches or passes a multiple of the step,
// the counter keeping the exact total modulo the step, however far the total goes past the int range.
func TestCtxCounterAdd(t *testing.T) {
	t.Parallel()

	for _, step := range []int{1, 5, resolveCtxCopies} {
		t.Run(fmt.Sprintf("step %d", step), func(t *testing.T) {
			t.Parallel()

			rng := rand.New(rand.NewPCG(1, uint64(step)))
			k := ctxCounter{step: step}
			total, bigStep := new(big.Int), big.NewInt(int64(step))
			before, after, mod := new(big.Int), new(big.Int), new(big.Int)
			for i := range 20000 {
				var n int
				switch rng.IntN(5) {
				case 0:
					n = 0
				case 1:
					n = math.MaxInt
				case 2:
					n = math.MaxInt - rng.IntN(2*step)
				default:
					n = rng.IntN(3 * step)
				}
				before.Quo(total, bigStep)
				total.Add(total, big.NewInt(int64(n)))
				after.Quo(total, bigStep)
				require.Equal(t, after.Cmp(before) > 0, k.add(n), "addition %d of %d", i, n)
				require.Equal(t, mod.Mod(total, bigStep).Int64(), int64(k.n), "addition %d of %d", i, n)
			}
		})
	}
}

// TestCountCopiesNearTheStep starts a read's count of copies just below a multiple of 4096
// and counts a group of three copies with ctx done:
// the group that reaches or passes the multiple fails the check, and a group stopping short of it is not checked.
func TestCountCopiesNearTheStep(t *testing.T) {
	t.Parallel()

	tests := []struct {
		count   int
		stopped bool
	}{
		{count: resolveCtxCopies - 1, stopped: true},
		{count: resolveCtxCopies - 3, stopped: true},
		{count: resolveCtxCopies - 4, stopped: false},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("from %d", tt.count), func(t *testing.T) {
			t.Parallel()

			run, _ := planResolve(t, t.Context(), Query{}, 0, nil, mergePack(t, seg0, nil, hourSteps(1, 2, 2)))
			require.Equal(t, resolveCtxCopies, run.copies.step)
			run.copies.n = tt.count
			ctx, cancel := context.WithCancel(t.Context())
			cancel()

			err := run.countCopies(ctx, 3)
			if !tt.stopped {
				require.NoError(t, err)
				require.Equal(t, resolveCtxCopies-1, run.copies.n)
				err = run.countCopies(ctx, 3)
			}
			require.ErrorIs(t, err, context.Canceled)
			require.ErrorContains(t, err, "tracepack: merge iterate:")
		})
	}
}

// TestYieldNearTheStep starts a read's count of calls of fn two below a multiple of 4096 and cancels ctx:
// the next call of fn is made unchecked, and the one after, which would make the multiple, is not made.
func TestYieldNearTheStep(t *testing.T) {
	t.Parallel()

	run, _ := planResolve(t, t.Context(), Query{}, 0, nil, mergePack(t, seg0, nil, hourSteps(1, 2, 2)))
	require.Equal(t, yieldCtxCalls, run.yields.step)
	pc := &run.p.captures[0]
	cr := newClusterResolver(run, pc.id, &pc.clusters[0])
	cands, ok, err := cr.next(t.Context(), nil)
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, cands, 1)

	run.yields.n = yieldCtxCalls - 2
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	calls := 0
	fn := func(*Item) error {
		calls++

		return nil
	}
	var it Item
	require.NoError(t, run.yield(ctx, &cands[0], &it, fn))
	err = run.yield(ctx, &cands[0], &it, fn)
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorContains(t, err, "tracepack: merge iterate:")
	assert.Equal(t, 1, calls, "no call of fn after the check fails")
	releaseAll(t, run, cr, cands)
}

// TestClusterResolverOwnership requires a candidate to keep its representative's block, and its bytes,
// after the cursor is done with the block and other blocks are loaded, until the candidate is released.
func TestClusterResolverOwnership(t *testing.T) {
	t.Parallel()

	// Cluster order: a0 (10, 11), b0 (10..13), a1 (12, 13); seq 11 is a0's last record,
	// and b0 is the representative of 12 and 13.
	a := mergePack(t, seg0, nil, hourSteps(10, 13, 2))
	b := mergePack(t, seg1, nil, hourSteps(10, 13, 4))
	run, _ := planResolve(t, t.Context(), Query{Payloads: true}, 0, nil, a, b)
	records := recordsOf(t, a)
	pc := &run.p.captures[0]
	cr := newClusterResolver(run, pc.id, &pc.clusters[0])

	var cands []candidate
	for range 2 {
		var ok bool
		var err error
		cands, ok, err = cr.next(t.Context(), cands)
		require.NoError(t, err)
		require.True(t, ok)
	}
	a0 := cands[1].h
	require.Same(t, a0, cands[0].h)
	assert.Nil(t, cr.held[0], "the cursor is done with a0")
	assert.Equal(t, 2, a0.owners, "the candidates of 10 and 11 own a0")

	cands, ok, err := cr.next(t.Context(), cands)
	require.NoError(t, err)
	require.True(t, ok)
	require.NotNil(t, cr.held[2], "a1 is loaded")
	for i := range cands {
		it := itemOf(run, &cands[i])
		assert.Equal(t, records[i].rec, it.rec, "seq %d: the record intact", records[i].rec.Seq)
	}
	assert.Equal(t, 1, cands[2].h.pack, "b0 holds seq 12's candidate")

	for ok {
		cands, ok, err = cr.next(t.Context(), cands)
		require.NoError(t, err)
	}
	cr.close()
	total := heldSize(t, a) + heldSize(t, b)
	assert.Equal(t, total, run.p.loader.budget.held, "only the candidates hold blocks: a0 and b0, a1 no representative")

	cands[0].release(&run.p.loader)
	assert.NotNil(t, a0.buf.raw, "a0 held while seq 11's candidate is pending")
	assert.Equal(t, total, run.p.loader.budget.held)
	cands[1].release(&run.p.loader)
	requireDropped(t, a0.buf)
	assert.Equal(t, heldSize(t, b), run.p.loader.budget.held)
	releaseAll(t, run, cr, cands[2:])
}
