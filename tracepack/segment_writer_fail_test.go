package tracepack

import (
	"context"
	"errors"
	"math"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/format"
)

func TestNewSegmentWriterRefusesBeforeSinkCalls(t *testing.T) {
	t.Parallel()

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	tests := []struct {
		name   string
		ctx    context.Context
		mutate func(*SegmentWriterOptions)
		check  func(t *testing.T, err error)
	}{
		{
			name:   "an invalid descriptor",
			mutate: func(o *SegmentWriterOptions) { o.Capture.Recorder = "" },
			check:  fieldErr("recorder", ErrRequiredTag),
		},
		{
			name: "a flush interval not dividing the hour",
			// The origin's 7-minute period, [+29 min, +36 min), lies inside the hour, so only the interval is wrong.
			mutate: func(o *SegmentWriterOptions) {
				o.FlushInterval, o.Capture.CaptureOrigin = 7*time.Minute, time.Unix(0, blockTestHour+int64(30*time.Minute))
			},
		},
		{name: "a negative flush interval", mutate: func(o *SegmentWriterOptions) { o.FlushInterval = -time.Minute }},
		{name: "a flush interval above the hour", mutate: func(o *SegmentWriterOptions) { o.FlushInterval = 2 * time.Hour }},
		{name: "a negative MaxSegmentBytes", mutate: func(o *SegmentWriterOptions) { o.MaxSegmentBytes = -1 }},
		{name: "an invalid BlockThreshold", mutate: func(o *SegmentWriterOptions) { o.BlockThreshold = -1 }},
		{
			name:   "a classifier ceiling above 2^63-1",
			mutate: func(o *SegmentWriterOptions) { o.Classifier = &fakeClassifier{name: "fake/1", max: format.MaxU64 + 1} },
			check:  fieldErr("max_frame_len", ErrFieldValue),
		},
		{
			name:   "a classifier without a name",
			mutate: func(o *SegmentWriterOptions) { o.Classifier = &fakeClassifier{} },
			check:  fieldErr("classifier", ErrFieldValue),
		},
		{name: "a classifier name the encoding refuses", mutate: func(o *SegmentWriterOptions) { o.Classifier = &fakeClassifier{name: "\xff"} }},
		{
			name:   "a CaptureOrigin not representable",
			mutate: func(o *SegmentWriterOptions) { o.Capture.CaptureOrigin = time.Date(2300, 1, 1, 0, 0, 0, 0, time.UTC) },
		},
		{
			name: "a zero CaptureOrigin taking a Now() not representable",
			mutate: func(o *SegmentWriterOptions) {
				o.Capture.CaptureOrigin, o.Now = time.Time{}, nowAt(time.Unix(0, math.MinInt64))
			},
		},
		{
			name:  "a done context",
			ctx:   canceled,
			check: func(t *testing.T, err error) { t.Helper(); require.ErrorIs(t, err, context.Canceled) },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sink := newMemSink()
			opts := segOptions(sink)
			if tt.mutate != nil {
				tt.mutate(&opts)
			}
			ctx := tt.ctx
			if ctx == nil {
				ctx = t.Context()
			}
			w, err := NewSegmentWriter(ctx, opts)
			require.Error(t, err)
			assert.Nil(t, w)
			if tt.check != nil {
				tt.check(t, err)
			}
			assert.Equal(t, [sinkOpCount]int{}, sink.calls, "no sink call")
		})
	}

	_, err := NewSegmentWriter(t.Context(), SegmentWriterOptions{Capture: validDescriptor()})
	require.Error(t, err, "a nil sink")
}

// fieldErr returns a check that err is a *FieldError on field wrapping want.
func fieldErr(field string, want error) func(t *testing.T, err error) {
	return func(t *testing.T, err error) {
		t.Helper()
		var fe *FieldError
		require.ErrorAs(t, err, &fe)
		assert.Equal(t, field, fe.Field)
		require.ErrorIs(t, err, want)
	}
}

func TestNewSegmentWriterAcceptsOptions(t *testing.T) {
	t.Parallel()

	for _, f := range []time.Duration{time.Nanosecond, time.Second, 15 * time.Minute, time.Hour} {
		w, sink := newSeg(t, func(o *SegmentWriterOptions) { o.FlushInterval = f })
		require.NoError(t, w.Close(t.Context()))
		m := checkCapture(t, w, sink)
		require.NotEmpty(t, m)
		info := sink.infos[0]
		assert.Equal(t, f, info.PeriodEnd.Sub(info.PeriodStart), "flush interval %v", f)
	}

	// A zero CaptureOrigin takes Now().
	at := segAt(time.Minute)
	w, sink := newSeg(t, func(o *SegmentWriterOptions) { o.Capture.CaptureOrigin, o.Now = time.Time{}, nowAt(at) })
	require.NoError(t, w.Close(t.Context()))
	recs := checkCapture(t, w, sink)
	assert.Equal(t, at.UnixNano(), recs[0][0].TSUTCNs)
	assert.Equal(t, new(at.UnixNano()), sink.segments(t)[0].Header().Meta.CaptureOriginUTCNs)
}

