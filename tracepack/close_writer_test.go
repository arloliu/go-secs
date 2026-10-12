package tracepack

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// Times of the close tests: the trigger 7 minutes into blockTestHour, in the flush interval [+5m, +10m).
var (
	closeTrigger     = time.Unix(0, blockTestHour+int64(7*time.Minute)).UTC()
	closePeriodStart = blockTestHour + int64(5*time.Minute)
	closeNow         = time.Unix(0, blockTestHour+int64(8*time.Minute)).UTC()
)

// closeCapture is the capture the close tests close.
var closeCapture = UUID{0xC1, 0x05, 0xE0}

// closeMeta returns the pack metadata of a close segment, before WriteClose sets its pack_role and seq_start:
// the capture's descriptor (descriptorMeta, its thinning only when thinned, its tolerance tol),
// with the consumer-owned values every segment of the capture carries,
// and, when masked, the policy, classifier and max_frame_len of a capture masked at write.
func closeMeta(tol uint64, masked, thinned bool) *PackMeta {
	m := descriptorMeta()
	if !thinned {
		m.Thinning = nil
	}
	m.ClockStepToleranceNs = new(tol)
	m.Writer = "consumer/1"
	m.PackRole, m.ScopeGeneration = PackRoleSegment, new(uint64(0))
	m.PeriodStart, m.PeriodEnd = closePeriodStart, closePeriodStart+int64(5*time.Minute)
	m.FlushIntervalNs = new(uint64(5 * time.Minute))
	if masked {
		m.RedactionPolicy = encryptedPolicy()
		m.Classifiers, m.MaxFrameLens = []string{"consumer-classifier/1"}, []uint64{1 << 20}
	}

	return m
}

// closeOptions returns the options of the close of closeCapture over m, with a fixed pack id and clock,
// the highest staged seq's timestamp highest.
func closeOptions(m *PackMeta, highest *int64) CloseOptions {
	return CloseOptions{Meta: m, CaptureID: closeCapture, Trigger: closeTrigger, HighestSeqTS: highest, PackID: seg3, Now: nowAt(closeNow)}
}

// writeCloseBytes writes the close of opts and returns its bytes, failing the test on error.
func writeCloseBytes(t *testing.T, opts CloseOptions) []byte {
	t.Helper()

	var buf bytes.Buffer
	id, err := WriteClose(&buf, opts)
	require.NoError(t, err)
	assert.Equal(t, opts.PackID, id)

	return buf.Bytes()
}

// closeEvent returns the event of the one record of the close pack file, checking the record's fields.
func closeEvent(t *testing.T, file []byte) *TransportEvent {
	t.Helper()

	run := iterate(t, mustOpen(t, file, ReaderOptions{}), Query{Payloads: true})
	require.Len(t, run.items, 1)
	rec := run.items[0].rec
	assert.Equal(t, format.MaxU64, rec.Seq, "the format maximum, not the uint64 maximum")
	assert.Equal(t, closeTrigger.UnixNano(), rec.TSUTCNs, "the trigger time")
	assert.Equal(t, [4]any{KindTransportEvent, DirLocal, uint32(0), false}, [4]any{rec.Kind, rec.Dir, rec.Epoch, rec.MonoPresent})
	assert.Equal(t, [2]any{FidelityNotApplicable, DecodeStatusNotApplicable}, [2]any{rec.Fidelity, rec.DecodeStatus})
	assert.Equal(t, QualityCaptureBoundary|QualityCorrelationIncomplete, rec.Quality)
	ev, err := UnmarshalTransportEvent(rec.Payload)
	require.NoError(t, err)
	assert.Equal(t, EventCaptureBoundary, ev.Event)
	require.NotNil(t, ev.BoundaryKind)
	assert.Equal(t, BoundaryKindStopUnclean, *ev.BoundaryKind)

	return ev
}

