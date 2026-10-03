package tracepack

import (
	"cmp"
	"context"
	"fmt"
	"slices"
)

// sTypeRejectReq is the SType of a Reject.req control message (SEMI E37 §8.3.20).
const sTypeRejectReq = 7

// txPrimaryKey is what a transaction lookup classifies records against:
// the association key and match fields of the first version of the primary its first scope read yielded.
type txPrimaryKey struct {
	// keyed reports that the version is a primary with an association key; a version without one classifies nothing.
	keyed bool
	epoch uint32
	// dir is the primary's direction, and opp the opposite one.
	dir, opp    Dir
	sessionID   uint16
	systemBytes [4]byte
	// stream is valid iff streamAvailable; function is available on every keyed primary.
	stream          uint8
	streamAvailable bool
	function        uint8
}

// txRun is a run of consecutive seqs, first to last, both included.
type txRun struct {
	first, last uint64
}

// txRuns is the run set of one scope read: the seqs at or above the primary's that the read yielded,
// as sorted runs, disjoint and not adjacent.
// Within a read, seqs ascend unless a block disagrees with its F-2 entry,
// so an insertion almost always extends or follows the last run.
type txRuns struct {
	hour int64
	runs []txRun
}

// txCrossConflict is a seq that the reads of several scopes yielded, a conflict across scope reads.
type txCrossConflict struct {
	// hours holds the hours whose reads yielded the seq, ascending.
	hours []int64
	// gap is the index of the conflict's TxGapConflict gap in the result's Gaps; -1 until the gap is added.
	gap int
}

// txGroup is the seq group a scope read is yielding: the versions of one seq, which arrive one after the other.
type txGroup struct {
	open bool
	seq  uint64
	// keep reports that a version of the group plays a role, so that every version buffered is kept.
	keep bool
	// conflict reports that the group's conflict within the read has its TxGapConflict gap.
	conflict bool
	// versions holds the versions buffered, each cloned as it arrived.
	versions []TxRecord
}

// txPendingGap is a conflict gap a scope read found, added to the result after the read's other gaps.
type txPendingGap struct {
	gap TxGap
	// cross is the conflict across scope reads whose hours the gap lists; nil for a conflict within the read,
	// or for the TxGapNoKey gap that a later version of the primary adds.
	cross *txCrossConflict
}

// txSeen is where a scope read yielded a record:
// the read's hour, the record's seq, and its representative's pack and block.
type txSeen struct {
	hour  int64
	seq   uint64
	pack  UUID
	block int
}

// txBoundaryRecord is a capture-boundary record of the primary's epoch, above the primary, that a scope read yielded,
// one per version.
type txBoundaryRecord struct {
	at txSeen
	// boundary is the record as a Boundary: its seq, kind (BoundaryKindUnknown when its payload has none), ts_utc_ns, epoch
	// and gap bounds, the pointers its own.
	boundary Boundary
}

// primaryKeyOf returns what a lookup classifies records against, taken from rec, a version of the primary.
func primaryKeyOf(rec *Record) txPrimaryKey {
	h := rec.HSMSHeader()
	k := txPrimaryKey{
		keyed: rec.Kind == KindData && h.Available&FieldValidityFunction != 0 && h.Function%2 == 1 &&
			keyMissing(rec.Dir, h.Available) == "",
		epoch: rec.Epoch, dir: rec.Dir, opp: DirEquipmentToHost,
		sessionID: h.SessionID, systemBytes: h.SystemBytes,
		stream: h.Stream, streamAvailable: h.Available&FieldValidityStreamAndW != 0, function: h.Function,
	}
	if rec.Dir == DirEquipmentToHost {
		k.opp = DirHostToEquipment
	}

	return k
}

