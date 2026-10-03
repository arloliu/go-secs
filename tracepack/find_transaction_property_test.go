package tracepack

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// Generated lookups (genTxCapture) are checked twice, each time by code written without the lookup's own.
//
// The observation-bounded check (txSnapshot.expect) takes the observation a lookup takes,
// reads every block of every pack of each scope the lookup reads with the package's block reader,
// as the reference of a read over several packs does (newMergeIterateReference),
// groups the copies of each seq by their bytes within each scope and by seq across the scopes,
// and computes the whole TxResult from the definitions of the tracepack semantics specification §7.2.
//
// The safety check (txTruth.violations) takes the capture as it was written,
// before its packs were cut, damaged or dropped,
// and requires every TxUnmatched, and every TxMatched without a gap, to hold over it.

// txGenFacts counts what generated captures hold, for the check that together they exercise what they should.
type txGenFacts struct {
	epochs, backwardSteps, dropped, unavailable, unknownDirs, orderingUncertain, correlation int
	primaries, replies, aborts, rejects, t3s, closes, refusedSockets, boundaries, stops      int
	localVersions, crossVersions, copies, walked, unevaluated, coverage, invalidFooters      int
	flipped, staged, cold, falseClosures, falseStops, partial, captureBarriers, toolBarriers int
}

// txGenPack is one pack of a generated capture, as the generator wrote it.
type txGenPack struct {
	hour int64
	id   UUID
	// recs holds the versions written into the pack, in ascending seq.
	recs []Record
	// admitted reports a pack the catalog admitted; a pack not admitted is staged, and only a listing view reads it.
	admitted bool
	// evaluated is the pack metadata's quality_evaluated, and coverage its coverage entries.
	evaluated bool
	coverage  []Coverage
	// walked reports a pack left unfinalized, and damaged a pack with a block whose body byte was flipped:
	// either gives every read of it a defect.
	walked, damaged bool
	// file is the pack as put in the source.
	file []byte
}

// txGen is a generated capture of captureLow in a memSource, with its ground truth and the lookups to run.
type txGen struct {
	src *memSource
	// truth holds every record of the capture as written, one per seq, the seqs dense,
	// before the packs were cut, damaged or dropped.
	truth []Record
	// packs holds every pack put in the source, admitted or staged.
	packs []txGenPack
	// first is the capture's first hour, and hours the number of hours its records lie in.
	first int64
	hours int
	// indexed holds whether the catalog indexes each hour from first on, the hours after the capture's included.
	indexed map[int64]bool
	// keys holds the lookups to run, maxScopes their MaxScopes.
	keys      []TxKey
	maxScopes int
	// rough reports a capture whose packs, scopes and evidence are now and then damaged, cold or partial (txGen.now);
	// the others hold none of that, so that their lookups can establish absence.
	rough bool
	made  txGenFacts
}

// txGenOpen is a transaction a generated capture opened: its primary's key and match fields.
type txGenOpen struct {
	epoch    uint32
	dir      Dir
	sid      uint16
	sb       [4]byte
	stream   uint8
	function uint8
}

// txGenState is the state of a generated capture's traffic while its records are drawn.
type txGenState struct {
	rng   *rand.Rand
	made  *txGenFacts
	hours int
	// rough is the capture's txGen.rough: only a rough capture's traffic holds capture-boundary records other than a stop.
	rough bool
	// epoch is the epoch of the connection open, next the epoch the next connection takes.
	epoch, next uint32
	// open holds the transactions opened and not answered yet, oldest first.
	open []txGenOpen
	// stopped reports that the last record drawn is a clean stop, which ends the capture.
	stopped bool
}

// txGenHours is the number of hours after a generated capture's last hour whose scopes a lookup can read.
const txGenHours = 3

// genTxCapture returns the capture generated from seed (genTxTraffic), cut into packs (txGen.cut),
// with per-capture evidence and barriers beside it (txGen.evidence) and the lookups to run:
// one of every record at the hour of its ts_utc_ns, and one of each version written into another hour.
// Every hour from the first to txGenHours past the last is indexed, except now and then one in a rough capture.
func genTxCapture(t testing.TB, seed uint64) *txGen {
	t.Helper()

	rng := rand.New(rand.NewPCG(seed, 0x74782d6c6f6f6b))
	g := &txGen{
		src: newMemSource(), first: memTestHour, hours: 3 + rng.IntN(2), maxScopes: 1 + rng.IntN(3), indexed: map[int64]bool{},
		rough: rng.IntN(2) == 0,
	}
	st := &txGenState{rng: rng, made: &g.made, hours: g.hours, rough: g.rough, epoch: 1}
	if rng.IntN(10) == 0 {
		st.epoch = 0
	}
	st.next = st.epoch + 1
	g.truth = genTxTraffic(t, st, g.hours)
	g.made.epochs += int(st.next - st.epoch)

	for _, r := range g.truth {
		g.keys = append(g.keys, TxKey{Capture: captureLow, Seq: r.Seq, Hour: hourOf(r.TSUTCNs)})
	}
	g.cut(t, rng)
	g.evidence(rng)
	for h := g.first; h < g.first+int64(g.hours+txGenHours); h++ {
		g.indexed[h] = !g.now(rng, 6)
		g.src.setIndexed(captureLow, h, g.indexed[h])
		if !g.indexed[h] {
			g.made.cold++
		}
	}

	return g
}

// genTxTraffic returns the records of a generated capture over hours hours, drawn by st (txGenState.record):
// 24 to 63 records at dense seqs from a random start, at rising times that move to the next hour now and then,
// one now and then stepped back into the hour before, as a backward clock step leaves it;
// a clean stop ends them.
func genTxTraffic(t testing.TB, st *txGenState, hours int) []Record {
	t.Helper()

	rng := st.rng
	n := 24 + rng.IntN(40)
	seq := 1 + rng.Uint64N(1000)
	hour, at := 0, int64(0)
	out := make([]Record, 0, n)
	for range n {
		if hour < hours-1 && rng.IntN(9) == 0 {
			hour, at = hour+1, 0
		}
		at += 1 + rng.Int64N(1000)
		h := hour
		if h > 0 && rng.IntN(15) == 0 {
			h--
			st.made.backwardSteps++
		}
		out = append(out, st.record(t, seq, blockTestHour+int64(h)*hourNs+at))
		seq++
		if st.stopped {
			break
		}
	}

	return out
}

// record returns the record of seq at ts, drawn from st:
// a primary, a reply to an open transaction (now and then an abort, another stream or another function),
// a data record of no transaction, a control record (mostly a Reject.req of an open transaction),
// a T3 timer-expiry naming an open transaction (now and then another one),
// an annotation, a socket-close of the open connection, a capture-boundary record,
// a socket-close of a refused connection,
// or a data record in unknown direction;
// each then marked now and then (txGenState.mark).
// A socket-close moves the traffic to the next epoch, and a stop ends it.
func (st *txGenState) record(t testing.TB, seq uint64, ts int64) Record {
	t.Helper()

	rng := st.rng
	var r Record
	switch roll := rng.IntN(100); {
	case roll < 30:
		r = st.primary(seq, ts)
	case roll < 55 && len(st.open) > 0:
		r = st.reply(seq, ts)
	case roll < 63:
		r = st.unrelated(seq, ts, nil)
	case roll < 68:
		r = st.control(seq, ts)
	case roll < 72 && len(st.open) > 0:
		r = st.t3(t, seq, ts)
	case roll < 80:
		r = noteRecord(t, st.epoch)
		r.Seq, r.TSUTCNs = seq, ts
	case roll < 85:
		r = testEventRecord(t, seq, ts, st.epoch, &TransportEvent{Event: EventSocketClose})
		st.epoch, st.next = st.next, st.next+1
		st.made.closes++
	case roll < 88 && (st.rough || rng.IntN(3) == 0):
		r = st.boundary(t, seq, ts)
	case roll < 91:
		r = testEventRecord(t, seq, ts, st.next, &TransportEvent{Event: EventSocketClose})
		st.next++
		st.made.refusedSockets++
	default:
		var tx *txGenOpen
		if len(st.open) > 0 {
			tx = &st.open[rng.IntN(len(st.open))]
		}
		r = st.unrelated(seq, ts, tx)
		r.Dir = DirUnknown
		st.made.unknownDirs++
	}
	st.mark(&r)

	return txCanonical(r)
}

// mark makes, now and then, a field of r unavailable when it is a data record (txGenState.unavailable),
// and r ordering-uncertain, more rarely in a capture that is not rough,
// or correlation-incomplete when it is a data record.
func (st *txGenState) mark(r *Record) {
	rng := st.rng
	if r.Kind == KindData && rng.IntN(7) == 0 {
		st.unavailable(r)
	}
	if rng.IntN(40) == 0 && (st.rough || rng.IntN(4) == 0) {
		r.Quality |= QualityOrderingUncertain
		st.made.orderingUncertain++
	}
	if r.Kind == KindData && rng.IntN(30) == 0 {
		r.Quality |= QualityCorrelationIncomplete
		st.made.correlation++
	}
}

// genTxFrame returns an HSMS frame of SessionID sid, stream, the W bit w, function and System Bytes sb,
// followed by up to 8 bytes of message text, none of them zero.
func genTxFrame(rng *rand.Rand, sid uint16, stream uint8, w bool, function uint8, sb [4]byte) []byte {
	p := make([]byte, len(blockTestFrame), len(blockTestFrame)+8)
	binary.BigEndian.PutUint16(p[fieldSessionIDOff:], sid)
	p[fieldByte6Off] = stream
	if w {
		p[fieldByte6Off] |= 0x80
	}
	p[fieldFunctionOff] = function
	copy(p[fieldSystemBytesOff:], sb[:])
	for range rng.IntN(9) {
		p = append(p, byte(1+rng.IntN(255)))
	}
	binary.BigEndian.PutUint32(p, uint32(len(p)-4))

	return p
}

// genTxData returns a data record of seq at ts in epoch and dir over payload, with the field_validity of a raw capture.
func genTxData(seq uint64, ts int64, epoch uint32, dir Dir, payload []byte) Record {
	r := testDataRecord(seq, ts, epoch)
	r.Dir, r.Payload = dir, payload
	r.SetCapturedFieldValidity()

	return r
}

