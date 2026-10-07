package tracepack

import (
	"bytes"
	"context"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// srcPack writes steps into a pack id of capture of the reader source tests, with writeRepairPack:
// finalized, or with open left without a footer;
// its pack metadata has quality_evaluated true and is then changed by meta when set.
func srcPack(t testing.TB, id, capture UUID, open bool, meta func(m *PackMeta), steps []footerTestStep) []byte {
	t.Helper()

	return withIDs(t, writeRepairPack(t, CodecZstd, func(m *PackMeta) {
		m.QualityEvaluated = true
		if meta != nil {
			meta(m)
		}
	}, open, steps).file, id, capture)
}

// srcLookupSteps returns the records of captureLow's lookup pack:
// the primary at 12 and a same-key primary at 14, with nothing between them that replies,
// so a complete lookup of 12 is TxUnmatched.
func srcLookupSteps() []footerTestStep {
	return txBlock(txRecord(12, nil), txNote(13, nil), txRecord(14, nil))
}

// srcLookupPack writes captureLow's lookup pack seg0, finalized.
func srcLookupPack(t testing.TB) []byte {
	t.Helper()

	return srcPack(t, seg0, captureLow, false, nil, srcLookupSteps())
}

// srcUncleanPack writes a segment id of captureHigh holding one stop-unclean boundary at 7 in epoch 1,
// whose gap lies inside memTestHour; with open it has no footer.
func srcUncleanPack(t testing.TB, id UUID, open bool) []byte {
	t.Helper()

	return srcPack(t, id, captureHigh, open, nil,
		txBlock(txBoundaryAt(t, 7, 1, BoundaryKindStopUnclean, new(blockTestHour+10), new(blockTestHour+20))))
}

// newSrc returns the reader source over the readers of view and evidence, failing the test on error.
func newSrc(t testing.TB, complete bool, view, evidence [][]byte) PackSource {
	t.Helper()

	s, err := NewReaderSource(openAll(t, view...), ReaderSourceOptions{Complete: complete, Evidence: openAll(t, evidence...)})
	require.NoError(t, err)

	return s
}

// observe observes capture over [from, to) of s, failing the test on error; the observation is closed when the test ends.
func observe(t testing.TB, s PackSource, capture UUID, from, to int64) Observation {
	t.Helper()

	o, err := s.Observe(t.Context(), capture, from, to)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, o.Close()) })

	return o
}

// scopeOf returns the scope of hour of o, failing the test on error.
func scopeOf(t testing.TB, o Observation, hour int64) SourceScope {
	t.Helper()

	sc, err := o.Scope(t.Context(), hour)
	require.NoError(t, err)

	return sc
}

// evidenceOf returns the evidence of o, failing the test on error.
func evidenceOf(t testing.TB, o Observation) CaptureEvidence {
	t.Helper()

	e, err := o.Evidence(t.Context())
	require.NoError(t, err)

	return e
}

// hourBarriers returns the barriers of o that meet memTestHour, failing the test on error.
func hourBarriers(t testing.TB, o Observation) []Boundary {
	t.Helper()

	bs, err := o.Barriers(t.Context(), memTestHour*hourNs, (memTestHour+1)*hourNs)
	require.NoError(t, err)

	return bs
}

// readerIDs returns the pack_id of each reader of sc.
func readerIDs(sc SourceScope) []UUID {
	return packIDs(sc.Readers)
}