// classify returns the roles that rec, a version with a seq above the primary's, plays against k,
// by the definitions of the tracepack semantics specification §7.2, comparing available fields only:
// TxCandidate, TxPossibleReply, TxSameKeyPrimary and TxPossiblePrimary for a data record,
// TxOutcomeRecord for a Reject.req control record or a T3 timer-expiry transport event,
// and TxClosing for a socket-close transport event of the primary's epoch or a capture-boundary stop of any epoch.
// The roles come from the version's own bytes: whether a conflict keeps a closing record from bounding the window
// is decided once every scope is read.
//
// Returns:
//   - TxClass: the roles; zero for none.
//   - *TransportEvent: the decoded payload of a transport-event record;
//     nil for any other record or a payload that does not decode.
func (k *txPrimaryKey) classify(rec *Record) (TxClass, *TransportEvent) {
	switch {
	case rec.Kind == KindData:
		return k.classifyData(rec), nil
	case rec.Kind == KindControl && k.isReject(rec):
		return TxOutcomeRecord, nil
	case rec.Kind != KindTransportEvent:
		return 0, nil
	}
	ev, err := UnmarshalTransportEvent(rec.Payload)
	if err != nil {
		return 0, nil
	}

	return k.classifyEvent(rec, ev), ev
}

// keyFields compares the SessionID and System Bytes of h with the primary's.
//
// Returns:
//   - equal: both are available and equal to the primary's.
//   - compatible: each is unavailable or equal to the primary's.
func (k *txPrimaryKey) keyFields(h *HSMSHeader) (equal, compatible bool) {
	sid := h.Available&FieldValiditySessionID != 0
	sb := h.Available&FieldValiditySystemBytes != 0
	sidEqual := sid && h.SessionID == k.sessionID
	sbEqual := sb && h.SystemBytes == k.systemBytes

	return sidEqual && sbEqual, (!sid || sidEqual) && (!sb || sbEqual)
}

// classifyData returns the roles of the data record rec in the primary's capture:
// a candidate or a possible reply, and a same-key primary or a possible same-key primary;
// a record in unknown direction can be both a possible reply and a possible same-key primary.
// A record of another epoch, or one whose available SessionID or System Bytes differ from the primary's, plays none.
func (k *txPrimaryKey) classifyData(rec *Record) TxClass {
	if rec.Epoch != k.epoch {
		return 0
	}
	h := rec.HSMSHeader()
	equal, compatible := k.keyFields(&h)
	if !compatible {
		return 0
	}

	var c TxClass
	switch {
	case rec.Dir == k.opp && equal:
		c |= TxCandidate
	case rec.Dir == k.opp || rec.Dir == DirUnknown:
		c |= TxPossibleReply
	default:
		// The primary's direction, or local: no reply.
	}

	function := h.Available&FieldValidityFunction != 0
	if function && h.Function%2 == 0 {
		return c
	}
	switch {
	case rec.Dir == k.dir && function && equal:
		c |= TxSameKeyPrimary
	case rec.Dir == k.dir || rec.Dir == DirUnknown:
		c |= TxPossiblePrimary
	default:
		// The opposite direction, or local: no primary of the same key.
	}

	return c
}

// isReject reports whether the control record rec is a Reject.req of the primary:
// SType available and 7, the opposite direction, the primary's epoch, SessionID and System Bytes available and equal.
func (k *txPrimaryKey) isReject(rec *Record) bool {
	if rec.Epoch != k.epoch || rec.Dir != k.opp {
		return false
	}
	h := rec.HSMSHeader()
	equal, _ := k.keyFields(&h)

	return equal && h.Available&FieldValiditySType != 0 && h.SType == sTypeRejectReq
}

// classifyEvent returns the roles of the transport-event record rec whose payload decodes to ev:
// TxClosing for a socket-close of the primary's epoch or a capture-boundary stop of any epoch,
// TxOutcomeRecord for a T3 timer-expiry of the primary's epoch that names the primary.
func (k *txPrimaryKey) classifyEvent(rec *Record, ev *TransportEvent) TxClass {
	if ev.Event == EventSocketClose && rec.Epoch == k.epoch {
		return TxClosing
	}
	if ev.Event == EventCaptureBoundary && ev.BoundaryKind != nil && *ev.BoundaryKind == BoundaryKindStop {
		return TxClosing
	}
	if ev.Event == EventTimerExpiry && rec.Epoch == k.epoch && k.namesPrimary(ev) {
		return TxOutcomeRecord
	}

	return 0
}