// periodByBig returns the period of length f holding ts, computed exactly, and whether it fits in int64.
func periodByBig(ts, f int64) (int64, int64, bool) {
	q := new(big.Int).Div(big.NewInt(ts), big.NewInt(f)) // Euclidean division: floor for f > 0
	start := new(big.Int).Mul(q, big.NewInt(f))
	end := new(big.Int).Add(start, big.NewInt(f))
	if !start.IsInt64() || !end.IsInt64() {
		return 0, 0, false
	}

	return start.Int64(), end.Int64(), true
}

func TestPeriodOf(t *testing.T) {
	t.Parallel()

	for _, f := range []int64{1, int64(time.Second), int64(7 * time.Minute), int64(DefaultFlushInterval), hourNs} {
		for _, ts := range []int64{
			0, 1, -1, f - 1, f, -f, -f - 1, -f + 1, blockTestHour,
			math.MinInt64, math.MinInt64 + 1, math.MinInt64 + f, math.MaxInt64, math.MaxInt64 - 1, math.MaxInt64 - f, math.MaxInt64 - 2*f,
		} {
			start, end, ok := periodOf(ts, f)
			wantStart, wantEnd, wantOK := periodByBig(ts, f)
			assert.Equal(t, [3]any{wantStart, wantEnd, wantOK}, [3]any{start, end, ok}, "ts %d, period %d", ts, f)
		}
	}
}

// representabilityVectors returns times around the edges of what a SegmentWriter with the default flush interval represents,
// with whether each is representable.
func representabilityVectors() []struct {
	name string
	at   time.Time
	ok   bool
} {
	f := int64(DefaultFlushInterval)
	firstStart, _, _ := periodByBig(math.MinInt64+f, f) // the first period that fits
	lastStart, _, _ := periodByBig(math.MaxInt64-f, f)  // the last period that fits
	ns := func(v int64) time.Time { return time.Unix(0, v).UTC() }

	return []struct {
		name string
		at   time.Time
		ok   bool
	}{
		{"-1ns", ns(-1), true},
		{"a negative period's start", ns(-f), true},
		{"the nanosecond before a negative period's start", ns(-f - 1), true},
		{"the smallest int64", ns(math.MinInt64), false},
		{"the nanosecond before the first period that fits", ns(firstStart - 1), false},
		{"the first period that fits", ns(firstStart), true},
		{"the largest int64", ns(math.MaxInt64), false},
		{"the first nanosecond after the last period that fits", ns(lastStart + f), false},
		{"the end of the last period that fits", ns(lastStart + f - 1), true},
		{"a time past the int64 nanoseconds", time.Date(2300, 1, 1, 0, 0, 0, 0, time.UTC), false},
		{"a time before the int64 nanoseconds", time.Date(1600, 1, 1, 0, 0, 0, 0, time.UTC), false},
	}
}

func TestSegmentWriterRepresentableAt(t *testing.T) {
	t.Parallel()

	for _, v := range representabilityVectors() {
		t.Run(v.name, func(t *testing.T) {
			t.Parallel()

			w, sink := newSeg(t, nil)
			err := w.AppendFrame(t.Context(), v.at, DirHostToEquipment, 1, blockTestFrame)
			if !v.ok {
				require.ErrorIs(t, err, ErrInvalidRecord)
				assert.Equal(t, 1, sink.calls[sinkCreate], "no segment rolled or opened")
				require.NoError(t, w.Close(t.Context()), "the writer stays usable")
				assert.Equal(t, [][]uint64{{0, 1}}, segSeqs(checkCapture(t, w, sink)))

				return
			}
			require.NoError(t, err)
			require.NoError(t, w.Close(t.Context()))
			recs := checkCapture(t, w, sink)
			require.Len(t, recs, 3)
			start, end, _ := periodByBig(v.at.UnixNano(), int64(DefaultFlushInterval))
			assert.Equal(t, [2]time.Time{time.Unix(0, start).UTC(), time.Unix(0, end).UTC()}, [2]time.Time{sink.infos[1].PeriodStart, sink.infos[1].PeriodEnd})
		})
	}
}

func TestSegmentWriterRepresentableOrigin(t *testing.T) {
	t.Parallel()

	for _, v := range representabilityVectors() {
		t.Run(v.name, func(t *testing.T) {
			t.Parallel()

			sink := newMemSink()
			opts := segOptions(sink)
			opts.Capture.CaptureOrigin = v.at
			w, err := NewSegmentWriter(t.Context(), opts)
			if !v.ok {
				require.Error(t, err)
				assert.Equal(t, [sinkOpCount]int{}, sink.calls, "no sink call")

				return
			}
			require.NoError(t, err)
			require.NoError(t, w.Close(t.Context()))
			recs := checkCapture(t, w, sink)
			assert.Equal(t, v.at.UnixNano(), recs[0][0].TSUTCNs)
		})
	}
}

