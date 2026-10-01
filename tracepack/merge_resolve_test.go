package tracepack

import (
	"bytes"
	"cmp"
	"context"
	"encoding/binary"
	"math/rand/v2"
	"runtime"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// seqRun returns the seqs first to last.
func seqRun(first, last uint64) []uint64 {
	out := make([]uint64, 0, last-first+1)
	for seq := first; seq <= last; seq++ {
		out = append(out, seq)
	}

	return out
}

// seqBlocks returns data records in the merge test hour, the record of seq s at blockTestHour + s as seqSteps writes it,
// one block for each list of seqs.
func seqBlocks(blocks ...[]uint64) []footerTestStep {
	var steps []footerTestStep
	for _, seqs := range blocks {
		for i, seq := range seqs {
			steps = append(steps, footerTestStep{rec: testDataRecord(seq, blockTestHour+int64(seq), 1), flush: i == len(seqs)-1})
		}
	}

	return steps
}

// changed returns steps with the payload of the record of each seq of seqs changed in its last byte by delta.
func changed(steps []footerTestStep, delta byte, seqs ...uint64) []footerTestStep {
	out := slices.Clone(steps)
	for i := range out {
		if slices.Contains(seqs, out[i].rec.Seq) {
			p := slices.Clone(out[i].rec.Payload)
			p[len(p)-1] += delta
			out[i].rec.Payload = p
		}
	}

	return out
}

// busSteps returns the segment of consumer k of three consumers of a durable bus
// that took seqs 100 to 147 in turns of two records, consumer k the turns k, k+3 and so on,
// with the seqs of extra it also took,
// in blocks of four records: segments that interleave, each block's seq range overlapping the others' blocks.
func busSteps(k int, extra ...uint64) []footerTestStep {
	seqs := slices.Clone(extra)
	for turn := uint64(k); turn < 24; turn += 3 {
		seqs = append(seqs, 100+2*turn, 101+2*turn)
	}
	slices.Sort(seqs)

	var blocks [][]uint64
	for len(seqs) > 0 {
		n := min(4, len(seqs))
		blocks = append(blocks, seqs[:n])
		seqs = seqs[n:]
	}

	return seqBlocks(blocks...)
}

// blockShapes returns, for every block of the finalized pack file in file order, its record count and its record_header_len.
func blockShapes(t testing.TB, file []byte) [][2]int {
	t.Helper()

	blocks := mustOpen(t, file, ReaderOptions{}).Blocks()
	out := make([][2]int, 0, len(blocks))
	for _, b := range blocks {
		out = append(out, [2]int{int(b.RecordCount), int(b.RecordHeaderLen)})
	}

	return out
}

// loadEvent is one block the resolution loaded: the input and first seq of the block,
// and the number of blocks open and of records resolved before it was loaded.
type loadEvent struct {
	input    int
	first    uint64
	open     int
	resolved int
}

// writeWithHooks writes the archive of the plan of inputs with fixedIDs(opts) into a buffer,
// the plan's hooks set by hooks before it writes, and returns what it wrote, its report and its error.
// An archive written without an error must have the properties of every successful merge (requireMergeProperties).
func writeWithHooks(t *testing.T, v View, inputs []MergeInput, opts MergeOptions, hooks func(p *mergePlan)) ([]byte, MergeReport, error) {
	t.Helper()

	opts = fixedIDs(opts)
	p := mustPlanMerge(t, v, inputs, opts)
	hooks(p)
	var out bytes.Buffer
	rep, err := p.write(t.Context(), &out, &opts)
	if err == nil {
		requireMergeProperties(t, inputFiles(t, inputs), out.Bytes(), rep, opts)
	}

	return out.Bytes(), rep, err
}

func TestMergeResolvesOverlaps(t *testing.T) {
	t.Parallel()

	base := mergePack(t, seg0, nil, hourSteps(10, 13, 4))
	tests := []struct {
		name  string
		files [][]byte
		want  mergeCounts
	}{
		{
			name: "containment",
			files: [][]byte{
				mergePack(t, seg0, nil, hourSteps(10, 20, 11)),
				mergePack(t, seg1, nil, hourSteps(13, 15, 3)),
			},
			want: mergeCounts{blocks: 1, records: 11, resolved: 2},
		},
		{
			name: "shared endpoints",
			files: [][]byte{
				mergePack(t, seg0, nil, hourSteps(10, 13, 4)),
				mergePack(t, seg1, nil, hourSteps(13, 16, 4)),
			},
			want: mergeCounts{blocks: 1, records: 7, resolved: 2},
		},
		{
			name: "gaps filled by another input",
			files: [][]byte{
				mergePack(t, seg0, nil, seqBlocks([]uint64{10, 11, 12, 15, 16})),
				mergePack(t, seg1, nil, seqBlocks([]uint64{13, 14})),
			},
			want: mergeCounts{blocks: 1, records: 7, resolved: 2},
		},
		{
			// The blocks differ in codec and in their boundaries, so they chain into one cluster of five blocks.
			name: "identical records with different encodings",
			files: [][]byte{
				mergePackCodec(t, CodecZstd, seg0, hourSteps(10, 19, 5)),
				mergePackCodec(t, CodecNone, seg1, hourSteps(10, 19, 4)),
			},
			want: mergeCounts{blocks: 1, records: 10, resolved: 5},
		},
		{
			// seg1's block holds seg0's body under an envelope with a reserved byte set: it is resolved, not dropped,
			// while seg2's, seg0's byte for byte, is dropped.
			name: "equal body bytes with different envelopes",
			files: [][]byte{
				base,
				withIDs(t, resealedEnvelope(t, base, 0), seg1, viewCapture),
				withIDs(t, base, seg2, viewCapture),
			},
			want: mergeCounts{blocks: 1, records: 4, dups: 1, resolved: 2},
		},
		{
			// Consumer 1 also took seq 106, which consumer 0 holds: a record written twice, byte-identical.
			name: "interleaved bus-style segments",
			files: [][]byte{
				mergePack(t, seg0, nil, busSteps(0)),
				mergePack(t, seg1, nil, busSteps(1, 106)),
				mergePack(t, seg2, nil, busSteps(2)),
			},
			want: mergeCounts{blocks: 1, records: 48, resolved: 4 + 5 + 4},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			v := mergeView(t, nil, tt.files...)
			p := mustPlanMerge(t, v, inputsOf(t, v, tt.files...), mergeOpts())
			require.Len(t, p.clusters, 1)
			_, rep := mustMerge(t, v, inputsOf(t, v, tt.files...), mergeOpts())
			requireCounts(t, rep, tt.want)
		})
	}
}