// TestNewReaderSourceComplete looks up captureLow's primary at 12, which has no reply,
// over the same pack with and without Complete:
// without it the scope is not indexed and the evidence partial, so the lookup is TxIncomplete, never TxUnmatched;
// with it the scope is indexed and the evidence whole, and the lookup is TxUnmatched.
func TestNewReaderSourceComplete(t *testing.T) {
	t.Parallel()

	file := srcLookupPack(t)
	for _, complete := range []bool{false, true} {
		s := newSrc(t, complete, [][]byte{file}, nil)
		o := observe(t, s, captureLow, memTestHour, memTestHour+2)
		sc := scopeOf(t, o, memTestHour)
		assert.Equal(t, []UUID{seg0}, readerIDs(sc), "complete %v", complete)
		assert.Equal(t, complete, sc.Indexed, "complete %v", complete)
		assert.False(t, sc.Conflicted)
		empty := scopeOf(t, o, memTestHour+1)
		assert.Equal(t, SourceScope{Indexed: complete}, empty, "complete %v: an hour without packs", complete)
		assert.Equal(t, !complete, evidenceOf(t, o).Partial, "complete %v", complete)

		res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 1})
		require.NoError(t, err)
		if complete {
			assert.Equal(t, TxUnmatched, res.Outcome)
			assert.Empty(t, res.Gaps)

			continue
		}
		assert.Equal(t, TxIncomplete, res.Outcome)
		assert.Contains(t, gapReasons(res.Gaps), TxGapCold)
		assert.Contains(t, gapReasons(res.Gaps), TxGapEvidence)
	}
}

// TestReaderSourceBarrierOfAnotherCapture looks up captureLow's primary at 12 under Complete,
// beside captureHigh's stop-unclean boundary, whose gap meets the hour read:
// left out, the lookup is TxUnmatched;
// given, it is a TxGapBarrier gap;
// given in a pack without a footer, the boundary is unknown and every capture's evidence partial,
// so the lookup is not TxUnmatched.
func TestReaderSourceBarrierOfAnotherCapture(t *testing.T) {
	t.Parallel()

	a := srcLookupPack(t)
	tests := []struct {
		name    string
		view    [][]byte
		outcome TxOutcome
		gaps    []TxGapReason
	}{
		{name: "the boundary left out", view: [][]byte{a}, outcome: TxUnmatched},
		{name: "the boundary given", view: [][]byte{a, srcUncleanPack(t, seg1, false)}, outcome: TxIncomplete, gaps: []TxGapReason{TxGapBarrier}},
		{name: "the boundary's pack without a footer", view: [][]byte{a, srcUncleanPack(t, seg1, true)}, outcome: TxIncomplete, gaps: []TxGapReason{TxGapEvidence}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := newSrc(t, true, tt.view, nil)
			res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 1})
			require.NoError(t, err)
			assert.Equal(t, tt.outcome, res.Outcome)
			assert.Equal(t, tt.gaps, nilIfEmpty(gapReasons(res.Gaps)))
		})
	}
}

// TestReaderSourceEvidenceReaders gives captureHigh's stop-unclean boundary as an Evidence reader,
// a segment whose registration was rejected:
// it is a barrier of captureLow's lookup, and captureHigh's evidence holds it,
// while no scope holds the segment:
// captureLow's view is unchanged,
// and captureHigh, without a view reader, has only empty scopes, the segment's own not indexed.
func TestReaderSourceEvidenceReaders(t *testing.T) {
	t.Parallel()

	s := newSrc(t, true, [][]byte{srcLookupPack(t)}, [][]byte{srcUncleanPack(t, seg1, false)})
	res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 1})
	require.NoError(t, err)
	assert.Equal(t, TxIncomplete, res.Outcome)
	assert.Equal(t, []TxGapReason{TxGapBarrier}, gapReasons(res.Gaps))
	assert.Equal(t, []TxScope{{Hour: memTestHour, Indexed: true, Packs: []UUID{seg0}}}, res.Searched)

	low := observe(t, s, captureLow, memTestHour, memTestHour+1)
	assert.Equal(t, []UUID{seg0}, readerIDs(scopeOf(t, low, memTestHour)))
	assert.Equal(t, CaptureEvidence{}, evidenceOf(t, low), "captureLow's evidence holds nothing of the rejected segment")

	high := observe(t, s, captureHigh, memTestHour, memTestHour+2)
	assert.Equal(t, SourceScope{}, scopeOf(t, high, memTestHour), "no scope holds an evidence reader, and its scope is not indexed")
	assert.Equal(t, SourceScope{Indexed: true}, scopeOf(t, high, memTestHour+1), "an hour of no evidence reader")
	want := txUnclean(captureHigh, 7, 1, new(blockTestHour+10), new(blockTestHour+20))
	e := evidenceOf(t, high)
	assert.Equal(t, EndStoppedUnclean, e.End)
	assert.Equal(t, []Boundary{want}, e.Boundaries)
	assert.False(t, e.Partial)
	assert.Equal(t, []Boundary{want}, hourBarriers(t, high), "the barriers of an evidence-only capture")
	next, err := high.Barriers(t.Context(), (memTestHour+1)*hourNs, (memTestHour+2)*hourNs)
	require.NoError(t, err)
	assert.Empty(t, next, "the gap does not meet the next hour")
}

