package tracepack

import (
	"bytes"
	"cmp"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"runtime"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/codec"
	"github.com/arloliu/go-secs/tracepack/internal/format"
	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// The ids fixedIDs gives the archives of the merge tests.
var (
	archiveID  = UUID{0xAC, 0x01}
	archiveSet = UUID{0x5E, 0xAC}
	// seg4 is a fifth segment of the merge test scopes.
	seg4 = UUID{0x54}
)

// cancelWriter writes into buf and calls cancel once its nth Write has written.
type cancelWriter struct {
	buf    bytes.Buffer
	n      int
	calls  int
	cancel func()
}

func (c *cancelWriter) Write(p []byte) (int, error) {
	n, err := c.buf.Write(p)
	c.calls++
	if c.calls == c.n {
		c.cancel()
	}

	return n, err
}

// mergePackCodec is mergePack of the default metadata, with blocks and footer written with codec c.
func mergePackCodec(t testing.TB, c Codec, id UUID, steps []footerTestStep) []byte {
	t.Helper()

	return withIDs(t, writeRepairPack(t, c, nil, false, steps).file, id, viewCapture)
}

// fixedIDs returns opts with the archive's pack_id set to archiveID and its replacement_set_id to archiveSet,
// where opts leaves them zero, so every merge of the same inputs writes the same pack metadata.
func fixedIDs(opts MergeOptions) MergeOptions {
	if opts.PackID.IsZero() {
		opts.PackID = archiveID
	}
	if opts.ReplacementSetID.IsZero() {
		opts.ReplacementSetID = archiveSet
	}

	return opts
}

// mergeBytes runs Merge into a buffer with fixedIDs(opts) and returns what it wrote, its report and its error.
func mergeBytes(ctx context.Context, v View, inputs []MergeInput, opts MergeOptions) ([]byte, MergeReport, error) {
	var out bytes.Buffer
	rep, err := Merge(ctx, &out, v, inputs, fixedIDs(opts))

	return out.Bytes(), rep, err
}

// mustMerge merges inputs, which v.Packs names, with fixedIDs(opts) and requires the archive to be written:
// finalized-consistent and rep.Size bytes long;
// its file header carrying rep's pack_id, the view's capture exactly and no redaction-present flag;
// its pack metadata the one planMerge plans, byte for byte;
// its records those of the inputs, each once, as mergedRecords reads them;
// and no conflict in rep, whose blocks and records are the archive's, and whose counts agree (requireCountsAgree).
// It returns the archive and the report, whose block counts the caller checks.
func mustMerge(t *testing.T, v View, inputs []MergeInput, opts MergeOptions) ([]byte, MergeReport) {
	t.Helper()

	opts = fixedIDs(opts)
	out, rep, err := mergeBytes(t.Context(), v, inputs, opts)
	require.NoError(t, err)
	requireConsistent(t, out)
	assert.Equal(t, uint64(len(out)), rep.Size)
	assert.Equal(t, opts.PackID, rep.PackID)
	assert.Equal(t, opts.ReplacementSetID, rep.ReplacementSetID)
	assert.Equal(t, len(inputs), rep.Inputs)
	assert.True(t, rep.ConflictsComplete)
	assert.Zero(t, rep.Conflicts)

	hdr := layoutHeader(t, out)
	assert.Equal(t, format.UUID(opts.PackID), hdr.PackID)
	assert.Equal(t, format.UUID(v.Capture), hdr.CaptureID, "the scope's capture exactly")
	assert.Zero(t, hdr.Flags&fileHeaderRedactionPresent)

	want, err := mustPlanMerge(t, v, inputs, opts).meta.MarshalBinary()
	require.NoError(t, err)
	assert.Equal(t, want, mustOpen(t, out, ReaderOptions{}).metaRaw, "the planned pack metadata")

	files := inputFiles(t, inputs)
	records := make([][]rawRecord, len(files))
	for i, f := range files {
		records[i] = rawRecordsOf(t, f)
	}
	assert.Equal(t, mergedRecords(t, slices.Concat(records...)), rawRecordsOf(t, out), "the inputs' records, each once, in seq order")
	tr := layoutOf(t, out).tr
	assert.Equal(t, int(tr.BlockCount), rep.Blocks)
	assert.Equal(t, tr.RecordCount, rep.Records)
	requireCountsAgree(t, files, out, rep)

	return out, rep
}

// requireCountsAgree requires the counts of rep, the report of a merge of the pack files inputs into out, to agree.
// Every input block is counted exactly once:
// copied, dropped as a duplicate, resolved, or coalesced into a new block with other blocks.
// Every block written is an input block copied, a block the resolution encoded and did not coalesce,
// or a coalesced block, whose group took at least two blocks, input blocks or blocks the resolution encoded:
// so without a block of the resolution, the blocks written are the blocks copied and the coalesced blocks,
// and without a coalesced block, no input block is coalesced and the blocks written are those copied or resolved.
// Each block copied is an input block, its bytes and its F-3 list byte for byte.
func requireCountsAgree(t *testing.T, inputs [][]byte, out []byte, rep MergeReport) {
	t.Helper()

	type block struct{ bytes, f3 string }
	kept := make(map[block]struct{})
	blocks := 0
	for _, f := range inputs {
		lists := f3ListsOf(t, f)
		for i, b := range blockBytes(t, f, allBlocks) {
			kept[block{string(b), string(lists[i])}] = struct{}{}
		}
		blocks += len(lists)
	}
	assert.Equal(t, blocks, rep.Copied+rep.Duplicates+rep.Resolved+rep.Coalesced, "each input block copied, dropped, resolved or coalesced")
	// twice is the number of blocks the resolution encoded that were coalesced, so encoded twice.
	twice := rep.Copied + rep.ResolvedEncodings + rep.CoalescedEncodings - rep.Blocks
	assert.GreaterOrEqual(t, twice, 0, "blocks of the resolution coalesced")
	assert.LessOrEqual(t, twice, rep.ResolvedEncodings, "blocks of the resolution coalesced")
	if rep.CoalescedEncodings == 0 {
		assert.Zero(t, rep.Coalesced, "without a coalesced block, no input block is coalesced")
		assert.Zero(t, twice, "without a coalesced block, every block of the resolution is written as it encoded it")
	} else {
		assert.GreaterOrEqual(t, rep.Coalesced+twice, 2*rep.CoalescedEncodings, "each coalesced block from two blocks or more")
	}

	copied := 0
	lists := f3ListsOf(t, out)
	for i, b := range blockBytes(t, out, allBlocks) {
		if _, ok := kept[block{string(b), string(lists[i])}]; ok {
			copied++
		}
	}
	assert.GreaterOrEqual(t, copied, rep.Copied, "each block copied is an input block, with its F-3 list")
}

// requireCopied requires rep to count blocks blocks written, of records records, every one an input block copied verbatim,
// and dups input blocks dropped as duplicates, with no block resolved or encoded.
func requireCopied(t *testing.T, rep MergeReport, blocks int, records uint64, dups int) {
	t.Helper()

	requireCounts(t, rep, mergeCounts{blocks: blocks, records: records, copied: blocks, dups: dups})
}

// mergeCounts is what a merge's report counts of the blocks it wrote and of its input blocks.
type mergeCounts struct {
	// blocks is the number of blocks written, records the number of records written,
	// copied the number of blocks written that are input blocks copied verbatim,
	// and coalescedEncodings the number of blocks written that coalescing encoded;
	// the others are blocks the resolution encoded.
	blocks             int
	records            uint64
	copied             int
	coalescedEncodings int
	// dups is the number of input blocks dropped as duplicates, resolved the number resolved record by record,
	// and coalesced the number coalesced into a new block.
	dups      int
	resolved  int
	coalesced int
	// twice is the number of blocks the resolution encoded that coalescing encoded again, with other blocks,
	// and fallbacks the number of groups whose encoding failed, written as their blocks.
	twice     int
	fallbacks int
}

// requireCounts requires rep to count want.
func requireCounts(t *testing.T, rep MergeReport, want mergeCounts) {
	t.Helper()

	assert.Equal(t, want.blocks, rep.Blocks, "blocks written")
	assert.Equal(t, want.records, rep.Records, "records written")
	assert.Equal(t, want.copied, rep.Copied, "input blocks copied verbatim")
	assert.Equal(t, want.blocks-want.copied-want.coalescedEncodings+want.twice, rep.ResolvedEncodings, "blocks encoded by the resolution")
	assert.Equal(t, want.coalescedEncodings, rep.CoalescedEncodings, "blocks encoded by coalescing")
	assert.Equal(t, want.fallbacks, rep.CoalesceFallbacks, "groups written as their blocks")
	assert.Equal(t, want.dups, rep.Duplicates, "duplicates dropped")
	assert.Equal(t, want.resolved, rep.Resolved, "input blocks resolved")
	assert.Equal(t, want.coalesced, rep.Coalesced, "input blocks coalesced")
}

// rawRecord is one record as its block stores it:
// its seq, its header row, record_header_len bytes as stored, and its payload.
type rawRecord struct {
	seq     uint64
	row     []byte
	payload []byte
}

// rawRecordsOf returns every record of the finalized pack file, in file order, as its block stores it:
// each block decoded from its bytes as the tracepack format specification §6 lays them out,
// without the Reader's record path or Verify.
func rawRecordsOf(t testing.TB, file []byte) []rawRecord {
	t.Helper()

	var out []rawRecord
	for _, b := range mustOpen(t, file, ReaderOptions{}).Blocks() {
		blk := file[b.Offset : b.Offset+uint64(b.OnDiskLen)]
		env, err := format.UnmarshalBlockEnvelope(blk)
		require.NoError(t, err)
		body, err := codec.Decode(env.Codec, nil, blk[format.EnvelopeLen:], int(env.UncompressedLen))
		require.NoError(t, err)

		n, rhl := int(env.RecordCount), int(env.RecordHeaderLen)
		rows := format.UntransposeHeaders(nil, body[:n*rhl], n, rhl)
		off := n * rhl
		for j := range n {
			row := rows[j*rhl : (j+1)*rhl]
			h, err := format.UnmarshalRecordHeader(row, format.RecordHeaderLen)
			require.NoError(t, err)
			end := off + int(h.PayloadLen)
			out = append(out, rawRecord{seq: h.Seq, row: row, payload: body[off:end]})
			off = end
		}
	}

	return out
}

// mergedRecords returns records in ascending seq, keeping the first of those of one seq,
// which must all be equal, header row and payload byte for byte: the records a merge writes of its inputs' records.
func mergedRecords(t testing.TB, records []rawRecord) []rawRecord {
	t.Helper()

	sorted := slices.SortedStableFunc(slices.Values(records), func(a, b rawRecord) int { return cmp.Compare(a.seq, b.seq) })
	var out []rawRecord
	for _, r := range sorted {
		if n := len(out); n > 0 && out[n-1].seq == r.seq {
			require.Equal(t, out[n-1], r, "two records of seq %d differ", r.seq)
			continue
		}
		out = append(out, r)
	}

	return out
}

// inputFiles returns the bytes of every merge input, each read with one ReadAt of the whole object,
// which no test that counts an input's block reads mistakes for a block read.
func inputFiles(t testing.TB, inputs []MergeInput) [][]byte {
	t.Helper()

	out := make([][]byte, len(inputs))
	for i, in := range inputs {
		out[i] = make([]byte, in.Size)
		n, err := in.ReaderAt.ReadAt(out[i], 0)
		if !errors.Is(err, io.EOF) {
			require.NoError(t, err)
		}
		require.Equal(t, int(in.Size), n)
	}

	return out
}

// conflictLog collects the conflicts a merge reports, as its MergeOptions.OnConflict.
type conflictLog struct {
	got []Conflict
}

// add is the MergeOptions.OnConflict of l.
func (l *conflictLog) add(c Conflict) {
	l.got = append(l.got, c)
}

// requireNoTrailer requires out to be a pack that is not finalized, whose first blocks blocks validate:
// what a merge that failed after its header wrote.
func requireNoTrailer(t *testing.T, out []byte, blocks int) {
	t.Helper()

	v := mustVerify(t, out)
	assert.Equal(t, OutcomeUnfinalized, v.Outcome)
	assert.Equal(t, blocks, v.Validated, "the blocks written before the error")
}

// recordsOf returns every record of the finalized pack file in seq order, with its header extension area,
// without the index of the block holding it; a coverage entry of the pack does not make it fail.
func recordsOf(t testing.TB, file []byte) []iterItem {
	t.Helper()

	run := iterate(t, mustOpen(t, file, ReaderOptions{}), Query{Payloads: true})
	for i := range run.items {
		run.items[i].block = 0
	}

	return run.items
}

// allBlocks selects every block for blockBytes.
func allBlocks(int) bool { return true }

// f3ListsOf returns every block's F-3 entry list as the finalized pack file stores it, in file order.
func f3ListsOf(t testing.TB, file []byte) [][]byte {
	t.Helper()

	return rawF3Lists(mustOpenKeepingF3(t, file, ReaderOptions{}))
}

// unusualF3File returns file, a finalized pack, with every block's F-3 list changed by unusualF3List.
func unusualF3File(t testing.TB, file []byte) []byte {
	t.Helper()

	parts := splitFooter(t, footerOf(t, file))
	for i := range parts.f3 {
		parts.f3[i] = unusualF3List(t, parts.f3[i])
	}

	return refooter(t, file, parts.encode())
}

// nestedF3Pack returns file, a finalized pack of at least one block, with its F-3 section rebuilt so that the lists nest:
// each block's list is its own entries followed by an unknown entry holding the next block's list,
// and the last block's list ends with an unknown entry of pad bytes.
// Every list is a suffix of F-3, so the section is about pad bytes long while the lists together are about pad bytes each,
// which the footer validation of the tracepack format specification §10 accepts:
// it requires each list inside F-3, not disjoint lists.
// It also returns each block's list as the rebuilt footer stores it.
func nestedF3Pack(t testing.TB, file []byte, pad int) ([]byte, [][]byte) {
	t.Helper()

	parts := splitFooter(t, footerOf(t, file))
	n := len(parts.f3Raw)
	require.Positive(t, n)
	lists := make([][]byte, n)
	lists[n-1] = tlv.AppendEntry(slices.Clone(parts.f3Raw[n-1]), tlv.BytesEntry(0x0100, make([]byte, pad)))
	for i := n - 2; i >= 0; i-- {
		lists[i] = tlv.AppendEntry(slices.Clone(parts.f3Raw[i]), tlv.BytesEntry(0x0101, lists[i+1]))
	}
	f3 := lists[0]
	spans := make([]f3Span, n)
	for i := range lists {
		spans[i] = f3Span{offset: uint64(len(f3) - len(lists[i])), n: uint32(len(lists[i]))}
	}

	return refooter(t, file, parts.encodeF3(f3, spans)), lists
}

// resealedEnvelope returns file with a reserved byte of block i's envelope set and its envelope_crc recomputed:
// the block keeps its body and its F-2 entry, which holds no envelope_crc, while its envelope bytes change.
func resealedEnvelope(t testing.TB, file []byte, i int) []byte {
	t.Helper()

	out := bytes.Clone(file)
	env := out[mustOpen(t, file, ReaderOptions{}).Blocks()[i].Offset:][:format.EnvelopeLen]
	env[35]++
	binary.LittleEndian.PutUint32(env[36:], format.CRC(env[:36]))

	return out
}

// blockReads returns, for every block of the finalized pack file in file order, the number of calls of g
// that read exactly the block's envelope and on-disk body.
func blockReads(t testing.TB, file []byte, g *gatedReader) []int {
	t.Helper()

	blocks := mustOpen(t, file, ReaderOptions{}).Blocks()
	out := make([]int, len(blocks))
	for _, c := range g.allCalls() {
		for i, b := range blocks {
			if c.off == int64(b.Offset) && c.n == int(b.OnDiskLen) {
				out[i]++
			}
		}
	}

	return out
}

// loggedInputs returns the files that v.Packs names as merge inputs in that order, as inputsOf does,
// each read through a gatedReader without gated rounds, which logs every call.
func loggedInputs(t testing.TB, v View, files ...[]byte) ([]MergeInput, []*gatedReader) {
	t.Helper()

	ordered := filesOf(t, v, files...)
	inputs := make([]MergeInput, len(ordered))
	readers := make([]*gatedReader, len(ordered))
	for i, f := range ordered {
		readers[i] = newGatedReader(f)
		inputs[i] = MergeInput{ReaderAt: readers[i], Size: int64(len(f))}
	}

	return inputs, readers
}

func TestMergeCopiesNonOverlappingSegments(t *testing.T) {
	t.Parallel()

	for _, c := range []Codec{CodecNone, CodecZstd} {
		t.Run(c.String(), func(t *testing.T) {
			t.Parallel()

			// seg1 holds the scope's first records, so the clusters are written in seq order, not in input order;
			// seg2's last block follows a seq gap.
			a := mergePackCodec(t, c, seg0, hourSteps(30, 49, 5))
			b := mergePackCodec(t, c, seg1, hourSteps(10, 29, 7))
			d := mergePackCodec(t, c, seg2, append(hourSteps(50, 53, 2), hourSteps(70, 71, 2)...))
			files := [][]byte{a, b, d}
			v := mergeView(t, nil, files...)
			require.Equal(t, []UUID{seg0, seg1, seg2}, v.Packs)

			opts := mergeOpts()
			opts.Codec, opts.NoCoalesce = c, true
			out, rep := mustMerge(t, v, inputsOf(t, v, files...), opts)
			requireCopied(t, rep, 4+3+3, 46, 0)
			assert.Equal(t, uint64(10), mustOpen(t, out, ReaderOptions{}).Header().Meta.SeqStart)

			assert.Equal(t, slices.Concat(recordsOf(t, b), recordsOf(t, a), recordsOf(t, d)), recordsOf(t, out), "the inputs' records in seq order")
			want := slices.Concat(blockBytes(t, b, allBlocks), blockBytes(t, a, allBlocks), blockBytes(t, d, allBlocks))
			assert.Equal(t, want, blockBytes(t, out, allBlocks), "every block copied verbatim, in seq order")
		})
	}
}

// TestMergeCopiesBlocksByteForByte merges blocks that no other block overlaps:
// without coalescing, each is copied with its envelope and on-disk body,
// and its F-3 list as its input's footer stores it, whatever the list holds, and nothing is encoded.
// With coalescing, blocks of unequal record_header_len are left apart, so seg0's first four blocks,
// whose record_header_len alternates, are each a group of one, copied byte for byte with its list,
// while seg0's last block and the blocks of seg1 and seg2 are coalesced into one new block.
func TestMergeCopiesBlocksByteForByte(t *testing.T) {
	t.Parallel()

	// seg0's blocks hold boundaries, epochs and seq gaps, blocks 1 and 3 with 52-byte record headers,
	// and F-3 lists that no Writer writes: unknown, reserved and retired tags, reordered entries.
	a := unusualF3File(t, widenFile(t, mergePack(t, seg0, nil, repairSteps(t)), func(i int) bool { return i%2 == 1 }))
	b := mergePackCodec(t, CodecNone, seg1, hourSteps(40, 45, 3))
	// seg2's F-3 lists nest, each inside the one before it.
	d, nested := nestedF3Pack(t, mergePack(t, seg2, nil, hourSteps(50, 57, 2)), 3)
	files := [][]byte{a, b, d}
	for _, f := range files {
		requireConsistent(t, f)
	}
	v := mergeView(t, nil, files...)

	opts := mergeOpts()
	opts.NoCoalesce = true
	out, rep := mustMerge(t, v, inputsOf(t, v, files...), opts)
	requireCopied(t, rep, 5+2+4, 11+6+8, 0)

	want := slices.Concat(blockBytes(t, a, allBlocks), blockBytes(t, b, allBlocks), blockBytes(t, d, allBlocks))
	assert.Equal(t, want, blockBytes(t, out, allBlocks))
	lists := slices.Concat(f3ListsOf(t, a), f3ListsOf(t, b), nested)
	assert.Equal(t, lists, f3ListsOf(t, out), "every F-3 list whole, byte for byte")
	assert.NotEqual(t, appendF3(nil, &mustOpen(t, out, ReaderOptions{}).footer.blocks[0]), lists[0], "a list is not rebuilt")

	opts.NoCoalesce = false
	coalesced, rep := mustMerge(t, v, inputsOf(t, v, files...), opts)
	requireCounts(t, rep, mergeCounts{blocks: 4 + 1, records: 11 + 6 + 8, copied: 4, coalescedEncodings: 1, coalesced: 7})
	wide := format.RecordHeaderLen + wideExtraLen
	h := format.RecordHeaderLen
	assert.Equal(t, [][2]int{{3, h}, {3, wide}, {2, h}, {2, wide}, {1 + 6 + 8, h}}, blockShapes(t, coalesced))
	assert.Equal(t, want[:4], blockBytes(t, coalesced, allBlocks)[:4], "the groups of one block copied verbatim")
	assert.Equal(t, lists[:4], f3ListsOf(t, coalesced)[:4], "with their F-3 lists")
	assert.Equal(t, recordsOf(t, out), recordsOf(t, coalesced))
}

func TestMergeDropsExactDuplicates(t *testing.T) {
	t.Parallel()

	a := mergePack(t, seg0, nil, hourSteps(10, 19, 5))
	// seg1 and seg3 hold seg0's blocks byte for byte: the same pack under other ids.
	b, d := withIDs(t, a, seg1, viewCapture), withIDs(t, a, seg3, viewCapture)
	e := mergePack(t, seg2, nil, hourSteps(20, 23, 2))
	files := [][]byte{a, b, e, d}
	v := mergeView(t, nil, files...)
	require.Equal(t, []UUID{seg0, seg1, seg2, seg3}, v.Packs)

	t.Run("dropped", func(t *testing.T) {
		t.Parallel()

		opts := mergeOpts()
		opts.NoCoalesce = true
		out, rep := mustMerge(t, v, inputsOf(t, v, files...), opts)
		requireCopied(t, rep, 4, 14, 4)
		assert.Equal(t, slices.Concat(blockBytes(t, a, allBlocks), blockBytes(t, e, allBlocks)), blockBytes(t, out, allBlocks))

		// With coalescing, the blocks kept are coalesced into one; every block, each duplicate included, is read once.
		inputs, readers := loggedInputs(t, v, files...)
		_, rep = mustMerge(t, v, inputs, mergeOpts())
		requireCounts(t, rep, mergeCounts{blocks: 1, records: 14, coalescedEncodings: 1, coalesced: 4, dups: 4})
		for i, g := range readers {
			assert.Equal(t, []int{1, 1}, blockReads(t, files[i], g), "input %d", i)
		}
	})

	t.Run("a duplicate whose F-3 summary is false", func(t *testing.T) {
		t.Parallel()

		// seg1's block 1 is seg0's byte for byte, but its F-3 summary claims a quality bit no record carries.
		falseF3 := reindexedWith(t, b, func(i int, s *blockSummary) {
			if i == 1 {
				s.qualityUnion |= QualityOrderingUncertain
			}
		})
		require.Equal(t, blockBytes(t, a, allBlocks), blockBytes(t, falseF3, allBlocks))
		in := [][]byte{a, falseF3, e, d}
		fv := mergeView(t, nil, in...)
		out, rep, err := mergeBytes(t.Context(), fv, inputsOf(t, fv, in...), mergeOpts())
		require.ErrorIs(t, err, ErrMergeInput)
		require.ErrorContains(t, err, "input 1, pack "+seg1.String()+": tracepack: block 1 at offset")
		require.ErrorContains(t, err, "F-3 quality_union")
		assert.Equal(t, MergeReport{}, rep)
		// The first block waited in its coalescing group, which is not written.
		requireNoTrailer(t, out, 0)
	})
}

// TestMergeDuplicateCandidateWhoseBytesDiffer merges a cluster of four blocks of one F-2 identity:
// three byte-identical blocks, then a block whose envelope differs in a reserved byte, over the same body.
// Each block is read once; the first block keeps the origins of the two dropped as its duplicates,
// and the block that differs makes the cluster one that is resolved record by record, with the blocks already read.
func TestMergeDuplicateCandidateWhoseBytesDiffer(t *testing.T) {
	t.Parallel()

	early := mergePack(t, seg4, nil, hourSteps(1, 4, 4))
	base := mergePack(t, seg0, nil, hourSteps(10, 13, 4))
	s1, s2 := withIDs(t, base, seg1, viewCapture), withIDs(t, base, seg2, viewCapture)
	s3 := withIDs(t, resealedEnvelope(t, base, 0), seg3, viewCapture)
	requireConsistent(t, s3)
	files := [][]byte{early, base, s1, s2, s3}
	v := mergeView(t, nil, files...)
	require.Equal(t, []UUID{seg4, seg0, seg1, seg2, seg3}, v.Packs)

	t.Run("read", func(t *testing.T) {
		t.Parallel()

		inputs, readers := loggedInputs(t, v, files...)
		p := mustPlanMerge(t, v, inputs, mergeOpts())
		require.Len(t, p.clusters, 2)
		c := &p.clusters[1]
		require.Len(t, c.blocks, 4)
		require.True(t, p.duplicateCandidate(c), "one F-2 identity")

		m := &mergeRun{p: p}
		r := &resolution{m: m, c: c}
		require.NoError(t, r.loadFrontier(t.Context()))
		require.Len(t, r.open, 2, "the first block and the block that differs")
		first, differs := r.open[0], r.open[1]
		assert.Equal(t, [2]int{0, 3}, [2]int{first.pos, differs.pos})
		assert.Equal(t, []UUID{seg0, seg1, seg2}, originIDs(p, first), "the origins of every block dropped as its duplicate")
		assert.Equal(t, []UUID{seg3}, originIDs(p, differs))
		assert.Equal(t, 2, m.rep.Duplicates)
		assert.Equal(t, blockBytes(t, base, allBlocks)[0], first.buf.raw)
		assert.Equal(t, blockBytes(t, s3, allBlocks)[0], differs.buf.raw)
		for i, f := range files[1:] {
			assert.Equal(t, []int{1}, blockReads(t, f, readers[i+1]), "input %d", i+1)
		}
	})

	t.Run("merged", func(t *testing.T) {
		t.Parallel()

		inputs, readers := loggedInputs(t, v, files...)
		_, rep := mustMerge(t, v, inputs, mergeOpts())
		// The early block and the resolution's block are coalesced.
		requireCounts(t, rep, mergeCounts{blocks: 1, records: 8, coalescedEncodings: 1, coalesced: 1, dups: 2, resolved: 2, twice: 1})
		for i, f := range files {
			assert.Equal(t, []int{1}, blockReads(t, f, readers[i]), "input %d: one full read per block", i)
		}
	})
}

// TestMergeClusterOfSeveralIdentities merges clusters whose blocks have different F-2 entries,
// which Merge resolves record by record without first comparing their blocks' bytes, each block read once:
// the equal records of overlapping ranges are written once,
// while the same records under another record_header_len, and records one payload byte apart, are conflicts.
func TestMergeClusterOfSeveralIdentities(t *testing.T) {
	t.Parallel()

	early := mergePack(t, seg4, nil, hourSteps(1, 4, 4))
	// other's records are hourSteps', with the last System Bytes byte of record 12 changed:
	// with the none codec its block has the same seqs, record count and lengths, and another body_crc.
	steps := hourSteps(10, 13, 4)
	steps[2].rec.Payload = slices.Clone(steps[2].rec.Payload)
	steps[2].rec.Payload[len(steps[2].rec.Payload)-1]++
	same, other := mergePackCodec(t, CodecNone, seg0, hourSteps(10, 13, 4)), mergePackCodec(t, CodecNone, seg1, steps)
	sb, ob := mustOpen(t, same, ReaderOptions{}).Blocks()[0], mustOpen(t, other, ReaderOptions{}).Blocks()[0]
	sb.Offset, ob.Offset = 0, 0
	sb.BodyCRC, ob.BodyCRC = 0, 0
	require.Equal(t, sb, ob, "the F-2 entries differ in body_crc only")

	versions := [][]UUID{{seg0}, {seg1}}
	tests := []struct {
		name string
		a, b []byte
		// conflicts holds the seqs of the conflicts, each of versions; none for a merge that succeeds.
		conflicts []uint64
	}{
		{name: "overlapping ranges",
			a: mergePack(t, seg0, nil, hourSteps(10, 13, 4)), b: mergePack(t, seg1, nil, hourSteps(12, 15, 4))},
		// Different envelopes over the same records: another record_header_len, which changes the header section too.
		{name: "the same records under another record_header_len",
			a: mergePack(t, seg0, nil, hourSteps(10, 13, 4)),
			b: widenFile(t, mergePack(t, seg1, nil, hourSteps(10, 13, 4)), allBlocks), conflicts: []uint64{10, 11, 12, 13}},
		// Records of the same seqs and lengths, one payload byte apart.
		{name: "another body_crc", a: same, b: other, conflicts: []uint64{12}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			files := [][]byte{early, tt.a, tt.b}
			v := mergeView(t, nil, files...)
			require.Equal(t, []UUID{seg4, seg0, seg1}, v.Packs, "readers[i] reads files[i]")
			inputs, readers := loggedInputs(t, v, files...)
			if tt.conflicts == nil {
				_, rep := mustMerge(t, v, inputs, mergeOpts())
				// The early block and the resolution's block are coalesced.
				requireCounts(t, rep, mergeCounts{blocks: 1, records: 10, coalescedEncodings: 1, coalesced: 1, resolved: 2, twice: 1})
			} else {
				var log conflictLog
				opts := mergeOpts()
				opts.OnConflict = log.add
				out, rep, err := mergeBytes(t.Context(), v, inputs, opts)
				require.ErrorIs(t, err, ErrMergeConflict)
				assert.Equal(t, MergeReport{Conflicts: len(tt.conflicts), ConflictsComplete: true}, rep)
				want := make([]Conflict, len(tt.conflicts))
				for i, seq := range tt.conflicts {
					want[i] = Conflict{CaptureID: viewCapture, Seq: seq, Versions: versions}
				}
				assert.Equal(t, want, log.got)
				// The early block waited in its coalescing group, which the conflict dropped.
				requireNoTrailer(t, out, 0)
			}
			for i, f := range files {
				assert.Equal(t, []int{1}, blockReads(t, f, readers[i]), "input %d: one full read per block", i)
			}
		})
	}
}

