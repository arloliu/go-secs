package tracepack

import (
	"bytes"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// retainedCopy returns a copy of every span of bytes r retains.
func retainedCopy(r *Reader) [][]byte {
	out := make([][]byte, len(r.held))
	for i, s := range r.held {
		out[i] = bytes.Clone(s.data)
	}

	return out
}

// retainedPacks returns packs that Open retains whole, with None and zstd blocks, one with a widened block.
func retainedPacks(t *testing.T) map[string][]byte {
	t.Helper()

	widen1 := func(p *readerPack) []byte {
		return rebuildPack(t, p.file, func(i int, b *testBlock) {
			if i == 1 {
				widen(b)
			}
		})
	}

	return map[string][]byte{
		"none":      writeReaderPack(t, readerPackConfig{codec: CodecNone}, hourRecords(3, 4)).file,
		"zstd":      writeReaderPack(t, readerPackConfig{codec: CodecZstd}, hourRecords(3, 4)).file,
		"none wide": widen1(writeReaderPack(t, readerPackConfig{codec: CodecNone}, hourRecords(2, 3))),
		"zstd wide": widen1(writeReaderPack(t, readerPackConfig{codec: CodecZstd}, hourRecords(2, 3))),
	}
}

// overwriteItemBytes flips every byte a callback can reach through it: its payload and its header extension.
func overwriteItemBytes(it *Item) {
	for k := range it.Record.Payload {
		it.Record.Payload[k] ^= 0xff
	}
	for k := range it.HeaderExtra {
		it.HeaderExtra[k] ^= 0xff
	}
}

// A callback that overwrites the payload and header extension of every Item, of Iterate or of MergeIterate,
// changes neither the bytes the Reader retains nor what a later read of the same Reader yields:
// a compressed block is decoded from the retained bytes in place,
// but a None block, whose body is the payloads, is copied first.
func TestRetainedBytesUnaffectedByCallbacks(t *testing.T) {
	t.Parallel()

	for name, file := range retainedPacks(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			r := mustOpen(t, file, ReaderOptions{})
			require.NotEmpty(t, r.held, "Open retains the pack")
			held := retainedCopy(r)
			want := iterate(t, r, Query{Payloads: true})
			require.NotEmpty(t, want.items)

			_, err := r.Iterate(t.Context(), Query{Payloads: true}, func(it *Item) error {
				overwriteItemBytes(it)
				return nil
			})
			require.NoError(t, err)
			_, err = MergeIterate(t.Context(), []*Reader{r}, Query{Payloads: true}, MergeIterateOptions{Order: OrderCapture}, func(it *Item) error {
				overwriteItemBytes(it)
				return nil
			})
			require.NoError(t, err)

			assert.Equal(t, held, retainedCopy(r), "the retained bytes are unchanged")
			assert.Equal(t, want, iterate(t, r, Query{Payloads: true}), "a later read yields the same")
			items, res, err := collectMergeIterate(t.Context(), []*Reader{r}, Query{Payloads: true}, MergeIterateOptions{Order: OrderTime})
			require.NoError(t, err)
			assert.Equal(t, want.items, items)
			assert.Equal(t, want.res, res)
		})
	}
}

// errStopRead ends a read early.
var errStopRead = errors.New("stop")

// Reads of one Reader nested in a callback, ended early, or run on two goroutines at once,
// each overwriting the bytes of its Items, each yield what a lone read yields;
// with -race, any write two reads share is reported.
func TestRetainedReadsNestedAndConcurrent(t *testing.T) {
	t.Parallel()

	for name, file := range retainedPacks(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			r := mustOpen(t, file, ReaderOptions{})
			held := retainedCopy(r)
			want := iterate(t, r, Query{Payloads: true})

			// Nested: an inner read in the first callback of the outer read.
			var nested iterRun
			var outer []iterItem
			_, err := r.Iterate(t.Context(), Query{Payloads: true}, func(it *Item) error {
				outer = append(outer, copyItem(it))
				if len(outer) == 1 {
					nested = iterate(t, r, Query{Payloads: true})
				}
				overwriteItemBytes(it)

				return nil
			})
			require.NoError(t, err)
			assert.Equal(t, want, nested, "the nested read")
			assert.Equal(t, want.items, outer, "the outer read")

			// Early exit after two records.
			n := 0
			_, err = r.Iterate(t.Context(), Query{Payloads: true}, func(it *Item) error {
				overwriteItemBytes(it)
				if n++; n == 2 {
					return errStopRead
				}
				return nil
			})
			require.ErrorIs(t, err, errStopRead)

			// Concurrent: both reads stop at their first record until the other has reached its own.
			var wg sync.WaitGroup
			ready := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
			got := [2][]iterItem{}
			errs := [2]error{}
			for g := range 2 {
				wg.Go(func() {
					first := true
					_, errs[g] = r.Iterate(t.Context(), Query{Payloads: true}, func(it *Item) error {
						got[g] = append(got[g], copyItem(it))
						if first {
							first = false
							close(ready[g])
							<-ready[1-g]
						}
						overwriteItemBytes(it)

						return nil
					})
				})
			}
			wg.Wait()
			for g := range 2 {
				require.NoError(t, errs[g])
				assert.Equal(t, want.items, got[g], "goroutine %d", g)
			}
			assert.Equal(t, held, retainedCopy(r), "the retained bytes are unchanged")
		})
	}
}

// Two retained zstd blocks of one capture with the same first seq but different bytes are both kept:
// MergeIterate yields both versions as a conflict, and Merge reports the conflict,
// each comparing the blocks' bytes as read, which are the retained bytes themselves.
func TestRetainedBlocksWithTheSameFirstSeqCompared(t *testing.T) {
	t.Parallel()

	a := planTestPack(t, seg0, captureLow, false, blockAt(0, 6, 7))
	b := planTestPack(t, seg1, captureLow, false, changed(blockAt(0, 6, 7), 1, 7))
	readers := []*Reader{mustOpen(t, a, ReaderOptions{}), mustOpen(t, b, ReaderOptions{})}
	for _, r := range readers {
		require.NotEmpty(t, r.held, "planTestPack writes small zstd packs, which Open retains")
	}
	for _, order := range []Order{OrderCapture, OrderTime} {
		items, res, err := collectMergeIterate(t.Context(), readers, Query{Payloads: true}, MergeIterateOptions{Order: order})
		require.NoError(t, err)
		assert.Len(t, items, 3, "order %v: seq 6 once, seq 7 in two versions", order)
		assert.Len(t, res.Conflicts, 1, "order %v", order)
	}

	ma := mergePackCodec(t, CodecZstd, seg0, blockAt(0, 6, 7))
	mb := mergePackCodec(t, CodecZstd, seg1, changed(blockAt(0, 6, 7), 1, 7))
	var conflicts int
	opts := mergeOpts()
	opts.OnConflict = func(Conflict) { conflicts++ }
	_, err := Merge(t.Context(), &bytes.Buffer{}, inputsOf(ma, mb), opts)
	require.ErrorIs(t, err, ErrMergeConflict)
	assert.Equal(t, 1, conflicts)
}