// txGenOpposite returns the direction opposite to dir, host-to-equipment or equipment-to-host.
func txGenOpposite(dir Dir) Dir {
	if dir == DirHostToEquipment {
		return DirEquipmentToHost
	}

	return DirHostToEquipment
}

// primary returns a primary of seq at ts in the open connection, and opens its transaction:
// SessionID 1 or 2, System Bytes from a set of three, stream 1 to 3, function 1, 3 or 5, the W bit mostly set,
// the stream and W bit now and then unavailable.
func (st *txGenState) primary(seq uint64, ts int64) Record {
	rng := st.rng
	tx := txGenOpen{
		epoch: st.epoch, dir: DirHostToEquipment, sid: uint16(1 + rng.IntN(2)),
		sb: [4]byte{0xDE, 0xAD, 0xBE, byte(rng.IntN(3))}, stream: uint8(1 + rng.IntN(3)), function: uint8(1 + 2*rng.IntN(3)),
	}
	if rng.IntN(3) == 0 {
		tx.dir = DirEquipmentToHost
	}
	st.open = append(st.open, tx)
	if len(st.open) > 6 {
		st.open = st.open[1:]
	}
	st.made.primaries++
	r := genTxData(seq, ts, tx.epoch, tx.dir, genTxFrame(rng, tx.sid, tx.stream, rng.IntN(4) != 0, tx.function, tx.sb))
	if rng.IntN(8) == 0 {
		r.FieldValidity &^= FieldValidityStreamAndW
	}

	return r
}

// reply returns a reply of seq at ts to an open transaction, which it closes half the time:
// the function + 1, or now and then function 0, another stream or another function; mostly in the transaction's epoch.
func (st *txGenState) reply(seq uint64, ts int64) Record {
	rng := st.rng
	k := rng.IntN(len(st.open))
	tx := st.open[k]
	stream, function := tx.stream, tx.function+1
	switch rng.IntN(10) {
	case 0:
		function = 0
		st.made.aborts++
	case 1:
		stream = tx.stream%3 + 1
	case 2:
		function = tx.function + 3
	default:
	}
	epoch := tx.epoch
	if rng.IntN(10) == 0 {
		epoch = st.epoch
	}
	if rng.IntN(2) == 0 {
		st.open = slices.Delete(st.open, k, k+1)
	}
	st.made.replies++

	return genTxData(seq, ts, epoch, txGenOpposite(tx.dir), genTxFrame(rng, tx.sid, stream, false, function, tx.sb))
}

// unrelated returns a data record of seq at ts with a random direction, stream and function 0 to 6,
// the SessionID and System Bytes, and epoch, of tx when set, random ones of the open connection otherwise.
func (st *txGenState) unrelated(seq uint64, ts int64, tx *txGenOpen) Record {
	rng := st.rng
	sid, sb, epoch := uint16(1+rng.IntN(2)), [4]byte{0xDE, 0xAD, 0xBE, byte(rng.IntN(4))}, st.epoch
	if tx != nil {
		sid, sb, epoch = tx.sid, tx.sb, tx.epoch
	}
	dir := DirHostToEquipment
	if rng.IntN(2) == 0 {
		dir = DirEquipmentToHost
	}

	return genTxData(seq, ts, epoch, dir, genTxFrame(rng, sid, uint8(1+rng.IntN(3)), rng.IntN(2) == 0, uint8(rng.IntN(7)), sb))
}

// control returns a control record of seq at ts, whose bytes 6 and 7 are random:
// mostly the Reject.req of an open transaction, now and then of another SType, its SType now and then unavailable.
func (st *txGenState) control(seq uint64, ts int64) Record {
	rng := st.rng
	dir, sid, sb, epoch := DirEquipmentToHost, uint16(0xFFFF), [4]byte{0, 0, 0, 1}, st.epoch
	if len(st.open) > 0 && rng.IntN(3) != 0 {
		tx := st.open[rng.IntN(len(st.open))]
		dir, sid, sb, epoch = txGenOpposite(tx.dir), tx.sid, tx.sb, tx.epoch
	}
	p := genTxFrame(rng, sid, uint8(rng.IntN(2)), false, uint8(rng.IntN(8)), sb)[:len(blockTestFrame)]
	p[fieldSTypeOff] = sTypeRejectReq
	if rng.IntN(4) == 0 {
		p[fieldSTypeOff] = byte(1 + rng.IntN(9))
	}
	r := Record{
		Seq: seq, TSUTCNs: ts, MonoPresent: true, Epoch: epoch, Kind: KindControl, Dir: dir,
		DecodeStatus: DecodeStatusNotApplicable, Payload: p,
	}
	r.SetCapturedFieldValidity()
	if rng.IntN(6) == 0 {
		r.FieldValidity &^= FieldValiditySType
	}
	if p[fieldSTypeOff] == sTypeRejectReq {
		st.made.rejects++
	}

	return r
}

// t3 returns a T3 timer-expiry event of seq at ts naming an open transaction,
// its stream and function present half the time each, the stream now and then another;
// now and then it names other System Bytes, is a T6 expiry, has no SessionID, or a SessionID above 16 bits.
func (st *txGenState) t3(t testing.TB, seq uint64, ts int64) Record {
	t.Helper()

	rng := st.rng
	tx := st.open[rng.IntN(len(st.open))]
	ev := &TransportEvent{
		Event: EventTimerExpiry, Timer: new(TimerT3),
		PrimarySessionID: new(uint64(tx.sid)), PrimarySystemBytes: tx.sb, PrimarySystemBytesPresent: true,
	}
	if rng.IntN(2) == 0 {
		ev.PrimaryStream = new(uint64(tx.stream) + uint64(rng.IntN(4)/3))
	}
	if rng.IntN(2) == 0 {
		ev.PrimaryFunction = new(uint64(tx.function))
	}
	switch rng.IntN(8) {
	case 0:
		ev.PrimarySystemBytes[3] ^= 1
	case 1:
		ev.Timer = new(TimerT6)
	case 2:
		ev.PrimarySessionID = nil
	case 3:
		ev.PrimarySessionID = new(uint64(tx.sid) + 1<<16)
	default:
	}
	st.made.t3s++

	return testEventRecord(t, seq, ts, tx.epoch, ev)
}

// boundary returns a capture-boundary record of seq at ts in the open connection's epoch, now and then with gap bounds:
// a stop in a capture that is not rough, and half the time in a rough one,
// otherwise of kind start, stop, gap or stop-unclean, or now and then of no kind.
// A stop ends the capture; a third of the stops are of the next epoch, not the open connection's.
func (st *txGenState) boundary(t testing.TB, seq uint64, ts int64) Record {
	t.Helper()

	rng := st.rng
	kinds := [...]BoundaryKind{BoundaryKindStart, BoundaryKindStop, BoundaryKindGap, BoundaryKindStopUnclean}
	ev := &TransportEvent{Event: EventCaptureBoundary}
	switch {
	case !st.rough, rng.IntN(2) == 0:
		ev.BoundaryKind = new(BoundaryKindStop)
	case rng.IntN(8) != 0:
		ev.BoundaryKind = new(kinds[rng.IntN(len(kinds))])
	default:
	}
	if rng.IntN(3) == 0 {
		ev.GapStart, ev.GapEnd = txGenGapBound(rng, st.hours), txGenGapBound(rng, st.hours)
	}
	stop := ev.BoundaryKind != nil && *ev.BoundaryKind == BoundaryKindStop
	epoch := st.epoch
	if stop && rng.IntN(3) == 0 {
		epoch = st.next
	}
	r := testEventRecord(t, seq, ts, epoch, ev)
	st.made.boundaries++
	if stop {
		st.stopped = true
		st.made.stops++
	}

	return r
}

// unavailable makes a field of the data record r unavailable:
// its SessionID, stream, function or System Bytes bit cleared, its payload cut short, or its direction unknown.
func (st *txGenState) unavailable(r *Record) {
	rng := st.rng
	switch rng.IntN(3) {
	case 0:
		bits := [...]FieldValidity{FieldValiditySessionID, FieldValidityStreamAndW, FieldValidityFunction, FieldValiditySystemBytes}
		r.FieldValidity &^= bits[rng.IntN(len(bits))]
	case 1:
		r.Payload = r.Payload[:rng.IntN(len(blockTestFrame))]
		r.SetCapturedFieldValidity()
	default:
		r.Dir = DirUnknown
	}
	st.made.unavailable++
}

// txGenGapBound returns a gap bound drawn from rng: absent a fourth of the time,
// otherwise the start, the middle or the last ns of an hour,
// from the one before the capture's first to one past its last.
func txGenGapBound(rng *rand.Rand, hours int) *int64 {
	if rng.IntN(4) == 0 {
		return nil
	}
	at := [...]int64{0, hourNs / 2, hourNs - 1}[rng.IntN(3)]

	return new(blockTestHour + (rng.Int64N(int64(hours)+3)-1)*hourNs + at)
}

// txCanonical returns r as a Writer stores it (canonicalHeader):
// capture-boundary quality set exactly on a capture-boundary record, correlation-incomplete on epoch 0,
// no monotonic time without mono_present, and field_validity only on a record that carries an HSMS frame.
func txCanonical(r Record) Record {
	r.Quality &^= QualityCaptureBoundary
	if ev := transportEventOf(&r); ev != nil && ev.Event == EventCaptureBoundary {
		r.Quality |= QualityCaptureBoundary
	}
	if r.Epoch == 0 {
		r.Quality |= QualityCorrelationIncomplete
	}
	if !r.MonoPresent {
		r.MonoNs = 0
	}
	if r.Kind != KindData && r.Kind != KindControl {
		r.FieldValidity = 0
	}

	return r
}

