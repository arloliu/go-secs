package tracepack

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// memScopeKey names one scope of a memSource: a capture in a UTC hour.
type memScopeKey struct {
	capture UUID
	hour    int64
}

// memScope is what a memSource holds of one scope.
type memScope struct {
	// registered holds the packs the catalog admitted, by pack_id: an indexed scope's view is computed from them.
	registered map[UUID][]byte
	// staged holds the packs present in storage that the catalog did not admit, by pack_id:
	// rejected segments and late uploads, which only a listing view reads.
	staged  map[UUID][]byte
	commits CommitSet
}

// memEvidence is the registration history of one capture's per-capture evidence:
// what the catalog recorded, independent of the packs present.
// Each registration is the statistics it recorded, in registration order;
// nil for a pack whose footer is not used.
// Observe folds them with AddPackEvidence, and the safety check reads them as recorded.
type memEvidence struct {
	stats []*PackStats
}

// memSource is a PackSource over packs held in memory, for one tool, modelling a catalog and its storage
// (the tracepack storage specification §5).
//
// A scope is indexed or not; an indexed scope's view is the active view of its registered packs,
// a scope not indexed is read through its listing view, the active view of every pack present, staged ones included.
// Per-capture evidence and barriers are a registration history, kept apart from the packs:
// evidence survives the removal of its pack,
// and a test records evidence of a pack it never admits, as for a rejected registration.
// Observe fixes every answer, so a test can change the source after it and between the observation's calls.
//
// Its helpers lock mu, so the hooks, which run without it held, can change the source.
type memSource struct {
	mu       sync.Mutex
	scopes   map[memScopeKey]*memScope
	indexed  map[memScopeKey]bool
	evidence map[UUID]*memEvidence

	// duringObserve, when set, runs inside Observe after the views are fixed,
	// before Observe checks that every scope it fixed as not indexed still is.
	duringObserve func()
	// beforeScope, beforeEvidence and beforeBarriers, when set, run before the observation's Scope, Evidence and Barriers answer;
	// an error they return is returned instead of the answer.
	beforeScope    func(ctx context.Context, hour int64) error
	beforeEvidence func(ctx context.Context) error
	beforeBarriers func(ctx context.Context, from, to int64) error
	// observeErr and closeErr, when set, are returned by every Observe and every Close.
	observeErr error
	closeErr   error

	// observes counts the calls of Observe, scopeCalls the calls of Scope by hour, closes the calls of Close.
	observes   int
	scopeCalls map[int64]int
	closes     int
}

var _ PackSource = (*memSource)(nil)

// memObservation is an Observation of a memSource: the descriptor Observe fixed.
type memObservation struct {
	src      *memSource
	from, to int64
	// scopes holds each hour's fixed view: its packs' bytes in view order and whether it is indexed.
	scopes   map[int64]memFixedScope
	evidence CaptureEvidence
	// barriers holds every stop-unclean boundary of the tool recorded when Observe ran.
	barriers []Boundary
	closed   bool
}

var _ Observation = (*memObservation)(nil)

// memFixedScope is one scope as an observation fixed it.
type memFixedScope struct {
	files   [][]byte
	indexed bool
}

// newMemSource returns an empty memSource.
func newMemSource() *memSource {
	return &memSource{
		scopes: map[memScopeKey]*memScope{}, indexed: map[memScopeKey]bool{}, evidence: map[UUID]*memEvidence{},
		scopeCalls: map[int64]int{},
	}
}

// cloneBoundaries returns a deep copy of bs, nil for an empty bs.
func cloneBoundaries(bs []Boundary) []Boundary {
	if len(bs) == 0 {
		return nil
	}
	out := make([]Boundary, len(bs))
	for i := range bs {
		out[i] = cloneBoundary(bs[i])
	}

	return out
}

// scope returns the scope of capture in hour, created empty when absent; s.mu is held.
func (s *memSource) scope(capture UUID, hour int64) *memScope {
	k := memScopeKey{capture: capture, hour: hour}
	sc := s.scopes[k]
	if sc == nil {
		sc = &memScope{registered: map[UUID][]byte{}, staged: map[UUID][]byte{}, commits: CommitSet{}}
		s.scopes[k] = sc
	}

	return sc
}