func TestSegmentWriterRepresentableNow(t *testing.T) {
	t.Parallel()

	for _, v := range representabilityVectors() {
		t.Run(v.name, func(t *testing.T) {
			t.Parallel()

			w, sink := newSeg(t, func(o *SegmentWriterOptions) { o.Now = nowAt(v.at) })
			segFrame(t, w, time.Second)
			err := w.Close(t.Context())
			if !v.ok {
				require.Error(t, err)
				assert.Equal(t, 1, sink.aborts(), "the open segment aborted")
				assert.Empty(t, sink.keys, "nothing published")
				calls := sink.calls
				assert.Equal(t, err, w.Close(t.Context()), "a second Close returns the same failure")
				assert.Equal(t, calls, sink.calls)
				sink.noMisuse(t)

				return
			}
			require.NoError(t, err)
			recs := checkCapture(t, w, sink)
			last := recs[len(recs)-1]
			assert.Equal(t, v.at.UnixNano(), last[len(last)-1].TSUTCNs, "stop at Now()")
		})
	}
}

func TestSegmentWriterPreflight(t *testing.T) {
	t.Parallel()

	for _, bySize := range []bool{false, true} {
		// d places every record: in the next hour, which rolls the segment by hour,
		// or, with a size limit every caller record reaches, in the same hour, which rolls it by size only.
		d, variant := time.Hour, ", by hour"
		if bySize {
			d, variant = 2*time.Second, ", by size"
		}
		for _, tt := range preflightCases(d) {
			t.Run(tt.name+variant, func(t *testing.T) {
				t.Parallel()

				w, sink := newSeg(t, func(o *SegmentWriterOptions) {
					if bySize {
						o.MaxSegmentBytes, o.BlockThreshold = 1, 1
					}
				})
				segFrame(t, w, time.Second)
				offset, seq, anchor, calls := w.seg.w.offset, w.nextSeq, w.anchorInForce(), sink.calls

				err := tt.call(t, w)
				require.Error(t, err)
				if tt.want != nil {
					require.ErrorIs(t, err, tt.want)
				}
				assert.Equal(t, calls, sink.calls, "no sink call")
				assert.Equal(t, offset, w.seg.w.offset, "nothing written")
				assert.Equal(t, [2]any{seq, anchor}, [2]any{w.nextSeq, w.anchorInForce()}, "no seq taken, no anchor moved")

				// The writer stays usable and leaves no segment without records.
				r := segMonoRecord(d, d)
				require.NoError(t, w.Append(t.Context(), &r))
				require.NoError(t, w.Rotate(t.Context()))
				require.NoError(t, w.Close(t.Context()))
				recs := checkCapture(t, w, sink)
				for _, seg := range recs {
					for _, rec := range seg[1:] {
						assert.NotEqual(t, KindTransportEvent, rec.Kind, "only the boundaries are events")
					}
				}
			})
		}
	}
}

// preflightCase is a call the SegmentWriter refuses before it touches a segment, with the error it wraps (nil for any).
type preflightCase struct {
	name string
	call func(t *testing.T, w *SegmentWriter) error
	want error
}

