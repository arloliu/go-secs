package tracepack

import (
	"bytes"
	"crypto/rand"
	"math"
	"runtime/debug"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// segTestOrigin is the CaptureOrigin of most segment writer tests:
// 10 s into blockTestHour, without a monotonic reading, so its first period is [blockTestHour, blockTestHour+5m).
var segTestOrigin = time.Unix(0, blockTestHour+10*int64(time.Second)).UTC()

// segAt returns the time d after segTestOrigin, without a monotonic reading.
func segAt(d time.Duration) time.Time {
	return segTestOrigin.Add(d)
}

// segOptions returns the options of a capture-clock raw-stream capture at segTestOrigin over sink,
// whose Now is one minute after the origin.
func segOptions(sink SegmentSink) SegmentWriterOptions {
	d := validDescriptor()
	d.CaptureOrigin = segTestOrigin

	return SegmentWriterOptions{Capture: d, Sink: sink, Now: func() time.Time { return segAt(time.Minute) }}
}

// newSeg returns a SegmentWriter over a new memSink, with segOptions changed by mutate when it is not nil.
func newSeg(t testing.TB, mutate func(*SegmentWriterOptions)) (*SegmentWriter, *memSink) {
	t.Helper()

	sink := newMemSink()
	opts := segOptions(sink)
	if mutate != nil {
		mutate(&opts)
	}
	w, err := NewSegmentWriter(t.Context(), opts)
	require.NoError(t, err)

	return w, sink
}

// monotonicAt returns a time whose wall clock is wall's and which carries a monotonic reading.
func monotonicAt(wall time.Time) time.Time {
	now := time.Now()

	return now.Add(wall.Sub(now.Round(0)))
}

// nowAt returns a Now function that always returns at.
func nowAt(at time.Time) func() time.Time {
	return func() time.Time { return at }
}

// segFrame appends blockTestFrame at segAt(d) on epoch 1 and fails the test on error.
func segFrame(t testing.TB, w *SegmentWriter, d time.Duration) {
	t.Helper()
	require.NoError(t, w.AppendFrame(t.Context(), segAt(d), DirHostToEquipment, 1, blockTestFrame))
}

// segMonoRecord returns a data record over blockTestFrame, as Append takes it, on epoch 1:
// ts_utc_ns wall after segTestOrigin and mono_ns mono.
func segMonoRecord(wall, mono time.Duration) Record {
	r := testDataRecord(0, segTestOrigin.UnixNano()+int64(wall), 1)
	r.MonoNs = int64(mono)
	r.Fidelity, r.DecodeStatus = FidelityWireExact, DecodeStatusNotAttempted

	return r
}

// eventOfRecord decodes the transport event r carries.
func eventOfRecord(t testing.TB, r Record) *TransportEvent {
	t.Helper()
	require.Equal(t, KindTransportEvent, r.Kind)
	ev, err := UnmarshalTransportEvent(r.Payload)
	require.NoError(t, err)

	return ev
}

// boundaryKindOf returns the boundary kind r carries, failing the test when r is no capture boundary.
func boundaryKindOf(t testing.TB, r Record) BoundaryKind {
	t.Helper()
	ev := eventOfRecord(t, r)
	require.Equal(t, EventCaptureBoundary, ev.Event)
	require.NotNil(t, ev.BoundaryKind)

	return *ev.BoundaryKind
}

// isClockStepRecord reports whether r is a clock-step event.
func isClockStepRecord(t testing.TB, r Record) bool {
	t.Helper()
	if r.Kind != KindTransportEvent {
		return false
	}

	return eventOfRecord(t, r).Event == EventClockStep
}

// segSeqs returns the seqs of each published segment's records, in commit order.
func segSeqs(recs [][]Record) [][]uint64 {
	out := make([][]uint64, 0, len(recs))
	for _, seg := range recs {
		seqs := make([]uint64, 0, len(seg))
		for _, r := range seg {
			seqs = append(seqs, r.Seq)
		}
		out = append(out, seqs)
	}

	return out
}

// checkCapture checks what every capture written without a failure holds:
// each published segment has records, all of its period's hour, its header names w's capture,
// its seq_start is its first seq and its SegmentInfo matches it;
// the seqs run from 0 without a gap across the segments; every segment created was committed, none aborted;
// and every segment verifies finalized-consistent, without a writer defect.
// It returns the records of each segment.
func checkCapture(t testing.TB, w *SegmentWriter, sink *memSink) [][]Record {
	t.Helper()
	sink.noMisuse(t)
	require.Equal(t, len(sink.infos), len(sink.keys), "every segment created was published")
	assert.Equal(t, sink.calls[sinkCreate], sink.calls[sinkCommit], "every segment created was committed")
	assert.Zero(t, sink.aborts(), "no segment aborted")
	for i, k := range sink.keys {
		data := sink.published[k]
		rep, err := Verify(t.Context(), bytes.NewReader(data), int64(len(data)), VerifyOptions{})
		require.NoError(t, err, "segment %d", i)
		assert.Equal(t, OutcomeFinalizedConsistent, rep.Outcome, "segment %d", i)
		assert.Empty(t, rep.WriterDefects, "segment %d", i)
	}

	recs := sink.records(t)
	var next uint64
	for i, r := range sink.segments(t) {
		h := r.Header()
		m := h.Meta
		require.NotEmpty(t, recs[i], "segment %d holds records", i)
		assert.Equal(t, w.CaptureID(), h.CaptureID, "segment %d capture_id", i)
		assert.True(t, h.Finalized, "segment %d finalized", i)
		assert.Equal(t, recs[i][0].Seq, m.SeqStart, "segment %d seq_start", i)
		info := sink.infos[i]
		assert.Equal(t, SegmentInfo{
			ToolID: m.ToolID, CaptureID: h.CaptureID, PackID: h.PackID, SeqFirst: m.SeqStart, Hour: hourOf(m.PeriodStart),
			PeriodStart: time.Unix(0, m.PeriodStart).UTC(), PeriodEnd: time.Unix(0, m.PeriodEnd).UTC(),
		}, info, "segment %d info", i)
		for _, rec := range recs[i] {
			assert.Equal(t, hourOf(m.PeriodStart), hourOf(rec.TSUTCNs), "segment %d record %d in its period's hour", i, rec.Seq)
			assert.Equal(t, next, rec.Seq, "segment %d: seqs continue across segments", i)
			next = rec.Seq + 1
		}
	}

	return recs
}

func TestSegmentWriterWritesCapture(t *testing.T) {
	t.Parallel()

	w, sink := newSeg(t, nil)
	id := w.CaptureID()
	require.False(t, id.IsZero(), "CaptureID is set from construction on")
	segFrame(t, w, time.Second)
	require.NoError(t, w.AppendFrame(t.Context(), segAt(2*time.Second), DirEquipmentToHost, 0, controlTestFrame))
	require.NoError(t, w.Close(t.Context()))

	recs := checkCapture(t, w, sink)
	require.Len(t, recs, 1)
	got := recs[0]
	require.Len(t, got, 4)

	for i, kind := range map[int]BoundaryKind{0: BoundaryKindStart, 3: BoundaryKindStop} {
		r := got[i]
		assert.Equal(t, kind, boundaryKindOf(t, r))
		ev := eventOfRecord(t, r)
		assert.Nil(t, ev.BoundarySeqFirst, "%s has no seq range", kind)
		assert.Nil(t, ev.BoundarySeqLast, "%s has no seq range", kind)
		assert.Equal(t, uint32(0), r.Epoch)
		assert.Equal(t, DirLocal, r.Dir)
		assert.Equal(t, FidelityNotApplicable, r.Fidelity)
		assert.Equal(t, DecodeStatusNotApplicable, r.DecodeStatus)
		assert.Equal(t, QualityCaptureBoundary|QualityCorrelationIncomplete, r.Quality)
		assert.False(t, r.MonoPresent, "the origin carries no monotonic reading")
	}
	assert.Equal(t, segTestOrigin.UnixNano(), got[0].TSUTCNs, "start at CaptureOrigin")
	assert.Equal(t, segAt(time.Minute).UnixNano(), got[3].TSUTCNs, "stop at Now()")

	data, control := got[1], got[2]
	assert.Equal(t, KindData, data.Kind)
	assert.Equal(t, blockTestFrame, data.Payload)
	assert.Equal(t, capturedFields(len(blockTestFrame)), data.FieldValidity)
	assert.Equal(t, Quality(0), data.Quality, "epoch 1 needs no derived bit")
	assert.Equal(t, KindControl, control.Kind)
	assert.Equal(t, QualityCorrelationIncomplete, control.Quality, "epoch 0 is correlation-incomplete")
	assert.Equal(t, DirEquipmentToHost, control.Dir)
	for _, r := range got[1:3] {
		assert.Equal(t, FidelityWireExact, r.Fidelity)
		assert.Equal(t, DecodeStatusNotAttempted, r.DecodeStatus)
		assert.Equal(t, uint32(0), r.TrailingBytes)
	}

	m := sink.segments(t)[0].Header().Meta
	assert.Equal(t, PackRoleSegment, m.PackRole)
	assert.Equal(t, uint8(0), m.CompactionLevel)
	assert.Equal(t, new(uint64(0)), m.ScopeGeneration)
	assert.Equal(t, writerTag, m.Writer)
	assert.Nil(t, m.FlushIntervalNs, "no flush_interval_ns without a durable spool")
	assert.Empty(t, m.Classifiers)
	assert.Empty(t, m.MaxFrameLens)
	assert.Equal(t, [2]int64{blockTestHour, blockTestHour + int64(DefaultFlushInterval)}, [2]int64{m.PeriodStart, m.PeriodEnd})
	assert.Equal(t, new(segTestOrigin.UnixNano()), m.CaptureOriginUTCNs)
	assert.Equal(t, new(int64(0)), m.CaptureOriginMonoNs)
	assert.Equal(t, new(uint64(DefaultClockStepTolerance)), m.ClockStepToleranceNs)
	assert.Positive(t, sink.calls[sinkSync], "blocks and the trailer are synced")
}

func TestSegmentWriterCaptureIDSeedsSuccessor(t *testing.T) {
	t.Parallel()

	first, sink := newSeg(t, nil)
	require.NoError(t, first.Close(t.Context()))
	checkCapture(t, first, sink)

	next, nextSink := newSeg(t, func(o *SegmentWriterOptions) { o.Capture.PreviousCaptureID = new(first.CaptureID()) })
	require.NoError(t, next.Close(t.Context()))
	checkCapture(t, next, nextSink)
	assert.NotEqual(t, first.CaptureID(), next.CaptureID())
	for _, r := range nextSink.segments(t) {
		assert.Equal(t, new(first.CaptureID()), r.Header().Meta.PreviousCaptureID)
	}
}

func TestSegmentWriterRolls(t *testing.T) {
	t.Parallel()

	period := int64(DefaultFlushInterval)
	tests := []struct {
		name string
		opts func(*SegmentWriterOptions)
		// run appends after the start boundary; the capture is then closed.
		run  func(t *testing.T, w *SegmentWriter)
		want [][]uint64
	}{
		{
			name: "a record of the next hour",
			run:  func(t *testing.T, w *SegmentWriter) { segFrame(t, w, time.Second); segFrame(t, w, time.Hour) },
			opts: func(o *SegmentWriterOptions) { o.Now = nowAt(segAt(time.Hour + time.Second)) },
			want: [][]uint64{{0, 1}, {2, 3}},
		},
		{
			name: "a record of the hour before",
			run:  func(t *testing.T, w *SegmentWriter) { segFrame(t, w, -time.Hour) },
			want: [][]uint64{{0}, {1}, {2}},
		},
		{
			name: "a record of the same hour at the period's end",
			run: func(t *testing.T, w *SegmentWriter) {
				segFrame(t, w, time.Duration(period)-10*time.Second-1)
				segFrame(t, w, time.Duration(period)-10*time.Second)
			},
			opts: func(o *SegmentWriterOptions) { o.Now = nowAt(segAt(DefaultFlushInterval)) },
			want: [][]uint64{{0, 1}, {2, 3}},
		},
		{
			name: "a record of the same hour before the period's start stays",
			opts: func(o *SegmentWriterOptions) {
				o.Capture.CaptureOrigin = segAt(20 * time.Minute)
				o.Now = nowAt(segAt(20*time.Minute + time.Second))
			},
			run:  func(t *testing.T, w *SegmentWriter) { segFrame(t, w, time.Minute) },
			want: [][]uint64{{0, 1, 2}},
		},
		{
			name: "Rotate",
			run: func(t *testing.T, w *SegmentWriter) {
				segFrame(t, w, time.Second)
				require.NoError(t, w.Rotate(t.Context()))
				require.NoError(t, w.Rotate(t.Context()), "a Rotate without an open segment does nothing")
				segFrame(t, w, 2*time.Second)
			},
			want: [][]uint64{{0, 1}, {2, 3}},
		},
		{
			name: "Tick before and at the period's end",
			run: func(t *testing.T, w *SegmentWriter) {
				sink, ok := w.sink.(*memSink)
				require.True(t, ok)
				end := time.Unix(0, blockTestHour+period)
				require.NoError(t, w.Tick(t.Context(), end.Add(-1)))
				require.Empty(t, sink.keys, "a Tick before the period's end commits nothing")
				segFrame(t, w, time.Second)
				require.NoError(t, w.Tick(t.Context(), end))
				require.Len(t, sink.keys, 1, "a Tick at the period's end commits the segment")
				require.NoError(t, w.Tick(t.Context(), end.Add(time.Hour)), "a Tick without an open segment does nothing")
			},
			want: [][]uint64{{0, 1}, {2}},
		},
		{
			name: "every caller record past the size limit, never the stop",
			opts: func(o *SegmentWriterOptions) { o.MaxSegmentBytes, o.BlockThreshold = 1, 1 },
			run:  func(t *testing.T, w *SegmentWriter) { segFrame(t, w, time.Second); segFrame(t, w, 2*time.Second) },
			want: [][]uint64{{0}, {1}, {2, 3}},
		},
		{
			name: "a stop of a new hour alone",
			opts: func(o *SegmentWriterOptions) { o.Now = nowAt(segAt(time.Hour)) },
			run:  func(t *testing.T, w *SegmentWriter) { segFrame(t, w, time.Second) },
			want: [][]uint64{{0, 1}, {2}},
		},
		{
			name: "a start alone when the first frame comes an hour later",
			opts: func(o *SegmentWriterOptions) { o.Now = nowAt(segAt(time.Hour + time.Second)) },
			run:  func(t *testing.T, w *SegmentWriter) { segFrame(t, w, time.Hour) },
			want: [][]uint64{{0}, {1, 2}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			w, sink := newSeg(t, tt.opts)
			tt.run(t, w)
			require.NoError(t, w.Close(t.Context()))

			recs := checkCapture(t, w, sink)
			assert.Equal(t, tt.want, segSeqs(recs))
			last := recs[len(recs)-1]
			assert.Equal(t, BoundaryKindStart, boundaryKindOf(t, recs[0][0]))
			assert.Equal(t, BoundaryKindStop, boundaryKindOf(t, last[len(last)-1]))
		})
	}
}

func TestSegmentWriterClockSteps(t *testing.T) {
	t.Parallel()

	// Each case appends records with mono_ns through Append; the anchor starts at (segTestOrigin, 0) and the tolerance is 1 s.
	tests := []struct {
		name    string
		opts    func(*SegmentWriterOptions)
		records []Record
		// between, when set, runs after the first record.
		between func(t *testing.T, w *SegmentWriter)
		want    [][]uint64
		// steps holds the seq of every clock-step record.
		steps []uint64
	}{
		{
			name:    "a forward step into the next hour rolls, then steps in the new segment",
			records: []Record{segMonoRecord(time.Hour, 20*time.Second), segMonoRecord(time.Hour+time.Second, 21*time.Second)},
			opts:    func(o *SegmentWriterOptions) { o.Now = nowAt(segAt(time.Hour + 2*time.Second)) },
			want:    [][]uint64{{0}, {1, 2, 3, 4}},
			steps:   []uint64{1},
		},
		{
			name:    "a backward step into the hour before",
			records: []Record{segMonoRecord(-30*time.Minute, 20*time.Second)},
			want:    [][]uint64{{0}, {1, 2}, {3}},
			steps:   []uint64{1},
		},
		{
			name:    "a forward step within the hour",
			records: []Record{segMonoRecord(time.Minute, 55*time.Second)},
			want:    [][]uint64{{0, 1, 2, 3}},
			steps:   []uint64{1},
		},
		{
			name: "a backward step before the period's start stays in the segment",
			opts: func(o *SegmentWriterOptions) {
				o.Capture.CaptureOrigin = segAt(20 * time.Minute)
				o.Now = nowAt(segAt(20*time.Minute + time.Minute))
			},
			// The anchor is (origin + 20 min, 0): a record 10 min after segTestOrigin at mono 10 s steps back.
			records: []Record{segMonoRecord(10*time.Minute, 10*time.Second)},
			want:    [][]uint64{{0, 1, 2, 3}},
			steps:   []uint64{1},
		},
		{
			name:    "the anchor carries over a Rotate",
			records: []Record{segMonoRecord(time.Minute, 55*time.Second), segMonoRecord(time.Minute+time.Second, 56*time.Second)},
			between: func(t *testing.T, w *SegmentWriter) { require.NoError(t, w.Rotate(t.Context())) },
			want:    [][]uint64{{0, 1, 2}, {3, 4}},
			steps:   []uint64{1},
		},
		{
			name: "a caller's clock step, then Rotate",
			records: []Record{
				clockStepRecord(t, time.Minute, 50*time.Second),
				segMonoRecord(time.Minute+time.Second, 51*time.Second),
			},
			between: func(t *testing.T, w *SegmentWriter) { require.NoError(t, w.Rotate(t.Context())) },
			want:    [][]uint64{{0, 1}, {2, 3}},
			steps:   []uint64{1},
		},
		{
			name: "a caller's clock step, then Tick",
			records: []Record{
				clockStepRecord(t, time.Minute, 50*time.Second),
				segMonoRecord(5*time.Minute, 4*time.Minute+50*time.Second),
			},
			between: func(t *testing.T, w *SegmentWriter) {
				require.NoError(t, w.Tick(t.Context(), time.Unix(0, blockTestHour+int64(DefaultFlushInterval))))
			},
			opts:  func(o *SegmentWriterOptions) { o.Now = nowAt(segAt(5*time.Minute + time.Second)) },
			want:  [][]uint64{{0, 1}, {2, 3}},
			steps: []uint64{1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			w, sink := newSeg(t, tt.opts)
			for i := range tt.records {
				require.NoError(t, w.Append(t.Context(), &tt.records[i]))
				if i == 0 && tt.between != nil {
					tt.between(t, w)
				}
			}
			require.NoError(t, w.Close(t.Context()))

			recs := checkCapture(t, w, sink)
			assert.Equal(t, tt.want, segSeqs(recs))
			var steps []uint64
			for _, seg := range recs {
				for i, r := range seg {
					if !isClockStepRecord(t, r) {
						continue
					}
					steps = append(steps, r.Seq)
					if i+1 == len(seg) {
						continue // a caller's own step, which its segment ends with
					}
					next := seg[i+1]
					assert.Equal(t, [2]int64{next.TSUTCNs, next.MonoNs}, [2]int64{r.TSUTCNs, r.MonoNs}, "an inserted step carries its record's times")
				}
			}
			assert.Equal(t, tt.steps, steps)
		})
	}
}

// clockStepRecord returns a caller's clock-step event record at wall after segTestOrigin with mono_ns mono,
// whose clock_step_ns is the drift it declares against the origin.
func clockStepRecord(t testing.TB, wall, mono time.Duration) Record {
	t.Helper()

	return Record{
		TSUTCNs: segTestOrigin.UnixNano() + int64(wall), MonoNs: int64(mono), MonoPresent: true, Epoch: 1,
		Kind: KindTransportEvent, Dir: DirLocal, Fidelity: FidelityNotApplicable, DecodeStatus: DecodeStatusNotApplicable,
		Payload: mustMarshalEvent(t, &TransportEvent{Event: EventClockStep, ClockStepNs: new(int64(wall - mono))}),
	}
}

// mustMarshalEvent encodes ev and fails the test on error.
func mustMarshalEvent(t testing.TB, ev *TransportEvent) []byte {
	t.Helper()
	p, err := ev.MarshalBinary()
	require.NoError(t, err)

	return p
}

// mustMarshalNote returns the payload of a note annotation, failing the test on error.
func mustMarshalNote(t testing.TB) []byte {
	t.Helper()
	p, err := (&Annotation{AnnotationKind: AnnotationKindNote, Text: new("note")}).MarshalBinary()
	require.NoError(t, err)

	return p
}

// TestSegmentWriterAppendsEventRecords appends, with Append, a transport-event and an annotation record
// whose field_validity is 0 and whose payloads decode:
// both are accepted, and the capture verifies without a writer defect.
func TestSegmentWriterAppendsEventRecords(t *testing.T) {
	t.Parallel()

	w, sink := newSeg(t, nil)
	ev := Record{
		TSUTCNs: segAt(time.Second).UnixNano(), Epoch: 1, Kind: KindTransportEvent, Dir: DirLocal,
		Fidelity: FidelityNotApplicable, DecodeStatus: DecodeStatusNotApplicable,
		Payload: mustMarshalEvent(t, &TransportEvent{Event: EventSocketClose}),
	}
	note := ev
	note.TSUTCNs, note.Kind, note.Payload = segAt(2*time.Second).UnixNano(), KindAnnotation, mustMarshalNote(t)
	require.NoError(t, w.Append(t.Context(), &ev))
	require.NoError(t, w.Append(t.Context(), &note))
	require.NoError(t, w.Close(t.Context()))

	recs := checkCapture(t, w, sink)
	require.Len(t, recs, 1)
	require.Len(t, recs[0], 4)
	assert.Equal(t, []Kind{KindTransportEvent, KindAnnotation}, []Kind{recs[0][1].Kind, recs[0][2].Kind})
}

func TestSegmentWriterMaxSegmentBytes(t *testing.T) {
	t.Parallel()

	// headLen is the size of a segment's file header and pack metadata, the size of a segment holding only its open block;
	// the probe writer exists only to measure it.
	probe, _ := newSeg(t, nil)
	headLen := int64(probe.seg.w.offset)
	require.NoError(t, probe.Close(t.Context()))

	incompressible := make([]byte, 3000)
	_, err := rand.Read(incompressible)
	require.NoError(t, err)
	zeros := make([]byte, 100_000)

	tests := []struct {
		name         string
		uncompressed bool
		// big is the record over the limit, steps whether it triggers a clock step.
		big   []byte
		steps bool
		want  [][]uint64
	}{
		{name: "a small record, then one over the limit", big: incompressible, want: [][]uint64{{0, 1, 2}, {3, 4}}},
		{name: "a compressible record over the limit", big: zeros, want: [][]uint64{{0, 1, 2, 3, 4}}},
		{name: "the same record uncompressed", big: zeros, uncompressed: true, want: [][]uint64{{0, 1, 2}, {3, 4}}},
		{name: "a record over the limit that triggers a clock step", big: incompressible, steps: true, want: [][]uint64{{0, 1, 2, 3}, {4, 5}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			w, sink := newSeg(t, func(o *SegmentWriterOptions) {
				o.MaxSegmentBytes, o.BlockThreshold, o.Uncompressed = headLen+1000, 2000, tt.uncompressed
			})
			segFrame(t, w, time.Second)
			require.Equal(t, uint64(headLen), w.seg.w.offset, "the small record stays in the open block")

			big := segMonoRecord(2*time.Second, 2*time.Second)
			if tt.steps {
				big.MonoNs = 0
			}
			big.Payload = tt.big
			big.SetCapturedFieldValidity()
			require.NoError(t, w.Append(t.Context(), &big))
			segFrame(t, w, 3*time.Second)
			require.NoError(t, w.Close(t.Context()))

			assert.Equal(t, tt.want, segSeqs(checkCapture(t, w, sink)))
		})
	}
}

func TestSegmentWriterAppendFrame(t *testing.T) {
	t.Parallel()

	// frame is a control frame whose SType, 0x7F, no standard defines.
	frame := bytes.Clone(controlTestFrame)
	frame[fieldSTypeOff] = 0x7F

	w, sink := newSeg(t, nil)
	for n := range len(frame) + 1 {
		require.NoError(t, w.AppendFrame(t.Context(), segAt(time.Duration(n)), DirHostToEquipment, 2, frame[:n]))
	}
	zeroSType := bytes.Clone(frame)
	zeroSType[fieldSTypeOff] = 0
	require.NoError(t, w.AppendFrame(t.Context(), segAt(time.Second), DirHostToEquipment, 2, zeroSType))
	require.NoError(t, w.Close(t.Context()))

	recs := checkCapture(t, w, sink)[0]
	frames := recs[1 : len(recs)-1]
	require.Len(t, frames, len(frame)+2)
	for n, r := range frames[:len(frame)+1] {
		want := KindData
		if n > fieldSTypeOff {
			want = KindControl
		}
		assert.Equal(t, want, r.Kind, "a %d-byte frame", n)
		assert.Equal(t, capturedFields(n), r.FieldValidity, "a %d-byte frame", n)
		assert.Equal(t, frame[:n], r.Payload, "a %d-byte frame", n)
	}
	assert.Equal(t, KindData, frames[len(frame)+1].Kind, "a frame whose SType is 0 is data")
}

func TestSegmentWriterAppendFrameFidelity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		method CaptureMethod
		want   Fidelity
	}{
		{CaptureMethodRawStream, FidelityWireExact},
		{CaptureMethodDecodedMessage, FidelityReEncoded},
		{CaptureMethodGenerator, FidelitySynthesized},
		{CaptureMethodLog, FidelityUnknown},
		{CaptureMethodUnknown, FidelityUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.method.String(), func(t *testing.T) {
			t.Parallel()

			w, sink := newSeg(t, func(o *SegmentWriterOptions) {
				o.Capture.CaptureMethod = tt.method
				o.Capture.SourceDialect, o.Capture.SourceRefs = new("eap-log"), []string{"a.log"}
			})
			err := w.AppendFrame(t.Context(), segAt(time.Second), DirHostToEquipment, 1, blockTestFrame)
			if tt.want == FidelityUnknown {
				require.ErrorIs(t, err, ErrInvalidRecord)
				require.NoError(t, w.Close(t.Context()), "a refused frame leaves the writer usable")
				assert.Equal(t, [][]uint64{{0, 1}}, segSeqs(checkCapture(t, w, sink)))

				return
			}
			require.NoError(t, err)
			require.NoError(t, w.Close(t.Context()))
			assert.Equal(t, tt.want, checkCapture(t, w, sink)[0][1].Fidelity)
		})
	}
}

