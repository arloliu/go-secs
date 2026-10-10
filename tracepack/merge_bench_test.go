package tracepack

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// mergeBenchSegments returns recs, one hour of traffic from layoutStart, as 12 zstd segments of five minutes each,
// with contiguous seqs from 0, as a recorder that flushes every five minutes writes them.
func mergeBenchSegments(tb testing.TB, recs []Record) [][]byte {
	tb.Helper()

	const segments = 12
	period := time.Hour / segments
	out := make([][]byte, 0, segments)
	seq, i := uint64(0), 0
	for k := range segments {
		start := layoutStart.Add(time.Duration(k) * period)
		end := start.Add(period)
		j := i
		for j < len(recs) && recs[j].TSUTCNs < end.UnixNano() {
			j++
		}

		m := readerTestMeta()
		m.PeriodStart, m.PeriodEnd, m.SeqStart = start.UnixNano(), end.UnixNano(), seq
		var buf bytes.Buffer
		w, err := NewWriter(&buf, WriterOptions{
			Meta: m, Facts: PackFacts{AnyClassified: true}, Codec: CodecZstd,
			AssignSeq: true, NextSeq: seq, CaptureID: perfCapture,
		})
		require.NoError(tb, err)
		for r := i; r < j; r++ {
			require.NoError(tb, w.Append(&recs[r]))
		}
		_, err = w.Close()
		require.NoError(tb, err)

		out = append(out, buf.Bytes())
		seq += uint64(j - i)
		i = j
	}
	require.Equal(tb, len(recs), i, "every record in a segment")

	return out
}

// BenchmarkMergeCoalesce merges the 12 five-minute zstd segments of one hour of idle or busy traffic into a zstd archive,
// once coalescing the segments' blocks and once with MergeOptions.NoCoalesce, which copies every block verbatim.
// Its throughput counts the content bytes of the input records, a record header and the payload per record,
// so both modes report in one unit;
// archive-bytes/op and blocks/op report the archive each writes.
func BenchmarkMergeCoalesce(b *testing.B) {
	for _, profile := range []struct {
		name string
		busy bool
	}{{"idle", false}, {"busy", true}} {
		recs := perfHour(profile.busy, 0)
		segs := mergeBenchSegments(b, recs)
		for _, mode := range []struct {
			name       string
			noCoalesce bool
		}{{"coalesce", false}, {"nocoalesce", true}} {
			b.Run(profile.name+"/"+mode.name, func(b *testing.B) {
				opts := MergeOptions{
					Writer: "bench", ScopeGeneration: 1, PublisherEpoch: 1, Codec: CodecZstd, NoCoalesce: mode.noCoalesce,
					OnConflict: func(Conflict) {},
				}
				b.SetBytes(perfContentBytes(recs))
				b.ReportAllocs()
				var out bytes.Buffer
				var rep MergeReport
				for b.Loop() {
					out.Reset()
					var err error
					rep, err = Merge(b.Context(), &out, inputsOf(segs...), opts)
					if err != nil {
						b.Fatal(err)
					}
				}
				b.ReportMetric(float64(out.Len()), "archive-bytes/op")
				b.ReportMetric(float64(rep.Blocks), "blocks/op")
			})
		}
	}
}