// originIDs returns the pack_ids of the origins of the open block o of a merge of the plan p.
func originIDs(p *mergePlan, o *openBlock) []UUID {
	ids := make([]UUID, len(o.origins))
	for i, in := range o.origins {
		ids[i] = p.inputID(in)
	}

	return ids
}

func TestMergeRefusesDefectiveBlocks(t *testing.T) {
	t.Parallel()

	a := mergePack(t, seg0, nil, hourSteps(10, 15, 2))
	b := mergePack(t, seg1, nil, hourSteps(20, 23, 2))
	blocks := mustOpen(t, a, ReaderOptions{}).Blocks()
	require.Len(t, blocks, 3)
	// Each vector's footer is valid and its preflight passes; its block 1 fails when it is read,
	// while block 0 waits in its coalescing group, so no block is written.
	tests := []struct {
		name string
		file []byte
		msg  string
	}{
		{name: "a corrupt body", file: flipByte(a, blocks[1].Offset+format.EnvelopeLen+1), msg: "body CRC"},
		{name: "a block failing I-2", file: editBlock(t, a, 1, func(b *testBlock) { b.tail = []byte{0} }), msg: "uncompressed_len"},
		{name: "records disagreeing with the F-2 entry", file: editBlock(t, a, 1, func(b *testBlock) { b.headers[1].TSUTCNs++ }),
			msg: "records give ts_max"},
		{name: "records disagreeing with the F-3 summary", file: editBlock(t, a, 1, func(b *testBlock) {
			b.headers[0].Dir = uint8(DirEquipmentToHost)
		}), msg: "F-3 dir_counts"},
		// The preflight checks commitments from F-3 and F-5; the record itself is only seen when its block is read.
		{name: "a redacted record the F-3 summary hides", file: editBlock(t, a, 1, func(b *testBlock) {
			b.headers[0].Quality |= uint16(QualityRedacted)
		}), msg: "F-3 quality_union"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			require.NoError(t, mustOpen(t, tt.file, ReaderOptions{}).Header().FooterErr, "the footer is valid")
			v := mergeView(t, nil, tt.file, b)
			inputs := inputsOf(t, v, tt.file, b)
			mustPlanMerge(t, v, inputs, mergeOpts())

			out, rep, err := mergeBytes(t.Context(), v, inputs, mergeOpts())
			require.ErrorIs(t, err, ErrMergeInput)
			require.ErrorContains(t, err, "input 0, pack "+seg0.String()+": ")
			require.ErrorContains(t, err, "block 1 at offset")
			require.ErrorContains(t, err, tt.msg)
			assert.Equal(t, MergeReport{}, rep)
			requireNoTrailer(t, out, 0)
		})
	}

	v := mergeView(t, nil, a, b)
	t.Run("a failed read", func(t *testing.T) {
		t.Parallel()

		inputs := inputsOf(t, v, a, b)
		failing := newGatedReader(a)
		failing.fail = func(off int64, _ int) bool { return off == int64(blocks[1].Offset) }
		inputs[0].ReaderAt = failing
		out, rep, err := mergeBytes(t.Context(), v, inputs, mergeOpts())
		// A failed read is not the input's defect: a caller retries it instead of repairing the pack.
		require.ErrorIs(t, err, errInjected)
		require.ErrorContains(t, err, "input 0, pack "+seg0.String()+", block 1")
		assert.NotErrorIs(t, err, ErrMergeInput)
		assert.Equal(t, MergeReport{}, rep)
		requireNoTrailer(t, out, 0)
	})

	t.Run("a read cut short", func(t *testing.T) {
		t.Parallel()

		inputs := inputsOf(t, v, a, b)
		// The read of block 1 ends, with io.EOF, before a byte of it is read.
		inputs[0].ReaderAt = &blockReadAt{file: a, off: int64(blocks[1].Offset), n: int(blocks[1].OnDiskLen),
			hook: func(int, []byte) error { return io.EOF }}
		out, rep, err := mergeBytes(t.Context(), v, inputs, mergeOpts())
		// The input was opened whole, so the object changed since or its ReaderAt is inconsistent:
		// a read error, as verification returns it, not a defect of the pack.
		require.ErrorIs(t, err, io.ErrUnexpectedEOF)
		assert.NotErrorIs(t, err, ErrMergeInput)
		require.ErrorContains(t, err, "input 0, pack "+seg0.String()+", block 1")
		assert.Equal(t, MergeReport{}, rep)
		requireNoTrailer(t, out, 0)
	})
}