// TestReaderSourceEvidenceScopeNotIndexed gives, under Complete, a segment of captureLow whose registration was rejected,
// as an Evidence reader in the hour of captureLow's lookup pack,
// holding a reply at 13, where the view holds an annotation:
// the scope still reads only the view, but it is not indexed,
// so the lookup of 12 is a TxGapCold gap and never TxUnmatched, as it would be through the catalog.
// Another hour of captureLow, and the same hour of captureHigh, stay indexed.
func TestReaderSourceEvidenceScopeNotIndexed(t *testing.T) {
	t.Parallel()

	rejected := srcPack(t, seg1, captureLow, false, nil, txBlock(txReply(13, nil)))
	s := newSrc(t, true, [][]byte{srcLookupPack(t)}, [][]byte{rejected})
	low := observe(t, s, captureLow, memTestHour, memTestHour+2)
	sc := scopeOf(t, low, memTestHour)
	assert.Equal(t, []UUID{seg0}, readerIDs(sc), "the view holds no evidence reader")
	assert.False(t, sc.Indexed, "the evidence reader's scope")
	assert.Equal(t, SourceScope{Indexed: true}, scopeOf(t, low, memTestHour+1), "another hour of the capture")
	assert.False(t, evidenceOf(t, low).Partial, "every footer is used")
	high := observe(t, s, captureHigh, memTestHour, memTestHour+1)
	assert.Equal(t, SourceScope{Indexed: true}, scopeOf(t, high, memTestHour), "the hour of another capture")

	res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 1})
	require.NoError(t, err)
	assert.Equal(t, TxIncomplete, res.Outcome)
	assert.Equal(t, []TxGapReason{TxGapCold}, gapReasons(res.Gaps))
	assert.Equal(t, []TxScope{{Hour: memTestHour, Packs: []UUID{seg0}}}, res.Searched)
}

// TestNewReaderSourceRejectsEvidencePeriod gives, under Complete,
// the rejected segment of TestReaderSourceEvidenceScopeNotIndexed,
// with its period started in the hour before and its reply still in the hour of the lookup,
// or with an empty period:
// grouped by its start, its scope would leave the hour of the lookup indexed,
// and the lookup of 12 could be TxUnmatched although the segment holds its reply,
// so NewReaderSource refuses it with an error wrapping ErrInvalidQuery.
func TestNewReaderSourceRejectsEvidencePeriod(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name   string
		period func(m *PackMeta)
	}{
		{name: "started in the hour before", period: func(m *PackMeta) { m.PeriodStart -= hourNs }},
		{name: "empty", period: func(m *PackMeta) { m.PeriodEnd = m.PeriodStart }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rejected := srcPack(t, seg1, captureLow, false, tt.period, txBlock(txReply(13, nil)))
			s, err := NewReaderSource(openAll(t, srcLookupPack(t)), ReaderSourceOptions{Complete: true, Evidence: openAll(t, rejected)})
			require.ErrorIs(t, err, ErrInvalidQuery)
			require.ErrorContains(t, err, "Evidence[0] has the period")
			assert.Nil(t, s)
		})
	}
}

