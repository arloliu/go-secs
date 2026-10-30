package tracepack

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"slices"
	"sync"
	"testing"

	"github.com/arloliu/go-secs/tracepack/internal/format"
	"github.com/stretchr/testify/require"
)

// listBlockBufs is a blockBufPool that lends its buffers last in, first out, deterministically, also under -race,
// where a sync.Pool drops some of what it is given.
type listBlockBufs struct {
	free []*blockBuf
}

func (l *listBlockBufs) get(blockSizes) *blockBuf {
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

// loanBufs is a blockBufPool that records every loan and every return, safe for concurrent use.
// It lends a fresh buffer, or, with reuse, the buffer returned last;
// with a shape, a held block's loan is instead a fresh buffer whose slices have the capacities of that shape against the block's sizes.
type loanBufs struct {
	reuse bool
	shape seedKind

	mu      sync.Mutex
	free    []*blockBuf
	out     map[*blockBuf]bool
	loans   int
	returns int
	faults  []string
}

func (l *loanBufs) get(hint blockSizes) *blockBuf {
	l.mu.Lock()
	defer l.mu.Unlock()

	buf := new(blockBuf)
	if hint != (blockSizes{}) && l.shape != seedEmpty {
		buf = &blockBuf{
			raw:     make([]byte, 0, seedCap(l.shape, hint.raw)),
			decoded: make([]byte, 0, seedCap(l.shape, hint.decoded)),
			rows:    make([]byte, 0, seedCap(l.shape, hint.rows)),
			offs:    make([]uint32, 0, seedCap(l.shape, hint.offs)),
		}
	} else if n := len(l.free); l.reuse && n > 0 {
		buf = l.free[n-1]
		l.free = l.free[:n-1]
	}
	if l.out == nil {
		l.out = map[*blockBuf]bool{}
	}
	l.out[buf] = true
	l.loans++

	return buf
}

func (l *loanBufs) put(buf *blockBuf) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if !l.out[buf] {
		l.faults = append(l.faults, fmt.Sprintf("a buffer given back that is not lent: %p", buf))
	}
	delete(l.out, buf)
	l.returns++
	if readyForPool(buf) {
		l.free = append(l.free, buf)
	}
}

// lent reports whether buf is lent and not given back.
func (l *loanBufs) lent(buf *blockBuf) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.out[buf]
}

// outstanding returns the number of buffers lent and not given back.
func (l *loanBufs) outstanding() int {
	l.mu.Lock()
	defer l.mu.Unlock()

	return len(l.out)
}

// requireSettled requires every buffer l lent to have been given back exactly once, loans of them in all,
// or any number when loans is negative.
func (l *loanBufs) requireSettled(t *testing.T, loans int) {
	t.Helper()

	l.mu.Lock()
	defer l.mu.Unlock()
	require.Empty(t, l.faults)
	require.Empty(t, l.out, "every buffer lent given back")
	if loans >= 0 {
		require.Equal(t, loans, l.loans, "loans")
	}
	require.Equal(t, l.loans, l.returns, "each loan given back once")
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
	require.Positive(t, cap(buf.rows), "the read used the buffer")
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

	// diag is a pack with a coverage entry carrying an unknown nested entry and a stop-unclean boundary with gap bounds;
	// walked, read by the forward walk, reports both, and bad, with its first block corrupt, fails that block and loses its records.
	diag := crashGapPack(t, seg0, captureLow, func(m *PackMeta) {
		m.Coverage = []Coverage{{
			CaptureID: new(captureLow), SeqFirst: new(uint64(10)), SeqLast: new(uint64(11)),
			Unknown: []RawEntry{{Tag: 0x0050, Type: 7, Value: []byte{9, 8, 7}}},
		}}
	}, crashGapSteps(t, BoundaryKindStopUnclean, new(crashGapStart), new(crashGapEnd)))
	walked := invalidFooterFile(t, diag.file)
	bad := flipByte(diag.file, diag.bodyByteOf(0))
	// writer holds valid blocks, one with a transport event whose payload is not a TLV body: a writer defect.
	writer := editBlock(t, defectTestPack(t).file, 0, func(b *testBlock) {
		b.payloads[2] = []byte{0xFF}
		b.headers[2].PayloadLen = 1
	})
	files := [][]byte{
		poolTestPack(t, CodecZstd, 3, 40, false),
		walked,
		poolTestPack(t, CodecNone, 2, 25, true),
		bad,
		poolTestPack(t, CodecZstd, 1, 7, true),
		writer,
	}
	// Each read returns what it gives: the items' copies, the export and the Result, or the VerifyReport;
	// diagnostics checks that the read of walked or bad reports what the test keeps across the later reads.
	reads := []struct {
		name        string
		run         func(t *testing.T, r *Reader) any
		diagnostics func(t *testing.T, walked, bad, writer any)
	}{
		{"iterate", func(t *testing.T, r *Reader) any {
			items, res := iterateAll(t, r)

			return []any{items, res}
		}, func(t *testing.T, walked, bad, _ any) {
			requireReadDiagnostics(t, readResult(t, walked), readResult(t, bad))
		}},
		{"export", func(t *testing.T, r *Reader) any {
			var out bytes.Buffer
			res, err := ExportJSONL(t.Context(), r, &out)
			require.NoError(t, err)

			return []any{out.String(), res}
		}, func(t *testing.T, walked, bad, _ any) {
			requireReadDiagnostics(t, readResult(t, walked), readResult(t, bad))
		}},
		{"verify", func(t *testing.T, r *Reader) any {
			rep, err := r.analyze(t.Context())
			require.NoError(t, err)

			return rep
		}, func(t *testing.T, _, bad, writer any) {
			rep, ok := bad.(VerifyReport)
			require.True(t, ok)
			require.NotEmpty(t, rep.Failed, "the corrupt block failed")
			require.NotEmpty(t, rep.Lost, "its records are lost")
			rep, ok = writer.(VerifyReport)
			require.True(t, ok)
			require.NotEmpty(t, rep.WriterDefects, "the event payload's writer defect")
		}},
	}
	for _, rd := range reads {
		t.Run(rd.name, func(t *testing.T) {
			t.Parallel()

			buf := new(blockBuf)
			pool := &listBlockBufs{free: []*blockBuf{buf}}
			all := slices.Concat(files, files)
			kept, errs := make([]any, 0, len(all)), make([][]string, 0, len(all))
			for _, file := range all {
				want := rd.run(t, mustOpen(t, file, ReaderOptions{}))
				r := mustOpen(t, file, ReaderOptions{})
				r.bufs = pool
				got := rd.run(t, r)
				require.Equal(t, want, got)
				requirePooledBack(t, pool, buf)
				kept, errs = append(kept, got), append(errs, errorChains(got))
			}
			rd.diagnostics(t, kept[1], kept[3], kept[5])
			// The errors each read returned keep their text, the types of their trees, and the sentinels they match.
			for k := range kept {
				require.Equal(t, errs[k], errorChains(kept[k]), "the errors of read %d, after the later reads", k)
			}
			// What each read returned is compared in depth with a fresh read's, after every later read reused the buffer.
			for k, file := range all {
				require.Equal(t, rd.run(t, mustOpen(t, file, ReaderOptions{})), kept[k], "what read %d returned, after the later reads", k)
			}
		})
	}
}