func TestMergeCancelledWhileWriting(t *testing.T) {
	t.Parallel()

	a := mergePack(t, seg0, nil, hourSteps(10, 15, 2))
	empty := mergePack(t, seg1, func(m *PackMeta) { m.SeqStart = 5 }, nil)
	// Without coalescing, Merge writes the header, each block, and the footer with the trailer, in one Write each.
	tests := []struct {
		name string
		file []byte
		// cancelAt is the Write after which ctx is cancelled; blocks is the number of blocks written by then.
		cancelAt, blocks int
	}{
		{name: "before the first block", file: a, cancelAt: 1, blocks: 0},
		{name: "before a block", file: a, cancelAt: 2, blocks: 1},
		{name: "after the last block", file: a, cancelAt: 4, blocks: 3},
		{name: "an empty output", file: empty, cancelAt: 1, blocks: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			v := mergeView(t, nil, tt.file)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			dst := &cancelWriter{n: tt.cancelAt, cancel: cancel}
			opts := mergeOpts()
			opts.NoCoalesce = true
			rep, err := Merge(ctx, dst, v, inputsOf(t, v, tt.file), opts)
			require.ErrorIs(t, err, context.Canceled)
			assert.NotErrorIs(t, err, ErrMergeInput)
			assert.Equal(t, MergeReport{}, rep)
			assert.Equal(t, tt.cancelAt, dst.calls, "nothing is written after the cancellation")
			requireNoTrailer(t, dst.buf.Bytes(), tt.blocks)
		})
	}
}