// TestReaderSourceMissingFooter gives, under Complete, a pack without a used footer
// that the view replaces, of another capture, or as an Evidence reader:
// each makes captureLow's evidence partial, so its lookup is not TxUnmatched;
// with every footer used, the evidence is whole.
// The Evidence reader's scope, the one read, is also not indexed.
func TestReaderSourceMissingFooter(t *testing.T) {
	t.Parallel()

	member := srcPack(t, memA, captureLow, false, func(m *PackMeta) { generationMeta(m, setA, 1, 1, []UUID{seg0}) }, srcLookupSteps())
	tests := []struct {
		name           string
		view, evidence [][]byte
		partial        bool
		gaps           []TxGapReason
	}{
		{name: "every footer used", view: [][]byte{member, srcLookupPack(t)}},
		{
			name: "a replaced pack", view: [][]byte{member, srcPack(t, seg0, captureLow, true, nil, srcLookupSteps())},
			partial: true, gaps: []TxGapReason{TxGapEvidence},
		},
		{
			name: "another capture's pack", view: [][]byte{member, srcPack(t, seg1, captureHigh, true, nil, txBlock(txRecord(3, nil)))},
			partial: true, gaps: []TxGapReason{TxGapEvidence},
		},
		{
			name: "an evidence reader", view: [][]byte{member}, evidence: [][]byte{srcPack(t, seg1, captureLow, true, nil, txBlock(txRecord(3, nil)))},
			partial: true, gaps: []TxGapReason{TxGapCold, TxGapEvidence},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := newSrc(t, true, tt.view, tt.evidence)
			o := observe(t, s, captureLow, memTestHour, memTestHour+1)
			assert.Equal(t, []UUID{memA}, readerIDs(scopeOf(t, o, memTestHour)), "the view reads the generation")
			assert.Equal(t, tt.partial, evidenceOf(t, o).Partial)

			res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 1})
			require.NoError(t, err)
			if !tt.partial {
				assert.Equal(t, TxUnmatched, res.Outcome)

				return
			}
			assert.Equal(t, TxIncomplete, res.Outcome)
			assert.Equal(t, tt.gaps, gapReasons(res.Gaps))
		})
	}
}

// TestReaderSourceConflictedScope gives two generations of one rank in captureLow's scope, both committed by being given:
// the scope is conflicted, without readers, and the lookup reads none of its packs,
// which explains the missing primary.
func TestReaderSourceConflictedScope(t *testing.T) {
	t.Parallel()

	a := srcPack(t, memA, captureLow, false, func(m *PackMeta) { generationMeta(m, setA, 1, 2, []UUID{seg0}) }, srcLookupSteps())
	b := srcPack(t, memB, captureLow, false, func(m *PackMeta) { generationMeta(m, setB, 1, 2, []UUID{seg0}) }, srcLookupSteps())
	for _, complete := range []bool{false, true} {
		s := newSrc(t, complete, [][]byte{a, b, srcLookupPack(t)}, nil)
		o := observe(t, s, captureLow, memTestHour, memTestHour+1)
		assert.Equal(t, SourceScope{Indexed: complete, Conflicted: true}, scopeOf(t, o, memTestHour), "complete %v", complete)

		res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 1})
		require.NoError(t, err)
		assert.Equal(t, TxIncomplete, res.Outcome)
		want := []TxGapReason{TxGapConflicted, TxGapNoKey}
		if !complete {
			want = append([]TxGapReason{TxGapCold}, want...)
		}
		assert.Equal(t, want, gapReasons(res.Gaps), "complete %v: the conflicted scope explains the missing primary", complete)
		assert.Empty(t, res.Searched, "complete %v", complete)
	}
}

// TestReaderSourceCommitsGivenPacks gives a generation, and a patch, beside the segment they replace:
// each counts as committed, so the view reads it in the segment's place.
func TestReaderSourceCommitsGivenPacks(t *testing.T) {
	t.Parallel()

	member := srcPack(t, memA, captureLow, false, func(m *PackMeta) { generationMeta(m, setA, 1, 1, []UUID{seg0}) }, srcLookupSteps())
	patch := srcPack(t, patP1, captureLow, false, func(m *PackMeta) {
		m.PackRole, m.Supersedes = PackRoleRepair, []UUID{seg0}
	}, srcLookupSteps())
	for _, tt := range []struct {
		name string
		pack []byte
		want UUID
	}{{name: "a generation", pack: member, want: memA}, {name: "a patch", pack: patch, want: patP1}} {
		s := newSrc(t, true, [][]byte{srcLookupPack(t), tt.pack}, nil)
		o := observe(t, s, captureLow, memTestHour, memTestHour+1)
		assert.Equal(t, []UUID{tt.want}, readerIDs(scopeOf(t, o, memTestHour)), tt.name)
	}
}