// TestMergeResolvedBlockBoundaries checks where the resolution ends the blocks it encodes:
// a block holds records of one record_header_len while their decoded size stays within the threshold,
// so a record that alone reaches the threshold is written in a block of its own.
func TestMergeResolvedBlockBoundaries(t *testing.T) {
	t.Parallel()

	t.Run("a record_header_len change starts a new block", func(t *testing.T) {
		t.Parallel()

		// seg1's widened records fill the gap in seg0's block, whose range holds theirs without sharing a seq.
		a := mergePack(t, seg0, nil, seqBlocks([]uint64{10, 11, 20}))
		b := widenFile(t, mergePack(t, seg1, nil, seqBlocks(seqRun(12, 15))), allBlocks)
		v := mergeView(t, nil, a, b)
		out, rep := mustMerge(t, v, inputsOf(t, v, a, b), mergeOpts())
		requireCounts(t, rep, mergeCounts{blocks: 3, records: 7, resolved: 2})
		wide := format.RecordHeaderLen + wideExtraLen
		assert.Equal(t, [][2]int{{2, format.RecordHeaderLen}, {4, wide}, {1, format.RecordHeaderLen}}, blockShapes(t, out))
	})

	t.Run("an oversized record is written alone", func(t *testing.T) {
		t.Parallel()

		// Each record is 44 + 14 bytes decoded; the threshold holds three, and seq 13 alone exceeds it.
		steps := hourSteps(10, 16, 7)
		steps[3].rec = bigDataRecord(13, blockTestHour+13, 200)
		a, b := mergePackCodec(t, CodecZstd, seg0, steps), mergePackCodec(t, CodecNone, seg1, steps)
		v := mergeView(t, nil, a, b)
		opts := mergeOpts()
		opts.BlockThreshold = 3 * testRecordLen
		out, rep := mustMerge(t, v, inputsOf(t, v, a, b), opts)
		requireCounts(t, rep, mergeCounts{blocks: 3, records: 7, resolved: 2})
		h := format.RecordHeaderLen
		assert.Equal(t, [][2]int{{3, h}, {1, h}, {3, h}}, blockShapes(t, out))
	})
}

