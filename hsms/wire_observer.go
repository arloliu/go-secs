package hsms

import (
	"bytes"
	"net"
	"strconv"
	"time"
)

// WireDirection says which way a frame crossed the socket (see [WireEvent]).
type WireDirection uint8

const (
	// WireInbound marks a frame read from the peer.
	WireInbound WireDirection = iota + 1
	// WireOutbound marks a frame written to the peer.
	WireOutbound
)

// WireEvent describes one complete HSMS frame as it crossed a socket —
// see [WithWireObserver] for which frames are reported and when.
//
// Frame aliases a buffer the connection owns and is valid only for the duration of the observer call:
// an observer that keeps the bytes copies them, and never writes into Frame.
// Every other field is a plain value and safe to retain.
type WireEvent struct {
	// Direction says whether the frame was read from or written to the peer.
	Direction WireDirection
	// Socket identifies the socket the frame crossed, as [SocketEvent.Socket] does:
	// a per-connection counter that advances for every socket dialed or accepted, refused ones included.
	Socket uint64
	// Generation identifies the connection generation that owns the socket:
	// a per-connection counter that advances each time the connection establishes a new link,
	// so every frame of one link carries the same value and a reconnected link carries a larger one.
	// It is 0 for a frame of a refused socket, which no generation owns:
	// the peer's first frame on a passive connection's extra socket and the Select.rsp refusing it.
	Generation uint64
	// At is taken when the frame was fully read or fully written.
	// It carries a monotonic clock reading,
	// so At.Sub against another event's At measures elapsed time even across a wall-clock step.
	At time.Time
	// Frame holds the wire bytes: the 4-byte length prefix, the 10-byte header, and the message text.
	Frame []byte
}

// wireReporter is the optional transport capability that opts the core's outbound frames into wire observation.
//
// It is reached by type assertion, deliberately not a method of the transport seam,
// so a transport implementation that predates it keeps compiling.
// A transport offers it only when Write puts the handed buffers on its socket byte for byte:
// the HSMS-SS transport does, while the SECS-I transport reports success for an HSMS control frame without any I/O
// and re-frames a data message as SECS-I blocks, so reporting its frames would describe bytes that never crossed its wire.
//
// WireReportScope opens the report scope of socket, the identity the transport minted for the epoch's conn,
// around one outbound write, and returns the function that releases it, to be called exactly once.
// The core opens it just before the write and releases it after the frame's report,
// or as soon as the write has failed, and on a panic in the observer.
// While a scope is held, the transport holds back the report of that socket's close, never the close itself,
// which is what keeps a frame's report ahead of its socket's close report without making a closer wait;
// when the close lands meanwhile, the release delivers the close report on the goroutine that releases the scope:
// the writing goroutine for a frame writeFrame reported,
// or the goroutine that delivers the courtesy Separate's report (deliverWireReport).
// It returns nil when that close is already pending, or when socket is not the transport's socket;
// the core then writes the frame without reporting it.
// The transport returns a function it holds for the socket's lifetime, so a call allocates nothing.
type wireReporter interface {
	WireReportScope(socket uint64) func()
}

// String returns the string representation of the WireDirection.
func (d WireDirection) String() string {
	switch d {
	case WireInbound:
		return "Inbound"
	case WireOutbound:
		return "Outbound"
	default:
		return "WireDirection(" + strconv.FormatUint(uint64(d), 10) + ")"
	}
}

// ObserveWire hands ev to the observer installed by [WithWireObserver], and returns at once when none is.
//
// It is the capability the HSMS-SS transport reaches by type assertion to report the frames it reads itself;
// it is not part of any interface an application implements or calls.
func (c *connection) ObserveWire(ev WireEvent) {
	if obs := c.cfg.Load().wireObserver; obs != nil {
		obs(ev)
	}
}

// outboundWireObserver returns the wire observer that should see the core's outbound frames,
// together with the transport's wire-reporting capability,
// or nil for both when no observer is installed or the transport does not offer the capability.
// A write site loads it once and uses that one value for both the capture and the report,
// so a concurrent UpdateConfigOptions cannot leave a frame captured but unreported, or reported uncaptured.
func (c *connection) outboundWireObserver() (func(WireEvent), wireReporter) {
	obs := c.cfg.Load().wireObserver
	if obs == nil {
		return nil, nil
	}

	wr, ok := c.tr.(wireReporter)
	if !ok {
		return nil, nil
	}

	return obs, wr
}

// captureWireFrame copies the frame held by bufs into this epoch's scratch buffer and returns it.
// The caller holds e.writeMu, which guards the scratch buffer,
// and calls it before handing bufs to the transport, because the write consumes bufs as it goes.
//
// The scratch buffer grows to the largest frame this generation has written with an observer installed,
// and is reused after that,
// so observation adds no allocation per frame once warm;
// it is released with the epoch when the generation ends.
func (e *epoch) captureWireFrame(bufs net.Buffers) []byte {
	n := 0
	for _, b := range bufs {
		n += len(b)
	}

	if cap(e.wireScratch) < n {
		e.wireScratch = make([]byte, n)
	}

	frame := e.wireScratch[:n]
	off := 0
	for _, b := range bufs {
		off += copy(frame[off:], b)
	}

	return frame
}

// copyWireFrame returns a fresh copy of the frame held by bufs, for a report made off the writing goroutine.
// The caller calls it before handing bufs to the transport, because the write consumes bufs as it goes.
// Unlike captureWireFrame it does not use the epoch's scratch buffer,
// which belongs to whoever holds e.writeMu and would be overwritten by the next write while the copy is still being reported.
func copyWireFrame(bufs net.Buffers) []byte {
	return bytes.Join(bufs, nil)
}

// deliverWireReport reports ev to obs and then releases the socket's report scope,
// on a panic in obs too, so the close report the scope holds back is never lost.
//
// It is the body of the goroutine writeFarewellSeparate starts for the courtesy Separate's report:
// the supervisor goroutine must never call the observer, because it runs the teardown that Close waits for,
// so an observer that did not return there would hang Close.
// Nothing joins that goroutine: an observer that never returns leaks it and delays only the socket's close report.
// A panic in obs is not recovered, as at every other observer call site.
func deliverWireReport(obs func(WireEvent), ev WireEvent, release func()) {
	defer release()

	obs(ev)
}
