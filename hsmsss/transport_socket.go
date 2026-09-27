package hsmsss

// transport_socket.go — the socket record: one per socket this transport dials or accepts, refused sockets included.
// It carries the socket's identity, reports the socket to hsms.WithSocketObserver through the runtime's optional capability,
// owns the one close gate every path that closes the socket goes through,
// and orders every report of a frame that crossed the socket ahead of the report of its close.
// It also carries the frames of a refused socket's exchange from the accept goroutine to the goroutine that reports them.

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

// The connection core reports its outbound frames only through a transport with this capability (WireReportScope);
// it is reached by type assertion, so nothing else would catch its loss.
var _ interface {
	WireReportScope(socket uint64) func()
} = (*transport)(nil)

// adoptRuntime is the optional capability a runtime offers to adopt a socket together with the identity this transport minted for it.
// A runtime without it is handed the socket through tcpUp instead, and its generation never learns the identity.
type adoptRuntime = gencap.SocketAdopter

// socketRecord is one socket this transport dialed or accepted.
//
// It is minted as soon as the dial or accept returns, before anything decides the socket's fate,
// so a socket that is sealed out, refused by the core, or refused as an extra passive peer is still recorded and reported.
// Its close is a gate:
// the first call closes the conn, has the socket reported closed with the failure it was handed, and wins;
// every later call does nothing and loses.
// Every path that closes a recorded socket goes through the gate,
// which is what makes the close reported exactly once, with the failure of whichever path got there first.
//
// A frame's report is ordered ahead of the close's through report scopes (openReport), without anyone waiting:
// a goroutine that moves a frame across the socket holds a scope from before the read or write until after the frame's report,
// or hands the scope to the goroutine that reports the frame for it,
// the record counts the open scopes,
// and the close is reported by whichever goroutine finds the count at zero once the gate's winner has closed the conn:
// the winner itself when no scope is open, otherwise the goroutine that ends the last one.
// Whichever goroutine delivers it, the close report carries the time the winner closed the conn.
type socketRecord struct {
	t    *transport
	conn net.Conn
	id   uint64

	// local and remote are captured at mint, so the close reports the addresses even for a conn that forgets them once closed.
	local  net.Addr
	remote net.Addr

	// gen is the generation that adopted the socket, or 0 while it is not adopted.
	// It is stored before the adoption is attempted,
	// so a teardown that closes the socket the instant it is adopted still reports the generation,
	// and reset to 0 when the adoption is refused.
	gen atomic.Uint64

	// release ends one report scope (endReport).
	// It is bound once here, so handing it out per frame allocates nothing.
	release func()

	// mu guards the fields below and is held only for that bookkeeping:
	// never across the conn's Close, an observer call, or any other lock.
	mu             sync.Mutex
	closed         bool      // set by the gate's winner, before it closes the conn
	closedByReport bool      // the winner was a disconnect report (closeForReport)
	openReports    int       // report scopes open (openReport) and not yet ended (endReport)
	closing        bool      // the winner has decided the close, set with closed: no scope opens any more
	closeArmed     bool      // the winner has closed the conn: the close report is pending or made
	closeErr       error     // the failure the close reports, set with closeArmed
	closedAt       time.Time // when the winner closed the conn, set with closeArmed; the close report's At, however late it is delivered
	closeReported  bool      // the close report has been taken, by the winner or by the goroutine ending the last scope
}

// refusalCapture carries the frames of one refused socket's exchange (refusalExchange) to the goroutine that reports them,
// together with the socket's report scope, which that goroutine releases once it has reported.
//
// The exchange runs on the accept goroutine, which Stop joins without a bound,
// so the accept goroutine must never call the wire observer:
// it captures each frame as it crosses, with its At taken at that moment,
// and hands the capture to a new goroutine (deliver) once the exchange has ended, while the scope is still held.
// One capture is allocated per refused socket whose exchange is reported.
type refusalCapture struct {
	wr      wireRuntime
	rec     *socketRecord
	release func()

	// in and out hold the frames that crossed, both header-only control frames of 14 bytes:
	// the peer's first frame, and the Select.rsp refusing it.
	// Each is present only when its flag is set, with its At taken when the read or write completed.
	in, out       [14]byte
	inAt, outAt   time.Time
	hasIn, hasOut bool
}