// captureEvidence returns the registration history of capture, created empty when absent; s.mu is held.
func (s *memSource) captureEvidence(capture UUID) *memEvidence {
	e := s.evidence[capture]
	if e == nil {
		e = &memEvidence{}
		s.evidence[capture] = e
	}

	return e
}

// addPack admits file to the scope of its capture in hour,
// and records its footer's evidence as a registration of a segment does
// (the tracepack storage specification §5, Per capture).
// As a shortcut, it takes every pack of a later generation or of a replacement set for the output of a merge,
// which adds no evidence;
// a test records a converter archive's evidence with registerEvidence.
// The shortcut is this source's registration policy, not AddPackEvidence's, which folds whatever statistics it is given.
func (s *memSource) addPack(t testing.TB, hour int64, file []byte) {
	t.Helper()

	h := mustOpen(t, file, ReaderOptions{}).Header()
	s.mu.Lock()
	s.scope(h.CaptureID, hour).registered[h.PackID] = bytes.Clone(file)
	s.mu.Unlock()
	if m := h.Meta; (m.ScopeGeneration != nil && *m.ScopeGeneration > 0) || m.ReplacementSetID != nil {
		return
	}
	s.registerEvidence(t, file)
}

// stagePack puts file in storage in the scope of its capture in hour without admitting it or recording its evidence.
func (s *memSource) stagePack(t testing.TB, hour int64, file []byte) {
	t.Helper()

	h := mustOpen(t, file, ReaderOptions{}).Header()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scope(h.CaptureID, hour).staged[h.PackID] = bytes.Clone(file)
}

// removePack removes the pack id of capture from hour's scope, admitted or staged; its recorded evidence stays.
func (s *memSource) removePack(capture UUID, hour int64, id UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sc := s.scope(capture, hour)
	delete(sc.registered, id)
	delete(sc.staged, id)
}

// commit adds id to the commit set of capture's scope in hour.
func (s *memSource) commit(capture UUID, hour int64, id UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scope(capture, hour).commits[id] = struct{}{}
}

// setIndexed makes the catalog index capture's scope in hour, or not.
func (s *memSource) setIndexed(capture UUID, hour int64, indexed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.indexed[memScopeKey{capture: capture, hour: hour}] = indexed
}

// registerEvidence records the per-capture evidence of file's footer as a registration would, as the footer claims it:
// its F-5 statistics, from which Observe takes every capture-boundary entry and every epoch's close_seq.
// A pack whose footer is not used marks its capture's evidence partial.
func (s *memSource) registerEvidence(t testing.TB, file []byte) {
	t.Helper()

	r := mustOpen(t, file, ReaderOptions{})
	var stats *PackStats
	if st, ok := r.Stats(); ok {
		stats = &st
	}
	s.record(UUID(r.hdr.CaptureID), stats)
}

// addBoundary records b as a capture-boundary entry of its capture's evidence, by hand.
func (s *memSource) addBoundary(b Boundary) {
	s.record(b.Capture, &PackStats{Boundaries: []Boundary{cloneBoundary(b)}})
}

// addClosure records that close_seq closes epoch of capture, by hand.
func (s *memSource) addClosure(capture UUID, epoch uint32, closeSeq uint64) {
	s.record(capture, &PackStats{Epochs: []EpochStats{{Epoch: epoch, CloseSeq: &closeSeq}}})
}

// setPartial marks capture's evidence partial.
func (s *memSource) setPartial(capture UUID) {
	s.record(capture, nil)
}

// record appends the registration stats to capture's evidence; nil marks it partial.
func (s *memSource) record(capture UUID, stats *PackStats) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.captureEvidence(capture)
	e.stats = append(e.stats, stats)
}

// counts returns the calls of Observe, of Scope by hour, and of Close so far.
func (s *memSource) counts() (observes int, scopes map[int64]int, closes int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.observes, maps.Clone(s.scopeCalls), s.closes
}

// Observe fixes the views of capture's scopes over [from, to), the capture's evidence and the tool's barriers.
// A scope fixed as not indexed that is indexed once duringObserve ran fails the observation.
func (s *memSource) Observe(ctx context.Context, capture UUID, from, to int64) (Observation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.observes++
	if s.observeErr != nil {
		s.mu.Unlock()

		return nil, s.observeErr
	}
	o, err := s.fix(ctx, capture, from, to)
	hook := s.duringObserve
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}

	if hook != nil {
		hook()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for h, sc := range o.scopes {
		if !sc.indexed && s.indexed[memScopeKey{capture: capture, hour: h}] {
			return nil, fmt.Errorf("memSource: scope %d became indexed while it was observed", h)
		}
	}

	return o, nil
}

