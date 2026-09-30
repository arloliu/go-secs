package tracepack

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"runtime"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/codec"
	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// testRecordLen is the decoded size of a record of testDataRecord: its 44-byte header and blockTestFrame.
var testRecordLen = format.RecordHeaderLen + len(blockTestFrame)

// editedBody returns a hook for the on-disk body of new blocks of codec c whose decoded body is n bytes long,
// as mergePlan.coalescedHook takes it: it counts its calls in calls,
// decodes the body, changes it with edit, and encodes it again, so the envelope built after the hook matches the body.
func editedBody(t testing.TB, c Codec, n int, calls *int, edit func(body []byte)) func(enc []byte) []byte {
	return func(enc []byte) []byte {
		*calls++
		body, err := codec.Decode(uint8(c), nil, enc, n)
		require.NoError(t, err)
		edit(body)
		out, err := codec.Encode(uint8(c), nil, body)
		require.NoError(t, err)

		return out
	}
}

// countedHook returns a hook for the on-disk body of new blocks that counts its calls in calls and changes nothing.
func countedHook(calls *int) func(enc []byte) []byte {
	return func(enc []byte) []byte {
		*calls++

		return enc
	}
}

// TestMergeCoalescesSmallBlocks merges blocks that no other block overlaps, which coalescing groups greedily in seq order
// (the tracepack storage specification §4 Merge, and its §8 coalescing vectors):
// a group grows while the next block has the group's record_header_len and fits the threshold with the group,
// a block that alone reaches the threshold is a group of its own,
// a group of one block is copied verbatim with its F-3 list and its codec,
// and a larger group is encoded as one new block, with the codec encodeBlock chooses for MergeOptions.Codec,
// the inputs being written with the other codec.
// Only the larger groups are encoded, and without coalescing no block is.
func TestMergeCoalescesSmallBlocks(t *testing.T) {
	t.Parallel()

	h, wide := format.RecordHeaderLen, format.RecordHeaderLen+wideExtraLen
	oversized := slices.Concat(hourSteps(10, 11, 2), []footerTestStep{{rec: bigDataRecord(12, blockTestHour+12, 200), flush: true}},
		hourSteps(13, 16, 2))
	tests := []struct {
		name  string
		steps []footerTestStep
		// wide selects the input blocks whose records get 52-byte headers.
		wide      func(i int) bool
		threshold int
		// shapes holds the record count and record_header_len of each block written,
		// and verbatim maps each block written as it is to the input block it copies.
		shapes   [][2]int
		verbatim map[int]int
		want     mergeCounts
	}{
		{
			// Each block holds two records: the first two fit a threshold of five records together, the third does not.
			name: "three small blocks of which two fit", steps: hourSteps(10, 15, 2), threshold: 5 * testRecordLen,
			shapes: [][2]int{{4, h}, {2, h}}, verbatim: map[int]int{1: 2},
			want: mergeCounts{blocks: 2, records: 6, copied: 1, coalescedEncodings: 1, coalesced: 2},
		},
		{
			// The threshold holds four records: seq 12's block exceeds it alone,
			// and the blocks before and after it do not fit with it.
			name: "a block of one oversized record alone", steps: oversized, threshold: 4 * testRecordLen,
			shapes: [][2]int{{2, h}, {1, h}, {4, h}}, verbatim: map[int]int{0: 0, 1: 1},
			want: mergeCounts{blocks: 3, records: 7, copied: 2, coalescedEncodings: 1, coalesced: 2},
		},
		{
			name: "unequal record_header_len left apart", steps: hourSteps(10, 17, 2), wide: func(i int) bool { return i == 1 || i == 2 },
			shapes: [][2]int{{2, h}, {4, wide}, {2, h}}, verbatim: map[int]int{0: 0, 2: 3},
			want: mergeCounts{blocks: 3, records: 8, copied: 2, coalescedEncodings: 1, coalesced: 2},
		},
	}
	for _, c := range []Codec{CodecNone, CodecZstd} {
		for _, tt := range tests {
			t.Run(c.String()+"/"+tt.name, func(t *testing.T) {
				t.Parallel()

				input := CodecZstd
				if c == CodecZstd {
					input = CodecNone
				}
				a := mergePackCodec(t, input, seg0, tt.steps)
				if tt.wide != nil {
					a = widenFile(t, a, tt.wide)
				}
				v := mergeView(t, nil, a)
				opts := mergeOpts()
				opts.Codec, opts.BlockThreshold = c, tt.threshold
				out, rep := mustMerge(t, v, inputsOf(t, v, a), opts)
				requireCounts(t, rep, tt.want)
				assert.Equal(t, tt.shapes, blockShapes(t, out))
				in, inLists := blockBytes(t, a, allBlocks), f3ListsOf(t, a)
				got, gotLists := blockBytes(t, out, allBlocks), f3ListsOf(t, out)
				for k, i := range tt.verbatim {
					assert.Equal(t, in[i], got[k], "block %d copies input block %d", k, i)
					assert.Equal(t, inLists[i], gotLists[k], "block %d keeps the F-3 list of input block %d", k, i)
				}
				for k, b := range got {
					env, err := format.UnmarshalBlockEnvelope(b)
					require.NoError(t, err)
					want := input
					if _, ok := tt.verbatim[k]; !ok {
						body, err := codec.Decode(env.Codec, nil, b[format.EnvelopeLen:], int(env.UncompressedLen))
						require.NoError(t, err)
						_, want, err = encodeBlock(c, nil, body)
						require.NoError(t, err)
					}
					assert.Equal(t, want, Codec(env.Codec), "block %d's codec", k)
				}

				// Only the groups of more than one block are encoded, and without coalescing none is.
				var encoded int
				_, rep, err := writeWithHooks(t, v, inputsOf(t, v, a), opts, func(p *mergePlan) { p.coalescedHook = countedHook(&encoded) })
				require.NoError(t, err)
				assert.Equal(t, tt.want.coalescedEncodings, encoded)
				assert.Equal(t, tt.want.coalescedEncodings, rep.CoalescedEncodings)

				encoded = 0
				opts.NoCoalesce = true
				_, rep, err = writeWithHooks(t, v, inputsOf(t, v, a), opts, func(p *mergePlan) {
					p.coalescedHook, p.encodedHook = countedHook(&encoded), countedHook(&encoded)
				})
				require.NoError(t, err)
				assert.Zero(t, encoded)
				requireCopied(t, rep, len(in), tt.want.records, 0)
			})
		}
	}
}