// TestNewReaderSourceRejectsScope gives a scope that ActiveView rejects for another reason than a conflict,
// a period of two hours:
// NewReaderSource fails with ActiveView's error, which is not an invalid query.
func TestNewReaderSourceRejectsScope(t *testing.T) {
	t.Parallel()

	twoHours := srcPack(t, seg1, captureLow, false, func(m *PackMeta) { m.PeriodEnd += hourNs }, srcLookupSteps())
	s, err := NewReaderSource(openAll(t, srcLookupPack(t), twoHours), ReaderSourceOptions{Complete: true})
	require.Error(t, err)
	require.ErrorContains(t, err, "active view")
	require.NotErrorIs(t, err, ErrInvalidQuery)
	require.NotErrorIs(t, err, ErrViewConflicted)
	assert.Nil(t, s)
}

// TestNewReaderSourceRejectsReaders gives readers NewReaderSource refuses, over readers and Evidence together:
// each fails it with an error wrapping ErrInvalidQuery.
func TestNewReaderSourceRejectsReaders(t *testing.T) {
	t.Parallel()

	a := mustOpen(t, srcLookupPack(t), ReaderOptions{})
	aAgain := mustOpen(t, srcLookupPack(t), ReaderOptions{})
	b := mustOpen(t, srcUncleanPack(t, seg1, false), ReaderOptions{})
	foreign := mustOpen(t, srcPack(t, seg2, captureLow, false, func(m *PackMeta) { m.ToolID = "other" }, srcLookupSteps()), ReaderOptions{})
	repair := mustOpen(t, srcPack(t, patP1, captureLow, false, func(m *PackMeta) {
		m.PackRole, m.Supersedes = PackRoleRepair, []UUID{seg0}
	}, srcLookupSteps()), ReaderOptions{})
	extract := mustOpen(t, srcPack(t, seg3, captureLow, false, func(m *PackMeta) {
		m.PackRole, m.ScopeGeneration, m.ExtractFilter = PackRoleExtract, nil, new("seq 12-14")
	}, srcLookupSteps()), ReaderOptions{})
	tests := []struct {
		name           string
		view, evidence []*Reader
		want           string
	}{
		{name: "a nil view reader", view: []*Reader{a, nil}, want: "readers[1] is nil"},
		{name: "a nil evidence reader", view: []*Reader{a}, evidence: []*Reader{nil}, want: "Evidence[0] is nil"},
		{name: "a view reader twice", view: []*Reader{a, b, a}, want: "readers[0] and readers[2] are the same Reader"},
		{name: "an evidence reader twice", evidence: []*Reader{b, b}, want: "Evidence[0] and Evidence[1] are the same Reader"},
		{name: "one reader in both", view: []*Reader{a}, evidence: []*Reader{b, a}, want: "readers[0] and Evidence[1] are the same Reader"},
		{name: "a pack_id twice in the view", view: []*Reader{a, aAgain}, want: "readers[0] and readers[1] hold the same pack_id"},
		{name: "a pack_id in both", view: []*Reader{a}, evidence: []*Reader{aAgain}, want: "readers[0] and Evidence[0] hold the same pack_id"},
		{name: "two tools in the view", view: []*Reader{a, foreign}, want: `readers[0] holds tool_id "tool", readers[1] tool_id "other"`},
		{name: "a foreign evidence reader", view: []*Reader{a}, evidence: []*Reader{foreign}, want: `readers[0] holds tool_id "tool", Evidence[0] tool_id "other"`},
		{name: "two tools in the evidence", evidence: []*Reader{b, foreign}, want: `Evidence[0] holds tool_id "tool", Evidence[1] tool_id "other"`},
		{name: "an extract in the view", view: []*Reader{a, extract}, want: "readers[1] is a pack of role extract"},
		{name: "a repair as evidence", view: []*Reader{a}, evidence: []*Reader{repair}, want: "Evidence[0] is a pack of role repair"},
		{name: "an extract as evidence", evidence: []*Reader{extract}, want: "Evidence[0] is a pack of role extract"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s, err := NewReaderSource(tt.view, ReaderSourceOptions{Complete: true, Evidence: tt.evidence})
			require.ErrorIs(t, err, ErrInvalidQuery)
			require.ErrorContains(t, err, tt.want)
			assert.Nil(t, s)
		})
	}

	// A repair is a view reader, and a segment or archive an evidence reader.
	_, err := NewReaderSource([]*Reader{a, repair}, ReaderSourceOptions{Evidence: []*Reader{b}})
	require.NoError(t, err)
}