// preflightCases returns the refused calls of TestSegmentWriterPreflight, each at d after segTestOrigin.
func preflightCases(d time.Duration) []preflightCase {
	stop, unclean, start := BoundaryKindStop, BoundaryKindStopUnclean, BoundaryKindStart
	boundary := func(k *BoundaryKind) *TransportEvent {
		return &TransportEvent{Event: EventCaptureBoundary, BoundaryKind: k}
	}
	// with returns a valid record at d, changed by fn.
	with := func(fn func(r *Record)) *Record { r := segMonoRecord(d, d); fn(&r); return &r }
	asEvent := func(t *testing.T, ev *TransportEvent) *Record {
		return with(func(r *Record) {
			r.Kind, r.Dir, r.Fidelity, r.DecodeStatus = KindTransportEvent, DirLocal, FidelityNotApplicable, DecodeStatusNotApplicable
			r.FieldValidity, r.Payload = 0, mustMarshalEvent(t, ev)
		})
	}
	// asEventValidity returns a valid record of kind, a transport event or an annotation, with field_validity fv.
	asEventValidity := func(t *testing.T, kind Kind, fv FieldValidity) *Record {
		r := asEvent(t, &TransportEvent{Event: EventSocketClose})
		if kind == KindAnnotation {
			r.Payload = mustMarshalNote(t)
		}
		r.Kind, r.FieldValidity = kind, fv

		return r
	}

	return []preflightCase{
		{"a classified record without a classifier", func(t *testing.T, w *SegmentWriter) error {
			return w.Append(t.Context(), with(func(r *Record) { r.DecodeStatus = DecodeStatusOK }))
		}, ErrMetadataCommitment},
		{"an oversized record", func(t *testing.T, w *SegmentWriter) error {
			return w.Append(t.Context(), with(func(r *Record) { r.DecodeStatus = DecodeStatusOversized }))
		}, ErrMetadataCommitment},
		{"a redacted record", func(t *testing.T, w *SegmentWriter) error {
			return w.Append(t.Context(), with(func(r *Record) { r.Quality = QualityRedacted }))
		}, ErrMetadataCommitment},
		{"field_validity beyond the payload", func(t *testing.T, w *SegmentWriter) error {
			return w.Append(t.Context(), with(func(r *Record) { r.Payload = r.Payload[:9] }))
		}, ErrFieldValidity},
		{"a nil record", func(t *testing.T, w *SegmentWriter) error { return w.Append(t.Context(), nil) }, ErrInvalidRecord},
		{"a ts_utc_ns not representable", func(t *testing.T, w *SegmentWriter) error {
			return w.Append(t.Context(), with(func(r *Record) { r.TSUTCNs = math.MaxInt64 }))
		}, ErrInvalidRecord},
		{"Append of a start boundary", func(t *testing.T, w *SegmentWriter) error {
			return w.Append(t.Context(), asEvent(t, boundary(&start)))
		}, ErrInvalidRecord},
		{"Append of a stop boundary", func(t *testing.T, w *SegmentWriter) error {
			return w.Append(t.Context(), asEvent(t, boundary(&stop)))
		}, ErrInvalidRecord},
		{"Append of a stop-unclean boundary", func(t *testing.T, w *SegmentWriter) error {
			return w.Append(t.Context(), asEvent(t, boundary(&unclean)))
		}, ErrInvalidRecord},
		{"Append of a transport event whose payload does not decode", func(t *testing.T, w *SegmentWriter) error {
			return w.Append(t.Context(), with(func(r *Record) {
				*r = *asEvent(t, &TransportEvent{Event: EventSocketClose})
				r.Payload = []byte{0x01}
			}))
		}, ErrInvalidRecord},
		{"Append of an annotation whose payload does not decode", func(t *testing.T, w *SegmentWriter) error {
			return w.Append(t.Context(), with(func(r *Record) {
				*r = *asEvent(t, &TransportEvent{Event: EventSocketClose})
				r.Kind, r.Payload = KindAnnotation, []byte{0x01}
			}))
		}, ErrInvalidRecord},
		{"Append of a transport event with field_validity", func(t *testing.T, w *SegmentWriter) error {
			return w.Append(t.Context(), with(func(r *Record) {
				*r = *asEvent(t, &TransportEvent{Event: EventSocketClose})
				r.FieldValidity = FieldValiditySessionID
			}))
		}, ErrInvalidRecord},
		{"Append of an annotation with field_validity", func(t *testing.T, w *SegmentWriter) error {
			return w.Append(t.Context(), with(func(r *Record) {
				*r = *asEvent(t, &TransportEvent{Event: EventSocketClose})
				r.Kind, r.FieldValidity, r.Payload = KindAnnotation, FieldValidityFunction, mustMarshalNote(t)
			}))
		}, ErrInvalidRecord},
		{"Append of a transport event with a reserved field_validity bit 6", func(t *testing.T, w *SegmentWriter) error {
			return w.Append(t.Context(), asEventValidity(t, KindTransportEvent, 0x40))
		}, ErrInvalidRecord},
		{"Append of a transport event with a reserved field_validity bit 7", func(t *testing.T, w *SegmentWriter) error {
			return w.Append(t.Context(), asEventValidity(t, KindTransportEvent, 0x80))
		}, ErrInvalidRecord},
		{"Append of a transport event with both reserved field_validity bits", func(t *testing.T, w *SegmentWriter) error {
			return w.Append(t.Context(), asEventValidity(t, KindTransportEvent, 0xC0))
		}, ErrInvalidRecord},
		{"Append of an annotation with a reserved field_validity bit 6", func(t *testing.T, w *SegmentWriter) error {
			return w.Append(t.Context(), asEventValidity(t, KindAnnotation, 0x40))
		}, ErrInvalidRecord},
		{"Append of an annotation with a reserved field_validity bit 7", func(t *testing.T, w *SegmentWriter) error {
			return w.Append(t.Context(), asEventValidity(t, KindAnnotation, 0x80))
		}, ErrInvalidRecord},
		{"Append of an annotation with both reserved field_validity bits", func(t *testing.T, w *SegmentWriter) error {
			return w.Append(t.Context(), asEventValidity(t, KindAnnotation, 0xC0))
		}, ErrInvalidRecord},
		{"AppendEvent of a stop boundary", func(t *testing.T, w *SegmentWriter) error {
			return w.AppendEvent(t.Context(), segAt(d), 0, boundary(&stop))
		}, ErrInvalidRecord},
		{"AppendEvent of a start boundary", func(t *testing.T, w *SegmentWriter) error {
			return w.AppendEvent(t.Context(), segAt(d), 0, boundary(&start))
		}, ErrInvalidRecord},
		{"AppendEvent of a stop-unclean boundary", func(t *testing.T, w *SegmentWriter) error {
			return w.AppendEvent(t.Context(), segAt(d), 0, boundary(&unclean))
		}, ErrInvalidRecord},
		{"AppendEvent of a nil event", func(t *testing.T, w *SegmentWriter) error {
			return w.AppendEvent(t.Context(), segAt(d), 0, nil)
		}, ErrInvalidRecord},
		{"AppendEvent of an event the encoding refuses", func(t *testing.T, w *SegmentWriter) error {
			return w.AppendEvent(t.Context(), segAt(d), 0, &TransportEvent{Event: EventSocketClose, Detail: new("\xff")})
		}, nil},
		{"AppendFrame at a time not representable", func(t *testing.T, w *SegmentWriter) error {
			return w.AppendFrame(t.Context(), time.Unix(0, math.MaxInt64), DirHostToEquipment, 1, blockTestFrame)
		}, ErrInvalidRecord},
	}
}