// TestMergeCoalescedEncodingFails injects a defect into the new block of a group,
// after the block is encoded and before it is checked:
// an I-2 failure, the second record given the first record's seq,
// and a payload byte that no longer matches its source, which only the comparison of every record with its source finds.
// The new block is discarded before any of it is written, and the group's blocks are written as they are
// (the tracepack storage specification §4 Merge, and its §8 vector):
// the archive is the one written without coalescing, every copied block with its F-3 list byte for byte,
// and the report counts the fallback.
// The groups are three blocks of one input, each read into the buffers the block before it was read into,
// and a copied block with a block the resolution encoded, written again as its own encoding.
func TestMergeCoalescedEncodingFails(t *testing.T) {
	t.Parallel()

	edits := []struct {
		name string
		edit func(body []byte)
	}{
		// Column 0 of the header section holds each record's lowest seq byte, and the seqs differ in it only.
		{name: "an I-2 failure", edit: func(body []byte) { body[1] = body[0] }},
		{name: "a payload byte changed", edit: func(body []byte) { body[len(body)-1] ^= 1 }},
	}
	for _, c := range []Codec{CodecNone, CodecZstd} {
		early := mergePackCodec(t, c, seg4, hourSteps(1, 2, 2))
		copied := mergePackCodec(t, c, seg0, hourSteps(10, 15, 2))
		groups := []struct {
			name  string
			files [][]byte
			want  mergeCounts
		}{
			{name: "copied blocks", files: [][]byte{copied}, want: mergeCounts{blocks: 3, records: 6, copied: 3, fallbacks: 1}},
			{
				name:  "a copied and a resolved block",
				files: [][]byte{early, mergePackCodec(t, c, seg0, hourSteps(10, 13, 4)), mergePackCodec(t, c, seg1, hourSteps(12, 15, 4))},
				want:  mergeCounts{blocks: 2, records: 8, copied: 1, resolved: 2, fallbacks: 1},
			},
		}
		for _, g := range groups {
			for _, e := range edits {
				t.Run(c.String()+"/"+g.name+"/"+e.name, func(t *testing.T) {
					t.Parallel()

					v := mergeView(t, nil, g.files...)
					opts := mergeOpts()
					opts.Codec = c
					var calls int
					n := int(g.want.records) * testRecordLen
					out, rep, err := writeWithHooks(t, v, inputsOf(t, v, g.files...), opts, func(p *mergePlan) {
						p.coalescedHook = editedBody(t, c, n, &calls, e.edit)
					})
					require.NoError(t, err)
					assert.Equal(t, 1, calls, "one group encoded")
					requireConsistent(t, out)
					requireCounts(t, rep, g.want)

					opts.NoCoalesce = true
					plain, _ := mustMerge(t, v, inputsOf(t, v, g.files...), opts)
					// The file headers differ in writer_start_utc_ns only.
					assert.Equal(t, plain[format.FileHeaderLen:], out[format.FileHeaderLen:], "the archive written without coalescing")
					if g.want.resolved == 0 {
						assert.Equal(t, blockBytes(t, copied, allBlocks), blockBytes(t, out, allBlocks))
						assert.Equal(t, f3ListsOf(t, copied), f3ListsOf(t, out))
					}
				})
			}
		}
	}
}

