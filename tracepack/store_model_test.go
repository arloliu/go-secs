package tracepack

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// The acquisition model is an independent check of a store source's Observe (the tracepack storage specification §5,
// Coherent observation and Observation of a lookup).
// From the doubles' logs alone — what each listing returned, the bytes each key held and when, the catalog's snapshot,
// its index-state changes and its confirmations, on one clock — it derives the observation the source must have fixed,
// as a memSource replay, and the packs it must have reported excluded.
// It shares no acquisition code with the source: it uses the key parsers and listing prefixes, the file header and pack metadata
// decoders, Open, ActiveView through the replay, and the role and generation rules of view.go.
//
// For each hour the snapshot does not index,
// the model finds the first two consecutive complete traversals of the hour's commit objects that return the same keys,
// which must be the last two the source took;
// the witness instant t follows the first of them.
// The hour's view is ActiveView, over the packs ascending by pack_id, of the packs present at t —
// the hour's archive packs of the capture and the segments whose period lies in the hour, superseded patches included —
// and of exactly those segments uploaded after t that the staging traversal returned, under the commit objects present at t.
// The archive packs of the capture that the hour's archive traversal returned
// and whose role takes no part in a view are reported.
// An indexed hour holds the snapshot's packs in its order, and the evidence and barriers are the snapshot's.

// modelRun is one Observe of captureLow's hours [from, to) through a store source, with the doubles' logs,
// each from before the store was filled, and where the Observe's own events begin in them.
type modelRun struct {
	from, to int64
	events   []memStoreEvent
	start    int
	log      []fakeCatalogEvent
	catStart int
	// maxMeta is the source's MaxPackMetadataLen.
	maxMeta int64
}

// modelExpected is what the acquisition model derives from one Observe.
type modelExpected struct {
	// replay holds the observation the source must have fixed; excluded the packs it must have reported.
	replay   *memSource
	excluded []ExcludedPack
	// problems lists every way the Observe could not have succeeded, or left the domain the model covers.
	problems []string
	stats    modelStats
	// maxMeta is the source's MaxPackMetadataLen.
	maxMeta int64
	// late holds, by listed hour, the pack_ids of the segments uploaded after the hour's witness instant.
	late map[int64]map[UUID]bool
	// opens holds, by key, the instants the Observe opened it at.
	opens map[string][]int64
}

// bytesAt returns the bytes the source read of key: those it held when the Observe first opened it after instant after,
// or, when it did not, the bytes it held last.
func (x *modelExpected) bytesAt(hist modelHistory, key string, after int64) ([]byte, bool) {
	for _, at := range x.opens[key] {
		if at > after {
			return hist.at(key, at)
		}
	}

	return hist.last(key)
}

// modelStats counts what the acquisition model met in the Observes it checked, for the non-vacuity of its tests.
type modelStats struct {
	// observed counts the Observes that succeeded, listed those of them with a listed hour.
	observed, listed int
	// listedHours counts the listed hours; lateCommits the commit objects of a listed hour put after the snapshot;
	// lateSegments the segments uploaded after their hour's witness instant that the hour's expected view holds;
	// repeated the listed hours whose commit objects took more than two traversals.
	listedHours, lateCommits, lateSegments, repeated int
	// patches counts the patches the expected listed views hold, nonEmpty the listed views that hold a pack,
	// conflicted the listed views that are conflicted, and excluded the packs reported.
	patches, nonEmpty, conflicted, excluded int
	// withLateCommit and withLateSegment count the Observes that succeeded with a late commit object,
	// or a late segment in a view.
	withLateCommit, withLateSegment int
	// evidence counts the Observes that succeeded with evidence that is not zero, barriers those with a barrier,
	// and inverted those with an inverted barrier.
	evidence, barriers, inverted int
	// emptySegments counts the segments known to hold no record that an expected listed view holds.
	emptySegments int
	// notFound counts the Observes that had to fail, and failed, with ErrObjectNotFound,
	// goneAtRead those of them that read an object deleted during the read.
	notFound, goneAtRead int
}

// add adds the counts of o to s.
func (s *modelStats) add(o modelStats) {
	s.observed += o.observed
	s.listed += o.listed
	s.listedHours += o.listedHours
	s.lateCommits += o.lateCommits
	s.lateSegments += o.lateSegments
	s.repeated += o.repeated
	s.patches += o.patches
	s.nonEmpty += o.nonEmpty
	s.conflicted += o.conflicted
	s.excluded += o.excluded
	s.withLateCommit += o.withLateCommit
	s.withLateSegment += o.withLateSegment
	s.evidence += o.evidence
	s.barriers += o.barriers
	s.inverted += o.inverted
	s.emptySegments += o.emptySegments
	s.notFound += o.notFound
	s.goneAtRead += o.goneAtRead
}

// problemf records one way the Observe could not have succeeded.
func (x *modelExpected) problemf(format string, args ...any) {
	x.problems = append(x.problems, fmt.Sprintf(format, args...))
}

// modelLife is one stretch of a key's existence: from its put to its deletion, MaxInt64 when never deleted.
type modelLife struct {
	from, until int64
	data        []byte
}