// fakeClassifier is a Classifier returning a fixed result, which records the frames it saw.
type fakeClassifier struct {
	name     string
	max      uint64
	status   DecodeStatus
	trailing int
	seen     [][]byte
}

func (c *fakeClassifier) Name() string        { return c.name }
func (c *fakeClassifier) MaxFrameLen() uint64 { return c.max }

func (c *fakeClassifier) Frame(frame []byte) (DecodeStatus, int) {
	c.seen = append(c.seen, frame)

	return c.status, c.trailing
}

func TestSegmentWriterClassifier(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		c            fakeClassifier
		wantTrailing uint32
		wantMax      []uint64
	}{
		{name: "ok with trailing", c: fakeClassifier{name: "fake/1", status: DecodeStatusOKWithTrailing, trailing: 3}, wantTrailing: 3},
		{name: "trailing ignored for another status", c: fakeClassifier{name: "fake/1", status: DecodeStatusShortFrame, trailing: 3}},
		{name: "oversized under a ceiling", c: fakeClassifier{name: "fake/1", max: 13, status: DecodeStatusOversized}, wantMax: []uint64{13}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := tt.c
			w, sink := newSeg(t, func(o *SegmentWriterOptions) { o.Classifier = &c })
			frame := bytes.Clone(blockTestFrame)
			require.NoError(t, w.AppendFrame(t.Context(), segAt(time.Second), DirHostToEquipment, 1, frame))
			require.Len(t, c.seen, 1)
			assert.Same(t, &frame[0], &c.seen[0][0], "the classifier reads the caller's frame")
			frame[0] = 0xEE // the caller reuses its buffer
			require.NoError(t, w.Close(t.Context()))

			recs := checkCapture(t, w, sink)[0]
			r := recs[1]
			assert.Equal(t, blockTestFrame, r.Payload, "the stored frame is a copy")
			assert.Equal(t, c.status, r.DecodeStatus)
			assert.Equal(t, tt.wantTrailing, r.TrailingBytes)
			assert.Equal(t, DecodeStatusNotApplicable, recs[0].DecodeStatus, "the classifier does not apply to boundaries")
			m := sink.segments(t)[0].Header().Meta
			assert.Equal(t, []string{c.name}, m.Classifiers)
			assert.Equal(t, tt.wantMax, m.MaxFrameLens)
		})
	}

	t.Run("Append does not apply it", func(t *testing.T) {
		t.Parallel()

		c := fakeClassifier{name: "fake/1", status: DecodeStatusOK}
		w, sink := newSeg(t, func(o *SegmentWriterOptions) { o.Classifier = &c })
		r := segMonoRecord(time.Second, time.Second)
		require.NoError(t, w.Append(t.Context(), &r))
		require.NoError(t, w.Close(t.Context()))
		assert.Empty(t, c.seen)
		assert.Equal(t, DecodeStatusNotAttempted, checkCapture(t, w, sink)[0][1].DecodeStatus)
	})

	t.Run("trailing_bytes outside u32", func(t *testing.T) {
		t.Parallel()

		values := []int{-1}
		if strconv.IntSize == 64 {
			above := int64(math.MaxUint32) + 1
			values = append(values, int(above))
		}
		for _, trailing := range values {
			c := fakeClassifier{name: "fake/1", status: DecodeStatusOK, trailing: trailing}
			w, sink := newSeg(t, func(o *SegmentWriterOptions) { o.Classifier = &c })
			require.ErrorIs(t, w.AppendFrame(t.Context(), segAt(time.Second), DirHostToEquipment, 1, blockTestFrame), ErrInvalidRecord)
			require.NoError(t, w.Close(t.Context()))
			assert.Equal(t, [][]uint64{{0, 1}}, segSeqs(checkCapture(t, w, sink)))
		}
	})
}