// The close of a capture its producer left open is one stop-unclean record at seq 2^63-1, alone in a segment
// carrying the capture's descriptor and consumer-owned values, its format_minor following its features
// (the tracepack storage specification §4, Closing a capture left open).
func TestWriteClose(t *testing.T) {
	t.Parallel()

	highest := blockTestHour + int64(2*time.Minute)
	for _, tt := range []struct {
		name            string
		masked, thinned bool
		minor           uint16
	}{
		{"a plain capture", false, false, 0},
		{"a capture masked at write", true, false, 1},
		{"a thinned capture", false, true, 1},
		{"a masked and thinned capture", true, true, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := closeMeta(1000, tt.masked, tt.thinned)
			given := *m
			file := writeCloseBytes(t, closeOptions(m, &highest))
			assert.Equal(t, given, *m, "WriteClose does not change the caller's metadata")

			r := mustOpen(t, file, ReaderOptions{})
			h := r.Header()
			assert.Equal(t, tt.minor, h.FormatMinor)
			assert.Equal(t, closeCapture, h.CaptureID)
			assert.Zero(t, h.Flags, "no redaction-present: the close is never masked")
			want := closeMeta(1000, tt.masked, tt.thinned)
			want.SeqStart = format.MaxU64
			assert.Equal(t, want, h.Meta)

			ev := closeEvent(t, file)
			assert.Equal(t, [2]*int64{new(highest - 1000), new(closeTrigger.UnixNano() + 1000)}, [2]*int64{ev.GapStart, ev.GapEnd})

			var export bytes.Buffer
			_, err := ExportJSONL(t.Context(), r, &export)
			require.NoError(t, err)
			schema := map[uint16]string{0: "tracepack-jsonl/1", 1: "tracepack-jsonl/2"}[tt.minor]
			assert.True(t, strings.HasPrefix(export.String(), fmt.Sprintf(`{"schema":%q,`, schema)), "export schema %s", schema)
		})
	}

	t.Run("the bounds", func(t *testing.T) {
		t.Parallel()

		triggerNs := closeTrigger.UnixNano()
		// Staged records whose timestamps move backward: the highest seq's timestamp, not the latest, gives gap_start.
		staged := []struct {
			seq uint64
			ts  int64
		}{{1, triggerNs - 500}, {2, triggerNs - 100}, {3, triggerNs - 900}}
		highestSeqTS := staged[len(staged)-1].ts
		tests := []struct {
			name       string
			tol        uint64
			highest    *int64
			start, end *int64
		}{
			{"the highest seq's timestamp", 1000, new(highestSeqTS), new(triggerNs - 1900), new(triggerNs + 1000)},
			{"no staged record", 1000, nil, nil, new(triggerNs + 1000)},
			{"a tolerance of zero", 0, new(highestSeqTS), new(highestSeqTS), new(triggerNs)},
			{"an inverted gap", 0, new(triggerNs + 50), new(triggerNs + 50), new(triggerNs)},
			{"gap_start at the i64 minimum", 5, new(int64(math.MinInt64 + 5)), new(int64(math.MinInt64)), new(triggerNs + 5)},
			{"gap_start below the i64 range", 6, new(int64(math.MinInt64 + 5)), nil, new(triggerNs + 6)},
			{"gap_end past the i64 range", math.MaxInt64, new(int64(-5)), nil, nil},
			{"gap_end at the i64 maximum", uint64(math.MaxInt64 - triggerNs), new(int64(0)), new(triggerNs - math.MaxInt64), new(int64(math.MaxInt64))},
		}
		for _, tt := range tests {
			ev := closeEvent(t, writeCloseBytes(t, closeOptions(closeMeta(tt.tol, false, false), tt.highest)))
			assert.Equal(t, tt.start, ev.GapStart, "%s: gap_start", tt.name)
			assert.Equal(t, tt.end, ev.GapEnd, "%s: gap_end", tt.name)
		}
	})

	t.Run("a retried close", func(t *testing.T) {
		t.Parallel()

		opts := closeOptions(closeMeta(1000, true, true), new(blockTestHour))
		first, again := writeCloseBytes(t, opts), writeCloseBytes(t, opts)
		require.Equal(t, first, again, "a retry writes the same bytes")

		// A close written again in another pack holds the same record, read once and without a conflict.
		opts.PackID = seg2
		other := writeCloseBytes(t, opts)
		readers := []*Reader{mustOpen(t, first, ReaderOptions{}), mustOpen(t, other, ReaderOptions{})}
		items, res, err := collectMergeIterate(t.Context(), readers, Query{}, MergeIterateOptions{Order: OrderCapture})
		require.NoError(t, err)
		require.Len(t, items, 1, "read once")
		assert.Equal(t, format.MaxU64, items[0].rec.Seq)
		assert.False(t, items[0].conflict)
		assert.Empty(t, res.Conflicts)
	})

	t.Run("refused options", func(t *testing.T) {
		t.Parallel()

		edit := func(fn func(o *CloseOptions)) CloseOptions {
			o := closeOptions(closeMeta(1000, false, false), nil)
			fn(&o)
			return o
		}
		for name, opts := range map[string]CloseOptions{
			"no metadata":                               edit(func(o *CloseOptions) { o.Meta = nil }),
			"the nil capture":                           edit(func(o *CloseOptions) { o.CaptureID = UUID{} }),
			"no tolerance":                              edit(func(o *CloseOptions) { o.Meta.ClockStepToleranceNs = nil }),
			"a source-log capture":                      edit(func(o *CloseOptions) { o.Meta.TimeSource = TimeSourceSourceLog }),
			"a trigger outside i64":                     edit(func(o *CloseOptions) { o.Trigger = time.Time{} }),
			"no flush_interval_ns":                      edit(func(o *CloseOptions) { o.Meta.FlushIntervalNs = nil }),
			"a trigger of an hour the period is not in": edit(func(o *CloseOptions) { o.Trigger = o.Trigger.Add(time.Hour) }),
			"a tolerance outside the i64 range": edit(func(o *CloseOptions) {
				o.Meta.ClockStepToleranceNs = new(uint64(math.MaxInt64 + 1))
			}),
		} {
			var buf bytes.Buffer
			_, err := WriteClose(&buf, opts)
			require.Error(t, err, name)
			assert.Zero(t, buf.Len(), "%s: nothing written", name)
		}
	})
}