// modelHistory holds the life of every key of a memStore, replayed from its log.
type modelHistory map[string][]modelLife

// newModelHistory replays the puts and deletions of events.
func newModelHistory(events []memStoreEvent) modelHistory {
	h := modelHistory{}
	for _, e := range events {
		lives := h[e.key]
		n := len(lives)
		switch e.op {
		case "put":
			if n > 0 && lives[n-1].until == math.MaxInt64 {
				lives[n-1].until = e.at
			}
			h[e.key] = append(lives, modelLife{from: e.at, until: math.MaxInt64, data: e.data})
		case "delete":
			if n > 0 && lives[n-1].until == math.MaxInt64 {
				lives[n-1].until = e.at
			}
		default:
		}
	}

	return h
}

// at returns the bytes key holds just after instant x, and whether it exists then.
func (h modelHistory) at(key string, x int64) ([]byte, bool) {
	for _, l := range h[key] {
		if l.from <= x && x < l.until {
			return l.data, true
		}
	}

	return nil, false
}

// under returns the keys under prefix that exist just after instant x, in byte order.
func (h modelHistory) under(prefix string, x int64) []string {
	var out []string
	for key := range h {
		if _, ok := h.at(key, x); ok && strings.HasPrefix(key, prefix) {
			out = append(out, key)
		}
	}
	slices.Sort(out)

	return out
}

// last returns the bytes key held last, deleted since or not, and whether it was ever put.
func (h modelHistory) last(key string) ([]byte, bool) {
	lives := h[key]
	if len(lives) == 0 {
		return nil, false
	}

	return lives[len(lives)-1].data, true
}

// modelTraversal is one traversal of a listing as the store's log shows it.
type modelTraversal struct {
	// start is the instant its first page was taken, and end the instant its last page was returned,
	// after the store's listing hooks ran.
	start, end int64
	objects    []ObjectInfo
	complete   bool
}

// keys returns the keys of tr, in order.
func (tr *modelTraversal) keys() []string {
	out := make([]string, len(tr.objects))
	for i, o := range tr.objects {
		out[i] = o.Key
	}

	return out
}

// traversals groups the listings of prefix in events into traversals,
// recording a problem for any break of the listing contract of ObjectStore the source must have refused,
// and for a listing error or a traversal left unfinished.
func (x *modelExpected) traversals(events []memStoreEvent, prefix string) []modelTraversal {
	var out []modelTraversal
	var tokens map[string]bool
	prev, next := "", ""
	for _, e := range events {
		if e.op != "list" || e.key != prefix {
			continue
		}
		n := len(out)
		switch {
		case e.token == "":
			if n > 0 && !out[n-1].complete {
				x.problemf("a traversal of %q is left unfinished", prefix)
			}
			out = append(out, modelTraversal{start: e.at})
			n++
			tokens, prev = map[string]bool{}, ""
		case n == 0 || out[n-1].complete || e.token != next:
			x.problemf("a page of %q after token %q belongs to no traversal", prefix, e.token)

			continue
		default:
		}
		tr := &out[n-1]
		if e.listErr != nil {
			x.problemf("listing %q failed: %v", prefix, e.listErr)
		}
		for _, o := range e.page.Objects {
			if !strings.HasPrefix(o.Key, prefix) || o.Key <= prev {
				x.problemf("listing %q returned key %q outside the prefix or not above %q", prefix, o.Key, prev)
			}
			prev = o.Key
			tr.objects = append(tr.objects, o)
		}
		tr.end = e.at
		if e.returned != 0 {
			tr.end = e.returned
		}
		switch {
		case e.page.Next == "":
			tr.complete = true
		case tokens[e.page.Next]:
			x.problemf("listing %q returned token %q again", prefix, e.page.Next)
		default:
			tokens[e.page.Next] = true
			next = e.page.Next
		}
	}
	if n := len(out); n > 0 && !out[n-1].complete {
		x.problemf("a traversal of %q is left unfinished", prefix)
	}

	return out
}

// modelPack is a pack the acquisition model read from the store's log.
type modelPack struct {
	key  string
	data []byte
	hdr  format.FileHeader
	meta *PackMeta
	// hour is its period's UTC hour, valid when inHour is set.
	hour   int64
	inHour bool
}

// readPack decodes and validates the head of data, the bytes of key, as Open does before it decodes any block:
// its file header, and its pack metadata within the object, within the source's MaxPackMetadataLen, and matching its CRC.
func (x *modelExpected) readPack(key string, data []byte) (modelPack, error) {
	if len(data) < format.FileHeaderLen {
		return modelPack{}, fmt.Errorf("%q holds %d bytes", key, len(data))
	}
	hdr, err := format.UnmarshalFileHeader(data[:format.FileHeaderLen])
	if err != nil {
		return modelPack{}, fmt.Errorf("%q: %w", key, err)
	}
	end := uint64(format.FileHeaderLen) + uint64(hdr.PackMetadataLen)
	if end > uint64(len(data)) {
		return modelPack{}, fmt.Errorf("%q: pack metadata beyond the object", key)
	}
	if int64(hdr.PackMetadataLen) > x.maxMeta {
		return modelPack{}, fmt.Errorf("%q: %d bytes of pack metadata, over %d", key, hdr.PackMetadataLen, x.maxMeta)
	}
	raw := data[format.FileHeaderLen:end]
	if crc := format.CRC(raw); crc != hdr.PackMetadataCRC {
		return modelPack{}, fmt.Errorf("%q: pack metadata CRC 0x%08X, not 0x%08X", key, crc, hdr.PackMetadataCRC)
	}
	meta, err := UnmarshalPackMeta(raw)
	if err != nil {
		return modelPack{}, fmt.Errorf("%q: %w", key, err)
	}
	p := modelPack{key: key, data: data, hdr: hdr, meta: meta}
	p.hour, p.inHour = periodHour(meta.PeriodStart, meta.PeriodEnd)

	return p, nil
}