// cut drops a record of the capture now and then from every pack,
// then writes each hour's records into packs (txGen.pack):
// one pack, or the segments of two or three consumers taking turns of one or two records;
// now and then an identical copy of a window of them,
// and a window with one or two records changed into other versions, half the time from the hour's first closing record;
// and, in half the captures, one or two records given another version in the hour before or after, alone in a pack,
// each looked up at that hour too.
func (g *txGen) cut(t testing.TB, rng *rand.Rand) {
	t.Helper()

	byHour := make([][]Record, g.hours)
	for _, r := range g.truth {
		if rng.IntN(30) == 0 {
			g.made.dropped++
			continue
		}
		h := hourOf(r.TSUTCNs) - g.first
		byHour[h] = append(byHour[h], r)
	}
	for h, recs := range byHour {
		if len(recs) == 0 {
			continue
		}
		hour := g.first + int64(h)
		for _, part := range txGenParts(rng, recs) {
			g.pack(t, rng, hour, part)
		}
		if rng.IntN(3) == 0 {
			s := rng.IntN(len(recs))
			g.pack(t, rng, hour, slices.Clone(recs[s:s+1+rng.IntN(len(recs)-s)]))
			g.made.copies++
		}
		if rng.IntN(3) == 0 && (g.rough || rng.IntN(3) == 0) {
			s := rng.IntN(len(recs))
			closer := slices.IndexFunc(recs, func(r Record) bool { return txGenIsSocketClose(&r) || txGenIsStop(&r) })
			if closer >= 0 && rng.IntN(2) == 0 {
				s = closer
			}
			w := slices.Clone(recs[s : s+1+rng.IntN(min(4, len(recs)-s))])
			for range 1 + rng.IntN(2) {
				k := rng.IntN(len(w))
				if s == closer && rng.IntN(2) == 0 {
					k = 0
				}
				w[k] = txGenVersion(rng, w[k])
			}
			g.pack(t, rng, hour, w)
			g.made.localVersions++
		}
	}
	if rng.IntN(2) == 0 && (g.rough || rng.IntN(3) == 0) {
		for range 1 + rng.IntN(2) {
			g.crossVersion(t, rng)
		}
	}
}

// txGenParts returns recs whole, half the time,
// or as the segments of two or three consumers taking turns of one or two records.
func txGenParts(rng *rand.Rand, recs []Record) [][]Record {
	if rng.IntN(2) == 0 {
		return [][]Record{recs}
	}
	consumers, turn := 2+rng.IntN(2), 1+rng.IntN(2)
	parts := make([][]Record, consumers)
	for i := range recs {
		parts[i/turn%consumers] = append(parts[i/turn%consumers], recs[i])
	}

	return parts
}

// txGenVersion returns another version of r in its hour, drawn from rng:
// its last payload byte changed, its direction changed, its ordering-uncertain bit flipped,
// or its time moved by a few ns.
func txGenVersion(rng *rand.Rand, r Record) Record {
	switch rng.IntN(4) {
	case 0:
		if len(r.Payload) > 0 {
			p := slices.Clone(r.Payload)
			p[len(p)-1]++
			r.Payload = p

			break
		}
		r.TSUTCNs++
	case 1:
		r.Dir ^= 1
	case 2:
		r.Quality ^= QualityOrderingUncertain
	default:
		r.TSUTCNs += 1 + rng.Int64N(5)
	}

	return txCanonical(r)
}

// crossVersion writes another version of a record into the hour before or after its own, alone in a pack,
// half the time of a closing record, a socket-close or a stop, when the capture has one, of a random record otherwise:
// the record moved in time, half the time changed further (txGenVersion); a lookup of it at that hour is added.
func (g *txGen) crossVersion(t testing.TB, rng *rand.Rand) {
	t.Helper()

	r := g.truth[rng.IntN(len(g.truth))]
	if closers := txGenClosers(g.truth); len(closers) > 0 && rng.IntN(2) == 0 {
		r = *closers[rng.IntN(len(closers))]
	}
	h := hourOf(r.TSUTCNs) - g.first
	switch {
	case h == 0:
		h++
	case h == int64(g.hours)-1, rng.IntN(2) == 0:
		h--
	default:
		h++
	}
	r.TSUTCNs = blockTestHour + h*hourNs + 1 + rng.Int64N(1000)
	if rng.IntN(2) == 0 {
		r = txGenVersion(rng, r)
	}
	g.pack(t, rng, g.first+h, []Record{r})
	g.keys = append(g.keys, TxKey{Capture: captureLow, Seq: r.Seq, Hour: g.first + h})
	g.made.crossVersions++
}

// pack writes recs, unless empty, into a pack of captureLow for hour, its period that hour,
// in blocks closed at random, with a random codec,
// and puts it in the source: admitted, which registers its footer's evidence,
// or staged, a third of those with its evidence registered as a rejected registration does.
// In a rough capture, now and then the pack is staged,
// its pack metadata has quality_evaluated false or a coverage entry,
// or it is left unfinalized, written with a footer that is not valid, or given a block with a body byte flipped.
func (g *txGen) pack(t testing.TB, rng *rand.Rand, hour int64, recs []Record) {
	t.Helper()

	if len(recs) == 0 {
		return
	}
	p := txGenPack{
		hour: hour, id: UUID{0x70, byte(len(g.packs) >> 8), byte(len(g.packs))}, recs: recs,
		admitted: !g.now(rng, 10), evaluated: !g.now(rng, 6), walked: g.now(rng, 8),
	}
	if g.now(rng, 5) {
		p.coverage = []Coverage{g.coverageEntry(rng)}
		g.made.coverage++
	}
	prob := 0.1 + 0.5*rng.Float64()
	steps := make([]footerTestStep, len(recs))
	for i := range recs {
		steps[i] = footerTestStep{rec: recs[i], flush: rng.Float64() < prob}
	}
	off := (hour - memTestHour) * hourNs
	file := writeRepairPack(t, [...]Codec{CodecNone, CodecZstd}[rng.IntN(2)], func(m *PackMeta) {
		m.PeriodStart += off
		m.PeriodEnd += off
		m.QualityEvaluated = p.evaluated
		m.Coverage = slices.Clone(p.coverage)
	}, p.walked, steps).file
	if !p.walked && g.now(rng, 10) {
		file = invalidFooterFile(t, file)
		g.made.invalidFooters++
	}
	file = withIDs(t, file, p.id, captureLow)
	if g.now(rng, 8) {
		blocks := mustOpen(t, file, ReaderOptions{}).Blocks()
		b := blocks[rng.IntN(len(blocks))]
		file = flipByte(file, b.Offset+format.EnvelopeLen+uint64(rng.IntN(int(b.OnDiskLen)-format.EnvelopeLen)))
		p.damaged = true
		g.made.flipped++
	}
	p.file = file
	switch {
	case p.admitted:
		g.src.addPack(t, hour, file)
	case rng.IntN(3) == 0:
		g.src.stagePack(t, hour, file)
		g.src.registerEvidence(t, file)
	default:
		g.src.stagePack(t, hour, file)
	}
	if !p.admitted {
		g.made.staged++
	}
	if p.walked {
		g.made.walked++
	}
	if !p.evaluated {
		g.made.unevaluated++
	}
	g.packs = append(g.packs, p)
}

// txGenClosers returns the closing records of recs, socket-closes and stops, in their order.
func txGenClosers(recs []Record) []*Record {
	var out []*Record
	for i := range recs {
		if r := &recs[i]; txGenIsSocketClose(r) || txGenIsStop(r) {
			out = append(out, r)
		}
	}

	return out
}

// txGenIsSocketClose reports whether r is a socket-close transport event.
func txGenIsSocketClose(r *Record) bool {
	ev := transportEventOf(r)

	return ev != nil && ev.Event == EventSocketClose
}

// txGenIsStop reports whether r is a capture-boundary record of kind stop.
func txGenIsStop(r *Record) bool {
	ev := transportEventOf(r)

	return ev != nil && ev.Event == EventCaptureBoundary && ev.BoundaryKind != nil && *ev.BoundaryKind == BoundaryKindStop
}

// now reports, for a rough capture, a draw from rng of probability 1/n; it is false for any other capture.
func (g *txGen) now(rng *rand.Rand, n int) bool {
	return g.rough && rng.IntN(n) == 0
}

// coverageEntry returns a coverage entry drawn from rng:
// each seq bound, now and then absent, a seq of the capture, so that the entry is inverted now and then;
// each time bound, now and then absent, a gap bound (txGenGapBound);
// its capture_id mostly absent, now and then captureLow or captureHigh.
func (g *txGen) coverageEntry(rng *rand.Rand) Coverage {
	var c Coverage
	if rng.IntN(4) != 0 {
		c.SeqFirst = new(g.truth[rng.IntN(len(g.truth))].Seq)
	}
	if rng.IntN(4) != 0 {
		c.SeqLast = new(g.truth[rng.IntN(len(g.truth))].Seq)
	}
	c.TimeStart, c.TimeEnd = txGenGapBound(rng, g.hours), txGenGapBound(rng, g.hours)
	switch rng.IntN(8) {
	case 0:
		c.CaptureID = new(captureLow)
	case 1:
		c.CaptureID = new(captureHigh)
	default:
	}

	return c
}

// evidence records, in a rough capture, per-capture evidence beside what the packs' registrations recorded,
// each now and then:
// a close_seq and a clean stop, each half the time,
// at a closing record of the capture, mostly its last, or at a random record,
// the close_seq of the record's epoch or, half the time, of another record's, and the stop likewise;
// partial evidence; a stop-unclean of the capture, three times in four;
// a stop-unclean of captureHigh, a barrier of the tool;
// the stop-uncleans with random gap bounds.
func (g *txGen) evidence(rng *rand.Rand) {
	pick := func() *Record { return &g.truth[rng.IntN(len(g.truth))] }
	closers := txGenClosers(g.truth)
	at := func() *Record {
		switch {
		case len(closers) == 0 || rng.IntN(4) == 0:
			return pick()
		case rng.IntN(2) == 0:
			return closers[len(closers)-1]
		default:
			return closers[rng.IntN(len(closers))]
		}
	}
	epochOf := func(r *Record) uint32 {
		if rng.IntN(2) == 0 {
			return r.Epoch
		}
		for range 8 {
			if o := pick(); o.Epoch != r.Epoch {
				return o.Epoch
			}
		}

		return r.Epoch + 1
	}
	if g.now(rng, 2) {
		r := at()
		g.src.addClosure(captureLow, epochOf(r), r.Seq)
		g.made.falseClosures++
	}
	if g.now(rng, 2) {
		r := at()
		g.src.addBoundary(Boundary{Capture: captureLow, Seq: r.Seq, Kind: BoundaryKindStop, TS: r.TSUTCNs, Epoch: epochOf(r)})
		g.made.falseStops++
	}
	if g.now(rng, 5) {
		g.src.setPartial(captureLow)
		g.made.partial++
	}
	if g.rough && rng.IntN(4) != 0 {
		r := pick()
		g.src.addBoundary(Boundary{
			Capture: captureLow, Seq: r.Seq, Kind: BoundaryKindStopUnclean, TS: r.TSUTCNs, Epoch: r.Epoch,
			GapStart: txGenGapBound(rng, g.hours), GapEnd: txGenGapBound(rng, g.hours),
		})
		g.made.captureBarriers++
	}
	if g.now(rng, 3) {
		g.src.addBoundary(Boundary{
			Capture: captureHigh, Seq: 1 + rng.Uint64N(100), Kind: BoundaryKindStopUnclean, TS: blockTestHour, Epoch: 1,
			GapStart: txGenGapBound(rng, g.hours), GapEnd: txGenGapBound(rng, g.hours),
		})
		g.made.toolBarriers++
	}
}