// closeSegments returns the packs of closeCapture the close read tests read:
// a segment of seqs 0 to 2 at the start of blockTestHour, the close, whose gap is inverted when inverted,
// and a late segment holding a record at seq 2^63-2 inside the gap.
func closeSegments(t *testing.T, inverted bool) (segment, closePack, late []byte) {
	t.Helper()

	write := func(seqStart uint64, recs ...Record) []byte {
		m := closeMeta(1000, false, false)
		m.SeqStart = seqStart
		m.PeriodStart, m.PeriodEnd = blockTestHour, blockTestHour+hourNs
		var buf bytes.Buffer
		w, err := NewWriter(&buf, WriterOptions{Meta: m, CaptureID: closeCapture, Facts: PackFacts{DurableRecorder: true}})
		require.NoError(t, err)
		for i := range recs {
			require.NoError(t, w.Append(&recs[i]))
		}
		_, err = w.Close()
		require.NoError(t, err)

		return buf.Bytes()
	}
	data := func(seq uint64, ts int64) Record {
		r := frameRecord(seq, hsmsFrame(1, 1, nil))
		r.TSUTCNs, r.DecodeStatus = ts, DecodeStatusNotAttempted

		return r
	}

	highest := blockTestHour + int64(2*time.Minute)
	if inverted {
		highest = closeTrigger.UnixNano() + int64(time.Minute)
	}
	segment = write(0, data(0, blockTestHour), data(1, blockTestHour+int64(time.Minute)), data(2, highest))
	closePack = writeCloseBytes(t, closeOptions(closeMeta(1000, false, false), &highest))
	late = write(format.MaxU64-1, data(format.MaxU64-1, closeTrigger.UnixNano()-int64(time.Minute)))

	return segment, closePack, late
}