func TestMergeOutputFailures(t *testing.T) {
	t.Parallel()

	a := mergePack(t, seg0, nil, hourSteps(10, 15, 2))
	v := mergeView(t, nil, a)
	// Without coalescing, each of a's three blocks is written and synced on its own.
	noCoalesce := mergeOpts()
	noCoalesce.NoCoalesce = true
	good, _ := mustMerge(t, v, inputsOf(t, v, a), noCoalesce)
	l := layoutOf(t, good)
	head, footerAt := int(l.blocksStart), int(l.tr.FooterOffset)

	run := func(t *testing.T, dst io.Writer, sync Syncer) (MergeReport, error) {
		t.Helper()

		opts := fixedIDs(noCoalesce)
		opts.Sync = sync

		return Merge(t.Context(), dst, v, inputsOf(t, v, a), opts)
	}

	tests := []struct {
		name string
		dst  interface {
			io.Writer
			Bytes() []byte
		}
		sync Syncer
		want error
		// finalized reports that the bytes written form a finalized pack, which Merge still reports as a failure.
		finalized bool
	}{
		{name: "the header fails", dst: &failAfterWriterBytes{failAfterWriter{limit: 10}}, want: errTestWrite},
		{name: "the header written short", dst: &shortAfterWriterBytes{shortAfterWriter{limit: 10}}, want: io.ErrShortWrite},
		{name: "a block fails", dst: &failAfterWriterBytes{failAfterWriter{limit: head + 10}}, want: errTestWrite},
		{name: "a block written short", dst: &shortAfterWriterBytes{shortAfterWriter{limit: head + 10}}, want: io.ErrShortWrite},
		{name: "a block's sync fails", dst: &bytes.Buffer{}, sync: &syncFailer{ok: 1}, want: errTestSync},
		{name: "the footer fails", dst: &failAfterWriterBytes{failAfterWriter{limit: footerAt + 10}}, want: errTestWrite},
		{name: "the footer written short", dst: &shortAfterWriterBytes{shortAfterWriter{limit: footerAt + 10}}, want: io.ErrShortWrite},
		{name: "the trailer's sync fails", dst: &bytes.Buffer{}, sync: &syncFailer{ok: 3}, want: errTestSync, finalized: true},
		{name: "a full final write returns an error", dst: &failAfterWriterBytes{failAfterWriter{limit: footerAt, takeAll: true}},
			want: errTestWrite, finalized: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rep, err := run(t, tt.dst, tt.sync)
			require.ErrorIs(t, err, tt.want)
			assert.Equal(t, MergeReport{}, rep)
			out := tt.dst.Bytes()
			switch {
			case tt.finalized:
				// The file headers differ in writer_start_utc_ns only.
				assert.Equal(t, good[format.FileHeaderLen:], out[format.FileHeaderLen:], "the bytes form the archive, which the failure does not vouch for")
				requireConsistent(t, out)
			case len(out) >= head:
				assert.Equal(t, OutcomeUnfinalized, mustVerify(t, out).Outcome)
			default:
				assert.Less(t, len(out), head, "the header is incomplete")
			}
		})
	}
}