// txOracleScope is one scope as the observation-bounded check reads it.
type txOracleScope struct {
	hour    int64
	indexed bool
	// packs holds the pack_ids of the view, in view order; nil for a scope without packs.
	packs []UUID
	// items holds what a read of the scope in capture order with a zero filter and payloads yields,
	// and conflicts the conflicts it lists, by the reference of a read over several packs.
	items     []iterItem
	conflicts []Conflict
	// defects holds the read's defects other than coverage entries, with their packs, as TxGapRead gaps.
	defects []TxGap
	// coverage holds the coverage entries of the packs, those of another capture left out, each with its pack.
	coverage []txOracleCoverage
	// unevaluated holds the packs whose quality_evaluated is false, footerErrs the footer errors.
	unevaluated []UUID
	footerErrs  []TxPackError
}

// txOracleCoverage is a coverage entry of a pack a scope's view holds.
type txOracleCoverage struct {
	pack UUID
	c    Coverage
}

// txSnapshot is the observation a lookup takes, as the observation-bounded check reads it:
// each scope of its hours, the capture's per-capture evidence, and every stop-unclean of the tool.
type txSnapshot struct {
	capture  UUID
	scopes   map[int64]*txOracleScope
	evidence CaptureEvidence
	barriers []Boundary
}

// newTxSnapshot takes one observation of capture over the hours [from, to) of src,
// and reads each of its scopes with the reference of a read over several packs (newTxOracleScope).
// The barriers are every stop-unclean boundary the observation fixed, unfiltered.
func newTxSnapshot(t testing.TB, src *memSource, capture UUID, from, to int64) *txSnapshot {
	t.Helper()

	obs, err := src.Observe(t.Context(), capture, from, to)
	require.NoError(t, err)
	defer func() { require.NoError(t, obs.Close()) }()

	s := &txSnapshot{capture: capture, scopes: map[int64]*txOracleScope{}}
	s.evidence, err = obs.Evidence(t.Context())
	require.NoError(t, err)
	mo, ok := obs.(*memObservation)
	require.True(t, ok)
	s.barriers = cloneBoundaries(mo.barriers)
	for h := from; h < to; h++ {
		sc, err := obs.Scope(t.Context(), h)
		require.NoError(t, err)
		s.scopes[h] = newTxOracleScope(t, h, sc)
	}

	return s
}

// newTxOracleScope reads the scope of hour, whose fixed view is sc,
// with the reference of a read over several packs in capture order, a zero filter and payloads,
// and takes from each pack what Open found:
// its defects, its coverage entries, its quality_evaluated and its footer error.
func newTxOracleScope(t testing.TB, hour int64, sc SourceScope) *txOracleScope {
	t.Helper()

	o := &txOracleScope{hour: hour, indexed: sc.Indexed}
	for _, r := range sc.Readers {
		o.packs = append(o.packs, r.Header().PackID)
	}
	ref := newMergeIterateReference(t, sc.Readers, Query{Payloads: true})
	o.items, o.conflicts = ref.read(OrderCapture)

	defect := func(pack UUID, block int, offset int64, reason IncompleteReason) TxGap {
		return TxGap{Reason: TxGapRead, Hours: []int64{hour}, Pack: new(pack), Block: block, Offset: offset, Defect: reason}
	}
	for i, r := range sc.Readers {
		h := r.Header()
		for _, d := range r.openDefects {
			o.defects = append(o.defects, defect(o.packs[i], d.Block, d.Offset, d.Reason))
		}
		for _, c := range h.Meta.Coverage {
			if c.CaptureID == nil || *c.CaptureID == h.CaptureID {
				o.coverage = append(o.coverage, txOracleCoverage{pack: o.packs[i], c: c})
			}
		}
		if !h.Meta.QualityEvaluated {
			o.unevaluated = append(o.unevaluated, o.packs[i])
		}
		if h.FooterErr != nil {
			o.footerErrs = append(o.footerErrs, TxPackError{Hour: hour, Pack: o.packs[i], Err: h.FooterErr})
		}
	}
	for _, d := range slices.Concat(ref.preReadDefects(), ref.resolutionDefects(OrderCapture)) {
		o.defects = append(o.defects, defect(o.packs[d.pack], d.block, d.offset, d.reason))
	}

	return o
}

// txOracleField reports whether the HSMS header field of rec whose bit is bit and whose bytes end at end is available:
// rec carries an HSMS frame, its field_validity has the bit, and its payload holds the bytes.
func txOracleField(rec *Record, bit FieldValidity, end int) bool {
	return (rec.Kind == KindData || rec.Kind == KindControl) && rec.FieldValidity&bit != 0 && len(rec.Payload) >= end
}

// txOracleKey is a primary as the observation-bounded check reads it: its association key and match fields.
type txOracleKey struct {
	keyed bool
	epoch uint32
	dir   Dir
	sid   uint16
	sb    [4]byte
	// stream is valid iff streamOK; function is valid on a primary.
	stream   uint8
	streamOK bool
	function uint8
}

// txOracleIsPrimary reports whether rec is a primary: a data record with an available odd function.
func txOracleIsPrimary(rec *Record) bool {
	return rec.Kind == KindData && txOracleField(rec, FieldValidityFunction, fieldFunctionEnd) && rec.Payload[fieldFunctionOff]%2 == 1
}

// txOracleKeyOf returns the key of the primary rec;
// keyed when its direction is host-to-equipment or equipment-to-host and its SessionID and System Bytes are available.
func txOracleKeyOf(rec *Record) txOracleKey {
	k := txOracleKey{epoch: rec.Epoch, dir: rec.Dir, function: rec.Payload[fieldFunctionOff]}
	sidOK := txOracleField(rec, FieldValiditySessionID, fieldSessionIDEnd)
	sbOK := txOracleField(rec, FieldValiditySystemBytes, fieldSystemBytesEnd)
	k.keyed = (rec.Dir == DirHostToEquipment || rec.Dir == DirEquipmentToHost) && sidOK && sbOK
	if sidOK {
		k.sid = binary.BigEndian.Uint16(rec.Payload[fieldSessionIDOff:])
	}
	if sbOK {
		k.sb = [4]byte(rec.Payload[fieldSystemBytesOff:fieldSystemBytesEnd])
	}
	if txOracleField(rec, FieldValidityStreamAndW, fieldByte6End) {
		k.stream, k.streamOK = rec.Payload[fieldByte6Off]&0x7F, true
	}

	return k
}

// opposite returns the direction opposite to the key's.
func (k *txOracleKey) opposite() Dir {
	if k.dir == DirHostToEquipment {
		return DirEquipmentToHost
	}

	return DirHostToEquipment
}

// ids compares the SessionID and System Bytes of rec with the key's.
//
// Returns:
//   - equal: both available and equal to the key's.
//   - compatible: each unavailable or equal to the key's.
//   - known: both available.
func (k *txOracleKey) ids(rec *Record) (equal, compatible, known bool) {
	sidOK := txOracleField(rec, FieldValiditySessionID, fieldSessionIDEnd)
	sbOK := txOracleField(rec, FieldValiditySystemBytes, fieldSystemBytesEnd)
	sidEq := sidOK && binary.BigEndian.Uint16(rec.Payload[fieldSessionIDOff:]) == k.sid
	sbEq := sbOK && [4]byte(rec.Payload[fieldSystemBytesOff:fieldSystemBytesEnd]) == k.sb

	return sidEq && sbEq, (!sidOK || sidEq) && (!sbOK || sbEq), sidOK && sbOK
}

// roles returns the roles rec, a record above the primary, plays against the key k,
// by the definitions of the tracepack semantics specification §7.2.
func (k *txOracleKey) roles(rec *Record) TxClass {
	if rec.Kind == KindData {
		return k.dataRoles(rec)
	}
	if rec.Kind == KindTransportEvent {
		return k.eventRoles(rec)
	}
	equal, _, _ := k.ids(rec)
	if rec.Kind == KindControl && rec.Epoch == k.epoch && rec.Dir == k.opposite() && equal &&
		txOracleField(rec, FieldValiditySType, fieldSTypeEnd) && rec.Payload[fieldSTypeOff] == sTypeRejectReq {
		return TxOutcomeRecord
	}

	return 0
}

// dataRoles returns the roles of the data record rec:
// candidate, possible reply, same-key primary, possible same-key primary.
func (k *txOracleKey) dataRoles(rec *Record) TxClass {
	if rec.Epoch != k.epoch {
		return 0
	}
	equal, compatible, known := k.ids(rec)
	unknownDir := rec.Dir == DirUnknown
	var c TxClass
	if rec.Dir == k.opposite() && equal {
		c |= TxCandidate
	} else if (rec.Dir == k.opposite() || unknownDir) && compatible && (unknownDir || !known) {
		c |= TxPossibleReply
	}
	fnOK := txOracleField(rec, FieldValidityFunction, fieldFunctionEnd)
	odd := fnOK && rec.Payload[fieldFunctionOff]%2 == 1
	if rec.Dir == k.dir && odd && equal {
		c |= TxSameKeyPrimary
	} else if (rec.Dir == k.dir || unknownDir) && (odd || !fnOK) && compatible && (unknownDir || !fnOK || !known) {
		c |= TxPossiblePrimary
	}

	return c
}