// A close-only pack counts one record and ends no epoch;
// it is read as a crash gap, as a historical stop-unclean at an ordinary seq is,
// through an accepted footer and with its footer rejected, and a record of the capture written after it is read
// (the tracepack storage specification §4, Closing a capture left open; the tracepack semantics specification §7.4).
func TestCloseReadSemantics(t *testing.T) {
	t.Parallel()

	_, closePack, _ := closeSegments(t, false)
	r := mustOpen(t, closePack, ReaderOptions{})
	st, ok := r.Stats()
	require.True(t, ok)
	assert.Equal(t, uint64(1), st.RecordCount)
	assert.Equal(t, uint64(1), r.Header().Trailer.RecordCount)
	assert.Equal(t, []SeqRange{{First: format.MaxU64, Last: format.MaxU64}}, st.SeqRanges)
	require.Len(t, st.Epochs, 1)
	assert.Equal(t, uint32(0), st.Epochs[0].Epoch)
	assert.Nil(t, st.Epochs[0].CloseSeq, "the close ends no epoch")
	require.Len(t, st.Boundaries, 1)
	assert.Equal(t, BoundaryKindStopUnclean, st.Boundaries[0].Kind)

	// A query whose time range lies after the gap of an ordinary close, and a query without one.
	afterGap := Filter{TimeFrom: new(closeTrigger.UnixNano() + int64(time.Minute)), TimeTo: new(blockTestHour + hourNs)}
	for _, inverted := range []bool{false, true} {
		segment, closePack, late := closeSegments(t, inverted)
		for _, footer := range []string{"accepted", "rejected"} {
			name := fmt.Sprintf("inverted %t, footer %s", inverted, footer)
			if footer == "rejected" {
				closePack = invalidFooterFile(t, closePack)
			}
			readers := []*Reader{
				mustOpen(t, segment, ReaderOptions{}), mustOpen(t, closePack, ReaderOptions{}), mustOpen(t, late, ReaderOptions{}),
			}
			items, res, err := collectMergeIterate(t.Context(), readers, Query{}, MergeIterateOptions{Order: OrderCapture})
			require.NoError(t, err, name)
			seqs := make([]uint64, 0, len(items))
			for _, it := range items {
				seqs = append(seqs, it.rec.Seq)
			}
			assert.Equal(t, []uint64{0, 1, 2, format.MaxU64 - 1, format.MaxU64}, seqs, "%s: the late record is read", name)
			require.Len(t, res.Incomplete, 1, name)
			assert.Equal(t, ReasonCrashGap, res.Incomplete[0].Reason, name)

			_, res, err = collectMergeIterate(t.Context(), readers, Query{Filter: afterGap}, MergeIterateOptions{Order: OrderCapture})
			require.NoError(t, err, name)
			if inverted {
				require.Len(t, res.Incomplete, 1, "%s: an inverted gap meets every range", name)
				assert.Equal(t, ReasonCrashGap, res.Incomplete[0].Reason, name)
			} else {
				assert.Empty(t, res.Incomplete, "%s: the range misses the gap", name)
			}

			run := iterate(t, mustOpen(t, closePack, ReaderOptions{}), Query{})
			assert.Equal(t, []uint64{format.MaxU64}, run.seqs(), name)
			assert.Len(t, run.res.Incomplete, 1, "%s: Iterate reports the crash gap", name)
		}
	}

	t.Run("a historical stop-unclean at an ordinary seq", func(t *testing.T) {
		t.Parallel()

		p := uncleanPack(t, seg0, captureLow)
		for _, file := range [][]byte{p.file, invalidFooterFile(t, p.file)} {
			run := iterate(t, mustOpen(t, file, ReaderOptions{}), Query{Filter: pastBoundaryBlock()})
			assert.Equal(t, []Defect{crashGapDefect(0, captureLow, crashGapStart, crashGapEnd)}, withoutErrs(t, run.res.Incomplete))
		}
	})
}

