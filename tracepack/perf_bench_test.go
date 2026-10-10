package tracepack

// Benchmarks of the read and write paths a caller of an object store sees,
// on the synthetic HSMS traffic of layout_bench_test.go, with every pack written with the zstd codec:
//
//	GOWORK=off go test -run '^$' -bench 'OpenIterateHour|MergeIterateDay|MergeIterateLargePacks|WriterZstd|OpenAllocs' -benchmem -count 6
//
// The read benchmarks serve each pack through a ReaderAt that counts its calls and reports
// readat/op (ReadAt calls per operation), readbytes/op (bytes requested per operation)
// and maxinflight (the most calls in flight at once, a high-water mark over all iterations, not a per-operation value).
// BenchmarkMergeIterateDay and BenchmarkMergeIterateLargePacks also delay every call by perfLatency,
// standing in for the round trip of an object store GET.
// Their ns/op covers both phases of an operation, the opens and the MergeIterate after them;
// open-ns/op, open-readat/op and open-readbytes/op cover the opens alone,
// and iterate-ns/op, readat/op, readbytes/op and maxinflight cover MergeIterate alone.
//
// BenchmarkWriterZstd reports Writer throughput in uncompressed bytes per second on one goroutine, so per core;
// the zstd codec encodes at the encoder's default level (klauspost/compress SpeedDefault, about zstd level 3).