// checkModelPack returns why the source must have refused p, a pack of hour's scope under key k,
// or "" when the model finds nothing: its pack_id, capture_id and tool_id, its period, its role against its key's area,
// its size, its generation against its role, and a segment key's seq_first against its first block's first seq,
// or against its seq_start when it is known to hold no record: its footer is used and counts no record.
// A segment's hour is its period's, so hour is not compared for one; open opens it in full.
func checkModelPack(p *modelPack, packID UUID, segment bool, seqFirst uint64, hour int64, size int64, open bool) string {
	m := p.meta
	tier := tierRole(m.PackRole)
	switch {
	case UUID(p.hdr.PackID) != packID:
		return fmt.Sprintf("pack_id %s, not its key's %s", UUID(p.hdr.PackID), packID)
	case UUID(p.hdr.CaptureID) != captureLow:
		return fmt.Sprintf("capture_id %s", UUID(p.hdr.CaptureID))
	case m.ToolID != memStoreTool:
		return fmt.Sprintf("tool_id %q", m.ToolID)
	case !p.inHour:
		return "a period outside one hour"
	case !segment && p.hour != hour:
		return fmt.Sprintf("a period in hour %d", p.hour)
	case segment != (m.PackRole == PackRoleSegment):
		return fmt.Sprintf("pack_role %s under its key's area", m.PackRole)
	case size != int64(len(p.data)):
		return fmt.Sprintf("size %d, not %d", size, len(p.data))
	}
	if tier {
		if err := checkRoleGeneration(m); err != nil {
			return err.Error()
		}
	}
	if !open {
		return ""
	}
	r, err := Open(context.Background(), bytes.NewReader(p.data), int64(len(p.data)), ReaderOptions{})
	if err != nil {
		return err.Error()
	}
	if !segment {
		return ""
	}
	if b := r.Blocks(); len(b) > 0 && b[0].FirstSeq != seqFirst {
		return fmt.Sprintf("seq_first %d, not its first seq %d", seqFirst, b[0].FirstSeq)
	}
	if st, ok := r.Stats(); ok && len(r.Blocks()) == 0 && st.RecordCount == 0 && m.SeqStart != seqFirst {
		return fmt.Sprintf("seq_first %d, not the seq_start %d of a segment without records", seqFirst, m.SeqStart)
	}

	return ""
}

// storeModel derives, from run, the observation the source must have fixed when its Observe succeeded.
func storeModel(t testing.TB, run *modelRun) *modelExpected {
	t.Helper()

	x := &modelExpected{replay: newMemSource(), stats: modelStats{observed: 1}, maxMeta: run.maxMeta, late: map[int64]map[UUID]bool{}}
	hist := newModelHistory(run.events)
	events := run.events[run.start:]
	for _, gone := range modelGoneOps(hist, events) {
		x.problemf("%s of %q, which was gone then, did not fail Observe", gone.op, gone.key)
	}
	x.opens = map[string][]int64{}
	for _, e := range events {
		if e.op == "open" {
			x.opens[e.key] = append(x.opens[e.key], e.at)
		}
	}
	var snapAt int64
	var snap CatalogSnapshot
	snaps := 0
	for _, e := range run.log[run.catStart:] {
		if e.op == "snapshot" {
			snaps++
			snapAt, snap = e.at, e.snap
		}
	}
	if snaps != 1 {
		x.problemf("%d catalog snapshots", snaps)

		return x
	}
	if int64(len(snap.Scopes)) != run.to-run.from {
		x.problemf("a snapshot of %d scopes for hours [%d, %d)", len(snap.Scopes), run.from, run.to)

		return x
	}
	x.replay.giveEvidence(captureLow, snap.Evidence)
	x.replay.giveBarriers(snap.Barriers)
	x.countSnapshot(&snap)

	var listed []int64
	for i, sc := range snap.Scopes {
		h := run.from + int64(i)
		if sc.Hour != h {
			x.problemf("the snapshot's scope %d is of hour %d", i, sc.Hour)

			return x
		}
		x.replay.setIndexed(captureLow, h, sc.Indexed)
		if !sc.Indexed {
			listed = append(listed, h)

			continue
		}
		x.replay.setConflicted(captureLow, h, sc.Conflicted)
		if !sc.Conflicted {
			x.expectIndexed(hist, snapAt, h, sc.Packs)
		}
	}
	if len(listed) == 0 {
		return x
	}
	x.stats.listed = 1

	staging := x.traversals(events, stagingDir(t))
	if len(staging) != 1 {
		x.problemf("%d traversals of the staging segments", len(staging))

		return x
	}
	segments := x.readSegments(hist, &staging[0])
	for _, h := range listed {
		x.expectListed(t, run, hist, events, snapAt, &snap.Scopes[h-run.from], &staging[0], segments)
	}
	slices.SortFunc(x.excluded, func(a, b ExcludedPack) int { return strings.Compare(a.Key, b.Key) })
	x.stats.excluded = len(x.excluded)

	return x
}

