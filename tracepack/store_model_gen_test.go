package tracepack

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

// modelHours is the number of hours the schedule generator fills, from memTestHour; a lookup reads one to all of them.
const modelHours = 3

// Schedules TestStoreSourceModel checks, under -short and without it.
const (
	modelSchedules     = 300
	modelSchedulesLong = 1000
)

// modelHourPacks holds the packs of one hour of the generator's pool, each of captureLow,
// its period and records in the hour.
type modelHourPacks struct {
	// segA holds seqs b..b+2 (10, 11 and the lookup's primary 12 in the first hour), segB b+5 and b+6,
	// reply txReply's reply at 13, and spare b+4, where b is 10 times the hour's index plus one;
	// empty holds no record, its seq_start b+3.
	segA, segB, reply, spare, empty []byte
	// memA is the member of generation setA ranked (1, 1), patch a patch based on it replacing it,
	// memB the member of setB ranked (2, 2), which cancels setA,
	// and memC the member of setC ranked (1, 1), which conflicts with setA;
	// each holds seqs b+7 and b+8, memC b+7 only.
	memA, patch, memB, memC []byte
	// retired is an archive of the retired role holding b+9.
	retired []byte
}

// modelPool holds the packs the schedule generator composes stores from.
type modelPool struct {
	hours [modelHours]modelHourPacks
	// before and after are segments of the hours before and after the generator's,
	// which an observation reads the heads of only.
	before, after []byte
}

// newModelPool builds the generator's packs.
func newModelPool(t testing.TB) *modelPool {
	t.Helper()

	p := &modelPool{
		before: txPackIn(t, UUID{0x50, 0xFF, 1}, -1, nil, txBlock(txSeqs(1, 2)...)),
		after:  txPackIn(t, UUID{0x50, 0xFF, 2}, modelHours, nil, txBlock(txSeqs(50)...)),
	}
	for k := range int64(modelHours) {
		b := 10 * uint64(k+1)
		id := func(kind byte, i byte) UUID { return UUID{kind, byte(k), i} }
		seqs := func(s ...uint64) []footerTestStep { return txBlock(txSeqs(s...)...) }
		hp := &p.hours[k]
		hp.segA = txPackIn(t, id(0x50, 1), k, nil, seqs(b, b+1, b+2))
		hp.segB = txPackIn(t, id(0x50, 2), k, nil, seqs(b+5, b+6))
		hp.reply = txPackIn(t, id(0x50, 3), k, nil, txBlock(txReply(13, nil)))
		hp.spare = txPackIn(t, id(0x50, 4), k, nil, seqs(b+4))
		hp.empty = txPackIn(t, id(0x50, 5), k, func(m *PackMeta) { m.SeqStart = b + 3 }, nil)
		hp.memA = listedArchive(t, id(0xA0, 1), setA, k, 1, 1, seqs(b+7, b+8))
		hp.patch = listedPatch(t, id(0xB0, 1), k, new(setA), []UUID{id(0xA0, 1)}, seqs(b+7, b+8))
		hp.memB = listedArchive(t, id(0xA0, 2), setB, k, 2, 2, seqs(b+7, b+8))
		hp.memC = listedArchive(t, id(0xA0, 3), setC, k, 1, 1, seqs(b+7))
		hp.retired = setMetaRole(t, listedArchive(t, id(0xA0, 4), setD, k, 1, 1, seqs(b+9)), packRoleRetired)
	}

	return p
}

// modelPoint names a moment of an observation at which a schedule's action fires.
type modelPoint struct {
	// op is "snapshot", after the catalog took its snapshot; "list", after a page of a listing;
	// "open", before an object is opened; "read", during a read of an object, before the store checks that it exists;
	// or "confirm", before the catalog confirms an hour.
	op string
	// key is the listing's prefix, or the key opened.
	key string
	// n is the traversal of the prefix, from 1, and page its page, from 0.
	n, page int
	hour    int64
}

// modelAction is what a schedule does at one moment of the observation, once.
type modelAction struct {
	at   modelPoint
	do   func()
	done bool
}

// modelSchedule is a store and a catalog the generator filled from a seed, for a lookup of txKeyAt(12),
// with the actions it takes during the observation: late uploads, deletions and index-state changes.
type modelSchedule struct {
	c       *storeCase
	actions []*modelAction
}

// on schedules do at the moment at.
func (s *modelSchedule) on(at modelPoint, do func()) {
	s.actions = append(s.actions, &modelAction{at: at, do: do})
}