// eventRoles returns the roles of the transport-event record rec:
// closing for a socket-close of the key's epoch or a capture-boundary stop of any epoch,
// outcome for a T3 timer-expiry of the key's epoch that names the primary.
func (k *txOracleKey) eventRoles(rec *Record) TxClass {
	ev, err := UnmarshalTransportEvent(rec.Payload)
	if err != nil {
		return 0
	}
	switch {
	case ev.Event == EventSocketClose && rec.Epoch == k.epoch:
		return TxClosing
	case ev.Event == EventCaptureBoundary && ev.BoundaryKind != nil && *ev.BoundaryKind == BoundaryKindStop:
		return TxClosing
	case ev.Event == EventTimerExpiry && rec.Epoch == k.epoch && ev.Timer != nil && *ev.Timer == TimerT3 &&
		ev.PrimarySessionID != nil && *ev.PrimarySessionID == uint64(k.sid) &&
		ev.PrimarySystemBytesPresent && ev.PrimarySystemBytes == k.sb &&
		(ev.PrimaryStream == nil || !k.streamOK || *ev.PrimaryStream == uint64(k.stream)) &&
		(ev.PrimaryFunction == nil || *ev.PrimaryFunction == uint64(k.function)):
		return TxOutcomeRecord
	default:
		return 0
	}
}

// txOracleBoundaryOf returns the capture-boundary record rec as a Boundary of capture, and whether it is one.
func txOracleBoundaryOf(capture UUID, rec *Record) (Boundary, bool) {
	if rec.Kind != KindTransportEvent {
		return Boundary{}, false
	}
	ev, err := UnmarshalTransportEvent(rec.Payload)
	if err != nil || ev.Event != EventCaptureBoundary {
		return Boundary{}, false
	}
	b := Boundary{Capture: capture, Seq: rec.Seq, TS: rec.TSUTCNs, Epoch: rec.Epoch, GapStart: ev.GapStart, GapEnd: ev.GapEnd}
	if ev.BoundaryKind != nil {
		b.Kind = *ev.BoundaryKind
	}

	return b, true
}

// txOracleMeets reports whether the interval [lo, hi], a nil bound unbounded, meets [from, to):
// always when lo exceeds hi, as the tracepack format specification §5 says of an inverted entry.
func txOracleMeets(lo, hi *int64, from, to int64) bool {
	if lo != nil && hi != nil && *lo > *hi {
		return true
	}

	return (lo == nil || *lo < to) && (hi == nil || *hi >= from)
}

// txOracleCoverageMeets reports whether the coverage entry c of a pack of capture meets the seqs [first, last]
// and the time range [from, to), by the tracepack format specification §5; an empty seq range (first > last) meets
// only an entry inverted on either side.
func txOracleCoverageMeets(c *Coverage, capture UUID, first, last uint64, from, to int64) bool {
	if c.CaptureID != nil && *c.CaptureID != capture {
		return false
	}
	if c.SeqFirst != nil && c.SeqLast != nil && *c.SeqFirst > *c.SeqLast ||
		c.TimeStart != nil && c.TimeEnd != nil && *c.TimeStart > *c.TimeEnd {
		return true
	}

	return first <= last && (c.SeqFirst == nil || *c.SeqFirst <= last) && (c.SeqLast == nil || *c.SeqLast >= first) &&
		txOracleMeets(c.TimeStart, c.TimeEnd, from, to)
}

// txExpect is the observation-bounded check's computation of one lookup's result.
type txExpect struct {
	snap  *txSnapshot
	key   TxKey
	reads []*txOracleScope
	res   TxResult
	k     txOracleKey
	// hours maps each seq at or above the primary's that a read yielded to the hours of those reads, ascending.
	hours map[uint64][]int64
	// e is the window's end, valid iff bounded; possible the first possible same-key primary inside it, valid iff hasPossible.
	e, possible          uint64
	bounded, hasPossible bool
	valid                int
}

// expect computes from the snapshot alone what a lookup of key with maxScopes scopes returns:
// the result, and whether the lookup fails with ErrNotPrimary,
// the result then holding what the read of scope key.Hour found.
func (s *txSnapshot) expect(key TxKey, maxScopes int) (TxResult, bool) {
	x := &txExpect{snap: s, key: key, hours: map[uint64][]int64{}}
	first := s.scopes[key.Hour]
	x.read(first)
	var prims []iterItem
	for _, it := range first.items {
		if it.rec.Seq == key.Seq {
			prims = append(prims, it)
		}
	}
	switch {
	case len(prims) == 0:
		return x.missing(first)
	case len(prims) > 1:
		x.setKey(&prims[0].rec)
		for _, it := range prims {
			x.res.Records = append(x.res.Records, x.version(first, it, TxPrimary, true))
		}
		x.keyGap(TxGapConflict)
		x.keyGap(TxGapNoKey)
		x.res.Outcome = TxIncomplete

		return x.res, false
	}

	prim := prims[0]
	if !txOracleIsPrimary(&prim.rec) {
		return x.res, true
	}
	x.setKey(&prim.rec)
	x.k = txOracleKeyOf(&prim.rec)
	x.res.Records = append(x.res.Records, x.version(first, prim, TxPrimary, false))
	if prim.rec.Quality.Has(QualityCorrelationIncomplete) {
		x.keyGap(TxGapCorrelation)
	}
	if !x.k.keyed {
		x.keyGap(TxGapNoKey)
		x.res.Outcome = TxIncomplete

		return x.res, false
	}

	for h := key.Hour + 1; h < key.Hour+int64(maxScopes); h++ {
		x.read(s.scopes[h])
	}
	x.identity()
	x.window()
	x.flags()
	x.completeness()
	x.outcome()

	return x.res, false
}

// read adds the scope sc to the reads, with what its read reports: its defects, unevaluated packs, coldness,
// conflicts, footer errors, and the scope as searched.
func (x *txExpect) read(sc *txOracleScope) {
	x.reads = append(x.reads, sc)
	for _, g := range sc.defects {
		x.gap(g)
	}
	for _, id := range sc.unevaluated {
		x.gap(TxGap{Reason: TxGapUnevaluated, Hours: []int64{sc.hour}, Pack: new(id)})
	}
	if !sc.indexed {
		x.gap(TxGap{Reason: TxGapCold, Hours: []int64{sc.hour}})
	}
	x.res.Conflicts = append(x.res.Conflicts, sc.conflicts...)
	x.res.FooterErrs = append(x.res.FooterErrs, sc.footerErrs...)
	x.res.Searched = append(x.res.Searched, TxScope{Hour: sc.hour, Indexed: sc.indexed, Packs: sc.packs})
}

// gap adds g, its Block -1 unless it names a block of its pack, its Offset -1 unless it is a read defect's.
func (x *txExpect) gap(g TxGap) {
	if g.Pack == nil || g.Reason == TxGapUnevaluated || g.Reason == TxGapCoverage {
		g.Block = -1
	}
	if g.Reason != TxGapRead {
		g.Offset = -1
	}
	x.res.Gaps = append(x.res.Gaps, g)
}

// keyGap adds a gap of reason at the primary's seq in its hour.
func (x *txExpect) keyGap(reason TxGapReason) {
	x.gap(TxGap{Reason: reason, Hours: []int64{x.key.Hour}, Seq: new(x.key.Seq)})
}

// missing returns the result of a primary that scope key.Hour does not hold:
// TxIncomplete with a TxGapNoKey gap when the scope is not indexed, its read has a defect,
// or a coverage entry meets the primary's seq and hour (the gap carries the first);
// ErrNotPrimary otherwise.
func (x *txExpect) missing(sc *txOracleScope) (TxResult, bool) {
	from, to := x.key.Hour*hourNs, (x.key.Hour+1)*hourNs
	var at *Coverage
	for i := range sc.coverage {
		if c := &sc.coverage[i].c; txOracleCoverageMeets(c, x.snap.capture, x.key.Seq, x.key.Seq, from, to) {
			at = cloneCoverage(c)
			break
		}
	}
	if sc.indexed && len(sc.defects) == 0 && at == nil {
		return x.res, true
	}
	x.gap(TxGap{Reason: TxGapNoKey, Hours: []int64{x.key.Hour}, Seq: new(x.key.Seq), Coverage: at})
	x.res.Outcome = TxIncomplete

	return x.res, false
}

// setKey sets the result's association key and match fields from rec, each unavailable field zero.
func (x *txExpect) setKey(rec *Record) {
	r := &x.res
	r.Epoch, r.Dir = rec.Epoch, rec.Dir
	if txOracleField(rec, FieldValiditySessionID, fieldSessionIDEnd) {
		r.SessionID = binary.BigEndian.Uint16(rec.Payload[fieldSessionIDOff:])
	}
	if txOracleField(rec, FieldValiditySystemBytes, fieldSystemBytesEnd) {
		r.SystemBytes = [4]byte(rec.Payload[fieldSystemBytesOff:fieldSystemBytesEnd])
	}
	if txOracleField(rec, FieldValidityFunction, fieldFunctionEnd) {
		r.Function = rec.Payload[fieldFunctionOff]
	}
	if txOracleField(rec, FieldValidityStreamAndW, fieldByte6End) {
		r.Stream, r.W = rec.Payload[fieldByte6Off]&0x7F, rec.Payload[fieldByte6Off]&0x80 != 0
		r.StreamAvailable, r.WAvailable = true, true
	}
}

// version returns the version it of the read of sc as the lookup keeps it, with class, a conflict when conflict is set.
func (x *txExpect) version(sc *txOracleScope, it iterItem, class TxClass, conflict bool) TxRecord {
	return TxRecord{
		Record: it.rec, HeaderExtra: it.extra, Hour: sc.hour, Pack: sc.packs[it.pack], Block: it.block,
		Conflict: conflict || it.conflict, Class: class,
	}
}

// txOracleGroups returns the items of each seq of items, which a read yields one after the other.
func txOracleGroups(items []iterItem) [][]iterItem {
	var out [][]iterItem
	for i := 0; i < len(items); {
		j := i + 1
		for j < len(items) && items[j].rec.Seq == items[i].rec.Seq {
			j++
		}
		out = append(out, items[i:j])
		i = j
	}

	return out
}