func TestSegmentWriterMono(t *testing.T) {
	t.Parallel()

	mono := monotonicAt(segTestOrigin)
	wall := mono.Round(0)
	tests := []struct {
		name   string
		source TimeSource
		origin time.Time
		at     time.Time
		// wantStart and wantFrame are the mono_ns of the start boundary and of the frame, nil for none.
		wantStart, wantFrame *int64
	}{
		{"both readings", TimeSourceCaptureClock, mono, mono.Add(3 * time.Millisecond), new(int64(0)), new(int64(3 * time.Millisecond))},
		{"at without one", TimeSourceCaptureClock, mono, wall.Add(3 * time.Millisecond), new(int64(0)), nil},
		{"the origin without one", TimeSourceCaptureClock, wall, mono.Add(3 * time.Millisecond), nil, nil},
		{"not capture-clock", TimeSourceGenerator, mono, mono.Add(3 * time.Millisecond), nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			stop := tt.at.Add(time.Millisecond)
			w, sink := newSeg(t, func(o *SegmentWriterOptions) {
				o.Capture.TimeSource, o.Capture.CaptureOrigin, o.Now = tt.source, tt.origin, nowAt(stop)
			})
			require.NoError(t, w.AppendFrame(t.Context(), tt.at, DirHostToEquipment, 1, blockTestFrame))
			require.NoError(t, w.AppendEvent(t.Context(), tt.at, 1, &TransportEvent{Event: EventSocketConnect}))
			require.NoError(t, w.Close(t.Context()))

			var flat []Record
			for _, seg := range checkCapture(t, w, sink) {
				flat = append(flat, seg...)
			}
			require.Len(t, flat, 4)
			monoOf := func(r Record) *int64 {
				if !r.MonoPresent {
					return nil
				}

				return new(r.MonoNs)
			}
			assert.Equal(t, tt.wantStart, monoOf(flat[0]), "start")
			assert.Equal(t, tt.wantFrame, monoOf(flat[1]), "frame")
			assert.Equal(t, tt.wantFrame, monoOf(flat[2]), "event, as a frame")
			var wantStop *int64
			if tt.wantFrame != nil {
				wantStop = new(*tt.wantFrame + int64(time.Millisecond))
			}
			assert.Equal(t, wantStop, monoOf(flat[3]), "stop, as a frame")
			for _, r := range flat {
				assert.False(t, isClockStepRecord(t, r), "readings that agree make no clock step")
			}
		})
	}
}