// TestMergeCoalescesResolvedBlocks coalesces the blocks the record-by-record resolution encodes:
// the block of a resolved cluster alone in its group is written as its own encoding, not encoded again,
// while the blocks of a group of more than one block, the resolution's included, are encoded again as one block,
// so each record of a block of the resolution is encoded twice.
// Two blocks of one resolution never fit one group, so the group of two resolved blocks spans two clusters.
// A group written because a block of the resolution cannot join it is encoded apart from that block,
// which is then written as the resolution encoded it.
func TestMergeCoalescesResolvedBlocks(t *testing.T) {
	t.Parallel()

	early := mergePack(t, seg4, nil, hourSteps(1, 4, 4))
	low := mergePack(t, seg0, nil, hourSteps(10, 13, 4))
	high := mergePack(t, seg1, nil, hourSteps(12, 15, 4))
	tests := []struct {
		name  string
		files [][]byte
		want  mergeCounts
		// shapes holds the record count and record_header_len of each block written, when not nil.
		shapes [][2]int
	}{
		{name: "a resolved cluster yielding one block", files: [][]byte{low, high}, want: mergeCounts{blocks: 1, records: 6, resolved: 2}},
		{
			name: "a group written as a block of the resolution arrives",
			// The same records with 52-byte headers under two codecs make a cluster to resolve.
			files: [][]byte{
				mergePack(t, seg4, nil, hourSteps(1, 4, 2)),
				widenFile(t, mergePackCodec(t, CodecZstd, seg0, hourSteps(10, 15, 6)), allBlocks),
				widenFile(t, mergePackCodec(t, CodecNone, seg1, hourSteps(10, 15, 6)), allBlocks),
			},
			want:   mergeCounts{blocks: 2, records: 10, coalescedEncodings: 1, coalesced: 2, resolved: 2},
			shapes: [][2]int{{4, format.RecordHeaderLen}, {6, format.RecordHeaderLen + wideExtraLen}},
		},
		{
			name: "a copied block and a resolved block", files: [][]byte{early, low, high},
			want: mergeCounts{blocks: 1, records: 10, coalescedEncodings: 1, coalesced: 1, resolved: 2, twice: 1},
		},
		{
			name: "two resolved blocks",
			files: [][]byte{
				mergePack(t, seg0, nil, slices.Concat(hourSteps(10, 13, 4), hourSteps(20, 23, 4))),
				mergePack(t, seg1, nil, slices.Concat(hourSteps(12, 15, 4), hourSteps(22, 25, 4))),
			},
			want: mergeCounts{blocks: 1, records: 12, coalescedEncodings: 1, resolved: 4, twice: 2},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			v := mergeView(t, nil, tt.files...)
			out, rep := mustMerge(t, v, inputsOf(t, v, tt.files...), mergeOpts())
			requireCounts(t, rep, tt.want)
			if tt.shapes != nil {
				assert.Equal(t, tt.shapes, blockShapes(t, out))
			}

			var resolved, coalesced int
			_, rep, err := writeWithHooks(t, v, inputsOf(t, v, tt.files...), mergeOpts(), func(p *mergePlan) {
				p.encodedHook, p.coalescedHook = countedHook(&resolved), countedHook(&coalesced)
			})
			require.NoError(t, err)
			assert.Equal(t, rep.ResolvedEncodings, resolved, "encodings of the resolution")
			assert.Equal(t, rep.CoalescedEncodings, coalesced, "encodings of coalescing")
			if tt.want.coalescedEncodings == 0 {
				opts := mergeOpts()
				opts.NoCoalesce = true
				plain, _ := mustMerge(t, v, inputsOf(t, v, tt.files...), opts)
				// The file headers differ in writer_start_utc_ns only.
				assert.Equal(t, plain[format.FileHeaderLen:], out[format.FileHeaderLen:], "the resolution's block as it encoded it")
			}
		})
	}
}