// namesPrimary reports whether the timer-expiry ev is a T3 expiry of the primary:
// primary_session_id and primary_system_bytes present and equal to the primary's, compared as a uint64 and as 4 bytes,
// and primary_stream and primary_function each equal to the primary's where present on ev and available on the primary.
func (k *txPrimaryKey) namesPrimary(ev *TransportEvent) bool {
	return ev.Timer != nil && *ev.Timer == TimerT3 &&
		ev.PrimarySessionID != nil && *ev.PrimarySessionID == uint64(k.sessionID) &&
		ev.PrimarySystemBytesPresent && ev.PrimarySystemBytes == k.systemBytes &&
		(ev.PrimaryStream == nil || !k.streamAvailable || *ev.PrimaryStream == uint64(k.stream)) &&
		(ev.PrimaryFunction == nil || *ev.PrimaryFunction == uint64(k.function))
}

// insert adds seq to the run set.
// A new run is charged to b before it is added (txRunCharge), and the charge of a run that seq joins to the next is released.
//
// Returns:
//   - error: the charge's error, wrapping ErrReadLimit, seq not added.
func (s *txRuns) insert(seq uint64, b *txBudget) error {
	n := len(s.runs)
	if n > 0 && seq > s.runs[n-1].last {
		if seq-s.runs[n-1].last == 1 {
			s.runs[n-1].last = seq
			return nil
		}
		if err := b.reserve(txRunCharge); err != nil {
			return err
		}
		s.runs = append(s.runs, txRun{first: seq, last: seq})

		return nil
	}
	if n == 0 {
		if err := b.reserve(txRunCharge); err != nil {
			return err
		}
		s.runs = append(s.runs, txRun{first: seq, last: seq})

		return nil
	}

	// Out of order: seq lies at or before the last run.
	i, _ := slices.BinarySearchFunc(s.runs, seq, func(r txRun, seq uint64) int { return cmp.Compare(r.last, seq) })
	if s.runs[i].first <= seq {
		return nil
	}
	// runs[i-1].last < seq < runs[i].first.
	prev := i > 0 && s.runs[i-1].last+1 == seq
	next := s.runs[i].first-1 == seq
	switch {
	case prev && next:
		s.runs[i-1].last = s.runs[i].last
		s.runs = slices.Delete(s.runs, i, i+1)
		b.release(txRunCharge)
	case prev:
		s.runs[i-1].last = seq
	case next:
		s.runs[i].first = seq
	default:
		if err := b.reserve(txRunCharge); err != nil {
			return err
		}
		s.runs = slices.Insert(s.runs, i, txRun{first: seq, last: seq})
	}

	return nil
}

// contains reports whether seq lies in a run of the set.
func (s *txRuns) contains(seq uint64) bool {
	i, _ := slices.BinarySearchFunc(s.runs, seq, func(r txRun, seq uint64) int { return cmp.Compare(r.last, seq) })

	return i < len(s.runs) && s.runs[i].first <= seq
}

// searchWindow reads the scopes after key.Hour, each once, in order, and evaluates the window of a keyed primary.
// Each read's kept versions and conflict gaps are added to the result after its other gaps, also when the read fails,
// and every kept version of a seq in conflict, within a read or across reads, is then marked as a conflict (markConflicts).
//
// Returns:
//   - error: the error of a read, wrapped with its hour; ctx's error, wrapped; the error of the evaluation.
func (l *txLookup) searchWindow(ctx context.Context) error {
	for i := 1; i < l.opts.MaxScopes; i++ {
		rd, err := l.readScope(ctx, l.key.Hour+int64(i), l.collect)
		if rd != nil {
			if cerr := l.commitRead(ctx, rd); cerr != nil && err == nil {
				err = fmt.Errorf("tracepack: find transaction: %w", cerr)
			}
		}
		if err != nil {
			// The marks complete what the result holds so far; the read's error is the one returned.
			_ = l.markConflicts(ctx)

			return err
		}
	}
	if err := l.markConflicts(ctx); err != nil {
		return fmt.Errorf("tracepack: find transaction: %w", err)
	}

	return l.evaluate(ctx)
}

