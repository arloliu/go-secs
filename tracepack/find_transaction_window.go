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
func (s *txRuns) insert(seq uint64) {
	n := len(s.runs)
	if n > 0 && seq > s.runs[n-1].last {
		if seq-s.runs[n-1].last == 1 {
			s.runs[n-1].last = seq
		} else {
			s.runs = append(s.runs, txRun{first: seq, last: seq})
		}

		return
	}
	if n == 0 {
		s.runs = append(s.runs, txRun{first: seq, last: seq})
		return
	}

	// Out of order: seq lies at or before the last run.
	i, _ := slices.BinarySearchFunc(s.runs, seq, func(r txRun, seq uint64) int { return cmp.Compare(r.last, seq) })
	if s.runs[i].first <= seq {
		return
	}
	// runs[i-1].last < seq < runs[i].first.
	prev := i > 0 && s.runs[i-1].last+1 == seq
	next := s.runs[i].first-1 == seq
	switch {
	case prev && next:
		s.runs[i-1].last = s.runs[i].last
		s.runs = slices.Delete(s.runs, i, i+1)
	case prev:
		s.runs[i-1].last = seq
	case next:
		s.runs[i].first = seq
	default:
		s.runs = slices.Insert(s.runs, i, txRun{first: seq, last: seq})
	}
}

// contains reports whether seq lies in a run of the set.
func (s *txRuns) contains(seq uint64) bool {
	i, _ := slices.BinarySearchFunc(s.runs, seq, func(r txRun, seq uint64) int { return cmp.Compare(r.last, seq) })

	return i < len(s.runs) && s.runs[i].first <= seq
}

// searchWindow reads the scopes after key.Hour, each once, in order, and evaluates the window of a keyed primary.
// Each read's kept versions and conflict gaps are added to the result after its other gaps, also when the read fails,
// and every kept version of a seq in conflict across reads is then marked as a conflict.
//
// Returns:
//   - error: the error of a read, wrapped with its hour; the error of the evaluation.
func (l *txLookup) searchWindow(ctx context.Context) error {
	for i := 1; i < l.opts.MaxScopes; i++ {
		rd, err := l.readScope(ctx, l.key.Hour+int64(i), l.collect)
		if rd != nil {
			l.commitRead(rd)
		}
		if err != nil {
			l.markCrossConflicts()

			return err
		}
	}
	l.markCrossConflicts()

	return l.evaluate(ctx)
}

// evaluate decides the outcome once every scope is read.
// It is not in place yet: it returns errTxNotImplemented, the result holding what the reads gave.
func (l *txLookup) evaluate(context.Context) error {
	return errTxNotImplemented
}

// collect takes an item of the read rd.
//
// Every item whose record carries ordering-uncertain notes its epoch, whatever its seq.
// An item at or above the primary's seq enters the read's run set and its seq group (beginGroup).
// In scope key.Hour, a version of the primary is cloned and kept, the first one taken as the key to classify against,
// and an item above the primary that arrives before any version of it is counted, not classified.
// Every other item at or above the primary's seq is classified and buffered in its group (addVersion).
//
// Returns:
//   - error: the error of a conflict reservation, which ends the read.
func (l *txLookup) collect(rd *txScopeRead, it *Item) error {
	rec := &it.Record
	if rec.Quality&QualityOrderingUncertain != 0 {
		l.noteOrderingUncertain(rd, it)
	}
	if rec.Seq < l.key.Seq {
		return nil
	}
	if !rd.group.open || rd.group.seq != rec.Seq {
		l.endGroup(rd)
		if err := l.beginGroup(rd, it); err != nil {
			return err
		}
	}

	first := rd.hour == l.key.Hour
	switch {
	case first && rec.Seq == l.key.Seq:
		l.primary = append(l.primary, keepVersion(rd, it, TxPrimary))
		if len(l.primary) == 1 {
			l.prim = primaryKeyOf(&l.primary[0].Record)
		}
	case first && len(l.primary) == 0:
		l.early++
	default:
		l.addVersion(rd, it)
	}

	return nil
}

// noteOrderingUncertain records the epoch of it's record, which carries ordering-uncertain, with where it was first seen.
func (l *txLookup) noteOrderingUncertain(rd *txScopeRead, it *Item) {
	epoch := it.Record.Epoch
	if _, ok := l.uncertain[epoch]; ok {
		return
	}
	if l.uncertain == nil {
		l.uncertain = make(map[uint32]txSeen)
	}
	l.uncertain[epoch] = seenAt(rd, it)
}

// seenAt returns where the read rd yielded it.
func seenAt(rd *txScopeRead, it *Item) txSeen {
	return txSeen{hour: rd.hour, seq: it.Record.Seq, pack: rd.packs[it.Pack], block: it.Block}
}

// beginGroup starts the seq group of it, the first version of its seq in this stretch of the read rd:
// the seq enters the read's run set;
// a version marked as a conflict within the read adds a TxGapConflict gap, except at the primary in scope key.Hour,
// whose settlement reports it;
// and, in a later scope, a seq that an earlier read yielded is a conflict across scope reads (crossConflict).
//
// Returns:
//   - error: the error of the conflict reservation.
func (l *txLookup) beginGroup(rd *txScopeRead, it *Item) error {
	seq := it.Record.Seq
	rd.group.open, rd.group.seq = true, seq
	rd.runs.insert(seq)

	first := rd.hour == l.key.Hour
	if it.Conflict && (!first || seq != l.key.Seq) {
		rd.gaps = append(rd.gaps, txPendingGap{gap: TxGap{
			Reason: TxGapConflict, Hours: []int64{rd.hour}, Block: -1, Offset: -1, Seq: new(seq),
			Err: fmt.Errorf("the read of hour %d yields several versions of seq %d", rd.hour, seq),
		}})
	}
	if first {
		return nil
	}

	return l.crossConflict(rd, seq)
}