// TestMergeCoalescingAfterAConflict merges a block, then a cluster with a conflict,
// then three blocks whose record_header_len alternates, each of which would make the group before it be written:
// the group pending at the conflict is dropped, and no later group is written, so the archive holds no block.
func TestMergeCoalescingAfterAConflict(t *testing.T) {
	t.Parallel()

	early := mergePack(t, seg4, nil, hourSteps(1, 4, 4))
	steps := hourSteps(10, 13, 4)
	a, b := mergePack(t, seg0, nil, steps), mergePack(t, seg1, nil, changed(steps, 1, 12))
	late := widenFile(t, mergePack(t, seg2, nil, hourSteps(20, 25, 2)), func(i int) bool { return i == 1 })
	v := mergeView(t, nil, early, a, b, late)

	var log conflictLog
	opts := mergeOpts()
	opts.OnConflict = log.add
	out, rep, err := mergeBytes(t.Context(), v, inputsOf(t, v, early, a, b, late), opts)
	require.ErrorIs(t, err, ErrMergeConflict)
	assert.Equal(t, MergeReport{Conflicts: 1, ConflictsComplete: true}, rep)
	require.Len(t, log.got, 1)
	requireNoTrailer(t, out, 0)
	assert.Len(t, out, format.FileHeaderLen+int(layoutHeader(t, out).PackMetadataLen), "the header alone")
}

// TestMergeCoalescingGroupOwnsItsUnits emits the unit of an input block into the pending coalescing group,
// then reads the next block into the same buffers:
// the group keeps the block's bytes and records,
// for a block decoded in place (none) and one decoded into its own buffer (zstd),
// while the unit and the group keep its F-3 list as the input's retained list, uncopied, and nothing is written.
func TestMergeCoalescingGroupOwnsItsUnits(t *testing.T) {
	t.Parallel()

	for _, c := range []Codec{CodecNone, CodecZstd} {
		t.Run(c.String(), func(t *testing.T) {
			t.Parallel()

			a := widenFile(t, mergePackCodec(t, c, seg0, hourSteps(10, 15, 3)), func(i int) bool { return i == 0 })
			v := mergeView(t, nil, a)
			opts := fixedIDs(mergeOpts())
			p := mustPlanMerge(t, v, inputsOf(t, v, a), opts)
			require.Len(t, p.clusters, 2)
			var out bytes.Buffer
			m, err := p.startRun(&out, &opts)
			require.NoError(t, err)

			var buf blockBuf
			u, err := p.readUnit(t.Context(), &p.clusters[0].blocks[0], &buf)
			require.NoError(t, err)
			f3 := p.inputs[0].rawF3(0)
			assert.Same(t, &f3[0], &u.f3[0], "the unit's list aliases the input's retained F-3 section")
			require.NoError(t, m.emit(&u))
			require.Len(t, m.group.units, 1, "the unit waits in the group")
			_, _, err = p.readInputBlock(t.Context(), &p.clusters[1].blocks[0], &buf)
			require.NoError(t, err)

			assert.Equal(t, blockBytes(t, a, allBlocks)[0], m.group.raw)
			rows, payloads := m.group.records.records()
			want := recordsOf(t, a)[:3]
			require.Len(t, rows, len(want))
			for i, it := range want {
				assert.Len(t, rows[i], format.RecordHeaderLen+wideExtraLen, "record %d", i)
				assert.Equal(t, it.extra, rows[i][format.RecordHeaderLen:], "record %d", i)
				assert.Equal(t, it.rec.Seq, binary.LittleEndian.Uint64(rows[i]), "record %d", i)
				assert.Equal(t, it.rec.Payload, payloads[i], "record %d", i)
			}
			gu := m.group.units[0]
			assert.False(t, gu.encoded)
			assert.Same(t, &f3[0], &gu.f3[0], "the group's list aliases the input's retained F-3 section")
			assert.Empty(t, m.w.blocks, "nothing is appended while the group waits")
		})
	}
}

