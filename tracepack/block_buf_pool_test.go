package tracepack

import (
	"bytes"
	"fmt"
	"math"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

// listBlockBufs is a blockBufPool that lends its buffers last in, first out, deterministically, also under -race,
// where a sync.Pool drops some of what it is given.
type listBlockBufs struct {
	free []*blockBuf
}

func (l *listBlockBufs) get() *blockBuf {
	n := len(l.free)
	if n == 0 {
		return new(blockBuf)
	}
	buf := l.free[n-1]
	l.free[n-1] = nil
	l.free = l.free[:n-1]

	return buf
}

func (l *listBlockBufs) put(buf *blockBuf) {
	if readyForPool(buf) {
		l.free = append(l.free, buf)
	}
}

// poolTestPack returns a pack of hours hours of perHour data records with codec c, every record header widened when wide is set.
func poolTestPack(t *testing.T, c Codec, hours, perHour int, wide bool) []byte {
	t.Helper()

	file := writeReaderPack(t, readerPackConfig{codec: c, validate: true}, hourRecords(hours, perHour)).file
	if wide {
		file = widenFile(t, file, func(int) bool { return true })
	}

	return file
}

// poolItem is a copy of what an Item holds.
type poolItem struct {
	rec   Record
	extra []byte
	block int
}

func copyPoolItem(it *Item) poolItem {
	rec := it.Record
	rec.Payload = bytes.Clone(rec.Payload)

	return poolItem{rec: rec, extra: bytes.Clone(it.HeaderExtra), block: it.Block}
}

// iterateAll returns copies of every item r's Iterate yields with payloads, and its Result.
func iterateAll(t *testing.T, r *Reader) ([]poolItem, Result) {
	t.Helper()

	var items []poolItem
	res, err := r.Iterate(t.Context(), Query{Payloads: true}, func(it *Item) error {
		items = append(items, copyPoolItem(it))

		return nil
	})
	require.NoError(t, err)

	return items, res
}

// TestIterateNestedPooledReads runs an Iterate inside the callback of another, both lending from one pool:
// the inner read must not take the outer read's buffer, so the outer item is unchanged when the inner read returns.
func TestIterateNestedPooledReads(t *testing.T) {
	t.Parallel()

	for _, c := range []Codec{CodecNone, CodecZstd} {
		t.Run(c.String(), func(t *testing.T) {
			t.Parallel()

			pool := &listBlockBufs{}
			outer := mustOpen(t, poolTestPack(t, c, 2, 20, true), ReaderOptions{})
			inner := mustOpen(t, poolTestPack(t, c, 3, 31, false), ReaderOptions{})
			outer.bufs, inner.bufs = pool, pool
			wantOuter, wantOuterRes := iterateAll(t, mustOpen(t, poolTestPack(t, c, 2, 20, true), ReaderOptions{}))
			wantInner, _ := iterateAll(t, mustOpen(t, poolTestPack(t, c, 3, 31, false), ReaderOptions{}))

			var got []poolItem
			res, err := outer.Iterate(t.Context(), Query{Payloads: true}, func(it *Item) error {
				before := copyPoolItem(it)
				innerItems, _ := iterateAll(t, inner)
				require.Equal(t, wantInner, innerItems)
				require.Equal(t, before, copyPoolItem(it), "the outer item after the inner read")
				got = append(got, before)

				return nil
			})
			require.NoError(t, err)
			require.Equal(t, wantOuter, got)
			require.Equal(t, wantOuterRes, res)
			require.Len(t, pool.free, 2, "both reads returned their buffers")
		})
	}
}

// requirePooledBack checks that pool holds exactly buf again, emptied, its storage kept.
func requirePooledBack(t *testing.T, pool *listBlockBufs, buf *blockBuf) {
	t.Helper()

	require.Len(t, pool.free, 1)
	require.Same(t, buf, pool.free[0], "the read returned the buffer it was lent")
	require.Empty(t, buf.raw)
	require.Positive(t, cap(buf.raw), "the read used the buffer")
	require.Empty(t, buf.decoded)
	require.Empty(t, buf.rows)
	require.Empty(t, buf.offs)
	require.Equal(t, decodedBlock{}, buf.blk)
}

// TestSingleBlockReadsReuseTheirBuffer lends one buffer to Iterate, ExportJSONL and Verify in turn,
// each read of another pack than the one before:
// each returns that same buffer, emptied, and gives the result of a read through a fresh buffer.
func TestSingleBlockReadsReuseTheirBuffer(t *testing.T) {
	t.Parallel()

	files := [][]byte{
		poolTestPack(t, CodecZstd, 3, 40, false),
		poolTestPack(t, CodecNone, 2, 25, true),
		poolTestPack(t, CodecZstd, 1, 7, true),
	}
	// Each read returns what it gives: the items' copies, the export and the Result, or the VerifyReport, kept across later reads.
	reads := []struct {
		name string
		run  func(t *testing.T, r *Reader) any
	}{
		{"iterate", func(t *testing.T, r *Reader) any {
			items, res := iterateAll(t, r)

			return []any{items, res}
		}},
		{"export", func(t *testing.T, r *Reader) any {
			var out bytes.Buffer
			res, err := ExportJSONL(t.Context(), r, &out)
			require.NoError(t, err)

			return []any{out.String(), res}
		}},
		{"verify", func(t *testing.T, r *Reader) any {
			rep, err := r.analyze(t.Context())
			require.NoError(t, err)

			return rep
		}},
	}
	for _, rd := range reads {
		t.Run(rd.name, func(t *testing.T) {
			t.Parallel()

			buf := new(blockBuf)
			pool := &listBlockBufs{free: []*blockBuf{buf}}
			all := slices.Concat(files, files)
			kept, printed := make([]any, 0, len(all)), make([]string, 0, len(all))
			for _, file := range all {
				want := rd.run(t, mustOpen(t, file, ReaderOptions{}))
				r := mustOpen(t, file, ReaderOptions{})
				r.bufs = pool
				got := rd.run(t, r)
				require.Equal(t, want, got)
				requirePooledBack(t, pool, buf)
				kept, printed = append(kept, got), append(printed, fmt.Sprintf("%+v", got))
				for k := range kept {
					require.Equal(t, printed[k], fmt.Sprintf("%+v", kept[k]), "what read %d returned, after the later reads", k)
				}
			}
		})
	}
}

// TestBlockBufBytes checks the capacity sum in int64, where an int sum would overflow on 32-bit platforms,
// and that a buffer above maxPooledBlockBuf is not pooled.
func TestBlockBufBytes(t *testing.T) {
	t.Parallel()

	const m = math.MaxInt32
	require.Equal(t, int64(7)*m, blockBufBytes(m, m, m, m))
	require.Equal(t, int64(maxPooledBlockBuf), blockBufBytes(maxPooledBlockBuf-4, 0, 0, 1))

	require.True(t, readyForPool(&blockBuf{raw: make([]byte, 3, maxPooledBlockBuf)}))
	require.False(t, readyForPool(&blockBuf{raw: make([]byte, 0, maxPooledBlockBuf), offs: make([]uint32, 0, 1)}))
}