// mintSocket records conn as a new socket with the next identity and reports it with kind (connected or accepted).
// Identities start at 1 and never repeat within this transport, which serves exactly one connection.
func (t *transport) mintSocket(conn net.Conn, kind hsms.SocketEventKind) *socketRecord {
	rec := &socketRecord{t: t, conn: conn, id: t.socketSeq.Add(1), local: conn.LocalAddr(), remote: conn.RemoteAddr()}
	rec.release = rec.endReport
	t.observeSocket(rec.event(kind, nil, time.Now()))

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

// WireReportScope opens the report scope of socket, the socket an epoch adopted, around one of the core's outbound writes.
// The connection core reaches it by type assertion for its outbound frames,
// and its presence is also what tells the core that Write puts a frame on the socket byte for byte,
// so the core's outbound frames are reported to an hsms.WithWireObserver hook;
// it is not meant to be called otherwise.
//
// It returns the scope's release, for the core to call after the frame's report,
// or nil when the socket's close is already pending,
// or when socket is not the socket adoptSocket published last:
// an epoch's socket is closed before the next generation's Start can publish another,
// so a socket that is no longer the last one published is already closed.
func (t *transport) WireReportScope(socket uint64) func() {
	rec := t.adopted.Load()
	if rec == nil || rec.id != socket {
		return nil
	}

	return rec.openReport()
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

// captureRefusal opens rec's report scope for its refusal exchange and returns the capture that carries the exchange's frames,
// or nil when the frames are not to be reported:
// the runtime offers no wire observer, or the socket's close is already pending (openReport).
// The caller hands the capture to finish once the exchange has ended, on a panic too.
func (t *transport) captureRefusal(rec *socketRecord) *refusalCapture {
	wr, ok := t.rt.(wireRuntime)
	if !ok {
		return nil
	}

	release := rec.openReport()
	if release == nil {
		return nil
	}

	return &refusalCapture{wr: wr, rec: rec, release: release}
}

// event builds a socket event of kind for this record, stamped at.
func (r *socketRecord) event(kind hsms.SocketEventKind, err error, at time.Time) hsms.SocketEvent {
	return hsms.SocketEvent{
		Kind: kind, Socket: r.id, Generation: r.gen.Load(), At: at,
		Local: r.local, Remote: r.remote, Err: err,
	}
}

// openReport opens a report scope on the socket and returns its release, for the caller to call once,
// or returns nil once the socket's close is decided (the gate has a winner, whether or not the conn is closed yet);
// a frame read or written without a scope is not reported.
//
// The caller opens it before the read or write that moves the frame and releases it after the frame's report,
// on a panic in the observer too;
// a caller that must not call the observer itself hands the scope to the goroutine that reports for it (refusalCapture).
// While a scope is open the close report is held back, never the close itself:
// the gate's winner closes the conn, which fails a read or write in progress, and leaves the report to the release.
func (r *socketRecord) openReport() func() {
	r.mu.Lock()
	if r.closing {
		r.mu.Unlock()

		return nil
	}
	r.openReports++
	r.mu.Unlock()

	return r.release
}

// endReport ends one report scope (openReport) and reports the socket closed when it was the last one
// and the gate's winner has already closed the conn and left the report to it.
// The report is taken under mu, exactly once between this and the gate, and delivered outside it,
// stamped with the time the winner closed the conn, not with the time of this delivery.
func (r *socketRecord) endReport() {
	r.mu.Lock()
	r.openReports--
	report := r.closeArmed && r.openReports == 0 && !r.closeReported
	if report {
		r.closeReported = true
	}
	err, at := r.closeErr, r.closedAt
	r.mu.Unlock()

	if report {
		r.t.observeSocket(r.event(hsms.SocketClosed, err, at))
	}
}

// close is the socket's close gate, for every path that is not a disconnect report.
// The first call closes the conn and reports the socket closed with err,
// the failure that initiated the close (nil when none did).
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
// The winner is decided under mu, and everything else runs after it is released,
// so a losing caller returns at once and never waits on the conn's Close or on the observer.
//
// The winner marks the socket closing as it is decided, which refuses every later report scope (openReport),
// then closes the conn, which fails any read or write in progress on it,
// and takes the time of that close, which every close report carries however late it is delivered;
// then it arms the close report under mu and takes it only when no scope is open;
// otherwise the goroutine that ends the last open scope reports the close (endReport).
// So every frame read or written while a scope was open is reported before the close,
// a frame read or written after that is never reported,
// and the winner never waits: on a goroutine parked inside an observer call it delays only the close report,
// which that goroutine delivers when its call returns.
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
	r.closing = true
	r.mu.Unlock()

	_ = r.conn.Close()
	closedAt := time.Now()

	r.mu.Lock()
	r.closeArmed = true
	r.closeErr = err
	r.closedAt = closedAt
	report := r.openReports == 0
	if report {
		r.closeReported = true
	}
	r.mu.Unlock()

	if report {
		r.t.observeSocket(r.event(hsms.SocketClosed, err, closedAt))
	}

	return true, false
}

// inbound records frame, the peer's first frame, as read now.
func (c *refusalCapture) inbound(frame []byte) {
	copy(c.in[:], frame)
	c.inAt = time.Now()
	c.hasIn = true
}

// outbound records frame, the Select.rsp, as written now.
func (c *refusalCapture) outbound(frame []byte) {
	copy(c.out[:], frame)
	c.outAt = time.Now()
	c.hasOut = true
}

// finish ends the capture once the exchange has ended, while the caller still holds the record's scope:
// when a frame crossed, a new goroutine reports the frames and releases the scope (deliver);
// when none did, there is nothing to report and the scope is released here.
// Nothing joins the goroutine, so an observer that never returns leaks it and delays only this socket's close report.
func (c *refusalCapture) finish() {
	if !c.hasIn && !c.hasOut {
		c.release()

		return
	}

	go c.deliver()
}

// deliver reports the captured frames to the wire observer in wire order, the peer's frame first,
// and then releases the record's report scope, on a panic in the observer too,
// so the close report that the scope holds back is never lost.
// A refused socket belongs to no generation, so the frames carry generation 0.
// A panic in the observer is not recovered, as at every other observer call site.
func (c *refusalCapture) deliver() {
	defer c.release()

	if c.hasIn {
		c.wr.ObserveWire(hsms.WireEvent{Direction: hsms.WireInbound, Socket: c.rec.id, At: c.inAt, Frame: c.in[:]})
	}

	if c.hasOut {
		c.wr.ObserveWire(hsms.WireEvent{Direction: hsms.WireOutbound, Socket: c.rec.id, At: c.outAt, Frame: c.out[:]})
	}
}