// TestNewReaderSourceWithoutReaders builds sources over no readers at all:
// every scope is empty, indexed only with Complete, and the evidence partial only without it.
func TestNewReaderSourceWithoutReaders(t *testing.T) {
	t.Parallel()

	for _, complete := range []bool{false, true} {
		s, err := NewReaderSource(nil, ReaderSourceOptions{Complete: complete})
		require.NoError(t, err)
		o := observe(t, s, captureLow, memTestHour, memTestHour+1)
		assert.Equal(t, SourceScope{Indexed: complete}, scopeOf(t, o, memTestHour))
		assert.Equal(t, CaptureEvidence{Partial: !complete}, evidenceOf(t, o))
		assert.Empty(t, hourBarriers(t, o))
	}
}

// TestReaderSourceAnswersAreCopies changes every value an observation returned:
// the scope's Readers, the evidence's boundaries, their gap bounds and closures, the barriers;
// then it asks again, of the same observation and of a new one:
// every answer is the one before the changes.
func TestReaderSourceAnswersAreCopies(t *testing.T) {
	t.Parallel()

	high := srcPack(t, seg1, captureHigh, false, nil, txBlock(
		txBoundaryAt(t, 7, 1, BoundaryKindStopUnclean, new(blockTestHour+10), new(blockTestHour+20)),
		txBoundaryAt(t, 8, 1, BoundaryKindStop, nil, nil),
	))
	s := newSrc(t, true, [][]byte{srcLookupPack(t), high}, nil)
	first := observe(t, s, captureHigh, memTestHour, memTestHour+1)
	sc, e, bs := scopeOf(t, first, memTestHour), evidenceOf(t, first), hourBarriers(t, first)
	require.Equal(t, []UUID{seg1}, readerIDs(sc))
	require.Len(t, e.Boundaries, 2)
	require.Equal(t, []EpochClosure{{Epoch: 1, CloseSeq: 8}}, e.Closures, "the clean stop closes its epoch")
	require.Len(t, bs, 1)
	wantScope, wantEvidence, wantBarriers := cloneScope(sc), cloneEvidence(e), cloneBoundaries(bs)

	sc.Readers[0] = nil
	e.Boundaries[0].Seq++
	*e.Boundaries[0].GapStart, *e.Boundaries[0].GapEnd = 0, 0
	e.Closures[0].CloseSeq++
	bs[0].Kind = BoundaryKindStop
	*bs[0].GapStart, *bs[0].GapEnd = 0, 0

	for _, o := range []Observation{first, observe(t, s, captureHigh, memTestHour, memTestHour+1)} {
		assert.Equal(t, wantScope, scopeOf(t, o, memTestHour))
		assert.Equal(t, wantEvidence, evidenceOf(t, o))
		assert.Equal(t, wantBarriers, hourBarriers(t, o))
	}
}

// cloneScope returns sc with a fresh Readers slice.
func cloneScope(sc SourceScope) SourceScope {
	sc.Readers = append([]*Reader(nil), sc.Readers...)

	return sc
}

// cloneEvidence returns a deep copy of e.
func cloneEvidence(e CaptureEvidence) CaptureEvidence {
	e.Boundaries = cloneBoundaries(e.Boundaries)
	e.Closures = append([]EpochClosure(nil), e.Closures...)

	return e
}

