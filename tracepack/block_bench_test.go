package tracepack

// Benchmarks of a block summary, which the Writer, Verify and Repair build record by record:
//
//	GOWORK=off go test -run '^$' -bench 'BlockSummary' -benchmem
//
// BenchmarkBlockSummaryEpochs summarizes a block of n records in n epochs,
// where a search over the epoch entries would take time quadratic in n;
// the summary's epoch index keeps it linear, so ns/op and B/op grow about tenfold from each size to the next.

import (
	"strconv"
	"testing"
)

// BenchmarkBlockSummaryEpochs summarizes a block of n records, each in an epoch of its own, for n of 1 000 to 100 000.
func BenchmarkBlockSummaryEpochs(b *testing.B) {
	for _, n := range []uint32{1_000, 10_000, 100_000} {
		recs := epochRecords(0, epochRange(0, n-1))
		b.Run(strconv.FormatUint(uint64(n), 10), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				var s blockSummary
				summarize(&s, recs)
			}
		})
	}
}