// fire takes every action of s not yet taken whose moment is p.
func (s *modelSchedule) fire(p modelPoint) {
	for _, a := range s.actions {
		if !a.done && a.at == p {
			a.done = true
			a.do()
		}
	}
}

// install sets the doubles' hooks that fire the schedule's actions.
func (s *modelSchedule) install() {
	traversals, pages := map[string]int{}, map[string]int{}
	s.c.store.afterList = func(prefix, token string, _ *ObjectPage) error {
		if token == "" {
			traversals[prefix]++
			pages[prefix] = 0
		} else {
			pages[prefix]++
		}
		s.fire(modelPoint{op: "list", key: prefix, n: traversals[prefix], page: pages[prefix]})

		return nil
	}
	s.c.store.openHook = func(_ context.Context, key string) (bool, Object, error) {
		s.fire(modelPoint{op: "open", key: key})

		return false, nil, nil
	}
	s.c.store.readHook = func(key string, _ int64, _ int) error {
		s.fire(modelPoint{op: "read", key: key})

		return nil
	}
	s.c.cat.afterSnapshot = func() { s.fire(modelPoint{op: "snapshot"}) }
	s.c.cat.beforeConfirm = func(hour int64) error {
		s.fire(modelPoint{op: "confirm", hour: hour})

		return nil
	}
}

// modelGen generates one schedule.
type modelGen struct {
	t   testing.TB
	rng *rand.Rand
	// alt is a second random stream, for choices added later, so that rng keeps the schedules of earlier seeds.
	alt *rand.Rand
	s   *modelSchedule
	// n is the lookup's MaxScopes, the hours observed.
	n int
}

// chance reports true with probability p percent.
func (g *modelGen) chance(p int) bool {
	return g.rng.IntN(100) < p
}

// late returns a moment of the observation of hour at which a late write may land:
// after the snapshot, during the hour's first or second commit traversal,
// during its archive traversal, or during the staging traversal.
func (g *modelGen) late(hour int64) modelPoint {
	switch g.rng.IntN(5) {
	case 0:
		return modelPoint{op: "snapshot"}
	case 1:
		return modelPoint{op: "list", key: commitDir(g.t, hour), n: 1, page: g.rng.IntN(2)}
	case 2:
		return modelPoint{op: "list", key: commitDir(g.t, hour), n: 2}
	case 3:
		return modelPoint{op: "list", key: archiveDir(g.t, hour), n: 1, page: g.rng.IntN(2)}
	default:
		return modelPoint{op: "list", key: stagingDir(g.t), n: 1, page: g.rng.IntN(3)}
	}
}

// putLater puts file in hour's scope now, or at a late moment when late is set; it returns its key.
func (g *modelGen) putLater(hour int64, file []byte, late bool) string {
	key := packKeyOf(g.t, hour, file)
	if late {
		g.s.on(g.late(hour), func() { g.s.c.store.put(key, file) })
	} else {
		g.s.c.store.put(key, file)
	}

	return key
}

// commitLater puts the commit object of id in hour's scope now, or at a late moment when late is set.
func (g *modelGen) commitLater(hour int64, id UUID, late bool) {
	if late {
		g.s.on(g.late(hour), func() { g.s.c.store.putCommit(g.t, captureLow, hour, id) })
	} else {
		g.s.c.store.putCommit(g.t, captureLow, hour, id)
	}
}

// deleteLater deletes key when the source opens it, or during its first read, half of the time each.
func (g *modelGen) deleteLater(key string) {
	if g.alt.IntN(2) == 0 {
		g.deleteAtOpen(key)
	} else {
		g.deleteAtRead(key)
	}
}

// deleteAtRead deletes key during the source's first read of it, after the store logged the read and before it checks the object.
func (g *modelGen) deleteAtRead(key string) {
	g.s.on(modelPoint{op: "read", key: key}, func() { g.s.c.store.remove(key) })
}

// deleteAtOpen deletes key when the source opens it.
func (g *modelGen) deleteAtOpen(key string) {
	g.s.on(modelPoint{op: "open", key: key}, func() { g.s.c.store.remove(key) })
}