// TestReaderSourceObserveArguments checks the arguments of Observe, Scope and Barriers, and ctx:
// each refused as the store source refuses it; Close closes nothing, as often as it is called.
func TestReaderSourceObserveArguments(t *testing.T) {
	t.Parallel()

	s := newSrc(t, true, [][]byte{srcLookupPack(t)}, nil)
	for _, tt := range []struct {
		name     string
		capture  UUID
		from, to int64
		want     string
	}{
		{name: "zero capture", from: 0, to: 1, want: "the capture_id is zero"},
		{name: "empty", capture: captureLow, from: 1, to: 1, want: "are empty"},
		{name: "reversed", capture: captureLow, from: 2, to: 1, want: "are empty"},
		{name: "below the hours", capture: captureLow, from: MinTxHour - 1, to: 0, want: "are not within"},
		{name: "above the hours", capture: captureLow, from: 0, to: MaxTxHour + 2, want: "are not within"},
	} {
		o, err := s.Observe(t.Context(), tt.capture, tt.from, tt.to)
		require.ErrorContains(t, err, "tracepack: reader source: ", tt.name)
		require.ErrorContains(t, err, tt.want, tt.name)
		assert.Nil(t, o, tt.name)
	}
	o, err := s.Observe(t.Context(), captureLow, MinTxHour, MaxTxHour+1)
	require.NoError(t, err, "the widest hours")
	require.NoError(t, o.Close())

	o = observe(t, s, captureLow, memTestHour, memTestHour+2)
	for _, hour := range []int64{memTestHour - 1, memTestHour + 2} {
		_, err := o.Scope(t.Context(), hour)
		require.ErrorContains(t, err, "outside the observation's hours")
	}
	from, to := memTestHour*hourNs, (memTestHour+2)*hourNs
	for _, r := range [][2]int64{{from, from}, {from - 1, to}, {from, to + 1}, {to, from}} {
		_, err := o.Barriers(t.Context(), r[0], r[1])
		require.ErrorContains(t, err, "is empty or not within", "range %v", r)
	}
	_, err = o.Barriers(t.Context(), from, to)
	require.NoError(t, err, "the whole hours")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = s.Observe(ctx, captureLow, memTestHour, memTestHour+1)
	require.ErrorIs(t, err, context.Canceled)
	_, err = o.Scope(ctx, memTestHour)
	require.ErrorIs(t, err, context.Canceled)
	_, err = o.Evidence(ctx)
	require.ErrorIs(t, err, context.Canceled)
	_, err = o.Barriers(ctx, from, to)
	require.ErrorIs(t, err, context.Canceled)

	require.NoError(t, o.Close())
	require.NoError(t, o.Close())
	sc := scopeOf(t, o, memTestHour)
	require.Equal(t, []UUID{seg0}, readerIDs(sc))
	assert.Len(t, iterate(t, sc.Readers[0], Query{}).items, len(srcLookupSteps()), "Close closes no reader: the source's reader still reads its records")
}

// TestTxKeyOf builds the key of a primary from its record's time: its hour is HourOf's, before 1970 too.
func TestTxKeyOf(t *testing.T) {
	t.Parallel()

	for _, ns := range []int64{blockTestHour, blockTestHour + hourNs - 1, 0, -1, -hourNs, -hourNs - 1} {
		ts := time.Unix(0, ns)
		assert.Equal(t, TxKey{Capture: captureLow, Seq: 7, Hour: hourOf(ns)}, TxKeyOf(captureLow, 7, ts), "ts %d", ns)
	}
	assert.Equal(t, memTestHour, TxKeyOf(captureLow, 0, time.Unix(0, blockTestHour).In(time.FixedZone("x", 5*3600))).Hour,
		"the hour is UTC's, whatever the time's location")
}

// e2eFrame returns blockTestFrame as an S1F<function> frame, its W bit set by w,
// with blockTestFrame's SessionID and System Bytes.
func e2eFrame(function byte, w bool) []byte {
	p := slices.Clone(blockTestFrame)
	p[fieldByte6Off] = 0x01
	if w {
		p[fieldByte6Off] |= 0x80
	}
	p[fieldFunctionOff] = function

	return p
}