// TestMergeResolutionKeepsRecordBytes resolves records whose header rows carry an extension area,
// reserved bits of quality and field_validity, and retired and reserved record_flags bits,
// which no record header encoder writes: the archive holds every row and payload byte for byte.
func TestMergeResolutionKeepsRecordBytes(t *testing.T) {
	t.Parallel()

	unusual := func(_ int, b *testBlock) {
		for j := range b.headers {
			seq := b.headers[j].Seq
			b.headers[j].Extra = []byte{0xE0, byte(seq), 0xA5, 3, 4, 5, 6, 7}
		}
		b.patchRows = func(rows []byte, rhl int) {
			for off := 0; off < len(rows); off += rhl {
				row := rows[off : off+rhl]
				binary.LittleEndian.PutUint16(row[36:], binary.LittleEndian.Uint16(row[36:])|1<<15)
				row[42] |= 1 << 7
				row[43] |= 1<<0 | 1<<5
			}
		}
	}
	// The two packs hold the same records in blocks of other codecs and boundaries, so they form one cluster.
	a := reindexed(t, rebuildPack(t, mergePackCodec(t, CodecZstd, seg0, hourSteps(10, 17, 4)), unusual))
	b := reindexed(t, rebuildPack(t, mergePackCodec(t, CodecNone, seg1, hourSteps(10, 17, 3)), unusual))
	requireConsistent(t, a)
	requireConsistent(t, b)
	v := mergeView(t, nil, a, b)
	out, rep := mustMerge(t, v, inputsOf(t, v, a, b), mergeOpts())
	requireCounts(t, rep, mergeCounts{blocks: 1, records: 8, resolved: 5})

	records := rawRecordsOf(t, out)
	require.Len(t, records, 8)
	assert.Equal(t, rawRecordsOf(t, a), records, "every row and payload byte for byte")
	for _, r := range records {
		require.Len(t, r.row, format.RecordHeaderLen+8)
		assert.Equal(t, byte(r.seq), r.row[format.RecordHeaderLen+1], "seq %d: the extension area", r.seq)
		assert.NotZero(t, r.row[37]&0x80, "seq %d: the reserved quality bit", r.seq)
		assert.Equal(t, byte(1<<0|1<<5), r.row[43]&(1<<0|1<<5), "seq %d: the retired and reserved record_flags bits", r.seq)
	}
}

func TestMergeConflicts(t *testing.T) {
	t.Parallel()

	early := mergePack(t, seg4, nil, hourSteps(1, 4, 4))
	steps := hourSteps(10, 13, 4)
	a := mergePack(t, seg0, nil, steps)
	conflictAt := func(seq uint64, versions ...[]UUID) Conflict {
		return Conflict{CaptureID: viewCapture, Seq: seq, Versions: versions}
	}
	// rowApart holds a's records, the header row of seq 12 with its reserved quality bit 15 set:
	// the same header length and payload, one header byte apart.
	rowApart := reindexed(t, rebuildPack(t, mergePack(t, seg1, nil, steps), func(_ int, b *testBlock) {
		b.patchRows = func(rows []byte, rhl int) {
			for off := 0; off < len(rows); off += rhl {
				if binary.LittleEndian.Uint64(rows[off:]) == 12 {
					rows[off+37] ^= 0x80
				}
			}
		}
	}))
	want, got := rawRecordsOf(t, a)[2], rawRecordsOf(t, rowApart)[2]
	require.Equal(t, want.payload, got.payload)
	require.Len(t, got.row, len(want.row))
	require.NotEqual(t, want.row, got.row)
	tests := []struct {
		name  string
		files [][]byte
		want  []Conflict
	}{
		{
			name:  "two versions",
			files: [][]byte{a, mergePack(t, seg1, nil, changed(steps, 1, 12))},
			want:  []Conflict{conflictAt(12, []UUID{seg0}, []UUID{seg1})},
		},
		{
			name: "three versions",
			files: [][]byte{
				a,
				mergePack(t, seg1, nil, changed(steps, 1, 12)),
				mergePack(t, seg2, nil, changed(steps, 2, 12)),
			},
			want: []Conflict{conflictAt(12, []UUID{seg0}, []UUID{seg1}, []UUID{seg2})},
		},
		{
			name:  "header rows of one length, a reserved bit apart",
			files: [][]byte{a, rowApart},
			want:  []Conflict{conflictAt(12, []UUID{seg0}, []UUID{seg1})},
		},
		{
			name:  "a version of another header length",
			files: [][]byte{a, widenFile(t, mergePack(t, seg1, nil, steps), allBlocks)},
			want: []Conflict{
				conflictAt(10, []UUID{seg0}, []UUID{seg1}), conflictAt(11, []UUID{seg0}, []UUID{seg1}),
				conflictAt(12, []UUID{seg0}, []UUID{seg1}), conflictAt(13, []UUID{seg0}, []UUID{seg1}),
			},
		},
		{
			// seg1's block is seg0's byte for byte, dropped as its duplicate; seg1 still holds the version.
			name:  "a duplicate's origin",
			files: [][]byte{a, withIDs(t, a, seg1, viewCapture), mergePack(t, seg2, nil, changed(steps, 1, 12))},
			want:  []Conflict{conflictAt(12, []UUID{seg0, seg1}, []UUID{seg2})},
		},
		{
			// The blocks start at 10 (seg2), 11 (seg1) and 12 (seg0):
			// seg2's version comes first, held by seg0 too, and the ids follow the view's Packs.
			name: "versions by first block, ids by the view's order",
			files: [][]byte{
				mergePack(t, seg0, nil, seqBlocks([]uint64{12, 13})),
				mergePack(t, seg1, nil, changed(seqBlocks([]uint64{11, 12}), 1, 12)),
				mergePack(t, seg2, nil, seqBlocks(seqRun(10, 13))),
			},
			want: []Conflict{conflictAt(12, []UUID{seg0, seg2}, []UUID{seg1})},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			files := append(slices.Clone(tt.files), early)
			v := mergeView(t, nil, files...)
			require.Equal(t, seg4, v.Packs[len(v.Packs)-1], "the input order of the files")
			var log conflictLog
			opts := mergeOpts()
			opts.OnConflict = log.add
			out, rep, err := mergeBytes(t.Context(), v, inputsOf(t, v, files...), opts)
			require.ErrorIs(t, err, ErrMergeConflict)
			assert.NotErrorIs(t, err, ErrMergeInput)
			assert.Equal(t, MergeReport{Conflicts: len(tt.want), ConflictsComplete: true}, rep)
			assert.Equal(t, tt.want, log.got)
			// The cluster before the conflicts waited in its coalescing group, which the conflict dropped.
			requireNoTrailer(t, out, 0)
		})
	}
}

