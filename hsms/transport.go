package hsms

import (
	"context"
	"net"
	"time"
)

// transport is the unexported core->transport seam (spec §5.4): the surface the shared
// connection core drives on a concrete per-transport implementation (HSMS-SS in this
// sub-project, SECS-I in SP5b). It is an UNEXPORTED type with EXPORTED methods, so
// hsmsss/secs1 can implement it from another package while consumers cannot name it
// (the sealed-A boundary). The core constructs its own value and hands it to Start as
// the TransportRuntime back-channel.
type transport interface {
	// Start dials (active) or accepts (passive) and spawns the per-generation recv loop
	// that drives rt (the connection core). rt is the back-channel the transport calls to
	// report TCP lifecycle, deliver frames, and route messages.
	Start(ctx context.Context, rt TransportRuntime) error

	// IsActive reports whether this transport is configured for the active (dialing) role, as
	// opposed to passive (listening). Open uses it to decide whether a first-dial failure under
	// OpenBackground is a retryable cold-peer condition (active) or a fatal local/listen error
	// (passive) — see the Active first-connect contract doc on (*connection).Open.
	IsActive() bool

	// Stop joins the per-generation recv loop and releases transport resources. It is
	// invoked by epoch teardown (Codex round-7) after the socket has already been closed,
	// so a parked recv Read is already unblocked (J5). Stop seals the transport's
	// Add-vs-Wait guard (I1) so a concurrent/subsequent Start aborts its WaitGroup Adds
	// rather than racing Stop's Waits on the GENERATION's WaitGroup bundle (NEW-1: each
	// generation has its own bundle; Stop captures and waits the one it sealed).
	Stop(ctx context.Context) error

	// ArmStart clears the Stop-seal (I1) AND installs a FRESH per-generation WaitGroup bundle
	// (NEW-1) so the NEXT generation's Start registers its goroutines on a bundle no prior
	// generation — nor any straggler a bounded Stop abandoned — touches. The core calls it —
	// under the start-linearization mutex in the reconnect loop, and under lifeMu in Open —
	// immediately BEFORE publishing a fresh generation and calling Start. Ordering it before the
	// publish is what makes it safe: a voluntary Close that seals a just-published successor
	// generation still wins (its Stop-seal is never undone by a later arm), so Start-vs-Stop for
	// that generation can never race Add-vs-Wait.
	ArmStart()

	// Write performs the low-level writev of the pre-framed buffers over the raw
	// *net.TCPConn (spec §6.2). Framing happens in the core; this is only the byte sink.
	//
	// conn is the EPOCH's socket, captured by the core and passed in explicitly (I1 full-fix):
	// the transport must write onto exactly this conn, NOT re-resolve its own current socket.
	// Binding the write to the generation's conn closes the residual TOCTOU where a sync sender
	// pinned to epoch N — descheduled across a full teardown+reconnect — could otherwise writev
	// onto epoch N+1's live socket. A stale sender's captured conn is epoch N's (closed by
	// teardown), so the write errors instead of hitting the successor. bufs.WriteTo still takes
	// the writev fast path because conn's dynamic type is *net.TCPConn.
	Write(ctx context.Context, conn net.Conn, bufs net.Buffers) error

	// SetReadDeadline sets the read deadline on conn — the epoch's socket, passed explicitly
	// for the SAME reason as Write/SetWriteDeadline (I1): the transport must never re-resolve
	// its own current socket, or a future caller would reintroduce the cross-generation TOCTOU
	// this seam's write side closes. It currently has no core caller (the recv loop arms its
	// read deadline on its own captured conn directly), but stays conn-bound for symmetry.
	SetReadDeadline(conn net.Conn, t time.Time) error

	// SetWriteDeadline sets the write deadline on conn — the SAME epoch socket handed to Write
	// (I1 full-fix), so the bounded-write deadline is armed on the exact conn the frame is written
	// to, never a successor generation's socket.
	SetWriteDeadline(conn net.Conn, t time.Time) error
}