// failAfterWriterBytes is a failAfterWriter that returns what it wrote.
type failAfterWriterBytes struct {
	failAfterWriter
}

func (f *failAfterWriterBytes) Bytes() []byte { return f.buf.Bytes() }

// shortAfterWriterBytes is a shortAfterWriter that returns what it wrote.
type shortAfterWriterBytes struct {
	shortAfterWriter
}

func (s *shortAfterWriterBytes) Bytes() []byte { return s.buf.Bytes() }

func TestMergeBlockOverReaderBudget(t *testing.T) {
	t.Parallel()

	// zstd stores seg0's incompressible block a few bytes longer than its decoded body,
	// and seg1's text block much shorter.
	random := mergePack(t, seg0, nil, []footerTestStep{{rec: randomDataRecord(10, blockTestHour+10, 4096), flush: true}})
	text := mergePack(t, seg1, nil, []footerTestStep{{rec: textDataRecord(10, blockTestHour+10, 4096), flush: true}})
	rb, tb := mustOpen(t, random, ReaderOptions{}).Blocks()[0], mustOpen(t, text, ReaderOptions{}).Blocks()[0]
	require.Greater(t, rb.OnDiskLen-format.EnvelopeLen, rb.UncompressedLen)
	require.Less(t, tb.OnDiskLen-format.EnvelopeLen, tb.UncompressedLen)

	tests := []struct {
		name     string
		file     []byte
		id       UUID
		maxBlock int64
		msg      string
	}{
		{name: "on disk", file: random, id: seg0, maxBlock: int64(rb.UncompressedLen), msg: "body_len"},
		{name: "decoded", file: text, id: seg1, maxBlock: int64(tb.OnDiskLen - format.EnvelopeLen), msg: "uncompressed_len"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			v := mergeView(t, nil, tt.file)
			opts := mergeOpts()
			opts.Reader.MaxBlockLen = tt.maxBlock
			out, rep, err := mergeBytes(t.Context(), v, inputsOf(t, v, tt.file), opts)
			// A budget is not the input's defect: a caller raises it instead of repairing the pack.
			require.ErrorIs(t, err, ErrReadLimit)
			assert.NotErrorIs(t, err, ErrMergeInput)
			require.ErrorContains(t, err, "input 0, pack "+tt.id.String())
			require.ErrorContains(t, err, "block 0 at offset")
			require.ErrorContains(t, err, tt.msg+" ")
			assert.Equal(t, MergeReport{}, rep)
			requireNoTrailer(t, out, 0)

			// A budget that holds the block both on disk and decoded.
			info := mustOpen(t, tt.file, ReaderOptions{}).Blocks()[0]
			opts.Reader.MaxBlockLen = int64(max(info.OnDiskLen-format.EnvelopeLen, info.UncompressedLen))
			_, rep = mustMerge(t, v, inputsOf(t, v, tt.file), opts)
			requireCopied(t, rep, 1, 1, 0)
		})
	}
}