func TestSegmentWriterSeqCapacity(t *testing.T) {
	t.Parallel()

	w, sink := newSeg(t, nil)
	open := w.seg
	w.nextSeq, open.w.nextSeq = format.MaxU64, format.MaxU64 // one seq left

	// A record of the next hour whose drift needs a clock-step record before it: two seqs.
	two := segMonoRecord(time.Hour, 0)
	anchor, offset, calls := w.anchorInForce(), open.w.offset, sink.calls
	require.ErrorIs(t, w.Append(t.Context(), &two), ErrSeqOrder)
	assert.Equal(t, calls, sink.calls, "no sink call: no segment rolled or opened")
	assert.Same(t, open, w.seg, "the open segment stays open")
	assert.Equal(t, offset, open.w.offset, "nothing written")
	assert.Equal(t, anchor, w.anchorInForce(), "the anchor did not move")
	assert.Equal(t, [2]uint64{format.MaxU64, format.MaxU64}, [2]uint64{w.nextSeq, open.w.nextSeq}, "no seq taken")

	one := segMonoRecord(time.Hour, time.Hour)
	require.NoError(t, w.Append(t.Context(), &one), "a record needing one seq takes the last")
	require.ErrorIs(t, w.AppendFrame(t.Context(), segAt(time.Hour), DirHostToEquipment, 1, blockTestFrame), ErrSeqOrder,
		"an exhausted capture refuses every record")

	err := w.Close(t.Context())
	require.ErrorIs(t, err, ErrSeqOrder, "no seq left for stop")
	assert.Equal(t, err, w.Close(t.Context()), "a second Close returns the same failure")
	require.ErrorIs(t, w.Rotate(t.Context()), ErrWriterFailed)
	sink.noMisuse(t)
	assert.Zero(t, sink.aborts())

	recs := sink.records(t)
	require.Len(t, recs, 2)
	assert.Equal(t, [][]uint64{{0}, {format.MaxU64}}, segSeqs(recs), "the open segment finalized without stop")
}

func TestSegmentWriterSeqCapacityForStop(t *testing.T) {
	t.Parallel()

	// A stop whose drift needs a clock-step record before it, with one seq left.
	origin := monotonicAt(segTestOrigin)
	w, sink := newSeg(t, func(o *SegmentWriterOptions) {
		o.Capture.CaptureOrigin, o.Now = origin, nowAt(origin.Add(time.Second))
	})
	step := Record{
		TSUTCNs: origin.UnixNano() + int64(10*time.Second), MonoNs: int64(5 * time.Second), MonoPresent: true, Epoch: 1,
		Kind: KindTransportEvent, Dir: DirLocal, Fidelity: FidelityNotApplicable, DecodeStatus: DecodeStatusNotApplicable,
		Payload: mustMarshalEvent(t, &TransportEvent{Event: EventClockStep, ClockStepNs: new(int64(5 * time.Second))}),
	}
	require.NoError(t, w.Append(t.Context(), &step))
	require.NoError(t, w.Rotate(t.Context()))
	w.nextSeq = format.MaxU64

	require.ErrorIs(t, w.Close(t.Context()), ErrSeqOrder)
	assert.Equal(t, 1, sink.calls[sinkCreate], "no segment opened for the stop")
	assert.Zero(t, sink.aborts())
}