// TestMergeConflictsAcrossClusters merges three clusters, the first two with conflicts:
// OnConflict receives them in ascending seq, each a new value that the callee may keep and change,
// while every block, those after the first conflict included, is read once and nothing more is written.
func TestMergeConflictsAcrossClusters(t *testing.T) {
	t.Parallel()

	steps := slices.Concat(hourSteps(10, 13, 4), hourSteps(20, 23, 4), hourSteps(30, 33, 4))
	a := mergePack(t, seg0, nil, steps)
	b := mergePack(t, seg1, nil, changed(steps[:8], 1, 11, 12, 21))
	v := mergeView(t, nil, a, b)
	inputs, readers := loggedInputs(t, v, a, b)

	var kept, seen []Conflict
	opts := mergeOpts()
	opts.OnConflict = func(c Conflict) {
		snapshot := c
		snapshot.Versions = make([][]UUID, len(c.Versions))
		for i := range c.Versions {
			snapshot.Versions[i] = slices.Clone(c.Versions[i])
		}
		seen = append(seen, snapshot)
		// The callee owns c: a later conflict must not see this change, nor undo it.
		c.Versions[0][0] = UUID{0xFF}
		kept = append(kept, c)
	}
	out, rep, err := mergeBytes(t.Context(), v, inputs, opts)
	require.ErrorIs(t, err, ErrMergeConflict)
	assert.Equal(t, MergeReport{Conflicts: 3, ConflictsComplete: true}, rep)

	versions := [][]UUID{{seg0}, {seg1}}
	want := []Conflict{
		{CaptureID: viewCapture, Seq: 11, Versions: versions},
		{CaptureID: viewCapture, Seq: 12, Versions: versions},
		{CaptureID: viewCapture, Seq: 21, Versions: versions},
	}
	assert.Equal(t, want, seen)
	for i, c := range kept {
		assert.Equal(t, [][]UUID{{{0xFF}}, {seg1}}, c.Versions, "conflict %d keeps the callee's change", i)
	}
	requireNoTrailer(t, out, 0)
	assert.Equal(t, []int{1, 1, 1}, blockReads(t, a, readers[0]))
	assert.Equal(t, []int{1, 1}, blockReads(t, b, readers[1]))
}