// TestMergeFooterBudgetBoundsCopiedF3Lists merges an input whose small footer holds 32 nested F-3 lists,
// of about 1 MiB each, with an output footer budget of 4 MiB:
// the budget rejects the second block, the merge fails without a trailer,
// and it allocates far less than the 32 MiB that copying every list would take.
// That no unit copies its list, nor a coalescing group, is TestMergeCoalescingGroupOwnsItsUnits's check.
// It is not parallel: it measures the process-wide TotalAlloc.
func TestMergeFooterBudgetBoundsCopiedF3Lists(t *testing.T) {
	const (
		blocks = 32
		pad    = 1 << 20
	)

	file, lists := nestedF3Pack(t, mergePack(t, seg0, nil, hourSteps(1, blocks, 1)), pad)
	require.Len(t, lists, blocks)
	requireConsistent(t, file)
	require.Less(t, layoutOf(t, file).tr.FooterUncompressedLen, uint64(pad+pad/2), "the input's footer is small")
	v := mergeView(t, nil, file)
	opts := mergeOpts()
	opts.NoCoalesce = true
	opts.MaxFooterLen = 4 << 20

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	out, rep, err := mergeBytes(t.Context(), v, inputsOf(t, v, file), opts)
	runtime.ReadMemStats(&after)

	require.ErrorIs(t, err, ErrMergeLimit)
	assert.NotErrorIs(t, err, ErrMergeInput)
	require.ErrorContains(t, err, "block of seq 2-2")
	assert.Equal(t, MergeReport{}, rep)
	requireNoTrailer(t, out, 1)
	delta := after.TotalAlloc - before.TotalAlloc
	t.Logf("a merge of %d lists of %d bytes allocated %d bytes", blocks, len(lists[0]), delta)
	assert.Less(t, delta, uint64(12<<20), "no list is copied before the budget admits it")
}