// commitWriterPack writes, through a Writer with fixed ids and clock, a segment of one record to out,
// syncing with syncer, and returns the error of the block's flush and of Close.
func commitWriterPack(t *testing.T, out io.Writer, syncer Syncer) (flushErr, closeErr error) {
	t.Helper()

	opts := WriterOptions{Meta: repairTestMeta(), PackID: seg1, CaptureID: closeCapture, Now: nowAt(closeNow)}
	if syncer != nil {
		opts.Sync = syncer
	}
	w, err := NewWriter(out, opts)
	require.NoError(t, err)
	r := frameRecord(0, hsmsFrame(1, 1, nil))
	require.NoError(t, w.Append(&r))
	flushErr = w.Flush()
	_, closeErr = w.Close()
	if closeErr != nil {
		_, again := w.Close()
		require.ErrorIs(t, again, ErrWriterFailed, "a failed Writer stays failed")
	}

	return flushErr, closeErr
}

// A Writer and WriteClose separate a failure before the trailer is whole, which leaves the pack unfinalized,
// from a failure of the sync after the trailer, which may leave a finalized pack (the tracepack format specification §12);
// WriteClose returns the pack id with the error, and a retry writes the bytes of the first successful pack.
func TestWriterCommitBoundary(t *testing.T) {
	t.Parallel()

	closeGolden := writeCloseBytes(t, closeOptions(closeMeta(1000, true, true), new(blockTestHour)))
	var writerOut bytes.Buffer
	flushErr, closeErr := commitWriterPack(t, &writerOut, nil)
	require.NoError(t, flushErr)
	require.NoError(t, closeErr)
	writerGolden := writerOut.Bytes()

	tests := []struct {
		name string
		// cut is the number of trailer bytes the output takes before it fails, strictly inside the trailer; -1 never fails.
		cut int
		// failSync is the sync that fails, counted from 1: the block's is 1, the trailer's 2; 0 none.
		failSync  int
		finalized bool
	}{
		{"the block's sync", -1, 1, false},
		{"one byte into the trailer", 1, 0, false},
		{"partway through the trailer", 37, 0, false},
		{"one byte short of the trailer's end", format.TrailerLen - 1, 0, false},
		{"the trailer's sync", -1, 2, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			for _, branch := range []struct {
				name   string
				golden []byte
				write  func(out *failingOutput, syncer *failingSyncer) error
			}{
				{"WriteClose", closeGolden, func(out *failingOutput, syncer *failingSyncer) error {
					opts := closeOptions(closeMeta(1000, true, true), new(blockTestHour))
					opts.Sync = syncer
					id, err := WriteClose(out, opts)
					assert.Equal(t, seg3, id, "the pack id with the error")

					return err
				}},
				{"Writer", writerGolden, func(out *failingOutput, syncer *failingSyncer) error {
					flushErr, closeErr := commitWriterPack(t, out, syncer)
					if syncer.fail == 1 {
						require.ErrorIs(t, flushErr, errTestOutput, "the block's sync")
					} else {
						require.NoError(t, flushErr)
					}

					return closeErr
				}},
			} {
				trailerStart := len(branch.golden) - format.TrailerLen
				out, syncer := &failingOutput{limit: -1}, &failingSyncer{fail: tt.failSync}
				if tt.cut >= 0 {
					out.limit = trailerStart + tt.cut
				}

				err := branch.write(out, syncer)
				require.ErrorIs(t, err, errTestOutput, branch.name)
				got := out.buf.Bytes()
				if tt.cut >= 0 {
					require.Len(t, got, trailerStart+tt.cut, "%s: the output stops inside the trailer", branch.name)
					assert.Greater(t, len(got), trailerStart, branch.name)
					assert.Less(t, len(got), len(branch.golden), branch.name)
					assert.Equal(t, branch.golden[:len(got)], got, "%s: every byte before the cut as written", branch.name)
				}
				outcome := mustVerify(t, got).Outcome
				if tt.finalized {
					assert.Equal(t, OutcomeFinalizedConsistent, outcome, "%s: finalized, though not durable", branch.name)
					assert.Equal(t, branch.golden, got, branch.name)
				} else {
					assert.Equal(t, OutcomeUnfinalized, outcome, branch.name)
				}

				retry := &failingOutput{limit: -1}
				require.NoError(t, branch.write(retry, &failingSyncer{}), "%s: the retry", branch.name)
				assert.Equal(t, branch.golden, retry.buf.Bytes(), "%s: the retry writes the first successful pack's bytes", branch.name)
			}
		})
	}
}