// TestMergeManyConflictsRetainNothing merges two packs whose every record conflicts, at an overlap depth of two throughout:
// the merge retains nothing per conflict, so its live heap does not grow while OnConflict receives them.
// It is not parallel: it measures the process-wide heap.
func TestMergeManyConflictsRetainNothing(t *testing.T) {
	const n = 50_000

	steps := hourSteps(10, 10+n-1, n)
	other := slices.Clone(steps)
	for i := range other {
		p := slices.Clone(other[i].rec.Payload)
		p[len(p)-1]++
		other[i].rec.Payload = p
	}
	a, b := mergePack(t, seg0, nil, steps), mergePack(t, seg1, nil, other)
	v := mergeView(t, nil, a, b)

	heapNow := func() uint64 {
		var ms runtime.MemStats
		runtime.GC() //nolint:revive // a collected heap is what makes the HeapAlloc comparison below meaningful
		runtime.ReadMemStats(&ms)

		return ms.HeapAlloc
	}
	var count int
	var early, late uint64
	opts := mergeOpts()
	opts.OnConflict = func(c Conflict) {
		count++
		assert.Len(t, c.Versions, 2)
		if count == 1000 {
			early = heapNow()
		}
		if count == n {
			late = heapNow()
		}
	}
	_, rep, err := mergeBytes(t.Context(), v, inputsOf(t, v, a, b), opts)
	require.ErrorIs(t, err, ErrMergeConflict)
	assert.Equal(t, MergeReport{Conflicts: n, ConflictsComplete: true}, rep)
	t.Logf("live heap after conflict 1000: %d bytes, after conflict %d: %d bytes", early, n, late)
	assert.Less(t, int64(late)-int64(early), int64(1<<20), "nothing is retained per conflict")
}