// fix returns the observation of capture over [from, to) from the source's current state; s.mu is held.
func (s *memSource) fix(ctx context.Context, capture UUID, from, to int64) (*memObservation, error) {
	if from >= to {
		return nil, fmt.Errorf("memSource: empty hours [%d, %d)", from, to)
	}
	o := &memObservation{src: s, from: from, to: to, scopes: map[int64]memFixedScope{}}
	for h := from; h < to; h++ {
		k := memScopeKey{capture: capture, hour: h}
		indexed := s.indexed[k]
		var present map[UUID][]byte
		var commits CommitSet
		if sc := s.scopes[k]; sc != nil {
			present, commits = maps.Clone(sc.registered), sc.commits
			if !indexed {
				maps.Copy(present, sc.staged)
			}
		}
		files, err := viewFiles(ctx, present, commits)
		if err != nil {
			return nil, fmt.Errorf("memSource: hour %d: %w", h, err)
		}
		o.scopes[h] = memFixedScope{files: files, indexed: indexed}
	}

	o.evidence = s.foldEvidence(capture)
	for _, id := range slices.SortedFunc(maps.Keys(s.evidence), func(a, b UUID) int { return bytes.Compare(a[:], b[:]) }) {
		for _, b := range s.foldEvidence(id).Boundaries {
			if b.Kind == BoundaryKindStopUnclean {
				o.barriers = append(o.barriers, b)
			}
		}
	}

	return o, nil
}

// foldEvidence returns capture's evidence:
// its registrations folded by AddPackEvidence in registration order; s.mu is held.
func (s *memSource) foldEvidence(capture UUID) CaptureEvidence {
	var out CaptureEvidence
	if e := s.evidence[capture]; e != nil {
		for _, st := range e.stats {
			AddPackEvidence(&out, st)
		}
	}

	return out
}

// viewFiles returns the files of the active view of the packs present under commits, in view order; nil for no packs.
func viewFiles(ctx context.Context, present map[UUID][]byte, commits CommitSet) ([][]byte, error) {
	if len(present) == 0 {
		return nil, nil
	}
	infos := make([]PackInfo, 0, len(present))
	for _, id := range slices.SortedFunc(maps.Keys(present), func(a, b UUID) int { return bytes.Compare(a[:], b[:]) }) {
		f := present[id]
		r, err := Open(ctx, bytes.NewReader(f), int64(len(f)), ReaderOptions{})
		if err != nil {
			return nil, err
		}
		infos = append(infos, r.Info())
	}
	v, err := ActiveView(infos, commits)
	if err != nil {
		return nil, err
	}
	files := make([][]byte, len(v.Packs))
	for i, id := range v.Packs {
		files[i] = bytes.Clone(present[id])
	}

	return files, nil
}

// Scope opens a fresh Reader for each pack of hour's fixed view.
func (o *memObservation) Scope(ctx context.Context, hour int64) (SourceScope, error) {
	if err := o.check(ctx); err != nil {
		return SourceScope{}, err
	}
	if hour < o.from || hour >= o.to {
		return SourceScope{}, fmt.Errorf("memSource: hour %d outside [%d, %d)", hour, o.from, o.to)
	}
	o.src.mu.Lock()
	o.src.scopeCalls[hour]++
	hook := o.src.beforeScope
	o.src.mu.Unlock()
	if hook != nil {
		if err := hook(ctx, hour); err != nil {
			return SourceScope{}, err
		}
	}

	sc := o.scopes[hour]
	out := SourceScope{Readers: make([]*Reader, len(sc.files)), Indexed: sc.indexed}
	for i, f := range sc.files {
		r, err := Open(ctx, bytes.NewReader(f), int64(len(f)), ReaderOptions{})
		if err != nil {
			return SourceScope{}, err
		}
		out.Readers[i] = r
	}

	return out, nil
}