func TestSegmentWriterCancellation(t *testing.T) {
	t.Parallel()

	t.Run("before any sink call", func(t *testing.T) {
		t.Parallel()

		w, sink := newSeg(t, nil)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		calls := sink.calls
		require.ErrorIs(t, w.AppendFrame(ctx, segAt(time.Hour), DirHostToEquipment, 1, blockTestFrame), context.Canceled)
		require.ErrorIs(t, w.AppendEvent(ctx, segAt(time.Hour), 1, &TransportEvent{Event: EventSocketClose}), context.Canceled)
		r := segMonoRecord(time.Hour, time.Hour)
		require.ErrorIs(t, w.Append(ctx, &r), context.Canceled)
		require.ErrorIs(t, w.Rotate(ctx), context.Canceled)
		require.ErrorIs(t, w.Tick(ctx, segAt(time.Hour)), context.Canceled)
		require.ErrorIs(t, w.Close(ctx), context.Canceled)
		assert.Equal(t, calls, sink.calls, "no sink call")
		assert.Equal(t, uint64(1), w.nextSeq)

		segFrame(t, w, time.Second)
		require.NoError(t, w.Close(t.Context()), "the writer stays usable")
		assert.Equal(t, [][]uint64{{0, 1, 2}}, segSeqs(checkCapture(t, w, sink)))
	})

	tests := []struct {
		name string
		// arm sets the sink's hooks to cancel ctx.
		arm func(sink *memSink, cancel func())
		// commits and creates are the Commit and Create calls, the first segment's Create included.
		commits, creates int
		// published and aborts are the segments published and the Abort calls; uncertain whether the error wraps ErrPublishUncertain.
		published, aborts int
		uncertain         bool
	}{
		{
			name:    "in a write of the finalized segment",
			arm:     func(s *memSink, cancel func()) { s.fault = cancelAt(sinkWrite, cancel) },
			commits: 0, creates: 1, published: 0, aborts: 1,
		},
		{
			name:    "in Commit before publication",
			arm:     func(s *memSink, cancel func()) { s.fault = cancelAt(sinkCommit, cancel) },
			commits: 1, creates: 1, published: 0, aborts: 1,
		},
		{
			name:    "in Commit after publication",
			arm:     func(s *memSink, cancel func()) { s.fault = cancelAt(sinkPublished, cancel) },
			commits: 1, creates: 1, published: 1, aborts: 1, uncertain: true,
		},
		{
			name:    "after a Commit returned",
			arm:     func(s *memSink, cancel func()) { s.afterCommit = cancel },
			commits: 1, creates: 1, published: 1, aborts: 0,
		},
		{
			name:    "in Create",
			arm:     func(s *memSink, cancel func()) { s.fault = cancelAt(sinkCreate, cancel) },
			commits: 1, creates: 2, published: 1, aborts: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			w, sink := newSeg(t, nil)
			segFrame(t, w, time.Second)
			ctx, cancel := context.WithCancel(t.Context())
			tt.arm(sink, cancel)
			before := w.nextSeq

			// A frame of the next hour rolls: Writer.Close, Commit, Create, then the head write.
			err := w.AppendFrame(ctx, segAt(time.Hour), DirHostToEquipment, 1, blockTestFrame)
			require.ErrorIs(t, err, context.Canceled)
			assert.Equal(t, tt.uncertain, errors.Is(err, ErrPublishUncertain), "ErrPublishUncertain")
			assert.Equal(t, [2]int{tt.commits, tt.creates}, [2]int{sink.calls[sinkCommit], sink.calls[sinkCreate]}, "Commit and Create calls")
			assert.Len(t, sink.keys, tt.published, "segments published")
			assert.Equal(t, tt.aborts, sink.aborts(), "Abort calls")
			assert.GreaterOrEqual(t, w.nextSeq, before, "the seq counter is not rewound")
			sink.noMisuse(t)

			calls := sink.calls
			require.ErrorIs(t, w.AppendFrame(t.Context(), segAt(time.Hour), DirHostToEquipment, 1, blockTestFrame), ErrWriterFailed)
			assert.Equal(t, err, w.Close(t.Context()), "Close returns the failure")
			assert.Equal(t, calls, sink.calls, "no sink call after the failure")
		})
	}
}

// cancelAt returns a fault hook that calls cancel at every call of op and fails nothing.
func cancelAt(op sinkOp, cancel func()) func(sinkOp, int) error {
	return func(o sinkOp, _ int) error {
		if o == op {
			cancel()
		}

		return nil
	}
}

// TestSegmentWriterCancellationWithinSegment cancels ctx in a sink call of an append that stays in the open segment,
// whose block, with BlockThreshold 1, is written and synced at once,
// while the sink call itself succeeds:
// the append fails the SegmentWriter, aborting the segment once, rather than report success.
func TestSegmentWriterCancellationWithinSegment(t *testing.T) {
	t.Parallel()

	for _, op := range []sinkOp{sinkWrite, sinkSync} {
		t.Run(op.String(), func(t *testing.T) {
			t.Parallel()

			w, sink := newSeg(t, func(o *SegmentWriterOptions) { o.BlockThreshold = 1 })
			ctx, cancel := context.WithCancel(t.Context())
			after := armCancel(ctx, cancel, sink, op)
			calls := sink.calls[op]

			err := w.AppendFrame(ctx, segAt(time.Second), DirHostToEquipment, 1, blockTestFrame)
			require.ErrorIs(t, err, context.Canceled)
			assert.Equal(t, calls+1, sink.calls[op], "ctx canceled in the block's %s", op)
			assert.Empty(t, *after, "no sink call but Abort once ctx is done")
			assert.Equal(t, 1, sink.aborts(), "Abort calls")
			assert.Empty(t, sink.keys, "nothing published")
			sink.noMisuse(t)

			before := sink.calls
			require.ErrorIs(t, w.AppendFrame(t.Context(), segAt(2*time.Second), DirHostToEquipment, 1, blockTestFrame), ErrWriterFailed)
			assert.Equal(t, err, w.Close(t.Context()), "Close returns the failure")
			assert.Equal(t, before, sink.calls, "no sink call after the failure")
		})
	}
}

// armCancel sets a fault hook on sink that cancels ctx at every call of op and fails nothing,
// and returns the operations, Abort aside, that sink saw once ctx was done.
func armCancel(ctx context.Context, cancel func(), sink *memSink, op sinkOp) *[]sinkOp {
	var after []sinkOp
	sink.fault = func(o sinkOp, _ int) error {
		if ctx.Err() != nil && o != sinkAbort {
			after = append(after, o)
		}
		if o == op {
			cancel()
		}

		return nil
	}

	return &after
}