// TestMergeConflictThenError merges a cluster with a conflict at seq 12, then finds another error:
// Merge returns that error, with the conflict counted and ConflictsComplete false, and writes no block and no trailer.
func TestMergeConflictThenError(t *testing.T) {
	t.Parallel()

	a := mergePack(t, seg0, nil, hourSteps(10, 16, 7))
	b := mergePack(t, seg1, nil, changed(hourSteps(10, 12, 3), 1, 12))
	late := mergePack(t, seg3, nil, hourSteps(20, 23, 4))
	// corrupt's block joins the cluster at seq 14, after the conflict, and fails its body CRC when it is read.
	corrupt := mergePack(t, seg2, nil, hourSteps(14, 16, 3))
	corrupt = flipByte(corrupt, mustOpen(t, corrupt, ReaderOptions{}).Blocks()[0].Offset+format.EnvelopeLen+1)
	// With seg1's block released after seq 12, deep's two blocks open at 13 and 14 make three blocks open.
	deep := [][]byte{mergePack(t, seg2, nil, hourSteps(13, 16, 4)), mergePack(t, seg3, nil, hourSteps(14, 16, 3))}
	big := mergePack(t, seg3, nil, []footerTestStep{{rec: textDataRecord(20, blockTestHour+20, 4096), flush: true}})
	lateOff := int64(mustOpen(t, late, ReaderOptions{}).Blocks()[0].Offset)

	tests := []struct {
		name  string
		files [][]byte
		// edit changes the options and the inputs; cancel cancels the merge's ctx.
		edit func(opts *MergeOptions, inputs []MergeInput, cancel func())
		want error
		msg  string
	}{
		{name: "a corrupt block", files: [][]byte{a, b, corrupt}, want: ErrMergeInput, msg: "body CRC"},
		{
			name: "a failed read", files: [][]byte{a, b, late}, want: errInjected, msg: "input 2, pack " + seg3.String() + ", block 0",
			edit: func(_ *MergeOptions, inputs []MergeInput, _ func()) {
				g := newGatedReader(late)
				g.fail = func(off int64, _ int) bool { return off == lateOff }
				inputs[2].ReaderAt = g
			},
		},
		{
			name: "MaxOpenBlocks", files: append([][]byte{a, b}, deep...), want: ErrMergeLimit, msg: "overlap 3 deep at seq 14",
			edit: func(opts *MergeOptions, _ []MergeInput, _ func()) { opts.MaxOpenBlocks = 2 },
		},
		{
			name: "a block over MaxBlockLen", files: [][]byte{a, b, big}, want: ErrReadLimit, msg: "uncompressed_len",
			edit: func(opts *MergeOptions, _ []MergeInput, _ func()) { opts.Reader.MaxBlockLen = 1024 },
		},
		{
			name: "a cancellation", files: [][]byte{a, b, late}, want: context.Canceled,
			edit: func(opts *MergeOptions, _ []MergeInput, cancel func()) {
				onConflict := opts.OnConflict
				opts.OnConflict = func(c Conflict) {
					onConflict(c)
					cancel()
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			v := mergeView(t, nil, tt.files...)
			inputs := inputsOf(t, v, tt.files...)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var log conflictLog
			opts := mergeOpts()
			opts.OnConflict = log.add
			if tt.edit != nil {
				tt.edit(&opts, inputs, cancel)
			}

			out, rep, err := mergeBytes(ctx, v, inputs, opts)
			require.ErrorIs(t, err, tt.want)
			require.ErrorContains(t, err, tt.msg)
			assert.NotErrorIs(t, err, ErrMergeConflict)
			assert.Equal(t, MergeReport{Conflicts: 1}, rep, "the conflict found, not complete")
			assert.Equal(t, []Conflict{{CaptureID: viewCapture, Seq: 12, Versions: [][]UUID{{seg0}, {seg1}}}}, log.got)
			requireNoTrailer(t, out, 0)
		})
	}
}

// TestMergeCancelledAfterTheLastConflict cancels ctx while OnConflict receives the conflict of the last cluster,
// whose blocks were read before it, with fewer than 4096 records resolved, so no check of ctx follows:
// every block was compared, so Merge returns ErrMergeConflict with ConflictsComplete, not ctx's error.
func TestMergeCancelledAfterTheLastConflict(t *testing.T) {
	t.Parallel()

	early := mergePack(t, seg4, nil, hourSteps(1, 4, 4))
	steps := hourSteps(10, 13, 4)
	a, b := mergePack(t, seg0, nil, steps), mergePack(t, seg1, nil, changed(steps, 1, 12))
	v := mergeView(t, nil, a, b, early)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var log conflictLog
	opts := mergeOpts()
	opts.OnConflict = func(c Conflict) {
		log.add(c)
		cancel()
	}
	out, rep, err := mergeBytes(ctx, v, inputsOf(t, v, a, b, early), opts)
	require.ErrorIs(t, err, ErrMergeConflict)
	assert.NotErrorIs(t, err, context.Canceled)
	assert.Equal(t, MergeReport{Conflicts: 1, ConflictsComplete: true}, rep)
	assert.Equal(t, []Conflict{{CaptureID: viewCapture, Seq: 12, Versions: [][]UUID{{seg0}, {seg1}}}}, log.got)
	// The early cluster waited in its coalescing group, which the conflict dropped.
	requireNoTrailer(t, out, 0)
}

// TestMergeMaxOpenBlocks merges a chain of nineteen blocks that overlap two deep,
// a defective block that would be a second open block, and a cluster of five byte-identical blocks:
// the chain holds two blocks at once, its blocks released as their last records are resolved, and one is too few for it;
// the defective block is refused as an input's defect before the bound is checked;
// the duplicates, chosen from F-2, are dropped under a bound of one.
func TestMergeMaxOpenBlocks(t *testing.T) {
	t.Parallel()

	var even, odd [][]uint64
	for first := uint64(100); first < 200; first += 10 {
		even = append(even, seqRun(first, first+9))
		if first < 190 {
			odd = append(odd, seqRun(first+5, first+14))
		}
	}
	x, y := mergePack(t, seg0, nil, seqBlocks(even...)), mergePack(t, seg1, nil, seqBlocks(odd...))
	chain := mergeView(t, nil, x, y)

	t.Run("a chain two deep", func(t *testing.T) {
		t.Parallel()

		opts := mergeOpts()
		opts.MaxOpenBlocks = 2
		_, rep := mustMerge(t, chain, inputsOf(t, chain, x, y), opts)
		requireCounts(t, rep, mergeCounts{blocks: 1, records: 100, resolved: 19})

		var loads []loadEvent
		_, _, err := writeWithHooks(t, chain, inputsOf(t, chain, x, y), opts, func(p *mergePlan) {
			p.loadHook = func(b *mergeBlock, open, resolved int) {
				loads = append(loads, loadEvent{input: b.input, first: b.first, open: open, resolved: resolved})
			}
		})
		require.NoError(t, err)
		require.Len(t, loads, 19)
		for i, l := range loads {
			assert.LessOrEqual(t, l.open, 1, "load %d: the blocks before it released", i)
			assert.Equal(t, int(l.first-100), l.resolved, "load %d: loaded when its first seq is reached", i)
		}
	})

	t.Run("a chain over MaxOpenBlocks", func(t *testing.T) {
		t.Parallel()

		opts := mergeOpts()
		opts.MaxOpenBlocks = 1
		out, rep, err := mergeBytes(t.Context(), chain, inputsOf(t, chain, x, y), opts)
		require.ErrorIs(t, err, ErrMergeLimit)
		assert.NotErrorIs(t, err, ErrMergeInput)
		require.ErrorContains(t, err, "the blocks of seqs 100 to 199 overlap 2 deep at seq 105, above MaxOpenBlocks 1")
		assert.Equal(t, MergeReport{}, rep)
		requireNoTrailer(t, out, 0)
	})

	t.Run("a defective block over MaxOpenBlocks", func(t *testing.T) {
		t.Parallel()

		// corrupt's block, inside a's range, would be a second open block; its read fails first, on its body CRC.
		a := mergePack(t, seg0, nil, hourSteps(10, 16, 7))
		corrupt := mergePack(t, seg1, nil, hourSteps(14, 16, 3))
		corrupt = flipByte(corrupt, mustOpen(t, corrupt, ReaderOptions{}).Blocks()[0].Offset+format.EnvelopeLen+1)
		v := mergeView(t, nil, a, corrupt)
		opts := mergeOpts()
		opts.MaxOpenBlocks = 1
		out, rep, err := mergeBytes(t.Context(), v, inputsOf(t, v, a, corrupt), opts)
		require.ErrorIs(t, err, ErrMergeInput)
		assert.NotErrorIs(t, err, ErrMergeLimit)
		require.ErrorContains(t, err, "input 1, pack "+seg1.String()+": ")
		require.ErrorContains(t, err, "body CRC")
		assert.Equal(t, MergeReport{}, rep)
		requireNoTrailer(t, out, 0)
	})

	t.Run("a duplicate-only cluster", func(t *testing.T) {
		t.Parallel()

		base := mergePack(t, seg0, nil, hourSteps(10, 13, 4))
		files := [][]byte{
			base, withIDs(t, base, seg1, viewCapture), withIDs(t, base, seg2, viewCapture),
			withIDs(t, base, seg3, viewCapture), withIDs(t, base, seg4, viewCapture),
		}
		v := mergeView(t, nil, files...)
		opts := mergeOpts()
		opts.MaxOpenBlocks = 1
		out, rep := mustMerge(t, v, inputsOf(t, v, files...), opts)
		requireCopied(t, rep, 1, 4, 4)
		assert.Equal(t, blockBytes(t, base, allBlocks), blockBytes(t, out, allBlocks))
	})
}

// TestMergeStreamingFrontier merges A = {0, 1000}, B = {1, 2} and C = {3, 4}, one block each:
// B is loaded once seq 0 is resolved, before seq 1, and C once seq 2 is, before seq 3 and after B was released,
// never all at once from the stale smallest seq 1000 that A holds after seq 0.
func TestMergeStreamingFrontier(t *testing.T) {
	t.Parallel()

	a := mergePack(t, seg0, nil, seqBlocks([]uint64{0, 1000}))
	b := mergePack(t, seg1, nil, seqBlocks([]uint64{1, 2}))
	c := mergePack(t, seg2, nil, seqBlocks([]uint64{3, 4}))
	v := mergeView(t, nil, a, b, c)
	require.Equal(t, []UUID{seg0, seg1, seg2}, v.Packs)

	out, rep := mustMerge(t, v, inputsOf(t, v, a, b, c), mergeOpts())
	requireCounts(t, rep, mergeCounts{blocks: 1, records: 6, resolved: 3})
	seqs := make([]uint64, 0, 6)
	for _, r := range rawRecordsOf(t, out) {
		seqs = append(seqs, r.seq)
	}
	assert.Equal(t, []uint64{0, 1, 2, 3, 4, 1000}, seqs)

	var loads []loadEvent
	_, _, err := writeWithHooks(t, v, inputsOf(t, v, a, b, c), mergeOpts(), func(p *mergePlan) {
		p.loadHook = func(b *mergeBlock, open, resolved int) {
			loads = append(loads, loadEvent{input: b.input, first: b.first, open: open, resolved: resolved})
		}
	})
	require.NoError(t, err)
	assert.Equal(t, []loadEvent{
		{input: 0, first: 0, open: 0, resolved: 0},
		{input: 1, first: 1, open: 1, resolved: 1},
		{input: 2, first: 3, open: 1, resolved: 3},
	}, loads)
}

// TestMergeCancelledDuringResolution cancels a merge while it resolves two blocks of 5000 records each,
// both loaded before the first record is resolved, so no block read checks ctx after the cancellation:
// the check after every 4096 records resolved stops the merge, without a trailer.
func TestMergeCancelledDuringResolution(t *testing.T) {
	t.Parallel()

	const records = 5000
	// pack writes seqs 0 to records-1 in one block with codec c.
	pack := func(id UUID, c Codec) []byte {
		m := repairTestMeta()
		var buf bytes.Buffer
		w, err := NewWriter(&buf, WriterOptions{Meta: m, Codec: c, BlockThreshold: 1 << 20})
		require.NoError(t, err)
		for seq := range uint64(records) {
			rec := testDataRecord(seq, blockTestHour+int64(seq), 1)
			require.NoError(t, w.Append(&rec))
		}
		_, err = w.Close()
		require.NoError(t, err)

		return withIDs(t, buf.Bytes(), id, viewCapture)
	}
	a, b := pack(seg0, CodecZstd), pack(seg1, CodecNone)
	require.Len(t, mustOpen(t, a, ReaderOptions{}).Blocks(), 1)
	v := mergeView(t, nil, a, b)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// The resolution writes blocks of 16 records; the header is Write 1 and the first block Write 2.
	dst := &cancelWriter{n: 2, cancel: cancel}
	opts := fixedIDs(mergeOpts())
	opts.BlockThreshold = 16 * testRecordLen
	rep, err := Merge(ctx, dst, v, inputsOf(t, v, a, b), opts)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, MergeReport{}, rep)
	// A block is written when the record after it arrives, so by the check at record 4096
	// the blocks of records 0 to 4079 are written and the block of 4080 to 4095 waits.
	requireNoTrailer(t, dst.buf.Bytes(), 4080/16)
	assert.Equal(t, 1+4080/16, dst.calls)
}