// TestMergeFooterBudgetBelowAnEmptyFooter merges inputs without records
// under a budget one byte below the archive's footer, which F-1 and F-5 alone make up:
// the merge fails when it closes, writing neither footer nor trailer.
func TestMergeFooterBudgetBelowAnEmptyFooter(t *testing.T) {
	t.Parallel()

	empty := mergePack(t, seg0, func(m *PackMeta) { m.SeqStart = 5 }, nil)
	v := mergeView(t, nil, empty)
	full, rep := mustMerge(t, v, inputsOf(t, v, empty), mergeOpts())
	requireCopied(t, rep, 0, 0, 0)
	l := layoutOf(t, full)
	require.Equal(t, l.tr.FooterLen, l.tr.FooterUncompressedLen, "the none codec")

	opts := mergeOpts()
	opts.MaxFooterLen = int64(l.tr.FooterLen)
	fits, _ := mustMerge(t, v, inputsOf(t, v, empty), opts)
	// The file headers differ in writer_start_utc_ns only.
	assert.Equal(t, full[format.FileHeaderLen:], fits[format.FileHeaderLen:], "a footer that fills the budget")

	opts.MaxFooterLen--
	out, rep, err := mergeBytes(t.Context(), v, inputsOf(t, v, empty), opts)
	require.ErrorIs(t, err, ErrMergeLimit)
	assert.Equal(t, MergeReport{}, rep)
	assert.Equal(t, full[format.FileHeaderLen:l.blocksStart], out[format.FileHeaderLen:], "only the header is written")
}

// TestMergeCarriesWriterDefects merges an input with readable writer defects, which a merge does not refuse,
// copying its blocks, resolving them with another input's blocks of the same records under another codec,
// and coalescing the blocks copied or resolved into one:
// the archive holds the records byte for byte, the copied blocks too, and verifies with the same defects.
func TestMergeCarriesWriterDefects(t *testing.T) {
	t.Parallel()

	a := editBlock(t, mergePack(t, seg0, nil, defectTestSteps(t)), 0, func(b *testBlock) {
		// Record 0 marks System Bytes its 10-byte payload lacks; record 2, a state transition, carries no TLV body.
		b.payloads[0], b.headers[0].PayloadLen = b.payloads[0][:10], 10
		b.payloads[2], b.headers[2].PayloadLen = []byte{0xFF}, 1
	})
	in := mustVerify(t, a)
	require.Equal(t, OutcomeFinalizedConsistent, in.Outcome)
	require.Len(t, in.WriterDefects, 2)
	b := mergePack(t, seg1, nil, hourSteps(20, 23, 2))
	// again holds a's records, the defective ones included, in blocks of the none codec: each overlaps a block of a.
	again := withIDs(t, rebuildPack(t, a, func(_ int, b *testBlock) { b.codec = uint8(CodecNone) }), seg2, viewCapture)
	require.Equal(t, rawRecordsOf(t, a), rawRecordsOf(t, again))

	tests := []struct {
		name       string
		files      [][]byte
		noCoalesce bool
		want       mergeCounts
	}{
		{name: "copied", files: [][]byte{a, b}, noCoalesce: true, want: mergeCounts{blocks: 5, records: 10, copied: 5}},
		{name: "resolved", files: [][]byte{a, b, again}, noCoalesce: true, want: mergeCounts{blocks: 5, records: 10, copied: 2, resolved: 6}},
		{name: "coalesced", files: [][]byte{a, b}, want: mergeCounts{blocks: 1, records: 10, coalescedEncodings: 1, coalesced: 5}},
		{name: "resolved and coalesced", files: [][]byte{a, b, again},
			want: mergeCounts{blocks: 1, records: 10, coalescedEncodings: 1, coalesced: 2, resolved: 6, twice: 3}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			v := mergeView(t, nil, tt.files...)
			opts := mergeOpts()
			opts.NoCoalesce = tt.noCoalesce
			out, rep := mustMerge(t, v, inputsOf(t, v, tt.files...), opts)
			requireCounts(t, rep, tt.want)
			assert.Equal(t, slices.Concat(rawRecordsOf(t, a), rawRecordsOf(t, b)), rawRecordsOf(t, out))
			if tt.want.copied == tt.want.blocks {
				assert.Equal(t, slices.Concat(blockBytes(t, a, allBlocks), blockBytes(t, b, allBlocks)), blockBytes(t, out, allBlocks))
			}
			got := mustVerify(t, out)
			require.Len(t, got.WriterDefects, len(in.WriterDefects))
			for i, d := range got.WriterDefects {
				assert.Equal(t, [2]any{in.WriterDefects[i].Kind, in.WriterDefects[i].Seq}, [2]any{d.Kind, d.Seq}, "defect %d", i)
			}
		})
	}
}

// TestMergeWithoutRecords merges a view of a patch holding only coverage and segments without records:
// the archive has no block, carries the coverage, and takes the largest seq_start of its inputs.
func TestMergeWithoutRecords(t *testing.T) {
	t.Parallel()

	empty := func(id UUID, seq uint64, edit func(m *PackMeta)) []byte {
		return mergePack(t, id, func(m *PackMeta) {
			m.SeqStart = seq
			if edit != nil {
				edit(m)
			}
		}, nil)
	}
	cov := Coverage{CaptureID: new(viewCapture), SeqFirst: new(uint64(7)), TimeStart: new(blockTestHour), TimeEnd: new(blockTestHour)}
	files := [][]byte{
		empty(seg0, 5, nil),
		empty(seg1, 9, nil),
		empty(patP1, 7, func(m *PackMeta) {
			m.PackRole, m.Supersedes, m.Coverage = PackRoleRepair, []UUID{{0xD0}}, []Coverage{cov}
		}),
	}
	v := mergeView(t, commitSet(patP1), files...)
	require.Len(t, v.Packs, 3)

	out, rep := mustMerge(t, v, inputsOf(t, v, files...), mergeOpts())
	requireCopied(t, rep, 0, 0, 0)
	h := mustOpen(t, out, ReaderOptions{}).Header()
	assert.Zero(t, h.Trailer.BlockCount)
	assert.Equal(t, uint64(9), h.Meta.SeqStart)
	assert.Equal(t, []Coverage{cov}, h.Meta.Coverage)
}