// genModelSchedule generates the schedule of seed from pool: a lookup over one to three hours, each indexed or listed,
// holding segments, a generation, a patch, a cancelling or conflicting generation and a retired archive, a reply in one of them;
// commit objects and segments landing late, during the observation; packs deleted before the snapshot or when opened;
// scopes indexed between the snapshot and their confirmation, after it, or evicted; and segments of hours outside the lookup.
// Every late commit object lands after its pack, and no pack of an hour observed is deleted before it is opened,
// so a successful observation lies in the domain of the coherent observation (the tracepack storage specification §5).
func genModelSchedule(t testing.TB, pool *modelPool, seed uint64) *modelSchedule {
	t.Helper()

	rng := rand.New(rand.NewPCG(seed, 0x6d6f64656c))
	g := &modelGen{t: t, rng: rng, alt: rand.New(rand.NewPCG(seed, 0x72656164)), n: 1 + rng.IntN(modelHours)}
	g.s = &modelSchedule{c: newStoreCase(g.n)}
	g.s.c.store.pageSize = []int{1, 2, 3, 1000}[rng.IntN(4)]
	replyHour := rng.IntN(modelHours + 1)
	for k := range modelHours {
		hour := memTestHour + int64(k)
		hp := &pool.hours[k]
		if k >= g.n {
			if g.chance(50) {
				g.putLater(hour, hp.segA, false)
			}

			continue
		}
		if g.chance(40) {
			g.indexedHour(hour, hp, k == replyHour)
		} else {
			g.listedHour(hour, hp, k == replyHour)
		}
	}
	if g.chance(40) {
		g.evidence()
	}
	if g.chance(40) {
		g.barriers()
	}
	for _, f := range [][]byte{pool.before, pool.after} {
		if g.chance(50) {
			key := g.putLater(hourOf(mustOpen(t, f, ReaderOptions{}).Header().Meta.PeriodStart), f, false)
			if g.chance(5) {
				g.deleteLater(key)
			} else if g.alt.IntN(100) < 6 {
				g.deleteAtRead(key)
			}
		}
	}
	g.s.install()

	return g.s
}

// evidence gives the catalog evidence of captureLow: an end state, a closure of the records' epoch,
// a stop boundary, and partial, each now and then.
func (g *modelGen) evidence() {
	e := CaptureEvidence{End: []EndState{EndOpen, EndStopped, EndStoppedUnclean}[g.rng.IntN(3)], Partial: g.chance(20)}
	if g.chance(60) {
		e.Closures = []EpochClosure{{Epoch: txTestEpoch, CloseSeq: 10 + uint64(g.rng.IntN(30))}}
	}
	if g.chance(30) {
		e.Boundaries = []Boundary{{Capture: captureLow, Seq: 10 + uint64(g.rng.IntN(30)), Kind: BoundaryKindStop}}
	}
	g.s.c.cat.setEvidence(captureLow, e)
}

// barriers gives the catalog stop-unclean boundaries of the tool, each now and then:
// one of another capture whose interval starts in an hour of the lookup, one of captureLow,
// an inverted one, which meets every range, and one far before the lookup's hours, which the snapshot leaves out.
func (g *modelGen) barriers() {
	at := func(hours int64) *int64 { return new(blockTestHour + hours*hourNs + int64(g.rng.IntN(1000))) }
	seq := func() uint64 { return 1 + uint64(g.rng.IntN(40)) }
	if g.chance(70) {
		g.s.c.cat.addBarrier(Boundary{Capture: captureHigh, Seq: seq(), Kind: BoundaryKindStopUnclean, GapStart: at(int64(g.rng.IntN(g.n)))})
	}
	if g.chance(40) {
		start := at(int64(g.rng.IntN(g.n)))
		g.s.c.cat.addBarrier(Boundary{Capture: captureLow, Seq: seq(), Kind: BoundaryKindStopUnclean, GapStart: start, GapEnd: new(*start + 1000)})
	}
	if g.chance(40) {
		g.s.c.cat.addBarrier(Boundary{Capture: captureHigh, Seq: seq(), Kind: BoundaryKindStopUnclean, GapStart: at(2), GapEnd: at(0)})
	}
	if g.chance(30) {
		g.s.c.cat.addBarrier(Boundary{Capture: captureHigh, Seq: seq(), Kind: BoundaryKindStopUnclean, GapStart: at(-10), GapEnd: at(-9)})
	}
}

