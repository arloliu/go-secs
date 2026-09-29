package tracepack

import (
	"bytes"
	"fmt"
	"reflect"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// sweepFailureLimit bounds the failures a sweep of one pack reports in full;
// the rest are only counted, so one broken rule cannot flood the log.
const sweepFailureLimit = 10

// Indexes into sweepQueries of the queries whose baseline results newSweepBaseline checks beyond completeness.
const (
	sweepQueryEmptySF = 2
	sweepQueryPrunes  = 3
	sweepQuerySF      = 4
)

// sweepVariant is one pack of the corruption and truncation sweeps:
// the sweep records written with one codec, validated by the Writer or not, and the options Open reads the pack with.
type sweepVariant struct {
	name     string
	codec    Codec
	validate bool
	opts     ReaderOptions
}

// sweepQuery is one query the sweeps run over every mutated pack.
type sweepQuery struct {
	name string
	q    Query
}

// sweepBaseline is what the unmutated pack of a variant yields: the oracle every mutated read is judged against.
type sweepBaseline struct {
	opts    ReaderOptions
	queries []sweepQuery
	size    int
	// blocksStart is 80 + pack_metadata_len, where the first block starts.
	blocksStart uint64
	blocks      []BlockInfo
	// runs holds the complete result of each query.
	runs []iterRun
	// stored maps each seq to the record a full read yields for it.
	stored map[uint64]iterItem
	// selected holds, per query, the seqs the query yields.
	selected []map[uint64]bool
	// pruned holds, per query, the blocks the footer excludes from it.
	pruned [][]int
}

// sweepCounts counts the outcomes of the reads of one sweep.
type sweepCounts struct {
	reads    int
	openErrs int
	// complete and incomplete count, per query, the reads that were complete and incomplete.
	complete   []int
	incomplete []int
	failures   int
}

// sweepVariants returns the packs of the sweeps: codec none and zstd, each validated by the Writer and not.
// The zstd packs are read with windows small enough that Open reads the head and the tail,
// then the rest of the pack metadata and of the footer in a second round;
// the none packs are read whole.
func sweepVariants() []sweepVariant {
	windows := ReaderOptions{WholeReadThreshold: 1, HeadWindow: 96, TailWindow: 96}

	return []sweepVariant{
		{name: "none", codec: CodecNone},
		{name: "none validated", codec: CodecNone, validate: true},
		{name: "zstd", codec: CodecZstd, opts: windows},
		{name: "zstd validated", codec: CodecZstd, validate: true, opts: windows},
	}
}

// sweepRecords returns the records of the sweep packs, which sweepPack writes as three blocks:
//
//	block 0, hour 0: 0 data, 1 control, then Flush
//	block 1, hour 0: 2 socket-close transport event, 3 short capture of 7 bytes
//	block 2, hour 1: 4 data, 5 data
func sweepRecords(t testing.TB) []Record {
	t.Helper()

	h1 := blockTestHour + hourNs

	return []Record{
		testDataRecord(0, blockTestHour+10, 1),
		controlRecord(1, blockTestHour+20, 1, DirHostToEquipment),
		testEventRecord(t, 2, blockTestHour+30, 1, &TransportEvent{Event: EventSocketClose}),
		shortCapture(3, blockTestHour+40, 7),
		testDataRecord(4, h1+1, 2),
		testDataRecord(5, h1+2, 2),
	}
}

// writeFlushedPack writes recs into a new pack with codec c, validated by the Writer when validate is set,
// flushing the open block after record flushAfter;
// meta, when set, adjusts the pack metadata before the Writer is created.
func writeFlushedPack(t testing.TB, c Codec, validate bool, meta func(*PackMeta), recs []Record, flushAfter int) []byte {
	t.Helper()

	m := readerTestMeta()
	if meta != nil {
		meta(m)
	}

	var buf bytes.Buffer
	w, err := NewWriter(&buf, WriterOptions{Meta: m, Facts: PackFacts{AnyClassified: true}, Codec: c, Validate: validate})
	require.NoError(t, err)
	for i := range recs {
		require.NoError(t, w.Append(&recs[i]), "record %d", i)
		if i == flushAfter {
			require.NoError(t, w.Flush())
		}
	}
	_, err = w.Close()
	require.NoError(t, err)

	return buf.Bytes()
}

// sweepPack writes sweepRecords with codec c, validated by the Writer when validate is set,
// then widens every record header of block 1.
// The widened block is re-encoded with the same records, so it still satisfies I-2.
func sweepPack(t testing.TB, c Codec, validate bool) []byte {
	t.Helper()

	file := writeFlushedPack(t, c, validate, nil, sweepRecords(t), 1)

	return rebuildPack(t, file, func(i int, b *testBlock) {
		if i == 1 {
			widen(b)
		}
	})
}

// sweepQueries returns the queries of the sweeps:
// every record with and without payloads;
// an S/F query with an empty result, which still reads every block;
// a time query whose F-2 ranges exclude the blocks of hour 0;
// and an S/F query that selects on the payload's HSMS header fields.
func sweepQueries() []sweepQuery {
	h1 := blockTestHour + hourNs

	return []sweepQuery{
		{name: "payloads", q: Query{Payloads: true}},
		{name: "no payloads", q: Query{}},
		{name: "S6F11, empty", q: Query{Filter: Filter{SF: []SF{{Stream: 6, Function: 11}}}}},
		{name: "hour 1, prunes", q: Query{Filter: Filter{TimeFrom: new(h1), TimeTo: new(h1 + hourNs)}}},
		{name: "S1F3", q: Query{Filter: Filter{SF: []SF{{Stream: 1, Function: 3}}}}},
	}
}

// newSweepBaseline opens the unmutated pack file of v, runs every query, and requires each result complete.
func newSweepBaseline(t *testing.T, file []byte, v *sweepVariant) *sweepBaseline {
	t.Helper()

	r := mustOpen(t, file, v.opts)
	h := r.Header()
	require.True(t, h.Finalized)
	require.NoError(t, h.FooterErr)

	queries := sweepQueries()
	b := &sweepBaseline{
		opts: v.opts, queries: queries, size: len(file), blocksStart: r.blocksStart(), blocks: r.Blocks(),
		stored: make(map[uint64]iterItem),
	}
	require.Len(t, b.blocks, 3)
	for qi := range queries {
		q := &queries[qi]
		run := iterate(t, r, q.q)
		require.True(t, run.res.Complete(), "%s: %v", q.name, run.res.Incomplete)
		require.NoError(t, run.res.FooterErr, q.name)

		sel := make(map[uint64]bool, len(run.items))
		for _, it := range run.items {
			sel[it.rec.Seq] = true
		}
		var pruned []int
		for i := range b.blocks {
			if r.prunes(i, &q.q.Filter) {
				pruned = append(pruned, i)
			}
		}
		b.runs, b.selected, b.pruned = append(b.runs, run), append(b.selected, sel), append(b.pruned, pruned)
	}

	recs := sweepRecords(t)
	all := &b.runs[0]
	require.Len(t, all.items, len(recs))
	for k, it := range all.items {
		require.Equal(t, uint64(k), it.rec.Seq)
		require.Equal(t, it.block == 1, it.extra != nil, "block 1 carries the widened header bytes")
		b.stored[it.rec.Seq] = it
	}

	// The queries do what their names say over this pack.
	require.Empty(t, b.runs[sweepQueryEmptySF].items)
	require.Equal(t, []int{0, 1}, b.pruned[sweepQueryPrunes])
	require.Equal(t, []uint64{4, 5}, b.runs[sweepQueryPrunes].seqs())
	require.Equal(t, []uint64{0, 4, 5}, b.runs[sweepQuerySF].seqs(), "the short capture lacks the function byte")
	for qi, p := range b.pruned {
		if qi != sweepQueryPrunes {
			require.Empty(t, p, queries[qi].name)
		}
	}

	return b
}

// sameRecord reports whether a and b hold the same field values and payload bytes.
func sameRecord(a, b Record) bool {
	if !bytes.Equal(a.Payload, b.Payload) {
		return false
	}
	a.Payload, b.Payload = nil, nil

	return reflect.DeepEqual(a, b)
}

// sameItems reports whether a and b yield the same records, header extension bytes and blocks.
func sameItems(a, b []iterItem) bool {
	return slices.EqualFunc(a, b, func(x, y iterItem) bool {
		return sameRecord(x.rec, y.rec) && bytes.Equal(x.extra, y.extra) && x.block == y.block
	})
}

// sameErr reports whether a and b are both nil, or both non-nil with the same text.
func sameErr(a, b error) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}

	return a.Error() == b.Error()
}