// identity finds the seqs at or above the primary's that each read yielded,
// adds a TxGapConflict gap for each seq with several versions within a read (the primary's in its own scope excepted),
// one for each seq several reads yielded, with their hours, and then a TxGapNoKey gap when it is the primary's,
// and keeps every seq group above the primary in which a version plays a role, and every later version of the primary,
// each version a conflict when its seq is, then sorts the kept versions by seq, then hour.
func (x *txExpect) identity() {
	p := x.key.Seq
	for _, sc := range x.reads {
		for _, g := range txOracleGroups(sc.items) {
			seq := g[0].rec.Seq
			if seq < p {
				continue
			}
			x.hours[seq] = append(x.hours[seq], sc.hour)
			if g[0].conflict && (sc.hour != x.key.Hour || seq != p) {
				x.gap(TxGap{Reason: TxGapConflict, Hours: []int64{sc.hour}, Seq: new(seq)})
			}
		}
	}
	for _, seq := range slices.Sorted(maps.Keys(x.hours)) {
		if hs := x.hours[seq]; len(hs) > 1 {
			x.gap(TxGap{Reason: TxGapConflict, Hours: hs, Seq: new(seq)})
			if seq == p {
				x.keyGap(TxGapNoKey)
			}
		}
	}

	x.res.Records[0].Conflict = len(x.hours[p]) > 1
	for _, sc := range x.reads {
		for _, g := range txOracleGroups(sc.items) {
			seq := g[0].rec.Seq
			if seq < p || seq == p && sc.hour == x.key.Hour {
				continue
			}
			classes := make([]TxClass, len(g))
			for i := range g {
				classes[i] = TxPrimary
				if seq != p {
					classes[i] = x.k.roles(&g[i].rec)
				}
			}
			if !slices.ContainsFunc(classes, func(c TxClass) bool { return c != 0 }) {
				continue
			}
			for i := range g {
				x.res.Records = append(x.res.Records, x.version(sc, g[i], classes[i], len(x.hours[seq]) > 1))
			}
		}
	}
	slices.SortStableFunc(x.res.Records, func(a, b TxRecord) int {
		return cmp.Or(cmp.Compare(a.Record.Seq, b.Record.Seq), cmp.Compare(a.Hour, b.Hour))
	})
}

// bounds reports whether the kept version v bounds the window:
// a same-key primary, or a closing record without a conflict.
func (x *txExpect) bounds(v *TxRecord) bool {
	return v.Record.Seq > x.key.Seq && (v.Class.Has(TxSameKeyPrimary) || v.Class.Has(TxClosing) && !v.Conflict)
}

// window finds the window's end, the smallest seq of a version that bounds it,
// and the first possible same-key primary inside it,
// then adds the window's gap: TxGapOpenWindow when nothing bounds it,
// else TxGapSeqGap at the first seq inside it that no read yielded.
func (x *txExpect) window() {
	p := x.key.Seq
	for i := range x.res.Records {
		if v := &x.res.Records[i]; x.bounds(v) && (!x.bounded || v.Record.Seq < x.e) {
			x.e, x.bounded = v.Record.Seq, true
		}
	}
	for i := range x.res.Records {
		v := &x.res.Records[i]
		if seq := v.Record.Seq; x.in(seq) && v.Class.Has(TxPossiblePrimary) && (!x.hasPossible || seq < x.possible) {
			x.possible, x.hasPossible = seq, true
		}
	}
	if !x.bounded {
		x.gap(TxGap{Reason: TxGapOpenWindow})
		x.res.WindowEnd = nil

		return
	}
	x.res.WindowEnd = new(x.e)
	cur := p + 1
	for _, seq := range slices.Sorted(maps.Keys(x.hours)) {
		if seq == cur {
			cur++
		}
	}
	if cur < x.e {
		x.gap(TxGap{Reason: TxGapSeqGap, Seq: new(cur)})
	}
}

// in reports whether seq lies inside the window.
func (x *txExpect) in(seq uint64) bool {
	return seq > x.key.Seq && (!x.bounded || seq < x.e)
}

// flags sets each kept version's window and candidate flags, counts the valid matches,
// and adds a TxGapUnavailable gap for each version inside the window that could be the reply:
// a possible reply, an eligible candidate that is not decidable,
// or a candidate at or after a possible same-key primary.
func (x *txExpect) flags() {
	for i := range x.res.Records {
		v := &x.res.Records[i]
		rec := &v.Record
		v.InWindow = x.in(rec.Seq)
		v.Bound = x.bounded && rec.Seq == x.e && x.bounds(v)
		if !v.Class.Has(TxCandidate) {
			if v.InWindow && v.Class.Has(TxPossibleReply) {
				x.unavailableGap(v)
			}

			continue
		}
		v.Decidable = x.k.streamOK && txOracleField(rec, FieldValidityStreamAndW, fieldByte6End) &&
			txOracleField(rec, FieldValidityFunction, fieldFunctionEnd)
		v.Eligible = v.InWindow && (!x.hasPossible || rec.Seq < x.possible)
		if v.Eligible && v.Decidable {
			fn := int(rec.Payload[fieldFunctionOff])
			v.Valid = rec.Payload[fieldByte6Off]&0x7F == x.k.stream && (fn == int(x.k.function)+1 || fn == 0)
		}
		if v.Valid {
			x.valid++
		}
		if v.InWindow && (v.Class.Has(TxPossibleReply) || !v.Eligible || !v.Decidable) {
			x.unavailableGap(v)
		}
	}
}

// unavailableGap adds the TxGapUnavailable gap of the kept version v.
func (x *txExpect) unavailableGap(v *TxRecord) {
	x.gap(TxGap{Reason: TxGapUnavailable, Hours: []int64{v.Hour}, Pack: new(v.Pack), Block: v.Block, Seq: new(v.Record.Seq)})
}

// completeness adds the gaps of the tracepack semantics specification §7.2
// and of the tracepack storage specification §5, Completeness, beside the window's own:
// coverage, barriers, capture-boundaries, ordering-uncertain, partial evidence, contradictions.
func (x *txExpect) completeness() {
	from, to := x.key.Hour*hourNs, x.key.Hour*hourNs+int64(len(x.reads))*hourNs
	first, last := x.key.Seq+1, uint64(1<<64-1)
	if x.bounded {
		last = x.e - 1
	}
	for _, sc := range x.reads {
		for i := range sc.coverage {
			if c := &sc.coverage[i]; txOracleCoverageMeets(&c.c, x.snap.capture, first, last, from, to) {
				x.gap(TxGap{Reason: TxGapCoverage, Hours: []int64{sc.hour}, Pack: new(c.pack), Coverage: cloneCoverage(&c.c)})
			}
		}
	}

	closed := slices.ContainsFunc(x.res.Records, func(v TxRecord) bool {
		return v.Record.Seq > x.key.Seq && v.Class.Has(TxClosing) && !v.Conflict && v.Record.Epoch == x.k.epoch
	})
	for _, b := range x.snap.evidence.Boundaries {
		if b.Kind == BoundaryKindStopUnclean && !closed {
			x.gap(TxGap{Reason: TxGapBarrier, Barrier: new(cloneBoundary(b))})
		}
	}
	for _, b := range x.snap.barriers {
		if txOracleMeets(b.GapStart, b.GapEnd, from, to) {
			x.gap(TxGap{Reason: TxGapBarrier, Barrier: new(cloneBoundary(b))})
		}
	}
	x.captureBoundaries()
	x.orderingUncertain()
	if x.snap.evidence.Partial {
		x.gap(TxGap{Reason: TxGapEvidence})
	}
	x.contradictions()
}

// captureBoundaries adds a TxGapCaptureBoundary gap for each capture-boundary of the primary's epoch:
// each entry of the evidence,
// then each version of a capture-boundary record of the epoch above the primary that a read yielded,
// unless an entry of the evidence has its seq, kind and epoch;
// the clean stop that bounds the window, a stop at its end whose bounding version is a capture-boundary stop, excepted.
func (x *txExpect) captureBoundaries() {
	stopBound := slices.ContainsFunc(x.res.Records, func(v TxRecord) bool {
		b, ok := txOracleBoundaryOf(x.key.Capture, &v.Record)
		return v.Bound && ok && b.Kind == BoundaryKindStop
	})
	bounding := func(b *Boundary) bool { return stopBound && b.Seq == x.e && b.Kind == BoundaryKindStop }
	for _, b := range x.snap.evidence.Boundaries {
		if b.Epoch == x.k.epoch && !bounding(&b) {
			x.gap(TxGap{Reason: TxGapCaptureBoundary, Seq: new(b.Seq), Barrier: new(cloneBoundary(b))})
		}
	}
	for _, sc := range x.reads {
		for _, it := range sc.items {
			b, ok := txOracleBoundaryOf(x.key.Capture, &it.rec)
			if !ok || b.Seq <= x.key.Seq || b.Epoch != x.k.epoch || bounding(&b) ||
				slices.ContainsFunc(x.snap.evidence.Boundaries, func(e Boundary) bool {
					return e.Seq == b.Seq && e.Kind == b.Kind && e.Epoch == b.Epoch
				}) {
				continue
			}
			x.gap(TxGap{
				Reason: TxGapCaptureBoundary, Hours: []int64{sc.hour}, Pack: new(sc.packs[it.pack]), Block: it.block,
				Seq: new(b.Seq), Barrier: new(cloneBoundary(b)),
			})
		}
	}
}

// orderingUncertain adds a TxGapOrderingUncertain gap
// at the first record of the primary's epoch with ordering-uncertain that the reads yielded, at any seq.
func (x *txExpect) orderingUncertain() {
	for _, sc := range x.reads {
		for _, it := range sc.items {
			if it.rec.Epoch == x.k.epoch && it.rec.Quality.Has(QualityOrderingUncertain) {
				x.gap(TxGap{
					Reason: TxGapOrderingUncertain, Hours: []int64{sc.hour}, Pack: new(sc.packs[it.pack]), Block: it.block,
					Seq: new(it.rec.Seq),
				})

				return
			}
		}
	}
}

// contradictions adds a TxGapContradiction gap for each closure of the evidence that the records read contradict:
// the close_seq of the primary's epoch, which names a record ending the epoch,
// and each clean stop, which names a capture-boundary stop of its epoch;
// one at or below the primary, or at a seq a read yielded where no version is what it names.
func (x *txExpect) contradictions() {
	versionsAt := func(seq uint64) []*Record {
		var out []*Record
		for _, sc := range x.reads {
			for i := range sc.items {
				if sc.items[i].rec.Seq == seq {
					out = append(out, &sc.items[i].rec)
				}
			}
		}

		return out
	}
	claim := func(seq uint64, is func(rec *Record) bool) {
		_, read := x.hours[seq]
		if seq > x.key.Seq && (!read || slices.ContainsFunc(versionsAt(seq), is)) {
			return
		}
		x.gap(TxGap{Reason: TxGapContradiction, Seq: new(seq)})
	}
	isStop := func(epoch uint32) func(rec *Record) bool {
		return func(rec *Record) bool {
			b, ok := txOracleBoundaryOf(x.key.Capture, rec)
			return ok && b.Kind == BoundaryKindStop && rec.Epoch == epoch
		}
	}
	for _, c := range x.snap.evidence.Closures {
		if c.Epoch != x.k.epoch {
			continue
		}
		claim(c.CloseSeq, func(rec *Record) bool {
			if isStop(c.Epoch)(rec) {
				return true
			}
			ev, err := UnmarshalTransportEvent(rec.Payload)

			return rec.Kind == KindTransportEvent && err == nil && ev.Event == EventSocketClose && rec.Epoch == c.Epoch
		})
	}
	for _, b := range x.snap.evidence.Boundaries {
		if b.Kind == BoundaryKindStop {
			claim(b.Seq, isStop(b.Epoch))
		}
	}
}