// requireReadDiagnostics checks that walked's Result reports the coverage entry, with its unknown nested entry,
// and the crash gap, with its bounds, and that bad's reports its corrupt block.
func requireReadDiagnostics(t *testing.T, walked, bad Result) {
	t.Helper()

	var coverage, crash bool
	for _, d := range walked.Incomplete {
		if d.Reason == ReasonCoverage {
			coverage = d.Coverage != nil && len(d.Coverage.Unknown) == 1 && len(d.Coverage.Unknown[0].Value) == 3
		}
		if d.Reason == ReasonCrashGap {
			crash = d.Boundary != nil && d.Boundary.GapStart != nil && d.Boundary.GapEnd != nil
		}
	}
	require.True(t, coverage, "the coverage defect with its unknown entry")
	require.True(t, crash, "the crash gap with its bounds")
	require.True(t, slices.ContainsFunc(bad.Incomplete, func(d Defect) bool { return d.Reason == ReasonCorruptBlock && d.Err != nil }))
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

// readResult returns the Result of what the iterate or export read of TestSingleBlockReadsReuseTheirBuffer returned.
func readResult(t *testing.T, v any) Result {
	t.Helper()

	pair, ok := v.([]any)
	require.True(t, ok)
	require.Len(t, pair, 2)
	res, ok := pair[1].(Result)
	require.True(t, ok)

	return res
}

// readSentinels are the sentinels a read's errors may wrap, which errorChains tests each error against with errors.Is.
var readSentinels = []error{
	ErrInvalidFooter, ErrReadLimit, ErrNotTracepack, ErrUnsupportedFormat, ErrChecksum, ErrRequiredTag, ErrFieldValue, ErrFieldLength,
	ErrReservedTag, ErrRedactionDefect, format.ErrBadMagic, format.ErrCRC, format.ErrLimit, format.ErrSchema, format.ErrShort, format.ErrCorrupt,
}

// errorChains returns, for every error a read returned in v, the text and type of each error in its tree,
// both single and multiple wraps followed, and which of readSentinels the error matches with errors.Is.
func errorChains(v any) []string {
	var out []string
	var walk func(err error, depth int)
	walk = func(err error, depth int) {
		if err == nil {
			return
		}
		out = append(out, fmt.Sprintf("%d %T: %v", depth, err, err))
		// The node's own wraps, one level: errors.As would search the whole tree instead.
		node := any(err)
		if u, ok := node.(interface{ Unwrap() error }); ok {
			walk(u.Unwrap(), depth+1)
		}
		if u, ok := node.(interface{ Unwrap() []error }); ok {
			for _, e := range u.Unwrap() {
				walk(e, depth+1)
			}
		}
	}
	chain := func(err error) {
		walk(err, 0)
		for i, sentinel := range readSentinels {
			if errors.Is(err, sentinel) {
				out = append(out, fmt.Sprintf("is %d", i))
			}
		}
		out = append(out, "--")
	}
	defects := func(ds []Defect) {
		for i := range ds {
			chain(ds[i].Err)
		}
	}
	switch r := v.(type) {
	case []any:
		if res, ok := r[1].(Result); ok {
			defects(res.Incomplete)
			for i := range res.FooterErrs {
				chain(res.FooterErrs[i].Err)
			}
		}
	case VerifyReport:
		defects(r.Failed)
		defects(r.Disagreements)
		for i := range r.WriterDefects {
			chain(r.WriterDefects[i].Err)
		}
		chain(r.FooterErr)
	default:
		panic(fmt.Sprintf("errorChains: %T", v))
	}

	return out
}