// countSnapshot counts the snapshot's evidence that is not zero, its barriers and its inverted barriers.
func (x *modelExpected) countSnapshot(snap *CatalogSnapshot) {
	if e := snap.Evidence; e.End != EndOpen || len(e.Closures) > 0 || len(e.Boundaries) > 0 || e.Partial {
		x.stats.evidence = 1
	}
	if len(snap.Barriers) > 0 {
		x.stats.barriers = 1
	}
	for _, b := range snap.Barriers {
		if b.GapStart != nil && b.GapEnd != nil && *b.GapStart > *b.GapEnd {
			x.stats.inverted = 1
		}
	}
}

// expectIndexed sets the replay's view of the indexed hour to the snapshot's packs, in its order, and checks each.
func (x *modelExpected) expectIndexed(hist modelHistory, snapAt, h int64, packs []CatalogPack) {
	files := make([][]byte, 0, len(packs))
	for _, cp := range packs {
		data, ok := x.bytesAt(hist, cp.Key, snapAt)
		if !ok {
			x.problemf("hour %d: the indexed pack %q is not there", h, cp.Key)

			continue
		}
		p, err := x.readPack(cp.Key, data)
		if err != nil {
			x.problemf("hour %d: the indexed pack's head: %v", h, err)

			continue
		}
		var segment bool
		var seqFirst uint64
		if s, err := parseSegmentKey(storeTestPrefix, cp.Key); err == nil {
			segment, seqFirst = true, s.seqFirst
			if s.pack != cp.PackID || s.capture != captureLow {
				x.problemf("hour %d: the indexed pack %q disagrees with its descriptor", h, cp.Key)
			}
		} else if a, err := parseArchiveKey(storeTestPrefix, cp.Key); err != nil ||
			a.pack != cp.PackID || a.capture != captureLow || a.hour != h {
			x.problemf("hour %d: the indexed pack's key %q disagrees with its descriptor or the scope", h, cp.Key)
		}
		if why := checkModelPack(&p, cp.PackID, segment, seqFirst, h, cp.Size, true); why != "" {
			x.problemf("hour %d: the indexed pack %q: %s", h, cp.Key, why)
		}
		if segment && p.hour != h {
			x.problemf("hour %d: the indexed segment %q lies in hour %d", h, cp.Key, p.hour)
		}
		if !tierRole(p.meta.PackRole) {
			x.problemf("hour %d: the indexed pack %q has pack_role %s", h, cp.Key, p.meta.PackRole)
		}
		files = append(files, data)
	}
	x.replay.giveView(captureLow, h, files...)
}

// modelSegment is a segment the staging traversal returned, with its key's parts and its head;
// head is nil when it does not decode.
type modelSegment struct {
	info ObjectInfo
	key  segmentKeyParts
	head *modelPack
}

// readSegments parses every key the staging traversal tr returned and decodes each segment's head,
// recording a problem for any the source must have refused.
func (x *modelExpected) readSegments(hist modelHistory, tr *modelTraversal) []modelSegment {
	out := make([]modelSegment, 0, len(tr.objects))
	for _, o := range tr.objects {
		k, err := parseSegmentKey(storeTestPrefix, o.Key)
		if err != nil {
			x.problemf("the staging traversal returned %q: %v", o.Key, err)

			continue
		}
		s := modelSegment{info: o, key: k}
		data, _ := x.bytesAt(hist, o.Key, tr.end)
		if p, err := x.readPack(o.Key, data); err != nil {
			x.problemf("a segment's head: %v", err)
		} else {
			s.head = &p
			if why := checkModelPack(&p, k.pack, true, k.seqFirst, 0, o.Size, false); why != "" {
				x.problemf("segment %q: %s", o.Key, why)
			}
		}
		out = append(out, s)
	}

	return out
}

// modelHour is what the acquisition model derives for one listed hour.
type modelHour struct {
	h int64
	// witness is the instant just after the first of the two agreeing commit traversals ended; ids the commit set there.
	witness int64
	ids     CommitSet
	// packs holds the packs ActiveView takes for the hour's view, opened counts the packs listed of each pack_id,
	// and keys holds every key the hour's listings returned of a pack the source reads.
	packs  []*modelPack
	opened map[UUID]int
	keys   map[string]bool
	// prefixes holds the listing prefixes of the hour's acquisition.
	prefixes []string
	// token is the token the snapshot issued for the hour.
	token string
}

