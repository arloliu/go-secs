package hsmsss

// transport_socket.go — the socket record: one per socket this transport dials or accepts, refused sockets included.
// It carries the socket's identity, reports the socket to hsms.WithSocketObserver through the runtime's optional capability,
// and owns the one close gate every path that closes the socket goes through.

import (
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/internal/gencap"
)

// errRefusalBadLength is the close failure of a refused socket whose first frame does not announce a header-only control frame.
var errRefusalBadLength = errors.New("hsmsss: refused connection sent a first frame that is not a header-only control frame")

// errRefusalNotSelectReq is the close failure of a refused socket whose first frame is a control frame other than a Select.req.
var errRefusalNotSelectReq = errors.New("hsmsss: refused connection sent a first frame that is not a Select.req")

// socketRuntime is the optional capability a runtime offers to receive the life of every socket this transport dials or accepts,
// for the observer hsms.WithSocketObserver installs.
// It is reached by type assertion for the same reason causeRuntime is,
// and a runtime without it simply receives no socket events.
type socketRuntime interface {
	ObserveSocket(ev hsms.SocketEvent)
}

// adoptRuntime is the optional capability a runtime offers to adopt a socket together with the identity this transport minted for it.
// A runtime without it is handed the socket through tcpUp instead, and its generation never learns the identity.
type adoptRuntime = gencap.SocketAdopter

// socketRecord is one socket this transport dialed or accepted.
//
// It is minted as soon as the dial or accept returns, before anything decides the socket's fate,
// so a socket that is sealed out, refused by the core, or refused as an extra passive peer is still recorded and reported.
// Its close is a gate: the first call closes the conn, reports the socket closed with the failure it was handed, and wins;
// every later call does nothing and loses.
// Every path that closes a recorded socket goes through the gate,
// which is what makes the close reported exactly once, with the failure of whichever path got there first.
type socketRecord struct {
	t    *transport
	conn net.Conn
	id   uint64

	// local and remote are captured at mint, so the close reports the addresses even for a conn that forgets them once closed.
	local  net.Addr
	remote net.Addr

	// refused marks an extra passive socket the refusal exchange owns.
	// Its gate reports it refused immediately before it reports it closed,
	// so the refusal is reported exactly once and always ahead of the close, whichever side closes it.
	// Written once, before the record is published to anything that can close it.
	refused bool

	// gen is the generation that adopted the socket, or 0 while it is not adopted.
	// It is stored before the adoption is attempted,
	// so a teardown that closes the socket the instant it is adopted still reports the generation,
	// and reset to 0 when the adoption is refused.
	gen atomic.Uint64

	mu             sync.Mutex
	closed         bool // guarded by mu; set by the gate's winner
	closedByReport bool // guarded by mu; the winner was a disconnect report (closeForReport)
}

// mintSocket records conn as a new socket with the next identity and reports it with kind (connected or accepted).
// Identities start at 1 and never repeat within this transport, which serves exactly one connection.
func (t *transport) mintSocket(conn net.Conn, kind hsms.SocketEventKind) *socketRecord {
	rec := &socketRecord{t: t, conn: conn, id: t.socketSeq.Add(1), local: conn.LocalAddr(), remote: conn.RemoteAddr()}
	t.observeSocket(rec.event(kind, nil))

	return rec
}

// observeSocket reports ev to the runtime's socket observer, when the runtime offers the capability.
func (t *transport) observeSocket(ev hsms.SocketEvent) {
	if sr, ok := t.rt.(socketRuntime); ok {
		sr.ObserveSocket(ev)
	}
}

