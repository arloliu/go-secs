package hsms

import (
	"net"
	"strconv"
	"time"
)

// SocketEventKind says what happened to a socket (see [SocketEvent]).
type SocketEventKind uint8

const (
	// SocketConnected marks a socket the active role dialed successfully.
	SocketConnected SocketEventKind = iota + 1
	// SocketAccepted marks a socket the passive role accepted, including one it goes on to refuse.
	SocketAccepted
	// SocketRefused marks an accepted socket the passive role rejected because a session was already live.
	// It is reported as soon as the passive role decides to refuse the socket, before the refusal exchange with the peer.
	SocketRefused
	// SocketClosed marks a socket that has been closed, by either side or by the connection's teardown.
	// It is reported once, after every frame the socket carried:
	// by the goroutine that closed the socket, or, when a frame report was in progress on the socket at that moment,
	// by the goroutine that made that report, once the wire observer has returned.
	// Either way its At is the time the socket was closed.
	SocketClosed
)

// SocketEvent describes one step in the life of a socket the connection dialed or accepted —
// see [WithSocketObserver] for which steps are reported and in what order.
type SocketEvent struct {
	// Kind says what happened.
	Kind SocketEventKind
	// Socket identifies the socket within this connection:
	// a per-connection counter that advances for every socket dialed or accepted, refused ones included,
	// so it is never 0 and never reused while the connection lives.
	// It is the same value [WireEvent.Socket] carries for the frames that crossed this socket.
	Socket uint64
	// Generation identifies the connection generation that adopted the socket, as [WireEvent.Generation] does.
	// It is 0 while the socket is not adopted:
	// on its Connected or Accepted event,
	// and on every event of a socket that is never adopted —
	// a refused socket, a dial that completed after Close began, or a socket the connection declined because its generation had already ended.
	Generation uint64
	// At is taken when the event happened.
	// For a SocketClosed that is when the socket was closed,
	// even when the event is delivered later, once a frame report that was in progress on the socket has returned.
	// It carries a monotonic clock reading, like [WireEvent.At], so the two event streams can be ordered against each other.
	At time.Time
	// Local and Remote are the socket's addresses, as its net.Conn reported them when it was dialed or accepted.
	// Either can be nil for a connection supplied by a custom dialer or listener.
	Local  net.Addr
	Remote net.Addr
	// Err is set only on a SocketClosed event, and only when a failure initiated the close:
	// the read or write error that broke the link,
	// a protocol failure that made the connection drop it
	// (a refused or unanswered Select, a failed linktest, [ErrT7Timeout]),
	// or, for a refused socket, whatever cut its refusal exchange short.
	// It is nil for a close the application asked for, for a close the peer asked for with a Separate.req,
	// and for a refused socket whose refusal exchange completed.
	Err error
}

// socketCloser is the optional transport capability that takes over closing the socket an epoch adopted.
//
// It is reached by type assertion, deliberately not a method of the transport seam,
// so a transport implementation that predates it keeps compiling;
// without it the epoch closes its socket directly.
// The HSMS-SS transport offers it so that every socket, adopted or not, closes through one gate
// that records the failure that initiated the close and reports the close exactly once.
// err is the failure the epoch recorded (see epoch.recordCloseFailure), or nil for a close no failure initiated.
type socketCloser interface {
	CloseSocket(conn net.Conn, err error)
}

// String returns the string representation of the SocketEventKind.
func (k SocketEventKind) String() string {
	switch k {
	case SocketConnected:
		return "Connected"
	case SocketAccepted:
		return "Accepted"
	case SocketRefused:
		return "Refused"
	case SocketClosed:
		return "Closed"
	default:
		return "SocketEventKind(" + strconv.FormatUint(uint64(k), 10) + ")"
	}
}

// ObserveSocket hands ev to the observer installed by [WithSocketObserver], and returns at once when none is.
//
// It is the capability the HSMS-SS transport reaches by type assertion to report the sockets it dials, accepts, refuses and closes;
// it is not part of any interface an application implements or calls.
func (c *connection) ObserveSocket(ev SocketEvent) {
	if obs := c.cfg.Load().socketObserver; obs != nil {
		obs(ev)
	}
}

// AdoptSocketFromGeneration is [connection.TCPUpFromGeneration] that also records socket,
// the identity the transport minted for conn, on the generation that adopts it.
//
// The identity is stored with the conn, under the same liveness check,
// so a generation that has already ended refuses both and keeps whatever it recorded before.
// The epoch keeps the identity after its teardown clears the conn,
// so an event built for that generation afterwards still names the socket.
// It reports whether conn was accepted onto a live generation, exactly as TCPUpFromGeneration does.
func (c *connection) AdoptSocketFromGeneration(gen uint64, conn net.Conn, socket uint64) bool {
	return c.commitTCPUp(gen, conn, socket)
}

// bindSocketCloser returns the close the epoch uses for its socket:
// the transport's CloseSocket capability when it offers one, or nil so the epoch closes the socket directly.
func (c *connection) bindSocketCloser() func(net.Conn, error) {
	if sc, ok := c.tr.(socketCloser); ok {
		return sc.CloseSocket
	}

	return nil
}