// TestMergeConverterArchiveTwice merges a converter's archive, the scope's only pack, and then that merge's archive:
// neither archive names generation-0 packs, so neither has compacted_from.
// The first merge coalesces the converter's two blocks, and the second copies that block verbatim.
func TestMergeConverterArchiveTwice(t *testing.T) {
	t.Parallel()

	converter := mergePack(t, memA, func(m *PackMeta) {
		generationMeta(m, setA, 1, 1, nil)
		m.CompactionLevel = 0
	}, hourSteps(10, 13, 2))
	v := mergeView(t, commitSet(setA), converter)
	opts := mergeOpts()
	opts.ScopeGeneration, opts.PackID, opts.ReplacementSetID = 2, memB, setB
	first, rep := mustMerge(t, v, inputsOf(t, v, converter), opts)
	requireCounts(t, rep, mergeCounts{blocks: 1, records: 4, coalescedEncodings: 1, coalesced: 2})

	v = mergeView(t, commitSet(setA, setB), converter, first)
	require.Equal(t, []UUID{memB}, v.Packs)
	opts.ScopeGeneration, opts.PackID, opts.ReplacementSetID = 3, memC, setC
	second, rep := mustMerge(t, v, inputsOf(t, v, first), opts)
	requireCopied(t, rep, 1, 4, 0)
	assert.Equal(t, blockBytes(t, first, allBlocks), blockBytes(t, second, allBlocks))
	assert.Equal(t, f3ListsOf(t, first), f3ListsOf(t, second))

	for i, archive := range [][]byte{first, second} {
		m := mustOpen(t, archive, ReaderOptions{}).Header().Meta
		assert.Nil(t, m.CompactedFrom, "archive %d", i)
		assert.Equal(t, uint8(i+1), m.CompactionLevel, "archive %d", i)
		assert.Equal(t, rawRecordsOf(t, converter), rawRecordsOf(t, archive), "archive %d", i)
	}
	assert.Equal(t, []UUID{memA}, mustOpen(t, first, ReaderOptions{}).Header().Meta.Supersedes)
	assert.Equal(t, []UUID{memB}, mustOpen(t, second, ReaderOptions{}).Header().Meta.Supersedes)
}

// TestMergeAfterRepair repairs a damaged segment and then merges its scope:
// the patch replaces the segment, and the archive holds the patch's records and its coverage of the records lost.
func TestMergeAfterRepair(t *testing.T) {
	t.Parallel()

	baseline := mergePack(t, seg0, nil, hourSteps(10, 15, 2))
	damaged := flipByte(baseline, mustOpen(t, baseline, ReaderOptions{}).Blocks()[1].Offset+format.EnvelopeLen+1)
	ropts := repairOpts()
	ropts.PackID = patP1
	patch, _, err := repairBytes(t, damaged, ropts)
	require.NoError(t, err)
	late := mergePack(t, seg1, nil, hourSteps(20, 23, 2))

	files := [][]byte{damaged, late, patch}
	v := mergeView(t, commitSet(patP1), files...)
	require.Equal(t, []UUID{seg1, patP1}, v.Packs, "the patch replaces the damaged segment")
	require.Equal(t, []UUID{seg0, seg1, patP1}, v.CompactedFrom)
	out, rep := mustMerge(t, v, inputsOf(t, v, files...), mergeOpts())
	requireCounts(t, rep, mergeCounts{blocks: 1, records: 8, coalescedEncodings: 1, coalesced: 4})

	records := recordsOf(t, out)
	seqs := make([]uint64, 0, len(records))
	for _, it := range records {
		seqs = append(seqs, it.rec.Seq)
	}
	assert.Equal(t, []uint64{10, 11, 14, 15, 20, 21, 22, 23}, seqs)
	m := mustOpen(t, out, ReaderOptions{}).Header().Meta
	assert.Equal(t, mustOpen(t, patch, ReaderOptions{}).Header().Meta.Coverage, m.Coverage, "the patch's coverage of the lost records")
	assert.Equal(t, v.CompactedFrom, m.CompactedFrom)
}

// TestMergeLineageUnderPermutations merges the scope of patchChainScope, with records:
// generation A's member memA replaced by patQ1 and then patQ2, segment seg1 by patP1 and then patP2,
// the scope's packs listed to ActiveView in every order of the patches, and of memA and seg1.
// Every order writes the same records, compaction_level and supersedes,
// compacted_from as the view lists it, which always holds A's list first and the same packs,
// and the union of the inputs' coverage.
func TestMergeLineageUnderPermutations(t *testing.T) {
	t.Parallel()

	covered := func(seq uint64) Coverage {
		return Coverage{CaptureID: new(viewCapture), SeqFirst: new(seq), SeqLast: new(seq), TimeStart: new(blockTestHour), TimeEnd: new(blockTestHour)}
	}
	patch := func(id UUID, of []byte, supersedes UUID, cov ...Coverage) []byte {
		// A patch copies the pack metadata of the pack it repairs, compaction_level and compacted_from included.
		m := mustOpen(t, of, ReaderOptions{}).Header().Meta
		first, last := uint64(10), uint64(13)
		if m.PackRole == PackRoleSegment || slices.Equal(m.Supersedes, []UUID{seg1}) {
			first, last = 20, 23
		}

		return mergePack(t, id, func(p *PackMeta) {
			*p = *m
			p.PackRole, p.ScopeGeneration, p.PublisherEpoch = PackRoleRepair, new(uint64(0)), nil
			p.ReplacementSetID, p.ReplacementSetSize, p.ReplacementSetIndex = nil, nil, nil
			p.PatchBase, p.Supersedes = new(setA), []UUID{supersedes}
			p.Coverage = append(slices.Clone(m.Coverage), cov...)
		}, hourSteps(first, last, 2))
	}
	mem := mergePack(t, memA, func(m *PackMeta) { generationMeta(m, setA, 1, 1, []UUID{seg0}) }, hourSteps(10, 13, 2))
	seg := mergePack(t, seg1, nil, hourSteps(20, 23, 2))
	p1 := patch(patP1, seg, seg1, covered(100))
	p2 := patch(patP2, p1, patP1, covered(101))
	q1 := patch(patQ1, mem, memA, covered(200))
	q2 := patch(patQ2, q1, patQ1, covered(201))
	require.Equal(t, uint8(1), mustOpen(t, q2, ReaderOptions{}).Header().Meta.CompactionLevel, "patQ2 keeps memA's level")
	commits := commitSet(setA, patP1, patP2, patQ1, patQ2)
	lineage := []UUID{seg0, seg1, patP1, patP2, patQ1, patQ2}

	var records []iterItem
	patches := [][]byte{p1, p2, q1, q2}
	for _, order := range permutations([]UUID{{0}, {1}, {2}, {3}}) {
		for _, swap := range []bool{false, true} {
			files := []([]byte){mem, seg}
			if swap {
				files = []([]byte){seg, mem}
			}
			for _, k := range order {
				files = append(files, patches[k[0]])
			}
			v := mergeView(t, commits, files...)
			require.ElementsMatch(t, []UUID{patP2, patQ2}, v.Packs)

			opts := mergeOpts()
			opts.PublisherEpoch, opts.ScopeGeneration = 1, 2
			out, rep := mustMerge(t, v, inputsOf(t, v, files...), opts)
			requireCounts(t, rep, mergeCounts{blocks: 1, records: 8, coalescedEncodings: 1, coalesced: 4})
			if records == nil {
				records = recordsOf(t, out)
			}
			assert.Equal(t, records, recordsOf(t, out))

			m := mustOpen(t, out, ReaderOptions{}).Header().Meta
			assert.Equal(t, uint8(2), m.CompactionLevel, "one above patQ2's level")
			assert.Equal(t, []UUID{memA}, m.Supersedes)
			assert.Equal(t, v.CompactedFrom, m.CompactedFrom)
			assert.Equal(t, seg0, m.CompactedFrom[0], "generation A's list first")
			assert.ElementsMatch(t, lineage, m.CompactedFrom)
			assert.ElementsMatch(t, []Coverage{covered(100), covered(101), covered(200), covered(201)}, m.Coverage)
		}
	}
}