// TestNewSegmentWriterCancellationInHeaderWrite cancels ctx in the write of the first segment's file header,
// which succeeds: NewSegmentWriter fails with ctx's error and aborts the segment once.
// The default BlockThreshold writes nothing more; BlockThreshold 1 would write the start's block.
func TestNewSegmentWriterCancellationInHeaderWrite(t *testing.T) {
	t.Parallel()

	for _, threshold := range []int{0, 1} {
		sink := newMemSink()
		ctx, cancel := context.WithCancel(t.Context())
		after := armCancel(ctx, cancel, sink, sinkWrite)
		opts := segOptions(sink)
		opts.BlockThreshold = threshold

		w, err := NewSegmentWriter(ctx, opts)
		require.ErrorIs(t, err, context.Canceled, "threshold %d", threshold)
		assert.Nil(t, w, "threshold %d", threshold)
		assert.Equal(t, 1, sink.calls[sinkWrite], "threshold %d: the header write only", threshold)
		assert.Empty(t, *after, "threshold %d: no sink call but Abort once ctx is done", threshold)
		assert.Equal(t, 1, sink.aborts(), "threshold %d: Abort calls", threshold)
		assert.Empty(t, sink.keys, "threshold %d", threshold)
		sink.noMisuse(t)
	}
}

func TestSegmentWriterSinkFailures(t *testing.T) {
	t.Parallel()

	abortErr := errors.New("abort failed")
	tests := []struct {
		name string
		// fault fails an operation of the roll a frame of the next hour causes; sink is the sink it is set on.
		fault func(sink *memSink) func(sinkOp, int) error
		// published and aborts are the segments published and the Abort calls; uncertain whether the error wraps ErrPublishUncertain.
		published, aborts int
		uncertain         bool
		joined            bool
	}{
		{
			name:      "a write of the finalized segment",
			fault:     func(s *memSink) func(sinkOp, int) error { return failAt(sinkWrite, s.calls[sinkWrite]) },
			published: 0, aborts: 1,
		},
		{
			name:      "a sync of the finalized segment",
			fault:     func(s *memSink) func(sinkOp, int) error { return failAt(sinkSync, s.calls[sinkSync]) },
			published: 0, aborts: 1,
		},
		{
			name:      "Commit before publication",
			fault:     func(*memSink) func(sinkOp, int) error { return failAt(sinkCommit, 0) },
			published: 0, aborts: 1,
		},
		{
			name:      "Commit after publication",
			fault:     func(*memSink) func(sinkOp, int) error { return failAt(sinkPublished, 0) },
			published: 1, aborts: 1, uncertain: true,
		},
		{
			name: "Commit after publication, then Abort",
			fault: func(*memSink) func(sinkOp, int) error {
				return func(op sinkOp, n int) error {
					if op == sinkAbort {
						return abortErr
					}

					return failAt(sinkPublished, 0)(op, n)
				}
			},
			published: 1, aborts: 1, uncertain: true, joined: true,
		},
		{
			name:      "Create",
			fault:     func(*memSink) func(sinkOp, int) error { return failAt(sinkCreate, 1) },
			published: 1, aborts: 0,
		},
		{
			name: "the head write of the new segment",
			fault: func(s *memSink) func(sinkOp, int) error {
				return func(op sinkOp, n int) error {
					if op == sinkWrite && len(s.files) == 2 {
						return errInjected
					}

					return nil
				}
			},
			published: 1, aborts: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			w, sink := newSeg(t, nil)
			segFrame(t, w, time.Second)
			sink.fault = tt.fault(sink)
			before := w.nextSeq

			err := w.AppendFrame(t.Context(), segAt(time.Hour), DirHostToEquipment, 1, blockTestFrame)
			require.ErrorIs(t, err, errInjected)
			assert.Equal(t, tt.uncertain, errors.Is(err, ErrPublishUncertain), "ErrPublishUncertain")
			assert.Len(t, sink.keys, tt.published, "segments published")
			assert.Equal(t, tt.aborts, sink.aborts(), "Abort calls")
			assert.Equal(t, before, w.nextSeq, "the seq counter is not rewound")
			sink.noMisuse(t)
			if tt.joined {
				require.ErrorIs(t, err, abortErr)
				var joined interface{ Unwrap() []error }
				require.ErrorAs(t, err, &joined)
				parts := joined.Unwrap()
				require.Len(t, parts, 2)
				require.ErrorIs(t, parts[0], ErrPublishUncertain, "the cause first")
				require.ErrorIs(t, parts[1], abortErr, "the Abort error after it")
			}

			calls := sink.calls
			require.ErrorIs(t, w.AppendFrame(t.Context(), segAt(time.Hour), DirHostToEquipment, 1, blockTestFrame), ErrWriterFailed)
			require.ErrorIs(t, w.Tick(t.Context(), segAt(2*time.Hour)), ErrWriterFailed)
			assert.Equal(t, err, w.Close(t.Context()), "Close returns the failure")
			assert.Equal(t, err, w.Close(t.Context()), "and again")
			assert.Equal(t, calls, sink.calls, "no sink call after the failure")
		})
	}
}