// indexedHour fills hour as a scope the catalog indexes: its packs present, its view the active view of its tier-role packs,
// or conflicted; evicted after the snapshot, or a pack of its view deleted when opened, now and then.
func (g *modelGen) indexedHour(hour int64, hp *modelHourPacks, reply bool) {
	present := map[UUID][]byte{}
	keys := map[UUID]string{}
	add := func(file []byte) {
		id := mustOpen(g.t, file, ReaderOptions{}).Header().PackID
		present[id], keys[id] = file, g.putLater(hour, file, false)
	}
	commits := CommitSet{}
	if g.chance(85) {
		add(hp.segA)
	}
	if g.chance(40) {
		add(hp.segB)
	}
	if reply {
		add(hp.reply)
	}
	if g.chance(40) {
		add(hp.memA)
		commits[setA] = struct{}{}
		if g.chance(50) {
			add(hp.patch)
			commits[UUID{0xB0, byte(hour - memTestHour), 1}] = struct{}{}
		}
		if g.chance(15) {
			add(hp.memC)
			commits[setC] = struct{}{}
		}
	}
	if g.chance(15) {
		g.putLater(hour, hp.retired, false)
	}
	for id := range commits {
		g.s.c.store.putCommit(g.t, captureLow, hour, id)
	}
	files, err := viewFiles(g.t.Context(), present, commits)
	if errors.Is(err, ErrViewConflicted) || (hour > memTestHour && g.chance(15)) {
		g.s.c.cat.setConflicted(captureLow, hour, true)
	} else {
		require.NoError(g.t, err)
		packs := make([]CatalogPack, len(files))
		for i, f := range files {
			h := mustOpen(g.t, f, ReaderOptions{}).Header()
			packs[i] = CatalogPack{Key: keys[h.PackID], PackID: h.PackID, Size: int64(len(f))}
		}
		g.s.c.cat.setView(captureLow, hour, packs...)
		if len(packs) > 0 && g.chance(4) {
			g.deleteLater(packs[g.rng.IntN(len(packs))].Key)
		}
	}
	if g.chance(15) {
		g.s.on(g.late(hour), func() { g.s.c.cat.setIndexed(captureLow, hour, false) })
	}
}

// listedHour fills hour as a scope the catalog does not index: segments present or uploaded late,
// a generation whose commit object may land late, a patch committed before, during or after the commit traversals, or never,
// a cancelling or conflicting generation, a retired archive present or uploaded late,
// and a segment put and deleted before the snapshot;
// now and then, indexed between the snapshot and its confirmation,
// or after its confirmation while the next hour's is pending, or a pack deleted when opened.
func (g *modelGen) listedHour(hour int64, hp *modelHourPacks, reply bool) {
	g.s.c.cat.setIndexed(captureLow, hour, false)
	var keys []string
	keep := func(key string) { keys = append(keys, key) }
	switch r := g.rng.IntN(10); {
	case r < 8:
		keep(g.putLater(hour, hp.segA, false))
	case r < 9:
		g.putLater(hour, hp.segA, true)
	default:
	}
	switch r := g.rng.IntN(10); {
	case r < 3:
		keep(g.putLater(hour, hp.segB, false))
	case r < 7:
		g.putLater(hour, hp.segB, true)
	default:
	}
	if reply {
		if g.chance(60) {
			keep(g.putLater(hour, hp.reply, false))
		} else {
			g.putLater(hour, hp.reply, true)
		}
	}
	if g.chance(50) {
		g.listedGenerations(hour, hp, keep)
	}
	switch r := g.rng.IntN(20); {
	case r < 3:
		g.putLater(hour, hp.retired, false)
	case r < 5:
		g.putLater(hour, hp.retired, true)
	default:
	}
	if g.chance(10) {
		g.s.c.store.remove(g.putLater(hour, hp.spare, false))
	}
	if g.chance(15) {
		g.putLater(hour, hp.empty, g.chance(30))
	}
	switch r := g.rng.IntN(100); {
	case r < 8:
		evict := g.chance(50)
		g.s.on(g.late(hour), func() {
			g.s.c.cat.setIndexed(captureLow, hour, true)
			if evict {
				g.s.c.cat.setIndexed(captureLow, hour, false)
			}
		})
	case r < 16 && hour+1 < memTestHour+int64(g.n):
		g.s.on(modelPoint{op: "confirm", hour: hour + 1}, func() { g.s.c.cat.setIndexed(captureLow, hour, true) })
	case r < 20 && len(keys) > 0:
		g.deleteLater(keys[g.rng.IntN(len(keys))])
	default:
	}
}

