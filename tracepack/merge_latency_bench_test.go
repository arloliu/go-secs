package tracepack

import (
	"bytes"
	"testing"
)

// BenchmarkMergeSegmentsLatency merges the busy hour's 12 segments, each behind a ReaderAt that takes perfLatency per call,
// as an object store would serve them, reporting the ReadAt calls per merge.
func BenchmarkMergeSegmentsLatency(b *testing.B) {
	recs := perfHour(true, 0)
	segs := mergeBenchSegments(b, recs)
	opts := MergeOptions{Writer: "bench", ScopeGeneration: 1, PublisherEpoch: 1, OnConflict: func(Conflict) {}}
	var stats perfStats
	var out bytes.Buffer
	b.SetBytes(perfContentBytes(recs))
	b.ReportAllocs()
	for b.Loop() {
		out.Reset()
		inputs := make([]MergeInput, len(segs))
		for i, s := range segs {
			inputs[i] = MergeInput{ReaderAt: &perfReaderAt{data: s, delay: perfLatency, stats: &stats}, Size: int64(len(s))}
		}
		if _, err := Merge(b.Context(), &out, inputs, opts); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(stats.calls.Load())/float64(b.N), "readat/op")
}