// expectListed derives the replay's view of the listed hour of sc and the packs of the hour to report excluded,
// and checks the hour's confirmation.
func (x *modelExpected) expectListed(t testing.TB, run *modelRun, hist modelHistory, events []memStoreEvent, snapAt int64,
	sc *CatalogScope, staging *modelTraversal, segments []modelSegment,
) {
	t.Helper()

	h := sc.Hour
	x.stats.listedHours++
	mh := &modelHour{h: h, ids: CommitSet{}, opened: map[UUID]int{}, keys: map[string]bool{},
		prefixes: []string{commitDir(t, h), archiveDir(t, h), stagingDir(t)}, token: sc.Token}
	if !x.commitWitness(run, hist, events, snapAt, mh) || !x.listedArchives(hist, events, staging, mh) {
		return
	}
	x.listedSegments(hist, segments, staging.end, mh)
	for id, n := range mh.opened {
		if n > 1 {
			x.problemf("hour %d: pack_id %s is listed %d times", h, id, n)
		}
	}
	for id := range mh.ids {
		x.replay.commit(captureLow, h, id)
	}
	for _, p := range mh.packs {
		if _, err := Open(context.Background(), bytes.NewReader(p.data), int64(len(p.data)), ReaderOptions{}); err != nil {
			continue
		}
		x.replay.stagePack(t, h, p.data)
	}
	opened := map[string]bool{}
	for _, e := range events {
		if e.op == "open" {
			opened[e.key] = true
		}
	}
	for key := range mh.keys {
		if !opened[key] {
			x.problemf("hour %d: %q, which a listing returned, was never opened", h, key)
		}
	}
	x.checkConfirm(run, events, snapAt, mh)
}

// commitWitness finds the hour's witness instant: just after the first of the first two consecutive complete traversals
// of its commit objects that agree, which must be the last two;
// and takes the hour's commit set from the commit objects present there, which must be the ones they returned.
// It reports false when no two traversals agree.
func (x *modelExpected) commitWitness(run *modelRun, hist modelHistory, events []memStoreEvent, snapAt int64, mh *modelHour) bool {
	prefix := mh.prefixes[0]
	commits := x.traversals(events, prefix)
	agreed := -1
	for i := 0; i+1 < len(commits); i++ {
		if commits[i].complete && commits[i+1].complete && slices.Equal(commits[i].keys(), commits[i+1].keys()) {
			agreed = i

			break
		}
	}
	if agreed < 0 {
		x.problemf("hour %d: no two consecutive traversals of its %d of the commit objects agree", mh.h, len(commits))

		return false
	}
	if agreed+2 != len(commits) {
		x.problemf("hour %d: %d traversals of the commit objects follow the first two that agree", mh.h, len(commits)-agreed-2)
	}
	if len(commits) > 2 {
		x.stats.repeated++
	}
	mh.witness = commits[agreed].end
	present := hist.under(prefix, mh.witness)
	if !slices.Equal(present, commits[agreed].keys()) {
		x.problemf("hour %d: the commit objects at the witness instant %v differ from the agreeing traversals' %v",
			mh.h, present, commits[agreed].keys())
	}
	for _, key := range present {
		c, err := parseCommitKey(storeTestPrefix, key)
		if err != nil {
			x.problemf("hour %d: commit key %q: %v", mh.h, key, err)

			continue
		}
		mh.ids[c.id] = struct{}{}
	}
	for _, e := range run.events {
		if e.op == "put" && e.at > snapAt && strings.HasPrefix(e.key, prefix) {
			x.stats.lateCommits++
		}
	}

	return true
}

// listedArchives takes the hour's one archive traversal, which, as the staging traversal, must start after the witness instant:
// every key must parse, and every archive key of the capture present at the witness instant must be returned
// and read with the bytes it held then.
// Of the archive packs of the capture it returned, those whose role takes no part in a view are to be reported,
// and ActiveView takes the others, also those uploaded after the witness instant,
// whose commit objects cannot be among those present there, unless a component did not conform.
// It reports false when the hour has no single archive traversal.
func (x *modelExpected) listedArchives(hist modelHistory, events []memStoreEvent, staging *modelTraversal, mh *modelHour) bool {
	prefix := mh.prefixes[1]
	archives := x.traversals(events, prefix)
	if len(archives) != 1 {
		x.problemf("hour %d: %d traversals of the archive packs", mh.h, len(archives))

		return false
	}
	if archives[0].start <= mh.witness || staging.start <= mh.witness {
		x.problemf("hour %d: a pack listing starts before the witness instant", mh.h)
	}
	for _, o := range archives[0].objects {
		a, err := parseArchiveKey(storeTestPrefix, o.Key)
		if err != nil {
			x.problemf("hour %d: the archive traversal returned %q: %v", mh.h, o.Key, err)

			continue
		}
		if a.capture != captureLow {
			continue
		}
		mh.keys[o.Key] = true
		data, _ := x.bytesAt(hist, o.Key, archives[0].end)
		p, err := x.readPack(o.Key, data)
		if err != nil {
			x.problemf("hour %d: an archive's head: %v", mh.h, err)

			continue
		}
		if why := checkModelPack(&p, a.pack, false, 0, mh.h, o.Size, true); why != "" {
			x.problemf("hour %d: archive %q: %s", mh.h, o.Key, why)
		}
		mh.opened[a.pack]++
		if tierRole(p.meta.PackRole) {
			mh.packs = append(mh.packs, &p)
		} else {
			x.excluded = append(x.excluded, ExcludedPack{Key: o.Key, PackID: a.pack, Role: p.meta.PackRole, Hour: mh.h})
		}
	}
	for _, key := range hist.under(prefix, mh.witness) {
		if a, err := parseArchiveKey(storeTestPrefix, key); err == nil && a.capture == captureLow {
			witnessed, _ := hist.at(key, mh.witness)
			read, _ := x.bytesAt(hist, key, archives[0].end)
			x.checkPremises(hist, mh, key, witnessed, read, archives[0].end)
		}
	}

	return true
}