// Evidence returns a deep copy of the fixed evidence.
func (o *memObservation) Evidence(ctx context.Context) (CaptureEvidence, error) {
	if err := o.check(ctx); err != nil {
		return CaptureEvidence{}, err
	}
	o.src.mu.Lock()
	hook := o.src.beforeEvidence
	o.src.mu.Unlock()
	if hook != nil {
		if err := hook(ctx); err != nil {
			return CaptureEvidence{}, err
		}
	}

	e := o.evidence
	e.Boundaries = cloneBoundaries(e.Boundaries)
	e.Closures = slices.Clone(e.Closures)

	return e, nil
}

// Barriers returns deep copies of the fixed barriers whose gap interval meets [from, to), an inverted one meeting every range.
func (o *memObservation) Barriers(ctx context.Context, from, to int64) ([]Boundary, error) {
	if err := o.check(ctx); err != nil {
		return nil, err
	}
	o.src.mu.Lock()
	hook := o.src.beforeBarriers
	o.src.mu.Unlock()
	if hook != nil {
		if err := hook(ctx, from, to); err != nil {
			return nil, err
		}
	}

	var out []Boundary
	for _, b := range o.barriers {
		if barrierMeets(&b, from, to) {
			out = append(out, cloneBoundary(b))
		}
	}

	return out, nil
}

// Close counts the call and returns the source's closeErr; it never waits.
func (o *memObservation) Close() error {
	o.src.mu.Lock()
	defer o.src.mu.Unlock()
	o.src.closes++
	o.closed = true

	return o.src.closeErr
}

// check returns ctx's error, or an error once the observation is closed.
func (o *memObservation) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	o.src.mu.Lock()
	defer o.src.mu.Unlock()
	if o.closed {
		return errors.New("memSource: observation closed")
	}

	return nil
}

// memTestHour is the UTC hour of blockTestHour, the hour of the test packs' records.
var memTestHour = hourOf(blockTestHour)

// packIDs returns the pack_id of each reader, in order.
func packIDs(readers []*Reader) []UUID {
	out := make([]UUID, len(readers))
	for i, r := range readers {
		out[i] = UUID(r.hdr.PackID)
	}

	return out
}

// TestMemSourceFixedAtObserve changes the source after Observe, then between the observation's calls:
// packs added, removed and staged, the scope evicted, evidence and barriers recorded;
// every answer stays the one Observe fixed.
func TestMemSourceFixedAtObserve(t *testing.T) {
	t.Parallel()

	s := newMemSource()
	s.addPack(t, memTestHour, planTestPack(t, seg0, captureLow, false, blockAt(0, 1, 2)))
	s.setIndexed(captureLow, memTestHour, true)
	gapStart := blockTestHour
	s.addBoundary(Boundary{Capture: captureHigh, Seq: 9, Kind: BoundaryKindStopUnclean, GapStart: &gapStart})

	o, err := s.Observe(t.Context(), captureLow, memTestHour, memTestHour+2)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, o.Close()) })
	change := func() {
		s.addPack(t, memTestHour, planTestPack(t, seg1, captureLow, false, blockAt(10, 3)))
		s.stagePack(t, memTestHour+1, planTestPack(t, seg2, captureLow, false, blockAt(20, 4)))
		s.removePack(captureLow, memTestHour, seg0)
		s.setIndexed(captureLow, memTestHour, false)
		s.addBoundary(Boundary{Capture: captureLow, Seq: 5, Kind: BoundaryKindStop})
		s.addBoundary(Boundary{Capture: captureHigh, Seq: 10, Kind: BoundaryKindStopUnclean})
		s.addClosure(captureLow, 1, 2)
		s.setPartial(captureLow)
	}
	change()
	s.beforeScope = func(context.Context, int64) error {
		change()

		return nil
	}

	for range 2 {
		sc, err := o.Scope(t.Context(), memTestHour)
		require.NoError(t, err)
		assert.True(t, sc.Indexed)
		assert.Equal(t, []UUID{seg0}, packIDs(sc.Readers))

		sc, err = o.Scope(t.Context(), memTestHour+1)
		require.NoError(t, err)
		assert.False(t, sc.Indexed)
		assert.Empty(t, sc.Readers)

		e, err := o.Evidence(t.Context())
		require.NoError(t, err)
		assert.Equal(t, CaptureEvidence{}, e)

		b, err := o.Barriers(t.Context(), blockTestHour, blockTestHour+hourNs)
		require.NoError(t, err)
		assert.Equal(t, []Boundary{{Capture: captureHigh, Seq: 9, Kind: BoundaryKindStopUnclean, GapStart: &gapStart}}, b)
	}
	_, scopes, _ := s.counts()
	assert.Equal(t, map[int64]int{memTestHour: 2, memTestHour + 1: 2}, scopes)
}