// TestReaderSourceEndToEnd records a capture with a SegmentWriter into a NewDirSink directory:
// an S1F1 W primary late in one hour and its S1F2 reply early in the next.
// It opens every committed segment, and a lookup over NewReaderSource with Complete finds the reply across the hour.
// The segments of each hour then merge without a refused input,
// and the lookup over the segments and the archives together, each archive's generation committed by being given, still finds it.
func TestReaderSourceEndToEnd(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	sink, err := NewDirSink(root, dirTestPrefix)
	require.NoError(t, err)
	opts := segOptions(sink)
	opts.Capture.ToolID = dirTestTool
	opts.Now = nowAt(segAt(time.Hour + time.Minute))
	w, err := NewSegmentWriter(t.Context(), opts)
	require.NoError(t, err)
	primaryAt, replyAt := segAt(time.Hour-20*time.Second), segAt(time.Hour)
	require.Equal(t, HourOf(primaryAt)+1, HourOf(replyAt), "the reply lies in the next hour")
	require.NoError(t, w.AppendFrame(t.Context(), primaryAt, DirHostToEquipment, 1, e2eFrame(1, true)))
	require.NoError(t, w.AppendFrame(t.Context(), replyAt, DirEquipmentToHost, 1, e2eFrame(2, false)))
	require.NoError(t, w.Close(t.Context()))

	keys, partial := dirFiles(t, root)
	assert.Empty(t, partial)
	require.Len(t, keys, 3, "the start's period, the primary's, and the reply's in the next hour")
	byHour := make(map[int64][][]byte)
	readers := make([]*Reader, 0, len(keys))
	primarySeq := uint64(math.MaxUint64)
	for _, key := range keys {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(key)))
		require.NoError(t, err)
		r := mustOpen(t, data, ReaderOptions{})
		readers = append(readers, r)
		byHour[hourOf(r.Header().Meta.PeriodStart)] = append(byHour[hourOf(r.Header().Meta.PeriodStart)], data)
		for _, it := range iterate(t, r, Query{}).items {
			if it.rec.Kind == KindData && it.rec.Dir == DirHostToEquipment {
				primarySeq = it.rec.Seq
			}
		}
	}
	require.NotEqual(t, uint64(math.MaxUint64), primarySeq, "the primary was recorded")

	key := TxKeyOf(w.CaptureID(), primarySeq, primaryAt)
	lookup := func(readers []*Reader) TxResult {
		t.Helper()

		src, err := NewReaderSource(readers, ReaderSourceOptions{Complete: true})
		require.NoError(t, err)
		res, err := FindTransaction(t.Context(), src, key, TxOptions{MaxScopes: 2})
		require.NoError(t, err)

		return res
	}
	res := lookup(readers)
	require.Equal(t, TxMatched, res.Outcome, "gaps %v", res.Gaps)
	assert.Empty(t, res.Gaps)
	var valid []TxRecord
	for _, v := range res.Records {
		if v.Valid {
			valid = append(valid, v)
		}
	}
	require.Len(t, valid, 1)
	assert.Equal(t, replyAt.UnixNano(), valid[0].Record.TSUTCNs, "the reply")
	assert.Equal(t, HourOf(replyAt), valid[0].Hour, "read in the next hour's scope")
	assert.Len(t, res.Searched, 2)

	archives := slices.Clone(readers)
	for hour, files := range byHour {
		v := mergeView(t, nil, files...)
		require.Equal(t, hour, v.Hour)
		var buf bytes.Buffer
		_, err := Merge(t.Context(), &buf, v, inputsOf(t, v, files...), mergeOpts())
		require.NoError(t, err, "hour %d", hour)
		archives = append(archives, mustOpen(t, buf.Bytes(), ReaderOptions{}))
	}
	res = lookup(archives)
	require.Equal(t, TxMatched, res.Outcome, "gaps %v", res.Gaps)
	require.Len(t, res.Searched, 2)
	segments := packIDs(readers)
	for _, sc := range res.Searched {
		require.Len(t, sc.Packs, 1, "hour %d: the generation alone", sc.Hour)
		assert.NotContains(t, segments, sc.Packs[0], "hour %d", sc.Hour)
	}
}