// TestMergeResolutionEncodingFails merges a copied cluster, then a cluster to resolve,
// whose first new block fails its in-memory check through an injected change of its on-disk body:
// the last block of the cluster, encoded once the cluster is resolved,
// or a block encoded before the cluster ends, when the next record does not fit it.
// The merge fails without a trailer, and not as an input's defect:
// under the default threshold the copied block waits in its coalescing group and is not written,
// while under a threshold of two records it is a group of its own, written before the failure.
func TestMergeResolutionEncodingFails(t *testing.T) {
	t.Parallel()

	early := mergePack(t, seg4, nil, hourSteps(1, 4, 4))
	a := mergePackCodec(t, CodecZstd, seg0, hourSteps(10, 13, 4))
	b := mergePackCodec(t, CodecNone, seg1, hourSteps(10, 13, 2))
	v := mergeView(t, nil, early, a, b)

	tests := []struct {
		name string
		// threshold is the BlockThreshold: 0 for one block of the cluster's four records, or two records' size;
		// blocks is the number of blocks written before the failure.
		threshold, blocks int
	}{
		{name: "the last block of the cluster"},
		{name: "a block before the cluster ends", threshold: 2 * testRecordLen, blocks: 1},
	}
	for _, c := range []Codec{CodecNone, CodecZstd} {
		for _, tt := range tests {
			t.Run(c.String()+"/"+tt.name, func(t *testing.T) {
				t.Parallel()

				opts := mergeOpts()
				opts.Codec, opts.BlockThreshold = c, tt.threshold
				var hooked int
				out, rep, err := writeWithHooks(t, v, inputsOf(t, v, early, a, b), opts, func(p *mergePlan) {
					p.encodedHook = func(enc []byte) []byte {
						hooked++
						enc[len(enc)-1] ^= 1

						return enc
					}
				})
				require.ErrorContains(t, err, "the new block of seq 10 fails its check")
				for _, sentinel := range []error{ErrMergeInput, ErrMergeLimit, ErrMergeConflict} {
					assert.NotErrorIs(t, err, sentinel)
				}
				assert.Equal(t, MergeReport{}, rep)
				assert.Equal(t, 1, hooked, "the merge stops at the first failed block")
				requireNoTrailer(t, out, tt.blocks)
			})
		}
	}
}