// outcome decides the outcome: TxIncomplete beside a no-key, conflict or index gap;
// otherwise TxMatched for one valid match, TxAmbiguous for several;
// otherwise TxUnmatched without a gap, TxIncomplete with one.
func (x *txExpect) outcome() {
	r := &x.res
	switch {
	case slices.ContainsFunc(r.Gaps, func(g TxGap) bool {
		return g.Reason == TxGapNoKey || g.Reason == TxGapConflict || g.Reason == TxGapIndex
	}):
		r.Outcome = TxIncomplete
	case x.valid == 1:
		r.Outcome = TxMatched
	case x.valid > 1:
		r.Outcome = TxAmbiguous
	case len(r.Gaps) == 0:
		r.Outcome = TxUnmatched
	default:
		r.Outcome = TxIncomplete
	}
}

// txComparable returns res with what the observation-bounded check does not compare exactly made comparable:
// empty payloads and header extensions nil, gaps without their Err text, sorted in a fixed order of every field,
// and footer errors as their text.
func txComparable(res TxResult) (TxResult, []string) {
	res.Records = slices.Clone(res.Records)
	for i := range res.Records {
		v := &res.Records[i]
		if len(v.Record.Payload) == 0 {
			v.Record.Payload = nil
		}
		if len(v.HeaderExtra) == 0 {
			v.HeaderExtra = nil
		}
	}
	keys := make([]string, 0, len(res.Gaps)+len(res.FooterErrs))
	for i := range res.Gaps {
		keys = append(keys, txGapKey(&res.Gaps[i]))
	}
	slices.Sort(keys)
	for _, fe := range res.FooterErrs {
		keys = append(keys, fe.Error())
	}
	res.Gaps, res.FooterErrs = nil, nil
	if len(res.Conflicts) == 0 {
		res.Conflicts = nil
	}

	return res, keys
}

// txGapKey describes every field of g but Err, pointers by their values.
func txGapKey(g *TxGap) string {
	s := fmt.Sprintf("%v hours=%v block=%d offset=%d defect=%v", g.Reason, g.Hours, g.Block, g.Offset, g.Defect)
	if g.Pack != nil {
		s += fmt.Sprintf(" pack=%v", *g.Pack)
	}
	if g.Seq != nil {
		s += fmt.Sprintf(" seq=%d", *g.Seq)
	}
	if c := g.Coverage; c != nil {
		s += fmt.Sprintf(" coverage=(%v %v %v %v %v %v)", optString(c.CaptureID), optString(c.SeqFirst), optString(c.SeqLast),
			optString(c.TimeStart), optString(c.TimeEnd), c.Unknown)
	}
	if b := g.Barrier; b != nil {
		s += fmt.Sprintf(" barrier=(%v %d %v %d %d %v %v)", b.Capture, b.Seq, b.Kind, b.TS, b.Epoch, optString(b.GapStart), optString(b.GapEnd))
	}

	return s
}

// optString returns the value p points to as text, or "-" for nil.
func optString[T any](p *T) string {
	if p == nil {
		return "-"
	}

	return fmt.Sprint(*p)
}

// requireTxExpected requires res and err, a lookup's, to be what the snapshot expects (txSnapshot.expect):
// every field equal, the gaps as a set of every field but Err text, and the footer errors by their text.
func requireTxExpected(t testing.TB, want TxResult, notPrimary bool, res TxResult, err error) {
	t.Helper()

	if notPrimary {
		require.ErrorIs(t, err, ErrNotPrimary)
	} else {
		require.NoError(t, err)
	}
	w, wantKeys := txComparable(want)
	g, gotKeys := txComparable(res)
	require.Equal(t, wantKeys, gotKeys, "gaps and footer errors")
	require.Equal(t, w, g)
}

// txGenStats counts what the lookups over generated captures found.
type txGenStats struct {
	made                                                                 txGenFacts
	outcomes                                                             map[TxOutcome]int
	gaps                                                                 map[TxGapReason]int
	classes                                                              map[TxClass]int
	lookups, notPrimary, bounded, conflicted, valid, eligible, decidable int
}

// add counts the lookup that returned res and err.
func (s *txGenStats) add(res *TxResult, err error) {
	s.lookups++
	if errors.Is(err, ErrNotPrimary) {
		s.notPrimary++
		return
	}
	s.outcomes[res.Outcome]++
	for _, g := range res.Gaps {
		s.gaps[g.Reason]++
	}
	if res.WindowEnd != nil {
		s.bounded++
	}
	for _, v := range res.Records {
		for bit := TxPrimary; bit <= TxOutcomeRecord; bit <<= 1 {
			if v.Class.Has(bit) {
				s.classes[bit]++
			}
		}
		if v.Conflict {
			s.conflicted++
		}
		if v.Valid {
			s.valid++
		}
		if v.Eligible {
			s.eligible++
		}
		if v.Decidable {
			s.decidable++
		}
	}
}

// genTxLookups and genTxLookupsLong are the numbers of captures TestFindTransactionGenerated generates,
// with and without -short.
const (
	genTxLookups     = 24
	genTxLookupsLong = 96
)

// TestFindTransactionGenerated generates captures (genTxCapture), looks up every record of each at its hour,
// and requires each lookup to return what the observation-bounded check computes from the observation
// (txSnapshot.expect), its state charged as recounted, and to hold over the capture as written (txTruth.violations).
// Together the captures exercise every kind of record, version, pack, evidence and outcome,
// which it checks when every capture ran, not when a -run pattern selects some of them.
func TestFindTransactionGenerated(t *testing.T) {
	t.Parallel()

	n := genTxLookupsLong
	if testing.Short() {
		n = genTxLookups
	}
	var mu sync.Mutex
	sum := txGenStats{outcomes: map[TxOutcome]int{}, gaps: map[TxGapReason]int{}, classes: map[TxClass]int{}}
	ran := 0
	t.Cleanup(func() {
		if ran == n {
			requireTxGenStats(t, n, &sum)
		}
	})
	for seed := range uint64(n) {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			t.Parallel()

			g := genTxCapture(t, seed)
			var stats txGenStats
			stats.outcomes, stats.gaps, stats.classes = map[TxOutcome]int{}, map[TxGapReason]int{}, map[TxClass]int{}
			checkGeneratedTx(t, g, TxOptions{MaxScopes: g.maxScopes}, &stats)

			mu.Lock()
			defer mu.Unlock()
			ran++
			sum.addAll(&stats, &g.made)
		})
	}
}

// addAll adds the counts of o and the facts of made to s.
func (s *txGenStats) addAll(o *txGenStats, made *txGenFacts) {
	s.lookups += o.lookups
	s.notPrimary += o.notPrimary
	s.bounded += o.bounded
	s.conflicted += o.conflicted
	s.valid += o.valid
	s.eligible += o.eligible
	s.decidable += o.decidable
	for k, v := range o.outcomes {
		s.outcomes[k] += v
	}
	for k, v := range o.gaps {
		s.gaps[k] += v
	}
	for k, v := range o.classes {
		s.classes[k] += v
	}
	m := &s.made
	for _, p := range [][2]*int{
		{&m.epochs, &made.epochs}, {&m.backwardSteps, &made.backwardSteps}, {&m.dropped, &made.dropped},
		{&m.unavailable, &made.unavailable}, {&m.unknownDirs, &made.unknownDirs}, {&m.orderingUncertain, &made.orderingUncertain},
		{&m.correlation, &made.correlation}, {&m.primaries, &made.primaries}, {&m.replies, &made.replies}, {&m.aborts, &made.aborts},
		{&m.rejects, &made.rejects}, {&m.t3s, &made.t3s}, {&m.closes, &made.closes}, {&m.refusedSockets, &made.refusedSockets},
		{&m.boundaries, &made.boundaries}, {&m.stops, &made.stops}, {&m.localVersions, &made.localVersions},
		{&m.crossVersions, &made.crossVersions}, {&m.copies, &made.copies}, {&m.walked, &made.walked},
		{&m.unevaluated, &made.unevaluated}, {&m.coverage, &made.coverage}, {&m.invalidFooters, &made.invalidFooters},
		{&m.flipped, &made.flipped}, {&m.staged, &made.staged}, {&m.cold, &made.cold}, {&m.falseClosures, &made.falseClosures},
		{&m.falseStops, &made.falseStops}, {&m.partial, &made.partial}, {&m.captureBarriers, &made.captureBarriers},
		{&m.toolBarriers, &made.toolBarriers},
	} {
		*p[0] += *p[1]
	}
}