// collect takes an item of the read rd.
//
// Every item whose record carries ordering-uncertain notes its epoch, whatever its seq.
// An item at or above the primary's seq enters the read's run set and its seq group (beginGroup),
// or, joining the open group, adds the group's conflict gap when it is the group's first version marked as a conflict (localConflict).
// In scope key.Hour, a version of the primary is cloned and kept, the first one taken as the key to classify against,
// and an item above the primary that arrives before any version of it is counted and its seq noted, not classified.
// Every other item at or above the primary's seq is classified and buffered in its group (addVersion).
// Each item counts toward the checks of ctx (tick), and what it adds to the state is charged before it is added.
//
// Returns:
//   - error: ctx's error, as is; the error of a conflict reservation or of a charge, which ends the read.
func (l *txLookup) collect(ctx context.Context, rd *txScopeRead, it *Item) error {
	if err := l.tick(ctx); err != nil {
		return err
	}
	rec := &it.Record
	if rec.Quality&QualityOrderingUncertain != 0 {
		if err := l.noteOrderingUncertain(rd, it); err != nil {
			return err
		}
	}
	if rec.Seq < l.key.Seq {
		return nil
	}
	if !rd.group.open || rd.group.seq != rec.Seq {
		l.endGroup(rd)
		if err := l.beginGroup(ctx, rd, it); err != nil {
			return err
		}
	} else if err := l.localConflict(rd, it); err != nil {
		return err
	}

	first := rd.hour == l.key.Hour
	switch {
	case first && rec.Seq == l.key.Seq:
		v, err := l.cloneVersion(rd, it, TxPrimary)
		if err != nil {
			return err
		}
		l.primary, l.primaryFlags = append(l.primary, v), append(l.primaryFlags, l.flags)
		if len(l.primary) == 1 {
			l.prim = primaryKeyOf(&l.primary[0].Record)
		}
	case first && len(l.primary) == 0:
		if err := l.earlyRuns.insert(rec.Seq, &l.state); err != nil {
			return err
		}
		l.early++
	default:
		return l.addVersion(rd, it)
	}

	return nil
}

// noteOrderingUncertain records the epoch of it's record, which carries ordering-uncertain, with where it was first seen,
// charging the entry before it is added.
//
// Returns:
//   - error: the charge's error, wrapping ErrReadLimit.
func (l *txLookup) noteOrderingUncertain(rd *txScopeRead, it *Item) error {
	epoch := it.Record.Epoch
	if _, ok := l.uncertain[epoch]; ok {
		return nil
	}
	if err := l.state.reserve(txUncertainCharge); err != nil {
		return err
	}
	if l.uncertain == nil {
		l.uncertain = make(map[uint32]txSeen)
	}
	l.uncertain[epoch] = seenAt(rd, it)

	return nil
}

// seenAt returns where the read rd yielded it.
func seenAt(rd *txScopeRead, it *Item) txSeen {
	return txSeen{hour: rd.hour, seq: it.Record.Seq, pack: rd.packs[it.Pack], block: it.Block}
}

// beginGroup starts the seq group of it, the first version of its seq in this stretch of the read rd:
// the seq enters the read's run set;
// a version marked as a conflict within the read adds a TxGapConflict gap (localConflict);
// and, in a later scope, a seq that an earlier read yielded is a conflict across scope reads (crossConflict).
//
// Returns:
//   - error: the error of the conflict reservation; the error of a charge, wrapping ErrReadLimit; ctx's error, as is.
func (l *txLookup) beginGroup(ctx context.Context, rd *txScopeRead, it *Item) error {
	seq := it.Record.Seq
	if err := rd.runs.insert(seq, &l.state); err != nil {
		return err
	}
	rd.group.open, rd.group.seq = true, seq
	if err := l.localConflict(rd, it); err != nil {
		return err
	}
	if rd.hour == l.key.Hour {
		return nil
	}

	return l.crossConflict(ctx, rd, seq)
}

// localConflict adds a TxGapConflict gap for the seq group of the read rd when it, a version of the group,
// is marked as a conflict within the read and the group has no such gap yet,
// except at the primary in scope key.Hour, whose settlement reports it;
// the seq is noted as in conflict, charged before it is added, so that every kept version of it is marked (markConflicts).
// A version that a block disagreeing with its F-2 entry let arrive uncompared can open the group
// before the versions the read compared, or form a group of its own, so every version of the group is looked at, not only its first,
// and the copies of the seq that arrived uncompared are marked as well.
//
// Returns:
//   - error: the error of a charge, wrapping ErrReadLimit.
func (l *txLookup) localConflict(rd *txScopeRead, it *Item) error {
	seq := it.Record.Seq
	if !it.Conflict || rd.group.conflict || rd.hour == l.key.Hour && seq == l.key.Seq {
		return nil
	}
	if _, ok := l.conflicted[seq]; !ok {
		if err := l.state.reserve(txConflictedCharge); err != nil {
			return err
		}
		if l.conflicted == nil {
			l.conflicted = make(map[uint64]struct{})
		}
		l.conflicted[seq] = struct{}{}
	}
	if err := l.pendGap(rd, txPendingGap{gap: TxGap{
		Reason: TxGapConflict, Hours: []int64{rd.hour}, Block: -1, Offset: -1, Seq: new(seq),
		Err: fmt.Errorf("the read of hour %d yields several versions of seq %d", rd.hour, seq),
	}}); err != nil {
		return err
	}
	rd.group.conflict = true

	return nil
}

