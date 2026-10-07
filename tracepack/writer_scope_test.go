package tracepack

import (
	"bytes"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scopeTolerance is the clock_step_tolerance_ns of scopeClockMeta.
const scopeTolerance = 1_000

// scopeClockMeta returns repairTestMeta for a capture-clock segment of the hour blockTestHour,
// whose clock anchor starts at (blockTestHour, 0) and tolerates scopeTolerance of drift.
func scopeClockMeta() *PackMeta {
	m := repairTestMeta()
	m.TimeSource = TimeSourceCaptureClock
	m.CaptureOriginUTCNs, m.CaptureOriginMonoNs = new(blockTestHour), new(int64(0))
	m.ClockStepToleranceNs = new(uint64(scopeTolerance))

	return m
}

// newScopeClockWriter opens a Writer over out for scopeClockMeta that assigns seqs from next and detects clock steps,
// starting from anchor when it is set.
func newScopeClockWriter(t testing.TB, out *bytes.Buffer, next uint64, anchor *clockAnchor) *Writer {
	t.Helper()

	w, err := NewWriter(out, WriterOptions{
		Meta: scopeClockMeta(), AssignSeq: true, NextSeq: next, DetectClockSteps: true, anchor: anchor,
	})
	require.NoError(t, err)

	return w
}

// monoAt returns a data record of epoch 1 observed at wall clock wall and mono clock mono.
func monoAt(wall, mono int64) Record {
	r := testDataRecord(0, wall, 1)
	r.MonoNs = mono

	return r
}

// kindsOf returns the kind of every record of the finalized pack file, in seq order.
func kindsOf(t testing.TB, file []byte) []Kind {
	t.Helper()

	items := recordsOf(t, file)
	out := make([]Kind, 0, len(items))
	for _, it := range items {
		out = append(out, it.rec.Kind)
	}

	return out
}

// TestWriterRefusesARecordOfAnotherHour appends, to a segment that assigns seqs and detects clock steps,
// records of other UTC hours than the period's whose drift would also make each a clock step:
// each is refused before it takes a seq, so no seq moves, no clock-step record is written and the anchor stays,
// and the Writer goes on with the next record of the hour.
func TestWriterRefusesARecordOfAnotherHour(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	w := newScopeClockWriter(t, &buf, 10, nil)
	first := monoAt(blockTestHour+100, 100)
	require.NoError(t, w.Append(&first))

	next, anchor, pending := w.nextSeq, w.anchor(), w.block.summary.recordCount
	for _, ts := range []int64{blockTestHour + hourNs, blockTestHour - 1, math.MinInt64, math.MaxInt64} {
		r := monoAt(ts, 200)
		r.Seq = 99
		err := w.Append(&r)
		require.ErrorIs(t, err, ErrScopeBreach, "ts %d", ts)
		assert.Equal(t, next, w.nextSeq, "ts %d: no seq taken", ts)
		assert.Equal(t, uint64(99), r.Seq, "ts %d: no seq assigned", ts)
		assert.Equal(t, anchor, w.anchor(), "ts %d: the anchor unchanged", ts)
		assert.Equal(t, pending, w.block.summary.recordCount, "ts %d: the open block unchanged", ts)
		assert.Empty(t, w.blocks, "ts %d: no block closed, no clock-step written", ts)
	}

	second := monoAt(blockTestHour+300, 300)
	require.NoError(t, w.Append(&second), "the Writer is still usable")
	_, err := w.Close()
	require.NoError(t, err)

	assert.Equal(t, uint64(11), second.Seq, "the seq after the first record's")
	assert.Equal(t, []Kind{KindData, KindData}, kindsOf(t, buf.Bytes()), "no clock-step record")
}

// TestWriterRefusesAFirstRecordOfAnotherHour refuses the first record of a pack with caller seqs:
// the record's seq, seq_start, is still the one the first record must carry.
// The hour is checked after the pack metadata's commitments and before the seq.
func TestWriterRefusesAFirstRecordOfAnotherHour(t *testing.T) {
	t.Parallel()

	m := repairTestMeta()
	m.SeqStart = 5
	var buf bytes.Buffer
	w, err := NewWriter(&buf, WriterOptions{Meta: m})
	require.NoError(t, err)

	r := testDataRecord(5, blockTestHour+hourNs+1, 1)
	r.DecodeStatus = DecodeStatusOversized
	require.ErrorIs(t, w.Append(&r), ErrMetadataCommitment, "a pack without max_frame_len")
	r.DecodeStatus, r.Seq = DecodeStatusOK, 6
	require.ErrorIs(t, w.Append(&r), ErrScopeBreach, "before the seq, which is not seq_start")
	r.Seq = 5
	require.ErrorIs(t, w.Append(&r), ErrScopeBreach)
	r.TSUTCNs = blockTestHour + 1
	require.NoError(t, w.Append(&r), "seq_start is still the first seq")
	_, err = w.Close()
	require.NoError(t, err)
}

// TestWriterAcceptsTheWholeHourOfItsPeriod writes a segment whose period is ten minutes of an hour:
// a record of that hour outside the period is accepted, as a clock step within the hour puts it there,
// and a record a nanosecond outside the hour is refused.
func TestWriterAcceptsTheWholeHourOfItsPeriod(t *testing.T) {
	t.Parallel()

	m := repairTestMeta()
	m.PeriodStart, m.PeriodEnd = blockTestHour+10*hourNs/60, blockTestHour+20*hourNs/60
	var buf bytes.Buffer
	w, err := NewWriter(&buf, WriterOptions{Meta: m, AssignSeq: true})
	require.NoError(t, err)

	for _, ts := range []int64{blockTestHour + hourNs - 1, blockTestHour, m.PeriodEnd, m.PeriodStart - 1} {
		r := testDataRecord(0, ts, 1)
		require.NoError(t, w.Append(&r), "ts %d", ts)
	}
	for _, ts := range []int64{blockTestHour - 1, blockTestHour + hourNs} {
		r := testDataRecord(0, ts, 1)
		require.ErrorIs(t, w.Append(&r), ErrScopeBreach, "ts %d", ts)
	}
	_, err = w.Close()
	require.NoError(t, err)
	assert.Len(t, recordsOf(t, buf.Bytes()), 4)
}

// extractScopeMeta returns repairTestMeta for an extract, whose period may be anything and whose records may span hours.
func extractScopeMeta(start, end int64) *PackMeta {
	m := repairTestMeta()
	m.PackRole, m.ExtractFilter, m.ScopeGeneration = PackRoleExtract, new("filter"), nil
	m.PeriodStart, m.PeriodEnd = start, end

	return m
}

// TestWriterExtractTakesAnyHour writes extracts whose period is empty, inside one hour or across hours,
// each with records of several hours: an extract has no scope, so nothing is refused,
// and each hour's records get blocks of their own.
func TestWriterExtractTakesAnyHour(t *testing.T) {
	t.Parallel()

	h := blockTestHour
	for _, period := range [][2]int64{{0, 0}, {h, h + hourNs}, {h, h + 3*hourNs}, {h + 1, h}} {
		var buf bytes.Buffer
		w, err := NewWriter(&buf, WriterOptions{Meta: extractScopeMeta(period[0], period[1]), AssignSeq: true})
		require.NoError(t, err, "period %v", period)

		for _, ts := range []int64{h - 1, h, h + hourNs, h + 2*hourNs + 5, math.MinInt64} {
			r := testDataRecord(0, ts, 1)
			require.NoError(t, w.Append(&r), "period %v, ts %d", period, ts)
		}
		_, err = w.Close()
		require.NoError(t, err)
		assert.Len(t, mustOpen(t, buf.Bytes(), ReaderOptions{}).Blocks(), 5, "period %v: one block per hour", period)
	}
}

// TestNewWriterRefusesAPeriodOutsideOneHour checks the period of every pack role the scope rule covers:
// an empty period and one not inside one UTC hour are refused with ErrScopeBreach before anything is written,
// and a period that fills or starts or ends an hour, the first and last hours of int64 nanoseconds included, is accepted.
// An extract takes every one of them.
func TestNewWriterRefusesAPeriodOutsideOneHour(t *testing.T) {
	t.Parallel()

	h := blockTestHour
	first, last := (minKeyHour+1)*hourNs, maxKeyHour*hourNs // the end of the first hour, the start of the last
	refused := [][2]int64{
		{h, h}, {h + 1, h}, {0, 0}, {math.MaxInt64, math.MinInt64},
		{h, h + hourNs + 1}, {h - 1, h + 1}, {h + hourNs - 1, h + 2*hourNs},
		{math.MinInt64, first + 1}, {last - 1, math.MaxInt64}, {math.MinInt64, math.MaxInt64},
	}
	accepted := [][2]int64{
		{h, h + hourNs}, {h, h + 1}, {h + hourNs - 1, h + hourNs}, {-hourNs, 0}, {-1, 0},
		{math.MinInt64, first}, {math.MinInt64, math.MinInt64 + 1}, {last, math.MaxInt64}, {math.MaxInt64 - 1, math.MaxInt64},
	}
	roles := map[string]func(m *PackMeta){
		"segment": func(*PackMeta) {},
		"archive": archiveMeta,
		"repair":  func(m *PackMeta) { m.PackRole, m.Supersedes = PackRoleRepair, []UUID{{0xD0}} },
	}

	for name, role := range roles {
		for _, p := range refused {
			m := repairTestMeta()
			role(m)
			m.PeriodStart, m.PeriodEnd = p[0], p[1]
			var buf bytes.Buffer
			w, err := NewWriter(&buf, WriterOptions{Meta: m})
			require.ErrorIs(t, err, ErrScopeBreach, "%s, period %v", name, p)
			assert.Nil(t, w)
			assert.Zero(t, buf.Len(), "%s, period %v: nothing written", name, p)

			_, err = NewWriter(&buf, WriterOptions{Meta: m, allowScopeBreach: true})
			require.NoError(t, err, "%s, period %v, without the scope checks", name, p)
		}
		for _, p := range accepted {
			m := repairTestMeta()
			role(m)
			m.PeriodStart, m.PeriodEnd = p[0], p[1]
			_, err := NewWriter(&bytes.Buffer{}, WriterOptions{Meta: m})
			require.NoError(t, err, "%s, period %v", name, p)
		}
	}
	for _, p := range append(refused, accepted...) {
		_, err := NewWriter(&bytes.Buffer{}, WriterOptions{Meta: extractScopeMeta(p[0], p[1])})
		require.NoError(t, err, "extract, period %v", p)
	}
}

// TestWriterStartsFromASeededClockAnchor starts a Writer from an anchor other than the capture origin:
// a record that agrees with the seeded anchor is no clock step, though its drift against the origin is,
// and the anchor in force after Close, that of the clock step the Writer wrote, seeds the capture's next pack,
// where a record that agrees with it is not detected as a step again.
// Without the seed, the next pack measures that record against the origin and writes a step.
func TestWriterStartsFromASeededClockAnchor(t *testing.T) {
	t.Parallel()

	h := blockTestHour
	seed := clockAnchor{wall: h + 5_000_000, mono: 7_000_000}
	var buf bytes.Buffer
	w := newScopeClockWriter(t, &buf, 0, &seed)

	agreeing := monoAt(seed.wall+1_000, seed.mono+1_000) // drift 0 against the seed, -2 ms against the origin
	require.NoError(t, w.Append(&agreeing))
	assert.Equal(t, seed, w.anchor(), "no step: the anchor is the seed")
	stepped := monoAt(seed.wall+2_000+50_000, seed.mono+2_000) // drift +50 µs against the seed
	require.NoError(t, w.Append(&stepped))
	next, err := w.Close()
	require.NoError(t, err)
	assert.Equal(t, []Kind{KindData, KindTransportEvent, KindData}, kindsOf(t, buf.Bytes()))
	after := clockAnchor{wall: stepped.TSUTCNs, mono: stepped.MonoNs}
	assert.Equal(t, after, w.anchor(), "the anchor in force after Close is the step's")

	later := monoAt(stepped.TSUTCNs+1_000, stepped.MonoNs+1_000) // drift 0 against the step
	tests := []struct {
		name   string
		anchor *clockAnchor
		want   []Kind
	}{
		{"seeded with the closing anchor", new(w.anchor()), []Kind{KindData}},
		{"from the capture origin", nil, []Kind{KindTransportEvent, KindData}},
	}
	for _, tt := range tests {
		var out bytes.Buffer
		nw := newScopeClockWriter(t, &out, next, tt.anchor)
		r := later
		require.NoError(t, nw.Append(&r), tt.name)
		_, err := nw.Close()
		require.NoError(t, err, tt.name)
		assert.Equal(t, tt.want, kindsOf(t, out.Bytes()), tt.name)
	}
}

// TestWriterKeepsItsAnchorAfterAFailure fails a Writer after it wrote a clock step:
// the anchor in force, the step's, is still readable, so the capture's next pack can start from it.
func TestWriterKeepsItsAnchorAfterAFailure(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	w := newScopeClockWriter(t, &buf, 0, nil)
	stepped := monoAt(blockTestHour+50_000, 0) // drift +50 µs against the origin
	require.NoError(t, w.Append(&stepped))
	want := clockAnchor{wall: stepped.TSUTCNs, mono: stepped.MonoNs}
	require.Equal(t, want, w.anchor())

	// A truncated body fails the validation of the next block written.
	w.encodedHook = func(enc []byte) []byte { return enc[:len(enc)-1] }
	require.ErrorIs(t, w.Flush(), ErrValidation)
	r := monoAt(stepped.TSUTCNs+1_000, stepped.MonoNs+1_000)
	require.ErrorIs(t, w.Append(&r), ErrWriterFailed)
	assert.Equal(t, want, w.anchor(), "the anchor of the failed Writer")
}

// TestWriterIgnoresTheAnchorWithoutDetection seeds the anchor of a Writer that does not detect clock steps:
// the seed is not taken, the Writer holds no anchor, and a record far from the seed and from the origin is stored alone.
func TestWriterIgnoresTheAnchorWithoutDetection(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	w, err := NewWriter(&buf, WriterOptions{
		Meta: scopeClockMeta(), AssignSeq: true, anchor: &clockAnchor{wall: blockTestHour + 5_000_000, mono: 7_000_000},
	})
	require.NoError(t, err)
	assert.Equal(t, clockAnchor{}, w.anchor())

	r := monoAt(blockTestHour+50_000_000, 1)
	require.NoError(t, w.Append(&r))
	_, err = w.Close()
	require.NoError(t, err)
	assert.Equal(t, clockAnchor{}, w.anchor())
	assert.Equal(t, []Kind{KindData}, kindsOf(t, buf.Bytes()), "no clock step")
}