// listedGenerations puts in the listed hour the member of setA, its commit object present or landing late,
// a patch based on it, committed before, during or after the commit traversals, or never,
// and, now and then, a cancelling or a conflicting generation; keep is given the key of every pack present before the snapshot.
func (g *modelGen) listedGenerations(hour int64, hp *modelHourPacks, keep func(key string)) {
	keep(g.putLater(hour, hp.memA, false))
	g.commitLater(hour, setA, g.chance(30))
	if g.chance(60) {
		patchID := UUID{0xB0, byte(hour - memTestHour), 1}
		switch r := g.rng.IntN(10); {
		case r < 4:
			keep(g.putLater(hour, hp.patch, false))
			g.commitLater(hour, patchID, false)
		case r < 8:
			keep(g.putLater(hour, hp.patch, false))
			g.commitLater(hour, patchID, true)
		default:
			g.putLater(hour, hp.patch, g.chance(50))
		}
	}
	if g.chance(15) {
		keep(g.putLater(hour, hp.memB, false))
		g.commitLater(hour, setB, g.chance(50))
	}
	if g.chance(15) {
		keep(g.putLater(hour, hp.memC, false))
		g.commitLater(hour, setC, g.chance(50))
	}
}

// modelOutcomes counts the lookups of the schedules checked that matched beside a cold or a conflicted gap.
type modelOutcomes struct {
	schedules, failed              int
	matchedCold, matchedConflicted int
}

// add counts r, the check of one schedule.
func (o *modelOutcomes) add(r *storeCaseResult) {
	o.schedules++
	if !r.observed {
		o.failed++
	}
	if r.res.Outcome != TxMatched {
		return
	}
	reasons := gapReasons(r.res.Gaps)
	if slices.Contains(reasons, TxGapCold) {
		o.matchedCold++
	}
	if slices.Contains(reasons, TxGapConflicted) {
		o.matchedConflicted++
	}
}

// modelVacuity returns the requirements that the schedules checked, stats and outcomes their sums, fail to meet:
// a fixed fraction of them must give successful observations with a listed hour,
// with a late commit object, and with a late segment in a view;
// and some must take more than two commit traversals, fix a committed patch, a non-empty view and a conflicted one,
// hold a segment without records, report a retired archive, carry evidence, barriers and an inverted barrier,
// fail, fail with ErrObjectNotFound, and match beside a cold gap and beside a conflicted one.
func modelVacuity(stats *modelStats, o *modelOutcomes) []string {
	var out []string
	fraction := func(name string, got, percent int) {
		if got*100 < o.schedules*percent {
			out = append(out, fmt.Sprintf("%s: %d of %d schedules, under %d%%", name, got, o.schedules, percent))
		}
	}
	fraction("successful with a listed hour", stats.listed, 40)
	fraction("successful with a late commit object", stats.withLateCommit, 10)
	fraction("successful with a late segment in a view", stats.withLateSegment, 10)
	for name, n := range map[string]int{
		"more than two commit traversals":     stats.repeated,
		"a patch in a listed view":            stats.patches,
		"a non-empty listed view":             stats.nonEmpty,
		"a conflicted listed view":            stats.conflicted,
		"a retired archive reported":          stats.excluded,
		"an empty segment in a listed view":   stats.emptySegments,
		"evidence":                            stats.evidence,
		"barriers":                            stats.barriers,
		"an inverted barrier":                 stats.inverted,
		"failures with ErrObjectNotFound":     stats.notFound,
		"failures at a read of a gone object": stats.goneAtRead,
		"failed observations":                 o.failed,
		"matched beside cold":                 o.matchedCold,
		"matched beside conflicted":           o.matchedConflicted,
	} {
		if n == 0 {
			out = append(out, "no "+name)
		}
	}

	return out
}

// modelChecked is what checking generated schedules found.
type modelChecked struct {
	// rejected counts the schedules the check rejects, and first holds the problems of the first of them.
	rejected int
	first    string
	// stats and outcomes sum what the check met in the others.
	stats    modelStats
	outcomes modelOutcomes
}

// checkModelSchedules checks the schedules of seeds 0 to n-1 from pool, each changed by edit when set.
func checkModelSchedules(t testing.TB, pool *modelPool, n int, edit func(c *storeCase)) modelChecked {
	t.Helper()

	var out modelChecked
	for seed := range uint64(n) {
		s := genModelSchedule(t, pool, seed)
		if edit != nil {
			edit(s.c)
		}
		r := checkStoreCase(t, s.c)
		if len(r.problems) > 0 {
			if out.rejected == 0 {
				out.first = fmt.Sprintf("seed %d: %v", seed, r.problems)
			}
			out.rejected++

			continue
		}
		out.stats.add(r.stats)
		out.outcomes.add(&r)
	}

	return out
}