// TestMemSourceListingView fixes a scope with an admitted pack and a staged one, indexed and not:
// the indexed view reads the admitted pack only, the listing view both.
func TestMemSourceListingView(t *testing.T) {
	t.Parallel()

	s := newMemSource()
	s.addPack(t, memTestHour, planTestPack(t, seg0, captureLow, false, blockAt(0, 1, 2)))
	s.stagePack(t, memTestHour, planTestPack(t, seg1, captureLow, false, blockAt(10, 3)))

	for _, indexed := range []bool{true, false} {
		s.setIndexed(captureLow, memTestHour, indexed)
		o, err := s.Observe(t.Context(), captureLow, memTestHour, memTestHour+1)
		require.NoError(t, err)
		sc, err := o.Scope(t.Context(), memTestHour)
		require.NoError(t, err)
		assert.Equal(t, indexed, sc.Indexed)
		want := []UUID{seg0, seg1}
		if indexed {
			want = want[:1]
		}
		assert.Equal(t, want, packIDs(sc.Readers), "indexed %v", indexed)
		require.NoError(t, o.Close())
	}
}

// TestMemSourceCommits fixes a scope holding a segment and the archive of a generation compacted from it:
// the view reads the segment until the generation is committed, the archive after.
func TestMemSourceCommits(t *testing.T) {
	t.Parallel()

	s := newMemSource()
	s.setIndexed(captureLow, memTestHour, true)
	s.addPack(t, memTestHour, planTestPack(t, seg0, captureLow, false, blockAt(0, 1, 2)))
	archive := writeRepairPack(t, CodecZstd, func(m *PackMeta) { generationMeta(m, setA, 1, 1, []UUID{seg0}) }, false, blockAt(0, 1, 2))
	s.addPack(t, memTestHour, withIDs(t, archive.file, memA, captureLow))

	for _, want := range []UUID{seg0, memA} {
		o, err := s.Observe(t.Context(), captureLow, memTestHour, memTestHour+1)
		require.NoError(t, err)
		sc, err := o.Scope(t.Context(), memTestHour)
		require.NoError(t, err)
		assert.Equal(t, []UUID{want}, packIDs(sc.Readers))
		require.NoError(t, o.Close())
		s.commit(captureLow, memTestHour, setA)
	}
}

// TestMemSourceEvidence records the evidence of a pack's footer and of a walked pack:
// boundaries in ascending seq, the smallest close_seq per epoch, the end state of the last end boundary,
// kept after the pack is removed; the walked pack marks the evidence partial.
func TestMemSourceEvidence(t *testing.T) {
	t.Parallel()

	s := newMemSource()
	rich := planTestPack(t, seg0, captureLow, false, richFooterSteps(t))
	s.addPack(t, memTestHour, rich)
	s.removePack(captureLow, memTestHour, seg0)

	o, err := s.Observe(t.Context(), captureLow, memTestHour, memTestHour+1)
	require.NoError(t, err)
	e, err := o.Evidence(t.Context())
	require.NoError(t, err)
	require.NoError(t, o.Close())
	gapStart, gapEnd := blockTestHour+100, blockTestHour+200
	assert.Equal(t, CaptureEvidence{
		End: EndStopped,
		Boundaries: []Boundary{
			{Capture: captureLow, Seq: 10, Kind: BoundaryKindStart, TS: blockTestHour + 50, Epoch: 0},
			{Capture: captureLow, Seq: 20, Kind: BoundaryKindGap, TS: blockTestHour + 300, Epoch: 0, GapStart: &gapStart, GapEnd: &gapEnd},
			{Capture: captureLow, Seq: 31, Kind: BoundaryKindStop, TS: blockTestHour + hourNs + 4, Epoch: 3},
		},
		Closures: []EpochClosure{{Epoch: 1, CloseSeq: 15}, {Epoch: 3, CloseSeq: 31}},
	}, e)

	s.registerEvidence(t, planTestPack(t, seg1, captureLow, true, blockAt(0, 40)))
	o, err = s.Observe(t.Context(), captureLow, memTestHour, memTestHour+1)
	require.NoError(t, err)
	e, err = o.Evidence(t.Context())
	require.NoError(t, err)
	require.NoError(t, o.Close())
	assert.True(t, e.Partial)
}

