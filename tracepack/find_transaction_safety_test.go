package tracepack

import (
	"bytes"
	"cmp"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// The safety check (txTruth.violations) takes a capture as it was written,
// before its packs were cut, damaged or dropped,
// and requires every complete TxNotFound and TxMatched to hold over it,
// with code of its own, independent of the lookup's and of the snapshot-bounded check's.
// It keeps two premises apart (the tracepack semantics specification §7.2, the comparison covers the hours read):
// which seqs exist is taken from the whole capture, every hour included;
// which versions of a seq count, as a reply, a bound, a possible primary or a conflict,
// is restricted to the comparison domain:
// the versions of the capture written into the packs given for the hours the lookup reads.
// A version only in an hour not read never counts as a reply, a bound or a conflict,
// while a seq with no version in the hours read still counts as missing.

// txTruth is a capture as written, for the safety check: the source holding it,
// every seq it wrote, and every pack put in the source with the versions written into it,
// packs of another capture included.
type txTruth struct {
	src     *memSource
	capture UUID
	// seqs holds every seq the capture wrote, in every hour, ascending.
	seqs  []uint64
	packs []txGenPack
}

// txTruthOf returns the ground truth of the generated capture g.
func txTruthOf(g *txGen) *txTruth {
	tr := &txTruth{src: g.src, capture: captureLow, packs: g.packs}
	for _, r := range g.truth {
		tr.seqs = append(tr.seqs, r.Seq)
	}

	return tr
}

// txTruthDomain is the comparison domain of one lookup:
// the distinct versions of each seq of the capture written into the packs given for the hours it reads,
// and those packs, of any capture.
type txTruthDomain struct {
	versions map[uint64][]Record
	seqs     []uint64
	packs    []*txGenPack
}

// domain returns the comparison domain of a lookup of key over maxScopes hours.
func (tr *txTruth) domain(key TxKey, maxScopes int) *txTruthDomain {
	d := &txTruthDomain{versions: map[uint64][]Record{}}
	for i := range tr.packs {
		p := &tr.packs[i]
		if p.hour < key.Hour || p.hour >= key.Hour+int64(maxScopes) {
			continue
		}
		d.packs = append(d.packs, p)
		if p.capture != tr.capture {
			continue
		}
		for _, r := range p.recs {
			vs := d.versions[r.Seq]
			if !slices.ContainsFunc(vs, func(v Record) bool { return txTruthSame(&v, &r) }) {
				d.versions[r.Seq] = append(vs, r)
			}
		}
	}
	for seq := range d.versions {
		d.seqs = append(d.seqs, seq)
	}
	slices.Sort(d.seqs)

	return d
}

// txTruthSame reports whether a and b are one record: every header field and the payload equal.
func txTruthSame(a, b *Record) bool {
	x, y := *a, *b
	x.Payload, y.Payload = nil, nil

	return reflect.DeepEqual(x, y) && bytes.Equal(a.Payload, b.Payload)
}

// txTruthCount returns how many of the ascending seqs lie in [lo, hi].
func txTruthCount(seqs []uint64, lo, hi uint64) uint64 {
	if lo > hi {
		return 0
	}
	i, _ := slices.BinarySearch(seqs, lo)
	j, found := slices.BinarySearch(seqs, hi)
	if found {
		j++
	}

	return uint64(j - i)
}

// txRunsFirstMissing returns the first seq of [lo, hi] that no run of runs holds, by interval arithmetic,
// and whether there is one.
func txRunsFirstMissing(runs []txRun, lo, hi uint64) (uint64, bool) {
	if lo > hi {
		return 0, false
	}
	sorted := slices.SortedFunc(slices.Values(runs), func(a, b txRun) int { return cmp.Compare(a.first, b.first) })
	cur := lo
	for _, r := range sorted {
		if r.last < cur {
			continue
		}
		if r.first > cur {
			return cur, true
		}
		if r.last >= hi {
			return 0, false
		}
		cur = r.last + 1
	}

	return cur, true
}

// txTruthKey is the key and match fields of a primary as the safety check reads them.
type txTruthKey struct {
	epoch    uint32
	dir, opp Dir
	h        HSMSHeader
}

// txTruthHas reports whether rec, a data or control record, has the fields of bits available.
func txTruthHas(rec *Record, bits FieldValidity) bool {
	return rec.HSMSHeader().Available&bits == bits
}

// sameIDs reports whether rec's SessionID and System Bytes are available and equal to the key's,
// and compatible whether each is unavailable or equal.
func (k *txTruthKey) sameIDs(rec *Record) (same, compatible bool) {
	h := rec.HSMSHeader()
	sid, sb := h.Available&FieldValiditySessionID != 0, h.Available&FieldValiditySystemBytes != 0
	sidEq, sbEq := sid && h.SessionID == k.h.SessionID, sb && h.SystemBytes == k.h.SystemBytes

	return sidEq && sbEq, (!sid || sidEq) && (!sb || sbEq)
}

// sameKeyPrimary reports whether rec is a primary of the key: a data record of its epoch and direction,
// an available odd function, SessionID and System Bytes available and equal.
func (k *txTruthKey) sameKeyPrimary(rec *Record) bool {
	same, _ := k.sameIDs(rec)
	h := rec.HSMSHeader()

	return rec.Kind == KindData && rec.Epoch == k.epoch && rec.Dir == k.dir && same &&
		h.Available&FieldValidityFunction != 0 && h.Function%2 == 1
}

// possiblePrimary reports whether rec could be a primary of the key although it is not known to be one.
func (k *txTruthKey) possiblePrimary(rec *Record) bool {
	_, compatible := k.sameIDs(rec)
	h := rec.HSMSHeader()
	fn := h.Available&FieldValidityFunction != 0

	return rec.Kind == KindData && rec.Epoch == k.epoch && !k.sameKeyPrimary(rec) && compatible &&
		(rec.Dir == k.dir || rec.Dir == DirUnknown) && (!fn || h.Function%2 == 1)
}

// reply returns whether rec could answer the primary: a candidate, a valid match, or a record a field keeps undecided.
func (k *txTruthKey) reply(rec *Record) (candidate, valid, undecided bool) {
	if rec.Kind != KindData || rec.Epoch != k.epoch {
		return false, false, false
	}
	same, compatible := k.sameIDs(rec)
	if rec.Dir == k.opp && same {
		h := rec.HSMSHeader()
		decidable := k.h.Available&FieldValidityStreamAndW != 0 && txTruthHas(rec, FieldValidityStreamAndW|FieldValidityFunction)
		valid = decidable && h.Stream == k.h.Stream && (int(h.Function) == int(k.h.Function)+1 || h.Function == 0)

		return true, valid, !decidable
	}

	return false, false, (rec.Dir == k.opp || rec.Dir == DirUnknown) && compatible
}

// closing reports whether rec ends the key's epoch, a socket-close of it, or the capture, a capture-boundary stop,
// and whether it is a capture-boundary stop.
func (k *txTruthKey) closing(rec *Record) (closes, stop bool) {
	if rec.Kind != KindTransportEvent {
		return false, false
	}
	ev, err := UnmarshalTransportEvent(rec.Payload)
	if err != nil {
		return false, false
	}
	stop = ev.Event == EventCaptureBoundary && ev.BoundaryKind != nil && *ev.BoundaryKind == BoundaryKindStop

	return stop || ev.Event == EventSocketClose && rec.Epoch == k.epoch, stop
}

// violations returns how res, the result of a lookup of key over maxScopes scopes of tr's source, breaks what tr shows,
// runs holding the run set of each scope the lookup read; nil when nothing breaks.
// A complete TxNotFound must have an unconflicted primary whose key the result reports, a bound e confirmed by the domain,
// every seq of (p, e) in the runs, which hold seqs of the domain only, every one of them a seq the capture wrote,
// no valid match, no record that could be the reply and no conflict in the domain,
// and none of the conditions that make a lookup incomplete.
// A complete TxMatched must report the domain's one valid match.
// Neither may come from a domain whose packs hold a version outside their hour.
func (tr *txTruth) violations(key TxKey, maxScopes int, res *TxResult, runs [][]txRun) []string {
	if len(res.Incomplete) > 0 || res.Outcome != TxNotFound && res.Outcome != TxMatched {
		return nil
	}
	unmatched := res.Outcome == TxNotFound
	d := tr.domain(key, maxScopes)
	c := &txSafetyCheck{tr: tr, d: d, key: key, maxScopes: maxScopes, res: res}
	c.breaches()
	if !c.primary() {
		return c.out
	}
	e, ok := c.bound()
	if !ok {
		return c.out
	}
	if unmatched {
		c.window(e, runs)
		c.replies(e, true)
		c.conditions(e)
	} else {
		c.replies(e, false)
	}

	return c.out
}

// txSafetyCheck is the safety check of one lookup.
type txSafetyCheck struct {
	tr        *txTruth
	d         *txTruthDomain
	key       TxKey
	maxScopes int
	res       *TxResult
	k         txTruthKey
	out       []string
}

// failf notes a violation, described by format and args.
func (c *txSafetyCheck) failf(format string, args ...any) {
	c.out = append(c.out, fmt.Sprintf(format, args...))
}

// breaches requires every version of every pack of the domain, of any capture, to lie in the pack's hour, the hour of its scope:
// a version outside it breaches the scope (the tracepack format specification I-13), which makes a lookup incomplete.
func (c *txSafetyCheck) breaches() {
	for _, pk := range c.d.packs {
		for i := range pk.recs {
			if h := hourOf(pk.recs[i].TSUTCNs); h != pk.hour {
				c.failf("pack %v of hour %d holds seq %d of hour %d", pk.id, pk.hour, pk.recs[i].Seq, h)
			}
		}
	}
}

// primary requires one version of the primary's seq in the domain, in the hour key.Hour,
// a primary whose key the result reports, and no seq at or above it with several versions.
func (c *txSafetyCheck) primary() bool {
	p := c.key.Seq
	for _, seq := range c.d.seqs {
		if seq >= p && len(c.d.versions[seq]) > 1 {
			c.failf("seq %d has %d versions in the hours read", seq, len(c.d.versions[seq]))
		}
	}
	vs := c.d.versions[p]
	if len(vs) != 1 {
		c.failf("the primary's seq %d has %d versions in the hours read", p, len(vs))
		return false
	}
	rec := &vs[0]
	if hourOf(rec.TSUTCNs) != c.key.Hour {
		c.failf("the primary's version lies in hour %d, not in hour %d", hourOf(rec.TSUTCNs), c.key.Hour)
	}
	h := rec.HSMSHeader()
	c.k = txTruthKey{epoch: rec.Epoch, dir: rec.Dir, opp: DirHostToEquipment, h: h}
	if rec.Dir == DirHostToEquipment {
		c.k.opp = DirEquipmentToHost
	}
	r := c.res
	if r.Epoch != rec.Epoch || r.Dir != rec.Dir || r.SessionID != h.SessionID || r.SystemBytes != h.SystemBytes ||
		r.Function != h.Function || r.StreamAvailable != (h.Available&FieldValidityStreamAndW != 0) {
		c.failf("the key reported, epoch %d %v %#x %x F%d, is not the primary's", r.Epoch, r.Dir, r.SessionID, r.SystemBytes, r.Function)
	}
	if !txTruthHas(rec, FieldValiditySessionID|FieldValiditySystemBytes|FieldValidityFunction) ||
		rec.Dir != DirHostToEquipment && rec.Dir != DirEquipmentToHost {
		c.failf("the primary has no key")
	}
	if rec.Quality.HasAny(QualityCorrelationIncomplete) {
		c.failf("the primary carries correlation-incomplete")
	}

	return true
}

// bound returns e, the smallest seq above the primary
// whose version in the domain is a same-key primary or a closing record,
// and requires the result to report it as the window's end.
func (c *txSafetyCheck) bound() (uint64, bool) {
	for _, seq := range c.d.seqs {
		if seq <= c.key.Seq {
			continue
		}
		v := &c.d.versions[seq][0]
		if closes, _ := c.k.closing(v); closes || c.k.sameKeyPrimary(v) {
			if c.res.WindowEnd == nil || *c.res.WindowEnd != seq {
				c.failf("the window ends at %s, the hours read bound it at %d", txOptSeq(c.res.WindowEnd), seq)
			}

			return seq, true
		}
	}
	if c.res.WindowEnd != nil {
		v := c.d.versions[*c.res.WindowEnd]
		c.failf("the window ends at %d, where the hours read hold %d versions, none a same-key primary or a closing record",
			*c.res.WindowEnd, len(v))
	} else {
		c.failf("the window is not bounded")
	}

	return 0, false
}

// txOptSeq returns the seq p points to as text, or "none".
func txOptSeq(p *uint64) string {
	if p == nil {
		return "none"
	}

	return fmt.Sprint(*p)
}

// window requires every seq of (p, e) to lie in the runs the lookup visited, by interval arithmetic,
// each run above the primary to hold seqs of the domain only, and every seq of (p, e) to be one the capture wrote.
func (c *txSafetyCheck) window(e uint64, runs [][]txRun) {
	p := c.key.Seq
	var all []txRun
	for _, rs := range runs {
		for _, r := range rs {
			if r.last <= p {
				continue
			}
			r.first = max(r.first, p+1)
			if n := txTruthCount(c.d.seqs, r.first, r.last); n != r.last-r.first+1 {
				c.failf("the run [%d, %d] holds %d seqs absent from the hours read", r.first, r.last, r.last-r.first+1-n)
			}
			all = append(all, r)
		}
	}
	if missing, ok := txRunsFirstMissing(all, p+1, e-1); ok {
		c.failf("seq %d of the window (%d, %d) lies in no run the lookup visited", missing, p, e)
	}
	if n := txTruthCount(c.tr.seqs, p+1, e-1); n != e-p-1 {
		c.failf("the capture wrote %d of the %d seqs of the window (%d, %d)", n, e-p-1, p, e)
	}
}

// replies checks the domain's versions inside the window (p, e) against the primary:
// for a TxNotFound, none may be a valid match or a record that could be the reply;
// for a TxMatched, exactly one is a valid match, the one the result reports.
func (c *txSafetyCheck) replies(e uint64, unmatched bool) {
	p := c.key.Seq
	possible := e
	for _, seq := range c.d.seqs {
		if seq > p && seq < possible && c.k.possiblePrimary(&c.d.versions[seq][0]) {
			possible = seq
		}
	}
	var valid []Record
	for _, seq := range c.d.seqs {
		if seq <= p || seq >= e {
			continue
		}
		v := &c.d.versions[seq][0]
		candidate, ok, undecided := c.k.reply(v)
		eligible := seq < possible
		switch {
		case ok && eligible:
			valid = append(valid, *v)
		case unmatched && (undecided || candidate && !eligible):
			c.failf("seq %d could be the reply", seq)
		default:
		}
	}
	if unmatched {
		if len(valid) > 0 {
			c.failf("seq %d is a valid match", valid[0].Seq)
		}

		return
	}
	var got []Record
	for _, v := range c.res.Records {
		if v.Valid {
			got = append(got, v.Record)
		}
	}
	if len(valid) != 1 || len(got) != 1 || !txTruthSame(&valid[0], &got[0]) {
		c.failf("the hours read hold %d valid matches, the result reports %d", len(valid), len(got))
	}
}

// conditions requires none of the conditions that make a lookup incomplete, in the hours read:
// a pack read unfinalized, damaged or unevaluated, of any capture,
// a coverage entry of the capture meeting the window and the hours,
// a stop-unclean of any capture whose gap meets the hours scheduled,
// a capture-boundary of the epoch other than the stop that bounds the window,
// or an ordering-uncertain record of the epoch.
func (c *txSafetyCheck) conditions(e uint64) {
	p := c.key.Seq
	from, to := c.key.Hour*hourNs, (c.key.Hour+int64(c.maxScopes))*hourNs
	for _, pk := range c.d.packs {
		if pk.walked || pk.damaged || !pk.evaluated {
			c.failf("pack %v is unfinalized, damaged or unevaluated", pk.id)
		}
		for _, cv := range pk.coverage {
			if txTruthCoverage(&cv, c.tr.capture, pk.capture, p+1, e-1, from, to) {
				c.failf("a coverage entry of pack %v meets the window", pk.id)
			}
		}
	}
	c.crashGaps(from, to)
	c.boundaries(e)
	for _, seq := range c.d.seqs {
		for _, v := range c.d.versions[seq] {
			if v.Epoch == c.k.epoch && v.Quality.HasAny(QualityOrderingUncertain) {
				c.failf("seq %d of the epoch is ordering-uncertain", seq)
			}
		}
	}
}

// txTruthCoverage reports whether the coverage entry cv of a pack of capture pack, for a lookup of capture,
// meets the seqs [lo, hi] and the times [from, to):
// an entry whose capture, its capture_id else the pack's, is another capture, never, whichever pack holds it;
// an inverted one always, an empty seq range never otherwise.
func txTruthCoverage(cv *Coverage, capture, pack UUID, lo, hi uint64, from, to int64) bool {
	own := pack
	if cv.CaptureID != nil {
		own = *cv.CaptureID
	}
	if own != capture {
		return false
	}
	seqInverted := cv.SeqFirst != nil && cv.SeqLast != nil && *cv.SeqFirst > *cv.SeqLast
	timeInverted := cv.TimeStart != nil && cv.TimeEnd != nil && *cv.TimeStart > *cv.TimeEnd
	if seqInverted || timeInverted {
		return true
	}
	seqs := lo <= hi && (cv.SeqFirst == nil || *cv.SeqFirst <= hi) && (cv.SeqLast == nil || *cv.SeqLast >= lo)

	return seqs && (cv.TimeStart == nil || *cv.TimeStart < to) && (cv.TimeEnd == nil || *cv.TimeEnd >= from)
}

// crashGaps requires no stop-unclean boundary written into a pack of the domain, of any capture,
// whose gap interval meets the hours scheduled [from, to), an inverted one meeting every range.
func (c *txSafetyCheck) crashGaps(from, to int64) {
	for _, pk := range c.d.packs {
		for i := range pk.recs {
			ev := transportEventOf(&pk.recs[i])
			if ev == nil || ev.Event != EventCaptureBoundary || ev.BoundaryKind == nil || *ev.BoundaryKind != BoundaryKindStopUnclean {
				continue
			}
			lo, hi := ev.GapStart, ev.GapEnd
			inverted := lo != nil && hi != nil && *lo > *hi
			if inverted || (lo == nil || *lo < to) && (hi == nil || *hi >= from) {
				c.failf("a stop-unclean of capture %v at seq %d has a gap that meets the hours scheduled", pk.capture, pk.recs[i].Seq)
			}
		}
	}
}

// boundaries requires no capture-boundary of the primary's epoch in the domain above the primary
// other than the stop of the epoch at e that bounds the window.
func (c *txSafetyCheck) boundaries(e uint64) {
	_, stop := c.k.closing(&c.d.versions[e][0])
	stopBound := stop && c.d.versions[e][0].Epoch == c.k.epoch
	bounding := func(seq uint64, kind BoundaryKind) bool { return stopBound && seq == e && kind == BoundaryKindStop }
	for _, seq := range c.d.seqs {
		v := &c.d.versions[seq][0]
		if seq <= c.key.Seq || v.Epoch != c.k.epoch || v.Kind != KindTransportEvent {
			continue
		}
		te, err := UnmarshalTransportEvent(v.Payload)
		if err != nil || te.Event != EventCaptureBoundary {
			continue
		}
		kind := BoundaryKindUnknown
		if te.BoundaryKind != nil {
			kind = *te.BoundaryKind
		}
		if !bounding(seq, kind) {
			c.failf("seq %d is a %v boundary of the epoch", seq, kind)
		}
	}
}

// txLookupRuns returns the run set of each hour the lookup l read.
func txLookupRuns(l *txLookup) [][]txRun {
	out := make([][]txRun, len(l.reads))
	for i, rd := range l.reads {
		out[i] = slices.Clone(rd.runs.runs)
	}

	return out
}

// runTxSafety runs a lookup of key with opts over tr's source, the lookup changed by mutate when set,
// and returns its result and the violations of the safety check.
func runTxSafety(t testing.TB, tr *txTruth, key TxKey, opts TxOptions, mutate func(l *txLookup)) (TxResult, []string) {
	t.Helper()

	var l *txLookup
	res, err := findTransactionWith(t.Context(), tr.src, key, opts, func(lk *txLookup) {
		l = lk
		if mutate != nil {
			mutate(lk)
		}
	})
	require.NoError(t, err)
	requireStateRecount(t, l)

	return res, tr.violations(key, l.opts.MaxHours, &res, txLookupRuns(l))
}

// txTruthPack writes recs, already in hour memTestHour + hours, into a finalized pack id of captureLow
// whose pack metadata has quality_evaluated true, in blocks closed after the seqs of flushAfter,
// gives it to tr's source for its hour, and records it in tr.
func txTruthPack(t testing.TB, tr *txTruth, id UUID, hours int64, flushAfter []uint64, recs ...Record) {
	t.Helper()

	steps := make([]footerTestStep, len(recs))
	for i, r := range recs {
		steps[i] = footerTestStep{rec: r, flush: slices.Contains(flushAfter, r.Seq)}
	}
	file := writeRepairPack(t, CodecZstd, func(m *PackMeta) {
		m.PeriodStart += hours * hourNs
		m.PeriodEnd += hours * hourNs
		m.QualityEvaluated = true
	}, false, steps).file
	file = withIDs(t, file, id, captureLow)
	tr.src.addPack(t, memTestHour+hours, file)
	tr.packs = append(tr.packs, txGenPack{hour: memTestHour + hours, id: id, capture: captureLow, recs: recs, evaluated: true, file: file})
}

// newTxTruth returns an empty truth over a fresh source of captureLow that wrote seqs.
func newTxTruth(seqs []uint64) *txTruth {
	return &txTruth{src: newMemSource(), capture: captureLow, seqs: seqs}
}

// txSeqRange returns the seqs first to last.
func txSeqRange(first, last uint64) []uint64 {
	var out []uint64
	for seq := first; seq <= last; seq++ {
		out = append(out, seq)
	}

	return out
}

// TestFindTransactionSafetyCheckCatches shows that the safety check catches a lookup without seq-gap detection.
// Each source holds txRecord's primary at 12 in memTestHour, read with MaxHours 1, and the capture wrote 12 to 14.
// Without seq-gap detection, the lookup returns a complete TxNotFound, which the check rejects,
// for an annotation at 13 that a backward clock step put in the hour before, which is not read,
// for an annotation at 13 dropped from every pack, and for 13 inside the F-2 range of a block holding 12 and 14 only;
// for 13 only in a walked block whose read fails, the read's own gaps keep the lookup incomplete,
// and the visited runs still lack 13.
func TestFindTransactionSafetyCheckCatches(t *testing.T) {
	t.Parallel()

	note := func(seq uint64, hours int64) Record { return inHour(hours, txNote(seq, []byte("x"))) }
	tests := []struct {
		name string
		// source fills the truth of a capture that wrote 12 to last.
		source func(t *testing.T, tr *txTruth)
		last   uint64
		mutate func(l *txLookup)
		// normal is the outcome and gaps of the lookup as it is, mutated those of the mutated lookup.
		normal, mutated []string
		// violation is a part of the violation the mutated lookup must show; empty when it shows none.
		violation string
	}{
		{
			name: "a non-reply in the hour before", last: 14, mutate: func(l *txLookup) { l.skipSeqGap = true },
			source: func(t *testing.T, tr *txTruth) {
				txTruthPack(t, tr, seg0, 0, nil, txRecord(12, nil), txRecord(14, nil))
				txTruthPack(t, tr, seg1, -1, nil, note(13, -1))
			},
			normal: []string{"not-found", "seq-gap@13"}, mutated: []string{"not-found"},
			violation: "seq 13 of the window (12, 14) lies in no run the lookup visited",
		},
		{
			name: "a dropped annotation", last: 14, mutate: func(l *txLookup) { l.skipSeqGap = true },
			source: func(t *testing.T, tr *txTruth) {
				txTruthPack(t, tr, seg0, 0, []uint64{12}, txRecord(12, nil), txRecord(14, nil))
			},
			normal: []string{"not-found", "seq-gap@13"}, mutated: []string{"not-found"},
			violation: "seq 13 of the window (12, 14) lies in no run the lookup visited",
		},
		{
			name: "a seq only in an F-2 range", last: 14, mutate: func(l *txLookup) { l.skipSeqGap = true },
			source: func(t *testing.T, tr *txTruth) {
				txTruthPack(t, tr, seg0, 0, nil, txRecord(12, nil), txRecord(14, nil))
				r := mustOpen(t, tr.packs[0].file, ReaderOptions{})
				require.Len(t, r.Blocks(), 1)
				require.Equal(t, [2]uint64{12, 14}, [2]uint64{r.Blocks()[0].FirstSeq, r.Blocks()[0].LastSeq})
			},
			normal: []string{"not-found", "seq-gap@13"}, mutated: []string{"not-found"},
			violation: "seq 13 of the window (12, 14) lies in no run the lookup visited",
		},
		{
			name: "a seq only in a failed walked block", last: 14, mutate: func(l *txLookup) { l.skipSeqGap = true },
			source: func(t *testing.T, tr *txTruth) {
				txTruthPack(t, tr, seg0, 0, nil, txRecord(12, nil), txRecord(14, nil))
				walked := writeRepairPack(t, CodecZstd, func(m *PackMeta) { m.QualityEvaluated = true }, true, txBlock(note(13, 0))).file
				b := mustOpen(t, walked, ReaderOptions{}).Blocks()[0]
				tr.src.addPack(t, memTestHour, withIDs(t, flipByte(walked, b.Offset+format.EnvelopeLen+1), seg1, captureLow))
				tr.packs = append(tr.packs, txGenPack{hour: memTestHour, id: seg1, capture: captureLow, recs: []Record{note(13, 0)},
					evaluated: true, walked: true, damaged: true})
			},
			normal:  []string{"not-found", "read@-", "read@-", "seq-gap@13"},
			mutated: []string{"not-found", "read@-", "read@-"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tr := newTxTruth(txSeqRange(12, tt.last))
			tt.source(t, tr)
			opts := TxOptions{MaxHours: 1}

			res, out := runTxSafety(t, tr, txKeyAt(12), opts, nil)
			assert.Equal(t, tt.normal, txOutcomeGaps(&res))
			assert.Empty(t, out, "the lookup as it is")

			res, l, out := runTxSafetyKeeping(t, tr, txKeyAt(12), opts, tt.mutate)
			assert.Equal(t, tt.mutated, txOutcomeGaps(&res))
			if tt.violation == "" {
				assert.Empty(t, out)
				missing, ok := txRunsFirstMissing(slices.Concat(txLookupRuns(l)...), 13, 13)
				assert.True(t, ok && missing == 13, "13 is not visited")

				return
			}
			t.Logf("the safety check rejects the mutated lookup: %s", strings.Join(out, "; "))
			assert.Contains(t, out, tt.violation)
		})
	}
}

// runTxSafetyKeeping is runTxSafety, returning the lookup too.
func runTxSafetyKeeping(t testing.TB, tr *txTruth, key TxKey, opts TxOptions, mutate func(l *txLookup)) (TxResult, *txLookup, []string) {
	t.Helper()

	var l *txLookup
	res, out := runTxSafety(t, tr, key, opts, func(lk *txLookup) {
		l = lk
		if mutate != nil {
			mutate(lk)
		}
	})

	return res, l, out
}

// txOutcomeGaps describes res as its outcome, then each gap as "reason@seq", "-" for no seq.
func txOutcomeGaps(res *TxResult) []string {
	out := make([]string, 0, 1+len(res.Incomplete))
	out = append(out, res.Outcome.String())
	for _, g := range res.Incomplete {
		out = append(out, fmt.Sprintf("%v@%s", g.Reason, strings.Replace(txOptSeq(g.Seq), "none", "-", 1)))
	}

	return out
}

// TestFindTransactionUnreadEarlierHour pins that the comparison of a record's versions covers the hours a lookup reads
// (the tracepack semantics specification §7.2):
// beside txRecord's primary at 12 and a same-key primary at 14 in hour memTestHour + 1, read with MaxHours 1,
// another version of 13 lies in memTestHour, where a backward clock step put it:
// a valid reply, a smaller same-key primary, or an unrelated record.
// With an annotation at 13 in the hour read, the lookup returns a complete TxNotFound, as the hours read imply,
// and the snapshot-bounded and safety checks agree, the version in the hour before being outside the comparison;
// with no version of 13 in the hour read, 13 is a seq gap, and the TxNotFound is incomplete.
func TestFindTransactionUnreadEarlierHour(t *testing.T) {
	t.Parallel()

	earlier := []struct {
		name string
		rec  Record
	}{
		{name: "a valid reply", rec: txReply(13, nil)},
		{name: "a smaller same-key primary", rec: txRecord(13, nil)},
		{name: "an unrelated record", rec: txRecord(13, txFrame(func(p []byte) { p[fieldSystemBytesOff] = 0 }))},
	}
	for _, e := range earlier {
		for _, read := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s, a version read %t", e.name, read), func(t *testing.T) {
				t.Parallel()

				tr := newTxTruth(txSeqRange(12, 14))
				recs := []Record{inHour(1, txRecord(12, nil)), inHour(1, txRecord(14, nil))}
				if read {
					recs = slices.Insert(recs, 1, inHour(1, txNote(13, []byte("x"))))
				}
				txTruthPack(t, tr, seg0, 1, nil, recs...)
				txTruthPack(t, tr, seg1, 0, nil, e.rec)
				key := TxKey{CaptureID: captureLow, Seq: 12, Hour: memTestHour + 1}
				opts := TxOptions{MaxHours: 1}

				res, out := runTxSafety(t, tr, key, opts, nil)
				assert.Empty(t, out)
				want := []string{"not-found"}
				if !read {
					want = []string{"not-found", "seq-gap@13"}
				}
				assert.Equal(t, want, txOutcomeGaps(&res))
				assert.Equal(t, []string{"12@1 primary", "14@1 same-key-primary"}, recordSeqs(res.Records))

				snap := newTxSnapshot(t, tr.src, captureLow, memTestHour, memTestHour+2)
				exp, notPrimary := snap.expect(key, opts.MaxHours)
				requireTxExpected(t, exp, notPrimary, res, nil)
			})
		}
	}
}