func TestSegmentWriterWriterFailure(t *testing.T) {
	t.Parallel()

	truncate := func(enc []byte) []byte { return enc[:len(enc)-1] }

	// A Writer error, whatever its sentinel, fails the SegmentWriter and aborts the open segment.
	w, sink := newSeg(t, nil)
	w.seg.w.encodedHook = truncate
	err := w.Rotate(t.Context())
	require.ErrorIs(t, err, ErrValidation)
	assert.Equal(t, 1, sink.aborts())
	assert.Empty(t, sink.keys)
	assert.Equal(t, err, w.Close(t.Context()))
	sink.noMisuse(t)

	// With SkipValidation the segment's Writer writes the damaged block, so the segment is published.
	w, sink = newSeg(t, func(o *SegmentWriterOptions) { o.SkipValidation = true })
	w.seg.w.encodedHook = truncate
	require.NoError(t, w.Rotate(t.Context()))
	assert.Len(t, sink.keys, 1)
	assert.Zero(t, sink.aborts())
}

func TestSegmentWriterCloseFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// fault fails the final Commit; sink is the sink it is set on.
		fault func(sink *memSink) func(sinkOp, int) error
		// published is the segments published; uncertain whether the error wraps ErrPublishUncertain.
		published int
		uncertain bool
	}{
		{"before publication", func(*memSink) func(sinkOp, int) error { return failAt(sinkCommit, 0) }, 0, false},
		{"after publication", func(*memSink) func(sinkOp, int) error { return failAt(sinkPublished, 0) }, 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			w, sink := newSeg(t, nil)
			segFrame(t, w, time.Second)
			sink.fault = tt.fault(sink)

			err := w.Close(t.Context())
			require.ErrorIs(t, err, errInjected)
			assert.Equal(t, tt.uncertain, errors.Is(err, ErrPublishUncertain), "ErrPublishUncertain")
			assert.Len(t, sink.keys, tt.published, "segments published")
			assert.Equal(t, 1, sink.aborts(), "one Abort of the failed commit")
			sink.noMisuse(t)

			calls := sink.calls
			assert.Equal(t, err, w.Close(t.Context()), "a second Close returns the same failure")
			assert.Equal(t, calls, sink.calls, "no sink call after the failure")
		})
	}

	t.Run("no seq left for stop, then the final commit fails", func(t *testing.T) {
		t.Parallel()

		w, sink := newSeg(t, nil)
		require.NoError(t, w.Rotate(t.Context()))
		w.nextSeq = format.MaxU64
		one := segMonoRecord(time.Hour, time.Hour)
		require.NoError(t, w.Append(t.Context(), &one), "the last seq")
		// The first Commit published the start's segment; fail the next one after publication.
		sink.fault = failAt(sinkPublished, sink.calls[sinkPublished])

		err := w.Close(t.Context())
		require.ErrorIs(t, err, ErrSeqOrder)
		require.ErrorIs(t, err, ErrPublishUncertain)
		var joined interface{ Unwrap() []error }
		require.ErrorAs(t, err, &joined)
		parts := joined.Unwrap()
		require.Len(t, parts, 2)
		require.ErrorIs(t, parts[0], ErrSeqOrder, "the missing stop first")
		require.ErrorIs(t, parts[1], ErrPublishUncertain, "the commit's failure after it")
		assert.Equal(t, 1, sink.aborts())
		assert.Len(t, sink.keys, 2)
		sink.noMisuse(t)

		calls := sink.calls
		assert.Equal(t, err, w.Close(t.Context()), "a second Close returns the same failure")
		assert.Equal(t, calls, sink.calls, "no sink call after the failure")
	})
}

// nilFileSink is a SegmentSink whose Create returns neither a file nor an error.
type nilFileSink struct{}

// Create returns no file and no error.
func (nilFileSink) Create(context.Context, SegmentInfo) (SegmentFile, error) {
	return nil, nil //nolint:nilnil // the contract violation under test
}

func TestNewSegmentWriterSinkFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		fault  func(sinkOp, int) error
		aborts int
	}{
		{"Create", failAt(sinkCreate, 0), 0},
		{"the header write", failAt(sinkWrite, 0), 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sink := newMemSink()
			sink.fault = tt.fault
			w, err := NewSegmentWriter(t.Context(), segOptions(sink))
			require.ErrorIs(t, err, errInjected)
			assert.Nil(t, w)
			assert.Equal(t, tt.aborts, sink.aborts(), "Abort calls")
			assert.Empty(t, sink.keys)
			sink.noMisuse(t)
		})
	}

	t.Run("a Create returning no file", func(t *testing.T) {
		t.Parallel()

		w, err := NewSegmentWriter(t.Context(), segOptions(nilFileSink{}))
		require.Error(t, err)
		assert.Nil(t, w)
	})
}