// inPrunedBlock reports whether the file offset off lies in a block that the footer excludes from query qi.
func (b *sweepBaseline) inPrunedBlock(qi int, off uint64) bool {
	for _, i := range b.pruned[qi] {
		blk := &b.blocks[i]
		if off >= blk.Offset && off < blk.Offset+uint64(blk.OnDiskLen) {
			return true
		}
	}

	return false
}

// region names the structure of the unmutated pack that holds the file offset off.
func (b *sweepBaseline) region(off uint64) string {
	switch {
	case off < format.FileHeaderLen:
		return "file header"
	case off < b.blocksStart:
		return "pack metadata"
	}
	for i := range b.blocks {
		blk := &b.blocks[i]
		switch {
		case off < blk.Offset+format.EnvelopeLen:
			return fmt.Sprintf("block %d envelope", i)
		case off < blk.Offset+uint64(blk.OnDiskLen):
			return fmt.Sprintf("block %d body", i)
		}
	}
	if off < uint64(b.size-format.TrailerLen) {
		return "footer"
	}

	return "trailer"
}

// read opens data and runs every query over it, judging each outcome against the baseline and counting it.
//
// Open must fail unless wantOpen is set.
// A complete result must equal the baseline's: its items byte for byte and FooterErr;
// a complete result that differs is a silent error.
// An incomplete result must yield a subset of the baseline's records, byte for byte.
// wantComplete(qi) states which of the two query qi must give,
// so a mutation the Reader misses fails the sweep, and so does one it reports although the query does not read the mutated bytes.
func (b *sweepBaseline) read(t *testing.T, data []byte, what func() string, wantOpen bool, wantComplete func(qi int) bool, c *sweepCounts) {
	t.Helper()

	c.reads++
	r, err := Open(t.Context(), bytes.NewReader(data), int64(len(data)), b.opts)
	if err != nil {
		c.openErrs++
		if wantOpen {
			c.failf(t, "%s: Open failed: %v", what(), err)
		}

		return
	}
	if !wantOpen {
		c.failf(t, "%s: Open succeeded, want an error", what())
		return
	}

	for qi := range b.queries {
		q := &b.queries[qi]
		run, err := runQuery(t.Context(), r, q.q)
		if err != nil {
			c.failf(t, "%s, query %q: Iterate failed: %v", what(), q.name, err)
			continue
		}

		if !run.res.Complete() {
			c.incomplete[qi]++
			if wantComplete(qi) {
				c.failf(t, "%s, query %q: incomplete, want the complete baseline: %v", what(), q.name, run.res.Incomplete)
			}
			if problem := b.subsetProblem(qi, r, &run); problem != "" {
				c.failf(t, "%s, query %q: incomplete result: %s", what(), q.name, problem)
			}

			continue
		}

		c.complete[qi]++
		base := &b.runs[qi]
		if !sameItems(base.items, run.items) || !sameErr(base.res.FooterErr, run.res.FooterErr) {
			msg := fmt.Sprintf("%s, query %q: complete but different from the baseline", what(), q.name)
			c.failf(t, "%s", msg)
			assert.Equal(t, base.items, run.items, msg)
			assert.Equal(t, base.res.FooterErr, run.res.FooterErr, msg)

			continue
		}
		if !wantComplete(qi) {
			c.failf(t, "%s, query %q: complete and equal to the baseline, want incomplete: the mutation went undetected", what(), q.name)
		}
	}
}