// crossConflict checks seq, which the read rd of a later scope yields, against the run sets of the earlier reads.
// A seq already in conflict across reads adds rd's hour to it.
// A seq an earlier read yielded becomes a conflict across reads:
// it reserves one conflict of the lookup and adds a TxGapConflict gap that lists the hours;
// at the primary's seq it also adds a TxGapNoKey gap, since the primary is no longer established.
//
// Returns:
//   - error: the error of the conflict reservation, wrapped with the seq and hours.
func (l *txLookup) crossConflict(rd *txScopeRead, seq uint64) error {
	if c, ok := l.cross[seq]; ok {
		if c.hours[len(c.hours)-1] != rd.hour {
			c.hours = append(c.hours, rd.hour)
			if c.gap >= 0 {
				l.res.Gaps[c.gap].Hours = c.hours
			}
		}

		return nil
	}

	earlier := l.runs[:len(l.runs)-1]
	at := slices.IndexFunc(earlier, func(r *txRuns) bool { return r.contains(seq) })
	if at < 0 {
		return nil
	}
	hour := earlier[at].hour
	if err := l.reserveConflict(); err != nil {
		return fmt.Errorf("the conflict at seq %d between hours %d and %d: %w", seq, hour, rd.hour, err)
	}

	c := &txCrossConflict{hours: []int64{hour, rd.hour}, gap: -1}
	if l.cross == nil {
		l.cross = make(map[uint64]*txCrossConflict)
	}
	l.cross[seq] = c
	rd.gaps = append(rd.gaps, txPendingGap{gap: TxGap{
		Reason: TxGapConflict, Block: -1, Offset: -1, Seq: new(seq),
		Err: fmt.Errorf("the reads of several hours yield seq %d", seq),
	}, cross: c})
	if seq == l.key.Seq {
		rd.gaps = append(rd.gaps, txPendingGap{gap: TxGap{
			Reason: TxGapNoKey, Hours: []int64{l.key.Hour}, Block: -1, Offset: -1, Seq: new(seq),
			Err: fmt.Errorf("the primary conflicts with a version in hour %d", rd.hour),
		}})
	}

	return nil
}

// addVersion classifies the version it of the read rd's open group and buffers it:
// a later scope's version of the primary is TxPrimary, any other version takes the roles classify gives,
// and a capture-boundary record of the primary's epoch is noted.
// A version that plays no role, and that the read compared with no other copy (Item.Conflict false),
// is the only version of its seq in the read, unless a block disagrees with its F-2 entry,
// so it is dropped without a copy, unless the group already buffers a version.
func (l *txLookup) addVersion(rd *txScopeRead, it *Item) {
	if !l.prim.keyed {
		// The first version of the primary has no key: the lookup ends after this read, with nothing to classify.
		return
	}

	rec := &it.Record
	g := &rd.group
	class := TxPrimary
	if rec.Seq != l.key.Seq {
		var ev *TransportEvent
		class, ev = l.prim.classify(rec)
		if ev != nil && ev.Event == EventCaptureBoundary && rec.Epoch == l.prim.epoch {
			l.noteBoundary(rd, it, ev)
		}
	}
	if class == 0 && !it.Conflict && len(g.versions) == 0 {
		return
	}
	g.keep = g.keep || class != 0
	g.versions = append(g.versions, keepVersion(rd, it, class))
}

// noteBoundary records the capture-boundary record of it, whose payload decodes to ev.
func (l *txLookup) noteBoundary(rd *txScopeRead, it *Item, ev *TransportEvent) {
	kind := BoundaryKindUnknown
	if ev.BoundaryKind != nil {
		kind = *ev.BoundaryKind
	}
	rec := &it.Record
	l.boundaries = append(l.boundaries, txBoundaryRecord{at: seenAt(rd, it), boundary: Boundary{
		Capture: l.key.Capture, Seq: rec.Seq, Kind: kind, TS: rec.TSUTCNs, Epoch: rec.Epoch, GapStart: ev.GapStart, GapEnd: ev.GapEnd,
	}})
}

// endGroup ends the read rd's open group:
// its versions are kept, in rd, when one of them plays a role, and dropped otherwise.
func (l *txLookup) endGroup(rd *txScopeRead) {
	g := &rd.group
	if g.keep {
		rd.kept = append(rd.kept, g.versions...)
	}
	clear(g.versions)
	*g = txGroup{versions: g.versions[:0]}
}

// commitRead adds to the result the versions the read rd kept and its conflict gaps, in the order found.
func (l *txLookup) commitRead(rd *txScopeRead) {
	l.res.Records = append(l.res.Records, rd.kept...)
	for _, pg := range rd.gaps {
		if pg.cross != nil {
			pg.gap.Hours = pg.cross.hours
			pg.cross.gap = len(l.res.Gaps)
		}
		l.res.Gaps = append(l.res.Gaps, pg.gap)
	}
	rd.kept, rd.gaps = nil, nil
}

// markCrossConflicts marks as conflicting every kept version of a seq in conflict across scope reads.
func (l *txLookup) markCrossConflicts() {
	if len(l.cross) == 0 {
		return
	}
	for i := range l.res.Records {
		if _, ok := l.cross[l.res.Records[i].Record.Seq]; ok {
			l.res.Records[i].Conflict = true
		}
	}
}