// crossConflict checks seq, which the read rd of a later scope yields, against the run sets of the earlier reads.
// A seq already in conflict across reads adds rd's hour to it.
// A seq an earlier read yielded becomes a conflict across reads:
// it reserves one conflict of the lookup and adds a TxGapConflict gap that lists the hours;
// at the primary's seq it also adds a TxGapNoKey gap, since the primary is no longer established.
//
// A new entry is charged with its two hours and its gaps in one charge before any of them is added,
// and a later hour is charged before it is added.
// Each probe of an earlier read's run set counts toward the checks of ctx (tick).
//
// Returns:
//   - error: the error of the conflict reservation, wrapped with the seq and hours; the error of a charge, wrapping ErrReadLimit;
//     ctx's error, as is.
func (l *txLookup) crossConflict(ctx context.Context, rd *txScopeRead, seq uint64) error {
	if c, ok := l.cross[seq]; ok {
		if c.hours[len(c.hours)-1] != rd.hour {
			if err := l.state.reserve(txHourCharge); err != nil {
				return err
			}
			c.hours = append(c.hours, rd.hour)
			if c.gap >= 0 {
				l.res.Gaps[c.gap].Hours = c.hours
			}
		}

		return nil
	}

	earlier := l.reads[:len(l.reads)-1]
	at := -1
	for k, r := range earlier {
		if len(r.runs.runs) == 0 {
			continue
		}
		if err := l.tick(ctx); err != nil {
			return err
		}
		if r.runs.contains(seq) {
			at = k
			break
		}
	}
	if at < 0 {
		return nil
	}
	hour := earlier[at].hour
	if err := l.reserveConflict(); err != nil {
		return fmt.Errorf("the conflict at seq %d between hours %d and %d: %w", seq, hour, rd.hour, err)
	}

	gaps := []txPendingGap{{gap: TxGap{
		Reason: TxGapConflict, Block: -1, Offset: -1, Seq: new(seq),
		Err: fmt.Errorf("the reads of several hours yield seq %d", seq),
	}}}
	if seq == l.key.Seq {
		gaps = append(gaps, txPendingGap{gap: TxGap{
			Reason: TxGapNoKey, Hours: []int64{l.key.Hour}, Block: -1, Offset: -1, Seq: new(seq),
			Err: fmt.Errorf("the primary conflicts with a version in hour %d", rd.hour),
		}})
	}
	// The entry, its two hours and its gaps are charged at once, so that a failed charge adds none of them.
	cost := txCrossCharge + 2*txHourCharge
	for k := range gaps {
		cost += gapCost(&gaps[k].gap)
	}
	if err := l.state.reserve(cost); err != nil {
		return err
	}

	c := &txCrossConflict{hours: []int64{hour, rd.hour}, gap: -1}
	if l.cross == nil {
		l.cross = make(map[uint64]*txCrossConflict)
	}
	l.cross[seq] = c
	gaps[0].cross = c
	rd.gaps = append(rd.gaps, gaps...)

	return nil
}

// pendGap charges the gap of pg, then adds pg to the read rd's conflict gaps, which commitRead adds to the result.
// The gap of a conflict across scope reads has no Hours yet: they are the conflict's, which crossConflict charged.
//
// Returns:
//   - error: the charge's error, wrapping ErrReadLimit.
func (l *txLookup) pendGap(rd *txScopeRead, pg txPendingGap) error {
	if err := l.state.reserve(gapCost(&pg.gap)); err != nil {
		return err
	}
	rd.gaps = append(rd.gaps, pg)

	return nil
}