// TransportRuntime is the back-channel from a transport implementation into the
// shared connection core. The transport calls these methods to report TCP lifecycle
// events, deliver owned frames, and route inbound messages.
//
// TransportRuntime is exported so that the hsmsss and secs1 transport packages can name it in
// their Start signatures, but it is effectively sealed: DeliverOwnedFrame accepts
// an owned []byte that only the in-module recv loop produces, and
// WriteMessage/SendAsync take [Message] whose body uses the unexported wire.Body
// interface — an external type can name TransportRuntime but cannot usefully
// implement or drive it.
type TransportRuntime interface {
	// TCPUp is called by the transport when a TCP connection is established.
	// It advances the FSM NotConnected -> NotSelected SYNCHRONOUSLY via a guarded
	// CAS (CommitConnected) before returning — so State() reads NotSelected the
	// instant TCPUp returns — and enqueues evTCPUp for the deduped reaction/notify.
	TCPUp(conn net.Conn)

	// TCPDown is called by the transport when the TCP connection is lost.
	// cause classifies the failure (graceful vs comms-failure) for the teardown
	// farewell decision (E37 §9.1.1).
	TCPDown(cause error)

	// CommitSelected atomically advances the FSM to Selected before the transport
	// emits Select.rsp (the simultaneous-Select case, E37 §7.4.3). Returns true
	// if the CAS succeeded (this caller committed the transition), false if the
	// state was already Selected (idempotent — the simultaneous case).
	CommitSelected() (committed bool)

	// SelectLost is called when the Selected state is lost due to a peer Separate
	// or Deselect, injecting evSelectLost (Selected → NotSelected).
	SelectLost()

	// T7Expired is called by the transport's T7 (NOT-SELECTED dwell) timer on expiry. It injects
	// evT7Timeout, which the supervisor evaluates SERIALLY: NotSelected -> NotConnected (reconnect),
	// but a NO-OP if the session has since reached Selected or NotConnected — so a validly-Selected
	// session is NEVER torn down by a stale T7 (E37 §9.2.2).
	T7Expired()

	// DeliverOwnedFrame passes a freshly-read, GC-owned frame buffer to the core
	// for decode and routing. The transport transfers ownership of frame; the core
	// must not be called with the same buffer again.
	DeliverOwnedFrame(frame []byte) error

	// RouteReply looks up the System Bytes of msg in the per-generation reply
	// registry. Returns true if a waiting sender received the reply, false if the
	// reply was unsolicited (routed as unsolicited or dropped).
	RouteReply(msg Message) bool

	// RouteData delivers an inbound data message to the session fan-out.
	RouteData(msg *DataMessage) error

	// WriteMessage performs a synchronous framed write of msg over the current
	// epoch's TCP connection (a core-owned writev). Used for W-bit primary
	// data messages and synchronous control sends. Returns the sent message (for
	// System-Bytes accounting) and any write error.
	WriteMessage(ctx context.Context, msg Message) (Message, error)

	// WriteMessageNoReply performs a synchronous framed write of msg over the
	// current epoch's TCP connection WITHOUT registering a reply expectation. The
	// B1 pre-write gate and the B2 write-boundary re-check still apply, but no
	// reply channel is created and no protocol timer is armed, so a secondary the
	// peer later sends against msg's System Bytes misses the reply registry and is
	// delivered to the session's DataMessageHandlers (RouteData). Backs
	// SECS2Endpoint.ForwardDataMessage. Returns the write error, or nil once the
	// frame is on the wire.
	WriteMessageNoReply(ctx context.Context, msg Message) error

	// SendAsync enqueues msg on the per-generation async send channel. Used for
	// fire-and-forget sends (Reject, Separate, S9Fx, async data) where the caller
	// must not block on a wedged peer.
	SendAsync(ctx context.Context, msg Message) error

	// State returns the current FSM ConnState (lock-free atomic read).
	State() ConnState

	// Done returns the per-generation cancellation channel. Closed when the current
	// epoch is torn down. SELECT-ONLY — not a blocking call.
	Done() <-chan struct{}

	// Timers returns the protocol timer configuration for the current connection.
	Timers() TimerConfig

	// SessionID returns the configured HSMS session ID (0xFFFF for HSMS-SS control
	// frames per E37.1).
	SessionID() uint16

	// LinktestInterval returns the LIVE auto-linktest interval (0 = disabled). The auto-linktest
	// goroutine reads it once per entry to Selected (a reconfig applies on the next
	// Selected-entry, never mid-session — no live ticker.Reset).
	LinktestInterval() time.Duration

	// LinktestFailThreshold returns the LIVE consecutive-T6-timeout count that triggers a
	// linktest-failure disconnect (>= 1).
	LinktestFailThreshold() int

	// NextSystemBytes returns the next unique System Bytes value for an outbound
	// control request the transport itself initiates (such as Select.req, Linktest.req, or
	// Separate.req). Uniqueness is centralized on the connection's
	// per-connection generator (E37 §5.5 / §8.2.6.8) so control and data sends draw
	// from one monotonic space. It is safe for concurrent use.
	NextSystemBytes() [4]byte
}