func TestSegmentWriterCodec(t *testing.T) {
	t.Parallel()

	for _, uncompressed := range []bool{false, true} {
		w, sink := newSeg(t, func(o *SegmentWriterOptions) { o.Uncompressed = uncompressed })
		segFrame(t, w, time.Second)
		require.NoError(t, w.Close(t.Context()))
		checkCapture(t, w, sink)

		want := CodecZstd
		if uncompressed {
			want = CodecNone
		}
		file := sink.published[sink.keys[0]]
		r := sink.segments(t)[0]
		for _, b := range r.Blocks() {
			env, err := format.UnmarshalBlockEnvelope(file[b.Offset:])
			require.NoError(t, err)
			assert.Equal(t, uint8(want), env.Codec, "Uncompressed %v", uncompressed)
		}
	}
}

func TestSegmentWriterRecordsAndEvents(t *testing.T) {
	t.Parallel()

	w, sink := newSeg(t, nil)
	gap := BoundaryKindGap
	require.NoError(t, w.AppendEvent(t.Context(), segAt(time.Second), 0, &TransportEvent{Event: EventCaptureBoundary, BoundaryKind: &gap}),
		"a gap boundary is accepted")

	payload := bytes.Clone(blockTestFrame)
	r := Record{
		Seq: 99, TSUTCNs: segAt(2 * time.Second).UnixNano(), Epoch: 3, Kind: KindData, Dir: DirEquipmentToHost,
		Fidelity: FidelityReconstructed, DecodeStatus: DecodeStatusNotAttempted, Quality: QualityDirectionInferred | QualityCaptureBoundary,
		FieldValidity: FieldValiditySessionID, Payload: payload,
	}
	given := r
	require.NoError(t, w.Append(t.Context(), &r))
	assert.Equal(t, given, r, "Append does not modify the record")
	payload[0] = 0xEE
	require.NoError(t, w.Close(t.Context()))

	recs := checkCapture(t, w, sink)[0]
	require.Len(t, recs, 4)
	ev := recs[1]
	assert.Equal(t, BoundaryKindGap, boundaryKindOf(t, ev))
	assert.Equal(t, [4]any{DirLocal, FidelityNotApplicable, DecodeStatusNotApplicable, QualityCaptureBoundary | QualityCorrelationIncomplete},
		[4]any{ev.Dir, ev.Fidelity, ev.DecodeStatus, ev.Quality})

	got := recs[2]
	want := given
	want.Seq, want.Payload, want.Quality = 2, blockTestFrame, QualityDirectionInferred
	assert.Equal(t, want, got, "Append keeps every field but the seq and the derived quality bits")
}