import (
	"bytes"
	"errors"
	"io"
	"math/rand/v2"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// perfLatency is the delay perfReaderAt adds to every ReadAt call in the latency benchmarks.
const perfLatency = 10 * time.Millisecond

// perfCapture is the capture of every pack the benchmarks write.
var perfCapture = UUID{0x01, 0x9a, 0, 0, 0, 0, 0x70, 0, 0x80, 0, 0, 0, 0, 0, 0, 0x11}

// perfStats counts the ReadAt calls of one or more perfReaderAt values.
type perfStats struct {
	calls       atomic.Int64
	bytes       atomic.Int64
	inflight    atomic.Int64
	maxInflight atomic.Int64
}

// report adds readat/op, readbytes/op and maxinflight to b's results.
func (s *perfStats) report(b *testing.B) {
	b.ReportMetric(float64(s.calls.Load())/float64(b.N), "readat/op")
	b.ReportMetric(float64(s.bytes.Load())/float64(b.N), "readbytes/op")
	b.ReportMetric(float64(s.maxInflight.Load()), "maxinflight")
}

// reportOpen adds open-readat/op and open-readbytes/op to b's results.
func (s *perfStats) reportOpen(b *testing.B) {
	b.ReportMetric(float64(s.calls.Load())/float64(b.N), "open-readat/op")
	b.ReportMetric(float64(s.bytes.Load())/float64(b.N), "open-readbytes/op")
}

// perfReaderAt serves a pack from memory, counting its calls in stats and delaying each by delay.
// The delay simulates the round trip of an object store GET; like a GET already sent, it ignores the context.
type perfReaderAt struct {
	data  []byte
	delay time.Duration
	stats *perfStats
}

// ReadAt reads from the pack after the delay, counting the call while it is in flight.
func (r *perfReaderAt) ReadAt(p []byte, off int64) (int, error) {
	s := r.stats
	s.calls.Add(1)
	s.bytes.Add(int64(len(p)))
	cur := s.inflight.Add(1)
	defer s.inflight.Add(-1)
	for {
		m := s.maxInflight.Load()
		if cur <= m || s.maxInflight.CompareAndSwap(m, cur) {
			break
		}
	}
	if r.delay > 0 {
		time.Sleep(r.delay)
	}
	if off < 0 {
		return 0, errors.New("perfReaderAt: negative offset")
	}
	if off >= int64(len(r.data)) {
		return 0, io.EOF
	}
	n := copy(p, r.data[off:])
	if n < len(p) {
		return n, io.EOF
	}

	return n, nil
}

// perfHour returns the records of the given hour of traffic after layoutStart,
// generated from seed hour+1, without the replies that fall after the hour.
func perfHour(busy bool, hour int) []Record {
	recs := layoutRecords(layoutTraffic(busy, time.Hour, uint64(hour)+1))
	shift := int64(hour) * int64(time.Hour)
	endNs := layoutStart.Add(time.Hour).UnixNano()
	n := 0
	for i := range recs {
		if recs[i].TSUTCNs >= endNs {
			continue
		}
		recs[n] = recs[i]
		recs[n].TSUTCNs += shift
		recs[n].MonoNs += shift
		n++
	}

	return recs[:n]
}

// perfPack writes recs, the records of the given hour after layoutStart, into one segment of perfCapture
// whose first seq is seq, with the zstd codec and the given block threshold (zero for the default).
func perfPack(tb testing.TB, recs []Record, hour int, seq uint64, blockThreshold int) []byte {
	tb.Helper()

	start := layoutStart.Add(time.Duration(hour) * time.Hour)
	m := readerTestMeta()
	m.PeriodStart, m.PeriodEnd, m.SeqStart = start.UnixNano(), start.Add(time.Hour).UnixNano(), seq
	var buf bytes.Buffer
	w, err := NewWriter(&buf, WriterOptions{
		Meta: m, Facts: PackFacts{AnyClassified: true}, Codec: CodecZstd, BlockThreshold: blockThreshold,
		AssignSeq: true, NextSeq: seq, CaptureID: perfCapture,
	})
	require.NoError(tb, err)
	for i := range recs {
		require.NoError(tb, w.Append(&recs[i]))
	}
	_, err = w.Close()
	require.NoError(tb, err)

	return buf.Bytes()
}

// perfDay returns hours consecutive hourly packs of busy traffic, one capture with contiguous seqs.
func perfDay(tb testing.TB, hours, blockThreshold int) [][]byte {
	tb.Helper()

	packs := make([][]byte, hours)
	seq := uint64(0)
	for h := range packs {
		recs := perfHour(true, h)
		packs[h] = perfPack(tb, recs, h, seq, blockThreshold)
		seq += uint64(len(recs))
	}

	return packs
}

// perfContentBytes returns the uncompressed bytes of recs in a block: a record header and the payload per record.
func perfContentBytes(recs []Record) int64 {
	n := int64(len(recs)) * recordHeaderLen
	for i := range recs {
		n += int64(len(recs[i].Payload))
	}

	return n
}

// perfPhases accumulates the time of the two phases of a mergeIteratePacks operation.
type perfPhases struct {
	open, iterate time.Duration
}

// report adds open-ns/op and iterate-ns/op to b's results.
func (t *perfPhases) report(b *testing.B) {
	b.ReportMetric(float64(t.open.Nanoseconds())/float64(b.N), "open-ns/op")
	b.ReportMetric(float64(t.iterate.Nanoseconds())/float64(b.N), "iterate-ns/op")
}

// mergeIteratePacks opens every pack in turn through a perfReaderAt with perfLatency and opts,
// counting the calls in opens, then reads them all in time order with payloads, counting the calls in reads;
// it adds the time of each phase to t.
func mergeIteratePacks(b *testing.B, packs [][]byte, opts ReaderOptions, opens, reads *perfStats, t *perfPhases) {
	start := time.Now()
	readers := make([]*Reader, len(packs))
	ras := make([]*perfReaderAt, len(packs))
	for i, p := range packs {
		ras[i] = &perfReaderAt{data: p, delay: perfLatency, stats: opens}
		r, err := Open(b.Context(), ras[i], int64(len(p)), opts)
		require.NoError(b, err)
		readers[i] = r
	}
	// Open has returned and started nothing in the background, so no call is in flight while the counters change.
	for _, ra := range ras {
		ra.stats = reads
	}
	opened := time.Now()
	res, err := MergeIterate(b.Context(), readers, Query{Payloads: true}, MergeIterateOptions{Order: OrderTime},
		func(*Item) error { return nil })
	t.open += opened.Sub(start)
	t.iterate += time.Since(opened)
	require.NoError(b, err)
	require.True(b, res.Complete())
}

// BenchmarkOpenIterateHour opens one hourly pack and iterates its records with payloads,
// for idle traffic, read whole by Open, and busy traffic, above the default whole-read threshold.
func BenchmarkOpenIterateHour(b *testing.B) {
	for _, tt := range []struct {
		name string
		busy bool
	}{
		{"idle", false},
		{"busy", true},
	} {
		b.Run(tt.name, func(b *testing.B) {
			recs := perfHour(tt.busy, 0)
			pack := perfPack(b, recs, 0, 0, 0)
			var stats perfStats
			ra := &perfReaderAt{data: pack, stats: &stats}
			b.SetBytes(perfContentBytes(recs))
			b.ReportAllocs()
			for b.Loop() {
				r, err := Open(b.Context(), ra, int64(len(pack)), ReaderOptions{})
				require.NoError(b, err)
				res, err := r.Iterate(b.Context(), Query{Payloads: true}, func(*Item) error { return nil })
				require.NoError(b, err)
				require.True(b, res.Complete())
			}
			stats.report(b)
			b.ReportMetric(float64(len(pack)), "pack-B")
		})
	}
}

// BenchmarkMergeIterateDay opens 24 hourly packs of busy traffic, one block each, under the latency reader
// and reads them in time order, with the default ReaderOptions.
func BenchmarkMergeIterateDay(b *testing.B) {
	packs := perfDay(b, 24, 0)
	var opens, reads perfStats
	var phases perfPhases
	b.ReportAllocs()
	for b.Loop() {
		mergeIteratePacks(b, packs, ReaderOptions{}, &opens, &reads, &phases)
	}
	phases.report(b)
	opens.reportOpen(b)
	reads.report(b)
}

// BenchmarkMergeIterateLargePacks reads 6 hourly packs of busy traffic in blocks of at most 256 KiB in time order,
// under the latency reader, with a whole-read threshold and windows small enough that Open never holds a whole block,
// so every block is read by MergeIterate whatever the default ReaderOptions.
func BenchmarkMergeIterateLargePacks(b *testing.B) {
	packs := perfDay(b, 6, 256<<10)
	opts := ReaderOptions{WholeReadThreshold: 64 << 10, HeadWindow: 16 << 10, TailWindow: 16 << 10}
	var opens, reads perfStats
	var phases perfPhases
	b.ReportAllocs()
	for b.Loop() {
		mergeIteratePacks(b, packs, opts, &opens, &reads, &phases)
	}
	phases.report(b)
	opens.reportOpen(b)
	reads.report(b)
}

// BenchmarkWriterZstd writes one hour of busy traffic into a pack with the zstd codec and block validation on,
// reporting throughput in uncompressed bytes.
func BenchmarkWriterZstd(b *testing.B) {
	recs := perfHour(true, 0)
	m := readerTestMeta()
	m.PeriodStart, m.PeriodEnd = layoutStart.UnixNano(), layoutStart.Add(time.Hour).UnixNano()
	var buf bytes.Buffer
	b.SetBytes(perfContentBytes(recs))
	b.ReportAllocs()
	for b.Loop() {
		buf.Reset()
		w, err := NewWriter(&buf, WriterOptions{
			Meta: m, Facts: PackFacts{AnyClassified: true}, Codec: CodecZstd, AssignSeq: true, CaptureID: perfCapture,
		})
		require.NoError(b, err)
		for i := range recs {
			require.NoError(b, w.Append(&recs[i]))
		}
		_, err = w.Close()
		require.NoError(b, err)
	}
	b.ReportMetric(float64(buf.Len()), "pack-B")
}

// perfSizedPack returns a pack of about size bytes:
// records of one hour whose payloads are HSMS frames with random text, so the zstd codec cannot shrink them.
// Open never decodes a block, so the content does not matter to it; the random text only reaches the size cheaply.
func perfSizedPack(tb testing.TB, size int) []byte {
	tb.Helper()

	const textLen = 4 << 10
	rng := rand.New(rand.NewPCG(uint64(size), 1))
	m := readerTestMeta()
	m.PeriodStart, m.PeriodEnd = layoutStart.UnixNano(), layoutStart.Add(time.Hour).UnixNano()
	buf := bytes.NewBuffer(make([]byte, 0, size+size/8))
	w, err := NewWriter(buf, WriterOptions{
		Meta: m, Facts: PackFacts{AnyClassified: true}, Codec: CodecZstd, AssignSeq: true, CaptureID: perfCapture,
	})
	require.NoError(tb, err)
	for i, written := 0, 0; written < size-1024; i++ {
		text := make([]byte, textLen)
		for j := 0; j < textLen; j += 8 {
			v := rng.Uint64()
			for k := range 8 {
				text[j+k] = byte(v >> (8 * k))
			}
		}
		msg := layoutMsg{at: time.Duration(i) * time.Millisecond, dir: DirEquipmentToHost, kind: KindData,
			stream: 6, function: 11, w: true, sysBytes: uint32(i), text: text}
		recs := layoutRecords([]layoutMsg{msg})
		require.NoError(tb, w.Append(&recs[0]))
		written += recordHeaderLen + len(recs[0].Payload)
	}
	_, err = w.Close()
	require.NoError(tb, err)

	return buf.Bytes()
}

// BenchmarkOpenAllocs opens packs of 64 KiB, of the default whole-read threshold, and of 4 and 16 times it,
// reporting Open's allocations, which should stay flat in the pack size above the threshold.
// The largest pack is 16 times the threshold, not more, so that it stays at 64 MiB for a 4 MiB threshold:
// the benchmark builds it in memory on every run.
func BenchmarkOpenAllocs(b *testing.B) {
	for _, size := range []int{64 << 10, DefaultWholeReadThreshold, 4 * DefaultWholeReadThreshold, 16 * DefaultWholeReadThreshold} {
		b.Run(strconv.Itoa(size>>10)+"KiB", func(b *testing.B) {
			pack := perfSizedPack(b, size)
			var stats perfStats
			ra := &perfReaderAt{data: pack, stats: &stats}
			b.ReportAllocs()
			for b.Loop() {
				_, err := Open(b.Context(), ra, int64(len(pack)), ReaderOptions{})
				require.NoError(b, err)
			}
			stats.report(b)
			b.ReportMetric(float64(len(pack)), "pack-B")
		})
	}
}