// TestCursorHeap pushes and pops open blocks, whose next records' seqs often tie,
// in a seeded random order that holds up to 24 blocks at once,
// and requires every pop to return the smallest (seq, position) held, as a sorted reference gives it.
// A block popped is often pushed again with a later seq, as the resolution advances its cursor.
func TestCursorHeap(t *testing.T) {
	t.Parallel()

	rng := rand.New(rand.NewPCG(1, 2))
	var h cursorHeap
	var ref []*openBlock
	byOrder := func(a, b *openBlock) int {
		return cmp.Or(cmp.Compare(a.seq, b.seq), cmp.Compare(a.pos, b.pos))
	}
	pop := func() *openBlock {
		slices.SortFunc(ref, byOrder)
		want := ref[0]
		ref = ref[1:]
		got := h.pop()
		require.Same(t, want, got, "seq %d pos %d, got seq %d pos %d", want.seq, want.pos, got.seq, got.pos)

		return got
	}

	pos, most := 0, 0
	for range 5000 {
		if len(h) == 0 || (len(h) < 24 && rng.IntN(5) < 3) {
			o := &openBlock{pos: pos, seq: rng.Uint64N(6)}
			pos++
			h.push(o)
			ref = append(ref, o)
		} else if o := pop(); rng.IntN(2) == 0 {
			o.seq += rng.Uint64N(3)
			h.push(o)
			ref = append(ref, o)
		}
		require.Len(t, h, len(ref))
		most = max(most, len(h))
	}
	for len(h) > 0 {
		pop()
	}
	assert.GreaterOrEqual(t, most, 16, "the heap held enough blocks to reach every branch")
}