// subsetProblem describes how the incomplete run of query qi over r departs from the baseline, or returns "".
// Every item must be a record the baseline yields for the query, byte for byte, in ascending seq order,
// with its payload exactly when the query asks for payloads.
func (b *sweepBaseline) subsetProblem(qi int, r *Reader, run *iterRun) string {
	q := &b.queries[qi].q
	nblocks := len(r.blocks)

	for k := range run.items {
		it := &run.items[k]
		seq := it.rec.Seq
		switch {
		case k > 0 && seq <= run.items[k-1].rec.Seq:
			return fmt.Sprintf("item %d: seq %d after seq %d", k, seq, run.items[k-1].rec.Seq)
		case !b.selected[qi][seq]:
			return fmt.Sprintf("item %d: seq %d is not in the baseline result", k, seq)
		case it.block < 0 || it.block >= nblocks:
			return fmt.Sprintf("item %d: block %d outside the %d blocks", k, it.block, nblocks)
		}

		ref := b.stored[seq]
		want := storedAs(ref.rec, q.Payloads)
		if !sameRecord(it.rec, want) || !bytes.Equal(it.extra, ref.extra) {
			return fmt.Sprintf("item %d: seq %d differs from the stored record:\n got %+v extra %x\nwant %+v extra %x", k, seq, it.rec, it.extra, want, ref.extra)
		}
	}

	return ""
}