// TestMemSourceEvidenceOutOfOrder registers, for one capture, a footer with a clean stop after a footer with a stop-unclean
// of a lower seq, both boundaries with gap bounds, and two close_seqs of one epoch;
// then registers the first footer again and adds a merge's archive holding a boundary:
// the end is stopped-unclean, although the clean stop has the larger seq;
// each epoch keeps its smallest close_seq; the boundaries come in ascending seq, each once, none from the archive;
// and Barriers returns only the stop-unclean boundary.
func TestMemSourceEvidenceOutOfOrder(t *testing.T) {
	t.Parallel()

	h := blockTestHour
	g1, g2, g3, g4 := h+1, h+2, h+3, h+4
	boundary := func(seq uint64, ts int64, epoch uint32, kind BoundaryKind, gapStart, gapEnd int64) footerTestStep {
		return footerTestStep{rec: testEventRecord(t, seq, ts, epoch, &TransportEvent{
			Event: EventCaptureBoundary, BoundaryKind: new(kind), GapStart: &gapStart, GapEnd: &gapEnd,
		})}
	}
	closeAt := func(seq uint64, ts int64, epoch uint32) footerTestStep {
		return footerTestStep{rec: testEventRecord(t, seq, ts, epoch, &TransportEvent{Event: EventSocketClose})}
	}
	late := planTestPack(t, seg0, captureLow, false, []footerTestStep{
		{rec: testDataRecord(40, h+40, 2)}, closeAt(45, h+45, 2), boundary(50, h+50, 3, BoundaryKindStop, g1, g2),
	})
	early := planTestPack(t, seg1, captureLow, false, []footerTestStep{
		{rec: testDataRecord(20, h+20, 2)}, closeAt(25, h+25, 2), boundary(30, h+30, 3, BoundaryKindStopUnclean, g3, g4),
	})
	s := newMemSource()
	s.registerEvidence(t, late)
	s.registerEvidence(t, early)
	s.registerEvidence(t, late)
	archive := writeRepairPack(t, CodecZstd, func(m *PackMeta) { generationMeta(m, setA, 1, 1, []UUID{seg0}) }, false,
		[]footerTestStep{boundary(60, h+60, 3, BoundaryKindGap, g1, g2)})
	s.addPack(t, memTestHour, withIDs(t, archive.file, memA, captureLow))

	o, err := s.Observe(t.Context(), captureLow, memTestHour, memTestHour+1)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, o.Close()) })
	e, err := o.Evidence(t.Context())
	require.NoError(t, err)
	unclean := Boundary{Capture: captureLow, Seq: 30, Kind: BoundaryKindStopUnclean, TS: h + 30, Epoch: 3, GapStart: &g3, GapEnd: &g4}
	assert.Equal(t, CaptureEvidence{
		End: EndStoppedUnclean,
		Boundaries: []Boundary{
			unclean,
			{Capture: captureLow, Seq: 50, Kind: BoundaryKindStop, TS: h + 50, Epoch: 3, GapStart: &g1, GapEnd: &g2},
		},
		Closures: []EpochClosure{{Epoch: 2, CloseSeq: 25}, {Epoch: 3, CloseSeq: 50}},
	}, e)

	b, err := o.Barriers(t.Context(), math.MinInt64, math.MaxInt64)
	require.NoError(t, err)
	assert.Equal(t, []Boundary{unclean}, b)
}