// checkPremises records a problem when key, a pack of the listed hour present at the witness instant with the bytes witnessed,
// was not listed, or was read with other bytes:
// it was deleted or replaced after the witness instant, before its listing, outside the coherent observation's premises,
// under which no pack of a scope that is not indexed is deleted (the tracepack storage specification §5),
// or the source missed it.
func (x *modelExpected) checkPremises(hist modelHistory, mh *modelHour, key string, witnessed, read []byte, listed int64) {
	gone := false
	for _, l := range hist[key] {
		if l.until > mh.witness && l.until <= listed {
			gone = true
		}
	}
	switch {
	case !mh.keys[key] && gone:
		x.problemf("hour %d: %q, present at the witness instant, was deleted before its listing: outside the coherent observation's premises",
			mh.h, key)
	case !mh.keys[key]:
		x.problemf("hour %d: %q, present at the witness instant, is not listed", mh.h, key)
	case !bytes.Equal(witnessed, read):
		x.problemf("hour %d: %q was replaced after the witness instant, before it was read: outside the coherent observation's premises",
			mh.h, key)
	default:
	}
}

// listedSegments adds to the hour's view the segments the staging traversal returned whose period lies in the hour,
// present at the witness instant or uploaded after it;
// every segment of the hour present at the witness instant must be returned and read with the bytes it held then.
func (x *modelExpected) listedSegments(hist modelHistory, segments []modelSegment, listedEnd int64, mh *modelHour) {
	for i := range segments {
		s := &segments[i]
		mh.keys[s.info.Key] = true
		if s.head == nil || !s.head.inHour || s.head.hour != mh.h {
			continue
		}
		if why := checkModelPack(s.head, s.key.pack, true, s.key.seqFirst, mh.h, s.info.Size, true); why != "" {
			x.problemf("hour %d: segment %q: %s", mh.h, s.info.Key, why)
		}
		mh.opened[s.key.pack]++
		if _, before := hist.at(s.info.Key, mh.witness); !before {
			if x.late[mh.h] == nil {
				x.late[mh.h] = map[UUID]bool{}
			}
			x.late[mh.h][s.key.pack] = true
		}
		mh.packs = append(mh.packs, s.head)
	}
	read := map[string][]byte{}
	for i := range segments {
		if segments[i].head != nil {
			read[segments[i].info.Key] = segments[i].head.data
		}
	}
	for _, key := range hist.under(mh.prefixes[2], mh.witness) {
		data, _ := hist.at(key, mh.witness)
		if p, err := x.readPack(key, data); err == nil && p.inHour && p.hour == mh.h {
			x.checkPremises(hist, mh, key, data, read[key], listedEnd)
		}
	}
}

// checkConfirm checks the confirmation of the listed hour: the catalog's one confirmation of it answered true,
// the scope was not indexed at any instant from the snapshot to it,
// and it follows every listing of the hour and every open and read of a pack the hour's listings returned.
func (x *modelExpected) checkConfirm(run *modelRun, events []memStoreEvent, snapAt int64, mh *modelHour) {
	var confirmAt int64
	confirms := 0
	for _, e := range run.log[run.catStart:] {
		if e.op == "confirm" && e.capture == captureLow && e.hour == mh.h {
			confirms++
			confirmAt = e.at
			if !e.indexed {
				x.problemf("hour %d: the catalog did not confirm the scope", mh.h)
			}
			if e.token != mh.token {
				x.problemf("hour %d: confirmed with token %q, not the snapshot's %q", mh.h, e.token, mh.token)
			}
		}
	}
	if confirms != 1 {
		x.problemf("hour %d: %d confirmations", mh.h, confirms)

		return
	}
	for _, e := range run.log {
		if e.op == "index" && e.capture == captureLow && e.hour == mh.h && e.indexed && e.at > snapAt && e.at <= confirmAt {
			x.problemf("hour %d: the scope was indexed at instant %d, between the snapshot and its confirmation", mh.h, e.at)
		}
	}
	for _, e := range events {
		acquired := (e.op == "list" && slices.Contains(mh.prefixes, e.key)) || ((e.op == "open" || e.op == "read") && mh.keys[e.key])
		if acquired && e.at > confirmAt {
			x.problemf("hour %d: %s %q follows its confirmation", mh.h, e.op, e.key)
		}
	}
}