// TestMergeFooterBudgetWithCoalescing merges the input of TestMergeFooterBudgetBoundsCopiedF3Lists,
// 32 blocks of one record whose F-3 lists nest, about 1 MiB each, with coalescing, which makes the blocks one group,
// under an output footer budget of 4 MiB.
// While the group waits, no block is appended and no F-3 list copied;
// the coalesced block takes a list built from its summary, so the merge succeeds.
// When the group's new block fails its check, its blocks are written as they are with their lists,
// which the budget rejects at the second block, without a trailer and before much is allocated;
// with lists short enough for the budget, every list is written byte for byte.
// It is not parallel: it measures the process-wide TotalAlloc.
func TestMergeFooterBudgetWithCoalescing(t *testing.T) {
	const (
		blocks = 32
		pad    = 1 << 20
	)

	file, lists := nestedF3Pack(t, mergePack(t, seg0, nil, hourSteps(1, blocks, 1)), pad)
	require.Len(t, lists, blocks)
	v := mergeView(t, nil, file)
	opts := fixedIDs(mergeOpts())
	opts.MaxFooterLen = 4 << 20
	// fail changes the last payload byte of the group's new block, of the none codec, so the block fails its check.
	fail := func(p *mergePlan) {
		p.coalescedHook = func(enc []byte) []byte {
			enc[len(enc)-1] ^= 1

			return enc
		}
	}

	t.Run("the pending group", func(t *testing.T) {
		p := mustPlanMerge(t, v, inputsOf(t, v, file), opts)
		require.Len(t, p.clusters, blocks)
		var out bytes.Buffer
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		m, err := p.startRun(&out, &opts)
		require.NoError(t, err)
		for k := range p.clusters {
			require.NoError(t, m.writeCluster(t.Context(), &p.clusters[k]))
		}
		runtime.ReadMemStats(&after)

		require.Len(t, m.group.units, blocks, "one group")
		assert.Empty(t, m.w.blocks, "no block appended")
		assert.Zero(t, m.w.footerCounted, "nothing counted against the budget")
		for j := range m.group.units {
			f3 := p.inputs[0].rawF3(j)
			assert.Same(t, &f3[0], &m.group.units[j].f3[0], "block %d: the list stays in the input's retained F-3 section", j)
		}
		delta := after.TotalAlloc - before.TotalAlloc
		t.Logf("a group of %d blocks with lists of %d bytes allocated %d bytes", blocks, len(lists[0]), delta)
		assert.Less(t, delta, uint64(pad), "no list is copied while the group waits")

		require.NoError(t, m.flushGroup())
		assert.Equal(t, 1, m.rep.CoalescedEncodings)
		require.Len(t, m.w.blocks, 1)
		assert.Nil(t, m.w.blocks[0].verbatimF3, "the coalesced block's list is built from its summary")
		_, err = m.w.Close()
		require.NoError(t, err)
		requireConsistent(t, out.Bytes())
	})

	t.Run("coalesced", func(t *testing.T) {
		_, rep := mustMerge(t, v, inputsOf(t, v, file), opts)
		requireCounts(t, rep, mergeCounts{blocks: 1, records: blocks, coalescedEncodings: 1, coalesced: blocks})
	})

	t.Run("a fallback over the budget", func(t *testing.T) {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		out, rep, err := writeWithHooks(t, v, inputsOf(t, v, file), opts, fail)
		runtime.ReadMemStats(&after)

		require.ErrorIs(t, err, ErrMergeLimit)
		assert.NotErrorIs(t, err, ErrMergeInput)
		require.ErrorContains(t, err, "block of seq 2-2")
		assert.Equal(t, MergeReport{}, rep)
		requireNoTrailer(t, out, 1)
		delta := after.TotalAlloc - before.TotalAlloc
		t.Logf("a fallback of %d lists of %d bytes allocated %d bytes", blocks, len(lists[0]), delta)
		assert.Less(t, delta, uint64(12<<20), "no list is copied before the budget admits it")
	})

	t.Run("a fallback within the budget", func(t *testing.T) {
		short, shortLists := nestedF3Pack(t, mergePack(t, seg0, nil, hourSteps(1, blocks, 1)), 4096)
		sv := mergeView(t, nil, short)
		out, rep, err := writeWithHooks(t, sv, inputsOf(t, sv, short), opts, fail)
		require.NoError(t, err)
		requireConsistent(t, out)
		requireCounts(t, rep, mergeCounts{blocks: blocks, records: blocks, copied: blocks, fallbacks: 1})
		assert.Equal(t, blockBytes(t, short, allBlocks), blockBytes(t, out, allBlocks))
		assert.Equal(t, shortLists, f3ListsOf(t, out), "every list byte for byte")
	})
}