// TestMemSourceRejectedRegistrations stages a converter archive and a segment in a scope the catalog does not index,
// rejected, with their evidence recorded (the tracepack storage specification §5, Per capture):
// the listing view reads both, and the evidence holds their boundaries and closures.
// Evidence rebuilt by folding the statistics of the scope's packs in another order is the same.
// A merge's archive admitted by addPack adds no evidence, by the source's registration policy,
// although AddPackEvidence folds its statistics like any others.
func TestMemSourceRejectedRegistrations(t *testing.T) {
	t.Parallel()

	h := blockTestHour
	gapStart, gapEnd := h+100, h+200
	boundary := func(seq uint64, epoch uint32, kind BoundaryKind, gap bool) footerTestStep {
		ev := &TransportEvent{Event: EventCaptureBoundary, BoundaryKind: new(kind)}
		if gap {
			ev.GapStart, ev.GapEnd = &gapStart, &gapEnd
		}

		return footerTestStep{rec: testEventRecord(t, seq, h+int64(seq), epoch, ev)}
	}
	closeAt := func(seq uint64, epoch uint32) footerTestStep {
		return footerTestStep{rec: testEventRecord(t, seq, h+int64(seq), epoch, &TransportEvent{Event: EventSocketClose})}
	}
	converter := writeRepairPack(t, CodecZstd, func(m *PackMeta) {
		generationMeta(m, setA, 1, 1, nil)
		m.CompactionLevel = 0
	}, false, []footerTestStep{
		boundary(1, 0, BoundaryKindStart, false), {rec: testDataRecord(2, h+2, 0)}, closeAt(3, 0), boundary(4, 1, BoundaryKindStop, false),
	})
	converterFile := withIDs(t, converter.file, memA, captureLow)
	segment := planTestPack(t, seg1, captureLow, false, []footerTestStep{
		{rec: testDataRecord(20, h+20, 2)}, boundary(21, 2, BoundaryKindGap, true), closeAt(22, 2),
	})

	s := newMemSource()
	for _, f := range [][]byte{converterFile, segment} {
		s.stagePack(t, memTestHour, f)
		s.registerEvidence(t, f)
	}
	s.commit(captureLow, memTestHour, setA)
	merged := writeRepairPack(t, CodecZstd, func(m *PackMeta) { generationMeta(m, setB, 1, 1, []UUID{seg2}) }, false,
		[]footerTestStep{boundary(40, 3, BoundaryKindGap, false)})
	mergedFile := withIDs(t, merged.file, memB, captureLow)
	s.setIndexed(captureLow, memTestHour+1, true)
	s.addPack(t, memTestHour+1, mergedFile)

	o, err := s.Observe(t.Context(), captureLow, memTestHour, memTestHour+1)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, o.Close()) })
	sc, err := o.Scope(t.Context(), memTestHour)
	require.NoError(t, err)
	assert.False(t, sc.Indexed)
	require.Equal(t, []UUID{memA, seg1}, packIDs(sc.Readers))
	e, err := o.Evidence(t.Context())
	require.NoError(t, err)
	want := CaptureEvidence{
		End: EndStopped,
		Boundaries: []Boundary{
			{Capture: captureLow, Seq: 1, Kind: BoundaryKindStart, TS: h + 1, Epoch: 0},
			{Capture: captureLow, Seq: 4, Kind: BoundaryKindStop, TS: h + 4, Epoch: 1},
			{Capture: captureLow, Seq: 21, Kind: BoundaryKindGap, TS: h + 21, Epoch: 2, GapStart: &gapStart, GapEnd: &gapEnd},
		},
		Closures: []EpochClosure{{Epoch: 0, CloseSeq: 3}, {Epoch: 1, CloseSeq: 4}, {Epoch: 2, CloseSeq: 22}},
	}
	assert.Equal(t, want, e)

	var rebuilt CaptureEvidence
	for _, r := range slices.Backward(sc.Readers) {
		st, ok := r.Stats()
		require.True(t, ok)
		AddPackEvidence(&rebuilt, &st)
	}
	assert.Equal(t, want, rebuilt)

	st, ok := mustOpen(t, mergedFile, ReaderOptions{}).Stats()
	require.True(t, ok)
	AddPackEvidence(&rebuilt, &st)
	assert.Len(t, rebuilt.Boundaries, 4, "the helper folds a merge's statistics")
}