// countView counts the listed view readers of hour h expects: the view, its patches,
// its segments uploaded after the witness instant, and its segments known to hold no record.
func (x *modelExpected) countView(h int64, readers []*Reader) {
	x.stats.nonEmpty++
	for _, r := range readers {
		switch {
		case r.meta.PackRole == PackRoleRepair:
			x.stats.patches++
		case x.late[h][UUID(r.hdr.PackID)]:
			x.stats.lateSegments++
		default:
		}
		if st, ok := r.Stats(); ok && r.meta.PackRole == PackRoleSegment && len(r.Blocks()) == 0 && st.RecordCount == 0 {
			x.stats.emptySegments++
		}
	}
}

// modelGone is an open or a read of a key that was gone at that instant.
type modelGone struct {
	op, key string
}

// modelGoneOps returns every open and read among events of a key gone when the store checked that it exists,
// which fails it with ErrObjectNotFound:
// an open checks at its instant, and a read at its checked instant, after the store's read hook ran.
func modelGoneOps(hist modelHistory, events []memStoreEvent) []modelGone {
	var out []modelGone
	for _, e := range events {
		at := e.at
		switch {
		case e.op == "read" && e.checked != 0:
			at = e.checked
		case e.op != "open":
			continue
		default:
		}
		if _, ok := hist.at(e.key, at); !ok {
			out = append(out, modelGone{op: e.op, key: e.key})
		}
	}

	return out
}

// storeModelFailed checks run, an Observe that failed with err:
// when it opened or read a key that was gone, err must wrap ErrObjectNotFound, and only then.
func storeModelFailed(run *modelRun, err error) *modelExpected {
	x := &modelExpected{}
	gone := modelGoneOps(newModelHistory(run.events), run.events[run.start:])
	notFound := errors.Is(err, ErrObjectNotFound)
	switch {
	case len(gone) > 0 && !notFound:
		x.problemf("%s of %q, which was gone, failed Observe with %v, not ErrObjectNotFound", gone[0].op, gone[0].key, err)
	case len(gone) == 0 && notFound:
		x.problemf("Observe failed with %v, though no object it opened or read was gone", err)
	case notFound:
		x.stats.notFound = 1
		if gone[0].op == "read" {
			x.stats.goneAtRead = 1
		}
	default:
	}

	return x
}

// compare records a problem for every way the observation o of the source, and the packs it reported excluded,
// differ from the model's.
func (x *modelExpected) compare(ctx context.Context, o Observation, from, to int64, excluded []ExcludedPack) {
	want, err := x.replay.Observe(ctx, captureLow, from, to)
	if err != nil {
		x.problemf("the model's observation fails: %v", err)

		return
	}
	defer want.Close()
	for h := from; h < to; h++ {
		w, werr := want.Scope(ctx, h)
		g, gerr := o.Scope(ctx, h)
		if werr != nil || gerr != nil {
			x.problemf("hour %d: Scope: %v, %v", h, werr, gerr)

			continue
		}
		if !slices.Equal(packIDs(w.Readers), packIDs(g.Readers)) || w.Indexed != g.Indexed || w.Conflicted != g.Conflicted {
			x.problemf("hour %d: the source fixed %v (indexed %v, conflicted %v), the model %v (indexed %v, conflicted %v)",
				h, packIDs(g.Readers), g.Indexed, g.Conflicted, packIDs(w.Readers), w.Indexed, w.Conflicted)
		}
		if !w.Indexed && !w.Conflicted && len(w.Readers) > 0 {
			x.countView(h, w.Readers)
		}
		if !w.Indexed && w.Conflicted {
			x.stats.conflicted++
		}
	}
	we, _ := want.Evidence(ctx)
	ge, err := o.Evidence(ctx)
	if err != nil || !assert.ObjectsAreEqual(we, ge) {
		x.problemf("the evidence %+v (%v), the model's %+v", ge, err, we)
	}
	wb, _ := want.Barriers(ctx, from*hourNs, to*hourNs)
	gb, err := o.Barriers(ctx, from*hourNs, to*hourNs)
	if err != nil || !assert.ObjectsAreEqual(wb, gb) {
		x.problemf("the barriers %+v (%v), the model's %+v", gb, err, wb)
	}
	got := slices.Clone(excluded)
	slices.SortFunc(got, func(a, b ExcludedPack) int { return strings.Compare(a.Key, b.Key) })
	if !slices.Equal(got, x.excluded) {
		x.problemf("the source reported %+v excluded, the model %+v", got, x.excluded)
	}
	if x.stats.lateCommits > 0 {
		x.stats.withLateCommit = 1
	}
	if x.stats.lateSegments > 0 {
		x.stats.withLateSegment = 1
	}
}

// storeCase is one store and catalog a store source observes for a lookup.
type storeCase struct {
	store *memStore
	cat   *fakeCatalog
	key   TxKey
	opts  TxOptions
	// edit changes the source's options when set, after its OnExcluded is set to record the reports.
	edit func(o *StoreSourceOptions)
	// catalog, when set, wraps the catalog the source asks.
	catalog func(c Catalog) Catalog
	// mutate, when set, changes the source before it observes.
	mutate func(s *storeSource)
	// afterObserve, when set, runs once an Observe that succeeded was checked, before the lookup reads its observation.
	afterObserve func()
}