// TestStoreSourceModel checks the lookups of generated schedules against the acquisition model:
// every observation that succeeds holds the views, evidence, barriers and excluded packs
// that the model derives from the doubles' logs,
// and the lookup over it returns what the lookup over the model's replay returns.
// A fixed fraction of the schedules must give successful observations with a listed hour,
// a late commit object and a late segment, and the schedules must meet every case modelVacuity names.
func TestStoreSourceModel(t *testing.T) {
	t.Parallel()

	n := modelSchedulesLong
	if testing.Short() {
		n = modelSchedules
	}
	pool := newModelPool(t)
	got := checkModelSchedules(t, pool, n, nil)
	require.Zero(t, got.rejected, got.first)
	require.Empty(t, modelVacuity(&got.stats, &got.outcomes), "stats %+v, outcomes %+v", got.stats, got.outcomes)
}

// TestStoreSourceModelVacuity checks the generated schedules through a source whose every Observe fails:
// the check rejects none of them, and the non-vacuity of TestStoreSourceModel fails.
func TestStoreSourceModelVacuity(t *testing.T) {
	t.Parallel()

	got := checkModelSchedules(t, newModelPool(t), modelSchedules, func(c *storeCase) { c.cat.snapshotErr = errors.New("always") })
	require.Zero(t, got.rejected, got.first)
	require.Zero(t, got.stats.observed)
	require.Contains(t, modelVacuity(&got.stats, &got.outcomes), "successful with a listed hour: 0 of 300 schedules, under 40%")
}

// confirmingCatalog is a Catalog that confirms every scope it is asked about once its catalog answered, whatever the answer.
type confirmingCatalog struct {
	Catalog
}

// ConfirmUnindexed asks the catalog, then confirms the scope unless the catalog failed.
func (c confirmingCatalog) ConfirmUnindexed(ctx context.Context, capture UUID, hour int64, token string) (bool, error) {
	if _, err := c.Catalog.ConfirmUnindexed(ctx, capture, hour, token); err != nil {
		return false, err
	}

	return true, nil
}

// TestStoreSourceModelMutations breaks the source four ways and checks the generated schedules through it:
// a committed patch dropped from a listed view, an empty listed view, a listed hour's commit objects taken from one traversal,
// and a scope the catalog does not confirm accepted.
// The check rejects some of the schedules under each.
// The last two kills are structural: the model's witness rule requires two agreeing commit traversals,
// and its confirmation rule requires the catalog's confirmation and an index history that allows it,
// so any successful observation of a listed hour under them is rejected;
// the first two are caught by comparing views and lookups.
func TestStoreSourceModelMutations(t *testing.T) {
	t.Parallel()

	// dropPatch drops the first patch of the view ActiveView returns.
	dropPatch := func(infos []PackInfo, commits CommitSet) (View, error) {
		v, err := ActiveView(infos, commits)
		for _, p := range infos {
			if i := slices.Index(v.Packs, p.PackID()); i >= 0 && p.Meta().PackRole == PackRoleRepair {
				v.Packs = slices.Delete(v.Packs, i, i+1)

				break
			}
		}

		return v, err
	}
	// empty empties the view ActiveView returns, keeping its capture and hour.
	empty := func(infos []PackInfo, commits CommitSet) (View, error) {
		v, err := ActiveView(infos, commits)
		v.Packs = nil

		return v, err
	}
	mutations := []struct {
		name string
		edit func(c *storeCase)
	}{
		{name: "a committed patch dropped", edit: func(c *storeCase) { c.mutate = func(s *storeSource) { s.activeView = dropPatch } }},
		{name: "an empty view", edit: func(c *storeCase) { c.mutate = func(s *storeSource) { s.activeView = empty } }},
		{name: "one commit traversal", edit: func(c *storeCase) {
			c.mutate = func(s *storeSource) { s.stopCommits = func(int) bool { return true } }
		}},
		{name: "a refused confirmation accepted", edit: func(c *storeCase) {
			c.catalog = func(cat Catalog) Catalog { return confirmingCatalog{Catalog: cat} }
		}},
	}
	pool := newModelPool(t)
	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()

			got := checkModelSchedules(t, pool, modelSchedules, m.edit)
			require.Positive(t, got.rejected)
			t.Logf("%d of %d schedules rejected; the first: %s", got.rejected, modelSchedules, got.first)
		})
	}
}