func TestSegmentWriterCopiesDescriptor(t *testing.T) {
	t.Parallel()

	sink := newMemSink()
	opts := segOptions(sink)
	opts.Capture.SiteID = new("fab-12")
	w, err := NewSegmentWriter(t.Context(), opts)
	require.NoError(t, err)
	*opts.Capture.SiteID = "changed"
	require.NoError(t, w.Rotate(t.Context()))
	segFrame(t, w, time.Second)
	require.NoError(t, w.Close(t.Context()))

	for _, r := range sink.segments(t) {
		assert.Equal(t, new("fab-12"), r.Header().Meta.SiteID)
	}
}

func TestSegmentWriterCloseTwice(t *testing.T) {
	t.Parallel()

	w, sink := newSeg(t, nil)
	require.NoError(t, w.Close(t.Context()))
	calls := sink.calls
	require.NoError(t, w.Close(t.Context()), "a second Close returns the first one's result")
	assert.Equal(t, calls, sink.calls, "a second Close makes no sink call")
	require.ErrorIs(t, w.AppendFrame(t.Context(), segAt(time.Second), DirHostToEquipment, 1, blockTestFrame), ErrClosed)
	require.ErrorIs(t, w.Rotate(t.Context()), ErrClosed)
}

func TestWriterTagOf(t *testing.T) {
	t.Parallel()

	dep := func(path, version string) *debug.Module { return &debug.Module{Path: path, Version: version} }
	tests := []struct {
		name string
		info *debug.BuildInfo
		ok   bool
		want string
	}{
		{"no build info", nil, false, "tracepack/unknown"},
		{"the main module", &debug.BuildInfo{Main: debug.Module{Path: tracepackModulePath, Version: "v0.3.0"}}, true, "tracepack/v0.3.0"},
		{"a development build", &debug.BuildInfo{Main: debug.Module{Path: tracepackModulePath, Version: "(devel)"}}, true, "tracepack/devel"},
		{
			"a dependency",
			&debug.BuildInfo{Main: debug.Module{Path: "example.com/recorder"}, Deps: []*debug.Module{dep("other", "v1"), dep(tracepackModulePath, "v0.4.1")}},
			true, "tracepack/v0.4.1",
		},
		{"not found", &debug.BuildInfo{Main: debug.Module{Path: "example.com/recorder"}}, true, "tracepack/unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, writerTagOf(tt.info, tt.ok))
		})
	}
}

func TestHasMonotonic(t *testing.T) {
	t.Parallel()

	now := time.Now()
	assert.True(t, hasMonotonic(now))
	assert.True(t, hasMonotonic(now.Add(time.Hour)))
	assert.False(t, hasMonotonic(now.Round(0)))
	assert.False(t, hasMonotonic(time.Unix(0, 1)))
	assert.False(t, hasMonotonic(time.Time{}))
}