// requireTxGenStats logs what n generated captures exercised and requires each count to be positive:
// every fact the generator counts, every outcome, every gap reason but TxGapIndex, every role,
// and lookups failing with ErrNotPrimary, bounded windows, conflicting versions and each candidate flag.
func requireTxGenStats(t *testing.T, n int, s *txGenStats) {
	t.Helper()

	m := &s.made
	counts := map[string]int{
		"epochs": m.epochs, "backward clock steps": m.backwardSteps, "dropped records": m.dropped,
		"unavailable fields": m.unavailable, "unknown directions": m.unknownDirs, "ordering-uncertain": m.orderingUncertain,
		"correlation-incomplete": m.correlation, "primaries": m.primaries, "replies": m.replies, "aborts": m.aborts,
		"Reject.req": m.rejects, "T3 expiries": m.t3s, "socket-closes": m.closes, "refused sockets": m.refusedSockets,
		"capture-boundaries": m.boundaries, "stops": m.stops, "versions within a scope": m.localVersions,
		"versions in another scope": m.crossVersions, "identical copies": m.copies, "walked packs": m.walked,
		"unevaluated packs": m.unevaluated, "coverage entries": m.coverage, "invalid footers": m.invalidFooters,
		"flipped blocks": m.flipped, "staged packs": m.staged, "cold scopes": m.cold, "closure claims": m.falseClosures,
		"stop claims": m.falseStops, "partial evidence": m.partial, "stop-uncleans of the capture": m.captureBarriers,
		"stop-uncleans of the tool": m.toolBarriers,
		"lookups":                   s.lookups, "not a primary": s.notPrimary, "bounded windows": s.bounded, "conflicting versions": s.conflicted,
		"valid": s.valid, "eligible": s.eligible, "decidable": s.decidable,
	}
	for o := TxMatched; o <= TxIncomplete; o++ {
		counts["outcome "+o.String()] = s.outcomes[o]
	}
	for r := TxGapNoKey; r <= TxGapContradiction; r++ {
		if r != TxGapIndex {
			counts["gap "+r.String()] = s.gaps[r]
		}
	}
	for bit := TxPrimary; bit <= TxOutcomeRecord; bit <<= 1 {
		counts["class "+bit.String()] = s.classes[bit]
	}
	t.Logf("over %d captures: %v", n, counts)
	for name, v := range counts {
		assert.Positive(t, v, name)
	}
}

// checkGeneratedTx looks up every key of g with opts and requires each lookup to return what the snapshot expects,
// its state charged as recounted, no TxUnmatched beside a gap,
// and nothing the safety check rejects (txTruth.violations);
// it counts what the lookups found in stats.
func checkGeneratedTx(t *testing.T, g *txGen, opts TxOptions, stats *txGenStats) {
	t.Helper()

	snap := newTxSnapshot(t, g.src, captureLow, g.first, g.first+int64(g.hours+txGenHours))
	tr := txTruthOf(g)
	for _, key := range g.keys {
		var l *txLookup
		res, err := findTransactionWith(t.Context(), g.src, key, opts, func(lk *txLookup) { l = lk })
		requireStateRecount(t, l)
		want, notPrimary := snap.expect(key, opts.MaxScopes)
		requireTxExpected(t, want, notPrimary, res, err)
		if res.Outcome == TxUnmatched {
			require.Empty(t, res.Gaps)
		}
		require.Empty(t, tr.violations(key, opts.MaxScopes, &res, txLookupRuns(l)), "the safety check")
		stats.add(&res, err)
	}
}

// FuzzFindTransactionGenerated looks up every record of the capture genTxCapture generates from a seed,
// with MaxScopes 1 to 3 as the control byte chooses, or the capture's own for 0,
// so that mutations explore valid captures, and requires every lookup to pass both checks (checkGeneratedTx).
func FuzzFindTransactionGenerated(f *testing.F) {
	for i := range uint64(8) {
		f.Add(i, uint8(i%4))
	}

	f.Fuzz(func(t *testing.T, seed uint64, ctl uint8) {
		g := genTxCapture(t, seed)
		opts := TxOptions{MaxScopes: g.maxScopes}
		if n := int(ctl % 4); n > 0 {
			opts.MaxScopes = n
		}
		stats := txGenStats{outcomes: map[TxOutcome]int{}, gaps: map[TxGapReason]int{}, classes: map[TxClass]int{}}
		checkGeneratedTx(t, g, opts, &stats)
	})
}

// Bits of the control byte of FuzzFindTransaction.
const (
	// fuzzTxIndexedShift places the three bits that index the hours key.Hour to key.Hour + 2.
	fuzzTxIndexedShift = 0
	// fuzzTxScopesShift places the two bits that choose MaxScopes, 1 to 3, or 2 for 3.
	fuzzTxScopesShift = 3
	// fuzzTxSmallState sets MaxStateBytes to 4 KiB, and fuzzTxOneConflict MaxConflicts to 1.
	fuzzTxSmallState  = 1 << 5
	fuzzTxOneConflict = 1 << 6
)

// fuzzTxSeeds returns seed inputs of FuzzFindTransaction: the first three packs of a few generated captures,
// each with the seq of a primary of the first, and packs already damaged beside valid ones.
func fuzzTxSeeds(t testing.TB) []fuzzTxInput {
	t.Helper()

	damaged := fuzzReadSeeds(t)[6:]
	out := make([]fuzzTxInput, 0, 6+len(damaged))
	for seed := range uint64(6) {
		g := genTxCapture(t, seed)
		var in fuzzTxInput
		for i := 0; i < len(g.packs) && i < len(in.packs); i++ {
			in.packs[i] = g.packs[i].file
		}
		for _, r := range g.packs[0].recs {
			if txOracleIsPrimary(&r) {
				in.seq = r.Seq
				break
			}
		}
		in.ctl = uint8(seed)<<fuzzTxScopesShift | 0b111
		out = append(out, in)
	}
	for i, in := range damaged {
		out = append(out, fuzzTxInput{packs: in, seq: 10, ctl: uint8(i) | 0b111})
	}

	return out
}

// fuzzTxInput is one input of FuzzFindTransaction.
type fuzzTxInput struct {
	packs [3][]byte
	seq   uint64
	ctl   uint8
}

// FuzzFindTransaction looks up seq in the capture of the first of up to three mutated packs,
// each put in the scope of its pack metadata's period, the hours indexed and the limits as a control byte chooses.
// Packs that do not open are left to FuzzOpen, and inputs with two packs of one pack_id are skipped.
// Every lookup either fails with Outcome zero or returns an outcome, never TxUnmatched beside a gap,
// with its state charged as recounted, each scope read at most once and the observation closed once,
// its records above or at the primary's seq in ascending (seq, hour),
// each version's window and candidate flags consistent with WindowEnd;
// a second lookup returns the same.
// FuzzFindTransactionGenerated mutates valid captures instead.
func FuzzFindTransaction(f *testing.F) {
	for _, in := range fuzzTxSeeds(f) {
		f.Add(in.packs[0], in.packs[1], in.packs[2], in.seq, in.ctl)
	}

	f.Fuzz(func(t *testing.T, a, b, c []byte, seq uint64, ctl uint8) {
		s := newMemSource()
		var key TxKey
		ids := map[UUID]bool{}
		for _, data := range [][]byte{a, b, c} {
			if len(data) == 0 {
				continue
			}
			r, err := Open(t.Context(), bytes.NewReader(data), int64(len(data)), ReaderOptions{})
			if err != nil {
				continue
			}
			h := r.Header()
			if ids[h.PackID] {
				return
			}
			ids[h.PackID] = true
			if len(ids) == 1 {
				key = TxKey{Capture: h.CaptureID, Seq: seq, Hour: hourOf(h.Meta.PeriodStart)}
			}
			s.addPack(t, hourOf(h.Meta.PeriodStart), data)
		}
		if len(ids) == 0 {
			return
		}
		for i := range int64(3) {
			s.setIndexed(key.Capture, key.Hour+i, ctl>>(fuzzTxIndexedShift+i)&1 != 0)
		}
		opts := TxOptions{MaxScopes: 1 + int(ctl>>fuzzTxScopesShift&3)%3}
		if ctl&fuzzTxSmallState != 0 {
			opts.MaxStateBytes = 4 << 10
		}
		if ctl&fuzzTxOneConflict != 0 {
			opts.MaxConflicts = 1
		}

		res, err := requireTxInvariants(t, s, key, opts)
		again, againErr := requireTxInvariants(t, s, key, opts)
		require.Equal(t, err == nil, againErr == nil)
		w, _ := txComparable(res)
		g, _ := txComparable(again)
		require.Equal(t, w, g)
	})
}

// requireTxInvariants looks up key with opts over s and requires what every lookup holds:
// Outcome zero beside an error and an outcome otherwise, never TxUnmatched beside a gap,
// its state charged as recounted, each scope read at most once, the observation closed once when Observe returned one,
// its records at or above the primary's seq in ascending (seq, hour),
// and each version's window and candidate flags consistent with WindowEnd.
func requireTxInvariants(t *testing.T, s *memSource, key TxKey, opts TxOptions) (TxResult, error) {
	t.Helper()

	observes0, scopes0, closes0 := s.counts()
	var l *txLookup
	res, err := findTransactionWith(t.Context(), s, key, opts, func(lk *txLookup) { l = lk })
	observes, scopes, closes := s.counts()
	opened := observes - observes0
	if err != nil && strings.Contains(err.Error(), "find transaction: observe hours") {
		opened = 0
	}
	require.Equal(t, opened, closes-closes0, "every observation opened closed once")
	for h, n := range scopes {
		require.LessOrEqual(t, n-scopes0[h], 1, "hour %d read at most once", h)
		require.True(t, h >= key.Hour && h < key.Hour+int64(max(opts.MaxScopes, 1)), "hour %d read", h)
	}
	if l != nil {
		requireStateRecount(t, l)
	}
	if err != nil {
		require.Zero(t, res.Outcome)
		return res, err
	}
	require.True(t, res.Outcome >= TxMatched && res.Outcome <= TxIncomplete, "outcome %v", res.Outcome)
	if res.Outcome == TxUnmatched {
		require.Empty(t, res.Gaps)
	}
	if res.WindowEnd != nil {
		require.Greater(t, *res.WindowEnd, key.Seq)
	}
	for i := range res.Records {
		v := &res.Records[i]
		seq := v.Record.Seq
		require.GreaterOrEqual(t, seq, key.Seq)
		if i > 0 {
			prev := &res.Records[i-1]
			require.LessOrEqual(t, cmp.Or(cmp.Compare(prev.Record.Seq, seq), cmp.Compare(prev.Hour, v.Hour)), 0, "record %d in order", i)
		}
		in := seq > key.Seq && (res.WindowEnd == nil || seq < *res.WindowEnd)
		require.Equal(t, in, v.InWindow, "record %d in the window", i)
		require.False(t, v.Bound && (res.WindowEnd == nil || seq != *res.WindowEnd), "record %d bounds the window at its end only", i)
		require.False(t, !v.Class.Has(TxCandidate) && (v.Decidable || v.Eligible || v.Valid), "record %d has flags of a candidate", i)
		require.False(t, v.Valid && (!v.Eligible || !v.Decidable), "record %d valid but not eligible and decidable", i)
		require.False(t, v.Eligible && !v.InWindow, "record %d eligible outside the window", i)
	}

	return res, nil
}