// addVersion classifies the version it of the read rd's open group and buffers it:
// a later scope's version of the primary is TxPrimary, any other version takes the roles classify gives,
// and a capture-boundary record of the primary's epoch is noted.
// A version that plays no role, and that the read compared with no other copy (Item.Conflict false),
// is the only version of its seq in the read, unless a block disagrees with its F-2 entry,
// so it is dropped without a copy, unless the group already buffers a version.
// A version buffered is charged when it is cloned (cloneVersion).
//
// Returns:
//   - error: the error of a charge, wrapping ErrReadLimit.
func (l *txLookup) addVersion(rd *txScopeRead, it *Item) error {
	if !l.prim.keyed {
		// The first version of the primary has no key: the lookup ends after this read, with nothing to classify.
		return nil
	}

	rec := &it.Record
	g := &rd.group
	class := TxPrimary
	if rec.Seq != l.key.Seq {
		var ev *TransportEvent
		class, ev = l.prim.classify(rec)
		if ev != nil && ev.Event == EventCaptureBoundary && rec.Epoch == l.prim.epoch {
			if err := l.noteBoundary(rd, it, ev); err != nil {
				return err
			}
		}
	}
	if class == 0 && !it.Conflict && len(g.versions) == 0 {
		return nil
	}
	v, err := l.cloneVersion(rd, it, class)
	if err != nil {
		return err
	}
	g.keep = g.keep || class != 0
	g.versions = append(g.versions, v)

	return nil
}

// noteBoundary records the capture-boundary record of it, whose payload decodes to ev, charging it before it is added.
//
// Returns:
//   - error: the charge's error, wrapping ErrReadLimit.
func (l *txLookup) noteBoundary(rd *txScopeRead, it *Item, ev *TransportEvent) error {
	if err := l.state.reserve(txBoundaryRecordCharge); err != nil {
		return err
	}
	kind := BoundaryKindUnknown
	if ev.BoundaryKind != nil {
		kind = *ev.BoundaryKind
	}
	rec := &it.Record
	l.boundaries = append(l.boundaries, txBoundaryRecord{at: seenAt(rd, it), boundary: Boundary{
		Capture: l.key.Capture, Seq: rec.Seq, Kind: kind, TS: rec.TSUTCNs, Epoch: rec.Epoch, GapStart: ev.GapStart, GapEnd: ev.GapEnd,
	}})

	return nil
}

// endGroup ends the read rd's open group:
// its versions are kept, in rd, their charge with them, when one of them plays a role;
// otherwise they are dropped, and their charge released.
func (l *txLookup) endGroup(rd *txScopeRead) {
	g := &rd.group
	if g.keep {
		rd.kept = append(rd.kept, g.versions...)
	} else {
		for i := range g.versions {
			l.state.release(versionCost(&g.versions[i].Record, g.versions[i].HeaderExtra))
		}
	}
	clear(g.versions)
	*g = txGroup{versions: g.versions[:0]}
}

// commitRead adds to the result the versions the read rd kept and its conflict gaps, in the order found;
// their charges, made when they were found, move with them.
//
// Returns:
//   - error: ctx's error, as is, the versions added and the gaps added until then.
func (l *txLookup) commitRead(ctx context.Context, rd *txScopeRead) error {
	l.res.Records = append(l.res.Records, rd.kept...)
	rd.kept = nil
	for i := range rd.gaps {
		if err := l.tick(ctx); err != nil {
			rd.gaps = rd.gaps[i:]

			return err
		}
		pg := &rd.gaps[i]
		if pg.cross != nil {
			pg.gap.Hours = pg.cross.hours
			pg.cross.gap = len(l.res.Gaps)
		}
		l.res.Gaps = append(l.res.Gaps, pg.gap)
	}
	rd.gaps = nil

	return nil
}

// markConflicts marks as conflicting every kept version of a seq in conflict across scope reads or within one,
// so that no version of a seq in conflict counts as one without a conflict (the tracepack semantics specification §7.2),
// a copy that arrived uncompared included.
//
// Returns:
//   - error: ctx's error, as is, the versions before it marked.
func (l *txLookup) markConflicts(ctx context.Context) error {
	if len(l.cross) == 0 && len(l.conflicted) == 0 {
		return nil
	}
	for i := range l.res.Records {
		if err := l.tick(ctx); err != nil {
			return err
		}
		seq := l.res.Records[i].Record.Seq
		_, cross := l.cross[seq]
		_, local := l.conflicted[seq]
		if cross || local {
			l.res.Records[i].Conflict = true
		}
	}

	return nil
}