// TestMemSourceBarriers fixes stop-unclean boundaries of two captures, one-sided, bounded and inverted:
// Barriers returns those whose gap interval meets the range, gap_end inclusive, the range's end exclusive,
// and the inverted one, gap_start after gap_end, for every range.
func TestMemSourceBarriers(t *testing.T) {
	t.Parallel()

	s := newMemSource()
	at := func(v int64) *int64 { return &v }
	bounded := Boundary{Capture: captureHigh, Seq: 1, Kind: BoundaryKindStopUnclean, GapStart: at(100), GapEnd: at(200)}
	open := Boundary{Capture: captureLow, Seq: 2, Kind: BoundaryKindStopUnclean, GapStart: at(500)}
	reversed := Boundary{Capture: captureHigh, Seq: 4, Kind: BoundaryKindStopUnclean, GapStart: at(300), GapEnd: at(250)}
	s.addBoundary(bounded)
	s.addBoundary(open)
	s.addBoundary(reversed)
	s.addBoundary(Boundary{Capture: captureLow, Seq: 3, Kind: BoundaryKindGap, GapStart: at(0), GapEnd: at(1000)})
	o, err := s.Observe(t.Context(), captureLow, 0, 1)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, o.Close()) })

	tests := []struct {
		from, to int64
		want     []Boundary
	}{
		{from: 0, to: 100, want: []Boundary{reversed}},
		{from: 0, to: 101, want: []Boundary{bounded, reversed}},
		{from: 200, to: 300, want: []Boundary{bounded, reversed}},
		{from: 201, to: 500, want: []Boundary{reversed}},
		{from: 201, to: 501, want: []Boundary{open, reversed}},
		{from: 150, to: 1 << 40, want: []Boundary{open, bounded, reversed}},
	}
	for _, tt := range tests {
		got, err := o.Barriers(t.Context(), tt.from, tt.to)
		require.NoError(t, err)
		assert.Equal(t, tt.want, got, "[%d, %d)", tt.from, tt.to)
	}
}

// TestMemSourceOwnership mutates every value an observation returned, then asks again:
// the answers are unchanged, and each Readers slice and boundary pointer is fresh.
func TestMemSourceOwnership(t *testing.T) {
	t.Parallel()

	s := newMemSource()
	s.addPack(t, memTestHour, planTestPack(t, seg0, captureLow, false, blockAt(0, 1)))
	gapStart := int64(7)
	s.addBoundary(Boundary{Capture: captureLow, Seq: 1, Kind: BoundaryKindStopUnclean, GapStart: &gapStart})
	s.addClosure(captureLow, 1, 1)
	o, err := s.Observe(t.Context(), captureLow, memTestHour, memTestHour+1)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, o.Close()) })

	sc, err := o.Scope(t.Context(), memTestHour)
	require.NoError(t, err)
	sc.Readers[0] = nil
	e, err := o.Evidence(t.Context())
	require.NoError(t, err)
	*e.Boundaries[0].GapStart = 99
	e.Boundaries[0].Seq = 99
	e.Closures[0].CloseSeq = 99
	b, err := o.Barriers(t.Context(), 0, 10)
	require.NoError(t, err)
	*b[0].GapStart = 99

	sc, err = o.Scope(t.Context(), memTestHour)
	require.NoError(t, err)
	assert.Equal(t, []UUID{seg0}, packIDs(sc.Readers))
	e, err = o.Evidence(t.Context())
	require.NoError(t, err)
	assert.Equal(t, uint64(1), e.Boundaries[0].Seq)
	assert.Equal(t, int64(7), *e.Boundaries[0].GapStart)
	assert.Equal(t, uint64(1), e.Closures[0].CloseSeq)
	b, err = o.Barriers(t.Context(), 0, 10)
	require.NoError(t, err)
	assert.Equal(t, int64(7), *b[0].GapStart)
	assert.Equal(t, int64(7), gapStart, "the source keeps its own copy")
}

// TestMemSourceObserveErrors observes a scope that becomes indexed during Observe, an empty range, and a scope outside the range:
// each fails; a closed observation answers nothing, and Close stays idempotent.
func TestMemSourceObserveErrors(t *testing.T) {
	t.Parallel()

	s := newMemSource()
	s.duringObserve = func() { s.setIndexed(captureLow, memTestHour+1, true) }
	_, err := s.Observe(t.Context(), captureLow, memTestHour, memTestHour+2)
	require.ErrorContains(t, err, "became indexed")
	s.duringObserve = nil

	_, err = s.Observe(t.Context(), captureLow, memTestHour, memTestHour)
	require.Error(t, err)

	o, err := s.Observe(t.Context(), captureLow, memTestHour, memTestHour+1)
	require.NoError(t, err)
	_, err = o.Scope(t.Context(), memTestHour+1)
	require.ErrorContains(t, err, "outside")
	require.NoError(t, o.Close())
	require.NoError(t, o.Close())
	_, err = o.Scope(t.Context(), memTestHour)
	require.ErrorContains(t, err, "closed")
	observes, _, closes := s.counts()
	assert.Equal(t, 3, observes)
	assert.Equal(t, 2, closes)
}