// newSweepCounts returns counts for n queries.
func newSweepCounts(n int) *sweepCounts {
	return &sweepCounts{complete: make([]int, n), incomplete: make([]int, n)}
}

// failf reports a failed rule, in full for the first sweepFailureLimit failures.
func (c *sweepCounts) failf(t *testing.T, format string, args ...any) {
	t.Helper()

	c.failures++
	if c.failures <= sweepFailureLimit {
		t.Errorf(format, args...)
	}
}

// report logs the counts, and fails the test for the failures failf did not report in full.
func (c *sweepCounts) report(t *testing.T, b *sweepBaseline, what string) {
	t.Helper()

	if c.failures > sweepFailureLimit {
		t.Errorf("%s: %d more failures", what, c.failures-sweepFailureLimit)
	}
	t.Logf("%s over %d bytes (blocks start at %d): %d reads, %d Open errors", what, b.size, b.blocksStart, c.reads, c.openErrs)
	for qi := range b.queries {
		t.Logf("  query %q: %d complete, %d incomplete", b.queries[qi].name, c.complete[qi], c.incomplete[qi])
	}
}

// TestReaderCorruptionSweep inverts every byte of small packs, one at a time,
// and reads each mutated pack with every sweep query.
//
// Every byte of a finalized pack lies under a CRC that the read of its structure checks:
// the file header and pack metadata CRCs at Open, the envelope and body CRCs when a block is read,
// the footer CRC and the trailer CRC when the footer is settled.
// CRC-32 detects every single-byte change, so the outcome of each mutation is exact:
// a byte before the first block fails Open;
// a byte of a block the footer excludes from the query leaves the result complete and equal to the baseline;
// any other byte leaves the result incomplete, with a subset of the baseline's records.
// A complete result that differs from the baseline fails the test:
// it is the silent error the tracepack format specification §13 forbids.
func TestReaderCorruptionSweep(t *testing.T) {
	t.Parallel()

	for _, v := range sweepVariants() {
		t.Run(v.name, func(t *testing.T) {
			t.Parallel()

			file := sweepPack(t, v.codec, v.validate)
			b := newSweepBaseline(t, file, &v)
			c := newSweepCounts(len(b.queries))

			data := bytes.Clone(file)
			for off := range uint64(len(data)) {
				data[off] ^= 0xFF
				what := func() string { return fmt.Sprintf("byte %d (%s) inverted", off, b.region(off)) }
				b.read(t, data, what, off >= b.blocksStart, func(qi int) bool { return b.inPrunedBlock(qi, off) }, c)
				data[off] ^= 0xFF
			}

			c.report(t, b, "corruption sweep")
			assert.Equal(t, int(b.blocksStart), c.openErrs, "exactly the bytes before the first block fail Open")
			assert.Positive(t, slices.Max(c.incomplete), "some mutation leaves a read incomplete")
			assert.Positive(t, c.complete[sweepQueryPrunes], "a mutation of a pruned block leaves the pruning query complete")
		})
	}
}

// TestReaderTruncationSweep reads every prefix of small packs with every sweep query.
//
// A prefix shorter than the file header and the pack metadata fails Open.
// A longer strict prefix cannot end in a valid trailer, so the pack is not finalized
// and every read is incomplete, with a subset of the baseline's records;
// the whole file is the complete baseline.
func TestReaderTruncationSweep(t *testing.T) {
	t.Parallel()

	for _, v := range sweepVariants() {
		t.Run(v.name, func(t *testing.T) {
			t.Parallel()

			file := sweepPack(t, v.codec, v.validate)
			b := newSweepBaseline(t, file, &v)
			c := newSweepCounts(len(b.queries))

			for n := range uint64(len(file)) + 1 {
				what := func() string {
					if n == 0 {
						return "empty prefix"
					}

					return fmt.Sprintf("prefix of %d bytes (ends in the %s)", n, b.region(n-1))
				}
				whole := n == uint64(len(file))
				b.read(t, file[:n], what, n >= b.blocksStart, func(int) bool { return whole }, c)
			}

			c.report(t, b, "truncation sweep")
			assert.Equal(t, int(b.blocksStart), c.openErrs, "exactly the prefixes without the whole pack metadata fail Open")
			assert.Positive(t, slices.Max(c.incomplete), "some prefix is read incomplete")
			for qi := range b.queries {
				assert.Equal(t, 1, c.complete[qi], "only the whole file is complete, query %q", b.queries[qi].name)
			}
		})
	}
}
