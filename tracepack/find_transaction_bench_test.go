package tracepack

// Benchmarks of transaction lookups over the busy traffic of perf_bench_test.go:
//
//	GOWORK=off go test -run '^$' -bench 'FindTransaction$' -benchmem -count 6
//	GOWORK=off go test -run '^$' -bench 'FindTransactionHour' -benchmem -benchtime 1x -count 3
//
// Every lookup reads two hourly packs of busy traffic, one capture, with the default TxOptions (two hours scheduled).
// BenchmarkFindTransaction looks up one primary near the start, middle or end of the first hour,
// with the default ReaderOptions, which hold each pack whole after Open,
// and, as "end-latency", with 256 KiB blocks and windows so small
// that every block is read through perfReaderAt and its perfLatency.
// BenchmarkFindTransactionHour pairs every primary of the first hour by one lookup each,
// the cost a batch read is measured against;
// one operation takes tens of seconds, so it runs with -benchtime 1x.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// perfPrimaries returns the keys of the primaries of recs, the records of the hour of perfHour(busy, 0) after layoutStart:
// every data record whose function is available and odd, keyed by perfCapture, its seq and its hour.
// The packs of perfDay assign seqs from 0 in record order, so a record's index is its seq.
func perfPrimaries(recs []Record) []TxKey {
	keys := make([]TxKey, 0, len(recs)/2)
	for i := range recs {
		h := recs[i].HSMSHeader()
		if recs[i].Kind != KindData || h.Available&FieldValidityFunction == 0 || h.Function%2 == 0 {
			continue
		}
		keys = append(keys, TxKeyOf(perfCapture, uint64(i), time.Unix(0, recs[i].TSUTCNs)))
	}

	return keys
}

// perfTxSource opens packs through perfReaderAt values that count their calls in stats and delay each by delay,
// and returns the source over them.
func perfTxSource(b *testing.B, packs [][]byte, opts ReaderOptions, delay time.Duration, stats *perfStats) PackSource {
	b.Helper()

	readers := make([]*Reader, len(packs))
	for i, p := range packs {
		r, err := Open(b.Context(), &perfReaderAt{data: p, delay: delay, stats: stats}, int64(len(p)), opts)
		require.NoError(b, err)
		readers[i] = r
	}
	src, err := NewReaderSource(readers)
	require.NoError(b, err)

	return src
}

// BenchmarkFindTransaction looks up one primary of the first of two busy hours, near the start, middle or end of the hour.
func BenchmarkFindTransaction(b *testing.B) {
	packs := perfDay(b, 2, 0)
	keys := perfPrimaries(perfHour(true, 0))
	require.NotEmpty(b, keys)
	small := perfDay(b, 2, 256<<10)
	smallOpts := ReaderOptions{WholeReadThreshold: 64 << 10, HeadWindow: 16 << 10, TailWindow: 16 << 10}

	for _, tt := range []struct {
		name  string
		frac  float64
		packs [][]byte
		opts  ReaderOptions
		delay time.Duration
	}{
		{"start", 0.01, packs, ReaderOptions{}, 0},
		{"middle", 0.5, packs, ReaderOptions{}, 0},
		{"end", 0.99, packs, ReaderOptions{}, 0},
		{"end-latency", 0.99, small, smallOpts, perfLatency},
	} {
		b.Run(tt.name, func(b *testing.B) {
			var stats perfStats
			src := perfTxSource(b, tt.packs, tt.opts, tt.delay, &stats)
			key := keys[int(float64(len(keys)-1)*tt.frac)]
			// Open's calls are not the lookup's.
			stats.reset()
			var res TxResult
			b.ReportAllocs()
			for b.Loop() {
				var err error
				res, err = FindTransaction(b.Context(), src, key, TxOptions{})
				if err != nil {
					b.Fatal(err)
				}
			}
			stats.report(b)
			b.ReportMetric(float64(len(res.Records)), "records")
		})
	}
}

// BenchmarkFindTransactionHour pairs every primary of the first of two busy hours by one FindTransaction each.
func BenchmarkFindTransactionHour(b *testing.B) {
	packs := perfDay(b, 2, 0)
	keys := perfPrimaries(perfHour(true, 0))
	var stats perfStats
	src := perfTxSource(b, packs, ReaderOptions{}, 0, &stats)
	b.Logf("%d primaries", len(keys))
	b.ReportAllocs()
	var records int
	for b.Loop() {
		records = 0
		for _, key := range keys {
			res, err := FindTransaction(b.Context(), src, key, TxOptions{})
			if err != nil {
				b.Fatal(err)
			}
			records += len(res.Records)
		}
	}
	b.ReportMetric(float64(len(keys)), "primaries")
	b.ReportMetric(float64(records), "records")
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(len(keys)), "ns/primary")
}

// reset zeroes the counts of s, so that what an operation counts excludes what came before it.
func (s *perfStats) reset() {
	s.calls.Store(0)
	s.bytes.Store(0)
	s.inflight.Store(0)
	s.maxInflight.Store(0)
}

// BenchmarkFindTransactions pairs every primary of the first of two busy hours with one FindTransactions call,
// against BenchmarkFindTransactionHour's lookup per primary.
func BenchmarkFindTransactions(b *testing.B) {
	packs := perfDay(b, 2, 0)
	keys := perfPrimaries(perfHour(true, 0))
	var stats perfStats
	src := perfTxSource(b, packs, ReaderOptions{}, 0, &stats)
	from := layoutStart
	r := TxRange{Capture: perfCapture, From: from, To: from.Add(time.Hour)}
	b.ReportAllocs()
	var res TxRangeResult
	var records int
	for b.Loop() {
		records = 0
		var err error
		res, err = FindTransactions(b.Context(), src, r, TxRangeOptions{}, func(p *TxPairing) error {
			records += len(p.Result.Records)
			return nil
		})
		if err != nil {
			b.Fatal(err)
		}
	}
	if res.Primaries != len(keys) {
		b.Fatalf("%d primaries paired, %d in the hour", res.Primaries, len(keys))
	}
	b.ReportMetric(float64(res.Primaries), "primaries")
	b.ReportMetric(float64(records), "records")
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(res.Primaries), "ns/primary")
}