// TestMergeFooterBudgetExceededByF5 coalesces two blocks, each of 100 records at every other seq,
// into one block of 200 seq ranges,
// whose F-3 list is built from its summary, so the footer budget counts the list once
// and each seq range at the 16 bytes the Writer keeps of it, below the 24 bytes of its F-5 entry.
// Under a budget one byte below the archive's footer, the block is admitted and F-2 and F-3 fit the budget,
// but F-5 takes the footer over it: the merge fails when it closes, writing neither footer nor trailer.
func TestMergeFooterBudgetExceededByF5(t *testing.T) {
	t.Parallel()

	var seqs [2][]uint64
	for seq := uint64(0); seq < 400; seq += 2 {
		seqs[seq/200] = append(seqs[seq/200], seq)
	}
	a := mergePack(t, seg0, nil, seqBlocks(seqs[0], seqs[1]))
	v := mergeView(t, nil, a)
	full, rep := mustMerge(t, v, inputsOf(t, v, a), mergeOpts())
	requireCounts(t, rep, mergeCounts{blocks: 1, records: 200, coalescedEncodings: 1, coalesced: 2})

	l := layoutOf(t, full)
	require.Equal(t, l.tr.FooterLen, l.tr.FooterUncompressedLen, "the none codec")
	footerLen := l.tr.FooterLen
	f5At := splitFooter(t, footerOf(t, full)).pro.F5Offset
	var counted uint64
	for _, s := range mustOpen(t, full, ReaderOptions{}).footer.blocks {
		require.Len(t, s.seqRanges, 200)
		counted += footerCost(&s, nil)
	}
	budget := footerLen - 1
	t.Logf("footer %d bytes, F-5 at %d, %d bytes counted", footerLen, f5At, counted)
	require.LessOrEqual(t, counted, budget, "the block is admitted")
	require.LessOrEqual(t, f5At, budget, "F-2 and F-3 fit the budget")

	opts := mergeOpts()
	opts.MaxFooterLen = int64(budget)
	out, rep, err := mergeBytes(t.Context(), v, inputsOf(t, v, a), opts)
	require.ErrorIs(t, err, ErrMergeLimit)
	require.ErrorContains(t, err, fmt.Sprintf("footer of %d bytes exceeds the budget of %d", footerLen, budget))
	assert.Equal(t, MergeReport{}, rep)
	// The file headers differ in writer_start_utc_ns only.
	assert.Equal(t, full[format.FileHeaderLen:l.tr.FooterOffset], out[format.FileHeaderLen:], "the header and the block, no footer or trailer")
	requireNoTrailer(t, out, 1)
}

// TestMergeCancelledWithAPendingGroup cancels a merge whose three blocks make one coalescing group
// while the second block is read, so the check before the third block's read stops the merge and the group is not written,
// or while the third block is read, so the group is written and the check before the footer stops the merge.
// Neither writes a trailer.
func TestMergeCancelledWithAPendingGroup(t *testing.T) {
	t.Parallel()

	a := mergePack(t, seg0, nil, hourSteps(10, 15, 2))
	blocks := mustOpen(t, a, ReaderOptions{}).Blocks()
	require.Len(t, blocks, 3)
	v := mergeView(t, nil, a)
	tests := []struct {
		name string
		// block is the block whose read cancels the merge, and written the number of blocks written.
		block, written int
	}{
		{name: "before the last block", block: 1, written: 0},
		{name: "after the last block", block: 2, written: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			b := blocks[tt.block]
			inputs := inputsOf(t, v, a)
			inputs[0].ReaderAt = &blockReadAt{file: a, off: int64(b.Offset), n: int(b.OnDiskLen), hook: func(int, []byte) error {
				cancel()
				return nil
			}}
			out, rep, err := mergeBytes(ctx, v, inputs, mergeOpts())
			require.ErrorIs(t, err, context.Canceled)
			assert.Equal(t, MergeReport{}, rep)
			requireNoTrailer(t, out, tt.written)
		})
	}
}