// storeCaseResult is what checkStoreCase found.
type storeCaseResult struct {
	// observed reports that the lookup's Observe succeeded.
	observed bool
	// observeErr is the Observe's error.
	observeErr error
	problems   []string
	stats      modelStats
	res        TxResult
	err        error
}

// modelSource is a PackSource whose Observe runs before ahead of its source's, and after on what it returned.
type modelSource struct {
	PackSource
	before func()
	after  func(ctx context.Context, o Observation, err error)
}

// Observe observes through the source between before and after.
func (s modelSource) Observe(ctx context.Context, capture UUID, from, to int64) (Observation, error) {
	s.before()
	o, err := s.PackSource.Observe(ctx, capture, from, to)
	s.after(ctx, o, err)

	return o, err
}

// newStoreCase returns an empty storeCase whose store and catalog share a clock,
// looking up txKeyAt(12) over maxScopes hours.
func newStoreCase(maxScopes int) *storeCase {
	var clock testClock
	c := &storeCase{store: newMemStore(), cat: newFakeCatalog(), key: txKeyAt(12), opts: TxOptions{MaxScopes: maxScopes}}
	c.store.shared, c.cat.shared = &clock, &clock

	return c
}

// checkStoreCase looks up c's key through a store source over c's store and catalog,
// checks the lookup's observation against the acquisition model,
// then looks the key up again through the model's replay of the observation, and returns what differs:
// the observation, the packs reported excluded, the result and its error, the charges, and every object closed once.
// A failed Observe must return no observation and leave the outcome zero,
// and it must fail with ErrObjectNotFound exactly when it opened or read an object that was gone.
func checkStoreCase(t testing.TB, c *storeCase) storeCaseResult {
	t.Helper()

	var excluded excludedLog
	var catalog Catalog = c.cat
	if c.catalog != nil {
		catalog = c.catalog(c.cat)
	}
	src := newTestStoreSource(t, c.store, catalog, func(o *StoreSourceOptions) {
		excluded.reporting(o)
		if c.edit != nil {
			c.edit(o)
		}
	})
	if c.mutate != nil {
		c.mutate(src)
	}

	var out storeCaseResult
	var x *modelExpected
	run := &modelRun{
		from: c.key.Hour, to: c.key.Hour + int64(max(c.opts.MaxScopes, 1)),
		maxMeta: src.opts.Reader.withDefaults().MaxPackMetadataLen,
	}
	ms := modelSource{PackSource: src}
	ms.before = func() {
		run.start, run.catStart = len(c.store.eventLog()), len(c.cat.catalogLog())
	}
	ms.after = func(ctx context.Context, o Observation, err error) {
		out.observeErr = err
		if err != nil {
			if o != nil {
				out.problems = append(out.problems, "a failed Observe returned an observation")
			}
			run.events = c.store.eventLog()
			fx := storeModelFailed(run, err)
			out.problems = append(out.problems, fx.problems...)
			out.stats = fx.stats

			return
		}
		out.observed = true
		run.events, run.log = c.store.eventLog(), c.cat.catalogLog()
		x = storeModel(t, run)
		x.compare(ctx, o, run.from, run.to, excluded.list())
		if so, ok := o.(*storeObservation); ok {
			if n := storeRecount(so); n != so.charged {
				x.problemf("charged %d, recounted %d", so.charged, n)
			}
		}
		if c.afterObserve != nil {
			c.afterObserve()
		}
	}
	out.res, out.err = FindTransaction(t.Context(), ms, c.key, c.opts)
	if x != nil {
		out.problems = append(out.problems, x.problems...)
		out.stats = x.stats
		want, werr := FindTransaction(t.Context(), x.replay, c.key, c.opts)
		switch {
		case (werr == nil) != (out.err == nil) || errors.Is(werr, ErrNotPrimary) != errors.Is(out.err, ErrNotPrimary):
			out.problems = append(out.problems, fmt.Sprintf("the lookup's error %v, the replay's %v", out.err, werr))
		case !assert.ObjectsAreEqual(want, out.res):
			out.problems = append(out.problems, fmt.Sprintf("the lookup returned %+v, the replay %+v", out.res, want))
		default:
		}
	} else if out.res.Outcome != 0 {
		out.problems = append(out.problems, fmt.Sprintf("outcome %s beside a failed Observe", out.res.Outcome))
	}
	opens, closes := c.store.openCounts()
	if !maps.Equal(opens, closes) || c.store.misuseCount() != 0 {
		out.problems = append(out.problems,
			fmt.Sprintf("objects opened %v, closed %v, misused %d times", opens, closes, c.store.misuseCount()))
	}

	return out
}

// requireStoreCase requires that checking c finds nothing, and returns what it found.
func requireStoreCase(t testing.TB, c *storeCase) storeCaseResult {
	t.Helper()

	r := checkStoreCase(t, c)
	require.Empty(t, r.problems)

	return r
}