// adoptSocket reports rec's socket up for g, the generation that dialed or accepted it,
// and reports whether the core accepted it onto that live generation.
// On false the caller still owns the socket and closes it through rec's gate.
//
// The record is published as the one CloseSocket resolves before the core can see the socket,
// so an epoch whose teardown closes the socket the moment it is adopted still closes it through the gate.
// A runtime without the adoption capability is handed the socket through tcpUp.
func (t *transport) adoptSocket(g *genWG, rec *socketRecord) bool {
	rec.gen.Store(g.gen)
	t.adopted.Store(rec)

	var accepted bool
	if ar, ok := t.rt.(adoptRuntime); ok {
		accepted = ar.AdoptSocketFromGeneration(g.gen, rec.conn, rec.id)
	} else {
		accepted = t.tcpUp(g.gen, rec.conn)
	}

	if !accepted {
		rec.gen.Store(0)
	}

	return accepted
}

// CloseSocket closes conn, the socket an epoch adopted, through its record's gate with err, the failure the epoch recorded.
// The connection core reaches it by type assertion from the epoch's socket close;
// it is not meant to be called otherwise.
//
// The record is the one adoptSocket published last, never found by comparing conn:
// a conn from a caller-supplied dialer or listener may have a non-comparable dynamic type.
// That record is conn's own, because only an adopted socket ever reaches an epoch,
// and a generation's socket is adopted, used and closed before the next generation's Start can mint another.
func (t *transport) CloseSocket(conn net.Conn, err error) {
	if rec := t.adopted.Load(); rec != nil {
		rec.close(err)

		return
	}

	_ = conn.Close()
}

// closeRecorded closes conn through rec's gate with no failure, or directly when conn has no record.
func closeRecorded(rec *socketRecord, conn net.Conn) {
	if rec != nil {
		rec.close(nil)

		return
	}

	_ = conn.Close()
}

// observeRefusalFrame reports one frame of a refused socket's exchange to the runtime's wire observer.
// A refused socket belongs to no generation, so the frame carries generation 0.
func (t *transport) observeRefusalFrame(rec *socketRecord, dir hsms.WireDirection, frame []byte) {
	if wr, ok := t.rt.(wireRuntime); ok {
		wr.ObserveWire(hsms.WireEvent{Direction: dir, Socket: rec.id, At: time.Now(), Frame: frame})
	}
}

// event builds a socket event of kind for this record, stamped now.
func (r *socketRecord) event(kind hsms.SocketEventKind, err error) hsms.SocketEvent {
	return hsms.SocketEvent{
		Kind: kind, Socket: r.id, Generation: r.gen.Load(), At: time.Now(),
		Local: r.local, Remote: r.remote, Err: err,
	}
}

// close is the socket's close gate, for every path that is not a disconnect report.
// The first call closes the conn and reports the socket closed with err,
// the failure that initiated the close (nil when none did),
// preceded by the refusal for a refused socket.
// Every later call does nothing.
func (r *socketRecord) close(err error) {
	r.gate(err, false)
}

// closeForReport is the gate for a disconnect report (tcpDown), and reports whether that report should still be made.
// It should, unless another disconnect report won the gate first:
// that report owns the socket's end,
// and the read error its close provokes in the recv loop must not reach the core ahead of it, or instead of it.
// A report that lost to any other close — a teardown, Stop, a refusal — is still made;
// the core discards it or finds the link already down, exactly as it did before the gate existed.
func (r *socketRecord) closeForReport(err error) bool {
	won, lostToReport := r.gate(err, true)

	return won || !lostToReport
}

// gate decides the socket's close.
// The winner is decided under mu, and the close and the reports run after it is released,
// so a racing caller never waits on the conn's Close or on the observer.
// It reports whether this call won and, when it did not, whether the winner was a disconnect report.
func (r *socketRecord) gate(err error, byReport bool) (won, lostToReport bool) {
	r.mu.Lock()
	if r.closed {
		lostToReport = r.closedByReport
		r.mu.Unlock()

		return false, lostToReport
	}
	r.closed = true
	r.closedByReport = byReport
	r.mu.Unlock()

	_ = r.conn.Close()

	if r.refused {
		r.t.observeSocket(r.event(hsms.SocketRefused, nil))
	}

	r.t.observeSocket(r.event(hsms.SocketClosed, err))

	return true, false
}
